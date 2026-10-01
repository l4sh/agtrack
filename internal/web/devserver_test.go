//go:build devui

package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestDevServer runs a fake backend against the fixtures in assets/dev, so
// the viewer page (assets/index.html, app.js, app.css) can be exercised in a
// real browser without the real agtrack server, database or CLI. It asserts
// nothing; it blocks serving until interrupted.
//
//	go test -tags devui -run TestDevServer -timeout 0 -v ./internal/web
//
// go test's default -timeout would otherwise kill the listener after 10m,
// hence -timeout 0. Stop it with Ctrl+C.
func TestDevServer(t *testing.T) {
	doc := mustReadJSONObject(t, "assets/dev/fixture.json")
	tasks := mustReadJSONObject(t, "assets/dev/fixture-tasks.json")
	diff := mustReadJSONObject(t, "assets/dev/fixture-diff.json")

	srv := newDevServer(doc, tasks)
	srv.diff = diff

	mux := http.NewServeMux()
	mux.HandleFunc("/s/", srv.handleIndex)
	mux.Handle("/assets/", http.FileServer(http.FS(Assets)))
	mux.HandleFunc("/api/spec/", srv.handleAPI)
	mux.HandleFunc("/api/events", srv.handleEvents)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	spec, _ := doc["spec"].(map[string]any)
	slug, _ := spec["slug"].(string)
	if slug == "" {
		slug = "auth"
	}
	url := fmt.Sprintf("http://%s/s/%s?t=%s", ln.Addr().String(), slug, srv.token)

	httpSrv := &http.Server{Handler: mux}
	go func() {
		_ = httpSrv.Serve(ln)
	}()
	t.Cleanup(func() { _ = httpSrv.Close() })

	t.Logf("devui: fixture server ready\n  %s", url)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	<-ctx.Done()
}

// --- fake backend ---------------------------------------------------------

type devServer struct {
	token string

	mu       sync.Mutex
	doc      map[string]any
	tasks    map[string]any
	diff     map[string]any
	viewer   string
	nextNote int64
	cursor   int64

	subMu sync.Mutex
	subs  map[chan sseEvent]struct{}
}

type sseEvent struct {
	id     int64
	action string
	target string
	detail string
}

func newDevServer(doc, tasks map[string]any) *devServer {
	s := &devServer{
		token: mustToken(),
		doc:   doc,
		tasks: tasks,
		subs:  map[chan sseEvent]struct{}{},
	}
	s.nextNote = 1000
	s.viewer = "web:dev"
	if v, ok := doc["viewer"].(map[string]any); ok {
		if a, _ := v["actor"].(string); a != "" {
			s.viewer = a
		}
	}
	if c, ok := doc["cursor"].(float64); ok {
		s.cursor = int64(c)
	}
	return s
}

// actor is the fixture's viewer, so notes posted from the page read as the
// reviewer's own (as `spec serve --as` does for real).
// Read once at start: handlers mutate s.doc under s.mu, the SSE loop does not hold it.
func (s *devServer) actor() string { return s.viewer }

func (s *devServer) checkToken(r *http.Request) bool {
	if t := r.URL.Query().Get("t"); t != "" {
		return t == s.token
	}
	return r.Header.Get("X-Agtrack-Token") == s.token
}

func (s *devServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	f, err := Assets.Open("assets/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprint(w, "") // ensure header sent before copy below
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
}

func (s *devServer) handleAPI(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(r) {
		devErr(w, http.StatusForbidden, "forbidden", "bad token")
		return
	}
	// /api/spec/{spec}[/tasks|/diff|/note|/note/{id}/resolve|/release|/approve]
	rest := strings.TrimPrefix(r.URL.Path, "/api/spec/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		devErr(w, http.StatusNotFound, "not_found", "spec not found")
		return
	}
	sub := ""
	if len(parts) > 1 {
		sub = strings.Join(parts[1:], "/")
	}

	switch {
	case sub == "" && r.Method == http.MethodGet:
		s.mu.Lock()
		defer s.mu.Unlock()
		devJSON(w, http.StatusOK, s.doc)
	case sub == "tasks" && r.Method == http.MethodGet:
		s.mu.Lock()
		defer s.mu.Unlock()
		devJSON(w, http.StatusOK, s.tasks)
	case sub == "diff" && r.Method == http.MethodGet:
		// The fixture holds one comparison; from/to are echoed so the
		// revision picker round-trips, the changes stay the same.
		s.mu.Lock()
		defer s.mu.Unlock()
		out := map[string]any{}
		for k, v := range s.diff {
			out[k] = v
		}
		for _, k := range []string{"from", "to"} {
			if v, err := strconv.Atoi(r.URL.Query().Get(k)); err == nil {
				out[k] = v
			}
		}
		devJSON(w, http.StatusOK, out)
	case sub == "note" && r.Method == http.MethodPost:
		s.createNote(w, r)
	case strings.HasSuffix(sub, "/resolve") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(sub, "note/"), "/resolve")
		s.resolveNote(w, r, id)
	case sub == "release" && r.Method == http.MethodPost:
		s.release(w, r)
	case sub == "approve" && r.Method == http.MethodPost:
		s.approve(w, r)
	default:
		devErr(w, http.StatusNotFound, "not_found", "no such endpoint")
	}
}

type noteRequest struct {
	Section  string `json:"section"`
	Quote    string `json:"quote"`
	Prefix   string `json:"prefix"`
	Suffix   string `json:"suffix"`
	Kind     string `json:"kind"`
	Body     string `json:"body"`
	ParentID *int64 `json:"parent_id"`
}

