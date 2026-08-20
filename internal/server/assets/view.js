// Pan and zoom for the canvas.
//
// Both pictures outgrow a screen: a DAG with thirty models is wider than any
// window, and a landscape with a hundred frames is longer. Scrolling a fixed
// canvas means the thing you are comparing against is always off-screen, so the
// canvas becomes a viewport onto a larger drawing instead.
//
// The view is the SVG's viewBox, not a transform on the contents. Stroke widths
// and font sizes then scale with the drawing, which is what you want here —
// zooming out is meant to show shape, not to render unreadable text at full
// size.

let VIEW = null;      // {x, y, w, h} in drawing coordinates
let CONTENT = null;   // the drawing's natural size
let DRAG = null;

const MIN_SCALE = 0.25, MAX_SCALE = 4;

// fitView is called by each renderer once it knows how big its drawing is. It
// keeps the current view across a live reload when the drawing has not changed
// size, so editing a model does not throw away where you were looking.
function fitView(svg, width, height) {
  const same = CONTENT && CONTENT.w === width && CONTENT.h === height;
  CONTENT = { w: width, h: height };
  svg.removeAttribute('width');
  svg.removeAttribute('height');
  svg.setAttribute('preserveAspectRatio', 'xMinYMin meet');
  if (!same || !VIEW) VIEW = { x: 0, y: 0, w: width, h: height };
  applyView(svg);
  attachPanZoom(svg);
}

function applyView(svg) {
  svg.setAttribute('viewBox', `${VIEW.x} ${VIEW.y} ${VIEW.w} ${VIEW.h}`);
}

function currentSvg() { return document.getElementById('svg'); }

// resetView is the way back when you are lost, which on a pannable canvas is a
// requirement rather than a convenience.
function resetView() {
  if (!CONTENT) return;
  VIEW = { x: 0, y: 0, w: CONTENT.w, h: CONTENT.h };
  applyView(currentSvg());
}

function attachPanZoom(svg) {
  if (svg.dataset.panzoom) return; // the renderer rebuilds children, not the node
  svg.dataset.panzoom = '1';

  // Drawing coordinates under a pointer, so zoom holds that point still and a
  // drag moves the drawing exactly as far as the pointer went.
  const at = (e) => {
    const r = svg.getBoundingClientRect();
    return {
      x: VIEW.x + (e.clientX - r.left) / r.width * VIEW.w,
      y: VIEW.y + (e.clientY - r.top) / r.height * VIEW.h,
    };
  };

  svg.addEventListener('mousedown', (e) => {
    if (e.button !== 0) return;
    DRAG = { from: at(e), view: { ...VIEW } };
    svg.classList.add('grabbing');
  });

  window.addEventListener('mousemove', (e) => {
    if (!DRAG) return;
    // Recomputed against the view the drag started from, so the pointer stays
    // on the same point of the drawing however far it travels.
    const r = svg.getBoundingClientRect();
    const now = {
      x: DRAG.view.x + (e.clientX - r.left) / r.width * DRAG.view.w,
      y: DRAG.view.y + (e.clientY - r.top) / r.height * DRAG.view.h,
    };
    VIEW.x = DRAG.view.x - (now.x - DRAG.from.x);
    VIEW.y = DRAG.view.y - (now.y - DRAG.from.y);
    applyView(svg);
  });

  window.addEventListener('mouseup', () => {
    if (!DRAG) return;
    DRAG = null;
    svg.classList.remove('grabbing');
  });

  svg.addEventListener('wheel', (e) => {
    e.preventDefault();
    const p = at(e);
    // A trackpad sends many small deltas and a mouse a few large ones; the
    // exponential keeps both feeling like the same gesture.
    const factor = Math.exp(e.deltaY * 0.0015);
    const scale = CONTENT.w / VIEW.w;
    const next = Math.min(MAX_SCALE, Math.max(MIN_SCALE, scale / factor));
    const w = CONTENT.w / next, h = CONTENT.h / next;
    // Hold the point under the cursor.
    VIEW.x = p.x - (p.x - VIEW.x) * (w / VIEW.w);
    VIEW.y = p.y - (p.y - VIEW.y) * (h / VIEW.h);
    VIEW.w = w;
    VIEW.h = h;
    applyView(svg);
  }, { passive: false });

  // A click that dragged is a pan, not a selection. Without this, letting go
  // over a node opens it, which makes the canvas feel like it fights you.
  svg.addEventListener('click', (e) => {
    if (svg.dataset.moved === '1') {
      e.stopPropagation();
      svg.dataset.moved = '0';
    }
  }, true);
  svg.addEventListener('mousemove', () => { if (DRAG) svg.dataset.moved = '1'; });
  svg.addEventListener('mousedown', () => { svg.dataset.moved = '0'; });
  svg.addEventListener('dblclick', resetView);
}
