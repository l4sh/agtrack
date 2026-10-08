package cli

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/l4sh/agtrack/internal/cli/argp"
	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

// resume caps (bounded mode). Anything cut is listed in `omitted` with the
// command that retrieves it: an omission is never rendered as "nothing".
const (
	resumeNotes    = 5
	resumeRecent   = 5
	resumeHeld     = 3
	resumeStepsHi  = 12
	resumeRules    = 5
	resumeLessons  = 5
	resumeClaims   = 5
	resumeHandoffs = 5
	resumeDelta    = 50
)

// resumeSections are the names `--section` takes (agent-features B4), each
// with the stable keys it fills.
var resumeSections = []struct {
	name string
	keys []string
}{
	{"task", []string{"current_task"}},
	{"steps", []string{"next_step", "steps"}},
	{"checkpoint", []string{"checkpoint"}},
	{"handoffs", []string{"unacked_handoffs"}},
	{"notes", []string{"open_notes"}},
	{"rules", []string{"rules"}},
	{"lessons", []string{"lessons"}},
	{"claims", []string{"claims"}},
	{"artifacts", []string{"artifacts"}},
	{"prs", []string{"prs"}},
	{"evidence", []string{"evidence"}},
	{"stale", []string{"stale"}},
	{"log", []string{"recent"}},
}

func resumeSectionNames() []string {
	out := make([]string, len(resumeSections))
	for i, s := range resumeSections {
		out[i] = s.name
	}
	return out
}

// resumeOutput documents `resume -f json` (B6). The stable keys come first
// and are always present; the rest are kept from earlier releases.
var resumeOutput = []argp.OutputKey{
	{Key: "project", Help: "project path (always present)"},
	{Key: "current_task", Help: "the task resumed: id, status, title, claim (claim_mine is false for this actor's claim from another session); null when none is in progress"},
	{Key: "next_step", Help: "first in-progress step, else first todo step; null when none"},
	{Key: "checkpoint", Help: "latest checkpoint: n, next, age, actor, session; body only with --complete or --section checkpoint; null when none"},
	{Key: "unacked_handoffs", Help: "unresolved, unacknowledged handoff/blocker/question notes on the task and project"},
	{Key: "open_notes", Help: "the task's other unresolved notes, typed first"},
	{Key: "rules", Help: "active deny/require/hard rules in scope (task, project and ancestors, context, global), plus allows from the last 7 days; at most 5"},
	{Key: "lessons", Help: "active lessons in scope, titles only; at most 5"},
	{Key: "claims", Help: "live claims in the project: id, claimed_by, session, mine, until, last_seen"},
	{Key: "artifacts", Help: "the task's branch/commit/file/url/run/worktree artifacts"},
	{Key: "prs", Help: "the task's pull requests"},
	{Key: "evidence", Help: "evidence recorded with the task's last close"},
	{Key: "stale", Help: "why the task looks stale, or null"},
	{Key: "context_mismatch", Help: "set when the cwd's branch or worktree differs from the task's artifacts: branches, worktrees, cwd_branch, cwd_worktree, message; else null"},
	{Key: "cursor", Help: "max event id seen; pass it to `resume --since` or `watch --after`"},
	{Key: "omitted", Help: "what was left out: what, count, retrieve (the command that gets it)"},
	{Key: "repos, resolved_by, task, next, steps, notes, blocked_by, spec, candidates, held, review, recent", Help: "earlier keys, kept unchanged (task = current_task; held = claims by others)"},
	{Key: "since (with --since)", Help: "delta mode returns project, current_task, since, cursor, events, notes, checkpoints, claims, status, omitted"},
}

func resumeFlags() []argp.Flag {
	return []argp.Flag{
		{Long: "--project", Short: "-p", Metavar: "REF"}, {Long: "--task", Short: "-t", Metavar: "ID"},
		{Long: "--complete", Kind: argp.Bool, Help: "everything in scope, uncapped, including linked spec text and the checkpoint body"},
		{Long: "--section", Metavar: "LIST", Help: "only these sections, uncapped: " + strings.Join(resumeSectionNames(), ",")},
		{Long: "--since", Kind: argp.Int, Metavar: "CURSOR", Help: "only what changed after this cursor (from a previous resume -f json)"},
	}
}

// cmdResume is the one command a cold agent runs (design §4.1).
//
// Selection rule: -t wins; else the project (-p, else the cwd remote, else
// the pin); among its in-progress tasks, the one this actor claims from this
// session, else the only one, else the most recently updated (the others are
// listed as candidates, never silently dropped).
func cmdResume(c *Ctx) error {
	sections, err := parseSections(c)
	if err != nil {
		return err
	}
	_, delta := c.V["since"].(int)
	if delta && sections != nil {
		return errs.Usage("--since and --section do not combine: a delta has its own keys")
	}
	if delta && c.Int("since") < 0 {
		return errs.Usage("--since takes a cursor (an event id, 0 or more)")
	}
	var packet *row.Row
	err = c.S.DB.ReadTx(func() error { // one snapshot for every part of the packet
		var err error
		switch {
		case delta:
			packet, err = buildDelta(c, int64(c.Int("since")))
		default:
			packet, err = buildResume(c, c.Bool("complete") || sections != nil, sections)
		}
		return err
	})
	if err != nil {
		return err
	}
	if sections != nil {
		packet = sectionPacket(c, packet, sections)
	}
	if c.structured() {
		return c.emitObject(packet)
	}
	switch {
	case delta:
		printDelta(c, packet)
	case sections != nil:
		printSections(c, packet)
	default:
		printResume(c, packet)
	}
	return nil
}

