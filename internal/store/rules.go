package store

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/row"
)

// Standing rules (agent-features spec H1, H3, H5). A rule lives at a scope
// (task, project, context or global) and binds everything under it. The
// effective set for a point is resolved narrowest first: task → project →
// parent project → context → global.

// RuleKinds, RuleVerbs and RuleChecks are the fixed vocabularies.
var (
	RuleKinds  = []string{"deny", "allow", "require", "remember"}
	RuleVerbs  = []string{"commit", "push", "pr", "merge", "release", "write", "install", "migrate", "delete", "deploy", "external-post", "config"}
	RuleChecks = []string{"branch-not-base", "browser-check", "tests-pass", "ask-first"}
	// RuleWhereKeys are the --where conditions a rule may carry.
	RuleWhereKeys = []string{"branch", "base"}
)

// Check decisions.
const (
	DecisionAllow         = "allow"
	DecisionDeny          = "deny"
	DecisionNeedsApproval = "needs-approval"
)

// SessionQuiet is how long a --session rule's session must stay silent
// before the rule lapses (agtrack cannot see a session end, only its quiet).
const SessionQuiet = 12 * time.Hour

// Scope names where a rule or lesson lives. Type is global, context,
// project or task; ID is 0 for global.
type Scope struct {
	Type string
	ID   int64
}

// Point is where rules and lessons are resolved from: a task, else a
// project, else a context. Global always applies; the zero Point is global
// only.
type Point struct {
	TaskID, ProjectID, ContextID int64
}

// PointOf is the point at a scope: the scope itself and everything wider.
func PointOf(sc Scope) Point {
	switch sc.Type {
	case "task":
		return Point{TaskID: sc.ID}
	case "project":
		return Point{ProjectID: sc.ID}
	case "context":
		return Point{ContextID: sc.ID}
	}
	return Point{}
}

type rung struct {
	typ  string
	id   int64
	rank int
}

// ladder lists the scopes that bind a point, narrowest first, with ranks
// (lower = narrower).
func (s *Store) ladder(p Point) []rung {
	var out []rung
	projectID, contextID := p.ProjectID, p.ContextID
	if p.TaskID != 0 {
		t := s.one("SELECT project_id FROM tasks WHERE id = ?", p.TaskID)
		if t == nil {
			fail(errs.NotFound("task", fmt.Sprint(p.TaskID), "no such task: %d", p.TaskID))
		}
		out = append(out, rung{"task", p.TaskID, 0})
		projectID = t.Int("project_id")
	}
	for id := projectID; id != 0; {
		pr := s.one("SELECT parent_id, context_id FROM projects WHERE id = ?", id)
		if pr == nil {
			break
		}
		out = append(out, rung{"project", id, len(out)})
		contextID = pr.Int("context_id")
		id = pr.Int("parent_id")
	}
	if contextID != 0 {
		out = append(out, rung{"context", contextID, len(out)})
	}
	return append(out, rung{"global", 0, len(out)})
}

// ladderSQL is the WHERE clause selecting rows at any rung, plus a rank
// lookup for the rows it returns.
func ladderSQL(l []rung) (where string, args []any, rank func(r *row.Row) int) {
	var parts []string
	ranks := map[string]int{}
	for _, r := range l {
		ranks[fmt.Sprintf("%s:%d", r.typ, r.id)] = r.rank
		if r.typ == "global" {
			parts = append(parts, "scope_type = 'global'")
			continue
		}
		parts = append(parts, "(scope_type = ? AND scope_id = ?)")
		args = append(args, r.typ, r.id)
	}
	rank = func(r *row.Row) int {
		id := r.Int("scope_id")
		if r.Str("scope_type") == "global" {
			id = 0
		}
		return ranks[fmt.Sprintf("%s:%d", r.Str("scope_type"), id)]
	}
	return "(" + strings.Join(parts, " OR ") + ")", args, rank
}

// mustScope checks a scope's target exists.
func (s *Store) mustScope(sc Scope) {
	switch sc.Type {
	case "global":
	case "context":
		if s.one("SELECT id FROM contexts WHERE id = ?", sc.ID) == nil {
			fail(errs.NotFound("context", fmt.Sprint(sc.ID), "no such context: %d", sc.ID))
		}
	case "project":
		if s.one("SELECT id FROM projects WHERE id = ?", sc.ID) == nil {
			fail(errs.NotFound("project", fmt.Sprint(sc.ID), "no such project: %d", sc.ID))
		}
	case "task":
		if s.one("SELECT id FROM tasks WHERE id = ?", sc.ID) == nil {
			fail(errs.NotFound("task", fmt.Sprint(sc.ID), "no such task: %d", sc.ID))
		}
	default:
		fail(errs.Usage("invalid scope: %q (global, context, project or task)", sc.Type))
	}
}

