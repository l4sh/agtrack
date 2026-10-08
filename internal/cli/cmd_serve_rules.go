package cli

import (
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/web"
)

// The browse app's Rules and Lessons views (agent-features spec H4). Reads
// and writes run the `rules`, `rule`, `lessons` and `lesson` verbs in-process
// as the --as actor, so the approver gate, the similar-rule check and the
// quote requirement are the CLI's own.

// scopeArgv turns a scope (global, context, project, task) and its ref into
// the CLI's scope flags.
func scopeArgv(scope, ref string) ([]string, error) {
	ref = strings.TrimSpace(ref)
	switch scope {
	case "global":
		return []string{"--global"}, nil
	case "context", "project", "task":
		if ref == "" {
			return nil, errs.New("scope %s needs a ref (a %s)", scope, map[string]string{"context": "context name", "project": "project path", "task": "task id"}[scope])
		}
		return []string{"--" + scope + "=" + ref}, nil
	}
	return nil, errs.New("scope must be global, context, project or task")
}

func (b *browser) rules(q map[string][]string) (any, error) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	scope := get("scope")
	if scope == "" {
		scope = "global"
	}
	s := b.store("")
	n, err := s.CountProposedRules()
	if err != nil {
		return nil, err
	}
	out := row.Of("scope", scope, "ref", get("ref"), "viewer", b.as, "approver", s.IsApprover(b.as), "proposed_total", int64(n))
	if scope == "proposed" {
		rs, err := s.ProposedRules()
		if err != nil {
			return nil, err
		}
		list := make([]any, 0, len(rs))
		for _, r := range rs {
			list = append(list, r)
		}
		out.Set("rules", list)
		return out, nil
	}
	argv, err := scopeArgv(scope, get("ref"))
	if err != nil {
		return nil, err
	}
	argv = append([]string{"rules"}, argv...)
	if v := get("all"); v == "1" || v == "true" {
		argv = append(argv, "--all")
	}
	rs, err := b.exec(argv...)
	if err != nil {
		return nil, err
	}
	out.Set("rules", asList(rs))
	return out, nil
}

func (b *browser) lessons(q map[string][]string) (any, error) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	scope := get("scope")
	if scope == "" {
		scope = "global"
	}
	argv, err := scopeArgv(scope, get("ref"))
	if err != nil {
		return nil, err
	}
	argv = append([]string{"-x", "lessons"}, argv...)
	if v := get("retired"); v == "1" || v == "true" {
		argv = append(argv, "--retired")
	}
	if v := get("q"); v != "" {
		argv = append(argv, "--q="+v)
	}
	ls, err := b.exec(argv...)
	if err != nil {
		return nil, err
	}
	return row.Of("scope", scope, "ref", get("ref"), "viewer", b.as, "lessons", asList(ls)), nil
}

func (b *browser) ruleAdd(in web.BrowseInput) (any, error) {
	if in.Kind == "" {
		return nil, errs.New("a rule needs a kind: deny, allow, require or remember")
	}
	sc, err := scopeArgv(in.Scope, in.ScopeRef)
	if err != nil {
		return nil, err
	}
	verb := "add"
	if in.Propose {
		verb = "propose"
	}
	argv := append(append([]string{"rule", verb}, sc...), idemFlag(in)...)
	for flag, v := range map[string]string{"--path": in.Path, "--repo": in.Repo, "--until": in.Until, "--quote": in.Quote} {
		if strings.TrimSpace(v) != "" {
			argv = append(argv, flag+"="+v)
		}
	}
	for flag, on := range map[string]bool{"--hard": in.Hard, "--overridable": in.Overridable, "--force": in.Force} {
		if on {
			argv = append(argv, flag)
		}
	}
	argv = append(argv, "--", in.Kind)
	for _, w := range []string{in.Verb, in.Text} {
		if strings.TrimSpace(w) != "" {
			argv = append(argv, w)
		}
	}
	return b.exec(argv...)
}

func (b *browser) lessonAdd(in web.BrowseInput) (any, error) {
	if strings.TrimSpace(in.Title) == "" {
		return nil, errs.New("a lesson needs a title")
	}
	sc, err := scopeArgv(in.Scope, in.ScopeRef)
	if err != nil {
		return nil, err
	}
	argv := append(append([]string{"lesson", "add"}, sc...), idemFlag(in)...)
	if strings.TrimSpace(in.Body) != "" {
		argv = append(argv, "--body="+in.Body)
	}
	if in.Kind != "" {
		argv = append(argv, "--kind="+in.Kind)
	}
	for flag, vs := range map[string][]string{"--path": in.Paths, "--cmd": in.Cmds, "--topic": in.Topics} {
		for _, v := range vs {
			if strings.TrimSpace(v) != "" {
				argv = append(argv, flag+"="+strings.TrimSpace(v))
			}
		}
	}
	if in.Force {
		argv = append(argv, "--force")
	}
	return b.exec(append(argv, "--", in.Title)...)
}
