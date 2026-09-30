// agtrack spec viewer — vanilla ES module, no build step, no framework.
//
// Security invariants this file must preserve (docs/viewer-api.md):
//   - Note bodies/quotes/authors are untrusted text: always textContent, never innerHTML.
//   - The only innerHTML assignment is a section's server-rendered `html` field (already
//     sanitised server-side) and Mermaid's own SVG output (rendered from textContent input).
//   - The capability token lives in memory only; it is stripped from the address bar on load
//     and sent as a header, never re-embedded in the DOM or in fetch bodies.

const SELECTION_CONTEXT_CHARS = 32;
const SSE_DEBOUNCE_MS = 300;

if (window.mermaid) {
  window.mermaid.initialize({ startOnLoad: false, securityLevel: "strict" });
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

/**
 * @param {string} path
 * @param {{method?: string, body?: unknown}} [opts]
 */
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

async function api(path, opts = {}) {
  const headers = { "X-Agtrack-Token": session.token };
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

function fetchDoc() {
  return api(apiPath(""));
}

function fetchTasks() {
  return api(apiPath("/tasks"));
}

function postNote(payload) {
  return api(apiPath("/note"), { method: "POST", body: payload });
}

function postResolve(noteId, note) {
  return api(apiPath(`/note/${noteId}/resolve`), { method: "POST", body: { note: note || "" } });
}

function postRelease() {
  return api(apiPath("/release"), { method: "POST", body: {} });
}

function postApprove(payload) {
  return api(apiPath("/approve"), { method: "POST", body: payload });
}

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

const state = {
  doc: null, // last GET /api/spec/{spec} payload
  tasks: null, // last GET /api/spec/{spec}/tasks payload
  activeTab: "notes", // "notes" | "tasks"
  openReplyFor: new Set(), // note ids with an open reply box
  es: null,
  sseTimer: null,
};

// ---------------------------------------------------------------------------
// DOM refs
// ---------------------------------------------------------------------------

const el = {
  app: document.getElementById("app"),
  specTitle: document.getElementById("spec-title"),
  specRef: document.getElementById("spec-ref"),
  specRevision: document.getElementById("spec-revision"),
  specMode: document.getElementById("spec-mode"),
  specApproval: document.getElementById("spec-approval"),
  btnSend: document.getElementById("btn-send"),
  btnApprove: document.getElementById("btn-approve"),
  doc: document.getElementById("doc"),
  tabNotes: document.getElementById("tab-notes"),
  tabTasks: document.getElementById("tab-tasks"),
  panelNotes: document.getElementById("panel-notes"),
  panelTasks: document.getElementById("panel-tasks"),
  notesList: document.getElementById("notes-list"),
  orphanedGroup: document.getElementById("orphaned-group"),
  orphanedList: document.getElementById("orphaned-list"),
  buildNotesList: document.getElementById("build-notes-list"),
  tasksLinks: document.getElementById("tasks-links"),
  tasksGaps: document.getElementById("tasks-gaps"),
  tasksDrift: document.getElementById("tasks-drift"),
  commentFab: document.getElementById("comment-fab"),
  commentDialog: document.getElementById("comment-dialog"),
  commentForm: document.getElementById("comment-form"),
  commentQuote: document.getElementById("comment-quote"),
  commentKind: document.getElementById("comment-kind"),
  commentBody: document.getElementById("comment-body"),
  commentError: document.getElementById("comment-error"),
  approveDialog: document.getElementById("approve-dialog"),
  approveForm: document.getElementById("approve-form"),
  approveNote: document.getElementById("approve-note"),
  approveBuildNotes: document.getElementById("approve-build-notes"),
  approveBuildNoteAdd: document.getElementById("approve-build-note-add"),
  approveError: document.getElementById("approve-error"),
  toast: document.getElementById("toast"),
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
// Header
// ---------------------------------------------------------------------------

function renderHeader(doc) {
  const spec = doc.spec;
  el.specTitle.textContent = spec.title || spec.slug;
  el.specRef.textContent = spec.ref;
  el.specRevision.textContent = "rev " + spec.revision;
  el.specMode.textContent = spec.release_mode;

  el.specApproval.classList.remove("is-approved", "is-stale", "is-none");
  if (spec.approval) {
    if (spec.approval_stale) {
      el.specApproval.textContent = `stale: approved r${spec.approval.revision}, now r${spec.revision}`;
      el.specApproval.classList.add("is-stale");
    } else {
      el.specApproval.textContent = `approved r${spec.approval.revision} by ${spec.approval.by}`;
      el.specApproval.classList.add("is-approved");
    }
  } else {
    el.specApproval.textContent = "not approved";
    el.specApproval.classList.add("is-none");
  }

  if (spec.release_mode === "manual") {
    el.btnSend.hidden = false;
    el.btnSend.textContent =
      spec.pending_comments > 0 ? `Send to agent (${spec.pending_comments})` : "Send to agent";
    el.btnSend.disabled = spec.pending_comments === 0;
  } else {
    el.btnSend.hidden = true;
  }
}

// ---------------------------------------------------------------------------
// Document body
// ---------------------------------------------------------------------------

function sectionText(sectionEl) {
  const body = sectionEl.querySelector(".section-body");
  return body ? body.textContent : "";
}

function renderDoc(doc) {
  el.doc.textContent = "";
  for (const section of doc.sections) {
    el.doc.appendChild(renderSection(section));
  }
  renderMermaid();
}

function renderSection(section) {
  const wrap = document.createElement("section");
  wrap.className = "section";
  wrap.dataset.section = section.slug;
  wrap.dataset.level = String(section.level);
  if (section.open_notes > 0) wrap.classList.add("has-open-note");
  wrap.id = "section-" + section.slug;

  if (section.kind !== "root") {
    const level = Math.min(Math.max(section.level, 1), 6);
    const heading = document.createElement("div");
    heading.className = "section-heading";

    const tag = document.createElement(`h${level}`);
    if (section.number) {
      const num = document.createElement("span");
      num.className = "section-number";
      num.textContent = section.number;
      tag.appendChild(num);
      tag.appendChild(document.createTextNode(" "));
    }
    tag.appendChild(document.createTextNode(section.heading));
    heading.appendChild(tag);

    const slugBtn = document.createElement("button");
    slugBtn.type = "button";
    slugBtn.className = "section-anchor-link";
    slugBtn.textContent = "#" + section.slug;
    slugBtn.title = "Copy reference";
    slugBtn.addEventListener("click", () => copyRef(session.specRef + "#" + section.slug));
    heading.appendChild(slugBtn);

    if (section.pin) {
      const pinBtn = document.createElement("button");
      pinBtn.type = "button";
      pinBtn.className = "section-anchor-link";
      pinBtn.textContent = "@" + section.pin;
      pinBtn.title = "Copy pinned reference";
      pinBtn.addEventListener("click", () => copyRef(session.specRef + "@" + section.pin));
      heading.appendChild(pinBtn);
    }

    wrap.appendChild(heading);
  }

  const body = document.createElement("div");
  body.className = "section-body";
  // The only innerHTML assignment in this file: server-sanitised markdown output.
  body.innerHTML = section.html || "";
  wrap.appendChild(body);

  return wrap;
}

function copyRef(text) {
  navigator.clipboard.writeText(text).then(
    () => showToast("Copied " + text),
    () => showToast("Could not copy to clipboard", true)
  );
}

function renderMermaid() {
  if (!window.mermaid) return;
  const blocks = el.doc.querySelectorAll("pre.mermaid-src[data-mermaid]");
  let i = 0;
  for (const block of blocks) {
    const code = block.querySelector("code");
    const source = code ? code.textContent : block.textContent;
    const host = document.createElement("div");
    host.className = "section-mermaid";
    block.insertAdjacentElement("afterend", host);
    const id = "mermaid-" + Date.now() + "-" + i++;
    window.mermaid
      .render(id, source)
      .then(({ svg }) => {
        host.innerHTML = svg;
      })
      .catch((err) => {
        host.textContent = "Mermaid render failed: " + err.message;
      });
  }
}

// ---------------------------------------------------------------------------
// Notes rail
// ---------------------------------------------------------------------------

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

function renderNotesRail(doc) {
  const sectionOrder = new Map(doc.sections.map((s, i) => [s.slug, i]));
  const inSection = doc.notes.filter((n) => n.section != null);
  const orphaned = doc.notes.filter((n) => n.section == null);

  const threads = buildThreads(inSection).sort((a, b) => {
    const openA = threadOpen(a) ? 0 : 1;
    const openB = threadOpen(b) ? 0 : 1;
    if (openA !== openB) return openA - openB;
    const oa = sectionOrder.get(a.root.section) ?? 0;
    const ob = sectionOrder.get(b.root.section) ?? 0;
    if (oa !== ob) return oa - ob;
    return a.root.id - b.root.id;
  });

  el.notesList.textContent = "";
  if (threads.length === 0) {
    el.notesList.appendChild(emptyHint("No comments yet."));
  }
  const sectionsBySlug = new Map(doc.sections.map((s) => [s.slug, s]));
  for (const thread of threads) {
    el.notesList.appendChild(renderThread(thread, sectionsBySlug.get(thread.root.section)));
  }

  const orphanThreads = buildThreads(orphaned).sort((a, b) => a.root.id - b.root.id);
  el.orphanedGroup.hidden = orphanThreads.length === 0;
  el.orphanedList.textContent = "";
  for (const thread of orphanThreads) {
    el.orphanedList.appendChild(renderThread(thread, null));
  }

  el.buildNotesList.textContent = "";
  if (!doc.build_notes || doc.build_notes.length === 0) {
    el.buildNotesList.appendChild(emptyHint("No build notes."));
  } else {
    for (const bn of doc.build_notes) {
      el.buildNotesList.appendChild(renderBuildNote(bn));
    }
  }
}

function emptyHint(text) {
  const p = document.createElement("p");
  p.className = "empty-hint";
  p.textContent = text;
  return p;
}

function renderBuildNote(bn) {
  const card = document.createElement("div");
  card.className = "build-note-item";
  const body = document.createElement("div");
  body.textContent = bn.body;
  card.appendChild(body);
  const meta = document.createElement("div");
  meta.className = "build-note-meta";
  meta.textContent = `${bn.actor} · ${formatTime(bn.created_at)}`;
  card.appendChild(meta);
  return card;
}

function renderThread(thread, section) {
  const card = document.createElement("div");
  card.className = "note-thread";
  if (!threadOpen(thread)) card.classList.add("is-resolved");

  if (section) {
    const head = document.createElement("div");
    head.className = "note-thread-section";
    const label = document.createElement("span");
    label.textContent = (section.number ? section.number + " " : "") + (section.heading || section.path);
    head.appendChild(label);
    const jump = document.createElement("button");
    jump.type = "button";
    jump.className = "btn btn-sm";
    jump.textContent = "Jump";
    jump.addEventListener("click", () => jumpToSection(section.slug));
    head.appendChild(jump);
    card.appendChild(head);
  } else if (thread.root.anchor) {
    const head = document.createElement("div");
    head.className = "note-thread-section";
    head.textContent = "@" + thread.root.anchor;
    card.appendChild(head);
  }

  if (thread.root.quote) {
    const q = document.createElement("p");
    q.className = "note-quote";
    if (thread.root.quote_ok === false) {
      q.classList.add("is-missing");
      q.textContent = `"${thread.root.quote}" — no longer present`;
    } else {
      q.textContent = `"${thread.root.quote}"`;
    }
    card.appendChild(q);
  }

  card.appendChild(renderNote(thread.root, false));
  for (const reply of thread.replies) {
    card.appendChild(renderNote(reply, true));
  }

  card.appendChild(renderReplyForm(thread.root));

  return card;
}

function renderNote(note, isReply) {
  const item = document.createElement("div");
  item.className = "note" + (isReply ? " is-reply" : "");
  item.dataset.noteId = String(note.id);

  const meta = document.createElement("div");
  meta.className = "note-meta";
  const kind = document.createElement("span");
  kind.className = "note-kind kind-" + note.kind;
  kind.textContent = note.kind;
  meta.appendChild(kind);
  const actor = document.createElement("span");
  actor.textContent = note.actor;
  meta.appendChild(actor);
  const time = document.createElement("span");
  time.textContent = formatTime(note.created_at);
  meta.appendChild(time);
  item.appendChild(meta);

  const body = document.createElement("div");
  body.className = "note-body";
  body.textContent = note.body;
  item.appendChild(body);

  if (note.resolved_at) {
    const resolved = document.createElement("div");
    resolved.className = "note-meta";
    resolved.textContent = `resolved by ${note.resolved_by} · ${formatTime(note.resolved_at)}`;
    item.appendChild(resolved);
  } else {
    const actions = document.createElement("div");
    actions.className = "note-actions";
    if (!isReply) {
      const replyBtn = document.createElement("button");
      replyBtn.type = "button";
      replyBtn.textContent = "Reply";
      replyBtn.addEventListener("click", () => toggleReplyForm(note.id));
      actions.appendChild(replyBtn);
    }
    const resolveBtn = document.createElement("button");
    resolveBtn.type = "button";
    resolveBtn.textContent = "Resolve";
    resolveBtn.addEventListener("click", () => resolveNote(note.id));
    actions.appendChild(resolveBtn);
    item.appendChild(actions);
  }

  return item;
}

function renderReplyForm(rootNote) {
  const form = document.createElement("form");
  form.className = "reply-form";
  form.dataset.rootId = String(rootNote.id);
  if (state.openReplyFor.has(rootNote.id)) form.classList.add("is-open");

  const textarea = document.createElement("textarea");
  textarea.rows = 2;
  textarea.placeholder = "Reply…";
  form.appendChild(textarea);

  const actions = document.createElement("div");
  actions.className = "dialog-actions";
  const cancel = document.createElement("button");
  cancel.type = "button";
  cancel.className = "btn btn-sm";
  cancel.textContent = "Cancel";
  cancel.addEventListener("click", () => toggleReplyForm(rootNote.id, false));
  actions.appendChild(cancel);
  const send = document.createElement("button");
  send.type = "submit";
  send.className = "btn btn-sm btn-primary";
  send.textContent = "Reply";
  actions.appendChild(send);
  form.appendChild(actions);

  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    const body = textarea.value.trim();
    if (!body) return;
    await busy(form, async () => {
      try {
        await postNote({
          section: rootNote.section,
          kind: "note",
          body,
          parent_id: rootNote.id,
        });
        state.openReplyFor.delete(rootNote.id);
        await refreshDoc();
        showToast("Reply posted.");
      } catch (err) {
        showToast(err.message, true);
      }
    });
  });

  return form;
}

function toggleReplyForm(noteId, force) {
  const open = force === undefined ? !state.openReplyFor.has(noteId) : force;
  if (open) state.openReplyFor.add(noteId);
  else state.openReplyFor.delete(noteId);
  renderNotesRail(state.doc);
}

async function resolveNote(noteId) {
  const rationale = window.prompt("Resolution rationale (optional):", "") || "";
  try {
    await postResolve(noteId, rationale);
    await refreshDoc();
    showToast("Resolved.");
  } catch (err) {
    showToast(err.message, true);
  }
}

function jumpToSection(slug) {
  const target = document.getElementById("section-" + slug);
  if (!target) return;
  target.scrollIntoView({ behavior: "smooth", block: "start" });
  target.classList.add("is-highlighted");
  setTimeout(() => target.classList.remove("is-highlighted"), 1500);
}

function formatTime(iso) {
  if (!iso) return "";
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}

// ---------------------------------------------------------------------------
// Tasks rail
// ---------------------------------------------------------------------------

function renderTasksRail(tasks) {
  el.tasksLinks.textContent = "";
  if (!tasks || tasks.links.length === 0) {
    el.tasksLinks.appendChild(emptyHint("No linked tasks."));
  } else {
    for (const link of tasks.links) {
      el.tasksLinks.appendChild(renderTaskLink(link));
    }
  }

  el.tasksGaps.textContent = "";
  if (!tasks || tasks.coverage_gaps.length === 0) {
    el.tasksGaps.appendChild(emptyHint("No coverage gaps."));
  } else {
    for (const gap of tasks.coverage_gaps) {
      const card = document.createElement("div");
      card.className = "task-gap";
      card.textContent = `${gap.number ? gap.number + " " : ""}${gap.heading}`;
      el.tasksGaps.appendChild(card);
    }
  }

  el.tasksDrift.textContent = "";
  if (!tasks || tasks.drift.length === 0) {
    el.tasksDrift.appendChild(emptyHint("No drift."));
  } else {
    for (const d of tasks.drift) {
      const card = document.createElement("div");
      card.className = "task-drift";
      card.textContent = `task #${d.task_id} agreed r${d.agreed_revision}, spec is r${d.head_revision}`;
      el.tasksDrift.appendChild(card);
    }
  }
}

function renderTaskLink(link) {
  const card = document.createElement("div");
  card.className = "task-link" + (link.stale ? " is-stale" : "");

  const title = document.createElement("div");
  title.className = "task-link-title";
  title.textContent = `#${link.task_id} ${link.title}`;
  card.appendChild(title);

  const status = document.createElement("span");
  status.className = "status-badge";
  status.textContent = link.status;
  title.appendChild(document.createTextNode(" "));
  title.appendChild(status);

  const meta = document.createElement("div");
  meta.className = "task-link-meta";
  const bits = [`${link.kind} → ${link.section}`];
  if (link.claimed_by) bits.push(`claimed: ${link.claimed_by}`);
  if (link.blocked_by && link.blocked_by.length) bits.push(`blocked by #${link.blocked_by.join(", #")}`);
  if (link.stale) bits.push(`stale (agreed r${link.agreed_revision})`);
  meta.textContent = bits.join(" · ");
  card.appendChild(meta);

  return card;
}

// ---------------------------------------------------------------------------
// Tab switching
// ---------------------------------------------------------------------------

function setTab(tab) {
  state.activeTab = tab;
  const notesActive = tab === "notes";
  el.tabNotes.classList.toggle("is-active", notesActive);
  el.tabTasks.classList.toggle("is-active", !notesActive);
  el.tabNotes.setAttribute("aria-selected", String(notesActive));
  el.tabTasks.setAttribute("aria-selected", String(!notesActive));
  el.panelNotes.classList.toggle("is-active", notesActive);
  el.panelTasks.classList.toggle("is-active", !notesActive);
  el.panelNotes.hidden = !notesActive;
  el.panelTasks.hidden = notesActive;
}

el.tabNotes.addEventListener("click", () => setTab("notes"));
el.tabTasks.addEventListener("click", () => setTab("tasks"));

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

function positionFab(range) {
  const rect = range.getBoundingClientRect();
  el.commentFab.hidden = false;
  const top = window.scrollY + rect.top - el.commentFab.offsetHeight - 8;
  const left = window.scrollX + rect.left + rect.width / 2 - el.commentFab.offsetWidth / 2;
  el.commentFab.style.top = Math.max(8, top) + "px";
  el.commentFab.style.left = Math.max(8, left) + "px";
}

document.addEventListener("selectionchange", () => {
  // selectionchange fires continuously while dragging; keep it cheap.
  clearTimeout(handleSelectionChange._t);
  handleSelectionChange._t = setTimeout(handleSelectionChange, 60);
});

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
  el.commentQuote.textContent = selection.quote ? `"${selection.quote}"` : "";
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
    el.btnSend.disabled = false;
  }
});