func parseSections(c *Ctx) (map[string]bool, error) {
	if !c.Has("section") {
		return nil, nil
	}
	known := map[string]bool{}
	for _, n := range resumeSectionNames() {
		known[n] = true
	}
	out := map[string]bool{}
	for _, s := range splitList(c.Str("section")) {
		if !known[s] {
			return nil, errs.Usage("unknown section %q; choose from %s", s, strings.Join(resumeSectionNames(), ","))
		}
		out[s] = true
	}
	if len(out) == 0 {
		return nil, errs.Usage("--section needs at least one of %s", strings.Join(resumeSectionNames(), ","))
	}
	return out, nil
}

func resumeProject(c *Ctx) (*row.Row, string, error) {
	if c.Has("project") {
		p, err := c.resolveProject(c.Str("project"), true)
		return p, "flag", err
	}
	if remote := c.S.CwdRepo(); remote != "" {
		matches, err := c.S.FindProjectsByRepo(remote)
		if err != nil {
			return nil, "", err
		}
		if len(matches) == 1 {
			return matches[0], "cwd", nil
		}
	}
	p, err := c.S.CurrentProject()
	if err != nil {
		return nil, "", err
	}
	if p == nil {
		return nil, "", errs.New("resume needs a project: run inside a linked repo, pass -p, or `agtrack project use <ref>`")
	}
	return p, "pin", nil
}

// resumeTarget resolves the project and task (nil when none is in
// progress) and the other in-progress tasks.
func resumeTarget(c *Ctx) (project, task *row.Row, how string, inProgress []*row.Row, err error) {
	if c.Has("task") {
		if task, err = c.S.GetTask(c.Str("task"), true); err != nil {
			return
		}
		if project, err = c.S.GetProject(task.Int("project_id")); err != nil {
			return
		}
		return project, task, "task", nil, nil
	}
	if project, how, err = resumeProject(c); err != nil {
		return
	}
	if inProgress, _, err = c.S.ListTasks(store.TaskFilter{Project: project, Statuses: []string{"in-progress"}, Order: "updated"}); err != nil {
		return
	}
	for _, t := range inProgress {
		if claimMine(c, t) {
			task = t
			break
		}
	}
	if task == nil && len(inProgress) > 0 {
		task = inProgress[0]
	}
	if task != nil {
		task, err = c.S.GetTaskID(task.Int("id"))
	}
	return
}

// resumeClaim is the claim part of a task object: holder, session and whether
// it is this actor's from this session.
func resumeClaim(c *Ctx, t, task *row.Row) {
	if holder := liveHolder(task); holder != "" {
		t.Set("claimed_by", holder)
		t.Set("claim_expires_at", task.Get("claim_expires_at"))
		if s := task.Str("claim_session"); s != "" {
			t.Set("claim_session", s)
		}
		t.Set("claim_mine", claimMine(c, task))
	} else if task.Str("claimed_by") != "" {
		t.Set("claim_expired", task.Str("claimed_by"))
	}
}

