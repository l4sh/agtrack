// agtrack spec viewer — vanilla ES module, no build step, no framework.
//
// Security invariants this file must preserve (docs/viewer-api.md):
//   - Note bodies/quotes/authors/diffs are untrusted text: always textContent, never innerHTML.
//   - The only innerHTML assignments are a section's server-rendered `html` field (already
//     sanitised server-side) and Mermaid's own SVG output (rendered from textContent input).
//     Quote highlights are applied afterwards by walking text nodes, never by editing HTML.
//   - The capability token lives in memory only; it is stripped from the address bar on load
//     and sent as a header, never re-embedded in the DOM or in fetch bodies.

const SELECTION_CONTEXT_CHARS = 32;
const SSE_DEBOUNCE_MS = 300;
const WORDS_PER_MINUTE = 200;
const COMFORT_KEY = "agtrack.viewer.comfort";
const SORT_KEY = "agtrack.viewer.sort";
const SVG_NS = "http://www.w3.org/2000/svg";

if (window.mermaid) {
  window.mermaid.initialize({
    startOnLoad: false,
    securityLevel: "strict",
    theme: "redux-color",
    fontFamily: '"Inter Variable", Inter, system-ui, sans-serif',
  });
}

/** @typedef {{token: string, specRef: string}} Session */

// ---------------------------------------------------------------------------
// Boot: resolve the spec ref and token from the URL, then strip the token.
// ---------------------------------------------------------------------------

function bootSession() {
  const path = decodeURIComponent(location.pathname);
  const m = /^\/s\/(.+)$/.exec(path);
  const specRef = m ? m[1] : "";
  const params = new URLSearchParams(location.search);
  const token = params.get("t") || "";
  params.delete("t");
  const qs = params.toString();
  const cleanUrl = location.pathname + (qs ? "?" + qs : "") + location.hash;
  history.replaceState(null, "", cleanUrl);
  return { token, specRef };
}

/** @type {Session} */
const session = bootSession();

// ---------------------------------------------------------------------------
// API client
// ---------------------------------------------------------------------------

function apiPath(p) {
  return "/api/spec/" + encodeURIComponent(session.specRef) + p;
}

// busy runs fn with the form's submit buttons disabled, and ignores a
// second submit while the first request is in flight (double-click safe).
async function busy(form, fn) {
  if (form.dataset.busy === "1") return;
  form.dataset.busy = "1";
  const buttons = Array.from(form.querySelectorAll('button[type="submit"]'));
  for (const b of buttons) b.disabled = true;
  try {
    await fn();
  } finally {
    delete form.dataset.busy;
    for (const b of buttons) b.disabled = false;
  }
}

/**
 * @param {string} path
 * @param {{method?: string, body?: unknown}} [opts]
 */
async function api(path, opts = {}) {
  // After a reload the token is gone from the URL; the session cookie the
  // server set on first load authenticates instead.
  const headers = session.token ? { "X-Agtrack-Token": session.token } : {};
  let body;
  if (opts.body !== undefined) {
    headers["Content-Type"] = "application/json";
    body = JSON.stringify(opts.body);
  }
  let res;
  try {
    res = await fetch(path, { method: opts.method || "GET", headers, body });
  } catch (err) {
    throw new ApiError("network_error", "Could not reach the server.");
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
    throw new ApiError(tok, msg);
  }
  return payload;
}

class ApiError extends Error {
  constructor(token, message) {
    super(message);
    this.token = token;
  }
}

const fetchDoc = () => api(apiPath(""));
const fetchTasks = () => api(apiPath("/tasks"));
function fetchDiff(req) {
  const q = new URLSearchParams();
  if (req && req.from) q.set("from", String(req.from));
  if (req && req.to) q.set("to", String(req.to));
  const qs = q.toString();
  return api(apiPath("/diff" + (qs ? "?" + qs : "")));
}
const postNote = (payload) => api(apiPath("/note"), { method: "POST", body: payload });
const postResolve = (noteId, note) =>
  api(apiPath(`/note/${noteId}/resolve`), { method: "POST", body: { note: note || "" } });
const postRelease = () => api(apiPath("/release"), { method: "POST", body: {} });
const postApprove = (payload) => api(apiPath("/approve"), { method: "POST", body: payload });

// ---------------------------------------------------------------------------
// Small DOM helpers (text only — there is deliberately no HTML-string path)
// ---------------------------------------------------------------------------

/**
 * h("div", {class: "x", title: "y", on: {click: fn}, data: {k: v}}, child, "text", …)
 * Strings become text nodes; null/false children are skipped.
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
  for (const c of children) {
    if (c == null || c === false) continue;
    node.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
  }
  return node;
}

const ICONS = {
  check: "M5 13l4 4L19 7",
  arrow: "M14 5l7 7m0 0-7 7m7-7H3",
  bubble: "M20 2H4a2 2 0 0 0-2 2v18l4-4h14a2 2 0 0 0 2-2V4a2 2 0 0 0-2-2z",
  warn: "M12 9v2m0 4h.01M5.07 19h13.86c1.54 0 2.5-1.67 1.73-3L13.73 4a2 2 0 0 0-3.46 0L3.34 16c-.77 1.33.19 3 1.73 3z",
  copy: "M8 16H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h8a2 2 0 0 1 2 2v2m-6 12h8a2 2 0 0 0 2-2v-8a2 2 0 0 0-2-2h-8a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2z",
};

function icon(name, cls) {
  const svg = document.createElementNS(SVG_NS, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("aria-hidden", "true");
  if (cls) svg.setAttribute("class", cls);
  const path = document.createElementNS(SVG_NS, "path");
  path.setAttribute("d", ICONS[name]);
  svg.appendChild(path);
  return svg;
}

function storageGet(key) {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function storageSet(key, value) {
  try {
    localStorage.setItem(key, value);
  } catch {
    /* private mode / blocked storage: the preference just does not persist */
  }
}

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

const state = {
  doc: null, // last GET /api/spec/{spec} payload
  tasks: null, // last GET /api/spec/{spec}/tasks payload
  diff: null, // last GET /api/spec/{spec}/diff payload (loaded on demand)
  diffReq: null, // {from, to} chosen in a picker; null = server defaults
  view: "document", // "document" | "diff"
  activeTab: "notes", // "notes" | "tasks" | "build" | "diffs"
  filter: "all", // notes filter
  sort: storageGet(SORT_KEY) === "oldest" ? "oldest" : "newest",
  selectedThread: null, // root note id the composer replies to
  activeSection: null, // slug of the section in view
  openReplyFor: new Set(), // root note ids with an open reply box
  openResolveFor: new Set(), // note ids with an open resolve box
  drafts: new Map(), // "reply:ID" / "resolve:ID" → unsent text, kept across re-renders
  mermaidCache: new Map(), // mermaid source → svg markup produced by mermaid itself
  es: null,
  sseTimer: null,
};

// ---------------------------------------------------------------------------
// DOM refs
// ---------------------------------------------------------------------------

const $ = (id) => document.getElementById(id);
const el = {
  app: $("app"),
  specTitle: $("spec-title"),
  specRef: $("spec-ref"),
  specRev: $("spec-rev"),
  specApproval: $("spec-approval"),
  presence: $("presence"),
  btnSend: $("btn-send"),
  btnSendCount: $("btn-send-count"),
  btnApprove: $("btn-approve"),
  btnComfort: $("btn-comfort"),
  btnOutline: $("btn-outline"),
  btnRail: $("btn-rail"),
  btnRailCount: $("btn-rail-count"),
  btnRailClose: $("btn-rail-close"),
  scrim: $("scrim"),
  outline: $("outline"),
  outlineCount: $("outline-count"),
  toc: $("toc"),
  tocFilter: $("toc-filter"),
  metricRead: $("metric-read"),
  metricMeter: $("metric-meter"),
  metricThreads: $("metric-threads"),
  metricResolved: $("metric-resolved"),
  canvas: $("canvas"),
  sheetEyebrow: $("sheet-eyebrow"),
  sheetTitle: $("sheet-title"),
  diffBanner: $("diff-banner"),
  doc: $("doc"),
  rail: $("rail"),
  notesScroll: $("notes-scroll"),
  notesList: $("notes-list"),
  orphanedGroup: $("orphaned-group"),
  orphanedList: $("orphaned-list"),
  btnSort: $("btn-sort"),
  composer: $("composer"),
  composerTarget: $("composer-target-text"),
  composerClear: $("composer-clear"),
  composerKind: $("composer-kind"),
  composerBody: $("composer-body"),
  buildNotesList: $("build-notes-list"),
  tasksLinks: $("tasks-links"),
  tasksGaps: $("tasks-gaps"),
  tasksDrift: $("tasks-drift"),
  diffsList: $("diffs-list"),
  revPickerRail: $("rev-picker-rail"),
  btnOpenDiffView: $("btn-open-diffview"),
  commentFab: $("comment-fab"),
  commentDialog: $("comment-dialog"),
  commentForm: $("comment-form"),
  commentQuote: $("comment-quote"),
  commentKind: $("comment-kind"),
  commentBody: $("comment-body"),
  commentError: $("comment-error"),
  approveDialog: $("approve-dialog"),
  approveForm: $("approve-form"),
  approveSub: $("approve-sub"),
  approveNote: $("approve-note"),
  approveBuildNotes: $("approve-build-notes"),
  approveBuildNoteAdd: $("approve-build-note-add"),
  approveError: $("approve-error"),
  toast: $("toast"),
};

