package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

type fake struct {
	mu       sync.Mutex
	notes    []store.SpecNoteInput
	events   []*row.Row
	diffs    [][2]int // SpecDiff calls (from, to)
	head     int64    // head revision; default 4
	approved int64    // approved revision; 0 = none
	// emptyAbove: SpecDiff from a revision above this reports no changes
	// (pin-only revisions); 0 = never.
	emptyAbove int
}

func (f *fake) SpecModel(id int64) (*row.Row, error) {
	head := f.head
	if head == 0 {
		head = 4
	}
	var approval any
	if f.approved > 0 {
		approval = row.Of("id", int64(50), "by", "alex", "revision", f.approved)
	}
	return row.Of("spec", row.Of("slug", "auth", "revision", head, "approval", approval), "sections", []*row.Row{},
		"notes", []*row.Row{}, "cursor", int64(0)), nil
}
func (f *fake) SpecDiff(id int64, from, to int) ([]*row.Row, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.diffs = append(f.diffs, [2]int{from, to})
	if f.emptyAbove > 0 && from > f.emptyAbove {
		return []*row.Row{}, nil // pin-only revisions
	}
	if from > 9 || to > 9 {
		return nil, errs.NotFound("spec", "auth", "no such revision: auth@%d", max(from, to))
	}
	return []*row.Row{row.Of("change", "changed", "slug", "tokens__rotation", "diff", "-a\n+b\n")}, nil
}
func (f *fake) SpecVersions(id int64) ([]*row.Row, error) {
	return []*row.Row{row.Of("id", int64(1), "n", int64(1)), row.Of("id", int64(2), "n", int64(2))}, nil
}
func (f *fake) SpecTasks(id int64) (*row.Row, error) { return row.Of("links", []*row.Row{}), nil }
func (f *fake) SpecAddNote(id int64, in store.SpecNoteInput) (*row.Row, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in.Section == "missing" {
		return nil, "", false, errs.NotFound("section", in.Section, "no such section: %s", in.Section)
	}
	f.notes = append(f.notes, in)
	f.events = append(f.events, row.Of("id", int64(100+len(f.events)), "action", "spec-note", "actor", "alex", "target", in.Section, "detail", in.Body))
	return row.Of("id", int64(len(f.notes)), "body", in.Body), "grace-policy", true, nil
}
func (f *fake) SpecRelease(id int64) (int, int64, error) { return 0, 1, nil }
func (f *fake) SpecApprove(id int64, note string, bn []string) (*row.Row, []*row.Row, error) {
	return row.Of("id", int64(9), "note", note), nil, nil
}
func (f *fake) SpecEventsAfter(id, after int64, limit int) ([]*row.Row, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*row.Row
	for _, e := range f.events {
		if e.Int("id") > after {
			out = append(out, e)
		}
	}
	return out, nil
}
func (f *fake) SpecNoteBelongs(id, nid int64) (bool, error) { return nid == 1, nil }
func (f *fake) ResolveNote(id int64, r string, reopen bool) (*row.Row, bool, error) {
	return row.Of("id", id), true, nil
}

func start(t *testing.T, idle time.Duration) (string, *fake, context.CancelFunc, chan error) {
	t.Helper()
	return startWith(t, &fake{}, idle)
}

func startWith(t *testing.T, f *fake, idle time.Duration) (string, *fake, context.CancelFunc, chan error) {
	t.Helper()
	ready := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, Options{Store: f, SpecID: 1, SpecRef: "orbiter:api/auth", Token: "tok", Idle: idle, Viewer: "alex",
			Ready: func(u string) { ready <- u }})
	}()
	select {
	case u := <-ready:
		return u, f, cancel, done
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}
	return "", nil, nil, nil
}

