package store

import (
	"fmt"
	"strings"
	"time"

	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
)

// Read-only queries behind the stable `resume` packet (agent-features B6).
// The rules and lessons queries are deliberately minimal (active rows by
// scope, titles only); the rules/lessons feature (H) replaces them with its
// own resolution.

// UnackedHandoffs lists unresolved, unacknowledged handoff, blocker and
// question notes on a task (and its steps) and on the project itself,
// oldest first. taskID 0 = project only.
func (s *Store) UnackedHandoffs(projectID, taskID int64) (out []*row.Row, err error) {
	defer guard(&err)
	where := "(n.target_type = 'project' AND n.target_id = ?)"
	args := []any{projectID}
	if taskID != 0 {
		where = "(" + where + " OR (n.target_type = 'task' AND n.target_id = ?) OR (n.target_type = 'subtask' AND n.target_id IN (SELECT id FROM subtasks WHERE task_id = ?)))"
		args = append(args, taskID, taskID)
	}
	out = []*row.Row{}
	out = append(out, s.q(`SELECT n.id, n.kind, n.target_type, n.target_id, n.actor, n.body, n.created_at FROM notes n
WHERE `+where+` AND n.kind IN ('handoff','blocker','question') AND n.resolved_at IS NULL
  AND n.acknowledged_at IS NULL AND n.parent_id IS NULL ORDER BY n.created_at, n.id`, args...)...)
	return out, nil
}

// NotesSince lists notes written at or after a time on a task (and its
// steps) or, with taskID 0, anywhere in the project tree (delta resume).
func (s *Store) NotesSince(projectID, taskID int64, since string) (out []*row.Row, err error) {
	defer guard(&err)
	var where string
	var args []any
	if taskID != 0 {
		where = "((n.target_type = 'task' AND n.target_id = ?) OR (n.target_type = 'subtask' AND n.target_id IN (SELECT id FROM subtasks WHERE task_id = ?)) OR (n.target_type = 'project' AND n.target_id = ?))"
		args = []any{taskID, taskID, projectID}
	} else {
		ids := s.descendantIDs(projectID)
		m := marks(len(ids))
		where = "((n.target_type = 'project' AND n.target_id IN (" + m + ")) OR (n.target_type = 'task' AND n.target_id IN (SELECT id FROM tasks WHERE project_id IN (" + m + "))) OR (n.target_type = 'subtask' AND n.target_id IN (SELECT s.id FROM subtasks s JOIN tasks t ON t.id = s.task_id WHERE t.project_id IN (" + m + "))))"
		for i := 0; i < 3; i++ {
			args = append(args, ids64(ids)...)
		}
	}
	args = append(args, since)
	out = []*row.Row{}
	out = append(out, s.q(`SELECT n.id, n.kind, n.target_type, n.target_id, n.actor, n.body, n.created_at, n.resolved_at, n.acknowledged_at FROM notes n
WHERE `+where+` AND n.created_at >= ? ORDER BY n.created_at, n.id`, args...)...)
	return out, nil
}

// EventTime is the write time of the event a cursor points at (the latest
// event at or before it), or "" for a cursor before the first event.
func (s *Store) EventTime(cursor int64) (at string, err error) {
	defer guard(&err)
	r := s.one("SELECT COALESCE(recorded_at, at) AS t FROM events WHERE id <= ? ORDER BY id DESC LIMIT 1", cursor)
	if r == nil {
		return "", nil
	}
	return r.Str("t"), nil
}

// LastEvidence is the evidence recorded with the task's latest close (the
// newest status event that carries evidence).
func (s *Store) LastEvidence(taskID int64) (out []*row.Row, err error) {
	defer guard(&err)
	out = []*row.Row{}
	out = append(out, s.q(`SELECT id, subtask_id, event_id, kind, value, actor, created_at FROM task_evidence
WHERE task_id = ?1 AND COALESCE(event_id, 0) = (SELECT COALESCE(MAX(event_id), 0) FROM task_evidence WHERE task_id = ?1)
ORDER BY id`, taskID)...)
	return out, nil
}

