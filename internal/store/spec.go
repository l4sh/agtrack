package store

// The spec entity (design §5): markdown documents imported as immutable
// versions, addressed by section, annotated through sparse anchors.
//
// Prose flows file → DB (add/sync); pins flow DB → file, one comment line at
// a time, only when a note or link first needs one (spec_notes.go).

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/spec"
)

// SpecNoteInput is a comment created from the viewer or `spec note`.
type SpecNoteInput struct {
	Section, Quote, Prefix, Suffix, Kind, Body string
	ParentID                                   int64
	RepoRoot                                   string
}

// SpecStatuses are the values of specs.status.
var SpecStatuses = []string{"draft", "active", "superseded", "archived"}

// --- references ----------------------------------------------------------------

// SpecRef is a parsed `[project/]slug[@N][#section]`.
type SpecRef struct {
	Project string // project ref, "" = resolve by slug in the context
	Slug    string
	Version int // 0 = head
	Section string
}

var versionRe = regexp.MustCompile(`^v?(\d+)$`)

// ParseSpecRef splits a spec or section reference. `#` separates the
// section; `@N` (or `@vN`) pins a revision; everything before the last `/`
// is the project reference.
func ParseSpecRef(ref string) (SpecRef, error) {
	var r SpecRef
	body := strings.TrimSpace(ref)
	if i := strings.IndexByte(body, '#'); i >= 0 {
		r.Section = body[i+1:]
		body = body[:i]
		if r.Section == "" {
			return r, errs.New("empty section in %s: use spec#section", ref)
		}
	}
	if i := strings.LastIndexByte(body, '@'); i >= 0 {
		m := versionRe.FindStringSubmatch(body[i+1:])
		if m == nil {
			return r, errs.New("invalid revision in %s: use spec@N", ref)
		}
		r.Version, _ = strconv.Atoi(m[1])
		body = body[:i]
	}
	if i := strings.LastIndexByte(body, '/'); i >= 0 {
		r.Project = body[:i]
		body = body[i+1:]
	}
	if body == "" {
		return r, errs.New("invalid spec reference: %s", ref)
	}
	r.Slug = body
	return r, nil
}

func (s *Store) resolveSpecRef(r SpecRef, context string) *row.Row {
	label := r.Slug
	if r.Project != "" {
		p := s.resolveProject(r.Project, context, true)
		sp := s.one("SELECT * FROM specs WHERE project_id = ? AND slug = ?", p.Int("id"), r.Slug)
		if sp == nil {
			fail(errs.NotFound("spec", r.Project+"/"+r.Slug, "no such spec: %s/%s", s.shortPath(p), r.Slug))
		}
		return sp
	}
	// Unqualified: the current (or cwd) project wins, else unique in the context.
	var cur *row.Row
	if p := s.currentProject(); p != nil {
		cur = p
	} else if remote := s.CwdRepo(); remote != "" {
		if m := s.findProjectsByRepo(remote); len(m) == 1 {
			cur = m[0]
		}
	}
	if cur != nil {
		if sp := s.one("SELECT * FROM specs WHERE project_id = ? AND slug = ?", cur.Int("id"), r.Slug); sp != nil {
			return sp
		}
	}
	ctx := s.contextOrDefault(context)
	rows := s.q("SELECT sp.* FROM specs sp JOIN projects p ON p.id = sp.project_id WHERE p.context_id = ? AND sp.slug = ? ORDER BY sp.id",
		ctx.Int("id"), r.Slug)
	switch len(rows) {
	case 0:
		fail(errs.NotFound("spec", label, "no such spec: %s", label))
	case 1:
		return rows[0]
	}
	var cands []string
	for _, sp := range rows {
		cands = append(cands, s.specRef(sp))
	}
	fail(&errs.Error{Code: errs.ExitError, Token: errs.TokAmbiguous, Entity: "spec", Ref: label,
		Message: fmt.Sprintf("spec %s exists in several projects: %s", label, strings.Join(cands, ", ")),
		Extra:   map[string]any{"candidates": cands}})
	return nil
}

// ResolveSpec resolves a spec reference (any `@N`/`#section` part is
// ignored) to its row, as SpecInfo renders it.
func (s *Store) ResolveSpec(ref, context string) (r *row.Row, err error) {
	defer guard(&err)
	sr, e := ParseSpecRef(ref)
	if e != nil {
		fail(e)
	}
	return s.specInfo(s.resolveSpecRef(sr, context)), nil
}

// specRef is `project[:sub]/slug`, the form that resolves in any project.
func (s *Store) specRef(sp *row.Row) string {
	p := s.one("SELECT * FROM projects WHERE id = ?", sp.Int("project_id"))
	return s.shortPath(projectRow(p)) + "/" + sp.Str("slug")
}

