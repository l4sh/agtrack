package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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
	// Each row carries the actor of its latest event (one aggregate query).
	for _, x := range tasks {
		if tk := x.(map[string]any); tk["last_actor"] != "agent1" || tk["last_event_at"] == nil {
			t.Errorf("task %v last_actor=%v last_event_at=%v", tk["id"], tk["last_actor"], tk["last_event_at"])
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
	// R8: limit is clamped to 1..1000; above that, or not a number, is a 400.
	for q, want := range map[string]int{"1000": 200, "0": 200, "-5": 200, "1001": 400, "lots": 400} {
		if r := get(t, base+"log?limit="+q); r.code != want {
			t.Errorf("log limit=%s: %d, want %d: %s", q, r.code, want, r.body)
		}
	}
	if r := get(t, base+"log?limit=0").json(t); len(r["events"].([]any)) != 1 {
		t.Errorf("limit=0 is not clamped to 1: %v", r["events"])
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
	if r := post(t, base+"steps/3.1/title", `{"title":"eins","expected":"one"}`); r.code != 409 || r.json(t)["current"] != "uno" {
		t.Fatalf("stale step title: %d %s", r.code, r.body)
	}
	if r := post(t, base+"tasks/3/title", `{"title":"Renamed again"}`); r.code != 400 {
		t.Errorf("title without expected: %d %s", r.code, r.body)
	}

	// Tags: an unknown tag to remove fails the whole change before anything
	// is written, so the tag to add is not applied either.
	if r := post(t, base+"tasks/3/tags", `{"add":["fresh"],"remove":["nosuch"]}`); r.code != 404 {
		t.Errorf("unknown tag removal: %d %s", r.code, r.body)
	}
	if out := e.ok("task", "show", "3"); strings.Contains(out, "fresh") {
		t.Errorf("a refused tag change wrote its additions:\n%s", out)
	}

	// A browser write makes the viewer the row's latest actor.
	if r := post(t, base+"tasks/2/notes", `{"body":"seen"}`); r.code != 200 {
		t.Fatalf("note: %d %s", r.code, r.body)
	}
	for _, x := range get(t, base+"projects/acme%2Forbiter/tasks").json(t)["tasks"].([]any) {
		tk := x.(map[string]any)
		if want := map[float64]string{1: "agent1", 2: "human"}[tk["id"].(float64)]; want != "" && tk["last_actor"] != want {
			t.Errorf("task %v last_actor=%v want %s", tk["id"], tk["last_actor"], want)
		}
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
		{"steps/3.1/notes", `{"kind":"blocker","body":"step note","idem":"sn-1"}`},
		{"steps/3.1/notes", `{"kind":"blocker","body":"step note","idem":"sn-1"}`},
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
	// A step's notes ride on the step; the idem replay wrote one.
	steps := get(t, base+"tasks/3").json(t)["task"].(map[string]any)["subtask_list"].([]any)
	sn := steps[0].(map[string]any)["note_list"].([]any)
	if len(sn) != 1 || sn[0].(map[string]any)["kind"] != "blocker" || sn[0].(map[string]any)["actor"] != "human" {
		t.Errorf("step notes: %v", sn)
	}
	if bad := post(t, base+"steps/3.1/notes", `{"kind":"bogus","body":"x"}`); bad.code != 400 {
		t.Errorf("bad step note kind: %d", bad.code)
	}
	if bad := post(t, base+"steps/3.9/notes", `{"body":"x"}`); bad.code != 404 {
		t.Errorf("missing step: %d", bad.code)
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

// T6: the browse-mode gates: Host on the page and the API, no ?t= on the
// API, and the session cookie from the first /?t= load working on its own.
func TestServeBrowseGates(t *testing.T) {
	e := newEnv(t)
	seedBrowse(e)
	base := browseServer(t, e)
	withHost := func(method, url, host, token string) int {
		req, _ := http.NewRequest(method, url, nil)
		req.Host = host
		if token != "" {
			req.Header.Set("X-Agtrack-Token", token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := withHost("GET", base+"/?t=tok", "evil.example:80", ""); c != 403 {
		t.Errorf("bad Host on /: %d", c)
	}
	if c := withHost("GET", base+"/api/browse/contexts", "evil.example:80", "tok"); c != 403 {
		t.Errorf("bad Host on /api/browse/: %d", c)
	}
	if r := call(t, "GET", base+"/api/browse/contexts?t=tok", "", "", ""); r.code != 403 {
		t.Errorf("?t= on /api/browse/: %d", r.code)
	}

	// The first load sets the cookie; afterwards it alone opens the page and the API.
	resp, err := http.Get(base + "/?t=tok")
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
	if resp.StatusCode != 200 || cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("first load: %d cookie %+v", resp.StatusCode, cookie)
	}
	withCookie := func(method, url, ctype, body string) int {
		req, _ := http.NewRequest(method, url, strings.NewReader(body))
		req.AddCookie(cookie)
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := withCookie("GET", base+"/", "", ""); c != 200 {
		t.Errorf("page with the cookie alone: %d", c)
	}
	if c := withCookie("GET", base+"/api/browse/contexts", "", ""); c != 200 {
		t.Errorf("API read with the cookie alone: %d", c)
	}
	if c := withCookie("POST", base+"/api/browse/tasks/1/notes", "application/json", `{"body":"by cookie"}`); c != 200 {
		t.Errorf("API write with the cookie alone: %d", c)
	}
	if c := withCookie("POST", base+"/api/browse/tasks/1/notes", "text/plain", `{"body":"x"}`); c != 415 {
		t.Errorf("cookie write without JSON: %d", c)
	}
}

// T1: browser text that looks like a flag is data. It is stored verbatim on
// the target the URL names (tags as their slug, as the CLI stores any tag),
// and never redirects the write or changes how it runs.
func TestServeFlagLikeInput(t *testing.T) {
	e := newEnv(t)
	seedBrowse(e)
	e.ok("project", "add", "decoy")
	base := browseServer(t, e) + "/api/browse/"
	const flagA, flagB = "--project=acme/decoy", "-n"
	enc := func(s string) string { b, _ := json.Marshal(s); return string(b) }

	add := post(t, base+"projects/acme%2Forbiter/tasks", `{"title":`+enc(flagA)+`,"tags":[`+enc(flagB)+`,`+enc(flagA)+`],"note":`+enc(flagB)+`,"steps":[`+enc(flagA)+`,`+enc(flagB)+`]}`)
	if add.code != 200 {
		t.Fatalf("add: %d %s", add.code, add.body)
	}
	id := fmt.Sprint(int(add.json(t)["id"].(float64)))
	for _, w := range []struct{ path, body string }{
		{"tasks/" + id + "/steps", `{"steps":[` + enc("--status=done") + `]}`},
		{"tasks/" + id + "/notes", `{"kind":"question","body":` + enc(flagA) + `}`},
		{"tasks/" + id + "/log", `{"body":` + enc(flagB) + `}`},
		{"projects/acme%2Forbiter/log", `{"body":` + enc("--task=1") + `}`},
		{"projects/acme%2Forbiter/notes", `{"body":` + enc("--context=home") + `}`},
		{"steps/" + id + ".1/notes", `{"body":` + enc("--kind=blocker") + `}`},
		{"tasks/" + id + "/tags", `{"add":[` + enc("--force") + `]}`},
	} {
		if r := post(t, base+w.path, w.body); r.code != 200 {
			t.Fatalf("%s: %d %s", w.path, r.code, r.body)
		}
	}
	if r := post(t, base+"steps/"+id+".2/title", `{"title":`+enc("--title=x")+`,"expected":`+enc(flagB)+`}`); r.code != 200 {
		t.Fatalf("step title: %d %s", r.code, r.body)
	}

	task := e.json("task", "show", id, "-x").(map[string]any)
	if task["title"] != flagA || task["project"] != "orbiter" {
		t.Errorf("task: title %q project %v", task["title"], task["project"])
	}
	var tags []string
	for _, x := range task["tag_list"].([]any) {
		tags = append(tags, x.(string))
	}
	if strings.Join(tags, ",") != "force,n,project-acme-decoy" {
		t.Errorf("tags: %v", tags)
	}
	var steps []string
	for _, x := range task["subtask_list"].([]any) {
		st := x.(map[string]any)
		steps = append(steps, st["title"].(string)+"|"+st["status"].(string))
	}
	if want := []string{flagA + "|todo", "--title=x|todo", "--status=done|todo"}; strings.Join(steps, ";") != strings.Join(want, ";") {
		t.Errorf("steps: %q", steps)
	}
	var bodies []string
	for _, x := range task["note_list"].([]any) {
		n := x.(map[string]any)
		kind, _ := n["kind"].(string) // a plain note has no kind
		if kind == "" {
			kind = "note"
		}
		bodies = append(bodies, kind+":"+n["body"].(string))
	}
	if strings.Join(bodies, ";") != "note:"+flagB+";question:"+flagA {
		t.Errorf("task notes: %q", bodies)
	}
	sn := e.json("sub", "show", id+".1").(map[string]any)["note_list"].([]any)
	if len(sn) != 1 || sn[0].(map[string]any)["body"] != "--kind=blocker" || sn[0].(map[string]any)["kind"] == "blocker" {
		t.Errorf("step note: %v", sn)
	}
	logs := e.ok("log", "-p", "orbiter", "--action", "log", "-f", "json")
	if !strings.Contains(logs, `"`+flagB+`"`) || !strings.Contains(logs, `"--task=1"`) {
		t.Errorf("log entries:\n%s", logs)
	}
	if n := strings.TrimSpace(e.ok("task", "ls", "-p", "decoy", "--status", "all", "--count")); n != "0" {
		t.Errorf("the decoy project got %s tasks", n)
	}
	if out := e.ok("project", "show", "orbiter", "-f", "json"); !strings.Contains(out, `"--context=home"`) {
		t.Errorf("project note:\n%s", out)
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
	if r := call(t, "GET", base+"/?t=tok", "", "", ""); r.code != 200 || !strings.Contains(r.body, "browse.js") {
		t.Errorf("browse page: %d", r.code)
	}
	if r := call(t, "GET", base+"/", "", "", ""); r.code != 403 {
		t.Errorf("browse page without token: %d", r.code)
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

// Rules and lessons views: reads, approver actions, and the gate for a
// viewer who is not an approver.
func TestServeRulesAndLessons(t *testing.T) {
	e := newEnv(t)
	seedBrowse(e)
	e.ok("rule", "add", "-g", "deny", "push", "--quote", "never push")
	e.ok("rule", "propose", "-p", "acme/orbiter", "allow", "push", "--quote", "ok on feature branches")
	e.ok("lesson", "add", "-g", "Gotcha <b>one</b>", "--body", "see https://example.com/x", "--path", "src/**")
	base := browseServer(t, e) + "/api/browse/"

	r := get(t, base+"rules?scope=global")
	if r.code != 200 {
		t.Fatalf("rules: %d %s", r.code, r.body)
	}
	j := r.json(t)
	if j["approver"] != true || j["proposed_total"].(float64) != 1 || len(j["rules"].([]any)) != 1 {
		t.Fatalf("global rules: %s", r.body)
	}
	if r := get(t, base+"rules?scope=project&ref=acme%2Forbiter&all=1"); r.code != 200 || len(r.json(t)["rules"].([]any)) != 2 {
		t.Fatalf("project rules (effective + proposed): %d %s", r.code, r.body)
	}
	if r := get(t, base+"rules?scope=proposed"); r.code != 200 || len(r.json(t)["rules"].([]any)) != 1 {
		t.Fatalf("proposed: %d %s", r.code, r.body)
	}
	if r := get(t, base+"rules?scope=task"); r.code != 400 {
		t.Errorf("task scope without ref: %d %s", r.code, r.body)
	}
	if r := get(t, base+"rules?scope=bogus"); r.code != 400 {
		t.Errorf("bogus scope: %d", r.code)
	}
	if r := get(t, base+"rules?scope=task&ref=1"); r.code != 200 {
		t.Errorf("task rules: %d %s", r.code, r.body)
	}

	// The approver approves; revoking needs a note.
	if r := post(t, base+"rules/2/approve", `{"note":"fine"}`); r.code != 200 || r.json(t)["status"] != "active" {
		t.Fatalf("approve: %d %s", r.code, r.body)
	}
	if r := post(t, base+"rules/1/revoke", `{}`); r.code != 400 {
		t.Errorf("revoke without note: %d %s", r.code, r.body)
	}
	if r := post(t, base+"rules/1/revoke", `{"note":"changed my mind"}`); r.code != 200 || r.json(t)["status"] != "revoked" {
		t.Fatalf("revoke: %d %s", r.code, r.body)
	}
	// Add: a deny needs the human's words.
	if r := post(t, base+"rules", `{"scope":"global","kind":"deny","verb":"delete"}`); r.code != 400 {
		t.Errorf("deny without quote: %d %s", r.code, r.body)
	}
	add := post(t, base+"rules", `{"scope":"project","scope_ref":"acme/orbiter","kind":"deny","verb":"deploy","text":"not on fridays","quote":"no friday deploys","hard":true}`)
	if add.code != 200 || add.json(t)["status"] != "active" || add.json(t)["hard"] != true || add.json(t)["scope"] != "acme/orbiter" {
		t.Fatalf("add: %d %s", add.code, add.body)
	}
	if r := post(t, base+"rules", `{"scope":"global","kind":"require","verb":"tests-pass","propose":true}`); r.code != 200 || r.json(t)["status"] != "proposed" {
		t.Errorf("propose: %d %s", r.code, r.body)
	}

	// Lessons: bodies come back as plain text; add, verify, retire.
	l := get(t, base+"lessons?scope=global")
	if l.code != 200 || !strings.Contains(l.body, "see https://example.com/x") {
		t.Fatalf("lessons: %d %s", l.code, l.body)
	}
	la := post(t, base+"lessons", `{"scope":"project","scope_ref":"acme/orbiter","title":"Use pnpm","body":"not npm","kind":"fix","paths":["package.json"],"cmds":["pnpm install"]}`)
	if la.code != 200 || la.json(t)["title"] != "Use pnpm" {
		t.Fatalf("lesson add: %d %s", la.code, la.body)
	}
	if r := post(t, base+"lessons/1/verify", `{"note":"checked today"}`); r.code != 200 {
		t.Errorf("verify: %d %s", r.code, r.body)
	}
	if r := post(t, base+"lessons/1/retire", `{}`); r.code != 400 {
		t.Errorf("retire without note: %d %s", r.code, r.body)
	}
	if r := post(t, base+"lessons/1/retire", `{"note":"fixed upstream"}`); r.code != 200 {
		t.Errorf("retire: %d %s", r.code, r.body)
	}
	if r := get(t, base+"lessons?scope=project&ref=acme%2Forbiter"); r.code != 200 || len(r.json(t)["lessons"].([]any)) != 1 {
		t.Errorf("lessons after retire: %d %s", r.code, r.body)
	}
}

func TestServeRulesGateForNonApprover(t *testing.T) {
	e := newEnv(t)
	seedBrowse(e)
	e.ok("rule", "propose", "-g", "allow", "push", "--quote", "go ahead")
	base := browseServer(t, e, "--as", "visitor") + "/api/browse/"

	if j := get(t, base+"rules?scope=global").json(t); j["approver"] != false {
		t.Fatalf("visitor is an approver: %v", j)
	}
	r := post(t, base+"rules/1/approve", `{}`)
	if r.code != 400 || !strings.Contains(r.body, "only an approver may approve") {
		t.Fatalf("non-approver approve: %d %s", r.code, r.body)
	}
	if out := e.ok("rule", "show", "1"); !strings.Contains(out, "proposed") {
		t.Errorf("rule changed:\n%s", out)
	}
	// Anyone may add a deny; an allow or hard rule is stored as proposed.
	if r := post(t, base+"rules", `{"scope":"global","kind":"deny","verb":"delete","quote":"no deletes"}`); r.code != 200 || r.json(t)["status"] != "active" {
		t.Errorf("deny: %d %s", r.code, r.body)
	}
	if r := post(t, base+"rules", `{"scope":"global","kind":"allow","verb":"merge","quote":"merge freely"}`); r.code != 200 || r.json(t)["status"] != "proposed" {
		t.Errorf("allow from a non-approver: %d %s", r.code, r.body)
	}
	if r := post(t, base+"rules", `{"scope":"global","kind":"deny","verb":"release","quote":"q","hard":true}`); r.code != 200 || r.json(t)["status"] != "proposed" {
		t.Errorf("hard from a non-approver: %d %s", r.code, r.body)
	}
}

// Continuity data on the task page: checkpoints, evidence, snooze, acks.
func TestServeTaskContinuity(t *testing.T) {
	e := newEnv(t)
	seedBrowse(e)
	e.ok("task", "checkpoint", "1", "--next", "finish build", "--body", "state <i>so far</i>")
	e.ok("task", "checkpoint", "1", "--next", "ship it", "--body", "later")
	base := browseServer(t, e) + "/api/browse/"

	tk := get(t, base+"tasks/1").json(t)
	task := tk["task"].(map[string]any)
	cp := task["checkpoint"].(map[string]any)
	if cp["next"] != "ship it" || cp["n"].(float64) != 2 || len(task["checkpoints"].([]any)) != 2 {
		t.Fatalf("checkpoint: %v / %v", cp, task["checkpoints"])
	}
	if _, ok := task["claim_last_seen"]; !ok {
		t.Errorf("claim_last_seen missing")
	}
	if get(t, base+"tasks/2").json(t)["task"].(map[string]any)["checkpoint"] != nil {
		t.Errorf("a task without a checkpoint reports one")
	}

	// Acknowledge the handoff note; it shows who and when.
	if r := post(t, base+"notes/1/ack", `{"note":"on it"}`); r.code != 200 {
		t.Fatalf("ack: %d %s", r.code, r.body)
	}
	notes := get(t, base+"tasks/1").json(t)["task"].(map[string]any)["note_list"].([]any)
	if n := notes[0].(map[string]any); n["acknowledged_by"] != "human" {
		t.Errorf("ack not recorded: %v", n)
	}

	// Snooze and unsnooze.
	if r := post(t, base+"tasks/2/snooze", `{"until":"3d","note":"later"}`); r.code != 200 {
		t.Fatalf("snooze: %d %s", r.code, r.body)
	}
	if v := get(t, base+"tasks/2").json(t)["task"].(map[string]any)["snoozed_until"]; v == nil {
		t.Errorf("snoozed_until not set")
	}
	if r := post(t, base+"tasks/2/snooze", `{"until":"garbage"}`); r.code != 400 {
		t.Errorf("bad snooze: %d %s", r.code, r.body)
	}
	if r := post(t, base+"tasks/2/unsnooze", `{}`); r.code != 200 {
		t.Fatalf("unsnooze: %d %s", r.code, r.body)
	}
	if v := get(t, base+"tasks/2").json(t)["task"].(map[string]any)["snoozed_until"]; v != nil {
		t.Errorf("snooze not cleared: %v", v)
	}

	// Evidence on a close shows on the task; the list carries stale_reason.
	e.ok("task", "done", "1", "--evidence", "test:go test ./...=0", "--evidence", "url:https://example.com/pr/1")
	task = get(t, base+"tasks/1").json(t)["task"].(map[string]any)
	if ev := task["evidence"].([]any); len(ev) != 2 {
		t.Errorf("evidence: %v", task["evidence"])
	}
	pl := get(t, base+"projects/acme%2Forbiter/tasks?status=all")
	if !strings.Contains(pl.body, "stale_reason") {
		t.Errorf("task list lacks stale_reason: %s", pl.body)
	}
}
