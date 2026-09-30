package render

import (
	"bytes"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/l4sh/agtrack/internal/row"
)

// TestPyFloatReprAgainstPython pins pyFloatRepr against actual Python
// repr(float) output (python3 -c 'print(repr(v))') for boundary cases:
// the exponent-notation cutoffs (decpt <= -4 or > 16), subnormals, the
// float64 extremes, negative zero, and values needing every significant
// digit. Bit patterns avoid any ambiguity about which float64 is meant.
func TestPyFloatReprAgainstPython(t *testing.T) {
	cases := []struct {
		bits uint64
		want string
	}{
		{4591870180066957722, "0.1"},
		{4596373779694328218, "0.2"},
		{4599075939470750515, "0.3"},
		{4609434218613702656, "1.5"},
		{4613205983301625446, "2.675"},
		{4653144502051863213, "1234.5678"},
		{13876516538906639021, "-1234.5678"},
		{6103021453049119613, "1e+100"},
		{3110860544497550640, "1e-100"},
		{9094988921128908188, "1e+300"},
		{118622047889322841, "1e-300"},
		{9218868437227405311, "1.7976931348623157e+308"}, // math.MaxFloat64
		{4503599627370496, "2.2250738585072014e-308"},    // smallest normal
		{1, "5e-324"},                                    // smallest subnormal
		{4846369599423283199, "9999999999999998.0"},
		{4817745636528479846, "123456789012345.6"},
		{4599075939470750516, "0.30000000000000004"},
		{4636737291354636288, "100.0"},
		{9223372036854775808, "-0.0"},
		{4613937818241073152, "3.0"},
		{4619567317775286272, "7.0"},
	}
	for _, c := range cases {
		f := math.Float64frombits(c.bits)
		if got := pyFloatRepr(f); got != c.want {
			t.Errorf("pyFloatRepr(%v) = %q, want %q", f, got, c.want)
		}
	}
}

func TestTypedNilSlicesRenderAsEmptyList(t *testing.T) {
	var nilRows []*row.Row
	var nilAny []any
	var nilStrings []string

	for _, tc := range []struct {
		name string
		v    any
	}{
		{"[]*row.Row(nil)", nilRows},
		{"[]any(nil)", nilAny},
		{"[]string(nil)", nilStrings},
	} {
		t.Run(tc.name+"/json", func(t *testing.T) {
			got := string(JSONCompact(tc.v))
			if got != "[]" {
				t.Errorf("JSONCompact(%s) = %q, want []", tc.name, got)
			}
		})
		t.Run(tc.name+"/yaml", func(t *testing.T) {
			got := ToYAML(tc.v)
			if got != "[]\n" {
				t.Errorf("ToYAML(%s) = %q, want []\\n", tc.name, got)
			}
		})
	}

	// An untyped nil interface value is still null.
	var untyped any
	if got := string(JSONCompact(untyped)); got != "null" {
		t.Errorf("JSONCompact(nil) = %q, want null", got)
	}
	if got := ToYAML(untyped); got != "null\n" {
		t.Errorf("ToYAML(nil) = %q, want null\\n", got)
	}

	// A row value holding a typed-nil slice must also come out as [].
	r := row.Of("tags", nilStrings)
	if got := string(JSONCompact(r)); got != `{"tags":[]}` {
		t.Errorf("JSONCompact(row with nil []string) = %q", got)
	}
}

func TestIntAcceptedAlongsideInt64(t *testing.T) {
	if got, want := Cell(5), "5"; got != want {
		t.Errorf("Cell(int) = %q, want %q", got, want)
	}
	if got, want := string(JSONCompact(5)), "5"; got != want {
		t.Errorf("JSONCompact(int) = %q, want %q", got, want)
	}
	if got, want := ToYAML(5), "5\n"; got != want {
		t.Errorf("ToYAML(int) = %q, want %q", got, want)
	}
}

