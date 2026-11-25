# Agent log

How AI agents were used to build this repository, one section per checkpoint.

## CP1: foundation, schema, create/get/list/cancel/batch API, e2e (Tasks 1 to 14)

### Roles

| Agent | Role |
|---|---|
| Claude Opus | Controller and planner. Wrote the design and the plan, briefed each task, verified every review finding before applying it, ruled on conflicts, kept the ledger. |
| Claude Sonnet subagents | One implementer per task. Test first, then code, then one conventional commit. Resumed for fix rounds so context carried over. |
| OpenAI Codex CLI (read-only) | Design reviewer (before any code), preflight conflict scanner for the plan, and per-task reviewer (spec compliance and code quality together). |

Limits followed: at most 2 Claude subagents at a time, Sonnet at normal effort, all reviews on Codex. Work stayed on `main` (no worktree), because the commit history is part of the deliverable.

### Per-task loop

1. Brief: the controller writes a task brief from the plan and the binding design spec, plus shared implementer rules.
2. Implement: a Sonnet subagent writes tests and code and makes one feature commit (Opus co-author trailer).
3. Review: Codex reads the diff against the brief and returns a spec verdict (PASS or FAIL) and a quality verdict (APPROVED or CHANGES), with findings graded critical, important or minor, plus "cannot verify" items.
4. Verify: the controller checks every Codex finding and every cannot-verify item against the code before acting.
5. Fix rounds: important findings go back to the same implementer. Each round is its own `fix(<scope>)` commit, never an amend, so the review iteration is visible and the scoped re-review has a stable base.
6. Re-review: Codex re-checks only the fix range. Minor findings are deferred and listed below.

### Task table

| Task | Subject | Commits | Review r0 | Fix rounds |
|---|---|---|---|---|
| 1 | Repo scaffold, testcontainers smoke tests | 2d58fce, 2161da5 | spec PASS, 1 important | 1 (2161da5) |
| 2 | Initial schema migration and migrate runner | 785b8bd | PASS, APPROVED | 0 |
| 3 | Domain types, validation, request hashing | a348af1 | clean | 0 |
| 4 | Env config, slog JSON logging, correlation ids | e024f8f | clean | 0 |
| 5 | Notification create with idempotency, get, attempts | 2a81058 | clean | 0 |
| 6 | Listing with filters and cursor pagination | 7525c52 | clean | 0 |
| 7 | Atomic batch create with replay, batch summary | a7a2dc4 | spec FAIL on commit trailer only, code clean | 0 (message amended, no code change) |
| 8 | Single and batch cancel as conditional updates | 16701b4, c04c812 | 1 important | 1 (c04c812) |
| 9 | HTTP foundation: request id, problem+json, strict decoding, health | d84c070, 5faa4cc | spec FAIL, 2 important | 1 (5faa4cc), third finding overruled |
| 10 | Create, get, list endpoints | 73d6fe7 | PASS, APPROVED | 0 |
| 11 | Batch create, batch summary, cancel endpoints | e535efa | PASS, APPROVED | 0 |
| 12 | api and migrate binaries, Dockerfile, compose stack | 6b5f199 | PASS, APPROVED | 0 |
| 13 | OpenAPI spec with route contract test | c78c10b, bbc86b9 | spec FAIL, 1 important | 1 (bbc86b9) |
| 14 | API-level e2e suite (build tag `e2e`), plus CP1 lint fixes | d83e3d9, a538512, 63addda | spec FAIL, 2 important | 1 (a538512 and 63addda), re-review PASS |

Task 1 starts from the repo root. Every other task starts at the previous task's last commit. All 14 tasks closed with a clean review.

### Review findings that changed code

- Task 1: test DB cleanup failures were only logged, now they fail the test (2161da5).
- Task 8: the plan-mandated cancel race test discarded errors, now it asserts both outcomes (c04c812).
- Task 9: the wrapper dispatch left `PathValue` empty, now path values are kept (5faa4cc).
- Task 9: panics skipped the access log, now a recovered panic is logged once and the request still gets one access record (5faa4cc).
- Task 13: OpenAPI said the email limit was 254 characters, the spec says 254 UTF-8 bytes, now corrected (bbc86b9).
- Task 14: readiness probes and sleeps could exceed the 90 second e2e deadline, now readiness is bounded by the remaining deadline (a538512).
- Task 14: the e2e `call` helper swallowed body read and JSON decode errors, now it fails the test on both (a538512).
- CP1 lint (found by `make lint`, not by Codex): 7 errcheck findings on close and scan calls, now handled (63addda).