func buildResume(c *Ctx, complete bool, sections map[string]bool) (*row.Row, error) {
	type omission struct {
		section string
		row     *row.Row
	}
	var omissions []omission
	omit := func(section, what string, n int, cmd string) {
		if n > 0 {
			omissions = append(omissions, omission{section, row.Of("what", what, "count", int64(n), "retrieve", cmd)})
		}
	}
	project, task, how, inProgress, err := resumeTarget(c)
	if err != nil {
		return nil, err
	}
	path, _ := c.S.ProjectPath(project)
	repos, _ := c.S.ProjectRepos(project.Int("id"))
	var taskID int64
	tflag := ""
	if task != nil {
		taskID = task.Int("id")
		tflag = fmt.Sprintf(" -t %d", taskID)
	}
	// Key order is the documented order: stable keys, then the earlier ones.
	packet := row.Of("project", path, "repos", repos, "resolved_by", how,
		"current_task", nil, "next_step", nil, "checkpoint", nil, "context_mismatch", nil,
		"unacked_handoffs", []*row.Row{}, "open_notes", []*row.Row{}, "rules", []*row.Row{}, "lessons", []*row.Row{},
		"claims", []*row.Row{}, "artifacts", []*row.Row{}, "prs", []*row.Row{}, "evidence", []*row.Row{}, "stale", nil,
		"task", nil)

	candidates := []*row.Row{}
	for _, t := range inProgress {
		if task != nil && t.Int("id") != taskID {
			cand := row.Of("id", t.Int("id"), "title", t.Str("title"), "claimed_by", nilIfEmpty(liveHolder(t)))
			if s := claimOtherSession(c, t); s != "" {
				cand.Set("other_session", s)
			}
			candidates = append(candidates, cand)
		}
	}

	// Handoffs first (C1): unacknowledged ones on the task and the project.
	handoffs, err := c.S.UnackedHandoffs(project.Int("id"), taskID)
	if err != nil {
		return nil, err
	}
	unacked := map[int64]bool{}
	var hs []*row.Row
	for _, n := range handoffs {
		unacked[n.Int("id")] = true
		hs = append(hs, row.Of("id", n.Int("id"), "kind", n.Str("kind"), "on", n.Str("target_type"), "actor", n.Get("actor"),
			"body", firstLineOf(n.Str("body"), complete), "created_at", n.Str("created_at")))
	}
	if !complete && len(hs) > resumeHandoffs {
		omit("handoffs", "unacknowledged handoffs", len(hs)-resumeHandoffs, "agtrack notes -p "+path+" --kind handoff,blocker,question")
		hs = hs[:resumeHandoffs]
	}
	packet.Set("unacked_handoffs", nonNil(hs))

	if task == nil {
		next, total, _ := c.S.ListTasks(store.TaskFilter{Project: project, Statuses: []string{"todo"}, Ready: true, Unclaimed: true, Order: "id", Limit: 3})
		var nx []*row.Row
		for _, t := range next {
			nx = append(nx, row.Of("id", t.Int("id"), "title", t.Str("title")))
		}
		packet.Set("next", nonNil(nx))
		omit("task", "ready todo tasks", total-len(next), "agtrack task ls -p "+path+" -s todo --ready --unclaimed")
	} else {
		t := row.Of("id", taskID, "status", task.Str("status"), "title", task.Str("title"),
			"started_at", task.Get("started_at"), "started_by", nilIfEmpty(startedBy(c, taskID)))
		resumeClaim(c, t, task)
		packet.Set("task", t)
		packet.Set("current_task", t)

		steps, _, err := c.S.ListSubtasks(taskID, nil, 0)
		if err != nil {
			return nil, err
		}
		packet.Set("next_step", nextStep(steps))
		shown := steps
		if !complete && len(steps) > resumeStepsHi {
			// Keep open steps; collapse the done ones beyond the cap.
			var open []*row.Row
			for _, s := range steps {
				if s.Str("status") == "todo" || s.Str("status") == "in-progress" {
					open = append(open, s)
				}
			}
			shown = open
			omit("steps", "closed steps", len(steps)-len(open), fmt.Sprintf("agtrack sub ls %d -s closed", taskID))
		}
		var st []*row.Row
		for _, s := range shown {
			st = append(st, row.Of("ref", s.Str("ref"), "status", s.Str("status"), "title", s.Str("title")))
		}
		packet.Set("steps", nonNil(st))

		cp, err := c.S.Checkpoint(taskID, 0)
		if err != nil {
			return nil, err
		}
		if cp != nil {
			obj := row.Of("n", cp.Int("n"), "next", cp.Get("next"), "age", ageOf(cp.Str("created_at")),
				"actor", cp.Get("actor"), "session", cp.Get("session"), "created_at", cp.Str("created_at"))
			if complete {
				obj.Set("body", cp.Str("body"))
			} else if cp.Str("body") != "" {
				omit("checkpoint", "checkpoint body", 1, fmt.Sprintf("agtrack task checkpoint %d --show", taskID))
			}
			packet.Set("checkpoint", obj)
		}

		notes, err := c.S.OpenNotes(nil, taskID, nil)
		if err != nil {
			return nil, err
		}
		// Typed notes (handoffs, blockers, questions, decisions) first.
		var typed, plain []*row.Row
		for _, n := range notes {
			if n.Str("kind") != "note" {
				typed = append(typed, n)
			} else {
				plain = append(plain, n)
			}
		}
		ordered := append(typed, lastN(plain, 2)...)
		if len(plain) > 2 {
			omit("notes", "older plain notes", len(plain)-2, fmt.Sprintf("agtrack task notes %d", taskID))
		}
		if !complete && len(ordered) > resumeNotes {
			omit("notes", "notes", len(ordered)-resumeNotes, fmt.Sprintf("agtrack notes -t %d", taskID))
			ordered = ordered[:resumeNotes]
		}
		var ns, open []*row.Row
		for _, n := range ordered {
			r := row.Of("id", n.Int("id"), "kind", n.Str("kind"), "actor", n.Get("actor"), "body", firstLineOf(n.Str("body"), complete))
			ns = append(ns, r)
			if !unacked[n.Int("id")] {
				open = append(open, r)
			}
		}
		packet.Set("notes", nonNil(ns))
		packet.Set("open_notes", nonNil(open))

		deps, _ := c.S.DepsOf(taskID)
		var blocked []*row.Row
		for _, d := range deps {
			if d.Get("unmet") == true {
				blocked = append(blocked, row.Of("id", d.Int("depends_on_id"), "status", d.Str("status"), "title", d.Str("title")))
			}
		}
		packet.Set("blocked_by", nonNil(blocked))

		checks, err := c.S.SpecChecksForTask(taskID, complete)
		if err != nil {
			return nil, err
		}
		packet.Set("spec", nonNil(checks))

		arts, _ := c.S.Artifacts(taskID)
		var as []*row.Row
		for _, a := range arts {
			as = append(as, row.Of("kind", a.Str("kind"), "value", a.Str("value"), "label", a.Get("label")))
		}
		packet.Set("artifacts", nonNil(as))
		packet.Set("context_mismatch", contextMismatch(task, arts))

		prs, _ := c.S.PRsFor(taskID)
		var ps []*row.Row
		for _, p := range prs {
			ps = append(ps, row.Of("id", p.Int("id"), "url", p.Str("url"), "state", p.Get("state"), "title", p.Get("title")))
		}
		packet.Set("prs", nonNil(ps))

		ev, err := c.S.LastEvidence(taskID)
		if err != nil {
			return nil, err
		}
		var es []*row.Row
		for _, e := range ev {
			es = append(es, row.Of("kind", e.Str("kind"), "value", e.Str("value"), "actor", e.Get("actor"), "created_at", e.Str("created_at")))
		}
		packet.Set("evidence", nonNil(es))

		stale, err := resumeStale(c, task)
		if err != nil {
			return nil, err
		}
		packet.Set("stale", stale)
	}
	packet.Set("candidates", candidates)

	// Standing rules and lessons for the scope (H4): a few lines, the rest named.
	ruleLimit, lessonLimit := resumeRules, resumeLessons
	if complete {
		ruleLimit, lessonLimit = 0, 0
	}
	rules, rtotal, err := c.S.ResumeRules(project.Int("id"), taskID, ruleLimit)
	if err != nil {
		return nil, err
	}
	var rs []*row.Row
	for _, r := range rules {
		rs = append(rs, row.Of("id", r.Int("id"), "kind", r.Str("kind"), "rule", ruleSummary(r), "scope", r.Str("scope_type"), "hard", r.Int("hard") == 1))
	}
	packet.Set("rules", nonNil(rs))
	omit("rules", "rules", rtotal-len(rules), "agtrack rules -p "+path)
	lessons, ltotal, err := c.S.ResumeLessons(project.Int("id"), taskID, lessonLimit)
	if err != nil {
		return nil, err
	}
	var ls []*row.Row
	for _, l := range lessons {
		ls = append(ls, row.Of("id", l.Int("id"), "title", l.Str("title"), "scope", l.Str("scope_type")))
	}
	packet.Set("lessons", nonNil(ls))
	omit("lessons", "lessons", ltotal-len(lessons), "agtrack lessons -p "+path)

	// Live claims in the project, this actor's other sessions included (B1).
	claims, err := c.S.ProjectClaims(project.Int("id"))
	if err != nil {
		return nil, err
	}
	var cs, held []*row.Row
	for _, t := range claims {
		mine := claimMine(c, t)
		cs = append(cs, row.Of("id", t.Int("id"), "claimed_by", t.Str("claimed_by"), "session", t.Get("claim_session"), "mine", mine,
			"until", t.Get("claim_expires_at"), "last_seen", t.Get("claim_last_seen")))
	}
	if !complete && len(cs) > resumeClaims {
		omit("claims", "claims", len(cs)-resumeClaims, "agtrack task ls -p "+path+" -x")
		cs = cs[:resumeClaims]
	}
	packet.Set("claims", nonNil(cs))

	// Tasks in this project held by others (another session of this actor
	// counts as another holder).
	open, _, _ := c.S.ListTasks(store.TaskFilter{Project: project, Statuses: refs.OpenStatuses, Order: "updated"})
	for _, t := range open {
		holder := liveHolder(t)
		if holder == "" || claimMine(c, t) {
			continue
		}
		h := row.Of("id", t.Int("id"), "claimed_by", holder, "until", t.Get("claim_expires_at"))
		if s := claimOtherSession(c, t); s != "" {
			h.Set("other_session", s)
		}
		held = append(held, h)
	}
	if !complete && len(held) > resumeHeld {
		omit("claims", "claims by others", len(held)-resumeHeld, "agtrack task ls -p "+path+" -x")
		held = held[:resumeHeld]
	}
	packet.Set("held", nonNil(held))

	// Spec review state for the project's specs: pending comments and approvals.
	review, err := c.S.SpecReviewState(project.Int("id"))
	if err != nil {
		return nil, err
	}
	packet.Set("review", nonNil(review))

	limit := resumeRecent
	if complete {
		limit = 50
	}
	f := store.EventFilter{Project: project, Limit: limit, Ascending: true}
	if task != nil {
		f = store.EventFilter{Task: task, Limit: limit, Ascending: true}
	}
	events, total, err := c.S.ListEvents(f)
	if err != nil {
		return nil, err
	}
	var rec []*row.Row
	for _, e := range events {
		rec = append(rec, eventRow(e))
	}
	packet.Set("recent", nonNil(rec))
	if task != nil {
		omit("log", "older worklog entries", total-len(events), fmt.Sprintf("agtrack log -t %d -n %d", taskID, total))
	} else {
		omit("log", "older worklog entries", total-len(events), "agtrack log -p "+path)
	}
	cursor, _ := c.S.MaxEventID()
	packet.Set("cursor", cursor)
	omitted := []*row.Row{}
	for _, o := range omissions {
		if sections == nil || sections[o.section] {
			omitted = append(omitted, o.row)
		}
	}
	if sections != nil {
		// Every section left out is named, with the command that returns it.
		for _, s := range resumeSections {
			if sections[s.name] {
				continue
			}
			n := 0
			for _, k := range s.keys {
				n += sectionCount(packet.Get(k))
			}
			if n > 0 {
				omitted = append(omitted, row.Of("what", "section "+s.name, "count", int64(n), "retrieve", "agtrack resume --section "+s.name+tflag))
			}
		}
	}
	packet.Set("omitted", omitted)
	return packet, nil
}

