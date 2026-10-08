package cli

import (
	"testing"

	"github.com/l4sh/agtrack/internal/db"
	"github.com/l4sh/agtrack/internal/row"
)

// agent-features spec B1 + C3: writes carry the session; a claim records the
// holder's session, and the holder's later writes refresh claim_last_seen.
func TestSessionStampedAndClaimLiveness(t *testing.T) {
	e := newEnv(t)
	e.actor = "bot"
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "p")
	e.ok("task", "add", "work", "-p", "p")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "harness-1")
	e.ok("task", "claim", "1", "--ttl", "1h")
	e.ok("--session", "explicit-2", "log", "add", "-t", "1", "from another session")

	d, err := db.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rs, err := d.Query("SELECT action, session FROM events WHERE task_id = 1 ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	evs, err := row.Scan(rs)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, ev := range evs {
		got[ev.Str("action")] = ev.Get("session")
	}
	if got["created"] != nil || got["claim"] != "harness-1" || got["log"] != "explicit-2" {
		t.Errorf("sessions by action = %v", got)
	}
	var sess, seen, claimed string
	if err := d.QueryRow("SELECT claim_session, claim_last_seen, claimed_at FROM tasks WHERE id = 1").Scan(&sess, &seen, &claimed); err != nil {
		t.Fatal(err)
	}
	if sess != "harness-1" || seen < claimed {
		t.Errorf("claim session %q last_seen %q claimed_at %q", sess, seen, claimed)
	}

	e.ok("task", "release", "1")
	var nullSess any
	if err := d.QueryRow("SELECT claim_session FROM tasks WHERE id = 1").Scan(&nullSess); err != nil || nullSess != nil {
		t.Errorf("release left claim_session %v (%v)", nullSess, err)
	}
}

// info names the session only when one is detected.
func TestInfoSession(t *testing.T) {
	e := newEnv(t)
	if m := obj(e.json("info")); m["session"] != nil {
		t.Errorf("session without a harness: %v", m["session"])
	}
	t.Setenv("AGTRACK_SESSION", "s-9")
	m := obj(e.json("info"))
	if m["session"] != "s-9" || m["session_source"] != "$AGTRACK_SESSION" {
		t.Errorf("info session %v from %v", m["session"], m["session_source"])
	}
}

// resume's `stale` key carries the A2 staleness of the current task.
func TestResumeStaleFromClosure(t *testing.T) {
	e := newEnv(t)
	e.actor = "bot"
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "p")
	e.ok("task", "add", "work", "-p", "p", "--step", "only")
	e.ok("task", "claim", "1")
	e.ok("sub", "done", "1.1")
	st, ok := obj(e.json("resume", "-p", "p"))["stale"].(map[string]any)
	if !ok || st["stale_reason"] != "all-steps-done" {
		t.Errorf("resume stale = %v", obj(e.json("resume", "-p", "p"))["stale"])
	}
	e.ok("task", "done", "1")
	if s := obj(e.json("resume", "-p", "p"))["stale"]; s != nil {
		t.Errorf("closed task still stale: %v", s)
	}
}