### Controller rulings

- Work on `main`, no worktree: the public history is the deliverable and a branch can be cut later.
- Task review goes to Codex, spec compliance and quality in one pass: spec section 4 sends all reviews to Codex, which saves Claude quota and controller context. Cost: the separate Sonnet spec check from the workflow is skipped.
- Review fixes land as separate commits instead of amends: the scoped re-review needs the fix base as an ancestor, and the iteration stays visible.
- Task 7: only the commit message was amended (trailer had the wrong model name), no code change, so no re-review. The shared implementer rules now pin the trailer.
- Task 8: accept the race test finding, the plan text did not require discarding errors and the spec wants races resolved in the database.
- Task 9: the brief's middleware order conflicted with one access record per request, the requirement won. A re-review asked to restore the brief's order (I3), which was overruled: the kept order is RequestID, AccessLog, Recover, mux, it meets the requirement with no extra machinery and the PF1 and PF3 tests pass.
- Task 14: fix the swallowed errors in the e2e helper, it is a test hygiene defect and the stricter test costs nothing.
- Task 14: fold the 7 errcheck fixes into the Task 14 fix round as a separate `fix(lint)` commit, because `make lint` is part of the CP1 gate and Task 14 closes CP1.

Preflight rulings (Codex conflict scan of the plan against the spec, PF1 to PF8):

- PF1 accept: no raw panic or error strings in logs, only a safe error type plus SQLSTATE.
- PF2 reject: keep 0x21 to 0x7E for idempotency keys, because the spec word "printable" is ambiguous and net/http trims whitespace. Cost if wrong: a key with spaces gets 400.
- PF3 accept: 404 and 405 answer as problem+json.
- PF4 accept: e2e HTTP clients are bounded.
- PF5 accept: e2e runs with `-race`.
- PF6 accept: sanitized identifiers when creating and dropping test databases.
- PF7 accept: a test for the 101 row batch cap.
- PF8 accept: single cancel returns 200, 409 or 404, batch cancel returns 200 or 404.

### Deferred minor findings

- Task 2: `migrate.go` discards `m.Close()` errors, join them with the return error.
- Task 3: hash tests check relationships only, add fixed expected digest vectors.
- Task 3: the validation message says "254 characters" but the check counts bytes (found in the Task 13 review).
- Task 4: `config_test.go` has a handwritten `contains` helper, use `strings.Contains`.
- Task 5: attempt retrieval is tested only with zero rows, no populated case and no non-nil slice assertion.
- Task 5: the NULL stored-hash conflict branch is implemented but untested.
- Task 6: the cursor test does not exercise the ID predicate across a timestamp tie (needs three equal-timestamp rows across pages).
- Task 9: implementer wrote tests and code together, no separate red run observed.
- Task 10: the GET test covers only empty attempt history, no nullable fields or duration mapping case.
- Task 11: red step not observed (same as Task 9), and no PF1 marker test for the batch handlers.
- Task 13: `openapi.yaml` says every 4xx and 5xx is problem JSON, but `/readyz` 503 returns plain JSON, state the exception.
- Task 13: the contract test checks the route set and document validity, not response bodies against schemas.
- Task 14: batch summary and cancel calls in the e2e test discard HTTP status codes, assert 200 alongside the payload checks.

### Environment notes

- Docker Desktop's engine API returned 500 at Task 1, fixed with `docker desktop restart`.
- testcontainers on the Windows host needs `MSYS_NO_PATHCONV=1` and `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock`, otherwise Ryuk exits.
- Four testcontainers containers (never started) were left behind after test runs and were removed by hand. Not chased yet, watch in later runs.

### CP1 gate

Gate: passed at commit 63addda on a clean tree. `make test` (with -race): all 7 packages ok. `make lint`: 0 issues. `make e2e` (with -race): ok, stack torn down, no containers left.

## CP2: delivery engine, rate limiting, retries, metrics, engine e2e (Tasks 15 to 22)

