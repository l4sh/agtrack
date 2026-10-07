package web

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/row"
)

// Browse backs `agtrack serve`: the browse app's reads and writes, the spec
// resolver for /s/{spec} and the all-events stream. The CLI implements it;
// every write runs the same verb handler the CLI runs, attributed to --as.
type Browse interface {
	// ResolveSpec resolves a spec ref (context-qualified: ctx/project/slug)
	// to its id and its context-qualified project.
	ResolveSpec(ref string) (id int64, project string, err error)
	// Read answers a GET under /api/browse/.
	Read(req BrowseRequest) (any, error)
	// Write performs a POST under /api/browse/.
	Write(req BrowseRequest) (any, error)
	// EventsAfter lists every event after the cursor, oldest first.
	EventsAfter(after int64, limit int) ([]*row.Row, error)
}

// BrowseRequest is one browse API call, routed but not yet interpreted.
type BrowseRequest struct {
	// Op names the endpoint, e.g. "task", "task.status", "note.reply".
	Op string
	// Ref is the path parameter: context name, project ref, task id, step
	// ref or note id ("" for collection endpoints).
	Ref string
	// Query holds read filters, named like the CLI flags.
	Query url.Values
	// In is the decoded body of a write.
	In BrowseInput
	// Viewer is the --as actor (for marking the human's own rows).
	Viewer string
}

// BrowseInput is the union of every write's JSON body; each endpoint reads
// the fields it documents in docs/viewer-api.md and ignores the rest.
type BrowseInput struct {
	Title     string   `json:"title"`
	Expected  *string  `json:"expected"` // compare-and-set: the title the page last saw
	Tags      []string `json:"tags"`
	Add       []string `json:"add"`
	Remove    []string `json:"remove"`
	Note      string   `json:"note"`
	Steps     []string `json:"steps"`
	Status    string   `json:"status"`
	Kind      string   `json:"kind"`
	Body      string   `json:"body"`
	On        string   `json:"on"`
	Rationale string   `json:"rationale"`
	Idem      string   `json:"idem"`
}

// browseRoutes maps "METHOD kind[/action]" to an op. {ref} sits between kind
// and action: tasks/{id}/status is "POST tasks/status".
var browseRoutes = map[string]string{
	"GET contexts":       "home",
	"GET contexts/*":     "context",
	"GET projects/*":     "project",
	"GET projects/tasks": "project.tasks",
	"GET projects/specs": "project.specs",
	"GET projects/notes": "project.notes",
	"GET projects/prs":   "project.prs",
	"GET tasks/*":        "task",
	"GET notes":          "notes",
	"GET log":            "log",

	"POST projects/tasks": "task.add",
	"POST projects/notes": "project.note",
	"POST projects/log":   "project.log",
	"POST tasks/title":    "task.title",
	"POST tasks/tags":     "task.tags",
	"POST tasks/status":   "task.status",
	"POST tasks/release":  "task.release",
	"POST tasks/block":    "task.block",
	"POST tasks/unblock":  "task.unblock",
	"POST tasks/steps":    "steps.add",
	"POST tasks/notes":    "task.note",
	"POST tasks/log":      "task.log",
	"POST steps/title":    "step.title",
	"POST steps/status":   "step.status",
	"POST notes/reply":    "note.reply",
	"POST notes/resolve":  "note.resolve",
	"POST notes/reopen":   "note.reopen",
}

// browseAPI routes /api/browse/{kind}[/{ref}[/{action}]]. {ref} is one
// URL-escaped segment (a project ref like acme/orbiter:api is sent as
// acme%2Forbiter%3Aapi).
func (s *Server) browseAPI(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	if len(parts) > 3 {
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}
	key := r.Method + " " + parts[0]
	var ref string
	if len(parts) >= 2 {
		var err error
		if ref, err = url.PathUnescape(parts[1]); err != nil || ref == "" {
			writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
			return
		}
		if len(parts) == 3 {
			key += "/" + parts[2]
		} else {
			key += "/*"
		}
	}
	op, ok := browseRoutes[key]
	if !ok {
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or POST only")
			return
		}
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
		return
	}
	req := BrowseRequest{Op: op, Ref: ref, Query: r.URL.Query(), Viewer: s.o.Viewer}
	if r.Method == http.MethodGet {
		s.locked(w, func() (any, error) { return s.o.Browse.Read(req) })
		return
	}
	if !requireJSON(w, r) || !decode(w, r, &req.In) {
		return
	}
	var obj any
	var err error
	s.withLock(func() { obj, err = s.o.Browse.Write(req) })
	if err != nil {
		if e, ok := errs.As(err); ok && e.Code == errs.ExitConflict && e.Extra != nil {
			// A stale compare-and-set: return both versions so the page can
			// show them side by side.
			obj := row.Of("error", e.Token, "message", e.Message)
			for k, v := range e.Extra {
				obj.Set(k, v)
			}
			writeJSON(w, http.StatusConflict, obj)
			return
		}
		storeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, obj)
}
