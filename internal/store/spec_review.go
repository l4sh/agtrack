package store

import (
	"fmt"
	"math"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/spec"
)

// --- the review loop (go spec §9) ---------------------------------------------------------

func (s *Store) specAnchorIDsSQL() string {
	return "SELECT id FROM spec_anchors WHERE spec_id = ?"
}

// pendingComments counts comments not yet released to waiting agents.
func (s *Store) pendingComments(specID int64) int64 {
	return s.count("SELECT COUNT(*) FROM notes WHERE released_at IS NULL AND target_type = 'spec_anchor' AND target_id IN ("+s.specAnchorIDsSQL()+")", specID)
}

func (s *Store) openNoteCount(specID int64) int64 {
	return s.count("SELECT COUNT(*) FROM notes WHERE resolved_at IS NULL AND kind NOT IN ('approval','build-note') AND target_type = 'spec_anchor' AND target_id IN ("+s.specAnchorIDsSQL()+")", specID)
}

func (s *Store) latestApproval(sp *row.Row) *row.Row {
	a := s.one("SELECT n.*, v.n AS revision FROM notes n LEFT JOIN spec_versions v ON v.id = n.version_id WHERE n.target_type = 'spec' AND n.target_id = ? AND n.kind = 'approval' ORDER BY n.id DESC LIMIT 1", sp.Int("id"))
	if a == nil {
		return nil
	}
	return approvalRow(a)
}

// sameContent reports whether two versions say the same thing: identical
// sections, headings and text. A lazy pin adds a revision that differs only
// by a comment line; it must not make an approval stale or end a review.
func (s *Store) sameContent(v1, v2 int64) bool {
	if v1 == v2 {
		return true
	}
	key := func(v int64) string {
		var b strings.Builder
		for _, r := range s.q("SELECT path_slug, context_sha FROM spec_sections WHERE version_id = ? ORDER BY ordinal", v) {
			b.WriteString(r.Str("path_slug") + " " + r.Str("context_sha") + "\n")
		}
		return b.String()
	}
	return key(v1) == key(v2)
}

// approvalStale: the approval names content that is no longer at head.
func (s *Store) approvalStale(sp, approval *row.Row) bool {
	if approval == nil || sp.IsNull("head_version_id") {
		return false
	}
	return !s.sameContent(approval.Int("version_id"), sp.Int("head_version_id"))
}

// SpecSameContent reports whether two revisions of a spec differ only by
// pin comments (or not at all).
func (s *Store) SpecSameContent(specID int64, n1, n2 int) (same bool, err error) {
	defer guard(&err)
	sp := s.specByID(specID)
	return s.sameContent(s.versionN(sp, n1).Int("id"), s.versionN(sp, n2).Int("id")), nil
}

// SpecOfNote is the spec a note belongs to (0 if none).
func (s *Store) SpecOfNote(noteID int64) (specID int64, err error) {
	defer guard(&err)
	n := s.one("SELECT * FROM notes WHERE id = ?", noteID)
	for depth := 0; n != nil && depth < 50; depth++ {
		switch n.Str("target_type") {
		case "spec":
			return n.Int("target_id"), nil
		case "spec_anchor":
			if a := s.one("SELECT spec_id FROM spec_anchors WHERE id = ?", n.Int("target_id")); a != nil {
				return a.Int("spec_id"), nil
			}
			return 0, nil
		}
		if n.IsNull("parent_id") {
			break
		}
		n = s.one("SELECT * FROM notes WHERE id = ?", n.Int("parent_id"))
	}
	return 0, nil
}

func approvalRow(a *row.Row) *row.Row {
	return row.Of("id", a.Int("id"), "by", a.Get("actor"), "at", a.Str("created_at"), "revision", a.Get("revision"), "note", nullable(a.Str("body")),
		"version_id", a.Get("version_id"))
}

func buildNoteRow(n *row.Row) *row.Row {
	return row.Of("id", n.Int("id"), "body", n.Str("body"), "actor", n.Get("actor"), "created_at", n.Str("created_at"))
}

