package store

import (
	"fmt"
	"os"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
)

var projectCols = []string{"id", "context_id", "parent_id", "name", "title", "archived", "created_at", "updated_at"}

func projectRow(r *row.Row) *row.Row { return pick(r, projectCols...) }

func (s *Store) getProject(id int64) *row.Row {
	r := s.one("SELECT * FROM projects WHERE id = ?", id)
	if r == nil {
		return nil
	}
	return projectRow(r)
}

// GetProject fetches a project row by id (nil if absent).
func (s *Store) GetProject(id int64) (r *row.Row, err error) {
	defer guard(&err)
	return s.getProject(id), nil
}

func (s *Store) projectPath(p *row.Row) string {
	ctxName := "?"
	if c := s.one("SELECT name FROM contexts WHERE id = ?", p.Int("context_id")); c != nil {
		ctxName = c.Str("name")
	}
	if !p.IsNull("parent_id") && p.Int("parent_id") != 0 {
		parent := s.one("SELECT name FROM projects WHERE id = ?", p.Int("parent_id"))
		return fmt.Sprintf("%s/%s:%s", ctxName, parent.Str("name"), p.Str("name"))
	}
	return fmt.Sprintf("%s/%s", ctxName, p.Str("name"))
}

// ProjectPath renders `context/project[:subproject]`.
func (s *Store) ProjectPath(p *row.Row) (path string, err error) {
	defer guard(&err)
	return s.projectPath(p), nil
}

func (s *Store) shortPath(p *row.Row) string {
	path := s.projectPath(p)
	_, after, _ := strings.Cut(path, "/")
	return after
}

// ShortPath renders `project[:subproject]` without the context.
func (s *Store) ShortPath(p *row.Row) (path string, err error) {
	defer guard(&err)
	return s.shortPath(p), nil
}

func (s *Store) resolveProject(ref, context string, required bool) *row.Row {
	refCtx, name, sub, err := refs.ParseProjectRef(ref)
	if err != nil {
		fail(err)
	}
	ctxName := refCtx
	if ctxName == "" {
		ctxName = context
	}
	ctx := s.contextOrDefault(ctxName)
	r := s.one("SELECT * FROM projects WHERE context_id = ? AND parent_id IS NULL AND name = ?", ctx.Int("id"), name)
	if r == nil {
		if required {
			fail(errs.NotFound("project", ref, "no such project: %s/%s", ctx.Str("name"), name))
		}
		return nil
	}
	if sub != "" {
		sr := s.one("SELECT * FROM projects WHERE parent_id = ? AND name = ?", r.Int("id"), sub)
		if sr == nil {
			if required {
				fail(errs.NotFound("project", ref, "no such subproject: %s/%s:%s", ctx.Str("name"), name, sub))
			}
			return nil
		}
		return projectRow(sr)
	}
	return projectRow(r)
}

// ResolveProject resolves `[context/]project[:subproject]`.
func (s *Store) ResolveProject(ref, context string, required bool) (r *row.Row, err error) {
	defer guard(&err)
	return s.resolveProject(ref, context, required), nil
}

// CreateProject adds a project or subproject with optional repo links.
func (s *Store) CreateProject(ref, context, title string, repos []string) (r *row.Row, err error) {
	defer guard(&err)
	refCtx, name, sub, e := refs.ParseProjectRef(ref)
	if e != nil {
		fail(e)
	}
	ctxName := refCtx
	if ctxName == "" {
		ctxName = context
	}
	ctx := s.contextOrDefault(ctxName)
	var parentID any
	if sub != "" {
		parent := s.one("SELECT * FROM projects WHERE context_id = ? AND parent_id IS NULL AND name = ?", ctx.Int("id"), name)
		if parent == nil {
			fail(errs.NotFound("project", ref, "no such parent project: %s/%s (create it before its subprojects)", ctx.Str("name"), name))
		}
		parentID = parent.Int("id")
		name = sub
	}
	now := refs.NowISO()
	var id int64
	s.tx(func() {
		res, e := s.execC("INSERT INTO projects (context_id, parent_id, name, title, created_at, updated_at, uid) VALUES (?,?,?,?,?,?,?)",
			ctx.Int("id"), parentID, name, nullable(title), now, now, NewUID())
		if e != nil {
			fail(errs.Conflict("project already exists: %s", ref))
		}
		id, _ = res.LastInsertId()
		for _, repo := range repos {
			s.insertRepo(id, repo)
		}
		s.event("project", id, "created", ref, 0, id, "")
	})
	return s.getProject(id), nil
}