Same roles, limits and per-task loop as CP1. Before Task 16, Codex ran a second preflight conflict scan of the CP2 plan against the spec (PF9 to PF16 below).

### Task table

| Task | Subject | Commits | Review r0 | Fix rounds |
|---|---|---|---|---|
| 15 | Provider HTTP client and response classification | 17b56b5 | PASS, APPROVED | 0 |
| 16 | Redis GCRA rate limiter with bounded wait | 76278a4, cb3b2de | spec FAIL, 1 important | 1 (cb3b2de), remainder routed to Task 19 |
| 17 | Retry policy (backoff, jitter, 429 handling) | e66110a | PASS, APPROVED | 0 |
| 18 | Queue operations: claim, start, finish, release, reap, depth | c0ed571 | PASS, 2 minor | 0 |
| 19 | Worker engine, reaper loop, shutdown, worker config | 7245875, 0edeb8d, 8805747 | 2 important | 1 (8805747), plus controller fix 0edeb8d |
| 20 | Configurable mock provider | bf2d021, f62ea0e | spec FAIL, 4 important | 1 (f62ea0e) |
| 21 | Prometheus metrics, worker and mock provider in compose | 2616ba3, 8969ba8 | spec FAIL, 1 important | 1 (8969ba8, by ruling) |
| 22 | Engine e2e suite and claim index plan check (R8) | ee55fed, 3ac38ea | spec FAIL, 2 important | 1 (3ac38ea) |

### Review findings that changed code

- Task 16: max wait was not enforced across retries and Redis calls, now the budget is checked before every script call and overdue grants are rejected (cb3b2de). The rest (go-redis ignores context deadlines on socket I/O unless `ContextTimeoutEnabled` is set) was routed to Task 19, where the worker builds its Redis client.
- Task 19: the default idle backoff was never applied, so an idle worker polled every 200ms, now it grows to 1s (8805747).
- Task 19: `StartAttempt` ran on the drain context, so new attempts could begin after shutdown started, now no new attempt starts once shutdown begins (8805747).
- Task 20: the mock accepted trailing JSON, a missing `content` field and NaN or Inf rates, now all are rejected (f62ea0e).
- Task 20: a scripted 1xx status produced a final 200 (Go writes 200 after a 1xx), now statuses are limited to 200..599 (f62ea0e).
- Task 22: the priority scenario inferred priority from arrival order while several senders ran in parallel, now it runs with one sender per channel (3ac38ea).
- Task 22: the R8 check did not multiply filtered rows by `Actual Loops`, so a repeated scan could pass the 500 row budget, now the whole per-loop sum is multiplied and unexecuted nodes add nothing (3ac38ea).

### Controller rulings

- Task 16: route the remaining max wait point to Task 19. The limiter receives an injected client and cannot set its options; the knob lives where the client is built.
- Task 19: an implementer reported two "pre-existing intermittent" ratelimit failures. The controller traced them to test isolation (a 1h key leaked across `-count` runs) and timing, and fixed the tests (0edeb8d) instead of accepting the label.
- Task 19: a Claim context cancelled at shutdown can leave committed rows in `processing` until the lease expires; the reaper recovers them (PF13). Accepted as designed, documented for the README.
- Task 20: 1xx statuses removed from the mock protocol (PF15 amendment) instead of emulating them.
- Task 21: the review asked for 27 zero `queue_depth` series when the first query fails. Overruled: zeros during a startup database outage read as an empty queue and hide the outage. The series is absent until the first successful refresh (an `absent()` alert can catch it), then all 27 combinations are emitted. The controller added the missing test (8969ba8) and wrote the PF14 amendment.
- Task 21: the implementer changed `TestRateLimiterWaitBoundedThenRelease` outside the brief, calling it flaky. It passed 10 of 10 in isolation on the previous commit, but the stated cause (a cancel landing inside a Claim call, the PF13 path) is real under load and the change is test only, so it was accepted.
- Task 21: the Codex re-review hung (no CPU for over 1.5 hours) and was killed, not retried. The fix range was a single test written by the controller, so the controller re-reviewed it. The review script now has a 15 minute timeout.

Preflight rulings (PF9 to PF16):

