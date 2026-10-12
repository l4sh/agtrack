package store

import (
	"fmt"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
)

var subtaskCols = []string{"id", "task_id", "position", "title", "status", "created_at", "updated_at", "started_at", "closed_at"}

func subtaskRow(r *row.Row) *row.Row { return pick(r, subtaskCols...) }

func (s *Store) getSubtask(ref string, required bool) *row.Row {
	taskID, n, err := refs.ParseSubtaskRef(ref)
	if err != nil {
		fail(err)
	}
	var r *row.Row
	if taskID == 0 {
		r = s.one("SELECT * FROM subtasks WHERE id = ?", n)
	} else {
		r = s.one("SELECT * FROM subtasks WHERE task_id = ? AND position = ?", taskID, n)
	}
	if r == nil {
		if required {
			fail(errs.NotFound("subtask", ref, "no such subtask: %s", ref))
		}
		return nil
	}
	return subtaskRow(r)
}

func (s *Store) getSubtaskID(id int64) *row.Row {
	r := s.one("SELECT * FROM subtasks WHERE id = ?", id)
	if r == nil {
		fail(errs.NotFound("subtask", fmt.Sprint(id), "no such subtask: %d", id))
	}
	return subtaskRow(r)
}

// GetSubtask resolves `47` or `12.3`.
func (s *Store) GetSubtask(ref string, required bool) (r *row.Row, err error) {
	defer guard(&err)
	return s.getSubtask(ref, required), nil
}

// CreateSubtask appends a step; its position is allocated inside the
// creating transaction.
func (s *Store) CreateSubtask(task *row.Row, title string, tags []string, note, created string) (r *row.Row, err error) {
	defer guard(&err)
	return s.createSubtask(task, title, "todo", tags, note, created), nil
}

func (s *Store) createSubtask(task *row.Row, title, status string, tags []string, note, created string) *row.Row {
	title = strings.TrimSpace(title)
	if title == "" {
		fail(errs.New("a subtask needs a title"))
	}
	st := created
	if st == "" {
		st = refs.NowISO()
	} else {
		notBefore("--at", st, "the task's creation", task.Str("created_at"))
	}
	var id int64
	s.tx(func() {
		pos := s.count("SELECT COALESCE(MAX(position), 0) + 1 FROM subtasks WHERE task_id = ?", task.Int("id"))
		var started any
		if status == "in-progress" {
			started = st
		}
		id = s.insert("INSERT INTO subtasks (task_id, position, title, status, created_at, updated_at, started_at, uid) VALUES (?,?,?,?,?,?,?,?)",
			task.Int("id"), pos, title, status, st, st, started, NewUID())
		for _, tag := range tags {
			s.attachTag("subtask", id, tag)
		}
		if note != "" {
			s.insertNote("subtask", id, note, "", "note", 0)
		}
		s.event("subtask", id, "created", title, task.Int("id"), task.Int("project_id"), st)
	})
	return s.getSubtaskID(id)
}

// SubtaskStatusResult reports what a subtask transition did.
type SubtaskStatusResult struct {
	Subtask   *row.Row
	Changed   bool
	TaskMoved bool // the cascade pulled the parent into in-progress
}

// SetSubtaskStatus moves a step. Starting or finishing a step pulls a todo
// parent into in-progress in the SAME transaction (D9); finishing every step
// does not close the task.
func (s *Store) SetSubtaskStatus(subID int64, status string, cascade bool, at string) (res SubtaskStatusResult, err error) {
	defer guard(&err)
	s.tx(func() {
		sub := s.getSubtaskID(subID)
		if sub.Str("status") == status {
			res = SubtaskStatusResult{Subtask: sub}
			return
		}
		task := s.getTaskID(sub.Int("task_id"), "", true)
		stampAt := at
		if stampAt == "" {
			stampAt = refs.NowISO()
		}
		if at != "" {
			notBefore("--at", stampAt, "the step's creation", sub.Str("created_at"))
			if Closed(status) {
				notBefore("--at", stampAt, "the step's start", sub.Str("started_at"))
			}
		}
		started := sub.Get("started_at")
		if started == nil && status == "in-progress" {
			started = stampAt
		}
		var closedAt any
		if Closed(status) {
			closedAt = stampAt
		}
		s.exec("UPDATE subtasks SET status = ?, updated_at = ?, started_at = ?, closed_at = ? WHERE id = ?",
			status, stampAt, started, closedAt, subID)
		s.event("subtask", subID, "status", sub.Str("status")+" -> "+status, task.Int("id"), task.Int("project_id"), stampAt)
		moved := false
		if cascade && (status == "in-progress" || status == "done") && task.Str("status") == "todo" {
			_, moved = s.setTaskStatus(task.Int("id"), "in-progress", stampAt)
		}
		res = SubtaskStatusResult{Subtask: s.getSubtaskID(subID), Changed: true, TaskMoved: moved}
	})
	return res, nil
}

