package cli

import "github.com/l4sh/agtrack/internal/cli/argp"

const batchEpilog = `Input is JSON lines, one op each: {"argv": ["sub","done","12.3"]} or
{"op": "sub done", "args": {"ref": "12.3", "note": "ok"}}. In the op form,
args keys are positional names or flag names as describe lists them; bool
flags take true, append flags a list. Ops run in-process in one transaction
with the batch's own --actor, --session and -c; an op cannot set --db or
those. --idem inside an op works as on the CLI. watch, await, serve,
spec serve|watch|await and batch itself are refused.

One JSON line per op on stdout: {"ok":true,"op":N,"result":...} or
{"ok":false,"op":N,"error":...,"code":...,"message":...}. Without --continue
the first failure rolls back every op (earlier lines carry "rolled_back":true)
and the exit code is that failure's. With --continue each failing op is
undone alone, the rest commit, and the exit code is the first failure's.`

var batchOutput = []argp.OutputKey{
	{Key: "ok", Help: "true for a result line, false for an error line (one line per op, in order)"},
	{Key: "op", Help: "1-based input line number of the op"},
	{Key: "result", Help: "the op's -f json output (null when it prints nothing)"},
	{Key: "error", Help: "error token on a failed op; with code, message, entity, ref as for any structured error"},
	{Key: "rolled_back", Help: "true on an ok line whose effects were undone by a later failure"},
}