// buildNotes lists the open build notes of a spec: spec-level ones and those
// posted on a section (which also carry "section").
func (s *Store) buildNotes(specID int64) []*row.Row {
	sp := s.specByID(specID)
	out := []*row.Row{}
	for _, n := range s.q(`SELECT * FROM notes WHERE kind = 'build-note' AND resolved_at IS NULL AND
 ((target_type = 'spec' AND target_id = ?) OR (target_type = 'spec_anchor' AND target_id IN (`+s.specAnchorIDsSQL()+`))) ORDER BY id`, specID, specID) {
		r := buildNoteRow(n)
		if n.Str("target_type") == "spec_anchor" {
			v := s.viewerNote(sp, n)
			sec := v.Get("section")
			if sec == nil {
				sec = v.Get("anchor")
			}
			r.Set("section", sec)
		}
		out = append(out, r)
	}
	return out
}

func (s *Store) release(sp *row.Row) (int, int64) {
	pending := s.pendingComments(sp.Int("id"))
	if pending == 0 {
		return 0, s.count("SELECT COALESCE(MAX(id), 0) FROM events")
	}
	now := refs.NowISO()
	id := s.event("spec", sp.Int("id"), "spec-released", fmt.Sprintf("%s: %d comment(s)", sp.Str("slug"), pending), 0, sp.Int("project_id"), now)
	// release_event names the batch exactly; watchers expand the release by it.
	n, _ := s.exec("UPDATE notes SET released_at = ?, release_event = ? WHERE released_at IS NULL AND target_type = 'spec_anchor' AND target_id IN ("+s.specAnchorIDsSQL()+")",
		now, id, sp.Int("id")).RowsAffected()
	return int(n), id
}

// SpecRelease sends the accumulated comments to waiting agents (manual mode).
func (s *Store) SpecRelease(specID int64) (released int, cursor int64, err error) {
	defer guard(&err)
	s.tx(func() { released, cursor = s.release(s.specByID(specID)) })
	return released, cursor, nil
}

// SpecApprove records approval of the head revision, releasing any pending
// comments and adding build notes. Approving the same revision twice returns
// the first approval and writes nothing.
func (s *Store) SpecApprove(specID int64, note string, buildNotes []string) (approval *row.Row, buildNoteRows []*row.Row, err error) {
	defer guard(&err)
	buildNoteRows = []*row.Row{}
	s.tx(func() {
		sp := s.specByID(specID)
		head := s.versionN(sp, 0)
		// A bare repeat of a current approval is a no-op (double-click safe);
		// one carrying a note or build notes is recorded as a new approval.
		fresh := strings.TrimSpace(note) != ""
		for _, bn := range buildNotes {
			fresh = fresh || strings.TrimSpace(bn) != ""
		}
		if a := s.latestApproval(sp); a != nil && !s.approvalStale(sp, a) && !fresh {
			s.release(sp) // comments written since are still sent
			approval = a
			return
		}
		s.release(sp)
		now := refs.NowISO()
		body := strings.TrimSpace(note)
		id := s.insert("INSERT INTO notes (uid, target_type, target_id, body, actor, created_at, kind, version_id, released_at) VALUES (?,?,?,?,?,?,?,?,?)",
			NewUID(), "spec", specID, body, nullable(s.Actor), now, "approval", head.Int("id"), now)
		for _, bn := range buildNotes {
			if bn = strings.TrimSpace(bn); bn == "" {
				continue
			}
			bid := s.insert("INSERT INTO notes (uid, target_type, target_id, body, actor, created_at, kind, version_id, released_at) VALUES (?,?,?,?,?,?,?,?,?)",
				NewUID(), "spec", specID, bn, nullable(s.Actor), now, "build-note", head.Int("id"), now)
			s.event("note", bid, "spec-build-note", firstLine(bn), 0, sp.Int("project_id"), now)
			buildNoteRows = append(buildNoteRows, buildNoteRow(s.one("SELECT * FROM notes WHERE id = ?", bid)))
		}
		detail := fmt.Sprintf("revision %d", head.Int("n"))
		if body != "" {
			detail += "; " + firstLine(body)
		}
		s.event("note", id, "spec-approved", detail, 0, sp.Int("project_id"), now)
		approval = approvalRow(s.one("SELECT n.*, v.n AS revision FROM notes n JOIN spec_versions v ON v.id = n.version_id WHERE n.id = ?", id))
	})
	return approval, buildNoteRows, nil
}

