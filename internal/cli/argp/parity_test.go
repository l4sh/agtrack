package argp

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/l4sh/agtrack/internal/errs"
)

// pyCase is one argv run through the Python CLI's argparse by
// testdata/gen_python_cases.py.
type pyCase struct {
	Argv   []string       `json:"argv"`
	Typed  []string       `json:"typed"`
	Values map[string]any `json:"values"`
	Error  *struct {
		Prog    string `json:"prog"`
		Message string `json:"message"`
	} `json:"error"`
	Help   *string `json:"help"`
	Exit   *int    `json:"exit"`
	Stdout string  `json:"stdout"`
}

// noteVerbs override the noun in Python's namespace via set_defaults.
var noteVerbs = map[string]bool{"note": true, "notes": true, "tag": true, "untag": true}

func TestParityWithPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/python_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []pyCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	p := agtrackParser()
	for _, c := range cases {
		t.Run(strings.Join(c.Argv, " "), func(t *testing.T) {
			res, err := p.Parse(c.Argv)
			var help *HelpRequest
			errors.As(err, &help)
			switch {
			case c.Error != nil:
				e, ok := errs.As(err)
				if !ok {
					t.Fatalf("want usage error %q, got res=%v err=%v", c.Error.Message, res, err)
				}
				if e.Code != errs.ExitUsage || e.Token != errs.TokUsage {
					t.Errorf("code/token = %d/%s", e.Code, e.Token)
				}
				if e.Message != c.Error.Message {
					t.Errorf("message\n got %q\nwant %q", e.Message, c.Error.Message)
				}
				path, _ := ErrorPath(err)
				if got := p.progFor(path); got != c.Error.Prog {
					t.Errorf("prog = %q, want %q", got, c.Error.Prog)
				}
			case c.Help != nil:
				if help == nil || help.Version {
					t.Fatalf("want help for %q, got res=%v err=%v", *c.Help, res, err)
				}
				if got := p.progFor(help.Path); got != *c.Help {
					t.Errorf("help prog = %q, want %q", got, *c.Help)
				}
				if !strings.HasPrefix(help.Text, "usage: "+*c.Help) {
					t.Errorf("help text starts %q", help.Text[:min(40, len(help.Text))])
				}
			case c.Exit != nil:
				if help == nil || !help.Version || help.Text != c.Stdout {
					t.Fatalf("want version %q, got res=%v err=%v", c.Stdout, res, err)
				}
			default:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				typed, want := res.Typed, c.Typed
				if len(want) == 2 && noteVerbs[want[1]] {
					typed, want = typed[1:], want[1:]
				}
				if !reflect.DeepEqual(typed, want) {
					t.Errorf("typed = %v, want %v", res.Typed, c.Typed)
				}
				if got := normalize(t, res.Values); !reflect.DeepEqual(got, normalize(t, c.Values)) {
					t.Errorf("values\n got %v\nwant %v", got, c.Values)
				}
			}
		})
	}
}

// normalize round-trips through JSON so Go and Python values compare.
func normalize(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
