package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/l4sh/agtrack/internal/db"
	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/render"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

// Exit codes of the wait commands (go spec §9.1.3).
const (
	ExitApproved    = 0
	ExitComments    = 10
	ExitTimeout     = 11
	ExitSpecChanged = 12
	ExitLost        = 13
)

// exitCode is an error that carries a specific exit status but prints
// nothing more: the command already wrote its terminal line.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

// watchLoop polls the events table by insertion id — never by `at`, which
// backdating makes non-monotonic (go spec §7.5) — and hands each new event to
// onEvent. It returns when onEvent says stop, the timeout expires, or the
// database becomes unreachable; every one of those ends in a TERMINAL line.
type watchLoop struct {
	c        *Ctx
	filter   store.EventFilter
	after    int64
	timeout  time.Duration
	interval time.Duration
	// onEvent returns a non-empty terminal state (and exit code) to stop.
	onEvent func(e *row.Row) (terminal string, code int, extra *row.Row)
	// onTick runs after each poll; it may also end the watch.
	onTick func() (terminal string, code int, extra *row.Row)
	// emit, if set, replaces the default one-line printing of each event
	// (spec watch holds unreleased comments and expands releases).
	emit func(e *row.Row)
	// finish, if set, replaces the TERMINAL line (spec await prints its
	// result object instead) and returns the command's error.
	finish func(state string, code int, extra *row.Row) error
}

// errWriter remembers the first write error, so a watch whose reader went
// away (EPIPE) stops instead of polling until its timeout.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	if err != nil {
		e.err = err
	}
	return n, err
}

func (w *watchLoop) run() error {
	deadline := time.Now().Add(w.timeout)
	path := w.c.S.DB.Path
	ew := &errWriter{w: w.c.Out}
	orig := w.c.Out
	w.c.Out = ew
	defer func() { w.c.Out = orig }()
	for {
		if ew.err != nil {
			return ew.err
		}
		if _, err := os.Stat(path); err != nil {
			return w.terminal("lost", ExitLost, row.Of("reason", "database file is gone: "+path))
		}
		f := w.filter
		f.AfterID = w.after
		f.ByID = true
		f.Limit = 500
		events, _, err := w.c.S.ListEvents(f)
		if err != nil {
			if _, ok := errs.As(err); ok {
				return err
			}
			return w.terminal("lost", ExitLost, row.Of("reason", err.Error()))
		}
		for _, e := range events {
			if ew.err != nil {
				return ew.err
			}
			w.after = e.Int("id")
			if w.emit != nil {
				w.emit(e)
			} else {
				w.emitEvent(e)
			}
			if w.onEvent != nil {
				if t, code, extra := w.onEvent(e); t != "" {
					return w.terminal(t, code, extra)
				}
			}
		}
		if w.onTick != nil {
			if t, code, extra := w.onTick(); t != "" {
				return w.terminal(t, code, extra)
			}
		}
		if len(events) == 500 {
			continue // drain a backlog before sleeping
		}
		if !time.Now().Before(deadline) {
			return w.terminal("timeout", ExitTimeout, nil)
		}
		sleep := w.interval
		if rem := time.Until(deadline); rem < sleep {
			sleep = rem
		}
		time.Sleep(sleep)
	}
}

func (w *watchLoop) emitEvent(e *row.Row) {
	c := w.c
	if c.structured() {
		obj := row.Of("event", e.Int("id"), "action", e.Str("action"), "actor", e.Get("actor"), "target", e.Str("target"),
			"detail", e.Get("detail"), "at", e.Str("at"), "task_id", e.Get("task_id"), "project_id", e.Get("project_id"), "cursor", e.Int("id"))
		c.Out.Write(render.JSONCompact(obj))
		fmt.Fprintln(c.Out)
		return
	}
	actor := e.Str("actor")
	if actor == "" {
		actor = "-"
	}
	detail := e.Str("detail")
	if e.Str("task") != "" && e.Str("target_type") == "task" && e.Str("action") != "log" && e.Str("action") != "note" {
		detail = strings.TrimSpace(detail + "  " + e.Str("task"))
	}
	fmt.Fprintf(c.Out, "EVENT %d %-14s %-8s %s %s\n", e.Int("id"), e.Str("action"), actor, e.Str("target"), strings.ReplaceAll(detail, "\n", " "))
}

func (w *watchLoop) terminal(state string, code int, extra *row.Row) error {
	if w.finish != nil {
		return w.finish(state, code, extra)
	}
	c := w.c
	if c.structured() {
		obj := row.Of("terminal", state, "code", int64(code), "cursor", w.after)
		if extra != nil {
			for _, k := range extra.Keys() {
				obj.Set(k, extra.Get(k))
			}
		}
		c.Out.Write(render.JSONCompact(obj))
		fmt.Fprintln(c.Out)
	} else {
		suffix := ""
		if extra != nil && extra.Str("reason") != "" {
			suffix = " " + extra.Str("reason")
		}
		fmt.Fprintf(c.Out, "TERMINAL %s cursor=%d%s\n", state, w.after, suffix)
	}
	if code == 0 {
		return nil
	}
	return exitCode(code)
}

func (c *Ctx) actorNot() string {
	v := c.Str("actor_not")
	if v == "self" {
		return c.Actor
	}
	return v
}

// cmdWatch streams worklog events for agent-to-agent signalling (go spec §9.1.7).
func cmdWatch(c *Ctx) error {
	timeout, err := parseDuration(c.Str("timeout"))
	if err != nil {
		return err
	}
	if timeout <= 0 {
		return errs.New("watch needs a timeout: a hang needs a human to notice, a timeout is recoverable")
	}
	interval, err := parseDuration(c.Str("interval"))
	if err != nil || interval <= 0 {
		interval = time.Second
	}
	f := store.EventFilter{Context: c.Context, AllContexts: c.Bool("all_contexts"), ActorNot: c.actorNot()}
	if c.Str("actor_not") == "self" && c.Actor == "" {
		fmt.Fprintln(c.Err, "agtrack: warning: --actor-not self has no effect without AGTRACK_ACTOR or --actor")
	}
	if c.Has("task") {
		if f.Task, err = c.S.GetTask(c.Str("task"), true); err != nil {
			return err
		}
	} else if c.Has("project") {
		if f.Project, err = c.resolveProject(c.Str("project"), true); err != nil {
			return err
		}
	}
	if c.Has("action") {
		f.Actions = splitList(c.Str("action"))
	}
	after := int64(c.Int("after"))
	if _, given := c.V["after"].(int); !given {
		if after, err = c.S.MaxEventID(); err != nil {
			return err
		}
	}
	until := map[string]bool{}
	for _, a := range splitList(c.Str("until")) {
		until[a] = true
	}
	w := &watchLoop{c: c, filter: f, after: after, timeout: timeout, interval: interval}
	if len(until) > 0 {
		w.onEvent = func(e *row.Row) (string, int, *row.Row) {
			if until[e.Str("action")] {
				return "matched", 0, row.Of("action", e.Str("action"))
			}
			return "", 0, nil
		}
	}
	return w.run()
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

var _ = db.SchemaVersion
