package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wang33550/splice/internal/codexinstall"
	"github.com/wang33550/splice/internal/config"
	"github.com/wang33550/splice/internal/fingerprint"
	"github.com/wang33550/splice/internal/hook"
	"github.com/wang33550/splice/internal/install"
	"github.com/wang33550/splice/internal/store"
)

func TestMain(m *testing.M) {
	if os.Getenv("SPLICE_TEST_EXIT_HELPER") != "" {
		if os.Getenv("SPLICE_TEST_EXIT_ERROR") != "" {
			exit(fmt.Errorf("boom"))
		}
		exit(nil)
		return
	}
	root, err := os.MkdirTemp("", "splice-main-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.Setenv("SPLICE_HOME", root)
	_ = os.Setenv("SPLICE_NO_AUTO_WATCH", "1")
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

func runExitForTest(err error) int {
	args := []string{"-test.run=TestMain"}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "SPLICE_TEST_EXIT_HELPER=1")
	if err != nil {
		cmd.Env = append(cmd.Env, "SPLICE_TEST_EXIT_ERROR=1")
	}
	runErr := cmd.Run()
	if runErr == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// In v0.3, splice is dormant until PreCompact has fired for the session.
// A repeat call before any compaction must be allowed and not produce
// any injected context — even though pre-tool-use still records the call
// so that PostToolUse can attach a result.
func TestNoPreCompactNoIntercept(t *testing.T) {
	cwd := t.TempDir()

	first := runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-D", "npm test"))
	if first.HookSpecificOutput.AdditionalContext != "" || first.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("first call: %+v", first.HookSpecificOutput)
	}
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, "sess-D", "npm test", map[string]any{
		"exit_code": 0,
		"output":    "12 passed",
	}))

	second := runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-D", "npm test"))
	if second.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("v0.3 must not intercept before PreCompact; got %+v", second.HookSpecificOutput)
	}
	if second.HookSpecificOutput.AdditionalContext != "" {
		t.Fatalf("v0.3 must not inject before PreCompact; got %q", second.HookSpecificOutput.AdditionalContext)
	}
}

func TestParseScope(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    install.Scope
		wantErr string
	}{
		{"user", []string{"--user"}, install.ScopeUser, ""},
		{"local", []string{"--local"}, install.ScopeLocal, ""},
		{"both", []string{"--user", "--local"}, 0, "choose one"},
		{"missing", nil, 0, "must specify"},
		{"bad flag", []string{"--bad"}, 0, "flag provided"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseScope("test-scope", tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("scope = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseCodexScope(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    codexinstall.Scope
		wantErr string
	}{
		{"user", []string{"--user"}, codexinstall.ScopeUser, ""},
		{"project", []string{"--project"}, codexinstall.ScopeProject, ""},
		{"both", []string{"--user", "--project"}, 0, "choose one"},
		{"missing", nil, 0, "must specify"},
		{"bad flag", []string{"--bad"}, 0, "flag provided"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCodexScope("test-codex-scope", tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("scope = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRunInstallUninstallClaudeHooksLocal(t *testing.T) {
	cwd := t.TempDir()
	withTempCwd(t, cwd)

	if err := runInstallClaudeHooks([]string{"--local"}); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(cwd, ".claude", "settings.local.json")
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pre-tool-use", "post-tool-use", "pre-compact", "session-start"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("installed Claude settings missing %q:\n%s", want, raw)
		}
	}

	if err := runUninstallClaudeHooks([]string{"--local"}); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "splice-managed") {
		t.Fatalf("managed Claude hooks should be removed:\n%s", raw)
	}
}

func TestRunInstallUninstallCodexHooksProject(t *testing.T) {
	cwd := t.TempDir()
	withTempCwd(t, cwd)

	if err := runInstallCodexHooks([]string{"--project"}); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(cwd, ".codex", "config.toml")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"codex-pre-tool-use", "codex-post-tool-use", "codex-session-start"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("installed Codex config missing %q:\n%s", want, raw)
		}
	}

	if err := runUninstallCodexHooks([]string{"--project"}); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "splice-managed") {
		t.Fatalf("managed Codex hooks should be removed:\n%s", raw)
	}
}

func TestRunInstallCommandScopeErrors(t *testing.T) {
	if err := runInstallClaudeHooks(nil); err == nil || !strings.Contains(err.Error(), "must specify") {
		t.Fatalf("expected Claude install scope error, got %v", err)
	}
	if err := runUninstallClaudeHooks(nil); err == nil || !strings.Contains(err.Error(), "must specify") {
		t.Fatalf("expected Claude uninstall scope error, got %v", err)
	}
	if err := runInstallCodexHooks(nil); err == nil || !strings.Contains(err.Error(), "must specify") {
		t.Fatalf("expected Codex install scope error, got %v", err)
	}
	if err := runUninstallCodexHooks(nil); err == nil || !strings.Contains(err.Error(), "must specify") {
		t.Fatalf("expected Codex uninstall scope error, got %v", err)
	}
}

func TestEmitAllowHelpers(t *testing.T) {
	var claude bytes.Buffer
	emitAllow(&claude)
	var out hook.PreToolUseOutput
	if err := json.Unmarshal(claude.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.HookSpecificOutput.HookEventName != "PreToolUse" || out.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("bad Claude allow output: %+v", out)
	}

	var codex bytes.Buffer
	emitCodexAllow(&codex)
	var cOut hook.CodexPreToolUseOutput
	if err := json.Unmarshal(codex.Bytes(), &cOut); err != nil {
		t.Fatal(err)
	}
	if cOut.HookSpecificOutput.HookEventName != "PreToolUse" || cOut.HookSpecificOutput.Decision != nil {
		t.Fatalf("bad Codex allow output: %+v", cOut)
	}
}

func TestUsageAndExitHelpers(t *testing.T) {
	var stderr bytes.Buffer
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	usage()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stderr = oldStderr
	if _, err := io.Copy(&stderr, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "splice "+version) || !strings.Contains(stderr.String(), "install-codex-hooks") {
		t.Fatalf("usage output missing expected content:\n%s", stderr.String())
	}

	if got := runExitForTest(nil); got != 0 {
		t.Fatalf("exit(nil) code = %d, want 0", got)
	}
	if got := runExitForTest(fmt.Errorf("boom")); got != 1 {
		t.Fatalf("exit(error) code = %d, want 1", got)
	}
}

func TestSmallDecisionHelpers(t *testing.T) {
	if got := codexCanonicalToolName("shell"); got != "Bash" {
		t.Fatalf("shell canonical name = %q", got)
	}
	if got := codexCanonicalToolName("Read"); got != "Read" {
		t.Fatalf("non-shell canonical name = %q", got)
	}
	if got := pickLabel("", "Read", map[string]any{"file_path": "/tmp/a.go"}); got != "/tmp/a.go" {
		t.Fatalf("fallback label = %q", got)
	}
	if got := pickLabel("stored", "Read", map[string]any{"file_path": "/tmp/a.go"}); got != "stored" {
		t.Fatalf("stored label = %q", got)
	}
	if !bashIsFence("") || !bashIsFence("rm file") {
		t.Fatal("empty and rm Bash commands should fence")
	}
	if bashIsFence("npm test") {
		t.Fatal("npm test should not fence")
	}
	cfgFence := bashIsFenceWithConfig(config.Resolved{NeverCacheBashPatterns: []string{"./status"}})
	if cfgFence("./status") {
		t.Fatal("configured unknown status command should not fence")
	}
	if !cfgFence("rm file") {
		t.Fatal("known side-effect should fence even with config")
	}
}

