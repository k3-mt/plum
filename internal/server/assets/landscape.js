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
  draw();
  document.getElementById('done').onclick = async () => {
    await fetch('/api/done', { method: 'POST' });
    document.getElementById('done').textContent = 'quiz unlocked — run: plum quiz';
  };
  document.getElementById('asked').onclick = ask;
  document.getElementById('q').addEventListener('keydown', e => { if (e.key === 'Enter') ask(); });
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
      : (w.risk ? 'var(--risk)' : (w.phase === 'resume' ? 'var(--resume)' : 'var(--enter)'));
    const rect = el('rect', {
      x, y, width: W - 20, height: 26, rx: 3, fill,
      opacity: w.phase === 'resume' ? .45 : .9,
      stroke: w.doc ? 'none' : 'var(--enter)',
      'stroke-dasharray': w.doc ? '' : '3 2',
    });
    g.appendChild(rect);
    g.appendChild(el('text', { x: cx(i), y: y + 17, 'text-anchor': 'middle', class: 'wlabel', fill: '#0f1113' },
      trunc(w.label, 15)));
    g.appendChild(el('text', { x: cx(i), y: y + 38, 'text-anchor': 'middle', class: 'blabel' },
      'd' + w.depth + (w.phase === 'resume' ? ' · resumed' : w.phase === 'escape' ? ' · escaped' : '')));
    g.onclick = () => select(w.symbol, w);
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

async function ask() {
  if (!SELECTED) { document.getElementById('answer').textContent = 'select a frame first.'; return; }
  const q = document.getElementById('q').value.trim();
  if (!q) return;
  const out = document.getElementById('answer');
  out.textContent = 'thinking…';
  const r = await (await fetch('/api/ask', {
    method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ symbol: SELECTED, question: q }),
  })).json();
  out.textContent = (r.unanswered ? '⚠ nothing in the assembled context grounds this — that gap is itself the finding.\n\n' : '') + r.answer;
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
