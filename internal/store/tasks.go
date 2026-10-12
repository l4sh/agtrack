package store

import (
	"fmt"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
)

var taskCols = []string{"id", "project_id", "title", "status", "created_at", "updated_at", "started_at", "closed_at"}

// taskRow is the v2 task columns, plus the claim columns when a claim is
// held (so unclaimed tasks render exactly as before).
func taskRow(r *row.Row) *row.Row {
	out := pick(r, taskCols...)
	if r.Has("claimed_by") && !r.IsNull("claimed_by") {
		out.Set("claimed_by", r.Get("claimed_by"))
		out.Set("claimed_at", r.Get("claimed_at"))
		out.Set("claim_expires_at", r.Get("claim_expires_at"))
		if r.Has("claim_session") && !r.IsNull("claim_session") {
			out.Set("claim_session", r.Get("claim_session"))
		}
	}
	if r.Has("snoozed_until") && !r.IsNull("snoozed_until") {
		out.Set("snoozed_until", r.Get("snoozed_until"))
		out.Set("snooze_note", r.Get("snooze_note"))
	}
	return out
}

// Orders maps `task ls --order` values to SQL.
var Orders = map[string]string{
	"id":      "t.id ASC",
	"created": "t.created_at ASC",
	"updated": "t.updated_at DESC",
	"status":  "CASE t.status WHEN 'in-progress' THEN 0 WHEN 'todo' THEN 1 WHEN 'done' THEN 2 ELSE 3 END ASC, t.id ASC",
}

func (s *Store) getTaskID(id int64, ref string, required bool) *row.Row {
	r := s.one("SELECT * FROM tasks WHERE id = ?", id)
	if r == nil {
		if required {
			fail(errs.NotFound("task", ref, "no such task: %d", id))
		}
		return nil
	}
	return taskRow(r)
}

func (s *Store) getTask(ref string, required bool) *row.Row {
	id, err := refs.ParseTaskRef(ref)
	if err != nil {
		fail(err)
	}
	return s.getTaskID(id, ref, required)
}

// GetTask resolves `12`/`#12`.
func (s *Store) GetTask(ref string, required bool) (r *row.Row, err error) {
	defer guard(&err)
	return s.getTask(ref, required), nil
}

// GetTaskID fetches a task by id.
func (s *Store) GetTaskID(id int64) (r *row.Row, err error) {
	defer guard(&err)
	return s.getTaskID(id, fmt.Sprint(id), true), nil
}

// NewTask are the inputs of CreateTask.
type NewTask struct {
	Title                    string
	Status                   string // "" = todo
	Tags                     []string
	Note                     string
	Created, Started, Closed string // backfill stamps, already parsed ISO
}

// CreateTask creates a task, optionally reconstructing work that already
// happened: backdating writes the transitions it must have gone through.
func (s *Store) CreateTask(project *row.Row, in NewTask) (r *row.Row, err error) {
	defer guard(&err)
	return s.createTask(project, in), nil
}

func (s *Store) createTask(project *row.Row, in NewTask) *row.Row {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		fail(errs.New("a task needs a title"))
	}
	status := in.Status
	if status == "" {
		status = "todo"
	}
	now := refs.NowISO()
	createdAt := in.Created
	if createdAt == "" {
		createdAt = now
	}
	startedAt := in.Started
	if startedAt == "" && status == "in-progress" {
		if in.Created != "" {
			startedAt = createdAt
		} else {
			startedAt = now
		}
	}
	closedAt := in.Closed
	if closedAt == "" && Closed(status) {
		if in.Created != "" {
			closedAt = startedAt
			if closedAt == "" {
				closedAt = createdAt
			}
		} else {
			closedAt = now
		}
	}
	if closedAt != "" && !Closed(status) {
		fail(errs.New("--closed given but the status is %s: pass --status done or --status cancelled", status))
	}
	checkOrder(stamp{"--created", createdAt}, stamp{"--started", startedAt}, stamp{"--closed", closedAt})
	updatedAt := createdAt
	for _, v := range []string{startedAt, closedAt} {
		if v > updatedAt {
			updatedAt = v
		}
	}
	var id int64
	pid := project.Int("id")
	s.tx(func() {
		id = s.insert("INSERT INTO tasks (project_id, title, status, created_at, updated_at, started_at, closed_at, uid) VALUES (?,?,?,?,?,?,?,?)",
			pid, title, status, createdAt, updatedAt, nullable(startedAt), nullable(closedAt), NewUID())
		for _, tag := range in.Tags {
			s.attachTag("task", id, tag)
		}
		if in.Note != "" {
			s.insertNote("task", id, in.Note, createdAt, "note", 0)
		}
		s.event("task", id, "created", title, id, pid, createdAt)
		previous := "todo"
		if startedAt != "" {
			s.event("task", id, "status", previous+" -> in-progress", id, pid, startedAt)
			previous = "in-progress"
		}
		if closedAt != "" {
			s.event("task", id, "status", previous+" -> "+status, id, pid, closedAt)
		}
	})
	return s.getTaskID(id, "", true)
}

