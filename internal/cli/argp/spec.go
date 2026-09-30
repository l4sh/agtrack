package argp

import (
	"fmt"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
)

// The compiled form of one command: argparse's action list for its parser.

type actKind int

const (
	actFlag actKind = iota
	actPos
	actSub
	actHelp
	actVersion
)

// Internal nargs values, mirroring argparse.
const (
	nargsZero   = "0"      // store_true, help, version
	nargsOne    = ""       // exactly one
	nargsOpt    = "?"      // zero or one (positional only)
	nargsStar   = "*"      // zero or more (positional only)
	nargsPlus   = "+"      // one or more
	nargsParser = "PARSER" // subcommand slot: name then everything after it
)

type action struct {
	kind     actKind
	flag     Flag // actFlag
	arg      Arg  // actPos
	opts     []string
	dest     string
	nargs    string
	choices  []string
	suppress bool   // no default: only present when given (globals)
	metavar  string // actSub
}

// name is argparse's _get_action_name: "-n/--limit", "ref", "<verb>".
func (a *action) name() string {
	switch {
	case len(a.opts) > 0:
		return strings.Join(a.opts, "/")
	case a.metavar != "":
		return a.metavar
	default:
		return a.dest
	}
}

type spec struct {
	cmd         *Command
	path        []string // canonical
	actions     []*action
	optMap      map[string]*action
	optOrder    []string // option strings in registration order
	positionals []*action
	sub         *action
	negOpts     bool // some option string looks like a negative number
}

// Validate checks the command table for mistakes argparse would reject at
// build time: duplicate option strings, duplicate subcommand names, flags
// without a name. Parse calls it on first use.
func (p *Parser) Validate() error {
	if p.Root == nil {
		return fmt.Errorf("argp: parser has no root command")
	}
	var err error
	p.Walk(func(path []string, c *Command) {
		if err == nil {
			_, err = p.spec(path, c)
		}
	})
	return err
}

func (p *Parser) spec(path []string, c *Command) (*spec, error) {
	if s, ok := p.built[c]; ok {
		return s, nil
	}
	s, err := p.compile(path, c)
	if err != nil {
		return nil, err
	}
	if p.built == nil {
		p.built = map[*Command]*spec{}
	}
	p.built[c] = s
	return s, nil
}

func (p *Parser) compile(path []string, c *Command) (*spec, error) {
	where := p.prog()
	if len(path) > 0 {
		where += " " + strings.Join(path, " ")
	}
	s := &spec{cmd: c, path: path, optMap: map[string]*action{}}
	add := func(a *action) error {
		for _, o := range a.opts {
			if _, dup := s.optMap[o]; dup {
				return fmt.Errorf("argp: %s: conflicting option string: %s", where, o)
			}
			if !strings.HasPrefix(o, "-") || len(o) < 2 {
				return fmt.Errorf("argp: %s: invalid option string: %q", where, o)
			}
		}
		for _, o := range a.opts {
			s.optMap[o] = a
			s.optOrder = append(s.optOrder, o)
			if negNumber(o) {
				s.negOpts = true
			}
		}
		s.actions = append(s.actions, a)
		return nil
	}
	flag := func(f Flag, global bool) error {
		if f.Long == "" && f.Short == "" {
			return fmt.Errorf("argp: %s: flag without a name", where)
		}
		a := &action{kind: actFlag, flag: f, dest: DestFor(f), choices: f.Choices, suppress: global}
		for _, o := range []string{f.Short, f.Long} {
			if o != "" {
				a.opts = append(a.opts, o)
			}
		}
		switch f.Kind {
		case Bool:
			a.nargs = nargsZero
		case NArgsPlus:
			a.nargs = nargsPlus
		default:
			a.nargs = nargsOne
		}
		return add(a)
	}

	// argparse order: -h/--help, the parent (global) flags, -V at the root,
	// then the command's own arguments.
	if err := add(&action{kind: actHelp, opts: []string{"-h", "--help"}, nargs: nargsZero}); err != nil {
		return nil, err
	}
	for _, f := range p.Globals {
		if err := flag(f, true); err != nil {
			return nil, err
		}
	}
	if c == p.Root && p.Version != "" {
		if err := add(&action{kind: actVersion, opts: []string{"-V", "--version"}, nargs: nargsZero}); err != nil {
			return nil, err
		}
	}
	for _, f := range c.Flags {
		if err := flag(f, false); err != nil {
			return nil, err
		}
	}
	for _, a := range c.Args {
		switch a.NArgs {
		case nargsOne, nargsOpt, nargsStar, nargsPlus:
		default:
			return nil, fmt.Errorf("argp: %s: argument %s: bad NArgs %q", where, a.Name, a.NArgs)
		}
		pa := &action{kind: actPos, arg: a, dest: a.Name, nargs: a.NArgs, choices: a.Choices}
		s.actions = append(s.actions, pa)
		s.positionals = append(s.positionals, pa)
	}
	if len(c.Subs) > 0 {
		seen := map[string]bool{}
		var choices []string
		for _, sc := range c.Subs {
			for _, n := range append([]string{sc.Name}, sc.Aliases...) {
				if seen[n] {
					return nil, fmt.Errorf("argp: %s: conflicting subcommand: %s", where, n)
				}
				seen[n] = true
				if !sc.Hidden {
					choices = append(choices, n)
				}
			}
		}
		sa := &action{kind: actSub, nargs: nargsParser, choices: choices, metavar: subMetavar(c, c == p.Root)}
		s.actions = append(s.actions, sa)
		s.positionals = append(s.positionals, sa)
		s.sub = sa
	}
	return s, nil
}

func subMetavar(c *Command, root bool) string {
	switch {
	case c.SubMetavar != "":
		return c.SubMetavar
	case root:
		return "<command>"
	case c.SubRequired:
		return "<verb>"
	default:
		return "[<verb>]"
	}
}

// required reports whether argparse would list the action as required.
func (a *action) required(c *Command) bool {
	switch a.kind {
	case actPos:
		return a.nargs == nargsOne || a.nargs == nargsPlus
	case actSub:
		return c.SubRequired
	}
	return false
}

func usageErr(path []string, format string, args ...any) *errs.Error {
	e := errs.Usage(format, args...)
	e.Extra = map[string]any{"command": strings.Join(path, " ")}
	return e
}

func asErr(err error) (*errs.Error, bool) { return errs.As(err) }
