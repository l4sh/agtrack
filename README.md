# agtrack

A task tracker built for coding agents and the humans who work with them: contexts,
projects, tasks, subtasks, a worklog, and markdown **specs** that can be addressed by
section, annotated, reviewed in a browser, and linked to the tasks that implement them.
It is one static binary over one SQLite file.

```console
$ agtrack resume
project   work/orbiter:api   repo github.com/acme-tools/orbiter
task      #1 [~] Rotate refresh tokens on use   started 2026-09-26 by codex
claim     codex until 2026-09-29 15:20
steps     1.1    [x] parse the grace window
          1.2    [~] invalidate the old token
          1.3    [ ] backfill existing sessions
notes     > alex: grace window must be configurable, not 60s hardcoded  (note 7)
spec      auth#tokens__rotation  changed since #1 was linked (v3 -> v5)
blocked   none
recent    09-27 14:02 codex 1.1 status todo -> done
```

## Scope: one machine

agtrack is a **per-machine store**. Every agent and every checkout on a machine shares one
database; agents on two machines share nothing. Do not point `AGTRACK_DB` at a network
share: SQLite's WAL mode needs every process on the same host. Every entity carries a
`uid` so a cross-machine transport can be added later without renumbering anything.

## Install

Download a release archive for your platform (`linux/amd64`, `linux/arm64`,
`darwin/amd64`, `darwin/arm64`), check it against `SHA256SUMS`, and put the binary on your
`PATH`:

```sh
version=2.0.0
os=$(uname -s | tr A-Z a-z); arch=$(uname -m); [ "$arch" = x86_64 ] && arch=amd64; [ "$arch" = aarch64 ] && arch=arm64
curl -LO https://github.com/l4sh/agtrack/releases/download/v$version/agtrack_${version}_${os}_${arch}.tar.gz
tar -xzf agtrack_${version}_${os}_${arch}.tar.gz
install -m 0755 agtrack_${version}_${os}_${arch}/agtrack ~/.local/bin/agtrack
```

Or build from source (Go 1.24+, no cgo, no C toolchain):

```sh
go install github.com/l4sh/agtrack/cmd/agtrack@latest
# or, from a checkout, all four targets with checksums:
scripts/build-release.sh 2.0.0
```

### The agent skill

agtrack ships an agent skill (`skills/agtrack/SKILL.md` plus reference files, in the
open SKILL.md format) that teaches coding agents how to use it. The binary embeds the
skill that matches its version:

```sh
agtrack skill install                 # every harness found under $HOME (Claude Code, Codex, Antigravity/Gemini CLI)
agtrack skill install --agent claude  # just one (claude, codex, agy, all)
agtrack skill install --dir .claude/skills   # any skills directory, e.g. a project's
agtrack skill status                  # where it is installed, and whether it is current
agtrack skill uninstall --agent codex
agtrack skill print                   # SKILL.md to stdout, for harnesses without skill folders
```

Installs are copies with a marker file, so `skill status` reports outdated or locally
edited copies, and an existing skill agtrack did not install (for example a symlink) is
only replaced with `--force`. Re-run `agtrack skill install` after upgrading the binary.

Harness skill directories: Claude Code `~/.claude/skills`, Codex `~/.codex/skills`,
Antigravity/Gemini CLI `~/.gemini/config/skills`. For any other harness that reads
SKILL.md skills, use `--dir`. The skill sits in the standard `skills/<name>/SKILL.md` layout, so repository-based
installers such as `npx skills add l4sh/agtrack` should find it (not yet tested).

On macOS an unsigned binary downloaded by a browser is quarantined; `xattr -d
com.apple.quarantine ./agtrack` clears it (archives fetched with `curl` are not affected).

## Storage

The database is resolved from `--db PATH`, else `$AGTRACK_DB`, else
`$XDG_DATA_HOME/agtrack/agtrack.db`, else `~/.local/share/agtrack/agtrack.db`.

**Sandboxed agents need `AGTRACK_DB`.** WAL mode creates `-wal` and `-shm` files beside the
database, so a process needs write access to the *directory*; an agent confined to its
workspace cannot use the default location. The error names the path it tried and exits 5.

