package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/l4sh/agtrack/internal/db"
	"github.com/l4sh/agtrack/internal/store"
	"github.com/l4sh/agtrack/internal/web"
)

// browseServer runs `agtrack serve` in-process against e's database, served
// by actor "agent1" with browser edits attributed to "human".
func browseServer(t *testing.T, e *env, extra ...string) string {
	t.Helper()
	argv := append([]string{"--db", e.db, "--actor", "agent1", "serve"}, extra...)
	res, err := NewParser().Parse(argv)
	if err != nil {
		t.Fatal(err)
	}
	c, err := newCtx(res, Streams{Out: io.Discard, Err: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	c.S = store.New(d, c.Actor)
	c.S.Context = c.Context
	o, err := c.browseOptions()
	if err != nil {
		t.Fatal(err)
	}
	o.Token = "tok"
	ready := make(chan string, 1)
	o.Ready = func(u string) { ready <- u }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- web.Serve(ctx, o) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case u := <-ready:
		return u
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("server did not start")
	}
	return ""
}

type httpResult struct {
	code int
	body string
}

func (r httpResult) json(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(r.body), &v); err != nil {
		t.Fatalf("not a json object: %v\n%s", err, r.body)
	}
	return v
}

func call(t *testing.T, method, url, token, ctype, body string) httpResult {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
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
	return httpResult{resp.StatusCode, string(b)}
}

func get(t *testing.T, url string) httpResult { return call(t, "GET", url, "tok", "", "") }
func post(t *testing.T, url, body string) httpResult {
	return call(t, "POST", url, "tok", "application/json", body)
}

func seedBrowse(e *env) {
	e.actor = "agent1"
	e.ok("ctx", "add", "acme", "--use")
	e.ok("project", "add", "orbiter", "--tag", "web")
	e.ok("project", "add", "orbiter:api")
	e.ok("task", "add", "-p", "orbiter", "Rotate tokens", "--step", "design", "--step", "build")
	e.ok("task", "add", "-p", "orbiter:api", "Rate limiter")
	e.ok("task", "block", "2", "--on", "1")
	e.ok("task", "claim", "1")
	e.ok("task", "note", "1", "--kind", "handoff", "pick up the build step")
	e.ok("ctx", "add", "home")
	e.ok("-c", "home", "project", "add", "garden")
}

func TestServeRefusesSameActor(t *testing.T) {
	e := newEnv(t)
	seedBrowse(e)
	errOut := e.fails(1, "serve", "--as", "agent1", "--idle", "1s")
	if !strings.Contains(errOut, "also the serving actor") {
		t.Errorf("same actor accepted: %q", errOut)
	}
	if errOut := e.fails(3, "-c", "nowhere", "serve", "--idle", "1s"); !strings.Contains(errOut, "nowhere") {
		t.Errorf("unknown -c: %q", errOut)
	}
}

