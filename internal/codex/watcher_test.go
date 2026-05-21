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

// Build a complete CODEX_HOME + cwd test environment with a marker pointing
// at a rollout file containing pre/post-compact tool activity.
func setupWatchEnv(t *testing.T) (cwd, sessionID, rolloutPath string) {
	t.Helper()

	codexHome := t.TempDir()
	cwd = t.TempDir()
	sessionID = "sess-watch-1"

	// Plant rollout file under CODEX_HOME/sessions/.../...jsonl
	dateDir := filepath.Join(codexHome, "sessions", "2026", "05", "19")
	if err := os.MkdirAll(dateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rolloutPath = filepath.Join(dateDir, "rollout-2026-05-19-"+sessionID+".jsonl")
	rollout := `{"timestamp":"2026-05-19T10:00:00.000Z","type":"session_meta","payload":{"session_id":"sess-watch-1","cwd":"/x"}}
{"timestamp":"2026-05-19T10:00:01.000Z","type":"response_item","payload":{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"command\":\"npm test\"}"}}
{"timestamp":"2026-05-19T10:00:02.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"12 passed","exit_code":0}}
{"timestamp":"2026-05-19T10:00:30.000Z","type":"context_compaction"}
`
	if err := os.WriteFile(rolloutPath, []byte(rollout), 0o644); err != nil {
		t.Fatal(err)
	}

	// Plant SessionStart marker the watcher will pick up.
	markerDir := filepath.Join(cwd, ".splice", "active-sessions")
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
	if err := os.WriteFile(filepath.Join(markerDir, sessionID+".json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CODEX_HOME", codexHome)
	return
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
	st, err := store.OpenSession(cwd, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, hash := FingerprintToolCall("shell", `{"command":"npm test"}`)
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
	_, hash := FingerprintToolCall("shell", `{"command":"go test"}`)
	waitForLogged(t, &logs, "froze 1 trail events", 2, 10*time.Second, done)
	stopWatcher(t, cancel, done, &logs)

	st, err := store.OpenSession(cwd, sessionID)
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
	markerPath := filepath.Join(cwd, ".splice", "active-sessions", sessionID+".json")
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

func TestWatcherShutdownDropsAllPending(t *testing.T) {
	cwd := t.TempDir()
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

	_, hash := FingerprintToolCall("Bash", `{"command":"npm test"}`)
	st, err := store.OpenSession(cwd, sessionID)
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

func TestWatcherInFlightAcrossCompactionEmitsRunningHit(t *testing.T) {
	cwd := t.TempDir()
	sessionID := "sess-watch-inflight"
	w, err := New(cwd)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSession(cwd, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	applyEvent(w, st, sessionID, mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:01.000Z","type":"response_item","payload":{"type":"function_call","call_id":"c-long","name":"shell","arguments":"{\"command\":\"python run_sim.py\"}"}}`,
	))
	applyEvent(w, st, sessionID, mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:30.000Z","type":"context_compaction"}`,
	))

	_, hash := FingerprintToolCall("shell", `{"command":"python run_sim.py"}`)
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
	st, err := store.OpenSession(cwd, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	applyEvent(w, st, sessionID, mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:01.000Z","type":"response_item","payload":{"type":"function_call","call_id":"c-long","name":"shell","arguments":"{\"command\":\"npm test\"}"}}`,
	))
	applyEvent(w, st, sessionID, mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:30.000Z","type":"context_compaction"}`,
	))
	applyEvent(w, st, sessionID, mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:31.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c-long","output":"12 passed after compact","exit_code":0}}`,
	))
	applyEvent(w, st, sessionID, mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:32.000Z","type":"response_item","payload":{"type":"function_call","call_id":"c-read","name":"Read","arguments":"{\"file_path\":\"/tmp/notes.md\"}"}}`,
	))
	applyEvent(w, st, sessionID, mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:33.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c-read","output":"post compact note"}}`,
	))

	_, hash := FingerprintToolCall("shell", `{"command":"npm test"}`)
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
	st, err := store.OpenSession(cwd, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	applyEvent(first, st, sessionID, mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:01.000Z","type":"response_item","payload":{"type":"function_call","call_id":"c-long","name":"shell","arguments":"{\"command\":\"npm test\"}"}}`,
	))
	applyEvent(first, st, sessionID, mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:30.000Z","type":"context_compaction"}`,
	))

	// Simulate watcher restart: pending call metadata is gone, but the frozen
	// running row still carries the rollout call_id.
	restarted, err := New(cwd)
	if err != nil {
		t.Fatal(err)
	}
	applyEvent(restarted, st, sessionID, mustParseEvent(t,
		`{"timestamp":"2026-05-19T10:00:31.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c-long","output":"12 passed after restart","exit_code":0}}`,
	))

	_, hash := FingerprintToolCall("shell", `{"command":"npm test"}`)
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

// TestWatcherSecondInstanceLocked verifies that running a second watcher
// against the same cwd while the first is still alive returns ErrLocked,
// preventing duplicate ingestion of the same rollout events.
func TestWatcherSecondInstanceLocked(t *testing.T) {
	cwd := t.TempDir()

	first, err := New(cwd)
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
		_, err := os.Stat(filepath.Join(cwd, ".splice", "codex-watch.lock"))
		return err == nil
	}, 1*time.Second)

	second, err := New(cwd)
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

	if _, err := os.Stat(filepath.Join(cwd, ".splice", "codex-watch.lock")); !os.IsNotExist(err) {
		t.Errorf("lock file should be removed on watcher exit, stat err=%v", err)
	}
}

// TestWatcherStaleLockReclaimed verifies that a lock file from a crashed
// process is reclaimed (rather than blocking forever).
func TestWatcherStaleLockReclaimed(t *testing.T) {
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".splice")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write a fake stale lock with a PID very unlikely to be alive.
	stalePath := filepath.Join(dir, "codex-watch.lock")
	if err := os.WriteFile(stalePath, []byte("99999999"), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := New(cwd)
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
	if _, err := New(""); err == nil {
		t.Fatal("expected error for empty cwd")
	}
}

func TestReadMarkersSkipsBrokenFiles(t *testing.T) {
	cwd := t.TempDir()
	dir := filepath.Join(cwd, ".splice", "active-sessions")
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

	markers, err := readMarkers(cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 1 || markers[0].SessionID != "s1" {
		t.Fatalf("unexpected markers: %+v", markers)
	}
}

func TestReadMarkersMissingDirIsEmpty(t *testing.T) {
	markers, err := readMarkers(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 0 {
		t.Fatalf("expected no markers, got %+v", markers)
	}
}

func TestWatcherHandlersIgnoreMalformedEvents(t *testing.T) {
	cwd := t.TempDir()
	w, err := New(cwd)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSession(cwd, "s")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	w.handleToolCall(st, "s", nil)
	w.handleToolCall(st, "s", &ToolCall{})
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
