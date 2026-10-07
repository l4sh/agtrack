# Spec viewer — HTTP contract

The contract between `internal/web` (server) and `internal/web/assets` (the embedded page).
Security rules are not optional; see the checklist at the end.

## Session

`agtrack spec serve <spec>` binds `127.0.0.1:<port>` (port 0 by default = random), mints a
32-byte `crypto/rand` token (base64url), and prints the URL:

```
http://127.0.0.1:<port>/s/<spec>?t=<token>
```

- Page and SSE requests carry the token as `?t=<token>`.
- API requests carry it in the `X-Agtrack-Token` header (the page reads it from its own URL
  once, then strips it with `history.replaceState`).
- Fonts (Inter, JetBrains Mono; latin subset) are vendored under `/assets/vendor/fonts/`
  with their OFL licences and a `VERSIONS` file of sources and sha256 sums — no external font
  requests (`font-src 'self'`).
- `style-src` allows inline styles only because Mermaid's SVG output embeds a `<style>`
  block; scripts stay `'self'` only.
- The first `GET /s/{spec}?t=` sets `agtrack_session=<token>` (HttpOnly, SameSite=Strict,
  Path=/), so a reload after the page stripped `?t=` still works; page, SSE and API requests
  accept that cookie as an alternative to `?t=` / the header.
- Every request: `Host` must be `127.0.0.1:<port>` or `localhost:<port>`, else 403.
- Every mutating request: `Content-Type: application/json`, else 415.
- The server exits after an idle timeout (default 30m without requests or SSE clients).

`<spec>` in URLs is the spec ref as typed on the CLI (`auth`, or `orbiter:api/auth`,
URL-escaped).

## GET /s/{spec}?t=TOKEN

The single-page app (`index.html`), plus static assets under `/assets/…` (no token needed for
static assets; they contain no data). CSP header:

```
default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:;
connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'
```

## GET /api/spec/{spec}

```json
{
  "spec": {
    "slug": "auth", "ref": "orbiter:api/auth", "title": "Authentication v2",
    "source_path": "specs/auth.md", "status": "active",
    "revision": 6, "sha": "7f0d…",
    "release_mode": "manual",
    "approval": {"id": 50, "by": "alex", "at": "2026-09-29T14:08:11Z",
                 "revision": 6, "note": "ship it"} ,
    "approval_stale": false,
    "pending_comments": 2
  },
  "front_matter": {"title": "Authentication v2", "owner": "alex"},
  "sections": [
    {"ordinal": 0, "slug": "root", "pin": null, "number": "", "heading": "",
     "level": 0, "kind": "root", "path": "",
     "html": "<p>…</p>", "has_mermaid": false,
     "line_start": 7, "line_end": 9,
     "open_notes": 0, "tasks": []},
    {"ordinal": 3, "slug": "tokens__rotation", "pin": "grace-policy", "number": "2.1",
     "heading": "Rotation", "level": 3, "kind": "heading", "path": "Tokens / Rotation",
     "html": "…", "has_mermaid": false, "line_start": 27, "line_end": 45,
     "open_notes": 1, "tasks": [12, 13]}
  ],
  "notes": [
    {"id": 41, "section": "tokens__rotation", "anchor": "grace-policy",
     "kind": "question", "body": "is 60s per-client?", "actor": "alex",
     "created_at": "2026-09-29T14:01:00Z", "quote": "grace window of 60s",
     "quote_ok": true, "parent_id": null, "resolved_at": null, "resolved_by": null,
     "released": false}
  ],
  "build_notes": [{"id": 44, "body": "don't touch billing", "actor": "alex",
                   "created_at": "…"}],
  "viewer": {"actor": "alex"},
  "cursor": 2074
}
```

- `viewer.actor` is the name browser actions are attributed to (`spec serve --as`, default
  `human`). The page uses it to tell the reviewer's own notes from other actors' (agents).

- `html` is server-rendered by goldmark **without** `html.WithUnsafe()` and sanitised; raw HTML
  in the spec is escaped. The heading itself is not part of `html` — the page renders it
  from `heading`/`level`/`number`.
- Mermaid fences are rendered as
  `<pre class="mermaid-src" data-mermaid="1"><code>…escaped source…</code></pre>`. The page
  renders them client-side with the bundled Mermaid at `securityLevel: 'strict'`, passing the
  element's `textContent` (never `innerHTML`).
  Clicking a rendered diagram opens it in a floating window (`assets/diagram.js`), which shows
  a clone of Mermaid's SVG and the source via `textContent`. Save PNG re-renders the source
  off-screen with `htmlLabels: false` and draws it on a canvas from a `data:` image; it needs
  no CSP change (`img-src data:`, `connect-src 'self'` for the bundled fonts).
- Sections of an orphaned pin are not in `sections`; their notes carry `"section": null` and
  `"anchor": "<key>"`, and the page lists them under "Orphaned".
- `notes` holds every note on the spec's anchors (open and resolved); replies have `parent_id`.

## GET /api/spec/{spec}/tasks