func TestServeReadAPI(t *testing.T) {
	e := newEnv(t)
	seedBrowse(e)
	base := browseServer(t, e) + "/api/browse/"

	home := get(t, base+"contexts")
	if home.code != 200 {
		t.Fatalf("home %d %s", home.code, home.body)
	}
	h := home.json(t)
	if h["start"] != "acme" || h["viewer"] != "human" || len(h["contexts"].([]any)) != 2 {
		t.Fatalf("home: %s", home.body)
	}
	if notes := h["notes"].([]any); len(notes) != 1 || notes[0].(map[string]any)["project"] != "acme/orbiter" {
		t.Fatalf("home notes: %v", h["notes"])
	}

	if r := get(t, base+"contexts/home"); r.code != 200 || !strings.Contains(r.body, `"garden"`) {
		t.Fatalf("context: %d %s", r.code, r.body)
	}
	p := get(t, base+"projects/acme%2Forbiter%3Aapi").json(t)
	if p["path"] != "acme/orbiter:api" || len(p["parents"].([]any)) != 2 {
		t.Fatalf("project: %v", p)
	}
	tasks := get(t, base+"projects/acme%2Forbiter/tasks").json(t)["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("tasks: %v", tasks)
	}
	for _, x := range tasks {
		tk := x.(map[string]any)
		if want := tk["id"].(float64) == 2; tk["blocked"] != want {
			t.Errorf("task %v blocked=%v", tk["id"], tk["blocked"])
		}
	}
	if r := get(t, base+"projects/acme%2Forbiter/tasks?claimed_by=agent1"); strings.Count(r.body, `"title"`) != 1 {
		t.Errorf("claimed_by filter: %s", r.body)
	}
	if r := get(t, base+"projects/acme%2Forbiter/tasks?grep=limiter&direct=1"); strings.Count(r.body, `"title"`) != 0 {
		t.Errorf("direct+grep: %s", r.body)
	}
	task := get(t, base+"tasks/1").json(t)
	if task["project_path"] != "acme/orbiter" || len(task["task"].(map[string]any)["subtask_list"].([]any)) != 2 {
		t.Fatalf("task: %v", task)
	}
	if deps := task["deps"].(map[string]any)["dependents"].([]any); len(deps) != 1 {
		t.Errorf("dependents: %v", deps)
	}
	if r := get(t, base+"notes?kind=handoff&context=home"); r.body != "[]" {
		t.Errorf("notes in home: %s", r.body)
	}
	if r := get(t, base+"log?project=acme%2Forbiter&limit=2").json(t); len(r["events"].([]any)) != 2 || r["total"].(float64) < 5 {
		t.Errorf("log: %v", r)
	}
	if r := get(t, base+"tasks/99"); r.code != 404 {
		t.Errorf("missing task: %d", r.code)
	}
	if r := get(t, base+"nope"); r.code != 404 {
		t.Errorf("unknown endpoint: %d", r.code)
	}
}

func TestServeWriteAPI(t *testing.T) {
	e := newEnv(t)
	seedBrowse(e)
	base := browseServer(t, e) + "/api/browse/"

	// Security posture: token and JSON, as for the spec API.
	if r := call(t, "POST", base+"tasks/1/notes", "", "application/json", `{"body":"x"}`); r.code != 403 {
		t.Errorf("no token: %d", r.code)
	}
	if r := call(t, "POST", base+"tasks/1/notes", "tok", "application/x-www-form-urlencoded", "body=x"); r.code != 415 {
		t.Errorf("form post: %d", r.code)
	}
	if r := call(t, "POST", base+"tasks/1/notes", "tok", "text/plain", `{"body":"x"}`); r.code != 415 {
		t.Errorf("text/plain: %d", r.code)
	}

	// New task, twice with the same idem key: written once.
	add := `{"title":"From the browser","tags":["ui"],"note":"first","steps":["one","two"],"idem":"k-1"}`
	r1, r2 := post(t, base+"projects/acme%2Forbiter/tasks", add), post(t, base+"projects/acme%2Forbiter/tasks", add)
	if r1.code != 200 || r1.body != r2.body {
		t.Fatalf("add: %d %s / %s", r1.code, r1.body, r2.body)
	}
	if n := strings.TrimSpace(e.ok("task", "ls", "--grep", "From the browser", "--count")); n != "1" {
		t.Fatalf("idem replay wrote %s tasks", n)
	}
	id := r1.json(t)["id"].(float64)
	if id != 3 {
		t.Fatalf("new task id %v", id)
	}

	// Title: compare-and-set.
	stale := post(t, base+"tasks/3/title", `{"title":"Renamed","expected":"something else"}`)
	if stale.code != 409 || stale.json(t)["current"] != "From the browser" || stale.json(t)["yours"] != "Renamed" {
		t.Fatalf("stale title: %d %s", stale.code, stale.body)
	}
	if r := post(t, base+"tasks/3/title", `{"title":"Renamed","expected":"From the browser"}`); r.code != 200 {
		t.Fatalf("title: %d %s", r.code, r.body)
	}
	if r := post(t, base+"steps/3.1/title", `{"title":"uno","expected":"one"}`); r.code != 200 {
		t.Fatalf("step title: %d %s", r.code, r.body)
	}
	if r := post(t, base+"steps/3.1/title", `{"title":"eins","expected":"one"}`); r.code != 409 {
		t.Fatalf("stale step title: %d %s", r.code, r.body)
	}

	for _, w := range []struct{ path, body string }{
		{"tasks/3/tags", `{"add":["new"],"remove":["ui"]}`},
		{"tasks/3/status", `{"status":"in-progress","note":"on it"}`},
		{"tasks/3/steps", `{"steps":["three"],"idem":"s-1"}`},
		{"steps/3.2/status", `{"status":"done"}`},
		{"tasks/3/block", `{"on":"2"}`},
		{"tasks/3/unblock", `{"on":"2"}`},
		{"tasks/1/release", `{}`},
		{"tasks/3/notes", `{"kind":"question","body":"why?","idem":"n-1"}`},
		{"projects/acme%2Forbiter/notes", `{"kind":"decision","body":"use rotation","idem":"n-2"}`},
		{"notes/1/reply", `{"body":"on it","idem":"r-1"}`},
		{"notes/1/resolve", `{"rationale":"handled"}`},
		{"notes/1/reopen", `{}`},
		{"tasks/3/log", `{"body":"worked on it","idem":"l-1"}`},
		{"projects/acme%2Forbiter/log", `{"body":"project entry","idem":"l-2"}`},
	} {
		if r := post(t, base+w.path, w.body); r.code != 200 {
			t.Errorf("%s: %d %s", w.path, r.code, r.body)
		}
	}
	if r := post(t, base+"tasks/3/status", `{"status":"bogus"}`); r.code != 400 {
		t.Errorf("bad status: %d", r.code)
	}
	if r := post(t, base+"tasks/3/delete", `{}`); r.code != 404 {
		t.Errorf("delete exists: %d", r.code)
	}

	// Attribution: every browser write is human's, none the serving agent's.
	evs := e.json("log", "--since", "1h", "-n", "200").([]any)
	human := 0
	for _, x := range evs {
		ev := x.(map[string]any)
		if ev["actor"] == "human" {
			human++
		}
	}
	if human < 15 {
		t.Fatalf("only %d events by human: %v", human, evs)
	}
	if out := e.ok("task", "show", "1"); strings.Contains(out, "claimed:") {
		t.Errorf("claim not released:\n%s", out)
	}
	show := e.ok("task", "show", "3")
	for _, want := range []string{"Renamed", "new", "uno", "three", "why?", "[~]"} {
		if !strings.Contains(show, want) {
			t.Errorf("task 3 lacks %q:\n%s", want, show)
		}
	}
}

