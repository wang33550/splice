# splice

`splice` is a local safety net for Claude Code and Codex sessions that lose a
small piece of recent memory after context compaction.

It solves one narrow problem: after a model has just run a tool and received a
result, automatic compaction can make the model try to run the same tool again.
splice records the pre-compaction causal trail through official hooks. If the
post-compaction request is a safe duplicate, splice can restore the prior tool
result instead of re-running it.

Current release: `v0.5.2`.

## Status

splice is ready for real project trials, but should be treated as beta software:

- Claude Code support uses official PreToolUse / PostToolUse / PreCompact /
  SessionStart hooks.
- Codex support uses official PreToolUse / PostToolUse / SessionStart hooks.
  Codex SessionStart auto-starts one global `splice codex-watch` process to
  detect compaction from Codex rollout files.
- All decisions are local and deterministic. splice does not call an LLM, send
  telemetry, or upload data.
- Session data is stored under `~/.splice/` and may include command
  arguments and tool output. Do not commit or share that directory.

## What It Does

Example failure mode:

```text
T1: agent runs `npm test` and gets "12 passed"
T2: automatic context compaction happens
T3: agent wakes up and tries to run `npm test` again
```

splice can intercept `T3` and show or inject the pre-compaction result:

```text
splice: detected a duplicate tool call after context compaction
command: npm test
previous result: exit 0, 12 passed
```

It is not a general command cache, memory system, summarizer, or semantic
retriever. It only acts around compaction boundaries.

## Install

From source:

```bash
go install github.com/wang33550/splice/cmd/splice@v0.5.2
splice version
```

Or clone and build:

```bash
git clone https://github.com/wang33550/splice.git
cd splice
go build -o splice ./cmd/splice
./splice version
```

On Windows the binary name may be `splice.exe`. The commands below assume the
binary is available on `PATH` as `splice`; if it is not, run the local binary
directly or let the installer write its absolute path into the hook config.

## Quickstart: Claude Code

Install hooks. Project-local is usually best when you only want splice in one
repository:

```bash
cd /path/to/your/project
splice install-claude-hooks --local
```

Restart Claude Code so it reloads settings.

Uninstall:

```bash
splice uninstall-claude-hooks --local
```

Detailed guide: [docs/CLAUDE_INTEGRATION.md](docs/CLAUDE_INTEGRATION.md).

## Quickstart: Codex

Install user-wide hooks. This is the recommended Codex desktop setup because
desktop users can open existing projects, create new projects, create new
conversations, and use projectless chats without first running a command inside
each project directory:

```bash
splice install-codex-hooks --user
```

After installation, Codex SessionStart auto-starts one background global watcher
when Codex opens any session. This covers Codex desktop users who open an
existing project, create a new project, create a new conversation, switch
between project directories, or use a projectless conversation. It also covers
CLI users who run one Codex session in one terminal or multiple Codex sessions
in multiple terminals; each conversation is isolated by `session_id`, even when
the terminals share the same project directory.

Within a session, duplicate command identity is also scoped by project `cwd`.
If a desktop conversation switches from project A to project B, `npm test` in B
will not reuse the result of `npm test` from A.

Manual watcher start is optional and mainly for debugging:

```bash
splice codex-watch
```

The manual watcher is global too; it is not tied to the shell's current
directory. Logs from the auto-started watcher go to `~/.splice/codex-watch.log`.

Uninstall:

```bash
splice uninstall-codex-hooks --user
```

Detailed guide: [docs/CODEX_INTEGRATION.md](docs/CODEX_INTEGRATION.md).

## Configuration

Project config: `<cwd>/.splice/config.json`

Global config: `~/.splice/config.json`

Config is loaded as global defaults first, then project config overrides those
defaults when the host provides a `cwd`. Projectless Codex desktop chats use the
global config only.

```json
{
  "ask_on_intercept": true,
  "snapshot_eviction_after": 20,
  "never_cache_bash_patterns": ["./check_sim_status", "tail sim.log"],
  "force_cache_bash_patterns": ["./run_eval --suite stable"],
  "force_fence_bash_patterns": ["pytest --update-snapshots"]
}
```

- `ask_on_intercept`: default `true`. Ask the host/user before reusing a
  duplicate result. In bypass modes, splice automatically degrades to deny +
  context injection because the host may swallow ask decisions.
- `snapshot_eviction_after`: default `20`. Drop a frozen snapshot after N
  consecutive post-compaction tool calls do not hit it. Set `0` to disable.
- `never_cache_bash_patterns`: project-specific live/status commands that
  should always re-run. Known dangerous commands still fence even if matched.
- `force_cache_bash_patterns`: project-specific stable commands that splice
  should treat as reusable even if the built-in classifier does not know them.
- `force_fence_bash_patterns`: commands that should always invalidate older
  cached facts and should never be restored as cached results.

Pattern priority is safety-first: known dangerous Bash syntax and mutating
prefixes win, then `force_fence_bash_patterns`, then
`never_cache_bash_patterns`, then `force_cache_bash_patterns`, then the built-in
classifier. Use `never_cache_bash_patterns` for live status files/logs and
`force_fence_bash_patterns` for commands that look like tests but rewrite
snapshots, generated files, databases, or remote state.

## Safety Model

splice is conservative by design:

- It restores only successful terminal results.
- It does not restore failed, interrupted, or timed-out results.
- It allows re-run when Edit/Write or side-effect Bash happened after the prior
  result.
- It allows live/status queries to run again.
- It treats false reuse as worse than duplicated work.
- It restores the repeated call plus the later observed pre-compaction tool
  trail, not just the single repeated command.

Scenario coverage is tracked in
[docs/SCENARIO_COVERAGE.md](docs/SCENARIO_COVERAGE.md).

## Known Limits

- splice cannot see file changes made outside the host agent, such as edits from
  an IDE, another terminal, or `git pull`. Keep `ask_on_intercept=true` if that
  happens in your workflow.
- Codex protection requires the global watcher because Codex does not expose a
  PreCompact hook. The Codex SessionStart hook starts it automatically; if it is
  stopped, run `splice codex-watch` manually.
- Codex rollout parsing depends on private rollout JSONL shape and may need
  updates if Codex changes it. The watcher handles normal truncation,
  replacement, restart, and stale-marker cleanup defensively.
- Trail state is scoped to the current session and the latest compaction window.
- `~/.splice/` can contain sensitive tool output and should stay local.

## Development

Maintainer and local continuous-development notes live in
[docs/dev/DEVELOPMENT.md](docs/dev/DEVELOPMENT.md).

## License

MIT
