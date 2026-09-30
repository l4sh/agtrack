package store

import (
	"fmt"
	"time"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
)

// --- idempotency receipts (design §4.4) ----------------------------------------------
//
// A receipt is written in the same transaction as the effects it covers and
// checked before any other precondition, so a retry after a successful create
// returns the original result instead of failing because the world moved on.

// Receipt is a recorded create.
type Receipt struct {
	TargetType string
	TargetIDs  []int64
}

// CheckReceipt looks a key up. ok=false: not seen. A key reused with a
// different payload is a caller bug (request_key_reused).
func (s *Store) CheckReceipt(key, payloadSHA string) (rc *Receipt, ok bool, err error) {
	defer guard(&err)
	r := s.one("SELECT * FROM idempotency WHERE key = ?", key)
	if r == nil {
		return nil, false, nil
	}
	if r.Str("payload_sha") != payloadSHA {
		e := errs.Conflict("idempotency key %s was already used for a different request; pick a new key", key)
		e.Token = errs.TokRequestKeyReused
		e.Ref = key
		fail(e)
	}
	return &Receipt{TargetType: r.Str("target_type"), TargetIDs: parseIDs(r.Str("output"), r.Int("target_id"))}, true, nil
}

// WriteReceipt records a key inside the caller's transaction.
func (s *Store) WriteReceipt(key, payloadSHA, targetType string, ids []int64) (err error) {
	defer guard(&err)
	var first int64
	if len(ids) > 0 {
		first = ids[0]
	}
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprint(id)
	}
	if _, e := s.execC("INSERT INTO idempotency (key, target_type, target_id, payload_sha, output, actor, created_at) VALUES (?,?,?,?,?,?,?)",
		key, targetType, first, payloadSHA, out, nullable(s.Actor), refs.NowISO()); e != nil {
		// Lost a race with an identical retry: the whole tx rolls back and
		// the caller re-checks.
		fail(errs.Conflict("idempotency key %s was recorded concurrently; retry", key))
	}
	return nil
}

func parseIDs(s string, fallback int64) []int64 {
	var out []int64
	var cur int64
	have := false
	for _, ch := range s {
		if ch == ',' {
			if have {
				out = append(out, cur)
			}
			cur, have = 0, false
			continue
		}
		if ch >= '0' && ch <= '9' {
			cur = cur*10 + int64(ch-'0')
			have = true
		}
	}
	if have {
		out = append(out, cur)
	}
	if len(out) == 0 {
		out = []int64{fallback}
	}
	return out
}

// --- claims (design §4.6) ------------------------------------------------------------------

// ClaimResult reports a claim attempt.
type ClaimResult struct {
	Task    *row.Row
	Stolen  string // previous live holder when --steal took it over
	Renewed bool
}

// ClaimTask takes an advisory, expiring claim. A live claim held by someone
// else is a conflict unless steal is set. ttl <= 0 means no expiry.
func (s *Store) ClaimTask(taskID int64, ttl time.Duration, steal bool) (res ClaimResult, err error) {
	return s.claimTask(taskID, ttl, steal, false)
}

// ClaimDoable claims only a task that is still open and unblocked at the
// moment of the claim (for `next --claim`: the list it chose from may be stale).
func (s *Store) ClaimDoable(taskID int64, ttl time.Duration) (res ClaimResult, err error) {
	return s.claimTask(taskID, ttl, false, true)
}

