# backend/internal/server: Run branches, merge candidates and the merge queue

Split out of `backend/internal/server/README.md` (the index) on 2026-10-29 so concurrent runs stop colliding on one file. Section text is moved verbatim.

## Run-less `fishhawk_audit_complete` publish + App-identity discriminator (`pullrequest_synchronize.go` / `webhook.go`, E64.43 / [#3160](https://github.com/kuhlman-labs/fishhawk/issues/3160))

`republishOnPullRequestEvent` (renamed from `republishOnSynchronize`) is the `pull_request` webhook consumer that re-drives the audit-complete Compute + publish flow. Two changes here.

**Widened dispatch.** `webhook.go` routes `opened`, `reopened` AND `synchronize` (`isAuditRepublishAction`), not `synchronize` alone. A Dependabot PR that is never pushed to after opening emits NO `synchronize` at all, so under the old condition it received no publish whatsoever — and once the check is marked Required in branch protection, it stayed blocked forever on a context nothing would post. The run-BEARING path is idempotent under the extra events: the publisher's per-`(forge, repo, head_sha)` dedup cache suppresses a repeat at the same head. No other action is routed; `edited` / `labeled` must NOT reach the handler, and that negative is pinned by the same table that pins the positives.

**Run-less publish.** When `ListRuns` returns zero runs the handler now publishes a terminal `neutral` not-applicable Check Run (`auditcheckpublisher.PublishNotApplicable`) instead of no-oping, so the required context is satisfiable on every PR class. The handler takes `webhook.Event` rather than raw bytes because it needs `Sender` / `Repo` / `InstallationID`, which the raw-bytes signature does not carry; the credential scope is `forge.FromGitHubInstallationID(ev.InstallationID)` (`webhook.Event.credentialScope` is unexported).

### The App-identity discriminator — FAIL CLOSED

The publish is gated on `authoredByFishhawkApp`, which must **positively establish** the PR was neither opened nor pushed by Fishhawk's own GitHub App. Without it there is a false-green race: an `opened` webhook for a Fishhawk-managed PR can arrive before `runs.pull_request_url` is denormalized, `ListRuns` returns zero, and a `neutral` check would green an audit gate that was never verified. A stranded `in_progress` fails SAFE under a required context; a stranded `neutral` fails **OPEN**.

- The App's own bot login is resolved through the existing memoized `resolveAppBotIdentity` (`prompt.go`), whose `name` return IS `<app-slug>[bot]`, derived from the App's own `GET /app` slug. **No login literal is hardcoded** — a self-hosted install running under a different App resolves ITS OWN slug.
- The comparison is case-insensitive against **BOTH** the PR author (`pull_request.user.login`) and the event sender. The AUTHOR covers a Fishhawk-opened PR later reopened or pushed by someone else; the SENDER covers a fix-up push our App makes to a PR whose run row is not yet denormalized. A match on either means **no** not-applicable publish.
- **An UNRESOLVABLE identity also means no publish.** `authoredByFishhawkApp` returns `(isOurs, resolved)`; a false `isOurs` under a false `resolved` is "unknown", never "not ours". The caller logs at **WARN** — `zero runs but App identity unresolvable — not publishing not-applicable` — which is what makes the residual operator-visible. The trade is deliberate: a missing publish leaves the PR blocked (today's behaviour, recoverable with `gh pr merge <n> --admin`), while a wrong publish greens a real audit gate silently and is not recoverable.
- The `ListRuns` **error** path is unchanged and equally fail-closed: an error is not evidence of zero runs, so it logs and returns without reaching the publish.

**Residual, stated not asserted away.** `resolveAppBotIdentity` needs BOTH `GetApp` AND `GetUser` to succeed (it builds a commit email as well as a login), so an App whose `GetApp` works but whose `GetUser` fails yields an empty login and — under the fail-closed rule — **no not-applicable publishes at all**, leaving non-Fishhawk PRs blocked. A narrower `GetApp`-only resolver would shrink that residual but needs new memo fields on the `Server` struct. Resolution memoization is success-only, so the steady-state cost is one round-trip per daemon start and a transient failure is retried on the next event.

**State alternation (reviewer-raised residual, not foreclosed by a contract).** The publisher's dedup cache is keyed on state VALUE, so a not-applicable publish is not suppressed by an earlier real pass at the same head — and GitHub surfaces the NEWEST check run of a given name. In principle, a zero-run event arriving at a head where a real pass was already cached would post a `neutral` row that visually supersedes the verified pass. Two things make it unreachable in practice rather than merely unlikely: the ListRuns-error path never publishes, and the discriminator refuses any PR our App authored or pushed — so reaching it requires a PR that HAD a run when the pass was computed and has zero runs on a later event, with a non-Fishhawk author AND a non-Fishhawk sender. That is not a contract, so it is named here rather than left unstated. If it is ever observed, the fix is to skip the not-applicable publish when the dedup cache already holds a real state for that head.

**Durable follow-up.** The structurally stronger fix is a head-SHA-indexed run lookup, but `run.ListRunsFilter` carries no head-SHA field, so adding one touches `run/repository.go`, `run/postgres.go`, `queries.sql` and the sqlc-generated `db/` package — outside this change's scope. The App-identity discriminator is the shipped control.

### What the tests pin

Per-layer handler tests drive the REAL `republishOnPullRequestEvent` against a real `auditcheckpublisher.Publisher` over a recording forge fake and an `appIdentityGetterOverride` stub: foreign author → EXACTLY ONE neutral publish across all three actions; own-App author → ZERO; own-App sender → ZERO; unresolvable identity (`GetApp` errors, `GetUser` errors, GitHub entirely unwired) → ZERO in all three; `ListRuns` error → ZERO; malformed payload / empty PR URL / empty head SHA / unparseable repo / zero installation id → ZERO; and the unaffected-path pin, a PR WITH a run publishing its computed state and never the not-applicable conclusion.

`TestWebhook_PullRequestActionRouting_NotApplicable` is the CROSS-BOUNDARY test: it POSTs a request the test signs itself, in-process, to the real `POST /webhooks/github` route, so signature verify → `webhook.ParseEvent` → the dispatch condition → the handler → the publisher → the forge fake all execute (no live forge, no live delivery). It is not redundant with the per-layer tests: with the dispatch narrowed back to `synchronize`, every per-layer test still PASSES and only this table goes red — the seam the units structurally cannot cover.

## Safe CI re-trigger (`retrigger_ci.go`, E83.49 / [#4082](https://github.com/kuhlman-labs/fishhawk/issues/4082))

`POST /v0/runs/{run_id}/retrigger-ci` (`fishhawk_retrigger_ci`) re-runs the run PR's completed, non-successful `pull_request` / `pull_request_target` / `push` GitHub Actions workflow runs at the PR's CURRENT head through the Actions re-run API. It makes no PR write, no ref write and no commit — the only writes are the re-run POSTs and one `ci_retriggered` audit entry — so it is the safe alternative to closing and reopening a run's PR. `workflow_dispatch` runs (Fishhawk's own runner dispatches) are never re-run. Auth mirrors `vouch-commit` (operator `write:stages`; a run-bound token is refused `run_token_forbidden`); a closed PR refuses `409 pull_request_not_open`, a head with no CI run refuses `409 no_ci_runs_at_head`, and all-failed re-runs answer `502 retrigger_failed` with no audit entry. Full error table: `handleRetriggerCI`'s doc comment and `docs/api/v0.md`.

## PR-reopen revive (`pullrequest_reopen.go`, E83.49 / [#4082](https://github.com/kuhlman-labs/fishhawk/issues/4082))

Closing a run's PR without merging cancels the run (`resolveReviewStageOnMerge`, ADR-018 / #316): the review stage goes `awaiting_approval → cancelled` and the orchestrator's Advance resolves the run to `cancelled`. Before #4082 a reopen did not undo that, so the natural "close and reopen to re-trigger CI" move destroyed the run. Now a QUICK reopen at the SAME head revives it, and the safe CI re-trigger is `fishhawk_retrigger_ci` (`retrigger_ci.go`), which never touches PR state.

**Close-row inputs.** The `pr_closed_without_merge` payload carries two additive fields, set by `resolveReviewStageOnMerge` itself on the closed arm so the webhook, the merge-reconciler poll and the GitLab MR path all record them: `run_state_at_close` (the run row's state when the close was handled, before its Advance cancels it) and `review_state_at_close` (the review stage's state before the cancel transition; empty with no review stage). Rows written before #4082 lack them, so a reopen whose close predates the deploy refuses `close_did_not_cancel_run` (fail closed).

**Routing.** `webhook.go` routes `pull_request` + `reopened` to `handlePullRequestReopened` AHEAD of the `isAuditRepublishAction` block, so the audit-complete republish the same delivery triggers recomputes against the revived run. Only `reopened` reaches it (`TestWebhook_PullRequestReopenedRoutesToRevive`). The GitLab MR `reopen` action is NOT routed here; `next_actions` therefore offers a GitLab run only the fresh-run fallback.

**Candidate gate.** The run must be `cancelled` AND its review stage `cancelled`. Anything else — a live run (a redelivered or out-of-order reopen), no review stage, a review not cancelled, an unmanaged PR, an unreadable stage list — logs and returns with NO audit entry.

**Guards**, in order; the first that fails appends ONE `run_revive_on_reopen_refused` entry `{pr_url, reason, detail, reopened_by, reopen_head_sha}` (actor system, internal only) and the run stays cancelled:

| | `reason` | refuses when |
|---|---|---|
| a | `audit_unreadable` | a `pr_closed_without_merge` / `run_revived_on_reopen` / `acceptance_scenario_retirement_dropped` read (or the reported-head fallback read) fails |
| b | `close_did_not_cancel_run` | no close row records a non-empty, non-terminal `run_state_at_close` (an operator- or budget-cancelled run whose PR closed afterwards; a legacy row), or the newest such row is already followed by a `run_revived_on_reopen` (the current cancel came from elsewhere) |
| c | `review_not_parked_at_close` | that row's `review_state_at_close` ≠ `awaiting_approval` |
| d | `window_elapsed` | more than `reopenReviveWindow` (10 minutes) since that row's Timestamp — one clock domain: fishhawkd stamps the row and measures the window |
| e | `close_head_unrecorded` | the row's `head_sha` is empty (the poll path) and `latestRunHeadSHA` resolves none |
| f | `head_changed` | the reopened PR's `head.sha` ≠ that recorded head |
| g | `acceptance_retirements_dropped` | an `acceptance_scenario_retirement_dropped` row with `cancel_source` `stage_cancelled` sits at or after the anchor — reviving would contradict it (`acceptance_retirement_cancel.go`) |
| h | `stage_in_flight` | any other stage is `dispatched` or `running` |
| i | `reviver_unavailable` | `RunRepo` does not implement `run.RunReopenReviver` |
| j | `revive_transition_refused` | `ReviveRunOnReopen` returned an error (it re-checks both states under row locks) |

**Success.** `run.RunReopenReviver.ReviveRunOnReopen` re-parks the review at `awaiting_approval` and reopens the run to `running` in ONE transaction; the handler appends `run_revived_on_reopen` `{pr_url, reopened_by, head_sha, closed_at, closed_by, elapsed_seconds, window_seconds, review_stage_id}` (actor user = the reopener) and marks it operator-visible (`notifyOperatorVisible`; the anchor line is "<actor> reopened the PR; the run was revived to its review gate"). There is NO orchestrator Advance: like `ReviveRun`, the revive re-parks only, so an acceptance stage the cancel left pending is not dispatched behind the operator's back. The revived run resolves through the ordinary merge / close paths.

**Residuals.** fishhawkd assumes no webhook ordering: a reopen processed before its close is a not-a-candidate no-op, leaving the run cancelled with the PR open (recovery: the fresh-run fallback `get_run_status` names under `cancelled_pr_closed`). An operator cancel issued AFTER a PR close is not detected. The SPA run narrative (`frontend/src/run-narrative/narrative.ts` `MERGE_CATEGORIES`) still shows a revived run as "closed without merge" until it merges; the issue-comment anchor is the authoritative surface meanwhile.

**Tests.** `pullrequest_reopen_test.go` is a table over one all-guards-pass baseline, one row per refusal reason plus the not-a-candidate and success rows (9 vs 11 minutes pins the window); `pullrequest_reopen_pg_test.go` drives signed close → reopen → merged-close webhooks over real Postgres, the production repositories and a real orchestrator, plus an 11-minute-old close that stays cancelled with `window_elapsed`.

## Default-branch revert detection (`merge_outcome.go`, E82.2 / [#3779](https://github.com/kuhlman-labs/fishhawk/issues/3779))

`webhook.go` routes every `push` delivery — LAST, after the other consumers and outside the dispatcher's bot-sender skip — to `observeDefaultBranchPush`, which hands a default-branch push to `mergeoutcome.RevertObserver` (long-form contract: `backend/internal/mergeoutcome/` — package doc, `revert.go`, and the package README). A revert merged by the GitHub App is still observed.

