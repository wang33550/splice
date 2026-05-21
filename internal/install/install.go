// Package install writes splice's PreToolUse / PostToolUse hooks into a
// Claude Code settings.json file, preserving any unrelated configuration.
//
// Two scopes:
//   - user:  ~/.claude/settings.json
//   - local: <cwd>/.claude/settings.local.json
//
// Idempotent: re-running on the same path either no-ops or rewrites the
// splice entries, never duplicating or clobbering unrelated hooks.
package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
)

// Marker is embedded in every hook entry splice writes so we can recognize
// our own entries on subsequent runs and replace them in place.
const Marker = "splice-managed"

// Plan describes the install operation: where settings live, which command
// to invoke, and whether splice is already on PATH.
type Plan struct {
	SettingsPath  string // absolute path to settings.json that will be written
	BinaryCommand string // the command we'll write into hook entries
	OnPath        bool   // true if `splice` was found on PATH (BinaryCommand=="splice")
	BinaryPath    string // resolved absolute path to splice binary, regardless of PATH
}

// Scope picks the settings file location.
type Scope int

const (
	ScopeUser Scope = iota
	ScopeLocal
)

// ResolvePlan figures out where to write and which command to invoke.
// `binaryPath` is splice's own absolute path (typically os.Executable()).
// `lookPath` is dependency-injected for testability — pass exec.LookPath in
// production.
func ResolvePlan(scope Scope, cwd, binaryPath string, lookPath func(string) (string, error)) (Plan, error) {
	settingsPath, err := settingsPathFor(scope, cwd)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{SettingsPath: settingsPath, BinaryPath: binaryPath}

	if lookPath != nil {
		if found, err := lookPath("splice"); err == nil && samePath(found, binaryPath) {
			plan.OnPath = true
			plan.BinaryCommand = "splice"
			return plan, nil
		}
	}
	plan.BinaryCommand = quoteIfNeeded(binaryPath)
	return plan, nil
}

func settingsPathFor(scope Scope, cwd string) (string, error) {
	switch scope {
	case ScopeUser:
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("install: resolve home: %w", err)
		}
		return filepath.Join(home, ".claude", "settings.json"), nil
	case ScopeLocal:
		if cwd == "" {
			var err error
			cwd, err = os.Getwd()
			if err != nil {
				return "", fmt.Errorf("install: resolve cwd: %w", err)
			}
		}
		return filepath.Join(cwd, ".claude", "settings.local.json"), nil
	default:
		return "", fmt.Errorf("install: unknown scope %d", scope)
	}
}

func samePath(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return a == b
	}
	if runtime.GOOS == "windows" {
		return equalFold(aa, bb)
	}
	return aa == bb
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 32
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// quoteIfNeeded quotes a binary path if it contains whitespace, so Claude
// Code's hook executor can run it as a single argv[0] token.
func quoteIfNeeded(p string) string {
	for _, r := range p {
		if r == ' ' || r == '\t' {
			return `"` + p + `"`
		}
	}
	return p
}

// Apply writes splice's hook entries into the settings file at plan.SettingsPath,
// preserving any unrelated hooks. Returns the action taken: "created" if the
// file was new, "updated" if existing settings were merged.
func Apply(plan Plan) (string, error) {
	if err := os.MkdirAll(filepath.Dir(plan.SettingsPath), 0o755); err != nil {
		return "", fmt.Errorf("install: mkdir settings dir: %w", err)
	}

	settings, action, err := readSettings(plan.SettingsPath)
	if err != nil {
		return "", err
	}

	settings = mergeSpliceHooks(settings, plan.BinaryCommand)

	if err := writeSettings(plan.SettingsPath, settings); err != nil {
		return "", err
	}
	return action, nil
}

// Uninstall removes splice's managed hook entries from the settings file.
// Other hooks and top-level keys are left intact. If the resulting hooks
// section is empty, it is dropped to keep the file clean.
func Uninstall(scope Scope, cwd string) (removed bool, path string, err error) {
	settingsPath, err := settingsPathFor(scope, cwd)
	if err != nil {
		return false, "", err
	}
	settings, _, err := readSettings(settingsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, settingsPath, nil
		}
		return false, settingsPath, err
	}
	settings, removed = stripSpliceHooks(settings)
	if !removed {
		return false, settingsPath, nil
	}
	if err := writeSettings(settingsPath, settings); err != nil {
		return false, settingsPath, err
	}
	return true, settingsPath, nil
}

