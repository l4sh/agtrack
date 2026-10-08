package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/l4sh/agtrack/internal/db"
	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
	"github.com/l4sh/agtrack/internal/web"
)

// `agtrack serve`: the browse app (web-browse-spec "agtrack serve"). The
// server is internal/web in ModeBrowse; this file is its backend. Reads and
// writes run the CLI's own verb handlers in-process with -f json, so every
// write has the CLI's validation, events, idempotency receipts and
// attribution, and every read returns the rows `-f json` prints.

func cmdServe(c *Ctx) error {
	o, err := c.browseOptions()
	if err != nil {
		return err
	}
	path := serveRecordPath(c.DBPath)
	register := true
	if rec := liveServeRecord(path); rec != nil && !c.Bool("new") {
		switch {
		case c.Bool("restart"):
			if err := stopServer(rec); err != nil {
				return err
			}
			os.Remove(path)
		case rec.As == o.Viewer:
			link := rec.base() + "/?t=" + rec.Token
			fmt.Fprintf(c.Out, "agtrack serve is already running at %s\n", link)
			fmt.Fprintf(c.Out, "pid %d, browser edits by %s, started %s\n", rec.PID, rec.As, rec.StartedAt)
			if rec.Build != buildID() {
				fmt.Fprintln(c.Out, "it predates this build of agtrack: `agtrack serve --restart` replaces it")
			}
			if c.Bool("open") {
				web.OpenBrowser(link)
			}
			return nil
		default:
			// A different --as gets its own server; the recorded one stays
			// the one others reuse.
			fmt.Fprintf(c.Err, "agtrack: agtrack serve (pid %d) runs as %s; starting another for %s\n", rec.PID, rec.As, o.Viewer)
			register = false
		}
	} else if rec != nil {
		register = false
	}
	if register {
		pid := os.Getpid()
		o.Build = buildID()
		o.OnListen = func(port int, token string) error {
			return writeServeRecord(path, &serveRecord{PID: pid, Port: port, Token: token, As: o.Viewer,
				Build: o.Build, DB: db.ResolvePath(c.DBPath), Root: o.RepoRoot, StartedAt: time.Now().UTC().Format(time.RFC3339)})
		}
		// Deferred for a normal return (idle timeout); the exit hook covers
		// Ctrl+C and SIGTERM, where main exits without unwinding.
		defer removeServeRecord(path, pid)
		AtExit(func() { removeServeRecord(path, pid) })
	}
	return web.Serve(context.Background(), o)
}

// browseOptions builds the `agtrack serve` session. -c picks the context the
// app opens on; without it, the serving actor's current context.
func (c *Ctx) browseOptions() (web.Options, error) {
	idle, err := parseDuration(c.Str("idle"))
	if err != nil {
		return web.Options{}, err
	}
	ws, as, err := c.browserStore()
	if err != nil {
		return web.Options{}, err
	}
	start := c.Context
	if start != "" {
		if _, err := c.S.GetContext(start, true); err != nil {
			return web.Options{}, err
		}
	} else if cur, err := c.S.CurrentContext(); err != nil {
		return web.Options{}, err
	} else if cur != nil {
		start = cur.Str("name")
	}
	b := &browser{db: c.S.DB, as: as, cwdRepo: c.S.CwdRepo, start: start}
	return web.Options{Mode: web.ModeBrowse, Store: ws, Browse: b, Context: start,
		Addr: fmt.Sprintf("127.0.0.1:%d", c.Int("port")), Idle: idle, IdleNever: idle == 0, Out: c.Out,
		Open: c.Bool("open"), RepoRoot: cwdRoot(), Viewer: as}, nil
}

// browserStore is the store browser actions are written through: --as
// (default "human") on the same connection, never the serving actor, whose
// `--actor-not self` would otherwise hide every human edit.
func (c *Ctx) browserStore() (*store.Store, string, error) {
	as := strings.TrimSpace(c.Str("as"))
	if as == "" {
		as = "human"
	}
	if as == c.Actor {
		return nil, "", errs.New("--as %s is also the serving actor: browser actions would be attributed to it (and hidden by its --actor-not self); pick another name", as)
	}
	ws := store.New(c.S.DB, as)
	ws.CwdRepo = c.S.CwdRepo
	return ws, as, nil
}