// ---------------------------------------------------------------------------
// Approve dialog
// ---------------------------------------------------------------------------

function addBuildNoteRow(value) {
  const row = document.createElement("div");
  row.className = "approve-build-note-row";
  const input = document.createElement("input");
  input.type = "text";
  input.value = value || "";
  input.placeholder = "Build note…";
  row.appendChild(input);
  const rm = document.createElement("button");
  rm.type = "button";
  rm.textContent = "×";
  rm.title = "Remove";
  rm.addEventListener("click", () => row.remove());
  row.appendChild(rm);
  el.approveBuildNotes.appendChild(row);
}

el.approveBuildNoteAdd.addEventListener("click", () => addBuildNoteRow(""));

el.btnApprove.addEventListener("click", () => {
  el.approveNote.value = "";
  el.approveBuildNotes.textContent = "";
  el.approveError.hidden = true;
  el.approveDialog.hidden = false;
  el.approveNote.focus();
});

for (const btn of el.approveForm.querySelectorAll("[data-close]")) {
  btn.addEventListener("click", () => {
    el.approveDialog.hidden = true;
  });
}

el.approveForm.addEventListener("submit", async (ev) => {
  ev.preventDefault();
  const buildNotes = Array.from(el.approveBuildNotes.querySelectorAll("input"))
    .map((i) => i.value.trim())
    .filter(Boolean);
  await busy(el.approveForm, async () => {
    try {
      await postApprove({ note: el.approveNote.value.trim(), build_notes: buildNotes });
      el.approveDialog.hidden = true;
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

document.addEventListener("keydown", (ev) => {
  if (ev.key === "Escape") {
    if (!el.commentDialog.hidden) {
      closeCommentDialog();
      return;
    }
    if (!el.approveDialog.hidden) {
      el.approveDialog.hidden = true;
      return;
    }
    clearFab();
    window.getSelection().removeAllRanges();
    return;
  }
  if (ev.key === "c" || ev.key === "C") {
    const active = document.activeElement;
    const typing = active && (active.tagName === "INPUT" || active.tagName === "TEXTAREA" || active.isContentEditable);
    if (typing) return;
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
  const scrollY = window.scrollY;
  try {
    const doc = await fetchDoc();
    state.doc = doc;
    renderHeader(doc);
    renderDoc(doc);
    renderNotesRail(doc);
    window.scrollTo(0, scrollY);
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
  await Promise.all([refreshDoc(), refreshTasks()]);
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
    "&t=" +
    encodeURIComponent(session.token);
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
}

main();
