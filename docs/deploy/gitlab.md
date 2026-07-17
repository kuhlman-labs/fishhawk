# GitLab CI/CD quickstart

Run a Fishhawk change through GitLab CI/CD. This is the GitLab analog of the
GitHub App path (ADR-058 / E45.8): instead of a GitHub Actions
`workflow_dispatch`, the Fishhawk backend triggers a GitLab **pipeline**
against the run branch and that pipeline hands the run/stage identifiers to
the same backend-agnostic runner. The runner self-detects `gitlab_ci` from
the GitLab-set `GITLAB_CI` / `CI_PIPELINE_ID` environment, so there is no
GitLab-specific runner build.

The customer-side pipeline definition is
`backend/internal/onboarding/templates/.gitlab-ci.yml`, served by
`onboarding.GitLabScaffoldFiles`. Add it to your project as `.gitlab-ci.yml`
at the repo root.

> **Live GitLab bring-up is operator-led (#2032).** The CI-observable
> proxies (pipeline-create ref/variable assertions, backend routing tests,
> failed-pipeline retry unit tests, and the template served-and-parses test)
> gate this change; a real GitLab pipeline/webhook round-trip is exercised in
> the operator-led live walk, not the acceptance sandbox.

## Prerequisites

- A GitLab project (SaaS `gitlab.com` or a self-managed instance) with
  `.fishhawk/workflows.yaml` committed on the default branch.
- The Fishhawk backend reachable from your GitLab runners.
- A GitLab access token with the `api` scope — a **group or project access
  token** in v0. The runner uses it to push the run branch and open the merge
  request.

## Configure CI/CD variables

In **Settings → CI/CD → Variables**, add these as **masked** (and
**protected**, if your run branches are protected) variables:

| Variable | Purpose |
|---|---|
| `FISHHAWK_BACKEND_URL` | Fishhawk backend base URL. |
| `FISHHAWK_GITLAB_TOKEN` | GitLab access token with `api` scope (run-branch push + MR create). |
| `ANTHROPIC_API_KEY` | Consumed when the executor is `claude-code`. |
| `OPENAI_API_KEY` | Consumed when the executor is `codex`. |

`FISHHAWK_GITLAB_TOKEN` is on the runner's gate and acceptance **denylists**,
so agent-authored gate code and the acceptance agent never see it.

## Add the pipeline

Copy `.gitlab-ci.yml` into the repo root. When Fishhawk dispatches a stage it
triggers a pipeline against the run branch (`fishhawk/run-<short>`, or a
decomposed child's `fishhawk/run-<short>/slice-<n>`) and supplies these
pipeline-trigger variables — the GitLab analog of the GitHub
`workflow_dispatch` inputs:

| Variable | Meaning |
|---|---|
| `run_id` | Workflow run UUID. |
| `stage_id` | Stage UUID for this dispatch (drives prompt fetch + trace upload). |
| `workflow_id` | Workflow ID from `.fishhawk/workflows.yaml` (e.g. `feature_change`). |
| `stage` | Executor ref / agent provider (`claude-code`\|`codex`); default `claude-code`. |
| `parent_run_id` | Decomposition-parent run UUID — set only for fan-out children. |

The pipeline job:

1. Runs only for a Fishhawk trigger — a plain branch push (including the
   runner's own run-branch push) carries no `run_id` and no-ops via a `rules`
   guard, so it never spawns a stray run.
2. Serializes decomposition siblings via a `resource_group` keyed on
   `parent_run_id` (falling back to the unique pipeline id for a top-level
   run), mirroring the GitHub template's `concurrency` group.
3. Invokes `fishhawk-runner --forge gitlab --gitlab-base-url $CI_SERVER_URL`
   with the trigger variables, `--fetch-prompt`, `--upload-trace`, and a
   `--check-base-ref origin/$CI_DEFAULT_BRANCH` diff base.

The `image:` pins the published runner image. Bump the pinned tag when you
adopt a newer runner release, exactly as you would bump the pinned
`kuhlman-labs/fishhawk/runner` action tag in the GitHub template.

## Verify

Dispatch a stage from Fishhawk and confirm a pipeline appears on the run
branch in **Build → Pipelines**. The runner posts progress back to the
backend; the `fishhawk`-gate commit status on the run's head commit is
published by the backend (`auditcheckpublisher`), not by the pipeline itself.
