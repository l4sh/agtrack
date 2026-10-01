// Package web serves the spec viewer: a loopback-only HTTP server with a
// capability token, through which a human reviews a spec in a browser and
// comments reach the running agent (go-rewrite-spec §9).
//
// Every item of go-rewrite-spec §7.4 is a requirement here, not a nicety:
// 127.0.0.1 only, a 32-byte crypto/rand token compared in constant time,
// Host validation against DNS rebinding, no CORS, a CSP forbidding anything
// external, sanitised rendering (internal/spec.RenderHTML), and an idle
// timeout so no mutating server outlives the session. Mutations go through
// the same store, transactions and attribution as the CLI.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/render"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

// CSP forbids external script, style and connect; everything ships from embed.
// Inline *styles* are allowed because Mermaid's SVG output carries a <style>
// block; inline and external scripts stay forbidden.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
	"connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Backend is the slice of the store the viewer uses. *store.Store implements
// it; tests use a fake.
type Backend interface {
	SpecModel(specID int64) (*row.Row, error)
	SpecTasks(specID int64) (*row.Row, error)
	SpecAddNote(specID int64, in store.SpecNoteInput) (note *row.Row, pinned string, fileModified bool, err error)
	SpecRelease(specID int64) (released int, cursor int64, err error)
	SpecApprove(specID int64, note string, buildNotes []string) (approval *row.Row, buildNoteRows []*row.Row, err error)
	SpecEventsAfter(specID, after int64, limit int) ([]*row.Row, error)
	SpecNoteBelongs(specID, noteID int64) (bool, error)
	ResolveNote(id int64, rationale string, reopen bool) (*row.Row, bool, error)
	SpecDiff(specID int64, from, to int) ([]*row.Row, error)
	SpecVersions(specID int64) ([]*row.Row, error)
}

// Options configure one serving session.
type Options struct {
	Store    Backend
	SpecID   int64
	SpecRef  string
	Addr     string        // default 127.0.0.1:0
	Idle     time.Duration // default 30m
	Out      io.Writer     // where the URL line goes
	Open     bool          // try to open a browser
	RepoRoot string        // for pin write-back
	Token    string        // tests only; default: fresh random
	// Viewer is the actor browser actions are attributed to (`spec serve
	// --as`); the page uses it to tell the reviewer's notes from agents'.
	Viewer string
	// Ready, if set, receives the base URL once listening (tests).
	Ready func(url string)
}

// Server is one session.
type Server struct {
	o        Options
	mu       sync.Mutex // serialises store access: one *sql.Conn per process
	token    string
	hosts    map[string]bool
	last     time.Time
	lastMu   sync.Mutex
	clients  int
	assets   fs.FS
	stopOnce sync.Once
	stop     chan struct{}
}