func do(t *testing.T, method, url, host, token, ctype, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if host != "" {
		req.Host = host
	}
	if token != "" {
		req.Header.Set("X-Agtrack-Token", token)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestSecurityGates(t *testing.T) {
	base, _, cancel, _ := start(t, time.Hour)
	defer cancel()
	spec := base + "/api/spec/orbiter:api%2Fauth"
	cases := []struct {
		name, method, url, host, token, ctype, body string
		want                                        int
	}{
		{"page without token", "GET", base + "/s/auth", "", "", "", "", 403},
		{"page with wrong token", "GET", base + "/s/auth?t=nope", "", "", "", "", 403},
		{"page with token", "GET", base + "/s/auth?t=tok", "", "", "", "", 200},
		{"dns rebinding host", "GET", base + "/s/auth?t=tok", "evil.example:80", "", "", "", 403},
		{"api without token", "GET", spec, "", "", "", "", 403},
		{"api with query token only", "GET", spec + "?t=tok", "", "", "", "", 403},
		{"api ok", "GET", spec, "", "tok", "", "", 200},
		{"other spec", "GET", base + "/api/spec/other", "", "tok", "", "", 404},
		{"post form content type", "POST", spec + "/note", "", "tok", "application/x-www-form-urlencoded", "body=x", 415},
		{"post note", "POST", spec + "/note", "", "tok", "application/json", `{"section":"tokens__rotation","body":"hi","quote":"60s"}`, 201},
		{"post note bad kind", "POST", spec + "/note", "", "tok", "application/json", `{"section":"x","body":"hi","kind":"approval"}`, 400},
		{"post note missing section", "POST", spec + "/note", "", "tok", "application/json", `{"section":"missing","body":"hi"}`, 404},
		{"resolve foreign note", "POST", spec + "/note/7/resolve", "", "tok", "application/json", `{}`, 404},
		{"diff", "GET", spec + "/diff", "", "tok", "", "", 200},
		{"diff without token", "GET", spec + "/diff", "", "", "", "", 403},
		{"diff bad revision", "GET", spec + "/diff?from=x", "", "tok", "", "", 400},
		{"diff unknown revision", "GET", spec + "/diff?from=1&to=12", "", "tok", "", "", 404},
		{"font asset", "GET", base + "/assets/vendor/fonts/inter-latin-wght-normal.woff2", "", "", "", "", 200},
		{"approve", "POST", spec + "/approve", "", "tok", "application/json", `{"note":"ship it"}`, 200},
		{"asset", "GET", base + "/assets/app.js", "", "", "", "", 200},
		{"dev fixtures are not served", "GET", base + "/assets/dev/fixture.json", "", "", "", "", 404},
		{"traversal", "GET", base + "/assets/../server.go", "", "", "", "", 404},
		// spec serve has no browse app: / and /api/browse/ do not exist.
		{"no browse page in spec mode", "GET", base + "/?t=tok", "", "", "", "", 404},
		{"no browse api in spec mode", "GET", base + "/api/browse/contexts", "", "tok", "", "", 404},
		{"events for another spec", "GET", base + "/api/events?spec=other&t=tok", "", "", "", "", 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, body := do(t, c.method, c.url, c.host, c.token, c.ctype, c.body)
			if got != c.want {
				t.Fatalf("status %d, want %d: %s", got, c.want, body)
			}
		})
	}
}

func TestHeaders(t *testing.T) {
	base, _, cancel, _ := start(t, time.Hour)
	defer cancel()
	resp, err := http.Get(base + "/s/auth?t=tok")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Content-Security-Policy") != CSP {
		t.Fatal("CSP missing")
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("no CORS headers, ever")
	}
}

func TestSSEDeliversNotes(t *testing.T) {
	base, _, cancel, _ := start(t, time.Hour)
	defer cancel()
	resp, err := http.Get(base + "/api/events?after=0&t=tok")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go func() {
		req, _ := http.NewRequest("POST", base+"/api/spec/orbiter:api%2Fauth/note", strings.NewReader(`{"section":"s","body":"streamed"}`))
		req.Header.Set("X-Agtrack-Token", "tok")
		req.Header.Set("Content-Type", "application/json")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	sc := bufio.NewScanner(resp.Body)
	deadline := time.After(5 * time.Second)
	lines := make(chan string)
	go func() {
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	for {
		select {
		case l := <-lines:
			if strings.HasPrefix(l, "data:") && strings.Contains(l, "streamed") {
				return
			}
		case <-deadline:
			t.Fatal("no SSE event")
		}
	}
}

func TestIdleExit(t *testing.T) {
	_, _, cancel, done := start(t, 50*time.Millisecond)
	defer cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("idle server did not exit")
	}
}

func TestRefusesNonLoopback(t *testing.T) {
	err := Serve(context.Background(), Options{Store: &fake{}, Addr: "0.0.0.0:0", SpecRef: "x"})
	if err == nil {
		t.Fatal("bound a non-loopback address")
	}
}

func TestExtensionlessAssetAndIdleWithOpenStream(t *testing.T) {
	base, _, cancel, done := start(t, 300*time.Millisecond)
	defer cancel()
	if code, _ := do(t, "GET", base+"/assets/vendor/MERMAID_VERSION", "", "", "", ""); code != 200 {
		t.Fatalf("extensionless asset: %d", code)
	}
	resp, err := http.Get(base + "/api/events?after=0&t=tok")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an open event stream kept the server alive past its idle timeout")
	}
}

