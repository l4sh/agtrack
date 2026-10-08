package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

// Multi-agent coordination verbs (docs/agent-features-spec.md section C).

// ExitStale ends `watch --until stale`: a watched claim went quiet.
const ExitStale = 14

// noteAck acknowledges handoff, blocker and question notes (C1).
func noteAck(c *Ctx) error {
	var ids []int64
	for _, s := range c.Strs("id") {
		id, err := parseID(strings.TrimPrefix(s, "n"))
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	res, err := c.S.AckNotes(ids, c.Str("note"))
	if err != nil {
		return err
	}
	var lines []string
	for _, r := range res {
		if r.Get("changed") == true {
			lines = append(lines, fmt.Sprintf("note %d acknowledged", r.Int("id")))
		} else {
			lines = append(lines, fmt.Sprintf("note %d was already acknowledged by %s", r.Int("id"), r.Str("acknowledged_by")))
		}
	}
	return c.mutated(strings.Join(lines, "\n"), row.Of("acknowledged", res))
}

// unackedOnly keeps the notes that still await an acknowledgement.
func unackedOnly(rows []*row.Row) []*row.Row {
	out := []*row.Row{}
	for _, r := range rows {
		if store.Ackable[r.Str("kind")] && !r.Has("acknowledged_at") {
			out = append(out, r)
		}
	}
	return out
}

// taskTransfer hands a task to another actor (C4).
func taskTransfer(c *Ctx) error {
	task, err := c.S.GetTask(c.Str("ref"), true)
	if err != nil {
		return err
	}
	if !c.Has("to") {
		return errs.New("task transfer needs --to ACTOR")
	}
	ttl, err := parseDuration(c.Str("ttl"))
	if err != nil {
		return err
	}
	t, n, err := c.S.TransferTask(task.Int("id"), c.Str("to"), c.Str("note"), ttl, c.Bool("force"))
	if err != nil {
		return err
	}
	obj, _ := c.S.TaskDict(t)
	obj.Set("note_id", n.Int("id"))
	return c.mutated(fmt.Sprintf("#%d transferred to %s (handoff note %d)", t.Int("id"), c.Str("to"), n.Int("id")), c.lean(obj, "task"))
}

// claimScope resolves the -p/-t flags of claims and watch.
func claimScope(c *Ctx) (store.ClaimsFilter, error) {
	var f store.ClaimsFilter
	switch {
	case c.Has("task"):
		t, err := c.S.GetTask(c.Str("task"), true)
		if err != nil {
			return f, err
		}
		f.TaskID = t.Int("id")
	case c.Has("project"):
		p, err := c.resolveProject(c.Str("project"), true)
		if err != nil {
			return f, err
		}
		if f.ProjectIDs, err = c.S.DescendantIDs(p.Int("id")); err != nil {
			return f, err
		}
	}
	return f, nil
}

// claimsLs lists live claims (C3).
func claimsLs(c *Ctx) error {
	f, err := claimScope(c)
	if err != nil {
		return err
	}
	if c.Has("stale") {
		d, err := parseDuration(c.Str("stale"))
		if err != nil {
			return err
		}
		f.StaleSince = refs.Now().Add(-d)
	}
	rows, err := c.S.Claims(f)
	if err != nil {
		return err
	}
	return c.emit(rows, "claim", len(rows))
}

// staleTick is the watch onTick for `--until stale`: it ends the watch with
// a `stale` terminal line (exit 14) naming the first quiet claim.
func staleTick(c *Ctx, f store.ClaimsFilter, quiet time.Duration) func() (string, int, *row.Row) {
	return func() (string, int, *row.Row) {
		f.StaleSince = refs.Now().Add(-quiet)
		rows, err := c.S.Claims(f)
		if err != nil || len(rows) == 0 {
			return "", 0, nil
		}
		r := rows[0]
		reason := fmt.Sprintf("#%d held by %s quiet for %s", r.Int("id"), r.Str("holder"), r.Str("quiet"))
		return "stale", ExitStale, row.Of("reason", reason, "task_id", r.Int("id"), "holder", r.Get("holder"),
			"session", r.Get("session"), "last_seen", r.Get("last_seen"))
	}
}
