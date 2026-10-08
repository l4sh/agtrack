package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// env runs agtrack in-process against a private database.
type env struct {
	t     *testing.T
	db    string
	actor string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, db: filepath.Join(t.TempDir(), "t.db")}
	t.Setenv("AGTRACK_DB", e.db)
	t.Setenv("AGTRACK_ACTOR", "")
	t.Setenv("AGENT", "")
	t.Setenv("AGTRACK_FORMAT", "")
	// F1 must not read the checkout the tests run in; tests that want git
	// replace gitProbe themselves.
	oldProbe := gitProbe
	gitProbe = func() gitInfo { return gitInfo{} }
	t.Cleanup(func() { gitProbe = oldProbe })
	for _, k := range sessionEnv {
		t.Setenv(k, "") // a harness session must not leak into test output
	}
	return e
}

func (e *env) run(args ...string) (string, string, int) {
	var out, errb bytes.Buffer
	argv := append([]string{"--db", e.db}, args...)
	if e.actor != "" {
		argv = append([]string{"--actor", e.actor}, argv...)
	}
	code := Run(argv, Streams{Out: &out, Err: &errb})
	return out.String(), errb.String(), code
}

func (e *env) ok(args ...string) string {
	e.t.Helper()
	out, errs, code := e.run(args...)
	if code != 0 {
		e.t.Fatalf("%v: exit %d\nstdout: %s\nstderr: %s", args, code, out, errs)
	}
	return out
}

func (e *env) fails(want int, args ...string) string {
	e.t.Helper()
	out, errs, code := e.run(args...)
	if code != want {
		e.t.Fatalf("%v: exit %d, want %d\nstdout: %s\nstderr: %s", args, code, want, out, errs)
	}
	return errs
}

func (e *env) json(args ...string) any {
	e.t.Helper()
	out := e.ok(append(args, "-f", "json")...)
	var v any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		e.t.Fatalf("%v: not json: %v\n%s", args, err, out)
	}
	return v
}

// Design §3.1 / D1: `ctx use` must not leave a project pin pointing into
// the departed context.
func TestStalePinCannotMisfile(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "test")
	e.ok("ctx", "add", "acme-tools")
	e.ok("project", "add", "test/shared")
	e.ok("project", "use", "test/shared")
	if got := strings.TrimSpace(e.ok("ctx", "current")); got != "test shared" {
		t.Fatalf("ctx current = %q", got)
	}
	e.ok("ctx", "use", "acme-tools")
	if got := strings.TrimSpace(e.ok("ctx", "current")); got != "acme-tools" {
		t.Fatalf("ctx current printed a pair that does not resolve: %q", got)
	}
	errs := e.fails(1, "task", "add", "canary")
	if !strings.Contains(errs, "no project given") {
		t.Fatalf("unexpected error: %s", errs)
	}
	if n := strings.TrimSpace(e.ok("task", "ls", "-c", "test", "-s", "all", "--count")); n != "0" {
		t.Fatalf("canary landed somewhere: %s", n)
	}
}

// D2: pins are per actor.
func TestPinsArePerActor(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "work")
	e.ok("project", "add", "alpha")
	e.ok("project", "add", "beta")
	e.actor = "agent-a"
	e.ok("project", "use", "alpha")
	e.actor = "agent-b"
	e.ok("project", "use", "beta")
	e.actor = "agent-a"
	e.ok("task", "add", "from a")
	e.actor = "agent-b"
	e.ok("task", "add", "from b")
	e.actor = ""
	out := e.ok("task", "ls", "-f", "tsv", "--fields", "project,title", "--no-header")
	if !strings.Contains(out, "alpha\tfrom a") || !strings.Contains(out, "beta\tfrom b") {
		t.Fatalf("writes crossed:\n%s", out)
	}
}

// D3: --count is the total before --limit.
func TestCountIgnoresLimit(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "p", "--use")
	for i := 0; i < 5; i++ {
		e.ok("task", "add", fmt.Sprintf("t%d", i))
	}
	if n := strings.TrimSpace(e.ok("task", "ls", "--count", "-n", "2")); n != "5" {
		t.Fatalf("--count -n 2 = %s", n)
	}
	if n := strings.TrimSpace(e.ok("log", "--count", "-n", "2")); n == "2" {
		t.Fatalf("log --count honoured the limit")
	}
}

