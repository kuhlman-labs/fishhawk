# GitLab CI onboarding quickstart

How a GitLab project runs Fishhawk stages through GitLab CI/CD, using the
customer-side `.gitlab-ci.yml` template at
`backend/internal/onboarding/templates/.gitlab-ci.yml`. This is the GitLab
analog of the App-onboarded GitHub Actions workflow
(`.github/workflows/fishhawk.yml`): instead of a `workflow_dispatch` against a
composite action, the Fishhawk backend triggers a **pipeline** via the GitLab
pipelines API and the pipeline invokes the published, backend-agnostic
`fishhawk-runner` against the GitLab forge (`--forge=gitlab`).

> **Status — plumbing only (ADR-058 / #1861).** The `gitlab_ci` runner backend
> is dormant: no `gitlab_ci` run is created yet, so this template is exercised
> only by unit/wire tests. Go-live enablement (run creation, image publishing,
> credential wiring) is tracked in **#2043**. The steps below describe the
> intended operator path once enablement lands.

## What the template does

The `fishhawk` job runs only when the backend triggered the pipeline with the
Fishhawk stage inputs set — its `rules:` gate requires `$FISHHAWK_RUN_ID` and
`$FISHHAWK_STAGE_ID`, so an ordinary branch push or merge-request pipeline is
skipped and the file never runs a stage on routine commits.

When it does run, it invokes the published runner image
(`ghcr.io/kuhlman-labs/fishhawk-runner:v1` — never a local checkout) with:

```sh
fishhawk-runner \
  --forge gitlab \
  --gitlab-base-url "$CI_SERVER_URL" \
  --backend-url "$FISHHAWK_BACKEND_URL" \
  --run-id "$FISHHAWK_RUN_ID" \
  --stage-id "$FISHHAWK_STAGE_ID" \
  --workflow "$FISHHAWK_WORKFLOW_ID" \
  --stage "$FISHHAWK_STAGE" \
  --agent "$FISHHAWK_STAGE" \
  --fetch-prompt --upload-trace \
  --plan-out /tmp/fishhawk-plan.json \
  --check-base-ref "origin/$CI_DEFAULT_BRANCH"
```

`--fetch-prompt` resolves the real stage work from `FISHHAWK_STAGE_ID`, so the
run/stage identity is what is load-bearing; `--forge=gitlab` routes the push +
open-merge-request path through `FISHHAWK_GITLAB_TOKEN` against this instance
(`$CI_SERVER_URL`). Bump the pinned image tag when you adopt a newer runner
release.

## Variables the backend supplies

The backend passes these as pipeline (trigger) variables — they take precedence
over any `.gitlab-ci.yml` default:

| Variable | Meaning |
|---|---|
| `FISHHAWK_RUN_ID` | Workflow run UUID (supplied by the dispatcher). |
| `FISHHAWK_STAGE_ID` | Stage UUID for this dispatch. |
| `FISHHAWK_WORKFLOW_ID` | Workflow ID from `.fishhawk/workflows.yaml`. |
| `FISHHAWK_STAGE` | Stage executor ref / agent provider (`claude-code`\|`codex`); defaults to `claude-code`. |
| `FISHHAWK_PARENT_RUN_ID` | Decomposition-parent run UUID — set **only** for fan-out children. |

The pipeline ref the backend triggers on is the run's sole-writer branch
(`fishhawk/run-<short>`, or `fishhawk/run-<short>/slice-<n>` for a
decomposition child; ADR-035).

### Fan-out serialization

The job's `resource_group` is `fishhawk-run-$FISHHAWK_PARENT_RUN_ID`. The
template defaults `FISHHAWK_PARENT_RUN_ID` to `$CI_PIPELINE_ID`, so each
top-level run gets a unique resource group and never waits. The backend
overrides it for a decomposition child, so a fan-out's siblings share one key
and serialize — the GitLab analog of the GitHub concurrency group
`parent_run_id || github.run_id`.

## Prerequisites (operator-configured CI/CD variables)

Under **Settings → CI/CD → Variables**, configure:

- **`FISHHAWK_BACKEND_URL`** — the Fishhawk backend base URL the runner ships
  its trace bundle to and fetches prompts from.
- **`FISHHAWK_GITLAB_TOKEN`** (masked) — a project or group access token the
  runner pushes the run branch and opens the merge request with. This is the
  `--forge=gitlab` push-path credential.
- **`ANTHROPIC_API_KEY`** (masked) — forwarded to Claude Code when
  `agent=claude-code`.
- **`OPENAI_API_KEY`** (masked) — forwarded to the Codex CLI when `agent=codex`.

### Backend deployment configuration (Helm)

The CI/CD variables above configure the **GitLab side** (the runner's push
credential and the agent API keys). The **backend** (`fishhawkd`) is configured
separately through the Helm chart's `FISHHAWKD_GITLAB_*` family (E45.32 / #2922):

- Non-secret values — the `gitlab.*` block: `baseUrl`, `oauthClientId`,
  `oauthCallbackUrl`, `deviceClientId`, `installationHostAllowlist`.
- Secret values — `secrets.values.gitlab*`: `gitlabToken`
  (`FISHHAWKD_GITLAB_TOKEN`), `gitlabWebhookSecret`
  (`FISHHAWKD_GITLAB_WEBHOOK_SECRET`), `gitlabOauthClientSecret`
  (`FISHHAWKD_GITLAB_OAUTH_CLIENT_SECRET`).

**Mind the near-identical names.** `FISHHAWK_GITLAB_TOKEN` (the CI/CD variable
above) is the **runner's** push credential — it pushes the run branch and opens
the merge request. `FISHHAWKD_GITLAB_TOKEN` (chart secret, note the trailing
`D`) is the **backend's** REST credential — it gates the forge/work-item provider
and the login-gate group lister. They read alike and serve different sides; set
whichever the side you are configuring needs.

See [`deploy/helm/fishhawk/README.md`](https://github.com/kuhlman-labs/fishhawk/blob/main/deploy/helm/fishhawk/README.md)
(the "GitLab" section) for the full chart contract: the graduated enablement
(base-URL-alone is a supported login-gate posture), the all-three-or-none OAuth
trio guard, and which secrets are required when.

## Onboarding the file

Unlike the GitHub App-PR scaffold — which seeds four files including
`.github/workflows/fishhawk.yml` — the GitLab template is **not** part of the
default `ScaffoldFiles` map (a GitHub scaffold with a stray `.gitlab-ci.yml`
would be dead config). It is embedded additively and surfaced through
`onboarding.GitLabCITemplate()`; the GitLab onboarding path (enablement #2043)
writes it into the project's default branch as `.gitlab-ci.yml`.

## See also

- `backend/internal/onboarding/templates/fishhawk.yml` — the GitHub Actions
  counterpart this template mirrors.
- [`docs/ARCHITECTURE.md`](../ARCHITECTURE.md) §10 — the "Where to look" row for
  the GitLab CI onboarding template and the surrounding GitLab forge surface.

## Go-live: GitLab-triggered runs (E45.22 / #2043)

Run creation from a GitLab trigger is live as of #2043. To turn it on for a deployment:

1. **Register the project as an installation.** GitLab run creation is authorization-gated and FAILS CLOSED: fishhawkd acts only on projects an operator has registered, because a GitLab delivery is authenticated by a shared `X-Gitlab-Token` with no signature over the body — the project a payload names proves nothing on its own. Register an `installations` row for the project under an account whose `account_key` is the project path's namespace segment, using the `fishhawkd` subcommands (E45.33 / #2923) — direct-DB, no running server, so run them wherever `FISHHAWKD_DATABASE_URL` reaches Postgres:

   ```sh
   # account_key is the namespace segment of path_with_namespace ("acme" for acme/widgets).
   # --granularity defaults to "group" for gitlab; pass it explicitly to override.
   fishhawkd account create \
     --db "$FISHHAWKD_DATABASE_URL" \
     --provider gitlab --account-key acme --display-name Acme

   fishhawkd installation register \
     --db "$FISHHAWKD_DATABASE_URL" \
     --provider gitlab --account-key acme --installation-ref gitlab:4242 \
     --project-path acme/widgets
   ```

   `--project-path` is **REQUIRED** for a `gitlab` registration (E45.26 / #2877) and is the project's full `path_with_namespace`. Its first segment must equal `--account-key`, and nested groups are supported — `--project-path acme/platform/widgets` under `--account-key acme` is valid, because the path is split on the FIRST separator only. Every component must be non-empty after trimming whitespace, so `acme//widgets`, `acme/platform//widgets`, `acme/widgets/`, `acme/ /widgets` and `acme/platform/ ` are refused — GitLab never canonicalises a `path_with_namespace` carrying an empty or whitespace-only component, so such a binding could only ever refuse every trigger. It does not apply to `--provider github`, whose payload identity arrives HMAC-signed and resolves through the installation id.

   `installation register` FAILS CLOSED if no `acme` account exists yet, naming the `account create` line to run first — it never conjures the account, because the account is the operator's authorization decision. Verify what is registered with `fishhawkd installation list`, which renders each `installation_ref` alongside its owning `account_key` and its `PROJECT_PATH` (a gitlab row recording none renders `(unbound)`).

   Re-running the `account create` command is safe (idempotent on `provider,account_key`) but re-run it WITH `--display-name` or the name is cleared — see the account README's omitted-field convention.

   `gitlab:4242` is `gitlab:<numeric project id>` — the same string the run row's `installation_ref` carries. Without a matching row (or without a database at all) an admitted GitLab trigger is refused before the spec is read and before any pipeline is created, and a `run_rejected_misconfigured` audit row records the reason (`gitlab_project_not_registered`, `gitlab_project_registry_unwired`, `gitlab_project_authorization_lookup_failed`, or `gitlab_project_path_unbound`). A registered project id paired with any project path other than the registered one is refused too — both halves of the payload identity are bound.

   The SAME gate runs on the CI-failure retry path, before any candidate lookup, retry child, or pipeline trigger. The audit payload's `path` field names which gate refused (`create_run` or `ci_failure_retry`), since both share the `run_rejected_misconfigured` category.

   **The path binding is EXACT.** Both selectors in the payload are bound exactly: the project *id* must be a registered `installation_ref`, and the project *path* must equal the installation's recorded `project_path` byte for byte. A registered `gitlab:4242` under account `acme` bound to `acme/widgets` is admitted ONLY with `acme/widgets` — `acme/other-project` is refused, as is `acme/platform/widgets`. The path's namespace segment must additionally still equal the owning `account_key`; that tenancy check is retained alongside the exact compare, so a row mis-registered outside its account's namespace by hand-written SQL is refused even though its recorded path matches the payload.

   Comparison is **case-sensitive**. GitLab canonicalises project path case, so `acme/Widgets` and `acme/widgets` name different projects and a case difference is a refusal, not a match. Register the path exactly as GitLab reports `path_with_namespace`.

   Before #2877 the path was bound only at the NAMESPACE level, which admitted a registered id paired with any sibling project in the same namespace and left the workflow-spec read (the only payload-path-selected forge call) steerable within the tenant. That is now closed.

   **Upgrading a deployment with existing registrations.** `installations.project_path` is added NULLABLE by migration `0078` with **no backfill** — there is no correct value to invent, because the project path is an operator authorization decision and deriving it from the payload would derive the authorization from the thing being authorized. Every row registered before the upgrade is therefore **unbound**, and an unbound row **REFUSES** rather than falling back to the old namespace-only admit: that fallback would preserve exactly the steering this change closes. The refusal audits as `run_rejected_misconfigured` with reason `gitlab_project_path_unbound`, on both the run-creation and the CI-failure-retry gate.

   The remedy is re-registration, per installation:

   ```sh
   # 1. Enumerate what needs repairing — unbound gitlab rows render "(unbound)".
   fishhawkd installation list --db "$FISHHAWKD_DATABASE_URL" --provider gitlab

   # 2. Re-register each. The upsert is idempotent on (provider, installation_ref),
   #    so this REPAIRS the row in place rather than duplicating it.
   fishhawkd installation register \
     --db "$FISHHAWKD_DATABASE_URL" \
     --provider gitlab --account-key acme --installation-ref gitlab:4242 \
     --project-path acme/widgets
   ```

   The same procedure is the recovery path if migration `0078` is ever rolled back and later re-applied: the down migration DROPs the column and permanently discards every recorded binding (no other table holds a copy), so re-applying leaves every gitlab installation unbound. A code-only revert that LEAVES the migration applied is harmless by comparison — the pre-#2877 reads name explicit column lists, so the extra column is simply never selected and the recorded values sit dormant.
2. **Register the GitLab forge.** Set the GitLab base URL and token so `forge.Get("gitlab")` resolves (see `resolveGitLabForge` in `backend/cmd/fishhawkd/serve.go`; the config gate is both-or-neither). The dispatcher's spec reader is `registeredFileFetcher("gitlab")` — with GitLab unconfigured, an admitted GitLab trigger logs `no GitLab file reader configured` and creates no run.
3. **Configure the GitLab webhook secret** (`X-Gitlab-Token`) so deliveries authenticate, and enable at minimum the **Issue**, **Comment**, and **Pipeline** hooks. The **Job** hook may be enabled; Fishhawk skips it deliberately so one failing job drives at most one retry.
4. **Commit `.fishhawk/workflows.yaml`** to the project's default branch. Fishhawk reads it through the GitLab Repository Files API at the deployment's default ref.
5. **Label an issue `fishhawk`** (or comment `/fishhawk run`) to trigger.

### `installation_ref` format

A GitLab-created run persists `runs.installation_ref = "gitlab:<project_id>"` — the numeric project id, matching the credential-scope ref the GitLab forge's `projectIDFromScope` parses. A GitHub run persists the BARE base-10 installation id (no `github:` prefix), which is `forge.FromGitHubInstallationID`'s canonical form. Both are read ref-first by `orchestrator.runCredentialScope`, falling back to `installation_id` only when the ref is absent or empty.

### CI-failure retry on GitLab

A failed **Pipeline Hook** triggers the auto-retry when its `ref` matches a run's `fishhawk/run-<short>` branch AND its `sha` matches that run's recorded head SHA. Three operator-visible consequences:

- **The first pipeline of a run never auto-retries.** It runs on the default branch, before the run branch exists. This is a deliberate deferral (issue #2043's own second option) rather than a gap — re-run it manually, or let the next stage's pipeline carry the retry. The audit records `ci_retry_skipped` with reason `first_stage_pipeline_on_non_run_branch` so a missing retry is diagnosable rather than silent.
- **A merge-request pipeline is a DISTINCT deferral, not the first-stage case.** An MR pipeline runs on the merge request's target branch, so it is never on a run branch either — but it is not a run's first pipeline, and saying so would send you down the wrong path. It draws its own reason, `merge_request_pipeline_ref_not_a_run_branch`, so a missing retry on an MR pipeline is diagnosable as what it actually is. Fishhawk identifies these by the Pipeline Hook's `object_attributes.source == "merge_request_event"` (the primary, documented signal) or by a `refs/merge-requests/<iid>/head` / `.../merge` ref shape. Same remedy as above: re-run the pipeline manually, or let the next stage's pipeline on the run branch carry the retry.
- **Every other non-retry is also named** in a `ci_retry_skipped` audit row: `no_candidate_run_owns_pipeline_ref`, `pipeline_sha_does_not_match_run_head_sha`, `run_head_sha_lookup_failed`, `run_lineage_cancelled`, `retry_policy_unresolvable_from_cached_spec`, `gitlab_pipeline_trigger_unconfigured`, `run_has_no_credential_scope`. Query them with `GET /v0/runs/{run_id}/audit?category=ci_retry_skipped`.
- **Two of those reasons are yours to fix**, and each names a different remedy. `gitlab_pipeline_trigger_unconfigured` means the deployment has no GitLab pipeline trigger wired at all — configure the GitLab forge credentials. `run_has_no_credential_scope` means THAT run row carries no `installation_ref`, so no credential scope can be resolved for it — backfill the row's `installation_ref` (the shape a run minted before migration `0076` and missed by its backfill carries). Until then Fishhawk refuses the retry outright rather than marking a stage `dispatched` with no pipeline behind it.

## What is GitHub-only today

The workflow-spec issue trigger and the issue-content prompt path are forge-neutral; a handful of surrounding entry points and side effects are still GitHub-only. Each item names its code site and its open tracking issue so the follow-up doc sweep (#3467) can retire the line when the fix lands.

- **`inputs[].source: github_issue` is forge-neutral — not a GitHub-only literal.** It is the workflow spec's issue-anchored enum member on every forge; a GitLab issue trigger mints a run with `trigger_source: github_issue`. Full reasoning: [`docs/spec/workflow-v2.md`](../spec/workflow-v2.md) § "Inputs and `needs:`". There is no `gitlab_issue` member and none is needed.

- **Issue CONTENT reaches the prompt forge-neutrally.** The run's `run.IssueContext` (`backend/internal/run/run.go` — `Title`/`Body`/`URL`/`Number`/`Comments`/`Labels`) is forge-agnostic, and `fillIssueContext` (`backend/internal/server/prompt.go`) is forge-neutral since E45.42 / #3347: branch 1 uses the cached `issue_context` verbatim (including its `URL`); a gitlab-family run (installation_ref `gitlab:<id>`, or `runner_kind gitlab_ci`) fetches through the forge ladder; and the `IssueURL` ladder never fabricates a `github.com` URL for a non-GitHub run (the github.com format string is applied for a github-family run ONLY). This path is done — no tracking issue.

- **The DEFAULT fetch entry point is NOT forge-neutral (#3463).** `fishhawk_start_run`'s `issue` param shells to `gh issue view` (`backend/internal/mcpserver/issue_fetch.go`), which resolves against github.com regardless of the project's forge. Separately, a local-runner run started with no `installation_ref` resolves as the **github family** (`runForge` in `backend/internal/server/issue_approval.go`: a nil/empty ref → `github`), and with `InstallationID` nil it has no credential, so `fillIssueContext` leaves the prompt without issue context and records an `issue_context_unresolved` audit row with reason `no_credential`. Closing the MCP/CLI issue-fetch entry-point gap for non-GitHub forges is tracked in **#3463**.

- **SUPPORTED PATH TODAY: pass `issue_context` inline.** Supply `issue_context` (title, body, url, number, comments) together with `trigger_source: github_issue` to `fishhawk_start_run`. This satisfies the issue-anchored pairing check at run creation (`backend/internal/server/runs.go` — `issue_context` is only valid when `Run.IsIssueAnchored` holds for the `trigger_source`), persists on the run row, and is served by branch 1 of `fillIssueContext`. Put the GitLab **web** URL in `issue_context.url` so the rendered issue link is correct. Minimal argument shape:

  ```json
  {
    "trigger_source": "github_issue",
    "issue_context": {
      "title": "Widget import fails on empty CSV",
      "body": "Steps to reproduce ...",
      "url": "https://gitlab.example.com/acme/widgets/-/issues/42",
      "number": 42,
      "comments": ["First triage note ...", "Repro confirmed ..."]
    }
  }
  ```

- **RESIDUAL — issue-comment persistence is skipped on GitLab (#3346).** A GitLab-created run carries `InstallationID` nil (`backend/internal/webhook/gitlab_dispatch.go` step 4 sets the credential reference but leaves `InstallationID` nil, because a GitLab project has no GitHub installation id), and every `backend/internal/issuecomment` notifier path gates on `InstallationID == nil`. So `persistence.target: originating_issue` (the plan `rendered_comment`) and the status/approval comments are silently skipped on GitLab. Tracked in **E45.41 / #3346**.

- **RESIDUAL — `fishhawk_doctor` has no not-applicable path for GitLab (#3348).** Its GitHub-only checks have no GitLab branch, so a GitLab deployment sees them as unconditional rather than skipped-as-not-applicable. Tracked in **E45.43 / #3348**.

- **RESIDUAL — merge / auto-merger is GitHub-only (#3464).** The run-completion merge path is not yet wired for the GitLab forge. Tracked in **#3464**.
