package cli

import (
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func stubGit(t *testing.T, g gitInfo) {
	t.Helper()
	old := gitProbe
	gitProbe = func() gitInfo { return g }
	t.Cleanup(func() { gitProbe = old })
}

func artifactsOf(e *env, id string) string {
	return e.ok("task", "artifact", id, "ls")
}

func TestGitArtifactsOnStartAndClaim(t *testing.T) {
	e := closureEnv(t)
	stubGit(t, gitInfo{Branch: "feat/x", Top: "/w/one", Head: "abc1234"})
	e.ok("task", "add", "a")
	e.ok("task", "add", "b")
	e.ok("task", "add", "c")
	e.ok("task", "start", "1")
	out := artifactsOf(e, "1")
	if !strings.Contains(out, "feat/x") || !strings.Contains(out, "/w/one") {
		t.Fatalf("start: %q", out)
	}
	e.ok("task", "start", "1") // idempotent
	if n := strings.Count(artifactsOf(e, "1"), "feat/x"); n != 1 {
		t.Fatalf("duplicate branch artifact: %d", n)
	}
	e.ok("task", "claim", "2")
	if !strings.Contains(artifactsOf(e, "2"), "feat/x") {
		t.Fatal("claim recorded nothing")
	}
	e.ok("task", "claim", "3", "--no-git")
	if strings.Contains(artifactsOf(e, "3"), "feat/x") {
		t.Fatal("--no-git ignored")
	}
	// a step pulls its task into progress
	e.ok("task", "add", "d", "--step", "s")
	e.ok("sub", "start", "4.1")
	if !strings.Contains(artifactsOf(e, "4"), "feat/x") {
		t.Fatal("sub start recorded nothing")
	}
	e.ok("config", "set", "git.auto", "false")
	e.ok("task", "add", "e")
	e.ok("task", "start", "5")
	if strings.Contains(artifactsOf(e, "5"), "feat/x") {
		t.Fatal("git.auto=false ignored")
	}
}

func TestGitNextClaim(t *testing.T) {
	e := closureEnv(t)
	stubGit(t, gitInfo{Branch: "feat/y", Top: "/w/two"})
	e.ok("task", "add", "a")
	e.ok("next", "--claim")
	if !strings.Contains(artifactsOf(e, "1"), "feat/y") {
		t.Fatal("next --claim recorded nothing")
	}
}

func TestGitBaseBranchWarning(t *testing.T) {
	e := closureEnv(t)
	stubGit(t, gitInfo{Branch: "develop", Top: "/w/b", Base: true})
	e.ok("task", "add", "a")
	_, errs, code := e.run("task", "start", "1")
	if code != 0 || !strings.Contains(errs, "task 1 started on base branch develop") {
		t.Fatalf("code %d stderr %q", code, errs)
	}
	if !strings.Contains(artifactsOf(e, "1"), "develop") {
		t.Fatal("base branch not recorded")
	}
}

func TestGitDoneRecordsCommitEvidence(t *testing.T) {
	e := closureEnv(t)
	stubGit(t, gitInfo{Branch: "feat/x", Top: "/w/one", Head: "deadbeef"})
	e.ok("task", "add", "a")
	e.ok("task", "add", "b")
	e.ok("task", "done", "1")
	if !strings.Contains(artifactsOf(e, "1"), "deadbeef") {
		t.Fatal("no commit artifact")
	}
	if !strings.Contains(e.ok("task", "show", "1"), "commit:deadbeef") {
		t.Fatal("no commit evidence")
	}
	e.ok("task", "done", "2", "--no-git")
	if strings.Contains(artifactsOf(e, "2"), "deadbeef") {
		t.Fatal("--no-git ignored on done")
	}
}

func TestMissingArtifactFilter(t *testing.T) {
	e := closureEnv(t)
	e.ok("task", "add", "a")
	e.ok("task", "add", "b")
	e.ok("task", "artifact", "1", "branch", "feat/x")
	e.ok("task", "add", "c", "--pr", "https://github.com/o/r/pull/9")
	if got := strings.TrimSpace(e.ok("task", "ls", "--missing-artifact", "branch", "--ids")); got != "2\n3" {
		t.Fatalf("branch: %q", got)
	}
	if got := strings.TrimSpace(e.ok("task", "ls", "--missing-artifact", "pr", "--ids")); got != "1\n2" {
		t.Fatalf("pr: %q", got)
	}
	e.fails(2, "task", "ls", "--missing-artifact", "bogus")
}

func TestPRSync(t *testing.T) {
	e := closureEnv(t)
	old := ghPRView
	t.Cleanup(func() { ghPRView = old })
	ghPRView = func(url string) (string, string, error) {
		switch {
		case strings.HasSuffix(url, "/1"):
			return "MERGED", "2026-10-01T00:00:00Z", nil
		case strings.HasSuffix(url, "/2"):
			return "OPEN", "", nil
		}
		return "", "", errors.New("gh is not authenticated")
	}
	e.ok("task", "add", "a", "--pr", "https://github.com/o/r/pull/1")
	e.ok("task", "add", "b", "--pr", "https://github.com/o/r/pull/2")
	e.ok("task", "add", "c", "--pr", "https://github.com/o/r/pull/3")
	var res struct {
		PRs []struct {
			TaskID  float64 `json:"task_id"`
			State   string  `json:"state"`
			Changed bool    `json:"changed"`
		} `json:"prs"`
		Skipped []struct{ Reason string }  `json:"skipped"`
		Merged  []struct{ Command string } `json:"merged_open_tasks"`
	}
	out := e.ok("pr", "sync", "-f", "json")
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err, out)
	}
	if len(res.PRs) != 2 || res.PRs[0].State != "merged" || !res.PRs[0].Changed || res.PRs[1].Changed {
		t.Fatalf("prs: %s", out)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Reason != "gh is not authenticated" {
		t.Fatalf("skipped: %s", out)
	}
	if len(res.Merged) != 1 || !strings.HasPrefix(res.Merged[0].Command, "agtrack task done 1") {
		t.Fatalf("merged: %s", out)
	}
	if !strings.Contains(e.ok("pr", "ls"), "merged") {
		t.Fatal("state not updated")
	}
	// once the task is done it is no longer listed
	e.ok("task", "done", "1")
	out = e.ok("pr", "sync", "-f", "json")
	if strings.Contains(out, "agtrack task done") {
		t.Fatalf("closed task still listed: %s", out)
	}
}