// D4: structured errors under -f json, on stderr, exit code kept.
func TestStructuredErrors(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w")
	errs := e.fails(3, "-f", "json", "task", "show", "999")
	var obj map[string]any
	if err := json.Unmarshal([]byte(errs), &obj); err != nil {
		t.Fatalf("stderr not json: %q", errs)
	}
	if obj["error"] != "not_found" || obj["ref"] != "999" || obj["code"] != float64(3) {
		t.Fatalf("bad error object: %v", obj)
	}
	errs = e.fails(2, "task", "show", "-f", "json")
	if !strings.Contains(errs, `"error":"usage"`) {
		t.Fatalf("usage error not structured: %s", errs)
	}
}

// D10 and idempotent status verbs: a repeated transition writes nothing.
func TestRepeatedTransitionIsNoop(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "p", "--use")
	e.ok("task", "add", "x")
	e.ok("task", "done", "1", "--note", "shipped")
	before := e.ok("log", "--count", "-t", "1")
	e.ok("task", "done", "1", "--note", "shipped")
	after := e.ok("log", "--count", "-t", "1")
	if before != after {
		t.Fatalf("repeat wrote events: %s -> %s", before, after)
	}
	if n := strings.TrimSpace(e.ok("task", "notes", "1", "--count")); n != "1" {
		t.Fatalf("note re-appended: %s", n)
	}
}

// Subtask cascade happens in the same transaction and only from todo.
func TestCascade(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "p", "--use")
	e.ok("task", "add", "x", "--step", "a", "--step", "b")
	e.ok("sub", "done", "1.1", "--no-cascade")
	if st := strings.TrimSpace(e.ok("task", "show", "1", "--fields", "status")); st != "status: todo" {
		t.Fatalf("--no-cascade moved the task: %s", st)
	}
	e.ok("sub", "start", "1.2")
	if st := strings.TrimSpace(e.ok("task", "show", "1", "--fields", "status")); st != "status: in-progress" {
		t.Fatalf("cascade missing: %s", st)
	}
	e.ok("sub", "done", "1.2")
	if st := strings.TrimSpace(e.ok("task", "show", "1", "--fields", "status")); st != "status: in-progress" {
		t.Fatalf("finishing every step closed the task: %s", st)
	}
}

// Design §4.4: idempotency keys.
func TestIdempotency(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "p", "--use")
	first := e.ok("task", "add", "retry me", "--idem", "k1")
	second := e.ok("task", "add", "retry me", "--idem", "k1")
	if first != second {
		t.Fatalf("retry printed differently: %q vs %q", first, second)
	}
	if n := strings.TrimSpace(e.ok("task", "ls", "--count")); n != "1" {
		t.Fatalf("retry double-wrote: %s tasks", n)
	}
	// A retry still succeeds after the world moved on (project archived).
	e.ok("project", "set", "p", "--archive")
	if again := e.ok("task", "add", "retry me", "--idem", "k1"); again != first {
		t.Fatalf("retry after change: %q", again)
	}
	errs := e.fails(4, "-f", "json", "task", "add", "something else", "--idem", "k1")
	if !strings.Contains(errs, "request_key_reused") {
		t.Fatalf("reused key not flagged: %s", errs)
	}
	e.ok("sub", "add", "1", "s1", "s2", "--idem", "k2")
	e.ok("sub", "add", "1", "s1", "s2", "--idem", "k2")
	if n := strings.TrimSpace(e.ok("sub", "ls", "1", "--count")); n != "2" {
		t.Fatalf("sub add retry double-wrote: %s", n)
	}
	e.ok("log", "add", "once", "--idem", "k3")
	e.ok("log", "add", "once", "--idem", "k3")
	if n := strings.TrimSpace(e.ok("log", "--action", "log", "--count", "--all-contexts")); n != "1" {
		t.Fatalf("log add retry double-wrote: %s", n)
	}
}

