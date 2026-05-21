// Package inject formats the human-readable text splice writes back to Claude
// Code via additionalContext (deny path) or permissionDecisionReason (ask path).
package inject

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const maxOutputChars = 2000
const maxTrailOutputChars = 600

// EventDescriptor is the minimal shape of a trail entry that FormatInFlight
// needs. We define it locally to avoid an import cycle with the store package.
type EventDescriptor struct {
	ToolName string
	Label    string
	Output   string
	Status   string
	ExitCode sql.NullInt64
}

// FormatDenyHit: same args matched a fresh, successful pre-compact entry,
// no fence event followed it. Inject the cached output and deny the call.
func FormatDenyHit(toolName, label string, exitCode *int, ago time.Duration, output string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[splice] 上次已运行 %s（%s前", toolName, humanDuration(ago))
	if exitCode != nil {
		fmt.Fprintf(&b, "，exit %d", *exitCode)
	}
	b.WriteString("）\n")
	if label != "" {
		fmt.Fprintf(&b, "调用：%s\n", oneLine(label))
	}
	out := truncate(strings.TrimSpace(output), maxOutputChars)
	if out != "" {
		b.WriteString("输出：\n")
		b.WriteString(out)
		b.WriteString("\n")
	}
	b.WriteString("压缩前后未检测到 Edit/Write/Bash，splice 判定模型可能丢失了上次结果。可直接复用。如确需重跑请显式指示。")
	return b.String()
}

// FormatAskReason: shown in Claude Code's permission prompt when ask mode is on.
// Must give the user enough context to choose Allow vs Reject without seeing
// the rest of the conversation.
func FormatAskReason(toolName, label string, exitCode *int, ago time.Duration, output string) string {
	var b strings.Builder
	b.WriteString("splice: 检测到压缩后重复执行\n\n")
	fmt.Fprintf(&b, "命令：%s\n", oneLine(label))
	fmt.Fprintf(&b, "上次结果：%s前", humanDuration(ago))
	if exitCode != nil {
		fmt.Fprintf(&b, "，exit %d", *exitCode)
	}
	b.WriteString("\n")
	out := truncate(strings.TrimSpace(output), 800)
	if out != "" {
		b.WriteString("输出摘要：\n")
		b.WriteString(out)
		b.WriteString("\n")
	}
	b.WriteString("\n压缩前后此命令之后未检测到 Edit/Write/副作用 Bash —— splice 怀疑模型只是丢失了上次结果。\n")
	b.WriteString("splice 也会把从该命令开始直到压缩前的后续工具轨迹注入给模型。\n\n")
	b.WriteString("建议：\n")
	b.WriteString("- 如果你（在 IDE 或另一终端）改过文件，splice 看不到这种改动 → 选 Allow 让模型重跑\n")
	b.WriteString("- 如果你没改文件 → 选 Reject，splice 会把上次结果交给模型，省一次重跑")
	return b.String()
}

// FormatTerminalHitContext restores the causal slice the model likely lost:
// the repeated call itself plus every observed tool event after it and before
// the compaction boundary. Later live-state observations are explicitly marked
// as historical so the model does not confuse old `tail`/`ps` output with the
// current simulator/process state.
func FormatTerminalHitContext(toolName, label string, exitCode *int, ago time.Duration, output string, after []EventDescriptor) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[splice] 检测到压缩后重复执行：上次已运行 %s（%s前", toolName, humanDuration(ago))
	if exitCode != nil {
		fmt.Fprintf(&b, "，exit %d", *exitCode)
	}
	b.WriteString("）\n")
	if label != "" {
		fmt.Fprintf(&b, "重复调用：%s\n", oneLine(label))
	}

	out := truncate(strings.TrimSpace(output), maxOutputChars)
	if out != "" {
		b.WriteString("重复调用的上次输出：\n")
		b.WriteString(out)
		b.WriteString("\n")
	}

	if len(after) == 0 {
		b.WriteString("\n该调用完成后到压缩发生前，splice 未观察到其他工具调用。\n")
		b.WriteString("压缩前后未检测到 Edit/Write/副作用 Bash；如无外部改动，可直接基于以上结果继续。\n")
		return b.String()
	}

	fmt.Fprintf(&b, "\n该调用之后、压缩发生前，splice 还观察到以下 %d 个工具事件（均为压缩前历史结果）：\n", len(after))
	writeEventList(&b, after)
	b.WriteString("\n请把以上轨迹视为压缩前已经完成/观察到的上下文；其中 live 状态查询（例如 tail/ps/docker/kubectl）只代表当时状态，如需要当前状态应重新查询。\n")
	return b.String()
}

func humanDuration(d time.Duration) string {
	if d < time.Second {
		return "刚刚"
	}
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
	return fmt.Sprintf("%.1f 小时", d.Hours())
}

// AfterEvent isn't a real type any more — kept as deprecated stub so callers
// have a single migration point. Use EventDescriptor in new code.
type AfterEvent = EventDescriptor

// FormatInFlight is the in-flight branch of the v0.4.1 long-task path.
// Two sub-branches based on whether anything happened after the unfinished
// task:
//
//   - empty: just warn the model that the task started but never produced
//     a result, suggesting it confirm process state before re-launching.
//   - non-empty: dump the causal chain so the model knows what work was
//     done in the meantime, even though the original task itself is in
//     unknown state.
//
// We never inject the original task's stdout because, by definition, an
// in-flight task has no terminal output to show.
func FormatInFlight(toolName, label string, ago time.Duration, after []EventDescriptor) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[splice] 上次启动 %s（%s前）但压缩前未拿到完成结果\n", toolName, humanDuration(ago))
	if label != "" {
		fmt.Fprintf(&b, "调用：%s\n", oneLine(label))
	}

	if len(after) == 0 {
		b.WriteString("\n该工具启动后未观察到其他工具调用。\n")
		b.WriteString("建议：在重启之前先确认上次进程是否仍在运行（ps / pgrep / 检查 PID 等），\n")
		b.WriteString("以免重复启动后台任务。\n")
		return b.String()
	}

	b.WriteString("\n该任务启动后压缩前依次发生了以下工具调用：\n")
	writeEventList(&b, after)
	b.WriteString("\n以上事件已完成；但原任务本身的最终结果仍未知。\n")
	b.WriteString("建议：先确认原任务是否仍在运行再决定是否重启。\n")
	return b.String()
}

func writeEventList(b *strings.Builder, events []EventDescriptor) {
	for i, e := range events {
		fmt.Fprintf(b, "%d. %s", i+1, e.ToolName)
		if l := oneLine(e.Label); l != "" {
			fmt.Fprintf(b, " %s", l)
		}
		if e.ExitCode.Valid {
			fmt.Fprintf(b, " → exit %d", e.ExitCode.Int64)
		}
		if e.Status != "" && e.Status != "ok" {
			fmt.Fprintf(b, " [%s]", e.Status)
		}
		out := truncate(strings.TrimSpace(e.Output), maxTrailOutputChars)
		if out != "" {
			fmt.Fprintf(b, "\n   输出：%s", oneLineMulti(out))
		}
		b.WriteString("\n")
	}
}

// oneLineMulti collapses internal newlines to "; " for display in single-line
// trail summaries. Differs from oneLine which collapses all whitespace.
func oneLineMulti(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", "; ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... (truncated)"
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
