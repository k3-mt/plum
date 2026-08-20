// The landscape is a reaction coordinate, not a flame graph: vertical is stack
// depth, descent is entering a call, ascent is returning, and the path must close.
const SVG = 'http://www.w3.org/2000/svg';
let DATA = null, SELECTED = null, DWELL = null;

const el = (name, attrs = {}, text) => {
  const n = document.createElementNS(SVG, name);
  for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v);
  if (text !== undefined) n.textContent = text;
  return n;
};

async function boot() {
  DATA = await (await fetch('/api/landscape')).json();
  const g = document.getElementById('gate');
  g.textContent = DATA.gate.fired ? 'GATE FIRED — ' + DATA.gate.reasons.join(' · ') : 'gate clear';
  const notes = document.getElementById('notes');
  notes.innerHTML = (DATA.notes || []).map(n =>
    n.replace(/\*\*(.+?)\*\*/g, '<b>$1</b>').replace(/`(.+?)`/g, '<code>$1</code>')).join(' · ');
  if ((DATA.unannotated || []).length) {
    notes.innerHTML += '<br>expensive and unexplained: ' +
      DATA.unannotated.map(u => u.replace(/`(.+?)`/g, '<code>$1</code>')).join('; ');
  }
  document.getElementById('summary').textContent = DATA.summary || '';
  drawNarration();
  drawReading();
  draw();
  document.getElementById('done').onclick = async () => {
    await fetch('/api/done', { method: 'POST' });
    document.getElementById('done').textContent = 'quiz unlocked — run: plum quiz';
  };
  document.getElementById('asked').onclick = ask;
  document.getElementById('route').textContent =
    'answers via ' + (DATA.ask_route === 'tmux' ? 'your tmux agent session' : DATA.ask_route);
  for (const kind of ['journal', 'claim', 'comment']) {
    document.getElementById('keep-' + kind).onclick = () => keep(kind);
  }
  document.getElementById('q').addEventListener('keydown', e => { if (e.key === 'Enter') ask(); });
}

// The narration is the same evidence the landscape draws, said in sentences.
// Hovering a shape shows its line; the list below shows all of them in order.
function drawNarration() {
  const list = document.getElementById('steps');
  list.innerHTML = '';
  (DATA.narration || []).forEach((step, i) => {
    const li = document.createElement('li');
    li.className = step.kind;
    li.dataset.step = i;
    li.textContent = step.text;
    if (step.note) {
      const warn = document.createElement('span');
      warn.className = 'warn';
      warn.textContent = '\u26a0 ' + step.note;
      li.appendChild(warn);
    }
    if (step.kind === 'frame' && step.index >= 0) {
      li.onclick = () => select(DATA.landscape.wells[step.index].symbol);
      li.onmouseenter = () => showStep(i);
    }
    list.appendChild(li);
  });
}

