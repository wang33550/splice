// Package codex parses Codex rollout JSONL and bridges to splice's shared
// fingerprint logic. Fingerprinting itself lives in internal/fingerprint —
// this package only adapts Codex's wire format to the canonical input shape.
package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/wang33550/splice/internal/fingerprint"
)

// FingerprintToolCall produces (canonicalJSON, hexHash) for a Codex rollout
// tool call entry. The result is byte-identical to what fingerprint.Compute
// returns for the same call when invoked from a Claude Code hook, which
// means trail entries written by codex-watch will hash-match the runtime
// PreToolUse hook decisions.
//
// "shell" (Codex name) is folded to "Bash" (Claude name) so the same
// `npm test` invocation hashes the same on both hosts.
func FingerprintToolCall(toolName, argsJSON string) (canonical, hexHash string) {
	return FingerprintToolCallScoped(toolName, argsJSON, "")
}

// FingerprintToolCallScoped is FingerprintToolCall plus an execution scope,
// usually store.ProjectKey(cwd). Codex rollout events do not repeat cwd on
// every tool call, so the watcher supplies it from session metadata.
func FingerprintToolCallScoped(toolName, argsJSON, scope string) (canonical, hexHash string) {
	args := decodeArgs(argsJSON)
	canonName := canonicalToolName(toolName)
	canonical, hash, err := fingerprint.ComputeScoped(canonName, args, scope)
	if err != nil {
		// fingerprint.Compute only fails on JSON marshal errors of bizarre
		// inputs (NaN, Inf, channels). For Codex-sourced data this should
		// not happen, but if it ever does we fall back to a deterministic
		// "tool only" hash so downstream comparisons stay stable.
		canonical = fmt.Sprintf(`{"args":null,"tool":%q}`, canonName)
		hash = sha256HexOf(canonical)
		return
	}
	return canonical, hash
}

func canonicalToolName(name string) string {
	if name == "shell" {
		return "Bash"
	}
	return name
}

// decodeArgs parses Codex's argument JSON into a map. Codex stores args as
// either a JSON object string or an empty/missing value. We always return a
// map so fingerprint.Compute can apply its normalization rules consistently.
func decodeArgs(argsJSON string) map[string]any {
	if argsJSON == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		// Some payloads put a bare string under arguments. Keep it under a
		// synthetic key so fingerprint.Compute still produces a stable hash.
		return map[string]any{"_raw": argsJSON}
	}
	return m
}

// LabelFromToolCall extracts a human-readable label for a Codex rollout
// tool call. For shell/Bash this is the command. We use this when injecting
// "上次运行了 X" so the user/model sees something meaningful.
func LabelFromToolCall(toolName, argsJSON string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil || m == nil {
		return ""
	}
	if cmd, ok := m["command"].(string); ok && cmd != "" {
		return cmd
	}
	if fp, ok := m["file_path"].(string); ok && fp != "" {
		return fp
	}
	return fmt.Sprintf("%v", m)
}

// sha256HexOf is used only on the bizarre-input fallback path of
// FingerprintToolCall. Defining it locally keeps that path self-contained.
func sha256HexOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
