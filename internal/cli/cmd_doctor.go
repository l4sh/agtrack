package cli

import (
	"fmt"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/render"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

// infoUsage is `info --usage [--since 30d]` (G2): events per actor and per
// command family (store.UsageFamily documents the mapping).
func infoUsage(c *Ctx) error {
	since := ""
	if c.Has("since") {
		d, err := parseDuration(c.Str("since"))
		if err != nil {
			return err
		}
		since = refs.ToUTCISO(refs.Now().Add(-d))
	}
	res, err := c.S.Usage(since)
	if err != nil {
		return err
	}
	if c.structured() {
		return c.emitObject(res)
	}
	line := func(key, list string) string {
		s := ""
		rows, _ := res.Get(list).([]*row.Row)
		for _, r := range rows {
			if s != "" {
				s += ", "
			}
			s += fmt.Sprintf("%s %d", r.Str(key), r.Int("events"))
		}
		return s
	}
	return render.EmitDetail(c.Out, []render.Section{
		{Label: "events", Value: fmt.Sprint(res.Int("events"))},
		{Label: "no actor", Value: fmt.Sprint(res.Int("no_actor"))},
		{Label: "actors", Value: line("actor", "actors")},
		{Label: "families", Value: line("family", "families")},
	}, c.Color)
}

// --- doctor (G1) ---------------------------------------------------------------------

// doctor is read-only: every finding comes from logic other verbs already
// use (staleness, claims, unacked notes, actor registry, lessons, rules,
// the git context check). Task-bound findings follow the scope (-p, -c,
// --all-contexts); actor, lesson and rule findings are machine-wide.
func cmdDoctor(c *Ctx) error {
	var project *row.Row
	var err error
	if c.Has("project") {
		if project, err = c.resolveProject(c.Str("project"), true); err != nil {
			return err
		}
	}
	quiet, err := parseDuration(c.Str("claim_quiet"))
	if err != nil {
		return err
	}
	days, err := c.staleDays()
	if err != nil {
		return err
	}
	findings := []*row.Row{}
	add := func(typ string, taskID int64, msg, suggested string) {
		var tid any
		if taskID != 0 {
			tid = taskID
		}
		findings = append(findings, row.Of("type", typ, "task", tid, "message", msg, "suggested", suggested))
	}
	f := store.TaskFilter{Context: c.Context, Project: project, AllContexts: c.Bool("all_contexts"), Order: "id"}

	// open tasks: finished_parent_open, stale_task, stale_claim, unacked_handoff, branch_mismatch
	f.Statuses = []string{"in-progress", "todo"}
	open, _, err := c.S.ListTasks(f)
	if err != nil {
		return err
	}
	inScope := map[int64]bool{}
	for _, t := range open {
		inScope[t.Int("id")] = true
	}
	parked := map[int64]bool{}
	if sn, _, err := c.S.ListTasks(store.TaskFilter{Context: f.Context, Project: project, AllContexts: f.AllContexts,
		Statuses: f.Statuses, OnlySnoozed: true}); err == nil {
		for _, t := range sn {
			parked[t.Int("id")] = true
		}
	}
	var prog []*row.Row
	for _, t := range open {
		if t.Str("status") == "in-progress" && !parked[t.Int("id")] {
			prog = append(prog, t)
		}
	}
	if prog, err = c.staleRows(prog, days); err != nil {
		return err
	}
	for _, t := range prog {
		id := t.Int("id")
		switch reason := t.Str("stale_reason"); reason {
		case "":
		case "all-steps-done":
			add("finished_parent_open", id, fmt.Sprintf("#%d has every step done but is still in progress", id), fmt.Sprintf("agtrack task done %d", id))
		default:
			add("stale_task", id, fmt.Sprintf("#%d is stale (%s, last event %s)", id, reason, t.Str("last_event")), fmt.Sprintf("agtrack task snooze %d --until 7d", id))
		}
	}
	claims, err := c.S.Claims(store.ClaimsFilter{StaleSince: refs.Now().Add(-quiet)})
	if err != nil {
		return err
	}
	for _, cl := range claims {
		id := cl.Int("id")
		if !inScope[id] {
			continue
		}
		add("stale_claim", id, fmt.Sprintf("#%d claimed by %s, quiet for %s", id, cl.Str("holder"), cl.Str("quiet")), fmt.Sprintf("agtrack task release %d --force", id))
	}
	seenNote := map[int64]bool{}
	for _, t := range open {
		notes, err := c.S.UnackedHandoffs(t.Int("project_id"), t.Int("id"))
		if err != nil {
			return err
		}
		for _, n := range notes {
			if n.Str("kind") != "handoff" || seenNote[n.Int("id")] || n.Str("target_type") == "project" {
				continue
			}
			seenNote[n.Int("id")] = true
			add("unacked_handoff", t.Int("id"), fmt.Sprintf("handoff note %d by %s on #%d is not acknowledged", n.Int("id"), n.Str("actor"), t.Int("id")), fmt.Sprintf("agtrack note ack %d", n.Int("id")))
		}
	}
	for _, t := range prog {
		if !claimMine(c, t) {
			continue
		}
		arts, err := c.S.Artifacts(t.Int("id"))
		if err != nil {
			return err
		}
		if res, ok, checked := contextCheck(t, arts); checked && !ok {
			add("branch_mismatch", t.Int("id"), res.Str("message"), fmt.Sprintf("agtrack task verify-context %d", t.Int("id")))
		}
	}

	// done tasks with a branch and no PR, closed recently: years of history
	// before PR links existed would otherwise drown the report.
	f.Statuses = []string{"done"}
	f.Since = refs.ToUTCISO(refs.Now().AddDate(0, 0, -30))
	done, _, err := c.S.ListTasks(f)
	if err != nil {
		return err
	}
	for _, t := range done {
		if yes, err := c.S.HasBranchNoPR(t.Int("id")); err == nil && yes {
			add("missing_pr", t.Int("id"), fmt.Sprintf("#%d is done with a branch artifact and no PR", t.Int("id")), fmt.Sprintf("agtrack pr add %d <url>", t.Int("id")))
		}
	}

	// machine-wide
	aliased, unknown, err := c.S.ActorDrift()
	if err != nil {
		return err
	}
	for _, a := range aliased {
		add("actor_alias_in_use", 0, fmt.Sprintf("%q is an alias of %s but was written %d times", a.Str("name"), a.Str("slug"), a.Int("uses")),
			fmt.Sprintf("agtrack actor merge %s --into %s --yes", a.Str("name"), a.Str("slug")))
	}
	for _, a := range unknown {
		add("unknown_actor", 0, fmt.Sprintf("%q wrote %d times and is not in the actor registry", a.Str("name"), a.Int("uses")),
			fmt.Sprintf("agtrack actor add %s", a.Str("name")))
	}
	lessons, err := c.S.StaleLessons()
	if err != nil {
		return err
	}
	for _, l := range lessons {
		add("stale_lesson", 0, fmt.Sprintf("lesson %d %q has not been verified since %s", l.Int("id"), l.Str("title"), l.Str("verified")),
			fmt.Sprintf("agtrack lesson verify %d", l.Int("id")))
	}
	rules, err := c.S.ProposedRules()
	if err != nil {
		return err
	}
	for _, r := range rules {
		add("proposed_rule", 0, fmt.Sprintf("rule %d waits for approval: %s", r.Int("id"), r.Str("summary")), fmt.Sprintf("agtrack rule approve %d", r.Int("id")))
	}

	if err := c.emit(findings, "finding", len(findings)); err != nil {
		return err
	}
	if len(findings) > 0 && c.Bool("strict") {
		return exitCode(errs.ExitConflict)
	}
	return nil
}
