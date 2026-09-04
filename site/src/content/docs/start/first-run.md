---
title: Your first run
description: Install the binaries, point Fishhawk at your own repository, and drive one issue through plan, implement, review and merge.
---

This page takes you from nothing installed to a merged pull request in **your**
repository. Every step names the exact command to run and what its output looks
like when it worked.

It is deliberately **not** the repository
[README's quickstart](https://github.com/kuhlman-labs/fishhawk#quickstart).
That quickstart stands the stack up so Fishhawk can change *itself* — it
validates the spec that already lives in the Fishhawk repository. This page
assumes the repository you care about is somewhere else, and that Fishhawk has
never touched it.

:::caution[This page is provisional]
Nobody outside the team has yet walked this guide end to end. Every command on
it is verified against the source it comes from and was run while the page was
written, but the *narrative* — that these steps in this order get a newcomer to
a merged pull request without a missing prerequisite — has not been checked by
a reader who did not write it. That validation happens at the first
design-partner onboarding
([E11.2 / #2257](https://github.com/kuhlman-labs/fishhawk/issues/2257)) and the
second-repository walk
([E36 / #1642](https://github.com/kuhlman-labs/fishhawk/issues/1642)). Until
then, treat a step that does not work as a gap in this page and
[file it](https://github.com/kuhlman-labs/fishhawk/issues/new).
:::

## Before you start

Fishhawk is pre-alpha. There is no installer, no hosted control plane, and no
published runner action — you build the binaries from source and run the
backend yourself. That means the baseline below is assumed, not installed for
you. Check all six now; a missing one surfaces two sections from here as a
confusing failure rather than as a missing dependency.

| You need | Why | Confirm with |
|---|---|---|
| Git | You clone the Fishhawk repository to build from, and Fishhawk drives git in your repository | `git --version` |
| [Go](https://go.dev/dl/) 1.25.0 or newer | Every module in the workspace declares `go 1.25`/`go 1.25.0`; an older toolchain refuses the build outright | `go version` |
| A container runtime with Compose v2 | `make up` shells to `docker compose up -d` for Postgres and MinIO | `docker compose version` |
| `make` | The repository's loops are Makefile targets | `make --version` |
| The [Claude Code](https://claude.com/claude-code) CLI on `PATH` as `claude`, with `ANTHROPIC_API_KEY` exported | The runner spawns that binary for the default `claude-code` executor and reads that variable for the key | `claude --version` |
| [`gh`](https://cli.github.com/), authenticated | `fishhawk run start --issue` shells to `gh issue view` to ship the issue body inline | `gh auth status` |

One nuance on the last row: the issue fetch is **best-effort**. A missing or
unauthenticated `gh` prints a warning and the run starts anyway — but the agent
then plans without the issue text, which is rarely what you want. Treat `gh` as
required in practice even though the code does not fail without it.

If you are missing something here, stop and install it. Nothing later on this
page recovers from an absent prerequisite.

## Get the binaries

There is no packaged install. No CLI release workflow exists in the repository,
no version tag has ever been cut, and `go install` does not work either — the
CLI module carries a filesystem `replace` directive for its sibling
`credstore` module, which module-aware installs refuse:

```console
$ go install github.com/kuhlman-labs/fishhawk/cli/cmd/fishhawk@latest
go: github.com/kuhlman-labs/fishhawk/cli/cmd/fishhawk@latest (in github.com/kuhlman-labs/fishhawk/cli@v0.0.0-20260825053024-219e3c55a98e):
	The go.mod file for the module providing named packages contains one or
	more replace directives. It must not contain directives that would cause
	it to be interpreted differently than if it were the main module.
```

Build from a clone instead. You need three binaries: the CLI you drive the loop
with, the runner the CLI spawns per stage, and `fishhawkd` — which you need on
`PATH` for the token step below even though you will run the *server* through
`make`.

```sh
git clone https://github.com/kuhlman-labs/fishhawk.git
cd fishhawk
go build -o bin/fishhawk ./cli/cmd/fishhawk
go build -o bin/fishhawk-runner ./runner/cmd/fishhawk-runner
go build -o bin/fishhawkd ./backend/cmd/fishhawkd
export PATH="$PWD/bin:$PATH"
```

Confirm all three resolve before going on:

```console
$ fishhawk version
dev
$ command -v fishhawk-runner
/path/to/fishhawk/bin/fishhawk-runner
$ command -v fishhawkd
/path/to/fishhawk/bin/fishhawkd
```

`fishhawk version` printing `dev` is correct — an unstamped build reports the
`dev` version, which also carries "do not enforce a minimum runner version"
semantics. `fishhawk-runner` has no `--version` flag; `command -v` is the
check that it is on `PATH`, which is what the CLI needs it for.

Packaged installs are tracked under
[E36 / #1638](https://github.com/kuhlman-labs/fishhawk/issues/1638).

## Stand up a backend

There is no hosted control plane to point at. The only published `fishhawkd`
artifacts are rolling container images
(`ghcr.io/kuhlman-labs/fishhawkd:main` and `:sha-<short>`), and the CLI defaults
`--backend-url` to `http://localhost:8080` (overridable with
`$FISHHAWK_BACKEND_URL`). So you run it locally, from the same clone:

```sh
cp .env.example .env     # optional now; GitHub App credentials go here later
make up                  # docker compose: Postgres :5432, MinIO :9000/:9001
make migrate             # apply the backend migrations — required before first serve
make dev-backend         # runs fishhawkd on :8080 in the foreground
```

Leave `make dev-backend` running and confirm from another shell:

```console
$ curl -s http://localhost:8080/healthz
{"status":"ok","version":"dev","git_sha":"219e3c55","min_runner_version":"dev"}
```

If `healthz` does not answer, nothing else on this page will work. Skipping
`make migrate` is the usual cause.

A hosted control plane is tracked under
[E36 / #1638](https://github.com/kuhlman-labs/fishhawk/issues/1638).

## Connect GitHub

Fishhawk reads issues, pushes branches, opens pull requests and reads review
state through a GitHub App you register on your own account. The authoritative
permission and event table is in
[`docs/github-app/README.md`](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/github-app/README.md)
under **Permissions** — follow it there rather than a copy here, which would
drift.

What your decision actually turns on: that document describes three modes, and
a backend on `localhost` puts you in **Mode B — App with OAuth, no webhooks**.
Register the App, uncheck the webhook's **Active** box, enable **Device Flow**,
and put the credentials in the `.env` at the root of the clone (`cp .env.example
.env` first if you skipped that above). These are the variables that carry them
— the App and OAuth blocks of `.env.example`, uncommented:

```sh
FISHHAWKD_GITHUB_APP_ID=123456
FISHHAWKD_GITHUB_APP_PRIVATE_KEY_FILE=/abs/path/fishhawk-app.private-key.pem
FISHHAWKD_GITHUB_WEBHOOK_SECRET=whatever-you-set
FISHHAWKD_OAUTH_CLIENT_ID=Iv1.xxxx
FISHHAWKD_OAUTH_CLIENT_SECRET=xxxx
FISHHAWKD_OAUTH_CALLBACK_URL=http://localhost:8080/v0/auth/github/callback
```

The Makefile includes and exports `.env`, so restart `make dev-backend` to pick
these up. `FISHHAWKD_GITHUB_WEBHOOK_SECRET` only gates `POST /webhooks/github`,
which Mode B never receives; leaving it unset costs you a startup warning and a
503 on that route, both expected here.

Mode B gives you UI sign-in and
CLI-driven run dispatch; it does *not* deliver GitHub events to your backend.
That is the mode this guide's CLI-driven path is written for — you drive each
gate by hand instead of waiting for a webhook to advance the run.

One consequence to keep in mind for the merge step: without webhooks, the
`pull_request`-closed event that settles a run to completed never arrives. The
merge still lands on GitHub; the run row stays open. That is expected in Mode B,
not a failure.

## Issue an operator token

Every CLI call that touches a run carries a bearer token. Issuing one is a
`fishhawkd` subcommand — **not** a `fishhawk` CLI verb. The CLI's `token`
subcommands are only `login` and `list`:

```console
$ fishhawk token issue
fishhawk token: unknown subcommand "issue"
```

Issuance writes a row to the backend's database, so it needs a database URL,
not a backend URL. Set it explicitly to the same value `make` uses:

```sh
export FISHHAWKD_DATABASE_URL='postgres://fishhawk:fishhawk@localhost:5432/fishhawk?sslmode=disable'
fishhawkd token issue --subject "$(whoami)"
```

```console
fishhawkd token issue: applying default operator scope set
fhk_<40-odd-opaque-characters>
issued token id=<uuid> subject=<you> scopes=[read:runs read:audit write:runs write:approvals write:stages write:deploy write:campaigns read:audit-export]
```

The middle line is the token — it is printed once and not recoverable. Export
it, along with the backend URL, so the rest of this page's commands pick both
up without flags:

```sh
export FISHHAWK_TOKEN=<the-token-printed-above>
export FISHHAWK_BACKEND_URL=http://localhost:8080
```

If you have not built `fishhawkd` onto `PATH`, the equivalent from inside the
clone is `go run ./backend/cmd/fishhawkd token issue --subject "$(whoami)"`,
with the same `FISHHAWKD_DATABASE_URL` exported.

## Scaffold your repository

Now switch to the repository you actually want Fishhawk to work on. `fishhawk
init` writes `.fishhawk/workflows.yaml` from an autonomy preset, ensures the
`AGENTS.md` + `CLAUDE.md` bridge, and runs a preflight.

The preset is a real decision, not a default to skim past. It sets how much
judgment the operator agent may exercise on your behalf:

- `low` delegates nothing — every judgment point pages you.
- `medium` delegates plan approval, fix-up passes and stage retries under named
  conditions; waiving a reviewer concern and merging stay yours.
- `high` adds waive and merge to that set.

This walkthrough uses `medium`, which is the shipped default and the tier the
rest of this page's gate sequence assumes:

```sh
cd /path/to/your-repo
fishhawk init --preset medium
```

Then open `.fishhawk/workflows.yaml` and replace the placeholders. The preset is
a starting document, not a finished configuration:

- **`executor.verify.command` on the `implement` stage is `make test`.** This is
  wrong for almost every repository that is not Fishhawk itself. Change it to
  your project's test entrypoint, or delete the whole `verify:` block if you
  have none. The runner executes this command in a fresh worktree after the
  agent exits, so gitignored build output and downloaded dependencies will not
  be there.
- **`constraints.forbidden_paths` and `max_files_changed`** are Fishhawk's own
  boundaries. Set them to yours.
- **`budgets[0].limit_usd`** is an advisory weekly ceiling of `$50`. It emits an
  alert, never blocks a run.
- **The commented `applies_to` and `escalations` starters** are inert until you
  uncomment them.

You do *not* need to fill in a reviewer handle. The approval gates use the
forge-neutral `approvals` predicate — `count: 1` with `not: [author, agent]`,
meaning one approval from somebody who is neither the change's author nor an
agent identity. There is no repository-specific name to substitute.

## Validate the spec

```sh
fishhawk doctor --spec-only
```

`--spec-only` runs the environment-free rungs — schema validity and
execution-path coverage — and skips every Docker, backend, token, MCP, git and
`gh` check. It is the right check for a freshly scaffolded repository, because
it passes or fails on your spec alone. A non-zero exit prints which rung failed.

## Open your first run

### Why `--runner-kind local`

`fishhawk run start` takes `--runner-kind`, documented as
`github_actions | local`. Use `local`. The `github_actions` value is not usable
from a repository other than Fishhawk's own today, for two independent reasons:

1. **There is no published runner action.** Fishhawk's own dispatch workflow
   invokes the runner as `uses: ./runner` — a repository-local path that
   resolves only inside a checkout of the Fishhawk repository. An external
   repository would need the published composite action the workflow's own
   comment names, and no version tag has ever been cut.
2. **The Actions path needs a publicly reachable backend.** That workflow
   resolves its backend URL to `vars.FISHHAWK_BACKEND_URL` or
   `http://localhost:8080` and calls it from a GitHub-hosted runner, where
   `localhost` is the runner itself — not your machine. The backend you stood
   up above is not reachable from there.

Both gaps are tracked under
[E36 / #1638](https://github.com/kuhlman-labs/fishhawk/issues/1638), alongside
the packaged install and the hosted control plane.

### What `local` means for you

Under `runner_kind=local` the backend **never spawns anything**. It cannot: the
local runner is host-spawned by design, so every agent stage parks at
`awaiting_host_dispatch` and waits for you. `fishhawk runner start` is the
spawn — a per-stage subprocess, not a daemon you leave running. It resolves the
runner binary in this order: `--runner-binary`, then `$FISHHAWK_RUNNER_BIN`,
then a `PATH` lookup of `fishhawk-runner`. Putting `bin/` on `PATH` earlier
already satisfies the third.

If you forget this, the symptom is a run that looks stuck with nothing in any
log. See [Troubleshooting](#troubleshooting).

### Start the run

```sh
fishhawk run start --repo <owner/name> --workflow feature_change --issue <N> --runner-kind local
```

It prints the run's fields, including the id you will use for every gate:

```console
id:             3f1c9a2e-4b7d-4c81-9a55-0e6b2d8f1c34
repo:           <owner/name>
workflow_id:    feature_change
state:          pending
runner_kind:    local
```

## Walk the gates

Five gates stand between the run you just opened and a merged pull request.
Each is named below with the exact invocation that passes it, and where a gate
has no `fishhawk` CLI verb, that is said plainly rather than glossed.

Every gate needs a **stage id**, which `fishhawk run status` does not print —
it renders the run, not its stages. Read them from the API:

```sh
curl -s -H "Authorization: Bearer $FISHHAWK_TOKEN" \
  "$FISHHAWK_BACKEND_URL/v0/runs/<run-id>/stages"
```

Two `curl` calls below post to the host-dispatch marker. That endpoint is what
flips a parked stage from `awaiting_host_dispatch` to `dispatched`, so
`dispatched` reliably means "a spawn attempt exists". The CLI's `runner start`
does not post it — the MCP host-spawn verbs do — so on a pure-CLI path you post
it yourself immediately before spawning.

### Gate 1 — run the plan stage

Mark the plan stage dispatched, then spawn the runner against it:

```sh
curl -s -X POST -H "Authorization: Bearer $FISHHAWK_TOKEN" \
  "$FISHHAWK_BACKEND_URL/v0/runs/<run-id>/stages/<plan-stage-id>/host-dispatch"

fishhawk runner start \
  --run-id <run-id> \
  --stage-id <plan-stage-id> \
  --workflow feature_change \
  --stage plan \
  --working-dir /path/to/your-repo
```

The agent writes a plan — a scope, an approach, a verification strategy — and
posts it as a rendered comment on the originating issue. Under the `medium`
preset two agent reviewers read it first and surface advisory verdicts; they do
not decide. You do.

### Gate 2 — approve or reject the plan

This gate has a CLI verb. Read the plan on the issue, then either:

```sh
fishhawk plan approve <run-id> --reason "Scope matches the issue; the migration is in scope.files."
```

or send it back:

```sh
fishhawk plan reject <run-id> --reason "Wrong fork: change the storage layer, not the handler."
```

Both take the **run** id and resolve the plan stage themselves. On `reject`,
your reason is what the next planning pass reads, so make it specific — a
reject without `--reason` warns you that the audit row will carry an empty
comment and the requester will be guessing.

On `approve`, your reason is **binding**: it is injected verbatim into the
implement agent's prompt as conditions it must follow. It does not add anything
to the plan's scope — naming a file path in the prose will not put that file in
scope.

### Gate 3 — dispatch the implement stage

Same two-step shape as the plan stage, against the implement stage id:

```sh
curl -s -X POST -H "Authorization: Bearer $FISHHAWK_TOKEN" \
  "$FISHHAWK_BACKEND_URL/v0/runs/<run-id>/stages/<implement-stage-id>/host-dispatch"

fishhawk runner start \
  --run-id <run-id> \
  --stage-id <implement-stage-id> \
  --workflow feature_change \
  --stage implement \
  --working-dir /path/to/your-repo \
  --base-branch main
```

The agent implements against the approved plan, the runner executes your
`verify.command` against the committed tree, feeds a test failure back for a
bounded fix loop, then pushes a branch and opens a pull request. Watch progress
with `fishhawk run status <run-id>` or `fishhawk audit tail <run-id>`.

### Gate 4 — review the pull request

**This gate has no `fishhawk` CLI verb. It is a forge action.** The review
stage's executor is a human and its approval predicate is the same
`count: 1, not: [author, agent]`. You satisfy it by approving the pull request
under your own GitHub identity:

```sh
gh pr review <pr-number> --approve --body "Diff matches the approved plan; verify command is green."
```

Agent reviewers will have posted advisory verdicts on the diff, including a
scope-drift flag if the change wandered outside the plan's `scope.files`. They
are input to your decision, not a substitute for it.

### Gate 5 — merge

**This gate has no `fishhawk` CLI verb either.** The merge is an audited
operator declaration, exposed as a REST endpoint and as the `fishhawk_merge_run`
MCP tool. On a pure-CLI path, post the verdict:

```sh
curl -s -X POST -H "Authorization: Bearer $FISHHAWK_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"verdict":"Reviewed and approved; merging."}' \
  "$FISHHAWK_BACKEND_URL/v0/runs/<run-id>/merge"
```

That records a `merge_verdict_recorded` audit entry and queues the squash merge
through the same seam the delegated path uses. It is idempotent — a repeated
post appends no duplicate verdict but re-queues the merge. It requires
`write:approvals`, which the default operator scope set from your token
includes.

The endpoint does not wait for the merge to land. In Mode B, with no webhook
delivery, the `pull_request`-closed event that would settle the run to completed
never arrives, so the run row stays open after GitHub merges the branch. Confirm
the merge on the forge:

```sh
gh pr view <pr-number> --json state,mergedAt
```

That is one issue, driven through five gates, to a merged pull request.

## Make the check enforce

Throughout the run Fishhawk posts a `fishhawk_audit_complete` Check Run on the
pull request. It carries the audit verdict: the plan is on file, the trace
bundle shipped, the agent review landed. What it does not do, on its own, is
stop a merge. A Check Run is a report until the repository's branch protection
makes it a required status check, and Fishhawk cannot make it one for you — the
App installation holds no `administration: write` permission.

Until you add it, gate 5 has a real hole. `fishhawk_merge_run` queues a GitHub
auto-merge, and an auto-merge on a branch that does not require the check will
fire as soon as the other protections clear — including while the agent review
verdict is still pending. The audit trail records what happened; nothing stopped
it.

Add the check to a repository ruleset on your default branch:

```sh
gh api -X POST "repos/<owner>/<repo>/rulesets" --input - <<'JSON'
{
  "name": "fishhawk audit gate",
  "target": "branch",
  "enforcement": "active",
  "conditions": {
    "ref_name": { "include": ["~DEFAULT_BRANCH"], "exclude": [] }
  },
  "rules": [
    {
      "type": "required_status_checks",
      "parameters": {
        "strict_required_status_checks_policy": false,
        "required_status_checks": [
          { "context": "fishhawk_audit_complete" }
        ]
      }
    }
  ]
}
JSON
```

If you already run branch protection through a ruleset, add the context to that
one instead of creating a second — two rulesets both requiring the check is
harmless but confusing to read later.

A required check is not automatically an enforced one. A ruleset can carry
`bypass_actors`, and classic branch protection with `enforce_admins: false`
exempts every repository admin. Either one is a legitimate escape hatch and
either one means somebody can merge past the gate.

Confirm what the forge actually enforces rather than assuming the API call took:

```sh
fishhawk doctor --repo <owner>/<repo>
```

The `merge gate enforced` rung reads your branch protection and reconciles it
against the check Fishhawk publishes. It reports one of three things:

- **ok** — the check is required on the default branch, naming each protection
  source that requires it and that source's own bypass condition.
- **warn, not required** — the check is published and nothing enforces it. The
  remediation names the exact context to add.
- **warn, unknown** — the question could not be settled, with a reason. The App
  installation may predate the `administration: read` permission, or the
  rulesets endpoint may be unavailable on your GitHub Enterprise version. This
  is not evidence the check is unrequired; it is evidence the doctor could not
  look.

Both warnings are non-fatal to the doctor's exit code. If you have decided this
repository does not need the gate, you stay informed and unblocked.

## Troubleshooting

### The run looks stuck and no runner log exists

Almost always the parked-stage case, and the failure mode this guide's own
`--runner-kind local` choice makes likeliest: the stage is sitting at
`awaiting_host_dispatch` waiting for a `fishhawk runner start` you have not
issued. Check the raw stage state:

```sh
curl -s -H "Authorization: Bearer $FISHHAWK_TOKEN" \
  "$FISHHAWK_BACKEND_URL/v0/runs/<run-id>/stages"
```

A `"state": "awaiting_host_dispatch"` means the backend wants that stage
executed but cannot spawn it. Post the host-dispatch marker and spawn the runner
as in [Gate 1](#gate-1--run-the-plan-stage). This is not an error state — it is
the local runner working as designed.

### Missing or invalid token

Symptom: a `401` or `403` from any CLI call that touches a run. `--token`
defaults to `$FISHHAWK_TOKEN` and silently accepts empty, so the first question
is whether the variable is populated at all. Check that without printing the
token — it is a live bearer credential, and a terminal transcript, a captured
log or a shared screen keeps whatever you echo into it:

```sh
if [ -n "${FISHHAWK_TOKEN:-}" ]; then
  printf 'FISHHAWK_TOKEN: set, %s characters\n' "${#FISHHAWK_TOKEN}"
else
  printf 'FISHHAWK_TOKEN: unset or empty\n'
fi
fishhawk run list --backend-url "$FISHHAWK_BACKEND_URL"
```

An `unset or empty` verdict explains a `401` on its own — `-n` is false for
both, and the CLI accepts either without complaint. If it reports a length and
the call still fails, the value is not the thing to inspect: reissue below
instead.

A `403 insufficient_scope` names the scope it wanted. Issue a fresh token with
the default operator scope set:

```sh
fishhawkd token issue --subject "$(whoami)"
```

A token minted against a *different* database than the one the running backend
uses will authenticate as unknown — confirm `FISHHAWKD_DATABASE_URL` matched
what `make dev-backend` is serving.

### `fishhawk doctor` exits non-zero

Run it again without `--spec-only` to see which rung failed and whether it is a
spec problem or an environment one:

```sh
fishhawk doctor --working-dir /path/to/your-repo
```

The full run adds Docker, backend-reachability, token, git and `gh` checks on
top of the spec rungs. If the spec rungs pass and an environment rung fails,
the scaffold is fine and the local setup is not. The `verify command` rung is
skipped by default — doctor will not execute a command string out of a
repository you have not reviewed unless you ask with `--run-verify-command`.

### The plan stage times out

The `medium` preset caps a plan stage at `max_runtime: 15m`, under a
workflow-wide `max_stage_runtime: 30m`. A timeout is recorded as a stage
failure; read what happened before retrying:

```sh
fishhawk audit list <run-id>
fishhawk run retry <plan-stage-id>
```

`run retry` takes a **stage** id, not a run id. If it times out repeatedly with
no agent output, check that `claude` is on `PATH` and `ANTHROPIC_API_KEY` is
exported in the shell you ran `fishhawk runner start` from — the runner spawns
that binary and reads that variable, and neither is inherited from the backend.

## Where to go next

This page covers first contact only. Driving runs day to day — fix-up passes,
reviewer concerns, scope amendments, recovering a stranded stage — is the
[operator guide's](/fishhawk/operating/driving-a-run/) subject. For why each
gate exists rather than how to pass it, read [Concepts](/fishhawk/concepts/).