func (s *devServer) createNote(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		devErr(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "expected application/json")
		return
	}
	var req noteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		devErr(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}
	if req.Body == "" {
		devErr(w, http.StatusBadRequest, "invalid_argument", "body is required")
		return
	}
	if req.Kind == "" {
		req.Kind = "note"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	section := req.Section
	anchor := section
	var pinned any
	sections, _ := s.doc["sections"].([]any)
	for _, raw := range sections {
		sec, _ := raw.(map[string]any)
		if sec["slug"] == section {
			if pin, _ := sec["pin"].(string); pin != "" {
				anchor = pin
			} else {
				anchor = "auto-" + strconv.FormatInt(s.nextNote, 36)
				sec["pin"] = anchor
				pinned = anchor
			}
			break
		}
	}

	if req.ParentID != nil {
		notes, _ := s.doc["notes"].([]any)
		for _, raw := range notes {
			n, _ := raw.(map[string]any)
			if numID(n["id"]) == *req.ParentID {
				section, _ = n["section"].(string)
				anchor, _ = n["anchor"].(string)
				break
			}
		}
	}

	id := s.nextNote
	s.nextNote++
	var parentID any
	if req.ParentID != nil {
		parentID = *req.ParentID
	}
	note := map[string]any{
		"id": id, "section": nullableString(section), "anchor": anchor,
		"kind": req.Kind, "body": req.Body, "actor": s.actor(),
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"quote":      nullableString(req.Quote), "quote_ok": true,
		"parent_id": parentID, "resolved_at": nil, "resolved_by": nil,
		"released": false,
	}
	notes, _ := s.doc["notes"].([]any)
	s.doc["notes"] = append(notes, note)

	if spec, ok := s.doc["spec"].(map[string]any); ok {
		pc, _ := spec["pending_comments"].(float64)
		spec["pending_comments"] = pc + 1
	}

	s.publish("spec-note", section, req.Body)
	devJSON(w, http.StatusCreated, map[string]any{"note": note, "pinned": pinned, "file_modified": true})
}

func (s *devServer) resolveNote(w http.ResponseWriter, r *http.Request, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		devErr(w, http.StatusBadRequest, "invalid_argument", "bad note id")
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	s.mu.Lock()
	defer s.mu.Unlock()
	notes, _ := s.doc["notes"].([]any)
	for _, raw := range notes {
		n, _ := raw.(map[string]any)
		if numID(n["id"]) == id {
			if n["resolved_at"] == nil {
				n["resolved_at"] = time.Now().UTC().Format(time.RFC3339)
				n["resolved_by"] = s.actor()
				s.publish("spec-note-resolved", fmt.Sprint(n["section"]), req.Note)
			}
			devJSON(w, http.StatusOK, map[string]any{"note": n})
			return
		}
	}
	devErr(w, http.StatusNotFound, "not_found", "note not found")
}

func (s *devServer) release(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	notes, _ := s.doc["notes"].([]any)
	released := 0
	for _, raw := range notes {
		n, _ := raw.(map[string]any)
		if rel, _ := n["released"].(bool); !rel {
			n["released"] = true
			released++
		}
	}
	if spec, ok := s.doc["spec"].(map[string]any); ok {
		spec["pending_comments"] = float64(0)
	}
	if released > 0 {
		s.publish("spec-released", "", fmt.Sprintf("%d comment(s)", released))
	}
	devJSON(w, http.StatusOK, map[string]any{"released": released, "cursor": s.cursor})
}

func (s *devServer) approve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Note       string   `json:"note"`
		BuildNotes []string `json:"build_notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		devErr(w, http.StatusBadRequest, "invalid_argument", err.Error())
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	spec, _ := s.doc["spec"].(map[string]any)
	revision, _ := spec["revision"].(float64)
	approval := map[string]any{
		"id": s.nextNote, "by": s.actor(), "at": time.Now().UTC().Format(time.RFC3339),
		"revision": revision, "note": req.Note,
	}
	s.nextNote++
	spec["approval"] = approval
	spec["approval_stale"] = false

	buildNotes, _ := s.doc["build_notes"].([]any)
	var added []map[string]any
	for _, body := range req.BuildNotes {
		bn := map[string]any{
			"id": s.nextNote, "body": body, "actor": s.actor(),
			"created_at": time.Now().UTC().Format(time.RFC3339),
		}
		s.nextNote++
		buildNotes = append(buildNotes, bn)
		added = append(added, bn)
	}
	s.doc["build_notes"] = buildNotes

	s.publish("spec-approved", "", req.Note)
	devJSON(w, http.StatusOK, map[string]any{"approval": approval, "build_notes": added})
}

func (s *devServer) publish(action, target, detail string) {
	s.cursor++
	ev := sseEvent{id: s.cursor, action: action, target: target, detail: detail}
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (s *devServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !s.checkToken(r) {
		devErr(w, http.StatusForbidden, "forbidden", "bad token")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := make(chan sseEvent, 8)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	defer func() {
		s.subMu.Lock()
		delete(s.subs, ch)
		s.subMu.Unlock()
	}()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			payload, _ := json.Marshal(map[string]any{
				"id": ev.id, "action": ev.action, "actor": s.actor(),
				"target": ev.target, "detail": ev.detail,
			})
			fmt.Fprintf(w, "id: %d\nevent: agtrack\ndata: %s\n\n", ev.id, payload)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, "event: ping\ndata: {}\n\n")
			flusher.Flush()
		}
	}
}

// --- helpers ---------------------------------------------------------------

func mustReadJSONObject(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := Assets.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return v
}

func mustToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func devJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func devErr(w http.ResponseWriter, status int, token, message string) {
	devJSON(w, status, map[string]any{"error": token, "message": message})
}

// numID reads a note id whether it came from the fixture (JSON float64) or
// was minted in this session (int64).
func numID(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	}
	return -1
}
