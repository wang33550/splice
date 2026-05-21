// Package codex parses Codex CLI's rollout JSONL files. Each session is
// stored as a stream of newline-delimited JSON events under
// ~/.codex/sessions/<date>/<session-id>.jsonl. The fields we care about:
//
//   - the session-meta header (first line) which carries cwd
//   - per-turn tool_call / tool_result events
//   - context-compaction events (which let splice reconstruct the
//     PreCompact boundary that Codex doesn't expose as a hook)
//
// The format is private to Codex and may change. We only key off field
// names that have been stable across recent versions and degrade gracefully
// (skip unknown event types) when something doesn't fit.
package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Event is a parsed rollout entry. Type-specific fields are populated
// based on Kind. Anything we couldn't classify falls into KindUnknown
// with the raw JSON kept in Raw for debugging.
type Event struct {
	Kind        Kind
	Timestamp   time.Time
	Raw         []byte // verbatim JSON line, preserved for telemetry/debugging
	SessionMeta *SessionMeta
	ToolCall    *ToolCall
	ToolResult  *ToolResult
	Compaction  *Compaction
}

type Kind int

const (
	KindUnknown Kind = iota
	KindSessionMeta
	KindToolCall
	KindToolResult
	KindCompaction
	KindResponseItem // assistant text / reasoning — not a tool invocation
)

// SessionMeta is what we read from the first line of a rollout JSONL.
// At minimum we want cwd so the watcher can decide which sessions belong
// to the user's current project directory.
type SessionMeta struct {
	SessionID string
	Cwd       string
}

type ToolCall struct {
	CallID    string
	ToolName  string
	ArgsJSON  string // canonical-ish: just the args object verbatim
	Timestamp time.Time
}

type ToolResult struct {
	CallID    string
	Output    string
	ExitCode  *int
	Status    string // ok | error | interrupted | timeout | unknown
	Timestamp time.Time
}

// Compaction is what splice keys off to know "the PreCompact moment happened
// here". Codex emits context-compaction events at the rollout level when it
// summarizes prior turns to fit the context window.
type Compaction struct {
	Timestamp time.Time
}

// SessionsRoot returns the conventional rollout root: $CODEX_HOME or ~/.codex.
func SessionsRoot() (string, error) {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return filepath.Join(v, "sessions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "sessions"), nil
}

// FindRolloutFile locates the JSONL for a given session id. Codex partitions
// sessions by date (yyyy/mm/dd), so we walk the sessions root looking for
// "<id>.jsonl". Returns os.ErrNotExist if not found.
func FindRolloutFile(sessionID string) (string, error) {
	root, err := SessionsRoot()
	if err != nil {
		return "", err
	}
	var match string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// skip unreadable subtrees rather than aborting the whole walk
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		// Codex names files like "rollout-<ts>-<id>.jsonl" or "<id>.jsonl".
		// Match either by exact id at the end of the basename or by the id
		// being a substring (covers any future variant).
		base := strings.TrimSuffix(d.Name(), ".jsonl")
		if base == sessionID || strings.HasSuffix(base, "-"+sessionID) || strings.Contains(base, sessionID) {
			match = path
			return io.EOF // sentinel to stop walk early
		}
		return nil
	})
	if errors.Is(err, io.EOF) {
		return match, nil
	}
	if err != nil {
		return "", err
	}
	if match == "" {
		return "", os.ErrNotExist
	}
	return match, nil
}

// ReadAll reads and parses an entire rollout file. Used for replay on
// watcher startup. Caller passes maxBytes to cap memory; 0 means unlimited.
func ReadAll(path string, maxBytes int64) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if maxBytes > 0 {
		st, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if st.Size() > maxBytes {
			// seek to (size - maxBytes) and discard the partial leading line
			if _, err := f.Seek(-maxBytes, io.SeekEnd); err != nil {
				return nil, err
			}
			r := bufio.NewReader(f)
			if _, err := r.ReadString('\n'); err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			return parseStream(r)
		}
	}
	return parseStream(bufio.NewReader(f))
}

// FindLastCompactionOffset walks the file backwards in chunks looking for
// the last compaction event. Returns the byte offset of the line *after*
// that compaction (i.e. where to start replaying live trail content), or 0
// if no compaction was found. We scan the file forward (it's typically
// small enough — <10MB even for long sessions) and just remember the
// last position; the chunked reverse scan is a future optimization.
func FindLastCompactionOffset(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	r := bufio.NewReader(f)
	var pos, lastAfter int64
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			ev, perr := parseLine(line)
			if perr == nil && ev.Kind == KindCompaction {
				lastAfter = pos + int64(len(line))
			}
			pos += int64(len(line))
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return 0, err
		}
	}
	return lastAfter, nil
}

