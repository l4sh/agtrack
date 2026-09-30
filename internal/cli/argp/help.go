package argp

import (
	"strings"
)

// Help rendering. The layout follows argparse's HelpFormatter (usage
// wrapping, a 24-column help position) but, per design §4.9, per-verb help
// lists only the verb's own flags: the global flags appear once, in the
// root help, and every other page points there.

const maxHelpPosition = 24

// progFor is "agtrack task add".
func (p *Parser) progFor(path []string) string {
	return strings.TrimSpace(p.prog() + " " + strings.Join(path, " "))
}

// UsageLine renders the argparse-style "usage: agtrack task add ..." line
// (wrapped, no trailing newline) for a command path of names or aliases.
// The root usage includes the global flags; verb usage does not.
func (p *Parser) UsageLine(path []string) string {
	path = p.canonical(path)
	c := p.Find(path)
	if c == nil {
		return ""
	}
	s, err := p.spec(path, c)
	if err != nil {
		return ""
	}
	var opts, pos []string
	for _, a := range s.actions {
		switch {
		case a.kind == actFlag && a.suppress && c != p.Root:
			continue
		case len(a.opts) > 0:
			part := a.opts[0]
			if args := flagArgs(a); args != "" {
				part += " " + args
			}
			opts = append(opts, "["+part+"]")
		default:
			pos = append(pos, posUsage(a))
		}
	}

	prefix := "usage: " + p.progFor(path)
	full := strings.Join(append(append([]string{prefix}, opts...), pos...), " ")
	textWidth := p.width() - 2
	if len(full) <= textWidth {
		return full
	}
	// argparse's get_lines: options fill lines after the prog, positionals
	// start on a fresh line, all aligned under the first option.
	indent := strings.Repeat(" ", len(prefix)+1)
	var lines []string
	fill := func(parts []string) {
		var line []string
		n := len(indent) - 1
		for _, part := range parts {
			if n+1+len(part) > textWidth && len(line) > 0 {
				lines = append(lines, indent+strings.Join(line, " "))
				line, n = nil, len(indent)-1
			}
			line = append(line, part)
			n += len(part) + 1
		}
		if len(line) > 0 {
			lines = append(lines, indent+strings.Join(line, " "))
		}
	}
	if len(opts) > 0 {
		fill(opts)
		fill(pos)
	} else {
		fill(pos)
	}
	lines[0] = prefix + " " + strings.TrimLeft(lines[0], " ")
	return strings.Join(lines, "\n")
}

// Help renders the help page for a command path of names or aliases; ""
// for an unknown path.
func (p *Parser) Help(path []string) string {
	path = p.canonical(path)
	c := p.Find(path)
	if c == nil {
		return ""
	}
	s, err := p.spec(path, c)
	if err != nil {
		return ""
	}
	root := c == p.Root
	var b strings.Builder
	b.WriteString(p.UsageLine(path))
	b.WriteString("\n")
	if c.Help != "" {
		b.WriteString("\n" + c.Help + "\n")
	}

	var posItems, optItems, globalItems, subItems [][2]string
	for _, a := range s.actions {
		switch a.kind {
		case actPos:
			posItems = append(posItems, [2]string{posMetavar(a), a.arg.Help})
		case actSub:
			for _, sc := range c.Subs {
				if sc.Hidden {
					continue
				}
				name := sc.Name
				if len(sc.Aliases) > 0 {
					name += " (" + strings.Join(sc.Aliases, ", ") + ")"
				}
				subItems = append(subItems, [2]string{name, sc.Help})
			}
		case actHelp:
			optItems = append(optItems, [2]string{flagInvocation(a), "show this help message and exit"})
		case actVersion:
			globalItems = append(globalItems, [2]string{flagInvocation(a), "show program's version number and exit"})
		case actFlag:
			item := [2]string{flagInvocation(a), a.flag.Help}
			switch {
			case !a.suppress:
				optItems = append(optItems, item)
			case root:
				globalItems = append(globalItems, item)
			}
		}
	}
	if root {
		// At the root, -h is one of the global flags.
		globalItems = append(optItems[:1:1], globalItems...)
		optItems = optItems[1:]
	}

	all := append(append(append(append([][2]string{}, posItems...), optItems...), globalItems...), subItems...)
	col := helpColumn(all)
	section := func(title string, items [][2]string) {
		if len(items) == 0 {
			return
		}
		b.WriteString("\n" + title + ":\n")
		for _, it := range items {
			b.WriteString(p.formatItem(it[0], it[1], col))
		}
	}
	subTitle := "verbs"
	if root {
		subTitle = "commands"
	}
	if root {
		section(subTitle, subItems)
		section("positional arguments", posItems)
		section("options", optItems)
		section("global flags", globalItems)
	} else {
		section("positional arguments", posItems)
		section("options", optItems)
		section(subTitle, subItems)
		b.WriteString("\nglobal output flags: see " + p.prog() + " --help\n")
	}
	if c.Epilog != "" {
		b.WriteString("\n" + strings.TrimRight(c.Epilog, "\n") + "\n")
	}
	return b.String()
}

