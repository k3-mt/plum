// A warehouse build drawn as what it is.
//
// The landscape next door draws code: a path that descends into a call and
// comes back, where closure is the shape you read it by. SQL has no returns.
// So this draws a DAG instead — layered left to right in build order, every
// table strictly to the right of everything it reads, and every arrow labelled
// with how the rows were matched. The two facts a call arrow cannot carry are
// the join type and the row count, and they are the two a warehouse reader
// asks for first.

function drawFlow() {
  const flow = DATA.flow;
  const svg = document.getElementById('svg');
  svg.innerHTML = '';
  document.getElementById('closed').textContent = '';

  const nodes = relayerOutsideTables(flow.nodes || [], flow.links || []);
  const links = flow.links || [];
  if (!nodes.length) {
    svg.setAttribute('height', 40);
    svg.appendChild(el('text', { x: 8, y: 24, class: 'blabel' }, 'Nothing in the DAG. Run: plum ingest'));
    return;
  }

  // Layout is measured, not assumed. Every box is as tall as it has facts to
  // state, so a fixed row height either crushes the tall ones together or
  // wastes a screen on the short ones. Columns are packed by real heights and
  // centred against each other, which also keeps the arrows short.
  const BOXW = 232, GAPX = 168, GAPY = 40, PADX = 28, PADY = 52;
  const COL = BOXW + GAPX;
  const byLayer = new Map();
  for (const n of nodes) {
    if (!byLayer.has(n.layer)) byLayer.set(n.layer, []);
    byLayer.get(n.layer).push(n);
  }
  const layers = [...byLayer.keys()].sort((a, b) => a - b);
  orderColumns(layers, byLayer, links);

  const stack = layers.map(L => {
    const col = byLayer.get(L);
    let h = 0;
    const heights = col.map(n => boxHeight(n));
    heights.forEach(v => { h += v + GAPY; });
    return { col, heights, total: h - GAPY };
  });
  const tallest = Math.max(...stack.map(s => s.total));

  const pos = new Map();
  stack.forEach((s, li) => {
    let y = PADY + (tallest - s.total) / 2;   // centre each column against the tallest
    s.col.forEach((n, i) => {
      pos.set(n.symbol, { x: PADX + li * COL, y, h: s.heights[i], node: n });
      y += s.heights[i] + GAPY;
    });
  });
  const width = PADX * 2 + (layers.length - 1) * COL + BOXW;
  const height = PADY * 2 + tallest;
  svg.setAttribute('width', width);
  svg.setAttribute('height', height);
  svg.setAttribute('viewBox', `0 0 ${width} ${height}`);

  // Arrows first, so the tables sit on top of them.
  // Several arrows can land on one table. Their labels are spread along the
  // span rather than all sitting at the midpoint, where they would overlap into
  // an unreadable pile.
  const arriving = new Map();
  for (const l of links) arriving.set(l.to, (arriving.get(l.to) || 0) + 1);
  const placed = new Map();
  const labels = [];

  for (const l of links) {
    const a = pos.get(l.from), b = pos.get(l.to);
    if (!a || !b) continue;
    const x1 = a.x + BOXW, y1 = a.y + a.h / 2;
    const x2 = b.x, y2 = b.y + b.h / 2;
    const k = placed.get(l.to) || 0;
    placed.set(l.to, k + 1);
    const spread = arriving.get(l.to) > 1 ? (k - (arriving.get(l.to) - 1) / 2) * 24 : 0;
    const mid = (x1 + x2) / 2 + spread;
    const d = `M${x1},${y1} C${mid},${y1} ${mid},${y2} ${x2},${y2}`;
    // A dependency dbt does not know about is drawn differently, because the
    // whole hazard is that it looks like part of the DAG and is not.
    const cls = !l.in_dag ? 'link-outside' : (l.relation === 'from' ? 'link-from' : 'link-join');
    svg.appendChild(el('path', { d, class: 'flowlink ' + cls, fill: 'none', 'marker-end': 'url(#arrow)' }));

    // Labels sit in the gutter immediately left of the table they arrive at,
    // not at the midpoint of the span. An arrow that crosses two layers has its
    // midpoint inside another column, and a label there lands on top of an
    // unrelated table.
    const label = linkLabel(l);
    if (label) {
      labels.push({
        x: x2 - 16, // clear of the arrowhead
        y: y2 + (k - (arriving.get(l.to) - 1) / 2) * 36 - 4,
        label, rows: l.rows,
      });
    }
  }
  // Every label goes on after every arrow. Drawing them per link meant a later
  // link's arrow painted over an earlier link's text — which read as a typo in
  // the join key rather than as an overlap.
  for (const t of labels) {
    const rows = t.rows ? fmtRows(t.rows) + ' rows' : '';
    // A plate behind the text, not a halo around the glyphs. An outline stroke
    // only covers the line where it runs under a letter; between the letters it
    // still shows, and a horizontal arrow reads as a strikethrough.
    const chars = Math.max(t.label.length, rows.length);
    const w = chars * CHARW + 8;
    svg.appendChild(el('rect', {
      x: t.x - w + 4, y: t.y - 10, width: w, height: rows ? 27 : 14, class: 'labelplate',
    }));
    svg.appendChild(el('text', { x: t.x, y: t.y, 'text-anchor': 'end', class: 'flabel' }, t.label));
    if (rows) {
      svg.appendChild(el('text', { x: t.x, y: t.y + 14, 'text-anchor': 'end', class: 'rowlabel' }, rows));
    }
  }
  svg.appendChild(arrowhead());

  for (const [symbol, p] of pos) {
    const n = p.node;
    const g = el('g', { class: 'flownode' });
    const failing = (n.tests || []).some(t => t.status === 'fail' || t.status === 'error');
    let cls = 'fn-model';
    if (n.kind === 'source') cls = 'fn-source';
    else if (n.kind === 'outside') cls = 'fn-outside';
    else if (n.changed) cls = 'fn-changed';
    if (failing) cls += ' fn-failing';

    g.appendChild(el('rect', { x: p.x, y: p.y, width: BOXW, height: p.h, rx: 4, class: 'fnbox ' + cls }));
    g.appendChild(el('text', { x: p.x + 12, y: p.y + 21, class: 'fntitle' }, truncName(n.name, 27)));

    let line = p.y + 39;
    const put = (text, klass) => {
      if (!text) return;
      g.appendChild(el('text', { x: p.x + 12, y: line, class: klass || 'blabel' }, trunc(text, 36)));
      line += LINE;
    };
    put(materialization(n), 'sp-svg-code');
    if (n.rows || n.bytes || n.nanos) {
      put([n.rows ? fmtRows(n.rows) + ' rows' : '', n.bytes ? fmtBytes(n.bytes) : '',
      n.nanos ? fmtNs(n.nanos) : ''].filter(Boolean).join(' · '), 'sp-svg-cost');
    }
    if (n.grain) put('one row per ' + n.grain, 'sp-svg-quote');
    if (n.unresolved) put('grain unreadable', 'sp-svg-risk');
    if (n.filter) put('where ' + n.filter, 'sp-svg-code');
    if ((n.aggregates || []).length) put(n.aggregates.join(', ') + ' — rows collapse', 'sp-svg-code');
    for (const t of n.tests || []) {
      const bad = t.status === 'fail' || t.status === 'error';
      put((bad ? '✗ ' : '✓ ') + t.name + (bad ? ' · ' + fmtRows(t.failures) + ' rows' : ''),
        bad ? 'sp-svg-risk' : 'sp-svg-ok');
    }
    if ((n.risks || []).length) put('! ' + n.risks.length + ' risk marker' + (n.risks.length > 1 ? 's' : ''), 'sp-svg-risk');

    g.appendChild(el('title', {}, flowTitle(n)));
    g.onclick = () => selectFlowNode(n);
    svg.appendChild(g);
  }
}

