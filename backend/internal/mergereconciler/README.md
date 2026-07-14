# backend/internal/mergereconciler

Merge-status reconciler (ADR-031 Phase 1 / #702): the catch-net for a missed `pull_request.closed` webhook.

## Tick loop

Each tick lists review stages parked in `awaiting_approval`, reads each run's live PR state from the GitHub REST API (`GetPullRequest` via `runs.pull_request_url`), and resolves the gate ONLY on a terminal PR state — `merged → succeeded`, `closed && !merged → cancelled` (matching the ADR-018 webhook semantics; an open PR is left parked, no force-succeed).

Resolution routes through `server.ResolveReviewFromPollState`, which delegates to the SAME `resolveReviewStageOnMerge` method the webhook handler uses — so webhook and poll are **idempotent against each other by construction** (`TransitionStage` is a no-op on an already-terminal stage; whichever fires first wins).

Skips cleanly on nil `installation_id` or nil/empty `pull_request_url` (pre-existing no-PR parked runs are untouched) and on a malformed PR URL.

The poll lacks the merger/SHA detail the webhook payload carries, so the audit row records a `merge-reconciler` system actor with empty SHAs, but the category is unchanged (`pr_merged` / `pr_closed_without_merge`) so audit consumers + the SPA render identically regardless of source.

## Resolution provider seam (ADR-031 Phase 2 / #711)

`Ticker.Resolver` is no longer `*server.Server` directly — it is a `reviewresolver.Resolver` selected at startup from the deployment-level `review.resolution` config (`--review-resolution` / `FISHHAWKD_REVIEW_RESOLUTION`, default `github_merge`).

`backend/internal/reviewresolver/` mirrors the workmgmt provider registry (`Register`/`Get`/`Registered` + a `Select(name)` helper that defaults the empty string to `github_merge` and **fails closed** on an unknown name — an `UnknownResolverError` fails fishhawkd startup rather than silently defaulting, so a misconfigured resolver cannot mask a deployment error).

The `github_merge` logic is registered as the first provider via a `reviewresolver.Func` adapter wrapping `srv.ResolveReviewFromPollState` (no import of `server` into `reviewresolver`, no cycle), so the default path is byte-for-byte unchanged and **`succeeded` still means a verified GitHub merge — there is no force-succeed path**.

Because the `Func` adapter is not `*server.Server`, serve.go wires the optional `LineageReverifier` / `AuditCheckRepublisher` / `DriveObserver` / `BoardTransitionHealer` capabilities explicitly rather than relying on `Tick`'s Resolver type-assertion upgrades.

## Run completion (#727)

`resolveReviewStageOnMerge` calls `Orchestrator.Advance` after the review-stage transition so the RUN itself reaches terminal (`succeeded` on merge, `cancelled` on closed-unmerged) — transitioning only the stage left a merged run stuck `{review succeeded, run running}` forever.

Both webhook and poll inherit the fix since they share the resolver; the call is nil-guarded and best-effort (an Advance error logs without rolling back the stage transition or audit row). The audit-only (`reviewStage == nil`) routine_change shapes are intentionally left without an Advance call.

## Merge-resolution lineage re-check (ADR-035 second line of defense / #862, beyond #858's report boundary)

On a verified `pr.Merged` state ONLY, the reconciler calls the optional nil-safe `LineageReverifier` (wired to `server.ReverifyBranchLineage`, satisfied by `*server.Server`) BEFORE resolving the run succeeded.

`ReverifyBranchLineage` reuses the side-effect-free `detectForeignCommitOnBranch` core extracted from the #858 `verifyBranchLineage` guard (`backend/internal/server/lineage.go`), seeding the reported-head ledger with `""` so the live branch tip is NOT auto-whitelisted into the set it is checked against (the report-boundary caller still seeds with the current head to bootstrap a not-yet-audited PR-open head).

The ledger is **decomposition-aware (#1038)**: each child's `child_pushed`/`fixup_pushed` entries land on the CHILD's own audit chain, so for a decomposition parent the ledger also unions in the heads reported by the parent's child runs (enumerated via the `decomposed_from` linkage) — a cleanly merged fan-out re-verifies `clean=true` and terminalizes instead of false-flagging sibling commits as foreign and parking the parent forever, while a commit with NO such provenance still flags.
A child-enumeration or per-child chain-read error marks the ledger incomplete (fail open here; the #867 reset classifier fails closed on the same condition).

A foreign commit on the merged run branch refuses the resolve — the run is left **parked/flagged** (NOT auto-succeeded, NOT auto-failed; remediation is #867) — and the shared `emitForeignCommitInvariant` writer (used by both the #858 stage-failing path and this detect-only path, so attribution is defined once) records a `foreign_commit_on_branch` `invariant_violation` audit entry (`stage_id` empty — no producing stage at merge time) plus a `lineage_violation` notify.

The emit is **idempotent**: because the parked run is re-polled every tick, `ReverifyBranchLineage` dedups on an already-recorded entry with the SAME offending+head SHA (skipping the re-emit + re-notify but still returning `clean=false`), so a contaminated merge doesn't spam the audit chain; a genuinely different foreign commit still emits.

Fail-open throughout (unresolvable anchor / absent client / CompareCommits error / incomplete ledger → `clean=true`), so a legitimately-merged run is never wrongly refused. A nil `LineageReverifier` preserves the pre-#862 behavior (resolve every verified merge with no re-check).

**Honest limitation**: this observes a merge GitHub has ALREADY performed, so it refuses to mark the run succeeded and flags loudly rather than physically blocking the GitHub-side merge; the pre-merge open-PR window is covered by the periodic sweep (#868). The cancelled (`closed && !merged`) branch lands nothing and is not re-checked.

## Audit-check republish heal (#973)

Each tick also calls the optional nil-safe `AuditCheckRepublisher` (wired to `server.RepublishAuditCheck` → `recomputeAndPublishAuditComplete`) once per parked review stage, BEFORE the merge poll so a GitHub poll failure cannot also skip the heal — the `fishhawk_audit_complete` Check Run publish surfaces are otherwise one-shot best-effort, so a transient GitHub failure (the #971 401) permanently dropped the merge-gate check until the next SPA visit or webhook.

The publisher's dedup cache records only on SUCCESS, so the sweep retries exactly the dropped publishes and an already-published state is a no-op; a PERSISTENT failure streak (5 consecutive `CreateCheckRun` failures per `(run_id, head_sha)` episode, #993) surfaces on the run record as a chained `audit_check_publish_degraded` audit entry, paired with `audit_check_publish_recovered` on the eventual successful publish.

See `docs/architecture/audit-complete.md` § Reconcile sweep.

## Configuration and caveats

Off by default; enable with `--enable-merge-reconciler` plus `--merge-reconciler-interval` (default 60s).

**Rate-limit caveat**: each tick makes one synchronous `GetPullRequest` per parked review stage with no per-stage cooldown (unlike reactionpoller's adaptive cadence), plus up to one more inside the audit-complete recompute (the auditcomplete PRHead rule) — up to 2N REST calls per tick for N parked stages; acceptable at v0 scale, but tune `--merge-reconciler-interval` upward at scale to stay within GitHub's 5,000/hour per-installation REST budget.

**Operator-loop shift (ADR-031 Phase 1)**: the MCP `fishhawk_run_stage` tool defaults `push_and_open_pr` to **true** (the input field is `*bool`; nil → true, explicit false honored), so the runner commits + opens a PR before the review gate rather than leaving the operator to commit. A scope_drift recovery is therefore a follow-up commit on the PR branch, not a `git add` before a local commit.
