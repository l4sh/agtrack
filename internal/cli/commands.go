package cli

import (
	"github.com/l4sh/agtrack/internal/cli/argp"
	"github.com/l4sh/agtrack/internal/render"
)

// Handler runs one verb.
type Handler func(c *Ctx) error

// withNoun binds the noun for the shared note/tag verbs.
func withNoun(noun string, h Handler) Handler {
	return func(c *Ctx) error { c.Noun = noun; return h(c) }
}

func status(st string, h func(*Ctx, string) error) Handler {
	return func(c *Ctx) error { return h(c, st) }
}

// Globals are accepted at every level (go spec §3.3).
var Globals = []argp.Flag{
	{Long: "--db", Metavar: "PATH", Help: "database file (default $AGTRACK_DB, else $XDG_DATA_HOME/agtrack/agtrack.db, else ~/.local/share/agtrack/agtrack.db)"},
	{Long: "--context", Short: "-c", Metavar: "NAME", Help: "context to work in (default: the current one)"},
	{Long: "--format", Short: "-f", Choices: render.Formats, Help: "output format (default table)"},
	{Long: "--fields", Metavar: "A,B", Help: "exact columns (or keys of a show) to print, comma separated"},
	{Long: "--extended", Short: "-x", Kind: argp.Bool, Help: "wider view: extra columns and nested detail"},
	{Long: "--no-header", Kind: argp.Bool, Help: "omit the header row"},
	{Long: "--color", Kind: argp.Bool},
	{Long: "--no-color", Kind: argp.Bool},
	{Long: "--quiet", Short: "-q", Kind: argp.Bool, Help: "identifiers only: a bare id from a create, ids from a listing, nothing from a mutation"},
	{Long: "--actor", Metavar: "NAME", Help: "attribute writes to this actor (default $AGTRACK_ACTOR)"},
	{Long: "--session", Metavar: "ID", Help: "session id stamped on writes (default $AGTRACK_SESSION, else the harness's session id)"},
	{Long: "--max-bytes", Kind: argp.PositiveInt, Metavar: "N", Help: "bound the output; omitted records are listed in a manifest, never dropped silently"},
	{Long: "--idem", Metavar: "KEY", Help: "idempotency key for a create: a retry with the same key reprints the first result"},
}

func listFlags() []argp.Flag {
	return []argp.Flag{
		{Long: "--count", Kind: argp.Bool, Help: "print the number of matches (before --limit)"},
		{Long: "--ids", Kind: argp.Bool, Help: "print identifiers only"},
	}
}

func noteVerbs(noun, refHelp string, taggable bool) []*argp.Command {
	cmds := []*argp.Command{
		{Name: "note", Help: "attach a note to a " + noun, Run: withNoun(noun, noteAdd),
			Args: []argp.Arg{{Name: "ref", Help: refHelp}, {Name: "body", NArgs: "+"}},
			Flags: []argp.Flag{
				{Long: "--at", Metavar: "WHEN", Help: "backfill: when it was written"},
				{Long: "--kind", Choices: []string{"note", "decision", "handoff", "blocker", "question"}, Help: "note (default), decision, handoff, blocker or question"},
			}},
		{Name: "notes", Help: "notes on a " + noun, Run: withNoun(noun, noteLs),
			Args: []argp.Arg{{Name: "ref", Help: refHelp}},
			Flags: append([]argp.Flag{
				{Long: "--open", Kind: argp.Bool, Help: "unresolved only"},
				{Long: "--unacked", Kind: argp.Bool, Help: "handoffs, blockers and questions nobody has acknowledged"},
				{Long: "--kind", Help: "only this kind"},
			}, listFlags()...)},
	}
	if noun == "task" {
		cmds[0].Flags = append(cmds[0].Flags, argp.Flag{Long: "--close", Metavar: "REF",
			Help: "with --kind handoff: also finish this step (12.3) or the task itself, in one transaction"})
	}
	if taggable {
		cmds = append(cmds,
			&argp.Command{Name: "tag", Help: "add tags to a " + noun, Run: withNoun(noun, tagAttach),
				Args: []argp.Arg{{Name: "ref", Help: refHelp}, {Name: "tags", NArgs: "+"}}},
			&argp.Command{Name: "untag", Help: "remove tags from a " + noun, Run: withNoun(noun, tagDetach),
				Args: []argp.Arg{{Name: "ref", Help: refHelp}, {Name: "tags", NArgs: "+"}}},
		)
	}
	return cmds
}