// A table written straight into the SQL has nothing upstream, so the
// longest-path layering puts it at 0, beside the sources, with its arrow
// crossing the whole picture and passing behind unrelated tables. It is not a
// source — it is an input to one model — so it is moved to sit beside that
// model's other inputs. The layer is presentation; the edge is unchanged.
function relayerOutsideTables(nodes, links) {
  const layerOf = new Map(nodes.map(n => [n.symbol, n.layer]));
  return nodes.map(n => {
    if (n.kind !== 'outside') return n;
    let deepest = 0;
    for (const l of links) {
      if (l.from === n.symbol) deepest = Math.max(deepest, layerOf.get(l.to) || 0);
    }
    return deepest > 1 ? { ...n, layer: deepest - 1 } : n;
  });
}

// orderColumns puts each table near the ones it connects to, so the arrows
// mostly do not cross. Sorting a column by name instead put the hardcoded
// refunds table at the top of its column while the model that reads it sat at
// the bottom of the next one, dragging a line across everything between.
//
// This is the barycentre sweep: order a column by the mean position of its
// neighbours in the column beside it, forwards then backwards, a few times.
// It is a heuristic and it does not promise zero crossings — but on a DAG this
// shape it removes the ones that make the picture hard to follow.
function orderColumns(layers, byLayer, links) {
  const index = new Map();
  const reindex = () => {
    for (const L of layers) byLayer.get(L).forEach((n, i) => index.set(n.symbol, i));
  };
  reindex();
  const neighbours = (n, back) => {
    const out = [];
    for (const l of links) {
      const other = back ? (l.to === n.symbol && l.from) : (l.from === n.symbol && l.to);
      if (other && index.has(other)) out.push(index.get(other));
    }
    return out;
  };
  for (let pass = 0; pass < 4; pass++) {
    const back = pass % 2 === 0;
    const order = back ? layers : [...layers].reverse();
    for (const L of order) {
      const col = byLayer.get(L);
      const key = new Map(col.map(n => {
        const ns = neighbours(n, back);
        // A table with nothing to anchor it keeps where it is, rather than
        // being sorted to one end for no reason.
        return [n.symbol, ns.length ? ns.reduce((a, b) => a + b, 0) / ns.length : index.get(n.symbol)];
      }));
      col.sort((a, b) => key.get(a.symbol) - key.get(b.symbol) || a.name.localeCompare(b.name));
      reindex();
    }
  }
}

