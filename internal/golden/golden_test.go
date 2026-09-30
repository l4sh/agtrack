// Package golden replays the parity corpus captured from the Python build
// (scripts/golden/, testdata/golden/) against the Go binary.
//
// Until cmd/agtrack exists this whole test skips cleanly: there is nothing to
// build and nothing to compare against. Once it exists, TestGolden builds it
// once, then replays every testdata/golden/<scenario>.json in its own fresh
// scratch database, comparing exit code, stdout and stderr against what the
// Python build produced. See go-rewrite-spec.md §4 for the parity contract
// this enforces, and scripts/golden/README.md for how the corpus is made.
package golden

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	updateFlag   = flag.Bool("update", false, "rewrite expected output for steps listed in testdata/golden/deltas.json")
	scenarioFlag = flag.String("scenario", "", "replay only the scenario with this name")
)

// Step mirrors one entry of testdata/golden/<scenario>.json's "steps" array.
// See scripts/golden/capture.py for how these fields are produced.
type Step struct {
	Argv      []string          `json:"argv"`
	Cwd       string            `json:"cwd"`
	Now       string            `json:"now"`
	Env       map[string]string `json:"env"`
	Stdin     string            `json:"stdin"`
	Stdout    string            `json:"stdout"`
	Stderr    string            `json:"stderr"`
	Exit      int               `json:"exit"`
	Normalize []string          `json:"normalize"`
	Tags      []string          `json:"tags"`
}

// Scenario mirrors a whole testdata/golden/<scenario>.json file.
type Scenario struct {
	Scenario string  `json:"scenario"`
	Actor    *string `json:"actor"`
	Steps    []Step  `json:"steps"`
}

func TestGolden(t *testing.T) {
	root := repoRoot(t)
	mainPath := filepath.Join(root, "cmd", "agtrack", "main.go")
	if _, err := os.Stat(mainPath); err != nil {
		t.Skip("cmd/agtrack does not exist yet; nothing to replay the golden corpus against")
	}

	binPath := filepath.Join(t.TempDir(), "agtrack")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/agtrack")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/agtrack: %v\n%s", err, out)
	}

	deltasPath := filepath.Join(root, "testdata", "golden", "deltas.json")
	deltas := loadDeltas(t, deltasPath)

	dir := filepath.Join(root, "testdata", "golden")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || e.Name() == "deltas.json" {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		t.Fatalf("no scenarios found in %s", dir)
	}

	for _, path := range paths {
		path := path
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		var sc Scenario
		if err := json.Unmarshal(raw, &sc); err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		if *scenarioFlag != "" && sc.Scenario != *scenarioFlag {
			continue
		}
		t.Run(sc.Scenario, func(t *testing.T) {
			replayScenario(t, binPath, path, &sc, deltas)
		})
	}
}

func replayScenario(t *testing.T, binPath, path string, sc *Scenario, deltas map[string]string) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	mustMkdir(t, home)
	dbPath := filepath.Join(tmp, "db", "agtrack.db")
	mustMkdir(t, filepath.Dir(dbPath))

	plainDir := filepath.Join(tmp, "cwd-plain")
	mustMkdir(t, plainDir)
	cwdDirs := map[string]string{"plain": plainDir}
	for _, step := range sc.Steps {
		if step.Cwd == "repo" && cwdDirs["repo"] == "" {
			repoDir := filepath.Join(tmp, "cwd-repo")
			mustMkdir(t, repoDir)
			runGit(t, repoDir, "init", "-q", "-b", "main")
			runGit(t, repoDir, "remote", "add", "origin", "git@github.com:acme/widgets.git")
			cwdDirs["repo"] = repoDir
		}
	}

	baseEnv := []string{
		"PATH=" + os.Getenv("PATH"),
		"TZ=UTC",
		"LC_ALL=C.UTF-8",
		"HOME=" + home,
		"AGTRACK_DB=" + dbPath,
	}
	if sc.Actor != nil && *sc.Actor != "" {
		baseEnv = append(baseEnv, "AGTRACK_ACTOR="+*sc.Actor)
	}

	changed := false
	for i := range sc.Steps {
		step := sc.Steps[i]
		if hasTag(step.Tags, "skip-replay") || containsHelpFlag(step.Argv) {
			continue
		}

		env := append([]string{}, baseEnv...)
		for k, v := range step.Env {
			env = append(env, k+"="+v)
		}
		env = append(env, "AGTRACK_NOW="+step.Now)

		cwd := cwdDirs[step.Cwd]
		if cwd == "" {
			cwd = cwdDirs["plain"]
		}

		var stdout, stderr bytes.Buffer
		cmd := exec.Command(binPath, step.Argv...)
		cmd.Dir = cwd
		cmd.Env = env
		if step.Stdin != "" {
			cmd.Stdin = strings.NewReader(step.Stdin)
		}
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		exitCode := 0
		if err := cmd.Run(); err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("step %d %v: could not run binary: %v", i, step.Argv, err)
			}
			exitCode = ee.ExitCode()
		}

		key := fmt.Sprintf("%s#%d", sc.Scenario, i)
		note, hasDelta := deltas[key]

		if *updateFlag && hasDelta {
			sc.Steps[i].Stdout = stdout.String()
			sc.Steps[i].Stderr = stderr.String()
			sc.Steps[i].Exit = exitCode
			changed = true
			continue
		}

		t.Run(fmt.Sprintf("step_%02d", i), func(t *testing.T) {
			if hasDelta {
				t.Logf("known delta: %s", note)
			}
			checkStep(t, step, exitCode, stdout.String(), stderr.String())
		})
	}

	if changed {
		writeScenario(t, path, sc)
	}
}

