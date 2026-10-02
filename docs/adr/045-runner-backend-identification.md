---
id: ADR-045
title: "runner-backend identification — derive runner_kind from the execution channel (host-dispatch ⇒ local, runner self-report, mismatch guardrail), not an operator declaration"
status: unknown
issue: https://github.com/kuhlman-labs/fishhawk/issues/1347
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-045: runner-backend identification — derive runner_kind from the execution channel (host-dispatch ⇒ local, runner self-report, mismatch guardrail), not an operator declaration

## Context

`runner_kind` (`github_actions` | `local`) tags a run's execution backend, and the drive state machine branches on it (`approvals.go:474/477`, `drive/drive.go`): `github_actions` auto-advances a stage via the orchestrator's `workflow_dispatch` edge (a GitHub Actions runner is expected to pick it up and, ultimately, open the PR), while `local` parks with a host-side dispatch next-action for the operator to run. Today `runner_kind` is a **creation-time operator declaration** that **defaults to `github_actions`** (`backend/cmd/fishhawk-mcp/tools.go:1372`, `backend/internal/run/postgres.go:79`), and the runner performs **no environment detection** (grep of `runner/` for `GITHUB_ACTIONS`/`CI` is empty).

Consequence (root cause of #1344 and the broader 2026-06-24 wedge saga): an operator who omits `runner_kind:local` on `fishhawk_start_run` but drives the run locally via `fishhawk_dispatch_stage`/`fishhawk_run_stage` produces a **tag/execution mismatch** — the run is tagged `github_actions` but executed on the host. The drive then waits on a phantom GitHub-Actions runner: misleading "auto-dispatches … nothing to run from the operator host" next-actions, fixup/retry re-opens that don't auto-spawn a local runner, and — worst — after a `scope_completeness_exempted` the drive waits on the Actions runner to open the PR, which never happens locally → the run wedges (no PR, stuck `running`, `next_actions:None`). The mismatch is silently accepted today.

## Options

1. **Status quo — operator-declared.** Error-prone (demonstrated); the human must know the environment and remember the flag, and the default is the *wrong* one for the local dogfood loop.
2. **Infer from the dispatch channel.** The execution backend is definitional in *how* a stage is dispatched: a host-side dispatch endpoint call (`fishhawk_dispatch_stage`/`run_stage`) IS local; a `workflow_dispatch` job claimed by a GitHub Actions runner IS `github_actions`.
3. **Runner self-identification.** The runner knows its own environment (`GITHUB_ACTIONS`/`GITHUB_RUN_ID`/`CI` present ⇒ `github_actions`, absent ⇒ `local`) and can report it.
4. **Combine 2+3 + a guardrail.**

## Recommendation

Option 4, with the host-dispatch ⇒ `local` rung as the surgical, highest-leverage first step.

## Decision

1. **Host-dispatch ⇒ `local` (surgical, do first).** When the host-side dispatch endpoint (`fishhawk_dispatch_stage`/`run_stage`) dispatches a stage, set/lock `runner_kind=local` on the run — the call IS a host dispatch. This alone removes the operator from the decision for the entire local loop (calling `run_stage` from the host flips the tag to `local`; the drive then parks correctly instead of waiting on a phantom Actions runner).
2. **Runner self-identification (authoritative confirmation).** The runner detects its environment and stamps it on its `runner_started` report; the backend reconciles `runner_kind` from it. The runner cannot be wrong about where it runs.
3. **Mismatch guardrail.** If a run tagged `github_actions` receives a host-side dispatch (or a `local` run is claimed by an Actions runner), warn or reject rather than silently executing — turn a silent wedge into an actionable error.
4. **Resolve at first dispatch, not creation.** The drive picks the channel at plan-approval, before execution reveals the kind, so the creation-time `runner_kind` is treated as a hint/default and the first stage's actual channel locks the truth; subsequent auto-advance decisions use the locked kind.

## Consequences

- The operator never declares the backend; the drive parks (local) or auto-advances (github_actions) by observation, eliminating the #1344 wedge class.
- Compatible with the ADR-022 / #388 compliance use of `runner_kind`: the webhook/hosted path still resolves to `github_actions`; only the host-dispatch path flips to `local`.
- Cost is small and localized: a set-on-dispatch change at the host endpoint (#1346 rung 1), a runner env-detection + report addition (rung 2), and a guardrail check (rung 3).
- A run could in principle change backends mid-life; locking on first dispatch (with the guardrail) bounds that.

## Children

- #1346 — implementation (the three rungs).
- #1344 — the product bug this closes (silent host-dispatch of a `github_actions`-tagged run).
- #1355 — deferred follow-up: the pre-dispatch mismatch BLOCK (decision 1's host-endpoint set/reject + decision 3's reject variant), optional defense-in-depth on top of #1346's post-execution audit.

## Status — RESOLVED (2026-06-24)

Decision realized and **validated live**. #1346 (merged + deployed) implemented the runner self-report (decision 2 — runner detects `GITHUB_ACTIONS`/`GITHUB_RUN_ID`/`CI` with `CI`-alone NOT treated as github_actions, stamps `runner_kind` into the signed bundle manifest), first-dispatch locking (decision 4 — `runner_kind_resolved` set on the first signed manifest, before the plan-approval dispatch gate), and the mismatch guardrail as a **post-execution `runner_kind_mismatch` audit** (decision 3, warn variant). Validation: a run started with no `runner_kind` auto-resolved `github_actions`→`local` (`runner_kind_resolved` audit `{from:github_actions, to:local}`) before plan-approval, eliminating the #1344 wedge class — the operator no longer declares the backend.

**As-shipped deviation from the recommendation:** the recommendation led with decision 1 (host-endpoint sets `local`, surgical). #1346 chose the more-attestable runner-self-report-via-signed-manifest path instead, which achieves the same outcome (host runs self-resolve to `local`) while keeping the claim tamper-evident (migration-0024 chain unaffected; verifier doesn't read `runner_kind`). Consequently the **PRE-dispatch** form of decisions 1 & 3 (block/correct before execution, vs the post-execution audit shipped) is deferred to **#1355** as optional defense-in-depth — auto-detection already makes mismatches rare. ADR closed; the residual surface is tracked on #1355.
