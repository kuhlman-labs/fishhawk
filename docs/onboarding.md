# Onboarding a repo to Fishhawk

This document covers the first-run preflight surfaced by `fishhawk doctor`. It
is the operator-facing companion to the E29.4 readiness endpoint
(`backend/internal/server/onboarding.go`) and the E29.5 doctor extension
(`cli/cmd/fishhawk/doctor_onboarding.go`).

## `fishhawk doctor`

`fishhawk doctor` runs a set of preflight rungs and prints, for each, one of
`ok` / `warn` / `fail` plus a remediation hint when the rung is not `ok`. The
command exits non-zero if any rung **fails**; warnings alone still exit 0.

```
fishhawk doctor [--repo owner/name] [--working-dir D] [--runner-binary P]
```

Beyond the local-loop rungs (Docker stack, backend reachability, token
acceptance, spec presence, runner binary, MCP registration, git remote/tree,
`gh` auth, version/schema drift), `doctor` runs the **onboarding preflight**:
the per-repo prerequisites that make a repo *look* onboarded but wedge on the
first run.

### `--repo`

The onboarding rungs target a specific GitHub repo. Pass it as
`--repo owner/name`. When omitted, `doctor` auto-detects it from the working
dir's git `origin` remote (via the same parser `fishhawk run start` uses). If
the repo cannot be resolved (no `github.com` origin), the onboarding rungs
degrade to a single **warning** prompting for `--repo` — they do not fail the
whole command.

### Onboarding rungs

Four rungs are read from the readiness endpoint
(`GET /v0/onboarding/readiness?repo=owner/name`) — they are server-side-only
checks the CLI cannot perform locally:

| Rung | Fails when | Remediation |
|---|---|---|
| **app installed** | the Fishhawk GitHub App is not installed on the target repo | install the App: `https://github.com/apps/fishhawk/installations/new` |
| **reviewer available: `<provider>`** (one per spec-declared reviewer) | the reviewer's backend is not wired on this deployment | the adapter's missing-env hint, carried verbatim (e.g. set `FISHHAWKD_ANTHROPIC_API_KEY`) |
| **token scope adequate** | the caller token lacks a run-driving scope | reissue with the named missing scope(s) via `fishhawkd token issue --subject <login> --scopes …` |
| **workflow spec (committed) valid** | the spec on the repo's default branch fails to parse/validate (`source==fetched && !valid`); **warns** when the spec is unavailable (App not installed / no spec on the default branch) | run `fishhawk validate` for details |

A fifth rung is checked **client-side** against the discovered
`.fishhawk/workflows.yaml`:

| Rung | Fails when | Remediation |
|---|---|---|
| **execution path configured** | *any* stage in the spec declares no executor (a spec that looks onboarded but wedges on the first unconfigured stage) | add an executor to each named stage (see `docs/spec/workflows-v0.md`) |

The execution-path rung reports `ok` **only** when *every* stage declares a
non-empty executor (`agent`, `human`, or a `delegate`). A mixed workflow —
some stages configured, at least one not — **fails**, and the remediation names
the unconfigured stage(s). It warns (rather than fails) when no spec is found;
the local-loop **workflow spec present** rung is the authority on a
hard-missing or schema-invalid spec.

### Degradation

The onboarding rungs never crash the doctor. A transport error, a non-200
response (401/403/5xx), or an unparseable body from the readiness endpoint
degrades to a single **warning** naming the failure, so a backend that does not
yet serve `/v0/onboarding/readiness` (or a token that is rejected) leaves the
rest of the preflight intact.

## `fishhawk init`

`fishhawk init` (E29.3) is the primary onboarding surface. It scaffolds a repo
for Fishhawk in one command: it writes a schema-valid `.fishhawk/workflows.yaml`
from an autonomy preset, ensures the managed agent-docs bridge (AGENTS.md +
CLAUDE.md), prints the out-of-band prerequisites it cannot perform, and runs the
`doctor` preflight above as a closing step.

```
fishhawk init [--preset low|medium|high] [--working-dir D] \
              [--budget-usd N] [--single-reviewer] [--human-gates ids] \
              [--force] [--repo owner/name]
```

### What it does

1. **Resolves the repo root** by walking up from `--working-dir` to the
   directory containing `.git` (falling back to `--working-dir` when no `.git`
   is found), then targets `<root>/.fishhawk/workflows.yaml`.
2. **Writes the workflow spec** from the chosen `--preset` (default `medium`)
   plus optional structured deltas, reusing the E29.1 preset generator — which
   validates its own output and fails closed on a delta that would break schema
   validity:
   - `--budget-usd N` overrides the `feature_change` weekly advisory cost
     ceiling (`budgets[0].limit_usd`).
   - `--single-reviewer` drops the Codex agent reviewer, leaving Claude alone
     on every stage.
   - `--human-gates id,id` keeps the human gate only on the named stages; any
     stage with a gate whose id is not listed has it removed (omit the flag to
     leave every gate as authored).
3. **Ensures the agent-docs bridge** via the E29.2 `bridge` package: the
   Fishhawk managed block in AGENTS.md and the `@AGENTS.md` import in CLAUDE.md.
   Both are idempotent and preserve content outside the managed markers, and
   `init` reports each file's per-file status (created / updated / unchanged).
4. **Prints the out-of-band checklist** — the three prerequisites `init`
   deliberately does not perform: installing the GitHub App, issuing an operator
   token, and configuring the execution path (the `.github/workflows/fishhawk.yml`
   workflow, `vars.FISHHAWK_BACKEND_URL`, and the reviewer API-key secrets).
5. **Runs the `doctor` preflight** so `init` finishes by telling the operator
   exactly which first-run rungs still need attention.

### Non-destructive

`init` refuses to clobber an existing `.fishhawk/workflows.yaml`: if the spec is
already present it prints the path and the `--force` escape hatch and exits
non-zero **without touching the file**. Pass `--force` to overwrite it. Offer-to-
merge an existing spec is out of scope for v1 — refuse is the safe default.

The bridge files are always merged idempotently (managed block / import line
only), so re-running `init` on an already-scaffolded tree yields a clean diff
even under `--force`.

### Guides, does not perform

`init` only writes files in the working tree. It does **not** install the App,
mint a token, or push the execution-path workflow — those cross an external
boundary and are the operator's to complete. The closing `doctor` run and the
printed checklist name each one. A `doctor` failure does not fail `init`: the
scaffold succeeded, so `init` reports the doctor issues and still exits 0.