// Design §4.6: claims are advisory, expiring, and conflict on a live holder.
func TestClaims(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "p", "--use")
	e.ok("task", "add", "a")
	e.ok("task", "add", "b")
	e.actor = "claude"
	e.ok("task", "claim", "1", "--ttl", "45m")
	e.actor = "codex"
	errs := e.fails(4, "task", "claim", "1")
	if !strings.Contains(errs, "claimed by claude") {
		t.Fatalf("claim conflict text: %s", errs)
	}
	// next --claim skips the held task.
	out := e.ok("next", "--claim", "-q")
	if strings.TrimSpace(out) != "2" {
		t.Fatalf("next --claim took %q", out)
	}
	if n := strings.TrimSpace(e.ok("task", "ls", "--unclaimed", "--count")); n != "0" {
		t.Fatalf("--unclaimed = %s", n)
	}
	e.ok("task", "claim", "1", "--steal")
	e.actor = "claude"
	e.ok("task", "add", "c")
	// The claim never blocks a write.
	e.ok("task", "done", "1")
}

func TestConcurrentClaimsOneOwner(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "p", "--use")
	e.ok("task", "add", "contested")
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var out, errb bytes.Buffer
			codes[i] = Run([]string{"--db", e.db, "--actor", fmt.Sprintf("a%d", i), "task", "claim", "1"}, Streams{Out: &out, Err: &errb})
		}(i)
	}
	wg.Wait()
	winners := 0
	for _, c := range codes {
		if c == 0 {
			winners++
		} else if c != 4 {
			t.Fatalf("unexpected exit %d", c)
		}
	}
	if winners != 1 {
		t.Fatalf("%d owners", winners)
	}
}

// Design §4.8: deps with a cycle check; blocked tasks leave next.
func TestDependencies(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "p", "--use")
	e.ok("task", "add", "base", "--status", "in-progress")
	e.ok("task", "add", "top", "--status", "in-progress")
	e.ok("task", "block", "2", "--on", "1")
	e.fails(4, "task", "block", "1", "--on", "2")
	if out := e.ok("next"); strings.Contains(out, "top") {
		t.Fatalf("blocked task in next:\n%s", out)
	}
	if n := strings.TrimSpace(e.ok("task", "ls", "--ready", "--count")); n != "1" {
		t.Fatalf("--ready = %s", n)
	}
	e.ok("task", "done", "1")
	if out := e.ok("next"); !strings.Contains(out, "top") {
		t.Fatalf("unblocked task missing from next:\n%s", out)
	}
}

// D6: notes can be resolved, reopened and removed; typed notes surface.
func TestNoteLifecycle(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "p", "--use")
	e.ok("task", "add", "x", "--status", "in-progress")
	e.ok("task", "note", "1", "--kind", "question", "is 60s per-client?")
	e.ok("note", "reply", "1", "global, per spec")
	if n := strings.TrimSpace(e.ok("notes", "--count")); n != "2" {
		t.Fatalf("open notes = %s", n)
	}
	e.ok("note", "resolve", "1", "--note", "answered")
	e.ok("note", "resolve", "1") // idempotent
	if n := strings.TrimSpace(e.ok("notes", "--kind", "question", "--count")); n != "0" {
		t.Fatalf("resolved question still open")
	}
	e.ok("note", "rm", "2")
	e.fails(3, "note", "rm", "2")
}

// Structured output of a create honours the format (D11) and is lean (D12).
func TestLeanJSON(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "p", "--use")
	obj := e.json("task", "add", "lean").(map[string]any)
	for _, k := range []string{"created", "updated", "mark", "path", "context"} {
		if _, ok := obj[k]; ok {
			t.Fatalf("derived key %s in structured output", k)
		}
	}
	show := e.json("task", "show", "1").(map[string]any)
	if _, ok := show["events"]; ok {
		t.Fatal("events in json without -x")
	}
	show = e.json("task", "show", "1", "-x").(map[string]any)
	if _, ok := show["events"]; !ok {
		t.Fatal("events missing with -x")
	}
}

