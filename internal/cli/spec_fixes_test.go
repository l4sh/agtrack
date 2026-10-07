package cli

// Regression tests for REVIEW-spec-web.md, one per finding (numbers match).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/l4sh/agtrack/internal/db"
	"github.com/l4sh/agtrack/internal/store"
)

// Finding 1: browser actions are attributed to --as (default "human"), never
// to the agent serving the page, so its `--actor-not self` still sees them.
func TestReviewServeAttributesToHuman(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	e.actor = "agent1"
	errOut := e.fails(1, "spec", "serve", "auth", "--as", "agent1", "--idle", "1s")
	if !strings.Contains(errOut, "also the serving actor") {
		t.Errorf("same actor accepted: %q", errOut)
	}

	p := NewParser()
	res, err := p.Parse([]string{"--db", e.db, "--actor", "agent1", "spec", "serve", "auth"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := newCtx(res, Streams{Out: os.Stdout, Err: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c.S = store.New(d, c.Actor)
	sp, err := c.S.ResolveSpec("auth", "")
	if err != nil {
		t.Fatal(err)
	}
	o, err := c.serveOptions(sp, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ws := o.Store.(*store.Store)
	if ws.Actor != "human" || ws.DB != c.S.DB {
		t.Fatalf("server store actor %q (shared db %v)", ws.Actor, ws.DB == c.S.DB)
	}

	// The agent watches with --actor-not self; the "browser" comments and approves.
	done := e.awaitAsync("spec", "watch", "auth", "--actor-not", "self", "--timeout", "5s", "--interval", "50ms")
	note, _, _, err := ws.SpecAddNote(sp.Int("id"), store.SpecNoteInput{Section: "tokens__rotation", Body: "human says: per client?"})
	if err != nil || note.Str("actor") != "human" {
		t.Fatalf("note %v %v", note, err)
	}
	if _, _, err := ws.SpecApprove(sp.Int("id"), "ship", nil); err != nil {
		t.Fatal(err)
	}
	out, code := done()
	if code != 0 || !strings.Contains(out, "human says: per client?") || !strings.Contains(out, "TERMINAL approved") {
		t.Errorf("watch as agent: %d %q", code, out)
	}
}

// Finding 2: a repeat approval with new build notes records them and
// releases comments made since; a bare repeat is a no-op.
func TestReviewReapproveKeepsBuildNotes(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	e.ok("spec", "approve", "auth", "--note", "ship")
	e.ok("spec", "note", "auth#tokens__revocation", "also revoke on password change") // held
	e.ok("spec", "approve", "auth", "--note", "ok now", "--build-note", "don't touch billing")
	m := obj(e.json("spec", "render", "auth"))
	sp := obj(m["spec"])
	if sp["pending_comments"].(float64) != 0 || len(list(m["build_notes"])) != 1 || obj(sp["approval"])["note"] != "ok now" {
		t.Errorf("after re-approval: %v build notes %v", sp, m["build_notes"])
	}
	before := e.cursor()
	if out := e.ok("spec", "approve", "auth"); !strings.HasPrefix(out, "already approved") {
		t.Errorf("bare repeat %q", out)
	}
	if e.cursor() != before {
		t.Error("bare repeat wrote events")
	}
}

// Finding 3: a stale approval is never honoured, and "changed" wins over
// "approved" in the same poll.
func TestReviewStaleApprovalNotHonoured(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	e.ok("spec", "approve", "auth", "--note", "ship")
	e.edit("nightly job", "janitor")
	e.ok("spec", "sync", "auth")
	out, _, code := e.run("spec", "await", "auth", "--after", "0", "--timeout", "600ms", "--interval", "50ms")
	if code != ExitTimeout || strings.Contains(out, "result: approved") {
		t.Errorf("replayed stale approval: %d %q", code, out)
	}
	out, _, _ = e.run("spec", "watch", "auth", "--after", "0", "--timeout", "300ms", "--interval", "50ms")
	if !strings.Contains(out, "(stale: the spec changed since)") {
		t.Errorf("stale approval not marked: %q", out)
	}
	// Same poll: an edit and an approval of the new content -> changed.
	done := e.awaitAsync("spec", "await", "auth", "--after", e.cursor(), "--timeout", "5s", "--interval", "1500ms")
	e.edit("janitor", "sweeper")
	e.ok("spec", "sync", "auth")
	e.ok("spec", "approve", "auth", "--note", "new content")
	out, code = done()
	if code != ExitSpecChanged {
		t.Errorf("changed must win: %d %q", code, out)
	}
}

// Finding 4: sync reads the spec's own repository, not whatever file sits at
// the same path in the cwd's repository; a worktree of the same repo is fine.
func TestReviewSyncIgnoresUnrelatedRepo(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(other, "specs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "specs", "auth.md"), []byte("# Other\n\n## Unrelated\n\ntext\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, other, "init", "-q")
	git(t, other, "add", ".")
	git(t, other, "commit", "-qm", "other")
	t.Chdir(other)
	if out := e.ok("spec", "sync", "auth"); !strings.Contains(out, "unchanged (v1)") {
		t.Errorf("read the unrelated repo: %q", out)
	}
	if out := e.ok("spec", "status"); !strings.Contains(out, "v1  clean") {
		t.Errorf("status %q", out)
	}
	// A worktree of the spec's repository is read.
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, e.repo, "worktree", "add", "-q", "-b", "feature", wt)
	wfile := filepath.Join(wt, "specs", "auth.md")
	src, _ := os.ReadFile(wfile)
	if err := os.WriteFile(wfile, []byte(strings.Replace(string(src), "nightly job", "janitor", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "commit", "-qam", "wt edit")
	t.Chdir(wt)
	out, errOut, code := e.run("spec", "sync", "auth")
	if code != 0 || !strings.Contains(out, "imported v2") || !strings.Contains(errOut, "(this worktree)") {
		t.Errorf("worktree sync: %d %q %q", code, out, errOut)
	}
}

// Finding 5: a quote selected in the rendered page is found.
func TestReviewRenderedQuoteIsFound(t *testing.T) {
	e := newSpecEnv(t)
	e.edit("valid for a grace\nwindow of 60s", "valid for a **grace window** of 60s, see [RFC 6749](https://x)")
	e.ok("spec", "add", e.file)
	e.ok("spec", "note", "auth#tokens__rotation", "--quote", "a grace window of 60s, see RFC 6749", "per client?")
	notes := list(e.json("spec", "notes", "auth#tokens__rotation"))
	if len(notes) != 1 || obj(notes[0])["quote_ok"] != true {
		t.Errorf("rendered quote not located: %v", notes)
	}
}

// Finding 6: a build note posted on a section reaches `spec await`.
func TestReviewSectionBuildNoteReachesAwait(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	done := e.awaitAsync("spec", "await", "auth", "--timeout", "5s", "--interval", "50ms", "-f", "json")
	e.ok("spec", "note", "auth#tokens__rotation", "--kind", "build-note", "keep the grace window configurable")
	e.ok("spec", "approve", "auth", "--note", "ok")
	out, code := done()
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil || code != 0 {
		t.Fatalf("await: %d %q", code, out)
	}
	bn := list(res["build_notes"])
	if len(bn) != 1 || obj(bn[0])["section"] != "tokens__rotation" {
		t.Errorf("build notes %v", bn)
	}
}

// Finding 7: `task add --spec` writes pins only after its transaction commits.
func TestReviewTaskAddRollbackLeavesFile(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	_, errOut, code := e.run("task", "add", "impl", "--spec", "auth#sessions", "--spec", "auth#nope")
	if code != 3 || strings.Contains(errOut, "pinned") {
		t.Errorf("rollback: %d %q", code, errOut)
	}
	if st := git(t, e.repo, "status", "--porcelain"); st != "" {
		t.Errorf("file written despite rollback: %s", st)
	}
	if out := e.ok("spec", "status"); !strings.Contains(out, "v1  clean") {
		t.Errorf("status %q", out)
	}
	// The committed path still writes, once.
	_, errOut, code = e.run("task", "add", "impl", "--spec", "auth#sessions")
	if code != 0 || !strings.Contains(errOut, "pinned this section as `sessions`; specs/auth.md modified") {
		t.Errorf("commit path: %d %q", code, errOut)
	}
}

// Finding 8 is TestSpecPinWriteFailureRecovers (spec_test.go).

// Finding 9: a note on the preamble does not report "unpinned root" forever.
func TestReviewPreambleNoteNotUnpinned(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	e.ok("spec", "note", "auth#root", "about the intro")
	for _, w := range []string{"janitor", "sweeper"} {
		e.edit("This document defines", "This document ("+w+") defines")
		out := e.ok("spec", "sync", "auth")
		if strings.Contains(out, "unpinned") {
			t.Errorf("sync reports root unpinned: %q", out)
		}
		e.edit("This document ("+w+") defines", "This document defines")
		e.ok("spec", "sync", "auth")
	}
	if notes := list(e.json("spec", "notes", "auth#root")); len(notes) != 1 {
		t.Errorf("root notes %v", notes)
	}
}

// Finding 10: orphaned notes lose quote_ok; relinked ones are re-checked.
func TestReviewOrphanedQuotes(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	e.ok("spec", "note", "auth#tokens__revocation", "--quote", "Revoking a", "q")
	e.commit()
	src := e.read()
	start := strings.Index(src, "<!-- agtrack:id tokens-revocation -->")
	end := strings.Index(src, "## Sessions")
	if err := os.WriteFile(e.file, []byte(src[:start]+src[end:]), 0o644); err != nil {
		t.Fatal(err)
	}
	e.commit()
	e.ok("spec", "sync", "auth")
	quoteOK := func() any {
		for _, n := range list(e.json("spec", "notes", "auth")) {
			if obj(n)["quote"] == "Revoking a" {
				return obj(n)["quote_ok"]
			}
		}
		return nil
	}
	if q := quoteOK(); q != false {
		t.Errorf("orphaned quote_ok %v", q)
	}
	e.ok("spec", "relink", "auth#tokens-revocation", "--into", "sessions__revocation", "--kind", "moved")
	e.commit()
	if q := quoteOK(); q != true {
		t.Errorf("relinked quote not re-checked: %v", q)
	}
}

// Finding 11: the watcher's own sync does not end its watch under --actor-not self.
func TestReviewOwnSyncDoesNotEndWatch(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	e.actor = "agent"
	done := e.awaitAsync("spec", "watch", "auth", "--actor-not", "self", "--timeout", "5s", "--interval", "50ms")
	e.edit("nightly job", "janitor")
	e.ok("spec", "sync", "auth")
	time.Sleep(300 * time.Millisecond)
	e.actor = "alex"
	e.ok("spec", "approve", "auth", "--note", "fine")
	out, code := done()
	if code != 0 || !strings.Contains(out, "TERMINAL approved") {
		t.Errorf("own sync ended the watch: %d %q", code, out)
	}
	// Without --actor-not, the same sync is a change.
	e.actor = "agent"
	done = e.awaitAsync("spec", "watch", "auth", "--timeout", "5s", "--interval", "50ms")
	e.edit("janitor", "sweeper")
	e.ok("spec", "sync", "auth")
	if _, code := done(); code != ExitSpecChanged {
		t.Errorf("unfiltered change: %d", code)
	}
}

// Finding 12: await answers at once from durable state before its cursor.
func TestReviewAwaitSeesExistingState(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	e.ok("spec", "note", "auth#sessions", "a comment")
	e.ok("spec", "release", "auth")
	out, _, code := e.run("spec", "await", "auth", "--until", "comments", "--timeout", "5s", "--interval", "50ms")
	if code != ExitComments || !strings.Contains(out, "a comment") {
		t.Errorf("existing released comment: %d %q", code, out)
	}
	e.ok("spec", "approve", "auth", "--note", "ship")
	start := time.Now()
	out, _, code = e.run("spec", "await", "auth", "--timeout", "5s", "--interval", "50ms")
	if code != 0 || !strings.Contains(out, "result: approved") || time.Since(start) > 3*time.Second {
		t.Errorf("existing approval: %d %q", code, out)
	}
}

// web-browse spec §3: resolving a spec note resolves its open replies at any
// depth, each with its own event; reopening touches the parent alone; task
// notes do not cascade.
func TestResolveSpecNoteResolvesReplies(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	e.ok("spec", "note", "auth#tokens__rotation", "per client?")
	root := obj(list(e.json("spec", "notes", "auth#tokens__rotation"))[0])["id"].(float64)
	rootID := strconv.Itoa(int(root))
	e.ok("note", "reply", rootID, "first reply")
	e.ok("note", "reply", rootID, "second reply")
	var replies []string
	for _, n := range list(e.json("spec", "notes", "auth#tokens__rotation")) {
		if id := strconv.Itoa(int(obj(n)["id"].(float64))); id != rootID {
			replies = append(replies, id)
		}
	}
	if len(replies) != 2 {
		t.Fatalf("replies %v", replies)
	}
	e.ok("note", "reply", replies[0], "nested reply")
	e.ok("note", "resolve", replies[1], "--note", "answered on its own")

	out := e.ok("note", "resolve", rootID, "--note", "settled")
	if !strings.Contains(out, "2 replies resolved with it") {
		t.Errorf("resolve output %q", out)
	}
	for _, n := range list(e.json("spec", "notes", "auth#tokens__rotation")) {
		m := obj(n)
		if m["resolved_at"] == nil {
			t.Errorf("note %v still open", m["id"])
		}
	}
	if got := obj(e.json("note", "show", replies[1]))["resolution"]; got != "answered on its own" {
		t.Errorf("earlier resolution overwritten: %v", got)
	}
	evs := 0
	for _, ev := range list(e.json("log", "-n", "50")) {
		if m := obj(ev); m["action"] == "note-resolved" && m["detail"] == "resolved with #"+rootID {
			evs++
		}
	}
	if evs != 2 {
		t.Errorf("cascade events = %d, want 2", evs)
	}

	e.ok("note", "reopen", rootID)
	open := 0
	for _, n := range list(e.json("spec", "notes", "auth#tokens__rotation")) {
		if obj(n)["resolved_at"] == nil {
			open++
		}
	}
	if open != 1 {
		t.Errorf("reopen reopened %d notes, want only the parent", open)
	}

	tid := strconv.Itoa(int(obj(e.json("task", "add", "impl"))["id"].(float64)))
	tn := strconv.Itoa(int(obj(e.json("task", "note", tid, "--kind", "question", "task question"))["id"].(float64)))
	e.ok("note", "reply", tn, "task reply")
	if out := e.ok("note", "resolve", tn); strings.Contains(out, "resolved with it") {
		t.Errorf("task note cascaded: %q", out)
	}
	if n := len(list(e.json("notes", "-t", tid))); n != 1 {
		t.Errorf("open task notes = %d, want the reply still open", n)
	}
}
