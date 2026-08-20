# `failuresig` — failure-signature registry

Matches a failed stage's already-held evidence against an ordered catalog of named failure signatures, each carrying what the failure MEANS and the recommended verb sequence to recover from it ([#1703](https://github.com/kuhlman-labs/fishhawk/issues/1703)).

The operator-facing catalog — one section per signature, written for an operator with no dogfood history — is published at [`docs/architecture/failure-signatures.md`](../../../docs/architecture/failure-signatures.md). This README is the package contract.

## API

| Symbol | Role |
|---|---|
| `Evidence` | the narrow, consumer-independent value a caller adapts a failed stage into |
| `Signature` | one catalog entry: `ID` / `Title` / `Means` / `Playbook` + an unexported matcher |
| `Hint` | the surfaced match — what a consumer renders |
| `Match(Evidence) *Hint` | the entry point; nil when nothing matches |
| `Registry() []Signature` | the catalog, in precedence order |
| `RegistryVersion` | the catalog revision stamped onto every `Hint` |

The function is `Match` and the result type is `Hint`, deliberately: a package-level type and function share one identifier namespace in Go, so `type Match` + `func Match` would not compile.

## Invariants

**Display-only, never gates.** Nothing here participates in an applicability decision, reorders a caller's actions, or influences a state transition. The one consumer (`next_actions`) folds the hint in via a helper that ONLY assigns the field — `foldFailureSignature` never appends to, reorders, or rewrites the action list, and `TestNextActions_SignatureNeverMutatesActions` compares the actions against a no-signature baseline by deep equality. A hint that gates a run is a bug.

**Fail-open.** `Match` returns nil whenever nothing fires, and a nil hint must leave the caller byte-for-byte as it was without the registry. This is the standing safety property: a registry that could change behaviour on a NON-match would be strictly worse than no registry, because it would put every unrecognized failure — the ones an operator most needs to read plainly — behind a new layer of product opinion.

**First match wins, and precedence is behaviour.** `Registry()` is ordered most-specific-first and `Match` returns on the first hit. Where evidence can satisfy two entries the order decides the recovery: a category-A failure citing BOTH a terminal external API error and an absorbed `verify_infra_flake_retry` classifies as `external_api_incident`, because backing off and retrying immediately are opposite moves and the wrong one burns retry budget against a live upstream incident. An entry placed after one whose evidence it overlaps can never fire on the overlapping shape.

**Constant-size by construction.** A `Hint` carries only registry-owned strings — no failure-reason passthrough, no caller text of any kind — so a matched block cannot grow with run data. `TestMatchBlockIsConstantSize` drives every catalog entry through `Match` on realistic evidence and asserts the marshalled hint is both under a named cap and byte-for-byte the same size when the SAME evidence carries a 200 KB failure reason; `TestMatchBlockNeverEchoesEvidence` pins the short-vs-long pair directly. Those two tests are what LICENSE classifying `next_actions.signature` as `tierNever` in the MCP response byte ladder (`backend/internal/mcpserver/bound.go`): the block rides the budget untiered precisely because it cannot grow. A future field echoing caller input turns them RED — and both must drive the PRODUCTION path to do so, because a hand-built `Hint` leaves such a field empty and the cap assertion green.

**The catalog is rebuilt per call.** `Registry()` returns a fresh slice with fresh `Playbook` literals every time, which is the control that makes a caller's mutation of a returned hint unable to reach the next caller; `TestRegistryReturnsFreshPlaybooksPerCall` is its counterfactual vehicle. `Match`'s `append([]string(nil), …)` copy is a second layer that only becomes load-bearing if `Registry()` is ever changed to hand out a shared package-level catalog — `TestMatch_ReturnsIndependentPlaybookCopies` asserts the end-to-end property but stays green if that copy alone is deleted, so it is not the copy's discriminator.

**Pure.** No I/O, no reflection, no backend round-trip. Every matcher is a string/int predicate over `Evidence`.

## The healthy-evidence guard

`Evidence.describesFailure` admits only evidence that actually describes a FAILED stage, and has two clauses with different standing:

