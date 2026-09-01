# Webhook event dispatcher

Per-area appendix for the `Webhook event dispatcher (events → runs + stages)` row in [ARCHITECTURE.md](../ARCHITECTURE.md). Hand-extracted from that row for readability; content is verbatim, not a rewrite.

Implementation: `backend/internal/webhook/dispatcher.go` (`MatchEvent` pure + `Dispatcher.Handle` orchestrator); wired via `cfg.WebhookDispatcher`. Creates one `Stage` row per spec-stage definition; first stage transitions to `dispatched` on workflow_dispatch.

## Branch-protection snapshot

(#251 / ADR-017) Between spec validation and run-create, `resolveRequiredChecks` calls `githubclient.GetBranchProtection` + `ListRulesetRequiredChecks` and unions the results into `run.RequiredChecksSnapshot{Contexts, Sources}`, persisted to `runs.required_checks_snapshot` (JSONB, migration 0017). No protection covers the target ref → `errNoBranchProtection` → refuse the run with a `webhook dispatch refused: branch protection` WARN log; missing `administration:read` scope → `errProtectionScopeMissing` so the log line names the operator-side fix (re-install per #252). `branch_protection_rule` and `repository_ruleset` webhook events are recognized by `MatchEvent` and skip-with-reason in v0 — the receiver acknowledges so a future cache layer has a path without changing the webhook contract.

## CI policy re-evaluation

(#300) `check_run.completed` events on a Fishhawk-managed PR's required checks re-fire the implement-stage policy evaluator. Runs server-side (`server/policy_reeval.go::reevaluateCIPolicy`, called from `server/webhook.go` after `ingestCheckRun` writes the stage_checks row) so it sees the fresh state. Semantics: per-check completion with audit-row dedup — every event triggers the re-eval; only events that change the aggregate `ci_green` value write a new `policy_evaluated` row. Aggregate rule: `true` when every required check is in the pass bucket, `false` on the first fail-bucket check (failure is decisive; we don't wait for siblings), `nil` while some required checks haven't reported. `fishhawk_audit_complete` is excluded from the aggregate (Fishhawk's own derived check, would circularly depend on the policy eval). The dispatcher's CI-retry path below is a sibling concern that handles the failure-bucket conclusions for follow-up dispatch.

## CI-failure retry

### Trigger

(#278 / E16) `check_run.completed` events route through `matchCheckRun`, which fires `MatchActionCIFailureRetry` when the conclusion is in `failedCheckRunConclusions` (the same closed set `stagecheck.DeriveState` uses for the "fail" pill), the check name isn't `fishhawk_audit_complete` (retrying won't fix Fishhawk's own audit gaps), and `pull_requests[]` is non-empty. The `Match` carries a `CheckRunRef{PRNumber, HeadSHA, CheckName, Conclusion}` for the handler.

### Handler

(#279 / E16) `handleCIFailureRetry` reads the parent run via `ListRuns({PullRequestURL: …})`, filters to checks named in the parent's `required_checks_snapshot` (non-required failures are not merge blockers), dedups against existing runs that already recorded the same head_sha via their implement-stage `pull_request` artifact, resolves `on_ci_failure.max_retries` from `parent.WorkflowSpec` (cached at run-create per #283, defaulting to `spec.DefaultMaxRetries = 1`), and either creates a follow-up run with `ParentRunID = parent.ID`, `RetryAttempt = parent.RetryAttempt + 1` (migration 0020) and fires `workflow_dispatch` — or appends a `ci_retry_exhausted` chained audit row against the parent when the cap is hit.

### Retry cap snapshot

(#280 / E16) Every run-create path also stamps `runs.max_retries_snapshot` (migration 0021, default 1) — for the original-run path the value comes from the parsed spec via `workflowMaxRetries`; the retry-handler path copies the parent's value forward so a long-running chain shows the same N/M on every row. Surfaces on the SPA as a "Retry N/M" badge in the run-detail header and a "Retry #N" chip in the related-runs panel.

### Plan reuse on retry

**Variant A: retry runs skip the plan stage** (`filterOutPlanStages`); the implement-stage prompt builder (`server/prompt.go::loadApprovedPlanForRun`) walks `parent_run_id` up to `retryPlanChainDepth = 8` levels to find the parent's approved standard_v1 plan, so the retry runs against the same plan without re-prompting. Successful dispatches chain a `ci_failure_retry_dispatched` audit row against the child and best-effort post an issue comment (`issuecomment.Notifier.NotifyCIRetry`, `KindCIRetry`) with per-attempt dedup (payload carries `retry_attempt` and the audit-log scan matches both `kind` and `retry_attempt`, so redeliveries of the same `check_run.completed` are absorbed but a fresh attempt N+1 still announces itself). The dispatcher's optional `Artifacts artifact.Repository` is what the dedup guard reads; nil leaves the guard at "no, this head_sha isn't recorded" — the `max_retries` cap still bounds runaway retries.

### Budget admission does not gate a retry

(ADR-030, E45.27 / #2878) `handleCIFailureRetry` never calls `refusedByBlockingBudget`. The blocking periodic-budget seams gate whether NEW work starts; a CI-failure retry continues a lineage that already passed admission, and ADR-030's position is that in-flight work finishes. Gating it would strand a red PR with no in-product path forward, because the webhook path carries no `budget_override` and the fail-open sum would make the stranding nondeterministic. The spend is bounded by construction rather than by trust: `retry_attempt = parent.RetryAttempt + 1` plus the `runs_retry_child_once_idx` partial unique index caps a lineage at `on_ci_failure.max_retries` children no matter how many deliveries arrive, and opening a NEW lineage still goes through a gated create seam. `backend/internal/webhook/README.md` ("Why the CI-retry paths are exempt") owns the long-form contract, including the two behavioural tests that pin it; this section is the narrative pointer.

## Review-stage merge signal

(ADR-018 / #312) `server/pullrequest_review_events.go` handles `pull_request.closed` with `merged=true` (transitions the run's review stage to `succeeded` + writes a `pr_merged` audit row naming the merger from `merged_by.login`) and `pull_request_review.submitted` (audit-only; `pr_approved_on_github` for `state=approved`, `pr_review_submitted` for everything else). The handlers look up the run by `runs.pull_request_url` (#216); PRs that aren't Fishhawk-managed skip cleanly.

## PR closed without merging

(#316) Transitions the review stage to `cancelled` + writes a `pr_closed_without_merge` audit row naming the closer from `sender.login`. The run-level state cascades to `cancelled` once every stage is terminal (existing state-machine behavior). Reopen is intentionally out of scope — terminal stages don't resurrect; the reviewer re-triggers via `/fishhawk run` and the new run threads off the cancelled parent via `parent_run_id`. Plan-stage approval flow (#238) unchanged; the review stage is now a read-only summary of PR-side activity. App manifest adds `pull_request_review` to default events.

## In-Fishhawk approval prune

(ADR-018 / #313) `server/approvals.go::handleSubmitApproval` returns `409 review_stage_managed_by_github` when targeted against a review stage and includes the run's `pull_request_url` in the error details. The slash-command path (`server/issue_approval.go::HandleApprovalCommand`) replies with a help message pointing at the PR instead of submitting an approval. Both surfaces continue to accept plan-stage approvals — Fishhawk's vote at plan time is independent and meaningful; GitHub has no equivalent. The workflow spec's `gates: [approval]` with `approvers.any_of: [...]` on review stages becomes informational (stored on the row for display, not enforced); teams that want strict approver enforcement configure branch protection's required-reviewers.

## SPA review-stage read-only summary

(ADR-018 / #314) `frontend/src/review/review-document.tsx` drops the in-Fishhawk `ApprovalPanel` for review stages. Header gains a "View on GitHub" affordance pointing at `artifact.pr_url`. A new Activity section reads `listRunAudit({stageId, limit})` and renders the three PR-side categories from #312 — `pr_merged` → "@x merged the PR", `pr_approved_on_github` → "@x approved", `pr_review_submitted` → "@x requested changes / commented / dismissed" — oldest first so reviewers scan a left-to-right timeline. Approvers panel renamed "Approvers (informational)" with copy noting branch protection's required-reviewers is the actual gate. Plan-stage approval flow in `plan-document.tsx` unchanged.

## GitLab run creation and CI-failure retry (E45.22 / #2043)

### Run creation is live

`Dispatcher.Handle` no longer parks a GitLab `MatchActionRun` at the E45.6 / #1860 boundary. An admitted GitLab trigger routes to `gitlab_dispatch.go::handleGitLabCreateRun`, which is a SEPARATE path from the GitHub create rather than a shared one, for three reasons the shared code cannot absorb:

- the spec is read through the forge-neutral `forge.FileFetcher` (`Dispatcher.GitLabFiles`, wired in `serve.go` from `registeredFileFetcher("gitlab")`), because the GitHub client's scope parse rejects a `gitlab:<project_id>` credential ref outright;
- there is no branch-protection snapshot to take — GitLab's protected-branches API contributes no required-status contexts, so the ADR-017 refusal would reject every GitLab run;
- comment-back / board-sync notifiers are GitHub-only.

Everything that IS shared is called, not reimplemented: the coarse plan-reviewer capability gate, the `applies_to` routing gate, the blocking periodic-budget gate (in that order), `CreateStagesFromSpec`, and the `run_dispatched` audit.

### Project authorization gates the create path

`handleGitLabCreateRun`'s FIRST statement is an authorization check, ahead of the spec read and the pipeline trigger. It has to be: the GitLab receiver authenticates a delivery by comparing `X-Gitlab-Token` against ONE shared deployment secret, and GitLab signs no HMAC over the body, so a valid token proves the sender knows the deployment secret and NOTHING about the project the payload names. Both project selectors on the create path are payload-derived — `ev.CredentialRef` (`gitlab:<project_id>`) selects the credential and the project a pipeline is created in, `ev.Repo` (`path_with_namespace`) selects the project the executable workflow spec is READ from — so leaving them unbound is a confused deputy: untrusted input steering deployment credentials, network egress and code execution. The GitHub path needs no analogue; its `installation_id` arrives inside an HMAC-signed payload.

`Dispatcher.GitLabProjects` (`webhook.GitLabProjectAuthorizer`) is the seam. Production's implementation is `gitLabProjectRegistry` in `backend/cmd/fishhawkd/serve.go`, reading the ADR-057 tenancy tables: the credential ref must resolve to a registered `installations` row under `provider='gitlab'`, AND the project path's namespace segment must equal that installation's account `account_key` (the same owner-segment convention `account.Resolver` uses). Binding only the ref would leave the spec read steerable at some other project's path, so both halves are required.

Every outcome other than a positive vouch REFUSES, writes a `run_rejected_misconfigured` global-chain audit row naming the reason, and leaves ZERO forge calls behind:

| reason | meaning |
|---|---|
| `gitlab_project_registry_unwired` | no authorizer configured (e.g. no database). The nil seam FAILS CLOSED — unlike `GitLabFiles`, where nil merely turns an optional read off |
| `gitlab_project_ref_absent` | the payload carried no project id |
| `gitlab_project_authorization_lookup_failed` | the registry could not be read; a transient fault must not open the gate |
| `gitlab_project_not_registered` | the project (or the id/path pair) is not one this deployment was authorized to act on |

Operator setup — the `installations` row to insert — is in [`docs/deploy/gitlab.md`](../deploy/gitlab.md).

The created run carries `runner_kind=gitlab_ci` as an ADR-045 creation-time HINT with `runner_kind_resolved` left false (the runner's signed self-report stays authoritative), `installation_ref = "gitlab:<project_id>"` (migration 0076 — this is what lets `orchestrator.runCredentialScope` resolve a non-zero scope for later stages instead of warn-skipping every one), and a nil `installation_id`. Its first stage's pipeline necessarily runs on the DEFAULT ref: the ADR-035 sole-writer run branch does not exist until the runner creates it.

### Pipeline classified, Job (build) skipped

GitLab emits BOTH a Pipeline Hook (`object_kind: "pipeline"`) and a Job Hook (`object_kind: "build"`) for the same failing job. `matchGitLabCIFailure` classifies a FAILED pipeline as `MatchActionCIFailureRetry` and SKIPS the build kind with an explicit reason, so one failing job drives at most one retry attempt at the classification layer.

The build skip is a CONTROL, not a parse failure. The Job Hook payload carries top-level `ref`, `sha`, `build_status` and `pipeline_id` — every correlation input the pipeline payload carries under `object_attributes` — and the classifier's union decode reads both shapes, so a build payload would classify just as happily. Delete the build arm and a combined delivery drives two retry attempts; `TestGitLabCIRetry_BuildOnlyEventCreatesNoRetry` is the counterfactual vehicle.

### Correlation contract: ref AND sha

A GitLab pipeline carries no `pull_request_url` handle, so `gitlab_ciretry.go` correlates DETERMINISTICALLY on two signals that must BOTH hold:

- the pipeline's `ref` equals the candidate run's ADR-035 run branch (`fishhawk/run-<short>`, or a decomposed child's `fishhawk/run-<shortParent>/slice-<n>` — the derivation duplicated from `orchestrator.runBranchRef`. `TestGitLabRunBranch_MatchesOrchestratorDerivation` compares this package's copy against `orchestrator.RunBranchRef` — the exported seam onto that same unexported derivation — rather than against hard-coded strings, because a drift would make every correlation miss silently; `TestRunBranchRef_IsTheRefTriggerParamsDispatches` closes the chain by pinning the exported seam to the ref `triggerParams` actually dispatches), AND
- the pipeline's `sha` equals the head SHA that run recorded on its implement-stage `pull_request` artifact.

The merge-request `iid` only NARROWS the candidate set. It cannot select a run: a retry lineage produces MULTIPLE runs per merge request by construction. The pipeline id is captured into the audit payload as correlation provenance.

**First-stage retry is DEFERRED, by design.** Issue #2043 offers two resolutions — "correlate by pipeline/project+SHA, or defer first-stage retry until the run branch exists" — and this contract takes the SECOND. A first pipeline runs on the default ref, so its ref can never equal a run branch and it correlates to nothing. Correlating a default-ref pipeline by project+SHA alone would match any run whose recorded head happened to be that commit, across branches and lineages, with no second signal to disambiguate. A deferred retry costs one manual re-run; a mis-correlated one retries the WRONG run.

A deferral is not a silent nothing. Every non-correlation writes a `ci_retry_skipped` audit row naming WHICH reason applied — `first_stage_pipeline_on_non_run_branch`, `no_candidate_run_owns_pipeline_ref`, `pipeline_sha_does_not_match_run_head_sha`, `run_head_sha_lookup_failed`, `run_lineage_cancelled`, `retry_policy_unresolvable_from_cached_spec`, `gitlab_pipeline_trigger_unconfigured`, `run_has_no_credential_scope` — chained to the run that OWNS the pipeline's ref and global-chained when none does, so a failure that was correctly ignored is distinguishable from one that should have retried and did not (#2860). The one case with no audit row is "no gitlab_ci runs for this project at all": there is no run to chain against, and the delivery is not a Fishhawk failure.

**The last two reasons are PRE-FLIGHT refusals, and the GitLab path diverges from the GitHub path here (E45.25 / #2876).** `runnerbackend.GitLabCI.TriggerStage` returns nil on BOTH of its fail-closed warn-skip branches — a nil `Trigger` (GitLab unconfigured) and a zero `p.Scope` — which is indistinguishable from a created pipeline, so the handler used to read that nil as a successful dispatch, transition the first retry stage to `dispatched` and audit outcome `dispatched` with nothing behind it. The reachable instance is a correlated legacy `gitlab_ci` parent whose `installation_ref` is nil or empty. Both preconditions are now checked BEFORE `run.ChildParamsFrom` and therefore before any mutation. That is a deliberate, operator-ratified divergence from the GitHub path, which mints the child and CONVERTS each warn-skip into a `dispatchErr` (`errCIRetryGitHubUnconfigured` / `errCIRetryNoInstallation`), leaving a persisted child with an untransitioned stage and a `dispatch_failed` row; the GitLab path leaves no child at all and a `ci_retry_skipped` row. The reason is the dedup index: a minted child consumes the single retry slot `runs_retry_child_once_idx` enforces (`retry_attempt = parent.RetryAttempt + 1`), so a child that never dispatched permanently occupies the attempt a corrected deployment would otherwise use. The guard resolves the `gitlab_ci` backend ONCE and reads the resolved instance's own `Trigger` field — the very field `TriggerStage` consults on the very instance dispatched — so the two cannot disagree by construction, rather than by an equivalence a later edit could break silently.

### Budget admission does not gate the GitLab retry either

`handleGitLabCIRetry` inherits the same ADR-030 continuation exemption as the GitHub path, for the same reason and with the same per-lineage `max_retries` bound — deliberate parity, unlike the E45.25 / #2876 pre-flight divergence above; the reachable population is one exact-project-bound registered project (E45.26 / #2877), correlated on `(ref, sha)`. Contract and pinning tests: `backend/internal/webhook/README.md`.

### Dedup is a CONSTRAINT on both retry paths

Both the GitLab path and the pre-existing GitHub `check_run` path dedup on the `runs_retry_child_once_idx` partial unique index (migration 0076). A `23505` on that index — recognized by `run.IsRetryChildDuplicate`, which matches the constraint name specifically and additionally accepts the `run.ErrRetryChildDuplicate` sentinel for fakes — is the benign "someone else won" branch: no error surfaces, so the forge does not redeliver. Every other create error stays a hard failure.

The index only dedups when both racing inserts compute the SAME `retry_attempt`, so BOTH paths derive it as `parent.RetryAttempt + 1` from the PARENT row, never from the latest existing child. A child-derived value would make the two inserts disagree, the index would never fire, and a count-only concurrency test would stay green while the defect stayed open. The index is pinned as an ATOMIC guard by `TestCreateRun_ConcurrentRetryChildrenRaceToOne` in `backend/internal/run/postgres_test.go`, which races two goroutines at one parent through a real Postgres-backed repository and asserts WHICH attempt survived; dropping the index from migration 0076 lets both inserts succeed and turns it red. It lives in the run package deliberately — the index is a persistence-layer constraint and this package has no Postgres-backed fixture. This package pins only the HANDLER half (`TestGitLabCIRetry_DuplicateSentinelIsBenign`): a delivery handed the duplicate sentinel returns nil rather than a 5xx the forge would redeliver, and mints no second child.

**The GitHub path additionally KEEPS its head_sha pre-read, as defence in depth.** The constraint is the atomic guard — a read-then-write cannot serialize concurrent deliveries — but it keys on `(parent_run_id, retry_attempt)` and so covers only deliveries that resolve the SAME parent. Once a retry child has itself set `pull_request_url` on the PR, a late or redelivered `check_run` failure for the ORIGINAL head_sha resolves the CHILD as parent (`findRunForCIRetry` takes the newest non-terminal row on the PR) and mints attempt N+1 for a SHA that was already retried — a different parent, so the index never fires. `runOnHeadSHAExists` refuses that case; `TestHandle_CIFailureRetry_StaleRedeliveryOnRetriedHeadSHACreatesNoSecondChild` pins it. The GitLab path needs no analogue: it correlates on `(ref, sha)` and a pipeline's SHA is matched against the candidate run's own recorded head, so a stale pipeline lands on `pipeline_sha_does_not_match_run_head_sha` instead.
