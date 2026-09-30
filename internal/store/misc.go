package store

import (
	"fmt"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
)

// --- tags --------------------------------------------------------------------

func (s *Store) ensureTag(name string) *row.Row {
	slug, err := refs.Slugify(name)
	if err != nil {
		fail(err)
	}
	if r := s.one("SELECT id, name, created_at FROM tags WHERE name = ?", slug); r != nil {
		return r
	}
	s.exec("INSERT INTO tags (name, created_at) VALUES (?,?)", slug, refs.NowISO())
	return s.one("SELECT id, name, created_at FROM tags WHERE name = ?", slug)
}

func (s *Store) getTag(name string, required bool) *row.Row {
	slug, err := refs.Slugify(name)
	if err != nil {
		fail(err)
	}
	r := s.one("SELECT id, name, created_at FROM tags WHERE name = ?", slug)
	if r == nil && required {
		fail(errs.NotFound("tag", name, "no such tag: %s", name))
	}
	return r
}

// GetTag looks a tag up by name.
func (s *Store) GetTag(name string, required bool) (r *row.Row, err error) {
	defer guard(&err)
	return s.getTag(name, required), nil
}

// attachTag applies a tag, reporting whether the tagging is new.
func (s *Store) attachTag(targetType string, targetID int64, name string) (string, bool) {
	tag := s.ensureTag(name)
	res := s.exec("INSERT INTO taggings (tag_id, target_type, target_id, created_at) VALUES (?,?,?,?) ON CONFLICT DO NOTHING",
		tag.Int("id"), targetType, targetID, refs.NowISO())
	n, _ := res.RowsAffected()
	return tag.Str("name"), n > 0
}

// AttachTags applies tags and records one `tagged` event. With onlyNew, the
// event is skipped when every tag was already present (D10: a repeated
// invocation writes nothing).
func (s *Store) AttachTags(targetType string, targetID int64, names []string, taskID, projectID int64, onlyNew bool) (applied []string, err error) {
	defer guard(&err)
	s.tx(func() {
		fresh := false
		for _, name := range names {
			tag, isNew := s.attachTag(targetType, targetID, name)
			applied = append(applied, tag)
			fresh = fresh || isNew
		}
		if fresh || !onlyNew {
			s.event(targetType, targetID, "tagged", strings.Join(applied, ","), taskID, projectID, "")
		}
	})
	return applied, nil
}

// DetachTags removes tags; unknown tags are not-found errors.
func (s *Store) DetachTags(targetType string, targetID int64, names []string) (removed []string, err error) {
	defer guard(&err)
	s.tx(func() {
		for _, name := range names {
			tag := s.getTag(name, true)
			res := s.exec("DELETE FROM taggings WHERE tag_id = ? AND target_type = ? AND target_id = ?",
				tag.Int("id"), targetType, targetID)
			if n, _ := res.RowsAffected(); n > 0 {
				removed = append(removed, tag.Str("name"))
			}
		}
		if len(removed) > 0 {
			s.event(targetType, targetID, "untagged", strings.Join(removed, ","), 0, 0, "")
		}
	})
	return removed, nil
}

func (s *Store) tagsFor(targetType string, targetID int64) []string {
	out := []string{}
	for _, r := range s.q("SELECT g.name FROM taggings tg JOIN tags g ON g.id = tg.tag_id WHERE tg.target_type = ? AND tg.target_id = ? ORDER BY g.name",
		targetType, targetID) {
		out = append(out, r.Str("name"))
	}
	return out
}

// TagsFor lists the tag names on a target.
func (s *Store) TagsFor(targetType string, targetID int64) (out []string, err error) {
	defer guard(&err)
	return s.tagsFor(targetType, targetID), nil
}

// ListTags lists tags with per-kind usage counts.
func (s *Store) ListTags() (out []*row.Row, err error) {
	defer guard(&err)
	for _, r := range s.q(`SELECT g.id, g.name, g.created_at,
  (SELECT COUNT(*) FROM taggings WHERE tag_id = g.id AND target_type = 'project') AS projects,
  (SELECT COUNT(*) FROM taggings WHERE tag_id = g.id AND target_type = 'task') AS tasks,
  (SELECT COUNT(*) FROM taggings WHERE tag_id = g.id AND target_type = 'subtask') AS subtasks,
  (SELECT COUNT(*) FROM notes WHERE target_type = 'tag' AND target_id = g.id) AS notes
FROM tags g ORDER BY g.name`) {
		out = append(out, r)
	}
	return out, nil
}