// NewToken mints a capability token: 32 bytes from crypto/rand, base64url.
func NewToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// Serve listens on loopback, prints the URL and blocks until the idle
// timeout, ctx cancellation, or a fatal listener error.
func Serve(ctx context.Context, o Options) error {
	if o.Addr == "" {
		o.Addr = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(o.Addr)
	if err != nil {
		return errs.New("invalid listen address: %s", o.Addr)
	}
	if host != "127.0.0.1" {
		// Never 0.0.0.0: on a café network that difference is the whole exposure.
		return errs.New("the viewer binds 127.0.0.1 only (got %s)", host)
	}
	if o.Idle <= 0 {
		o.Idle = 30 * time.Minute
	}
	tok := o.Token
	if tok == "" {
		if tok, err = NewToken(); err != nil {
			return err
		}
	}
	sub, err := fs.Sub(Assets, "assets")
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", o.Addr)
	if err != nil {
		return errs.New("cannot listen on %s: %v", o.Addr, err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	s := &Server{o: o, token: tok, assets: sub, stop: make(chan struct{}),
		hosts: map[string]bool{fmt.Sprintf("127.0.0.1:%d", port): true, fmt.Sprintf("localhost:%d", port): true},
		last:  time.Now()}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	link := fmt.Sprintf("%s/s/%s?t=%s", base, url.PathEscape(o.SpecRef), tok)
	if o.Out != nil {
		fmt.Fprintf(o.Out, "serving %s at %s\n", o.SpecRef, link)
		fmt.Fprintf(o.Out, "idle timeout %s; comments reach agents via `agtrack spec watch|await %s`\n", o.Idle, o.SpecRef)
	}
	if o.Open {
		openBrowser(link)
	}
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	if o.Ready != nil {
		o.Ready(base)
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case err := <-errc:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ctx.Done():
			s.shutdown(srv)
			return nil
		case <-s.stop:
			s.shutdown(srv)
			return nil
		case <-tick.C:
			if s.idle() {
				if o.Out != nil {
					fmt.Fprintf(o.Out, "idle for %s; viewer stopped\n", o.Idle)
				}
				s.shutdown(srv)
				return nil
			}
		}
	}
}

func (s *Server) shutdown(srv *http.Server) {
	s.stopOnce.Do(func() { close(s.stop) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func (s *Server) touch() {
	s.lastMu.Lock()
	s.last = time.Now()
	s.lastMu.Unlock()
}

func (s *Server) idle() bool {
	s.lastMu.Lock()
	defer s.lastMu.Unlock()
	// An open tab's event stream does not count as activity: a forgotten
	// browser tab must not keep a mutating server alive forever.
	return time.Since(s.last) > s.o.Idle
}

func openBrowser(link string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", link)
	default:
		cmd = exec.Command("xdg-open", link)
	}
	_ = cmd.Start()
}

// --- request plumbing --------------------------------------------------------------

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", CSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	// DNS rebinding: a public name resolving to 127.0.0.1 carries its own Host.
	if !s.hosts[r.Host] {
		writeErr(w, http.StatusForbidden, "forbidden", "unexpected Host header")
		return
	}
	s.touch()
	path := r.URL.EscapedPath()
	switch {
	case strings.HasPrefix(path, "/assets/"):
		s.serveAsset(w, r, strings.TrimPrefix(r.URL.Path, "/assets/"))
	case strings.HasPrefix(path, "/s/"):
		if s.authQuery(r) {
			// The page strips ?t= from the address bar; a strict, HttpOnly
			// session cookie lets a reload (or a second tab) keep working.
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: s.token, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteStrictMode})
		} else if !s.authCookie(r) {
			writeErr(w, http.StatusForbidden, "forbidden", "missing or wrong token")
			return
		}
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET only")
			return
		}
		s.serveAsset(w, r, "index.html")
	case path == "/api/events":
		if !s.authQuery(r) && !s.authHeader(r) && !s.authCookie(r) {
			writeErr(w, http.StatusForbidden, "forbidden", "missing or wrong token")
			return
		}
		s.events(w, r)
	case strings.HasPrefix(path, "/api/spec/"):
		// Header or the SameSite=Strict cookie. Cross-site requests carry
		// neither, and POSTs must also be application/json (no simple form CSRF).
		if !s.authHeader(r) && !s.authCookie(r) {
			writeErr(w, http.StatusForbidden, "forbidden", "missing or wrong token")
			return
		}
		s.api(w, r, strings.TrimPrefix(path, "/api/spec/"))
	default:
		writeErr(w, http.StatusNotFound, "not_found", "no such page")
	}
}

func (s *Server) tokenOK(got string) bool {
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func (s *Server) authQuery(r *http.Request) bool { return s.tokenOK(r.URL.Query().Get("t")) }

func (s *Server) authHeader(r *http.Request) bool { return s.tokenOK(r.Header.Get("X-Agtrack-Token")) }

// sessionCookie carries the capability token after the first tokened load.
const sessionCookie = "agtrack_session"

func (s *Server) authCookie(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	return err == nil && s.tokenOK(c.Value)
}

func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" || strings.Contains(name, "..") || strings.HasPrefix(name, "dev/") {
		writeErr(w, http.StatusNotFound, "not_found", "no such asset")
		return
	}
	data, err := fs.ReadFile(s.assets, name)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "no such asset")
		return
	}
	ct := mime.TypeByExtension(path.Ext(name))
	if path.Ext(name) == ".woff2" {
		ct = "font/woff2" // not in Go's built-in table; system tables vary
	}
	if ct == "" {
		ct = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Write(data)
}

func writeErr(w http.ResponseWriter, status int, token, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(render.JSONCompact(row.Of("error", token, "message", msg)))
}

func writeJSON(w http.ResponseWriter, status int, obj any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(render.JSONCompact(obj))
}

// storeErr maps a store failure onto an HTTP status with the CLI's token.
func storeErr(w http.ResponseWriter, err error) {
	e, ok := errs.As(err)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errs.TokDatabase, err.Error())
		return
	}
	status := http.StatusBadRequest
	switch e.Code {
	case errs.ExitNotFound:
		status = http.StatusNotFound
	case errs.ExitConflict:
		status = http.StatusConflict
	}
	writeErr(w, status, e.Token, e.Message)
}

// --- API -------------------------------------------------------------------------------

