package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/l4sh/agtrack/internal/cli/argp"
	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/render"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

// Shared lessons (agent-features spec H2, H3, H4).

func init() {
	fieldsFor["lesson"] = [2][]string{{"id", "title", "scope", "age"}, {"kind", "stale", "paths", "cmds", "topics", "body"}}
}

func lessonListFlags() []argp.Flag {
	return append(scopeFlags(),
		append([]argp.Flag{
			{Long: "--path", Metavar: "FILE", Help: "lessons whose path trigger matches this repo-relative (or absolute) file"},
			{Long: "--cmd", Metavar: "CMD", Help: "lessons whose command trigger prefixes this command"},
			{Long: "--topic", Metavar: "T", Help: "lessons with this topic"},
			{Long: "--q", Dest: "query", Metavar: "TEXT", Help: "full-text search over titles and bodies"},
			{Long: "--unverified", Metavar: "AGE", Help: "only lessons not verified for this long (30d)"},
			{Long: "--retired", Kind: argp.Bool, Help: "include retired lessons"},
			{Long: "--limit", Short: "-n", Kind: argp.PositiveInt},
		}, listFlags()...)...)
}

func lessonTree() *argp.Command {
	return &argp.Command{Name: "lesson", Help: "shared lessons: gotchas, fixes, decisions and facts", SubRequired: true,
		Subs: []*argp.Command{
			{Name: "add", Help: "record a lesson (refused when a similar one exists at this or a wider scope)", Run: Handler(lessonAdd),
				Args: []argp.Arg{{Name: "title", NArgs: "+"}},
				Flags: append(scopeFlags(),
					argp.Flag{Long: "--body", Metavar: "TEXT"},
					argp.Flag{Long: "--body-file", Metavar: "F", Help: "read the body from a file (- = stdin)"},
					argp.Flag{Long: "--kind", Choices: store.LessonKinds, Help: "gotcha (default), fix, decision or fact"},
					argp.Flag{Long: "--path", Kind: argp.Append, Metavar: "GLOB", Help: "trigger: files it applies to; repeatable"},
					argp.Flag{Long: "--cmd", Kind: argp.Append, Metavar: "PREFIX", Help: "trigger: commands it applies to; repeatable"},
					argp.Flag{Long: "--topic", Kind: argp.Append, Metavar: "T", Help: "trigger: topic; repeatable"},
					argp.Flag{Long: "--supersedes", Kind: argp.Int, Metavar: "ID", Help: "replace this lesson (it is retired)"},
					argp.Flag{Long: "--force", Kind: argp.Bool, Help: "add even though a similar lesson exists"})},
			{Name: "show", Help: "a lesson with its body and history", Run: Handler(lessonShow), Args: []argp.Arg{{Name: "id"}}},
			{Name: "verify", Help: "record that a lesson still holds", Run: Handler(lessonVerify), Args: []argp.Arg{{Name: "id"}},
				Flags: []argp.Flag{{Long: "--note", Help: "how you know"}}},
			{Name: "retire", Help: "take a lesson out of the lists", Run: Handler(lessonRetire), Args: []argp.Arg{{Name: "id"}},
				Flags: []argp.Flag{{Long: "--note", Help: "why (required)"}}},
			{Name: "amend", Help: "edit a lesson or add triggers", Run: Handler(lessonAmend), Args: []argp.Arg{{Name: "id"}},
				Flags: []argp.Flag{{Long: "--title"}, {Long: "--body"}, {Long: "--body-file", Metavar: "F"},
					{Long: "--kind", Choices: store.LessonKinds},
					{Long: "--add-path", Kind: argp.Append, Metavar: "GLOB"}, {Long: "--add-cmd", Kind: argp.Append, Metavar: "PREFIX"},
					{Long: "--add-topic", Kind: argp.Append, Metavar: "T"}}},
			{Name: "ls", Aliases: []string{"list"}, Help: "same as `lessons`", Run: Handler(lessonsLs), Flags: lessonListFlags()},
		}}
}

func lessonsCmd() *argp.Command {
	return &argp.Command{Name: "lessons", Help: "lessons that apply here, one line each, best match first (-x adds bodies)", Run: Handler(lessonsLs), Flags: lessonListFlags()}
}

func readBody(c *Ctx, inline, file string) (*string, error) {
	if c.Has(file) {
		if c.Has(inline) {
			return nil, errs.Usage("give --%s or --%s, not both", strings.ReplaceAll(inline, "_", "-"), strings.ReplaceAll(file, "_", "-"))
		}
		var b []byte
		var err error
		if c.Str(file) == "-" {
			b, err = readAllStdin()
		} else {
			b, err = os.ReadFile(c.Str(file))
		}
		if err != nil {
			return nil, errs.New("cannot read %s: %v", c.Str(file), err)
		}
		s := string(b)
		return &s, nil
	}
	if c.Given[inline] || c.Has(inline) {
		s := c.Str(inline)
		return &s, nil
	}
	return nil, nil
}

var readAllStdin = func() ([]byte, error) {
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return []byte(sb.String()), nil
}

