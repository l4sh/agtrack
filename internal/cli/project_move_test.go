package cli

import (
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
