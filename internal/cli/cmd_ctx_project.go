package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/render"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

// --- contexts ---------------------------------------------------------------------

func contextAdd(c *Ctx) error {
	show := func(ids []int64) error {
		ctx, err := c.S.GetContextID(ids[0])
		if err != nil {
			return err
		}
		return c.created("context", ctx.Str("name"), ctx, "context")
	}
	if done, err := c.replay(show); done {
		return err
	}
	ids, err := c.record("context", func() ([]int64, error) {
		var id int64
		err := c.S.Tx(func() error {
			ctx, err := c.S.CreateContext(c.Str("name"), c.Str("title"))
			if err != nil {
				return err
			}
			id = ctx.Int("id")
			if c.Has("note") {
				if _, err := c.S.AddNote(store.NewNote{TargetType: "context", TargetID: id, Body: c.Str("note")}); err != nil {
					return err
				}
			}
			if c.Bool("use") {
				if _, err := c.S.UseContext(ctx.Str("name")); err != nil {
					return err
				}
			}
			return nil
		})
		return []int64{id}, err
	})
	if err != nil {
		return err
	}
	return show(ids)
}

func contextLs(c *Ctx) error {
	rows, err := c.S.ListContexts(c.Bool("archived"))
	if err != nil {
		return err
	}
	return c.emit(rows, "context", len(rows))
}

func contextUse(c *Ctx) error {
	ctx, err := c.S.UseContext(c.Str("name"))
	if err != nil {
		return err
	}
	return c.mutated("context "+ctx.Str("name"), row.Of("context", ctx.Str("name")))
}

func contextCurrent(c *Ctx) error {
	current, err := c.S.CurrentContext()
	if err != nil {
		return err
	}
	if current == nil {
		return errs.New("no current context")
	}
	// D1: CurrentProject only returns a pin that lies in this context.
	project, err := c.S.CurrentProject()
	if err != nil {
		return err
	}
	short := ""
	if project != nil {
		short, _ = c.S.ShortPath(project)
	}
	if c.structured() {
		var p any
		if short != "" {
			p = short
		}
		return c.emitObject(row.Of("context", current.Str("name"), "project", p))
	}
	if short != "" {
		c.println(current.Str("name") + " " + short)
	} else {
		c.println(current.Str("name"))
	}
	return nil
}

func contextShow(c *Ctx) error {
	ctx, err := c.S.GetContext(c.Str("name"), true)
	if err != nil {
		return err
	}
	projects, err := c.S.ListProjects(store.ProjectFilter{Context: ctx.Str("name"), IncludeArchived: true})
	if err != nil {
		return err
	}
	notes, err := c.S.NotesFor("context", ctx.Int("id"), store.NoteFilter{})
	if err != nil {
		return err
	}
	var ids []int64
	for _, p := range projects {
		ids = append(ids, p.Int("id"))
	}
	counts, err := c.S.TaskCounts(ids)
	if err != nil {
		return err
	}
	obj := ctx.Copy()
	obj.Set("projects", nonNil(projects))
	obj.Set("counts", counts)
	obj.Set("note_list", notes)
	var plines []string
	for _, p := range projects {
		plines = append(plines, fmt.Sprintf("%-24s %d open", p.Str("ref"), p.Int("open")))
	}
	return c.detail(obj, "context", []render.Section{
		{Label: "context", Value: ctx.Str("name")},
		{Label: "title", Value: ctx.Get("title")},
		{Label: "created", Value: refs.FmtTS(ctx.Str("created_at"), false, false)},
		{Label: "tasks", Value: kvLine(counts)},
		{Label: "projects", Value: plines},
		{Label: "notes", Value: noteLines(notes)},
	})
}

func kvLine(r *row.Row) string {
	var parts []string
	for _, k := range r.Keys() {
		parts = append(parts, fmt.Sprintf("%s=%s", k, render.Cell(r.Get(k))))
	}
	return strings.Join(parts, " ")
}

