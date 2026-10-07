package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/refs"
	"github.com/l4sh/agtrack/internal/render"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
)

// parseDuration accepts Go durations (45m, 1h30m) plus d and w, and 0.
func parseDuration(v string) (time.Duration, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" || v == "0" || v == "none" {
		return 0, nil
	}
	if n := len(v); n > 1 && (v[n-1] == 'd' || v[n-1] == 'w') {
		num, err := strconv.Atoi(v[:n-1])
		if err == nil {
			unit := 24 * time.Hour
			if v[n-1] == 'w' {
				unit *= 7
			}
			return time.Duration(num) * unit, nil
		}
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, errs.New("cannot parse duration: %s (use 45m, 2h, 1d or 0)", v)
	}
	return d, nil
}

// --- claims ----------------------------------------------------------------------------

func taskClaim(c *Ctx) error {
	task, err := c.S.GetTask(c.Str("ref"), true)
	if err != nil {
		return err
	}
	ttl, err := parseDuration(c.Str("ttl"))
	if err != nil {
		return err
	}
	res, err := c.S.ClaimTask(task.Int("id"), ttl, c.Bool("steal"))
	if err != nil {
		return err
	}
	t := res.Task
	text := fmt.Sprintf("#%d claimed by %s", t.Int("id"), t.Str("claimed_by"))
	if exp := t.Str("claim_expires_at"); exp != "" {
		text += " until " + refs.FmtTS(exp, false, false)
	}
	if res.Stolen != "" {
		text += " (taken from " + res.Stolen + ")"
	}
	obj, _ := c.S.TaskDict(t)
	obj.Set("stolen_from", nilIfEmpty(res.Stolen))
	obj.Set("renewed", res.Renewed)
	return c.mutated(text, c.lean(obj, "task"))
}

func taskRelease(c *Ctx) error {
	task, err := c.S.GetTask(c.Str("ref"), true)
	if err != nil {
		return err
	}
	t, changed, err := c.S.ReleaseTask(task.Int("id"), c.Bool("force"))
	if err != nil {
		return err
	}
	text := fmt.Sprintf("#%d released", t.Int("id"))
	if !changed {
		text = fmt.Sprintf("#%d was not claimed", t.Int("id"))
	}
	return c.mutated(text, row.Of("id", t.Int("id"), "released", changed))
}

// nextClaim folds find-and-take into one round trip: the first unclaimed,
// unblocked open task is claimed; losing a race to another agent moves on
// to the next candidate (design §4.6).
func nextClaim(c *Ctx, f store.TaskFilter) error {
	if c.Actor == "" {
		return errs.New("a claim needs an actor: set AGTRACK_ACTOR or pass --actor")
	}
	ttl, err := parseDuration(c.Str("ttl"))
	if err != nil {
		return err
	}
	f.Statuses = []string{"in-progress", "todo"}
	f.Unclaimed = true
	f.Ready = true
	f.Limit = 0
	// Tasks this actor already holds come first: resuming beats starting.
	mine, _, err := c.S.ListTasks(store.TaskFilter{Context: f.Context, Project: f.Project, AllContexts: f.AllContexts,
		Statuses: f.Statuses, ClaimedBy: c.Actor, Order: "status"})
	if err != nil {
		return err
	}
	candidates, _, err := c.S.ListTasks(f)
	if err != nil {
		return err
	}
	for _, t := range append(mine, candidates...) {
		res, err := c.S.ClaimDoable(t.Int("id"), ttl)
		if err != nil {
			if e, ok := errs.As(err); ok && e.Code == errs.ExitConflict {
				continue
			}
			return err
		}
		obj, _ := c.S.TaskDict(res.Task)
		subs, _, _ := c.S.ListSubtasks(t.Int("id"), refs.OpenStatuses, 0)
		if c.structured() {
			obj = c.lean(obj, "task").(*row.Row)
			obj.Set("open_subtasks", nonNil(subs))
			return c.emitObject(obj)
		}
		if c.Quiet {
			c.println(t.Int("id"))
			return nil
		}
		c.printf("#%d %s %s  %s  (claimed by %s)\n", obj.Int("id"), obj.Str("mark"), obj.Str("project"), obj.Str("title"), claimLine(res.Task))
		for _, s := range subs {
			c.printf("    %-8s %s %s\n", s.Str("ref"), s.Str("mark"), s.Str("title"))
		}
		return nil
	}
	return errs.NotFound("task", "", "nothing to claim: every open task is claimed, blocked or done").WithToken("nothing_to_claim")
}