// specInfo is the public shape of a spec row.
func (s *Store) specInfo(sp *row.Row) *row.Row {
	out := row.Of("id", sp.Int("id"), "slug", sp.Str("slug"), "ref", s.specRef(sp), "title", sp.Get("title"),
		"status", sp.Str("status"), "source_path", sp.Get("source_path"), "repo_root", sp.Get("repo_root"))
	revision, sha := int64(0), ""
	if h := s.headVersion(sp); h != nil {
		revision, sha = h.Int("n"), h.Str("sha256")
	}
	p := s.one("SELECT * FROM projects WHERE id = ?", sp.Int("project_id"))
	out.Set("revision", revision)
	out.Set("sha", sha)
	out.Set("project", s.shortPath(projectRow(p)))
	out.Set("project_id", sp.Int("project_id"))
	out.Set("created_at", sp.Str("created_at"))
	out.Set("updated_at", sp.Str("updated_at"))
	return out
}

func (s *Store) specByID(id int64) *row.Row {
	sp := s.one("SELECT * FROM specs WHERE id = ?", id)
	if sp == nil {
		fail(errs.NotFound("spec", fmt.Sprint(id), "no such spec: %d", id))
	}
	return sp
}

func (s *Store) headVersion(sp *row.Row) *row.Row {
	if sp.IsNull("head_version_id") {
		return nil
	}
	return s.one("SELECT * FROM spec_versions WHERE id = ?", sp.Int("head_version_id"))
}

func (s *Store) versionN(sp *row.Row, n int) *row.Row {
	if n == 0 {
		if h := s.headVersion(sp); h != nil {
			return h
		}
		fail(errs.NotFound("spec", sp.Str("slug"), "spec %s has no revision yet", sp.Str("slug")))
	}
	v := s.one("SELECT * FROM spec_versions WHERE spec_id = ? AND n = ?", sp.Int("id"), n)
	if v == nil {
		fail(errs.NotFound("spec", fmt.Sprintf("%s@%d", sp.Str("slug"), n), "no such revision: %s@%d", sp.Str("slug"), n))
	}
	return v
}

// sectionCols reads a section with its anchor key.
const sectionCols = "SELECT s.*, a.key AS pin, a.state AS anchor_state FROM spec_sections s LEFT JOIN spec_anchors a ON a.id = s.anchor_id"

func (s *Store) versionSections(versionID int64) []*row.Row {
	return s.q(sectionCols+" WHERE s.version_id = ? ORDER BY s.ordinal", versionID)
}

func (s *Store) sectionByOrdinal(versionID int64, ordinal int) *row.Row {
	return s.one(sectionCols+" WHERE s.version_id = ? AND s.ordinal = ?", versionID, ordinal)
}

func parseVersion(v *row.Row) *spec.Doc {
	d, err := spec.Parse([]byte(v.Str("body")))
	if err != nil {
		fail(err) // cannot happen: the body parsed when it was imported
	}
	return d
}

// ownSlug is the last segment of a path slug.
func ownSlug(pathSlug string) string {
	if i := strings.LastIndex(pathSlug, "__"); i >= 0 {
		return pathSlug[i+2:]
	}
	return pathSlug
}

// Retired is the not-found error for a section that existed once: its Extra
// carries what `spec show` prints instead of a body (design §5.4).
func (s *Store) retiredErr(sp *row.Row, ref string, anchor *row.Row, lastSeen *row.Row) *errs.Error {
	extra := map[string]any{"state": "retired", "spec": sp.Str("slug")}
	msg := fmt.Sprintf("section %s#%s is not in the head revision", sp.Str("slug"), ref)
	if lastSeen != nil {
		extra["last_seen"] = lastSeen.Int("n")
		extra["content"] = fmt.Sprintf("%s@%d#%s", sp.Str("slug"), lastSeen.Int("n"), ref)
		msg += fmt.Sprintf(" (last seen v%d; content: %s)", lastSeen.Int("n"), extra["content"])
	}
	if anchor != nil {
		extra["anchor"] = anchor.Str("key")
		extra["state"] = anchor.Str("state")
		n, open := s.anchorNoteCounts(anchor.Int("id"))
		extra["notes"], extra["open_notes"] = n, open
		extra["tasks"] = s.anchorTaskIDs(anchor.Int("id"))
		var succ []string
		for _, l := range s.q("SELECT a.key, l.kind FROM spec_anchor_links l JOIN spec_anchors a ON a.id = l.to_anchor WHERE l.from_anchor = ? ORDER BY a.key", anchor.Int("id")) {
			succ = append(succ, l.Str("key")+" ("+l.Str("kind")+")")
		}
		extra["successors"] = succ
	}
	return &errs.Error{Code: errs.ExitNotFound, Token: errs.TokNotFound, Entity: "section", Ref: ref, Message: msg, Extra: extra}
}

