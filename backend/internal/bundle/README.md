# backend/internal/bundle

Backend-side reader for the runner's gzipped JSONL trace bundle.

## Trace bundle reader

`ReadEvents` parses gzipped JSONL bundle bytes.

`ExtractDiff` returns the `policy.Diff` carried in the runner's `git_diff` event — including the optional unified-diff `patch` text (size-capped, redacted; surfaced to the implement-review prompt, never read by the policy engine; #585) alongside the name-status `ChangedFiles`. Older bundles without the field decode to an empty `Patch`.

**Last-write-wins (#870)**: when a bundle carries more than one `git_diff` event, `ExtractDiff` returns the **LAST** one — the authoritative diff both the implement review and the policy re-eval read is the runner's FINAL scope-only diff (post-drift-exclusion, post-verify-reinvoke).
A verify-fix loop (#651) that reinvokes the agent rewrites in-scope files AFTER `computeAndEmitDiff` emitted the first `git_diff`, so the runner re-emits a fresh reconciled scope-only `git_diff` (`reemitScopedGitDiff`) after the loop and the backend reads the last-emitted event.
Every bundle without a reconciling reinvoke carries exactly one `git_diff`, so last == first and the change is a no-op for them.

`ExtractScopeDrift` returns the `undeclared` path list from the runner's `scope_drift` `policy_event` (or `(nil, nil)` when absent — drift is the exception, not an error), surfaced to the implement-review prompt so an operator-stageable drifted file is not false-rejected as missing (#695).

`ExtractGateEvidence` returns the digested gate results from the runner's synthesized `gate_evidence` event (#963) — verify run/summary outcomes with bounded pre-redacted tails, flake retries, scope facts (including the per-path A/B drift categories, #991; nil on older bundles), constraint violations — or `ErrNoGateEvidence` when absent (older bundles / no-gate stages; fail-open like `ErrNoHeadSHA`), surfaced to the implement-review prompt's "Gate evidence" section.

`ExtractTiming` returns (startedAt, endedAt, ok) from the first and last non-manifest, non-trailer events — used by `emitRuntimeObserved` to compute actual stage duration.

Hand-rolled rather than importing `runner/internal/bundle` because the modules are separate; the read-side is small enough that duplication beats promoting bundle to a shared module.
