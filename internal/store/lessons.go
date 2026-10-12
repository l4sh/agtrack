package store

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/spec"
)

// Shared lessons (agent-features spec H2, H3): gotchas, fixes, decisions and
// facts learned the hard way, scoped like rules and found by trigger (a
// path glob, a command prefix, a topic) or full-text search.

// LessonKinds are the kinds a lesson can have.
var LessonKinds = []string{"gotcha", "fix", "decision", "fact"}

// DefaultLessonStaleDays is lessons.stale_days when unset.
const DefaultLessonStaleDays = 90

// LessonInput are the inputs of AddLesson.
type LessonInput struct {
	Title, Body, Kind   string
	Scope               Scope
	Paths, Cmds, Topics []string
	Supersedes          int64
	Force               bool // skip the duplicate check
}

const lessonCols = `id, uid, title, body, kind, scope_type, scope_id, status, source_task, verified_at, verified_by, supersedes, created_at, actor`

func normTrigger(typ, v string) string {
	v = strings.TrimSpace(v)
	switch typ {
	case "cmd":
		v = strings.Join(strings.Fields(v), " ")
	case "topic":
		v = strings.ToLower(v)
	case "path":
		v = strings.TrimPrefix(v, "./")
	}
	return v
}

func (s *Store) addTriggers(id int64, typ string, values []string) {
	for _, v := range values {
		if v = normTrigger(typ, v); v != "" {
			s.exec("INSERT OR IGNORE INTO lesson_triggers (lesson_id, type, value) VALUES (?,?,?)", id, typ, v)
		}
	}
}

func (s *Store) lessonEvent(id int64, action, note string) {
	l := s.one("SELECT scope_type, scope_id, title FROM lessons WHERE id = ?", id)
	taskID, projectID := s.scopeEventIDs(l.Str("scope_type"), l.Int("scope_id"))
	detail := l.Str("title")
	if note != "" {
		detail += ": " + note
	}
	s.event("lesson", id, "lesson-"+action, detail, taskID, projectID, "")
}

// AddLesson records a lesson. A lesson whose title is similar (token
// Jaccard ≥ SimilarityThreshold) to an active lesson at the same or a wider
// scope refuses the add unless in.Force or in.Supersedes names it; a
// superseded lesson is retired.
func (s *Store) AddLesson(in LessonInput) (r *row.Row, err error) {
	defer guard(&err)
	var id int64
	s.tx(func() {
		in.Title = strings.TrimSpace(in.Title)
		if in.Title == "" {
			fail(errs.Usage("a lesson needs a title"))
		}
		if in.Kind == "" {
			in.Kind = "gotcha"
		}
		if !oneOf(in.Kind, LessonKinds) {
			fail(errs.Usage("invalid lesson kind: %q (%s)", in.Kind, strings.Join(LessonKinds, ", ")))
		}
		s.mustScope(in.Scope)
		if in.Supersedes != 0 {
			s.mustLesson(in.Supersedes)
		}
		if !in.Force {
			where, args, _ := ladderSQL(s.ladder(PointOf(in.Scope)))
			cache := map[string]string{}
			for _, old := range s.q("SELECT id, title, scope_type, scope_id FROM lessons WHERE "+where+" AND status = 'active' ORDER BY id", args...) {
				if old.Int("id") != in.Supersedes && Jaccard(old.Str("title"), in.Title) >= SimilarityThreshold {
					e := errs.Conflict("similar lesson #%d exists (%q, %s); use --supersedes %d or --force",
						old.Int("id"), old.Str("title"), s.scopeLabel(old.Str("scope_type"), old.Int("scope_id"), cache), old.Int("id"))
					e.Extra = map[string]any{"similar": old.Int("id")}
					fail(e)
				}
			}
		}
		var scopeID, sourceTask any
		if in.Scope.Type != "global" {
			scopeID = in.Scope.ID
		}
		if in.Scope.Type == "task" {
			sourceTask = in.Scope.ID
		}
		now := refs.NowISO()
		id = s.insert(`INSERT INTO lessons (uid, title, body, kind, scope_type, scope_id, status, source_task, verified_at, verified_by, supersedes, created_at, actor)
 VALUES (?,?,?,?,?,?,'active',?,?,?,?,?,?)`,
			NewUID(), in.Title, strings.TrimSpace(in.Body), in.Kind, in.Scope.Type, scopeID, sourceTask, now, nullable(s.Actor),
			nullableID(in.Supersedes), now, nullable(s.Actor))
		s.addTriggers(id, "path", in.Paths)
		s.addTriggers(id, "cmd", in.Cmds)
		s.addTriggers(id, "topic", in.Topics)
		s.lessonEvent(id, "add", "")
		if in.Supersedes != 0 {
			res := s.exec("UPDATE lessons SET status = 'retired' WHERE id = ? AND status = 'active'", in.Supersedes)
			if n, _ := res.RowsAffected(); n > 0 {
				s.lessonEvent(in.Supersedes, "retire", fmt.Sprintf("superseded by lesson %d", id))
			}
		}
	})
	return s.lessonView(id), nil
}