// ReadFromOffset reads events starting at byteOffset. Used after
// FindLastCompactionOffset to replay just the post-compaction window.
func ReadFromOffset(path string, byteOffset int64) ([]Event, error) {
	if byteOffset < 0 {
		return nil, fmt.Errorf("negative rollout offset: %d", byteOffset)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if byteOffset > 0 {
		if _, err := f.Seek(byteOffset, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return parseStream(bufio.NewReader(f))
}

func parseStream(r *bufio.Reader) ([]Event, error) {
	var events []Event
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			ev, perr := parseLine(line)
			if perr == nil {
				events = append(events, ev)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return events, nil
			}
			return events, err
		}
	}
}

// parseLine recognizes the subset of rollout shapes splice cares about.
// We err on the side of returning KindUnknown rather than failing, so a
// future Codex schema change degrades quietly.
//
// The wire format we expect:
//
//	{"timestamp": "...", "type": "session_meta",        "payload": {...}}
//	{"timestamp": "...", "type": "response_item",       "payload": {"type":"function_call",...}}
//	{"timestamp": "...", "type": "response_item",       "payload": {"type":"function_call_output",...}}
//	{"timestamp": "...", "type": "context_compaction",  ...}
//
// (Older Codex versions used "kind" instead of "type" — we honor either.)
func parseLine(raw []byte) (Event, error) {
	raw = bytesTrimRight(raw, '\n', '\r', ' ', '\t')
	if len(raw) == 0 {
		return Event{}, errors.New("empty line")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Event{}, err
	}
	kindStr := jsonString(probe, "type", "kind")
	tsStr := jsonString(probe, "timestamp", "ts")
	ts, _ := time.Parse(time.RFC3339Nano, tsStr) // best effort

	ev := Event{Raw: append([]byte{}, raw...), Timestamp: ts, Kind: KindUnknown}

	switch kindStr {
	case "session_meta", "session_metadata":
		ev.Kind = KindSessionMeta
		ev.SessionMeta = parseSessionMeta(probe)
	case "context_compaction", "compaction", "compact":
		ev.Kind = KindCompaction
		ev.Compaction = &Compaction{Timestamp: ts}
	case "response_item", "turn_item", "item":
		// Need to look at payload.type to distinguish call vs result vs other.
		var payload map[string]json.RawMessage
		if pl, ok := probe["payload"]; ok {
			_ = json.Unmarshal(pl, &payload)
		}
		ptype := jsonString(payload, "type")
		switch ptype {
		case "function_call", "tool_call":
			ev.Kind = KindToolCall
			ev.ToolCall = parseToolCall(payload, ts)
		case "function_call_output", "tool_result":
			ev.Kind = KindToolResult
			ev.ToolResult = parseToolResult(payload, ts)
		default:
			ev.Kind = KindResponseItem
		}
	}
	return ev, nil
}

func parseSessionMeta(m map[string]json.RawMessage) *SessionMeta {
	var payload map[string]json.RawMessage
	if pl, ok := m["payload"]; ok {
		_ = json.Unmarshal(pl, &payload)
	}
	return &SessionMeta{
		SessionID: firstNonEmpty(jsonString(m, "session_id"), jsonString(payload, "session_id"), jsonString(payload, "id")),
		Cwd:       firstNonEmpty(jsonString(m, "cwd"), jsonString(payload, "cwd")),
	}
}

func parseToolCall(payload map[string]json.RawMessage, ts time.Time) *ToolCall {
	if payload == nil {
		return &ToolCall{Timestamp: ts}
	}
	args := jsonString(payload, "arguments", "args", "input")
	return &ToolCall{
		CallID:    jsonString(payload, "call_id", "id", "tool_use_id"),
		ToolName:  jsonString(payload, "name", "tool_name"),
		ArgsJSON:  args,
		Timestamp: ts,
	}
}

func parseToolResult(payload map[string]json.RawMessage, ts time.Time) *ToolResult {
	if payload == nil {
		return &ToolResult{Status: "ok", Timestamp: ts}
	}
	output := jsonString(payload, "output", "content", "text")
	exitCode := jsonInt(payload, "exit_code", "exitCode")
	status := classifyStatus(payload, exitCode)
	return &ToolResult{
		CallID:    jsonString(payload, "call_id", "id", "tool_use_id"),
		Output:    output,
		ExitCode:  exitCode,
		Status:    status,
		Timestamp: ts,
	}
}

func classifyStatus(payload map[string]json.RawMessage, exitCode *int) string {
	if payload == nil {
		return "ok"
	}
	if jsonBool(payload, "interrupted") {
		return "interrupted"
	}
	if jsonBool(payload, "timed_out") || jsonBool(payload, "timedOut") {
		return "timeout"
	}
	if jsonBool(payload, "is_error") {
		return "error"
	}
	if exitCode != nil && *exitCode != 0 {
		return "error"
	}
	return "ok"
}

// ----------------------------------------------------------------------
// JSON helpers — defensive about mixed types because Codex rollout schema
// has small variations across versions.
// ----------------------------------------------------------------------

func jsonString(m map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok || len(raw) == 0 {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			return s
		}
		// Some payloads use raw JSON object as the args; serialize it back.
		if raw[0] == '{' || raw[0] == '[' {
			return string(raw)
		}
	}
	return ""
}

func jsonBool(m map[string]json.RawMessage, keys ...string) bool {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var b bool
		if err := json.Unmarshal(raw, &b); err == nil && b {
			return true
		}
	}
	return false
}

func jsonInt(m map[string]json.RawMessage, keys ...string) *int {
	for _, k := range keys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var n int
		if err := json.Unmarshal(raw, &n); err == nil {
			return &n
		}
		var f float64
		if err := json.Unmarshal(raw, &f); err == nil {
			i := int(f)
			return &i
		}
	}
	return nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func bytesTrimRight(b []byte, cut ...byte) []byte {
	for len(b) > 0 {
		last := b[len(b)-1]
		drop := false
		for _, c := range cut {
			if last == c {
				drop = true
				break
			}
		}
		if !drop {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}

// FmtErr is a small helper for callers that want a stable error format
// when wrapping codex parse errors.
func FmtErr(path string, err error) error {
	return fmt.Errorf("codex %s: %w", path, err)
}
