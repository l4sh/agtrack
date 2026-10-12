package store

import (
	"fmt"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
)

// --- checkpoints (agent-features B2) ---------------------------------------------------
//
// A checkpoint is a versioned snapshot of a task's working state: a markdown
// body plus a one-line next action. Versions are numbered per task (n) and
// never overwritten.

// CheckpointMaxBytes caps a checkpoint body.
const CheckpointMaxBytes = 64 << 10

const checkpointCols = "id, task_id, n, body, next, actor, session, created_at"

// AddCheckpoint stores the next version of a task's checkpoint.
func (s *Store) AddCheckpoint(task *row.Row, body, next string) (r *row.Row, err error) {
	defer guard(&err)
	body = strings.TrimRight(body, "\n")
	next = strings.TrimSpace(next)
	if strings.ContainsAny(next, "\n\r") {
		fail(errs.New("--next is one line; put the detail in the body"))
	}
	if strings.TrimSpace(body) == "" && next == "" {
		fail(errs.New("a checkpoint needs a body (--body, --file or stdin) or --next"))
	}
	if len(body) > CheckpointMaxBytes {
		fail(errs.New("checkpoint body is %d bytes; the limit is %d (64 KiB): keep the working state, link the rest", len(body), CheckpointMaxBytes))
	}
	id := task.Int("id")
	s.tx(func() {
		n := s.count("SELECT COALESCE(MAX(n), 0) + 1 FROM checkpoints WHERE task_id = ?", id)
		cid := s.insert("INSERT INTO checkpoints (task_id, n, body, next, actor, session, created_at) VALUES (?,?,?,?,?,?,?)",
			id, n, body, nullable(next), nullable(s.Actor), nullable(s.Session), refs.NowISO())
		detail := fmt.Sprintf("v%d", n)
		if next != "" {
			detail += " next: " + firstLine(next)
		}
		s.event("task", id, "checkpoint", detail, id, task.Int("project_id"), "")
		r = s.one("SELECT "+checkpointCols+" FROM checkpoints WHERE id = ?", cid)
	})
	return r, nil
}

// Checkpoint returns version n of a task's checkpoint (n <= 0: the latest),
// or nil when there is none.
func (s *Store) Checkpoint(taskID int64, n int64) (r *row.Row, err error) {
	defer guard(&err)
	if n > 0 {
		return s.one("SELECT "+checkpointCols+" FROM checkpoints WHERE task_id = ? AND n = ?", taskID, n), nil
	}
	return s.one("SELECT "+checkpointCols+" FROM checkpoints WHERE task_id = ? ORDER BY n DESC LIMIT 1", taskID), nil
}

// CheckpointByID fetches one checkpoint by its row id (idempotent replay).
func (s *Store) CheckpointByID(id int64) (r *row.Row, err error) {
	defer guard(&err)
	r = s.one("SELECT "+checkpointCols+" FROM checkpoints WHERE id = ?", id)
	if r == nil {
		fail(errs.NotFound("checkpoint", fmt.Sprint(id), "no such checkpoint: %d", id))
	}
	return r, nil
}

// Checkpoints lists a task's checkpoint versions, newest first, without
// bodies (bytes says how large each one is).
func (s *Store) Checkpoints(taskID int64) (out []*row.Row, err error) {
	defer guard(&err)
	out = []*row.Row{}
	out = append(out, s.q("SELECT id, task_id, n, next, actor, session, created_at, length(body) AS bytes FROM checkpoints WHERE task_id = ? ORDER BY n DESC", taskID)...)
	return out, nil
}

// CheckpointsSince lists a task's checkpoints written at or after a time,
// oldest first, without bodies (delta resume).
func (s *Store) CheckpointsSince(taskID int64, since string) (out []*row.Row, err error) {
	defer guard(&err)
	out = []*row.Row{}
	out = append(out, s.q("SELECT id, task_id, n, next, actor, session, created_at, length(body) AS bytes FROM checkpoints WHERE task_id = ? AND created_at >= ? ORDER BY n", taskID, since)...)
	return out, nil
}