// SpecBuildNote adds guidance for the build that is not part of the spec.
func (s *Store) SpecBuildNote(specID int64, body string) (r *row.Row, err error) {
	defer guard(&err)
	s.tx(func() {
		n, _, _ := s.specAddNote(s.specByID(specID), SpecNoteInput{Kind: "build-note", Body: body})
		r = n
	})
	return r, nil
}

// SpecEventsAfter lists the events relevant to a spec after a cursor: its
// own, its notes', and those of tasks linked to it.
func (s *Store) SpecEventsAfter(specID, after int64, limit int) (out []*row.Row, err error) {
	defer guard(&err)
	sp := s.specByID(specID)
	if limit <= 0 {
		limit = 500
	}
	out = []*row.Row{}
	for _, e := range s.q(`SELECT e.* FROM events e WHERE e.id > ? AND (
 (e.target_type = 'spec' AND e.target_id = ?)
 OR (e.target_type = 'note' AND e.target_id IN (SELECT n.id FROM notes n WHERE (n.target_type = 'spec' AND n.target_id = ?)
      OR (n.target_type = 'spec_anchor' AND n.target_id IN (SELECT id FROM spec_anchors WHERE spec_id = ?))))
 OR (e.task_id IN (SELECT l.task_id FROM spec_task_links l JOIN spec_anchors a ON a.id = l.anchor_id WHERE a.spec_id = ?))
) ORDER BY e.id LIMIT ?`, after, specID, specID, specID, specID, limit) {
		out = append(out, row.Of("id", e.Int("id"), "action", e.Str("action"), "actor", e.Get("actor"),
			"target", s.eventTarget(sp, e), "detail", e.Get("detail"), "at", e.Str("at")))
	}
	return out, nil
}

func (s *Store) eventTarget(sp, e *row.Row) string {
	switch e.Str("target_type") {
	case "note":
		if n := s.one("SELECT * FROM notes WHERE id = ?", e.Int("target_id")); n != nil {
			v := s.viewerNote(sp, n)
			if v.Str("section") != "" {
				return v.Str("section")
			}
			if v.Str("anchor") != "" {
				return v.Str("anchor")
			}
		}
		return sp.Str("slug")
	case "spec":
		return sp.Str("slug")
	}
	if e.Int("task_id") != 0 {
		return fmt.Sprintf("#%d", e.Int("task_id"))
	}
	return e.Str("target_type")
}

// ReleasedBy lists the comments one release event sent, in the note shape.
// `spec watch` expands a release with it.
func (s *Store) ReleasedBy(specID, eventID int64) (out []*row.Row, err error) {
	defer guard(&err)
	sp := s.specByID(specID)
	out = []*row.Row{}
	for _, n := range s.q("SELECT * FROM notes WHERE release_event = ? ORDER BY id", eventID) {
		out = append(out, s.viewerNote(sp, n))
	}
	return out, nil
}

// SpecNote is one note of a spec in the viewer shape, or nil.
func (s *Store) SpecNote(specID, noteID int64) (r *row.Row, err error) {
	defer guard(&err)
	sp := s.specByID(specID)
	n := s.one("SELECT * FROM notes WHERE id = ?", noteID)
	if n == nil || !s.noteBelongs(specID, n) {
		return nil, nil
	}
	return s.viewerNote(sp, n), nil
}

// SpecBuildNotes lists the open build notes of a spec.
func (s *Store) SpecBuildNotes(specID int64) (out []*row.Row, err error) {
	defer guard(&err)
	return s.buildNotes(specID), nil
}

// SpecApprovalNote is the approval recorded by one note, with whether its
// content is still head (nil when the note is not an approval of the spec).
func (s *Store) SpecApprovalNote(specID, noteID int64) (a *row.Row, stale bool, err error) {
	defer guard(&err)
	sp := s.specByID(specID)
	r := s.one("SELECT n.*, v.n AS revision FROM notes n LEFT JOIN spec_versions v ON v.id = n.version_id WHERE n.id = ? AND n.kind = 'approval' AND n.target_type = 'spec' AND n.target_id = ?", noteID, specID)
	if r == nil {
		return nil, false, nil
	}
	a = approvalRow(r)
	return a, s.approvalStale(sp, a), nil
}

