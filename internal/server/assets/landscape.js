// The landscape is a reaction coordinate, not a flame graph: vertical is stack
// depth, descent is entering a call, ascent is returning, and the path must close.
const SVG = 'http://www.w3.org/2000/svg';
let DATA = null, SELECTED = null, DWELL = null;
// BEHIND is set when the page skipped a reload it could not usefully draw —
// collapsed to its meter, or hidden behind other windows. It is what makes that
// skipping safe: the page knows it owes itself a redraw, and pays it on the way
// back rather than sitting there showing something out of date.
let BEHIND = false;

const el = (name, attrs = {}, text) => {
  const n = document.createElementNS(SVG, name);
  for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v);
  if (text !== undefined) n.textContent = text;
  return n;
};

// OFFLINE is set by an exported file, which carries the whole session inline.
// The page then never reaches the network: same markup, same drawing, no server.
const OFFLINE = typeof window !== 'undefined' ? window.__PLUM__ : null;

// symbolBrief is the one place the page asks for a symbol, so it is the one
// place that has to know whether anything is running.
async function symbolBrief(symbol) {
  if (OFFLINE) return OFFLINE.briefs[symbol] || { symbol, markdown: '' };
  return (await fetch('/api/symbol/' + encodeURIComponent(symbol))).json();
}

async function boot() {
  DATA = OFFLINE ? OFFLINE.payload : await (await fetch('/api/landscape')).json();
  setGate();
  drawDebt();
  drawOwed();
  document.getElementById('summary').textContent = DATA.summary || '';
  drawReading();
  render();
  if (OFFLINE) offlineMode();
  document.getElementById('done').onclick = async () => {
    await fetch('/api/done', { method: 'POST' });
    document.getElementById('done').textContent = 'quiz unlocked — run: plum quiz';
  };
  document.getElementById('asked').onclick = ask;
  document.getElementById('fit').onclick = resetView;
  listen();
  document.getElementById('route').textContent =
    'answers via ' + (DATA.ask_route === 'tmux' ? 'your tmux agent session' : DATA.ask_route);
  for (const kind of ['journal', 'claim', 'comment']) {
    document.getElementById('keep-' + kind).onclick = () => keep(kind);
  }
  document.getElementById('q').addEventListener('keydown', e => { if (e.key === 'Enter') ask(); });
  applyMode();
  window.addEventListener('resize', applyMode);
  document.addEventListener('visibilitychange', () => { if (!document.hidden) catchUp(); });
}

// A window someone left open serves two reading distances, and which one it is
// serving is decided by how wide they have made it. Narrow is a glance from
// across the room; opened out is a tool. An explore tab is never collapsed —
// a small screen showing nothing but a number would just be broken.
function peripheral() { return document.body.classList.contains('peripheral'); }

function applyMode() {
  const want = !!(DATA && DATA.resident) && window.innerWidth <= 560;
  if (want === peripheral()) return;
  document.body.classList.toggle('peripheral', want);
  if (!want) catchUp(); // opened out again: draw whatever was skipped meanwhile
}

// catchUp pays back the reloads the page chose not to draw.
function catchUp() {
  if (!BEHIND || peripheral() || document.hidden) return;
  BEHIND = false;
  refreshAll('caught up — the session moved while this window was away');
}

