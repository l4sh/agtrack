package argp

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// This file is a port of argparse._parse_known_args (CPython 3.14). The
// structure and names follow the original so the two can be read side by
// side: argv is classified into a pattern string ('O' option, 'A' argument,
// '-' the first "--"), then options and positional chunks are consumed
// alternately, positionals by regex over that pattern.

// argError is argparse's ArgumentError: it becomes a usage error stamped
// with the command whose parser raised it.
type argError struct {
	act *action // nil: the message stands alone
	msg string
}

func (e *argError) Error() string {
	if e.act == nil {
		return e.msg
	}
	return "argument " + e.act.name() + ": " + e.msg
}

func argErrf(a *action, format string, args ...any) *argError {
	return &argError{act: a, msg: fmt.Sprintf(format, args...)}
}

// namespace is what one level of parsing produces.
type namespace struct {
	vals  map[string]any
	given map[string]bool
	path  []string // canonical path of the deepest command reached
	typed []string // spellings typed below this level
	cmd   *Command
	unrec []string // unrecognized arguments handed up by subcommands
}

// optTuple is one interpretation of an option-looking token.
type optTuple struct {
	act      *action // nil: looks like an option, but no such option here
	opt      string  // the option string it resolved to
	sep      string  // "=" when the value was attached with '='
	explicit string  // attached value (--x=v, -xv)
	has      bool    // explicit is set (possibly empty)
}

// Parse parses argv (without the program name).
//
// It returns a *HelpRequest for -h/--help/-V/--version, a usage *errs.Error
// (exit 2) for bad input, or a plain error if the command table is invalid.
func (p *Parser) Parse(argv []string) (*Result, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	ns, extras, err := p.parseKnown([]string{}, p.Root, argv)
	if err != nil {
		return nil, err
	}
	if len(extras) > 0 {
		return nil, usageErr([]string{}, "unrecognized arguments: %s", strings.Join(extras, " "))
	}
	return &Result{Path: ns.path, Typed: ns.typed, Cmd: ns.cmd, Values: ns.vals, Given: ns.given}, nil
}

// parseKnown is parse_known_args for one command: a fresh namespace with
// defaults, then the arguments; extras are returned rather than rejected.
func (p *Parser) parseKnown(path []string, c *Command, args []string) (*namespace, []string, error) {
	s, err := p.spec(path, c)
	if err != nil {
		return nil, nil, err
	}
	ns := &namespace{vals: map[string]any{}, given: map[string]bool{}, path: path, typed: []string{}, cmd: c}
	for _, a := range s.actions {
		if a.dest == "" || a.suppress {
			continue
		}
		if _, ok := ns.vals[a.dest]; !ok {
			ns.vals[a.dest] = a.defaultValue()
		}
	}
	extras, err := p.parseArgs(s, ns, args)
	if err != nil {
		var ae *argError
		if errors.As(err, &ae) {
			return nil, nil, usageErr(path, "%s", ae.Error())
		}
		return nil, nil, err
	}
	return ns, append(extras, ns.unrec...), nil
}

func (a *action) defaultValue() any {
	switch a.kind {
	case actFlag:
		if a.flag.Default == nil && a.flag.Kind == Bool {
			return false
		}
		return a.flag.Default
	case actPos:
		return a.arg.Default
	}
	return nil
}