// SpecReleasedOpen lists released, unresolved comments on a spec.
func (s *Store) SpecReleasedOpen(specID int64) (out []*row.Row, err error) {
	defer guard(&err)
	sp := s.specByID(specID)
	out = []*row.Row{}
	for _, n := range s.q("SELECT * FROM notes WHERE released_at IS NOT NULL AND resolved_at IS NULL AND kind NOT IN ('build-note','approval') AND target_type = 'spec_anchor' AND target_id IN ("+s.specAnchorIDsSQL()+") ORDER BY id", specID) {
		out = append(out, s.viewerNote(sp, n))
	}
	return out, nil
}

// SpecApproval is the latest approval, with whether it names the head.
func (s *Store) SpecApproval(specID int64) (a *row.Row, stale bool, err error) {
	defer guard(&err)
	sp := s.specByID(specID)
	a = s.latestApproval(sp)
	stale = s.approvalStale(sp, a)
	return a, stale, nil
}

// SpecModel is GET /api/spec/{spec} (docs/viewer-api.md).
func (s *Store) SpecModel(specID int64) (out *row.Row, err error) {
	defer guard(&err)
	sp := s.specByID(specID)
	head := s.versionN(sp, 0)
	info := s.specInfo(sp)
	approval := s.latestApproval(sp)
	var approvalAny any
	stale := false
	if approval != nil {
		approvalAny = approval
		stale = s.approvalStale(sp, approval)
	}
	specObj := row.Of("slug", info.Str("slug"), "ref", info.Str("ref"), "title", info.Get("title"), "source_path", info.Get("source_path"),
		"status", info.Str("status"), "revision", head.Int("n"), "sha", head.Str("sha256"), "release_mode", s.releaseMode(),
		"approval", approvalAny, "approval_stale", stale, "pending_comments", s.pendingComments(specID))
	fm := row.New()
	if doc := parseVersion(head); doc.FrontMatter != nil {
		for _, k := range doc.FrontMatter.Keys {
			fm.Set(k, doc.FrontMatter.Fields[k])
		}
	}
	sections := []*row.Row{}
	for _, r := range s.versionSections(head.Int("id")) {
		html, e := spec.RenderHTML(r.Str("body"))
		if e != nil {
			fail(e)
		}
		open, tasks := int64(0), []any{}
		if !r.IsNull("anchor_id") {
			_, open = s.anchorNoteCounts(r.Int("anchor_id"))
			tasks = s.anchorTaskIDs(r.Int("anchor_id"))
		}
		sections = append(sections, row.Of("ordinal", r.Int("ordinal"), "slug", r.Str("path_slug"), "pin", r.Get("pin"),
			"number", r.Str("number"), "heading", r.Str("heading"), "level", r.Int("level"), "kind", r.Str("kind"), "path", r.Str("path"),
			"html", html, "has_mermaid", r.Int("has_mermaid") == 1, "line_start", r.Int("line_start"), "line_end", r.Int("line_end"),
			"open_notes", open, "tasks", tasks))
	}
	notes, _ := s.SpecNotes(specID, "", false)
	out = row.Of("spec", specObj, "front_matter", fm, "sections", sections, "notes", notes,
		"build_notes", s.buildNotes(specID), "cursor", s.count("SELECT COALESCE(MAX(id), 0) FROM events"))
	return out, nil
}

