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
