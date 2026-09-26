# GitLab CI onboarding quickstart

How a GitLab project runs Fishhawk stages through GitLab CI/CD, using the
customer-side `.gitlab-ci.yml` template at
`backend/internal/onboarding/templates/.gitlab-ci.yml`. This is the GitLab
analog of the App-onboarded GitHub Actions workflow
(`.github/workflows/fishhawk.yml`): instead of a `workflow_dispatch` against a
composite action, the Fishhawk backend triggers a **pipeline** via the GitLab
pipelines API and the pipeline invokes the published, backend-agnostic
`fishhawk-runner` against the GitLab forge (`--forge=gitlab`).

> **Status — live (ADR-058 / #1861).** GitLab run creation is live via the
> GitLab webhook receiver (E45.22 / #2043, closed) —
> `backend/internal/webhook/gitlab_dispatch.go` creates `gitlab_ci` runs, and
> this template is what those runs execute. Setup is in "## Go-live:
> GitLab-triggered runs" below. The MCP/CLI operator-verb path
> (`fishhawk_start_run forge=gitlab` / `fishhawk run start --forge gitlab`) is
> also live and can create and drive a GitLab run with a local runner
> (E45.46 / #3463, closed).

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
  `--forge=gitlab` push-path credential. Since E45.82 / [#3617](https://github.com/kuhlman-labs/fishhawk/issues/3617)
  the runner reads it at prompt-fetch time and REFUSES a gitlab-forge implement
  stage outright when it is absent or whitespace-only
  (`runner_failed`, reason `gitlab_push_credential_missing`), so a job missing
  this variable fails in seconds instead of after a full agent pass.
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

**A GitLab run needs BOTH, on two different hosts.** Stated once, with the
surface that reports each:

| Variable | Whose process | What it does | Where it must be set | What reports it |
|---|---|---|---|---|
| `FISHHAWKD_GITLAB_TOKEN` (trailing `D`) | the **backend** (`fishhawkd`) | REST reads: forge/work-item provider, project resolution, login-gate group lister | the fishhawkd deployment (Helm `secrets.values.gitlabToken`) | `fishhawk_doctor`'s `app` rung (`resolvable` / `installed`) |
| `FISHHAWK_GITLAB_TOKEN` | the **runner** | pushes the run branch, opens the merge request (`api` scope) | the host that SPAWNS the runner — the CI/CD variable on the `gitlab_ci` channel, or the environment that launches `fishhawk-mcp` / `fishhawk runner start` locally | `fishhawk_doctor`'s `runner_credentials` rung (LOCAL channel only — it cannot see a CI/CD variable), and the runner's own startup preflight on BOTH channels |

Configuring only the first is the common failure: everything registers and
plans cleanly, and the run then fails at the implement stage's push.

See [`deploy/helm/fishhawk/README.md`](https://github.com/kuhlman-labs/fishhawk/blob/main/deploy/helm/fishhawk/README.md)
(the "GitLab" section) for the full chart contract: the graduated enablement
(base-URL-alone is a supported login-gate posture), the all-three-or-none OAuth
trio guard, and which secrets are required when.

**A GitLab-only backend needs no GitHub App (E45.45 / #3461).** A `fishhawkd`
configured with only the `gitlab.*` / `secrets.values.gitlab*` values above —
no `--github-app-id` / App private key file — serves stage prompts and
executes runs without a GitHub App: the prompt handlers' `prompt_unconfigured`
gate requires only the run (and, for the signed route, signing) repositories,
not a GitHub client. If a github-family run somehow lands on such a server, it
records an `issue_context_unresolved` audit row with reason `forge_unresolved`
rather than 503ing the prompt.

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

## Deploy stages on GitLab

A deploy stage on a GitLab-created run must use the **`webhook`** delegate. The `github_actions` delegate dispatches `workflow_dispatch` through a GitHub App installation, and a GitLab run (`runner_kind: gitlab_ci`, or `installation_ref: gitlab:<project_id>`) has none — so `backend/internal/server/deploy_trigger.go` fails the stage at trigger time (category C) instead of parking it, with a `deployment_dispatch_failed` audit row whose `reason` names the fix and whose payload carries `runner_kind`, `installation_ref` and `remedy: "executor.delegate.target: webhook"` (#3465). The check runs BEFORE the deployment's GitHub-client guard, so a GitLab-only backend with no GitHub client configured fails loud rather than leaving the stage at `dispatched` forever.

**Keep the deploy credential OUT of `url`.** The spec is repository content with no environment or variable substitution (`$NAME` in `url` is sent literally, not expanded), and `triggerDeployWebhook` persists `delegate.url` VERBATIM into the `deployment_dispatched` audit payload and every `deployment_dispatch_failed` payload — so a GitLab pipeline trigger token pasted into the URL (`/projects/:id/trigger/pipeline?token=…`) would land in source history, in the persisted workflow spec, and in every audit row and proxy/access log that records the URL. Use the webhook delegate's **secret channel** instead (E45.57 / #3497, workflow-v2 only): declare the NAME of an environment variable and where its value goes, and fishhawkd resolves the value at dispatch. Direct trigger-token flow, targeting GitLab's [pipeline trigger API](https://docs.gitlab.com/api/pipeline_triggers/) with no relay:

```yaml
version: "2"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: webhook
            url: https://gitlab.example.com/api/v4/projects/<project_id>/trigger/pipeline?ref=main
            secret_env: DEPLOY_TRIGGER_TOKEN   # the NAME; the value lives only in fishhawkd's environment
            secret_field: token                # GitLab reads the trigger token from the JSON body key `token`
        gates:
          - type: approval
            approvers:
              any_of: [release_managers]
```

How the variable reaches fishhawkd — it is read from the **process environment of `fishhawkd` itself**, not the runner host and not the MCP host:

- Shell / systemd: `export DEPLOY_TRIGGER_TOKEN=glptt-…` in the environment that spawns `fishhawkd` (or an `Environment=` line in the unit).
- Helm chart (`deploy/helm/fishhawk`): add the key to the Secret the workloads consume via `envFrom` — in the default `secrets.mode: existing` that is your `existingSecret` (`fishhawk-secrets`); `externalSecrets` mode adds a `data[]` entry; dev-only `chartManaged` reads `secrets.values`. The `envFrom` wiring puts every key of that Secret into fishhawkd's environment, so no template change is needed.

The name is refused at run admission if it starts with `FISHHAWKD_` or `FISHHAWK_` (Fishhawk's own configuration namespace — a committed spec could otherwise exfiltrate fishhawkd's database URL or forge token), and `secret_field` is refused if it collides with a reserved trigger-body key (`fishhawk_run_id`, `fishhawk_stage_id`, `fishhawk_rollback`, `repo`, `workflow_id`, `variables`). Both refusals are server-side only (`POST /v0/runs`); `fishhawk validate` accepts such a spec. An unset or empty variable at dispatch fails the deploy stage category C with a `deployment_dispatch_failed` row naming the variable — no request leaves the process. For a target that authenticates by header instead, drop `secret_field`: the value rides `PRIVATE-TOKEN` by default, or the header you name in `secret_header`.

The webhook POST body is `{fishhawk_run_id, fishhawk_stage_id, repo, workflow_id, variables: {FISHHAWK_RUN_ID, FISHHAWK_STAGE_ID, FISHHAWK_REPO, FISHHAWK_WORKFLOW_ID}}` (plus `fishhawk_rollback: true` / `variables.FISHHAWK_ROLLBACK: "true"` on a rollback re-dispatch), with `token` added when `secret_field: token` is set. GitLab turns the `variables` object into CI variables of the triggered pipeline, so a callback job can report the outcome directly:

```yaml
# .gitlab-ci.yml of the deploy project
report_to_fishhawk:
  stage: .post
  script:
    - >
      curl -fsS -X POST "$FISHHAWKD_URL/v0/runs/$FISHHAWK_RUN_ID/deployment"
      -H "Authorization: Bearer $FISHHAWK_API_TOKEN" -H "Content-Type: application/json"
      -d "{\"outcome\":\"success\",\"stage_id\":\"$FISHHAWK_STAGE_ID\"}"
```

A webhook target has no GitHub Actions run to poll, so the deploy stage reaches its terminal state only when your pipeline calls back into `POST /v0/runs/{run_id}/deployment` with the outcome (`backend/internal/deployreconciler/README.md`). fishhawkd **never follows a redirect** from the trigger endpoint: a 3xx fails the stage carrying only the status code (never the `Location` header or the body), so point `url` at the final endpoint. Transport failures are recorded by class (`timeout`, `dns`, `connection_refused`, `tls`, `redirect_parse`, `other`) plus the committed `url`; the resolved token is redacted from every audit row, log line and error.

**Extra-key tolerance is pending the live walk (#2032).** GitLab's trigger endpoint documents `token`, `ref`, `variables` and `inputs`; whether it tolerates the flat correlation keys (`fishhawk_run_id`, `repo`, …) alongside them in a JSON body is undocumented (Grape ignores undeclared params by default). If the live walk disproves it, the fallback is the **relay**: point `url` at a deploy endpoint you operate (`https://deploy-relay.example.internal/fishhawk/deploy`), authenticate Fishhawk's call with `secret_env` + `secret_header` (or by network placement / mTLS / a source-IP allow-list), and have the relay hold the GitLab trigger token in its own environment and call `POST /projects/:id/trigger/pipeline` itself, forwarding `variables` as-is and dropping the flat keys. The relay receives the same body shape documented above; a relay validating with `additionalProperties: false` must admit the additive `variables` key.

## `ci_green` on GitLab today

Two surfaces read the run's **required-checks snapshot** (`runs.required_checks_snapshot`). Since E45.55 / #3490 the GitLab webhook run-creation path captures one from the project's `only_allow_merge_if_pipeline_succeeds` setting (`backend/internal/webhook/README.md` § "CI-requirement snapshot"): ON → `contexts: ["gitlab/pipeline"]` (+ `gitlab_allow_skipped_pipeline`), OFF → present-but-empty (nothing required), GitLab forge unconfigured or the Projects API read failing → nil, the run still created. The `stage_checks` writer for `gitlab/pipeline` is `server.ingestGitLabPipeline` (`backend/internal/server/README.md` § "GitLab pipeline ingest"): every Pipeline Hook (`object_kind: pipeline`, any status) is recorded on the review stage of each run whose `pull_request` artifact matches the pipeline's sha (+ MR iid when present) AND whose `repo` + `installation_ref` match the delivering project, and the post-CI policy re-evaluation then re-runs per matched run — so `ci_green` is a real signal on GitLab. What that means, per surface:

- **`constraints: [required_upstream: [ci_green]]` on a deploy stage clears on a GitLab run once the newest pipeline for the evaluated run's head sha is green.** The pre-flight gate names every refusal in `details.branch`: `snapshot_absent` ONLY when the CI-requirement read at run creation was unwired or failed (permanent for that run — retrying will not change the verdict; the message says so and points at #3490 / the fishhawkd log at run creation); `contexts_pending` naming `gitlab/pipeline` while no terminal pipeline has been recorded; `contexts_failed` naming it when the newest pipeline failed, was cancelled, or was `skipped` on a project that does NOT allow merge on a skipped pipeline; `Satisfied` on a present-but-empty snapshot (the project does not require a passing pipeline) or on a green newest pipeline. The remaining branches (`upstream_unresolvable`, `stage_checks_unavailable`, `no_ci_signal_stage`, `stage_check_read_failed`) are forge-neutral. Full branch table: `backend/internal/server/README.md` § "Deploy gate `ci_green` verdict".
- **`constraints: [required_outcomes: [ci_green]]` on an implement stage is asserted on a GitLab run.** The policy engine still defers `ci_green` when no CI signal exists at trace-upload time (#297, `deferred_outcomes: [ci_green]`); the pipeline ingester's per-run re-evaluation then supplies the signal from the `gitlab/pipeline` row exactly as GitHub's `check_run` path does. Only a snapshot-less run (the failed-read case above) keeps the permanent `deferred_unresolvable: [ci_green]` / `applied_constraints.ci_green_unresolvable: true` label (#3465) — read it as "this outcome was not asserted and nothing downstream will assert it". Contract: `backend/internal/policy/README.md` § "Permanent `ci_green` deferral".
- **The deploy gate reads checks from the evaluated run's REVIEW stage** (`server.findCISignalStage`, E68.67 / #3489) — the stage both `ingestCheckRun` (GitHub) and `ingestGitLabPipeline` (GitLab) record rows against.
- **Precedence and the skipped rule.** The same head sha can carry several pipelines (retry, manual re-run); the newest `object_attributes.id` wins regardless of `ts` or delivery order — structurally, through the `stage_checks` readers' `gitlab_pipeline_id DESC NULLS LAST` ordering. A `skipped` pipeline passes only when the run's snapshot carries `gitlab_allow_skipped_pipeline: true` (the project's `allow_merge_on_skipped_pipeline`); otherwise it is recorded with the fail-bucket conclusion `skipped_not_allowed`.
- **Live validation.** The end-to-end flow — Projects API read → snapshot → Pipeline Hook → `gitlab/pipeline` row → deploy verdict — is proven against real Postgres and a real GitLab forge client over an httptest Projects API (`backend/internal/server/gitlab_pipeline_pg_test.go`), but has NOT yet been observed against a live GitLab instance; the operator walk filed under **E45.55 / #3490** (three arms: pipeline-required + green, pipeline-required + failed, pipeline-not-required) is what closes that.

## What `fishhawk_doctor` verifies about the merge gate on GitLab (E45.66 / #3580)

On GitHub, `fishhawk_doctor`'s `merge_gate` rung reads branch protection and rulesets and answers whether the `fishhawk_audit_complete` check Fishhawk publishes is individually REQUIRED to merge. GitLab has no equivalent surface — there is no per-context required status check — so that rung is omitted on GitLab and a SEPARATE `gitlab_merge_gate` rung (`GET /v0/onboarding/readiness`, `fishhawk_doctor`, contract in `backend/internal/server/README.md` § "Check (5) on GitLab") answers the question GitLab CAN answer.

**What it reads.** Two GitLab API calls with the deployment credential, project settings FIRST: `GET /projects/:id` (the real `default_branch`, `only_allow_merge_if_pipeline_succeeds`, `allow_merge_on_skipped_pipeline`, `only_allow_merge_if_all_discussions_are_resolved`) and then `GET /projects/:id/protected_branches` (every rule, paged — the walk is bounded by a fail-closed page cap that refuses a partial rule set rather than truncating, so a truncated list can never be misread as "unprotected"). It matches the default branch against EVERY rule — the exact-name rule and any `*` wildcard rule — and, because GitLab enforces the MOST PERMISSIVE of all matching rules, reports `matched_rules[]` (exact first, then wildcards in API order), the push/merge access levels as the UNION across those rules (ascending, so the most permissive level is first) and `allow_force_push` as the OR across them. On an UNPROTECTED default branch (no rule matched) these three rule-derived signals are OMITTED, not rendered `false`: an unprotected GitLab branch permits force pushes to anyone with push access, so a rendered `allow_force_push:false` would read as the opposite of the truth.

**What `pipeline_gated` means.** The default branch is covered by at least one protected-branch rule AND the project's "Pipelines must succeed" merge check is on. `not_pipeline_gated` is a positive finding — both reads answered and at least one of those is off; `detail` names which and `remediation` names the settings to change (Settings → Repository → Protected branches; Settings → Merge requests → Merge checks → "Pipelines must succeed"). `unknown` means the protection could NOT be read — the forge is unconfigured, the project is not visible, the credential lacks the Maintainer role the `protected_branches` API requires (a 403 renders `unknown` with reason `forbidden`, never `not_pipeline_gated`), the default branch is unresolved, or the read timed out — and `reason` names which; every signal that was never read is absent from the object, never rendered as `false`.

**What remains UNVERIFIED — read this plainly.**

- **No per-context required check exists on GitLab, so nothing verifies that the `fishhawk_audit_complete` commit status individually gates the merge.** `pipeline_gated` rests on the assumption that the commit status Fishhawk posts on the MR head sha participates in the head pipeline's status as an external job, making "Pipelines must succeed" the closest GitLab analogue to a required check; no offline test can prove that, and the rung's constant `note` says so on every report. The rung reports `pipeline_gated`, never "fishhawk check required".
- **Approval rules are not read.** Merge request approval rules (a Premium feature) may add or remove gating independently of anything this rung inspects; they are deliberately out of scope.
- **The operator confirms both by hand**: open an MR on the project, confirm the `fishhawk_audit_complete` status appears on the MR's head pipeline and that the MR's merge button is blocked while it is pending or failed, and review the project's approval rules directly. The live walk is tracked with the other GitLab live validations (#3490).
- **Registration is its own rung (E45.68 / #3582).** `app.installed` on GitLab means "a gitlab installation is registered for exactly this project path AND the project resolves with the deployment credential"; the weaker resolvability fact is `app.resolvable`, and the `gitlab_registration` rung pre-flights the `POST /v0/runs` registry check (`422 gitlab_project_not_registered`) through the same seam. It does NOT check the `account_key` binding the webhook receiver enforces. Live walk: register via `fishhawkd installation register`, GET readiness, assert `installed`/`registered`/`ref_matches` true; re-register with a wrong `--installation-ref`, assert `ref_matches: false` naming both refs.

## Driving a GitLab project from MCP / CLI with a local runner (E45.46 / #3463)

The GitLab CI channel above is one entry point; since #3463 the operator surfaces — `fishhawk_start_run` over MCP and `fishhawk run start` on the CLI — can create a GitLab run too, and the four runner-spawning MCP verbs (`fishhawk_run_stage`, `fishhawk_dispatch_stage`, `fishhawk_drive_run`, `fishhawk_run_children`) plus `fishhawk runner start` can drive it with a **local** runner. The DECISION recorded on #3463: a GitLab project MAY be driven by a local runner (`runner_kind: local`). The `gitlab_ci`-LOCKED refusal — a host dispatch against a run already locked to `runner_kind=gitlab_ci` — is correct and unchanged; it is a channel mismatch, not a forge one.

**Prerequisites.**

1. **Register the project** so the backend can stamp the run's credential reference: `fishhawkd installation register --provider gitlab --account-key <namespace> --installation-ref gitlab:<project_id> --project-path <path_with_namespace> --forge-base-url <instance root>`. `POST /v0/runs` resolves the `installation_ref` from the installation whose `project_path` EXACTLY equals `repo`; an unregistered (or ambiguously registered) path is refused `422 gitlab_project_not_registered` naming this command. Verify with `fishhawk_doctor` (E45.68 / #3582): its `gitlab_registration` rung reports the registration through the same registry check (`registered` / `not_registered` / `unknown`, with a copy-pasteable register command carrying the real project id, and `ref_matches` comparing the registered ref with the id the path resolves to), and `app.installed` is `true` only once the project is BOTH registered and visible to the deployment credential.
2. **Give the run an instance root.** The spawn producers read it back from `GET /v0/runs/{id}` as `forge_base_url` — the installation's `--forge-base-url`, else the deployment default `FISHHAWKD_GITLAB_BASE_URL`. A gitlab run with NEITHER is refused at spawn time (`… carries no forge_base_url; not spawning`) naming both remedies; nothing is dispatched.
3. **Export `FISHHAWK_GITLAB_TOKEN` in the environment that LAUNCHES the runner-spawning process** — not merely "somewhere on the host". The four MCP verbs (`fishhawk_run_stage`, `fishhawk_dispatch_stage`, `fishhawk_drive_run`, `fishhawk_run_children`) spawn the runner with `append(os.Environ(), …)`, so the runner inherits the environment of the **`fishhawk-mcp` process**, which is the environment the MCP registration created — a `claude mcp add fishhawk <cmd>` that omits `-e FISHHAWK_GITLAB_TOKEN=<token>` produces exactly this failure even when the variable is exported in your interactive shell. On the CLI path it is the shell running `fishhawk runner start`. Use `claude mcp add fishhawk <cmd> -e FISHHAWK_GITLAB_TOKEN=<token>` (then `/mcp` to reconnect) or add it to the registration's env block.

   **Two surfaces now report it** (E45.82 / [#3617](https://github.com/kuhlman-labs/fishhawk/issues/3617)). `fishhawk_doctor` carries a `runner_credentials` rung computed LOCALLY by the MCP server about its own environment — `present` / `missing` / `not_applicable` (a github run) / `unknown` (the report named no forge family) — which reports PRESENCE only, never the token's validity or scope, and which describes the MCP server's spawning environment ONLY: it says nothing about a `gitlab_ci` runner, whose credential is a CI/CD variable this process cannot read. And the runner itself now REFUSES at prompt-fetch time rather than at the push: a gitlab-forge implement stage that will push and finds no credential exits with `{"event":"runner_failed","reason":"gitlab_push_credential_missing"}` before the agent is invoked, so a missing token costs zero agent tokens instead of a complete paid pass. That refusal covers BOTH channels, local and `gitlab_ci` — a CI job missing the CI/CD variable is refused the same way.
4. **Pass `workflow_spec` inline** (the MCP server auto-discovers it from `working_dir`; the CLI reads `--spec-file`). There is no GitLab spec-fetch fallback on `POST /v0/runs`: an empty spec on a gitlab run is `422 workflow_spec_required`.

**Creating the run — the forge selector and the github-pin rule.** `fishhawk_start_run` / `fishhawk run start` take an optional `forge` (`github` | `gitlab`); `repo` is the GitLab `path_with_namespace` (nested groups allowed). The ladder differs from REST in ONE respect, the no-github-fetch guarantee:

- `forge: gitlab` — the request carries `forge: gitlab`; the `issue` convenience is NOT fetched (the `gh issue view` fetch resolves against github.com regardless of the project's forge) and a warning names `issue_context` as the supported path. Pass the GitLab issue inline (title, body, the GitLab **web** URL, number, comments), exactly as the "SUPPORTED PATH" example below shows.
- `forge` omitted with `issue` given and no inline `issue_context` — the client fetched the issue from github.com via `gh`, so it PINS `forge: github` on the request. The pin lands on the fetch ATTEMPT (an absent or failing `gh` still pins), so two otherwise-identical invocations can never mint runs of different forges, and the backend can never re-derive gitlab under a github.com-fetched issue (explicit wins on the REST ladder).
- `forge` omitted with NO github.com fetch — no `issue`, or `issue_context` supplied inline — the request carries no `forge` and the backend derives it from the registered installation for the repo owner (default github). An owner registered under BOTH forges is ambiguous and defaults to github: pass `forge` explicitly there.
- `runner_kind` must be `local` (or `gitlab_ci`) on a gitlab run; an omitted or `github_actions` value is `400 validation_failed` — a GitHub Actions workflow cannot push to a GitLab project.

**Driving it.** Every runner-spawn producer resolves the run's forge target through ONE helper (`resolveRunForgeTarget`, `backend/internal/mcpserver/run_stage.go`) that performs a single-run `GET /v0/runs/{id}` — never a list read, because `forge_base_url` is served on the single-run route only — and FAILS CLOSED: an unreadable run row, an unknown forge, or a gitlab run without `forge_base_url` refuses before the host-dispatch marker and the spawn (drive_run stops with `stopped_reason: forge_unresolved`). A gitlab target appends `--forge gitlab --gitlab-base-url <url>` to the runner argv right after `--github-repo`, whose value is the project slug on both forges; an omitted `github_repo` on a gitlab run defaults to the run row's `path_with_namespace` instead of the github.com-only origin auto-detect. Decomposition children are minted server-side (`orchestrator.fanoutIfDecomposed` → `run.ChildParamsFrom`) and inherit the parent's `installation_ref`, spec and `runner_kind`, so `fishhawk_run_children` reads the PARENT's row once and stamps the same two flags on every child argv. `fishhawk runner start` mirrors the contract: it reads the run row before spawning whenever `--forge`, `--gitlab-base-url` or `--github-repo` is omitted and refuses to spawn on a read failure with `--forge` omitted (`cli/README.md` § "Local-runner spawn"). Contract detail: `backend/internal/mcpserver/README.md` § "GitLab runs".

## What is GitHub-only today

The workflow-spec issue trigger, the issue-content prompt path and (since #3463) the MCP / CLI run-creation and local-runner spawn paths are forge-neutral; a handful of surrounding entry points and side effects are still GitHub-only. Each item names its code site and its open tracking issue so the follow-up doc sweep (#3467) can retire the line when the fix lands.

- **`inputs[].source: github_issue` is forge-neutral — not a GitHub-only literal.** It is the workflow spec's issue-anchored enum member on every forge; a GitLab issue trigger mints a run with `trigger_source: github_issue`. Full reasoning: [`docs/spec/workflow-v2.md`](../spec/workflow-v2.md) § "Inputs and `needs:`". There is no `gitlab_issue` member and none is needed.

- **Issue CONTENT reaches the prompt forge-neutrally.** The run's `run.IssueContext` (`backend/internal/run/run.go` — `Title`/`Body`/`URL`/`Number`/`Comments`/`Labels`) is forge-agnostic, and `fillIssueContext` (`backend/internal/server/prompt.go`) is forge-neutral since E45.42 / #3347: branch 1 uses the cached `issue_context` verbatim (including its `URL`); a gitlab-family run (installation_ref `gitlab:<id>`, or `runner_kind gitlab_ci`) fetches through the forge ladder; and the `IssueURL` ladder never fabricates a `github.com` URL for a non-GitHub run (the github.com format string is applied for a github-family run ONLY). This path is done — no tracking issue.

- **RETIRED by E45.46 / #3463 — the MCP / CLI entry points now create and drive GitLab runs.** The `issue` convenience on `fishhawk_start_run` / `fishhawk run start` still shells to `gh issue view` (`backend/internal/mcpserver/issue_fetch.go`), which resolves against github.com — that is why the clients PIN `forge: github` whenever they used it and SKIP it under `forge: gitlab`, rather than the fetch being made forge-neutral. A gitlab run minted through `POST /v0/runs` now carries the registered `gitlab:<project_id>` `installation_ref`, so it derives as the gitlab family and takes `fillIssueContext`'s forge fetch branch instead of landing on `no_credential`. Walkthrough: "Driving a GitLab project from MCP / CLI with a local runner" above. Residual, stated plainly: a `gh`-shaped fetch of a GitLab issue does not exist — the inline `issue_context` below IS the GitLab issue path.

- **RETIRED by #3658 — campaigns now assemble on GitLab.** The gitlab work-item provider (`backend/internal/workmgmt/gitlab/campaign.go`) implements both campaign sources, so `fishhawk_start_campaign` / `POST /v0/campaigns` against a `provider: gitlab` repo assembles instead of refusing `501 epic_children_unsupported` / `issue_set_resolution_unsupported`, and `fishhawk_doctor`'s `work_item_provider` rung reports `campaign_sources: ["epic_ref", "items"]`. **`items` mode** resolves each named issue's `is_blocked_by` links (read through the Free-tier issue-links API) as `depends_on` edges. **`epic_ref` mode** takes an epic ISSUE's `relates_to` links as its children — the reciprocal of the link `fishhawk_file_issue` writes from child to parent — admitting a candidate unless its body carries a `Parent epic:` marker naming a DIFFERENT epic, and excluding a cross-project link unread. A side effect: a gitlab `fishhawk_file_issue` with a `parent_epic` and an `[EX.n]` title format now allocates `n` from those children. Residuals, stated plainly: `is_blocked_by` / `blocks` are GitLab **Premium** link types, so on a Free / Core project every campaign assembles with NO edges (all items in the first wave, no dependency ordering); a hand-added `relates_to` link to an unrelated issue with no `Parent epic:` marker IS swept in as an epic child; GitLab issues carry no `state_reason`, so any closed issue counts as complete (a won't-do close satisfies a dependency); and Premium **group epics** (`group&5`, a `/-/epics/N` URL) remain refused `422 campaign_epic_ref_group_unsupported` — use `items` mode over the child issues. Mapping decisions: `backend/internal/workmgmt/gitlab/README.md`.

- **SUPPORTED PATH for a GitLab issue: pass `issue_context` inline.** Supply `issue_context` (title, body, url, number, comments) together with `trigger_source: github_issue` (and `forge: gitlab`) to `fishhawk_start_run`. This satisfies the issue-anchored pairing check at run creation (`backend/internal/server/runs.go` — `issue_context` is only valid when `Run.IsIssueAnchored` holds for the `trigger_source`), persists on the run row, and is served by branch 1 of `fillIssueContext`. Put the GitLab **web** URL in `issue_context.url` so the rendered issue link is correct. Minimal argument shape:

  ```json
  {
    "forge": "gitlab",
    "runner_kind": "local",
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

- **RETIRED by E45.52 / #3481 — issue-locus comments now land on GitLab.** A GitLab-created run still carries `InstallationID` nil (`backend/internal/webhook/gitlab_dispatch.go` step 4 sets the credential reference `gitlab:<project_id>` but no GitHub installation id); the `backend/internal/issuecomment` notifier now derives the comment FAMILY from that `installation_ref` (`commentForgeFamily`, mirroring `server.runForge`) instead of gating on `InstallationID`, and routes every issue-locus surface — the living anchor / `persistence.target: originating_issue` plan echo, page-class pings, CI-retry, budget-alert and the generic status posts — through `forge.IssueOperations`, resolved by `server.New` via the shared `issueOpsFor` ladder (`Deps.ForgeIssueOps`). The GitLab adapter implements the optional `forge.IssueCommentEditor` (`gitlabclient.UpdateIssueNote`, `PUT /projects/:id/issues/:iid/notes/:note_id`), so the anchor is edited in place exactly as on GitHub; a forge lacking the editor degrades to append-only, and the `status_comment_posted` audit row names which happened (`forge: gitlab`, `comment_mode: edit_in_place | append_only`). The github family keeps its App-client path byte-for-byte. Contract: [`docs/issue-comment-surfaces.md`](../issue-comment-surfaces.md) § "Forge family routing". Named residuals that remain GitHub-only:
  - The MR-locus surfaces — the sticky PR status comment (`pr_status_comment_posted`) and the advisory agent-review PR reviews (`pr_review_posted`) — stay on their `InstallationID` guard and are skipped on GitLab.
  - Slash-approval replies (`NotifySlashApprovalReply`) and the run-rejected / not-applicable posts (`NotifyRunRejected`, `NotifyRunNotApplicable`) call the GitHub client with a caller-supplied scope.
  - `server.New` constructs the notifier only inside its `cfg.GitHub != nil` block, so a GitLab-only fishhawkd with NO GitHub App configured has no notifier at all and posts nothing on either forge.

- **RETIRED — `fishhawk_doctor` now has a not-applicable path for GitLab (E45.43 / #3348, closed).** Its GitHub-only checks skip as not-applicable on a GitLab deployment instead of running unconditionally; since E45.66 / #3580 the GitLab report carries the `gitlab_merge_gate` rung in place of the GitHub-only `merge_gate` — what it does and does not verify is stated under "What `fishhawk_doctor` verifies about the merge gate on GitLab" above.

- **Merge, merge reconciler and implement review are forge-resolved (E45.47 / #3464).** The run-completion merge seam (`POST /v0/runs/{run_id}/merge` and the delegated `may_merge`), the merge-status reconciler poll, and the four implement-review diff sites (consolidated review, post-fix-up re-review, fix-up delta, cumulative evaluation) all resolve by forge family, so a GitLab run reaches `awaiting_merge` and merges (`merge_when_pipeline_succeeds`, squash). The merge reconciler is off by default (`--enable-merge-reconciler`) and now starts on a GitLab-only deployment. Named residuals that remain:
  - A conflicting GitLab MR is not classified — `prMergeConflicting` fails OPEN, so a conflict falls through to the merge queue rather than a `409 merge_conflicting` (the `forge.PullRequest` mergeability fields are zero on the GitLab adapter).
  - `ReverifyBranchLineage` (ADR-035) is GitHub-only and fails open (`clean=true`) on GitLab, so a merged GitLab MR is not lineage-re-checked in the reconciler.
  - The acceptance-complete Rule 5 board/no-op paths and campaign auto-drive still require a GitHub client (`newCampaignGateActor` refuses without one).
  - A nested GitLab group path (`group/sub/project`) is refused at the merge seam only (`resolveObservationTarget` requires `owner/name`); the compare and reconciler paths tolerate it.