// ---------------------------------------------------------------------------
// Toast
// ---------------------------------------------------------------------------

let toastTimer = null;
function showToast(message, isError) {
  el.toast.textContent = message;
  el.toast.classList.toggle("is-error", !!isError);
  el.toast.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => {
    el.toast.hidden = true;
  }, 4000);
}

// ---------------------------------------------------------------------------
// Derived data
// ---------------------------------------------------------------------------

function viewerActor() {
  return (state.doc && state.doc.viewer && state.doc.viewer.actor) || "";
}

function isViewer(actor) {
  const v = viewerActor();
  return !!v && actor === v;
}

function buildThreads(notes) {
  const byParent = new Map();
  const roots = [];
  for (const n of notes) {
    if (n.parent_id == null) {
      roots.push(n);
    } else {
      if (!byParent.has(n.parent_id)) byParent.set(n.parent_id, []);
      byParent.get(n.parent_id).push(n);
    }
  }
  const replyOf = (id) => (byParent.get(id) || []).slice().sort((a, b) => a.id - b.id);
  return roots.map((root) => ({ root, replies: replyOf(root.id) }));
}

function threadOpen(thread) {
  if (!thread.root.resolved_at) return true;
  return thread.replies.some((r) => !r.resolved_at);
}

function threadLatest(thread) {
  let t = thread.root.created_at || "";
  for (const r of thread.replies) if ((r.created_at || "") > t) t = r.created_at;
  return t;
}

function allThreads(doc) {
  return buildThreads(doc.notes || []);
}

function sectionLabel(section) {
  if (!section) return "";
  if (section.kind === "root") return "Introduction";
  return (section.number ? section.number + " " : "") + (section.heading || section.path);
}

function sectionsBySlug(doc) {
  return new Map(doc.sections.map((s) => [s.slug, s]));
}

/**
 * Nesting depth per section slug: the number of enclosing headings, so "## Items" under no
 * "#" is top-level and a "####" directly under a "##" is one level in, not two.
 */
function depthMap(doc) {
  const out = new Map();
  const stack = [];
  for (const s of doc.sections) {
    if (s.kind === "root") {
      out.set(s.slug, 0);
      continue;
    }
    while (stack.length && stack[stack.length - 1] >= s.level) stack.pop();
    out.set(s.slug, stack.length);
    stack.push(s.level);
  }
  return out;
}

function kindBadge(note) {
  if (note.kind === "note" && !isViewer(note.actor)) return { label: "AI note", cls: "badge-note" };
  const map = {
    decision: "badge-decision",
    question: "badge-question",
    note: "badge-note",
    blocker: "badge-blocker",
    "build-note": "badge-build",
  };
  return { label: note.kind, cls: map[note.kind] || "badge-note" };
}

function formatTime(iso) {
  if (!iso) return "";
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  const now = new Date();
  const sameDay = d.toDateString() === now.toDateString();
  return sameDay
    ? d.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })
    : d.toLocaleString([], { year: "numeric", month: "numeric", day: "numeric", hour: "numeric", minute: "2-digit" });
}

function initials(name) {
  const parts = String(name).split(/[^A-Za-z0-9]+/).filter(Boolean);
  if (parts.length === 0) return "?";
  if (parts.length === 1) return parts[0][0].toUpperCase();
  return (parts[0][0] + parts[1][0]).toUpperCase();
}

// ---------------------------------------------------------------------------
// Header
// ---------------------------------------------------------------------------

function renderHeader(doc) {
  const spec = doc.spec;
  const title = spec.title || spec.slug;
  el.specTitle.textContent = title;
  el.specTitle.title = title;
  document.title = title + " — agtrack review";
  el.specRef.textContent = spec.ref;
  el.specRev.textContent = `rev ${spec.revision} • ${spec.release_mode}`;
  el.specRev.title =
    spec.release_mode === "manual"
      ? "Manual release: comments reach the agent when you press Send to agent"
      : "Auto release: comments reach the agent as you post them";

  const ap = el.specApproval;
  ap.textContent = "";
  ap.className = "chip";
  if (spec.approval) {
    const a = spec.approval;
    const stale = !!spec.approval_stale;
    ap.classList.add(stale ? "chip-amber" : "chip-emerald");
    ap.appendChild(document.createTextNode(stale ? `approved r${a.revision} — stale` : `approved r${a.revision}`));
    ap.title =
      `Approved by ${a.by} · ${formatTime(a.at)}` +
      (a.note ? `\n“${a.note}”` : "") +
      (stale ? `\nThe spec changed since (now r${spec.revision}).` : "");
  } else {
    ap.classList.add("chip-rose");
    ap.appendChild(h("span", { class: "pulse-dot", "aria-hidden": "true" }));
    ap.appendChild(document.createTextNode("not approved"));
    ap.title = "Not approved yet";
  }

  if (spec.release_mode === "manual") {
    el.btnSend.hidden = false;
    el.btnSendCount.textContent = String(spec.pending_comments || 0);
    el.btnSend.disabled = !spec.pending_comments;
    el.btnSend.title = spec.pending_comments
      ? `Release ${spec.pending_comments} pending comment(s) to the agent`
      : "No pending comments";
  } else {
    el.btnSend.hidden = true;
  }

  renderPresence(doc);
}

function renderPresence(doc) {
  const viewer = viewerActor();
  const actors = [];
  const seen = new Set();
  const add = (a) => {
    if (a && !seen.has(a)) {
      seen.add(a);
      actors.push(a);
    }
  };
  add(viewer);
  for (const n of doc.notes || []) add(n.actor);
  el.presence.textContent = "";
  const MAX = 4;
  for (const a of actors.slice(0, MAX)) {
    const mine = a === viewer;
    const role = mine ? "reviewer" : "agent";
    el.presence.appendChild(
      h(
        "span",
        { class: "avatar " + (mine ? "avatar-reviewer" : "avatar-agent"), title: `${a} (${role})`, data: { tip: `${a} (${role})` } },
        mine ? initials(a) : "AI",
        h("span", { class: "avatar-dot", "aria-hidden": "true" })
      )
    );
  }
  if (actors.length > MAX) {
    const rest = actors.slice(MAX);
    el.presence.appendChild(h("span", { class: "avatar avatar-more", title: rest.join(", ") }, "+" + rest.length));
  }
}

// ---------------------------------------------------------------------------
// Outline (left rail)
// ---------------------------------------------------------------------------

function renderOutline(doc) {
  const depths = depthMap(doc);
  const threads = allThreads(doc);
  const resolvedBySection = new Map();
  for (const t of threads) {
    if (!threadOpen(t) && t.root.section) {
      resolvedBySection.set(t.root.section, (resolvedBySection.get(t.root.section) || 0) + 1);
    }
  }
  const visible = doc.sections.filter((s) => s.kind !== "root" || (s.html || "").trim());
  const headed = doc.sections.filter((s) => s.kind !== "root");
  el.outlineCount.textContent = `${headed.length} section${headed.length === 1 ? "" : "s"}`;

  el.toc.textContent = "";
  for (const s of visible) {
    const depth = depths.get(s.slug) || 0;
    const badges = h("span", { class: "toc-badges" });
    if (s.open_notes > 0) badges.appendChild(h("span", { class: "toc-open", title: `${s.open_notes} open note(s)` }, `${s.open_notes} open`));
    const resolved = resolvedBySection.get(s.slug) || 0;
    if (resolved > 0) badges.appendChild(h("span", { class: "toc-dot", title: `${resolved} resolved thread(s)` }));
    if (s.tasks && s.tasks.length) badges.appendChild(h("span", { class: "toc-tasks", title: `${s.tasks.length} linked task(s)` }, String(s.tasks.length)));
    if (s.kind === "root") badges.appendChild(h("span", { class: "toc-meta" }, "Intro"));

    const label = h("span", { class: "toc-label" });
    if (s.number) label.appendChild(h("span", { class: "toc-num" }, s.number));
    label.appendChild(document.createTextNode(s.kind === "root" ? "Introduction" : s.heading));

    const a = h(
      "a",
      {
        class: `toc-item depth-${Math.min(depth, 3)}`,
        href: "#section-" + s.slug,
        data: { slug: s.slug, search: (sectionLabel(s) + " " + s.slug + " " + (s.pin || "")).toLowerCase() },
        on: {
          click: (ev) => {
            ev.preventDefault();
            jumpToSection(s.slug);
            closeDrawers();
          },
        },
      },
      label,
      badges
    );
    el.toc.appendChild(a);
  }
  applyTocFilter();
  markActiveToc();

  // Metrics: read time from the rendered text, thread counts from the notes.
  const words = (el.doc.textContent || "").split(/\s+/).filter(Boolean).length;
  const mins = Math.max(1, Math.round(words / WORDS_PER_MINUTE));
  el.metricRead.textContent = `~${mins} min (${words} words)`;
  const open = threads.filter(threadOpen).length;
  const resolved = threads.length - open;
  el.metricThreads.textContent = `${open} open · ${resolved} resolved`;
  const pct = threads.length ? Math.round((resolved / threads.length) * 100) : 0;
  el.metricResolved.textContent = threads.length ? `${pct}% resolved` : "no threads";
  el.metricMeter.style.width = pct + "%";
}

