# backend/internal/scopeamendment

Mid-stage scope amendments (#961, E22.X): the operator-gated escape hatch for a scope.files entry discovered missing WHILE the implement stage runs (a coupled test, a registration table, a doc companion) — instead of the runner silently dropping the undeclared edit (#581) or failing category-B on an undeclared created file (#818/#825).

## Storage

This package: domain `Amendment`/`PathEntry`/`Status` + `Repository` {Create, GetByID, ListByRun, CountByStage, Decide}; sqlc `db/`; migration `0029_scope_amendments` — paths jsonb of `{path, operation: modify|create}`, status `pending|approved|denied`, pending-only atomic decide.

## HTTP surface

`backend/internal/server/scope_amendment.go`:

- `POST /v0/runs/{id}/scope-amendments` — run-bound `fhm_` token with `write:scope-amendments` ONLY; path-run == token-run; the run's resolved active stage (first dispatched/running, else first non-terminal in sequence order — the local-runner first-stage gap, #1030: local-runner stages stay `pending` until trace upload, so a decomposition child's implement stage never reaches dispatched/running at request time) must be implement; capped at **2 rows per stage** — denied requests consume budget → 422 `amendment_budget_exhausted`.
- `GET` — run-bound token with `mcp:read` own-run-only OR operator bearer/session.
- `POST .../{amendment_id}/decision` — operator-only `write:stages`; run-bound tokens → 403 `self_decision`; already-decided → 409.

