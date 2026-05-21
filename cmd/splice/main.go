// splice — context-compaction safety net for Claude Code.
//
// Subcommands:
//
//	splice pre-tool-use            reads PreToolUse stdin, emits hook output
//	splice post-tool-use           reads PostToolUse stdin, records the tool result
//	splice pre-compact             reads PreCompact stdin, freezes the trail
//	splice post-compact            reads PostCompact stdin (no-op for now)
//	splice install-claude-hooks    register splice in Claude Code's settings.json
//	splice uninstall-claude-hooks  remove splice's entries from settings.json
//	splice version                 prints version
//
// Storage: per-session SQLite WAL databases under
// <workspace>/.splice/sessions/<session_id>.db.
package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wang33550/splice/internal/classify"
	"github.com/wang33550/splice/internal/codex"
	"github.com/wang33550/splice/internal/codexinstall"
	"github.com/wang33550/splice/internal/config"
	"github.com/wang33550/splice/internal/fingerprint"
	"github.com/wang33550/splice/internal/hook"
	"github.com/wang33550/splice/internal/inject"
	"github.com/wang33550/splice/internal/install"
	"github.com/wang33550/splice/internal/store"
)

const version = "0.5.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "pre-tool-use":
		// Graceful-fail: any internal error → ALLOW + warn on stderr, never block.
		// We buffer stdout so a partial write inside runPreToolUse never bleeds
		// out alongside the fallback emitAllow JSON — that would corrupt the
		// hook protocol (two JSON objects on stdout).
		buf := &bytes.Buffer{}
		err := runPreToolUseWith(buf)
		if err != nil {
			fmt.Fprintln(os.Stderr, "splice: warn:", err)
			emitAllow(os.Stdout)
		} else {
			_, _ = io.Copy(os.Stdout, buf)
		}
	case "codex-pre-tool-use":
		buf := &bytes.Buffer{}
		err := runCodexPreToolUseWith(buf)
		if err != nil {
			fmt.Fprintln(os.Stderr, "splice: warn:", err)
			emitCodexAllow(os.Stdout)
		} else {
			_, _ = io.Copy(os.Stdout, buf)
		}
	case "post-tool-use":
		if err := runPostToolUse(); err != nil {
			fmt.Fprintln(os.Stderr, "splice: warn:", err)
		}
	case "codex-post-tool-use":
		// Watcher-offline fallback: records the result against the same
		// canonical (Bash, hash) used by codex-pre-tool-use so the entry
		// attaches correctly regardless of watcher state.
		if err := runCodexPostToolUse(); err != nil {
			fmt.Fprintln(os.Stderr, "splice: warn:", err)
		}
	case "codex-watch":
		exit(runCodexWatch(os.Args[2:]))
	case "pre-compact":
		if err := runPreCompact(); err != nil {
			fmt.Fprintln(os.Stderr, "splice: warn:", err)
		}
	case "post-compact":
		// No state-mutating work for v0.3. Read stdin to satisfy Claude Code, exit clean.
		_, _ = io.ReadAll(os.Stdin)
	case "session-start":
		if err := runSessionStart(); err != nil {
			fmt.Fprintln(os.Stderr, "splice: warn:", err)
		}
	case "install-claude-hooks":
		exit(runInstallClaudeHooks(os.Args[2:]))
	case "uninstall-claude-hooks":
		exit(runUninstallClaudeHooks(os.Args[2:]))
	case "install-codex-hooks":
		exit(runInstallCodexHooks(os.Args[2:]))
	case "uninstall-codex-hooks":
		exit(runUninstallCodexHooks(os.Args[2:]))
	case "version", "--version", "-v":
		fmt.Println("splice", version)
	case "help", "--help", "-h":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `splice %s — splice across context compaction

usage:
  splice pre-tool-use            < hook-stdin.json   > hook-stdout.json
  splice post-tool-use           < hook-stdin.json
  splice codex-pre-tool-use      < hook-stdin.json   > hook-stdout.json
  splice codex-post-tool-use     < hook-stdin.json
  splice codex-watch             [--cwd <dir>] [--poll-ms <ms>]
  splice pre-compact             < hook-stdin.json
  splice post-compact            < hook-stdin.json
  splice session-start           < hook-stdin.json
  splice install-claude-hooks    [--user | --local]
  splice uninstall-claude-hooks  [--user | --local]
  splice install-codex-hooks     [--user | --project]
  splice uninstall-codex-hooks   [--user | --project]
  splice version
`, version)
}

func exit(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "splice:", err)
		os.Exit(1)
	}
}

func emitAllow(w io.Writer) {
	out := hook.PreToolUseOutput{
		HookSpecificOutput: hook.PreToolUseHookOutput{HookEventName: "PreToolUse"},
	}
	_ = writeJSON(w, out)
}

func emitCodexAllow(w io.Writer) {
	out := hook.CodexPreToolUseOutput{
		HookSpecificOutput: hook.CodexPreToolUseHookOutput{HookEventName: "PreToolUse"},
	}
	_ = writeJSON(w, out)
}