// browser implements web.Browse.
type browser struct {
	db      *db.DB
	as      string
	cwdRepo func() string
	start   string // the context the app opens on
}

func (b *browser) store(context string) *store.Store {
	s := store.New(b.db, b.as)
	s.Context = context
	s.CwdRepo = b.cwdRepo
	return s
}

// exec runs one agtrack verb in-process as the --as actor and returns its
// -f json output, decoded. The caller builds argv from typed fields only:
// user text goes into `--flag=value` or after `--`, never where it could
// be read as a flag.
func (b *browser) exec(argv ...string) (any, error) { return b.execExpect(nil, argv...) }

// execExpect is exec with a title compare-and-set precondition for `task
// set` / `sub set`, checked by the store inside the write transaction.
func (b *browser) execExpect(expectTitle *string, argv ...string) (any, error) {
	full := append([]string{"--actor=" + b.as, "--format=json"}, argv...)
	res, err := NewParser().Parse(full)
	if err != nil {
		if _, ok := errs.As(err); ok {
			return nil, err
		}
		return nil, errs.Usage("%s", err.Error())
	}
	var out, errb bytes.Buffer
	c, err := newCtx(res, Streams{Out: &out, Err: &errb})
	if err != nil {
		return nil, err
	}
	h, ok := res.Cmd.Run.(Handler)
	if !ok {
		return nil, errs.Usage("not a command: %s", strings.Join(res.Path, " "))
	}
	c.S = b.store(c.Context)
	c.expectTitle = expectTitle
	if c.idemKey != "" {
		if !idemVerbs[strings.Join(res.Path, " ")] {
			return nil, errs.Usage("--idem is only accepted by create verbs")
		}
		c.idemSHA = requestSHA(res.Path, res.Values, c.Actor, c.Context)
		rc, hit, err := c.S.CheckReceipt(c.idemKey, c.idemSHA)
		if err != nil {
			return nil, err
		}
		if hit {
			c.receipt = rc
		}
	}
	if err := h(c); err != nil {
		return nil, err
	}
	if out.Len() == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(&out)
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%s: unexpected output: %v", strings.Join(res.Path, " "), err)
	}
	return plainJSON(v), nil
}

// plainJSON turns decoded JSON into the value types internal/render
// encodes natively (json.Number would be stringified).
func plainJSON(v any) any {
	switch x := v.(type) {
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n
		}
		f, _ := x.Float64()
		return f
	case []any:
		for i := range x {
			x[i] = plainJSON(x[i])
		}
		return x
	case map[string]any:
		r := row.New()
		// Keep a stable order for the page; the CLI's own key order is lost
		// in decoding, and the API consumer reads by key.
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			r.Set(k, plainJSON(x[k]))
		}
		return r
	}
	return v
}

func asList(v any) []any {
	l, _ := v.([]any)
	if l == nil {
		return []any{}
	}
	return l
}

func asRow(v any) *row.Row {
	r, _ := v.(*row.Row)
	if r == nil {
		return row.New()
	}
	return r
}

// --- spec resolution and events -------------------------------------------------------

func (b *browser) ResolveSpec(ref string) (int64, string, error) {
	s := b.store("")
	sp, err := s.ResolveSpec(ref, "")
	if err != nil {
		return 0, "", err
	}
	p, err := s.GetProject(sp.Int("project_id"))
	if err != nil {
		return 0, "", err
	}
	path, err := s.ProjectPath(p)
	return sp.Int("id"), path, err
}

func (b *browser) EventsAfter(after int64, limit int) ([]*row.Row, error) {
	rows, _, err := b.store("").ListEvents(store.EventFilter{AfterID: after, ByID: true, Ascending: true, AllContexts: true, Limit: limit})
	return rows, err
}

// --- reads ----------------------------------------------------------------------------

