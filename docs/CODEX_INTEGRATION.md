# Codex Integration

Codex integration has one extra moving part: Codex exposes PreToolUse,
PostToolUse, and SessionStart hooks, but not a PreCompact hook. splice therefore
uses `splice codex-watch` to watch Codex rollout JSONL files and detect
compaction boundaries.

## Requirements

- Codex with hooks enabled.
- `splice` available on `PATH`, or a local splice binary that will not be moved
  after installation.
- Hooks installed user-wide for desktop use, or project-local for a deliberately
  scoped CLI/project setup. A single global watcher is auto-started by the Codex
  SessionStart hook.

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

User-wide installation is recommended for Codex desktop:

```bash
splice install-codex-hooks --user
```

This writes `$CODEX_HOME/config.toml`, or `~/.codex/config.toml` when
`CODEX_HOME` is not set. Desktop users should prefer this mode because the app
can enter an existing project, create a new project, create a new conversation,
or use a projectless chat before there is any project directory where a
project-local splice command could have run.

Project-local installation is also supported when you intentionally want splice
only in one project:

```bash
cd /path/to/your/project
splice install-codex-hooks --project
```

This writes `<cwd>/.codex/config.toml`.

The installer is idempotent. It preserves unrelated config and rewrites only
entries marked with `description = "splice-managed"`.

## Start The Watcher

In normal use you do not start the watcher yourself. `splice install-codex-hooks`
registers `splice codex-session-start`; every Codex SessionStart event writes a
global active-session marker and best-effort starts one background
`splice codex-watch` process.

You do not need two terminals. In Codex desktop, open projects and conversations
normally; the SessionStart hook is the front door that performs splice's
background setup.

This is designed for Codex desktop:

- Opening an existing project is covered when the session starts.
- Creating a new project is covered when that new session starts.
- Creating a new conversation inside the same project is covered as a separate
  session.
- Switching between project directories in the same desktop app is covered
  because state is keyed by `session_id`, not by terminal cwd.
- Projectless conversations are covered in a projectless session bucket.

Command identity is still scoped by project `cwd`. A single desktop
conversation can carry memory across project switches, but a repeated `npm test`
in project B will not reuse the result of `npm test` from project A.

The same model covers Codex CLI:

- One terminal running one Codex conversation uses one `session_id` and one
  session DB.
- Multiple terminals can run multiple Codex conversations at the same time,
  including in the same project directory. They share the single global watcher,
  but each terminal's conversation stays isolated by `session_id`.

Manual watcher start is optional and mainly useful for debugging or recovery:

```bash
splice codex-watch
```

The manual watcher is global. `--cwd` is accepted for old scripts but ignored.
The auto-started watcher writes logs to `~/.splice/codex-watch.log`.

Only one watcher should run per user account. If a second watcher starts while
one is alive, the global lock prevents duplicate rollout ingestion.

The watcher:

1. Reads global active session markers written by the SessionStart hook.
2. Locates the matching rollout JSONL under `$CODEX_HOME/sessions/...` or
   `~/.codex/sessions/...`.
3. Replays existing events and tails new events.
4. Freezes the trail when it sees a context-compaction event.

If the watcher stops, new compactions are not protected until a later
SessionStart starts it again or you run `splice codex-watch` manually. When
restarted, it replays rollout files and rebuilds state as long as the rollout
files still exist.

The watcher is also defensive about normal file lifecycle edges:

- If a rollout file is truncated or replaced, splice drops that session's
  in-memory pending call map and replays the current file from byte 0.
- If a watched rollout temporarily disappears and later comes back, the global
  refresh loop restarts that session's tail.
- If multiple rollout files match a session id, splice chooses the newest
  matching file.
- Fresh markers without a rollout are kept because Codex may create the file
  shortly after SessionStart. Very old orphan markers with no rollout file are
  removed automatically.

## Configuration

Project-local config:

```text
<project>/.splice/config.json
```

Global config:

```text
~/.splice/config.json
```

Config is loaded as global defaults first, then project-local values override
those defaults when Codex provides a project `cwd`. Projectless Codex desktop
conversations use the global config only.

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

For Codex desktop users, put broad defaults in `~/.splice/config.json`. Put
project-specific live/status rules in `<project>/.splice/config.json` when that
project has commands whose results should always be re-queried after compaction.

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
splice codex-watch --poll-ms 500
```

You should see diagnostics such as:

```text
splice codex-watch: starting globally in /home/me/.splice
splice codex-watch: tailing <session-id> [<project-key>] -> .../rollout-...jsonl
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
splice uninstall-codex-hooks --user
```

Or:

```bash
splice uninstall-codex-hooks --project
```

Only splice-managed entries are removed. Unrelated Codex config is preserved.

## Known Limits

- The global watcher is required. Hooks alone are not enough, but the Codex
  SessionStart hook starts the watcher automatically.
- `CODEX_HOME` must point to the same Codex home that contains rollout files if
  you use a non-default Codex home.
- Codex rollout JSONL is not a public stable API. splice handles unknown events
  gracefully, but if Codex changes the compaction marker, watcher-based
  protection may need an update.
- splice cannot see file changes made outside Codex.
- Runtime state under `~/.splice/` can contain command output and should not be
  committed or shared.
