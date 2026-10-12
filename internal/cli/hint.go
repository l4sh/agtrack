package cli

import "github.com/l4sh/agtrack/internal/row"

// Hints (agent-features spec E3). A mutation that has an obvious next action
// calls c.Hint("agtrack task done 12") before it emits. Structured output then
// carries it as "hint"; text output ends with a "→ …" line. One hint per
// invocation: the first one set wins, so the most specific caller goes first.

// Hint records the next action for this invocation (first call wins).
func (c *Ctx) Hint(cmd string) {
	if c.hint == "" {
		c.hint = cmd
	}
}

// applyHint writes the hint into a structured payload: a row, or the last row
// of a result list.
func (c *Ctx) applyHint(obj any) {
	if c.hint == "" {
		return
	}
	switch v := obj.(type) {
	case *row.Row:
		v.Set("hint", c.hint)
	case []*row.Row:
		if len(v) > 0 {
			v[len(v)-1].Set("hint", c.hint)
		}
	}
}

// FlushHint prints the text hint line. Run calls it after a handler succeeds;
// it is a no-op under a structured format (the hint is in the payload) or -q.
func (c *Ctx) FlushHint() {
	if c.hint == "" || c.structured() || c.Quiet {
		return
	}
	c.printf("→ %s\n", c.hint)
	c.hint = ""
}
