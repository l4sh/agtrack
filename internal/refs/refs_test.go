package refs

import (
	"reflect"
	"testing"
	"time"

	"github.com/l4sh/agtrack/internal/errs"
)

// TestMain pins time.Local so today/yesterday/local-conversion tests are
// deterministic and unaffected by the host's zone or DST rules.
func TestMain(m *testing.M) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		panic(err)
	}
	time.Local = loc
	m.Run()
}

func wantErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error %q, got nil", want)
	}
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	if _, ok := errs.As(err); !ok {
		t.Fatalf("error is not *errs.Error: %#v", err)
	}
}

// --- status ---------------------------------------------------------------

func TestNormalizeStatus(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr string
	}{
		{in: "wip", want: "in-progress"},
		{in: " done ", want: "done"},
		{in: "todo", want: "todo"},
		{in: "t", want: "todo"},
		{in: "open", want: "todo"},
		{in: "pending", want: "todo"},
		{in: "in-progress", want: "in-progress"},
		{in: "in_progress", want: "in-progress"},
		{in: "inprogress", want: "in-progress"},
		{in: "progress", want: "in-progress"},
		{in: "doing", want: "in-progress"},
		{in: "started", want: "in-progress"},
		{in: "d", want: "done"},
		{in: "complete", want: "done"},
		{in: "completed", want: "done"},
		{in: "finished", want: "done"},
		{in: "cancelled", want: "cancelled"},
		{in: "canceled", want: "cancelled"},
		{in: "cancel", want: "cancelled"},
		{in: "x", want: "cancelled"},
		{in: "dropped", want: "cancelled"},
		{in: "WIP", want: "in-progress"},
		{in: "unknown", wantErr: "unknown status: unknown (expected one of todo, in-progress, done, cancelled)"},
		{in: "", wantErr: "unknown status:  (expected one of todo, in-progress, done, cancelled)"},
	}
	for _, c := range cases {
		got, err := NormalizeStatus(c.in)
		if c.wantErr != "" {
			wantErr(t, err, c.wantErr)
			continue
		}
		if err != nil {
			t.Fatalf("NormalizeStatus(%q) unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("NormalizeStatus(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseStatusFilter(t *testing.T) {
	cases := []struct {
		in      string
		want    []string
		wantErr string
	}{
		{in: "open,done", want: []string{"todo", "in-progress", "done"}},
		{in: "ALL", want: []string{"todo", "in-progress", "done", "cancelled"}},
		{in: "closed", want: []string{"done", "cancelled"}},
		{in: "", want: []string{}},
		{in: "done,,cancelled", want: []string{"done", "cancelled"}},
		{in: "done,complete", want: []string{"done"}},
		{in: "bogus", wantErr: "unknown status: bogus (expected one of todo, in-progress, done, cancelled)"},
	}
	for _, c := range cases {
		got, err := ParseStatusFilter(c.in)
		if c.wantErr != "" {
			wantErr(t, err, c.wantErr)
			continue
		}
		if err != nil {
			t.Fatalf("ParseStatusFilter(%q) unexpected error: %v", c.in, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseStatusFilter(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestNormalizePRState(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr string
	}{
		{in: "OPEN", want: "open"},
		{in: " draft ", want: "draft"},
		{in: "merged", want: "merged"},
		{in: "closed", want: "closed"},
		{in: "bogus", wantErr: "unknown pull request state: bogus (expected one of open, draft, merged, closed)"},
	}
	for _, c := range cases {
		got, err := NormalizePRState(c.in)
		if c.wantErr != "" {
			wantErr(t, err, c.wantErr)
			continue
		}
		if err != nil {
			t.Fatalf("NormalizePRState(%q) unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("NormalizePRState(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- slugify ----------------------------------------------------------------

func TestSlugify(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr string
	}{
		{in: "Foo Bar!", want: "foo-bar"},
		{in: "  hello_world  ", want: "hello_world"},
		{in: "___", wantErr: "invalid name: '___' (use letters, digits, . _ -)"},
		{in: "-abc-", want: "abc"},
		{in: "ABC123", want: "abc123"},
		{in: "a..b", want: "a..b"},
		{in: "  ", wantErr: "invalid name: '  ' (use letters, digits, . _ -)"},
		{in: "!!!", wantErr: "invalid name: '!!!' (use letters, digits, . _ -)"},
		{in: "", wantErr: "invalid name: '' (use letters, digits, . _ -)"},
	}
	for _, c := range cases {
		got, err := Slugify(c.in)
		if c.wantErr != "" {
			wantErr(t, err, c.wantErr)
			continue
		}
		if err != nil {
			t.Fatalf("Slugify(%q) unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("Slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSlugifyReprEscaping(t *testing.T) {
	cases := []struct {
		in      string
		wantErr string
	}{
		{in: ".it's", wantErr: `invalid name: ".it's" (use letters, digits, . _ -)`},
		{in: `.back\slash`, wantErr: `invalid name: '.back\\slash' (use letters, digits, . _ -)`},
		{in: ".tab\there", wantErr: `invalid name: '.tab\there' (use letters, digits, . _ -)`},
		// Non-ASCII, non-printable runes escape the way Python's repr()
		// does: \xHH below U+100, \uHHHH below U+10000, \UHHHHHHHH above.
		{in: ". 󠀁", wantErr: `invalid name: '.\x80\u2028\U000e0001' (use letters, digits, . _ -)`},
	}
	for _, c := range cases {
		_, err := Slugify(c.in)
		wantErr(t, err, c.wantErr)
	}
}

// --- project ref -------------------------------------------------------------

func TestParseProjectRef(t *testing.T) {
	cases := []struct {
		in             string
		ctx, proj, sub string
		wantErr        string
	}{
		{in: "orbiter", proj: "orbiter"},
		{in: "work/orbiter:frontend", ctx: "work", proj: "orbiter", sub: "frontend"},
		{in: "a:b:c", wantErr: "invalid project reference: a:b:c (nesting is project:subproject only)"},
		{in: "Work/Orbiter", ctx: "work", proj: "orbiter"},
		{in: "/leadingslash", wantErr: "invalid name: '' (use letters, digits, . _ -)"},
		{in: "a/b/c:d", ctx: "a", proj: "b-c", sub: "d"},
		{in: ":sub", wantErr: "invalid project reference: :sub"},
		{in: "proj:", proj: "proj"},
		{in: "", wantErr: "empty project reference"},
		{in: "   ", wantErr: "empty project reference"},
	}
	for _, c := range cases {
		ctx, proj, sub, err := ParseProjectRef(c.in)
		if c.wantErr != "" {
			wantErr(t, err, c.wantErr)
			continue
		}
		if err != nil {
			t.Fatalf("ParseProjectRef(%q) unexpected error: %v", c.in, err)
		}
		if ctx != c.ctx || proj != c.proj || sub != c.sub {
			t.Errorf("ParseProjectRef(%q) = (%q,%q,%q), want (%q,%q,%q)",
				c.in, ctx, proj, sub, c.ctx, c.proj, c.sub)
		}
	}
}

// --- task / subtask refs -----------------------------------------------------

func TestParseTaskRef(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr string
	}{
		{in: "12", want: 12},
		{in: "#12", want: 12},
		{in: "##12", want: 12},
		{in: "t12", want: 12},
		{in: "T12", want: 12},
		{in: " 12 ", want: 12},
		{in: "t", wantErr: "invalid task reference: t (expected a task id)"},
		{in: "tt12", wantErr: "invalid task reference: tt12 (expected a task id)"},
		{in: "-5", wantErr: "invalid task reference: -5 (expected a task id)"},
		{in: "12.5", wantErr: "invalid task reference: 12.5 (expected a task id)"},
		{in: "", wantErr: "invalid task reference:  (expected a task id)"},
		{in: "99999999999999999999", wantErr: "invalid task reference: 99999999999999999999 (expected a task id)"},
	}
	for _, c := range cases {
		got, err := ParseTaskRef(c.in)
		if c.wantErr != "" {
			wantErr(t, err, c.wantErr)
			continue
		}
		if err != nil {
			t.Fatalf("ParseTaskRef(%q) unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseTaskRef(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseSubtaskRef(t *testing.T) {
	cases := []struct {
		in          string
		taskID, pos int64
		wantErr     string
	}{
		{in: "12.3", taskID: 12, pos: 3},
		{in: "47", taskID: 0, pos: 47},
		{in: "#47", taskID: 0, pos: 47},
		{in: "t12.3", taskID: 12, pos: 3},
		{in: "tt12.3", taskID: 12, pos: 3},
		{in: "s47", taskID: 0, pos: 47},
		{in: "S47", taskID: 0, pos: 47},
		{in: "  47  ", taskID: 0, pos: 47},
		{in: "12.", wantErr: "invalid subtask reference: 12."},
		{in: ".3", wantErr: "invalid subtask reference: .3"},
		{in: "12.3.4", wantErr: "invalid subtask reference: 12.3.4"},
		{in: "t12.s3", wantErr: "invalid subtask reference: t12.s3"},
		{in: "T12.3", wantErr: "invalid subtask reference: T12.3"},
		{in: "Tt12.3", wantErr: "invalid subtask reference: Tt12.3"},
		{in: "tT12.3", wantErr: "invalid subtask reference: tT12.3"},
		{in: "TT12.3", wantErr: "invalid subtask reference: TT12.3"},
		{in: "t12.S3", wantErr: "invalid subtask reference: t12.S3"},
		{in: "12.s3", wantErr: "invalid subtask reference: 12.s3"},
		{in: "#T47", wantErr: "invalid subtask reference: #T47 (expected <id> or <task>.<position>)"},
		{in: "SS47", wantErr: "invalid subtask reference: SS47 (expected <id> or <task>.<position>)"},
		{in: "ss47", wantErr: "invalid subtask reference: ss47 (expected <id> or <task>.<position>)"},
		{in: "nope", wantErr: "invalid subtask reference: nope (expected <id> or <task>.<position>)"},
		{in: "99999999999999999999", wantErr: "invalid subtask reference: 99999999999999999999 (expected <id> or <task>.<position>)"},
		{in: "99999999999999999999.3", wantErr: "invalid subtask reference: 99999999999999999999.3"},
	}
	for _, c := range cases {
		taskID, pos, err := ParseSubtaskRef(c.in)
		if c.wantErr != "" {
			wantErr(t, err, c.wantErr)
			continue
		}
		if err != nil {
			t.Fatalf("ParseSubtaskRef(%q) unexpected error: %v", c.in, err)
		}
		if taskID != c.taskID || pos != c.pos {
			t.Errorf("ParseSubtaskRef(%q) = (%d,%d), want (%d,%d)", c.in, taskID, pos, c.taskID, c.pos)
		}
	}
}

// --- repo -------------------------------------------------------------------

func TestNormalizeRepo(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr string
	}{
		{in: "git@github.com:Acme-Tools/Orbiter.git", want: "acme-tools/orbiter"},
		{in: "https://github.com/acme-tools/orbiter", want: "acme-tools/orbiter"},
		{in: "ssh://git@github.com/Acme-Tools/orbiter.git", want: "acme-tools/orbiter"},
		{in: "github.com/Acme-Tools/orbiter", want: "acme-tools/orbiter"},
		{in: "Acme-Tools/orbiter", want: "acme-tools/orbiter"},
		{in: "https://github.com/Acme-Tools/Orbiter", want: "acme-tools/orbiter"},
		{in: "http://github.com/Acme-Tools/Orbiter", want: "acme-tools/orbiter"},
		{in: "git://github.com/Acme-Tools/Orbiter.git", want: "acme-tools/orbiter"},
		{in: "https://gitlab.com/group/sub/Orbiter.git", want: "sub/orbiter"},
		{in: "https://gitlab.com/a/b/c.git", want: "b/c"},
		{in: "https://gitlab.com/a/b/c", want: "b/c"},
		{in: "git@github.com:x/y.git", want: "x/y"},
		{in: "gitlab.com/x/y", want: "x/y"},
		{in: "x/y", want: "x/y"},
		{in: "x/y.git", want: "x/y"},
		{in: "Acme-Tools/orbiter.git", want: "acme-tools/orbiter"},
		{in: "http://example.com/owner/repo/", want: "owner/repo"},
		{in: "git://example.com/owner/repo.git", want: "owner/repo"},
		{in: "x/y/", wantErr: "cannot parse repository reference: x/y/"},
		{in: "Acme-Tools/orbiter/", wantErr: "cannot parse repository reference: Acme-Tools/orbiter/"},
		{in: "git@gitlab.com:group/sub/Orbiter.git", wantErr: "cannot parse repository reference: git@gitlab.com:group/sub/Orbiter.git"},
		{in: "not a repo!!", wantErr: "cannot parse repository reference: not a repo!!"},
		{in: "a.b.c.d/owner/name", wantErr: "cannot parse repository reference: a.b.c.d/owner/name"},
		{in: "", wantErr: "empty repository reference"},
		{in: "   ", wantErr: "empty repository reference"},
	}
	for _, c := range cases {
		got, err := NormalizeRepo(c.in)
		if c.wantErr != "" {
			wantErr(t, err, c.wantErr)
			continue
		}
		if err != nil {
			t.Fatalf("NormalizeRepo(%q) unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("NormalizeRepo(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRepoHost(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{in: "https://github.com/acme-tools/orbiter", want: "github.com"},
		{in: "git@github.com:Acme-Tools/Orbiter.git", want: "github.com"},
		{in: "ssh://git@github.com/Acme-Tools/orbiter.git", want: "github.com"},
		{in: "gitlab.com/x/y", want: "gitlab.com"},
		{in: "Acme-Tools/orbiter", want: ""},
		{in: "not a repo!!", want: ""},
		{in: "", want: ""},
	}
	for _, c := range cases {
		if got := RepoHost(c.in); got != c.want {
			t.Errorf("RepoHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRepoIdentity(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr string
	}{
		{in: "https://github.com/acme-tools/orbiter", want: "github.com/acme-tools/orbiter"},
		{in: "git@github.com:Acme-Tools/Orbiter.git", want: "github.com/acme-tools/orbiter"},
		{in: "gitlab.com/x/y", want: "gitlab.com/x/y"},
		{in: "Acme-Tools/orbiter", want: "acme-tools/orbiter"},
		{in: "not a repo!!", wantErr: "cannot parse repository reference: not a repo!!"},
		{in: "", wantErr: "empty repository reference"},
	}
	for _, c := range cases {
		got, err := RepoIdentity(c.in)
		if c.wantErr != "" {
			wantErr(t, err, c.wantErr)
			continue
		}
		if err != nil {
			t.Fatalf("RepoIdentity(%q) unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("RepoIdentity(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParsePRURL(t *testing.T) {
	cases := []struct {
		in       string
		wantRepo string
		wantNum  int64
		wantOK   bool
	}{
		{in: "https://github.com/o/N/pull/12", wantRepo: "o/n", wantNum: 12, wantOK: true},
		{in: "https://github.com/o/n/pulls/12", wantRepo: "o/n", wantNum: 12, wantOK: true},
		{in: "https://gitlab.com/o/n/pull/7extra", wantRepo: "o/n", wantNum: 7, wantOK: true},
		{in: "http://x.com/O/N/pull/5", wantRepo: "o/n", wantNum: 5, wantOK: true},
		{in: "https://example.com/x", wantOK: false},
		{in: "https://example.com/notapr", wantOK: false},
		{in: "https://github.com/o/n/pull/99999999999999999999", wantOK: false},
	}
	for _, c := range cases {
		repo, num, ok := ParsePRURL(c.in)
		if ok != c.wantOK {
			t.Errorf("ParsePRURL(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if repo != c.wantRepo || num != c.wantNum {
			t.Errorf("ParsePRURL(%q) = (%q,%d), want (%q,%d)", c.in, repo, num, c.wantRepo, c.wantNum)
		}
	}
}

// --- time --------------------------------------------------------------------

func TestNowAndNowISO(t *testing.T) {
	t.Setenv("AGTRACK_NOW", "2026-01-15T12:00:00Z")
	got := Now()
	want := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("Now() = %v, want %v", got, want)
	}
	if iso := NowISO(); iso != "2026-01-15T12:00:00Z" {
		t.Fatalf("NowISO() = %q", iso)
	}
}

func TestNowFallsBackOnBadOverride(t *testing.T) {
	t.Setenv("AGTRACK_NOW", "not-a-time")
	got := Now()
	if time.Since(got) > time.Minute || time.Since(got) < -time.Minute {
		t.Fatalf("Now() with bad override = %v, want close to real now", got)
	}
}

func TestToUTCISO(t *testing.T) {
	loc := time.FixedZone("test", 5*3600)
	tm := time.Date(2026, 9, 29, 7, 22, 8, 500_000_000, loc)
	got := ToUTCISO(tm)
	want := "2026-09-29T02:22:08Z"
	if got != want {
		t.Fatalf("ToUTCISO = %q, want %q", got, want)
	}
}

func TestParseSinceTodayYesterday(t *testing.T) {
	t.Setenv("AGTRACK_NOW", "2026-01-15T12:00:00Z") // 07:00 local (America/New_York, EST, UTC-5)
	got, err := ParseSince("today")
	if err != nil {
		t.Fatal(err)
	}
	if want := "2026-01-15T05:00:00Z"; got != want {
		t.Errorf("today = %q, want %q", got, want)
	}
	got, err = ParseSince("YESTERDAY")
	if err != nil {
		t.Fatal(err)
	}
	if want := "2026-01-14T05:00:00Z"; got != want {
		t.Errorf("yesterday = %q, want %q", got, want)
	}
}

func TestParseSinceDurations(t *testing.T) {
	t.Setenv("AGTRACK_NOW", "2026-01-15T12:00:00Z")
	cases := []struct {
		in   string
		want string
	}{
		{"0s", "2026-01-15T12:00:00Z"},
		{"30s", "2026-01-15T11:59:30Z"},
		{"5m", "2026-01-15T11:55:00Z"},
		{"2d", "2026-01-13T12:00:00Z"},
		{"36H", "2026-01-14T00:00:00Z"},
		{"1w", "2026-01-08T12:00:00Z"},
	}
	for _, c := range cases {
		got, err := ParseSince(c.in)
		if err != nil {
			t.Fatalf("ParseSince(%q) unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseSince(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseSinceISO(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr string
	}{
		{in: "2026-01-15", want: "2026-01-15T05:00:00Z"},
		{in: "2026-01-15T10:30", want: "2026-01-15T15:30:00Z"},
		{in: "2026-01-15T10:30:45", want: "2026-01-15T15:30:45Z"},
		{in: "2026-01-15 10:30:45", want: "2026-01-15T15:30:45Z"},
		{in: "2026-01-15T10:30:45.123456", want: "2026-01-15T15:30:45Z"},
		{in: "2026-01-15T10:30:45Z", want: "2026-01-15T10:30:45Z"},
		{in: "2026-01-15T10:30:45+05:00", want: "2026-01-15T05:30:45Z"},
		{in: "2026-01-15T10:30:45-05:00", want: "2026-01-15T15:30:45Z"},
		{in: "2026-02-30", wantErr: "cannot parse time: 2026-02-30 (use 2d, 36h, today, yesterday or a date)"},
		{in: "2026-01-15T25:00", wantErr: "cannot parse time: 2026-01-15T25:00 (use 2d, 36h, today, yesterday or a date)"},
		{in: "not-a-date", wantErr: "cannot parse time: not-a-date (use 2d, 36h, today, yesterday or a date)"},
		{in: "5x", wantErr: "cannot parse time: 5x (use 2d, 36h, today, yesterday or a date)"},
		{in: "soonish", wantErr: "cannot parse time: soonish (use 2d, 36h, today, yesterday or a date)"},
	}
	for _, c := range cases {
		got, err := ParseSince(c.in)
		if c.wantErr != "" {
			wantErr(t, err, c.wantErr)
			continue
		}
		if err != nil {
			t.Fatalf("ParseSince(%q) unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("ParseSince(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseWhen(t *testing.T) {
	t.Setenv("AGTRACK_NOW", "2026-01-15T12:00:00Z")
	got, err := ParseWhen("1h", "started")
	if err != nil {
		t.Fatal(err)
	}
	if want := "2026-01-15T11:00:00Z"; got != want {
		t.Errorf("ParseWhen(1h) = %q, want %q", got, want)
	}

	_, err = ParseWhen("2026-01-15T13:00:00Z", "started")
	wantErr(t, err, "started is in the future: 2026-01-15T13:00:00Z. agtrack records work that happened, not work that is planned")

	_, err = ParseWhen("not-a-date", "started")
	wantErr(t, err, "cannot parse time: not-a-date (use 2d, 36h, today, yesterday or a date)")

	// Within the 5 minute grace window: allowed.
	got, err = ParseWhen("2026-01-15T12:04:00Z", "started")
	if err != nil {
		t.Fatalf("within grace window should not error: %v", err)
	}
	if want := "2026-01-15T12:04:00Z"; got != want {
		t.Errorf("ParseWhen(grace) = %q, want %q", got, want)
	}
}

func TestFmtTS(t *testing.T) {
	cases := []struct {
		value    string
		utc, doo bool
		want     string
	}{
		{value: "", want: ""},
		{value: "garbage", want: "garbage"},
		{value: "2026-01-15T10:30:45Z", want: "2026-01-15 05:30"},
		{value: "2026-01-15T10:30:45Z", utc: true, want: "2026-01-15 10:30"},
		{value: "2026-01-15T10:30:45Z", doo: true, want: "2026-01-15"},
		{value: "2026-01-15T10:30:45+05:00", want: "2026-01-15 00:30"},
		{value: "2026-01-15T10:30:45+05:00", utc: true, want: "2026-01-15 10:30"},
		{value: "2026-01-15T10:30:45+05:00", doo: true, want: "2026-01-15"},
	}
	for _, c := range cases {
		got := FmtTS(c.value, c.utc, c.doo)
		if got != c.want {
			t.Errorf("FmtTS(%q, utc=%v, dateOnly=%v) = %q, want %q", c.value, c.utc, c.doo, got, c.want)
		}
	}
}

// --- constants ---------------------------------------------------------------

func TestConstants(t *testing.T) {
	if !reflect.DeepEqual(Statuses, []string{"todo", "in-progress", "done", "cancelled"}) {
		t.Errorf("Statuses = %#v", Statuses)
	}
	if !reflect.DeepEqual(PRStates, []string{"open", "draft", "merged", "closed"}) {
		t.Errorf("PRStates = %#v", PRStates)
	}
	if !reflect.DeepEqual(OpenStatuses, []string{"todo", "in-progress"}) {
		t.Errorf("OpenStatuses = %#v", OpenStatuses)
	}
	if !reflect.DeepEqual(ClosedStatuses, []string{"done", "cancelled"}) {
		t.Errorf("ClosedStatuses = %#v", ClosedStatuses)
	}
	wantMarks := map[string]string{
		"todo": "[ ]", "in-progress": "[~]", "done": "[x]", "cancelled": "[-]",
	}
	if !reflect.DeepEqual(StatusMark, wantMarks) {
		t.Errorf("StatusMark = %#v", StatusMark)
	}
}
