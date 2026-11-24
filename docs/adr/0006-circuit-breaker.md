# 0006. Circuit breaker per worker replica

Status: Accepted (23 Nov 2026)

## Context

When the provider is down, every send times out after 5s and fails. Continuing to claim and send wastes attempts, fills the retry queue and delays recovery.

## Decision

Each worker keeps one in process circuit breaker per channel.

- Closed: normal. Only retryable provider results (5xx, timeout, connection error) count as failures. A success or a permanent result (the provider answered) resets the count. A 429 neither counts nor resets.
- Open: after `BREAKER_THRESHOLD` consecutive failures (default 10), the channel stops claiming for `BREAKER_OPEN_FOR` (default 30s). Rows already claimed are released with no attempt used.
- Half open: after the open period, exactly one probe send is allowed. A success or permanent result closes the breaker. A retryable result reopens it for another 30s. A 429 frees the probe slot and stays half open.
- The breaker gates claiming and sending only. Breaker pauses never consume attempts (ADR 0002).
- State is exposed as the `circuit_breaker_state` gauge.

## Alternatives considered

- Shared breaker state in Redis: all replicas open and close together, and the provider sees one probe instead of one per replica. It adds a Redis dependency to the failure path and a second source of state to reason about.
- No breaker, only backoff: each row retries independently, so a provider outage still receives a steady stream of doomed calls.

## Consequences

Good: no coordination, no extra dependency, trivial to test with an injected clock. A down provider is probed at a low rate.

Bad: with N replicas the provider can see up to N probes per open period, and replicas open at different times because each counts only its own failures.

Known limitation: the breaker is per process, so a restart resets it. Next step: shared breaker state in Redis (failure count and open until timestamp per channel), with a single elected probe.
