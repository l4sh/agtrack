package cli

import (
	"strings"

	"github.com/l4sh/agtrack/internal/row"
)

// Session-aware claims (agent-features B1). A claim is "mine" when this
// actor holds it live from this session. A claim without a session (a
// transfer, or a pre-v4 claim) or an invocation without one cannot be told
// apart, so it counts as mine, as it did before sessions existed.

func claimMine(c *Ctx, t *row.Row) bool {
	if c.Actor == "" || !claimLive(t) || t.Str("claimed_by") != c.Actor {
		return false
	}
	cs := t.Str("claim_session")
	return cs == "" || c.Session == "" || cs == c.Session
}

// claimOtherSession returns the session of a live claim this actor holds
// from another session, or "".
func claimOtherSession(c *Ctx, t *row.Row) string {
	if c.Actor == "" || !claimLive(t) || t.Str("claimed_by") != c.Actor {
		return ""
	}
	if cs := t.Str("claim_session"); cs != "" && c.Session != "" && cs != c.Session {
		return cs
	}
	return ""
}

func shortSession(s string) string {
	if r := []rune(s); len(r) > 8 {
		return string(r[:8]) + "…"
	}
	return s
}

// claimLineFor is claimLine with the other-session marker.
func claimLineFor(c *Ctx, t *row.Row) string {
	line := claimLine(t)
	if other := claimOtherSession(c, t); other != "" {
		holder := t.Str("claimed_by")
		line = holder + " (other session " + shortSession(other) + ")" + strings.TrimPrefix(line, holder)
	}
	return line
}

// preferMine moves the tasks claimed by this actor and session to the front,
// keeping the order otherwise.
func preferMine(c *Ctx, tasks []*row.Row) []*row.Row {
	var mine, rest []*row.Row
	for _, t := range tasks {
		if claimMine(c, t) {
			mine = append(mine, t)
		} else {
			rest = append(rest, t)
		}
	}
	return append(mine, rest...)
}

// markClaim sets claim_mine on a live-claimed listing row (additive key).
func markClaim(c *Ctx, item *row.Row) {
	if claimLive(item) {
		item.Set("claim_mine", claimMine(c, item))
	}
}
