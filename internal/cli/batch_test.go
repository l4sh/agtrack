package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// batch runs `agtrack batch` with the given op lines in a file and returns
// the decoded result lines, stderr and the exit code.
func (e *env) batch(lines []string, flags ...string) ([]map[string]any, string, int) {
	e.t.Helper()
	f := filepath.Join(e.t.TempDir(), "ops.jsonl")
	if err := os.WriteFile(f, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		e.t.Fatal(err)
	}
	out, errs, code := e.run(append([]string{"batch", "--file", f}, flags...)...)
	var res []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			e.t.Fatalf("bad result line %q: %v", l, err)
		}
		res = append(res, m)
	}
	return res, errs, code
}

func taskCount(e *env) int {
	out := e.ok("task", "ls", "--status=all", "--count")
	var n int
	for _, ch := range strings.TrimSpace(out) {
		n = n*10 + int(ch-'0')
	}
	return n
}

func TestBatchSuccess(t *testing.T) {
	e := closureEnv(t)
	res, _, code := e.batch([]string{
		`{"argv": ["task","add","one","--step","a"]}`,
		``,
		`{"argv": ["sub","start","1.1"]}`,
		`{"argv": ["sub","done","1.1"]}`,
	})
	if code != 0 || len(res) != 3 {
		t.Fatalf("code %d res %v", code, res)
	}
	for i, r := range res {
		if r["ok"] != true || r["result"] == nil {
			t.Fatalf("line %d: %v", i, r)
		}
	}
	if res[2]["op"] != float64(4) {
		t.Fatalf("op index: %v", res[2])
	}
	if !strings.Contains(e.ok("sub", "ls", "1", "-f", "json"), `"status": "done"`) {
		t.Fatal("step not done")
	}
}

func TestBatchRollbackOnFailure(t *testing.T) {
	e := closureEnv(t)
	res, _, code := e.batch([]string{
		`{"argv": ["task","add","one"]}`,
		`{"argv": ["task","show","999"]}`,
		`{"argv": ["task","add","never"]}`,
	})
	if code == 0 || len(res) != 2 {
		t.Fatalf("code %d res %v", code, res)
	}
	if res[0]["ok"] != true || res[0]["rolled_back"] != true {
		t.Fatalf("first: %v", res[0])
	}
	if res[1]["ok"] != false || res[1]["error"] == nil || res[1]["code"] == nil {
		t.Fatalf("second: %v", res[1])
	}
	if n := taskCount(e); n != 0 {
		t.Fatalf("%d tasks survived the rollback", n)
	}
}

func TestBatchContinue(t *testing.T) {
	e := closureEnv(t)
	res, _, code := e.batch([]string{
		`{"argv": ["task","add","one"]}`,
		`not json`,
		`{"argv": ["task","show","999"]}`,
		`{"argv": ["task","add","two"]}`,
	}, "--continue")
	if code == 0 || len(res) != 4 {
		t.Fatalf("code %d res %v", code, res)
	}
	if res[0]["ok"] != true || res[1]["ok"] != false || res[2]["ok"] != false || res[3]["ok"] != true {
		t.Fatalf("%v", res)
	}
	if res[0]["rolled_back"] != nil {
		t.Fatalf("continue must not roll back: %v", res[0])
	}
	if n := taskCount(e); n != 2 {
		t.Fatalf("want 2 tasks, got %d", n)
	}
}