**Execution model (operator condition 2).** The route never influences the 202. It returns early unless `cfg.GitHub`, `RunRepo` and `AuditRepo` are all wired — `cfg.GitHub` is nil-checked as the CONCRETE `*githubclient.Client` before it is wrapped in the `RevertForge` interface (typed-nil trap) — parses the payload, returns silently — BEFORE taking an observation slot — unless the push is to the default branch and carries at least one revert signal (`IsDefaultBranchPush` + `HasRevertSignals`, zero forge calls, #3990), then runs `ObservePush` (which keeps both checks as defence in depth) on its own goroutine under `detachedPushContext`: `context.WithoutCancel` plus a 60s `pushObserveTimeout`, so the observation outlives the request and never delays the delivery. `runDetachedPushObservation` bounds concurrent observations at `maxConcurrentPushObservations = 4` (a process-wide semaphore): a delivery arriving with every slot held is SKIPPED with a WARN (`concurrent observations saturated`), never queued, so the webhook never blocks — that revert stays unrecorded (the loss residual below). A panic inside an observation is recovered, logged at ERROR (`push observation panicked; dropped`) and the observation dropped; the goroutine sits outside net/http's per-request recovery, so it would otherwise take fishhawkd down. There is no shutdown wait: an observation in flight at exit is lost. The observer resolves at most 50 revert signals per push (`DefaultMaxRevertSignals`; past that it logs `revert_signals_truncated`) and gives each forge call 3 attempts with exponential backoff, retrying only network errors and 5xx (the effective bound with the client transport's own retries: `backend/internal/mergeoutcome/README.md` § "Forge retry bound"). `pushObservers` (a package `sync.WaitGroup`) is the test seam that waits for in-flight observations.

**What gets recorded.** Commit text is a POINTER only: the `This reverts commit <sha>` trailer resolves through `ListPullRequestsForCommit`, the `Revert "... (#N)"` subject through the run whose `pull_request_url` is `<repo html_url>/pull/N`; every candidate is confirmed by `GetPullRequest` (merged, a `merge_commit_sha`, and on the trailer path that SHA must start with the trailer SHA). Each confirmed run gets ONE chained `run_merge_reverted` row (system actor `merge-outcome-observer`, deduped atomically on `reverting_commit_sha`) carrying an `attestation` computed from the forge's file-level diffs (`githubclient.GetCommitFiles` on both the reverting and the merge commit): `inverse_diff` (every merge file inverted), `partial_inverse` (a strict subset, each inverted) or `signal_only` (anything else, including an unavailable or truncated file list, or a file entry missing its counts). **The beta projection must count only `inverse_diff` or `partial_inverse` as a revert outcome**; a `signal_only` row is a recorded pointer the diffs do not corroborate. No commit message, PR title or body is recorded.

**Same-GUID redelivery does NOT reach the observer.** `handleWebhook` calls `WebhookDeliveries.Mark(deliveryID)` BEFORE any routing, and the production store is Postgres-backed (`webhook.PostgresStore`, persistent across restarts), so a redelivery with the same `X-GitHub-Delivery` is acknowledged 202 and dropped (pinned by `TestWebhookPush_SameDeliveryID_NeverReachesObserver`). **Loss residual:** an observation that fails after the 202 — forge outage beyond the 3 attempts, the 60s timeout, a saturated observation bound, a recovered panic, or a `fishhawkd` restart mid-observation — cannot be recovered by redelivering it from GitHub's UI; that revert stays unrecorded. A delivery GitHub never made (fishhawkd down, a local install without the relay) is lost the same way. A replay under a FRESH delivery id reaches the observer and is absorbed by the audit-layer dedup (`TestWebhookPush_ReplayUnderNewDeliveryID_RecordsOnce`).

**Detection residuals.** A revert whose message carries NO signal (no trailer, no `Revert "... (#N)"` subject) is not detected — the operator files a follow-up for signal-less inverse-diff scanning. A revert-of-revert subject without a trailer is deliberately skipped. Only the first page of a commit's files is read (GitHub's 300-file page), and a truncated list attests `signal_only`.

**What the tests pin.** `merge_outcome_test.go` is the CROSS-BOUNDARY test: a signed `push` through `s.Handler()` → the observer → a REAL `*githubclient.Client` against an httptest GitHub mux → pgtest-backed run and audit repositories → the committed row (`TestWebhookPush_RevertOfRunMerge_RecordsRunMergeReverted`; deleting the `push` route reddens it). Siblings pin zero GitHub calls for an unrelated commit and a non-default branch, the bot-sender path, the detached execution (a BLOCKED forge lookup still answers 202, and the observation survives the request context's cancellation), the 60s bound, the panic recovery and slot release (`TestRunDetachedPushObservation_PanicRecovered`), the saturation skip (`TestRunDetachedPushObservation_SaturatedSkipsWithWarn`), the pre-scan before slot acquisition (`TestObserveDefaultBranchPush_NoForgeWorkPush_TakesNoSlot`: with every slot pre-held, a non-default-branch and a signal-less push log no saturation WARN, while a control revert push does), and the unwired-forge early return. The per-control observer tests live in `backend/internal/mergeoutcome/revert_test.go`.

## Run-branch operator-vouch remediation (ADR-035 / #1044)

`vouch.go::handleVouchCommit` — route
`POST /v0/runs/{run_id}/vouch-commit`, MCP verb
`fishhawk_vouch_commit`. The operator-gated, audited provenance path
for a foreign commit on a run branch that no loop-native remediation
can route — e.g. an operator's mechanical remediation commit (a
`scripts/sync-schemas` output pushed onto a fan-out branch whose
children are all terminal with zero open concerns).

