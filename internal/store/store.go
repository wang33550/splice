// Package store persists splice's per-session trail. Each conversation
// gets its own SQLite database under <cwd>/.splice/sessions/<sid>.db so
// that:
//
//   - rollback / cleanup is a single file delete (e.g. on /clear)
//   - no row carries a session_id discriminator — the file IS the scope
//   - long-lived projects don't accumulate dead sessions inside a shared DB
//
// Each session DB has these logical tables:
//
//   - live_trail:        events from the most recent compaction up to "now"
//     (or from session start if nothing has been compacted
//     yet). PreToolUse appends a row at status='running';
//     PostToolUse flips it to a terminal status.
//   - pre_compact_trail: the events that lived in live_trail at the moment
//     of the most recent compaction. Replaced wholesale on
//     each PreCompact freeze — splice keeps at most one
//     snapshot per session, since real users almost never
//     try to re-run a command from before the previous
//     compaction (that work is on a different topic).
//   - cooldown:          (args_hash) entries that splice has already
//     intercepted in the current post-compact window.
//     Cleared on the next freeze so a new compaction
//     starts a fresh window.
//   - meta:              key/value bag for the eviction counter and the
//     watcher's last_offset (codex rollout tail).
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "modernc.org/sqlite"
)

const trailSchemaVersion = 4

const schema = `
CREATE TABLE IF NOT EXISTS schema_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- live_trail accumulates events between PreCompact boundaries.
-- A row's call_id is set by PreToolUse so PostToolUse can find and
-- finish the right row; rows arriving via AppendLiveTrail (watcher
-- replay or sidecar-missing fallback) carry an empty call_id.
CREATE TABLE IF NOT EXISTS live_trail (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    call_id     TEXT,
    seq         INTEGER NOT NULL,
    tool_name   TEXT NOT NULL,
    args_hash   TEXT NOT NULL,
    args_json   TEXT NOT NULL,
    label       TEXT NOT NULL DEFAULT '',
    exit_code   INTEGER,
    output      TEXT,
    status      TEXT NOT NULL DEFAULT 'running',
    recorded_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_live_trail_seq ON live_trail(seq);
CREATE INDEX IF NOT EXISTS idx_live_trail_call_id ON live_trail(call_id);
CREATE INDEX IF NOT EXISTS idx_live_trail_hash ON live_trail(args_hash);

-- pre_compact_trail holds at most one frozen snapshot. FreezePreCompact
-- truncates this table before copying rows over.
CREATE TABLE IF NOT EXISTS pre_compact_trail (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    call_id     TEXT DEFAULT '',
    seq         INTEGER NOT NULL,
    tool_name   TEXT NOT NULL,
    args_hash   TEXT NOT NULL,
    args_json   TEXT NOT NULL,
    label       TEXT NOT NULL DEFAULT '',
    exit_code   INTEGER,
    output      TEXT,
    status      TEXT NOT NULL DEFAULT 'ok',
    recorded_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pct_hash ON pre_compact_trail(args_hash);
CREATE INDEX IF NOT EXISTS idx_pct_seq ON pre_compact_trail(seq);

CREATE TABLE IF NOT EXISTS cooldown (
    args_hash TEXT PRIMARY KEY,
    added_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// Status values for live_trail / pre_compact_trail rows. Anything other
// than StatusOK disqualifies a row from being served as a cache hit
// (failed/interrupted runs aren't reusable). StatusRunning is the
// in-flight signal: a row still 'running' when freeze happens means the
// task crossed the compaction boundary without producing a result.
const (
	StatusOK          = "ok"
	StatusError       = "error"
	StatusInterrupted = "interrupted"
	StatusTimeout     = "timeout"
	StatusUnknown     = "unknown"
	StatusRunning     = "running"
)

// Store is a handle to one session's SQLite file. Open with OpenSession.
type Store struct {
	db   *sql.DB
	path string // <cwd>/.splice/sessions/<sid>.db
}

// SessionsDir returns <cwd>/.splice/sessions, creating it if needed.
func SessionsDir(cwd string) string {
	return filepath.Join(cwd, ".splice", "sessions")
}

// SessionDBPath returns the .db path for a given session.
func SessionDBPath(cwd, sessionID string) string {
	return filepath.Join(SessionsDir(cwd), sessionID+".db")
}

// SessionMetaPath returns the side-car JSON metadata path for a session.
// (Stored separately from the .db so external tools / future watchers
// can read it without opening SQLite.)
func SessionMetaPath(cwd, sessionID string) string {
	return filepath.Join(SessionsDir(cwd), sessionID+".meta.json")
}

// OpenSession opens (or creates) the per-session DB.
func OpenSession(cwd, sessionID string) (*Store, error) {
	if cwd == "" {
		return nil, errors.New("store: empty cwd")
	}
	if sessionID == "" {
		return nil, errors.New("store: empty session id")
	}
	dir := SessionsDir(cwd)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: mkdir sessions: %w", err)
	}
	path := SessionDBPath(cwd, sessionID)
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: init schema: %w", err)
	}
	if err := migrateSchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate schema: %w", err)
	}
	if _, err := db.Exec(
		`INSERT OR REPLACE INTO schema_meta (key, value) VALUES ('trail_schema_version', ?)`,
		fmt.Sprint(trailSchemaVersion),
	); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: write schema_meta: %w", err)
	}
	return &Store{db: db, path: path}, nil
}

func migrateSchema(db *sql.DB) error {
	hasCallID, err := tableHasColumn(db, "pre_compact_trail", "call_id")
	if err != nil {
		return err
	}
	if !hasCallID {
		if _, err := db.Exec(`ALTER TABLE pre_compact_trail ADD COLUMN call_id TEXT DEFAULT ''`); err != nil {
			return err
		}
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_pct_call_id ON pre_compact_trail(call_id)`)
	return err
}

func tableHasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Path returns the on-disk DB path. Useful for tests and for Drop.
func (s *Store) Path() string { return s.path }

// Close releases the SQLite handle.
func (s *Store) Close() error { return s.db.Close() }

// Drop closes and deletes this session's files. Idempotent.
// Used on /clear and when a session's rollout has been deleted by the
// user (GC). Removes the .db, the WAL/SHM sidecars, and meta.json.
func (s *Store) Drop() error {
	_ = s.db.Close()
	return DropSessionFiles(s.path)
}

// DropSessionFiles deletes the .db, its WAL/SHM siblings, and the meta
// file given a base .db path. Exposed for the GC path that doesn't have
// an open Store handle.
func DropSessionFiles(dbPath string) error {
	siblings := []string{
		dbPath,
		dbPath + "-wal",
		dbPath + "-shm",
		metaPathFromDB(dbPath),
	}
	var firstErr error
	for _, p := range siblings {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func metaPathFromDB(dbPath string) string {
	base := dbPath
	if filepath.Ext(base) == ".db" {
		base = base[:len(base)-len(".db")]
	}
	return base + ".meta.json"
}

// ----------------------------------------------------------------------
// live_trail: PreToolUse begins, PostToolUse finishes (or AppendLiveTrail
// stuffs a fresh terminal row when the sidecar's gone).
// ----------------------------------------------------------------------

type TrailEntry struct {
	Seq        int64
	CallID     string // empty for AppendLiveTrail rows
	ToolName   string
	ArgsHash   string
	ArgsJSON   string
	Label      string
	ExitCode   sql.NullInt64
	Output     string
	Status     string
	RecordedAt time.Time
}

// BeginLiveTrailEntry inserts a row at status='running'. Caller mints
// the call_id and stashes it in a sidecar so PostToolUse can find it.
func (s *Store) BeginLiveTrailEntry(e TrailEntry) error {
	if e.Status == "" {
		e.Status = StatusRunning
	}
	_, err := s.db.Exec(
		`INSERT INTO live_trail (call_id, seq, tool_name, args_hash, args_json, label, status, recorded_at)
		 VALUES (?, COALESCE((SELECT MAX(seq) FROM live_trail), 0) + 1, ?, ?, ?, ?, ?, ?)`,
		e.CallID,
		e.ToolName, e.ArgsHash, e.ArgsJSON, e.Label,
		e.Status,
		e.RecordedAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

// FinishLiveTrailEntry flips a 'running' row to a terminal status by
// call_id. Returns errNoRunningRow if no such row matched, so callers
// can fall back to AppendLiveTrail.
func (s *Store) FinishLiveTrailEntry(callID string, exitCode sql.NullInt64, output, status string, finishedAt time.Time) error {
	if status == "" {
		status = StatusOK
	}
	if callID == "" {
		return errNoRunningRow
	}
	res, err := s.db.Exec(
		`UPDATE live_trail
		   SET exit_code = ?, output = ?, status = ?
		 WHERE call_id = ? AND status = ?`,
		exitCode, output, status,
		callID, StatusRunning,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		_ = finishedAt
		return nil
	}
	return errNoRunningRow
}

// AppendTerminalFromFrozenRunning finds a running snapshot row by call_id and
// appends a terminal live_trail row with the same fingerprint metadata. This
// is used when a watcher restart observes the result after compaction but has
// lost the in-memory tool_call metadata needed for a normal AppendLiveTrail.
func (s *Store) AppendTerminalFromFrozenRunning(callID string, exitCode sql.NullInt64, output, status string, recordedAt time.Time) error {
	if status == "" {
		status = StatusOK
	}
	if callID == "" {
		return errNoRunningRow
	}
	row := s.db.QueryRow(
		`SELECT tool_name, args_hash, args_json, label
		   FROM pre_compact_trail
		  WHERE call_id = ? AND status = ?
		  ORDER BY seq DESC LIMIT 1`,
		callID, StatusRunning,
	)
	var e TrailEntry
	if err := row.Scan(&e.ToolName, &e.ArgsHash, &e.ArgsJSON, &e.Label); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errNoRunningRow
		}
		return err
	}
	e.CallID = callID
	e.ExitCode = exitCode
	e.Output = output
	e.Status = status
	e.RecordedAt = recordedAt
	return s.AppendLiveTrail(e)
}

var errNoRunningRow = errors.New("store: no in-flight live_trail row matched call_id")

// IsErrNoRunningRow lets callers distinguish "nothing to update" from
// real DB errors so they can fall back to AppendLiveTrail.
func IsErrNoRunningRow(err error) bool { return errors.Is(err, errNoRunningRow) }

// AppendLiveTrail inserts an already-completed event row. Used by the
// codex watcher (which sees call+result paired in the rollout) and by
// the PostToolUse fallback when the sidecar is missing.
func (s *Store) AppendLiveTrail(e TrailEntry) error {
	if e.Status == "" {
		e.Status = StatusOK
	}
	_, err := s.db.Exec(
		`INSERT INTO live_trail (call_id, seq, tool_name, args_hash, args_json, label, exit_code, output, status, recorded_at)
		 VALUES (?, COALESCE((SELECT MAX(seq) FROM live_trail), 0) + 1, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.CallID,
		e.ToolName, e.ArgsHash, e.ArgsJSON, e.Label,
		e.ExitCode, e.Output, e.Status,
		e.RecordedAt.UTC().Format(time.RFC3339Nano),
	)
	return err
}

// ----------------------------------------------------------------------
// PreCompact freeze: replace the (single) snapshot in pre_compact_trail
// with the current live_trail contents; clear live_trail; clear
// cooldown; reset eviction counter.
// ----------------------------------------------------------------------

// FreezePreCompact returns the number of rows copied from live_trail
// into pre_compact_trail.
func (s *Store) FreezePreCompact() (rowsCopied int, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	// Replace the prior snapshot wholesale. Real users essentially never
	// try to re-use commands from across two compaction boundaries, so
	// keeping multiple snapshots adds complexity without payoff.
	if _, err = tx.Exec(`DELETE FROM pre_compact_trail`); err != nil {
		return 0, err
	}
	res, err := tx.Exec(
		`INSERT INTO pre_compact_trail
		   (call_id, seq, tool_name, args_hash, args_json, label, exit_code, output, status, recorded_at)
		 SELECT call_id, seq, tool_name, args_hash, args_json, label, exit_code, output, status, recorded_at
		 FROM live_trail
		 ORDER BY seq`,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()

	if _, err = tx.Exec(`DELETE FROM live_trail`); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(`DELETE FROM cooldown`); err != nil {
		return 0, err
	}
	if err = writeMetaInt(tx, evictionKey, 0); err != nil {
		return 0, err
	}

	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// HasPreCompactTrail reports whether this session has any frozen rows.
func (s *Store) HasPreCompactTrail() (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM pre_compact_trail`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ----------------------------------------------------------------------
// LookupCachedHit: the post-compact decision rule.
//
// Match P = latest pre_compact_trail entry with the given args_hash.
// Outcomes:
//
//  1. P terminal (status=ok, exit==0, cacheable tool) and no fence
//     event sits between P and the present moment → cache hit. Caller
//     serves it via ask/deny and receives every later pre-compaction
//     snapshot row in AfterEvents so it can restore the full recent causal
//     trail, not just P's output. Fence range = "in pre_compact_trail after
//     P" ∪ "everything in live_trail" — because users almost always
//     edit files between two compactions, an Edit/Write/side-effect
//     Bash *anywhere* in live_trail invalidates the cached answer.
//
//  2. P.status == 'running' → in-flight match. Caller informs the
//     model that the prior task started but never finished; AfterEvents
//     is populated with everything in pre_compact_trail after P, so
//     the model can see what the meantime did.
//
//  3. P doesn't exist, P failed, P is non-cacheable, or P is fenced
//     → return nil (no hit; caller lets the call through).
//
// ----------------------------------------------------------------------

func (s *Store) LookupCachedHit(argsHash string, fenceBashFn func(command string) bool, cacheableBashFn ...func(command string) bool) (*HitResult, error) {
	row := s.db.QueryRow(
		`SELECT seq, COALESCE(call_id,''), tool_name, args_hash, args_json, label, exit_code, COALESCE(output,''), status, recorded_at
		 FROM pre_compact_trail
		 WHERE args_hash = ?
		 ORDER BY seq DESC LIMIT 1`,
		argsHash,
	)
	var e TrailEntry
	var ts string
	if err := row.Scan(&e.Seq, &e.CallID, &e.ToolName, &e.ArgsHash, &e.ArgsJSON, &e.Label, &e.ExitCode, &e.Output, &e.Status, &ts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return nil, fmt.Errorf("store: parse recorded_at: %w", err)
	}
	e.RecordedAt = t

	// Case 2: in-flight (long task that never finished before compact).
	if e.Status == StatusRunning {
		if liveHit, observed, err := s.lookupLiveTerminalHit(e, argsHash, fenceBashFn, cacheableBashFn...); err != nil {
			return nil, err
		} else if liveHit != nil {
			return liveHit, nil
		} else if observed {
			return nil, nil
		}
		if priorHit, observed, err := s.lookupPriorSnapshotTerminalHit(e, argsHash, fenceBashFn, cacheableBashFn...); err != nil {
			return nil, err
		} else if priorHit != nil {
			return priorHit, nil
		} else if observed {
			return nil, nil
		}
		after, err := s.eventsAfterInSnapshot(e.Seq)
		if err != nil {
			return nil, err
		}
		return &HitResult{Entry: e, InFlight: true, AfterEvents: after}, nil
	}

	// Case 3 filters: terminal but unsuitable.
	if e.Status != StatusOK {
		return nil, nil
	}
	if e.ExitCode.Valid && e.ExitCode.Int64 != 0 {
		return nil, nil
	}
	if !IsCacheable(e.ToolName) {
		return nil, nil
	}
	if e.ToolName == "Bash" && len(cacheableBashFn) > 0 && cacheableBashFn[0] != nil && !cacheableBashFn[0](e.Label) {
		return nil, nil
	}

	// Case 1 fence check: walk events strictly after P in the snapshot
	// AND every event in live_trail. Either side seeing a fence → miss.
	fenced, err := s.snapshotFencedAfter(e.Seq, fenceBashFn)
	if err != nil {
		return nil, err
	}
	if fenced {
		return nil, nil
	}
	fenced, err = s.liveTrailFenced(fenceBashFn)
	if err != nil {
		return nil, err
	}
	if fenced {
		return nil, nil
	}
	after, err := s.eventsAfterInSnapshot(e.Seq)
	if err != nil {
		return nil, err
	}
	return &HitResult{Entry: e, InFlight: false, AfterEvents: after}, nil
}

func (s *Store) lookupLiveTerminalHit(running TrailEntry, argsHash string, fenceBashFn func(command string) bool, cacheableBashFn ...func(command string) bool) (*HitResult, bool, error) {
	row := s.db.QueryRow(
		`SELECT seq, tool_name, args_hash, args_json, label, exit_code, COALESCE(output,''), status, recorded_at
		 FROM live_trail
		 WHERE args_hash = ? AND status != ?
		 ORDER BY seq DESC LIMIT 1`,
		argsHash, StatusRunning,
	)
	var e TrailEntry
	var ts string
	if err := row.Scan(&e.Seq, &e.ToolName, &e.ArgsHash, &e.ArgsJSON, &e.Label, &e.ExitCode, &e.Output, &e.Status, &ts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return nil, true, fmt.Errorf("store: parse recorded_at: %w", err)
	}
	e.RecordedAt = t
	if e.Status != StatusOK {
		return nil, true, nil
	}
	if e.ExitCode.Valid && e.ExitCode.Int64 != 0 {
		return nil, true, nil
	}
	if !IsCacheable(e.ToolName) {
		return nil, true, nil
	}
	if e.ToolName == "Bash" && len(cacheableBashFn) > 0 && cacheableBashFn[0] != nil && !cacheableBashFn[0](e.Label) {
		return nil, true, nil
	}
	fenced, err := s.snapshotFencedAfter(running.Seq, fenceBashFn)
	if err != nil {
		return nil, true, err
	}
	if fenced {
		return nil, true, nil
	}
	fenced, err = s.liveTrailFencedBefore(e.Seq, fenceBashFn)
	if err != nil {
		return nil, true, err
	}
	if fenced {
		return nil, true, nil
	}
	fenced, err = s.liveTrailFencedAfter(e.Seq, fenceBashFn)
	if err != nil {
		return nil, true, err
	}
	if fenced {
		return nil, true, nil
	}
	after, err := s.eventsAfterInSnapshot(running.Seq)
	if err != nil {
		return nil, true, err
	}
	beforeLive, err := s.eventsBeforeInLiveTrail(e.Seq)
	if err != nil {
		return nil, true, err
	}
	after = append(after, beforeLive...)
	afterLive, err := s.eventsAfterInLiveTrail(e.Seq)
	if err != nil {
		return nil, true, err
	}
	after = append(after, afterLive...)
	return &HitResult{Entry: e, InFlight: false, AfterEvents: after}, true, nil
}

func (s *Store) lookupPriorSnapshotTerminalHit(running TrailEntry, argsHash string, fenceBashFn func(command string) bool, cacheableBashFn ...func(command string) bool) (*HitResult, bool, error) {
	row := s.db.QueryRow(
		`SELECT seq, COALESCE(call_id,''), tool_name, args_hash, args_json, label, exit_code, COALESCE(output,''), status, recorded_at
		 FROM pre_compact_trail
		 WHERE args_hash = ? AND seq < ? AND status != ?
		 ORDER BY seq DESC LIMIT 1`,
		argsHash, running.Seq, StatusRunning,
	)
	var e TrailEntry
	var ts string
	if err := row.Scan(&e.Seq, &e.CallID, &e.ToolName, &e.ArgsHash, &e.ArgsJSON, &e.Label, &e.ExitCode, &e.Output, &e.Status, &ts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return nil, true, fmt.Errorf("store: parse recorded_at: %w", err)
	}
	e.RecordedAt = t
	if !isDuplicateSourceRunning(running, e) {
		return nil, false, nil
	}
	interveningRunning, err := s.hasInterveningSameHashRunning(e.Seq, running.Seq, argsHash)
	if err != nil {
		return nil, false, err
	}
	if interveningRunning {
		return nil, false, nil
	}
	hit, ok, err := s.terminalSnapshotHit(e, fenceBashFn, cacheableBashFn...)
	return hit, ok, err
}

func isDuplicateSourceRunning(running, prior TrailEntry) bool {
	return isCodexRolloutCallID(running.CallID) && !isCodexRolloutCallID(prior.CallID)
}

func isCodexRolloutCallID(callID string) bool {
	return len(callID) > len("codex-rollout:") && callID[:len("codex-rollout:")] == "codex-rollout:"
}

func (s *Store) hasInterveningSameHashRunning(afterSeq, beforeSeq int64, argsHash string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*)
		   FROM pre_compact_trail
		  WHERE args_hash = ? AND status = ? AND seq > ? AND seq < ?`,
		argsHash, StatusRunning, afterSeq, beforeSeq,
	).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Store) terminalSnapshotHit(e TrailEntry, fenceBashFn func(command string) bool, cacheableBashFn ...func(command string) bool) (*HitResult, bool, error) {
	if e.Status != StatusOK {
		return nil, true, nil
	}
	if e.ExitCode.Valid && e.ExitCode.Int64 != 0 {
		return nil, true, nil
	}
	if !IsCacheable(e.ToolName) {
		return nil, true, nil
	}
	if e.ToolName == "Bash" && len(cacheableBashFn) > 0 && cacheableBashFn[0] != nil && !cacheableBashFn[0](e.Label) {
		return nil, true, nil
	}
	fenced, err := s.snapshotFencedBetween(e.Seq, e.ArgsHash, fenceBashFn)
	if err != nil {
		return nil, true, err
	}
	if fenced {
		return nil, true, nil
	}
	fenced, err = s.liveTrailFenced(fenceBashFn)
	if err != nil {
		return nil, true, err
	}
	if fenced {
		return nil, true, nil
	}
	after, err := s.eventsAfterInSnapshotSkippingDuplicateRunning(e.Seq, e.ArgsHash)
	if err != nil {
		return nil, true, err
	}
	return &HitResult{Entry: e, InFlight: false, AfterEvents: after}, true, nil
}

// HitResult is returned by LookupCachedHit. AfterEvents is only
// populated with the snapshot rows after Entry and before the compaction
// boundary. Terminal hits use it to restore the model's full recent context;
// in-flight hits use it as the causal chain after the unfinished task.
type HitResult struct {
	Entry       TrailEntry
	InFlight    bool
	AfterEvents []TrailEntry
}

// snapshotFencedAfter returns true if any event after seq in the
// snapshot is a fence (Edit/Write/NotebookEdit, or side-effect Bash).
func (s *Store) snapshotFencedAfter(seq int64, fenceBashFn func(command string) bool) (bool, error) {
	rows, err := s.db.Query(
		`SELECT tool_name, label FROM pre_compact_trail
		 WHERE seq > ?
		 ORDER BY seq`,
		seq,
	)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return scanForFence(rows, fenceBashFn)
}

func (s *Store) snapshotFencedBetween(afterSeq int64, sameArgsHash string, fenceBashFn func(command string) bool) (bool, error) {
	rows, err := s.db.Query(
		`SELECT tool_name, label, args_hash, status FROM pre_compact_trail
		 WHERE seq > ?
		 ORDER BY seq`,
		afterSeq,
	)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, label, argsHash, status string
		if err := rows.Scan(&name, &label, &argsHash, &status); err != nil {
			return false, err
		}
		if argsHash == sameArgsHash && status == StatusRunning {
			continue
		}
		if isFenceTool(name) {
			return true, nil
		}
		if name == "Bash" {
			if fenceBashFn == nil || fenceBashFn(label) {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}

// liveTrailFenced returns true if any event currently in live_trail
// is a fence. This is the post-compaction half of the fence rule —
// users routinely edit files between compactions, and a stale snapshot
// answer must not survive that.
func (s *Store) liveTrailFenced(fenceBashFn func(command string) bool) (bool, error) {
	rows, err := s.db.Query(`SELECT tool_name, label FROM live_trail ORDER BY seq`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return scanForFence(rows, fenceBashFn)
}

func (s *Store) liveTrailFencedAfter(seq int64, fenceBashFn func(command string) bool) (bool, error) {
	rows, err := s.db.Query(
		`SELECT tool_name, label FROM live_trail
		 WHERE seq > ?
		 ORDER BY seq`,
		seq,
	)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return scanForFence(rows, fenceBashFn)
}

func (s *Store) liveTrailFencedBefore(seq int64, fenceBashFn func(command string) bool) (bool, error) {
	rows, err := s.db.Query(
		`SELECT tool_name, label FROM live_trail
		 WHERE seq < ?
		 ORDER BY seq`,
		seq,
	)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return scanForFence(rows, fenceBashFn)
}

func scanForFence(rows *sql.Rows, fenceBashFn func(command string) bool) (bool, error) {
	for rows.Next() {
		var name, label string
		if err := rows.Scan(&name, &label); err != nil {
			return false, err
		}
		if isFenceTool(name) {
			return true, nil
		}
		if name == "Bash" {
			if fenceBashFn == nil || fenceBashFn(label) {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}

// eventsAfterInSnapshot returns every snapshot row with seq > afterSeq,
// used to build the causal chain shown to the model in the in-flight
// injection.
func (s *Store) eventsAfterInSnapshot(afterSeq int64) ([]TrailEntry, error) {
	rows, err := s.db.Query(
		`SELECT seq, tool_name, args_hash, args_json, label, exit_code, COALESCE(output,''), status, recorded_at
		 FROM pre_compact_trail
		 WHERE seq > ?
		 ORDER BY seq`,
		afterSeq,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrailEntry
	for rows.Next() {
		var e TrailEntry
		var ts string
		if err := rows.Scan(&e.Seq, &e.ToolName, &e.ArgsHash, &e.ArgsJSON, &e.Label, &e.ExitCode, &e.Output, &e.Status, &ts); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			e.RecordedAt = t
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) eventsAfterInSnapshotSkippingDuplicateRunning(afterSeq int64, argsHash string) ([]TrailEntry, error) {
	after, err := s.eventsAfterInSnapshot(afterSeq)
	if err != nil {
		return nil, err
	}
	out := after[:0]
	for _, e := range after {
		if e.ArgsHash == argsHash && e.Status == StatusRunning {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func (s *Store) eventsBeforeInLiveTrail(beforeSeq int64) ([]TrailEntry, error) {
	rows, err := s.db.Query(
		`SELECT seq, tool_name, args_hash, args_json, label, exit_code, COALESCE(output,''), status, recorded_at
		 FROM live_trail
		 WHERE seq < ?
		 ORDER BY seq`,
		beforeSeq,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrailEntry
	for rows.Next() {
		var e TrailEntry
		var ts string
		if err := rows.Scan(&e.Seq, &e.ToolName, &e.ArgsHash, &e.ArgsJSON, &e.Label, &e.ExitCode, &e.Output, &e.Status, &ts); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			e.RecordedAt = t
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) eventsAfterInLiveTrail(afterSeq int64) ([]TrailEntry, error) {
	rows, err := s.db.Query(
		`SELECT seq, tool_name, args_hash, args_json, label, exit_code, COALESCE(output,''), status, recorded_at
		 FROM live_trail
		 WHERE seq > ?
		 ORDER BY seq`,
		afterSeq,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrailEntry
	for rows.Next() {
		var e TrailEntry
		var ts string
		if err := rows.Scan(&e.Seq, &e.ToolName, &e.ArgsHash, &e.ArgsJSON, &e.Label, &e.ExitCode, &e.Output, &e.Status, &ts); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			e.RecordedAt = t
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// isFenceTool returns true for tools whose presence after P
// unconditionally invalidates the cached result. Bash is intentionally
// not in this list — it needs command-level classification, see the
// fenceBashFn callback used by LookupCachedHit.
func isFenceTool(toolName string) bool {
	switch toolName {
	case "Edit", "Write", "NotebookEdit":
		return true
	}
	return false
}

// IsCacheable reports whether a tool's prior result can be safely
// served as a cache hit. Allowlist rather than denylist so unknown
// tools default to "don't cache".
func IsCacheable(toolName string) bool {
	switch toolName {
	case "Bash", "Read", "Grep", "Glob":
		return true
	}
	return false
}

// ----------------------------------------------------------------------
// cooldown: prevent intercept loops within the post-compact window.
// ----------------------------------------------------------------------

func (s *Store) IsInCooldown(argsHash string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM cooldown WHERE args_hash = ?`, argsHash).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Store) AddCooldown(argsHash string) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO cooldown (args_hash, added_at) VALUES (?, ?)`,
		argsHash, time.Now().UTC().Format(time.RFC3339Nano),
	)
	return err
}

// ----------------------------------------------------------------------
// Eviction counter: count consecutive PreToolUse events that were NOT
// intercepted post-compact. When this exceeds a threshold, callers
// drop the snapshot via DropSnapshot — meaning splice has decided the
// model has moved on and the snapshot is no longer worth keeping.
// ----------------------------------------------------------------------

const evictionKey = "eviction_counter"

// BumpEviction increments the eviction counter and returns the new
// value. Called on each PreToolUse that did NOT result in a cache hit.
func (s *Store) BumpEviction() (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	v, err := readMetaInt(tx, evictionKey, 0)
	if err != nil {
		return 0, err
	}
	v++
	if err := writeMetaInt(tx, evictionKey, v); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return v, nil
}

// ResetEviction sets the counter back to 0. Called on cache hit.
func (s *Store) ResetEviction() error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`,
		evictionKey, "0",
	)
	return err
}

