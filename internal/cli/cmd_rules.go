package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/l4sh/agtrack/internal/cli/argp"
	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/render"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

// Standing rules and `check` (agent-features spec H1, H3, H4, H5).

// Exit codes of `check`.
const (
	ExitCheckDeny          = 20
	ExitCheckHardDeny      = 21
	ExitCheckNeedsApproval = 22
)

func init() {
	fieldsFor["rule"] = [2][]string{{"id", "kind", "rule", "scope", "status", "hard"}, {"quote", "path", "repo", "where", "expires", "actor", "created_at"}}
	readLeaves["rules"] = true
	readLeaves["lessons"] = true
	for _, v := range []string{"rule add", "rule propose", "lesson add"} {
		idemVerbs[v] = true
	}
}

// scopeFlags are -g / -p / -t; -c (the global context flag) names a context scope.
func scopeFlags() []argp.Flag {
	return []argp.Flag{
		{Long: "--global", Short: "-g", Kind: argp.Bool, Help: "machine-wide scope: applies in every context"},
		{Long: "--project", Short: "-p", Metavar: "REF", Help: "project, project:subproject or . (the cwd repo's project)"},
		{Long: "--task", Short: "-t", Metavar: "ID", Help: "task scope (a sub-agent handed this task inherits it)"},
	}
}

func ruleAddFlags() []argp.Flag {
	return append(scopeFlags(),
		argp.Flag{Long: "--verb", Metavar: "VERB", Help: "for require/remember: only this verb (" + strings.Join(store.RuleVerbs, ", ") + ")"},
		argp.Flag{Long: "--path", Metavar: "GLOB", Help: "only actions touching a matching repo-relative path (src/db/**, *.sql)"},
		argp.Flag{Long: "--repo", Metavar: "SLUG", Help: "only in this repository (owner/name or a remote URL)"},
		argp.Flag{Long: "--where", Kind: argp.Append, Metavar: "K=V", Help: "condition: branch=GLOB or base=true|false; repeatable"},
		argp.Flag{Long: "--hard", Kind: argp.Bool, Help: "cannot be cancelled by an allow (approvers only; agents propose)"},
		argp.Flag{Long: "--overridable", Kind: argp.Bool, Help: "deny only: an approved allow at the same or a narrower scope cancels it"},
		argp.Flag{Long: "--until", Metavar: "WHEN", Help: "expires at a date or after a duration (2026-11-01, 3d)"},
		argp.Flag{Long: "--until-task-closed", Kind: argp.Bool, Help: "expires when the scope task (-t) closes"},
		argp.Flag{Long: "--for-session", Kind: argp.Bool, Help: "binds this session only; lapses once the session is quiet for 12h"},
		argp.Flag{Long: "--quote", Metavar: "TEXT", Help: "the human's own words; required for deny, allow and --hard"},
		argp.Flag{Long: "--stated-by", Metavar: "ACTOR", Help: "who stated the rule (default: you, when you are an approver)"},
		argp.Flag{Long: "--supersedes", Kind: argp.Int, Metavar: "ID", Help: "replace this rule (revoked once the new one is active)"},
		argp.Flag{Long: "--force", Kind: argp.Bool, Help: "add even though a similar rule exists"},
	)
}

var ruleArgs = []argp.Arg{
	{Name: "kind", Choices: store.RuleKinds},
	{Name: "what", NArgs: "*", Help: "deny/allow: VERB [text]; require: CHECK [text]; remember: text"},
}

func ruleListFlags() []argp.Flag {
	return append(scopeFlags(),
		append([]argp.Flag{
			{Long: "--all", Kind: argp.Bool, Help: "every status: proposed, revoked and expired too"},
			{Long: "--proposed", Kind: argp.Bool, Help: "only rules waiting for an approver"},
		}, listFlags()...)...)
}

var checkOutput = []argp.OutputKey{
	{Key: "action", Help: "the verb checked"},
	{Key: "decision", Help: "allow | deny | needs-approval (exit 0 | 20, or 21 when hard | 22)"},
	{Key: "hard", Help: "true when a hard rule denies"},
	{Key: "matched", Help: "rules that bear on the action: id, kind, rule, scope, hard, quote, effect (deny, cancelled, pending-approval, failed, passed, unverified, ask-first, self-check, remember)"},
	{Key: "allowed_by", Help: "allow rules that cancelled a deny"},
	{Key: "hint", Help: "what to do next, or null"},
}

