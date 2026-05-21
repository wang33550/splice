package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sample = `{"timestamp":"2026-05-19T10:00:00.000Z","type":"session_meta","payload":{"session_id":"sess-1","cwd":"/proj/foo"}}
{"timestamp":"2026-05-19T10:00:01.000Z","type":"response_item","payload":{"type":"function_call","call_id":"c1","name":"shell","arguments":"{\"command\":\"npm test\"}"}}
{"timestamp":"2026-05-19T10:00:05.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"12 passed","exit_code":0}}
{"timestamp":"2026-05-19T10:00:10.000Z","type":"response_item","payload":{"type":"function_call","call_id":"c2","name":"shell","arguments":"{\"command\":\"git status\"}"}}
{"timestamp":"2026-05-19T10:00:11.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c2","output":"clean","exit_code":0}}
{"timestamp":"2026-05-19T10:01:00.000Z","type":"context_compaction"}
{"timestamp":"2026-05-19T10:01:01.000Z","type":"response_item","payload":{"type":"function_call","call_id":"c3","name":"shell","arguments":"{\"command\":\"npm test\"}"}}
{"timestamp":"2026-05-19T10:01:02.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c3","output":"12 passed","exit_code":0}}
`

func writeSampleRollout(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "rollout-2026-05-19-sess-1.jsonl")
	if err := os.WriteFile(p, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadAllParsesAllExpectedKinds(t *testing.T) {
	p := writeSampleRollout(t)
	events, err := ReadAll(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 8 {
		t.Fatalf("got %d events, want 8", len(events))
	}
	if events[0].Kind != KindSessionMeta || events[0].SessionMeta.Cwd != "/proj/foo" {
		t.Errorf("session meta: %+v", events[0])
	}
	if events[1].Kind != KindToolCall || events[1].ToolCall.ToolName != "shell" {
		t.Errorf("tool call: %+v", events[1])
	}
	if !strings.Contains(events[1].ToolCall.ArgsJSON, "npm test") {
		t.Errorf("tool call args: %q", events[1].ToolCall.ArgsJSON)
	}
	if events[2].Kind != KindToolResult || events[2].ToolResult.Output != "12 passed" {
		t.Errorf("tool result: %+v", events[2])
	}
	if events[2].ToolResult.ExitCode == nil || *events[2].ToolResult.ExitCode != 0 {
		t.Errorf("exit code: %+v", events[2].ToolResult.ExitCode)
	}
	if events[5].Kind != KindCompaction {
		t.Errorf("compaction event: %+v", events[5])
	}
}

func TestFindLastCompactionOffset(t *testing.T) {
	p := writeSampleRollout(t)
	off, err := FindLastCompactionOffset(p)
	if err != nil {
		t.Fatal(err)
	}
	if off == 0 {
		t.Fatal("expected non-zero offset")
	}
	tail, err := ReadFromOffset(p, off)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 2 {
		t.Fatalf("expected 2 events after compaction, got %d", len(tail))
	}
	if tail[0].Kind != KindToolCall {
		t.Errorf("first post-compact event: %+v", tail[0])
	}
}

func TestFindRolloutFileMatchesByTrailingID(t *testing.T) {
	root := t.TempDir()
	dateDir := filepath.Join(root, "sessions", "2026", "05", "19")
	if err := os.MkdirAll(dateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dateDir, "rollout-2026-05-19-abc123.jsonl")
	if err := os.WriteFile(target, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", root)

	got, err := FindRolloutFile("abc123")
	if err != nil {
		t.Fatal(err)
	}
	if got != target {
		t.Errorf("got %q, want %q", got, target)
	}
}

func TestFindRolloutFileMissing(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	if _, err := FindRolloutFile("nope"); !os.IsNotExist(err) {
		t.Errorf("expected ErrNotExist, got %v", err)
	}
}

func TestParseHandlesUnknownEventGracefully(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.jsonl")
	content := `{"timestamp":"2026-05-19T10:00:00.000Z","type":"some_future_thing","payload":{}}
{"timestamp":"2026-05-19T10:00:01.000Z","type":"response_item","payload":{"type":"function_call","call_id":"c","name":"shell","arguments":"x"}}
not-json-at-all
{"timestamp":"2026-05-19T10:00:02.000Z","type":"context_compaction"}
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	events, err := ReadAll(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The 3 valid lines should be parsed; the bad one is dropped silently.
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3 (one bad line dropped): %+v", len(events), events)
	}
	if events[0].Kind != KindUnknown {
		t.Errorf("unknown event misclassified: %+v", events[0])
	}
	if events[1].Kind != KindToolCall {
		t.Errorf("tool call after unknown: %+v", events[1])
	}
	if events[2].Kind != KindCompaction {
		t.Errorf("compaction after garbage line: %+v", events[2])
	}
}

func TestParseHandlesSchemaDriftAliases(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "aliases.jsonl")
	content := `{"ts":"2026-05-19T10:00:00.000Z","kind":"session_metadata","payload":{"id":"sess-alias","cwd":"/proj"}}
{"ts":"2026-05-19T10:00:01.000Z","kind":"turn_item","payload":{"type":"tool_call","id":"call-1","tool_name":"shell","args":{"command":"go test"}}}
{"ts":"2026-05-19T10:00:02.000Z","kind":"turn_item","payload":{"type":"tool_result","tool_use_id":"call-1","content":"ok","exitCode":0}}
{"ts":"2026-05-19T10:00:03.000Z","kind":"compact"}
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	events, err := ReadAll(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4", len(events))
	}
	if events[0].Kind != KindSessionMeta || events[0].SessionMeta.SessionID != "sess-alias" || events[0].SessionMeta.Cwd != "/proj" {
		t.Fatalf("session alias parse failed: %+v", events[0])
	}
	if events[1].Kind != KindToolCall || events[1].ToolCall.CallID != "call-1" || events[1].ToolCall.ToolName != "shell" {
		t.Fatalf("tool call alias parse failed: %+v", events[1])
	}
	if !strings.Contains(events[1].ToolCall.ArgsJSON, "go test") {
		t.Fatalf("tool call args alias parse failed: %q", events[1].ToolCall.ArgsJSON)
	}
	if events[2].Kind != KindToolResult || events[2].ToolResult.CallID != "call-1" || events[2].ToolResult.Output != "ok" {
		t.Fatalf("tool result alias parse failed: %+v", events[2])
	}
	if events[2].ToolResult.ExitCode == nil || *events[2].ToolResult.ExitCode != 0 {
		t.Fatalf("exitCode alias parse failed: %+v", events[2].ToolResult.ExitCode)
	}
	if events[3].Kind != KindCompaction {
		t.Fatalf("compact alias parse failed: %+v", events[3])
	}
}

func TestParseToolResultStatusVariants(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "status.jsonl")
	content := `{"timestamp":"2026-05-19T10:00:00.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"i","interrupted":true}}
{"timestamp":"2026-05-19T10:00:01.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"t","timedOut":true}}
{"timestamp":"2026-05-19T10:00:02.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"e","is_error":true}}
{"timestamp":"2026-05-19T10:00:03.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"x","exit_code":2}}
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	events, err := ReadAll(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"interrupted", "timeout", "error", "error"}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d", len(events), len(want))
	}
	for i, ev := range events {
		if ev.Kind != KindToolResult {
			t.Fatalf("event %d kind = %v", i, ev.Kind)
		}
		if ev.ToolResult.Status != want[i] {
			t.Fatalf("event %d status = %q, want %q", i, ev.ToolResult.Status, want[i])
		}
	}
}

func TestSessionsRootUsesCodexHomeAndDefaultHome(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	got, err := SessionsRoot()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(codexHome, "sessions") {
		t.Fatalf("CODEX_HOME sessions root = %q", got)
	}

	t.Setenv("CODEX_HOME", "")
	got, err = SessionsRoot()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "sessions" || filepath.Base(filepath.Dir(got)) != ".codex" {
		t.Fatalf("default sessions root should end in .codex/sessions, got %q", got)
	}
}

func TestReadAllMaxBytesSkipsPartialLeadingLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "long.jsonl")
	lines := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		lines = append(lines, fmt.Sprintf(
			`{"timestamp":"2026-05-19T10:00:0%d.000Z","type":"response_item","payload":{"type":"function_call","call_id":"c%d","name":"shell","arguments":"{\"command\":\"cmd-%d\"}"}}`,
			i, i, i,
		))
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	events, err := ReadAll(p, int64(len(lines[len(lines)-2])+len(lines[len(lines)-1])+2))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("expected only the final complete line after truncation, got %d", len(events))
	}
	if events[0].ToolCall == nil || !strings.Contains(events[0].ToolCall.ArgsJSON, "cmd-5") {
		t.Fatalf("unexpected truncated events: %+v", events)
	}

	all, err := ReadAll(p, int64(len(content)+100))
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 6 {
		t.Fatalf("large maxBytes should read all events, got %d", len(all))
	}
}

func TestFindLastCompactionOffsetMissingAndInvalidLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "no-compact.jsonl")
	content := "not json\n" +
		`{"timestamp":"2026-05-19T10:00:01.000Z","type":"response_item","payload":{"type":"text"}}` + "\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	off, err := FindLastCompactionOffset(p)
	if err != nil {
		t.Fatal(err)
	}
	if off != 0 {
		t.Fatalf("offset = %d, want 0 without compaction", off)
	}
}

func TestReadFromOffsetRejectsBadOffset(t *testing.T) {
	p := writeSampleRollout(t)
	if _, err := ReadFromOffset(p, -1); err == nil {
		t.Fatal("expected negative offset seek error")
	}
}

func TestParseHelpersCoverNilAndMixedTypes(t *testing.T) {
	ts := time.Date(2026, 5, 19, 10, 0, 0, 0, time.UTC)
	call := parseToolCall(nil, ts)
	if call == nil || !call.Timestamp.Equal(ts) {
		t.Fatalf("nil call parse = %+v", call)
	}
	result := parseToolResult(nil, ts)
	if result == nil || result.Status != "ok" || !result.Timestamp.Equal(ts) {
		t.Fatalf("nil result parse = %+v", result)
	}
	if got := classifyStatus(nil, nil); got != "ok" {
		t.Fatalf("nil classifyStatus = %q", got)
	}
}

func TestJSONHelpersMixedTypes(t *testing.T) {
	m := map[string]json.RawMessage{
		"object": []byte(`{"a":1}`),
		"array":  []byte(`[1,2]`),
		"float":  []byte(`2.9`),
		"false":  []byte(`false`),
		"text":   []byte(`"hello"`),
	}
	if got := jsonString(m, "missing", "object"); got != `{"a":1}` {
		t.Fatalf("object string = %q", got)
	}
	if got := jsonString(m, "array"); got != `[1,2]` {
		t.Fatalf("array string = %q", got)
	}
	if n := jsonInt(m, "float"); n == nil || *n != 2 {
		t.Fatalf("float int = %v", n)
	}
	if jsonBool(m, "false") {
		t.Fatal("false bool should remain false")
	}
	if got := firstNonEmpty("", "", "x"); got != "x" {
		t.Fatalf("firstNonEmpty = %q", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Fatalf("empty firstNonEmpty = %q", got)
	}
}

func TestParseLineEmptyAndResponseItemFallback(t *testing.T) {
	if _, err := parseLine([]byte(" \n\t")); err == nil {
		t.Fatal("empty line should be rejected")
	}
	ev, err := parseLine([]byte(`{"type":"response_item","payload":{"type":"message","text":"hi"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != KindResponseItem {
		t.Fatalf("response item fallback kind = %v", ev.Kind)
	}
}

func TestFmtErrWrapsPathAndCause(t *testing.T) {
	cause := errors.New("boom")
	err := FmtErr("rollout.jsonl", cause)
	if !errors.Is(err, cause) {
		t.Fatalf("FmtErr should wrap cause, got %v", err)
	}
	if !strings.Contains(err.Error(), "codex rollout.jsonl") {
		t.Fatalf("FmtErr text = %q", err.Error())
	}
}