func noteLines(notes []*row.Row) []string {
	var out []string
	for _, n := range notes {
		out = append(out, n.Str("created")+" "+n.Str("body"))
	}
	return out
}

func nonNil(rows []*row.Row) []*row.Row {
	if rows == nil {
		return []*row.Row{}
	}
	return rows
}

func setFields(c *Ctx) ([]store.Field, error) {
	var fields []store.Field
	if _, ok := c.V["title"]; ok && c.V["title"] != nil {
		fields = append(fields, store.Field{Name: "title", Value: c.Str("title")})
	}
	if c.Has("rename") {
		slug, err := refs.Slugify(c.Str("rename"))
		if err != nil {
			return nil, err
		}
		fields = append(fields, store.Field{Name: "name", Value: slug})
	}
	if c.Bool("archive") {
		fields = append(fields, store.Field{Name: "archived", Value: int64(1)})
	}
	if c.Bool("unarchive") {
		fields = append(fields, store.Field{Name: "archived", Value: int64(0)})
	}
	return fields, nil
}

func contextSet(c *Ctx) error {
	fields, err := setFields(c)
	if err != nil {
		return err
	}
	ctx, err := c.S.UpdateContext(c.Str("name"), fields)
	if err != nil {
		return err
	}
	return c.mutated("context "+ctx.Str("name"), ctx)
}

func contextRm(c *Ctx) error {
	ctx, err := c.S.DeleteContext(c.Str("name"), c.Bool("force"))
	if err != nil {
		return err
	}
	return c.mutated("deleted context "+ctx.Str("name"), row.Of("deleted", "context", "name", ctx.Str("name")))
}

// --- projects ------------------------------------------------------------------------

