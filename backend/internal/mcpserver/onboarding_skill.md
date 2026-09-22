---
name: fishhawk-onboarding
description: Help me onboard a repo to Fishhawk — first-run readiness, a starter workflow spec and a pre-commit spec check via fishhawk_doctor, fishhawk_init and fishhawk_validate.
---

# Fishhawk onboarding

Use this skill when a connecting repository has no `.fishhawk/workflows.yaml`
yet, or you are unsure whether it is ready for its first Fishhawk run. It
walks `fishhawk_doctor` (readiness), `fishhawk_init` (starter spec) and
`fishhawk_validate` (the pre-commit spec check) to a committed spec and a
first run.

## Step 1 — `fishhawk_doctor`

Call `fishhawk_doctor`, passing `repo` (owner/name on GitHub, or a
namespace/project path on GitLab) and `forge` when it cannot be resolved from
the environment. Read each rung of the returned report:

- **`app`** not installed — surface the reason to the operator; installing the
  App is a human action, not something this skill does. On GitLab `installed`
  means BOTH that a gitlab installation is registered for exactly this project
  path AND that the project resolves with the deployment credential, so a
  not-installed app there means one of two things: `resolvable` is `false`
  (the deployment credential cannot see the project — a credential fix), or
  the project is resolvable but unregistered — surface
  `gitlab_registration.remediation`, the `fishhawkd installation register`
  command, to the operator; registering is a human action on the fishhawkd
  host.
- **`spec`** unavailable or invalid — proceed to Step 2.
- **`reviewers[]`** carrying a `missing_hint` — the named environment variable
  is a deployment-side action; surface it, do not attempt to set it yourself.
- **`scopes`** with a non-empty `missing[]` — the caller token needs to be
  re-issued with the missing scopes.
- **`merge_gate`** — read FAIL-CLOSED. `unknown` is not evidence the merge
  check is unrequired; it means the question could not be settled. (Omitted
  entirely on a GitLab-family report — that is by design, not a stale
  backend; GitLab carries the separate `gitlab_merge_gate` rung instead.)
- **`gitlab_registration`** (GitLab only) — the registration `POST /v0/runs`
  actually checks. `not_registered` means a run will be refused
  `422 gitlab_project_not_registered`; `unknown` means the registry could not
  answer (`reason` names `registry_unwired` or `registry_lookup_failed`), NOT
  that the project is unregistered. `ref_matches: false` means the registered
  `installation_ref` names a different project than the path resolves to —
  surface `detail` (both refs) and `remediation` to the operator before any
  run.
- **`gitlab_merge_gate`** (GitLab only) — read FAIL-CLOSED the same way.
  `unknown` means the protection could not be read and `reason` names why
  (forge unconfigured, project not visible, a 403 because the deployment
  credential lacks the Maintainer role, an unresolved default branch, a
  transport error); it is not evidence the branch is unprotected.
  `not_pipeline_gated` is a positive finding: `detail` names what is off and
  `remediation` names the GitLab settings to enable (protect the default
  branch; "Pipelines must succeed"). Both are operator actions on the GitLab
  project, not something this skill does. `pipeline_gated` does NOT mean any
  named check is individually required — GitLab has no per-context required
  check, and approval rules are not read — so the operator confirms those by
  hand; `note` says so on every report.

## Step 2 — `fishhawk_init`

Once a spec is missing or invalid, call `fishhawk_init` with the chosen
autonomy preset:

- **low** — human-led: nothing delegated, every judgment point pages a human.
- **medium** — the recommended default: the operator agent may approve /
  route fixup / retry under named conditions; waive and merge stay human.
- **high** — adds waive (solo low-severity concern) and merge (gates
  resolved, CI green) on top of medium.

Also choose the `shape` — this is a deliberate choice, not something to infer
from the working directory:

- **app** (the default) — a repository with a test entrypoint. The implement
  stage runs an execution-grounded verifier and requires tests to be added.
- **config-only** — a config- or docs-only repository with no test entrypoint.
  Pick this when there is no test command to run: the implement stage omits the
  verifier, drops `tests_added_or_updated`, keeps `ci_green` and raises
  `max_files_changed` so a docs reorganisation fits. The output echoes the
  resolved `shape`.

`fishhawk_init` returns `workflow_yaml` and `target_path` — it writes nothing
itself. Write `workflow_yaml` to `target_path` (`.fishhawk/workflows.yaml`) in
the target repository's working tree.

## Step 3 — Validate before committing

Call `fishhawk_validate` with `working_dir` set to the checkout (or pass the
bytes inline as `workflow_spec`). It runs the same validator
`fishhawk_start_run` and run creation use, in-process, against the file you
just wrote. Read the result:

- `valid: false` — read `diagnostics[]` (`kind`, `path`, `workflow`,
  `stage_index`, `stage`, `message`), correct the file, and call it again
  until `valid: true`. The validator stops at the first failure, so expect one
  diagnostic per call.
- `charter_required_by[]` — each named workflow produces a `grooming_report`
  and REQUIRES a repository charter at run creation; this verb cannot check
  that rule, so note it for the operator.
- `not_checked[]` — what `valid: true` does NOT cover (the charter rule,
  reviewer model ids, deployment wiring) and where each is checked instead.

`fishhawk_doctor`'s `spec` rung reads the DEFAULT BRANCH and stays
`unavailable` until the spec is merged — do not loop on it to confirm an
uncommitted file. Without MCP, `fishhawk validate <path>` is the CLI
equivalent.

## Step 4 — Commit and open the PR

The agent takes no git actions. The operator commits the written
`.fishhawk/workflows.yaml` and opens the pull request under their own
identity — this mirrors the operator-role rule the rest of the loop follows:
the agent proposes, the operator acts.

## Step 5 — First run

Once the spec is merged, run `fishhawk_doctor` once more: its `spec` rung now
reads the merged file and should report `valid: true`, and `reviewers[]`
carries `model_status` for each declared reviewer — model ids are checked
only there, never by `fishhawk_validate`. Then start the first run with
`fishhawk_start_run` (`runner_kind:local` for a local dogfood loop). For the
loop itself — plan, approve, dispatch, review, acceptance, merge — read the
`fishhawk://runbook` resource.

## Install as a project skill

To make this walk available as a standing project skill in the target repo,
copy this document to `.claude/skills/fishhawk-onboarding/SKILL.md`.
