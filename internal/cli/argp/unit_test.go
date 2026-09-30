package argp

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/l4sh/agtrack/internal/errs"
)

func mustParse(t *testing.T, p *Parser, argv ...string) *Result {
	t.Helper()
	res, err := p.Parse(argv)
	if err != nil {
		t.Fatalf("Parse(%q): %v", argv, err)
	}
	return res
}

func TestDefaultValueTypes(t *testing.T) {
	// Lead addendum: absent flags without a Default are nil (not ""), Bool is
	// false, Append nil; +/* positionals are []string, ?/single string or nil.
	p := &Parser{Root: &Command{Name: "x", SubRequired: true, Subs: []*Command{{
		Name: "v",
		Args: []Arg{{Name: "one"}, {Name: "opt", NArgs: "?"}, {Name: "star", NArgs: "*"}},
		Flags: []Flag{
			{Long: "--title"}, {Long: "--flag", Kind: Bool}, {Long: "--tag", Kind: Append},
			{Long: "--n", Kind: Int}, {Long: "--lim", Kind: PositiveInt, Default: 50},
			{Long: "--words", Kind: NArgsPlus}, {Long: "--order", Default: "id"},
		},
	}}}, Globals: []Flag{{Long: "--db"}, {Long: "--quiet", Kind: Bool}}}

	res := mustParse(t, p, "v", "a")
	want := map[string]any{
		"one": "a", "opt": nil, "star": []string{},
		"title": nil, "flag": false, "tag": nil, "n": nil, "lim": 50, "words": nil, "order": "id",
	}
	if !reflect.DeepEqual(res.Values, want) {
		t.Errorf("Values = %#v\nwant %#v", res.Values, want)
	}
	if len(res.Given) != 1 || !res.Given["one"] {
		t.Errorf("Given = %v", res.Given)
	}

	res = mustParse(t, p, "v", "a", "b", "c", "d", "--title", "", "--flag", "--tag", "x", "--tag=y",
		"--n", "-4", "--lim", "3", "--words", "p", "q", "--db", "f", "--quiet")
	want = map[string]any{
		"one": "a", "opt": "b", "star": []string{"c", "d"},
		"title": "", "flag": true, "tag": []string{"x", "y"}, "n": -4, "lim": 3,
		"words": []string{"p", "q"}, "order": "id", "db": "f", "quiet": true,
	}
	if !reflect.DeepEqual(res.Values, want) {
		t.Errorf("Values = %#v\nwant %#v", res.Values, want)
	}
	for _, d := range []string{"title", "flag", "tag", "n", "lim", "words", "db", "quiet", "opt", "star"} {
		if !res.Given[d] {
			t.Errorf("Given[%s] = false", d)
		}
	}
	if res.Given["order"] {
		t.Error("Given[order] = true for a default")
	}
	if res.Str("title") != "" || !res.Has("title") || res.Has("nope") || !res.Bool("flag") ||
		!reflect.DeepEqual(res.Strs("tag"), []string{"x", "y"}) {
		t.Error("accessors disagree with Values")
	}
	if n, ok := res.Int("lim"); !ok || n != 3 {
		t.Errorf("Int(lim) = %d, %v", n, ok)
	}
}

func TestResultPathAndCmd(t *testing.T) {
	p := agtrackParser()
	res := mustParse(t, p, "t", "list", "-n", "2")
	if !reflect.DeepEqual(res.Path, []string{"task", "ls"}) || !reflect.DeepEqual(res.Typed, []string{"t", "list"}) {
		t.Errorf("Path %v Typed %v", res.Path, res.Typed)
	}
	if res.Cmd != p.Find([]string{"task", "ls"}) {
		t.Error("Cmd is not the task ls command")
	}
	res = mustParse(t, p, "worklog")
	if !reflect.DeepEqual(res.Path, []string{"log"}) || res.Cmd.Name != "log" {
		t.Errorf("bare log: Path %v", res.Path)
	}
}

func TestErrorShape(t *testing.T) {
	p := agtrackParser()
	_, err := p.Parse([]string{"task", "ls", "-n", "0"})
	e, ok := errs.As(err)
	if !ok || e.Code != errs.ExitUsage || e.Token != errs.TokUsage {
		t.Fatalf("err = %#v", err)
	}
	if e.Extra["command"] != "task ls" {
		t.Errorf("Extra = %v", e.Extra)
	}
	if path, ok := ErrorPath(err); !ok || !reflect.DeepEqual(path, []string{"task", "ls"}) {
		t.Errorf("ErrorPath = %v %v", path, ok)
	}
	_, err = p.Parse([]string{"task", "ls", "junk"})
	if path, ok := ErrorPath(err); !ok || len(path) != 0 {
		t.Errorf("root ErrorPath = %v %v", path, ok)
	}
	if _, ok := ErrorPath(errors.New("x")); ok {
		t.Error("ErrorPath accepted a plain error")
	}
	if strings.Contains(e.Message, "usage:") {
		t.Error("message carries the usage banner")
	}
}

