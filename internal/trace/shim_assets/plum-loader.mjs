// PLUM's ESM module-customization hook.
//
// CommonJS can be instrumented by hooking module loading and wrapping the
// exports object afterwards. ES modules cannot: their exports are live bindings
// resolved at link time, and there is no object to mutate from outside. So the
// only place to attach is the source itself, before it is evaluated — which is
// exactly what a `load` hook is for.
//
// The transform appends to the module's own source rather than rewriting it.
// Nothing already there moves, so line numbers stay honest and a mistake here
// cannot corrupt the code being observed.
//
// Registered by plum-shim.cjs via module.register() (Node 18.19+ / 20.6+).

// MARKER identifies source this hook has already transformed. The hook can be
// registered more than once — NODE_OPTIONS reaches worker threads, including
// the one hooks run on — and appending twice would wrap every symbol twice.
const MARKER = '/* plum: appended instrumentation — the source above is untouched */';

const ROOT = process.env.PLUM_REPO_ROOT || process.cwd();
const WANTED = new Set([
  ...(process.env.PLUM_SYMBOLS || '').split(','),
  ...(process.env.PLUM_CONTEXT_SYMBOLS || '').split(','),
].filter(Boolean));

// symbolsByFile groups the instrumentation set by the file it lives in, so a
// module only pays for the symbols the AST pass actually named.
const symbolsByFile = new Map();
for (const id of WANTED) {
  const at = id.indexOf('::');
  if (at < 0) continue;
  const file = id.slice(0, at);
  const name = id.slice(at + 2);
  if (!symbolsByFile.has(file)) symbolsByFile.set(file, []);
  symbolsByFile.get(file).push(name);
}

function relativeToRoot(url) {
  if (!url.startsWith('file://')) return null;
  let p;
  try {
    p = decodeURIComponent(new URL(url).pathname);
  } catch {
    return null;
  }
  if (p.includes('/node_modules/')) return null;
  if (!p.startsWith(ROOT)) return null;
  let rel = p.slice(ROOT.length);
  if (rel.startsWith('/')) rel = rel.slice(1);
  return rel;
}

// buildSuffix writes the wrapping code appended to a module.
//
// A function declaration's binding is mutable, so it can be rebound to a traced
// version and every importer sees it — ES module imports are live bindings, so
// they read the current value rather than a copy taken at import time.
//
// A class needs no rebinding at all: its methods live on the prototype object,
// and mutating that object is visible everywhere the class is.
//
// A `const`-bound arrow cannot be rebound. That attempt throws, is caught, and
// the symbol is reported as uninstrumented rather than silently missing.
function buildSuffix(rel, names) {
  const lines = [
    '\n' + MARKER,
    'try {',
    '  const __plum = globalThis.__PLUM__;',
    '  if (__plum) {',
  ];

  const classes = new Map(); // class name -> method names
  const functions = [];
  for (const name of names) {
    const dot = name.indexOf('.');
    if (dot < 0) {
      functions.push(name);
      continue;
    }
    const cls = name.slice(0, dot);
    const method = name.slice(dot + 1);
    if (!classes.has(cls)) classes.set(cls, []);
    classes.get(cls).push(method);
  }

  for (const name of functions) {
    // Rebinding is attempted per symbol so one const export cannot stop the rest.
    lines.push(
      `    try { if (typeof ${name} === 'function') ${name} = __plum.wrap(${JSON.stringify(rel + '::' + name)}, ${name}); }`,
      `    catch (e) { __plum.uninstrumentable(${JSON.stringify(rel + '::' + name)}, e); }`,
    );
  }
  for (const [cls, methods] of classes) {
    lines.push(
      `    try { __plum.wrapClass(${JSON.stringify(rel)}, ${JSON.stringify(cls)}, ${JSON.stringify(methods)}, ${cls}); }`,
      `    catch (e) { __plum.uninstrumentable(${JSON.stringify(rel + '::' + cls)}, e); }`,
    );
  }

  lines.push('  }', '} catch (e) { /* tracing must never break the run it observes */ }', '');
  return lines.join('\n');
}

export async function load(url, context, nextLoad) {
  const result = await nextLoad(url, context);
  if (result.format !== 'module' || !result.source) return result;

  const rel = relativeToRoot(url);
  if (!rel) return result;
  const names = symbolsByFile.get(rel);
  if (!names || names.length === 0) return result;

  const source = typeof result.source === 'string'
    ? result.source
    : Buffer.from(result.source).toString('utf8');
  if (source.includes(MARKER)) return result;

  return { ...result, source: source + buildSuffix(rel, names) };
}