func (b *browser) Read(req web.BrowseRequest) (any, error) {
	q := req.Query
	switch req.Op {
	case "home":
		return b.home()
	case "context":
		return b.context(req.Ref)
	case "project":
		return b.project(req.Ref)
	case "project.tasks":
		return b.projectTasks(req.Ref, q)
	case "project.specs":
		p, path, err := b.project0(req.Ref)
		if err != nil {
			return nil, err
		}
		return b.specSummaries(p, contextOf(path))
	case "project.notes":
		_, path, err := b.project0(req.Ref)
		if err != nil {
			return nil, err
		}
		open, err := b.openNotes("", path, q.Get("kind"))
		if err != nil {
			return nil, err
		}
		own, err := b.exec("project", "notes", "--", path)
		if err != nil {
			return nil, err
		}
		return row.Of("open", open, "notes", asList(own)), nil
	case "project.prs":
		return b.projectPRs(req.Ref)
	case "task":
		return b.task(req.Ref)
	case "notes":
		return b.openNotes(q.Get("context"), q.Get("project"), q.Get("kind"))
	case "log":
		return b.log(q)
	}
	return nil, errs.NotFound("endpoint", req.Op, "no such endpoint")
}

func contextOf(path string) string {
	c, _, _ := strings.Cut(path, "/")
	return c
}

func (b *browser) home() (any, error) {
	ctxs, err := b.exec("ctx", "ls", "--archived")
	if err != nil {
		return nil, err
	}
	sum, err := b.exec("summary", "--all-contexts")
	if err != nil {
		return nil, err
	}
	counts := map[string]any{}
	for _, c := range asList(asRow(sum).Get("contexts")) {
		cr := asRow(c)
		counts[cr.Str("name")] = cr.Get("counts")
	}
	for _, c := range asList(ctxs) {
		cr := asRow(c)
		if n, ok := counts[cr.Str("name")]; ok {
			cr.Set("counts", n)
		}
	}
	log, err := b.exec("log", "--all-contexts", "-n", "15")
	if err != nil {
		return nil, err
	}
	notes, err := b.openNotes("", "", "handoff,blocker")
	if err != nil {
		return nil, err
	}
	// The newest event id: where the page's event stream starts.
	cursor, err := b.store("").MaxEventID()
	if err != nil {
		return nil, err
	}
	return row.Of("start", b.start, "viewer", b.as, "cursor", cursor, "contexts", asList(ctxs), "log", asList(log), "notes", notes), nil
}

func (b *browser) context(name string) (any, error) {
	show, err := b.exec("ctx", "show", "--", name)
	if err != nil {
		return nil, err
	}
	projects, err := b.exec("project", "ls", "--archived", "--context="+name)
	if err != nil {
		return nil, err
	}
	specs, err := b.specSummaries(nil, name)
	if err != nil {
		return nil, err
	}
	return row.Of("context", show, "projects", asList(projects), "specs", specs), nil
}

// project0 resolves a context-qualified project ref.
func (b *browser) project0(ref string) (*row.Row, string, error) {
	s := b.store("")
	p, err := s.ResolveProject(ref, "", true)
	if err != nil {
		return nil, "", err
	}
	path, err := s.ProjectPath(p)
	return p, path, err
}

func (b *browser) project(ref string) (any, error) {
	p, path, err := b.project0(ref)
	if err != nil {
		return nil, err
	}
	show, err := b.exec("project", "show", "--", path)
	if err != nil {
		return nil, err
	}
	pins, err := b.store("").ProjectPinnedBy(p.Int("id"))
	if err != nil {
		return nil, err
	}
	// Breadcrumb: context, then the parent project for a subproject.
	ctxName, rest, _ := strings.Cut(path, "/")
	crumbs := []any{row.Of("kind", "context", "ref", ctxName, "name", ctxName)}
	if parent, _, ok := strings.Cut(rest, ":"); ok {
		crumbs = append(crumbs, row.Of("kind", "project", "ref", ctxName+"/"+parent, "name", parent))
	}
	return row.Of("project", show, "path", path, "context", ctxName, "short", rest, "parents", crumbs, "pinned_by", pins), nil
}

