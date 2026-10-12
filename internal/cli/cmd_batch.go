package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/l4sh/agtrack/internal/cli/argp"
	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/render"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

// `agtrack batch` (agent-features E2): many operations, one process, one
// transaction. Each line of the input is one op, run in-process through the
// same path as the browse backend (inproc), so validation, events,
// attribution and --idem receipts are the CLI's own.
//
// Transactions: the whole batch is one write transaction. Without
// --continue the first failing op rolls back everything. With --continue
// each op runs in its own savepoint: a failing op undoes only itself and
// the rest still commit together at the end.

// batchRefused verbs block or serve, or would nest a batch.
var batchRefused = map[string]bool{
	"watch": true, "await": true, "serve": true, "batch": true,
	"spec serve": true, "spec watch": true, "spec await": true,
}

// batchForbidden are global flags an op may not set: the batch invocation's
// own apply to every op, and --db in particular must not be redirected.
var batchForbidden = []string{"db", "actor", "session", "context"}

func batchVet(res *argp.Result) error {
	if p := strings.Join(res.Path, " "); batchRefused[p] {
		return errs.Usage("%s blocks or serves and cannot run inside a batch", p)
	}
	for _, k := range batchForbidden {
		if res.Given[k] {
			return errs.Usage("--%s is set by the batch invocation and cannot be overridden by an op", k)
		}
	}
	return nil
}

type batchOp struct {
	line int
	argv []string
	err  error
}

func cmdBatch(c *Ctx) error {
	var in io.Reader = c.stdin()
	if f := c.Str("file"); f != "" {
		fh, err := os.Open(f)
		if err != nil {
			return errs.Usage("cannot read %s: %v", f, err)
		}
		defer fh.Close()
		in = fh
	}
	ops, err := readBatchOps(in)
	if err != nil {
		return err
	}
	cont := c.Bool("continue")
	r := &inproc{db: c.S.DB, actor: c.S.Actor, session: c.Session, context: c.Context,
		cwdRepo: c.S.CwdRepo, checkActor: true, pre: []string{"--format=json"}, vet: batchVet}

	var lines []*row.Row
	var first error
	rolledBack := false
	err = c.S.Tx(func() error {
		for _, op := range ops {
			var res any
			opErr := op.err
			if opErr == nil {
				run := func() (e error) { res, e = r.run(nil, op.argv...); return e }
				if cont {
					opErr = c.S.Savepoint(run)
				} else {
					opErr = run()
				}
			}
			if opErr != nil {
				if first == nil {
					first = opErr
				}
				lines = append(lines, batchErrLine(op.line, opErr))
				if !cont {
					return opErr // roll everything back
				}
				continue
			}
			lines = append(lines, row.Of("ok", true, "op", int64(op.line), "result", res))
		}
		return nil
	})
	if err != nil && first == nil {
		first = err // commit failure
		lines = append(lines, batchErrLine(0, err))
	}
	if err != nil && !cont {
		rolledBack = true
	}
	for _, l := range lines {
		if rolledBack && l.Get("ok") == true {
			l.Set("rolled_back", true)
		}
		c.Out.Write(render.JSONCompact(l))
		fmt.Fprintln(c.Out)
	}
	if first == nil {
		return nil
	}
	if e, ok := errs.As(first); ok {
		return exitCode(e.Code)
	}
	if isSQLiteError(first) {
		return exitCode(errs.ExitDatabase)
	}
	return exitCode(errs.ExitError)
}

// stdin is split out so tests can feed it.
func (c *Ctx) stdin() io.Reader {
	if c.In != nil {
		return c.In
	}
	return os.Stdin
}

func batchErrLine(line int, err error) *row.Row {
	l := row.Of("ok", false)
	if line > 0 {
		l.Set("op", int64(line))
	}
	var dbe *store.DBError
	e, ok := errs.As(err)
	switch {
	case ok:
		l.Set("error", e.Token).Set("code", int64(e.Code)).Set("message", e.Message)
		if e.Entity != "" {
			l.Set("entity", e.Entity)
		}
		if e.Ref != "" {
			l.Set("ref", e.Ref)
		}
		keys := make([]string, 0, len(e.Extra))
		for k := range e.Extra {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			l.Set(k, e.Extra[k])
		}
	case isSQLiteError(err) || asDBError(err, &dbe):
		l.Set("error", errs.TokDatabase).Set("code", int64(errs.ExitDatabase)).Set("message", "database error: "+err.Error())
	default:
		l.Set("error", errs.TokError).Set("code", int64(errs.ExitError)).Set("message", err.Error())
	}
	return l
}

func asDBError(err error, target **store.DBError) bool {
	d, ok := err.(*store.DBError)
	if ok {
		*target = d
	}
	return ok
}

