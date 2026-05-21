# Changelog

## v0.5.0 - 2026-05-22

First beta release prepared for public GitHub use.

Added:

- Per-session SQLite stores under `<cwd>/.splice/sessions/`.
- Claude Code hook integration for PreToolUse, PostToolUse, PreCompact, and
  SessionStart.
- Codex hook integration plus `splice codex-watch` for compaction detection.
- Official `tool_use_id` pairing for safer PreToolUse/PostToolUse matching.
- `/clear` handling for Claude Code and Codex.
- In-flight handling for long-running tasks that cross compaction.
- Full post-call trail restoration from the repeated command through the
  compaction boundary.
- Configurable live/status Bash patterns through `.splice/config.json`.
- GitHub CI and release-build workflow.

Known beta limits:

- Codex requires a separately running watcher.
- Codex rollout JSONL parsing may need updates if Codex changes its private
  rollout schema.
- splice cannot detect file changes made outside the host agent.
- `.splice/` may contain sensitive local tool output and must remain private.