func lessonAdd(c *Ctx) error {
	show := func(ids []int64) error {
		l, err := c.S.GetLesson(ids[0])
		if err != nil {
			return err
		}
		return lessonCreated(c, l)
	}
	if done, err := c.replay(show); done {
		return err
	}
	sc, err := addScope(c)
	if err != nil {
		return err
	}
	body, err := readBody(c, "body", "body_file")
	if err != nil {
		return err
	}
	in := store.LessonInput{Title: c.Str("title"), Kind: c.Str("kind"), Scope: sc, Paths: c.Strs("path"), Cmds: c.Strs("cmd"),
		Topics: c.Tags("topic"), Supersedes: int64(c.Int("supersedes")), Force: c.Bool("force")}
	if body != nil {
		in.Body = *body
	}
	var added *row.Row
	ids, err := c.record("lesson", func() ([]int64, error) {
		l, err := c.S.AddLesson(in)
		if err != nil {
			return nil, err
		}
		added = l
		return []int64{l.Int("id")}, nil
	})
	if err != nil {
		return err
	}
	if added == nil {
		return show(ids)
	}
	return lessonCreated(c, added)
}

func lessonCreated(c *Ctx, l *row.Row) error {
	l.Delete("events")
	if !c.structured() && !c.Quiet {
		c.printf("lesson %d: %s (%s)\n", l.Int("id"), l.Str("title"), l.Str("scope"))
		return nil
	}
	return c.created("lesson", l.Int("id"), l, "lesson")
}

func lessonIDArg(c *Ctx) (int64, error) {
	return parseID(strings.TrimPrefix(c.Str("id"), "#"))
}

func lessonShow(c *Ctx) error {
	id, err := lessonIDArg(c)
	if err != nil {
		return err
	}
	l, err := c.S.GetLesson(id)
	if err != nil {
		return err
	}
	var secs []render.Section
	for _, k := range []string{"id", "title", "kind", "scope", "status", "stale", "verified_at", "verified_by", "actor", "created_at"} {
		if v := l.Get(k); v != nil && v != "" {
			secs = append(secs, render.Section{Label: k, Value: v})
		}
	}
	for _, k := range []string{"paths", "cmds", "topics"} {
		if v, _ := l.Get(k).([]string); len(v) > 0 {
			secs = append(secs, render.Section{Label: k, Value: strings.Join(v, ", ")})
		}
	}
	if b := l.Str("body"); b != "" {
		secs = append(secs, render.Section{Label: "body", Value: b})
	}
	var hist []string
	for _, e := range rows(l.Get("events")) {
		hist = append(hist, fmt.Sprintf("%s %s by %s: %s", refs.FmtTS(e.Str("at"), false, false), e.Str("action"), e.Str("actor"), e.Str("detail")))
	}
	secs = append(secs, render.Section{Label: "history", Value: strings.Join(hist, "\n")})
	return c.detail(l, "lesson", secs)
}

func lessonVerify(c *Ctx) error {
	id, err := lessonIDArg(c)
	if err != nil {
		return err
	}
	l, err := c.S.VerifyLesson(id, c.Str("note"))
	if err != nil {
		return err
	}
	return c.mutated(fmt.Sprintf("verified lesson %d: %s", id, l.Str("title")), l)
}

func lessonRetire(c *Ctx) error {
	id, err := lessonIDArg(c)
	if err != nil {
		return err
	}
	if strings.TrimSpace(c.Str("note")) == "" {
		return errs.Usage("lesson retire needs --note: why it no longer holds")
	}
	l, err := c.S.RetireLesson(id, c.Str("note"))
	if err != nil {
		return err
	}
	return c.mutated(fmt.Sprintf("retired lesson %d: %s", id, l.Str("title")), l)
}

func lessonAmend(c *Ctx) error {
	id, err := lessonIDArg(c)
	if err != nil {
		return err
	}
	body, err := readBody(c, "body", "body_file")
	if err != nil {
		return err
	}
	a := store.LessonAmend{Body: body, Kind: c.Str("kind"), AddPaths: c.Strs("add_path"), AddCmds: c.Strs("add_cmd"), AddTopics: c.Tags("add_topic")}
	if c.Has("title") {
		t := c.Str("title")
		a.Title = &t
	}
	l, err := c.S.AmendLesson(id, a)
	if err != nil {
		return err
	}
	return c.mutated(fmt.Sprintf("amended lesson %d: %s", id, l.Str("title")), l)
}

func lessonsLs(c *Ctx) error {
	p, err := resolvePoint(c)
	if err != nil {
		return err
	}
	q := store.LessonQuery{Point: p, Cmd: c.Str("cmd"), Topic: c.Str("topic"), Q: c.Str("query"), Retired: c.Bool("retired"), Limit: c.Int("limit")}
	if v := c.Str("path"); v != "" {
		_, top := cwdGit()
		q.Path = repoRel(v, top)
	}
	if v := c.Str("unverified"); v != "" {
		since, err := refs.ParseSince(v)
		if err != nil {
			return err
		}
		q.UnverifiedSince = since
	}
	ls, total, err := c.S.ListLessons(q)
	if err != nil {
		return err
	}
	if !c.Extended && len(c.Fields) == 0 {
		for _, l := range ls {
			l.Delete("body")
		}
	}
	return c.emit(ls, "lesson", total)
}
