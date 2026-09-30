package cli

import (
	"github.com/l4sh/agtrack/internal/cli/argp"
	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/row"
)

func specTree() *argp.Command {
	return &argp.Command{Name: "spec", Aliases: []string{"specs"}, Help: "specs: markdown documents addressable by section", SubRequired: true, Hidden: true,
		Subs: []*argp.Command{{Name: "ls", Run: Handler(func(c *Ctx) error { return errs.New("spec: not built yet") })}}}
}

func (c *Ctx) linkSpec(ref string, task *row.Row, kind, scope string) error {
	return errs.New("spec links: not built yet")
}

func taskReviewSpec(c *Ctx) error { return errs.New("spec review: not built yet") }