// SpecTasks is GET /api/spec/{spec}/tasks.
func (s *Store) SpecTasks(specID int64) (out *row.Row, err error) {
	defer guard(&err)
	sp := s.specByID(specID)
	links, drift := []*row.Row{}, []*row.Row{}
	for _, l := range s.specStale(sp) {
		blocked := []any{}
		for _, d := range s.q("SELECT d.depends_on_id FROM task_deps d JOIN tasks b ON b.id = d.depends_on_id WHERE d.task_id = ? AND d.kind = 'blocks' AND b.status <> 'done' ORDER BY d.depends_on_id", l.Int("task_id")) {
			blocked = append(blocked, d.Int("depends_on_id"))
		}
		links = append(links, row.Of("section", l.Str("section"), "anchor", l.Str("anchor"), "task_id", l.Int("task_id"),
			"title", l.Str("title"), "status", l.Str("status"), "claimed_by", l.Get("claimed_by"), "claim_expires_at", l.Get("claim_expires_at"),
			"kind", l.Str("kind"), "blocked_by", blocked, "agreed_revision", l.Int("agreed_revision"), "stale", l.Get("stale")))
		if l.Get("stale") == true && l.Str("status") == "done" {
			drift = append(drift, row.Of("section", l.Str("section"), "task_id", l.Int("task_id"),
				"agreed_revision", l.Int("agreed_revision"), "head_revision", l.Int("head_revision")))
		}
	}
	gaps := []*row.Row{}
	for _, g := range s.specCoverage(sp) {
		gaps = append(gaps, row.Of("section", g.Str("section"), "number", g.Str("number"), "heading", g.Str("heading")))
	}
	return row.Of("links", links, "coverage_gaps", gaps, "drift", drift), nil
}

// --- resume ---------------------------------------------------------------------------------

// SpecChecksForTask summarises the spec sections linked to a task — what
// `resume` shows so a cold agent learns about open questions and drift
// without being told to look (design §5.6).
func (s *Store) SpecChecksForTask(taskID int64, complete bool) (out []*row.Row, err error) {
	defer guard(&err)
	out = []*row.Row{}
	for _, l := range s.q(`SELECT l.*, a.key, a.state, a.spec_id FROM spec_task_links l JOIN spec_anchors a ON a.id = l.anchor_id
 WHERE l.task_id = ? ORDER BY a.spec_id, l.id`, taskID) {
		sp := s.specByID(l.Int("spec_id"))
		head := s.headVersion(sp)
		sec := s.anchorHeadSection(sp, l.Int("anchor_id"))
		label := sp.Str("slug") + "#" + l.Str("key")
		if sec != nil {
			label = sp.Str("slug") + "#" + sec.Str("path_slug")
		}
		var parts []string
		agreed := int64(0)
		if !l.IsNull("agreed_version_id") {
			if v := s.one("SELECT n FROM spec_versions WHERE id = ?", l.Int("agreed_version_id")); v != nil {
				agreed = v.Int("n")
			}
		}
		switch {
		case sec == nil:
			parts = append(parts, fmt.Sprintf("section %s (pin %s); see `agtrack spec orphans %s`", l.Str("state"), l.Str("key"), sp.Str("slug")))
		case head != nil && s.linkHash(head.Int("id"), l.Int("anchor_id"), l.Str("scope")) != l.Str("agreed_body_sha"):
			parts = append(parts, fmt.Sprintf("changed since agreed v%d (head v%d)", agreed, head.Int("n")))
		}
		var open []string
		for _, n := range s.q("SELECT id, actor, kind FROM notes WHERE target_type = 'spec_anchor' AND target_id = ? AND resolved_at IS NULL ORDER BY id", l.Int("anchor_id")) {
			open = append(open, fmt.Sprintf("%d %s %s", n.Int("id"), strOrDash(n.Str("actor")), n.Str("kind")))
		}
		if len(open) > 0 {
			parts = append(parts, fmt.Sprintf("%d unresolved note(s) (%s)", len(open), strings.Join(open, "; ")))
		}
		state := "ok"
		if len(parts) > 0 {
			state = strings.Join(parts, "; ")
		}
		r := row.Of("section", label, "kind", l.Str("kind"), "state", state, "open_notes", int64(len(open)))
		if complete && sec != nil {
			r.Set("body", sec.Str("body"))
		}
		out = append(out, r)
	}
	return out, nil
}

func strOrDash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}

