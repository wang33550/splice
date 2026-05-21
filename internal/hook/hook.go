// Package hook describes the wire format for Claude Code's PreToolUse and
// PostToolUse hook events. Only the fields splice consumes are modeled.
package hook

import "strings"

// PreToolUseInput is the stdin payload Claude Code sends for PreToolUse.
//
// We accept several alternative permission-mode field names to stay tolerant
// of Codex/Claude schema drift. As of writing, Claude Code sets
// "permission_mode" and Codex sets "approval_mode" or includes the policy
// in a top-level "policy" field (per developers.openai.com/codex docs).
type PreToolUseInput struct {
	SessionID      string         `json:"session_id"`
	HookEventName  string         `json:"hook_event_name"`
	Cwd            string         `json:"cwd"`
	ToolUseID      string         `json:"tool_use_id,omitempty"`
	CallID         string         `json:"call_id,omitempty"`
	ID             string         `json:"id,omitempty"`
	ToolName       string         `json:"tool_name"`
	ToolInput      map[string]any `json:"tool_input"`
	PermissionMode string         `json:"permission_mode,omitempty"`
	ApprovalMode   string         `json:"approval_mode,omitempty"` // Codex variant
	ApprovalPolicy string         `json:"approval_policy,omitempty"`
	Policy         string         `json:"policy,omitempty"`
}

// IsBypassMode reports whether the host is running in a mode where
// permissionDecision: "ask" gets silently swallowed (i.e. treated as allow).
// In those modes splice degrades to a deny + cached-injection path so the
// model still benefits from the post-compact protection.
//
// Recognized signals (any one triggers bypass):
//
//   - permission_mode == "bypassPermissions"  (Claude Code)
//   - approval_mode == "never"                (Codex --full-auto)
//   - approval_policy == "never"              (Codex config alias)
//   - policy == "never" or "bypass"           (older variants)
func (in PreToolUseInput) IsBypassMode() bool {
	if in.PermissionMode == "bypassPermissions" {
		return true
	}
	for _, v := range []string{in.ApprovalMode, in.ApprovalPolicy, in.Policy} {
		switch v {
		case "never", "bypass", "bypassPermissions", "bypass_permissions":
			return true
		}
	}
	return false
}

// HostToolUseID returns the host-provided identifier for this exact tool
// invocation. Official Claude/Codex hooks use tool_use_id; aliases keep splice
// tolerant of older Codex rollout/hook schema variants.
func (in PreToolUseInput) HostToolUseID() string {
	return firstNonEmpty(in.ToolUseID, in.CallID, in.ID)
}

