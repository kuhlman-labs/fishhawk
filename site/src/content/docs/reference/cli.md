---
title: CLI
description: The fishhawk command surface, generated from the command inventory bound to the executable.
---

`fishhawk` is the command-line interface to a Fishhawk backend. The command
groups today:

| Command | What |
|---|---|
| `init` | Scaffold `.fishhawk/workflows.yaml` from an autonomy preset. |
| `validate` | Validate a workflow document against its declared major. |
| `migrate-spec` | Translate a v1 document to v2 with an eligibility report. |
| `run` | Open and drive runs. |
| `plan` | Read a run's plan. |
| `audit` | Query the audit log. |
| `export` | Export an audit chain for offline verification. |
| `token` | Mint and store a backend credential. |
| `doctor` | Diagnose a local setup. |

`fishhawk <command> --help` is authoritative for flags. The component README is
[`cli/README.md`](https://github.com/kuhlman-labs/fishhawk/blob/main/cli/README.md).

## Generated command reference

<!-- BEGIN GENERATED cli -->

_Generated from the canonical sources by `scripts/gen-site-reference`; do not edit between the markers. Description-only edits to a source are not diffed — the delta tables below compare shape (type, requiredness, enum members, default)._

> **See also:** [Driving a run](/fishhawk/operating/driving-a-run/) — the operator loop these reference surfaces serve. This page is the field reference, not a restatement of that guide.

## Commands

Every command below is rendered from the `cli/internal/cmdinfo` inventory, which `TestCLIFlagsMatchExecutableSurface` binds to the live `flag.FlagSet` of each command in both directions — so a flag cannot appear here that the binary does not register, nor a registered flag be omitted. `fishhawk <command> --help` prints the same flags with their defaults.

| Command | Arguments | Synopsis |
|---|---|---|
| `fishhawk run start` | — | Trigger a workflow run. |
| `fishhawk run status` | `<run-id>` | Show a run's current state. |
| `fishhawk run list` | — | List runs with optional filters. |
| `fishhawk run cancel` | `<run-id>` | Cancel an in-flight run. |
| `fishhawk run open` | `<run-id>` | Open a run's detail page in the browser. |
| `fishhawk run retry` | `<stage-id>` | Retry a failed stage (takes a stage id, not a run id). |
| `fishhawk run watch` | `<run-id>` | Block until a stage settles. |
| `fishhawk plan approve` | `<run-id>` | Approve the plan stage on a run. |
| `fishhawk plan reject` | `<run-id>` | Reject the plan stage on a run (category-D failure). |
| `fishhawk plan revise` | `<run-id>` | Force a constrained replan pass. |
| `fishhawk token login` | — | Log in via the OAuth device flow; mint + store a user-bound token. |
| `fishhawk token list` | — | List locally stored credentials (per backend URL). |
| `fishhawk deploy status` | `<run-id>` | Show the deploy stage state and the deployment artifact. |
| `fishhawk deploy approve` | `<run-id>` | Approve the deploy stage's pre-execution gate (needs write:deploy). |
| `fishhawk deploy reject` | `<run-id>` | Reject the deploy stage's pre-execution gate (category-D failure). |
| `fishhawk deploy rollback` | `<run-id>` | Roll back a settled deploy (re-dispatches the rollback path). |
| `fishhawk release preview` | — | Render release notes for a ref range without persisting. |
| `fishhawk release prepare` | — | Persist rendered release notes as a release_notes artifact. |
| `fishhawk release cut` | — | Record the operator's ratified release version (no git tag push). |
| `fishhawk release publish` | — | Write the notes to the GitHub Release body + asset. |
| `fishhawk campaign start` | — | Create a campaign from an epic ref. |
| `fishhawk campaign status` | `<campaign-id>` | Show a campaign's rollup status and next action. |
| `fishhawk campaign list` | — | List campaigns with optional filters. |
| `fishhawk campaign resume` | `<campaign-id>` | Resume a paused campaign (hand back to the auto-driver). |
| `fishhawk audit list` | `<run-id>` | List audit entries for a run. |
| `fishhawk audit tail` | `<run-id>` | Follow the audit log of a run in real time. |
| `fishhawk init` | — | Scaffold a repo for Fishhawk (workflow spec + agent docs + preflight). |
| `fishhawk validate` | `[path]` | Validate a workflow spec file locally. |
| `fishhawk migrate-spec` | `[path]` | Migrate a workflow-v1 spec to workflow-v2 with an approval-eligibility report. |
| `fishhawk runner start` | — | Spawn the fishhawk-runner locally against an already-minted run. |
| `fishhawk doctor` | — | Run local-loop install checks. |
| `fishhawk file-issue` | — | File a work item (issue/bug/chore/adr) via repo conventions. |
| `fishhawk diagnose` | `<run-id>` | Show a run's product-facts diagnostic bundle. |
| `fishhawk report-issue` | `<run-id>` | File an upstream Fishhawk product bug/feature with a redacted, deduped bundle. |
| `fishhawk export` | — | Assemble a complete compliance export (JSON or --csv) for external verification. |

### Flags per command

#### `fishhawk run start`

Flags: `--backend-url`, `--token`, `--timeout`, `--repo`, `--workflow`, `--workflow-sha`, `--trigger-ref`, `--runner-kind`, `--working-dir`, `--spec-file`, `--issue`, `--override-budget`, `--upstream-run-id`, `--applies-to-override`, `--applies-to-override-reason`

#### `fishhawk run status`

Flags: `--backend-url`, `--token`, `--timeout`, `--output`, `--o`

#### `fishhawk run list`

Flags: `--backend-url`, `--token`, `--timeout`, `--repo`, `--workflow`, `--state`, `--limit`, `--cursor`

#### `fishhawk run cancel`

Flags: `--backend-url`, `--token`, `--timeout`

#### `fishhawk run open`

Flags: `--backend-url`, `--token`, `--timeout`, `--print-url`

#### `fishhawk run retry`

Flags: `--backend-url`, `--token`, `--timeout`, `--output`, `--o`

#### `fishhawk run watch`

Flags: `--backend-url`, `--token`, `--timeout`, `--stage`, `--until`, `--poll`, `--max-duration`

#### `fishhawk plan approve`

Flags: `--backend-url`, `--token`, `--timeout`, `--reason`, `--output`, `--o`

#### `fishhawk plan reject`

Flags: `--backend-url`, `--token`, `--timeout`, `--reason`, `--output`, `--o`

#### `fishhawk plan revise`

Flags: `--backend-url`, `--token`, `--timeout`, `--constraint`, `--force`, `--output`, `--o`

#### `fishhawk token login`

Flags: `--backend-url`, `--token`, `--timeout`, `--provider`, `--client-id`

#### `fishhawk token list`

Flags: none.

#### `fishhawk deploy status`

Flags: `--backend-url`, `--token`, `--timeout`, `--output`, `--o`

#### `fishhawk deploy approve`

Flags: `--backend-url`, `--token`, `--timeout`, `--reason`, `--environment`, `--override-freeze`, `--output`, `--o`

#### `fishhawk deploy reject`

Flags: `--backend-url`, `--token`, `--timeout`, `--reason`, `--output`, `--o`

#### `fishhawk deploy rollback`

Flags: `--backend-url`, `--token`, `--timeout`, `--output`, `--o`

#### `fishhawk release preview`

Flags: `--backend-url`, `--token`, `--timeout`, `--repo`, `--from`, `--to`, `--output`, `--o`

#### `fishhawk release prepare`

Flags: `--backend-url`, `--token`, `--timeout`, `--repo`, `--from`, `--to`, `--stage-id`, `--output`, `--o`

#### `fishhawk release cut`

Flags: `--backend-url`, `--token`, `--timeout`, `--repo`, `--run-id`, `--artifact-id`, `--version`, `--stage-id`, `--bump-level`, `--output`, `--o`

#### `fishhawk release publish`

Flags: `--backend-url`, `--token`, `--timeout`, `--repo`, `--tag`, `--run-id`, `--artifact-id`, `--stage-id`, `--output`, `--o`

#### `fishhawk campaign start`

Flags: `--backend-url`, `--token`, `--timeout`, `--repo`, `--epic`, `--pause-policy`, `--operator-agent`, `--output`, `--o`

#### `fishhawk campaign status`

Flags: `--backend-url`, `--token`, `--timeout`, `--output`, `--o`

#### `fishhawk campaign list`

Flags: `--backend-url`, `--token`, `--timeout`, `--repo`, `--state`, `--limit`, `--cursor`

#### `fishhawk campaign resume`

Flags: `--backend-url`, `--token`, `--timeout`, `--output`, `--o`

#### `fishhawk audit list`

Flags: `--backend-url`, `--token`, `--timeout`, `--category`, `--stage`, `--limit`, `--cursor`, `--output`, `--o`

#### `fishhawk audit tail`

Flags: `--backend-url`, `--token`, `--timeout`, `--interval`, `--output`, `--o`, `--max-polls`

#### `fishhawk init`

Flags: `--backend-url`, `--token`, `--timeout`, `--preset`, `--working-dir`, `--budget-usd`, `--single-reviewer`, `--human-gates`, `--force`, `--repo`

#### `fishhawk validate`

Flags: none.

#### `fishhawk migrate-spec`

Flags: `--out`, `--in-place`, `--report-only`

#### `fishhawk runner start`

Flags: `--backend-url`, `--token`, `--timeout`, `--run-id`, `--stage-id`, `--workflow`, `--stage`, `--working-dir`, `--github-repo`, `--base-branch`, `--no-pr`, `--runner-binary`

#### `fishhawk doctor`

Flags: `--backend-url`, `--token`, `--timeout`, `--runner-binary`, `--working-dir`, `--repo`, `--spec-only`, `--run-verify-command`, `--skip-verify-command`, `--verify-timeout`

#### `fishhawk file-issue`

Flags: `--backend-url`, `--token`, `--timeout`, `--repo`, `--type`, `--summary`, `--body`, `--complexity`, `--status`, `--parent-epic`, `--run-id`, `--label`, `--supersedes`, `--companion-to`, `--evidence-run`, `--output`, `--o`

#### `fishhawk diagnose`

Flags: `--backend-url`, `--token`, `--timeout`, `--output`, `--o`

#### `fishhawk report-issue`

Flags: `--backend-url`, `--token`, `--timeout`, `--kind`, `--description`, `--include-free-text`, `--output`, `--o`

#### `fishhawk export`

Flags: `--backend-url`, `--token`, `--timeout`, `--from`, `--to`, `--repo`, `--run`, `--limit`, `--csv`, `--out`

<!-- END GENERATED cli -->