func setFlags() []argp.Flag {
	return []argp.Flag{
		{Long: "--title"},
		{Long: "--rename", Metavar: "NEW"},
		{Long: "--archive", Kind: argp.Bool},
		{Long: "--unarchive", Kind: argp.Bool},
	}
}

func statusVerbs(group string, set func(*Ctx, string) error, cascade bool) []*argp.Command {
	var out []*argp.Command
	for _, v := range []struct{ verb, status, help string }{
		{"start", "in-progress", "mark in-progress"},
		{"done", "done", "mark done"},
		{"cancel", "cancelled", "mark cancelled"},
		{"reopen", "todo", "back to todo"},
	} {
		flags := []argp.Flag{
			{Long: "--note", Help: "record a note with the transition"},
			{Long: "--tag", Kind: argp.Append, Metavar: "TAG", Help: "tag the item as part of the transition; repeatable"},
			{Long: "--at", Metavar: "WHEN", Help: "backfill: when the transition actually happened"},
		}
		if cascade {
			flags = append(flags, argp.Flag{Long: "--no-cascade", Kind: argp.Bool, Help: "do not pull the parent task into in-progress"})
		}
		if v.verb == "start" || v.verb == "done" {
			flags = append(flags, argp.Flag{Long: "--no-git", Kind: argp.Bool, Help: "do not record branch/worktree/commit artifacts from the cwd checkout"})
		}
		if v.verb == "done" {
			flags = append(flags, argp.Flag{Long: "--evidence", Kind: argp.Append, Metavar: "KIND:VALUE",
				Help: "proof of completion: commit|branch|pr|test|file|url|note:VALUE; repeatable"})
			if cascade {
				flags = append(flags, argp.Flag{Long: "--close-parent", Kind: argp.Bool, Help: "also close the task if this was its last open step"})
			}
		}
		refHelp := "task id"
		if group == "sub" {
			refHelp = "subtask id or task.position"
		}
		out = append(out, &argp.Command{Name: v.verb, Help: v.help, Run: status(v.status, set),
			Args: []argp.Arg{{Name: "ref", NArgs: "+", Help: refHelp}}, Flags: flags})
	}
	flags := []argp.Flag{
		{Long: "--note"},
		{Long: "--tag", Kind: argp.Append, Metavar: "TAG"},
		{Long: "--at", Metavar: "WHEN"},
	}
	if cascade {
		flags = append(flags, argp.Flag{Long: "--no-cascade", Kind: argp.Bool})
	}
	out = append(out, &argp.Command{Name: "status", Help: "set an explicit status (status first, then refs)", Run: status("", set),
		Args: []argp.Arg{{Name: "status"}, {Name: "ref", NArgs: "+"}}, Flags: flags})
	return out
}

func logFilters() []argp.Flag {
	return append([]argp.Flag{
		{Long: "--since", Metavar: "WHEN", Help: "2d, 36h, today, 2026-09-01"},
		{Long: "--until", Metavar: "WHEN"},
		{Long: "--task", Short: "-t", Metavar: "ID"},
		{Long: "--project", Short: "-p", Metavar: "REF"},
		{Long: "--direct", Kind: argp.Bool, Help: "exclude subproject activity"},
		{Long: "--by", Dest: "actor_filter", Metavar: "ACTOR"},
		{Long: "--action", Metavar: "LIST", Help: "created,status,note,log,pr,tagged,..."},
		{Long: "--limit", Short: "-n", Kind: argp.PositiveInt, Default: 50},
		{Long: "--all-contexts", Kind: argp.Bool},
		{Long: "--by-day", Kind: argp.Bool, Help: "group under date headers"},
		{Long: "--backdated", Kind: argp.Bool, Help: "only entries recorded after the fact"},
	}, listFlags()...)
}