// projectTasks lists a project's tasks with the `task ls` filters, named as
// its flags: status, tag (repeatable), grep, ready, blocked, unclaimed,
// direct, since, order, limit; plus claimed_by, which `task ls` has no flag
// for and is applied to its rows.
func (b *browser) projectTasks(ref string, q map[string][]string) (any, error) {
	_, path, err := b.project0(ref)
	if err != nil {
		return nil, err
	}
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	argv := []string{"task", "ls", "--project=" + path}
	if v := get("status"); v != "" {
		argv = append(argv, "--status="+v)
	}
	for _, t := range q["tag"] {
		if t != "" {
			argv = append(argv, "--tag="+t)
		}
	}
	for _, k := range []string{"grep", "since", "order", "limit"} {
		if v := get(k); v != "" {
			argv = append(argv, "--"+k+"="+v)
		}
	}
	flags := []string{}
	for _, k := range []string{"ready", "blocked", "unclaimed", "direct", "tags_all"} {
		if v := get(k); v == "1" || v == "true" {
			flags = append(flags, "--"+strings.ReplaceAll(k, "_", "-"))
		}
	}
	rows, err := b.exec(append(argv, flags...)...)
	if err != nil {
		return nil, err
	}
	// The blocked marker: the same listing restricted to --blocked.
	blockedArgv := append(append([]string{}, argv...), "--blocked")
	for _, f := range flags {
		if f != "--ready" && f != "--blocked" {
			blockedArgv = append(blockedArgv, f)
		}
	}
	blocked := map[int64]bool{}
	if get("ready") == "" {
		brows, err := b.exec(blockedArgv...)
		if err != nil {
			return nil, err
		}
		for _, r := range asList(brows) {
			blocked[asRow(r).Int("id")] = true
		}
	}
	// Who touched each task last: one aggregate query for the whole listing.
	ids := []int64{}
	for _, r := range asList(rows) {
		ids = append(ids, asRow(r).Int("id"))
	}
	last, err := b.store("").LastEventByTask(ids)
	if err != nil {
		return nil, err
	}
	claimed := get("claimed_by")
	out := []any{}
	for _, r := range asList(rows) {
		t := asRow(r)
		if claimed != "" && t.Str("claimed_by") != claimed {
			continue
		}
		t.Set("blocked", blocked[t.Int("id")])
		if ev := last[t.Int("id")]; ev != nil {
			t.Set("last_actor", ev.Get("actor"))
			t.Set("last_event_at", ev.Get("at"))
		}
		out = append(out, t)
	}
	return row.Of("project", path, "tasks", out), nil
}

// specSummaries lists specs (of a project, or of a whole context) with their
// review state: approval, staleness, open notes, comments awaiting release.
func (b *browser) specSummaries(p *row.Row, context string) (any, error) {
	s := b.store(context)
	specs, err := s.ListSpecs(p, context)
	if err != nil {
		return nil, err
	}
	states := map[int64]map[string]*row.Row{}
	out := []any{}
	for _, sp := range specs {
		pid := sp.Int("project_id")
		if _, ok := states[pid]; !ok {
			rs, err := s.SpecReviewState(pid)
			if err != nil {
				return nil, err
			}
			states[pid] = map[string]*row.Row{}
			for _, r := range rs {
				states[pid][r.Str("spec")] = r
			}
		}
		proj, err := s.GetProject(pid)
		if err != nil {
			return nil, err
		}
		path, err := s.ProjectPath(proj)
		if err != nil {
			return nil, err
		}
		item := row.Of("slug", sp.Str("slug"), "ref", sp.Str("ref"), "qualified_ref", path+"/"+sp.Str("slug"),
			"title", sp.Get("title"), "status", sp.Str("status"), "source_path", sp.Get("source_path"),
			"revision", sp.Int("revision"), "project", path,
			"approval", "none", "approval_revision", nil, "open_notes", int64(0), "pending_comments", int64(0))
		if st := states[pid][sp.Str("slug")]; st != nil {
			item.Set("open_notes", st.Get("open_notes"))
			item.Set("pending_comments", st.Get("pending_comments"))
			if !st.IsNull("approval_revision") && st.Has("approval_revision") {
				item.Set("approval_revision", st.Get("approval_revision"))
				if st.Get("approval_stale") == true {
					item.Set("approval", "stale")
				} else {
					item.Set("approval", "approved")
				}
			}
		}
		out = append(out, item)
	}
	return out, nil
}