- PF9: worker config lives in `internal/config` with defaults and fail fast parsing that names the variable and never echoes URL values. `max_attempts` stays the column default 8 in CP2.
- PF10: `Acquire` blocks until admission with one overall max wait; the worker sends only after it returns nil.
- PF11: the retry policy takes the provider result, post increment attempts, row max and an injected clock.
- PF12: the queue contract is frozen before Task 18; ownership loss is `store.ErrLeaseLost` and means do not send, write nothing.
- PF13: Task 19 owns the reaper and the shutdown order (cancel claims, drain started sends, release unstarted rows on a non cancelled context).
- PF14: observation is injectable with no-op defaults; Prometheus registries are process local; queue depth is collected by the API only with a 5s cache.
- PF15: the mock protocol is frozen before Task 20; e2e resets mock state per scenario; priority is checked with one sender per channel or by claim order.
- PF16: the claim index check uses `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)` of the real claim probe on an analyzed, mostly future backlog, in `tests/integration`.

### Deferred minor findings

- Task 16: the max wait test ceiling (500ms) is loose and covers only immediate rejection.
- Task 16: the Redis down test prints go-redis dial noise; a closed client variant would be quiet.
- Task 18: the claim order test has no identical `next_attempt_at` tie-breaker case.
- Task 18: the `StartAttempt` test does not assert lease renewal of `locked_until`.

### Environment notes

- Docker Desktop failed to start with "removing stale socket ... userAnalyticsOtlpHttp.sock". The file is an AF_UNIX reparse point that `del`, `fsutil` and `Remove-Item` cannot remove. Fix: quit Docker, `wsl --shutdown`, rename `%LOCALAPPDATA%\Docker\run`, start Docker. Needed twice in CP2.
- The Docker engine hung during Task 22 (`docker info` blocked for minutes, testcontainers got 500). A forced restart with the same steps fixed it.
- `core.autocrlf=true` turned Go files into CRLF after stash or checkout and gofmt flagged them; `.gitattributes` now pins LF (0edeb8d).
- The engine e2e phase takes about 4.5 to 5 minutes, mostly retry waits and worker stop and start in two scenarios.

### CP2 gate

Gate: passed at commit 3ac38ea on a clean tree. `make test` (with -race): all 12 packages ok. `make lint`: 0 issues. `make e2e`: API phase ok (4s), engine phase ok (257s), stack torn down, no containers left.

## CP3: CI, scheduled sends, manual retry, circuit breaker, templates, OpenAPI validation (Tasks 23 to 28)

Same roles and per-task loop. Before Task 24, the controller wrote CP3 preflight rulings (PF17 to PF23) so that tasks sharing the request hash, the read model and the route table could not drift apart. On 24 Nov, Tarık asked for more parallelism: Task 28 then ran on `main` while the tier 3 tracing task and two documentation and test jobs ran in separate git worktrees, each reviewed before merge.

### Task table

| Task | Subject | Commits | Review r0 | Fix rounds |
|---|---|---|---|---|
| 23 | GitHub Actions CI (lint, test, e2e) | 1f4785e, 80fb344 | spec FAIL, 2 important | 1 (80fb344), second finding answered with evidence |
| 24 | Scheduled notifications | c10a49b | PASS, 1 minor | 0 |
| 25 | Manual retry endpoint | 86bc0bc | PASS, 1 minor | 0 |
| 26 | Per channel circuit breaker | 1fdd83c, 0fde5ff, 4f01858 | spec FAIL, 1 important | 2 |
| 27 | Templates (migration 0002, rendering, endpoints) | bdd10ce, 16195d5 | spec FAIL, 1 important, 2 minor | 1 |
| 28 | OpenAPI completion, responses validated against the spec | 9c760c9 | PASS, APPROVED | 0 |

### Review findings that changed code

- Task 23: the lint action was one major version behind (`@v8`); checkout, setup-go and golangci-lint-action were moved to their current majors after the controller verified the tags exist (80fb344).
- Task 26: the probe abort test failed on the limiter before the breaker admitted a probe, so it never exercised the abort path. New engine tests admit a probe and then lose the lease, hit an infrastructure error, or shut down, and each proves the probe slot is freed. Removing `defer tk.abort()` makes all three fail (0fde5ff, 4f01858). The first fix round left the shutdown case unproven; the second review caught it.
- Task 27: `"template_vars": {}` without `template_id` passed validation because the check was `len(...) > 0`. Now any non nil map without an id is a 422, with single and batch regression tests written red first (16195d5).