func TestHelpRequest(t *testing.T) {
	p := agtrackParser()
	tests := []struct {
		argv    []string
		path    []string
		version bool
	}{
		{[]string{"-h"}, []string{}, false},
		{[]string{"t", "add", "--he"}, []string{"task", "add"}, false},
		{[]string{"sub", "-h", "add"}, []string{"sub"}, false},
		{[]string{"-V"}, nil, true},
	}
	for _, tt := range tests {
		_, err := p.Parse(tt.argv)
		var h *HelpRequest
		if !errors.As(err, &h) {
			t.Fatalf("%v: err = %v", tt.argv, err)
		}
		if h.Version != tt.version || (!tt.version && !reflect.DeepEqual(h.Path, tt.path)) {
			t.Errorf("%v: got %+v", tt.argv, h)
		}
		if !tt.version && h.Text != p.Help(tt.path) {
			t.Errorf("%v: text differs from Help()", tt.argv)
		}
	}
}

func TestHelpLayout(t *testing.T) {
	p := agtrackParser()
	root := p.Help(nil)
	for _, want := range []string{
		"usage: agtrack [-h] [--db PATH]", "Context / project / task", "commands:",
		"  ctx (context, contexts)", "  task (tasks, t)       tasks: the unit of work",
		"global flags:", "  -f, --format {table,plain,tsv,json,yaml}\n                        output format",
		"-V, --version", "exit codes:\n  0 ok",
	} {
		if !strings.Contains(root, want) {
			t.Errorf("root help lacks %q", want)
		}
	}
	verb := p.Help([]string{"tasks", "add"})
	for _, want := range []string{
		"usage: agtrack task add [-h] [-p REF]", "title [title ...]", "create a task",
		"positional arguments:\n  title", "  -p, --project REF  project, project:subproject",
		"--step TITLE", "global output flags: see agtrack --help",
	} {
		if !strings.Contains(verb, want) {
			t.Errorf("task add help lacks %q", want)
		}
	}
	for _, unwanted := range []string{"--db", "--format", "--quiet", "--version"} {
		if strings.Contains(verb, unwanted) {
			t.Errorf("task add help repeats global %s", unwanted)
		}
	}
	noun := p.Help([]string{"task"})
	if !strings.Contains(noun, "verbs:") || !strings.Contains(noun, "  ls (list)") || strings.Count(noun, "list") != 2 {
		t.Errorf("task help verbs:\n%s", noun)
	}
	for _, path := range [][]string{nil, {"task", "ls"}, {"log"}, {"project", "add"}} {
		for _, line := range strings.Split(p.Help(path), "\n") {
			if len(line) > 78 && !strings.HasPrefix(line, "  ") {
				t.Errorf("%v: line over 78 columns: %q", path, line)
			}
		}
	}
	if p.Help([]string{"nope"}) != "" || p.UsageLine([]string{"task", "nope"}) != "" {
		t.Error("unknown path rendered help")
	}
}

func TestUsageLine(t *testing.T) {
	p := agtrackParser()
	tests := []struct {
		path []string
		want string
	}{
		{[]string{"project", "repo"}, "usage: agtrack project repo [-h] {add,rm,ls} ref [url ...]"},
		{[]string{"task", "set"}, "usage: agtrack task set [-h] [--title TITLE [TITLE ...]] [--move REF] ref"},
		{[]string{"here"}, "usage: agtrack here [-h]"},
		{[]string{"t"}, "usage: agtrack task [-h] <verb> ..."},
		{[]string{"task", "add"}, "usage: agtrack task add [-h] [-p REF] [--status STATUS] [--tag TAG]\n" +
			"                        [--note NOTE] [--pr URL] [--step TITLE]\n" +
			"                        [--created WHEN] [--started WHEN] [--closed WHEN]\n" +
			"                        title [title ...]"},
	}
	for _, tt := range tests {
		if got := p.UsageLine(tt.path); got != tt.want {
			t.Errorf("UsageLine(%v)\n got %q\nwant %q", tt.path, got, tt.want)
		}
	}
	if got := p.UsageLine([]string{"log"}); !strings.HasSuffix(got, "[<verb>] ...") {
		t.Errorf("log usage = %q", got)
	}
}

