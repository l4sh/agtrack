package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/l4sh/agtrack/internal/db"
)

// checkFixture builds a realistic database: 3 contexts, 30 projects with
// subprojects, 600 tasks and ~400 rules spread over every scope.
func checkFixture(tb testing.TB) (*Store, CheckInput) {
	tb.Helper()
	d, err := db.Open(filepath.Join(tb.TempDir(), "bench.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { d.Close() })
	s := New(d, "human") // an approver: allow and hard rules land active
	must := func(err error) {
		if err != nil {
			tb.Fatal(err)
		}
	}
	var lastTask int64
	n := 0
	for c := 0; c < 3; c++ {
		ctx := fmt.Sprintf("ctx%d", c)
		cr, err := s.CreateContext(ctx, "")
		must(err)
		_, err = s.AddRule(RuleInput{Kind: "deny", Verb: RuleVerbs[c], Scope: Scope{"context", cr.Int("id")}, Quote: "q", Force: true})
		must(err)
		for p := 0; p < 10; p++ {
			pr, err := s.CreateProject(fmt.Sprintf("p%d", p), ctx, "", nil)
			must(err)
			sub, err := s.CreateProject(fmt.Sprintf("p%d:api", p), ctx, "", nil)
			must(err)

			for t := 0; t < 20; t++ {
				tr, err := s.CreateTask(sub, NewTask{Title: fmt.Sprintf("task %d", t)})
				must(err)
				lastTask = tr.Int("id")
				if t%2 == 0 {
					_, err = s.AddRule(RuleInput{Kind: "remember", Text: fmt.Sprintf("note %d", n), Scope: Scope{"task", lastTask}, Force: true})
					must(err)
					n++
				}
			}
			for i, v := range RuleVerbs {
				in := RuleInput{Kind: "deny", Verb: v, Scope: Scope{"project", pr.Int("id")}, Quote: "q", Overridable: true, Force: true}
				if i%3 == 0 {
					in.PathGlob = "src/**"
				}
				_, err = s.AddRule(in)
				must(err)
			}
			_, err = s.AddRule(RuleInput{Kind: "allow", Verb: "push", Scope: Scope{"project", sub.Int("id")}, Quote: "q", Force: true})
			must(err)
		}
	}
	for _, v := range RuleVerbs {
		_, err := s.AddRule(RuleInput{Kind: "require", Check: "branch-not-base", Verb: v, Scope: Scope{Type: "global"}, Force: true})
		must(err)
	}

	return s, CheckInput{Verb: "push", Point: Point{TaskID: lastTask}, Repo: "github.com/acme/x", Branch: "feat/x",
		Paths: []string{"src/a.go"}, IsBase: func(b string) bool { return b == "main" }}
}

func TestCheckIsFast(t *testing.T) {
	s, in := checkFixture(t)
	if n := s.count("SELECT COUNT(*) FROM rules"); n < 300 {
		t.Fatalf("fixture has %d rules", n)
	}
	res, err := s.Check(in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Str("decision") != DecisionAllow {
		t.Fatalf("decision: %v", res.Get("decision"))
	}
	start := time.Now()
	const runs = 20
	for i := 0; i < runs; i++ {
		if _, err := s.Check(in); err != nil {
			t.Fatal(err)
		}
	}
	if per := time.Since(start) / runs; per > 25*time.Millisecond {
		t.Fatalf("check takes %v; hooks need well under 100ms", per)
	}
}

func BenchmarkCheck(b *testing.B) {
	s, in := checkFixture(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Check(in); err != nil {
			b.Fatal(err)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	for _, c := range []struct {
		pat, path string
		want      bool
	}{
		{"src/**", "src/a/b.go", true},
		{"src/", "src/a.go", true},
		{"src", "src/a.go", true},
		{"src", "srcx/a.go", false},
		{"*.sql", "db/m/001.sql", true},
		{"db/*.sql", "db/m/001.sql", false},
		{"db/**/*.sql", "db/m/001.sql", true},
		{"db/**/*.sql", "db/001.sql", true},
		{"?.go", "a.go", true},
	} {
		if got := GlobMatch(c.pat, c.path); got != c.want {
			t.Errorf("GlobMatch(%q, %q) = %v", c.pat, c.path, got)
		}
	}
}

func TestJaccard(t *testing.T) {
	if j := Jaccard("pnpm install needs node 20", "pnpm install requires node 20"); j < SimilarityThreshold {
		t.Errorf("near duplicate: %v", j)
	}
	if j := Jaccard("never push to main", "the staging db is shared"); j >= SimilarityThreshold {
		t.Errorf("unrelated: %v", j)
	}
}