// insertRepo stores the host-qualified identity when the form carries a host
// (D8), the bare owner/name otherwise.
func (s *Store) insertRepo(projectID int64, repo string) string {
	slug, err := refs.RepoIdentity(repo)
	if err != nil {
		fail(err)
	}
	bare, _ := refs.NormalizeRepo(repo)
	var url any
	if strings.Contains(repo, "/") && repo != bare {
		url = repo
	}
	s.exec("INSERT INTO project_repos (project_id, slug, url, created_at) VALUES (?,?,?,?) ON CONFLICT(project_id, slug) DO NOTHING",
		projectID, slug, url, refs.NowISO())
	return slug
}

// AddRepo links a git remote to a project, warning when another project
// already claims it (then `-p .` cannot resolve it).
func (s *Store) AddRepo(project *row.Row, repo string) (slug string, err error) {
	defer guard(&err)
	var others []string
	for _, p := range s.findProjectsByRepo(repo) {
		if p.Int("id") != project.Int("id") {
			others = append(others, s.projectPath(p))
		}
	}
	if len(others) > 0 {
		id, _ := refs.RepoIdentity(repo)
		fmt.Fprintf(os.Stderr, "agtrack: warning: %s is already linked to %s; `-p .` will not resolve it until one link is removed\n",
			id, strings.Join(others, ", "))
	}
	s.tx(func() {
		slug = s.insertRepo(project.Int("id"), repo)
		s.event("project", project.Int("id"), "repo", slug, 0, project.Int("id"), "")
	})
	return slug, nil
}

// RemoveRepo unlinks a remote (D5: row and event in one tx). A host-less
// `owner/name` removes the one binding of that name on any host; several
// hosts make it ambiguous.
func (s *Store) RemoveRepo(project *row.Row, repo string) (slug string, err error) {
	defer guard(&err)
	id, e := refs.RepoIdentity(repo)
	if e != nil {
		fail(e)
	}
	bare, _ := refs.NormalizeRepo(repo)
	s.tx(func() {
		var slugs []string
		for _, r := range s.q("SELECT slug FROM project_repos r WHERE r.project_id = ? AND "+repoMatch(id, bare), append([]any{project.Int("id")}, repoArgs(id, bare)...)...) {
			slugs = append(slugs, r.Str("slug"))
		}
		switch {
		case len(slugs) == 0:
			fail(errs.NotFound("repo", repo, "%s is not linked to %s", id, s.projectPath(project)))
		case len(slugs) > 1:
			e := errs.New("%s matches %s on %s; name the host", repo, strings.Join(slugs, ", "), s.projectPath(project))
			e.Token = errs.TokAmbiguous
			e.Extra = map[string]any{"candidates": slugs}
			fail(e)
		}
		slug = slugs[0]
		s.exec("DELETE FROM project_repos WHERE project_id = ? AND slug = ?", project.Int("id"), slug)
		s.event("project", project.Int("id"), "repo-removed", slug, 0, project.Int("id"), "")
	})
	return slug, nil
}

// repoMatch is the SQL predicate (on alias r) matching a stored binding
// against a query: the exact identity, the bare owner/name, or — for a
// host-less query — any host (compared with substr, never LIKE: `_` and `%`
// are ordinary characters in repo names).
func repoMatch(id, bare string) string { return repoMatchCol("r.slug", id, bare) }

func repoMatchCol(col, id, bare string) string {
	if id == bare {
		return "(" + col + " = ? OR (length(" + col + ") > length(?) AND substr(" + col + ", length(" + col + ") - length(?)) = ?))"
	}
	return "(" + col + " IN (?, ?))"
}

func repoArgs(id, bare string) []any {
	if id == bare {
		return []any{bare, bare, bare, "/" + bare}
	}
	return []any{id, bare}
}

