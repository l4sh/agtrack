---
name: agtrack
description: Track work in agtrack, the task tracker shared by every agent on the machine - resume where work stands, claim and run tasks with step lists, leave handoffs and worklog entries, and review markdown specs with a human through the local browser viewer (spec serve / watch / await). Use at the start of any coding work, when picking up a task, when another agent or a human needs to know what happened, and whenever a spec is reviewed, linked to tasks or checked for drift.
compatibility: Requires the agtrack 2.x binary on PATH (agtrack --version). Specs, resume, claims and --idem do not exist in 1.x.
---

# agtrack

One SQLite store, shared by every agent and every checkout on this machine, holds the
**step list** (tasks and subtasks), the **worklog** (what happened, when, by whom) and
**specs** (markdown documents addressed by section, annotated, reviewed and linked to the
tasks that implement them).

Every piece of work goes in it, from a one-line fix to a multi-session feature. Size sets
the granularity - a trivial fix is one task with no steps - never whether you track it.

Three facts shape everything below:

- **Other agents write at the same time.** Writes are atomic; never cache state in your
  head and write it back wholesale. Claims and `--actor-not self` exist because of this.
- **Read narrowly.** Every listing takes `--fields`, `--ids`, `--count`, `-f json|jsonl`,
  `--max-bytes`. Pulling a full table when you needed one id is waste.
- **The database row is the signal.** Anything you miss live (a comment, an approval, a
  handoff) is still there; `agtrack resume` shows it next time.

## 1. Before anything else

```bash
agtrack --version            # must be 2.x
agtrack info                 # database path, schema, your actor, current context/project
```

**Set your actor** before writing: `export AGTRACK_ACTOR=<model id>` (lowercase,
hyphenated, the concrete model: `claude-opus-5-5`, `gpt-6-astra`, `gemini-3-pro`; not the
harness, not a role). It is stamped on every write, it scopes your project pin to you, and
it is what `--actor-not self` filters on. Do not also tag items with your name.

| `agtrack info` says | Meaning | Fix |
|---|---|---|
| `command not found` | not installed | install the release binary (see the repo README), then `agtrack skill install` |
| exit 5, database error | cannot reach the database | sandboxed agents cannot write `~/.local/share/agtrack` (WAL needs the directory); pass `--db <path inside your workspace>` and say so |
| `newer than this agtrack` | old binary | upgrade; never downgrade the database |
| `agtrack skill status` says outdated | skill older than the binary | `agtrack skill install` |

**Testing a development build of agtrack itself:** put `--db <scratch path>` on every
command line. Do not rely on `export AGTRACK_DB=...`: in `export X=... && (...) &` the
export is backgrounded with the subshell, the foreground falls back to the shared
database, and a dev build may migrate it under every other agent. Brief subagents the
same way, in those words.

## 2. Orient: resume, don't reconstruct

```bash
agtrack resume                     # project, your task, steps, open notes, spec drift, blockers, claims, recent log
agtrack resume -p acme/shop        # when the cwd repo does not resolve
agtrack grammar                    # one-screen reference: refs, statuses, formats, exit codes
```

`resume` answers in one call what used to take five (`here`, `project use`, `next`,
`task show`, `log`). It is bounded (~500 bytes); anything cut is listed under `omitted`
with the command that retrieves it - an omission is never "nothing there".
`resume --complete` drops the caps.

Project resolution, in order: `-p` → your pinned project (per actor) → the project linked
to the cwd's git `origin` → **error**. Unqualified writes never guess; `-c <context>` is
honoured over a pin from elsewhere. `-p .` fails when two projects link the same remote
- use the explicit ref.

**One thing at a time.** If `resume` shows a task in progress, continue it or ask the
human which to drop; do not start a second.

## 3. The work loop

```bash
agtrack next --claim                                   # take the next doable task: skips claimed, blocked, closed
id=$(agtrack task add "Implement OAuth login" -p acme/shop:web \
      --step "provider config" --step "callback route" --step "tests" \
      --tag auth --idem "oauth-$(date +%F)" -q)        # -q prints the bare id
agtrack task claim "$id" --ttl 2h
agtrack sub start "$id".1 ; agtrack sub done "$id".1  # move the marker as you go, not at the end
agtrack task note "$id" --kind handoff "provider secret is in the vault as oauth-app; route half done"
agtrack log add -t "$id" "Blocked on provider credentials; asked the human"
agtrack task done "$id" --note "merged behind a flag"   # releases the claim
```

- **Break the work into steps before writing code.** `sub add 12 "a" "b" "c"` adds many.
- Starting or finishing a step pulls a `todo` task into `in-progress`. Finishing every
  step does **not** close the task - that is your explicit call.
- **Status verbs are idempotent**: repeating `task done 12 --note x` writes nothing, not
  even a second note. **Multi-ref verbs are atomic**: `task done 4 999` writes nothing if
  999 is bad.
- **Retries:** `--idem KEY` on any create (`ctx/project/task/sub/log/pr add`,
  `<noun> note`, `note reply`, `task artifact`). A retry with the same key reprints the
  first result and writes nothing; the same key with different input is refused
  (`request_key_reused`). Other verbs reject `--idem`.
- **Claims are advisory**: they expire, never block a write, and tell `resume`, `next`,
  `task ls` who is on what. `--steal` takes over a live claim; ask first if it is not yours
  to take. An expired holder may still be editing files - a claim cannot fence a worktree.
- **Dependencies:** `task block 14 --on 12`; `next` and `task ls --ready` skip blocked work.
  Blocked-ness is not a status.
- **Notes have kinds**: `note`, `decision`, `handoff`, `blocker`, `question`.
  `agtrack notes -p .` lists every unresolved one; `note resolve 7 --note why`,
  `note reopen`, `note reply`, `note rm`.
