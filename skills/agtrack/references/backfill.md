# Recording work that already happened

Every write stamps the current time by default. When the work is older than the record -
migrating a markdown tracker, picking up work tracked in someone's head, or logging
yesterday - pass the real time instead.

| Flag | Where |
|---|---|
| `--created WHEN` | `task add` - when the task came into existence |
| `--started WHEN` | `task add` - when work actually began |
| `--closed WHEN` | `task add` - when it was finished or dropped |
| `--at WHEN` | `sub add`, `log add`, any `note`, every status verb |

`WHEN` accepts `2026-08-14`, `2026-08-14T09:30`, `3d`, `yesterday`, `today`.

Backfilling a finished task writes the transitions it must have passed through, so the
worklog reads as if it had been tracked live:

```bash
agtrack task add "Ship the portal" -p acme/shop \
    --status done --created 2026-08-14 --started 2026-08-15T09:00 --closed 2026-08-19T17:22
agtrack log add "Paired on the callback route" -t 12 --at 2026-08-15T14:30
agtrack task note 12 "Spec was ambiguous about refresh" --at 2026-08-16
```

## Rules

- **Do not invent precision.** If the source says only a date, pass the date (it lands at
  00:00, which honestly means "sometime that day"). Inventing `09:47` fabricates a record.
- **Backfill only what you have evidence for**: a file, a commit or PR timestamp, something
  the human told you. Never a guess about when work "probably" happened.
- **Oldest first.** A record cannot predate what it belongs to: create the task with
  `--created`, then its steps, then their transitions. agtrack refuses out-of-order
  timestamps, future timestamps (5-minute grace), and `--closed` on a task that is not
  done or cancelled - exit 1, naming the floor you hit. Fix the timestamp; do not drop
  the flag to get past it.
- **The trail knows.** Each entry keeps when the work happened and when it was written;
  `agtrack log --backdated` lists reconstructed entries.
- **Never use a clock override** to backfill. `AGTRACK_NOW` exists for agtrack's own tests;
  it rewrites every timestamp and moves the future-timestamp guard.

## Migrating a markdown tracker in a repo

Checklists such as `AGTODO-<agent>.md` (one task and its steps) and `AGWORKLOG.md` (dated
entries) move into agtrack rather than being worked from both. Read them, then:

```bash
id=$(agtrack task add "<the checklist heading>" -p <project> --created <first log date> -q)
agtrack sub add "$id" "<each checkbox>" "<in file order>" --at <the same date>
agtrack sub done "$id".1 --at <the date it was ticked, only if a source gives one>
agtrack log add "<each worklog line>" -t "$id" --at <its timestamp>
```

- **Do not omit `--at` on `sub add`**, or the steps land today while everything around them
  sits in the past. One `sub add` applies one `--at`; use one call per day if needed.
- **A checkbox usually has no date.** If nothing says when a box was ticked, mark it done
  without `--at` - "done, time unknown" is honest; borrowing the task's closing date is not.
- Say in one line that you migrated them, and leave the markdown files for the human to
  delete.
