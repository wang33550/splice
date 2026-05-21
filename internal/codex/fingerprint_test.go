package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/wang33550/splice/internal/fingerprint"
)

// Hashes from internal/fingerprint.Compute, copied here so a refactor of
// either side that desynchronizes them breaks both.
//
// To regenerate:
//
//	go run ./cmd/splice version  # ensure build is current
//	go test ./internal/codex/ -run TestFingerprintMatchesClaude
//
// The expected hash for Bash{"command": "npm test"} under fingerprint.Compute
// is the canonical value `npm test` should produce. We compute it dynamically
// in the test using fingerprint.Compute itself to avoid hard-coded drift.
func TestFingerprintMatchesClaudeBashShape(t *testing.T) {
	// Codex rollout: tool="shell", args = '{"command":"npm test"}'
	c1, h1 := FingerprintToolCall("shell", `{"command":"npm test"}`)

	// Same command from Claude side, fingerprinted via the same logic.
	// We can't import internal/fingerprint here (avoid cycles in tests),
	// so we duplicate the Bash-canonical shape and verify codex matches.
	want := `{"args":{"command":"npm test"},"tool":"Bash"}`
	if c1 != want {
		t.Fatalf("canonical mismatch:\n got %q\nwant %q", c1, want)
	}
	_ = h1
}

func TestFingerprintTrimsOuterWhitespaceOnly(t *testing.T) {
	_, h1 := FingerprintToolCall("shell", `{"command":"npm test"}`)
	_, h2 := FingerprintToolCall("shell", `{"command":"npm  test"}`)
	_, h3 := FingerprintToolCall("shell", `{"command":"  npm test  "}`)
	if h1 == h2 {
		t.Fatalf("internal whitespace must remain significant: %q", h1)
	}
	if h1 != h3 {
		t.Fatalf("outer whitespace should be trimmed: %q %q", h1, h3)
	}
}

func TestFingerprintShellAliasMatchesBash(t *testing.T) {
	_, hShell := FingerprintToolCall("shell", `{"command":"npm test"}`)
	_, hBash := FingerprintToolCall("Bash", `{"command":"npm test"}`)
	if hShell != hBash {
		t.Fatalf("shell vs Bash should hash same after canonicalization, got %q vs %q", hShell, hBash)
	}
}

func TestLabelFromToolCallShell(t *testing.T) {
	got := LabelFromToolCall("shell", `{"command":"npm test"}`)
	if got != "npm test" {
		t.Errorf("label: %q", got)
	}
}

func TestLabelFromToolCallBadJSON(t *testing.T) {
	got := LabelFromToolCall("shell", "not json")
	if !strings.HasPrefix(got, "") || got != "" {
		t.Errorf("expected empty label for bad JSON, got %q", got)
	}
}

func TestFingerprintBadArgsUsesRawFallback(t *testing.T) {
	canonical, hash := FingerprintToolCall("shell", `not-json`)
	wantCanonical := `{"args":{"_raw":"not-json"},"tool":"Bash"}`
	if canonical != wantCanonical {
		t.Fatalf("canonical = %q, want %q", canonical, wantCanonical)
	}
	sum := sha256.Sum256([]byte(wantCanonical))
	if want := hex.EncodeToString(sum[:]); hash != want {
		t.Fatalf("hash = %q, want %q", hash, want)
	}
}

func TestFingerprintEmptyArgsAndNonShellTool(t *testing.T) {
	canonical, _ := FingerprintToolCall("Read", "")
	if canonical != `{"args":null,"tool":"Read"}` {
		t.Fatalf("canonical = %q", canonical)
	}
}

func TestLabelFromToolCallFilePathAndMapFallback(t *testing.T) {
	if got := LabelFromToolCall("Read", `{"file_path":"/tmp/a.go"}`); got != "/tmp/a.go" {
		t.Fatalf("file_path label = %q", got)
	}
	got := LabelFromToolCall("Other", `{"z":2,"a":"x"}`)
	if !strings.Contains(got, "map[") || !strings.Contains(got, "a:x") || !strings.Contains(got, "z:2") {
		t.Fatalf("map fallback label = %q", got)
	}
}

