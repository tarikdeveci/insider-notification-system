# 0003. Rate limiter: GCRA in Redis

Status: Accepted (23 Nov 2026)

## Context

Each channel may send at most 100 messages per second, across all worker replicas. The limit has to be smooth: a burst of 200 at a window boundary breaks the provider contract.

## Decision

Use GCRA (generic cell rate algorithm) in one atomic Redis Lua script per channel, key `ratelimit:{channel}`.

- Emission interval 10ms (100 per second), burst tolerance 0. The script keeps one value, the theoretical arrival time. If it is in the future the call is refused with the wait in microseconds; otherwise it is admitted and the value moves forward by 10ms.
- The script reads the clock with `redis.call('TIME')`. There is one clock for all replicas, so worker clock skew cannot break the limit.
- A permit is taken per send, right before the attempt starts. Retries therefore count against the limit, and empty claims waste no permits.
- The wait is bounded: `RATELIMIT_MAX_WAIT` (default 10s). Past it, the row is released back to `pending` with no attempt used. The same happens on shutdown or when the breaker opens.
- It fails closed: any Redis error means no permit and the row is released. Workers pause sending rather than risk exceeding the provider limit during a flash sale. The worker `/readyz` reports 503 when Redis does not answer.
- Guarantee, stated for admissions: granted slot times are at least 10ms apart, so any half-open one second window `[t, t+1s)` holds at most 100 admissions. The exact bound is tested at admission. Arrivals at the provider can bunch slightly after admission, so the end to end arrival check uses a small tolerance.

## Alternatives considered

- Token bucket with capacity 100 refilled each second: allows about 200 sends in a window that straddles a refill.
- Per replica limits (limit divided by replica count): wrong when replicas are added, removed or unevenly loaded.
- Fail open when Redis is down: keeps delivering but can exceed the provider limit.

## Consequences

Good: a strict, smooth bound shared by all replicas, with one round trip per send and a tested admission guarantee.

Bad: Redis is on the send path, so a Redis outage stops all sending (a deliberate choice). The limit also caps throughput at 100 per second per channel, so each send costs one script call, and a replica waiting for a slot holds a sender slot while it waits.

Known limitation: a single Redis instance, not a replicated one. Next step: Redis with failover (or Sentinel), and a measured decision on batching permit requests if the call rate ever becomes the bottleneck.