// DeleteTag removes a tag and its assignments.
func (s *Store) DeleteTag(name string) (r *row.Row, err error) {
	defer guard(&err)
	tag := s.getTag(name, true)
	s.tx(func() {
		s.purgePolymorphic("tag", []int64{tag.Int("id")})
		s.exec("DELETE FROM tags WHERE id = ?", tag.Int("id"))
		s.event("tag", tag.Int("id"), "deleted", tag.Str("name"), 0, 0, "")
	})
	return tag, nil
}

// --- notes -------------------------------------------------------------------

func (s *Store) insertNote(targetType string, targetID int64, body, at, kind string, parentID int64) int64 {
	if at == "" {
		at = refs.NowISO()
	}
	if kind == "" {
		kind = "note"
	}
	return s.insert("INSERT INTO notes (target_type, target_id, body, actor, created_at, kind, parent_id, uid) VALUES (?,?,?,?,?,?,?,?)",
		targetType, targetID, body, nullable(s.Actor), at, kind, nullableID(parentID), NewUID())
}

// NewNote are the inputs of AddNote.
type NewNote struct {
	TargetType string
	TargetID   int64
	Body       string
	TaskID     int64
	ProjectID  int64
	At         string
	Kind       string // "" = note
	ParentID   int64
	Quote, Prefix, Suffix string
	VersionID  int64
}

// AddNote attaches a note and records a `note` event.
func (s *Store) AddNote(in NewNote) (r *row.Row, err error) {
	defer guard(&err)
	return s.addNote(in), nil
}

func (s *Store) addNote(in NewNote) *row.Row {
	body := strings.TrimSpace(in.Body)
	if body == "" {
		fail(errs.New("a note needs a body"))
	}
	var id int64
	s.tx(func() {
		id = s.insertNote(in.TargetType, in.TargetID, body, in.At, in.Kind, in.ParentID)
		if in.Quote != "" || in.VersionID != 0 {
			s.exec("UPDATE notes SET quote = ?, quote_prefix = ?, quote_suffix = ?, quote_ok = ?, version_id = ? WHERE id = ?",
				nullable(in.Quote), nullable(in.Prefix), nullable(in.Suffix), boolQuote(in.Quote), nullableID(in.VersionID), id)
		}
		s.event(in.TargetType, in.TargetID, noteAction(in.TargetType, in.Kind), firstLine(body), in.TaskID, in.ProjectID, in.At)
	})
	return s.noteRow(s.one("SELECT * FROM notes WHERE id = ?", id))
}

// noteAction is the event action for a new note: plain notes keep the v2
// `note`; typed notes say what they are; spec notes are `spec-*`, which is
// what `spec watch` keys on.
func noteAction(targetType, kind string) string {
	spec := targetType == "spec_anchor" || targetType == "spec"
	switch {
	case spec && kind == "approval":
		return "spec-approved"
	case spec && kind == "build-note":
		return "spec-build-note"
	case spec:
		return "spec-note"
	case kind == "" || kind == "note":
		return "note"
	}
	return "note-" + kind
}

func boolQuote(q string) any {
	if q == "" {
		return nil
	}
	return int64(1)
}

var noteCols = []string{"id", "target_type", "target_id", "body", "actor", "created_at"}

// noteRow is the v2 note columns, plus kind/resolution/thread/quote fields
// only when they carry something (so plain notes render exactly as before).
func (s *Store) noteRow(r *row.Row) *row.Row {
	out := pick(r, noteCols...)
	if k := r.Str("kind"); k != "" && k != "note" {
		out.Set("kind", k)
	}
	for _, k := range []string{"parent_id", "resolved_at", "resolved_by", "resolution", "quote", "quote_ok"} {
		if r.Has(k) && !r.IsNull(k) {
			out.Set(k, r.Get(k))
		}
	}
	return out
}

// NoteFilter narrows NotesFor.
type NoteFilter struct {
	Limit    int
	OpenOnly bool
	Kind     string
}