func ruleTree() *argp.Command {
	return &argp.Command{Name: "rule", Help: "standing rules: deny, allow, require, remember", SubRequired: true,
		Subs: []*argp.Command{
			{Name: "add", Help: "record a rule (an agent's allow or --hard rule is stored as proposed)", Run: Handler(ruleAdd), Args: ruleArgs, Flags: ruleAddFlags()},
			{Name: "propose", Help: "rule add, always stored as proposed for an approver", Run: Handler(rulePropose), Args: ruleArgs, Flags: ruleAddFlags()},
			{Name: "approve", Help: "make a proposed rule active (approvers only)", Run: Handler(ruleApprove), Args: []argp.Arg{{Name: "id"}},
				Flags: []argp.Flag{{Long: "--note"}}},
			{Name: "revoke", Help: "end a rule; it stays in history", Run: Handler(ruleRevoke), Args: []argp.Arg{{Name: "id"}},
				Flags: []argp.Flag{{Long: "--note", Help: "why (required)"}}},
			{Name: "show", Help: "a rule with its history", Run: Handler(ruleShow), Args: []argp.Arg{{Name: "id"}}},
			{Name: "ls", Aliases: []string{"list"}, Help: "same as `rules`", Run: Handler(rulesLs), Flags: ruleListFlags()},
		}}
}

func rulesCmd() *argp.Command {
	return &argp.Command{Name: "rules", Help: "the effective rules here (cwd project, or a scope), narrowest first", Run: Handler(rulesLs), Flags: ruleListFlags()}
}

func checkCmd() *argp.Command {
	return &argp.Command{Name: "check", Help: "may I do VERB here? exit 0 allow, 20 deny, 21 hard deny, 22 needs approval", Run: Handler(cmdCheck),
		Args: []argp.Arg{{Name: "verb", Choices: store.RuleVerbs}},
		Flags: []argp.Flag{
			{Long: "--repo", Metavar: "R", Help: "repository (default: the cwd's origin remote)"},
			{Long: "--branch", Metavar: "B", Help: "branch (default: the cwd's)"},
			{Long: "--path", Kind: argp.Append, Metavar: "P", Help: "a repo-relative (or absolute) path the action touches; repeatable"},
			{Long: "--task", Short: "-t", Metavar: "ID", Help: "resolve from this task's scope"},
			{Long: "--project", Short: "-p", Metavar: "REF", Help: "resolve from this project (default: the cwd's)"},
		}, Output: checkOutput}
}

// addScope is where a new rule or lesson lives: -g, -t, -p or -c, else the
// current project.
func addScope(c *Ctx) (store.Scope, error) {
	n := 0
	for _, k := range []string{"global", "task", "project"} {
		if c.Has(k) {
			n++
		}
	}
	if n > 1 {
		return store.Scope{}, errs.Usage("give one scope: -g, -c CTX, -p REF or -t ID")
	}
	switch {
	case c.Bool("global"):
		return store.Scope{Type: "global"}, nil
	case c.Has("task"):
		t, err := c.S.GetTask(c.Str("task"), true)
		if err != nil {
			return store.Scope{}, err
		}
		return store.Scope{Type: "task", ID: t.Int("id")}, nil
	case c.Has("project"):
		p, err := c.resolveProject(c.Str("project"), true)
		if err != nil {
			return store.Scope{}, err
		}
		return store.Scope{Type: "project", ID: p.Int("id")}, nil
	case c.Context != "":
		ctx, err := c.S.GetContext(c.Context, true)
		if err != nil {
			return store.Scope{}, err
		}
		return store.Scope{Type: "context", ID: ctx.Int("id")}, nil
	}
	p, err := c.S.DefaultProject()
	if err != nil {
		return store.Scope{}, errs.Usage("give a scope: -g (global), -c CTX, -p REF or -t ID (no project is current here)")
	}
	return store.Scope{Type: "project", ID: p.Int("id")}, nil
}

// resolvePoint is where rules and lessons are read from: -t, -p, -g or -c,
// else the cwd's project (or the pinned one), else the current context.
func resolvePoint(c *Ctx) (store.Point, error) {
	switch {
	case c.Has("task"):
		t, err := c.S.GetTask(c.Str("task"), true)
		if err != nil {
			return store.Point{}, err
		}
		return store.Point{TaskID: t.Int("id")}, nil
	case c.Has("project"):
		p, err := c.resolveProject(c.Str("project"), true)
		if err != nil {
			return store.Point{}, err
		}
		return store.Point{ProjectID: p.Int("id")}, nil
	case c.Bool("global"):
		return store.Point{}, nil
	case c.Context != "":
		ctx, err := c.S.GetContext(c.Context, true)
		if err != nil {
			return store.Point{}, err
		}
		return store.Point{ContextID: ctx.Int("id")}, nil
	}
	if p, err := c.S.DefaultProject(); err == nil && p != nil {
		return store.Point{ProjectID: p.Int("id")}, nil
	}
	if ctx, err := c.S.CurrentContext(); err == nil && ctx != nil {
		return store.Point{ContextID: ctx.Int("id")}, nil
	}
	return store.Point{}, nil
}

