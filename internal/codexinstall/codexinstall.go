// Package codexinstall writes splice's Codex CLI hooks into ~/.codex/config.toml.
//
// Schema (from developers.openai.com/codex/hooks):
//
//	[[hooks.PreToolUse]]
//	matcher = "*"
//	description = "splice-managed"     # our marker for idempotent install
//
//	[[hooks.PreToolUse.hooks]]
//	type = "command"
//	command = "splice codex-pre-tool-use"
//	timeout = 30
//
// We register PreToolUse / PostToolUse / SessionStart hooks. PreCompact
// doesn't exist in Codex — that's why we have codex-watch instead.
package codexinstall

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

const Marker = "splice-managed"

type Scope int

const (
	ScopeUser Scope = iota
	ScopeProject
)

// Plan describes the install operation.
type Plan struct {
	ConfigPath    string
	BinaryCommand string
	OnPath        bool
	BinaryPath    string
}

func ResolvePlan(scope Scope, cwd, binaryPath string, lookPath func(string) (string, error)) (Plan, error) {
	configPath, err := configPathFor(scope, cwd)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{ConfigPath: configPath, BinaryPath: binaryPath}
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

func configPathFor(scope Scope, cwd string) (string, error) {
	switch scope {
	case ScopeUser:
		if v := os.Getenv("CODEX_HOME"); v != "" {
			return filepath.Join(v, "config.toml"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".codex", "config.toml"), nil
	case ScopeProject:
		if cwd == "" {
			c, err := os.Getwd()
			if err != nil {
				return "", err
			}
			cwd = c
		}
		return filepath.Join(cwd, ".codex", "config.toml"), nil
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
		return strings.EqualFold(aa, bb)
	}
	return aa == bb
}

func quoteIfNeeded(p string) string {
	for _, r := range p {
		if r == ' ' || r == '\t' {
			return `"` + p + `"`
		}
	}
	return p
}

// Apply writes splice's hook entries into the config file at plan.ConfigPath,
// preserving any unrelated configuration. Returns "created" or "updated".
func Apply(plan Plan) (string, error) {
	if err := os.MkdirAll(filepath.Dir(plan.ConfigPath), 0o755); err != nil {
		return "", fmt.Errorf("install: mkdir config dir: %w", err)
	}

	cfg, action, err := readTOML(plan.ConfigPath)
	if err != nil {
		return "", err
	}

	cfg = mergeSpliceHooks(cfg, plan.BinaryCommand)

	if err := writeTOML(plan.ConfigPath, cfg); err != nil {
		return "", err
	}
	return action, nil
}

func Uninstall(scope Scope, cwd string) (removed bool, path string, err error) {
	configPath, err := configPathFor(scope, cwd)
	if err != nil {
		return false, "", err
	}
	cfg, _, err := readTOML(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, configPath, nil
		}
		return false, configPath, err
	}
	cfg, removed = stripSpliceHooks(cfg)
	if !removed {
		return false, configPath, nil
	}
	if err := writeTOML(configPath, cfg); err != nil {
		return false, configPath, err
	}
	return true, configPath, nil
}

// ----------------------------------------------------------------------
// TOML I/O — we keep the document as a generic map[string]any so that any
// keys we don't manage (model, theme, mcp_servers, ...) round-trip
// untouched.
// ----------------------------------------------------------------------

func readTOML(path string) (map[string]any, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, "created", nil
		}
		return nil, "", fmt.Errorf("install: read %s: %w", path, err)
	}
	if len(raw) == 0 {
		return map[string]any{}, "updated", nil
	}
	var m map[string]any
	if err := toml.Unmarshal(raw, &m); err != nil {
		return nil, "", fmt.Errorf("install: parse %s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, "updated", nil
}

func writeTOML(path string, cfg map[string]any) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("install: write tmp: %w", err)
	}
	enc := toml.NewEncoder(f)
	enc.Indent = ""
	if err := enc.Encode(orderedTopLevel(cfg)); err != nil {
		_ = f.Close()
		return fmt.Errorf("install: encode toml: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install: rename: %w", err)
	}
	return nil
}

// orderedTopLevel returns a copy of cfg whose top-level keys iterate
// alphabetically so the resulting TOML diff is stable across runs.
// (BurntSushi/toml encodes maps in source order; an explicit sorted copy
// is the simplest way to get determinism.)
func orderedTopLevel(cfg map[string]any) map[string]any {
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]any, len(cfg))
	for _, k := range keys {
		out[k] = cfg[k]
	}
	return out
}

// ----------------------------------------------------------------------
// merge / strip
// ----------------------------------------------------------------------

func mergeSpliceHooks(cfg map[string]any, binaryCommand string) map[string]any {
	hooks, _ := cfg["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}

	hooks["PreToolUse"] = mergeEntries(hooks["PreToolUse"], spliceMatcherEntry(binaryCommand+" codex-pre-tool-use"))
	hooks["PostToolUse"] = mergeEntries(hooks["PostToolUse"], spliceMatcherEntry(binaryCommand+" codex-post-tool-use"))
	hooks["SessionStart"] = mergeEntries(hooks["SessionStart"], spliceMatcherEntry(binaryCommand+" session-start"))

	cfg["hooks"] = hooks
	return cfg
}

func stripSpliceHooks(cfg map[string]any) (map[string]any, bool) {
	hooks, _ := cfg["hooks"].(map[string]any)
	if hooks == nil {
		return cfg, false
	}
	removed := false
	for _, key := range []string{"PreToolUse", "PostToolUse", "SessionStart", "Stop", "PermissionRequest", "UserPromptSubmit"} {
		raw, ok := hooks[key]
		if !ok {
			continue
		}
		entries := toEntryList(raw)
		kept := make([]any, 0, len(entries))
		for _, e := range entries {
			if isSpliceManaged(e) {
				removed = true
				continue
			}
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			delete(hooks, key)
		} else {
			hooks[key] = kept
		}
	}
	if len(hooks) == 0 {
		delete(cfg, "hooks")
	} else {
		cfg["hooks"] = hooks
	}
	return cfg, removed
}

func spliceMatcherEntry(commandLine string) map[string]any {
	return map[string]any{
		"matcher":     "*",
		"description": Marker,
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": commandLine,
				"timeout": int64(30),
			},
		},
	}
}

// mergeEntries replaces any pre-existing splice-managed entries while
// preserving unrelated ones, then appends our fresh entry.
func mergeEntries(existing any, ours map[string]any) []any {
	out := []any{}
	for _, e := range toEntryList(existing) {
		if isSpliceManaged(e) {
			continue
		}
		out = append(out, e)
	}
	out = append(out, ours)
	return out
}

func toEntryList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []map[string]any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			out = append(out, e)
		}
		return out
	case map[string]any:
		// A bare table (single hook entry); treat as a one-element array.
		return []any{x}
	}
	return nil
}

func isSpliceManaged(e any) bool {
	m, ok := e.(map[string]any)
	if !ok {
		return false
	}
	if d, ok := m["description"].(string); ok && d == Marker {
		return true
	}
	// Defensive: detect by command token even without a description marker.
	hooks := toEntryList(m["hooks"])
	for _, h := range hooks {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		cmd, _ := hm["command"].(string)
		if strings.Contains(cmd, "splice codex-pre-tool-use") ||
			strings.Contains(cmd, "splice codex-post-tool-use") ||
			strings.Contains(cmd, "splice session-start") {
			return true
		}
	}
	return false
}