func (s *Store) notesFor(targetType string, targetID int64, limit int) []*row.Row {
	return s.notesForF(targetType, targetID, NoteFilter{Limit: limit})
}

func (s *Store) notesForF(targetType string, targetID int64, f NoteFilter) []*row.Row {
	query := "SELECT * FROM notes WHERE target_type = ? AND target_id = ?"
	args := []any{targetType, targetID}
	if f.OpenOnly {
		query += " AND resolved_at IS NULL"
	}
	if f.Kind != "" {
		query += " AND kind = ?"
		args = append(args, f.Kind)
	}
	query += " ORDER BY created_at, id"
	if f.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	out := []*row.Row{}
	for _, r := range s.q(query, args...) {
		item := s.noteRow(r)
		item.Set("created", refs.FmtTS(r.Str("created_at"), false, false))
		out = append(out, item)
	}
	return out
}

// NotesFor lists the notes on a target, oldest first.
func (s *Store) NotesFor(targetType string, targetID int64, f NoteFilter) (out []*row.Row, err error) {
	defer guard(&err)
	return s.notesForF(targetType, targetID, f), nil
}

// GetNote fetches a note by id (full row including v3 columns).
func (s *Store) GetNote(id int64) (r *row.Row, err error) {
	defer guard(&err)
	r = s.one("SELECT * FROM notes WHERE id = ?", id)
	if r == nil {
		fail(errs.NotFound("note", fmt.Sprint(id), "no such note: %d", id))
	}
	return r, nil
}

// DeleteNote removes a note and records it (D5, D6).
func (s *Store) DeleteNote(id int64) (err error) {
	defer guard(&err)
	s.tx(func() {
		n := s.one("SELECT * FROM notes WHERE id = ?", id)
		if n == nil {
			fail(errs.NotFound("note", fmt.Sprint(id), "no such note: %d", id))
		}
		taskID, projectID := s.noteScope(n)
		s.exec("DELETE FROM notes WHERE id = ?", id)
		s.event("note", id, "note-removed", firstLine(n.Str("body")), taskID, projectID, "")
	})
	return nil
}

// ResolveNote marks a note resolved (idempotent: resolving a resolved note
// changes nothing). changed reports whether anything was written.
func (s *Store) ResolveNote(id int64, rationale string, reopen bool) (r *row.Row, changed bool, err error) {
	defer guard(&err)
	s.tx(func() {
		n := s.one("SELECT * FROM notes WHERE id = ?", id)
		if n == nil {
			fail(errs.NotFound("note", fmt.Sprint(id), "no such note: %d", id))
		}
		taskID, projectID := s.noteScope(n)
		if reopen {
			if n.IsNull("resolved_at") {
				return
			}
			s.exec("UPDATE notes SET resolved_at = NULL, resolved_by = NULL, resolution = NULL WHERE id = ?", id)
			s.event("note", id, "note-reopened", nil, taskID, projectID, "")
			changed = true
			return
		}
		if !n.IsNull("resolved_at") {
			return
		}
		s.exec("UPDATE notes SET resolved_at = ?, resolved_by = ?, resolution = ? WHERE id = ?",
			refs.NowISO(), nullable(s.Actor), nullable(rationale), id)
		detail := firstLine(n.Str("body"))
		if rationale != "" {
			detail = firstLine(rationale)
		}
		s.event("note", id, "note-resolved", detail, taskID, projectID, "")
		changed = true
	})
	r = s.one("SELECT * FROM notes WHERE id = ?", id)
	return r, changed, nil
}

// noteScope finds the task/project a note belongs to, for event scoping.
func (s *Store) noteScope(n *row.Row) (taskID, projectID int64) {
	id := n.Int("target_id")
	switch n.Str("target_type") {
	case "task":
		if t := s.one("SELECT project_id FROM tasks WHERE id = ?", id); t != nil {
			return id, t.Int("project_id")
		}
	case "subtask":
		if t := s.one("SELECT t.id, t.project_id FROM subtasks st JOIN tasks t ON t.id = st.task_id WHERE st.id = ?", id); t != nil {
			return t.Int("id"), t.Int("project_id")
		}
	case "project":
		return 0, id
	case "spec":
		if sp := s.one("SELECT project_id FROM specs WHERE id = ?", id); sp != nil {
			return 0, sp.Int("project_id")
		}
	case "spec_anchor":
		if sp := s.one("SELECT sp.project_id FROM spec_anchors a JOIN specs sp ON sp.id = a.spec_id WHERE a.id = ?", id); sp != nil {
			return 0, sp.Int("project_id")
		}
	}
	return 0, 0
}

