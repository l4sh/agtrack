package cli

import (
	"fmt"
	"strings"

	"github.com/l4sh/agtrack/internal/db"
	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/render"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

// Version is set at build time with -ldflags "-X .../cli.Version=...".
var Version = "2.0.0-dev"

// --- worklog --------------------------------------------------------------------------

func logAdd(c *Ctx) error {
	show := func(ids []int64) error {
		return c.mutated("logged", row.Of("logged", true, "id", ids[0]))
	}
	if done, err := c.replay(show); done {
		return err
	}
	var task, project *row.Row
	var err error
	if c.Has("task") {
		if task, err = c.S.GetTask(c.Str("task"), true); err != nil {
			return err
		}
	}
	if c.Has("project") {
		if project, err = c.resolveProject(c.Str("project"), true); err != nil {
			return err
		}
	}
	at, err := c.When("at")
	if err != nil {
		return err
	}
	body := c.Str("body")
	ids, err := c.record("event", func() ([]int64, error) {
		id, err := c.S.AddLog(body, task, project, at)
		return []int64{id}, err
	})
	if err != nil {
		return err
	}
	return show(ids)
}

func logLs(c *Ctx) error {
	var task, project *row.Row
	var err error
	if c.Has("task") {
		if task, err = c.S.GetTask(c.Str("task"), true); err != nil {
			return err
		}
	}
	if c.Has("project") {
		if project, err = c.resolveProject(c.Str("project"), true); err != nil {
			return err
		}
	}
	f := store.EventFilter{Task: task, Project: project, Direct: c.Bool("direct"), Context: c.Context,
		Actor: c.Str("actor_filter"), Limit: c.Int("limit"), AllContexts: c.Bool("all_contexts"),
		Ascending: true, BackdatedOnly: c.Bool("backdated")}
	if c.Has("since") {
		if f.Since, err = refs.ParseSince(c.Str("since")); err != nil {
			return err
		}
	}
	if c.Has("until") {
		if f.Until, err = refs.ParseSince(c.Str("until")); err != nil {
			return err
		}
	}
	if c.Has("action") {
		for _, a := range strings.Split(c.Str("action"), ",") {
			if a != "" {
				f.Actions = append(f.Actions, a)
			}
		}
	}
	rows, total, err := c.S.ListEvents(f)
	if err != nil {
		return err
	}
	if c.Bool("by_day") && !c.structured() && !c.Bool("count") && !c.Bool("ids") && !c.Quiet {
		day := ""
		for _, r := range rows {
			if r.Str("date") != day {
				day = r.Str("date")
				if day != rows[0].Str("date") {
					c.println("\n" + day)
				} else {
					c.println(day)
				}
			}
			detail, actor, late := "", "", ""
			if r.Str("detail") != "" {
				detail = " " + r.Str("detail")
			}
			if r.Str("actor") != "" {
				actor = " " + r.Str("actor")
			}
			if r.Get("backdated") == true {
				late = "  (recorded " + r.Str("recorded") + ")"
			}
			when := r.Str("when")
			if len(when) > 11 {
				when = when[11:]
			} else {
				when = ""
			}
			c.printf("  %s%s %s %s%s%s\n", when, actor, r.Str("target"), r.Str("action"), detail, late)
		}
		return nil
	}
	return c.emit(rows, "event", total)
}

// --- overviews ---------------------------------------------------------------------------

func cmdNext(c *Ctx) error {
	var project *row.Row
	var err error
	if c.Has("project") {
		if project, err = c.resolveProject(c.Str("project"), true); err != nil {
			return err
		}
	}
	statuses := []string{"in-progress"}
	if c.Bool("todo") {
		statuses = append(statuses, "todo")
	}
	f := store.TaskFilter{Context: c.Context, Project: project, Statuses: statuses,
		AllContexts: c.Bool("all_contexts"), Order: "status", Limit: c.Int("limit"), Ready: !c.Bool("blocked")}
	f.ExcludeSnoozed = !c.Bool("parked")
	f.OnlySnoozed = c.Bool("parked")
	if c.Bool("claim") {
		return nextClaim(c, f)
	}
	limit := f.Limit
	f.Limit = 0
	tasks, _, err := c.S.ListTasks(f)
	if err != nil {
		return err
	}
	days, err := c.staleDays()
	if err != nil {
		return err
	}
	if tasks, err = c.staleRows(tasks, days); err != nil {
		return err
	}
	var fresh, stale []*row.Row
	for _, t := range tasks {
		if t.Str("stale_reason") != "" {
			stale = append(stale, t)
		} else {
			fresh = append(fresh, t)
		}
	}
	if c.Bool("stale") || c.Has("stale_after") {
		fresh = nil
	}
	tasks = append(fresh, stale...)
	if limit > 0 && len(tasks) > limit {
		tasks = tasks[:limit]
	}
	payload := []*row.Row{}
	for _, t := range tasks {
		subs, _, err := c.S.ListSubtasks(t.Int("id"), refs.OpenStatuses, 0)
		if err != nil {
			return err
		}
		item := t.Copy()
		if c.structured() {
			item = c.lean(item, "task").(*row.Row)
			leaned := make([]*row.Row, len(subs))
			for i, s := range subs {
				leaned[i] = c.lean(s, "subtask").(*row.Row)
			}
			subs = leaned
		}
		item.Set("open_subtasks", nonNil(subs))
		payload = append(payload, item)
	}
	if c.structured() {
		if len(c.Fields) > 0 {
			fields, err := render.SelectFields(payload, c.Fields)
			if err != nil {
				return err
			}
			return c.emitObject(render.ProjectRows(payload, fields))
		}
		return c.emitObject(payload)
	}
	for _, item := range payload {
		line := fmt.Sprintf("#%d %s %s  %s", item.Int("id"), item.Str("mark"), item.Str("project"), item.Str("title"))
		if r := item.Str("stale_reason"); r != "" {
			line = fmt.Sprintf("#%d %s ! %s  %s  (%s)", item.Int("id"), item.Str("mark"), item.Str("project"), item.Str("title"), r)
		}
		if p := parkedNote(item); p != "" {
			line += "  (" + p + ")"
		}
		if item.Str("tags") != "" {
			line += "  [" + item.Str("tags") + "]"
		}
		if claim := claimLine(item); claim != "" {
			line += "  (claimed by " + claim + ")"
		}
		c.println(line)
		for _, s := range item.Get("open_subtasks").([]*row.Row) {
			c.printf("    %-8s %s %s\n", s.Str("ref"), s.Str("mark"), s.Str("title"))
		}
	}
	return nil
}

func cmdSummary(c *Ctx) error {
	data, err := c.S.Summary(c.Context, c.Bool("all_contexts"))
	if err != nil {
		return err
	}
	if c.structured() {
		return c.emitObject(c.lean(data, ""))
	}
	for _, ctx := range data.Get("contexts").([]*row.Row) {
		counts := ctx.Get("counts").(*row.Row)
		c.printf("%s: %d todo, %d in-progress, %d done, %d cancelled\n", ctx.Str("name"),
			counts.Int("todo"), counts.Int("in-progress"), counts.Int("done"), counts.Int("cancelled"))
		for _, p := range ctx.Get("projects").([]*row.Row) {
			if p.Int("tasks") > 0 || p.IsNull("parent_id") {
				c.printf("  %-28s %3d open / %3d\n", p.Str("ref"), p.Int("open"), p.Int("tasks"))
			}
		}
		for _, t := range ctx.Get("in_progress").([]*row.Row) {
			c.printf("  -> #%d %s %s\n", t.Int("id"), t.Str("project"), t.Str("title"))
		}
	}
	return nil
}

func cmdHere(c *Ctx) error {
	remote := c.S.CwdRepo()
	if remote == "" {
		return errs.New("not inside a git repository with an origin remote")
	}
	matches, err := c.S.FindProjectsByRepo(remote)
	if err != nil {
		return err
	}
	id, err := refs.RepoIdentity(remote)
	if err != nil {
		return err
	}
	if c.structured() {
		projects := []*row.Row{}
		for _, p := range matches {
			r := p.Copy()
			path, _ := c.S.ProjectPath(p)
			short, _ := c.S.ShortPath(p)
			r.Set("path", path)
			r.Set("ref", short)
			projects = append(projects, r)
		}
		return c.emitObject(c.lean(row.Of("repo", id, "projects", projects), ""))
	}
	c.println(id)
	for _, p := range matches {
		path, _ := c.S.ProjectPath(p)
		c.println("  " + path)
	}
	if len(matches) == 0 {
		c.println("  (no project linked)")
	}
	return nil
}

// cmdDump is a human-readable export of a whole context (yaml by default).
// It is not a restoration format (D7): see README.
func cmdDump(c *Ctx) error {
	var contexts []*row.Row
	if c.Bool("all_contexts") {
		var err error
		if contexts, err = c.S.ListContexts(true); err != nil {
			return err
		}
	} else {
		ctx := c.Context
		var r *row.Row
		var err error
		if ctx != "" {
			r, err = c.S.GetContext(ctx, true)
		} else {
			r, err = c.S.DefaultContext()
		}
		if err != nil {
			return err
		}
		contexts = []*row.Row{r}
	}
	out := []*row.Row{}
	for _, ctx := range contexts {
		projects := []*row.Row{}
		plist, err := c.S.ListProjects(store.ProjectFilter{Context: ctx.Str("name"), IncludeArchived: true})
		if err != nil {
			return err
		}
		for _, p := range plist {
			tasks := []*row.Row{}
			tl, _, err := c.S.ListTasks(store.TaskFilter{Project: p, Direct: true, Order: "id"})
			if err != nil {
				return err
			}
			for _, t := range tl {
				full, _ := c.S.GetTaskID(t.Int("id"))
				d, _ := c.S.TaskDetail(full, false)
				tasks = append(tasks, d)
			}
			repos, _ := c.S.ProjectRepos(p.Int("id"))
			tags, _ := c.S.TagsFor("project", p.Int("id"))
			notes, _ := c.S.NotesFor("project", p.Int("id"), store.NoteFilter{})
			projects = append(projects, row.Of("ref", p.Str("ref"), "title", p.Get("title"), "repos", repos,
				"tags", tags, "notes", notes, "tasks", tasks))
		}
		notes, _ := c.S.NotesFor("context", ctx.Int("id"), store.NoteFilter{})
		out = append(out, row.Of("context", ctx.Str("name"), "title", ctx.Get("title"), "notes", notes, "projects", projects))
	}
	f := c.Format
	if !c.structured() {
		f = "yaml"
	}
	return render.EmitObject(c.Out, out, f)
}

func cmdInfo(c *Ctx) error {
	ctx, _ := c.S.CurrentContext()
	project, _ := c.S.CurrentProject()
	counts, err := c.S.TableCounts()
	if err != nil {
		return err
	}
	schema, _ := c.S.DB.UserVersion()
	var ctxName, projName any
	if ctx != nil {
		ctxName = ctx.Str("name")
	}
	if project != nil {
		projName, _ = c.S.ShortPath(project)
	}
	obj := row.Of("version", Version, "database", db.ResolvePath(c.DBPath), "schema", int64(schema),
		"schema_expected", int64(db.SchemaVersion), "current_context", ctxName, "current_project", projName,
		"counts", counts, "sqlite", c.S.DB.SQLiteVersion(), "actor", nilIfEmpty(c.Actor))
	// Only when one is detected, so output is the same outside a harness.
	session := ""
	if c.Session != "" {
		session = c.Session + " (from " + c.SessionSource + ")"
		obj.Set("session", c.Session).Set("session_source", c.SessionSource)
	}
	if c.structured() {
		return c.emitObject(obj)
	}
	return render.EmitDetail(c.Out, []render.Section{
		{Label: "agtrack", Value: Version},
		{Label: "database", Value: obj.Str("database")},
		{Label: "schema", Value: fmt.Sprintf("v%d (migrates automatically; this build expects v%d)", schema, db.SchemaVersion)},
		{Label: "sqlite", Value: obj.Str("sqlite")},
		{Label: "actor", Value: c.Actor},
		{Label: "session", Value: session},
		{Label: "context", Value: ctxName},
		{Label: "project", Value: projName},
		{Label: "counts", Value: kvLine(counts)},
	}, c.Color)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
