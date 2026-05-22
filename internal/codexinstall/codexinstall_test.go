package codexinstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestApplyCreatesFresh(t *testing.T) {
	dir := t.TempDir()
	plan := Plan{
		ConfigPath:    filepath.Join(dir, "config.toml"),
		BinaryCommand: "splice",
	}
	action, err := Apply(plan)
	if err != nil {
		t.Fatal(err)
	}
	if action != "created" {
		t.Errorf("action: %q", action)
	}
	cfg := readTOMLForTest(t, plan.ConfigPath)
	hooks := cfg["hooks"].(map[string]any)
	pre := entryList(t, hooks["PreToolUse"])
	if len(pre) != 1 {
		t.Fatalf("PreToolUse: %v", pre)
	}
	first := pre[0]
	if first["description"] != Marker {
		t.Errorf("missing splice marker: %v", first)
	}
	innerHooks := entryList(t, first["hooks"])
	cmd, _ := innerHooks[0]["command"].(string)
	if cmd != "splice codex-pre-tool-use" {
		t.Errorf("command: %q", cmd)
	}
}

func TestApplyMergesExistingUnrelated(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	preExisting := `model = "gpt-5"
[mcp_servers.fs]
command = "fs-server"

[[hooks.PreToolUse]]
matcher = "Bash"

[[hooks.PreToolUse.hooks]]
type = "command"
command = "/usr/local/bin/audit"
`
	if err := os.WriteFile(configPath, []byte(preExisting), 0o644); err != nil {
		t.Fatal(err)
	}

	plan := Plan{ConfigPath: configPath, BinaryCommand: "splice"}
	if _, err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	cfg := readTOMLForTest(t, configPath)
	if cfg["model"] != "gpt-5" {
		t.Errorf("model dropped: %v", cfg)
	}
	if _, ok := cfg["mcp_servers"]; !ok {
		t.Errorf("mcp_servers dropped: %v", cfg)
	}
	hooks := cfg["hooks"].(map[string]any)
	pre := entryList(t, hooks["PreToolUse"])
	if len(pre) != 2 {
		t.Fatalf("expected 2 PreToolUse entries, got %d: %v", len(pre), pre)
	}

	foundAudit := false
	foundSplice := false
	for _, em := range pre {
		inner := entryList(t, em["hooks"])
		if len(inner) == 0 {
			continue
		}
		cmd, _ := inner[0]["command"].(string)
		if strings.Contains(cmd, "audit") {
			foundAudit = true
		}
		if strings.Contains(cmd, "splice codex-pre-tool-use") {
			foundSplice = true
		}
	}
	if !foundAudit || !foundSplice {
		t.Errorf("audit=%v splice=%v entries=%v", foundAudit, foundSplice, pre)
	}
}

func TestApplyIdempotent(t *testing.T) {
	dir := t.TempDir()
	plan := Plan{ConfigPath: filepath.Join(dir, "config.toml"), BinaryCommand: "splice"}
	if _, err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(plan); err != nil {
		t.Fatal(err)
	}
	cfg := readTOMLForTest(t, plan.ConfigPath)
	hooks := cfg["hooks"].(map[string]any)
	pre := entryList(t, hooks["PreToolUse"])
	post := entryList(t, hooks["PostToolUse"])
	ss := entryList(t, hooks["SessionStart"])
	if len(pre) != 1 || len(post) != 1 || len(ss) != 1 {
		t.Fatalf("re-install duplicated entries: pre=%d post=%d ss=%d", len(pre), len(post), len(ss))
	}
	ssHooks := entryList(t, ss[0]["hooks"])
	if got, _ := ssHooks[0]["command"].(string); got != "splice codex-session-start" {
		t.Fatalf("SessionStart should auto-start watcher via codex-session-start, got %q", got)
	}
}

