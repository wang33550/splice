package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyCreatesFresh(t *testing.T) {
	dir := t.TempDir()
	plan := Plan{
		SettingsPath:  filepath.Join(dir, "settings.json"),
		BinaryCommand: "splice",
		OnPath:        true,
		BinaryPath:    "/usr/local/bin/splice",
	}
	action, err := Apply(plan)
	if err != nil {
		t.Fatal(err)
	}
	if action != "created" {
		t.Errorf("action: %q", action)
	}
	got := readJSON(t, plan.SettingsPath)
	hooks := got["hooks"].(map[string]any)
	pre := hooks["PreToolUse"].([]any)
	if len(pre) != 1 {
		t.Fatalf("PreToolUse: %v", pre)
	}
	cmd := firstHookCommand(t, pre[0])
	if cmd != "splice pre-tool-use" {
		t.Errorf("command: %q", cmd)
	}
}

func TestApplyMergesExistingUnrelated(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	preExisting := map[string]any{
		"theme": "dark",
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": "Bash",
					"hooks": []any{
						map[string]any{"type": "command", "command": "/usr/local/bin/audit"},
					},
				},
			},
		},
	}
	writeJSON(t, settingsPath, preExisting)

	plan := Plan{SettingsPath: settingsPath, BinaryCommand: "splice"}
	if _, err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	got := readJSON(t, settingsPath)
	if got["theme"] != "dark" {
		t.Errorf("theme dropped: %v", got)
	}
	hooks := got["hooks"].(map[string]any)
	pre := hooks["PreToolUse"].([]any)
	if len(pre) != 2 {
		t.Fatalf("expected 2 PreToolUse entries, got %d: %v", len(pre), pre)
	}
	// One of them must be the audit one.
	foundAudit := false
	foundSplice := false
	for _, e := range pre {
		if firstHookCommand(t, e) == "/usr/local/bin/audit" {
			foundAudit = true
		}
		if firstHookCommand(t, e) == "splice pre-tool-use" {
			foundSplice = true
		}
	}
	if !foundAudit || !foundSplice {
		t.Errorf("audit=%v splice=%v entries=%v", foundAudit, foundSplice, pre)
	}
}

func TestApplyIdempotent(t *testing.T) {
	dir := t.TempDir()
	plan := Plan{
		SettingsPath:  filepath.Join(dir, "settings.json"),
		BinaryCommand: "splice",
	}
	if _, err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	got := readJSON(t, plan.SettingsPath)
	hooks := got["hooks"].(map[string]any)
	pre := hooks["PreToolUse"].([]any)
	post := hooks["PostToolUse"].([]any)
	preCompact := hooks["PreCompact"].([]any)
	sessionStart := hooks["SessionStart"].([]any)
	if len(pre) != 1 || len(post) != 1 || len(preCompact) != 1 || len(sessionStart) != 1 {
		t.Fatalf("re-install duplicated entries: pre=%d post=%d preCompact=%d sessionStart=%d",
			len(pre), len(post), len(preCompact), len(sessionStart))
	}
	if firstHookCommand(t, sessionStart[0]) != "splice session-start" {
		t.Errorf("SessionStart command: %q", firstHookCommand(t, sessionStart[0]))
	}
}

func TestUninstallRemovesOnlyOurs(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, "settings.json")
	preExisting := map[string]any{
		"theme": "dark",
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": "Bash",
					"hooks": []any{
						map[string]any{"type": "command", "command": "/usr/local/bin/audit"},
					},
				},
			},
		},
	}
	writeJSON(t, settingsPath, preExisting)
	plan := Plan{SettingsPath: settingsPath, BinaryCommand: "splice"}
	if _, err := Apply(plan); err != nil {
		t.Fatal(err)
	}

	removed, _, err := Uninstall(ScopeLocal, dir)
	// Local scope uses cwd/.claude/settings.local.json, so this should NOT find our file.
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Error("Uninstall should not have found a file at the local scope path")
	}

	// Now uninstall by manually targeting the same path used in Apply.
	settings, _, err := readSettings(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	settings, removed = stripSpliceHooks(settings)
	if !removed {
		t.Fatal("stripSpliceHooks reported no removal")
	}
	if err := writeSettings(settingsPath, settings); err != nil {
		t.Fatal(err)
	}
	got := readJSON(t, settingsPath)
	if got["theme"] != "dark" {
		t.Errorf("theme lost on uninstall: %v", got)
	}
	hooks, _ := got["hooks"].(map[string]any)
	pre, _ := hooks["PreToolUse"].([]any)
	if len(pre) != 1 || firstHookCommand(t, pre[0]) != "/usr/local/bin/audit" {
		t.Errorf("audit hook lost: %v", pre)
	}
	if _, exists := hooks["PostToolUse"]; exists {
		t.Errorf("empty PostToolUse not pruned: %v", hooks)
	}
}