**Upgrading from agtrack 1.x (Python).** The Go build opens an existing schema-v2 database
in place and migrates it to v3 on first open: existing ids, including worklog event ids,
are kept. The migration is one-way — the Python build refuses a v3 database — so before
migrating agtrack writes a consistent copy of the old file to `<db>.v2.bak` (restore it with
`mv agtrack.db.v2.bak agtrack.db` to go back to 1.x, losing anything written since).

| Variable | Effect |
|---|---|
| `AGTRACK_DB` | database path |
| `AGTRACK_ACTOR` | who is writing: recorded on every note and worklog entry, and scopes the project pin to this actor (`--actor` overrides) |
| `AGTRACK_FORMAT` | default output format |
| `NO_COLOR` | disables colour (so do `--no-color` and any non-terminal stdout) |

## For agents: start here

```sh
agtrack grammar          # one screen: refs, statuses, formats, exit codes, the core commands
agtrack resume           # where am I: task, steps, open notes, spec drift, blockers, claims, recent log
```

The commands an agent actually uses:

```sh
agtrack next --claim                                  # take the next doable task (skips claimed and blocked)
agtrack task add "title" -p . --step one --step two --idem "$KEY"
agtrack sub start 12.1; agtrack sub done 12.1          # a step pulls a todo task into in-progress
agtrack task note 12 --kind handoff "what the next agent needs"
agtrack log add -t 12 "milestone or blocker, and why"
agtrack task done 12                                    # releases the claim
```

- **Closure and hygiene**: `sub done 12.3 --close-parent` closes the task with its last
  step (otherwise a hint says so); `sub next 12` closes the current step and starts the
  next; `--evidence commit:SHA` on `task done`/`sub done`; `next` flags stale tasks with `!`
  (`next --stale`, `task done-check [--apply --yes]`, `config set next.stale_days N`);
  `task snooze 97 --until 3d` / `task unsnooze` / `next --parked`. Mutations may return a
  `hint` (JSON) or a final `→ command` line.
- **Pins are per actor.** `project use` pins for the current `AGTRACK_ACTOR` only; `ctx use`
  drops a project pin that lies in another context; an unqualified write with no
  resolvable project fails instead of guessing. `-p .` resolves the project linked to the
  cwd's git remote.
- **Retries are safe** with `--idem KEY` on any create: a retry with the same key reprints
  the first result and writes nothing; the same key with a different request is refused
  (`request_key_reused`).
- **Claims are advisory and expire** (`task claim 12 --ttl 45m`, `--steal`, `task release`,
  `task ls --unclaimed`). They never block a write; they tell `resume`, `next` and `task ls`
  who is on what. A claim cannot fence a git worktree: an expired holder may still be editing.
  `claims [--stale 15m]` shows holders and who went quiet; `task transfer 12 --to X` hands a
  task over with a handoff note; `watch --until stale` exits 14 when a claim goes quiet.
- **Acknowledge handoffs**: `note ack 7` takes on a handoff, blocker or question (resolving
  stays separate); `notes --unacked` lists the ones nobody took. `task note 12 --kind
  handoff --close 12.3 "..."` hands off and finishes the step in one transaction.
- **Dependencies** (`task block 14 --on 12`) keep blocked tasks out of `next`; `task ls
  --ready` lists the doable ones. Blocked-ness is not a status.
- **Typed notes**: `--kind note|decision|handoff|blocker|question`; `notes -p .` lists every
  unresolved one; `note resolve 7 --note why`, `note reopen`, `note reply`, `note rm`.
- **Actors**: optional registry (`actor add <slug> [--kind agent|human] [--alias A]`, `actor alias`,
  `actor ls`, `actor rm`, `actor merge <alias> --into <slug> --yes`). Writes store the canonical
  slug; an unregistered actor warns (`config set actor.strict true` makes it fail), and
  `config set actor.required true` fails writes with no actor. `config set rules.approvers a,b`
  (default `human`) names who may approve rules. Settings: `config get` lists them.
- **Artifacts**: `task artifact 12 branch feat/x`, `... file auth/tokens.py --label "..."`.

## References

| Kind | Syntax |
|---|---|
| context | `personal`, `company-a` |
| project | `orbiter`, `orbiter:frontend`, `work/orbiter:frontend`, `.` for the cwd repo |
| task | `12`, `#12` |
| subtask | `47` (id) or `12.3` (task 12, third step) |
| status | `todo`, `in-progress`, `done`, `cancelled` plus aliases (`wip`, `doing`, `complete`, ...); filters also take `open`, `closed`, `all` and comma lists |
| spec | `auth`, `orbiter:api/auth` |
| section | `auth#tokens__rotation` (path slug from the heading hierarchy), `auth#grace-policy` (a pin), `auth@3#tokens__rotation` (a past revision) |