// runCodexPreToolUse mirrors runPreToolUse but emits the Codex-shaped
// hook response (decision: { behavior: "ask"|"deny" }). It uses the same
// fingerprint.Compute as Claude PreToolUse so trails populated by
// codex-watch (which routes through codex.FingerprintToolCall →
// fingerprint.Compute) hash-match runtime hook decisions byte-for-byte.
//
// shell→Bash folding happens once here and once in codex.FingerprintToolCall;
// the canonical name in the store is always "Bash".
// runCodexPreToolUse keeps the old signature for tests; runCodexPreToolUseWith
// is what main uses behind a stdout buffer.
func runCodexPreToolUse() error {
	return runCodexPreToolUseWith(os.Stdout)
}

func runCodexPreToolUseWith(stdout io.Writer) error {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	var in hook.PreToolUseInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("decode stdin: %w", err)
	}

	out := hook.CodexPreToolUseOutput{
		HookSpecificOutput: hook.CodexPreToolUseHookOutput{HookEventName: "PreToolUse"},
	}

	canonName := codexCanonicalToolName(in.ToolName)
	canonical, hash, err := fingerprint.Compute(canonName, in.ToolInput)
	if err != nil {
		return fmt.Errorf("fingerprint: %w", err)
	}

	st, err := store.OpenSession(in.Cwd, in.SessionID)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	// Record in-flight live_trail row so codex-post-tool-use (the watcher-
	// offline fallback) can attach a result. Modern hooks carry tool_use_id,
	// so bind the row directly to that identity. The hash sidecar is only a
	// legacy fallback for id-less hosts.
	hostToolUseID := in.HostToolUseID()
	callID := hookCallID(hostToolUseID)
	if callID == "" {
		callID = newID()
	}
	now := time.Now()
	if err := st.BeginLiveTrailEntry(store.TrailEntry{
		CallID:     callID,
		ToolName:   canonName,
		ArgsHash:   hash,
		ArgsJSON:   canonical,
		Label:      hook.LabelFor(canonName, in.ToolInput),
		Status:     store.StatusRunning,
		RecordedAt: now,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "splice: warn: begin live_trail:", err)
	}
	if hostToolUseID == "" {
		if err := writePendingID(in.Cwd, in.SessionID, hash, callID); err != nil {
			fmt.Fprintln(os.Stderr, "splice: warn: stash pending id:", err)
		}
	} else {
		clearPendingID(in.Cwd, in.SessionID, hash)
	}

	cfg, err := config.Load(in.Cwd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "splice: warn: load config:", err)
		cfg.AskOnIntercept = true
		cfg.SnapshotEvictionAfter = config.DefaultSnapshotEvictionAfter
	}

	// Same decision flow as Claude PreToolUse:
	hasTrail, err := st.HasPreCompactTrail()
	if err != nil {
		return fmt.Errorf("trail check: %w", err)
	}
	if !hasTrail {
		return writeJSON(stdout, out)
	}
	cooled, err := st.IsInCooldown(hash)
	if err != nil {
		return fmt.Errorf("cooldown check: %w", err)
	}
	if cooled {
		return writeJSON(stdout, out)
	}
	hit, err := st.LookupCachedHit(hash, bashIsFenceWithConfig(cfg), bashCacheableFn(cfg))
	if err != nil {
		return fmt.Errorf("trail lookup: %w", err)
	}
	if hit == nil {
		// No cache hit — bump the eviction counter; if it crosses the
		// configured threshold, drop the snapshot entirely (the model
		// has clearly moved on).
		maybeEvictSnapshot(st, cfg.SnapshotEvictionAfter)
		return writeJSON(stdout, out)
	}

	// Cache hit (terminal or in-flight): reset the eviction counter.
	if err := st.ResetEviction(); err != nil {
		fmt.Fprintln(os.Stderr, "splice: warn: reset eviction:", err)
	}
	// Cooldown is added only for terminal hits — in-flight is informational
	// and may need to fire again on the next attempt if the model still hasn't
	// gotten clear signal that the prior task ended.
	if !hit.InFlight {
		if err := st.AddCooldown(hash); err != nil {
			fmt.Fprintln(os.Stderr, "splice: warn: cooldown add:", err)
		}
		if err := preserveInterceptedTerminalHit(st, callID, hit.Entry); err != nil {
			fmt.Fprintln(os.Stderr, "splice: warn: preserve cache hit:", err)
		}
		clearPendingID(in.Cwd, in.SessionID, hash)
	}
	emitCodexHitDecision(&out, hit, in, cfg.AskOnIntercept)
	return writeJSON(stdout, out)
}

