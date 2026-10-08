package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/render"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

// Ctx is one invocation: parsed values, global options, the store and the
// output streams. Handlers receive it and never touch SQL.
type Ctx struct {
	S     *store.Store
	V     map[string]any
	Given map[string]bool
	Path  []string
	Out   io.Writer
	Err   io.Writer

	DBPath   string
	Context  string
	Format   string
	Fields   []string
	Extended bool
	Header   bool
	Color    *bool
	Quiet    bool
	Actor    string
	// Session is the harness session writes are stamped with (B1);
	// SessionSource names where it came from, for `info`.
	Session, SessionSource string
	MaxBytes int
	Noun     string // for shared note/tag verbs: the noun in play
	idem
}

// --- value accessors -----------------------------------------------------------

func (c *Ctx) Str(k string) string {
	switch v := c.V[k].(type) {
	case string:
		return v
	case []string:
		return strings.Join(v, " ")
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func (c *Ctx) Has(k string) bool {
	v, ok := c.V[k]
	if !ok || v == nil {
		return false
	}
	switch x := v.(type) {
	case string:
		return x != ""
	case []string:
		return len(x) > 0
	case bool:
		return x
	}
	return true
}

func (c *Ctx) Strs(k string) []string {
	switch v := c.V[k].(type) {
	case []string:
		return v
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	}
	return nil
}

func (c *Ctx) Bool(k string) bool {
	b, _ := c.V[k].(bool)
	return b
}

func (c *Ctx) Int(k string) int {
	switch v := c.V[k].(type) {
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

// Tags splits every --tag value on commas (`--tag a,b --tag c`).
func (c *Ctx) Tags(k string) []string {
	var out []string
	for _, v := range c.Strs(k) {
		for _, part := range strings.Split(v, ",") {
			if strings.TrimSpace(part) != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// When parses a backfill flag, if given.
func (c *Ctx) When(k string) (string, error) {
	v := c.Str(k)
	if v == "" {
		return "", nil
	}
	return refs.ParseWhen(v, "--"+k)
}

func (c *Ctx) structured() bool {
	return c.Format == "json" || c.Format == "yaml" || c.Format == "jsonl"
}

// --- output ------------------------------------------------------------------------

func (c *Ctx) println(a ...any) { fmt.Fprintln(c.Out, a...) }
func (c *Ctx) printf(format string, a ...any) {
	fmt.Fprintf(c.Out, format, a...)
}

// derived are display duplicates dropped from structured output (D12).
var derived = map[string][]string{
	"task":    {"created", "updated", "started", "closed", "mark", "path", "context"},
	"subtask": {"created", "updated", "mark"},
	"note":    {"created"},
	"pr":      {"created"},
	"project": {"path"},
	"event":   {"when", "date", "recorded"},
	"context": {},
	"tag":     {},
	"spec":    {},
}

func (c *Ctx) lean(obj any, kind string) any {
	if len(c.Fields) > 0 {
		return obj
	}
	return leanKind(obj, kind)
}

// nestedKind names the entity kind of a nested collection, so each level
// drops its own derived duplicates (D12), not its parent's.
var nestedKind = map[string]string{
	"projects": "project", "subprojects": "project", "note_list": "note", "notes": "note",
	"replies": "note", "subtask_list": "subtask", "subtasks": "subtask", "open_subtasks": "subtask",
	"pr_list": "pr", "events": "event", "tasks": "task", "in_progress": "task", "contexts": "context",
}

func leanKind(obj any, kind string) any {
	switch v := obj.(type) {
	case []*row.Row:
		out := make([]*row.Row, len(v))
		for i, r := range v {
			out[i] = leanKind(r, kind).(*row.Row)
		}
		return out
	case *row.Row:
		drop := map[string]bool{}
		for _, k := range derived[kind] {
			drop[k] = true
		}
		out := row.New()
		for _, k := range v.Keys() {
			if drop[k] {
				continue
			}
			val := v.Get(k)
			if nk, ok := nestedKind[k]; ok {
				if rows, isRows := val.([]*row.Row); isRows {
					val = leanKind(rows, nk)
				}
			}
			out.Set(k, val)
		}
		return out
	}
	return obj
}

// emitObject prints one structured object in the selected structured format.
func (c *Ctx) emitObject(obj any) error {
	f := c.Format
	if !c.structured() {
		f = "json"
	}
	return render.EmitObject(c.Out, obj, f)
}

// fieldsFor are the default column sets; -x appends the second list.
var fieldsFor = map[string][2][]string{
	"context":  {{"name", "current", "projects", "open_tasks"}, {"title", "archived", "created_at"}},
	"project":  {{"ref", "tasks", "open", "repos", "tags"}, {"title", "path", "archived", "created_at"}},
	"task":     {{"id", "status", "project", "title", "subtasks", "tags"}, {"prs", "notes", "started", "closed", "updated", "context"}},
	"subtask":  {{"ref", "mark", "status", "title", "tags"}, {"notes", "created", "updated"}},
	"tag":      {{"name", "projects", "tasks", "subtasks", "notes"}, {"created_at"}},
	"pr":       {{"id", "state", "repo", "number", "task_id", "task_title"}, {"url", "created"}},
	"event":    {{"when", "actor", "target", "action", "detail"}, {"task", "target_type", "recorded"}},
	"note":     {{"id", "created", "actor", "body"}, {"kind", "resolved_at"}},
	"spec":     {{"slug", "revision", "status", "source_path", "title"}, {"project", "sha", "updated_at"}},
	"dep":      {{"task_id", "depends_on_id", "kind", "status", "title"}, {"reason", "created_at"}},
	"artifact": {{"id", "kind", "value", "label"}, {"actor", "created_at"}},
}

// emit renders rows honouring --count/--ids/-q/--fields/-f/-x. total is the
// number of matches before any --limit (D3).
func (c *Ctx) emit(rows []*row.Row, kind string, total int) error {
	if c.Bool("count") {
		c.println(total)
		return nil
	}
	base := fieldsFor[kind]
	fields := append([]string{}, base[0]...)
	if c.Extended {
		fields = append(fields, base[1]...)
	}
	restrict := false
	if len(c.Fields) > 0 {
		var err error
		if fields, err = render.SelectFields(rows, c.Fields); err != nil {
			return err
		}
		restrict = true
	}
	if c.Bool("ids") || c.Quiet {
		key := "id"
		switch kind {
		case "subtask":
			key = "ref"
		case "context", "tag":
			key = "name"
			if kind == "tag" {
				key = "id"
			}
		case "spec":
			key = "slug"
		}
		for _, r := range rows {
			c.println(render.Cell(r.Get(key)))
		}
		return nil
	}
	if c.structured() && !restrict {
		leaned := make([]*row.Row, len(rows))
		for i, r := range rows {
			leaned[i] = c.lean(r, kind).(*row.Row)
		}
		rows = leaned
	}
	return render.Emit(c.Out, rows, fields, render.Options{
		Format: c.Format, Header: c.Header, Color: c.Color, Restrict: restrict, MaxBytes: c.MaxBytes, Err: c.Err,
	})
}

// detail prints a `show` result: structured when asked, labelled lines otherwise.
func (c *Ctx) detail(obj *row.Row, kind string, sections []render.Section) error {
	if c.structured() {
		if len(c.Fields) > 0 {
			fields, err := render.SelectFields([]*row.Row{obj}, c.Fields)
			if err != nil {
				return err
			}
			return c.emitObject(render.ProjectRows([]*row.Row{obj}, fields)[0])
		}
		return c.emitObject(c.lean(obj, kind))
	}
	if len(c.Fields) > 0 {
		fields, err := render.SelectFields([]*row.Row{obj}, c.Fields)
		if err != nil {
			return err
		}
		var secs []render.Section
		for _, f := range fields {
			secs = append(secs, render.Section{Label: f, Value: obj.Get(f)})
		}
		return render.EmitDetail(c.Out, secs, c.Color)
	}
	return render.EmitDetail(c.Out, sections, c.Color)
}

// created reports a create: a bare identifier under -q, the created object
// under a structured format (D11), `label value` otherwise.
func (c *Ctx) created(label string, value any, obj *row.Row, kind string) error {
	switch {
	case c.Quiet:
		c.println(value)
	case c.structured():
		if obj == nil {
			obj = row.Of(label, value)
		}
		return c.emitObject(c.lean(obj, kind))
	default:
		c.println(label, value)
	}
	return nil
}

// mutated reports a mutation: nothing under -q, obj under a structured
// format (D11), text otherwise.
func (c *Ctx) mutated(text string, obj any) error {
	switch {
	case c.Quiet:
	case c.structured():
		if obj == nil {
			obj = row.Of("ok", true, "message", text)
		}
		return c.emitObject(obj)
	default:
		c.println(text)
	}
	return nil
}

// --- shared resolution -------------------------------------------------------------

// cwdRepo is the origin remote of the repository containing the cwd.
func cwdRepo() string {
	for _, args := range [][]string{
		{"git", "remote", "get-url", "origin"},
		{"git", "config", "--get", "remote.origin.url"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Stderr = nil
		done := make(chan struct{})
		var out []byte
		var err error
		go func() { out, err = cmd.Output(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
			return ""
		}
		if err == nil && strings.TrimSpace(string(out)) != "" {
			return strings.TrimSpace(string(out))
		}
		if _, ok := err.(*exec.Error); ok {
			return ""
		}
	}
	return ""
}

// resolveProject resolves a project ref, `.` (the cwd repo), or the pin.
func (c *Ctx) resolveProject(ref string, required bool) (*row.Row, error) {
	if ref == "" {
		if required {
			return c.S.DefaultProject()
		}
		return c.S.CurrentProject()
	}
	if ref == "." {
		remote := c.S.CwdRepo()
		if remote == "" {
			return nil, errs.New("not inside a git repository with an origin remote")
		}
		matches, err := c.S.FindProjectsByRepo(remote)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			id, _ := refs.RepoIdentity(remote)
			return nil, errs.NotFound("project", ".", "no project linked to %s; link one with `agtrack project repo add <ref> <url>`", id)
		}
		if len(matches) > 1 {
			var paths []string
			for _, m := range matches {
				p, _ := c.S.ProjectPath(m)
				paths = append(paths, p)
			}
			e := errs.New("%d projects share that remote: %s", len(matches), strings.Join(paths, ", "))
			e.Token = errs.TokAmbiguous
			e.Extra = map[string]any{"candidates": paths}
			return nil, e
		}
		return matches[0], nil
	}
	return c.S.ResolveProject(ref, c.Context, required)
}

func (c *Ctx) statusArg(value, group string) (string, error) {
	st, err := refs.NormalizeStatus(value)
	if err == nil {
		return st, nil
	}
	refsGiven := c.Strs("ref")
	if refRE(strings.TrimSpace(value)) && len(refsGiven) > 0 {
		return "", errs.New("%s looks like a reference, not a status: this verb takes the status first, as `agtrack %s status %s %s`",
			value, group, refsGiven[0], value)
	}
	return "", err
}

func refRE(s string) bool {
	s = strings.TrimPrefix(s, "#")
	if s == "" {
		return false
	}
	dot := false
	prevDigit := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			prevDigit = true
		case r == '.' && !dot && prevDigit:
			dot = true
			prevDigit = false
		default:
			return false
		}
	}
	return prevDigit
}

func guardChildren(force bool, counts *row.Row, label string) error {
	if force {
		return nil
	}
	var carried []string
	for _, k := range counts.Keys() {
		if n := counts.Int(k); n > 0 {
			carried = append(carried, fmt.Sprintf("%d %s", n, k))
		}
	}
	if len(carried) > 0 {
		return errs.Conflict("%s still holds %s; pass --force to delete it all, or cancel the item instead of deleting it",
			label, strings.Join(carried, ", "))
	}
	return nil
}

func envOr(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