## Specs

A spec is a markdown file in the repo. The file owns the prose; agtrack owns sections,
annotations and task links. Pins flow the other way only when something durable needs one.

```sh
agtrack spec add specs/auth.md -p .          # v1; the file is not modified
agtrack spec toc auth                         # sections, notes, linked tasks
agtrack spec show auth#tokens__rotation       # the section body, nothing else
agtrack spec find auth "grace window"         # FTS5 search over head revisions
agtrack spec note auth#tokens__rotation --kind question --quote "grace window of 60s" "per client?"
agtrack spec link auth#tokens__rotation 12    # stamps what the task agreed to
agtrack spec sync auth                        # import an edited file as a new revision
agtrack spec stale auth                       # tasks whose sections changed under them
```

Sections are addressed by a **path slug** derived from the heading hierarchy
(`## Tokens` → `### Rotation` is `tokens__rotation`), so the document needs no markup. A
slug does not survive a heading rename, so the first note or link on a section **pins** it
by inserting one line above its heading — `<!-- agtrack:id grace-policy -->` — and says so.
Nothing else is ever written to the file. A table or block can be anchored by hand with
`<!-- agtrack:block lifetimes -->`.

`spec sync` reconciles a new revision deterministically: pins first, then identical path,
then identical heading elsewhere (moved), then body similarity (renamed, with a
confidence), else new. A pinned section that disappears is **orphaned, never deleted**, and
keeps its notes and links; splits and merges are never guessed (`spec relink`).

### Reviewing a spec in the browser

```sh
agtrack spec serve auth            # prints http://127.0.0.1:<port>/s/auth?t=<token>
```

Select text to comment on it; resolve threads; **Send to agent** releases the accumulated
comments (the default `manual` mode — `agtrack config set review.release auto` releases each
comment immediately); **Approve / Ready to build** records an approval for the current
revision plus optional build notes.

The viewer binds `127.0.0.1` only, requires the capability token in the link, validates the
`Host` header, sends no CORS headers, renders markdown without raw HTML and Mermaid at
`securityLevel: 'strict'` from an embedded, pinned copy, and exits after 30 idle minutes.
Every browser action goes through the same store, transactions and attribution as the CLI.

### Browsing and editing everything: `agtrack serve`

```sh
agtrack serve                      # prints http://127.0.0.1:<port>/?t=<token>
agtrack serve -c work --as alex --idle 0 --open
agtrack serve --restart            # replace the running server (e.g. after rebuilding)
```

One server per database: a second `agtrack serve` (same `--as`) prints the running
server's link instead of starting another, and `agtrack spec serve <spec>` links to the spec
on it. The running server is recorded in `<db>.serve.json`, which is checked (process alive
and answering with its token) before use and deleted when stale. `--new` opts out.

A local web app over the whole store: contexts, the project tree, tasks with their steps,
dependencies, claims, notes, artifacts and PRs, open notes across a scope, and the worklog.
Every spec opens in the spec viewer above, with the full review flow. From the browser a
human can create tasks, retitle and tag them, change task and step status, release a claim
(after a confirm), add and remove dependencies, add steps, write notes of any kind, reply,
resolve and reopen them, and add worklog entries. There are no deletes, and claims are never
taken from the browser.

Edits run the CLI's own verbs, so they are ordinary events attributed to `--as` (default
`human`, which must differ from the serving actor). An agent sees them with
`agtrack watch -p <project> --actor-not self`, and in `resume` the next time it starts;
spec comments reach `spec watch|await` exactly as under `spec serve`. Agents never need the
URL. The same security rules as `spec serve` apply; the idle timeout defaults to 2h
(`--idle 0` = never).

### Waiting for a human (or another agent)

