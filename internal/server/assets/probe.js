async function usePane(target, force) {
  let res = {};
  try {
    res = await (await fetch('/api/pane', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ target, force: !!force }),
    })).json();
  } catch (e) {
    res = { ok: false, warning: 'could not reach plum' };
  }
  if (res.ok === false) {
    // Refused, with a reason and a way past it. plum does not know every agent,
    // so this is a warning rather than a rule — but it is not silent.
    confirmPane(target, res.warning);
    return;
  }
  // Pointing at a pane is only useful if the question then goes there, so it is
  // asked again rather than left for the reader to redo.
  explainSelection();
}

function confirmPane(target, warning) {
  const box = $('explained');
  const row = document.createElement('div');
  row.className = 'warn';
  row.textContent = warning || (target + ' does not look like an agent');
  const go = document.createElement('button');
  go.type = 'button';
  go.textContent = 'send it there anyway';
  go.onclick = () => usePane(target, true);
  row.appendChild(go);
  box.appendChild(row);
  reveal(box);
}

// The probe page: one test, what it did, and what it does after you change
// something. It asks the server for a run and draws it. That is the whole page.
let RUN = null, STATIONS = [], SELECTED = null, TESTS = [], MATCHES = [];
// What the source pane is currently showing, and which name is lit inside it.
let CURRENT_PC = {}, INSPECTING = null, INSPECT_LINE = 0, SRC_LINES = [], SRC_FIRST = 0;
let SELECTION = null, EXPLAIN_BUSY = false, EXPLAIN_PENDING = null;
// STEP is where you are in the run, the way a debugger has a current frame.
// The journey is the whole path at once; stepping is walking it.
let STEP = -1;

const $ = (id) => document.getElementById(id);

async function boot() {
  RUN = await (await fetch('/api/probe')).json();
  draw();
  $('again').onclick = async () => {
    verdict('running', 'running…');
    RUN = await (await fetch('/api/probe/run', { method: 'POST' })).json();
    draw();
  };
  $('fixture-save').onclick = saveFixture;
  loadTests();
  const box = $('filter');
  box.addEventListener('focus', () => openList(true));
  box.addEventListener('input', () => openList(true));
  box.addEventListener('keydown', onFilterKey);
  // Clicking anywhere else is how you dismiss a menu.
  document.addEventListener('click', (e) => {
    if (!e.target.closest('.picker')) openList(false);
  });
  $('back').onclick = () => step(STEP - 1);
  $('fwd').onclick = () => step(STEP + 1);
  document.addEventListener('keydown', onKey);
  window.addEventListener('resize', fitToViewport);
  // The button appears when there is something selected to ask about, and only
  // inside the source pane — selecting the narration is not a question.
  document.addEventListener('selectionchange', offerExplain);
  // The tooltip follows the pointer's interest, not the layout: it shows while
  // you are over the code you selected and gets out of the way when you leave.
  $('detail-src').addEventListener('mousemove', () => showTip(true));
  $('detail-src').addEventListener('mouseleave', () => showTip(false));
  $('tip').addEventListener('mouseenter', () => showTip(true));
  $('tip').onclick = explainSelection;
  listen();
}

// listen is why this is a window and not a page you refresh. The server re-runs
// the probe when the code changes; this is how the result arrives.
function listen() {
  if (!window.EventSource) return;
  const src = new EventSource('/api/live');
  src.addEventListener('reload', async (e) => {
    if (e.data !== 'probe') return; // a session changing is not this page's business
    RUN = await (await fetch('/api/probe')).json();
    draw();
  });
}

function verdict(kind, text) {
  const v = $('verdict');
  v.className = 'verdict ' + kind;
  v.textContent = text;
}

function draw() {
  if (!RUN) return;
  if (document.activeElement !== $('filter')) $('filter').value = RUN.test || '';
  document.title = 'plum · ' + (RUN.test || 'probe');

  if (RUN.error) {
    verdict('fail', '● could not run');
  } else if (RUN.why === 'nothing chosen yet') {
    // Not a verdict. Red "did not run" before anything was asked to run reads
    // as something having gone wrong, on the one screen where nothing has.
    verdict('', '');
  } else if (RUN.why === 'not run yet') {
    verdict('running', '\u25cb not run yet');
  } else if (RUN.why === 'running') {
    // Queued behind a run already going. The result arrives over the live
    // stream when it lands, so this is a state to show rather than an end.
    verdict('running', '\u25cb running\u2026');
  } else {
    // "failed" for a test that ran and disagreed with the code is a different
    // report from one for a command that never started.
    const label = RUN.passed ? '● passed'
      : RUN.recorded ? '● failed'
      : '● did not run';
    verdict(RUN.passed ? 'pass' : 'fail', label + '  ' + duration(RUN.duration_ms));
  }

  $('why').innerHTML = '';
  if (RUN.why === 'nothing chosen yet') {
    // The welcome, when it is showing, says this at length; the strip saying
    // it again in short is the same sentence twice on one screen.
    $('why').textContent = wantsWelcome() ? '' : 'choose a test above and it runs';
  } else if (RUN.why === 'running') {
    $('why').textContent = 'running ' + (RUN.test || '') + '\u2026';
  } else if (RUN.why && RUN.why !== 'not run yet') {
    $('why').append('ran because ');
    const b = document.createElement('b');
    b.textContent = RUN.why;
    $('why').append(b, RUN.at ? '  \u00b7  ' + RUN.at : '');
    // A test that calls something twelve times records twelve separate trees
    // and this draws one of them. Showing one and saying nothing reads as "this
    // is what happened", when it is one of twelve things that happened.
    const chains = (RUN.landscape && RUN.landscape.chains) || 0;
    if (chains > 1) {
      const more = document.createElement('span');
      more.className = 'chains';
      more.textContent = '  \u00b7  showing the longest of ' + chains + ' separate call trees';
      more.title = 'The test entered instrumented code ' + chains + ' times from the top level. '
        + 'The journey below is the one that went deepest.';
      $('why').appendChild(more);
    }
  } else {
    $('why').textContent = 'press run, or save a file it touches';
  }

  drawWelcome();
  drawTrace();

  // The test's own output earns space when something went wrong OR when there
  // is no picture to read instead: a passing run that drew nothing is exactly
  // when its output is the only thing that explains why, so hiding it then is
  // what makes a real run look like it never happened.
  const out = $('output');
  const drewNothing = !((RUN.landscape && RUN.landscape.wells) || []).length
    && RUN.why !== 'nothing chosen yet' && RUN.why !== 'not run yet' && RUN.why !== 'running';
  const worthShowing = !RUN.passed || RUN.error || drewNothing;
  out.hidden = !worthShowing || !(RUN.output || RUN.error);
  out.textContent = [RUN.error, RUN.output].filter(Boolean).join('\n\n');

  drawFixture();
}

function duration(ms) {
  if (!ms) return '';
  return ms < 1000 ? ms + 'ms' : (ms / 1000).toFixed(1) + 's';
}

