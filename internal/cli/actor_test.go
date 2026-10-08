package cli

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/l4sh/agtrack/internal/db"
	"github.com/l4sh/agtrack/internal/store"
)

// q1 runs a one-value query against the test database.
func (e *env) q1(query string, args ...any) string {
	e.t.Helper()
	d, err := db.Open(e.db)
	if err != nil {
		e.t.Fatal(err)
	}
	defer d.Close()
	var v sql.NullString
	if err := d.QueryRow(query, args...).Scan(&v); err != nil {
		e.t.Fatal(err)
	}
	return v.String
}

// D1: a fresh database never warns; aliases are rewritten to the slug on
// every write; unknown actors warn, or fail under actor.strict.
func TestActorRegistryCanonicalAndStrict(t *testing.T) {
	e := newEnv(t)
	e.actor = "claude-opus-5.5"
	if _, errb, code := e.run("ctx", "add", "w"); code != 0 || errb != "" {
		t.Fatalf("fresh db: code %d stderr %q", code, errb)
	}
	e.ok("project", "add", "p")
	e.ok("actor", "add", "claude-opus-5-5", "--alias", "claude-opus-5.5", "--alias", "opus")
	e.ok("task", "add", "t1", "-p", "p")
	e.ok("--actor", "opus", "task", "claim", "1")
	if got := e.q1("SELECT claimed_by FROM tasks WHERE id = 1"); got != "claude-opus-5-5" {
		t.Errorf("claimed_by = %q, want canonical slug", got)
	}
	if got := e.q1("SELECT actor FROM events WHERE action = 'created' AND target_type = 'task'"); got != "claude-opus-5-5" {
		t.Errorf("event actor = %q", got)
	}
	_, errb, code := e.run("--actor", "stranger", "log", "add", "hello")
	if code != 0 || !strings.Contains(errb, "unknown actor") {
		t.Fatalf("unknown actor: code %d stderr %q", code, errb)
	}
	if _, errb, code := e.run("--actor", "stranger", "task", "ls"); code != 0 || errb != "" {
		t.Errorf("read warned: %q", errb)
	}
	e.ok("config", "set", "actor.strict", "true")
	e.fails(1, "--actor", "stranger", "log", "add", "nope")
	e.ok("--actor", "opus", "log", "add", "fine")
}

// D2: actor.required fails writes with no actor, with the fix-it message.
func TestActorRequired(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w") // no actor yet, not required
	e.ok("config", "set", "actor.required", "true")
	msg := e.fails(1, "project", "add", "p")
	if !strings.Contains(msg, "AGTRACK_ACTOR") || !strings.Contains(msg, "--actor") {
		t.Errorf("message %q", msg)
	}
	e.ok("ctx", "ls") // reads are fine
	e.actor = "bot"
	e.ok("project", "add", "p")
}

// D1 merge: every actor-bearing column is rewritten, needs --yes, and rm
// refuses a used actor.
func TestActorMergeAndRm(t *testing.T) {
	e := newEnv(t)
	e.actor = "claude-opus-5.5"
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "p")
	e.ok("task", "add", "t1", "-p", "p")
	e.ok("task", "claim", "1")
	e.ok("task", "note", "1", "--kind", "handoff", "hi")
	e.ok("actor", "add", "claude-opus-5-5")
	e.ok("actor", "add", "unused")
	e.fails(2, "actor", "merge", "claude-opus-5.5", "--into", "claude-opus-5-5")
	e.fails(3, "actor", "merge", "claude-opus-5.5", "--into", "ghost", "--yes")
	e.ok("actor", "merge", "claude-opus-5.5", "--into", "claude-opus-5-5", "--yes")
	for _, q := range []string{
		"SELECT COUNT(*) FROM events WHERE actor = 'claude-opus-5.5'",
		"SELECT COUNT(*) FROM notes WHERE actor = 'claude-opus-5.5'",
		"SELECT COUNT(*) FROM tasks WHERE claimed_by = 'claude-opus-5.5'",
		"SELECT COUNT(*) FROM meta WHERE key LIKE '%:claude-opus-5.5'",
	} {
		if got := e.q1(q); got != "0" {
			t.Errorf("%s = %s", q, got)
		}
	}
	if got := e.q1("SELECT claimed_by FROM tasks WHERE id = 1"); got != "claude-opus-5-5" {
		t.Errorf("claimed_by %q", got)
	}
	e.ok("log", "add", "after merge") // the old spelling now resolves
	if got := e.q1("SELECT COUNT(*) FROM events WHERE actor = 'claude-opus-5.5'"); got != "0" {
		t.Errorf("old alias written after merge: %s", got)
	}
	e.fails(4, "actor", "rm", "claude-opus-5-5")
	e.ok("actor", "rm", "unused")
	if !strings.Contains(e.ok("actor", "ls", "--ids"), "claude-opus-5-5") {
		t.Error("ls --ids")
	}
}

// config: new keys validate; approvers resolve aliases.
func TestConfigKeysAndApprovers(t *testing.T) {
	e := newEnv(t)
	e.actor = "bot"
	e.ok("ctx", "add", "w")
	e.fails(1, "config", "set", "actor.strict", "maybe")
	e.fails(1, "config", "set", "next.stale_days", "0")
	e.fails(1, "config", "set", "git.auto", "sometimes")
	e.fails(1, "config", "set", "nope.key", "x")
	e.ok("config", "set", "next.stale_days", "14")
	e.ok("config", "set", "lessons.stale_days", "30")
	e.ok("config", "set", "git.auto", "false")
	m := obj(e.json("config", "get"))
	if m["next.stale_days"] != "14" || m["rules.approvers"] != "human" || m["actor.required"] != "false" {
		t.Errorf("config get = %v", m)
	}
	e.ok("actor", "add", "luis", "--kind", "human", "--alias", "lz")
	e.ok("config", "set", "rules.approvers", "luis, human")
	d, err := db.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := store.New(d, "bot")
	for actor, want := range map[string]bool{"luis": true, "lz": true, "human": true, "bot": false, "": false} {
		if got := s.IsApprover(actor); got != want {
			t.Errorf("IsApprover(%q) = %v", actor, got)
		}
	}
}
