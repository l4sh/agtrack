package argp

import "strings"

// Schema is the JSON-able description of one command, generated from the
// same definitions the parser uses (`agtrack describe`, design §4.9).
// Field order is fixed by the struct, so the output is deterministic.
type Schema struct {
	Command     string       `json:"command"` // "task add"; "" for the root
	Aliases     []string     `json:"aliases"`
	Help        string       `json:"help"`
	Usage       string       `json:"usage"`
	Args        []ArgSchema  `json:"args"`
	Flags       []FlagSchema `json:"flags"`
	Subcommands []SubSchema  `json:"subcommands"`
	SubRequired bool         `json:"subcommand_required"`
	Output      []OutputKey  `json:"output,omitempty"`
}

// ArgSchema describes a positional.
type ArgSchema struct {
	Name    string   `json:"name"`
	NArgs   string   `json:"nargs"` // "1", "?", "*", "+"
	Choices []string `json:"choices"`
	Help    string   `json:"help"`
}

// FlagSchema describes a flag.
type FlagSchema struct {
	Long       string   `json:"long"`
	Short      string   `json:"short"`
	Dest       string   `json:"dest"`
	Kind       string   `json:"kind"`
	Choices    []string `json:"choices"`
	Default    any      `json:"default"`
	Metavar    string   `json:"metavar"`
	Help       string   `json:"help"`
	Repeatable bool     `json:"repeatable"`
	Global     bool     `json:"global"`
}

// SubSchema is one entry of a command's subcommand list.
type SubSchema struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases"`
	Help    string   `json:"help"`
}

// Describe returns the schema of a command path of names or aliases, or nil
// for an unknown path. A verb lists only its own flags; the root lists the
// global flags (Global: true), as its help does.
func (p *Parser) Describe(path []string) *Schema {
	path = p.canonical(path)
	c := p.Find(path)
	if path == nil || c == nil {
		return nil
	}
	s, err := p.spec(path, c)
	if err != nil {
		return nil
	}
	root := c == p.Root
	out := &Schema{
		Command:     strings.Join(path, " "),
		Aliases:     nonNil(c.Aliases),
		Help:        c.Help,
		Usage:       strings.TrimPrefix(p.UsageLine(path), "usage: "),
		Args:        []ArgSchema{},
		Flags:       []FlagSchema{},
		Subcommands: []SubSchema{},
		SubRequired: len(c.Subs) > 0 && c.SubRequired,
		Output:      c.Output,
	}
	for _, a := range s.actions {
		switch a.kind {
		case actPos:
			n := a.nargs
			if n == nargsOne {
				n = "1"
			}
			out.Args = append(out.Args, ArgSchema{Name: a.dest, NArgs: n, Choices: nonNil(a.choices), Help: a.arg.Help})
		case actFlag:
			if a.suppress && !root {
				continue
			}
			f := a.flag
			fs := FlagSchema{Long: f.Long, Short: f.Short, Dest: a.dest, Kind: f.Kind.String(),
				Choices: nonNil(a.choices), Metavar: flagMetavar(a), Help: f.Help,
				Repeatable: f.Kind == Append, Global: a.suppress}
			if f.Kind == Bool {
				fs.Metavar = ""
			}
			if !a.suppress {
				fs.Default = a.defaultValue()
			}
			out.Flags = append(out.Flags, fs)
		case actVersion:
			out.Flags = append(out.Flags, FlagSchema{Long: "--version", Short: "-V", Dest: "version",
				Kind: Bool.String(), Choices: []string{}, Help: "show program's version number and exit", Global: true})
		}
	}
	for _, sc := range c.Subs {
		if !sc.Hidden {
			out.Subcommands = append(out.Subcommands, SubSchema{Name: sc.Name, Aliases: nonNil(sc.Aliases), Help: sc.Help})
		}
	}
	return out
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}