```json
{
  "links": [
    {"section": "tokens__rotation", "anchor": "grace-policy", "task_id": 12,
     "title": "Rotate refresh tokens", "status": "done", "claimed_by": "codex",
     "claim_expires_at": null, "kind": "implements", "blocked_by": [],
     "agreed_revision": 3, "stale": true}
  ],
  "coverage_gaps": [{"section": "sessions__idle_timeout", "number": "3.2",
                     "heading": "Idle timeout"}],
  "drift": [{"section": "tokens__rotation", "task_id": 12, "agreed_revision": 3,
             "head_revision": 6}]
}
```

## GET /api/spec/{spec}/diff?from=N&to=M

Section-level comparison of two revisions (the store's `SpecDiff`, as `agtrack spec diff`
prints it). Both parameters are optional revision numbers ≥ 1 (else `400 invalid_argument`):
`to` defaults to the head; `from` defaults to the approved revision when one exists and is
older than `to`, else `to - 1` — stepping further back past revisions that change no section
(lazy pinning adds a revision that only writes an id comment), so the default shows the last
real edit. An unknown revision is `404 not_found`. A spec with a single
revision returns `"from": 0` and no changes.

```json
{
  "from": 5, "to": 6, "head": 6, "approved_revision": 5,
  "versions": [{"n": 5, "imported_at": "…", "actor": "codex"}, {"n": 6, "…": "…"}],
  "changes": [
    {"change": "renamed", "slug": "tokens__rotation", "prior_slug": "tokens__refresh",
     "heading": "Rotation", "prior_heading": "Refresh", "number": "2.1", "prior_number": "2.1",
     "tier": "pin", "diff": "--- v5\n+++ v6\n@@ -1 +1 @@\n-…\n+…\n"},
    {"change": "new", "slug": "sessions__idle_timeout", "heading": "Idle timeout", "number": "3.2"},
    {"change": "removed", "slug": "sessions__legacy", "heading": "Legacy cookies"},
    {"change": "orphaned", "slug": "…", "anchor": "legacy-cookie-flag", "revision": 6}
  ]
}
```

- `change` ∈ `new|removed|orphaned|moved|renamed|changed`. `diff` (a unified diff of the
  section body, raw markdown) is present when the body changed; it is untrusted text and the
  page renders it with `textContent` only. `confidence` appears on fuzzy (`similar`) matches.
- `approved_revision` is null when the spec has never been approved.

## POST /api/spec/{spec}/note

Request — created from a browser selection:

```json
{"section": "tokens__rotation", "quote": "grace window of 60s",
 "prefix": "stays valid for a ", "suffix": " to absorb in-flight",
 "kind": "question", "body": "is 60s configurable per client?",
 "parent_id": null}
```

`kind` ∈ `note|question|decision|blocker|build-note`. `quote`, `prefix`, `suffix`,
`parent_id` optional. A reply (`parent_id` set) inherits its parent's section.

Response `201`:

```json
{"note": { …note object as above… }, "pinned": "grace-policy",
 "file_modified": true}
```

`pinned` is the key minted by lazy pinning (null when the section was already pinned).

## POST /api/spec/{spec}/note/{id}/resolve

Request `{"note": "optional rationale"}` → `200 {"note": {…}}`.
Resolving an already-resolved note is a no-op returning the note.

## POST /api/spec/{spec}/release

Manual mode: releases the accumulated comments to the waiting agent.
Request `{}` → `200 {"released": 2, "cursor": 2080}`. Releasing with nothing pending returns
`{"released": 0, …}`.

## POST /api/spec/{spec}/approve

Request `{"note": "ship it, keep the grace window configurable",
"build_notes": ["don't touch the billing migration in this pass"]}` →
`200 {"approval": {…}, "build_notes": [{…}]}`.
Approving the same revision twice returns the existing approval (double-click safe).

## GET /api/events?after=N&t=TOKEN&spec=REF

Server-sent events. One event per `events` row after cursor `N` relevant to the spec:

```
id: 2081
event: agtrack
data: {"id":2081,"action":"spec-note","actor":"alex","target":"tokens__rotation","detail":"…"}
```

Plus `event: ping` every 15s. The page re-fetches `/api/spec/{spec}` (and `/tasks`) on any
`agtrack` event, debounced.

## Errors

Non-2xx responses carry `{"error": "<token>", "message": "…"}` — the same tokens as the CLI's
structured errors (`not_found`, `invalid_argument`, `conflict`, …), plus `forbidden` (403),
`unsupported_media_type` (415).

## Security checklist (go-rewrite-spec §7.4)

- bind 127.0.0.1 only; token ≥32 bytes from crypto/rand, compared with
  `subtle.ConstantTimeCompare`; Host header validated; no CORS headers; CSP above; goldmark
  without unsafe HTML + sanitised output; Mermaid pinned and `securityLevel: 'strict'`;
  idle-timeout exit; every mutation through the same store, transactions, idempotency and
  attribution: browser actions are recorded as `spec serve --as NAME` (default `human`),
  through a second store on the same connection — never as the `--actor`/`AGTRACK_ACTOR` of
  the agent running `serve`, which would hide them from its `--actor-not self` watch.
  `serve` refuses to start when `--as` equals the serving actor.