function applyTocFilter() {
  const q = el.tocFilter.value.trim().toLowerCase();
  for (const a of el.toc.querySelectorAll(".toc-item")) {
    a.hidden = !!q && !a.dataset.search.includes(q);
  }
}

el.tocFilter.addEventListener("input", applyTocFilter);
el.tocFilter.addEventListener("keydown", (ev) => {
  if (ev.key === "Enter") {
    ev.preventDefault();
    const first = Array.from(el.toc.querySelectorAll(".toc-item")).find((a) => !a.hidden);
    if (first) {
      jumpToSection(first.dataset.slug);
      el.tocFilter.value = "";
      applyTocFilter();
      el.tocFilter.blur();
      closeDrawers();
    }
  } else if (ev.key === "Escape") {
    el.tocFilter.value = "";
    applyTocFilter();
    el.tocFilter.blur();
  }
});

function markActiveToc() {
  for (const a of el.toc.querySelectorAll(".toc-item")) {
    const on = a.dataset.slug === state.activeSection;
    a.classList.toggle("is-active", on);
    if (on) a.setAttribute("aria-current", "location");
    else a.removeAttribute("aria-current");
  }
}

// Active-section tracking: the last section whose top has scrolled past a
// line a little below the canvas top.
let scrollRaf = 0;
function trackActiveSection() {
  scrollRaf = 0;
  const sections = el.doc.querySelectorAll(".section");
  if (!sections.length) return;
  const line = el.canvas.getBoundingClientRect().top + 96;
  let current = sections[0].dataset.section;
  for (const s of sections) {
    if (s.getBoundingClientRect().top <= line) current = s.dataset.section;
    else break;
  }
  // At the very bottom, the last section is the one being read.
  const scrollable = el.canvas.scrollHeight > el.canvas.clientHeight + 4;
  if (scrollable && el.canvas.scrollTop > 0 && el.canvas.scrollTop + el.canvas.clientHeight >= el.canvas.scrollHeight - 4) {
    current = sections[sections.length - 1].dataset.section;
  }
  if (current !== state.activeSection) {
    state.activeSection = current;
    markActiveToc();
    renderComposerTarget();
  }
}

el.canvas.addEventListener("scroll", () => {
  if (!scrollRaf) scrollRaf = requestAnimationFrame(trackActiveSection);
});

// ---------------------------------------------------------------------------
// Document sheet
// ---------------------------------------------------------------------------

function sectionText(sectionEl) {
  const body = sectionEl.querySelector(".section-body");
  return body ? body.textContent : "";
}

function renderSheetHead(doc) {
  const spec = doc.spec;
  const fm = doc.front_matter || {};
  el.sheetTitle.textContent = spec.title || spec.slug;
  el.sheetEyebrow.textContent = "";
  el.sheetEyebrow.appendChild(h("span", { class: "eyebrow-accent" }, "Specification"));
  const bits = [`rev ${spec.revision}`];
  if (typeof fm.status === "string" && fm.status) bits.push(fm.status);
  if (typeof fm.owner === "string" && fm.owner) bits.push("owner " + fm.owner);
  for (const b of bits) {
    el.sheetEyebrow.appendChild(h("span", { class: "eyebrow-sep", "aria-hidden": "true" }, "•"));
    el.sheetEyebrow.appendChild(h("span", null, b));
  }
  if (spec.source_path) {
    el.sheetEyebrow.appendChild(h("span", { class: "eyebrow-sep", "aria-hidden": "true" }, "•"));
    el.sheetEyebrow.appendChild(h("span", { class: "mono" }, spec.source_path));
  }
}

function renderDoc(doc) {
  const depths = depthMap(doc);
  const diffBySlug = new Map();
  if (state.view === "diff" && state.diff) {
    for (const c of state.diff.changes) if (c.change !== "removed" && c.change !== "orphaned") diffBySlug.set(c.slug, c);
  }
  const threadsBySection = new Map();
  for (const t of allThreads(doc)) {
    if (!t.root.section) continue;
    if (!threadsBySection.has(t.root.section)) threadsBySection.set(t.root.section, []);
    threadsBySection.get(t.root.section).push(t);
  }

  el.doc.textContent = "";
  for (const section of doc.sections) {
    if (section.kind === "root" && !(section.html || "").trim()) continue;
    el.doc.appendChild(
      renderSection(section, depths.get(section.slug) || 0, threadsBySection.get(section.slug) || [], diffBySlug.get(section.slug))
    );
  }
  if (state.view === "diff" && state.diff) {
    const gone = state.diff.changes.filter((c) => c.change === "removed" || c.change === "orphaned");
    if (gone.length) el.doc.appendChild(renderRemoved(gone));
  }
  renderDiffBanner();
  renderMermaid();
}

function renderSection(section, depth, threads, change) {
  const isRoot = section.kind === "root";
  const wrap = h("section", {
    class: `section depth-${Math.min(depth, 3)}` + (isRoot ? " section-root" : ""),
    id: "section-" + section.slug,
    data: { section: section.slug, level: section.level },
  });
  if (section.open_notes > 0) wrap.classList.add("has-open-note");

  if (!isRoot) {
    const level = Math.min(Math.max(depth + 2, 2), 6);
    const tag = h(`h${level}`, { class: "section-title" });
    if (section.number) {
      tag.appendChild(h("span", { class: "section-number" }, section.number));
      tag.appendChild(document.createTextNode(" "));
    }
    tag.appendChild(document.createTextNode(section.heading));

    const refs = h("span", { class: "section-refs" });
    refs.appendChild(
      h("button", {
        type: "button",
        class: "ref-chip ref-slug",
        title: "Copy reference " + session.specRef + "#" + section.slug,
        text: "#" + section.slug,
        on: { click: () => copyRef(session.specRef + "#" + section.slug) },
      })
    );
    if (section.pin) {
      refs.appendChild(
        h("button", {
          type: "button",
          class: "ref-chip ref-pin",
          title: "Copy pinned reference " + session.specRef + "@" + section.pin,
          text: "@" + section.pin,
          on: { click: () => copyRef(session.specRef + "@" + section.pin) },
        })
      );
    }

    const right = h("span", { class: "section-aside" });
    if (threads.length) {
      const decisions = threads.filter((t) => t.root.kind === "decision").length;
      const label =
        decisions && decisions === threads.length
          ? `${decisions} decision${decisions === 1 ? "" : "s"}`
          : `${threads.length} thread${threads.length === 1 ? "" : "s"}`;
      right.appendChild(
        h(
          "button",
          { type: "button", class: "link-btn section-threads", on: { click: () => focusThread(threads[0].root.id) } },
          label,
          icon("arrow", "ic-xs")
        )
      );
    }

    const head = h("div", { class: "section-heading" }, h("div", { class: "section-heading-main" }, tag, refs), right);
    wrap.appendChild(head);

    if (section.open_notes > 0) {
      wrap.appendChild(
        h(
          "div",
          { class: "discussion-pill" },
          h("span", { class: "dot", "aria-hidden": "true" }),
          `Active Discussion${section.open_notes > 1 ? ` · ${section.open_notes}` : ""}`
        )
      );
    }
  }

  if (change) wrap.appendChild(renderChangeBox(change));

  const body = h("div", { class: "section-body" + (isRoot ? " lead" : "") });
  // The only innerHTML assignment for document content: server-sanitised markdown output.
  body.innerHTML = section.html || "";
  decorateBody(body);
  applyHighlights(body, threads.filter(threadOpen));
  wrap.appendChild(body);

  if (change) wrap.classList.add("diff-" + change.change);
  return wrap;
}

/** DOM-only touch-ups on the sanitised body: scrollable tables. */
function decorateBody(body) {
  for (const table of body.querySelectorAll("table")) {
    const wrap = h("div", { class: "table-wrap" });
    table.replaceWith(wrap);
    wrap.appendChild(table);
  }
}

// --- quote highlights (text-node walk) ----------------------------------------------

function textNodesOf(root) {
  const out = [];
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, {
    acceptNode: (n) =>
      n.parentElement && n.parentElement.closest("pre.mermaid-src, .diagram")
        ? NodeFilter.FILTER_REJECT
        : NodeFilter.FILTER_ACCEPT,
  });
  for (let n = walker.nextNode(); n; n = walker.nextNode()) out.push(n);
  return out;
}

