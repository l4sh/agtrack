package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// specEnv is an agtrack database plus a real git repository holding
// specs/auth.md (the design's example spec), committed.
type specEnv struct {
	*env
	repo string
	file string
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func newSpecEnv(t *testing.T) *specEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := &specEnv{env: newEnv(t), repo: t.TempDir()}
	e.file = filepath.Join(e.repo, "specs", "auth.md")
	src, err := os.ReadFile("../spec/testdata/example-spec.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(e.file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.file, src, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, e.repo, "init", "-q")
	git(t, e.repo, "add", ".")
	git(t, e.repo, "commit", "-qm", "init")
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "orbiter")
	e.ok("project", "use", "orbiter")
	return e
}

func (e *specEnv) read() string {
	b, err := os.ReadFile(e.file)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

// edit rewrites the spec file and commits it.
func (e *specEnv) edit(from, to string) {
	e.t.Helper()
	src := e.read()
	if !strings.Contains(src, from) {
		e.t.Fatalf("edit: %q not in file", from)
	}
	if err := os.WriteFile(e.file, []byte(strings.Replace(src, from, to, 1)), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.commit()
}

func (e *specEnv) commit() {
	git(e.t, e.repo, "add", ".")
	git(e.t, e.repo, "commit", "-qm", "edit")
}

func obj(v any) map[string]any { return v.(map[string]any) }
func list(v any) []any         { return v.([]any) }

// addedLines returns the lines b has that a does not, failing if anything
// else changed (the pin splice must be a pure one-line insertion).
func addedLines(t *testing.T, a, b string) []string {
	t.Helper()
	al, bl := strings.SplitAfter(a, "\n"), strings.SplitAfter(b, "\n")
	var added []string
	i := 0
	for _, l := range bl {
		if i < len(al) && al[i] == l {
			i++
			continue
		}
		added = append(added, l)
	}
	if i != len(al) {
		t.Fatalf("lines were changed or removed, not only added")
	}
	return added
}

func TestSpecAddLeavesFileByteIdentical(t *testing.T) {
	e := newSpecEnv(t)
	before := e.read()
	out := e.ok("spec", "add", e.file)
	if !strings.Contains(out, "auth  v1  9 sections indexed, 0 anchors written (specs/auth.md unmodified)") {
		t.Errorf("add output %q", out)
	}
	if e.read() != before {
		t.Fatal("spec add modified the file")
	}
	if st := git(t, e.repo, "status", "--porcelain"); st != "" {
		t.Fatalf("working tree dirty after add: %s", st)
	}
	toc := obj(e.json("spec", "toc", "auth"))
	var slugs []string
	for _, s := range list(toc["sections"]) {
		slugs = append(slugs, obj(s)["slug"].(string))
	}
	want := "overview tokens tokens__rotation tokens__rotation__lifetimes tokens__revocation sessions sessions__revocation sessions__idle_timeout"
	if strings.Join(slugs, " ") != want {
		t.Errorf("toc %v", slugs)
	}
	if out := e.ok("spec", "show", "auth#grace-policy"); out != "Refresh tokens rotate on every use. The previous token stays valid for a grace\nwindow of 60s to absorb in-flight requests.\n" {
		t.Errorf("show by pin %q", out)
	}
	if out := e.ok("spec", "status"); !strings.Contains(out, "clean") {
		t.Errorf("status %q", out)
	}
	ls := e.ok("spec", "ls")
	if !strings.Contains(ls, "auth") || !strings.Contains(ls, "specs/auth.md") {
		t.Errorf("ls %q", ls)
	}
	e.fails(4, "spec", "add", e.file) // same slug in the same project
	// Positional refs work and warn.
	_, errOut, code := e.run("spec", "show", "auth#2.1")
	if code != 0 || !strings.Contains(errOut, "positional") {
		t.Errorf("positional: %d %q", code, errOut)
	}
	e.fails(1, "spec", "show", "auth#revocation") // ambiguous suffix
	e.fails(3, "spec", "show", "auth#nope")
}

func TestSpecDuplicatePinsFailLoudly(t *testing.T) {
	e := newSpecEnv(t)
	e.edit("### Idle timeout", "<!-- agtrack:id grace-policy -->\n### Idle timeout")
	errOut := e.fails(1, "spec", "add", e.file)
	if !regexp.MustCompile(`duplicate pin id "grace-policy" at lines 27 and \d+`).MatchString(errOut) {
		t.Errorf("error %q", errOut)
	}
}

func TestSpecNoteChangesExactlyOneLine(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	before := e.read()
	out, errOut, code := e.run("spec", "note", "auth#tokens__revocation", "--kind", "question", "--quote", "entire family", "is this per device?")
	if code != 0 {
		t.Fatalf("note: %d %s", code, errOut)
	}
	if !strings.Contains(out, "note 1 on auth#tokens__revocation (unresolved") {
		t.Errorf("note output %q", out)
	}
	if !strings.Contains(errOut, "agtrack: pinned this section as `tokens-revocation`; specs/auth.md modified (1 line), review it") {
		t.Errorf("stderr %q", errOut)
	}
	if added := addedLines(t, before, e.read()); len(added) != 1 || added[0] != "<!-- agtrack:id tokens-revocation -->\n" {
		t.Fatalf("added %q", added)
	}
	// The pinned file is the head revision: status is clean, the pin resolves.
	if out := e.ok("spec", "status"); !strings.Contains(out, "v2  clean") {
		t.Errorf("status %q", out)
	}
	if out := e.ok("spec", "show", "auth#tokens-revocation"); !strings.HasPrefix(out, "Revoking a refresh token") {
		t.Errorf("show by new pin %q", out)
	}
	// A second note on the same section writes nothing more.
	after := e.read()
	_, errOut, _ = e.run("spec", "note", "auth#tokens-revocation", "second")
	if e.read() != after || strings.Contains(errOut, "pinned") {
		t.Errorf("second note touched the file: %q", errOut)
	}
	// A reply inherits the section.
	out = e.ok("spec", "note", "--reply", "1", "per", "device", "yes")
	if !strings.Contains(out, "reply to 1 on auth#tokens__revocation") {
		t.Errorf("reply %q", out)
	}
	notes := list(e.json("spec", "notes", "auth#tokens__revocation"))
	if len(notes) != 3 || obj(notes[1])["parent_id"].(float64) != 1 || obj(notes[0])["quote_ok"] != true {
		t.Errorf("notes %v", notes)
	}
	// Drift: the file was edited, pinning must refuse until synced.
	if err := os.WriteFile(e.file, []byte(e.read()+"\nextra\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	errOut = e.fails(4, "spec", "note", "auth#sessions__idle_timeout", "x")
	if !strings.Contains(errOut, "spec sync auth") {
		t.Errorf("drift refusal %q", errOut)
	}
}

func TestSpecRenameWithPinKeepsNotes(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	e.ok("spec", "note", "auth#tokens__rotation", "is 60s per-client?")
	e.edit("## Tokens\n", "## Token handling\n")
	e.edit("### Rotation\n", "### Rotating refresh tokens\n")
	out := e.ok("spec", "sync", "auth")
	if !strings.Contains(out, `~ renamed    tokens__rotation -> token_handling__rotating_refresh_tokens   "Rotation" -> "Rotating refresh tokens"  (pinned @grace-policy)`) {
		t.Errorf("sync report %q", out)
	}
	if !strings.Contains(out, "imported v2") {
		t.Errorf("sync summary %q", out)
	}
	notes := list(e.json("spec", "notes", "auth#grace-policy"))
	if len(notes) != 1 || obj(notes[0])["section"] != "token_handling__rotating_refresh_tokens" {
		t.Errorf("notes after rename %v", notes)
	}
	if got := e.ok("spec", "show", "auth#token_handling__rotating_refresh_tokens"); !strings.HasPrefix(got, "Refresh tokens rotate") {
		t.Errorf("new path %q", got)
	}
	// Old revisions stay readable at their old addresses.
	if got := e.ok("spec", "show", "auth@1#tokens__rotation"); !strings.HasPrefix(got, "Refresh tokens rotate") {
		t.Errorf("old revision %q", got)
	}
	// An unchanged file is a no-op.
	if out := e.ok("spec", "sync", "auth"); !strings.Contains(out, "unchanged (v2)") {
		t.Errorf("no-op sync %q", out)
	}
	diff := e.ok("spec", "diff", "auth", "v1..v2")
	if !strings.Contains(diff, `~ tokens__rotation   "Rotation" -> "Rotating refresh tokens"`) {
		t.Errorf("diff %q", diff)
	}
}

func TestSpecSyncRefusesDirtyFile(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	if err := os.WriteFile(e.file, []byte(strings.Replace(e.read(), "14 days", "7 days", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := e.ok("spec", "status"); !strings.Contains(out, "drift: file is ahead (uncommitted)") {
		t.Errorf("status %q", out)
	}
	e.fails(4, "spec", "sync", "auth")
	dry := e.ok("spec", "sync", "auth", "--dry-run")
	if !strings.Contains(dry, "dry run: would import v2") {
		t.Errorf("dry run %q", dry)
	}
	if out := e.ok("spec", "sync", "auth", "--force"); !strings.Contains(out, "imported v2") {
		t.Errorf("forced %q", out)
	}
	e.commit()
	if err := os.Remove(e.file); err != nil {
		t.Fatal(err)
	}
	if out := e.ok("spec", "status"); !strings.Contains(out, "missing: file not found") {
		t.Errorf("status missing %q", out)
	}
}

func TestSpecRetirementAndRelink(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	e.ok("task", "add", "Family revocation")
	e.ok("spec", "link", "auth#tokens__revocation", "1")
	e.ok("spec", "note", "auth#tokens__revocation", "--kind", "question", "which family?")
	e.commit() // the pin comment
	src := e.read()
	start := strings.Index(src, "<!-- agtrack:id tokens-revocation -->")
	end := strings.Index(src, "## Sessions")
	if err := os.WriteFile(e.file, []byte(src[:start]+src[end:]), 0o644); err != nil {
		t.Fatal(err)
	}
	e.commit()
	out, errOut, code := e.run("spec", "sync", "auth")
	if code != 0 || !strings.Contains(out, "? orphaned   tokens__revocation   1 note (1 unresolved), 1 task link (#1)") {
		t.Fatalf("sync %d %q %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "1 orphaned anchor holds annotations") {
		t.Errorf("hint %q", errOut)
	}
	orph := list(e.json("spec", "orphans", "auth"))
	if len(orph) != 1 || obj(orph[0])["anchor"] != "tokens-revocation" {
		t.Errorf("orphans %v", orph)
	}
	// A retired ref reads informatively, with exit 3.
	out, _, code = e.run("spec", "show", "auth#tokens-revocation")
	if code != 3 || !strings.Contains(out, "state: orphaned (last seen v2)") || !strings.Contains(out, "tasks: #1") {
		t.Errorf("retired show %d %q", code, out)
	}
	// --strict refuses a sync that would orphan more.
	e.edit("### Idle timeout\n\nSessions idle for 14 days are closed by a nightly job.\n", "")
	e.ok("spec", "sync", "auth")
	e.ok("spec", "link", "auth#sessions", "1") // pins sessions (file modified), commit it
	e.commit()
	e.edit("## Sessions\n", "## Session records\n")
	e.edit("A session is the server-side record a token family points at.", "Totally different words now.")
	src = e.read()
	if err := os.WriteFile(e.file, []byte(strings.Replace(src, "<!-- agtrack:id sessions -->\n", "", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	e.commit()
	e.fails(4, "spec", "sync", "auth", "--strict")
	// Explicit relink: the note and the task link move; the old anchor retires.
	out = e.ok("spec", "relink", "auth#tokens-revocation", "--into", "tokens__rotation", "--kind", "merged")
	if !strings.Contains(out, "moved 1 note and 1 task link onto auth#grace-policy") {
		t.Errorf("relink %q", out)
	}
	if notes := list(e.json("spec", "notes", "auth#grace-policy")); len(notes) != 1 {
		t.Errorf("notes moved %v", notes)
	}
	out, _, _ = e.run("spec", "show", "auth#tokens-revocation")
	if !strings.Contains(out, "state: retired") || !strings.Contains(out, "successors: grace-policy (merged)") {
		t.Errorf("after relink %q", out)
	}
	// Old revisions keep the content.
	if got := e.ok("spec", "show", "auth@1#tokens__revocation"); !strings.HasPrefix(got, "Revoking a refresh token") {
		t.Errorf("old content %q", got)
	}
}

func TestSpecImpact(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	e.ok("task", "add", "Rotate refresh tokens", "--spec", "auth#tokens__rotation")
	e.ok("task", "done", "1")
	staleOf := func() any {
		rows := list(e.json("spec", "stale", "auth"))
		return obj(rows[0])["stale"]
	}
	// Reordering siblings changes nothing.
	src := e.read()
	rot := src[strings.Index(src, "<!-- agtrack:id grace-policy -->"):strings.Index(src, "### Revocation")]
	rev := src[strings.Index(src, "### Revocation"):strings.Index(src, "## Sessions")]
	src = strings.Replace(src, rot+rev, rev+rot, 1)
	if err := os.WriteFile(e.file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	e.commit()
	e.ok("spec", "sync", "auth")
	if staleOf() != false {
		t.Error("sibling reorder made the link stale")
	}
	// An ancestor heading change invalidates the baseline and emits spec-drift.
	e.edit("## Tokens\n", "## Refresh tokens\n")
	e.ok("spec", "sync", "auth")
	if staleOf() != true {
		t.Error("ancestor change not stale")
	}
	if out := e.ok("spec", "stale", "auth"); !regexp.MustCompile(`#1 +done +refresh_tokens__rotation  agreed v1 \(....…\)  head v3 \(....…\)  changed`).MatchString(out) {
		t.Errorf("stale text %q", out)
	}
	if log := e.ok("log", "-t", "1", "--action", "spec-drift"); !strings.Contains(log, "auth#grace-policy changed in v3 (agreed v1)") {
		t.Errorf("drift event %q", log)
	}
	// A reviewed still-valid verdict re-stamps.
	e.ok("task", "review-spec", "1", "auth#grace-policy", "--reason", "still fine")
	if staleOf() != false {
		t.Error("review did not re-stamp")
	}
	// Subtree links see descendant changes; self links do not.
	e.ok("task", "add", "Tokens overall")
	e.ok("spec", "link", "auth#refresh_tokens", "2", "--subtree")
	e.ok("spec", "link", "auth#refresh_tokens", "2", "--kind", "documents")
	e.commit()
	e.edit("Revoking a refresh token revokes its entire family.", "Revoking a refresh token revokes its whole family.")
	e.ok("spec", "sync", "auth")
	var sub, self any
	for _, r := range list(e.json("spec", "stale", "auth")) {
		m := obj(r)
		if m["task_id"].(float64) == 2 {
			if m["scope"] == "subtree" {
				sub = m["stale"]
			} else {
				self = m["stale"]
			}
		}
	}
	if sub != true || self != false {
		t.Errorf("subtree stale %v, self stale %v", sub, self)
	}
	// Reopen: done tasks with stale links go back to todo.
	e.ok("task", "done", "2")
	e.edit("Token lifetimes are global", "Token lifetimes are per-deploy")
	e.ok("spec", "sync", "auth")
	_, errOut, _ := e.run("spec", "stale", "auth", "--reopen")
	if !strings.Contains(errOut, "reopened #2") {
		t.Errorf("reopen %q", errOut)
	}
	cov := list(e.json("spec", "coverage", "auth"))
	for _, c := range cov {
		if s := obj(c)["section"]; s == "refresh_tokens" || s == "refresh_tokens__revocation" {
			t.Errorf("covered by subtree link but listed: %v", s)
		}
	}
	if len(cov) == 0 {
		t.Error("coverage lists nothing")
	}
}

func TestSpecFindHeadOnly(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	hits := list(e.json("spec", "find", "auth", "grace", "window"))
	if len(hits) == 0 || obj(hits[0])["ref"] == nil {
		t.Fatalf("find body %v", hits)
	}
	found := false
	for _, h := range hits {
		found = found || obj(h)["section"] == "tokens__rotation"
	}
	if !found {
		t.Errorf("rotation not found: %v", hits)
	}
	if hits := list(e.json("spec", "find", "auth", "idle", "timeout")); len(hits) == 0 || obj(hits[0])["section"] != "sessions__idle_timeout" {
		t.Errorf("find heading %v", hits)
	}
	e.edit("nightly job", "janitor sweep")
	e.ok("spec", "sync", "auth")
	if hits := list(e.json("spec", "find", "auth", "nightly")); len(hits) != 0 {
		t.Errorf("old revision matched: %v", hits)
	}
	if hits := list(e.json("spec", "find", "auth", "janitor")); len(hits) != 1 {
		t.Errorf("new text not found: %v", hits)
	}
	// Operators are literal by default; fts5 syntax is opt-in.
	if hits := list(e.json("spec", "find", "auth", "NOT", "OR")); len(hits) != 0 {
		t.Errorf("literal operators matched %v", hits)
	}
	e.ok("spec", "find", "auth", "grace NEAR window", "--query-syntax", "fts5")
	e.fails(1, "spec", "find", "auth", `"unbalanced`, "--query-syntax", "fts5")
	if out := e.ok("spec", "find", "auth", "janitor", "--anchors"); out != "auth#sessions__idle_timeout\n" {
		t.Errorf("anchors %q", out)
	}
}

// awaitAsync runs a wait command in the background and returns its result.
func (e *specEnv) awaitAsync(args ...string) func() (string, int) {
	var out string
	var code int
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		out, _, code = e.run(args...)
	}()
	time.Sleep(400 * time.Millisecond) // let it take its cursor
	return func() (string, int) { wg.Wait(); return out, code }
}

func TestSpecAwaitExitCodes(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	wait := []string{"--timeout", "5s", "--interval", "50ms"}

	// 11: timeout.
	out, _, code := e.run("spec", "await", "auth", "--timeout", "200ms", "--interval", "50ms")
	if code != ExitTimeout || !strings.Contains(out, "TERMINAL timeout") {
		t.Errorf("timeout: %d %q", code, out)
	}

	// 0: approval, with the build note and the comments it released.
	done := e.awaitAsync(append([]string{"spec", "await", "auth", "-f", "json"}, wait...)...)
	e.ok("spec", "note", "auth#tokens__rotation", "--quote", "grace window of 60s", "make this per-client")
	e.ok("spec", "approve", "auth", "--note", "ship it", "--build-note", "don't touch billing")
	out, code = done()
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil || code != 0 {
		t.Fatalf("approval: %d %q", code, out)
	}
	if res["result"] != "approved" || obj(res["approval"])["note"] != "ship it" || len(list(res["comments"])) != 1 || len(list(res["build_notes"])) != 1 {
		t.Errorf("approval result %v", res)
	}
	if c := obj(list(res["comments"])[0]); c["section"] != "tokens__rotation" || c["quote"] != "grace window of 60s" || c["state"] != "open" {
		t.Errorf("comment %v", c)
	}
	// Approving twice is one approval.
	if out := e.ok("spec", "approve", "auth"); !strings.HasPrefix(out, "already approved auth v1") {
		t.Errorf("double approve %q", out)
	}

	// 10: manual mode holds comments until release.
	done = e.awaitAsync(append([]string{"spec", "await", "auth", "--until", "comments"}, wait...)...)
	e.ok("spec", "note", "auth#tokens__rotation", "first thought")
	time.Sleep(300 * time.Millisecond)
	e.ok("spec", "note", "auth#tokens__rotation", "second thought")
	e.ok("spec", "release", "auth")
	out, code = done()
	if code != ExitComments || strings.Count(out, "comment ") != 2 || !strings.Contains(out, "TERMINAL comments") {
		t.Errorf("manual release: %d %q", code, out)
	}

	// 10: auto mode releases each comment as written.
	e.ok("config", "set", "review.release", "auto")
	done = e.awaitAsync(append([]string{"spec", "await", "auth", "--until", "comments"}, wait...)...)
	e.ok("spec", "note", "auth#sessions", "auto comment") // pins sessions: a pin is not a content change
	out, code = done()
	if code != ExitComments || !strings.Contains(out, "auto comment") {
		t.Errorf("auto: %d %q", code, out)
	}

	// The pin above made a new revision with the same content: the old
	// approval is not stale.
	if a := obj(obj(e.json("spec", "render", "auth"))["spec"]); a["approval_stale"] != false || a["revision"].(float64) != 2 {
		t.Errorf("approval after pin-only revision: %v", a)
	}

	// 12: the spec changes during the wait.
	e.commit()
	done = e.awaitAsync(append([]string{"spec", "await", "auth"}, wait...)...)
	e.edit("nightly job", "janitor")
	e.ok("spec", "sync", "auth")
	out, code = done()
	if code != ExitSpecChanged || !strings.Contains(out, "TERMINAL changed") {
		t.Errorf("changed: %d %q", code, out)
	}
	if a := obj(obj(e.json("spec", "render", "auth"))["spec"]); a["approval_stale"] != true {
		t.Errorf("approval should be stale: %v", a)
	}

	// 13: the database goes away.
	done = e.awaitAsync(append([]string{"spec", "await", "auth"}, wait...)...)
	for _, suf := range []string{"", "-wal", "-shm"} {
		os.Remove(e.db + suf)
	}
	out, code = done()
	if code != ExitLost || !strings.Contains(out, "TERMINAL lost") {
		t.Errorf("lost: %d %q", code, out)
	}
}

func TestSpecWatchStreams(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	done := e.awaitAsync("spec", "watch", "auth", "--timeout", "5s", "--interval", "50ms")
	e.ok("spec", "note", "auth#tokens__rotation", "is 60s per-client?")
	e.ok("spec", "note", "auth#sessions__idle_timeout", "14d too aggressive")
	e.ok("spec", "approve", "auth", "--note", "keep grace window configurable")
	out, code := done()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) < 4 || lines[len(lines)-1] != "TERMINAL approved cursor="+regexp.MustCompile(`\d+`).FindString(lines[len(lines)-1]) {
		t.Fatalf("watch: %d\n%s", code, out)
	}
	for _, want := range []string{"spec-note      -        tokens__rotation: is 60s per-client?", "sessions__idle_timeout: 14d too aggressive", "spec-approved", "revision 2; keep grace window configurable"} {
		if !strings.Contains(out, want) {
			t.Errorf("watch output lacks %q:\n%s", want, out)
		}
	}
}

func TestSpecModelAndResume(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	e.ok("task", "add", "Rotate", "--spec", "auth#grace-policy")
	e.ok("spec", "note", "auth#grace-policy", "--kind", "question", "--quote", "grace\nwindow", "per client?")
	e.ok("spec", "build-note", "auth", "don't touch billing")
	m := obj(e.json("spec", "render", "auth"))
	spec := obj(m["spec"])
	if spec["slug"] != "auth" || spec["ref"] != "orbiter/auth" || spec["release_mode"] != "manual" || spec["pending_comments"].(float64) != 1 {
		t.Errorf("spec object %v", spec)
	}
	var rot map[string]any
	for _, s := range list(m["sections"]) {
		if obj(s)["slug"] == "tokens__rotation" {
			rot = obj(s)
		}
	}
	if rot == nil || rot["pin"] != "grace-policy" || rot["open_notes"].(float64) != 1 || len(list(rot["tasks"])) != 1 || !strings.HasPrefix(rot["html"].(string), "<p>") {
		t.Errorf("section %v", rot)
	}
	notes := list(m["notes"])
	if len(notes) != 1 || obj(notes[0])["anchor"] != "grace-policy" || obj(notes[0])["released"] != false || obj(notes[0])["quote_ok"] != true {
		t.Errorf("notes %v", notes)
	}
	if bn := list(m["build_notes"]); len(bn) != 1 {
		t.Errorf("build notes %v", bn)
	}
	if fm := obj(m["front_matter"]); fm["owner"] != "alex" {
		t.Errorf("front matter %v", fm)
	}
	if !strings.Contains(e.ok("spec", "check", "auth#grace-policy"), "! 1 unresolved note") {
		t.Error("check does not show the open note")
	}
	r := obj(e.json("resume", "-t", "1"))
	specRows := list(r["spec"])
	if len(specRows) != 1 || !strings.Contains(obj(specRows[0])["state"].(string), "1 unresolved note(s)") {
		t.Errorf("resume spec %v", specRows)
	}
	review := list(r["review"])
	if len(review) != 1 || !strings.Contains(obj(review[0])["summary"].(string), "1 comment(s) awaiting release") {
		t.Errorf("resume review %v", review)
	}
}

func TestSpecPinWriteFailureRecovers(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	dir := filepath.Dir(e.file)
	before := e.read()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := e.run("spec", "note", "auth#sessions", "note survives")
	os.Chmod(dir, 0o755)
	if code != 0 || !strings.Contains(out, "note 1 on auth#sessions") || !strings.Contains(errOut, "the note is saved") {
		t.Fatalf("note: %d %q %q", code, out, errOut)
	}
	if e.read() != before {
		t.Fatal("file changed despite the failure")
	}
	// The DB write survived; sync puts the pin back rather than importing.
	if out := e.ok("spec", "sync", "auth"); !strings.Contains(out, "restored 1 pin comment to specs/auth.md (v2)") {
		t.Errorf("recovery %q", out)
	}
	if added := addedLines(t, before, e.read()); len(added) != 1 || added[0] != "<!-- agtrack:id sessions -->\n" {
		t.Errorf("restored %q", added)
	}
	if out := e.ok("spec", "status"); !strings.Contains(out, "clean") {
		t.Errorf("status %q", out)
	}
}

func TestSpecGC(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	for _, w := range []string{"one", "two", "three"} {
		e.edit("nightly job", "nightly job "+w)
		e.ok("spec", "sync", "auth")
		e.edit("nightly job "+w, "nightly job")
		e.ok("spec", "sync", "auth")
	}
	e.ok("task", "add", "t", "--spec", "auth#sessions__idle_timeout") // pins at head, referencing it
	out := e.ok("spec", "gc", "auth", "--keep", "1")
	if !strings.Contains(out, "dropped 6 revisions: v1, v2, v3, v4, v5, v6") {
		t.Errorf("gc %q", out)
	}
	e.fails(3, "spec", "show", "auth@1#sessions")
	e.ok("spec", "show", "auth@7#sessions")
}