// Tree builds the command tree. Kept as data so `grammar` and `describe`
// read the same definitions the parser uses.
func Tree() *argp.Command {
	ctx := &argp.Command{Name: "ctx", Aliases: []string{"context", "contexts"}, Help: "contexts: personal, company-a, ...", SubRequired: true,
		Subs: append([]*argp.Command{
			{Name: "add", Help: "create a context", Run: Handler(contextAdd), Args: []argp.Arg{{Name: "name"}},
				Flags: []argp.Flag{{Long: "--title"}, {Long: "--note"}, {Long: "--use", Kind: argp.Bool, Help: "make it current"}}},
			{Name: "ls", Aliases: []string{"list"}, Help: "list contexts", Run: Handler(contextLs),
				Flags: append([]argp.Flag{{Long: "--archived", Kind: argp.Bool, Help: "include archived"}}, listFlags()...)},
			{Name: "use", Help: "set the current context (drops a project pin from another context)", Run: Handler(contextUse), Args: []argp.Arg{{Name: "name"}}},
			{Name: "current", Help: "print the current context", Run: Handler(contextCurrent)},
			{Name: "show", Help: "context detail", Run: Handler(contextShow), Args: []argp.Arg{{Name: "name"}}},
			{Name: "set", Help: "rename, retitle or archive", Run: Handler(contextSet), Args: []argp.Arg{{Name: "name"}}, Flags: setFlags()},
			{Name: "rm", Aliases: []string{"delete"}, Help: "delete a context", Run: Handler(contextRm), Args: []argp.Arg{{Name: "name"}},
				Flags: []argp.Flag{{Long: "--force", Kind: argp.Bool, Help: "delete its projects and tasks too"}}},
		}, noteVerbs("context", "context name", false)...)}

	project := &argp.Command{Name: "project", Aliases: []string{"proj", "projects", "p"}, Help: "projects and subprojects (orbiter:frontend)", SubRequired: true,
		Subs: append([]*argp.Command{
			{Name: "add", Help: "create a project or subproject", Run: Handler(projectAdd), Args: []argp.Arg{{Name: "ref", Help: "project or project:subproject"}},
				Flags: []argp.Flag{{Long: "--title"}, {Long: "--repo", Kind: argp.Append, Metavar: "URL", Help: "link a git remote; repeatable"},
					{Long: "--tag", Kind: argp.Append, Metavar: "TAG"}, {Long: "--note"}, {Long: "--use", Kind: argp.Bool, Help: "make it the current project"}}},
			{Name: "ls", Aliases: []string{"list"}, Help: "list projects", Run: Handler(projectLs),
				Flags: append([]argp.Flag{{Long: "--parent", Metavar: "REF", Help: "only subprojects of REF"}, {Long: "--top", Kind: argp.Bool, Help: "top level projects only"},
					{Long: "--archived", Kind: argp.Bool}, {Long: "--all-contexts", Kind: argp.Bool}}, listFlags()...)},
			{Name: "show", Help: "project detail", Run: Handler(projectShow), Args: []argp.Arg{{Name: "ref"}}},
			{Name: "use", Help: "pin a project so later commands can omit --project", Run: Handler(projectUse), Args: []argp.Arg{{Name: "ref"}}},
			{Name: "set", Help: "rename, retitle or archive", Run: Handler(projectSet), Args: []argp.Arg{{Name: "ref"}}, Flags: setFlags()},
			{Name: "rm", Aliases: []string{"delete"}, Help: "delete a project", Run: Handler(projectRm), Args: []argp.Arg{{Name: "ref"}},
				Flags: []argp.Flag{{Long: "--force", Kind: argp.Bool, Help: "delete its subprojects and tasks too"}}},
			{Name: "repo", Help: "link git remotes to a project", Run: Handler(projectRepo),
				Args: []argp.Arg{{Name: "repo_action", Choices: []string{"add", "rm", "ls"}}, {Name: "ref"}, {Name: "url", NArgs: "*"}}},
			{Name: "find", Help: "which project owns a git remote (default: the cwd repo)", Run: Handler(projectFind),
				Flags: append([]argp.Flag{{Long: "--repo", Metavar: "URL"}, {Long: "--use", Kind: argp.Bool, Help: "pin the match"}}, listFlags()...)},
		}, noteVerbs("project", "project or project:subproject", true)...)}

	taskSubs := []*argp.Command{
		{Name: "add", Help: "create a task", Run: Handler(taskAdd), Args: []argp.Arg{{Name: "title", NArgs: "+"}},
			Flags: []argp.Flag{
				{Long: "--project", Short: "-p", Metavar: "REF", Help: "project, project:subproject or . for the cwd repo"},
				{Long: "--status", Help: "initial status (default todo)"},
				{Long: "--tag", Kind: argp.Append, Metavar: "TAG"},
				{Long: "--note"},
				{Long: "--pr", Kind: argp.Append, Metavar: "URL"},
				{Long: "--step", Kind: argp.Append, Metavar: "TITLE", Help: "create a subtask at the same time; repeatable"},
				{Long: "--spec", Kind: argp.Append, Metavar: "SECTION", Help: "link a spec section this task implements (auth#tokens__rotation); repeatable"},
				{Long: "--created", Metavar: "WHEN", Help: "backfill: when the work started being tracked (2026-08-14, 3d, yesterday)"},
				{Long: "--started", Metavar: "WHEN", Help: "backfill: when work actually began"},
				{Long: "--closed", Metavar: "WHEN", Help: "backfill: when it was finished or dropped; needs --status done or cancelled"},
			}},
		{Name: "ls", Aliases: []string{"list"}, Help: "list tasks", Run: Handler(taskLs),
			Flags: append([]argp.Flag{
				{Long: "--project", Short: "-p", Metavar: "REF"},
				{Long: "--direct", Kind: argp.Bool, Help: "exclude subproject tasks"},
				{Long: "--status", Short: "-s", Metavar: "LIST", Help: "status filter (default open)"},
				{Long: "--tag", Kind: argp.Append, Metavar: "TAG"},
				{Long: "--tags-all", Kind: argp.Bool, Help: "require every --tag instead of any"},
				{Long: "--grep", Metavar: "TEXT", Help: "substring match on the title"},
				{Long: "--since", Metavar: "WHEN", Help: "updated since 2d, today, ..."},
				{Long: "--repo", Metavar: "URL", Help: "tasks of the project owning URL"},
				{Long: "--limit", Short: "-n", Kind: argp.PositiveInt},
				{Long: "--order", Choices: []string{"created", "id", "status", "updated"}, Default: "id"},
				{Long: "--all-contexts", Kind: argp.Bool},
				{Long: "--unclaimed", Kind: argp.Bool, Help: "only tasks nobody holds a live claim on"},
				{Long: "--ready", Kind: argp.Bool, Help: "only tasks with no unmet blocker"},
				{Long: "--blocked", Kind: argp.Bool, Help: "only tasks waiting on an unmet blocker"},
				{Long: "--missing-artifact", Choices: []string{"branch", "pr"}, Help: "only tasks with no branch artifact / no linked PR"},
			}, listFlags()...)},
		{Name: "show", Help: "one task with its steps, notes and PRs (-x adds history)", Run: Handler(taskShow), Args: []argp.Arg{{Name: "ref"}}},
	}
	taskSubs = append(taskSubs, statusVerbs("task", taskStatus, false)...)
	taskSubs = append(taskSubs,
		&argp.Command{Name: "set", Help: "retitle or move a task", Run: Handler(taskSet), Args: []argp.Arg{{Name: "ref"}},
			Flags: []argp.Flag{{Long: "--title", Kind: argp.NArgsPlus}, {Long: "--move", Metavar: "REF", Help: "move to another project"}}},
		&argp.Command{Name: "rm", Aliases: []string{"delete"}, Help: "delete tasks", Run: Handler(taskRm), Args: []argp.Arg{{Name: "ref", NArgs: "+"}},
			Flags: []argp.Flag{{Long: "--force", Kind: argp.Bool, Help: "delete its steps, notes and PR links too"}}},
		&argp.Command{Name: "snooze", Help: "park a task until a date or duration (hidden from next)", Run: Handler(taskSnooze), Args: []argp.Arg{{Name: "ref"}},
			Flags: []argp.Flag{{Long: "--until", Metavar: "DATE|DURATION", Help: "2026-10-20, 3d, 12h, 2w"}, {Long: "--note"}}},
		&argp.Command{Name: "unsnooze", Help: "clear a task's park", Run: Handler(taskUnsnooze), Args: []argp.Arg{{Name: "ref"}}},
		&argp.Command{Name: "done-check", Help: "in-progress tasks that look finished or stale, with the command for each", Run: Handler(taskDoneCheck),
			Flags: append([]argp.Flag{{Long: "--project", Short: "-p", Metavar: "REF"}, {Long: "--all-contexts", Kind: argp.Bool},
				{Long: "--stale-after", Metavar: "Nd", Help: "idle threshold in days (default config next.stale_days, 7)"},
				{Long: "--apply", Kind: argp.Bool, Help: "close the all-steps-done candidates (needs a terminal confirmation, or --yes)"},
				{Long: "--yes", Kind: argp.Bool, Help: "confirm --apply without a terminal"}}, listFlags()...)},
		&argp.Command{Name: "claim", Help: "take an advisory, expiring claim on a task", Run: Handler(taskClaim), Args: []argp.Arg{{Name: "ref"}},
			Flags: []argp.Flag{{Long: "--ttl", Metavar: "DURATION", Default: "2h", Help: "claim lifetime: 45m, 2h, 1d; 0 = no expiry (default 2h)"},
				{Long: "--steal", Kind: argp.Bool, Help: "take over another actor's live claim"},
				{Long: "--no-git", Kind: argp.Bool, Help: "do not record branch/worktree artifacts from the cwd checkout"}}},
		&argp.Command{Name: "transfer", Help: "hand a task to another actor: release, claim for them and a handoff note, atomically", Run: Handler(taskTransfer), Args: []argp.Arg{{Name: "ref"}},
			Flags: []argp.Flag{{Long: "--to", Metavar: "ACTOR", Help: "receiving actor (required)"}, {Long: "--note", Help: "the handoff note (default: who handed over to whom)"},
				{Long: "--ttl", Metavar: "DURATION", Default: "2h", Help: "the new claim's lifetime"},
				{Long: "--force", Kind: argp.Bool, Help: "transfer someone else's live claim"}}},
		&argp.Command{Name: "release", Help: "drop your claim on a task", Run: Handler(taskRelease), Args: []argp.Arg{{Name: "ref"}},
			Flags: []argp.Flag{{Long: "--force", Kind: argp.Bool, Help: "release someone else's claim"}}},
		&argp.Command{Name: "block", Help: "record that a task waits on another", Run: Handler(taskBlock), Args: []argp.Arg{{Name: "ref"}},
			Flags: []argp.Flag{{Long: "--on", Kind: argp.Append, Metavar: "TASK", Help: "prerequisite task; repeatable"},
				{Long: "--kind", Choices: []string{"blocks", "relates", "duplicates"}, Default: "blocks"}, {Long: "--reason"}}},
		&argp.Command{Name: "unblock", Help: "remove a dependency", Run: Handler(taskUnblock), Args: []argp.Arg{{Name: "ref"}},
			Flags: []argp.Flag{{Long: "--on", Kind: argp.Append, Metavar: "TASK"}, {Long: "--kind", Choices: []string{"blocks", "relates", "duplicates"}}}},
		&argp.Command{Name: "deps", Help: "what a task waits on and what waits on it", Run: Handler(taskDeps), Args: []argp.Arg{{Name: "ref"}}, Flags: listFlags()},
		&argp.Command{Name: "artifact", Help: "record a branch, commit, file, url, run or worktree on a task", Run: Handler(taskArtifact),
			Args:  []argp.Arg{{Name: "ref"}, {Name: "kind", NArgs: "?", Choices: []string{"branch", "commit", "file", "url", "run", "worktree", "ls", "rm"}}, {Name: "value", NArgs: "?"}},
			Flags: []argp.Flag{{Long: "--label"}}},
		&argp.Command{Name: "review-spec", Help: "record a review of a stale spec link", Run: Handler(taskReviewSpec),
			Args:  []argp.Arg{{Name: "ref"}, {Name: "section"}},
			Flags: []argp.Flag{{Long: "--verdict", Choices: []string{"still-valid", "needs-work"}, Default: "still-valid"}, {Long: "--reason"}, {Long: "--version", Kind: argp.Int}}},
	)
	taskSubs = append(taskSubs, continuityTaskVerbs()...)
	taskSubs = append(taskSubs, noteVerbs("task", "task id", true)...)
	task := &argp.Command{Name: "task", Aliases: []string{"tasks", "t"}, Help: "tasks: the unit of work, one per project", SubRequired: true, Subs: taskSubs}

	subSubs := []*argp.Command{
		{Name: "add", Help: "add one or more steps to a task", Run: Handler(subtaskAdd),
			Args:  []argp.Arg{{Name: "task", Help: "task id"}, {Name: "title", NArgs: "+", Help: "one step per argument"}},
			Flags: []argp.Flag{{Long: "--tag", Kind: argp.Append, Metavar: "TAG"}, {Long: "--note"}, {Long: "--at", Metavar: "WHEN", Help: "backfill: when the step was added"}}},
		{Name: "ls", Aliases: []string{"list"}, Help: "steps of a task", Run: Handler(subtaskLs), Args: []argp.Arg{{Name: "task"}},
			Flags: append([]argp.Flag{{Long: "--status", Short: "-s", Metavar: "LIST"}, {Long: "--limit", Short: "-n", Kind: argp.PositiveInt}}, listFlags()...)},
		{Name: "show", Help: "subtask detail", Run: Handler(subtaskShow), Args: []argp.Arg{{Name: "ref"}}},
	}
	subSubs = append(subSubs, statusVerbs("sub", subtaskStatus, true)...)
	subSubs = append(subSubs, &argp.Command{Name: "next", Help: "close the in-progress step and start the next one", Run: Handler(subtaskNext),
		Args: []argp.Arg{{Name: "task", Help: "task id"}},
		Flags: []argp.Flag{{Long: "--close-parent", Kind: argp.Bool, Help: "close the task when no step is left"}, {Long: "--note", Help: "note on the closed step (and the task when it closes)"},
			{Long: "--evidence", Kind: argp.Append, Metavar: "KIND:VALUE"},
			{Long: "--no-git", Kind: argp.Bool, Help: "do not record the commit artifact when the task closes"}}})
	subSubs = append(subSubs,
		&argp.Command{Name: "set", Help: "retitle a step", Run: Handler(subtaskSet), Args: []argp.Arg{{Name: "ref"}},
			Flags: []argp.Flag{{Long: "--title", Kind: argp.NArgsPlus}}},
		&argp.Command{Name: "rm", Aliases: []string{"delete"}, Help: "delete steps", Run: Handler(subtaskRm), Args: []argp.Arg{{Name: "ref", NArgs: "+"}},
			Flags: []argp.Flag{{Long: "--force", Kind: argp.Bool, Help: "delete its notes too"}}},
	)
	subSubs = append(subSubs, noteVerbs("subtask", "subtask id or task.position", true)...)
	sub := &argp.Command{Name: "sub", Aliases: []string{"subtask", "subtasks", "s"}, Help: "subtasks: the step list under a task", SubRequired: true, Subs: subSubs}

	tag := &argp.Command{Name: "tag", Aliases: []string{"tags"}, Help: "tags across projects, tasks and subtasks", SubRequired: true,
		Subs: append([]*argp.Command{
			{Name: "ls", Aliases: []string{"list"}, Help: "list tags", Run: Handler(tagLs), Flags: listFlags()},
			{Name: "show", Help: "everything carrying a tag", Run: Handler(tagShow), Args: []argp.Arg{{Name: "ref"}}},
			{Name: "rm", Aliases: []string{"delete"}, Help: "delete a tag and its assignments", Run: Handler(tagRm), Args: []argp.Arg{{Name: "ref"}}},
		}, noteVerbs("tag", "tag name", false)...)}

	pr := &argp.Command{Name: "pr", Aliases: []string{"prs"}, Help: "pull requests linked to tasks", SubRequired: true,
		Subs: []*argp.Command{
			{Name: "add", Help: "link a PR to a task", Run: Handler(prAdd), Args: []argp.Arg{{Name: "task"}, {Name: "url"}},
				Flags: []argp.Flag{{Long: "--title"}, {Long: "--state", Help: "open, draft, merged or closed"}}},
			{Name: "ls", Aliases: []string{"list"}, Help: "list PRs, or find the task behind one", Run: Handler(prLs),
				Flags: append([]argp.Flag{{Long: "--task", Short: "-t", Metavar: "ID"}, {Long: "--repo", Metavar: "URL"}, {Long: "--number", Kind: argp.Int},
					{Long: "--url"}, {Long: "--state"}, {Long: "--all-contexts", Kind: argp.Bool}}, listFlags()...)},
			{Name: "sync", Help: "read each linked PR's state with `gh pr view` (read only) and list open tasks whose PR merged", Run: Handler(prSync),
				Flags: []argp.Flag{{Long: "--project", Short: "-p", Metavar: "REF"}, {Long: "--all-contexts", Kind: argp.Bool}}},
			{Name: "state", Help: "update a PR state", Run: Handler(prState), Args: []argp.Arg{{Name: "id", Help: "pr record id"}, {Name: "new_state"}}},
			{Name: "rm", Aliases: []string{"delete"}, Help: "unlink a PR", Run: Handler(prRm), Args: []argp.Arg{{Name: "id"}}},
		}}

	logCmd := &argp.Command{Name: "log", Aliases: []string{"worklog"}, Help: "the worklog: what happened and when", Run: Handler(logLs), Flags: logFilters(),
		Subs: []*argp.Command{
			{Name: "add", Help: "append a free form entry", Run: Handler(logAdd), Args: []argp.Arg{{Name: "body", NArgs: "+"}},
				Flags: []argp.Flag{{Long: "--task", Short: "-t", Metavar: "ID"}, {Long: "--project", Short: "-p", Metavar: "REF"},
					{Long: "--at", Metavar: "WHEN", Help: "backfill: when it happened (2026-08-14T09:30, 3d)"}}},
			{Name: "ls", Help: "read the worklog", Run: Handler(logLs), Flags: logFilters()},
			{Name: "list", Help: "read the worklog", Run: Handler(logLs), Flags: logFilters(), Hidden: true},
		}}

	note := &argp.Command{Name: "note", Help: "notes by id: resolve, reopen, remove, reply", SubRequired: true,
		Subs: []*argp.Command{
			{Name: "resolve", Help: "resolve a note (with an optional rationale)", Run: Handler(noteResolve), Args: []argp.Arg{{Name: "id"}},
				Flags: []argp.Flag{{Long: "--note", Help: "why it is resolved"}}},
			{Name: "reopen", Help: "reopen a resolved note", Run: Handler(noteReopen), Args: []argp.Arg{{Name: "id"}}},
			{Name: "rm", Aliases: []string{"delete"}, Help: "delete a note", Run: Handler(noteRm), Args: []argp.Arg{{Name: "id"}}},
			{Name: "reply", Help: "reply to a note thread", Run: Handler(noteReply), Args: []argp.Arg{{Name: "id"}, {Name: "body", NArgs: "+"}}},
			{Name: "show", Help: "a note with its thread", Run: Handler(noteShow), Args: []argp.Arg{{Name: "id"}}},
			{Name: "ack", Help: "acknowledge handoffs, blockers or questions (not the same as resolving them)", Run: Handler(noteAck),
				Args:  []argp.Arg{{Name: "id", NArgs: "+"}},
				Flags: []argp.Flag{{Long: "--note", Help: "reply recorded with the acknowledgement"}}},
		}}

	notes := &argp.Command{Name: "notes", Help: "unresolved notes across a project (handoffs, blockers, questions, spec comments)", Run: Handler(notesOpen),
		Flags: append([]argp.Flag{{Long: "--project", Short: "-p", Metavar: "REF"}, {Long: "--task", Short: "-t", Metavar: "ID"},
			{Long: "--kind", Metavar: "LIST", Help: "comma separated kinds"}, {Long: "--open", Kind: argp.Bool, Help: "unresolved only (the default; kept for symmetry)"},
			{Long: "--unacked", Kind: argp.Bool, Help: "handoffs, blockers and questions nobody has acknowledged"}}, listFlags()...)}

	claims := &argp.Command{Name: "claims", Help: "live claims: holder, session, age, last seen, last event", Run: Handler(claimsLs),
		Flags: append([]argp.Flag{{Long: "--project", Short: "-p", Metavar: "REF"}, {Long: "--task", Short: "-t", Metavar: "ID"},
			{Long: "--stale", Metavar: "DURATION", Help: "only holders quiet for longer than this (15m, 2h)"}}, listFlags()...)}

	config := &argp.Command{Name: "config", Help: "preferences stored in the database", SubRequired: true,
		Subs: []*argp.Command{
			{Name: "get", Help: "print a setting", Run: Handler(configGet), Args: []argp.Arg{{Name: "key", NArgs: "?"}}},
			{Name: "set", Help: "change a setting (known: review.release, actor.strict, actor.required, rules.approvers, next.stale_days, lessons.stale_days, git.auto)", Run: Handler(configSet), Args: []argp.Arg{{Name: "key"}, {Name: "value"}}},
		}}

	root := &argp.Command{Name: "agtrack", Help: "Context / project / task / subtask tracker for agents and humans.", Epilog: Epilog, SubRequired: true,
		Subs: []*argp.Command{ctx, project, task, sub, tag, pr, logCmd, note, notes, claims, specTree(), skillTree(), hooksCommand(),
			{Name: "doctor", Help: "read-only health check: stale tasks and claims, unacked handoffs, actor drift, missing PRs, stale lessons, proposed rules; each finding names a fix", Run: Handler(cmdDoctor),
				Flags: append([]argp.Flag{{Long: "--project", Short: "-p", Metavar: "REF"}, {Long: "--all-contexts", Kind: argp.Bool},
					{Long: "--strict", Kind: argp.Bool, Help: "exit 4 when there are findings"},
					{Long: "--stale-after", Metavar: "Nd", Help: "idle days before a task is stale (default: next.stale_days)"},
					{Long: "--claim-quiet", Metavar: "DURATION", Default: "1h", Help: "a live claim quiet for longer than this is stale (default 1h)"}}, listFlags()...)},
			{Name: "next", Help: "what is in progress, with its open steps", Run: Handler(cmdNext),
				Flags: []argp.Flag{{Long: "--project", Short: "-p", Metavar: "REF"}, {Long: "--todo", Kind: argp.Bool, Help: "include todo tasks"},
					{Long: "--limit", Short: "-n", Kind: argp.PositiveInt}, {Long: "--all-contexts", Kind: argp.Bool},
					{Long: "--claim", Kind: argp.Bool, Help: "claim the first unclaimed, unblocked task (implies --todo)"},
					{Long: "--ttl", Metavar: "DURATION", Default: "2h", Help: "claim lifetime with --claim"},
					{Long: "--no-git", Kind: argp.Bool, Help: "with --claim: do not record git artifacts"},
					{Long: "--blocked", Kind: argp.Bool, Help: "include tasks waiting on an unmet blocker"},
					{Long: "--stale", Kind: argp.Bool, Help: "only stale tasks (all steps done, idle, claim expired)"},
					{Long: "--stale-after", Metavar: "Nd", Help: "idle threshold in days; implies --stale (default config next.stale_days, 7)"},
					{Long: "--parked", Kind: argp.Bool, Help: "list parked (snoozed) tasks instead"}}},
			{Name: "resume", Help: "the working-state packet a cold agent reads first", Run: Handler(cmdResume),
				Flags: resumeFlags(), Output: resumeOutput},
			{Name: "batch", Help: "run many operations from JSON lines in one transaction", Run: Handler(cmdBatch),
				Flags: []argp.Flag{{Long: "--file", Metavar: "PATH", Help: "read ops from this file instead of stdin"},
					{Long: "--continue", Kind: argp.Bool, Help: "keep going after a failing op (it alone is undone); default is to roll everything back"}},
				Epilog: batchEpilog, Output: batchOutput},
			{Name: "summary", Help: "counts per project plus current work", Run: Handler(cmdSummary), Flags: []argp.Flag{{Long: "--all-contexts", Kind: argp.Bool}}},
			{Name: "here", Help: "which project the current repository belongs to", Run: Handler(cmdHere)},
			{Name: "watch", Help: "stream new worklog events, one line each, until a timeout", Run: Handler(cmdWatch),
				Flags: []argp.Flag{{Long: "--project", Short: "-p", Metavar: "REF"}, {Long: "--task", Short: "-t", Metavar: "ID"},
					{Long: "--action", Metavar: "LIST"}, {Long: "--actor-not", Metavar: "NAME", Help: "suppress events by this actor; `self` = your actor"},
					{Long: "--session-not", Metavar: "ID", Help: "suppress events from this session; `self` = your session"},
					{Long: "--after", Kind: argp.Int, Metavar: "CURSOR", Help: "start after this event id (default: now)"},
					{Long: "--timeout", Metavar: "DURATION", Default: "1h"}, {Long: "--interval", Metavar: "DURATION", Default: "1s"},
					{Long: "--until", Metavar: "ACTIONS", Help: "exit 0 after an event with one of these actions; `stale` exits 14 when a claim goes quiet"},
					{Long: "--stale", Metavar: "DURATION", Default: "15m", Help: "with --until stale: how long a claim holder may stay quiet"},
					{Long: "--all-contexts", Kind: argp.Bool}}},
			{Name: "serve", Help: "browse and edit the store in a browser (127.0.0.1 only)", Run: Handler(cmdServe),
				Flags: []argp.Flag{{Long: "--port", Kind: argp.Int, Default: 0, Help: "port (default: random)"},
					{Long: "--idle", Metavar: "DURATION", Default: "2h", Help: "exit after this long without requests; 0 = never"},
					{Long: "--open", Kind: argp.Bool, Help: "open the page in a browser"},
					{Long: "--as", Metavar: "NAME", Default: "human", Help: "actor for edits made in the browser (default human; must differ from yours)"},
					{Long: "--new", Kind: argp.Bool, Help: "start another server even if one is running"},
					{Long: "--restart", Kind: argp.Bool, Help: "stop the running server and start a fresh one"}}},
			{Name: "dump", Help: "human-readable export of a whole context (yaml by default); not a backup format", Run: Handler(cmdDump), Flags: []argp.Flag{{Long: "--all-contexts", Kind: argp.Bool}}},
			{Name: "info", Help: "database path, version and row counts", Run: Handler(cmdInfo),
				Flags: []argp.Flag{{Long: "--usage", Kind: argp.Bool, Help: "event counts per actor and per command family, plus events with no actor"},
					{Long: "--since", Metavar: "DURATION", Help: "with --usage: only events from the last 30d, 12h, ... (default all)"}}},
			{Name: "grammar", Help: "the reference grammar, statuses, formats, exit codes and core commands", Run: Handler(cmdGrammar)},
			{Name: "describe", Help: "a command's arguments as JSON", Run: Handler(cmdDescribe), Args: []argp.Arg{{Name: "command", NArgs: "*"}}},
			config, actorTree(),
			ruleTree(), rulesCmd(), checkCmd(), lessonTree(), lessonsCmd(),
		}}
	return root
}

// NewParser builds the parser.
func NewParser() *argp.Parser {
	return &argp.Parser{Prog: "agtrack", Root: Tree(), Globals: Globals, Version: Version}
}