// boxHeight sizes a table to what it has to say. Everything on a node is a fact
// from the run or the statement, so nothing is dropped to make the boxes tidy.
// LINE is the leading inside a table box, and boxHeight has to agree with it or
// the text runs out through the bottom edge.
const LINE = 14;

// The page is monospace throughout, so a string's width is its length. That is
// what makes a background plate sizeable without measuring text in the DOM.
const CHARW = 6.05;

function boxHeight(n) {
  let lines = 1; // materialization, always drawn
  if (n.rows || n.bytes || n.nanos) lines++;
  if (n.grain) lines++;
  if (n.unresolved) lines++;
  if (n.filter) lines++;
  if ((n.aggregates || []).length) lines++;
  lines += (n.tests || []).length;
  if ((n.risks || []).length) lines++;
  return 30 + lines * LINE + 10; // title band, the lines themselves, bottom padding
}

// truncName drops the front of a long qualified name, not the back. What
// identifies shop-prod-1234.shop_raw.refunds is "refunds"; cutting it to
// "shop-prod-1234.shop_raw.re…" throws away the only part you would recognise.
function truncName(s, n) {
  if (s.length <= n) return s;
  if (s.includes('.')) return '…' + s.slice(s.length - (n - 1));
  return trunc(s, n);
}

function materialization(n) {
  if (n.kind === 'source') return 'source';
  if (n.kind === 'outside') return 'outside the DAG';
  let m = n.materialized || 'model';
  if (n.unique_key) m += ' on ' + n.unique_key;
  if (n.status === 'not-run') m += ' · not rebuilt';
  return m;
}

// linkLabel is how the rows were matched. "left join on order_id" is the single
// most load-bearing string on the picture: it is the difference between rows
// being dropped, kept, or multiplied.
function linkLabel(l) {
  if (l.relation === 'from') return 'from';
  if (l.relation === 'ref') return 'declared, not in the statement';
  if (l.key) return l.relation + ' join on ' + l.key;
  if (l.on) return l.relation + ' join';
  return l.relation;
}