// api routes /api/spec/{spec}[/tasks|/diff|/note|/note/{id}/resolve|/release|/approve].
// The server is bound to one spec; {spec} must name it.
func (s *Server) api(w http.ResponseWriter, r *http.Request, rest string) {
	specPart, tail, _ := strings.Cut(rest, "/")
	ref, err := url.PathUnescape(specPart)
	if err != nil || ref != s.o.SpecRef {
		writeErr(w, http.StatusNotFound, "not_found", "this viewer serves "+s.o.SpecRef)
		return
	}
	if r.Method == http.MethodPost {
		ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if ct != "application/json" {
			writeErr(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "POST bodies must be application/json")
			return
		}
	}
	id := s.o.SpecID
	switch {
	case tail == "" && r.Method == http.MethodGet:
		s.locked(w, func() (any, error) {
			m, err := s.o.Store.SpecModel(id)
			if err != nil {
				return nil, err
			}
			return m.Set("viewer", row.Of("actor", s.o.Viewer)), nil
		})
	case tail == "tasks" && r.Method == http.MethodGet:
		s.locked(w, func() (any, error) { return s.o.Store.SpecTasks(id) })
	case tail == "diff" && r.Method == http.MethodGet:
		q := r.URL.Query()
		from, to := 0, 0
		for _, p := range []struct {
			name string
			dst  *int
		}{{"from", &from}, {"to", &to}} {
			if v := q.Get(p.name); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil || n < 1 {
					writeErr(w, http.StatusBadRequest, errs.TokInvalidArgument, p.name+" must be a revision number ≥ 1")
					return
				}
				*p.dst = n
			}
		}
		s.locked(w, func() (any, error) { return s.diff(id, from, to) })
	case tail == "note" && r.Method == http.MethodPost:
		var in struct {
			Section, Quote, Prefix, Suffix, Kind, Body string
			ParentID                                   *int64 `json:"parent_id"`
		}
		if !decode(w, r, &in) {
			return
		}
		kind := in.Kind
		if kind == "" {
			kind = "note"
		}
		switch kind {
		case "note", "question", "decision", "blocker", "build-note":
		default:
			writeErr(w, http.StatusBadRequest, errs.TokInvalidArgument, "kind must be note, question, decision, blocker or build-note")
			return
		}
		if strings.TrimSpace(in.Body) == "" {
			writeErr(w, http.StatusBadRequest, errs.TokInvalidArgument, "a note needs a body")
			return
		}
		var parent int64
		if in.ParentID != nil {
			parent = *in.ParentID
		}
		var note *row.Row
		var pinned string
		var modified bool
		var err error
		s.withLock(func() {
			note, pinned, modified, err = s.o.Store.SpecAddNote(id, store.SpecNoteInput{Section: in.Section, Quote: in.Quote,
				Prefix: in.Prefix, Suffix: in.Suffix, Kind: kind, Body: in.Body, ParentID: parent, RepoRoot: s.o.RepoRoot})
		})
		if err != nil {
			storeErr(w, err)
			return
		}
		var p any
		if pinned != "" {
			p = pinned
		}
		writeJSON(w, http.StatusCreated, row.Of("note", note, "pinned", p, "file_modified", modified))
	case strings.HasPrefix(tail, "note/") && strings.HasSuffix(tail, "/resolve") && r.Method == http.MethodPost:
		nid, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(tail, "note/"), "/resolve"), 10, 64)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "no such note")
			return
		}
		var in struct{ Note string }
		if !decode(w, r, &in) {
			return
		}
		s.locked(w, func() (any, error) {
			ok, err := s.o.Store.SpecNoteBelongs(id, nid)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, errs.NotFound("note", fmt.Sprint(nid), "no such note on this spec: %d", nid)
			}
			if _, _, err := s.o.Store.ResolveNote(nid, in.Note, false); err != nil {
				return nil, err
			}
			model, err := s.o.Store.SpecModel(id)
			if err != nil {
				return nil, err
			}
			for _, n := range asRows(model.Get("notes")) {
				if n.Int("id") == nid {
					return row.Of("note", n), nil
				}
			}
			return row.Of("note", row.Of("id", nid)), nil
		})
	case tail == "release" && r.Method == http.MethodPost:
		var in struct{}
		if !decode(w, r, &in) {
			return
		}
		s.locked(w, func() (any, error) {
			n, cursor, err := s.o.Store.SpecRelease(id)
			if err != nil {
				return nil, err
			}
			return row.Of("released", int64(n), "cursor", cursor), nil
		})
	case tail == "approve" && r.Method == http.MethodPost:
		var in struct {
			Note       string
			BuildNotes []string `json:"build_notes"`
		}
		if !decode(w, r, &in) {
			return
		}
		s.locked(w, func() (any, error) {
			a, bn, err := s.o.Store.SpecApprove(id, in.Note, in.BuildNotes)
			if err != nil {
				return nil, err
			}
			if bn == nil {
				bn = []*row.Row{}
			}
			return row.Of("approval", a, "build_notes", bn), nil
		})
	default:
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
	}
}