// --- dependencies ------------------------------------------------------------------------

func depTargets(c *Ctx) ([]int64, error) {
	var out []int64
	for _, ref := range splitTags(c.Strs("on")) {
		id, err := refs.ParseTaskRef(ref)
		if err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil, errs.Usage("the following arguments are required: --on")
	}
	return out, nil
}

func taskBlock(c *Ctx) error {
	task, err := c.S.GetTask(c.Str("ref"), true)
	if err != nil {
		return err
	}
	on, err := depTargets(c)
	if err != nil {
		return err
	}
	err = c.S.Tx(func() error {
		for _, id := range on {
			if _, err := c.S.AddDep(task.Int("id"), id, c.Str("kind"), c.Str("reason")); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return taskDepsOut(c, task, true)
}

func taskUnblock(c *Ctx) error {
	task, err := c.S.GetTask(c.Str("ref"), true)
	if err != nil {
		return err
	}
	on, err := depTargets(c)
	if err != nil {
		return err
	}
	err = c.S.Tx(func() error {
		for _, id := range on {
			if _, err := c.S.RemoveDep(task.Int("id"), id, c.Str("kind")); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return taskDepsOut(c, task, true)
}

func taskDeps(c *Ctx) error {
	task, err := c.S.GetTask(c.Str("ref"), true)
	if err != nil {
		return err
	}
	return taskDepsOut(c, task, false)
}

func taskDepsOut(c *Ctx, task *row.Row, mutation bool) error {
	deps, err := c.S.DepsOf(task.Int("id"))
	if err != nil {
		return err
	}
	dependents, err := c.S.Dependents(task.Int("id"))
	if err != nil {
		return err
	}
	if mutation && c.Quiet {
		return nil
	}
	if c.structured() {
		return c.emitObject(row.Of("task", task.Int("id"), "depends_on", deps, "dependents", dependents))
	}
	if c.Bool("count") || c.Bool("ids") {
		return c.emit(deps, "dep", len(deps))
	}
	if len(deps) == 0 && len(dependents) == 0 {
		c.printf("#%d has no dependencies\n", task.Int("id"))
		return nil
	}
	for _, d := range deps {
		state := "met"
		if d.Get("unmet") == true {
			state = "UNMET"
		} else if d.Str("kind") != "blocks" {
			state = d.Str("kind")
		}
		c.printf("#%d waits on #%d %s %s  [%s]\n", task.Int("id"), d.Int("depends_on_id"), refs.StatusMark[d.Str("status")], d.Str("title"), state)
	}
	for _, d := range dependents {
		c.printf("#%d %s %s waits on #%d  [%s]\n", d.Int("task_id"), refs.StatusMark[d.Str("status")], d.Str("title"), task.Int("id"), d.Str("kind"))
	}
	return nil
}

// --- artifacts ----------------------------------------------------------------------------

func taskArtifact(c *Ctx) error {
	task, err := c.S.GetTask(c.Str("ref"), true)
	if err != nil {
		return err
	}
	kind := c.Str("kind")
	switch kind {
	case "", "ls":
		arts, err := c.S.Artifacts(task.Int("id"))
		if err != nil {
			return err
		}
		return c.emit(arts, "artifact", len(arts))
	case "rm":
		id, err := parseID(c.Str("value"))
		if err != nil {
			return err
		}
		if err := c.S.RemoveArtifact(task.Int("id"), id); err != nil {
			return err
		}
		return c.mutated(fmt.Sprintf("removed artifact %d", id), row.Of("deleted", "artifact", "id", id))
	}
	if !c.Has("value") {
		return errs.Usage("the following arguments are required: value")
	}
	show := func(ids []int64) error {
		a, err := c.S.GetArtifact(ids[0])
		if err != nil {
			return err
		}
		return c.created("artifact", ids[0], a, "artifact")
	}
	if done, err := c.replay(show); done {
		return err
	}
	ids, err := c.record("artifact", func() ([]int64, error) {
		a, err := c.S.AddArtifact(task, kind, c.Str("value"), c.Str("label"))
		if err != nil {
			return nil, err
		}
		return []int64{a.Int("id")}, nil
	})
	if err != nil {
		return err
	}
	return show(ids)
}

// --- notes by id ------------------------------------------------------------------------

func noteID(c *Ctx) (int64, error) {
	return parseID(strings.TrimPrefix(c.Str("id"), "n"))
}

func noteResolve(c *Ctx) error {
	id, err := noteID(c)
	if err != nil {
		return err
	}
	wasOpen := false
	if n, err := c.S.GetNote(id); err == nil {
		wasOpen = n.IsNull("resolved_at")
	}
	n, changed, cascaded, err := c.S.ResolveNoteThread(id, c.Str("note"), false)
	if err != nil {
		return err
	}
	text := fmt.Sprintf("note %d resolved", id)
	if !wasOpen {
		text = fmt.Sprintf("note %d was already resolved", id)
	}
	switch cascaded {
	case 0:
	case 1:
		text += "; 1 reply resolved with it"
	default:
		text += fmt.Sprintf("; %d replies resolved with it", cascaded)
	}
	return c.mutated(text, row.Of("id", id, "resolved_at", n.Get("resolved_at"), "resolved_by", n.Get("resolved_by"), "changed", changed,
		"replies_resolved", int64(cascaded)))
}

func noteReopen(c *Ctx) error {
	id, err := noteID(c)
	if err != nil {
		return err
	}
	_, changed, err := c.S.ResolveNote(id, "", true)
	if err != nil {
		return err
	}
	text := fmt.Sprintf("note %d reopened", id)
	if !changed {
		text = fmt.Sprintf("note %d is open", id)
	}
	return c.mutated(text, row.Of("id", id, "changed", changed))
}

func noteRm(c *Ctx) error {
	id, err := noteID(c)
	if err != nil {
		return err
	}
	if err := c.S.DeleteNote(id); err != nil {
		return err
	}
	return c.mutated(fmt.Sprintf("deleted note %d", id), row.Of("deleted", "note", "id", id))
}

func noteReply(c *Ctx) error {
	show := func(ids []int64) error {
		n, err := c.S.GetNote(ids[0])
		if err != nil {
			return err
		}
		return c.created("note", ids[0], row.Of("id", ids[0], "parent_id", n.Get("parent_id")), "note")
	}
	if done, err := c.replay(show); done {
		return err
	}
	id, err := noteID(c)
	if err != nil {
		return err
	}
	parent, err := c.S.GetNote(id)
	if err != nil {
		return err
	}
	// Threads stay one level deep in display; a reply to a reply joins the root.
	root := parent
	for !root.IsNull("parent_id") {
		if root, err = c.S.GetNote(root.Int("parent_id")); err != nil {
			return err
		}
	}
	ids, err := c.record("note", func() ([]int64, error) {
		n, err := c.S.AddNote(store.NewNote{TargetType: root.Str("target_type"), TargetID: root.Int("target_id"),
			Body: c.Str("body"), ParentID: root.Int("id"), Kind: "note"})
		if err != nil {
			return nil, err
		}
		return []int64{n.Int("id")}, nil
	})
	if err != nil {
		return err
	}
	return show(ids)
}

func noteShow(c *Ctx) error {
	id, err := noteID(c)
	if err != nil {
		return err
	}
	n, err := c.S.GetNote(id)
	if err != nil {
		return err
	}
	thread, err := c.S.NoteThread(id)
	if err != nil {
		return err
	}
	obj := n.Copy()
	obj.Delete("uid")
	obj.Set("replies", thread)
	var lines []string
	for _, r := range thread {
		lines = append(lines, fmt.Sprintf("%d %s %s: %s", r.Int("id"), refs.FmtTS(r.Str("created_at"), false, false), r.Str("actor"), r.Str("body")))
	}
	state := "open"
	if !n.IsNull("resolved_at") {
		state = "resolved by " + n.Str("resolved_by") + " " + refs.FmtTS(n.Str("resolved_at"), false, false)
		if n.Str("resolution") != "" {
			state += ": " + n.Str("resolution")
		}
	}
	return c.detail(obj, "note", []render.Section{
		{Label: "note", Value: fmt.Sprintf("%d [%s] on %s %d", id, n.Str("kind"), n.Str("target_type"), n.Int("target_id"))},
		{Label: "by", Value: n.Str("actor")},
		{Label: "created", Value: refs.FmtTS(n.Str("created_at"), false, false)},
		{Label: "state", Value: state},
		{Label: "quote", Value: n.Str("quote")},
		{Label: "body", Value: n.Str("body")},
		{Label: "replies", Value: lines},
	})
}

func notesOpen(c *Ctx) error {
	var projectIDs []int64
	var taskID int64
	switch {
	case c.Has("task"):
		t, err := c.S.GetTask(c.Str("task"), true)
		if err != nil {
			return err
		}
		taskID = t.Int("id")
	default:
		p, err := c.resolveProject(c.Str("project"), true)
		if err != nil {
			return err
		}
		if projectIDs, err = c.S.DescendantIDs(p.Int("id")); err != nil {
			return err
		}
	}
	var kinds []string
	if c.Has("kind") {
		for _, k := range strings.Split(c.Str("kind"), ",") {
			if k = strings.TrimSpace(k); k != "" {
				nk, err := normalizeNoteKind(k)
				if err != nil {
					return err
				}
				kinds = append(kinds, nk)
			}
		}
	}
	rows, err := c.S.OpenNotes(projectIDs, taskID, kinds)
	if err != nil {
		return err
	}
	return c.emit(rows, "note", len(rows))
}

func normalizeNoteKind(k string) (string, error) {
	k = strings.ToLower(strings.TrimSpace(k))
	switch k {
	case "note", "decision", "handoff", "blocker", "question", "approval", "build-note":
		return k, nil
	case "build_note", "buildnote":
		return "build-note", nil
	}
	return "", errs.New("unknown note kind: %s (expected note, decision, handoff, blocker, question, approval or build-note)", k)
}

// --- config ---------------------------------------------------------------------------------

var configKeys = map[string][]string{
	"review.release": {"manual", "auto"},
}

func configGet(c *Ctx) error {
	keys := []string{c.Str("key")}
	if keys[0] == "" {
		keys = []string{"review.release"}
	}
	out := row.New()
	for _, k := range keys {
		if _, ok := configKeys[k]; !ok {
			return errs.New("unknown setting: %s (known: review.release)", k)
		}
		v, ok, err := c.S.GetMeta("config:" + k)
		if err != nil {
			return err
		}
		if !ok {
			v = configKeys[k][0]
		}
		out.Set(k, v)
	}
	if c.structured() {
		return c.emitObject(out)
	}
	for _, k := range out.Keys() {
		if len(keys) == 1 && c.Str("key") != "" {
			c.println(out.Str(k))
		} else {
			c.printf("%s = %s\n", k, out.Str(k))
		}
	}
	return nil
}

func configSet(c *Ctx) error {
	k, v := c.Str("key"), strings.ToLower(c.Str("value"))
	allowed, ok := configKeys[k]
	if !ok {
		return errs.New("unknown setting: %s (known: review.release)", k)
	}
	valid := false
	for _, a := range allowed {
		valid = valid || a == v
	}
	if !valid {
		return errs.New("invalid value for %s: %s (expected %s)", k, v, strings.Join(allowed, " or "))
	}
	if err := c.S.SetMeta("config:"+k, &v); err != nil {
		return err
	}
	return c.mutated(k+" = "+v, row.Of(k, v))
}

// --- grammar / describe ------------------------------------------------------------------------

const grammarText = `agtrack grammar (one screen; ` + "`agtrack describe <cmd>`" + ` for any command's flags)

refs      project  [context/]project[:subproject]   orbiter, orbiter:api, work/orbiter, . (cwd git remote)
          task     12, #12            subtask  47 (id) or 12.3 (task 12, step 3)
          spec     auth, orbiter:api/auth          section  auth#tokens__rotation, auth#<pin>, auth@3#...
          note     41
status    todo | in-progress | done | cancelled   aliases: wip doing started / complete finished / cancel dropped
          filters: open, closed, all, comma lists (todo,done)
note kind note | decision | handoff | blocker | question   (spec: approval, build-note)
formats   -f table (default) | plain | tsv | json | jsonl | yaml   --fields a,b   -x   -q   --max-bytes N
scope     project pin is per actor (AGTRACK_ACTOR); ` + "`ctx use`" + ` drops a pin from another context;
          an unqualified write with no resolvable project fails, it never guesses
exit      0 ok  1 error  2 usage  3 not found  4 conflict  5 database  130 interrupted
          spec await/watch: 0 approved  10 comments  11 timeout  12 spec changed  13 channel lost
errors    under -f json|yaml|jsonl: one object on stderr {"error","code","message","ref",...}
retries   --idem KEY on any create: a retry reprints the first result, never double-writes

the six commands an agent actually uses
  agtrack resume                          where am I: task, steps, notes, blockers, claims, recent log
  agtrack next --claim                    take the next doable task (skips claimed and blocked ones)
  agtrack task add "title" -p . --step "one" --step "two" --idem K
  agtrack sub start 12.1 / sub done 12.1  move a step (pulls a todo task into in-progress)
  agtrack task note 12 --kind handoff "what the next agent needs"
  agtrack log add -t 12 "milestone or blocker, and why"

waiting for a human or another agent (pick by harness)
  background notifications (Claude Code): run in the background, react per line
      agtrack spec watch <spec> --timeout 2h          one line per comment/approval, TERMINAL line at the end
      agtrack watch -p . --actor-not self --timeout 1h
  one completion only: background ` + "`agtrack spec await <spec> --until approval --timeout 2h`" + `
  no background tasks (one-shot runners): run ` + "`spec await`" + ` in the foreground; the turn ends waiting
  nothing waiting at all: the signal is durable; ` + "`agtrack resume`" + ` shows it next time
`

func cmdGrammar(c *Ctx) error {
	if c.structured() {
		return c.emitObject(row.Of("grammar", grammarText))
	}
	fmt.Fprint(c.Out, grammarText)
	return nil
}

func cmdDescribe(c *Ctx) error {
	p := NewParser()
	path := c.Strs("command")
	if len(path) > 0 && p.Find(path) == nil {
		return errs.NotFound("command", strings.Join(path, " "), "no such command: %s", strings.Join(path, " "))
	}
	schema := p.Describe(path)
	b, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return err
	}
	if c.Format == "yaml" {
		var generic any
		_ = json.Unmarshal(b, &generic)
		fmt.Fprint(c.Out, render.ToYAML(generic))
		return nil
	}
	c.Out.Write(b)
	fmt.Fprintln(c.Out)
	return nil
}
