agtrack 2.0 — the Go rewrite of agtrack 1.0 (Python).

- One static binary for linux and macOS (amd64, arm64); no runtime to install.
- Opens a 1.x database in place and migrates it to schema v3 (one-way; copy the file first
  if you still need the Python build).
- New: `resume`, claims, task dependencies, typed and resolvable notes, idempotent creates
  (`--idem`), `-f jsonl`, `--max-bytes`, structured errors, `grammar`/`describe`, `watch`,
  and the `spec` entity with a local browser review loop (`spec serve`, `spec watch`,
  `spec await`).
- Fixed: the stale project pin that could misfile a task into another context, `--count`
  with `--limit`, non-atomic mutations, format-blind errors and overviews, and more — see
  "Changes from agtrack 1.x" in the README.
- Deliberately different: `--help` layout, and `yaml` output is not guaranteed
  byte-identical to 1.x.
