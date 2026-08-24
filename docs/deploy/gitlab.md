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

1. **Register the project as an installation.** GitLab run creation is authorization-gated and FAILS CLOSED: fishhawkd acts only on projects an operator has registered, because a GitLab delivery is authenticated by a shared `X-Gitlab-Token` with no signature over the body — the project a payload names proves nothing on its own. Insert an `installations` row for the project under an account whose `account_key` is the project path's namespace segment:

   ```sql
   -- account_key is the namespace segment of path_with_namespace ("acme" for acme/widgets)
   INSERT INTO accounts (id, provider, account_key, display_name, granularity)
        VALUES (gen_random_uuid(), 'gitlab', 'acme', 'Acme', 'org')
   ON CONFLICT (provider, account_key) DO NOTHING;

   INSERT INTO installations (id, account_id, provider, installation_ref)
        SELECT gen_random_uuid(), id, 'gitlab', 'gitlab:4242' FROM accounts
         WHERE provider = 'gitlab' AND account_key = 'acme';
   ```

   `gitlab:4242` is `gitlab:<numeric project id>` — the same string the run row's `installation_ref` carries. Without a matching row (or without a database at all) an admitted GitLab trigger is refused before the spec is read and before any pipeline is created, and a `run_rejected_misconfigured` audit row records the reason (`gitlab_project_not_registered`, `gitlab_project_registry_unwired`, or `gitlab_project_authorization_lookup_failed`). A registered project id paired with a project path OUTSIDE that account's namespace is refused too — both halves of the payload identity are bound.
2. **Register the GitLab forge.** Set the GitLab base URL and token so `forge.Get("gitlab")` resolves (see `resolveGitLabForge` in `backend/cmd/fishhawkd/serve.go`; the config gate is both-or-neither). The dispatcher's spec reader is `registeredFileFetcher("gitlab")` — with GitLab unconfigured, an admitted GitLab trigger logs `no GitLab file reader configured` and creates no run.
3. **Configure the GitLab webhook secret** (`X-Gitlab-Token`) so deliveries authenticate, and enable at minimum the **Issue**, **Comment**, and **Pipeline** hooks. The **Job** hook may be enabled; Fishhawk skips it deliberately so one failing job drives at most one retry.
4. **Commit `.fishhawk/workflows.yaml`** to the project's default branch. Fishhawk reads it through the GitLab Repository Files API at the deployment's default ref.
5. **Label an issue `fishhawk`** (or comment `/fishhawk run`) to trigger.

### `installation_ref` format

A GitLab-created run persists `runs.installation_ref = "gitlab:<project_id>"` — the numeric project id, matching the credential-scope ref the GitLab forge's `projectIDFromScope` parses. A GitHub run persists the BARE base-10 installation id (no `github:` prefix), which is `forge.FromGitHubInstallationID`'s canonical form. Both are read ref-first by `orchestrator.runCredentialScope`, falling back to `installation_id` only when the ref is absent or empty.

### CI-failure retry on GitLab

A failed **Pipeline Hook** triggers the auto-retry when its `ref` matches a run's `fishhawk/run-<short>` branch AND its `sha` matches that run's recorded head SHA. Two operator-visible consequences:

- **The first pipeline of a run never auto-retries.** It runs on the default branch, before the run branch exists. This is a deliberate deferral (issue #2043's own second option) rather than a gap — re-run it manually, or let the next stage's pipeline carry the retry. The audit records `ci_retry_skipped` with reason `first_stage_pipeline_on_non_run_branch` so a missing retry is diagnosable rather than silent.
- **Every other non-retry is also named** in a `ci_retry_skipped` audit row: `no_candidate_run_owns_pipeline_ref`, `pipeline_sha_does_not_match_run_head_sha`, `run_lineage_cancelled`, `retry_policy_unresolvable_from_cached_spec`. Query them with `GET /v0/runs/{run_id}/audit?category=ci_retry_skipped`.