// sectionPacket keeps project, the requested sections' keys, cursor and the
// omission manifest.
func sectionPacket(c *Ctx, p *row.Row, sections map[string]bool) *row.Row {
	out := row.Of("project", p.Get("project"))
	for _, s := range resumeSections {
		if sections[s.name] {
			for _, k := range s.keys {
				out.Set(k, p.Get(k))
			}
		}
	}
	if p.Get("context_mismatch") != nil {
		out.Set("context_mismatch", p.Get("context_mismatch"))
	}
	out.Set("cursor", p.Get("cursor"))
	out.Set("omitted", p.Get("omitted"))
	return out
}

func sectionCount(v any) int {
	switch x := v.(type) {
	case nil:
		return 0
	case []*row.Row:
		return len(x)
	default:
		return 1
	}
}

func nextStep(steps []*row.Row) *row.Row {
	var todo *row.Row
	for _, s := range steps {
		switch s.Str("status") {
		case "in-progress":
			return row.Of("ref", s.Str("ref"), "status", s.Str("status"), "title", s.Str("title"))
		case "todo":
			if todo == nil {
				todo = row.Of("ref", s.Str("ref"), "status", s.Str("status"), "title", s.Str("title"))
			}
		}
	}
	return todo
}

func ruleSummary(r *row.Row) string {
	var parts []string
	for _, k := range []string{"verb", "check_name", "text"} {
		if v := r.Str(k); v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, " ")
}

