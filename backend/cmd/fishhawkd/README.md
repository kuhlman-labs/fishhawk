# fishhawkd

Fishhawk control-plane daemon binary: the backend HTTP API server plus its operational subcommands
(`serve.go`, `migrate.go`, `token.go`, `audit_rehash.go`).

## Webhook receiver secrets

`FISHHAWKD_GITHUB_WEBHOOK_SECRET` (`--github-webhook-secret`) enables `POST /webhooks/github` (HMAC-verified);
when unset the endpoint responds 503 and `serve.go` warns.

`FISHHAWKD_GITLAB_WEBHOOK_SECRET` (`--gitlab-webhook-secret`) enables `POST /webhooks/gitlab` (E45.6 / #1860).
GitLab sends this secret VERBATIM in `X-Gitlab-Token` (no HMAC); when unset the endpoint responds 503.
Deliberately asymmetric with GitHub: an absent GitLab secret logs nothing (GitLab is optional — an absent-warn
would nag every GitHub-only deployment). The shared webhook delivery store (`webhook_deliveries` on Postgres,
else in-memory) is created when EITHER secret is set, so a GitLab-only deployment gets the store too.

## Work-management provider registration at startup (#1104)

`workmgmt_wiring.go` — `registerWorkmgmtProviders(cfg.GitHub, jiraClient, gitlabClient)`, called from
`serve.go`, registers each work-management provider gated on its OWN client:

- A configured **GitHub** client registers the `github_projects` work-item provider
  (`*githubclient.Client` satisfies the work-item `API` interface directly) **and** the
  product-feedback provider — the latter via `feedbackAPIAdapter`, since
  `FeedbackAPI.SearchOpenIssues` returns the workmgmt/github `MatchedIssue` type.
- A configured **Jira** client registers the `jira` work-item provider.
- A configured **GitLab** client registers the `gitlab` work-item provider
  (`*gitlabclient.Client` satisfies the gitlab `API` interface directly). It is gated on
  `FISHHAWKD_GITLAB_BASE_URL` + `FISHHAWKD_GITLAB_TOKEN` (all-or-warn, the jira precedent), built by
  `resolveGitLabClient` in `serve.go` (ADR-058 Phase 2, #1856).

An unconfigured client leaves that provider unregistered, and the affected endpoint keeps returning
**501** — the v0 not-yet-wired posture. This is the wiring behind #1104: `fishhawk_file_issue` /
`fishhawk_report_product_issue` answer 501 unless the providers are registered.

## Deployment-level work-management conventions override (ADR-058 Phase 2, #1856)

`FISHHAWKD_WORKMGMT_CONVENTIONS` names a YAML conventions file that `serve.go` reads and parses
(`loadConventionsOverride`) fail-fast at startup — an unreadable or invalid file aborts serve with a
precise error naming the path + cause. The parsed document is installed for **every** repo via
`server.SetConventionsLoader`, replacing the `Default()`-only stub. This is enough to run a
non-`github_projects` provider (e.g. `provider: gitlab`) end-to-end against a single-tenant
deployment; the true in-repo per-repo loader is deferred to #2022 (the server can't know which forge
to fetch `.fishhawk/work-management.yaml` from before the conventions declare the provider). The
run-absent GitHub installation-resolution branch in `workitems.go` is gated on
`provider == github_projects`, so a gitlab filing never attempts GitHub egress.