func ruleInput(c *Ctx) (store.RuleInput, error) {
	in := store.RuleInput{Kind: c.Str("kind"), PathGlob: c.Str("path"), Repo: c.Str("repo"), Hard: c.Bool("hard"),
		Overridable: c.Bool("overridable"), UntilTaskClosed: c.Bool("until_task_closed"), Quote: c.Str("quote"),
		StatedBy: c.Str("stated_by"), Supersedes: int64(c.Int("supersedes")), Force: c.Bool("force"), Verb: c.Str("verb")}
	what := c.Strs("what")
	rest := func(from int) string {
		if len(what) <= from {
			return ""
		}
		return strings.Join(what[from:], " ")
	}
	switch in.Kind {
	case "deny", "allow":
		if len(what) == 0 {
			return in, errs.Usage("a %s rule names a verb: %s", in.Kind, strings.Join(store.RuleVerbs, ", "))
		}
		if in.Verb != "" {
			return in, errs.Usage("a %s rule takes its verb as the first argument, not --verb", in.Kind)
		}
		in.Verb, in.Text = what[0], rest(1)
	case "require":
		if len(what) == 0 {
			return in, errs.Usage("a require rule names a check: %s", strings.Join(store.RuleChecks, ", "))
		}
		in.Check, in.Text = what[0], rest(1)
	case "remember":
		in.Text = rest(0)
	}
	if v := c.Str("until"); v != "" {
		t, err := refs.ParseUntil(v)
		if err != nil {
			return in, err
		}
		in.Until = t
	}
	if c.Bool("for_session") {
		if c.Session == "" {
			return in, errs.Usage("--for-session needs a session id: set AGTRACK_SESSION or pass --session")
		}
		in.Session = c.Session
	}
	for _, kv := range c.Strs("where") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return in, errs.Usage("--where takes K=V: %s", kv)
		}
		if in.Where == nil {
			in.Where = map[string]string{}
		}
		in.Where[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	sc, err := addScope(c)
	if err != nil {
		return in, err
	}
	in.Scope = sc
	return in, nil
}

func ruleAdd(c *Ctx) error     { return ruleCreate(c, false) }
func rulePropose(c *Ctx) error { return ruleCreate(c, true) }

func ruleCreate(c *Ctx, propose bool) error {
	show := func(ids []int64) error {
		r, err := c.S.GetRule(ids[0])
		if err != nil {
			return err
		}
		return ruleCreated(c, r)
	}
	if done, err := c.replay(show); done {
		return err
	}
	in, err := ruleInput(c)
	if err != nil {
		return err
	}
	in.Propose = propose
	var added *row.Row
	ids, err := c.record("rule", func() ([]int64, error) {
		r, err := c.S.AddRule(in)
		if err != nil {
			return nil, err
		}
		added = r
		return []int64{r.Int("id")}, nil
	})
	if err != nil {
		return err
	}
	if added == nil {
		return show(ids)
	}
	return ruleCreated(c, added)
}

func ruleCreated(c *Ctx, r *row.Row) error {
	r.Delete("events")
	if r.Str("status") == "proposed" {
		c.Hint(fmt.Sprintf("an approver runs: agtrack rule approve %d", r.Int("id")))
	}
	if !c.structured() && !c.Quiet {
		c.printf("rule %d %s: %s %s (%s)\n", r.Int("id"), r.Str("status"), r.Str("kind"), r.Str("rule"), r.Str("scope"))
		return nil
	}
	return c.created("rule", r.Int("id"), r, "rule")
}

func ruleIDArg(c *Ctx) (int64, error) {
	return parseID(strings.TrimPrefix(c.Str("id"), "#"))
}

func ruleApprove(c *Ctx) error {
	id, err := ruleIDArg(c)
	if err != nil {
		return err
	}
	r, err := c.S.ApproveRule(id, c.Str("note"))
	if err != nil {
		return err
	}
	return c.mutated(fmt.Sprintf("approved rule %d: %s %s", id, r.Str("kind"), r.Str("rule")), r)
}

