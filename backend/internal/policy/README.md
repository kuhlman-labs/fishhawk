# backend/internal/policy

Backend-side source of truth for the closed set of workflow-spec constraints (`forbidden_paths`, `allowed_paths`, `max_files_changed`, `required_outcomes`). `Evaluate` runs them against a stage's produced diff; `EmitEvaluation` writes the chained `policy_evaluated` audit entry that compliance exports quote. The runner runs the same checks in-line (`runner/internal/constraint/`) for immediate agent feedback, but its report alone is not auditable — the backend re-evaluates every uploaded trace here.

## `required_outcomes` — the two test-shaped outcomes

| Outcome | Reads | Satisfied when |
|---|---|---|
| `tests_added_or_updated` | the diff's file **names** (`isTestPath`) | a test-named file was added/modified — or the diff touches no unit-testable source at all (docs/scripts/config only, #610) |
| `verification_reported` (v1.5, #1886 / ADR-059) | the stage's **machine-verified verify result** | the committed-tree verify gate reported `passed` — nothing else |

`tests_added_or_updated` is filename-shape-aware: a diff containing `foo_test.go` satisfies it whether or not the file contains a real test and whether or not anything ever ran. `verification_reported` is the substance-aware sibling that closes that gap. The two are independent and may be declared together; `tests_added_or_updated` behavior is unchanged.

### `verification_reported` semantics (fail-closed)

`checkRequiredOutcomes` reads only `Constraints.Verification`:

| Signal | Result |
|---|---|
| `nil` (no evidence in the trace) | violation — `no verification evidence in trace` |
| `Outcome: "failed"` | violation naming the outcome and, when known, the failing command |
| `Outcome: "skipped"` | violation — a skipped verify gate is **not** a passed gate |
| any other non-`passed` value (including `""`) | violation naming the outcome |
| `Outcome: "passed"` | **satisfied** |

Two deliberate omissions, both load-bearing:

- **No filename inspection.** `isTestPath` / `diffTouchesTests` are not consulted, so a diff whose only change is a test-*named* file does not satisfy this outcome. That is exactly the diff shape that satisfies `tests_added_or_updated`, and the asymmetry is the entire point.
- **No docs-only vacuous branch.** The `diffTouchesTestableCode` carve-out that vacuously satisfies `tests_added_or_updated` on a docs-only diff is not inherited. A docs-only diff with no verification signal still violates.

It is also **not deferrable**. `DeferredRequiredOutcomes` still returns only `ci_green` (whose missing signal defers to branch protection per #251 / ADR-017). Adding `verification_reported` there would reconstruct the vacuous pass it exists to remove.

## Signal derivation

The signal is derived at trace-upload time by `verificationSignalFromBundle` in `backend/internal/server/trace.go` — this package stays free of any `bundle` import. It reads the runner's single pre-redacted `gate_evidence` event (#963), which already digests every machine-verified verify result, so no new runner emission was needed:

1. `verify_summary` when present — the verify-fix loop's terminal, once-per-stage result (#804).
2. otherwise the **last non-superseded** `verify_run` — the single-shot committed-tree gate (#802) path. Only the last run reflects the pushed tree; earlier verify-fix-loop iterations are marked `superseded` (#1205) and skipped.

`Commands` carries the non-superseded runs as `{command, exit_code, outcome}` only — no output tails, so the audit payload stays bounded.

Returns **nil** (read as a violation, never a pass) when: the bundle carries no `gate_evidence` event (`bundle.ErrNoGateEvidence` — older runner, or a stage that ran no gates), extraction fails, or the evidence carries neither a summary nor any verify run.

## Runner defers to the backend

`runner/internal/constraint` **skips** `verification_reported` rather than evaluating it. Its in-line check fires on the implement push path before either committed-tree verify gate runs, so no verify result exists locally. Without the explicit skip case its `default:` branch would emit `unknown outcome "verification_reported"` and fail every opted-in run as category-B. This is the one deliberate divergence from the otherwise-lockstep runner/backend `checkRequiredOutcomes` pair.

## Audit round-trip invariant

`Constraints` is the `applied_constraints` shape of the `policy_evaluated` audit payload (`EvaluationPayload.Applied`). The post-CI re-evaluation (`backend/internal/server/policy_reeval.go`) **decodes the prior payload, mutates only `CIGreen`, and re-emits** — so every signal field must be exported and json-tagged or it is silently dropped on re-eval, turning a satisfied outcome into a violation. `Verification` carries an explicit `json:"verification,omitempty"` tag for exactly this reason; the `omitempty` keeps pre-#1886 audit entries byte-identical.

## Anchors

- #1886 / ADR-059 — substance-aware `verification_reported` (workflow-v1.5).
- #610 / #601 — the `tests_added_or_updated` heuristic and its docs-only scoping.
- #297 / #251 (ADR-017) — deferred outcomes and branch protection.
- #283 / #247 / #233 — constraints cache, always-emit, audit payload shape.
- #963 / #1205 / #804 / #802 — gate evidence, superseded verify runs, verify summary, committed-tree gate.
