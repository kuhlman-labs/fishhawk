# backend/internal/stagecheck

Stage blocking-check ingestion (#228): typed surface over the `stage_checks` table.

## Data model and API

The `stage_checks` table (migration 0015) records every observed state of every blocking check declared on a stage's gate. Append-only; the latest row per `(stage_id, check_name)` is what consumers see.

This package exposes the typed surface: `Append`, `LatestForStage`, `LatestForStageAndName`, `FindMatchingStages` (the (PR, head_sha, check_name) → stage_id lookup the GitHub ingester uses) and `FindMatchingStagesForGitLabPipeline` (the project-scoped `GitLabPipelineMatch` → `[]StageRef` lookup the GitLab ingester uses, below).

**Two writers** feed the table (E45.55 / #3490): the GitHub `check_run` ingester (`server/checkrun.go`) and the GitLab Pipeline Hook ingester (`server/gitlab_pipeline.go`), which records every pipeline event under the check name `gitlab/pipeline`. GitLab rows carry the pipeline's instance-global `object_attributes.id` in `stage_checks.gitlab_pipeline_id` (migration 0084, nullable BIGINT, `Check.GitLabPipelineID *int64`); GitHub rows carry NULL there.

State derivation lives in `DeriveState(status, conclusion)`:

- pass on `success`/`neutral`/`skipped`
- fail on `failure`/`timed_out`/`cancelled`/`action_required`/`stale`/`startup_failure`
- fail on `skipped_not_allowed` — the ONE non-GitHub conclusion: the GitLab ingester records a `skipped` pipeline under it when the run's `RequiredChecksSnapshot` does not carry `gitlab_allow_skipped_pipeline`, because GitLab itself would refuse the merge; a snapshot carrying the flag records plain `skipped` (pass)
- pending on anything still in progress or carrying a conclusion we haven't catalogued

### Precedence is structural (#3490)

Both latest-row readers (`queries.sql::ListStageChecksLatest`, `::GetStageCheckLatest`, mirrored verbatim in the Go consts in `db/queries.sql.go` — the Go const is what executes) order by

```
[check_name,] gitlab_pipeline_id DESC NULLS LAST, ts DESC
```

so within `(stage, gitlab/pipeline)` the **highest pipeline id is the latest row regardless of `ts` or webhook delivery order**. The same `head_sha` can carry several pipelines (retry / manual re-run / merge-request vs branch pipeline) and GitLab pipeline ids are monotonically increasing per instance, so the newest id is authoritative; an older id appended LATER, or carrying a LATER `ts`, can never become the latest row. Postgres `NULLS LAST` sorts every NULL after every non-NULL on that key and falls through to `ts` only among equal keys, so GitHub rows (all NULL) keep byte-identical pure-`ts` ordering. `DISTINCT ON (check_name)` requires the ORDER BY to lead with `check_name`, which the clause preserves.

This is a property of the SQL, **not a guard in the writer**. The ingester's `latest.GitLabPipelineID > incoming → skip` is write-avoidance only: with it deleted, a redundant older row is appended and the readers never surface it. `TestLatestForStageAndName_GitLabPipelineIDPrecedence` pins both readers against the Go const with the adversarial seed (id 100 carries the later `ts` and is appended last; the reader must return 101) plus a NULL-id check name that must stay `ts`-ordered.

### Project-scoped match (#3490)

`FindMatchingStagesForGitLabPipeline(ctx, GitLabPipelineMatch{Repo, InstallationRef, HeadSHA, MergeRequestIID})` (`queries.sql::FindRunStagesForGitLabPipeline`) walks `pull_request` artifact → implement stage → run → review stage(s) like `FindMatchingStages`, and returns `StageRef{StageID, RunID}` pairs (the ingester appends to the stage and re-runs the post-CI policy evaluation for the run). It requires **BOTH** `runs.repo = Repo` AND `runs.installation_ref = InstallationRef`: GitLab pipeline hooks are delivered per project, and a fork can carry the same `head_sha` and the same merge-request iid, so either predicate alone admits a decoy sharing the other. `MergeRequestIID` 0 disables the `pr_number` predicate (a branch pipeline still matches by sha). `TestFindMatchingStagesForGitLabPipeline_ProjectScoped` seeds one decoy per predicate — (same repo, different ref), (different repo, same ref) — plus a both-different decoy, so deleting either predicate reddens on the decoy only it excludes.

### RedVerdict vs StateFail (#3414)

`DeriveState` maps `cancelled` and `stale` to `StateFail` — a check superseded by a newer head reads not-green, keeping the SPA fail rendering and the conservative approval/merge-gate posture unchanged. But a superseded conclusion carries **no verdict about the code**: `cancelled` is what `ci.yml`'s `concurrency: cancel-in-progress` assigns to the in-flight run's check runs when a new push lands, and `stale` is what GitHub assigns when a check run is superseded.

Two predicates express that distinction WITHOUT touching `DeriveState`:

- `ConclusionSuperseded(conclusion)` — true for exactly `cancelled` and `stale`; nil and every other value false.
- `(*Check).RedVerdict()` — `State == StateFail && !ConclusionSuperseded(Conclusion)`. A `StateFail` with a nil conclusion stays red (production never produces it, so it can only come from a fake, and the conservative direction is to park).

**The drive `ci_failed` park keys on `RedVerdict`, not `StateFail`** (`server.go::reviewChecksFailed`), so a superseded cancelled/stale conclusion never trips `ci_failed`; the recovery lifecycle is in `backend/internal/drive/README.md`.

## Ingest and read paths

- Ingest (GitHub): `backend/internal/server/checkrun.go::ingestCheckRun` parses the `check_run` event, walks `pull_requests[]`, asks the repo for matching stages via `(pr_number, head_sha, check_name)`, and appends a row per match.
- Ingest (GitLab): `backend/internal/server/gitlab_pipeline.go::ingestGitLabPipeline` parses the Pipeline Hook, asks the repo for matching stages via the project-scoped `GitLabPipelineMatch`, appends a `gitlab/pipeline` row per match carrying the pipeline id, and re-runs the post-CI policy evaluation per run (contract: `backend/internal/server/README.md`).
- Read: `GET /v0/stages/{id}/checks` returns the gate's declared list + the latest observed state per name.

### Readers (#3489)

`FindMatchingStages` (`queries.sql::FindRunStagesForCheckRun`) and `FindMatchingStagesForGitLabPipeline` (`::FindRunStagesForGitLabPipeline`) both target the run's **review** stage(s) — it walks `pull_request` artifact → implement stage → run → every stage with `stage_type = 'review'` (#254), so the ingester's rows land on the review stage and on no other. Every server-side reader of the CI signal — the deploy gate's `ci_green` verdict (`server/approvals.go::deployCIGreenVerdict`) and the post-CI policy re-eval (`server/policy_reeval.go::reevaluateCIPolicyForPR`) — resolves the stage it passes to `LatestForStage` through `server.findCISignalStage`, the ONLY sanctioned way to pick that stage. Before #3489 both readers read the implement stage, which never receives a row, so the signal was permanently pending. Changing this SQL filter requires changing `findCISignalStage` in lockstep; `server/ci_signal_stage_pg_test.go` drives a real `check_run` through this repository into both readers and fails if the two disagree.

**The approval handler does NOT gate on this data as of #253 / ADR-017** — reviewers approve based on plan + diff; GitHub branch protection blocks the merge until the repository's required checks report green. `fishhawk_audit_complete` is **published** as a Check Run per #231, but whether it is one of those required checks is the repository's own branch-protection configuration, not something Fishhawk sets — the `merge_gate` readiness rung reconciles the published check against the forge and reports it (E64.44 / [#3161](https://github.com/kuhlman-labs/fishhawk/issues/3161)). The `stage_checks` table still feeds the review-page panel as informational live state.

The `fishhawk_audit_complete` self-derivation is the unfilled half of the same data model — see `backend/internal/auditcomplete/README.md` and `docs/architecture/audit-complete.md`.
