package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// project set --move re-homes a project with its subprojects; tasks, specs
// and scoped rules follow by id, and a pinned actor stays pinned to it.
func TestProjectMove(t *testing.T) {
	e := newEnv(t)
	e.actor = "bot"
	e.ok("ctx", "add", "work")
	e.ok("ctx", "add", "home")
	e.ok("project", "add", "work/app")
	e.ok("project", "add", "work/app:api")
	e.ok("project", "add", "home/infra")
	e.ok("task", "add", "top", "-p", "work/app")
	e.ok("task", "add", "sub", "-p", "work/app:api")
	e.ok("rule", "add", "deny", "pr", "-p", "work/app", "--quote", "no PRs")
	e.ok("project", "use", "work/app:api")

	e.ok("project", "set", "work/app", "--move", "home")
	if got := e.ok("task", "show", "2"); !strings.Contains(got, "project: home/app:api") {
		t.Errorf("subproject task not moved:\n%s", got)
	}
	if got := e.ok("task", "show", "1"); !strings.Contains(got, "project: home/app") {
		t.Errorf("task not moved:\n%s", got)
	}
	if out, _, code := e.run("check", "pr", "-p", "home/app"); code != 20 {
		t.Errorf("rule did not follow the project: exit %d\n%s", code, out)
	}
	if got := e.ok("info"); !strings.Contains(got, "context: home") || !strings.Contains(got, "project: app:api") {
		t.Errorf("pin lost after move:\n%s", got)
	}

	// A subproject becomes top-level, then nests under another project, renamed.
	e.ok("project", "set", "home/app:api", "--move", "work")
	e.ok("project", "set", "work/api", "--move", "home/infra", "--rename", "gateway")
	if got := e.ok("task", "show", "2"); !strings.Contains(got, "project: home/infra:gateway") {
		t.Errorf("rename+nest:\n%s", got)
	}

	// Projects nest one level: a project with subprojects cannot become one.
	e.ok("project", "add", "home/app:web")
	e.fails(4, "project", "set", "home/app", "--move", "home/infra")
	// Name collisions and self-nesting are refused without changing anything.
	e.ok("project", "add", "work/infra")
	e.fails(4, "project", "set", "work/infra", "--move", "home")
	e.fails(1, "project", "set", "home/infra", "--move", "home/infra")
	e.fails(3, "project", "set", "work/infra", "--move", "nowhere")
	if got := e.ok("task", "show", "1"); !strings.Contains(got, "project: home/app") {
		t.Errorf("failed move changed state:\n%s", got)
	}
}

// project merge folds a project into another: tasks, specs, repos, notes,
// worklog and pins follow, and the source is deleted. Without --yes it only
// previews.
func TestProjectMerge(t *testing.T) {
	e := newEnv(t)
	e.actor = "bot"
	e.ok("ctx", "add", "work")
	e.ok("project", "add", "work/platform")
	e.ok("project", "add", "work/platform:tools", "--repo", "github.com/acme/platform-tools")
	e.ok("task", "add", "cross-repo", "-p", "work/platform:tools")
	e.ok("log", "add", "-t", "1", "did things")
	e.ok("rule", "add", "deny", "pr", "-p", "work/platform:tools", "--quote", "no PRs")
	spec := filepath.Join(t.TempDir(), "s.md")
	if err := os.WriteFile(spec, []byte("# S\n\n## A\n\ntext\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.ok("spec", "add", spec, "-p", "work/platform:tools", "--slug", "s")
	e.ok("project", "use", "work/platform:tools")

	if out := e.ok("project", "merge", "platform:tools", "--into", "platform"); !strings.Contains(out, "would merge") {
		t.Fatalf("preview:\n%s", out)
	}
	if got := e.ok("task", "show", "1"); !strings.Contains(got, "project: work/platform:tools") {
		t.Fatalf("preview wrote:\n%s", got)
	}

	e.ok("project", "merge", "platform:tools", "--into", "platform", "--yes")
	if got := e.ok("task", "show", "1"); !strings.Contains(got, "project: work/platform\n") {
		t.Errorf("task not merged:\n%s", got)
	}
	if got := e.ok("log", "-p", "platform"); !strings.Contains(got, "did things") {
		t.Errorf("worklog not merged:\n%s", got)
	}
	if got := e.ok("spec", "ls", "-p", "platform"); !strings.Contains(got, "s.md") {
		t.Errorf("spec not merged:\n%s", got)
	}
	if got := e.ok("project", "repo", "ls", "platform"); !strings.Contains(got, "platform-tools") {
		t.Errorf("repo not merged:\n%s", got)
	}
	if _, _, code := e.run("check", "pr", "-p", "platform"); code != 20 {
		t.Errorf("rule not merged: exit %d", code)
	}
	if got := e.ok("info"); !strings.Contains(got, "project: platform\n") {
		t.Errorf("pin not merged:\n%s", got)
	}
	e.fails(3, "project", "show", "platform:tools")
	// A project with subprojects is refused.
	e.ok("project", "add", "work/other")
	e.ok("project", "add", "work/platform:api")
	e.fails(4, "project", "merge", "platform", "--into", "other", "--yes")
}
