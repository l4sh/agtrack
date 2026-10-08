package cli

import (
	"strings"
	"testing"
)

func findingTypes(v any) map[string]bool {
	out := map[string]bool{}
	for _, r := range v.([]any) {
		m := r.(map[string]any)
		if m["suggested"] == nil || m["suggested"] == "" {
			out["NO-SUGGESTED:"+m["type"].(string)] = true
		}
		out[m["type"].(string)] = true
	}
	return out
}

func TestDoctor(t *testing.T) {
	e := coordEnv(t)
	e.ok("actor", "add", "alice")
	e.ok("actor", "alias", "alice", "al")
	e.ok("task", "note", "1", "--kind", "handoff", "pick up")
	e.ok("task", "claim", "1")
	e.ok("sub", "done", "1.1")
	e.ok("sub", "done", "1.2")
	e.actor = "bot"
	e.ok("rule", "add", "allow", "push", "-p", "p", "--quote", "q")
	e.actor = "alice"
	got := findingTypes(e.json("doctor"))
	for _, want := range []string{"finished_parent_open", "unacked_handoff"} {
		if !got[want] {
			t.Fatalf("missing %s in %v", want, got)
		}
	}
	for k := range got {
		if strings.HasPrefix(k, "NO-SUGGESTED") {
			t.Fatalf("finding without suggested: %v", got)
		}
	}
	e.ok("doctor")
	e.fails(4, "doctor", "--strict")
	e.ok("task", "done", "1")
	e.ok("note", "ack", "1")
	e.ok("actor", "add", "bot")
	e.ok("actor", "add", "human")
	e.actor = "human"
	e.ok("rule", "approve", "1")
	e.actor = "alice"
	e.ok("doctor", "--strict")
}

func TestInfoUsage(t *testing.T) {
	e := coordEnv(t)
	e.ok("task", "claim", "1")
	o := obj(e.json("info", "--usage", "--since", "30d"))
	if o["events"].(float64) < 1 || o["families"] == nil || o["actors"] == nil || o["no_actor"] == nil {
		t.Fatalf("usage = %v", o)
	}
	if !strings.Contains(e.ok("info", "--usage"), "families") {
		t.Fatal("text output")
	}
}

func TestClaimAckHint(t *testing.T) {
	e := coordEnv(t)
	e.ok("task", "note", "1", "--kind", "handoff", "pick up")
	e.actor = "bob"
	o := obj(e.json("task", "claim", "1"))
	if o["hint"] != "agtrack note ack 1" {
		t.Fatalf("hint = %v", o["hint"])
	}
	e.ok("task", "release", "1")
	e.ok("note", "ack", "1")
	if o := obj(e.json("task", "claim", "1")); o["hint"] == "agtrack note ack 1" {
		t.Fatalf("hint after ack = %v", o["hint"])
	}
}

func TestSubNextNoGit(t *testing.T) {
	e := coordEnv(t)
	e.ok("sub", "next", "1", "--no-git")
}

func TestWatchActorNotAlias(t *testing.T) {
	e := coordEnv(t)
	e.ok("actor", "add", "alice")
	e.ok("actor", "alias", "alice", "al")
	e.ok("task", "note", "1", "x")
	out, _, _ := e.run("watch", "--actor-not", "al", "--after", "0", "--timeout", "300ms", "--interval", "50ms")
	if strings.Contains(out, "alice") {
		t.Fatalf("alias not resolved: %s", out)
	}
}
