# 0004. At least once delivery and provider idempotency

Status: Accepted (23 Nov 2026)

## Context

A worker can crash after the provider accepted a message but before the row is marked `sent`. The lease then expires, the reaper requeues the row, and another worker sends it again. A network timeout has the same effect: the client cannot know whether the provider received the request.

Exactly once delivery across two systems that share no transaction is not achievable without the provider's cooperation.

## Decision

Delivery is at least once. Exactly once is not claimed.

- Every provider call carries the header `Idempotency-Key: <notification id>`. The id is stable across retries and resends, so the provider can recognize a repeat.
- The mock provider deduplicates on that header and answers a repeat with the original message id and marks it `deduplicated` in its arrival log, to demonstrate the contract.
- A 2xx is final: the row becomes `sent` and is never retried.
- Ownership is fenced by `lease_token` (ADR 0001), so a stale worker cannot overwrite a newer result. This limits duplicate writes to our database, not duplicate sends.
- An attempt with an unknown outcome counts as used (ADR 0002), because the provider may have received it.
- The idempotency key on the public API (`Idempotency-Key` header on create and batch) is a separate mechanism: it makes client retries of `POST` safe and does not affect provider delivery.

## Alternatives considered

- At most once (mark `sent` before calling the provider): a crash loses messages silently, which is worse than a rare duplicate for notifications.
- Two phase or transactional delivery: needs provider support that is not part of the contract.
- A durable "uncertain" state to hold unknown outcomes for manual review: too much machinery for the value.

## Consequences

Good: no message is lost after acceptance. Duplicates are rare (they need a crash or timeout in the window between provider accept and our write) and harmless when the provider honors the key.

Bad: if the real provider ignores `Idempotency-Key`, a recipient can get a duplicate message in that window.

Known limitation: the guarantee depends on the provider's deduplication and on its dedup window. Next step: confirm the real provider's idempotency window and, if it has none, add reconciliation by `provider_message_id`.
