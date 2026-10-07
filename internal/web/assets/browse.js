// agtrack serve — the browse app. Vanilla ES module, no build step, no framework.
//
// Security invariants this file must preserve (docs/viewer-api.md):
//   - Every string from the store (titles, notes, log entries, actors, refs) is untrusted
//     text: always textContent / text nodes, never innerHTML. There is no HTML-string path.
//   - Links detected in text are only ever http(s) URLs, set through setAttribute("href").
//   - The capability token lives in memory only; it is stripped from the address bar on load
//     and sent as a header, never re-embedded in the DOM or in request bodies.

const SSE_DEBOUNCE_MS = 300;
const LOG_PAGE = 50;
const SVG_NS = "http://www.w3.org/2000/svg";
const NOTE_KINDS = ["note", "decision", "handoff", "blocker", "question"];
const STATUSES = ["todo", "in-progress", "done", "cancelled"];
const STATUS_LABEL = { todo: "todo", "in-progress": "in progress", done: "done", cancelled: "cancelled" };
const STATUS_MARK = { todo: "[ ]", "in-progress": "[~]", done: "[x]", cancelled: "[-]" };

// ---------------------------------------------------------------------------
// Boot: take the token from the URL, then strip it.
// ---------------------------------------------------------------------------

function bootSession() {
  const params = new URLSearchParams(location.search);
  const token = params.get("t") || "";
  params.delete("t");
  const qs = params.toString();
  history.replaceState(null, "", location.pathname + (qs ? "?" + qs : "") + location.hash);
  return { token };
}

const session = bootSession();

// ---------------------------------------------------------------------------
// API client
// ---------------------------------------------------------------------------

class ApiError extends Error {
  constructor(token, message, payload, status) {
    super(message);
    this.token = token;
    this.payload = payload;
    this.status = status;
  }
}

async function api(path, opts = {}) {
  // After a reload the token is gone from the URL; the session cookie set on the
  // first load authenticates instead.
  const headers = session.token ? { "X-Agtrack-Token": session.token } : {};
  let body;
  if (opts.body !== undefined) {
    headers["Content-Type"] = "application/json";
    body = JSON.stringify(opts.body);
  }
  let res;
  try {
    res = await fetch(path, { method: opts.method || "GET", headers, body });
  } catch {
    throw new ApiError("network_error", "Could not reach the server.", null, 0);
  }
  let payload = null;
  const text = await res.text();
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch {
      payload = null;
    }
  }
  if (!res.ok) {
    const tok = (payload && payload.error) || "error";
    const msg = (payload && payload.message) || res.statusText || "Request failed.";
    throw new ApiError(tok, msg, payload, res.status);
  }
  return payload;
}

const enc = encodeURIComponent;
const get = (p) => api("/api/browse/" + p);
const post = (p, body) => api("/api/browse/" + p, { method: "POST", body });

function newIdem() {
  if (window.crypto && crypto.randomUUID) return crypto.randomUUID();
  const b = new Uint8Array(16);
  crypto.getRandomValues(b);
  return Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
}

// ---------------------------------------------------------------------------
// DOM helpers (text only)
// ---------------------------------------------------------------------------

/**
 * h("div", {class: "x", title: "y", on: {click: fn}, data: {k: v}}, child, "text", …)
 * Strings become text nodes; null/false children are skipped; arrays are flattened.
 */
function h(tag, attrs, ...children) {
  const node = document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v == null || v === false) continue;
      if (k === "class") node.className = v;
      else if (k === "text") node.textContent = v;
      else if (k === "on") for (const [ev, fn] of Object.entries(v)) node.addEventListener(ev, fn);
      else if (k === "data") for (const [dk, dv] of Object.entries(v)) node.dataset[dk] = String(dv);
      else if (k in node && typeof v !== "string") node[k] = v;
      else node.setAttribute(k, v === true ? "" : String(v));
    }
  }
  append(node, children);
  return node;
}

function append(node, children) {
  for (const c of children) {
    if (c == null || c === false) continue;
    if (Array.isArray(c)) append(node, c);
    else node.appendChild(typeof c === "string" || typeof c === "number" ? document.createTextNode(String(c)) : c);
  }
}

const ICONS = {
  copy: "M8 16H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h8a2 2 0 0 1 2 2v2m-6 12h8a2 2 0 0 0 2-2v-8a2 2 0 0 0-2-2h-8a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2z",
  doc: "M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8zM14 2v6h6M16 13H8M16 17H8",
  lock: "M12 15v2m-6 4h12a2 2 0 0 0 2-2v-6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v6a2 2 0 0 0 2 2zm10-10V7a4 4 0 0 0-8 0v4h8z",
  plus: "M12 5v14M5 12h14",
};

