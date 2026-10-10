package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/l4sh/agtrack/internal/db"
)

func (e *env) sql(query string, args ...any) {
	e.t.Helper()
	d, err := db.Open(e.db)
	if err != nil {
		e.t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(query, args...); err != nil {
		e.t.Fatal(err)
	}
}

func continuityEnv(t *testing.T) *env {
	e := newEnv(t)
	e.actor = "bot"
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "p", "--use")
	return e
}

// B1: resume and next prefer this actor's claim from this session; the same
// actor's claim from another session is shown as such, never as mine.
func TestResumePrefersThisSession(t *testing.T) {
	e := continuityEnv(t)
	e.ok("task", "add", "one")
	e.ok("task", "add", "two")
	e.ok("task", "start", "1", "2")
	e.ok("--session", "s1", "task", "claim", "1")
	e.ok("--session", "s2", "task", "claim", "2")

	for sess, want := range map[string]float64{"s1": 1, "s2": 2} {
		p := obj(e.json("--session", sess, "resume", "-p", "p"))
		cur := obj(p["current_task"])
		if cur["id"] != want || cur["claim_mine"] != true {
			t.Errorf("session %s resumed %v", sess, cur)
		}
	}
	// An actor without a session cannot tell its claims apart: either counts.
	if cur := obj(obj(e.json("resume", "-p", "p"))["current_task"]); cur["claim_mine"] != true {
		t.Errorf("no session: %v", cur)
	}
	out := e.ok("--session", "s1", "resume", "-p", "p")
	if !strings.Contains(out, "#2 claimed by bot (other session s2)") {
		t.Errorf("held line missing other session:\n%s", out)
	}
	p := obj(e.json("--session", "s3", "resume", "-t", "1"))
	if cur := obj(p["current_task"]); cur["claim_mine"] != false || cur["claim_session"] != "s1" {
		t.Errorf("other session shown as mine: %v", cur)
	}
	for _, c := range p["claims"].([]any) {
		if obj(c)["mine"] != false {
			t.Errorf("claims from s3: %v", c)
		}
	}
	if out := e.ok("--session", "s3", "resume", "-t", "1"); !strings.Contains(out, "claim     bot (other session s1)") {
		t.Errorf("claim line:\n%s", out)
	}

	// next: mine first, the other session marked.
	out = e.ok("--session", "s2", "next")
	if !strings.HasPrefix(out, "#2 ") || !strings.Contains(out, "bot (other session s1) until") {
		t.Errorf("next from s2:\n%s", out)
	}
	rows := e.json("--session", "s2", "next").([]any)
	if obj(rows[0])["claim_mine"] != true || obj(rows[1])["claim_mine"] != false {
		t.Errorf("next claim_mine: %v", rows)
	}
	// next --claim does not take over the same actor's other session.
	e.ok("task", "add", "three")
	got := obj(e.json("--session", "s3", "next", "--claim"))
	if got["id"] != float64(3) {
		t.Errorf("next --claim from s3 took %v", got["id"])
	}
}

func TestWatchSessionNotSelf(t *testing.T) {
	e := continuityEnv(t)
	e.ok("--session", "me", "task", "add", "mine")
	e.ok("--session", "other", "task", "add", "theirs")
	out, _, code := e.run("--session", "me", "watch", "-p", "p", "--after", "0", "--session-not", "self", "--timeout", "300ms", "--interval", "50ms")
	if code != ExitTimeout || strings.Contains(out, "mine") || !strings.Contains(out, "theirs") {
		t.Fatalf("watch --session-not self: %d\n%s", code, out)
	}
}