Found by the implementer, not by review:

- Task 26: a Claim could still run after the breaker opened between the gate check and `takeSlots`; the gate is now checked again after slots are taken.
- Task 24: the scheduled e2e test sent a `+` in a query string unescaped, so the lookup missed the row.

### Controller rulings

- Task 23: `TestRateSmoke` failed twice in the first CI run. Diagnosis: host Docker latency (API single insert about 117ms server side, worker about 16/s, provider 9ms, limiter 21ms, in-container Postgres commit 4ms). The test passes alone (225s) and in every full e2e run since. Accepted as an environment property, not a defect; implementers rerun it alone once if only it fails.
- Task 23: the fix round was a three line version bump, so the controller verified it instead of a second Codex run.
- Task 26: the breaker is per replica (spec section 15, ADR 0006). Shared state in Redis is the next step.
- Task 27: the implementer reported partial TDD evidence (renderer tests written together with the code). Recorded as a process deviation; the fix round had to show red output first.
- Tier 3 decision (Tarık, 24 Nov, at the CP3 gate): tracing only, WebSocket dropped. Reasons: the safe version needs connection caps, an origin check, bounded per client buffers and write deadlines (review item S8), and the remaining time is worth more in load testing, a fresh clone test and the README.

Preflight rulings (PF17 to PF23):

- PF17: the request hash stays backward compatible: new fields are `omitempty`, `scheduled_at` hashes as UTC RFC3339Nano, `template_vars` as a JSON object; the hash never covers rendered content.
- PF18: field format errors are 422; the schedule window check runs only for new inserts with an injectable clock, and a failing check with an Idempotency-Key first tries replay (same hash 200, other hash 409, no row 422).
- PF19: the read model gains `scheduled_at`, `template_id` and `template_vars`, always present and null when unset.
- PF20: manual retry is one conditional UPDATE from `failed` to `pending` with attempts reset; `last_error` and `scheduled_at` are kept.
- PF21: breaker semantics: only Retryable counts, Success and Permanent reset, Throttled does neither; half open admits exactly one probe; open releases claimed unstarted rows without using an attempt.
- PF22: unknown template and channel mismatch are 422 on `template_id`; a duplicate template name is 409; the template list is bounded.
- PF23: a task that adds a route changes `routes()` and `api/openapi.yaml` in the same commit.

### Deferred minor findings

- Task 24: the batch 409 replay test changed the batch so it no longer reached `batchScheduleRejected`. Fixed after the gate in a separate test commit.
- Task 25: the concurrent reaper subtest used a lease in the future, so lease expiry alone excluded the row. Fixed after the gate in the same test commit.
- Task 27: the report summarizes passing gates without pasting their output.
- Task 28: the 405 and unmatched route 404 problem responses are not spec paths, so they are not sampled.

### Environment notes

- Docker Desktop hung three more times (Task 24 e2e, Task 26 `make test`, start of 24 Nov). The same recovery worked each time: quit Docker, `wsl --shutdown`, rename `%LOCALAPPDATA%\Docker\run`, start Docker.
- A background implementer was lost when its session ended overnight; its fix round had not been written, so a new implementer redid it from the review file.
- One `internal/api` run hit a transient testcontainers reaper error ("container status removing"); the rerun passed.

### CP3 gate

Gate: passed at commit 9c760c9 on a clean tree, 24 Nov morning, well before the 20:00 tier 3 deadline. `make test` (with -race): all 13 packages ok. `make lint`: 0 issues. `make e2e`: API phase ok (21s), engine phase ok (279s, `TestRateSmoke` included), stack torn down.

## CP4: tracing, load test, ADRs, final checks (Task 29 and the final phase)

Tarık asked for real parallelism on 24 Nov, so independent work ran in separate git worktrees (`ns-tracing`, `ns-minors`, `ns-docs`, `ns-load`), each on its own branch, and the controller reviewed every branch before a `--no-ff` merge into `main`. Sonnet implementers did the writing; Codex did the read only reviews.

### Task table