function icon(name) {
  const svg = document.createElementNS(SVG_NS, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("aria-hidden", "true");
  const path = document.createElementNS(SVG_NS, "path");
  path.setAttribute("d", ICONS[name]);
  svg.appendChild(path);
  return svg;
}

const URL_RE = /\bhttps?:\/\/[^\s<>"'`]+[^\s<>"'`.,;:!?)\]}]/g;

// linkify renders plain text with http(s) URLs as links. Never markdown, never HTML.
function linkify(text) {
  const frag = document.createDocumentFragment();
  const s = String(text == null ? "" : text);
  let last = 0;
  for (const m of s.matchAll(URL_RE)) {
    if (m.index > last) frag.appendChild(document.createTextNode(s.slice(last, m.index)));
    const a = document.createElement("a");
    a.setAttribute("href", m[0]);
    a.setAttribute("target", "_blank");
    a.setAttribute("rel", "noopener noreferrer");
    a.textContent = m[0];
    frag.appendChild(a);
    last = m.index + m[0].length;
  }
  if (last < s.length) frag.appendChild(document.createTextNode(s.slice(last)));
  return frag;
}

function parseTS(ts) {
  if (!ts) return null;
  const d = new Date(ts);
  return isNaN(d.getTime()) ? null : d;
}

function fmtTS(ts) {
  const d = parseTS(ts);
  if (!d) return "";
  const pad = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function ago(ts) {
  const d = parseTS(ts);
  if (!d) return "";
  const s = Math.round((Date.now() - d.getTime()) / 1000);
  const abs = Math.abs(s);
  let v;
  if (abs < 60) v = "now";
  else if (abs < 3600) v = Math.round(abs / 60) + "m";
  else if (abs < 86400) v = Math.round(abs / 3600) + "h";
  else if (abs < 86400 * 60) v = Math.round(abs / 86400) + "d";
  else v = Math.round(abs / (86400 * 30)) + "mo";
  if (v === "now") return v;
  return s >= 0 ? v + " ago" : "in " + v;
}

function when(ts) {
  return h("time", { class: "b-when", datetime: ts || "", title: fmtTS(ts) }, ago(ts));
}

let toastTimer = null;
function showToast(message, isError) {
  const t = document.getElementById("toast");
  t.textContent = message;
  t.classList.toggle("is-error", !!isError);
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), 4000);
}

function copyRef(text) {
  navigator.clipboard.writeText(text).then(
    () => showToast("Copied " + text),
    () => showToast("Could not copy to clipboard", true)
  );
}

function copyBtn(ref, label) {
  return h(
    "button",
    { type: "button", class: "b-copy", title: "Copy CLI reference " + ref, "aria-label": "Copy reference " + ref, on: { click: () => copyRef(ref) } },
    icon("copy"),
    label || ref
  );
}

// ---------------------------------------------------------------------------
// Small components
// ---------------------------------------------------------------------------

const state = {
  viewer: "",
  start: "",
  cursor: 0,
  contexts: [],
  es: null,
  sseTimer: null,
  pending: false,
  drafts: new Map(), // route|form key → unsent text, kept across re-renders
  renderSeq: 0,
  stopped: false,
};

const isMine = (actor) => !!state.viewer && actor === state.viewer;

function actorEl(actor) {
  if (!actor) return h("span", { class: "b-actor is-none" }, "—");
  const mine = isMine(actor);
  return h(
    "span",
    { class: "b-actor" + (mine ? " is-mine" : ""), title: mine ? `${actor} (you, in this browser)` : `${actor} (agent)` },
    actor,
    mine ? h("span", { class: "b-you" }, "you") : null
  );
}

function statusBadge(st) {
  return h("span", { class: "b-status", data: { status: st || "todo" } }, STATUS_LABEL[st] || st || "");
}

function kindBadge(kind) {
  const k = kind || "note";
  return h("span", { class: "b-kind", data: { kind: k } }, k);
}

function tagChips(tags) {
  const list = Array.isArray(tags) ? tags : String(tags || "").split(",").filter(Boolean);
  return list.map((t) => h("span", { class: "b-tag" }, t));
}

function card(title, extra, ...children) {
  return h("section", { class: "b-card" }, h("div", { class: "b-card-head" }, h("h2", null, title), extra || null), ...children);
}

function empty(text) {
  return h("p", { class: "b-empty" }, text);
}

function field(label, control) {
  return h("label", { class: "b-field" }, h("span", null, label), control);
}

function selectEl(name, options, value) {
  const s = h("select", { name });
  for (const o of options) {
    const [v, label] = Array.isArray(o) ? o : [o, o];
    s.appendChild(h("option", { value: v, selected: v === value }, label));
  }
  return s;
}

// Hrefs for the hash routes.
const projPath = (path) => String(path).split("/").map(enc).join("/");
const hrefCtx = (name) => "#/c/" + enc(name);
const hrefProject = (path, tab) => "#/p/" + projPath(path) + (tab ? "?tab=" + tab : "");
const hrefTask = (id) => "#/t/" + enc(id);
const hrefSpec = (qref, slug) => "/s/" + enc(qref) + (slug ? "#" + enc(slug) : "");

function taskLink(id, title) {
  return h("a", { class: "b-ref", href: hrefTask(id) }, "#" + id, title ? h("span", { class: "b-ref-title" }, " " + title) : null);
}

// noteWhere links a note to where it lives (task, step, project, spec).
function noteWhere(n) {
  if (n.anchor || n.spec_slug) {
    return n.spec
      ? h("a", { class: "b-ref", href: hrefSpec(n.spec), title: "Open in the spec viewer" }, icon("doc"), n.spec_slug + (n.anchor ? "#" + n.anchor : ""))
      : h("span", null, n.spec_slug || "spec");
  }
  if (n.task_id) return h("span", null, taskLink(n.task_id, n.task_title), n.step ? h("span", { class: "b-muted" }, " step " + n.step) : null);
  if (n.project) return h("a", { class: "b-ref", href: hrefProject(n.project, "notes") }, n.project);
  if (n.context) return h("a", { class: "b-ref", href: hrefCtx(n.context) }, n.context);
  return null;
}

// markDirty: a form with unsaved input holds off live re-renders until it is
// submitted or cancelled, so an incoming event never wipes what is being typed.
function trackDirty(form) {
  form.addEventListener("input", () => (form.dataset.dirty = "1"));
  return form;
}

// Drafts: text typed into a form survives every re-render (a write elsewhere on the page,
// a live refresh, a cancel of another form). A control opts in through keep(); a draft is
// dropped only when its own form submits successfully or is cancelled.
function routePath() {
  return location.hash.replace(/\?.*/, "");
}

function keep(key, control) {
  const k = routePath() + "|" + key;
  control.dataset.dkey = k;
  if (state.drafts.has(k)) control.value = state.drafts.get(k);
  control.addEventListener("input", () => state.drafts.set(k, control.value));
  return control;
}

function hasDraft(key) {
  return !!state.drafts.get(routePath() + "|" + key);
}

function clearDrafts(form) {
  for (const c of form.querySelectorAll("[data-dkey]")) state.drafts.delete(c.dataset.dkey);
  delete form.dataset.dirty;
}

async function busy(form, fn) {
  if (form.dataset.busy === "1") return;
  form.dataset.busy = "1";
  const buttons = Array.from(form.querySelectorAll("button"));
  for (const b of buttons) b.disabled = true;
  try {
    await fn();
  } finally {
    delete form.dataset.busy;
    for (const b of buttons) b.disabled = false;
  }
}

// write posts, toasts the outcome and re-renders the view.
async function write(path, body, okText, form) {
  try {
    const res = await post(path, body);
    if (form) clearDrafts(form);
    if (okText) showToast(okText);
    await render({ keepScroll: true, force: true });
    return res;
  } catch (err) {
    if (err.status !== 409) showToast(err.message, true);
    throw err;
  }
}

// ---------------------------------------------------------------------------
// Dialogs
// ---------------------------------------------------------------------------

function closeDialog() {
  const d = document.getElementById("dialog");
  d.hidden = true;
  d.textContent = "";
  if (state.pending) render({ keepScroll: true });
}

/** openDialog({title, sub, fields: [nodes], confirm, danger, onSubmit(form)}) */
function openDialog(o) {
  const d = document.getElementById("dialog");
  const err = h("p", { class: "b-dialog-error", hidden: true });
  const form = h(
    "form",
    { class: "b-dialog", role: "dialog", "aria-modal": "true", "aria-label": o.title, novalidate: true },
    h("h2", null, o.title),
    o.sub ? h("p", { class: "b-dialog-sub" }, o.sub) : null,
    ...(o.fields || []),
    err,
    h(
      "div",
      { class: "b-dialog-actions" },
      h("button", { type: "button", class: "b-btn", on: { click: closeDialog } }, "Cancel"),
      h("button", { type: "submit", class: "b-btn " + (o.danger ? "b-btn-danger" : "b-btn-primary") }, o.confirm || "OK")
    )
  );
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    busy(form, async () => {
      try {
        await o.onSubmit(form);
        closeDialog();
      } catch (e2) {
        err.textContent = e2.message;
        err.hidden = false;
      }
    });
  });
  d.textContent = "";
  d.appendChild(form);
  d.hidden = false;
  const first = form.querySelector("textarea, input, select") || form.querySelector("button[type=submit]");
  if (first) first.focus();
}

document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && !document.getElementById("dialog").hidden) closeDialog();
});
document.getElementById("dialog").addEventListener("click", (e) => {
  if (e.target.id === "dialog") closeDialog();
});

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

