package codex

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wang33550/splice/internal/store"
)

// MarkerInfo is what the SessionStart hook drops into the global
// ~/.splice/active-sessions directory.
// We re-declare the JSON shape here to avoid an import cycle with cmd/splice.
type MarkerInfo struct {
	SessionID      string `json:"session_id"`
	Source         string `json:"source"`
	TranscriptPath string `json:"transcript_path,omitempty"`
	Cwd            string `json:"cwd"`
	UpdatedAt      string `json:"updated_at"`
}

// Watcher tails Codex rollout JSONL files for every globally active session
// and replays compaction events into splice's per-session stores, providing
// the same PreCompact-driven trail mechanism that Claude Code natively offers.
type Watcher struct {
	PollInterval time.Duration
	Logger       func(format string, args ...any)

	mu       sync.Mutex
	sessions map[string]*sessionTail // session_id -> tail goroutine handle

	// pending pairs tool_call events to their tool_result by call_id.
	// Per-session map; one watcher process can be tailing many sessions
	// in parallel.
	pendingMu sync.Mutex
	pending   map[string]map[string]*pendingCall // session_id -> call_id -> pending
}

const orphanMarkerTTL = 24 * time.Hour

// New constructs a global Watcher with sensible defaults. The cwd argument is
// accepted for API compatibility but no longer scopes the watcher; project
// routing comes from each session marker's cwd metadata.
func New(_ string) (*Watcher, error) {
	return &Watcher{
		PollInterval: 1 * time.Second,
		Logger:       func(string, ...any) {},
		sessions:     map[string]*sessionTail{},
		pending:      map[string]map[string]*pendingCall{},
	}, nil
}

// Close is a no-op; per-session stores are owned by their tail goroutines
// and closed when those goroutines exit.
func (w *Watcher) Close() error { return nil }

// Run blocks until ctx is canceled. It periodically scans the global marker
// directory for new active sessions and starts/stops tail goroutines.
//
// Run also acquires a user-global lock file so two concurrent watchers don't
// both ingest the same rollout events. Returns ErrLocked when another watcher
// already holds it.
func (w *Watcher) Run(ctx context.Context) error {
	released, err := w.acquireLock()
	if err != nil {
		return err
	}
	defer released()

	w.Logger("splice codex-watch: starting globally in %s", store.HomeDir())
	defer w.Logger("splice codex-watch: stopped")

	t := time.NewTicker(w.PollInterval)
	defer t.Stop()

	w.refresh(ctx) // immediate first scan
	for {
		select {
		case <-ctx.Done():
			w.shutdownAll()
			return nil
		case <-t.C:
			w.refresh(ctx)
		}
	}
}

// ErrLocked is returned by Run when another watcher process is already
// active for this user. The caller is expected to surface this
// distinctly so the user knows they don't need a second watcher.
var ErrLocked = errors.New("watcher: another splice codex-watch is already running")

// acquireLock writes the global codex-watch.lock with our PID. If a lock file
// already exists and the recorded PID is still running, returns ErrLocked.
// Stale locks (from crashed processes) are reclaimed.
func (w *Watcher) acquireLock() (release func(), err error) {
	dir := store.HomeDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("watcher: mkdir splice dir: %w", err)
	}
	lockPath := store.WatcherLockPath()

	if existing, err := os.ReadFile(lockPath); err == nil {
		pidStr := strings.TrimSpace(string(existing))
		if pid, perr := strconv.Atoi(pidStr); perr == nil && pid > 0 {
			if isProcessAlive(pid) {
				return nil, fmt.Errorf("%w (pid %d)", ErrLocked, pid)
			}
			// Stale lock — owner crashed without cleanup. Reclaim.
			w.Logger("splice codex-watch: reclaiming stale lock (pid %d not running)", pid)
		}
	}

	pid := os.Getpid()
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return nil, fmt.Errorf("watcher: write lock: %w", err)
	}
	return func() { _ = os.Remove(lockPath) }, nil
}

// isProcessAlive is implemented per-platform in process_unix.go and
// process_windows.go.