// SpecReviewState summarises review traffic on a project's specs: comments
// awaiting release, open threads, and approvals (stale when they name an
// older revision).
func (s *Store) SpecReviewState(projectID int64) (out []*row.Row, err error) {
	defer guard(&err)
	out = []*row.Row{}
	for _, sp := range s.q("SELECT * FROM specs WHERE project_id = ? ORDER BY slug", projectID) {
		head := s.headVersion(sp)
		if head == nil {
			continue
		}
		pending, open := s.pendingComments(sp.Int("id")), s.openNoteCount(sp.Int("id"))
		a := s.latestApproval(sp)
		var parts []string
		if pending > 0 {
			parts = append(parts, fmt.Sprintf("%d comment(s) awaiting release", pending))
		}
		if open > 0 {
			parts = append(parts, fmt.Sprintf("%d open note(s)", open))
		}
		stale := false
		if a != nil {
			stale = s.approvalStale(sp, a)
			txt := fmt.Sprintf("approved v%d", a.Int("revision"))
			if stale {
				txt += fmt.Sprintf(" (stale: head is v%d)", head.Int("n"))
			}
			parts = append(parts, txt)
		}
		if bn := s.buildNotes(sp.Int("id")); len(bn) > 0 {
			parts = append(parts, fmt.Sprintf("%d build note(s)", len(bn)))
		}
		if len(parts) == 0 {
			continue
		}
		var approvalRev any
		if a != nil {
			approvalRev = a.Get("revision")
		}
		out = append(out, row.Of("spec", sp.Str("slug"), "revision", head.Int("n"), "pending_comments", pending, "open_notes", open,
			"approval_revision", approvalRev, "approval_stale", stale,
			"summary", fmt.Sprintf("%s v%d: %s", sp.Str("slug"), head.Int("n"), strings.Join(parts, ", "))))
	}
	return out, nil
}

// --- toc / find / show -----------------------------------------------------------------------

// SpecToc lists a revision's sections with depth, pins, open notes and tasks.
func (s *Store) SpecToc(specID int64, version, maxDepth int) (out []*row.Row, err error) {
	defer guard(&err)
	sp := s.specByID(specID)
	ver := s.versionN(sp, version)
	rows := s.versionSections(ver.Int("id"))
	depth := map[int64]int64{}
	out = []*row.Row{}
	for _, r := range rows {
		d := int64(1)
		if !r.IsNull("parent_id") {
			d = depth[r.Int("parent_id")] + 1
		}
		if r.Str("kind") == spec.KindRoot {
			d = 0
		}
		depth[r.Int("id")] = d
		if r.Str("kind") == spec.KindRoot || (maxDepth > 0 && d > int64(maxDepth)) {
			continue
		}
		open, tasks := int64(0), []any{}
		if !r.IsNull("anchor_id") {
			_, open = s.anchorNoteCounts(r.Int("anchor_id"))
			tasks = s.anchorTaskIDs(r.Int("anchor_id"))
		}
		out = append(out, row.Of("slug", r.Str("path_slug"), "number", r.Str("number"), "heading", r.Str("heading"), "kind", r.Str("kind"),
			"level", r.Int("level"), "depth", d, "pin", r.Get("pin"), "open_notes", open, "tasks", tasks,
			"line_start", r.Int("line_start"), "line_end", r.Int("line_end")))
	}
	return out, nil
}

// SpecFind is FTS5 search over the head revisions of the given specs, best
// first. Unless raw, the query is escaped to literal terms (spec.EscapeFTS).
func (s *Store) SpecFind(specIDs []int64, query string, raw bool, top int) (out []*row.Row, err error) {
	defer guard(&err)
	out = []*row.Row{}
	if len(specIDs) == 0 {
		return out, nil
	}
	match := query
	if !raw {
		match = spec.EscapeFTS(query)
	}
	if strings.TrimSpace(match) == "" {
		fail(errs.New("empty search query"))
	}
	if top <= 0 {
		top = 10
	}
	args := append([]any{match}, ids64(specIDs)...)
	args = append(args, top)
	rs, e := s.DB.Query(`SELECT s.id, s.path_slug, s.number, s.heading, s.anchor_id, sp.id spec_id, sp.slug spec_slug, v.n revision,
 bm25(spec_fts) AS rank, snippet(spec_fts, 1, '*', '*', '…', 12) AS snip
 FROM spec_fts JOIN spec_sections s ON s.id = spec_fts.rowid JOIN specs sp ON sp.head_version_id = s.version_id
 JOIN spec_versions v ON v.id = s.version_id
 WHERE spec_fts MATCH ? AND sp.id IN (`+marks(len(specIDs))+`) ORDER BY rank LIMIT ?`, args...)
	var rows []*row.Row
	if e == nil {
		rows, e = row.Scan(rs)
	}
	if e != nil {
		// Only raw FTS5 syntax can be malformed; escaped queries cannot.
		if raw || strings.Contains(e.Error(), "fts5") {
			fail(errs.New("invalid search query %q: %v", query, e))
		}
		dbFail(e)
	}
	for _, r := range rows {
		score := -r.Get("rank").(float64)
		out = append(out, row.Of("ref", r.Str("spec_slug")+"#"+r.Str("path_slug"), "spec", r.Str("spec_slug"), "section", r.Str("path_slug"),
			"number", r.Str("number"), "heading", r.Str("heading"), "score", math.Round(score*100)/100, "snippet", strings.Join(strings.Fields(r.Str("snip")), " "),
			"revision", r.Int("revision"), "anchor_id", r.Get("anchor_id"), "spec_id", r.Int("spec_id")))
	}
	return out, nil
}

