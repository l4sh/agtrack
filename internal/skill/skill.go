// Package skill installs the agent skill embedded in the binary into the
// skill directories of agent harnesses (Claude Code, Codex, Antigravity, ...).
//
// Installs are copies, not symlinks: a user who downloaded a release has no
// checkout to link to. Each install carries a marker file recording the
// version and content hash, so status can tell current, outdated, modified
// and foreign installs apart, and nothing we did not install is overwritten
// without --force.
package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
)

// Name is the skill's directory name inside a harness skills directory.
const Name = "agtrack"

// Marker is written next to SKILL.md in every install we make.
const Marker = ".agtrack-skill"

// Harness is an agent harness and where it reads user-level skills.
type Harness struct {
	Name   string // flag value
	Label  string
	Home   string // directory (relative to $HOME) whose presence means the harness is installed
	Skills string // skills directory relative to $HOME
}

// Harnesses are the harnesses whose skill directories are verified. Add one
// only after confirming where it reads SKILL.md skills from; `--dir` covers
// anything else.
var Harnesses = []Harness{
	{Name: "claude", Label: "Claude Code", Home: ".claude", Skills: ".claude/skills"},
	{Name: "codex", Label: "Codex CLI", Home: ".codex", Skills: ".codex/skills"},
	{Name: "agy", Label: "Antigravity / Gemini CLI", Home: ".gemini", Skills: ".gemini/config/skills"},
}

// Lookup finds a harness by name or alias.
func Lookup(name string) (Harness, bool) {
	switch strings.ToLower(name) {
	case "claude-code", "claudecode":
		name = "claude"
	case "codex-cli":
		name = "codex"
	case "antigravity", "gemini", "gemini-cli":
		name = "agy"
	}
	for _, h := range Harnesses {
		if h.Name == strings.ToLower(name) {
			return h, true
		}
	}
	return Harness{}, false
}

// Target is one place a skill can be installed.
type Target struct {
	Harness string // "" for an explicit --dir
	Label   string
	Dir     string // the skill's own directory: <skills dir>/agtrack
	Present bool   // the harness is installed (or the dir was given explicitly)
}

// Targets resolves the install targets. With no agents and no dirs, every
// harness present under home is chosen.
func Targets(home string, agents, dirs []string) ([]Target, error) {
	var out []Target
	add := func(h Harness) {
		_, err := os.Stat(filepath.Join(home, h.Home))
		out = append(out, Target{Harness: h.Name, Label: h.Label, Dir: filepath.Join(home, h.Skills, Name), Present: err == nil})
	}
	for _, a := range agents {
		if a == "all" {
			for _, h := range Harnesses {
				add(h)
			}
			continue
		}
		h, ok := Lookup(a)
		if !ok {
			var names []string
			for _, h := range Harnesses {
				names = append(names, h.Name)
			}
			return nil, errs.New("unknown agent harness: %s (known: %s, all; use --dir for any other skills directory)", a, strings.Join(names, ", "))
		}
		add(h)
	}
	for _, d := range dirs {
		abs, err := filepath.Abs(d)
		if err != nil {
			return nil, err
		}
		out = append(out, Target{Label: "directory", Dir: filepath.Join(abs, Name), Present: true})
	}
	if len(agents) == 0 && len(dirs) == 0 {
		for _, h := range Harnesses {
			if _, err := os.Stat(filepath.Join(home, h.Home)); err == nil {
				add(h)
			}
		}
	}
	return dedupe(out), nil
}

func dedupe(ts []Target) []Target {
	seen := map[string]bool{}
	var out []Target
	for _, t := range ts {
		if !seen[t.Dir] {
			seen[t.Dir] = true
			out = append(out, t)
		}
	}
	return out
}

// Files lists the embedded skill files (relative paths) and their content.
func Files(fsys fs.FS) (map[string][]byte, error) {
	root := path.Join("skills", Name)
	files := map[string][]byte{}
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		files[strings.TrimPrefix(p, root+"/")] = b
		return nil
	})
	return files, err
}

// Hash is a stable digest of a file set.
func Hash(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%s\x00%d\x00", n, len(files[n]))
		h.Write(files[n])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// State is what is found at a target.
type State string

const (
	Missing  State = "not installed"
	Current  State = "current"
	Outdated State = "outdated"
	Modified State = "modified"
	Foreign  State = "not installed by agtrack"
)

// Inspect reports the state of a target against the embedded files.
func Inspect(t Target, files map[string][]byte) (State, string) {
	info, err := os.Lstat(t.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return Missing, ""
	}
	if err != nil {
		return Foreign, err.Error()
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		dest, _ := os.Readlink(t.Dir)
		return Foreign, "symlink to " + dest
	}
	marker, err := os.ReadFile(filepath.Join(t.Dir, Marker))
	if err != nil {
		return Foreign, "no " + Marker + " marker"
	}
	installedVersion, installedHash := parseMarker(string(marker))
	onDisk, err := readDir(t.Dir)
	if err != nil {
		return Foreign, err.Error()
	}
	if Hash(onDisk) != installedHash {
		return Modified, "edited since agtrack " + installedVersion + " installed it"
	}
	if installedHash != Hash(files) {
		return Outdated, "installed by agtrack " + installedVersion
	}
	return Current, "agtrack " + installedVersion
}

func parseMarker(s string) (version, hash string) {
	for _, line := range strings.Split(s, "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch strings.TrimSpace(k) {
		case "version":
			version = strings.TrimSpace(v)
		case "hash":
			hash = strings.TrimSpace(v)
		}
	}
	return
}

func readDir(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == Marker {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	return files, err
}

// Install writes the files into t.Dir. A foreign or locally modified install
// is refused unless force is set.
func Install(t Target, files map[string][]byte, version string, force bool) (State, error) {
	before, why := Inspect(t, files)
	switch before {
	case Current:
		return before, nil
	case Foreign, Modified:
		if !force {
			return before, errs.Conflict("%s: %s (%s); pass --force to replace it", t.Dir, before, why)
		}
	}
	parent := filepath.Dir(t.Dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return before, err
	}
	// Build the new copy beside the target, then swap it in.
	tmp, err := os.MkdirTemp(parent, "."+Name+"-install-")
	if err != nil {
		return before, err
	}
	defer os.RemoveAll(tmp)
	for rel, data := range files {
		p := filepath.Join(tmp, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return before, err
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return before, err
		}
	}
	marker := fmt.Sprintf("# written by `agtrack skill install`; edits are reported by `agtrack skill status`\nversion=%s\nhash=%s\n", version, Hash(files))
	if err := os.WriteFile(filepath.Join(tmp, Marker), []byte(marker), 0o644); err != nil {
		return before, err
	}
	if before != Missing {
		if err := os.RemoveAll(t.Dir); err != nil {
			return before, err
		}
	}
	if err := os.Rename(tmp, t.Dir); err != nil {
		return before, err
	}
	return before, nil
}

// Uninstall removes an install we made; anything else needs force.
func Uninstall(t Target, files map[string][]byte, force bool) (State, error) {
	before, why := Inspect(t, files)
	switch before {
	case Missing:
		return before, nil
	case Foreign, Modified:
		if !force {
			return before, errs.Conflict("%s: %s (%s); pass --force to remove it", t.Dir, before, why)
		}
	}
	info, err := os.Lstat(t.Dir)
	if err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return before, os.Remove(t.Dir) // remove the link, never its target
	}
	return before, os.RemoveAll(t.Dir)
}