func (b *browser) projectPRs(ref string) (any, error) {
	_, path, err := b.project0(ref)
	if err != nil {
		return nil, err
	}
	tasks, err := b.exec("task", "ls", "--project="+path, "--status=all")
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, t := range asList(tasks) {
		tr := asRow(t)
		if tr.Int("prs") == 0 {
			continue
		}
		prs, err := b.exec("pr", "ls", "--task="+strconv.FormatInt(tr.Int("id"), 10))
		if err != nil {
			return nil, err
		}
		for _, pr := range asList(prs) {
			pr := asRow(pr)
			pr.Set("task_title", tr.Get("title"))
			out = append(out, pr)
		}
	}
	return out, nil
}

func (b *browser) task(ref string) (any, error) {
	id, err := parseID(ref)
	if err != nil {
		return nil, err
	}
	sid := strconv.FormatInt(id, 10)
	show, err := b.exec("task", "show", "-x", "--", sid)
	if err != nil {
		return nil, err
	}
	deps, err := b.exec("task", "deps", "--", sid)
	if err != nil {
		return nil, err
	}
	arts, err := b.exec("task", "artifact", "--", sid, "ls")
	if err != nil {
		return nil, err
	}
	s := b.store("")
	t, err := s.GetTaskID(id)
	if err != nil {
		return nil, err
	}
	p, err := s.GetProject(t.Int("project_id"))
	if err != nil {
		return nil, err
	}
	path, err := s.ProjectPath(p)
	if err != nil {
		return nil, err
	}
	checks, err := s.SpecLinksForTask(id)
	if err != nil {
		return nil, err
	}
	specs := []any{}
	for _, c := range checks {
		item := row.Of("section", c.Get("section"), "kind", c.Get("kind"), "state", c.Get("state"), "open_notes", c.Get("open_notes"))
		if c.Has("spec") {
			item.Set("spec", contextOf(path)+"/"+c.Str("spec"))
			item.Set("slug", c.Get("slug"))
		}
		specs = append(specs, item)
	}
	// Notes on steps ride on each step as note_list, same shape as the task's.
	for _, st := range asList(asRow(show).Get("subtask_list")) {
		sr, ok := st.(*row.Row)
		if !ok {
			continue
		}
		ns, err := s.NotesFor("subtask", sr.Int("id"), store.NoteFilter{})
		if err != nil {
			return nil, err
		}
		sr.Set("note_list", ns)
	}
	return row.Of("task", show, "project_path", path, "context", contextOf(path), "deps", deps,
		"artifacts", asList(arts), "specs", specs), nil
}