// runCodexPostToolUse is the watcher-offline fallback. It finishes the
// in-flight live_trail row created by codex-pre-tool-use (looked up via
// the sidecar call_id), or — if the sidecar is missing — appends a fresh
// terminal row so the trail still captures the event.
func runCodexPostToolUse() error {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	var in hook.PostToolUseInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("decode stdin: %w", err)
	}

	canonName := codexCanonicalToolName(in.ToolName)
	canonical, hash, err := fingerprint.Compute(canonName, in.ToolInput)
	if err != nil {
		return fmt.Errorf("fingerprint: %w", err)
	}

	st, err := store.OpenSession(in.Cwd, in.SessionID)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	exitCode := hook.ExtractExitCode(in.ToolResponse)
	output := hook.ExtractOutput(in.ToolResponse)
	status := hook.ExtractStatus(in.ToolResponse, exitCode)
	label := hook.LabelFor(canonName, in.ToolInput)

	var ec sql.NullInt64
	if exitCode != nil {
		ec = sql.NullInt64{Int64: int64(*exitCode), Valid: true}
	}

	callID, pairedByID := callIDFromPost(in)
	if pairedByID {
		if finished, err := finishPostToolUseResult(st, callID, ec, output, status, time.Now()); err != nil {
			return err
		} else if finished {
			return nil
		}
		if err := st.AppendTerminalFromFrozenRunning(callID, ec, output, status, time.Now()); err == nil {
			return nil
		} else if !store.IsErrNoRunningRow(err) {
			return fmt.Errorf("append frozen terminal: %w", err)
		}
		if hasClearBarrier(in.Cwd, in.SessionID) {
			return nil
		}
	} else {
		if hasClearBarrier(in.Cwd, in.SessionID) {
			clearPendingID(in.Cwd, in.SessionID, hash)
			return nil
		}
		callID, _ = readPendingID(in.Cwd, in.SessionID, hash)
		clearPendingID(in.Cwd, in.SessionID, hash)
	}

	if callID != "" {
		if finished, err := finishPostToolUseResult(st, callID, ec, output, status, time.Now()); err != nil {
			return err
		} else if finished {
			return nil
		}
		// Running row vanished (e.g. cleared / freezed / never begun) —
		// fall through to Append so the result is still captured.
	}

	if err := st.AppendLiveTrail(store.TrailEntry{
		CallID:     callID, // may be "" — only used for Begin/Finish pairing
		ToolName:   canonName,
		ArgsHash:   hash,
		ArgsJSON:   canonical,
		Label:      label,
		ExitCode:   ec,
		Output:     output,
		Status:     status,
		RecordedAt: time.Now(),
	}); err != nil {
		return fmt.Errorf("append live_trail: %w", err)
	}
	return nil
}

// codexCanonicalToolName folds Codex's "shell" alias to "Bash" so trail
// entries from rollout files (via codex-watch) and runtime hooks share the
// same tool_name index.
func codexCanonicalToolName(name string) string {
	if name == "shell" {
		return "Bash"
	}
	return name
}

func runCodexWatch(args []string) error {
	fs := flag.NewFlagSet("codex-watch", flag.ContinueOnError)
	cwd := fs.String("cwd", "", "workspace cwd to watch (default: current directory)")
	pollMs := fs.Int("poll-ms", 1000, "marker scan interval in milliseconds")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("codex-watch: %w", err)
	}
	if *cwd == "" {
		var err error
		*cwd, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("codex-watch: resolve cwd: %w", err)
		}
	}

	w, err := codex.New(*cwd)
	if err != nil {
		return err
	}
	defer w.Close()
	w.PollInterval = time.Duration(*pollMs) * time.Millisecond
	w.Logger = codex.CompactStdoutLogger(os.Stderr)

	ctx, cancel := signalContext()
	defer cancel()
	return w.Run(ctx)
}

// signalContext returns a context that's canceled on SIGINT / SIGTERM so
// the watcher exits cleanly when the user Ctrl+C's it.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}

// ---------------------------------------------------------------------
// PreToolUse: the v0.3 decision flow.
// ---------------------------------------------------------------------

// runPreToolUse keeps the old signature (writes to os.Stdout) for tests
// that exercise the pipeline directly. The buffered version
// runPreToolUseWith is what main() uses so a mid-flight error doesn't
// produce two JSON documents on stdout.
func runPreToolUse() error {
	return runPreToolUseWith(os.Stdout)
}

