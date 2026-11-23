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