// openNotes lists unresolved notes across every context, or narrowed to a
// context or project (with its subprojects), each with where it lives.
func (b *browser) openNotes(context, project, kind string) (any, error) {
	s := b.store("")
	var kinds []string
	for _, k := range strings.Split(kind, ",") {
		if k = strings.TrimSpace(k); k != "" {
			nk, err := normalizeNoteKind(k)
			if err != nil {
				return nil, err
			}
			kinds = append(kinds, nk)
		}
	}
	if project != "" {
		_, path, err := b.project0(project)
		if err != nil {
			return nil, err
		}
		project = path
	}
	rows, err := s.OpenNotes(nil, 0, kinds)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, n := range leanKind(rows, "note").([]*row.Row) {
		where, err := s.NoteWhere(n)
		if err != nil {
			return nil, err
		}
		for _, k := range where.Keys() {
			n.Set(k, where.Get(k))
		}
		path := n.Str("project")
		switch {
		case project != "" && path != project && !strings.HasPrefix(path, project+":"):
			continue
		case context != "" && n.Str("context") != context:
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// log reads the worklog with the `log` filters: context, project, actor
// (--by), action, since, until, limit (default 50). Without context or
// project it covers every context.
func (b *browser) log(q map[string][]string) (any, error) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	argv := []string{"log"}
	switch {
	case get("project") != "":
		_, path, err := b.project0(get("project"))
		if err != nil {
			return nil, err
		}
		argv = append(argv, "--project="+path)
	case get("context") != "":
		argv = append(argv, "--context="+get("context"))
	default:
		argv = append(argv, "--all-contexts")
	}
	for _, f := range [][2]string{{"actor", "by"}, {"action", "action"}, {"since", "since"}, {"until", "until"}, {"task", "task"}} {
		if v := get(f[0]); v != "" {
			argv = append(argv, "--"+f[1]+"="+v)
		}
	}
	limit := get("limit")
	if limit == "" {
		limit = "50"
	}
	rows, err := b.exec(append(argv, "--limit="+limit)...)
	if err != nil {
		return nil, err
	}
	total, err := b.exec(append(argv, "--count")...)
	if err != nil {
		return nil, err
	}
	return row.Of("events", asList(rows), "total", total), nil
}

// --- writes ---------------------------------------------------------------------------

var statusVerb = map[string]string{"todo": "reopen", "in-progress": "start", "done": "done", "cancelled": "cancel"}

func idemFlag(in web.BrowseInput) []string {
	if in.Idem == "" {
		return nil
	}
	return []string{"--idem=" + in.Idem}
}

func (b *browser) Write(req web.BrowseRequest) (any, error) {
	in := req.In
	ref := req.Ref
	switch req.Op {
	case "task.add":
		_, path, err := b.project0(ref)
		if err != nil {
			return nil, err
		}
		argv := append([]string{"task", "add", "--project=" + path}, idemFlag(in)...)
		for _, t := range in.Tags {
			argv = append(argv, "--tag="+t)
		}
		if strings.TrimSpace(in.Note) != "" {
			argv = append(argv, "--note="+in.Note)
		}
		for _, st := range in.Steps {
			if strings.TrimSpace(st) != "" {
				argv = append(argv, "--step="+st)
			}
		}
		return b.exec(append(argv, "--", in.Title)...)
	case "task.title":
		id, err := b.taskID(ref)
		if err != nil {
			return nil, err
		}
		if err := checkTitle("task", in); err != nil {
			return nil, err
		}
		return b.execExpect(in.Expected, "task", "set", "--title="+in.Title, "--", id)
	case "task.tags":
		id, err := parseID(ref)
		if err != nil {
			return nil, err
		}
		s := b.store("")
		t, err := s.GetTaskID(id)
		if err != nil {
			return nil, err
		}
		if _, _, err := s.ChangeTags("task", id, splitTags(in.Add), splitTags(in.Remove), id, t.Int("project_id")); err != nil {
			return nil, err
		}
		return b.exec("task", "show", "--", strconv.FormatInt(id, 10))
	case "task.status", "step.status":
		verb, ok := statusVerb[in.Status]
		if !ok {
			return nil, errs.New("status must be todo, in-progress, done or cancelled")
		}
		noun, target := "task", ref
		if req.Op == "step.status" {
			noun = "sub"
		} else {
			id, err := b.taskID(ref)
			if err != nil {
				return nil, err
			}
			target = id
		}
		argv := []string{noun, verb}
		if strings.TrimSpace(in.Note) != "" {
			argv = append(argv, "--note="+in.Note)
		}
		return b.exec(append(argv, "--", target)...)
	case "task.release":
		id, err := b.taskID(ref)
		if err != nil {
			return nil, err
		}
		return b.exec("task", "release", "--force", "--", id)
	case "task.block", "task.unblock":
		id, err := b.taskID(ref)
		if err != nil {
			return nil, err
		}
		on, err := b.taskID(in.On)
		if err != nil {
			return nil, err
		}
		verb := strings.TrimPrefix(req.Op, "task.")
		argv := []string{"task", verb, "--on=" + on}
		if verb == "block" {
			argv = append(argv, "--kind=blocks")
		}
		return b.exec(append(argv, "--", id)...)
	case "steps.add":
		id, err := b.taskID(ref)
		if err != nil {
			return nil, err
		}
		var titles []string
		for _, st := range in.Steps {
			if strings.TrimSpace(st) != "" {
				titles = append(titles, st)
			}
		}
		if len(titles) == 0 {
			return nil, errs.New("a step needs a title")
		}
		argv := append([]string{"sub", "add"}, idemFlag(in)...)
		return b.exec(append(append(argv, "--", id), titles...)...)
	case "step.title":
		st, err := b.store("").GetSubtask(ref, true)
		if err != nil {
			return nil, err
		}
		if err := checkTitle("step", in); err != nil {
			return nil, err
		}
		return b.execExpect(in.Expected, "sub", "set", "--title="+in.Title, "--", strconv.FormatInt(st.Int("id"), 10))
	case "task.note", "project.note":
		target := ref
		noun := "project"
		if req.Op == "task.note" {
			noun = "task"
			id, err := b.taskID(ref)
			if err != nil {
				return nil, err
			}
			target = id
		} else {
			_, path, err := b.project0(ref)
			if err != nil {
				return nil, err
			}
			target = path
		}
		kind := in.Kind
		if kind == "" {
			kind = "note"
		}
		argv := append([]string{noun, "note", "--kind=" + kind}, idemFlag(in)...)
		return b.exec(append(argv, "--", target, in.Body)...)
	case "step.note":
		st, err := b.store("").GetSubtask(ref, true)
		if err != nil {
			return nil, err
		}
		kind := in.Kind
		if kind == "" {
			kind = "note"
		}
		argv := append([]string{"sub", "note", "--kind=" + kind}, idemFlag(in)...)
		return b.exec(append(argv, "--", strconv.FormatInt(st.Int("id"), 10), in.Body)...)
	case "task.log", "project.log":
		argv := append([]string{"log", "add"}, idemFlag(in)...)
		if req.Op == "task.log" {
			id, err := b.taskID(ref)
			if err != nil {
				return nil, err
			}
			argv = append(argv, "--task="+id)
		} else {
			_, path, err := b.project0(ref)
			if err != nil {
				return nil, err
			}
			argv = append(argv, "--project="+path)
		}
		return b.exec(append(argv, "--", in.Body)...)
	case "note.reply":
		id, err := b.taskID(ref) // a note id is a plain integer too
		if err != nil {
			return nil, err
		}
		return b.exec(append(append([]string{"note", "reply"}, idemFlag(in)...), "--", id, in.Body)...)
	case "note.resolve":
		id, err := b.taskID(ref)
		if err != nil {
			return nil, err
		}
		argv := []string{"note", "resolve"}
		if strings.TrimSpace(in.Rationale) != "" {
			argv = append(argv, "--note="+in.Rationale)
		}
		return b.exec(append(argv, "--", id)...)
	case "note.reopen":
		id, err := b.taskID(ref)
		if err != nil {
			return nil, err
		}
		return b.exec("note", "reopen", "--", id)
	}
	return nil, errs.NotFound("endpoint", req.Op, "no such endpoint")
}

// taskID validates a numeric id (`12` or `#12`) and returns it bare.
func (b *browser) taskID(ref string) (string, error) {
	id, err := parseID(ref)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(id, 10), nil
}

// checkTitle validates a title edit before it runs. The page sends the title
// it last saw as `expected`; the store compares it with the stored title
// inside the write transaction (UpdateTask/UpdateSubtask), and a mismatch is
// a 409 carrying `current` and `yours`, so the human can re-apply rather
// than overwrite.
func checkTitle(noun string, in web.BrowseInput) error {
	if strings.TrimSpace(in.Title) == "" {
		return errs.New("a %s needs a title", noun)
	}
	if in.Expected == nil {
		return errs.New("a title edit must send `expected`, the title the page last saw")
	}
	return nil
}

var _ web.Browse = (*browser)(nil)