// helpColumn is argparse's help position: two past the longest invocation
// (indented by two), capped at 24.
func helpColumn(items [][2]string) int {
	longest := 0
	for _, it := range items {
		longest = max(longest, len(it[0])+2)
	}
	return min(longest+2, maxHelpPosition)
}

func (p *Parser) formatItem(invocation, help string, col int) string {
	var b strings.Builder
	head := "  " + invocation
	if help == "" {
		return head + "\n"
	}
	lines := wrap(help, p.width()-2-col)
	if len(head) <= col-2 {
		b.WriteString(head + strings.Repeat(" ", col-len(head)) + lines[0] + "\n")
		lines = lines[1:]
	} else {
		b.WriteString(head + "\n")
	}
	for _, l := range lines {
		b.WriteString(strings.Repeat(" ", col) + l + "\n")
	}
	return b.String()
}

// wrap splits text into lines of at most width runes at spaces.
func wrap(text string, width int) []string {
	width = max(width, 11)
	var lines []string
	line := ""
	for _, w := range strings.Fields(text) {
		switch {
		case line == "":
			line = w
		case len(line)+1+len(w) <= width:
			line += " " + w
		default:
			lines = append(lines, line)
			line = w
		}
	}
	if line != "" || len(lines) == 0 {
		lines = append(lines, line)
	}
	return lines
}

// flagMetavar is the flag's metavar: explicit, {choices}, or DEST.
func flagMetavar(a *action) string {
	switch {
	case a.flag.Metavar != "":
		return a.flag.Metavar
	case a.choices != nil:
		return "{" + strings.Join(a.choices, ",") + "}"
	default:
		return strings.ToUpper(a.dest)
	}
}

// flagArgs is what follows the option string: "REF", "TITLE [TITLE ...]".
func flagArgs(a *action) string {
	switch a.nargs {
	case nargsZero:
		return ""
	case nargsPlus:
		m := flagMetavar(a)
		return m + " [" + m + " ...]"
	default:
		return flagMetavar(a)
	}
}

// flagInvocation is "-p, --project REF".
func flagInvocation(a *action) string {
	inv := strings.Join(a.opts, ", ")
	if args := flagArgs(a); args != "" {
		inv += " " + args
	}
	return inv
}

func posMetavar(a *action) string {
	if a.kind == actSub {
		return a.metavar
	}
	if a.choices != nil {
		return "{" + strings.Join(a.choices, ",") + "}"
	}
	return a.dest
}

func posUsage(a *action) string {
	m := posMetavar(a)
	switch a.nargs {
	case nargsOpt:
		return "[" + m + "]"
	case nargsStar:
		return "[" + m + " ...]"
	case nargsPlus:
		return m + " [" + m + " ...]"
	case nargsParser:
		return m + " ..."
	default:
		return m
	}
}
