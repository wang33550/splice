package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "splice-store-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.Setenv("SPLICE_HOME", root)
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := OpenSession("test-session-" + t.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func nullInt(n int) sql.NullInt64 {
	return sql.NullInt64{Int64: int64(n), Valid: true}
}

// TestSnapshotReplacedOnEachFreeze verifies the v0.5 rule that
// pre_compact_trail keeps at most one frozen snapshot at a time.
func TestSnapshotReplacedOnEachFreeze(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()

	// Round 1.
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h1",
		ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`, Label: "npm test",
		Status: StatusOK, ExitCode: nullInt(0), Output: "12 passed",
		RecordedAt: now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}

	// Round 2 — newer output replaces older snapshot.
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h1",
		ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`, Label: "npm test",
		Status: StatusOK, ExitCode: nullInt(0), Output: "13 passed",
		RecordedAt: now.Add(-1 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}

	hit, err := st.LookupCachedHit("h1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil {
		t.Fatal("expected hit, got nil")
	}
	if hit.Entry.Output != "13 passed" {
		t.Errorf("expected newer snapshot output, got %q", hit.Entry.Output)
	}

	// Verify the snapshot really only has the new content.
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM pre_compact_trail`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected exactly 1 frozen row after second freeze, got %d", n)
	}
}

// TestLiveTrailFenceInvalidatesHit verifies the v0.5 fence rule: a fence
// event in live_trail (post-compact accumulation) must invalidate a
// matching snapshot hit. This is the critical "users edit files between
// compactions" coverage.
func TestLiveTrailFenceInvalidatesHit(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()

	// Snapshot has npm test → ok.
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h",
		ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`, Label: "npm test",
		Status: StatusOK, ExitCode: nullInt(0), Output: "ok",
		RecordedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}

	// No fence yet → hit.
	hit, err := st.LookupCachedHit("h", nil)
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil {
		t.Fatal("expected hit before any post-compact fence")
	}

	// Now an Edit happens post-compact (in live_trail).
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Edit", ArgsHash: "h-edit",
		ArgsJSON: `{"args":{"file_path":"/x.go"},"tool":"Edit"}`, Label: "/x.go",
		Status: StatusOK, RecordedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Same lookup must now miss because live_trail has a fence event.
	hit, err = st.LookupCachedHit("h", nil)
	if err != nil {
		t.Fatal(err)
	}
	if hit != nil {
		t.Fatalf("expected miss after live_trail Edit, got %+v", hit)
	}
}

func TestLookupCachedHitHonorsBashCacheableCallback(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()

	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h",
		ArgsJSON: `{"args":{"command":"tail -n 20 sim.log"},"tool":"Bash"}`,
		Label:    "tail -n 20 sim.log",
		Status:   StatusOK, ExitCode: nullInt(0), Output: "old status",
		RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}

	hit, err := st.LookupCachedHit("h", nil, func(command string) bool {
		return command != "tail -n 20 sim.log"
	})
	if err != nil {
		t.Fatal(err)
	}
	if hit != nil {
		t.Fatalf("cacheable callback should suppress volatile Bash hit, got %+v", hit)
	}

	hit, err = st.LookupCachedHit("h", nil, func(command string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil {
		t.Fatal("expected hit when cacheable callback allows it")
	}
}

func TestTerminalHitIncludesAfterEventsUntilCompaction(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()

	entries := []TrailEntry{
		{
			ToolName: "Bash", ArgsHash: "h-npm",
			ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
			Label:    "npm test", Status: StatusOK, ExitCode: nullInt(0), Output: "12 passed",
			RecordedAt: now,
		},
		{
			ToolName: "Bash", ArgsHash: "h-status",
			ArgsJSON: `{"args":{"command":"git status --porcelain"},"tool":"Bash"}`,
			Label:    "git status --porcelain", Status: StatusOK, ExitCode: nullInt(0), Output: "clean",
			RecordedAt: now.Add(time.Second),
		},
		{
			ToolName: "Bash", ArgsHash: "h-tail",
			ArgsJSON: `{"args":{"command":"tail -n 20 sim.log"},"tool":"Bash"}`,
			Label:    "tail -n 20 sim.log", Status: StatusOK, ExitCode: nullInt(0), Output: "progress=80%",
			RecordedAt: now.Add(2 * time.Second),
		},
	}
	for _, e := range entries {
		if err := st.AppendLiveTrail(e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}

	hit, err := st.LookupCachedHit("h-npm", func(command string) bool {
		return command == ""
	}, func(command string) bool {
		return command == "npm test"
	})
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || hit.InFlight {
		t.Fatalf("expected terminal hit, got %+v", hit)
	}
	if len(hit.AfterEvents) != 2 {
		t.Fatalf("expected two after-events, got %+v", hit.AfterEvents)
	}
	if hit.AfterEvents[0].Label != "git status --porcelain" || hit.AfterEvents[1].Label != "tail -n 20 sim.log" {
		t.Fatalf("wrong after-events: %+v", hit.AfterEvents)
	}
}

func TestHasPreCompactTrailTransitions(t *testing.T) {
	st := newTestStore(t)
	has, err := st.HasPreCompactTrail()
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("fresh store should not have pre-compact trail")
	}
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h",
		ArgsJSON: `{}`, Label: "npm test",
		Status: StatusOK, RecordedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}
	has, err = st.HasPreCompactTrail()
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("freeze should create pre-compact trail")
	}
}

