# backend/internal/server

fishhawkd's HTTP surface: route handlers for the v0 REST API and the
cross-component seams they anchor.

## Index

This README is an INDEX (split 2026-10-29). Each section below lives, verbatim, in the topic file named; a reference of the form `server/README.md` § "<title>" resolves through this table. Add a NEW section to the topic file it belongs to (or a new topic file), and add one row here.

### [Authorization, identity and sessions](docs/auth.md)

- 5xx information-disclosure chokepoint (`errors.go`, E67.15 / #2587)
- Account-ownership authorization (ADR-057 / E44.5, #1829)
- Repo-scoped in-workspace visibility (`repovisibility.go`, ADR-057 Amendment A2 / E44.10, #2071)
- Change-author attribution and `not:` enforcement (#2358)
- `approvals.members` enforcement (#4116)
- The approve advance is a compare-and-swap (`approvals.go`, E50.15 / #2656)
- Approval-hook self-protection: one CAS, one subordinate lock (`live_validation_filing.go` / `split_filing.go`, E50.16 / #2657)
- Slash-command approval (#238)
- Webhook delegate secret channel (`deploy_trigger.go` / `deploy_rollback.go`, E45.57 / #3497)
- Onboarding + session security
- MCP surface on fishhawkd (`mcproute.go`, ADR-076 slice 2 / E66.2 #2390)
- OAuth 2.1 authorization server (`oauthas.go` / `oauthauthorize.go` / `oauthtoken.go`, ADR-076 slice 3 / E66.19 #2436)
- Federated identity providers (E66.4 / #2392)
- Membership-gate denial page (`accessdenied.go`, E44.31 / [#2467](https://github.com/kuhlman-labs/fishhawk/issues/2467))

### [Captain, crew, delegation and historian surfaces](docs/crew.md)

- Delegation read (`delegation_view.go`, E76.1 / #3747)
- Precedent query (`precedent.go`, E75.3 / #3731, ADR-082 #3728 decision (b))
- Precedent at the gate (`gate_precedent.go`, E75.4 / #3732, ADR-082 #3728 decision (c))
- Divergence at the gate (`gate_divergence.go`, E75.5 / #3733, ADR-082 #3728 decision (d), rule 5)
- Attention queue (`attention.go`, E40.1 / #1713)
- Restart blockers (`restart_blockers.go`, E83.30 / #3974)
- Repo dashboard rollups (`repodash.go`, E40.3 / #1714)
- "Since you last looked" digest (`digest.go`, E75.6 / [#3734](https://github.com/kuhlman-labs/fishhawk/issues/3734))
- Delegation confirmation (`delegation_confirm.go`, E76.5 / [#3768](https://github.com/kuhlman-labs/fishhawk/issues/3768))
- Captain record (`captain.go`, E76.2 / [#3765](https://github.com/kuhlman-labs/fishhawk/issues/3765))
- Crew messages (`crewmessage.go`, E77.3 / [#3737](https://github.com/kuhlman-labs/fishhawk/issues/3737))
- First-officer seat (E85.6 / [#4148](https://github.com/kuhlman-labs/fishhawk/issues/4148))
- Handover brief (`handover_brief.go`, E76.4 / [#3767](https://github.com/kuhlman-labs/fishhawk/issues/3767))
- Delegation shadow stamp (`delegation_shadow.go`, E82.1 / [#3778](https://github.com/kuhlman-labs/fishhawk/issues/3778))

### [Plan and implement review, concerns and plan gates](docs/review.md)

- Charter injection consumer (`charter_injection.go`, E54.2 / #2234)
- Review-convention consumer (`review_conventions.go`, E55.3 / #2244)
- Reviewer personas (`reviewer_persona.go`, ADR-084 #3752 / E55.8 #3753)
- Plan-review seed (#4436)
- Plan-gate scope/constraint pre-check (#658)
- Plan-gate test sweep (#942)
- Routed-concern NOT-ATTEMPTED pre-review signal (#2896)
- Routed-reporting-obligation undelivered pre-review signal (#2737)
- Operator-scope-undelivered pre-review signal (#1407)
- Stale-review detection: `review_head_mismatch` (#3655, comparison rewritten by #3665)
- Retry supersedes the prior review round (#3593)
- Re-review convergence: settled ledger + re-litigation guard (#1913)
- Per-resolution reopen veto + the `reject_without_concern` advisory (`trace.go`, E68.41 / #3319)
- Round-level concern-resolution veto (`trace.go`, E48.103 / #2551)
- Clean-round auto-close of a routed concern (`trace.go`, E45.84 / #3619)
- Diff secrets check (`diff_secrets.go`, E80.3 / #3760, ADR-084 D5 / rule 5)
- Permission-drift check (`permission_drift.go`, E80.4 / #3761, ADR-084 D5 / rule 5)
- Runner-observed counterfactuals (`counterfactual_observed.go`, E85.3 / #4143)
- Gate decision view (`gateview.go`, E48.13 / #1960)
- Run concern listing (`run_concerns.go`, #4101)
- Plan-gate surface sweep (#763)
- Plan-approval pre-Submit gate: add_scope_files fan-in (#2103)
- Human-executor review-gate admission (`review_gate_admission.go`, E54.53 / [#3041](https://github.com/kuhlman-labs/fishhawk/issues/3041))
- Trace-time policy re-evaluation (`trace.go::reEvaluatePolicy`)
- Ship-time plan gates
- Scope-completeness exempt emission gate (#2501)
- Build-required scope-drift park — the third shortfall class (E48.101 / #2548)
- Amend-and-resume a scope-completeness park (`scope_completeness_amend.go`, E67.18 / [#2591](https://github.com/kuhlman-labs/fishhawk/issues/2591))
- Acceptance-criteria amendment at the plan gate (`acceptance_amendments.go`, E67.11 / #2581)
- Bulk concern waive + the condition-claim shorthand (`bulk_waive.go`, `condition_claims.go`, E64.77 / #3318)
- Merge readiness + operator-evidence resolve (`merge_readiness.go`, `resolve_concerns.go`, `gateview.go`, E83.53 / [#4086](https://github.com/kuhlman-labs/fishhawk/issues/4086))
- Escape records (`escape.go`, E94.3 / [#4694](https://github.com/kuhlman-labs/fishhawk/issues/4694))
- Child-slice concern rollup, the late child-review import and the `open_at_merge` merge stamp (`concern_rollup.go`, E94.1 / [#4692](https://github.com/kuhlman-labs/fishhawk/issues/4692)) — contract in [`backend/internal/concern/README.md`](../concern/README.md) § "Child-slice rollup + the outcome column", not a topic file here

### [Acceptance stage and live validation](docs/acceptance.md)

- Acceptance stage seam (E31, ADR-049 / ADR-050)
- Replayable scenario corpus — backend half (E72.4 / #3328)
- Pre-spawn acceptance-dispatch admission (E31.23 / #1928)
- Plan-gate acceptance pre-check (#1533, ADR-049 decision #4)
- Acceptance-stage omission at plan approval (`acceptance_omission.go`, E72.1 / #3325)
- Acceptance failure triage (E31.8 / #1536, ADR-049 decision #2)
- Live-validation walk auto-filing (`live_validation_filing.go`, E48.35 / #2045)

### [Run branches, merge candidates and the merge queue](docs/merge.md)

- Run-less `fishhawk_audit_complete` publish + App-identity discriminator (`pullrequest_synchronize.go` / `webhook.go`, E64.43 / [#3160](https://github.com/kuhlman-labs/fishhawk/issues/3160))
- Safe CI re-trigger (`retrigger_ci.go`, E83.49 / [#4082](https://github.com/kuhlman-labs/fishhawk/issues/4082))
- PR-reopen revive (`pullrequest_reopen.go`, E83.49 / [#4082](https://github.com/kuhlman-labs/fishhawk/issues/4082))
- Default-branch revert detection (`merge_outcome.go`, E82.2 / [#3779](https://github.com/kuhlman-labs/fishhawk/issues/3779))
- Run-branch operator-vouch remediation (ADR-035 / #1044)
- Run-branch base advance (E64.23 / #3125)
- Bounded conflict-resolution pass (E64.62 / #3202)
- Operator verify-only re-run of a failed merge-candidate head (#4391)
- Operator-vouched base merges (ADR-090 D8 Amendment 1, #4368)
- PR-open checkpoint resume gate (E48.46 / #2169)
- Held-commit PR text (`heldcommitpr.go`, E67.5 / [#2570](https://github.com/kuhlman-labs/fishhawk/issues/2570))
- Partial delivery (E83.52 / #4085)
- Merge-supersede sweep + `reconcile-merge` recovery (`merge_supersede.go`, E64.2 / #3083)
- Merge-observation verb (`merge_observation.go`, E64.32 / #3136, E64.40 / #3151, E45.87 / #3622)
- Terminal run-branch sweep (`run_branch_sweep.go`, E68.67 / #3562)

### [Merge-candidate verification and the merge queue](docs/merge-queue.md)

- Merge-candidate held-pass queue (`merge_candidate_queue.go`, ADR-092 / E83.85 #4200)
- Merge queue (ADR-093 slice 1, E87.4 / [#4295](https://github.com/kuhlman-labs/fishhawk/issues/4295))
- Merge overlap guard (`merge_overlap_guard.go`, E83.61 / [#4119](https://github.com/kuhlman-labs/fishhawk/issues/4119))
- Merge-candidate verdict transfer (ADR-096 D1/D2/D5, ADR-098 D3, E87.2 / #4293)
- Consolidated fan-in verify hold (`fanin_verify.go`, ADR-090 D5 / #4018, #4812)

### [Host dispatch, decomposition, stage lifecycle and fix-ups](docs/dispatch.md)

- Writer-side anchor intent marker + gate (`operator_visible.go`, #3406)
- Preview/dev-mode host-dispatch refusal (`devmode.go`, E72.13 / #3500)
- Interim host memory hold (`host_dispatch.go`, E85.11 / [#4330](https://github.com/kuhlman-labs/fishhawk/issues/4330))
- Decomposition wave-order host-dispatch guard (`decomposition_dispatch_guard.go`, E48.99 / #2546)
- Per-wave integration guard + `base_branch` derivation (`decomposition_dispatch_guard.go`, E50.13 / #2363)
- Decomposed-parent fix-up policy increment (#4391)
- Decomposed-parent review scope baseline (#4425)
- Concurrency-group promote (`stage_concurrency_promote.go`, E83.81 / [#4191](https://github.com/kuhlman-labs/fishhawk/issues/4191))
- Stage terminal-wait long-poll (#1252, E24.X)
- Runner terminal-failure self-report on reap-failure (`reap_failure.go`, E45.73 / #3598)
- Verify-failure (reverify) held-commit resume (`prompt.go` / `pullrequest.go`, E83.80 / [#4190](https://github.com/kuhlman-labs/fishhawk/issues/4190))
- Stage progress heartbeat ingest (`stage_progress.go`, E48.96 / #2541)
- Fix-up concern delivery: route-time fit + id pairing (#4284)
- Fix-up delivered-nothing refund — one chokepoint (#3085)
- Fix-up delivered-nothing round closure (#3395)
- Fix-up re-review backstop (#1932)
- Default-branch resolution (`default_branch.go`, #4660)
- Slice-ownership guard on mid-stage scope amendments (`slice_ownership.go`, E89.40 / [#4679](https://github.com/kuhlman-labs/fishhawk/issues/4679)) — kept in THIS file, below, until it moves to `docs/dispatch.md`

### [Work management, grooming, comms and upkeep](docs/workmgmt.md)

- Per-repo work-management conventions loader (`conventions_loader.go`, E45.16 / #2022)
- On-approval grooming apply (`grooming_apply.go`, E54.19 / [#2822](https://github.com/kuhlman-labs/fishhawk/issues/2822))
- Shared proposal-report seam (`report_seam.go`, [#4012](https://github.com/kuhlman-labs/fishhawk/issues/4012))
- Comms scan gather (`comms_scan.go` / `comms_record.go`, E81.5 / [#4014](https://github.com/kuhlman-labs/fishhawk/issues/4014))
- On-approval comms apply (`comms_apply.go`, [#4017](https://github.com/kuhlman-labs/fishhawk/issues/4017), E81.5 / #3775)
- On-approval upkeep apply (`upkeep_apply.go`, [#3924](https://github.com/kuhlman-labs/fishhawk/issues/3924), E79 / #3726)
- In-flight advisory findings (`upkeep_inflight.go`, [#3763](https://github.com/kuhlman-labs/fishhawk/issues/3763), E80.6)
- Per-entry grooming disposition capture (`grooming_dispositions.go`, E54.30 / [#2843](https://github.com/kuhlman-labs/fishhawk/issues/2843))
- Plan-stage terminal settle is shared across artifact siblings (`grooming_report.go` / `plan.go`, E54.29 / #2837)

### [REST endpoint families](docs/endpoints.md)

- Deploy-record environment label (`trace.go`, E23.18 / #2324)
- Run lifecycle endpoints
- Deploy rollback stored handle (`deploy_rollback_handle.go`, E35.3 / #1600)
- Artifact upload endpoints
- Release endpoints (ADR-051)
- Campaign REST API + rollup status (`campaigns.go`, ADR-047 / #1437, E25.4 / #1456, resume #1460)
- Read + export surfaces

## Slice-ownership guard on mid-stage scope amendments (#4679)

`slice_ownership.go`, wired in `scope_amendment.go`. Closes the slice-vs-slice fan-in hole (#4626 / #4668): a decomposition CHILD's mid-stage scope amendment may not widen its scope onto a path another slice of the parent's approved decomposition declares, unless that slice's commits are already in the requester's base.

- **Ancestor rule.** For each requested path and each OTHER slice `j` with a declared `scope.files`, an overlap (`scopePathsOverlap`: identical, or directory containment on segment boundaries; `overlap_kind` names which) is ALLOWED when `j` is in the requester's transitive `depends_on` closure (`sliceAncestors`) and REFUSED otherwise — a same-wave sibling, an earlier-wave non-dependency, or a descendant. Same-slice overlaps are never recorded. The structural closure is a subset of what may be in the base (an intermediate integration can fold in a non-dependency sibling), so the guard errs conservative. Reads only the plan's slice scopes and `depends_on`; repo-agnostic.
- **Request route** (`POST /v0/runs/{run_id}/scope-amendments`). After the budget check and BEFORE `Create`: a refusal answers 409 `amendment_path_owned_by_sibling_slice` naming each path, its owning slice index/title/child run id, the requester's slice and ancestor set, and the remedy (leave the edit to the owning slice, or defer it to a follow-up). No row is created, so no budget is spent and no page-class ping fires. A run or plan READ ERROR fails OPEN (WARN-log; the request proceeds with `slice_ownership: {checked: false, error}` on its `scope_amendment_requested` payload), because the decide route re-checks fail-closed. An admitted child request carries `slice_ownership` (`slice_index`, `ancestor_slices`, `ancestor_owned`, `conflicts: []`, `checked: true`); a non-child run's payload is byte-identical (key absent).
- **Decide route** (`POST .../{amendment_id}/decision`, `fishhawk_decide_scope_amendment`). An APPROVE of a PENDING row re-runs the check: a refusal answers the same 409 (the row stays pending) and tells the operator to DENY with the remedy as the reason. **There is no override.** A read error answers 500 `slice_ownership_check_failed` (retryable, row untouched): fail-CLOSED, so an approve can never land an unchecked conflicting path. Deny is never checked, and a non-pending row skips the check so `amendment_already_decided` / `amendment_lapsed` keep their precedence. An admitted child approve carries `slice_ownership` on `scope_amendment_decided`. Message-fidelity residual: a row still READING `pending` that has already lapsed (its stage or run went terminal but the lapse has not committed yet) can get the ownership 409 instead of `amendment_lapsed`; the row is untouched either way.
- **Nil-RunRepo skip.** The decide check is skipped when `RunRepo` is nil. That is reachable only in an unconfigured deployment: `cmd/fishhawkd/serve.go` wires `cfg.RunRepo` and `cfg.ScopeAmendmentRepo` in the same Postgres block (`InstallLapseHooks` takes both), and the request route answers 503 `scope_amendment_unconfigured` without a `RunRepo`, so such a deployment can file no pending amendment for the skip to admit (`TestDecideScopeAmendment_SliceOwnership` pins both halves). A nil `ArtifactRepo` is NOT a second skip: `loadApprovedPlanForRun` answers `(nil, nil)` without one, which would read as an ABSENT plan and admit the approve, so `amendmentSliceOwnership` returns an error for a decomposition child instead and the approve answers 500 `slice_ownership_check_failed` (the request route fails open on it as on any read error); pinned by the `nil ArtifactRepo fails closed` subtest.
- **Audit.** Every refusal appends `scope_amendment_refused` (INTERNAL; registered in `audit/categories.go`; no Notifier, not in `activityCategories`): phase `request` (actor agent, `{phase, paths, reason, slice_ownership}`) or phase `decide` (actor user, `{phase, amendment_id, paths, slice_ownership}`). The append is best-effort and never changes the outcome: an append failure still answers 409 with no row (request) or the row still pending (decide). The row names sibling slices' titles, paths and child run ids, so it is WITHHELD from run-bound audit reads (per-run read and global feed) by the interim deny-list entry in `withholdScopeOverlapFromRunBound` — the same deliberate deviation from ADR-106 as `escape_recorded`, which MUST migrate into ADR-106's per-category registry flag when it lands (owned by E89 [#4537](https://github.com/kuhlman-labs/fishhawk/issues/4537)); guarded until then by `TestWithholdScopeOverlapFromRunBound_ScopeAmendmentRefusedWithheld`. The withhold is defense in depth, not the only disclosure path, and #4537 should scope the registry flag with that in mind: the request-time 409 body (`details.conflicts[]`: `owning_slice`, `owning_title`, `owning_path`, `owning_child_run_id`) INTENTIONALLY names the conflicting sibling slice to the requesting run-bound agent, because the refusal must name the owning slice and child run (#4679 criterion 1) for the agent to act on the remedy; and an admitted child amendment's `scope_amendment_requested` / `scope_amendment_decided` payloads carry the same fields for ANCESTOR slices under `slice_ownership.ancestor_owned`, unwithheld, because `scope_amendment_requested` is the agent's own await anchor. Both disclose only the slices whose declared paths overlap the agent's own requested paths.
- **Coverage limit.** The guard covers ONLY the request and decide routes. `recover.go`'s `fishhawk_resume_run` `add_scope_files` and the scope-completeness `amend` decision fold paths without it; follow-up [#4798](https://github.com/kuhlman-labs/fishhawk/issues/4798).
- **Plan gate.** `sliceSharedPathOverlaps` lists every pair of distinct slices whose declared paths overlap EXCEPT raw-equal pairs (`plan.checkCrossSliceSharedFiles` already refuses those in every wave), i.e. directory containment or trailing-slash spelling, in the same wave or across waves. Each draws one `plan_warnings` advisory (`sliceSharedPathWarnings`, appended LAST so every earlier advisory keeps its position) naming the file, both slices and their waves, the dependency relation, and the remedy: a single owner for the file, or a final integration slice depending on both. The same overlap rides `plan_surface_sweep`'s `cross_slice_findings` as pattern `shared path across decomposition slices` (`sliceSharedPathFindings`), which `planGateEvidence` maps onto the plan-review prompt's existing CROSS-SLICE COUPLING line — no `backend/internal/prompt` change, so the review-prompt catch-rate evidence is not staled.