// UpdateSubtask retitles a step. expectTitle is UpdateTask's
// compare-and-set precondition.
func (s *Store) UpdateSubtask(sub *row.Row, fields []Field, expectTitle *string) (r *row.Row, err error) {
	defer guard(&err)
	task := s.getTaskID(sub.Int("task_id"), "", true)
	s.tx(func() {
		s.expectTitle("subtasks", "step", sub.Int("id"), expectTitle, fields)
		s.updateRow("subtasks", sub.Int("id"), fields)
		s.event("subtask", sub.Int("id"), "updated", describeFields(fields), task.Int("id"), task.Int("project_id"), "")
	})
	return s.getSubtaskID(sub.Int("id")), nil
}

// DeleteSubtask removes a step and its notes/taggings.
func (s *Store) DeleteSubtask(sub *row.Row) (err error) {
	defer guard(&err)
	task := s.getTaskID(sub.Int("task_id"), "", true)
	s.tx(func() {
		s.purgePolymorphic("subtask", []int64{sub.Int("id")})
		s.exec("DELETE FROM subtasks WHERE id = ?", sub.Int("id"))
		s.event("subtask", sub.Int("id"), "deleted", sub.Str("title"), task.Int("id"), task.Int("project_id"), "")
	})
	return nil
}

// ListSubtasks lists a task's steps. total is before limit (D3).
func (s *Store) ListSubtasks(taskID int64, statuses []string, limit int) (out []*row.Row, total int, err error) {
	defer guard(&err)
	out = s.listSubtasks(taskID, statuses, limit)
	if limit > 0 {
		total = len(s.listSubtasks(taskID, statuses, 0))
	} else {
		total = len(out)
	}
	return out, total, nil
}

func (s *Store) listSubtasks(taskID int64, statuses []string, limit int) []*row.Row {
	query := `SELECT s.*,
  (SELECT COUNT(*) FROM notes n WHERE n.target_type = 'subtask' AND n.target_id = s.id) AS n_notes,
  (SELECT group_concat(name, ',') FROM (SELECT g.name FROM taggings tg JOIN tags g ON g.id = tg.tag_id
     WHERE tg.target_type = 'subtask' AND tg.target_id = s.id ORDER BY g.name)) AS tag_list
FROM subtasks s WHERE s.task_id = ?`
	args := []any{taskID}
	if len(statuses) > 0 {
		query += " AND s.status IN (" + marks(len(statuses)) + ")"
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	query += " ORDER BY s.position"
	if limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", limit)
	}
	out := []*row.Row{}
	for _, r := range s.q(query, args...) {
		item := subtaskRow(r)
		item.Set("ref", fmt.Sprintf("%d.%d", taskID, r.Int("position")))
		item.Set("mark", refs.StatusMark[r.Str("status")])
		item.Set("tags", r.Str("tag_list"))
		item.Set("notes", r.Int("n_notes"))
		item.Set("created", refs.FmtTS(r.Str("created_at"), false, false))
		item.Set("updated", refs.FmtTS(r.Str("updated_at"), false, false))
		out = append(out, item)
	}
	return out
}

// SubtaskDetail is the `sub show` object.
func (s *Store) SubtaskDetail(sub *row.Row) (r *row.Row, err error) {
	defer guard(&err)
	return s.subtaskDetail(sub), nil
}

func (s *Store) subtaskDetail(sub *row.Row) *row.Row {
	item := sub.Copy()
	item.Set("ref", fmt.Sprintf("%d.%d", sub.Int("task_id"), sub.Int("position")))
	item.Set("mark", refs.StatusMark[sub.Str("status")])
	item.Set("tag_list", s.tagsFor("subtask", sub.Int("id")))
	item.Set("note_list", s.notesFor("subtask", sub.Int("id"), 0))
	item.Set("created", refs.FmtTS(sub.Str("created_at"), false, false))
	item.Set("updated", refs.FmtTS(sub.Str("updated_at"), false, false))
	return item
}

// SubtasksTagged lists subtask details carrying a tag (for `tag show`).
func (s *Store) SubtasksTagged(tagID int64) (out []*row.Row, err error) {
	defer guard(&err)
	for _, r := range s.q("SELECT s.* FROM subtasks s JOIN taggings tg ON tg.target_id = s.id"+
		" WHERE tg.target_type = 'subtask' AND tg.tag_id = ? ORDER BY s.task_id, s.position", tagID) {
		out = append(out, s.subtaskDetail(subtaskRow(r)))
	}
	return out, nil
}