// collapse folds a run of identical sibling calls into one station.
//
// `rank` called six times in a row is six stations saying the same thing, and
// the shape of the journey is what this page is for. The count is kept and the
// costs are summed, so nothing is hidden — six calls at 10µs each is a
// different fact from one at 60µs, and both are on the row.
function collapse(wells, steps) {
  // Each frame's leg is the transition immediately before it: what was called,
  // what it cost, and whether anything at the call site explained why.
  const legs = new Map();
  steps.forEach((st, k) => {
    if (st.kind !== 'frame') return;
    const before = steps[k - 1];
    if (before && before.kind === 'transition') legs.set(st.index, before);
  });

  const out = [];
  wells.forEach((w, i) => {
    // A resume is the same frame being returned into. Drawing it again would
    // make one call look like several.
    if (w.phase === 'resume') return;
    const prev = out[out.length - 1];
    if (prev && prev.well.symbol === w.symbol && prev.well.depth === w.depth) {
      prev.count += 1;
      prev.selfNs += w.self_ns || 0;
      return;
    }
    out.push({
      well: w, index: i, count: 1, selfNs: w.self_ns || 0,
      step: steps.find((st) => st.kind === 'frame' && st.index === i),
      leg: legs.get(i),
    });
  });
  return out;
}

// drawTrace draws the run as a journey down the page: stations are the frames it
// went through, legs are the movement between them.
function drawTrace() {
  const list = $('trace');
  list.innerHTML = '';
  const wells = (RUN.landscape && RUN.landscape.wells) || [];
  if (!wells.length) {
    const li = document.createElement('li');
    li.className = 'empty';
    // Four different reasons for an empty picture, and saying the wrong one
    // sends the reader looking in the wrong place. A build failure is not a
    // test that declined to enter your code, and neither is not having asked
    // for one yet.
    if (RUN.why === 'nothing chosen yet') {
      li.textContent = 'Nothing chosen. Pick a test above and it runs.';
    } else if (RUN.error) {
      li.textContent = 'Could not run it — ' + RUN.error;
    } else if (!RUN.recorded && !RUN.passed) {
      li.textContent = 'The command failed before it recorded anything — the output '
        + 'below is what it printed.';
    } else if (!RUN.recorded) {
      li.textContent = 'It ran and passed, but plum recorded nothing: no instrumented '
        + 'function was entered. Either this test does not exercise the changed code, '
        + 'or the tracer did not attach. The output below is the whole run.';
    } else {
      li.textContent = 'It ran, but entered none of the code this session changed.';
    }
    list.appendChild(li);
    select(null);
    return;
  }

  STATIONS = collapse(wells, RUN.narration || []);
  const peak = STATIONS.reduce((m, r) => Math.max(m, r.selfNs), 0) || 1;

  STATIONS.forEach((row, n) => {
    if (row.leg) list.appendChild(legRow(row));
    list.appendChild(stationRow(row, n, peak));
  });

  // Keep the reader where they were across a re-run, so saving a file does not
  // throw away what they were reading.
  // Keep the reader where they were across a re-run: saving a file must not
  // send them back to the start of the walk.
  const keep = STATIONS.findIndex((r) => r.well.symbol === SELECTED);
  select(keep >= 0 ? keep : null);
  fitToViewport();
}

// spine draws one rail per level still open at this point: the call stack made
// visible, so you can see what a station is going to return into.
function spine(depth, cls) {
  const el = document.createElement('span');
  el.className = 'spine';
  for (let d = 0; d < depth; d++) {
    const i = document.createElement('i');
    if (cls) i.className = cls;
    el.appendChild(i);
  }
  return el;
}

function legRow(row) {
  const li = document.createElement('li');
  li.className = 'leg';
  li.appendChild(spine(row.well.depth, 'live'));
  // The cost is its own element so the ladder can drop the prose and keep the
  // number: what a step cost is the part you cannot reconstruct by looking at
  // the stations either side of it.
  const cost = document.createElement('span');
  cost.className = 'legcost';
  cost.textContent = (row.leg.spans || []).filter((sp) => sp.kind === 'cost').map((sp) => sp.text).join(' ');
  li.appendChild(cost);

  const text = document.createElement('span');
  text.className = 'text';
  text.textContent = row.leg.text || '';
  if (row.leg.note) text.title = row.leg.note;
  li.appendChild(text);
  return li;
}

// fitToViewport compresses the journey until it fits, and stops compressing the
// moment it does.
//
// Measured against the real layout rather than calculated from row heights: the
// arithmetic version has to know every margin on the page and is wrong the first
// time anyone changes one. Three reflows on a redraw is nothing next to the run
// that produced the data.
function fitToViewport() {
  const list = $('trace');
  if (!list) return;
  list.classList.remove('tight', 'tighter');
  const fits = () => list.scrollHeight <= list.clientHeight + 1;
  if (fits()) return;
  list.classList.add('tight');
  if (fits()) return;
  list.classList.add('tighter');
  // Still too tall: let it scroll. Shrinking further would trade a scrollbar
  // for text nobody can read, which is not a trade.
}

function stationRow(row, n, peak) {
  const w = row.well;
  const li = document.createElement('li');
  li.className = 'station';
  if (w.context) li.classList.add('context');
  if (w.risk) li.classList.add('risk');
  if (w.phase === 'escape') li.classList.add('escape');
  if (!w.doc) li.classList.add('undocumented');

  const b = document.createElement('button');
  b.appendChild(spine(w.depth));

  const dot = document.createElement('span');
  dot.className = 'dot';
  b.appendChild(dot);

  const name = document.createElement('span');
  name.className = 'name';
  name.textContent = w.label || w.symbol;
  b.appendChild(name);

  if (row.count > 1) {
    const times = document.createElement('span');
    times.className = 'times';
    times.textContent = '\u00d7' + row.count;
    b.appendChild(times);
  }

  // A station that wrote back through its arguments is worth seeing without
  // opening it: it is the one kind of step whose effect is not in its result.
  const fvRow = (RUN.values || {})[row.index];
  const changed = ((fvRow && fvRow.args) || []).filter((a) => a.after).length;
  if (changed) {
    const m = document.createElement('span');
    m.className = 'mut';
    m.textContent = '\u270e' + changed;
    m.title = changed + ' argument(s) came back different from how they went in';
    b.appendChild(m);
  }

  const vals = valuesOf(row);
  const v = document.createElement('span');
  v.className = 'vals';
  v.textContent = vals;
  b.appendChild(v);

  const bar = document.createElement('span');
  bar.className = 'bar';
  const fill = document.createElement('i');
  // A floor, so a station that cost something never draws as if it cost nothing.
  fill.style.width = Math.max(row.selfNs ? 2 : 0, (row.selfNs / peak) * 100) + '%';
  bar.appendChild(fill);
  b.appendChild(bar);

  const cost = document.createElement('span');
  cost.className = 'cost';
  cost.textContent = row.selfNs ? micros(row.selfNs) : '';
  b.appendChild(cost);

  b.onclick = () => select(n);
  li.appendChild(b);
  return li;
}

