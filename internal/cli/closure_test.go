package cli

import (
	"strings"
	"testing"
)

func closureEnv(t *testing.T) *env {
	e := newEnv(t)
	e.actor = "tester"
	e.ok("ctx", "add", "w", "--use")
	e.ok("project", "add", "proj", "--use")
	return e
}

func lastObj(v any) map[string]any {
	if a, ok := v.([]any); ok {
		return a[len(a)-1].(map[string]any)
	}
	return v.(map[string]any)
}

func TestLastStepHintAndCloseParent(t *testing.T) {
	e := closureEnv(t)
	e.ok("task", "add", "t", "--step", "a", "--step", "b")
	e.ok("sub", "start", "1.1")
	out := e.ok("sub", "done", "1.1", "--close-parent")
	if !strings.Contains(out, "1 steps still open on task 1") {
		t.Fatalf("got %q", out)
	}
	out = e.ok("sub", "done", "1.2")
	if !strings.Contains(out, "all steps closed; task 1 still in-progress") || !strings.Contains(out, "→ agtrack task done 1") {
		t.Fatalf("got %q", out)
	}
	e.ok("sub", "reopen", "1.2")
	j := lastObj(e.json("sub", "done", "1.2", "--close-parent", "--note", "wrapped"))
	if j["hint"] != nil || j["task_closed"] != true {
		t.Fatalf("got %v", j)
	}
	if !strings.Contains(e.ok("task", "show", "1"), "status: done") {
		t.Fatal("task not closed")
	}
	e.ok("task", "add", "u", "--step", "only")
	j = lastObj(e.json("sub", "done", "2.1"))
	if j["hint"] != "agtrack task done 2" {
		t.Fatalf("got %v", j)
	}
}

func TestSubNext(t *testing.T) {
	e := closureEnv(t)
	e.ok("task", "add", "t", "--step", "a", "--step", "b")
	j := lastObj(e.json("sub", "next", "1"))
	if j["closed"] != nil || j["started"] == nil {
		t.Fatalf("got %v", j)
	}
	j = lastObj(e.json("sub", "next", "1", "--note", "ok"))
	if j["closed"] == nil || j["started"] == nil {
		t.Fatalf("got %v", j)
	}
	j = lastObj(e.json("sub", "next", "1", "--close-parent"))
	if j["task_closed"] != true || j["started"] != nil {
		t.Fatalf("got %v", j)
	}
	e.fails(4, "sub", "next", "1")
}

func TestEvidence(t *testing.T) {
	e := closureEnv(t)
	e.ok("task", "add", "t")
	e.fails(1, "task", "done", "1", "--evidence", "bogus:x")
	e.ok("task", "done", "1", "--evidence", "commit:abc123", "--evidence", "test:go test ./...=0")
	out := e.ok("task", "show", "1")
	if !strings.Contains(out, "commit:abc123") || !strings.Contains(out, "test:go test ./...=0") {
		t.Fatalf("got %q", out)
	}
	e.ok("task", "add", "u")
	e.ok("task", "done", "2")
	if !strings.Contains(e.ok("task", "show", "2"), "none") {
		t.Fatal("expected evidence: none")
	}
	e.ok("task", "add", "v", "--step", "s")
	e.ok("sub", "done", "3.1", "--evidence", "file:x.go")
	if !strings.Contains(e.ok("task", "show", "3"), "file:x.go") {
		t.Fatal("step evidence missing")
	}
}

func TestDoneHintBranchNoPR(t *testing.T) {
	e := closureEnv(t)
	e.ok("task", "add", "t")
	e.ok("task", "artifact", "1", "branch", "feat/x")
	j := lastObj(e.json("task", "done", "1"))
	if j["hint"] != "agtrack pr add 1 <url>" {
		t.Fatalf("got %v", j)
	}
}

func TestClaimExpiryHint(t *testing.T) {
	e := closureEnv(t)
	e.ok("task", "add", "t")
	j := lastObj(e.json("task", "claim", "1", "--ttl", "5m"))
	if j["hint"] != "agtrack task claim 1 --ttl 1h" {
		t.Fatalf("got %v", j)
	}
	j = lastObj(e.json("task", "claim", "1", "--ttl", "2h"))
	if j["hint"] != nil {
		t.Fatalf("got %v", j)
	}
}

func TestSnoozeAndNext(t *testing.T) {
	e := closureEnv(t)
	e.ok("task", "add", "t", "--status", "in-progress")
	e.ok("task", "add", "u", "--status", "in-progress")
	e.fails(1, "task", "snooze", "1", "--until", "2020-01-01")
	e.ok("task", "snooze", "1", "--until", "3d", "--note", "waiting")
	if out := e.ok("next"); strings.Contains(out, "#1 ") || !strings.Contains(out, "#2 ") {
		t.Fatalf("got %q", out)
	}
	if out := e.ok("next", "--parked"); !strings.Contains(out, "#1 ") || strings.Contains(out, "#2 ") {
		t.Fatalf("got %q", out)
	}
	if out := e.ok("task", "ls"); !strings.Contains(out, "parked until") {
		t.Fatalf("got %q", out)
	}
	e.ok("task", "unsnooze", "1")
	if out := e.ok("next"); !strings.Contains(out, "#1 ") {
		t.Fatalf("got %q", out)
	}
}

func TestStaleNextAndDoneCheck(t *testing.T) {
	e := closureEnv(t)
	e.ok("task", "add", "finished", "--status", "in-progress", "--step", "s")
	e.ok("sub", "done", "1.1")
	e.ok("task", "add", "fresh", "--status", "in-progress", "--step", "s")
	rows := e.json("next").([]any)
	if len(rows) != 2 || rows[0].(map[string]any)["id"].(float64) != 2 || rows[1].(map[string]any)["stale_reason"] != "all-steps-done" {
		t.Fatalf("order/reason wrong: %v", rows)
	}
	if out := e.ok("next"); !strings.Contains(out, "! ") {
		t.Fatalf("no marker: %q", out)
	}
	only := e.json("next", "--stale").([]any)
	if len(only) != 1 {
		t.Fatalf("got %v", only)
	}
	t.Setenv("AGTRACK_NOW", "2099-01-01T00:00:00Z")
	if got := e.json("next", "--stale").([]any); len(got) != 2 {
		t.Fatalf("idle: %v", got)
	}
	t.Setenv("AGTRACK_NOW", "")
	chk := e.json("task", "done-check").([]any)
	if len(chk) != 1 || chk[0].(map[string]any)["command"] != "agtrack task done 1" {
		t.Fatalf("got %v", chk)
	}
	e.fails(1, "task", "done-check", "--apply")
	e.ok("task", "done-check", "--apply", "--yes")
	if !strings.Contains(e.ok("task", "show", "1"), "status: done") {
		t.Fatal("not closed")
	}
	e.ok("config", "set", "next.stale_days", "3")
	e.fails(1, "config", "set", "next.stale_days", "x")
}