function parseRoute() {
  const raw = location.hash.replace(/^#/, "") || "/";
  const [pathPart, qs] = raw.split("?");
  let parts;
  try {
    parts = pathPart.split("/").filter(Boolean).map(decodeURIComponent);
  } catch {
    return { view: "missing", q: new URLSearchParams() };
  }
  const q = new URLSearchParams(qs || "");
  if (!parts.length) return { view: "home", q };
  switch (parts[0]) {
    case "c":
      if (parts[1]) return { view: "context", ctx: parts[1], q };
      break;
    case "p":
      if (parts.length >= 3) return { view: "project", path: parts[1] + "/" + parts.slice(2).join("/"), q };
      break;
    case "t":
      if (parts[1]) return { view: "task", id: parts[1].replace(/^#/, ""), q };
      break;
    case "notes":
      return { view: "notes", q };
    case "log":
      return { view: "log", q };
  }
  return { view: "missing", q };
}

// setQuery rewrites the current route's query (filters, tab) without a new
// history entry, then re-renders.
function setQuery(updates) {
  const raw = location.hash.replace(/^#/, "") || "/";
  const [pathPart, qs] = raw.split("?");
  const q = new URLSearchParams(qs || "");
  for (const [k, v] of Object.entries(updates)) {
    if (v == null || v === "" || v === false) q.delete(k);
    else q.set(k, String(v));
  }
  const s = q.toString();
  history.replaceState(null, "", "#" + pathPart + (s ? "?" + s : ""));
  render({ force: true });
}

function setNav(view) {
  for (const a of document.querySelectorAll("[data-nav]")) {
    if (a.dataset.nav === view) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
}

function hasDirtyForm() {
  const v = document.getElementById("view");
  if (!document.getElementById("dialog").hidden) return true;
  if (v.querySelector("form[data-dirty]")) return true;
  for (const c of v.querySelectorAll("[data-dkey]")) if (state.drafts.get(c.dataset.dkey)) return true;
  const a = document.activeElement;
  return !!(a && v.contains(a) && /^(INPUT|TEXTAREA|SELECT)$/.test(a.tagName) && a.closest("form"));
}

async function render(opts = {}) {
  const view = document.getElementById("view");
  if (!opts.force && hasDirtyForm()) {
    // Never pull the page out from under a half-typed edit; catch up afterwards.
    state.pending = true;
    return;
  }
  state.pending = false;
  const route = parseRoute();
  setNav(route.view);
  const seq = ++state.renderSeq;
  const scroll = opts.keepScroll ? window.scrollY : 0;
  let node;
  try {
    node = await VIEWS[route.view](route);
  } catch (err) {
    node = h(
      "div",
      { class: "b-error" },
      h("h1", null, err.token === "not_found" ? "Not found" : "Could not load this view"),
      h("p", null, err.message)
    );
  }
  if (seq !== state.renderSeq) return;
  const a = document.activeElement;
  const focus = a && view.contains(a) && a.dataset && a.dataset.dkey ? { key: a.dataset.dkey, start: a.selectionStart, end: a.selectionEnd } : null;
  view.replaceChildren(node);
  if (focus) {
    const c = Array.from(view.querySelectorAll("[data-dkey]")).find((x) => x.dataset.dkey === focus.key);
    if (c) {
      c.focus({ preventScroll: true });
      try {
        c.setSelectionRange(focus.start, focus.end);
      } catch {
        /* not a text control */
      }
    }
  }
  window.scrollTo(0, scroll);
  if (!opts.keepScroll && !focus) view.focus({ preventScroll: true });
}

window.addEventListener("hashchange", () => render({ force: true }));
document.addEventListener("focusout", () => {
  setTimeout(() => {
    if (state.pending && !hasDirtyForm()) render({ keepScroll: true });
  }, 0);
});

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

const VIEWS = {
  home: viewHome,
  context: viewContext,
  project: viewProject,
  task: viewTask,
  notes: viewNotes,
  log: viewLog,
  missing: async () => h("div", { class: "b-error" }, h("h1", null, "Not found"), h("p", null, "No view at this address.")),
};

async function loadHome() {
  const home = await get("contexts");
  state.viewer = home.viewer || "";
  state.start = home.start || "";
  state.contexts = home.contexts || [];
  if (home.cursor && !state.cursor) state.cursor = home.cursor;
  const v = document.getElementById("viewer");
  v.textContent = "";
  append(v, ["editing as ", h("strong", null, state.viewer)]);
  return home;
}

function countsLine(c) {
  const n = (k) => (c && c[k]) || 0;
  return h(
    "div",
    { class: "b-counts" },
    h("span", { class: "b-count", data: { status: "todo" } }, h("strong", null, String(n("todo"))), " todo"),
    h("span", { class: "b-count", data: { status: "in-progress" } }, h("strong", null, String(n("in-progress"))), " in progress"),
    h("span", { class: "b-count", data: { status: "done" } }, h("strong", null, String(n("done"))), " done")
  );
}

function header(title, sub, ...actions) {
  return h("div", { class: "b-head" }, h("div", { class: "b-head-text" }, h("h1", null, title), sub || null), actions.length ? h("div", { class: "b-head-actions" }, ...actions) : null);
}

function crumbs(...items) {
  const nav = h("nav", { class: "b-crumbs", "aria-label": "Breadcrumb" });
  items.forEach((it, i) => {
    if (i) nav.appendChild(h("span", { class: "b-sep", "aria-hidden": "true" }, "›"));
    nav.appendChild(it.href ? h("a", { href: it.href }, it.label) : h("span", null, it.label));
  });
  return nav;
}

async function viewHome(route) {
  const home = await loadHome();
  const showArchived = route.q.get("archived") === "1";
  const ctxs = home.contexts.filter((c) => showArchived || !c.archived);
  const archivedCount = home.contexts.filter((c) => c.archived).length;
  const grid = h("div", { class: "b-grid" });
  for (const c of ctxs) {
    grid.appendChild(
      h(
        "a",
        { class: "b-ctx" + (c.archived ? " is-archived" : ""), href: hrefCtx(c.name) },
        h("div", { class: "b-ctx-head" }, h("strong", null, c.title || c.name), c.name === state.start ? h("span", { class: "b-pill" }, "current") : null, c.archived ? h("span", { class: "b-pill" }, "archived") : null),
        h("div", { class: "b-mono b-muted" }, c.name),
        h("div", { class: "b-muted" }, `${c.projects || 0} project${c.projects === 1 ? "" : "s"}`),
        countsLine(c.counts)
      )
    );
  }
  if (!ctxs.length) grid.appendChild(empty("No contexts yet. Create one with `agtrack ctx add <name>`."));
  const toggle = archivedCount
    ? h("label", { class: "b-toggle" }, h("input", { type: "checkbox", checked: showArchived, on: { change: (e) => setQuery({ archived: e.target.checked ? "1" : "" }) } }), ` Show archived (${archivedCount})`)
    : null;
  return h(
    "div",
    { class: "b-page" },
    header("Contexts", h("p", { class: "b-sub" }, "Everything in the store, as agents see it."), toggle),
    grid,
    h(
      "div",
      { class: "b-cols" },
      card("Open handoffs and blockers", h("a", { href: "#/notes?kind=handoff,blocker", class: "b-more" }, "All open notes"), noteList(home.notes, { where: true })),
      card("Recent activity", h("a", { href: "#/log", class: "b-more" }, "Worklog"), eventList(home.log))
    )
  );
}

async function viewContext(route) {
  if (!state.viewer) await loadHome();
  const data = await get("contexts/" + enc(route.ctx));
  const c = data.context;
  const showArchived = route.q.get("archived") === "1";
  const projects = data.projects || [];
  const byParent = new Map();
  for (const p of projects) {
    const k = p.parent_id || 0;
    if (!byParent.has(k)) byParent.set(k, []);
    byParent.get(k).push(p);
  }
  const tree = h("ul", { class: "b-tree" });
  const addLevel = (parentId, into) => {
    for (const p of byParent.get(parentId) || []) {
      if (p.archived && !showArchived) continue;
      const path = c.name + "/" + p.ref;
      const li = h(
        "li",
        { class: p.archived ? "is-archived" : "" },
        h(
          "div",
          { class: "b-tree-row" },
          h("a", { href: hrefProject(path), class: "b-tree-name" }, h("strong", null, p.ref), p.title ? h("span", { class: "b-muted" }, " — " + p.title) : null),
          h("span", { class: "b-tree-counts", title: "open / all tasks" }, `${p.open || 0} open · ${p.tasks || 0} tasks`),
          tagChips(p.tags),
          p.repos ? h("span", { class: "b-mono b-muted b-small" }, p.repos) : null,
          p.archived ? h("span", { class: "b-pill" }, "archived") : null,
          copyBtn(path, "")
        )
      );
      const sub = h("ul");
      addLevel(p.id, sub);
      if (sub.childNodes.length) li.appendChild(sub);
      into.appendChild(li);
    }
  };
  addLevel(0, tree);
  const archivedCount = projects.filter((p) => p.archived).length;
  return h(
    "div",
    { class: "b-page" },
    crumbs({ label: "Contexts", href: "#/" }, { label: c.name }),
    header(c.title || c.name, h("p", { class: "b-sub" }, countsLine(c.counts)), copyBtn(c.name)),
    card(
      "Projects",
      archivedCount
        ? h("label", { class: "b-toggle" }, h("input", { type: "checkbox", checked: showArchived, on: { change: (e) => setQuery({ archived: e.target.checked ? "1" : "" }) } }), ` Show archived (${archivedCount})`)
        : null,
      tree.childNodes.length ? tree : empty("No projects in this context.")
    ),
    h(
      "div",
      { class: "b-cols" },
      card("Specs", null, specTable(data.specs || [])),
      card("Context notes", h("a", { class: "b-more", href: "#/notes?context=" + enc(c.name) }, "Open notes in " + c.name), noteThreads(c.note_list || [], { readOnly: true }))
    )
  );
}

function specTable(specs) {
  if (!specs.length) return empty("No specs.");
  const rows = specs.map((s) => {
    const appr =
      s.approval === "approved"
        ? h("span", { class: "b-pill is-ok" }, "approved v" + s.approval_revision)
        : s.approval === "stale"
          ? h("span", { class: "b-pill is-warn", title: "approved an older revision" }, "stale (v" + s.approval_revision + ")")
          : h("span", { class: "b-pill" }, "not approved");
    return h(
      "tr",
      null,
      h("td", null, h("a", { href: hrefSpec(s.qualified_ref), class: "b-ref" }, icon("doc"), s.slug), h("div", { class: "b-muted b-small" }, s.title || "")),
      h("td", { class: "b-mono b-small" }, s.source_path || ""),
      h("td", null, "v" + s.revision),
      h("td", null, appr),
      h("td", { title: "open notes" }, String(s.open_notes || 0)),
      h("td", { title: "comments awaiting release" }, String(s.pending_comments || 0)),
      h("td", null, copyBtn(s.qualified_ref, ""))
    );
  });
  return h(
    "table",
    { class: "b-table" },
    h("thead", null, h("tr", null, h("th", null, "Spec"), h("th", null, "Source"), h("th", null, "Head"), h("th", null, "Approval"), h("th", null, "Open notes"), h("th", null, "Pending"), h("th", null, ""))),
    h("tbody", null, rows)
  );
}

// --- project ------------------------------------------------------------------------

const TABS = [
  ["tasks", "Tasks"],
  ["specs", "Specs"],
  ["notes", "Notes"],
  ["prs", "PRs"],
  ["log", "Worklog"],
];

async function viewProject(route) {
  if (!state.viewer) await loadHome();
  const data = await get("projects/" + enc(route.path));
  const p = data.project;
  const tab = route.q.get("tab") || "tasks";
  const tabs = h(
    "div",
    { class: "b-tabs", role: "tablist" },
    TABS.map(([k, label]) =>
      h("a", { role: "tab", href: hrefProject(data.path, k === "tasks" ? "" : k), "aria-selected": String(k === tab), class: k === tab ? "is-active" : "" }, label)
    )
  );
  const crumbItems = [{ label: "Contexts", href: "#/" }];
  for (const c of data.parents || []) crumbItems.push({ label: c.name, href: c.kind === "context" ? hrefCtx(c.ref) : hrefProject(c.ref) });
  crumbItems.push({ label: p.name });
  const meta = h(
    "div",
    { class: "b-meta" },
    h("span", { class: "b-mono" }, data.path),
    (p.repos || []).map((r) => h("span", { class: "b-mono b-small b-muted" }, r)),
    tagChips(p.tags || []),
    data.pinned_by && data.pinned_by.length ? h("span", { class: "b-muted b-small", title: "actors whose current project this is" }, "pinned by " + data.pinned_by.join(", ")) : null,
    p.archived ? h("span", { class: "b-pill" }, "archived") : null
  );
  let body;
  switch (tab) {
    case "specs":
      body = card("Specs", null, specTable(await get("projects/" + enc(data.path) + "/specs")));
      break;
    case "notes":
      body = await projectNotesTab(data);
      break;
    case "prs":
      body = await projectPRsTab(data);
      break;
    case "log":
      body = await logPanel(route.q, { project: data.path }, data.path);
      break;
    default:
      body = await projectTasksTab(data, route.q);
  }
  return h("div", { class: "b-page" }, crumbs(...crumbItems), header(p.title || p.name, h("div", null, meta, countsLine(p.counts)), copyBtn(data.path)), tabs, body);
}

async function projectTasksTab(data, q) {
  const params = new URLSearchParams();
  const status = q.get("status") || "open";
  params.set("status", status);
  for (const k of ["tag", "grep", "claimed_by"]) if (q.get(k)) params.set(k, q.get(k));
  const readiness = q.get("readiness") || "";
  if (readiness) params.set(readiness, "1");
  if (q.get("direct") === "1") params.set("direct", "1");
  const res = await get("projects/" + enc(data.path) + "/tasks?" + params.toString());
  const filterForm = h(
    "form",
    { class: "b-filters", on: { submit: (e) => e.preventDefault() } },
    field(
      "Status",
      selectEl("status", [["open", "open"], ["all", "all"], ["todo", "todo"], ["in-progress", "in progress"], ["done", "done"], ["cancelled", "cancelled"], ["closed", "closed"]], status)
    ),
    field("Tag", h("input", { name: "tag", value: q.get("tag") || "", placeholder: "any", size: 10 })),
    field("Claimed by", h("input", { name: "claimed_by", value: q.get("claimed_by") || "", placeholder: "anyone", size: 12 })),
    field("Readiness", selectEl("readiness", [["", "any"], ["ready", "ready"], ["blocked", "blocked"], ["unclaimed", "unclaimed"]], readiness)),
    field("Title contains", h("input", { name: "grep", type: "search", value: q.get("grep") || "", size: 16 })),
    h("label", { class: "b-toggle" }, h("input", { type: "checkbox", name: "direct", checked: q.get("direct") === "1" }), " Only this project")
  );
  filterForm.addEventListener("change", (e) => {
    const f = e.target;
    setQuery({ [f.name]: f.type === "checkbox" ? (f.checked ? "1" : "") : f.value.trim() });
  });
  const rows = (res.tasks || []).map((t) => {
    const expired = t.claim_expires_at && parseTS(t.claim_expires_at) < new Date();
    return h(
      "tr",
      { class: t.blocked ? "is-blocked" : "" },
      h("td", null, h("a", { href: hrefTask(t.id), class: "b-ref" }, "#" + t.id)),
      h("td", { class: "b-title-cell" }, h("a", { href: hrefTask(t.id) }, t.title), t.project && t.project !== data.short ? h("span", { class: "b-muted b-small" }, " · " + t.project) : null),
      h("td", null, statusBadge(t.status), t.blocked ? h("span", { class: "b-pill is-warn", title: "waits on an unmet blocker" }, "blocked") : null),
      h("td", { class: "b-mono b-small", title: "steps done / total" }, t.subtasks || "—"),
      h(
        "td",
        { class: "b-small" },
        t.claimed_by ? h("span", { class: expired ? "b-muted" : "" }, icon("lock"), " ", actorEl(t.claimed_by), " ", h("span", { class: "b-muted", title: fmtTS(t.claim_expires_at) }, t.claim_expires_at ? (expired ? "expired" : "until " + fmtTS(t.claim_expires_at).slice(11)) : "")) : null
      ),
      h("td", null, tagChips(t.tags)),
      h("td", { class: "b-small b-muted" }, when(t.created_at))
    );
  });
  const table = rows.length
    ? h(
        "table",
        { class: "b-table" },
        h("thead", null, h("tr", null, h("th", null, "ID"), h("th", null, "Title"), h("th", null, "Status"), h("th", null, "Steps"), h("th", null, "Claim"), h("th", null, "Tags"), h("th", null, "Age"))),
        h("tbody", null, rows)
      )
    : empty("No tasks match these filters.");
  return h("div", null, filterForm, card(`Tasks (${rows.length})`, newTaskButton(data), table));
}

function newTaskButton(data) {
  return h(
    "button",
    {
      type: "button",
      class: "b-btn b-btn-primary",
      on: {
        click: () => {
          const idem = newIdem();
          openDialog({
            title: "New task in " + data.path,
            fields: [
              field("Title", h("input", { name: "title", required: true, autocomplete: "off" })),
              field("Tags (comma separated)", h("input", { name: "tags", autocomplete: "off" })),
              field("First note (optional)", h("textarea", { name: "note", rows: 3 })),
              field("Steps (one per line)", h("textarea", { name: "steps", rows: 4 })),
            ],
            confirm: "Create task",
            onSubmit: async (form) => {
              const title = form.elements.title.value.trim();
              if (!title) throw new Error("A task needs a title.");
              const res = await post("projects/" + enc(data.path) + "/tasks", {
                title,
                tags: splitList(form.elements.tags.value, ","),
                note: form.elements.note.value.trim(),
                steps: splitList(form.elements.steps.value, "\n"),
                idem,
              });
              showToast("Created #" + res.id);
              location.hash = hrefTask(res.id);
            },
          });
        },
      },
    },
    icon("plus"),
    "New task"
  );
}

function splitList(s, sep) {
  return String(s || "")
    .split(sep)
    .map((x) => x.trim())
    .filter(Boolean);
}

async function projectNotesTab(data) {
  const res = await get("projects/" + enc(data.path) + "/notes");
  return h(
    "div",
    { class: "b-cols" },
    card("Open notes in this project", null, noteList(res.open, { where: true })),
    card("Project notes", null, noteThreads(res.notes || [], {}), addNoteForm("projects/" + enc(data.path) + "/notes"))
  );
}

async function projectPRsTab(data) {
  const prs = await get("projects/" + enc(data.path) + "/prs");
  if (!prs.length) return card("Pull requests", null, empty("No pull requests linked to tasks here."));
  return card(
    "Pull requests",
    null,
    h(
      "table",
      { class: "b-table" },
      h("thead", null, h("tr", null, h("th", null, "State"), h("th", null, "Pull request"), h("th", null, "Task"))),
      h(
        "tbody",
        null,
        prs.map((p) =>
          h(
            "tr",
            null,
            h("td", null, h("span", { class: "b-pill", data: { pr: p.state } }, p.state || "")),
            h("td", null, linkify(p.url), p.title ? h("div", { class: "b-small b-muted" }, p.title) : null),
            h("td", null, taskLink(p.task_id, p.task_title))
          )
        )
      )
    )
  );
}

// --- task -----------------------------------------------------------------------------

async function viewTask(route) {
  if (!state.viewer) await loadHome();
  const data = await get("tasks/" + enc(route.id));
  const t = data.task;
  const id = t.id;
  const base = "tasks/" + id;
  const titleNode = h("h1", { class: "b-task-title" }, t.title);
  const titleBox = h("div", { class: "b-title-box" }, titleNode, h("button", { type: "button", class: "b-btn b-btn-sm", on: { click: () => editTitle(titleBox, base + "/title", t.title, "task") } }, "Edit"));

  if (hasDraft("title:" + base + "/title")) editTitle(titleBox, base + "/title", t.title, "task", true);

  const main = h(
    "div",
    { class: "b-task-main" },
    card(`Steps (${(t.subtask_list || []).filter((s) => s.status === "done").length}/${(t.subtask_list || []).length})`, null, stepList(t.subtask_list || []), addStepsForm(base + "/steps")),
    card("Notes", null, noteThreads(t.note_list || [], {}), addNoteForm(base + "/notes")),
    card("History", null, eventList((t.events || []).slice().reverse(), { noTask: true }))
  );

  const side = h(
    "div",
    { class: "b-task-side" },
    card("Status", null, statusControls(t, base)),
    card("Claim", null, claimBox(t, base)),
    card("Tags", null, tagEditor(t, base)),
    card("Dependencies", null, depsBox(data.deps || {}, base)),
    card("Spec sections", null, specLinks(data.specs || [])),
    card("Artifacts", null, artifactList(data.artifacts || [])),
    card("Pull requests", null, prList(t.pr_list || [])),
    card("Worklog entry", null, logForm(base + "/log"))
  );

  return h(
    "div",
    { class: "b-page" },
    crumbs({ label: "Contexts", href: "#/" }, { label: data.context, href: hrefCtx(data.context) }, { label: t.project, href: hrefProject(data.project_path) }, { label: "#" + id }),
    h(
      "div",
      { class: "b-head" },
      h("div", { class: "b-head-text" }, titleBox, h("div", { class: "b-meta" }, h("span", { class: "b-mono" }, "#" + id), statusBadge(t.status), tagChips(t.tag_list || []))),
      h("div", { class: "b-head-actions" }, copyBtn("#" + id))
    ),
    h("div", { class: "b-task" }, main, side)
  );
}

// editTitle swaps a title for an inline editor. It sends the title the page last
// saw; a 409 means someone changed it meanwhile, and both versions are shown.
function editTitle(box, path, seen, noun, restoring) {
  const input = keep("title:" + path, h("input", { name: "title", value: seen, class: "b-title-input", "aria-label": noun + " title" }));
  const conflict = h("div", { class: "b-conflict", hidden: true });
  const form = trackDirty(
    h(
      "form",
      { class: "b-inline-form" },
      input,
      h("button", { type: "submit", class: "b-btn b-btn-primary b-btn-sm" }, "Save"),
      h(
        "button",
        {
          type: "button",
          class: "b-btn b-btn-sm",
          on: {
            click: () => {
              clearDrafts(form);
              render({ keepScroll: true, force: true });
            },
          },
        },
        "Cancel"
      ),
      conflict
    )
  );
  let expected = seen;
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    busy(form, async () => {
      const title = input.value.trim();
      if (!title) return showToast("A title cannot be empty.", true);
      try {
        await write(path, { title, expected }, "Title saved", form);
      } catch (err) {
        if (err.status !== 409 || !err.payload) return;
        const p = err.payload;
        conflict.textContent = "";
        append(conflict, [
          h("strong", null, "Changed since you loaded the page."),
          h("div", null, "Stored now: ", h("q", null, p.current)),
          h("div", null, "Yours: ", h("q", null, p.yours)),
          h(
            "div",
            { class: "b-conflict-actions" },
            h(
              "button",
              {
                type: "button",
                class: "b-btn b-btn-sm b-btn-primary",
                on: {
                  click: () => {
                    expected = p.current;
                    form.requestSubmit();
                  },
                },
              },
              "Apply mine over it"
            ),
            h(
              "button",
              {
                type: "button",
                class: "b-btn b-btn-sm",
                on: {
                  click: () => {
                    clearDrafts(form);
                    render({ keepScroll: true, force: true });
                  },
                },
              },
              "Keep the stored title"
            )
          ),
        ]);
        conflict.hidden = false;
      }
    });
  });
  box.replaceChildren(form);
  if (!restoring) {
    input.focus();
    input.select();
  }
}

function statusControls(t, base) {
  const note = keep("status:" + base, h("input", { name: "note", placeholder: "Note (optional)", autocomplete: "off" }));
  const form = trackDirty(h("form", { class: "b-status-form", on: { submit: (e) => e.preventDefault() } }));
  const verbs = { todo: "Reopen", "in-progress": "Start", done: "Done", cancelled: "Cancel" };
  const buttons = STATUSES.filter((s) => s !== t.status).map((s) =>
    h(
      "button",
      {
        type: "button",
        class: "b-btn b-btn-sm" + (s === "done" ? " b-btn-primary" : ""),
        on: { click: () => busy(form, () => write(base + "/status", { status: s, note: note.value.trim() }, `#${t.id} ${STATUS_LABEL[s]}`, form).catch(() => {})) },
      },
      verbs[s]
    )
  );
  append(form, [h("div", { class: "b-row" }, statusBadge(t.status)), note, h("div", { class: "b-row" }, buttons)]);
  const times = h(
    "dl",
    { class: "b-dl" },
    h("dt", null, "created"),
    h("dd", null, fmtTS(t.created_at)),
    t.started_at ? [h("dt", null, "started"), h("dd", null, fmtTS(t.started_at))] : null,
    t.closed_at ? [h("dt", null, "closed"), h("dd", null, fmtTS(t.closed_at))] : null,
    h("dt", null, "updated"),
    h("dd", null, fmtTS(t.updated_at))
  );
  return h("div", null, form, times);
}

function claimBox(t, base) {
  if (!t.claimed_by) return empty("Nobody holds a claim. Claims are taken by agents, not from the browser.");
  const expired = t.claim_expires_at && parseTS(t.claim_expires_at) < new Date();
  const until = t.claim_expires_at ? (expired ? "expired " : "until ") + fmtTS(t.claim_expires_at) : "no expiry";
  return h(
    "div",
    null,
    h("p", null, icon("lock"), " ", actorEl(t.claimed_by), " ", h("span", { class: "b-muted" }, until)),
    expired
      ? null
      : h(
          "button",
          {
            type: "button",
            class: "b-btn b-btn-sm b-btn-danger",
            on: {
              click: () =>
                openDialog({
                  title: "Release the claim on #" + t.id + "?",
                  sub: `${t.claimed_by} holds it ${until}. A claim says an agent is working on this task; releasing it lets another agent pick it up.`,
                  confirm: "Release claim",
                  danger: true,
                  onSubmit: async () => {
                    await write(base + "/release", {}, "Claim released");
                  },
                }),
            },
          },
          "Release claim…"
        )
  );
}

function tagEditor(t, base) {
  const chips = (t.tag_list || []).map((tag) =>
    h(
      "span",
      { class: "b-tag" },
      tag,
      h("button", { type: "button", class: "b-tag-x", "aria-label": "Remove tag " + tag, title: "Remove tag", on: { click: () => write(base + "/tags", { remove: [tag] }, "Tag removed").catch(() => {}) } }, "×")
    )
  );
  const input = keep("tags:" + base, h("input", { name: "tags", placeholder: "add tags, comma separated", autocomplete: "off" }));
  const form = trackDirty(h("form", { class: "b-inline-form" }, input, h("button", { type: "submit", class: "b-btn b-btn-sm" }, "Add")));
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    const add = splitList(input.value, ",");
    if (!add.length) return;
    busy(form, () => write(base + "/tags", { add }, "Tags added", form).catch(() => {}));
  });
  return h("div", null, chips.length ? h("div", { class: "b-chips" }, chips) : empty("No tags."), form);
}

function depsBox(deps, base) {
  const waits = (deps.depends_on || []).map((d) =>
    h(
      "li",
      { class: d.unmet ? "is-unmet" : "" },
      h("span", { class: "b-mono b-small" }, d.kind + " "),
      taskLink(d.depends_on_id, d.title),
      " ",
      statusBadge(d.status),
      d.unmet ? h("span", { class: "b-pill is-warn" }, "unmet") : null,
      d.kind === "blocks"
        ? h("button", { type: "button", class: "b-link-btn", title: "Remove this dependency", on: { click: () => write(base + "/unblock", { on: String(d.depends_on_id) }, "Dependency removed").catch(() => {}) } }, "remove")
        : null
    )
  );
  const blocks = (deps.dependents || []).map((d) => h("li", null, taskLink(d.task_id, d.title), " ", statusBadge(d.status)));
  const input = keep("dep:" + base, h("input", { name: "on", placeholder: "task id", inputmode: "numeric", size: 8, autocomplete: "off" }));
  const form = trackDirty(h("form", { class: "b-inline-form" }, h("span", { class: "b-small" }, "Waits on #"), input, h("button", { type: "submit", class: "b-btn b-btn-sm" }, "Add")));
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    const on = input.value.trim().replace(/^#/, "");
    if (!/^\d+$/.test(on)) return showToast("Enter a task id.", true);
    busy(form, () => write(base + "/block", { on }, "Dependency added", form).catch(() => {}));
  });
  return h(
    "div",
    null,
    h("h3", null, "Waits on"),
    waits.length ? h("ul", { class: "b-list" }, waits) : empty("Nothing."),
    form,
    h("h3", null, "Blocks"),
    blocks.length ? h("ul", { class: "b-list" }, blocks) : empty("Nothing waits on this task.")
  );
}

