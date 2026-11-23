# Project rules

Go notification system: an HTTP API accepts notifications, a Postgres-backed queue holds them, workers deliver them to an external provider, and a Redis rate limiter paces delivery.

- TDD: write the test first and watch it fail before implementing.
- Tests use the stdlib `testing` package only.
- `make test` must pass before every commit.
- One conventional commit per task.
- Layout: `cmd/`, `internal/domain|store|api|worker|ratelimit|provider|observability`, `migrations/`, `api/openapi.yaml`, `tests/`.
- Never log recipient, content, template vars, provider URL or response bodies.
- No secrets in the repo.
- No long or medium dashes in code, comments or docs.
- No junk files (summaries, scratch scripts, temp output).
- Migrations are append-only and versioned.
- All state transitions are single conditional `UPDATE ... WHERE status = <expected>` statements.
- SQL uses bound parameters only.
