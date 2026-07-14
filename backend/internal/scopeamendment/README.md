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

Rendered in the implement prompt (`backend/internal/prompt/prompt.go` `### Mid-stage scope amendments`): POST with `$FISHHAWK_API_TOKEN`, then AWAIT the decision via the GET `?wait=<seconds>` long-poll (#1035, slice 1) — re-issue `?wait=30` each time it returns still-`pending`, looping to a ~15-min TOTAL budget and proceeding as-denied at the cap (deterministic termination, 2-request budget preserved); never edit a requested file before approval, batch paths, deny → adapt in-scope or fail loud.

## In-window decisions are HONORED, not poll-missed (#1035, slice 2)

While `fishhawk_run_stage` blocks the driving session, the runner's `watchScopeAmendments` goroutine (`runner/cmd/fishhawk-runner/main.go`, implement stages only, best-effort) emits a single-line `scope_amendment_pending` JSONL event (fields `{event, run_id, stage_id, amendment_id, paths}`) the moment a request is observed pending.
Both that watcher and the agent heartbeat write the shared `logSink` through a mutex-guarded `syncWriter` so the one-JSON-object-per-line invariant the relay scanner depends on holds under the two concurrent writers.

The `fishhawk-mcp` `runStageEventMessage` relay (`backend/cmd/fishhawk-mcp/run_stage.go`) has an explicit `scope_amendment_pending` case that surfaces the `amendment_id` + paths as an in-band progress notification, so an operator driving a SECOND session can `fishhawk_decide_scope_amendment` during the agent's wait and the agent resumes WITH the decision.

This supersedes the older "delivery is poll-based, no push channel" note for the locally-driven case; the runner-emit→relay-decode seam is a literal-JSONL field-name contract pinned on both ends (#618). The `?wait`-absent / non-local path is unchanged (ADR-021 best-effort degradation).

## Related

MCP verbs: `fishhawk_list_scope_amendments` + `fishhawk_decide_scope_amendment` (`backend/cmd/fishhawk-mcp/scope_amendment.go`). #730 `add_scope_files` and fix-up `allow_create` paths are untouched siblings.