// lookupSections resolves a section reference within one version. An
// anchor key resolves first (it survives renames); then the parser's
// addressing (pin in file, path slug, glob, positional number, suffix).
func (s *Store) lookupSections(sp, ver *row.Row, ref string) (secs []*row.Row, positional bool) {
	if a := s.one("SELECT * FROM spec_anchors WHERE spec_id = ? AND key = ?", sp.Int("id"), ref); a != nil {
		if sec := s.one(sectionCols+" WHERE s.version_id = ? AND s.anchor_id = ?", ver.Int("id"), a.Int("id")); sec != nil {
			return []*row.Row{sec}, false
		}
		last := s.one("SELECT * FROM spec_versions WHERE id = ?", a.Int("last_seen_version"))
		fail(s.retiredErr(sp, ref, a, last))
	}
	d := parseVersion(ver)
	found, pos, err := d.Lookup(ref)
	if err != nil {
		if errs.IsNotFound(err) {
			// A section that existed in an older revision is informative, not missing.
			if old := s.one(sectionCols+" JOIN spec_versions v ON v.id = s.version_id WHERE v.spec_id = ? AND s.path_slug = ? ORDER BY v.n DESC LIMIT 1",
				sp.Int("id"), ref); old != nil {
				var a *row.Row
				if !old.IsNull("anchor_id") {
					a = s.one("SELECT * FROM spec_anchors WHERE id = ?", old.Int("anchor_id"))
				}
				fail(s.retiredErr(sp, ref, a, s.one("SELECT * FROM spec_versions WHERE id = ?", old.Int("version_id"))))
			}
			e, _ := errs.As(err)
			e.Message = fmt.Sprintf("no such section: %s#%s", sp.Str("slug"), ref)
		}
		fail(err)
	}
	for _, f := range found {
		secs = append(secs, s.sectionByOrdinal(ver.Int("id"), f.Ordinal))
	}
	return secs, pos
}

// SectionHit is a resolved section reference.
type SectionHit struct {
	Spec       *row.Row // raw specs row
	Version    *row.Row
	Sections   []*row.Row
	Positional bool
}

// ResolveSection resolves `spec#section` (optionally `spec@N#section`).
func (s *Store) ResolveSection(ref, context string) (h *SectionHit, err error) {
	defer guard(&err)
	return s.resolveSection(ref, context), nil
}

func (s *Store) resolveSection(ref, context string) *SectionHit {
	sr, e := ParseSpecRef(ref)
	if e != nil {
		fail(e)
	}
	if sr.Section == "" {
		fail(errs.New("%s names a spec, not a section: use %s#<section>", ref, ref))
	}
	sp := s.resolveSpecRef(sr, context)
	ver := s.versionN(sp, sr.Version)
	secs, pos := s.lookupSections(sp, ver, sr.Section)
	return &SectionHit{Spec: sp, Version: ver, Sections: secs, Positional: pos}
}

// --- files and git -------------------------------------------------------------------

func gitOut(dir string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimRight(string(out), "\n"), true
}

func realPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	return abs
}

// locate splits a file path into (repo root, repo-relative path). Outside a
// git repository the root is the file's directory.
func locate(path string) (root, rel string, inGit bool) {
	abs := realPath(path)
	dir := filepath.Dir(abs)
	if top, ok := gitOut(dir, "rev-parse", "--show-toplevel"); ok && top != "" {
		top = realPath(top)
		if r, err := filepath.Rel(top, abs); err == nil && !strings.HasPrefix(r, "..") {
			return top, filepath.ToSlash(r), true
		}
	}
	return dir, filepath.Base(abs), false
}

// GitToplevel is the repository root containing dir, or "".
func GitToplevel(dir string) string {
	top, ok := gitOut(dir, "rev-parse", "--show-toplevel")
	if !ok {
		return ""
	}
	return realPath(top)
}

type gitMeta struct{ commit, blob string }

func isGitRepo(root string) bool {
	_, ok := gitOut(root, "rev-parse", "--git-dir")
	return ok
}

// gitState reports whether rel has uncommitted changes (untracked counts),
// and when clean, the commit and blob it matches.
func gitState(root, rel string) (dirty bool, meta gitMeta) {
	if !isGitRepo(root) {
		return false, meta
	}
	st, ok := gitOut(root, "status", "--porcelain", "--", rel)
	if !ok {
		return false, meta
	}
	if strings.TrimSpace(st) != "" {
		return true, meta
	}
	meta.commit, _ = gitOut(root, "rev-parse", "HEAD")
	meta.blob, _ = gitOut(root, "rev-parse", "HEAD:"+rel)
	return false, meta
}

// specFile is where a spec's source lives: under repoRoot (the caller's
// worktree) when that is the spec's own repository — the recorded root,
// another worktree of it, or a clone whose origin is linked to the spec's
// project — and the file exists there; otherwise under the recorded
// repo_root. An unrelated repository with a file at the same relative path
// is never read.
func (s *Store) specFile(sp *row.Row, repoRoot string) (root, path string) {
	rel := sp.Str("source_path")
	if rel == "" {
		fail(errs.New("spec %s has no source file", sp.Str("slug")))
	}
	if repoRoot != "" && s.sameRepo(sp, repoRoot) {
		p := filepath.Join(repoRoot, filepath.FromSlash(rel))
		if _, err := os.Stat(p); err == nil {
			return repoRoot, p
		}
	}
	root = sp.Str("repo_root")
	return root, filepath.Join(root, filepath.FromSlash(rel))
}

