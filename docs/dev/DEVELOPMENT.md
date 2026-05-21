# Local Development

This document is for maintainers and for local continuous development. The root
README is the user-facing release document.

## Development Contract

splice is intentionally narrow. It repairs a specific post-compaction failure
mode where the model repeats a tool call because it lost the recent result.

Core rules:

- False reuse is worse than duplicated work.
- Restore only successful terminal results.
- Restore the repeated tool call plus all later observed pre-compaction tool
  calls/results, not only the single repeated command.
- Treat side effects after the candidate result as a fence.
- Let live/status queries run again.
- Decide locally and deterministically; never call an LLM.

The functional matrix lives in
[../SCENARIO_COVERAGE.md](../SCENARIO_COVERAGE.md).

## Local Loop

```bash
go test ./...
go vet ./...
go test ./... -coverprofile=coverage.out
go tool cover -func=coverage.out
```

Build the local binary:

```bash
go build -o splice ./cmd/splice
./splice version
```

Windows PowerShell:

```powershell
go build -o splice.exe ./cmd/splice
.\splice.exe version
```

## Cross-Platform Builds

The runtime is mostly platform-neutral Go. The only intentional OS split is
Codex watcher process-liveness detection:

- `internal/codex/process_windows.go`
- `internal/codex/process_unix.go`

Local cross-build smoke test from PowerShell:

```powershell
$env:CGO_ENABLED = "0"
$env:GOOS = "linux";   $env:GOARCH = "amd64"; go build -trimpath -o dist/splice-linux-amd64 ./cmd/splice
$env:GOOS = "darwin";  $env:GOARCH = "arm64"; go build -trimpath -o dist/splice-darwin-arm64 ./cmd/splice
$env:GOOS = "windows"; $env:GOARCH = "amd64"; go build -trimpath -o dist/splice-windows-amd64.exe ./cmd/splice
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
```

The GitHub release workflow builds the public release matrix.

## Release Checklist

Before tagging a release:

1. Update `cmd/splice/main.go` version.
2. Update `CHANGELOG.md`.
3. Remove local-machine paths and private notes from user-facing docs:
   scan the user-facing docs for local absolute paths, private notes, and
   machine-specific examples before tagging.
4. Run `go test ./...`.
5. Run `go vet ./...`.
6. Run cross-platform build smoke tests.
7. Install hooks in a disposable project and verify:
   `splice install-claude-hooks --local`.
8. For Codex, verify hooks plus watcher:
   `splice install-codex-hooks --project` and `splice codex-watch`.
9. Confirm `.splice/`, databases, coverage files, and built artifacts are not
   staged.
10. Tag as `vX.Y.Z` and let the release workflow publish artifacts.

## Manual Scenario Checks

The automated tests cover the scenario matrix, but before a public release it is
worth running these manually:

- Claude Code stable duplicate after compaction: run a stable command, compact,
  repeat, and confirm ask/deny behavior.
- Claude Code side-effect fence: run a command, edit a file through the agent,
  compact, repeat, and confirm splice allows re-run.
- Codex watcher recovery: start `splice codex-watch`, run a session through
  compaction, and confirm the watcher freezes the trail.
- `/clear`: trigger clear, repeat an old command, and confirm old results are
  not restored.
- Long task: start a task that crosses compaction and confirm splice emits
  in-flight context instead of pretending it completed.

## Repository Map

```text
cmd/splice/                 CLI entry point and hook runtime
internal/hook/              Claude/Codex hook input and output schema
internal/fingerprint/       canonical tool args and hashes
internal/classify/          Bash read-only / side-effect / volatile rules
internal/store/             per-session SQLite trail store
internal/inject/            ask/deny/in-flight context templates
internal/config/            .splice/config.json loader
internal/install/           Claude settings.json installer
internal/codex/             Codex rollout parser and watcher
internal/codexinstall/      Codex config.toml installer
docs/SCENARIO_COVERAGE.md   functional coverage matrix
docs/dev/archive/           historical bug/debug reports
```

## Data And Privacy

Local state under `.splice/` can contain command arguments, command output,
tool results, and session identifiers. Do not attach raw databases to public
issues unless they are sanitized.