func projectAdd(c *Ctx) error {
	show := func(ids []int64) error {
		project, err := c.S.GetProject(ids[0])
		if err != nil || project == nil {
			return errs.NotFound("project", fmt.Sprint(ids[0]), "no such project: %d", ids[0])
		}
		short, _ := c.S.ShortPath(project)
		return c.created("project", short, project, "project")
	}
	if done, err := c.replay(show); done {
		return err
	}
	var project *row.Row
	ids, err := c.record("project", func() ([]int64, error) {
		err := c.S.Tx(func() error {
			var err error
			project, err = c.S.CreateProject(c.Str("ref"), c.Context, c.Str("title"), c.Strs("repo"))
			if err != nil {
				return err
			}
			if tags := c.Tags("tag"); len(tags) > 0 {
				if _, err := c.S.AttachTags("project", project.Int("id"), tags, 0, project.Int("id"), false); err != nil {
					return err
				}
			}
			if c.Has("note") {
				if _, err := c.S.AddNote(store.NewNote{TargetType: "project", TargetID: project.Int("id"), Body: c.Str("note"), ProjectID: project.Int("id")}); err != nil {
					return err
				}
			}
			if c.Bool("use") {
				if _, err := c.S.UseProject(project); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return []int64{project.Int("id")}, nil
	})
	if err != nil {
		return err
	}
	return show(ids)
}

func projectLs(c *Ctx) error {
	var parent *row.Row
	if c.Has("parent") {
		var err error
		if parent, err = c.resolveProject(c.Str("parent"), true); err != nil {
			return err
		}
	}
	rows, err := c.S.ListProjects(store.ProjectFilter{
		Context: c.Context, Parent: parent, IncludeArchived: c.Bool("archived"), AllContexts: c.Bool("all_contexts"),
	})
	if err != nil {
		return err
	}
	if c.Bool("top") {
		var top []*row.Row
		for _, r := range rows {
			if r.IsNull("parent_id") {
				top = append(top, r)
			}
		}
		rows = top
	}
	return c.emit(rows, "project", len(rows))
}

func projectShow(c *Ctx) error {
	project, err := c.resolveProject(c.Str("ref"), true)
	if err != nil {
		return err
	}
	subs, err := c.S.ListProjects(store.ProjectFilter{Parent: project, AllContexts: true})
	if err != nil {
		return err
	}
	notes, _ := c.S.NotesFor("project", project.Int("id"), store.NoteFilter{})
	ids, _ := c.S.DescendantIDs(project.Int("id"))
	counts, _ := c.S.TaskCounts(ids)
	obj := project.Copy()
	path, _ := c.S.ProjectPath(project)
	repos, _ := c.S.ProjectRepos(project.Int("id"))
	tags, _ := c.S.TagsFor("project", project.Int("id"))
	obj.Set("path", path)
	obj.Set("repos", repos)
	obj.Set("tags", tags)
	obj.Set("subprojects", nonNil(subs))
	obj.Set("counts", counts)
	obj.Set("note_list", notes)
	var subRefs []string
	for _, p := range subs {
		subRefs = append(subRefs, p.Str("ref"))
	}
	return c.detail(obj, "project", []render.Section{
		{Label: "project", Value: path},
		{Label: "title", Value: project.Get("title")},
		{Label: "repos", Value: repos},
		{Label: "tags", Value: strings.Join(tags, ", ")},
		{Label: "tasks", Value: kvLine(counts)},
		{Label: "subprojects", Value: subRefs},
		{Label: "notes", Value: noteLines(notes)},
	})
}

func projectUse(c *Ctx) error {
	project, err := c.resolveProject(c.Str("ref"), true)
	if err != nil {
		return err
	}
	if project, err = c.S.UseProject(project); err != nil {
		return err
	}
	path, _ := c.S.ProjectPath(project)
	return c.mutated("project "+path, row.Of("project", path))
}

func projectSet(c *Ctx) error {
	project, err := c.resolveProject(c.Str("ref"), true)
	if err != nil {
		return err
	}
	fields, err := setFields(c)
	if err != nil {
		return err
	}
	err = c.S.Tx(func() error {
		if c.Has("move") {
			// The move takes the new name with it, so a rename into a
			// destination that already holds the old name does not collide.
			var rename string
			var rest []store.Field
			for _, f := range fields {
				if f.Name == "name" {
					rename = f.Value.(string)
				} else {
					rest = append(rest, f)
				}
			}
			if project, err = c.S.MoveProject(project, c.Str("move"), rename); err != nil {
				return err
			}
			fields = rest
		}
		if len(fields) > 0 || !c.Has("move") {
			project, err = c.S.UpdateProject(project, fields)
		}
		return err
	})
	if err != nil {
		return err
	}
	short, _ := c.S.ShortPath(project)
	return c.mutated("project "+short, project)
}

func projectMerge(c *Ctx) error {
	if c.Str("into") == "" {
		return errs.Usage("--into REF is required")
	}
	src, err := c.resolveProject(c.Str("ref"), true)
	if err != nil {
		return err
	}
	dst, err := c.resolveProject(c.Str("into"), true)
	if err != nil {
		return err
	}
	from, _ := c.S.ProjectPath(src)
	to, _ := c.S.ProjectPath(dst)
	counts, err := c.S.MergeProject(src, dst, !c.Bool("yes"))
	if err != nil {
		return err
	}
	moved := row.New()
	var parts []string
	for _, k := range store.MergeKeys {
		moved.Set(k, counts[k])
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
		}
	}
	summary := strings.Join(parts, ", ")
	if summary == "" {
		summary = "nothing attached"
	}
	obj := row.Of("merged", from, "into", to, "applied", c.Bool("yes"), "moved", moved)
	if !c.Bool("yes") {
		if c.structured() {
			return c.emitObject(obj)
		}
		fmt.Fprintf(c.Out, "would merge %s into %s: %s\n(pass --yes to do it; %s is deleted afterwards)\n", from, to, summary, from)
		return nil
	}
	return c.mutated(fmt.Sprintf("merged %s into %s: %s", from, to, summary), obj)
}

func projectRm(c *Ctx) error {
	project, err := c.resolveProject(c.Str("ref"), true)
	if err != nil {
		return err
	}
	path, _ := c.S.ProjectPath(project)
	if err := c.S.DeleteProject(project, c.Bool("force")); err != nil {
		return err
	}
	return c.mutated("deleted project "+path, row.Of("deleted", "project", "path", path))
}

func projectRepo(c *Ctx) error {
	project, err := c.resolveProject(c.Str("ref"), true)
	if err != nil {
		return err
	}
	short, _ := c.S.ShortPath(project)
	switch c.Str("repo_action") {
	case "add", "rm":
		var done []string
		err := c.S.Tx(func() error {
			for _, url := range c.Strs("url") {
				var slug string
				var err error
				if c.Str("repo_action") == "add" {
					slug, err = c.S.AddRepo(project, url)
				} else {
					slug, err = c.S.RemoveRepo(project, url)
				}
				if err != nil {
					return err
				}
				done = append(done, slug)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if c.structured() {
			if c.Quiet {
				return nil
			}
			return c.emitObject(row.Of("project", short, "action", c.Str("repo_action"), "repos", done))
		}
		if !c.Quiet {
			for _, slug := range done {
				if c.Str("repo_action") == "add" {
					c.printf("linked %s -> %s\n", slug, short)
				} else {
					c.printf("unlinked %s\n", slug)
				}
			}
		}
		return nil
	default:
		repos, err := c.S.ProjectRepos(project.Int("id"))
		if err != nil {
			return err
		}
		if c.structured() {
			var rows []*row.Row
			for _, s := range repos {
				rows = append(rows, row.Of("slug", s))
			}
			return c.emitObject(nonNil(rows))
		}
		for _, s := range repos {
			c.println(s)
		}
		return nil
	}
}

func projectFind(c *Ctx) error {
	remote := c.Str("repo")
	if remote == "" {
		remote = c.S.CwdRepo()
	}
	if remote == "" {
		return errs.New("pass a repo or run inside a git repository")
	}
	matches, err := c.S.FindProjectsByRepo(remote)
	if err != nil {
		return err
	}
	var rows []*row.Row
	for _, p := range matches {
		r := p.Copy()
		short, _ := c.S.ShortPath(p)
		path, _ := c.S.ProjectPath(p)
		repos, _ := c.S.ProjectRepos(p.Int("id"))
		ids, _ := c.S.DescendantIDs(p.Int("id"))
		counts, _ := c.S.TaskCounts(ids)
		tags, _ := c.S.TagsFor("project", p.Int("id"))
		r.Set("ref", short)
		r.Set("path", path)
		r.Set("repos", strings.Join(repos, ","))
		r.Set("open", counts.Int("todo")+counts.Int("in-progress"))
		var total int64
		for _, k := range counts.Keys() {
			total += counts.Int(k)
		}
		r.Set("tasks", total)
		r.Set("tags", strings.Join(tags, ","))
		rows = append(rows, r)
	}
	if len(rows) == 0 && !c.Quiet {
		id, _ := refs.RepoIdentity(remote)
		fmt.Fprintf(c.Err, "no project linked to %s\n", id)
	}
	if c.Bool("use") && len(rows) > 0 {
		if len(rows) > 1 {
			return errs.New("several projects share that remote; pick one")
		}
		if _, err := c.S.UseProject(matches[0]); err != nil {
			return err
		}
	}
	return c.emit(nonNil(rows), "project", len(rows))
}

// --- note / tag verbs shared by several nouns -------------------------------------------

type target struct {
	typ               string
	id, task, project int64
}

func (c *Ctx) target() (target, error) {
	ref := c.Str("ref")
	switch c.Noun {
	case "context":
		ctx, err := c.S.GetContext(ref, true)
		if err != nil {
			return target{}, err
		}
		return target{"context", ctx.Int("id"), 0, 0}, nil
	case "project":
		p, err := c.resolveProject(ref, true)
		if err != nil {
			return target{}, err
		}
		return target{"project", p.Int("id"), 0, p.Int("id")}, nil
	case "task":
		t, err := c.S.GetTask(ref, true)
		if err != nil {
			return target{}, err
		}
		return target{"task", t.Int("id"), t.Int("id"), t.Int("project_id")}, nil
	case "subtask":
		sub, err := c.S.GetSubtask(ref, true)
		if err != nil {
			return target{}, err
		}
		t, err := c.S.GetTaskID(sub.Int("task_id"))
		if err != nil {
			return target{}, err
		}
		return target{"subtask", sub.Int("id"), t.Int("id"), t.Int("project_id")}, nil
	case "tag":
		tag, err := c.S.GetTag(ref, true)
		if err != nil {
			return target{}, err
		}
		return target{"tag", tag.Int("id"), 0, 0}, nil
	}
	return target{}, errs.New("notes are not supported on %s", c.Noun)
}

func noteAdd(c *Ctx) error {
	show := func(ids []int64) error {
		n, err := c.S.GetNote(ids[0])
		if err != nil {
			return err
		}
		return c.created("note", ids[0], c.S.NoteRow(n), "note")
	}
	if done, err := c.replay(show); done {
		return err
	}
	t, err := c.target()
	if err != nil {
		return err
	}
	at, err := c.When("at")
	if err != nil {
		return err
	}
	kind := c.Str("kind")
	if kind != "" {
		if kind, err = normalizeNoteKind(kind); err != nil {
			return err
		}
	}
	if c.Has("close") {
		if kind == "" {
			kind = "handoff"
		}
		if kind != "handoff" {
			return errs.New("--close goes with --kind handoff")
		}
	}
	ids, err := c.record("note", func() ([]int64, error) {
		in := store.NewNote{
			TargetType: t.typ, TargetID: t.id, Body: c.Str("body"), TaskID: t.task, ProjectID: t.project, At: at, Kind: kind,
		}
		if c.Has("close") {
			note, _, err := c.S.CloseWithNote(in, c.Str("close"))
			if err != nil {
				return nil, err
			}
			return []int64{note.Int("id")}, nil
		}
		note, err := c.S.AddNote(in)
		if err != nil {
			return nil, err
		}
		return []int64{note.Int("id")}, nil
	})
	if err != nil {
		return err
	}
	return show(ids)
}

func noteLs(c *Ctx) error {
	t, err := c.target()
	if err != nil {
		return err
	}
	rows, err := c.S.NotesFor(t.typ, t.id, store.NoteFilter{OpenOnly: c.Bool("open"), Kind: c.Str("kind")})
	if err != nil {
		return err
	}
	if c.Bool("unacked") {
		rows = unackedOnly(rows)
	}
	return c.emit(rows, "note", len(rows))
}

func tagAttach(c *Ctx) error {
	t, err := c.target()
	if err != nil {
		return err
	}
	applied, err := c.S.AttachTags(t.typ, t.id, splitTags(c.Strs("tags")), t.task, t.project, false)
	if err != nil {
		return err
	}
	return c.mutated(strings.Join(applied, " "), row.Of("tags", applied))
}

func tagDetach(c *Ctx) error {
	t, err := c.target()
	if err != nil {
		return err
	}
	removed, err := c.S.DetachTags(t.typ, t.id, splitTags(c.Strs("tags")))
	if err != nil {
		return err
	}
	if removed == nil {
		removed = []string{}
	}
	return c.mutated(strings.Join(removed, " "), row.Of("removed", removed))
}

func splitTags(values []string) []string {
	var out []string
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			if strings.TrimSpace(part) != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

var _ = os.Stderr