func (p *Parser) parseArgs(s *spec, ns *namespace, args []string) ([]string, error) {
	// Classify every token.
	optIdx := map[int][]optTuple{}
	pat := make([]byte, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			pat[i] = '-'
			for j := i + 1; j < len(args); j++ {
				pat[j] = 'A'
			}
			break
		}
		if tups := s.parseOptional(args[i]); tups != nil {
			optIdx[i] = tups
			pat[i] = 'O'
		} else {
			pat[i] = 'A'
		}
	}
	pattern := string(pat)

	seen := map[*action]bool{}
	positionals := append([]*action(nil), s.positionals...)
	var extras []string

	takeAction := func(a *action, strs []string) error {
		seen[a] = true
		switch a.kind {
		case actHelp:
			return &HelpRequest{Path: s.path, Text: p.Help(s.path)}
		case actVersion:
			return &HelpRequest{Text: p.prog() + " " + p.Version + "\n", Version: true}
		case actSub:
			return p.takeSub(s, ns, strs)
		}
		val, err := getValues(a, strs)
		if err != nil {
			return err
		}
		if a.kind == actFlag {
			switch a.flag.Kind {
			case Bool:
				val = true
			case Append:
				prev, _ := ns.vals[a.dest].([]string)
				val = append(append([]string{}, prev...), val.(string))
			}
			ns.given[a.dest] = true
		} else if len(strs) > 0 {
			ns.given[a.dest] = true
		}
		ns.vals[a.dest] = val
		return nil
	}

	consumeOptional := func(start int) (int, error) {
		tups := optIdx[start]
		if len(tups) > 1 {
			names := make([]string, len(tups))
			for i, t := range tups {
				names[i] = t.opt
			}
			return 0, argErrf(nil, "ambiguous option: %s could match %s", args[start], strings.Join(names, ", "))
		}
		t := tups[0]
		a, opt, sep, explicit, has := t.act, t.opt, t.sep, t.explicit, t.has

		type pending struct {
			a    *action
			strs []string
		}
		var todo []pending
		var stop int
	loop:
		for {
			if a == nil {
				extras = append(extras, args[start])
				return start + 1, nil
			}
			if has {
				count, err := matchArgument(a, "A")
				if err != nil {
					return 0, err
				}
				switch {
				case count == 0 && opt[1] != '-' && explicit != "":
					// -xyz: a zero-argument short option followed by more.
					if sep != "" || explicit[0] == '-' {
						return 0, argErrf(a, "ignored explicit argument %s", pyRepr(explicit))
					}
					todo = append(todo, pending{a, nil})
					r, size := utf8.DecodeRuneInString(explicit)
					opt = "-" + string(r)
					next, ok := s.optMap[opt]
					if !ok {
						extras = append(extras, "-"+explicit)
						stop = start + 1
						break loop
					}
					a, explicit = next, explicit[size:]
					switch {
					case explicit == "":
						sep, has = "", false
					case explicit[0] == '=':
						sep, explicit = "=", explicit[1:]
					default:
						sep = ""
					}
				case count == 1:
					stop = start + 1
					todo = append(todo, pending{a, []string{explicit}})
					break loop
				default:
					return 0, argErrf(a, "ignored explicit argument %s", pyRepr(explicit))
				}
			} else {
				first := start + 1
				count, err := matchArgument(a, pattern[first:])
				if err != nil {
					return 0, err
				}
				stop = first + count
				todo = append(todo, pending{a, args[first:stop]})
				break loop
			}
		}
		for _, pd := range todo {
			if err := takeAction(pd.a, pd.strs); err != nil {
				return 0, err
			}
		}
		return stop, nil
	}

	consumePositionals := func(start int) (int, error) {
		counts := matchPartial(positionals, pattern[start:])
		for i, n := range counts {
			a := positionals[i]
			strs := append([]string(nil), args[start:start+n]...)
			if a.nargs == nargsParser {
				if pattern[start] == '-' {
					strs = removeFirst(strs, "--")
				}
			} else if strings.IndexByte(pattern[start:start+n], '-') >= 0 {
				strs = removeFirst(strs, "--")
			}
			start += n
			if err := takeAction(a, strs); err != nil {
				return 0, err
			}
		}
		positionals = positionals[len(counts):]
		return start, nil
	}

	// Consume positionals and options alternately up to the last option.
	maxOpt := -1
	for i := range optIdx {
		maxOpt = max(maxOpt, i)
	}
	start := 0
	for start <= maxOpt {
		next := start
		for next <= maxOpt {
			if _, ok := optIdx[next]; ok {
				break
			}
			next++
		}
		if start != next {
			end, err := consumePositionals(start)
			if err != nil {
				return nil, err
			}
			if end > start {
				start = end
				continue
			}
			start = end
		}
		if _, ok := optIdx[start]; !ok {
			extras = append(extras, args[start:next]...)
			start = next
		}
		var err error
		if start, err = consumeOptional(start); err != nil {
			return nil, err
		}
	}
	stop, err := consumePositionals(start)
	if err != nil {
		return nil, err
	}
	extras = append(extras, args[stop:]...)

	var missing []string
	for _, a := range s.actions {
		if !seen[a] && a.required(s.cmd) {
			missing = append(missing, a.name())
		}
	}
	if len(missing) > 0 {
		return nil, argErrf(nil, "the following arguments are required: %s", strings.Join(missing, ", "))
	}
	return extras, nil
}