func (s *Store) claimTask(taskID int64, ttl time.Duration, steal, doable bool) (res ClaimResult, err error) {
	defer guard(&err)
	if s.Actor == "" {
		fail(errs.New("a claim needs an actor: set AGTRACK_ACTOR or pass --actor"))
	}
	s.tx(func() {
		t := s.one("SELECT * FROM tasks WHERE id = ?", taskID)
		if t == nil {
			fail(errs.NotFound("task", fmt.Sprint(taskID), "no such task: %d", taskID))
		}
		if doable {
			if Closed(t.Str("status")) || s.count("SELECT COUNT(*) FROM task_deps d JOIN tasks b ON b.id = d.depends_on_id WHERE d.task_id = ? AND d.kind = 'blocks' AND b.status <> 'done'", taskID) > 0 {
				fail(errs.Conflict("task #%d was closed or blocked meanwhile", taskID))
			}
		}
		now := refs.Now()
		nowISO := refs.ToUTCISO(now)
		holder := t.Str("claimed_by")
		live := holder != "" && (t.IsNull("claim_expires_at") || t.Str("claim_expires_at") > nowISO)
		if live && holder != s.Actor {
			if !steal {
				until := "indefinitely"
				if !t.IsNull("claim_expires_at") {
					until = "until " + refs.FmtTS(t.Str("claim_expires_at"), false, false)
				}
				e := errs.Conflict("task #%d is claimed by %s %s; pass --steal, or pick another", taskID, holder, until)
				e.Token = "claimed"
				e.Extra = map[string]any{"claimed_by": holder, "claim_expires_at": t.Get("claim_expires_at")}
				fail(e)
			}
			res.Stolen = holder
		}
		res.Renewed = live && holder == s.Actor
		var expires any
		if ttl > 0 {
			expires = refs.ToUTCISO(now.Add(ttl))
		}
		s.exec("UPDATE tasks SET claimed_by = ?, claimed_at = ?, claim_expires_at = ? WHERE id = ?", s.Actor, nowISO, expires, taskID)
		detail := "claimed"
		if expires != nil {
			detail += " until " + expires.(string)
		}
		if res.Stolen != "" {
			detail += " (from " + res.Stolen + ")"
		}
		s.event("task", taskID, "claim", detail, taskID, t.Int("project_id"), "")
		res.Task = s.getTaskID(taskID, "", true)
	})
	return res, nil
}

// ReleaseTask drops this actor's claim (or any claim with force).
func (s *Store) ReleaseTask(taskID int64, force bool) (r *row.Row, changed bool, err error) {
	defer guard(&err)
	s.tx(func() {
		t := s.one("SELECT * FROM tasks WHERE id = ?", taskID)
		if t == nil {
			fail(errs.NotFound("task", fmt.Sprint(taskID), "no such task: %d", taskID))
		}
		holder := t.Str("claimed_by")
		if holder == "" {
			r = taskRow(t)
			return
		}
		if holder != s.Actor && !force {
			fail(errs.Conflict("task #%d is claimed by %s, not you; pass --force to release it anyway", taskID, holder))
		}
		s.exec("UPDATE tasks SET claimed_by = NULL, claimed_at = NULL, claim_expires_at = NULL WHERE id = ?", taskID)
		s.event("task", taskID, "release", holder, taskID, t.Int("project_id"), "")
		changed = true
		r = s.getTaskID(taskID, "", true)
	})
	return r, changed, nil
}

// --- dependencies (design §4.8) ---------------------------------------------------------------

// DepKinds are the task_deps kinds.
var DepKinds = []string{"blocks", "relates", "duplicates"}

// AddDep records that taskID depends on dependsOn. `blocks` edges are cycle
// checked inside the write transaction.
func (s *Store) AddDep(taskID, dependsOn int64, kind, reason string) (changed bool, err error) {
	defer guard(&err)
	if kind == "" {
		kind = "blocks"
	}
	if taskID == dependsOn {
		fail(errs.New("a task cannot depend on itself"))
	}
	s.tx(func() {
		t := s.getTaskID(taskID, fmt.Sprint(taskID), true)
		s.getTaskID(dependsOn, fmt.Sprint(dependsOn), true)
		if kind == "blocks" {
			// Would dependsOn (transitively) wait on taskID?
			if s.count(`WITH RECURSIVE up(id) AS (SELECT ? UNION SELECT d.depends_on_id FROM task_deps d JOIN up ON d.task_id = up.id WHERE d.kind = 'blocks')
SELECT COUNT(*) FROM up WHERE id = ?`, dependsOn, taskID) > 0 {
				fail(errs.Conflict("#%d already waits on #%d; blocking it on #%d would make a cycle", dependsOn, taskID, dependsOn))
			}
		}
		res := s.exec("INSERT INTO task_deps (task_id, depends_on_id, kind, reason, actor, created_at) VALUES (?,?,?,?,?,?) ON CONFLICT DO NOTHING",
			taskID, dependsOn, kind, nullable(reason), nullable(s.Actor), refs.NowISO())
		if n, _ := res.RowsAffected(); n > 0 {
			changed = true
			s.event("task", taskID, "dep-added", fmt.Sprintf("%s #%d", kind, dependsOn), taskID, t.Int("project_id"), "")
		}
	})
	return changed, nil
}