func (s *Store) projectRepos(projectID int64) []string {
	out := []string{}
	for _, r := range s.q("SELECT slug FROM project_repos WHERE project_id = ? ORDER BY slug", projectID) {
		out = append(out, r.Str("slug"))
	}
	return out
}

// ProjectRepos lists a project's linked repo identities.
func (s *Store) ProjectRepos(projectID int64) (out []string, err error) {
	defer guard(&err)
	return s.projectRepos(projectID), nil
}

// findProjectsByRepo matches a remote against stored bindings. A binding
// matches on the exact identity, on the bare owner/name (legacy and
// host-less bindings), or — for a host-less query — any host.
func (s *Store) findProjectsByRepo(repo string) []*row.Row {
	id, err := refs.RepoIdentity(repo)
	if err != nil {
		fail(err)
	}
	bare, _ := refs.NormalizeRepo(repo)
	query := "SELECT DISTINCT p.* FROM projects p JOIN project_repos r ON r.project_id = p.id WHERE " + repoMatch(id, bare) +
		" ORDER BY p.parent_id IS NULL DESC, p.id"
	args := repoArgs(id, bare)
	var out []*row.Row
	for _, r := range s.q(query, args...) {
		out = append(out, projectRow(r))
	}
	return out
}

// FindProjectsByRepo lists every project or subproject linked to a remote.
func (s *Store) FindProjectsByRepo(repo string) (out []*row.Row, err error) {
	defer guard(&err)
	return s.findProjectsByRepo(repo), nil
}

func (s *Store) descendantIDs(projectID int64) []int64 {
	ids := []int64{projectID}
	for _, r := range s.q("SELECT id FROM projects WHERE parent_id = ?", projectID) {
		ids = append(ids, r.Int("id"))
	}
	return ids
}

// DescendantIDs is the project itself plus its subprojects.
func (s *Store) DescendantIDs(projectID int64) (ids []int64, err error) {
	defer guard(&err)
	return s.descendantIDs(projectID), nil
}

// ProjectFilter narrows ListProjects.
type ProjectFilter struct {
	Context         string
	Parent          *row.Row
	IncludeArchived bool
	AllContexts     bool
}

// ListProjects is one aggregate query (D13), not six per row.
func (s *Store) ListProjects(f ProjectFilter) (out []*row.Row, err error) {
	defer guard(&err)
	return s.listProjects(f), nil
}

func (s *Store) listProjects(f ProjectFilter) []*row.Row {
	var where []string
	var args []any
	if !f.AllContexts {
		ctx := s.contextOrDefault(f.Context)
		where = append(where, "p.context_id = ?")
		args = append(args, ctx.Int("id"))
	}
	if f.Parent != nil {
		where = append(where, "p.parent_id = ?")
		args = append(args, f.Parent.Int("id"))
	}
	if !f.IncludeArchived {
		where = append(where, "p.archived = 0")
	}
	query := `SELECT p.id, p.context_id, p.parent_id, p.name, p.title, p.archived, p.created_at, p.updated_at,
  COALESCE(c.name, '?') AS ctx_name, pp.name AS parent_name,
  (SELECT group_concat(slug, ',') FROM (SELECT slug FROM project_repos WHERE project_id = p.id ORDER BY slug)) AS repo_list,
  (SELECT group_concat(name, ',') FROM (SELECT g.name FROM taggings tg JOIN tags g ON g.id = tg.tag_id
     WHERE tg.target_type = 'project' AND tg.target_id = p.id ORDER BY g.name)) AS tag_list,
  (SELECT COUNT(*) FROM tasks t WHERE t.project_id = p.id OR t.project_id IN (SELECT id FROM projects WHERE parent_id = p.id)) AS n_tasks,
  (SELECT COUNT(*) FROM tasks t WHERE (t.project_id = p.id OR t.project_id IN (SELECT id FROM projects WHERE parent_id = p.id))
     AND t.status IN ('todo','in-progress')) AS n_open
FROM projects p
LEFT JOIN contexts c ON c.id = p.context_id
LEFT JOIN projects pp ON pp.id = p.parent_id`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY COALESCE(p.parent_id, p.id), p.parent_id IS NOT NULL, p.name"
	var out []*row.Row
	for _, r := range s.q(query, args...) {
		item := projectRow(r)
		ref := r.Str("name")
		if !r.IsNull("parent_id") {
			ref = r.Str("parent_name") + ":" + ref
		}
		item.Set("path", r.Str("ctx_name")+"/"+ref)
		item.Set("ref", ref)
		item.Set("repos", r.Str("repo_list"))
		item.Set("tags", r.Str("tag_list"))
		item.Set("open", r.Int("n_open"))
		item.Set("tasks", r.Int("n_tasks"))
		out = append(out, item)
	}
	return out
}

