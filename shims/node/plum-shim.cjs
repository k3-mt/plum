// PLUM's Node trace shim.
//
// Preloaded with `node --require .../plum-shim.cjs`, it wraps only the symbols
// named in PLUM_SYMBOLS — the AST pass already decided the instrumentation set —
// and writes the same JSONL Event schema the Go and Python shims write. The core
// ingests all three identically, which is what keeps the tool polyglot without
// the engine learning anything about any runtime.
//
// Both module systems are covered, by different means, because they need
// different means:
//
//   CommonJS  hook Module._load and wrap the exports object afterwards.
//   ESM       register plum-loader.mjs, which appends wrapping code to the
//             module source before evaluation — ES exports are live bindings
//             resolved at link time, so there is no object to mutate from
//             outside.
//
// The tracing runtime itself is shared: it hangs off globalThis so the appended
// ESM code and the CJS wrapper report into the same stack, counter and file.
'use strict';

const fs = require('fs');
const Module = require('module');
const path = require('path');

const OUT = process.env.PLUM_TRACE_OUT;
const MAX = parseInt(process.env.PLUM_TRACE_MAX || '200000', 10);
const ROOT = process.env.PLUM_REPO_ROOT || process.cwd();
const WANTED = new Set((process.env.PLUM_SYMBOLS || '').split(',').filter(Boolean));