// select is one step of the walk. It shows what this station is, where it sits
// in the stack, what the run said about it, and its code — with the line it is
// about to leave from marked, which is the thing a debugger gives you and a
// static picture does not.
async function select(n) {
  const rows = document.querySelectorAll('#trace .station');
  rows.forEach((el, i) => el.classList.toggle('on', i === n));

  if (n === null || !STATIONS[n]) {
    STEP = -1;
    SELECTED = null;
    $('detail-empty').hidden = false;
    $('detail-body').hidden = true;
    drawStepper();
    return;
  }

  STEP = n;
  drawStepper();
  const row = STATIONS[n];
  SELECTED = row.well.symbol;
  if (rows[n]) rows[n].scrollIntoView({ block: 'nearest' });

  $('detail-empty').hidden = true;
  $('detail-body').hidden = false;
  $('detail-name').textContent = row.well.label || row.well.symbol;

  drawStack(n);
  drawValues(n);

  $('detail-sentence').innerHTML = '';
  $('detail-sentence').appendChild(renderSpans(row.step));
  if (row.step && row.step.note) {
    const note = document.createElement('div');
    note.className = 'note';
    note.textContent = row.step.note;
    $('detail-sentence').appendChild(note);
  }
  if (row.count > 1) {
    const many = document.createElement('div');
    many.className = 'note';
    many.textContent = 'Called ' + row.count + ' times in a row here; the cost shown is all of them together.';
    $('detail-sentence').appendChild(many);
  }

  const file = row.well.symbol.split('::')[0];
  $('detail-where').textContent = file;
  $('detail-src').textContent = 'loading…';
  closeInspect();
  try {
    const pc = await fetch('/api/symbol/' + encodeURIComponent(row.well.symbol)).then((r) => r.json());
    if (SELECTED !== row.well.symbol) return; // they stepped on while this was in flight
    const first = (pc.declaration || {}).line_start || 0;
    if (first) $('detail-where').textContent = file + ':' + first;
    if (!pc.source) {
      $('detail-src').textContent = '(no source for this symbol in the working tree)';
      return;
    }
    CURRENT_PC = pc;
    const line = callLineFor(n, pc);
    drawAt(n, line);
    drawSource(pc, first, line);
  } catch (e) {
    $('detail-src').textContent = '(could not read the source)';
  }
}

// drawAt says where the walk is standing. The highlighted line says it too, but
// only if you notice it — and when no call site was recorded there is no line to
// notice, which is a thing to say rather than an absence to leave.
function drawAt(n, line) {
  const box = $('detail-at');
  const next = nextCall(n);
  box.className = 'at';
  if (next && line) {
    box.textContent = 'line ' + line + ' — about to call ' + (next.well.label || next.well.symbol);
    return;
  }
  if (next) {
    box.classList.add('unknown');
    box.textContent = 'calls ' + (next.well.label || next.well.symbol)
      + ', but no call site was recorded for it — the line cannot be pointed at.';
    return;
  }
  box.classList.add('unknown');
  box.textContent = n >= STATIONS.length - 1
    ? 'the last frame recorded on this path'
    : 'returns from here';
}

// callLineFor is the line this frame leaves from on the next step: the call site
// of whatever it calls into. Without it the highlight would just be "the top of
// the function", which is where you already are.
function callLineFor(n, pc) {
  const next = nextCall(n);
  if (!next) return 0;
  for (const cs of pc.call_sites || []) {
    if (cs.callee === next.well.symbol) return cs.line;
  }
  return 0;
}

// drawValues lays out what the frame was called with and what it gave back.
// Rows, because that is the shape of the data: a name and a value, repeated.
function drawValues(n) {
  const box = $('detail-values');
  box.innerHTML = '';
  const fv = (RUN.values || {})[STATIONS[n].index];
  if (!fv) return;

  if (fv.args && fv.args.length) {
    const changed = fv.args.filter((a) => a.after).length;
    box.appendChild(group('in' + (changed ? ' \u00b7 ' + changed + ' changed by this call' : '')));
    for (const a of fv.args) {
      box.appendChild(valueRow(a.name, a.value, !!a.after));
      // What the caller holds now, directly beneath what it passed, so the
      // change is a thing you see rather than a thing you work out.
      if (a.after) box.appendChild(valueRow('', a.after, false, 'after'));
    }
  }
  if (fv.raised) {
    box.appendChild(group('raised'));
    const row = valueRow('panic', fv.raised);
    row.classList.add('raised');
    box.appendChild(row);
  } else if (fv.result) {
    box.appendChild(group('out'));
    box.appendChild(valueRow('', fv.result));
  }
}

function group(label) {
  const el = document.createElement('div');
  el.className = 'grp';
  el.textContent = label;
  return el;
}

// A long value folds to three lines and opens on a click. Folded rather than
// truncated: the rest is still there, which is the difference between hiding
// something and losing it.
function valueRow(name, value, mutated, kind) {
  const row = document.createElement('div');
  row.className = 'v' + (mutated ? ' mutated' : '') + (kind === 'after' ? ' after' : '');

  const k = document.createElement('span');
  k.className = 'k';
  k.textContent = kind === 'after' ? '\u21b3 after' : name;
  row.appendChild(k);

  const v = document.createElement('span');
  v.className = 'val';
  v.textContent = value;
  if (value.length > 160) {
    v.classList.add('long', 'folded');
    v.title = 'click to unfold';
    v.onclick = () => v.classList.toggle('folded');
  }
  row.appendChild(v);
  return row;
}

function drawStack(n) {
  const box = $('detail-stack');
  box.innerHTML = '';
  const stack = callStack(n);
  if (!stack.length) {
    box.textContent = 'the entry point of this run';
    return;
  }
  stack.forEach((row, i) => {
    const b = document.createElement('b');
    b.textContent = row.well.label || row.well.symbol;
    box.appendChild(b);
    const sep = document.createElement('span');
    sep.className = 'sep';
    sep.textContent = ' → ';
    box.appendChild(sep);
  });
  const here = document.createElement('span');
  here.textContent = STATIONS[n].well.label || STATIONS[n].well.symbol;
  box.appendChild(here);
}

// valuesOf is the one-line form for a journey row, where there is room for a
// hint and not for the data. It prefers what came back — the outcome of a step
// is what you are scanning for — and falls back to naming the arguments when
// the values are too big to show.
function valuesOf(row) {
  const fv = (RUN.values || {})[row.index];
  if (!fv) return '';
  if (fv.raised) return '\u26a0 ' + oneLine(fv.raised, 44);
  if (fv.result && fv.result.length <= 44) return '\u2192 ' + oneLine(fv.result, 44);

  const args = fv.args || [];
  if (!args.length) return fv.result ? '\u2192 ' + oneLine(fv.result, 44) : '';
  const full = args.map((a) => a.name + '=' + a.value).join(' ');
  if (full.length <= 52) return oneLine(full, 52);
  // Too big to show: name them instead. Which arguments a call takes is still
  // information, and a truncated structure is not.
  return '(' + args.map((a) => a.name).join(', ') + ')';
}

function oneLine(s, n) {
  const flat = String(s).replace(/\s+/g, ' ').trim();
  return flat.length > n ? flat.slice(0, n - 1) + '\u2026' : flat;
}

function renderSpans(step) {
  const frag = document.createDocumentFragment();
  if (!step) {
    frag.appendChild(document.createTextNode('No sentence for this frame.'));
    return frag;
  }
  if (!step.spans || !step.spans.length) {
    frag.appendChild(document.createTextNode(step.text || ''));
    return frag;
  }
  for (const span of step.spans) {
    if (span.kind === 'text') {
      frag.appendChild(document.createTextNode(span.text));
      continue;
    }
    const el = document.createElement('span');
    el.className = 'sp-' + span.kind;
    el.textContent = span.text;
    frag.appendChild(el);
  }
  return frag;
}