// takeSub runs a subcommand on the rest of argv and folds its namespace
// into the parent's, the way _SubParsersAction.__call__ does: the child's
// values (defaults included) overwrite the parent's.
func (p *Parser) takeSub(s *spec, ns *namespace, strs []string) error {
	name := strs[0]
	child := s.cmd.sub(name)
	if child == nil {
		return argErrf(s.sub, "invalid choice: %s (choose from %s)", pyRepr(name), reprList(s.sub.choices))
	}
	cpath := append(append([]string{}, s.path...), child.Name)
	cns, extras, err := p.parseKnown(cpath, child, strs[1:])
	if err != nil {
		return err
	}
	for k, v := range cns.vals {
		ns.vals[k] = v
		if cns.given[k] {
			ns.given[k] = true
		} else {
			delete(ns.given, k)
		}
	}
	ns.path, ns.cmd = cns.path, cns.cmd
	ns.typed = append([]string{name}, cns.typed...)
	ns.unrec = append(ns.unrec, extras...)
	return nil
}

// parseOptional is argparse's _parse_optional: nil means "a positional".
func (s *spec) parseOptional(arg string) []optTuple {
	if arg == "" || arg[0] != '-' {
		return nil
	}
	if a, ok := s.optMap[arg]; ok {
		return []optTuple{{act: a, opt: arg}}
	}
	if len(arg) == 1 {
		return nil
	}
	if o, v, found := strings.Cut(arg, "="); found {
		if a, ok := s.optMap[o]; ok {
			return []optTuple{{act: a, opt: o, sep: "=", explicit: v, has: true}}
		}
	}
	if tups := s.optionTuples(arg); len(tups) > 0 {
		return tups
	}
	if negNumber(arg) && !s.negOpts {
		return nil
	}
	if strings.Contains(arg, " ") {
		return nil
	}
	return []optTuple{{opt: arg}}
}

// optionTuples is _get_option_tuples: long options match by unique prefix;
// a single-dash token may be a short option with its value attached.
func (s *spec) optionTuples(arg string) []optTuple {
	var out []optTuple
	prefix, explicit, found := strings.Cut(arg, "=")
	sep := ""
	if found {
		sep = "="
	}
	if arg[1] == '-' {
		for _, o := range s.optOrder {
			if strings.HasPrefix(o, prefix) {
				out = append(out, optTuple{act: s.optMap[o], opt: o, sep: sep, explicit: explicit, has: found})
			}
		}
		return out
	}
	_, size := utf8.DecodeRuneInString(arg[1:])
	short, shortExplicit := arg[:1+size], arg[1+size:]
	for _, o := range s.optOrder {
		switch {
		case o == short:
			out = append(out, optTuple{act: s.optMap[o], opt: o, explicit: shortExplicit, has: true})
		case strings.HasPrefix(o, prefix):
			out = append(out, optTuple{act: s.optMap[o], opt: o, sep: sep, explicit: explicit, has: found})
		}
	}
	return out
}

