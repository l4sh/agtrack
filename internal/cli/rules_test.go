package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// H: standing rules, check, lessons.

func rulesEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.ok("ctx", "add", "w")
	e.ok("project", "add", "orb", "--use")
	e.ok("project", "add", "orb:api")
	e.ok("task", "add", "t1", "-p", "orb:api")
	e.actor = "bot"
	return e
}

func checkJSON(t *testing.T, e *env, want int, args ...string) map[string]any {
	t.Helper()
	out, errOut, code := e.run(append([]string{"check"}, append(args, "-f", "json")...)...)
	if code != want {
		t.Fatalf("check %v: exit %d, want %d\n%s%s", args, code, want, out, errOut)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("check %v: not json: %s", args, out)
	}
	return v
}

func TestRuleAddValidationAndProposal(t *testing.T) {
	e := rulesEnv(t)
	if msg := e.fails(2, "rule", "add", "deny", "push", "-p", "orb"); !strings.Contains(msg, "--quote is required") {
		t.Fatalf("deny without quote: %s", msg)
	}
	e.fails(2, "rule", "add", "deny", "fly", "-p", "orb", "--quote", "q")
	e.fails(2, "rule", "add", "require", "vibes", "-p", "orb")
	e.fails(2, "rule", "add", "remember", "-p", "orb")
	e.fails(2, "rule", "add", "allow", "push", "-g", "-p", "orb", "--quote", "q")
	e.fails(2, "rule", "add", "remember", "x", "-p", "orb", "--until-task-closed")

	// An agent's allow and hard rules are proposals (H5.1).
	r := obj(e.json("rule", "add", "allow", "push", "-p", "orb", "--quote", "go ahead"))
	if r["status"] != "proposed" {
		t.Fatalf("agent allow: %v", r)
	}
	r = obj(e.json("rule", "add", "deny", "release", "--hard", "-p", "orb", "--quote", "never release"))
	if r["status"] != "proposed" || r["hard"] != true {
		t.Fatalf("agent hard: %v", r)
	}
	if r := obj(e.json("rule", "add", "deny", "push", "-p", "orb", "--quote", "no")); r["status"] != "active" {
		t.Fatalf("agent deny: %v", r)
	}
	// Approval is for approvers only.
	if msg := e.fails(1, "rule", "approve", "1"); !strings.Contains(msg, "only an approver") {
		t.Fatalf("agent approve: %s", msg)
	}
	e.actor = "human"
	if r := obj(e.json("rule", "approve", "1", "--note", "ok")); r["status"] != "active" {
		t.Fatalf("approve: %v", r)
	}
	e.fails(4, "rule", "approve", "1")
	if r := obj(e.json("rule", "add", "allow", "merge", "-p", "orb", "--quote", "merge freely")); r["status"] != "active" {
		t.Fatalf("approver allow: %v", r)
	}
	if r := obj(e.json("rule", "propose", "deny", "deploy", "-p", "orb", "--quote", "q")); r["status"] != "proposed" {
		t.Fatalf("propose: %v", r)
	}
	show := obj(e.json("rule", "show", "1"))
	evs := list(show["events"])
	if len(evs) != 2 || obj(evs[0])["action"] != "propose" || obj(evs[1])["action"] != "approve" {
		t.Fatalf("history: %v", evs)
	}
	if n := strings.TrimSpace(e.ok("rules", "-p", "orb", "--proposed", "--count")); n != "2" {
		t.Fatalf("proposed count %s", n)
	}
}

func TestRuleDuplicateCheck(t *testing.T) {
	e := rulesEnv(t)
	e.ok("rule", "add", "deny", "push", "-p", "orb", "--quote", "never push")
	// Same rule at a narrower scope: the wider one already covers it.
	if msg := e.fails(4, "rule", "add", "deny", "push", "-p", "orb:api", "--quote", "do not push"); !strings.Contains(msg, "similar rule #1") {
		t.Fatalf("dup: %s", msg)
	}
	// Different path: a different rule.
	e.ok("rule", "add", "deny", "push", "-p", "orb", "--path", "infra/**", "--quote", "q")
	e.ok("rule", "add", "remember", "use conventional commit messages always", "-p", "orb")
	e.fails(4, "rule", "add", "remember", "always use conventional commit messages", "-p", "orb")
	e.ok("rule", "add", "remember", "the staging database is shared with QA", "-p", "orb")
	r := obj(e.json("rule", "add", "deny", "push", "-p", "orb", "--quote", "new words", "--supersedes", "1"))
	if r["supersedes"] != float64(1) {
		t.Fatalf("supersedes: %v", r)
	}
	if old := obj(e.json("rule", "show", "1")); old["status"] != "revoked" {
		t.Fatalf("superseded rule: %v", old["status"])
	}
	e.ok("rule", "add", "deny", "push", "-p", "orb", "--quote", "q", "--force")
	// A wider scope is not covered by a narrower rule, but then covers it.
	e.ok("rule", "add", "deny", "push", "-g", "--quote", "never push anywhere")
	e.fails(4, "rule", "add", "deny", "push", "-p", "orb:api", "--quote", "again")
}