| Harness | Use |
|---|---|
| Background notifications (Claude Code) | `agtrack spec watch auth --timeout 2h` in the background: one line per comment/approval, a `TERMINAL` line at the end. For agent-to-agent signalling: `agtrack watch -p . --actor-not self --timeout 1h` |
| Background tasks, one notification wanted | `agtrack spec await auth --until approval --timeout 2h` in the background |
| No background tasks (e.g. one-shot `codex exec`) | `agtrack spec await ...` in the foreground; the turn ends waiting |
| Nothing waiting | the comments and the approval are rows in the database; `agtrack resume` shows them on the next action |

Background notification is a harness capability, not a guarantee. The database row is the
signal; the watch is only a latency optimisation. Wait exit codes: 0 approved, 10 comments,
11 timeout, 12 spec changed during the wait, 13 channel lost.

## Output

`-f table` (default), `plain`, `tsv`, `json`, `jsonl` (one object per line), `yaml`.
`--fields a,b` picks columns (and keys of a `show`), `-x` widens, `-q` prints identifiers
only, `--count` prints the number of matches before `--limit`, `--max-bytes N` bounds output
and lists anything it left out with the command that retrieves it.

Structured output is lean: display duplicates (`created`, `mark`, `path`, ...) are left out
of `json`/`yaml`/`jsonl`, and a task's event history appears only with `-x`. Every command,
including mutations and failures, honours the format; under `json`/`yaml`/`jsonl` an error
is one object on **stderr**:

```console
$ agtrack -f json task show 999
{"error":"not_found","code":3,"message":"no such task: 999","entity":"task","ref":"999"}
```

## Exit codes

| Code | Meaning |
|---|---|
| 0 | success (also when stdout is closed early, as in `agtrack log \| head`) |
| 1 | error |
| 2 | usage error |
| 3 | not found |
| 4 | conflict (includes a live claim held by someone else, a reused `--idem` key) |
| 5 | database unreachable or broken |
| 10 / 11 / 12 / 13 | `spec await`/`watch`: comments / timeout / spec changed / channel lost |
| 130 | interrupted |

## Changes from agtrack 1.x

agtrack 2 is a rewrite of the Python 1.0 build in Go. Its acceptance test is a golden
corpus captured from the Python build (`testdata/golden/`); every intended difference is
recorded against a defect number in `testdata/golden/deltas.json`.

Fixed:

- `ctx use` no longer leaves the project pin pointing into another context, which let an
  unqualified `task add` land in the wrong context with exit 0. Pins are per actor.
- `--count` is the number of matches, not the page size.
- Errors, mutations and every overview honour `-f json`/`yaml`; errors are structured.
- Structured output drops derived duplicates; `task show -f json` no longer embeds the
  event history unless `-x`.
- Every mutation writes its row and its worklog event in one transaction; a multi-part
  command (`task add --step ...`) is all or nothing.
- A repeated status transition is a no-op, including its `--note`.
- Notes can be resolved, reopened, replied to and removed.
- Repository links keep their host (`github.com/x/y` and `gitlab.com/x/y` no longer collide).
- `dump` is documented as a human-readable export; it is not a backup format — copy the
  database file (and its `-wal`) instead.

Deliberately different, not bugs:

- `--help` text is laid out differently (per-verb help no longer repeats the global flags).
- `yaml` output is emitted by a port of the old emitter but is not guaranteed byte-identical.
- Long flags still accept unique prefixes (`--proj` for `--project`), as argparse did.

## Development

```sh
go test ./...                                   # unit, contract and golden tests
go test ./internal/golden -update               # accept Go output for steps listed in deltas.json
python3 scripts/golden/capture.py               # recapture the corpus from the Python 1.0 build
go test -tags devui -run TestDevServer -timeout 0 ./internal/web   # the viewer against fixtures
```

Tests always use temporary databases. `AGTRACK_NOW=2026-09-01T12:00:00Z` freezes agtrack's
clock; it exists for the golden corpus and tests. It changes every timestamp agtrack
writes and moves the "refuse future `--at`" guard with it, so never set it in normal use —
to record past work, use `--at`/`--created`/`--started`/`--closed`. Layout: `cmd/agtrack` (entry), `internal/cli` (command
tree, handlers), `internal/cli/argp` (argparse-compatible parser), `internal/store` (every
read and write), `internal/db` (connection, schema, migrations), `internal/refs` (reference
grammar, time), `internal/render` (output formats), `internal/spec` (markdown sections,
pins, reconciliation), `internal/web` (viewer). The viewer's HTTP contract is
`docs/viewer-api.md`.