function specLinks(specs) {
  if (!specs.length) return empty("Implements no spec section.");
  return h(
    "ul",
    { class: "b-list" },
    specs.map((s) => {
      const drift = /changed since/.test(s.state || "");
      return h(
        "li",
        null,
        s.spec ? h("a", { href: hrefSpec(s.spec, s.slug), class: "b-ref" }, icon("doc"), s.section) : h("span", null, s.section),
        " ",
        h("span", { class: "b-muted b-small" }, s.kind),
        drift ? h("span", { class: "b-pill is-warn", title: s.state }, "drift") : null,
        s.state && s.state !== "ok" ? h("div", { class: "b-small b-muted" }, s.state) : null,
        copyBtn(s.section, "")
      );
    })
  );
}

function artifactList(arts) {
  if (!arts.length) return empty("None recorded.");
  return h(
    "ul",
    { class: "b-list" },
    arts.map((a) => h("li", null, h("span", { class: "b-pill" }, a.kind), " ", h("span", { class: "b-mono b-small" }, linkify(a.value)), a.label ? h("span", { class: "b-muted b-small" }, " — " + a.label) : null))
  );
}

function prList(prs) {
  if (!prs.length) return empty("None linked.");
  return h(
    "ul",
    { class: "b-list" },
    prs.map((p) => h("li", null, h("span", { class: "b-pill", data: { pr: p.state } }, p.state || ""), " ", linkify(p.url)))
  );
}