// SpecFileRoot is the checkout a spec's file is read from and pinned in when
// the invoking checkout is repoRoot: repoRoot itself when the file is in it,
// else the root recorded with the spec.
func (s *Store) SpecFileRoot(sp *row.Row, repoRoot string) (root string, err error) {
	defer guard(&err)
	root, _ = s.specFile(sp, repoRoot)
	return root, nil
}

func gitCommonDir(dir string) string {
	d, ok := gitOut(dir, "rev-parse", "--git-common-dir")
	if !ok || d == "" {
		return ""
	}
	if !filepath.IsAbs(d) {
		d = filepath.Join(dir, d)
	}
	return realPath(d)
}

func (s *Store) sameRepo(sp *row.Row, repoRoot string) bool {
	recorded := sp.Str("repo_root")
	if recorded != "" && realPath(repoRoot) == realPath(recorded) {
		return true
	}
	if a, b := gitCommonDir(repoRoot), gitCommonDir(recorded); a != "" && a == b {
		return true
	}
	if remote, ok := gitOut(repoRoot, "remote", "get-url", "origin"); ok && remote != "" {
		for _, p := range s.findProjectsByRepo(remote) {
			if p.Int("id") == sp.Int("project_id") {
				return true
			}
		}
	}
	return false
}

// committedBlob is the file's content at HEAD, if it is tracked.
func committedBlob(root, rel string) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", root, "show", "HEAD:"+rel).Output()
	if err != nil {
		return nil, false
	}
	return out, true
}

func readFile(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fail(errs.NotFound("file", path, "file not found: %s", path))
		}
		fail(errs.New("cannot read %s: %v", path, err))
	}
	return b
}

// --- import ---------------------------------------------------------------------------

// ImportResult describes one add/sync.
type ImportResult struct {
	Spec         *row.Row // SpecInfo shape after the import
	Changed      bool     // a new version was (or, dry run, would be) written
	Version      int64    // revision number now at head (or that would be)
	Base         int64    // revision the import reconciled against (0 for add)
	Sections     int
	Anchors      int // anchors written into the file (always 0: add/sync never write)
	Lines        []*row.Row
	Counts       *row.Row // new, renamed, moved, orphaned, removed, unpinned
	Warnings     []string
	HeldOrphans  int // orphaned anchors that hold notes or links
	Weak         int // similar matches below 0.9 (for --strict)
	Restored     int // pins restored to the file (failed write-back recovery)
	File         string
	Path         string // the file actually read
	Root         string // the repository root it was read under
	SHA, BaseSHA string
}

type plan struct {
	doc      *spec.Doc
	report   *spec.Report
	anchorOf map[int]int64 // ordinal -> anchor id
	matchOf  map[int]spec.Match
	orphaned []*row.Row // anchors becoming orphaned
	revived  map[int64]bool
	lines    []*row.Row
	counts   *row.Row
	held     int
	weak     int
}

func (s *Store) anchorNoteCounts(anchorID int64) (n, open int64) {
	r := s.one("SELECT COUNT(*) n, SUM(CASE WHEN resolved_at IS NULL THEN 1 ELSE 0 END) open FROM notes WHERE target_type = 'spec_anchor' AND target_id = ?", anchorID)
	return r.Int("n"), r.Int("open")
}

// anchorTaskIDs lists linked task ids ([]any of int64: the value types a
// row may hold).
func (s *Store) anchorTaskIDs(anchorID int64) []any {
	out := []any{}
	for _, r := range s.q("SELECT DISTINCT task_id FROM spec_task_links WHERE anchor_id = ? ORDER BY task_id", anchorID) {
		out = append(out, r.Int("task_id"))
	}
	return out
}

