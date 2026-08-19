// PLUM's Node trace shim.
//
// Loaded with `node --require ./shims/node/plum-shim.cjs`, it wraps only the
// exported symbols named in PLUM_SYMBOLS — the AST pass already decided the
// instrumentation set — and writes the same JSONL Event schema the Go and
// Python shims write. The core ingests all three identically.
'use strict';

const fs = require('fs');
const Module = require('module');
const path = require('path');

const OUT = process.env.PLUM_TRACE_OUT;
const MAX = parseInt(process.env.PLUM_TRACE_MAX || '200000', 10);
const ROOT = process.env.PLUM_REPO_ROOT || process.cwd();
const WANTED = new Set((process.env.PLUM_SYMBOLS || '').split(',').filter(Boolean));

if (OUT) {
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
    try { s = typeof v === 'string' ? v : JSON.stringify(v); } catch { s = String(v); }
    if (s === undefined) s = String(v);
    return s.length > 200 ? s.slice(0, 200) + '...' : s;
  };

  // wrap replaces one exported function with a tracing proxy. Async functions
  // are awaited so the return event lands when the promise settles, not when the
  // call returns a pending promise.
  const wrap = (symbol, fn) => function (...args) {
    const invocation = `${process.pid}-${++counter}`;
    const parent = stack.length ? stack[stack.length - 1] : '';
    const depth = stack.length;
    emit({
      event: 'call', symbol_id: symbol, invocation_id: invocation,
      parent_invocation_id: parent, depth,
      args: Object.fromEntries(args.map((a, i) => [`arg${i}`, truncate(a)])),
    });
    stack.push(invocation);
    const done = (kind, extra) => {
      stack.pop();
      emit(Object.assign({ event: kind, symbol_id: symbol, invocation_id: invocation, depth }, extra));
    };
    try {
      const out = fn.apply(this, args);
      if (out && typeof out.then === 'function') {
        return out.then(
          (v) => { done('return', { result: truncate(v) }); return v; },
          (e) => { done('raise', { exception: truncate(e && e.message || e) }); throw e; },
        );
      }
      done('return', { result: truncate(out) });
      return out;
    } catch (e) {
      done('raise', { exception: truncate(e && e.message || e) });
      throw e;
    }
  };

  const originalLoad = Module._load;
  Module._load = function (request, parent, isMain) {
    const exported = originalLoad.apply(this, arguments);
    if (!parent || !exported || typeof exported !== 'object') return exported;
    let resolved;
    try { resolved = require.resolve(request, { paths: [path.dirname(parent.filename)] }); } catch { return exported; }
    if (resolved.includes('node_modules')) return exported;
    const rel = path.relative(ROOT, resolved);
    for (const key of Object.keys(exported)) {
      const symbol = `${rel}::${key}`;
      if (typeof exported[key] === 'function' && (WANTED.size === 0 || WANTED.has(symbol))) {
        exported[key] = wrap(symbol, exported[key]);
      }
    }
    return exported;
  };
}