func TestUninstallPublicLocalRemovesOnlyOurs(t *testing.T) {
	cwd := t.TempDir()
	settingsPath := filepath.Join(cwd, ".claude", "settings.local.json")
	preExisting := map[string]any{
		"theme": "dark",
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": "Bash",
					"hooks": []any{
						map[string]any{"type": "command", "command": "/usr/local/bin/audit"},
					},
				},
			},
		},
	}
	writeJSON(t, settingsPath, preExisting)
	if _, err := Apply(Plan{SettingsPath: settingsPath, BinaryCommand: "splice"}); err != nil {
		t.Fatal(err)
	}

	removed, path, err := Uninstall(ScopeLocal, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("expected managed hooks to be removed")
	}
	if path != settingsPath {
		t.Fatalf("path = %q, want %q", path, settingsPath)
	}
	got := readJSON(t, settingsPath)
	hooks := got["hooks"].(map[string]any)
	pre := hooks["PreToolUse"].([]any)
	if len(pre) != 1 || firstHookCommand(t, pre[0]) != "/usr/local/bin/audit" {
		t.Fatalf("audit hook should remain, got %v", pre)
	}
	if _, exists := hooks["PostToolUse"]; exists {
		t.Fatalf("PostToolUse should be pruned, got %v", hooks)
	}
}

func TestUninstallMissingFileIsNoop(t *testing.T) {
	cwd := t.TempDir()
	removed, path, err := Uninstall(ScopeLocal, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatal("missing settings should not report removal")
	}
	if path != filepath.Join(cwd, ".claude", "settings.local.json") {
		t.Fatalf("unexpected path: %q", path)
	}
}

func TestResolvePlanPathDetection(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "splice")
	cwd := t.TempDir()

	plan, err := ResolvePlan(ScopeLocal, cwd, binPath, func(name string) (string, error) {
		if name == "splice" {
			return binPath, nil
		}
		return "", os.ErrNotExist
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.OnPath {
		t.Errorf("plan.OnPath should be true when LookPath matches")
	}
	if plan.BinaryCommand != "splice" {
		t.Errorf("BinaryCommand: %q", plan.BinaryCommand)
	}

	plan2, err := ResolvePlan(ScopeLocal, cwd, binPath, func(string) (string, error) {
		return "", os.ErrNotExist
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan2.OnPath {
		t.Errorf("plan2.OnPath should be false")
	}
	if plan2.BinaryCommand != binPath {
		t.Errorf("expected absolute path command, got %q", plan2.BinaryCommand)
	}
}

func TestResolvePlanQuotesPathWithSpaces(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "with spaces")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(dir, "splice")
	plan, err := ResolvePlan(ScopeLocal, t.TempDir(), binPath, func(string) (string, error) {
		return "", os.ErrNotExist
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plan.BinaryCommand, `"`) || !strings.HasSuffix(plan.BinaryCommand, `"`) {
		t.Errorf("expected quoted command for path with spaces: %q", plan.BinaryCommand)
	}
}

func TestResolvePlanDefaultLocalCwd(t *testing.T) {
	cwd := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	plan, err := ResolvePlan(ScopeLocal, "", "/bin/splice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SettingsPath != filepath.Join(cwd, ".claude", "settings.local.json") {
		t.Fatalf("default local settings path = %q", plan.SettingsPath)
	}
}

func TestResolvePlanUnknownScopeErrors(t *testing.T) {
	_, err := ResolvePlan(Scope(99), t.TempDir(), "/bin/splice", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown scope") {
		t.Fatalf("expected unknown scope error, got %v", err)
	}
}

func TestReadSettingsNullAndMalformed(t *testing.T) {
	dir := t.TempDir()
	nullPath := filepath.Join(dir, "null.json")
	if err := os.WriteFile(nullPath, []byte(`null`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, action, err := readSettings(nullPath)
	if err != nil {
		t.Fatal(err)
	}
	if action != "updated" || len(m) != 0 {
		t.Fatalf("null settings should become empty updated map, action=%q map=%v", action, m)
	}

	badPath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badPath, []byte(`{bad`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readSettings(badPath); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestStripSpliceHooksNoHooksAndNonArray(t *testing.T) {
	settings := map[string]any{"theme": "dark"}
	got, removed := stripSpliceHooks(settings)
	if removed || got["theme"] != "dark" {
		t.Fatalf("no hooks should be no-op, removed=%v map=%v", removed, got)
	}

	settings = map[string]any{"hooks": map[string]any{"PreToolUse": map[string]any{"description": Marker}}}
	got, removed = stripSpliceHooks(settings)
	if removed {
		t.Fatalf("non-array Claude hook shape should be ignored, got %v", got)
	}
}

func TestLegacySpliceManagedDetection(t *testing.T) {
	legacy := map[string]any{
		"matcher": "*",
		"hooks": []any{
			map[string]any{"type": "command", "command": "/tmp/splice pre-tool-use"},
		},
	}
	if !isSpliceManaged(legacy) {
		t.Fatal("legacy command should be recognized as splice-managed")
	}
	if isSpliceManaged(map[string]any{"hooks": []any{map[string]any{"command": ""}}}) {
		t.Fatal("empty command should not be splice-managed")
	}
	if containsToken("abc", "abcd") {
		t.Fatal("longer token should not match")
	}
	if matcherOf("not-map") != "" {
		t.Fatal("non-map matcher should be empty")
	}
}

// helpers

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func firstHookCommand(t *testing.T, entry any) string {
	t.Helper()
	em, ok := entry.(map[string]any)
	if !ok {
		t.Fatalf("entry not a map: %T", entry)
	}
	hooks, _ := em["hooks"].([]any)
	if len(hooks) == 0 {
		return ""
	}
	hm, ok := hooks[0].(map[string]any)
	if !ok {
		t.Fatalf("hook not a map: %T", hooks[0])
	}
	cmd, _ := hm["command"].(string)
	return cmd
}
