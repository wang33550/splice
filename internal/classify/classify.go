// Package classify decides whether a tool invocation is read-only (cache-hit
// semantics) or has side effects (do-not-repeat semantics).
package classify

import "strings"

type Policy int

const (
	// PolicyReadOnly: hitting cache is safe. On match, splice intercepts and
	// returns the previous result.
	PolicyReadOnly Policy = iota
	// PolicyVolatileReadOnly: no direct side effect, but the answer describes
	// external/live state, so a repeat should query again instead of reusing.
	PolicyVolatileReadOnly
	// PolicySideEffect: do not blindly re-run. On match, splice intercepts and
	// asks the model to confirm explicitly.
	PolicySideEffect
)

// readOnlyPrefixes lists Bash command prefixes whose effect on the workspace
// is conventionally read-only or idempotent. Order does not matter.
var readOnlyPrefixes = []string{
	"cat ", "ls", "pwd", "find ", "grep ", "rg ", "head ", "wc ",
	"which ", "whereis ", "type ", "file ",
	"git status", "git log", "git diff", "git show", "git branch",
	"git remote -v", "git rev-parse", "git config --get",
	"npm test", "npm run test", "npm run build", "npm run lint",
	"pnpm test", "yarn test",
	"pytest", "vitest", "jest",
	"cargo test", "cargo check", "cargo build",
	"go test", "go build", "go vet",
	"make test", "make check",
	"node --version", "python --version", "go version",
}

// volatileReadOnlyPrefixes lists read-only commands whose result is expected
// to change because of external processes, wall-clock state, schedulers, or
// service status. They should not fence other cache hits, but their own output
// must not be served as cached truth after compaction.
var volatileReadOnlyPrefixes = []string{
	"tail", "ps", "pgrep ", "jobs",
	"docker ps", "docker compose ps",
	"kubectl get", "kubectl describe", "kubectl logs",
	"nvidia-smi", "squeue", "sacct",
	"lsof ", "netstat", "ss ",
	"watch ",
}

// sideEffectPrefixes lists prefixes that change shared/persistent state.
// When a Bash command matches one of these, splice treats it as PolicySideEffect.
var sideEffectPrefixes = []string{
	"git push", "git commit", "git reset", "git rebase", "git checkout -b",
	"git merge", "git tag", "git stash drop",
	"rm ", "rm\t", "mv ", "cp ",
	"npm install", "npm publish", "pip install", "yarn add", "pnpm add",
	"curl -X POST", "curl -X PUT", "curl -X DELETE", "curl -X PATCH",
	"wget ",
	"kubectl apply", "kubectl delete", "kubectl create",
	"docker rm", "docker push", "docker rmi",
	"ssh ", "scp ",
}

// ClassifyBash returns the policy for a Bash command. Default for unknown
// commands is PolicySideEffect (conservative).
func ClassifyBash(command string) Policy {
	c := strings.TrimSpace(command)
	if c == "" {
		return PolicySideEffect
	}
	if IsKnownSideEffectBash(c) {
		return PolicySideEffect
	}
	for _, p := range volatileReadOnlyPrefixes {
		if hasPrefix(c, p) {
			return PolicyVolatileReadOnly
		}
	}
	for _, p := range readOnlyPrefixes {
		if hasPrefix(c, p) {
			return PolicyReadOnly
		}
	}
	return PolicySideEffect
}

// IsKnownSideEffectBash reports whether a Bash command is side-effecting for
// an explicit reason (shell control syntax or a known mutating prefix), rather
// than merely falling through to the conservative unknown-command default.
func IsKnownSideEffectBash(command string) bool {
	c := strings.TrimSpace(command)
	if c == "" {
		return true
	}
	if hasShellControlSyntax(c) {
		return true
	}
	for _, p := range sideEffectPrefixes {
		if hasPrefix(c, p) {
			return true
		}
	}
	return false
}

// ClassifyTool returns the policy for a non-Bash Claude Code tool by name.
func ClassifyTool(toolName string) Policy {
	switch toolName {
	case "Read", "Glob", "Grep", "WebFetch", "WebSearch":
		return PolicyReadOnly
	case "Write", "Edit", "NotebookEdit":
		return PolicySideEffect
	default:
		return PolicySideEffect
	}
}

func hasPrefix(cmd, prefix string) bool {
	if !strings.HasPrefix(cmd, prefix) {
		return false
	}
	// If the prefix already ends with a space/tab, the boundary is baked in.
	last := prefix[len(prefix)-1]
	if last == ' ' || last == '\t' {
		return true
	}
	// Otherwise `rm` should not match `rmdir`: require end-of-string, space, or tab.
	if len(cmd) == len(prefix) {
		return true
	}
	next := cmd[len(prefix)]
	return next == ' ' || next == '\t'
}

// hasShellControlSyntax reports whether a command contains shell syntax that
// can sequence commands, redirect output, start background work, or perform
// command substitution. When we see any of it, the safe choice is to fence:
// "git status && rm x" must never inherit git-status read-only semantics.
func hasShellControlSyntax(cmd string) bool {
	inSingle := false
	inDouble := false
	escaped := false

	for i := 0; i < len(cmd); i++ {
		ch := cmd[i]
		if ch == '\n' || ch == '\r' {
			return true
		}
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && !inSingle {
			escaped = true
			continue
		}
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}
		if !inSingle && ch == '$' && i+1 < len(cmd) && cmd[i+1] == '(' {
			return true
		}
		if !inSingle && ch == '`' {
			return true
		}
		if inSingle || inDouble {
			continue
		}
		switch ch {
		case ';', '&', '|', '<', '>':
			return true
		}
	}
	return false
}
