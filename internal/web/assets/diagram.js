// agtrack spec viewer — diagram window. Classic script, loaded before app.js.
//
// A click on a rendered Mermaid diagram opens it in a non-modal floating window: draggable,
// resizable, minimisable to a bottom-left dock, maximisable, dockable to the left or right
// edge as a split view, with pan/zoom, a source view and PNG export
// (docs/web-browse-spec.md, "Diagram window").
//
// Docking is generic: the manager publishes the docked widths as the CSS variables
// --dock-left / --dock-right on <html>, toggles html.has-docked, and fires an
// "agtrack:dock" event on window; the host page makes room for the docked windows.
//
// Entry point: window.AgtrackDiagram.update({root, specSlug, h, toast}). app.js calls it
// after every Mermaid render pass; it decorates the diagrams under `root`, wires the click
// handler once, and reconciles every open window with the freshly rendered document.
//
// Security: the window shows a clone of Mermaid's own SVG (never re-parsed from a string)
// and the diagram source via textContent. No innerHTML anywhere in this file.

(function () {
  "use strict";

  const MIN_W = 320;
  const MIN_H = 240;
  const EDGE = 8; // maximised margin
  const CASCADE = 24;
  const FIT_PAD = 16;
  const ZOOM_MIN = 0.1;
  const ZOOM_MAX = 8;
  const ZOOM_STEP = 1.25;
  const PAN_STEP = 48;
  const Z_BASE = 60; // below dialogs (80) and the toast (90)
  const Z_SPAN = 19;
  const SVG_NS = "http://www.w3.org/2000/svg";
  const DOCK_MIN_VW = 900; // docking is offered only this wide and up
  const PHONE_VW = 768; // below this, floating windows open maximised
  const DOCK_DEFAULT = 40; // % of the viewport width a docked window takes
  const DOCK_MIN = 25;
  const DOCK_MAX = 75;
  const DOCK_DOC_MIN = 20; // with both sides docked, the document keeps at least this
  const DOCK_KEY_STEP = 2;
  const Z_DOCKED = 35; // under the drawers' scrim (40) and drawers (50)

  const ICONS = {
    expand: "M4 14v6h6M20 10V4h-6M4 20l6-6M20 4l-6 6",
    min: "M5 19h14",
    max: "M5 5h14v14H5z",
    restore: "M8 8V4h12v12h-4M4 8h12v12H4z",
    close: "M6 6l12 12M18 6L6 18",
    dockLeft: "M4 4h16v16H4zM10 4v16",
    dockRight: "M4 4h16v16H4zM14 4v16",
    undock: "M8 8h12v12H8zM4 16V4h12",
  };

  /** @type {{root: Element, specSlug: string, h: Function, toast: Function} | null} */
  let ctx = null;
  const wired = new WeakSet();
  /** key → window record */
  const windows = new Map();
  /** windows from back to front */
  const stack = [];
  let dock = null;
  let lastRect = null;
  /** side → docked window record (or null) */
  const docked = { left: null, right: null };
  /** side → width of a docked window, in % of the viewport width */
  const dockPct = { left: DOCK_DEFAULT, right: DOCK_DEFAULT };
  /** side → splitter element */
  const splitters = { left: null, right: null };
  let seq = 0;

  const clamp = (v, lo, hi) => Math.min(Math.max(v, lo), hi);

  function icon(name) {
    const svg = document.createElementNS(SVG_NS, "svg");
    svg.setAttribute("viewBox", "0 0 24 24");
    svg.setAttribute("aria-hidden", "true");
    const path = document.createElementNS(SVG_NS, "path");
    path.setAttribute("d", ICONS[name]);
    svg.appendChild(path);
    return svg;
  }

  function h(...args) {
    return ctx.h(...args);
  }

  // ------------------------------------------------------------------------------------
  // Finding diagrams in the document
  // ------------------------------------------------------------------------------------

  function keyOf(slug, idx) {
    return slug + "#" + idx;
  }

  /** Every successfully rendered diagram under root, keyed by (section slug, index). */
  function collect(root) {
    const out = new Map();
    for (const sec of root.querySelectorAll("section[data-section]")) {
      const slug = sec.dataset.section;
      const titleEl = sec.querySelector(".section-title");
      const heading = titleEl ? titleEl.textContent.trim() : "";
      let idx = 0;
      for (const host of sec.querySelectorAll(".diagram")) {
        idx++;
        const svg = host.querySelector("svg");
        if (!svg || host.classList.contains("diagram-error")) continue;
        const pre = host.previousElementSibling;
        const code = pre && pre.matches("pre.mermaid-src") ? pre.querySelector("code") || pre : null;
        out.set(keyOf(slug, idx), {
          key: keyOf(slug, idx),
          slug,
          idx,
          host,
          svg,
          source: code ? code.textContent : "",
          title: "Diagram " + idx + (heading ? " · " + heading : ""),
        });
      }
    }
    return out;
  }

  function hostInfo(host) {
    const sec = host.closest("section[data-section]");
    if (!sec) return null;
    const idx = Array.from(sec.querySelectorAll(".diagram")).indexOf(host) + 1;
    return collect(ctx.root).get(keyOf(sec.dataset.section, idx)) || null;
  }

  function decorate(root) {
    for (const info of collect(root).values()) {
      const host = info.host;
      host.classList.add("is-expandable");
      if (host.querySelector(":scope > .diagram-expand")) continue;
      host.appendChild(
        h("button", {
          type: "button",
          class: "diagram-expand",
          "aria-label": "Expand diagram",
          title: "Expand diagram",
        }, icon("expand"))
      );
    }
  }

  function wire(root) {
    if (wired.has(root)) return;
    wired.add(root);
    root.addEventListener("click", (e) => {
      const host = e.target.closest(".diagram.is-expandable");
      if (!host || !root.contains(host)) return;
      const viaButton = !!e.target.closest(".diagram-expand");
      if (!viaButton) {
        // Selecting labels to comment on them must not open the window.
        const sel = window.getSelection();
        if (sel && !sel.isCollapsed && sel.anchorNode && host.contains(sel.anchorNode)) return;
      }
      const info = hostInfo(host);
      if (info) openWindow(info);
    });
  }

  // ------------------------------------------------------------------------------------
  // Intrinsic size and cloning
  // ------------------------------------------------------------------------------------

  /** A detached clone of Mermaid's SVG at its intrinsic size, plus that size. */
  function prepareSvg(svg) {
    const clone = svg.cloneNode(true);
    let w = 0;
    let hgt = 0;
    const vb = (svg.getAttribute("viewBox") || "").trim().split(/[\s,]+/).map(Number);
    if (vb.length === 4 && vb[2] > 0 && vb[3] > 0) {
      w = vb[2];
      hgt = vb[3];
    } else {
      const r = svg.getBoundingClientRect();
      w = r.width || 300;
      hgt = r.height || 150;
    }
    clone.setAttribute("width", String(w));
    clone.setAttribute("height", String(hgt));
    clone.style.maxWidth = "none";
    clone.style.height = hgt + "px";
    return { node: clone, w, h: hgt };
  }

  // ------------------------------------------------------------------------------------
  // Window geometry, stacking, dock
  // ------------------------------------------------------------------------------------

  function viewport() {
    return { w: document.documentElement.clientWidth, h: document.documentElement.clientHeight };
  }

  /** Where docked windows start: the host's --dock-top, else its --topbar-h, else 0. */
  function dockTop() {
    const cs = getComputedStyle(document.documentElement);
    return parseFloat(cs.getPropertyValue("--dock-top")) || parseFloat(cs.getPropertyValue("--topbar-h")) || 0;
  }

  function dockWidth(side) {
    return Math.round((viewport().w * dockPct[side]) / 100);
  }

  function currentRect(w) {
    const v = viewport();
    if (w.max) {
      return { x: EDGE, y: EDGE, w: Math.max(MIN_W, v.w - 2 * EDGE), h: Math.max(MIN_H, v.h - 2 * EDGE) };
    }
    if (w.dock) {
      const top = dockTop();
      const width = dockWidth(w.dock);
      return { x: w.dock === "left" ? 0 : v.w - width, y: top, w: width, h: Math.max(0, v.h - top) };
    }
    return w.rect;
  }

  function clampRect(r) {
    const v = viewport();
    const width = clamp(r.w, MIN_W, Math.max(MIN_W, v.w));
    const height = clamp(r.h, MIN_H, Math.max(MIN_H, v.h));
    // The title bar always stays inside the viewport.
    const x = clamp(r.x, 0, Math.max(0, v.w - width));
    const y = clamp(r.y, 0, Math.max(0, v.h - 40));
    return { x, y, w: width, h: height };
  }

  function applyRect(w) {
    const r = currentRect(w);
    const s = w.el.style;
    s.left = r.x + "px";
    s.top = r.y + "px";
    s.width = r.w + "px";
    s.height = r.h + "px";
    w.el.classList.toggle("is-max", w.max);
    w.el.classList.toggle("is-docked", !!w.dock);
    w.el.classList.toggle("is-docked-left", w.dock === "left");
    w.el.classList.toggle("is-docked-right", w.dock === "right");
    // A docked window keeps Undock, Maximise and Close; a floating one offers docking.
    w.btnDockLeft.hidden = !!w.dock;
    w.btnDockRight.hidden = !!w.dock;
    w.btnMin.hidden = !!w.dock;
    w.btnUndock.hidden = !w.dock;
    w.btnMax.setAttribute("aria-label", w.max ? "Restore" : "Maximise");
    w.btnMax.title = w.max ? "Restore" : "Maximise";
    w.btnMax.replaceChildren(icon(w.max ? "restore" : "max"));
    if (w.fitted && !w.minimised && !w.sourceView) fit(w);
  }

  function restack() {
    stack.forEach((w, i) => {
      // A docked window is part of the page layout: it sits under drawers and floating windows.
      w.el.style.zIndex = String(w.dock && !w.max ? Z_DOCKED : Z_BASE + Math.min(i, Z_SPAN));
    });
    if (dock) dock.style.zIndex = String(Z_BASE + Z_SPAN);
  }

  function focusWindow(w, takeFocus) {
    const i = stack.indexOf(w);
    if (i < 0 || i !== stack.length - 1) {
      if (i >= 0) stack.splice(i, 1);
      stack.push(w);
      restack();
    }
    for (const o of stack) o.el.classList.toggle("is-front", o === w);
    if (takeFocus) w.el.focus({ preventScroll: true });
  }

  function ensureDock() {
    if (dock) return dock;
    dock = h("div", { class: "dgw-dock", role: "toolbar", "aria-label": "Minimised diagrams" });
    document.body.appendChild(dock);
    return dock;
  }

  function minimise(w) {
    if (w.minimised) return;
    w.minimised = true;
    w.el.hidden = true;
    w.chip = h("button", {
      type: "button",
      class: "dgw-chip",
      title: "Restore " + w.title,
      on: { click: () => restoreFromDock(w) },
    }, icon("expand"), h("span", { class: "dgw-chip-text", text: w.title }));
    ensureDock().appendChild(w.chip);
  }

  function restoreFromDock(w) {
    if (!w.minimised) return;
    w.minimised = false;
    w.el.hidden = false;
    if (w.chip) w.chip.remove();
    w.chip = null;
    applyRect(w);
    focusWindow(w, true);
  }

  function toggleMax(w) {
    w.max = !w.max;
    applyRect(w);
    restack();
  }

  // ------------------------------------------------------------------------------------
  // Docking: split view
  // ------------------------------------------------------------------------------------

  function canDock() {
    return viewport().w >= DOCK_MIN_VW;
  }

  /** Clamp a side's width to 25–75 %, leaving the document its share when both are docked. */
  function clampPct(side, pct) {
    const other = side === "left" ? "right" : "left";
    const hi = docked[other] ? Math.min(DOCK_MAX, 100 - DOCK_DOC_MIN - dockPct[other]) : DOCK_MAX;
    return clamp(Math.round(pct * 10) / 10, DOCK_MIN, Math.max(DOCK_MIN, hi));
  }

  function dockWindow(w, side) {
    if (!canDock() || w.dock === side) return;
    if (w.minimised) restoreFromDock(w);
    w.max = false;
    if (w.dock) docked[w.dock] = null;
    const prev = docked[side];
    if (prev && prev !== w) {
      // One window per side: the one that was there goes back to floating.
      prev.dock = null;
      applyRect(prev);
    }
    docked[side] = w;
    w.dock = side;
    layoutDock();
    focusWindow(w, true);
  }

  function undock(w) {
    if (!w.dock) return;
    docked[w.dock] = null;
    w.dock = null;
    layoutDock();
    applyRect(w);
    focusWindow(w, true);
  }

  function setDockPct(side, pct) {
    dockPct[side] = clampPct(side, pct);
    layoutDock();
  }

  function splitterFor(side) {
    if (splitters[side]) return splitters[side];
    const sp = h("div", {
      class: "dgw-splitter dgw-splitter-" + side,
      role: "separator",
      tabindex: 0,
      "aria-orientation": "vertical",
      "aria-valuemin": String(DOCK_MIN),
      "aria-valuemax": String(DOCK_MAX),
      "aria-label": "Resize the window docked " + side,
      title: "Drag to resize; double-click to reset",
    });
    let drag = null;
    sp.addEventListener("pointerdown", (e) => {
      if (e.pointerType === "mouse" && e.button !== 0) return;
      sp.setPointerCapture(e.pointerId);
      drag = e.pointerId;
      document.documentElement.classList.add("dgw-resizing");
      e.preventDefault();
    });
    sp.addEventListener("pointermove", (e) => {
      if (drag !== e.pointerId) return;
      const vw = viewport().w;
      setDockPct(side, ((side === "left" ? e.clientX : vw - e.clientX) / vw) * 100);
    });
    const end = (e) => {
      if (drag !== e.pointerId) return;
      drag = null;
      document.documentElement.classList.remove("dgw-resizing");
    };
    sp.addEventListener("pointerup", end);
    sp.addEventListener("pointercancel", end);
    sp.addEventListener("dblclick", () => setDockPct(side, DOCK_DEFAULT));
    sp.addEventListener("keydown", (e) => {
      // Arrow keys move the splitter itself: → widens a left window and narrows a right one.
      const grow = side === "left" ? 1 : -1;
      let pct = null;
      if (e.key === "ArrowLeft") pct = dockPct[side] - grow * DOCK_KEY_STEP;
      else if (e.key === "ArrowRight") pct = dockPct[side] + grow * DOCK_KEY_STEP;
      else if (e.key === "Home") pct = DOCK_MIN;
      else if (e.key === "End") pct = DOCK_MAX;
      if (pct == null) return;
      e.preventDefault();
      setDockPct(side, pct);
    });
    splitters[side] = sp;
    document.body.appendChild(sp);
    return sp;
  }

  /** Lay out the docked windows, their splitters and the host page's room for them. */
  function layoutDock() {
    const root = document.documentElement;
    const top = dockTop();
    const vw = viewport().w;
    for (const side of ["left", "right"]) {
      const w = docked[side];
      if (!w) {
        root.style.removeProperty("--dock-" + side);
        if (splitters[side]) {
          splitters[side].remove();
          splitters[side] = null;
        }
        continue;
      }
      dockPct[side] = clampPct(side, dockPct[side]);
      const width = dockWidth(side);
      root.style.setProperty("--dock-" + side, width + "px");
      const sp = splitterFor(side);
      sp.style.top = top + "px";
      sp.style.left = (side === "left" ? width : vw - width) + "px";
      sp.setAttribute("aria-valuenow", String(Math.round(dockPct[side])));
      sp.setAttribute("aria-valuetext", Math.round(dockPct[side]) + "% of the width");
      sp.setAttribute("aria-controls", w.el.id);
      applyRect(w);
    }
    root.classList.toggle("has-docked", !!(docked.left || docked.right));
    restack();
    window.dispatchEvent(new CustomEvent("agtrack:dock", { detail: { left: !!docked.left, right: !!docked.right } }));
  }

  function closeWindow(w) {
    windows.delete(w.key);
    const i = stack.indexOf(w);
    if (i >= 0) stack.splice(i, 1);
    if (w.chip) w.chip.remove();
    clearTimeout(w.flagTimer);
    w.el.remove();
    if (w.dock) {
      docked[w.dock] = null;
      w.dock = null;
      layoutDock();
    }
    restack();
    if (!windows.size) lastRect = null;
    const info = ctx && collect(ctx.root).get(w.key);
    const btn = info && info.host.querySelector(":scope > .diagram-expand");
    if (btn) btn.focus({ preventScroll: true });
  }

  // ------------------------------------------------------------------------------------
  // Pan and zoom
  // ------------------------------------------------------------------------------------

  function canvasSize(w) {
    return { w: w.canvas.clientWidth, h: w.canvas.clientHeight };
  }

  function paint(w) {
    w.stage.style.transform = "translate(" + w.tx + "px," + w.ty + "px) scale(" + w.scale + ")";
    w.btnPct.textContent = Math.round(w.scale * 100) + "%";
  }

  function fit(w) {
    const c = canvasSize(w);
    if (c.w <= 0 || c.h <= 0) return;
    const s = clamp(Math.min((c.w - 2 * FIT_PAD) / w.iw, (c.h - 2 * FIT_PAD) / w.ih), ZOOM_MIN, ZOOM_MAX);
    w.scale = s;
    w.tx = (c.w - w.iw * s) / 2;
    w.ty = (c.h - w.ih * s) / 2;
    w.fitted = true;
    paint(w);
  }

  function zoomAt(w, factor, px, py) {
    const ns = clamp(w.scale * factor, ZOOM_MIN, ZOOM_MAX);
    const k = ns / w.scale;
    w.tx = px - (px - w.tx) * k;
    w.ty = py - (py - w.ty) * k;
    w.scale = ns;
    w.fitted = false;
    paint(w);
  }

  function zoomCentre(w, factor) {
    const c = canvasSize(w);
    zoomAt(w, factor, c.w / 2, c.h / 2);
  }

  function setActual(w) {
    const c = canvasSize(w);
    zoomAt(w, 1 / w.scale, c.w / 2, c.h / 2);
  }

  function panBy(w, dx, dy) {
    w.tx += dx;
    w.ty += dy;
    w.fitted = false;
    paint(w);
  }

  function wireCanvas(w) {
    const cv = w.canvas;
    const pts = new Map();
    let pinch = null;

    const local = (e) => {
      const r = cv.getBoundingClientRect();
      return { x: e.clientX - r.left, y: e.clientY - r.top };
    };
    const pinchState = () => {
      const [a, b] = Array.from(pts.values());
      return { d: Math.hypot(a.x - b.x, a.y - b.y) || 1, mx: (a.x + b.x) / 2, my: (a.y + b.y) / 2 };
    };

    cv.addEventListener("pointerdown", (e) => {
      if (e.pointerType === "mouse" && e.button !== 0) return;
      cv.setPointerCapture(e.pointerId);
      pts.set(e.pointerId, local(e));
      cv.classList.add("is-panning");
      pinch = pts.size === 2 ? pinchState() : null;
    });
    cv.addEventListener("pointermove", (e) => {
      if (!pts.has(e.pointerId)) return;
      const prev = pts.get(e.pointerId);
      const cur = local(e);
      pts.set(e.pointerId, cur);
      if (pts.size === 1) {
        panBy(w, cur.x - prev.x, cur.y - prev.y);
      } else if (pts.size === 2 && pinch) {
        // Pan by the midpoint's move first, then zoom about where the midpoint is
        // now: the content under the fingers stays under them.
        const next = pinchState();
        panBy(w, next.mx - pinch.mx, next.my - pinch.my);
        zoomAt(w, next.d / pinch.d, next.mx, next.my);
        pinch = next;
      }
    });
    const end = (e) => {
      pts.delete(e.pointerId);
      pinch = pts.size === 2 ? pinchState() : null;
      if (!pts.size) cv.classList.remove("is-panning");
    };
    cv.addEventListener("pointerup", end);
    cv.addEventListener("pointercancel", end);
    cv.addEventListener(
      "wheel",
      (e) => {
        e.preventDefault();
        const unit = e.deltaMode === 1 ? 16 : e.deltaMode === 2 ? 400 : 1;
        const dy = e.deltaY * unit;
        const p = local(e);
        zoomAt(w, Math.exp(-dy * (e.ctrlKey ? 0.01 : 0.0015)), p.x, p.y);
      },
      { passive: false }
    );
  }

  // ------------------------------------------------------------------------------------
  // Dragging and resizing
  // ------------------------------------------------------------------------------------

  function wireDrag(w) {
    let drag = null;
    w.bar.addEventListener("pointerdown", (e) => {
      if ((e.pointerType === "mouse" && e.button !== 0) || e.target.closest("button") || w.max || w.dock) return;
      w.bar.setPointerCapture(e.pointerId);
      drag = { id: e.pointerId, dx: e.clientX - w.rect.x, dy: e.clientY - w.rect.y };
    });
    w.bar.addEventListener("pointermove", (e) => {
      if (!drag || drag.id !== e.pointerId) return;
      w.rect = clampRect({ ...w.rect, x: e.clientX - drag.dx, y: e.clientY - drag.dy });
      applyRect(w);
    });
    const end = (e) => {
      if (drag && drag.id === e.pointerId) drag = null;
    };
    w.bar.addEventListener("pointerup", end);
    w.bar.addEventListener("pointercancel", end);
  }

  function wireResize(w) {
    let rs = null;
    w.grip.addEventListener("pointerdown", (e) => {
      if ((e.pointerType === "mouse" && e.button !== 0) || w.max || w.dock) return;
      w.grip.setPointerCapture(e.pointerId);
      rs = { id: e.pointerId, x: e.clientX, y: e.clientY, w: w.rect.w, h: w.rect.h };
      e.preventDefault();
    });
    w.grip.addEventListener("pointermove", (e) => {
      if (!rs || rs.id !== e.pointerId) return;
      const v = viewport();
      w.rect = clampRect({
        ...w.rect,
        w: clamp(rs.w + e.clientX - rs.x, MIN_W, Math.max(MIN_W, v.w - w.rect.x)),
        h: clamp(rs.h + e.clientY - rs.y, MIN_H, Math.max(MIN_H, v.h - w.rect.y)),
      });
      applyRect(w);
    });
    const end = (e) => {
      if (rs && rs.id === e.pointerId) rs = null;
    };
    w.grip.addEventListener("pointerup", end);
    w.grip.addEventListener("pointercancel", end);
  }

  // ------------------------------------------------------------------------------------
  // Source view
  // ------------------------------------------------------------------------------------

  function setSourceView(w, on) {
    w.sourceView = on;
    w.canvas.hidden = on;
    w.sourceEl.hidden = !on;
    w.btnSource.setAttribute("aria-pressed", String(on));
    for (const b of [w.btnOut, w.btnPct, w.btnIn, w.btnFit]) b.disabled = on;
    if (on) w.sourceEl.focus({ preventScroll: true });
    else if (w.fitted) fit(w);
    else paint(w);
  }

  function copySource(w) {
    navigator.clipboard.writeText(w.source).then(
      () => ctx.toast("Copied diagram source"),
      () => ctx.toast("Could not copy to clipboard", true)
    );
  }

  // ------------------------------------------------------------------------------------
  // Save as PNG
  // ------------------------------------------------------------------------------------
  //
  // serialise the SVG with the vendored fonts inlined as data: URLs, load it as a
  // data:image/svg+xml image, draw it on a canvas at 2x, toBlob. A foreignObject (Mermaid's
  // HTML labels) taints the canvas, so such a diagram is re-rendered off-screen with
  // htmlLabels:false first. All of it is allowed by the existing CSP (img-src data:,
  // connect-src 'self', font-src 'self').

  const EXPORT_SCALE = 2;
  const MAX_CANVAS_SIDE = 16384;
  const FONTS = [
    { family: "Inter Variable", weight: "100 900", url: "/assets/vendor/fonts/inter-latin-wght-normal.woff2" },
    { family: "JetBrains Mono Variable", weight: "100 800", url: "/assets/vendor/fonts/jetbrains-mono-latin-wght-normal.woff2" },
  ];
  const fontData = new Map(); // url → Promise<data: URL>

  function bufferToBase64(buf) {
    const bytes = new Uint8Array(buf);
    let bin = "";
    for (let i = 0; i < bytes.length; i += 0x8000) bin += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000));
    return btoa(bin);
  }

  function loadFont(f) {
    if (!fontData.has(f.url)) {
      fontData.set(
        f.url,
        fetch(f.url)
          .then((r) => {
            if (!r.ok) throw new Error("font " + f.url + ": HTTP " + r.status);
            return r.arrayBuffer();
          })
          .then((buf) => "data:font/woff2;base64," + bufferToBase64(buf))
          .catch((err) => {
            fontData.delete(f.url);
            throw err;
          })
      );
    }
    return fontData.get(f.url);
  }

  async function fontCss(markup) {
    let css = "";
    for (const f of FONTS) {
      if (!markup.includes(f.family.replace(" Variable", ""))) continue;
      const data = await loadFont(f);
      css += '@font-face{font-family:"' + f.family + '";font-style:normal;font-weight:' + f.weight +
        ";src:url(" + data + ') format("woff2");}';
    }
    return css;
  }

  /**
   * Mermaid's SVG text for the source, rendered without foreignObject (pure SVG text).
   * The setting rides on an init directive in front of the source, so Mermaid's global
   * config is never touched and a live refresh rendering at the same time is unaffected.
   */
  async function renderPlain(w) {
    const m = window.mermaid;
    if (!m) throw new Error("Mermaid is not available");
    const id = "dgw-export-" + ++seq;
    const plain = '%%{init: {"htmlLabels": false, "flowchart": {"htmlLabels": false}}}%%\n' + w.source;
    try {
      const { svg } = await m.render(id, plain);
      return svg;
    } finally {
      const stray = document.getElementById("d" + id);
      if (stray) stray.remove();
    }
  }

  /** The export SVG as {xml, w, h}: pure SVG, intrinsic size, fonts inlined. */
  async function exportSvg(w) {
    let svg = w.svgNode.cloneNode(true);
    if (svg.querySelector("foreignObject")) {
      const markup = await renderPlain(w);
      // Parsed into a detached document: nothing from it is ever attached to the page.
      const doc = new DOMParser().parseFromString(markup, "text/html");
      svg = doc.body.querySelector("svg");
      if (!svg) throw new Error("Mermaid returned no SVG");
      if (svg.querySelector("foreignObject")) throw new Error("diagram still uses HTML labels");
    }
    const size = prepareSvg(svg);
    svg = size.node;
    svg.removeAttribute("style");
    svg.setAttribute("xmlns", SVG_NS);
    const css = await fontCss(new XMLSerializer().serializeToString(svg));
    if (css) {
      const style = document.createElementNS(SVG_NS, "style");
      style.textContent = css;
      svg.insertBefore(style, svg.firstChild);
    }
    return { xml: new XMLSerializer().serializeToString(svg), w: size.w, h: size.h };
  }

  function loadImage(src) {
    return new Promise((resolve, reject) => {
      const img = new Image();
      img.onload = () => resolve(img);
      img.onerror = () => reject(new Error("the browser could not load the SVG as an image"));
      img.src = src;
    });
  }

  /** PNG blob of the whole diagram at 2x its intrinsic size on the page background. */
  async function renderPng(w) {
    w.lastSvg = null; // Save SVG offers only what this attempt produced
    const out = await exportSvg(w);
    w.lastSvg = out.xml;
    const img = await loadImage("data:image/svg+xml;charset=utf-8," + encodeURIComponent(out.xml));
    if (img.decode) await img.decode().catch(() => {});
    const k = Math.min(EXPORT_SCALE, MAX_CANVAS_SIDE / Math.max(out.w, out.h));
    const canvas = document.createElement("canvas");
    canvas.width = Math.max(1, Math.round(out.w * k));
    canvas.height = Math.max(1, Math.round(out.h * k));
    const g = canvas.getContext("2d");
    g.fillStyle = getComputedStyle(document.body).backgroundColor || "#ffffff";
    g.fillRect(0, 0, canvas.width, canvas.height);
    g.drawImage(img, 0, 0, canvas.width, canvas.height);
    return new Promise((resolve, reject) => {
      try {
        canvas.toBlob((blob) => (blob ? resolve(blob) : reject(new Error("the canvas produced no image"))), "image/png");
      } catch (err) {
        reject(err);
      }
    });
  }

  function exportName(w, ext) {
    const safe = (v) => String(v).replace(/[^\w.-]+/g, "-");
    return safe(ctx.specSlug) + "-" + safe(w.slug) + "-diagram-" + w.idx + "." + ext;
  }

  function download(blob, name) {
    const url = URL.createObjectURL(blob);
    const a = h("a", { href: url, download: name });
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 10000);
  }

  async function savePng(w) {
    if (w.exporting) return;
    w.exporting = true;
    w.btnPng.disabled = true;
    try {
      const blob = await renderPng(w);
      const name = exportName(w, "png");
      download(blob, name);
      ctx.toast("Saved " + name);
      w.btnSvg.hidden = true;
    } catch (err) {
      console.error("diagram PNG export failed", err);
      const why = err && err.message ? err.message.replace(/\.$/, "") : String(err);
      // Save SVG only when this attempt produced an SVG; otherwise the error is all there is.
      ctx.toast(w.lastSvg ? "Could not save PNG (" + why + "). Use Save SVG instead." : "Could not export the diagram (" + why + ").", true);
      w.btnSvg.hidden = !w.lastSvg;
    } finally {
      w.exporting = false;
      w.btnPng.disabled = false;
    }
  }

  function saveSvg(w) {
    if (!w.lastSvg) return;
    const name = exportName(w, "svg");
    download(new Blob([w.lastSvg], { type: "image/svg+xml" }), name);
    ctx.toast("Saved " + name);
  }

  // ------------------------------------------------------------------------------------
  // Window construction
  // ------------------------------------------------------------------------------------

  function openWindow(info) {
    let w = windows.get(info.key);
    if (w) {
      if (w.minimised) restoreFromDock(w);
      else focusWindow(w, true);
      return;
    }
    const v = viewport();
    const width = clamp(Math.round(v.w * 0.6), MIN_W, Math.max(MIN_W, v.w - 2 * CASCADE));
    const height = clamp(Math.round(v.h * 0.7), MIN_H, Math.max(MIN_H, v.h - 2 * CASCADE));
    const base = lastRect
      ? { x: lastRect.x + CASCADE, y: lastRect.y + CASCADE }
      : { x: v.w - width - CASCADE, y: Math.round((v.h - height) / 2) };
    const rect = clampRect({ x: base.x, y: base.y, w: width, h: height });
    lastRect = rect;

    const prepared = prepareSvg(info.svg);
    w = {
      key: info.key,
      slug: info.slug,
      idx: info.idx,
      source: info.source,
      title: info.title,
      svgNode: info.svg.cloneNode(true),
      iw: prepared.w,
      ih: prepared.h,
      rect,
      // On a phone a floating window opens maximised; Restore still returns to `rect`.
      max: v.w < PHONE_VW,
      dock: null,
      minimised: false,
      sourceView: false,
      exporting: false,
      lastSvg: "",
      fitted: true,
      scale: 1,
      tx: 0,
      ty: 0,
      chip: null,
      flagTimer: 0,
    };

    const id = "dgw-title-" + ++seq;
    w.titleEl = h("span", { class: "dgw-title", id, text: w.title });
    w.flagEl = h("span", { class: "dgw-flag", "aria-live": "polite" });
    w.btnDockLeft = h("button", { type: "button", class: "dgw-btn dgw-dock-btn", "aria-label": "Dock left", title: "Dock left (Alt+←)", on: { click: () => dockWindow(w, "left") } }, icon("dockLeft"));
    w.btnDockRight = h("button", { type: "button", class: "dgw-btn dgw-dock-btn", "aria-label": "Dock right", title: "Dock right (Alt+→)", on: { click: () => dockWindow(w, "right") } }, icon("dockRight"));
    w.btnUndock = h("button", { type: "button", class: "dgw-btn", "aria-label": "Undock", title: "Undock (Alt+↓)", hidden: true, on: { click: () => undock(w) } }, icon("undock"));
    w.btnMin = h("button", { type: "button", class: "dgw-btn", "aria-label": "Minimise", title: "Minimise", on: { click: () => minimise(w) } }, icon("min"));
    w.btnMax = h("button", { type: "button", class: "dgw-btn", "aria-label": "Maximise", title: "Maximise", on: { click: () => toggleMax(w) } }, icon("max"));
    const btnClose = h("button", { type: "button", class: "dgw-btn dgw-close", "aria-label": "Close", title: "Close", on: { click: () => closeWindow(w) } }, icon("close"));
    w.bar = h("div", { class: "dgw-bar", on: { dblclick: (e) => !e.target.closest("button") && toggleMax(w) } },
      h("div", { class: "dgw-heading" }, w.titleEl, w.flagEl),
      h("div", { class: "dgw-ctl" }, w.btnDockLeft, w.btnDockRight, w.btnUndock, w.btnMin, w.btnMax, btnClose));

    w.btnOut = h("button", { type: "button", class: "dgw-tool", "aria-label": "Zoom out", title: "Zoom out (-)", text: "−", on: { click: () => zoomCentre(w, 1 / ZOOM_STEP) } });
    w.btnPct = h("button", {
      type: "button",
      class: "dgw-tool dgw-pct",
      title: "Toggle Fit / 100%",
      text: "100%",
      on: { click: () => (w.fitted ? setActual(w) : fit(w)) },
    });
    w.btnIn = h("button", { type: "button", class: "dgw-tool", "aria-label": "Zoom in", title: "Zoom in (+)", text: "+", on: { click: () => zoomCentre(w, ZOOM_STEP) } });
    w.btnFit = h("button", { type: "button", class: "dgw-tool", title: "Fit to window (0)", text: "Fit", on: { click: () => fit(w) } });
    w.btnSource = h("button", { type: "button", class: "dgw-tool", "aria-pressed": "false", title: "Show Mermaid source", text: "Source", on: { click: () => setSourceView(w, !w.sourceView) } });
    const btnCopy = h("button", { type: "button", class: "dgw-tool", title: "Copy Mermaid source", text: "Copy", on: { click: () => copySource(w) } });
    w.btnPng = h("button", { type: "button", class: "dgw-tool", title: "Save as PNG (2x)", text: "Save PNG", on: { click: () => savePng(w) } });
    w.btnSvg = h("button", { type: "button", class: "dgw-tool", title: "PNG export failed; save the SVG instead", text: "Save SVG", hidden: true, on: { click: () => saveSvg(w) } });
    w.tools = h("div", { class: "dgw-tools", role: "toolbar", "aria-label": "Diagram tools" },
      w.btnOut, w.btnPct, w.btnIn, w.btnFit, h("span", { class: "dgw-sep" }), w.btnSource, btnCopy, w.btnPng, w.btnSvg);

    w.stage = h("div", { class: "dgw-stage" });
    w.stage.style.width = w.iw + "px";
    w.stage.style.height = w.ih + "px";
    w.stage.appendChild(prepared.node);
    w.canvas = h("div", { class: "dgw-canvas" }, w.stage);
    w.sourceEl = h("pre", { class: "dgw-source", tabindex: 0, hidden: true, text: w.source });
    w.body = h("div", { class: "dgw-body" }, w.canvas, w.sourceEl);
    w.grip = h("div", { class: "dgw-grip", "aria-hidden": "true" });

    w.el = h("div", {
      id: "dgw-" + seq,
      class: "dgw",
      role: "dialog",
      "aria-modal": "false",
      "aria-labelledby": id,
      tabindex: -1,
      on: {
        pointerdown: () => focusWindow(w, false),
        keydown: (e) => onKey(w, e),
      },
    }, w.bar, w.tools, w.body, w.grip);

    document.body.appendChild(w.el);
    windows.set(w.key, w);
    wireCanvas(w);
    wireDrag(w);
    wireResize(w);
    if (typeof ResizeObserver === "function") {
      new ResizeObserver(() => {
        if (w.fitted && !w.minimised && !w.sourceView) fit(w);
      }).observe(w.canvas);
    }
    applyRect(w);
    fit(w);
    focusWindow(w, true);
  }

  function onKey(w, e) {
    if (e.altKey && !e.ctrlKey && !e.metaKey && !e.shiftKey) {
      if (e.key === "ArrowLeft" && canDock()) dockWindow(w, "left");
      else if (e.key === "ArrowRight" && canDock()) dockWindow(w, "right");
      else if (e.key === "ArrowDown" && w.dock) undock(w);
      else return;
      e.preventDefault();
      e.stopPropagation();
      return;
    }
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    if (e.key === "Escape") {
      if (w.sourceView) setSourceView(w, false);
      else if (w.max) toggleMax(w);
      else return;
      e.preventDefault();
      e.stopPropagation();
      return;
    }
    if (w.sourceView) return;
    const stop = () => {
      e.preventDefault();
      e.stopPropagation();
    };
    switch (e.key) {
      case "+":
      case "=":
        zoomCentre(w, ZOOM_STEP);
        return stop();
      case "-":
      case "_":
        zoomCentre(w, 1 / ZOOM_STEP);
        return stop();
      case "0":
        fit(w);
        return stop();
      case "1":
        setActual(w);
        return stop();
      case "ArrowLeft":
        panBy(w, PAN_STEP, 0);
        return stop();
      case "ArrowRight":
        panBy(w, -PAN_STEP, 0);
        return stop();
      case "ArrowUp":
        panBy(w, 0, PAN_STEP);
        return stop();
      case "ArrowDown":
        panBy(w, 0, -PAN_STEP);
        return stop();
    }
  }

  // ------------------------------------------------------------------------------------
  // Live refresh
  // ------------------------------------------------------------------------------------

  function flag(w, text, cls) {
    clearTimeout(w.flagTimer);
    w.flagEl.textContent = text;
    w.flagEl.className = "dgw-flag" + (text ? " " + cls : "");
    if (text && cls === "is-updated") {
      w.flagTimer = setTimeout(() => {
        w.flagEl.textContent = "";
        w.flagEl.className = "dgw-flag";
      }, 3000);
    }
  }

  function reconcile() {
    if (!windows.size) return;
    const now = collect(ctx.root);
    for (const w of windows.values()) {
      const info = now.get(w.key);
      if (!info) {
        flag(w, "no longer in the spec", "is-gone");
        continue;
      }
      if (w.flagEl.classList.contains("is-gone")) flag(w, "", "");
      if (info.title !== w.title) {
        w.title = info.title;
        w.titleEl.textContent = w.title;
        if (w.chip) {
          w.chip.title = "Restore " + w.title;
          w.chip.querySelector(".dgw-chip-text").textContent = w.title;
        }
      }
      if (info.source === w.source) continue;
      const prepared = prepareSvg(info.svg);
      w.source = info.source;
      w.svgNode = info.svg.cloneNode(true);
      w.iw = prepared.w;
      w.ih = prepared.h;
      w.stage.style.width = w.iw + "px";
      w.stage.style.height = w.ih + "px";
      w.stage.replaceChildren(prepared.node);
      w.sourceEl.textContent = w.source;
      w.lastSvg = "";
      w.btnSvg.hidden = true;
      w.fitted = true;
      if (!w.minimised && !w.sourceView) fit(w);
      flag(w, "updated", "is-updated");
    }
  }

  window.addEventListener("resize", () => {
    // Too narrow to split: every docked window goes back to floating.
    const undocking = !canDock() && (docked.left || docked.right);
    if (undocking) {
      for (const side of ["left", "right"]) {
        if (docked[side]) docked[side].dock = null;
        docked[side] = null;
      }
    }
    for (const w of windows.values()) {
      w.rect = clampRect(w.rect);
      applyRect(w);
    }
    if (undocking || docked.left || docked.right) layoutDock();
  });

  window.AgtrackDiagram = {
    update(c) {
      ctx = c;
      wire(c.root);
      decorate(c.root);
      reconcile();
    },
  };
})();
