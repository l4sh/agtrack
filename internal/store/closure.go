package store

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
)

// Closure and hygiene (agent-features spec A1-A5): staleness, evidence on
// done, parked tasks.

// DefaultStaleDays is the idle threshold when `next.stale_days` is unset.
const DefaultStaleDays = 7

// StaleDays reads config `next.stale_days`.
func (s *Store) StaleDays() int {
	v, ok, err := s.GetMeta("config:next.stale_days")
	if err != nil || !ok {
		return DefaultStaleDays
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 {
		return DefaultStaleDays
	}
	return n
}

// OpenStepCount is the number of todo/in-progress steps of a task.
func (s *Store) OpenStepCount(taskID int64) (n int, err error) {
	defer guard(&err)
	return int(s.count("SELECT COUNT(*) FROM subtasks WHERE task_id = ? AND status IN ('todo','in-progress')", taskID)), nil
}

// Staleness describes how long a task has been going and why it may be stale
// (A2). The returned row has age, last_event, open_steps and stale_reason
// (null when fresh). idleDays <= 0 means the configured default.
func (s *Store) Staleness(task *row.Row, idleDays int) (r *row.Row, err error) {
	defer guard(&err)
	if idleDays <= 0 {
		idleDays = s.StaleDays()
	}
	return s.staleness(task, idleDays), nil
}

// TaskStaleness is Staleness by task id (staleDays <= 0 = configured default).
// It is the shared entry point for `next`, `done-check` and `resume`.
func (s *Store) TaskStaleness(taskID int64, staleDays int) (r *row.Row, err error) {
	defer guard(&err)
	task := s.getTaskID(taskID, fmt.Sprint(taskID), true)
	if staleDays <= 0 {
		staleDays = s.StaleDays()
	}
	return s.staleness(task, staleDays), nil
}

func (s *Store) staleness(task *row.Row, idleDays int) *row.Row {
	id := task.Int("id")
	now := refs.Now()
	nowISO := refs.ToUTCISO(now)
	since := task.Str("started_at")
	if since == "" {
		since = task.Str("created_at")
	}
	last := ""
	if v := s.one("SELECT MAX(at) AS at FROM events WHERE task_id = ?", id); v != nil {
		last = v.Str("at")
	}
	if last == "" {
		last = task.Str("updated_at")
	}
	agg := s.one(`SELECT
  (SELECT COUNT(*) FROM subtasks WHERE task_id = ?1) AS total,
  (SELECT COUNT(*) FROM subtasks WHERE task_id = ?1 AND status IN ('todo','in-progress')) AS open`, id)
	open, total := agg.Int("open"), agg.Int("total")
	var reason any
	if task.Str("status") == "in-progress" {
		lastT, ok := parseStored(last)
		switch {
		case total > 0 && open == 0:
			reason = "all-steps-done"
		case !task.IsNull("claimed_by") && task.Has("claim_expires_at") && !task.IsNull("claim_expires_at") &&
			task.Str("claim_expires_at") <= nowISO:
			reason = "claim-expired"
		case ok && now.Sub(lastT) > time.Duration(idleDays)*24*time.Hour:
			reason = fmt.Sprintf("idle>%dd", idleDays)
		}
	}
	age := ""
	if t, ok := parseStored(since); ok {
		age = HumanAge(now.Sub(t))
	}
	return row.Of("age", age, "last_event", last, "open_steps", int64(open), "stale_reason", reason)
}

func parseStored(v string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// HumanAge renders a duration as 3d, 5h or 12m.
func HumanAge(d time.Duration) string {
	switch {
	case d < 0:
		return "0m"
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}

// --- evidence (A4) -----------------------------------------------------------------

// EvidenceKinds are the accepted `--evidence KIND:VALUE` kinds.
var EvidenceKinds = []string{"commit", "branch", "pr", "test", "file", "url", "note"}

// Evidence is one KIND:VALUE pair.
type Evidence struct{ Kind, Value string }

// ParseEvidence parses `KIND:VALUE`.
func ParseEvidence(v string) (Evidence, error) {
	kind, value, ok := strings.Cut(v, ":")
	kind, value = strings.ToLower(strings.TrimSpace(kind)), strings.TrimSpace(value)
	valid := false
	for _, k := range EvidenceKinds {
		valid = valid || k == kind
	}
	if !ok || !valid || value == "" {
		return Evidence{}, errs.New("invalid --evidence %q: expected KIND:VALUE with KIND one of %s (e.g. commit:abc123, test:go test ./...=0)",
			v, strings.Join(EvidenceKinds, ", "))
	}
	return Evidence{kind, value}, nil
}

// AddEvidence stores evidence linked to the latest status event of the target
// (a task, or a subtask with subID set). Call it right after the transition.
func (s *Store) AddEvidence(taskID, subID int64, items []Evidence) (err error) {
	defer guard(&err)
	if len(items) == 0 {
		return nil
	}
	s.tx(func() {
		targetType, targetID := "task", taskID
		if subID > 0 {
			targetType, targetID = "subtask", subID
		}
		var eventID any
		if e := s.one("SELECT id FROM events WHERE target_type = ? AND target_id = ? AND action = 'status' ORDER BY id DESC LIMIT 1", targetType, targetID); e != nil {
			eventID = e.Int("id")
		}
		for _, it := range items {
			s.exec("INSERT INTO task_evidence (task_id, subtask_id, event_id, kind, value, actor, created_at) VALUES (?,?,?,?,?,?,?)",
				taskID, nullableID(subID), eventID, it.Kind, it.Value, nullable(s.Actor), refs.NowISO())
		}
	})
	return nil
}

// EvidenceFor lists a task's evidence (steps' included), oldest first.
func (s *Store) EvidenceFor(taskID int64) (out []*row.Row, err error) {
	defer guard(&err)
	out = []*row.Row{}
	for _, r := range s.q("SELECT id, subtask_id, event_id, kind, value, actor, created_at FROM task_evidence WHERE task_id = ? ORDER BY id", taskID) {
		out = append(out, r)
	}
	return out, nil
}

// LastCloseEvidence is the evidence recorded with the task's latest close.
func (s *Store) LastCloseEvidence(taskID int64) (out []*row.Row, err error) {
	defer guard(&err)
	out = []*row.Row{}
	ev := s.one("SELECT id FROM events WHERE target_type = 'task' AND target_id = ? AND action = 'status' AND detail LIKE '% -> done' ORDER BY id DESC LIMIT 1", taskID)
	if ev == nil {
		return out, nil
	}
	for _, r := range s.q("SELECT id, subtask_id, event_id, kind, value, actor, created_at FROM task_evidence WHERE task_id = ? AND event_id = ? ORDER BY id", taskID, ev.Int("id")) {
		out = append(out, r)
	}
	return out, nil
}

// HasBranchNoPR reports whether a task has a branch artifact and no PR.
func (s *Store) HasBranchNoPR(taskID int64) (yes bool, err error) {
	defer guard(&err)
	return s.count("SELECT COUNT(*) FROM artifacts WHERE task_id = ? AND kind = 'branch'", taskID) > 0 &&
		s.count("SELECT COUNT(*) FROM pull_requests WHERE task_id = ?", taskID) == 0, nil
}

// --- parked tasks (A5) -------------------------------------------------------------

// SnoozeTask parks a task until the given UTC timestamp.
func (s *Store) SnoozeTask(taskID int64, until, note string) (r *row.Row, err error) {
	defer guard(&err)
	s.tx(func() {
		t := s.getTaskID(taskID, fmt.Sprint(taskID), true)
		if Closed(t.Str("status")) {
			fail(errs.Conflict("task #%d is %s; only open tasks can be parked", taskID, t.Str("status")))
		}
		s.exec("UPDATE tasks SET snoozed_until = ?, snooze_note = ? WHERE id = ?", until, nullable(note), taskID)
		detail := "until " + until
		if note != "" {
			detail += ": " + note
		}
		s.event("task", taskID, "snooze", detail, taskID, t.Int("project_id"), "")
		r = s.getTaskID(taskID, "", true)
	})
	return r, nil
}

// UnsnoozeTask clears a park. changed is false when it was not parked.
func (s *Store) UnsnoozeTask(taskID int64) (r *row.Row, changed bool, err error) {
	defer guard(&err)
	s.tx(func() {
		t := s.getTaskID(taskID, fmt.Sprint(taskID), true)
		r = t
		if t.IsNull("snoozed_until") || !t.Has("snoozed_until") {
			return
		}
		s.exec("UPDATE tasks SET snoozed_until = NULL, snooze_note = NULL WHERE id = ?", taskID)
		s.event("task", taskID, "unsnooze", t.Str("snoozed_until"), taskID, t.Int("project_id"), "")
		changed = true
		r = s.getTaskID(taskID, "", true)
	})
	return r, changed, nil
}
