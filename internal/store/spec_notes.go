package store

import (
	"fmt"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/spec"
)

// --- lazy pinning ---------------------------------------------------------------------

// FileWrite is a pin write-back owed to the spec's file. It is applied only
// after the database transaction that published the pinned revision has
// committed, so a failed file write never loses the database write (design
// §5.8); a later `spec sync` restores the missing comment.
type FileWrite struct {
	Path, Rel string
	Data      []byte
	ExpectSHA string
}

// Apply writes the file atomically, refusing if it changed since it was read.
func (w *FileWrite) Apply() error {
	if w == nil {
		return nil
	}
	return spec.WriteFileAtomic(w.Path, w.Data, w.ExpectSHA)
}

// pinResult is the outcome of pinning one section.
type pinResult struct {
	anchorID int64
	key      string
	minted   bool // a new anchor row was created
	write    *FileWrite
	version  *row.Row // head after pinning
	section  *row.Row // the section in that head
}

// pinSection gives a head section a durable anchor, minting a key and
// splicing a pin comment into the file when it has none (design §5.2).
func (s *Store) pinSection(sp, head, sec *row.Row, repoRoot string) *pinResult {
	res := &pinResult{version: head, section: sec}
	if !sec.IsNull("anchor_id") {
		res.anchorID, res.key = sec.Int("anchor_id"), sec.Str("pin")
		return res
	}
	now := refs.NowISO()
	newAnchor := func(key, kind string) int64 {
		if a := s.one("SELECT * FROM spec_anchors WHERE spec_id = ? AND key = ?", sp.Int("id"), key); a != nil {
			if a.Str("state") == "retired" {
				fail(errs.Conflict("pin id %q belongs to a retired section of %s; rename the pin comment in the file", key, sp.Str("slug")))
			}
			s.exec("UPDATE spec_anchors SET state = 'live', last_seen_version = ? WHERE id = ?", head.Int("id"), a.Int("id"))
			return a.Int("id")
		}
		res.minted = true
		return s.insert("INSERT INTO spec_anchors (spec_id, key, kind, state, pinned_at_version, first_seen_version, last_seen_version, created_at) VALUES (?,?,?,'live',?,?,?,?)",
			sp.Int("id"), key, kind, head.Int("id"), head.Int("id"), head.Int("id"), now)
	}
	attach := func(aid int64) {
		s.exec("UPDATE spec_sections SET anchor_id = ? WHERE id = ?", aid, sec.Int("id"))
		res.anchorID = aid
		res.section = s.one(sectionCols+" WHERE s.id = ?", sec.Int("id"))
	}
	doc := parseVersion(head)
	dsec := doc.Section(int(sec.Int("ordinal")))
	switch {
	case sec.Str("kind") == spec.KindRoot && sec.Int("level") == 0:
		res.key = spec.RootSlug // the preamble's identity is structural: nothing to write
		attach(newAnchor(res.key, "root"))
		return res
	case dsec.PinKey != "":
		// Already pinned in the file (pin comment, {#key}, or a block key).
		res.key = dsec.PinKey
		attach(newAnchor(res.key, anchorKind(sec.Str("kind"))))
		return res
	}
	taken := doc.TakenKeys()
	delete(taken, dsec.PathSlug) // a key equal to its own path slug shadows nothing
	for _, a := range s.q("SELECT key FROM spec_anchors WHERE spec_id = ?", sp.Int("id")) {
		taken[a.Str("key")] = true
	}
	// A key from the own slug alone would shadow the ambiguous suffix lookup
	// (`revocation` under both tokens and sessions): use the path instead.
	mint := *dsec
	for i := range doc.Sections {
		if o := &doc.Sections[i]; o.Ordinal != dsec.Ordinal && o.OwnSlug == dsec.OwnSlug {
			mint.OwnSlug = strings.ReplaceAll(dsec.PathSlug, "__", "_")
			break
		}
	}
	res.key = spec.MintKey(&mint, taken)
	_, path := s.specFile(sp, repoRoot)
	src := readFile(path)
	if spec.SHA(src) != head.Str("sha256") {
		fail(errs.Conflict("%s has changed since v%d; run `agtrack spec sync %s` first, then retry", sp.Str("source_path"), head.Int("n"), sp.Str("slug")))
	}
	fdoc, e := spec.Parse(src)
	if e != nil {
		fail(e)
	}
	out, e := spec.InsertPin(src, fdoc.Section(int(sec.Int("ordinal"))), res.key)
	if e != nil {
		fail(e)
	}
	attach(newAnchor(res.key, anchorKind(sec.Str("kind"))))
	// Publish the pinned file as the next revision: tier 1 carries the new
	// anchor, and the one comment line is the only difference.
	ndoc, e := spec.Parse(out)
	if e != nil {
		fail(e)
	}
	p := s.planImport(sp, head, ndoc)
	v := s.writeVersion(sp, head, p, gitMeta{}, now)
	s.event("spec", sp.Int("id"), "spec-pinned", fmt.Sprintf("%s#%s pinned as %s (v%d)", sp.Str("slug"), sec.Str("path_slug"), res.key, v.Int("n")),
		0, sp.Int("project_id"), now)
	res.version = v
	res.section = s.one(sectionCols+" WHERE s.version_id = ? AND s.anchor_id = ?", v.Int("id"), res.anchorID)
	res.write = &FileWrite{Path: path, Rel: sp.Str("source_path"), Data: out, ExpectSHA: head.Str("sha256")}
	return res
}