func runPreToolUseWith(stdout io.Writer) error {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	var in hook.PreToolUseInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("decode stdin: %w", err)
	}

	out := hook.PreToolUseOutput{
		HookSpecificOutput: hook.PreToolUseHookOutput{HookEventName: "PreToolUse"},
	}

	canonical, hash, err := fingerprint.Compute(in.ToolName, in.ToolInput)
	if err != nil {
		return fmt.Errorf("fingerprint: %w", err)
	}

	st, err := store.OpenSession(in.Cwd, in.SessionID)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	// Begin the in-flight live_trail row. Modern hooks carry tool_use_id,
	// so bind the row directly to that identity. The hash sidecar is only a
	// legacy fallback for id-less hosts.
	hostToolUseID := in.HostToolUseID()
	callID := hookCallID(hostToolUseID)
	if callID == "" {
		callID = newID()
	}
	now := time.Now()
	if err := st.BeginLiveTrailEntry(store.TrailEntry{
		CallID:     callID,
		ToolName:   in.ToolName,
		ArgsHash:   hash,
		ArgsJSON:   canonical,
		Label:      hook.LabelFor(in.ToolName, in.ToolInput),
		Status:     store.StatusRunning,
		RecordedAt: now,
	}); err != nil {
		// in-flight row is best-effort; don't block the tool call on store hiccup.
		fmt.Fprintln(os.Stderr, "splice: warn: begin live_trail:", err)
	}
	if hostToolUseID == "" {
		if err := writePendingID(in.Cwd, in.SessionID, hash, callID); err != nil {
			fmt.Fprintln(os.Stderr, "splice: warn: stash pending id:", err)
		}
	} else {
		clearPendingID(in.Cwd, in.SessionID, hash)
	}

	cfg, err := config.Load(in.Cwd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "splice: warn: load config:", err)
		cfg.AskOnIntercept = true
		cfg.SnapshotEvictionAfter = config.DefaultSnapshotEvictionAfter
	}

	// 1. No frozen trail for this session → splice has nothing to enforce yet.
	hasTrail, err := st.HasPreCompactTrail()
	if err != nil {
		return fmt.Errorf("trail check: %w", err)
	}
	if !hasTrail {
		return writeJSON(stdout, out)
	}

	// 2. Cooldown: if we already intercepted this hash this window, let it through.
	cooled, err := st.IsInCooldown(hash)
	if err != nil {
		return fmt.Errorf("cooldown check: %w", err)
	}
	if cooled {
		return writeJSON(stdout, out)
	}

	// 3. Look up pre-compact trail entry. LookupCachedHit handles status/exit/fence.
	hit, err := st.LookupCachedHit(hash, bashIsFenceWithConfig(cfg), bashCacheableFn(cfg))
	if err != nil {
		return fmt.Errorf("trail lookup: %w", err)
	}
	if hit == nil {
		// No cache hit — bump eviction; if we cross the threshold, drop
		// the snapshot. This is the "model has moved on" signal.
		maybeEvictSnapshot(st, cfg.SnapshotEvictionAfter)
		return writeJSON(stdout, out)
	}

	// We have a hit. Reset eviction (the snapshot is still useful).
	if err := st.ResetEviction(); err != nil {
		fmt.Fprintln(os.Stderr, "splice: warn: reset eviction:", err)
	}
	if !hit.InFlight {
		if err := st.AddCooldown(hash); err != nil {
			// non-fatal: cooldown best-effort
			fmt.Fprintln(os.Stderr, "splice: warn: cooldown add:", err)
		}
		if err := preserveInterceptedTerminalHit(st, callID, hit.Entry); err != nil {
			fmt.Fprintln(os.Stderr, "splice: warn: preserve cache hit:", err)
		}
		clearPendingID(in.Cwd, in.SessionID, hash)
	}
	emitHitDecision(&out, hit, in, cfg.AskOnIntercept)
	return writeJSON(stdout, out)
}

func pickLabel(stored, toolName string, toolInput map[string]any) string {
	if stored != "" {
		return stored
	}
	return hook.LabelFor(toolName, toolInput)
}

// bashIsFence returns true when a Bash command should invalidate prior cache
// entries. Side-effect commands (git push / rm / npm install / curl POST /
// kubectl apply / etc.) fence; read-only commands (npm test / cat / grep /
// git status) do not. Unknown commands fence — safer to re-run than to
// inject potentially-stale cache.
//
// Empty command (label was not captured) also fences, on the same logic.
func bashIsFence(command string) bool {
	if command == "" {
		return true
	}
	return classify.ClassifyBash(command) == classify.PolicySideEffect
}

func bashIsFenceWithConfig(cfg config.Resolved) func(string) bool {
	return func(command string) bool {
		if command == "" {
			return true
		}
		if classify.IsKnownSideEffectBash(command) {
			return true
		}
		for _, pattern := range cfg.NeverCacheBashPatterns {
			if bashPatternMatches(pattern, command) {
				return false
			}
		}
		return classify.ClassifyBash(command) == classify.PolicySideEffect
	}
}

// bashIsCacheable returns false for live/volatile status checks. These are
// safe as non-fence observations, but their own result should be re-queried
// after compaction because an external process may have changed the answer.
func bashIsCacheable(command string) bool {
	return classify.ClassifyBash(command) == classify.PolicyReadOnly
}

func bashCacheableFn(cfg config.Resolved) func(string) bool {
	return func(command string) bool {
		if !bashIsCacheable(command) {
			return false
		}
		for _, pattern := range cfg.NeverCacheBashPatterns {
			if bashPatternMatches(pattern, command) {
				return false
			}
		}
		return true
	}
}

func bashPatternMatches(pattern, command string) bool {
	pattern = strings.TrimSpace(pattern)
	command = strings.TrimSpace(command)
	if pattern == "" {
		return false
	}
	if ok, err := filepath.Match(pattern, command); err == nil && ok {
		return true
	}
	return strings.Contains(command, pattern)
}

