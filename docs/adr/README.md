# Architecture decision records

Short records of the decisions that shape the system. Each states the context, the choice, what else was considered, and what it costs. All were accepted on 23 Nov 2026.

| Number | Title | Status |
|---|---|---|
| [0001](0001-postgres-as-the-queue.md) | Postgres as the queue | Accepted |
| [0002](0002-retry-policy.md) | Retry policy | Accepted |
| [0003](0003-rate-limiter.md) | Rate limiter: GCRA in Redis | Accepted |
| [0004](0004-at-least-once-delivery.md) | At least once delivery and provider idempotency | Accepted |
| [0005](0005-batch-semantics.md) | Batch semantics: all or nothing | Accepted |
| [0006](0006-circuit-breaker.md) | Circuit breaker per worker replica | Accepted |
| [0007](0007-strict-priority.md) | Strict priority | Accepted |
| [0008](0008-separate-api-and-worker.md) | Separate api and worker processes | Accepted |
