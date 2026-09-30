"""The golden parity corpus, as data.

Each scenario is an ordered sequence of CLI invocations sharing one scratch
database (a fresh one per scenario, never shared across scenarios). capture.py
replays each step through `pyagtrack`, freezing the clock at
BASE + <step index> minutes, and records argv/stdout/stderr/exit for
internal/golden/golden_test.go to replay later against the Go binary.

Notes on how these are written:
  * IDs (task #, subtask position, pr id) are assumed sequential per scenario,
    starting at 1, in creation order. A step that fails inside a transaction
    consumes no id (BEGIN IMMEDIATE / ROLLBACK), so the numbering stays
    contiguous across a scenario even when a failure case sits in the middle.
  * Steps tagged "known-defect-D<n>" capture current (buggy) Python behaviour
    on purpose -- see go-rewrite-spec.md #5. The Go build fixes these; the
    lead reconciles the resulting golden diff through testdata/golden/deltas.json,
    not by editing these scenarios.
  * A step's default tag is ["parity"]; failure-case steps are additionally
    tagged "failure" so the replay runner (and a human skimming a diff) can
    tell "this failed on purpose" from "this is a regression".
"""

from __future__ import annotations

from typing import Any


def S(*argv: str, **kw: Any) -> dict[str, Any]:
    """One golden step. kw: cwd, env, stdin, normalize, tags."""
    return {"argv": list(argv), **kw}


def F(*argv: str, **kw: Any) -> dict[str, Any]:
    """A step expected to fail: same as S but tags it "failure" too."""
    tags = kw.pop("tags", ["parity"])
    return S(*argv, tags=[*tags, "failure"], **kw)


def D(defect: str, *argv: str, **kw: Any) -> dict[str, Any]:
    """A step that reproduces a numbered Python defect (see spec #5) verbatim."""
    tags = kw.pop("tags", ["parity"])
    return S(*argv, tags=[*tags, f"known-defect-{defect}"], **kw)