func ruleRevoke(c *Ctx) error {
	id, err := ruleIDArg(c)
	if err != nil {
		return err
	}
	if strings.TrimSpace(c.Str("note")) == "" {
		return errs.Usage("rule revoke needs --note: why the rule no longer holds")
	}
	r, err := c.S.RevokeRule(id, c.Str("note"))
	if err != nil {
		return err
	}
	return c.mutated(fmt.Sprintf("revoked rule %d: %s %s", id, r.Str("kind"), r.Str("rule")), r)
}

func ruleShow(c *Ctx) error {
	id, err := ruleIDArg(c)
	if err != nil {
		return err
	}
	r, err := c.S.GetRule(id)
	if err != nil {
		return err
	}
	var secs []render.Section
	for _, k := range []string{"id", "kind", "rule", "scope", "status", "hard", "overridable", "path", "repo", "where", "expires", "quote", "stated_by", "source", "supersedes", "actor", "created_at"} {
		if v := r.Get(k); v != nil && v != false && v != "" {
			secs = append(secs, render.Section{Label: k, Value: v})
		}
	}
	var hist []string
	for _, e := range rows(r.Get("events")) {
		line := fmt.Sprintf("%s %s by %s", refs.FmtTS(e.Str("at"), false, false), e.Str("action"), e.Str("actor"))
		if e.Str("note") != "" {
			line += ": " + e.Str("note")
		}
		hist = append(hist, line)
	}
	secs = append(secs, render.Section{Label: "history", Value: strings.Join(hist, "\n")})
	return c.detail(r, "rule", secs)
}

func rulesLs(c *Ctx) error {
	p, err := resolvePoint(c)
	if err != nil {
		return err
	}
	rs, err := c.S.ListRules(p, store.RuleListOpts{All: c.Bool("all"), Proposed: c.Bool("proposed")})
	if err != nil {
		return err
	}
	return c.emit(rs, "rule", len(rs))
}

// repoRel makes an absolute path relative to the repository root when it
// lies inside it. A relative path is taken as repo-relative already.
func repoRel(p, top string) string {
	if top == "" || !filepath.IsAbs(p) {
		return filepath.ToSlash(strings.TrimPrefix(p, "./"))
	}
	if rel, err := filepath.Rel(cleanPath(top), cleanPath(p)); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(p)
}

func cmdCheck(c *Ctx) error {
	p, err := resolvePoint(c)
	if err != nil {
		return err
	}
	branch, top := cwdGit()
	if c.Has("branch") {
		branch = c.Str("branch")
	}
	repo := c.Str("repo")
	if repo == "" {
		repo = c.S.CwdRepo()
	}
	var paths []string
	for _, v := range c.Strs("path") {
		paths = append(paths, repoRel(v, top))
	}
	res, err := c.S.Check(store.CheckInput{Verb: c.Str("verb"), Point: p, Repo: repo, Branch: branch, Paths: paths,
		IsBase: func(b string) bool { return store.IsBaseBranch(top, b) }})
	if err != nil {
		return err
	}
	code := 0
	switch res.Str("decision") {
	case store.DecisionDeny:
		code = ExitCheckDeny
		if res.Get("hard") == true {
			code = ExitCheckHardDeny
		}
	case store.DecisionNeedsApproval:
		code = ExitCheckNeedsApproval
	}
	switch {
	case c.Quiet:
	case c.structured():
		if err := c.emitObject(res); err != nil {
			return err
		}
	default:
		head := res.Str("decision") + ": " + res.Str("action")
		if res.Get("hard") == true {
			head += " (hard)"
		}
		c.println(head)
		for _, m := range rows(res.Get("matched")) {
			line := fmt.Sprintf("  %-16s rule %d  %s %s  [%s]", m.Str("effect"), m.Int("id"), m.Str("kind"), m.Str("rule"), m.Str("scope"))
			if q := m.Str("quote"); q != "" {
				line += fmt.Sprintf("  %q", q)
			}
			c.println(line)
		}
		for _, a := range rows(res.Get("allowed_by")) {
			c.printf("  %-16s rule %d  allow %s  [%s]\n", "allowed-by", a.Int("id"), a.Str("rule"), a.Str("scope"))
		}
		if h := res.Str("hint"); h != "" {
			c.println("hint: " + h)
		}
	}
	if code != 0 {
		return exitCode(code)
	}
	return nil
}