// emitHitDecision translates a HitResult into the right Claude-shaped hook
// response. Codex shares the same logic via emitCodexHitDecision below.
//
// Three branches:
//
//   - InFlight=true: notify-only (no permission decision). Caller must
//     not deny because the model may genuinely need to re-launch.
//   - cfgAsk + not bypass: ask the user via Claude's permission prompt.
//   - else (bypass mode or ask disabled): deny + inject cached output.
//
// emitHitDecision translates a HitResult into the right Claude-shaped hook
// response. Codex shares the same logic via emitCodexHitDecision below.
//
// Three branches:
//
//   - InFlight=true: notify-only (no permission decision). Caller must
//     not deny because the model may genuinely need to re-launch.
//   - cfgAsk + not bypass: ask the user via Claude's permission prompt.
//   - else (bypass mode or ask disabled): deny + inject cached output.
func emitHitDecision(out *hook.PreToolUseOutput, hit *store.HitResult, in hook.PreToolUseInput, cfgAsk bool) {
	entry := hit.Entry
	ago := time.Since(entry.RecordedAt)
	label := pickLabel(entry.Label, in.ToolName, in.ToolInput)

	if hit.InFlight {
		out.HookSpecificOutput.AdditionalContext = inject.FormatInFlight(
			entry.ToolName, label, ago, toEventDescriptors(hit.AfterEvents),
		)
		return
	}

	var ec *int
	if entry.ExitCode.Valid {
		v := int(entry.ExitCode.Int64)
		ec = &v
	}

	if cfgAsk && !in.IsBypassMode() {
		out.HookSpecificOutput.PermissionDecision = "ask"
		out.HookSpecificOutput.PermissionDecisionReason = inject.FormatAskReason(
			entry.ToolName, label, ec, ago, entry.Output,
		)
		out.HookSpecificOutput.AdditionalContext = inject.FormatTerminalHitContext(
			entry.ToolName, label, ec, ago, entry.Output, toEventDescriptors(hit.AfterEvents),
		)
		return
	}
	reason := "splice: cache hit (post-compact, fence-clear)"
	if in.IsBypassMode() {
		reason = "splice: cache hit; bypass mode detected, ask path unavailable"
	}
	out.HookSpecificOutput.PermissionDecision = "deny"
	out.HookSpecificOutput.PermissionDecisionReason = reason
	out.HookSpecificOutput.AdditionalContext = inject.FormatTerminalHitContext(
		entry.ToolName, label, ec, ago, entry.Output, toEventDescriptors(hit.AfterEvents),
	)
}

// emitCodexHitDecision is the Codex twin of emitHitDecision. Same three
// branches, different output schema (decision: { behavior, reason }).
func emitCodexHitDecision(out *hook.CodexPreToolUseOutput, hit *store.HitResult, in hook.PreToolUseInput, cfgAsk bool) {
	entry := hit.Entry
	ago := time.Since(entry.RecordedAt)
	label := pickLabel(entry.Label, in.ToolName, in.ToolInput)

	if hit.InFlight {
		out.HookSpecificOutput.AdditionalContext = inject.FormatInFlight(
			entry.ToolName, label, ago, toEventDescriptors(hit.AfterEvents),
		)
		return
	}

	var ec *int
	if entry.ExitCode.Valid {
		v := int(entry.ExitCode.Int64)
		ec = &v
	}

	if cfgAsk && !in.IsBypassMode() {
		out.HookSpecificOutput.Decision = &hook.CodexDecision{
			Behavior: "ask",
			Reason:   inject.FormatAskReason(entry.ToolName, label, ec, ago, entry.Output),
		}
		out.HookSpecificOutput.AdditionalContext = inject.FormatTerminalHitContext(
			entry.ToolName, label, ec, ago, entry.Output, toEventDescriptors(hit.AfterEvents),
		)
		return
	}
	reason := "splice: cache hit (post-compact, fence-clear)"
	if in.IsBypassMode() {
		reason = "splice: cache hit; bypass mode detected, ask path unavailable"
	}
	out.HookSpecificOutput.Decision = &hook.CodexDecision{
		Behavior: "deny",
		Reason:   reason,
	}
	out.HookSpecificOutput.AdditionalContext = inject.FormatTerminalHitContext(
		entry.ToolName, label, ec, ago, entry.Output, toEventDescriptors(hit.AfterEvents),
	)
}

// toEventDescriptors converts store TrailEntry slice into the lightweight
// shape inject.FormatInFlight wants. Keeping the conversion in main keeps
// the inject package free of any store import.
func toEventDescriptors(events []store.TrailEntry) []inject.EventDescriptor {
	out := make([]inject.EventDescriptor, 0, len(events))
	for _, e := range events {
		out = append(out, inject.EventDescriptor{
			ToolName: e.ToolName,
			Label:    e.Label,
			Output:   e.Output,
			Status:   e.Status,
			ExitCode: e.ExitCode,
		})
	}
	return out
}

// ---------------------------------------------------------------------
// PostToolUse: record the result, append to live_trail.
// ---------------------------------------------------------------------

