package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/render"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

// --- tasks ---------------------------------------------------------------------------

func taskAdd(c *Ctx) error {
	show := func(ids []int64) error {
		task, err := c.S.GetTaskID(ids[0])
		if err != nil {
			return err
		}
		obj, _ := c.S.TaskDict(task)
		return c.created("task", ids[0], obj, "task")
	}
	if done, err := c.replay(show); done {
		return err
	}
	project, err := c.resolveProject(c.Str("project"), true)
	if err != nil {
		return err
	}
	status := "todo"
	if c.Has("status") {
		if status, err = refs.NormalizeStatus(c.Str("status")); err != nil {
			return err
		}
	}
	in := store.NewTask{Title: c.Str("title"), Status: status, Tags: c.Tags("tag"), Note: c.Str("note")}
	if in.Created, err = c.When("created"); err != nil {
		return err
	}
	if in.Started, err = c.When("started"); err != nil {
		return err
	}
	if in.Closed, err = c.When("closed"); err != nil {
		return err
	}
	var pins []*pendingPin // pin write-backs, applied only after the commit
	ids, err := c.record("task", func() ([]int64, error) {
		var task *row.Row
		err := c.S.Tx(func() error {
			var err error
			if task, err = c.S.CreateTask(project, in); err != nil {
				return err
			}
			for _, url := range c.Strs("pr") {
				if _, err := c.S.AddPR(task, url, "", "open"); err != nil {
					return err
				}
			}
			for _, title := range c.Strs("step") {
				if _, err := c.S.CreateSubtask(task, title, nil, "", ""); err != nil {
					return err
				}
			}
			for _, ref := range c.Strs("spec") {
				p, err := c.linkSpec(ref, task, "implements", "self")
				if err != nil {
					return err
				}
				pins = append(pins, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return []int64{task.Int("id")}, nil
	})
	if err != nil {
		return err
	}
	c.applyPins(pins)
	return show(ids)
}

func taskLs(c *Ctx) error {
	var project *row.Row
	if c.Has("project") {
		var err error
		if project, err = c.resolveProject(c.Str("project"), true); err != nil {
			return err
		}
	}
	statuses := refs.OpenStatuses
	if c.Has("status") {
		var err error
		if statuses, err = refs.ParseStatusFilter(c.Str("status")); err != nil {
			return err
		}
	}
	since := ""
	if c.Has("since") {
		var err error
		if since, err = refs.ParseSince(c.Str("since")); err != nil {
			return err
		}
	}
	rows, total, err := c.S.ListTasks(store.TaskFilter{
		Context: c.Context, Project: project, Direct: c.Bool("direct"), Statuses: statuses,
		Tags: c.Tags("tag"), TagsAll: c.Bool("tags_all"), Grep: c.Str("grep"), Since: since,
		Repo: c.Str("repo"), Limit: c.Int("limit"), Order: c.Str("order"), AllContexts: c.Bool("all_contexts"),
		Unclaimed: c.Bool("unclaimed"), Ready: c.Bool("ready"), Blocked: c.Bool("blocked"),
	})
	if err != nil {
		return err
	}
	return c.emit(rows, "task", total)
}

func taskShow(c *Ctx) error {
	task, err := c.S.GetTask(c.Str("ref"), true)
	if err != nil {
		return err
	}
	detail, err := c.S.TaskDetail(task, c.Extended)
	if err != nil {
		return err
	}
	var subLines, prLines, history []string
	for _, s := range detail.Get("subtask_list").([]*row.Row) {
		subLines = append(subLines, fmt.Sprintf("%-8s %s %s", s.Str("ref"), s.Str("mark"), s.Str("title")))
	}
	for _, p := range detail.Get("pr_list").([]*row.Row) {
		prLines = append(prLines, fmt.Sprintf("[%s] %s", p.Str("state"), p.Str("url")))
	}
	sections := []render.Section{
		{Label: "task", Value: fmt.Sprintf("#%d %s %s", task.Int("id"), refs.StatusMark[task.Str("status")], task.Str("title"))},
		{Label: "status", Value: task.Str("status")},
		{Label: "project", Value: detail.Str("path")},
		{Label: "tags", Value: strings.Join(detail.Get("tag_list").([]string), ", ")},
		{Label: "created", Value: detail.Str("created")},
		{Label: "started", Value: detail.Str("started")},
		{Label: "closed", Value: detail.Str("closed")},
	}
	if claim := claimLine(task); claim != "" {
		sections = append(sections, render.Section{Label: "claimed", Value: claim})
	}
	if deps, _ := c.S.DepsOf(task.Int("id")); len(deps) > 0 {
		var lines []string
		for _, d := range deps {
			lines = append(lines, fmt.Sprintf("%s #%d %s %s", d.Str("kind"), d.Int("depends_on_id"), refs.StatusMark[d.Str("status")], d.Str("title")))
		}
		sections = append(sections, render.Section{Label: "depends on", Value: lines})
	}
	sections = append(sections,
		render.Section{Label: "subtasks", Value: subLines},
		render.Section{Label: "pull requests", Value: prLines},
		render.Section{Label: "notes", Value: noteLinesKind(detail.Get("note_list").([]*row.Row))},
	)
	if arts, _ := c.S.Artifacts(task.Int("id")); len(arts) > 0 {
		var lines []string
		for _, a := range arts {
			line := a.Str("kind") + " " + a.Str("value")
			if a.Str("label") != "" {
				line += "  (" + a.Str("label") + ")"
			}
			lines = append(lines, line)
		}
		sections = append(sections, render.Section{Label: "artifacts", Value: lines})
	}
	if c.Extended {
		for _, e := range detail.Get("events").([]*row.Row) {
			line := strings.TrimRight(fmt.Sprintf("%s  %-6s %s %s", e.Str("when"), e.Str("target"), e.Str("action"), e.Str("detail")), " ")
			if e.Get("backdated") == true {
				line += "  (recorded " + e.Str("recorded") + ")"
			}
			history = append(history, line)
		}
		sections = append(sections, render.Section{Label: "history", Value: history})
	}
	return c.detail(detail, "task", sections)
}

func noteLinesKind(notes []*row.Row) []string {
	var out []string
	for _, n := range notes {
		prefix := ""
		if k := n.Str("kind"); k != "" && k != "note" {
			prefix = "[" + k + "] "
		}
		if !n.IsNull("resolved_at") && n.Has("resolved_at") {
			prefix += "(resolved) "
		}
		out = append(out, n.Str("created")+" "+prefix+n.Str("body"))
	}
	return out
}

func claimLine(task *row.Row) string {
	if task.IsNull("claimed_by") {
		return ""
	}
	line := task.Str("claimed_by")
	if exp := task.Str("claim_expires_at"); exp != "" {
		if exp <= refs.NowISO() {
			line += " (expired " + refs.FmtTS(exp, false, false) + ")"
		} else {
			line += " until " + refs.FmtTS(exp, false, false)
		}
	}
	return line
}

// taskStatus applies a status to each ref. D10: when the status is already
// set, the whole invocation is a no-op for that task — no note, no tag event.
func taskStatus(c *Ctx, status string) error {
	var err error
	if status == "" {
		if status, err = c.statusArg(c.Str("status"), "task"); err != nil {
			return err
		}
	}
	at, err := c.When("at")
	if err != nil {
		return err
	}
	// Resolve every ref before writing anything, then apply them in one
	// transaction: a bad ref fails the whole command with nothing written.
	var tasks []*row.Row
	for _, ref := range c.Strs("ref") {
		task, err := c.S.GetTask(ref, true)
		if err != nil {
			return err
		}
		tasks = append(tasks, task)
	}
	type outcome struct {
		task    *row.Row
		changed bool
	}
	var done []outcome
	err = c.S.Tx(func() error {
		for _, task := range tasks {
			task, changed, err := c.S.SetTaskStatus(task.Int("id"), status, at)
			if err != nil {
				return err
			}
			if tags := c.Tags("tag"); len(tags) > 0 {
				if _, err := c.S.AttachTags("task", task.Int("id"), tags, task.Int("id"), task.Int("project_id"), !changed); err != nil {
					return err
				}
			}
			if c.Has("note") && changed {
				if _, err := c.S.AddNote(store.NewNote{TargetType: "task", TargetID: task.Int("id"), Body: c.Str("note"),
					TaskID: task.Int("id"), ProjectID: task.Int("project_id"), At: at}); err != nil {
					return err
				}
			}
			done = append(done, outcome{task, changed})
		}
		return nil
	})
	if err != nil {
		return err
	}
	var results []*row.Row
	for _, o := range done {
		if c.structured() {
			obj, _ := c.S.TaskDict(o.task)
			obj.Set("changed", o.changed)
			results = append(results, c.lean(obj, "task").(*row.Row))
		} else if !c.Quiet {
			c.printf("#%d %s %s\n", o.task.Int("id"), refs.StatusMark[status], o.task.Str("title"))
		}
	}
	if c.structured() && !c.Quiet {
		return c.emitObject(nonNil(results))
	}
	return nil
}

func taskSet(c *Ctx) error {
	task, err := c.S.GetTask(c.Str("ref"), true)
	if err != nil {
		return err
	}
	var fields []store.Field
	if c.Has("title") {
		fields = append(fields, store.Field{Name: "title", Value: c.Str("title")})
	}
	if c.Has("move") {
		p, err := c.resolveProject(c.Str("move"), true)
		if err != nil {
			return err
		}
		fields = append(fields, store.Field{Name: "project_id", Value: p.Int("id")})
	}
	if task, err = c.S.UpdateTask(task, fields); err != nil {
		return err
	}
	obj, _ := c.S.TaskDict(task)
	return c.mutated(fmt.Sprintf("#%d %s", task.Int("id"), task.Str("title")), c.lean(obj, "task"))
}

func taskRm(c *Ctx) error {
	var tasks []*row.Row
	for _, ref := range c.Strs("ref") {
		task, err := c.S.GetTask(ref, true)
		if err != nil {
			return err
		}
		counts, err := c.S.TaskChildren(task.Int("id"))
		if err != nil {
			return err
		}
		if err := guardChildren(c.Bool("force"), counts, fmt.Sprintf("task #%d", task.Int("id"))); err != nil {
			return err
		}
		tasks = append(tasks, task)
	}
	err := c.S.Tx(func() error {
		for _, task := range tasks {
			if err := c.S.DeleteTask(task); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	var deleted []int64
	for _, task := range tasks {
		deleted = append(deleted, task.Int("id"))
		if !c.structured() && !c.Quiet {
			c.printf("deleted task #%d\n", task.Int("id"))
		}
	}
	if c.structured() && !c.Quiet {
		return c.emitObject(row.Of("deleted", "task", "ids", deleted))
	}
	return nil
}

// --- subtasks ---------------------------------------------------------------------------

func subtaskAdd(c *Ctx) error {
	show := func(ids []int64) error {
		var objs []*row.Row
		for _, id := range ids {
			sub, err := c.S.GetSubtask(fmt.Sprint(id), true)
			if err != nil {
				return err
			}
			if c.structured() && !c.Quiet {
				d, _ := c.S.SubtaskDetail(sub)
				objs = append(objs, c.lean(d, "subtask").(*row.Row))
				continue
			}
			ref := fmt.Sprintf("%d.%d", sub.Int("task_id"), sub.Int("position"))
			if err := c.created("subtask", ref, nil, "subtask"); err != nil {
				return err
			}
		}
		if c.structured() && !c.Quiet {
			return c.emitObject(nonNil(objs))
		}
		return nil
	}
	if done, err := c.replay(show); done {
		return err
	}
	task, err := c.S.GetTask(c.Str("task"), true)
	if err != nil {
		return err
	}
	at, err := c.When("at")
	if err != nil {
		return err
	}
	titles := c.Strs("title")
	ids, err := c.record("subtask", func() ([]int64, error) {
		var ids []int64
		err := c.S.Tx(func() error {
			for _, title := range titles {
				sub, err := c.S.CreateSubtask(task, title, c.Tags("tag"), c.Str("note"), at)
				if err != nil {
					return err
				}
				ids = append(ids, sub.Int("id"))
			}
			return nil
		})
		return ids, err
	})
	if err != nil {
		return err
	}
	return show(ids)
}

func subtaskLs(c *Ctx) error {
	task, err := c.S.GetTask(c.Str("task"), true)
	if err != nil {
		return err
	}
	var statuses []string
	if c.Has("status") {
		if statuses, err = refs.ParseStatusFilter(c.Str("status")); err != nil {
			return err
		}
	}
	rows, total, err := c.S.ListSubtasks(task.Int("id"), statuses, c.Int("limit"))
	if err != nil {
		return err
	}
	return c.emit(rows, "subtask", total)
}

func subtaskStatus(c *Ctx, status string) error {
	var err error
	if status == "" {
		if status, err = c.statusArg(c.Str("status"), "sub"); err != nil {
			return err
		}
	}
	at, err := c.When("at")
	if err != nil {
		return err
	}
	var subs []*row.Row
	for _, ref := range c.Strs("ref") {
		sub, err := c.S.GetSubtask(ref, true)
		if err != nil {
			return err
		}
		subs = append(subs, sub)
	}
	var done []store.SubtaskStatusResult
	err = c.S.Tx(func() error {
		for _, sub := range subs {
			res, err := c.S.SetSubtaskStatus(sub.Int("id"), status, !c.Bool("no_cascade"), at)
			if err != nil {
				return err
			}
			task, err := c.S.GetTaskID(res.Subtask.Int("task_id"))
			if err != nil {
				return err
			}
			if tags := c.Tags("tag"); len(tags) > 0 {
				if _, err := c.S.AttachTags("subtask", sub.Int("id"), tags, task.Int("id"), task.Int("project_id"), !res.Changed); err != nil {
					return err
				}
			}
			if c.Has("note") && res.Changed {
				if _, err := c.S.AddNote(store.NewNote{TargetType: "subtask", TargetID: sub.Int("id"), Body: c.Str("note"),
					TaskID: task.Int("id"), ProjectID: task.Int("project_id"), At: at}); err != nil {
					return err
				}
			}
			done = append(done, res)
		}
		return nil
	})
	if err != nil {
		return err
	}
	var results []*row.Row
	for _, res := range done {
		s := res.Subtask
		if c.structured() {
			d, _ := c.S.SubtaskDetail(s)
			d.Set("changed", res.Changed)
			d.Set("task_moved", res.TaskMoved)
			results = append(results, c.lean(d, "subtask").(*row.Row))
		} else if !c.Quiet {
			c.printf("%d.%d %s %s\n", s.Int("task_id"), s.Int("position"), refs.StatusMark[status], s.Str("title"))
		}
	}
	if c.structured() && !c.Quiet {
		return c.emitObject(nonNil(results))
	}
	return nil
}

func subtaskShow(c *Ctx) error {
	sub, err := c.S.GetSubtask(c.Str("ref"), true)
	if err != nil {
		return err
	}
	d, err := c.S.SubtaskDetail(sub)
	if err != nil {
		return err
	}
	return c.detail(d, "subtask", []render.Section{
		{Label: "subtask", Value: fmt.Sprintf("%s %s %s", d.Str("ref"), d.Str("mark"), sub.Str("title"))},
		{Label: "status", Value: sub.Str("status")},
		{Label: "task", Value: fmt.Sprintf("#%d", sub.Int("task_id"))},
		{Label: "tags", Value: strings.Join(d.Get("tag_list").([]string), ", ")},
		{Label: "created", Value: d.Str("created")},
		{Label: "notes", Value: noteLinesKind(d.Get("note_list").([]*row.Row))},
	})
}

func subtaskSet(c *Ctx) error {
	sub, err := c.S.GetSubtask(c.Str("ref"), true)
	if err != nil {
		return err
	}
	var fields []store.Field
	if c.Has("title") {
		fields = append(fields, store.Field{Name: "title", Value: c.Str("title")})
	}
	if sub, err = c.S.UpdateSubtask(sub, fields); err != nil {
		return err
	}
	d, _ := c.S.SubtaskDetail(sub)
	return c.mutated(fmt.Sprintf("%d.%d %s", sub.Int("task_id"), sub.Int("position"), sub.Str("title")), c.lean(d, "subtask"))
}

func subtaskRm(c *Ctx) error {
	var subs []*row.Row
	var labels []string
	for _, ref := range c.Strs("ref") {
		sub, err := c.S.GetSubtask(ref, true)
		if err != nil {
			return err
		}
		label := fmt.Sprintf("%d.%d", sub.Int("task_id"), sub.Int("position"))
		counts, err := c.S.SubtaskChildren(sub.Int("id"))
		if err != nil {
			return err
		}
		if err := guardChildren(c.Bool("force"), counts, "subtask "+label); err != nil {
			return err
		}
		subs = append(subs, sub)
		labels = append(labels, label)
	}
	err := c.S.Tx(func() error {
		for _, sub := range subs {
			if err := c.S.DeleteSubtask(sub); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if c.structured() && !c.Quiet {
		return c.emitObject(row.Of("deleted", "subtask", "refs", labels))
	}
	if !c.Quiet {
		for _, l := range labels {
			c.println("deleted subtask " + l)
		}
	}
	return nil
}

// --- tags ---------------------------------------------------------------------------

func tagLs(c *Ctx) error {
	rows, err := c.S.ListTags()
	if err != nil {
		return err
	}
	return c.emit(nonNil(rows), "tag", len(rows))
}

func tagShow(c *Ctx) error {
	tag, err := c.S.GetTag(c.Str("ref"), true)
	if err != nil {
		return err
	}
	tasks, _, err := c.S.ListTasks(store.TaskFilter{Tags: []string{tag.Str("name")}, AllContexts: true, Order: "id"})
	if err != nil {
		return err
	}
	all, err := c.S.ListProjects(store.ProjectFilter{AllContexts: true, IncludeArchived: true})
	if err != nil {
		return err
	}
	var projects []*row.Row
	for _, p := range all {
		for _, t := range strings.Split(p.Str("tags"), ",") {
			if t == tag.Str("name") {
				projects = append(projects, p)
				break
			}
		}
	}
	subtasks, _ := c.S.SubtasksTagged(tag.Int("id"))
	notes, _ := c.S.NotesFor("tag", tag.Int("id"), store.NoteFilter{})
	obj := tag.Copy()
	obj.Set("tasks", nonNil(tasks))
	obj.Set("subtasks", nonNil(subtasks))
	obj.Set("projects", nonNil(projects))
	obj.Set("note_list", notes)
	var plines, tlines, slines []string
	for _, p := range projects {
		plines = append(plines, p.Str("path"))
	}
	for _, t := range tasks {
		tlines = append(tlines, fmt.Sprintf("#%-5d %s %s %s", t.Int("id"), refs.StatusMark[t.Str("status")], t.Str("project"), t.Str("title")))
	}
	for _, s := range subtasks {
		slines = append(slines, fmt.Sprintf("%-8s %s %s", s.Str("ref"), s.Str("mark"), s.Str("title")))
	}
	return c.detail(obj, "tag", []render.Section{
		{Label: "tag", Value: tag.Str("name")},
		{Label: "projects", Value: plines},
		{Label: "tasks", Value: tlines},
		{Label: "subtasks", Value: slines},
		{Label: "notes", Value: noteLines(notes)},
	})
}

func tagRm(c *Ctx) error {
	tag, err := c.S.DeleteTag(c.Str("ref"))
	if err != nil {
		return err
	}
	return c.mutated("deleted tag "+tag.Str("name"), row.Of("deleted", "tag", "name", tag.Str("name")))
}

// --- pull requests ---------------------------------------------------------------------

func prAdd(c *Ctx) error {
	show := func(ids []int64) error {
		pr, err := c.S.GetPR(ids[0])
		if err != nil {
			return err
		}
		return c.created("pr", ids[0], pr, "pr")
	}
	if done, err := c.replay(show); done {
		return err
	}
	task, err := c.S.GetTask(c.Str("task"), true)
	if err != nil {
		return err
	}
	state := "open"
	if c.Has("state") {
		if state, err = refs.NormalizePRState(c.Str("state")); err != nil {
			return err
		}
	}
	ids, err := c.record("pr", func() ([]int64, error) {
		pr, err := c.S.AddPR(task, c.Str("url"), c.Str("title"), state)
		if err != nil {
			return nil, err
		}
		return []int64{pr.Int("id")}, nil
	})
	if err != nil {
		return err
	}
	return show(ids)
}

func prLs(c *Ctx) error {
	var rows []*row.Row
	if c.Has("task") {
		task, err := c.S.GetTask(c.Str("task"), true)
		if err != nil {
			return err
		}
		if rows, err = c.S.PRsFor(task.Int("id")); err != nil {
			return err
		}
		for _, r := range rows {
			r.Set("task_title", task.Str("title"))
		}
	} else {
		state := ""
		if c.Has("state") {
			var err error
			if state, err = refs.NormalizePRState(c.Str("state")); err != nil {
				return err
			}
		}
		f := store.PRFilter{URL: c.Str("url"), Repo: c.Str("repo"), State: state, Context: c.Context, AllContexts: c.Bool("all_contexts")}
		if _, ok := c.V["number"]; ok && c.V["number"] != nil {
			f.Number, f.HasNumber = int64(c.Int("number")), true
		}
		var err error
		if rows, err = c.S.FindPRs(f); err != nil {
			return err
		}
	}
	return c.emit(nonNil(rows), "pr", len(rows))
}

func prState(c *Ctx) error {
	id, err := parseID(c.Str("id"))
	if err != nil {
		return err
	}
	state, err := refs.NormalizePRState(c.Str("new_state"))
	if err != nil {
		return err
	}
	pr, err := c.S.SetPRState(id, state)
	if err != nil {
		return err
	}
	return c.mutated(fmt.Sprintf("pr %d %s", pr.Int("id"), pr.Str("state")), pr)
}

func prRm(c *Ctx) error {
	id, err := parseID(c.Str("id"))
	if err != nil {
		return err
	}
	if err := c.S.RemovePR(id); err != nil {
		return err
	}
	return c.mutated("deleted pr record "+c.Str("id"), row.Of("deleted", "pr", "id", id))
}

// parseID mirrors Python int() on a record id.
func parseID(s string) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, errs.New("invalid literal for int() with base 10: '%s'", s)
	}
	return n, nil
}