func (s *Store) mustLesson(id int64) *row.Row {
	l := s.one("SELECT "+lessonCols+" FROM lessons WHERE id = ?", id)
	if l == nil {
		fail(errs.NotFound("lesson", fmt.Sprint(id), "no such lesson: %d", id))
	}
	return l
}

// staleCutoff is the verified_at before which a lesson reads as unverified.
func (s *Store) staleCutoff() string {
	days := DefaultLessonStaleDays
	if v, ok := s.getMeta("config:lessons.stale_days"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			days = n
		}
	}
	return refs.ToUTCISO(refs.Now().Add(-time.Duration(days) * 24 * time.Hour))
}

func (s *Store) triggers(id int64) (paths, cmds, topics []string) {
	paths, cmds, topics = []string{}, []string{}, []string{}
	for _, t := range s.q("SELECT type, value FROM lesson_triggers WHERE lesson_id = ? ORDER BY type, value", id) {
		switch t.Str("type") {
		case "path":
			paths = append(paths, t.Str("value"))
		case "cmd":
			cmds = append(cmds, t.Str("value"))
		case "topic":
			topics = append(topics, t.Str("value"))
		}
	}
	return
}

// lessonOut is the public shape of a lesson row.
func (s *Store) lessonOut(l *row.Row, cache map[string]string, cutoff string) *row.Row {
	paths, cmds, topics := s.triggers(l.Int("id"))
	verified := l.Str("verified_at")
	if verified == "" {
		verified = l.Str("created_at")
	}
	var stale any
	if verified < cutoff {
		stale = "unverified since " + refs.FmtTS(verified, false, true)
	}
	var scopeID any
	if l.Str("scope_type") != "global" {
		scopeID = l.Int("scope_id")
	}
	return row.Of("id", l.Int("id"), "uid", l.Str("uid"), "title", l.Str("title"), "kind", l.Str("kind"),
		"scope", s.scopeLabel(l.Str("scope_type"), l.Int("scope_id"), cache), "scope_type", l.Str("scope_type"), "scope_id", scopeID,
		"status", l.Str("status"), "age", ageText(l.Str("created_at")), "verified_at", l.Get("verified_at"), "verified_by", l.Get("verified_by"),
		"stale", stale, "paths", paths, "cmds", cmds, "topics", topics, "body", l.Str("body"),
		"source_task", l.Get("source_task"), "supersedes", l.Get("supersedes"), "actor", l.Get("actor"), "created_at", l.Str("created_at"))
}