func TestJSONLFormat(t *testing.T) {
	rows := []*row.Row{
		row.Of("id", int64(1), "status", "todo"),
		row.Of("id", int64(2), "status", "done"),
	}
	var buf bytes.Buffer
	if err := Emit(&buf, rows, []string{"id", "status"}, Options{Format: "jsonl"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	want := `{"id":1,"status":"todo"}` + "\n" + `{"id":2,"status":"done"}` + "\n"
	if buf.String() != want {
		t.Errorf("got %q want %q", buf.String(), want)
	}
}

func TestJSONLEmptyList(t *testing.T) {
	var buf bytes.Buffer
	if err := Emit(&buf, nil, []string{"id"}, Options{Format: "jsonl"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no lines for an empty listing, got %q", buf.String())
	}
}

func TestEmitObjectJSONLNonList(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitObject(&buf, row.Of("a", int64(1)), "jsonl"); err != nil {
		t.Fatalf("EmitObject: %v", err)
	}
	want := `{"a":1}` + "\n"
	if buf.String() != want {
		t.Errorf("got %q want %q", buf.String(), want)
	}
}

func TestEmitUnknownFormat(t *testing.T) {
	var buf bytes.Buffer
	err := Emit(&buf, nil, nil, Options{Format: "xml"})
	if err == nil {
		t.Fatal("expected an error for an unknown format")
	}
}

func TestColorEnabledEnvOverride(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if ColorEnabled(os.Stdout) {
		t.Error("NO_COLOR should disable color even on a real terminal")
	}
	os.Unsetenv("NO_COLOR")
	t.Setenv("AGTRACK_NO_COLOR", "1")
	if ColorEnabled(os.Stdout) {
		t.Error("AGTRACK_NO_COLOR should disable color")
	}
}

func TestColorEnabledNonFileWriter(t *testing.T) {
	var buf bytes.Buffer
	if ColorEnabled(&buf) {
		t.Error("a non-*os.File writer is never a terminal")
	}
}

func TestEmitDetail(t *testing.T) {
	sections := []Section{
		{"Title", "hello"},
		{"Empty", ""},
		{"Nil", nil},
		{"Steps", []string{"a", "b"}},
		{"EmptySteps", []string{}},
	}
	var buf bytes.Buffer
	color := false
	if err := EmitDetail(&buf, sections, &color); err != nil {
		t.Fatalf("EmitDetail: %v", err)
	}
	want := "Title: hello\nSteps:\n  a\n  b\n"
	if buf.String() != want {
		t.Errorf("got %q want %q", buf.String(), want)
	}
}

func TestEmitDetailColor(t *testing.T) {
	sections := []Section{{"Title", "hello"}}
	var buf bytes.Buffer
	color := true
	if err := EmitDetail(&buf, sections, &color); err != nil {
		t.Fatalf("EmitDetail: %v", err)
	}
	want := dimCode + "Title:" + resetCode + " hello\n"
	if buf.String() != want {
		t.Errorf("got %q want %q", buf.String(), want)
	}
}

func TestLeanDropsKeysAtEveryLevel(t *testing.T) {
	child := row.Of("id", int64(1), "created", "2026-01-01", "title", "sub")
	parent := row.Of("id", int64(9), "created", "2026-01-01", "subtasks", []*row.Row{child})

	out := Lean(parent, "created").(*row.Row)
	if out.Has("created") {
		t.Error("expected top-level created to be dropped")
	}
	subs := out.Get("subtasks").([]*row.Row)
	if subs[0].Has("created") {
		t.Error("expected nested created to be dropped")
	}
	if !subs[0].Has("title") {
		t.Error("expected unrelated nested keys to survive")
	}

	// Lean must not mutate the original.
	if !parent.Has("created") {
		t.Error("Lean mutated its input")
	}
	origChild := parent.Get("subtasks").([]*row.Row)[0]
	if !origChild.Has("created") {
		t.Error("Lean mutated a nested row in its input")
	}
}

func TestBoundWithinLimitReturnsAllRowsNoManifest(t *testing.T) {
	rows := []*row.Row{
		row.Of("id", int64(1), "title", "a"),
		row.Of("id", int64(2), "title", "b"),
	}
	kept, manifest := Bound(rows, 10000, "jsonl", []string{"id", "title"}, false)
	if len(kept) != 2 || manifest != nil {
		t.Fatalf("expected all rows kept with no manifest, got %d rows manifest=%v", len(kept), manifest)
	}
}

func TestBoundTrimsAndReportsOmission(t *testing.T) {
	rows := []*row.Row{
		row.Of("id", int64(1), "title", strings.Repeat("x", 50)),
		row.Of("id", int64(2), "title", strings.Repeat("y", 50)),
		row.Of("id", int64(3), "title", strings.Repeat("z", 50)),
	}
	kept, manifest := Bound(rows, 70, "jsonl", []string{"id", "title"}, false)
	if len(kept) == 0 || len(kept) >= len(rows) {
		t.Fatalf("expected a partial keep, got %d of %d", len(kept), len(rows))
	}
	if manifest == nil {
		t.Fatal("expected a non-nil manifest when rows are omitted")
	}
	if manifest.Int("total") != 3 {
		t.Errorf("manifest total = %d, want 3", manifest.Int("total"))
	}
	if manifest.Int("returned") != int64(len(kept)) {
		t.Errorf("manifest returned = %d, want %d", manifest.Int("returned"), len(kept))
	}
	if manifest.Int("omitted") != 3-int64(len(kept)) {
		t.Errorf("manifest omitted = %d, want %d", manifest.Int("omitted"), 3-int64(len(kept)))
	}
	if manifest.Str("retrieve") == "" {
		t.Error("expected a non-empty retrieve hint")
	}
}

func TestBoundFirstRowTooBigReturnsZeroRowsWithSizeInHint(t *testing.T) {
	rows := []*row.Row{
		row.Of("id", int64(1), "title", strings.Repeat("x", 500)),
	}
	kept, manifest := Bound(rows, 10, "jsonl", []string{"id", "title"}, false)
	if len(kept) != 0 {
		t.Fatalf("expected zero rows, got %d", len(kept))
	}
	if manifest == nil {
		t.Fatal("expected a manifest naming the oversized row")
	}
	if manifest.Int("returned") != 0 {
		t.Errorf("returned = %d, want 0", manifest.Int("returned"))
	}
	if manifest.Int("omitted") != 1 {
		t.Errorf("omitted = %d, want 1", manifest.Int("omitted"))
	}
}

func TestEmitJSONWithMaxBytesWrapsOmission(t *testing.T) {
	rows := []*row.Row{
		row.Of("id", int64(1), "title", strings.Repeat("x", 50)),
		row.Of("id", int64(2), "title", strings.Repeat("y", 50)),
		row.Of("id", int64(3), "title", strings.Repeat("z", 50)),
	}
	var buf bytes.Buffer
	if err := Emit(&buf, rows, []string{"id", "title"}, Options{Format: "json", MaxBytes: 80}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"items"`) || !strings.Contains(out, `"omitted"`) {
		t.Errorf("expected a wrapper with items/omitted, got %s", out)
	}
}

func TestEmitJSONLWithMaxBytesAppendsTrailingOmittedLine(t *testing.T) {
	rows := []*row.Row{
		row.Of("id", int64(1), "title", strings.Repeat("x", 50)),
		row.Of("id", int64(2), "title", strings.Repeat("y", 50)),
		row.Of("id", int64(3), "title", strings.Repeat("z", 50)),
	}
	var buf bytes.Buffer
	if err := Emit(&buf, rows, []string{"id", "title"}, Options{Format: "jsonl", MaxBytes: 70}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, `{"_omitted":`) {
		t.Errorf("expected trailing _omitted line, got %q", last)
	}
}

func TestSelectFieldsSuccess(t *testing.T) {
	rows := []*row.Row{row.Of("id", int64(1), "status", "todo")}
	got, err := SelectFields(rows, []string{"id", "status"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "id" || got[1] != "status" {
		t.Errorf("got %v", got)
	}
}