/** Wraps [start, end) of the concatenated text of `nodes` in <mark>s; returns them. */
function wrapRange(nodes, start, end, makeMark) {
  const marks = [];
  let pos = 0;
  for (const node of nodes) {
    const len = node.data.length;
    const s = Math.max(start, pos);
    const e = Math.min(end, pos + len);
    pos += len;
    if (s >= e) continue;
    let target = node;
    const localStart = s - (pos - len);
    const localEnd = e - (pos - len);
    if (localStart > 0) target = target.splitText(localStart);
    if (localEnd - localStart < target.data.length) target.splitText(localEnd - localStart);
    if (!target.data.trim()) continue;
    const mark = makeMark();
    target.parentNode.insertBefore(mark, target);
    mark.appendChild(target);
    marks.push(mark);
  }
  return marks;
}

function applyHighlights(body, openThreads) {
  const jumpsByBlock = new Map();
  for (const t of openThreads) {
    const quote = (t.root.quote || "").trim();
    if (!quote || t.root.quote_ok === false) continue;
    const nodes = textNodesOf(body);
    const text = nodes.map((n) => n.data).join("");
    let at = text.indexOf(quote);
    let len = quote.length;
    if (at < 0) {
      // Rendered text collapses markdown whitespace; retry whitespace-insensitively.
      const re = new RegExp(quote.split(/\s+/).map(escapeRegExp).join("\\s+"));
      const m = re.exec(text);
      if (!m) continue;
      at = m.index;
      len = m[0].length;
    }
    const tone = t.root.kind === "note" ? "hl-note" : t.root.kind === "blocker" ? "hl-blocker" : "hl-open";
    const marks = wrapRange(nodes, at, at + len, () =>
      h("mark", {
        class: "hl " + tone,
        data: { noteId: t.root.id },
        title: `${t.root.kind} by ${t.root.actor}: ${t.root.body}`,
        on: { click: () => focusThread(t.root.id) },
      })
    );
    if (!marks.length) continue;
    const block = marks[0].closest("p, li, blockquote, td, th, dd, dt, h1, h2, h3, h4, h5, h6") || body;
    if (!jumpsByBlock.has(block)) jumpsByBlock.set(block, []);
    jumpsByBlock.get(block).push({ thread: t, tone });
  }
  for (const [block, items] of jumpsByBlock) {
    const discuss = block !== body && (block.tagName === "P" || block.tagName === "BLOCKQUOTE" || block.tagName === "LI");
    if (discuss) {
      block.classList.add("discuss-block", items.every((i) => i.tone === "hl-note") ? "tone-note" : "tone-open");
      block.style.setProperty("--jumps", String(items.length)); // room for the button stack
    }
    const stack = h("span", { class: "hl-jumps" + (discuss ? "" : " inline") });
    for (const { thread } of items) {
      stack.appendChild(
        h(
          "button",
          {
            type: "button",
            class: "hl-jump",
            title: `Jump to ${thread.root.kind} by ${thread.root.actor}`,
            "aria-label": `Jump to ${thread.root.kind} by ${thread.root.actor}`,
            on: { click: () => focusThread(thread.root.id) },
          },
          icon("bubble")
        )
      );
    }
    block.appendChild(stack);
  }
}

function escapeRegExp(s) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function copyRef(text) {
  navigator.clipboard.writeText(text).then(
    () => showToast("Copied " + text),
    () => showToast("Could not copy to clipboard", true)
  );
}

// --- Mermaid ------------------------------------------------------------------------

let mermaidSeq = 0;
async function renderMermaid() {
  if (!window.mermaid) return;
  const todo = [];
  for (const block of el.doc.querySelectorAll("pre.mermaid-src[data-mermaid]")) {
    const code = block.querySelector("code");
    const source = code ? code.textContent : block.textContent;
    const host = h("div", { class: "diagram" });
    block.insertAdjacentElement("afterend", host);
    block.classList.add("is-rendered");
    const cached = state.mermaidCache.get(source);
    // Cached: Mermaid's own output for this exact source, from an earlier render. Applied
    // synchronously so a refresh does not shift the layout under the reader.
    if (cached) host.innerHTML = cached;
    else todo.push({ block, host, source });
  }
  if (!todo.length) return;
  // Measure labels with the vendored font, not a fallback that swaps in later.
  if (document.fonts && document.fonts.ready) {
    try {
      await document.fonts.ready;
    } catch {
      /* render with whatever is loaded */
    }
  }
  for (const { block, host, source } of todo) {
    const id = "mermaid-" + ++mermaidSeq;
    try {
      const { svg } = await window.mermaid.render(id, source);
      state.mermaidCache.set(source, svg);
      host.innerHTML = svg;
    } catch (err) {
      block.classList.remove("is-rendered");
      host.classList.add("diagram-error");
      host.textContent = "Mermaid render failed: " + (err && err.message ? err.message : String(err));
      const stray = document.getElementById("d" + id);
      if (stray) stray.remove();
    }
  }
}

// --- Diff view ----------------------------------------------------------------------

const CHANGE_LABEL = {
  new: "Added",
  changed: "Changed",
  renamed: "Renamed",
  moved: "Moved",
  removed: "Removed",
  orphaned: "Orphaned",
};

function changeDetail(c) {
  if (c.change === "renamed") {
    const bits = [];
    if (c.prior_heading && c.prior_heading !== c.heading) bits.push(`was “${c.prior_heading}”`);
    if (c.prior_slug && c.prior_slug !== c.slug) bits.push(`was #${c.prior_slug}`);
    return bits.join(" · ");
  }
  if (c.change === "moved") return `moved from ${c.prior_number || "#" + c.prior_slug}`;
  if (c.change === "orphaned") return `@${c.anchor} orphaned in r${c.revision}`;
  return "";
}

function diffStats(diff) {
  let add = 0;
  let del = 0;
  for (const line of String(diff || "").split("\n")) {
    if (line.startsWith("+++") || line.startsWith("---")) continue;
    if (line.startsWith("+")) add++;
    else if (line.startsWith("-")) del++;
  }
  return { add, del };
}

function renderUnifiedDiff(diff) {
  const box = h("div", { class: "udiff", role: "group", "aria-label": "Changes" });
  for (const line of String(diff || "").replace(/\n$/, "").split("\n")) {
    if (line.startsWith("+++") || line.startsWith("---")) continue;
    let cls = "ctx";
    if (line.startsWith("@@")) cls = "hunk";
    else if (line.startsWith("+")) cls = "add";
    else if (line.startsWith("-")) cls = "del";
    box.appendChild(h("div", { class: "udiff-line " + cls }, line || " "));
  }
  return box;
}

function renderChangeBox(c) {
  const box = h(
    "div",
    { class: "change-box change-" + c.change },
    h(
      "div",
      { class: "change-head" },
      h("span", { class: "badge badge-change-" + c.change }, CHANGE_LABEL[c.change] || c.change),
      h("span", { class: "change-detail" }, changeDetail(c) || (c.change === "new" ? `new in r${state.diff.to}` : ""))
    )
  );
  if (c.diff) box.appendChild(renderUnifiedDiff(c.diff));
  return box;
}

function renderRemoved(gone) {
  const wrap = h(
    "section",
    { class: "removed-group" },
    h("h2", { class: "removed-title" }, `No longer present in r${state.diff.to}`)
  );
  for (const c of gone) {
    wrap.appendChild(
      h(
        "div",
        { class: "removed-item" },
        h("span", { class: "badge badge-change-" + c.change }, CHANGE_LABEL[c.change] || c.change),
        h("span", { class: "removed-heading" }, c.heading || c.slug),
        h("span", { class: "mono muted" }, "#" + c.slug),
        changeDetail(c) ? h("span", { class: "muted" }, changeDetail(c)) : null
      )
    );
  }
  return wrap;
}

function renderRevPicker(container) {
  container.textContent = "";
  const d = state.diff;
  if (!d || !d.versions || d.versions.length < 2) {
    container.appendChild(h("span", { class: "muted" }, d ? "Only one revision" : "Loading…"));
    return;
  }
  const mk = (which, value) => {
    const sel = h("select", { "aria-label": which === "from" ? "Compare from revision" : "Compare to revision" });
    for (const v of d.versions) {
      const n = v.n;
      let label = "r" + n;
      if (n === d.head) label += " (head)";
      if (d.approved_revision && n === d.approved_revision) label += " (approved)";
      sel.appendChild(h("option", { value: String(n), selected: n === value }, label));
    }
    sel.addEventListener("change", () => {
      const from = which === "from" ? Number(sel.value) : d.from;
      const to = which === "to" ? Number(sel.value) : d.to;
      state.diffReq = { from, to };
      loadDiff(true);
    });
    return sel;
  };
  container.appendChild(mk("from", d.from));
  container.appendChild(h("span", { class: "muted", "aria-hidden": "true" }, "→"));
  container.appendChild(mk("to", d.to));
}