// planImport reconciles doc against the head version without writing.
func (s *Store) planImport(sp *row.Row, head *row.Row, doc *spec.Doc) *plan {
	p := &plan{doc: doc, anchorOf: map[int]int64{}, matchOf: map[int]spec.Match{}, revived: map[int64]bool{}}
	anchors := map[string]*row.Row{}
	for _, a := range s.q("SELECT * FROM spec_anchors WHERE spec_id = ?", sp.Int("id")) {
		anchors[a.Str("key")] = a
	}
	var priorRows []*row.Row
	var priors []spec.Prior
	if head != nil {
		priorRows = s.versionSections(head.Int("id"))
		for _, r := range priorRows {
			priors = append(priors, spec.Prior{Key: r.Str("pin"), Kind: r.Str("kind"), PathSlug: r.Str("path_slug"),
				OwnSlug: ownSlug(r.Str("path_slug")), Heading: r.Str("heading"), Body: r.Str("body"),
				BodySHA: r.Str("body_sha"), Pinned: !r.IsNull("anchor_id"), Number: r.Str("number")})
		}
	}
	rep := spec.Reconcile(priors, doc)
	p.report = rep
	used := map[int64]bool{}
	for _, m := range rep.Matches {
		p.matchOf[m.Ordinal] = m
		sec := &doc.Sections[m.Ordinal]
		var aid int64
		switch {
		case m.Tier == spec.TierPinned:
			aid = priorRows[m.Prior].Int("anchor_id")
		case m.Tier == spec.TierPath && m.Prior >= 0 && !priorRows[m.Prior].IsNull("anchor_id"):
			aid = priorRows[m.Prior].Int("anchor_id") // pin comment gone, same path: kept
		}
		if aid == 0 && sec.PinKey != "" {
			// A pin in the file naming an orphaned anchor revives it.
			if a := anchors[sec.PinKey]; a != nil && a.Str("state") != "retired" {
				aid = a.Int("id")
				if a.Str("state") == "orphaned" {
					p.revived[aid] = true
				}
			}
		}
		if aid != 0 && !used[aid] {
			p.anchorOf[m.Ordinal] = aid
			used[aid] = true
		}
	}
	counts := map[string]int64{}
	add := func(kind string, r *row.Row) {
		r.Set("change", kind)
		p.lines = append(p.lines, r)
		counts[kind]++
	}
	priorLine := func(pi int) *row.Row {
		pr := priorRows[pi]
		return row.Of("slug", pr.Str("path_slug"), "heading", pr.Get("heading"), "number", pr.Get("number"), "key", pr.Get("pin"))
	}
	for _, l := range rep.Renamed {
		r := priorLine(l.Prior)
		r.Set("new_slug", l.PathSlug)
		r.Set("new_heading", l.Heading)
		r.Set("confidence", l.Confidence)
		r.Set("tier", l.Tier)
		if l.Tier == spec.TierSimilar && l.Confidence < 0.9 {
			p.weak++
		}
		add("renamed", r)
	}
	for _, l := range rep.Moved {
		r := priorLine(l.Prior)
		r.Set("new_slug", l.PathSlug)
		r.Set("new_number", l.Number)
		add("moved", r)
	}
	for _, l := range rep.New {
		add("new", row.Of("slug", l.PathSlug, "heading", l.Heading, "number", l.Number, "key", nullable(l.Key)))
	}
	for _, l := range rep.PinDropped {
		if l.Tier == spec.TierPath {
			r := priorLine(l.Prior)
			add("unpinned", r)
		}
	}
	for _, l := range rep.Removed {
		add("removed", priorLine(l.Prior))
	}
	// Every live anchor not placed in the new version is orphaned — including
	// pinned sections matched only at tier 3/4: durable state never follows
	// a guess silently.
	likely := map[int64]string{}
	for _, l := range rep.PinDropped {
		if l.Tier != spec.TierPath {
			likely[priorRows[l.Prior].Int("anchor_id")] = l.PathSlug
		}
	}
	for _, a := range anchors {
		if used[a.Int("id")] || a.Str("state") != "live" {
			continue
		}
		p.orphaned = append(p.orphaned, a)
		n, open := s.anchorNoteCounts(a.Int("id"))
		tasks := s.anchorTaskIDs(a.Int("id"))
		slug := ""
		for _, pr := range priorRows {
			if pr.Int("anchor_id") == a.Int("id") {
				slug = pr.Str("path_slug")
			}
		}
		r := row.Of("slug", slug, "key", a.Str("key"), "notes", n, "open_notes", open, "tasks", tasks)
		if l := likely[a.Int("id")]; l != "" {
			r.Set("likely", l)
		}
		if n > 0 || len(tasks) > 0 {
			p.held++
		}
		add("orphaned", r)
	}
	for aid := range p.revived {
		for _, a := range anchors {
			if a.Int("id") == aid {
				add("revived", row.Of("key", a.Str("key")))
			}
		}
	}
	p.counts = row.New()
	for _, k := range []string{"new", "renamed", "moved", "orphaned", "removed", "unpinned", "revived"} {
		p.counts.Set(k, counts[k])
	}
	return p
}

