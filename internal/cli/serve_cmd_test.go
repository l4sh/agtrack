package cli

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// runResult is one in-process `agtrack` run that may block (a server).
type runResult struct {
	out, err string
	code     int
}

// goRun starts e.run in the background; the channel yields its result.
func (e *env) goRun(args ...string) <-chan runResult {
	ch := make(chan runResult, 1)
	go func() {
		out, errs, code := e.run(args...)
		ch <- runResult{out, errs, code}
	}()
	return ch
}

func waitRun(t *testing.T, ch <-chan runResult, within time.Duration, what string) runResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(within):
		t.Fatalf("%s did not return within %s", what, within)
	}
	return runResult{}
}

// waitRecord polls the serve record until ok accepts it.
func waitRecord(t *testing.T, path string, ok func(*serveRecord) bool, what string) *serveRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			var r serveRecord
			if json.Unmarshal(data, &r) == nil && ok(&r) {
				return &r
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("serve record: %s", what)
	return nil
}

// T2, T3: `agtrack serve` end to end, through Run, with real servers. Every
// server here runs in this process, so the recorded pid is the test's own:
// --restart must stop the server through POST /api/shutdown, never a signal.
func TestServeCommandEndToEnd(t *testing.T) {
	e := newEnv(t)
	seedBrowse(e)
	e.actor = "agent1"
	path := serveRecordPath(e.db)

	first := e.goRun("serve", "--idle", "1m")
	rec := waitRecord(t, path, func(r *serveRecord) bool { return r.Port > 0 }, "never written")
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("record mode: %v %v", st, err)
	}
	if rec.PID != os.Getpid() || rec.As != "human" || rec.Token == "" || rec.Root != cwdRoot() || rec.DB == "" {
		t.Fatalf("record: %+v", rec)
	}
	// T3: the record checks out against the real server.
	if live := liveServeRecord(path); live == nil || live.Port != rec.Port {
		t.Fatalf("live record not recognised: %+v", live)
	}
	base := rec.base()
	if r := call(t, "GET", base+"/api/ping?t="+url.QueryEscape(rec.Token), "", "", ""); r.code != 403 {
		t.Errorf("ping with ?t= and no header: %d", r.code)
	}
	if r := call(t, "GET", base+"/api/ping", rec.Token, "", ""); r.code != 200 || !strings.Contains(r.body, `"mode":"browse"`) {
		t.Errorf("ping: %d %s", r.code, r.body)
	}

	// Same --as: the running server's link, and no second server.
	out := e.ok("serve")
	if !strings.Contains(out, "already running at "+base+"/?t="+rec.Token) {
		t.Errorf("reuse: %q", out)
	}

	// A different --as gets its own, unrecorded server.
	other := e.goRun("serve", "--as", "bob", "--idle", "1s")
	r := waitRun(t, other, 10*time.Second, "serve --as bob")
	if r.code != 0 || !strings.Contains(r.err, "runs as human; starting another for bob") || !strings.Contains(r.out, "serving agtrack at") {
		t.Errorf("--as bob: %+v", r)
	}
	// --new starts one more regardless, also unrecorded.
	r = waitRun(t, e.goRun("serve", "--new", "--idle", "1s"), 10*time.Second, "serve --new")
	if r.code != 0 || !strings.Contains(r.out, "serving agtrack at") || strings.Contains(r.out, base) {
		t.Errorf("--new: %+v", r)
	}
	if now := liveServeRecord(path); now == nil || now.Port != rec.Port {
		t.Fatalf("another server replaced the record: %+v", now)
	}

	// --restart stops the recorded server (by request) and takes its place.
	restarted := e.goRun("serve", "--restart", "--idle", "1s")
	r = waitRun(t, first, 10*time.Second, "the restarted server")
	if r.code != 0 || !strings.Contains(r.out, "shutdown requested") {
		t.Errorf("first server: %+v", r)
	}
	next := waitRecord(t, path, func(x *serveRecord) bool { return x.Port > 0 && x.Port != rec.Port }, "not replaced by --restart")
	if next.Token == rec.Token {
		t.Error("the restarted server kept the old token")
	}
	// The idle exit removes the record.
	r = waitRun(t, restarted, 10*time.Second, "serve --restart")
	if r.code != 0 || !strings.Contains(r.out, "idle for") {
		t.Errorf("restarted server: %+v", r)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("record kept after the idle exit: %v", err)
	}
}

var linkRE = regexp.MustCompile(`http://127\.0\.0\.1:\d+/s/[^?\s]+\?t=\S+`)

// T4: `spec serve` hands out a deep link on a running browse server, and
// reuses it only under R7's root rule.
func TestSpecServeReuse(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	e.actor = "agent1"
	path := serveRecordPath(e.db)
	server := e.goRun("serve", "--idle", "1m")
	rec := waitRecord(t, path, func(r *serveRecord) bool { return r.Port > 0 }, "never written")
	t.Cleanup(func() {
		requestShutdown(rec)
		waitRun(t, server, 10*time.Second, "agtrack serve")
	})
	if rec.Root != cwdRoot() {
		t.Fatalf("record root %q, cwd root %q", rec.Root, cwdRoot())
	}

	// From the server's own checkout: reused, and the link opens that spec.
	out := e.ok("spec", "serve", "auth")
	link := linkRE.FindString(out)
	if !strings.Contains(out, "reusing agtrack serve") || !strings.HasPrefix(link, rec.base()+"/s/w%2Forbiter%2Fauth?t=") {
		t.Fatalf("reuse: %q", out)
	}
	if r := call(t, "GET", link, "", "", ""); r.code != 200 || !strings.Contains(r.body, "app.js") {
		t.Errorf("deep link: %d", r.code)
	}
	model := call(t, "GET", rec.base()+"/api/spec/w%2Forbiter%2Fauth", rec.Token, "", "")
	var m struct {
		Spec struct {
			Slug string `json:"slug"`
		} `json:"spec"`
	}
	if model.code != 200 || json.Unmarshal([]byte(model.body), &m) != nil || m.Spec.Slug != "auth" {
		t.Errorf("spec model: %d %.200s", model.code, model.body)
	}

	// Outside any repository the spec's file is not in "this checkout": reused.
	t.Chdir(t.TempDir())
	if out := e.ok("spec", "serve", "auth"); !strings.Contains(out, "reusing agtrack serve") {
		t.Errorf("from outside a repo: %q", out)
	}

	// From another repository that does not hold the spec: reused.
	elsewhere := t.TempDir()
	git(t, elsewhere, "init", "-q")
	t.Chdir(elsewhere)
	if out := e.ok("spec", "serve", "auth"); !strings.Contains(out, "reusing agtrack serve") {
		t.Errorf("from an unrelated repo: %q", out)
	}

	// From the checkout that holds the spec's file, which is not the server's:
	// its own viewer, and one line saying why.
	t.Chdir(filepath.Join(e.repo, "specs"))
	r := waitRun(t, e.goRun("spec", "serve", "auth", "--idle", "1s"), 10*time.Second, "spec serve in the spec's checkout")
	if r.code != 0 || strings.Contains(r.out, "reusing") || !strings.Contains(r.out, "serving auth at") ||
		!strings.Contains(r.err, "specs/auth.md is in this checkout") || !strings.Contains(r.err, "starting a viewer here") {
		t.Errorf("own viewer: %+v", r)
	}
}