function renderDiffBanner() {
  const b = el.diffBanner;
  if (state.view !== "diff") {
    b.hidden = true;
    return;
  }
  b.hidden = false;
  b.textContent = "";
  const d = state.diff;
  if (!d) {
    b.appendChild(h("span", { class: "muted" }, "Loading changes…"));
    return;
  }
  const picker = h("span", { class: "rev-picker" });
  renderRevPicker(picker);
  const n = d.changes.length;
  b.appendChild(h("span", { class: "diff-banner-title" }, "Comparing"));
  b.appendChild(picker);
  b.appendChild(
    h("span", { class: "diff-banner-count" }, d.from ? `${n} section change${n === 1 ? "" : "s"}` : "nothing to compare yet")
  );
  if (d.to !== d.head) {
    b.appendChild(h("span", { class: "diff-banner-note" }, `The document below is the head (r${d.head}).`));
  }
}

function renderDiffsRail() {
  const d = state.diff;
  renderRevPicker(el.revPickerRail);
  el.diffsList.textContent = "";
  $("count-diffs").textContent = d && d.changes.length ? String(d.changes.length) : "";
  if (!d) {
    el.diffsList.appendChild(emptyHint("Loading changes…"));
    return;
  }
  if (!d.from) {
    el.diffsList.appendChild(emptyHint("This spec has a single revision; nothing to compare."));
    return;
  }
  if (!d.changes.length) {
    el.diffsList.appendChild(emptyHint(`No section changes between r${d.from} and r${d.to}.`));
    return;
  }
  for (const c of d.changes) {
    const st = diffStats(c.diff);
    const head = h(
      "div",
      { class: "diff-card-head" },
      h("span", { class: "badge badge-change-" + c.change }, CHANGE_LABEL[c.change] || c.change),
      h("span", { class: "diff-card-title" }, (c.number ? c.number + " " : "") + (c.heading || c.slug)),
      c.diff ? h("span", { class: "diff-stats" }, h("span", { class: "add" }, "+" + st.add), h("span", { class: "del" }, "−" + st.del)) : null
    );
    const card = h("div", { class: "card diff-card" }, head, h("div", { class: "mono muted small" }, "#" + c.slug));
    const detail = changeDetail(c);
    if (detail) card.appendChild(h("div", { class: "muted small" }, detail));
    const actions = h("div", { class: "card-actions" });
    if (c.change !== "removed" && c.change !== "orphaned") {
      actions.appendChild(
        h("button", {
          type: "button",
          class: "link-btn",
          text: "Show in Diff View",
          on: {
            click: () => {
              setView("diff");
              jumpToSection(c.slug);
              closeDrawers();
            },
          },
        })
      );
    }
    if (c.diff) {
      const det = h("details", { class: "diff-details" }, h("summary", null, "Unified diff"), renderUnifiedDiff(c.diff));
      card.appendChild(det);
    }
    if (actions.childNodes.length) card.appendChild(actions);
    el.diffsList.appendChild(card);
  }
}

async function loadDiff(rerender) {
  try {
    state.diff = await fetchDiff(state.diffReq);
  } catch (err) {
    showToast(err.message, true);
    if (!state.diff) state.diff = { from: 0, to: 0, head: 0, versions: [], changes: [] };
  }
  renderDiffsRail();
  if (rerender && state.view === "diff" && state.doc) rerenderDocKeepingScroll();
}

function setView(view) {
  state.view = view;
  for (const b of document.querySelectorAll(".viewmode-btn[data-view]")) {
    const on = b.dataset.view === view;
    b.classList.toggle("is-active", on);
    b.setAttribute("aria-pressed", String(on));
  }
  el.app.classList.toggle("is-diff", view === "diff");
  if (!state.doc) return;
  rerenderDocKeepingScroll();
  if (view === "diff" && !state.diff) loadDiff(true);
}

for (const b of document.querySelectorAll(".viewmode-btn[data-view]")) {
  b.addEventListener("click", () => setView(b.dataset.view));
}

function rerenderDocKeepingScroll() {
  const top = el.canvas.scrollTop;
  renderDoc(state.doc);
  el.canvas.scrollTop = top;
}

// Comfort layout: roomier type and spacing, remembered per browser.
function setComfort(on) {
  document.documentElement.classList.toggle("comfort", on);
  el.btnComfort.classList.toggle("is-active", on);
  el.btnComfort.setAttribute("aria-pressed", String(on));
  storageSet(COMFORT_KEY, on ? "1" : "0");
}
el.btnComfort.addEventListener("click", () => setComfort(!document.documentElement.classList.contains("comfort")));
setComfort(storageGet(COMFORT_KEY) === "1");

// ---------------------------------------------------------------------------
// Notes rail
// ---------------------------------------------------------------------------

function emptyHint(text) {
  return h("p", { class: "empty-hint" }, text);
}

function filterThread(t) {
  switch (state.filter) {
    case "decisions":
      return t.root.kind === "decision";
    case "questions":
      return t.root.kind === "question";
    case "unresolved":
      return threadOpen(t);
    default:
      return true;
  }
}

function renderNotesRail(doc) {
  const bySlug = sectionsBySlug(doc);
  const threads = allThreads(doc);
  $("count-notes").textContent = String(threads.length);
  el.btnRailCount.textContent = threads.filter(threadOpen).length ? String(threads.filter(threadOpen).length) : "";
  const dir = state.sort === "oldest" ? 1 : -1;
  const byTime = (a, b) => dir * (threadLatest(a) < threadLatest(b) ? -1 : threadLatest(a) > threadLatest(b) ? 1 : a.root.id - b.root.id);

  const inSection = threads.filter((t) => t.root.section != null && filterThread(t)).sort(byTime);
  const orphaned = threads.filter((t) => t.root.section == null && filterThread(t)).sort(byTime);

  if (state.selectedThread != null && !threads.some((t) => t.root.id === state.selectedThread)) {
    state.selectedThread = null;
  }

  const scrollTop = el.notesScroll.scrollTop;
  el.notesList.textContent = "";
  if (inSection.length === 0) {
    el.notesList.appendChild(
      emptyHint(threads.length ? "No notes match this filter." : "No notes yet. Select text in the document to comment.")
    );
  }
  for (const t of inSection) el.notesList.appendChild(renderThread(t, bySlug.get(t.root.section)));

  el.orphanedGroup.hidden = orphaned.length === 0;
  el.orphanedList.textContent = "";
  for (const t of orphaned) el.orphanedList.appendChild(renderThread(t, null));
  el.notesScroll.scrollTop = scrollTop;

  renderComposerTarget();
}

function renderThread(thread, section) {
  const root = thread.root;
  const open = threadOpen(thread);
  const badge = kindBadge(root);
  const card = h("article", {
    class: "card note-card" + (open ? "" : " is-resolved") + (state.selectedThread === root.id ? " is-selected" : ""),
    id: "thread-" + root.id,
    data: { noteId: root.id },
    tabindex: "-1",
  });
  card.addEventListener("click", (ev) => {
    if (ev.target.closest("button, a, textarea, input, select, form, summary")) return;
    selectThread(state.selectedThread === root.id ? null : root.id);
  });

  const head = h(
    "div",
    { class: "card-head" },
    h(
      "div",
      { class: "card-head-main" },
      h("span", { class: "badge " + badge.cls }, badge.label),
      h("span", { class: "author", title: root.actor + (isViewer(root.actor) ? " (you)" : "") }, root.actor),
      h("time", { class: "time", datetime: root.created_at, title: root.created_at }, formatTime(root.created_at))
    )
  );
  if (section) {
    head.appendChild(
      h("button", { type: "button", class: "jump-btn", text: "Jump", title: "Jump to " + sectionLabel(section), on: { click: () => jumpToSection(section.slug, root.id) } })
    );
  }
  card.appendChild(head);
  card.appendChild(
    h("div", { class: "card-section mono" }, section ? sectionLabel(section) : "@" + (root.anchor || "?") + " · orphaned")
  );

  if (root.quote) {
    if (root.quote_ok === false) {
      card.appendChild(
        h("blockquote", { class: "quote is-missing" }, h("s", null, `“${root.quote}”`), h("div", { class: "quote-note" }, "— no longer present"))
      );
    } else {
      card.appendChild(h("blockquote", { class: "quote" }, `“${root.quote}”`));
    }
  }

  card.appendChild(h("p", { class: "note-body" }, root.body));
  if (root.resolved_at) card.appendChild(resolvedLine(root));

  if (thread.replies.length) {
    const replies = h("div", { class: "replies" });
    for (const r of thread.replies) replies.appendChild(renderReply(r));
    card.appendChild(replies);
  }

  if (state.openReplyFor.has(root.id)) card.appendChild(renderReplyForm(root));
  if (state.openResolveFor.has(root.id)) card.appendChild(renderResolveForm(root));

  const foot = h("div", { class: "card-foot" });
  foot.appendChild(
    h("button", {
      type: "button",
      class: "link-btn muted-btn",
      text: state.openReplyFor.has(root.id) ? "Cancel reply" : "Reply",
      on: { click: () => toggleReplyForm(root.id) },
    })
  );
  if (root.resolved_at) {
    foot.appendChild(h("span", { class: "resolved-state", title: `Resolved by ${root.resolved_by} · ${formatTime(root.resolved_at)}` }, icon("check", "ic-xs"), "Resolved"));
  } else if (!state.openResolveFor.has(root.id)) {
    foot.appendChild(h("button", { type: "button", class: "link-btn resolve-btn", text: "Resolve", on: { click: () => toggleResolveForm(root.id, true) } }));
  }
  card.appendChild(foot);
  return card;
}

