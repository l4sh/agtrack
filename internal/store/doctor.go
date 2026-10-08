package store

import "github.com/l4sh/agtrack/internal/row"

// Read-only queries behind `doctor` (G1) that no other verb already exposes.

// ActorDrift lists actor names in use (events, notes, live claims) that the
// registry does not hold as a slug. aliased names resolve to a registered
// actor (merge them); unknown names are in no registry entry at all, and are
// only reported when the registry has actors. Each row: name, slug (aliased
// only), uses.
func (s *Store) ActorDrift() (aliased, unknown []*row.Row, err error) {
	defer guard(&err)
	aliased, unknown = []*row.Row{}, []*row.Row{}
	empty := s.count("SELECT COUNT(*) FROM actors") == 0
	for _, r := range s.q(`SELECT name, SUM(n) AS uses FROM (
  SELECT actor AS name, COUNT(*) AS n FROM events WHERE actor IS NOT NULL AND actor <> '' GROUP BY actor
  UNION ALL SELECT actor, COUNT(*) FROM notes WHERE actor IS NOT NULL AND actor <> '' GROUP BY actor
  UNION ALL SELECT claimed_by, COUNT(*) FROM tasks WHERE claimed_by IS NOT NULL AND claimed_by <> '' GROUP BY claimed_by
) GROUP BY name ORDER BY name`) {
		name := r.Str("name")
		slug, known := s.resolveActor(name)
		switch {
		case known && slug != name:
			aliased = append(aliased, row.Of("name", name, "slug", slug, "uses", r.Int("uses")))
		case !known && !empty:
			unknown = append(unknown, row.Of("name", name, "uses", r.Int("uses")))
		}
	}
	return aliased, unknown, nil
}

// StaleLessons lists active lessons not verified within lessons.stale_days.
func (s *Store) StaleLessons() (out []*row.Row, err error) {
	defer guard(&err)
	out = []*row.Row{}
	out = append(out, s.q(`SELECT id, title, COALESCE(verified_at, created_at) AS verified FROM lessons
WHERE status = 'active' AND COALESCE(verified_at, created_at) < ? ORDER BY id`, s.staleCutoff())...)
	return out, nil
}

// ProposedRules lists rules waiting for an approver, machine-wide.
func (s *Store) ProposedRules() (out []*row.Row, err error) {
	defer guard(&err)
	out = []*row.Row{}
	for _, r := range s.q("SELECT * FROM rules WHERE status = 'proposed' ORDER BY id") {
		out = append(out, row.Of("id", r.Int("id"), "summary", RuleSummary(r)))
	}
	return out, nil
}