// B2: versioned checkpoints with a 64 KiB cap.
func TestTaskCheckpoint(t *testing.T) {
	e := continuityEnv(t)
	e.ok("task", "add", "work")
	e.fails(1, "task", "checkpoint", "1", "--body", "  ")
	e.fails(3, "task", "checkpoint", "1", "--show")
	if out := e.ok("task", "checkpoint", "1", "--body", "# state\nparsed", "--next", "invalidate"); !strings.Contains(out, "checkpoint v1 on #1") {
		t.Fatalf("create: %s", out)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "state.md")
	os.WriteFile(f, []byte("second\n"), 0o600)
	if n := strings.TrimSpace(e.ok("-q", "--session", "s9", "task", "checkpoint", "1", "--file", f)); n != "2" {
		t.Fatalf("-q prints the version: %q", n)
	}
	orig := checkpointStdin
	checkpointStdin = func() io.Reader { return strings.NewReader("from stdin") }
	e.ok("task", "checkpoint", "1", "--next", "third")
	checkpointStdin = orig

	latest := obj(e.json("task", "checkpoint", "1", "--show"))
	if latest["n"] != float64(3) || latest["body"] != "from stdin" || latest["next"] != "third" {
		t.Errorf("latest: %v", latest)
	}
	if v2 := obj(e.json("task", "checkpoint", "1", "--show", "--n", "2")); v2["body"] != "second" || v2["session"] != "s9" {
		t.Errorf("v2: %v", v2)
	}
	e.fails(3, "task", "checkpoint", "1", "--show", "--n", "9")
	hist := e.json("task", "checkpoint", "1", "--history").([]any)
	if len(hist) != 3 || obj(hist[0])["n"] != float64(3) || obj(hist[0])["body"] != nil {
		t.Errorf("history: %v", hist)
	}
	if out := e.ok("task", "checkpoint", "1", "--history"); !strings.Contains(out, "invalidate") {
		t.Errorf("history table:\n%s", out)
	}
	e.fails(2, "task", "checkpoint", "1", "--show", "--next", "x")
	e.fails(1, "task", "checkpoint", "1", "--body", strings.Repeat("x", 64<<10+1))
	e.ok("task", "checkpoint", "1", "--body", strings.Repeat("x", 64<<10))
	e.fails(1, "task", "checkpoint", "1", "--next", "two\nlines")
	// --idem replays the first write.
	a := obj(e.json("task", "checkpoint", "1", "--next", "once", "--idem", "k1"))
	b := obj(e.json("task", "checkpoint", "1", "--next", "once", "--idem", "k1"))
	if a["n"] != b["n"] || a["n"] != float64(5) {
		t.Errorf("idem: %v %v", a["n"], b["n"])
	}
	// resume shows the latest next line, the body only on request.
	p := obj(e.json("resume", "-t", "1"))
	cp := obj(p["checkpoint"])
	if cp["next"] != "once" || cp["body"] != nil {
		t.Errorf("resume checkpoint: %v", cp)
	}
	if out := e.ok("resume", "-t", "1"); !strings.Contains(out, "checkpt   v5") || !strings.Contains(out, "next once") {
		t.Errorf("resume text:\n%s", out)
	}
	if cp := obj(obj(e.json("resume", "-t", "1", "--complete"))["checkpoint"]); cp["body"] == nil {
		t.Errorf("--complete has no body: %v", cp)
	}
}

// B6: the stable keys are always present, also with no task in progress.
func TestResumeStableKeys(t *testing.T) {
	e := continuityEnv(t)
	keys := []string{"project", "current_task", "next_step", "checkpoint", "unacked_handoffs", "open_notes", "rules",
		"lessons", "claims", "artifacts", "prs", "evidence", "stale", "context_mismatch", "cursor", "omitted"}
	check := func(p map[string]any) {
		t.Helper()
		for _, k := range keys {
			if _, ok := p[k]; !ok {
				t.Errorf("resume -f json lacks %q", k)
			}
		}
	}
	check(obj(e.json("resume", "-p", "p")))
	e.ok("task", "add", "work", "--step", "a", "--step", "b", "--pr", "https://github.com/o/r/pull/7")
	e.ok("sub", "start", "1.2")
	p := obj(e.json("resume", "-p", "p"))
	check(p)
	if obj(p["next_step"])["ref"] != "1.2" || len(p["prs"].([]any)) != 1 {
		t.Errorf("next_step/prs: %v %v", p["next_step"], p["prs"])
	}
	out := e.ok("describe", "resume")
	for _, k := range keys {
		if !strings.Contains(out, `"key": "`+k+`"`) {
			t.Errorf("describe resume does not document %q", k)
		}
	}
}

