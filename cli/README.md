# fishhawk CLI

Command-line interface for the Fishhawk control plane. Wraps the HTTP API documented in `docs/api/v0.openapi.yaml` so users can drive runs from a terminal.

This directory is its own Go module (`github.com/kuhlman-labs/fishhawk/cli`) so it can be released independently of the backend and runner. Per ADR-014 (#78), the multi-module workspace lets each component carry its own version tag.

## Layout

- `cmd/fishhawk/` — the binary entrypoint. Subcommand dispatch in `main.go`, per-command flags in `run.go`, validate logic in `validate.go`.
- `internal/httpclient/` — typed wrapper around the backend API. Marshals `CreateRunInput`, decodes `Run`, surfaces `*APIError` for non-2xx responses.
- `internal/spec/` — workflow-spec validator. Embeds `workflow-v0.schema.json` (mirrored from `docs/spec/`; the schema-sync diff in CI fails if the copies drift) and runs JSON Schema validation locally so users iterate on errors before opening a PR.
- `internal/version/` — build-version package; set via `-ldflags` at release time.

## Status

E6.1 (#55), E6.2 (#33), E6.3 (#34), E6.4 (#35), E6.5 (#36) shipped: scaffold + `run start`, `run status`, `run list`, `run cancel`, `run open`, `validate`. E18.1 (#332), E18.2 (#333), E18.3 (#334), E18.4 (#335), E18.5 (#336) added `plan approve`, `plan reject`, `run retry`, `audit list`, `audit tail`. E23.8 (#1388) added `deploy status`, `deploy approve`, `deploy reject`, `deploy rollback`. E25.9 (#1448) added `campaign start`, `campaign status`, `campaign list`, `campaign resume`. E29.3 (#1504) added `init`. E9.4 (#1607) added `export`. E32.3 (#1550) added `run watch`.

## Subcommands

```
fishhawk run start    --repo R --workflow W --workflow-sha S [--trigger-ref REF] [--upstream-run-id UUID]
fishhawk run status   <run-id> [--output text|json]
fishhawk run list     [--repo R] [--workflow W] [--state S] [--limit N] [--cursor C]
fishhawk run cancel   <run-id>
fishhawk run open     <run-id> [--print-url]
fishhawk run retry    <stage-id> [--output text|json]
fishhawk run watch    <run-id> [--stage TYPE] [--until terminal|amendment|any] [--poll N] [--max-duration D]
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
fishhawk export       [--from RFC3339] [--to RFC3339] [--repo owner/name] [--run UUID]... [--limit N] [--csv] [--out PATH]
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

`export` assembles a **complete** compliance export for external verification (`GET /v0/audit/export`, or `GET /v0/audit/export.csv` with `--csv`). The two endpoints bound each page to whole runs and ride the partiality signal on response headers (`X-Fishhawk-Export-Complete` / `X-Fishhawk-Export-Next-Cursor`) because the JSON body is the verifier's strict three-field Export v1 shape (`{schema, exported_at, runs}`, decoded with `DisallowUnknownFields`) and cannot carry a cursor field. `export` follows that continuation automatically: it fetches pages until the server reports complete, unions the per-page `runs` maps byte-for-byte (each run subtree is kept as raw JSON so the entry hashes and signatures still verify), and emits ONE assembled file that is exactly the verifier's Export v1 wire shape. The global (run-less) chain partition rides the first page only under the reserved nil-UUID key, so the union is disjoint; a run key appearing on two pages, or a page reporting incomplete with no continuation cursor, is a hard error rather than a silent merge or an infinite loop. `--csv` concatenates the CSV pages instead, keeping only the first page's header row. Filter selection is server-authoritative: pass `--run UUID` (repeatable) **or** the `--repo`/`--from`/`--to` filter shape — the two modes are mutually exclusive and the CLI renders the server's `validation_failed` verbatim rather than pre-checking. `--out PATH` writes the assembled file atomically (temp file + rename), so a mid-pagination failure never leaves a partial file at the destination; without `--out` the export streams to stdout.

### External verification

`export` is the producer half of the audit-grade external-verification flow (ADR-008 / ADR-054):

1. Issue a `read:audit-export`-scoped token for the auditor (or run `export` yourself with an operator token).
2. `fishhawk export --from <RFC3339> --to <RFC3339> --repo owner/name --out export.json` (or `--run <UUID>` for an explicit set; add `--csv` for the spreadsheet rendering).
3. Hand `export.json` — which carries each run's public signing key and full chained audit trail — to the external party.
4. The external party runs `fishhawk-verify --export export.json`. It recomputes every entry hash and chain link with no backend trust required and exits `0` (every chain verified), `1` (one or more issues, e.g. `kind=hash_mismatch` for a tampered entry), or `2` (usage error: missing flag, unreadable file, malformed JSON).

A worked example of this flow, run against Fishhawk's own development audit log
and published with provenance + verification instructions, lives at
[`docs/compliance/`](../docs/compliance/) (E9.6 / #1609).

`run retry` takes a **stage** id, not a run id — retry is stage-scoped per the state machine. Pick the failed stage from `fishhawk run status <run-id> --output json` (`.stages[].id`).

`run start --upstream-run-id UUID` names the upstream `feature_change` run whose `ci_green` / `review_merged` a standalone deploy-only `release` run's `required_upstream` pre-flight gate evaluates (E23.11 / #1417). Distinct from `parent_run_id` — a deploy-gate safety reference, not a lineage link. The value is validated locally as a well-formed UUID before the round-trip; a malformed value exits with usage error without calling the backend.

`deploy` drives a run's deploy stage from the terminal. `deploy status` shows the deploy stage state plus the persisted `deployment` artifact (environment, ref, external run URL, outcome, and a rollback handle when one exists), or `deployment: (not yet recorded)` when no deployment has been attached yet. `deploy approve` / `deploy reject` decide the deploy stage's pre-execution gate through the same approvals endpoint as `plan`; `deploy approve` additionally requires the `write:deploy` scope, enforced server-side (ADR-038 / #1390) — a token without it surfaces a `403 insufficient_scope` (`required_scope: write:deploy`) verbatim. `deploy approve` requires `--environment=<allowed_env>` (one of the deploy stage's `allowed_environments`); the CLI composes it into the approval comment as `--environment=<env>`, which the backend deploy pre-flight parses (an absent or disallowed value is rejected `422 deploy_environment_not_allowed`). Pass `--override-freeze` to permit a deploy during a declared `change_freeze` (it appends a standalone `--override-freeze` token to the comment; only meaningful when the deploy stage declares `change_freeze`). `--reason` stays free-text and is appended after the flags — but it is rejected if it carries a standalone `--override-freeze` token unless `--override-freeze` is also set, and `--environment` must be a single whitespace-free token, so neither can smuggle a flag past the pre-flight. This composition is byte-for-byte identical to the MCP `fishhawk_approve_deploy` tool. `deploy reject` needs no environment and routes through the standard advance path. `deploy rollback` re-dispatches the same delegating pipeline down its rollback path (Fishhawk holds no prod credentials, so a rollback is just another delegating trigger); it only applies to a settled deploy (`409 deploy_not_settled` otherwise) and a run whose cached spec carries a delegating deploy stage (`422 rollback_unconfigured` otherwise).

`campaign` drives a campaign — the parent record of an epic-driven multi-issue run (ADR-047 / #1437) — from the terminal. `campaign start --repo R --epic E` mints a campaign from an epic ref (`issue:N`, `#N`, `N`, or a `.../issues/N` URL; normalized to the canonical `issue:N` the API expects) by decomposing the epic's child issues into a wave-ordered DAG; `--pause-policy pause_campaign|pause_item` (validated locally before the round-trip) sets what the auto-driver pauses on a gate hand-off, omitted to take the backend default. `--operator-agent <json|@file>` sets an optional campaign-level `operator_agent` delegation override (literal JSON, or `@path` to read it from a file; validated as JSON locally) that wholesale-replaces — never merges with — every issue-run's per-workflow `operator_agent` contract for the whole campaign; an explicit `{}` is a valid override that delegates no knobs (page on every action), and omitting the flag leaves each issue-run on its workflow default. `campaign status <campaign-id>` renders the campaign block, the distilled `next_action` (action + issue ref + detail), and a per-issue run grid (one line per item: issue ref, state, and its run id or `-` when unlinked). `campaign list` pages campaigns (`created_at` descending) with optional `--repo` / `--state` filters. `campaign resume <campaign-id>` hands a paused campaign back to the auto-driver after a human owned a run gate; a campaign with nothing to resume surfaces `409 campaign_not_paused`, and a token missing `write:campaigns` (required by `start` and `resume`) surfaces `403 insufficient_scope` verbatim.

`run watch` is the operator's **blocking wait-for-a-stage-to-settle** verb (E32.3 / #1550). Launch it (typically detached) alongside a `fishhawk_dispatch_stage` to block until a stage settles instead of grepping the per-run runner log for a guessed event name — the fragile contract that silently stalled runs when the guessed name never appeared. It resolves the stage id from `--stage <type>` (default `implement`; the operator passes a stage TYPE, not a raw id), then polls two already-existing long-poll endpoints: the durable `(run_id, stage_id)` stage-wait (`GET /v0/runs/{run_id}/stages/{stage_id}?wait`, #1252) and, when `--until` is `amendment` or `any`, the run's pending scope amendments (`GET /v0/runs/{run_id}/scope-amendments?wait`, #1035). `--until terminal|amendment|any` (default `any`) selects the settle condition. It exits with a distinct code per outcome class — `0` terminal-ok (the stage settled succeeded or a parked `awaiting_*` state), `1` failed (state `failed` or a non-nil `failure_category`) OR a transport/lookup error, `3` amendment-pending, `4` timeout (`--max-duration` elapsed, default 50m) — and writes **exactly one** JSON summary line to stdout (`{run_id, stage_id, stage_type, until, outcome, state, exit_code}`, `outcome` one of `terminal_ok|failed|amendment_pending|timeout|error`) so a caller can `jq` the last stdout line regardless of exit class. A settled stage ends the wait for **every** `--until` mode, including `amendment` — once the stage is terminal no amendment can arrive, so `--until amendment` returns the terminal outcome rather than hanging. `--poll` is the per-iteration stage-wait long-poll seconds (default 15, clamped to the backend's 30s cap). It changes no backend or runner code; it reuses endpoints that already exist.

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