// diff compares two revisions for the page. to defaults to the head; from to
// the approved revision when it is older than to, else to-1 — stepping further
// back past revisions that change no section (a lazy pin adds a revision that
// only writes an id comment), so the default shows the last real edit. A spec
// with a single revision has nothing to compare and returns no changes.
func (s *Server) diff(id int64, from, to int) (any, error) {
	model, err := s.o.Store.SpecModel(id)
	if err != nil {
		return nil, err
	}
	versions, err := s.o.Store.SpecVersions(id)
	if err != nil {
		return nil, err
	}
	sp, _ := model.Get("spec").(*row.Row)
	head := 0
	approved := 0
	if sp != nil {
		head = int(sp.Int("revision"))
		if a, ok := sp.Get("approval").(*row.Row); ok && a != nil {
			approved = int(a.Int("revision"))
		}
	}
	if to == 0 {
		to = head
	}
	stepBack := false
	if from == 0 {
		from = to - 1
		if approved > 0 && approved < to {
			from = approved
		} else {
			stepBack = true
		}
	}
	vs := make([]*row.Row, 0, len(versions))
	for _, v := range versions {
		vs = append(vs, row.Of("n", v.Int("n"), "imported_at", v.Get("imported_at"), "actor", v.Get("actor")))
	}
	out := row.Of("from", int64(from), "to", int64(to), "head", int64(head), "approved_revision", nilIfZero(approved),
		"versions", vs, "changes", []*row.Row{})
	if from < 1 || from == to {
		return out.Set("from", int64(max(from, 0))), nil
	}
	changes, err := s.o.Store.SpecDiff(id, from, to)
	if err != nil {
		return nil, err
	}
	for stepBack && len(changes) == 0 && from > 1 {
		if changes, err = s.o.Store.SpecDiff(id, from-1, to); err != nil {
			return nil, err
		}
		from--
	}
	out.Set("from", int64(from))
	if changes == nil {
		changes = []*row.Row{}
	}
	return out.Set("changes", changes), nil
}

func nilIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return int64(n)
}

func asRows(v any) []*row.Row {
	rows, _ := v.([]*row.Row)
	return rows
}

// withLock runs fn holding the store mutex, releasing it even on panic.
func (s *Server) withLock(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn()
}

func (s *Server) locked(w http.ResponseWriter, fn func() (any, error)) {
	var obj any
	var err error
	s.withLock(func() { obj, err = fn() })
	if err != nil {
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, obj)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, errs.TokInvalidArgument, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// events is the SSE stream. The signal itself is a database row; this is a
// latency optimisation for the page (go spec §9.1.6).
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "error", "streaming unsupported")
		return
	}
	if ref := r.URL.Query().Get("spec"); ref != "" && ref != s.o.SpecRef {
		writeErr(w, http.StatusNotFound, "not_found", "this viewer serves "+s.o.SpecRef)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			after = n
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	s.lastMu.Lock()
	s.clients++
	s.lastMu.Unlock()
	defer func() {
		s.lastMu.Lock()
		s.clients--
		s.last = time.Now()
		s.lastMu.Unlock()
	}()
	poll := time.NewTicker(500 * time.Millisecond)
	ping := time.NewTicker(15 * time.Second)
	defer poll.Stop()
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.stop:
			return
		case <-ping.C:
			fmt.Fprint(w, "event: ping\ndata: {}\n\n")
			fl.Flush()
		case <-poll.C:
			var evs []*row.Row
			var err error
			s.withLock(func() { evs, err = s.o.Store.SpecEventsAfter(s.o.SpecID, after, 200) })
			if err != nil {
				fmt.Fprintf(w, "event: error\ndata: %s\n\n", render.JSONCompact(row.Of("message", err.Error())))
				fl.Flush()
				return
			}
			for _, e := range evs {
				after = e.Int("id")
				data := row.Of("id", e.Int("id"), "action", e.Str("action"), "actor", e.Get("actor"),
					"target", e.Get("target"), "detail", e.Get("detail"))
				fmt.Fprintf(w, "id: %d\nevent: agtrack\ndata: %s\n\n", after, render.JSONCompact(data))
			}
			if len(evs) > 0 {
				fl.Flush()
			}
		}
	}
}
