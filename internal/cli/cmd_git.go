package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

// Git adoption (agent-features spec F1, F2).

// gitInfo is the cwd checkout as F1 sees it. Zero value = not a checkout.
type gitInfo struct {
	Branch string // "" when detached
	Top    string // worktree root
	Head   string // HEAD commit, "" in a repo with no commits
	Base   bool   // Branch is a base branch
}

// gitProbe reads the cwd checkout. Tests replace it. It never fails: any git
// trouble yields the zero value.
var gitProbe = realGitProbe

func realGitProbe() gitInfo {
	top, ok := store.GitQuick("", "rev-parse", "--show-toplevel")
	if !ok || top == "" {
		return gitInfo{}
	}
	g := gitInfo{Top: top}
	if b, ok := store.GitQuick("", "rev-parse", "--abbrev-ref", "HEAD"); ok && b != "HEAD" {
		g.Branch = b
		g.Base = store.IsBaseBranch(top, b)
	}
	g.Head, _ = store.GitQuick("", "rev-parse", "HEAD")
	return g
}

// gitAuto reports whether this call may touch git artifacts: not --no-git,
// and config git.auto is not false.
func (c *Ctx) gitAuto() bool {
	if c.Bool("no_git") {
		return false
	}
	if v, ok, err := c.S.GetMeta("config:git.auto"); err == nil && ok && strings.EqualFold(v, "false") {
		return false
	}
	return true
}

// gitStart records branch and worktree artifacts on the task when it has none
// for this checkout, and warns on a base branch. It never fails the command.
func (c *Ctx) gitStart(task *row.Row) {
	if !c.gitAuto() {
		return
	}
	g := gitProbe()
	if g.Top == "" {
		return
	}
	arts, err := c.S.Artifacts(task.Int("id"))
	if err != nil {
		c.warnf("git artifacts skipped: %v", err)
		return
	}
	has := func(kind, val string) bool {
		for _, a := range arts {
			if a.Str("kind") == kind && a.Str("value") == val {
				return true
			}
		}
		return false
	}
	add := func(kind, val string) {
		if val == "" || has(kind, val) {
			return
		}
		if _, err := c.S.AddArtifact(task, kind, val, ""); err != nil {
			c.warnf("could not record %s artifact: %v", kind, err)
		}
	}
	add("branch", g.Branch)
	add("worktree", g.Top)
	if g.Base {
		c.warnf("task %d started on base branch %s", task.Int("id"), g.Branch)
	}
}

// gitDone runs inside the done transaction: it records HEAD as a commit
// artifact and returns evs plus the commit as evidence (unless the caller
// already passed that commit).
func (c *Ctx) gitDone(task *row.Row, evs []store.Evidence) []store.Evidence {
	if !c.gitAuto() {
		return evs
	}
	g := gitProbe()
	if g.Head == "" {
		return evs
	}
	if _, err := c.S.AddArtifact(task, "commit", g.Head, ""); err != nil {
		c.warnf("could not record commit artifact: %v", err)
	}
	for _, e := range evs {
		if e.Kind == "commit" && e.Value == g.Head {
			return evs
		}
	}
	return append(append([]store.Evidence{}, evs...), store.Evidence{Kind: "commit", Value: g.Head})
}

// --- pr sync (F2) --------------------------------------------------------------------

// ghPRView reads a PR's state with the gh CLI (read only). Tests replace it.
// A returned error's text is the skip reason.
var ghPRView = func(url string) (state, mergedAt string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", url, "--json", "state,mergedAt", "--jq", `.state + " " + (.mergedAt // "")`)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var nf *exec.Error
		msg := strings.TrimSpace(stderr.String())
		switch {
		case errors.As(err, &nf):
			return "", "", errors.New("gh is not installed")
		case ctx.Err() != nil:
			return "", "", errors.New("gh timed out")
		case strings.Contains(msg, "gh auth login") || strings.Contains(msg, "not logged in") || strings.Contains(msg, "authentication"):
			return "", "", errors.New("gh is not authenticated")
		case strings.Contains(msg, "Could not resolve") || strings.Contains(msg, "not found") || strings.Contains(msg, "404"):
			return "", "", errors.New("PR not found")
		case msg == "":
			return "", "", err
		}
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i]
		}
		return "", "", errors.New(msg)
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return "", "", errors.New("gh returned no state")
	}
	if len(f) > 1 {
		mergedAt = f[1]
	}
	return f[0], mergedAt, nil
}

func prSync(c *Ctx) error {
	f := store.PRFilter{Context: c.Context, AllContexts: c.Bool("all_contexts")}
	if c.Has("project") {
		p, err := c.resolveProject(c.Str("project"), true)
		if err != nil {
			return err
		}
		f.Project = p
	}
	prs, err := c.S.FindPRs(f)
	if err != nil {
		return err
	}
	results := []*row.Row{}
	skipped := []*row.Row{}
	var ready []*row.Row
	seenTask := map[int64]bool{}
	for _, pr := range prs {
		old := pr.Str("state")
		state, _, gerr := ghPRView(pr.Str("url"))
		if gerr != nil {
			skipped = append(skipped, row.Of("id", pr.Int("id"), "task_id", pr.Int("task_id"), "url", pr.Str("url"), "reason", gerr.Error()))
			continue
		}
		cur := strings.ToLower(state)
		if cur == "open" && old == "draft" {
			cur = "draft" // gh state has no draft; keep what we know
		}
		if norm, nerr := refs.NormalizePRState(cur); nerr == nil {
			cur = norm
		} else {
			skipped = append(skipped, row.Of("id", pr.Int("id"), "task_id", pr.Int("task_id"), "url", pr.Str("url"), "reason", "unknown state "+state))
			continue
		}
		changed := cur != old
		if changed {
			if _, err := c.S.SetPRState(pr.Int("id"), cur); err != nil {
				return err
			}
		}
		results = append(results, row.Of("id", pr.Int("id"), "task_id", pr.Int("task_id"), "url", pr.Str("url"), "from", old, "state", cur, "changed", changed))
		st := pr.Str("task_status")
		if cur == "merged" && !store.Closed(st) && !seenTask[pr.Int("task_id")] {
			seenTask[pr.Int("task_id")] = true
			ready = append(ready, row.Of("task_id", pr.Int("task_id"), "title", pr.Str("task_title"), "pr", pr.Str("url"),
				"command", fmt.Sprintf("agtrack task done %d --evidence pr:%s", pr.Int("task_id"), pr.Str("url"))))
		}
	}
	if c.Quiet {
		return nil
	}
	if c.structured() {
		return c.emitObject(row.Of("prs", results, "skipped", skipped, "merged_open_tasks", nonNil(ready)))
	}
	for _, r := range results {
		mark := " "
		if r.Get("changed") == true {
			mark = "*"
		}
		c.printf("%s pr %d (task %d) %s -> %s\n", mark, r.Int("id"), r.Int("task_id"), r.Str("from"), r.Str("state"))
	}
	for _, r := range skipped {
		c.printf("  pr %d (task %d) skipped: %s\n", r.Int("id"), r.Int("task_id"), r.Str("reason"))
	}
	if len(results)+len(skipped) == 0 {
		c.println("no linked PRs")
	}
	for _, r := range ready {
		c.printf("merged, task still open: #%d %s\n    %s\n", r.Int("task_id"), r.Str("title"), r.Str("command"))
	}
	return nil
}
