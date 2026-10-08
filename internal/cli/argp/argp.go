// Package argp is a small declarative command-line parser that reproduces the
// parts of Python's argparse (3.14) that agtrack's CLI relies on: nested
// subcommands with aliases, global flags accepted at every level, unique
// prefix matching of long flags, short-flag clusters, `--`, negative numbers
// as values and argparse's chunked matching of positionals.
//
// The command table is plain data (Command, Flag, Arg); Parse walks argv
// against it and returns a Result, a *HelpRequest for -h/--version, or a
// usage *errs.Error (exit 2) whose Extra["command"] names the command the
// error belongs to (see ErrorPath and UsageLine).
package argp

import (
	"fmt"
	"strings"
	"sync"
)

// Kind is how a flag consumes and stores its value.
type Kind int

const (
	// String takes exactly one value; value is string (nil when absent).
	// It is the zero value, so a bare Flag{Long: "--note"} is a string flag.
	String Kind = iota
	// Bool is a store_true switch; value is bool.
	Bool
	// Int takes one value parsed like Python int(); value is int.
	Int
	// PositiveInt is an Int that must be 1 or more; value is int.
	PositiveInt
	// Append takes one value per occurrence and accumulates; value is []string.
	Append
	// NArgsPlus consumes one or more following values (--title a b c);
	// value is []string. Repeating it replaces the earlier list.
	NArgsPlus
)

var kindNames = [...]string{"string", "bool", "int", "positive_int", "append", "nargs_plus"}

func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Flag is an optional argument.
type Flag struct {
	Long, Short string // "--project", "-p"; at least one is required
	// Dest is the result key; default derived from Long
	// ("--all-contexts" → "all_contexts"), else from Short.
	Dest    string
	Kind    Kind
	Choices []string
	// Default is the value when the flag is absent. nil means Python's None
	// (false for Bool). Globals never get a default: they appear in
	// Result.Values only when given.
	Default any
	Metavar string // default: upper-cased Dest ("STATUS"); {a,b} when Choices is set
	Help    string
}

// Arg is a positional argument.
type Arg struct {
	Name    string // result key and display name
	NArgs   string // "" (exactly one) | "?" | "*" | "+"
	Choices []string
	Default any // used for "?" (nil) and "*" (nil means empty list)
	Help    string
}

// Command is one node of the command tree: the root, a noun, or a verb.
type Command struct {
	Name    string
	Aliases []string
	Help    string
	Args    []Arg
	Flags   []Flag
	Subs    []*Command
	// SubRequired makes a subcommand mandatory (`task` needs a verb; bare
	// `log` runs its own handler with its own flags).
	SubRequired bool
	// SubMetavar names the subcommand slot in usage and errors. Default:
	// "<command>" at the root, "<verb>" below it, "[<verb>]" when optional.
	SubMetavar string
	Run        any // opaque handler, handed back in Result.Cmd
	Epilog     string
	// Hidden commands still parse but are left out of help, Describe's
	// subcommand lists and invalid-choice messages.
	Hidden bool
	// Output documents the top-level keys of the command's -f json object,
	// for `describe` (agent-features B6).
	Output []OutputKey
}

// OutputKey is one documented top-level key of a command's JSON output.
type OutputKey struct {
	Key  string `json:"key"`
	Help string `json:"help"`
}

// Parser is the entry point: a root command plus the flags every level accepts.
type Parser struct {
	Prog string // program name; default "agtrack"
	Root *Command
	// Globals are accepted at every level, before or after the noun/verb.
	// If given at several levels, the deepest (last) one wins.
	Globals []Flag
	// Version enables -V/--version at the root, printing "<prog> <version>".
	Version string
	// Width is the help text width; default 80 (argparse wraps at width-2).
	Width int

	mu    sync.Mutex
	built map[*Command]*spec // compiled lazily; do not edit the table after first use
}

// Result is a successful parse.
type Result struct {
	Path  []string // canonical names, e.g. ["task", "ls"]
	Typed []string // the spellings as typed, e.g. ["t", "list"]
	Cmd   *Command // deepest matched command
	// Values holds every flag and positional of the matched command (and of
	// the commands above it) with defaults applied; globals only if given.
	// Types: bool, string, int, []string, or nil for Python's None.
	Values map[string]any
	// Given reports which dests were set from the command line.
	Given map[string]bool
}

// HelpRequest is returned by Parse for -h/--help and -V/--version. It is
// not a failure: print Text to stdout and exit 0.
type HelpRequest struct {
	Path    []string // command whose help was requested; nil for --version
	Text    string
	Version bool
}

func (h *HelpRequest) Error() string { return h.Text }

// Str returns a string value, or "" when absent/None.
func (r *Result) Str(dest string) string {
	s, _ := r.Values[dest].(string)
	return s
}

// Strs returns a list value ([]string), or nil when absent/None.
func (r *Result) Strs(dest string) []string {
	s, _ := r.Values[dest].([]string)
	return s
}

// Bool returns a bool value, false when absent.
func (r *Result) Bool(dest string) bool {
	b, _ := r.Values[dest].(bool)
	return b
}

// Int returns an int value and whether one is set (false for None).
func (r *Result) Int(dest string) (int, bool) {
	n, ok := r.Values[dest].(int)
	return n, ok
}

// Has reports whether dest is present and not None.
func (r *Result) Has(dest string) bool {
	v, ok := r.Values[dest]
	return ok && v != nil
}

func (p *Parser) prog() string {
	if p.Prog != "" {
		return p.Prog
	}
	return "agtrack"
}

func (p *Parser) width() int {
	if p.Width > 0 {
		return p.Width
	}
	return 80
}

// Find resolves a path of names or aliases to a command; nil if unknown.
func (p *Parser) Find(path []string) *Command {
	c := p.Root
	for _, name := range path {
		c = c.sub(name)
		if c == nil {
			return nil
		}
	}
	return c
}

// canonical returns the canonical names for a path of names or aliases,
// nil if the path is unknown.
func (p *Parser) canonical(path []string) []string {
	out := make([]string, 0, len(path))
	c := p.Root
	for _, name := range path {
		c = c.sub(name)
		if c == nil {
			return nil
		}
		out = append(out, c.Name)
	}
	return out
}

func (c *Command) sub(name string) *Command {
	for _, s := range c.Subs {
		if s.Name == name {
			return s
		}
		for _, a := range s.Aliases {
			if a == name {
				return s
			}
		}
	}
	return nil
}

// Walk calls fn for every command, parents before children, in definition
// order. The root is visited first with an empty path.
func (p *Parser) Walk(fn func(path []string, c *Command)) {
	var walk func(path []string, c *Command)
	walk = func(path []string, c *Command) {
		fn(path, c)
		for _, s := range c.Subs {
			walk(append(append([]string(nil), path...), s.Name), s)
		}
	}
	walk([]string{}, p.Root)
}

// DestFor derives the result key of a flag the way argparse does.
func DestFor(f Flag) string {
	if f.Dest != "" {
		return f.Dest
	}
	name := f.Long
	if name == "" {
		name = f.Short
	}
	return strings.ReplaceAll(strings.TrimLeft(name, "-"), "-", "_")
}

// ErrorPath returns the command path a usage error belongs to ("task add"
// → ["task", "add"]; the root → []). ok is false for other errors.
func ErrorPath(err error) (path []string, ok bool) {
	e, isErr := asErr(err)
	if !isErr || e.Extra == nil {
		return nil, false
	}
	s, isStr := e.Extra["command"].(string)
	if !isStr {
		return nil, false
	}
	if s == "" {
		return []string{}, true
	}
	return strings.Fields(s), true
}