// SetTaskStatus moves a task, re-reading it inside the write transaction
// (D9). Setting the current status is a no-op: changed is false and nothing
// is written.
func (s *Store) SetTaskStatus(taskID int64, status, at string) (r *row.Row, changed bool, err error) {
	defer guard(&err)
	s.tx(func() { r, changed = s.setTaskStatus(taskID, status, at) })
	return r, changed, nil
}

func (s *Store) setTaskStatus(taskID int64, status, at string) (*row.Row, bool) {
	task := s.getTaskID(taskID, fmt.Sprint(taskID), true)
	if task.Str("status") == status {
		return task, false
	}
	stampAt := at
	if stampAt == "" {
		stampAt = refs.NowISO()
	}
	if at != "" {
		notBefore("--at", stampAt, "the task's creation", task.Str("created_at"))
		if Closed(status) {
			notBefore("--at", stampAt, "the task's start", task.Str("started_at"))
		}
	}
	started := task.Get("started_at")
	if started == nil && status == "in-progress" {
		started = stampAt
	}
	var closedAt any
	if Closed(status) {
		closedAt = stampAt
	}
	s.tx(func() {
		s.exec("UPDATE tasks SET status = ?, updated_at = ?, started_at = ?, closed_at = ? WHERE id = ?",
			status, stampAt, started, closedAt, taskID)
		if status == "done" {
			// A finished task releases its claim (design §4.6).
			s.exec("UPDATE tasks SET claimed_by = NULL, claimed_at = NULL, claim_expires_at = NULL, claim_session = NULL, claim_last_seen = NULL WHERE id = ?", taskID)
			// Finishing re-stamps what the task agreed to in its spec links (design §5.7).
			s.restampTaskLinks(taskID)
		}
		s.event("task", taskID, "status", task.Str("status")+" -> "+status, taskID, task.Int("project_id"), stampAt)
	})
	return s.getTaskID(taskID, "", true), true
}

// UpdateTask retitles or moves a task. A non-nil expectTitle is a
// compare-and-set precondition checked inside the write transaction: a task
// whose stored title differs is a conflict carrying `current` and `yours`.
func (s *Store) UpdateTask(task *row.Row, fields []Field, expectTitle *string) (r *row.Row, err error) {
	defer guard(&err)
	pid := task.Int("project_id")
	for _, f := range fields {
		if f.Name == "project_id" && f.Value != nil {
			pid = f.Value.(int64)
		}
	}
	s.tx(func() {
		s.expectTitle("tasks", "task", task.Int("id"), expectTitle, fields)
		s.updateRow("tasks", task.Int("id"), fields)
		s.event("task", task.Int("id"), "updated", describeFields(fields), task.Int("id"), pid, "")
	})
	return s.getTaskID(task.Int("id"), "", true), nil
}

// TaskChildren is what a `task rm` would destroy with the task, in order.
func (s *Store) TaskChildren(taskID int64) (out *row.Row, err error) {
	defer guard(&err)
	return row.Of(
		"subtasks", s.count("SELECT COUNT(*) FROM subtasks WHERE task_id = ?", taskID),
		"pull requests", s.count("SELECT COUNT(*) FROM pull_requests WHERE task_id = ?", taskID),
		"notes", s.count("SELECT COUNT(*) FROM notes WHERE target_type = 'task' AND target_id = ?", taskID),
	), nil
}

// SubtaskChildren is what a `sub rm` would destroy.
func (s *Store) SubtaskChildren(subID int64) (out *row.Row, err error) {
	defer guard(&err)
	return row.Of("notes", s.count("SELECT COUNT(*) FROM notes WHERE target_type = 'subtask' AND target_id = ?", subID)), nil
}

