package store

import "github.com/l4sh/agtrack/internal/row"

// ProposedRuleRows lists every rule waiting for an approver, in any scope,
// oldest first (the browse app's "proposed" list behind the top-bar badge).
func (s *Store) ProposedRuleRows() (out []*row.Row, err error) {
	defer guard(&err)
	cache := map[string]string{}
	out = []*row.Row{}
	for _, r := range s.q("SELECT " + ruleCols + " FROM rules WHERE status = 'proposed' ORDER BY id") {
		out = append(out, s.ruleOut(r, cache))
	}
	return out, nil
}