func eventRow(e *row.Row) *row.Row {
	return row.Of("id", e.Int("id"), "at", e.Str("at"), "actor", e.Get("actor"), "target", e.Str("target"),
		"action", e.Str("action"), "detail", e.Get("detail"))
}

// ageOf renders how long ago a stored timestamp was: 45s, 12m, 3h, 2d.
func ageOf(ts string) string {
	t, err := time.Parse("2006-01-02T15:04:05Z", ts)
	if err != nil {
		return ""
	}
	d := refs.Now().Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// buildDelta is `resume --since CURSOR` (B3): only what changed on the
// resolved project and task after the cursor.
func buildDelta(c *Ctx, since int64) (*row.Row, error) {
	project, task, _, _, err := resumeTarget(c)
	if err != nil {
		return nil, err
	}
	path, _ := c.S.ProjectPath(project)
	omitted := []*row.Row{}
	packet := row.Of("project", path, "current_task", nil, "since", since)
	var taskID int64
	if task != nil {
		taskID = task.Int("id")
		t := row.Of("id", taskID, "status", task.Str("status"), "title", task.Str("title"))
		resumeClaim(c, t, task)
		packet.Set("current_task", t)
	}
	cursor, err := c.S.MaxEventID()
	if err != nil {
		return nil, err
	}
	limit := resumeDelta
	if c.Bool("complete") {
		limit = 0
	}
	events, total, err := c.S.ListEvents(store.EventFilter{Project: project, AfterID: since, ByID: true, Limit: limit})
	if err != nil {
		return nil, err
	}
	if len(events) < total {
		// The cursor stops at the last event shown, so the next delta
		// continues where this one ends.
		cursor = events[len(events)-1].Int("id")
		omitted = append(omitted, row.Of("what", "later events", "count", int64(total-len(events)),
			"retrieve", fmt.Sprintf("agtrack resume --since %d", cursor)))
	}
	var evs []*row.Row
	claimed, statused := map[int64]bool{}, map[int64]bool{}
	var claimIDs, statusIDs []int64
	for _, e := range events {
		evs = append(evs, eventRow(e))
		id := e.Int("task_id")
		if id == 0 {
			continue
		}
		switch e.Str("action") {
		case "claim", "release", "transfer":
			if !claimed[id] {
				claimed[id] = true
				claimIDs = append(claimIDs, id)
			}
		case "status":
			if e.Str("target_type") == "task" && !statused[id] {
				statused[id] = true
				statusIDs = append(statusIDs, id)
			}
			if !claimed[id] { // closing a task releases its claim
				claimed[id] = true
				claimIDs = append(claimIDs, id)
			}
		}
	}
	packet.Set("cursor", cursor)
	packet.Set("events", nonNil(evs))

	at, err := c.S.EventTime(since)
	if err != nil {
		return nil, err
	}
	notes, err := c.S.NotesSince(project.Int("id"), taskID, at)
	if err != nil {
		return nil, err
	}
	var ns []*row.Row
	for _, n := range notes {
		ns = append(ns, row.Of("id", n.Int("id"), "kind", n.Str("kind"), "on", n.Str("target_type"), "actor", n.Get("actor"),
			"body", firstLineOf(n.Str("body"), c.Bool("complete")), "created_at", n.Str("created_at"),
			"resolved", !n.IsNull("resolved_at"), "acknowledged", !n.IsNull("acknowledged_at")))
	}
	packet.Set("notes", nonNil(ns))

	cps := []*row.Row{}
	if taskID != 0 {
		if cps, err = c.S.CheckpointsSince(taskID, at); err != nil {
			return nil, err
		}
	}
	packet.Set("checkpoints", cps)

	var cl []*row.Row
	sort.Slice(claimIDs, func(i, j int) bool { return claimIDs[i] < claimIDs[j] })
	for _, id := range claimIDs {
		t, err := c.S.GetTaskID(id)
		if err != nil {
			continue // deleted since
		}
		r := row.Of("id", id, "claimed_by", nilIfEmpty(liveHolder(t)))
		if liveHolder(t) != "" {
			r.Set("session", t.Get("claim_session")).Set("mine", claimMine(c, t)).Set("until", t.Get("claim_expires_at"))
		}
		cl = append(cl, r)
	}
	packet.Set("claims", nonNil(cl))
	var sts []*row.Row
	for _, id := range statusIDs {
		t, err := c.S.GetTaskID(id)
		if err != nil {
			continue
		}
		sts = append(sts, row.Of("id", id, "status", t.Str("status"), "title", t.Str("title")))
	}
	packet.Set("status", nonNil(sts))
	packet.Set("omitted", omitted)
	return packet, nil
}

func lastN(rows []*row.Row, n int) []*row.Row {
	if len(rows) <= n {
		return rows
	}
	return rows[len(rows)-n:]
}

func firstLineOf(s string, complete bool) string {
	if complete {
		return s
	}
	line := strings.TrimSpace(s)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i] + " …"
	}
	if r := []rune(line); len(r) > 160 {
		line = string(r[:157]) + "..."
	}
	return line
}

