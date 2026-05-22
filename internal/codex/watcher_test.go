package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wang33550/splice/internal/store"
)

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "splice-codex-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.Setenv("SPLICE_HOME", root)
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

// Build a complete CODEX_HOME + cwd test environment with a marker pointing
// at a rollout file containing pre/post-compact tool activity.
func setupWatchEnv(t *testing.T) (cwd, sessionID, rolloutPath string) {
	t.Helper()

	t.Setenv("SPLICE_HOME", t.TempDir())
	codexHome := t.TempDir()
	cwd = t.TempDir()
	sessionID = "sess-watch-1"

	rollout := fmt.Sprintf(`{"timestamp":"2026-05-19T10:00:00.000Z","type":"session_meta","payload":{"session_id":"sess-watch-1","cwd":%q}}
{"timestamp":"2026-05-19T10:00:01.000Z","type":"response_item","payload":{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"command\":\"npm test\"}"}}
{"timestamp":"2026-05-19T10:00:02.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"12 passed","exit_code":0}}
{"timestamp":"2026-05-19T10:00:30.000Z","type":"context_compaction"}
`, cwd)
	rolloutPath = writeRolloutFile(t, codexHome, sessionID, rollout)
	writeMarker(t, sessionID, cwd)
	t.Setenv("CODEX_HOME", codexHome)
	return
}