// --- pull requests -----------------------------------------------------------

var prCols = []string{"id", "task_id", "url", "repo", "number", "title", "state", "created_at", "updated_at"}

// AddPR links a pull request to a task.
func (s *Store) AddPR(task *row.Row, url, title, state string) (r *row.Row, err error) {
	defer guard(&err)
	return s.addPR(task, url, title, state), nil
}

func (s *Store) addPR(task *row.Row, url, title, state string) *row.Row {
	repo, number, ok := refs.ParsePRURL(url)
	var repoV, numV any
	if ok {
		repoV, numV = repo, number
	}
	if state == "" {
		state = "open"
	}
	now := refs.NowISO()
	var id int64
	s.tx(func() {
		res, e := s.execC("INSERT INTO pull_requests (task_id, url, repo, number, title, state, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)",
			task.Int("id"), url, repoV, numV, nullable(title), state, now, now)
		if e != nil {
			fail(errs.Conflict("pull request already linked to task %d", task.Int("id")))
		}
		id, _ = res.LastInsertId()
		detail := strings.TrimSpace(fmt.Sprintf("%s#%s %s", repo, numStr(number, ok), state))
		s.event("pr", id, "pr", detail, task.Int("id"), task.Int("project_id"), "")
	})
	return pick(s.one("SELECT * FROM pull_requests WHERE id = ?", id), prCols...)
}

func numStr(n int64, ok bool) string {
	if !ok || n == 0 {
		return ""
	}
	return fmt.Sprint(n)
}

// SetPRState updates a PR's state.
func (s *Store) SetPRState(prID int64, state string) (r *row.Row, err error) {
	defer guard(&err)
	s.tx(func() {
		pr := s.one("SELECT * FROM pull_requests WHERE id = ?", prID)
		if pr == nil {
			fail(errs.NotFound("pr", fmt.Sprint(prID), "no such pull request record: %d", prID))
		}
		s.exec("UPDATE pull_requests SET state = ?, updated_at = ? WHERE id = ?", state, refs.NowISO(), prID)
		s.event("pr", prID, "pr-state", pr.Str("state")+" -> "+state, pr.Int("task_id"), 0, "")
	})
	return pick(s.one("SELECT * FROM pull_requests WHERE id = ?", prID), prCols...), nil
}

// RemovePR unlinks a PR (D5: row and event in one tx).
func (s *Store) RemovePR(prID int64) (err error) {
	defer guard(&err)
	s.tx(func() {
		res := s.exec("DELETE FROM pull_requests WHERE id = ?", prID)
		if n, _ := res.RowsAffected(); n == 0 {
			fail(errs.NotFound("pr", fmt.Sprint(prID), "no such pull request record: %d", prID))
		}
		s.event("pr", prID, "pr-removed", nil, 0, 0, "")
	})
	return nil
}

func (s *Store) prsFor(taskID int64) []*row.Row {
	out := []*row.Row{}
	for _, r := range s.q("SELECT * FROM pull_requests WHERE task_id = ? ORDER BY id", taskID) {
		item := pick(r, prCols...)
		item.Set("created", refs.FmtTS(r.Str("created_at"), false, false))
		out = append(out, item)
	}
	return out
}

// PRsFor lists a task's PRs.
func (s *Store) PRsFor(taskID int64) (out []*row.Row, err error) {
	defer guard(&err)
	return s.prsFor(taskID), nil
}

// PRFilter narrows FindPRs.
type PRFilter struct {
	URL, Repo, State string
	Number           int64
	HasNumber        bool
	Context          string
	AllContexts      bool
}