func readSettings(path string) (map[string]any, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, "created", nil
		}
		return nil, "", fmt.Errorf("install: read settings: %w", err)
	}
	if len(raw) == 0 {
		return map[string]any{}, "updated", nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, "", fmt.Errorf("install: parse %s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, "updated", nil
}

func writeSettings(path string, settings map[string]any) error {
	// Preserve a stable key order in the top-level object for human readability.
	out, err := json.MarshalIndent(sortedRoot(settings), "", "  ")
	if err != nil {
		return fmt.Errorf("install: marshal settings: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return fmt.Errorf("install: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install: rename: %w", err)
	}
	return nil
}

// sortedRoot returns a copy of m with deterministic iteration: encoding/json
// already sorts map keys alphabetically, so this is a no-op pass-through —
// kept as a hook in case we want a custom order later.
func sortedRoot(m map[string]any) map[string]any { return m }

// mergeSpliceHooks ensures hooks.PreToolUse, hooks.PostToolUse and hooks.PreCompact
// each contain a single splice-managed entry that calls binaryCommand. Any
// pre-existing splice-managed entries are replaced; any unrelated entries
// (other tools, other matchers) are preserved verbatim.
func mergeSpliceHooks(settings map[string]any, binaryCommand string) map[string]any {
	hooksAny, _ := settings["hooks"].(map[string]any)
	if hooksAny == nil {
		hooksAny = map[string]any{}
	}

	hooksAny["PreToolUse"] = mergeEntries(
		hooksAny["PreToolUse"],
		spliceMatcherEntry(binaryCommand+" pre-tool-use"),
	)
	hooksAny["PostToolUse"] = mergeEntries(
		hooksAny["PostToolUse"],
		spliceMatcherEntry(binaryCommand+" post-tool-use"),
	)
	// PreCompact uses matcher field "auto|manual" per Claude Code docs;
	// however our spliceMatcherEntry uses "*" which Claude Code accepts as
	// "any trigger". We register it for both auto and manual compaction.
	hooksAny["PreCompact"] = mergeEntries(
		hooksAny["PreCompact"],
		spliceMatcherEntry(binaryCommand+" pre-compact"),
	)
	// SessionStart: needed so splice can write per-session markers (used by
	// the ecosystem's Codex watcher and by future cross-host work) and react
	// to source=clear by wiping that session's trail. Without this hook the
	// /clear command leaves stale cached results that mislead the freshly
	// amnesic model.
	hooksAny["SessionStart"] = mergeEntries(
		hooksAny["SessionStart"],
		spliceMatcherEntry(binaryCommand+" session-start"),
	)

	settings["hooks"] = hooksAny
	return settings
}

// stripSpliceHooks removes any matcher entries we own and reports whether
// anything was actually removed. Empty arrays / empty hooks sections are
// pruned to keep the settings file tidy.
func stripSpliceHooks(settings map[string]any) (map[string]any, bool) {
	hooksAny, _ := settings["hooks"].(map[string]any)
	if hooksAny == nil {
		return settings, false
	}
	removed := false
	for _, key := range []string{"PreToolUse", "PostToolUse", "PreCompact", "SessionStart"} {
		entries, ok := hooksAny[key].([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(entries))
		for _, e := range entries {
			if isSpliceManaged(e) {
				removed = true
				continue
			}
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			delete(hooksAny, key)
		} else {
			hooksAny[key] = kept
		}
	}
	if len(hooksAny) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooksAny
	}
	return settings, removed
}

// spliceMatcherEntry builds a fresh splice-managed matcher entry. We tag it
// with our marker via a description so future installs can find and replace
// it without touching unrelated hooks.
//
// Schema follows Claude Code's documented hooks format:
//
//	{ "matcher": "*", "hooks": [{ "type": "command", "command": "..." }] }
func spliceMatcherEntry(commandLine string) map[string]any {
	return map[string]any{
		"matcher":     "*",
		"description": Marker, // marker for idempotent re-install / uninstall
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": commandLine,
			},
		},
	}
}

// mergeEntries keeps unrelated entries and replaces (or inserts) the
// splice-managed one.
func mergeEntries(existing any, ours map[string]any) []any {
	out := []any{}
	if list, ok := existing.([]any); ok {
		for _, e := range list {
			if isSpliceManaged(e) {
				continue // drop old splice entries; we rewrite a fresh one below
			}
			out = append(out, e)
		}
	}
	out = append(out, ours)
	// Sort by matcher for stable diffs; splice entry uses "*" so it tends to
	// land first naturally.
	sort.SliceStable(out, func(i, j int) bool {
		return matcherOf(out[i]) < matcherOf(out[j])
	})
	return out
}

func isSpliceManaged(e any) bool {
	m, ok := e.(map[string]any)
	if !ok {
		return false
	}
	if d, ok := m["description"].(string); ok && d == Marker {
		return true
	}
	// Defensive: also recognize legacy entries by their command shape.
	hooks, _ := m["hooks"].([]any)
	for _, h := range hooks {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		cmd, _ := hm["command"].(string)
		if cmd == "" {
			continue
		}
		if containsToken(cmd, "splice pre-tool-use") || containsToken(cmd, "splice post-tool-use") || containsToken(cmd, "splice pre-compact") || containsToken(cmd, "splice session-start") {
			return true
		}
	}
	return false
}

func containsToken(haystack, token string) bool {
	for i := 0; i+len(token) <= len(haystack); i++ {
		if haystack[i:i+len(token)] == token {
			return true
		}
	}
	return false
}

func matcherOf(e any) string {
	m, ok := e.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m["matcher"].(string)
	return s
}
