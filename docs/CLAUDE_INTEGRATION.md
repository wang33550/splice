# Claude Code Integration

This guide installs splice into Claude Code through official hooks. No wrapper,
PTY injection, or network service is required.

## Requirements

- Claude Code with hooks enabled.
- `splice` available on `PATH`, or a local splice binary that will not be moved
  after installation.

If you build from source:

```bash
go build -o splice ./cmd/splice
./splice version
```

Windows users may build `splice.exe` instead:

```powershell
go build -o splice.exe ./cmd/splice
.\splice.exe version
```

## Install Hooks

Project-local installation is usually best when you only want splice in one
repository:

```bash
cd /path/to/your/project
splice install-claude-hooks --local
```

This writes `<cwd>/.claude/settings.local.json`. Claude Code normally treats
this as a local settings file; keep it out of repository commits unless you
intentionally want to share that configuration.

User-wide installation:

```bash
splice install-claude-hooks --user
```

This writes `~/.claude/settings.json`.

The installer is idempotent. It preserves unrelated hook entries and rewrites
only entries marked with `description: "splice-managed"`.

Restart Claude Code after changing hook settings.

## Runtime Flow

- `SessionStart`: marks the active session and clears state when `source=clear`.
- `PreToolUse`: records an in-flight tool call and checks for a safe
  post-compaction duplicate.
- `PostToolUse`: attaches the terminal result to the same tool call, using
  official `tool_use_id` when present.
- `PreCompact`: freezes the current live trail into the recoverable
  pre-compaction snapshot.

If Claude Code runs in a bypass mode where ask decisions are swallowed, splice
automatically uses deny + injected context instead of ask.

## Verify

1. Start Claude Code in the project.
2. Run a stable command such as `npm test`.
3. Let automatic compaction happen.
4. Ask the model to run the same command again.

In default ask mode, Claude Code should show a tool confirmation with splice's
prior result and guidance. If you reject the repeated command, splice injects
the cached result back to the model. If you allow it, the tool runs normally and
splice cools down that hash for the current window.

Manual hook smoke test:

```bash
echo '{"session_id":"test","hook_event_name":"PreToolUse","cwd":".","tool_name":"Bash","tool_input":{"command":"ls"}}' | splice pre-tool-use
```

Expected output when no compaction snapshot exists:

```json
{"hookSpecificOutput":{"hookEventName":"PreToolUse"}}
```

## Storage

Runtime state is under `~/.splice/` (or `$SPLICE_HOME` when set). It is keyed by
conversation `session_id`; cwd is stored as metadata for configuration/routing,
not used as the ownership boundary:

```text
~/.splice/
  sessions/
    <session-id>.db
    <session-id>.db-shm
    <session-id>.db-wal
    <session-id>.pending/
    <session-id>.meta.json
  active-sessions/<session-id>.json
```

The SQLite database can contain command arguments and tool output. Do not share
it.

Reset local splice state:

```bash
rm -rf ~/.splice
```

PowerShell:

```powershell
Remove-Item -LiteralPath "$HOME\.splice" -Recurse -Force
```

## Uninstall

```bash
splice uninstall-claude-hooks --local
```

Or:

```bash
splice uninstall-claude-hooks --user
```

Only splice-managed entries are removed. Unrelated hooks and top-level settings
are preserved.

## Known Limits

- Claude Code must be restarted after settings changes.
- splice cannot see file changes made outside Claude Code.
- Command identity is scoped by project `cwd`; the same command text in a
  different project is treated as a different fact.
- `/clear` should be treated as an explicit state boundary; do not assume the
  host always changes `session_id`.
- Without `tool_use_id`, late PostToolUse events after `/clear` are ambiguous.
  splice handles them conservatively and avoids claiming them as completed
  results.
- Failed, interrupted, and timed-out results are never restored.