// maybeEvictSnapshot is called on every no-hit PreToolUse. It bumps
// the eviction counter; if the new value crosses the configured
// threshold, the snapshot is dropped — splice has decided the model
// has wandered off the topic that produced the snapshot.
//
// `after` of 0 disables eviction (snapshot survives until the next
// freeze or session deletion). Errors are logged but don't propagate;
// eviction is best-effort housekeeping, not on the decision path.
func maybeEvictSnapshot(st *store.Store, after int) {
	if after <= 0 {
		return
	}
	n, err := st.BumpEviction()
	if err != nil {
		fmt.Fprintln(os.Stderr, "splice: warn: bump eviction:", err)
		return
	}
	if n >= after {
		if err := st.DropSnapshot(); err != nil {
			fmt.Fprintln(os.Stderr, "splice: warn: drop snapshot:", err)
		}
	}
}

func preserveInterceptedTerminalHit(st *store.Store, callID string, entry store.TrailEntry) error {
	if callID == "" {
		return nil
	}
	err := st.FinishLiveTrailEntry(callID, entry.ExitCode, entry.Output, entry.Status, time.Now())
	if err == nil || store.IsErrNoRunningRow(err) {
		return nil
	}
	return err
}

// runPostToolUse records the tool result against the corresponding
// running live_trail row (sidecar-paired) or, if the sidecar is
// missing, appends a fresh terminal row.
func runPostToolUse() error {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	var in hook.PostToolUseInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("decode stdin: %w", err)
	}

	canonical, hash, err := fingerprint.Compute(in.ToolName, in.ToolInput)
	if err != nil {
		return fmt.Errorf("fingerprint: %w", err)
	}

	st, err := store.OpenSession(in.Cwd, in.SessionID)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	exitCode := hook.ExtractExitCode(in.ToolResponse)
	output := hook.ExtractOutput(in.ToolResponse)
	status := hook.ExtractStatus(in.ToolResponse, exitCode)
	label := hook.LabelFor(in.ToolName, in.ToolInput)

	var ec sql.NullInt64
	if exitCode != nil {
		ec = sql.NullInt64{Int64: int64(*exitCode), Valid: true}
	}

	callID, pairedByID := callIDFromPost(in)
	if pairedByID {
		if finished, err := finishPostToolUseResult(st, callID, ec, output, status, time.Now()); err != nil {
			return err
		} else if finished {
			return nil
		}
		if err := st.AppendTerminalFromFrozenRunning(callID, ec, output, status, time.Now()); err == nil {
			return nil
		} else if !store.IsErrNoRunningRow(err) {
			return fmt.Errorf("append frozen terminal: %w", err)
		}
		if hasClearBarrier(in.Cwd, in.SessionID) {
			return nil
		}
	} else {
		if hasClearBarrier(in.Cwd, in.SessionID) {
			clearPendingID(in.Cwd, in.SessionID, hash)
			return nil
		}
		callID, _ = readPendingID(in.Cwd, in.SessionID, hash)
		clearPendingID(in.Cwd, in.SessionID, hash)
	}

	// Sidecar found: try to finish the matching running row.
	if callID != "" {
		if finished, err := finishPostToolUseResult(st, callID, ec, output, status, time.Now()); err != nil {
			return err
		} else if finished {
			return nil
		}
		// Running row vanished (e.g. /clear ran, or PreCompact already
		// froze it). Fall through to Append so the data is captured.
	}

	if err := st.AppendLiveTrail(store.TrailEntry{
		CallID:     callID,
		ToolName:   in.ToolName,
		ArgsHash:   hash,
		ArgsJSON:   canonical,
		Label:      label,
		ExitCode:   ec,
		Output:     output,
		Status:     status,
		RecordedAt: time.Now(),
	}); err != nil {
		return fmt.Errorf("append live_trail: %w", err)
	}
	return nil
}

func callIDFromPost(in hook.PostToolUseInput) (string, bool) {
	hostToolUseID := in.HostToolUseID()
	if hostToolUseID == "" {
		return "", false
	}
	return hookCallID(hostToolUseID), true
}

func hookCallID(hostToolUseID string) string {
	hostToolUseID = strings.TrimSpace(hostToolUseID)
	if hostToolUseID == "" {
		return ""
	}
	return "hook:" + hostToolUseID
}

func finishPostToolUseResult(st *store.Store, callID string, exitCode sql.NullInt64, output, status string, finishedAt time.Time) (bool, error) {
	if err := st.FinishLiveTrailEntry(callID, exitCode, output, status, finishedAt); err == nil {
		return true, nil
	} else if !store.IsErrNoRunningRow(err) {
		return false, fmt.Errorf("finish live_trail: %w", err)
	}
	return false, nil
}

// ---------------------------------------------------------------------
// PreCompact: freeze live_trail into pre_compact_trail, clear cooldown.
// ---------------------------------------------------------------------

