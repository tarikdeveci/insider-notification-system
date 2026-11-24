# 0002. Retry policy

Status: Accepted (23 Nov 2026)

## Context

The provider can fail, time out, throttle or reject. Some outcomes are worth retrying and some are not, and retries must not hammer a struggling provider or loop forever.

## Decision

Every provider call is classified into one of four classes:

| Response | Class | Action |
|---|---|---|
| any 2xx (expected 202) | success | `sent`. Never retried, because the provider accepted it. |
| 429 | throttled | Back to `pending`; honors `Retry-After` clamped to 1s..60s, else backoff. Does not consume an attempt. |
| 5xx, timeout (client timeout 5s), connection error | retryable | Backoff, consumes the attempt. |
| other 4xx, and any 3xx (redirects are disabled) | permanent | `failed`. |

- Backoff is full jitter exponential: `delay = random(0, min(cap, base * 2^(attempt-1)))`, with `RETRY_BASE_DELAY` 2s and `RETRY_MAX_DELAY` 10m.
- `max_attempts` is 8 (column default). Only real send attempts count. `attempts` is incremented right before the HTTP call, not at claim. 8 attempts have 7 waits, at most 2+4+8+16+32+64+128 = 254 seconds in total (about 4.2 minutes worst case, about 2.1 minutes on average), so a notification survives provider outages of a few minutes.
- A 429 refunds its attempt in the same statement that requeues the row. Circuit breaker pauses and rate limiter waits do not consume attempts either.
- Because 429s are free, a second bound stops endless retry traffic: `MAX_DELIVERY_AGE` (default 24h), measured from `COALESCE(scheduled_at, created_at)`. A retry, 429 requeue or reaper transition past that age ends in `failed` with reason `expired`.
- An attempt whose outcome is unknown (the worker died after the increment and before the result) counts as used, because the provider may have received it. The reaper fails the row with reason `exhausted` if no attempts are left.
- The decision logic is a pure function (`internal/worker/retry.go`) with injected clock and jitter source, so it is unit tested without I/O.

## Alternatives considered

- Fixed or non jittered backoff: retries from many notifications synchronize and hit the provider in waves.
- Counting 429 as an attempt: a throttling provider would push healthy notifications to `failed` through no fault of their own.
- A durable "uncertain attempt" state for crashes: more machinery than the benefit justifies, since resends are safe under provider idempotency (ADR 0004).

## Consequences

Good: transient failures heal themselves, permanent ones fail fast, and every path is bounded by attempts or delivery age.

Bad: a notification that fails for longer than the 8 attempt budget lands in `failed` even if the provider recovers minutes later. A crash after the increment can spend an attempt that never reached the provider.

Known limitation: no automatic redrive of `failed` rows. Next step: use the manual retry endpoint (`POST /v1/notifications/{id}/retry`) or add a scheduled redrive for rows that failed during a provider outage.
