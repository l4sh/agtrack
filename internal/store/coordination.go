package store

import (
	"fmt"
	"strings"
	"time"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
)

// Multi-agent coordination (docs/agent-features-spec.md section C).

// Ackable are the note kinds that ask for an acknowledgement (C1).
var Ackable = map[string]bool{"handoff": true, "blocker": true, "question": true}

// AckNotes acknowledges notes in one transaction. Idempotent: an already
// acknowledged note keeps its first acknowledgement and reports changed=false.
// A non-empty reply is added as a reply under each note that changed.
func (s *Store) AckNotes(ids []int64, reply string) (out []*row.Row, err error) {
	defer guard(&err)
	if s.Actor == "" {
		fail(errs.New("an acknowledgement needs an actor: set AGTRACK_ACTOR or pass --actor"))
	}
	s.tx(func() {
		for _, id := range ids {
			n := s.one("SELECT * FROM notes WHERE id = ?", id)
			if n == nil {
				fail(errs.NotFound("note", fmt.Sprint(id), "no such note: %d", id))
			}
			if !Ackable[n.Str("kind")] {
				fail(errs.New("note %d is a %s note; only handoff, blocker and question notes are acknowledged", id, orNote(n.Str("kind"))))
			}
			changed := false
			if n.IsNull("acknowledged_at") {
				now := refs.NowISO()
				s.exec("UPDATE notes SET acknowledged_at = ?, acknowledged_by = ? WHERE id = ?", now, s.Actor, id)
				taskID, projectID := s.noteScope(n)
				s.event("note", id, "note-acked", firstLine(n.Str("body")), taskID, projectID, "")
				if reply != "" {
					s.addNote(NewNote{TargetType: n.Str("target_type"), TargetID: n.Int("target_id"), Body: reply,
						TaskID: taskID, ProjectID: projectID, ParentID: id, Kind: "note"})
				}
				changed = true
			}
			n = s.one("SELECT * FROM notes WHERE id = ?", id)
			out = append(out, row.Of("id", id, "kind", n.Str("kind"), "acknowledged_at", n.Get("acknowledged_at"),
				"acknowledged_by", n.Get("acknowledged_by"), "changed", changed))
		}
	})
	return out, nil
}

func orNote(k string) string {
	if k == "" {
		return "note"
	}
	return k
}

// CloseWithNote writes a handoff note on a task and, in the same
// transaction, finishes a step of that task or the task itself (C2).
// target is a step ref (task.position or id) or the task ref; it must belong
// to the note's task. closedRef names what was finished.
func (s *Store) CloseWithNote(in NewNote, target string) (note *row.Row, closedRef string, err error) {
	defer guard(&err)
	if in.TargetType != "task" {
		fail(errs.New("--close applies to task notes"))
	}
	s.tx(func() {
		var subID int64
		taskTarget := false
		if t, e := s.GetTask(target, false); e == nil && t != nil && t.Int("id") == in.TargetID && !strings.Contains(target, ".") {
			taskTarget = true
		} else if sub, e := s.GetSubtask(target, false); e == nil && sub != nil && sub.Int("task_id") == in.TargetID {
			subID = sub.Int("id")
			closedRef = fmt.Sprintf("%d.%d", in.TargetID, sub.Int("position"))
		} else {
			fail(errs.New("--close %s: not this task (#%d) or one of its steps", target, in.TargetID))
		}
		note = s.addNote(in)
		if taskTarget {
			s.setTaskStatus(in.TargetID, "done", "")
			closedRef = fmt.Sprint(in.TargetID)
		} else if _, err := s.SetSubtaskStatus(subID, "done", true, ""); err != nil {
			fail(err)
		}
	})
	return note, closedRef, nil
}