function micros(ns) {
  if (ns < 1000) return ns + 'ns';
  if (ns < 1e6) return Math.round(ns / 1000) + 'µs';
  return (ns / 1e6).toFixed(1) + 'ms';
}

function drawFixture() {
  const box = $('fixture');
  if (!RUN.fixture) { box.hidden = true; return; }
  box.hidden = false;
  const area = $('fixture-body');
  // Do not overwrite what somebody is in the middle of typing.
  if (document.activeElement !== area) area.value = RUN.fixture_body || '';
  $('fixture-note').textContent = RUN.fixture_error
    ? RUN.fixture + ' — ' + RUN.fixture_error
    : RUN.fixture;
}

async function saveFixture() {
  $('fixture-note').textContent = 'saving…';
  RUN = await (await fetch('/api/probe/fixture', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ body: $('fixture-body').value }),
  })).json();
  draw();
}

// step moves to a station and takes the reader with it: the row is selected and
// scrolled to, and the code pane points at the line this frame is about to leave
// from. Clamped rather than wrapped — running off the end of a run and reappearing
// at the beginning is disorienting in a way a debugger never is.
function step(n) {
  if (!STATIONS.length) return;
  select(Math.max(0, Math.min(STATIONS.length - 1, n)));
}

// Arrow keys and j/k, the two things anyone reaches for. Ignored while typing in
// the fixture, where they mean what they normally mean.
// onFilterKey drives the list from the box: enter takes the only match or the
// first one, escape gets out without changing anything.
function onFilterKey(e) {
  if (e.key === 'Escape') {
    openList(false);
    $('filter').blur();
    if (RUN && RUN.test) $('filter').value = RUN.test;
    return;
  }
  if (e.key === 'Enter') {
    e.preventDefault();
    const typed = $('filter').value.trim();
    const exact = TESTS.find((t) => t.name === typed);
    if (exact) {
      pick(exact.name);
      return;
    }
    if (MATCHES.length) pick(MATCHES[0]);
  }
}

function onKey(e) {
  const t = e.target;
  if (t && (t.tagName === 'TEXTAREA' || t.tagName === 'INPUT')) return;
  if (e.metaKey || e.ctrlKey || e.altKey) return;
  const moves = {
    ArrowRight: 1, ArrowDown: 1, j: 1, n: 1,
    ArrowLeft: -1, ArrowUp: -1, k: -1, p: -1,
  };
  if (e.key in moves) {
    e.preventDefault();
    step(STEP === -1 ? 0 : STEP + moves[e.key]);
    return;
  }
  if (e.key === 'Home') { e.preventDefault(); step(0); }
  if (e.key === 'End') { e.preventDefault(); step(STATIONS.length - 1); }
}

// loadTests fills the picker. Rediscovered on every load rather than cached: a
// test written a moment ago is the one you are most likely to be looking for.
async function loadTests() {
  try {
    const d = await (await fetch('/api/tests')).json();
    TESTS = d.tests || [];
  } catch (e) {
    TESTS = [];
  }
  $('found').textContent = TESTS.length ? TESTS.length + ' tests' : 'no tests found';
  drawTestList();
}

// drawTestList lays the tests out as a collapsible directory tree, because which
// directory a test lives in is the first thing you know about what it covers,
// and a repo nested a few directories deep is a wall of long paths as a flat
// list. Two hundred names you scroll; a tree you open a branch of.
//
// Filtering matches the name and the test's own comment, and opens the sections
// that contain a hit — searching a tree that stays shut is searching nothing.
function drawTestList() {
  const box = $('testlist');
  const q = $('filter').value.trim().toLowerCase();
  const current = RUN && RUN.test;
  box.innerHTML = '';
  MATCHES = [];

  const hits = TESTS.filter((t) => !q
    || t.name.toLowerCase().includes(q)
    || (t.doc || '').toLowerCase().includes(q)
    || t.package.toLowerCase().includes(q));

  if (!hits.length) {
    const none = document.createElement('div');
    none.className = 'none';
    none.textContent = TESTS.length ? 'nothing matches that' : 'no tests found in this repository';
    box.appendChild(none);
    return;
  }

  // A directory tree, not a flat list of leaf packages. A repo nested a few
  // deep is a wall of long identical prefixes otherwise; a tree is something you
  // open a branch of. Each node is one path segment, holding the directories
  // under it and the tests declared directly in it.
  const root = { name: '', dirs: new Map(), tests: [] };
  for (const t of hits) {
    const segs = (t.package && t.package !== '.') ? t.package.split('/').filter(Boolean) : [];
    let node = root;
    for (const seg of segs) {
      let child = node.dirs.get(seg);
      if (!child) { child = { name: seg, dirs: new Map(), tests: [] }; node.dirs.set(seg, child); }
      node = child;
    }
    node.tests.push(t);
  }

  // Count every test beneath each node — the number on a closed section has to
  // mean "this many inside", not "this many at this exact level".
  const settle = (node) => {
    node.count = node.tests.length;
    for (const child of node.dirs.values()) node.count += settle(child);
    return node.count;
  };
  settle(root);

  // Fold a run of directories that each hold nothing but a single subdirectory
  // into one row: "internal/lang/dbt", not "internal" opening to "lang" opening
  // to "dbt". A directory that actually branches, or holds its own tests, stays.
  const collapse = (node) => {
    for (const child of node.dirs.values()) collapse(child);
    const entries = [...node.dirs.entries()];
    node.dirs = new Map();
    for (let [key, child] of entries) {
      while (child.tests.length === 0 && child.dirs.size === 1) {
        const [ck, only] = [...child.dirs.entries()][0];
        only.name = child.name + '/' + only.name;
        key = key + '/' + ck;
        child = only;
      }
      node.dirs.set(key, child);
    }
  };
  collapse(root);

  const byName = (a, b) => (a < b ? -1 : a > b ? 1 : 0);
  const containsCurrent = (node) =>
    node.tests.some((t) => t.name === current) || [...node.dirs.values()].some(containsCurrent);

  const renderTests = (items, container) => {
    for (const t of [...items].sort((a, b) => byName(a.name, b.name))) {
      const b = document.createElement('button');
      b.className = 'opt' + (t.name === current ? ' on' : '');
      b.type = 'button';
      const nm = document.createElement('span');
      nm.className = 'nm';
      nm.appendChild(mark(t.name, q));
      if (t.handle) {
        const h = document.createElement('span');
        h.className = 'hnd';
        h.textContent = t.handle;
        nm.appendChild(h);
      }
      b.appendChild(nm);
      if (t.doc) {
        const doc = document.createElement('span');
        doc.className = 'doc';
        doc.appendChild(mark(t.doc, q));
        b.appendChild(doc);
      }
      b.onclick = () => pick(t.name);
      MATCHES.push(t.name);
      container.appendChild(b);
    }
  };

  const renderDir = (node, container, depth) => {
    const sec = document.createElement('details');
    sec.className = 'sec';
    sec.style.setProperty('--depth', depth);
    // Filtering opens what it found; with nothing typed only the branch holding
    // the current test opens, so the list starts as an overview, not a wall.
    sec.open = !!q || containsCurrent(node);

    const sum = document.createElement('summary');
    const caret = document.createElement('span');
    caret.className = 'caret';
    caret.textContent = '\u25b8';
    const name = document.createElement('span');
    name.className = 'pkg';
    name.textContent = node.name;
    const n = document.createElement('span');
    n.className = 'n';
    n.textContent = node.count;
    sum.append(caret, name, n);
    sec.appendChild(sum);

    for (const [, child] of [...node.dirs.entries()].sort((a, b) => byName(a[0], b[0]))) {
      renderDir(child, sec, depth + 1);
    }
    renderTests(node.tests, sec);
    container.appendChild(sec);
  };

  // Tests sitting at the repo root come first, above the directory sections.
  renderTests(root.tests, box);
  for (const [, child] of [...root.dirs.entries()].sort((a, b) => byName(a[0], b[0]))) {
    renderDir(child, box, 0);
  }

  // The limit is worth stating where it applies, rather than in a tooltip
  // nobody hovers: a short list that looks complete is the failure here.
  const limit = document.createElement('div');
  limit.className = 'limit';
  limit.textContent = 'Found by parsing declarations. Tests registered by calling a '
    + 'framework, such as it(...) or describe(...), are not declarations and are not listed.';
  box.appendChild(limit);
}

