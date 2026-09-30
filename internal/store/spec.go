package store

import "github.com/l4sh/agtrack/internal/row"

// SpecChecksForTask summarises the spec sections linked to a task (placeholder
// until the spec store lands).
func (s *Store) SpecChecksForTask(taskID int64, complete bool) (out []*row.Row, err error) {
	defer guard(&err)
	return []*row.Row{}, nil
}

// SpecReviewState summarises pending review traffic on a project's specs.
func (s *Store) SpecReviewState(projectID int64) (out []*row.Row, err error) {
	defer guard(&err)
	return []*row.Row{}, nil
}
