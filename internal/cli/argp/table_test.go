package argp

// agtrackParser is a faithful replica of the Python CLI's parser
// (agtracklib/cli.py build_parser) used as the fixture for parity tests.
func agtrackParser() *Parser {
	formats := []string{"table", "plain", "tsv", "json", "yaml"}
	globals := []Flag{
		{Long: "--db", Metavar: "PATH", Help: "database file"},
		{Long: "--context", Short: "-c", Metavar: "NAME", Help: "context to work in (default: the current one)"},
		{Long: "--format", Short: "-f", Choices: formats, Help: "output format (default table)"},
		{Long: "--fields", Metavar: "A,B", Help: "exact columns to print, comma separated"},
		{Long: "--extended", Short: "-x", Kind: Bool, Help: "wider view: extra columns and nested detail"},
		{Long: "--no-header", Kind: Bool, Help: "omit the header row"},
		{Long: "--color", Kind: Bool},
		{Long: "--no-color", Kind: Bool},
		{Long: "--quiet", Short: "-q", Kind: Bool, Help: "identifiers only"},
		{Long: "--actor", Metavar: "NAME", Help: "attribute writes to this actor (default $AGTRACK_ACTOR)"},
	}
	listFlags := []Flag{
		{Long: "--count", Kind: Bool, Help: "print the row count only"},
		{Long: "--ids", Kind: Bool, Help: "print identifiers only"},
	}
	cat := func(parts ...[]Flag) []Flag {
		var out []Flag
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	one := func(name string) Arg { return Arg{Name: name} }
	plus := func(name string) Arg { return Arg{Name: name, NArgs: "+"} }
	noteVerbs := func(noun, refHelp string, taggable bool) []*Command {
		out := []*Command{
			{Name: "note", Help: "attach a note to a " + noun,
				Args:  []Arg{{Name: "ref", Help: refHelp}, plus("body")},
				Flags: []Flag{{Long: "--at", Metavar: "WHEN", Help: "backfill: when it was written"}}},
			{Name: "notes", Help: "notes on a " + noun, Args: []Arg{{Name: "ref", Help: refHelp}}},
		}
		if taggable {
			out = append(out,
				&Command{Name: "tag", Help: "add tags to a " + noun, Args: []Arg{{Name: "ref", Help: refHelp}, plus("tags")}},
				&Command{Name: "untag", Help: "remove tags from a " + noun, Args: []Arg{{Name: "ref", Help: refHelp}, plus("tags")}})
		}
		return out
	}
	tagFlag := Flag{Long: "--tag", Kind: Append, Metavar: "TAG"}
	statusVerbs := func(sub bool) []*Command {
		var out []*Command
		for _, v := range [][2]string{{"start", "mark in-progress"}, {"done", "mark done"}, {"cancel", "mark cancelled"}, {"reopen", "back to todo"}} {
			flags := []Flag{{Long: "--note"}, tagFlag, {Long: "--at", Metavar: "WHEN"}}
			if sub {
				flags = append(flags, Flag{Long: "--no-cascade", Kind: Bool})
			}
			out = append(out, &Command{Name: v[0], Help: v[1], Args: []Arg{plus("ref")}, Flags: flags})
		}
		flags := []Flag{{Long: "--note"}, tagFlag, {Long: "--at", Metavar: "WHEN"}}
		if sub {
			flags = append(flags, Flag{Long: "--no-cascade", Kind: Bool})
		}
		return append(out, &Command{Name: "status", Help: "set an explicit status",
			Args: []Arg{one("status"), plus("ref")}, Flags: flags})
	}
	logFilters := cat([]Flag{
		{Long: "--since", Metavar: "WHEN"},
		{Long: "--until", Metavar: "WHEN"},
		{Long: "--task", Short: "-t", Metavar: "ID"},
		{Long: "--project", Short: "-p", Metavar: "REF"},
		{Long: "--direct", Kind: Bool},
		{Long: "--by", Dest: "actor_filter", Metavar: "ACTOR"},
		{Long: "--action", Metavar: "LIST"},
		{Long: "--limit", Short: "-n", Kind: PositiveInt, Default: 50},
		{Long: "--all-contexts", Kind: Bool},
		{Long: "--by-day", Kind: Bool},
		{Long: "--backdated", Kind: Bool},
	}, listFlags)

	ctx := &Command{Name: "ctx", Aliases: []string{"context", "contexts"}, Help: "contexts: personal, company-a, ...", SubRequired: true,
		Subs: append([]*Command{
			{Name: "add", Help: "create a context", Args: []Arg{one("name")},
				Flags: []Flag{{Long: "--title"}, {Long: "--note"}, {Long: "--use", Kind: Bool, Help: "make it current"}}},
			{Name: "ls", Aliases: []string{"list"}, Help: "list contexts", Flags: cat([]Flag{{Long: "--archived", Kind: Bool}}, listFlags)},
			{Name: "use", Help: "set the current context", Args: []Arg{one("name")}},
			{Name: "current", Help: "print the current context"},
			{Name: "show", Help: "context detail", Args: []Arg{one("name")}},
			{Name: "set", Help: "rename, retitle or archive", Args: []Arg{one("name")},
				Flags: []Flag{{Long: "--title"}, {Long: "--rename", Metavar: "NEW"}, {Long: "--archive", Kind: Bool}, {Long: "--unarchive", Kind: Bool}}},
			{Name: "rm", Aliases: []string{"delete"}, Help: "delete a context", Args: []Arg{one("name")}, Flags: []Flag{{Long: "--force", Kind: Bool}}},
		}, noteVerbs("context", "context name", false)...)}

	project := &Command{Name: "project", Aliases: []string{"proj", "projects", "p"}, Help: "projects and subprojects", SubRequired: true,
		Subs: append([]*Command{
			{Name: "add", Help: "create a project or subproject", Args: []Arg{{Name: "ref", Help: "project or project:subproject"}},
				Flags: []Flag{{Long: "--title"}, {Long: "--repo", Kind: Append, Metavar: "URL"}, tagFlag, {Long: "--note"}, {Long: "--use", Kind: Bool}}},
			{Name: "ls", Aliases: []string{"list"}, Help: "list projects",
				Flags: cat([]Flag{{Long: "--parent", Metavar: "REF"}, {Long: "--top", Kind: Bool}, {Long: "--archived", Kind: Bool}, {Long: "--all-contexts", Kind: Bool}}, listFlags)},
			{Name: "show", Help: "project detail", Args: []Arg{one("ref")}},
			{Name: "use", Help: "pin a project", Args: []Arg{one("ref")}},
			{Name: "set", Help: "rename, retitle or archive", Args: []Arg{one("ref")},
				Flags: []Flag{{Long: "--title"}, {Long: "--rename", Metavar: "NEW"}, {Long: "--archive", Kind: Bool}, {Long: "--unarchive", Kind: Bool}}},
			{Name: "rm", Aliases: []string{"delete"}, Help: "delete a project", Args: []Arg{one("ref")}, Flags: []Flag{{Long: "--force", Kind: Bool}}},
			{Name: "repo", Help: "link git remotes to a project",
				Args: []Arg{{Name: "repo_action", Choices: []string{"add", "rm", "ls"}}, one("ref"), {Name: "url", NArgs: "*"}}},
			{Name: "find", Help: "which project owns a git remote",
				Flags: cat([]Flag{{Long: "--repo", Metavar: "URL"}, {Long: "--use", Kind: Bool}}, listFlags)},
		}, noteVerbs("project", "project or project:subproject", true)...)}

	taskSubs := []*Command{
		{Name: "add", Help: "create a task", Args: []Arg{plus("title")},
			Flags: []Flag{
				{Long: "--project", Short: "-p", Metavar: "REF", Help: "project, project:subproject or . for the cwd repo"},
				{Long: "--status", Help: "initial status (default todo)"},
				tagFlag, {Long: "--note"},
				{Long: "--pr", Kind: Append, Metavar: "URL"},
				{Long: "--step", Kind: Append, Metavar: "TITLE", Help: "create a subtask at the same time; repeatable"},
				{Long: "--created", Metavar: "WHEN"}, {Long: "--started", Metavar: "WHEN"}, {Long: "--closed", Metavar: "WHEN"},
			}},
		{Name: "ls", Aliases: []string{"list"}, Help: "list tasks",
			Flags: cat([]Flag{
				{Long: "--project", Short: "-p", Metavar: "REF"},
				{Long: "--direct", Kind: Bool},
				{Long: "--status", Short: "-s", Metavar: "LIST"},
				tagFlag,
				{Long: "--tags-all", Kind: Bool},
				{Long: "--grep", Metavar: "TEXT"},
				{Long: "--since", Metavar: "WHEN"},
				{Long: "--repo", Metavar: "URL"},
				{Long: "--limit", Short: "-n", Kind: PositiveInt},
				{Long: "--order", Choices: []string{"created", "id", "status", "updated"}, Default: "id"},
				{Long: "--all-contexts", Kind: Bool},
			}, listFlags)},
		{Name: "show", Help: "one task with its steps, notes and PRs", Args: []Arg{one("ref")}},
	}
	taskSubs = append(taskSubs, statusVerbs(false)...)
	taskSubs = append(taskSubs,
		&Command{Name: "set", Help: "retitle or move a task", Args: []Arg{one("ref")},
			Flags: []Flag{{Long: "--title", Kind: NArgsPlus}, {Long: "--move", Metavar: "REF"}}},
		&Command{Name: "rm", Aliases: []string{"delete"}, Help: "delete tasks", Args: []Arg{plus("ref")}, Flags: []Flag{{Long: "--force", Kind: Bool}}})
	task := &Command{Name: "task", Aliases: []string{"tasks", "t"}, Help: "tasks: the unit of work, one per project", SubRequired: true,
		Subs: append(taskSubs, noteVerbs("task", "task id", true)...)}

	subSubs := []*Command{
		{Name: "add", Help: "add one or more steps to a task", Args: []Arg{{Name: "task", Help: "task id"}, {Name: "title", NArgs: "+", Help: "one step per argument"}},
			Flags: []Flag{tagFlag, {Long: "--note"}, {Long: "--at", Metavar: "WHEN"}}},
		{Name: "ls", Aliases: []string{"list"}, Help: "steps of a task", Args: []Arg{one("task")},
			Flags: cat([]Flag{{Long: "--status", Short: "-s", Metavar: "LIST"}, {Long: "--limit", Short: "-n", Kind: PositiveInt}}, listFlags)},
		{Name: "show", Help: "subtask detail", Args: []Arg{one("ref")}},
	}
	subSubs = append(subSubs, statusVerbs(true)...)
	subSubs = append(subSubs,
		&Command{Name: "set", Help: "retitle a step", Args: []Arg{one("ref")}, Flags: []Flag{{Long: "--title", Kind: NArgsPlus}}},
		&Command{Name: "rm", Aliases: []string{"delete"}, Help: "delete steps", Args: []Arg{plus("ref")}, Flags: []Flag{{Long: "--force", Kind: Bool}}})
	sub := &Command{Name: "sub", Aliases: []string{"subtask", "subtasks", "s"}, Help: "subtasks: the step list under a task", SubRequired: true,
		Subs: append(subSubs, noteVerbs("subtask", "subtask id or task.position", true)...)}

	tag := &Command{Name: "tag", Aliases: []string{"tags"}, Help: "tags across projects, tasks and subtasks", SubRequired: true,
		Subs: append([]*Command{
			{Name: "ls", Aliases: []string{"list"}, Help: "list tags", Flags: listFlags},
			{Name: "show", Help: "everything carrying a tag", Args: []Arg{one("ref")}},
			{Name: "rm", Aliases: []string{"delete"}, Help: "delete a tag and its assignments", Args: []Arg{one("ref")}},
		}, noteVerbs("tag", "tag name", false)...)}

	pr := &Command{Name: "pr", Aliases: []string{"prs"}, Help: "pull requests linked to tasks", SubRequired: true,
		Subs: []*Command{
			{Name: "add", Help: "link a PR to a task", Args: []Arg{one("task"), one("url")},
				Flags: []Flag{{Long: "--title"}, {Long: "--state", Help: "open, draft, merged or closed"}}},
			{Name: "ls", Aliases: []string{"list"}, Help: "list PRs",
				Flags: cat([]Flag{{Long: "--task", Short: "-t", Metavar: "ID"}, {Long: "--repo", Metavar: "URL"}, {Long: "--number", Kind: Int},
					{Long: "--url"}, {Long: "--state"}, {Long: "--all-contexts", Kind: Bool}}, listFlags)},
			{Name: "state", Help: "update a PR state", Args: []Arg{{Name: "id", Help: "pr record id"}, one("new_state")}},
			{Name: "rm", Aliases: []string{"delete"}, Help: "unlink a PR", Args: []Arg{one("id")}},
		}}

	log := &Command{Name: "log", Aliases: []string{"worklog"}, Help: "the worklog: what happened and when",
		Flags: logFilters,
		Subs: []*Command{
			{Name: "add", Help: "append a free form entry", Args: []Arg{plus("body")},
				Flags: []Flag{{Long: "--task", Short: "-t", Metavar: "ID"}, {Long: "--project", Short: "-p", Metavar: "REF"}, {Long: "--at", Metavar: "WHEN"}}},
			{Name: "ls", Help: "read the worklog", Flags: logFilters},
			{Name: "list", Help: "read the worklog", Flags: logFilters},
		}}

	root := &Command{Name: "agtrack", Help: "Context / project / task / subtask tracker for agents and humans.",
		SubRequired: true, Epilog: "exit codes:\n  0 ok  1 error  2 usage",
		Subs: []*Command{ctx, project, task, sub, tag, pr, log,
			{Name: "next", Help: "what is in progress, with its open steps",
				Flags: []Flag{{Long: "--project", Short: "-p", Metavar: "REF"}, {Long: "--todo", Kind: Bool}, {Long: "--limit", Short: "-n", Kind: PositiveInt}, {Long: "--all-contexts", Kind: Bool}}},
			{Name: "summary", Help: "counts per project plus current work", Flags: []Flag{{Long: "--all-contexts", Kind: Bool}}},
			{Name: "here", Help: "which project the current repository belongs to"},
			{Name: "dump", Help: "export a whole context (yaml by default)", Flags: []Flag{{Long: "--all-contexts", Kind: Bool}}},
			{Name: "info", Help: "database path, version and row counts"},
		}}
	return &Parser{Prog: "agtrack", Root: root, Globals: globals, Version: "1.0.0"}
}