// DropSnapshot empties pre_compact_trail and resets the counter. Used
// when N-no-hit triggers, indicating the model has moved on. live_trail
// is left intact (post-compact events keep accumulating).
func (s *Store) DropSnapshot() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM pre_compact_trail`); err != nil {
		return err
	}
	if err := writeMetaInt(tx, evictionKey, 0); err != nil {
		return err
	}
	return tx.Commit()
}

// ----------------------------------------------------------------------
// Watcher offset (Codex rollout tail).
// ----------------------------------------------------------------------

const lastOffsetKey = "rollout_last_offset"

// LastRolloutOffset returns the last offset the watcher persisted, or
// 0 if none. Used to resume tailing across watcher restarts without
// double-applying events.
func (s *Store) LastRolloutOffset() (int64, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, lastOffsetKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, perr := strconv.ParseInt(v, 10, 64)
	if perr != nil {
		return 0, nil
	}
	return n, nil
}

// SetLastRolloutOffset persists the watcher's current rollout file
// offset so a restart can resume without reprocessing.
func (s *Store) SetLastRolloutOffset(off int64) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`,
		lastOffsetKey, strconv.FormatInt(off, 10),
	)
	return err
}

// ----------------------------------------------------------------------
// meta helpers
// ----------------------------------------------------------------------

func readMetaInt(tx *sql.Tx, key string, def int) (int, error) {
	var v string
	err := tx.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	if err != nil {
		return 0, err
	}
	n, perr := strconv.Atoi(v)
	if perr != nil {
		return def, nil
	}
	return n, nil
}

func writeMetaInt(tx *sql.Tx, key string, v int) error {
	_, err := tx.Exec(
		`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`,
		key, strconv.Itoa(v),
	)
	return err
}
