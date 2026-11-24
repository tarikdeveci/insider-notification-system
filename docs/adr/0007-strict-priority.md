# 0007. Strict priority

Status: Accepted (23 Nov 2026)

## Context

Notifications have three priorities: high (0), normal (1), low (2). A high priority message such as a one time password should not wait behind a marketing campaign of low priority messages.

## Decision

Priority is strict. The claim runs one probe per priority:

1. Claim up to `n` high priority rows that are due (`next_attempt_at <= now()`).
2. If fewer than `n` came back, claim the rest from normal, then from low.
3. The claimed rows are sorted in Go by `(priority, next_attempt_at, id)` before dispatch, because `UPDATE ... RETURNING` has no defined order.

Each probe is an equality on channel and priority plus a range on `next_attempt_at`, served by the partial claim index `(channel, priority, next_attempt_at) WHERE status = 'pending'`. This also keeps a large backlog of future scheduled rows out of the scan. A worker claims only as many rows as it has free sender slots, so a claimed row starts its send almost at once.

## Alternatives considered

- A single query ordered by `priority, next_attempt_at`: needs a sort across priorities and loses the clean index probe.
- Weighted fair share or aging from the start: protects low priority, but adds a policy to design and test, and is not needed by the requirements.

## Consequences

Good: high priority work is always claimed first (rows claimed together still go to concurrent senders, so they can finish out of order), the behavior is easy to explain and test, and each probe is a cheap index range scan.

Bad: under sustained high priority load, low priority rows can starve, because they are only claimed when higher priorities are empty. Since the channel is capped at 100 sends per second, a high priority stream at that rate blocks everything below it.

Known limitation: starvation of low priority work is possible and documented. Next step: aging, where the effective priority of a row improves with the time it has waited (for example one extra claim of low priority per N high priority claims, or promoting rows older than a threshold), plus a metric for the age of the oldest pending row per priority.