// writeVersion stores doc as the next version and moves the head.
func (s *Store) writeVersion(sp *row.Row, head *row.Row, p *plan, meta gitMeta, now string) *row.Row {
	n := int64(1)
	if head != nil {
		n = head.Int("n") + 1
	}
	doc := p.doc
	var fm any
	if doc.FrontMatter != nil {
		fm = doc.FrontMatter.Raw
	}
	vid := s.insert("INSERT INTO spec_versions (spec_id, n, sha256, git_blob, git_commit, body, front_matter, parser_version, imported_at, actor) VALUES (?,?,?,?,?,?,?,?,?,?)",
		sp.Int("id"), n, doc.SHA, nullable(meta.blob), nullable(meta.commit), string(doc.Text), fm, spec.ParserVersion, now, nullable(s.Actor))
	ids := map[int]int64{}
	for i := range doc.Sections {
		sec := &doc.Sections[i]
		m := p.matchOf[sec.Ordinal]
		var parent, anchor, conf any
		if sec.ParentOrdinal >= 0 {
			parent = ids[sec.ParentOrdinal]
		}
		if aid := p.anchorOf[sec.Ordinal]; aid != 0 {
			anchor = aid
		}
		if m.Tier == spec.TierSimilar {
			conf = m.Confidence
		}
		mermaid := int64(0)
		if sec.HasMermaid {
			mermaid = 1
		}
		ids[sec.Ordinal] = s.insert(`INSERT INTO spec_sections (version_id, anchor_id, parent_id, ordinal, level, kind, number, heading, path, path_slug,
 body, body_sha, context_sha, line_start, line_end, byte_start, byte_end, has_mermaid, match, confidence) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			vid, anchor, parent, sec.Ordinal, sec.Level, sec.Kind, nullable(sec.Number), nullable(sec.Heading), sec.Path, sec.PathSlug,
			sec.Body, sec.BodySHA, sec.ContextSHA, sec.LineStart, sec.LineEnd, sec.ByteStart, sec.ByteEnd, mermaid, m.Tier, conf)
		// Carried identity: the anchor is seen again, and its quotes re-checked.
		if aid := p.anchorOf[sec.Ordinal]; aid != 0 {
			s.exec("UPDATE spec_anchors SET state = 'live', last_seen_version = ? WHERE id = ?", vid, aid)
			s.recheckQuotes(aid, sec.Body)
		}
	}
	for _, a := range p.orphaned {
		s.exec("UPDATE spec_anchors SET state = 'orphaned' WHERE id = ?", a.Int("id"))
		// Its section is gone, so its quotes are not present anywhere (§5.6).
		s.exec("UPDATE notes SET quote_ok = 0 WHERE target_type = 'spec_anchor' AND target_id = ? AND quote IS NOT NULL", a.Int("id"))
	}
	title := sp.Get("title")
	if title == nil && doc.Title != "" {
		title = doc.Title
	}
	s.exec("UPDATE specs SET head_version_id = ?, title = ?, updated_at = ? WHERE id = ?", vid, title, now, sp.Int("id"))
	return s.one("SELECT * FROM spec_versions WHERE id = ?", vid)
}

func (s *Store) recheckQuotes(anchorID int64, body string) {
	for _, n := range s.q("SELECT id, quote, quote_prefix, quote_suffix FROM notes WHERE target_type = 'spec_anchor' AND target_id = ? AND quote IS NOT NULL", anchorID) {
		st, _ := spec.LocateQuote(body, n.Str("quote"), n.Str("quote_prefix"), n.Str("quote_suffix"))
		ok := int64(0)
		if st == spec.QuoteOK {
			ok = 1
		}
		s.exec("UPDATE notes SET quote_ok = ? WHERE id = ?", ok, n.Int("id"))
	}
}

// linkHash is what a task link agrees to: the section's context hash (its
// ancestors' headings and its own text), or for a subtree link, the context
// hashes of the whole subtree. "" when the anchor is not in the version.
func (s *Store) linkHash(versionID, anchorID int64, scope string) string {
	rows := s.versionSections(versionID)
	idx := -1
	for i, r := range rows {
		if r.Int("anchor_id") == anchorID && !r.IsNull("anchor_id") {
			idx = i
		}
	}
	if idx < 0 {
		return ""
	}
	if scope != "subtree" {
		return rows[idx].Str("context_sha")
	}
	var parts []string
	for _, r := range subtreeRows(rows, idx) {
		parts = append(parts, r.Str("context_sha"))
	}
	return spec.SHA([]byte(strings.Join(parts, "\n")))
}

// subtreeRows is rows[idx] and its descendants (rows ordered by ordinal).
func subtreeRows(rows []*row.Row, idx int) []*row.Row {
	in := map[int64]bool{rows[idx].Int("id"): true}
	out := []*row.Row{rows[idx]}
	for _, r := range rows[idx+1:] {
		if !r.IsNull("parent_id") && in[r.Int("parent_id")] {
			in[r.Int("id")] = true
			out = append(out, r)
		}
	}
	return out
}

// driftEvents records `spec-drift` on every task whose linked section
// changed between two versions.
func (s *Store) driftEvents(sp *row.Row, from, to *row.Row) {
	if from == nil {
		return
	}
	for _, l := range s.q(`SELECT l.*, a.key, t.project_id FROM spec_task_links l JOIN spec_anchors a ON a.id = l.anchor_id
 JOIN tasks t ON t.id = l.task_id WHERE a.spec_id = ? ORDER BY l.id`, sp.Int("id")) {
		before := s.linkHash(from.Int("id"), l.Int("anchor_id"), l.Str("scope"))
		after := s.linkHash(to.Int("id"), l.Int("anchor_id"), l.Str("scope"))
		if before == after {
			continue
		}
		agreed := ""
		if !l.IsNull("agreed_version_id") {
			if v := s.one("SELECT n FROM spec_versions WHERE id = ?", l.Int("agreed_version_id")); v != nil {
				agreed = fmt.Sprintf(" (agreed v%d)", v.Int("n"))
			}
		}
		what := "changed"
		if after == "" {
			what = "orphaned"
		}
		s.event("task", l.Int("task_id"), "spec-drift", fmt.Sprintf("%s#%s %s in v%d%s", sp.Str("slug"), l.Str("key"), what, to.Int("n"), agreed),
			l.Int("task_id"), l.Int("project_id"), "")
	}
}

// --- add / sync / status ---------------------------------------------------------------

// SpecAddInput are the inputs of AddSpec.
type SpecAddInput struct {
	File, Slug, Title string
	Project           *row.Row
}

// AddSpec imports a markdown file as v1 of a new spec. The file is never
// modified: zero anchors are written.
func (s *Store) AddSpec(in SpecAddInput) (res *ImportResult, err error) {
	defer guard(&err)
	src := readFile(in.File)
	doc, e := spec.Parse(src)
	if e != nil {
		fail(e)
	}
	slug := in.Slug
	if slug == "" {
		base := filepath.Base(in.File)
		slug = strings.TrimSuffix(base, filepath.Ext(base))
	}
	slug, e = refs.Slugify(slug)
	if e != nil {
		fail(e)
	}
	root, rel, _ := locate(in.File)
	_, meta := gitState(root, rel)
	status := "draft"
	if doc.FrontMatter != nil {
		for _, st := range SpecStatuses {
			if doc.FrontMatter.Fields["status"] == st {
				status = st
			}
		}
	}
	title := in.Title
	if title == "" {
		title = doc.Title
	}
	res = &ImportResult{Changed: true, Version: 1, File: rel, SHA: doc.SHA, Warnings: doc.Warnings, Sections: len(doc.Sections)}
	s.tx(func() {
		if s.one("SELECT id FROM specs WHERE project_id = ? AND slug = ?", in.Project.Int("id"), slug) != nil {
			fail(errs.Conflict("spec already exists: %s/%s", s.shortPath(in.Project), slug))
		}
		now := refs.NowISO()
		id := s.insert("INSERT INTO specs (uid, project_id, slug, title, source_path, repo_root, status, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?)",
			NewUID(), in.Project.Int("id"), slug, nullable(title), rel, root, status, now, now)
		sp := s.one("SELECT * FROM specs WHERE id = ?", id)
		p := s.planImport(sp, nil, doc)
		s.writeVersion(sp, nil, p, meta, now)
		s.event("spec", id, "spec-added", fmt.Sprintf("%s v1 (%s, %d sections)", slug, rel, len(doc.Sections)), 0, in.Project.Int("id"), now)
		res.Counts = p.counts
		res.Spec = s.specInfo(s.one("SELECT * FROM specs WHERE id = ?", id))
	})
	return res, nil
}

// SpecSyncInput are the inputs of SyncSpec.
type SpecSyncInput struct {
	DryRun, Force, Strict bool
	RepoRoot              string // cwd's git toplevel, to find the file in this worktree
}

// SyncSpec re-imports a spec's file. Identical content is a no-op; anything
// else becomes the next version, reconciled against head (design §5.4).
func (s *Store) SyncSpec(specID int64, in SpecSyncInput) (res *ImportResult, err error) {
	defer guard(&err)
	sp := s.specByID(specID)
	root, path := s.specFile(sp, in.RepoRoot)
	rel := sp.Str("source_path")
	src := readFile(path)
	head := s.headVersion(sp)
	res = &ImportResult{File: rel, SHA: spec.SHA(src), Path: path, Root: root}
	if head != nil {
		res.Base, res.BaseSHA, res.Version = head.Int("n"), head.Str("sha256"), head.Int("n")
	}
	if head != nil && res.SHA == head.Str("sha256") {
		res.Spec = s.specInfo(sp)
		return res, nil
	}
	// A pin write-back that failed after its version was published leaves the
	// file one comment short of head: put the pins back instead of importing.
	// Not when that pin-less file is what is committed: removing a pin and
	// committing it is the human's decision, imported (and reported) below.
	committed, tracked := committedBlob(root, rel)
	if head != nil && !in.DryRun && !(tracked && string(committed) == string(src)) {
		if fixed, n := s.restorePins(sp, head, src); n > 0 {
			if e := spec.WriteFileAtomic(path, fixed, res.SHA); e != nil {
				fail(e)
			}
			res.Restored, res.SHA = n, head.Str("sha256")
			res.Spec = s.specInfo(sp)
			return res, nil
		}
	}
	dirty, meta := gitState(root, rel)
	if dirty && !in.Force && !in.DryRun {
		fail(errs.Conflict("%s has uncommitted changes; commit it first, or pass --force to import the working copy", rel))
	}
	doc, e := spec.Parse(src)
	if e != nil {
		fail(e)
	}
	res.Warnings = doc.Warnings
	res.Sections = len(doc.Sections)
	res.Changed = true
	res.Version = res.Base + 1
	baseID := sp.Get("head_version_id")
	var p *plan
	s.tx(func() {
		// Publication re-checks the head inside the transaction (design §5.4).
		cur := s.one("SELECT head_version_id FROM specs WHERE id = ?", specID)
		if fmt.Sprint(cur.Get("head_version_id")) != fmt.Sprint(baseID) {
			fail(errs.Conflict("spec %s was published by someone else meanwhile; re-run sync", sp.Str("slug")))
		}
		p = s.planImport(sp, head, doc)
		res.Lines, res.Counts, res.HeldOrphans, res.Weak = p.lines, p.counts, p.held, p.weak
		if in.DryRun || (in.Strict && (len(p.orphaned) > 0 || p.weak > 0)) {
			return
		}
		now := refs.NowISO()
		v := s.writeVersion(sp, head, p, meta, now)
		s.driftEvents(sp, head, v)
		s.event("spec", specID, "spec-synced", fmt.Sprintf("%s v%d: %d sections (%s)", sp.Str("slug"), v.Int("n"), len(doc.Sections), countsText(p.counts)),
			0, sp.Int("project_id"), now)
	})
	res.Spec = s.specInfo(s.specByID(specID))
	if in.Strict && !in.DryRun && (len(p.orphaned) > 0 || p.weak > 0) {
		e := errs.Conflict("--strict: %d orphaned anchor(s), %d rename(s) below 0.9 confidence; nothing imported", len(p.orphaned), p.weak)
		e.Extra = map[string]any{"changes": res.Lines}
		fail(e)
	}
	return res, nil
}

func countsText(c *row.Row) string {
	var parts []string
	for _, k := range c.Keys() {
		if n := c.Int(k); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, k))
		}
	}
	if len(parts) == 0 {
		return "no structural changes"
	}
	return strings.Join(parts, ", ")
}

// restorePins re-inserts the head's anchor comments missing from src; it
// succeeds only when the result is byte-for-byte the head revision.
func (s *Store) restorePins(sp, head *row.Row, src []byte) ([]byte, int) {
	doc, err := spec.Parse(src)
	if err != nil {
		return nil, 0
	}
	n := 0
	for _, sec := range s.versionSections(head.Int("id")) {
		key := sec.Str("pin")
		if key == "" || sec.Str("kind") != spec.KindHeading {
			continue
		}
		var target *spec.Section
		for i := range doc.Sections {
			if doc.Sections[i].PathSlug == sec.Str("path_slug") {
				target = &doc.Sections[i]
			}
		}
		if target == nil || target.PinKey == key {
			continue
		}
		out, err := spec.InsertPin(src, target, key)
		if err != nil {
			return nil, 0
		}
		src, n = out, n+1
		if doc, err = spec.Parse(src); err != nil {
			return nil, 0
		}
	}
	if n == 0 || spec.SHA(src) != head.Str("sha256") {
		return nil, 0
	}
	return src, n
}

// ListSpecs lists the specs of a project, or of the whole context.
func (s *Store) ListSpecs(project *row.Row, context string) (out []*row.Row, err error) {
	defer guard(&err)
	var rows []*row.Row
	if project != nil {
		rows = s.q("SELECT * FROM specs WHERE project_id = ? ORDER BY slug", project.Int("id"))
	} else {
		ctx := s.contextOrDefault(context)
		rows = s.q("SELECT sp.* FROM specs sp JOIN projects p ON p.id = sp.project_id WHERE p.context_id = ? ORDER BY sp.slug, sp.id", ctx.Int("id"))
	}
	out = []*row.Row{}
	for _, sp := range rows {
		out = append(out, s.specInfo(sp))
	}
	return out, nil
}

// SpecStatus compares each spec's file with its head (design §5.8).
func (s *Store) SpecStatus(project *row.Row, context, repoRoot string) (out []*row.Row, err error) {
	defer guard(&err)
	list, _ := s.ListSpecs(project, context)
	out = []*row.Row{}
	for _, info := range list {
		sp := s.specByID(info.Int("id"))
		root, path := s.specFile(sp, repoRoot)
		state := "clean"
		if b, e := os.ReadFile(path); e != nil {
			state = "missing: file not found"
		} else if spec.SHA(b) != info.Str("sha") {
			state = "drift: file is ahead"
			if dirty, _ := gitState(root, sp.Str("source_path")); dirty {
				state += " (uncommitted)"
			}
		}
		out = append(out, row.Of("slug", info.Str("slug"), "ref", info.Str("ref"), "source_path", info.Get("source_path"),
			"revision", info.Int("revision"), "state", state, "file", path))
	}
	return out, nil
}
