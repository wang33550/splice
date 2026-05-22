// Package fingerprint computes byte-precise hashes over (tool_name, normalized args)
// so PreToolUse can match a previous call deterministically.
package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Compute returns (canonicalJSON, hexHash) for the given tool invocation.
// Normalization rules:
//   - object keys are sorted (recursive)
//   - empty strings, nil values, and empty objects/arrays are dropped
//   - for Bash, only leading/trailing whitespace around "command" is trimmed
func Compute(toolName string, args map[string]any) (string, string, error) {
	return ComputeScoped(toolName, args, "")
}

// ComputeScoped is Compute plus an execution scope, normally store.ProjectKey(cwd).
// Scope is part of semantic identity: `npm test` in project A is not the same
// fact as `npm test` in project B, even inside one desktop conversation.
func ComputeScoped(toolName string, args map[string]any, scope string) (string, string, error) {
	normalized := normalize(args)
	if toolName == "Bash" {
		normalized = normalizeBash(normalized)
	}
	payload := map[string]any{
		"tool": toolName,
		"args": normalized,
	}
	if strings.TrimSpace(scope) != "" {
		payload["scope"] = strings.TrimSpace(scope)
	}
	canonical, err := canonicalJSON(payload)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return canonical, hex.EncodeToString(sum[:]), nil
}

func normalizeBash(args any) any {
	m, ok := args.(map[string]any)
	if !ok {
		return args
	}
	if cmd, ok := m["command"].(string); ok {
		m["command"] = strings.TrimSpace(cmd)
	}
	return m
}

func normalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			n := normalize(val)
			if isEmpty(n) {
				continue
			}
			out[k] = n
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		out := []any{}
		for _, item := range x {
			n := normalize(item)
			if isEmpty(n) {
				continue
			}
			out = append(out, n)
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case string:
		if x == "" {
			return nil
		}
		return x
	case nil:
		return nil
	default:
		return x
	}
}

func isEmpty(v any) bool {
	if v == nil {
		return true
	}
	if s, ok := v.(string); ok && s == "" {
		return true
	}
	if m, ok := v.(map[string]any); ok && len(m) == 0 {
		return true
	}
	if a, ok := v.([]any); ok && len(a) == 0 {
		return true
	}
	return false
}

// canonicalJSON marshals v with deterministically sorted keys at every level.
func canonicalJSON(v any) (string, error) {
	var sb strings.Builder
	if err := writeCanonical(&sb, v); err != nil {
		return "", err
	}
	return sb.String(), nil
}

func writeCanonical(sb *strings.Builder, v any) error {
	switch x := v.(type) {
	case map[string]any:
		sb.WriteByte('{')
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i, k := range keys {
			if i > 0 {
				sb.WriteByte(',')
			}
			b, err := json.Marshal(k)
			if err != nil {
				return err
			}
			sb.Write(b)
			sb.WriteByte(':')
			if err := writeCanonical(sb, x[k]); err != nil {
				return err
			}
		}
		sb.WriteByte('}')
	case []any:
		sb.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				sb.WriteByte(',')
			}
			if err := writeCanonical(sb, item); err != nil {
				return err
			}
		}
		sb.WriteByte(']')
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Errorf("fingerprint: marshal %T: %w", x, err)
		}
		sb.Write(b)
	}
	return nil
}