function stepList(steps) {
  if (!steps.length) return empty("No steps.");
  return h(
    "ol",
    { class: "b-steps" },
    steps.map((s) => {
      const box = h("span", { class: "b-step-title" + (s.status === "done" || s.status === "cancelled" ? " is-closed" : "") }, s.title);
      const statusSel = selectEl("status", STATUSES.map((x) => [x, STATUS_MARK[x] + " " + STATUS_LABEL[x]]), s.status);
      statusSel.setAttribute("aria-label", "Status of step " + s.ref);
      statusSel.addEventListener("change", () => write("steps/" + enc(s.ref) + "/status", { status: statusSel.value }, s.ref + " " + STATUS_LABEL[statusSel.value]).catch(() => render({ keepScroll: true, force: true })));
      const titleBox = h("span", { class: "b-step-body" }, box);
      const notes = s.note_list || [];
      const notesBox = h("div", { class: "b-step-notes", hidden: !notes.length }, notes.length ? noteThreads(notes, {}) : null);
      if (hasDraft("note:steps/" + enc(s.ref) + "/notes")) openStepNote(notesBox, s, true);
      if (hasDraft("title:steps/" + enc(s.ref) + "/title")) editTitle(titleBox, "steps/" + enc(s.ref) + "/title", s.title, "step", true);
      return h(
        "li",
        { class: "b-step", data: { status: s.status } },
        h("span", { class: "b-mono b-step-ref" }, s.ref),
        statusSel,
        titleBox,
        h("button", { type: "button", class: "b-link-btn", on: { click: () => editTitle(titleBox, "steps/" + enc(s.ref) + "/title", s.title, "step") } }, "rename"),
        h("button", { type: "button", class: "b-link-btn", on: { click: () => openStepNote(notesBox, s) } }, "note"),
        copyBtn(s.ref, ""),
        notesBox
      );
    })
  );
}