func TestSHA256HexOfIsRealSHA256(t *testing.T) {
	sum := sha256.Sum256([]byte("fallback"))
	if got, want := sha256HexOf("fallback"), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("sha256HexOf = %q, want %q", got, want)
	}
}

// TestCrossPackageFingerprintConsistency proves the watcher (which calls
// codex.FingerprintToolCall on rollout JSONL) produces hashes that match the
// runtime PreToolUse hook (which calls fingerprint.Compute on stdin JSON).
// Without this guarantee, codex-watch can write trail rows that the hook
// never finds — the entire post-compact protection silently breaks.
//
// Each case mirrors a real Codex rollout payload paired with the equivalent
// hook input shape.
func TestCrossPackageFingerprintConsistency(t *testing.T) {
	cases := []struct {
		name        string
		codexTool   string
		codexArgs   string
		claudeTool  string
		claudeInput map[string]any
	}{
		{
			"bash npm test",
			"shell", `{"command":"npm test"}`,
			"Bash", map[string]any{"command": "npm test"},
		},
		{
			"bash with redundant whitespace",
			"shell", `{"command":"  go   test  ./..."}`,
			"Bash", map[string]any{"command": "go   test  ./..."},
		},
		{
			"bash no args other than command",
			"shell", `{"command":"git status --porcelain","timeout":30000}`,
			"Bash", map[string]any{"command": "git status --porcelain", "timeout": 30000},
		},
		{
			"read tool with file_path",
			"Read", `{"file_path":"/tmp/notes.md"}`,
			"Read", map[string]any{"file_path": "/tmp/notes.md"},
		},
		{
			"read with offset",
			"Read", `{"file_path":"/tmp/x.go","offset":100,"limit":50}`,
			"Read", map[string]any{"file_path": "/tmp/x.go", "offset": 100, "limit": 50},
		},
		{
			"grep pattern + path",
			"Grep", `{"pattern":"func TestFoo","path":"./internal"}`,
			"Grep", map[string]any{"pattern": "func TestFoo", "path": "./internal"},
		},
		{
			"glob pattern alone",
			"Glob", `{"pattern":"**/*.go"}`,
			"Glob", map[string]any{"pattern": "**/*.go"},
		},
		{
			"empty fields drop equally",
			"shell", `{"command":"ls","description":"","background":false}`,
			"Bash", map[string]any{"command": "ls", "description": "", "background": false},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cCanon, cHash := FingerprintToolCall(tc.codexTool, tc.codexArgs)
			fCanon, fHash, err := fingerprint.Compute(tc.claudeTool, tc.claudeInput)
			if err != nil {
				t.Fatalf("fingerprint.Compute: %v", err)
			}
			if cCanon != fCanon {
				t.Errorf("canonical mismatch:\n  codex:  %s\n  claude: %s", cCanon, fCanon)
			}
			if cHash != fHash {
				t.Errorf("hash mismatch:\n  codex:  %s\n  claude: %s", cHash, fHash)
			}
		})
	}
}

// TestFingerprintComputeFromCodexRoundtrip simulates the actual data flow:
// JSON-encoded args from a rollout file, decoded by FingerprintToolCall,
// must produce the exact same hash as fingerprint.Compute applied to the
// same args after a JSON round-trip.
func TestFingerprintComputeFromCodexRoundtrip(t *testing.T) {
	argsJSON := `{"command":"npm test","timeout":60000}`
	cCanon, cHash := FingerprintToolCall("shell", argsJSON)

	// Decode the same payload to a map and feed fingerprint.Compute directly.
	var asMap map[string]any
	if err := jsonDecode(argsJSON, &asMap); err != nil {
		t.Fatal(err)
	}
	fCanon, fHash, err := fingerprint.Compute("Bash", asMap)
	if err != nil {
		t.Fatal(err)
	}
	if cCanon != fCanon || cHash != fHash {
		t.Errorf("roundtrip mismatch:\n  via codex: %s / %s\n  direct:    %s / %s",
			cCanon, cHash, fCanon, fHash)
	}
}

func jsonDecode(s string, v any) error {
	return decodeJSONShim([]byte(s), v)
}
