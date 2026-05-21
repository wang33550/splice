# Security Policy

splice runs locally and does not send telemetry or call external services.

Project-local data under `.splice/` can contain command arguments, command
output, tool results, and session identifiers. Treat it as sensitive. Do not
upload raw databases or logs to public issues unless they are sanitized.

For security-sensitive reports, use GitHub private vulnerability reporting if it
is enabled for the repository. If it is not enabled, open a minimal public issue
that describes the affected area without including sensitive values or raw
`.splice` data.
