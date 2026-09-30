package cli

import (
	"fmt"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

// resume caps (bounded mode). Anything cut is listed in `omitted` with the
// command that retrieves it: an omission is never rendered as "nothing".
const (
	resumeNotes   = 5
	resumeRecent  = 5
	resumeHeld    = 3
	resumeStepsHi = 12
)

// cmdResume is the one command a cold agent runs (design §4.1).
//
// Selection rule: -t wins; else the project (-p, else the cwd remote, else
// the pin); among its in-progress tasks, the one this actor claims, else the
// only one, else the most recently updated (the others are listed as
// candidates, never silently dropped).
func cmdResume(c *Ctx) error {
	var packet *row.Row
	err := c.S.DB.ReadTx(func() error { // one snapshot for every part of the packet
		var err error
		packet, err = buildResume(c)
		return err
	})
	if err != nil {
		return err
	}
	if c.structured() {
		return c.emitObject(packet)
	}
	printResume(c, packet)
	return nil
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

func buildResume(c *Ctx) (*row.Row, error) {
	complete := c.Bool("complete")
	omitted := []*row.Row{}
	omit := func(what string, n int, cmd string) {
		if n > 0 {
			omitted = append(omitted, row.Of("what", what, "count", int64(n), "retrieve", cmd))
		}
	}
	var task *row.Row
	var project *row.Row
	var how string
	var err error
	if c.Has("task") {
		if task, err = c.S.GetTask(c.Str("task"), true); err != nil {
			return nil, err
		}
		if project, err = c.S.GetProject(task.Int("project_id")); err != nil {
			return nil, err
		}
		how = "task"
	} else if project, how, err = resumeProject(c); err != nil {
		return nil, err
	}
	path, _ := c.S.ProjectPath(project)
	repos, _ := c.S.ProjectRepos(project.Int("id"))
	packet := row.Of("project", path, "repos", repos, "resolved_by", how)

	candidates := []*row.Row{}
	if task == nil {
		inProgress, _, err := c.S.ListTasks(store.TaskFilter{Project: project, Statuses: []string{"in-progress"}, Order: "updated"})
		if err != nil {
			return nil, err
		}
		for _, t := range inProgress {
			if c.Actor != "" && t.Str("claimed_by") == c.Actor && claimLive(t) {
				task = t
				break
			}
		}
		if task == nil && len(inProgress) > 0 {
			task = inProgress[0]
		}
		for _, t := range inProgress {
			if task != nil && t.Int("id") != task.Int("id") {
				candidates = append(candidates, row.Of("id", t.Int("id"), "title", t.Str("title"), "claimed_by", nilIfEmpty(liveHolder(t))))
			}
		}
		if task != nil {
			full, err := c.S.GetTaskID(task.Int("id"))
			if err != nil {
				return nil, err
			}
			task = full
		}
	}
	if task == nil {
		packet.Set("task", nil)
		next, total, _ := c.S.ListTasks(store.TaskFilter{Project: project, Statuses: []string{"todo"}, Ready: true, Unclaimed: true, Order: "id", Limit: 3})
		var nx []*row.Row
		for _, t := range next {
			nx = append(nx, row.Of("id", t.Int("id"), "title", t.Str("title")))
		}
		packet.Set("next", nonNil(nx))
		omit("ready todo tasks", total-len(next), "agtrack task ls -p "+shortRef(path)+" -s todo --ready --unclaimed")
	} else {
		id := task.Int("id")
		t := row.Of("id", id, "status", task.Str("status"), "title", task.Str("title"),
			"started_at", task.Get("started_at"), "started_by", nilIfEmpty(startedBy(c, id)))
		if holder := liveHolder(task); holder != "" {
			t.Set("claimed_by", holder)
			t.Set("claim_expires_at", task.Get("claim_expires_at"))
		} else if task.Str("claimed_by") != "" {
			t.Set("claim_expired", task.Str("claimed_by"))
		}
		packet.Set("task", t)

		steps, _, err := c.S.ListSubtasks(id, nil, 0)
		if err != nil {
			return nil, err
		}
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
			omit("closed steps", len(steps)-len(open), fmt.Sprintf("agtrack sub ls %d -s closed", id))
		}
		var st []*row.Row
		for _, s := range shown {
			st = append(st, row.Of("ref", s.Str("ref"), "status", s.Str("status"), "title", s.Str("title")))
		}
		packet.Set("steps", nonNil(st))

		notes, err := c.S.OpenNotes(nil, id, nil)
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
			omit("older plain notes", len(plain)-2, fmt.Sprintf("agtrack task notes %d", id))
		}
		if !complete && len(ordered) > resumeNotes {
			omit("notes", len(ordered)-resumeNotes, fmt.Sprintf("agtrack notes -t %d", id))
			ordered = ordered[:resumeNotes]
		}
		var ns []*row.Row
		for _, n := range ordered {
			ns = append(ns, row.Of("id", n.Int("id"), "kind", n.Str("kind"), "actor", n.Get("actor"), "body", firstLineOf(n.Str("body"), complete)))
		}
		packet.Set("notes", nonNil(ns))

		deps, _ := c.S.DepsOf(id)
		var blocked []*row.Row
		for _, d := range deps {
			if d.Get("unmet") == true {
				blocked = append(blocked, row.Of("id", d.Int("depends_on_id"), "status", d.Str("status"), "title", d.Str("title")))
			}
		}
		packet.Set("blocked_by", nonNil(blocked))

		checks, err := c.S.SpecChecksForTask(id, complete)
		if err != nil {
			return nil, err
		}
		packet.Set("spec", nonNil(checks))

		arts, _ := c.S.Artifacts(id)
		var as []*row.Row
		for _, a := range arts {
			as = append(as, row.Of("kind", a.Str("kind"), "value", a.Str("value"), "label", a.Get("label")))
		}
		packet.Set("artifacts", nonNil(as))
	}
	packet.Set("candidates", candidates)

	// Tasks in this project held by other actors.
	held, _, _ := c.S.ListTasks(store.TaskFilter{Project: project, Statuses: refs.OpenStatuses, Order: "updated"})
	var hs []*row.Row
	for _, t := range held {
		holder := liveHolder(t)
		if holder == "" || holder == c.Actor {
			continue
		}
		hs = append(hs, row.Of("id", t.Int("id"), "claimed_by", holder, "until", t.Get("claim_expires_at")))
	}
	if !complete && len(hs) > resumeHeld {
		omit("claims by others", len(hs)-resumeHeld, "agtrack task ls -p "+shortRef(path)+" -x")
		hs = hs[:resumeHeld]
	}
	packet.Set("held", nonNil(hs))

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
		rec = append(rec, row.Of("id", e.Int("id"), "at", e.Str("at"), "actor", e.Get("actor"), "target", e.Str("target"),
			"action", e.Str("action"), "detail", e.Get("detail")))
	}
	packet.Set("recent", nonNil(rec))
	if task != nil {
		omit("older worklog entries", total-len(events), fmt.Sprintf("agtrack log -t %d -n %d", task.Int("id"), total))
	} else {
		omit("older worklog entries", total-len(events), "agtrack log -p "+shortRef(path))
	}
	cursor, _ := c.S.MaxEventID()
	packet.Set("cursor", cursor)
	packet.Set("omitted", omitted)
	return packet, nil
}

