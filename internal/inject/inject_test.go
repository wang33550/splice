package inject

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestFormatDenyHitContainsBasics(t *testing.T) {
	exit := 0
	got := FormatDenyHit("Bash", "npm test", &exit, 5*time.Minute, "12 passed")
	for _, want := range []string{"上次已运行", "Bash", "5 分钟前", "exit 0", "npm test", "12 passed"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
}

func TestFormatAskReasonOmitsExitWhenNil(t *testing.T) {
	got := FormatAskReason("Read", "/tmp/x.go", nil, time.Second, "content")
	if strings.Contains(got, "exit") {
		t.Errorf("ask reason should not mention exit when ec is nil:\n%s", got)
	}
	if !strings.Contains(got, "/tmp/x.go") {
		t.Errorf("missing label; got %s", got)
	}
	if !strings.Contains(got, "splice: 检测到压缩后重复执行") {
		t.Errorf("missing splice header; got %s", got)
	}
}

func TestFormatTerminalHitContextIncludesFollowupTrail(t *testing.T) {
	exit := 0
	after := []EventDescriptor{
		{ToolName: "Bash", Label: "git status --porcelain", Output: "clean", Status: "ok",
			ExitCode: sql.NullInt64{Int64: 0, Valid: true}},
		{ToolName: "Bash", Label: "tail -n 20 sim.log", Output: "progress=80%", Status: "ok",
			ExitCode: sql.NullInt64{Int64: 0, Valid: true}},
		{ToolName: "Read", Label: "/tmp/report.txt", Output: "final note", Status: "ok"},
	}
	got := FormatTerminalHitContext("Bash", "npm test", &exit, 3*time.Minute, "12 passed", after)
	for _, want := range []string{
		"重复调用：npm test",
		"重复调用的上次输出",
		"12 passed",
		"git status --porcelain",
		"tail -n 20 sim.log",
		"progress=80%",
		"/tmp/report.txt",
		"压缩前历史结果",
		"如需要当前状态应重新查询",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("terminal context missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatTerminalHitContextEmptyFollowup(t *testing.T) {
	got := FormatTerminalHitContext("Read", "/tmp/a.go", nil, time.Second, "content", nil)
	for _, want := range []string{"未观察到其他工具调用", "/tmp/a.go", "content"} {
		if !strings.Contains(got, want) {
			t.Errorf("empty terminal context missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatInFlightEmptyAfter(t *testing.T) {
	got := FormatInFlight("Bash", "npm run dev", time.Minute, nil)
	for _, want := range []string{"上次启动 Bash", "未观察到其他工具调用", "建议", "ps"} {
		if !strings.Contains(got, want) {
			t.Errorf("empty-after branch missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatInFlightWithCausalChain(t *testing.T) {
	after := []EventDescriptor{
		{ToolName: "Bash", Label: "git status", Output: "clean", Status: "ok",
			ExitCode: sql.NullInt64{Int64: 0, Valid: true}},
		{ToolName: "Edit", Label: "/main.go", Output: "", Status: "ok"},
		{ToolName: "Bash", Label: "go test", Output: "ok    foo  1.234s", Status: "ok",
			ExitCode: sql.NullInt64{Int64: 0, Valid: true}},
	}
	got := FormatInFlight("Bash", "docker compose up", time.Minute, after)
	for _, want := range []string{"上次启动 Bash", "依次发生了", "git status", "/main.go", "go test", "原任务本身的最终结果仍未知"} {
		if !strings.Contains(got, want) {
			t.Errorf("causal-chain branch missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatInFlightTruncatesAfterTwelve(t *testing.T) {
	after := make([]EventDescriptor, 20)
	for i := range after {
		after[i] = EventDescriptor{ToolName: "Bash", Label: "ls", Status: "ok"}
	}
	got := FormatInFlight("Bash", "long task", time.Minute, after)
	if strings.Contains(got, "... 还有") {
		t.Errorf("event list should not omit calls now that terminal context recovery needs full trail; got:\n%s", got)
	}
	if !strings.Contains(got, "20. Bash ls") {
		t.Errorf("expected full event list through item 20; got:\n%s", got)
	}
}

func TestHumanDurationBuckets(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Millisecond, "刚刚"},
		{2 * time.Second, "2 秒"},
		{2 * time.Minute, "2 分钟"},
		{2 * time.Hour, "2.0 小时"},
	}
	for _, tc := range cases {
		if got := humanDuration(tc.d); got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestTruncateLong(t *testing.T) {
	long := strings.Repeat("a", maxOutputChars+500)
	got := truncate(long, maxOutputChars)
	if !strings.HasSuffix(got, "... (truncated)") {
		t.Errorf("expected truncation marker; got %q (len=%d)", got[len(got)-30:], len(got))
	}
}