Distinct from reset (#867, which DROPS an on-top commit): vouch
**KEEPS** the operator commit and attributes it.

Mechanics:

- The handler appends an `operator_commit_vouched` audit entry
  (operator/`ActorUser` actor; payload
  `{run_id, vouched_sha, reason}`, with `vouched_sha` keyed on the
  shared `lineageVouchedSHAField` constant — an alias of
  `auditcomplete.VouchedSHAField`, as `CategoryOperatorCommitVouched` is
  of `auditcomplete.CategoryOperatorCommitVouched`; auditcomplete owns
  the literals because it cannot import this package).
- The entry has TWO readers, and the union flows to BOTH ledgers:
  - `lineage.go::buildReportedHeadLedger` unions vouched SHAs
    (`lineageVouchLedgerCategory`, read via `addVouchedSHAs` from the
    `vouched_sha` field — parallel to `addReportedHeads`/`head_sha`) into
    the reported-head ledger on the run's OWN chain AND inside the
    per-child decomposition loop. The union therefore flows automatically
    to BOTH the #858 report-boundary check (`verifyBranchLineage`) and
    the merge-resolution re-check (`ReverifyBranchLineage`) with no
    caller edits — un-wedging the run an operator commit had parked.
  - `auditcomplete.gatherForeignCommitInputs` (rule 5 of
    `fishhawk_audit_complete`, the `foreign_commit` known set) unions the
    same `vouched_sha`s on each walked run's own chain AND its
    decomposition children (#3415). This reader was added AFTER the
    lineage ledger without the union, so a vouched head attributed cleanly
    under ADR-035 while the re-post below recomputed it as
    `foreign_commit` and stamped a FAILURE at the vouched head — the
    #3415 symptom. The issue title blamed the write path ("does not union
    the sha into the reported-head ledger"); the write path was fine, the
    second reader was blind.
- The runner's **conflict-resolution merge commit** flows to both readers the
  same way (#3673): `buildReportedHeadLedger` admits
  `auditcomplete.CategoryConflictResolutionPushed` to
  `lineageLedgerCategories` AND `lineageChildLedgerCategories` (the merge commit
  is in the ordinary `head_sha` field, so `addReportedHeads` reads it with no new
  decoder), and `auditcomplete.gatherForeignCommitInputs` unions the same
  `head_sha`s via `addConflictResolutionHeads` on each walked run's own chain and
  its decomposition children. Rule 5 was again a reader that would otherwise have
  omitted the union — see "Bounded conflict-resolution pass" above for the
  precedence decision and the collateral readers.
- The fan-in INTEGRATION categories now flow to both readers the same way
  (#3429). `buildReportedHeadLedger` and `gatherForeignCommitInputs` each
  union `slices_integrated` `integration_commit_shas` + `integration_commit_recorded`
  `merge_sha` on the decomposed parent's OWN chain, via the shared seam
  constants `auditcomplete.CategorySlicesIntegrated` /
  `CategoryIntegrationCommitRecorded` (aliased here as
  `lineageIntegrationLedgerCategory` / `lineageIntegrationCommitCategory`, so a
  rename on either reader is a compile-time drift). Rule 5 was again the
  later reader that omitted the union, so a post-PR-open integrate-wave
  advanced the live head to a fan-in merge the ledger accepted while rule 5
  stamped `foreign_commit`; the union is gated on the run having decomposition
  children so a non-decomposed recompute pays nothing.
- **Re-posts the `fishhawk_audit_complete` check at the vouched head
  (E64.14 / #3109).** After the `operator_commit_vouched` entry is
  durable, the handler calls
  `checks.go::recomputeAndPublishAuditCompleteAtHead(runID, vouchedSHA)`
  — but ONLY once the publish bound below has confirmed the vouched sha
  IS the run's own live pull-request head — which republishes the
  required Check Run AT that commit via the publisher's head override.
  This is load-bearing for the base-advance
  wedge: an operator-pushed head appears in NO head-report audit category
  (`fixup_pushed` / `child_pushed` / `pull_request_opened`), so the
  publisher's normal `findHeadSHA` resolution would publish against a
  STALE sha — leaving the check absent from the merged head rather than
  red on it. The recompute itself reads the vouch through rule 5's known
  set (#3415, above), so the state stamped at the vouched head is a
  non-foreign one; before #3415 the re-post landed at the right sha with
  the wrong verdict. The re-post is best-effort for
  the vouch's success but its outcome is REPORTED on the response
  (`audit_check_republished` / `audit_check_republish_warning`), NOT
  swallowed — the merge reconciler's heal (`RepublishAuditCheck`) uses
  the SAME operator-vouched-blind head resolution and so cannot retry it,
  which is why the failure is surfaced and why re-invoking the vouch is
  the sanctioned idempotent retry (the publisher's dedup cache records
  only successes, so a re-vouch re-posts exactly the dropped check and
  no-ops once it is live). The sanctioned route out of a base advance is
  now `fishhawk_rebase_run_branch` (E64.23 / #3125, below), which has the
  RUNNER advance its own branch. Operator-resolve-then-vouch survives ONLY
  as the fallback for a CONFLICTING base merge once the bounded
  agent-driven conflict-resolution pass that verb now authorizes
  (202, ceiling 1 — E64.62 / #3202, below) has been REFUSED or was never
  startable, at which point it fails closed (`rebase_conflict`).

- **The re-post is BOUND to the run's own live PR head (E64.26 /
  #3129).** Read this together with the record-verbatim bullet below —
  the two properties are deliberately different. The RECORD is verbatim
  and unbounded: any sha the operator names is appended and the response
  is a 200. The PUBLISH is bounded: before republishing, the handler
  resolves the run's live pull-request head via
  `resolveVouchPublishHead` — the same determinability ladder
  `rebase_branch.go` uses (installation → `parseRepoOwnerName` →
  `parsePRNumberFromURL` → `GetPullRequest` → non-empty head sha, every
  rung an ERROR, never an empty-string-means-ok) — and stamps the check
  ONLY when the vouched sha equals that head (`shaEqualsHead`, a
  case-insensitive FULL-string compare on the trimmed spellings; an
  ABBREVIATED sha therefore takes the mismatch arm by design). A
  MISMATCH skips the publish and reports it on
  `audit_check_republish_warning` naming BOTH shas; an UNRESOLVABLE head
  fails closed the same way, naming the resolution failure. The check is
  NEVER posted at an unverified sha. Arm ORDERING is load-bearing: a nil
  `auditCheckPublisher` (dev/CLI posture) is checked FIRST and keeps its
  silent, warning-free skip, so a dev vouch is not newly warned about a
  check it was never going to post. Without the bound an operator
  carrying `write:stages` could have this run's recomputed check state
  stamped onto ANY commit in the repository — including the head of an
  unrelated pull request (the widening #3129 records).
  - **Residual, stated honestly**: the bound narrows WHERE the check is
    stamped, not what a vouch MEANS. An operator can still record a
    declaration for any sha, and the ledger union it feeds is unchanged
    — this is not a validity check on the vouched commit.

Invariants:

- **Fail-closed preserved**: the handler records the declaration
  verbatim without verifying the SHA exists on the branch, so an
  UN-vouched foreign commit still fails category-B and still blocks
  resolution. A vouch read error sets `complete=false` (the ledger
  fails open at detection, matching the head-category contract).
- **Operator-token-only by design** (the ADR-035 sole-writer
  invariant): requires `write:stages`, and a run-bound
  `mcp:run:<uuid>` token is REJECTED OUTRIGHT (`run_token_forbidden`)
  — even for its own run — mirroring the #961
  `decide_scope_amendment` guard, because an agent self-declaring
  lineage for a commit on its own branch would defeat the #797/#856
  cross-write protection the vouch must preserve.
- `operator_commit_vouched` is an internal audit kind, NOT an
  issue-comment surface (the #1067 living anchor comment projects it
  via the audit chain).

## Run-branch base advance (E64.23 / #3125)

`rebase_branch.go::handleRebaseRunBranch` — route
`POST /v0/runs/{run_id}/rebase-branch`, MCP verb
`fishhawk_rebase_run_branch`. The operator-gated, audited path for a run
branch that has fallen BEHIND its declared base. It closes the half of
#3109's Done-means that remained open: an operator no longer has to
resolve in a worktree and PUSH to a branch ADR-035 declares runner-owned.
The RUNNER (the App installation, sole writer) performs the write; the
operator authorizes it.

**MECHANISM, stated plainly because the verb's NAME is misleading.** The
forge REST API exposes NO rebase primitive, so this performs a forge-side
MERGE OF THE BASE INTO THE RUN BRANCH — `POST /repos/{o}/{r}/merges` with
`base=<run branch>`, `head=<base ref>`. It leaves a MERGE COMMIT and does
NOT produce linear history. There is no force-push. The direction is
load-bearing: the merges API's `base` is the branch that RECEIVES the
merge, so an inversion would merge the run branch into the base branch —
the worst failure available to this verb, and one no compiler and no
status code catches. `TestRebaseRunBranch_MergesBaseIntoRunBranch` asserts
it on the CAPTURED request body. Every 200 also ships a constant
`mechanism_note`, so a reader of the response cannot infer linear history.

Flow:

- **Auth is operator-token-only**, mirroring `vouch.go` rather than
  reset-branch's softer subject-binding: anonymous → 401; a run-bound
  `mcp:run:<uuid>` token → 403 `run_token_forbidden` EVEN FOR ITS OWN RUN
  (advancing a branch onto a new base is a lineage-moving write the
  sole-writer invariant reserves to an operator authorization); any
  identity without `write:stages` → 403 `insufficient_scope`, enforced
  UNCONDITIONALLY with no cookie-session bypass.
- **`confirm` must be true** (else 400 `confirmation_required`) — the
  advance moves the PR head and leaves a merge commit.
- **The determinability ladder fails CLOSED** (422
  `rebase_not_determinable`): no installation, an unparseable repo, no
  tracked PR, a `GetPullRequest` error, or an empty head sha / branch /
  base ref. Never a merge on an uncertain anchor.
- **THE BEHIND-PROBE.** "The branch already contains the base" is decided
  BEFORE any merge, by `CompareCommits(base=<live run branch head>,
  head=<base ref>)`. GitHub's three-dot compare returns the commits on
  head since its merge base with base, so zero commits ⟺ the base ref is
  already an ancestor of the run branch head. A compare error fails
  closed. Nothing here reads `MergeBranch`'s ambiguous `("", nil)` as a
  discriminator — that value is a legitimate undecodable-201 body, pinned
  benign by `githubclient`'s `TestMergeBranch_MergedMissingSHAIsBenign` —
  and `githubclient.MergeBranch`, `forge.Forge` and the GitLab stub are
  untouched by this change.
- **LEASE RE-CHECK** before the merge: the live PR head is re-read and the
  call aborts 422 if it moved since the probe. The merges API has no
  compare-and-swap, so this narrows but does not eliminate the
  concurrent-push window — the same residual reset-branch documents. The
  consequence of losing that race is benign HERE (a merge commit on a head
  one commit newer than classified), which is why a merge is acceptable
  where a force-update would not be.
- **ON CONFLICT: the budgeted 202 trigger arm (E64.62 / #3202).**
  `errors.Is(merr, forge.ErrMergeConflict)` no longer refuses outright — it
  calls `startConflictResolutionPass` (`conflictresolution_trigger.go`).
  NOTHING is written to the branch either way (no merge commit, no
  `branch_rebased` entry, no check re-post); under the ceiling the call
  answers **202** carrying `conflict_resolution_triggered`,
  `conflict_resolution_stage_id`, `conflict_resolution_pass` and the
  constant `conflict_resolution_note`, with `new_head_sha` and
  `merge_commit_sha` EMPTY by construction so a 202 is never mistakable
  for an advance. Any other merge error → 502 `rebase_merge_failed`,
  likewise nothing written. The full contract is the
  **Bounded conflict-resolution pass** section below.
- **The new head is resolved by PROVENANCE (#4199)**, not taken from one
  PR re-read: a DECODED `MergeBranch` sha is the new head unless the
  bounded re-read observed a genuine concurrent push; only on the
  undecodable-201 shape does the re-read supply the head (see
  **Post-merge read-after-write lag** below). Both are reported —
  `merge_commit_sha` (may legitimately be empty) and `new_head_sha` (the
  resolved head, and the head the check is published at).

**THE SHARED TAIL, and why the advertised retry is REAL.** Both arms —
merged, and already-contains-base — fall into ONE re-park → audit →
attribute → republish → notify path with a resolved
`(newHead, mergeSHA, alreadyUpToDate)` triple. The already-contains-base
arm is therefore a 200 that STILL re-parks the gate and republishes at the
current head. So the retry the republish warning advertises genuinely
works: invocation 1 merges and fails to publish; the operator re-invokes
as instructed; the branch now contains the base, so the probe
short-circuits the merge and the required check IS published at the
correct post-merge head. The publisher's dedup cache records only
SUCCESSES, which is what makes the re-post reachable a second time.
`TestRebaseRunBranch_PublishFailsThenReinvokeRepublishesAtHead` drives
exactly that sequence.

**Post-merge read-after-write lag (#4199).** The forge's PR head lags the
branch-ref update, so a single post-merge `GetPullRequest` routinely
returned the PRE-merge head (3 of 3 live rebases): the handler then
reported a false concurrent push AND anchored `new_head_sha`, the
`branch_rebased` row, the check re-post and the ADR-090 merge-candidate
trigger on the stale head, so every performed-merge pass settled
`head_moved`. `rebase_postmerge_read.go::readPostMergeHead` now re-reads
boundedly (an initial read plus 5 re-reads over
`defaultPostMergeHeadReadBackoff`, ~10s, ctx-aware; the
`Server.postMergeHeadReadBackoff` seam scales it in tests) and CLASSIFIES:
`converged` (read == merge sha), `read_after_write_lag` (every successful
read == the pre-merge head — the ONLY lag signature; an ancestry probe is
deliberately not used, because it would read a foreign force-push that
rewinds the branch as benign lag), `concurrent_push` (a decoded merge sha
and a head that is neither), `unreadable` (no read succeeded) and
`read_back` (no decoded merge sha, a head that differs from the pre-merge
head). `resolvePostMergeHead` then resolves the head by PROVENANCE: a
decoded merge sha IS the new head for `converged`, `read_after_write_lag`
and `unreadable`; only a genuine `concurrent_push` keeps the observed head
(the merge commit is then already superseded). The outcome ships as
`post_merge_head_read` / `post_merge_head_read_note` on the 200 and as
`post_merge_head_read` / `post_merge_observed_head_sha` /
`post_merge_read_attempts` on the existing `branch_rebased` payload. Pinned
by `TestRebaseRunBranch_LaggingPostMergeReadConverges`,
`TestRebaseRunBranch_PersistentPostMergeLag_ReportsReadAfterWriteLag`,
`TestRebaseRunBranch_PostMergeLagAnchorsMergeCandidateTriggerOnMergeCommit`
(the trigger's `expected_head_sha` and the `branch_rebased` `new_head_sha`
are the merge commit), `TestRebaseRunBranch_PostMergeReadIsBounded`,
`TestRebaseRunBranch_PostMergeReadUnreadable_AnchorsOnMergeCommit`,
`TestRebaseRunBranch_UndecodableMergeSHA_StaleReadIsNotAccepted` and the
`rebase_postmerge_read_test.go` classifier table.

**The post-merge tail runs DETACHED from request cancellation.** The MCP
client calls this verb with a 30s timeout, and against a degraded forge
(every post-merge read 5xx or slow, each with githubclient's own retry
budget) the re-read plus its backoff can outlast it. The re-read itself
stays on the request context, so a departed caller ends it early; but once
`mergePerformed` is true the handler rebinds `r` to
`context.WithTimeout(context.WithoutCancel(r.Context()),
rebasePostMergeTailBudget)` (60s) before the shared tail. On the request
context every append would fail AFTER the installation-authored merge
landed — no `branch_rebased` row and no attribution, the wedged-FOREIGN
state the attribution exists to prevent. The already-contains-base arm
keeps the request context: it performed no irreversible write. Pinned by
`TestRebaseRunBranch_CallerCancelDuringPostMergeRead_StillRecordsTheMerge`,
whose audit fake fails an append on a dead context the way pgx does.

**The degraded head read does NOT fall back to "no override" — only when
the merge sha did not decode.** On the undecodable-201 shape the re-read
supplies the head, accepted only when it differs from the pre-merge head;
if every re-read failed or stayed at the pre-merge head, the response is
200 with `new_head_sha` empty and publication SKIPPED. Publishing at no
override (or at the stale read) resolves to the pre-merge audit-recorded
head — precisely the staleness this endpoint exists to remove — so a
fallback would make the verb cause the bug it fixes. The warning names
re-invocation, and the shared tail above makes that retry real.
`TestRebaseRunBranch_PostMergeHeadReadFails_NoPublication` asserts NO
publication occurred. A DECODED merge sha with an unreadable re-read is NOT
this case: the merge commit is the head the forge reported creating, so it
is published there.

**LINEAGE ATTRIBUTION.** The merge commit is authored by the App
installation but appears in NO head-report audit category, so
`lineage.go::buildReportedHeadLedger` would read it as FOREIGN and wedge
the very run this verb un-wedged. `writeRebaseLineageAttribution` admits it
into the ledger using the SAME mechanism the vouch path uses (an
`operator_commit_vouched` entry whose `vouched_sha` field `addVouchedSHAs`
reads). This was verified by EXECUTION, not asserted:
`TestRebaseRunBranch_LedgerAttributesTheMergeCommit` drives the REAL
`ReverifyBranchLineage` recompute and goes RED without the call.
Attribution is written ONLY when THIS invocation performed the merge — the
already-contains-base arm attributes nothing, because vouching whatever
head happens to be live would silently launder a genuinely foreign
operator-pushed commit and defeat the fail-closed property that makes
reset-branch and vouch-commit meaningful
(`TestRebaseRunBranch_LedgerStillFlagsAnUnattributedForeignCommit` pins
that an unattributed foreign commit still violates).

**EXACTLY ONE SHA is ever attributed, chosen by PROVENANCE not by
availability.** The lease re-check runs only BEFORE the merge, so a foreign
push landing in the window between `MergeBranch` and the post-merge
re-read becomes the observed head. Attributing it would launder into
the ledger precisely the commit the ledger exists to catch — the same
laundering the already-contains-base arm refuses. So when the merge SHA
decoded it is the ONLY sha attributed, and a head the bounded re-read
classified `concurrent_push` (neither the pre-merge head nor the merge
commit) is treated as in-band evidence of a concurrent push: not
attributed, logged, and surfaced on the response with today's warning
sentence. A re-read stuck at the pre-merge head is `read_after_write_lag`
(#4199), NOT a concurrent push, and warns nothing.
`TestRebaseRunBranch_ConcurrentPushIntoPostMergeRead_IsNotAttributed`
seeds that race BY CONSTRUCTION (the merges endpoint returns one sha, the
subsequent PR read returns a third one) and asserts against committed
state plus the REAL recompute. `new_head_sha` is attributed alone ONLY on
the undecodable-201 shape (`read_back`), where the merge provably happened
and there is nothing else to attribute.

**An incomplete attribution is REPORTED, never silent.** The attribution is
load-bearing — without it the merge commit is classified FOREIGN and the run
stays wedged — so "best-effort with only a Warn log" would let the endpoint
return 200, publish `fishhawk_audit_complete` and leave the run wedged with
the operator told nothing. The append failure is still non-fatal to the
already-completed merge, but the response carries
`lineage_attribution_warning` in all three incomplete cases: a
concurrent-push post-merge head, a failed attribution append, and nothing
attributable at all. Pinned by
`TestRebaseRunBranch_AttributionAppendFails_WarnsAndNamesVouch` (the
failure injected in isolation on the `operator_commit_vouched` category, so
the `branch_rebased` entry still lands and the test discriminates an
attribution failure from a blanket audit outage).

**Residual, stated rather than papered over:** when the merge SHA is
undecodable AND the post-merge re-read fails or stays at the pre-merge
head, that invocation has no SHA to
attribute, and the retry invocation takes the already-contains-base arm,
which attributes nothing. Such a run needs `fishhawk_vouch_commit`, exactly
as it did before this verb existed — and the response now SAYS so rather
than directing the operator only to re-invoke this verb, which would be a
recovery instruction that cannot deliver.
`TestRebaseRunBranch_NoAttributableSHA_ThroughReinvocation_NamesVouch`
drives that sequence THROUGH the reinvocation and asserts the retry indeed
attributes nothing and the real recompute still flags the run foreign, so
the `fishhawk_vouch_commit` instruction is decidable from committed
evidence rather than from reading the handler.

Other invariants:

- **Consumes NO fix-up budget.** The verb adds no stage and writes no
  `stage_fixup_triggered` entry, so it does not spend budget meant for
  review concerns (#3109 recorded that cost).
  `TestRebaseRunBranch_ConsumesNoFixupBudget` reads the stage list and the
  counter back after the call and asserts both unchanged.
- **Bidirectional cross-links.** `reset_out_of_scope` and
  `reset_not_applicable` now name `fishhawk_rebase_run_branch` as the
  right verb for a BASE ADVANCE, and `rebase_conflict` names
  `fishhawk_reset_run_branch` for a foreign on-top commit. Both directions
  are pinned by substring assertions, because neither string is
  compiler-enforced. Since E64.62 / #3202 the `rebase_conflict` refusal is
  reached only when no conflict-resolution pass could be started, and it
  additionally names the FAILED pass, its refusal reason and the
  resolve-push-vouch fallback.
- **`reparkReviewGateAfterHeadMove`** (renamed from
  `reparkReviewGateForReset`) is shared by both handlers; its body and
  semantics are byte-for-byte what the reset path always had.

## Bounded conflict-resolution pass (E64.62 / #3202)

`conflictresolution_trigger.go` is the rebase verb's **202 arm**, and
`conflictresolution.go` (written alongside it) owns the trigger resolver and
the failure recovery. Together they replace the outright `rebase_conflict`
refusal with an operator-authorized, bounded, agent-driven pass that runs ON
the run branch — so an operator whose branch conflicts with an advanced base
never has to push to a branch ADR-035 declares runner-owned.

- **What a 202 means.** `startConflictResolutionPass` records a
  `stage_conflict_resolution_triggered` audit entry and re-opens the run's
  implement stage, and the handler answers 202. NOTHING has been written to
  the branch. The runner recognises the pass from the four
  `conflict_resolution*` prompt-response fields, performs the base merge
  LOCALLY, resolves only inside the hunks git itself marked, and pushes the
  merge commit through the App installation. Contract:
  `runner/README.md` § "Bounded conflict-resolution pass".
- **The budget is its OWN, and the separation is STRUCTURAL.**
  `conflictResolutionCeiling` is **1**, counted by `countStageEntries` over
  `stage_conflict_resolution_triggered` entries bound to the implement stage.
  The fix-up counter never reads that category and this counter never reads
  `stage_fixup_triggered`, so a pass can never spend a fix-up slot and a
  fix-up can never spend the pass. The ceiling is deliberately NOT
  operator-overridable: the pass resolves mechanically inside the marked
  hunks, so a second attempt at the same conflict has no new information and
  would only widen the blast radius. A trigger CONSUMED by a later settlement
  still counts — the pass it authorized ran.
- **CONSUMPTION is written by BOTH terminal outcomes.**
  `resolveConflictResolutionTrigger` treats a same-stage
  `stage_conflict_resolution_failed` (refused) or `conflict_resolution_pushed`
  (succeeded) entry at or newer than the trigger's sequence as spending it, and
  serves no pass thereafter. Reading only the failure category left a
  SUCCESSFUL pass's trigger live, so the next ordinary fix-up on that implement
  stage was served `conflict_resolution=true` with anchors naming a merge
  already committed and pushed — the same hijack the failure comparison closes,
  reached through the happy path. `succeedConflictResolutionPushStage` and the
  resolver share the `CategoryConflictResolutionPushed` constant so the writer
  cannot drift out of the consumption set.
- **A SUCCESSFUL pass's merge commit is ADMITTED to both reported-head readers
  ([#3673](https://github.com/kuhlman-labs/fishhawk/issues/3673)).** The pass is
  the product's own sanctioned recovery path, but neither reader of the ADR-035
  reported-head concept knew its category, so the merge commit the RUNNER pushed
  through the App installation read as FOREIGN and the operator had to
  `fishhawk_vouch_commit` it by hand (observed on run `3d660c84`). The
  `conflict_resolution_pushed` `head_sha` now enters BOTH: this package's
  branch-lineage ledger (`lineageLedgerCategories` + `lineageChildLedgerCategories`,
  read through the existing `addReportedHeads`/`head_sha` path) and
  `auditcomplete`'s rule-5 known set (`addConflictResolutionHeads`, own chain +
  decomposition children). `CategoryConflictResolutionPushed` is now an ALIAS of
  `auditcomplete.CategoryConflictResolutionPushed`, so ONE spelling covers the
  writer, the consumption set and both ledger readers and a rename on either
  side is a compile-time drift. It is a LEDGER-ONLY membership category:
  `auditcomplete.HeadReportCategoriesByPrecedence` is deliberately NOT widened,
  because that slice is a static PRECEDENCE ordering and a successful pass
  re-parks the review gate to `awaiting_approval` — so an ordinary fix-up can
  follow, and a position above `fixup_pushed` would shadow the newer fix-up head
  while a position below it would shadow the merge commit. With the union in
  place, re-invoking `fishhawk_rebase_run_branch` (whose already-up-to-date arm
  republishes at the LIVE head) re-posts a `fishhawk_audit_complete` check
  carrying no `foreign_commit` item, with no manual vouch.
- **Collateral, and deliberate: the other four readers of
  `lineageLedgerCategories` see the category too.** The list is not exclusive to
  `buildReportedHeadLedger`; it is also walked by
  `resolveNewestReportedHeadSHA` (prompt.go — `expected_head_sha` for a fix-up /
  acceptance push), `resolveAcceptanceRunBranchAndPR` (prompt.go),
  `resolveStageCumulativeEval` (trace.go — the `stageBase` for the #3029
  cumulative implement-review evaluation) and
  `resolveSecondNewestReportedHeadSHA` (trace.go — the #1725 prior-reviewed-head
  fallback). Each is either neutral or STRICTLY MORE CORRECT with the pass's push
  admitted: the first two order by (timestamp, sequence) rather than by a static
  precedence position, so after a pass the newest ledger head is the merge commit
  that IS the branch tip — previously they resolved a stale pre-pass head, which
  is a lease mismatch waiting to happen; `resolveStageCumulativeEval` takes the
  OLDEST stage-scoped entry, which is still the PR-open one, and gains the pass as
  evidence a push happened on that stage; and the #1725 fallback's "prior head" is
  genuinely the pre-merge head. This is the collateral the #3091 note below warns
  about, checked rather than assumed.
- **The failure marker is appended BEFORE the restore, and a persistence
  failure REFUSES the recovery.** Consumption lives only in the audit chain, so
  a restore that lands while the marker append fails would acknowledge the
  recovery (implement back to `succeeded`, review back to `awaiting_approval`)
  while leaving the trigger dispatchable. Marker-first inverts the exposure:
  the append error returns false, nothing is restored, the stage stays `failed`
  and the caller's normal failure path runs. The residual is deliberate and
  strictly smaller — a marker landing ahead of a restore that turns out not to
  apply spends a trigger for a pass the runner has already reported as failed.
- **ORDERING IS LOAD-BEARING, and it is APPEND-THEN-REOPEN.** The trigger
  entry is BOTH the durable budget counter and the runner's instruction; the
  re-open is what makes the local loop dispatch the stage. A failed re-open
  after a successful append leaves a spent slot and no dispatch — loud,
  fail-closed, one wasted slot. The reverse order's half-failure is far worse:
  an implement stage left PENDING with no instruction, which the loop
  dispatches as an ORDINARY full implement re-run doing arbitrary work — and
  it cannot be walked back, because `run.RestoreFixupStage` only accepts a
  `failed` stage. So the payload is made COMPLETE before any mutation:
  `PriorState` and the review stage to re-park (`openReviewStageFor`) are
  derived here, mirroring `run.FixupStage`'s own non-acceptance-mode
  selection, rather than read out of the decision afterwards. Append-then-
  reopen no longer spends the slot on a DETERMINISTIC re-open refusal: those
  are pre-flighted (next bullet). A slot is still spent, loudly, in three
  residual cases after the pre-flight passes: a precondition CHANGES between
  the pre-flight and the re-open (a concurrent writer); a repository READ
  fails inside `run.FixupStage`'s own admission, which re-reads after the
  append; or a `TransitionStage` WRITE fails in its mutation phase.
- **Re-open preconditions are PRE-FLIGHTED (#4304).** `startConflictResolutionPass`
  calls `run.FixupApplicable` — `run.FixupStage`'s own read-only admission —
  AFTER the budget-spent check (so at the ceiling the 422 still names the
  failed pass) and the implement-state check, and BEFORE the trigger append.
  Both it and the later `run.FixupStage` take ONE `reopenOpts`, so the
  pre-flight predicts exactly the call it guards. A terminal run, a review
  gate not parked at `awaiting_approval`, an unsettled acceptance stage (the
  `feature_change` shape, where acceptance runs ahead of review), or no review
  stage is refused HERE with nothing written: no trigger entry, no budget
  slot, no stage moved. `conflictResolutionPreflightRefusal` builds the reason
  — "…so NO conflict-resolution budget was spent; <precondition>" — with the
  run fix-up sentinel prefix stripped (`stripFixupSentinel`), so it never reads
  as a fix-up refusal. Only the acceptance rows, whose precondition the
  operator CAN clear, carry a re-invoke instruction: `reopenOpts` sets
  `AcceptanceBlockerTail` to `conflictResolutionAcceptanceBlockerTail`
  (", then re-invoke fishhawk_rebase_run_branch — waiting spends no
  conflict-resolution budget") in place of the fix-up's "then route the
  fix-up". The terminal-run, review-resolved and no-review-stage rows carry
  their own remedy and no re-invoke. A pre-flight READ failure refuses too
  ("could not be confirmed"): the ceiling-of-one slot is never spent on an
  uncertain state. On every pre-flight refusal `message()` words the
  resolve-push-vouch route as the FALLBACK ("If the precondition cannot be
  cleared: …"); the 422 code, error code and `details` keys are unchanged.
- **`run.FixupStage` owns the state-machine edge** (implement → pending, plus
  the review re-park on the push_and_open_pr flow) and the terminal-run guard.
  The pass counts handed to it are the CONFLICT-RESOLUTION counts, never the
  fix-up ones.
- **Fail-closed refusals, each with its own named reason**, all rendered by
  `conflictResolutionRefusal.message()` into today's 422 `rebase_conflict`:
  no run/audit repository; an unresolvable branch / base ref / head anchor; no
  implement stage; an unreadable budget count (a listing error must never read
  as zero, which the caller treats as headroom); an implement stage not parked
  at its review gate; an unmet or unconfirmable re-open precondition (the
  pre-flight above, which spends nothing); a failed trigger append; and a
  re-open that fails AFTER the append (the three residuals above, which spend
  the slot and say so: "the ceiling-of-one budget is now SPENT"). The
  ceiling arm is distinct: it names the FAILED pass and the runner's own
  refusal reason, read from the newest same-stage
  `stage_conflict_resolution_failed` entry, degrading to the count alone when
  that entry is absent or undecodable — a missing explanation never suppresses
  the refusal. Every arm names the resolve-push-vouch route
  (`conflictResolutionRoute`) and cross-links `fishhawk_reset_run_branch`.
  Because the conflict arm otherwise returns 202, the refusal is normally
  observable on the NEXT invocation.
- **Failure recovery is ordered AHEAD of fix-up recovery at BOTH chokepoints**
  — `pullrequest.go`'s `failPullRequestStage` and `trace.go`'s
  `advanceAfterFailure`. A stage can carry BOTH trigger categories (an
  ordinary fix-up earlier, a conflict-resolution pass now) and each recovery
  reads a DIFFERENT set of restore anchors from its OWN trigger payload, so
  consulting the fix-up anchor first would restore a failed pass from the
  stale one. `maybeRecoverConflictResolutionFailure` returns false whenever no
  LIVE trigger exists (including one an earlier failure already consumed), so
  an ordinary fix-up failure falls through unchanged. The trace chokepoint
  carries no runner-supplied refusal reason (it handles cat-A/B/C stage
  failures), so `stageFailureReason` passes the stage's own recorded failure
  category + reason through instead, falling back to a NAMED
  no-recorded-reason sentence rather than an empty string.
  `TestConflictResolutionRecoveryOrderedAtBothCallSites` pins the ordering at
  both sites by source position, and
  `TestAdvanceAfterFailure_ConflictResolutionRecoveryPrecedesFixup` pins it
  behaviourally by seeding deliberately DIFFERENT anchors so the restored
  state discriminates which one was consulted.

## Operator verify-only re-run of a failed merge-candidate head (#4391)

`POST /v0/runs/{run_id}/rebase-branch` with `rerun_failed_verify: true` (`rebase_branch.go` → `rebaseRerunFailedMergeCandidateVerify`, `merge_candidate_verify.go` → `voidFailedMergeCandidateVerdict`). It re-runs ONLY the ADR-090 verify of a head whose recorded verdict is `failed` (D3), with no agent and no fix-up budget.

- **Reason required.** A blank `reason` with the flag is `400 validation_failed` (`field: reason`), checked AFTER the 503 `rebase_unconfigured` guard and BEFORE the run lookup — the reason is the operator's attestation that the failure was not the change's.
- **Where it acts.** Only on the already-up-to-date arm, only when `mergeCandidateVerifyState` reads the live head `failed`. A performed merge ignores the flag (the new head gets its own pass); any other state takes today's path. Either way `merge_candidate_verify_note` says the flag had no effect.
- **Order: pre-check → void → start.** (1) The implement stage must be parked at its review gate (`succeeded` / `awaiting_approval`), or hold a queued D6 fix-up (`pending` / `awaiting_host_dispatch`, below); any other state is refused on `merge_candidate_verify_refusal` having written NO verdict and NO trigger (the shared tail's `branch_rebased` row still lands), naming the stage and its state. (2) `voidFailedMergeCandidateVerdict` appends ONE `merge_candidate_verified{not_executed, reason "operator_rerun: <reason>", voids_sequence}` row on the failed row's stage; its error is a refusal with the verdict still failed. (3) `startMergeCandidateVerifyPass`, unchanged (direct for `fan_in`, the ADR-092 queue otherwise).
- **No reader change.** ADR-090 already reads `not_executed` as re-triggerable, so the merge gate, the fan-in hold, queue admission and the gate view see the head as `unverified`. The void deliberately calls neither `mergeCandidateQueueOnVerdict` (the failed verdict already ejected the queue entry) nor `detectMergeCandidateFalseTransfers`.
- **Replay safety.** Copying `trigger_sequence` makes a late runner replay of the ORIGINAL failed report hit `recordMergeCandidateVerifiedEvidence`'s same-trigger-sequence duplicate arm (answered `not_executed`, nothing appended). A replay arriving after the NEW trigger binds to it — the exposure any stale report already has.
- **TOCTOU.** A stage leaving its gate between the pre-check and the start leaves the head voided (`unverified`) with no pass; the refusal says so, and the merge gate already routes that state back to this verb.
- **Merge gate.** The `merge_candidate_verify_failed` 409 names the re-run in its message and `details.rerun_step: fishhawk_rebase_run_branch`; `next_step` stays `fishhawk_fixup_stage`.
- **In-place conversion of a queued D6 fix-up (E86.54, `merge_candidate_rerun.go`).** At `pending` / `awaiting_host_dispatch` the verb calls `rerunQueuedMergeCandidateFixup`, which converts an already-queued, never-dispatched ADR-090 D6 fix-up IN PLACE into the verify-only pass: no stage transition, no new transition edge, no run-package change. `merge_candidate_fixup_superseded: true` on the 200 says THIS call converted it.
  - **Eligibility** (`queuedMergeCandidateFixupFor`, the ONE predicate the verb and the run-read advertisement share; every read error fails closed): (a) stage `pending` or `awaiting_host_dispatch`; (b) the newest `merge_candidate_verified` row for EXACTLY the head is `failed` (sequence F; an already-voided one is refused naming its sequence); (c) the stage's NEWEST `stage_fixup_triggered` T carries `delegated: merge_candidate_verify_failed` (the key `writeFixupAudit` writes; an operator-routed fix-up is refused naming `fishhawk_dispatch_stage`), T > F, `prior_state` ∈ {succeeded, awaiting_approval}, a parseable `reparked_review_stage_id`; (d) no stage-bound row follows T in `queuedFixupDisqualifyingCategories` — fix-up settles (`fixup_pushed`, `fixup_no_changes`, `stage_fixup_recovered`, `stage_fixup_superseded`, `dispatch_reaper_failed`), retries (`stage_retried`, `stage_override_retried`), dispatch marks (`stage_concurrency_admitted`, `installation_token_issued`), the slot queue (`stage_concurrency_queued`) and later re-opens (`stage_merge_candidate_verify_triggered`, `stage_conflict_resolution_triggered`); (e) `Concurrency.StatusForStages` holds NO queued or held row for the stage (nil store = bare CAS, nothing to read); (f) the ADR-092 pre-check `mergeCandidateRerunWouldHold`: when the queue routes the cause, ANY active entry of ANOTHER run on the base refuses up front.
  - **The fence.** `holdStageAdmission` takes the per-stage `orchestrator.LockStageAdmission` mutex host dispatch holds (#1936) across re-read → eligibility → marker → void → start, and stamps a context marker; it is re-entrant per request, so the pump admitting the run's OWN entry on the same goroutine (serialize → pump → admit → begin; `mergeCandidateAdmissionContext`'s `WithoutCancel` keeps the marker) never re-locks. No `Orchestrator` = no lock (the host_dispatch residual); the mutex is process-local (the `orchestrator.go` multi-replica residual).
  - **Lock order (approval condition C1).** While the verb holds stage S's fence, the pump on its goroutine CAN admit ANOTHER run's entry (the dropped-base pump in `serializeMergeCandidateVerifyPass` admits whatever heads that base), so an S holder can reach begin's arm for a different stage T. The arm therefore takes a fence its context does not already hold through `tryHoldStageAdmission`, bounded by `stageAdmissionTryWait` (2 s), and on a miss answers a `Transient` refusal that `admitMergeCandidateEntry` routes through its existing `unadmit` (the entry returns to held at its place for a later pump) instead of ejecting it `pass_not_startable`. It never blocks unboundedly while another stage's fence is held.
  - **Begin's SUPERSEDED-FIX-UP arm** (`beginSupersededFixupPass`, beside PENDING REPLACEMENT): with no live trigger on a `pending`/`awaiting_host_dispatch` stage whose newest D6 trigger T is named by a `stage_fixup_superseded` marker after it, it re-reads under the fence and refuses (nothing written) when the stage left its queued state, a conversion trigger already follows the marker, any OTHER disqualifying row follows T (the marker is then INERT), or the slot store holds a row. Otherwise it appends ONE `stage_merge_candidate_verify_triggered` carrying T's `prior_state` / `reparked_review_stage_id` plus `supersedes_fixup_trigger_sequence: T`, without `run.FixupStage`: the stage keeps its place, its next dispatch resolves to `local-verify:<host>` and is served the verify-only prompt, and the pass restores the gate as every verify pass does. Every producer reaches begin (the rerun, a plain rebase re-invoke, the fan-in hold, the queue admission), so a marker + void state converts on the next start for that head. Not handled falls through to the prior-state refusal byte-unchanged.
  - **Reader rule** (`confirmedFixupSupersessions`, pure): a marker M confirms only when a `stage_merge_candidate_verify_triggered` row on the same stage, sequenced after M, carries `supersedes_fixup_trigger_sequence` equal to M's `trigger_sequence`. The verify pass's own dispatch follows the marker, so "no later dispatch" cannot be the rule; only begin's arm writes that field, after proving under the fence that nothing disqualifying follows. An inert marker is never a supersession. The fix-up refund, the gate view and the MCP review floor apply this rule (the readers slice of #4391).
  - **Held coexistence.** The verb refuses up front when the base is occupied (f). If another run goes live between the pre-check and the enqueue (TOCTOU the stage fence cannot close), the start answers `held`: marker + void stand with NO conversion trigger, the fix-up is still dispatchable AS A FIX-UP and every reader treats it as live; the queue's later admission converts it through begin's arm. If the fix-up is dispatched first the arm refuses (inert marker) and the admission ejects the entry `pass_not_startable`.
  - **Partial failures** (each answer says exactly what landed): eligibility refusal → nothing; marker append failed → nothing, the fix-up still queued; void failed → the marker only (sequence named), the fix-up still dispatchable as a fix-up, and every later re-run on it is refused because its marker is spent; start refused → marker + void, no conversion trigger, re-invoke `fishhawk_rebase_run_branch` with no flag to convert; held → marker + void, converted at admission.
- **Advertisement (`merge_candidate_rerun_signal.go`).** `GET /v0/runs/{run_id}` carries the advisory `merge_candidate_rerun` block built by `mergeCandidateRerunSignalFor` (audit-first, no forge call, best-effort): at the review gate it names the failed head; at `pending` / `awaiting_host_dispatch` it calls `queuedMergeCandidateFixupFor` (including the (f) occupied-base pre-check) and adds `queued_fixup_*` only when the predicate admits, so it never offers a call the verb would refuse up front. The MCP `next_actions` APPENDS the re-run as an alternative, never the default (`backend/internal/mcpserver/README.md` § "`run.merge_candidate_rerun`").
- **Staleness (E85.4 / #4147).** The same run read also carries `merge_candidate_staleness`, which (unlike the re-run signal) reads the forge, bounded per cache entry; contract in the gate-view section's "Merge-candidate staleness block".
- **Tests.** `merge_candidate_rerun_test.go` (handler arms, the void helper, the in-place conversion T1–T8 and the bounded other-stage fence, and the ADR-092 held case on real Postgres), `merge_candidate_rerun_e2e_test.go` (real Postgres: own-entry admission under the held fence, refused on an occupied base, held after the pre-check then converted at admission, both admission-fence arms), `merge_run_test.go` (`rerun_step`).

## Operator-vouched base merges (ADR-090 D8 Amendment 1, #4368)

Rule 4 of `merge_candidate_verify.go::classifyMergeCandidateHead` (an operator-vouched head) no longer exempts unconditionally: it delegates to `merge_candidate_vouch.go::classifyVouchedHead`. The trigger was #4293 (run e16e6c42): an operator pushed a merge of `main`, vouched it, `rebase_run_branch` answered `already_up_to_date` + `not_required`, and the merge queued with no pass, so the combined tree was never product-verified.

- **Reference head** (`newestVerifiedHead`): the newest head on the run's chain, by chain sequence, whose tree the product gated or verified — the `head_sha` of a `pull_request_opened` / `fixup_pushed` / `child_pushed` row, the `head_sha` of a `passed` `merge_candidate_verified` row, and ONLY in ADR-096 `enforce` mode the `to_head` of a `merge_candidate_verdict_transferred` row (the same rollback contract as `newestMergeCandidateVerdict`). The probed head itself is never its own reference: a passed verdict for H must read `passed`, not `not_required` via an empty `H...H`.
- **Probe**: two three-dot compares, `CompareCommits(V, H)` and `CompareCommits(<PR base ref>, H)` — base first, head second in both. A commit in the first and not in the second (`bringsInBaseCommits`) is one H has, V lacks and the base already contains → `base_advance`. None → `not_required` (D8's non-merge operator edit). An empty `V...H` answers `not_required` without the second compare.
- **Fail posture:**

  | Situation | Answer |
  |---|---|
  | No GitHub client, no installation, unparseable repo, no PR number (cannot probe) | `not_required` + WARN — the pre-amendment exemption, mirroring D7 |
  | No verified reference head on the chain | `base_advance` (fail closed) |
  | Empty PR base ref | `base_advance` (fail closed) |
  | Either compare at the 250-commit cap (`vouchProbeAtCap`; the compare API does not paginate) | `base_advance` (fail closed) |
  | PR read, compare or audit read error | error returned; every reader fails closed on it (D7) |

- **Readers need no change of their own.** Every `mergeCandidateVerifyState` caller gets the behaviour: the merge gate (`merge_run.go`, 409 `merge_candidate_unverified` cause `base_advance` until a pass passes on exactly that head), the rebase already-up-to-date arm (`rebase_branch.go`, which starts the verify-only pass), the fan-in hold (`fanin_verify.go`), queue admission (`merge_candidate_queue.go`), the gate view (`gateview.go`), the verdict transfer (`merge_candidate_transfer.go`) and the `fanin_verify_held` re-run hint (`review_rerun.go`). No new cause, state, wire field or audit category.
- **Forge-read cost.** Classifying a vouched head that rules 1–3 did not catch costs one `GET /pulls/{n}` and up to two compares, on EACH of those readers (`merge_run.go`, `rebase_branch.go`, `fanin_verify.go`, `merge_candidate_queue.go`, `gateview.go`, `merge_candidate_transfer.go`, `review_rerun.go`). A read error there takes each reader's existing fail posture (the gate view records a `merge_candidate_verdict` gap; the merge gate answers 502 `merge_candidate_check_failed`; the `review_rerun.go` hint reads `unreadable`).
- **Intended widening.** A vouched edit on top of a head whose tree was never verified (a product rebase whose pass never reported, a decomposed parent before its fan-in pass) now reads `base_advance`, or fails closed when nothing on the chain was ever verified.
- **Residuals.** A base change carried in by a NON-merge commit (cherry-pick, hand copy) has a fresh SHA the base lacks, so it is not detected; a scope-bounded tree diff was rejected because it would strip D8's canonical case (an operator remediation commit outside the run's scope). An unprobeable deployment keeps the exemption. The 250-commit cap fails closed, so a very long branch pays a pass it may not need.
- **Tests.** `merge_candidate_vouch_test.go` (`TestClassifyVouchedHead`: every arm and fail mode through the real `mergeCandidateVerifyState`; `TestBringsInBaseCommits`), `merge_run_test.go` (`TestMergeRun_OperatorVouchedBaseMergeNeedsPass`, committed state), `rebase_branch_test.go` (`TestRebaseRunBranch_UpToDateTriggersPassForVouchedBaseMerge`, the trigger row read back). The two GitHub stubs serve per-pair compares (`compares` / `compareByPair`, keyed `<base>...<head>`) and fall back to their behind synthesis, so no older fixture changed.

## PR-open checkpoint resume gate (E48.46 / #2169)

`prompt.go::resolvePushCheckpointResume` is the sibling of the exempt gate above, modelled directly on it, and emits the SAME four held-commit fields plus a fifth discriminator — `held_commit_resume_kind: "pr_open"`.

- **What it recovers.** The implement agent ran, the committed-tree gates passed, and `CommitAndPush` landed the gate-verified commit on the run branch — and only THEN did the PR open (or the artifact ship) fail, typically a sustained forge outage. Before this, `retry_stage` re-ran the whole agent: a ~$4-6, ~50-minute redo of work already sitting on the branch. Emitting the held-commit fields sends the runner to the same pre-agent short-circuit `#1231` uses, where it re-attempts only the idempotent adopt-then-create `OpenPR` (#2167) — which also covers the case where the PR actually opened and only the ship failed, since the adopt-by-head arm returns the existing PR.
- **Where the checkpoint comes from.** `pullrequest.go::failPullRequestStage` records `push_checkpoint` (`{branch, head_sha, base_sha, verified_tree_sha}`) onto the `pull_request_failed` audit payload when a `{outcome:"failed"}` report carries BOTH a non-empty `branch` and `head_sha`. A partial checkpoint records NOTHING, and `validate()`'s `failed` arm is deliberately NOT tightened to reject one: a 400 there would strand the implement stage in `running` until the SLA watchdog, converting a runner bug into a hung run. No new decode field and no migration — the runner reuses `pullRequestBody`'s existing coordinates and the checkpoint rides the audit chain.
- **Category set.** The walk covers `pull_request_failed` (the carrier) plus every category that can SUPERSEDE it: `pull_request_opened`, the three `scope_completeness_*`, `fixup_pushed`, and `child_pushed`. A walk over the carrier alone would happily resume a stage that has since opened its PR, parked, or pushed a fix-up.
- **Newest-wins, and it is load-bearing.** The gate tracks the newest entry across ALL categories and the newest `pull_request_failed` separately, then requires them to be the SAME entry. That identity IS the rule (keeping the two walks separate is what stops it being incidentally satisfied), and it makes the gate SELF-INVALIDATING: a successful resume writes `pull_request_opened`, which becomes newest, so a later retry takes the ordinary agent path instead of re-opening a PR that already exists.
- **Base SHA is REQUIRED, not optional.** The success-arm `validate()` requires `base_sha`, so emitting on a base-less checkpoint would open the PR and then always fail category-B, orphaning it (the #2562/#2563 defect the runner now also refuses before the forge). Failing closed here keeps the two layers agreeing rather than leaning on the runner's last line.
- **Exempt WINS.** Wired into both handlers as the `else` of the exempt call, so an exempt-resolved park always takes precedence and the two can never both set the fields — an exempt emission therefore always carries an EMPTY resume kind. The operator's exempt decision is a judgment about the held tree; a checkpoint is a mechanical retry hint.
- **Fail-closed** on: a nil stage, a non-implement stage, a FIXUP dispatch (a fix-up exists to re-invoke the agent with the reviewer's concerns — resuming would open a PR from the UNFIXED commit and report the fix-up as done), a nil `AuditRepo`, any `ListForRunByCategory` error, no entry for the stage, a newest entry that is not a checkpoint-bearing `pull_request_failed`, an undecodable payload, and an incomplete checkpoint. The cost of a wrong emission is a PR opened from an unintended head; the cost of a wrong omission is today's agent re-run. Those are not symmetric.
- **Runner-side guards** (`runner/cmd/fishhawk-runner/README.md`): the `pr_open` kind additionally makes the runner re-verify the run branch's REMOTE TIP still equals `held_commit_sha` before touching the forge (a checkpointed commit passed no operator gate the way a park did), and re-report the checkpoint on its own failure so a multi-retry outage stays resumable.
- **Decomposition-child gating (E55.16 / #3916).** A decomposition child (`runRow.DecomposedFrom` set) must never open a standalone PR for its slice branch, so the held-commit gates apply three child-only refusals, each a TOTAL decline (no held-commit fields; the retry is an ordinary agent re-run). TWO of them go through `resolvePushCheckpointResume`'s `decline()` and carry a reason value: (1) a push-kind checkpoint served to a runner that does not also advertise `child-push-resume` (`capabilityChildPushResume`, the token certifying the runner's #4079 child arm that publishes the slice branch, reports `pushed` and never opens a PR) → `runner_child_capability_absent` — a runner advertising `push-resume` alone may predate that arm and fall into the PR-open tail, the #3910 mechanism; (2) any pr_open checkpoint on a child → `child_resume_kind_unsupported` (mirroring the runner's own refusal token). `decline()` WARN-logs the reason and, on the dispatch path only, writes the existing `verified_tree_discarded` row — and only when the checkpoint carries a `VerifiedTreeSHA` (`appendVerifiedTreeDiscarded` is inert otherwise). The reasons are payload values on that already-registered category, not new audit categories. (3) `resolveHeldCommitExemption` refuses the exempt open-PR resolution for a child; that refusal is LOG-ONLY — no reason token and no audit row. The child token never gates a non-child run. Tests: `TestResolvePushCheckpointResume_ChildPushKindRequiresChildCapability`, `TestResolvePushCheckpointResume_ChildPROpenDeclined`, `TestPushResume_ChildNotServedWithoutChildCapability` / `_ChildServedWithChildCapability`, `TestPromptHeldCommit_ChildExemptRefused`, and the real-handler done-means lifecycle `trace_test.go::TestChildPushFailure_RetryRedispatch_NeverOpensPR` (push failure → retry → re-dispatch → `pushed`, zero `pull_request_opened`, no `pull_request` artifact, nil `PullRequestURL`).

## Held-commit PR text (`heldcommitpr.go`, E67.5 / [#2570](https://github.com/kuhlman-labs/fishhawk/issues/2570))

Both resume gates above serve a runner that takes its PRE-AGENT short-circuit, so no agent runs and no PR-description handoff is written. The resumed PR therefore opened as `chore: fishhawk implement stage <id>` with no summary, no test plan and — the blocking part — no `Closes #N`, so merging never auto-closed the trigger issue. `heldCommitPRTitleBody` resolves the text the resumed runner opens with, served as `held_commit_pr_title` / `held_commit_pr_body` alongside the existing held-commit fields on BOTH `/prompt` and `/prompt-render`.

- **Why the backend owns this.** The runner's `/tmp` PR-description handoff is deleted on read and consumed by the pass that later parks or checkpoints, and on an ephemeral runner `/tmp` is gone before the retry dispatches. The park row (`run.ScopeCompletenessPark.PRTitle`/`PRBody`, additive JSONB — no migration) and the `push_checkpoint` payload are the only storage a same-host AND a fresh-host resume can both reach.
- **Recovery ladder, PER-FIELD.** Each field is resolved independently, so a recovered title is never discarded because the body was empty. Exempt gate: the park row's field, then the newest `scope_completeness_parked` audit payload's `pr_title`/`pr_body` (the legacy-row fallback, same ladder shape #2563 uses for `base_sha`, for a park written before the struct carried them). Checkpoint gate: the `push_checkpoint`'s `pr_title`/`pr_body`. Then, for whichever field is still empty, a documented synthesis from the run's cached `IssueContext` — a Conventional-Commits title (the issue title verbatim when it already matches the header shape, else `chore: `-prefixed, modelled on `orchestrator.consolidatedPRTitleBody`) and a body naming the issue and stating explicitly that the agent did not re-run. With no issue context either, BOTH come back empty and the runner keeps today's placeholder.
- **`Closes #N` is appended unless a REAL closing reference is already present.** Detection is deliberately precise in both directions. A false negative costs a duplicated line; a false POSITIVE silences the only auto-close directive and the issue stays open on merge — precisely what #2570 exists to prevent. So a match requires a GitHub closing keyword (`close`/`closes`/`closed`, `fix`/`fixes`/`fixed`, `resolve`/`resolves`/`resolved`) immediately preceding `#<n>` with the number terminated by a real word boundary, evaluated over text with inline code spans and fenced code blocks ELIDED (`stripCodeContexts`). A bare or non-closing mention (`Related to #N`, `See #N`), a longer number (`#25701`), trailing garbage (`#2570foo`), and closing-looking text inside a code span or fence ALL still get the directive appended. The elision itself must be false-positive-free, which cost two corrections: an elided inline span leaves a NON-SPACE, NON-WORD boundary behind (`codeSpanElision`) rather than joining its neighbours, so `` Closes `note` #N `` cannot collapse into the active directive `Closes  #N` that the source never contained; and only a VALID closing fence ends a block (same character, at least as long as the opener, no info string, per CommonMark §4.5), so an inner `~~~`, a three-backtick run inside a four-backtick block, or an info-string line like ` ```go ` is fence CONTENT and cannot expose the directive still inside it. Fence recognition is asymmetric on purpose: OPENING is liberal (an over-eager open only elides more text, costing a duplicate line) while CLOSING is strict (a wrongly accepted closer is the silently-un-closed-issue direction).
- **NEVER a safety precondition.** Every branch here degrades to empty text; nothing in this resolver can withhold a resume. The fail-closed controls in the two gates above are about opening a PR from an unintended HEAD, which is a different risk class. `TestPromptHeldCommitPRText_ExemptNoContextOmitsKeys` asserts both directions at once: the text keys absent, `open_pr_from_held_commit` still `true`.
- **Recording is byte-identical when absent.** `pr_title`/`pr_body` are written into the `scope_completeness_parked` and `push_checkpoint` payloads only when non-empty — those payloads are raw maps, not `omitempty` structs, so writing them unconditionally would stamp two JSON nulls onto every pre-#2570 entry. Neither is enforced in `validate()`, for the same reason as the checkpoint coordinates: a 400 there would strand the implement stage in `running`.
- **Footer ownership.** The served body EXCLUDES the Fishhawk attribution footer; the runner that opens the pull request appends it exactly once.
- **Pattern mirroring.** `conventionalCommitHeaderRe` here is the THIRD copy of the header pattern. `TestConventionalCommitHeaderRe_MatchesOrchestratorSource` pins it against `orchestrator.go`'s source literal (same module, directly readable), leaving only the runner's cross-module copy comment-guarded.
- **Partial delivery uses `Refs #N` (E83.52 / #4085).** `heldCommitPRTitleBody`'s `partial` argument — both callers pass `partialDeliveryPlan(ctx, runRow.ID)`'s boolean — swaps the `Closes #N` append for `Refs #N`: the recovered or synthesized body is passed through `neutralizeClosingReferences` (every active closing reference to the issue becomes `Refs #N`), then `Refs #N` is appended unless `hasRefsReference` finds an active one (same code-context rules as `hasClosingReference`). `partial=false` is byte-identical. A plan that fails to load reads as full delivery (`Closes #N`); the ship-time guard still re-checks the body when the resumed runner ships it. Pinned by `TestHeldCommitPRTitleBody_PartialUsesRefs` and, through the real `/prompt` handler for BOTH resume gates, `TestPromptHeldCommitPRText_PartialDeliveryServesRefs`.

## Partial delivery (E83.52 / #4085)

A standard_v1 plan may declare `delivery: partial` plus a `remaining_scope` (`docs/spec/plan-standard-v1.md`): the run deliberately ships only a slice of its triggering issue, so merging it must not close the issue. `plan.(*Plan).IsPartialDelivery` is the ONE definition every consumer reads; it does not require a non-blank `remaining_scope`, because these consumers decode with `json.Unmarshal` and skip `semanticCheck`, and failing toward partial keeps the issue open. This package owns four of its surfaces:

- **Upload refusal** — `handleShipPlan`'s `plan.Validate` runs `checkDelivery` (the same function `plan.Parse` runs), so a `remaining_scope` paired with an absent or `full` delivery, or a whitespace-only one, is refused `400 plan_invalid` through the shared `refusePlanInvalid` tail before the artifact is stored. Pinned by `TestShipPlan_DeliveryRule_RefusedAtUpload` (`partial_delivery_pr_test.go`) with `TestShipPlan_DeliveryRule_ValidPartialAdmitted` as its control.
- **Plan-gate advisory** — `partialDeliveryWarning` (`plan_warnings.go`), above.
- **Ship-time guard** — `neutralizePartialDeliveryClosingRef` (`partial_delivery_pr.go`), called from `handleShipPullRequest` on every successful implement ship, after the branch-lineage guard and OUTSIDE the `implement && running` terminal drive (so the non-gated flow whose stage the trace handler already advanced is covered). It loads the run itself; needs `IssueContext.Number > 0` (checked before the plan loads), a partial plan, and the GitHub client + an installation id (GitLab / no installation is an INFO skip). It re-reads the LIVE PR body, rewrites each active `Closes/Fixes/Resolves #N` (code spans and fences excluded, the #2570 rules, via the offset-preserving `maskCodeContexts`) to `Refs #N` with `EditPullRequest`, and only after the edit succeeds appends a system-actor `partial_delivery_closing_reference_neutralized` row `{issue_number, pr_number, rewritten}`. Every failure WARN-logs and never unwinds the ship response.
- **Merge-time remaining-scope comment** — `postPartialDeliveryRemainingScope` (`partial_delivery_merge.go`), called on BOTH merged arms of `resolveReviewStageOnMerge` (implement-only and review stage), after `writePostMergeObservedAudit` and before the economics stamp and branch sweep; never on closed-without-merge. Preconditions, each a no-post return: `IssueContext.Number > 0` (before the plan load), a partial plan, no prior `partial_delivery_remaining_scope_posted` row (the dedup; a dedup read error also skips, so a failed read never risks a duplicate), a parseable repo (`splitParentRepoRef`) and resolvable issue operations (`issueOpsFor` on the family from `splitParentForgeFamilyFromRef(InstallationRef)`, scope from `mergeObservationScope`) — forge-neutral, so GitLab runs post too. The body is a hidden `<!-- fishhawk:partial-delivery run=<id> -->` marker, the run id and merged PR URL, and the remaining scope as a blockquote passed through `neutralizeCommsProse` (planner text posted under the bot identity: no mentions, links, raw HTML, issue autolinks or forged markers). It says the remaining scope "was not delivered by this PR" and never asserts the issue stays open, so it stays true when the issue closed anyway. The system-actor row `{issue_number, pr_url, forge}` is appended only after `PostIssueComment` succeeds, so a failed post is retried on redelivery.
- **Held-commit resume** — `heldCommitPRTitleBody`'s `partial` branch, above.

Named residuals: **(a)** a later fix-up push or operator PR-body edit that reintroduces `Closes #N` is not re-checked before merge (the guard runs once, at the implement ship); **(b)** on GitLab the merge-time comment posts while the ship-time neutralizer is GitHub-only, so nothing rewrote a closing reference in the merge request description; **(c)** a closing keyword in an agent-authored COMMIT message is not rewritten (the prompt forbids it); **(d)** two genuinely concurrent merge deliveries, or an audit append failure after a successful post followed by a redelivery, can post the remaining-scope comment twice (cosmetic, mirroring `split_parent_close`). Done-means test: `TestPartialDelivery_MergedPRLeavesIssueOpenWithRemainingScopeComment` drives the real ship and merge handlers against a stateful GitHub stub; `TestPartialDelivery_FullDeliveryStillCloses` is its full-delivery twin.

## Merge-supersede sweep + `reconcile-merge` recovery (`merge_supersede.go`, E64.2 / #3083)

A merge can leave a stage parked in a state nothing will ever re-dispatch — a fix-up pass re-parks `acceptance` at `awaiting_host_dispatch`, or the human `review` gate is still `awaiting_approval` when the PR merges. `Orchestrator.completeRun`'s #968 guard then CORRECTLY refuses to stamp the run `succeeded` around a non-terminal stage, and every pre-existing escape hatch records something untrue: `reap_stage` writes `failed` for work that was never attempted, `cancel_run` writes `cancelled` for a change that shipped, and re-dispatching acceptance runs the agent against a preview bound to a commit that is no longer the change.

`superseded` (the state itself lives in `backend/internal/run`) records the fact instead: the merge made the stage UNREACHABLE. It is terminal, so the #968 guard passes it — which is exactly why what may enter it is bounded.

### The sweep primitive

`supersedeParkedStagesOnMerge(ctx, runID, skipStageID, reason)` is THE shared primitive. Both writers call it — the operator recovery endpoint here and the automatic merged-path invocation in `pullrequest_review_events.go` — so the two can never disagree about which stages a merge may terminalize.

Four properties carry the design:

- **DEFAULT DENY, and type-aware.** Admission is `run.MergeSupersedable(stageType, state)` — a `(stage_type, state)` pair table with exactly two rows. A state-only allow-list would admit a `pending` plan stage and complete a run `succeeded` around work never planned, defeating precisely the invariant #968 protects. The table is ALSO enforced at the repository transition boundary (`run/postgres.go` consults it with the ROW-LOCKED stage's own `stage_type`), so it is not advisory guidance a caller reaching the repository directly could bypass.
- **Compare-and-swap, not a bare transition.** The move goes through `run.StageCASTransitioner.TransitionStageFrom` pinned to the state the sweep classified, closing the classify→transition race atomically under the stage row lock: a concurrent writer that re-parks or fails the stage in that window is refused with a typed `run.StageStateChangedError` instead of having its state destroyed. The capability is REQUIRED, not optional — a repository without it sweeps NOTHING rather than degrading to the non-CAS `TransitionStage`, mirroring `reap_failure.go`'s refusal (#2672).
- **TRANSITION FIRST, THEN AUDIT.** The chain is append-only, so a `stage_superseded_by_merge` row written before a CAS that then refuses would be an IMMUTABLE record of a supersession that never happened. The failure mode must be a MISSING row, never a false one. `TestSupersedeParkedStages_NoAuditRowWhenCASRefuses` seeds the premise mismatch by construction (a stale stage snapshot against a moved DB row) and asserts ZERO rows.
- **Best-effort throughout.** Every failure warn-logs and the sweep continues; it never returns an error, because both callers are tails on a merge resolution that has already committed and must not be unwound.

`skipStageID` exempts the caller's own stage. It is genuinely load-bearing at the primitive: `review@awaiting_approval` is itself a pair-table row, so a caller that resolves the review stage itself must hand its id in or the sweep would race the transition that caller owns (`TestSupersedeParkedStages_SkipStageIDIsHonored`).

### `POST /v0/runs/{run_id}/reconcile-merge`

The operator recovery verb. Authorization is TWO gates: `requireRunAccount(memberWrite)` for account ownership (an operator-decision write, not a destructive one — the merged-PR precondition and the pair table bound what it can do), **plus a handler-side `requireWriteScope("write:runs")` rung 0** (E45.95 / [#3635](https://github.com/kuhlman-labs/fishhawk/issues/3635)). Refusals, all evaluated BEFORE any write so a refused reconcile moves zero stages and writes zero rows:

| status | code | condition |
| --- | --- | --- |
| 401 | `authentication_required` | no authenticated identity (rung 0) |
| 403 | `insufficient_scope` | an authenticated BEARER not holding `write:runs` (rung 0; `details.required_scope` names it) |
| 400 | `validation_failed` | non-UUID `run_id` (field `run_id`), or a body that is neither empty nor exactly `{"supersede_stranded": <bool>}` (field `body`: wrong type, unknown field, trailing data, over 4 KiB) |
| 503 | `reconcile_merge_unconfigured` | run/audit repositories unwired, or `supersede_stranded` set and the run repository does not implement `run.StrandedStageMergeSuperseder` (`details.missing_capability`) |
| 404 | `run_not_found` | — |
| 409 | `reconcile_merge_pr_not_merged` | no `pr_merged` / `post_merge_observed` / `merge_observation_recorded` entry on the run's chain (the third is the row `POST /v0/runs/{run_id}/record-merge-observation` appends after a live `merged=true` forge read, E64.32 / #3136) — applies to EVERY arm, settle-only and stranded included |
| 409 | `reconcile_merge_not_applicable` | no pair-table-admissible parked stage, no already-superseded stage to repair, no stranded candidate (counted only under `supersede_stranded`), and not the settle-only shape; `details.supersede_stranded` echoes the flag |
| 409 | `reconcile_merge_stage_live` | `supersede_stranded` set and a `dispatched`/`running` candidate shows DB-stamped activity inside `strandedStageIdleThreshold` (24h); `details.live_stages[]` + `details.idle_threshold_seconds` |

The merged-PR precondition is what stops the verb manufacturing a `succeeded` run for a change that never shipped; a chain-read failure is a 500, never a write (fail closed on unknown evidence).

**Idempotent, and self-repairing.** A second POST finds the stage already `superseded` — not a pair-table state — moves nothing, and returns 200 with two empty lists. The **repair scan** closes the missing-row window the transition-first ordering deliberately allows: a stage that is `superseded` but carries no audit row (a sweep whose CAS committed and whose append then failed) gets a row back with reason `repair`, transitioning nothing. That scan compares against a **PRE-sweep** snapshot of the chain, which is exactly why the same-invocation exclusion is load-bearing rather than decorative: without it the stages this very invocation moved would look un-recorded and draw a SECOND row. `TestReconcileMerge_ExactlyOneAuditRowPerSweptStage` asserts EXACTLY one row per swept stage across two sequential POSTs, not at-least-one.

**Exactly-one holds ACROSS PROCESSES, and the control is the database (E64.29 / #3133).** The repair scan is a read-then-append, and #3083's fix-up serialized it only under the package-level `supersedeRepairMu` — which two fishhawkd instances (two replicas, or a rolling restart's overlap) do not share, so each could observe the row missing and each append. Migration 0081's partial unique index `audit_entries_stage_superseded_by_merge_once_idx` on `(run_id, stage_id) WHERE category = 'stage_superseded_by_merge'` now REFUSES the second append; `supersedeRepairMu` is RETAINED and demotes to a **same-process fast path** (it still spares the common race a round-trip ending in a constraint violation, and it keeps the in-process guarantee if the index is ever rolled back). The key is `(run_id, stage_id)` and not `run_id` alone because one run legitimately supersedes several stages. `TestSupersedeAudit_PG_TwoPoolsWriteExactlyOneRow` races the append from TWO INDEPENDENT pools — what a second replica actually is, and what the pre-existing two-GOROUTINE test structurally could not discriminate — and `TestSupersedeAudit_PG_ExactlyOneRowRequiresTheIndex` runs that same race with the index DROPPED and asserts TWO rows land, so the index is the empirically identified control.

**`repaired` means THIS invocation restored the row.** A stage whose append collides with the 0081 index is OMITTED from `repaired` — the row is durable, but a concurrent replica wrote it, and the response claims authorship, not existence. The emitter logs that collision at INFO rather than the WARN that (correctly, for every other append error) says the row is MISSING and repairable. The SWEEP side is the mirror image: its compare-and-swap has already committed, so a stage whose audit append duplicates is still reported as MOVED (`TestSupersedeParkedStages_DuplicateAppendStillReportsStageMoved`).

After the sweep the handler re-runs `advanceRunAfterReviewResolve`, so the now-all-terminal stage set routes through `completeRun` on the same request (`Advance` walks a still-`pending` legacy run to `running` first; `TestReconcileMerge_LegacyPendingRunWithParkedReview`).

### Settle-only arm (#4222)

A merged run whose stages have ALL already `succeeded` but whose run row is still `pending`/`running` (a legacy run whose completion never re-ran) used to draw `409 reconcile_merge_not_applicable`, leaving `fishhawkd sweep-stale-runs --apply` as the only way to settle it — and recording the merge observation made it look fresh to the stale sweep. Guard 5 now also proceeds on the settle-only shape (>= 1 stage, every stage `succeeded`, run not terminal): nothing moves, completion re-runs, and the response carries two empty lists plus the settled `run_state`. Guard 4 (merge evidence) still runs first and only `succeeded` stages qualify, so it cannot settle a failed or cancelled run. Tests: `TestReconcileMerge_SettleOnlyArm`, `TestReconcileMerge_SettleOnlyRefusesFailedStage`.

### Stranded arm (opt-in `{"supersede_stranded": true}`, #4222)

A legacy run whose PR merged while `implement` stayed `running` (its runner long gone) beside a never-opened `review@pending` gate can never settle: neither state is a merge-supersedable park, so the default verb correctly leaves both alone (#968). The opt-in arm retires them, and four things bound it:

- **Opt-in body.** `decodeReconcileMergeRequest` accepts an absent/empty/whitespace body (the pre-#4222 verb, byte-identical) or exactly one JSON object with only `supersede_stranded` (`DisallowUnknownFields`, no trailing data, 4 KiB cap — the `reap_failure.go` precedent). Without the flag a stranded stage is not even classified (`TestReconcileMerge_SupersedeStranded_FlagAbsentLeavesStrandedStageAlone`, `_MalformedBodyRefused`).
- **A SEPARATE default-deny table outside the transition union.** Candidates are `run.StrandedMergeSupersedable` rows (`implement@dispatched/running`, `review|acceptance@pending/dispatched/running`; `plan`, `deploy`, `implement@pending` denied), moved through `run.StrandedStageMergeSuperseder.SupersedeStrandedStageOnMerge` — row-locked, pinned to the classified state, the attempt (`StageAttemptToken(dispatched_at)`) and the handler's idle cutoff. It is an OPTIONAL capability: a repository without it is `503`, never a degrade to an ordinary transition (`_CapabilityAbsentIs503`). The edge is not in `transitionStageTx`'s union, so no other caller can reach `running → superseded` (contract: [`backend/internal/run/README.md`](../../run/README.md)).
- **Liveness gate, whole-call, before any write.** Each `dispatched`/`running` candidate's last activity is `strandedLastActivity` = max(`updated_at`, `dispatched_at`) — both DB-stamped (`stages_set_updated_at`, migration 0072); `started_at` is deliberately excluded (Go-stamped by the same UPDATE that bumps `updated_at`, which dominates it). One candidate active after `nowFunc() - 24h` refuses the call `409 reconcile_merge_stage_live`. Why 24h: every runner heartbeat UPDATEs the stage row, the no-heartbeat phases (verify gate, acceptance zero-credential posture) are bounded by agent/gate timeouts far shorter, and every re-dispatch resets both clocks. Pending candidates are held by no runner and skip the gate. Clock domains: the cutoff is fishhawkd's clock against Postgres-stamped columns; skew is unbounded in principle (#3048) but only matters at the 24h boundary, and the tests anchor `nowFunc` to the stage's DB-stamped `updated_at` read back. Tests: `_RefusesLiveStage`, `_HeartbeatKeepsStageLive` (pins the trigger assumption).
- **Row-locked twin (TOCTOU).** The SAME cutoff is passed into the capability, which refuses under the row lock when `updated_at` is newer (`run.StageRecentlyActiveError`), the state moved (`StageStateChangedError`) or the stage was re-dispatched (`StageAttemptChangedError`). Those stop the stranded sweep (candidates run in stage-sequence order, so an in-flight implement precedes the gates downstream of it), skip the repair scan and completion, and answer the same 409 with `live_stages[].refusal` (`recently_active` / `state_changed` / `attempt_changed`) and `details.superseded` listing what the call already moved. Tests: `_HeartbeatAfterGateRefusedUnderRowLock`, `_StateDriftStopsSweep`, `_AttemptDriftStopsSweep`.

Writes keep the sweep's discipline: parked sweep first (`operator_reconcile`), then the stranded sweep, TRANSITION FIRST then one `stage_superseded_by_merge` row with reason `operator_reconcile_stranded` plus `last_activity_at` / `idle_threshold_seconds`. Stranded moves join the repair scan's moved-this-invocation exclusion set, since their rows are not in the pre-sweep snapshot either (`_ParkedAndStrandedTogether`, `_SameCallRepairExcludesStrandedMove`). End to end on the live `74e8eade` shape: `_SettlesMergedRun`. The MCP tool adds a host `pgrep` probe for `runner_kind=local` runs before calling this endpoint ([`backend/internal/mcpserver/README.md`](../../mcpserver/README.md)).

### `completion_blocked` on `GET /v0/runs/{id}`

The read half (`runs.go`'s `completionBlockedForRun`). Populated on the single-run read ONLY — the same best-effort posture as `Concerns` / `DerivedStatus` / `SliceDependsOn`, so the list endpoint pays no per-row stage + audit reads — and `omitempty`, so every unblocked run keeps a byte-identical response.

It names the **lowest-sequence** non-terminal stage (the run's own order, so the answer is stable and names the stage an operator reaches for first) and DISCRIMINATES the recovery across a closed THREE-value set (E64.40 / #3151): `reconcile-merge` when the blocker is pair-table-admissible AND the run's PR is observably merged — the endpoint's own preconditions, read through the same helper, so the surface and the verb cannot disagree; `record-merge-observation` when such a blocker's PR merge has NOT been observed yet but the run carries a PR URL that resolves under its forge family (`resolveObservationTarget`) — observe off the forge first, then reconcile; and `none` otherwise, with `reason` naming the state. Pointing an operator at a verb guaranteed to refuse is #3083's own complaint about `merge_run`.

The `record-merge-observation` arm mirrors ONLY the observe verb's CHAIN-LOCAL preconditions (admissible blocker, resolvable PR URL, no merge evidence) — it deliberately does NOT re-check the forge-side rungs (is the PR actually merged, does it carry a SHA and a timestamp), because a GET must not perform a forge read. So the surface can name the verb and the verb can still refuse with `record_merge_observation_pr_not_merged`; that residual is the honest cost of the no-forge-read constraint and is strictly better than `none`, which names no verb for a shape where one applies.

A merge-evidence read failure does NOT omit the field: it FAILS CLOSED to `none` (never advertise a verb we cannot confirm applies — including the observe verb) and still names the blocking stage, the half of the answer that does not depend on the chain.

## Merge-observation verb (`merge_observation.go`, E64.32 / #3136, E64.40 / #3151, E45.87 / #3622)

> **Rungs 7–10 are SHARED since E45.87 / [#3622](https://github.com/kuhlman-labs/fishhawk/issues/3622).** The forge read and the three fact guards live in the side-effect-free `observeForgeMergeScoped` helper (`observeForgeMerge` is its run-own-credential wrapper, the merge endpoint's entry point; #4222), and the single append lives in `appendMergeObservation`; this handler maps the helper's `obsForgeReason` discriminator back onto the refusal codes and statuses documented below, which are unchanged byte-for-byte. The operator merge verb's observe-before-dispatch rung runs the IDENTICAL ladder (see "Operator merge verb" above), which is what stops the two drifting. `TestObserveForgeMerge_Discriminators` pins the classification per rung independently of either handler.


`POST /v0/runs/{run_id}/record-merge-observation` is the OBSERVE half of the #3083 recovery pair. `reconcile-merge`'s evidence gate reads the run's audit CHAIN and never the forge, so a run whose PR genuinely merged but whose merge was never recorded (a webhook that never arrived, an observation lost when fishhawkd restarted mid-write) is unreconcilable — the evidence it needs can never appear. This verb is the ONLY new way onto that chain, and it requires a live `merged=true` forge answer to use it.

**Both halves require `write:runs` (E45.95 / [#3635](https://github.com/kuhlman-labs/fishhawk/issues/3635)).** The scope check is rung 0 on BOTH handlers — ahead of the `run_id` parse, the unconfigured check and the run lookup — so a refused caller learns nothing about whether the run exists and the observe verb issues no forge request. The pair is kept IDENTICAL deliberately: the observe and settle verbs must not diverge in who may call them. `write:runs` is the scope every sibling run-lifecycle recovery verb already enforces (`consolidate.go`, `reap_failure.go`, `reset_branch.go`, `recover.go`). A **cookie session** (`TokenID == ""`) is exempt by `requireWriteScope`'s documented contract, so the browser/OAuth-session path is not tightened at all — a signed-in operator session reaches both verbs exactly as before, bounded by `requireRunAccount`'s ownership and role-bounding. A run-bound `fhm_` token is REFUSED (it carries only `mcp:read` plus the two conditional run-bound write scopes, and neither handler authorizes it by SUBJECT), which is what the `/mcp` scope table mirrors with `runBoundSubjectOK: false`.

**Impact inventory.** `operatorDefaultScopes` (`backend/cmd/fishhawkd/token.go`) and `oauthas.DefaultScopes` both already carry `write:runs`, so every token minted by `fishhawkd token issue` and every default OAuth grant is unaffected; only a hand-narrowed token whose scope set omits `write:runs` loses these two verbs, and the remedy is re-issuing it.

**The observe/settle split is load-bearing.** This verb OBSERVES and records a fact; it settles nothing, transitions no stage and completes no run. `reconcile-merge` SETTLES and still reads only the chain. So the fail-closed posture #3083 established is preserved exactly: evidence is still REQUIRED, and the settling verb still never re-observes. The category `merge_observation_recorded` is DELIBERATELY DISTINCT from `pr_merged` — `pr_merged` carries a live-observation timestamp the latency/cost surfaces read as "when Fishhawk knew", so back-dating one would corrupt those series; recording `merged_at` (the forge's merge time) alongside `observed_at` (when Fishhawk learned it) plus `reconciled_after_the_fact:true` lets a reader see the gap without anything being back-dated.

### The rung ladder

Every refusal is evaluated BEFORE any write, so a refused call leaves ZERO rows:

| status | code | condition |
| --- | --- | --- |
| 401 | `authentication_required` | no authenticated identity (rung 0) |
| 403 | `insufficient_scope` | an authenticated BEARER not holding `write:runs` (rung 0; `details.required_scope` names it) |
| 400 | `validation_failed` | non-UUID `run_id` |
| 503 | `record_merge_observation_unconfigured` | run/audit repositories unwired (rung 2), OR the per-forge reader could not be resolved (rung 7) |
| 404 | `run_not_found` | — |
| 409 | `record_merge_observation_no_pull_request` | neither the run row nor any `pull_request_opened` entry on the chain carries a PR URL (a failed read of those entries is a 500) |
| 400 | `record_merge_observation_malformed_pr_url` | the recorded URL is structurally unresolvable under ANY forge shape, or the run's `repo` is not owner/name |
| 409 | `record_merge_observation_pr_url_repo_mismatch` | the URL resolves but names another repository, or a DIFFERENT forge family than the run authenticates against |
| 500 | `internal_error` | a chain read failed — fails CLOSED, never a write on unknown evidence (rung 6); or the audit append failed |
| 409 | `record_merge_observation_no_credential` | rung 6b: a credential-less GitHub-family run whose repository installation cannot be used — `details.reason` `app_not_installed` / `installation_account_mismatch` / `installation_account_unverifiable`; no PR read |
| 502 | `record_merge_observation_forge_unavailable` | `GetPullRequest` errored, or (rung 6b) the repository installation lookup errored |
| 409 | `record_merge_observation_pr_not_merged` | the forge answers NOT merged (or a nil PR) — the guard against manufacturing evidence for a change that never shipped |
| 409 | `record_merge_observation_no_merge_commit` | merged but an empty merge commit SHA — a generic partial-evidence guard on BOTH forges |
| 409 | `record_merge_observation_no_merge_timestamp` | merged with a SHA but a nil `merged_at` — refuse the partial fact rather than claim a merge time it does not carry |

A rung-6 idempotent 200 (`already_recorded:true`, appending NOTHING) fires when the chain already carries `pr_merged` / `post_merge_observed` / `merge_observation_recorded`.

**The audit-append-failure `500` routes its cause through `internalCauseKey`** (E45.94 / [#3631](https://github.com/kuhlman-labs/fishhawk/issues/3631)) rather than a plain `"error"` details key. This is a CONFORMANCE change, not a body-content fix: `"error"` was never a member of `redactableDetailKeys`, so `writeError`'s 5xx default-deny allow-list already stripped it from the shipped body before this change. Routing it through `internalCauseKey` instead makes the operator-only intent explicit at the call site and folds the cause into the log record's dedicated `cause` attribute rather than an incidental `details.error` member; `details.run_id` (allow-listed) still gives the caller a correlation handle. `TestRecordMergeObservationAppendFailure_CauseIsOperatorOnly` pins the `cause`-attribute routing; `TestRecordMergeObservationAppendFailureIsAnError` is the unchanged regression pin on status/code/zero-rows.

### Legacy runs: credential fallback + PR-URL derivation (E83.95 / #4222)

Two legacy-run gaps made the observe verb unusable on runs whose PRs genuinely merged:

- **Credential fallback (rung 6b, `resolveObservationCredential`).** A GitHub-family run carrying NO credential (no `InstallationRef` / `InstallationID`) with `cfg.GitHub` wired resolves the App's CURRENT installation on the run's own repository (`resolveRepoScope`). Not installed → `409 record_merge_observation_no_credential` (`app_not_installed`, message names the remedy); lookup error → `502 forge_unavailable`. **Account condition (Captain, binding):** the resolved installation is used ONLY when the run is untenanted (empty `AccountID`, the legacy shape) or the installation maps to the run's OWN account through the webhook `InstallationAccountLookup` seam; another account or none is `installation_account_mismatch`, an unreadable mapping is `installation_account_unverifiable` — both fail closed with zero rows and zero PR reads. A run carrying its own credential never looks the installation up. **Placement:** the fallback is computed in `handleRecordMergeObservation` and passed into `observeForgeMergeScoped`; `observeForgeMerge` (the merge endpoint's observe rung) passes only the run's own credential, so `POST /v0/runs/{run_id}/merge` gains no installation GET. Tests: `TestRecordMergeObservation_FallsBackToRepositoryInstallation`, `_AppNotInstalledNamesRemedy`, `_InstallationLookupFailureIs502`, `_RunCredentialSkipsFallback`, `_FallbackAccountCondition`, `TestMergeRun_ObserveRungNeverLooksUpRepositoryInstallation`, `TestObserveForgeMerge_NoCredentialState`.
- **PR-URL derivation (rung 4).** A run row with no `pull_request_url` derives one from the `pr_url` of its newest (highest-sequence) `pull_request_opened` entry — read-time only, never written back, so other surfaces still see no URL. The derived URL flows through the unchanged rungs 5/5b before any forge read, so an entry naming another repository is still refused. Tests: `_DerivesPRURLFromPullRequestOpened`, `_DerivationPicksNewestEntry`, `_DerivationNoUsableEntry`, `_DerivedURLStillRepoChecked`, `_DerivationChainReadErrorFailsClosed`, `_RunRowURLPreferred`.

The observation and the `merge_observation_recorded` payload carry `credential_source` (`run` | `repository_installation`) and `pull_request_url_source` (`run_row` | `pull_request_opened_audit`), both `omitempty` so the `already_recorded` arm's zero observation keeps its wire shape.

### Forge-family URL resolution + per-forge reader dispatch (E64.40 / #3151)

The original verb was GitHub-URL-shaped end to end — it resolved the PR number only from a `/pull/` segment and refused any run whose credential ref named a non-GitHub forge — so a GitLab run drew a 409 BEFORE the forge was ever read, and acceptance criterion 2 was unreachable without widening it. `resolveObservationTarget` replaces that with a forge-FAMILY-aware resolution, WIDENED without being weakened:

- the forge id is derived from the run's `InstallationRef` (bare decimal or absent = `github`, `<id>:...` = that forge);
- the recorded URL is classified family-AGNOSTICALLY (`/pull/<n>` = github; `/-/merge_requests/<n>` canonical or `/merge_requests/<n>` legacy = gitlab), and the URL's OWN family must equal the run's forge family — a github-ref run can never present a GitLab URL, and vice versa, and any unimplemented forge fails closed to a mismatch;
- the URL's project path must equal the run's `repo` case-insensitively.

The reader is then resolved per-forge (`prStateReaderFor`) with an UNAMBIGUOUS ladder (binding approval condition 2): `cfg.PRStateReader` overrides everything (test seam); a **github**-family run resolves ONLY through `cfg.GitHub`, else 503 — it NEVER falls through to the forge resolver or the process registry, so registry availability can never change a GitHub outcome; any other family resolves through `cfg.ForgeResolver` (defaulting to `forge.Get`), a resolver error or nil forge yielding 503. This keeps the rung-5b security property (the verb can never confirm a DIFFERENT pull request and then write a trusted row asserting this run's URL was observed merged) by construction: the URL shape and the reader are both bound to the SAME forge id.

### Idempotence — the honest (weak) claim

The rung-6 guard is a READ-THEN-APPEND and is NOT atomic: two CONCURRENT posts can both read an empty chain and both append. This code does NOT serialize them. A SEQUENTIAL repeat POST appends nothing (the test pins that); a concurrent duplicate is POSSIBLE and HARMLESS, because the evidence gate asks only whether AT LEAST ONE qualifying row exists — a second row is inert. The stronger `supersedeRepairMu` mechanism was deliberately NOT adopted here: it would buy nothing a caller can observe.

### Live-validation operator walk (binding approval condition 3)

The `requires_live_validation` criterion — a merged GitLab MR records exactly one observation carrying the MR's SHA and `merged_at` — is validated by this exact walk against a live deployment:

`$FISHHAWK_API_TOKEN` must hold `write:runs` (E45.95 / #3635) — every token minted by `fishhawkd token issue` and every default OAuth grant already does; a hand-narrowed one draws `403 insufficient_scope` with `details.required_scope: write:runs`.

```sh
# A run whose InstallationRef is "gitlab:<project_id>" and whose PR URL is a
# merged merge request, e.g. https://gitlab.com/group/project/-/merge_requests/7
curl -sS -X POST "$FISHHAWK_BACKEND_URL/v0/runs/$RUN_ID/record-merge-observation" \
  -H "Authorization: Bearer $FISHHAWK_API_TOKEN"
# -> 200 {"run_id":"...","already_recorded":false,"observation":{...,"merge_commit_sha":"<sha>","merged_at":"<forge time>",...}}

# Confirm EXACTLY ONE merge_observation_recorded row carrying the MR's SHA and merged_at:
curl -sS "$FISHHAWK_BACKEND_URL/v0/runs/$RUN_ID/audit?category=merge_observation_recorded" \
  -H "Authorization: Bearer $FISHHAWK_API_TOKEN" \
  | jq '.items | length, .[0].payload.merge_commit_sha, .[0].payload.merged_at'
# -> 1, "<sha>", "<forge time>"
```

## Terminal run-branch sweep (`run_branch_sweep.go`, E68.67 / #3562)

A run's Fishhawk-owned branches are deleted from the forge when the run reaches a terminal state, instead of accumulating (204 stale `fishhawk/run-*` refs by 2026-09-21). `sweepRunBranches` is best-effort throughout, on the `stampEconomicsIntoPRBody` model: no failure unwinds the caller's already-committed transition.

### Candidate derivation (`runSweepCandidates`, pure)

Derived from the single sources of truth, never by listing refs (#1245):

- every non-child run: `fishhawk/run-<short(run)>/stage-<short(stage)>` per stage (the `fixupBranchFor` form);
- a decomposed PARENT (children enumerated via `listAllDecomposedChildren`): additionally `orchestrator.ConsolidatedBranch(run)` and `orchestrator.SliceBranch(run, SliceIndex)` per child (nil index → 0);
- a decomposed CHILD: nothing. Its slice branch is derived from the PARENT's id and belongs to the parent's fan-in.

Every candidate passes the namespace guard (`strings.HasPrefix(b, "fishhawk/run-")`), so a destructive call can never name a default, release or human branch. A children-enumeration error degrades to the consolidated branch plus the parent's stage branches and is recorded as `children_unenumerated`; a stage-enumeration error is recorded as `stages_unenumerated`.

### Preservation gates (each fails CLOSED toward keeping the branch)

1. **Existence probe.** When the forge carries `GetBranchSHA` (both registered adapters do), an absent branch is classified `already_absent` and gets no further calls. `RefDeleter` maps an absent ref to nil, so without the probe an absent ref would read as `deleted`.
2. **Open-PR head.** `ListOpenPullRequestsByHead(branch, "")`. An empty base omits the base filter on both forges. A non-empty result → `skipped_open_pr` with reason `open_pr_head`.
3. **Open-PR base** (#3562 approval condition 1). Deleting a consolidated branch would auto-close its open slice PRs. For each surviving candidate, the forge PR list is queried keyed on that candidate as BASE, once per candidate head that has an open PR (or whose PR read failed). A hit → `skipped_open_pr` with reason `open_pr_base`.
4. **Unreadable PR state.** Any PR-list error in gate 2 or 3 → `skipped_pr_read_error`. An unreadable read never authorizes a delete.

Residual: gate 3 only sees PRs whose head is one of the run's own candidates. The GitHub PR list is head-keyed in `forge.Forge`, so a PR into a `fishhawk/run-*` branch from an unrelated head is not detected.

### Forge resolution (`runBranchForgeFor`)

Same ladder shape as `prStateReaderFor` / `issueOpsFor`. `cfg.RunBranchDeleter` (the test seam) overrides the ladder. A github-family run resolves only `cfg.GitHub` (via `forgegithub.New`). Any other family resolves `cfg.ForgeResolver` (default `forge.Get`), guarded by `isNilForge`. The resolved forge must carry `DeleteRef` and `ListOpenPullRequestsByHead`. Otherwise it resolves to nil and there is no sweep; the sweep never deletes without the PR gates.

### Trigger sites

- `resolveReviewStageOnMerge`: the LAST statement of both merge-resolution success tails (no-review-stage and review-stage), after the economics stamp. Synchronous. It is not on the closed-without-merge arm, because that PR's head may be reopened.
- `handleCancelRun`: AFTER `writeJSON`, via `sweepRunBranchesDetached`, a goroutine on `context.WithoutCancel(r.Context())` bounded by `runBranchSweepBudget` (2m) and tracked by `bgBranchSweeps` (drained by `Shutdown`, synced in tests by `waitBranchSweeps`). The cancel response never waits on forge round-trips (#3562 approval condition 3). An already-cancelled 200 re-sweeps idempotently.

### The `run_branches_swept` row

ONE chained system-actor entry per sweep: `trigger` (`pr_merged` | `cancelled`), `deleted`, `already_absent`, `skipped_open_pr` (`{branch, reason}`), `skipped_pr_read_error` and `errors` (`{branch, error}`, messages capped at 300 bytes), and the optional `children_unenumerated` / `stages_unenumerated`. No row is written when nothing was attempted: a nil forge, a zero credential scope, an unparseable repo, a terminal-failed or decomposed-child run, or an empty candidate set. A per-branch delete error (e.g. a ruleset 403 → `forge.ErrForbidden`) is recorded and the loop continues. A nil `AuditRepo` or a failed append skips only the row; the deletions still happen. The category is registered in `audit.KnownCategories`. It is not in `issuecomment`'s `activityCategories`, because the sweep is housekeeping and never calls `notifyOperatorVisible` / `notifyStatusUpdate`.

### Stated residuals

- **No terminal-FAILURE sweep.** `POST /v0/runs/{run_id}/revive` re-admits a failed run and resumes on exactly these branches, so `sweepRunBranches` returns early on `run.StateFailed`. The failed-run sweep is deferred to #3678.
- **No retroactive sweep** of refs that predate this change.