func TestBatchRefusals(t *testing.T) {
	e := closureEnv(t)
	for _, argv := range []string{
		`["watch"]`, `["serve"]`, `["spec","serve","x.md"]`, `["spec","watch","x.md"]`,
		`["spec","await","x.md"]`, `["batch","--file","x"]`,
	} {
		res, _, code := e.batch([]string{`{"argv": ` + argv + `}`})
		if code != 2 || len(res) != 1 || res[0]["ok"] != false || !strings.Contains(res[0]["message"].(string), "cannot run inside a batch") {
			t.Fatalf("%s: code %d res %v", argv, code, res)
		}
	}
	// ops cannot redirect the database or override the batch's identity
	for _, argv := range []string{`["--db","/tmp/other.db","task","ls"]`, `["task","ls","--actor","x"]`,
		`["task","ls","--session=s"]`, `["task","ls","-c","w"]`} {
		res, _, code := e.batch([]string{`{"argv": ` + argv + `}`})
		if code != 2 || res[0]["ok"] != false || !strings.Contains(res[0]["message"].(string), "cannot be overridden") {
			t.Fatalf("%s: code %d res %v", argv, code, res)
		}
	}
}

func TestBatchIdem(t *testing.T) {
	e := closureEnv(t)
	op := `{"argv": ["task","add","once","--idem","k1"]}`
	res, _, code := e.batch([]string{op, op})
	if code != 0 || len(res) != 2 {
		t.Fatalf("code %d res %v", code, res)
	}
	if n := taskCount(e); n != 1 {
		t.Fatalf("replay created %d tasks", n)
	}
	res2, _, _ := e.batch([]string{op})
	if res2[0]["ok"] != true {
		t.Fatalf("%v", res2)
	}
	if n := taskCount(e); n != 1 {
		t.Fatalf("cross-batch replay created %d tasks", n)
	}
	// --idem on a non-create verb is the CLI's usage error
	res3, _, code := e.batch([]string{`{"argv": ["task","ls","--idem","k"]}`})
	if code != 2 || res3[0]["ok"] != false {
		t.Fatalf("%d %v", code, res3)
	}
}

func TestBatchOpForm(t *testing.T) {
	e := closureEnv(t)
	e.ok("task", "add", "t", "--step", "a", "--step", "b")
	res, _, code := e.batch([]string{
		`{"op": "sub start", "args": {"ref": "1.1"}}`,
		`{"op": "sub done", "args": {"ref": "1.1", "note": "finished it", "tag": ["x","y"], "close-parent": false}}`,
		`{"op": "task add", "args": {"title": "from op", "step": ["s1","s2"], "idem": "op-k"}}`,
		`{"op": "task note", "args": {"ref": 1, "body": ["a", "-dash", "words"], "kind": "decision"}}`,
	})
	if code != 0 {
		t.Fatalf("code %d res %v", code, res)
	}
	show := e.ok("task", "show", "1", "-x")
	for _, want := range []string{"finished it", "-dash"} {
		if !strings.Contains(show, want) {
			t.Fatalf("missing %q in %s", want, show)
		}
	}
	if !strings.Contains(e.ok("task", "show", "2", "-x"), "s2") {
		t.Fatal("append flag not mapped")
	}
	// unknown keys and bad shapes fail with a usage error naming the key
	bad, _, code := e.batch([]string{`{"op": "sub done", "args": {"ref": "1.2", "bogus": 1}}`,
		`{"op": "nope"}`, `{"argv": ["task","ls"], "op": "task ls"}`, `{"args": {}}`})
	if code != 2 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(bad[0]["message"].(string), "bogus") {
		t.Fatalf("%v", bad[0])
	}
}

func TestOpArgvMapping(t *testing.T) {
	p := NewParser()
	argv, err := opArgv(p, []string{"sub", "done"}, map[string]any{"ref": "12.3", "note": "n", "close_parent": true, "evidence": []any{"test:ok"}})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(argv, "|")
	want := "sub|done|--close-parent|--evidence=test:ok|--note=n|--|12.3"
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestBatchStdin(t *testing.T) {
	e := closureEnv(t)
	var out bytes.Buffer
	c := Run([]string{"--db", e.db, "--actor", "tester", "batch"}, Streams{Out: &out, Err: &bytes.Buffer{}, In: strings.NewReader(`{"argv":["task","add","x"]}`+"\n")})
	if c != 0 || !strings.Contains(out.String(), `"ok":true`) {
		t.Fatalf("%d %s", c, out.String())
	}
}