| Task | Subject | Commits | Review r0 | Fix rounds |
|---|---|---|---|---|
| 29 | OpenTelemetry tracing across the queue (API, stored trace context, worker, provider `traceparent`, Jaeger profile) | 11e271b, 9f85af7 | spec FAIL, 3 important, 1 minor | 1 |
| Final | Deferred CP3 minors | 0a5bb2d | controller checked by mutation | 0 |
| Final | ADRs 0001 to 0008 | 72dbb16 | controller spot checked numbers against code | 0 |
| Final | k6 burst load test, `make load` | e353ab0, 552a4bd | controller read the diff | 1 (pinned the k6 image) |

### Review findings that changed code

- Task 29 I1: an `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` set on its own still created an exporter. Tracing is now off unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set, with a regression test for the traces only variable.
- Task 29 I2: a row with an empty or malformed stored context, and every claim, inherited whatever span the caller had. The stored parent is now extracted into a context with the ambient span cleared, and claims start with `trace.WithNewRoot()`.
- Task 29 I3: the compose profile test skipped on a broken compose file. It now skips only when Docker or Compose is missing and fails on any config error.
- Task 29 M1: a test now receives the real provider HTTP request and checks that `traceparent` names the active provider span.
- Found by the implementer: idle claim polls produced one empty span every tick; spans are now only recorded for claims that return rows.
- Deferred minors: the batch 409 test now also sends a different body that fails the schedule check (mutating the conflict branch to 422 makes it fail), and the reaper subtest now expires the lease so only the status guard protects the row.

Found by the load test agent, not by any review:

- The host had a user level `DATABASE_URL` pointing at a hosted Postgres for another project, and `docker-compose.yml` used `${DATABASE_URL:-...}`, so the compose stack silently used that remote database instead of its own container. Every `make up` and `make e2e` since 23 Nov most likely did the same. The compose file now pins `DATABASE_URL` and `REDIS_URL` to its own containers, guarded by `TestComposeIgnoresHostDatabaseAndRedisURLs` (written red first), and `.env.example` says so (c24626c). The remote data was left untouched pending Tarık's decision.

### Controller rulings

- Correction to the CP3 ruling on `TestRateSmoke`: the diagnosis "host Docker latency" was wrong. The 117ms server side insert and the slow worker were the round trip to the remote database above, not Docker. No review caught it because every reviewer read the code, and the code was right; only the environment was wrong.
- Task 29 r1 returned `cannot_verify` for missing test output; the controller ran `make test` and `make lint` in the worktree instead of a second review.
- The ADRs follow the code where the spec drifted: the worker `/readyz` returns 503 when not ready, a permanent result resets the breaker count and a 429 is neutral, `MAX_DELIVERY_AGE` is checked before the attempt budget, and a 3xx is permanent.
- `make e2e` and `make load` both own ports 8080 and 9090, so they never ran at the same time.

### Environment notes

- Codex `workspace-write` jobs (the minors and the ADRs) hung for 15 to 20 minutes without writing a file on this Windows host. They were stopped; the controller did the minors and a sonnet agent wrote the ADRs. Read only Codex reviews kept working.

### CP4 gate

Gate: passed on `main` at 552a4bd after all four branches were merged. `make test` (with -race): all packages ok, 22s with a warm build cache. `make lint`: 0 issues. `make e2e`: API phase ok (1.6s, was 21s at CP3), engine phase ok (44.7s, was 279s), 86s in total including build and teardown. The speedup is the database fix above, not a code change. Tracing checked live with `make up-tracing`: one sms produced a single trace in Jaeger with `POST /v1/notifications` and `db.insert_notification` from the API and `deliver` and `provider.send` from the worker.

## Red team: independent adversarial review before submission (24 Nov 2026)

An Opus red team agent with no knowledge of how the code was built reviewed the whole repository. It reported 15 findings (5 medium, 8 low, 2 nits that bundled 4 items). The controller verified findings 1, 2, 3, 4 and 13 in the code before any fix. Two sonnet agents fixed them on separate worktrees (worker and store, API and provider), one commit per finding, test first.

Fixed:

- Redis outage: the worker released and reclaimed rows in a hot loop and logged one warning per row. Now the loop backs off, released rows wait out the pause, one warning per outage.
- `MAX_DELIVERY_AGE` was only checked when an attempt ended, so a row that waited 25 hours was still sent once. The claim now fails it as `expired` on the database clock.
- Template rendering was unbounded before the length check. The rendered size is computed first, and a body holds at most 100 placeholders.
- The API `/readyz` depended on Redis, which the API never uses. It checks Postgres only, and the dead `Deps.Redis` field is gone.
- Error logs always said `err_type=*fmt.wrapError`. A data free `err_class` is logged next to it.
- A NUL byte was a 500 from Postgres. Control characters are now a 422.
- Attempt history had no stable order after a 429 refund and the reaper wrote no entry. Migration 0003 adds a write order and the reaper records `lease_expired`.
- Retry times mixed the worker and database clocks. The policy returns a delay and the store adds it to `now()`.
- The provider client used the default transport (2 idle connections per host). It has its own, sized for every sender.
- Compose let Docker kill the worker after 10s while it drains for 15s, and `RATELIMIT_MAX_WAIT` was not checked against the 30s lease. Both fixed, both tested.
- A failing claim probe left rows claimed by earlier probes in `processing`. They are returned now.
- 408 was permanent; 408 and 425 are retryable.
- Nits: the API logged `correlation_id` twice, the limiter wait metric counted errors, `GetBatch` read counts and the batch in two snapshots.

Found by the controller while documenting the fixes, not by the red team: moving the age check to claim made `POST /retry` useless for a row past the age, because the next claim failed it again with no attempt. Migration 0004 adds `requeued_at` and a manual retry now starts a fresh window (12704ac).

Left on purpose, with reasons in the README: zero burst GCRA (finding 7), the trusted client `traceparent` (finding 14) and the 160 character SMS limit against UCS 2 segments.

Environment: Docker Desktop crashed twice during the fixes (the engine pipe vanished, then a 500 from the daemon). Killing the Docker processes, `wsl --shutdown` and moving the stale `run` folder aside brought it back both times. The worker agent stalled after the second crash; the controller finished its last findings in the same worktree.

Gate: passed on `main` at 12704ac. `make test` (with -race): all packages ok. `make lint`: 0 issues. `make e2e`: API phase 1.4s, engine phase 44.1s. `make load`: 10,000 sent, 0 failed, 93.8 per second per channel, busiest 1 second window 101 to 102 arrivals, PASS.

## Codex second opinion: hardcore read only review (25 Nov 2026)

OpenAI Codex (read only, high reasoning) reviewed the repository against the case requirements after the red team fixes. It reported 10 findings (3 high, 5 medium, 2 low). The controller checked each one in the code; six were real defects and were fixed test first in one commit:

- The reaper cast elapsed milliseconds to `int`, so one row stuck in `processing` for about 25 days failed the whole reaper statement on every pass and no lease was ever reclaimed again. `duration_ms` is now clamped before the cast, in the reaper and in `FinishAttempt`.
- A provider `messageId` with a NUL byte made Postgres reject the `sent` write, and the reaper sent the accepted message again until it was exhausted. An id with a control character or over 256 bytes is now dropped and counted as unparsed.
- The age was checked at claim time only, before the rate limiter wait, so a row could be sent up to `RATELIMIT_MAX_WAIT` past `MAX_DELIVERY_AGE`; and the decision after an attempt compared database timestamps with the worker clock. The attempt start now rechecks the age on the database clock (an aged out row fails unsent), and the retry decision uses the database time from `StartAttempt` plus elapsed worker time.
- The provider client drained a response body with no bound after its 64 KiB read, so an endless body held a sender until the 5s timeout. The drain is bounded now.
- A template could use more distinct placeholders than the 50 `template_vars` a request may carry, so it was created but could never render. It is a 422 now.
- The OpenAPI text for `POST /retry` still described the age window from before migration 0004.

Not changed in code, documented in the README instead: the permit is taken before the attempt start, so a database stall can bunch admitted sends on the wire (finding 2); a worker frozen past its lease can still send a row that was cancelled after the reaper requeued it, which fencing cannot stop at the provider (finding 3); `Retry-After` is clamped to 60s on 429 on purpose and ignored on 503 (finding 7).

Gate: `make test` (with -race): all packages ok, 300 test functions. `make lint`: 0 issues. `make e2e`: API phase 1.5s, engine phase 42.6s.
