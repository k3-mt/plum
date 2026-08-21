// The probe page: one test, what it did, and what it does after you change
// something. It asks the server for a run and draws it. That is the whole page.
let RUN = null, STATIONS = [], SELECTED = null;
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
  $('back').onclick = () => step(STEP - 1);
  $('fwd').onclick = () => step(STEP + 1);
  document.addEventListener('keydown', onKey);
  window.addEventListener('resize', fitToViewport);
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
  $('test').textContent = RUN.test || '';
  document.title = 'plum · ' + (RUN.test || 'probe');

  if (RUN.error) {
    verdict('fail', '● could not run');
  } else if (RUN.why === 'not run yet') {
    verdict('running', '○ not run yet');
  } else {
    // "failed" for a test that ran and disagreed with the code is a different
    // report from one for a command that never started.
    const label = RUN.passed ? '● passed'
      : RUN.recorded ? '● failed'
      : '● did not run';
    verdict(RUN.passed ? 'pass' : 'fail', label + '  ' + duration(RUN.duration_ms));
  }

  $('why').innerHTML = '';
  if (RUN.why && RUN.why !== 'not run yet') {
    $('why').append('ran because ');
    const b = document.createElement('b');
    b.textContent = RUN.why;
    $('why').append(b, RUN.at ? '  ·  ' + RUN.at : '');
  } else {
    $('why').textContent = 'press run, or save a file it touches';
  }

  drawTrace();

  // The test's own output only earns space when something went wrong. When it
  // passed, the picture is the answer and the output is "ok  0.168s".
  const out = $('output');
  const failed = !RUN.passed || RUN.error;
  out.hidden = !failed || !(RUN.output || RUN.error);
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
    // Three different reasons for an empty picture, and saying the wrong one
    // sends the reader looking in the wrong place. A build failure is not a
    // test that declined to enter your code.
    if (RUN.error) {
      li.textContent = 'Could not run it — ' + RUN.error;
    } else if (!RUN.recorded) {
      li.textContent = 'The test never ran. Nothing was recorded at all, so this '
        + 'is the command failing rather than the code — the output below says why.';
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

  const vals = valuesOf(row.step);
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
  try {
    const pc = await fetch('/api/symbol/' + encodeURIComponent(row.well.symbol)).then((r) => r.json());
    if (SELECTED !== row.well.symbol) return; // they stepped on while this was in flight
    const first = (pc.declaration || {}).line_start || 0;
    if (first) $('detail-where').textContent = file + ':' + first;
    if (!pc.source) {
      $('detail-src').textContent = '(no source for this symbol in the working tree)';
      return;
    }
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

// valuesOf pulls just what the run produced out of the sentence — the arguments
// and the return. The prose around them is available on expanding the row.
function valuesOf(step) {
  if (!step || !step.spans) return '';
  const vals = step.spans.filter((s) => s.kind === 'value').map((s) => s.text);
  if (!vals.length) return '';
  return vals.map((v) => (v.length > 60 ? v.slice(0, 59) + '…' : v)).join('  →  ');
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
// it. Line numbers are the file's, not the fragment's, so they match what an
// editor shows.
function drawSource(pc, firstLine, callLine) {
  const box = $('detail-src');
  box.innerHTML = '';
  const lang = languageOf((SELECTED || '').split('::')[0]);
  const lines = (pc.source || '').split('\n');
  let hereRow = null;
  lines.forEach((text, i) => {
    const row = document.createElement('div');
    row.className = 'ln';
    const n = document.createElement('span');
    n.className = 'n';
    n.textContent = firstLine ? firstLine + i : '';
    const c = document.createElement('span');
    c.className = 'c';
    c.appendChild(highlight(text, lang));
    if (callLine && firstLine && firstLine + i === callLine) {
      row.classList.add('here');
      hereRow = row;
    }
    row.append(n, c);
    box.appendChild(row);
  });
  if (hereRow) hereRow.scrollIntoView({ block: 'center' });
}

boot();
