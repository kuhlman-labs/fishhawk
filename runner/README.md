# fishhawk/runner

The GitHub Action that runs an agent under a Fishhawk workflow stage and ships the signed trace bundle back to the backend. Customers reference the action as:

    uses: kuhlman-labs/fishhawk/runner@runner/v0.1.0

This directory is its own Go module (`github.com/kuhlman-labs/fishhawk/runner`) so it can be tagged independently of the backend and the CLI — the customer-facing version pin is on the runner alone. See [ADR-014 (#78)](https://github.com/kuhlman-labs/fishhawk/issues/78) for the multi-module rationale.

Tag prefix `runner/v…` follows the Go module convention for non-root modules in a monorepo. Self-execution in this repo uses `./runner` (the local path) rather than a tag; external customers pin a release.

## Layout

- `action.yml` — composite action manifest. Defines inputs, sets up the Go toolchain, invokes the binary.
- `cmd/fishhawk-runner/` — the binary entrypoint. Flag parsing in `flags.go`, dispatch in `main.go`.
- `internal/agent/` — the agent abstraction (`Invoker`, `Invocation`, `Result`, `Event`).
- `internal/agent/claudecode/` — adapter for Anthropic's Claude Code CLI.
- `internal/bundle/` — `*.jsonl.gz` trace bundle pack/unpack per ADR-007 (#71).
- `internal/plan/` — plan-artifact validator against `standard_v1` (E1.5 schema; embedded copy under `schemas/`).
- `internal/constraint/` — workflow-spec constraint evaluator (`forbidden_paths`, `allowed_paths`, `max_files_changed`, `required_outcomes`).
- `internal/gitdiff/` — thin shim around `git diff --cached --name-status -z <base>` producing a `constraint.Diff`. Compares <base>'s tree to the index, so the caller must stage everything with `git add -A` first (the runner's `computeAndEmitDiff` does this). Pre-#296 the form was `<base>...HEAD` and silently produced empty diffs when the agent's edits weren't committed yet. `RunPatch` (#585) additionally captures the full unified-diff hunk text (`git diff --cached <base>`, no `--name-status`) for content-level implement-review; the patch is size-capped at 256 KiB with a truncation marker and rides in the `git_diff` event's optional `patch` field (redacted with the rest of the event). It is additive trace content only — the policy engine reads the name-status list, never the patch — and a patch-compute failure degrades gracefully without failing the diff.
- `internal/upload/` — HTTP client for the backend's signing-key + trace endpoints; signs the bundle and POSTs.
- `internal/version/` — build-version package; set via `-ldflags` at release time.

## Status

E5.1 (#52) shipped the scaffold. E5.2 (#29) wired the Claude Code invocation harness. E5.3 (#30) added trace bundling. E5.4 (#31) added plan validation. E5.5 (#53) added constraint enforcement. E5.6 (#32) added signed trace shipping: with `--upload-trace` and `--stage-id`, the runner calls `POST /v0/runs/{run_id}/signing-key` to obtain an Ed25519 key, signs `sha256(bundle)`, and POSTs to `POST /v0/runs/{run_id}/trace`. Upload failures map to category-C (infrastructure) per MVP_SPEC §6 — and never override an earlier category-A or category-B failure.

- E5.7 (#54) — versioned, signed releases of `fishhawk/runner` with SBOM

## Inputs (action.yml)

| Input | Required | Description |
|---|---|---|
| `run-id` | yes | Workflow run identifier (UUID, supplied by backend dispatch). |
| `backend-url` | yes | Fishhawk backend URL the runner ships its trace bundle to. |
| `workflow` | yes | Workflow ID matching a key under `workflows:` in `.fishhawk/workflows.yaml`. |
| `stage` | yes | Stage ID within the workflow (e.g. `plan`, `implement`, `review`). |
| `agent` | no | Coding-agent provider to invoke (`claude-code`\|`codex`). Defaults to `claude-code`, preserving the historical Claude-only behavior. `codex` spawns the Codex CLI in non-interactive `exec` mode (`internal/agent/codex/`); any other value fails the stage category-A before the agent is invoked. The selected id is stamped into the trace bundle manifest's `agent` field. |
| `prompt-file` | no | Path to a file containing the constructed prompt. When unset the runner exits 0 without invoking the agent — useful for exercising the dispatch path before E5.2+ are wired upstream. |
| `working-dir` | no | The **repo root the run derives its working tree from**; defaults to the runner's CWD. On the `--fetch-prompt` path the runner provisions a per-run **lineage git worktree** under this repo's shared gitdir (`<git-common-dir>/fishhawk-worktrees/run-<root>`) and relocates the agent's effective working directory into it, so concurrent runs on one local host no longer share a single tree (E22.X / #1137). The worktree lives under `.git`, invisible to `git status`; solo runs get their own worktree, all children of one decomposition parent share one. See `docs/ARCHITECTURE.md` §4. |
| `max-tokens` | no | Hard cap on agent tokens (input + output); 0 means no cap. |
| `timeout` | no | Wall-clock cap on the agent invocation, e.g. `15m`. Default 15m. |
| `bundle-out` | no | Path to write the gzipped trace bundle. When set the runner produces an ADR-007 `*.jsonl.gz` artifact instead of JSONL on stdout. |
| `plan-out` | no | Path the agent writes its plan artifact to. When set, the runner validates the file against `standard_v1` after a successful agent invocation; a malformed plan demotes the run to category-B failure. With `upload-trace=true` the runner also POSTs the plan to `/v0/runs/{run_id}/plan` so the backend creates an `artifacts` row visible in the UI's plan review surface. Hardcoded to `/tmp/fishhawk-plan.json` in `.github/workflows/fishhawk.yml` to match the path the backend's plan-stage prompt instructs the agent to write to. |
| `constraints-file` | no | Path to a JSON file with the stage's constraints (`forbidden_paths`, `allowed_paths`, `max_files_changed`, `required_outcomes`, `ci_green`). |
| `check-base-ref` | no | Git ref to diff against for constraint evaluation. Constraints run only when both `constraints-file` and `check-base-ref` are set. |
| `upload-trace` | no | After the agent succeeds, issue a signing key from `backend-url` and POST the bundle to `/v0/runs/{run_id}/trace`. The runner ships **both** variants per stage: `raw` (compliance-gated) and `redacted` (default-readable; produced by `redaction.RedactDefault`). |
| `stage-id` | no | Stage UUID for trace upload (distinct from `stage` which is the workflow-spec stage name). Required with `upload-trace`. |
| `anthropic-api-key` | no | API key forwarded to Claude Code as `ANTHROPIC_API_KEY` when `agent=claude-code`. Populated from a GitHub Secret. |
| `openai-api-key` | no | API key forwarded to the Codex CLI as `OPENAI_API_KEY` when `agent=codex`. Populated from a GitHub Secret. Unused when `agent=claude-code`. |

The agent API key is sourced per provider from the host environment: `claude-code` reads `ANTHROPIC_API_KEY`, `codex` reads `OPENAI_API_KEY`. Customers populate these from their GitHub Secrets. v0.x will replace this with a Fishhawk-issued ephemeral key (MVP_SPEC §5.3).

The composite action installs the CLI matching the selected `agent` via Node 22 — `@anthropic-ai/claude-code` (the `claude` binary) for `claude-code`, or `@openai/codex` (the `codex` binary) for `codex`. Hosted Actions runners don't ship with either, and each adapter invokes its binary by name. Cold-cache install adds ~15s; pinning a version is deferred (v1+).

For implement stages the runner additionally commits the agent's edits, pushes a fresh branch, opens a PR, and ships a `pull_request` artifact to the backend. **Push and PR creation use the Fishhawk App's installation token** (fetched from `POST /v0/runs/{run_id}/installation-token` per #197) — installing the App is the only repo-side dependency. The workflow's `GITHUB_TOKEN` doesn't need elevated permissions, and the customer doesn't need to enable "Allow Actions to create and approve pull requests" in repo settings. Branch name is `fishhawk/run-<short>/stage-<short>`. A clean working tree (agent decided no changes were needed) skips push + PR cleanly without failing the stage; the trace records an `implement_no_changes` event so the approver can see why.

The compile/test/verify gates run *committed agent-authored code* (`go vet`, `go test`, and the spec `executor.verify.command`). For the implement stage the spec `executor.verify.command` runs `scripts/test verify` (golangci-lint per module THEN the test loop, no coverage), so formatting/lint defects fail the stage's verify in-loop rather than red-lining the PR in CI after the agent is terminal (#1064). Because `sanitizedGateEnv` passes PATH through, golangci-lint on the runner's PATH is reachable; `scripts/test verify` fails closed with an actionable error if it is absent, never silently skipping lint. Those subprocesses run with the runner's credentials stripped from their env (ADR-029 #650 item 4, `sanitizedGateEnv`): the GitHub App installation token, agent API keys, and MCP backend token are NOT visible to agent code — only PATH/HOME/system essentials and the Go toolchain (`GO*`/`CGO_*`) vars are passed through. The git-plumbing operations (worktree/rev-parse/reset) keep the inherited env so push/auth still work.

## Pre-checkout App-token flow (E5.X / #201)

The canonical `fishhawk.yml` workflow opens with three steps before the runner:

1. **Inline OIDC exchange** (or `kuhlman-labs/fishhawk/auth@auth/vX.Y.Z` for customers using the published action) — fetches an OIDC ID token bound to the workflow run via the `ACTIONS_ID_TOKEN_REQUEST_*` env vars, exchanges it at the backend's installation-token endpoint, masks the result with `::add-mask::`, and writes it to `$GITHUB_OUTPUT`.
2. **`actions/checkout@v6` with `token: ${{ steps.fishhawk-auth.outputs.token }}`** — sets up the local `http.<host>.extraheader` with the App's token, so the initial clone authenticates as the App.
3. **`./runner`** — the runner always mints a fresh App token at push-time via the backend's installation-token endpoint (`auth_method=ed25519`, signed by the per-run signing key), so a long-running implement stage doesn't outlive the auth pre-step's ~1-hour-TTL token. The fresh token is written to `http.<host>.extraheader` via `git config --local --replace-all` immediately before push; `gitops.CommitAndPush.PushToken` is the field that flows it through.

The audit ledger ends up with two `installation_token_issued` events per implement stage: the OIDC one at workflow start (used by `actions/checkout`), and the Ed25519 one right before push (used by `git push` + PR creation).

The workflow needs `permissions: id-token: write, contents: read`. Installing the App is the only repo-side dependency.

## Choosing the coding agent (Claude Code or Codex)

The runner can drive either of two coding-agent providers, selected by the `agent` action input (see the [Inputs](#inputs-actionyml) table above). The provider story (#839 runner provider selection, #840 the Codex adapter, #841 the Actions wiring):

| `agent` | Adapter | API key env var | GitHub secret |
|---|---|---|---|
| `claude-code` (default) | `internal/agent/claudecode/` | `ANTHROPIC_API_KEY` | `ANTHROPIC_API_KEY` |
| `codex` | `internal/agent/codex/` | `OPENAI_API_KEY` | `OPENAI_API_KEY` |

- **Default and fallback.** Omitting `agent` selects `claude-code`, so existing workflows are unchanged. Any value other than `claude-code` or `codex` fails the stage **category-A before the agent is invoked** (`selectInvoker` returns `errUnknownAgent` in `cmd/fishhawk-runner/agentselect.go`) — a typo can't silently fall through to the wrong provider.
- **Codex key wiring.** Pass `agent: codex` plus `openai-api-key: ${{ secrets.OPENAI_API_KEY }}` to the action. The composite action threads that input into the `OPENAI_API_KEY` environment variable only when `agent == 'codex'` (`runner/action.yml`), and the codex adapter forwards it to the `codex` CLI child. The `anthropic-api-key` / `openai-api-key` inputs are independent; the unused one is left empty.
- **Trace attribution.** The selected provider id is stamped into the trace bundle manifest's `agent` field, so a post-hoc reviewer can see which agent produced the run.
- **Provenance on startup (#1741).** The `runner_started` log line records `agent_kind` (the selected provider), `agent_binary` (the resolved CLI executable), and `agent_version` (that binary's probed `--version` line, or `unknown` when the binary has no working `--version`), so the exact agent build behind a run is recoverable from the logs alone.

### Pinning the agent CLI binary (`FISHHAWK_AGENT_BIN` / `FISHHAWK_CODEX_BIN`, #1741)

By default each adapter resolves its CLI by name against `PATH` (`claude` for `claude-code`, `codex` for `codex`). To pin a **known-good** build without touching a global symlink, set an operator environment variable on the host that runs the runner:

| `agent` | Override env var | Overrides |
|---|---|---|
| `claude-code` | `FISHHAWK_AGENT_BIN` | the `claude` binary |
| `codex` | `FISHHAWK_CODEX_BIN` | the `codex` binary |

The value may be an absolute path or a name resolvable on `PATH`. A whitespace-only value is treated as unset, so an empty export never shadows the adapter default. The resolved binary is both probed for its version (recorded as `agent_version`) and invoked as the agent — the log and the invocation can never disagree.

> **Recommended: disable agent-CLI auto-update on runner hosts.** A CLI that silently auto-updates can introduce a breaking change mid-flight — exactly the #1741 incident, where Claude CLI `2.1.205` tightened `--json-schema` validation and took every plan stage down. Pin a vetted version (uninstall/reinstall a specific version, or point `FISHHAWK_AGENT_BIN` / `FISHHAWK_CODEX_BIN` at a pinned build) and turn off the CLI's self-update on operator hosts so upgrades are deliberate and reviewable. The `agent_version` on `runner_started` lets you confirm which build actually ran.

### Local verification with a fake Codex binary

You can exercise the codex dispatch path without the real OpenAI CLI or an API key by putting an executable named `codex` early on `PATH` that emits a canned `codex exec --json` event stream. The codex adapter parses newline-delimited JSON events; a minimal happy-path transcript (mirroring the helper in `internal/agent/codex/codex_test.go`) is:

```sh
mkdir -p /tmp/fakebin
cat > /tmp/fakebin/codex <<'EOF'
#!/usr/bin/env bash
echo '{"type":"thread.started","thread_id":"t-1"}'
echo '{"type":"turn.started"}'
echo '{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"hello"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":42,"cached_input_tokens":10,"output_tokens":50,"reasoning_output_tokens":8}}'
EOF
chmod +x /tmp/fakebin/codex

echo "Summarize the README" > /tmp/prompt.txt
PATH="/tmp/fakebin:$PATH" go run ./cmd/fishhawk-runner \
  --run-id 11111111-2222-3333-4444-555555555555 \
  --backend-url http://localhost:8080 \
  --workflow feature_change \
  --stage plan \
  --agent codex \
  --prompt-file /tmp/prompt.txt \
  --bundle-out /tmp/trace.jsonl.gz
```

The fake binary stands in for the real `codex` so the adapter's event-parse, token-accounting, and bundle paths run end-to-end against a deterministic transcript.

### Hosted Actions verification

To verify against the real OpenAI CLI on a hosted Actions runner:

1. Add the repo secret `OPENAI_API_KEY`.
2. Pass `agent: codex` and `openai-api-key: ${{ secrets.OPENAI_API_KEY }}` to the action.

The composite action's `Install Codex CLI` step installs the pinned `@openai/codex@0.137.0` (a specific immutable version per CLAUDE.md's run-time-tool pinning rule — never a floating tag) via Node 22 and invokes the `codex` binary by name.

### Migration note

Existing Claude Code users need no changes: `agent` defaults to `claude-code` and behavior is byte-identical to before provider selection landed. Opting into Codex is a per-stage `executor.agent: codex` in `.fishhawk/workflows.yaml` plus the `OPENAI_API_KEY` secret wired through `openai-api-key` — nothing else changes.

## Build and test

From the repo root (workspace-aware):

    go build ./runner/...
    go test -race ./runner/...

Or from this directory directly:

    go build ./...
    go test ./...

To mirror the implement-stage verify gate locally, run the repo-root wrapper (`scripts/test lint` for golangci-lint per module, `scripts/test verify` for lint + tests).

## Local invocation

The same binary the action runs can be invoked locally for development:

    # Dispatch-path probe (no agent invocation)
    go run ./cmd/fishhawk-runner \
      --run-id 11111111-2222-3333-4444-555555555555 \
      --backend-url http://localhost:8080 \
      --workflow feature_change \
      --stage plan

    # With the Claude Code harness (E5.2+) and bundled output (E5.3+)
    echo "Summarize the README" > /tmp/prompt.txt
    ANTHROPIC_API_KEY=sk-... go run ./cmd/fishhawk-runner \
      --run-id 11111111-2222-3333-4444-555555555555 \
      --backend-url http://localhost:8080 \
      --workflow feature_change \
      --stage plan \
      --prompt-file /tmp/prompt.txt \
      --max-tokens 50000 \
      --timeout 5m \
      --bundle-out /tmp/trace.jsonl.gz

    # Inspect the bundle: manifest first, trailer last (with content hash).
    gunzip -c /tmp/trace.jsonl.gz | jq -c .

When `--prompt-file` is set the runner invokes Claude Code; the structured runner log lines (`runner_started`, `runner_completed`) go to stderr. With `--bundle-out`, captured events are packed into `*.jsonl.gz` per ADR-007. Without it, events fall back to JSONL on stdout.

To pin a known-good agent CLI build on a local (or any operator) host — and the recommendation to disable the CLI's auto-update so an upgrade like the #1741 Claude CLI `2.1.205` break can't land mid-run — set `FISHHAWK_AGENT_BIN` / `FISHHAWK_CODEX_BIN`; see [Pinning the agent CLI binary](#pinning-the-agent-cli-binary-fishhawk_agent_bin--fishhawk_codex_bin-1741) above. The `runner_started` line's `agent_binary`/`agent_version` fields confirm which build actually ran.

### Progress heartbeats (#580)

While the agent runs, the runner writes a `stage_progress` liveness line to stderr every ~15 seconds:

    {"event":"stage_progress","elapsed_seconds":42,"turns":7,"tokens_so_far":13402,"last_event_kind":"assistant"}

The counters are coarse and structural — elapsed seconds, parsed-event count, cumulative tokens, and the last event kind — never agent payload text. The cadence is time-driven, so a stalled stage keeps emitting heartbeats with non-advancing `turns`/`tokens_so_far`, distinguishing "alive and progressing" from "stuck". These lines go to stderr **only**: they never enter the signed trace bundle. The `fishhawk-mcp` `fishhawk_run_stage` tool forwards them as MCP progress notifications. There is no flag to disable them in normal operation; they are suppressed only when the runner is driven without a progress sink (not reachable from the CLI).

### Out-of-tree write detection (#611)

The agent runs under `--dangerously-skip-permissions` (a `--print` non-interactive invocation has no human to answer Claude's permission prompts; the trace bundle is the authoritative after-the-fact record). Empirically, no claude-native `--permission-mode` confines filesystem writes while still allowing the arbitrary non-interactive Bash the implement stage needs (`go build/test`, `golangci-lint`, `scripts/test`): the modes that confine the Write/Edit tools also deny that Bash, and the modes that allow it leave a shell-redirect (`>`) escape hatch. True confinement therefore requires an OS-level sandbox, which is deferred to an ADR (see Notes).

As a purely additive safety net, the runner inspects each `assistant` stream-json line and emits an `out_of_tree_write` trace event for any file-writing tool call (`Write`, `Edit`, `MultiEdit`, `NotebookEdit`) whose target path falls outside the working tree plus the allowlisted extra dirs (`/tmp`, shared with `--add-dir` so the flag and the detector can't drift):

    {"kind":"out_of_tree_write","ts":"…","payload":{"path":"/Users/op/.claude/memory.md","tool":"Edit","run_id":"…","stage":"implement"}}

This makes a previously invisible boundary crossing (the #601 class) visible in the trace bundle and audit log. Important limits:

- **Surfacing only, never blocking.** The detector is additive: it appends a warning event and does **not** flip `OK` to false or fail the stage. It is also fail-open — an unparseable or unknown-shape line yields no event and never panics, so a stream-json schema drift across claude versions degrades to no-signal rather than a crash.
- **Residual gap.** It catches writes through the Write/Edit **tools** only. **Bash-mediated writes** (shell `>` redirects) are NOT visible to it. Closing that gap, and confining writes rather than merely surfacing them, is the OS-sandbox ADR's domain.
- Containment is resolved against the target's deepest **existing** ancestor (the common case is a brand-new file that doesn't exist yet) and canonicalises symlinks first, so e.g. macOS's `/tmp` → `/private/tmp` symlink does not cause false positives.

### Acceptance-stage egress containment + target credentials (E31.4 / #1532, ADR-050)

The acceptance stage is the one agent invocation that holds code execution, network access, and credentials at once, so the runner contains it (packages `internal/egressproxy` + `internal/acceptenv`; consumed by the E31.7 acceptance executor):

- **Default-deny egress proxy.** The invocation's `HTTP(S)_PROXY` points at a runner-embedded filtering proxy whose allow-list is exactly the workflow spec's `egress.target_hosts` (the only customer-controlled entries), the model API endpoint, and the Fishhawk backend. Anything else is refused `403`. Hostname resolutions are DNS-pinned for the proxy's lifetime and a public hostname resolving into loopback/private space is refused (anti-rebinding). Residual: the proxy env binds cooperating HTTP clients — raw-socket bypass needs the OS sandbox (same residual class as the write detector above).
- **`FISHHAWK_ACCEPTANCE_ENV_<NAME>` (operator input).** The explicit channel for customer-supplied target-instance test credentials: set `FISHHAWK_ACCEPTANCE_ENV_APP_PASSWORD=…` on the runner env and the acceptance invocation sees `APP_PASSWORD=…`. Everything else is default-denied; the model API keys are the one secret class that survives. The acceptance invocation NEVER carries `FISHHAWK_API_TOKEN` (its evidence ships signature-authed, no MCP token — ADR-050) or any repo/deploy token, and a passthrough whose stripped name collides with a denied key or a proxy variable is refused and logged, never honored.

### Acceptance target-identity gate + preview provisioning (E31.18 / #1569)

`docs/acceptance-preview.md` (E36.2 / #1640) is the full, build-system-agnostic hook-contract reference (injected env vars, exit-code semantics, timeouts, the teardown-on-every-return guarantee, the runner's event vocabulary, and a self-contained docker-compose worked example). The summary below covers the dogfood posture.

Before the acceptance agent spawns, the runner verifies that the first spec-declared `egress.target_hosts` entry actually serves the run's merge candidate — otherwise acceptance validates whatever build happens to answer there (typically current `main`). The backend sends the expected head SHA on the acceptance prompt response (`acceptance_expected_head_sha`); the runner probes `<host>/healthz` (http first for loopback/IP-literal hosts, https first otherwise, always falling back to the other scheme) and compares the body's `git_sha` build identifier:

- **verified** — `git_sha` is a ≥7-char prefix of the expected head. Logged `acceptance_target_verified`; the agent spawns.
- **stale** — a `git_sha` is exposed but mismatched, **including any `-dirty`-suffixed value** (a dirty build is not the committed merge candidate — fail closed). Stage fails pre-spawn, category C, reason `acceptance_target_stale`, expected-vs-got in the detail.
- **unreachable** — no scheme produced an HTTP response. Stage fails pre-spawn, category C, reason `acceptance_target_unreachable`.
- **unverifiable** — reachable but no comparable identity (non-200, non-JSON, missing/`unknown` `git_sha`, or an older backend sent no expectation). Logged `acceptance_target_unverified` and the agent **proceeds** — mixed-version compat, never a hard fail on a missing identifier.

No declared target hosts skips the gate entirely. The probe dials direct from the runner process — the egress proxy contains the agent, not the runner.

#### Backend wire and gate internals

`backend/internal/server/prompt.go::resolveAcceptanceExpectedHeadSHA` resolves the run's merge-candidate identity — the newest reported PR-head SHA from the reported-head audit ledger (the `resolveFixupExpectedHeadSHA` pattern; best-effort WARN-and-omit on an empty ledger or read error, so a ledger gap can never hard-fail a stage) — and both prompt handlers set it as `acceptance_expected_head_sha` on their acceptance branches, next to `egress_target_hosts`.

The runner decodes it on `runner/internal/upload/upload.go::FetchedPrompt.AcceptanceExpectedHeadSHA` (byte-identical json tag, lockstep-asserted). The "merge candidate" is the PR head SHA, not a synthesized merge commit — a base that moved after branch-off is exercised by the PR's required CI at merge time.

Runner side, `runner/cmd/fishhawk-runner/previewprobe.go::acceptanceTargetGate` implements the provision → readiness → gate sequence, wired into `main.go`'s acceptance pre-spawn block; `probeTargetIdentity` performs the `/healthz` GET and the four-outcome classification above.

Operator env vars (runner-process config; acceptenv excludes all of them from the agent env):

- **`FISHHAWK_ACCEPTANCE_PREVIEW_CMD`** — optional provisioning hook, run via `sh -c` in the operator's dispatch `working_dir` (falling back to the runner's cwd when none was dispatched) **before** the identity gate, with `FISHHAWK_PREVIEW_SHA` (the expected head) and `FISHHAWK_PREVIEW_TARGET_HOST` (the first declared target host) added to its env. Anchoring to the dispatch checkout means a relative provision command like `scripts/dev preview` now resolves correctly from a worktree-launched MCP session — the operator's checkout carries the untracked `.env`, the runner-inherited fishhawk-mcp cwd does not (#1746). The dogfood value is `scripts/dev preview`. A non-zero exit or timeout fails the stage pre-spawn, category C, reason `acceptance_preview_provision_failed` (exit state + output tail in the detail). After a successful provision the runner readiness-polls the probe every 2s until verified or the ready budget expires; without a provision command the gate is single-shot (3 quick attempts absorb connection blips, definitive answers gate immediately). The provisioned preview instance runs UNTRUSTED merge-candidate code, so the dogfood `scripts/dev preview` hands that binary a **least-privilege** database credential — a dedicated non-superuser role that owns only the throwaway `<db>_preview` database and is denied `CONNECT` to the dev database — never the operator's superuser URL (E31.19 / #1577). An external operator wiring a custom provision command against a shared Postgres should mirror this: give the preview binary a role scoped to a throwaway database, not the admin credential.
- **`FISHHAWK_ACCEPTANCE_PREVIEW_TEARDOWN_CMD`** — optional teardown hook (same `sh -c` + env contract), deferred so it runs on **every** post-provision exit: after the verdict ships on the happy path, and before the stage failure returns on readiness-timeout/stale/any pre-spawn gate failure. Best-effort — a teardown failure logs `acceptance_preview_teardown_failed` and never changes the stage outcome. Configuring a provision command with **no** teardown command leaks the provisioned instance; the runner emits an advisory `acceptance_preview_teardown_missing` warning on that path (it does not block provisioning — a self-tearing-down provision command is legitimate).
- **`FISHHAWK_ACCEPTANCE_PREVIEW_TIMEOUT_SECS`** — provision/teardown command budget (default 300; the command typically includes a Go build).
- **`FISHHAWK_ACCEPTANCE_PREVIEW_READY_TIMEOUT_SECS`** — post-provision readiness budget (default 60).

#### Dogfood preview (`scripts/dev preview` / `preview-down`)

`scripts/dev preview [<sha-or-ref>]` (the arg defaults to `$FISHHAWK_PREVIEW_SHA` so the provisioning hook can invoke it bare) fetches + resolves the merge candidate and checks it out in a detached worktree under `.fishhawk/cache/preview-worktree` — inside the already-gitignored `.fishhawk/cache/`, so a live preview never dirties the main tree's `git status` or `-dirty`-stamps dev builds.

It builds fishhawkd GitSHA-stamped via the existing `_build_ldflags` path (clean detached checkout → no `-dirty`), DROP/CREATEs the isolated `<dbname>_preview` database on each start (PR-branch migrations never touch the dev DB) and migrates it with the preview binary, then serves on `FISHHAWK_PREVIEW_ADDR` (default `localhost:8090`, distinct from the orchestrating 8080), gated on `/healthz` echoing the stamped `git_sha` (pid tracked in `.fishhawk/preview.pid`). `scripts/dev preview-down` TERM→KILLs the tracked pid and removes the worktree.

**Least-privilege preview credential (E31.19 / #1577).** The UNTRUSTED merge-candidate binary is NOT handed the operator's dev-Postgres superuser URL. `scripts/dev preview` provisions a dedicated non-superuser role (default `fishhawk_preview`; `FISHHAWK_PREVIEW_DB_ROLE` / `FISHHAWK_PREVIEW_DB_PASSWORD` overridable) that OWNS only the throwaway `<dbname>_preview` database and is DENIED CONNECT to the dev database (`REVOKE CONNECT … FROM PUBLIC` and explicitly from the role).

The role is NORMALIZED (`ALTER ROLE … NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS`) on every provision, so a pre-existing/tampered role is forced back to the contract. The admin (operator) URL is used ONLY for the privileged role/DB provisioning and never reaches branch code; the preview URL the binary receives bears the least-privilege role.

The role is a stable, idempotently-(re)created artifact — not dropped at `preview-down` (it owns the persisted preview DB). The `REVOKE CONNECT … FROM PUBLIC` on the dev DB is a documented durable side effect (rollback SQL in the `scripts/dev` header comment). Preview helpers are unit-tested in `scripts/test-dev` (pure URL/SQL shape + an optional live psql-gated containment check of both role-connect modes).

#### Merge-candidate tree for repository-content criteria (#1881)

After the identity gate passes and before the agent spawns, the runner also provisions a disposable **merge-candidate checkout** — a `git worktree add --detach` of the same `acceptance_expected_head_sha` the gate verified, taken against the operator's dispatch `working_dir` (the repo the lineage worktrees hang off), at the run/stage-keyed path `/tmp/fishhawk-acceptance-tree-<run>-<stage>` (mirrored byte-for-byte between the prompt's `AcceptanceTreePath` and the runner's `acceptanceTreePath`). "Read-only" is a prompt-directed convention the agent is instructed to honor, not a mechanically enforced property: the worktree shares the dispatch repo's git admin dir and object store, so it is not an isolated clone (this does not widen the ADR-050 containment boundary — credential-free env + no MCP token + egress proxy, never filesystem isolation). This is the ONE tree the acceptance prompt's Posture B names for repository-content criteria: the agent's own `WorkingDir` stays the fresh **empty** temp dir (diff-withholding is unchanged), so without this checkout a repository-local criterion would grep whatever tree it found on the host — the dispatch checkout or a lineage worktree, either of which `working_tree_restored` may have detached back to `main`, so a reference the PR deletes appears to remain and the criterion false-fails (`#1881`). The prompt hard-rule forbids evaluating a repository-content criterion against any other checkout and instructs the agent to mark the criterion `skipped` when the sanctioned checkout is absent. Provisioning is **warn-and-proceed**: an empty expectation, an unfetchable SHA, or a non-git dispatch dir emits `acceptance_tree_skipped` / `acceptance_tree_failed` and the stage continues (an honest skipped criterion beats a false `assertion_fail`, and preview-target criteria are unaffected). The checkout is torn down on **every** post-provision return (`git worktree remove --force`, with a `git worktree unlock` + `os.RemoveAll` + `git worktree prune` fallback that is proof against the macOS `/tmp`→`/private/tmp` symlink registration mismatch and, via the unlock, against a locked worktree entry a plain prune would skip), emitting `acceptance_tree_removed` or `acceptance_tree_teardown_failed`; teardown is best-effort and never changes the stage outcome.

### OTel trace export (#649 / #679)

`internal/otelemit` emits one OpenTelemetry GenAI trace per stage invocation. Emission is **gated by `OTEL_EXPORTER_OTLP_ENDPOINT`**: when unset (the default), `Bootstrap` returns a disabled Emitter whose methods are no-ops, so the implement loop is completely unaffected. When set, an OTLP/HTTP exporter (`otlptracehttp`) POSTs spans to `{endpoint}/v1/traces`, honouring the standard `OTEL_EXPORTER_OTLP_*` env vars.

Span shape (one trace per run, stitched under the deterministic `otelemit.TraceIDFromRunID` trace id across the separate per-stage runner processes):

- `stage <name>` — parent span; attrs `fishhawk.run_id`, `fishhawk.stage`. Span status records the stage outcome (Ok / Error).
- `chat <model>` — child model-call span; GenAI-semconv attrs `gen_ai.system=anthropic`, `gen_ai.operation.name=chat`, `gen_ai.request.model`, `gen_ai.usage.input_tokens` / `output_tokens`, optional `gen_ai.request.temperature`; plus `fishhawk.*` cost/repro attrs `cost.usd`, `cost.estimated`, `cost.priced`, `pricing.as_of`, `latency_ms`, `repro.temperature_available`.

To view traces locally, start the opt-in Jaeger all-in-one (`docker compose --profile otel up -d`), set `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318`, and open the Jaeger UI at http://localhost:16686. **Caveat**: the collector must be reachable from where the runner actually executes — under the standard dogfood loop the runner runs on a GitHub-hosted CI runner where `localhost:4318` is the CI host's loopback, so end-to-end local viewing requires invoking `fishhawk-runner` locally (see "Local invocation" above). Full span-attribute reference, the k8s Jaeger story (#895), and the GHA-export deferral are in `internal/otelemit/README.md`.

## Releases

The release workflow at `.github/workflows/runner-release.yml` triggers on tags matching `runner/v*`. To cut a release:

1. Land everything on `main`. Verify `golangci-lint run ./runner/...` and `go test -race ./runner/...` are clean.
2. Tag the release commit: `git tag runner/v0.1.0 && git push origin runner/v0.1.0`.
3. The workflow re-runs lint + tests at the tag, builds a `linux-amd64` binary with the version stamped via `-ldflags`, generates an SPDX-JSON SBOM (anchore/sbom-action), computes SHA-256 checksums, signs `SHA256SUMS` keyless via cosign + GitHub OIDC, and publishes a GitHub Release with all artifacts attached.
4. Update `docs/spec/examples/` (or any sample workflow) to point at the new tag if appropriate.

Verify a release locally:

```sh
# Download SHA256SUMS, SHA256SUMS.sig, SHA256SUMS.pem from the GitHub Release.
cosign verify-blob \
  --certificate-identity-regexp 'https://github.com/kuhlman-labs/fishhawk/\.github/workflows/runner-release\.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --signature SHA256SUMS.sig \
  --certificate SHA256SUMS.pem \
  SHA256SUMS
sha256sum -c SHA256SUMS
```

The verify-identity is the workflow file's path; that's the URL Fulcio embeds in the cert when keyless-signing from a GitHub Action.

## See also

- `docs/MVP_SPEC.md` §5.1.2 — runner component definition.
- `docs/MVP_SPEC.md` §5.3 — trust model (signing, supply-chain, ephemeral keys).
- `docs/ARCHITECTURE.md` §4 — workflow run lifecycle, where the runner sits in the dispatch flow.