// ageText is a compact age: 45m, 6h, 3d.
func ageText(ts string) string {
	t, err := time.Parse(time.RFC3339, strings.Replace(ts, "+00:00", "Z", 1))
	if err != nil {
		return ""
	}
	d := refs.Now().Sub(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func (s *Store) lessonView(id int64) *row.Row {
	return s.lessonOut(s.mustLesson(id), map[string]string{}, s.staleCutoff())
}

// GetLesson returns one lesson with its triggers.
func (s *Store) GetLesson(id int64) (r *row.Row, err error) {
	defer guard(&err)
	out := s.lessonView(id)
	out.Set("events", s.q("SELECT action, actor, at, detail FROM events WHERE target_type = 'lesson' AND target_id = ? ORDER BY id", id))
	return out, nil
}

// LessonQuery filters ListLessons.
type LessonQuery struct {
	Point Point
	// Path, Cmd, Topic and Q select lessons by trigger or text; with any of
	// them set, only lessons matching at least one are returned.
	Path, Cmd, Topic, Q string
	// UnverifiedSince keeps lessons not verified since this ISO time.
	UnverifiedSince string
	Retired         bool // retired lessons too
	Limit           int  // 0 = all
}

// Lesson ranking weights: a trigger hit outranks a text hit.
const (
	scorePath  = 4
	scoreCmd   = 4
	scoreTopic = 3
	scoreText  = 2
)

// ListLessons returns the lessons binding a point, ranked by match score
// (path, command, topic, text), then by specificity (narrowest scope
// first), then newest first. total is before Limit.
func (s *Store) ListLessons(q LessonQuery) (out []*row.Row, total int, err error) {
	defer guard(&err)
	where, args, rank := ladderSQL(s.ladder(q.Point))
	sql := "SELECT " + lessonCols + " FROM lessons WHERE " + where
	if !q.Retired {
		sql += " AND status = 'active'"
	}
	if q.UnverifiedSince != "" {
		sql += " AND COALESCE(verified_at, created_at) < ?"
		args = append(args, q.UnverifiedSince)
	}
	rows := s.q(sql, args...)
	textHit := map[int64]bool{}
	if strings.TrimSpace(q.Q) != "" {
		for _, r := range s.q("SELECT rowid AS id FROM lessons_fts WHERE lessons_fts MATCH ?", spec.EscapeFTS(q.Q)) {
			textHit[r.Int("id")] = true
		}
	}
	path := normTrigger("path", q.Path)
	cmd := normTrigger("cmd", q.Cmd)
	topic := normTrigger("topic", q.Topic)
	filtered := path != "" || cmd != "" || topic != "" || strings.TrimSpace(q.Q) != ""
	type scored struct {
		r     *row.Row
		score int
		rank  int
	}
	var hits []scored
	for _, l := range rows {
		score := 0
		if filtered {
			for _, t := range s.q("SELECT type, value FROM lesson_triggers WHERE lesson_id = ?", l.Int("id")) {
				v := t.Str("value")
				switch t.Str("type") {
				case "path":
					if path != "" && GlobMatch(v, path) {
						score += scorePath
					}
				case "cmd":
					if cmd != "" && (cmd == v || strings.HasPrefix(cmd, v+" ")) {
						score += scoreCmd
					}
				case "topic":
					if topic != "" && v == topic {
						score += scoreTopic
					}
				}
			}
			if textHit[l.Int("id")] {
				score += scoreText
			}
			if score == 0 {
				continue
			}
		}
		hits = append(hits, scored{l, score, rank(l)})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		return a.r.Int("id") > b.r.Int("id")
	})
	total = len(hits)
	cache := map[string]string{}
	cutoff := s.staleCutoff()
	out = []*row.Row{}
	for _, h := range hits {
		if q.Limit > 0 && len(out) >= q.Limit {
			break
		}
		r := s.lessonOut(h.r, cache, cutoff)
		if filtered {
			r.Set("score", int64(h.score))
		}
		out = append(out, r)
	}
	return out, total, nil
}

// VerifyLesson records that a lesson still holds.
func (s *Store) VerifyLesson(id int64, note string) (r *row.Row, err error) {
	defer guard(&err)
	s.tx(func() {
		l := s.mustLesson(id)
		if l.Str("status") != "active" {
			fail(errs.Conflict("lesson %d is %s; add a new one instead", id, l.Str("status")))
		}
		s.exec("UPDATE lessons SET verified_at = ?, verified_by = ? WHERE id = ?", refs.NowISO(), nullable(s.Actor), id)
		s.lessonEvent(id, "verify", note)
	})
	return s.lessonView(id), nil
}

// RetireLesson takes a lesson out of the lists; it stays readable by id.
func (s *Store) RetireLesson(id int64, note string) (r *row.Row, err error) {
	defer guard(&err)
	s.tx(func() {
		l := s.mustLesson(id)
		if l.Str("status") == "retired" {
			fail(errs.Conflict("lesson %d is already retired", id))
		}
		s.exec("UPDATE lessons SET status = 'retired' WHERE id = ?", id)
		s.lessonEvent(id, "retire", note)
	})
	return s.lessonView(id), nil
}

// LessonAmend is a partial update; nil and empty fields are left alone.
type LessonAmend struct {
	Title, Body                  *string
	Kind                         string
	AddPaths, AddCmds, AddTopics []string
}

// AmendLesson edits a lesson in place and adds triggers.
func (s *Store) AmendLesson(id int64, a LessonAmend) (r *row.Row, err error) {
	defer guard(&err)
	s.tx(func() {
		s.mustLesson(id)
		var changed []string
		if a.Title != nil {
			t := strings.TrimSpace(*a.Title)
			if t == "" {
				fail(errs.Usage("a lesson needs a title"))
			}
			s.exec("UPDATE lessons SET title = ? WHERE id = ?", t, id)
			changed = append(changed, "title")
		}
		if a.Body != nil {
			s.exec("UPDATE lessons SET body = ? WHERE id = ?", strings.TrimSpace(*a.Body), id)
			changed = append(changed, "body")
		}
		if a.Kind != "" {
			if !oneOf(a.Kind, LessonKinds) {
				fail(errs.Usage("invalid lesson kind: %q (%s)", a.Kind, strings.Join(LessonKinds, ", ")))
			}
			s.exec("UPDATE lessons SET kind = ? WHERE id = ?", a.Kind, id)
			changed = append(changed, "kind")
		}
		for _, t := range []struct {
			typ  string
			vals []string
		}{{"path", a.AddPaths}, {"cmd", a.AddCmds}, {"topic", a.AddTopics}} {
			if len(t.vals) > 0 {
				s.addTriggers(id, t.typ, t.vals)
				changed = append(changed, t.typ)
			}
		}
		if len(changed) == 0 {
			fail(errs.Usage("nothing to amend: give --title, --body, --kind, --add-path, --add-cmd or --add-topic"))
		}
		s.lessonEvent(id, "amend", strings.Join(changed, ","))
	})
	return s.lessonView(id), nil
}

// ResumeLessons lists active lessons for a task or project, titles only,
// narrowest scope first, newest first within a scope. total is before limit.
func (s *Store) ResumeLessons(projectID, taskID int64, limit int) (out []*row.Row, total int, err error) {
	defer guard(&err)
	where, args, rank := ladderSQL(s.ladder(Point{TaskID: taskID, ProjectID: projectID}))
	rows := s.q("SELECT id, title, kind, scope_type, scope_id, verified_at FROM lessons WHERE "+where+" AND status = 'active'", args...)
	sort.SliceStable(rows, func(i, j int) bool {
		if ri, rj := rank(rows[i]), rank(rows[j]); ri != rj {
			return ri < rj
		}
		return rows[i].Int("id") > rows[j].Int("id")
	})
	total = len(rows)
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return append([]*row.Row{}, rows...), total, nil
}