func TestCodexPreCompactCacheHitAsks(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-CODEX-A"

	first := runCodexPreToolUseForTest(t, codexPreToolUseJSON(t, cwd, session, "npm test"))
	if first.HookSpecificOutput.Decision != nil || first.HookSpecificOutput.AdditionalContext != "" {
		t.Fatalf("first codex call should allow silently, got %+v", first.HookSpecificOutput)
	}
	runCodexPostToolUseForTest(t, codexPostToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0,
		"output":    "12 passed",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	hit := runCodexPreToolUseForTest(t, codexPreToolUseJSON(t, cwd, session, "npm test"))
	if hit.HookSpecificOutput.Decision == nil {
		t.Fatalf("expected codex ask decision, got %+v", hit.HookSpecificOutput)
	}
	if hit.HookSpecificOutput.Decision.Behavior != "ask" {
		t.Fatalf("expected codex ask, got %+v", hit.HookSpecificOutput.Decision)
	}
	if !strings.Contains(hit.HookSpecificOutput.Decision.Reason, "12 passed") {
		t.Fatalf("codex ask reason missing previous output: %q", hit.HookSpecificOutput.Decision.Reason)
	}
}

func TestCodexBypassModeAutoDegradesToDeny(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-CODEX-BYPASS"

	runCodexPreToolUseForTest(t, codexPreToolUseJSON(t, cwd, session, "npm test"))
	runCodexPostToolUseForTest(t, codexPostToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0,
		"output":    "all good",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runCodexPreToolUseForTest(t, codexPreToolUseJSONWithApproval(t, cwd, session, "npm test", "never"))
	if got.HookSpecificOutput.Decision == nil {
		t.Fatalf("expected codex deny decision, got %+v", got.HookSpecificOutput)
	}
	if got.HookSpecificOutput.Decision.Behavior != "deny" {
		t.Fatalf("expected codex deny in bypass mode, got %+v", got.HookSpecificOutput.Decision)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "all good") {
		t.Fatalf("codex deny should inject cached output, got %q", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestCodexTerminalHitInjectsFullPostCallTrail(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-CODEX-TRAIL"

	runCodexPreToolUseForTest(t, codexPreToolUseJSON(t, cwd, session, "npm test"))
	runCodexPostToolUseForTest(t, codexPostToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "12 passed",
	}))
	runCodexPreToolUseForTest(t, codexPreToolUseJSON(t, cwd, session, "git status --porcelain"))
	runCodexPostToolUseForTest(t, codexPostToolUseJSON(t, cwd, session, "git status --porcelain", map[string]any{
		"exit_code": 0, "output": "clean",
	}))
	runCodexPreToolUseForTest(t, codexPreToolUseJSON(t, cwd, session, "tail -n 20 sim.log"))
	runCodexPostToolUseForTest(t, codexPostToolUseJSON(t, cwd, session, "tail -n 20 sim.log", map[string]any{
		"exit_code": 0, "output": "progress=80%",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runCodexPreToolUseForTest(t, codexPreToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.Decision == nil || got.HookSpecificOutput.Decision.Behavior != "ask" {
		t.Fatalf("expected codex ask hit, got %+v", got.HookSpecificOutput)
	}
	ctx := got.HookSpecificOutput.AdditionalContext
	for _, want := range []string{"npm test", "12 passed", "git status --porcelain", "clean", "tail -n 20 sim.log", "progress=80%"} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("codex terminal context missing %q in:\n%s", want, ctx)
		}
	}
}

func TestCodexVolatileBashStatusQueryNotCached(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-CODEX-VOL"
	cmd := "tail -n 20 sim.log"

	runCodexPreToolUseForTest(t, codexPreToolUseJSON(t, cwd, session, cmd))
	runCodexPostToolUseForTest(t, codexPostToolUseJSON(t, cwd, session, cmd, map[string]any{
		"exit_code": 0,
		"output":    "old status",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runCodexPreToolUseForTest(t, codexPreToolUseJSON(t, cwd, session, cmd))
	if got.HookSpecificOutput.Decision != nil {
		t.Fatalf("codex volatile query must rerun, got %+v", got.HookSpecificOutput.Decision)
	}
	if got.HookSpecificOutput.AdditionalContext != "" {
		t.Fatalf("codex volatile query must not inject cached output, got %q", got.HookSpecificOutput.AdditionalContext)
	}
}

// After PreCompact, a repeat of the same (tool, args) without an Edit/Write/
// Bash fence afterwards must be intercepted via the ask path (default config).
func TestPreCompactCacheHitAsks(t *testing.T) {
	cwd := t.TempDir()

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-A", "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, "sess-A", "npm test", map[string]any{
		"exit_code": 0,
		"output":    "12 passed",
	}))

	runPreCompactForTest(t, preCompactJSON(t, cwd, "sess-A"))

	// Post-compact: same call → ask
	hit := runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-A", "npm test"))
	if hit.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("expected ask, got %+v", hit.HookSpecificOutput)
	}
	if !strings.Contains(hit.HookSpecificOutput.PermissionDecisionReason, "压缩后重复执行") {
		t.Fatalf("ask reason missing splice header: %q", hit.HookSpecificOutput.PermissionDecisionReason)
	}
	if !strings.Contains(hit.HookSpecificOutput.PermissionDecisionReason, "12 passed") {
		t.Fatalf("ask reason missing previous output: %q", hit.HookSpecificOutput.PermissionDecisionReason)
	}

	// Cooldown: second hit lets it through.
	cool := runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-A", "npm test"))
	if cool.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("cooldown should let second call through; got %+v", cool.HookSpecificOutput)
	}
}

func TestInterceptedTerminalHitDoesNotBecomeInFlightOnNextCompaction(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-INTERCEPT-PRESERVE"

	runPreToolUseForTest(t, preToolUseJSONWithToolUseID(t, cwd, session, "npm test", "first-call"))
	runPostToolUseForTest(t, postToolUseJSONWithToolUseID(t, cwd, session, "npm test", "first-call", map[string]any{
		"exit_code": 0, "output": "12 passed",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	hit := runPreToolUseForTest(t, preToolUseJSONWithToolUseID(t, cwd, session, "npm test", "intercepted-call"))
	if hit.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("expected terminal cache hit, got %+v", hit.HookSpecificOutput)
	}

	// Simulate user rejecting the duplicate execution: no PostToolUse arrives
	// for intercepted-call. The next compaction must not turn that internal
	// PreToolUse bookkeeping row into a stale in-flight warning.
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSONWithToolUseID(t, cwd, session, "npm test", "repeat-call"))
	if got.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("preserved terminal hit should ask again, got %+v", got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "12 passed") {
		t.Fatalf("preserved terminal output missing from context:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
	if strings.Contains(got.HookSpecificOutput.AdditionalContext, "上次启动") {
		t.Fatalf("intercepted terminal hit should not become in-flight:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestCodexInterceptedTerminalHitDoesNotBecomeInFlightOnNextCompaction(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-CODEX-INTERCEPT-PRESERVE"

	runCodexPreToolUseForTest(t, codexPreToolUseJSONWithToolUseID(t, cwd, session, "npm test", "first-call"))
	runCodexPostToolUseForTest(t, codexPostToolUseJSONWithToolUseID(t, cwd, session, "npm test", "first-call", map[string]any{
		"exit_code": 0, "output": "12 passed",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	hit := runCodexPreToolUseForTest(t, codexPreToolUseJSONWithToolUseID(t, cwd, session, "npm test", "intercepted-call"))
	if hit.HookSpecificOutput.Decision == nil || hit.HookSpecificOutput.Decision.Behavior != "ask" {
		t.Fatalf("expected Codex terminal cache hit, got %+v", hit.HookSpecificOutput)
	}

	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runCodexPreToolUseForTest(t, codexPreToolUseJSONWithToolUseID(t, cwd, session, "npm test", "repeat-call"))
	if got.HookSpecificOutput.Decision == nil || got.HookSpecificOutput.Decision.Behavior != "ask" {
		t.Fatalf("preserved Codex terminal hit should ask again, got %+v", got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "12 passed") {
		t.Fatalf("preserved Codex terminal output missing from context:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
	if strings.Contains(got.HookSpecificOutput.AdditionalContext, "上次启动") {
		t.Fatalf("Codex intercepted terminal hit should not become in-flight:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestRuntimeCooldownClearsOnNextCompaction(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-COOLDOWN-RESET"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "first result",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	firstHit := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if firstHit.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("expected first post-compact hit, got %+v", firstHit.HookSpecificOutput)
	}
	cooldownPass := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if cooldownPass.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("cooldown should allow immediate repeat, got %+v", cooldownPass.HookSpecificOutput)
	}
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "second result",
	}))

	runPreCompactForTest(t, preCompactJSON(t, cwd, session))
	nextWindowHit := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if nextWindowHit.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("new compaction should clear cooldown and intercept again, got %+v", nextWindowHit.HookSpecificOutput)
	}
	if !strings.Contains(nextWindowHit.HookSpecificOutput.AdditionalContext, "second result") {
		t.Fatalf("new compaction should use latest terminal result, got:\n%s", nextWindowHit.HookSpecificOutput.AdditionalContext)
	}
}

// After PreCompact, if Edit/Bash followed the cached call in the trail, the
// fence rule must let the repeat through.
func TestFenceLetsRepeatThrough(t *testing.T) {
	cwd := t.TempDir()

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-F", "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, "sess-F", "npm test", map[string]any{
		"exit_code": 0, "output": "ok",
	}))
	// Edit afterwards
	runPreToolUseForTest(t, preToolUseEditJSON(t, cwd, "sess-F", "/tmp/x.ts"))
	runPostToolUseForTest(t, postToolUseEditJSON(t, cwd, "sess-F", "/tmp/x.ts"))

	runPreCompactForTest(t, preCompactJSON(t, cwd, "sess-F"))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-F", "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("Edit fence should let through, got %+v", got.HookSpecificOutput)
	}
}

func TestFenceBeforeCandidateDoesNotInvalidateLaterHit(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-FENCE-BEFORE"

	runPreToolUseForTest(t, preToolUseEditJSON(t, cwd, session, "/tmp/before.ts"))
	runPostToolUseForTest(t, postToolUseEditJSON(t, cwd, session, "/tmp/before.ts"))
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "12 passed after edit",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("fence before candidate should not invalidate later terminal hit, got %+v", got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "12 passed after edit") {
		t.Fatalf("expected latest post-fence result in context, got:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
}

// A read-only Bash command after the cached call (e.g. ls / cat / git status)
// must NOT count as a fence — splice's most common scenario is the model
// running multiple read-only commands before compaction. If every Bash were
// a fence we'd never serve a cache hit in real workloads.
func TestReadOnlyBashAfterDoesNotFence(t *testing.T) {
	cwd := t.TempDir()

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-RB", "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, "sess-RB", "npm test", map[string]any{
		"exit_code": 0, "output": "12 passed",
	}))
	// A subsequent read-only Bash command — should not invalidate npm test cache.
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-RB", "git status --porcelain"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, "sess-RB", "git status --porcelain", map[string]any{
		"exit_code": 0, "output": "",
	}))
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-RB", "ls -la"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, "sess-RB", "ls -la", map[string]any{
		"exit_code": 0, "output": "total 0",
	}))

	runPreCompactForTest(t, preCompactJSON(t, cwd, "sess-RB"))

	// Repeating npm test should still hit cache despite ls/git-status sitting after it.
	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-RB", "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("read-only Bash should not fence; got %+v", got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.PermissionDecisionReason, "12 passed") {
		t.Fatalf("expected cached output in ask reason: %q", got.HookSpecificOutput.PermissionDecisionReason)
	}
	ctx := got.HookSpecificOutput.AdditionalContext
	for _, want := range []string{"12 passed", "git status --porcelain", "ls -la", "total 0", "压缩前历史结果"} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("expected terminal context to include %q, got:\n%s", want, ctx)
		}
	}
}

