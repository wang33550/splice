package hook

import "testing"

func TestIsBypassMode(t *testing.T) {
	cases := []struct {
		name string
		in   PreToolUseInput
		want bool
	}{
		{"plain default", PreToolUseInput{PermissionMode: "default"}, false},
		{"plain accept edits", PreToolUseInput{PermissionMode: "acceptEdits"}, false},
		{"plan mode", PreToolUseInput{PermissionMode: "plan"}, false},
		{"empty fields", PreToolUseInput{}, false},
		{"claude bypass", PreToolUseInput{PermissionMode: "bypassPermissions"}, true},
		{"codex full-auto via approval_mode", PreToolUseInput{ApprovalMode: "never"}, true},
		{"codex via approval_policy", PreToolUseInput{ApprovalPolicy: "never"}, true},
		{"codex via top-level policy never", PreToolUseInput{Policy: "never"}, true},
		{"codex via policy bypass", PreToolUseInput{Policy: "bypass"}, true},
		{"snake-case bypass_permissions", PreToolUseInput{Policy: "bypass_permissions"}, true},
		{"unknown approval value", PreToolUseInput{ApprovalMode: "ask"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.IsBypassMode(); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHostToolUseID(t *testing.T) {
	pre := PreToolUseInput{
		ToolUseID: " official ",
		CallID:    "call",
		ID:        "id",
	}
	if got := pre.HostToolUseID(); got != "official" {
		t.Fatalf("PreToolUseInput.HostToolUseID = %q, want official", got)
	}
	pre = PreToolUseInput{CallID: " call ", ID: "id"}
	if got := pre.HostToolUseID(); got != "call" {
		t.Fatalf("PreToolUseInput.HostToolUseID fallback = %q, want call", got)
	}
	pre = PreToolUseInput{ID: " id "}
	if got := pre.HostToolUseID(); got != "id" {
		t.Fatalf("PreToolUseInput.HostToolUseID id fallback = %q, want id", got)
	}

	post := PostToolUseInput{
		ToolUseID: " post-official ",
		CallID:    "post-call",
		ID:        "post-id",
	}
	if got := post.HostToolUseID(); got != "post-official" {
		t.Fatalf("PostToolUseInput.HostToolUseID = %q, want post-official", got)
	}
	post = PostToolUseInput{}
	if got := post.HostToolUseID(); got != "" {
		t.Fatalf("empty PostToolUseInput.HostToolUseID = %q, want empty", got)
	}
}

func TestExtractStatus(t *testing.T) {
	cases := []struct {
		name string
		resp map[string]any
		exit *int
		want string
	}{
		{"empty response, no exit", nil, nil, "ok"},
		{"empty response, exit 0", nil, intPtr(0), "ok"},
		{"empty response, exit 1", nil, intPtr(1), "error"},
		{"interrupted true", map[string]any{"interrupted": true}, nil, "interrupted"},
		{"timed_out true", map[string]any{"timed_out": true}, nil, "timeout"},
		{"timedOut alias", map[string]any{"timedOut": true}, nil, "timeout"},
		{"is_error", map[string]any{"is_error": true}, nil, "error"},
		{"error string non-empty", map[string]any{"error": "boom"}, nil, "error"},
		{"error object non-empty", map[string]any{"error": map[string]any{"message": "boom"}}, nil, "error"},
		{"error bool false ignored", map[string]any{"error": false}, nil, "ok"},
		{"explicit status string ok", map[string]any{"status": "ok"}, nil, "ok"},
		{"explicit status interrupted", map[string]any{"status": "interrupted"}, nil, "interrupted"},
		{"unknown status falls through", map[string]any{"status": "weird"}, nil, "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractStatus(tc.resp, tc.exit); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLabelFor(t *testing.T) {
	cases := []struct {
		name string
		tool string
		in   map[string]any
		want string
	}{
		{"nil", "Bash", nil, ""},
		{"bash command", "Bash", map[string]any{"command": "npm test"}, "npm test"},
		{"read path", "Read", map[string]any{"file_path": "/tmp/a.go"}, "/tmp/a.go"},
		{"write path", "Write", map[string]any{"file_path": "/tmp/a.go"}, "/tmp/a.go"},
		{"edit path", "Edit", map[string]any{"file_path": "/tmp/a.go"}, "/tmp/a.go"},
		{"notebook path", "NotebookEdit", map[string]any{"file_path": "/tmp/a.ipynb"}, "/tmp/a.ipynb"},
		{"glob pattern", "Glob", map[string]any{"pattern": "**/*.go"}, "**/*.go"},
		{"grep pattern", "Grep", map[string]any{"pattern": "func Test"}, "func Test"},
		{"web fetch url", "WebFetch", map[string]any{"url": "https://example.com"}, "https://example.com"},
		{"web search query", "WebSearch", map[string]any{"query": "go release"}, "go release"},
		{"unknown", "Other", map[string]any{"command": "x"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LabelFor(tc.tool, tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractOutput(t *testing.T) {
	cases := []struct {
		name string
		resp map[string]any
		want string
	}{
		{"nil", nil, ""},
		{"output", map[string]any{"output": "out"}, "out"},
		{"stdout", map[string]any{"stdout": "std"}, "std"},
		{"content", map[string]any{"content": "body"}, "body"},
		{"result", map[string]any{"result": "res"}, "res"},
		{"text", map[string]any{"text": "txt"}, "txt"},
		{"empty output falls through", map[string]any{"output": "", "stdout": "std"}, "std"},
		{"non-string ignored", map[string]any{"output": 123}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtractOutput(tc.resp); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractExitCode(t *testing.T) {
	cases := []struct {
		name string
		resp map[string]any
		want *int
	}{
		{"nil", nil, nil},
		{"exit_code", map[string]any{"exit_code": float64(2)}, intPtr(2)},
		{"exitCode", map[string]any{"exitCode": float64(3)}, intPtr(3)},
		{"code", map[string]any{"code": float64(4)}, intPtr(4)},
		{"status int", map[string]any{"status": 5}, intPtr(5)},
		{"none", map[string]any{"exit_code": "1"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractExitCode(tc.resp)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %d, want nil", *got)
				}
				return
			}
			if got == nil || *got != *tc.want {
				t.Fatalf("got %v, want %d", got, *tc.want)
			}
		})
	}
}

func intPtr(n int) *int { return &n }