// A failure later in a multi-part command rolls the whole command back.
func TestCommandIsAtomic(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "p", "--use")
	e.fails(1, "task", "add", "half", "--step", "ok", "--step", "  ")
	if n := strings.TrimSpace(e.ok("task", "ls", "-s", "all", "--count")); n != "0" {
		t.Fatalf("partial create left %s task(s)", n)
	}
}

func TestResume(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "p", "--use")
	e.actor = "claude"
	e.ok("task", "add", "rotate tokens", "--step", "parse", "--step", "invalidate")
	e.ok("sub", "start", "1.1")
	e.ok("task", "claim", "1")
	e.ok("task", "note", "1", "--kind", "handoff", "grace window hardcoded at tokens.py:88")
	out := e.ok("resume", "-p", "p")
	for _, want := range []string{"#1 [~] rotate tokens", "1.1", "handoff", "grace window", "blocked   none"} {
		if !strings.Contains(out, want) && !strings.Contains(out, strings.ReplaceAll(want, "handoff", ">")) {
			t.Fatalf("resume missing %q:\n%s", want, out)
		}
	}
	if len(out) > 900 {
		t.Fatalf("resume is %d bytes", len(out))
	}
	obj := e.json("resume", "-p", "p").(map[string]any)
	if obj["task"].(map[string]any)["claimed_by"] != "claude" {
		t.Fatalf("resume json: %v", obj["task"])
	}
}

func TestWatchTimeoutAndMatch(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "p", "--use")
	out, _, code := e.run("watch", "-p", "p", "--timeout", "200ms", "--interval", "50ms")
	if code != ExitTimeout || !strings.Contains(out, "TERMINAL timeout") {
		t.Fatalf("watch timeout: %d %q", code, out)
	}
	e.ok("task", "add", "seen")
	out, _, code = e.run("watch", "-p", "p", "--after", "0", "--timeout", "2s", "--interval", "50ms", "--until", "created")
	if code != 0 || !strings.Contains(out, "EVENT") || !strings.Contains(out, "TERMINAL matched") {
		t.Fatalf("watch --until: %d %q", code, out)
	}
}

func TestGrammarAndDescribeNeedNoDatabase(t *testing.T) {
	var out, errb bytes.Buffer
	dbPath := filepath.Join(t.TempDir(), "never.db")
	if code := Run([]string{"--db", dbPath, "grammar"}, Streams{Out: &out, Err: &errb}); code != 0 {
		t.Fatal(errb.String())
	}
	if code := Run([]string{"--db", dbPath, "describe", "task", "add"}, Streams{Out: &out, Err: &errb}); code != 0 {
		t.Fatal(errb.String())
	}
	if _, err := filepath.Glob(dbPath); err != nil {
		t.Fatal(err)
	}
	if matches, _ := filepath.Glob(dbPath + "*"); len(matches) != 0 {
		t.Fatalf("grammar/describe created a database: %v", matches)
	}
}

// Review regressions (waves 0-2).
func TestExplicitContextBoundsThePin(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "work")
	e.ok("ctx", "add", "personal")
	e.ok("project", "add", "work/api")
	e.ok("project", "use", "work/api")
	e.fails(1, "-c", "personal", "task", "add", "misfile?")
	e.fails(3, "log", "add", "-p", "work/nosuch", "important")
	e.fails(3, "next", "-p", "work/nosuch")
}

func TestActorWithOwnContextIgnoresGlobalProjectPin(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w")
	e.ok("ctx", "add", "p")
	e.ok("project", "add", "w/a")
	e.ok("project", "add", "p/b")
	e.ok("project", "use", "w/a")
	e.actor = "bot"
	e.ok("project", "use", "p/b")
	e.ok("ctx", "use", "w")
	e.fails(1, "task", "add", "bot write")
}

func TestMultiPartCommandsAreAtomic(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w")
	e.fails(1, "ctx", "add", "zz", "--note", "   ")
	if strings.Contains(e.ok("ctx", "ls"), "zz") {
		t.Fatal("ctx add left a context behind")
	}
	e.ok("project", "add", "p", "--use")
	e.ok("task", "add", "a")
	e.fails(3, "task", "start", "1", "999")
	if st := strings.TrimSpace(e.ok("task", "show", "1", "--fields", "status")); st != "status: todo" {
		t.Fatalf("partial multi-ref write: %s", st)
	}
	e.fails(3, "task", "rm", "1", "998")
	e.ok("task", "show", "1")
}

