package store

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
)

// --- meta / current scope ----------------------------------------------------
//
// D2: pins are scoped per actor. An actor's writes go to
// `current_<kind>:<actor>`; reads try that key, then the global key. The
// human, who usually sets no actor, keeps the global pin.
//
// D1: the project pin is only honoured when it lies inside the current
// context, and `ctx use` drops a project pin that does not, so the context
// and project pins can never describe two different places.

func (s *Store) getMeta(key string) (string, bool) {
	r := s.one("SELECT value FROM meta WHERE key = ?", key)
	if r == nil || r.IsNull("value") {
		return "", false
	}
	return r.Str("value"), true
}

func (s *Store) setMeta(key string, value *string) {
	if value == nil {
		s.exec("DELETE FROM meta WHERE key = ?", key)
		return
	}
	s.exec("INSERT INTO meta (key, value) VALUES (?,?) "+
		"ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, *value)
}

// GetMeta reads a meta key.
func (s *Store) GetMeta(key string) (v string, ok bool, err error) {
	defer guard(&err)
	v, ok = s.getMeta(key)
	return v, ok, nil
}

// SetMeta writes (or with nil, deletes) a meta key.
func (s *Store) SetMeta(key string, value *string) (err error) {
	defer guard(&err)
	s.tx(func() { s.setMeta(key, value) })
	return nil
}

func (s *Store) scoped(base string) string {
	if s.Actor == "" {
		return base
	}
	return base + ":" + s.Actor
}

func (s *Store) pin(base string) (string, bool) {
	if s.Actor != "" {
		if v, ok := s.getMeta(base + ":" + s.Actor); ok && v != "" {
			return v, true
		}
	}
	v, ok := s.getMeta(base)
	return v, ok && v != ""
}

func strp(v string) *string { return &v }

func (s *Store) currentContext() *row.Row {
	name, ok := s.pin("current_context")
	if !ok {
		return nil
	}
	if r := s.one("SELECT * FROM contexts WHERE name = ?", name); r != nil {
		return contextRow(r)
	}
	return nil
}

// CurrentContext is the pinned context, or nil.
func (s *Store) CurrentContext() (r *row.Row, err error) {
	defer guard(&err)
	return s.currentContext(), nil
}

// currentProject is the pinned project, provided it lies in the current
// context (D1). A pin into another context is stale and ignored.
func (s *Store) currentProject() *row.Row {
	value, ok := s.projectPin()
	if !ok {
		return nil
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil
	}
	p := s.one("SELECT * FROM projects WHERE id = ?", id)
	if p == nil {
		return nil
	}
	if ctx := s.scopeContext(); ctx != nil && ctx.Int("id") != p.Int("context_id") {
		return nil
	}
	return projectRow(p)
}

// projectPin reads the project pin. An actor that has scoped itself (its own
// context pin) never falls through to the human's global project pin: after
// `ctx use` drops the actor's pin, an unqualified write must fail, not land in
// someone else's project (design §4.5).
func (s *Store) projectPin() (string, bool) {
	if s.Actor != "" {
		if v, ok := s.getMeta("current_project:" + s.Actor); ok && v != "" {
			return v, true
		}
		if _, ok := s.getMeta("current_context:" + s.Actor); ok {
			return "", false
		}
	}
	v, ok := s.getMeta("current_project")
	return v, ok && v != ""
}

// scopeContext is the context an invocation works in: an explicit -c wins
// over the pin (D1: a named scope is never overridden by a pin elsewhere).
func (s *Store) scopeContext() *row.Row {
	if s.Context != "" {
		return s.getContext(s.Context, false)
	}
	return s.currentContext()
}

// CurrentProject is the pinned project, or nil.
func (s *Store) CurrentProject() (r *row.Row, err error) {
	defer guard(&err)
	return s.currentProject(), nil
}

func (s *Store) useContext(name string) *row.Row {
	ctx := s.getContext(name, true)
	s.tx(func() {
		s.setMeta(s.scoped("current_context"), strp(ctx.Str("name")))
		// D1: a project pin that points into another context is dropped.
		if value, ok := s.projectPin(); ok {
			id, _ := strconv.ParseInt(value, 10, 64)
			p := s.one("SELECT context_id FROM projects WHERE id = ?", id)
			if p == nil || p.Int("context_id") != ctx.Int("id") {
				// A global pin may still shine through a cleared actor pin;
				// currentProject() ignores it while it lies elsewhere.
				s.setMeta(s.scoped("current_project"), nil)
			}
		}
		s.event("context", ctx.Int("id"), "use", nil, 0, 0, "")
	})
	return ctx
}

// UseContext pins a context for this actor.
func (s *Store) UseContext(name string) (r *row.Row, err error) {
	defer guard(&err)
	return s.useContext(name), nil
}

func (s *Store) useProject(project *row.Row) *row.Row {
	s.tx(func() {
		s.setMeta(s.scoped("current_project"), strp(strconv.FormatInt(project.Int("id"), 10)))
		if ctx := s.one("SELECT name FROM contexts WHERE id = ?", project.Int("context_id")); ctx != nil {
			s.setMeta(s.scoped("current_context"), strp(ctx.Str("name")))
		}
		s.event("project", project.Int("id"), "use", nil, 0, project.Int("id"), "")
	})
	return project
}

// UseProject pins a project (and its context) so later commands can omit --project.
func (s *Store) UseProject(project *row.Row) (r *row.Row, err error) {
	defer guard(&err)
	return s.useProject(project), nil
}

// defaultProject resolves an omitted --project: the pin, else the unique
// project linked to the cwd remote, else fail (D1, design §4.5).
func (s *Store) defaultProject() *row.Row {
	if p := s.currentProject(); p != nil {
		return p
	}
	if remote := s.CwdRepo(); remote != "" {
		if matches := s.findProjectsByRepo(remote); len(matches) == 1 {
			if ctx := s.scopeContext(); s.Context == "" || (ctx != nil && ctx.Int("id") == matches[0].Int("context_id")) {
				return matches[0]
			}
		}
	}
	fail(errs.New("no project given and none is current: pass --project or run `agtrack project use <ref>`"))
	return nil
}

// DefaultProject is the project an unqualified write lands in.
func (s *Store) DefaultProject() (r *row.Row, err error) {
	defer guard(&err)
	return s.defaultProject(), nil
}

// --- contexts ----------------------------------------------------------------

func (s *Store) getContext(name string, required bool) *row.Row {
	slug, err := refs.Slugify(name)
	if err != nil {
		fail(err)
	}
	r := s.one("SELECT * FROM contexts WHERE name = ?", slug)
	if r == nil {
		if required {
			fail(errs.NotFound("context", name, "no such context: %s", name))
		}
		return nil
	}
	return contextRow(r)
}

// GetContext looks a context up by name.
func (s *Store) GetContext(name string, required bool) (r *row.Row, err error) {
	defer guard(&err)
	return s.getContext(name, required), nil
}

func (s *Store) defaultContext() *row.Row {
	if ctx := s.currentContext(); ctx != nil {
		return ctx
	}
	rows := s.q("SELECT * FROM contexts WHERE archived = 0 ORDER BY id")
	switch {
	case len(rows) == 1:
		return contextRow(rows[0])
	case len(rows) == 0:
		fail(errs.New("no contexts yet: agtrack ctx add <name>"))
	}
	fail(errs.New("several contexts exist and none is current: pass --context or run `agtrack ctx use <name>`"))
	return nil
}

// GetContextID fetches a context by id.
func (s *Store) GetContextID(id int64) (r *row.Row, err error) {
	defer guard(&err)
	r = s.one("SELECT * FROM contexts WHERE id = ?", id)
	if r == nil {
		fail(errs.NotFound("context", fmt.Sprint(id), "no such context: %d", id))
	}
	return contextRow(r), nil
}

// DefaultContext is the context to use when a command did not name one.
func (s *Store) DefaultContext() (r *row.Row, err error) {
	defer guard(&err)
	return s.defaultContext(), nil
}

func (s *Store) contextOrDefault(name string) *row.Row {
	if name != "" {
		return s.getContext(name, true)
	}
	return s.defaultContext()
}

// CreateContext adds a context; the first one becomes current.
func (s *Store) CreateContext(name, title string) (r *row.Row, err error) {
	defer guard(&err)
	slug, e := refs.Slugify(name)
	if e != nil {
		fail(e)
	}
	if s.getContext(slug, false) != nil {
		fail(errs.Conflict("context already exists: %s", slug))
	}
	now := refs.NowISO()
	s.tx(func() {
		id := s.insert("INSERT INTO contexts (name, title, created_at, updated_at, uid) VALUES (?,?,?,?,?)",
			slug, nullable(title), now, now, NewUID())
		s.event("context", id, "created", slug, 0, 0, "")
		if _, ok := s.getMeta("current_context"); !ok {
			s.setMeta("current_context", strp(slug))
		}
	})
	return s.getContext(slug, true), nil
}

// ListContexts lists contexts by name with project and open-task counts.
func (s *Store) ListContexts(includeArchived bool) (out []*row.Row, err error) {
	defer guard(&err)
	sql := "SELECT c.*," +
		" (SELECT COUNT(*) FROM projects p WHERE p.context_id = c.id AND p.parent_id IS NULL) AS projects_n," +
		" (SELECT COUNT(*) FROM tasks t JOIN projects p ON p.id = t.project_id" +
		"  WHERE p.context_id = c.id AND t.status IN ('todo','in-progress')) AS open_n" +
		" FROM contexts c"
	if !includeArchived {
		sql += " WHERE c.archived = 0"
	}
	sql += " ORDER BY c.name"
	cur := ""
	if c := s.currentContext(); c != nil {
		cur = c.Str("name")
	}
	for _, r := range s.q(sql) {
		item := contextRow(r)
		item.Set("current", item.Str("name") == cur)
		item.Set("projects", r.Int("projects_n"))
		item.Set("open_tasks", r.Int("open_n"))
		out = append(out, item)
	}
	return out, nil
}

// contextRow is the context table's own columns, in v2 order (uid is
// internal and never printed).
func contextRow(r *row.Row) *row.Row {
	return pick(r, "id", "name", "title", "archived", "created_at", "updated_at")
}

func pick(r *row.Row, keys ...string) *row.Row {
	out := row.New()
	for _, k := range keys {
		out.Set(k, r.Get(k))
	}
	return out
}

// UpdateContext renames, retitles or (un)archives a context (D5: one tx).
func (s *Store) UpdateContext(name string, fields []Field) (r *row.Row, err error) {
	defer guard(&err)
	ctx := s.getContext(name, true)
	newName := ctx.Str("name")
	for _, f := range fields {
		if f.Name == "name" && f.Value != nil {
			newName = f.Value.(string)
		}
	}
	s.tx(func() {
		if _, e := s.execUpdate("contexts", ctx.Int("id"), fields); e != nil {
			fail(errs.Conflict("context already exists: %s", newName))
		}
		s.event("context", ctx.Int("id"), "updated", describeFields(fields), 0, 0, "")
		if newName != ctx.Str("name") {
			// Keep every pin naming the old context pointing at the renamed one.
			s.exec("UPDATE meta SET value = ? WHERE (key = 'current_context' OR key LIKE 'current_context:%') AND value = ?",
				newName, ctx.Str("name"))
		}
	})
	return contextRow(s.getContext(newName, true)), nil
}

func (s *Store) execUpdate(table string, id int64, fields []Field) (bool, error) {
	var sets []string
	var args []any
	for _, f := range fields {
		if f.Value == nil {
			continue
		}
		sets = append(sets, f.Name+" = ?")
		args = append(args, f.Value)
	}
	if len(sets) == 0 {
		return false, nil
	}
	args = append(args, refs.NowISO(), id)
	_, err := s.execC("UPDATE "+table+" SET "+strings.Join(sets, ", ")+", updated_at = ? WHERE id = ?", args...)
	return err == nil, err
}

// DeleteContext removes a context; with force, everything under it.
func (s *Store) DeleteContext(name string, force bool) (r *row.Row, err error) {
	defer guard(&err)
	ctx := s.getContext(name, true)
	projects := s.q("SELECT id FROM projects WHERE context_id = ?", ctx.Int("id"))
	if len(projects) > 0 && !force {
		fail(errs.Conflict("context %s still holds %d project(s); pass --force to delete everything under it",
			ctx.Str("name"), len(projects)))
	}
	s.tx(func() {
		var ids []int64
		for _, p := range projects {
			ids = append(ids, p.Int("id"))
		}
		s.purgeProjectTree(ids)
		s.clearPinIfGone(ids)
		s.purgePolymorphic("context", []int64{ctx.Int("id")})
		s.exec("DELETE FROM contexts WHERE id = ?", ctx.Int("id"))
		s.exec("DELETE FROM meta WHERE (key = 'current_context' OR key LIKE 'current_context:%') AND value = ?", ctx.Str("name"))
		s.event("context", ctx.Int("id"), "deleted", ctx.Str("name"), 0, 0, "")
	})
	return contextRow(ctx), nil
}
