package spec

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/l4sh/agtrack/internal/errs"
)

func TestLookup(t *testing.T) {
	d := exampleDoc(t)
	cases := []struct {
		ref        string
		want       []string
		positional bool
		code       int
	}{
		{"grace-policy", []string{"tokens__rotation"}, false, 0},
		{"lifetimes", []string{"tokens__rotation__lifetimes"}, false, 0},
		{"tokens__rotation", []string{"tokens__rotation"}, false, 0},
		{"root", []string{"root"}, false, 0},
		{"tokens__*", []string{"tokens__rotation", "tokens__rotation__lifetimes", "tokens__revocation"}, false, 0},
		{"*__revocation", []string{"tokens__revocation", "sessions__revocation"}, false, 0},
		{"2.1", []string{"tokens__rotation"}, true, 0},
		{"3", []string{"sessions"}, true, 0},
		{"idle_timeout", []string{"sessions__idle_timeout"}, false, 0},
		{"revocation", nil, false, errs.ExitError},
		{"nope", nil, false, errs.ExitNotFound},
		{"9.9", nil, true, errs.ExitNotFound},
		{"zzz*", nil, false, errs.ExitNotFound},
	}
	for _, c := range cases {
		secs, pos, err := d.Lookup(c.ref)
		var got []string
		for _, s := range secs {
			got = append(got, s.PathSlug)
		}
		if c.code != 0 {
			e, ok := errs.As(err)
			if !ok || e.Code != c.code {
				t.Errorf("%s: want exit %d, got %v", c.ref, c.code, err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) || pos != c.positional {
			t.Errorf("%s: got %v %v %v, want %v %v", c.ref, got, pos, err, c.want, c.positional)
		}
	}
	_, _, err := d.Lookup("revocation")
	e, _ := errs.As(err)
	if e.Token != errs.TokAmbiguous || !reflect.DeepEqual(e.Extra["candidates"], []string{"tokens__revocation", "sessions__revocation"}) {
		t.Errorf("ambiguous error %+v", e)
	}
}

func TestLookupPinBeatsPath(t *testing.T) {
	// After a rename the pin still resolves, and it wins over a path slug
	// that happens to equal it.
	d := mustParse(t, "## Rotation\n\nold place\n\n<!-- agtrack:id rotation -->\n## Rotating refresh tokens\n")
	secs, _, err := d.Lookup("rotation")
	if err != nil || secs[0].PathSlug != "rotating_refresh_tokens" {
		t.Errorf("pin must resolve first: %v %v", secs, err)
	}
}

func TestMintKey(t *testing.T) {
	d := exampleDoc(t)
	taken := d.TakenKeys()
	idle := bySlug(t, d, "sessions__idle_timeout")
	if k := MintKey(idle, taken); k != "idle-timeout" {
		t.Errorf("mint %s", k)
	}
	taken["idle-timeout"] = true
	if k := MintKey(idle, taken); k != "idle-timeout-2" {
		t.Errorf("dedupe %s", k)
	}
	if k := MintKey(bySlug(t, d, "tokens__rotation"), taken); k != "grace-policy" {
		t.Errorf("already pinned %s", k)
	}
	// `sessions` is a path slug, so a bare key would shadow it.
	if k := MintKey(bySlug(t, d, "sessions"), taken); k != "sessions-2" {
		t.Errorf("shadowing %s", k)
	}
	if k := MintKey(&d.Sections[0], taken); k != RootSlug {
		t.Errorf("root %s", k)
	}
}

// lineDiff returns the lines added to a to produce b, failing unless b is a
// with lines inserted and nothing else changed.
func lineDiff(t *testing.T, a, b []byte) []string {
	t.Helper()
	al := strings.SplitAfter(string(a), "\n")
	bl := strings.SplitAfter(string(b), "\n")
	var added []string
	i := 0
	for _, l := range bl {
		if i < len(al) && al[i] == l {
			i++
			continue
		}
		added = append(added, l)
	}
	if i != len(al) {
		t.Fatalf("original lines changed or removed")
	}
	return added
}

func TestInsertPin(t *testing.T) {
	src, _ := os.ReadFile("testdata/example-spec.md")
	d := exampleDoc(t)
	sec := bySlug(t, d, "tokens__revocation")
	out, err := InsertPin(src, sec, "family-revoke")
	if err != nil {
		t.Fatal(err)
	}
	if added := lineDiff(t, src, out); !reflect.DeepEqual(added, []string{"<!-- agtrack:id family-revoke -->\n"}) {
		t.Fatalf("added %q", added)
	}
	if want := append(append(append([]byte{}, src[:sec.HeadingByteStart]...), "<!-- agtrack:id family-revoke -->\n"...), src[sec.HeadingByteStart:]...); !bytes.Equal(out, want) {
		t.Fatal("not a pure splice at HeadingByteStart")
	}
	d2, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(slugs(d2), slugs(d)) {
		t.Errorf("structure changed: %v", slugs(d2))
	}
	s2 := bySlug(t, d2, "tokens__revocation")
	if s2.PinKey != "family-revoke" || s2.Body != sec.Body || s2.BodySHA != sec.BodySHA {
		t.Errorf("after pin %+v", s2)
	}
	if prev := bySlug(t, d2, "tokens__rotation__lifetimes"); prev.Body != bySlug(t, d, "tokens__rotation__lifetimes").Body {
		t.Error("previous section body changed")
	}

	// Idempotent.
	again, err := InsertPin(out, s2, "family-revoke")
	if err != nil || !bytes.Equal(again, out) {
		t.Errorf("not idempotent: %v", err)
	}
	// Different key on a pinned section, a block, a stale section, a duplicate key.
	if _, err := InsertPin(out, s2, "other"); !isCode(err, errs.ExitConflict) {
		t.Errorf("repin: %v", err)
	}
	if _, err := InsertPin(src, bySlug(t, d, "tokens__rotation__lifetimes"), "x"); err == nil {
		t.Error("block pinned")
	}
	if _, err := InsertPin(src, bySlug(t, d, "sessions"), "bad key"); err == nil {
		t.Error("invalid key accepted")
	}
	if _, err := InsertPin(src, bySlug(t, d, "sessions"), "grace-policy"); err == nil || !strings.Contains(err.Error(), "duplicate pin id") {
		t.Errorf("duplicate key: %v", err)
	}
	stale := *bySlug(t, d, "sessions")
	stale.HeadingByteStart += 3
	if _, err := InsertPin(src, &stale, "s"); !isCode(err, errs.ExitConflict) {
		t.Errorf("stale offset: %v", err)
	}
	// The preamble's identity is structural: nothing to write.
	if o, err := InsertPin(src, &d.Sections[0], "root"); err != nil || !bytes.Equal(o, src) {
		t.Errorf("root: %v", err)
	}
}

func TestInsertPinVariants(t *testing.T) {
	for name, c := range map[string]struct {
		src, slug, want string
	}{
		"crlf":          {"\xEF\xBB\xBFIntro\r\n\r\n## A\r\ntext\r\n## B\r\n", "b", "\xEF\xBB\xBFIntro\r\n\r\n## A\r\ntext\r\n<!-- agtrack:id k -->\r\n## B\r\n"},
		"setext":        {"Intro\n\nHead\n----\n\nx\n", "head", "Intro\n\n<!-- agtrack:id k -->\nHead\n----\n\nx\n"},
		"after-para":    {"## A\npara\n## B\n", "b", "## A\npara\n<!-- agtrack:id k -->\n## B\n"},
		"indented":      {"   ## A\n", "a", "<!-- agtrack:id k -->\n   ## A\n"},
		"no-newline":    {"## A", "a", "<!-- agtrack:id k -->\n## A"},
		"elided-title":  {"# Title\n\n## A\n", "root", "<!-- agtrack:id k -->\n# Title\n\n## A\n"},
		"after-frontmt": {"---\na: b\n---\n## A\n", "a", "---\na: b\n---\n<!-- agtrack:id k -->\n## A\n"},
	} {
		t.Run(name, func(t *testing.T) {
			d := mustParse(t, c.src)
			sec := bySlug(t, d, c.slug)
			out, err := InsertPin([]byte(c.src), sec, "k")
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != c.want {
				t.Fatalf("got %q\nwant %q", out, c.want)
			}
			if got := bySlug(t, mustParse(t, string(out)), c.slug).PinKey; got != "k" {
				t.Errorf("reparse pin %q", got)
			}
		})
	}
}

func isCode(err error, code int) bool {
	e, ok := errs.As(err)
	return ok && e.Code == code
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "auth.md")
	orig := []byte("## A\r\n")
	if err := os.WriteFile(p, orig, 0o640); err != nil {
		t.Fatal(err)
	}
	// Expected hash is over the normalised bytes, as Doc.SHA is.
	if err := WriteFileAtomic(p, []byte("new\n"), SHA(orig)); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	fi, _ := os.Stat(p)
	if string(got) != "new\n" || fi.Mode().Perm() != 0o640 {
		t.Errorf("content %q mode %v", got, fi.Mode())
	}
	// Someone edited it since: refuse and leave it alone.
	if err := WriteFileAtomic(p, []byte("clobber\n"), SHA(orig)); !isCode(err, errs.ExitConflict) {
		t.Errorf("want conflict, got %v", err)
	}
	if got, _ := os.ReadFile(p); string(got) != "new\n" {
		t.Error("file modified on conflict")
	}
	// Removed since read.
	if err := WriteFileAtomic(filepath.Join(dir, "gone.md"), []byte("x"), SHA(orig)); !isCode(err, errs.ExitConflict) {
		t.Errorf("missing: %v", err)
	}
	// No check requested: creates.
	if err := WriteFileAtomic(filepath.Join(dir, "fresh.md"), []byte("x"), ""); err != nil {
		t.Error(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".agtrack-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}
