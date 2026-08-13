# backend/internal/policy

Backend-side source of truth for the closed set of workflow-spec constraints (`forbidden_paths`, `allowed_paths`, `max_files_changed`, `required_outcomes`). `Evaluate` runs them against a stage's produced diff; `EmitEvaluation` writes the chained `policy_evaluated` audit entry that compliance exports quote. The runner runs the same checks in-line (`runner/internal/constraint/`) for immediate agent feedback, but its report alone is not auditable — the backend re-evaluates every uploaded trace here.

## `required_outcomes` — the two test-shaped outcomes

| Outcome | Reads | Satisfied when |
|---|---|---|
| `tests_added_or_updated` | the diff's file **names** (`isTestPath`) — plus, as a third case, the emitted **patch** via `DetectCommentOnlyGo` (#2660) | a test-named file was added/modified — or the diff touches no unit-testable source at all (docs/scripts/config only, #610) — or the carried comment-only verdict says every changed testable-source file is a `.go` file whose emitted changed lines are blank or ordinary `//` comments |
| `verification_reported` (v1.5, #1886 / ADR-059) | the stage's **machine-verified verify result** | the committed-tree verify gate reported `passed` — nothing else |
| `diff_coverage` (v1.6, #1888 / ADR-059) — a constraint KIND, not a required outcome | the stage's **measured new-line coverage** | the runner measured coverage at or above `min_new_line_coverage`, OR measured zero new coverable lines |

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

## The comment-only Go exemption (#2660)

A doc-comment correction to a `.go` file has no behavior to test, yet `feature_change` requires `tests_added_or_updated` and `routine_change` forbids non-test `.go` paths — so it was unlandable through either workflow. `DetectCommentOnlyGo` (`commentonly.go`) adds the third satisfying case.

**What it proves is a NARROW SYNTACTIC PROPERTY over the emitted unified diff — never behavioral emptiness in general:**

- every changed testable-source file is a `.go` file with status `A` or `M`;
- every one of those files has **exactly one** section in the emitted patch, and that section carries at least one `@@` hunk;
- inside each section, every changed (`+`/`-`) line is blank or an ordinary `//` line comment — never a Go directive comment (`isGoDirectiveComment`); and
- neither a backtick nor `import "C"` appears anywhere in the section, context lines included.

Every other input is **fail-closed**, each with one code from the closed `Reason` set (the signal lands in the `policy_evaluated` audit payload, so a reason NEVER echoes diff content): `empty_diff`, `no_go_source_changed`, `non_go_source_changed`, `unsupported_status`, `patch_absent`, `patch_truncated`, `binary_patch`, `quoted_patch_path`, `patch_path_unmatched`, `file_missing_from_patch`, `duplicate_patch_section`, `section_without_hunks`, `raw_string_delimiter_in_section`, `cgo_import_in_section`, `directive_changed`, `non_comment_line_changed`.

Two of those codes exist because a per-path map and a vacuous loop are both fail-OPEN shapes: `duplicate_patch_section` refuses a malformed patch carrying two sections for one path (the later would overwrite the earlier, so a comment-only section could mask a behavioral one for the same file), and `section_without_hunks` refuses a `.go` section carrying no `@@` hunk at all — a mode-only change — which would otherwise clear the file on zero changed lines.

`isGoDirectiveComment` is a PORT, not a delegation: **`go/ast.IsDirective` does not exist** (verified against go1.26.3 — `go/ast` exports only `IsExported` and `IsGenerated`). It reproduces `go/ast`'s unexported `isDirective` (`line `/`extern `/`export ` prefixes plus the `[a-z0-9]+:[a-z0-9]` tool-directive shape) and extends it conservatively with the legacy `+build` tag and the cgo `#` preamble forms. The untrimmed remainder is load-bearing: `//go:build` is a directive, `// go:build` (with a space) is an ordinary comment. A table test pins both.

**Why the verdict rides on `Constraints`, not on `Diff.Patch`.** It is derived ONCE at trace-upload time (`trace.go`) and carried as `Constraints.CommentOnly` — the same audit-round-trip discipline `Verification` and `DiffCoverageSignal` follow. The post-CI re-evaluation rebuilds the diff from the audit payload, which carries file names and never the patch, so recomputing downstream would evaluate an empty patch and flip a satisfied outcome into a violation. `nil` reads as NOT comment-only, so every pre-#2660 row re-evaluates byte-identically.

**Runner/backend division of labour.** Unlike `verification_reported`, the runner does NOT defer here: `runner/internal/constraint/commentonly.go` is a logic-identical port evaluated IN-LINE from the patch the runner just captured, because otherwise the runner would fail the stage category-B before the backend ever saw it. The runner has no signal channel and no audit round trip, so it recomputes rather than carries. Change the two copies together.

**THE RESIDUALS (accepted deliberately).** Behavioral emptiness is not decidable from a unified diff, and the in-section scans close only what the emitted window shows. **Two** residuals are admitted, both of the same shape — a changed line whose disqualifying context lies OUTSIDE the emitted hunk:

1. **Raw string.** A changed blank or `//`-shaped line that actually sits inside a Go raw-string literal whose backtick delimiters lie entirely outside the emitted context.
2. **cgo preamble.** In a file that uses cgo, the ordinary `//` lines immediately preceding `import "C"` are compiled C code (e.g. `// int foo() { return 1; }`). Such a line is not a directive — the ported `#` arm of `isGoDirectiveComment` catches only the `#cgo`/`#include` preprocessor forms, not plain C declarations — so when the `import "C"` lies outside the emitted context, a change to it is admitted as an ordinary comment. The `cgo_import_in_section` scan closes the case where the import IS emitted; it cannot close the case where it is not. Exploiting this additionally requires a pre-existing `import "C"` file: introducing one is itself a non-comment change the detector refuses.

Each costs one vacuous satisfaction of `tests_added_or_updated` **per occurrence** — neither is bounded at one ever, and this document does not claim they are. Both are strictly narrower than the already-accepted #610 docs-only vacuous branch; the human plan gate (including its `--comment-only` override) and the implement review still apply to any such change; and closing them needs file contents — a runner-side AST or lexer signal, where the working tree exists — which neither a patch nor the trace bundle carries. `commentonly_test.go` pins each as a named KNOWN FALSE POSITIVE.

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

- #2660 — the comment-only Go exemption, its plan-gate sibling (`plan_missing_required_tests`), and the runner mirror.
- #1886 / ADR-059 — substance-aware `verification_reported` (workflow-v1.5).
- #610 / #601 — the `tests_added_or_updated` heuristic and its docs-only scoping.
- #297 / #251 (ADR-017) — deferred outcomes and branch protection.
- #283 / #247 / #233 — constraints cache, always-emit, audit payload shape.
- #963 / #1205 / #804 / #802 — gate evidence, superseded verify runs, verify summary, committed-tree gate.

## `diff_coverage` semantics (fail-closed, #1888 / ADR-059)

`diff_coverage` is a workflow-v1.6 post-hoc constraint KIND (a sibling of `max_files_changed`, not a `required_outcomes` member). The customer declares a coverage command, the report path it writes, and a minimum new-line percentage; the RUNNER executes and measures, and this package is AUTHORITATIVE for the verdict — the same division of labour `verification_reported` established.

`Constraints.DiffCoverage` is the DECLARATION (nil = the stage did not opt in, so `checkDiffCoverage` never runs and behavior is byte-identical to before #1888). `Constraints.DiffCoverageSignal` is the MEASUREMENT, derived by `backend/internal/server/trace.go`'s `diffCoverageSignalFromBundle` from the same `gate_evidence` event the verification signal reads.

| Signal | Result |
|---|---|
| `measured`, `Percent >= MinNewLineCoverage` | **satisfied** (compared with `>=`, so exactly AT the threshold passes) |
| `measured`, `NewLines == 0` | **satisfied** — the documented vacuous pass |
| `measured`, below the threshold | violation naming covered/total, the percentage, the threshold, the command + exit code, and the uncovered files |
| `failed` (command exited non-zero, no readable report, unparseable report, unresolvable base ref) | violation naming the outcome, command, exit code, report path, and the runner's reason |
| **nil** — no measurement reached evaluation time | violation (`no diff-coverage evidence in trace`) |

**Absence is a violation, not a pass.** The runner emits a signal WHENEVER the constraint is configured — a stage that added no coverable lines reports an explicit measured-with-zero result. So a nil signal unambiguously means the runner never ran or the evidence was lost, never "there was nothing to measure". An explicit zero is auditable; absence is indistinguishable from a runner that failed to run.

**Every failure detail is actionable** (#1888 condition 7): it names what ran, how it exited, and what was measured. A constraint that fails without saying why is not a usable gate.

Like `verification_reported`, it is **not deferrable** — deferring it would reconstruct the vacuous pass it exists to remove.

**Both json tags are load-bearing.** `EvaluationPayload.Applied` is the audit-payload shape the post-CI re-evaluation decodes and re-emits, so an untagged `DiffCoverage` or `DiffCoverageSignal` would silently drop the constraint or its measurement on re-eval and flip a satisfied constraint into a violation.
