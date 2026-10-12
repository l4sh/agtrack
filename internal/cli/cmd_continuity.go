package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/l4sh/agtrack/internal/cli/argp"
	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/render"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

// Continuity verbs on a task (agent-features B2, B5).
func continuityTaskVerbs() []*argp.Command {
	return []*argp.Command{
		{Name: "checkpoint", Help: "save the task's working state as a new version (body + one-line next); --show / --history read it",
			Run: Handler(taskCheckpoint), Args: []argp.Arg{{Name: "ref", Help: "task id"}},
			Flags: append([]argp.Flag{
				{Long: "--next", Metavar: "TEXT", Help: "the one-line next action"},
				{Long: "--body", Metavar: "TEXT", Help: "markdown body (default: stdin when piped)"},
				{Long: "--file", Metavar: "PATH", Help: "read the body from a file (- = stdin)"},
				{Long: "--show", Kind: argp.Bool, Help: "print the latest checkpoint (or --n N)"},
				{Long: "--n", Kind: argp.PositiveInt, Metavar: "N", Help: "with --show: that version"},
				{Long: "--history", Kind: argp.Bool, Help: "list the versions, newest first"},
			}, listFlags()...)},
		{Name: "verify-context", Help: "check this checkout's branch and worktree against the task's artifacts; exit 4 on a mismatch",
			Run: Handler(taskVerifyContext), Args: []argp.Arg{{Name: "ref", Help: "task id"}}},
	}
}

func init() {
	fieldsFor["checkpoint"] = [2][]string{{"n", "next", "actor", "created_at", "bytes"}, {"id", "session"}}
}

// checkpointStdin returns stdin when it carries input (a pipe or a file),
// so `… | agtrack task checkpoint 12` works and an interactive or /dev/null
// stdin is never waited on. Tests replace it.
var checkpointStdin = func() io.Reader {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	return os.Stdin
}

func taskCheckpoint(c *Ctx) error {
	task, err := c.S.GetTask(c.Str("ref"), true)
	if err != nil {
		return err
	}
	id := task.Int("id")
	writing := c.Has("next") || c.Has("body") || c.Has("file")
	reading := c.Bool("show") || c.Bool("history") || c.Has("n")
	if writing && reading {
		return errs.Usage("--show, --n and --history read checkpoints; they do not combine with --next, --body or --file")
	}
	if c.Bool("history") {
		if c.Bool("show") || c.Has("n") {
			return errs.Usage("--history lists every version; drop --show/--n")
		}
		list, err := c.S.Checkpoints(id)
		if err != nil {
			return err
		}
		return c.emit(list, "checkpoint", len(list))
	}
	if reading {
		cp, err := c.S.Checkpoint(id, int64(c.Int("n")))
		if err != nil {
			return err
		}
		if cp == nil {
			what := "no checkpoint"
			if c.Has("n") {
				what = fmt.Sprintf("no checkpoint v%d", c.Int("n"))
			}
			return errs.NotFound("checkpoint", fmt.Sprint(id), "%s on task #%d; write one with `agtrack task checkpoint %d --next …`", what, id, id)
		}
		return showCheckpoint(c, cp)
	}
	if c.Has("body") && c.Has("file") {
		return errs.Usage("give the body once: --body or --file")
	}
	body := c.Str("body")
	switch {
	case c.Has("file") && c.Str("file") == "-":
		if body, err = readCheckpoint(os.Stdin); err != nil {
			return err
		}
	case c.Has("file"):
		f, err := os.Open(c.Str("file"))
		if err != nil {
			return errs.New("cannot read %s: %v", c.Str("file"), err)
		}
		defer f.Close()
		if body, err = readCheckpoint(f); err != nil {
			return err
		}
	case !c.Has("body"):
		if in := checkpointStdin(); in != nil {
			if body, err = readCheckpoint(in); err != nil {
				return err
			}
		}
	}
	show := func(ids []int64) error {
		cp, err := c.S.CheckpointByID(ids[0])
		if err != nil {
			return err
		}
		switch {
		case c.Quiet:
			c.println(cp.Int("n"))
		case c.structured():
			return c.emitObject(cp)
		default:
			c.printf("checkpoint v%d on #%d (%d bytes)\n", cp.Int("n"), cp.Int("task_id"), len(cp.Str("body")))
		}
		return nil
	}
	if done, err := c.replay(show); done {
		return err
	}
	ids, err := c.record("checkpoint", func() ([]int64, error) {
		cp, err := c.S.AddCheckpoint(task, body, c.Str("next"))
		if err != nil {
			return nil, err
		}
		return []int64{cp.Int("id")}, nil
	})
	if err != nil {
		return err
	}
	return show(ids)
}

// readCheckpoint reads at most one byte past the cap, so an oversized body
// is refused by the store instead of being read whole.
func readCheckpoint(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, store.CheckpointMaxBytes+1))
	if err != nil {
		return "", errs.New("cannot read the checkpoint body: %v", err)
	}
	return string(b), nil
}

