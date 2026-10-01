package skill

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/l4sh/agtrack"
)

func files(t *testing.T) map[string][]byte {
	t.Helper()
	f, err := Files(agtrack.Skills)
	if err != nil || len(f["SKILL.md"]) == 0 {
		t.Fatalf("embedded skill missing: %v", err)
	}
	return f
}

func TestTargetsDefaultToPresentHarnesses(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	ts, err := Targets(home, nil, nil)
	if err != nil || len(ts) != 1 || ts[0].Harness != "claude" {
		t.Fatalf("targets = %+v, %v", ts, err)
	}
	all, _ := Targets(home, []string{"all"}, nil)
	if len(all) != len(Harnesses) {
		t.Fatalf("all = %d", len(all))
	}
	if _, err := Targets(home, []string{"cursor"}, nil); err == nil {
		t.Fatal("unverified harness accepted")
	}
	if ts, _ := Targets(home, []string{"gemini"}, nil); ts[0].Dir != filepath.Join(home, ".gemini/config/skills", Name) {
		t.Fatalf("alias resolved to %s", ts[0].Dir)
	}
}

func TestInstallLifecycle(t *testing.T) {
	f := files(t)
	target := Target{Dir: filepath.Join(t.TempDir(), "skills", Name), Present: true}
	if s, _ := Inspect(target, f); s != Missing {
		t.Fatal(s)
	}
	if _, err := Install(target, f, "2.0.0", false); err != nil {
		t.Fatal(err)
	}
	if s, _ := Inspect(target, f); s != Current {
		t.Fatalf("after install: %s", s)
	}
	if _, err := os.Stat(filepath.Join(target.Dir, "references", "spec-review.md")); err != nil {
		t.Fatal("reference files not installed")
	}
	// A newer binary with different content sees an outdated install and updates it.
	newer := map[string][]byte{}
	for k, v := range f {
		newer[k] = v
	}
	newer["SKILL.md"] = append([]byte{}, append(f["SKILL.md"], '\n')...)
	if s, _ := Inspect(target, newer); s != Outdated {
		t.Fatalf("expected outdated, got %s", s)
	}
	// Local edits are reported and protected.
	os.WriteFile(filepath.Join(target.Dir, "SKILL.md"), []byte("mine"), 0o644)
	if s, _ := Inspect(target, f); s != Modified {
		t.Fatalf("expected modified, got %s", s)
	}
	if _, err := Install(target, f, "2.0.0", false); err == nil {
		t.Fatal("overwrote a local edit without --force")
	}
	if _, err := Install(target, f, "2.0.0", true); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(target, f, false); err != nil {
		t.Fatal(err)
	}
	if s, _ := Inspect(target, f); s != Missing {
		t.Fatalf("after uninstall: %s", s)
	}
}

func TestForeignSymlinkIsProtectedAndNeverFollowed(t *testing.T) {
	f := files(t)
	base := t.TempDir()
	elsewhere := filepath.Join(base, "shared-skill")
	os.MkdirAll(elsewhere, 0o755)
	os.WriteFile(filepath.Join(elsewhere, "SKILL.md"), []byte("old skill"), 0o644)
	target := Target{Dir: filepath.Join(base, "skills", Name), Present: true}
	os.MkdirAll(filepath.Dir(target.Dir), 0o755)
	os.Symlink(elsewhere, target.Dir)
	if s, _ := Inspect(target, f); s != Foreign {
		t.Fatalf("symlink seen as %s", s)
	}
	if _, err := Install(target, f, "2.0.0", false); err == nil {
		t.Fatal("replaced a foreign symlink without --force")
	}
	if _, err := Install(target, f, "2.0.0", true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(elsewhere, "SKILL.md")); string(b) != "old skill" {
		t.Fatal("the symlink's target was modified")
	}
	if s, _ := Inspect(target, f); s != Current {
		t.Fatalf("after forced install: %s", s)
	}
}