func claimLive(t *row.Row) bool {
	if t.Str("claimed_by") == "" {
		return false
	}
	exp := t.Str("claim_expires_at")
	return exp == "" || exp > refs.NowISO()
}

func liveHolder(t *row.Row) string {
	if claimLive(t) {
		return t.Str("claimed_by")
	}
	return ""
}

func startedBy(c *Ctx, taskID int64) string {
	t, err := c.S.GetTaskID(taskID)
	if err != nil {
		return ""
	}
	events, _, err := c.S.ListEvents(store.EventFilter{Task: t, Actions: []string{"status"}, Limit: 0, Ascending: true})
	if err != nil {
		return ""
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Str("target_type") == "task" && strings.HasSuffix(e.Str("detail"), "-> in-progress") {
			return e.Str("actor")
		}
	}
	return ""
}

// --- text rendering ------------------------------------------------------------------

type resumePrinter struct{ c *Ctx }

func (p resumePrinter) line(label, text string) { p.c.printf("%-9s %s\n", label, text) }

// block prints lines under one label.
func (p resumePrinter) block(label string, lines []string) {
	for i, l := range lines {
		if i == 0 {
			p.line(label, l)
		} else {
			p.line("", l)
		}
	}
}

func rows(v any) []*row.Row {
	r, _ := v.([]*row.Row)
	return r
}

func claimText(t *row.Row) string {
	h := t.Str("claimed_by")
	if h == "" {
		return ""
	}
	if s := t.Str("claim_session"); s != "" && t.Get("claim_mine") == false {
		h += " (other session " + shortSession(s) + ")"
	}
	return h
}

func ruleLines(p *row.Row) []string {
	var out []string
	for _, r := range rows(p.Get("rules")) {
		text := fmt.Sprintf("%s %s  (rule %d, %s)", r.Str("kind"), r.Str("rule"), r.Int("id"), r.Str("scope"))
		if r.Get("hard") == true {
			text = "HARD " + text
		}
		out = append(out, text)
	}
	return out
}

func lessonLines(p *row.Row) []string {
	var out []string
	for _, l := range rows(p.Get("lessons")) {
		out = append(out, fmt.Sprintf("%s  (lesson %d)", l.Str("title"), l.Int("id")))
	}
	return out
}

func checkpointLine(cp *row.Row) string {
	text := fmt.Sprintf("v%d %s ago", cp.Int("n"), cp.Str("age"))
	if n := cp.Str("next"); n != "" {
		text += ": next " + n
	}
	return text
}

func noteLine(n *row.Row) string {
	mark := "-"
	switch n.Str("kind") {
	case "handoff":
		mark = ">"
	case "blocker":
		mark = "!"
	case "question":
		mark = "?"
	case "decision":
		mark = "="
	}
	return fmt.Sprintf("%s %s: %s  (note %d)", mark, render0(n.Get("actor")), n.Str("body"), n.Int("id"))
}

