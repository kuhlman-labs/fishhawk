# backend/internal/auditcheckpublisher

Audit-complete check-run publish (#231): posts the derived `fishhawk_audit_complete` state to the run's forge on the implement-stage PR's head commit.

Closes the loop: Fishhawk computes the state, the SPA reflects it, the approval handler enforces it, AND the forge's merge button (with branch protection) refuses the merge until Fishhawk reports `success`.

## Forge routing (runner_kind guard, #1861)

The publish target is keyed on the run's `runner_kind`:

- `github_actions` / `local` (and legacy runs with an empty `runner_kind`) → a **GitHub Check Run** via the injected `CheckRunCreator` (`POST /repos/{owner}/{repo}/check-runs`), scoped by the run's `installation_id`. A nil `installation_id` is a non-GitHub-triggered run (CLI ad-hoc) and skips.
- `gitlab_ci` → a **GitLab commit status** via the GitLab forge (`forge.Forge.CreateCheckRun` → `POST /projects/:id/statuses/:sha`, #1859). A gitlab_ci run carries no `installation_id` and does not persist its scope on the run row, so the publisher resolves the `gitlab:<project_id>` credential scope from the repo path with `ResolveRepoScope`, then posts the status against the same `findHeadSHA`-resolved head the GitHub path uses. The shared `fishhawk_audit_complete` identity rides through as the commit-status `name`.

The GitLab forge is taken from `Deps.GitLab` when wired, otherwise resolved lazily from the process-global forge registry (`forge.Get("gitlab")`, which `serve.go` registers at startup) — so GitLab publishing needs no `server.New` change. A gitlab_ci run whose forge is neither injected nor registered, or whose scope resolution fails, surfaces a `Publish` error (best-effort, logged) rather than silently skipping the merge gate; a scope-resolution error does **not** advance the persistent-failure streak (it is a pre-publish read error, not a `CreateCheckRun` attempt). The dedup cache, failure episodes, and head resolution are forge-agnostic.

> GitLab-only deployments (no GitHub client) are not yet supported here: `New` still requires `Deps.GitHub`, matching today's `server.New` wiring (`auditcheckpublisher.New` is called only when `cfg.GitHub != nil`). Lifting that guard is server-side follow-up.

## State mapping

- `pending` → `status=in_progress`
- `pass` → `status=completed, conclusion=success`
- `fail` → `status=completed, conclusion=failure`, with the `missing[]` list rendered as a markdown summary on the check

`details_url` points at `<ExternalURL>/runs/<id>` so a reviewer who clicks the check on github.com lands in Fishhawk.

## Hook points and failure posture

`server/checks.go::publishAuditCheck` is called after every `auditcomplete.Compute` in both the read endpoint (`handleListStageChecks`) and the gate-enforcement path (`deriveAuditCompleteState`).

Best-effort: a publish failure logs at WARN but doesn't unwind the in-Fishhawk gate.
A PERSISTENT failure (`auditcheckpublisher.DefaultDegradedThreshold` = 5 consecutive `CreateCheckRun` failures per `(run_id, head_sha)` episode, #993) additionally surfaces on the run chain as paired `audit_check_publish_degraded` / `audit_check_publish_recovered` audit entries via the publisher's `OnDegraded`/`OnRecovered` callbacks (wired in `server.New`; pairing is restart-proof because episode closure derives from the audit chain, not the in-memory counter — see `docs/architecture/audit-complete.md` § Reconcile sweep).

Dedup is process-lifetime in-memory keyed by `(repo, head_sha)` → most-recent published state — re-publishing identical state on every read would be wasteful.

## Configuration

New `Config.ExternalURL` (env `FISHHAWKD_EXTERNAL_URL`) is required for the publisher to wire up; absent it, `auditcheckpublisher.New` returns nil and `publishAuditCheck` is a no-op.

GitHub client method: `githubclient.Client.CreateCheckRun` (`POST /repos/{owner}/{repo}/check-runs`); the App's `checks: write` permission was already in `docs/github-app/manifest.template.json`.

**Customer setup**: after the App is installed, repo admins must mark `fishhawk_audit_complete` as a Required status check in branch protection — otherwise the check is informational only.