// ProjectClaims lists the live claims on open tasks in a project tree.
func (s *Store) ProjectClaims(projectID int64) (out []*row.Row, err error) {
	defer guard(&err)
	ids := s.descendantIDs(projectID)
	args := append(ids64(ids), refs.NowISO())
	out = []*row.Row{}
	out = append(out, s.q(`SELECT id, title, status, claimed_by, claim_session, claimed_at, claim_expires_at, claim_last_seen FROM tasks
WHERE project_id IN (`+marks(len(ids))+`) AND status IN ('todo','in-progress') AND claimed_by IS NOT NULL
  AND (claim_expires_at IS NULL OR claim_expires_at > ?) ORDER BY claimed_at DESC, id`, args...)...)
	return out, nil
}

// scopeChain is the scope ladder for a task or project, narrowest first:
// task, project and its ancestors, context, global.
func (s *Store) scopeChain(projectID, taskID int64) (where string, args []any, rank string) {
	var parts []string
	var rankParts []string
	if taskID != 0 {
		parts = append(parts, "(scope_type = 'task' AND scope_id = ?)")
		args = append(args, taskID)
		rankParts = append(rankParts, "WHEN scope_type = 'task' THEN 0")
	}
	var chain []int64
	ctxID := int64(0)
	for id := projectID; id != 0; {
		p := s.one("SELECT id, parent_id, context_id FROM projects WHERE id = ?", id)
		if p == nil {
			break
		}
		chain = append(chain, id)
		ctxID = p.Int("context_id")
		id = p.Int("parent_id")
	}
	if len(chain) > 0 {
		parts = append(parts, "(scope_type = 'project' AND scope_id IN ("+marks(len(chain))+"))")
		args = append(args, ids64(chain)...)
		rankParts = append(rankParts, "WHEN scope_type = 'project' THEN 1")
	}
	if ctxID != 0 {
		parts = append(parts, "(scope_type = 'context' AND scope_id = ?)")
		args = append(args, ctxID)
		rankParts = append(rankParts, "WHEN scope_type = 'context' THEN 2")
	}
	parts = append(parts, "scope_type = 'global'")
	rank = "CASE " + strings.Join(rankParts, " ") + " ELSE 3 END"
	return "(" + strings.Join(parts, " OR ") + ")", args, rank
}

// ResumeRules lists the active rules that bind a task or project: deny,
// require and hard rules, plus allow rules added in the last 7 days,
// narrowest scope first. total is before limit.
func (s *Store) ResumeRules(projectID, taskID int64, limit int) (out []*row.Row, total int, err error) {
	defer guard(&err)
	where, args, rank := s.scopeChain(projectID, taskID)
	now := refs.NowISO()
	week := refs.ToUTCISO(refs.Now().Add(-7 * 24 * time.Hour))
	cond := " FROM rules WHERE " + where + ` AND status = 'active' AND (expires_at IS NULL OR expires_at > ?)
  AND (kind IN ('deny','require') OR hard = 1 OR (kind = 'allow' AND created_at >= ?))`
	args = append(args, now, week)
	total = int(s.count("SELECT COUNT(*)"+cond, args...))
	q := "SELECT id, kind, verb, check_name, text, scope_type, scope_id, hard" + cond + " ORDER BY " + rank + ", hard DESC, id"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	out = []*row.Row{}
	out = append(out, s.q(q, args...)...)
	return out, total, nil
}

// ResumeLessons lists active lessons for a task or project, titles only,
// narrowest scope first, newest first within a scope. total is before limit.
func (s *Store) ResumeLessons(projectID, taskID int64, limit int) (out []*row.Row, total int, err error) {
	defer guard(&err)
	where, args, rank := s.scopeChain(projectID, taskID)
	cond := " FROM lessons WHERE " + where + " AND status = 'active'"
	total = int(s.count("SELECT COUNT(*)"+cond, args...))
	q := "SELECT id, title, kind, scope_type, scope_id, verified_at" + cond + " ORDER BY " + rank + ", id DESC"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	out = []*row.Row{}
	out = append(out, s.q(q, args...)...)
	return out, total, nil
}