function resolvedLine(note) {
  return h("div", { class: "resolved-line" }, `Resolved by ${note.resolved_by || "?"} · ${formatTime(note.resolved_at)}`);
}

function renderReply(note) {
  const badge = kindBadge(note);
  const item = h(
    "div",
    { class: "reply", data: { noteId: note.id } },
    h(
      "div",
      { class: "reply-head" },
      isViewer(note.actor) ? null : h("span", { class: "badge badge-note badge-xs" }, "AI"),
      note.kind !== "note" ? h("span", { class: "badge badge-xs " + badge.cls }, badge.label) : null,
      h("span", { class: "author" }, note.actor),
      h("time", { class: "time", datetime: note.created_at }, formatTime(note.created_at))
    ),
    h("p", { class: "note-body" }, note.body)
  );
  if (note.resolved_at) item.appendChild(resolvedLine(note));
  else if (state.openResolveFor.has(note.id)) item.appendChild(renderResolveForm(note));
  else
    item.appendChild(
      h("div", { class: "reply-actions" }, h("button", { type: "button", class: "link-btn resolve-btn", text: "Resolve", on: { click: () => toggleResolveForm(note.id, true) } }))
    );
  return item;
}

function draftArea(key, attrs) {
  const ta = h("textarea", attrs);
  ta.value = state.drafts.get(key) || "";
  ta.addEventListener("input", () => state.drafts.set(key, ta.value));
  return ta;
}

function renderReplyForm(rootNote) {
  const key = "reply:" + rootNote.id;
  const textarea = draftArea(key, { rows: 2, placeholder: "Reply…", "aria-label": "Reply" });
  const form = h(
    "form",
    { class: "inline-form", data: { rootId: rootNote.id } },
    textarea,
    h(
      "div",
      { class: "inline-actions" },
      h("button", { type: "button", class: "btn btn-sm", text: "Cancel", on: { click: () => toggleReplyForm(rootNote.id, false) } }),
      h("button", { type: "submit", class: "btn btn-sm btn-primary", text: "Reply" })
    )
  );
  textarea.addEventListener("keydown", (ev) => submitOnEnter(ev, form));
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const body = textarea.value.trim();
    if (!body) return;
    await busy(form, async () => {
      try {
        await postNote({ section: rootNote.section, kind: "note", body, parent_id: rootNote.id });
        state.openReplyFor.delete(rootNote.id);
        state.drafts.delete(key);
        await refreshDoc();
        showToast("Reply posted.");
      } catch (err) {
        showToast(err.message, true);
      }
    });
  });
  return form;
}

function renderResolveForm(note) {
  const key = "resolve:" + note.id;
  const input = draftArea(key, { rows: 1, placeholder: "Rationale (optional)", "aria-label": "Resolution rationale (optional)" });
  const form = h(
    "form",
    { class: "inline-form resolve-form" },
    input,
    h(
      "div",
      { class: "inline-actions" },
      h("button", { type: "button", class: "btn btn-sm", text: "Cancel", on: { click: () => toggleResolveForm(note.id, false) } }),
      h("button", { type: "submit", class: "btn btn-sm btn-emerald", text: "Resolve" })
    )
  );
  input.addEventListener("keydown", (ev) => submitOnEnter(ev, form));
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    await busy(form, async () => {
      try {
        await postResolve(note.id, input.value.trim());
        state.openResolveFor.delete(note.id);
        state.drafts.delete(key);
        await refreshDoc();
        showToast("Resolved.");
      } catch (err) {
        showToast(err.message, true);
      }
    });
  });
  return form;
}

function submitOnEnter(ev, form) {
  if (ev.key === "Enter" && !ev.shiftKey && !ev.isComposing) {
    ev.preventDefault();
    form.requestSubmit();
  }
}

function toggleReplyForm(noteId, force) {
  const open = force === undefined ? !state.openReplyFor.has(noteId) : force;
  if (open) state.openReplyFor.add(noteId);
  else state.openReplyFor.delete(noteId);
  renderNotesRail(state.doc);
  if (open) {
    const ta = document.querySelector(`#thread-${noteId} .inline-form textarea`);
    if (ta) ta.focus();
  }
}

function toggleResolveForm(noteId, open) {
  if (open) state.openResolveFor.add(noteId);
  else state.openResolveFor.delete(noteId);
  renderNotesRail(state.doc);
  if (open) {
    const ta = document.querySelector(`.resolve-form textarea`);
    if (ta) ta.focus();
  }
}

function selectThread(id) {
  state.selectedThread = id;
  for (const c of document.querySelectorAll(".note-card")) {
    c.classList.toggle("is-selected", Number(c.dataset.noteId) === id);
  }
  renderComposerTarget();
}

/** From the document (highlight / jump button / thread link) to the thread card. */
function focusThread(noteId) {
  setTab("notes");
  openRail();
  if (state.filter !== "all") {
    const thread = allThreads(state.doc).find((t) => t.root.id === noteId);
    if (thread && !filterThread(thread)) setFilter("all");
  }
  selectThread(noteId);
  const card = $("thread-" + noteId);
  if (!card) return;
  card.scrollIntoView({ behavior: "smooth", block: "nearest" });
  card.classList.remove("is-flash");
  void card.offsetWidth;
  card.classList.add("is-flash");
  card.focus({ preventScroll: true });
}

function jumpToSection(slug, noteId) {
  const target = $("section-" + slug);
  if (!target) return;
  const top = target.getBoundingClientRect().top - el.canvas.getBoundingClientRect().top + el.canvas.scrollTop - 24;
  el.canvas.scrollTo({ top, behavior: "smooth" });
  const mark = noteId != null ? target.querySelector(`mark.hl[data-note-id="${noteId}"]`) : null;
  const flash = mark ? mark.closest(".discuss-block") || mark : target;
  flash.classList.remove("is-flash");
  void flash.offsetWidth;
  flash.classList.add("is-flash");
  if (noteId != null) selectThread(noteId);
}

function setFilter(f) {
  state.filter = f;
  for (const b of document.querySelectorAll(".chip-btn[data-filter]")) {
    b.classList.toggle("is-active", b.dataset.filter === f);
    b.setAttribute("aria-pressed", String(b.dataset.filter === f));
  }
  if (state.doc) renderNotesRail(state.doc);
}

for (const b of document.querySelectorAll(".chip-btn[data-filter]")) {
  b.addEventListener("click", () => setFilter(b.dataset.filter));
}

function renderSortLabel() {
  el.btnSort.textContent = state.sort === "oldest" ? "Sort: Oldest" : "Sort: Newest";
}
el.btnSort.addEventListener("click", () => {
  state.sort = state.sort === "oldest" ? "newest" : "oldest";
  storageSet(SORT_KEY, state.sort);
  renderSortLabel();
  if (state.doc) renderNotesRail(state.doc);
});
renderSortLabel();

// --- composer -----------------------------------------------------------------------

function composerTarget() {
  if (!state.doc) return null;
  if (state.selectedThread != null) {
    const t = allThreads(state.doc).find((x) => x.root.id === state.selectedThread);
    if (t) return { kind: "reply", thread: t };
  }
  const bySlug = sectionsBySlug(state.doc);
  const section = bySlug.get(state.activeSection) || state.doc.sections[0];
  return section ? { kind: "section", section } : null;
}

function renderComposerTarget() {
  const t = composerTarget();
  el.composerTarget.textContent = "";
  el.composerClear.hidden = true;
  el.composerKind.hidden = true;
  if (!t) return;
  if (t.kind === "reply") {
    const r = t.thread.root;
    el.composerTarget.appendChild(h("span", { class: "muted" }, "Reply to "));
    el.composerTarget.appendChild(h("strong", null, `${r.kind} by ${r.actor}`));
    el.composerClear.hidden = false;
    el.composerBody.placeholder = "Reply to the selected thread…";
  } else {
    el.composerTarget.appendChild(h("span", { class: "muted" }, "New "));
    el.composerKind.hidden = false;
    el.composerTarget.appendChild(el.composerKind);
    el.composerTarget.appendChild(h("span", { class: "muted" }, " on "));
    el.composerTarget.appendChild(h("strong", { title: "The section in view; scroll or select a thread to change" }, sectionLabel(t.section)));
    el.composerBody.placeholder = "Add a note or reply…";
  }
}