// mark highlights the matched run so you can see why a row is in the list.
function mark(text, q) {
  const frag = document.createDocumentFragment();
  if (!q) {
    frag.appendChild(document.createTextNode(text));
    return frag;
  }
  const i = text.toLowerCase().indexOf(q);
  if (i < 0) {
    frag.appendChild(document.createTextNode(text));
    return frag;
  }
  frag.appendChild(document.createTextNode(text.slice(0, i)));
  const m = document.createElement('mark');
  m.textContent = text.slice(i, i + q.length);
  frag.appendChild(m);
  frag.appendChild(document.createTextNode(text.slice(i + q.length)));
  return frag;
}

function openList(open) {
  $('testlist').hidden = !open;
  $('filter').setAttribute('aria-expanded', String(!!open));
  if (open) drawTestList();
}

async function pick(name) {
  openList(false);
  $('filter').value = name;
  // Choosing a test is the thing the welcome was there to get you to do.
  welcomed();
  if (RUN && name === RUN.test) return;
  verdict('running', '\u25cb running ' + name + '\u2026');
  const res = await fetch('/api/probe/select', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ test: name }),
  });
  if (!res.ok) {
    verdict('fail', '\u25cf ' + (await res.text()).trim());
    return;
  }
  // A different test is a different journey; where you were standing in the old
  // one means nothing in the new one.
  SELECTED = null;
  STEP = -1;
  RUN = await res.json();
  draw();
  loadTests(); // the chosen test now has a handle, and the list should say so
}

// First run, once.
// The welcome stands in for the journey the first time the application opens
// with nothing chosen, and never again after a test has been picked. Kept in
// localStorage because it is a fact about this reader on this machine — the
// same place Chrome keeps the window position — and not about any session.
const WELCOME_KEY = 'plum-welcomed';

function wantsWelcome() {
  if (!RUN || RUN.why !== 'nothing chosen yet') return false;
  try { return localStorage.getItem(WELCOME_KEY) !== '1'; } catch (_) { return false; }
}

function welcomed() {
  try { localStorage.setItem(WELCOME_KEY, '1'); } catch (_) {}
  $('welcome').hidden = true;
}

function drawWelcome() {
  const w = $('welcome');
  const show = wantsWelcome();
  w.hidden = !show;
  if (show) {
    $('welcome-go').onclick = () => { $('filter').focus(); };
  }
}

function drawStepper() {
  const at = $('stepat');
  if (!STATIONS.length) { at.textContent = ''; return; }
  at.textContent = STEP < 0 ? STATIONS.length + ' steps' : (STEP + 1) + ' / ' + STATIONS.length;
  $('back').disabled = STEP <= 0;
  $('fwd').disabled = STEP >= STATIONS.length - 1;
}

// callStack is the stations still open above this one — what a debugger shows as
// the stack. It is read back off the journey rather than recorded separately:
// the nearest earlier station at each shallower depth is the frame that called
// into this one.
function callStack(n) {
  const out = [];
  let want = STATIONS[n].well.depth - 1;
  for (let i = n - 1; i >= 0 && want >= 0; i--) {
    if (STATIONS[i].well.depth === want) {
      out.unshift(STATIONS[i]);
      want--;
    }
  }
  return out;
}

// nextCall is the station this frame calls into next, if the next step goes
// deeper. That is what makes the highlighted line meaningful: you are here, and
// this is the line you leave from.
function nextCall(n) {
  const here = STATIONS[n];
  const next = STATIONS[n + 1];
  if (!next || next.well.depth !== here.well.depth + 1) return null;
  return next;
}

// drawSource renders the declaration a line at a time so a step can point into
// it, and every identifier as something you can click. Line numbers are the
// file's, not the fragment's, so they match what an editor shows.
function drawSource(pc, firstLine, callLine) {
  const box = $('detail-src');
  box.innerHTML = '';
  const lang = languageOf((SELECTED || '').split('::')[0]);
  const lines = (pc.source || '').split('\n');
  SRC_LINES = lines;
  SRC_FIRST = firstLine;
  let hereRow = null;

  lines.forEach((text, i) => {
    const row = document.createElement('div');
    row.className = 'ln';
    const n = document.createElement('span');
    n.className = 'n';
    n.textContent = firstLine ? firstLine + i : '';
    const c = document.createElement('span');
    c.className = 'c';

    for (const t of codeTokens(text, lang)) {
      if (t.kind === 'id') {
        // The unit a reader points at. "Where does n come from" is a question
        // about a name, so the name is the thing that takes the click.
        const b = document.createElement('button');
        b.className = 'id';
        b.type = 'button';
        b.textContent = t.text;
        b.dataset.name = t.text;
        b.onclick = (e) => { e.stopPropagation(); inspect(t.text, false, firstLine ? firstLine + i : 0); };
        c.appendChild(b);
        continue;
      }
      if (!t.kind) { c.appendChild(document.createTextNode(t.text)); continue; }
      const el = document.createElement('span');
      el.className = t.kind;
      el.textContent = t.text;
      c.appendChild(el);
    }

    if (callLine && firstLine && firstLine + i === callLine) {
      row.classList.add('here');
      hereRow = row;
    }
    row.append(n, c);
    box.appendChild(row);
  });
  if (hereRow) hereRow.scrollIntoView({ block: 'center' });
  if (INSPECTING) inspect(INSPECTING, true, INSPECT_LINE); // keep the highlight across a redraw
}