func TestCheckDecisions(t *testing.T) {
	e := rulesEnv(t)
	repo := []string{"--repo", "github.com/acme/orb", "--branch", "feat/x"}
	args := func(extra ...string) []string { return append(append([]string{}, extra...), repo...) }

	v := checkJSON(t, e, 0, args("push", "-p", "orb")...)
	for _, k := range []string{"action", "decision", "hard", "matched", "allowed_by", "hint"} {
		if _, ok := v[k]; !ok {
			t.Fatalf("check json lacks %s: %v", k, v)
		}
	}
	if v["decision"] != "allow" || v["hint"] != nil {
		t.Fatalf("empty rule set: %v", v)
	}

	e.ok("rule", "add", "deny", "push", "-p", "orb", "--overridable", "--quote", "ask before pushing")
	if v := checkJSON(t, e, 20, args("push", "-t", "1")...); v["decision"] != "deny" || v["hard"] != false {
		t.Fatalf("deny: %v", v)
	}
	checkJSON(t, e, 0, args("commit", "-t", "1")...)

	// A proposed allow at a narrower scope: needs approval.
	e.ok("rule", "add", "allow", "push", "-t", "1", "--quote", "push this one")
	if v := checkJSON(t, e, 22, args("push", "-t", "1")...); v["decision"] != "needs-approval" {
		t.Fatalf("pending allow: %v", v)
	}
	e.actor = "human"
	e.ok("rule", "approve", "2")
	e.actor = "bot"
	v = checkJSON(t, e, 0, args("push", "-t", "1")...)
	if len(list(v["allowed_by"])) != 1 || obj(list(v["matched"])[0])["effect"] != "cancelled" {
		t.Fatalf("allow cancels: %v", v)
	}
	// The allow lives on the task: the project itself is still denied.
	checkJSON(t, e, 20, args("push", "-p", "orb")...)

	// An allow at a wider scope cannot cancel a narrower deny.
	e.actor = "human"
	e.ok("rule", "add", "deny", "merge", "-t", "1", "--overridable", "--quote", "no merges here")
	e.ok("rule", "add", "allow", "merge", "-g", "--quote", "merge anywhere")
	e.actor = "bot"
	checkJSON(t, e, 20, args("merge", "-t", "1")...)

	// A non-overridable deny ignores allows.
	e.actor = "human"
	e.ok("rule", "add", "deny", "install", "-p", "orb", "--quote", "no installs")
	e.ok("rule", "add", "allow", "install", "-t", "1", "--quote", "install ok")
	checkJSON(t, e, 20, args("install", "-t", "1")...)

	// Hard deny: exit 21, never cancelled.
	e.ok("rule", "add", "deny", "release", "-g", "--hard", "--quote", "never release")
	e.ok("rule", "add", "allow", "release", "-t", "1", "--quote", "release it")
	if v := checkJSON(t, e, 21, args("release", "-t", "1")...); v["hard"] != true {
		t.Fatalf("hard: %v", v)
	}
	e.actor = "bot"
	e.fails(1, "rule", "revoke", "7", "--note", "agents cannot lift hard rules")
	e.fails(2, "rule", "revoke", "3")

	// require branch-not-base and ask-first.
	e.ok("rule", "add", "require", "branch-not-base", "--verb", "commit", "-p", "orb")
	checkJSON(t, e, 0, "commit", "-p", "orb", "--branch", "feat/x")
	if v := checkJSON(t, e, 20, "commit", "-p", "orb", "--branch", "develop"); !strings.Contains(v["hint"].(string), "base branch") {
		t.Fatalf("base: %v", v)
	}
	e.ok("rule", "add", "require", "ask-first", "--verb", "external-post", "-g")
	checkJSON(t, e, 22, args("external-post", "-p", "orb")...)
	e.ok("rule", "add", "require", "tests-pass", "--verb", "pr", "-p", "orb")
	if v := checkJSON(t, e, 0, args("pr", "-p", "orb")...); obj(list(v["matched"])[0])["effect"] != "self-check" {
		t.Fatalf("self-check: %v", v)
	}

	// Path and repo conditions.
	e.ok("rule", "add", "deny", "write", "-p", "orb", "--path", "migrations/**", "--quote", "hands off migrations")
	checkJSON(t, e, 0, args("write", "-p", "orb", "--path", "src/a.go")...)
	checkJSON(t, e, 20, args("write", "-p", "orb", "--path", "src/a.go", "--path", "migrations/001.sql")...)
	e.ok("rule", "add", "deny", "deploy", "-p", "orb", "--repo", "acme/other", "--quote", "not that repo")
	checkJSON(t, e, 0, args("deploy", "-p", "orb")...)
	checkJSON(t, e, 20, "deploy", "-p", "orb", "--repo", "https://github.com/acme/other.git", "--branch", "x")

	// Text output and -q keep the exit code.
	out, _, code := e.run("check", "push", "-p", "orb", "--repo", "acme/orb", "--branch", "x")
	if code != 20 || !strings.HasPrefix(out, "deny: push") || !strings.Contains(out, "hint:") {
		t.Fatalf("text check: %d %s", code, out)
	}
	if out, _, code := e.run("-q", "check", "push", "-p", "orb", "--repo", "acme/orb", "--branch", "x"); code != 20 || out != "" {
		t.Fatalf("quiet check: %d %q", code, out)
	}
	if !strings.Contains(e.ok("grammar"), "21 hard deny") {
		t.Fatal("grammar lacks check exit codes")
	}
}

