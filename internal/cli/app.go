// Package cli is agtrack's command layer: the command tree, the handlers,
// and the process entry point with its exit-code contract.
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/l4sh/agtrack/internal/cli/argp"
	"github.com/l4sh/agtrack/internal/db"
	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/render"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

// Epilog closes the root help. A cold agent reads --help, not the README,
// so the exit codes live here (design §4.3).
const Epilog = `references:
  project    [context/]project[:subproject]   orbiter, orbiter:frontend, work/orbiter
             ` + "`.`" + ` resolves the project linked to the git remote of the cwd
  task       12, #12
  subtask    47 (id) or 12.3 (task 12, third step)
  status     todo | in-progress | done | cancelled  (aliases: wip, doing, complete, ...)
             filters also accept open, closed, all and comma separated lists
  spec       auth, orbiter:api/auth; section auth#tokens__rotation or auth#<pin>

environment:
  AGTRACK_DB      database path (default $XDG_DATA_HOME/agtrack/agtrack.db,
                  else ~/.local/share/agtrack/agtrack.db). Point it inside the
                  workspace when the agent is sandboxed out of $HOME.
  AGTRACK_ACTOR   who is writing; recorded on notes and worklog entries, and
                  scopes the project pin to this actor
  AGTRACK_FORMAT  default output format

backfilling:
  --created/--started/--closed on ` + "`task add`" + `, and --at on ` + "`sub add`" + `,
  ` + "`log add`" + `, ` + "`note`" + ` and every status verb, record work that already happened.
  The worklog keeps both times: when the work happened and when it was written
  down. ` + "`log --backdated`" + ` lists what was reconstructed.

exit codes:
  0 ok   1 error   2 usage   3 not found   4 conflict   5 database   130 interrupted
  spec await/watch: 10 comments  11 timeout  12 spec changed  13 channel lost

start here: ` + "`agtrack grammar`" + ` (one screen), then ` + "`agtrack resume`" + `.
`

// Streams lets tests capture output.
type Streams struct {
	Out io.Writer
	Err io.Writer
	// ColorAuto is the auto-detected colour decision for Out (Main decides it
	// from os.Stdout; the buffered wrapper hides the terminal from render).
	ColorAuto bool
}

// Main runs agtrack with os.Args and returns the exit code.
func Main() int {
	out := bufio.NewWriter(os.Stdout)
	code := Run(os.Args[1:], Streams{Out: &flushOnNewline{w: out}, Err: os.Stderr, ColorAuto: render.ColorEnabled(os.Stdout)})
	if err := out.Flush(); err != nil && isEPIPE(err) {
		return 0
	}
	return code
}

// flushOnNewline flushes after every write that ends a line, so streaming
// commands (watch) deliver each event immediately (go spec §7.5) while bulk
// listings still benefit from buffering within a line batch.
type flushOnNewline struct{ w *bufio.Writer }

func (f *flushOnNewline) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil {
		return n, err
	}
	if len(p) > 0 && p[len(p)-1] == '\n' && f.w.Buffered() > 0 {
		if ferr := f.w.Flush(); ferr != nil {
			return n, ferr
		}
	}
	return n, nil
}

func isEPIPE(err error) bool {
	return errors.Is(err, syscall.EPIPE) || strings.Contains(err.Error(), "broken pipe")
}

