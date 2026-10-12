package cli

import (
	"fmt"
	"strings"

	"github.com/l4sh/agtrack/internal/cli/argp"
	"github.com/l4sh/agtrack/internal/row"
)

// Harness hook snippets (agent-features spec F3). `hooks print` only prints:
// installing hooks edits user-level configuration, which the human does.

func hooksCommand() *argp.Command {
	return &argp.Command{Name: "hooks", Help: "ready-to-paste harness hook configuration (prints only; never installs)", SubRequired: true,
		Subs: []*argp.Command{
			{Name: "print", Help: "print hook configuration and scripts for claude or codex", Run: Handler(hooksPrint),
				Args:  []argp.Arg{{Name: "harness", Choices: []string{"claude", "codex"}}},
				Flags: []argp.Flag{{Long: "--event", Choices: []string{"session-start", "session-end", "pre-commit"}, Help: "only this event (default: all three)"}}},
		}}
}

var hookEvents = []string{"session-start", "session-end", "pre-commit"}

// hookHarness holds what differs between the two harnesses.
type hookHarness struct {
	name, dir, settings string
	askCapable          bool // PreToolUse can answer "ask the user"
}

var hookHarnesses = map[string]hookHarness{
	"claude": {name: "Claude Code", dir: "$HOME/.claude/hooks", settings: "~/.claude/settings.json", askCapable: true},
	"codex":  {name: "Codex", dir: "$HOME/.codex/hooks", settings: "~/.codex/hooks.json"},
}

type hookPart struct {
	Kind string // "config" or "script"
	Path string // script path, or the settings file
	Text string
}

// hookParts builds the pieces for one event.
func hookParts(h hookHarness, event string) (title string, parts []hookPart) {
	switch event {
	case "session-start":
		return "session start: show where the work stands", []hookPart{{"config", h.settings,
			hookJSON(`"SessionStart": [
      { "hooks": [ { "type": "command", "command": "agtrack resume 2>/dev/null || true", "timeout": 10 } ] }
    ]`)}}
	case "session-end":
		script := h.dir + "/agtrack-session-end.sh"
		return "session end: finished-looking tasks and unacknowledged handoffs", []hookPart{
			{"config", h.settings, hookJSON(`"Stop": [
      { "hooks": [ { "type": "command", "command": "` + script + `", "timeout": 15 } ] }
    ]`)},
			{"script", script, sessionEndScript}}
	default:
		script := h.dir + "/agtrack-pre-commit.sh"
		return "before git commit/push and gh pr/release: agtrack check <verb>", []hookPart{
			{"config", h.settings, hookJSON(`"PreToolUse": [
      { ` + preMatcher(h) + `"hooks": [ { "type": "command", "command": "` + script + `", "timeout": 10 } ] }
    ]`)},
			{"script", script, preCommitScript(h)}}
	}
}

// preMatcher is the tool-name matcher. Codex names its shell tool differently
// across versions, so it matches every tool and lets the script filter.
func preMatcher(h hookHarness) string {
	if h.askCapable {
		return `"matcher": "Bash", `
	}
	return ""
}

func hookJSON(entry string) string {
	return "{\n  \"hooks\": {\n    " + entry + "\n  }\n}"
}

const sessionEndScript = `#!/bin/sh
# Stop hook: before the agent ends its turn, say what is left open.
# Exit 2 sends stderr back to the agent once; stop_hook_active stops a loop.
input=$(cat)
case "$input" in *'"stop_hook_active":true'*|*'"stop_hook_active": true'*) exit 0 ;; esac
command -v agtrack >/dev/null 2>&1 || exit 0
msg=""
done_ids=$(agtrack task done-check --ids 2>/dev/null)
if [ -n "$done_ids" ]; then
  msg="$msg
Tasks that look finished or stale (agtrack task done-check):
$(agtrack task done-check 2>/dev/null)"
fi
unacked=$(agtrack notes --unacked --ids 2>/dev/null)
if [ -n "$unacked" ]; then
  msg="$msg
Notes nobody has acknowledged (agtrack notes --unacked):
$(agtrack notes --unacked 2>/dev/null)"
fi
[ -n "$msg" ] || exit 0
printf 'agtrack: before you stop:%s\n' "$msg" >&2
exit 2
`

func preCommitScript(h hookHarness) string {
	ask := `    # needs approval: hand the decision to the human
    printf '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask","permissionDecisionReason":"agtrack check %s: needs approval"}}\n' "$verb"
    exit 0 ;;`
	if !h.askCapable {
		ask = `    # needs approval: this harness cannot ask, so block and say why
    printf 'agtrack check %s: needs approval (ask the human)\n' "$verb" >&2
    exit 2 ;;`
	}
	return `#!/bin/sh
# PreToolUse hook: run agtrack check for commands that commit, push or publish.
# agtrack check exits 0 allow, 20 deny, 21 hard deny, 22 needs approval.
# Any other result (agtrack missing, database busy) lets the command through.
input=$(cat)
command -v agtrack >/dev/null 2>&1 || exit 0
verbs=""
case "$input" in *'git commit'*) verbs="$verbs commit" ;; esac
case "$input" in *'git push'*) verbs="$verbs push" ;; esac
case "$input" in *'gh pr merge'*) verbs="$verbs merge" ;; *'gh pr '*) verbs="$verbs pr" ;; esac
case "$input" in *'gh release'*) verbs="$verbs release" ;; esac
for verb in $verbs; do
  out=$(agtrack check "$verb" 2>&1)
  case $? in
    20|21)
      printf 'agtrack check %s: denied\n%s\n' "$verb" "$out" >&2
      exit 2 ;;
    22)
` + ask + `
  esac
done
exit 0
`
}

func hooksPrint(c *Ctx) error {
	h := hookHarnesses[c.Str("harness")]
	events := hookEvents
	if c.Has("event") {
		events = []string{c.Str("event")}
	}
	type block struct {
		event, title string
		parts        []hookPart
	}
	var blocks []block
	for _, ev := range events {
		t, p := hookParts(h, ev)
		blocks = append(blocks, block{ev, t, p})
	}
	if c.structured() {
		var out []*row.Row
		for _, b := range blocks {
			var ps []*row.Row
			for _, p := range b.parts {
				ps = append(ps, row.Of("kind", p.Kind, "path", p.Path, "text", p.Text))
			}
			out = append(out, row.Of("event", b.event, "title", b.title, "parts", ps))
		}
		return c.emitObject(row.Of("harness", c.Str("harness"), "events", out))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# agtrack hooks for %s. This only prints; nothing is installed.\n", h.name)
	if c.Str("harness") == "codex" {
		fmt.Fprintf(&sb, "# Codex reads %s (feature flag `hooks = true` in ~/.codex/config.toml); SessionStart is\n"+
			"# verified against a local install, Stop and PreToolUse use the same schema but were not run here.\n", h.settings)
	}
	fmt.Fprintf(&sb, "# Merge the JSON into %s (add to existing event arrays rather than replacing them),\n"+
		"# save each script, and chmod +x it. See README, \"Installing the hooks by hand\".\n", h.settings)
	for _, b := range blocks {
		fmt.Fprintf(&sb, "\n# ---- %s: %s\n", b.event, b.title)
		for _, p := range b.parts {
			if p.Kind == "config" {
				fmt.Fprintf(&sb, "# %s\n%s\n", p.Path, p.Text)
			} else {
				fmt.Fprintf(&sb, "# %s (chmod +x)\n%s", p.Path, p.Text)
			}
		}
	}
	c.printf("%s", sb.String())
	return nil
}