// nargsPattern is _get_nargs_pattern.
func nargsPattern(a *action) string {
	if len(a.opts) > 0 {
		switch a.nargs {
		case nargsZero:
			return "()"
		case nargsPlus:
			return "(A+)"
		default:
			return "([A])"
		}
	}
	switch a.nargs {
	case nargsOpt:
		return "(-*A?-*)"
	case nargsStar:
		return "(-*[A-]*)"
	case nargsPlus:
		return "(-*A[A-]*)"
	case nargsParser:
		return "(-*A[-AO]*)"
	default:
		return "(-*A-*)"
	}
}

// matchArgument is _match_argument: how many strings an option takes.
func matchArgument(a *action, pattern string) (int, error) {
	m := regexp.MustCompile("^" + nargsPattern(a)).FindStringSubmatchIndex(pattern)
	if m == nil {
		switch a.nargs {
		case nargsPlus:
			return 0, argErrf(a, "expected at least one argument")
		default:
			return 0, argErrf(a, "expected one argument")
		}
	}
	return m[3] - m[2], nil
}

// matchPartial is _match_arguments_partial: match as many positionals as
// possible, dropping trailing empty matches when an option follows.
func matchPartial(actions []*action, pattern string) []int {
	for i := len(actions); i > 0; i-- {
		var re strings.Builder
		re.WriteString("^")
		for _, a := range actions[:i] {
			re.WriteString(nargsPattern(a))
		}
		m := regexp.MustCompile(re.String()).FindStringSubmatchIndex(pattern)
		if m == nil {
			continue
		}
		counts := make([]int, i)
		for j := range counts {
			counts[j] = m[2*j+3] - m[2*j+2]
		}
		if m[1] < len(pattern) && pattern[m[1]] == 'O' {
			for len(counts) > 0 && counts[len(counts)-1] == 0 {
				counts = counts[:len(counts)-1]
			}
		}
		return counts
	}
	return nil
}

// getValues converts and checks the strings an action consumed.
func getValues(a *action, strs []string) (any, error) {
	if a.kind == actPos {
		switch {
		case len(strs) == 0 && a.nargs == nargsOpt:
			return a.arg.Default, nil
		case len(strs) == 0 && a.nargs == nargsStar:
			if a.arg.Default != nil {
				return a.arg.Default, nil
			}
			return []string{}, nil
		case len(strs) == 1 && (a.nargs == nargsOne || a.nargs == nargsOpt):
			return strs[0], checkChoice(a, strs[0])
		}
		for _, v := range strs {
			if err := checkChoice(a, v); err != nil {
				return nil, err
			}
		}
		return strs, nil
	}
	switch a.nargs {
	case nargsZero:
		return true, nil
	case nargsPlus:
		for _, v := range strs {
			if err := checkChoice(a, v); err != nil {
				return nil, err
			}
		}
		return strs, nil
	}
	v, err := convert(a, strs[0])
	if err != nil {
		return nil, err
	}
	return v, checkChoice(a, strs[0])
}

func convert(a *action, s string) (any, error) {
	switch a.flag.Kind {
	case Int:
		n, ok := pyInt(s)
		if !ok {
			return nil, argErrf(a, "invalid int value: %s", pyRepr(s))
		}
		return n, nil
	case PositiveInt:
		n, ok := pyInt(s)
		if !ok {
			return nil, argErrf(a, "%s is not a whole number", s)
		}
		if n < 1 {
			return nil, argErrf(a, "%s must be 1 or more", s)
		}
		return n, nil
	}
	return s, nil
}

func checkChoice(a *action, v string) error {
	if a.choices == nil {
		return nil
	}
	for _, c := range a.choices {
		if c == v {
			return nil
		}
	}
	return argErrf(a, "invalid choice: %s (choose from %s)", pyRepr(v), reprList(a.choices))
}

func reprList(xs []string) string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = pyRepr(x)
	}
	return strings.Join(out, ", ")
}

func removeFirst(xs []string, v string) []string {
	for i, x := range xs {
		if x == v {
			return append(xs[:i:i], xs[i+1:]...)
		}
	}
	return xs
}