// scopeLabel names a scope for people: global, context work, work/orbiter:api, task 12.
func (s *Store) scopeLabel(typ string, id int64, cache map[string]string) string {
	key := fmt.Sprintf("%s:%d", typ, id)
	if v, ok := cache[key]; ok {
		return v
	}
	label := typ
	switch typ {
	case "context":
		if c := s.one("SELECT name FROM contexts WHERE id = ?", id); c != nil {
			label = "context " + c.Str("name")
		}
	case "project":
		if p := s.one("SELECT * FROM projects WHERE id = ?", id); p != nil {
			label = s.projectPath(p)
		}
	case "task":
		label = fmt.Sprintf("task %d", id)
	}
	cache[key] = label
	return label
}

// scopeEventIDs are the task and project an event about a scoped row is filed under.
func (s *Store) scopeEventIDs(typ string, id int64) (taskID, projectID int64) {
	switch typ {
	case "task":
		if t := s.one("SELECT project_id FROM tasks WHERE id = ?", id); t != nil {
			return id, t.Int("project_id")
		}
	case "project":
		return 0, id
	}
	return 0, 0
}

// --- similarity ----------------------------------------------------------------------

var tokenRE = regexp.MustCompile(`[\p{L}\p{N}]+`)

var stopwords = map[string]bool{
	"the": true, "an": true, "to": true, "of": true, "in": true, "on": true, "for": true, "and": true,
	"or": true, "is": true, "be": true, "it": true, "with": true, "this": true, "that": true, "at": true,
	"by": true, "as": true, "are": true, "do": true, "we": true, "you": true, "from": true,
}

// tokens is the normalised token set of a text: lowercased letter/digit
// runs, minus single characters and a short stopword list.
func tokens(text string) map[string]bool {
	out := map[string]bool{}
	for _, t := range tokenRE.FindAllString(strings.ToLower(text), -1) {
		if len([]rune(t)) < 2 || stopwords[t] {
			continue
		}
		out[t] = true
	}
	return out
}

// SimilarityThreshold is the token Jaccard at or above which two free texts
// count as the same rule or lesson.
const SimilarityThreshold = 0.6

// Jaccard is |A∩B| / |A∪B| over the normalised token sets of a and b
// (0 when both are empty).
func Jaccard(a, b string) float64 {
	ta, tb := tokens(a), tokens(b)
	if len(ta) == 0 && len(tb) == 0 {
		return 0
	}
	inter := 0
	for t := range ta {
		if tb[t] {
			inter++
		}
	}
	return float64(inter) / float64(len(ta)+len(tb)-inter)
}

// --- globs and repos -----------------------------------------------------------------

// GlobMatch matches a repo-relative path against a rule or lesson glob:
// `*` and `?` stay within a path segment, `**` crosses segments, a pattern
// without a slash also matches the base name, and a pattern without
// wildcards matches the path itself or anything under it.
func GlobMatch(pattern, p string) bool {
	pattern = strings.TrimPrefix(strings.TrimSpace(pattern), "./")
	p = strings.TrimPrefix(strings.TrimSpace(p), "./")
	if pattern == "" || p == "" {
		return false
	}
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	if !strings.ContainsAny(pattern, "*?") {
		return p == pattern || strings.HasPrefix(p, pattern+"/")
	}
	re := globRE(pattern)
	if re.MatchString(p) {
		return true
	}
	return !strings.Contains(pattern, "/") && re.MatchString(path.Base(p))
}

