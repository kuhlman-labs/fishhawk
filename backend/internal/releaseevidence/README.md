# backend/internal/releaseevidence

Pure, HTTP-free assembly of release evidence over a ref range, plus the advisory semver-bump classifier that reads it.

## Release-evidence assembly (E33.1 / #1586, ADR-051 option B evidence half)

An assembly layer that turns a ref range (`previousRef .. candidateRef`) into a `ReleaseEvidence` model.

`Assembler.Assemble`:

- Resolves the merged PRs in range via the `MergedPRResolver` seam (`GitHubResolver` over `githubclient.CompareCommits` + the new `ListPullRequestsForCommit`, de-duped by PR number).
- Maps each PR to its runs by `pull_request_url` equality (the `server/cost.go::mergedPRCostFor` precedent).
- Per change, assembles: the approved plan summary+link, both `implement_reviewed` verdicts, the latest `acceptance_outcome_recorded` outcome, deferred concerns (`concern.StateDeferred`), and per-PR cost (`cost.AggregateRunCost` over the `cost_recorded` ledger, summed across ALL runs on the PR).

Invariants and edge cases:

- The primary evidence run is the NEWEST terminal-succeeded run on the PR (not the earliest — a recovery/rerun history makes the earliest a failed parent).
- The plan is resolved via the `parent_run_id` walk (the `prompt.go::loadApprovedPlanForRun` precedent), so a plan-stage-less recovery child still surfaces its parent's plan.
- `ReleaseEvidence.TotalCostUSD` is the sum of the per-PR rollups by construction.
- A PR in range with no resolvable Fishhawk run (human-led / loop-bypassing) is emitted as a reduced-evidence `ChangeEvidence` (`ReducedEvidence=true` + `ReducedReason`), never fabricated (ADR-051 honesty constraint).
- The HTTP endpoint is E33.2.

## Semver-bump classifier (E33.4 / #1589, ADR-051)

`classify.go`: `ClassifyBump(*ReleaseEvidence) BumpHint` derives a heuristic semver-bump recommendation over E33.1's assembled evidence.

- Since `ReleaseEvidence` carries only prose (no structured per-file change data), the signal detectors are case-insensitive keyword heuristics over each change's plan summary + PR title + deferred-concern categories (a reduced-evidence change contributes its title only).
- Breaking signals (schema-major bump, removed/renamed OpenAPI path, migration down-incompat marker) → `major`; additive signals (new endpoint, new optional field, new stage/artifact enum member) → `minor`; else `patch`.
- The matchers are an ordered data table (`bumpMatchers`) kept as keyword lists — not inline conditionals — so E33.5's cut-time surface and future tuning stay diffable.
- Per-change signals roll up to the release's max level (`BumpLevel.rank`), and each `BumpSignal` names its introducing PR so the hint is auditable.
- The hint is **advisory only** — it never blocks or decides; the operator ratifies the version at cut time (E33.5), so a wrong hint has no failure mode beyond an operator override.
- `BumpHint.PreviewLine()` renders the `suggested bump: <level> (because ...)` line; E33.5 (#1590) wires `ClassifyBump(ev).PreviewLine()` into `releasenotes.Render` (`render.go`) where the reserved semver-hint slot used to sit, so both the preview and the persisted notes carry the advisory line.