func TestTerminalHitInjectsFullPostCallTrail(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-TRAIL"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "12 passed",
	}))
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "git status --porcelain"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "git status --porcelain", map[string]any{
		"exit_code": 0, "output": "clean",
	}))
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "tail -n 20 sim.log"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "tail -n 20 sim.log", map[string]any{
		"exit_code": 0, "output": "progress=80%",
	}))
	runPreToolUseForTest(t, preToolUseGenericJSON(t, cwd, session, "Read", map[string]any{
		"file_path": "/tmp/notes.md",
	}))
	runPostToolUseForTest(t, postToolUseGenericJSON(t, cwd, session, "Read", map[string]any{
		"file_path": "/tmp/notes.md",
	}, map[string]any{"output": "next conclusion"}))

	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("expected ask hit, got %+v", got.HookSpecificOutput)
	}
	ctx := got.HookSpecificOutput.AdditionalContext
	for _, want := range []string{
		"重复调用：npm test",
		"12 passed",
		"git status --porcelain",
		"clean",
		"tail -n 20 sim.log",
		"progress=80%",
		"/tmp/notes.md",
		"next conclusion",
		"如需要当前状态应重新查询",
	} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("terminal context missing %q in:\n%s", want, ctx)
		}
	}
}

func TestTerminalHitTrailPreservesFailedAfterEventStatus(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-TRAIL-FAILED-AFTER"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "12 passed",
	}))
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "go test ./broken"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "go test ./broken", map[string]any{
		"exit_code": 1, "output": "FAIL ./broken",
	}))
	runPreToolUseForTest(t, preToolUseGenericJSON(t, cwd, session, "Read", map[string]any{
		"file_path": "/tmp/notes.md",
	}))
	runPostToolUseForTest(t, postToolUseGenericJSON(t, cwd, session, "Read", map[string]any{
		"file_path": "/tmp/notes.md",
	}, map[string]any{"output": "investigated failure"}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("expected terminal hit despite later failed read-only command, got %+v", got.HookSpecificOutput)
	}
	ctx := got.HookSpecificOutput.AdditionalContext
	for _, want := range []string{"go test ./broken", "→ exit 1", "[error]", "FAIL ./broken", "investigated failure"} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("terminal trail should preserve failed after-event marker %q in:\n%s", want, ctx)
		}
	}
}

// Volatile read-only commands describe live external state. They should not
// be served from cache after compaction because the external simulator/process
// may have advanced even if splice observed no local file edit.
func TestVolatileBashStatusQueryNotCached(t *testing.T) {
	cwd := t.TempDir()

	cases := []string{
		"tail -n 20 sim.log",
		"ps aux",
		"docker ps",
		"kubectl get pods",
		"nvidia-smi",
		"squeue -u me",
	}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			session := "sess-V-" + strings.Map(func(r rune) rune {
				if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
					return r
				}
				return '-'
			}, cmd)
			runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, cmd))
			runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, cmd, map[string]any{
				"exit_code": 0, "output": "old status",
			}))
			runPreCompactForTest(t, preCompactJSON(t, cwd, session))

			got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, cmd))
			if got.HookSpecificOutput.PermissionDecision != "" {
				t.Fatalf("volatile status query must rerun, got %+v", got.HookSpecificOutput)
			}
			if got.HookSpecificOutput.AdditionalContext != "" {
				t.Fatalf("volatile status query must not inject cached output, got %q", got.HookSpecificOutput.AdditionalContext)
			}
		})
	}
}

func TestConfiguredNeverCacheBashPatternNotCached(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(cwd+"/.splice", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cwd+"/.splice/config.json", []byte(`{"never_cache_bash_patterns": ["cat progress*"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	session := "sess-CONFIG-NEVER"
	cmd := "cat progress.txt"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, cmd))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, cmd, map[string]any{
		"exit_code": 0, "output": "old progress",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, cmd))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("configured never-cache Bash command must rerun, got %+v", got.HookSpecificOutput)
	}
	if got.HookSpecificOutput.AdditionalContext != "" {
		t.Fatalf("configured never-cache command must not inject cached output, got %q", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestConfiguredNeverCacheUnknownStatusDoesNotFenceStableHit(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(cwd+"/.splice", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cwd+"/.splice/config.json", []byte(`{"never_cache_bash_patterns": ["./check_sim_status"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	session := "sess-CONFIG-VOL-AFTER"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "12 passed",
	}))
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "./check_sim_status"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "./check_sim_status", map[string]any{
		"exit_code": 0, "output": "progress=80%",
	}))

	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("configured status query should not fence stable hit; got %+v", got.HookSpecificOutput)
	}
	ctx := got.HookSpecificOutput.AdditionalContext
	for _, want := range []string{"12 passed", "./check_sim_status", "progress=80%", "如需要当前状态应重新查询"} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("terminal context missing %q in:\n%s", want, ctx)
		}
	}
}

func TestConfiguredNeverCacheCannotUnfenceKnownSideEffect(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(cwd+"/.splice", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cwd+"/.splice/config.json", []byte(`{"never_cache_bash_patterns": ["rm *"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	session := "sess-CONFIG-SIDE-FENCE"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "12 passed",
	}))
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "rm scratch.txt"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "rm scratch.txt", map[string]any{
		"exit_code": 0, "output": "",
	}))

	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("known side-effect must fence even if pattern matches; got %+v", got.HookSpecificOutput)
	}
}

func TestConfiguredForceCacheUnknownStableCommand(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(cwd+"/.splice", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cwd+"/.splice/config.json", []byte(`{"force_cache_bash_patterns": ["./run_eval --suite stable"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	session := "sess-CONFIG-FORCE-CACHE"
	cmd := "./run_eval --suite stable"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, cmd))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, cmd, map[string]any{
		"exit_code": 0, "output": "eval score=0.91",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, cmd))
	if got.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("configured stable unknown command should cache-hit, got %+v", got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "eval score=0.91") {
		t.Fatalf("forced-cache context missing prior output:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestConfiguredForceFenceOverridesBuiltinReadOnly(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(cwd+"/.splice", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cwd+"/.splice/config.json", []byte(`{"force_fence_bash_patterns": ["pytest --update-snapshots"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	session := "sess-CONFIG-FORCE-FENCE"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "12 passed",
	}))
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "pytest --update-snapshots"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "pytest --update-snapshots", map[string]any{
		"exit_code": 0, "output": "snapshots updated",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("configured force-fence command must invalidate earlier stable hit, got %+v", got.HookSpecificOutput)
	}

	got = runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "pytest --update-snapshots"))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("configured force-fence command itself must not cache-hit, got %+v", got.HookSpecificOutput)
	}
}

func TestConfiguredForceFenceWinsOverForceCache(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(cwd+"/.splice", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cwd+"/.splice/config.json", []byte(`{
		"force_cache_bash_patterns": ["./project_tool"],
		"force_fence_bash_patterns": ["./project_tool"]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	session := "sess-CONFIG-FENCE-WINS"
	cmd := "./project_tool"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, cmd))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, cmd, map[string]any{
		"exit_code": 0, "output": "done",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, cmd))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("force-fence should win over force-cache, got %+v", got.HookSpecificOutput)
	}
}

func TestConfiguredForceCacheCannotOverrideKnownDangerousBash(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(cwd+"/.splice", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cwd+"/.splice/config.json", []byte(`{"force_cache_bash_patterns": ["rm *", "git status && rm *"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []string{"rm scratch.txt", "git status && rm scratch.txt"}
	for _, cmd := range cases {
		t.Run(cmd, func(t *testing.T) {
			session := "sess-CONFIG-DANGER-" + strings.ReplaceAll(strings.ReplaceAll(cmd, " ", "-"), "&", "and")
			runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, cmd))
			runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, cmd, map[string]any{
				"exit_code": 0, "output": "danger happened",
			}))
			runPreCompactForTest(t, preCompactJSON(t, cwd, session))

			got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, cmd))
			if got.HookSpecificOutput.PermissionDecision != "" {
				t.Fatalf("known dangerous Bash must not be force-cached, got %+v", got.HookSpecificOutput)
			}
		})
	}
}

func TestGlobalNeverCacheBashPatternNotCached(t *testing.T) {
	cwd := t.TempDir()
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(home+"/.splice", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home+"/.splice/config.json", []byte(`{"never_cache_bash_patterns": ["cat progress*"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	session := "sess-GLOBAL-NEVER"
	cmd := "cat progress.txt"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, cmd))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, cmd, map[string]any{
		"exit_code": 0, "output": "old progress",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, cmd))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("global never-cache Bash command must rerun, got %+v", got.HookSpecificOutput)
	}
}

func TestProjectlessConversationUsesGlobalConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".splice"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".splice", "config.json"), []byte(`{"ask_on_intercept": false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	session := "sess-PROJECTLESS-GLOBAL-CONFIG"

	runPreToolUseForTest(t, preToolUseJSON(t, "", session, "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, "", session, "npm test", map[string]any{
		"exit_code": 0, "output": "12 passed",
	}))
	runPreCompactForTest(t, preCompactJSON(t, "", session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, "", session, "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("projectless conversation should use global ask_on_intercept=false, got %+v", got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "12 passed") {
		t.Fatalf("projectless global-config hit missing cached context:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
}

// Volatile read-only commands should not fence unrelated stable cache hits.
// Example: after `npm test`, the model checks a live simulator status before
// compaction. Repeating `npm test` is still eligible for reuse.
func TestVolatileBashAfterDoesNotFenceStableHit(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-VOL-AFTER"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "12 passed",
	}))
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "tail -n 20 sim.log"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "tail -n 20 sim.log", map[string]any{
		"exit_code": 0, "output": "running",
	}))

	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("volatile read-only command should not fence stable hit; got %+v", got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.PermissionDecisionReason, "12 passed") {
		t.Fatalf("expected stable cached output in ask reason: %q", got.HookSpecificOutput.PermissionDecisionReason)
	}
}