func globRE(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; {
		case strings.HasPrefix(pattern[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// repoKey normalises a repository reference for comparison ("" if unparsable).
func repoKey(v string) string {
	if strings.TrimSpace(v) == "" {
		return ""
	}
	id, err := refs.RepoIdentity(v)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(v))
	}
	return strings.ToLower(id)
}

// sameRepo compares two normalised repo identities; a side without a host
// matches on owner/name alone.
func sameRepo(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	tail := func(s string) string {
		parts := strings.Split(s, "/")
		if len(parts) > 2 {
			parts = parts[len(parts)-2:]
		}
		return strings.Join(parts, "/")
	}
	if strings.Count(a, "/") == 1 || strings.Count(b, "/") == 1 {
		return tail(a) == tail(b)
	}
	return false
}

func oneOf(v string, set []string) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

// --- rules: writes ------------------------------------------------------------------

// RuleInput are the inputs of AddRule.
type RuleInput struct {
	Kind  string // deny, allow, require, remember
	Verb  string // a RuleVerbs entry; required for deny and allow
	Check string // a RuleChecks entry; required for require
	Text  string // free text; required for remember
	Scope Scope

	PathGlob, Repo string
	Where          map[string]string
	Hard           bool
	Overridable    bool

	Until           string // ISO expiry, already parsed
	UntilTaskClosed bool
	Session         string // bind to this session ("" = none)

	Quote    string // the human's words; required for deny, allow and hard
	StatedBy string

	Supersedes int64
	Force      bool // skip the duplicate check
	Propose    bool // always store as proposed
}

const ruleCols = `id, uid, kind, verb, check_name, text, scope_type, scope_id, path_glob, repo, where_json, hard, overridable,
 status, expires_at, until_task_closed, session, source, stated_by, stated_at, quote, supersedes, created_at, actor`

func (s *Store) validateRule(in *RuleInput) {
	if !oneOf(in.Kind, RuleKinds) {
		fail(errs.Usage("invalid rule kind: %q (deny, allow, require or remember)", in.Kind))
	}
	if in.Verb != "" && !oneOf(in.Verb, RuleVerbs) {
		fail(errs.Usage("invalid verb: %q (one of %s)", in.Verb, strings.Join(RuleVerbs, ", ")))
	}
	switch in.Kind {
	case "deny", "allow":
		if in.Verb == "" {
			fail(errs.Usage("a %s rule names a verb: %s", in.Kind, strings.Join(RuleVerbs, ", ")))
		}
		if in.Check != "" {
			fail(errs.Usage("only a require rule names a check"))
		}
	case "require":
		if !oneOf(in.Check, RuleChecks) {
			fail(errs.Usage("a require rule names a check: %s", strings.Join(RuleChecks, ", ")))
		}
	case "remember":
		if strings.TrimSpace(in.Text) == "" {
			fail(errs.Usage("a remember rule needs its text"))
		}
		if in.Check != "" {
			fail(errs.Usage("only a require rule names a check"))
		}
	}
	if in.Hard && in.Kind != "deny" && in.Kind != "require" {
		fail(errs.Usage("only deny and require rules can be hard"))
	}
	if in.Overridable && in.Kind != "deny" {
		fail(errs.Usage("only a deny rule is overridable"))
	}
	if in.Overridable && in.Hard {
		fail(errs.Usage("a hard rule cannot be overridable"))
	}
	if strings.TrimSpace(in.Quote) == "" && (in.Kind == "deny" || in.Kind == "allow" || in.Hard) {
		fail(errs.Usage("--quote is required for deny, allow and hard rules: the human's own words"))
	}
	n := 0
	for _, b := range []bool{in.Until != "", in.UntilTaskClosed, in.Session != ""} {
		if b {
			n++
		}
	}
	if n > 1 {
		fail(errs.Usage("give one expiry: --until, --until-task-closed or --for-session"))
	}
	if in.UntilTaskClosed && in.Scope.Type != "task" {
		fail(errs.Usage("--until-task-closed needs a task scope (-t ID)"))
	}
	for k, v := range in.Where {
		if !oneOf(k, RuleWhereKeys) {
			fail(errs.Usage("unknown --where key %q (known: %s)", k, strings.Join(RuleWhereKeys, ", ")))
		}
		if k == "base" && v != "true" && v != "false" {
			fail(errs.Usage("--where base= takes true or false"))
		}
	}
	if in.Repo != "" {
		in.Repo = repoKey(in.Repo)
	}
	s.mustScope(in.Scope)
}

// similarRule reports whether an existing rule says the same as the input:
// same kind and, for rules naming a verb or check, the same verb, check,
// path and repo; for free-text rules a token Jaccard ≥ SimilarityThreshold.
func similarRule(r *row.Row, in *RuleInput) bool {
	if r.Str("kind") != in.Kind {
		return false
	}
	if in.Verb != "" || in.Check != "" || r.Str("verb") != "" || r.Str("check_name") != "" {
		return r.Str("verb") == in.Verb && r.Str("check_name") == in.Check &&
			r.Str("path_glob") == in.PathGlob && r.Str("repo") == in.Repo
	}
	return Jaccard(r.Str("text"), in.Text) >= SimilarityThreshold
}

// AddRule records a standing rule. An allow or hard rule from an actor who
// is not an approver (D3) is stored as proposed, as is everything when
// in.Propose is set. A similar active or proposed rule at the same or a
// wider scope refuses the add unless in.Force or in.Supersedes names it.
func (s *Store) AddRule(in RuleInput) (r *row.Row, err error) {
	defer guard(&err)
	var id int64
	s.tx(func() {
		s.validateRule(&in)
		if in.Supersedes != 0 {
			if s.one("SELECT id FROM rules WHERE id = ?", in.Supersedes) == nil {
				fail(errs.NotFound("rule", fmt.Sprint(in.Supersedes), "no such rule: %d", in.Supersedes))
			}
		}
		if !in.Force {
			where, args, _ := ladderSQL(s.ladder(PointOf(in.Scope)))
			cache := map[string]string{}
			for _, old := range s.q("SELECT "+ruleCols+" FROM rules WHERE "+where+" AND status IN ('active','proposed') ORDER BY id", args...) {
				if old.Int("id") == in.Supersedes || s.ruleLapsed(old, refs.NowISO()) {
					continue
				}
				if similarRule(old, &in) {
					e := errs.Conflict("similar rule #%d exists (%s %s, %s, %s); use --supersedes %d or --force",
						old.Int("id"), old.Str("kind"), RuleSummary(old), s.scopeLabel(old.Str("scope_type"), old.Int("scope_id"), cache),
						old.Str("status"), old.Int("id"))
					e.Extra = map[string]any{"similar": old.Int("id")}
					fail(e)
				}
			}
		}
		approver := s.IsApprover(s.Actor)
		status := "active"
		if in.Propose || ((in.Kind == "allow" || in.Hard) && !approver) {
			status = "proposed"
		}
		source := "agent"
		if approver || in.Quote != "" {
			source = "user"
		}
		statedBy := in.StatedBy
		if statedBy == "" && approver {
			statedBy = s.Actor
		}
		statedAt := ""
		if in.Quote != "" {
			statedAt = refs.NowISO()
		}
		var where any
		if len(in.Where) > 0 {
			b, _ := json.Marshal(in.Where)
			where = string(b)
		}
		var scopeID any
		if in.Scope.Type != "global" {
			scopeID = in.Scope.ID
		}
		now := refs.NowISO()
		id = s.insert(`INSERT INTO rules (uid, kind, verb, check_name, text, scope_type, scope_id, path_glob, repo, where_json,
 hard, overridable, status, expires_at, until_task_closed, session, source, stated_by, stated_at, quote, supersedes, created_at, actor)
 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			NewUID(), in.Kind, nullable(in.Verb), nullable(in.Check), nullable(strings.TrimSpace(in.Text)), in.Scope.Type, scopeID,
			nullable(in.PathGlob), nullable(in.Repo), where, b2i(in.Hard), b2i(in.Overridable), status, nullable(in.Until),
			b2i(in.UntilTaskClosed), nullable(in.Session), source, nullable(statedBy), nullable(statedAt), nullable(strings.TrimSpace(in.Quote)),
			nullableID(in.Supersedes), now, nullable(s.Actor))
		action := "add"
		if status == "proposed" {
			action = "propose"
		}
		s.ruleEvent(id, action, "")
		if status == "active" {
			s.supersede(id, in.Supersedes)
		}
	})
	return s.ruleView(id, true), nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ruleEvent appends to the rule's history and to the worklog.
func (s *Store) ruleEvent(id int64, action, note string) {
	s.exec("INSERT INTO rule_events (rule_id, action, actor, at, note) VALUES (?,?,?,?,?)", id, action, nullable(s.Actor), refs.NowISO(), nullable(note))
	r := s.one("SELECT "+ruleCols+" FROM rules WHERE id = ?", id)
	taskID, projectID := s.scopeEventIDs(r.Str("scope_type"), r.Int("scope_id"))
	detail := r.Str("kind") + " " + RuleSummary(r)
	if note != "" {
		detail += ": " + note
	}
	s.event("rule", id, "rule-"+action, detail, taskID, projectID, "")
}

// supersede revokes the rule a newly active rule replaces.
func (s *Store) supersede(newID, oldID int64) {
	if oldID == 0 {
		return
	}
	res := s.exec("UPDATE rules SET status = 'revoked' WHERE id = ? AND status IN ('active','proposed')", oldID)
	if n, _ := res.RowsAffected(); n > 0 {
		s.ruleEvent(oldID, "revoke", fmt.Sprintf("superseded by rule %d", newID))
	}
}

func (s *Store) mustRule(id int64) *row.Row {
	r := s.one("SELECT "+ruleCols+" FROM rules WHERE id = ?", id)
	if r == nil {
		fail(errs.NotFound("rule", fmt.Sprint(id), "no such rule: %d", id))
	}
	return r
}

func (s *Store) approverOnly(what string) {
	if s.IsApprover(s.Actor) {
		return
	}
	list, _ := s.Config("rules.approvers", DefaultApprovers)
	who := s.Actor
	if who == "" {
		who = "an unnamed actor"
	}
	fail(errs.New("only an approver may %s (config rules.approvers: %s; you are %s); ask the human to do it here or in `agtrack serve`", what, list, who))
}

// ApproveRule makes a proposed rule active. Approvers only (D3).
func (s *Store) ApproveRule(id int64, note string) (r *row.Row, err error) {
	defer guard(&err)
	s.tx(func() {
		old := s.mustRule(id)
		s.approverOnly("approve a rule")
		if old.Str("status") != "proposed" {
			fail(errs.Conflict("rule %d is %s, not proposed", id, old.Str("status")))
		}
		s.exec("UPDATE rules SET status = 'active', stated_by = COALESCE(stated_by, ?) WHERE id = ?", nullable(s.Actor), id)
		s.ruleEvent(id, "approve", note)
		s.supersede(id, old.Int("supersedes"))
	})
	return s.ruleView(id, true), nil
}

// RevokeRule ends a rule; it stays in history. Revoking an active hard rule
// is for approvers only; anyone may revoke other rules or withdraw a proposal.
func (s *Store) RevokeRule(id int64, note string) (r *row.Row, err error) {
	defer guard(&err)
	s.tx(func() {
		old := s.mustRule(id)
		st := old.Str("status")
		if st != "active" && st != "proposed" {
			fail(errs.Conflict("rule %d is already %s", id, st))
		}
		if old.Int("hard") == 1 && st == "active" {
			s.approverOnly("revoke a hard rule")
		}
		s.exec("UPDATE rules SET status = 'revoked' WHERE id = ?", id)
		s.ruleEvent(id, "revoke", note)
	})
	return s.ruleView(id, true), nil
}

// --- rules: reads -------------------------------------------------------------------

// RuleSummary is a rule's one-line text: verb or check, then its text.
func RuleSummary(r *row.Row) string {
	var parts []string
	for _, k := range []string{"verb", "check_name", "text"} {
		if v := r.Str(k); v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, " ")
}

// ruleLapsed reports whether an active rule's expiry has passed: its date,
// its task closed, or its session quiet for SessionQuiet.
func (s *Store) ruleLapsed(r *row.Row, now string) bool {
	if e := r.Str("expires_at"); e != "" && e <= now {
		return true
	}
	if r.Int("until_task_closed") == 1 && r.Str("scope_type") == "task" {
		if t := s.one("SELECT status FROM tasks WHERE id = ?", r.Int("scope_id")); t == nil || Closed(t.Str("status")) {
			return true
		}
	}
	if sess := r.Str("session"); sess != "" {
		cutoff := refs.ToUTCISO(refs.Now().Add(-SessionQuiet))
		if r.Str("created_at") < cutoff {
			last := s.one("SELECT MAX(COALESCE(recorded_at, at)) AS t FROM events WHERE session = ?", sess)
			if last == nil || last.Str("t") < cutoff {
				return true
			}
		}
	}
	return false
}

// markExpired lazily records lapsed rules as expired. It is best effort: a
// read must not fail because the write did (a read-only copy, a busy lock).
func (s *Store) markExpired(ids []int64) {
	if len(ids) == 0 {
		return
	}
	_ = s.DB.Tx(func() (err error) {
		defer guard(&err)
		for _, id := range ids {
			res := s.exec("UPDATE rules SET status = 'expired' WHERE id = ? AND status = 'active'", id)
			if n, _ := res.RowsAffected(); n > 0 {
				s.ruleEvent(id, "expire", "")
			}
		}
		return nil
	})
}

// scopedRules loads the rules binding a point with the given statuses,
// dropping (and lazily expiring) lapsed active ones and, unless
// allSessions, session rules of other sessions. Each row gains "rank".
func (s *Store) scopedRules(p Point, statuses []string, allSessions bool) []*row.Row {
	where, args, rank := ladderSQL(s.ladder(p))
	q := "SELECT " + ruleCols + " FROM rules WHERE " + where
	if len(statuses) > 0 {
		q += " AND status IN (" + marks(len(statuses)) + ")"
		args = append(args, strAny(statuses)...)
	}
	now := refs.NowISO()
	var out []*row.Row
	var lapsed []int64
	for _, r := range s.q(q, args...) {
		if r.Str("status") == "active" && s.ruleLapsed(r, now) {
			lapsed = append(lapsed, r.Int("id"))
			if !oneOf("expired", statuses) {
				continue
			}
			r.Set("status", "expired")
		}
		if !allSessions && r.Str("session") != "" && r.Str("session") != s.Session && oneOf(r.Str("status"), []string{"active", "proposed"}) {
			continue
		}
		r.Set("rank", int64(rank(r)))
		out = append(out, r)
	}
	s.markExpired(lapsed)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Int("rank") != b.Int("rank") {
			return a.Int("rank") < b.Int("rank")
		}
		if a.Int("hard") != b.Int("hard") {
			return a.Int("hard") > b.Int("hard")
		}
		return a.Int("id") < b.Int("id")
	})
	return out
}

// ruleOut is the public shape of a rule row.
func (s *Store) ruleOut(r *row.Row, cache map[string]string) *row.Row {
	var where any
	if w := r.Str("where_json"); w != "" {
		m := map[string]string{}
		if json.Unmarshal([]byte(w), &m) == nil {
			where = m
		}
	}
	var scopeID any
	if r.Str("scope_type") != "global" {
		scopeID = r.Int("scope_id")
	}
	expires := r.Get("expires_at")
	if r.Int("until_task_closed") == 1 {
		expires = "task closed"
	} else if r.Str("session") != "" {
		expires = "session " + r.Str("session")
	}
	return row.Of("id", r.Int("id"), "uid", r.Str("uid"), "kind", r.Str("kind"), "verb", r.Get("verb"), "check", r.Get("check_name"),
		"text", r.Get("text"), "rule", RuleSummary(r), "scope", s.scopeLabel(r.Str("scope_type"), r.Int("scope_id"), cache),
		"scope_type", r.Str("scope_type"), "scope_id", scopeID, "status", r.Str("status"),
		"hard", r.Int("hard") == 1, "overridable", r.Int("overridable") == 1,
		"path", r.Get("path_glob"), "repo", r.Get("repo"), "where", where, "expires", expires,
		"source", r.Str("source"), "stated_by", r.Get("stated_by"), "stated_at", r.Get("stated_at"), "quote", r.Get("quote"),
		"supersedes", r.Get("supersedes"), "actor", r.Get("actor"), "created_at", r.Str("created_at"))
}

func (s *Store) ruleView(id int64, withEvents bool) *row.Row {
	out := s.ruleOut(s.mustRule(id), map[string]string{})
	if withEvents {
		out.Set("events", s.q("SELECT action, actor, at, note FROM rule_events WHERE rule_id = ? ORDER BY id", id))
	}
	return out
}

// GetRule returns a rule with its history (events: action, actor, at, note).
func (s *Store) GetRule(id int64) (r *row.Row, err error) {
	defer guard(&err)
	return s.ruleView(id, true), nil
}

// RuleListOpts filter ListRules. The default is active rules only.
type RuleListOpts struct {
	All      bool // every status, revoked and expired included
	Proposed bool // proposed rules only
}

// ListRules lists the rules that bind a point, narrowest scope first
// (task, project, parent project, context, global), hard rules first within
// a scope. Rules bound to another session are left out of the default view.
func (s *Store) ListRules(p Point, opts RuleListOpts) (out []*row.Row, err error) {
	defer guard(&err)
	statuses := []string{"active"}
	switch {
	case opts.All:
		statuses = []string{"active", "proposed", "revoked", "expired"}
	case opts.Proposed:
		statuses = []string{"proposed"}
	}
	cache := map[string]string{}
	out = []*row.Row{}
	for _, r := range s.scopedRules(p, statuses, opts.All) {
		out = append(out, s.ruleOut(r, cache))
	}
	return out, nil
}

// CountProposedRules is the number of rules waiting for an approver,
// machine-wide (the browse app's top-bar badge).
func (s *Store) CountProposedRules() (n int, err error) {
	defer guard(&err)
	return int(s.count("SELECT COUNT(*) FROM rules WHERE status = 'proposed'")), nil
}

// --- check --------------------------------------------------------------------------

// CheckInput is what `check` resolves rules against.
type CheckInput struct {
	Verb   string
	Point  Point
	Repo   string   // the action's repository ("" = none known)
	Branch string   // the current branch ("" = none known)
	Paths  []string // repo-relative paths the action touches
	// IsBase decides branch-not-base; nil = IsBaseBranch in the cwd.
	IsBase func(branch string) bool
}

// ruleApplies reports whether a rule's conditions hold for the action.
func ruleApplies(r *row.Row, in *CheckInput, repo string, isBase func(string) bool) bool {
	if v := r.Str("verb"); v != "" && v != in.Verb {
		return false
	}
	if v := r.Str("verb"); v == "" && (r.Str("kind") == "deny" || r.Str("kind") == "allow" || r.Str("kind") == "remember") {
		return false // only verb-less require rules apply to every verb
	}
	if g := r.Str("path_glob"); g != "" {
		hit := false
		for _, p := range in.Paths {
			hit = hit || GlobMatch(g, p)
		}
		if !hit {
			return false
		}
	}
	if rr := r.Str("repo"); rr != "" && !sameRepo(rr, repo) {
		return false
	}
	if w := r.Str("where_json"); w != "" {
		m := map[string]string{}
		_ = json.Unmarshal([]byte(w), &m)
		if b, ok := m["branch"]; ok && (in.Branch == "" || !(b == in.Branch || GlobMatch(b, in.Branch))) {
			return false
		}
		if b, ok := m["base"]; ok && (in.Branch == "" || isBase(in.Branch) != (b == "true")) {
			return false
		}
	}
	return true
}

// Check resolves the effective rule set for an action and decides it:
//
//   - deny and require rules accumulate across scopes;
//   - an active allow for the verb cancels a non-hard, overridable deny at
//     the same or a wider scope than the allow (rank ≥ the allow's);
//   - a proposed allow that would cancel it makes the decision
//     needs-approval instead of deny, as does a require ask-first;
//   - a require branch-not-base fails like a deny when the branch is a base
//     branch; browser-check and tests-pass are the agent's own checks and
//     are reported, not decided;
//   - remember rules naming the verb are reported.
//
// The result carries action, decision, hard, matched, allowed_by and hint.
func (s *Store) Check(in CheckInput) (res *row.Row, err error) {
	defer guard(&err)
	if !oneOf(in.Verb, RuleVerbs) {
		fail(errs.Usage("invalid verb: %q (one of %s)", in.Verb, strings.Join(RuleVerbs, ", ")))
	}
	isBase := in.IsBase
	if isBase == nil {
		isBase = func(b string) bool { return IsBaseBranch("", b) }
	}
	repo := repoKey(in.Repo)
	var active, proposedAllows []*row.Row
	for _, r := range s.scopedRules(in.Point, []string{"active", "proposed"}, false) {
		if !ruleApplies(r, &in, repo, isBase) {
			continue
		}
		if r.Str("status") == "proposed" {
			if r.Str("kind") == "allow" {
				proposedAllows = append(proposedAllows, r)
			}
			continue
		}
		active = append(active, r)
	}
	cache := map[string]string{}
	brief := func(r *row.Row, effect string) *row.Row {
		return row.Of("id", r.Int("id"), "kind", r.Str("kind"), "rule", RuleSummary(r),
			"scope", s.scopeLabel(r.Str("scope_type"), r.Int("scope_id"), cache), "hard", r.Int("hard") == 1,
			"quote", r.Get("quote"), "effect", effect)
	}
	cancels := func(allow, deny *row.Row) bool { return allow.Int("rank") <= deny.Int("rank") }
	matched := []*row.Row{}
	allowedBy := []*row.Row{}
	seenAllow := map[int64]bool{}
	var hardDeny, deny, needsApproval bool
	var hints []string
	for _, r := range active {
		id := r.Int("id")
		switch r.Str("kind") {
		case "deny":
			if r.Int("hard") == 1 {
				hardDeny = true
				matched = append(matched, brief(r, "deny"))
				hints = append(hints, fmt.Sprintf("hard rule %d denies %s%s; only the human can lift it", id, in.Verb, quoted(r)))
				continue
			}
			var by, pending *row.Row
			if r.Int("overridable") == 1 {
				for _, a := range active {
					if a.Str("kind") == "allow" && cancels(a, r) {
						by = a
						break
					}
				}
				for _, a := range proposedAllows {
					if by == nil && pending == nil && cancels(a, r) {
						pending = a
					}
				}
			}
			switch {
			case by != nil:
				matched = append(matched, brief(r, "cancelled"))
				if !seenAllow[by.Int("id")] {
					seenAllow[by.Int("id")] = true
					allowedBy = append(allowedBy, brief(by, "allow"))
				}
			case pending != nil:
				needsApproval = true
				matched = append(matched, brief(r, "pending-approval"))
				hints = append(hints, fmt.Sprintf("rule %d denies %s until an approver approves allow rule %d (`agtrack rule approve %d`)", id, in.Verb, pending.Int("id"), pending.Int("id")))
			default:
				deny = true
				matched = append(matched, brief(r, "deny"))
				hints = append(hints, fmt.Sprintf("rule %d denies %s%s; do not proceed without the human", id, in.Verb, quoted(r)))
			}
		case "require":
			switch r.Str("check_name") {
			case "branch-not-base":
				switch {
				case in.Branch == "":
					matched = append(matched, brief(r, "unverified"))
					hints = append(hints, fmt.Sprintf("rule %d requires a non-base branch; no branch is known here (pass --branch)", id))
				case isBase(in.Branch):
					if r.Int("hard") == 1 {
						hardDeny = true
					} else {
						deny = true
					}
					matched = append(matched, brief(r, "failed"))
					hints = append(hints, fmt.Sprintf("rule %d: %s needs a working branch, and %s is a base branch; create one first", id, in.Verb, in.Branch))
				default:
					matched = append(matched, brief(r, "passed"))
				}
			case "ask-first":
				needsApproval = true
				matched = append(matched, brief(r, "ask-first"))
				hints = append(hints, fmt.Sprintf("rule %d: ask the human before you %s%s", id, in.Verb, quoted(r)))
			default: // browser-check, tests-pass: the agent's own check
				matched = append(matched, brief(r, "self-check"))
				hints = append(hints, fmt.Sprintf("rule %d: run %s before you %s", id, r.Str("check_name"), in.Verb))
			}
		case "remember":
			matched = append(matched, brief(r, "remember"))
		}
	}
	decision := DecisionAllow
	switch {
	case hardDeny || deny:
		decision = DecisionDeny
	case needsApproval:
		decision = DecisionNeedsApproval
	}
	var hint any
	if len(hints) > 0 {
		hint = strings.Join(hints, "; ")
	}
	return row.Of("action", in.Verb, "decision", decision, "hard", hardDeny, "matched", matched, "allowed_by", allowedBy, "hint", hint), nil
}

func quoted(r *row.Row) string {
	if q := r.Str("quote"); q != "" {
		return fmt.Sprintf(" (%q)", q)
	}
	return ""
}

// --- resume -------------------------------------------------------------------------

// ResumeRules lists the rules resume shows for a task or project (H4):
// active deny, require and hard rules, plus allow rules added in the last 7
// days, narrowest scope first. total is before limit (0 = no limit).
func (s *Store) ResumeRules(projectID, taskID int64, limit int) (out []*row.Row, total int, err error) {
	defer guard(&err)
	week := refs.ToUTCISO(refs.Now().Add(-7 * 24 * time.Hour))
	out = []*row.Row{}
	for _, r := range s.scopedRules(Point{TaskID: taskID, ProjectID: projectID}, []string{"active"}, false) {
		k := r.Str("kind")
		if !(k == "deny" || k == "require" || r.Int("hard") == 1 || (k == "allow" && r.Str("created_at") >= week)) {
			continue
		}
		total++
		if limit <= 0 || len(out) < limit {
			out = append(out, r)
		}
	}
	return out, total, nil
}
