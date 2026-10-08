// Package store is the data access layer. Every write goes through here so
// that the event trail (the worklog) stays in sync with the entity tables.
// The CLI layer never touches SQL directly.
//
// Internally the store panics with a sentinel on database and user errors and
// every exported method recovers into its error return (see guard). This keeps
// a faithful port of the Python store readable; nothing panics across the
// package boundary.
package store

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"strings"

	"github.com/l4sh/agtrack/internal/db"
	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
)

// Store is all reads and writes against one agtrack database.
type Store struct {
	DB    *db.DB
	Actor string // "" = unattributed
	// Session is the harness session stamped on events and claims ("" = none).
	Session string
	// Context is the invocation's explicit -c, if any; it bounds the pins.
	Context string
	// CwdRepo returns the origin remote of the cwd repository, or "". The CLI
	// injects it; tests stub it.
	CwdRepo func() string
}

// New wraps an open database.
func New(d *db.DB, actor string) *Store {
	s := &Store{DB: d, Actor: actor, CwdRepo: func() string { return "" }}
	s.CanonicalizeActor() // D1: an alias is rewritten to its registered slug on the way in
	return s
}

// panicErr carries an error through the internal panic path.
type panicErr struct{ err error }

func fail(err error) { panic(panicErr{err}) }

// guard converts an internal panic back into an error return.
func guard(err *error) {
	if r := recover(); r != nil {
		if p, ok := r.(panicErr); ok {
			*err = p.err
			return
		}
		panic(r)
	}
}

// DBError marks a failure of the database itself (exit 5).
type DBError struct{ Err error }

func (e *DBError) Error() string { return e.Err.Error() }
func (e *DBError) Unwrap() error { return e.Err }

func dbFail(err error) {
	if err != nil {
		fail(&DBError{err})
	}
}

// IsConstraint reports whether err is a SQLite constraint violation.
func IsConstraint(err error) bool {
	return err != nil && strings.Contains(err.Error(), "constraint failed")
}

func (s *Store) q(query string, args ...any) []*row.Row {
	rs, err := s.DB.Query(query, args...)
	dbFail(err)
	out, err := row.Scan(rs)
	dbFail(err)
	return out
}

func (s *Store) one(query string, args ...any) *row.Row {
	rows := s.q(query, args...)
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}

func (s *Store) count(query string, args ...any) int64 {
	var n sql.NullInt64
	dbFail(s.DB.QueryRow(query, args...).Scan(&n))
	return n.Int64
}

func (s *Store) exec(query string, args ...any) sql.Result {
	res, err := s.DB.Exec(query, args...)
	dbFail(err)
	return res
}

// execC runs a statement that may violate a constraint, returning that
// violation as an error instead of failing.
func (s *Store) execC(query string, args ...any) (sql.Result, error) {
	res, err := s.DB.Exec(query, args...)
	if err != nil && !IsConstraint(err) {
		dbFail(err)
	}
	return res, err
}

func (s *Store) insert(query string, args ...any) int64 {
	id, err := s.exec(query, args...).LastInsertId()
	dbFail(err)
	return id
}

// tx runs fn in a write transaction, re-panicking its failure.
func (s *Store) tx(fn func()) {
	err := s.DB.Tx(func() (err error) {
		defer guard(&err)
		fn()
		return nil
	})
	if err != nil {
		fail(err)
	}
}

// Tx runs fn inside one write transaction; store calls inside it join it.
// Used by the CLI to make a whole invocation atomic (D5).
func (s *Store) Tx(fn func() error) error { return s.DB.Tx(fn) }

// Savepoint is Tx for a unit that may fail on its own: inside an open
// transaction a failure undoes only that unit (batch --continue).
func (s *Store) Savepoint(fn func() error) error { return s.DB.Savepoint(fn) }