function flowTitle(n) {
  const out = [n.symbol];
  if (n.doc) out.push(n.doc);
  if (n.grain) out.push('grain: one row per ' + n.grain + ' (' + n.grain_from + ')');
  if (n.unresolved) out.push('grain unreadable — the SQL ' + n.unresolved);
  for (const r of n.risks || []) out.push('! ' + r);
  for (const t of n.tests || []) {
    out.push((t.status === 'fail' ? '✗ ' : '✓ ') + t.name +
      (t.failures ? ' — ' + fmtRows(t.failures) + ' rows failed' : ''));
  }
  return out.join('\n');
}

// Clicking a table copies its evidence, the same contract as clicking a frame:
// paste it into an agent and the question answers itself.
async function selectFlowNode(n) {
  const lines = [
    '## Model ' + n.name, '',
    materialization(n) + (n.rows ? ' · ' + fmtRows(n.rows) + ' rows' : '') +
    (n.bytes ? ' · ' + fmtBytes(n.bytes) + ' scanned' : '') + (n.nanos ? ' · ' + fmtNs(n.nanos) : ''),
  ];
  if (n.doc) lines.push('', n.doc);
  if (n.grain) lines.push('', 'Grain: one row per ' + n.grain + ' (' + n.grain_from + ')');
  if (n.unresolved) lines.push('', 'Grain could not be read: the SQL ' + n.unresolved);
  const ins = (DATA.flow.links || []).filter(l => l.to === n.symbol);
  if (ins.length) {
    lines.push('', '## Reads');
    for (const l of ins) {
      lines.push('- ' + l.from_name + ' — ' + linkLabel(l) + (l.note ? ': ' + l.note : '') +
        (l.in_dag ? '' : ' (written into the SQL, invisible to dbt)'));
    }
  }
  const outs = (DATA.flow.links || []).filter(l => l.from === n.symbol);
  if (outs.length) {
    lines.push('', '## Read by');
    for (const l of outs) lines.push('- ' + l.to_name + ' — ' + linkLabel(l));
  }
  if ((n.tests || []).length) {
    lines.push('', '## Tests');
    for (const t of n.tests) {
      lines.push('- ' + t.name + ' — ' + t.status + (t.failures ? ' (' + fmtRows(t.failures) + ' rows failed)' : ''));
    }
  }
  if ((n.risks || []).length) {
    lines.push('', '## Risk markers');
    for (const r of n.risks) lines.push('- ' + r);
  }
  await select(n.symbol, null, { copy: false });
  const pc = await (await fetch('/api/symbol/' + encodeURIComponent(n.symbol))).json();
  await copy(lines.join('\n') + '\n\n---\n\n' + (pc.markdown || ''),
    n.unresolved ? 'model copied — grain unreadable, paste it and ask' : 'model evidence copied');
}

function fmtRows(n) {
  if (!n) return '0';
  return String(n).replace(/\B(?=(\d{3})+(?!\d))/g, ',');
}

function fmtBytes(n) {
  if (!n) return '';
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let i = 0, v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(1)) + ' ' + units[i];
}

function arrowhead() {
  const defs = el('defs', {});
  const m = el('marker', {
    id: 'arrow', viewBox: '0 0 8 8', refX: 7, refY: 4,
    markerWidth: 7, markerHeight: 7, orient: 'auto-start-reverse',
  });
  m.appendChild(el('path', { d: 'M0,0 L8,4 L0,8 z', class: 'arrowhead' }));
  defs.appendChild(m);
  return defs;
}

// flowSummary is the header line: what the build cost and whether to trust it.
function flowSummary(flow) {
  const bits = [];
  if (flow.elapsed_ns) bits.push(fmtNs(flow.elapsed_ns) + ' elapsed');
  if (flow.bytes_scanned) bits.push(fmtBytes(flow.bytes_scanned) + ' scanned');
  if (flow.rows_written) bits.push(fmtRows(flow.rows_written) + ' rows written');
  let text = 'Build order, left to right: every table sits to the right of everything it reads. ' + bits.join(' · ') + '.';
  if (flow.failing) {
    text += ' ' + flow.failing + ' test' + (flow.failing > 1 ? 's are' : ' is') +
      ' failing, so at least one of these tables is not what it claims to be.';
  }
  return text;
}