// A side-effect Bash command after the cached call (git commit / rm /
// curl POST / npm install / etc.) MUST count as a fence — re-running the
// cached command after such a state-changing operation could legitimately
// produce different results.
func TestSideEffectBashAfterDoesFence(t *testing.T) {
	cwd := t.TempDir()

	cases := []struct {
		name string
		cmd  string
	}{
		{"git commit", `git commit -m "wip"`},
		{"rm", "rm -f /tmp/scratch"},
		{"npm install", "npm install lodash"},
		{"curl POST", `curl -X POST https://api.example.com/foo`},
		{"git push", "git push origin feature"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			session := "sess-FE-" + c.name
			runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
			runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
				"exit_code": 0, "output": "ok",
			}))
			// Side-effect Bash after — should fence.
			runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, c.cmd))
			runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, c.cmd, map[string]any{
				"exit_code": 0, "output": "",
			}))

			runPreCompactForTest(t, preCompactJSON(t, cwd, session))

			got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
			if got.HookSpecificOutput.PermissionDecision != "" {
				t.Fatalf("%s should fence; got %+v", c.name, got.HookSpecificOutput)
			}
		})
	}
}

// A compound Bash command that starts with a read-only prefix must still fence.
// Without this, `git status && rm ...` would inherit the read-only classification
// from `git status` and stale pre-compact results could be reused.
func TestCompoundBashAfterDoesFence(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-COMPOUND"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "12 passed",
	}))
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "git status && rm -rf tmp"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "git status && rm -rf tmp", map[string]any{
		"exit_code": 0, "output": "",
	}))

	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("compound Bash command must fence; got %+v", got.HookSpecificOutput)
	}
}

// Mixed sequence: read-only Bash, then a side-effect Bash, then more read-only.
// The side-effect entry must trigger the fence regardless of subsequent read-only.
func TestMixedBashFenceTriggersOnce(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-MIX"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "ok",
	}))
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "ls"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "ls", map[string]any{
		"exit_code": 0, "output": "",
	}))
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "rm /tmp/x"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "rm /tmp/x", map[string]any{
		"exit_code": 0, "output": "",
	}))
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "cat README.md"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "cat README.md", map[string]any{
		"exit_code": 0, "output": "...",
	}))

	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("rm in trail must fence the npm test cache; got %+v", got.HookSpecificOutput)
	}
}

// Failed result (exit != 0) in pre-compact trail → must NOT be served as cache.
func TestFailedResultNotCached(t *testing.T) {
	cwd := t.TempDir()

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-X", "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, "sess-X", "npm test", map[string]any{
		"exit_code": 1, "output": "3 tests failed",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, "sess-X"))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-X", "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("failed result must not be cached; got %+v", got.HookSpecificOutput)
	}
}

// Interrupted result must not be served as cache either.
func TestInterruptedResultNotCached(t *testing.T) {
	cwd := t.TempDir()

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-I", "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, "sess-I", "npm test", map[string]any{
		"interrupted": true, "output": "...",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, "sess-I"))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-I", "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("interrupted result must not be cached; got %+v", got.HookSpecificOutput)
	}
}

func TestTimeoutResultNotCached(t *testing.T) {
	cwd := t.TempDir()

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-T", "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, "sess-T", "npm test", map[string]any{
		"timed_out": true, "output": "...",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, "sess-T"))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-T", "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("timeout result must not be cached; got %+v", got.HookSpecificOutput)
	}
}

// Config: ask_on_intercept=false → use deny + injection instead of ask.
func TestConfigAskOff(t *testing.T) {
	cwd := t.TempDir()
	// Project-local config disabling ask.
	if err := os.MkdirAll(cwd+"/.splice", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cwd+"/.splice/config.json", []byte(`{"ask_on_intercept": false}`), 0o644); err != nil {
		t.Fatal(err)
	}

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-C", "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, "sess-C", "npm test", map[string]any{
		"exit_code": 0, "output": "all good",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, "sess-C"))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-C", "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("expected deny, got %+v", got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "all good") {
		t.Fatalf("deny path should inject cached output, got %q", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestNeverCachePatternInvalidGlobFallsBackToContains(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(cwd+"/.splice", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cwd+"/.splice/config.json", []byte(`{"never_cache_bash_patterns": ["[progress"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	session := "sess-CONFIG-BAD-GLOB"
	cmd := "cat [progress.txt"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, cmd))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, cmd, map[string]any{
		"exit_code": 0, "output": "old progress",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, cmd))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("invalid glob pattern should fall back to contains and rerun, got %+v", got.HookSpecificOutput)
	}
}

func TestMalformedConfigFallsBackSafely(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(cwd+"/.splice", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cwd+"/.splice/config.json", []byte(`{not valid json`), 0o644); err != nil {
		t.Fatal(err)
	}
	session := "sess-MALFORMED-CONFIG"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "12 passed",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("malformed config should fall back to default ask, got %+v", got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "12 passed") {
		t.Fatalf("fallback hit should still inject context, got %q", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestRuntimeSnapshotEvictionDropsOldTrailAfterNoHits(t *testing.T) {
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".splice"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".splice", "config.json"), []byte(`{"snapshot_eviction_after": 2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	session := "sess-RUNTIME-EVICT"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "12 passed",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	// Two unrelated no-hit calls mean the model has probably moved away from
	// the compacted topic. The runtime hook should drop the old snapshot.
	for _, cmd := range []string{"go test ./pkg/a", "go test ./pkg/b"} {
		got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, cmd))
		if got.HookSpecificOutput.PermissionDecision != "" || got.HookSpecificOutput.AdditionalContext != "" {
			t.Fatalf("unrelated no-hit %q should be allowed silently, got %+v", cmd, got.HookSpecificOutput)
		}
	}

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("old snapshot should be evicted after no-hit threshold, got %+v", got.HookSpecificOutput)
	}
	if got.HookSpecificOutput.AdditionalContext != "" {
		t.Fatalf("evicted snapshot should not inject stale context, got %q", got.HookSpecificOutput.AdditionalContext)
	}
}

// Cross-session isolation: sess-B does not pick up sess-A's frozen trail.
func TestCrossSessionIsolation(t *testing.T) {
	cwd := t.TempDir()

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-A", "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, "sess-A", "npm test", map[string]any{
		"exit_code": 0, "output": "ok",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, "sess-A"))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-B", "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("sess-B should not see sess-A trail; got %+v", got.HookSpecificOutput)
	}
}

func TestCliSingleTerminalSingleSessionFlow(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-cli-single-terminal"

	runSessionStartForTest(t, sessionStartJSON(t, cwd, session, "startup"))
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "single terminal ok",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("single-terminal CLI session should recover duplicate command, got %+v", got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "single terminal ok") {
		t.Fatalf("single-terminal cached context missing prior output:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestCliMultipleTerminalsSameProjectDifferentSessions(t *testing.T) {
	cwd := t.TempDir()

	runSessionStartForTest(t, sessionStartJSON(t, cwd, "sess-cli-terminal-a", "startup"))
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-cli-terminal-a", "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, "sess-cli-terminal-a", "npm test", map[string]any{
		"exit_code": 0, "output": "terminal A result",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, "sess-cli-terminal-a"))

	runSessionStartForTest(t, sessionStartJSON(t, cwd, "sess-cli-terminal-b", "startup"))
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-cli-terminal-b", "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, "sess-cli-terminal-b", "npm test", map[string]any{
		"exit_code": 0, "output": "terminal B result",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, "sess-cli-terminal-b"))

	gotA := runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-cli-terminal-a", "npm test"))
	if gotA.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("terminal A should see its own snapshot, got %+v", gotA.HookSpecificOutput)
	}
	if !strings.Contains(gotA.HookSpecificOutput.AdditionalContext, "terminal A result") ||
		strings.Contains(gotA.HookSpecificOutput.AdditionalContext, "terminal B result") {
		t.Fatalf("terminal A context crossed session boundary:\n%s", gotA.HookSpecificOutput.AdditionalContext)
	}

	gotB := runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-cli-terminal-b", "npm test"))
	if gotB.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("terminal B should see its own snapshot, got %+v", gotB.HookSpecificOutput)
	}
	if !strings.Contains(gotB.HookSpecificOutput.AdditionalContext, "terminal B result") ||
		strings.Contains(gotB.HookSpecificOutput.AdditionalContext, "terminal A result") {
		t.Fatalf("terminal B context crossed session boundary:\n%s", gotB.HookSpecificOutput.AdditionalContext)
	}
}

func TestClearDropsSessionTrail(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-CLEAR"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "12 passed",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	beforeClear := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if beforeClear.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("expected cache hit before clear, got %+v", beforeClear.HookSpecificOutput)
	}

	runSessionStartForTest(t, sessionStartJSON(t, cwd, session, "clear"))

	afterClear := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if afterClear.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("clear should drop stale trail, got %+v", afterClear.HookSpecificOutput)
	}
	if afterClear.HookSpecificOutput.AdditionalContext != "" {
		t.Fatalf("clear should not inject stale context, got %q", afterClear.HookSpecificOutput.AdditionalContext)
	}
}

