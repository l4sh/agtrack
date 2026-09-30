package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/l4sh/agtrack/internal/cli/argp"
	"github.com/l4sh/agtrack/internal/errs"
	"github.com/l4sh/agtrack/internal/render"
	"github.com/l4sh/agtrack/internal/row"
	"github.com/l4sh/agtrack/internal/store"
	"github.com/l4sh/agtrack/internal/web"
)

func specTree() *argp.Command {
	proj := argp.Flag{Long: "--project", Short: "-p", Metavar: "REF", Help: "project, project:subproject or . for the cwd repo"}
	waitFlags := func(extra ...argp.Flag) []argp.Flag {
		return append([]argp.Flag{
			{Long: "--timeout", Metavar: "DURATION", Default: "2h", Help: "give up after this long (exit 11)"},
			{Long: "--after", Kind: argp.Int, Metavar: "CURSOR", Help: "start after this event id (default: now)"},
			{Long: "--actor-not", Metavar: "NAME", Help: "suppress events by this actor; `self` = your actor"},
			{Long: "--interval", Metavar: "DURATION", Default: "1s", Help: "poll interval"},
		}, extra...)
	}
	return &argp.Command{Name: "spec", Aliases: []string{"specs"}, Help: "specs: markdown documents addressable by section", SubRequired: true,
		Subs: []*argp.Command{
			{Name: "add", Help: "import a markdown file as a spec (the file is not modified)", Run: Handler(specAdd),
				Args:  []argp.Arg{{Name: "file"}},
				Flags: []argp.Flag{proj, {Long: "--slug", Help: "spec slug (default: the file name)"}, {Long: "--title"}}},
			{Name: "sync", Help: "re-import the spec's file as a new revision, reconciling sections", Run: Handler(specSync),
				Args: []argp.Arg{{Name: "spec"}},
				Flags: []argp.Flag{{Long: "--dry-run", Kind: argp.Bool, Help: "report what would change; write nothing"},
					{Long: "--force", Kind: argp.Bool, Help: "import even with uncommitted changes to the file"},
					{Long: "--strict", Kind: argp.Bool, Help: "exit 4, importing nothing, on any orphan or a rename below 0.9 confidence"}}},
			{Name: "status", Help: "compare each spec's file with its head revision", Run: Handler(specStatus), Flags: []argp.Flag{proj}},
			{Name: "ls", Aliases: []string{"list"}, Help: "list specs", Run: Handler(specLs), Flags: append([]argp.Flag{proj}, listFlags()...)},
			{Name: "toc", Help: "a spec's sections: slug, number, heading, open notes, tasks, pin", Run: Handler(specToc),
				Args: []argp.Arg{{Name: "spec"}}, Flags: []argp.Flag{{Long: "--depth", Kind: argp.PositiveInt, Metavar: "N"}}},
			{Name: "show", Help: "a section's text (body only), or a spec's summary", Run: Handler(specShow),
				Args: []argp.Arg{{Name: "ref", Help: "spec, spec#section, spec@N#section"}},
				Flags: []argp.Flag{{Long: "--with", Metavar: "LIST", Help: "notes,tasks,children,context"},
					{Long: "--subtree", Kind: argp.Bool, Help: "include descendant sections"},
					{Long: "--ancestors", Dest: "with_context", Kind: argp.Bool, Help: "include the text of ancestor sections (= --with context; --context is the global context flag)"}}},
			{Name: "find", Help: "full-text search over head revisions", Run: Handler(specFind),
				Args: []argp.Arg{{Name: "words", NArgs: "+", Help: "spec then query, or just the query with -p"}},
				Flags: []argp.Flag{proj, {Long: "--top", Kind: argp.PositiveInt, Default: 10, Metavar: "N"},
					{Long: "--with", Metavar: "LIST", Help: "notes,tasks"}, {Long: "--anchors", Kind: argp.Bool, Help: "print bare section refs"},
					{Long: "--query-syntax", Choices: []string{"literal", "fts5"}, Default: "literal", Help: "fts5 passes the query to FTS5 unescaped"}}},
			{Name: "note", Help: "comment on a section (pins it lazily), or reply with --reply", Run: Handler(specNote),
				Args: []argp.Arg{{Name: "section", Help: "spec#section (with --reply: the first word of the body)"}, {Name: "body", NArgs: "*"}},
				Flags: []argp.Flag{{Long: "--kind", Choices: store.SpecNoteKinds, Help: "note (default), question, decision, blocker, handoff, build-note"},
					{Long: "--quote", Help: "the exact text the note is about"}, {Long: "--prefix", Help: "text just before the quote"},
					{Long: "--suffix", Help: "text just after the quote"}, {Long: "--reply", Kind: argp.Int, Metavar: "ID", Help: "reply to a note"}}},
			{Name: "notes", Help: "comments on a spec or a section", Run: Handler(specNotes),
				Args: []argp.Arg{{Name: "ref"}}, Flags: append([]argp.Flag{{Long: "--open", Kind: argp.Bool, Help: "unresolved only"}}, listFlags()...)},
			{Name: "check", Help: "open notes, drift and tasks on a section", Run: Handler(specCheck), Args: []argp.Arg{{Name: "section"}}},
			{Name: "link", Help: "link a section to a task (pins it lazily)", Run: Handler(specLink),
				Args: []argp.Arg{{Name: "section"}, {Name: "task"}},
				Flags: []argp.Flag{{Long: "--kind", Choices: store.SpecLinkKinds, Default: "implements"},
					{Long: "--subtree", Kind: argp.Bool, Help: "the link covers the section's descendants too"}}},
			{Name: "unlink", Help: "remove a task's links to a section", Run: Handler(specUnlink),
				Args: []argp.Arg{{Name: "section"}, {Name: "task"}}, Flags: []argp.Flag{{Long: "--kind", Choices: store.SpecLinkKinds}}},
			{Name: "coverage", Help: "sections with text that no task implements", Run: Handler(specCoverage), Args: []argp.Arg{{Name: "spec"}}},
			{Name: "stale", Help: "task links whose section changed since the task agreed to it", Run: Handler(specStale),
				Args: []argp.Arg{{Name: "spec"}}, Flags: []argp.Flag{{Long: "--reopen", Kind: argp.Bool, Help: "reopen done tasks whose section changed"}}},
			{Name: "relink", Help: "move notes and task links off a retired section, explicitly", Run: Handler(specRelink),
				Args: []argp.Arg{{Name: "section"}},
				Flags: []argp.Flag{{Long: "--into", Metavar: "SECTION"}, {Long: "--kind", Choices: []string{"merged", "renamed", "moved"}},
					{Long: "--split-into", Metavar: "A,B", Help: "copy task links to each; notes stay on the retired section"}}},
			{Name: "orphans", Help: "anchors that lost their section, with what they hold", Run: Handler(specOrphans), Args: []argp.Arg{{Name: "spec"}}},
			{Name: "diff", Help: "section-level changes between two revisions (vN..vM)", Run: Handler(specDiff),
				Args: []argp.Arg{{Name: "spec"}, {Name: "range", NArgs: "?", Help: "v3..v5, or v3 for v3..head (default: previous..head)"}}},
			{Name: "gc", Help: "drop old revisions nothing references", Run: Handler(specGC),
				Args: []argp.Arg{{Name: "spec"}}, Flags: []argp.Flag{{Long: "--keep", Kind: argp.PositiveInt, Default: 20, Metavar: "N"}}},
			{Name: "render", Help: "the full document model as JSON (the viewer's API object)", Run: Handler(specRender), Args: []argp.Arg{{Name: "spec"}}},
			{Name: "approve", Help: "approve the head revision (releases pending comments)", Run: Handler(specApprove),
				Args:  []argp.Arg{{Name: "spec"}},
				Flags: []argp.Flag{{Long: "--note", Help: "approval note"}, {Long: "--build-note", Kind: argp.Append, Metavar: "TEXT", Help: "guidance for the build; repeatable"}}},
			{Name: "release", Help: "send accumulated comments to waiting agents (manual mode)", Run: Handler(specRelease), Args: []argp.Arg{{Name: "spec"}}},
			{Name: "build-note", Help: "guidance for the build that is not part of the spec", Run: Handler(specBuildNote),
				Args: []argp.Arg{{Name: "spec"}, {Name: "body", NArgs: "+"}}},
			{Name: "watch", Help: "stream review events on a spec, one line each, until approval or timeout", Run: Handler(specWatch),
				Args: []argp.Arg{{Name: "spec"}}, Flags: waitFlags()},
			{Name: "await", Help: "wait for one review outcome and print it (exit 0 approved, 10 comments, 11 timeout, 12 changed, 13 lost)", Run: Handler(specAwait),
				Args:  []argp.Arg{{Name: "spec"}},
				Flags: waitFlags(argp.Flag{Long: "--until", Choices: []string{"approval", "comments", "release"}, Default: "approval"})},
			{Name: "serve", Help: "review a spec in the browser (127.0.0.1 only)", Run: Handler(specServe),
				Args: []argp.Arg{{Name: "spec"}},
				Flags: []argp.Flag{{Long: "--port", Kind: argp.Int, Default: 0, Help: "port (default: random)"},
					{Long: "--idle", Metavar: "DURATION", Default: "30m", Help: "exit after this long without requests"},
					{Long: "--open", Kind: argp.Bool, Help: "open the page in a browser"}}},
		}}
}