// Handoffs, evidence, rules and lessons come from the v4 tables.
func TestResumeHandoffsEvidenceRulesLessons(t *testing.T) {
	e := continuityEnv(t)
	e.ok("task", "add", "work")
	e.ok("task", "start", "1")
	e.ok("task", "note", "1", "--kind", "handoff", "taken")
	e.ok("task", "note", "1", "--kind", "blocker", "waiting")
	e.ok("project", "note", "p", "--kind", "question", "which db?")
	e.ok("task", "note", "1", "plain")
	e.sql("UPDATE notes SET acknowledged_at = '2026-10-01T00:00:00Z', acknowledged_by = 'x' WHERE body = 'taken'")
	now := "2026-10-01T00:00:00Z"
	for i, r := range [][]any{
		{"deny", "push", nil, "global", nil, 0},
		{"require", nil, "tests-pass", "project", 1, 0},
		{"remember", nil, nil, "project", 1, 0},
		{"deny", "release", nil, "task", 1, 1},
		{"deny", "merge", nil, "project", 99, 0},
	} {
		e.sql(`INSERT INTO rules (uid, kind, verb, check_name, text, scope_type, scope_id, hard, created_at) VALUES (?,?,?,?,?,?,?,?,?)`,
			"u"+string(rune('a'+i)), r[0], r[1], r[2], "t", r[3], r[4], r[5], now)
	}
	for i := 0; i < 7; i++ {
		e.sql(`INSERT INTO lessons (uid, title, scope_type, scope_id, created_at) VALUES (?,?,?,?,?)`, "l"+string(rune('a'+i)), "lesson "+string(rune('a'+i)), "project", 1, now)
	}
	e.sql(`INSERT INTO task_evidence (task_id, event_id, kind, value, created_at) VALUES (1, 1, 'commit', 'old', ?), (1, 2, 'commit', 'abc123', ?), (1, 2, 'test', 'go test=0', ?)`, now, now, now)

	p := obj(e.json("resume", "-p", "p"))
	var hs []string
	for _, h := range p["unacked_handoffs"].([]any) {
		hs = append(hs, obj(h)["body"].(string))
	}
	if strings.Join(hs, "|") != "waiting|which db?" {
		t.Errorf("unacked_handoffs: %v", hs)
	}
	var open []string
	for _, n := range p["open_notes"].([]any) {
		open = append(open, obj(n)["body"].(string))
	}
	if strings.Join(open, "|") != "taken" { // "plain" is a record, never open
		t.Errorf("open_notes: %v", open)
	}
	rules := p["rules"].([]any)
	if len(rules) != 3 || obj(rules[0])["scope"] != "task" || obj(rules[0])["hard"] != true || obj(rules[2])["scope"] != "global" {
		t.Errorf("rules: %v", rules)
	}
	if len(p["lessons"].([]any)) != 5 {
		t.Errorf("lessons: %v", p["lessons"])
	}
	ev := p["evidence"].([]any)
	if len(ev) != 2 || obj(ev[0])["value"] != "abc123" {
		t.Errorf("evidence: %v", ev)
	}
	found := false
	for _, o := range p["omitted"].([]any) {
		if obj(o)["what"] == "lessons" && obj(o)["count"] == float64(2) {
			found = true
		}
	}
	if !found {
		t.Errorf("2 lessons not named in omitted: %v", p["omitted"])
	}
	out := e.ok("resume", "-p", "p")
	if !strings.Contains(out, "rules     HARD deny release") || !strings.Contains(out, "lessons   lesson g") {
		t.Errorf("text rules/lessons:\n%s", out)
	}
}

// B3: a delta from a cursor holds only what changed after it.
func TestResumeSince(t *testing.T) {
	e := continuityEnv(t)
	e.ok("task", "add", "work")
	e.ok("task", "start", "1")
	cursor := obj(e.json("resume", "-p", "p"))["cursor"].(float64)
	d := obj(e.json("resume", "-p", "p", "--since", ftoa(cursor)))
	if len(d["events"].([]any)) != 0 || d["cursor"] != cursor {
		t.Fatalf("empty delta: %v", d)
	}
	e.ok("--session", "s1", "task", "claim", "1")
	e.ok("task", "note", "1", "--kind", "handoff", "new note")
	e.ok("task", "checkpoint", "1", "--next", "go")
	e.ok("task", "add", "other")
	e.ok("task", "done", "2")
	d = obj(e.json("--session", "s1", "resume", "-p", "p", "--since", ftoa(cursor)))
	if n := len(d["events"].([]any)); n != 5 {
		t.Errorf("events: %d %v", n, d["events"])
	}
	if ns := d["notes"].([]any); len(ns) != 1 || obj(ns[0])["body"] != "new note" {
		t.Errorf("notes: %v", ns)
	}
	if cps := d["checkpoints"].([]any); len(cps) != 1 || obj(cps[0])["next"] != "go" {
		t.Errorf("checkpoints: %v", cps)
	}
	cl := d["claims"].([]any)
	if len(cl) != 2 || obj(cl[0])["claimed_by"] != "bot" || obj(cl[0])["mine"] != true || obj(cl[1])["claimed_by"] != nil {
		t.Errorf("claims: %v", cl)
	}
	if st := d["status"].([]any); len(st) != 1 || obj(st[0])["status"] != "done" {
		t.Errorf("status: %v", st)
	}
	for _, k := range []string{"project", "current_task", "since", "cursor", "omitted"} {
		if _, ok := d[k]; !ok {
			t.Errorf("delta lacks %q", k)
		}
	}
	if out := e.ok("resume", "-p", "p", "--since", ftoa(cursor)); !strings.Contains(out, "new note") {
		t.Errorf("delta text:\n%s", out)
	}
	e.fails(2, "resume", "--since", "1", "--section", "task")
}