// NewUID returns a random RFC 4122 v4 uuid.
func NewUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullableID(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func marks(n int) string {
	if n == 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func ids64(ids []int64) []any {
	out := make([]any, len(ids))
	for i, v := range ids {
		out[i] = v
	}
	return out
}

// Event appends a worklog row. at is when the work happened ("" = now);
// the row also keeps the real write time.
func (s *Store) event(targetType string, targetID int64, action string, detail any, taskID, projectID int64, at string) int64 {
	now := refs.NowISO()
	if at == "" {
		at = now
	}
	id := s.insert(
		"INSERT INTO events (at, recorded_at, actor, target_type, target_id,"+
			" action, detail, task_id, project_id, uid, session) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		at, now, nullable(s.Actor), targetType, targetID, action, detail,
		nullableID(taskID), nullableID(projectID), NewUID(), nullable(s.Session))
	// Claim liveness (C3): any write by the holder refreshes last_seen, and a
	// claim with no session yet (a transfer) adopts the holder's first one.
	if taskID > 0 && s.Actor != "" {
		s.exec(`UPDATE tasks SET claim_last_seen = ?, claim_session = COALESCE(claim_session, ?)
WHERE id = ? AND claimed_by = ? AND (claim_session IS NULL OR claim_session = ? OR ? IS NULL)`,
			now, nullable(s.Session), taskID, s.Actor, nullable(s.Session), nullable(s.Session))
	}
	return id
}

// Event is the exported form of event, run in its own transaction.
func (s *Store) Event(targetType string, targetID int64, action, detail string, taskID, projectID int64) (id int64, err error) {
	defer guard(&err)
	s.tx(func() { id = s.event(targetType, targetID, action, nullable(detail), taskID, projectID, "") })
	return id, nil
}

// purgePolymorphic cleans notes and taggings, which have no foreign key.
func (s *Store) purgePolymorphic(targetType string, ids []int64) {
	if len(ids) == 0 {
		return
	}
	args := append([]any{targetType}, ids64(ids)...)
	s.exec("DELETE FROM notes WHERE target_type = ? AND target_id IN ("+marks(len(ids))+")", args...)
	s.exec("DELETE FROM taggings WHERE target_type = ? AND target_id IN ("+marks(len(ids))+")", args...)
}

func describe(fields [][2]any) string {
	var parts []string
	for _, kv := range fields {
		if kv[1] == nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%v=%v", kv[0], kv[1]))
	}
	return strings.Join(parts, ", ")
}

// Field is one column update, in order.
type Field struct {
	Name  string
	Value any // nil = not set
}

func (s *Store) updateRow(table string, id int64, fields []Field) {
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
		return
	}
	args = append(args, refs.NowISO(), id)
	s.exec("UPDATE "+table+" SET "+strings.Join(sets, ", ")+", updated_at = ? WHERE id = ?", args...)
}

// expectTitle is the compare-and-set precondition of a title edit, run inside
// the caller's write transaction so no other write lands between the check
// and the update. A nil expect checks nothing.
func (s *Store) expectTitle(table, noun string, id int64, expect *string, fields []Field) {
	if expect == nil {
		return
	}
	cur := s.one("SELECT title FROM "+table+" WHERE id = ?", id)
	if cur == nil {
		fail(errs.NotFound(noun, fmt.Sprint(id), "no such %s: %d", noun, id))
	}
	if cur.Str("title") == *expect {
		return
	}
	yours := ""
	for _, f := range fields {
		if f.Name == "title" {
			yours, _ = f.Value.(string)
		}
	}
	e := errs.Conflict("the %s's title changed since the page loaded it", noun)
	e.Extra = map[string]any{"current": cur.Str("title"), "yours": yours}
	fail(e)
}

func describeFields(fields []Field) string {
	var kv [][2]any
	for _, f := range fields {
		kv = append(kv, [2]any{f.Name, f.Value})
	}
	return describe(kv)
}

func notBefore(label, value, floorLabel, floor string) {
	if floor != "" && value < floor {
		fail(errs.New("%s (%s) is before %s (%s); backfill %s first if the record is wrong",
			label, refs.FmtTS(value, false, false), floorLabel, refs.FmtTS(floor, false, false), floorLabel))
	}
}

type stamp struct{ label, value string }

func checkOrder(stamps ...stamp) {
	var seen []stamp
	for _, st := range stamps {
		if st.value == "" {
			continue
		}
		if len(seen) > 0 && st.value < seen[len(seen)-1].value {
			prev := seen[len(seen)-1]
			fail(errs.New("%s (%s) is before %s (%s)", st.label, st.value, prev.label, prev.value))
		}
		seen = append(seen, st)
	}
}

func firstLine(body string) string {
	line := strings.TrimSpace(body)
	if i := strings.IndexAny(line, "\n\r"); i >= 0 {
		line = line[:i]
	}
	r := []rune(line)
	if len(r) <= 120 {
		return line
	}
	return string(r[:117]) + "..."
}

var closed = map[string]bool{"done": true, "cancelled": true}

// Closed reports whether a status is terminal.
func Closed(status string) bool { return closed[status] }