// openStepNote reveals the add-note form under a step (once).
function openStepNote(notesBox, s, restoring) {
  if (notesBox.querySelector(".b-step-note-form")) return;
  const form = addNoteForm("steps/" + enc(s.ref) + "/notes");
  form.classList.add("b-step-note-form");
  notesBox.hidden = false;
  notesBox.appendChild(form);
  if (!restoring) form.querySelector("textarea").focus();
}

function addStepsForm(path) {
  const ta = keep("steps:" + path, h("textarea", { name: "steps", rows: 2, placeholder: "New steps, one per line" }));
  let idem = newIdem();
  const form = trackDirty(h("form", { class: "b-add-form" }, ta, h("div", { class: "b-row" }, h("button", { type: "submit", class: "b-btn b-btn-sm" }, "Add steps"))));
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    const steps = splitList(ta.value, "\n");
    if (!steps.length) return;
    busy(form, async () => {
      try {
        await write(path, { steps, idem }, steps.length === 1 ? "Step added" : steps.length + " steps added", form);
        idem = newIdem();
      } catch {
        /* toasted; the same key makes a retry safe */
      }
    });
  });
  return form;
}

function logForm(path) {
  const ta = keep("log:" + path, h("textarea", { name: "body", rows: 2, placeholder: "What happened" }));
  let idem = newIdem();
  const form = trackDirty(h("form", { class: "b-add-form" }, ta, h("div", { class: "b-row" }, h("button", { type: "submit", class: "b-btn b-btn-sm" }, "Add to worklog"))));
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    const body = ta.value.trim();
    if (!body) return;
    busy(form, async () => {
      try {
        await write(path, { body, idem }, "Logged", form);
        idem = newIdem();
      } catch {
        /* toasted */
      }
    });
  });
  return form;
}