// Run parses argv, runs the command and maps failures to exit codes.
func Run(argv []string, st Streams) (code int) {
	format := sniffFormat(argv)
	p := NewParser()
	res, err := p.Parse(argv)
	if err != nil {
		var help *argp.HelpRequest
		if errors.As(err, &help) {
			fmt.Fprint(st.Out, help.Text)
			return 0
		}
		return reportError(st, format, err, p, argv)
	}
	c, err := newCtx(res, st)
	if err != nil {
		return reportError(st, format, err, p, argv)
	}
	format = c.Format
	h, ok := res.Cmd.Run.(Handler)
	if !ok {
		fmt.Fprint(st.Err, p.Help(res.Path))
		return errs.ExitError
	}
	if c.idemKey != "" && !idemVerbs[strings.Join(res.Path, " ")] {
		return reportError(st, format, errs.Usage("--idem is only accepted by create verbs (task add, sub add, log add, note, ...), not %s", strings.Join(res.Path, " ")), p, argv)
	}
	if needsNoDB[strings.Join(res.Path, " ")] {
		return finish(st, format, h(c), c.DBPath, p, argv)
	}
	d, err := db.Open(c.DBPath)
	if err != nil {
		return reportError(st, format, dbErr(err), p, argv, c.DBPath)
	}
	defer d.Close()
	c.S = store.New(d, c.Actor)
	if err := c.checkActor(res); err != nil {
		return reportError(st, format, err, p, argv, c.DBPath)
	}
	c.S.Session = c.Session
	c.S.Context = c.Context
	c.S.CwdRepo = cwdRepo
	if c.idemKey != "" {
		c.idemSHA = requestSHA(res.Path, res.Values, c.Actor, c.Context)
		rc, hit, err := c.S.CheckReceipt(c.idemKey, c.idemSHA)
		if err != nil {
			return reportError(st, format, err, p, argv, c.DBPath)
		}
		if hit {
			c.receipt = rc
		}
	}
	return finish(st, format, h(c), c.DBPath, p, argv)
}

// needsNoDB are commands that explain syntax without opening a database
// (design §4.9: describe must not create a database to explain itself).
var needsNoDB = map[string]bool{"grammar": true, "describe": true,
	"skill install": true, "skill status": true, "skill uninstall": true, "skill print": true}

func finish(st Streams, format string, err error, dbPath string, p *argp.Parser, argv []string) int {
	if err == nil {
		return 0
	}
	return reportError(st, format, err, p, argv, dbPath)
}

func dbErr(err error) error {
	if _, ok := errs.As(err); ok {
		return err
	}
	return &store.DBError{Err: err}
}

func newCtx(res *argp.Result, st Streams) (*Ctx, error) {
	c := &Ctx{V: res.Values, Given: res.Given, Path: res.Path, Out: st.Out, Err: st.Err}
	c.DBPath = strOr(res.Values["db"])
	c.Context = strOr(res.Values["context"])
	c.Extended = boolOr(res.Values["extended"])
	c.Quiet = boolOr(res.Values["quiet"])
	c.Header = !boolOr(res.Values["no_header"])
	c.Format = strOr(res.Values["format"])
	if c.Format == "" {
		c.Format = os.Getenv("AGTRACK_FORMAT")
	}
	if c.Format == "" {
		c.Format = "table"
	}
	valid := false
	for _, f := range render.Formats {
		valid = valid || f == c.Format
	}
	if !valid {
		return nil, errs.New("unknown format: %s", c.Format)
	}
	if fields := strOr(res.Values["fields"]); fields != "" {
		for _, f := range strings.Split(fields, ",") {
			if f = strings.TrimSpace(f); f != "" {
				c.Fields = append(c.Fields, f)
			}
		}
	}
	switch {
	case boolOr(res.Values["no_color"]):
		f := false
		c.Color = &f
	case boolOr(res.Values["color"]):
		t := true
		c.Color = &t
	default:
		auto := st.ColorAuto
		c.Color = &auto
	}
	c.Actor = strOr(res.Values["actor"])
	if c.Actor == "" {
		c.Actor = envOr("AGTRACK_ACTOR", "AGENT")
	}
	c.ActorGiven = c.Actor != ""
	c.Session, c.SessionSource = detectSession(strOr(res.Values["session"]))
	if n, ok := res.Values["max_bytes"].(int); ok {
		c.MaxBytes = n
	}
	c.idemKey = strOr(res.Values["idem"])
	return c, nil
}

func strOr(v any) string {
	s, _ := v.(string)
	return s
}

func boolOr(v any) bool {
	b, _ := v.(bool)
	return b
}

// sniffFormat finds the requested format before (or without) a successful
// parse, so even usage errors honour -f json (D11).
func sniffFormat(argv []string) string {
	format := os.Getenv("AGTRACK_FORMAT")
	isFormatFlag := func(name string) bool { // --form and longer prefixes are unambiguous
		return len(name) >= len("--form") && strings.HasPrefix("--format", name)
	}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			break
		}
		name, value, hasValue := strings.Cut(a, "=")
		switch {
		case a == "-f" || (isFormatFlag(name) && !hasValue):
			if i+1 < len(argv) {
				format = argv[i+1]
			}
		case isFormatFlag(name) && hasValue:
			format = value
		case strings.HasPrefix(a, "-f") && len(a) > 2 && !strings.HasPrefix(a, "--"):
			format = strings.TrimPrefix(a[2:], "=")
		}
	}
	return format
}