// The reading is the only part of this page a model wrote. It sits at the top,
// folds away, and is labelled — because prose that reads like a record but was
// inferred is worse than no prose at all.
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
  // Headings and emphasis only; the text is otherwise shown as written.
  body.innerHTML = escape(r.markdown)
    .replace(/^#+\s*(.+)$/gm, '<b>$1</b>')
    .replace(/\*\*(.+?)\*\*/g, '<b>$1</b>')
    .replace(/`([^`]+)`/g, '<code>$1</code>');
  box.appendChild(body);
}

// listen keeps the page in step with the files it is a view of. Work in one
// pane, watch it here in the other: a trace rewrites the landscape, an
// interpretation appears, an edit to the source turns a reading stale.
// offlineMode removes the parts of the page that only a running plum can do:
// asking an agent a question, recording that you met the code, watching the
// tree. Leaving buttons that quietly fail would be worse than not having them.
function offlineMode() {
  for (const id of ['debt', 'owed']) {
    const node = document.getElementById(id);
    if (node) node.remove();
  }
  for (const id of ['done', 'ask']) {
    const node = document.getElementById(id);
    if (node) node.remove();
  }
  const foot = document.createElement('div');
  foot.className = 'exported';
  const s = DATA.session || {};
  foot.textContent = 'Exported from plum · session ' + (s.id || '') +
    (s.end_sha ? ' · ' + s.end_sha.slice(0, 12) : '') +
    ' · a snapshot, not a live view';
  document.body.appendChild(foot);
}

function listen() {
  if (OFFLINE) return;
  if (!window.EventSource) return;
  const src = new EventSource('/api/live');
  src.addEventListener('reload', async (e) => {
    // Nothing on screen: draw nothing, and remember that you owe a redraw.
    if (document.hidden) { BEHIND = true; return; }
    // Collapsed to the meter: the landscape is not being shown, so there is no
    // reason to fetch one. That payload carries every changed symbol in the
    // session and runs to megabytes on a real capture, which is exactly the
    // cost a window left open all day must not keep paying. The number is a few
    // hundred bytes and is the only thing visible.
    if (peripheral()) { BEHIND = true; refreshDebt(); return; }
    refreshAll(e.data === 'source'
      ? 'source changed on disk — reading and staleness refreshed'
      : 'session updated — landscape reloaded');
  });
}

async function refreshAll(note) {
  const before = SELECTED;
  const scroll = document.getElementById('svgwrap').scrollLeft;
  DATA = await (await fetch('/api/landscape')).json();
  drawReading();
  document.getElementById('summary').textContent = DATA.summary || '';
  setGate();
  drawDebt();
  drawOwed();
  render();
  document.getElementById('svgwrap').scrollLeft = scroll;
  // Keep whatever the reader was looking at, if it still exists.
  if (before && (DATA.landscape.wells || []).some((w) => w.symbol === before)) {
    select(before, null, { copy: false });
  }
  if (note) toast(note);
}

// The debt meter: how much of what this session changed you have not met, at the
// version it is in now. It goes up on its own while an agent works and comes
// down only when you read something — which is the entire point of leaving this
// window open where you can see it.
function drawDebt() {
  const box = document.getElementById('debt');
  if (!box) return;
  const d = DATA.debt;
  // An export has no reader, so it has no debt. Showing zero would be a
  // different claim, and a false one: "you have met all of this".
  if (OFFLINE || !d || !d.total) { box.hidden = true; return; }
  box.hidden = false;
  const met = d.total - d.unmet;
  box.querySelector('.count').textContent = d.unmet;
  box.querySelector('.meter i').style.width = Math.round(100 * met / d.total) + '%';
  // Nothing outstanding only counts as clear if nothing is being written either.
  box.classList.toggle('clear', d.unmet === 0 && !d.drifted);
  box.classList.toggle('writing', !!d.drifted);

  // Which way it is going is most of what a glance is for: fourteen unmet reads
  // very differently depending on whether it was four ten minutes ago or forty.
  const arrow = box.querySelector('.trend');
  arrow.hidden = !d.trend;
  if (d.trend) {
    arrow.textContent = (d.trend > 0 ? '\u25b2' : '\u25bc') + Math.abs(d.trend);
    arrow.classList.toggle('up', d.trend > 0);
    arrow.classList.toggle('down', d.trend < 0);
    arrow.title = (d.trend > 0 ? 'up ' : 'down ') + Math.abs(d.trend)
      + ' in the last ' + d.trend_minutes + ' minutes';
  }

  const parts = [d.unmet === 0 ? 'met, all ' + d.total : 'unmet of ' + d.total];
  if (d.stale) parts.push(d.stale + ' changed since you read it');
  if (d.drifted) parts.push(d.drifted + ' being written now');
  else if (d.unmeasured) parts.push('drift not measured');
  box.querySelector('.what').textContent = parts.join(' \u00b7 ');

  const title = [d.unmet === 0
    ? 'You have seen every symbol this session changed, at the version it was captured in.'
    : d.unmet + ' of ' + d.total + ' changed symbols you have not seen at this version. '
      + 'Open one to pay it down, or click "I have met this code" to clear the lot.'];
  if (d.drifted) {
    title.push(d.drifted + ' of them no longer match the working tree: code written '
      + 'since the capture, which no session has recorded yet.');
  }
  if (d.unmeasured) title.push('Drift not measured — ' + d.unmeasured + '.');
  box.title = title.join('\n\n');
}

// The worklist is the meter made actionable. A number you can read but not act
// on is a scold; this is the same debt as a list you can work through, in the
// order `plum report` reads in — what could break other people first.
function drawOwed() {
  const box = document.getElementById('owed');
  if (!box) return;
  const items = (DATA.debt && DATA.debt.worklist) || [];
  if (OFFLINE || !items.length) { box.hidden = true; return; }
  box.hidden = false;

  const list = document.getElementById('owed-list');
  list.innerHTML = '';
  for (const it of items) {
    const li = document.createElement('li');
    const b = document.createElement('button');
    b.textContent = it.name || it.symbol;
    b.onclick = () => select(it.symbol, null, { copy: false });
    li.appendChild(b);
    const where = document.createElement('span');
    where.className = 'where';
    where.textContent = ' \u00b7 ' + it.file + ' ';
    li.appendChild(where);
    const why = document.createElement('span');
    why.className = 'why';
    why.textContent = it.why;
    li.appendChild(why);
    list.appendChild(li);
  }
  const more = document.getElementById('owed-more');
  more.textContent = DATA.debt.more
    ? '\u2026 and ' + DATA.debt.more + ' more, held back so this stays a list you finish'
    : '';
}

// Reading a symbol pays the debt down, so the number has to move when you read
// one. Asking for the meter alone rather than reloading the landscape keeps that
// cheap: the landscape payload carries every changed symbol in the session.
async function refreshDebt() {
  if (OFFLINE) return;
  try {
    DATA.debt = await (await fetch('/api/debt')).json();
    drawDebt();
    drawOwed();
    if (!peripheral()) render(); // the hollow frames only exist when drawn
  } catch (e) { /* the meter is not worth breaking the page over */ }
}

function setGate() {
  document.getElementById('gate').textContent = DATA.gate.fired
    ? 'GATE FIRED — ' + DATA.gate.reasons.join(' · ')
    : 'gate clear';
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
  box.innerHTML = '';
  if (!step) {
    box.textContent = 'Hover a frame or a step to read what happened there. Click to copy its evidence.';
    return;
  }
  box.appendChild(renderSpans(step));
  if (step.note) {
    const warn = document.createElement('span');
    warn.className = 'warn';
    warn.textContent = '\u26a0 ' + step.note;
    box.appendChild(warn);
  }
}

// renderSpans colours a sentence by what each part is. The server decided the
// kinds; nothing here guesses from the text.
function renderSpans(step) {
  const frag = document.createDocumentFragment();
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

// render picks the picture that fits what was recorded. A dbt build is a DAG
// and a code trace is a path; drawing one as the other is not a styling choice,
// it misstates how the thing ran.
function render() {
  const isFlow = !!(DATA.flow && (DATA.flow.nodes || []).length);
  document.getElementById('legend-flow').hidden = !isFlow;
  document.getElementById('legend-path').hidden = isFlow;
  document.getElementById('hover').textContent = isFlow
    ? 'Hover a table for its grain, tests and risks. Click to copy its evidence.'
    : "Hover to read what happened. Click to copy that frame's evidence.";
  if (isFlow) {
    document.getElementById('summary').textContent = flowSummary(DATA.flow);
    drawFlow();
    drawFindings();
    return;
  }
  draw();
}

// Findings are stated under the picture rather than drawn on it: they are
// sentences about the whole DAG, not properties of one table.
function drawFindings() {
  const box = document.getElementById('closed');
  const list = DATA.flow.findings || [];
  box.textContent = list.length ? list.map(f => '· ' + f).join('\n') : '';
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
    fitView(svg, 400, 40);
    svg.appendChild(el('text', { x: 8, y: 24, class: 'blabel' },
      'No trace recorded yet. Run: plum trace'));
    return;
  }

  const W = 132, ROW = 74, PAD = 40;
  const maxDepth = Math.max(...wells.map(w => w.depth));
  const width = PAD * 2 + wells.length * W;
  const height = PAD * 2 + (maxDepth + 1) * ROW + 30;
  fitView(svg, width, height);

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
    hit.onclick = () => copyCallSite(b);
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
    // A frame you have not met is drawn hollow: the shape is there, the substance
    // is not, which is exactly the reader's position on it.
    const unmet = (DATA.debt && DATA.debt.frames || []).includes(w.symbol);
    const rect = el('rect', {
      x, y, width: W - 20, height: 26, rx: 3,
      fill: unmet ? 'none' : fill,
      stroke: unmet ? fill : (w.doc ? 'none' : 'var(--enter)'),
      'stroke-width': unmet ? 1.5 : 1,
      opacity: w.context ? .3 : (w.phase === 'resume' ? .45 : .9),
      'stroke-dasharray': unmet || w.doc ? '' : '3 2',
    });
    g.appendChild(rect);
    g.appendChild(el('text', { x: cx(i), y: y + 17, 'text-anchor': 'middle', class: 'wlabel',
      fill: unmet ? 'var(--fg)' : '#0f1113' }, trunc(w.label, 15)));
    if (w.context) g.appendChild(el('title', {}, w.symbol + ' — surrounding code, recorded for structure only'));
    g.appendChild(el('text', { x: cx(i), y: y + 38, 'text-anchor': 'middle', class: 'blabel' },
      'd' + w.depth + (w.phase === 'resume' ? ' · resumed' : w.phase === 'escape' ? ' · escaped' : '')));
    g.onclick = () => select(w.symbol, w, { copy: true });
    const wStep = stepFor('frame', i);
    g.onmouseenter = () => showStep(wStep);
    svg.appendChild(g);
    drawJoins(svg, w, x, y, W - 20);
  });
}

// drawJoins hangs the edges the path walks past off the shoulder of a frame.
//
// They are deliberately not drawn as lines to other frames. The landscape reads
// because it is a path — descend, ascend, close — and drawing the real graph
// turns it into the hairball every lineage tool already gives you. So a join is
// a stub: it shows that something else touches this frame, from which side, and
// what it cost, and clicking it opens that symbol.
function drawJoins(svg, w, x, y, width) {
  const joins = w.joins || [];
  if (!joins.length && !w.joins_more) return;
  const sides = { in: joins.filter(j => j.dir === 'in'), out: joins.filter(j => j.dir !== 'in') };
  for (const [dir, list] of Object.entries(sides)) {
    if (!list.length) continue;
    const edge = dir === 'in' ? x : x + width;      // enters the left, leaves the right
    const sign = dir === 'in' ? -1 : 1;
    list.slice(0, 3).forEach((j, k) => {
      const gap = 5 + k * 5;
      const g = el('g', { class: 'join' });
      // A short hook from above the shoulder into the side of the frame.
      g.appendChild(el('path', {
        class: 'jstub',
        d: `M${edge + sign * (7 + gap)},${y - 9 - k * 4} L${edge + sign * gap},${y + 6} L${edge},${y + 13}`,
      }));
      g.appendChild(el('path', { d: `M${edge + sign * (7 + gap)},${y - 9 - k * 4} L${edge},${y + 13}`, stroke: 'transparent', 'stroke-width': 12, fill: 'none' }));
      const cost = j.cost_ns ? ' · ' + fmtNs(j.cost_ns) : '';
      const where = j.on_path ? ' · also drawn on this path' : '';
      g.appendChild(el('title', {}, (dir === 'in' ? 'also flows in: ' : 'also flows out: ') + j.symbol + cost + where));
      g.onclick = (e) => { e.stopPropagation(); select(j.symbol, null, { copy: true }); };
      svg.appendChild(g);
    });
    // Only three stubs are drawn per side; the count is the truth about how
    // many there are, and a trailing + means the frame has more joins than the
    // landscape kept at all (Well.JoinsMore).
    svg.appendChild(el('text', {
      class: 'jcount', x: edge + sign * 10, y: y - 14,
      'text-anchor': dir === 'in' ? 'end' : 'start',
    }, (dir === 'in' ? '↘' : '↗') + list.length + (w.joins_more ? '+' : '')));
  }
}

// copyCallSite hands over one transition: who called whom, what it cost, and
// whether anything explains it. An unannotated expensive call is the case this
// exists for — copy it, paste it, get the comment written.
async function copyCallSite(bar) {
  const from = DATA.landscape.wells[bar.from];
  const to = DATA.landscape.wells[bar.to];
  const pc = await symbolBrief(from.symbol);

  const head = [
    '# Call site: ' + from.symbol + ' → ' + to.symbol,
    '',
    'Recorded cost: ' + fmtNs(bar.cost_ns) + ' (' + bar.kind + ', ' + bar.direction + ').',
    bar.rationale
      ? 'The call site says why: "' + bar.rationale + '"'
      : 'Nothing at this call site explains why the call is made. That is what is missing.',
    '',
    '---',
    '',
  ].join('\n');

  await copy(head + (pc.markdown || ''), bar.rationale
    ? 'call site copied'
    : 'unexplained call copied — paste it and ask for the comment');
  select(from.symbol, from, { copy: false });
}

// copy puts text on the clipboard, falling back for browsers that refuse the
// async API outside a user gesture.
async function copy(text, message) {
  try {
    await navigator.clipboard.writeText(text);
  } catch {
    const el = document.createElement('textarea');
    el.value = text;
    el.style.position = 'fixed';
    el.style.opacity = '0';
    document.body.appendChild(el);
    el.select();
    try { document.execCommand('copy'); } catch { /* nothing else to try */ }
    document.body.removeChild(el);
  }
  toast(message + ' · ' + text.length.toLocaleString() + ' chars');
}

function toast(text) {
  const el = document.getElementById('toast');
  el.textContent = text;
  el.classList.add('show');
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => el.classList.remove('show'), 2600);
}

async function select(symbol, well, opts) {
  if (SELECTED && DWELL) {
    send({ symbol: SELECTED, action: 'click', dwell_ms: Date.now() - DWELL });
  }
  SELECTED = symbol; DWELL = Date.now();
  const pc = await symbolBrief(symbol);
  refreshDebt(); // the fetch above is what marked it met; show that happening
  const srcEl = document.getElementById('src');
  srcEl.innerHTML = '';
  if (pc.source) {
    srcEl.appendChild(highlight(pc.source, languageOf(symbol.split('::')[0])));
  } else {
    srcEl.textContent = '(source not available in the working tree)';
  }
  send({ symbol, action: 'expand_source' });

  // Clicking a frame hands its whole assembled brief to the clipboard: source,
  // recorded arguments and returns, neighbours with their code, risks,
  // rationale, claims. Paste it into an agent and the question answers itself.
  if (!opts || opts.copy !== false) {
    const undocumented = !pc.doc;
    await copy(pc.markdown || '', undocumented
      ? 'evidence copied — undocumented, paste it and ask for the doc'
      : 'evidence copied');
  }

  // What touches this frame that the drawn path walks past. Named here in full,
  // because the canvas only has room for three stubs a side.
  const joinRow = (well && (well.joins || []).length)
    ? '<dt>joins</dt><dd>' + well.joins.map(j =>
      '<div class="inv"><span class="sp-cost">' + (j.dir === 'in' ? 'also flows in' : 'also flows out') + '</span> ' +
      '<span class="sp-code">' + escape(j.label || j.symbol) + '</span>' +
      (j.cost_ns ? ' <span class="sp-cost">' + fmtNs(j.cost_ns) + '</span>' : '') +
      (j.on_path ? ' <span class="muted">also drawn on this path</span>' : '') +
      '</div>').join('') +
    (well.joins_more ? '<div class="muted">and ' + well.joins_more + ' more not listed</div>' : '') +
    '</dd>'
    : '';

  const body = document.getElementById('rail-body');
  // Recorded values are data, and are coloured as data wherever they appear.
  const val = (v) => '<span class="sp-value">' + escape(v) + '</span>';
  const invs = (pc.invocations || []).map(e => {
    const test = e.test_id ? ' <span class="muted">during ' + escape(e.test_id) + '</span>' : '';
    if (e.event === 'call') {
      const args = Object.entries(e.args || {}).map(([k, v]) =>
        '<span class="sp-code">' + escape(k) + '</span> = ' + val(v)).join(', ');
      return '<div class="inv">called with ' + (args || '<span class="muted">no arguments</span>') + test + '</div>';
    }
    if (e.event === 'return') return '<div class="inv">returned ' + val(e.result || 'nothing') + test + '</div>';
    return '<div class="inv raise">raised ' + val(e.exception || '') + test + '</div>';
  }).join('') || '<span class="muted">never executed by the traced run</span>';

  body.innerHTML = `
    <dl class="kv">
      <dt>symbol</dt><dd><span class="sp-code">${escape(symbol)}</span></dd>
      <dt>signature</dt><dd><code class="sp-code">${escape(pc.signature || '—')}</code></dd>
      <dt>doc</dt><dd>${pc.doc ? '<span class="sp-quote">' + escape(pc.doc) + '</span>' : '<span class="warn">no declaration doc</span>'}</dd>
      <dt>recorded invocations</dt><dd>${invs}</dd>
      ${joinRow}
      <dt>risks</dt><dd>${(pc.risks || []).map(r => `<div class="warn">${escape(r.kind)} — ${escape(r.note)}</div>`).join('') || '<span class="muted">none</span>'}</dd>
      <dt>rationale</dt><dd>${(pc.rationale || []).map(j => '<span class="sp-quote">' + escape(j.rationale) + '</span>').join('<br>') || '<span class="muted">never recorded</span>'}</dd>
      <dt>claims</dt><dd>${(pc.seams || []).map(c => `[${c.executable ? 'executable' : 'assertion'}] ${escape(c.claim)}`).join('<br>') || '<span class="muted">none</span>'}</dd>
      <dt>call sites</dt><dd>${(pc.call_sites || []).map(c => `L${c.line} → <span class="sp-code">${escape(c.callee_raw)}</span> ${c.rationale ? '<span class="sp-quote">“' + escape(c.rationale) + '”</span>' : '<span class="warn">(unannotated)</span>'}`).join('<br>') || '<span class="muted">none</span>'}</dd>
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

function send(e) { if (OFFLINE) return; fetch('/api/telemetry', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(e) }); }
function fmtNs(ns) {
  if (ns < 1000) return ns + 'ns';
  if (ns < 1e6) return (ns / 1e3).toFixed(1) + 'µs';
  if (ns < 1e9) return (ns / 1e6).toFixed(1) + 'ms';
  return (ns / 1e9).toFixed(2) + 's';
}
function trunc(s, n) { return s && s.length > n ? s.slice(0, n - 1) + '…' : (s || ''); }
function escape(s) { return String(s).replace(/[&<>]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;' }[c])); }

boot();