- **A non-empty `StageState` other than `failed` is refused.** This clause is load-bearing and attainable: a stage retried in place can carry the PREVIOUS attempt's `failure_reason` on its row while running again. Without it, a reason-anchored signature fires on a live stage and hands the operator a recovery playbook for a failure that is not happening. `TestMatch_HealthyEvidenceNeverMatches` is its counterfactual vehicle — its cases are byte-identical to matching cases except for `StageState`, so deleting the guard turns it RED on the behavioural assertion.
- **Evidence carrying neither a category nor a reason is refused.** This clause is defence in depth for a future counter-anchored signature. With the v1 catalog it has NO attainable mutation: every matcher independently requires a non-empty anchor (`strings.Contains` against a non-empty needle is false on an empty haystack) or a specific failure category, so removing this clause alone changes no observable behaviour. The matchers themselves, not this clause, are what defend the property today — see the counterfactual table in the [#1703](https://github.com/kuhlman-labs/fishhawk/issues/1703) PR.

## Evidence field provenance

Every field mirrors data the consumer already holds; none requires a new read.

| Field | Mirrors |
|---|---|
| `StageType`, `StageState`, `FailureCategory`, `FailureReason` | the stage row (`GET /v0/runs/{id}/stages`) |
| `ProgressReported`, `TurnsThisAttempt`, `TokensThisAttempt` | `stage.progress` — the runner's `stage_progress` heartbeat ([#2541](https://github.com/kuhlman-labs/fishhawk/issues/2541)) |
| `RetryAttempt`, `RunnerKind` | the run row |
| `IsDecompositionChild` | derived from the orchestrator's minted-child shape (a `parent_run_id` plus an implement stage but no plan or review of its own) |

`ProgressReported` is what distinguishes "the heartbeat reported zero activity" from "no heartbeat arrived": an absent heartbeat leaves the counters at their zero values, which must never be read as observed inactivity. `RunnerKind` and `IsDecompositionChild` are carried because they are part of the evidence contract a consumer populates and are cheap to supply; no v1 signature keys on them.

## The anchors are best-effort STRING CONTRACTS

`AnchorExternalAPIError`, `AnchorQuotaUnavailable`, `AnchorVerifyInfraFlake`, `AnchorSliceIntegrationConflict`, `AnchorLineageLock`, `AnchorRunnerExitedBeforeReporting` and `AnchorZeroExitStrand` are each a substring of a literal some component provably emits, with the emitting site named in the constant's doc comment.

They are not typed plumbing. The runner is a separate `go.work` module and cannot share a constant with the backend — the same [#1548](https://github.com/kuhlman-labs/fishhawk/issues/1548) limitation the pre-existing `next_actions` phrases carry — so a runner-side wording change silently stops a matcher firing. The failure mode is fail-open: a MISSING hint, never a wrong one.

What this package does fix is intra-backend drift. It is the SINGLE place the backend module declares these literals: `backend/internal/mcpserver/next_actions.go` reads `failuresig.Anchor*` for its external-API, quota, flake and slice-integration-conflict arms rather than declaring its own copies, so the hint and the surrounding action can no longer disagree about what a failure is. That silent-drift shape — the hint stops matching while the action still fires, presenting as "no signature matched" — is the worst failure mode this feature has, which is why the constants live here and are re-used rather than copied.

## Adding a signature

1. A matcher + catalog entry in `registry.go`, at the right point in precedence order.
2. A fixture in `matchFixtures()` (`failuresig_test.go`) driving REALISTIC evidence (the literal shape the emitting site renders, not a bare anchor) to the new id. It is the shared production-path corpus: the id, version-stamp and constant-size tests all drive `Match` through it, and `TestMatch_EverySignature` fails naming any catalog entry with no fixture.
3. A `### <id>` section in [`docs/architecture/failure-signatures.md`](../../../docs/architecture/failure-signatures.md).

`TestCatalogDocumentsEverySignature` fails naming the id if step 3 is skipped; `TestCatalogDocumentsNoUnknownSignature` fails if a doc section outlives its entry. The documentation requirement is structurally enforced, not asserted in prose.