// DeleteTask removes a task and everything polymorphic hanging off it.
func (s *Store) DeleteTask(task *row.Row) (err error) {
	defer guard(&err)
	s.tx(func() {
		var subIDs []int64
		for _, r := range s.q("SELECT id FROM subtasks WHERE task_id = ?", task.Int("id")) {
			subIDs = append(subIDs, r.Int("id"))
		}
		s.purgePolymorphic("subtask", subIDs)
		s.purgePolymorphic("task", []int64{task.Int("id")})
		s.exec("DELETE FROM tasks WHERE id = ?", task.Int("id"))
		s.event("task", task.Int("id"), "deleted", task.Str("title"), 0, task.Int("project_id"), "")
	})
	return nil
}

// TaskFilter narrows ListTasks.
type TaskFilter struct {
	Context     string
	Project     *row.Row
	Direct      bool
	Statuses    []string // nil = any
	Tags        []string
	TagsAll     bool
	Grep        string
	Since       string
	Repo        string
	Limit       int
	Order       string
	AllContexts bool
	Unclaimed   bool // no live claim
	Ready       bool // no unmet `blocks` prerequisite
	Blocked     bool // only tasks with an unmet prerequisite
	ClaimedBy   string
	// ExcludeSnoozed hides parked tasks (snoozed_until in the future);
	// OnlySnoozed shows only them (A5).
	ExcludeSnoozed, OnlySnoozed bool
	// MissingArtifact keeps tasks with no "branch" artifact or no linked "pr" (F1).
	MissingArtifact string
}

// ListTasks lists tasks as flat listing rows. total is the number of matches
// before Limit (D3).
func (s *Store) ListTasks(f TaskFilter) (out []*row.Row, total int, err error) {
	defer guard(&err)
	out, total = s.listTasks(f)
	return out, total, nil
}

func (s *Store) listTasks(f TaskFilter) ([]*row.Row, int) {
	var where []string
	var args []any
	var projectIDs []int64
	haveProjects := false
	if f.Project != nil {
		haveProjects = true
		if f.Direct {
			projectIDs = []int64{f.Project.Int("id")}
		} else {
			projectIDs = s.descendantIDs(f.Project.Int("id"))
		}
	} else if f.Repo != "" {
		haveProjects = true
		for _, p := range s.findProjectsByRepo(f.Repo) {
			if f.Direct {
				projectIDs = append(projectIDs, p.Int("id"))
			} else {
				projectIDs = append(projectIDs, s.descendantIDs(p.Int("id"))...)
			}
		}
		if len(projectIDs) == 0 {
			projectIDs = []int64{-1}
		}
	}
	if haveProjects {
		where = append(where, "t.project_id IN ("+marks(len(projectIDs))+")")
		args = append(args, ids64(projectIDs)...)
	} else if !f.AllContexts || f.Context != "" {
		ctx := s.contextOrDefault(f.Context)
		where = append(where, "p.context_id = ?")
		args = append(args, ctx.Int("id"))
	}
	if len(f.Statuses) > 0 {
		where = append(where, "t.status IN ("+marks(len(f.Statuses))+")")
		for _, st := range f.Statuses {
			args = append(args, st)
		}
	}
	if f.Grep != "" {
		where = append(where, "t.title LIKE ?")
		args = append(args, "%"+f.Grep+"%")
	}
	if f.Since != "" {
		where = append(where, "t.updated_at >= ?")
		args = append(args, f.Since)
	}
	tagSlugs := make([]any, 0, len(f.Tags))
	for _, t := range f.Tags {
		slug, err := refs.Slugify(t)
		if err != nil {
			fail(err)
		}
		tagSlugs = append(tagSlugs, slug)
	}
	const tagged = "t.id IN (SELECT tg.target_id FROM taggings tg JOIN tags g ON g.id = tg.tag_id WHERE tg.target_type = 'task' AND g.name "
	if f.TagsAll {
		for _, slug := range tagSlugs {
			where = append(where, tagged+"= ?)")
			args = append(args, slug)
		}
	} else if len(tagSlugs) > 0 {
		where = append(where, tagged+"IN ("+marks(len(tagSlugs))+"))")
		args = append(args, tagSlugs...)
	}
	now := refs.NowISO()
	if f.Unclaimed {
		where = append(where, "(t.claimed_by IS NULL OR (t.claim_expires_at IS NOT NULL AND t.claim_expires_at <= ?))")
		args = append(args, now)
	}
	if f.ClaimedBy != "" {
		where = append(where, "t.claimed_by = ? AND (t.claim_expires_at IS NULL OR t.claim_expires_at > ?)")
		args = append(args, f.ClaimedBy, now)
	}
	if f.ExcludeSnoozed {
		where = append(where, "(t.snoozed_until IS NULL OR t.snoozed_until <= ?)")
		args = append(args, now)
	}
	if f.OnlySnoozed {
		where = append(where, "t.snoozed_until > ?")
		args = append(args, now)
	}
	const unmet = "EXISTS (SELECT 1 FROM task_deps d JOIN tasks b ON b.id = d.depends_on_id WHERE d.task_id = t.id AND d.kind = 'blocks' AND b.status <> 'done')"
	if f.Ready {
		where = append(where, "NOT "+unmet)
	}
	if f.Blocked {
		where = append(where, unmet)
	}
	switch f.MissingArtifact {
	case "branch":
		where = append(where, "NOT EXISTS (SELECT 1 FROM artifacts a WHERE a.task_id = t.id AND a.kind = 'branch')")
	case "pr":
		where = append(where, "NOT EXISTS (SELECT 1 FROM pull_requests r WHERE r.task_id = t.id)")
	}
	from := " FROM tasks t JOIN projects p ON p.id = t.project_id"
	if len(where) > 0 {
		from += " WHERE " + strings.Join(where, " AND ")
	}
	total := int(s.count("SELECT COUNT(*)"+from, args...))
	order, ok := Orders[f.Order]
	if !ok {
		order = Orders["id"]
	}
	query := "SELECT t.*" + from + " ORDER BY " + order
	if f.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	var out []*row.Row
	for _, r := range s.q(query, args...) {
		out = append(out, s.taskDict(taskRow(r)))
	}
	return out, total
}