func TestServeSpecPagesAndEvents(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	e.actor = "agent1"
	base := browseServer(t, e.env)

	// Any spec resolves per request; the model carries the link back.
	r := get(t, base+"/api/spec/w%2Forbiter%2Fauth")
	if r.code != 200 || r.json(t)["serve"].(map[string]any)["project"] != "w/orbiter" {
		t.Fatalf("spec model: %d %.300s", r.code, r.body)
	}
	if r := get(t, base+"/api/spec/w%2Forbiter%2Fnope"); r.code != 404 {
		t.Errorf("unknown spec: %d", r.code)
	}
	if r := call(t, "GET", base+"/s/w%2Forbiter%2Fauth?t=tok", "", "", ""); r.code != 200 || !strings.Contains(r.body, "app.js") {
		t.Errorf("viewer page: %d", r.code)
	}

	// SSE without spec= streams every event, browser writes included.
	req, _ := http.NewRequest("GET", base+"/api/events?after=0&t=tok", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if r := post(t, base+"/api/browse/projects/w%2Forbiter/notes", `{"body":"from the browser"}`); r.code != 200 {
		t.Fatalf("note: %d %s", r.code, r.body)
	}
	sc := bufio.NewScanner(resp.Body)
	sawSpec, sawHuman := false, false
	for sc.Scan() && !(sawSpec && sawHuman) {
		line := sc.Text()
		sawSpec = sawSpec || strings.Contains(line, `"action":"spec-added"`) || strings.Contains(line, `"spec-`)
		sawHuman = sawHuman || (strings.Contains(line, `"actor":"human"`) && strings.Contains(line, "from the browser"))
	}
	if !sawSpec || !sawHuman {
		t.Fatalf("event stream: spec events %v, human note %v", sawSpec, sawHuman)
	}
}
