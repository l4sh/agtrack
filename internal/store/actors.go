package store

import (
	"sort"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
)

// The actor registry (agent-features spec D1-D3). The registry is optional:
// with no actors registered nothing is rewritten and nothing warns.

// DefaultApprovers is who may approve rules when rules.approvers is unset;
// it matches the `serve --as` default.
const DefaultApprovers = "human"

// resolveActor maps an alias or slug to its canonical slug.
func (s *Store) resolveActor(name string) (slug string, known bool) {
	if name == "" {
		return "", false
	}
	if r := s.one("SELECT slug FROM actors WHERE slug = ?", name); r != nil {
		return name, true
	}
	if r := s.one("SELECT a.slug FROM actor_aliases l JOIN actors a ON a.id = l.actor_id WHERE l.alias = ?", name); r != nil {
		return r.Str("slug"), true
	}
	return name, false
}

// ResolveActor returns the canonical slug for name, whether the registry
// knows it, and whether the registry has any actors at all.
func (s *Store) ResolveActor(name string) (slug string, known, registryEmpty bool, err error) {
	defer guard(&err)
	slug, known = s.resolveActor(name)
	return slug, known, s.count("SELECT COUNT(*) FROM actors") == 0, nil
}

// CanonicalizeActor rewrites s.Actor to its canonical slug. Errors are
// ignored: an unreadable registry must not stop a write.
func (s *Store) CanonicalizeActor() {
	if s == nil || s.DB == nil || s.Actor == "" {
		return
	}
	if slug, _, _, err := s.ResolveActor(s.Actor); err == nil {
		s.Actor = slug
	}
}

// Config reads a config setting stored by `config set`, or def when unset.
func (s *Store) Config(key, def string) (v string, err error) {
	defer guard(&err)
	if v, ok := s.getMeta("config:" + key); ok {
		return v, nil
	}
	return def, nil
}

// ConfigBool reads a true/false setting (false on any error).
func (s *Store) ConfigBool(key string) bool {
	v, err := s.Config(key, "false")
	return err == nil && v == "true"
}

// IsApprover reports whether actor may approve rules, make them hard, or
// apply done-check without a terminal (config rules.approvers, default
// "human"). Entries and actor are compared after alias resolution. This
// informs cooperative agents; it is not a security boundary.
func (s *Store) IsApprover(actor string) bool {
	if actor == "" {
		return false
	}
	list, err := s.Config("rules.approvers", DefaultApprovers)
	if err != nil {
		return false
	}
	canon, _, _, err := s.ResolveActor(actor)
	if err != nil {
		return false
	}
	for _, a := range strings.Split(list, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if a == actor || a == canon {
			return true
		}
		if c, _, _, err := s.ResolveActor(a); err == nil && c == canon {
			return true
		}
	}
	return false
}

func checkKind(kind string) string {
	if kind == "" {
		return "agent"
	}
	if kind != "agent" && kind != "human" {
		fail(errs.Usage("invalid actor kind: %s (expected agent or human)", kind))
	}
	return kind
}

// claimName fails when name already resolves to an actor.
func (s *Store) claimName(name string) {
	if name == "" {
		fail(errs.Usage("actor name is empty"))
	}
	if slug, known := s.resolveActor(name); known {
		fail(errs.Conflict("%s is already registered (as %s)", name, slug))
	}
}

// AddActor registers an actor with optional aliases.
func (s *Store) AddActor(slug, kind, note string, aliases []string) (r *row.Row, err error) {
	defer guard(&err)
	kind = checkKind(kind)
	s.tx(func() {
		s.claimName(slug)
		id := s.insert("INSERT INTO actors (slug, kind, note, created_at) VALUES (?,?,?,?)", slug, kind, nullable(note), refs.NowISO())
		for _, a := range aliases {
			if a == slug {
				continue
			}
			s.claimName(a)
			s.exec("INSERT INTO actor_aliases (alias, actor_id) VALUES (?,?)", a, id)
		}
		s.event("actor", id, "actor-add", nullable(slug), 0, 0, "")
	})
	return s.actorRow(slug), nil
}

// AliasActor adds an alias to a registered actor.
func (s *Store) AliasActor(slug, alias string) (r *row.Row, err error) {
	defer guard(&err)
	var canon string
	s.tx(func() {
		a := s.mustActor(slug)
		canon = a.Str("slug")
		s.claimName(alias)
		s.exec("INSERT INTO actor_aliases (alias, actor_id) VALUES (?,?)", alias, a.Int("id"))
		s.event("actor", a.Int("id"), "actor-alias", nullable(alias+" -> "+canon), 0, 0, "")
	})
	return s.actorRow(canon), nil
}

func (s *Store) mustActor(name string) *row.Row {
	slug, known := s.resolveActor(name)
	if !known {
		fail(errs.NotFound("actor", name, "no actor %s (see `actor ls`)", name))
	}
	return s.one("SELECT id, slug FROM actors WHERE slug = ?", slug)
}

func (s *Store) actorRow(slug string) *row.Row {
	r := s.one("SELECT id, slug, kind, note, created_at FROM actors WHERE slug = ?", slug)
	if r == nil {
		return nil
	}
	s.fillAliases(r)
	return r
}

