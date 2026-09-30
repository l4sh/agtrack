package web

import (
	"bufio"
	"context"
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
	mu     sync.Mutex
	notes  []store.SpecNoteInput
	events []*row.Row
}

func (f *fake) SpecModel(id int64) (*row.Row, error) {
	return row.Of("spec", row.Of("slug", "auth"), "sections", []*row.Row{}, "notes", []*row.Row{}, "cursor", int64(0)), nil
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
	f := &fake{}
	ready := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, Options{Store: f, SpecID: 1, SpecRef: "orbiter:api/auth", Token: "tok", Idle: idle,
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
		{"approve", "POST", spec + "/approve", "", "tok", "application/json", `{"note":"ship it"}`, 200},
		{"asset", "GET", base + "/assets/app.js", "", "", "", "", 200},
		{"dev fixtures are not served", "GET", base + "/assets/dev/fixture.json", "", "", "", "", 404},
		{"traversal", "GET", base + "/assets/../server.go", "", "", "", "", 404},
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
