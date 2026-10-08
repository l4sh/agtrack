package refs

import (
	"strconv"
	"strings"
	"time"

	"github.com/l4sh/agtrack/internal/errs"
)

// ParseUntil turns a future bound into a UTC ISO timestamp: a duration from
// now (3d, 12h, 2w) or a date/time (2026-10-20, 2026-10-20T09:00). It must
// lie in the future.
func ParseUntil(value string) (string, error) {
	raw := strings.ToLower(strings.TrimSpace(value))
	var t time.Time
	if m := durationRE.FindStringSubmatch(raw); m != nil {
		n, _ := strconv.Atoi(m[1])
		t = Now().Add(durationFor(m[2], n))
	} else {
		parsed, ok := parseISO(strings.ReplaceAll(strings.TrimSpace(value), "Z", "+00:00"))
		if !ok {
			return "", errs.New("cannot parse --until: %s (use 3d, 12h, 2w or a date like 2026-10-20)", value)
		}
		t = parsed
	}
	if !t.After(Now()) {
		return "", errs.New("--until must be in the future: %s", value)
	}
	return ToUTCISO(t), nil
}