// readBatchOps parses the input. A bad line becomes an op that fails when
// reached, so --continue can step over it.
func readBatchOps(in io.Reader) ([]batchOp, error) {
	var ops []batchOp
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	p := NewParser()
	n := 0
	for sc.Scan() {
		n++
		text := bytes.TrimSpace(sc.Bytes())
		if len(text) == 0 {
			continue
		}
		op := batchOp{line: n}
		op.argv, op.err = parseBatchLine(p, text)
		ops = append(ops, op)
	}
	if err := sc.Err(); err != nil {
		return nil, errs.Usage("cannot read batch input: %v", err)
	}
	if len(ops) == 0 {
		return nil, errs.Usage("no operations: give JSON lines on stdin or with --file")
	}
	return ops, nil
}

func parseBatchLine(p *argp.Parser, text []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(text))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	var o struct {
		Argv []string       `json:"argv"`
		Op   string         `json:"op"`
		Args map[string]any `json:"args"`
	}
	if err := dec.Decode(&o); err != nil {
		return nil, errs.Usage("not a valid op line: %v", err)
	}
	switch {
	case o.Argv != nil && o.Op != "":
		return nil, errs.Usage("an op gives argv or op/args, not both")
	case o.Argv != nil:
		return o.Argv, nil
	case o.Op != "":
		return opArgv(p, strings.Fields(o.Op), o.Args)
	}
	return nil, errs.Usage(`an op needs "argv" or "op"`)
}

// opArgv maps {"op": "sub done", "args": {...}} onto an argv using the
// command's own describe metadata: a key naming a positional fills it;
// a key naming a flag (its dest, or its long name with or without dashes)
// sets it. Bool flags take true/false, append flags take a list, nargs_plus
// flags a list or a string. Positionals go after `--`.
func opArgv(p *argp.Parser, path []string, args map[string]any) ([]string, error) {
	if len(path) == 0 {
		return nil, errs.Usage(`"op" is empty`)
	}
	sch := p.Describe(path)
	if sch == nil {
		return nil, errs.Usage("no such command: %s", strings.Join(path, " "))
	}
	norm := func(s string) string { return strings.ReplaceAll(strings.TrimLeft(s, "-"), "-", "_") }
	flags := map[string]argp.FlagSchema{}
	for _, f := range sch.Flags {
		flags[f.Dest] = f
		flags[norm(f.Long)] = f
	}
	for _, g := range p.Globals { // describe hides these below the root
		if _, ok := flags[argp.DestFor(g)]; !ok {
			fs := argp.FlagSchema{Long: g.Long, Dest: argp.DestFor(g), Kind: g.Kind.String()}
			flags[fs.Dest] = fs
			flags[norm(g.Long)] = fs
		}
	}
	pos := map[string]bool{}
	for _, a := range sch.Args {
		pos[a.Name] = true
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	argv := append([]string{}, path...)
	posVals := map[string][]string{}
	for _, k := range keys {
		v := args[k]
		if pos[k] {
			vals, err := batchStrings(k, v)
			if err != nil {
				return nil, err
			}
			posVals[k] = vals
			continue
		}
		f, ok := flags[norm(k)]
		if !ok {
			return nil, errs.Usage("%s has no argument or flag %q", sch.Command, k)
		}
		switch f.Kind {
		case "bool":
			b, isBool := v.(bool)
			if !isBool {
				return nil, errs.Usage("%s: --%s takes true or false", sch.Command, norm(k))
			}
			if b {
				argv = append(argv, f.Long)
			}
		case "append":
			vals, err := batchStrings(k, v)
			if err != nil {
				return nil, err
			}
			for _, s := range vals {
				argv = append(argv, f.Long+"="+s)
			}
		case "nargs_plus":
			vals, err := batchStrings(k, v)
			if err != nil {
				return nil, err
			}
			argv = append(argv, f.Long)
			argv = append(argv, vals...)
		default:
			vals, err := batchStrings(k, v)
			if err != nil || len(vals) != 1 {
				return nil, errs.Usage("%s: --%s takes one value", sch.Command, norm(k))
			}
			argv = append(argv, f.Long+"="+vals[0])
		}
	}
	var tail []string
	for _, a := range sch.Args {
		tail = append(tail, posVals[a.Name]...)
	}
	if len(tail) > 0 {
		argv = append(argv, "--")
		argv = append(argv, tail...)
	}
	return argv, nil
}

// batchStrings renders a JSON scalar or list of scalars as argv strings.
func batchStrings(key string, v any) ([]string, error) {
	scalar := func(x any) (string, error) {
		switch s := x.(type) {
		case string:
			return s, nil
		case json.Number:
			return s.String(), nil
		case bool:
			return fmt.Sprint(s), nil
		}
		return "", errs.Usage("%q: values must be strings, numbers or lists of them", key)
	}
	if l, ok := v.([]any); ok {
		out := make([]string, 0, len(l))
		for _, x := range l {
			s, err := scalar(x)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, nil
	}
	s, err := scalar(v)
	if err != nil {
		return nil, err
	}
	return []string{s}, nil
}
