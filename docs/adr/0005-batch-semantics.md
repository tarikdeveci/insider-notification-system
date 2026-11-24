# 0005. Batch semantics: all or nothing

Status: Accepted (23 Nov 2026)

## Context

`POST /v1/notifications/batch` accepts up to 1000 items. Some items may be invalid. The client needs a clear result and a safe way to retry after a timeout.

## Decision

A batch is atomic.

- If any item fails validation (including template rendering and schedule checks), the response is 422 with per item errors by index, and nothing is inserted.
- Otherwise the batch row and all notifications are inserted in one transaction, and the response is 201 with `batch_id` and the notification ids in request order.
- There is one idempotency key for the whole batch (the `Idempotency-Key` header), stored on `batches` with a hash of the normalized payload. The same key with the same payload replays the stored result (200, same `batch_id` and ids). The same key with a different payload is 409. There are no per item keys, because the batch is atomic and one key covers a safe replay.
- Each notification stores `batch_position`, with a unique `(batch_id, batch_position)`. Replay returns ids ordered by it, so the original request order can always be rebuilt.
- Limits: at most 1000 items, request body at most 8 MiB (413 above that).
- `GET /v1/batches/{id}` returns counts by status, and `POST /v1/batches/{id}/cancel` cancels the pending items.

## Alternatives considered

- Partial acceptance with per item results: friendlier in a flash sale, because one bad recipient does not block the rest. It needs per item idempotency or a result map that clients must reconcile on retry.

## Consequences

Good: simple client logic (fix the errors and resend the same batch, or retry with the same key after a timeout). No half created batches. Replay is exact.

Bad: one invalid item blocks the other 999. A very large insert is one transaction.

Known limitation: no partial success mode. Next step: an opt in partial acceptance mode (for example a query flag) that inserts the valid items, returns per item results, and defines replay for the rejected ones.
