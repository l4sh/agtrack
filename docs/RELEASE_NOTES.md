agtrack 2.0 — the Go rewrite of agtrack 1.0 (Python).

- One static binary for linux and macOS (amd64, arm64); no runtime to install.
- Opens a 1.x database in place and migrates it to schema v4 (one-way). Before migrating
  it writes a `VACUUM INTO` copy next to the database (`<db>.v2.bak` or `<db>.v3.bak`).
- **Upgrading from an earlier 2.0.0-dev build (schema v3): switch every agent at once.** The
  first v4 binary that opens the shared database migrates it, and from then on older builds
  refuse it ("schema version 4 is newer than this agtrack"). Replace the `agtrack` on `PATH`
  and run `agtrack skill install` in the same minute; the `.v3.bak` copy is the way back.
- New: `resume`, claims, task dependencies, typed and resolvable notes, idempotent creates
  (`--idem`), `-f jsonl`, `--max-bytes`, structured errors, `grammar`/`describe`, `watch`,
  and the `spec` entity with a local browser review loop (`spec serve`, `spec watch`,
  `spec await`).
- Agents: sessions on every write, claim liveness (`claims`, `watch --until stale`),
  checkpoints, delta/sectioned/stable `resume`, `sub done --close-parent`, `sub next`, stale
  tasks in `next`, `task done-check`, evidence on done, `task snooze`, `note ack`,
  `task transfer`, an actor registry, `batch`, automatic branch/commit artifacts, `pr sync`,
  `hooks print`, `doctor`, `info --usage`, `project set --move` (across contexts, with
  subprojects), standing rules with `check` (exit 20/21/22) and
  shared lessons.
- `agtrack serve`: the whole store in a browser — browse and edit tasks, steps and notes,
  approve rules, verify lessons — with one server per database, reused by `spec serve`.
- Fixed: the stale project pin that could misfile a task into another context, `--count`
  with `--limit`, non-atomic mutations, format-blind errors and overviews, and more — see
  "Changes from agtrack 1.x" in the README.
- Deliberately different: `--help` layout, and `yaml` output is not guaranteed
  byte-identical to 1.x.