func TestInFlightHitIncludesAfterEvents(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	if err := st.BeginLiveTrailEntry(TrailEntry{
		CallID: "long-call", ToolName: "Bash",
		ArgsHash: "h-long", ArgsJSON: `{}`, Label: "npm run dev",
		RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Read", ArgsHash: "h-read",
		ArgsJSON: `{}`, Label: "/tmp/a.go",
		Status: StatusOK, Output: "content",
		RecordedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}
	hit, err := st.LookupCachedHit("h-long", nil)
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || !hit.InFlight {
		t.Fatalf("expected in-flight hit, got %+v", hit)
	}
	if len(hit.AfterEvents) != 1 || hit.AfterEvents[0].ToolName != "Read" {
		t.Fatalf("unexpected after events: %+v", hit.AfterEvents)
	}
}

func TestInFlightSnapshotUsesLiveTerminalResultWhenPostArrivesAfterCompact(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	if err := st.BeginLiveTrailEntry(TrailEntry{
		CallID: "long-call", ToolName: "Bash",
		ArgsHash: "h-long", ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label: "npm test", RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}

	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Read", ArgsHash: "h-before",
		ArgsJSON: `{"args":{"file_path":"/tmp/before.go"},"tool":"Read"}`,
		Label:    "/tmp/before.go", Status: StatusOK, Output: "notes before result",
		RecordedAt: now.Add(500 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h-long",
		ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label:    "npm test", Status: StatusOK, ExitCode: nullInt(0), Output: "12 passed",
		RecordedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Read", ArgsHash: "h-read",
		ArgsJSON: `{"args":{"file_path":"/tmp/a.go"},"tool":"Read"}`,
		Label:    "/tmp/a.go", Status: StatusOK, Output: "notes",
		RecordedAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	hit, err := st.LookupCachedHit("h-long", func(command string) bool { return false }, func(command string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || hit.InFlight {
		t.Fatalf("expected live terminal hit to supersede running snapshot, got %+v", hit)
	}
	if hit.Entry.Output != "12 passed" {
		t.Fatalf("terminal output = %q", hit.Entry.Output)
	}
	if len(hit.AfterEvents) != 2 {
		t.Fatalf("unexpected live after-events: %+v", hit.AfterEvents)
	}
	if hit.AfterEvents[0].Label != "/tmp/before.go" || hit.AfterEvents[0].Output != "notes before result" {
		t.Fatalf("pre-result live context should be preserved, got %+v", hit.AfterEvents)
	}
	if hit.AfterEvents[1].Label != "/tmp/a.go" || hit.AfterEvents[1].Output != "notes" {
		t.Fatalf("post-result live context should be preserved, got %+v", hit.AfterEvents)
	}
}

func TestLiveTerminalResultWinsOverPriorDuplicateTerminal(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h-long",
		ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label:    "npm test", Status: StatusOK, ExitCode: nullInt(0), Output: "old terminal",
		RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginLiveTrailEntry(TrailEntry{
		CallID: "codex-rollout:dup", ToolName: "Bash",
		ArgsHash: "h-long", ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label: "npm test", RecordedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h-long",
		ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label:    "npm test", Status: StatusOK, ExitCode: nullInt(0), Output: "new terminal after compact",
		RecordedAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	hit, err := st.LookupCachedHit("h-long", func(string) bool { return false }, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || hit.InFlight {
		t.Fatalf("expected live terminal hit, got %+v", hit)
	}
	if hit.Entry.Output != "new terminal after compact" {
		t.Fatalf("live terminal should win over prior duplicate terminal, got %q", hit.Entry.Output)
	}
}

func TestLiveTerminalAfterInFlightSnapshotHonorsFenceBeforeResult(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	if err := st.BeginLiveTrailEntry(TrailEntry{
		CallID: "long-call", ToolName: "Bash",
		ArgsHash: "h-long", ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label: "npm test", RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Edit", ArgsHash: "h-edit", ArgsJSON: `{}`, Label: "/tmp/a.go",
		Status: StatusOK, RecordedAt: now.Add(500 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h-long",
		ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label:    "npm test", Status: StatusOK, ExitCode: nullInt(0), Output: "12 passed after edit",
		RecordedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	hit, err := st.LookupCachedHit("h-long", func(command string) bool { return command != "npm test" }, func(command string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hit != nil {
		t.Fatalf("pre-result fence should suppress late terminal hit, got %+v", hit)
	}
}

func TestLiveTerminalAfterInFlightSnapshotHonorsSnapshotFenceAfterStart(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	if err := st.BeginLiveTrailEntry(TrailEntry{
		CallID: "long-call", ToolName: "Bash",
		ArgsHash: "h-long", ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label: "npm test", RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h-rm",
		ArgsJSON: `{"args":{"command":"rm tmp.txt"},"tool":"Bash"}`,
		Label:    "rm tmp.txt", Status: StatusOK,
		RecordedAt: now.Add(500 * time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h-long",
		ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label:    "npm test", Status: StatusOK, ExitCode: nullInt(0), Output: "12 passed after rm",
		RecordedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	hit, err := st.LookupCachedHit("h-long", func(command string) bool { return command != "npm test" }, func(command string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hit != nil {
		t.Fatalf("snapshot fence after original start should suppress late terminal hit, got %+v", hit)
	}
}

func TestAppendTerminalFromFrozenRunningRecoversAfterRestart(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	if err := st.BeginLiveTrailEntry(TrailEntry{
		CallID: "codex-rollout:c-long", ToolName: "Bash",
		ArgsHash: "h-long", ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label: "npm test", RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendTerminalFromFrozenRunning(
		"codex-rollout:c-long",
		nullInt(0),
		"12 passed after restart",
		StatusOK,
		now.Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Read", ArgsHash: "h-read",
		ArgsJSON: `{"args":{"file_path":"/tmp/after.md"},"tool":"Read"}`,
		Label:    "/tmp/after.md", Status: StatusOK, Output: "note",
		RecordedAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	hit, err := st.LookupCachedHit("h-long", func(string) bool { return false }, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || hit.InFlight {
		t.Fatalf("expected recovered terminal hit, got %+v", hit)
	}
	if hit.Entry.Output != "12 passed after restart" {
		t.Fatalf("hit output = %q", hit.Entry.Output)
	}
	if len(hit.AfterEvents) != 1 || hit.AfterEvents[0].Label != "/tmp/after.md" {
		t.Fatalf("expected live after-event, got %+v", hit.AfterEvents)
	}
}

func TestAppendTerminalFromFrozenRunningMissingCallIDReturnsSentinel(t *testing.T) {
	st := newTestStore(t)
	if err := st.AppendTerminalFromFrozenRunning("missing", nullInt(0), "x", StatusOK, time.Now()); !IsErrNoRunningRow(err) {
		t.Fatalf("expected errNoRunningRow for missing frozen call id, got %v", err)
	}
}

func TestLiveTerminalAfterInFlightSnapshotStillHonorsFence(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	if err := st.BeginLiveTrailEntry(TrailEntry{
		CallID: "long-call", ToolName: "Bash",
		ArgsHash: "h-long", ArgsJSON: `{}`, Label: "npm test",
		RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h-long", ArgsJSON: `{}`, Label: "npm test",
		Status: StatusOK, ExitCode: nullInt(0), Output: "12 passed",
		RecordedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Edit", ArgsHash: "h-edit", ArgsJSON: `{}`, Label: "/tmp/a.go",
		Status: StatusOK, RecordedAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	hit, err := st.LookupCachedHit("h-long", func(command string) bool { return command != "npm test" }, func(command string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hit != nil {
		t.Fatalf("post-terminal fence should suppress live terminal hit, got %+v", hit)
	}
}

func TestPriorTerminalBehindRunningStillHonorsRealFence(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h-test",
		ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label:    "npm test", Status: StatusOK, ExitCode: nullInt(0), Output: "12 passed",
		RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h-rm",
		ArgsJSON: `{"args":{"command":"rm tmp.txt"},"tool":"Bash"}`,
		Label:    "rm tmp.txt", Status: StatusOK, ExitCode: nullInt(0),
		RecordedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginLiveTrailEntry(TrailEntry{
		CallID: "codex-rollout:dup", ToolName: "Bash",
		ArgsHash: "h-test", ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label: "npm test", RecordedAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}

	hit, err := st.LookupCachedHit("h-test", func(command string) bool { return command != "npm test" }, func(command string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hit != nil {
		t.Fatalf("real side-effect between terminal and duplicate running must fence, got %+v", hit)
	}
}

func TestPriorTerminalBehindDuplicateRunningKeepsRealAfterEvents(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h-test",
		ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label:    "npm test", Status: StatusOK, ExitCode: nullInt(0), Output: "12 passed",
		RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginLiveTrailEntry(TrailEntry{
		CallID: "codex-rollout:dup", ToolName: "Bash",
		ArgsHash: "h-test", ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label: "npm test", RecordedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Read", ArgsHash: "h-read",
		ArgsJSON: `{"args":{"file_path":"/tmp/after.md"},"tool":"Read"}`,
		Label:    "/tmp/after.md", Status: StatusOK, Output: "real later note",
		RecordedAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}

	hit, err := st.LookupCachedHit("h-test", func(command string) bool { return command != "npm test" }, func(command string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || hit.InFlight {
		t.Fatalf("expected prior terminal hit behind duplicate running, got %+v", hit)
	}
	if hit.Entry.Output != "12 passed" {
		t.Fatalf("terminal output = %q", hit.Entry.Output)
	}
	if len(hit.AfterEvents) != 1 {
		t.Fatalf("duplicate running should be filtered while real later event remains, got %+v", hit.AfterEvents)
	}
	if hit.AfterEvents[0].ToolName != "Read" || hit.AfterEvents[0].Output != "real later note" {
		t.Fatalf("unexpected after-events: %+v", hit.AfterEvents)
	}
}

func TestRealRepeatedRunningAfterPriorTerminalRemainsInFlight(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	if err := st.AppendLiveTrail(TrailEntry{
		CallID: "claude-call-1", ToolName: "Bash", ArgsHash: "h-test",
		ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label:    "npm test", Status: StatusOK, ExitCode: nullInt(0), Output: "first terminal result",
		RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginLiveTrailEntry(TrailEntry{
		CallID: "claude-call-2", ToolName: "Bash", ArgsHash: "h-test",
		ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label:    "npm test", RecordedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}

	hit, err := st.LookupCachedHit("h-test", func(command string) bool { return command != "npm test" }, func(command string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || !hit.InFlight {
		t.Fatalf("real second running call should remain in-flight, got %+v", hit)
	}
	if hit.Entry.CallID != "claude-call-2" {
		t.Fatalf("latest running call should drive in-flight warning, got call_id %q", hit.Entry.CallID)
	}
	if hit.Entry.Output != "" {
		t.Fatalf("in-flight branch must not reuse older terminal output, got %q", hit.Entry.Output)
	}
}

func TestDuplicateSourceFallbackStopsAtInterveningRunning(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h-test",
		ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label:    "npm test", Status: StatusOK, ExitCode: nullInt(0), Output: "old terminal",
		RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginLiveTrailEntry(TrailEntry{
		CallID: "codex-rollout:first-real-repeat", ToolName: "Bash",
		ArgsHash: "h-test", ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label: "npm test", RecordedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginLiveTrailEntry(TrailEntry{
		CallID: "codex-rollout:second-real-repeat", ToolName: "Bash",
		ArgsHash: "h-test", ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label: "npm test", RecordedAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}

	hit, err := st.LookupCachedHit("h-test", func(command string) bool { return command != "npm test" }, func(command string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || !hit.InFlight {
		t.Fatalf("intervening same-hash running should prevent old terminal fallback, got %+v", hit)
	}
	if hit.Entry.CallID != "codex-rollout:second-real-repeat" {
		t.Fatalf("latest running call should remain authoritative, got %q", hit.Entry.CallID)
	}
}

func TestNonCacheableToolMisses(t *testing.T) {
	st := newTestStore(t)
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "WebFetch", ArgsHash: "h",
		ArgsJSON: `{}`, Label: "https://example.com",
		Status: StatusOK, Output: "body",
		RecordedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}
	hit, err := st.LookupCachedHit("h", nil)
	if err != nil {
		t.Fatal(err)
	}
	if hit != nil {
		t.Fatalf("non-cacheable tool should miss, got %+v", hit)
	}
}

func TestUnknownToolAfterCandidateFencesHit(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()

	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h-test",
		ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`, Label: "npm test",
		Status: StatusOK, ExitCode: nullInt(0), Output: "ok",
		RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "mcp__db__query", ArgsHash: "h-mcp",
		ArgsJSON: `{}`, Label: "mcp__db__query",
		Status: StatusOK, Output: "updated remote cache",
		RecordedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}

	hit, err := st.LookupCachedHit("h-test", func(command string) bool { return false }, func(command string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hit != nil {
		t.Fatalf("unknown tool after candidate should fence old hit, got %+v", hit)
	}
}

func TestExternalReadOnlyToolAfterCandidateDoesNotFence(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()

	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h-test",
		ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`, Label: "npm test",
		Status: StatusOK, ExitCode: nullInt(0), Output: "ok",
		RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "WebSearch", ArgsHash: "h-web",
		ArgsJSON: `{}`, Label: "latest release",
		Status: StatusOK, Output: "remote observation",
		RecordedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}

	hit, err := st.LookupCachedHit("h-test", func(command string) bool { return false }, func(command string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil {
		t.Fatal("external read-only observation should not fence stable local hit")
	}
	if len(hit.AfterEvents) != 1 || hit.AfterEvents[0].ToolName != "WebSearch" {
		t.Fatalf("expected WebSearch in restored trail, got %+v", hit.AfterEvents)
	}
}

func TestSessionMetaPath(t *testing.T) {
	want := filepath.Join(HomeDir(), "sessions", "s.meta.json")
	if got := SessionMetaPath("s"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestOpenSessionMigratesPreCompactCallIDColumn(t *testing.T) {
	session := "legacy"
	if err := os.MkdirAll(SessionsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := SessionDBPath(session)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE pre_compact_trail (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
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
CREATE TABLE live_trail (
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
CREATE TABLE cooldown (args_hash TEXT PRIMARY KEY, added_at TEXT NOT NULL);
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO pre_compact_trail
    (seq, tool_name, args_hash, args_json, label, exit_code, output, status, recorded_at)
VALUES
    (1, 'Bash', 'h', '{"args":{"command":"npm test"},"tool":"Bash"}', 'npm test', 0, 'legacy ok', 'ok', ?);
`, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := OpenSession(session)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	hasCallID, err := tableHasColumn(st.db, "pre_compact_trail", "call_id")
	if err != nil {
		t.Fatal(err)
	}
	if !hasCallID {
		t.Fatal("OpenSession should migrate legacy pre_compact_trail with call_id")
	}
	hit, err := st.LookupCachedHit("h", nil, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || hit.Entry.Output != "legacy ok" {
		t.Fatalf("legacy row should remain usable after migration, got %+v", hit)
	}
}

// TestBeginFinishLiveTrailEntry verifies the PreToolUse → PostToolUse pairing.
func TestBeginFinishLiveTrailEntry(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()

	if err := st.BeginLiveTrailEntry(TrailEntry{
		CallID: "call-1", ToolName: "Bash",
		ArgsHash: "h", ArgsJSON: `{"tool":"Bash"}`, Label: "npm test",
		RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.FinishLiveTrailEntry("call-1", nullInt(0), "12 passed", StatusOK, now.Add(time.Second)); err != nil {
		t.Fatalf("finish: %v", err)
	}

	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}
	hit, err := st.LookupCachedHit("h", nil)
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || hit.InFlight {
		t.Fatalf("expected terminal hit, got %+v", hit)
	}
	if hit.Entry.Output != "12 passed" {
		t.Errorf("output: %q", hit.Entry.Output)
	}
}

// TestFinishWithoutBeginReturnsSentinel covers the fallback signal.
func TestFinishWithoutBeginReturnsSentinel(t *testing.T) {
	st := newTestStore(t)
	err := st.FinishLiveTrailEntry("missing-call", nullInt(0), "x", StatusOK, time.Now())
	if !IsErrNoRunningRow(err) {
		t.Fatalf("want errNoRunningRow, got %v", err)
	}
}

// TestAppendLiveTrailSurvivesFreeze verifies the watcher / fallback path.
func TestAppendLiveTrailSurvivesFreeze(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h2",
		ArgsJSON: `{"tool":"Bash"}`, Label: "ls",
		Status: StatusOK, ExitCode: nullInt(0), Output: "files",
		RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}
	hit, err := st.LookupCachedHit("h2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || hit.Entry.Output != "files" {
		t.Fatalf("expected hit with appended output, got %+v", hit)
	}
}

// TestDropClosesAndDeletesFiles covers /clear: Drop should remove the .db,
// its WAL/SHM siblings, and the meta sidecar.
func TestDropClosesAndDeletesFiles(t *testing.T) {
	st, err := OpenSession("drop-me")
	if err != nil {
		t.Fatal(err)
	}
	dbPath := st.Path()
	// Force a WAL file to exist by writing something.
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h", ArgsJSON: `{}`,
		Status: StatusOK, RecordedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.Drop(); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s gone after Drop, stat err = %v", filepath.Base(p), err)
		}
	}
}

func TestClearTrailStateKeepsDBAndClearsRuntimeRows(t *testing.T) {
	st := newTestStore(t)
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h",
		ArgsJSON: `{}`, Label: "npm test",
		Status: StatusOK, Output: "old",
		RecordedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}
	if err := st.AddCooldown("h"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetLastRolloutOffset(123); err != nil {
		t.Fatal(err)
	}

	path := st.Path()
	if err := st.ClearTrailState(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("clear should keep db file available: %v", err)
	}
	hit, err := st.LookupCachedHit("h", nil, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hit != nil {
		t.Fatalf("clear should remove cached hit, got %+v", hit)
	}
	cooled, err := st.IsInCooldown("h")
	if err != nil {
		t.Fatal(err)
	}
	if cooled {
		t.Fatal("clear should remove cooldown")
	}
	off, err := st.LastRolloutOffset()
	if err != nil {
		t.Fatal(err)
	}
	if off != 0 {
		t.Fatalf("clear should remove old rollout offset before caller writes barrier offset, got %d", off)
	}
}

func TestClearLiveTrailForRolloutResetKeepsSnapshotAndClearsCursor(t *testing.T) {
	st := newTestStore(t)
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h-old",
		ArgsJSON: `{}`, Label: "npm test",
		Status: StatusOK, Output: "old snapshot",
		RecordedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h-live",
		ArgsJSON: `{}`, Label: "go test",
		Status: StatusOK, Output: "live row",
		RecordedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRolloutCursor(123, "cursor-hash"); err != nil {
		t.Fatal(err)
	}

	if err := st.ClearLiveTrailForRolloutReset(); err != nil {
		t.Fatal(err)
	}
	oldHit, err := st.LookupCachedHit("h-old", nil, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if oldHit == nil || oldHit.Entry.Output != "old snapshot" {
		t.Fatalf("rollout reset should keep existing snapshot, got %+v", oldHit)
	}
	liveHit, err := st.LookupCachedHit("h-live", nil, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if liveHit != nil {
		t.Fatalf("rollout reset should clear live rows, got %+v", liveHit)
	}
	off, err := st.LastRolloutOffset()
	if err != nil {
		t.Fatal(err)
	}
	if off != 0 {
		t.Fatalf("rollout reset should clear offset, got %d", off)
	}
	cursorHash, err := st.LastRolloutCursorHash()
	if err != nil {
		t.Fatal(err)
	}
	if cursorHash != "" {
		t.Fatalf("rollout reset should clear cursor hash, got %q", cursorHash)
	}
}

// TestCooldownIsScopedToSession verifies that two different sessions in
// the same cwd don't share cooldown state (per-session DB → per-session
// cooldown by construction).
func TestCooldownIsScopedToSession(t *testing.T) {
	a, err := OpenSession("A")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenSession("B")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if err := a.AddCooldown("h"); err != nil {
		t.Fatal(err)
	}
	cooled, err := b.IsInCooldown("h")
	if err != nil {
		t.Fatal(err)
	}
	if cooled {
		t.Fatal("cooldown bled across per-session DBs")
	}
}

// TestEvictionCounterTrips verifies BumpEviction returns increasing values
// and DropSnapshot empties pre_compact_trail and resets the counter.
func TestEvictionCounterTrips(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h", ArgsJSON: `{}`,
		Status: StatusOK, RecordedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}
	for want := 1; want <= 3; want++ {
		got, err := st.BumpEviction()
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("bump %d: want %d, got %d", want, want, got)
		}
	}
	// ResetEviction zeroes it.
	if err := st.ResetEviction(); err != nil {
		t.Fatal(err)
	}
	got, err := st.BumpEviction()
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("after reset, want 1, got %d", got)
	}
	// DropSnapshot empties the snapshot and resets counter.
	if err := st.DropSnapshot(); err != nil {
		t.Fatal(err)
	}
	hit, err := st.LookupCachedHit("h", nil)
	if err != nil {
		t.Fatal(err)
	}
	if hit != nil {
		t.Fatalf("expected miss after DropSnapshot, got %+v", hit)
	}
}

// TestFreezeResetsEvictionCounter verifies a fresh post-compact window
// always starts the no-hit counter at zero.
func TestFreezeResetsEvictionCounter(t *testing.T) {
	st := newTestStore(t)
	if err := st.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h", ArgsJSON: `{}`,
		Status: StatusOK, RecordedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := st.BumpEviction(); err != nil {
			t.Fatal(err)
		}
	}
	// Another freeze should reset.
	if _, err := st.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}
	got, err := st.BumpEviction()
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("freeze should reset counter, got first bump = %d", got)
	}
}

// TestRolloutOffsetPersists verifies the watcher's offset round-trips.
func TestRolloutOffsetPersists(t *testing.T) {
	st, err := OpenSession("S")
	if err != nil {
		t.Fatal(err)
	}
	off, err := st.LastRolloutOffset()
	if err != nil {
		t.Fatal(err)
	}
	if off != 0 {
		t.Fatalf("fresh session should have offset 0, got %d", off)
	}
	if err := st.SetLastRolloutOffset(12345); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := OpenSession("S")
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	off, err = st2.LastRolloutOffset()
	if err != nil {
		t.Fatal(err)
	}
	if off != 12345 {
		t.Errorf("offset did not persist; got %d", off)
	}
}

func TestRolloutCursorPersists(t *testing.T) {
	st, err := OpenSession("cursor-session")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetRolloutCursor(42, "abc123"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := OpenSession("cursor-session")
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	off, err := st2.LastRolloutOffset()
	if err != nil {
		t.Fatal(err)
	}
	if off != 42 {
		t.Fatalf("offset = %d, want 42", off)
	}
	cursorHash, err := st2.LastRolloutCursorHash()
	if err != nil {
		t.Fatal(err)
	}
	if cursorHash != "abc123" {
		t.Fatalf("cursor hash = %q, want abc123", cursorHash)
	}
}

// TestSessionsAreIsolatedByFile verifies that two sessions genuinely live in
// different DB files.
func TestSessionsAreIsolatedByFile(t *testing.T) {
	a, err := OpenSession("A")
	if err != nil {
		t.Fatal(err)
	}
	if a.Path() != SessionDBPath("A") {
		t.Errorf("path mismatch: %s", a.Path())
	}
	defer a.Close()

	b, err := OpenSession("B")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if a.Path() == b.Path() {
		t.Fatal("two sessions resolved to the same DB file")
	}
	if _, err := os.Stat(a.Path()); err != nil {
		t.Errorf("A's DB file should exist on disk: %v", err)
	}
	if _, err := os.Stat(b.Path()); err != nil {
		t.Errorf("B's DB file should exist on disk: %v", err)
	}
}

func TestRuntimePathHelpersUseSpliceHome(t *testing.T) {
	home := HomeDir()
	if home == "" {
		t.Fatal("HomeDir should not be empty")
	}
	if got := SessionsDir(); got != filepath.Join(home, "sessions") {
		t.Fatalf("SessionsDir = %q, want under home", got)
	}
	if got := ActiveSessionsDir(); got != filepath.Join(home, "active-sessions") {
		t.Fatalf("ActiveSessionsDir = %q, want under home", got)
	}
	if got := WatcherLockPath(); got != filepath.Join(home, "codex-watch.lock") {
		t.Fatalf("WatcherLockPath = %q, want under home", got)
	}
	if got := SessionDBPath("path/test:id"); !strings.HasPrefix(got, SessionsDir()) || !strings.HasSuffix(got, ".db") {
		t.Fatalf("SessionDBPath should stay under sessions with db suffix, got %q", got)
	}
	if got := SessionMetaPath("path/test:id"); !strings.HasPrefix(got, SessionsDir()) || !strings.HasSuffix(got, ".meta.json") {
		t.Fatalf("SessionMetaPath should stay under sessions with meta suffix, got %q", got)
	}
}

func TestWriteSessionMetaAndProjectKey(t *testing.T) {
	cwd := filepath.Join("C:", "Users", "HP", "Project")
	sessionID := "meta/session:id"
	if err := WriteSessionMeta(cwd, sessionID); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(SessionMetaPath(sessionID))
	if err != nil {
		t.Fatal(err)
	}
	var meta SessionMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.SessionID != sessionID || meta.Cwd != cwd {
		t.Fatalf("bad meta: %+v", meta)
	}
	if meta.ProjectKey == "" || meta.ProjectKey == "_projectless" {
		t.Fatalf("project cwd should produce stable project key, got %+v", meta)
	}
	if ProjectKey("") != "_projectless" {
		t.Fatalf("blank cwd should be projectless, got %q", ProjectKey(""))
	}
	base := SessionFileBase(sessionID)
	if strings.ContainsAny(base, `\/:`) {
		t.Fatalf("session file base should be path-safe, got %q", base)
	}
}

func TestSameProjectDifferentConversationsAreIsolated(t *testing.T) {
	a, err := OpenSession("planning-a")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := OpenSession("planning-b")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if err := a.AppendLiveTrail(TrailEntry{
		ToolName: "Bash", ArgsHash: "h-shared",
		ArgsJSON: `{"args":{"command":"npm test"},"tool":"Bash"}`,
		Label:    "npm test", Status: StatusOK, Output: "A result",
		RecordedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.FreezePreCompact(); err != nil {
		t.Fatal(err)
	}

	hitA, err := a.LookupCachedHit("h-shared", nil, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hitA == nil || hitA.Entry.Output != "A result" {
		t.Fatalf("session A should see its own result, got %+v", hitA)
	}
	hitB, err := b.LookupCachedHit("h-shared", nil, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if hitB != nil {
		t.Fatalf("session B must not see session A trail, got %+v", hitB)
	}
}