// inspect answers "what is this name, and where does it lead" for the identifier
// under the cursor.
//
// Everything it says comes from something recorded or parsed, never guessed: the
// values are what the run actually passed, the call target is the edge the
// extractor resolved, and the occurrences are this declaration's own text. Where
// it cannot tell, it says which of those it is short of rather than inventing a
// plausible answer — a navigator that is sometimes wrong is worse than one that
// is sometimes silent.
async function inspect(name, keep, atLine) {
  const box = $('detail-inspect');
  if (INSPECTING === name && !keep) { closeInspect(); return; }
  INSPECTING = name;
  box.hidden = false;
  box.innerHTML = '';

  // Light every occurrence in the body straight away, so the click feels
  // answered before the answer arrives.
  const hits = [];
  document.querySelectorAll('#detail-src .id').forEach((el) => {
    const on = el.dataset.name === name;
    el.classList.toggle('lit', on);
    if (on) hits.push(el);
  });

  const head = document.createElement('div');
  head.className = 'ihead';
  const nm = document.createElement('b');
  nm.textContent = name;
  const close = document.createElement('button');
  close.className = 'x';
  close.type = 'button';
  close.textContent = '\u00d7';
  close.onclick = closeInspect;
  head.append(nm, close);
  box.appendChild(head);

  const body = document.createElement('div');
  body.className = 'ibody';
  body.textContent = 'resolving\u2026';
  box.appendChild(body);

  let res = {};
  try {
    // The line matters: it says which scope the reader is standing in, and two
    // variables sharing a name in one function are two variables.
    if (atLine) INSPECT_LINE = atLine;
    res = await fetch('/api/resolve?symbol=' + encodeURIComponent(SELECTED)
      + '&name=' + encodeURIComponent(name)
      + (INSPECT_LINE ? '&line=' + INSPECT_LINE : '')).then((r) => r.json());
  } catch (e) {
    res = { kind: 'unknown', note: 'could not reach the resolver' };
  }
  if (INSPECTING !== name) return; // they clicked on while this was in flight
  body.innerHTML = '';

  const row = STATIONS[STEP];
  const fv = row ? (RUN.values || {})[row.index] : null;
  const arg = ((fv && fv.args) || []).find((a) => a.name === name);

  // What it is, from the language rather than from a pattern.
  const kind = res.kind || 'unknown';
  fact(body, kind + (res.type ? ' \u00b7 ' + res.type : ''));
  if (res.doc) quote(body, res.doc);

  // Where it came from. This is the question — "where is n derived" — and the
  // expression on the right of its declaration is the answer.
  if (res.derived_from) {
    val(body, 'derived from', res.derived_from);
    if (res.declared_at) lineLink(body, 'declared at', [res.declared_at]);
  } else if (res.declared_at) {
    lineLink(body, 'declared at', [res.declared_at]);
  }

  // What the run actually saw, where the run saw it. Recorded evidence outranks
  // anything static, so it goes next to the static answer rather than instead.
  if (arg) {
    val(body, 'came in as', arg.value);
    if (arg.after) val(body, 'left as', arg.after, true);
  }

  if (res.writes && res.writes.length) lineLink(body, 'written at', res.writes);
  if (res.reads && res.reads.length) lineLink(body, 'read at', res.reads);

  // Where it leads. A call that a frame on this journey entered is a step you
  // can take; one that exists but was not entered is a file you can open.
  if (kind === 'call') {
    const call = (CURRENT_PC.call_sites || []).find((cs) =>
      cs.callee_raw === name || lastSegment(cs.callee_raw) === name);
    if (call && call.rationale) quote(body, call.rationale);
    const target = call && call.callee;
    const at = target ? STATIONS.findIndex((st) => st.well.symbol === target) : -1;
    if (at >= 0) {
      action(body, '\u2192 step to ' + (STATIONS[at].well.label || target), () => step(at));
    } else if (target && !target.startsWith('::')) {
      action(body, '\u2192 open ' + lastSegment(target), () => openSymbol(target));
    } else {
      fact(body, 'it leaves the recorded set \u2014 nothing on this journey entered it');
    }
  }

  if (res.note) note(body, res.note);
}

function lineLink(box, label, lines) {
  const row = document.createElement('div');
  row.className = 'ilines';
  row.append(label + ' ');
  lines.forEach((ln, i) => {
    const b = document.createElement('button');
    b.className = 'lnk';
    b.type = 'button';
    b.textContent = ln;
    b.onclick = () => jumpToLine(ln);
    row.appendChild(b);
    if (i < lines.length - 1) row.append(', ');
  });
  box.appendChild(row);
}

function lastSegment(id) {
  if (!id) return '';
  const after = id.split('::').pop();
  return after.split('.').pop();
}

function lineOf(el) {
  const row = el.closest('.ln');
  const n = row && row.querySelector('.n');
  return n ? parseInt(n.textContent, 10) || 0 : 0;
}

function jumpToLine(ln) {
  document.querySelectorAll('#detail-src .ln').forEach((row) => {
    const n = row.querySelector('.n');
    if (n && parseInt(n.textContent, 10) === ln) row.scrollIntoView({ block: 'center' });
  });
}

function closeInspect() {
  INSPECTING = null;
  INSPECT_LINE = 0;
  $('detail-inspect').hidden = true;
  document.querySelectorAll('#detail-src .id.lit').forEach((el) => el.classList.remove('lit'));
}

// openSymbol follows a call that no frame on this journey entered — unchanged
// code the run passed through, or code it never reached at all.
async function openSymbol(symbol) {
  try {
    const pc = await fetch('/api/symbol/' + encodeURIComponent(symbol)).then((r) => r.json());
    if (!pc.source) return;
    CURRENT_PC = pc;
    SELECTED = symbol;
    $('detail-name').textContent = lastSegment(symbol);
    const file = symbol.split('::')[0];
    const first = (pc.declaration || {}).line_start || 0;
    $('detail-where').textContent = first ? file + ':' + first : file;
    $('detail-at').className = 'at unknown';
    $('detail-at').textContent = 'followed from a call — not a frame on this journey';
    $('detail-values').innerHTML = '';
    $('detail-sentence').innerHTML = '';
    closeInspect();
    drawSource(pc, first, 0);
  } catch (e) { /* the pane keeps what it had */ }
}

function fact(box, text) {
  const el = document.createElement('div');
  el.className = 'ifact';
  el.textContent = text;
  box.appendChild(el);
}

function quote(box, text) {
  const el = document.createElement('div');
  el.className = 'iquote';
  el.textContent = text;
  box.appendChild(el);
}

function note(box, text) {
  const el = document.createElement('div');
  el.className = 'inote';
  el.textContent = text;
  box.appendChild(el);
}

function val(box, label, value, after) {
  const row = document.createElement('div');
  row.className = 'ival' + (after ? ' after' : '');
  const k = document.createElement('span');
  k.className = 'k';
  k.textContent = label;
  const v = document.createElement('span');
  v.className = 'v';
  v.textContent = value;
  row.append(k, v);
  box.appendChild(row);
}

function action(box, label, fn) {
  const b = document.createElement('button');
  b.className = 'igo';
  b.type = 'button';
  b.textContent = label;
  b.onclick = fn;
  box.appendChild(b);
}

// offerExplain records what is selected and where it sits on screen, so the
// tooltip can be put over it.
function offerExplain() {
  const sel = window.getSelection();
  const text = sel ? String(sel) : '';
  const src = $('detail-src');
  const inside = sel && sel.rangeCount && src && src.contains(sel.getRangeAt(0).commonAncestorContainer);
  if (!inside || text.trim().length < 2) {
    SELECTION = null;
    if (!EXPLAIN_BUSY) showTip(false);
    return;
  }
  const rect = sel.getRangeAt(0).getBoundingClientRect();
  SELECTION = {
    text,
    from: lineAtNode(sel.anchorNode),
    to: lineAtNode(sel.focusNode),
    x: rect.left + rect.width / 2,
    y: rect.top,
  };
  showTip(true);
}