// PostToolUseInput is the stdin payload for PostToolUse. tool_response shape
// varies by tool; we capture it as raw map and extract what's needed.
type PostToolUseInput struct {
	SessionID     string         `json:"session_id"`
	HookEventName string         `json:"hook_event_name"`
	Cwd           string         `json:"cwd"`
	ToolUseID     string         `json:"tool_use_id,omitempty"`
	CallID        string         `json:"call_id,omitempty"`
	ID            string         `json:"id,omitempty"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	ToolResponse  map[string]any `json:"tool_response"`
}

// HostToolUseID is the PostToolUse twin of PreToolUseInput.HostToolUseID.
func (in PostToolUseInput) HostToolUseID() string {
	return firstNonEmpty(in.ToolUseID, in.CallID, in.ID)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// PreCompactInput is the stdin payload Claude Code sends for PreCompact.
// Only session_id / cwd are interesting for splice's freeze action.
type PreCompactInput struct {
	SessionID     string `json:"session_id"`
	HookEventName string `json:"hook_event_name"`
	Cwd           string `json:"cwd"`
	Trigger       string `json:"trigger,omitempty"` // "auto" | "manual"
}

// CodexPreToolUseOutput is what Codex CLI expects from a PreToolUse hook.
// It differs from Claude Code: the decision lives nested under "decision"
// rather than as a flat "permissionDecision" field.
type CodexPreToolUseOutput struct {
	HookSpecificOutput CodexPreToolUseHookOutput `json:"hookSpecificOutput"`
}

type CodexPreToolUseHookOutput struct {
	HookEventName     string         `json:"hookEventName"`
	Decision          *CodexDecision `json:"decision,omitempty"`
	AdditionalContext string         `json:"additionalContext,omitempty"`
}

// CodexDecision is the nested decision shape Codex requires.
//
//	{"behavior": "allow"}
//	{"behavior": "deny", "reason": "..."}
//	{"behavior": "ask", "reason": "..."}
type CodexDecision struct {
	Behavior string `json:"behavior"`
	Reason   string `json:"reason,omitempty"`
}

// Source values:
//   - "startup": new session
//   - "resume":  --resume / --continue / picked from history
//   - "clear":   /clear inside a running session
//   - "compact": fired after auto/manual compaction completes
//
// splice writes a "session marker" file for startup/resume so the codex-watch
// daemon (or any other observer) can discover active sessions per cwd. On
// "clear" we also wipe this session's trails because the model just lost
// its memory and any cached results would mislead it.
type SessionStartInput struct {
	SessionID      string `json:"session_id"`
	HookEventName  string `json:"hook_event_name"`
	Cwd            string `json:"cwd"`
	Source         string `json:"source"`
	TranscriptPath string `json:"transcript_path,omitempty"`
}

// PreToolUseOutput is the JSON we write to stdout. permissionDecision
// "deny" blocks the tool call; "allow" lets it run; an empty value plus
// additionalContext just injects a hint without affecting permissioning.
type PreToolUseOutput struct {
	HookSpecificOutput PreToolUseHookOutput `json:"hookSpecificOutput"`
}

type PreToolUseHookOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
}

// PostToolUseOutput follows the same shape; PostToolUse cannot block but can
// inject additional context next to the tool result.
type PostToolUseOutput struct {
	HookSpecificOutput PostToolUseHookOutput `json:"hookSpecificOutput"`
}

type PostToolUseHookOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// LabelFor extracts a one-line human label for a tool invocation, used in
// injected text. For Bash it returns the command; for file tools the path.
func LabelFor(toolName string, toolInput map[string]any) string {
	if toolInput == nil {
		return ""
	}
	switch toolName {
	case "Bash":
		if v, ok := toolInput["command"].(string); ok {
			return v
		}
	case "Read", "Write", "Edit", "NotebookEdit":
		if v, ok := toolInput["file_path"].(string); ok {
			return v
		}
	case "Glob":
		if v, ok := toolInput["pattern"].(string); ok {
			return v
		}
	case "Grep":
		if v, ok := toolInput["pattern"].(string); ok {
			return v
		}
	case "WebFetch":
		if v, ok := toolInput["url"].(string); ok {
			return v
		}
	case "WebSearch":
		if v, ok := toolInput["query"].(string); ok {
			return v
		}
	}
	return ""
}

// ExtractOutput pulls a representative output string out of a PostToolUse
// tool_response payload. Different tools shape this differently; we look at
// the most common keys.
func ExtractOutput(toolResponse map[string]any) string {
	if toolResponse == nil {
		return ""
	}
	for _, k := range []string{"output", "stdout", "content", "result", "text"} {
		if v, ok := toolResponse[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// ExtractExitCode pulls an exit code out of a PostToolUse tool_response.
// Returns nil when no numeric exit code is present.
func ExtractExitCode(toolResponse map[string]any) *int {
	if toolResponse == nil {
		return nil
	}
	for _, k := range []string{"exit_code", "exitCode", "code", "status"} {
		switch v := toolResponse[k].(type) {
		case float64:
			i := int(v)
			return &i
		case int:
			i := v
			return &i
		}
	}
	return nil
}

// ExtractStatus classifies the tool response into one of:
//
//	ok | error | interrupted | timeout | unknown
//
// Used by splice to decide whether a recorded result is fit to be served
// as a cache hit. Anything other than "ok" is disqualified.
//
// Inputs we honor:
//   - tool_response.interrupted: true → "interrupted"
//   - tool_response.timed_out / timedOut: true → "timeout"
//   - tool_response.error: any non-empty value → "error"
//   - tool_response.is_error: true → "error"
//   - tool_response.status: explicit string → returned verbatim if recognized
//   - exit code (when present and != 0) → "error"
//   - everything else → "ok"
func ExtractStatus(toolResponse map[string]any, exitCode *int) string {
	if toolResponse != nil {
		if b, ok := toolResponse["interrupted"].(bool); ok && b {
			return "interrupted"
		}
		if b, ok := toolResponse["timed_out"].(bool); ok && b {
			return "timeout"
		}
		if b, ok := toolResponse["timedOut"].(bool); ok && b {
			return "timeout"
		}
		if b, ok := toolResponse["is_error"].(bool); ok && b {
			return "error"
		}
		if v, ok := toolResponse["error"]; ok {
			if s, ok := v.(string); ok && s != "" {
				return "error"
			}
			if v != nil && v != false {
				return "error"
			}
		}
		if s, ok := toolResponse["status"].(string); ok {
			switch s {
			case "ok", "error", "interrupted", "timeout":
				return s
			}
		}
	}
	if exitCode != nil && *exitCode != 0 {
		return "error"
	}
	return "ok"
}