func shortRef(path string) string { return path }

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

func printResume(c *Ctx, p *row.Row) {
	line := func(label, text string) { c.printf("%-9s %s\n", label, text) }
	cont := func(text string) { c.printf("%-9s %s\n", "", text) }
	proj := p.Str("project")
	if repos, _ := p.Get("repos").([]string); len(repos) > 0 {
		proj += "   repo " + strings.Join(repos, ",")
	}
	line("project", proj)
	t, _ := p.Get("task").(*row.Row)
	if t == nil {
		line("task", "none in progress")
		for i, n := range p.Get("next").([]*row.Row) {
			text := fmt.Sprintf("#%d %s", n.Int("id"), n.Str("title"))
			if i == 0 {
				line("next", text)
			} else {
				cont(text)
			}
		}
	} else {
		text := fmt.Sprintf("#%d %s %s", t.Int("id"), refs.StatusMark[t.Str("status")], t.Str("title"))
		if s := t.Str("started_at"); s != "" {
			text += "   started " + refs.FmtTS(s, false, true)
			if by := t.Str("started_by"); by != "" {
				text += " by " + by
			}
		}
		line("task", text)
		if h := t.Str("claimed_by"); h != "" {
			line("claim", h+" until "+refs.FmtTS(t.Str("claim_expires_at"), false, false))
		} else if x := t.Str("claim_expired"); x != "" {
			line("claim", "expired (was "+x+"); they may still be editing files")
		}
		for i, s := range p.Get("steps").([]*row.Row) {
			text := fmt.Sprintf("%-6s %s %s", s.Str("ref"), refs.StatusMark[s.Str("status")], s.Str("title"))
			if i == 0 {
				line("steps", text)
			} else {
				cont(text)
			}
		}
		for i, n := range p.Get("notes").([]*row.Row) {
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
			text := fmt.Sprintf("%s %s: %s  (note %d)", mark, render0(n.Get("actor")), n.Str("body"), n.Int("id"))
			if i == 0 {
				line("notes", text)
			} else {
				cont(text)
			}
		}
		for i, s := range p.Get("spec").([]*row.Row) {
			text := s.Str("section") + "  " + s.Str("state")
			if i == 0 {
				line("spec", text)
			} else {
				cont(text)
			}
		}
		blocked := p.Get("blocked_by").([]*row.Row)
		if len(blocked) == 0 {
			line("blocked", "none")
		}
		for i, b := range blocked {
			text := fmt.Sprintf("by #%d %s %s", b.Int("id"), refs.StatusMark[b.Str("status")], b.Str("title"))
			if i == 0 {
				line("blocked", text)
			} else {
				cont(text)
			}
		}
		for i, a := range p.Get("artifacts").([]*row.Row) {
			text := a.Str("kind") + " " + a.Str("value")
			if i == 0 {
				line("artifacts", text)
			} else {
				cont(text)
			}
		}
	}
	for i, cand := range p.Get("candidates").([]*row.Row) {
		text := fmt.Sprintf("#%d %s", cand.Int("id"), cand.Str("title"))
		if h := cand.Str("claimed_by"); h != "" {
			text += "  (" + h + ")"
		}
		if i == 0 {
			line("also", text)
		} else {
			cont(text)
		}
	}
	for i, h := range p.Get("held").([]*row.Row) {
		text := fmt.Sprintf("#%d claimed by %s", h.Int("id"), h.Str("claimed_by"))
		if u := h.Str("until"); u != "" {
			text += " until " + refs.FmtTS(u, false, false)
		}
		if i == 0 {
			line("held", text)
		} else {
			cont(text)
		}
	}
	for i, r := range p.Get("review").([]*row.Row) {
		if i == 0 {
			line("review", r.Str("summary"))
		} else {
			cont(r.Str("summary"))
		}
	}
	for i, e := range p.Get("recent").([]*row.Row) {
		text := fmt.Sprintf("%s %s %s %s %s", refs.FmtTS(e.Str("at"), false, false)[5:], render0(e.Get("actor")), e.Str("target"), e.Str("action"), e.Str("detail"))
		text = strings.TrimRight(text, " ")
		if i == 0 {
			line("recent", text)
		} else {
			cont(text)
		}
	}
	for i, o := range p.Get("omitted").([]*row.Row) {
		text := fmt.Sprintf("%d %s: %s", o.Int("count"), o.Str("what"), o.Str("retrieve"))
		if i == 0 {
			line("omitted", text)
		} else {
			cont(text)
		}
	}
}

func render0(v any) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprint(v)
}