// showTip places the tooltip over the selection. Position is fixed to the
// viewport and recomputed from the live range, so it stays put through a scroll
// of the pane rather than drifting off the text it is about.
function showTip(on) {
  const tip = $('tip');
  if (!on || !SELECTION) {
    if (EXPLAIN_BUSY) return; // never yank it away mid-question
    tip.hidden = true;
    tip.classList.remove('on');
    return;
  }
  const sel = window.getSelection();
  if (sel && sel.rangeCount) {
    const rect = sel.getRangeAt(0).getBoundingClientRect();
    if (rect.width || rect.height) {
      SELECTION.x = rect.left + rect.width / 2;
      SELECTION.y = rect.top;
    }
  }
  tip.innerHTML = '';
  const label = document.createElement('span');
  label.textContent = EXPLAIN_BUSY ? 'asking\u2026' : 'explain';
  tip.appendChild(label);
  if (!EXPLAIN_BUSY) {
    const from = Math.min(SELECTION.from, SELECTION.to);
    const to = Math.max(SELECTION.from, SELECTION.to);
    if (from) {
      const lines = document.createElement('span');
      lines.className = 'lines';
      lines.textContent = from === to ? ' line ' + from : ' lines ' + from + '\u2013' + to;
      tip.appendChild(lines);
    }
  }
  tip.classList.toggle('busy', EXPLAIN_BUSY);
  // Kept inside the viewport: a tooltip half off the screen is worse than one
  // slightly off-centre from what it points at.
  const x = Math.max(60, Math.min(window.innerWidth - 60, SELECTION.x));
  tip.style.left = x + 'px';
  tip.style.top = Math.max(28, SELECTION.y) + 'px';
  tip.hidden = false;
  requestAnimationFrame(() => tip.classList.add('on'));
}

function lineAtNode(node) {
  const el = node && (node.nodeType === 1 ? node : node.parentElement);
  const row = el && el.closest ? el.closest('.ln') : null;
  const n = row && row.querySelector('.n');
  return n ? parseInt(n.textContent, 10) || 0 : 0;
}

async function explainSelection() {
  if (!SELECTION || EXPLAIN_BUSY) return;
  EXPLAIN_BUSY = true;
  showTip(true);

  const box = $('explained');
  box.hidden = false;
  box.innerHTML = '';
  const wait = document.createElement('div');
  wait.className = 'thinking';
  wait.textContent = 'asking\u2026';
  box.appendChild(wait);

  const row = STATIONS[STEP];
  try {
    const res = await fetch('/api/explain', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({
        symbol: SELECTED,
        selection: SELECTION.text,
        from_line: Math.min(SELECTION.from, SELECTION.to),
        to_line: Math.max(SELECTION.from, SELECTION.to),
        station: row ? row.index : 0,
      }),
    });
    const d = await res.json();
    if (d.status === 'pending' && d.id) {
      // An agent answers in its own time. Say where the question went, then
      // wait: a spinner that names its destination is a different thing from
      // one that just spins.
      drawExplanation(d);
      pollExplain(d);
      return;
    }
    drawExplanation(d);
  } catch (e) {
    drawExplanation({ status: 'failed', error: 'could not reach plum' });
  } finally {
    if (!EXPLAIN_PENDING) { EXPLAIN_BUSY = false; showTip(false); }
  }
}

// pollExplain waits for the answer file the agent writes. Backs off from half a
// second to five: an agent reading a brief and writing a reply takes as long as
// it takes, and hammering the endpoint does not make it faster.
function pollExplain(started) {
  EXPLAIN_PENDING = started.id;
  let wait = 500;
  const tick = async () => {
    if (EXPLAIN_PENDING !== started.id) return;
    let d;
    try {
      d = await (await fetch('/api/explain/' + encodeURIComponent(started.id))).json();
    } catch (e) {
      d = { status: 'pending' };
    }
    if (d.status === 'pending') {
      wait = Math.min(5000, Math.round(wait * 1.4));
      setTimeout(tick, wait);
      return;
    }
    EXPLAIN_PENDING = null;
    EXPLAIN_BUSY = false;
    showTip(false);
    drawExplanation({ ...started, ...d });
  };
  setTimeout(tick, wait);
}

function drawExplanation(d) {
  const box = $('explained');
  box.innerHTML = '';
  box.hidden = false;

  const who = document.createElement('div');
  who.className = 'who';
  const bits = [];
  if (d.status === 'pending') {
    bits.push(d.route === 'file' ? 'waiting for an agent' : 'asked ' + (d.target || 'your agent'));
  } else if (d.status === 'failed') {
    bits.push('nothing could answer it');
  } else {
    bits.push(d.route === 'tmux' ? (d.target || 'your agent') : (d.target || 'model'));
    // Whether the run's values went with the question changes what the answer
    // is worth, so it is stated rather than left for the reader to assume.
    bits.push(d.grounded
      ? 'with this run\u2019s recorded values'
      : 'from the code alone \u2014 no values recorded for this frame');
    if (d.took_ms) bits.push((d.took_ms / 1000).toFixed(1) + 's');
  }
  who.textContent = bits.join(' \u00b7 ');

  const x = document.createElement('button');
  x.className = 'x';
  x.type = 'button';
  x.title = 'close';
  x.textContent = '\u00d7';
  x.onclick = () => { EXPLAIN_PENDING = null; EXPLAIN_BUSY = false; box.hidden = true; };
  who.appendChild(x);
  box.appendChild(who);

  if (d.note && d.route !== 'file') {
    const n = document.createElement('div');
    n.className = 'enote';
    n.textContent = d.note;
    box.appendChild(n);
  }

  if (d.status === 'pending') {
    const p = document.createElement('div');
    p.className = 'thinking';
    p.textContent = d.route === 'file'
      ? 'the question is written down and waiting \u2014 point any agent at it'
      : 'waiting for it to answer\u2026';
    box.appendChild(p);
    if (d.route === 'file') drawWaiting(box, d);
    else if (d.id) {
      const where = document.createElement('div');
      where.className = 'enote';
      where.textContent = 'it is in .plum/ask/' + d.id + '.md';
      box.appendChild(where);
    }
    reveal(box);
    return;
  }

  if (d.status === 'failed' || d.error) {
    const err = document.createElement('div');
    err.className = 'failed';
    err.textContent = d.error || 'no answer';
    box.appendChild(err);
    drawRecovery(box, d);
    reveal(box);
    return;
  }

  box.appendChild(markdown(String(d.answer || '')));
  reveal(box);
}