// UpdateProject renames, retitles or (un)archives (D5: one tx).
func (s *Store) UpdateProject(project *row.Row, fields []Field) (r *row.Row, err error) {
	defer guard(&err)
	s.tx(func() {
		if _, e := s.execUpdate("projects", project.Int("id"), fields); e != nil {
			fail(errs.Conflict("project already exists: %s", describeFields(fields)))
		}
		s.event("project", project.Int("id"), "updated", describeFields(fields), 0, project.Int("id"), "")
	})
	return s.getProject(project.Int("id")), nil
}

// DeleteProject removes a project; with force, its subprojects and tasks.
func (s *Store) DeleteProject(project *row.Row, force bool) (err error) {
	defer guard(&err)
	ids := s.descendantIDs(project.Int("id"))
	tasks := s.q("SELECT id FROM tasks WHERE project_id IN ("+marks(len(ids))+")", ids64(ids)...)
	if (len(ids) > 1 || len(tasks) > 0) && !force {
		fail(errs.Conflict("%s still holds %d subproject(s) and %d task(s); pass --force",
			s.projectPath(project), len(ids)-1, len(tasks)))
	}
	path := s.projectPath(project)
	s.tx(func() {
		s.purgeProjectTree(ids)
		s.exec("DELETE FROM projects WHERE id = ?", project.Int("id"))
		s.clearPinIfGone(ids)
		s.event("project", project.Int("id"), "deleted", path, 0, 0, "")
	})
	return nil
}

func (s *Store) purgeProjectTree(projectIDs []int64) {
	if len(projectIDs) == 0 {
		return
	}
	var taskIDs []int64
	for _, r := range s.q("SELECT id FROM tasks WHERE project_id IN ("+marks(len(projectIDs))+")", ids64(projectIDs)...) {
		taskIDs = append(taskIDs, r.Int("id"))
	}
	if len(taskIDs) > 0 {
		var subIDs []int64
		for _, r := range s.q("SELECT id FROM subtasks WHERE task_id IN ("+marks(len(taskIDs))+")", ids64(taskIDs)...) {
			subIDs = append(subIDs, r.Int("id"))
		}
		s.purgePolymorphic("subtask", subIDs)
		s.purgePolymorphic("task", taskIDs)
	}
	s.purgePolymorphic("project", projectIDs)
}

// clearPinIfGone drops every pin (global and per-actor) naming a deleted project.
func (s *Store) clearPinIfGone(projectIDs []int64) {
	for _, id := range projectIDs {
		s.exec("DELETE FROM meta WHERE (key = 'current_project' OR key LIKE 'current_project:%') AND value = ?", fmt.Sprint(id))
	}
}

// TaskCounts counts tasks per status over a set of projects.
func (s *Store) TaskCounts(projectIDs []int64) (counts *row.Row, err error) {
	defer guard(&err)
	return s.taskCounts(projectIDs), nil
}

func (s *Store) taskCounts(projectIDs []int64) *row.Row {
	counts := row.Of("todo", int64(0), "in-progress", int64(0), "done", int64(0), "cancelled", int64(0))
	if len(projectIDs) == 0 {
		return counts
	}
	for _, r := range s.q("SELECT status, COUNT(*) c FROM tasks WHERE project_id IN ("+marks(len(projectIDs))+") GROUP BY status", ids64(projectIDs)...) {
		counts.Set(r.Str("status"), r.Int("c"))
	}
	return counts
}