el.composerClear.addEventListener("click", () => selectThread(null));
el.composerBody.addEventListener("keydown", (ev) => submitOnEnter(ev, el.composer));
el.composer.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const body = el.composerBody.value.trim();
  const t = composerTarget();
  if (!body || !t) return;
  await busy(el.composer, async () => {
    try {
      if (t.kind === "reply") {
        await postNote({ section: t.thread.root.section, kind: "note", body, parent_id: t.thread.root.id });
      } else {
        await postNote({ section: t.section.slug, kind: el.composerKind.value, body, parent_id: null });
      }
      el.composerBody.value = "";
      await refreshDoc();
      showToast(t.kind === "reply" ? "Reply posted." : "Note posted.");
    } catch (err) {
      showToast(err.message, true);
    }
  });
});

// ---------------------------------------------------------------------------
// Build notes / tasks rails
// ---------------------------------------------------------------------------

function renderBuildNotes(doc) {
  const list = doc.build_notes || [];
  $("count-build").textContent = String(list.length);
  el.buildNotesList.textContent = "";
  if (!list.length) {
    el.buildNotesList.appendChild(emptyHint("No build notes. Add them when approving."));
    return;
  }
  for (const bn of list) {
    el.buildNotesList.appendChild(
      h(
        "div",
        { class: "card" },
        h(
          "div",
          { class: "card-head" },
          h("div", { class: "card-head-main" }, h("span", { class: "badge badge-build" }, "build note"), h("span", { class: "author" }, bn.actor), h("time", { class: "time" }, formatTime(bn.created_at)))
        ),
        h("p", { class: "note-body" }, bn.body)
      )
    );
  }
}

function renderTasksRail(tasks) {
  const links = (tasks && tasks.links) || [];
  $("count-tasks").textContent = String(links.length);
  el.tasksLinks.textContent = "";
  if (!links.length) el.tasksLinks.appendChild(emptyHint("No linked tasks."));
  for (const link of links) el.tasksLinks.appendChild(renderTaskLink(link));

  el.tasksGaps.textContent = "";
  const gaps = (tasks && tasks.coverage_gaps) || [];
  if (!gaps.length) el.tasksGaps.appendChild(emptyHint("No coverage gaps."));
  for (const gap of gaps) {
    el.tasksGaps.appendChild(
      h(
        "button",
        { type: "button", class: "card card-row", on: { click: () => jumpToSection(gap.section) } },
        h("span", { class: "badge badge-question" }, "gap"),
        h("span", null, `${gap.number ? gap.number + " " : ""}${gap.heading}`)
      )
    );
  }

  el.tasksDrift.textContent = "";
  const drift = (tasks && tasks.drift) || [];
  if (!drift.length) el.tasksDrift.appendChild(emptyHint("No drift."));
  for (const d of drift) {
    el.tasksDrift.appendChild(
      h("div", { class: "card card-row" }, h("span", { class: "badge badge-blocker" }, "drift"), h("span", null, `task #${d.task_id} agreed r${d.agreed_revision}, spec is r${d.head_revision}`))
    );
  }
}

function renderTaskLink(link) {
  const status = String(link.status || "");
  const tone = status === "done" ? "badge-decision" : status === "blocked" ? "badge-blocker" : status === "in_progress" ? "badge-note" : "badge-plain";
  const bits = [`${link.kind} → ${link.section}`];
  if (link.claimed_by) bits.push(`claimed: ${link.claimed_by}`);
  if (link.blocked_by && link.blocked_by.length) bits.push(`blocked by #${link.blocked_by.join(", #")}`);
  if (link.stale) bits.push(`stale (agreed r${link.agreed_revision})`);
  return h(
    "div",
    { class: "card task-card" + (link.stale ? " is-stale" : "") },
    h(
      "div",
      { class: "card-head" },
      h("div", { class: "card-head-main" }, h("span", { class: "mono task-id" }, "#" + link.task_id), h("span", { class: "task-title" }, link.title)),
      h("span", { class: "badge " + tone }, status.replace("_", " "))
    ),
    h("div", { class: "muted small" }, bits.join(" · ")),
    h("div", { class: "card-actions" }, h("button", { type: "button", class: "link-btn", text: "Jump", on: { click: () => jumpToSection(link.section) } }))
  );
}

// ---------------------------------------------------------------------------
// Tabs and drawers
// ---------------------------------------------------------------------------

const TABS = ["notes", "tasks", "build", "diffs"];
function setTab(tab) {
  state.activeTab = tab;
  for (const t of TABS) {
    const on = t === tab;
    const btn = $("tab-" + t);
    btn.classList.toggle("is-active", on);
    btn.setAttribute("aria-selected", String(on));
    const panel = $("panel-" + t);
    panel.classList.toggle("is-active", on);
    panel.hidden = !on;
  }
  if (tab === "diffs" && !state.diff) loadDiff(state.view === "diff");
}

for (const t of TABS) $("tab-" + t).addEventListener("click", () => setTab(t));
el.btnOpenDiffView.addEventListener("click", () => {
  setView("diff");
  closeDrawers();
});

function openRail() {
  el.app.classList.add("rail-open");
  el.btnRail.setAttribute("aria-expanded", "true");
  syncScrim();
}

function closeDrawers() {
  el.app.classList.remove("rail-open", "outline-open");
  el.btnRail.setAttribute("aria-expanded", "false");
  el.btnOutline.setAttribute("aria-expanded", "false");
  syncScrim();
}

function syncScrim() {
  el.scrim.hidden = !(el.app.classList.contains("rail-open") || el.app.classList.contains("outline-open"));
}

el.btnRail.addEventListener("click", () => {
  if (el.app.classList.contains("rail-open")) closeDrawers();
  else openRail();
});
el.btnRailClose.addEventListener("click", closeDrawers);
el.btnOutline.addEventListener("click", () => {
  const open = !el.app.classList.contains("outline-open");
  closeDrawers();
  if (open) {
    el.app.classList.add("outline-open");
    el.btnOutline.setAttribute("aria-expanded", "true");
  }
  syncScrim();
});
el.scrim.addEventListener("click", closeDrawers);
el.specRef.addEventListener("click", () => copyRef(session.specRef));
el.specRef.addEventListener("keydown", (ev) => {
  if (ev.key === "Enter" || ev.key === " ") {
    ev.preventDefault();
    copyRef(session.specRef);
  }
});

// ---------------------------------------------------------------------------
// Selection → floating comment button
// ---------------------------------------------------------------------------

let pendingSelection = null; // {section, quote, prefix, suffix}

function clearFab() {
  el.commentFab.hidden = true;
  pendingSelection = null;
}

function handleSelectionChange() {
  const sel = window.getSelection();
  if (!sel || sel.isCollapsed || sel.rangeCount === 0) {
    clearFab();
    return;
  }
  const range = sel.getRangeAt(0);
  const text = sel.toString();
  if (!text.trim()) {
    clearFab();
    return;
  }
  if (!el.doc.contains(range.commonAncestorContainer)) {
    clearFab();
    return;
  }

  const startSection = closestSection(range.startContainer);
  const endSection = closestSection(range.endContainer);
  if (!startSection || !endSection) {
    clearFab();
    return;
  }
  if (startSection !== endSection) {
    showToast("Select text within a single section.", true);
    clearFab();
    return;
  }

  const full = sectionText(startSection);
  const { prefix, suffix } = contextAround(startSection, range, full);

  pendingSelection = {
    section: startSection.dataset.section,
    quote: text,
    prefix,
    suffix,
  };

  positionFab(range);
}

function closestSection(node) {
  const elNode = node.nodeType === Node.ELEMENT_NODE ? node : node.parentElement;
  return elNode ? elNode.closest(".section") : null;
}

function contextAround(sectionEl, range, fullText) {
  const body = sectionEl.querySelector(".section-body");
  if (!body) return { prefix: "", suffix: "" };
  const pre = document.createRange();
  pre.selectNodeContents(body);
  pre.setEnd(range.startContainer, range.startOffset);
  const before = pre.toString();

  const post = document.createRange();
  post.selectNodeContents(body);
  post.setStart(range.endContainer, range.endOffset);
  const after = post.toString();

  return {
    prefix: before.slice(Math.max(0, before.length - SELECTION_CONTEXT_CHARS)),
    suffix: after.slice(0, SELECTION_CONTEXT_CHARS),
  };
}

// The FAB is position: fixed, so viewport coordinates are used directly.
function positionFab(range) {
  const rect = range.getBoundingClientRect();
  el.commentFab.hidden = false;
  const w = el.commentFab.offsetWidth;
  const hgt = el.commentFab.offsetHeight;
  let top = rect.top - hgt - 8;
  if (top < 64) top = rect.bottom + 8;
  const left = Math.min(window.innerWidth - w - 8, Math.max(8, rect.left + rect.width / 2 - w / 2));
  el.commentFab.style.top = top + "px";
  el.commentFab.style.left = left + "px";
}