func showCheckpoint(c *Ctx, cp *row.Row) error {
	if c.structured() {
		if len(c.Fields) > 0 {
			fields, err := render.SelectFields([]*row.Row{cp}, c.Fields)
			if err != nil {
				return err
			}
			return c.emitObject(render.ProjectRows([]*row.Row{cp}, fields)[0])
		}
		return c.emitObject(cp)
	}
	head := fmt.Sprintf("checkpoint v%d on #%d, %s ago by %s", cp.Int("n"), cp.Int("task_id"), ageOf(cp.Str("created_at")), render0(cp.Get("actor")))
	if s := cp.Str("session"); s != "" {
		head += " (session " + shortSession(s) + ")"
	}
	c.println(head)
	if n := cp.Str("next"); n != "" {
		c.println("next: " + n)
	}
	if b := cp.Str("body"); b != "" {
		c.println()
		c.println(b)
	}
	return nil
}

// --- stale (A2 seam) -----------------------------------------------------------------

// resumeStale is the `stale` key of resume: the A2 staleness of the current
// task (reason, age, last event, open steps) when it is stale, else nil.
func resumeStale(c *Ctx, task *row.Row) (any, error) {
	if task == nil {
		return nil, nil
	}
	st, err := c.S.TaskStaleness(task.Int("id"), 0)
	if err != nil || st == nil || st.Str("stale_reason") == "" {
		return nil, err
	}
	return st, nil
}

// --- git context (B5) ----------------------------------------------------------------

// cwdGit reports the cwd's branch and worktree root ("" outside git).
// Tests replace it.
var cwdGit = func() (branch, top string) {
	run := func(args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "git", args...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	return run("rev-parse", "--abbrev-ref", "HEAD"), run("rev-parse", "--show-toplevel")
}

func cleanPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Clean(p)
}

// contextCheck compares the cwd with a task's branch and worktree
// artifacts. checked is false when the task has neither.
func contextCheck(task *row.Row, arts []*row.Row) (res *row.Row, ok, checked bool) {
	var branches, worktrees []string
	for _, a := range arts {
		switch a.Str("kind") {
		case "branch":
			branches = append(branches, strings.TrimPrefix(a.Str("value"), "refs/heads/"))
		case "worktree":
			worktrees = append(worktrees, a.Str("value"))
		}
	}
	id := task.Int("id")
	if len(branches) == 0 && len(worktrees) == 0 {
		return row.Of("task_id", id, "ok", true, "checked", false,
			"message", fmt.Sprintf("task %d has no branch or worktree artifact; nothing to check", id)), true, false
	}
	branch, top := cwdGit()
	res = row.Of("task_id", id, "ok", true, "checked", true, "branches", nonNilS(branches), "worktrees", nonNilS(worktrees),
		"cwd_branch", nilIfEmpty(branch), "cwd_worktree", nilIfEmpty(top))
	var problems []string
	if len(branches) > 0 {
		match := false
		for _, b := range branches {
			if b == branch {
				match = true
			}
		}
		switch {
		case branch == "":
			problems = append(problems, fmt.Sprintf("task %d is on %s; this directory is not a git checkout", id, strings.Join(branches, ", ")))
		case !match:
			problems = append(problems, fmt.Sprintf("task %d is on %s; this checkout is on %s", id, strings.Join(branches, ", "), branch))
		}
	}
	if len(worktrees) > 0 && (branch != "" || len(branches) == 0) {
		match := false
		for _, w := range worktrees {
			if top != "" && cleanPath(w) == cleanPath(top) {
				match = true
			}
		}
		if !match {
			where := top
			if where == "" {
				where = "not a git checkout"
			}
			problems = append(problems, fmt.Sprintf("task %d is in worktree %s; this checkout is %s", id, strings.Join(worktrees, ", "), where))
		}
	}
	if len(problems) == 0 {
		where := branch
		if where == "" {
			where = top
		}
		res.Set("message", fmt.Sprintf("ok: task %d matches this checkout (%s)", id, where))
		return res, true, true
	}
	res.Set("ok", false)
	res.Set("message", "MISMATCH: "+strings.Join(problems, "; "))
	return res, false, true
}

// contextMismatch is resume's context_mismatch: the check's result on a
// mismatch, else nil. Outside a git checkout resume does not complain.
func contextMismatch(task *row.Row, arts []*row.Row) any {
	res, ok, checked := contextCheck(task, arts)
	if !checked || ok || (res.Get("cwd_branch") == nil && res.Get("cwd_worktree") == nil) {
		return nil
	}
	return res
}

func taskVerifyContext(c *Ctx) error {
	task, err := c.S.GetTask(c.Str("ref"), true)
	if err != nil {
		return err
	}
	arts, err := c.S.Artifacts(task.Int("id"))
	if err != nil {
		return err
	}
	res, ok, _ := contextCheck(task, arts)
	if !ok {
		e := errs.Conflict("%s", res.Str("message"))
		e.Token = "context_mismatch"
		e.Extra = map[string]any{"task_id": task.Int("id"), "branches": res.Get("branches"), "worktrees": res.Get("worktrees"),
			"cwd_branch": res.Get("cwd_branch"), "cwd_worktree": res.Get("cwd_worktree")}
		return e
	}
	if c.structured() {
		return c.emitObject(res)
	}
	if !c.Quiet {
		c.println(res.Str("message"))
	}
	return nil
}
