# backend/internal/auditcheckpublisher

Audit-complete check-run publish (#231): posts the derived `fishhawk_audit_complete` state to GitHub as a Check Run on the implement-stage PR's head commit.

Closes the loop: Fishhawk computes the state, the SPA reflects it, the approval handler enforces it, AND GitHub's merge button (with branch protection) refuses the merge until Fishhawk reports `success`.

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