func (w *Watcher) refresh(ctx context.Context) {
	markers, err := readMarkers()
	if err != nil {
		w.Logger("splice codex-watch: read markers: %v", err)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	seen := map[string]struct{}{}
	for _, m := range markers {
		seen[m.SessionID] = struct{}{}
		rolloutPath := ""
		if existing, exists := w.sessions[m.SessionID]; exists {
			if tailDone(existing) {
				delete(w.sessions, m.SessionID)
				w.dropSessionPending(m.SessionID)
				w.Logger("splice codex-watch: session %s tail stopped, restarting", m.SessionID)
			} else if existing.marker.UpdatedAt == m.UpdatedAt && existing.marker.Cwd == m.Cwd && existing.marker.Source == m.Source {
				var err error
				rolloutPath, err = FindRolloutFile(m.SessionID)
				if err != nil || rolloutPath == existing.path {
					continue
				}
				w.Logger("splice codex-watch: session %s rollout changed, restarting tail", m.SessionID)
				existing.cancel()
				waitTailDone(existing)
				delete(w.sessions, m.SessionID)
				w.dropSessionPending(m.SessionID)
			} else {
				existing.cancel()
				waitTailDone(existing)
				delete(w.sessions, m.SessionID)
				w.dropSessionPending(m.SessionID)
			}
		}
		// New active session — find its rollout file and start tailing.
		if rolloutPath == "" {
			var err error
			rolloutPath, err = FindRolloutFile(m.SessionID)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) && w.removeStaleOrphanMarker(m) {
					continue
				}
				// Codex may not have flushed the rollout file yet on a brand-new
				// session. We'll retry on the next refresh cycle.
				w.Logger("splice codex-watch: rollout for %s not found yet: %v", m.SessionID, err)
				continue
			}
		}
		ts := startTail(ctx, w, m, rolloutPath)
		w.sessions[m.SessionID] = ts
		w.Logger("splice codex-watch: tailing %s [%s] -> %s", m.SessionID, store.ProjectKey(m.Cwd), rolloutPath)
	}
	// Sessions whose markers are gone — codex exited cleanly. Stop their tails.
	for id, ts := range w.sessions {
		if _, ok := seen[id]; ok {
			continue
		}
		ts.cancel()
		waitTailDone(ts)
		delete(w.sessions, id)
		w.dropSessionPending(id)
		w.Logger("splice codex-watch: session %s gone, stopped tail", id)
	}
}

func tailDone(ts *sessionTail) bool {
	select {
	case <-ts.done:
		return true
	default:
		return false
	}
}

func (w *Watcher) removeStaleOrphanMarker(m MarkerInfo) bool {
	updatedAt, err := time.Parse(time.RFC3339Nano, m.UpdatedAt)
	if err != nil {
		return false
	}
	if time.Since(updatedAt) < orphanMarkerTTL {
		return false
	}
	markerPath := filepath.Join(store.ActiveSessionsDir(), store.SessionFileBase(m.SessionID)+".json")
	if err := os.Remove(markerPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			w.Logger("splice codex-watch: remove stale marker %s: %v", m.SessionID, err)
		}
		return false
	}
	w.Logger("splice codex-watch: removed stale marker %s (no rollout file)", m.SessionID)
	return true
}

func (w *Watcher) shutdownAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, ts := range w.sessions {
		ts.cancel()
		waitTailDone(ts)
	}
	w.sessions = map[string]*sessionTail{}
	w.dropAllPending()
}

// readMarkers loads every global active-sessions/*.json marker. We
// tolerate broken/empty files (skip them silently) so a half-written marker
// from a still-running hook doesn't take the watcher down.
func readMarkers() ([]MarkerInfo, error) {
	dir := store.ActiveSessionsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []MarkerInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var m MarkerInfo
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		if m.SessionID == "" {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// ---------------------------------------------------------------------
// Per-session tail
// ---------------------------------------------------------------------

type sessionTail struct {
	cancel context.CancelFunc
	done   chan struct{}
	marker MarkerInfo
	path   string
}

// startTail launches a goroutine that owns one session's store and tails
// the rollout file. The goroutine resumes from the last persisted offset
// (so a watcher restart skips already-applied events), then follows the
// file for new lines.
func startTail(parent context.Context, w *Watcher, marker MarkerInfo, path string) *sessionTail {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := tailLoop(ctx, w, marker, path); err != nil && !errors.Is(err, context.Canceled) {
			w.Logger("splice codex-watch: %s tail error: %v", marker.SessionID, err)
		}
	}()
	return &sessionTail{cancel: cancel, done: done, marker: marker, path: path}
}

func waitTailDone(ts *sessionTail) {
	select {
	case <-ts.done:
	case <-time.After(2 * time.Second):
	}
}

func tailLoop(ctx context.Context, w *Watcher, marker MarkerInfo, path string) error {
	st, err := store.OpenSession(marker.SessionID)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	// Resume from the last persisted offset if any. This skips events
	// that an earlier watcher run already applied — without it, every
	// restart would re-append the entire rollout into live_trail.
	startOffset, err := st.LastRolloutOffset()
	if err != nil {
		return fmt.Errorf("read offset: %w", err)
	}
	currentSize, err := fileSize(path)
	if err != nil {
		return err
	}
	startOffset, currentSize = w.reconcileRolloutCursor(st, marker.SessionID, path, startOffset, currentSize)

	// Initial replay from startOffset to current EOF.
	var initial []Event
	if startOffset > 0 {
		initial, err = ReadFromOffset(path, startOffset)
	} else {
		initial, err = ReadAll(path, 0)
	}
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	scope := store.ProjectKey(marker.Cwd)
	for _, ev := range initial {
		scope = applyEventScoped(w, st, marker.SessionID, scope, ev)
	}
	currentOffset, err := fileSize(path)
	if err != nil {
		return err
	}
	if err := persistRolloutCursor(st, path, currentOffset); err != nil {
		w.Logger("splice codex-watch: persist offset: %v", err)
	}

	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			size, err := fileSize(path)
			if err != nil {
				return err
			}
			currentOffset, size = w.reconcileRolloutCursor(st, marker.SessionID, path, currentOffset, size)
			if size <= currentOffset {
				continue
			}
			newEvents, err := ReadFromOffset(path, currentOffset)
			if err != nil {
				return err
			}
			for _, ev := range newEvents {
				scope = applyEventScoped(w, st, marker.SessionID, scope, ev)
			}
			currentOffset = size
			if err := persistRolloutCursor(st, path, currentOffset); err != nil {
				w.Logger("splice codex-watch: persist offset: %v", err)
			}
		}
	}
}