func TestUninstallRemovesOnlyOurs(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.toml")
	preExisting := `model = "gpt-5"
[[hooks.PreToolUse]]
matcher = "Bash"
[[hooks.PreToolUse.hooks]]
type = "command"
command = "/usr/local/bin/audit"
`
	if err := os.WriteFile(configPath, []byte(preExisting), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := Plan{ConfigPath: configPath, BinaryCommand: "splice"}
	if _, err := Apply(plan); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := readTOML(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, removed := stripSpliceHooks(cfg)
	if !removed {
		t.Fatal("strip reported nothing removed")
	}
	if err := writeTOML(configPath, cfg); err != nil {
		t.Fatal(err)
	}

	got := readTOMLForTest(t, configPath)
	if got["model"] != "gpt-5" {
		t.Errorf("model lost: %v", got)
	}
	hooks, _ := got["hooks"].(map[string]any)
	pre := entryList(t, hooks["PreToolUse"])
	if len(pre) != 1 {
		t.Errorf("audit hook should remain solo: %v", pre)
	}
	if _, exists := hooks["PostToolUse"]; exists {
		t.Errorf("empty PostToolUse not pruned: %v", hooks)
	}
	if _, exists := hooks["SessionStart"]; exists {
		t.Errorf("empty SessionStart not pruned: %v", hooks)
	}
}

func TestUninstallPublicRemovesOnlyOurs(t *testing.T) {
	cwd := t.TempDir()
	configPath := filepath.Join(cwd, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	preExisting := `model = "gpt-5"
[[hooks.PreToolUse]]
matcher = "Bash"
[[hooks.PreToolUse.hooks]]
type = "command"
command = "/usr/local/bin/audit"
`
	if err := os.WriteFile(configPath, []byte(preExisting), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(Plan{ConfigPath: configPath, BinaryCommand: "splice"}); err != nil {
		t.Fatal(err)
	}

	removed, path, err := Uninstall(ScopeProject, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("expected managed hooks to be removed")
	}
	if path != configPath {
		t.Fatalf("path = %q, want %q", path, configPath)
	}
	got := readTOMLForTest(t, configPath)
	hooks := got["hooks"].(map[string]any)
	pre := entryList(t, hooks["PreToolUse"])
	if len(pre) != 1 {
		t.Fatalf("unrelated audit hook should remain, got %v", pre)
	}
	if _, exists := hooks["PostToolUse"]; exists {
		t.Fatalf("PostToolUse should be pruned, got %v", hooks)
	}
}

func TestUninstallMissingFileIsNoop(t *testing.T) {
	cwd := t.TempDir()
	removed, path, err := Uninstall(ScopeProject, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Fatal("missing config should not report removal")
	}
	if path != filepath.Join(cwd, ".codex", "config.toml") {
		t.Fatalf("unexpected path: %q", path)
	}
}

func TestResolvePlanProjectPathAndQuoting(t *testing.T) {
	cwd := t.TempDir()
	bin := filepath.Join(cwd, "dir with spaces", "splice.exe")
	plan, err := ResolvePlan(ScopeProject, cwd, bin, func(string) (string, error) {
		return "", os.ErrNotExist
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ConfigPath != filepath.Join(cwd, ".codex", "config.toml") {
		t.Fatalf("config path = %q", plan.ConfigPath)
	}
	if plan.OnPath {
		t.Fatal("expected OnPath=false")
	}
	if !strings.HasPrefix(plan.BinaryCommand, `"`) || !strings.HasSuffix(plan.BinaryCommand, `"`) {
		t.Fatalf("binary path with spaces should be quoted, got %q", plan.BinaryCommand)
	}
}

func TestResolvePlanUserUsesCodexHomeAndPathDetection(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	bin := filepath.Join(codexHome, "bin", "splice.exe")
	plan, err := ResolvePlan(ScopeUser, "", bin, func(string) (string, error) {
		return bin, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ConfigPath != filepath.Join(codexHome, "config.toml") {
		t.Fatalf("config path = %q", plan.ConfigPath)
	}
	if !plan.OnPath || plan.BinaryCommand != "splice" {
		t.Fatalf("expected PATH command, got OnPath=%v command=%q", plan.OnPath, plan.BinaryCommand)
	}
}

func TestResolvePlanUnknownScopeErrors(t *testing.T) {
	_, err := ResolvePlan(Scope(99), t.TempDir(), "/bin/splice", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown scope") {
		t.Fatalf("expected unknown scope error, got %v", err)
	}
}

func TestReadTOMLNullAndMalformed(t *testing.T) {
	dir := t.TempDir()
	emptyTable := filepath.Join(dir, "empty-table.toml")
	if err := os.WriteFile(emptyTable, []byte(``), 0o644); err != nil {
		t.Fatal(err)
	}
	m, action, err := readTOML(emptyTable)
	if err != nil {
		t.Fatal(err)
	}
	if action != "updated" || len(m) != 0 {
		t.Fatalf("empty TOML should become empty updated map, action=%q map=%v", action, m)
	}

	badPath := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(badPath, []byte(`[bad`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readTOML(badPath); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("expected parse error, got %v", err)
	}
}

func TestEntryListShapesAndLegacySpliceManagedDetection(t *testing.T) {
	one := map[string]any{"description": Marker}
	if got := toEntryList(one); len(got) != 1 {
		t.Fatalf("bare map should become one entry, got %v", got)
	}
	many := []map[string]any{{"a": 1}, {"b": 2}}
	if got := toEntryList(many); len(got) != 2 {
		t.Fatalf("[]map shape should become two entries, got %v", got)
	}
	if got := toEntryList("bad"); got != nil {
		t.Fatalf("unexpected list for bad shape: %v", got)
	}

	legacy := map[string]any{
		"hooks": []any{
			map[string]any{"command": "/tmp/splice codex-post-tool-use"},
		},
	}
	if !isSpliceManaged(legacy) {
		t.Fatal("legacy codex command should be recognized")
	}
	if isSpliceManaged(map[string]any{"hooks": []any{map[string]any{"command": "/tmp/other"}}}) {
		t.Fatal("unrelated command should not be splice-managed")
	}
}

// entryList normalizes the slice shape returned by BurntSushi/toml: it can
// be []any (when keys mix types) or []map[string]any (uniform tables).
func entryList(t *testing.T, v any) []map[string]any {
	t.Helper()
	switch x := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]map[string]any, 0, len(x))
		for _, e := range x {
			m, ok := e.(map[string]any)
			if !ok {
				t.Fatalf("entry not a map: %T", e)
			}
			out = append(out, m)
		}
		return out
	case []map[string]any:
		return x
	case map[string]any:
		return []map[string]any{x}
	default:
		t.Fatalf("unexpected shape: %T", v)
		return nil
	}
}

func readTOMLForTest(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := toml.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