// reportError prints a failure — prose, or a structured object on stderr
// under json/yaml/jsonl (D4) — and returns its exit code.
func reportError(st Streams, format string, err error, p *argp.Parser, argv []string, dbPath ...string) int {
	if err == nil {
		return 0
	}
	if isEPIPE(err) {
		return 0
	}
	var ec exitCode
	if errors.As(err, &ec) {
		return int(ec)
	}
	var dbe *store.DBError
	path := ""
	if len(dbPath) > 0 {
		path = dbPath[0]
	}
	structured := format == "json" || format == "yaml" || format == "jsonl"
	if errors.As(err, &dbe) || isSQLiteError(err) {
		resolved := db.ResolvePath(path)
		msg := err.Error()
		tok := errs.TokDatabase
		if strings.Contains(msg, "database is locked") || strings.Contains(msg, "SQLITE_BUSY") {
			tok = errs.TokDatabaseBusy
		}
		hint := "set AGTRACK_DB or pass --db to use a reachable location (WAL needs write access to the directory, not just the file)"
		if tok == errs.TokDatabaseBusy {
			// Contention is not fixed by another database (design §4.3).
			hint = "another agtrack process held the write lock for over 5s; retry the command"
		}
		if structured {
			obj := row.Of("error", tok, "code", int64(errs.ExitDatabase), "message", "database error: "+msg, "database", resolved, "hint", hint)
			emitErr(st.Err, format, obj)
			return errs.ExitDatabase
		}
		if tok == errs.TokDatabaseBusy {
			fmt.Fprintf(st.Err, "agtrack: database error: %s\nagtrack: database path: %s\nagtrack: %s\n", msg, resolved, hint)
			return errs.ExitDatabase
		}
		fmt.Fprintf(st.Err, "agtrack: database error: %s\nagtrack: database path: %s\n"+
			"agtrack: set AGTRACK_DB or pass --db to use a reachable location (WAL needs write access to the directory, not just the file)\n",
			msg, resolved)
		return errs.ExitDatabase
	}
	e, ok := errs.As(err)
	if !ok {
		e = &errs.Error{Code: errs.ExitError, Token: errs.TokError, Message: err.Error()}
	}
	if structured {
		obj := row.Of("error", e.Token, "code", int64(e.Code), "message", e.Message)
		if e.Entity != "" {
			obj.Set("entity", e.Entity)
		}
		if e.Ref != "" {
			obj.Set("ref", e.Ref)
		}
		for k, v := range e.Extra {
			obj.Set(k, v)
		}
		emitErr(st.Err, format, obj)
		return e.Code
	}
	if e.Code == errs.ExitUsage && p != nil {
		path, _ := argp.ErrorPath(err)
		fmt.Fprintln(st.Err, p.UsageLine(path))
		prog := strings.TrimSpace("agtrack " + strings.Join(path, " "))
		fmt.Fprintf(st.Err, "%s: error: %s\n", prog, e.Message)
		return e.Code
	}
	fmt.Fprintf(st.Err, "agtrack: %s\n", e.Message)
	return e.Code
}

func emitErr(w io.Writer, format string, obj *row.Row) {
	if format == "yaml" {
		fmt.Fprint(w, render.ToYAML(obj))
		return
	}
	w.Write(render.JSONCompact(obj))
	fmt.Fprintln(w)
}

func isSQLiteError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_") || strings.Contains(msg, "unable to open database") ||
		strings.Contains(msg, "database is locked") || strings.Contains(msg, "disk I/O error")
}

// sessionEnv lists where a session id comes from, in order (agent-features
// spec B1): agtrack's own variable, then what the harnesses export.
var sessionEnv = []string{"AGTRACK_SESSION", "CLAUDE_CODE_SESSION_ID", "CODEX_SESSION_ID", "CODEX_THREAD_ID"}

// detectSession returns the session id writes are stamped with and its source.
func detectSession(flag string) (id, source string) {
	if flag = strings.TrimSpace(flag); flag != "" {
		return flag, "--session"
	}
	for _, k := range sessionEnv {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v, "$" + k
		}
	}
	return "", ""
}