// markdown renders the subset an answer actually uses: headings, bullets, bold,
// inline code and fenced blocks.
//
// Written rather than imported because the page has no build step and a 100KB
// budget, and every library that does this properly is larger than the whole
// page. It renders text nodes rather than assigning innerHTML, so a model that
// emits a stray angle bracket cannot put markup into the document.
function markdown(text) {
  const frag = document.createDocumentFragment();
  const lines = text.replace(/\r/g, '').split('\n');
  let list = null, para = null, fence = null;

  const endPara = () => { para = null; };
  const endList = () => { list = null; };

  for (const line of lines) {
    // Fenced code travels verbatim: it is the one place where what was written
    // matters more than how it reads.
    if (/^\s*```/.test(line)) {
      if (fence) { frag.appendChild(fence); fence = null; continue; }
      endPara(); endList();
      fence = document.createElement('pre');
      continue;
    }
    if (fence) {
      fence.appendChild(document.createTextNode(line + '\n'));
      continue;
    }

    if (!line.trim()) { endPara(); endList(); continue; }

    const heading = line.match(/^(#{1,4})\s+(.*)$/);
    if (heading) {
      endPara(); endList();
      const h = document.createElement('div');
      h.className = 'mh mh' + heading[1].length;
      h.appendChild(inline(heading[2]));
      frag.appendChild(h);
      continue;
    }

    const bullet = line.match(/^\s*[-*]\s+(.*)$/);
    if (bullet) {
      endPara();
      const nested = /^\s{2,}/.test(line);
      if (!list) { list = document.createElement('ul'); frag.appendChild(list); }
      const li = document.createElement('li');
      if (nested) li.className = 'sub';
      li.appendChild(inline(bullet[1]));
      list.appendChild(li);
      continue;
    }

    const numbered = line.match(/^\s*\d+[.)]\s+(.*)$/);
    if (numbered) {
      endPara();
      if (!list) { list = document.createElement('ul'); frag.appendChild(list); }
      const li = document.createElement('li');
      li.appendChild(inline(numbered[1]));
      list.appendChild(li);
      continue;
    }

    endList();
    if (!para) { para = document.createElement('p'); frag.appendChild(para); }
    else para.appendChild(document.createTextNode(' '));
    para.appendChild(inline(line));
  }
  if (fence) frag.appendChild(fence);
  return frag;
}

// inline handles `code`, **bold** and *emphasis* within one line.
function inline(text) {
  const frag = document.createDocumentFragment();
  // One pass, so a backtick inside bold markers cannot be mistaken for a fence.
  const re = /`([^`]+)`|\*\*([^*]+)\*\*|\*([^*]+)\*/g;
  let last = 0, m;
  while ((m = re.exec(text)) !== null) {
    if (m.index > last) frag.appendChild(document.createTextNode(text.slice(last, m.index)));
    let el;
    if (m[1] !== undefined) { el = document.createElement('code'); el.textContent = m[1]; }
    else if (m[2] !== undefined) { el = document.createElement('b'); el.textContent = m[2]; }
    else { el = document.createElement('em'); el.textContent = m[3]; }
    frag.appendChild(el);
    last = m.index + m[0].length;
  }
  if (last < text.length) frag.appendChild(document.createTextNode(text.slice(last)));
  return frag;
}

// reveal makes sure the answer is where the reader is looking. The pane scrolls
// independently and the panel sits above the source, so an answer could land
// entirely off-screen — which reads exactly like nothing having happened.
function reveal(box) {
  box.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
  box.classList.remove('flash');
  // Reflow, so the animation restarts on a second answer in the same place.
  void box.offsetWidth;
  box.classList.add('flash');
}

// drawWaiting is what an agent plum cannot reach looks like.
//
// An agent in an IDE has no tmux pane and never will, and that is a completely
// ordinary way to work. The question is a file either way, so nothing is
// broken — what is missing is telling that agent to look, which is one paste.
function drawWaiting(box, d) {
  if (d.instruction) {
    const row = document.createElement('div');
    row.className = 'erow';
    const copy = document.createElement('button');
    copy.type = 'button';
    copy.className = 'primary';
    copy.textContent = 'copy the one line to paste to your agent';
    copy.onclick = async () => {
      try {
        await navigator.clipboard.writeText(d.instruction);
        copy.textContent = 'copied \u2014 paste it to your agent';
      } catch (e) {
        copy.textContent = 'could not copy';
      }
    };
    row.appendChild(copy);
    box.appendChild(row);

    const line = document.createElement('div');
    line.className = 'instruction';
    line.textContent = d.instruction;
    box.appendChild(line);
  }

  const alt = document.createElement('div');
  alt.className = 'erow';
  if (d.can_ask_api) {
    // Offered, not done. A developer running their own agent has already paid
    // for it, and spending their API quota instead is not plum's call.
    const api = document.createElement('button');
    api.type = 'button';
    api.textContent = 'or ask the API instead';
    api.onclick = async () => {
      api.disabled = true;
      api.textContent = 'asking\u2026';
      try {
        const got = await (await fetch('/api/explain-api/' + encodeURIComponent(d.id))).json();
        EXPLAIN_PENDING = null;
        EXPLAIN_BUSY = false;
        drawExplanation({ ...d, ...got });
      } catch (e) {
        api.textContent = 'could not reach the model';
      }
    };
    alt.appendChild(api);
  }
  if (d.panes && d.panes.length) {
    const pick = document.createElement('button');
    pick.type = 'button';
    pick.textContent = 'or point plum at a tmux pane';
    pick.onclick = () => { pick.remove(); drawRecovery(box, d); reveal(box); };
    alt.appendChild(pick);
  }
  if (alt.children.length) box.appendChild(alt);

  if (d.note) {
    const n = document.createElement('div');
    n.className = 'enote';
    n.textContent = d.note;
    box.appendChild(n);
  }
}

// drawRecovery turns a dead end into a next step. Nothing could answer the
// question, but the question exists and is worth something: it can be pasted
// anywhere, and if tmux is running, plum can be pointed at the right pane
// without editing a config file and starting over.
function drawRecovery(box, d) {
  if (d.panes && d.panes.length) {
    const label = document.createElement('div');
    label.className = 'enote';
    label.textContent = 'point plum at the pane your agent is in:';
    box.appendChild(label);
    const list = document.createElement('div');
    list.className = 'panes';
    for (const p of d.panes) {
      const b = document.createElement('button');
      b.className = 'pane' + (p.is_agent ? ' agent' : '');
      b.type = 'button';
      const t = document.createElement('b');
      t.textContent = p.target;
      const c = document.createElement('span');
      c.textContent = ' ' + (p.command || '') + (p.path ? ' \u00b7 ' + shortPath(p.path) : '');
      b.append(t, c);
      // A pane that cannot answer is shown, because plum may not recognise
      // every agent — but it is not offered as if it would work.
      if (!p.is_agent) {
        const no = document.createElement('span');
        no.className = 'notagent';
        no.textContent = ' not an agent';
        b.appendChild(no);
      }
      b.onclick = () => usePane(p.target, false);
      list.appendChild(b);
    }
    box.appendChild(list);
  }

  if (d.brief) {
    const row = document.createElement('div');
    row.className = 'erow';
    const copy = document.createElement('button');
    copy.type = 'button';
    copy.textContent = 'copy the question';
    copy.onclick = async () => {
      try {
        await navigator.clipboard.writeText(d.brief);
        copy.textContent = 'copied \u2014 paste it to any agent';
      } catch (e) {
        copy.textContent = 'could not copy';
      }
    };
    row.appendChild(copy);
    if (d.brief_path) {
      const where = document.createElement('span');
      where.className = 'enote';
      where.textContent = ' or answer ' + d.brief_path;
      row.appendChild(where);
    }
    box.appendChild(row);
  }
}

function shortPath(p) {
  const parts = String(p).split('/').filter(Boolean);
  return parts.length > 2 ? '\u2026/' + parts.slice(-2).join('/') : p;
}

async function usePane(target) {
  try {
    await fetch('/api/pane', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ target }),
    });
  } catch (e) { /* the retry will report it */ }
  // Pointing at a pane is only useful if the question then goes there, so it
  // is asked again rather than left for the reader to redo.
  explainSelection();
}

boot();