func runPreCompact() error {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	var in hook.PreCompactInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("decode stdin: %w", err)
	}
	if in.SessionID == "" {
		return errors.New("pre-compact: missing session_id")
	}

	st, err := store.OpenSession(in.Cwd, in.SessionID)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	rows, err := st.FreezePreCompact()
	if err != nil {
		return fmt.Errorf("freeze: %w", err)
	}
	fmt.Fprintf(os.Stderr, "splice: froze %d trail events for session %s\n", rows, in.SessionID)
	return nil
}

// ---------------------------------------------------------------------
// SessionStart: write a per-session marker so observers (codex-watch) can
// discover active sessions, and on source=clear wipe this session's trails
// since the model just lost its memory.
// ---------------------------------------------------------------------

const markerDirName = "active-sessions"

func runSessionStart() error {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	var in hook.SessionStartInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("decode stdin: %w", err)
	}
	if in.SessionID == "" {
		return errors.New("session-start: missing session_id")
	}

	// Write/update the marker for every source — even "compact" — because the
	// session is still alive and watchers should treat it as active. Only
	// "stop" / process exit removes the marker (handled elsewhere).
	if err := writeSessionMarker(in.Cwd, in); err != nil {
		fmt.Fprintln(os.Stderr, "splice: warn: write marker:", err)
	}

	// On /clear: drop this session's entire DB file. Model just lost its
	// memory; any cached results would mislead the now-amnesic model.
	// Drop also clears the per-session pending sidecar dir.
	if in.Source == "clear" {
		dbPath := store.SessionDBPath(in.Cwd, in.SessionID)
		if err := store.DropSessionFiles(dbPath); err != nil {
			fmt.Fprintln(os.Stderr, "splice: warn: drop session files:", err)
		}
		_ = os.RemoveAll(pendingDir(in.Cwd, in.SessionID))
		if err := writeClearBarrier(in.Cwd, in.SessionID); err != nil {
			fmt.Fprintln(os.Stderr, "splice: warn: write clear barrier:", err)
		}
		fmt.Fprintf(os.Stderr, "splice: dropped session %s (source=clear)\n", in.SessionID)
	}
	return nil
}

func sessionMarkerDir(cwd string) string {
	return filepath.Join(cwd, ".splice", markerDirName)
}

// SessionMarker is the on-disk shape of an active-session marker. JSON because
// codex-watch (started from a different process tree) is the natural reader.
type SessionMarker struct {
	SessionID      string `json:"session_id"`
	Source         string `json:"source"`
	TranscriptPath string `json:"transcript_path,omitempty"`
	Cwd            string `json:"cwd"`
	UpdatedAt      string `json:"updated_at"`
}

func writeSessionMarker(cwd string, in hook.SessionStartInput) error {
	dir := sessionMarkerDir(cwd)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	m := SessionMarker{
		SessionID:      in.SessionID,
		Source:         in.Source,
		TranscriptPath: in.TranscriptPath,
		Cwd:            cwd,
		UpdatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, in.SessionID+".tmp")
	final := filepath.Join(dir, in.SessionID+".json")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// ---------------------------------------------------------------------
// install / uninstall / helpers (unchanged from v0.2.1).
// ---------------------------------------------------------------------

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func pendingDir(cwd, sessionID string) string {
	return filepath.Join(cwd, ".splice", "sessions", sessionID+".pending")
}

func writePendingID(cwd, sessionID, hash, callID string) error {
	dir := pendingDir(cwd, sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, hash), []byte(callID), 0o600)
}

func readPendingID(cwd, sessionID, hash string) (string, error) {
	b, err := os.ReadFile(filepath.Join(pendingDir(cwd, sessionID), hash))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func clearPendingID(cwd, sessionID, hash string) {
	_ = os.Remove(filepath.Join(pendingDir(cwd, sessionID), hash))
}

func clearBarrierPath(cwd, sessionID string) string {
	return filepath.Join(cwd, ".splice", "sessions", sessionID+".cleared")
}

func writeClearBarrier(cwd, sessionID string) error {
	dir := store.SessionsDir(cwd)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(clearBarrierPath(cwd, sessionID), []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o600)
}

func hasClearBarrier(cwd, sessionID string) bool {
	_, err := os.Stat(clearBarrierPath(cwd, sessionID))
	return err == nil
}

func newID() string {
	var buf [12]byte
	now := time.Now().UnixNano()
	for i := 0; i < 8; i++ {
		buf[i] = byte(now >> (i * 8))
	}
	pid := os.Getpid()
	for i := 0; i < 4; i++ {
		buf[8+i] = byte(pid >> (i * 8))
	}
	h := sha1.Sum(buf[:])
	return "id_" + hex.EncodeToString(h[:8])
}

func runInstallClaudeHooks(args []string) error {
	scope, err := parseScope("install-claude-hooks", args)
	if err != nil {
		return err
	}
	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate splice binary: %w", err)
	}
	cwd, _ := os.Getwd()
	plan, err := install.ResolvePlan(scope, cwd, binPath, exec.LookPath)
	if err != nil {
		return err
	}
	action, err := install.Apply(plan)
	if err != nil {
		return err
	}
	scopeName := "user"
	if scope == install.ScopeLocal {
		scopeName = "local"
	}
	fmt.Fprintf(os.Stderr, "splice: %s settings at %s (%s scope)\n", action, plan.SettingsPath, scopeName)
	fmt.Fprintf(os.Stderr, "splice: hook command -> %s pre-tool-use / post-tool-use / pre-compact\n", plan.BinaryCommand)
	if !plan.OnPath {
		fmt.Fprintln(os.Stderr, "splice: tip: add the binary's directory to PATH so the hook command becomes 'splice':")
		fmt.Fprintf(os.Stderr, "  export PATH=\"%s:$PATH\"   (bash/zsh)\n", filepath.Dir(plan.BinaryPath))
		fmt.Fprintf(os.Stderr, "  $env:Path = \"%s;\" + $env:Path  (PowerShell)\n", filepath.Dir(plan.BinaryPath))
	}
	fmt.Fprintln(os.Stderr, "splice: restart Claude Code (or reload settings) for hooks to take effect.")
	return nil
}

