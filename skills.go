// Package agtrack embeds the agent skill shipped with the binary, so
// `agtrack skill install` always installs the skill matching this build.
package agtrack

import "embed"

// Skills holds skills/<name>/SKILL.md and its reference files.
//
//go:embed skills
var Skills embed.FS
