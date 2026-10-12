package store

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// DefaultBaseBranches are the branch names every repo treats as a base
// (agent-features spec F1); the repo's own origin/HEAD is added on top.
var DefaultBaseBranches = []string{"main", "master", "develop", "dev", "stage", "staging", "trunk"}

// GitQuick runs git in dir ("" = the cwd) with a short timeout and returns
// its trimmed stdout. ok is false on any failure, including a timeout: git
// trouble must never fail an agtrack command.
func GitQuick(dir string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// BaseBranches is the base-branch list for the repository at dir: the
// defaults plus the branch origin/HEAD points at, when it is set.
func BaseBranches(dir string) []string {
	out := append([]string{}, DefaultBaseBranches...)
	if ref, ok := GitQuick(dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); ok {
		name := strings.TrimPrefix(ref, "origin/")
		if name != "" {
			for _, b := range out {
				if b == name {
					return out
				}
			}
			out = append(out, name)
		}
	}
	return out
}

// IsBaseBranch reports whether branch is one of BaseBranches(dir).
func IsBaseBranch(dir, branch string) bool {
	for _, b := range BaseBranches(dir) {
		if b == branch {
			return true
		}
	}
	return false
}
