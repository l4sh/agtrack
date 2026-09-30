// Package refs parses and normalises the strings users and agents type:
// context/project/task/subtask references, statuses, PR states, repository
// remotes and timestamps.
//
// Reference grammar:
//
//	context ref      personal
//	project ref      [context/]project[:subproject]      orbiter:frontend
//	task ref         12
//	subtask ref      47        (global id)
//	                 12.3      (task 12, subtask at position 3)
//
// Ported from the Python reference implementation (agtracklib/refs.py);
// error strings are copied verbatim where the brief requires parity.
package refs

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/l4sh/agtrack/internal/errs"
)

// Statuses is the canonical, ordered set of task/subtask statuses.
var Statuses = []string{"todo", "in-progress", "done", "cancelled"}

// PRStates is the canonical, ordered set of pull request states.
var PRStates = []string{"open", "draft", "merged", "closed"}

// OpenStatuses and ClosedStatuses partition Statuses for the "open"/"closed"
// filter shorthands.
var (
	OpenStatuses   = []string{"todo", "in-progress"}
	ClosedStatuses = []string{"done", "cancelled"}
)

// StatusAliases maps every accepted spelling (including the canonical
// statuses themselves) onto a canonical status.
var StatusAliases = map[string]string{
	"todo":    "todo",
	"t":       "todo",
	"open":    "todo",
	"pending": "todo",

	"in-progress": "in-progress",
	"in_progress": "in-progress",
	"inprogress":  "in-progress",
	"progress":    "in-progress",
	"doing":       "in-progress",
	"wip":         "in-progress",
	"started":     "in-progress",

	"done":      "done",
	"d":         "done",
	"complete":  "done",
	"completed": "done",
	"finished":  "done",

	"cancelled": "cancelled",
	"canceled":  "cancelled",
	"cancel":    "cancelled",
	"x":         "cancelled",
	"dropped":   "cancelled",
}

// StatusMark is the checkbox glyph rendered for each canonical status.
var StatusMark = map[string]string{
	"todo":        "[ ]",
	"in-progress": "[~]",
	"done":        "[x]",
	"cancelled":   "[-]",
}

var slugValidRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
var slugInvalidRE = regexp.MustCompile(`[^a-z0-9._-]+`)

// NormalizeStatus maps a status alias onto a canonical status.
func NormalizeStatus(value string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(value))
	if s, ok := StatusAliases[key]; ok {
		return s, nil
	}
	return "", errs.New("unknown status: %s (expected one of %s)", value, strings.Join(Statuses, ", "))
}

// ParseStatusFilter expands a comma separated status filter, including the
// open/closed/all sets, deduplicating while keeping first-seen order.
func ParseStatusFilter(value string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		switch part {
		case "all":
			out = append(out, Statuses...)
		case "open":
			out = append(out, OpenStatuses...)
		case "closed":
			out = append(out, ClosedStatuses...)
		default:
			s, err := NormalizeStatus(part)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
	}
	seen := make(map[string]bool, len(out))
	result := make([]string, 0, len(out))
	for _, s := range out {
		if seen[s] {
			continue
		}
		seen[s] = true
		result = append(result, s)
	}
	return result, nil
}

// NormalizePRState validates and lowercases a pull request state.
func NormalizePRState(value string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(value))
	for _, s := range PRStates {
		if s == key {
			return s, nil
		}
	}
	return "", errs.New("unknown pull request state: %s (expected one of %s)", value, strings.Join(PRStates, ", "))
}

// Slugify normalises a context/project/tag name into a lowercase slug.
func Slugify(value string) (string, error) {
	lowered := strings.ToLower(strings.TrimSpace(value))
	slug := slugInvalidRE.ReplaceAllString(lowered, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" || !slugValidRE.MatchString(slug) {
		return "", errs.New("invalid name: %s (use letters, digits, . _ -)", pyRepr(value))
	}
	return slug, nil
}