// The reading is the only part of this page a model wrote. It is kept visually
// apart from everything else, and labelled, because prose that reads like a
// record but was inferred is worse than no prose at all.
function drawReading() {
  const box = document.getElementById('reading-body');
  const r = DATA.interpretation;
  if (!r) return;
  box.className = '';
  box.innerHTML = '';

  const meta = document.createElement('div');
  meta.className = 'meta';
  meta.textContent = r.provider + ' · ' + r.generated_at;
  box.appendChild(meta);

  if (r.stale) {
    const stale = document.createElement('div');
    stale.className = 'stale';
    stale.textContent = '\u26a0 stale — ' + r.stale_reason;
    box.appendChild(stale);
  }

  const body = document.createElement('div');
  body.className = 'body';
  // Headings and emphasis only; the text is displayed as written otherwise.
  body.innerHTML = escape(r.markdown)
    .replace(/^#+\s*(.+)$/gm, '<b>$1</b>')
    .replace(/\*\*(.+?)\*\*/g, '<b>$1</b>')
    .replace(/`([^`]+)`/g, '<code>$1</code>');
  box.appendChild(body);
}

// stepFor maps a shape back to its sentence: frames by well index, transitions
// by the well they land on.
function stepFor(kind, idx) {
  const steps = DATA.narration || [];
  for (let i = 0; i < steps.length; i++) {
    if (kind === 'frame' && steps[i].kind === 'frame' && steps[i].index === idx) return i;
    if (kind === 'transition' && steps[i].kind === 'transition' &&
        steps[i + 1] && steps[i + 1].index === idx) return i;
  }
  return -1;
}

function showStep(i) {
  const step = (DATA.narration || [])[i];
  const box = document.getElementById('hover');
  if (!step) { box.textContent = 'Hover a frame or a step to read what happened there.'; return; }
  box.textContent = step.text;
  if (step.note) {
    const warn = document.createElement('span');
    warn.className = 'warn';
    warn.textContent = '\u26a0 ' + step.note;
    box.appendChild(warn);
  }
  document.querySelectorAll('#steps li').forEach((li) => {
    li.classList.toggle('active', Number(li.dataset.step) === i);
  });
}

function draw() {
  const wells = DATA.landscape.wells || [];
  const bars = DATA.landscape.barriers || [];
  const svg = document.getElementById('svg');
  svg.innerHTML = '';
  const closed = document.getElementById('closed');
  closed.textContent = DATA.landscape.closed === false
    ? '⚠ the path does not close — ' + DATA.landscape.open_frame + ' was entered and never returned'
    : '';
  if (!wells.length) {
    svg.setAttribute('height', 40);
    svg.appendChild(el('text', { x: 8, y: 24, class: 'blabel' },
      'No trace recorded yet. Run: plum trace'));
    return;
  }

  const W = 132, ROW = 74, PAD = 40;
  const maxDepth = Math.max(...wells.map(w => w.depth));
  const width = PAD * 2 + wells.length * W;
  const height = PAD * 2 + (maxDepth + 1) * ROW + 30;
  svg.setAttribute('width', width);
  svg.setAttribute('height', height);
  svg.setAttribute('viewBox', `0 0 ${width} ${height}`);

  const cx = i => PAD + i * W + W / 2;
  const cy = d => PAD + d * ROW;

  // Barriers first, so wells sit on top of the transition arcs.
  for (const b of bars) {
    const x1 = cx(b.from), y1 = cy(wells[b.from].depth);
    const x2 = cx(b.to), y2 = cy(wells[b.to].depth);
    const lift = 12 + b.height * 52;
    const mid = (x1 + x2) / 2;
    let d, stroke = '#5c6b74', dash = '';
    if (b.direction === 'unwind') {
      // A cliff: straight from the raising depth to the catching depth.
      d = `M${x1},${y1} L${mid},${y1} L${mid},${y2} L${x2},${y2}`;
      stroke = 'var(--unwind)';
    } else {
      const peak = Math.min(y1, y2) - lift;
      d = `M${x1},${y1} C${mid},${peak} ${mid},${peak} ${x2},${y2}`;
      if (b.direction === 'ascend') dash = '4 3';
    }
    svg.appendChild(el('path', { d, fill: 'none', stroke, 'stroke-width': 1 + b.height * 2.5, 'stroke-dasharray': dash, opacity: .85 }));
    // A drawn barrier is a couple of pixels wide; this invisible one is what a
    // pointer can actually hit.
    const hit = el('path', { d, fill: 'none', stroke: 'transparent', 'stroke-width': 18 });
    hit.style.cursor = 'help';
    const bStep = stepFor('transition', b.to);
    hit.onmouseenter = () => showStep(bStep);
    svg.appendChild(hit);
    const label = fmtNs(b.cost_ns) + (b.kind !== 'compute' ? ' · ' + b.kind : '') +
      (b.frames > 1 ? ' · ' + b.frames + ' frames' : '');
    svg.appendChild(el('text', { x: mid, y: Math.min(y1, y2) - lift - 4, 'text-anchor': 'middle', class: 'blabel' }, label));
    if (b.rationale) {
      svg.appendChild(el('text', { x: mid, y: Math.min(y1, y2) - lift + 8, 'text-anchor': 'middle', class: 'blabel' },
        '“' + trunc(b.rationale, 28) + '”'));
    } else if (b.height >= 0.6 && b.direction === 'descend') {
      svg.appendChild(el('text', { x: mid, y: Math.min(y1, y2) - lift + 8, 'text-anchor': 'middle', class: 'blabel' }, '(unexplained)'));
    }
  }

  wells.forEach((w, i) => {
    const g = el('g', { class: 'well' });
    const x = cx(i) - W / 2 + 10, y = cy(w.depth);
    const fill = w.phase === 'escape' ? 'var(--unwind)'
      : w.context ? 'var(--context)'
      : (w.risk ? 'var(--risk)' : (w.phase === 'resume' ? 'var(--resume)' : 'var(--enter)'));
    const rect = el('rect', {
      x, y, width: W - 20, height: 26, rx: 3, fill,
      opacity: w.context ? .3 : (w.phase === 'resume' ? .45 : .9),
      stroke: w.doc ? 'none' : 'var(--enter)',
      'stroke-dasharray': w.doc ? '' : '3 2',
    });
    g.appendChild(rect);
    g.appendChild(el('text', { x: cx(i), y: y + 17, 'text-anchor': 'middle', class: 'wlabel', fill: '#0f1113' },
      trunc(w.label, 15)));
    if (w.context) g.appendChild(el('title', {}, w.symbol + ' — surrounding code, recorded for structure only'));
    g.appendChild(el('text', { x: cx(i), y: y + 38, 'text-anchor': 'middle', class: 'blabel' },
      'd' + w.depth + (w.phase === 'resume' ? ' · resumed' : w.phase === 'escape' ? ' · escaped' : '')));
    g.onclick = () => select(w.symbol, w);
    const wStep = stepFor('frame', i);
    g.onmouseenter = () => showStep(wStep);
    svg.appendChild(g);
  });
}

async function select(symbol, well) {
  if (SELECTED && DWELL) {
    send({ symbol: SELECTED, action: 'click', dwell_ms: Date.now() - DWELL });
  }
  SELECTED = symbol; DWELL = Date.now();
  const pc = await (await fetch('/api/symbol/' + encodeURIComponent(symbol))).json();
  document.getElementById('src').textContent = pc.source || '(source not available in the working tree)';
  send({ symbol, action: 'expand_source' });

  const body = document.getElementById('rail-body');
  const invs = (pc.invocations || []).map(e => {
    if (e.event === 'call') return `<div class="inv">call ${escape(JSON.stringify(e.args || {}))}</div>`;
    if (e.event === 'return') return `<div class="inv">return ${escape(e.result || '')}</div>`;
    return `<div class="inv raise">raised ${escape(e.exception || '')}</div>`;
  }).join('') || '<span class="muted">never executed by the traced run</span>';

  body.innerHTML = `
    <dl class="kv">
      <dt>symbol</dt><dd>${escape(symbol)}</dd>
      <dt>signature</dt><dd>${escape(pc.signature || '—')}</dd>
      <dt>doc</dt><dd>${pc.doc ? escape(pc.doc) : '<span class="warn">no declaration doc</span>'}</dd>
      <dt>recorded invocations</dt><dd>${invs}</dd>
      <dt>risks</dt><dd>${(pc.risks || []).map(r => `<div class="warn">${escape(r.kind)} — ${escape(r.note)}</div>`).join('') || '<span class="muted">none</span>'}</dd>
      <dt>rationale</dt><dd>${(pc.rationale || []).map(j => escape(j.rationale)).join('<br>') || '<span class="muted">never recorded</span>'}</dd>
      <dt>claims</dt><dd>${(pc.seams || []).map(c => `[${c.executable ? 'executable' : 'assertion'}] ${escape(c.claim)}`).join('<br>') || '<span class="muted">none</span>'}</dd>
      <dt>call sites</dt><dd>${(pc.call_sites || []).map(c => `L${c.line} → ${escape(c.callee_raw)} ${c.rationale ? '“' + escape(c.rationale) + '”' : '<span class="muted">(unannotated)</span>'}`).join('<br>') || '<span class="muted">none</span>'}</dd>
    </dl>`;
}

let ASK_ID = null, POLL = null, LAST_ANSWER = '';

async function ask() {
  if (!SELECTED) { setAnswer('select a frame first.'); return; }
  const q = document.getElementById('q').value.trim();
  if (!q) return;
  stopPolling();
  setKeepVisible(false);
  setAnswer(DATA.ask_route === 'tmux'
    ? 'sent to your agent session — waiting for the answer…'
    : 'thinking…');

  const r = await (await fetch('/api/ask', {
    method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ symbol: SELECTED, question: q }),
  })).json();

  const preamble = r.unanswered
    ? '\u26a0 nothing in the assembled context grounds this \u2014 that gap is itself the finding.\n\n'
    : '';

  if (r.status === 'pending' && r.ask_id) {
    ASK_ID = r.ask_id;
    setAnswer(preamble + 'question ' + r.ask_id + ' sent to ' + (r.target || 'the agent pane') +
      '.\nit is reading .plum/ask/' + r.ask_id + '.md \u2014 the answer appears here when it lands.');
    startPolling(preamble);
    return;
  }
  if (r.status === 'failed') {
    setAnswer(preamble + '\u2717 ' + (r.error || 'the question could not be delivered'));
    return;
  }
  LAST_ANSWER = r.answer || '';
  setAnswer(preamble + LAST_ANSWER);
  setKeepVisible(!!LAST_ANSWER);
}

function startPolling(preamble) {
  const started = Date.now();
  POLL = setInterval(async () => {
    if (!ASK_ID) return stopPolling();
    const r = await (await fetch('/api/ask/' + ASK_ID)).json();
    if (r.status === 'answered') {
      stopPolling();
      LAST_ANSWER = r.answer || '';
      setAnswer(preamble + LAST_ANSWER);
      setKeepVisible(true);
      return;
    }
    const secs = Math.round((Date.now() - started) / 1000);
    const dots = '.'.repeat(1 + (secs % 3));
    setAnswer(preamble + 'waiting for the agent' + dots + ' (' + secs + 's)\n' +
      'question ' + ASK_ID + ' \u2014 it may need you to accept a prompt in that pane.');
  }, 1500);
}

function stopPolling() { if (POLL) { clearInterval(POLL); POLL = null; } }

function setAnswer(text) { document.getElementById('answer').textContent = text; }

function setKeepVisible(on) {
  document.getElementById('keep').style.display = on ? 'block' : 'none';
}

// Keeping an answer is what separates this from chat: it becomes rationale that
// survives into every future report, a claim that goes stale when the code
// moves, or a patch proposing the comment where the next reader will look.
async function keep(kind) {
  if (!LAST_ANSWER || !SELECTED) return;
  const body = {
    ask_id: ASK_ID || '', symbol: SELECTED, answer: LAST_ANSWER,
    journal: kind === 'journal', claim: kind === 'claim', comment: kind === 'comment',
  };
  const r = await (await fetch('/api/keep', {
    method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  })).json();
  const notes = (r.notes || []).join('\n');
  document.getElementById('kept').textContent =
    '\u2713 ' + (notes || 'kept') + (r.patch_path ? '\n' + r.patch_path : '');
}

function send(e) { fetch('/api/telemetry', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(e) }); }
function fmtNs(ns) {
  if (ns < 1000) return ns + 'ns';
  if (ns < 1e6) return (ns / 1e3).toFixed(1) + 'µs';
  if (ns < 1e9) return (ns / 1e6).toFixed(1) + 'ms';
  return (ns / 1e9).toFixed(2) + 's';
}
function trunc(s, n) { return s && s.length > n ? s.slice(0, n - 1) + '…' : (s || ''); }
function escape(s) { return String(s).replace(/[&<>]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;' }[c])); }

boot();