// RemoveDep drops a dependency.
func (s *Store) RemoveDep(taskID, dependsOn int64, kind string) (changed bool, err error) {
	defer guard(&err)
	s.tx(func() {
		t := s.getTaskID(taskID, fmt.Sprint(taskID), true)
		q := "DELETE FROM task_deps WHERE task_id = ? AND depends_on_id = ?"
		args := []any{taskID, dependsOn}
		if kind != "" {
			q += " AND kind = ?"
			args = append(args, kind)
		}
		res := s.exec(q, args...)
		if n, _ := res.RowsAffected(); n > 0 {
			changed = true
			s.event("task", taskID, "dep-removed", fmt.Sprintf("#%d", dependsOn), taskID, t.Int("project_id"), "")
		}
	})
	return changed, nil
}

// DepsOf lists what a task depends on, with the prerequisite's status.
func (s *Store) DepsOf(taskID int64) (out []*row.Row, err error) {
	defer guard(&err)
	out = []*row.Row{}
	for _, r := range s.q(`SELECT d.task_id, d.depends_on_id, d.kind, t.status, t.title, d.reason, d.created_at,
  (d.kind = 'blocks' AND t.status <> 'done') AS unmet
FROM task_deps d JOIN tasks t ON t.id = d.depends_on_id WHERE d.task_id = ? ORDER BY d.depends_on_id`, taskID) {
		r.Set("unmet", r.Int("unmet") == 1)
		out = append(out, r)
	}
	return out, nil
}

// Dependents lists the tasks waiting on a task.
func (s *Store) Dependents(taskID int64) (out []*row.Row, err error) {
	defer guard(&err)
	out = []*row.Row{}
	for _, r := range s.q(`SELECT d.task_id, d.depends_on_id, d.kind, t.status, t.title, d.reason, d.created_at
FROM task_deps d JOIN tasks t ON t.id = d.task_id WHERE d.depends_on_id = ? ORDER BY d.task_id`, taskID) {
		out = append(out, r)
	}
	return out, nil
}

// --- artifacts (design §6.2) --------------------------------------------------------------------

// ArtifactKinds are the artifact kinds.
var ArtifactKinds = []string{"branch", "commit", "file", "url", "run", "worktree"}

// AddArtifact records a branch/commit/file/... on a task (idempotent on
// task+kind+value; a new label updates it).
func (s *Store) AddArtifact(task *row.Row, kind, value, label string) (r *row.Row, err error) {
	defer guard(&err)
	s.tx(func() {
		res := s.exec("INSERT INTO artifacts (task_id, kind, value, label, actor, created_at) VALUES (?,?,?,?,?,?) ON CONFLICT(task_id, kind, value) DO NOTHING",
			task.Int("id"), kind, value, nullable(label), nullable(s.Actor), refs.NowISO())
		if n, _ := res.RowsAffected(); n > 0 {
			s.event("task", task.Int("id"), "artifact", kind+" "+value, task.Int("id"), task.Int("project_id"), "")
		} else if label != "" {
			s.exec("UPDATE artifacts SET label = ? WHERE task_id = ? AND kind = ? AND value = ?", label, task.Int("id"), kind, value)
		}
	})
	return s.one("SELECT id, task_id, kind, value, label, actor, created_at FROM artifacts WHERE task_id = ? AND kind = ? AND value = ?",
		task.Int("id"), kind, value), nil
}