// --- helpers ------------------------------------------------------------------------------

func cwdRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return store.GitToplevel(wd)
}

func (c *Ctx) spec(k string) (*row.Row, error) { return c.S.ResolveSpec(c.Str(k), c.Context) }

func (c *Ctx) warnf(format string, a ...any) {
	fmt.Fprintf(c.Err, "agtrack: "+format+"\n", a...)
}

func (c *Ctx) pinnedLine(pinned, rel string, modified bool) {
	if pinned == "" {
		return
	}
	if modified {
		c.warnf("pinned this section as `%s`; %s modified (1 line), review it", pinned, rel)
	} else {
		c.warnf("pinned this section as `%s`", pinned)
	}
}

func withSet(v string) map[string]bool {
	out := map[string]bool{}
	for _, p := range splitList(v) {
		out[p] = true
	}
	return out
}

func pad(s string, n int) string {
	if w := len([]rune(s)); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

func idList(v any) string {
	ids, _ := v.([]any)
	if len(ids) == 0 {
		return "-"
	}
	var parts []string
	for _, id := range ids {
		parts = append(parts, fmt.Sprint(id))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// --- add / sync / status / ls ---------------------------------------------------------------

func specAdd(c *Ctx) error {
	project, err := c.resolveProject(c.Str("project"), true)
	if err != nil {
		return err
	}
	res, err := c.S.AddSpec(store.SpecAddInput{File: c.Str("file"), Slug: c.Str("slug"), Title: c.Str("title"), Project: project})
	if err != nil {
		return err
	}
	for _, w := range res.Warnings {
		c.warnf("warning: %s", w)
	}
	if c.structured() {
		obj := res.Spec.Copy()
		obj.Set("sections", int64(res.Sections))
		obj.Set("anchors_written", int64(0))
		obj.Set("file_modified", false)
		obj.Set("warnings", nonNilS(res.Warnings))
		return c.emitObject(obj)
	}
	if c.Quiet {
		c.println(res.Spec.Str("slug"))
		return nil
	}
	c.printf("%s  v1  %d sections indexed, 0 anchors written (%s unmodified)\n", res.Spec.Str("slug"), res.Sections, res.File)
	return nil
}

func nonNilS(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func specSync(c *Ctx) error {
	sp, err := c.spec("spec")
	if err != nil {
		return err
	}
	res, err := c.S.SyncSpec(sp.Int("id"), store.SpecSyncInput{DryRun: c.Bool("dry_run"), Force: c.Bool("force"), Strict: c.Bool("strict"), RepoRoot: cwdRoot()})
	if err != nil {
		if e, ok := errs.As(err); ok && c.Bool("strict") && !c.structured() {
			if lines, ok := e.Extra["changes"].([]*row.Row); ok {
				for _, l := range lines {
					c.println(syncLine(l, sp.Str("slug")))
				}
			}
		}
		return err
	}
	for _, w := range res.Warnings {
		c.warnf("warning: %s", w)
	}
	slug := sp.Str("slug")
	if c.structured() {
		obj := row.Of("spec", slug, "changed", res.Changed, "dry_run", c.Bool("dry_run"), "base_revision", res.Base, "revision", res.Version,
			"sha", res.SHA, "sections", int64(res.Sections), "restored_pins", int64(res.Restored), "changes", nonNilRows(res.Lines))
		if res.Counts != nil {
			obj.Set("counts", res.Counts)
		}
		return c.emitObject(obj)
	}
	switch {
	case res.Restored > 0:
		c.printf("%s: restored %s to %s (v%d)\n", slug, plural(int64(res.Restored), "pin comment", "pin comments"), res.File, res.Version)
		return nil
	case !res.Changed:
		c.printf("%s: %s unchanged (v%d)\n", slug, res.File, res.Version)
		return nil
	}
	if res.Base > 0 {
		c.printf("%s: %s sha %s != head v%d sha %s\n", slug, res.File, shortSHA(res.SHA), res.Base, shortSHA(res.BaseSHA))
	}
	for _, l := range res.Lines {
		c.println(syncLine(l, slug))
	}
	summary := fmt.Sprintf("v%d: %d sections (%s)", res.Version, res.Sections, countsLine(res.Counts))
	if c.Bool("dry_run") {
		c.println("dry run: would import " + summary)
	} else {
		c.println("imported " + summary)
	}
	if res.HeldOrphans > 0 {
		c.warnf("%s holds annotations; `agtrack spec orphans %s` lists them, `agtrack spec relink` moves them",
			plural(int64(res.HeldOrphans), "orphaned anchor", "orphaned anchors"), slug)
	}
	return nil
}

func nonNilRows(r []*row.Row) []*row.Row {
	if r == nil {
		return []*row.Row{}
	}
	return r
}

func shortSHA(s string) string {
	if len(s) > 4 {
		return s[:4] + "…"
	}
	return s
}

func countsLine(c *row.Row) string {
	if c == nil {
		return ""
	}
	var parts []string
	for _, k := range c.Keys() {
		if n := c.Int(k); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, k))
		}
	}
	if len(parts) == 0 {
		return "no structural changes"
	}
	return strings.Join(parts, ", ")
}

func syncLine(l *row.Row, slug string) string {
	s := l.Str("slug")
	switch l.Str("change") {
	case "renamed":
		conf := fmt.Sprintf("(%.2f)", l.Get("confidence"))
		if l.Str("tier") == "pinned" {
			conf = "(pinned @" + l.Str("key") + ")"
		}
		target := ""
		if l.Str("new_slug") != s {
			target = " -> " + l.Str("new_slug")
		}
		return fmt.Sprintf("  ~ renamed    %s%s   %q -> %q  %s", s, target, l.Str("heading"), l.Str("new_heading"), conf)
	case "moved":
		return fmt.Sprintf("  > moved      %s   %s -> %s  (now %s)", s, l.Str("number"), l.Str("new_number"), l.Str("new_slug"))
	case "new":
		return fmt.Sprintf("  + new        %s   %q", s, l.Str("heading"))
	case "removed":
		return fmt.Sprintf("  - removed    %s   %q", s, l.Str("heading"))
	case "unpinned":
		return fmt.Sprintf("  ! unpinned   %s   pin comment `%s` removed from the file; the anchor is kept by path", s, l.Str("key"))
	case "revived":
		return fmt.Sprintf("  * revived    @%s", l.Str("key"))
	case "orphaned":
		if s == "" {
			s = "@" + l.Str("key")
		}
		line := fmt.Sprintf("  ? orphaned   %s   %s (%d unresolved), %s", s, plural(l.Int("notes"), "note", "notes"), l.Int("open_notes"),
			plural(int64(len(l.Get("tasks").([]any))), "task link", "task links"))
		if ids := l.Get("tasks").([]any); len(ids) > 0 {
			var t []string
			for _, id := range ids {
				t = append(t, fmt.Sprintf("#%d", id))
			}
			line += " (" + strings.Join(t, ", ") + ")"
		}
		if lk := l.Str("likely"); lk != "" {
			line += fmt.Sprintf("; looks like %s: `agtrack spec relink %s#%s --into %s --kind renamed`", lk, slug, l.Str("key"), lk)
		}
		return line
	}
	return "  " + l.Str("change") + " " + s
}

func specStatus(c *Ctx) error {
	var project *row.Row
	if c.Has("project") {
		var err error
		if project, err = c.resolveProject(c.Str("project"), true); err != nil {
			return err
		}
	}
	rows, err := c.S.SpecStatus(project, c.Context, cwdRoot())
	if err != nil {
		return err
	}
	if c.structured() {
		return c.emitObject(rows)
	}
	w1, w2 := 0, 0
	for _, r := range rows {
		w1, w2 = max(w1, len(r.Str("slug"))), max(w2, len(r.Str("source_path")))
	}
	for _, r := range rows {
		c.printf("%s  %s  v%d  %s\n", pad(r.Str("slug"), w1), pad(r.Str("source_path"), w2), r.Int("revision"), r.Str("state"))
	}
	return nil
}

func specLs(c *Ctx) error {
	var project *row.Row
	if c.Has("project") {
		var err error
		if project, err = c.resolveProject(c.Str("project"), true); err != nil {
			return err
		}
	}
	rows, err := c.S.ListSpecs(project, c.Context)
	if err != nil {
		return err
	}
	return c.emit(rows, "spec", len(rows))
}

// --- toc / show / find ------------------------------------------------------------------------

func specToc(c *Ctx) error {
	sr, err := store.ParseSpecRef(c.Str("spec"))
	if err != nil {
		return err
	}
	sp, err := c.spec("spec")
	if err != nil {
		return err
	}
	rows, err := c.S.SpecToc(sp.Int("id"), sr.Version, c.Int("depth"))
	if err != nil {
		return err
	}
	if c.structured() {
		return c.emitObject(row.Of("spec", sp.Str("slug"), "title", sp.Get("title"), "revision", sp.Int("revision"), "sections", nonNilRows(rows)))
	}
	state := "clean"
	if st, err := c.S.SpecStatus(nil, c.Context, cwdRoot()); err == nil {
		for _, r := range st {
			if r.Str("ref") == sp.Str("ref") {
				state = strings.SplitN(r.Str("state"), ":", 2)[0]
			}
		}
	}
	rev := sp.Int("revision")
	if sr.Version > 0 {
		rev = int64(sr.Version)
	}
	c.printf("%s  %s  v%d  (%s, %s)\n", sp.Str("slug"), sp.Str("title"), rev, sp.Str("source_path"), state)
	ws, wn, wh := 0, 0, 0
	for _, r := range rows {
		ws = max(ws, 2*int(r.Int("depth"))+len(r.Str("slug")))
		wn = max(wn, len(r.Str("number")))
		wh = max(wh, len([]rune(r.Str("heading"))))
	}
	for _, r := range rows {
		ind := strings.Repeat("  ", int(r.Int("depth")))
		heading := r.Str("heading")
		if r.Str("kind") == "block" {
			heading = "(block)"
		}
		notes := ""
		if n := r.Int("open_notes"); n > 0 {
			notes = fmt.Sprintf("!%d", n)
		}
		pin := ""
		if p := r.Str("pin"); p != "" {
			pin = "@" + p
		}
		line := fmt.Sprintf("%s  %s  %s  %s  %s  %s", pad(ind+r.Str("slug"), ws), pad(r.Str("number"), wn), pad(heading, wh), pad(notes, 3), idList(r.Get("tasks")), pin)
		c.println(strings.TrimRight(line, " "))
	}
	return nil
}

func specShow(c *Ctx) error {
	sr, err := store.ParseSpecRef(c.Str("ref"))
	if err != nil {
		return err
	}
	if sr.Section == "" {
		return specShowSpec(c, sr)
	}
	hit, err := c.S.ResolveSection(c.Str("ref"), c.Context)
	if err != nil {
		return c.retired(err)
	}
	if hit.Positional {
		first := hit.Sections[0]
		c.warnf("warning: %s is positional and changes when sections move; use %s#%s", sr.Section, hit.Spec.Str("slug"), first.Str("path_slug"))
	}
	with := withSet(c.Str("with"))
	var objs []*row.Row
	for _, sec := range hit.Sections {
		obj, err := c.sectionObject(hit, sec, with)
		if err != nil {
			return err
		}
		objs = append(objs, obj)
	}
	if c.structured() {
		if len(objs) == 1 {
			return c.emitObject(objs[0])
		}
		return c.emitObject(objs)
	}
	for i, obj := range objs {
		if len(objs) > 1 {
			if i > 0 {
				c.println()
			}
			c.printf("== %s#%s  %s %s\n", obj.Str("spec"), obj.Str("section"), obj.Str("number"), obj.Str("heading"))
		}
		c.printSection(obj)
	}
	return nil
}

func (c *Ctx) sectionObject(hit *store.SectionHit, sec *row.Row, with map[string]bool) (*row.Row, error) {
	obj := row.Of("spec", hit.Spec.Str("slug"), "revision", hit.Version.Int("n"), "section", sec.Str("path_slug"), "number", sec.Str("number"),
		"heading", sec.Str("heading"), "path", sec.Str("path"), "pin", sec.Get("pin"), "kind", sec.Str("kind"), "level", sec.Int("level"),
		"lines", []any{sec.Int("line_start"), sec.Int("line_end")}, "body", sec.Str("body"))
	anc, err := c.S.SectionAncestors(sec.Int("id"))
	if err != nil {
		return nil, err
	}
	var ancRefs []string
	omitted := false
	var ctxRows []*row.Row
	for _, a := range anc {
		ancRefs = append(ancRefs, a.Str("path_slug"))
		if strings.TrimSpace(a.Str("body")) != "" {
			omitted = true
			ctxRows = append(ctxRows, row.Of("section", a.Str("path_slug"), "heading", a.Str("heading"), "body", a.Str("body")))
		}
	}
	obj.Set("ancestors", nonNilS(ancRefs))
	if c.Bool("with_context") || with["context"] {
		obj.Set("context", nonNilRows(ctxRows))
		omitted = false
	}
	obj.Set("context_omitted", omitted)
	if c.Bool("subtree") {
		sub, err := c.S.SectionSubtree(sec.Int("id"))
		if err != nil {
			return nil, err
		}
		var rows []*row.Row
		for _, d := range sub[1:] {
			rows = append(rows, row.Of("section", d.Str("path_slug"), "number", d.Str("number"), "heading", d.Str("heading"),
				"kind", d.Str("kind"), "level", d.Int("level"), "pin", d.Get("pin"), "body", d.Str("body")))
		}
		obj.Set("subtree", nonNilRows(rows))
	}
	if with["notes"] || with["tasks"] || with["children"] {
		extra, err := c.S.SectionExtras(hit.Spec.Int("id"), sec.Int("id"), with)
		if err != nil {
			return nil, err
		}
		for _, k := range extra.Keys() {
			obj.Set(k, extra.Get(k))
		}
	}
	return obj, nil
}

func (c *Ctx) printSection(obj *row.Row) {
	if ctxRows, ok := obj.Get("context").([]*row.Row); ok {
		for _, a := range ctxRows {
			c.printf("[context %s: %s]\n%s\n\n", a.Str("section"), a.Str("heading"), a.Str("body"))
		}
	}
	if b := obj.Str("body"); b != "" {
		c.println(b)
	}
	if sub, ok := obj.Get("subtree").([]*row.Row); ok {
		for _, d := range sub {
			if d.Str("kind") == "block" {
				c.printf("\n<!-- agtrack:block %s -->\n", d.Str("pin"))
			} else {
				c.printf("\n%s %s\n", strings.Repeat("#", int(d.Int("level"))), d.Str("heading"))
			}
			if d.Str("body") != "" {
				c.printf("\n%s\n", d.Str("body"))
			}
		}
	}
	if obj.Get("context_omitted") == true {
		c.warnf("ancestors %s carry text not shown here; add --ancestors", strings.Join(obj.Get("ancestors").([]string), ", "))
	}
	if notes, ok := obj.Get("notes").([]*row.Row); ok {
		c.println("\nnotes:")
		if len(notes) == 0 {
			c.println("  (none)")
		}
		for _, n := range notes {
			c.printNote(n, false)
		}
	}
	if tasks, ok := obj.Get("tasks").([]*row.Row); ok {
		c.println("\ntasks:")
		if len(tasks) == 0 {
			c.println("  (none)")
		}
		for _, t := range tasks {
			c.printf("  #%d  %-11s  %-10s  %s\n", t.Int("id"), t.Str("status"), t.Str("kind"), t.Str("title"))
		}
	}
	if ch, ok := obj.Get("children").([]*row.Row); ok {
		c.println("\nchildren:")
		if len(ch) == 0 {
			c.println("  (none)")
		}
		for _, d := range ch {
			c.printf("  %s  %s  %s\n", d.Str("section"), d.Str("number"), d.Str("heading"))
		}
	}
}

var noteMarks = map[string]string{"question": "?", "blocker": "!", "decision": "=", "handoff": ">", "build-note": "+", "approval": "✓", "note": "-"}

func (c *Ctx) printNote(n *row.Row, withSection bool) {
	mark := noteMarks[n.Str("kind")]
	if !n.IsNull("parent_id") {
		mark = "↳"
	}
	actor := n.Str("actor")
	if actor == "" {
		actor = "-"
	}
	body := strings.ReplaceAll(n.Str("body"), "\n", " ")
	if withSection && n.IsNull("parent_id") {
		sec := n.Str("section")
		if sec == "" {
			sec = "@" + n.Str("anchor")
		}
		body = sec + ": " + body
	}
	resolved := ""
	if !n.IsNull("resolved_at") {
		resolved = "  (resolved)"
	}
	c.printf("%-4d %s %-8s %s%s\n", n.Int("id"), mark, actor, body, resolved)
	if q := n.Str("quote"); q != "" {
		state := "[still present]"
		if n.Get("quote_ok") != true {
			state = "[no longer found]"
		}
		c.printf("        quote: %q  %s\n", q, state)
	}
}

// retired prints what a section that is no longer at head was, and exits 3.
func (c *Ctx) retired(err error) error {
	e, ok := errs.As(err)
	if !ok || e.Code != errs.ExitNotFound || e.Extra == nil || e.Extra["state"] == nil || c.structured() {
		return err
	}
	x := e.Extra
	line := fmt.Sprintf("state: %v", x["state"])
	if ls, ok := x["last_seen"]; ok {
		line += fmt.Sprintf(" (last seen v%v)", ls)
	}
	if n, ok := x["notes"].(int64); ok {
		line += fmt.Sprintf("   %s (%v unresolved)", plural(n, "note", "notes"), x["open_notes"])
	}
	if t, ok := x["tasks"].([]any); ok && len(t) > 0 {
		var ids []string
		for _, id := range t {
			ids = append(ids, fmt.Sprintf("#%d", id))
		}
		line += "   tasks: " + strings.Join(ids, ", ")
	}
	c.println(line)
	var tail []string
	if s, ok := x["successors"].([]string); ok && len(s) > 0 {
		tail = append(tail, "successors: "+strings.Join(s, ", "))
	}
	if ct, ok := x["content"]; ok {
		tail = append(tail, fmt.Sprintf("content: %v", ct))
	}
	if len(tail) > 0 {
		c.println(strings.Join(tail, "   "))
	}
	return exitCode(errs.ExitNotFound)
}

func specShowSpec(c *Ctx, sr store.SpecRef) error {
	sp, err := c.spec("ref")
	if err != nil {
		return err
	}
	toc, err := c.S.SpecToc(sp.Int("id"), sr.Version, 0)
	if err != nil {
		return err
	}
	a, stale, err := c.S.SpecApproval(sp.Int("id"))
	if err != nil {
		return err
	}
	obj := sp.Copy()
	obj.Delete("repo_root")
	obj.Set("sections", int64(len(toc)))
	var open int64
	for _, r := range toc {
		open += r.Int("open_notes")
	}
	obj.Set("open_notes", open)
	if a != nil {
		obj.Set("approved", a.Get("revision"))
		obj.Set("approval_stale", stale)
	}
	secs := []render.Section{{Label: "spec", Value: obj.Str("ref")}, {Label: "title", Value: obj.Get("title")}, {Label: "status", Value: obj.Str("status")},
		{Label: "revision", Value: obj.Int("revision")}, {Label: "sha", Value: obj.Str("sha")}, {Label: "source", Value: obj.Get("source_path")},
		{Label: "sections", Value: obj.Int("sections")}, {Label: "open notes", Value: open}}
	if a != nil {
		txt := fmt.Sprintf("v%v by %v", a.Get("revision"), strOr(a.Get("by")))
		if stale {
			txt += " (stale)"
		}
		secs = append(secs, render.Section{Label: "approved", Value: txt})
	}
	return c.detail(obj, "spec", secs)
}

func specFind(c *Ctx) error {
	words := c.Strs("words")
	var ids []int64
	if c.Has("project") {
		project, err := c.resolveProject(c.Str("project"), true)
		if err != nil {
			return err
		}
		list, err := c.S.ListSpecs(project, c.Context)
		if err != nil {
			return err
		}
		for _, sp := range list {
			ids = append(ids, sp.Int("id"))
		}
	} else {
		if len(words) < 2 {
			return errs.Usage("spec find needs a spec and a query (or -p REF and a query)")
		}
		sp, err := c.S.ResolveSpec(words[0], c.Context)
		if err != nil {
			return err
		}
		ids, words = []int64{sp.Int("id")}, words[1:]
	}
	rows, err := c.S.SpecFind(ids, strings.Join(words, " "), c.Str("query_syntax") == "fts5", c.Int("top"))
	if err != nil {
		return err
	}
	with := withSet(c.Str("with"))
	if len(with) > 0 {
		for _, r := range rows {
			hit, err := c.S.ResolveSection(r.Str("ref"), c.Context)
			if err != nil || len(hit.Sections) != 1 {
				continue
			}
			extra, err := c.S.SectionExtras(r.Int("spec_id"), hit.Sections[0].Int("id"), with)
			if err != nil {
				return err
			}
			for _, k := range extra.Keys() {
				r.Set(k, extra.Get(k))
			}
		}
	}
	if c.Bool("anchors") {
		for _, r := range rows {
			c.println(r.Str("ref"))
		}
		return nil
	}
	if c.structured() {
		for _, r := range rows {
			r.Delete("spec_id")
			r.Delete("anchor_id")
		}
		return c.emitObject(nonNilRows(rows))
	}
	if len(rows) == 0 {
		c.println("no matches")
		return nil
	}
	w1, w2, w3 := 0, 0, 0
	for _, r := range rows {
		w1, w2, w3 = max(w1, len(r.Str("ref"))), max(w2, len(r.Str("number"))), max(w3, len([]rune(r.Str("heading"))))
	}
	for _, r := range rows {
		c.printf("%s  %s  %s  %5.2f  %s\n", pad(r.Str("ref"), w1), pad(r.Str("number"), w2), pad(r.Str("heading"), w3), r.Get("score"), r.Str("snippet"))
		if notes, ok := r.Get("notes").([]*row.Row); ok {
			for _, n := range notes {
				c.printNote(n, false)
			}
		}
		if tasks, ok := r.Get("tasks").([]*row.Row); ok {
			for _, t := range tasks {
				c.printf("     task #%d %s %s\n", t.Int("id"), t.Str("status"), t.Str("title"))
			}
		}
	}
	return nil
}

// --- notes / check ----------------------------------------------------------------------------

func specNote(c *Ctx) error {
	words := append([]string{c.Str("section")}, c.Strs("body")...)
	in := store.SpecNoteInput{Kind: c.Str("kind"), Quote: c.Str("quote"), Prefix: c.Str("prefix"), Suffix: c.Str("suffix"), RepoRoot: cwdRoot()}
	var specID int64
	if c.Has("reply") {
		in.ParentID = int64(c.Int("reply"))
		in.Body = strings.Join(words, " ")
		id, err := c.S.SpecOfNote(in.ParentID)
		if err != nil {
			return err
		}
		if id == 0 {
			return errs.NotFound("note", fmt.Sprint(in.ParentID), "no such spec note: %d", in.ParentID)
		}
		specID = id
	} else {
		if len(words) < 2 {
			return errs.Usage("spec note needs a section and a body: agtrack spec note auth#tokens__rotation \"...\"")
		}
		sr, err := store.ParseSpecRef(words[0])
		if err != nil {
			return err
		}
		if sr.Section == "" && in.Kind != "build-note" {
			return errs.New("%s names a spec, not a section: use %s#<section>", words[0], words[0])
		}
		sp, err := c.S.ResolveSpec(words[0], c.Context)
		if err != nil {
			return err
		}
		specID, in.Section, in.Body = sp.Int("id"), sr.Section, strings.Join(words[1:], " ")
	}
	note, pinned, modified, err := c.S.SpecAddNote(specID, in)
	var pwe *store.PinWriteError
	if err != nil && !errors.As(err, &pwe) {
		return err
	}
	sp, _ := c.S.SpecRow(specID)
	if c.structured() {
		var p any
		if pinned != "" {
			p = pinned
		}
		if e := c.emitObject(row.Of("note", note, "pinned", p, "file_modified", modified)); e != nil {
			return e
		}
	} else if c.Quiet {
		c.println(note.Int("id"))
	} else {
		where := sp.Str("slug")
		if note.Str("section") != "" {
			where += "#" + note.Str("section")
		} else if note.Str("anchor") != "" {
			where += "#" + note.Str("anchor")
		}
		state := "unresolved"
		if note.Str("kind") == "build-note" {
			state = "build note"
		} else if note.Get("released") != true {
			state += ", held until released"
		}
		if in.ParentID != 0 {
			c.printf("note %d reply to %d on %s (%s)\n", note.Int("id"), in.ParentID, where, state)
		} else {
			c.printf("note %d on %s (%s)\n", note.Int("id"), where, state)
		}
		if q := note.Str("quote"); q != "" && note.Get("quote_ok") != true {
			c.warnf("warning: the quote %q was not found in the section text", q)
		}
	}
	c.pinnedLine(pinned, sp.Str("source_path"), modified)
	if pwe != nil {
		c.warnf("warning: %s", pwe.Error())
	}
	return nil
}

func specNotes(c *Ctx) error {
	sr, err := store.ParseSpecRef(c.Str("ref"))
	if err != nil {
		return err
	}
	sp, err := c.spec("ref")
	if err != nil {
		return err
	}
	rows, err := c.S.SpecNotes(sp.Int("id"), sr.Section, c.Bool("open"))
	if err != nil {
		return err
	}
	if c.Bool("count") {
		c.println(len(rows))
		return nil
	}
	if c.Bool("ids") || c.Quiet {
		for _, r := range rows {
			c.println(r.Int("id"))
		}
		return nil
	}
	if c.structured() {
		return c.emitObject(nonNilRows(rows))
	}
	for _, r := range rows {
		c.printNote(r, sr.Section == "")
	}
	return nil
}

func specCheck(c *Ctx) error {
	hit, err := c.S.ResolveSection(c.Str("section"), c.Context)
	if err != nil {
		return c.retired(err)
	}
	sec := hit.Sections[0]
	if len(hit.Sections) != 1 {
		return errs.New("%s matches %d sections; check one at a time", c.Str("section"), len(hit.Sections))
	}
	extra, err := c.S.SectionExtras(hit.Spec.Int("id"), sec.Int("id"), map[string]bool{"notes": true, "tasks": true})
	if err != nil {
		return err
	}
	stale, err := c.S.SpecStale(hit.Spec.Int("id"))
	if err != nil {
		return err
	}
	var issues []string
	var open []*row.Row
	for _, n := range extra.Get("notes").([]*row.Row) {
		if n.IsNull("resolved_at") && n.Str("kind") != "build-note" {
			open = append(open, n)
		}
	}
	if len(open) > 0 {
		var parts []string
		for _, n := range open {
			parts = append(parts, fmt.Sprintf("%d, %s, %s", n.Int("id"), strOr(n.Get("actor")), n.Str("kind")))
		}
		issues = append(issues, fmt.Sprintf("%s (%s)", plural(int64(len(open)), "unresolved note", "unresolved notes"), strings.Join(parts, "; ")))
	}
	var staleRows []*row.Row
	for _, l := range stale {
		if l.Str("anchor") == sec.Str("pin") && l.Get("stale") == true {
			staleRows = append(staleRows, l)
			issues = append(issues, fmt.Sprintf("section changed in v%d; task #%d was linked at v%d", l.Int("head_revision"), l.Int("task_id"), l.Int("agreed_revision")))
		}
	}
	var tasks []string
	for _, t := range extra.Get("tasks").([]*row.Row) {
		tasks = append(tasks, fmt.Sprintf("#%d (%s)", t.Int("id"), t.Str("status")))
	}
	if c.structured() {
		return c.emitObject(row.Of("section", hit.Spec.Str("slug")+"#"+sec.Str("path_slug"), "revision", hit.Version.Int("n"), "pin", sec.Get("pin"),
			"ok", len(issues) == 0, "issues", nonNilS(issues), "open_notes", nonNilRows(open), "stale", nonNilRows(staleRows), "tasks", extra.Get("tasks")))
	}
	c.printf("%s#%s  v%d\n", hit.Spec.Str("slug"), sec.Str("path_slug"), hit.Version.Int("n"))
	for _, i := range issues {
		c.println("  ! " + i)
	}
	if len(tasks) > 0 {
		c.println("  tasks: " + strings.Join(tasks, ", "))
	}
	if len(issues) == 0 && len(tasks) == 0 {
		c.println("  ok: no open notes, no linked tasks")
	}
	return nil
}

// --- links ---------------------------------------------------------------------------------------

// linkSpec links a section to a task (task add --spec, spec link).
func (c *Ctx) linkSpec(ref string, task *row.Row, kind, scope string) error {
	link, pinned, write, err := c.S.SpecLink(ref, c.Context, task.Int("id"), kind, scope, cwdRoot())
	if err != nil {
		return err
	}
	modified := false
	if write != nil {
		if e := write.Apply(); e != nil {
			c.warnf("warning: %s", (&store.PinWriteError{Rel: write.Rel, Err: e}).Error())
		} else {
			modified = true
		}
	}
	rel := ""
	if write != nil {
		rel = write.Rel
	}
	c.pinnedLine(pinned, rel, modified)
	_ = link
	return nil
}

func specLink(c *Ctx) error {
	task, err := c.S.GetTask(c.Str("task"), true)
	if err != nil {
		return err
	}
	scope := "self"
	if c.Bool("subtree") {
		scope = "subtree"
	}
	link, pinned, write, err := c.S.SpecLink(c.Str("section"), c.Context, task.Int("id"), c.Str("kind"), scope, cwdRoot())
	if err != nil {
		return err
	}
	modified := false
	if write != nil {
		if e := write.Apply(); e != nil {
			c.warnf("warning: %s", (&store.PinWriteError{Rel: write.Rel, Err: e}).Error())
		} else {
			modified = true
		}
	}
	text := fmt.Sprintf("linked #%d to %s#%s (%s, agreed v%v)", task.Int("id"), link.Str("spec"), link.Str("section"), link.Str("kind"), link.Get("agreed_revision"))
	if err := c.mutated(text, link); err != nil {
		return err
	}
	if write != nil {
		c.pinnedLine(pinned, write.Rel, modified)
	} else {
		c.pinnedLine(pinned, "", false)
	}
	return nil
}

func specUnlink(c *Ctx) error {
	task, err := c.S.GetTask(c.Str("task"), true)
	if err != nil {
		return err
	}
	n, err := c.S.SpecUnlink(c.Str("section"), c.Context, task.Int("id"), c.Str("kind"))
	if err != nil {
		return err
	}
	return c.mutated(fmt.Sprintf("unlinked #%d from %s (%s)", task.Int("id"), c.Str("section"), plural(int64(n), "link", "links")),
		row.Of("task_id", task.Int("id"), "section", c.Str("section"), "removed", int64(n)))
}

func taskReviewSpec(c *Ctx) error {
	task, err := c.S.GetTask(c.Str("ref"), true)
	if err != nil {
		return err
	}
	out, err := c.S.SpecReview(task.Int("id"), c.Str("section"), c.Context, c.Str("verdict"), c.Str("reason"), c.Int("version"))
	if err != nil {
		return err
	}
	text := fmt.Sprintf("#%d %s v%d: %s", task.Int("id"), out.Str("section"), out.Int("revision"), out.Str("verdict"))
	if out.Int("restamped") > 0 {
		text += " (agreement re-stamped)"
	}
	return c.mutated(text, out)
}

func specCoverage(c *Ctx) error {
	sp, err := c.spec("spec")
	if err != nil {
		return err
	}
	rows, err := c.S.SpecCoverage(sp.Int("id"))
	if err != nil {
		return err
	}
	if c.structured() {
		return c.emitObject(nonNilRows(rows))
	}
	c.printf("%s v%d: %s without an implementing task\n", sp.Str("slug"), sp.Int("revision"), plural(int64(len(rows)), "section", "sections"))
	w1, w2 := 0, 0
	for _, r := range rows {
		w1, w2 = max(w1, len(r.Str("section"))), max(w2, len(r.Str("number")))
	}
	for _, r := range rows {
		c.printf("  %s  %s  %s\n", pad(r.Str("section"), w1), pad(r.Str("number"), w2), r.Str("heading"))
	}
	return nil
}

func specStale(c *Ctx) error {
	sp, err := c.spec("spec")
	if err != nil {
		return err
	}
	rows, err := c.S.SpecStale(sp.Int("id"))
	if err != nil {
		return err
	}
	var reopened []int64
	if c.Bool("reopen") {
		for _, r := range rows {
			if r.Get("stale") == true && r.Str("status") == "done" {
				if _, changed, err := c.S.SetTaskStatus(r.Int("task_id"), "todo", ""); err != nil {
					return err
				} else if changed {
					reopened = append(reopened, r.Int("task_id"))
					r.Set("status", "todo")
				}
			}
		}
	}
	if c.structured() {
		return c.emitObject(nonNilRows(rows))
	}
	for _, r := range rows {
		state := "ok"
		if r.Get("stale") == true {
			state = "changed"
			if at := r.Str("changed_at"); at != "" {
				state += " " + fmtDay(at)
			}
			if r.Str("anchor_state") != "live" {
				state = r.Str("anchor_state")
			}
		}
		agreed := fmt.Sprintf("agreed v%d (%s)", r.Int("agreed_revision"), r.Str("agreed_sha"))
		head := fmt.Sprintf("head v%d (%s)", r.Int("head_revision"), r.Str("head_sha"))
		if r.Get("stale") != true {
			agreed, head = fmt.Sprintf("agreed v%d", r.Int("agreed_revision")), fmt.Sprintf("head v%d", r.Int("head_revision"))
		}
		c.printf("#%-4d %-11s  %s  %s  %s  %s\n", r.Int("task_id"), r.Str("status"), r.Str("section"), agreed, head, state)
	}
	for _, id := range reopened {
		c.warnf("reopened #%d", id)
	}
	return nil
}

func fmtDay(iso string) string {
	if t, err := time.Parse(time.RFC3339, iso); err == nil {
		return t.Local().Format("01-02")
	}
	if len(iso) >= 10 {
		return iso[5:10]
	}
	return iso
}

func specRelink(c *Ctx) error {
	var into []string
	kind := c.Str("kind")
	switch {
	case c.Has("split_into") && c.Has("into"):
		return errs.Usage("use --into or --split-into, not both")
	case c.Has("split_into"):
		into, kind = splitList(c.Str("split_into")), "split"
	case c.Has("into"):
		into = []string{c.Str("into")}
		if kind == "" {
			return errs.Usage("--into needs --kind merged, renamed or moved")
		}
	default:
		return errs.Usage("relink needs --into SECTION --kind KIND, or --split-into A,B")
	}
	out, pinned, err := c.S.SpecRelink(c.Str("section"), c.Context, into, kind, cwdRoot())
	if err != nil {
		return err
	}
	sr, _ := store.ParseSpecRef(c.Str("section"))
	targets := strings.Join(out.Get("into").([]string), ", ")
	var text string
	if kind == "split" {
		text = fmt.Sprintf("copied %s onto %s#{%s}; %s stay on the retired @%s", plural(out.Int("links"), "task link", "task links"), sr.Slug, targets,
			"its notes", out.Str("from"))
	} else {
		text = fmt.Sprintf("moved %s and %s onto %s#%s", plural(out.Int("notes"), "note", "notes"), plural(out.Int("links"), "task link", "task links"), sr.Slug, targets)
	}
	if err := c.mutated(text, out); err != nil {
		return err
	}
	for _, k := range pinned {
		c.warnf("pinned a target section as `%s`; the spec file was modified (1 line), review it", k)
	}
	return nil
}

func specOrphans(c *Ctx) error {
	sp, err := c.spec("spec")
	if err != nil {
		return err
	}
	rows, err := c.S.SpecOrphans(sp.Int("id"))
	if err != nil {
		return err
	}
	if c.structured() {
		return c.emitObject(nonNilRows(rows))
	}
	if len(rows) == 0 {
		c.printf("%s: no orphaned anchors\n", sp.Str("slug"))
		return nil
	}
	for _, r := range rows {
		c.printf("@%s  %s %q  last seen v%d  %s (%d unresolved)  tasks: %s\n", r.Str("anchor"), r.Str("section"), r.Str("heading"),
			r.Int("last_seen"), plural(r.Int("notes"), "note", "notes"), r.Int("open_notes"), idList(r.Get("tasks")))
	}
	return nil
}

var rangeRe = regexp.MustCompile(`^v?(\d+)(?:\.\.v?(\d+))?$`)

func specDiff(c *Ctx) error {
	sp, err := c.spec("spec")
	if err != nil {
		return err
	}
	head := int(sp.Int("revision"))
	from, to := head-1, head
	if r := c.Str("range"); r != "" {
		m := rangeRe.FindStringSubmatch(r)
		if m == nil {
			return errs.Usage("invalid range %s: use v3..v5 or v3", r)
		}
		from, _ = strconv.Atoi(m[1])
		if m[2] != "" {
			to, _ = strconv.Atoi(m[2])
		}
	}
	if from < 1 {
		return errs.New("%s has only one revision", sp.Str("slug"))
	}
	rows, err := c.S.SpecDiff(sp.Int("id"), from, to)
	if err != nil {
		return err
	}
	if c.structured() {
		return c.emitObject(row.Of("spec", sp.Str("slug"), "from", int64(from), "to", int64(to), "changes", nonNilRows(rows)))
	}
	if len(rows) == 0 {
		c.printf("%s: v%d..v%d: no section changes\n", sp.Str("slug"), from, to)
		return nil
	}
	for _, r := range rows {
		switch r.Str("change") {
		case "new":
			c.printf("+ %s   %q\n", r.Str("slug"), r.Str("heading"))
		case "removed":
			c.printf("- %s   %q\n", r.Str("slug"), r.Str("heading"))
		case "orphaned":
			c.printf("? %s   @%s orphaned in v%d\n", r.Str("slug"), r.Str("anchor"), r.Int("revision"))
		case "moved":
			c.printf("> %s   %s -> %s (now %s)\n", r.Str("prior_slug"), r.Str("prior_number"), r.Str("number"), r.Str("slug"))
		default:
			line := "~ " + r.Str("prior_slug")
			if r.Str("heading") != r.Str("prior_heading") {
				line += fmt.Sprintf("   %q -> %q", r.Str("prior_heading"), r.Str("heading"))
			}
			if r.Str("slug") != r.Str("prior_slug") {
				line += "   now " + r.Str("slug")
			}
			c.println(line)
		}
		for _, l := range strings.Split(strings.TrimRight(r.Str("diff"), "\n"), "\n") {
			if l == "" || strings.HasPrefix(l, "---") || strings.HasPrefix(l, "+++") || strings.HasPrefix(l, "@@") {
				continue
			}
			c.println("  " + l)
		}
	}
	return nil
}

func specGC(c *Ctx) error {
	sp, err := c.spec("spec")
	if err != nil {
		return err
	}
	deleted, err := c.S.SpecGC(sp.Int("id"), c.Int("keep"))
	if err != nil {
		return err
	}
	if len(deleted) == 0 {
		return c.mutated(sp.Str("slug")+": nothing to drop", row.Of("spec", sp.Str("slug"), "dropped", deleted))
	}
	var vs []string
	for _, n := range deleted {
		vs = append(vs, fmt.Sprintf("v%d", n))
	}
	return c.mutated(fmt.Sprintf("%s: dropped %s: %s", sp.Str("slug"), plural(int64(len(deleted)), "revision", "revisions"), strings.Join(vs, ", ")),
		row.Of("spec", sp.Str("slug"), "dropped", deleted))
}

func specRender(c *Ctx) error {
	sp, err := c.spec("spec")
	if err != nil {
		return err
	}
	model, err := c.S.SpecModel(sp.Int("id"))
	if err != nil {
		return err
	}
	return c.emitObject(model)
}

// --- review loop ------------------------------------------------------------------------------------

func specApprove(c *Ctx) error {
	sp, err := c.spec("spec")
	if err != nil {
		return err
	}
	before, stale, err := c.S.SpecApproval(sp.Int("id"))
	if err != nil {
		return err
	}
	a, bn, err := c.S.SpecApprove(sp.Int("id"), c.Str("note"), c.Strs("build_note"))
	if err != nil {
		return err
	}
	again := before != nil && !stale && before.Int("id") == a.Int("id")
	text := fmt.Sprintf("approved %s v%v (approval %d)", sp.Str("slug"), a.Get("revision"), a.Int("id"))
	if again {
		text = "already " + text
	}
	if len(bn) > 0 {
		text += fmt.Sprintf(", %s", plural(int64(len(bn)), "build note", "build notes"))
	}
	return c.mutated(text, row.Of("approval", a, "build_notes", bn, "already", again))
}

func specRelease(c *Ctx) error {
	sp, err := c.spec("spec")
	if err != nil {
		return err
	}
	n, cursor, err := c.S.SpecRelease(sp.Int("id"))
	if err != nil {
		return err
	}
	return c.mutated(fmt.Sprintf("released %s on %s (cursor %d)", plural(int64(n), "comment", "comments"), sp.Str("slug"), cursor),
		row.Of("released", int64(n), "cursor", cursor))
}

func specBuildNote(c *Ctx) error {
	sp, err := c.spec("spec")
	if err != nil {
		return err
	}
	n, err := c.S.SpecBuildNote(sp.Int("id"), c.Str("body"))
	if err != nil {
		return err
	}
	return c.created("build note", n.Int("id"), n, "note")
}

// specWaiter follows one spec's review traffic for watch and await.
type specWaiter struct {
	c        *Ctx
	sp       *row.Row
	startRev int
	await    bool
	until    string
	comments []*row.Row
	approval *row.Row
	released bool
	w        *watchLoop
}

func (sw *specWaiter) line(id int64, action, actor, detail string) {
	c := sw.c
	if actor == "" {
		actor = "-"
	}
	if c.structured() {
		return
	}
	fmt.Fprintf(c.Out, "EVENT %d %-14s %-8s %s\n", id, action, actor, strings.ReplaceAll(detail, "\n", " "))
}

func (sw *specWaiter) jsonLine(obj *row.Row) {
	if sw.c.structured() && !sw.await {
		_ = sw.c.emitObjectLine(obj)
	}
}

func (sw *specWaiter) deliver(eventID int64, n *row.Row) {
	for _, seen := range sw.comments {
		if seen.Int("id") == n.Int("id") {
			return // released and seen already (its own event and the release)
		}
	}
	sw.comments = append(sw.comments, n)
	target := n.Str("section")
	if target == "" {
		target = "@" + n.Str("anchor")
	}
	if !sw.await {
		sw.line(eventID, "spec-note", n.Str("actor"), target+": "+n.Str("body"))
		sw.jsonLine(row.Of("event", eventID, "action", "spec-note", "actor", n.Get("actor"), "target", target, "note", n, "cursor", eventID))
	}
}

func (sw *specWaiter) emit(e *row.Row) {
	c, id := sw.c, sw.sp.Int("id")
	action := e.Str("action")
	switch e.Str("target_type") {
	case "note":
		n, err := c.S.SpecNote(id, e.Int("target_id"))
		if err != nil || n == nil {
			return
		}
		switch action {
		case "spec-note":
			if n.Get("released") == true {
				sw.deliver(e.Int("id"), n)
			}
			return // held until released (manual mode)
		case "spec-approved":
			a, _, _ := c.S.SpecApproval(id)
			if a != nil && a.Int("id") == n.Int("id") {
				sw.approval = a
			} else {
				sw.approval = row.Of("id", n.Int("id"), "by", n.Get("actor"), "at", n.Str("created_at"), "note", n.Get("body"))
			}
		}
	case "spec":
		if e.Int("target_id") != id {
			return
		}
		if action == "spec-released" {
			notes, _ := c.S.ReleasedBy(id, e.Int("id"))
			for _, n := range notes {
				sw.deliver(e.Int("id"), n)
			}
			sw.released = true
		}
	default:
		return
	}
	if !sw.await {
		sw.line(e.Int("id"), action, e.Str("actor"), e.Str("detail"))
		sw.jsonLine(row.Of("event", e.Int("id"), "action", action, "actor", e.Get("actor"), "target", sw.sp.Str("slug"),
			"detail", e.Get("detail"), "at", e.Str("at"), "cursor", e.Int("id")))
	}
}

func (sw *specWaiter) tick() (string, int, *row.Row) {
	c, id := sw.c, sw.sp.Int("id")
	if sw.approval != nil {
		return "approved", ExitApproved, nil
	}
	cur, err := c.S.SpecRow(id)
	if err != nil {
		return "lost", ExitLost, row.Of("reason", err.Error())
	}
	if rev := int(cur.Int("revision")); rev != sw.startRev {
		if same, err := c.S.SpecSameContent(id, sw.startRev, rev); err == nil && !same {
			return "changed", ExitSpecChanged, row.Of("reason", fmt.Sprintf("v%d -> v%d", sw.startRev, rev))
		}
	}
	if sw.await {
		switch sw.until {
		case "comments":
			if len(sw.comments) > 0 {
				return "comments", ExitComments, nil
			}
		case "release":
			if sw.released || len(sw.comments) > 0 {
				return "comments", ExitComments, nil
			}
		}
	}
	return "", 0, nil
}

func (sw *specWaiter) result(state string, code int, extra *row.Row) error {
	c := sw.c
	cur, _ := c.S.SpecRow(sw.sp.Int("id"))
	var approval any
	if sw.approval != nil {
		approval = row.Of("by", sw.approval.Get("by"), "at", sw.approval.Get("at"), "note", sw.approval.Get("note"), "revision", sw.approval.Get("revision"))
	}
	var comments []*row.Row
	for _, n := range sw.comments {
		st := "open"
		if !n.IsNull("resolved_at") {
			st = "resolved"
		}
		comments = append(comments, row.Of("id", n.Int("id"), "section", n.Get("section"), "quote", n.Get("quote"), "body", n.Str("body"),
			"kind", n.Str("kind"), "actor", n.Get("actor"), "state", st))
	}
	bn, _ := c.S.SpecBuildNotes(sw.sp.Int("id"))
	var bns []*row.Row
	for _, b := range bn {
		bns = append(bns, row.Of("id", b.Int("id"), "body", b.Str("body")))
	}
	obj := row.Of("result", state, "spec", sw.sp.Str("slug"), "revision", cur.Int("revision"), "cursor", sw.w.after,
		"approval", approval, "comments", nonNilRows(comments), "build_notes", nonNilRows(bns))
	if extra != nil && extra.Str("reason") != "" {
		obj.Set("reason", extra.Str("reason"))
	}
	if c.structured() {
		if err := c.emitObjectLine(obj); err != nil {
			return err
		}
	} else {
		c.printf("result: %s  spec: %s  revision: %d  cursor: %d\n", state, sw.sp.Str("slug"), cur.Int("revision"), sw.w.after)
		if r := obj.Str("reason"); r != "" {
			c.printf("reason: %s\n", r)
		}
		if sw.approval != nil {
			c.printf("approval: v%v by %v at %v", sw.approval.Get("revision"), strOr(sw.approval.Get("by")), sw.approval.Get("at"))
			if n := sw.approval.Str("note"); n != "" {
				c.printf(": %s", n)
			}
			c.println()
		}
		for _, n := range comments {
			q := ""
			if n.Str("quote") != "" {
				q = fmt.Sprintf(" %q", n.Str("quote"))
			}
			c.printf("comment %d %s [%s]%s: %s\n", n.Int("id"), n.Str("section"), n.Str("state"), q, strings.ReplaceAll(n.Str("body"), "\n", " "))
		}
		for _, b := range bns {
			c.printf("build-note %d: %s\n", b.Int("id"), b.Str("body"))
		}
		c.printf("TERMINAL %s\n", state)
	}
	if code == 0 {
		return nil
	}
	return exitCode(code)
}

func (c *Ctx) emitObjectLine(obj any) error {
	if c.Format == "yaml" {
		return c.emitObject(obj)
	}
	c.Out.Write(render.JSONCompact(obj))
	fmt.Fprintln(c.Out)
	return nil
}

func specWait(c *Ctx, await bool) error {
	sp, err := c.spec("spec")
	if err != nil {
		return err
	}
	timeout, err := parseDuration(c.Str("timeout"))
	if err != nil {
		return err
	}
	if timeout <= 0 {
		return errs.New("a wait needs a timeout: a hang needs a human to notice, a timeout is recoverable")
	}
	interval, err := parseDuration(c.Str("interval"))
	if err != nil || interval <= 0 {
		interval = time.Second
	}
	if c.Str("actor_not") == "self" && c.Actor == "" {
		c.warnf("warning: --actor-not self has no effect without AGTRACK_ACTOR or --actor")
	}
	after := int64(c.Int("after"))
	if _, given := c.V["after"].(int); !given {
		if after, err = c.S.MaxEventID(); err != nil {
			return err
		}
	}
	project := row.Of("id", sp.Int("project_id"))
	sw := &specWaiter{c: c, sp: sp, startRev: int(sp.Int("revision")), await: await, until: c.Str("until")}
	sw.w = &watchLoop{c: c, after: after, timeout: timeout, interval: interval,
		filter: store.EventFilter{Project: project, Direct: true, ActorNot: c.actorNot(),
			Actions: []string{"spec-note", "spec-released", "spec-approved", "spec-build-note", "spec-synced"}},
		emit: sw.emit, onTick: sw.tick}
	if await {
		sw.w.finish = sw.result
	}
	return sw.w.run()
}

func specWatch(c *Ctx) error { return specWait(c, false) }
func specAwait(c *Ctx) error { return specWait(c, true) }

func specServe(c *Ctx) error {
	sp, err := c.spec("spec")
	if err != nil {
		return err
	}
	idle, err := parseDuration(c.Str("idle"))
	if err != nil {
		return err
	}
	port := c.Int("port")
	return web.Serve(context.Background(), web.Options{Store: c.S, SpecID: sp.Int("id"), SpecRef: c.Str("spec"),
		Addr: fmt.Sprintf("127.0.0.1:%d", port), Idle: idle, Out: c.Out, Open: c.Bool("open"), RepoRoot: cwdRoot()})
}