func TestHidden(t *testing.T) {
	p := &Parser{Root: &Command{Name: "x", SubRequired: true, Subs: []*Command{
		{Name: "shown", Help: "visible"},
		{Name: "secret", Aliases: []string{"sss"}, Help: "internal", Hidden: true},
	}}}
	if res := mustParse(t, p, "sss"); res.Path[0] != "secret" {
		t.Errorf("hidden alias did not parse: %v", res.Path)
	}
	if h := p.Help(nil); strings.Contains(h, "secret") {
		t.Errorf("hidden command in help:\n%s", h)
	}
	_, err := p.Parse([]string{"bogus"})
	if err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "'shown'") {
		t.Errorf("invalid choice error = %v", err)
	}
	if d := p.Describe(nil); len(d.Subcommands) != 1 {
		t.Errorf("Describe lists hidden: %+v", d.Subcommands)
	}
}

func TestSubMetavarAndOptionalSub(t *testing.T) {
	p := &Parser{Root: &Command{Name: "x", SubRequired: true, SubMetavar: "<noun>", Subs: []*Command{
		{Name: "log", Flags: []Flag{{Long: "--since"}}, Subs: []*Command{{Name: "add", Args: []Arg{{Name: "body", NArgs: "+"}}}}},
	}}}
	_, err := p.Parse(nil)
	if err == nil || err.Error() != "the following arguments are required: <noun>" {
		t.Errorf("err = %v", err)
	}
	res := mustParse(t, p, "log", "--since", "2d")
	if res.Cmd.Name != "log" || res.Str("since") != "2d" {
		t.Errorf("bare log = %+v", res)
	}
	_, err = p.Parse([]string{"log", "nope"})
	if err == nil || !strings.HasPrefix(err.Error(), "argument [<verb>]: invalid choice: 'nope'") {
		t.Errorf("err = %v", err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		p    *Parser
		want string
	}{
		{"no root", &Parser{}, "no root command"},
		{"dup global", &Parser{Root: &Command{Name: "x"}, Globals: []Flag{{Long: "--help"}}}, "conflicting option string: --help"},
		{"dup flag", &Parser{Root: &Command{Name: "x", Flags: []Flag{{Long: "--a"}, {Short: "-z", Long: "--a"}}}}, "conflicting option string: --a"},
		{"nameless", &Parser{Root: &Command{Name: "x", Flags: []Flag{{Dest: "a"}}}}, "flag without a name"},
		{"bad nargs", &Parser{Root: &Command{Name: "x", Args: []Arg{{Name: "a", NArgs: "2"}}}}, `bad NArgs "2"`},
		{"dup sub", &Parser{Root: &Command{Name: "x", Subs: []*Command{{Name: "a"}, {Name: "b", Aliases: []string{"a"}}}}}, "conflicting subcommand: a"},
		{"deep", &Parser{Root: &Command{Name: "x", Subs: []*Command{{Name: "a", Flags: []Flag{{Long: "bad"}}}}}}, "agtrack a: invalid option string"},
	}
	for _, tt := range tests {
		err := tt.p.Validate()
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
		if _, perr := tt.p.Parse(nil); perr == nil {
			t.Errorf("%s: Parse accepted an invalid table", tt.name)
		} else if _, isUsage := errs.As(perr); isUsage {
			t.Errorf("%s: table error reported as usage error", tt.name)
		}
	}
	if err := agtrackParser().Validate(); err != nil {
		t.Errorf("agtrack table: %v", err)
	}
}

func TestNegativeNumberOptions(t *testing.T) {
	// argparse: once an option looks like a negative number, -5 is an option.
	p := &Parser{Root: &Command{Name: "x", Args: []Arg{{Name: "n", NArgs: "*"}}, Flags: []Flag{{Short: "-1", Kind: Bool, Dest: "one"}}}}
	res := mustParse(t, p, "-1")
	if !res.Bool("one") {
		t.Errorf("-1 not taken as option: %v", res.Values)
	}
	if _, err := p.Parse([]string{"-5"}); err == nil || err.Error() != "unrecognized arguments: -5" {
		t.Errorf("err = %v", err)
	}
}

func TestDescribe(t *testing.T) {
	p := agtrackParser()
	d := p.Describe([]string{"t", "add"})
	if d.Command != "task add" || d.Help != "create a task" || !strings.HasPrefix(d.Usage, "agtrack task add [-h]") {
		t.Errorf("header = %+v", d)
	}
	if !reflect.DeepEqual(d.Args, []ArgSchema{{Name: "title", NArgs: "+", Choices: []string{}}}) {
		t.Errorf("args = %+v", d.Args)
	}
	var names []string
	for _, f := range d.Flags {
		names = append(names, f.Long)
		if f.Global {
			t.Errorf("verb lists global %s", f.Long)
		}
	}
	if strings.Join(names, " ") != "--project --status --tag --note --pr --step --created --started --closed" {
		t.Errorf("flags = %v", names)
	}
	step := d.Flags[5]
	if step.Kind != "append" || !step.Repeatable || step.Metavar != "TITLE" || step.Default != nil {
		t.Errorf("--step = %+v", step)
	}

	ls := p.Describe([]string{"task", "ls"})
	for _, f := range ls.Flags {
		if f.Long == "--order" && (f.Default != "id" || !reflect.DeepEqual(f.Choices, []string{"created", "id", "status", "updated"})) {
			t.Errorf("--order = %+v", f)
		}
		if f.Long == "--limit" && (f.Kind != "positive_int" || f.Short != "-n") {
			t.Errorf("--limit = %+v", f)
		}
	}

	root := p.Describe(nil)
	if root.Command != "" || !root.SubRequired || len(root.Subcommands) != 12 ||
		!reflect.DeepEqual(root.Subcommands[0], SubSchema{Name: "ctx", Aliases: []string{"context", "contexts"}, Help: "contexts: personal, company-a, ..."}) {
		t.Errorf("root = %+v", root)
	}
	globals := 0
	for _, f := range root.Flags {
		if f.Global {
			globals++
		}
	}
	if globals != 11 { // ten globals plus --version
		t.Errorf("root globals = %d", globals)
	}
	if p.Describe([]string{"nope"}) != nil {
		t.Error("Describe of unknown path")
	}

	a, _ := json.Marshal(p.Describe([]string{"sub", "status"}))
	b, _ := json.Marshal(p.Describe([]string{"subtask", "status"}))
	if string(a) != string(b) {
		t.Error("Describe is not deterministic across aliases")
	}
	if !strings.HasPrefix(string(a), `{"command":"sub status","aliases":[],"help":"set an explicit status","usage":`) {
		t.Errorf("json = %s", a)
	}
}

func TestWalk(t *testing.T) {
	p := agtrackParser()
	var paths []string
	leaves := 0
	p.Walk(func(path []string, c *Command) {
		paths = append(paths, strings.Join(path, " "))
		if len(c.Subs) == 0 {
			leaves++
		}
	})
	if paths[0] != "" || paths[1] != "ctx" || paths[2] != "ctx add" {
		t.Errorf("order = %v", paths[:3])
	}
	// §3.2: 66 leaf commands.
	if leaves != 66 {
		t.Errorf("leaves = %d", leaves)
	}
}

func TestPyRepr(t *testing.T) {
	tests := map[string]string{
		"x":      `'x'`,
		"":       `''`,
		"it's":   `"it's"`,
		`a'b"c`:  `'a\'b"c'`,
		`back\`:  `'back\\'`,
		"tab\t":  `'tab\t'`,
		"nl\n":   `'nl\n'`,
		"\x01":   `'\x01'`,
		"héllo":  `'héllo'`,
		"\u200b": `'\u200b'`,
		"\xff":   `'\udcff'`, // argv bytes that are not UTF-8: Python sees surrogates
	}
	for in, want := range tests {
		if got := pyRepr(in); got != want {
			t.Errorf("pyRepr(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestPyInt(t *testing.T) {
	tests := []struct {
		in string
		n  int
		ok bool
	}{
		{"3", 3, true}, {"-3", -3, true}, {"+3", 3, true}, {" 7 ", 7, true}, {"007", 7, true},
		{"1_000", 1000, true}, {"1__0", 0, false}, {"_1", 0, false}, {"1_", 0, false},
		{"", 0, false}, {"-", 0, false}, {"+-3", 0, false}, {"3.5", 0, false}, {"3x", 0, false},
		{"- 3", 0, false}, {"99999999999999999999999", 0, false},
	}
	for _, tt := range tests {
		n, ok := pyInt(tt.in)
		if n != tt.n || ok != tt.ok {
			t.Errorf("pyInt(%q) = %d, %v", tt.in, n, ok)
		}
	}
}

func TestNegNumber(t *testing.T) {
	for in, want := range map[string]bool{"-5": true, "-1.5": true, "-.5": true, "-1abc": true, "-x": false, "-": false, "--5": false, "-.": false, "5": false} {
		if negNumber(in) != want {
			t.Errorf("negNumber(%q) = %v", in, !want)
		}
	}
}

func TestDestFor(t *testing.T) {
	for _, tt := range []struct {
		f    Flag
		want string
	}{
		{Flag{Long: "--all-contexts"}, "all_contexts"},
		{Flag{Short: "-x"}, "x"},
		{Flag{Long: "--by", Dest: "actor_filter"}, "actor_filter"},
		{Flag{Short: "-n", Long: "--limit"}, "limit"},
	} {
		if got := DestFor(tt.f); got != tt.want {
			t.Errorf("DestFor(%+v) = %q", tt.f, got)
		}
	}
}

func TestConcurrentParse(t *testing.T) {
	p := agtrackParser()
	done := make(chan error)
	for i := 0; i < 8; i++ {
		go func() {
			_, err := p.Parse([]string{"task", "ls", "-n", "3"})
			done <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}