// RemoveArtifact deletes an artifact by id.
func (s *Store) RemoveArtifact(taskID, id int64) (err error) {
	defer guard(&err)
	s.tx(func() {
		a := s.one("SELECT a.*, t.project_id FROM artifacts a JOIN tasks t ON t.id = a.task_id WHERE a.id = ? AND a.task_id = ?", id, taskID)
		if a == nil {
			fail(errs.NotFound("artifact", fmt.Sprint(id), "no such artifact on task #%d: %d", taskID, id))
		}
		s.exec("DELETE FROM artifacts WHERE id = ?", id)
		s.event("task", a.Int("task_id"), "artifact-removed", a.Str("kind")+" "+a.Str("value"), a.Int("task_id"), a.Int("project_id"), "")
	})
	return nil
}

// Artifacts lists a task's artifacts.
func (s *Store) Artifacts(taskID int64) (out []*row.Row, err error) {
	defer guard(&err)
	out = []*row.Row{}
	out = append(out, s.q("SELECT id, task_id, kind, value, label, actor, created_at FROM artifacts WHERE task_id = ? ORDER BY id", taskID)...)
	return out, nil
}

// OpenNotes lists unresolved typed notes (and plain notes when all is set)
// on a project's tasks, subtasks and the project itself.
func (s *Store) OpenNotes(projectIDs []int64, taskID int64, kinds []string) (out []*row.Row, err error) {
	defer guard(&err)
	var where []string
	var args []any
	if taskID != 0 {
		where = append(where, "((n.target_type = 'task' AND n.target_id = ?) OR (n.target_type = 'subtask' AND n.target_id IN (SELECT id FROM subtasks WHERE task_id = ?)))")
		args = append(args, taskID, taskID)
	} else if len(projectIDs) > 0 {
		m := marks(len(projectIDs))
		where = append(where, "((n.target_type = 'project' AND n.target_id IN ("+m+")) OR (n.target_type = 'task' AND n.target_id IN (SELECT id FROM tasks WHERE project_id IN ("+m+"))) OR (n.target_type = 'subtask' AND n.target_id IN (SELECT s.id FROM subtasks s JOIN tasks t ON t.id = s.task_id WHERE t.project_id IN ("+m+"))) OR (n.target_type = 'spec_anchor' AND n.target_id IN (SELECT a.id FROM spec_anchors a JOIN specs sp ON sp.id = a.spec_id WHERE sp.project_id IN ("+m+"))) OR (n.target_type = 'spec' AND n.target_id IN (SELECT id FROM specs WHERE project_id IN ("+m+"))))")
		for i := 0; i < 5; i++ {
			args = append(args, ids64(projectIDs)...)
		}
	}
	where = append(where, "n.resolved_at IS NULL")
	if len(kinds) > 0 {
		where = append(where, "n.kind IN ("+marks(len(kinds))+")")
		for _, k := range kinds {
			args = append(args, k)
		}
	}
	q := "SELECT n.* FROM notes n WHERE " + joinAnd(where) + " ORDER BY n.created_at, n.id"
	out = []*row.Row{}
	for _, r := range s.q(q, args...) {
		item := s.noteRow(r)
		if !item.Has("kind") {
			item.Set("kind", "note")
		}
		item.Set("created", refs.FmtTS(r.Str("created_at"), false, false))
		out = append(out, item)
	}
	return out, nil
}

func joinAnd(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " AND "
		}
		out += p
	}
	return out
}

// GetArtifact fetches an artifact by id.
func (s *Store) GetArtifact(id int64) (r *row.Row, err error) {
	defer guard(&err)
	r = s.one("SELECT id, task_id, kind, value, label, actor, created_at FROM artifacts WHERE id = ?", id)
	if r == nil {
		fail(errs.NotFound("artifact", fmt.Sprint(id), "no such artifact: %d", id))
	}
	return r, nil
}
