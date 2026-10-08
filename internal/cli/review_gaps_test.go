package cli

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/l4sh/agtrack/internal/db"
	"github.com/l4sh/agtrack/internal/store"
)

// T7: LastEventByTask over more ids than one query takes (it chunks by 500).
func TestLastEventByTaskManyTasks(t *testing.T) {
	e := newEnv(t)
	e.actor = "agent1"
	e.ok("ctx", "add", "acme", "--use")
	e.ok("project", "add", "big")
	d, err := db.Open(e.db)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := store.New(d, "agent1")
	s.Context = "acme"
	p, err := s.ResolveProject("big", "", true)
	if err != nil {
		t.Fatal(err)
	}
	const n = 1203 // three chunks, the last one partial
	var ids []int64
	for i := 0; i < n; i++ {
		task, err := s.CreateTask(p, store.NewTask{Title: fmt.Sprintf("task %d", i)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, task.Int("id"))
	}
	// The latest event of a task in each chunk is someone else's.
	bob := store.New(d, "bob")
	marked := map[int64]bool{ids[0]: true, ids[499]: true, ids[500]: true, ids[1000]: true, ids[n-1]: true}
	for id := range marked {
		if _, err := bob.AddNote(store.NewNote{TargetType: "task", TargetID: id, Body: "seen", TaskID: id, ProjectID: p.Int("id")}); err != nil {
			t.Fatal(err)
		}
	}
	last, err := s.LastEventByTask(ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != n {
		t.Fatalf("%d tasks with a last event, want %d", len(last), n)
	}
	for _, id := range ids {
		want := "agent1"
		if marked[id] {
			want = "bob"
		}
		if got := last[id].Str("actor"); got != want {
			t.Errorf("task %d: last actor %q, want %q", id, got, want)
		}
	}
}

// T7: resolving a spec note that is already resolved sweeps the replies
// added since, and leaves the root's own resolution as it was.
func TestResolveResolvedRootSweepsNewReplies(t *testing.T) {
	e := newSpecEnv(t)
	e.ok("spec", "add", e.file)
	e.ok("spec", "note", "auth#tokens__rotation", "per client?")
	rootID := strconv.Itoa(int(obj(list(e.json("spec", "notes", "auth#tokens__rotation"))[0])["id"].(float64)))
	e.ok("note", "reply", rootID, "first reply")
	e.ok("note", "resolve", rootID, "--note", "settled")
	before := obj(e.json("note", "show", rootID))

	late := strconv.Itoa(int(obj(e.json("note", "reply", rootID, "a reply after the resolution"))["id"].(float64)))
	if obj(e.json("note", "show", late))["resolved_at"] != nil {
		t.Fatal("a new reply started resolved")
	}
	out := e.ok("note", "resolve", rootID, "--note", "again")
	if !strings.Contains(out, "1 replies resolved with it") && !strings.Contains(out, "1 reply resolved with it") {
		t.Errorf("second resolve output %q", out)
	}
	if obj(e.json("note", "show", late))["resolved_at"] == nil {
		t.Error("the open reply of an already-resolved root was not swept")
	}
	after := obj(e.json("note", "show", rootID))
	if after["resolution"] != "settled" || after["resolved_at"] != before["resolved_at"] {
		t.Errorf("root resolution changed: %v at %v, was %v at %v", after["resolution"], after["resolved_at"], before["resolution"], before["resolved_at"])
	}
	evs := 0
	for _, ev := range list(e.json("log", "-n", "50")) {
		if m := obj(ev); m["action"] == "note-resolved" && fmt.Sprint(m["detail"]) == "resolved with #"+rootID {
			evs++
		}
	}
	if evs != 2 {
		t.Errorf("cascade events = %d, want 2 (one per reply)", evs)
	}
	// Nothing left to sweep: a third resolve changes nothing.
	if out := e.ok("note", "resolve", rootID); strings.Contains(out, "resolved with it") {
		t.Errorf("third resolve: %q", out)
	}
}