// SectionExtras returns what `--with notes,tasks,children` adds to a section.
func (s *Store) SectionExtras(specID int64, sectionID int64, with map[string]bool) (out *row.Row, err error) {
	defer guard(&err)
	sp := s.specByID(specID)
	sec := s.one(sectionCols+" WHERE s.id = ?", sectionID)
	out = row.New()
	if with["notes"] {
		notes := []*row.Row{}
		if !sec.IsNull("anchor_id") {
			for _, n := range s.q("SELECT * FROM notes WHERE target_type = 'spec_anchor' AND target_id = ? ORDER BY COALESCE(parent_id, id), id", sec.Int("anchor_id")) {
				notes = append(notes, s.viewerNote(sp, n))
			}
		}
		out.Set("notes", notes)
	}
	if with["tasks"] {
		tasks := []*row.Row{}
		if !sec.IsNull("anchor_id") {
			for _, t := range s.q(`SELECT t.id, t.status, t.title, l.kind, l.scope FROM spec_task_links l JOIN tasks t ON t.id = l.task_id
 WHERE l.anchor_id = ? ORDER BY t.id`, sec.Int("anchor_id")) {
				tasks = append(tasks, t)
			}
		}
		out.Set("tasks", tasks)
	}
	if with["children"] {
		children := []*row.Row{}
		for _, c := range s.q(sectionCols+" WHERE s.parent_id = ? ORDER BY s.ordinal", sectionID) {
			children = append(children, row.Of("section", c.Str("path_slug"), "number", c.Str("number"), "heading", c.Str("heading"), "kind", c.Str("kind")))
		}
		out.Set("children", children)
	}
	return out, nil
}

// SectionAncestors lists a section's ancestors, outermost first.
func (s *Store) SectionAncestors(sectionID int64) (out []*row.Row, err error) {
	defer guard(&err)
	out = []*row.Row{}
	sec := s.one("SELECT * FROM spec_sections WHERE id = ?", sectionID)
	for sec != nil && !sec.IsNull("parent_id") {
		sec = s.one(sectionCols+" WHERE s.id = ?", sec.Int("parent_id"))
		out = append([]*row.Row{sec}, out...)
	}
	return out, nil
}

// SectionSubtree is a section and its descendants, in order.
func (s *Store) SectionSubtree(sectionID int64) (out []*row.Row, err error) {
	defer guard(&err)
	sec := s.one("SELECT * FROM spec_sections WHERE id = ?", sectionID)
	rows := s.versionSections(sec.Int("version_id"))
	for i, r := range rows {
		if r.Int("id") == sectionID {
			return subtreeRows(rows, i), nil
		}
	}
	return []*row.Row{}, nil
}

// SpecVersions lists a spec's revisions, oldest first.
func (s *Store) SpecVersions(specID int64) (out []*row.Row, err error) {
	defer guard(&err)
	out = []*row.Row{}
	for _, v := range s.q("SELECT id, n, sha256, git_commit, parser_version, imported_at, actor FROM spec_versions WHERE spec_id = ? ORDER BY n", specID) {
		out = append(out, v)
	}
	return out, nil
}

// SpecRow is the spec row by id, as SpecInfo renders it.
func (s *Store) SpecRow(specID int64) (r *row.Row, err error) {
	defer guard(&err)
	return s.specInfo(s.specByID(specID)), nil
}