func TestClearDropsLatePostToolUseWithoutPending(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-CLEAR-LATE-POST"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runSessionStartForTest(t, sessionStartJSON(t, cwd, session, "clear"))

	// This PostToolUse belongs to the pre-clear command. /clear removed its
	// pending sidecar, so recording it would resurrect stale memory.
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "old result after clear",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("late pre-clear PostToolUse should not restore cache, got %+v", got.HookSpecificOutput)
	}
	if got.HookSpecificOutput.AdditionalContext != "" {
		t.Fatalf("late pre-clear PostToolUse should not inject stale context, got:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestClearDropsHashOnlyPostToolUseEvenWithNewPending(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-CLEAR-NEW-POST"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runSessionStartForTest(t, sessionStartJSON(t, cwd, session, "clear"))

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "ambiguous result after clear",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("hash-only post-clear command is ambiguous and should not cache-hit, got %+v", got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "压缩前未拿到完成结果") {
		t.Fatalf("hash-only post-clear command should degrade to in-flight notice, got:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
	if strings.Contains(got.HookSpecificOutput.AdditionalContext, "ambiguous result after clear") {
		t.Fatalf("hash-only post-clear command should not inject ambiguous output:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestClearAllowsNewPostToolUseWithOfficialID(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-CLEAR-NEW-POST-ID"

	runPreToolUseForTest(t, preToolUseJSONWithToolUseID(t, cwd, session, "npm test", "old-call"))
	runSessionStartForTest(t, sessionStartJSON(t, cwd, session, "clear"))

	runPreToolUseForTest(t, preToolUseJSONWithToolUseID(t, cwd, session, "npm test", "new-call"))
	runPostToolUseForTest(t, postToolUseJSONWithToolUseID(t, cwd, session, "npm test", "new-call", map[string]any{
		"exit_code": 0, "output": "new result after clear",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSONWithToolUseID(t, cwd, session, "npm test", "repeat-call"))
	if got.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("new post-clear command with official ID should remain cacheable, got %+v", got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "new result after clear") {
		t.Fatalf("post-clear result missing from context:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestClearPreventsOldToolUseIDPostFromClaimingNewSameHashPending(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-CLEAR-ID-RACE"

	runPreToolUseForTest(t, preToolUseJSONWithToolUseID(t, cwd, session, "npm test", "old-call"))
	runSessionStartForTest(t, sessionStartJSON(t, cwd, session, "clear"))
	runPreToolUseForTest(t, preToolUseJSONWithToolUseID(t, cwd, session, "npm test", "new-call"))

	// Same tool+args as the new post-clear call, but a different official
	// tool_use_id. This must not finish the new running row.
	runPostToolUseForTest(t, postToolUseJSONWithToolUseID(t, cwd, session, "npm test", "old-call", map[string]any{
		"exit_code": 0, "output": "old result after clear",
	}))
	runPostToolUseForTest(t, postToolUseJSONWithToolUseID(t, cwd, session, "npm test", "new-call", map[string]any{
		"exit_code": 0, "output": "new result after clear",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSONWithToolUseID(t, cwd, session, "npm test", "repeat-call"))
	if got.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("new post-clear result should cache-hit, got %+v", got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "new result after clear") {
		t.Fatalf("new post-clear result missing from context:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
	if strings.Contains(got.HookSpecificOutput.AdditionalContext, "old result after clear") {
		t.Fatalf("old pre-clear result claimed new pending:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestPostToolUseWithToolUseIDAfterCompactSupersedesInFlightSnapshot(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-ID-LATE-POST"

	runPreToolUseForTest(t, preToolUseJSONWithToolUseID(t, cwd, session, "npm test", "long-call"))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	runPostToolUseForTest(t, postToolUseJSONWithToolUseID(t, cwd, session, "npm test", "long-call", map[string]any{
		"exit_code": 0, "output": "12 passed after compact",
	}))

	got := runPreToolUseForTest(t, preToolUseJSONWithToolUseID(t, cwd, session, "npm test", "repeat-call"))
	if got.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("official tool_use_id should recover late terminal result, got %+v", got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "12 passed after compact") {
		t.Fatalf("late terminal result missing from context:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
	if strings.Contains(got.HookSpecificOutput.AdditionalContext, "上次启动") {
		t.Fatalf("should not emit in-flight warning after ID-paired terminal result:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestPostToolUseWithToolUseIDAfterCompactHonorsFenceBeforeResult(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-ID-LATE-FENCE"

	runPreToolUseForTest(t, preToolUseJSONWithToolUseID(t, cwd, session, "npm test", "long-call"))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	runPreToolUseForTest(t, preToolUseEditJSON(t, cwd, session, "/tmp/fence.ts"))
	runPostToolUseForTest(t, postToolUseEditJSON(t, cwd, session, "/tmp/fence.ts"))
	runPostToolUseForTest(t, postToolUseJSONWithToolUseID(t, cwd, session, "npm test", "long-call", map[string]any{
		"exit_code": 0, "output": "12 passed after fence",
	}))

	got := runPreToolUseForTest(t, preToolUseJSONWithToolUseID(t, cwd, session, "npm test", "repeat-call"))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("fence before ID-paired late result should allow rerun, got %+v", got.HookSpecificOutput)
	}
	if got.HookSpecificOutput.AdditionalContext != "" {
		t.Fatalf("fenced late result should not inject context, got:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestCodexClearPreventsOldToolUseIDPostFromClaimingNewSameHashPending(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-CODEX-CLEAR-ID-RACE"

	runCodexPreToolUseForTest(t, codexPreToolUseJSONWithToolUseID(t, cwd, session, "npm test", "old-call"))
	runSessionStartForTest(t, sessionStartJSON(t, cwd, session, "clear"))
	runCodexPreToolUseForTest(t, codexPreToolUseJSONWithToolUseID(t, cwd, session, "npm test", "new-call"))

	runCodexPostToolUseForTest(t, codexPostToolUseJSONWithToolUseID(t, cwd, session, "npm test", "old-call", map[string]any{
		"exit_code": 0, "output": "old codex result after clear",
	}))
	runCodexPostToolUseForTest(t, codexPostToolUseJSONWithToolUseID(t, cwd, session, "npm test", "new-call", map[string]any{
		"exit_code": 0, "output": "new codex result after clear",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runCodexPreToolUseForTest(t, codexPreToolUseJSONWithToolUseID(t, cwd, session, "npm test", "repeat-call"))
	if got.HookSpecificOutput.Decision == nil || got.HookSpecificOutput.Decision.Behavior != "ask" {
		t.Fatalf("new post-clear Codex result should cache-hit, got %+v", got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "new codex result after clear") {
		t.Fatalf("new Codex post-clear result missing from context:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
	if strings.Contains(got.HookSpecificOutput.AdditionalContext, "old codex result after clear") {
		t.Fatalf("old Codex pre-clear result claimed new pending:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestCodexPostToolUseWithToolUseIDAfterCompactSupersedesInFlightSnapshot(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-CODEX-ID-LATE-POST"

	runCodexPreToolUseForTest(t, codexPreToolUseJSONWithToolUseID(t, cwd, session, "npm test", "long-call"))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	runCodexPostToolUseForTest(t, codexPostToolUseJSONWithToolUseID(t, cwd, session, "npm test", "long-call", map[string]any{
		"exit_code": 0, "output": "12 passed after compact",
	}))

	got := runCodexPreToolUseForTest(t, codexPreToolUseJSONWithToolUseID(t, cwd, session, "npm test", "repeat-call"))
	if got.HookSpecificOutput.Decision == nil || got.HookSpecificOutput.Decision.Behavior != "ask" {
		t.Fatalf("official Codex tool_use_id should recover late terminal result, got %+v", got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "12 passed after compact") {
		t.Fatalf("late Codex terminal result missing from context:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
	if strings.Contains(got.HookSpecificOutput.AdditionalContext, "上次启动") {
		t.Fatalf("Codex should not emit in-flight warning after ID-paired terminal result:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestSessionMarkerAndPendingFilesArePrivate(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-PRIVATE"

	runSessionStartForTest(t, sessionStartJSON(t, cwd, session, "startup"))
	markerPath := filepath.Join(store.ActiveSessionsDir(), store.SessionFileBase(session)+".json")
	raw, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	var marker SessionMarker
	if err := json.Unmarshal(raw, &marker); err != nil {
		t.Fatal(err)
	}
	if marker.SessionID != session || marker.Cwd != cwd {
		t.Fatalf("bad marker: %+v", marker)
	}

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	pendingEntries, err := os.ReadDir(pendingDir(session))
	if err != nil {
		t.Fatal(err)
	}
	if len(pendingEntries) != 1 {
		t.Fatalf("expected one pending sidecar, got %d", len(pendingEntries))
	}

	if runtime.GOOS == "windows" {
		return
	}
	assertMode(t, store.ActiveSessionsDir(), 0o700)
	assertMode(t, markerPath, 0o600)
	assertMode(t, pendingDir(session), 0o700)
	assertMode(t, filepath.Join(pendingDir(session), pendingEntries[0].Name()), 0o600)
}

func TestCodexSessionStartUsesGlobalMarkerForProjectlessDesktopChat(t *testing.T) {
	session := "sess-CODEX-PROJECTLESS"

	runCodexSessionStartForTest(t, sessionStartJSON(t, "", session, "startup"))

	markerPath := filepath.Join(store.ActiveSessionsDir(), store.SessionFileBase(session)+".json")
	raw, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	var marker SessionMarker
	if err := json.Unmarshal(raw, &marker); err != nil {
		t.Fatal(err)
	}
	if marker.SessionID != session || marker.Cwd != "" {
		t.Fatalf("bad projectless marker: %+v", marker)
	}
	metaPath := store.SessionMetaPath(session)
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("projectless session meta should be written globally: %v", err)
	}
}

func TestCodexClearWritesRolloutOffsetBarrier(t *testing.T) {
	t.Setenv("SPLICE_NO_AUTO_WATCH", "1")
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	cwd := t.TempDir()
	session := "sess-CODEX-CLEAR-OFFSET"
	rolloutDir := filepath.Join(codexHome, "sessions", "2026", "05", "22")
	if err := os.MkdirAll(rolloutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(rolloutDir, "rollout-2026-05-22-"+session+".jsonl")
	rollout := `{"timestamp":"2026-05-22T10:00:00Z","type":"session_meta","payload":{"session_id":"sess-CODEX-CLEAR-OFFSET","cwd":"/x"}}
{"timestamp":"2026-05-22T10:00:01Z","type":"context_compaction"}
`
	if err := os.WriteFile(rolloutPath, []byte(rollout), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(rolloutPath)
	if err != nil {
		t.Fatal(err)
	}

	runCodexPreToolUseForTest(t, codexPreToolUseJSON(t, cwd, session, "npm test"))
	runCodexPostToolUseForTest(t, codexPostToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "old should disappear",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))
	runCodexSessionStartForTest(t, sessionStartJSON(t, cwd, session, "clear"))

	st, err := store.OpenSession(session)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	off, err := st.LastRolloutOffset()
	if err != nil {
		t.Fatal(err)
	}
	if off != info.Size() {
		t.Fatalf("clear should persist rollout offset barrier %d, got %d", info.Size(), off)
	}
	cursorHash, err := st.LastRolloutCursorHash()
	if err != nil {
		t.Fatal(err)
	}
	if cursorHash == "" {
		t.Fatal("clear should persist rollout cursor hash barrier")
	}
	got := runCodexPreToolUseForTest(t, codexPreToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.Decision != nil || got.HookSpecificOutput.AdditionalContext != "" {
		t.Fatalf("clear should remove old cache hit, got %+v", got.HookSpecificOutput)
	}
}

// In bypassPermissions mode the host swallows "ask", so splice must
// auto-degrade to deny + injection to keep the post-compact protection.
func TestBypassModeAutoDegradesToDeny(t *testing.T) {
	cwd := t.TempDir()

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, "sess-Y", "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, "sess-Y", "npm test", map[string]any{
		"exit_code": 0, "output": "12 passed",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, "sess-Y"))

	bypass := preToolUseJSONWithMode(t, cwd, "sess-Y", "npm test", "bypassPermissions")
	got := runPreToolUseForTest(t, bypass)
	if got.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("expected deny in bypass mode, got %+v", got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.PermissionDecisionReason, "bypass mode detected") {
		t.Fatalf("expected bypass note in reason: %q", got.HookSpecificOutput.PermissionDecisionReason)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "12 passed") {
		t.Fatalf("expected cached output in additionalContext: %q", got.HookSpecificOutput.AdditionalContext)
	}
}

// In-flight: PreToolUse fires (writes a running row) but PostToolUse never
// arrives before PreCompact. After compaction, model re-launches the same
// command. With nothing happening between the launch and PreCompact, splice
// emits a notify-only response telling the model the prior task may still
// be running.
func TestInFlightAloneEmitsNotify(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-IF1"

	// PreToolUse fires; PostToolUse never comes (simulates long task crossing compact).
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm run dev"))

	// Compaction happens with the running row still in live_trail.
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm run dev"))
	// In-flight: no permission decision, just additionalContext informing the model.
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("in-flight must not deny/ask; got decision=%q",
			got.HookSpecificOutput.PermissionDecision)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "上次启动") {
		t.Fatalf("expected in-flight notice; got %q",
			got.HookSpecificOutput.AdditionalContext)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "未观察到其他工具调用") {
		t.Fatalf("expected empty after_P branch text; got %q",
			got.HookSpecificOutput.AdditionalContext)
	}
}

// In-flight with subsequent calls: model launches a long task, does work
// after, then compaction hits. Re-launch must include the causal chain in
// the injected context so the model sees what was achieved while the
// long task was running.
func TestInFlightWithFollowupEmitsCausalChain(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-IF2"

	// Long task starts (PreToolUse only — no PostToolUse).
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "docker compose up"))

	// Subsequent fully-completed calls.
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "git status"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "git status", map[string]any{
		"exit_code": 0, "output": "clean",
	}))
	runPreToolUseForTest(t, preToolUseEditJSON(t, cwd, session, "/tmp/main.go"))
	runPostToolUseForTest(t, postToolUseEditJSON(t, cwd, session, "/tmp/main.go"))
	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "go test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "go test", map[string]any{
		"exit_code": 0, "output": "ok    foo  1.234s",
	}))

	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "docker compose up"))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("in-flight must be notify-only; got decision=%q",
			got.HookSpecificOutput.PermissionDecision)
	}
	ctx := got.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "上次启动") {
		t.Fatalf("missing in-flight prefix: %q", ctx)
	}
	if !strings.Contains(ctx, "依次发生了") {
		t.Fatalf("expected non-empty after_P branch; got %q", ctx)
	}
	// At least the followup commands' tool names must be visible.
	for _, want := range []string{"git status", "Edit", "go test"} {
		if !strings.Contains(ctx, want) {
			t.Errorf("causal chain missing %q in:\n%s", want, ctx)
		}
	}
}

func TestPostToolUseAfterCompactSupersedesInFlightSnapshot(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-IF-POST-AFTER"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	// The long task completes after compaction. PostToolUse falls back to
	// appending a terminal live row because FreezePreCompact moved the running
	// row into the snapshot.
	runPreToolUseForTest(t, preToolUseGenericJSON(t, cwd, session, "Read", map[string]any{
		"file_path": "/tmp/before-result.txt",
	}))
	runPostToolUseForTest(t, postToolUseGenericJSON(t, cwd, session, "Read", map[string]any{
		"file_path": "/tmp/before-result.txt",
	}, map[string]any{"output": "note before result"}))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "12 passed after compact",
	}))
	runPreToolUseForTest(t, preToolUseGenericJSON(t, cwd, session, "Read", map[string]any{
		"file_path": "/tmp/after.txt",
	}))
	runPostToolUseForTest(t, postToolUseGenericJSON(t, cwd, session, "Read", map[string]any{
		"file_path": "/tmp/after.txt",
	}, map[string]any{"output": "post compact note"}))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("completed post-compact result should ask as terminal hit, got %+v", got.HookSpecificOutput)
	}
	ctx := got.HookSpecificOutput.AdditionalContext
	for _, want := range []string{"12 passed after compact", "/tmp/before-result.txt", "note before result", "/tmp/after.txt", "post compact note"} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("terminal context missing %q in:\n%s", want, ctx)
		}
	}
	if strings.Contains(ctx, "上次启动") {
		t.Fatalf("should not emit in-flight warning after terminal PostToolUse arrived:\n%s", ctx)
	}
}

func TestPostToolUseAfterCompactHonorsFenceBeforeResult(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-IF-POST-AFTER-FENCE-BEFORE"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	runPreToolUseForTest(t, preToolUseEditJSON(t, cwd, session, "/tmp/a.go"))
	runPostToolUseForTest(t, postToolUseEditJSON(t, cwd, session, "/tmp/a.go"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "12 passed before edit mattered",
	}))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("fence before late result should allow rerun, got %+v", got.HookSpecificOutput)
	}
	if got.HookSpecificOutput.AdditionalContext != "" {
		t.Fatalf("fenced late result should not inject stale context, got:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestCodexPreToolUseConsumesWatcherRecoveredLateResult(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-CODEX-WATCHER-LATE"
	canonical, hash, err := fingerprintForCodexTest("Bash", map[string]any{"command": "npm test"}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSession(session)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Minute)
	if err := st.BeginLiveTrailEntry(store.TrailEntry{
		CallID:     "codex-rollout:c-long",
		ToolName:   "Bash",
		ArgsHash:   hash,
		ArgsJSON:   canonical,
		Label:      "npm test",
		Status:     store.StatusRunning,
		RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendTerminalFromFrozenRunning(
		"codex-rollout:c-long",
		sql.NullInt64{Int64: 0, Valid: true},
		"12 passed after watcher restart",
		store.StatusOK,
		now.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLiveTrail(store.TrailEntry{
		ToolName: "Read", ArgsHash: "h-read",
		ArgsJSON: `{"args":{"file_path":"/tmp/after.md"},"tool":"Read"}`,
		Label:    "/tmp/after.md", Status: store.StatusOK, Output: "post compact note",
		RecordedAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	got := runCodexPreToolUseForTest(t, codexPreToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.Decision == nil || got.HookSpecificOutput.Decision.Behavior != "ask" {
		t.Fatalf("expected Codex ask from watcher-recovered terminal result, got %+v", got.HookSpecificOutput)
	}
	ctx := got.HookSpecificOutput.AdditionalContext
	for _, want := range []string{"12 passed after watcher restart", "/tmp/after.md", "post compact note"} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("watcher recovered context missing %q in:\n%s", want, ctx)
		}
	}
	if strings.Contains(ctx, "上次启动") {
		t.Fatalf("should not emit stale in-flight warning after recovered terminal result:\n%s", ctx)
	}
}

func TestSameSessionDifferentProjectDoesNotReuseScopedBashResult(t *testing.T) {
	projectA := t.TempDir()
	projectB := t.TempDir()
	session := "sess-DESKTOP-PROJECT-SWITCH-SCOPE"

	runCodexPreToolUseForTest(t, codexPreToolUseJSON(t, projectA, session, "npm test"))
	runCodexPostToolUseForTest(t, codexPostToolUseJSON(t, projectA, session, "npm test", map[string]any{
		"exit_code": 0, "output": "project A result",
	}))
	runPreCompactForTest(t, preCompactJSON(t, projectA, session))

	gotB := runCodexPreToolUseForTest(t, codexPreToolUseJSON(t, projectB, session, "npm test"))
	if gotB.HookSpecificOutput.Decision != nil {
		t.Fatalf("same command in a different project must not reuse project A result, got %+v", gotB.HookSpecificOutput)
	}
	if strings.Contains(gotB.HookSpecificOutput.AdditionalContext, "project A result") {
		t.Fatalf("different-project repeat leaked old result:\n%s", gotB.HookSpecificOutput.AdditionalContext)
	}

	gotA := runCodexPreToolUseForTest(t, codexPreToolUseJSON(t, projectA, session, "npm test"))
	if gotA.HookSpecificOutput.Decision == nil || gotA.HookSpecificOutput.Decision.Behavior != "ask" {
		t.Fatalf("same project should still reuse the scoped result, got %+v", gotA.HookSpecificOutput)
	}
	if !strings.Contains(gotA.HookSpecificOutput.AdditionalContext, "project A result") {
		t.Fatalf("same-project result missing from context:\n%s", gotA.HookSpecificOutput.AdditionalContext)
	}
}

func TestCodexHookAndWatcherDoubleSourcePrefersTerminalResult(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-CODEX-DOUBLE-SOURCE"

	// Codex hook fallback recorded the terminal result before compaction.
	runCodexPreToolUseForTest(t, codexPreToolUseJSON(t, cwd, session, "npm test"))
	runCodexPostToolUseForTest(t, codexPostToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "hook terminal result",
	}))

	// The watcher also saw the rollout call but not its result yet. This can
	// happen around compaction boundaries when both Codex hooks and codex-watch
	// are enabled. The later running duplicate must not downgrade the same
	// repeated operation to an in-flight warning after a terminal result exists.
	canonical, hash, err := fingerprintForCodexTest("Bash", map[string]any{"command": "npm test"}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSession(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.BeginLiveTrailEntry(store.TrailEntry{
		CallID: "codex-rollout:c-long", ToolName: "Bash",
		ArgsHash: hash, ArgsJSON: canonical, Label: "npm test",
		Status: store.StatusRunning, RecordedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runCodexPreToolUseForTest(t, codexPreToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.Decision == nil || got.HookSpecificOutput.Decision.Behavior != "ask" {
		t.Fatalf("expected terminal result to win over duplicate running row, got %+v", got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "hook terminal result") {
		t.Fatalf("expected terminal output in context, got:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
	if strings.Contains(got.HookSpecificOutput.AdditionalContext, "上次启动") {
		t.Fatalf("duplicate running row must not produce stale in-flight warning:\n%s", got.HookSpecificOutput.AdditionalContext)
	}
}

func TestCodexDoubleSourceSkipsDuplicateRunningButKeepsLaterContext(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-CODEX-DOUBLE-SOURCE-AFTER"

	runCodexPreToolUseForTest(t, codexPreToolUseJSON(t, cwd, session, "npm test"))
	runCodexPostToolUseForTest(t, codexPostToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "hook terminal result",
	}))

	canonical, hash, err := fingerprintForCodexTest("Bash", map[string]any{"command": "npm test"}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSession(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.BeginLiveTrailEntry(store.TrailEntry{
		CallID: "codex-rollout:dup", ToolName: "Bash",
		ArgsHash: hash, ArgsJSON: canonical, Label: "npm test",
		Status: store.StatusRunning, RecordedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLiveTrail(store.TrailEntry{
		ToolName: "Read", ArgsHash: "h-read",
		ArgsJSON: `{"args":{"file_path":"/tmp/after.md"},"tool":"Read"}`,
		Label:    "/tmp/after.md", Status: store.StatusOK, Output: "post duplicate note",
		RecordedAt: time.Now().Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runCodexPreToolUseForTest(t, codexPreToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.Decision == nil || got.HookSpecificOutput.Decision.Behavior != "ask" {
		t.Fatalf("expected terminal result to win over duplicate running row, got %+v", got.HookSpecificOutput)
	}
	ctx := got.HookSpecificOutput.AdditionalContext
	for _, want := range []string{"hook terminal result", "以下 1 个工具事件", "/tmp/after.md", "post duplicate note"} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("terminal context missing %q in:\n%s", want, ctx)
		}
	}
	if strings.Contains(ctx, "上次启动") {
		t.Fatalf("duplicate running row must not produce stale in-flight warning:\n%s", ctx)
	}
}

// In-flight no longer applies once PostToolUse fires before PreCompact —
// that case becomes a normal terminal cache hit (or fence miss).
func TestInFlightFinishesNormallyBecomesTerminalHit(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-IF3"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0, "output": "12 passed",
	}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("Begin+Finish should yield terminal cache hit (ask); got %+v",
			got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.PermissionDecisionReason, "12 passed") {
		t.Fatalf("expected normal cache reason: %q", got.HookSpecificOutput.PermissionDecisionReason)
	}
}

// Cacheable tools (Read/Grep/Glob) hit cache the same way Bash does — the
// fingerprint pipeline is tool-agnostic. These tests exercise the actual
// args layout each tool uses so a future schema drift in tool_input shape
// would be caught.
func TestReadCacheHit(t *testing.T) {
	cwd := t.TempDir()

	runPreToolUseForTest(t, preToolUseGenericJSON(t, cwd, "sess-R", "Read", map[string]any{
		"file_path": "/tmp/notes.md",
	}))
	runPostToolUseForTest(t, postToolUseGenericJSON(t, cwd, "sess-R", "Read", map[string]any{
		"file_path": "/tmp/notes.md",
	}, map[string]any{"output": "hello world"}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, "sess-R"))

	got := runPreToolUseForTest(t, preToolUseGenericJSON(t, cwd, "sess-R", "Read", map[string]any{
		"file_path": "/tmp/notes.md",
	}))
	if got.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("Read repeat should ask, got %+v", got.HookSpecificOutput)
	}
	if !strings.Contains(got.HookSpecificOutput.PermissionDecisionReason, "hello world") {
		t.Fatalf("ask reason missing cached output: %q", got.HookSpecificOutput.PermissionDecisionReason)
	}
}

func TestGrepCacheHit(t *testing.T) {
	cwd := t.TempDir()

	runPreToolUseForTest(t, preToolUseGenericJSON(t, cwd, "sess-G", "Grep", map[string]any{
		"pattern": "func TestFoo",
		"path":    "./internal",
	}))
	runPostToolUseForTest(t, postToolUseGenericJSON(t, cwd, "sess-G", "Grep", map[string]any{
		"pattern": "func TestFoo",
		"path":    "./internal",
	}, map[string]any{"output": "internal/foo/foo_test.go:42"}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, "sess-G"))

	got := runPreToolUseForTest(t, preToolUseGenericJSON(t, cwd, "sess-G", "Grep", map[string]any{
		"pattern": "func TestFoo",
		"path":    "./internal",
	}))
	if got.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("Grep repeat should ask, got %+v", got.HookSpecificOutput)
	}
}

func TestGlobCacheHit(t *testing.T) {
	cwd := t.TempDir()

	runPreToolUseForTest(t, preToolUseGenericJSON(t, cwd, "sess-Gl", "Glob", map[string]any{
		"pattern": "**/*.go",
	}))
	runPostToolUseForTest(t, postToolUseGenericJSON(t, cwd, "sess-Gl", "Glob", map[string]any{
		"pattern": "**/*.go",
	}, map[string]any{"output": "internal/foo/foo.go\ninternal/bar/bar.go"}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, "sess-Gl"))

	got := runPreToolUseForTest(t, preToolUseGenericJSON(t, cwd, "sess-Gl", "Glob", map[string]any{
		"pattern": "**/*.go",
	}))
	if got.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("Glob repeat should ask, got %+v", got.HookSpecificOutput)
	}
}

// Non-cacheable tools never get cached output served. Edit/Write mutate
// state; WebFetch/WebSearch see remote state splice can't observe.
//
// The cooldown set is per-(session, hash); each subtest uses its own session
// to avoid cross-test contamination.
func TestNonCacheableToolsAlwaysAllow(t *testing.T) {
	cases := []struct {
		name     string
		toolName string
		input    map[string]any
		response map[string]any
	}{
		{
			name:     "Edit",
			toolName: "Edit",
			input: map[string]any{
				"file_path": "/tmp/x.ts", "old_string": "a", "new_string": "b",
			},
			response: map[string]any{},
		},
		{
			name:     "Write",
			toolName: "Write",
			input: map[string]any{
				"file_path": "/tmp/y.ts", "content": "console.log('x')",
			},
			response: map[string]any{},
		},
		{
			name:     "NotebookEdit",
			toolName: "NotebookEdit",
			input: map[string]any{
				"notebook_path": "/tmp/n.ipynb", "new_source": "print(1)",
			},
			response: map[string]any{},
		},
		{
			name:     "WebFetch",
			toolName: "WebFetch",
			input: map[string]any{
				"url": "https://api.weather.com/now", "prompt": "extract temp",
			},
			response: map[string]any{"output": "晴 25°C"},
		},
		{
			name:     "WebSearch",
			toolName: "WebSearch",
			input: map[string]any{
				"query": "latest go release",
			},
			response: map[string]any{"output": "Go 1.24"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cwd := t.TempDir()
			session := "sess-" + c.name

			runPreToolUseForTest(t, preToolUseGenericJSON(t, cwd, session, c.toolName, c.input))
			runPostToolUseForTest(t, postToolUseGenericJSON(t, cwd, session, c.toolName, c.input, c.response))
			runPreCompactForTest(t, preCompactJSON(t, cwd, session))

			got := runPreToolUseForTest(t, preToolUseGenericJSON(t, cwd, session, c.toolName, c.input))
			if got.HookSpecificOutput.PermissionDecision != "" {
				t.Fatalf("%s should never cache-hit; got %+v", c.name, got.HookSpecificOutput)
			}
			if got.HookSpecificOutput.AdditionalContext != "" {
				t.Fatalf("%s should not inject context; got %q", c.name, got.HookSpecificOutput.AdditionalContext)
			}
		})
	}
}

func TestUnknownToolAfterStableCandidateFencesRuntimeHit(t *testing.T) {
	cwd := t.TempDir()
	session := "sess-UNKNOWN-TOOL-FENCE"

	runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	runPostToolUseForTest(t, postToolUseJSON(t, cwd, session, "npm test", map[string]any{
		"exit_code": 0,
		"output":    "ok",
	}))
	runPreToolUseForTest(t, preToolUseGenericJSON(t, cwd, session, "mcp__db__query", map[string]any{
		"sql": "select refresh_materialized_view()",
	}))
	runPostToolUseForTest(t, postToolUseGenericJSON(t, cwd, session, "mcp__db__query", map[string]any{
		"sql": "select refresh_materialized_view()",
	}, map[string]any{"output": "refreshed"}))
	runPreCompactForTest(t, preCompactJSON(t, cwd, session))

	got := runPreToolUseForTest(t, preToolUseJSON(t, cwd, session, "npm test"))
	if got.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("unknown tool after candidate should fence and allow repeat, got %+v", got.HookSpecificOutput)
	}
	if got.HookSpecificOutput.AdditionalContext != "" {
		t.Fatalf("unknown tool fence should not inject stale context, got %q", got.HookSpecificOutput.AdditionalContext)
	}
}

func runPreToolUseForTest(t *testing.T, input string) hook.PreToolUseOutput {
	t.Helper()
	out, err := runHookForTest(input, runPreToolUse)
	if err != nil {
		t.Fatalf("runPreToolUse: %v", err)
	}
	var parsed hook.PreToolUseOutput
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("decode pre output %q: %v", out, err)
	}
	return parsed
}

func runCodexPreToolUseForTest(t *testing.T, input string) hook.CodexPreToolUseOutput {
	t.Helper()
	out, err := runHookForTest(input, runCodexPreToolUse)
	if err != nil {
		t.Fatalf("runCodexPreToolUse: %v", err)
	}
	var parsed hook.CodexPreToolUseOutput
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("decode codex pre output %q: %v", out, err)
	}
	return parsed
}

func runPostToolUseForTest(t *testing.T, input string) {
	t.Helper()
	out, err := runHookForTest(input, runPostToolUse)
	if err != nil {
		t.Fatalf("runPostToolUse: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("post-tool-use should not emit stdout, got %q", out)
	}
}

func runCodexPostToolUseForTest(t *testing.T, input string) {
	t.Helper()
	out, err := runHookForTest(input, runCodexPostToolUse)
	if err != nil {
		t.Fatalf("runCodexPostToolUse: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("codex-post-tool-use should not emit stdout, got %q", out)
	}
}

func runPreCompactForTest(t *testing.T, input string) {
	t.Helper()
	out, err := runHookForTest(input, runPreCompact)
	if err != nil {
		t.Fatalf("runPreCompact: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("pre-compact should not emit stdout, got %q", out)
	}
}

func runSessionStartForTest(t *testing.T, input string) {
	t.Helper()
	out, err := runHookForTest(input, runSessionStart)
	if err != nil {
		t.Fatalf("runSessionStart: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("session-start should not emit stdout, got %q", out)
	}
}

func runCodexSessionStartForTest(t *testing.T, input string) {
	t.Helper()
	out, err := runHookForTest(input, runCodexSessionStart)
	if err != nil {
		t.Fatalf("runCodexSessionStart: %v", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("codex-session-start should not emit stdout, got %q", out)
	}
}

func preCompactJSON(t *testing.T, cwd, sessionID string) string {
	t.Helper()
	return marshalJSON(t, map[string]any{
		"session_id":      sessionID,
		"hook_event_name": "PreCompact",
		"cwd":             cwd,
		"trigger":         "auto",
	})
}

func sessionStartJSON(t *testing.T, cwd, sessionID, source string) string {
	t.Helper()
	return marshalJSON(t, map[string]any{
		"session_id":      sessionID,
		"hook_event_name": "SessionStart",
		"cwd":             cwd,
		"source":          source,
	})
}

func preToolUseEditJSON(t *testing.T, cwd, sessionID, filePath string) string {
	t.Helper()
	return marshalJSON(t, map[string]any{
		"session_id":      sessionID,
		"hook_event_name": "PreToolUse",
		"cwd":             cwd,
		"tool_name":       "Edit",
		"tool_input": map[string]any{
			"file_path":  filePath,
			"old_string": "a",
			"new_string": "b",
		},
	})
}

func postToolUseEditJSON(t *testing.T, cwd, sessionID, filePath string) string {
	t.Helper()
	return marshalJSON(t, map[string]any{
		"session_id":      sessionID,
		"hook_event_name": "PostToolUse",
		"cwd":             cwd,
		"tool_name":       "Edit",
		"tool_input": map[string]any{
			"file_path":  filePath,
			"old_string": "a",
			"new_string": "b",
		},
		"tool_response": map[string]any{},
	})
}

func runHookForTest(input string, fn func() error) (string, error) {
	oldStdin := os.Stdin
	oldStdout := os.Stdout

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return "", err
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		return "", err
	}

	if _, err := io.WriteString(stdinW, input); err != nil {
		return "", err
	}
	if err := stdinW.Close(); err != nil {
		return "", err
	}

	os.Stdin = stdinR
	os.Stdout = stdoutW
	err = fn()
	_ = stdoutW.Close()
	out, readErr := io.ReadAll(stdoutR)

	os.Stdin = oldStdin
	os.Stdout = oldStdout
	_ = stdinR.Close()
	_ = stdoutR.Close()

	if readErr != nil {
		return "", readErr
	}
	return string(out), err
}

func preToolUseJSON(t *testing.T, cwd, sessionID, command string) string {
	t.Helper()
	return marshalJSON(t, map[string]any{
		"session_id":      sessionID,
		"hook_event_name": "PreToolUse",
		"cwd":             cwd,
		"tool_name":       "Bash",
		"tool_input": map[string]any{
			"command": command,
		},
	})
}

func preToolUseJSONWithToolUseID(t *testing.T, cwd, sessionID, command, toolUseID string) string {
	t.Helper()
	payload := map[string]any{
		"session_id":      sessionID,
		"hook_event_name": "PreToolUse",
		"cwd":             cwd,
		"tool_name":       "Bash",
		"tool_input": map[string]any{
			"command": command,
		},
	}
	if toolUseID != "" {
		payload["tool_use_id"] = toolUseID
	}
	return marshalJSON(t, payload)
}

func preToolUseJSONWithMode(t *testing.T, cwd, sessionID, command, mode string) string {
	t.Helper()
	return marshalJSON(t, map[string]any{
		"session_id":      sessionID,
		"hook_event_name": "PreToolUse",
		"cwd":             cwd,
		"tool_name":       "Bash",
		"permission_mode": mode,
		"tool_input": map[string]any{
			"command": command,
		},
	})
}

func codexPreToolUseJSON(t *testing.T, cwd, sessionID, command string) string {
	t.Helper()
	return codexPreToolUseJSONWithApprovalAndToolUseID(t, cwd, sessionID, command, "", "")
}

func codexPreToolUseJSONWithApproval(t *testing.T, cwd, sessionID, command, approvalMode string) string {
	t.Helper()
	return codexPreToolUseJSONWithApprovalAndToolUseID(t, cwd, sessionID, command, approvalMode, "")
}

func codexPreToolUseJSONWithToolUseID(t *testing.T, cwd, sessionID, command, toolUseID string) string {
	t.Helper()
	return codexPreToolUseJSONWithApprovalAndToolUseID(t, cwd, sessionID, command, "", toolUseID)
}

func codexPreToolUseJSONWithApprovalAndToolUseID(t *testing.T, cwd, sessionID, command, approvalMode, toolUseID string) string {
	t.Helper()
	payload := map[string]any{
		"session_id":      sessionID,
		"hook_event_name": "PreToolUse",
		"cwd":             cwd,
		"tool_name":       "shell",
		"tool_input": map[string]any{
			"command": command,
		},
	}
	if approvalMode != "" {
		payload["approval_mode"] = approvalMode
	}
	if toolUseID != "" {
		payload["tool_use_id"] = toolUseID
	}
	return marshalJSON(t, payload)
}

func postToolUseJSON(t *testing.T, cwd, sessionID, command string, response map[string]any) string {
	t.Helper()
	return marshalJSON(t, map[string]any{
		"session_id":      sessionID,
		"hook_event_name": "PostToolUse",
		"cwd":             cwd,
		"tool_name":       "Bash",
		"tool_input": map[string]any{
			"command": command,
		},
		"tool_response": response,
	})
}

func postToolUseJSONWithToolUseID(t *testing.T, cwd, sessionID, command, toolUseID string, response map[string]any) string {
	t.Helper()
	payload := map[string]any{
		"session_id":      sessionID,
		"hook_event_name": "PostToolUse",
		"cwd":             cwd,
		"tool_name":       "Bash",
		"tool_input": map[string]any{
			"command": command,
		},
		"tool_response": response,
	}
	if toolUseID != "" {
		payload["tool_use_id"] = toolUseID
	}
	return marshalJSON(t, payload)
}

func codexPostToolUseJSON(t *testing.T, cwd, sessionID, command string, response map[string]any) string {
	t.Helper()
	return codexPostToolUseJSONWithToolUseID(t, cwd, sessionID, command, "", response)
}

func codexPostToolUseJSONWithToolUseID(t *testing.T, cwd, sessionID, command, toolUseID string, response map[string]any) string {
	t.Helper()
	payload := map[string]any{
		"session_id":      sessionID,
		"hook_event_name": "PostToolUse",
		"cwd":             cwd,
		"tool_name":       "shell",
		"tool_input": map[string]any{
			"command": command,
		},
		"tool_response": response,
	}
	if toolUseID != "" {
		payload["tool_use_id"] = toolUseID
	}
	return marshalJSON(t, payload)
}

// preToolUseGenericJSON / postToolUseGenericJSON are tool-agnostic versions
// for testing tools other than Bash.
func preToolUseGenericJSON(t *testing.T, cwd, sessionID, toolName string, input map[string]any) string {
	t.Helper()
	return marshalJSON(t, map[string]any{
		"session_id":      sessionID,
		"hook_event_name": "PreToolUse",
		"cwd":             cwd,
		"tool_name":       toolName,
		"tool_input":      input,
	})
}

func postToolUseGenericJSON(t *testing.T, cwd, sessionID, toolName string, input, response map[string]any) string {
	t.Helper()
	return marshalJSON(t, map[string]any{
		"session_id":      sessionID,
		"hook_event_name": "PostToolUse",
		"cwd":             cwd,
		"tool_name":       toolName,
		"tool_input":      input,
		"tool_response":   response,
	})
}

func marshalJSON(t *testing.T, payload map[string]any) string {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func fingerprintForCodexTest(toolName string, input map[string]any, cwd string) (string, string, error) {
	return fingerprint.ComputeScoped(toolName, input, store.ProjectKey(cwd))
}

func withTempCwd(t *testing.T, cwd string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})
}