func ftoa(f float64) string { return fmt.Sprint(int64(f)) }

// B4: --section returns only the named sections and names the rest.
func TestResumeSections(t *testing.T) {
	e := continuityEnv(t)
	e.ok("task", "add", "work", "--step", "a")
	e.ok("task", "start", "1")
	e.ok("task", "checkpoint", "1", "--body", "full body", "--next", "n")
	p := obj(e.json("resume", "-p", "p", "--section", "checkpoint,steps"))
	if obj(p["checkpoint"])["body"] != "full body" || p["steps"] == nil || p["next_step"] == nil {
		t.Errorf("sections: %v", p)
	}
	if _, ok := p["current_task"]; ok {
		t.Errorf("current_task not requested: %v", p)
	}
	named := map[string]bool{}
	for _, o := range p["omitted"].([]any) {
		named[obj(o)["what"].(string)] = true
		if r := obj(o)["retrieve"].(string); strings.HasPrefix(obj(o)["what"].(string), "section ") && !strings.Contains(r, "--section") {
			t.Errorf("retrieve: %s", r)
		}
	}
	if !named["section task"] || !named["section log"] || named["section checkpoint"] || named["section rules"] {
		t.Errorf("omitted sections: %v", named)
	}
	if out := e.ok("resume", "-p", "p", "--section", "checkpoint"); !strings.Contains(out, "full body") {
		t.Errorf("text section:\n%s", out)
	}
	e.fails(2, "resume", "--section", "bogus")
}

// B5: the cwd's branch and worktree against the task's artifacts.
func TestContextCheck(t *testing.T) {
	e := continuityEnv(t)
	orig := cwdGit
	defer func() { cwdGit = orig }()
	wt := t.TempDir()
	cwdGit = func() (string, string) { return "develop", wt }
	e.ok("task", "add", "work")
	e.ok("task", "start", "1")
	if out := e.ok("task", "verify-context", "1"); !strings.Contains(out, "no branch or worktree artifact") {
		t.Errorf("no artifact: %s", out)
	}
	if obj(e.json("resume", "-p", "p"))["context_mismatch"] != nil {
		t.Error("mismatch without artifacts")
	}
	e.ok("task", "artifact", "1", "branch", "feat/x")
	errOut := e.fails(4, "task", "verify-context", "1")
	if !strings.Contains(errOut, "MISMATCH: task 1 is on feat/x; this checkout is on develop") {
		t.Errorf("verify-context: %s", errOut)
	}
	if out := e.ok("resume", "-p", "p"); !strings.Contains(out, "MISMATCH: task 1 is on feat/x; this checkout is on develop") {
		t.Errorf("resume text:\n%s", out)
	}
	m := obj(obj(e.json("resume", "-p", "p"))["context_mismatch"])
	if m["cwd_branch"] != "develop" || m["ok"] != false {
		t.Errorf("context_mismatch: %v", m)
	}
	cwdGit = func() (string, string) { return "feat/x", wt }
	e.ok("task", "verify-context", "1")
	e.ok("task", "artifact", "1", "worktree", filepath.Join(wt, "elsewhere"))
	e.fails(4, "task", "verify-context", "1")
	e.ok("task", "artifact", "1", "worktree", wt)
	if got := obj(e.json("task", "verify-context", "1")); got["ok"] != true {
		t.Errorf("worktree match: %v", got)
	}
	cwdGit = func() (string, string) { return "", "" }
	e.fails(4, "task", "verify-context", "1")
	if obj(e.json("resume", "-p", "p"))["context_mismatch"] != nil {
		t.Error("resume outside git should not report a mismatch")
	}
}
