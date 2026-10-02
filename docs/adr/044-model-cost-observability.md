---
id: ADR-044
title: "model cost observability — warn-not-fail price drift-check, 3-bucket fresh/read/write cache cost model with separate capture, split cost-accuracy from efficiency-metric, daily cadence"
status: unknown
issue: https://github.com/kuhlman-labs/fishhawk/issues/1336
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-044: model cost observability — warn-not-fail price drift-check, 3-bucket fresh/read/write cache cost model with separate capture, split cost-accuracy from efficiency-metric, daily cadence

## Context

`pricing/pricing.go` is the single source of truth for cost rollup, spend alerts, and budget. #1334 showed it drifts silently (Claude opus stale at 15/75 for ~3 releases after the real 5/25; OpenAI models entirely absent → codex recorded `$0`). Neither vendor offers a programmatic pricing API (`GET /v1/models` returns ids/metadata only), so freshness must be engineered. Separately, cached input is not modeled at all, and we want both (a) accurate cached cost and (b) a cache-efficiency metric to drive cache-hit optimization. A related gap: model ids in `.fishhawk/workflows.yaml` are unvalidated free strings, so a typo or deprecated model fails late instead of being surfaced early. Researched via a codex (gpt-5.5) advisory + a data-flow feasibility trace.

## Options & key findings

**Drift check:** warn vs fail the build. Coupling CI/deploys to a community dataset's correctness is the wrong dependency; a stale price is operational pressure, not a build break.

**Cache cost model:** "cached input is cheaper" is only half true. Cache **reads** are ~0.1× input (Anthropic), but cache **writes/creation** cost **more** than fresh (Anthropic 1.25× at 5-min TTL, 2× at 1-hour). So a 2-bucket (fresh/cached) model misprices; a 3-bucket model (fresh / read / write) is required. Today the capture layer **sums** read+creation into one `CachedInputTokens` (claudecode: `CacheReadInputTokens + CacheCreationInputTokens`), which cannot be priced correctly.

**Wire-contract blocker:** cached counts reach the backend for *reviewers* (the `cost_recorded` audit already carries `cached_input_tokens`), but the runner→backend **trace-bundle manifest carries only Model/Input/Output** (`runner/internal/bundle/bundle.go`, `backend/internal/bundle/bundle.go`), so the implement **agent's** cached tokens never reach the cost rollup. The Anthropic SDK reviewer adapter (`backend/internal/anthropic/client.go`) discards cache fields entirely.

**Model-input validity:** `executor.model` / `reviewers.agents[].model` are free strings (`workflow-v0.schema.json` = `{"type":"string"}`); the only model check is the opt-in, fail-open `modelpolicy.go` allow-list (operator POLICY), enforced at the gate against the implement model only. So a typo / nonexistent / deprecated model passes validation and either fails late at agent spawn or records `$0`. A *static* enum can't satisfy both "accept newly-released models" and "reject typos/deprecated" — that needs a *dynamic* oracle. `GET /v1/models` lists currently-offered models per provider (no prices, but the availability list we need).

## Decision

1. **Price drift-check WARNS, never fails normal builds.** Manual table stays source of truth; the [LiteLLM dataset](https://github.com/BerriAI/litellm/blob/main/model_prices_and_context_window.json) is an alarm, pinned to an immutable commit (per the AGENTS.md pin-tools-in-CI rule). PR CI = non-blocking warn; a daily scheduled job opens/updates an issue on drift. Tolerance bands: ignore <2% float noise, warn >2%, high-severity >10%; record directionality (ours lower = under-billing risk; higher = over-reporting) + provenance (LiteLLM SHA, timestamp, matched id). **The internal completeness invariant — every live model id must be priced — stays a hard CI FAIL** (already added in #1334's drift test); it is distinct from external drift and keeps its fail severity.

2. **Cache cost is a 3-bucket model — fresh input, cache READ (cheap), cache WRITE/creation (premium) — captured SEPARATELY, not summed.** `planreview.Usage` and the adapters split read vs creation; pricing gains per-family read/write rates. This requires the manifest wire change (runner↔backend, backward-compat via omitempty), runner-executor capture, fixing the Anthropic SDK adapter to surface `CacheReadInputTokens`/`CacheCreationInputTokens`, and the rollup.

3. **Split into two efforts:** (a) cache-aware cost **accuracy** (the multi-layer wire change + read/write rates); (b) the cache-**efficiency metric/dashboard** (the optimization driver). Metric set: cache-read ratio, reuse factor (read÷write), **net savings** (gross read savings − write penalty − storage), per-stage hit rate (system prompt / tool schema / planner / executor / memory), and "eligible-but-missed" tokens. The reviewer-stage ratio is queryable today; agent-stage needs (a) first.

4. **Cadence:** drift-check = per-PR advisory + daily scheduled + manual trigger; `/v1/models` availability poll = daily (6–12h only if dynamically exposing models), as a CI-cron/ops job **not** the app hot path; high-severity if a model we actively route to disappears; never auto-enable a new model (requires pricing + eval + safety review).

5. **Model-input validity is validated against the live `/v1/models` snapshot, not a hardcoded enum** — and the #1335 poll's cached output is **elevated from an ops-alert to the validation oracle**. Validate `executor.model` AND `reviewers.agents[].model` **early** (at `workflows.yaml` spec-submission/validation time, with a gate-time backstop), reading a per-provider cached `/v1/models` snapshot (never a live API call on the hot path). **Fail-OPEN when unverifiable:** a stale/empty cache or an unreachable provider accepts with a warning — only a model **definitively absent from a FRESH snapshot** is rejected. This makes the two requirements coexist: a newly-released model is accepted automatically (it is in the live list, or fail-open accepts it before the cache refreshes), while a typo or deprecated/sunset model is surfaced — hard error for definitively-unknown (with did-you-mean + the available set), warning for deprecated/retiring or can't-verify. This is distinct from and stacks on top of the `modelpolicy.go` allow-list (POLICY) and the pricing table (#1334): validity → policy → pricing.

## Consequences

- The budget/spend signal becomes trustworthy and cache-aware, and cache-hit optimization becomes measurable (directly supports maximizing cached-token usage).
- Cost: a multi-PR effort. The manifest wire change has a backward-compat surface (older runners/bundles parse cached as 0). CI gains a non-blocking external dependency, contained by pinning the dataset commit.
- Splitting read/write capture touches `planreview.Usage`, the bundle manifest (both sides), and the runner executors; the Anthropic SDK adapter stops discarding cache fields.
- Invalid/deprecated/typo'd model ids are surfaced to the user **early** (submit-time) with an actionable message instead of failing late at agent spawn or silently costing `$0`; newly-released models are accepted with no redeploy; the fail-open-when-unverifiable rule guarantees a `/v1/models` outage never falsely rejects a valid run. The `/v1/models` poll now serves double duty (ops-freshness alert + validation oracle), so its reliability/caching matters more.

## Children

- #1334 — immediate price correction (opus 5/25, add gpt-5.5, live-id drift test). In flight.
- #1335 — drift-check (warn) + `/v1/models` availability poll automation. The poll's cached output also feeds model-input validation (#1339), not just ops alerts.
- #1338 — cache-aware cost accounting + efficiency metric (3-bucket fresh/read/write).
- #1339 — model-input validity validation against the live `/v1/models` snapshot (accept new, surface deprecated/typo, fail-open when unverifiable).