// checkStep applies the parity rules from go-rewrite-spec.md §4.2:
//   - exit code always exact
//   - exit 2 (usage errors): only the final "prog: error: ..." line matters;
//     argparse's own usage: banner is not parity-bound
//   - otherwise stdout and stderr compare exact, falling back to a
//     whitespace-collapsed comparison so `table` column padding (which is not
//     parity-bound; see §4.2) does not fail the test. Any content difference
//     -- a changed column, a reordered field, a different value -- survives
//     the collapse and still fails.
func checkStep(t *testing.T, step Step, exitCode int, stdout, stderr string) {
	t.Helper()
	if exitCode != step.Exit {
		t.Errorf("argv %v\nexit = %d, want %d\n--- got stdout ---\n%s--- got stderr ---\n%s",
			step.Argv, exitCode, step.Exit, stdout, stderr)
		return
	}
	if step.Exit == 2 {
		want, got := lastErrorLine(step.Stderr), lastErrorLine(stderr)
		if !outputsMatch(want, got, step.Normalize) {
			t.Errorf("argv %v\nusage error message mismatch\n%s", step.Argv, unifiedDiff(want, got))
		}
		return
	}
	if !outputsMatch(step.Stdout, stdout, step.Normalize) {
		t.Errorf("argv %v\nstdout mismatch\n%s", step.Argv, unifiedDiff(step.Stdout, stdout))
	}
	if !outputsMatch(step.Stderr, stderr, step.Normalize) {
		t.Errorf("argv %v\nstderr mismatch\n%s", step.Argv, unifiedDiff(step.Stderr, stderr))
	}
}

// outputsMatch first tries an exact comparison (required for plain/tsv/json/
// yaml), then a whitespace-collapsed one (the leniency `table` needs for
// padding). `normalize` entries mask fields whose value is expected to differ
// run to run, e.g. "dbpath" for `info`'s absolute database path.
func outputsMatch(want, got string, normalize []string) bool {
	want, got = applyNormalize(want, normalize), applyNormalize(got, normalize)
	if want == got {
		return true
	}
	return collapseWhitespace(want) == collapseWhitespace(got)
}

var dbPathRE = regexp.MustCompile(`\S*agtrack\.db\b`)

func applyNormalize(s string, normalize []string) string {
	for _, n := range normalize {
		if n == "dbpath" {
			s = dbPathRE.ReplaceAllString(s, "<DBPATH>")
		}
	}
	return s
}

var wsRunRE = regexp.MustCompile(`[ \t]+`)

func collapseWhitespace(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(wsRunRE.ReplaceAllString(line, " "), " ")
	}
	return strings.Join(lines, "\n")
}

// lastErrorLine pulls argparse's final "prog: error: ..." line out of a
// usage-error stderr, ignoring the "usage: ..." banner above it (and any
// wrapped continuation lines of that banner), per go-rewrite-spec.md §4.2.
func lastErrorLine(stderr string) string {
	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], ": error:") {
			return strings.TrimSpace(lines[i])
		}
	}
	if len(lines) > 0 {
		return strings.TrimSpace(lines[len(lines)-1])
	}
	return ""
}

func hasTag(tags []string, want string) bool {
	for _, tg := range tags {
		if tg == want {
			return true
		}
	}
	return false
}

func containsHelpFlag(argv []string) bool {
	for _, a := range argv {
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

func loadDeltas(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]string{}
	}
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var deltas map[string]string
	if err := json.Unmarshal(raw, &deltas); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return deltas
}

// writeScenario rewrites only the stdout/stderr/exit of steps that changed,
// via a generic map round-trip so every other field (argv, cwd, now, env,
// normalize, tags, stdin) is byte-preserved. Go's encoding/json sorts
// map[string]any keys alphabetically, matching capture.py's sort_keys=True.
func writeScenario(t *testing.T, path string, sc *Scenario) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s for -update: %v", path, err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("parsing %s for -update: %v", path, err)
	}
	steps, ok := generic["steps"].([]any)
	if !ok || len(steps) != len(sc.Steps) {
		t.Fatalf("%s: steps shape changed under us, refusing to -update", path)
	}
	for i, step := range sc.Steps {
		stepMap, ok := steps[i].(map[string]any)
		if !ok {
			t.Fatalf("%s: step %d is not an object, refusing to -update", path, i)
		}
		stepMap["stdout"] = step.Stdout
		stepMap["stderr"] = step.Stderr
		stepMap["exit"] = step.Exit
	}
	out, err := json.MarshalIndent(generic, "", " ")
	if err != nil {
		t.Fatalf("marshaling %s for -update: %v", path, err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("writing %s for -update: %v", path, err)
	}
	t.Logf("-update: rewrote %s", path)
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root := filepath.Join(wd, "..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("could not find repo root (go.mod) above %s: %v", wd, err)
	}
	return root
}

// unifiedDiff is a minimal LCS-based line diff (stdlib only -- see
// AGTASK-CLAUDE.md's "no dependencies beyond the standard library"), good
// enough to point at the first real divergence in a test failure without
// dumping two full multi-line strings side by side.
func unifiedDiff(want, got string) string {
	a := strings.Split(want, "\n")
	b := strings.Split(got, "\n")
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var out strings.Builder
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			fmt.Fprintf(&out, "  %s\n", a[i])
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			fmt.Fprintf(&out, "- %s\n", a[i])
			i++
		default:
			fmt.Fprintf(&out, "+ %s\n", b[j])
			j++
		}
	}
	for ; i < n; i++ {
		fmt.Fprintf(&out, "- %s\n", a[i])
	}
	for ; j < m; j++ {
		fmt.Fprintf(&out, "+ %s\n", b[j])
	}
	return out.String()
}