document.addEventListener("selectionchange", () => {
  // selectionchange fires continuously while dragging; keep it cheap.
  clearTimeout(handleSelectionChange._t);
  handleSelectionChange._t = setTimeout(handleSelectionChange, 60);
});
el.canvas.addEventListener("scroll", () => {
  if (pendingSelection) {
    const sel = window.getSelection();
    if (sel && sel.rangeCount) positionFab(sel.getRangeAt(0));
  }
});

el.commentFab.addEventListener("mousedown", (ev) => ev.preventDefault()); // keep the selection
el.commentFab.addEventListener("click", openCommentDialogFromSelection);

function openCommentDialogFromSelection() {
  if (!pendingSelection) return;
  openCommentDialog(pendingSelection);
}

// ---------------------------------------------------------------------------
// Comment dialog
// ---------------------------------------------------------------------------

let activeComment = null;

function openCommentDialog(selection) {
  activeComment = selection;
  el.commentQuote.textContent = selection.quote ? `“${selection.quote}”` : "";
  el.commentKind.value = "question";
  el.commentBody.value = "";
  el.commentError.hidden = true;
  el.commentDialog.hidden = false;
  el.commentFab.hidden = true;
  el.commentBody.focus();
}

function closeCommentDialog() {
  el.commentDialog.hidden = true;
  activeComment = null;
}

el.commentBody.addEventListener("keydown", (ev) => {
  if (ev.key === "Enter" && (ev.metaKey || ev.ctrlKey)) {
    ev.preventDefault();
    el.commentForm.requestSubmit();
  }
});

el.commentForm.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  if (!activeComment) return;
  const body = el.commentBody.value.trim();
  if (!body) {
    el.commentError.textContent = "Comment body is required.";
    el.commentError.hidden = false;
    return;
  }
  const comment = activeComment;
  await busy(el.commentForm, async () => {
    try {
      await postNote({
        section: comment.section,
        quote: comment.quote,
        prefix: comment.prefix,
        suffix: comment.suffix,
        kind: el.commentKind.value,
        body,
        parent_id: null,
      });
      closeCommentDialog();
      window.getSelection().removeAllRanges();
      await refreshDoc();
      showToast("Comment posted.");
    } catch (err) {
      el.commentError.textContent = err.message;
      el.commentError.hidden = false;
    }
  });
});

for (const btn of el.commentForm.querySelectorAll("[data-close]")) {
  btn.addEventListener("click", closeCommentDialog);
}
el.commentDialog.addEventListener("click", (ev) => {
  if (ev.target === el.commentDialog) closeCommentDialog();
});

// ---------------------------------------------------------------------------
// Send to agent (manual release)
// ---------------------------------------------------------------------------

el.btnSend.addEventListener("click", async () => {
  el.btnSend.disabled = true;
  try {
    const res = await postRelease();
    showToast(res.released > 0 ? `Released ${res.released} comment(s).` : "Nothing pending.");
    await refreshDoc();
  } catch (err) {
    showToast(err.message, true);
  } finally {
    if (state.doc) renderHeader(state.doc);
    else el.btnSend.disabled = false;
  }
});

// ---------------------------------------------------------------------------
// Approve dialog
// ---------------------------------------------------------------------------

function addBuildNoteRow(value) {
  const input = h("input", { type: "text", placeholder: "Build note…", "aria-label": "Build note" });
  input.value = value || "";
  const row = h("div", { class: "approve-build-note-row" }, input);
  row.appendChild(h("button", { type: "button", class: "icon-btn", title: "Remove", "aria-label": "Remove build note", text: "×", on: { click: () => row.remove() } }));
  el.approveBuildNotes.appendChild(row);
  input.focus();
}

el.approveBuildNoteAdd.addEventListener("click", () => addBuildNoteRow(""));

el.btnApprove.addEventListener("click", () => {
  el.approveNote.value = "";
  el.approveBuildNotes.textContent = "";
  el.approveError.hidden = true;
  if (state.doc) {
    const spec = state.doc.spec;
    const open = allThreads(state.doc).filter(threadOpen).length;
    el.approveSub.textContent =
      `Approves revision ${spec.revision}.` + (open ? ` ${open} thread${open === 1 ? " is" : "s are"} still open.` : "");
  }
  el.approveDialog.hidden = false;
  el.approveNote.focus();
});

function closeApproveDialog() {
  el.approveDialog.hidden = true;
}

for (const btn of el.approveForm.querySelectorAll("[data-close]")) {
  btn.addEventListener("click", closeApproveDialog);
}
el.approveDialog.addEventListener("click", (ev) => {
  if (ev.target === el.approveDialog) closeApproveDialog();
});

el.approveForm.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const buildNotes = Array.from(el.approveBuildNotes.querySelectorAll("input"))
    .map((i) => i.value.trim())
    .filter(Boolean);
  await busy(el.approveForm, async () => {
    try {
      await postApprove({ note: el.approveNote.value.trim(), build_notes: buildNotes });
      closeApproveDialog();
      await refreshDoc();
      showToast("Approved.");
    } catch (err) {
      el.approveError.textContent = err.message;
      el.approveError.hidden = false;
    }
  });
});

// ---------------------------------------------------------------------------
// Keyboard shortcuts
// ---------------------------------------------------------------------------

function isTyping() {
  const a = document.activeElement;
  return !!a && (a.tagName === "INPUT" || a.tagName === "TEXTAREA" || a.tagName === "SELECT" || a.isContentEditable);
}

function focusTocFilter() {
  if (window.matchMedia("(max-width: 800px)").matches) {
    el.app.classList.add("outline-open");
    el.btnOutline.setAttribute("aria-expanded", "true");
    syncScrim();
  }
  el.tocFilter.focus();
  el.tocFilter.select();
}

document.addEventListener("keydown", (ev) => {
  if (ev.key === "Escape") {
    if (!el.commentDialog.hidden) {
      closeCommentDialog();
      return;
    }
    if (!el.approveDialog.hidden) {
      closeApproveDialog();
      return;
    }
    if (el.app.classList.contains("rail-open") || el.app.classList.contains("outline-open")) {
      closeDrawers();
      return;
    }
    clearFab();
    window.getSelection().removeAllRanges();
    return;
  }
  if ((ev.metaKey || ev.ctrlKey) && (ev.key === "k" || ev.key === "K")) {
    ev.preventDefault();
    focusTocFilter();
    return;
  }
  if (isTyping() || ev.metaKey || ev.ctrlKey || ev.altKey) return;
  if (!el.commentDialog.hidden || !el.approveDialog.hidden) return;
  if (ev.key === "/") {
    ev.preventDefault();
    focusTocFilter();
    return;
  }
  if (ev.key === "c" || ev.key === "C") {
    if (pendingSelection) {
      ev.preventDefault();
      openCommentDialogFromSelection();
    }
  }
});

// ---------------------------------------------------------------------------
// Refresh (initial load + after mutation + on SSE event)
// ---------------------------------------------------------------------------

async function refreshDoc() {
  const scrollTop = el.canvas.scrollTop;
  try {
    const doc = await fetchDoc();
    state.doc = doc;
    renderHeader(doc);
    renderSheetHead(doc);
    renderDoc(doc);
    renderOutline(doc);
    renderNotesRail(doc);
    renderBuildNotes(doc);
    el.canvas.scrollTop = scrollTop;
    trackActiveSection();
  } catch (err) {
    showToast(err.message, true);
  }
}

async function refreshTasks() {
  try {
    const tasks = await fetchTasks();
    state.tasks = tasks;
    renderTasksRail(tasks);
  } catch (err) {
    showToast(err.message, true);
  }
}

async function refreshAll() {
  const jobs = [refreshDoc(), refreshTasks()];
  if (state.diff) jobs.push(loadDiff(false));
  await Promise.all(jobs);
  if (state.diff && state.view === "diff") rerenderDocKeepingScroll();
}

// ---------------------------------------------------------------------------
// Live updates (SSE)
// ---------------------------------------------------------------------------

function connectEvents() {
  const cursor = state.doc ? state.doc.cursor : 0;
  const url =
    "/api/events?after=" +
    cursor +
    "&spec=" +
    encodeURIComponent(session.specRef) +
    (session.token ? "&t=" + encodeURIComponent(session.token) : "");
  const es = new EventSource(url);
  state.es = es;

  es.addEventListener("agtrack", () => {
    clearTimeout(state.sseTimer);
    state.sseTimer = setTimeout(() => {
      refreshAll();
    }, SSE_DEBOUNCE_MS);
  });

  es.addEventListener("error", () => {
    // EventSource retries on its own; nothing to do here beyond letting it.
  });
}

// ---------------------------------------------------------------------------
// Boot
// ---------------------------------------------------------------------------

async function main() {
  if (!session.specRef) {
    el.doc.textContent = "";
    el.doc.appendChild(emptyHint("No spec in URL."));
    return;
  }
  await refreshAll();
  el.app.removeAttribute("aria-busy");
  connectEvents();
  // The Diffs tab count is useful at a glance; fetch it once the page is up.
  if (!state.diff) loadDiff(true);
}

main();
