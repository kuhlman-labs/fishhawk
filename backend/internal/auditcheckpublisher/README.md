# backend/internal/auditcheckpublisher

Audit-complete check-run publish (#231): posts the derived `fishhawk_audit_complete` state to the run's forge as a commit status on the implement-stage PR's head commit.

Closes the loop: Fishhawk computes the state, the SPA reflects it, the approval handler enforces it, AND the forge's merge gate (with branch protection) refuses the merge until Fishhawk reports `success`.

## Forge routing by runner_kind

`Publish` selects the forge from the run's `runner_kind` (#1861 / E45.8, ADR-058):

- `github_actions` / `local` (and any legacy/unknown kind) → a GitHub Check Run (`POST /repos/{owner}/{repo}/check-runs`), authenticated against the run's `installation_id`. The `installation_id == nil` skip lives under this branch — a run without a GitHub installation (CLI ad-hoc) is informational-only.
- `gitlab_ci` → a GitLab commit status. The publisher resolves the registered GitLab forge (`forge.Get("gitlab")`, or the `Deps.GitLab` override in tests), resolves the repo's `gitlab:<project-id>` credential scope via `ResolveRepoScope`, then calls the same `forge.CreateCheckRun` (the GitLab forge maps it to `POST /projects/:id/statuses/:sha`, #1859). No GitHub installation id is required, so the GitHub-branch `installation_id` guard does not skip it.

The GitLab lookup is **nil-safe**: an unconfigured GitLab forge (not registered, no override) skips the `gitlab_ci` run best-effort — mirroring the GitHub nil-installation skip — rather than erroring. A `ResolveRepoScope` failure, by contrast, surfaces as a `Publish` error (never a silent publish to a wrong scope). This is dormant plumbing: no `gitlab_ci` run is created yet (go-live is enablement follow-up #2043), so the GitLab path is exercised only by unit tests today.

## State mapping

- `pending` → `status=in_progress`
- `pass` → `status=completed, conclusion=success`. When `PublishResult` is handed a non-empty `resolved[]` (#3092), the summary gains one line per resolution NAMING the child runs whose implement-stage traces satisfied the parent fan-out stage, so an auditor can follow the resolution chain from the forge itself. A pass with no resolutions renders the pre-#3092 summary byte-identically.
- `fail` → `status=completed, conclusion=failure`, with the `missing[]` list rendered as a markdown summary on the check

`details_url` points at `<ExternalURL>/runs/<id>` so a reviewer who clicks the check on github.com lands in Fishhawk.

## Hook points and failure posture

`server/checks.go::publishAuditCheck` is called after every `auditcomplete.ComputeResult` in both the read endpoint (`handleListStageChecks`) and the recompute/republish path (`recomputeAndPublishAuditComplete`, which the merge reconciler reaches; the pull_request webhook handler `republishOnPullRequestEvent` calls `publishAuditCheck` directly). It forwards the resolutions to `PublishResultAtHead` with an empty override (via `publishAuditCheckAtHead`); `PublishResult` and `Publish` remain thin wrappers delegating with an empty override / `resolved=nil`. The operator-vouch path reaches the same seam with a non-empty override — see **Head override** below.

### Publish-surface inventory

Every server-side path that reaches this package, so a call-site audit does not have to re-derive the list:

| Call site | Trigger |
|---|---|
| `server/checks.go::handleListStageChecks` | `GET /v0/stages/{id}/checks` — the SPA read (this is also the accidental HEALER: reading the endpoint republishes) |
| `server/pullrequest_synchronize.go::republishOnPullRequestEvent` | `pull_request` `opened` / `reopened` / `synchronize` (a fix-up push lands here, publishing `in_progress` against the NEW head) |
| `server/pullrequest.go` (fix-up push report) | the runner's `fixup_pushed` outcome report |
| `server/trace.go` (implement-review loop) | an advisory implement review reaching a terminal verdict |
| `server/review_reconcile.go` | review reconciliation |
| `server/vouch.go` | the operator-vouch re-post (the one `PublishResultAtHead` override caller) |
| `server/checks.go::RepublishAuditCheck` | the merge reconciler's per-tick heal sweep, over review stages parked at `awaiting_approval` |
| **`server/checks.go::republishAuditCheckBeforeMerge`** (E64.42 / #3159) | **immediately before a merge dispatch**, from BOTH `merge_run.go::handleMergeRun` and `autodrive.go::dispatchAcceptanceGatedMerge` |
| **`server/checks.go::republishAuditCheckOnRunTerminal`** (E64.42 / #3159) | **after the run reaches its terminal state on a merge**, from BOTH merged arms of `pullrequest_review_events.go::resolveReviewStageOnMerge` |

The last two exist because none of the seven above is guaranteed to fire between a fix-up push's `synchronize` and the merge, and merging removes the run from the heal sweep (it enumerates only PARKED review stages) — so a check left at `in_progress` by the fix-up push stayed `in_progress` on the merged head forever. See `docs/architecture/audit-complete.md` § Reconcile sweep for the ordering constraint on the terminal half and the named stale-`success`-to-`failure` consequence of the pre-merge half.

The dedup cache is UNCHANGED by #3092 — it still keys on `(forge, repo, head_sha, state)` only, so the resolution text lands with the FIRST pass publish at a given head; a pass already published at that head is not re-published merely because its summary would now name the child runs.

## Run-less not-applicable publish (E64.43 / #3160)

`PublishNotApplicable(ctx, repo, scope, headSHA)` posts a **terminal, non-blocking** `fishhawk_audit_complete` Check Run for a pull request that has **no Fishhawk run at all** — a Dependabot bump, a human hotfix, an operator-authored docs PR. Without it, a repo that has marked the check Required leaves every such PR permanently blocked on a context nothing would ever publish (#3160).

**Signature is deliberately RUN-LESS.** It takes `repo` / `scope` / `headSHA` directly rather than a `runID`, because there is no run row, no implement-stage `pull_request` artifact, and therefore nothing for `GetRun` / `findHeadSHA` to resolve.

**Wire shape**: `status=completed`, `conclusion=neutral`, `output.title = "Not a Fishhawk-managed change"`, `details_url = <ExternalURL>` (the root — there is no run to link), and a summary that states plainly that no Fishhawk run is associated with the PR and the audit gate does not apply. It is worded so it CANNOT read as an audit pass: the pass summary's phrase `audit chain verifies` is deliberately absent, and `TestPublishNotApplicable_PublishesNeutralCompleted` asserts that absence as well as the not-applicable wording's presence.

**Why `neutral`.** GitHub treats `success`, `neutral` and `skipped` as satisfying a required status check, and `neutral` stays semantically honest about the fact that nothing was verified. If live validation ever refutes that, the forward fix is a **one-line** change of `forge.CheckRunConclusionNeutral` to `forge.CheckRunConclusionSuccess` in `buildNotApplicableParams` — the summary text already reads unambiguously as not-applicable — not a revert.

**Dedup sentinel.** The path consults the SAME `(forge, repo, head_sha)` cache under a package-local `stateNotApplicable = stagecheck.State("fishhawk_not_applicable")`, so repeated `opened` / `reopened` / `synchronize` events at one head post once. It is deliberately NOT a new member of the `stagecheck` enum: nothing outside this package derives, persists or switches on it, so promoting it would move exhaustiveness tables in every consumer for a value none of them can see.

The invariant that makes sharing the cache safe: the cache stores the last published state as its VALUE and `shouldPublish` compares that value against the incoming state, so a `stateNotApplicable` entry can never suppress a later real pass/fail/pending at that head, and a real state can never suppress a not-applicable. `TestPublishNotApplicable_SentinelDoesNotSuppressRealState` pins this in BOTH orders — it is the test that fails if the sentinel is ever collapsed into an existing enum member.

**Deliberate omissions**, both observable in tests rather than merely claimed:

- **No degraded-failure EPISODE.** Episodes key on `(run_id, head_sha)`; a run-less failure would open one under `uuid.Nil` that no later publish could ever close. The path DOES route through `recordPublished` on success, which calls `clearEpisode(uuid.Nil, headSHA)` — a map lookup that always misses and returns 0, because no episode is ever created under that key. So the episode side is READ (harmlessly) but never WRITTEN, and `OnDegraded` / `OnRecovered` are never invoked from this path. `TestPublishNotApplicable_CreateCheckRunError` drives `DefaultDegradedThreshold+1` consecutive failures and asserts `OnDegraded` fires zero times; `TestPublishNotApplicable_SuccessOpensNoEpisode` asserts the success path fires `OnRecovered` zero times.
- **GitHub-only.** `runner_kind` is a property of a run, and a run-less PR has none — there is no signal to route a GitLab commit status on.

**Fail-closed guards**, each `(false, nil)` with zero forge calls and each with its own test case in `TestPublishNotApplicable_GuardModes` / `_NilReceiver`: nil receiver (publisher disabled — so the call site needs no branch), empty `repo.Owner`, empty `repo.Name`, empty-or-whitespace `headSHA`, and the zero `forge.CredentialScope` (nothing to authenticate with).

The caller-side discriminator that keeps a Fishhawk-managed PR from ever reaching this method is in `backend/internal/server` — see that package's README.

## Head override (E64.14 / #3109)

`PublishResultAtHead(ctx, runID, state, missing, resolved, headSHAOverride)` is `PublishResult` plus an explicit head. When `headSHAOverride` is non-empty the Check Run is published AT THAT SHA instead of the head `findHeadSHA` would resolve; everything downstream — repo/forge resolution, the `(forge, repo, head_sha, state)` dedup cache, the `(run_id, head_sha)` degraded-episode tracking, and `buildParams` — keys on the RESOLVED (overridden) head, so an override publishes and dedups against the overridden sha with **no** special-casing. `PublishResult` (and thus every pre-#3109 caller) delegates here with an empty override, so its head resolution, dedup and episode keys are byte-identical.

The override exists for the **operator-vouch path** (`server/vouch.go` via `checks.go::recomputeAndPublishAuditCompleteAtHead`): the live PR head there is an operator-pushed commit that by construction appears in NO head-report audit category (`fixup_pushed` / `child_pushed` / `pull_request_opened`), so `findHeadSHA` would resolve the stale audit-recorded head and republish the required check against a sha the operator's merge commit is not on — the base-advance wedge. Passing the vouched sha as the override re-posts the check on the live head. Because the merge reconciler's heal (`RepublishAuditCheck`) uses the empty-override path, it CANNOT recover a dropped vouch re-post; re-invoking the vouch (which re-runs this override publish) is the idempotent retry, and the dedup cache — recording only successes — makes it a no-op once the check is live.

Best-effort: a publish failure logs at WARN but doesn't unwind the in-Fishhawk gate.
A PERSISTENT failure (`auditcheckpublisher.DefaultDegradedThreshold` = 5 consecutive `CreateCheckRun` failures per `(run_id, head_sha)` episode, #993) additionally surfaces on the run chain as paired `audit_check_publish_degraded` / `audit_check_publish_recovered` audit entries via the publisher's `OnDegraded`/`OnRecovered` callbacks (wired in `server.New`; pairing is restart-proof because episode closure derives from the audit chain, not the in-memory counter — see `docs/architecture/audit-complete.md` § Reconcile sweep).

Dedup is process-lifetime in-memory keyed by `(repo, head_sha)` → most-recent published state — re-publishing identical state on every read would be wasteful.

## Configuration

New `Config.ExternalURL` (env `FISHHAWKD_EXTERNAL_URL`) is required for the publisher to wire up; absent it, `auditcheckpublisher.New` returns nil and `publishAuditCheck` is a no-op.

GitHub client method: `githubclient.Client.CreateCheckRun` (`POST /repos/{owner}/{repo}/check-runs`); the App's `checks: write` permission was already in `docs/github-app/manifest.template.json`.

**Customer setup**: after the App is installed, repo admins must mark `fishhawk_audit_complete` as a Required status check in branch protection — otherwise the check is informational only.
