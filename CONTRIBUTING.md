# Contributing

splice is intentionally conservative: a false restore is worse than duplicated
work. Changes should preserve that bias.

Before opening a pull request:

```bash
go test ./...
go vet ./...
```

If behavior changes, update the relevant scenario in
`docs/SCENARIO_COVERAGE.md` and add or update an automated test. User-facing
behavior changes should also update `README.md` or the integration guide.

Do not commit `.splice/`, built binaries, coverage files, rollout files, or
local hook configs containing machine-specific paths.
