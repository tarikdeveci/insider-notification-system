# 0008. Separate api and worker processes

Status: Accepted (23 Nov 2026)

## Context

The system has two jobs: accept requests (latency sensitive, mostly I/O to Postgres) and deliver notifications (long running, rate limited, bursty). A simpler local run could embed the worker loop inside the `api` process.

## Decision

`api` and `worker` are separate processes and separate compose services, built from the same image with different commands (`api`, `worker`, plus `migrate` and `mockprovider`). There is no embedded worker in `api`.

- The api never sends to the provider. It only writes to Postgres and reads for queries.
- The worker serves only `/healthz`, `/readyz` and `/metrics` on `WORKER_HTTP_ADDR` (default `:8081`) and is not published to the host.
- Workers scale horizontally with `docker compose up --scale worker=N`. They share nothing in memory: coordination is by `SKIP LOCKED` claims and `lease_token` fencing in Postgres (ADR 0001) and by the Redis rate limiter (ADR 0003).
- The two processes are configured independently. For example, a worker crash or a slow provider cannot affect request handling, and an api restart does not interrupt in flight sends.
- On shutdown the worker stops claiming, releases unstarted rows, and lets in flight sends finish within `SHUTDOWN_TIMEOUT` (default 15s).

## Alternatives considered

- Worker embedded in `api`: fewer containers for a local run, but couples scaling and failure of request handling and delivery, and makes it unclear which replicas should send.
- An optional flag to run both in one process: two modes to test and explain, for a convenience that `docker compose up` already provides.

## Consequences

Good: failure isolation, independent scaling (more workers for throughput, more api replicas for request rate), and a simple mental model.

Bad: one more service to run, and a local setup that has only the api running accepts notifications but never delivers them. Queue depth and delivery metrics are visible only from the worker and the api together.

Known limitation: the per replica circuit breaker (ADR 0006) and per replica concurrency settings mean that scaling workers multiplies provider facing concurrency. The rate limiter keeps the send rate bounded, but not the number of in flight calls. Next step: autoscale workers on queue depth and age of the oldest pending row.
