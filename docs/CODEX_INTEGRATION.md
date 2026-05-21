# Codex Integration

Codex integration has one extra moving part: Codex exposes PreToolUse,
PostToolUse, and SessionStart hooks, but not a PreCompact hook. splice therefore
uses `splice codex-watch` to watch Codex rollout JSONL files and detect
compaction boundaries.

## Requirements

- Codex with hooks enabled.
- `splice` available on `PATH`, or a local splice binary that will not be moved
  after installation.
- A running watcher in each project directory where you want Codex compaction
  protection.

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

Project-local installation is recommended:

```bash
cd /path/to/your/project
splice install-codex-hooks --project
```

This writes `<cwd>/.codex/config.toml`.

User-wide installation:

```bash
splice install-codex-hooks --user
```

This writes `$CODEX_HOME/config.toml`, or `~/.codex/config.toml` when
`CODEX_HOME` is not set.

The installer is idempotent. It preserves unrelated config and rewrites only
entries marked with `description = "splice-managed"`.

## Start The Watcher

The watcher must stay running while Codex runs. It does not have to be visible,
and it does not have to use a second terminal, but it must be a separate running
process.

The simplest option is two terminals.

Terminal 1, in the project directory:

```bash
splice codex-watch
```

Terminal 2, same project directory:

```bash
codex
```

One-terminal option for bash, zsh, or Git Bash:

```bash
mkdir -p .splice
splice codex-watch > .splice/codex-watch.log 2>&1 &
codex
```

One-terminal option for PowerShell:

```powershell
Start-Process -FilePath splice -ArgumentList @("codex-watch", "--cwd", (Get-Location).Path) -WindowStyle Hidden
codex
```

The watcher is bound to the current working directory. If you use Codex in
multiple projects at the same time, run one watcher per project. Multiple Codex
sessions in the same project can share one watcher.

The watcher:

1. Reads active session markers written by the SessionStart hook.
2. Locates the matching rollout JSONL under `$CODEX_HOME/sessions/...` or
   `~/.codex/sessions/...`.
3. Replays existing events and tails new events.
4. Freezes the trail when it sees a context-compaction event.

If the watcher stops, new compactions are not protected until it is restarted.
When restarted, it replays rollout files and rebuilds state as long as the
rollout files still exist.

## Configuration

Project-local config:

```text
<project>/.splice/config.json
```

Global config:

```text
~/.splice/config.json
```

Example:

```json
{
  "ask_on_intercept": true,
  "snapshot_eviction_after": 20,
  "never_cache_bash_patterns": ["./check_sim_status", "tail sim.log"]
}
```

`ask_on_intercept=true` is the default. When splice detects a safe duplicate, it
asks before reusing the old result. Set it to `false` if you prefer automatic
deny + context injection. In Codex bypass/full-auto modes, splice may
automatically use deny + context injection because ask decisions can be swallowed
by the host.

## Verify

Manual hook smoke test:

```bash
echo '{"session_id":"test","hook_event_name":"PreToolUse","cwd":".","tool_name":"shell","tool_input":{"command":"ls"}}' | splice codex-pre-tool-use
```

Expected output when no compaction snapshot exists:

```json
{"hookSpecificOutput":{"hookEventName":"PreToolUse"}}
```

Watcher smoke test:

```bash
splice codex-watch --cwd "$(pwd)" --poll-ms 500
```

You should see diagnostics such as:

```text
splice codex-watch: starting in /path/to/project
splice codex-watch: tailing <session-id> -> .../rollout-...jsonl
```

## /clear Behavior

Codex `/clear` triggers SessionStart with `source=clear`. splice treats that as
an explicit state boundary and clears the current session trail. Do not rely on
the host changing `session_id`.

When official `tool_use_id` is present, splice can distinguish old and new tool
calls safely across `/clear`. Without it, hash-only late PostToolUse events are
ambiguous, so splice degrades conservatively instead of claiming an old result as
new.

## Uninstall

```bash
splice uninstall-codex-hooks --project
```

Or:

```bash
splice uninstall-codex-hooks --user
```

Only splice-managed entries are removed. Unrelated Codex config is preserved.

## Known Limits

- `splice codex-watch` is required. Hooks alone are not enough.
- `CODEX_HOME` must point to the same Codex home that contains rollout files if
  you use a non-default Codex home.
- Codex rollout JSONL is not a public stable API. splice handles unknown events
  gracefully, but if Codex changes the compaction marker, watcher-based
  protection may need an update.
- splice cannot see file changes made outside Codex.
- Project-local state under `.splice/` can contain command output and should not
  be committed or shared.