// NODE_OPTIONS is inherited by worker threads, including the thread Node runs
// module-customization hooks on, so this preload can be evaluated more than
// once per process. Installing twice would wrap every function twice and report
// each call at two depths.
if (OUT && process.env.PLUM_TRACE === '1' && !globalThis.__PLUM__) {
  const fd = fs.openSync(OUT, 'a');
  const stack = [];
  let counter = 0;
  let written = 0;

  const emit = (fields) => {
    if (written >= MAX) return;
    written++;
    fs.writeSync(fd, JSON.stringify(Object.assign({
      schema_version: '1.0',
      ts_ns: Number(process.hrtime.bigint()),
      test_id: process.env.PLUM_TEST_ID || '',
    }, fields)) + '\n');
  };

  const truncate = (v) => {
    let s;
    if (typeof v === 'string') s = v;
    else if (v === undefined) s = 'undefined';
    else {
      try { s = JSON.stringify(v); } catch { s = String(v); }
      if (s === undefined) s = String(v);
    }
    return s.length > 200 ? s.slice(0, 200) + '...' : s;
  };

  // wrap replaces one function with a tracing proxy. Async functions are
  // awaited so the return event lands when the promise settles, not when the
  // call hands back a pending promise — otherwise every async frame would
  // appear to cost nothing and return an object.
  // WRAPPED marks a function this shim already instrumented. A module reachable
  // through both CommonJS and ESM would otherwise be wrapped twice, and every
  // call would be reported at two depths.
  const WRAPPED = Symbol.for('plum.wrapped');

  const wrap = (symbol, fn) => {
    if (typeof fn !== 'function' || fn[WRAPPED]) return fn;
    const traced = function (...args) {
      const invocation = `${process.pid}-${++counter}`;
      const parent = stack.length ? stack[stack.length - 1] : '';
      const depth = stack.length;
      emit({
        event: 'call', symbol_id: symbol, invocation_id: invocation,
        parent_invocation_id: parent, depth,
        args: Object.fromEntries(args.map((a, i) => [argName(fn, i), truncate(a)])),
      });
      stack.push(invocation);
      const done = (kind, extra) => {
        const i = stack.lastIndexOf(invocation);
        if (i >= 0) stack.splice(i, 1);
        emit(Object.assign({ event: kind, symbol_id: symbol, invocation_id: invocation, depth }, extra));
      };
      try {
        const out = fn.apply(this, args);
        if (out && typeof out.then === 'function') {
          return out.then(
            (v) => { done('return', { result: truncate(v) }); return v; },
            (e) => { done('raise', { exception: truncate((e && e.message) || e) }); throw e; },
          );
        }
        done('return', { result: truncate(out) });
        return out;
      } catch (e) {
        done('raise', { exception: truncate((e && e.message) || e) });
        throw e;
      }
    };
    Object.defineProperty(traced, 'name', { value: fn.name, configurable: true });
    Object.defineProperty(traced, WRAPPED, { value: symbol, enumerable: false });
    traced.prototype = fn.prototype;
    return traced;
  };

  // argName recovers a parameter's declared name so recorded arguments read
  // like the source rather than like arg0, arg1. Falls back when it cannot.
  const argNames = new WeakMap();
  const argName = (fn, i) => {
    if (!argNames.has(fn)) {
      let names = [];
      try {
        const src = Function.prototype.toString.call(fn);
        const open = src.indexOf('(');
        const close = src.indexOf(')', open);
        if (open >= 0 && close > open) {
          names = src.slice(open + 1, close).split(',')
            .map((s) => s.trim().split(/[=\s]/)[0].replace(/^\.\.\./, ''))
            .filter((s) => s && /^[A-Za-z_$][\w$]*$/.test(s));
        }
      } catch { /* a native or bound function has no readable source */ }
      argNames.set(fn, names);
    }
    const names = argNames.get(fn);
    return names[i] || `arg${i}`;
  };

  // instrument walks one module's exports, wrapping the functions and class
  // methods whose SymbolID is in the wanted set.
  const instrument = (exported, rel) => {
    for (const key of Object.keys(exported)) {
      const value = exported[key];
      if (typeof value !== 'function') continue;

      const symbol = `${rel}::${key}`;
      if (WANTED.size === 0 || WANTED.has(symbol)) {
        exported[key] = wrap(symbol, value);
      }

      // A class is a function whose prototype carries the methods. They are the
      // Class.method SymbolIDs the extractor produced, so they must be wrapped
      // on the prototype, not on the export.
      const proto = value.prototype;
      if (!proto || proto === Function.prototype) continue;
      for (const name of Object.getOwnPropertyNames(proto)) {
        if (name === 'constructor') continue;
        const desc = Object.getOwnPropertyDescriptor(proto, name);
        if (!desc || typeof desc.value !== 'function' || !desc.writable) continue;
        const methodSymbol = `${rel}::${key}.${name}`;
        if (WANTED.size === 0 || WANTED.has(methodSymbol)) {
          proto[name] = wrap(methodSymbol, desc.value);
        }
      }
    }
  };

  // The runtime is published on globalThis so the code the ESM loader appends
  // to a module can reach it without importing anything.
  const uninstrumentable = [];
  globalThis.__PLUM__ = {
    wrap,
    // wrapClass mutates the prototype in place, which needs no rebinding and so
    // works for `class C {}` and `const C = class {}` alike.
    wrapClass(rel, className, methods, cls) {
      if (typeof cls !== 'function' || !cls.prototype) return;
      for (const name of methods) {
        const desc = Object.getOwnPropertyDescriptor(cls.prototype, name);
        if (!desc || typeof desc.value !== 'function' || !desc.writable) continue;
        if (desc.value[WRAPPED]) continue;
        cls.prototype[name] = wrap(`${rel}::${className}.${name}`, desc.value);
      }
    },
    // A symbol that cannot be rebound — a const-bound arrow export — is
    // reported into the trace stream rather than silently dropped. Claiming to
    // have instrumented something that was never traced is worse than saying so.
    uninstrumentable(symbol, err) {
      const reason = (err && err.message) || String(err);
      uninstrumentable.push({ symbol, reason });
      emit({ event: 'uninstrumented', symbol_id: symbol, exception: reason });
    },
    uninstrumented: () => uninstrumentable,
  };

  // ESM cannot be reached by hooking require, so register a module-customization
  // hook. Available since Node 18.19 / 20.6; older runtimes keep CJS tracing.
  try {
    const { register } = require('node:module');
    const { pathToFileURL } = require('node:url');
    if (typeof register === 'function') {
      register('./plum-loader.mjs', pathToFileURL(__filename));
    }
  } catch (e) {
    if (process.env.PLUM_TRACE_DEBUG) {
      process.stderr.write(`plum: ESM loader not registered: ${e && e.message}\n`);
    }
  }

  const originalLoad = Module._load;
  Module._load = function (request, parent, isMain) {
    const exported = originalLoad.apply(this, arguments);
    if (!exported || (typeof exported !== 'object' && typeof exported !== 'function')) return exported;
    let resolved;
    try {
      resolved = Module._resolveFilename(request, parent, isMain);
    } catch {
      return exported;
    }
    if (typeof resolved !== 'string' || resolved.includes('node_modules') || !path.isAbsolute(resolved)) {
      return exported;
    }
    const rel = path.relative(ROOT, resolved);
    if (rel.startsWith('..')) return exported; // outside the repository
    try {
      instrument(exported, rel);
    } catch { /* instrumentation must never break the run it observes */ }
    return exported;
  };
}