func eventLine(e *row.Row) string {
	ts := refs.FmtTS(e.Str("at"), false, false)
	if len(ts) > 5 {
		ts = ts[5:]
	}
	return strings.TrimRight(fmt.Sprintf("%s %s %s %s %s", ts, render0(e.Get("actor")), e.Str("target"), e.Str("action"), e.Str("detail")), " ")
}

func omittedLines(p *row.Row) []string {
	var out []string
	for _, o := range rows(p.Get("omitted")) {
		out = append(out, fmt.Sprintf("%d %s: %s", o.Int("count"), o.Str("what"), o.Str("retrieve")))
	}
	return out
}

func printResume(c *Ctx, p *row.Row) {
	pr := resumePrinter{c}
	proj := p.Str("project")
	if repos, _ := p.Get("repos").([]string); len(repos) > 0 {
		proj += "   repo " + strings.Join(repos, ",")
	}
	pr.line("project", proj)
	if m, _ := p.Get("context_mismatch").(*row.Row); m != nil {
		c.println(m.Str("message"))
	}
	pr.block("rules", ruleLines(p))
	pr.block("lessons", lessonLines(p))
	t, _ := p.Get("task").(*row.Row)
	if t == nil {
		pr.line("task", "none in progress")
		var next []string
		for _, n := range rows(p.Get("next")) {
			next = append(next, fmt.Sprintf("#%d %s", n.Int("id"), n.Str("title")))
		}
		pr.block("next", next)
	} else {
		text := fmt.Sprintf("#%d %s %s", t.Int("id"), refs.StatusMark[t.Str("status")], t.Str("title"))
		if s := t.Str("started_at"); s != "" {
			text += "   started " + refs.FmtTS(s, false, true)
			if by := t.Str("started_by"); by != "" {
				text += " by " + by
			}
		}
		pr.line("task", text)
		if h := claimText(t); h != "" {
			pr.line("claim", h+" until "+refs.FmtTS(t.Str("claim_expires_at"), false, false))
		} else if x := t.Str("claim_expired"); x != "" {
			pr.line("claim", "expired (was "+x+"); they may still be editing files")
		}
		if cp, _ := p.Get("checkpoint").(*row.Row); cp != nil {
			pr.line("checkpt", checkpointLine(cp))
		}
		if s := p.Get("stale"); s != nil {
			pr.line("stale", fmt.Sprint(s))
		}
		var lines []string
		for _, s := range rows(p.Get("steps")) {
			lines = append(lines, fmt.Sprintf("%-6s %s %s", s.Str("ref"), refs.StatusMark[s.Str("status")], s.Str("title")))
		}
		pr.block("steps", lines)
		lines = nil
		for _, n := range rows(p.Get("notes")) {
			lines = append(lines, noteLine(n))
		}
		pr.block("notes", lines)
		lines = nil
		for _, s := range rows(p.Get("spec")) {
			lines = append(lines, s.Str("section")+"  "+s.Str("state"))
		}
		pr.block("spec", lines)
		blocked := rows(p.Get("blocked_by"))
		if len(blocked) == 0 {
			pr.line("blocked", "none")
		}
		lines = nil
		for _, b := range blocked {
			lines = append(lines, fmt.Sprintf("by #%d %s %s", b.Int("id"), refs.StatusMark[b.Str("status")], b.Str("title")))
		}
		pr.block("blocked", lines)
		lines = nil
		for _, a := range rows(p.Get("artifacts")) {
			lines = append(lines, a.Str("kind")+" "+a.Str("value"))
		}
		pr.block("artifacts", lines)
		lines = nil
		for _, x := range rows(p.Get("prs")) {
			lines = append(lines, fmt.Sprintf("%s %s", render0(x.Get("state")), x.Str("url")))
		}
		pr.block("prs", lines)
		lines = nil
		for _, e := range rows(p.Get("evidence")) {
			lines = append(lines, e.Str("kind")+" "+e.Str("value"))
		}
		pr.block("evidence", lines)
	}
	var lines []string
	for _, cand := range rows(p.Get("candidates")) {
		text := fmt.Sprintf("#%d %s", cand.Int("id"), cand.Str("title"))
		if h := cand.Str("claimed_by"); h != "" {
			if s := cand.Str("other_session"); s != "" {
				h += ", other session " + shortSession(s)
			}
			text += "  (" + h + ")"
		}
		lines = append(lines, text)
	}
	pr.block("also", lines)
	lines = nil
	for _, h := range rows(p.Get("held")) {
		text := fmt.Sprintf("#%d claimed by %s", h.Int("id"), h.Str("claimed_by"))
		if s := h.Str("other_session"); s != "" {
			text += " (other session " + shortSession(s) + ")"
		}
		if u := h.Str("until"); u != "" {
			text += " until " + refs.FmtTS(u, false, false)
		}
		lines = append(lines, text)
	}
	pr.block("held", lines)
	lines = nil
	for _, r := range rows(p.Get("review")) {
		lines = append(lines, r.Str("summary"))
	}
	pr.block("review", lines)
	lines = nil
	for _, e := range rows(p.Get("recent")) {
		lines = append(lines, eventLine(e))
	}
	pr.block("recent", lines)
	pr.block("omitted", omittedLines(p))
}

