# Webhook event dispatcher

Per-area appendix for the `Webhook event dispatcher (events → runs + stages)` row in [ARCHITECTURE.md](../ARCHITECTURE.md). Hand-extracted from that row for readability; content is verbatim, not a rewrite.

Implementation: `backend/internal/webhook/dispatcher.go` (`MatchEvent` pure + `Dispatcher.Handle` orchestrator); wired via `cfg.WebhookDispatcher`. Creates one `Stage` row per spec-stage definition; first stage transitions to `dispatched` on workflow_dispatch.

## Branch-protection snapshot

(#251 / ADR-017) Between spec validation and run-create, `resolveRequiredChecks` calls `githubclient.GetBranchProtection` + `ListRulesetRequiredChecks` and unions the results into `run.RequiredChecksSnapshot{Contexts, Sources}`, persisted to `runs.required_checks_snapshot` (JSONB, migration 0017). No protection covers the target ref → `errNoBranchProtection` → refuse the run with a `webhook dispatch refused: branch protection` WARN log; missing `administration:read` scope → `errProtectionScopeMissing` so the log line names the operator-side fix (re-install per #252). `branch_protection_rule` and `repository_ruleset` webhook events are recognized by `MatchEvent` and skip-with-reason in v0 — the receiver acknowledges so a future cache layer has a path without changing the webhook contract.

## CI-failure retry

### Trigger

(#278 / E16) `check_run.completed` events route through `matchCheckRun`, which fires `MatchActionCIFailureRetry` when the conclusion is in `failedCheckRunConclusions` (the same closed set `stagecheck.DeriveState` uses for the "fail" pill), the check name isn't `fishhawk_audit_complete` (retrying won't fix Fishhawk's own audit gaps), and `pull_requests[]` is non-empty. The `Match` carries a `CheckRunRef{PRNumber, HeadSHA, CheckName, Conclusion}` for the handler.

### Handler

(#279 / E16) `handleCIFailureRetry` reads the parent run via `ListRuns({PullRequestURL: …})`, filters to checks named in the parent's `required_checks_snapshot` (non-required failures are not merge blockers), dedups against existing runs that already recorded the same head_sha via their implement-stage `pull_request` artifact, resolves `on_ci_failure.max_retries` from `parent.WorkflowSpec` (cached at run-create per #283, defaulting to `spec.DefaultMaxRetries = 1`), and either creates a follow-up run with `ParentRunID = parent.ID`, `RetryAttempt = parent.RetryAttempt + 1` (migration 0020) and fires `workflow_dispatch` — or appends a `ci_retry_exhausted` chained audit row against the parent when the cap is hit.

### Retry cap snapshot

(#280 / E16) Every run-create path also stamps `runs.max_retries_snapshot` (migration 0021, default 1) — for the original-run path the value comes from the parsed spec via `workflowMaxRetries`; the retry-handler path copies the parent's value forward so a long-running chain shows the same N/M on every row. Surfaces on the SPA as a "Retry N/M" badge in the run-detail header and a "Retry #N" chip in the related-runs panel.

### Plan reuse on retry

**Variant A: retry runs skip the plan stage** (`filterOutPlanStages`); the implement-stage prompt builder (`server/prompt.go::loadApprovedPlanForRun`) walks `parent_run_id` up to `retryPlanChainDepth = 8` levels to find the parent's approved standard_v1 plan, so the retry runs against the same plan without re-prompting. Successful dispatches chain a `ci_failure_retry_dispatched` audit row against the child and best-effort post an issue comment (`issuecomment.Notifier.NotifyCIRetry`, `KindCIRetry`) with per-attempt dedup (payload carries `retry_attempt` and the audit-log scan matches both `kind` and `retry_attempt`, so redeliveries of the same `check_run.completed` are absorbed but a fresh attempt N+1 still announces itself). The dispatcher's optional `Artifacts artifact.Repository` is what the dedup guard reads; nil leaves the guard at "no, this head_sha isn't recorded" — the `max_retries` cap still bounds runaway retries.

## Review-stage merge signal

(ADR-018 / #312) `server/pullrequest_review_events.go` handles `pull_request.closed` with `merged=true` (transitions the run's review stage to `succeeded` + writes a `pr_merged` audit row naming the merger from `merged_by.login`) and `pull_request_review.submitted` (audit-only; `pr_approved_on_github` for `state=approved`, `pr_review_submitted` for everything else). The handlers look up the run by `runs.pull_request_url` (#216); PRs that aren't Fishhawk-managed skip cleanly.

## PR closed without merging

(#316) Transitions the review stage to `cancelled` + writes a `pr_closed_without_merge` audit row naming the closer from `sender.login`. The run-level state cascades to `cancelled` once every stage is terminal (existing state-machine behavior). Reopen is intentionally out of scope — terminal stages don't resurrect; the reviewer re-triggers via `/fishhawk run` and the new run threads off the cancelled parent via `parent_run_id`. Plan-stage approval flow (#238) unchanged; the review stage is now a read-only summary of PR-side activity. App manifest adds `pull_request_review` to default events.

## In-Fishhawk approval prune

(ADR-018 / #313) `server/approvals.go::handleSubmitApproval` returns `409 review_stage_managed_by_github` when targeted against a review stage and includes the run's `pull_request_url` in the error details. The slash-command path (`server/issue_approval.go::HandleApprovalCommand`) replies with a help message pointing at the PR instead of submitting an approval. Both surfaces continue to accept plan-stage approvals — Fishhawk's vote at plan time is independent and meaningful; GitHub has no equivalent. The workflow spec's `gates: [approval]` with `approvers.any_of: [...]` on review stages becomes informational (stored on the row for display, not enforced); teams that want strict approver enforcement configure branch protection's required-reviewers.

## SPA review-stage read-only summary

(ADR-018 / #314) `frontend/src/review/review-document.tsx` drops the in-Fishhawk `ApprovalPanel` for review stages. Header gains a "View on GitHub" affordance pointing at `artifact.pr_url`. A new Activity section reads `listRunAudit({stageId, limit})` and renders the three PR-side categories from #312 — `pr_merged` → "@x merged the PR", `pr_approved_on_github` → "@x approved", `pr_review_submitted` → "@x requested changes / commented / dismissed" — oldest first so reviewers scan a left-to-right timeline. Approvers panel renamed "Approvers (informational)" with copy noting branch protection's required-reviewers is the actual gate. Plan-stage approval flow in `plan-document.tsx` unchanged.
