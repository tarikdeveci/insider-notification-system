# 0001. Postgres as the queue

Status: Accepted (23 Nov 2026)

## Context

Notifications must be accepted over HTTP, held durably, and delivered by workers with priority, scheduled delivery, retry delay and cancellation. Creating a notification and making it deliverable must not be able to disagree (no lost or phantom messages). The case caps each channel at 100 sends per second, so the system tops out at 300 sends per second, far below what a single Postgres primary handles.

## Decision

Postgres is both the source of truth and the queue. A notification is one `INSERT` with `status = 'pending'`; there is no second system to keep in sync.

- Workers claim rows with one statement per priority: `UPDATE ... WHERE id IN (SELECT ... WHERE status = 'pending' AND channel = $1 AND priority = $2 AND next_attempt_at <= now() ORDER BY next_attempt_at, id LIMIT $3 FOR UPDATE SKIP LOCKED) RETURNING ...`. `SKIP LOCKED` lets many workers claim disjoint rows without blocking each other.
- Every claim sets a fresh `lease_token` and `locked_until = now() + 30s`. Every later write by that worker (attempt start, result, release) is a conditional `UPDATE ... WHERE id = $1 AND lease_token = $2 AND status = 'processing'`. Zero rows means the worker lost ownership and drops the row without writing. This fences a stale worker from overwriting a row that the reaper requeued and another worker now holds.
- A reaper in each worker (every `REAPER_INTERVAL`, 5s) moves `processing` rows with an expired lease back to `pending`, or to `failed` when attempts are used up or the delivery age has passed.
- The claim index is partial: `(channel, priority, next_attempt_at) WHERE status = 'pending'`. Probing one priority at a time keeps the query an equality plus range scan, so a large backlog of future scheduled rows is not scanned.

## Alternatives considered

- RabbitMQ or Redis Streams plus Postgres for state: needs an outbox to avoid a DB and broker mismatch, and delayed retry and scheduling need extra machinery (delayed exchanges, dead letter tricks).
- Kafka plus Postgres: matches the scale of a large platform, but has no per message priority or delay, and is heavy to run locally for this workload.

## Consequences

Good: one transactional store, no dual write, no outbox. One query covers priority, retry delay and scheduled delivery. State transitions are single conditional updates, so races (cancel vs claim, two workers) resolve in the database.

Bad: the ceiling is one Postgres primary, and every status change is a write plus index maintenance on a hot table. Polling adds idle load (the claim loop backs off from 200ms to 1s when nothing is claimed).

Known limitation: no horizontal write scaling of the queue.

Next step (scale path): the queue is accessed through the store and the worker's queue interface. At much higher volume, partition by channel, then move delivery to Kafka or Redis Streams fed from Postgres through a transactional outbox, keeping Postgres as the record of state.