// FindPRs searches PRs across tasks.
func (s *Store) FindPRs(f PRFilter) (out []*row.Row, err error) {
	defer guard(&err)
	var where []string
	var args []any
	if f.URL != "" {
		where = append(where, "r.url = ?")
		args = append(args, f.URL)
	}
	if f.Repo != "" {
		repo, e := refs.NormalizeRepo(f.Repo)
		if e != nil {
			fail(e)
		}
		where = append(where, "r.repo = ?")
		args = append(args, repo)
	}
	if f.HasNumber {
		where = append(where, "r.number = ?")
		args = append(args, f.Number)
	}
	if f.State != "" {
		where = append(where, "r.state = ?")
		args = append(args, f.State)
	}
	if !f.AllContexts || f.Context != "" {
		ctx := s.contextOrDefault(f.Context)
		where = append(where, "p.context_id = ?")
		args = append(args, ctx.Int("id"))
	}
	query := "SELECT r.id, r.task_id, r.url, r.repo, r.number, r.title, r.state, r.created_at, r.updated_at," +
		" t.title task_title, t.status task_status, t.id task_id" +
		" FROM pull_requests r JOIN tasks t ON t.id = r.task_id JOIN projects p ON p.id = t.project_id"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY r.id"
	out = []*row.Row{}
	for _, r := range s.q(query, args...) {
		r.Set("created", refs.FmtTS(r.Str("created_at"), false, false))
		out = append(out, r)
	}
	return out, nil
}

// --- events / worklog --------------------------------------------------------

// AddLog appends a free-form worklog entry.
func (s *Store) AddLog(body string, task, project *row.Row, at string) (id int64, err error) {
	defer guard(&err)
	body = strings.TrimSpace(body)
	if body == "" {
		fail(errs.New("a log entry needs a body"))
	}
	targetType, targetID := "log", int64(0)
	var taskID, projectID int64
	switch {
	case task != nil:
		targetType, targetID = "task", task.Int("id")
		taskID, projectID = task.Int("id"), task.Int("project_id")
	case project != nil:
		targetType, targetID = "project", project.Int("id")
		projectID = project.Int("id")
	}
	s.tx(func() { id = s.event(targetType, targetID, "log", body, taskID, projectID, at) })
	return id, nil
}

// EventFilter narrows ListEvents.
type EventFilter struct {
	Since, Until   string
	Task, Project  *row.Row
	Direct         bool
	Context        string
	Actor          string
	ActorNot       string
	Actions        []string
	Limit          int // 0 = unlimited
	AllContexts    bool
	Ascending      bool
	BackdatedOnly  bool
	AfterID        int64 // cursor: id > AfterID, insertion order
	ByID           bool  // order by id instead of at
}

// ListEvents reads the worklog. total is before Limit (D3).
func (s *Store) ListEvents(f EventFilter) (out []*row.Row, total int, err error) {
	defer guard(&err)
	out, total = s.listEventsT(f)
	return out, total, nil
}

func (s *Store) listEvents(f EventFilter) []*row.Row {
	out, _ := s.listEventsT(f)
	return out
}