SCENARIOS: list[dict[str, Any]] = [

    # ------------------------------------------------------------------
    # 1. Contexts: full lifecycle, formats, aliases, archive/rename, notes.
    # ------------------------------------------------------------------
    {
        "name": "ctx_lifecycle",
        "steps": [
            S("ctx", "add", "acme", "--title", "Acme Corp", "--note", "kickoff", "--use"),
            S("ctx", "add", "other-co", "--title", "Other Co"),
            S("ctx", "ls"),
            S("ctx", "ls", "-f", "json"),
            S("ctx", "ls", "-f", "yaml"),
            S("ctx", "ls", "-f", "tsv"),
            S("ctx", "ls", "-f", "plain"),
            S("ctx", "ls", "--no-header", "-f", "tsv"),
            S("ctx", "ls", "--count"),
            S("ctx", "ls", "--ids"),
            S("context", "ls"),
            S("contexts", "list"),
            S("ctx", "current"),
            S("ctx", "current", "-f", "json"),
            S("ctx", "show", "acme"),
            S("ctx", "show", "acme", "-f", "json"),
            S("ctx", "set", "acme", "--title", "Acme Corporation"),
            S("ctx", "show", "acme"),
            S("ctx", "set", "acme", "--archive"),
            S("ctx", "ls"),
            S("ctx", "ls", "--archived"),
            S("ctx", "set", "acme", "--unarchive"),
            S("ctx", "use", "other-co"),
            S("ctx", "current"),
            S("ctx", "note", "acme", "renamed for testing"),
            S("ctx", "notes", "acme"),
            S("ctx", "rm", "other-co"),
            S("ctx", "ls"),
        ],
    },

    # ------------------------------------------------------------------
    # 2. Context failure paths: conflicts, bad names, not-found, --force.
    # ------------------------------------------------------------------
    {
        "name": "ctx_failures",
        "steps": [
            S("context", "add", "solo", "--use"),
            F("ctx", "add", "solo"),
            F("ctx", "add", ""),
            F("ctx", "add", "!!!"),
            F("ctx", "show", "missing-ctx"),
            F("ctx", "rm", "missing-ctx"),
            F("ctx", "use", "missing-ctx"),
            F("ctx", "set", "missing-ctx", "--title", "x"),
            S("project", "add", "orbiter", "-c", "solo", "--use"),
            F("ctx", "rm", "solo"),
            S("ctx", "rm", "solo", "--force"),
            F("ctx", "current"),
            # Undocumented Python crash, not one of go-rewrite-spec.md #5's D1-D15:
            # `ctx set --rename` passes fields["name"] as a **kwarg into
            # Store.update_context(self, name, **fields), whose first positional
            # parameter is itself named `name` -> TypeError, uncaught, exit 1
            # with a raw traceback. Recorded here for the spec authors, not for
            # the Go build to reproduce -- "skip-replay" tells golden_test.go to
            # leave this step out of the replay comparison entirely.
            S("ctx", "add", "renametest", "--use"),
            S("ctx", "set", "renametest", "--rename", "renamed",
              tags=["parity", "failure", "skip-replay", "undocumented-python-bug"]),
        ],
    },

    # ------------------------------------------------------------------
    # 3. Projects: lifecycle, subprojects, formats, archive/rename, tags.
    # ------------------------------------------------------------------
    {
        "name": "project_lifecycle",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--title", "Orbiter CLI", "--note", "seed",
              "--tag", "core", "--tag", "infra,tools", "--use"),
            S("project", "ls"),
            S("project", "ls", "-f", "json"),
            S("project", "ls", "-f", "yaml"),
            S("project", "ls", "-x"),
            S("project", "show", "orbiter"),
            S("project", "show", "orbiter", "-f", "json"),
            S("project", "set", "orbiter", "--title", "Orbiter CLI Tool"),
            S("project", "set", "orbiter", "--rename", "orbiter2"),
            S("project", "show", "orbiter2"),
            S("project", "use", "orbiter2"),
            S("ctx", "current"),
            S("project", "set", "orbiter2", "--archive"),
            S("project", "ls"),
            S("project", "ls", "--archived"),
            S("project", "set", "orbiter2", "--unarchive"),
            S("project", "add", "orbiter2:frontend", "--title", "Frontend"),
            S("project", "ls"),
            S("project", "ls", "--top"),
            S("project", "ls", "--parent", "orbiter2"),
            S("project", "show", "orbiter2:frontend"),
            S("project", "tag", "orbiter2:frontend", "ui"),
            S("project", "untag", "orbiter2:frontend", "ui"),
            S("project", "note", "orbiter2", "seeded note"),
            S("project", "notes", "orbiter2"),
            S("project", "rm", "orbiter2:frontend"),
            S("project", "rm", "orbiter2"),
            S("project", "ls"),
        ],
    },

    # ------------------------------------------------------------------
    # 4. project repo add/rm/ls, project find, `.` resolution, `here`.
    # ------------------------------------------------------------------
    {
        "name": "project_repo_and_find",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("project", "repo", "add", "orbiter", "https://github.com/acme/widgets.git"),
            S("project", "repo", "add", "orbiter", "git@github.com:acme/widgets2.git"),
            S("project", "repo", "ls", "orbiter"),
            S("project", "repo", "ls", "orbiter", "-f", "json"),
            S("project", "find", "--repo", "https://github.com/acme/widgets"),
            S("project", "find", "--repo", "git@github.com:acme/widgets2.git", "-f", "json"),
            S("project", "find", "--repo", "https://example.com/no/such/repo"),
            S("project", "find", "--repo", "https://example.com/no/such/repo", "-f", "json"),
            S("project", "add", "other", "--use"),
            S("project", "repo", "add", "other", "https://github.com/acme/widgets.git"),
            S("project", "find", "--repo", "https://github.com/acme/widgets"),
            F("task", "add", "cwd task", "-p", ".", cwd="repo"),
            S("project", "repo", "rm", "other", "https://github.com/acme/widgets.git"),
            S("task", "add", "cwd task", "-p", ".", cwd="repo"),
            F("project", "repo", "rm", "orbiter", "https://not-linked.example/x/y"),
            S("here", cwd="repo"),
            S("here", "-f", "json", cwd="repo"),
            S("project", "find", "--use", cwd="repo"),
            S("ctx", "current"),
        ],
    },

    # ------------------------------------------------------------------
    # 5. Tasks: add with pr/step/tag/note, show, set/move, rm, note, tag.
    # ------------------------------------------------------------------
    {
        "name": "task_lifecycle",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("project", "add", "other"),
            S("task", "add", "ship feature"),
            S("task", "start", "1"),
            S("task", "done", "1", "--note", "shipped"),
            S("task", "cancel", "1"),
            S("task", "reopen", "1"),
            S("task", "status", "in-progress", "1", "--tag", "urgent"),
            F("task", "status", "1", "done"),
            S("task", "note", "1", "investigating root cause"),
            S("task", "notes", "1"),
            S("task", "tag", "1", "urgent", "priority"),
            S("task", "untag", "1", "priority"),
            S("task", "set", "1", "--title", "ship the feature properly"),
            S("task", "add", "helper task"),
            S("task", "set", "2", "--move", "other"),
            S("task", "show", "2"),
            S("task", "show", "2", "-x"),
            S("task", "show", "2", "-f", "json"),
            S("task", "rm", "2"),
            F("task", "rm", "1"),
            S("task", "rm", "1", "--force"),
        ],
    },

    # ------------------------------------------------------------------
    # 6. task ls filters: status sets, tags, grep, since, order, repo, limit.
    # ------------------------------------------------------------------
    {
        "name": "task_listing_and_filters",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("project", "add", "orbiter:frontend"),
            S("task", "add", "alpha task", "--tag", "core", "--status", "todo"),
            S("task", "add", "beta task", "--tag", "core", "--tag", "infra",
              "--status", "in-progress"),
            S("task", "add", "gamma task", "-p", "orbiter:frontend", "--status", "done"),
            S("task", "add", "delta cancelled", "--status", "cancelled"),
            S("task", "ls"),
            S("task", "ls", "--status", "all"),
            S("task", "ls", "--status", "open"),
            S("task", "ls", "--status", "closed"),
            S("task", "ls", "--status", "done,cancelled"),
            S("task", "ls", "--tag", "core"),
            S("task", "ls", "--tag", "core", "--tag", "infra", "--tags-all"),
            S("task", "ls", "--grep", "beta"),
            S("task", "ls", "--direct", "-p", "orbiter"),
            S("task", "ls", "-p", "orbiter"),
            S("task", "ls", "--order", "updated", "--status", "all"),
            S("task", "ls", "--order", "created", "--status", "all"),
            S("task", "ls", "--order", "status", "--status", "all"),
            S("task", "ls", "-n", "2", "--status", "all"),
            S("task", "ls", "--since", "1h", "--status", "all"),
            S("task", "ls", "--all-contexts", "--status", "all"),
            S("task", "ls", "--count", "--status", "all"),
            S("task", "ls", "--ids", "--status", "all"),
            S("task", "ls", "--repo", "https://github.com/acme/widgets"),
        ],
    },

    # ------------------------------------------------------------------
    # 7. task status verbs, note/tag combined with a transition, --at.
    # ------------------------------------------------------------------
    {
        "name": "task_status_and_notes",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("task", "add", "tracked task"),
            S("task", "start", "1", "--at", "2026-09-01T12:02:00Z"),
            S("task", "done", "1", "--note", "wrapped up", "--tag", "shipped",
              "--at", "2026-09-01T12:05:00Z"),
            F("task", "start", "1", "--at", "2026-09-01T12:20:00Z"),
            S("task", "add", "task two"),
            S("task", "start", "2", "1"),
            S("task", "show", "1"),
            S("task", "show", "2"),
        ],
    },

    # ------------------------------------------------------------------
    # 8. Subtasks: add (single & multi title), show (id & task.pos), note/tag.
    # ------------------------------------------------------------------
    {
        "name": "subtask_lifecycle",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("task", "add", "big task", "--step", "step one", "--step", "step two"),
            S("sub", "ls", "1"),
            S("sub", "add", "1", "step three", "step four"),
            S("sub", "ls", "1"),
            S("sub", "ls", "1", "-f", "json"),
            S("sub", "show", "1.1"),
            S("sub", "show", "3"),
            S("sub", "note", "1.1", "note on step one"),
            S("sub", "notes", "1.1"),
            S("sub", "tag", "1.1", "blocked"),
            S("sub", "untag", "1.1", "blocked"),
            S("sub", "set", "1.1", "--title", "step one renamed"),
            S("sub", "rm", "1.4"),
            S("sub", "ls", "1"),
        ],
    },

    # ------------------------------------------------------------------
    # 9. Subtask status: cascade, --no-cascade, idempotency, backfill failures.
    # ------------------------------------------------------------------
    {
        "name": "subtask_status_and_cascade",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("task", "add", "cascade task", "--step", "s1", "--step", "s2"),
            S("task", "show", "1"),
            S("sub", "start", "1.1"),
            S("task", "show", "1"),
            S("sub", "done", "1.1"),
            S("task", "show", "1"),
            S("sub", "done", "1.2"),
            S("task", "show", "1"),
            S("task", "add", "no-cascade task", "--step", "x1"),
            S("sub", "start", "2.1", "--no-cascade"),
            S("task", "show", "2"),
            S("sub", "status", "done", "2.1", "--no-cascade"),
            S("sub", "status", "done", "2.1"),
            S("sub", "status", "in-progress", "2.1", "--at", "2026-09-01T12:12:00Z"),
            F("sub", "add", "2", "future step", "--at", "2026-09-05T00:00:00Z"),
            F("sub", "add", "2", "past before task", "--at", "2026-09-01T11:00:00Z"),
        ],
    },

    # ------------------------------------------------------------------
    # 10. Tags overview: ls, show, note/notes, rm and its fallout.
    # ------------------------------------------------------------------
    {
        "name": "tag_overview",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("task", "add", "tagged task", "--tag", "core"),
            S("sub", "add", "1", "tagged step", "--tag", "core"),
            S("project", "tag", "orbiter", "core"),
            S("tag", "ls"),
            S("tag", "ls", "-f", "json"),
            S("tag", "show", "core"),
            S("tag", "show", "core", "-f", "json"),
            S("tag", "note", "core", "core tag note"),
            S("tag", "notes", "core"),
            S("tag", "rm", "core"),
            S("tag", "ls"),
            F("tag", "show", "core"),
        ],
    },

    # ------------------------------------------------------------------
    # 11. Pull requests: add, ls filters, state, rm, duplicate conflict.
    # ------------------------------------------------------------------
    {
        "name": "pr_lifecycle",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("task", "add", "pr task"),
            S("pr", "add", "1", "https://github.com/acme/widgets/pull/42",
              "--title", "Fix bug", "--state", "open"),
            S("pr", "add", "1", "https://example.com/not-a-pr-url", "--state", "draft"),
            S("pr", "ls"),
            S("pr", "ls", "-t", "1"),
            S("pr", "ls", "--repo", "acme/widgets"),
            S("pr", "ls", "--number", "42"),
            S("pr", "ls", "-f", "json"),
            S("pr", "state", "1", "merged"),
            F("pr", "add", "1", "https://github.com/acme/widgets/pull/42"),
            S("pr", "rm", "2"),
            F("pr", "rm", "99"),
            S("pr", "ls"),
        ],
    },

    # ------------------------------------------------------------------
    # 12. Worklog: add (task/project/free), ls filters, by-day, backdated.
    # ------------------------------------------------------------------
    {
        "name": "log_worklog",
        "actor": "agent-a",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("task", "add", "log task"),
            S("log", "add", "started investigating", "-t", "1"),
            S("log", "add", "context wide note", "-p", "orbiter"),
            S("log", "add", "generic note"),
            S("log", "ls"),
            S("log", "ls", "-f", "json"),
            S("log"),
            S("log", "--since", "1h"),
            S("log", "--action", "created,status"),
            S("log", "--by", "agent-a"),
            S("log", "--by-day"),
            S("log", "add", "backdated entry", "--at", "2026-09-01T12:00:30Z"),
            S("log", "--backdated"),
            S("log", "ls", "--task", "1"),
            S("log", "ls", "--project", "orbiter", "--direct"),
            S("log", "ls", "--count"),
            S("log", "ls", "--ids"),
            S("log", "ls", "-n", "1"),
            S("worklog", "add", "alias verb entry"),
        ],
    },

    # ------------------------------------------------------------------
    # 13. next/summary/here/dump/info.
    # ------------------------------------------------------------------
    {
        "name": "overview_commands",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("task", "add", "ov task 1", "--status", "in-progress", "--step", "s1"),
            S("task", "add", "ov task 2", "--status", "todo"),
            S("next"),
            S("next", "--todo"),
            S("next", "-f", "json"),
            S("next", "-n", "1"),
            S("summary"),
            S("summary", "-f", "json"),
            S("summary", "--all-contexts"),
            S("dump"),
            S("dump", "-f", "json"),
            S("dump", "--all-contexts"),
            S("info", normalize=["dbpath"]),
            S("info", "-f", "json", normalize=["dbpath"]),
            F("here"),
            S("here", cwd="repo"),
        ],
    },

    # ------------------------------------------------------------------
    # 14. All five output formats on a listing, a show, and ctx current.
    # ------------------------------------------------------------------
    {
        "name": "formats_all_five",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("task", "add", "fmt task", "--tag", "demo"),
            S("task", "ls", "-f", "table"),
            S("task", "ls", "-f", "plain"),
            S("task", "ls", "-f", "tsv"),
            S("task", "ls", "-f", "json"),
            S("task", "ls", "-f", "yaml"),
            S("task", "ls", "-f", "table", "--no-header"),
            S("task", "ls", "-f", "tsv", "--no-header"),
            S("task", "show", "1", "-f", "table"),
            D("D12", "task", "show", "1", "-f", "json"),
            S("task", "show", "1", "-f", "yaml"),
            S("ctx", "current", "-f", "table"),
            S("ctx", "current", "-f", "json"),
            S("ctx", "current", "-f", "yaml"),
        ],
    },

    # ------------------------------------------------------------------
    # 15. --fields, -q, --count, --ids, and the unknown-field failure.
    # ------------------------------------------------------------------
    {
        "name": "fields_quiet_count_ids",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("task", "add", "f1", "--tag", "x"),
            S("task", "add", "f2", "--tag", "y"),
            S("task", "ls", "--fields", "id,title,status"),
            S("task", "ls", "--fields", "id,title,status", "-f", "json"),
            F("task", "ls", "--fields", "bogus_field"),
            S("task", "ls", "-q"),
            S("task", "ls", "--ids"),
            S("task", "ls", "--count"),
            S("task", "add", "f3", "-q"),
            S("ctx", "add", "quiet-ctx", "-q"),
            S("project", "add", "quiet-proj", "-c", "quiet-ctx", "-q"),
            S("task", "done", "1", "-q"),
        ],
    },

    # ------------------------------------------------------------------
    # 16. Global flag placement (before/after noun) and every group alias.
    # ------------------------------------------------------------------
    {
        "name": "global_flags_and_aliases",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("ctx", "add", "other-ctx"),
            S("project", "add", "otherp", "-c", "other-ctx", "--use"),
            S("-c", "acme", "task", "ls"),
            S("task", "ls", "-c", "acme"),
            S("-f", "json", "task", "ls", "-c", "acme"),
            S("task", "ls", "-c", "acme", "-f", "json"),
            S("context", "ls"),
            S("contexts", "list"),
            S("p", "ls", "-c", "acme"),
            S("proj", "list", "-c", "acme"),
            S("projects", "show", "orbiter", "-c", "acme"),
            S("t", "add", "alias task", "-p", "orbiter", "-c", "acme"),
            S("tasks", "ls", "-p", "orbiter", "-c", "acme"),
            S("task", "list", "-p", "orbiter", "-c", "acme"),
            S("subtask", "add", "1", "aliased sub"),
            S("subtasks", "ls", "1"),
            S("s", "show", "1.1"),
            S("sub", "list", "1"),
            S("tags", "ls"),
            S("prs", "ls"),
            S("worklog", "add", "alias log entry"),
            S("log", "list"),
        ],
    },

    # ------------------------------------------------------------------
    # 17. argparse quirks: prefix flags, `--`, usage errors, bad choices.
    # ------------------------------------------------------------------
    {
        "name": "argparse_edges",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("task", "add", "edge task", "--tag", "a", "--tag", "b,c"),
            S("task", "ls", "--proj", "orbiter"),
            S("task", "ls", "--gre", "edge"),
            S("project", "add", "orbiter:sub1"),
            S("project", "ls", "--parent", "orbiter"),
            F("project", "ls", "-p", "orbiter"),
            F("task", "ls", "-n", "0"),
            F("task", "ls", "-f", "xml"),
            F("ctx", "add"),
            F("task", "add"),
            F(),
            F("ctx"),
            F("bogus-noun"),
            S("task", "add", "--", "--weird-title"),
            S("task", "ls", "--fie", "id,title"),
        ],
    },

    # ------------------------------------------------------------------
    # 18. Reference grammar: project/task/subtask ref forms, status aliases.
    # ------------------------------------------------------------------
    {
        "name": "refs_grammar",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("project", "add", "orbiter:frontend"),
            S("task", "add", "ref task 1"),
            S("task", "add", "ref task 2"),
            S("sub", "add", "1", "s1"),
            S("task", "show", "1"),
            S("task", "show", "#1"),
            S("sub", "show", "1"),
            S("sub", "show", "1.1"),
            S("project", "show", "orbiter"),
            S("project", "show", "acme/orbiter"),
            S("project", "show", "orbiter:frontend"),
            S("project", "show", "acme/orbiter:frontend"),
            S("project", "repo", "add", "orbiter", "git@github.com:acme/widgets.git"),
            S("task", "add", "cwd task", "-p", ".", cwd="repo"),
            S("here", cwd="repo"),
            S("project", "find", cwd="repo"),
            S("task", "status", "wip", "1"),
            S("task", "status", "complete", "1"),
            S("task", "status", "pending", "1"),
            S("task", "status", "x", "1"),
        ],
    },

    # ------------------------------------------------------------------
    # 19. Repo URL normalisation forms and PR URL parsing.
    # ------------------------------------------------------------------
    {
        "name": "repo_and_pr_url_parsing",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("project", "repo", "add", "orbiter", "https://github.com/acme/widgets.git"),
            S("project", "repo", "add", "orbiter", "git@github.com:acme/widgets-ssh.git"),
            S("project", "repo", "add", "orbiter", "git://github.com/acme/widgets-proto.git"),
            S("project", "repo", "add", "orbiter", "acme/widgets-bare"),
            S("project", "repo", "add", "orbiter", "https://gitlab.com/acme/widgets-gitlab/"),
            S("project", "repo", "ls", "orbiter"),
            S("task", "add", "pr url task"),
            S("pr", "add", "1", "https://github.com/acme/widgets/pull/7"),
            S("pr", "add", "1", "https://github.com/acme/widgets/pulls/8"),
            S("pr", "add", "1", "https://example.com/no/pr/pattern/here"),
            S("pr", "ls", "-t", "1", "-f", "json"),
        ],
    },

    # ------------------------------------------------------------------
    # 20. Time parsing: parse_since forms, parse_when order/future checks.
    # ------------------------------------------------------------------
    {
        "name": "time_parsing",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("task", "add", "time task 1", "--created", "2026-08-01"),
            S("task", "add", "time task 2", "--created", "2026-08-15",
              "--started", "2026-08-16", "--status", "in-progress"),
            S("task", "add", "time task 3", "--created", "2026-08-01",
              "--status", "done", "--closed", "2026-08-20"),
            S("task", "add", "time task 4", "--status", "done"),
            S("task", "ls", "--since", "2d", "--status", "all"),
            S("task", "ls", "--since", "36h", "--status", "all"),
            S("task", "ls", "--since", "90m", "--status", "all"),
            S("task", "ls", "--since", "45s", "--status", "all"),
            S("task", "ls", "--since", "3w", "--status", "all"),
            S("task", "ls", "--since", "today", "--status", "all"),
            S("task", "ls", "--since", "yesterday", "--status", "all"),
            S("task", "ls", "--since", "2026-08-10", "--status", "all"),
            F("task", "ls", "--since", "not-a-time"),
            F("task", "add", "future closed", "--status", "done",
              "--closed", "2026-08-01", "--created", "2026-08-05"),
            F("task", "add", "bad status closed", "--closed", "2026-08-01"),
        ],
    },

    # ------------------------------------------------------------------
    # 21. not-found and bad-reference failures across every noun.
    # ------------------------------------------------------------------
    {
        "name": "failure_notfound_and_badref",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("task", "add", "seed task"),
            F("task", "show", "999"),
            D("D4", "task", "show", "999", "-f", "json", tags=["parity", "failure"]),
            F("task", "show", "abc"),
            F("sub", "show", "999"),
            F("sub", "show", "abc"),
            F("sub", "show", "1.99"),
            F("project", "show", "missing"),
            F("project", "show", "a:b:c"),
            F("project", "show", "ctx-missing/orbiter"),
            F("ctx", "show", "missing"),
            F("tag", "show", "missing"),
            F("pr", "rm", "999"),
            F("pr", "state", "999", "merged"),
            F("ctx", "add", "!!!"),
        ],
    },

    # ------------------------------------------------------------------
    # 22. Conflict and children-guard failures across every noun.
    # ------------------------------------------------------------------
    {
        "name": "failure_conflict_and_children",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            F("ctx", "add", "acme"),
            F("project", "add", "orbiter"),
            S("task", "add", "pr dup task"),
            S("pr", "add", "1", "https://github.com/a/b/pull/1"),
            F("pr", "add", "1", "https://github.com/a/b/pull/1"),
            S("task", "add", "child task", "--step", "s1"),
            S("task", "note", "2", "a note"),
            F("task", "rm", "2"),
            S("task", "rm", "2", "--force"),
            S("task", "add", "sub child task", "--step", "s1"),
            S("sub", "note", "2.1", "note on sub"),
            F("sub", "rm", "2.1"),
            S("sub", "rm", "2.1", "--force"),
            S("project", "add", "orbiter:frontend"),
            F("project", "rm", "orbiter"),
            S("project", "rm", "orbiter", "--force"),
            S("ctx", "rm", "acme"),
        ],
    },

    # ------------------------------------------------------------------
    # 23. No-current-context / no-current-project error paths.
    # ------------------------------------------------------------------
    {
        "name": "failure_no_current_context",
        "steps": [
            S("ctx", "add", "a"),
            S("ctx", "add", "b"),
            S("ctx", "add", "c"),
            S("ctx", "rm", "a"),
            F("project", "ls"),
            F("task", "add", "x"),
            F("project", "add", "newproj"),
            S("ctx", "use", "b"),
            S("project", "ls"),
            F("task", "add", "y"),
            S("project", "add", "p1", "--use"),
            S("task", "add", "y"),
        ],
    },

    # ------------------------------------------------------------------
    # 24. D1: `ctx use` leaves the project pin behind in the old context.
    # ------------------------------------------------------------------
    {
        "name": "d1_stale_pin_bug",
        "actor": "agent-c",
        "steps": [
            S("ctx", "add", "work", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("ctx", "add", "personal", "--use"),
            D("D1", "ctx", "current"),
            D("D1", "ctx", "current", "-f", "json"),
            D("D1", "task", "add", "silently misfiled", "--status", "todo"),
            D("D1", "task", "ls"),
            D("D1", "task", "ls", "-c", "work"),
            D("D1", "task", "ls", "--all-contexts", "-f", "json"),
        ],
    },

    # ------------------------------------------------------------------
    # 25. D10: a repeated status transition re-appends its --note.
    # ------------------------------------------------------------------
    {
        "name": "idempotent_status_note_reappend",
        "actor": "agent-b",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("task", "add", "idempotent task"),
            S("task", "done", "1"),
            S("task", "done", "1"),
            D("D10", "task", "done", "1", "--note", "still done"),
            D("D10", "task", "done", "1", "--note", "again"),
            S("task", "notes", "1"),
            S("log", "ls", "--task", "1", "-f", "json"),
            S("sub", "add", "1", "step"),
            S("sub", "done", "1.1"),
            S("sub", "done", "1.1"),
            D("D10", "sub", "done", "1.1", "--note", "sub note repeat"),
            S("sub", "notes", "1.1"),
        ],
    },

    # ------------------------------------------------------------------
    # 26. task add backfill: --created/--started/--closed combinations.
    # ------------------------------------------------------------------
    {
        "name": "backfill_task_add",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("task", "add", "bf1", "--created", "2026-08-01T00:00:00Z"),
            S("task", "show", "1"),
            S("task", "add", "bf2", "--created", "2026-08-01T00:00:00Z",
              "--started", "2026-08-02T00:00:00Z", "--status", "in-progress"),
            S("task", "show", "2"),
            S("task", "add", "bf3", "--created", "2026-08-01T00:00:00Z",
              "--status", "done", "--closed", "2026-08-05T00:00:00Z"),
            S("task", "show", "3"),
            S("task", "add", "bf4", "--created", "2026-08-01T00:00:00Z",
              "--started", "2026-08-02T00:00:00Z", "--status", "cancelled",
              "--closed", "2026-08-03T00:00:00Z"),
            S("task", "show", "4"),
            S("log", "ls", "--backdated", "-f", "json"),
            S("log", "--by-day"),
        ],
    },

    # ------------------------------------------------------------------
    # 27. Subtask backfill (--at) ordering, including a before-creation error.
    # ------------------------------------------------------------------
    {
        "name": "backfill_subtask_and_ordering",
        "steps": [
            S("ctx", "add", "acme", "--use"),
            S("project", "add", "orbiter", "--use"),
            S("task", "add", "parent", "--created", "2026-08-01T00:00:00Z"),
            S("sub", "add", "1", "s1", "--at", "2026-08-02T00:00:00Z"),
            S("sub", "add", "1", "s2", "--at", "2026-08-03T00:00:00Z"),
            S("sub", "ls", "1"),
            S("sub", "status", "done", "1.1", "--at", "2026-08-04T00:00:00Z"),
            F("sub", "status", "done", "1.2", "--at", "2026-08-01T12:00:00Z"),
            S("sub", "status", "done", "1.2", "--at", "2026-08-05T00:00:00Z"),
            S("task", "show", "1", "-x"),
        ],
    },
]