// --- notes ----------------------------------------------------------------------------

function addNoteForm(path) {
  const kind = selectEl("kind", NOTE_KINDS, "note");
  kind.setAttribute("aria-label", "Note kind");
  const ta = keep("note:" + path, h("textarea", { name: "body", rows: 3, placeholder: "Add a note: a handoff, a blocker, a question, a decision…" }));
  let idem = newIdem();
  const form = trackDirty(h("form", { class: "b-add-form" }, ta, h("div", { class: "b-row" }, kind, h("button", { type: "submit", class: "b-btn b-btn-sm b-btn-primary" }, "Add note"))));
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    const body = ta.value.trim();
    if (!body) return;
    busy(form, async () => {
      try {
        await write(path, { kind: kind.value, body, idem }, "Note added", form);
        idem = newIdem();
      } catch {
        /* toasted */
      }
    });
  });
  return form;
}

// noteThreads renders notes as threads: roots with their replies, newest root last.
function noteThreads(notes, opts) {
  if (!notes.length) return empty("No notes.");
  const byId = new Map(notes.map((n) => [n.id, n]));
  const replies = new Map();
  const roots = [];
  for (const n of notes) {
    if (n.parent_id && byId.has(n.parent_id)) {
      if (!replies.has(n.parent_id)) replies.set(n.parent_id, []);
      replies.get(n.parent_id).push(n);
    } else roots.push(n);
  }
  return h(
    "div",
    { class: "b-threads" },
    roots.map((n) => {
      const thread = h("div", { class: "b-thread" }, noteItem(n, opts, true), h("div", { class: "b-replies" }, (replies.get(n.id) || []).map((r) => noteItem(r, opts, false))));
      if (!opts.readOnly && hasDraft("reply:" + n.id)) replyForm(thread, n, true);
      return thread;
    })
  );
}

function noteItem(n, opts, isRoot) {
  const resolved = !!n.resolved_at;
  const actions = h("div", { class: "b-note-actions" });
  if (!opts.readOnly) {
    if (isRoot) actions.appendChild(h("button", { type: "button", class: "b-link-btn", on: { click: (e) => replyForm(e.target.closest(".b-thread"), n) } }, "Reply"));
    if (resolved) actions.appendChild(h("button", { type: "button", class: "b-link-btn", on: { click: () => write("notes/" + n.id + "/reopen", {}, "Note reopened").catch(() => {}) } }, "Reopen"));
    else
      actions.appendChild(
        h(
          "button",
          {
            type: "button",
            class: "b-link-btn",
            on: {
              click: () =>
                openDialog({
                  title: "Resolve note " + n.id,
                  sub: String(n.body || "").slice(0, 160),
                  fields: [field("Rationale (optional)", h("textarea", { name: "rationale", rows: 3 }))],
                  confirm: "Resolve",
                  onSubmit: async (form) => {
                    await write("notes/" + n.id + "/resolve", { rationale: form.elements.rationale.value.trim() }, "Note resolved");
                  },
                }),
            },
          },
          "Resolve…"
        )
      );
  }
  actions.appendChild(copyBtn(String(n.id), "#" + n.id));
  return h(
    "article",
    { class: "b-note" + (isMine(n.actor) ? " is-mine" : "") + (resolved ? " is-resolved" : "") + (isRoot ? "" : " is-reply"), data: { kind: n.kind || "note" } },
    h(
      "div",
      { class: "b-note-head" },
      isRoot ? kindBadge(n.kind) : null,
      actorEl(n.actor),
      when(n.created_at),
      resolved ? h("span", { class: "b-pill is-ok", title: "resolved " + fmtTS(n.resolved_at) }, "resolved" + (n.resolved_by ? " by " + n.resolved_by : "")) : isRoot ? h("span", { class: "b-pill" }, "open") : null,
      opts.where ? noteWhere(n) : null
    ),
    h("p", { class: "b-note-body" }, linkify(n.body)),
    n.resolution ? h("p", { class: "b-note-resolution" }, "Rationale: ", linkify(n.resolution)) : null,
    actions
  );
}

function replyForm(thread, n, restoring) {
  if (!thread || thread.querySelector(".b-reply-form")) return;
  const ta = keep("reply:" + n.id, h("textarea", { name: "body", rows: 2, placeholder: "Reply to note " + n.id }));
  const idem = newIdem();
  const form = trackDirty(
    h(
      "form",
      { class: "b-add-form b-reply-form" },
      ta,
      h(
        "div",
        { class: "b-row" },
        h("button", { type: "submit", class: "b-btn b-btn-sm b-btn-primary" }, "Reply"),
        h(
          "button",
          {
            type: "button",
            class: "b-btn b-btn-sm",
            on: {
              click: () => {
                clearDrafts(form);
                render({ keepScroll: true, force: true });
              },
            },
          },
          "Cancel"
        )
      )
    )
  );
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    const body = ta.value.trim();
    if (!body) return;
    busy(form, () => write("notes/" + n.id + "/reply", { body, idem }, "Reply added", form).catch(() => {}));
  });
  thread.appendChild(form);
  if (!restoring) ta.focus();
}