- **Pick up a handoff by acknowledging it**: after `resume`, `note ack 7 [--note "on it"]`
  on each handoff, blocker or question you take on. It is idempotent and not a resolve
  (the final close stays `note resolve`). `notes --unacked` lists the ones nobody has taken.
- **Hand off and finish in one write**: `task note 12 --kind handoff --close 12.3 "..."`
  (a step of that task, or the task itself). To pass a whole task on:
  `task transfer 12 --to codex --note "why"` releases your claim, claims it for them and
  leaves a handoff for them to `ack`, atomically.
- **Who is still alive**: `claims [-p .] [--stale 15m]` shows holder, session, age,
  last_seen (refreshed by every write of the holder) and last event. An orchestrator can
  sleep on `watch --until stale --stale 15m` (exit 14, `TERMINAL stale`).
- **Artifacts** record where the work lives: `task artifact 12 branch feat/oauth`,
  `task artifact 12 file auth/tokens.ts --label "grace window"`. Store content that
  resumption needs, not paths under `/tmp`.

What goes where: a status change records *that* something moved; a note or log entry
records *why*, or what the next agent needs and cannot infer. Log at milestones and
blockers, not per file edit. Do not restate the store in chat - point at it.

## 4. Waiting on another agent or a human

```bash
agtrack watch -p . --actor-not self --timeout 1h          # one line per event; TERMINAL line at the end
agtrack spec await <spec> --until approval --timeout 2h   # one result, then exit
```

- **Always `--actor-not self`**, or your own writes wake you (and two agents watching each
  other livelock). **Always a timeout**; a hang needs a human to notice.
- Pick the shape your harness supports: if it turns **each line of a background command
  into a notification** (Claude Code: the Monitor tool, not a plain background shell), use
  `watch`; if it notifies only when a background command **exits**, use `await`; if it has
  no background tasks, run `await` in the foreground. Nothing waiting at all is fine too -
  `resume` shows the signal later.
- Re-arm after every timeout with `--after <cursor>` from the `TERMINAL ... cursor=N` line,
  so nothing replays or is skipped. Order is by insertion id, never by time.

## 5. Specs

A spec is a markdown file in a repo. The **file owns the prose**; agtrack owns sections,
annotations and task links; pins are the only thing written back, and only when something
durable needs one.

```bash
agtrack spec add docs/auth.md -p acme/shop --slug auth   # v1; the file is not modified
agtrack spec toc auth                                     # sections, open notes, linked task ids, pins
agtrack spec show auth#tokens__rotation                   # one section's body, nothing else
agtrack spec find auth "grace window"                     # full-text search over the head revision
agtrack spec check auth#tokens__rotation                  # open notes, drift, linked tasks - run before implementing
agtrack task add "Per-client grace window" -p acme/shop --spec auth#tokens__rotation
agtrack spec stale auth                                   # tasks whose sections changed since they were linked
```

Sections are addressed by **path slug** from the heading tree (`## Tokens` → `### Rotation`
is `tokens__rotation`); duplicate headings under different parents get different slugs,
and leading numbers are dropped. The first note or task link on a section **pins** it
with one `<!-- agtrack:id key -->` line above the heading, so the address survives renames.

**Reviewing a spec with a human** (serve, comments, replies, approval) has its own
procedure and several traps: read [references/spec-review.md](references/spec-review.md)
before running `spec serve`.

## 6. Recording work that already happened

Migrating a markdown checklist, logging yesterday, or picking up work tracked in someone's
head: pass the real time (`--created/--started/--closed` on `task add`, `--at` everywhere
else) and never invent precision. Read
[references/backfill.md](references/backfill.md) before backfilling.

## 7. Reading without drowning

| You want | Command |
|---|---|
| ids only | `agtrack task ls -s open --ids` (or `-q`) |
| a total | `--count` (matches before `--limit`) |
| two columns | `agtrack task ls --fields id,title -f tsv` |
| machine readable | `-f json` (lean, no display duplicates) or `-f jsonl` for listings |
| a bounded answer | `--max-bytes 2000` (omissions are reported, never silent) |
| one task, with history | `agtrack task show 12 -x` |
| a command's flags as JSON | `agtrack describe task add` |

Defaults that bite: `task ls` shows open work only (`-s all`); `-p proj` includes
subprojects (`--direct` to exclude); `log` shows the current context's last 50 events.

Errors under `-f json|yaml|jsonl` are one JSON object on **stderr**
(`{"error","code","message","ref",...}`). Branch on the exit code, not on prose:

| Code | Meaning |
|---|---|
| 0 | success (an empty result is success) |
| 1 | invalid input (bad ref, future timestamp, ...) |
| 2 | usage: the command line was rejected |
| 3 | not found |
| 4 | conflict: exists already, children block a delete, live claim, reused `--idem` key |
| 5 | database unreachable or busy (busy: retry; do not switch databases) |
| 10 / 11 / 12 / 13 | `await`/`watch`: comments / timeout / spec changed / channel lost |
| 14 | `watch --until stale`: a watched claim went quiet |

## 8. Rules

- **Never edit the database directly** - no `sqlite3` writes, no file surgery, no schema
  changes or downgrades. If the database looks wrong, stop and tell the human.
- **Keep the record in agtrack**, not in a markdown checklist beside it.
- **Cancel rather than delete.** Deletes with children are refused (exit 4); if you are
  reaching for `--force`, ask the human first. Never delete other agents' work.
- **Do not resolve a human's notes for them.** Reply; they resolve.
- **One task, one unit of work.** Split rather than widening the title.
- **Use invented examples in anything you publish** (READMEs, tests, fixtures): never real
  project, organisation or person names from the database you are working in.
