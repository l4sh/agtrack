package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/l4sh/agtrack"
	"github.com/l4sh/agtrack/internal/cli/argp"
	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/skill"
)

func skillTree() *argp.Command {
	target := []argp.Flag{
		{Long: "--agent", Kind: argp.Append, Metavar: "NAME", Help: "claude, codex, agy or all; repeatable (default: every harness found under $HOME)"},
		{Long: "--dir", Kind: argp.Append, Metavar: "PATH", Help: "a skills directory to use instead (e.g. a project's .claude/skills); repeatable"},
	}
	return &argp.Command{Name: "skill", Aliases: []string{"skills"}, Help: "install the agtrack agent skill into agent harnesses", SubRequired: true,
		Subs: []*argp.Command{
			{Name: "install", Help: "install or update the skill matching this binary", Run: Handler(skillInstall),
				Flags: append(append([]argp.Flag{}, target...),
					argp.Flag{Long: "--force", Kind: argp.Bool, Help: "replace an install agtrack did not make, or one edited locally"},
					argp.Flag{Long: "--dry-run", Kind: argp.Bool, Help: "show what would change"})},
			{Name: "status", Help: "where the skill is installed and whether it is current", Run: Handler(skillStatus), Flags: target},
			{Name: "uninstall", Help: "remove the skill", Run: Handler(skillUninstall),
				Flags: append(append([]argp.Flag{}, target...),
					argp.Flag{Long: "--force", Kind: argp.Bool, Help: "also remove an install agtrack did not make"},
					argp.Flag{Long: "--dry-run", Kind: argp.Bool, Help: "show what would change"})},
			{Name: "print", Help: "print a skill file to stdout (default SKILL.md)", Run: Handler(skillPrint),
				Args: []argp.Arg{{Name: "file", NArgs: "?", Help: "SKILL.md, references/spec-review.md, ..."}}},
		}}
}

func skillTargets(c *Ctx, all bool) ([]skill.Target, map[string][]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, err
	}
	agents := c.Strs("agent")
	if all && len(agents) == 0 && len(c.Strs("dir")) == 0 {
		agents = []string{"all"}
	}
	targets, err := skill.Targets(home, agents, c.Strs("dir"))
	if err != nil {
		return nil, nil, err
	}
	files, err := skill.Files(agtrack.Skills)
	return targets, files, err
}

func skillInstall(c *Ctx) error {
	targets, files, err := skillTargets(c, false)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		return errs.New("no agent harness found under your home directory; name one with --agent or a directory with --dir")
	}
	var results []*row.Row
	var failed error
	for _, t := range targets {
		state, why := skill.Inspect(t, files)
		plan := map[skill.State][2]string{
			skill.Missing:  {"install", "installed"},
			skill.Current:  {"up to date", "up to date"},
			skill.Outdated: {"update", "updated"},
			skill.Modified: {"replace a locally edited copy", "replaced a locally edited copy"},
			skill.Foreign:  {"replace (" + why + ")", "replaced (" + why + ")"},
		}[state]
		var action string
		switch {
		case !t.Present:
			action = "skipped: harness not installed"
		case (state == skill.Foreign || state == skill.Modified) && !c.Bool("force"):
			action = "refused: " + string(state) + " (" + why + "); use --force"
			failed = errs.Conflict("some targets were refused; see above")
		case c.Bool("dry-run"):
			action = "would " + plan[0]
		default:
			action = plan[1]
			if _, err := skill.Install(t, files, Version, c.Bool("force")); err != nil {
				action, failed = "failed: "+err.Error(), err
			}
		}
		results = append(results, row.Of("harness", nilIfEmpty(t.Harness), "label", t.Label, "dir", t.Dir, "result", action))
	}
	if err := printSkillRows(c, results, "result"); err != nil {
		return err
	}
	return failed
}

func skillStatus(c *Ctx) error {
	targets, files, err := skillTargets(c, true)
	if err != nil {
		return err
	}
	var results []*row.Row
	for _, t := range targets {
		state, why := skill.Inspect(t, files)
		detail := string(state)
		if !t.Present && state == skill.Missing {
			detail = "harness not installed"
		} else if why != "" {
			detail += " (" + why + ")"
		}
		results = append(results, row.Of("harness", nilIfEmpty(t.Harness), "label", t.Label, "dir", t.Dir, "state", string(state), "detail", detail))
	}
	return printSkillRows(c, results, "detail")
}

func skillUninstall(c *Ctx) error {
	targets, files, err := skillTargets(c, true)
	if err != nil {
		return err
	}
	var results []*row.Row
	var failed error
	for _, t := range targets {
		state, why := skill.Inspect(t, files)
		action := "removed"
		switch {
		case state == skill.Missing:
			action = "not installed"
		case (state == skill.Foreign || state == skill.Modified) && !c.Bool("force"):
			action = "refused: " + string(state) + " (" + why + "); use --force"
			failed = errs.Conflict("some targets were refused; see above")
		case c.Bool("dry-run"):
			action = "would remove"
		default:
			if _, err := skill.Uninstall(t, files, c.Bool("force")); err != nil {
				action, failed = "failed: "+err.Error(), err
			}
		}
		results = append(results, row.Of("harness", nilIfEmpty(t.Harness), "label", t.Label, "dir", t.Dir, "result", action))
	}
	if err := printSkillRows(c, results, "result"); err != nil {
		return err
	}
	return failed
}

func printSkillRows(c *Ctx, rows []*row.Row, key string) error {
	if c.structured() {
		return c.emitObject(nonNil(rows))
	}
	if c.Quiet {
		return nil
	}
	for _, r := range rows {
		label := r.Str("label")
		if h := r.Str("harness"); h != "" {
			label = h
		}
		c.printf("%-8s %s  %s\n", label, r.Str("dir"), r.Str(key))
	}
	return nil
}

func skillPrint(c *Ctx) error {
	files, err := skill.Files(agtrack.Skills)
	if err != nil {
		return err
	}
	name := c.Str("file")
	if name == "" {
		name = "SKILL.md"
	}
	data, ok := files[filepath.ToSlash(name)]
	if !ok {
		var names []string
		for n := range files {
			names = append(names, n)
		}
		return errs.NotFound("file", name, "no such skill file: %s (have: %s)", name, strings.Join(names, ", "))
	}
	_, err = fmt.Fprint(c.Out, string(data))
	return err
}