Audit kinds `scope_amendment_requested` / `scope_amendment_decided` (internal, NOT issue-comment surfaces; the request entry is the operator's `fishhawk_await_audit` anchor).

## Scope grant

`server/mcptoken.go::resolveExecutingStageType` adds `write:scope-amendments` to implement-stage tokens UNCONDITIONALLY (independent of the `agent_self_retry` conditional).
It resolves the stage via the same active-or-next rule (`activeOrNextStage`: first dispatched/running, else first non-terminal, #1030), so a decomposition child's still-pending implement stage grants the scope while a pending/awaiting_approval plan stage ahead of it does not — plan-stage tokens never carry it.

## Activation (both ends, #960 verified-tree invariant)

Activation folds at BOTH ends so the #960 verified-tree invariant holds:

- `server/prompt.go::mergeApprovedScopeAmendments` — a third `foldScopePaths` caller, source `scope-amendment`, both prompt + prompt-render sites — a restart/fix-up prompt carries the amended scope.
- The runner's pre-commit refresh `runner/cmd/fishhawk-runner/main.go::refreshScopeAmendments` (reuses the SAME `fhm_` bearer retained from `FetchMCPToken` — one agent-side auth path; the Ed25519 scheme signs request-body bytes so a body-less GET takes the bearer), which folds approved paths into `cfg.scopeFiles` BEFORE the committed-tree gates and every `StageScoped` call.

## Agent protocol

Rendered in the implement prompt (`backend/internal/prompt/prompt.go` `### Mid-stage scope amendments`): POST with `$FISHHAWK_API_TOKEN`, then AWAIT the decision via the GET `?wait=<seconds>` long-poll (#1035, slice 1) — re-issue `?wait=30` each time it returns still-`pending`, looping to a ~15-min TOTAL budget (deterministic termination, 2-request budget preserved); never edit a requested file before approval, batch paths, deny → adapt in-scope or fail loud.

## An EXPIRED window is NOT a denial (#2601)

The wait-poll can end two ways and they are **different facts**. `denied` is a DECISION carrying a `decision_reason`. Still-`pending` at the ~15-min cap is an **EXPIRY** — the ABSENCE of a decision. The prompt's step 2 used to say "proceed as if denied" at the cap, which made operator LATENCY indistinguishable from operator REFUSAL and destroyed the pass (run 7eaadaeb burned two verify-fix reinvocations unable to touch the requested file, then failed). Three parts now keep them distinct:

- **Agent contract** (`prompt.go` step 5, textually distinct from step 4): on expiry the agent either adapts within scope in a way that FULLY satisfies the done-means AND DISCLOSES that it adapted under an amendment that was never decided (naming the amendment id + requested paths), or STOPS IMMEDIATELY and fails loud. The #1170 silent-wrong-fix prohibition is carried on BOTH branches — a comment-only or no-op touch substituted for the real edit is forbidden under an expiry exactly as under a denial. Never reported as the operator having refused.
- **Runner consequence** (`runner/cmd/fishhawk-runner/main.go::detectUndecidedScopeAmendments` + `undecidedScopeAmendmentsForStage`): at agent exit, an amendment whose `status` is `pending` AND whose `stage_id` is THIS stage emits a `scope_amendment_undecided` JSONL line + a `policy_event` (field set `{event, run_id, stage_id, amendments:[{amendment_id, paths:[{path, operation}]}]}`) and moves the stage from the verify-fix loop to the SINGLE-SHOT committed gate: the tree is still verified (never pushed unverified), only the agent REINVOCATION is withheld, because a fix requiring a path the operator never granted cannot converge. A category-B demote is annotated with the amendment id, the requested paths, and the recovery — `fishhawk_decide_scope_amendment` then `fishhawk_retry_stage`. ORDERING is load-bearing: the check runs AFTER `refreshScopeAmendments` folds this pass, and statuses are monotonic, so an amendment approved inside the fold window can never be counted undecided. It is best-effort — nil client / empty `fhm_` token / non-implement stage / fetch error all fail OPEN to today's behaviour.
- **Relay** (`runStageEventMessage`, `backend/internal/mcpserver/run_stage.go`): an explicit `scope_amendment_undecided` case names the id + paths in-band, so an operator watching a blocking `fishhawk_run_stage` learns the request went unanswered instead of inferring it from a generic verify failure.

**There is deliberately NO `expired` status and no third `scope_amendment_decided` audit value.** The row staying `pending` IS the third value, and it is what keeps a late operator decision landable: `refreshScopeAmendments` folds any APPROVED amendment on a later implement invocation of the same run, including a `fishhawk_retry_stage` re-run (pinned by `TestRefreshScopeAmendments_ApprovedAfterExpiryFoldsOnRetry`). Marking the row expired server-side would discard exactly that. Out of scope: parking a LIVE agent on the pending request, and extending the window.

The ~15-minute figure is stated in ONE place the agent follows — the prompt — and the operator-facing `fishhawk_decide_scope_amendment` tool doc (`backend/internal/mcpserver/scope_amendment.go`) now quotes the same number; it previously said ~5 minutes and understated the window threefold. The `fishhawk-mcp` surfaces no longer hardcode the figure at all: every Go-rendered statement of it (`await_stage.go`, `dispatch_stage.go`, `scope_amendment.go`) renders from a single package constant, `amendmentPollWindowMinutes` in `backend/internal/mcpserver/amendment_window.go`, and a package guard test (`TestNoRetiredProceedAsDeniedWording` / `TestNoStaleAmendmentWindowFigure`) fails in-loop on any reintroduction of the retired proceed-as-denied wording or the stale ~5-minute figure on an operator-facing mcpserver surface. That constant unifies the mcpserver Go surfaces only — the prompt and the operator READMEs state the number in text and cannot render from it — so the guard is a tripwire, not a single source of truth. **This README is the canonical contract for "an expiry is not a denial".**

## Deadline observability on a request (#2540)

A request filed near the agent wall clock is **undecidable by construction**: the agent's ~15-minute wait-poll window can outlive the stage's remaining agent runtime, so the stage is SIGKILLed while the agent is still polling and a correct, correctly-approved amendment is lost with the whole pass (run 138281e7, amendment filed at t+3555s of a 3600s clock). The request handler derives the executing implement stage's **remaining agent wall clock** — the spec-resolved `agent_timeout_seconds` (`resolveAgentTimeout`) minus the stage's elapsed time — and, when it can, surfaces it OBSERVABILITY-ONLY on the created row's REQUEST response + `scope_amendment_requested` audit as `stage_deadline_seconds_remaining` + `amendment_poll_window_seconds` (`AmendmentPollWindowSeconds`, 900), so an operator can **see** how tight the race is. The row is always **created** and `pending` (a late decision plus `fishhawk_retry_stage` still folds the paths, and `detectUndecidedScopeAmendments` still names them on the failure path).

**There is DELIBERATELY no refusal.** An earlier design also emitted an `undecidable_before_deadline` flag that told the agent to skip its wait-poll, but #2540 approval condition 1 struck it: the remainder is uncertain in the **pessimistic** direction, so a computed "too short" can be a false positive, and refusing a WINNABLE amendment (killing a stage that would have succeeded) is strictly worse than the bug it guarded against (which merely loses an already-unwinnable amendment). Losing an occasional genuinely-unwinnable amendment is the status quo and costs nothing new. So the numbers are **displayed, never acted on** — there is no wire/audit/prompt signal an agent keys off to abandon a wait.

`amendmentDeadlineRemaining` still gates whether the numbers are shown (it fails open — omitting them — for the same uncertainty cases), but a misfire now costs at most a hidden number, never a killed amendment:

- **(a) unresolvable spec budget** — `resolveAgentTimeout` returns 0 for an absent/unparseable `workflow_spec` or a stage not matched in the spec.
- **(b) never-started stage** — `started_at` is nil, so there is no clock to measure elapsed against.
- **(c) non-positive remainder** — elapsed already meets/exceeds the whole budget; a LIVE agent posting the request proves the deadline has not literally passed, so this evidences a bad derivation (the recorded `started_at` precedes the agent's real clock start), not a passed deadline.
- **(d) self-retried stage** — `stages.started_at` is written under `COALESCE(started_at, $3)` and never overwritten, so a re-spawned stage's elapsed is cumulative and its remainder understated. (An operator `fishhawk_retry_stage` that re-opens the stage WITHOUT bumping `SelfRetryCount` leaves the same cumulative `started_at` — indistinguishable from a first run, which is precisely why no refusal is built on the remainder.)

The 900s window must stay equal to the prompt's ~15-minute figure and `amendmentPollWindowMinutes`; `AmendmentPollWindowSeconds` (`backend/internal/server/scope_amendment.go`) and `amendmentPollWindowMinutes` (`backend/internal/mcpserver/amendment_window.go`) are pinned equal by a cross-package test in `backend/internal/mcpserver/stage_wait_test.go`.

## In-window decisions are HONORED, not poll-missed (#1035, slice 2)

While `fishhawk_run_stage` blocks the driving session, the runner's `watchScopeAmendments` goroutine (`runner/cmd/fishhawk-runner/main.go`, implement stages only, best-effort) emits a single-line `scope_amendment_pending` JSONL event (fields `{event, run_id, stage_id, amendment_id, paths}`) the moment a request is observed pending.
Both that watcher and the agent heartbeat write the shared `logSink` through a mutex-guarded `syncWriter` so the one-JSON-object-per-line invariant the relay scanner depends on holds under the two concurrent writers.

The `fishhawk-mcp` `runStageEventMessage` relay (`backend/internal/mcpserver/run_stage.go`) has an explicit `scope_amendment_pending` case that surfaces the `amendment_id` + paths as an in-band progress notification, so an operator driving a SECOND session can `fishhawk_decide_scope_amendment` during the agent's wait and the agent resumes WITH the decision.

This supersedes the older "delivery is poll-based, no push channel" note for the locally-driven case; the runner-emit→relay-decode seam is a literal-JSONL field-name contract pinned on both ends (#618). The `?wait`-absent / non-local path is unchanged (ADR-021 best-effort degradation).

## Related

MCP verbs: `fishhawk_list_scope_amendments` + `fishhawk_decide_scope_amendment` (`backend/cmd/fishhawk-mcp/scope_amendment.go`). #730 `add_scope_files` and fix-up `allow_create` paths are untouched siblings.