func (s *Store) listEventsT(f EventFilter) ([]*row.Row, int) {
	var where []string
	var args []any
	switch {
	case f.Task != nil:
		where = append(where, "e.task_id = ?")
		args = append(args, f.Task.Int("id"))
	case f.Project != nil:
		ids := []int64{f.Project.Int("id")}
		if !f.Direct {
			ids = s.descendantIDs(f.Project.Int("id"))
		}
		where = append(where, "e.project_id IN ("+marks(len(ids))+")")
		args = append(args, ids64(ids)...)
	case !f.AllContexts:
		ctx := s.contextOrDefault(f.Context)
		where = append(where, "(e.project_id IN (SELECT id FROM projects WHERE context_id = ?) OR (e.target_type = 'context' AND e.target_id = ?))")
		args = append(args, ctx.Int("id"), ctx.Int("id"))
	}
	if f.Since != "" {
		where = append(where, "e.at >= ?")
		args = append(args, f.Since)
	}
	if f.Until != "" {
		where = append(where, "e.at <= ?")
		args = append(args, f.Until)
	}
	if f.Actor != "" {
		where = append(where, "e.actor = ?")
		args = append(args, f.Actor)
	}
	if f.ActorNot != "" {
		where = append(where, "(e.actor IS NULL OR e.actor <> ?)")
		args = append(args, f.ActorNot)
	}
	if len(f.Actions) > 0 {
		where = append(where, "e.action IN ("+marks(len(f.Actions))+")")
		for _, a := range f.Actions {
			args = append(args, a)
		}
	}
	if f.BackdatedOnly {
		where = append(where, "e.recorded_at IS NOT NULL AND e.recorded_at <> e.at")
	}
	if f.AfterID > 0 {
		where = append(where, "e.id > ?")
		args = append(args, f.AfterID)
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}
	total := int(s.count("SELECT COUNT(*) FROM events e"+cond, args...))
	query := "SELECT e.id, e.at, e.recorded_at, e.actor, e.target_type, e.target_id, e.action, e.detail, e.task_id, e.project_id," +
		" t.title task_title, s.position sub_position, s.task_id sub_task_id FROM events e" +
		" LEFT JOIN tasks t ON t.id = e.task_id" +
		" LEFT JOIN subtasks s ON s.id = e.target_id AND e.target_type = 'subtask'" + cond
	if f.ByID {
		query += " ORDER BY e.id ASC"
	} else {
		query += " ORDER BY e.at DESC, e.id DESC"
	}
	if f.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows := s.q(query, args...)
	if f.Ascending && !f.ByID {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}
	out := make([]*row.Row, 0, len(rows))
	for _, item := range rows {
		at := item.Str("at")
		item.Set("when", refs.FmtTS(at, false, false))
		item.Set("date", refs.FmtTS(at, false, true))
		backdated := !item.IsNull("recorded_at") && item.Str("recorded_at") != "" && item.Str("recorded_at") != at
		item.Set("backdated", backdated)
		if backdated {
			item.Set("recorded", refs.FmtTS(item.Str("recorded_at"), false, true))
		} else {
			item.Set("recorded", "")
		}
		pos, subTask := item.Get("sub_position"), item.Get("sub_task_id")
		item.Delete("sub_position")
		item.Delete("sub_task_id")
		switch {
		case pos != nil && subTask != nil:
			item.Set("target", fmt.Sprintf("%v.%v", subTask, pos))
		case item.Int("task_id") != 0:
			item.Set("target", fmt.Sprintf("#%d", item.Int("task_id")))
		default:
			item.Set("target", item.Str("target_type"))
		}
		title := item.Str("task_title")
		item.Delete("task_title")
		item.Set("task", title)
		out = append(out, item)
	}
	return out, total
}

// MaxEventID is the current end of the worklog (a watch cursor).
func (s *Store) MaxEventID() (id int64, err error) {
	defer guard(&err)
	return s.count("SELECT COALESCE(MAX(id), 0) FROM events"), nil
}

// --- aggregates ----------------------------------------------------------------

// Summary is counts per project plus current work, per context.
func (s *Store) Summary(context string, allContexts bool) (out *row.Row, err error) {
	defer guard(&err)
	var contexts []*row.Row
	if allContexts {
		contexts, _ = s.ListContexts(false)
	} else {
		contexts = []*row.Row{s.contextOrDefault(context)}
	}
	list := []*row.Row{}
	for _, ctx := range contexts {
		projects := s.listProjects(ProjectFilter{Context: ctx.Str("name")})
		var ids []int64
		for _, p := range projects {
			ids = append(ids, p.Int("id"))
		}
		inProgress, _ := s.listTasks(TaskFilter{Context: ctx.Str("name"), Statuses: []string{"in-progress"}, Order: "updated"})
		if inProgress == nil {
			inProgress = []*row.Row{}
		}
		if projects == nil {
			projects = []*row.Row{}
		}
		list = append(list, row.Of("name", ctx.Str("name"), "projects", projects, "counts", s.taskCounts(ids), "in_progress", inProgress))
	}
	return row.Of("contexts", list), nil
}

// TableCounts counts rows per table for `info`.
func (s *Store) TableCounts() (out *row.Row, err error) {
	defer guard(&err)
	out = row.New()
	for _, t := range []string{"contexts", "projects", "tasks", "subtasks", "tags", "notes", "pull_requests", "events"} {
		out.Set(t, s.count("SELECT COUNT(*) FROM "+t))
	}
	return out, nil
}

// NoteThread lists the replies under a note, oldest first.
func (s *Store) NoteThread(id int64) (out []*row.Row, err error) {
	defer guard(&err)
	out = []*row.Row{}
	for _, r := range s.q("SELECT * FROM notes WHERE parent_id = ? ORDER BY created_at, id", id) {
		item := s.noteRow(r)
		out = append(out, item)
	}
	return out, nil
}