func (w *Watcher) reconcileRolloutCursor(st *store.Store, sessionID, path string, offset, size int64) (int64, int64) {
	if offset == 0 {
		return offset, size
	}
	storedHash, err := st.LastRolloutCursorHash()
	if err != nil {
		w.Logger("splice codex-watch: read cursor hash: %v", err)
	}
	currentHash := ""
	if err == nil && storedHash != "" && offset <= size {
		currentHash, err = CursorHash(path, offset, rolloutCursorWindow)
		if err != nil {
			w.Logger("splice codex-watch: read cursor bytes: %v", err)
		}
	}
	if offset > size || (storedHash != "" && currentHash != "" && storedHash != currentHash) {
		reason := fmt.Sprintf("offset %d beyond file size %d", offset, size)
		if offset <= size {
			reason = fmt.Sprintf("cursor hash changed at offset %d", offset)
		}
		w.Logger("splice codex-watch: %s rollout %s; replaying from start", sessionID, reason)
		w.dropSessionPending(sessionID)
		offset = 0
		if err := st.ClearLiveTrailForRolloutReset(); err != nil {
			w.Logger("splice codex-watch: persist offset reset: %v", err)
		}
		size, _ = fileSize(path)
	}
	return offset, size
}

const rolloutCursorWindow = 4096

func persistRolloutCursor(st *store.Store, path string, offset int64) error {
	hash, err := CursorHash(path, offset, rolloutCursorWindow)
	if err != nil {
		return err
	}
	return st.SetRolloutCursor(offset, hash)
}

func fileSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// applyEvent dispatches a parsed rollout event to the right handler.
// `st` is the per-session store handle owned by the tail goroutine.
func applyEvent(w *Watcher, st *store.Store, sessionID string, ev Event) {
	_ = applyEventScoped(w, st, sessionID, "", ev)
}

func applyEventScoped(w *Watcher, st *store.Store, sessionID, scope string, ev Event) string {
	switch ev.Kind {
	case KindSessionMeta:
		// Rollout metadata is part of the historical stream being replayed,
		// while the marker is only the current discovery/routing hint. Prefer
		// rollout cwd when present so a current project switch cannot re-label
		// older events from another project.
		if ev.SessionMeta != nil && ev.SessionMeta.CwdPresent {
			return store.ProjectKey(ev.SessionMeta.Cwd)
		}
	case KindToolCall:
		w.handleToolCall(st, sessionID, scope, ev.ToolCall)
	case KindToolResult:
		w.handleToolResult(st, sessionID, ev.ToolResult)
	case KindCompaction:
		w.handleCompaction(st, sessionID, ev)
	}
	return scope
}

// pendingCall buffers an unmatched tool_call until its tool_result arrives.
type pendingCall struct {
	ToolName  string
	ArgsJSON  string
	Canonical string
	Hash      string
	Label     string
	StartedAt time.Time
}