// ParseProjectRef splits `[context/]project[:subproject]` into its parts.
// ctx and sub are "" when absent.
func ParseProjectRef(ref string) (ctx, project, sub string, err error) {
	raw := strings.TrimSpace(ref)
	if raw == "" {
		return "", "", "", errs.New("empty project reference")
	}
	if idx := strings.IndexByte(raw, '/'); idx >= 0 {
		ctxPart := raw[:idx]
		raw = raw[idx+1:]
		ctx, err = Slugify(ctxPart)
		if err != nil {
			return "", "", "", err
		}
	}
	if strings.Count(raw, ":") > 1 {
		return "", "", "", errs.New("invalid project reference: %s (nesting is project:subproject only)", ref)
	}
	projectPart, subPart, _ := strings.Cut(raw, ":")
	if projectPart == "" {
		return "", "", "", errs.New("invalid project reference: %s", ref)
	}
	project, err = Slugify(projectPart)
	if err != nil {
		return "", "", "", err
	}
	if subPart != "" {
		sub, err = Slugify(subPart)
		if err != nil {
			return "", "", "", err
		}
	}
	return ctx, project, sub, nil
}

// ParseTaskRef accepts `12`, `#12` or `t12`.
func ParseTaskRef(ref string) (int64, error) {
	raw := strings.TrimSpace(ref)
	raw = strings.TrimLeft(raw, "#")
	if len(raw) > 0 && (raw[0] == 't' || raw[0] == 'T') && isAllDigitsASCII(raw[1:]) {
		raw = raw[1:]
	}
	if !isAllDigitsASCII(raw) {
		return 0, errs.New("invalid task reference: %s (expected a task id)", ref)
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, errs.New("invalid task reference: %s (expected a task id)", ref)
	}
	return n, nil
}

// ParseSubtaskRef returns (taskID, position) for `12.3`, or (0, id) for a
// global id, where taskID == 0 means "global id".
func ParseSubtaskRef(ref string) (taskID int64, n int64, err error) {
	raw := strings.TrimSpace(ref)
	raw = strings.TrimLeft(raw, "#")
	if idx := strings.IndexByte(raw, '.'); idx >= 0 {
		taskPart := strings.TrimLeft(raw[:idx], "t")
		posPart := raw[idx+1:]
		if !isAllDigitsASCII(taskPart) || !isAllDigitsASCII(posPart) {
			return 0, 0, errs.New("invalid subtask reference: %s", ref)
		}
		tid, errT := strconv.ParseInt(taskPart, 10, 64)
		pos, errP := strconv.ParseInt(posPart, 10, 64)
		if errT != nil || errP != nil {
			return 0, 0, errs.New("invalid subtask reference: %s", ref)
		}
		return tid, pos, nil
	}
	if len(raw) > 0 && (raw[0] == 's' || raw[0] == 'S') && isAllDigitsASCII(raw[1:]) {
		raw = raw[1:]
	}
	if !isAllDigitsASCII(raw) {
		return 0, 0, errs.New("invalid subtask reference: %s (expected <id> or <task>.<position>)", ref)
	}
	id, err2 := strconv.ParseInt(raw, 10, 64)
	if err2 != nil {
		return 0, 0, errs.New("invalid subtask reference: %s (expected <id> or <task>.<position>)", ref)
	}
	return 0, id, nil
}

// isAllDigitsASCII reports whether s is non-empty and every byte is an ASCII
// digit. Python's str.isdigit() accepts Unicode digits; this port is
// ASCII-only, which is sufficient for realistic refs.
func isAllDigitsASCII(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// _REPO_PATTERNS, ported verbatim (order matters: first match wins).
var repoPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^(?:https?://|git://)(?P<host>[^/]+)/(?:[^/]+/)*?(?P<owner>[^/]+)/(?P<name>[^/]+?)(?:\.git)?/?$`),
	regexp.MustCompile(`^(?:ssh://)?[^@/]+@(?P<host>[^:/]+)[:/](?P<owner>[^/]+)/(?P<name>[^/]+?)(?:\.git)?/?$`),
	regexp.MustCompile(`^(?P<host>[A-Za-z0-9.-]+\.[A-Za-z]{2,})/(?P<owner>[^/]+)/(?P<name>[^/]+?)(?:\.git)?/?$`),
	regexp.MustCompile(`^(?P<owner>[A-Za-z0-9._-]+)/(?P<name>[A-Za-z0-9._-]+?)(?:\.git)?$`),
}

// matchRepo tries every pattern in order and returns the lowercased
// host (if the pattern carries one), owner and name of the first match.
func matchRepo(raw string) (host, owner, name string, ok bool) {
	for _, p := range repoPatterns {
		m := p.FindStringSubmatch(raw)
		if m == nil {
			continue
		}
		owner = strings.ToLower(m[p.SubexpIndex("owner")])
		name = strings.ToLower(m[p.SubexpIndex("name")])
		if hi := p.SubexpIndex("host"); hi != -1 {
			host = strings.ToLower(m[hi])
		}
		return host, owner, name, true
	}
	return "", "", "", false
}

// NormalizeRepo reduces any git remote form to a lowercase `owner/name` slug.
func NormalizeRepo(value string) (string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", errs.New("empty repository reference")
	}
	if _, owner, name, ok := matchRepo(raw); ok {
		return owner + "/" + name, nil
	}
	return "", errs.New("cannot parse repository reference: %s", value)
}