func runUninstallClaudeHooks(args []string) error {
	scope, err := parseScope("uninstall-claude-hooks", args)
	if err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	removed, path, err := install.Uninstall(scope, cwd)
	if err != nil {
		return err
	}
	scopeName := "user"
	if scope == install.ScopeLocal {
		scopeName = "local"
	}
	if !removed {
		fmt.Fprintf(os.Stderr, "splice: no managed hooks found at %s (%s scope)\n", path, scopeName)
		return nil
	}
	fmt.Fprintf(os.Stderr, "splice: removed managed hooks from %s (%s scope)\n", path, scopeName)
	return nil
}

func parseScope(name string, args []string) (install.Scope, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	user := fs.Bool("user", false, "write to ~/.claude/settings.json")
	local := fs.Bool("local", false, "write to <cwd>/.claude/settings.local.json")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if *user && *local {
		return 0, fmt.Errorf("%s: choose one of --user or --local", name)
	}
	if !*user && !*local {
		return 0, fmt.Errorf("%s: must specify --user or --local", name)
	}
	if *user {
		return install.ScopeUser, nil
	}
	return install.ScopeLocal, nil
}

func parseCodexScope(name string, args []string) (codexinstall.Scope, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	user := fs.Bool("user", false, "write to ~/.codex/config.toml")
	project := fs.Bool("project", false, "write to <cwd>/.codex/config.toml")
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if *user && *project {
		return 0, fmt.Errorf("%s: choose one of --user or --project", name)
	}
	if !*user && !*project {
		return 0, fmt.Errorf("%s: must specify --user or --project", name)
	}
	if *user {
		return codexinstall.ScopeUser, nil
	}
	return codexinstall.ScopeProject, nil
}

func runInstallCodexHooks(args []string) error {
	scope, err := parseCodexScope("install-codex-hooks", args)
	if err != nil {
		return err
	}
	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate splice binary: %w", err)
	}
	cwd, _ := os.Getwd()
	plan, err := codexinstall.ResolvePlan(scope, cwd, binPath, exec.LookPath)
	if err != nil {
		return err
	}
	action, err := codexinstall.Apply(plan)
	if err != nil {
		return err
	}
	scopeName := "user"
	if scope == codexinstall.ScopeProject {
		scopeName = "project"
	}
	fmt.Fprintf(os.Stderr, "splice: %s codex config at %s (%s scope)\n", action, plan.ConfigPath, scopeName)
	fmt.Fprintf(os.Stderr, "splice: hook command -> %s codex-pre-tool-use / codex-post-tool-use / session-start\n", plan.BinaryCommand)
	if !plan.OnPath {
		fmt.Fprintln(os.Stderr, "splice: tip: add the binary's directory to PATH so the hook command becomes 'splice':")
		fmt.Fprintf(os.Stderr, "  export PATH=\"%s:$PATH\"   (bash/zsh)\n", filepath.Dir(plan.BinaryPath))
		fmt.Fprintf(os.Stderr, "  $env:Path = \"%s;\" + $env:Path  (PowerShell)\n", filepath.Dir(plan.BinaryPath))
	}
	fmt.Fprintln(os.Stderr, "splice: now run `splice codex-watch &` in this directory to enable post-compact protection.")
	return nil
}

func runUninstallCodexHooks(args []string) error {
	scope, err := parseCodexScope("uninstall-codex-hooks", args)
	if err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	removed, path, err := codexinstall.Uninstall(scope, cwd)
	if err != nil {
		return err
	}
	scopeName := "user"
	if scope == codexinstall.ScopeProject {
		scopeName = "project"
	}
	if !removed {
		fmt.Fprintf(os.Stderr, "splice: no managed hooks found at %s (%s scope)\n", path, scopeName)
		return nil
	}
	fmt.Fprintf(os.Stderr, "splice: removed managed hooks from %s (%s scope)\n", path, scopeName)
	return nil
}