func (w *Watcher) handleToolCall(st *store.Store, sessionID, scope string, c *ToolCall) {
	if c == nil || c.CallID == "" {
		return
	}
	canonical, hash := FingerprintToolCallScoped(c.ToolName, c.ArgsJSON, scope)
	label := LabelFromToolCall(c.ToolName, c.ArgsJSON)
	callID := rolloutCallID(c.CallID)

	w.pendingMu.Lock()
	m, ok := w.pending[sessionID]
	if !ok {
		m = map[string]*pendingCall{}
		w.pending[sessionID] = m
	}
	m[c.CallID] = &pendingCall{
		ToolName:  toolNameForStore(c.ToolName),
		ArgsJSON:  canonical,
		Canonical: canonical,
		Hash:      hash,
		Label:     label,
		StartedAt: c.Timestamp,
	}
	w.pendingMu.Unlock()

	// Write the running edge immediately. Codex exposes compaction only via
	// rollout tailing, so a context_compaction can land between a tool_call and
	// its later tool_result. If we only persisted completed pairs, that boundary
	// would lose the long-task in-flight fact entirely.
	if err := st.BeginLiveTrailEntry(store.TrailEntry{
		CallID:     callID,
		ToolName:   toolNameForStore(c.ToolName),
		ArgsHash:   hash,
		ArgsJSON:   canonical,
		Label:      label,
		Status:     store.StatusRunning,
		RecordedAt: c.Timestamp,
	}); err != nil {
		w.Logger("splice codex-watch: begin trail: %v", err)
	}
}

func (w *Watcher) handleToolResult(st *store.Store, sessionID string, r *ToolResult) {
	if r == nil || r.CallID == "" {
		return
	}

	var ec sql.NullInt64
	if r.ExitCode != nil {
		ec = sql.NullInt64{Int64: int64(*r.ExitCode), Valid: true}
	}
	callID := rolloutCallID(r.CallID)
	if err := st.FinishLiveTrailEntry(callID, ec, r.Output, r.Status, r.Timestamp); err == nil {
		w.dropPending(sessionID, r.CallID)
		return
	} else if !store.IsErrNoRunningRow(err) {
		w.Logger("splice codex-watch: finish trail: %v", err)
		return
	}

	pending := w.takePending(sessionID, r.CallID)
	if pending == nil {
		if err := st.AppendTerminalFromFrozenRunning(callID, ec, r.Output, r.Status, r.Timestamp); err != nil {
			if !store.IsErrNoRunningRow(err) {
				w.Logger("splice codex-watch: append frozen terminal: %v", err)
			}
		}
		return
	}

	if err := st.AppendLiveTrail(store.TrailEntry{
		CallID:     callID,
		ToolName:   pending.ToolName,
		ArgsHash:   pending.Hash,
		ArgsJSON:   pending.Canonical,
		Label:      pending.Label,
		ExitCode:   ec,
		Output:     r.Output,
		Status:     r.Status,
		RecordedAt: r.Timestamp,
	}); err != nil {
		w.Logger("splice codex-watch: append trail: %v", err)
	}
}

func (w *Watcher) takePending(sessionID, callID string) *pendingCall {
	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()
	m := w.pending[sessionID]
	if m == nil {
		return nil
	}
	pending := m[callID]
	delete(m, callID)
	if len(m) == 0 {
		delete(w.pending, sessionID)
	}
	return pending
}

func (w *Watcher) dropPending(sessionID, callID string) {
	_ = w.takePending(sessionID, callID)
}

func (w *Watcher) dropSessionPending(sessionID string) {
	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()
	delete(w.pending, sessionID)
}

func (w *Watcher) dropAllPending() {
	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()
	w.pending = map[string]map[string]*pendingCall{}
}

func rolloutCallID(callID string) string {
	return "codex-rollout:" + callID
}

func (w *Watcher) handleCompaction(st *store.Store, sessionID string, ev Event) {
	rows, err := st.FreezePreCompact()
	if err != nil {
		w.Logger("splice codex-watch: freeze: %v", err)
		return
	}
	w.Logger("splice codex-watch: froze %d trail events for session %s",
		rows, sessionID)
	_ = ev // timestamp no longer carried into the snapshot — recorded_at on
	//        rows is what survives, and per-session schema doesn't carry a
	//        snapshot_at column anymore.
}

// toolNameForStore folds Codex's "shell" tool into "Bash" so the trail rows
// are interchangeable with Claude Code's data and the fence rule treats them
// identically.
func toolNameForStore(name string) string {
	if name == "shell" {
		return "Bash"
	}
	return name
}

// CompactStdoutLogger writes watcher diagnostics in a stable single-line
// format to the given writer, suitable for `splice codex-watch &` to leave
// in a terminal.
func CompactStdoutLogger(w io.Writer) func(string, ...any) {
	return func(format string, args ...any) {
		ts := time.Now().Format("2006-01-02 15:04:05")
		fmt.Fprintf(w, "[%s] ", ts)
		fmt.Fprintf(w, format, args...)
		if !strings.HasSuffix(format, "\n") {
			fmt.Fprintln(w)
		}
	}
}
