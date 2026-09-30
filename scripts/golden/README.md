# The golden parity corpus

This is the acceptance test for the Go rewrite (go-rewrite-spec.md §4): capture
what the Python build actually does today, then hold the Go binary to it.

```
scripts/golden/pyagtrack     wrapper that runs the real Python CLI with a frozen clock
scripts/golden/capture.py    replays scenarios.py through pyagtrack, writes the corpus
scripts/golden/scenarios.py  the corpus, as data: every scenario, every step
testdata/golden/*.json       the captured corpus (one file per scenario)
testdata/golden/deltas.json  the only steps -update is allowed to touch, keyed by defect
internal/golden/golden_test.go   replays the corpus against ./cmd/agtrack
```

## Recapturing

Only needed if scenarios.py changes, or to re-baseline after a deliberate Python
fix (D1 in go-rewrite-spec.md §5 is a live bug and was meant to be fixed in the
Python build directly, independent of this rewrite).

```
python3 scripts/golden/capture.py                    # every scenario
python3 scripts/golden/capture.py ctx_lifecycle d1_stale_pin_bug   # just these
```

Stdlib only, no venv needed. It shells out to
`<agtrack-1.x>/agtracklib` through `pyagtrack`, which
freezes `datetime.datetime` before importing it (see the module docstring in
`pyagtrack` for exactly what that patches and why) so that two runs produce
byte-identical output. The only field that legitimately differs run to run is
`info`'s database path, which is why those two steps carry
`"normalize": ["dbpath"]` instead of being asserted byte-for-byte.

Verify a re-capture didn't drift by diffing against git:

```
python3 scripts/golden/capture.py && git diff --stat testdata/golden/
```

An empty diff (or a diff touching only the two `info` steps) means nothing
changed.

## Replaying against the Go build

```
go test ./internal/golden/...                        # everything
go test ./internal/golden/... -scenario task_lifecycle -v
```

Skips cleanly (not a failure) until `cmd/agtrack/main.go` exists. Once it
does, the test builds it once with `go build`, then replays every scenario in
its own fresh database, comparing exit code, stdout and stderr per
go-rewrite-spec.md §4.2:

- exit code: always exact.
- `-f json`/`tsv`/`plain`/`yaml`: exact, byte for byte.
- `-f table`: exact after collapsing runs of spaces per line (column padding
  is not parity-bound; column set and order still are, because a real
  difference there is not whitespace-only and survives the collapse).
- exit-2 usage errors: only the final `prog: error: ...` line is compared;
  argparse's `usage: ...` banner is not parity-bound (a different flag
  library will not reproduce it, and isn't expected to).
- `--help`/`-h` steps are skipped outright (none are currently captured, but
  the runner accounts for them since a future scenario might add one).

The comparison logic doesn't try to sniff the format out of `argv` (prefix
matching and global-flag placement make that fragile); it just tries an exact
match first and falls back to the whitespace-collapsed one. That fallback
only saves a step from failing when the *only* difference is whitespace
width, so it can't accidentally paper over a real content mismatch.

## When the Go build deliberately diverges

go-rewrite-spec.md §5 lists defects (D1-D15) the Go build must **not**
reproduce. Where this corpus captured one of those defects on purpose, the
step is tagged `known-defect-D<n>` (see `scripts/golden/scenarios.py`'s `D()`
helper) so a human -- or the D1 fix, once it lands in Python and this corpus
is re-captured -- can find every affected step by grep.

When the Go build fixes one of these (or makes any other deliberate,
justified change), update the golden file for exactly those steps:

1. Add an entry to `testdata/golden/deltas.json`: `"<scenario>#<step index>":
   "D12: lean json drops derived keys"`. Steps not listed here are never
   touched by `-update`, on purpose -- an unrelated regression should still
   fail the test.
2. `go test ./internal/golden/... -update`
3. Diff `testdata/golden/` and confirm only the listed steps changed.

`-update` rewrites just `stdout`/`stderr`/`exit` on the listed steps, through
a generic JSON round-trip, so `argv`/`cwd`/`now`/`env`/`normalize`/`tags`
survive untouched.

## Known gaps, deliberately

- **`pr state <non-numeric-id>` and `pr rm <non-numeric-id>` are not
  captured.** `commands.pr_state`/`pr_rm` call `int(ns.id)` unguarded; a
  non-numeric id crashes with an uncaught `ValueError` and a raw Python
  traceback (exit 1, incidentally, since that's Python's default for an
  uncaught exception). That traceback embeds this machine's absolute paths
  and is not something a Go rewrite would or should reproduce, so it isn't
  useful golden material. Always pass numeric ids to these two verbs.
- **`ctx set --rename` crashes in the current Python build**, discovered
  while writing this corpus -- not one of go-rewrite-spec.md §5's D1-D15.
  `commands.context_set` calls `store.update_context(ns.name, **fields)`
  where `fields` can contain a `"name"` key (from `--rename`), and
  `update_context`'s first positional parameter is itself named `name`:
  `TypeError: got multiple values for argument 'name'`, uncaught, exit 1.
  (Project- and task-level rename don't have this bug: their `update_*`
  methods take the row as a dict, not `name` as a bound parameter.) One step
  in `ctx_failures.json` records this crash verbatim for the spec authors,
  tagged `skip-replay` so `golden_test.go` never asserts on it -- the Go
  build should simply implement rename correctly, not reproduce a Python
  stack trace. Flag this to whoever owns go-rewrite-spec.md #5 so it can be
  added to the defect list; it is not this brief's place to edit that
  read-only doc.
- **`-f jsonl`** doesn't exist in the current build (`render.FORMATS` is
  `table, plain, tsv, json, yaml`); it's new surface from
  go-rewrite-spec.md §5/D12, not part of this parity corpus.
