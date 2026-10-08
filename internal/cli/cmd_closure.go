package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

// Closure and hygiene verbs (agent-features spec A1-A5, E1).

// evidenceArgs parses every --evidence KIND:VALUE.
func (c *Ctx) evidenceArgs() ([]store.Evidence, error) {
	var out []store.Evidence
	for _, v := range c.Strs("evidence") {
		ev, err := store.ParseEvidence(v)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

// closeInfo is what closing a step did to its task.
type closeInfo struct {
	taskID     int64
	remaining  int  // open steps left
	allClosed  bool // that was the last open step
	taskClosed bool // --close-parent closed the task
	status     string
}

// stepClosed runs inside the invocation's transaction after a step moved to a
// closed status: it stores evidence, counts what is left and, with
// --close-parent, closes the task when nothing is.
func (c *Ctx) stepClosed(sub *row.Row, at string, evs []store.Evidence) (closeInfo, error) {
	info := closeInfo{taskID: sub.Int("task_id")}
	if sub.Str("status") == "done" && len(evs) > 0 {
		if err := c.S.AddEvidence(info.taskID, sub.Int("id"), evs); err != nil {
			return info, err
		}
	}
	n, err := c.S.OpenStepCount(info.taskID)
	if err != nil {
		return info, err
	}
	info.remaining, info.allClosed = n, n == 0
	return c.closeParent(info, at, evs)
}

// closeParent closes the task when --close-parent is set and no step is open.
func (c *Ctx) closeParent(info closeInfo, at string, evs []store.Evidence) (closeInfo, error) {
	task, err := c.S.GetTaskID(info.taskID)
	if err != nil {
		return info, err
	}
	info.status = task.Str("status")
	if !info.allClosed || !c.Bool("close_parent") || store.Closed(info.status) {
		return info, nil
	}
	t, changed, err := c.S.SetTaskStatus(info.taskID, "done", at)
	if err != nil {
		return info, err
	}
	if changed {
		if c.Has("note") {
			if _, err := c.S.AddNote(store.NewNote{TargetType: "task", TargetID: info.taskID, Body: c.Str("note"),
				TaskID: info.taskID, ProjectID: t.Int("project_id"), At: at}); err != nil {
				return info, err
			}
		}
		if err := c.S.AddEvidence(info.taskID, 0, c.gitDone(t, evs)); err != nil {
			return info, err
		}
		info.taskClosed, info.status = true, "done"
	}
	return info, nil
}

// describeClose adds the close-parent facts to a structured row and returns
// the text line for the human view ("" = none). It also sets the hint.
func (c *Ctx) describeClose(info closeInfo, obj *row.Row) string {
	line := ""
	switch {
	case info.taskClosed:
		line = fmt.Sprintf("all steps closed; task %d done", info.taskID)
	case info.allClosed && !store.Closed(info.status):
		line = fmt.Sprintf("all steps closed; task %d still %s", info.taskID, info.status)
		c.Hint(fmt.Sprintf("agtrack task done %d", info.taskID))
	case c.Bool("close_parent") && info.remaining > 0:
		line = fmt.Sprintf("%d steps still open on task %d", info.remaining, info.taskID)
	}
	if obj != nil {
		if c.Bool("close_parent") {
			obj.Set("task_closed", info.taskClosed)
		}
		if c.Bool("close_parent") || info.allClosed {
			obj.Set("open_steps", int64(info.remaining))
		}
		if info.allClosed && !info.taskClosed {
			obj.Set("all_steps_closed", true)
		}
	}
	return line
}

// taskDoneHint suggests the follow-up after a task closed (E3, A4).
func (c *Ctx) taskDoneHint(taskID int64) {
	branchNoPR, err := c.S.HasBranchNoPR(taskID)
	if err != nil || !branchNoPR {
		return
	}
	c.Hint(fmt.Sprintf("agtrack pr add %d <url>", taskID))
}

// --- sub next (E1) ------------------------------------------------------------------

func subtaskNext(c *Ctx) error {
	task, err := c.S.GetTask(c.Str("task"), true)
	if err != nil {
		return err
	}
	evs, err := c.evidenceArgs()
	if err != nil {
		return err
	}
	subs, _, err := c.S.ListSubtasks(task.Int("id"), nil, 0)
	if err != nil {
		return err
	}
	var cur, next *row.Row
	for _, s := range subs {
		switch s.Str("status") {
		case "in-progress":
			if cur == nil {
				cur = s
			}
		case "todo":
			if next == nil {
				next = s
			}
		}
	}
	if cur == nil && next == nil && !(c.Bool("close_parent") && !store.Closed(task.Str("status"))) {
		return errs.Conflict("task #%d has no open steps to advance", task.Int("id"))
	}
	var closedSub, started *row.Row
	var info closeInfo
	err = c.S.Tx(func() error {
		if cur != nil {
			res, err := c.S.SetSubtaskStatus(cur.Int("id"), "done", true, "")
			if err != nil {
				return err
			}
			closedSub = res.Subtask
			if c.Has("note") {
				if _, err := c.S.AddNote(store.NewNote{TargetType: "subtask", TargetID: cur.Int("id"), Body: c.Str("note"),
					TaskID: task.Int("id"), ProjectID: task.Int("project_id")}); err != nil {
					return err
				}
			}
			if len(evs) > 0 {
				if err := c.S.AddEvidence(task.Int("id"), cur.Int("id"), evs); err != nil {
					return err
				}
			}
		}
		if next != nil {
			res, err := c.S.SetSubtaskStatus(next.Int("id"), "in-progress", true, "")
			if err != nil {
				return err
			}
			started = res.Subtask
		}
		n, err := c.S.OpenStepCount(task.Int("id"))
		if err != nil {
			return err
		}
		info = closeInfo{taskID: task.Int("id"), remaining: n, allClosed: n == 0}
		info, err = c.closeParent(info, "", evs)
		return err
	})
	if err != nil {
		return err
	}
	label := func(s *row.Row) string { return fmt.Sprintf("%d.%d", s.Int("task_id"), s.Int("position")) }
	obj := row.New()
	if closedSub != nil {
		d, _ := c.S.SubtaskDetail(closedSub)
		obj.Set("closed", c.lean(d, "subtask"))
	} else {
		obj.Set("closed", nil)
	}
	if started != nil {
		d, _ := c.S.SubtaskDetail(started)
		obj.Set("started", c.lean(d, "subtask"))
	} else {
		obj.Set("started", nil)
	}
	obj.Set("task", task.Int("id"))
	line := c.describeClose(info, obj)
	if !c.Bool("close_parent") {
		obj.Set("open_steps", int64(info.remaining))
	}
	if c.Quiet {
		return nil
	}
	if c.structured() {
		return c.emitObject(obj)
	}
	if closedSub != nil {
		c.printf("%s %s %s\n", label(closedSub), refs.StatusMark["done"], closedSub.Str("title"))
	}
	if started != nil {
		c.printf("%s %s %s\n", label(started), refs.StatusMark["in-progress"], started.Str("title"))
	}
	if line != "" {
		c.println(line)
	}
	return nil
}

// --- snooze (A5) ---------------------------------------------------------------------

func taskSnooze(c *Ctx) error {
	task, err := c.S.GetTask(c.Str("ref"), true)
	if err != nil {
		return err
	}
	until, err := refs.ParseUntil(c.Str("until"))
	if err != nil {
		return err
	}
	t, err := c.S.SnoozeTask(task.Int("id"), until, c.Str("note"))
	if err != nil {
		return err
	}
	obj, _ := c.S.TaskDict(t)
	return c.mutated(fmt.Sprintf("#%d parked until %s", t.Int("id"), refs.FmtTS(until, false, false)), c.lean(obj, "task"))
}

func taskUnsnooze(c *Ctx) error {
	task, err := c.S.GetTask(c.Str("ref"), true)
	if err != nil {
		return err
	}
	t, changed, err := c.S.UnsnoozeTask(task.Int("id"))
	if err != nil {
		return err
	}
	text := fmt.Sprintf("#%d unparked", t.Int("id"))
	if !changed {
		text = fmt.Sprintf("#%d was not parked", t.Int("id"))
	}
	obj, _ := c.S.TaskDict(t)
	obj.Set("changed", changed)
	return c.mutated(text, c.lean(obj, "task"))
}

// parkedNote is the text-view marker for a parked task, or "".
func parkedNote(t *row.Row) string {
	until := t.Str("snoozed_until")
	if until == "" || until <= refs.NowISO() {
		return ""
	}
	s := "parked until " + refs.FmtTS(until, false, false)
	if n := t.Str("snooze_note"); n != "" {
		s += ": " + n
	}
	return s
}

// --- staleness (A2) and done-check (A3) --------------------------------------------------

// staleRows decorates tasks with age, last_event, open_steps and stale_reason.
func (c *Ctx) staleRows(tasks []*row.Row, idleDays int) ([]*row.Row, error) {
	for _, t := range tasks {
		st, err := c.S.Staleness(t, idleDays)
		if err != nil {
			return nil, err
		}
		for _, k := range st.Keys() {
			t.Set(k, st.Get(k))
		}
	}
	return tasks, nil
}

// staleDays resolves --stale-after (Nd) or the configured default.
func (c *Ctx) staleDays() (int, error) {
	if !c.Has("stale_after") {
		return c.S.StaleDays(), nil
	}
	d, err := parseDuration(c.Str("stale_after"))
	if err != nil || d <= 0 {
		return 0, errs.New("invalid --stale-after %q: use a number of days like 7d", c.Str("stale_after"))
	}
	days := int(d.Hours() / 24)
	if days < 1 {
		days = 1
	}
	return days, nil
}

func taskDoneCheck(c *Ctx) error {
	var project *row.Row
	var err error
	if c.Has("project") {
		if project, err = c.resolveProject(c.Str("project"), true); err != nil {
			return err
		}
	}
	days, err := c.staleDays()
	if err != nil {
		return err
	}
	tasks, _, err := c.S.ListTasks(store.TaskFilter{Context: c.Context, Project: project, Statuses: []string{"in-progress"},
		AllContexts: c.Bool("all_contexts"), Order: "id", ExcludeSnoozed: true})
	if err != nil {
		return err
	}
	if tasks, err = c.staleRows(tasks, days); err != nil {
		return err
	}
	cands := []*row.Row{}
	for _, t := range tasks {
		reason := t.Str("stale_reason")
		if reason == "" {
			continue
		}
		id := t.Int("id")
		cmd := fmt.Sprintf("agtrack task snooze %d --until 7d", id)
		if reason == "all-steps-done" {
			cmd = fmt.Sprintf("agtrack task done %d", id)
		}
		cands = append(cands, row.Of("id", id, "stale_reason", reason, "open_steps", t.Int("open_steps"),
			"last_event", t.Str("last_event"), "title", t.Str("title"), "command", cmd,
			"snooze", fmt.Sprintf("agtrack task snooze %d --until 7d", id)))
	}
	if !c.Bool("apply") {
		return c.emit(cands, "donecheck", len(cands))
	}
	// --apply closes the all-steps-done candidates, after a human says yes.
	var targets []*row.Row
	for _, cand := range cands {
		if cand.Str("stale_reason") == "all-steps-done" {
			targets = append(targets, cand)
		}
	}
	if len(targets) > 0 && !c.Bool("yes") {
		if !stdinIsTerminal() {
			return errs.New("--apply closes tasks and needs a human: run it from a terminal, or pass --yes")
		}
		fmt.Fprintf(c.Err, "close %d task(s) whose steps are all done? [y/N] ", len(targets))
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			return errs.New("not confirmed; nothing closed")
		}
	}
	closed := []*row.Row{}
	err = c.S.Tx(func() error {
		for _, t := range targets {
			_, changed, err := c.S.SetTaskStatus(t.Int("id"), "done", "")
			if err != nil {
				return err
			}
			if changed {
				closed = append(closed, row.Of("id", t.Int("id"), "title", t.Str("title")))
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if c.Quiet {
		return nil
	}
	if c.structured() {
		return c.emitObject(row.Of("closed", closed))
	}
	for _, t := range closed {
		c.printf("#%d %s %s\n", t.Int("id"), refs.StatusMark["done"], t.Str("title"))
	}
	if len(closed) == 0 {
		c.println("nothing to close")
	}
	return nil
}

func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