// noteList is a flat list of open notes across targets, each with where it lives.
function noteList(notes, opts) {
  if (!notes || !notes.length) return empty("No open notes.");
  return h("div", { class: "b-threads" }, notes.map((n) => noteItem(n, { readOnly: true, where: opts && opts.where }, true)));
}

async function viewNotes(route) {
  if (!state.viewer) await loadHome();
  const q = route.q;
  const params = new URLSearchParams();
  for (const k of ["context", "project", "kind"]) if (q.get(k)) params.set(k, q.get(k));
  const notes = await get("notes?" + params.toString());
  const kinds = new Set(splitList(q.get("kind") || "", ","));
  const form = h("form", { class: "b-filters", on: { submit: (e) => e.preventDefault() } });
  append(form, [
    field("Context", selectEl("context", [["", "all contexts"], ...state.contexts.map((c) => [c.name, c.name])], q.get("context") || "")),
    field("Project", h("input", { name: "project", value: q.get("project") || "", placeholder: "ctx/project", size: 18 })),
    h(
      "fieldset",
      { class: "b-kinds" },
      h("legend", null, "Kinds"),
      NOTE_KINDS.map((k) => h("label", { class: "b-toggle" }, h("input", { type: "checkbox", name: "kind", value: k, checked: kinds.has(k) }), " " + k))
    ),
  ]);
  form.addEventListener("change", (e) => {
    const f = e.target;
    if (f.name === "kind") {
      const picked = Array.from(form.querySelectorAll("input[name=kind]:checked")).map((x) => x.value);
      setQuery({ kind: picked.join(",") });
    } else setQuery({ [f.name]: f.value.trim() });
  });
  return h("div", { class: "b-page" }, header("Open notes", h("p", { class: "b-sub" }, "Unresolved notes, newest last. Pick no kind to see every kind.")), form, card(`${notes.length} open`, null, noteList(notes, { where: true })));
}

// --- worklog --------------------------------------------------------------------------

function eventList(events, opts) {
  if (!events || !events.length) return empty("Nothing logged yet.");
  return h(
    "ol",
    { class: "b-events" },
    events.map((e) =>
      h(
        "li",
        { class: "b-event" + (isMine(e.actor) ? " is-mine" : "") },
        when(e.at),
        actorEl(e.actor),
        h("span", { class: "b-action", data: { action: e.action } }, e.action),
        e.task_id && !(opts && opts.noTask) ? h("a", { class: "b-ref", href: hrefTask(e.task_id), title: e.task || "" }, e.target || "#" + e.task_id) : h("span", { class: "b-mono b-small" }, e.target || ""),
        h("span", { class: "b-detail" }, linkify(e.detail || "")),
        e.backdated ? h("span", { class: "b-pill", title: "recorded " + fmtTS(e.recorded_at) }, "backdated") : null
      )
    )
  );
}

async function logPanel(q, fixed, projectPath) {
  const params = new URLSearchParams();
  const limit = Math.max(LOG_PAGE, parseInt(q.get("limit") || "", 10) || LOG_PAGE);
  params.set("limit", String(limit));
  for (const k of ["context", "project", "actor", "action", "since", "until"]) {
    const v = (fixed && fixed[k]) || q.get(k);
    if (v) params.set(k, v);
  }
  const res = await get("log?" + params.toString());
  const form = h("form", { class: "b-filters", on: { submit: (e) => e.preventDefault() } });
  if (!fixed || !fixed.project) {
    append(form, [
      field("Context", selectEl("context", [["", "all contexts"], ...state.contexts.map((c) => [c.name, c.name])], q.get("context") || "")),
      field("Project", h("input", { name: "project", value: q.get("project") || "", placeholder: "ctx/project", size: 18 })),
    ]);
  }
  append(form, [
    field("Actor", h("input", { name: "actor", value: q.get("actor") || "", size: 12 })),
    field("Action", h("input", { name: "action", value: q.get("action") || "", placeholder: "status,note,log…", size: 14 })),
    field("Since", h("input", { name: "since", value: q.get("since") || "", placeholder: "2d, today", size: 10 })),
    field("Until", h("input", { name: "until", value: q.get("until") || "", placeholder: "2026-09-01", size: 10 })),
  ]);
  form.addEventListener("change", (e) => setQuery({ [e.target.name]: e.target.value.trim(), limit: "" }));
  const events = res.events || [];
  const more =
    events.length < (res.total || 0)
      ? h("button", { type: "button", class: "b-btn", on: { click: () => setQueryKeepScroll({ limit: String(limit + LOG_PAGE) }) } }, `Load older (${res.total - events.length} more)`)
      : null;
  const parts = [form, card(`Worklog (${events.length} of ${res.total || 0})`, null, eventList(events), more)];
  if (projectPath) parts.push(card("Add a worklog entry to " + projectPath, null, logForm("projects/" + enc(projectPath) + "/log")));
  return h("div", null, parts);
}

function setQueryKeepScroll(updates) {
  const y = window.scrollY;
  const raw = location.hash.replace(/^#/, "") || "/";
  const [pathPart, qs] = raw.split("?");
  const q = new URLSearchParams(qs || "");
  for (const [k, v] of Object.entries(updates)) q.set(k, v);
  history.replaceState(null, "", "#" + pathPart + "?" + q.toString());
  render({ force: true, keepScroll: true }).then(() => window.scrollTo(0, y));
}

async function viewLog(route) {
  if (!state.viewer) await loadHome();
  return h("div", { class: "b-page" }, header("Worklog", h("p", { class: "b-sub" }, "Every event, newest first.")), await logPanel(route.q, null, null));
}

// ---------------------------------------------------------------------------
// Live updates (SSE) and the connection indicator
// ---------------------------------------------------------------------------

function setConn(st, text) {
  const c = document.getElementById("conn");
  c.dataset.state = st;
  c.textContent = text || st;
}

let probeTimer = null;

function connectEvents() {
  const url = "/api/events?after=" + state.cursor + (session.token ? "&t=" + enc(session.token) : "");
  const es = new EventSource(url);
  state.es = es;
  es.addEventListener("open", () => {
    clearTimeout(probeTimer);
    setConn("live");
  });
  es.addEventListener("agtrack", (e) => {
    try {
      const ev = JSON.parse(e.data);
      if (ev.id > state.cursor) state.cursor = ev.id;
    } catch {
      /* the refresh below is what matters */
    }
    clearTimeout(state.sseTimer);
    state.sseTimer = setTimeout(() => render({ keepScroll: true }), SSE_DEBOUNCE_MS);
  });
  es.addEventListener("error", () => {
    if (state.stopped) return;
    setConn("reconnecting");
    clearTimeout(probeTimer);
    // EventSource retries on its own. If the server itself is gone (idle timeout),
    // say so instead of spinning forever.
    probeTimer = setTimeout(probe, 2500);
  });
}

async function probe() {
  try {
    await get("contexts");
  } catch (err) {
    if (err.status === 0 || err.status === 403) {
      state.stopped = true;
      if (state.es) state.es.close();
      setConn("stopped");
      document.getElementById("stopped").hidden = false;
    }
  }
}

// ---------------------------------------------------------------------------
// Boot
// ---------------------------------------------------------------------------

async function main() {
  try {
    await loadHome();
  } catch (err) {
    document.getElementById("view").replaceChildren(h("div", { class: "b-error" }, h("h1", null, "Could not load agtrack"), h("p", null, err.message)));
    setConn("stopped");
    return;
  }
  // Without a route, open on the starting context (-c, else the current one).
  if ((location.hash === "" || location.hash === "#") && state.start) history.replaceState(null, "", hrefCtx(state.start));
  await render({ force: true });
  document.getElementById("app").removeAttribute("aria-busy");
  connectEvents();
}

main();