func TestRuleGlobalAndContextScope(t *testing.T) {
	e := rulesEnv(t)
	e.ok("ctx", "add", "other")
	e.ok("project", "add", "other/zed")
	e.ok("rule", "add", "deny", "push", "-g", "--quote", "never push")
	e.ok("rule", "add", "deny", "merge", "-c", "w", "--quote", "never merge in w")
	checkJSON(t, e, 20, "push", "-p", "other/zed", "--repo", "a/b", "--branch", "x")
	checkJSON(t, e, 0, "merge", "-p", "other/zed", "--repo", "a/b", "--branch", "x")
	checkJSON(t, e, 20, "merge", "-p", "orb:api", "--repo", "a/b", "--branch", "x")
	rs := list(e.json("rules", "-p", "orb:api"))
	if len(rs) != 2 || obj(rs[0])["scope"] != "context w" || obj(rs[1])["scope"] != "global" {
		t.Fatalf("narrowest first: %v", rs)
	}
}

func TestRuleExpiry(t *testing.T) {
	e := rulesEnv(t)
	e.ok("rule", "add", "deny", "push", "-p", "orb", "--until", "1d", "--quote", "not today")
	e.ok("rule", "add", "deny", "merge", "-t", "1", "--until-task-closed", "--quote", "not while #1 is open")
	e.ok("--session", "s1", "rule", "add", "deny", "commit", "-p", "orb", "--for-session", "--quote", "hold commits")
	ck := []string{"--repo", "a/b", "--branch", "x", "-t", "1"}
	checkJSON(t, e, 20, append([]string{"push"}, ck...)...)
	checkJSON(t, e, 20, append([]string{"merge"}, ck...)...)
	// Session rules bind their own session only.
	checkJSON(t, e, 0, append([]string{"commit"}, ck...)...)
	if code := func() int {
		_, _, c := e.run(append([]string{"--session", "s1", "check", "commit"}, ck...)...)
		return c
	}(); code != 20 {
		t.Fatalf("session rule in its session: %d", code)
	}

	e.ok("task", "done", "1")
	checkJSON(t, e, 0, append([]string{"merge"}, ck...)...)
	t.Setenv("AGTRACK_NOW", time.Now().UTC().Add(48*time.Hour).Format(time.RFC3339))
	checkJSON(t, e, 0, append([]string{"push"}, ck...)...)
	all := list(e.json("rules", "-t", "1", "--all"))
	st := map[string]string{}
	for _, r := range all {
		st[fmt.Sprint(obj(r)["rule"])] = fmt.Sprint(obj(r)["status"])
	}
	if st["push"] != "expired" || st["merge"] != "expired" || st["commit"] != "expired" {
		t.Fatalf("lazy expiry: %v", st)
	}
	if n := strings.TrimSpace(e.ok("rules", "-t", "1", "--count")); n != "0" {
		t.Fatalf("expired rules listed: %s", n)
	}
}