func writeRolloutFile(t *testing.T, codexHome, sessionID, body string) string {
	t.Helper()
	dateDir := filepath.Join(codexHome, "sessions", "2026", "05", "19")
	if err := os.MkdirAll(dateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rolloutPath := filepath.Join(dateDir, "rollout-2026-05-19-"+sessionID+".jsonl")
	if err := os.WriteFile(rolloutPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return rolloutPath
}

func writeMarker(t *testing.T, sessionID, cwd string) {
	t.Helper()
	markerDir := store.ActiveSessionsDir()
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := MarkerInfo{
		SessionID: sessionID,
		Source:    "startup",
		Cwd:       cwd,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	mb, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(markerDir, store.SessionFileBase(sessionID)+".json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWatcherReplaysAndFreezes(t *testing.T) {
	cwd, sessionID, _ := setupWatchEnv(t)

	w, err := New(cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.PollInterval = 50 * time.Millisecond
	var logs logSink
	w.Logger = logs.logger

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	waitForLogged(t, &logs, "froze 1 trail events", 1, 10*time.Second, done)
	stopWatcher(t, cancel, done, &logs)

	// And the cached entry must be retrievable as a hit.
	st, err := store.OpenSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, hash := FingerprintToolCallScoped("shell", `{"command":"npm test"}`, store.ProjectKey(cwd))
	hit, err := st.LookupCachedHit(hash, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil {
		t.Fatal("expected cache hit for npm test, got nil")
	}
	if !strings.Contains(hit.Entry.Output, "12 passed") {
		t.Fatalf("hit output: %q", hit.Entry.Output)
	}
}

func formatLog(format string, args ...any) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, format, args...)
	b.WriteByte('\n')
	return b.String()
}

type logSink struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logSink) logger(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b.WriteString(formatLog(format, args...))
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *logSink) count(needle string) int {
	return strings.Count(l.String(), needle)
}

func waitForLogged(t *testing.T, logs *logSink, needle string, minCount int, timeout time.Duration, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if logs.count(needle) >= minCount {
			return
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("watcher exited early: %v\nlogs:\n%s", err, logs.String())
			}
			t.Fatalf("watcher exited early without error\nlogs:\n%s", logs.String())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for log %q count %d\nlogs:\n%s", needle, minCount, logs.String())
}

func stopWatcher(t *testing.T, cancel context.CancelFunc, done <-chan error, logs *logSink) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watcher exited with error: %v\nlogs:\n%s", err, logs.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("watcher did not stop\nlogs:\n%s", logs.String())
	}
}

func TestWatcherTailsNewLines(t *testing.T) {
	cwd, sessionID, rolloutPath := setupWatchEnv(t)

	w, err := New(cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.PollInterval = 50 * time.Millisecond
	var logs logSink
	w.Logger = logs.logger

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	// Wait for initial freeze.
	waitForLogged(t, &logs, "froze 1 trail events", 1, 10*time.Second, done)

	// Append a new tool call after the compaction.
	extra := `{"timestamp":"2026-05-19T10:01:00.000Z","type":"response_item","payload":{"type":"function_call","call_id":"c2","name":"shell","arguments":"{\"command\":\"go test\"}"}}` + "\n" +
		`{"timestamp":"2026-05-19T10:01:01.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c2","output":"all good","exit_code":0}}` + "\n" +
		`{"timestamp":"2026-05-19T10:01:30.000Z","type":"context_compaction"}` + "\n"
	f, err := os.OpenFile(rolloutPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(extra); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	// Now the second compaction should re-freeze, capturing go test.
	_, hash := FingerprintToolCallScoped("shell", `{"command":"go test"}`, store.ProjectKey(cwd))
	waitForLogged(t, &logs, "froze 1 trail events", 2, 10*time.Second, done)
	stopWatcher(t, cancel, done, &logs)

	st, err := store.OpenSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hit, err := st.LookupCachedHit(hash, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || !strings.Contains(hit.Entry.Output, "all good") {
		t.Fatalf("expected go test hit after second freeze, got %+v", hit)
	}
}

func TestWatcherInitialOffsetBeyondTruncatedRolloutReplaysFromStart(t *testing.T) {
	t.Setenv("SPLICE_HOME", t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	cwd := t.TempDir()
	sessionID := "sess-watch-initial-truncated"
	body := fmt.Sprintf(`{"timestamp":"2026-05-19T10:00:00.000Z","type":"session_meta","payload":{"session_id":"sess-watch-initial-truncated","cwd":%q}}
{"timestamp":"2026-05-19T10:00:01.000Z","type":"response_item","payload":{"type":"function_call","call_id":"new","name":"shell","arguments":"{\"command\":\"go test\"}"}}
{"timestamp":"2026-05-19T10:00:02.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"new","output":"new compacted result","exit_code":0}}
{"timestamp":"2026-05-19T10:00:03.000Z","type":"context_compaction"}
`, cwd)
	writeRolloutFile(t, codexHome, sessionID, body)
	writeMarker(t, sessionID, cwd)
	st, err := store.OpenSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetLastRolloutOffset(999999); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	w, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	w.PollInterval = 30 * time.Millisecond
	var logs logSink
	w.Logger = logs.logger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	waitForLogged(t, &logs, "replaying from start", 1, 10*time.Second, done)
	waitForLogged(t, &logs, "froze 1 trail events", 1, 10*time.Second, done)
	stopWatcher(t, cancel, done, &logs)

	st, err = store.OpenSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, hash := FingerprintToolCallScoped("shell", `{"command":"go test"}`, store.ProjectKey(cwd))
	hit, err := st.LookupCachedHit(hash, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || hit.Entry.Output != "new compacted result" {
		t.Fatalf("truncated rollout should replay current file from start, got %+v", hit)
	}
}

func TestWatcherRunningTailHandlesRolloutTruncation(t *testing.T) {
	cwd, sessionID, rolloutPath := setupWatchEnv(t)

	w, err := New(cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.PollInterval = 30 * time.Millisecond
	var logs logSink
	w.Logger = logs.logger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	waitForLogged(t, &logs, "froze 1 trail events", 1, 10*time.Second, done)

	truncated := fmt.Sprintf(`{"timestamp":"2026-05-19T10:02:00.000Z","type":"session_meta","payload":{"session_id":"sess-watch-1","cwd":%q}}
{"timestamp":"2026-05-19T10:02:01.000Z","type":"response_item","payload":{"type":"function_call","call_id":"after-truncate","name":"shell","arguments":"{\"command\":\"go test\"}"}}
{"timestamp":"2026-05-19T10:02:02.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"after-truncate","output":"after truncate ok","exit_code":0}}
{"timestamp":"2026-05-19T10:02:03.000Z","type":"context_compaction"}
`, cwd)
	if err := os.WriteFile(rolloutPath, []byte(truncated), 0o644); err != nil {
		t.Fatal(err)
	}

	waitForLogged(t, &logs, "replaying from start", 1, 10*time.Second, done)
	waitForLogged(t, &logs, "froze 1 trail events", 2, 10*time.Second, done)
	stopWatcher(t, cancel, done, &logs)

	st, err := store.OpenSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, hash := FingerprintToolCallScoped("shell", `{"command":"go test"}`, store.ProjectKey(cwd))
	hit, err := st.LookupCachedHit(hash, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || hit.Entry.Output != "after truncate ok" {
		t.Fatalf("expected hit after truncation replay, got %+v", hit)
	}
}

func TestWatcherRefreshRestartsExitedTail(t *testing.T) {
	cwd, sessionID, rolloutPath := setupWatchEnv(t)

	w, err := New(cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.PollInterval = 30 * time.Millisecond
	var logs logSink
	w.Logger = logs.logger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	waitForLogged(t, &logs, "froze 1 trail events", 1, 10*time.Second, done)
	if err := os.Remove(rolloutPath); err != nil {
		t.Fatal(err)
	}
	waitForLogged(t, &logs, "tail error", 1, 10*time.Second, done)

	replacement := fmt.Sprintf(`{"timestamp":"2026-05-19T10:03:00.000Z","type":"session_meta","payload":{"session_id":"sess-watch-1","cwd":%q}}
{"timestamp":"2026-05-19T10:03:01.000Z","type":"response_item","payload":{"type":"function_call","call_id":"after-restart","name":"shell","arguments":"{\"command\":\"go vet\"}"}}
{"timestamp":"2026-05-19T10:03:02.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"after-restart","output":"after restart ok","exit_code":0}}
{"timestamp":"2026-05-19T10:03:03.000Z","type":"context_compaction"}
`, cwd)
	if err := os.WriteFile(rolloutPath, []byte(replacement), 0o644); err != nil {
		t.Fatal(err)
	}

	waitForLogged(t, &logs, "tail stopped, restarting", 1, 10*time.Second, done)
	waitForLogged(t, &logs, "froze 1 trail events", 2, 10*time.Second, done)
	stopWatcher(t, cancel, done, &logs)

	st, err := store.OpenSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, hash := FingerprintToolCallScoped("shell", `{"command":"go vet"}`, store.ProjectKey(cwd))
	hit, err := st.LookupCachedHit(hash, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || hit.Entry.Output != "after restart ok" {
		t.Fatalf("restarted tail should read replacement rollout, got %+v", hit)
	}
}

func TestWatcherRefreshSwitchesToNewerRolloutFile(t *testing.T) {
	t.Setenv("SPLICE_HOME", t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	cwd := t.TempDir()
	sessionID := "sess-watch-newer-rollout"
	oldBody := fmt.Sprintf(`{"timestamp":"2026-05-19T10:00:00.000Z","type":"session_meta","payload":{"session_id":%q,"cwd":%q}}
{"timestamp":"2026-05-19T10:00:01.000Z","type":"response_item","payload":{"type":"function_call","call_id":"old","name":"shell","arguments":"{\"command\":\"npm test\"}"}}
{"timestamp":"2026-05-19T10:00:02.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"old","output":"old rollout result","exit_code":0}}
{"timestamp":"2026-05-19T10:00:03.000Z","type":"context_compaction"}
`, sessionID, cwd)
	oldPath := writeRolloutFile(t, codexHome, sessionID, oldBody)
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	writeMarker(t, sessionID, cwd)

	w, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	w.PollInterval = 30 * time.Millisecond
	var logs logSink
	w.Logger = logs.logger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	waitForLogged(t, &logs, "froze 1 trail events", 1, 10*time.Second, done)

	dateDir := filepath.Dir(oldPath)
	newPath := filepath.Join(dateDir, "rollout-2026-05-19-"+sessionID+"-replacement.jsonl")
	newBody := fmt.Sprintf(`{"timestamp":"2026-05-19T10:01:00.000Z","type":"session_meta","payload":{"session_id":%q,"cwd":%q}}
{"timestamp":"2026-05-19T10:01:01.000Z","type":"response_item","payload":{"type":"function_call","call_id":"new","name":"shell","arguments":"{\"command\":\"go test\"}"}}
{"timestamp":"2026-05-19T10:01:02.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"new","output":"new rollout result","exit_code":0}}
{"timestamp":"2026-05-19T10:01:03.000Z","type":"context_compaction"}
`, sessionID, cwd)
	if err := os.WriteFile(newPath, []byte(newBody), 0o644); err != nil {
		t.Fatal(err)
	}
	newTime := time.Now().Add(time.Hour)
	if err := os.Chtimes(newPath, newTime, newTime); err != nil {
		t.Fatal(err)
	}

	waitForLogged(t, &logs, "rollout changed, restarting tail", 1, 10*time.Second, done)
	waitForLogged(t, &logs, "froze 1 trail events", 2, 10*time.Second, done)
	stopWatcher(t, cancel, done, &logs)

	st, err := store.OpenSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, oldHash := FingerprintToolCallScoped("shell", `{"command":"npm test"}`, store.ProjectKey(cwd))
	oldHit, err := st.LookupCachedHit(oldHash, nil)
	if err != nil {
		t.Fatal(err)
	}
	if oldHit != nil {
		t.Fatalf("new rollout replay should replace old snapshot, got %+v", oldHit)
	}
	_, newHash := FingerprintToolCallScoped("shell", `{"command":"go test"}`, store.ProjectKey(cwd))
	newHit, err := st.LookupCachedHit(newHash, nil)
	if err != nil {
		t.Fatal(err)
	}
	if newHit == nil || newHit.Entry.Output != "new rollout result" {
		t.Fatalf("watcher should use newer rollout file, got %+v", newHit)
	}
}

func TestWatcherCleansUpOnMarkerRemoval(t *testing.T) {
	cwd, sessionID, _ := setupWatchEnv(t)

	w, err := New(cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.PollInterval = 30 * time.Millisecond
	w.Logger = func(string, ...any) {}
	w.pendingMu.Lock()
	w.pending[sessionID] = map[string]*pendingCall{
		"old-call": {ToolName: "Bash", Hash: "h-old"},
	}
	w.pendingMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	waitFor(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		_, ok := w.sessions[sessionID]
		return ok
	}, 500*time.Millisecond)

	// Remove the marker — codex exited cleanly.
	markerPath := filepath.Join(store.ActiveSessionsDir(), store.SessionFileBase(sessionID)+".json")
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		_, ok := w.sessions[sessionID]
		return !ok
	}, 500*time.Millisecond)
	w.pendingMu.Lock()
	_, pendingStillPresent := w.pending[sessionID]
	w.pendingMu.Unlock()
	if pendingStillPresent {
		t.Fatal("marker removal should drop stale pending calls for that session")
	}
}

func TestWatcherRemovesStaleOrphanMarkerWithoutRollout(t *testing.T) {
	t.Setenv("SPLICE_HOME", t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	sessionID := "sess-stale-orphan-marker"
	markerDir := store.ActiveSessionsDir()
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := MarkerInfo{
		SessionID: sessionID,
		Source:    "startup",
		Cwd:       t.TempDir(),
		UpdatedAt: time.Now().Add(-orphanMarkerTTL - time.Hour).UTC().Format(time.RFC3339Nano),
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(markerDir, store.SessionFileBase(sessionID)+".json")
	if err := os.WriteFile(markerPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	w, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	w.PollInterval = 30 * time.Millisecond
	var logs logSink
	w.Logger = logs.logger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	waitForLogged(t, &logs, "removed stale marker", 1, 10*time.Second, done)
	stopWatcher(t, cancel, done, &logs)
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("stale orphan marker should be removed, stat err=%v", err)
	}
}

func TestWatcherKeepsFreshMarkerWithoutRollout(t *testing.T) {
	t.Setenv("SPLICE_HOME", t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	sessionID := "sess-fresh-marker-no-rollout"
	writeMarker(t, sessionID, t.TempDir())
	markerPath := filepath.Join(store.ActiveSessionsDir(), store.SessionFileBase(sessionID)+".json")

	w, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	w.PollInterval = 30 * time.Millisecond
	var logs logSink
	w.Logger = logs.logger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	waitForLogged(t, &logs, "not found yet", 1, 10*time.Second, done)
	stopWatcher(t, cancel, done, &logs)
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("fresh marker should remain for later rollout discovery: %v", err)
	}
}

func TestWatcherShutdownDropsAllPending(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("SPLICE_HOME", t.TempDir())
	w, err := New(cwd)
	if err != nil {
		t.Fatal(err)
	}
	w.pendingMu.Lock()
	w.pending["s1"] = map[string]*pendingCall{"c1": {ToolName: "Bash", Hash: "h1"}}
	w.pending["s2"] = map[string]*pendingCall{"c2": {ToolName: "Read", Hash: "h2"}}
	w.pendingMu.Unlock()

	w.shutdownAll()

	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()
	if len(w.pending) != 0 {
		t.Fatalf("shutdown should drop all pending calls, got %+v", w.pending)
	}
}

// TestWatcherProducesUsableHits ensures hits survive watcher exit (i.e.
// they're really persisted to disk, not just held in memory).
func TestWatcherProducesUsableHits(t *testing.T) {
	cwd, sessionID, _ := setupWatchEnv(t)
	w, err := New(cwd)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.PollInterval = 50 * time.Millisecond
	var logs logSink
	w.Logger = logs.logger

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	waitForLogged(t, &logs, "froze 1 trail events", 1, 10*time.Second, done)
	stopWatcher(t, cancel, done, &logs)

	_, hash := FingerprintToolCallScoped("Bash", `{"command":"npm test"}`, store.ProjectKey(cwd))
	st, err := store.OpenSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hit, err := st.LookupCachedHit(hash, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil {
		t.Fatal("expected cache hit")
	}
	if hit.Entry.ToolName != "Bash" {
		t.Errorf("tool name should be folded to Bash; got %q", hit.Entry.ToolName)
	}
}

func TestGlobalWatcherCoversMultipleProjectsAndProjectlessSessions(t *testing.T) {
	t.Setenv("SPLICE_HOME", t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)

	projectA := t.TempDir()
	projectB := t.TempDir()
	sessions := []struct {
		id      string
		cwd     string
		command string
		output  string
	}{
		{id: "sess-desktop-existing-project", cwd: projectA, command: "go test ./...", output: "project A ok"},
		{id: "sess-desktop-new-project", cwd: projectB, command: "npm test", output: "project B ok"},
		{id: "sess-projectless-chat", cwd: "", command: "date", output: "projectless ok"},
	}
	for i, s := range sessions {
		body := fmt.Sprintf(
			`{"timestamp":"2026-05-19T10:00:%02d.000Z","type":"session_meta","payload":{"session_id":%q,"cwd":%q}}
{"timestamp":"2026-05-19T10:00:%02d.000Z","type":"response_item","payload":{"type":"function_call","call_id":%q,"name":"shell","arguments":%q}}
{"timestamp":"2026-05-19T10:00:%02d.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":%q,"output":%q,"exit_code":0}}
{"timestamp":"2026-05-19T10:00:%02d.000Z","type":"context_compaction"}
`,
			i, s.id, s.cwd,
			i+10, "call-"+s.id, `{"command":"`+s.command+`"}`,
			i+20, "call-"+s.id, s.output,
			i+30,
		)
		writeRolloutFile(t, codexHome, s.id, body)
		writeMarker(t, s.id, s.cwd)
	}

	w, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	w.PollInterval = 30 * time.Millisecond
	var logs logSink
	w.Logger = logs.logger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	waitForLogged(t, &logs, "froze 1 trail events", len(sessions), 10*time.Second, done)
	stopWatcher(t, cancel, done, &logs)

	for _, s := range sessions {
		_, hash := FingerprintToolCallScoped("shell", `{"command":"`+s.command+`"}`, store.ProjectKey(s.cwd))
		st, err := store.OpenSession(s.id)
		if err != nil {
			t.Fatal(err)
		}
		hit, err := st.LookupCachedHit(hash, nil)
		_ = st.Close()
		if err != nil {
			t.Fatal(err)
		}
		if hit == nil || hit.Entry.Output != s.output {
			t.Fatalf("session %s did not get its own cached result, got %+v", s.id, hit)
		}
	}
}

func TestGlobalWatcherCoversMultipleCliTerminalsInSameProject(t *testing.T) {
	t.Setenv("SPLICE_HOME", t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	cwd := t.TempDir()

	sessions := []struct {
		id      string
		command string
		output  string
	}{
		{id: "sess-cli-terminal-one", command: "npm test", output: "terminal one ok"},
		{id: "sess-cli-terminal-two", command: "go test ./...", output: "terminal two ok"},
	}
	for i, s := range sessions {
		body := fmt.Sprintf(
			`{"timestamp":"2026-05-19T11:00:%02d.000Z","type":"session_meta","payload":{"session_id":%q,"cwd":%q}}
{"timestamp":"2026-05-19T11:00:%02d.000Z","type":"response_item","payload":{"type":"function_call","call_id":%q,"name":"shell","arguments":%q}}
{"timestamp":"2026-05-19T11:00:%02d.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":%q,"output":%q,"exit_code":0}}
{"timestamp":"2026-05-19T11:00:%02d.000Z","type":"context_compaction"}
`,
			i, s.id, cwd,
			i+10, "call-"+s.id, `{"command":"`+s.command+`"}`,
			i+20, "call-"+s.id, s.output,
			i+30,
		)
		writeRolloutFile(t, codexHome, s.id, body)
		writeMarker(t, s.id, cwd)
	}

	w, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	w.PollInterval = 30 * time.Millisecond
	var logs logSink
	w.Logger = logs.logger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	waitForLogged(t, &logs, "froze 1 trail events", len(sessions), 10*time.Second, done)
	stopWatcher(t, cancel, done, &logs)

	for _, s := range sessions {
		_, hash := FingerprintToolCallScoped("shell", `{"command":"`+s.command+`"}`, store.ProjectKey(cwd))
		st, err := store.OpenSession(s.id)
		if err != nil {
			t.Fatal(err)
		}
		hit, err := st.LookupCachedHit(hash, nil)
		_ = st.Close()
		if err != nil {
			t.Fatal(err)
		}
		if hit == nil || hit.Entry.Output != s.output {
			t.Fatalf("CLI terminal session %s should keep its own watcher result, got %+v", s.id, hit)
		}
	}
}

func TestSessionStorageFollowsSessionAcrossDesktopProjectSwitch(t *testing.T) {
	t.Setenv("SPLICE_HOME", t.TempDir())
	sessionID := "sess-desktop-project-switch"

	stA, err := store.OpenSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := stA.AppendLiveTrail(store.TrailEntry{
		ToolName: "Bash", ArgsHash: "h-switch",
		ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`, Label: "npm test",
		Status: store.StatusOK, Output: "before switch ok",
		RecordedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := stA.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}
	pathA := stA.Path()
	if err := stA.Close(); err != nil {
		t.Fatal(err)
	}

	stB, err := store.OpenSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer stB.Close()
	if stB.Path() != pathA {
		t.Fatalf("same desktop conversation should keep one session DB across cwd changes: %s != %s", stB.Path(), pathA)
	}
	hit, err := stB.LookupCachedHit("h-switch", nil, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || hit.Entry.Output != "before switch ok" {
		t.Fatalf("session state did not survive cwd switch, got %+v", hit)
	}
}

func TestWatcherUsesRolloutCwdForHistoricalProjectScope(t *testing.T) {
	t.Setenv("SPLICE_HOME", t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	projectA := t.TempDir()
	projectB := t.TempDir()
	sessionID := "sess-watch-project-switch-scope"
	body := fmt.Sprintf(
		`{"timestamp":"2026-05-19T10:00:00.000Z","type":"session_meta","payload":{"session_id":%q,"cwd":%q}}
{"timestamp":"2026-05-19T10:00:01.000Z","type":"response_item","payload":{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"command\":\"npm test\"}"}}
{"timestamp":"2026-05-19T10:00:02.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"project B result","exit_code":0}}
{"timestamp":"2026-05-19T10:00:03.000Z","type":"context_compaction"}
`,
		sessionID, projectA,
	)
	writeRolloutFile(t, codexHome, sessionID, body)
	writeMarker(t, sessionID, projectB)

	w, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	w.PollInterval = 30 * time.Millisecond
	var logs logSink
	w.Logger = logs.logger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	waitForLogged(t, &logs, "froze 1 trail events", 1, 10*time.Second, done)
	stopWatcher(t, cancel, done, &logs)

	st, err := store.OpenSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, projectBHash := FingerprintToolCallScoped("shell", `{"command":"npm test"}`, store.ProjectKey(projectB))
	if hit, err := st.LookupCachedHit(projectBHash, nil); err != nil {
		t.Fatal(err)
	} else if hit != nil {
		t.Fatalf("watcher should not re-label historical project A result as current project B, got %+v", hit)
	}
	_, projectAHash := FingerprintToolCallScoped("shell", `{"command":"npm test"}`, store.ProjectKey(projectA))
	hit, err := st.LookupCachedHit(projectAHash, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || hit.Entry.Output != "project B result" {
		t.Fatalf("watcher should scope historical result to rollout cwd, got %+v", hit)
	}
}

func TestWatcherRestartsTailWhenClearMarkerAdvancesOffset(t *testing.T) {
	t.Setenv("SPLICE_HOME", t.TempDir())
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	cwd := t.TempDir()
	sessionID := "sess-watch-clear-restart"
	initial := fmt.Sprintf(`{"timestamp":"2026-05-19T10:00:00.000Z","type":"session_meta","payload":{"session_id":"sess-watch-clear-restart","cwd":%q}}
{"timestamp":"2026-05-19T10:00:01.000Z","type":"response_item","payload":{"type":"function_call","call_id":"old","name":"shell","arguments":"{\"command\":\"npm test\"}"}}
{"timestamp":"2026-05-19T10:00:02.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"old","output":"old result","exit_code":0}}
{"timestamp":"2026-05-19T10:00:03.000Z","type":"context_compaction"}
`, cwd)
	rolloutPath := writeRolloutFile(t, codexHome, sessionID, initial)
	writeMarker(t, sessionID, cwd)

	w, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	w.PollInterval = 30 * time.Millisecond
	var logs logSink
	w.Logger = logs.logger
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	waitForLogged(t, &logs, "froze 1 trail events", 1, 10*time.Second, done)

	st, err := store.OpenSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ClearTrailState(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(rolloutPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetLastRolloutOffset(info.Size()); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	clearMarker := MarkerInfo{
		SessionID: sessionID,
		Source:    "clear",
		Cwd:       cwd,
		UpdatedAt: time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano),
	}
	raw, err := json.Marshal(clearMarker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.ActiveSessionsDir(), store.SessionFileBase(sessionID)+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	extra := `{"timestamp":"2026-05-19T10:00:04.000Z","type":"response_item","payload":{"type":"function_call","call_id":"new","name":"shell","arguments":"{\"command\":\"go test\"}"}}
{"timestamp":"2026-05-19T10:00:05.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"new","output":"new result","exit_code":0}}
{"timestamp":"2026-05-19T10:00:06.000Z","type":"context_compaction"}
`
	f, err := os.OpenFile(rolloutPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(extra); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	waitForLogged(t, &logs, "froze 1 trail events", 2, 10*time.Second, done)
	stopWatcher(t, cancel, done, &logs)

	st, err = store.OpenSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, oldHash := FingerprintToolCallScoped("shell", `{"command":"npm test"}`, store.ProjectKey(cwd))
	oldHit, err := st.LookupCachedHit(oldHash, nil)
	if err != nil {
		t.Fatal(err)
	}
	if oldHit != nil {
		t.Fatalf("clear barrier should prevent old rollout result from returning, got %+v", oldHit)
	}
	_, newHash := FingerprintToolCallScoped("shell", `{"command":"go test"}`, store.ProjectKey(cwd))
	newHit, err := st.LookupCachedHit(newHash, nil)
	if err != nil {
		t.Fatal(err)
	}
	if newHit == nil || newHit.Entry.Output != "new result" {
		t.Fatalf("post-clear rollout result should be usable, got %+v", newHit)
	}
}

func TestWatcherInFlightAcrossCompactionEmitsRunningHit(t *testing.T) {
	cwd := t.TempDir()
	sessionID := "sess-watch-inflight"
	w, err := New(cwd)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	applyEventScoped(w, st, sessionID, store.ProjectKey(cwd), mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:01.000Z","type":"response_item","payload":{"type":"function_call","call_id":"c-long","name":"shell","arguments":"{\"command\":\"python run_sim.py\"}"}}`,
	))
	applyEventScoped(w, st, sessionID, store.ProjectKey(cwd), mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:30.000Z","type":"context_compaction"}`,
	))

	_, hash := FingerprintToolCallScoped("shell", `{"command":"python run_sim.py"}`, store.ProjectKey(cwd))
	hit, err := st.LookupCachedHit(hash, nil, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || !hit.InFlight {
		t.Fatalf("expected watcher-produced in-flight hit, got %+v", hit)
	}
	if hit.Entry.CallID != rolloutCallID("c-long") {
		t.Fatalf("running snapshot should preserve rollout call id, got %q", hit.Entry.CallID)
	}
}

func TestWatcherPostCompactResultSupersedesInFlightSnapshot(t *testing.T) {
	cwd := t.TempDir()
	sessionID := "sess-watch-post-compact-result"
	w, err := New(cwd)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	applyEventScoped(w, st, sessionID, store.ProjectKey(cwd), mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:01.000Z","type":"response_item","payload":{"type":"function_call","call_id":"c-long","name":"shell","arguments":"{\"command\":\"npm test\"}"}}`,
	))
	applyEventScoped(w, st, sessionID, store.ProjectKey(cwd), mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:30.000Z","type":"context_compaction"}`,
	))
	applyEventScoped(w, st, sessionID, store.ProjectKey(cwd), mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:31.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c-long","output":"12 passed after compact","exit_code":0}}`,
	))
	applyEventScoped(w, st, sessionID, store.ProjectKey(cwd), mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:32.000Z","type":"response_item","payload":{"type":"function_call","call_id":"c-read","name":"Read","arguments":"{\"file_path\":\"/tmp/notes.md\"}"}}`,
	))
	applyEventScoped(w, st, sessionID, store.ProjectKey(cwd), mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:33.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c-read","output":"post compact note"}}`,
	))

	_, hash := FingerprintToolCallScoped("shell", `{"command":"npm test"}`, store.ProjectKey(cwd))
	hit, err := st.LookupCachedHit(hash, func(string) bool { return false }, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || hit.InFlight {
		t.Fatalf("expected terminal live result to supersede running snapshot, got %+v", hit)
	}
	if hit.Entry.Output != "12 passed after compact" {
		t.Fatalf("hit output = %q", hit.Entry.Output)
	}
	if len(hit.AfterEvents) != 1 || hit.AfterEvents[0].Label != "/tmp/notes.md" {
		t.Fatalf("expected post-result live trail after-event, got %+v", hit.AfterEvents)
	}
}

func TestWatcherRestartCanAttachResultToFrozenRunningCall(t *testing.T) {
	cwd := t.TempDir()
	sessionID := "sess-watch-restart-late-result"
	first, err := New(cwd)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	applyEventScoped(first, st, sessionID, store.ProjectKey(cwd), mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:01.000Z","type":"response_item","payload":{"type":"function_call","call_id":"c-long","name":"shell","arguments":"{\"command\":\"npm test\"}"}}`,
	))
	applyEventScoped(first, st, sessionID, store.ProjectKey(cwd), mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:30.000Z","type":"context_compaction"}`,
	))

	// Simulate watcher restart: pending call metadata is gone, but the frozen
	// running row still carries the rollout call_id.
	restarted, err := New(cwd)
	if err != nil {
		t.Fatal(err)
	}
	applyEventScoped(restarted, st, sessionID, store.ProjectKey(cwd), mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:31.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c-long","output":"12 passed after restart","exit_code":0}}`,
	))

	_, hash := FingerprintToolCallScoped("shell", `{"command":"npm test"}`, store.ProjectKey(cwd))
	hit, err := st.LookupCachedHit(hash, func(string) bool { return false }, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || hit.InFlight {
		t.Fatalf("expected late result to recover terminal hit after watcher restart, got %+v", hit)
	}
	if hit.Entry.Output != "12 passed after restart" {
		t.Fatalf("hit output = %q", hit.Entry.Output)
	}
}

func waitFor(t *testing.T, pred func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for predicate")
}

func mustParseEvent(t *testing.T, raw string) Event {
	t.Helper()
	ev, err := parseLine([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

// TestWatcherSecondInstanceLocked verifies that running a second global watcher
// while the first is still alive returns ErrLocked, preventing duplicate
// ingestion of the same rollout events.
func TestWatcherSecondInstanceLocked(t *testing.T) {
	first, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	first.Logger = func(string, ...any) {}
	first.PollInterval = 50 * time.Millisecond

	firstCtx, firstCancel := context.WithCancel(context.Background())
	defer firstCancel()

	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Run(firstCtx) }()

	// Wait until first watcher acquires the lock.
	waitFor(t, func() bool {
		_, err := os.Stat(store.WatcherLockPath())
		return err == nil
	}, 1*time.Second)

	second, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.Logger = func(string, ...any) {}

	runCtx, runCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	err = second.Run(runCtx)
	runCancel()
	if err == nil {
		t.Fatal("second watcher should fail with ErrLocked, got nil")
	}
	if !errors.Is(err, ErrLocked) && !strings.Contains(err.Error(), "another splice codex-watch") {
		t.Errorf("expected ErrLocked-style error, got %v", err)
	}

	firstCancel()
	select {
	case <-firstDone:
	case <-time.After(1 * time.Second):
		t.Fatal("first watcher did not exit on cancel")
	}

	if _, err := os.Stat(store.WatcherLockPath()); !os.IsNotExist(err) {
		t.Errorf("lock file should be removed on watcher exit, stat err=%v", err)
	}
}

// TestWatcherStaleLockReclaimed verifies that a lock file from a crashed
// process is reclaimed (rather than blocking forever).
func TestWatcherStaleLockReclaimed(t *testing.T) {
	t.Setenv("SPLICE_HOME", t.TempDir())
	dir := store.HomeDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a fake stale lock with a PID very unlikely to be alive.
	stalePath := store.WatcherLockPath()
	if err := os.WriteFile(stalePath, []byte("99999999"), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.Logger = func(string, ...any) {}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := w.Run(ctx); err != nil {
		t.Fatalf("stale lock should be reclaimed; got %v", err)
	}
}

func TestWatcherNewRejectsEmptyCwd(t *testing.T) {
	if _, err := New(""); err != nil {
		t.Fatalf("global watcher should not require cwd: %v", err)
	}
}

func TestReadMarkersSkipsBrokenFiles(t *testing.T) {
	t.Setenv("SPLICE_HOME", t.TempDir())
	dir := store.ActiveSessionsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"good.json":       `{"session_id":"s1","source":"startup","cwd":"/x"}`,
		"empty.json":      ``,
		"bad.json":        `{not json`,
		"missing-id.json": `{"source":"startup"}`,
		"ignored.txt":     `{"session_id":"txt"}`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	markers, err := readMarkers()
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 1 || markers[0].SessionID != "s1" {
		t.Fatalf("unexpected markers: %+v", markers)
	}
}

func TestReadMarkersMissingDirIsEmpty(t *testing.T) {
	t.Setenv("SPLICE_HOME", t.TempDir())
	markers, err := readMarkers()
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 0 {
		t.Fatalf("expected no markers, got %+v", markers)
	}
}

func TestWatcherHandlersIgnoreMalformedEvents(t *testing.T) {
	w, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSession("s")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	w.handleToolCall(st, "s", "", nil)
	w.handleToolCall(st, "s", "", &ToolCall{})
	w.handleToolResult(st, "s", nil)
	w.handleToolResult(st, "s", &ToolResult{})
	w.handleToolResult(st, "s", &ToolResult{CallID: "missing"})

	has, err := st.HasPreCompactTrail()
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("malformed handler inputs should not write snapshot rows")
	}
}

func TestWatcherToolNameForStoreAndLogger(t *testing.T) {
	if got := toolNameForStore("shell"); got != "Bash" {
		t.Fatalf("shell canonicalization = %q", got)
	}
	if got := toolNameForStore("Read"); got != "Read" {
		t.Fatalf("non-shell tool name = %q", got)
	}

	var b bytes.Buffer
	logger := CompactStdoutLogger(&b)
	logger("hello %s", "world")
	got := b.String()
	if !strings.Contains(got, "hello world") || !strings.HasSuffix(got, "\n") {
		t.Fatalf("bad logger output: %q", got)
	}
}