func anchorKind(sectionKind string) string {
	if sectionKind == spec.KindBlock {
		return "block"
	}
	if sectionKind == spec.KindRoot {
		return "root"
	}
	return "heading"
}

func (s *Store) oneSection(sp *row.Row, ref string) (*row.Row, *row.Row) {
	head := s.versionN(sp, 0)
	secs, _ := s.lookupSections(sp, head, ref)
	if len(secs) != 1 {
		var c []string
		for _, x := range secs {
			c = append(c, x.Str("path_slug"))
		}
		fail(&errs.Error{Code: errs.ExitError, Token: errs.TokAmbiguous, Entity: "section", Ref: ref,
			Message: fmt.Sprintf("%s matches %d sections: %s", ref, len(secs), strings.Join(c, ", ")), Extra: map[string]any{"candidates": c}})
	}
	return head, secs[0]
}

// --- notes ------------------------------------------------------------------------------

// SpecNoteKinds are the kinds a spec comment may take.
var SpecNoteKinds = []string{"note", "question", "decision", "blocker", "handoff", "build-note"}

func (s *Store) releaseMode() string {
	if v, ok := s.getMeta("config:review.release"); ok && v != "" {
		return v
	}
	return "manual"
}

// SpecAddNote creates a comment on a section (pinning it lazily), a reply,
// or a spec-level build note. The pin's file write-back happens after the
// transaction; its failure is reported in err only when nothing else failed,
// wrapped so the caller can tell the note itself was stored.
func (s *Store) SpecAddNote(specID int64, in SpecNoteInput) (note *row.Row, pinned string, fileModified bool, err error) {
	defer guard(&err)
	var write *FileWrite
	s.tx(func() {
		note, pinned, write = s.specAddNote(s.specByID(specID), in)
	})
	if write != nil {
		if e := write.Apply(); e != nil {
			return note, pinned, false, &PinWriteError{Rel: write.Rel, Err: e}
		}
		fileModified = true
	}
	return note, pinned, fileModified, nil
}

// PinWriteError means the database write succeeded but the pin comment could
// not be written to the file; `spec sync` restores it.
type PinWriteError struct {
	Rel string
	Err error
}

func (e *PinWriteError) Error() string {
	return fmt.Sprintf("the note is saved, but writing the pin to %s failed (%v); run `agtrack spec sync` to restore it", e.Rel, e.Err)
}