func TestResumeCarriesRules(t *testing.T) {
	e := rulesEnv(t)
	e.ok("task", "start", "1")
	for i, v := range []string{"push", "merge", "release", "install", "delete", "deploy"} {
		e.ok("rule", "add", "deny", v, "-p", "orb", "--quote", fmt.Sprintf("q%d", i))
	}
	e.ok("rule", "add", "remember", "only remembered", "-p", "orb")
	e.ok("lesson", "add", "the api needs a running redis", "-t", "1")
	p := obj(e.json("resume", "-t", "1"))
	rs := list(p["rules"])
	if len(rs) != 5 {
		t.Fatalf("rules block: %v", rs)
	}
	found := false
	for _, o := range list(p["omitted"]) {
		if obj(o)["what"] == "rules" && obj(o)["count"] == float64(1) && strings.Contains(fmt.Sprint(obj(o)["retrieve"]), "agtrack rules -p") {
			found = true
		}
	}
	if !found {
		t.Fatalf("omitted: %v", p["omitted"])
	}
	if ls := list(p["lessons"]); len(ls) != 1 || obj(ls[0])["title"] != "the api needs a running redis" {
		t.Fatalf("lessons block: %v", ls)
	}
}

func TestLessons(t *testing.T) {
	e := rulesEnv(t)
	e.ok("lesson", "add", "pnpm install needs node 20", "-p", "orb", "--cmd", "pnpm install", "--body", "use nvm use 20 first", "--kind", "fix")
	e.ok("lesson", "add", "migrations run in a transaction", "-p", "orb:api", "--path", "migrations/", "--topic", "DB")
	e.ok("lesson", "add", "ssh agent forwarding is off on this machine", "-g")
	if msg := e.fails(4, "lesson", "add", "pnpm install requires node 20", "-p", "orb:api"); !strings.Contains(msg, "similar lesson #1") {
		t.Fatalf("dup: %s", msg)
	}
	e.ok("lesson", "add", "pnpm install requires node 20", "-p", "orb:api", "--supersedes", "1", "--cmd", "pnpm install")
	if l := obj(e.json("lesson", "show", "1")); l["status"] != "retired" {
		t.Fatalf("superseded: %v", l["status"])
	}

	all := list(e.json("lessons", "-p", "orb:api"))
	if len(all) != 3 {
		t.Fatalf("lessons: %v", all)
	}
	if _, has := obj(all[0])["body"]; has {
		t.Fatal("body without -x")
	}
	byPath := list(e.json("lessons", "-p", "orb:api", "--path", "migrations/002_add.sql"))
	if len(byPath) != 1 || obj(byPath[0])["id"] != float64(2) {
		t.Fatalf("by path: %v", byPath)
	}
	byCmd := list(e.json("lessons", "-p", "orb:api", "--cmd", "pnpm   install --frozen-lockfile", "-x"))
	if len(byCmd) != 1 || obj(byCmd[0])["id"] != float64(4) {
		t.Fatalf("by cmd: %v", byCmd)
	}
	if byTopic := list(e.json("lessons", "-p", "orb:api", "--topic", "db")); len(byTopic) != 1 {
		t.Fatalf("by topic: %v", byTopic)
	}
	if byQ := list(e.json("lessons", "-p", "orb:api", "--q", "forwarding")); len(byQ) != 1 || obj(byQ[0])["id"] != float64(3) {
		t.Fatalf("by text: %v", byQ)
	}
	if out := e.ok("lessons", "-p", "orb:api", "--path", "migrations/x.sql", "-x"); !strings.Contains(out, "migrations run") {
		t.Fatalf("text: %s", out)
	}

	e.ok("lesson", "amend", "2", "--body", "wrap DDL in BEGIN", "--add-path", "db/**")
	if l := obj(e.json("lesson", "show", "2")); l["body"] != "wrap DDL in BEGIN" || len(list(l["paths"])) != 2 {
		t.Fatalf("amend: %v", l)
	}
	t.Setenv("AGTRACK_NOW", time.Now().UTC().Add(100*24*time.Hour).Format(time.RFC3339))
	stale := list(e.json("lessons", "-p", "orb:api", "--unverified", "30d"))
	if len(stale) != 3 || !strings.HasPrefix(fmt.Sprint(obj(stale[0])["stale"]), "unverified since") {
		t.Fatalf("unverified: %v", stale)
	}
	e.ok("lesson", "verify", "2", "--note", "still true")
	if n := strings.TrimSpace(e.ok("lessons", "-p", "orb:api", "--unverified", "30d", "--count")); n != "2" {
		t.Fatalf("after verify: %s", n)
	}
	e.fails(2, "lesson", "retire", "2")
	e.ok("lesson", "retire", "2", "--note", "we moved to a new migrator")
	if n := strings.TrimSpace(e.ok("lessons", "-p", "orb:api", "--count")); n != "2" {
		t.Fatalf("after retire: %s", n)
	}
	// --idem replays.
	a := e.ok("lesson", "add", "a brand new fact", "-g", "--idem", "k1", "-q")
	b := e.ok("lesson", "add", "a brand new fact", "-g", "--idem", "k1", "-q")
	if a != b {
		t.Fatalf("idem: %q %q", a, b)
	}
}