func TestHooksPrint(t *testing.T) {
	e := newEnv(t)
	for _, h := range []string{"claude", "codex"} {
		for _, ev := range []string{"session-start", "session-end", "pre-commit"} {
			out := e.ok("hooks", "print", h, "--event", ev)
			if !strings.Contains(out, "nothing is installed") {
				t.Fatalf("%s %s: %q", h, ev, out)
			}
		}
		var v struct {
			Events []struct {
				Parts []struct{ Kind, Path, Text string }
			}
		}
		if err := json.Unmarshal([]byte(e.ok("hooks", "print", h, "-f", "json")), &v); err != nil || len(v.Events) != 3 {
			t.Fatalf("%s json: %v", h, err)
		}
		for _, ev := range v.Events {
			for _, p := range ev.Parts {
				switch p.Kind {
				case "config":
					var j map[string]any
					if err := json.Unmarshal([]byte(p.Text), &j); err != nil {
						t.Fatalf("%s config is not json: %v\n%s", h, err, p.Text)
					}
				case "script":
					cmd := exec.Command("sh", "-n")
					cmd.Stdin = strings.NewReader(p.Text)
					if b, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("%s script: %v %s", h, err, b)
					}
				}
			}
		}
	}
	e.fails(2, "hooks", "print", "gemini")
	if !strings.Contains(e.ok("hooks", "print", "claude", "--event", "pre-commit"), "agtrack check") {
		t.Fatal("pre-commit does not call agtrack check")
	}
}

func TestRealGitProbe(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git unavailable: %v %s", err, b)
		}
	}
	run("init", "-q", "-b", "main")
	run("commit", "-q", "--allow-empty", "-m", "x")
	t.Chdir(dir)
	g := realGitProbe()
	if g.Branch != "main" || !g.Base || len(g.Head) < 7 || g.Top == "" {
		t.Fatalf("got %+v", g)
	}
	run("checkout", "-q", "-b", "feat/z")
	if g = realGitProbe(); g.Branch != "feat/z" || g.Base {
		t.Fatalf("got %+v", g)
	}
}