// RepoHost returns the lowercase host embedded in a repo reference, or ""
// when the form carries none (a bare `owner/name`, or no match at all).
func RepoHost(value string) string {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return ""
	}
	host, _, _, ok := matchRepo(raw)
	if !ok {
		return ""
	}
	return host
}

// RepoIdentity returns "host/owner/name" when the reference carries a known
// host (D8: disambiguates github.com/x/y from gitlab.com/x/y), else
// "owner/name".
func RepoIdentity(value string) (string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", errs.New("empty repository reference")
	}
	host, owner, name, ok := matchRepo(raw)
	if !ok {
		return "", errs.New("cannot parse repository reference: %s", value)
	}
	if host != "" {
		return host + "/" + owner + "/" + name, nil
	}
	return owner + "/" + name, nil
}

var prURLRE = regexp.MustCompile(`^https?://[^/]+/(?P<owner>[^/]+)/(?P<name>[^/]+)/pulls?/(?P<number>\d+)`)

// ParsePRURL pulls `owner/name` and the PR number out of a GitHub-style pull
// request URL. ok is false when value does not look like one.
func ParsePRURL(value string) (repo string, number int64, ok bool) {
	m := prURLRE.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil {
		return "", 0, false
	}
	owner := strings.ToLower(m[prURLRE.SubexpIndex("owner")])
	name := strings.ToLower(m[prURLRE.SubexpIndex("name")])
	n, err := strconv.ParseInt(m[prURLRE.SubexpIndex("number")], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return owner + "/" + name, n, true
}

// --- time --------------------------------------------------------------

var durationRE = regexp.MustCompile(`^(\d+)([smhdw])$`)

// isoRE parses the ISO forms fmt_ts/parse_since accept, after "Z" has been
// replaced with "+00:00": YYYY-MM-DD, YYYY-MM-DDTHH:MM, YYYY-MM-DD HH:MM:SS,
// with optional fractional seconds and a +HH:MM offset.
var isoRE = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})(?:[T ](\d{2}):(\d{2})(?::(\d{2})(?:\.(\d+))?)?)?([+-]\d{2}:\d{2})?$`)

// Now returns the current instant in UTC. It honours AGTRACK_NOW (RFC3339,
// which subsumes YYYY-MM-DDTHH:MM:SSZ) so tests can pin the clock.
func Now() time.Time {
	if v := strings.TrimSpace(os.Getenv("AGTRACK_NOW")); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t.UTC()
		}
	}
	return time.Now().UTC()
}

// NowISO is Now formatted as a stored timestamp.
func NowISO() string { return ToUTCISO(Now()) }

// ToUTCISO renders t as the stored timestamp form: UTC, second precision,
// `Z` suffix, e.g. 2026-09-29T02:22:08Z.
func ToUTCISO(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05Z")
}

func localMidnight(now time.Time) time.Time {
	l := now.In(time.Local)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.Local)
}

func durationFor(unit string, n int) time.Duration {
	switch unit {
	case "s":
		return time.Duration(n) * time.Second
	case "m":
		return time.Duration(n) * time.Minute
	case "h":
		return time.Duration(n) * time.Hour
	case "d":
		return time.Duration(n) * 24 * time.Hour
	case "w":
		return time.Duration(n) * 7 * 24 * time.Hour
	default:
		return 0
	}
}

// ParseSince turns `2d`, `36h`, `today`, `yesterday` or an ISO date into a
// UTC ISO bound.
func ParseSince(value string) (string, error) {
	raw := strings.ToLower(strings.TrimSpace(value))
	if raw == "today" || raw == "yesterday" {
		midnight := localMidnight(Now())
		if raw == "yesterday" {
			midnight = midnight.Add(-24 * time.Hour)
		}
		return ToUTCISO(midnight), nil
	}
	if m := durationRE.FindStringSubmatch(raw); m != nil {
		n, _ := strconv.Atoi(m[1])
		return ToUTCISO(Now().Add(-durationFor(m[2], n))), nil
	}
	trimmed := strings.ReplaceAll(strings.TrimSpace(value), "Z", "+00:00")
	t, ok := parseISO(trimmed)
	if !ok {
		return "", errs.New("cannot parse time: %s (use 2d, 36h, today, yesterday or a date)", value)
	}
	return ToUTCISO(t), nil
}

// ParseWhen parses a timestamp that will be written, not just filtered on.
// Same grammar as ParseSince, but refuses (with a 5 minute grace) a
// timestamp in the future.
func ParseWhen(value, label string) (string, error) {
	parsed, err := ParseSince(value)
	if err != nil {
		return "", err
	}
	future := ToUTCISO(Now().Add(5 * time.Minute))
	if parsed > future {
		return "", errs.New("%s is in the future: %s. agtrack records work that happened, not work that is planned", label, parsed)
	}
	return parsed, nil
}

// FmtTS renders a stored timestamp for humans; local time unless utc is set.
// Unparseable input is returned unchanged; "" stays "".
func FmtTS(value string, utc, dateOnly bool) string {
	if value == "" {
		return ""
	}
	t, ok := parseISO(strings.ReplaceAll(value, "Z", "+00:00"))
	if !ok {
		return value
	}
	if !utc {
		t = t.In(time.Local)
	}
	if dateOnly {
		return t.Format("2006-01-02")
	}
	return t.Format("2006-01-02 15:04")
}

// parseISO parses the Python datetime.fromisoformat subset described by
// isoRE. A naive value (no offset) is tagged with time.Local without
// shifting its wall clock, mirroring Python's astimezone() on a naive
// datetime. Invalid calendar values (Feb 30, hour 24, ...) are rejected via
// a round-trip check, since time.Date silently normalises overflow.
func parseISO(s string) (time.Time, bool) {
	m := isoRE.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false
	}
	year, _ := strconv.Atoi(m[1])
	month, _ := strconv.Atoi(m[2])
	day, _ := strconv.Atoi(m[3])
	hour, minute, sec := 0, 0, 0
	if m[4] != "" {
		hour, _ = strconv.Atoi(m[4])
		minute, _ = strconv.Atoi(m[5])
		if m[6] != "" {
			sec, _ = strconv.Atoi(m[6])
		}
	}
	nsec := 0
	if m[7] != "" {
		frac := m[7]
		if len(frac) > 9 {
			frac = frac[:9]
		}
		for len(frac) < 9 {
			frac += "0"
		}
		nsec, _ = strconv.Atoi(frac)
	}
	loc := time.Local
	if m[8] != "" {
		off := m[8]
		sign := 1
		if off[0] == '-' {
			sign = -1
		}
		hh, _ := strconv.Atoi(off[1:3])
		mm, _ := strconv.Atoi(off[4:6])
		loc = time.FixedZone("", sign*(hh*3600+mm*60))
	}
	t := time.Date(year, time.Month(month), day, hour, minute, sec, nsec, loc)
	if t.Year() != year || int(t.Month()) != month || t.Day() != day ||
		t.Hour() != hour || t.Minute() != minute || t.Second() != sec {
		return time.Time{}, false
	}
	return t, true
}

// pyRepr renders s the way Python's repr() would for a str: single-quoted
// unless s holds a quote but no double quote, with backslash/quote/control
// characters escaped. It covers ASCII and the common escapes exactly; for
// exotic non-printable Unicode it approximates Python's isprintable() via
// unicode.IsPrint, which is close enough for realistic ref/name input.
func pyRepr(s string) string {
	quote := byte('\'')
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case !unicode.IsPrint(r):
			switch {
			case r <= 0xff:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r <= 0xffff:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}
