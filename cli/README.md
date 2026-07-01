# fishhawk CLI

Command-line interface for the Fishhawk control plane. Wraps the HTTP API documented in `docs/api/v0.openapi.yaml` so users can drive runs from a terminal.

This directory is its own Go module (`github.com/kuhlman-labs/fishhawk/cli`) so it can be released independently of the backend and runner. Per ADR-014 (#78), the multi-module workspace lets each component carry its own version tag.

## Layout

- `cmd/fishhawk/` — the binary entrypoint. Subcommand dispatch in `main.go`, per-command flags in `run.go`, validate logic in `validate.go`.
- `internal/httpclient/` — typed wrapper around the backend API. Marshals `CreateRunInput`, decodes `Run`, surfaces `*APIError` for non-2xx responses.
- `internal/spec/` — workflow-spec validator. Embeds `workflow-v0.schema.json` (mirrored from `docs/spec/`; the schema-sync diff in CI fails if the copies drift) and runs JSON Schema validation locally so users iterate on errors before opening a PR.
- `internal/version/` — build-version package; set via `-ldflags` at release time.

## Status

E6.1 (#55), E6.2 (#33), E6.3 (#34), E6.4 (#35), E6.5 (#36) shipped: scaffold + `run start`, `run status`, `run list`, `run cancel`, `run open`, `validate`. E18.1 (#332), E18.2 (#333), E18.3 (#334), E18.4 (#335), E18.5 (#336) added `plan approve`, `plan reject`, `run retry`, `audit list`, `audit tail`. E23.8 (#1388) added `deploy status`, `deploy approve`, `deploy reject`, `deploy rollback`. E25.9 (#1448) added `campaign start`, `campaign status`, `campaign list`, `campaign resume`. E29.3 (#1504) added `init`.

## Subcommands

```
fishhawk run start    --repo R --workflow W --workflow-sha S [--trigger-ref REF] [--upstream-run-id UUID]
fishhawk run status   <run-id> [--output text|json]
fishhawk run list     [--repo R] [--workflow W] [--state S] [--limit N] [--cursor C]
fishhawk run cancel   <run-id>
fishhawk run open     <run-id> [--print-url]
fishhawk run retry    <stage-id> [--output text|json]
fishhawk run auto-decide <run-id> [--poll N] [--max-duration D] [--dry-run]   # INTERIM (#1233)
fishhawk plan approve <run-id> [--reason R] [--output text|json]
fishhawk plan reject  <run-id> [--reason R] [--output text|json]
fishhawk deploy status   <run-id> [--output text|json]
fishhawk deploy approve  <run-id> --environment ENV [--override-freeze] [--reason R] [--output text|json]
fishhawk deploy reject   <run-id> [--reason R] [--output text|json]
fishhawk deploy rollback <run-id> [--output text|json]
fishhawk campaign start  --repo R --epic E [--pause-policy P] [--operator-agent <json|@file>] [--output text|json]
fishhawk campaign status <campaign-id> [--output text|json]
fishhawk campaign list   [--repo R] [--state S] [--limit N] [--cursor X]
fishhawk campaign resume <campaign-id> [--output text|json]
fishhawk audit list   <run-id> [--category C] [--stage UUID] [--limit N] [--cursor X] [--output text|json]
fishhawk audit tail   <run-id> [--interval D] [--output text|json] [--max-polls N]
fishhawk diagnose     <run-id> [--output text|json]
fishhawk report-issue <run-id> [--kind bug|feature] [--description T] [--include-free-text] [--output text|json]
fishhawk init         [--preset low|medium|high] [--working-dir D] [--budget-usd N] [--single-reviewer] [--human-gates ids] [--force] [--repo owner/name]
fishhawk validate     [path]                   # default: .fishhawk/workflows.yaml
fishhawk doctor       [--repo owner/name] [--working-dir D] [--runner-binary P]
fishhawk version
```

`doctor` runs the local-loop preflight: it checks the Docker stack (daemon, postgres, minio), backend reachability + token acceptance, the committed workflow spec, the runner binary, MCP registration, the git remote/working tree, `gh` auth, and cross-binary version/schema drift. Each rung prints `ok` / `warn` / `fail` plus a remediation hint; the command exits non-zero if any rung fails (warnings alone still exit 0).

Since E29.5 `doctor` also runs a per-repo **onboarding preflight** — the prerequisites that make a repo *look* onboarded but wedge on the first run. Four rungs are read from the backend readiness endpoint (`GET /v0/onboarding/readiness`): **app installed** (the Fishhawk GitHub App on the target repo), **reviewer available: `<provider>`** per spec-declared reviewer (carrying the adapter's missing-env hint), **token scope adequate** (the run-driving scope subset, with the missing scopes named), and **workflow spec (committed) valid** (the spec on the repo's default branch parses + validates). A fifth rung, **execution path configured**, is checked client-side against the discovered `.fishhawk/workflows.yaml`: it fails when *any* stage declares no executor, naming the unconfigured stage(s). The onboarding rungs target the repo named by `--repo owner/name`; when omitted it is auto-detected from the working dir's git origin, and an unresolved repo degrades to a single warning rather than failing the command. See `docs/onboarding.md` for the full check list and remediations.

`init` is the primary onboarding surface: it scaffolds a repo for Fishhawk in one command. It resolves the repo root (walks up from `--working-dir` to the `.git` boundary), writes a schema-valid `.fishhawk/workflows.yaml` from `--preset` (low/medium/high, default medium) plus optional deltas (`--budget-usd` overrides the weekly advisory cost ceiling; `--single-reviewer` drops the Codex agent reviewer; `--human-gates id,id` keeps human gates only on the named stages), then ensures the managed agent-docs bridge (AGENTS.md block + CLAUDE.md `@AGENTS.md` import). It reuses the E29.1 preset generator (which validates its output and fails closed on an invalid delta) and the E29.2 bridge package (idempotent). The spec write is **non-destructive**: an existing `.fishhawk/workflows.yaml` is refused (exit non-zero, file untouched) unless `--force`. `init` then prints the three out-of-band prerequisites it does not perform — install the GitHub App, issue an operator token, and configure the execution path (`.github/workflows/fishhawk.yml`, `vars.FISHHAWK_BACKEND_URL`, reviewer API-key secrets) — and closes by running the `doctor` preflight. A `doctor` failure does not fail `init`: the scaffold succeeded, so `init` reports the issues and still exits 0. See `docs/onboarding.md` for the full flow.

`diagnose` prints a run's **product-facts-only** diagnostic bundle (`GET /v0/runs/{id}/diagnostics`): run id, stage states, the failing stage's category + audit surface, audit sequence range, build versions + git SHAs, workflow spec hash, and runner kind. It is pure read — the bundle carries no diffs, paths, prompts, or free text, so it is safe to attach to an upstream Fishhawk product report.

`report-issue` files a deduped, audited **upstream Fishhawk product** bug or feature request (`POST /v0/runs/{id}/product-reports`), carrying the run's auto-collected diagnostic bundle. The destination is the fixed product repo, not the run's repo. By default the report carries **product facts only**; a dedup hit on the failure fingerprint appends an occurrence comment instead of opening a duplicate. Operator free text (`--description`) crosses the egress boundary **only** with the explicit `--include-free-text` consent flag, and is run through secret-redaction server-side first — without the flag the description is dropped with a warning. Egress requires the run's own run-bound token, and a per-repo `product_feedback` kill-switch returns `product_feedback_disabled`.

`run retry` takes a **stage** id, not a run id — retry is stage-scoped per the state machine. Pick the failed stage from `fishhawk run status <run-id> --output json` (`.stages[].id`).

`run start --upstream-run-id UUID` names the upstream `feature_change` run whose `ci_green` / `review_merged` a standalone deploy-only `release` run's `required_upstream` pre-flight gate evaluates (E23.11 / #1417). Distinct from `parent_run_id` — a deploy-gate safety reference, not a lineage link. The value is validated locally as a well-formed UUID before the round-trip; a malformed value exits with usage error without calling the backend.

`deploy` drives a run's deploy stage from the terminal. `deploy status` shows the deploy stage state plus the persisted `deployment` artifact (environment, ref, external run URL, outcome, and a rollback handle when one exists), or `deployment: (not yet recorded)` when no deployment has been attached yet. `deploy approve` / `deploy reject` decide the deploy stage's pre-execution gate through the same approvals endpoint as `plan`; `deploy approve` additionally requires the `write:deploy` scope, enforced server-side (ADR-038 / #1390) — a token without it surfaces a `403 insufficient_scope` (`required_scope: write:deploy`) verbatim. `deploy approve` requires `--environment=<allowed_env>` (one of the deploy stage's `allowed_environments`); the CLI composes it into the approval comment as `--environment=<env>`, which the backend deploy pre-flight parses (an absent or disallowed value is rejected `422 deploy_environment_not_allowed`). Pass `--override-freeze` to permit a deploy during a declared `change_freeze` (it appends a standalone `--override-freeze` token to the comment; only meaningful when the deploy stage declares `change_freeze`). `--reason` stays free-text and is appended after the flags — but it is rejected if it carries a standalone `--override-freeze` token unless `--override-freeze` is also set, and `--environment` must be a single whitespace-free token, so neither can smuggle a flag past the pre-flight. This composition is byte-for-byte identical to the MCP `fishhawk_approve_deploy` tool. `deploy reject` needs no environment and routes through the standard advance path. `deploy rollback` re-dispatches the same delegating pipeline down its rollback path (Fishhawk holds no prod credentials, so a rollback is just another delegating trigger); it only applies to a settled deploy (`409 deploy_not_settled` otherwise) and a run whose cached spec carries a delegating deploy stage (`422 rollback_unconfigured` otherwise).

`campaign` drives a campaign — the parent record of an epic-driven multi-issue run (ADR-047 / #1437) — from the terminal. `campaign start --repo R --epic E` mints a campaign from an epic ref (`issue:N`, `#N`, `N`, or a `.../issues/N` URL; normalized to the canonical `issue:N` the API expects) by decomposing the epic's child issues into a wave-ordered DAG; `--pause-policy pause_campaign|pause_item` (validated locally before the round-trip) sets what the auto-driver pauses on a gate hand-off, omitted to take the backend default. `--operator-agent <json|@file>` sets an optional campaign-level `operator_agent` delegation override (literal JSON, or `@path` to read it from a file; validated as JSON locally) that wholesale-replaces — never merges with — every issue-run's per-workflow `operator_agent` contract for the whole campaign; an explicit `{}` is a valid override that delegates no knobs (page on every action), and omitting the flag leaves each issue-run on its workflow default. `campaign status <campaign-id>` renders the campaign block, the distilled `next_action` (action + issue ref + detail), and a per-issue run grid (one line per item: issue ref, state, and its run id or `-` when unlinked). `campaign list` pages campaigns (`created_at` descending) with optional `--repo` / `--state` filters. `campaign resume <campaign-id>` hands a paused campaign back to the auto-driver after a human owned a run gate; a campaign with nothing to resume surfaces `409 campaign_not_paused`, and a token missing `write:campaigns` (required by `start` and `resume`) surfaces `403 insufficient_scope` verbatim.

`run auto-decide` is an **INTERIM** second-channel auto-decider for known-safe mid-stage scope amendments (#1233), to be **removed when #1232 (durable non-blocking dispatch) ships**. Launch it detached alongside a blocking `fishhawk_run_stage` so a known-safe amendment can be decided in-band while the driving MCP session is blocked. It polls the run's pending amendments (reusing the `?wait` long-poll, #1035) and **auto-approves only** amendments whose every path is a coupled `*_test.go` sibling — i.e. a `<dir>/<stem>_test.go` whose production sibling `<dir>/<stem>.go` is already in the run's approved-plan `scope.files`. Everything else (any non-test path, any production file, or a test whose production sibling is out of scope) is left undecided → today's fail-and-retry. It changes no backend or runner code; it reuses endpoints that already exist. Run it with the **operator/operator-agent token** (the decision endpoint rejects run-bound `fhm_` tokens). `--poll` is the per-iteration long-poll seconds (default 25, clamped to the backend's 30s cap); `--max-duration` bounds the overall loop (default 50m); `--dry-run` logs verdicts without POSTing.

`audit list` outputs NDJSON (one entry per line) when `--output json` is set so a long page can be piped through `head`/`tail` without breaking the parser.

`audit tail` polls the audit endpoint on a configurable interval (default 2s, minimum 500ms) and prints new entries as they land. It exits cleanly on Ctrl-C. There's no server-side SSE today — if streaming demand grows we'd add one and migrate the client.

## Global flags

| Flag | Env | Default |
|---|---|---|
| `--backend-url` | `FISHHAWK_BACKEND_URL` | `http://localhost:8080` |
| `--token` | `FISHHAWK_TOKEN` | `""` (dev backends with stubbed auth) |
| `--timeout` | — | `60s` |

`--token` will become required once `/v0/tokens` lands; for now most backends accept anonymous calls via the `authStub` middleware.

## Build and test

From the repo root (workspace-aware):

    go build ./cli/...
    go test -race ./cli/...

Or from this directory directly:

    go build ./...
    go test ./...

## Local invocation

    # Start a run
    fishhawk run start \
      --backend-url http://localhost:8080 \
      --repo kuhlman-labs/fishhawk \
      --workflow feature_change \
      --workflow-sha $(git rev-parse HEAD)

    # Watch its state
    fishhawk run status <run-id>

    # Pipe a machine-readable Run into jq (handy for demo / status loops)
    fishhawk run status <run-id> --output json | jq .state

    # List recent runs
    fishhawk run list --state running --limit 25

    # Approve the plan stage on a run from the terminal (ADR-019 / #320)
    fishhawk plan approve <run-id> --reason "scope looks right"

    # Reject — recording a reason is encouraged but not required
    fishhawk plan reject <run-id> --reason "scope too wide; split the migration"

    # Inspect the audit log without leaving the terminal
    fishhawk audit list <run-id>
    fishhawk audit list <run-id> --category approval_submitted --output json | jq .

    # Follow a run's audit log in a side terminal
    fishhawk audit tail <run-id> --interval 1s

## See also

- `docs/api/v0.openapi.yaml` — the contract this CLI consumes.
- `docs/api/v0.md` — human-readable companion.
- `docs/MVP_SPEC.md` §5.1.4 — CLI component definition.