// TaskDict flattens a task for listing: no nested collections.
func (s *Store) TaskDict(task *row.Row) (r *row.Row, err error) {
	defer guard(&err)
	return s.taskDict(task), nil
}

func (s *Store) taskDict(task *row.Row) *row.Row {
	id := task.Int("id")
	item := task.Copy()
	project := s.getProject(task.Int("project_id"))
	path := "?"
	short := "?"
	if project != nil {
		path = s.projectPath(project)
		_, short, _ = strings.Cut(path, "/")
	}
	agg := s.one(`SELECT
  (SELECT COUNT(*) FROM subtasks WHERE task_id = ?1) AS total,
  (SELECT COUNT(*) FROM subtasks WHERE task_id = ?1 AND status = 'done') AS done,
  (SELECT COUNT(*) FROM pull_requests WHERE task_id = ?1) AS prs,
  (SELECT COUNT(*) FROM notes WHERE target_type = 'task' AND target_id = ?1) AS notes`, id)
	ctxName, _, _ := strings.Cut(path, "/")
	item.Set("project", short)
	item.Set("path", path)
	item.Set("context", ctxName)
	item.Set("mark", refs.StatusMark[task.Str("status")])
	item.Set("tags", strings.Join(s.tagsFor("task", id), ","))
	if total := agg.Int("total"); total > 0 {
		item.Set("subtasks", fmt.Sprintf("%d/%d", agg.Int("done"), total))
	} else {
		item.Set("subtasks", "")
	}
	item.Set("prs", agg.Int("prs"))
	item.Set("notes", agg.Int("notes"))
	item.Set("created", refs.FmtTS(task.Str("created_at"), false, false))
	item.Set("updated", refs.FmtTS(task.Str("updated_at"), false, false))
	item.Set("started", refs.FmtTS(task.Str("started_at"), false, false))
	item.Set("closed", refs.FmtTS(task.Str("closed_at"), false, false))
	return item
}

// TaskDetail is everything hanging off one task. The event history is only
// attached when asked for (D12: the human view hides it behind -x too).
func (s *Store) TaskDetail(task *row.Row, withEvents bool) (r *row.Row, err error) {
	defer guard(&err)
	return s.taskDetail(task, withEvents), nil
}

func (s *Store) taskDetail(task *row.Row, withEvents bool) *row.Row {
	id := task.Int("id")
	item := s.taskDict(task)
	item.Set("tag_list", s.tagsFor("task", id))
	item.Set("subtask_list", s.listSubtasks(id, nil, 0))
	item.Set("note_list", s.notesFor("task", id, 0))
	item.Set("pr_list", s.prsFor(id))
	if withEvents {
		item.Set("events", s.listEvents(EventFilter{Task: task, Limit: 20, Ascending: true}))
	}
	return item
}
