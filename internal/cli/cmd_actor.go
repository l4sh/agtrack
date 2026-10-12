package cli

import (
	"fmt"
	"strings"

	"github.com/l4sh/agtrack/internal/cli/argp"
	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

// Actor registry, strict mode and actor-required writes (agent-features
// spec D1, D2). Approvers (D3) live in store.IsApprover.

func actorTree() *argp.Command {
	return &argp.Command{Name: "actor", Aliases: []string{"actors"}, Help: "the actor registry: canonical names and their aliases", SubRequired: true,
		Subs: []*argp.Command{
			{Name: "add", Help: "register an actor", Run: Handler(actorAdd), Args: []argp.Arg{{Name: "slug"}},
				Flags: []argp.Flag{{Long: "--kind", Choices: []string{"agent", "human"}, Default: "agent"},
					{Long: "--alias", Kind: argp.Append, Metavar: "A", Help: "another spelling that resolves to this actor; repeatable"},
					{Long: "--note"}}},
			{Name: "alias", Help: "add an alias to an actor", Run: Handler(actorAlias), Args: []argp.Arg{{Name: "slug"}, {Name: "alias"}}},
			{Name: "ls", Aliases: []string{"list"}, Help: "list registered actors", Run: Handler(actorLs), Flags: listFlags()},
			{Name: "rm", Aliases: []string{"delete"}, Help: "remove an actor nothing refers to", Run: Handler(actorRm), Args: []argp.Arg{{Name: "slug"}}},
			{Name: "merge", Help: "rewrite everything written as ALIAS to the actor INTO (one-off clean-up of name drift)", Run: Handler(actorMerge),
				Args: []argp.Arg{{Name: "alias"}},
				Flags: []argp.Flag{{Long: "--into", Metavar: "SLUG", Help: "the registered actor that keeps the history"},
					{Long: "--yes", Kind: argp.Bool, Help: "required: this rewrites past events, notes and claims"}}},
		}}
}

func actorAdd(c *Ctx) error {
	show := func(ids []int64) error {
		rows, err := c.S.ListActors()
		if err != nil {
			return err
		}
		for _, r := range rows {
			if r.Int("id") == ids[0] {
				return c.created("actor", r.Str("slug"), r, "actor")
			}
		}
		return errs.NotFound("actor", fmt.Sprint(ids[0]), "actor %d no longer exists", ids[0])
	}
	if done, err := c.replay(show); done {
		return err
	}
	var added *row.Row
	ids, err := c.record("actor", func() ([]int64, error) {
		r, err := c.S.AddActor(c.Str("slug"), c.Str("kind"), c.Str("note"), c.Tags("alias"))
		added = r
		if err != nil {
			return nil, err
		}
		return []int64{r.Int("id")}, nil
	})
	if err != nil {
		return err
	}
	if added == nil {
		return show(ids)
	}
	return c.created("actor", added.Str("slug"), added, "actor")
}

func actorAlias(c *Ctx) error {
	r, err := c.S.AliasActor(c.Str("slug"), c.Str("alias"))
	if err != nil {
		return err
	}
	return c.mutated(fmt.Sprintf("%s: alias %s", r.Str("slug"), c.Str("alias")), r)
}

func actorLs(c *Ctx) error {
	rows, err := c.S.ListActors()
	if err != nil {
		return err
	}
	return c.emit(rows, "actor", len(rows))
}

func actorRm(c *Ctx) error {
	if err := c.S.RemoveActor(c.Str("slug")); err != nil {
		return err
	}
	return c.mutated("removed actor "+c.Str("slug"), row.Of("removed", c.Str("slug")))
}

func actorMerge(c *Ctx) error {
	into := c.Str("into")
	if into == "" {
		return errs.Usage("--into SLUG is required")
	}
	if !c.Bool("yes") {
		return errs.Usage("actor merge rewrites past events, notes and claims; pass --yes to confirm")
	}
	changes, err := c.S.MergeActor(c.Str("alias"), into)
	if err != nil {
		return err
	}
	total := int64(0)
	rewritten := row.New()
	for _, k := range store.ActorKeys(changes) {
		rewritten.Set(k, changes[k])
		total += changes[k]
	}
	obj := row.Of("merged", c.Str("alias"), "into", into, "rows", total, "columns", rewritten)
	return c.mutated(fmt.Sprintf("merged %s into %s (%d rows)", c.Str("alias"), into, total), obj)
}

// readLeaves are verbs that only read; everything else that opens the store
// is a write for actor purposes. This is the one place that decides.
var readLeaves = map[string]bool{
	"ls": true, "list": true, "show": true, "get": true, "current": true, "find": true, "deps": true,
	"notes": true, "toc": true, "check": true, "coverage": true, "stale": true, "orphans": true,
	"diff": true, "render": true, "watch": true, "await": true, "serve": true, "resume": true,
	"summary": true, "here": true, "dump": true, "info": true,
}

// isWriteVerb reports whether the invocation mutates the store.
func isWriteVerb(path []string, c *Ctx) bool {
	if len(path) == 0 {
		return false
	}
	switch strings.Join(path, " ") {
	case "log": // bare `log` lists
		return false
	case "spec status", "grammar", "describe":
		return false
	case "next":
		return c.Bool("claim")
	case "project repo":
		return c.Str("repo_action") != "ls"
	}
	if path[0] == "skill" || path[0] == "config" || path[0] == "hooks" {
		return false
	}
	return !readLeaves[path[len(path)-1]]
}

// checkActor canonicalises the actor and enforces actor.required and
// actor.strict for writes. A fresh database (no registry) stays quiet.
func (c *Ctx) checkActor(res *argp.Result) error {
	c.Actor = c.S.Actor // store.New already rewrote an alias to its slug
	if !isWriteVerb(res.Path, c) {
		return nil
	}
	if !c.ActorGiven && c.S.ConfigBool("actor.required") {
		return errs.New("this write has no actor: set AGTRACK_ACTOR=<model id> or pass --actor")
	}
	if c.Actor == "" {
		return nil
	}
	_, known, empty, err := c.S.ResolveActor(c.Actor)
	if err != nil || known || empty {
		return err
	}
	msg := fmt.Sprintf("unknown actor %q (register it: agtrack actor add %s, or alias it to an existing one)", c.Actor, c.Actor)
	if c.S.ConfigBool("actor.strict") {
		return errs.New("%s", msg)
	}
	fmt.Fprintf(c.Err, "agtrack: warning: %s\n", msg)
	return nil
}