// printSections renders `resume --section` output: one block per section.
func printSections(c *Ctx, p *row.Row) {
	pr := resumePrinter{c}
	pr.line("project", p.Str("project"))
	if m, _ := p.Get("context_mismatch").(*row.Row); m != nil {
		c.println(m.Str("message"))
	}
	for _, k := range p.Keys() {
		v := p.Get(k)
		switch k {
		case "project", "context_mismatch", "cursor", "omitted":
			continue
		case "current_task":
			if t, _ := v.(*row.Row); t != nil {
				text := fmt.Sprintf("#%d %s %s", t.Int("id"), refs.StatusMark[t.Str("status")], t.Str("title"))
				if h := claimText(t); h != "" {
					text += "  (claimed by " + h + ")"
				}
				pr.line("task", text)
			} else {
				pr.line("task", "none in progress")
			}
		case "next_step":
			if s, _ := v.(*row.Row); s != nil {
				pr.line("next", s.Str("ref")+" "+s.Str("title"))
			}
		case "checkpoint":
			if cp, _ := v.(*row.Row); cp != nil {
				pr.line("checkpt", checkpointLine(cp))
				if b := cp.Str("body"); b != "" {
					c.println(b)
				}
			} else {
				pr.line("checkpt", "none")
			}
		case "rules":
			pr.block("rules", ruleLines(p))
		case "lessons":
			pr.block("lessons", lessonLines(p))
		case "stale":
			if v != nil {
				pr.line("stale", fmt.Sprint(v))
			}
		default:
			var lines []string
			for _, r := range rows(v) {
				switch k {
				case "steps":
					lines = append(lines, fmt.Sprintf("%-6s %s %s", r.Str("ref"), refs.StatusMark[r.Str("status")], r.Str("title")))
				case "unacked_handoffs", "open_notes":
					lines = append(lines, noteLine(r))
				case "claims":
					text := fmt.Sprintf("#%d %s", r.Int("id"), r.Str("claimed_by"))
					if s := r.Str("session"); s != "" {
						text += " session " + shortSession(s)
					}
					if r.Get("mine") == true {
						text += " (mine)"
					}
					lines = append(lines, text)
				case "artifacts", "evidence":
					lines = append(lines, r.Str("kind")+" "+r.Str("value"))
				case "prs":
					lines = append(lines, fmt.Sprintf("%s %s", render0(r.Get("state")), r.Str("url")))
				case "recent":
					lines = append(lines, eventLine(r))
				}
			}
			label := map[string]string{"unacked_handoffs": "handoffs", "open_notes": "notes", "recent": "log"}[k]
			if label == "" {
				label = k
			}
			if len(lines) == 0 {
				lines = []string{"none"}
			}
			pr.block(label, lines)
		}
	}
	pr.line("cursor", fmt.Sprint(p.Get("cursor")))
	pr.block("omitted", omittedLines(p))
}

// printDelta renders `resume --since`.
func printDelta(c *Ctx, p *row.Row) {
	pr := resumePrinter{c}
	pr.line("project", p.Str("project"))
	if t, _ := p.Get("current_task").(*row.Row); t != nil {
		pr.line("task", fmt.Sprintf("#%d %s %s", t.Int("id"), refs.StatusMark[t.Str("status")], t.Str("title")))
	}
	pr.line("since", fmt.Sprintf("%v -> cursor %v", p.Get("since"), p.Get("cursor")))
	var lines []string
	for _, e := range rows(p.Get("events")) {
		lines = append(lines, eventLine(e))
	}
	if len(lines) == 0 {
		lines = []string{"nothing new"}
	}
	pr.block("events", lines)
	lines = nil
	for _, n := range rows(p.Get("notes")) {
		lines = append(lines, noteLine(n))
	}
	pr.block("notes", lines)
	lines = nil
	for _, cp := range rows(p.Get("checkpoints")) {
		text := fmt.Sprintf("v%d", cp.Int("n"))
		if n := cp.Str("next"); n != "" {
			text += ": next " + n
		}
		lines = append(lines, text)
	}
	pr.block("checkpt", lines)
	lines = nil
	for _, r := range rows(p.Get("claims")) {
		text := fmt.Sprintf("#%d released", r.Int("id"))
		if h := r.Str("claimed_by"); h != "" {
			text = fmt.Sprintf("#%d claimed by %s", r.Int("id"), h)
			if r.Get("mine") == true {
				text += " (mine)"
			}
		}
		lines = append(lines, text)
	}
	pr.block("claims", lines)
	lines = nil
	for _, s := range rows(p.Get("status")) {
		lines = append(lines, fmt.Sprintf("#%d %s %s", s.Int("id"), refs.StatusMark[s.Str("status")], s.Str("title")))
	}
	pr.block("status", lines)
	pr.block("omitted", omittedLines(p))
}

func render0(v any) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprint(v)
}