func TestIdemOnEveryCreate(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w", "--idem", "c1")
	e.ok("ctx", "add", "w", "--idem", "c1")
	e.ok("project", "add", "p", "--use", "--idem", "p1")
	e.ok("project", "add", "p", "--use", "--idem", "p1")
	e.ok("task", "add", "t")
	e.ok("pr", "add", "1", "https://github.com/o/x/pull/9", "--idem", "r1")
	e.ok("pr", "add", "1", "https://github.com/o/x/pull/9", "--idem", "r1")
	e.ok("task", "note", "1", "once", "--idem", "n1")
	e.ok("task", "note", "1", "once", "--idem", "n1")
	if n := strings.TrimSpace(e.ok("task", "notes", "1", "--count")); n != "1" {
		t.Fatalf("note retry wrote %s notes", n)
	}
	e.fails(2, "task", "ls", "--idem", "x")
}

func TestConcurrentIdemRetriesAllReplay(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "p", "--use")
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var out, errb bytes.Buffer
			codes[i] = Run([]string{"--db", e.db, "task", "add", "race", "--idem", "R"}, Streams{Out: &out, Err: &errb})
		}(i)
	}
	wg.Wait()
	for _, c := range codes {
		if c != 0 {
			t.Fatalf("retry exit %d", c)
		}
	}
	if n := strings.TrimSpace(e.ok("task", "ls", "--count")); n != "1" {
		t.Fatalf("%s tasks", n)
	}
}

func TestRepoMatching(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "web", "--repo", "https://github.com/o/web")
	e.ok("project", "add", "other", "--repo", "https://github.com/o/myXrepo")
	if out := e.ok("project", "find", "--repo", "o/my_repo", "-q"); strings.TrimSpace(out) != "" {
		t.Fatalf("underscore matched as a wildcard: %q", out)
	}
	e.ok("project", "repo", "rm", "web", "o/web")
}

func TestArtifactRmIsScopedToItsTask(t *testing.T) {
	e := newEnv(t)
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "p", "--use")
	e.ok("task", "add", "a")
	e.ok("task", "add", "b")
	e.ok("task", "artifact", "1", "branch", "feat/x")
	e.fails(3, "task", "artifact", "2", "rm", "1")
}

func TestSkillCommands(t *testing.T) {
	e := newEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	out := e.ok("skill", "install", "--dry-run")
	if !strings.Contains(out, "would install") {
		t.Fatalf("dry run: %s", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude/skills/agtrack")); err == nil {
		t.Fatal("dry run wrote files")
	}
	e.ok("skill", "install")
	for _, d := range []string{".claude/skills/agtrack/SKILL.md", ".codex/skills/agtrack/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(home, d)); err != nil {
			t.Fatalf("%s missing", d)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini")); err == nil {
		t.Fatal("installed into a harness that is not present")
	}
	status := e.ok("skill", "status")
	if strings.Count(status, "current") != 2 || !strings.Contains(status, "harness not installed") {
		t.Fatalf("status:\n%s", status)
	}
	proj := t.TempDir()
	e.ok("skill", "install", "--dir", filepath.Join(proj, ".claude/skills"))
	if _, err := os.Stat(filepath.Join(proj, ".claude/skills/agtrack/SKILL.md")); err != nil {
		t.Fatal("--dir install missing")
	}
	if !strings.HasPrefix(e.ok("skill", "print"), "---\nname: agtrack") {
		t.Fatal("skill print")
	}
	e.ok("skill", "uninstall", "--agent", "codex")
	if _, err := os.Stat(filepath.Join(home, ".codex/skills/agtrack")); err == nil {
		t.Fatal("uninstall left the skill")
	}
	if matches, _ := filepath.Glob(e.db + "*"); len(matches) != 0 {
		t.Fatalf("skill commands created a database: %v", matches)
	}
}