func (s *Store) fillAliases(r *row.Row) {
	var al []string
	for _, a := range s.q("SELECT alias FROM actor_aliases WHERE actor_id = ? ORDER BY alias", r.Int("id")) {
		al = append(al, a.Str("alias"))
	}
	r.Set("aliases", strings.Join(al, ","))
}

// ListActors returns the registry with alias lists.
func (s *Store) ListActors() (out []*row.Row, err error) {
	defer guard(&err)
	out = s.q("SELECT id, slug, kind, note, created_at FROM actors ORDER BY slug")
	for _, r := range out {
		s.fillAliases(r)
	}
	return out, nil
}

// actorColumns lists every (table, column) that records who did something:
// columns named actor or ending in _by.
func (s *Store) actorColumns() [][2]string {
	var out [][2]string
	for _, t := range s.q("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name") {
		name := t.Str("name")
		if strings.HasPrefix(name, "lessons_fts") || name == "actors" || name == "actor_aliases" {
			continue
		}
		for _, c := range s.q(`SELECT name FROM pragma_table_info('` + strings.ReplaceAll(name, "'", "''") + `')`) {
			col := c.Str("name")
			if col == "actor" || strings.HasSuffix(col, "_by") {
				out = append(out, [2]string{name, col})
			}
		}
	}
	return out
}

// actorUsage counts the rows that name any of names.
func (s *Store) actorUsage(names []string) int64 {
	var n int64
	for _, tc := range s.actorColumns() {
		n += s.count("SELECT COUNT(*) FROM "+tc[0]+" WHERE "+tc[1]+" IN ("+marks(len(names))+")", strAny(names)...)
	}
	for _, name := range names {
		n += s.count(`SELECT COUNT(*) FROM meta WHERE key LIKE ? ESCAPE '\'`, "%:"+escapeLike(name))
	}
	return n
}

func strAny(v []string) []any {
	out := make([]any, len(v))
	for i, x := range v {
		out[i] = x
	}
	return out
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// RemoveActor deletes an actor that no row refers to.
func (s *Store) RemoveActor(name string) (err error) {
	defer guard(&err)
	s.tx(func() {
		a := s.mustActor(name)
		names := []string{a.Str("slug")}
		for _, al := range s.q("SELECT alias FROM actor_aliases WHERE actor_id = ?", a.Int("id")) {
			names = append(names, al.Str("alias"))
		}
		if n := s.actorUsage(names); n > 0 {
			fail(errs.Conflict("actor %s is used by %d row(s); `actor merge` it into another instead", a.Str("slug"), n))
		}
		s.exec("DELETE FROM actors WHERE id = ?", a.Int("id"))
		s.event("actor", a.Int("id"), "actor-rm", nullable(a.Str("slug")), 0, 0, "")
	})
	return nil
}

// MergeActor rewrites every record written under alias to into, in one
// transaction, and registers alias as an alias of into so later writes land
// on the canonical name. alias may be a registered alias, a registered actor
// (which is then removed) or a free-form string seen only in old rows.
// It returns the rows changed per table.column.
func (s *Store) MergeActor(alias, into string) (changes map[string]int64, err error) {
	defer guard(&err)
	changes = map[string]int64{}
	s.tx(func() {
		target := s.mustActor(into)
		slug := target.Str("slug")
		var names []string
		if src := s.one("SELECT id, slug FROM actors WHERE slug = ?", alias); src != nil {
			names = append(names, alias)
			for _, al := range s.q("SELECT alias FROM actor_aliases WHERE actor_id = ?", src.Int("id")) {
				names = append(names, al.Str("alias"))
			}
			s.exec("DELETE FROM actors WHERE id = ?", src.Int("id")) // cascades its aliases
		} else {
			names = []string{alias}
		}
		if alias == slug {
			fail(errs.Usage("%s is already the canonical name", alias))
		}
		for _, n := range names {
			if n == slug {
				continue
			}
			for _, tc := range s.actorColumns() {
				res := s.exec("UPDATE "+tc[0]+" SET "+tc[1]+" = ? WHERE "+tc[1]+" = ?", slug, n)
				if k, _ := res.RowsAffected(); k > 0 {
					changes[tc[0]+"."+tc[1]] += k
				}
			}
			// per-actor pins live in meta as <key>:<actor>
			for _, m := range s.q(`SELECT key FROM meta WHERE key LIKE ? ESCAPE '\'`, "%:"+escapeLike(n)) {
				base := strings.TrimSuffix(m.Str("key"), ":"+n)
				if _, taken := s.getMeta(base + ":" + slug); taken {
					s.exec("DELETE FROM meta WHERE key = ?", m.Str("key"))
				} else {
					s.exec("UPDATE meta SET key = ? WHERE key = ?", base+":"+slug, m.Str("key"))
				}
				changes["meta"]++
			}
			s.exec("INSERT OR IGNORE INTO actor_aliases (alias, actor_id) VALUES (?,?)", n, target.Int("id"))
		}
		for _, n := range names {
			if s.Actor == n {
				s.Actor = slug // the merge itself is written under the canonical name
			}
		}
		s.event("actor", target.Int("id"), "actor-merge", nullable(strings.Join(names, ",")+" -> "+slug), 0, 0, "")
	})
	return changes, nil
}

// ActorKeys returns the keys of a merge result in stable order.
func ActorKeys(m map[string]int64) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
