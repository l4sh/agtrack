package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/l4sh/agtrack/internal/refs"
)

func coordEnv(t *testing.T) *env {
	e := newEnv(t)
	e.actor = "alice"
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "p")
	e.ok("task", "add", "work", "-p", "p", "--step", "one", "--step", "two")
	return e
}

func ids(v any) []string {
	var out []string
	for _, r := range v.([]any) {
		out = append(out, strings.TrimSpace(strings.Split(strings.TrimSpace(r.(map[string]any)["body"].(string)), "\n")[0]))
	}
	return out
}

// C1: ack is idempotent, shows in note show / notes, and filters --unacked.
func TestNoteAck(t *testing.T) {
	e := coordEnv(t)
	e.ok("task", "note", "1", "--kind", "handoff", "pick this up")
	e.ok("task", "note", "1", "plain note")
	e.fails(1, "note", "ack", "2") // plain note
	e.fails(3, "note", "ack", "99")
	if got := ids(e.json("notes", "-p", "p", "--unacked")); len(got) != 1 || got[0] != "pick this up" {
		t.Fatalf("unacked before = %v", got)
	}
	e.actor = "bob"
	out := obj(e.json("note", "ack", "1", "--note", "on it"))
	a := out["acknowledged"].([]any)[0].(map[string]any)
	if a["changed"] != true || a["acknowledged_by"] != "bob" {
		t.Fatalf("ack = %v", a)
	}
	again := obj(e.json("note", "ack", "1"))["acknowledged"].([]any)[0].(map[string]any)
	if again["changed"] != false || again["acknowledged_by"] != "bob" || again["acknowledged_at"] != a["acknowledged_at"] {
		t.Fatalf("second ack = %v", again)
	}
	if got := e.json("notes", "-p", "p", "--unacked"); len(got.([]any)) != 0 {
		t.Fatalf("unacked after = %v", got)
	}
	if got := e.json("task", "notes", "1", "--unacked"); len(got.([]any)) != 0 {
		t.Fatalf("task notes --unacked after = %v", got)
	}
	n := obj(e.json("note", "show", "1"))
	if n["acknowledged_by"] != "bob" {
		t.Fatalf("note show = %v", n)
	}
	if s := e.ok("note", "show", "1"); !strings.Contains(s, "acknowledged by bob") {
		t.Fatalf("note show text: %s", s)
	}
	// Acknowledged but unresolved stays listed as open.
	if got := e.json("notes", "-p", "p", "--kind", "handoff"); len(got.([]any)) != 1 {
		t.Fatalf("open handoffs = %v", got)
	}
}

// C2: the handoff and the transition are one write.
func TestHandoffClose(t *testing.T) {
	e := coordEnv(t)
	e.ok("task", "note", "1", "--kind", "handoff", "--close", "1.1", "step one is done")
	if got := e.ok("sub", "ls", "1", "-f", "plain"); !strings.Contains(got, "done") {
		t.Fatalf("steps: %s", got)
	}
	e.fails(1, "task", "note", "1", "--close", "9.1", "wrong task")
	e.fails(1, "task", "note", "1", "--kind", "blocker", "--close", "1.2", "wrong kind")
	// nothing written by the failures
	if got := e.json("task", "notes", "1"); len(got.([]any)) != 1 {
		t.Fatalf("notes = %v", got)
	}
	e.ok("task", "note", "1", "--close", "1", "all done")
	if st := obj(e.json("task", "show", "1"))["status"]; st != "done" {
		t.Fatalf("task status = %v", st)
	}
	if got := e.json("task", "notes", "1", "--kind", "handoff"); len(got.([]any)) != 2 {
		t.Fatalf("handoffs = %v", got)
	}
}

// C3: claims lists live claims, --stale filters on quiet holders, and
// watch --until stale exits 14.
func TestClaimsStale(t *testing.T) {
	e := coordEnv(t)
	e.ok("task", "claim", "1", "--ttl", "0")
	rows := e.json("claims").([]any)
	if len(rows) != 1 || obj(rows[0])["holder"] != "alice" {
		t.Fatalf("claims = %v", rows)
	}
	if got := e.json("claims", "--stale", "15m").([]any); len(got) != 0 {
		t.Fatalf("fresh claim is stale: %v", got)
	}
	if got := e.json("claims", "-p", "p").([]any); len(got) != 1 {
		t.Fatalf("claims -p = %v", got)
	}
	t.Setenv("AGTRACK_NOW", refs.Now().Add(2*time.Hour).Format(time.RFC3339))
	got := e.json("claims", "--stale", "15m").([]any)
	if len(got) != 1 || obj(got[0])["last_seen"] == nil {
		t.Fatalf("stale claims = %v", got)
	}
	out, _, code := e.run("watch", "--until", "stale", "--stale", "15m", "--timeout", "5s", "--interval", "100ms")
	if code != ExitStale || !strings.Contains(out, "TERMINAL stale") || !strings.Contains(out, "#1 held by alice") {
		t.Fatalf("watch: exit %d\n%s", code, out)
	}
	// A write by the holder makes the claim fresh again.
	e.ok("log", "add", "-t", "1", "still here")
	if got := e.json("claims", "--stale", "15m").([]any); len(got) != 0 {
		t.Fatalf("claim still stale after a write: %v", got)
	}
}

// C4: transfer releases, re-claims for the receiver and leaves a handoff
// note to acknowledge; the receiver's first write adopts a session.
func TestTransfer(t *testing.T) {
	e := coordEnv(t)
	e.ok("task", "claim", "1")
	e.fails(1, "task", "transfer", "1")
	e.fails(1, "task", "transfer", "1", "--to", "alice")
	e.ok("task", "transfer", "1", "--to", "bob", "--note", "yours now")
	c := e.json("claims").([]any)
	if len(c) != 1 || obj(c[0])["holder"] != "bob" || obj(c[0])["session"] != nil {
		t.Fatalf("claims = %v", c)
	}
	if got := ids(e.json("notes", "-p", "p", "--unacked")); len(got) != 1 || got[0] != "yours now" {
		t.Fatalf("unacked = %v", got)
	}
	e.actor = "carol"
	e.fails(4, "task", "transfer", "1", "--to", "dave") // bob holds it
	e.ok("task", "transfer", "1", "--to", "dave", "--force")
	if h := obj(e.json("claims").([]any)[0])["holder"]; h != "dave" {
		t.Fatalf("holder = %v", h)
	}
}
