# Changelog

## v0.5.1 - 2026-05-22

First beta release prepared for public GitHub use.

Added:

- User-global, per-session SQLite stores under `~/.splice/sessions/`.
- Claude Code hook integration for PreToolUse, PostToolUse, PreCompact, and
  SessionStart.
- Codex hook integration plus auto-started global `splice codex-watch` for
  compaction detection.
- Official `tool_use_id` pairing for safer PreToolUse/PostToolUse matching.
- `/clear` handling for Claude Code and Codex.
- In-flight handling for long-running tasks that cross compaction.
- Full post-call trail restoration from the repeated command through the
  compaction boundary.
- Project-scoped command identity so the same session can switch projects
  without reusing project A command results in project B.
- Configurable live/status Bash patterns through `~/.splice/config.json` and
  project-local `<cwd>/.splice/config.json`.
- GitHub CI and release-build workflow.
- Conservative fence behavior for unknown non-Bash host/MCP tools.
- Codex watcher recovery for rollout truncation/replacement, newer rollout file
  discovery, temporary tail failure, newest-rollout selection, and stale orphan
  session markers.

Known beta limits:

- Codex requires the global watcher because Codex has no PreCompact hook; the
  SessionStart hook starts it automatically.
- Codex rollout JSONL parsing may need updates if Codex changes its private
  rollout schema.
- splice cannot detect file changes made outside the host agent.
- `~/.splice/` may contain sensitive local tool output and must remain private.
