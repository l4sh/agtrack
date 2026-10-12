package store

import (
	"sort"
	"strings"

	"github.com/l4sh/agtrack/internal/row"
)

// UsageFamily maps an event (target type + action) to a command family, the
// answer to "which part of agtrack was used" (G2). The mapping, first match:
//
//	action claim, release, transfer          -> claim
//	action checkpoint                        -> checkpoint
//	action snooze, unsnooze                  -> snooze
//	action artifact, artifact-removed        -> artifact
//	action dep-added, dep-removed            -> dep
//	action pr, pr-state, pr-removed          -> pr
//	action spec-*                            -> spec
//	target task / subtask                    -> task / sub
//	target rule / lesson / context           -> rules / lessons / ctx
//	any other target (note, pr, spec, actor, project, tag) -> the target type
//	no target type                           -> other
func UsageFamily(targetType, action string) string {
	switch action {
	case "claim", "release", "transfer":
		return "claim"
	case "checkpoint":
		return "checkpoint"
	case "snooze", "unsnooze":
		return "snooze"
	case "artifact", "artifact-removed":
		return "artifact"
	case "dep-added", "dep-removed":
		return "dep"
	case "pr", "pr-state", "pr-removed":
		return "pr"
	}
	if strings.HasPrefix(action, "spec-") {
		return "spec"
	}
	switch targetType {
	case "subtask":
		return "sub"
	case "rule":
		return "rules"
	case "lesson":
		return "lessons"
	case "context":
		return "ctx"
	case "":
		return "other"
	}
	return targetType
}

// Usage counts events since a UTC ISO time ("" = all): per actor, per command
// family, and the events with no actor. Both lists are busiest first.
func (s *Store) Usage(since string) (res *row.Row, err error) {
	defer guard(&err)
	where, args := "1=1", []any{}
	if since != "" {
		where, args = "at >= ?", []any{since}
	}
	byActor, byFamily := map[string]int64{}, map[string]int64{}
	var total, noActor int64
	for _, r := range s.q(`SELECT target_type, action, COALESCE(actor, '') AS actor, COUNT(*) AS n FROM events WHERE `+where+
		` GROUP BY target_type, action, COALESCE(actor, '')`, args...) {
		n := r.Int("n")
		total += n
		byFamily[UsageFamily(r.Str("target_type"), r.Str("action"))] += n
		if a := r.Str("actor"); a == "" {
			noActor += n
		} else {
			byActor[a] += n
		}
	}
	list := func(m map[string]int64, key string) []*row.Row {
		out := []*row.Row{}
		for k, n := range m {
			out = append(out, row.Of(key, k, "events", n))
		}
		sort.Slice(out, func(i, j int) bool {
			if a, b := out[i].Int("events"), out[j].Int("events"); a != b {
				return a > b
			}
			return out[i].Str(key) < out[j].Str(key)
		})
		return out
	}
	var sinceV any
	if since != "" {
		sinceV = since
	}
	return row.Of("since", sinceV, "events", total, "no_actor", noActor,
		"actors", list(byActor, "actor"), "families", list(byFamily, "family")), nil
}
