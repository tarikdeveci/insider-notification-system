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