func (s *Store) specAddNote(sp *row.Row, in SpecNoteInput) (*row.Row, string, *FileWrite) {
	body := strings.TrimSpace(in.Body)
	if body == "" {
		fail(errs.New("a note needs a body"))
	}
	kind := in.Kind
	if kind == "" {
		kind = "note"
	}
	valid := false
	for _, k := range SpecNoteKinds {
		valid = valid || k == kind
	}
	if !valid {
		fail(errs.New("invalid note kind: %s (expected one of %s)", kind, strings.Join(SpecNoteKinds, ", ")))
	}
	targetType, targetID := "spec", sp.Int("id")
	var pr *pinResult
	quoteOK := any(nil)
	sectionLabel := ""
	switch {
	case in.ParentID != 0:
		parent := s.one("SELECT * FROM notes WHERE id = ?", in.ParentID)
		if parent == nil || !s.noteBelongs(sp.Int("id"), parent) {
			fail(errs.NotFound("note", fmt.Sprint(in.ParentID), "no such note on spec %s: %d", sp.Str("slug"), in.ParentID))
		}
		targetType, targetID = parent.Str("target_type"), parent.Int("target_id")
	case in.Section == "" && kind == "build-note":
	case in.Section == "":
		fail(errs.New("a spec comment needs a section (or --reply)"))
	default:
		head, sec := s.oneSection(sp, in.Section)
		pr = s.pinSection(sp, head, sec, in.RepoRoot)
		targetType, targetID = "spec_anchor", pr.anchorID
		if in.Quote != "" {
			st, _ := spec.LocateQuote(pr.section.Str("body"), in.Quote, in.Prefix, in.Suffix)
			quoteOK = int64(0)
			if st == spec.QuoteOK {
				quoteOK = int64(1)
			}
		}
	}
	if targetType == "spec_anchor" {
		if sec := s.anchorHeadSection(sp, targetID); sec != nil {
			sectionLabel = sec.Str("path_slug")
		} else if a := s.one("SELECT key FROM spec_anchors WHERE id = ?", targetID); a != nil {
			sectionLabel = a.Str("key")
		}
	}
	sp = s.specByID(sp.Int("id")) // the pin may have moved the head
	now := refs.NowISO()
	var released any
	if kind == "build-note" || s.releaseMode() == "auto" {
		released = now
	}
	head := s.headVersion(sp)
	id := s.insert(`INSERT INTO notes (uid, target_type, target_id, body, actor, created_at, kind, parent_id, quote, quote_prefix, quote_suffix, quote_ok, version_id, released_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		NewUID(), targetType, targetID, body, nullable(s.Actor), now, kind, nullableID(in.ParentID),
		nullable(in.Quote), nullable(in.Prefix), nullable(in.Suffix), quoteOK, head.Int("id"), released)
	detail := firstLine(body)
	if sectionLabel != "" {
		detail = sectionLabel + ": " + detail
	}
	ev := s.event("note", id, noteAction("spec_anchor", kind), detail, 0, sp.Int("project_id"), now)
	if released != nil {
		s.exec("UPDATE notes SET release_event = ? WHERE id = ?", ev, id)
	}
	pinned := ""
	var write *FileWrite
	if pr != nil {
		if pr.minted || pr.write != nil {
			pinned = pr.key
		}
		write = pr.write
	}
	return s.viewerNote(sp, s.one("SELECT * FROM notes WHERE id = ?", id)), pinned, write
}

// anchorHeadSection is the head section carrying an anchor, or nil.
func (s *Store) anchorHeadSection(sp *row.Row, anchorID int64) *row.Row {
	if sp.IsNull("head_version_id") {
		return nil
	}
	return s.one(sectionCols+" WHERE s.version_id = ? AND s.anchor_id = ?", sp.Int("head_version_id"), anchorID)
}

func (s *Store) noteBelongs(specID int64, n *row.Row) bool {
	for depth := 0; n != nil && depth < 50; depth++ {
		switch n.Str("target_type") {
		case "spec":
			return n.Int("target_id") == specID
		case "spec_anchor":
			return s.one("SELECT id FROM spec_anchors WHERE id = ? AND spec_id = ?", n.Int("target_id"), specID) != nil
		}
		if n.IsNull("parent_id") {
			return false
		}
		n = s.one("SELECT * FROM notes WHERE id = ?", n.Int("parent_id"))
	}
	return false
}

// SpecNoteBelongs reports whether a note hangs off this spec.
func (s *Store) SpecNoteBelongs(specID, noteID int64) (ok bool, err error) {
	defer guard(&err)
	n := s.one("SELECT * FROM notes WHERE id = ?", noteID)
	return n != nil && s.noteBelongs(specID, n), nil
}

// viewerNote is the docs/viewer-api.md note object.
func (s *Store) viewerNote(sp *row.Row, n *row.Row) *row.Row {
	var section, anchor any
	if n.Str("target_type") == "spec_anchor" {
		if a := s.one("SELECT key FROM spec_anchors WHERE id = ?", n.Int("target_id")); a != nil {
			anchor = a.Str("key")
		}
		if sec := s.anchorHeadSection(sp, n.Int("target_id")); sec != nil {
			section = sec.Str("path_slug")
		}
	}
	var quoteOK any
	if !n.IsNull("quote_ok") {
		quoteOK = n.Int("quote_ok") == 1
	}
	return row.Of("id", n.Int("id"), "section", section, "anchor", anchor, "kind", n.Str("kind"), "body", n.Str("body"),
		"actor", n.Get("actor"), "created_at", n.Str("created_at"), "quote", n.Get("quote"), "quote_ok", quoteOK,
		"parent_id", n.Get("parent_id"), "resolved_at", n.Get("resolved_at"), "resolved_by", n.Get("resolved_by"),
		"released", !n.IsNull("released_at"))
}

// SpecNotes lists the comments on a spec (every anchor), or on one section,
// oldest first, threads kept together by the caller via parent_id.
func (s *Store) SpecNotes(specID int64, section string, openOnly bool) (out []*row.Row, err error) {
	defer guard(&err)
	sp := s.specByID(specID)
	var anchorIDs []int64
	if section != "" {
		_, sec := s.oneSection(sp, section)
		if sec.IsNull("anchor_id") {
			return []*row.Row{}, nil
		}
		anchorIDs = []int64{sec.Int("anchor_id")}
	} else {
		for _, a := range s.q("SELECT id FROM spec_anchors WHERE spec_id = ?", specID) {
			anchorIDs = append(anchorIDs, a.Int("id"))
		}
	}
	out = []*row.Row{}
	if len(anchorIDs) == 0 {
		return out, nil
	}
	q := "SELECT * FROM notes WHERE target_type = 'spec_anchor' AND target_id IN (" + marks(len(anchorIDs)) + ")"
	if openOnly {
		q += " AND resolved_at IS NULL"
	}
	for _, n := range s.q(q+" ORDER BY COALESCE(parent_id, id), id", ids64(anchorIDs)...) {
		out = append(out, s.viewerNote(sp, n))
	}
	return out, nil
}

// --- task links ------------------------------------------------------------------------

// SpecLinkKinds are the values of spec_task_links.kind.
var SpecLinkKinds = []string{"implements", "verifies", "documents", "blocked-by"}

// SpecLink links a section to a task, pinning the section lazily and
// stamping what the task agrees to (design §5.7). Linking twice is a no-op.
// The returned FileWrite (pin write-back) must be applied by the caller once
// its transaction commits.
func (s *Store) SpecLink(sectionRef, context string, taskID int64, kind, scope, repoRoot string) (link *row.Row, pinned string, write *FileWrite, err error) {
	defer guard(&err)
	if kind == "" {
		kind = "implements"
	}
	if scope == "" {
		scope = "self"
	}
	ok := false
	for _, k := range SpecLinkKinds {
		ok = ok || k == kind
	}
	if !ok {
		fail(errs.New("invalid link kind: %s (expected one of %s)", kind, strings.Join(SpecLinkKinds, ", ")))
	}
	task := s.getTaskID(taskID, fmt.Sprint(taskID), true)
	s.tx(func() {
		sr, e := ParseSpecRef(sectionRef)
		if e != nil {
			fail(e)
		}
		if sr.Section == "" {
			fail(errs.New("%s names a spec, not a section: use %s#<section>", sectionRef, sectionRef))
		}
		sp := s.resolveSpecRef(sr, context)
		head, sec := s.oneSection(sp, sr.Section)
		pr := s.pinSection(sp, head, sec, repoRoot)
		if pr.minted || pr.write != nil {
			pinned = pr.key
		}
		write = pr.write
		existing := s.one("SELECT * FROM spec_task_links WHERE anchor_id = ? AND task_id = ? AND kind = ? AND scope = ?", pr.anchorID, taskID, kind, scope)
		if existing != nil {
			link = s.linkRow(existing)
			return
		}
		now := refs.NowISO()
		id := s.insert("INSERT INTO spec_task_links (anchor_id, task_id, kind, scope, agreed_version_id, agreed_body_sha, actor, created_at) VALUES (?,?,?,?,?,?,?,?)",
			pr.anchorID, taskID, kind, scope, pr.version.Int("id"), s.linkHash(pr.version.Int("id"), pr.anchorID, scope), nullable(s.Actor), now)
		s.event("task", taskID, "spec-linked", fmt.Sprintf("%s#%s %s (%s, v%d)", sp.Str("slug"), pr.section.Str("path_slug"), kind, scope, pr.version.Int("n")),
			taskID, task.Int("project_id"), now)
		link = s.linkRow(s.one("SELECT * FROM spec_task_links WHERE id = ?", id))
	})
	return link, pinned, write, nil
}

func (s *Store) linkRow(l *row.Row) *row.Row {
	a := s.one("SELECT * FROM spec_anchors WHERE id = ?", l.Int("anchor_id"))
	sp := s.specByID(a.Int("spec_id"))
	section := any(nil)
	if sec := s.anchorHeadSection(sp, a.Int("id")); sec != nil {
		section = sec.Str("path_slug")
	}
	var agreed any
	if !l.IsNull("agreed_version_id") {
		if v := s.one("SELECT n FROM spec_versions WHERE id = ?", l.Int("agreed_version_id")); v != nil {
			agreed = v.Int("n")
		}
	}
	return row.Of("id", l.Int("id"), "spec", sp.Str("slug"), "section", section, "anchor", a.Str("key"), "task_id", l.Int("task_id"),
		"kind", l.Str("kind"), "scope", l.Str("scope"), "agreed_revision", agreed)
}

// SpecUnlink removes a task's links to a section (every kind unless given).
func (s *Store) SpecUnlink(sectionRef, context string, taskID int64, kind string) (removed int, err error) {
	defer guard(&err)
	s.tx(func() {
		sr, e := ParseSpecRef(sectionRef)
		if e != nil {
			fail(e)
		}
		sp := s.resolveSpecRef(sr, context)
		a := s.resolveAnchor(sp, sr.Section)
		q := "DELETE FROM spec_task_links WHERE anchor_id = ? AND task_id = ?"
		args := []any{a.Int("id"), taskID}
		if kind != "" {
			q += " AND kind = ?"
			args = append(args, kind)
		}
		n, _ := s.exec(q, args...).RowsAffected()
		if n == 0 {
			fail(errs.NotFound("link", sectionRef, "task #%d is not linked to %s#%s", taskID, sp.Str("slug"), sr.Section))
		}
		removed = int(n)
		task := s.getTaskID(taskID, fmt.Sprint(taskID), true)
		s.event("task", taskID, "spec-unlinked", fmt.Sprintf("%s#%s", sp.Str("slug"), a.Str("key")), taskID, task.Int("project_id"), "")
	})
	return removed, nil
}

// resolveAnchor finds the anchor a reference names: an anchor key, a head
// section that carries one, or the latest older section at that path that did.
func (s *Store) resolveAnchor(sp *row.Row, ref string) *row.Row {
	if ref == "" {
		fail(errs.New("a section is required"))
	}
	if a := s.one("SELECT * FROM spec_anchors WHERE spec_id = ? AND key = ?", sp.Int("id"), ref); a != nil {
		return a
	}
	if head := s.headVersion(sp); head != nil {
		if sec := s.one(sectionCols+" WHERE s.version_id = ? AND s.path_slug = ? AND s.anchor_id IS NOT NULL", head.Int("id"), ref); sec != nil {
			return s.one("SELECT * FROM spec_anchors WHERE id = ?", sec.Int("anchor_id"))
		}
	}
	if old := s.one("SELECT s.anchor_id FROM spec_sections s JOIN spec_versions v ON v.id = s.version_id WHERE v.spec_id = ? AND s.path_slug = ? AND s.anchor_id IS NOT NULL ORDER BY v.n DESC LIMIT 1",
		sp.Int("id"), ref); old != nil {
		return s.one("SELECT * FROM spec_anchors WHERE id = ?", old.Int("anchor_id"))
	}
	fail(errs.NotFound("section", ref, "%s#%s has no anchor: no note or task link points at it", sp.Str("slug"), ref))
	return nil
}

// restampTaskLinks re-stamps what a finished task agreed to (design §5.7).
func (s *Store) restampTaskLinks(taskID int64) {
	for _, l := range s.q(`SELECT l.id, l.anchor_id, l.scope, sp.head_version_id FROM spec_task_links l
 JOIN spec_anchors a ON a.id = l.anchor_id JOIN specs sp ON sp.id = a.spec_id WHERE l.task_id = ? AND a.state = 'live'`, taskID) {
		if l.IsNull("head_version_id") {
			continue
		}
		h := s.linkHash(l.Int("head_version_id"), l.Int("anchor_id"), l.Str("scope"))
		if h == "" {
			continue
		}
		s.exec("UPDATE spec_task_links SET agreed_version_id = ?, agreed_body_sha = ? WHERE id = ?", l.Int("head_version_id"), h, l.Int("id"))
	}
}

// RestampSpecLinks is restampTaskLinks in its own transaction.
func (s *Store) RestampSpecLinks(taskID int64) (err error) {
	defer guard(&err)
	s.tx(func() { s.restampTaskLinks(taskID) })
	return nil
}

// SpecReview records a review of a task's link to a section: still-valid
// re-stamps the agreement at the reviewed revision; needs-work leaves it
// stale. Either way a decision note on the task says what was decided.
func (s *Store) SpecReview(taskID int64, sectionRef, context, verdict, reason string, version int) (out *row.Row, err error) {
	defer guard(&err)
	task := s.getTaskID(taskID, fmt.Sprint(taskID), true)
	s.tx(func() {
		sr, e := ParseSpecRef(sectionRef)
		if e != nil {
			fail(e)
		}
		sp := s.resolveSpecRef(sr, context)
		a := s.resolveAnchor(sp, sr.Section)
		links := s.q("SELECT * FROM spec_task_links WHERE anchor_id = ? AND task_id = ?", a.Int("id"), taskID)
		if len(links) == 0 {
			fail(errs.NotFound("link", sectionRef, "task #%d is not linked to %s#%s", taskID, sp.Str("slug"), sr.Section))
		}
		v := s.versionN(sp, version)
		restamped := 0
		if verdict == "still-valid" {
			for _, l := range links {
				h := s.linkHash(v.Int("id"), a.Int("id"), l.Str("scope"))
				if h == "" {
					fail(errs.New("%s#%s is not in v%d; relink it first", sp.Str("slug"), a.Str("key"), v.Int("n")))
				}
				s.exec("UPDATE spec_task_links SET agreed_version_id = ?, agreed_body_sha = ? WHERE id = ?", v.Int("id"), h, l.Int("id"))
				restamped++
			}
		}
		body := fmt.Sprintf("spec review %s#%s v%d: %s", sp.Str("slug"), a.Str("key"), v.Int("n"), verdict)
		if reason != "" {
			body += " — " + reason
		}
		nid := s.insertNote("task", taskID, body, "", "decision", 0)
		s.event("task", taskID, "spec-reviewed", body, taskID, task.Int("project_id"), "")
		out = row.Of("task_id", taskID, "section", sp.Str("slug")+"#"+a.Str("key"), "revision", v.Int("n"), "verdict", verdict,
			"restamped", int64(restamped), "note_id", nid)
	})
	return out, nil
}

// SpecStale lists every task link of a spec with its staleness.
func (s *Store) SpecStale(specID int64) (out []*row.Row, err error) {
	defer guard(&err)
	return s.specStale(s.specByID(specID)), nil
}

func (s *Store) specStale(sp *row.Row) []*row.Row {
	out := []*row.Row{}
	head := s.headVersion(sp)
	if head == nil {
		return out
	}
	for _, l := range s.q(`SELECT l.*, a.key, a.state, t.title, t.status, t.claimed_by, t.claim_expires_at FROM spec_task_links l
 JOIN spec_anchors a ON a.id = l.anchor_id JOIN tasks t ON t.id = l.task_id WHERE a.spec_id = ? ORDER BY l.task_id, l.id`, sp.Int("id")) {
		cur := s.linkHash(head.Int("id"), l.Int("anchor_id"), l.Str("scope"))
		agreedN, agreedSHA := int64(0), l.Str("agreed_body_sha")
		var agreedID int64
		if !l.IsNull("agreed_version_id") {
			agreedID = l.Int("agreed_version_id")
			if v := s.one("SELECT n FROM spec_versions WHERE id = ?", agreedID); v != nil {
				agreedN = v.Int("n")
			}
		}
		stale := cur != agreedSHA
		changed := ""
		if stale {
			for _, v := range s.q("SELECT id, imported_at FROM spec_versions WHERE spec_id = ? AND n > ? ORDER BY n", sp.Int("id"), agreedN) {
				if s.linkHash(v.Int("id"), l.Int("anchor_id"), l.Str("scope")) != agreedSHA {
					changed = v.Str("imported_at")
					break
				}
			}
		}
		section := l.Str("key")
		if sec := s.anchorHeadSection(sp, l.Int("anchor_id")); sec != nil {
			section = sec.Str("path_slug")
		}
		out = append(out, row.Of("task_id", l.Int("task_id"), "status", l.Str("status"), "title", l.Str("title"),
			"section", section, "anchor", l.Str("key"), "anchor_state", l.Str("state"), "kind", l.Str("kind"), "scope", l.Str("scope"),
			"agreed_revision", agreedN, "agreed_sha", short(agreedSHA), "head_revision", head.Int("n"), "head_sha", short(cur),
			"stale", stale, "changed_at", nullable(changed), "claimed_by", l.Get("claimed_by"), "claim_expires_at", l.Get("claim_expires_at")))
	}
	return out
}

func short(sha string) string {
	if len(sha) > 4 {
		return sha[:4] + "…"
	}
	return sha
}

// SpecCoverage lists head sections nobody implements: every heading or
// block with text, unless marked coverage_required = 0, and not under a
// subtree link. "Covered" means linked work exists, not that it is done.
func (s *Store) SpecCoverage(specID int64) (out []*row.Row, err error) {
	defer guard(&err)
	return s.specCoverage(s.specByID(specID)), nil
}

func (s *Store) specCoverage(sp *row.Row) []*row.Row {
	out := []*row.Row{}
	head := s.headVersion(sp)
	if head == nil {
		return out
	}
	rows := s.versionSections(head.Int("id"))
	covered := map[int64]bool{}
	for i, r := range rows {
		if r.IsNull("anchor_id") {
			continue
		}
		for _, l := range s.q("SELECT scope FROM spec_task_links WHERE anchor_id = ? AND kind = 'implements'", r.Int("anchor_id")) {
			if l.Str("scope") == "subtree" {
				for _, d := range subtreeRows(rows, i) {
					covered[d.Int("id")] = true
				}
			}
			covered[r.Int("id")] = true
		}
	}
	for _, r := range rows {
		if r.Str("kind") == spec.KindRoot || covered[r.Int("id")] || strings.TrimSpace(r.Str("body")) == "" ||
			(!r.IsNull("coverage_required") && r.Int("coverage_required") == 0) {
			continue
		}
		out = append(out, row.Of("section", r.Str("path_slug"), "number", r.Str("number"), "heading", r.Str("heading"), "pin", r.Get("pin")))
	}
	return out
}

// --- relink / orphans ----------------------------------------------------------------

// SpecRelink moves annotations off a retired or renamed anchor, explicitly
// (design §5.4): merged/renamed/moved move notes and task links onto the one
// target; split copies the task links to every target and leaves the notes
// under the retired identity. The source anchor is retired, and the link
// between them recorded. Targets are pinned lazily, each written to the file
// before the next is pinned.
func (s *Store) SpecRelink(fromRef, context string, into []string, kind, repoRoot string) (out *row.Row, writes []string, err error) {
	defer guard(&err)
	sr, e := ParseSpecRef(fromRef)
	if e != nil {
		fail(e)
	}
	sp := s.resolveSpecRef(sr, context)
	from := s.resolveAnchor(sp, sr.Section)
	switch kind {
	case "merged", "renamed", "moved":
		if len(into) != 1 {
			fail(errs.New("--kind %s takes exactly one --into section", kind))
		}
	case "split":
		if len(into) < 2 {
			fail(errs.New("--split-into takes at least two sections"))
		}
	default:
		fail(errs.New("invalid relink kind: %s (merged, renamed, moved or split)", kind))
	}
	var targets []*row.Row
	for _, ref := range into {
		var pr *pinResult
		s.tx(func() {
			head, sec := s.oneSection(s.specByID(sp.Int("id")), ref)
			pr = s.pinSection(s.specByID(sp.Int("id")), head, sec, repoRoot)
		})
		if pr.write != nil {
			if e := pr.write.Apply(); e != nil {
				fail(&PinWriteError{Rel: pr.write.Rel, Err: e})
			}
			writes = append(writes, pr.key)
		}
		if pr.anchorID == from.Int("id") {
			fail(errs.New("%s and %s are the same section", fromRef, ref))
		}
		targets = append(targets, s.one("SELECT * FROM spec_anchors WHERE id = ?", pr.anchorID))
	}
	var notes, links int64
	s.tx(func() {
		sp = s.specByID(sp.Int("id"))
		now := refs.NowISO()
		for _, to := range targets {
			if kind == "split" {
				n, _ := s.exec(`INSERT OR IGNORE INTO spec_task_links (anchor_id, task_id, subtask_id, kind, scope, agreed_version_id, agreed_body_sha, actor, created_at)
 SELECT ?, task_id, subtask_id, kind, scope, agreed_version_id, agreed_body_sha, ?, ? FROM spec_task_links WHERE anchor_id = ?`,
					to.Int("id"), nullable(s.Actor), now, from.Int("id")).RowsAffected()
				links += n
			} else {
				n, _ := s.exec("UPDATE notes SET target_id = ? WHERE target_type = 'spec_anchor' AND target_id = ?", to.Int("id"), from.Int("id")).RowsAffected()
				notes += n
				n, _ = s.exec("UPDATE OR IGNORE spec_task_links SET anchor_id = ? WHERE anchor_id = ?", to.Int("id"), from.Int("id")).RowsAffected()
				links += n
				s.exec("DELETE FROM spec_task_links WHERE anchor_id = ?", from.Int("id")) // duplicates of existing target links
			}
			s.exec("INSERT OR IGNORE INTO spec_anchor_links (from_anchor, to_anchor, kind, version_id, actor, created_at) VALUES (?,?,?,?,?,?)",
				from.Int("id"), to.Int("id"), kind, sp.Get("head_version_id"), nullable(s.Actor), now)
		}
		if kind == "split" {
			s.exec("DELETE FROM spec_task_links WHERE anchor_id = ?", from.Int("id"))
			s.exec("UPDATE notes SET quote_ok = 0 WHERE target_type = 'spec_anchor' AND target_id = ? AND quote IS NOT NULL", from.Int("id"))
		} else if sec := s.anchorHeadSection(sp, targets[0].Int("id")); sec != nil {
			s.recheckQuotes(targets[0].Int("id"), sec.Str("body")) // moved quotes are checked where they now live
		}
		s.exec("UPDATE spec_anchors SET state = 'retired' WHERE id = ?", from.Int("id"))
		if !sp.IsNull("head_version_id") {
			s.exec("UPDATE spec_sections SET anchor_id = NULL WHERE version_id = ? AND anchor_id = ?", sp.Int("head_version_id"), from.Int("id"))
		}
		var keys []string
		for _, t := range targets {
			keys = append(keys, t.Str("key"))
		}
		s.event("spec", sp.Int("id"), "spec-relinked", fmt.Sprintf("%s#%s %s into %s: %d notes, %d task links", sp.Str("slug"), from.Str("key"), kind, strings.Join(keys, ","), notes, links),
			0, sp.Int("project_id"), now)
		out = row.Of("from", from.Str("key"), "into", keys, "kind", kind, "notes", notes, "links", links)
	})
	return out, writes, nil
}

// SpecOrphans lists anchors that lost their section, with what they hold.
func (s *Store) SpecOrphans(specID int64) (out []*row.Row, err error) {
	defer guard(&err)
	out = []*row.Row{}
	for _, a := range s.q("SELECT a.*, v.n last_n FROM spec_anchors a JOIN spec_versions v ON v.id = a.last_seen_version WHERE a.spec_id = ? AND a.state = 'orphaned' ORDER BY a.key", specID) {
		n, open := s.anchorNoteCounts(a.Int("id"))
		last := s.one("SELECT path_slug, heading FROM spec_sections WHERE version_id = ? AND anchor_id = ?", a.Int("last_seen_version"), a.Int("id"))
		slug, heading := "", ""
		if last != nil {
			slug, heading = last.Str("path_slug"), last.Str("heading")
		}
		out = append(out, row.Of("anchor", a.Str("key"), "section", slug, "heading", heading, "last_seen", a.Int("last_n"),
			"notes", n, "open_notes", open, "tasks", s.anchorTaskIDs(a.Int("id"))))
	}
	return out, nil
}

// --- diff / gc ------------------------------------------------------------------------

// SpecDiff compares two revisions section by section (design §5.9).
func (s *Store) SpecDiff(specID int64, from, to int) (out []*row.Row, err error) {
	defer guard(&err)
	sp := s.specByID(specID)
	va, vb := s.versionN(sp, from), s.versionN(sp, to)
	prior := s.versionSections(va.Int("id"))
	var priors []spec.Prior
	for _, r := range prior {
		priors = append(priors, spec.Prior{Key: r.Str("pin"), Kind: r.Str("kind"), PathSlug: r.Str("path_slug"), OwnSlug: ownSlug(r.Str("path_slug")),
			Heading: r.Str("heading"), Body: r.Str("body"), BodySHA: r.Str("body_sha"), Pinned: !r.IsNull("anchor_id"), Number: r.Str("number")})
	}
	// Match on the anchors vb actually carries, so pins survive renames here too.
	doc := parseVersion(vb)
	next := s.versionSections(vb.Int("id"))
	for i := range doc.Sections {
		if i < len(next) && next[i].Str("pin") != "" {
			doc.Sections[i].PinKey = next[i].Str("pin")
		}
	}
	rep := spec.Reconcile(priors, doc)
	out = []*row.Row{}
	for _, m := range rep.Matches {
		sec := &doc.Sections[m.Ordinal]
		if m.Prior < 0 {
			out = append(out, row.Of("change", "new", "slug", sec.PathSlug, "heading", sec.Heading, "number", sec.Number))
			continue
		}
		pr := prior[m.Prior]
		change := ""
		switch {
		case m.Tier == spec.TierSlug:
			change = "moved"
		case pr.Str("heading") != sec.Heading || pr.Str("path_slug") != sec.PathSlug:
			change = "renamed"
		case pr.Str("body_sha") != sec.BodySHA:
			change = "changed"
		}
		if change == "" && pr.Str("body_sha") == sec.BodySHA {
			continue
		}
		r := row.Of("change", change, "slug", sec.PathSlug, "prior_slug", pr.Str("path_slug"), "heading", sec.Heading, "prior_heading", pr.Str("heading"),
			"number", sec.Number, "prior_number", pr.Str("number"), "tier", m.Tier)
		if m.Tier == spec.TierSimilar {
			r.Set("confidence", m.Confidence)
		}
		if pr.Str("body_sha") != sec.BodySHA {
			r.Set("diff", spec.UnifiedDiff(pr.Str("body"), sec.Body, fmt.Sprintf("v%d", va.Int("n")), fmt.Sprintf("v%d", vb.Int("n")), 0))
		}
		out = append(out, r)
	}
	for _, l := range rep.Removed {
		pr := prior[l.Prior]
		out = append(out, row.Of("change", "removed", "slug", pr.Str("path_slug"), "heading", pr.Str("heading")))
	}
	for _, l := range rep.Orphaned {
		pr := prior[l.Prior]
		out = append(out, row.Of("change", "orphaned", "slug", pr.Str("path_slug"), "anchor", pr.Str("pin"), "revision", vb.Int("n")))
	}
	return out, nil
}

// SpecGC deletes all but the newest keep revisions, never the head and never
// one that anything references (agreed links, notes, anchors, anchor links).
func (s *Store) SpecGC(specID int64, keep int) (deleted []any, err error) {
	defer guard(&err)
	if keep < 1 {
		fail(errs.New("--keep must be at least 1"))
	}
	sp := s.specByID(specID)
	deleted = []any{}
	s.tx(func() {
		for _, v := range s.q(`SELECT id, n FROM spec_versions v WHERE spec_id = ? AND n <= (SELECT MAX(n) FROM spec_versions WHERE spec_id = ?) - ?
 AND id NOT IN (SELECT head_version_id FROM specs WHERE head_version_id IS NOT NULL)
 AND id NOT IN (SELECT agreed_version_id FROM spec_task_links WHERE agreed_version_id IS NOT NULL)
 AND id NOT IN (SELECT version_id FROM notes WHERE version_id IS NOT NULL)
 AND id NOT IN (SELECT pinned_at_version FROM spec_anchors UNION SELECT first_seen_version FROM spec_anchors UNION SELECT last_seen_version FROM spec_anchors)
 AND id NOT IN (SELECT version_id FROM spec_anchor_links WHERE version_id IS NOT NULL)
 AND id NOT IN (SELECT base_version_id FROM spec_pending WHERE base_version_id IS NOT NULL)
 ORDER BY n`, specID, specID, keep) {
			s.exec("DELETE FROM spec_versions WHERE id = ?", v.Int("id"))
			deleted = append(deleted, v.Int("n"))
		}
		if len(deleted) > 0 {
			s.event("spec", specID, "spec-gc", fmt.Sprintf("%s: dropped %d revision(s)", sp.Str("slug"), len(deleted)), 0, sp.Int("project_id"), "")
		}
	})
	return deleted, nil
}