// TransferTask hands a task to another actor atomically (C4): the current
// claim is released, ACTOR gets a fresh claim with no session (the receiver's
// first write fills it in) and a handoff note records why.
func (s *Store) TransferTask(taskID int64, to, note string, ttl time.Duration, force bool) (r *row.Row, noteRow *row.Row, err error) {
	defer guard(&err)
	if s.Actor == "" {
		fail(errs.New("a transfer needs an actor: set AGTRACK_ACTOR or pass --actor"))
	}
	if to == "" || to == s.Actor {
		fail(errs.New("transfer --to needs another actor than yourself"))
	}
	s.tx(func() {
		t := s.one("SELECT * FROM tasks WHERE id = ?", taskID)
		if t == nil {
			fail(errs.NotFound("task", fmt.Sprint(taskID), "no such task: %d", taskID))
		}
		if Closed(t.Str("status")) {
			fail(errs.Conflict("task #%d is %s; nothing to transfer", taskID, t.Str("status")))
		}
		now := refs.Now()
		nowISO := refs.ToUTCISO(now)
		holder := t.Str("claimed_by")
		live := holder != "" && (t.IsNull("claim_expires_at") || t.Str("claim_expires_at") > nowISO)
		if live && holder != s.Actor && !force {
			fail(errs.Conflict("task #%d is claimed by %s, not you; pass --force to transfer it anyway", taskID, holder))
		}
		pid := t.Int("project_id")
		if holder != "" {
			s.event("task", taskID, "release", holder, taskID, pid, "")
		}
		var expires any
		if ttl > 0 {
			expires = refs.ToUTCISO(now.Add(ttl))
		}
		s.exec("UPDATE tasks SET claimed_by = ?, claimed_at = ?, claim_expires_at = ?, claim_session = NULL, claim_last_seen = ? WHERE id = ?",
			to, nowISO, expires, nowISO, taskID)
		s.event("task", taskID, "claim", "transferred to "+to, taskID, pid, "")
		body := note
		if body == "" {
			body = fmt.Sprintf("Transferred to %s by %s.", to, s.Actor)
		}
		noteRow = s.addNote(NewNote{TargetType: "task", TargetID: taskID, Body: body, TaskID: taskID, ProjectID: pid, Kind: "handoff"})
		r = s.getTaskID(taskID, "", true)
	})
	return r, noteRow, nil
}

// ClaimsFilter narrows Claims.
type ClaimsFilter struct {
	ProjectIDs []int64   // empty = everywhere
	TaskID     int64     // 0 = any
	StaleSince time.Time // zero = every live claim; else only holders quiet since before this
}

// Claims lists live claims with holder, session, age, last_seen and the last
// event on the task (C3). A claim that never wrote anything counts as seen
// when it was taken.
func (s *Store) Claims(f ClaimsFilter) (out []*row.Row, err error) {
	defer guard(&err)
	where := []string{"t.claimed_by IS NOT NULL", "(t.claim_expires_at IS NULL OR t.claim_expires_at > ?)"}
	args := []any{refs.NowISO()}
	if f.TaskID != 0 {
		where = append(where, "t.id = ?")
		args = append(args, f.TaskID)
	} else if len(f.ProjectIDs) > 0 {
		where = append(where, "t.project_id IN ("+marks(len(f.ProjectIDs))+")")
		args = append(args, ids64(f.ProjectIDs)...)
	}
	if !f.StaleSince.IsZero() {
		where = append(where, "COALESCE(t.claim_last_seen, t.claimed_at) < ?")
		args = append(args, refs.ToUTCISO(f.StaleSince))
	}
	out = []*row.Row{}
	now := refs.Now()
	for _, t := range s.q(`SELECT t.id, t.title, t.status, t.project_id, t.claimed_by, t.claim_session, t.claimed_at, t.claim_expires_at, t.claim_last_seen,
 (SELECT e.action || ' ' || e.at FROM events e WHERE e.task_id = t.id ORDER BY e.id DESC LIMIT 1) AS last_event
FROM tasks t WHERE `+joinAnd(where)+` ORDER BY t.claimed_at, t.id`, args...) {
		seen := t.Str("claim_last_seen")
		if seen == "" {
			seen = t.Str("claimed_at")
		}
		r := row.Of("id", t.Int("id"), "title", t.Str("title"), "holder", t.Get("claimed_by"), "session", t.Get("claim_session"),
			"claimed_at", t.Get("claimed_at"), "claim_expires_at", t.Get("claim_expires_at"),
			"last_seen", seen, "last_event", t.Get("last_event"))
		r.Set("age", ageSince(now, t.Str("claimed_at")))
		r.Set("quiet", ageSince(now, seen))
		if p := s.getProject(t.Int("project_id")); p != nil {
			r.Set("project", s.projectPath(p))
		}
		out = append(out, r)
	}
	return out, nil
}

func ageSince(now time.Time, iso string) string {
	ts, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		return ""
	}
	d := now.Sub(ts).Round(time.Minute)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