// R6: reads marked X-Agtrack-Refresh (the page's live refresh) are not
// activity, while the same reads unmarked are.
func TestIdleIgnoresRefreshReads(t *testing.T) {
	poll := func(base string, refresh bool, stop <-chan struct{}) {
		for {
			select {
			case <-stop:
				return
			case <-time.After(40 * time.Millisecond):
			}
			req, _ := http.NewRequest("GET", base+"/api/spec/orbiter:api%2Fauth", nil)
			req.Header.Set("X-Agtrack-Token", "tok")
			if refresh {
				req.Header.Set("X-Agtrack-Refresh", "1")
			}
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}
	const idle = 300 * time.Millisecond

	// Unmarked reads every 40ms keep the server up well past its idle timeout.
	base, _, cancel, done := start(t, idle)
	stop := make(chan struct{})
	go poll(base, false, stop)
	select {
	case <-done:
		t.Fatal("user reads did not count as activity")
	case <-time.After(4 * idle):
	}
	close(stop)
	cancel()
	<-done

	// The same reads marked as a refresh let it time out.
	base, _, cancel, done = start(t, idle)
	defer cancel()
	stop = make(chan struct{})
	defer close(stop)
	go poll(base, true, stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh reads kept the server alive past its idle timeout")
	}
	// A marked write is still activity: only GETs are refresh reads.
	if isRefresh(&http.Request{Method: "POST", Header: http.Header{"X-Agtrack-Refresh": {"1"}}}) {
		t.Error("a POST counted as a refresh read")
	}
}

func TestModelCarriesViewer(t *testing.T) {
	base, _, cancel, _ := start(t, time.Hour)
	defer cancel()
	code, body := do(t, "GET", base+"/api/spec/orbiter:api%2Fauth", "", "tok", "", "")
	if code != 200 || !strings.Contains(body, `"viewer":{"actor":"alex"}`) {
		t.Fatalf("%d %s", code, body)
	}
}

func TestDiffDefaults(t *testing.T) {
	cases := []struct {
		name             string
		head, approved   int64
		emptyAbove       int
		query            string
		wantFrom, wantTo int
		wantCall         bool
	}{
		{"head-1 without approval", 4, 0, 0, "", 3, 4, true},
		{"from the approved revision", 4, 2, 0, "", 2, 4, true},
		{"approval at head falls back to head-1", 4, 4, 0, "", 3, 4, true},
		{"explicit range", 4, 0, 0, "?from=1&to=2", 1, 2, true},
		{"single revision: nothing to compare", 1, 0, 0, "", 0, 1, false},
		{"steps back past pin-only revisions", 5, 0, 2, "", 2, 5, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base, f, cancel, _ := startWith(t, &fake{head: c.head, approved: c.approved, emptyAbove: c.emptyAbove}, time.Hour)
			defer cancel()
			code, body := do(t, "GET", base+"/api/spec/orbiter:api%2Fauth/diff"+c.query, "", "tok", "", "")
			if code != 200 {
				t.Fatalf("%d %s", code, body)
			}
			var got struct {
				From, To, Head int
				Versions       []map[string]any
				Changes        []map[string]any
			}
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatal(err)
			}
			if got.From != c.wantFrom || got.To != c.wantTo || got.Head != int(c.head) || len(got.Versions) != 2 {
				t.Fatalf("got %+v", got)
			}
			if c.wantCall != (len(f.diffs) >= 1) {
				t.Fatalf("SpecDiff calls %v", f.diffs)
			}
			if c.wantCall && (f.diffs[len(f.diffs)-1] != [2]int{c.wantFrom, c.wantTo} || len(got.Changes) != 1) {
				t.Fatalf("calls %v changes %v", f.diffs, got.Changes)
			}
			if !c.wantCall && got.Changes == nil {
				t.Fatal("changes must be [] not null")
			}
		})
	}
}

func TestSessionCookieSurvivesReload(t *testing.T) {
	base, _, cancel, _ := start(t, time.Hour)
	defer cancel()
	resp, err := http.Get(base + "/s/auth?t=tok")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "agtrack_session" {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie missing or weak: %+v", cookie)
	}
	for _, u := range []string{base + "/s/auth", base + "/api/spec/orbiter:api%2Fauth"} {
		req, _ := http.NewRequest("GET", u, nil)
		req.AddCookie(cookie)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			t.Fatalf("%s with cookie: %d", u, r.StatusCode)
		}
	}
	req, _ := http.NewRequest("GET", base+"/s/auth", nil)
	req.AddCookie(&http.Cookie{Name: "agtrack_session", Value: "wrong"})
	r, _ := http.DefaultClient.Do(req)
	r.Body.Close()
	if r.StatusCode != 403 {
		t.Fatalf("wrong cookie accepted: %d", r.StatusCode)
	}
}
