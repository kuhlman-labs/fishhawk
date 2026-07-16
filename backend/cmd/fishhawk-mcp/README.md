# fishhawk-mcp

Model Context Protocol server that exposes Fishhawk run / plan / audit state to Claude Code (and any other MCP-compatible client) per [ADR-021 / #322](https://github.com/kuhlman-labs/fishhawk/issues/322).

Two audiences share one surface:

- The **in-runner Claude Code agent** reads its own run's state mid-execution — what's the active plan, what audit entries fired for the current retry, what constraints apply. Closes the agent-is-blind-to-Fishhawk-state gap that motivated ADR-019.
- The **interactive Claude Code session** — an engineer asking "what's the status of my current run" — gets the answer through natural language without a CLI alt-tab.

The v0 surface began read-only; action verbs (approve, reject, retry, cancel, start, run_stage, the implement-review fix-up below, and the run-branch reset below) have since landed as scoped write tools so the loop can be driven end-to-end from the agent session. Write tools require an operator-side token with the matching `write:*` scope; a run-bound runner token is restricted to its own run.

## Status

E19.2 / #342 shipped scaffolding + handshake. E19.3–E19.6 landed the v0 tool surface (all read-only per ADR-021):

- `fishhawk_get_active_run` (E19.3 / #343) — the "which run" resolver: use it when you hold a `pr_number`, `trigger_ref`, or `FISHHAWK_RUN_ID` env but need the run UUID the other tools take.
- `fishhawk_get_plan` (E19.4 / #344) — read the approved standard_v1 plan artifact after the plan stage and before approve/reject; walks `parent_run_id` up to 8 levels for CI-retry chains. Carries the plan-gate results alongside the plan: `scope_precheck` (#658), `surface_sweep` (#763), and `test_sweep` (#942) — the last flags EXISTING test files the plan omitted (a stem-sibling test, existing tests in a package gaining a new test file, or a path-trigger rule's pinned test — `migration_walk`: a scoped `migrations/*.sql` requires `backend/internal/postgres/postgres_test.go`); judge before approving whether the changed behavior's tests live there, since the runner scope_drift-excludes edits to unscoped files. `surface_sweep` also carries `cross_slice_findings` (#1102): when a decomposition splits a lockstep pattern's member files across two or more distinct slices (e.g. a schema's canonical and mirror copies landing in different slices), each finding names the pattern and which slice owns which files — the inverse of the same-file-in-two-slices gate (#1062), where the fix is consolidating the seam into one slice rather than declaring the shared file twice, because completing a split seam otherwise needs a runtime scope amendment that can time out (#1035). Also carries `plan_warnings` (#1684): soft advisory strings from `plan.Warnings()` — notably a multi-slice decomposition where every sub_plan omits `depends_on` (if any slice forms a producer->consumer chain, all slices run in parallel in wave 0 and the consumer can fail typecheck against the not-yet-integrated symbol, the shape that wedged #1551's first attempt), plus a sub-plan `predicted_runtime_minutes` sum less than the parent's, or an expensive `test_strategy` gate paired with an under-budgeted `predicted_runtime_minutes`. Purely advisory — it never blocks approval — and omitted when no advisory fired.
- `fishhawk_get_run_status` (E19.5 / #345) — the agent's "where are we" query: bundles Run + ordered stages + recent audit (time-descending) into one call. Also carries `plan_review_status` + `implement_review_status` (`none`/`pending`/`complete`/`skipped`/`failed`) and `plan_stage_wait_status` + `implement_stage_wait_status` — plus `acceptance_stage_wait_status` when the workflow declares an acceptance stage (E31.9 / ADR-049), omitted otherwise — (`pending`/`running`/`succeeded`/`failed`/`cancelled`). The acceptance field tracks stage **execution**, not the verdict: a FAILED acceptance verdict leaves the stage `succeeded`, so read the verdict from the `acceptance_outcome_recorded` audit entry (surfaced through `next_actions`), never from the stage state. **Re-polling this tool is the authoritative way to reach a terminal review *or* stage-execution status (#879/#880)**: on a non-terminal status each carries a server-suggested `poll_interval_seconds` (15s for reviews, 30s for stage execution) — re-call on that cadence until the status goes terminal. See [Stage-execution wait contract](#stage-execution-wait-contract-adr-037-880). The run row also carries `run.concerns` when the run has **open** review concerns (#964): the open count, a `by_state` breakdown, and `items[]` with each concern's **stable id** — the primary addressing scheme for `fishhawk_fixup_stage`'s `concern_ids`. Drive-enabled runs (#1023) additionally get a top-level `drive_status` block: `auto_advanced` (`[{rule, from, to, parked?, ts}]`, oldest first — the transitions the backend advanced itself, distilled from `run_auto_advanced` audit entries; `parked` marks a runner_kind-`local` dispatch that recorded a ready-to-run next action instead), `next_action` (`{action, detail?, pr_url?}` — the distilled operator next step, e.g. `run_implement_stage` or `merge_pr`; omitted on terminal runs), and `derived_status` (`awaiting_merge` when every gate is resolved and required PR checks are green on an open PR — presentation-only, `run.state` stays `running`). The block is omitted entirely for non-drive runs. Decomposed parents (#1147) additionally get a `children_status` block (per-child live state + the fan-in `integration_phase`) — see [Decomposed-parent observability](#decomposed-parent-observability-children_status-1147). Runs with unresolved high-severity code-scanning (CodeQL/SAST) findings on the implement diff additionally get a top-level `security_findings` block (#1096): `[{number, rule_id, description?, severity, state?, path, start_line?, html_url?}]`, distilled from the newest `implement_security_findings` audit entry (the webhook records one idempotent entry per scan, floored on the latest fix-up, so the newest reflects current state). It is a **SEPARATE signal** from `run.concerns` — a finding is held by its own merge gate (`security_findings_unresolved`) and routed to its own fix-up pass, so it never consumes a design-concern budget. Omitted when the run has no findings (no scan yet, a clean scan, or a clean re-scan after a fix-up cleared them). Every run additionally carries a `next_actions` block (#1024) — see [Server-suggested next actions](#server-suggested-next-actions-next_actions-1024). Runs with cost data additionally get a best-effort, display-only `cache_efficiency` block (ADR-044 slice 3 / #1352): the run's prompt-cache `cache_read_ratio` (share of input served from cache), `reuse_factor` (re-reads per cache-write token), and `gross_read_savings_usd` / `write_penalty_usd` / `net_savings_usd`, plus a per-stage (`plan_review` / `implement_review` / `agent`) breakdown — derived from the `cost_recorded` audit ledger via `GET /v0/runs/{run_id}/cache-efficiency`. Omitted when the run has no cost data; it never gates a run. Runs with cost data also get a best-effort, display-only `cost` block (#1372): the run's `total_cost_usd` (its rolled estimated US-dollar cost), a per-stage (`agent` / `plan_review` / `implement_review`) `stages[].cost_usd` breakdown, and — when the run resolved to a merged PR (a `pr_merged` audit row exists and the run carries a PR URL) — a `merged_pr` rollup giving `cost_per_merged_pr_usd` (summed across every run sharing that PR URL) plus the contributing `run_count` — derived from the `cost_recorded` audit ledger via `GET /v0/runs/{run_id}/cost`. Omitted when the run has no cost data, and the `merged_pr` rollup is omitted unless the run resolved to a merged PR; like `cache_efficiency` it never gates a run. Runs that have crossed at least one human gate additionally get a best-effort, display-only `latency` block (#1702): a `gates[]` breakdown of the wall-clock time parked at each human gate — `plan_approval` (`plan_generated` → the first following `approval_submitted`), `implement_review_to_dispatch` (the latest `implement_reviewed` → the next dispatch, falling back to `pr_merged` when the workflow has no acceptance stage), and `checks_green_to_merge` (checks-green → `pr_merged`) — each with `opened_at` / `closed_at` / `wait_seconds` (clamped to 0 on clock skew), plus `total_wait_on_human_seconds` and the run's end-to-end `wall_clock_seconds` — derived from the run's audit-chain timestamps via `GET /v0/runs/{run_id}/latency`. A gate whose opening or closing marker is absent is omitted (partial rollup); the block is omitted entirely when no gate has resolved. Like `cost` it never gates a run. **Compact by default (#1727, extended #1749):** the heavy `issue_context` (issue body + all comments) and reviewer free-text prose (`implement_reviews[].free_form` + concern notes, the same prose in `plan_review_status`/`implement_review_status` `reviews[]`, and `recent_audit` review-payload `free_form` / issue-fetch `body`+`comments`) are omitted so the snapshot stays within the tool-result token budget. In addition (#1749) each `recent_audit` entry's verifier-only hash-chain fields (`entry_hash` + `prev_hash`) are dropped and any oversized payload string value is truncated with a marker pointing at `fishhawk_list_audit`, and the `cache_efficiency` per-stage breakdown (`stages[]`) collapses to the run-level rollup. Every operator-playbook field is retained (`next_actions`, all wait statuses, `run.concerns`, and each review's/audit entry's verdict/severity/category/concern keys). Four opt-in flags restore today's full shape, all default false: `include_issue_context: true` (issue payload), `include_review_prose: true` (reviewer free-text), `include_audit_hashes: true` (the `recent_audit` hash-chain fields **and** the untruncated payload values together — one flag, not split), and `include_cache_stages: true` (the `cache_efficiency` per-stage breakdown). `fishhawk_list_audit` remains the full verifier surface (its `entry_hash`/`prev_hash` are unaffected).
- `fishhawk_await_review` (#600) — OPTIONAL convenience block over that poll: blocks until a stage's review reaches a terminal state. Default timeout **360s** (recalibrated from 120s to exceed the measured 3.5–4.5min review latency and the 300s reviewer budget, #878), cap 600s. Never strands — it also resolves when the run itself goes terminal (ADR-036 #874). Idempotent/resumable: a timeout returns `pending` + the `poll_interval_seconds` hint; re-call to resume, or switch to `fishhawk_get_run_status` polling (the primary path).
- `fishhawk_await_audit` (#962) — the sequence-anchored await primitive: blocks until the next audit entry with the given `category` and sequence strictly greater than `since_sequence` lands, and returns that entry. The anchoring contract makes the wait race-free: an event that happens after another always has a strictly greater audit sequence, so "the review after the fix-up" is the `implement_reviewed` entry with sequence > the `fixup_pushed` entry's sequence — a stale pre-fix-up verdict can never satisfy the wait (the #894 class of stale-read race). Inputs `{run_id, category, categories (plural, OR-semantics), allow_unknown (default false), since_sequence (default 0), timeout_seconds (default 360, cap 600 — same clamp as await_review)}`. Provide `category` OR `categories` (or both — they union). **Fail-loud category validation (#1764):** an unknown or misspelled `category` (e.g. the runner-log event `scope_amendment_pending` instead of the audit category `scope_amendment_requested`) is rejected UP FRONT — no wait armed — naming the nearest known categories, so a wrong-surface string can never silently block the full timeout on an unsatisfiable wait; pass `allow_unknown: true` to await a category legitimately absent from the curated registry. **Multi-category OR:** `categories` resolves the wait on the FIRST entry (lowest sequence) matching ANY listed category past the anchor, in one call — the implement-stage wait can resolve across several anchors (e.g. `implement_reviewed` OR `fixup_pushed`) without a separate call each. Statuses: `found` (with `entry` + `latest_sequence`), `timeout` (gapless re-arm: re-call with `since_sequence` = the returned `latest_sequence`, == your shared anchor when nothing landed — the max re-arm across ALL requested categories, so no entry of any category can be skipped), `run_terminal` (the ADR-036 non-stranding backstop fired after one final anchored read — do not re-arm blindly). **Compact by default (#1727):** the returned entry's payload has reviewer `free_form` prose and issue-context `body`/`comments` stripped so it stays within the tool-result token budget, while verdict/severity/category keys are always retained; pass `include_review_prose: true` or `include_issue_context: true` to restore the full payload. `fishhawk_await_review` stays unchanged as the review-specific convenience; re-polling `fishhawk_get_run_status` remains the authoritative fallback (ADR-037).
- `fishhawk_runtime_calibration` (#470) — the calibration read tool: agents call it before writing a plan; wraps `GET /v0/calibration`.
- `fishhawk_list_audit` (E19.6 / #346) — use when you need the filtered or paginated audit trail (category, stage_id) rather than the recent slice — e.g. to read an `implement_reviewed` concern's full note text. Mirrors the CLI's `fishhawk audit list`. (For fix-up addressing, prefer the stable concern IDs on `run.concerns` over audit-entry indices, #964.)
- `fishhawk_list_runs` (E22.5 / #394) — the "what runs do I have" enumeration: filter by `repo` / `workflow_id` / `state`, walk pages via the opaque `cursor`. Mirrors the CLI's `fishhawk run list`. **Compact by default (#1098):** each run's `issue_context` (issue body + every comment) is omitted from the list response so a single `list_runs` over issues with large bodies/comment threads stays within the tool-result token cap — the overflow that forced a `curl`+`jq` fallback when enumerating child run IDs during decomposition fan-out. Pass `include_issue_context: true` to re-include the full payload when it is actually needed. (`fishhawk_get_active_run` / `fishhawk_get_run_status` resolve a single run and are unaffected.)
- `fishhawk_file_issue` ([#1005](https://github.com/kuhlman-labs/fishhawk/issues/1005)) — file a work item (issue, bug, chore, ADR) through the repo's work-management conventions. The consistent cross-repo/cross-platform filing surface and the operator-agent follow-up-filing path ([ADR-040](https://github.com/kuhlman-labs/fishhawk/issues/1004)). See [Work-item filing](#work-item-filing-fishhawk_file_issue-1005).
- `fishhawk_draft_epic` ([E34.4 / ADR-052 / #1595](https://github.com/kuhlman-labs/fishhawk/issues/1595)) — the single operator surface over the E34 refinement loop: turn a natural-language **brief** into a structured epic + children, gated behind a preview + approval step before anything files. **One tool, five mutually-exclusive arms** (each 1:1 onto an E34.2/E34.3 endpoint): **open** (`brief`), **preview** (`session_id` alone), **edit** (`session_id` + `brief_amendment` \| `draft`), **decide** (`session_id` + `decision` + `reason`), **file** (`session_id` + `repo`). approve and file are **arms on this tool**, not `fishhawk_approve_plan` (which is stage-gated — a refinement session is neither a run nor a stage; the E31.9 reuse-first precedent). Arm dispatch **fails closed with no HTTP call** on zero arms or an illegal combination, and every result carries a `session_guidance` block naming the exact next arm for the derived state (`awaiting_approval` → decide, `rejected` → re-draft, `approved` → file, `drifted` → re-decide, `filed` → terminal). A **write** tool requiring `write:approvals` — **no new scope** (the E34.2 precedent). See the runbook's "Refinement intake loop" section.
- `fishhawk_report_product_issue` ([#1006](https://github.com/kuhlman-labs/fishhawk/issues/1006)) — file an upstream Fishhawk **product** bug/feature carrying an auto-collected, redacted, fingerprint-deduped diagnostic bundle. The first **write** tool that drives an egress on the run's chain. See [Product feedback](#product-feedback-fishhawk_report_product_issue-1006).
- `fishhawk_consolidate_slices` ([E24.2 / ADR-041 / #1238](https://github.com/kuhlman-labs/fishhawk/issues/1238)) — run the decomposed-parent fan-in on demand when a parent is stuck in `awaiting_children` after its children all succeeded on the **local** runner (the 60s sweeper backstop is off by default there). See [Local decomposition fan-in](#local-decomposition-fan-in-fishhawk_consolidate_slices-1238).
- `fishhawk_decide_scope_completeness` ([E22.X / #1231](https://github.com/kuhlman-labs/fishhawk/issues/1231)) — resolve an implement stage parked in `awaiting_scope_decision`: **exempt** the already-committed tree (open the PR from the held commit with **no agent re-run**) or **fail** it to category-B. The zero-re-run recovery for a missing-declared-scope-file-only gate failure. See [Scope-completeness park](#scope-completeness-park-fishhawk_decide_scope_completeness-1231).
- `fishhawk_approve_deploy` / `fishhawk_reject_deploy` ([E23.15 / #1432](https://github.com/kuhlman-labs/fishhawk/issues/1432)) — the deploy-gate counterparts to `fishhawk_approve_plan` / `fishhawk_reject_plan`. Use them when a release run's deploy stage is parked at `awaiting_deploy_approval` (the `next_actions` deploy arm points here): `fishhawk_approve_plan` fails on a plan-less release run because it resolves a `type=plan` stage first. Both take a **run id** and resolve the `type=deploy` stage internally. **`fishhawk_approve_deploy` requires an operator token with `write:deploy`** (ADR-038/#1390) **and a required `environment`** that is one of the deploy stage's `allowed_environments` — composed into the approval comment as `--environment=<env>`, which the backend deploy pre-flight parses; an optional `override_freeze` flag appends `--override-freeze` to permit a deploy during a spec-declared `change_freeze`. `fishhawk_reject_deploy` routes through `advanceStage` (not the approve-only pre-flight), so it needs neither `write:deploy` nor an environment. See [Deploy-gate approval](#deploy-gate-approval-fishhawk_approve_deploy-fishhawk_reject_deploy-1432).
- `fishhawk_start_campaign` / `fishhawk_get_campaign_status` / `fishhawk_resume_campaign` ([E25.8 / ADR-047 / #1447](https://github.com/kuhlman-labs/fishhawk/issues/1447)) — the campaign-driving surface, thin MCP wrappers over the E25.4 REST endpoints (`POST /v0/campaigns`, `GET /v0/campaigns/{id}/status`, `POST /v0/campaigns/{id}/resume`). **`fishhawk_start_campaign`** assembles a campaign from an epic ref (`repo` + `epic_ref` required, optional `pause_policy` of `pause_campaign`/`pause_item`) — the campaign counterpart to `fishhawk_start_run`; a dangling dependency edge fails `campaign_dangling_dependency` and an un-installed repo fails `repo_not_installed`. **`fishhawk_get_campaign_status`** is the operator-agent's drive-tick read (the campaign analogue of `fishhawk_get_run_status`): it returns the campaign + items + the readiness `rollup` (`eligible`/`blocked`/`running`/`done`/`failed`/`cancelled`/`paused`), the server-computed `next_action`, and a `next_actions` block mapping that action onto a legal operator move — `attention` → read the failed item's run and retry/abandon (`fishhawk_get_run_status`, `consumes:none`), `resume` → `fishhawk_resume_campaign` (`consumes:none`), `start_run` → `fishhawk_start_run` on the eligible item's `trigger_ref` (`consumes:new_run`), `wait` → re-poll `fishhawk_get_campaign_status` (`consumes:none`), `complete` → terminal (no actions); any unrecognized action lands in a `campaign_unclassified` fallback (re-poll + `file_product_issue`) so the block is never empty for a non-complete campaign. **`fishhawk_resume_campaign`** is the E25.7 hand-back: once you have handled a paged gate it flips the paused campaign and every paused item back to running (the campaign counterpart to `fishhawk_resume_run`); a campaign with nothing paused returns `campaign_not_paused`. `fishhawk_start_campaign` and `fishhawk_resume_campaign` are **write tools** needing an operator token with **`write:campaigns`** scope; `fishhawk_get_campaign_status` is read-only.
- `fishhawk_start_campaign_item_run` ([E26.2 / #1481](https://github.com/kuhlman-labs/fishhawk/issues/1481)) — start a run for **one eligible campaign item** and link it to the campaign, the operator-driven counterpart to the backend auto-driver's START pass. A thin wrapper over `POST /v0/campaigns/{id}/runs`. Call it when `fishhawk_get_campaign_status`'s `next_action` is `start_run`: it refuses unless the item is eligible per the DAG (`item_not_eligible`, naming the blocking dependency), then mints the run (pass **`runner_kind: local`** for the local dogfood loop), links it to the item, and moves the item to `running`. Pairs with `fishhawk_get_campaign_status`, which is reconcile-on-read — the status poll settles each run as it reaches terminal and advances the campaign in DAG order, so you drive a whole campaign locally with no auto-driver and no GitHub Actions. A **write tool** needing `write:campaigns`; an unknown `issue_ref` fails `campaign_item_not_found` and a paused/terminal campaign fails `campaign_not_startable`. There is deliberately no `idempotency_key` — the backend does not dedup this start, and the eligibility gate already refuses a re-start against an already-running item.
- `fishhawk_doctor` ([E29.6 / #1506](https://github.com/kuhlman-labs/fishhawk/issues/1506)) — the in-band first-run **readiness** report, the counterpart to the CLI `fishhawk doctor` (E29.4/E29.5). Read-only; wraps `GET /v0/onboarding/readiness` and returns the four server-side-only checks a repo's first `feature_change` run needs — `app` (GitHub App installed?), `spec` (committed `.fishhawk/workflows.yaml` fetch/parse/validate state), `reviewers[]` (per spec-declared reviewer availability on this deployment, with a `missing_hint` env-var pointer when a provider can't be resolved), and `scopes` (caller-token run-driving scope adequacy). `repo` falls back to `GITHUB_REPOSITORY` when omitted. The endpoint gates on **authentication only**, so a token with a scope gap still gets a report naming its gap. Pairs with `fishhawk_init`. See [Onboarding tools](#onboarding-tools-fishhawk_doctor--fishhawk_init-1506).
- `fishhawk_init` ([E29.6 / #1506](https://github.com/kuhlman-labs/fishhawk/issues/1506)) — the in-band starter-**scaffold** generator, the counterpart to the CLI `fishhawk init`. Returns the canonical workflow-v1 preset spec bytes for the chosen autonomy tier (`low` / `medium` / `high`, default `medium`), generated **in-process** from the backend's embedded preset library via `spec.PresetBytes` — there is no HTTP generation endpoint. **Preset-only:** it returns the scaffold bytes for the conversational agent to write to `.fishhawk/workflows.yaml`; it writes no file itself, and the delta options + the AGENTS.md/CLAUDE.md bridge the CLI performs are a follow-up (the delta-applying generator lives only in `cli/internal/spec`). See [Onboarding tools](#onboarding-tools-fishhawk_doctor--fishhawk_init-1506).
- `fishhawk_release_notes` ([E33.5 / ADR-051 / #1590](https://github.com/kuhlman-labs/fishhawk/issues/1590)) — the single operator surface over the E33.2 release-notes endpoints for the delegating `release` workflow: **one tool, two modes.** `mode: "preview"` (default) renders the notes markdown for the `from`/`to` ref range **without persisting** — read-only; `mode: "prepare"` renders **and** persists a `release_notes` artifact keyed to `stage_id` (required for `prepare`), the artifact the cut and publish verbs consume. `repo` falls back to `GITHUB_REPOSITORY`. The rendered markdown carries the advisory **semver bump hint** (E33.4). Reach for it when `fishhawk_get_run_status`'s `next_actions` reports a `notes_ready` (prepare) or `awaiting_cut` (preview) release-loop state. The **cut** and **publish** steps are **CLI verbs** (`fishhawk release cut` / `fishhawk release publish` over `/v0/releases/cut` and `/v0/releases/publish`), not MCP tools — `next_actions` names them at the `awaiting_cut` / `awaiting_publish` states — so the MCP surface grows by exactly one tool. Preview is an authenticated read (401 anonymous); prepare is a write additionally needing `write:runs` (403 without). The tag push between cut and the release pipeline stays a **human git action**. See [docs/deploy/release-loop.md](../../../docs/deploy/release-loop.md).
- `fishhawk_drive_run` ([E22.X / ADR-040 / #1700](https://github.com/kuhlman-labs/fishhawk/issues/1700)) — the **local auto-driver**: executes every mechanical operator step between human gates on a `runner_kind:local` run under ADR-040 delegation, and stops at the first genuine decision. The local sibling of the GHA campaign auto-driver. A **write** tool needing `write:approvals`. See [Local auto-driver](#local-auto-driver-fishhawk_drive_run-1700).
- `fishhawk_merge_run` ([E48.7 / #1954](https://github.com/kuhlman-labs/fishhawk/issues/1954)) — the **one-verb operator merge**: takes a gate-approved run from verdict to merged+terminal, replacing the four-step hand ceremony (approve → merge → post-merge). Records your operator merge verdict as a chained `merge_verdict_recorded` audit entry, queues the squash merge through the **same `GitHubMerger` seam** `drive_run`'s delegated `may_merge` arm uses, awaits the webhook-settled terminal run state (the `pr_merged` / `post_merge_observed` categories + the ADR-036 run-terminal backstop — there is no persisted `merged` state), and surfaces the operator post-merge dev-host step (surfaced, not invoked — ADR-038). The endpoint is **idempotent** (#1954): a repeated POST finds the existing verdict row, appends no duplicate (`already_recorded:true`), and STILL re-dispatches the merge — so a timed-out re-invoke or a 502 retry re-queues with no duplicate row. The PR-approval review itself STAYS a `gh` step under your own GitHub identity (App-identity approval deferred to E39). A **write** tool needing `write:approvals`. See [One-verb operator merge](#one-verb-operator-merge-fishhawk_merge_run-1954).

Lifecycle-write provenance (E22 / #389): `fishhawk_start_run` (E22.1 / #390), `fishhawk_cancel_run` (E22.2 / #391),
`fishhawk_retry_stage` (E22.3 / #392), `fishhawk_approve_plan` + `fishhawk_reject_plan` (E22.4 / #393),
`fishhawk_list_runs` (E22.5 / #394), `fishhawk_fixup_stage` (E22.X / #762), `fishhawk_revise_plan` (E22.X / #1099),
`fishhawk_resume_run` (E22.X / #978), `fishhawk_revive_run` (E22.X / #1915). Phase A closed with E22.6 (docs sweep + E2E test extension).

E19.7 / #347 wires the binary into the release pipeline next.

## Build

From the repo root (workspace-aware):

```sh
go build ./backend/cmd/fishhawk-mcp/...
```

The binary lands at `./fishhawk-mcp` when built explicitly:

```sh
go build -o fishhawk-mcp ./backend/cmd/fishhawk-mcp
```

## Configuration

Two env vars; both honored from the OS environment when the binary launches.

| Variable                | Required | Default                 | Notes                                                                                                                                                                                                  |
| ----------------------- | -------- | ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `FISHHAWK_API_TOKEN`    | **yes**  | —                       | Bearer token. Generated via the backend's API-token surface. No "anonymous" mode — unlike the CLI's dev path, MCP tools always round-trip the API and running unauthenticated would be a silent permission bug. |
| `FISHHAWK_BACKEND_URL`  | no       | `http://localhost:8080` | Same fallback as the CLI. Trailing slash is stripped.                                                                                                                                                  |

The binary exits non-zero on startup if `FISHHAWK_API_TOKEN` is missing.

## Transport (`--transport` / `--addr`)

Two transports, selected by flag. **stdio is the default and unchanged** — every existing per-client subprocess consumer (Claude Code, Codex) keeps working with no flags.

| Flag | Default | Notes |
| --- | --- | --- |
| `--transport` | `stdio` | `stdio` \| `http`. `http` is the opt-in [ADR-033](https://github.com/kuhlman-labs/fishhawk/issues/843) option-b streamable-HTTP transport ([#927](https://github.com/kuhlman-labs/fishhawk/issues/927)). |
| `--addr` | `127.0.0.1:8765` | `host:port` for `--transport http`; ignored for stdio. **Loopback-only** — see below. A bind collision surfaces as an operator-visible error. |

```sh
fishhawk-mcp --transport http --addr 127.0.0.1:8765
```

The HTTP transport is a **single-operator local shared endpoint, NOT multi-tenant** — a hosted/remote MCP server is [#655](https://github.com/kuhlman-labs/fishhawk/issues/655), out of scope here. Two enforcements back that posture:

- **Loopback-only bind.** `--addr` is validated before any bind: a literal IP must be loopback (`127.0.0.0/8` or `::1`), an empty host clamps to `127.0.0.1`, and a hostname is rejected unless **every** resolved IP is loopback (so `localhost` aliased to a routable address can't slip through). `0.0.0.0` and any routable address fail fast with a precise error.
- **Per-request bearer.** Every request must carry `Authorization: Bearer <FISHHAWK_API_TOKEN>`, compared in constant time. A missing/malformed/mismatched header gets `401` with `WWW-Authenticate: Bearer`. Loopback is explicitly **not** a trust boundary — co-tenant local processes still need the token.

The go-sdk's own DNS-rebinding protection (rejecting a non-loopback `Host` header) stays enabled; the loopback bind + bearer gate are independent of it. Tool registration is identical across both transports.

## In-band onboarding (server `instructions` + `fishhawk://runbook`, [#1356](https://github.com/kuhlman-labs/fishhawk/issues/1356))

A connecting client whose agent holds no operator memory gets enough to drive a run without a CLI alt-tab, delivered over the protocol itself:

- **Non-empty server `instructions`** — returned on every MCP `initialize`. A concise happy-path verb sequence (`fishhawk_start_run` → `fishhawk_run_stage` plan → `fishhawk_approve_plan` → `fishhawk_dispatch_stage` implement → `fishhawk_await_review` → the acceptance stage when declared → approve PR → merge → post-merge) plus the gate semantics that decide when each verb is legal (don't approve before plan review clears, wait for all configured reviewers, operator-gated scope amendments, a failed acceptance verdict leaves the stage succeeded and routes through deterministic triage, `next_actions` is authoritative). Kept deliberately short; the long form lives in the runbook resource it points at.
- **`fishhawk://runbook` resource** — a listable/readable `text/markdown` resource carrying the full loop-driving procedure (the ADR-040 operator-role contract) and the edge-case playbook: `runner_kind:local` for the local dogfood loop, failed-run revive (`fishhawk_revive_run`, incl. the re-parked-acceptance pre-dispatch check), the decomposed-parent native path (`fishhawk_run_children` → `fishhawk_consolidate_slices`, never `fishhawk_dispatch_stage` on an `awaiting_children` parent), the `fishhawk_drive_run` loop shape (gate-ordered dispatch, delegated plan approval, `awaiting_host_dispatch` auto-dispatch, and its `decision_required`/`paged`/`dispatched_stale` stops), the acceptance stage (E31.9 — advisory runner-hosted validator against a preview you provision; verdict-vs-stage-state; deterministic triage table; the local-runner explicit-re-dispatch rule; paged arbitration), local-drive fixup requiring an explicit `fishhawk_dispatch_stage` to spawn the runner, the scope-amendment decide/naming flow, heterogeneous-review two-verdict waits, and post-failure clean-tree discipline.

Both register in the single shared `newServer` construction path (`onboarding.go`, content in `runbook.md`), so they are **transport-neutral** — identical over stdio and streamable-HTTP, and they carry into the #655 gateway unchanged.

Implementation: `onboardingInstructions` is wired into `buildServer`'s `mcp.ServerOptions{Instructions: …}` so it is
returned verbatim on every `initialize`; `registerOnboardingResources(srv)` adds the readable `fishhawk://runbook`
`text/markdown` resource. The in-memory round-trip in `onboarding_test.go` and the HTTP-session assertion in
`http_transport_test.go` pin both seams. This is the in-band counterpart to #996 Themes 2/3 (the thin operator agent +
onboarding-as-data).

## Onboarding tools (`fishhawk_doctor` / `fishhawk_init`, [#1506](https://github.com/kuhlman-labs/fishhawk/issues/1506))

Two thin tools (E29.6) wrap the E29 onboarding engine so a connecting Claude Code agent can drive a conversational "help me onboard a repo" flow — **one engine, another frontend** (the CLI `fishhawk doctor` / `fishhawk init` and the App-PR path are the other frontends). Both live in `onboard.go`.

- **`fishhawk_doctor`** (read-only) wraps `GET /v0/onboarding/readiness` (E29.4 / [#1511](https://github.com/kuhlman-labs/fishhawk/issues/1511)) and returns the `report` — the four server-side-only readiness checks a repo's first `feature_change` run needs, which the agent cannot introspect locally:
  - `app` — `{installed, installation_id?, reason?}`: is the GitHub App installed on the target repo.
  - `spec` — `{source, valid, error?, note?}`: the committed `.fishhawk/workflows.yaml` fetch + parse + validate state (`source` is `fetched` or `unavailable`; `valid` is only meaningful when fetched). Only checked once the app is installed.
  - `reviewers[]` — `{provider, model?, reasoning_effort?, available, missing_hint?}`: per spec-declared reviewer availability on **this** deployment, with the adapter's missing-env-var hint when a provider can't be resolved. Empty when the spec is unavailable or invalid.
  - `scopes` — `{adequate, required[], missing[], note?}`: whether the caller token holds the run-driving scope subset. A cookie-session caller bypasses scope enforcement and is adequate by construction.

  `repo` falls back to `GITHUB_REPOSITORY` when omitted (a fast local fail when neither is present, before the HTTP hop). The endpoint gates on **authentication only** (401 anonymous) — scope adequacy is itself a reported field, so a scope-gapped token still gets a report naming its gap rather than a 403. Backend 4xx map onto clean tool errors: `authentication_required` (401, with a `FISHHAWK_API_TOKEN` pointer) and `validation_failed` (400, malformed repo).

- **`fishhawk_init`** generates the starter spec **in-process** via `backend/internal/spec.PresetBytes(preset)` — there is **no HTTP generation endpoint** (spec generation is CLI-local `spec.Generate`), and the `fishhawk-mcp` binary is built from the backend module (ADR-021) so it may import `backend/internal/spec` directly (it already does for spec parsing). Returns `{preset, workflow_yaml, target_path}` where `preset` is `low` / `medium` / `high` (default `medium`), `workflow_yaml` is the canonical workflow-v1 preset bytes, and `target_path` is `.fishhawk/workflows.yaml`. An unknown preset fails closed with a clean error naming the valid tiers.

  **Preset-only scoping:** `fishhawk_init` returns the scaffold bytes for the conversational agent to **write** — it writes no file itself. The delta options (`--budget-usd` / `--single-reviewer` / `--human-gates`) and the AGENTS.md/CLAUDE.md bridge (E29.2) the CLI `fishhawk init` performs are a **follow-up**, because the delta-applying `Generate` lives only in `cli/internal/spec` and porting it into the backend module is beyond a thin tool.

Wiring: the `OnboardingReadinessReport` wire mirror (+ nested `OnboardingApp`/`OnboardingSpec`/`OnboardingReviewer`/`OnboardingScopes`)
lives in `client.go` — all scalar/string/slice fields, so it is #371-safe. Both tools register in `tools.go`, bumping the
house-style tool-count guard to 39; tests live in `onboard_test.go` (the low `client_test.go` stem-sibling needs no new
coverage). The planned `.claude/skills/onboarding/SKILL.md` conversational-entry seed is DEFERRED — `.claude/` is
gitignored repo-wide, so the skill file cannot be committed; the onboarding frontend ships in full via the two tools
regardless, and the skill is a follow-up if the repo later tracks `.claude/`.

## Install (operators)

Pre-built binaries ship with every `mcp/vX.Y.Z` GitHub Release: darwin-arm64, darwin-amd64, linux-amd64, linux-arm64. Full install path including cosign verification and `claude mcp add` registration lives at [`docs/mcp/install.md`](../../../docs/mcp/install.md).

Short version for operators on Apple Silicon Macs:

```sh
curl -fSL "https://github.com/kuhlman-labs/fishhawk/releases/download/mcp/vX.Y.Z/fishhawk-mcp-vX.Y.Z-darwin-arm64" \
  -o /usr/local/bin/fishhawk-mcp
chmod +x /usr/local/bin/fishhawk-mcp
export FISHHAWK_API_TOKEN="<token>"
claude mcp add fishhawk \
  -e FISHHAWK_API_TOKEN=$FISHHAWK_API_TOKEN \
  -- /usr/local/bin/fishhawk-mcp
```

## Surviving rebuilds (`fishhawk-mcp-shim`)

Claude Code does not reconnect a restarted stdio MCP server, so a local `scripts/dev reload` that rebuilds this binary leaves the live session on the old code until the operator runs `/mcp` by hand. To survive rebuilds under a live session, register the [`fishhawk-mcp-shim`](../fishhawk-mcp-shim/) supervisor instead of this binary directly: it spawns `fishhawk-mcp` as a child, watches the child binary for a content change, and hot-swaps it in place (replaying the handshake and synthesizing `notifications/tools/list_changed`) with no manual reconnect. Details, flags, and the accepted residuals are in the [shim README](../fishhawk-mcp-shim/README.md) (ADR-060 / #1921).

The `scripts/dev` wiring is now live (#1922): `up`/`reload` rebuild `bin/fishhawk-mcp-shim` and, when the shim is the registered MCP server, replace the manual `/mcp` reconnect nag with a one-line auto-swap note (the shim refreshes the rebuilt child automatically; verify via a version-returning tool call reflecting the new GitSHA). This requires the **one-time operator re-registration** step — re-point the harness's `fishhawk` MCP entry at `bin/fishhawk-mcp-shim` — documented in the [shim README's Registration section](../fishhawk-mcp-shim/README.md#registration). Without that re-registration the legacy manual-reconnect banner still fires on a rebuild.

## Release pipeline

`.github/workflows/mcp-release.yml` (E19.7 / #347) — triggered by `mcp/v*` tags. Re-runs lint + tests at the tag commit, cross-builds the four-platform matrix with CGO disabled, generates an SPDX-JSON SBOM, signs `SHA256SUMS` with cosign keyless, publishes the GitHub Release.

Verification (after `cosign install`):

```sh
cosign verify-blob \
  --certificate-identity-regexp 'https://github.com/kuhlman-labs/fishhawk/\.github/workflows/mcp-release\.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --signature SHA256SUMS.sig \
  --certificate SHA256SUMS.pem \
  SHA256SUMS
```

## Stage-execution wait contract ([ADR-037](https://github.com/kuhlman-labs/fishhawk/issues/879), #880)

The durable `(run_id, stage_id)` handle is the unit of waiting on a stage's execution. `fishhawk_get_run_status` carries `plan_stage_wait_status` + `implement_stage_wait_status` — each a `StageWaitStatus` whose `status` is one of `pending`/`running`/`succeeded`/`failed`/`cancelled`, derived from the stage row (distinct from the `*_review_status` pair, which tracks a stage's **review** rather than its execution).

- **Poll the handle (primary, authoritative).** Re-polling `fishhawk_get_run_status` is the blessed way to await a stage's terminal status. While the status is non-terminal (`pending`/`running`) the `StageWaitStatus` carries a server-suggested `poll_interval_seconds` of **30s** — coarser than reviews' 15s because stages run minutes, not seconds. Re-call on that cadence until the status goes terminal. The interval is dropped once the run itself is terminal (ADR-036 [#874](https://github.com/kuhlman-labs/fishhawk/issues/874) backstop), so a stage that can no longer progress never advertises an unbounded poll.
- **Synchronous-with-progress `fishhawk_run_stage` (negotiated fallback).** The synchronous call runs the stage to completion and returns the terminal outcome (also surfacing `stage_wait_status` on the handle — normally already terminal, so the interval is omitted). It is the fallback for clients that prefer to block or for short stages; it is not the primary mechanism. Its run-terminal result also carries the `next_actions` block (#1024) so the operator gets the legal next move directly — see [Server-suggested next actions](#server-suggested-next-actions-next_actions-1024).
- **Non-blocking `fishhawk_dispatch_stage` ([#1232](https://github.com/kuhlman-labs/fishhawk/issues/1232)).** The SDK-independent dispatch verb spawns the runner **detached** and returns the `(run_id, stage_id)` handle plus a non-terminal `stage_wait_status` **immediately**, so a **single** MCP session can poll `fishhawk_get_run_status` to terminal AND decide a mid-stage scope amendment in-band between polls (`fishhawk_decide_scope_amendment`) — the durable fix for the [#1189](https://github.com/kuhlman-labs/fishhawk/issues/1189) amendment timeout. It ships the poll-to-terminal UX today and **superseded the interim `fishhawk run auto-decide` second channel** ([#1233](https://github.com/kuhlman-labs/fishhawk/issues/1233)/[#1234](https://github.com/kuhlman-labs/fishhawk/issues/1234)) for in-band mid-stage amendment decisions, since removed ([#1554](https://github.com/kuhlman-labs/fishhawk/issues/1554)). See [Non-blocking dispatch](#non-blocking-dispatch-fishhawk_dispatch_stage-1232) below.
- **Native MCP Tasks (`invocationMode: async`) — deferred.** A future mode that lets `fishhawk_run_stage` return a handle immediately and poll to terminal is **not built** here: it is gated on [ADR-033](https://github.com/kuhlman-labs/fishhawk/issues/843) transport plus MCP Tasks leaving experimental (ADR-037 two-phase delivery). It would be a later transport refinement layering onto the same `(run_id, stage_id)` handle that `fishhawk_dispatch_stage` already returns.

## Progress notifications (`fishhawk_run_stage`)

`fishhawk_run_stage` spawns the runner and relays its stderr JSONL lines as MCP `notifications/progress` updates — but **only when the client supplied a `progressToken`** on the call (the MCP opt-in progress model; without a token the runner's events are still returned post-hoc in the final result's `events` list).

While the agent runs, the runner emits a `stage_progress` heartbeat (~every 15s, see [runner/README.md](../../../runner/README.md#progress-heartbeats-580)). The relay renders it into the notification's message:

    stage_progress turns=7 tokens=13402 elapsed=42s last=assistant

Because the cadence is time-driven, a stalled stage keeps producing heartbeats with non-advancing `turns`/`tokens`, so a watching operator/client can tell a progressing stage from a stuck one. Note this is a signal for the **operator/client watching the run**, not a live early-cancel channel for the synchronously-blocked driving agent — that agent sees the heartbeats only after `fishhawk_run_stage` returns (and as groundwork for a future async run_stage).

### Compact-by-default result (#647)

The final tool result is **compact by default**: the routine `stage_progress` heartbeats are dropped from the `events` list, while every non-heartbeat event — `runner_completed`, `git_diff`, `runner_cancelled`, etc. — is retained in arrival order alongside `stage_state` and the best-effort enrichment fields. The heartbeats' signal is preserved in five scalar summary fields distilled from the stream:

| Field | Source |
|---|---|
| `outcome` | terminal `runner_completed` event (`ok` \| `failed`) |
| `tokens_used` | `runner_completed` when present, else the last heartbeat's `tokens_so_far` |
| `turns` / `elapsed_seconds` / `last_event_kind` | the last `stage_progress` heartbeat |

This roughly halves the driving agent's per-stage context cost without losing any durable signal — the audit log and signed trace bundle are unchanged. Pass `verbose: true` on the input to restore the full event list including every heartbeat (e.g. a driver that wants to inspect per-heartbeat progression).

## Non-blocking dispatch (`fishhawk_dispatch_stage`, [#1232](https://github.com/kuhlman-labs/fishhawk/issues/1232))

`fishhawk_dispatch_stage` is the **non-blocking sibling** of `fishhawk_run_stage`. Where `run_stage` blocks to terminal and returns the full event list, `dispatch_stage` spawns the same `fishhawk-runner` subprocess **detached** and returns the durable `(run_id, stage_id)` handle plus a (normally non-terminal) `stage_wait_status` **immediately**. It reuses `run_stage`'s input validation, stage-id resolution, runner-binary resolution, repo detection, and argv composition (the shared `composeRunnerArgv`, so the spawned argv is byte-identical) — the only difference is the spawn mode.

The flow it enables (the [#1189](https://github.com/kuhlman-labs/fishhawk/issues/1189) in-band amendment fix, ADR-037 poll-to-terminal half):

1. `fishhawk_dispatch_stage --stage implement …` — returns the handle now.
2. Poll `fishhawk_get_run_status` on the advertised `poll_interval_seconds` (30s) until the stage's `*_stage_wait_status` goes terminal.
3. **Between polls**, when a `scope_amendment_pending` surfaces, call `fishhawk_decide_scope_amendment` — so the runner's amendment `?wait` poll resolves **before its window elapses**, with no failed-stage retry.

This is what a **single** MCP session needs: a blocking `fishhawk_run_stage` call cannot decide an amendment the same agent's runner files mid-stage. `fishhawk_dispatch_stage` **superseded the interim `fishhawk run auto-decide` second channel** ([#1233](https://github.com/kuhlman-labs/fishhawk/issues/1233)/[#1234](https://github.com/kuhlman-labs/fishhawk/issues/1234)) for that decision, since removed ([#1554](https://github.com/kuhlman-labs/fishhawk/issues/1554)).

Detached-spawn properties (differ deliberately from the synchronous `spawnRunnerStage`):

- **Own process group** (`Setpgid`): a `SIGINT`/`SIGTERM` to the MCP server's foreground group is **not** forwarded to the runner — it is meant to outlive the tool call. There is **no** SIGTERM→grace→SIGKILL watcher.
- **Output → a per-invocation log file** under `os.TempDir()` (`fishhawk-runner-<run>-<stage>-<unixnano>.log`), **never a pipe**: an unread pipe fills its kernel buffer and blocks the writer once full (#446). The runner ships its trace via `--upload-trace` and its state to the backend, so the local log is a diagnostic only. `log_path` is returned for that diagnostic.
- **A reaper goroutine** (`go func(){ _ = cmd.Wait() }()`) collects the child's exit so it never zombies while the tool returns.

Restarting the MCP server (`scripts/dev reload`) while a detached stage is in flight **orphans** the runner (reparented to init) but it continues to terminal and stays pollable via `fishhawk_get_run_status` — the intended durability of the `(run_id, stage_id)` handle (ADR-037), not a regression. Requires the `fishhawk-runner` binary to resolve on the MCP host, exactly like `fishhawk_run_stage`.

### Sibling-in-flight dispatch refusal ([#1872](https://github.com/kuhlman-labs/fishhawk/issues/1872))

Both host-spawn verbs — `fishhawk_dispatch_stage` and `fishhawk_run_stage` — refuse to spawn a runner while **another stage of the same run is still executing**. Concretely, the dispatch is **blocked** (a non-nil tool error, **zero** runners spawned) when any stage OTHER than the target is `dispatched` or `running`, or when the **target stage itself is `running`** (a live runner already owns it — a second spawn would double-drive it). A sibling parked at **`awaiting_host_dispatch`** ([#1912](https://github.com/kuhlman-labs/fishhawk/issues/1912)) is **NOT** in-flight (no spawn attempt exists yet) and does **not** block — only `{dispatched, running}` siblings do. Dispatching a sibling stage while an implement runner is still in its ship phase (which spends its whole duration `running`) rotates the run's signing key out from under the in-flight runner; the block prevents that contention at admission time. The target stage's own **park** states — `awaiting_host_dispatch` (the plan-approved / retry / fixup local park, #1912) and the legacy/transitional `dispatched` (dead-runner re-dispatch) — are **allowed**, since blocking them would wedge every local dispatch. A stage-list read error **fails open** (a warning, the dispatch proceeds) — the backend's any-unexpired-key signature verify (#1872) is the correctness backstop. The refusal names the in-flight stage's type and state and tells you to wait for it to settle (the implement ship phase ends when its pull-request artifact upload lands).

### Acceptance-dispatch admission ([#1928](https://github.com/kuhlman-labs/fishhawk/issues/1928))

All three host-dispatch verbs — `fishhawk_dispatch_stage`, `fishhawk_run_stage`, and `fishhawk_drive_run` — call `POST /v0/stages/{stage_id}/acceptance-admission` for an **acceptance** stage BEFORE recording spawn evidence or spawning a runner. The backend evaluates the approved plan's three disjoint short-circuit predicates (out-of-scope skip, empty-criteria, all-skip-with-basis — the same arm `orchestrator.Advance` runs on the retry path); on a hit it settles the acceptance stage straight to `succeeded` (a passed verdict / skip marker recorded, **no runner dispatched**) so the verb returns/continues with the settled stage rather than spawning a runner that would only fail category-C `acceptance_target_unreachable`. `fishhawk_dispatch_stage` / `fishhawk_run_stage` return an output composed from the settled stage and record **no** spawn evidence; `fishhawk_drive_run` appends a short-circuit `DriveStep`, records no act, spawns nothing, and continues the loop (the next poll observes the terminal stage). No new tool is added. The call **fails OPEN on a TRANSPORT error only**: a `short_circuited:false` result (a non-admissible stage state — already settled, mixed criteria, an unconfigured orchestrator) is the normal no-op path and proceeds to record+spawn exactly as today with **no** warning; a network/5xx admission-call error appends a fail-open warning before proceeding to spawn. A **4xx admission REJECTION** (401 / 403 `cross_run_admission` / 404 / 422) is **NOT** fail-open — the verb **halts** with a tool error and spawns nothing, so a runner never executes after the run-subject authorization boundary rejected the request. On the 5xx fail-open path the verb also **re-checks the target stage** before spawning (a mid-walk 500 can leave the acceptance stage `running`); an observed non-dispatchable state halts rather than double-driving a partially-settled stage.

### Acceptance target-identity gate ([#1953](https://github.com/kuhlman-labs/fishhawk/issues/1953))

When admission returns `short_circuited:false` **and** the approved plan needs LIVE validation (no short-circuit predicate matched) **and** the acceptance stage's spec declares egress `target_hosts`, the response carries `needs_target:true` + `target_hosts` (verbatim spec hosts) + `expected_head_sha` (the resolved merge-candidate head SHA; may be empty). On such a result each host-dispatch verb runs a **verb-side target-identity gate** BEFORE any spawn evidence: it probes the first declared target host **from the dispatch host** (the same network position the local runner would probe from) — a direct-dial `GET <scheme>://<host>/healthz` (`Proxy:nil`, so an ambient operator proxy can't fake reachability; http-first for loopback/IP-literal hosts, https-first otherwise) whose `git_sha` is classified against `expected_head_sha` with the **runner's semantics** (`unreachable < unverifiable < stale < verified`; a `-dirty` suffix is stale/fail-closed, a `<7`-char sha is unverifiable, a `>=7`-char prefix match is verified). This intentionally **mirrors** the runner's `previewprobe.go` (a separate, non-importable Go module) — the classification table is pinned in `acceptance_target_test.go`.

Outcomes:

- **stale / unreachable** → **refuse to spawn**: `fishhawk_dispatch_stage` / `fishhawk_run_stage` return a structured `needs_target` (`{target_host, expected_head_sha, detail, remediation}`, `outcome:"needs_target"` on run_stage) recording **no** spawn evidence and stamping **no** host-dispatch marker; `fishhawk_drive_run` stops with `stopped_reason=acceptance_needs_target` + a `NextActions` pointer, records **no** act and spawns nothing. The stage stays `awaiting_host_dispatch`/`pending`, so re-dispatch is clean once the operator brings up the target at the named head SHA (e.g. `scripts/dev preview`).
- **verified** → **proceed to spawn** exactly as today.
- **unverifiable** (target answered but exposes no comparable build identity) → proceed with a warning.
- **`FISHHAWK_ACCEPTANCE_PREVIEW_CMD` set in the verb env** → proceed with an informational note **without probing**: the spawned local runner inherits `os.Environ()` and provisions the target itself ([#1569](https://github.com/kuhlman-labs/fishhawk/issues/1569)).
- **empty `expected_head_sha`** (older backend / ledger resolution failure) → proceed with a warning — the gate never hard-fails a stage on a missing expectation (runner parity).
- **no declared hosts** → proceed silently (the runner skips its own target gate then too).

All response fields are additive: a mixed old/new backend that omits them decodes to zero values and the verb spawns exactly as today.

## Local auto-driver (`fishhawk_drive_run`, [#1700](https://github.com/kuhlman-labs/fishhawk/issues/1700))

`fishhawk_drive_run` executes **every mechanical operator step between human gates** on a `runner_kind:local` run under ADR-040 delegation, and stops at the first genuine decision. It is the local sibling of the GHA campaign auto-driver (E25.6/E25.7's `AutoDriveRunGate`): a bounded, resumable loop that reuses this host's session, token, and detached-spawn machinery rather than a separate daemon (ADR-024 — the local runner can only be spawned by this MCP host).

Each loop iteration:

1. **Gate-ordered, record-before-dispatch.** The driver dispatches only the **earliest non-terminal stage**, and only once its **gate preconditions** hold: plan is always dispatchable; implement dispatches after the plan stage **succeeds**; acceptance dispatches after the implement stage **succeeds** and every review stage settles. A fresh run creates every stage as a `pending` row, so this ordering is load-bearing (#1890): the earlier lowest-sequence-dispatchable rule dispatched implement + acceptance the instant plan was spawned, and both died category-C on the lineage lock the plan runner held. The **host-spawnable** states are `{pending, awaiting_host_dispatch}` ([#1912](https://github.com/kuhlman-labs/fishhawk/issues/1912)): a `runner_kind`-locked-local run parks its agent stage at `awaiting_host_dispatch` (the backend cannot spawn the host-local runner, ADR-024), so a **parked implement after a delegated plan approval is AUTO-DISPATCHED by the loop with no manual handoff**. For that one dispatchable stage it FIRST calls `POST /v0/runs/{id}/auto-drive/acts` to record the dispatch; then, immediately before spawning, it calls `POST /v0/runs/{id}/stages/{stage_id}/host-dispatch` to **mark the spawn** (CAS `{pending, awaiting_host_dispatch}` → `dispatched`), and only on a **successful record AND marker** host-spawns the runner via the same `spawnRunnerStageDetached`/`composeRunnerArgv` path `fishhawk_dispatch_stage` uses. A failed record stops the verb (`stopped_reason=unrecorded_act`) and a failed marker stops it (`stopped_reason=host_dispatch_failed`), both **without dispatching** — an unaudited or unmarked mechanical act is impossible by construction (an unmarked spawn would recreate the `dispatched` ambiguity #1912 removes).

   **Fix-up re-opens, honestly, in two parts.** A stage re-opened to `awaiting_host_dispatch`/`pending` after a delegated fix-up/retry is handled per invocation:
   - **Intra-invocation** (the same drive loop that saw the gate act): the re-open is re-dispatched and attributed `fixup_redispatch`.
   - **Cross-invocation** (a fresh drive invocation resuming a fix-up re-opened stage): the driver **never auto-spawns** a stage already in `dispatched` that it did not spawn itself — double-spawn stays impossible by construction. A re-opened `pending` implement stage carrying a `stage_fixup_triggered` audit row newer than its newest implement dispatch is re-dispatched as `fixup_redispatch`.

   **`dispatched` staleness — spawned but never reached its prompt fetch ([#1912](https://github.com/kuhlman-labs/fishhawk/issues/1912)).** Post-#1912 `dispatched` unambiguously means **a spawn attempt exists** (the host-dispatch marker stamped it, refreshing `updated_at`). A `dispatched` stage this invocation did not spawn has a runner in flight — from a prior driver invocation OR a manual `fishhawk_dispatch_stage` — so the driver **never re-spawns** it. It is classified purely on the **runner-liveness threshold**, anchored on the stage's own `max(updated_at, started_at)` (the marker/spawn timestamp; the older #1905 dispatch-row-timestamp anchor is removed — the `run_auto_driven act:dispatch` row is now **attribution only**, still matched source-agnostically for the retry/fixup discriminator). Two branches:
   - **Anchor past the liveness threshold** (default 10 min; a live local runner flips `dispatched`→`running` within seconds of its prompt fetch, #1924) — the driver **probes host liveness itself** ([#1955](https://github.com/kuhlman-labs/fishhawk/issues/1955)): it execs `pgrep -f` scoped to the stage's `--stage-id <uuid>` argv token (the MCP host is the host that spawned the runner, ADR-024, so the probe is precise) and classifies the result three ways. **DEAD** (pgrep exit 1, no matching process) — the spawned runner died at or just after spawn — is **auto-recovered in place**: the driver falls through its `record-act → host-dispatch marker → spawn` path with a stale-re-dispatch `steps_taken`/`run_auto_driven` note (`stale re-dispatch: liveness probe found no runner process`) and drives on, no operator action. **LIVE** (exit 0: a process carrying the stage id exists yet never flipped `running`) stops `dispatched_stale` and **never spawns** — a second runner into the same lineage lock stays impossible; the warning names the live process + the dispatch `log_path`. **UNKNOWN** (pgrep absent / exit ≥ 2 / exec error) degrades to today's manual verify-first stop `dispatched_stale`, `next_actions` pointing at a manual `fishhawk_dispatch_stage` after confirming no runner is live (`pgrep -f fishhawk-runner` + the dispatch's `log_path`). The `dispatched_stale` stop therefore survives only for the LIVE-or-unprobeable ambiguous cases.
   - **Anchor fresh** (or a zero-value anchor with no timestamped evidence) — poll. A hand re-dispatch spawns a fresh runner whose prompt fetch flips `dispatched`→`running`, so a subsequent `fishhawk_drive_run` reads the stage as in-flight and **polls to convergence** instead of re-reporting `dispatched_stale`.
2. **Poll** stages/reviews on the established 30s cadence while a stage or review is in flight. A pending **review** stage counts as in-flight only when it is REACHABLE (every lower-sequence stage terminal): a `feature_change` run creates its human `review` row `pending` at run creation, so an unconditional "pending review is in flight" rule would poll forever once the plan gate parks and never reach the gate/decision branch — the #1905 silent hang. When a caller supplies a **`progressToken`**, the driver emits an MCP `notifications/progress` heartbeat once per poll iteration (run state + earliest non-terminal stage + steps taken + elapsed) so a long drive is not aborted by the client's idle timeout; progress is opt-in per the MCP spec, so no token means no heartbeat (the return is still resumable).
3. **Gate.** At every parked gate it FIRST waits for the parked stage's advisory agent reviews to settle — while `review_status` is `pending` (the #1127 count gate) it polls instead of calling the gate, so a delegated `may_approve` fires only on settled reviews rather than churning observe-only gate calls every interval; an unreadable review state at a parked gate **fails toward the operator** (a warning, then falls through to the gate/decision return — this path executes no code, unlike the dispatch-path fail-closed). Then it calls `POST /v0/runs/{id}/auto-drive` and **continues** on a delegated act (`approve`/`route_fixup`/`retry`/`merge`), **returns** immediately on a page (`paged:<event>`), on an observe-only outcome at a decision state (`decision_required:<state>` — a plan gate without `may_approve`, a split verdict), or on a **pending scope amendment** (no delegation knob covers amendments, so every one is a decision). A stall guard returns `stalled` rather than spinning. Two fail-closed guards: an **unreadable amendment state** (the amendment audit read errors) halts the driver (`stopped_reason=amendment_check_failed`) rather than falling through to dispatch, and a **queued merge** is remembered so later polls only await the webhook-settle — the gate is not re-called (no duplicate `run_auto_driven act:gate merge` rows, no re-enable of auto-merge). **Queued-merge memory persists across resumes**: the loop seeds it from an existing `run_auto_driven act:gate action:merge` audit row before starting, so a resume during merge latency polls for the webhook-settle instead of re-calling the gate. That seed read **fails OPEN** (a warning; the loop continues) — unlike the dispatch-path reads it opens no code-execution surface, so a false negative costs only a benign duplicate attribution row, and fail-closed halting would trade that for a wedge.

A clean run under fully delegated knobs goes `start_run` → `merged` with **no operator tool calls in between**, and its audit trail carries a delegated-context `run_auto_driven` row for **every** driver dispatch and **every** gate act (each gate row records the delegated rule as `delegated_rule` for provenance). **`run_auto_driven` is the supplementary driver-attribution record; each action's own audit row (`approval_submitted` with its delegated rule, `stage_fixup_triggered`, …) is the authoritative delegation record**, written transactionally by the action path. `merge` is **queued, not landed** (it enables GitHub auto-merge; the webhook settles the terminal run state), so `merged` is reported only after the run reaches `succeeded`.

Output: ordered `steps_taken[]` (each labeled mechanical vs delegated), the final run/stage state, `stopped_reason` (`merged` | `paged:<event>` | `decision_required:<state>` | `timeout` | `stalled` | `stage_failed` | `unrecorded_act` | `host_dispatch_failed` | `run_failed` | `cancelled` | `gate_error` | `amendment_check_failed` | `dispatch_check_failed` | `dispatched_stale` | `context_cancelled`), and a `next_actions` pointer on a parked stop. **Every outcome is resumable** by re-invoking with the same `run_id`. Inputs: `{run_id, working_dir, github_repo, base_branch, runner_binary, max_minutes (clamped [1,240], default 60)}`. A **write** tool requiring `write:approvals`; local-only, requires the `fishhawk-runner` binary on the MCP host.

## Parallel decomposed children (`fishhawk_run_children`, [#1144](https://github.com/kuhlman-labs/fishhawk/issues/1144))

`fishhawk_run_children` is the fan-out sibling of `fishhawk_run_stage`: where `run_stage` drives **one** stage of **one** run, `run_children` drives **all** of a decomposed parent's pending children **concurrently**. Pass the decomposed **parent's** `run_id`; the tool:

- **Discovers** the children from the parent's `plan_decomposed` audit entry (`child_run_ids` + `effective_max_parallel`); a run with no such entry is a clean error (it is not a decomposed parent).
- **Partitions** by freshly-read state — only children awaiting a host spawn (`pending` or `awaiting_host_dispatch`, #1912) are spawned; in-flight (`dispatched`/`running`, a spawn attempt already exists) and terminal children are reported as-is, so a re-invocation is **idempotent**.
- **Spawns** each pending child's implement stage as a `fishhawk-runner` subprocess (the same `spawnRunnerStage` process-group/SIGKILL core `run_stage` uses) with `--parallel-isolate` appended, so each child provisions its **own isolated per-child git worktree** (`run-<child>`) — concurrent siblings, which already own distinct per-slice sole-writer branches (E24.1), never race a shared checkout, and the operator's tracked tree stays untouched.
- **Bounds** concurrency with an `errgroup` whose limit is the orchestrator-resolved effective cap, **clamp-DOWN-only** against an optional `max_parallel` override (it can lower an unlimited/looser cap, never raise it; `effective_max_parallel == 0` means unlimited and skips the limit).
- **Awaits ALL with no sibling-cancel.** A child failure is **data**, not a tool error: every child is awaited and surfaces in `children[]` with its `exit_code`, `outcome`, and `stage_state` regardless of success.

Returns `children[]` (one entry per discovered child, in `plan_decomposed` order), `dispatched_count` (how many were pending and spawned), and `effective_cap` (the cap used; 0 = unlimited). Requires the `fishhawk-runner` binary to resolve on the MCP host, exactly like `fishhawk_run_stage`.

### Topological-wave dispatch (E24.X / [#1278](https://github.com/kuhlman-labs/fishhawk/issues/1278) slice B)

Decompositions whose `sub_plans` declare `depends_on` edges are dispatched in **topological waves** rather than one
global fan-out.

- **`plan_decomposed` `waves`.** `orchestrator.go::fanoutIfDecomposed` computes `waves [][]int` via
  `plan.Waves(decomposition)` (slice A, #1280 — a pure Kahn topological sort of the sub_plans' `depends_on` edges) and
  threads it through `emitPlanDecomposed` into the `plan_decomposed` audit payload alongside
  `child_run_ids`/`effective_max_parallel`. The waves carry SLICE INDICES that positionally index `child_run_ids`
  (`child_run_ids[i]` is slice `i`). A should-be-impossible `Waves` error (the plan was already validated in slice A)
  falls back to a single all-indices wave (`singleAllIndicesWave`) with a WARN. `waves` is additive: the audit map
  always carries it, and the MCP `PlanDecomposed.Waves` mirror decodes it `omitempty` (nil → single wave).
- **Non-settling per-wave fan-in.** `backend/internal/server/consolidate.go::handleIntegrateWave`
  (`POST /v0/runs/{run_id}/integrate-wave`; client method `apiClient.IntegrateWave`) reuses the EXACT same exported
  `IntegrateSlices` primitive `/consolidate` uses (no new git-merge code) to merge the slices SUCCEEDED SO FAR onto the
  consolidated branch — but, UNLIKE `/consolidate`, it does NOT require all children terminal, does NOT transition the
  parent stage, and does NOT advance/open the PR (the parent stage is identical before/after on BOTH the `integrated`
  and `slice_conflict` outcomes; the terminal fan-in stays `/consolidate`'s job after the last wave). It shares
  `/consolidate`'s auth + decomposed-parent precondition posture (`agent_token_forbidden`/`insufficient_scope` 403,
  `not_a_decomposed_parent` 400, `slice_integration_error` 502) minus the terminal-children gates.
- **`run_children` wave loop.** `run_children.go` replaces the single global errgroup with an ordered per-wave loop:
  each wave's pending children dispatch concurrently under the cap against `currentBase` (passed as both
  `--base-branch` and `--check-base-ref`, so wave N cuts from the prior wave's merged tree), then between waves it
  calls `IntegrateWave` and re-bases the next wave on the returned `consolidated_branch`. A nil/empty `waves` (an old
  entry, or a no-`depends_on` decomposition) collapses to a single all-indices wave dispatched against `main` and
  NEVER calls integrate-wave — byte-for-byte the pre-#1278 behavior.
- **Guards** (each surfaced as a warning; the loop stops): a dispatched wave-N child that did not succeed (no
  partial-wave integration), a `slice_conflict` or transport error from integrate-wave, and an empty
  `consolidated_branch` (the GitHub-not-wired graceful skip — `currentBase` is kept unchanged rather than dispatching
  against an empty ref). A waves index out of range is a loud tool error.

### Decomposed-parent observability (`children_status`, [#1147](https://github.com/kuhlman-labs/fishhawk/issues/1147))

For a **decomposed parent**, `fishhawk_get_run_status` carries a `children_status` block so the operator sees the fan-out's live progress instead of a bare `awaiting_children`:

- `children[]` — one entry per discovered child (`{run_id, slice_index, state}`) in `plan_decomposed` (slice-index) order. `state` is the child run's lifecycle state (`pending`/`running`/`succeeded`/`failed`) or `unknown` when that child's read failed. Aggregate counts (`total`/`pending`/`running`/`succeeded`/`failed`) accompany it.
- `integration_phase` — the fan-in phase classified from the `slices_integrated` / `slice_integration_conflict` audit kinds (ADR-041 / #1142): `running_children` (a child is still in flight), `ready_to_integrate` (all children succeeded, no fan-in yet), `integrated` (a clean fan-in — `consolidated_branch` is surfaced), or `integration_conflict` (a slice branch failed to merge — `conflicting_child_run_id` is surfaced).
- **Best-effort:** a per-child read failure degrades that child to `state="unknown"` and never fails the snapshot.
- **Cost-gated:** the per-child fetch runs only for a top-level run (no `parent_run_id`) whose implement stage is `awaiting_children` **or** whose recent-audit window carries a decomposition marker (`plan_decomposed` / `slices_integrated` / `slice_integration_conflict`). An ordinary run makes **zero** extra calls (no `plan_decomposed` read), and the block is omitted for non-decomposed runs. The `next_actions` `implement_awaiting_children` arm points the operator at `fishhawk_run_children` plus this block.

## Server-suggested next actions (`next_actions`, #1024)

`fishhawk_get_run_status` and the run-terminal `fishhawk_run_stage` result both carry a `next_actions` block — the generalization of `review_action_hint` (#777/#860) across the whole run lifecycle. The classifier (`next_actions.go`) is a pure function over data the tools already fetch (run row, stage rows, review statuses, the computed hint, the drive read view): no extra backend round-trip, no new endpoint.

Shape: `{state, actions[]}`. `state` is the classified lifecycle state (`plan_gate_parked`, `implement_failed_category_b`, `implement_concerns_open`, `succeeded_pr_open`, `succeeded_merged`, …; terminal runs name the run state with no actions; an unmatched non-terminal state classifies `unclassified`). Each action entry carries:

- `action` — the tool to call (`fishhawk_resume_run`, `fishhawk_fixup_stage`, …) or a named ritual step outside the MCP surface (`approve_pr`, `merge_pr`, `post_merge`, `merge_and_file_follow_up`, `file_product_issue`);
- `params` — key parameters (`run_id`, `stage_id`, `parent_run_id`, the `concern_ids` source);
- `precondition` — when the action is legal;
- `consumes` — what taking it spends: `none` | `fixup_budget` | `retry_budget` | `approval_slot` | `new_run`;
- `reason` — one-line why-this-now.

Invariants:

- **Display-only, never gates** — like the periodic-budget block and the hint it generalizes, the block is advisory; the server-side applicability predicates stay authoritative.
- **A non-terminal run always carries ≥ 1 action.** Any state the table does not match falls back to `unclassified` (re-poll + file a product issue naming the state), structurally — never an empty list.
- **The concern arm derives from the hint computation** (`ReviewActionHint.suggestedActions`), so `review_action_hint` and `next_actions` cannot disagree on the remaining fix-up budget or override availability.
- **Drive folds first**: on drive-enabled runs the `drive_status.next_action` is prepended, so drive and `next_actions` never point different ways.
- **Decomposed parent at `awaiting_children`** (#1147) classifies `implement_awaiting_children` — a dedicated arm offering `fishhawk_run_children` (fan out the still-pending children) plus a poll pointing at the `children_status` block for each child's live state and the fan-in/integration phase.
- **A delegating deploy stage classifies per its state** (E23.13/E23.15 / [#1429](https://github.com/kuhlman-labs/fishhawk/issues/1429) / [#1432](https://github.com/kuhlman-labs/fishhawk/issues/1432)). A standalone release run's deploy stage at `awaiting_deploy_approval` classifies `deploy_gate_parked` and offers `fishhawk_approve_deploy` (carrying the required `environment` param and a precondition naming the `write:deploy` scope + the `--environment=<allowed_environments>` requirement) plus a `fishhawk_reject_deploy` counterpart — **not** the older `fishhawk_approve_plan` hint, which errors on a plan-less release run. Once approved, the backend triggers the external pipeline and the run classifies `deploy_in_flight` (poll until the deploy stage settles). See [Deploy-gate approval](#deploy-gate-approval-fishhawk_approve_deploy-fishhawk_reject_deploy-1432).
- **An acceptance stage gates the merge** (E31.9 / [ADR-049](https://github.com/kuhlman-labs/fishhawk/issues/1519)). When the workflow declares an acceptance stage, the settled-implement path branches to it **before** the merge ritual, reusing the existing verbs — no new tool (the registry stays at 39). Arms: a non-terminal acceptance stage classifies `acceptance_pending` (offering `fishhawk_dispatch_stage` first — non-blocking, since acceptance runs long against the customer-provisioned preview — with `fishhawk_run_stage` as the blocking opt-in; `github_actions` runs get a poll) or `acceptance_running` (poll); a succeeded stage with verdict `passed` classifies `acceptance_passed` and returns the merge ritual (ADR-049 decision #6: the merge is gated on the `acceptance_passed` evidence condition); a failed verdict whose deterministic-triage disposition is a **paged** variant (`paged` / `rerun_budget_exhausted` / `fixup_unavailable_paged` / `retry_unavailable_paged` / `unsettled_paged`) classifies `acceptance_triage_paged` (read the evidence, then arbitrate: manual `fishhawk_fixup_stage`, `merge_and_file_follow_up`, or `fishhawk_cancel_run`); an **auto-routed** disposition (`fixup_dispatched` / `retry_dispatched`) re-opens the implement/acceptance stage server-side, so the next snapshot's existing stage-state arm serves it (`acceptance_triage_rerouting` is the transient poll in between). A settled stage whose verdict is not in the recent-audit window classifies the **defensive** `acceptance_settled_outcome_unknown` (point at `fishhawk_list_audit`; **never** the merge ritual — fail toward read, not toward merge). That arm also offers the **`fishhawk_retry_stage` settled-outcome-unknown recovery** (E31.16 / [#1567](https://github.com/kuhlman-labs/fishhawk/issues/1567)): once `fishhawk_list_audit` confirms no `acceptance_outcome_recorded` verdict exists for the stage (the agent shipped a non-schema field and the verdict failed closed), retrying the acceptance stage re-opens it `succeeded → pending` for a re-run (operator token only; the server 422s if a verdict IS recorded) — the reopen lands the stage in pending so the `acceptance_pending` arm's `fishhawk_dispatch_stage` then serves the actual re-run. **Exception — the out-of-scope skip disposition** (E38.3 / [#1877](https://github.com/kuhlman-labs/fishhawk/issues/1877)): a succeeded verdict-less stage whose recent-audit window carries the `acceptance_skipped_out_of_scope` marker classifies the **pre-succeeded** `acceptance_skipped_out_of_scope` state and returns the merge ritual — the orchestrator auto-terminated a degenerate stage (`verification.out_of_scope`, zero `acceptance_criteria`), a legitimate terminal disposition equivalent to a recorded outcome, so the run is merge-eligible and no verdict was recorded **by design**. This is checked **before** the `acceptance_settled_outcome_unknown` arm, so the futile `fishhawk_retry_stage` reopen arm is **not** offered for the skip (the server also `422 retry_not_applicable`s a direct retry against a skip-marked stage — a reopen would re-fire the same skip). When the marker ages out of the recent-audit window the flag is false and the arm degrades gracefully to the read-first `acceptance_settled_outcome_unknown` arm (fail toward read, never toward merge). The verdict/disposition vocabulary is **mirrored, not imported** from `backend/internal/server/acceptance.go` (the #875 compile trap), pinned by a literal-table test. A FAILED verdict leaves the STAGE `succeeded`, so the classifier reads the `acceptance_outcome_recorded` / `acceptance_triage_decided` audit payloads, never the stage state.
- **An out-of-scope plan auto-terminates the acceptance stage** (E38.3 / [#1657](https://github.com/kuhlman-labs/fishhawk/issues/1657)). When the approved plan declares `verification.out_of_scope` with **zero** `acceptance_criteria`, the acceptance stage has no observable criterion to validate, so the orchestrator walks it straight to `succeeded` and emits an `acceptance_skipped_out_of_scope` audit marker (rather than waiting for an operator to dispatch a degenerate no-observable-change stage). A succeeded run with an open PR whose recent-audit window carries that marker classifies **`succeeded_acceptance_skipped_out_of_scope`** — the full `approve_pr` → `merge_pr` → `post_merge` merge ritual, still **merge-eligible**; only the state label changes so the operator knows why no acceptance verdict was recorded. If the marker ages out of the recent-audit window the arm degrades gracefully to plain `succeeded_pr_open` (itself merge-eligible). `fishhawk_audit_complete` exempts the marked stage from its trace-required rule (the auto-terminated stage runs no agent and ships no trace).
- **The run lifecycle owns its post-merge tail** ([#1370](https://github.com/kuhlman-labs/fishhawk/issues/1370)). A succeeded run with an open PR URL classifies `succeeded_pr_open` (the full `approve_pr` → `merge_pr` → `post_merge` merge ritual) **until** the backend observes the PR merge resolve: `resolveReviewStageOnMerge` emits a `post_merge_observed` audit row alongside the `pr_merged` / `run_merged` board move (from both the `pull_request.closed` webhook and the merge-reconciler poll, which share that path). `get_run_status` reads that entry off the recent-audit slice it already fetches and reclassifies the run `succeeded_merged` — dropping the now-completed `approve_pr` / `merge_pr` steps and surfacing **only** the operator `post_merge` dev-host step (the `scripts/dev post-merge` rebuild/reload stays an operator/deploy concern, [ADR-038](https://github.com/kuhlman-labs/fishhawk/issues/925)). So a merged run's tail state is owned and observable in `get_run_status` rather than implicit in whether the operator ran the script. (`fishhawk_run_stage`'s run-terminal `next_actions` never observes a post-merge — its PR is not open at stage exit — so it always passes `mergeObserved=false`.)

## Deploy-gate approval (`fishhawk_approve_deploy` / `fishhawk_reject_deploy`, [#1432](https://github.com/kuhlman-labs/fishhawk/issues/1432))

`fishhawk_approve_deploy` and `fishhawk_reject_deploy` (E23.15) are the deploy-gate counterparts to `fishhawk_approve_plan` / `fishhawk_reject_plan`. After [#1429](https://github.com/kuhlman-labs/fishhawk/issues/1429) advances a release run's deploy stage to `awaiting_deploy_approval`, the operator loop needs a verb that targets the deploy gate: `fishhawk_approve_plan` resolves a `type=plan` stage first and errors `no plan stage on run …` on a plan-less release run before reaching the approval endpoint. Both new tools take a **run id** and resolve the `type=deploy` stage internally (`resolveDeployStage`, the deploy analogue of `resolvePlanStage`), then `POST` to the existing `/v0/stages/{id}/approvals` endpoint — no new backend, REST, or client-transport surface.

A deploy stage's gate is **pre-execution** ([ADR-038](https://github.com/kuhlman-labs/fishhawk/issues/925): a deploy stage's effect IS the side effect), so approving triggers the external pipeline — a production deploy pages the human regardless of runner kind.

- **`fishhawk_approve_deploy`** requires an operator token with **`write:deploy`** (ADR-038/#1390) and a **required `environment`** that must be one of the deploy stage's `allowed_environments`. The environment is conveyed to the backend **only through the approval comment** — the deploy pre-flight's `parseEnvironmentFlag` scans whitespace-delimited tokens for `--environment=<env>` (there is no structured environment field on the approval request body), so the tool composes `--environment=<environment>` into the comment. An optional `override_freeze` flag appends a standalone `--override-freeze` token (which the backend's `commentHasFlag` matches exactly) so a deploy during a spec-declared `change_freeze` is permitted. A trimmed `reason` is appended after the flags. An empty `environment` fails locally before the HTTP hop. Because the backend pre-flight parses flag tokens from the **whole** comment, the tool guards against flag smuggling (#1432): `environment` must be a single whitespace-free token (rejecting e.g. `production --override-freeze`), and `reason` must not carry a standalone `--override-freeze` token unless `override_freeze` is set — so `--override-freeze` appears in the comment **only** when the operator requested it. Both checks fail locally before the HTTP hop. Backend pre-flight refusals surface as typed tool errors: `422 deploy_environment_not_allowed` (absent / disallowed environment), `422 deploy_change_freeze_active` (freeze active, no override), `422 deploy_upstream_not_satisfied` (a `required_upstream` signal — `ci_green` / `review_merged` — not met), and a `403` when the token lacks `write:deploy`.
- **`fishhawk_reject_deploy`** mirrors `fishhawk_reject_plan`: a deploy reject routes through the backend `advanceStage` path (not the approve-only deploy pre-flight block), so it needs **neither `write:deploy` scope nor an environment**. The reason is recorded on the approval row as `comment`.

The `next_actions` `deploy_gate_parked` arm points at `fishhawk_approve_deploy` (with the `environment` param and a precondition naming the `write:deploy` + `--environment` requirement) plus a `fishhawk_reject_deploy` action. Duplicate-submission labeling (#986) applies to both, identical to the plan-gate tools. Deploy **rollback** is out of scope here — there is no rollback approval endpoint; the CLI `fishhawk deploy rollback` already exists and a rollback verb is a separate follow-up.

> **Operability note (stale local MCP token).** A local dev MCP server's `FISHHAWK_API_TOKEN` may have been issued before E23.10 added `write:deploy` to the operator default scope set, so `fishhawk_approve_deploy` returns `403 insufficient_scope` (`required_scope: write:deploy`) even for the operator. `operatorDefaultScopes` now includes `write:deploy`, so re-issuing the MCP token with the default scope set (`fishhawkd token issue --subject <s>`) fixes it.

**`fishhawk_start_run` — `upstream_run_id` input field (E23.11 / #1417 / #1490).** When minting a standalone deploy-only `release` run whose deploy stage carries a `required_upstream` pre-flight gate, pass `upstream_run_id` (a UUID string) to name the upstream `feature_change` run whose `ci_green` / `review_merged` the gate evaluates. Omit for all other run types — a non-deploy-gate run ignores the field. This is DISTINCT from `parent_run_id` (a follow-up/lineage link): `upstream_run_id` is a deploy-gate safety reference only, so it carries none of the `get_plan`-resolution / resume-retry / decomposition-provenance semantics `parent_run_id` does. The value is validated locally as a well-formed UUID before the HTTP hop; a malformed value surfaces a clean tool error without a backend round-trip. The echoed `upstream_run_id` on the returned Run confirms the value round-tripped correctly. The CLI mirror is `fishhawk run start --upstream-run-id <uuid>`; both surfaces validate locally.

## Implement-review fix-up (`fishhawk_fixup_stage`)

`fishhawk_fixup_stage` (E22.X / [#762](https://github.com/kuhlman-labs/fishhawk/issues/762)) routes one or more **advisory implement-review concerns** ([ADR-027](https://github.com/kuhlman-labs/fishhawk/issues/703) `approve_with_concerns`) back to the implement agent for a single fix-up pass, instead of the operator hand-editing the PR branch. It wraps `POST /v0/stages/{stage_id}/fixup`.

Inputs:

| Field | Required | Notes |
|---|---|---|
| `stage_id` | **yes** | The implement stage parked at the review gate. |
| `concern_ids` | one of `concern_ids`/`concerns` | **Primary addressing ([#964](https://github.com/kuhlman-labs/fishhawk/issues/964))** — stable concern UUIDs to route back (at least one). Read them from `fishhawk_get_run_status`'s `run.concerns.items[].id`. Only this stage's **open** implement-stage concerns resolve; an unknown, foreign, plan-stage, or already-resolved ID is `validation_failed`. Routed concerns are marked `addressed_pending` (with `reason` as `state_reason`) in the durable concern store. |
| `concerns` | one of `concern_ids`/`concerns` | **Deprecated positional fallback** — indices into the stage's flattened `implement_reviewed` concern set. Ambiguous once multiple heterogeneous review entries exist per stage; prefer `concern_ids`. Only valid when `concern_ids` is absent — supplying both is `validation_failed`. |
| `reason` | no | Operator note, recorded on the `stage_fixup_triggered` audit entry (and as the routed concerns' `state_reason` on the ID path). |
| `allow_create` | no | Repo-relative paths this fix-up will **create** ([#823](https://github.com/kuhlman-labs/fishhawk/issues/823)). See below. |

**Declaring net-new files (`allow_create`)** — a concern that requires a *new* file needs `allow_create`. Each declared path is folded into the implement stage's **effective `scope.files` for THIS fix-up pass only** (never the persisted plan scope), reusing the same [#824](https://github.com/kuhlman-labs/fishhawk/issues/824) `foldScopePaths` machinery `add_scope_files` uses. Because the runner's created-out-of-scope gate ([#818](https://github.com/kuhlman-labs/fishhawk/issues/818)) keys off that effective union, folding the path in makes the runner stage the new file so the gate stops tripping for it. The pass is bounded and operator-authorized: a fix-up only happens when the operator calls this verb, and `allow_create` widens the legitimate set only by the paths the operator names. **Preserved contract:** any created file **NOT** declared here still fails category-B per #818 — declaring paths does not reopen the silent-strip hole. Entries must be repo-relative; an absolute path or one containing `..` is rejected (`validation_failed`, 400, `field: allow_create`). The OpenAPI/`v0.md` surface remains the authoritative parameter reference.

What a fix-up does — and how it differs from `fishhawk_retry_stage`:

- The selected concerns are delivered to the agent as **binding instructions** (the [#558](https://github.com/kuhlman-labs/fishhawk/issues/558) condition-delivery framing: MANDATORY, win on conflict).
- The agent commits onto the **same PR branch** and the existing PR is **updated** — a fix-up does **not** regenerate a fresh diff or open a new PR. (`retry` re-opens a *failed* stage and regenerates; fix-up re-opens a *healthy* review gate.)
- The implement review re-runs on the result.
- On success the stage flips `awaiting_approval → pending` (the orchestrator advances it to `dispatched`, re-firing `workflow_dispatch`); the tool returns the re-opened stage.

**Operator-gated and bounded — this is never an unbounded auto-loop:**

- The bound defaults to **one pass per stage**, enforced server-side by counting prior `stage_fixup_triggered` audit entries. A second attempt once the bound is spent returns a `fixup_budget_exhausted` tool error (its details carry `max_passes` + `used`). The remaining budget is `max − fix-ups already triggered`, surfaced on the audit entry's `remaining_budget` field (read it via `fishhawk_list_audit`); the success response itself carries only the re-opened stage.
- **No-change refund ([#967](https://github.com/kuhlman-labs/fishhawk/issues/967)):** a pass whose re-dispatch produced **no commit** (the `fishhawk_run_stage` result carries `fixup_no_changes: true`; a `fixup_no_changes` audit entry exists for the stage) is refunded against the **normal** budget, so the next trigger is admitted without `force_additional_pass`. The refund **never** extends the absolute 3-pass ceiling, which counts every triggered pass including refunded ones (`refunded_passes` on the `stage_fixup_triggered` audit entry records the refund).
- **Operator owns the trigger and the merge.** A fix-up only ever happens when an operator calls this verb; the agent cannot self-trigger one, and the operator still approves the final merge.
- **Auth:** a write tool requiring `write:stages` (or the dedicated `write:fixups`) scope. A run-bound token may fix up only stages **within its own run** — a cross-run target returns `cross_run_fixup` (403).

Error surfaces propagated as tool errors: `validation_failed` (400, no concern selection / both `concern_ids` and `concerns` supplied / out-of-range index / unknown, foreign, plan-stage, or non-open `concern_id` — the empty/mixed selections are also caught locally before the HTTP hop), `cross_run_fixup` (403), `stage_not_found` (404), `fixup_not_applicable` (422, no recorded `approve_with_concerns` verdict to route back), `fixup_budget_exhausted` (422).

## Plan-gate revise (`fishhawk_revise_plan`)

`fishhawk_revise_plan` (E22.X / [#1099](https://github.com/kuhlman-labs/fishhawk/issues/1099)) is the **third plan-gate verdict** alongside `fishhawk_approve_plan` and `fishhawk_reject_plan`: it re-plans **in place** in the same run against a binding operator design constraint, instead of approving the plan as-is or rejecting it to a fresh-run replan. It wraps `POST /v0/stages/{stage_id}/revise`. Takes a **run id**; the tool resolves the plan stage internally (the `type=plan` stage, like the approve/reject tools).

Inputs:

| Field | Required | Notes |
|---|---|---|
| `run_id` | **yes** | The run whose plan stage to revise. |
| `constraint` | **yes** | The binding design constraint the planner must revise the prior plan to satisfy. Injected into the re-dispatched plan prompt as a dedicated, binding **"Revision constraint"** section (the [#558](https://github.com/kuhlman-labs/fishhawk/issues/558) condition-delivery framing: MANDATORY, wins on conflict), with the prior plan carried as the **revision base**. Empty constraints are rejected (`validation_failed`, also caught locally before the HTTP hop). |
| `force_additional_pass` | no | Bounded operator override — grant ONE revise pass **beyond** the normal budget when it is already spent (`revise_budget_exhausted`), hard-capped at 3 total passes per stage. The forced pass is audited. |

When to reach for revise vs the alternatives:

- **approve** — the plan is correct as written.
- **revise** — the plan's direction is sound but a design constraint must change first. Cheaper than a reject → fresh-run replan because the prior plan is the revision base and only the constrained parts change; the operator's design intent reaches the agent through the same binding channel as approval conditions.
- **reject** — the plan takes a wrong fork no constraint can amend.

What a revise does — and how it differs from `fishhawk_reject_plan`:

- The constraint is delivered to the planner as a **binding** instruction in a dedicated "Revision constraint" prompt section — never under the clarification-answers or approval-conditions heading — and the prior plan rides as the revision base so the planner **revises** rather than replanning blank-slate.
- On success the plan stage flips `awaiting_approval → pending` (the orchestrator advances it to `dispatched`, re-firing `workflow_dispatch`); the run re-enters the normal plan **review → approve** gate. (`reject` fails the gate as category D and the next step is a fresh run.)

**Operator-gated and bounded — this is never an unbounded auto-loop:**

- The bound defaults to **one pass per stage**, enforced server-side by counting prior `plan_revised` audit entries (no dedicated column — exactly as fix-up counts `stage_fixup_triggered`). A second attempt once the bound is spent returns a `revise_budget_exhausted` tool error (details carry `max_passes` + `used`); the operator may grant ONE more pass with `force_additional_pass=true`, hard-capped at 3 total passes. At the ceiling the tool returns the distinct `revise_ceiling_reached` error (a hard stop — reject and start a fresh run).
- **Auth:** a write tool requiring `write:approvals` scope (the #558 binding-conditions / gate-answer family). A run-bound token may revise only stages **within its own run** — a cross-run target returns `cross_run_revise` (403).

Error surfaces propagated as tool errors: `validation_failed` (400, empty constraint), `cross_run_revise` (403), `stage_not_found` (404), `revise_not_applicable` (409, the stage is not a plan stage parked at `awaiting_approval`), `revise_budget_exhausted` (409), `revise_ceiling_reached` (409). The OpenAPI/`v0.md` surface remains the authoritative parameter reference.

## Concern waiver (`fishhawk_waive_concern`)

`fishhawk_waive_concern` (E22.X / [#984](https://github.com/kuhlman-labs/fishhawk/issues/984)) waives one **open** review concern (`raised`, `addressed_pending`, or `reopened`) with a **required, audited reason** — the operator judgment that the concern does not warrant a change (false positive, accepted trade-off, deliberate deferral), as distinct from `fishhawk_fixup_stage` (route the concern back to the agent). It wraps `POST /v0/concerns/{concern_id}/waive`.

Inputs:

| Field | Required | Notes |
|---|---|---|
| `concern_id` | **yes** | The stable concern UUID, from `fishhawk_get_run_status`'s `run.concerns.items[].id`. |
| `reason` | **yes** | Audited rationale. Recorded on the `concern_waived` audit entry **before** the state change (append failure → `audit_append_failed`, no mutation), stored as the concern's `state_reason`, and rendered **verbatim** in later re-review prompts as the not-re-litigable waive context — make it self-contained. |

What a waive does:

- The concern transitions to the **terminal** `waived` state: it leaves `run.concerns` (the open block), can no longer be selected by `fishhawk_fixup_stage`'s `concern_ids`, and later re-reviews of the stage see it as context that must **not** be re-litigated absent new evidence.
- There is **no un-waive**. If the concern turns out to matter, a new concern from a later review is the path back.
- **Auth:** same write-scope pair as fix-up (`write:stages` or `write:fixups`); a run-bound token may waive only its own run's concerns (`cross_run_waive`, 403).

Error surfaces propagated as tool errors: `validation_failed` (400 — empty reason / bad UUID, both also caught locally before the HTTP hop), `cross_run_waive` (403), `concern_not_found` (404), `concern_waive_conflict` (422 — the concern is already `waived`/`superseded`/`addressed`; details carry the rejected `from`/`to` pair), `concern_store_unconfigured` (503).

## Concern defer (`fishhawk_defer_concern`)

`fishhawk_defer_concern` (E22.X / [#1202](https://github.com/kuhlman-labs/fishhawk/issues/1202)) converts one **open** review concern (`raised`, `addressed_pending`, or `reopened`) into a conventions-complete, boarded, epic-linked **follow-up work item** AND transitions the concern to the terminal `deferred` state — in a single call. It is the "not now, but track it" verb, sitting between `fishhawk_fixup_stage` (route the concern back to the agent now) and `fishhawk_waive_concern` (resolve with no follow-up). It consumes **no** fix-up budget. It wraps `POST /v0/concerns/{concern_id}/defer`.

The follow-up body is **auto-drafted** server-side from the concern — its note, severity, category, the reviewer model, the evidence run id, and the source PR link — so you do not hand-author it (the friction this replaces: ~7 hand-authored follow-ups via `fishhawk_file_issue` in one loop session). You supply only the title coordinates the concern cannot carry.

Inputs:

| Field | Required | Notes |
|---|---|---|
| `concern_id` | **yes** | The stable concern UUID, from `fishhawk_get_run_status`'s `run.concerns.items[].id`. |
| `parent_epic` | **yes** | The epic the follow-up rolls up to (an issue reference like `#1196`); its leading `[E<n>]` title token is fetched to derive the `{epic}` placeholder. Operator judgment — not derivable from the concern. |
| `n` | no | The child number for the `[E<epic>.<n>]` title. Discovered server-side (#1958) from the parent epic's existing children (open and closed) and the next one allocated, so you no longer have to guess it — pass `n` only to override. |
| `type` | no | Override the auto-selected work-item type (`bug` for a defect category, else `chore`). |
| `labels` | no | Labels merged on top of the type's default labels. |
| `note` | no | Operator addendum folded into the follow-up body and the concern's `state_reason`. |

What a defer does:

- Files the follow-up work item, then transitions the concern to the **terminal** `deferred` state: it leaves `run.concerns` (the open block), can no longer be selected by `fishhawk_fixup_stage`'s `concern_ids`, and its `state_reason` names the filed issue.
- **Orphan-issue-safe.** An already-resolved concern is rejected **before** any issue is filed (`concern_defer_conflict`, 422). A filing failure leaves the concern **open** (no transition) so you can retry. The success `concern_deferred` audit entry is written only **after** the transition succeeds; a post-filing transition race emits only a corrective `concern_defer_failed` entry (naming the actual state + the orphaned issue url) and returns 422.
- **Auth:** byte-identical to waive — same write-scope pair (`write:stages` or `write:fixups`); a run-bound token may defer only its own run's concerns (`cross_run_defer`, 403).

Returns the filed follow-up issue (`{type, title, number, url, provider, applied_labels}`) and the updated concern row (state `deferred`, `state_reason` naming the issue).

Error surfaces propagated as tool errors: `validation_failed` (400 / bad UUID, caught locally before the HTTP hop), `cross_run_defer` (403), `concern_not_found` (404), `concern_defer_conflict` (422 — non-open concern or a post-filing race), `work_item_invalid` (422), `provider_unimplemented` (501), `work_item_filing_failed` (502 — the concern stays open), `concern_store_unconfigured` (503).

## Run-branch reset (`fishhawk_reset_run_branch`)

`fishhawk_reset_run_branch` ([ADR-035](https://github.com/kuhlman-labs/fishhawk/issues/857) / [#867](https://github.com/kuhlman-labs/fishhawk/issues/867)) is the **destructive, operator-gated** remediation for a foreign commit pushed **ON TOP** of a run's own commits on the open PR branch. It force-rewinds the run/PR branch back to its **last run-authored HEAD** (the newest commit attributable to the run's reported-head ledger), dropping the on-top foreign commit, then re-parks the review gate so CI + the merge reconciler re-evaluate the rewound head. It wraps `POST /v0/runs/{run_id}/reset-branch`.

Inputs:

| Field | Required | Notes |
|---|---|---|
| `run_id` | **yes** | The run whose branch to reset. |
| `confirm` | **yes** | MUST be `true` — the reset is destructive, so it is never silent/auto. A missing/false value is refused (`confirmation_required`, 400; the tool also catches it locally before the HTTP hop). |
| `reason` | no | Operator note, recorded on the `branch_reset` audit entry. |

Safety (all server-enforced):

- **On-top only.** Refused with `reset_out_of_scope` (422) when the foreign commit is an ancestor/interleaved — a reset can't drop it; prevention ([#861](https://github.com/kuhlman-labs/fishhawk/issues/861)/[#865](https://github.com/kuhlman-labs/fishhawk/issues/865)) owns that.
- **Fail-closed.** Any classification uncertainty (unresolvable base ref, incomplete ledger, compare error, no identifiable run-authored HEAD, or a lease re-check that finds a concurrent push) returns `reset_not_determinable` (422) — the destructive action never force-updates on doubt. A clean tip returns `reset_not_applicable` (422).
- **Operator-gated + audited.** Requires `write:runs`; a run-bound token may reset only **its own** run's branch (`cross_run_reset`, 403). Every rewind writes a `branch_reset` audit entry; the dropped commit stays recoverable from the remote reflog / the foreign pusher's own branch (recorded in `recovery_note`).

Returns the rewind summary (`dropped_offending_sha`, `reset_to_sha`, `prior_head_sha`, `recovery_note`) on success.

## Local decomposition fan-in (`fishhawk_consolidate_slices`)

`fishhawk_consolidate_slices` ([E24.2 / ADR-041](https://github.com/kuhlman-labs/fishhawk/issues/857) / [#1238](https://github.com/kuhlman-labs/fishhawk/issues/1238)) runs the **decomposed-parent fan-in** on demand. After a decomposition's children all reach terminal-`succeeded`, the parent implement stage is parked in `awaiting_children` until the fan-in merges every slice branch onto the consolidated branch and opens the consolidated PR. That fan-in normally runs from the 60s **child-completion sweeper** — but the sweeper is **off by default in the local dev `fishhawkd`** ("dev-loop posture"), so on the local runner a settled parent stays parked with no consolidated branch/PR. This verb runs the same fan-in on demand, and (unlike the silent event-driven path) **surfaces** a non-conflict integration error so you can diagnose a stuck local fan-in. It wraps `POST /v0/runs/{run_id}/consolidate`.

> The local dev stack (`scripts/dev up`) now also passes `--enable-child-completion-sweeper`, so the sweeper backstop runs locally too; this verb is the explicit, error-surfacing operator path when you want to drive (or diagnose) the fan-in directly.

Inputs:

| Field | Required | Notes |
|---|---|---|
| `run_id` | **yes** | The decomposed **parent** run whose children's slices should be fanned in. |

Preconditions (each a clean tool error): the run is a decomposed parent (not a child, and it has children — `not_a_decomposed_parent`, 400); its implement stage is parked in `awaiting_children` (`not_awaiting_children`, 409); every child is terminal (`children_in_flight`, 409) and every one succeeded (`children_failed`, 409). Auth is operator `write:runs`; a run-bound token is refused (`agent_token_forbidden`, 403).

Outcomes (200):

- `integrated` — every slice merged cleanly; the parent implement stage resolved `succeeded` and the consolidated PR opened. Carries `consolidated_branch` + `pull_request_url`.
- `slice_conflict` — a slice branch failed to merge; the parent implement stage failed recoverable (category `B`), preserving the E24.2 contract. Carries `conflicting_slice_index` + `conflicting_child_run_id`.

A non-conflict failure returns `slice_integration_error` (502) with the cause in `details.error` — the diagnosability the event-driven fan-in path lacks.

Implementation: `backend/internal/server/consolidate.go::handleConsolidateRun`, registered as the MCP verb in `tools.go`.
It composes the **exported** orchestrator primitives `IntegrateSlices` → `TransitionStage` → `Advance`, mirroring
`childcompletion.resolveParent`'s all-succeeded arm, WITHOUT touching the hot event-driven/sweeper paths. The
`children_settled` and `slice_integration_conflict` audit payloads are byte-identical to the sweeper's, so the
`children_status` integration-phase classifier reports correctly whichever path settled the parent. Where the
event-driven `maybeAdvanceDecomposedParent` WARN-swallows a non-conflict `IntegrateSlices` error (leaving a silent
stuck parent), this endpoint returns it — the 502 `slice_integration_error` above, with the stage left
`awaiting_children` for retry.

## Run-branch vouch (`fishhawk_vouch_commit`)

`fishhawk_vouch_commit` ([ADR-035](https://github.com/kuhlman-labs/fishhawk/issues/857) / [#1044](https://github.com/kuhlman-labs/fishhawk/issues/1044)) is the **operator-gated, audited** provenance path for a foreign commit on a run branch that no loop-native remediation can route — an operator's mechanical remediation commit (e.g. a `scripts/sync-schemas` output pushed onto a decomposition fan-out branch whose children are all terminal with zero open concerns). Unlike `fishhawk_reset_run_branch` (which **drops** an on-top foreign commit), vouch **keeps** the operator commit and **declares it run-authored lineage**: the vouched SHA is unioned into the run's reported-head ledger (on the run's own chain and its decomposition children), so the merge reconciler's ADR-035 re-check attributes it cleanly and the run it fixed is no longer wedged. It wraps `POST /v0/runs/{run_id}/vouch-commit`.

Inputs:

| Field | Required | Notes |
|---|---|---|
| `run_id` | **yes** | The run whose branch carries the commit. |
| `sha` | **yes** | The commit SHA to declare as run-authored lineage. Empty is refused (`validation_failed`, 400; caught locally before the HTTP hop). |
| `reason` | **yes** | Operator rationale, recorded verbatim on the `operator_commit_vouched` audit entry. Empty is refused (`validation_failed`, 400). |

Safety (server-enforced):

- **Fail-closed preserved.** The handler records the declaration verbatim — it does **not** verify the SHA exists on the branch. Vouching a wrong/non-existent SHA un-wedges nothing; an UN-vouched foreign commit still fails category-B at the report boundary and still blocks merge resolution.
- **Operator-token-only.** Requires `write:stages`. A run-bound token (subject `mcp:run:<uuid>`) is **rejected outright** (`run_token_forbidden`, 403) — even for its own run — because an agent self-declaring lineage for a commit on its own branch would defeat the ADR-035 sole-writer invariant. Mirrors the `fishhawk_decide_scope_amendment` run-bound rejection.

Returns the recorded declaration (`run_id`, `vouched_sha`, `reason`) on success.

## One-verb operator merge (`fishhawk_merge_run`)

`fishhawk_merge_run` ([E48.7 / #1954](https://github.com/kuhlman-labs/fishhawk/issues/1954)) takes a **gate-approved run from verdict to merged+terminal in one verb**, replacing the four-step hand ceremony (approve the PR → merge it → run post-merge). Per the 2026-07-15 design decision (option a), the **PR-approval review itself stays a `gh` step under the operator's own GitHub identity** (`gh pr review --approve`); App-identity approval is deferred to E39. It wraps `POST /v0/runs/{run_id}/merge`.

The endpoint records the operator merge verdict as a chained `merge_verdict_recorded` audit entry (modeled on the `operator_commit_vouched` vouch handler) and queues the squash merge through the **same `GitHubMerger` seam** the delegated `may_merge` arm of `AutoDriveRunGate` uses — so `drive_run`'s merge act (which routes through `POST /v0/runs/{run_id}/auto-drive`) converges on the same path by construction. Because GitHub's `enablePullRequestAutoMerge` mutation errors on an already-merge-ready PR (clean status — the common flow after a `gh` approval with checks green), the backend `githubAutoMerger` gains a REST squash-merge fallback.

The tool then **awaits the webhook-settled terminal run state** using the `fishhawk_await_audit` polling idiom: it polls the `pr_merged` / `post_merge_observed` audit categories anchored past the verdict row's sequence, with the ADR-036 run-terminal backstop each tick. There is **no persisted `merged` run state** — terminal-on-merge is `succeeded` and `awaiting_merge` is a presentation-only drive surface — so the await keys on those categories plus the run-terminal backstop, never on a state string.

Inputs:

| Field | Required | Notes |
|---|---|---|
| `run_id` | **yes** | The gate-approved run to merge. It must carry a PR URL and must not be failed/cancelled (fast local refusal before the POST; the backend re-validates authoritatively, including the acceptance gate). |
| `verdict` | **yes** | Your operator merge verdict, recorded verbatim on the `merge_verdict_recorded` audit entry. Empty is refused locally before the HTTP hop. |
| `timeout_seconds` | no | Bounds the terminal await (default 360, cap 600 — same clamp as `await_audit`). |

Idempotence (**endpoint-side, #1954**):

- The tool **always re-POSTs on resume with NO client-side skip.** The ENDPOINT is idempotent: a repeated POST that finds an existing `merge_verdict_recorded` row appends no duplicate, responds `already_recorded:true`, and **still dispatches the merge helper**. So a timed-out re-invoke or a 502 (`merge_dispatch_failed`, the verdict row durable) re-queues the merge with no duplicate verdict row — closing the 502-retry hole a client-side skip would re-open.

Statuses: `merged` (a `pr_merged` / `post_merge_observed` entry landed past the anchor; `next_action` carries the operator post-merge dev-host step, **surfaced not invoked** — ADR-038), `timeout` (resumable — re-invoke; the endpoint's idempotence makes the re-POST safe), `run_terminal` (the run reached failed/cancelled while waiting — the merge will most likely never settle; check `fishhawk_get_run_status`).

A **write** tool needing `write:approvals`; a run-bound agent token is rejected (`run_token_forbidden`, 403). `next_actions`' merge-ritual states (`succeeded_pr_open`, the acceptance-skipped/passed states, and the drive-folded `awaiting_merge`) now emit `approve_pr` then `fishhawk_merge_run`, replacing the bare `merge_pr` + `post_merge` steps.

## Scope amendment at approval (`fishhawk_approve_plan` → `add_scope_files` / `remove_scope_files`)

`fishhawk_approve_plan` (E22.4 / [#393](https://github.com/kuhlman-labs/fishhawk/issues/393)) takes an optional `add_scope_files` array ([#824](https://github.com/kuhlman-labs/fishhawk/issues/824)) — the **structured, authoritative** way to add files to the implement stage's `scope.files` at approval time. On approve the named paths are recorded on the approval audit payload and folded into the implement stage's effective scope by the prompt builder, so a reviewer-authorized edit ships as a declared path rather than surfacing as benign `scope_drift`.

Prefer it over naming paths in the free-text `reason`. The `reason` fold ([#730](https://github.com/kuhlman-labs/fishhawk/issues/730)) is a best-effort regex scrape kept only as a fallback; it silently misses:

- **directories** — pass a trailing slash (e.g. `pkg/testdata/corpus/`); every created file under that prefix stages.
- **extensionless and repo-root files** — e.g. `go.work`, `Makefile`.
- **described-but-not-spelled paths** — anything the prose names in words rather than as a literal path token.
- **absolute / non-repo-relative tokens** — the fold now silently skips any token that is absolute (leading `/`) or contains a `..` traversal segment ([#1155](https://github.com/kuhlman-labs/fishhawk/issues/1155)), so naming a `/tmp` path or an exclusion in prose no longer injects a phantom scope entry. Only clean repo-relative paths fold; use `add_scope_files` for an authoritative add.

`reason` and `add_scope_files` compose: the structured paths fold first (authoritative), then the prose fold runs as a fallback, both deduping by path. Both no-op when the plan declares an empty scope, preserving the runner's `git add -A` fallback. `add_scope_files` does **not** weaken the policy gate — a folded path that matches `forbidden_paths` still fails category-B against the produced diff.

**Removing and replacing scope paths ([#1726](https://github.com/kuhlman-labs/fishhawk/issues/1726)).** `fishhawk_approve_plan` also takes an optional `remove_scope_files` array — the **inverse** of `add_scope_files`. On approve the named paths are **subtracted** from the implement stage's effective `scope.files` by the prompt builder, so every runner gate (created-out-of-scope, commit-in-scope, category-B) and the scope-cap gate honor the removal, and it applies to per-slice scope on decomposed plans via the same parent fan-out fallback `add_scope_files` uses. It is recorded on the `approval_submitted` audit payload alongside `remove_scope_files` plus `scope_files_before` / `scope_files_after` (the deduped effective-scope file lists). The removed path is also surfaced in the implement prompt text (an "Operator-removed scope files" section) telling the agent it is no longer in scope, since `writeApprovedPlan` still renders the immutable plan artifact's `scope.files`.

- **Replace = remove + add in one call.** There is no separate replace field: pass `remove_scope_files` AND `add_scope_files` in the same approve to swap paths (remove old + add new) at the plan gate with zero planner invocations — composable and consistent with the additive path. This is how an over-cap plan is reconciled entirely at the gate.
- **Validation / skip rules (fail-closed).** Each `remove_scope_files` path is refused `400 validation_failed` (`field: remove_scope_files`), before any approval row is inserted (a corrected retry flows normally), when it is: **not repo-relative** (a leading `/` or a `..` traversal segment — same containment contract `add_scope_files` skips on); **absent from the current effective scope** (plan `scope.files` ∪ prior folds ∪ approved amendments ∪ this call's `add_scope_files`) — this catches operator typos rather than silently no-op'ing; or a removal that **would empty a non-empty effective scope** — an empty scope re-enables the runner's `git add -A` fallback and disables scope enforcement, so keep at least one path or re-plan. Omitting the field is byte-identical to today. Honored only on `approve`; ignored on `reject`.

**Duplicate-submission labeling ([#986](https://github.com/kuhlman-labs/fishhawk/issues/986)).** A re-submission by the same subject — `fishhawk_approve_plan` or `fishhawk_reject_plan` against a stage that subject already decided — is a no-op the tools label explicitly instead of rendering as a normal result: the output carries `duplicate_submission: true` plus `prior_decision` (the existing row's), and the result text leads with a banner stating the prior decision stands, the stage state is unchanged, and the budget/scope gates were NOT re-run. The override markers (`--override-budget` / `--override-scope-cap`) are honored because both gates now run **pre-insert**: a 422 refusal records no approval row, leaving the submission slot free for the override retry.

**Scope-cap gate ([#983](https://github.com/kuhlman-labs/fishhawk/issues/983)).** A plan-stage approve is refused `422 plan_violates_scope_cap` when the effective scope — plan `scope.files` ∪ `add_scope_files` ∪ approved amendments, **minus `remove_scope_files`** ([#1726](https://github.com/kuhlman-labs/fishhawk/issues/1726)), deduped by exact path — exceeds the implement stage's `max_files_changed`. Because removals are subtracted first, an over-cap plan can be reconciled at the gate by removing or replacing paths. The refusal inserts no approval row, so a retry after re-scoping flows normally; to force it through (declared scope is an upper bound, and the cap may legitimately be about to change), include the `--override-scope-cap` marker in the comment, which records a `plan_scope_cap_override_acknowledged` audit entry — the same posture as `--override-budget`. Read headroom before approving: `fishhawk_get_plan`'s `scope_precheck` now carries `max_files_changed` alongside `scanned_files`.

**Binding assertions ([#1171](https://github.com/kuhlman-labs/fishhawk/issues/1171)).** `fishhawk_approve_plan` also takes an optional `binding_assertions` array — the **machine-checkable** half of an approval condition, the deterministic complement to the free-text `reason` fold. Where `reason` is restated to the implement agent as binding conditions (#558) and `add_scope_files` widens the scope, `binding_assertions` declares checks the runner enforces: each entry is `{type, path, literal}` where `type` is `file_contains` or `test_asserts` (open enum), `path` is repo-relative (and must end in `_test.go` for `test_asserts`), and `literal` is a non-empty substring that must appear in the committed file. On approve they are recorded on the approval audit payload alongside `add_scope_files` and echoed on the implement prompt-response; the runner evaluates each as a deterministic substring check against the committed scope-only tree post-implement, and any unsatisfied assertion fails the implement stage category-B. Substring matching only — never parses prose, so a literal chosen too loosely is an operator-declaration concern, not a gate defect. A malformed declaration (unknown `type`, empty `literal`, a `test_asserts` path not ending in `_test.go`) is rejected `400 validation_failed` before any approval row is recorded. Omitting the field is byte-identical to today.

**Implement-model override ([#1013](https://github.com/kuhlman-labs/fishhawk/issues/1013)).** `fishhawk_approve_plan` also takes an optional `implement_model` string — the operator's override for the implement-stage model, the top rung of the resolution ladder `deployment default < spec executor.model < plan model_recommendation < this override`. On a plan-stage approve the backend resolves the full ladder with this as the operator rung, validates the **resolved** value against the deployment's per-adapter allow-list, and records the choice as a `model_resolved` audit entry (`{model, model_source}`) that the runner spawn routes to the agent's `--model`. An unknown resolved model — from any rung, not just this override — is rejected `422 plan_invalid_model` (details carry `model`, `model_source`, `adapter`), pre-insert so a retry with an allowed `implement_model` flows normally. An empty/unconfigured allow-list fails **open** (any model accepted, byte-identical to today). Omit the field to ratify the plan's `model_recommendation` or fall through to the spec/deployment default; an empty resolution still records `model_resolved` and spawns with no `--model`, exactly as today.

The OpenAPI surface (`docs/api/v0.openapi.yaml`) and its companion `docs/api/v0.md` remain the authoritative parameter reference.

## Mid-stage scope amendments (`fishhawk_list_scope_amendments`, `fishhawk_decide_scope_amendment`)

E22.X / [#961](https://github.com/kuhlman-labs/fishhawk/issues/961) adds the **mid-stage** complement to approval-time `add_scope_files`: while the implement stage is RUNNING, the agent can request that specific paths be folded into the effective `scope.files` instead of silently dropping a coupled edit (the runner omits undeclared edits from the commit; an undeclared created file fails category-B, #818/#825).

**Agent protocol (poll-based, no push channel in v0).** The implement prompt instructs the agent to `POST /v0/runs/{run_id}/scope-amendments` with its run-bound `FISHHAWK_API_TOKEN` (`{paths: [{path, operation: modify|create}], reason}`), then poll the GET (same bearer, `mcp:read`) every 15–30s until the request leaves `pending`, working on in-scope files meanwhile and giving up after ~5 minutes. Cap: **2 requests per stage**, counted server-side on rows — a denied request still consumes budget. The agent must never edit/create a requested file before the approval lands.

**Operator loop:**

1. Await the request: `fishhawk_await_audit` anchored on category `scope_amendment_requested` (#977). The entry payload carries `{amendment_id, paths, reason, remaining_budget}`.
2. Inspect: `fishhawk_list_scope_amendments {run_id}` — paths, per-path operation, the agent's reason, status.
3. Decide: `fishhawk_decide_scope_amendment {run_id, amendment_id, decision: approve|deny, reason}`. Decide promptly — the agent's poll is bounded.

**Scope-cap headroom ([#983](https://github.com/kuhlman-labs/fishhawk/issues/983)).** When the implement stage has a `max_files_changed` cap, pending items in the list (and the request/decision responses) carry `effective_scope_files_after_approval` + `max_files_changed`, and both tools print an explicit `WARNING` line when approving would put the effective scope over the cap. Warn-only by design: an over-cap approve still succeeds — mid-stage amendments are often forced, and the post-implement gate plus the now-informed operator own the verdict. Fields are absent on older backends or when no cap is configured.

**Auth.** The decision is operator-only (`write:stages`); the backend rejects run-bound agent tokens outright (`self_decision`), so the requesting agent can never approve its own request. The agent-side POST requires the implement-stage token's `write:scope-amendments` scope (granted unconditionally at token issue for implement stages); the GET admits the run-bound token (`mcp:read`, own run only — cross-run is 403) or any operator bearer/session.

**Activation.** Approved paths fold into the effective scope at BOTH ends: the backend prompt fetch (`source "scope-amendment"`, so a stage restart or fix-up carries the amended scope) and the runner's pre-commit refresh, which re-reads the GET with the same run-bound token and folds approved paths BEFORE the committed-tree verify gates and `StageScoped` — preserving the #960 invariant that the gates verify the same folded tree that is pushed. Anything NOT requested still fails loud. Both `scope_amendment_requested` and `scope_amendment_decided` are internal audit kinds, not issue-comment surfaces.

## Scope-completeness park (`fishhawk_decide_scope_completeness`)

E22.X / [#1231](https://github.com/kuhlman-labs/fishhawk/issues/1231) adds a **zero-re-run** recovery for the case the [#1229](https://github.com/kuhlman-labs/fishhawk/issues/1229) one-re-run exempt lever otherwise served: an implement stage whose **only** committed-tree gate failure is the [#1151](https://github.com/kuhlman-labs/fishhawk/issues/1151) scope-completeness "missing declared scope file(s)" check, while the committed tree otherwise passed verify (created-out-of-scope, binding-assertion, compile/test, and verified-tree gates all green).

**Park, not category-B.** Instead of fail-and-restore, the runner pushes the **gate-verified commit** to the run branch (no PR opened — ADR-035 sole-writer preserved: the run writes its own branch) and PARKS the implement stage in a new `awaiting_scope_decision` state, carrying the held commit SHA, run branch, verified tree SHA, and the missing declared paths. The park leaves the gate waiting for an in-band operator decision over the [#1232](https://github.com/kuhlman-labs/fishhawk/issues/1232)/[#1235](https://github.com/kuhlman-labs/fishhawk/issues/1235) non-blocking dispatch substrate. Any compound failure (missing **plus** another gate) keeps today's category-B unchanged.

**Operator loop:**

1. Observe the park: `fishhawk_get_run_status` surfaces the `implement_awaiting_scope_decision` next action; `fishhawk_list_audit` on category `scope_completeness_parked` carries the missing paths + held SHA.
2. Decide: `fishhawk_decide_scope_completeness {run_id, decision: exempt|fail, reason}`.
   - `exempt` — the backend opens the PR from the **exact held commit** with **NO agent re-invocation** (the already-committed tree is accepted as-is; the implement-review gate proceeds). Appends `scope_completeness_exempted`.
   - `fail` — the stage falls through to today's category-B fail-and-restore. Appends `scope_completeness_failed`.

**Auth.** Operator-only (`write:stages`); the backend rejects run-bound agent tokens (`run_token_forbidden`), so the agent whose stage parked can never decide its own park (mirrors `fishhawk_decide_scope_amendment`). `reason` is required and non-empty; an invalid `decision` (anything but `exempt`/`fail`) or empty `reason` is caught before the HTTP hop. The endpoint returns 409 (`scope_completeness_not_parked`) when the stage is not parked in `awaiting_scope_decision`. It wraps `POST /v0/runs/{run_id}/scope-completeness/decision`.

`scope_completeness_parked`, `scope_completeness_exempted`, and `scope_completeness_failed` are internal audit kinds, not issue-comment surfaces.

## Category-B recovery (`fishhawk_resume_run`)

E22.X / [#978](https://github.com/kuhlman-labs/fishhawk/issues/978) adds operator-initiated recovery for a run whose implement stage failed **category-B** (scope/constraint violation) after its plan was approved — the gap between `fishhawk_retry_stage` (refuses B) and `fishhawk_start_run` (replans from scratch). The tool wraps `POST /v0/runs/{run_id}/recover` and mints a **new plan-stage-less child run** that re-executes against the parent's approved plan.

Pointed at a failed decomposition **child** run instead, it re-drives that child **in place** on the shared parent
branch (#1081) — not a new run. Both arms fold operator-named `add_scope_files` as a pre-approved #961 amendment.

Inputs: `parent_run_id` (the failed run), optional `add_scope_files` (`[{path, operation: modify|create}]`, operation defaults to `modify`), optional `reason`, `budget_override`, and `idempotency_key` (same replay semantics as `fishhawk_start_run`).

- **Eligibility**: parent's plan stage `succeeded` AND implement stage `failed` category-B; anything else returns `recovery_not_eligible` naming which leg failed. Parents without a cached workflow spec return `recovery_unsupported` — start a fresh run.
- **Plan reuse**: the child carries `parent_run_id`; `fishhawk_get_plan` and the prompt builder resolve the parent's plan via the existing parent walk. The parent's binding approval conditions and approval-time `add_scope_files` are inherited too.
- **Scope amendments**: operator-named `add_scope_files` land as a **pre-approved** #961 amendment row on the child's implement stage — visible via `fishhawk_list_scope_amendments`, folded by the prompt fetch and the runner's pre-commit refresh; `operation: create` entries flow into the #818/#825 net-new-file gates.
- **Budget**: `retry_attempt` is carried UNCHANGED — recovery never consumes the `on_ci_failure` auto-retry cap. Provenance lands as a `plan_reused_from` audit entry on the child (internal audit kind, not an issue-comment surface).

Drive the child like any local run: `fishhawk_run_stage` executes the implement stage directly — no plan stage exists, no plan approval is needed.

## Failed-run revive (`fishhawk_revive_run`)

`fishhawk_revive_run` (E22.X / [#1915](https://github.com/kuhlman-labs/fishhawk/issues/1915)) is the **single operator verb** that re-admits a terminal-**FAILED** run for another turn, replacing the old retry-without-dispatch dance (retry each failed stage, then hand-park the rest). It wraps `POST /v0/runs/{run_id}/revive`.

The backend **pre-validates** that **every** failed stage is retryable, then re-parks each in its correct gate-ordered pre-dispatch state (A/C → `pending`, D SLA-timeout → `awaiting_approval`, decomposed-parent implement → `awaiting_children`) and flips the run **failed → running**. A single non-retryable failed stage (category-B, D-rejected, or a stage with no recorded category) refuses the **whole** revive with `422 revive_not_applicable` naming the blocking stage — **no partial mutation**.

The load-bearing distinction from `fishhawk_retry_stage`: revive **re-parks only** — it performs **NO** orchestrator handoff and **never dispatches**. A re-parked stage sits in its pre-dispatch state until you dispatch it at its proper gate turn via the existing verbs (`fishhawk_dispatch_stage` / `fishhawk_run_stage` on the local runner), so the [#1700](https://github.com/kuhlman-labs/fishhawk/issues/1700) wrong-order re-dispatch corruption is structurally impossible. `fishhawk_retry_stage`, by contrast, re-opens **one** stage and auto-dispatches it. Reach for revive when a sibling stage's failure flipped the run terminal while a healthy stage's review is still settling and you want a safe batch re-park; reach for retry when you want one stage re-run immediately. Each re-park consumes that stage's per-stage retry budget exactly like a retry — revive is a batch retry-shaped re-open, not a budget bypass.

- **Input**: `run_id` (the terminal-FAILED run).
- **Auth**: operator-only. The backend requires `write:stages` **or** `write:retries` and rejects any run-bound agent (`mcp:run:*`) token outright (`403 agent_token_forbidden`).
- **Returns**: the re-opened run (now `running`), the per-stage re-park summary (`restored_stages` — each carrying id / type / prior failure category+reason / restored state), and a `next_step` hint that dispatch happens at each stage's proper gate turn.
- **Errors** propagated as tool errors: invalid UUID (caught before the HTTP hop), `agent_token_forbidden` (403), `insufficient_scope` (403), `run_not_found` (404), `revive_not_applicable` (422), `revive_unconfigured` (503).

The `next_actions` failed-run arms (`implement_failed_category_a` and the default `implement_failed`) surface `fishhawk_revive_run` alongside `fishhawk_retry_stage`.

## Clarification answer-and-resume (`fishhawk_answer_clarification`)

`fishhawk_answer_clarification` (E22.X / [#1088](https://github.com/kuhlman-labs/fishhawk/issues/1088), the [#1057](https://github.com/kuhlman-labs/fishhawk/issues/1057) answer-and-resume seam) answers the questions a planner parked at `awaiting_input` so its plan stage can resume. When an issue is not yet plannable the planner parks the plan stage at `awaiting_input` with a `clarification_request` ([#1080](https://github.com/kuhlman-labs/fishhawk/issues/1080)) instead of producing a plan; the run is stranded until the operator answers. This tool wraps `POST /v0/stages/{stage_id}/clarification`.

Inputs:

| Field | Required | Notes |
|---|---|---|
| `run_id` | **yes** | The run whose plan stage parked at `awaiting_input`. The tool resolves the plan stage internally — no stage id needed. |
| `answers` | **yes** | One `{id, answer}` per parked question, keyed by the question id from the `clarification_requested` audit entry (read it via `fishhawk_get_run_status` / `fishhawk_list_audit`). At least one; every parked question needs exactly one answer, and an unknown/missing/duplicate id is rejected. |
| `comment` | no | Free-text note appended after the answers in the binding conditions delivered to the resumed plan agent. |

What it does:

- The answers are persisted as a **dedicated `clarification_answered` audit entry** — **not** an approval (the plan is not yet approved), so the `approval_submitted`/`decision=approve` channel `loadApprovalConditions` reads stays isolated. The plan-stage prompt loads them into the resumed agent's binding conditions.
- The **same** plan stage re-opens (`awaiting_input → pending`) in the **same** run — no new run, no duplicate reviews (distinct from `fishhawk_resume_run`, which mints a child run). On a `github_actions`/drive run the backend re-dispatches the plan stage; on a local run, re-run it with `fishhawk_run_stage plan` after this returns.
- **Auth:** a write tool requiring `write:approvals` (the [#558](https://github.com/kuhlman-labs/fishhawk/issues/558) gate-answer family).

Error surfaces propagated as tool errors: `validation_failed` (400 — empty answers / unknown fields; the empty case is also caught locally before the HTTP hop), `clarification_answer_invalid` (400 — an answer id is unknown, missing, or duplicated relative to the parked questions), `stage_not_found` (404), `invalid_state_transition` (409 — the resolved stage is not a plan stage parked at `awaiting_input`). The `next_actions` `plan_awaiting_input` arm points here.

## Work-item filing (`fishhawk_file_issue`, [#1005](https://github.com/kuhlman-labs/fishhawk/issues/1005))

`fishhawk_file_issue` files a work item — issue, bug, chore, ADR — through the repo's **work-management conventions** rather than calling the tracker's API directly. It is both the consistent cross-repo/cross-platform filing surface (the conventions are the value: one call shape works against a GitHub-Projects-configured repo or a Jira-configured one — only the per-repo conventions differ) and the operator-agent follow-up-filing path ([ADR-040](https://github.com/kuhlman-labs/fishhawk/issues/1004)): the operator agent files deferred-work tickets through it instead of by hand. It wraps `POST /v0/work-items`.

The backend loads the repo's conventions, renders the title from the type's `title_format`, assembles the body from the type's skeleton + caller `sections` (or takes `body` verbatim), merges `default_labels` with explicit `labels`, resolves board placement / complexity / ADR numbering, links the relations, and dispatches to the registered provider (GitHub Projects in v0).

Inputs:

| Field | Required | Notes |
|---|---|---|
| `type` | **yes** | Work-item type; a key in the repo's conventions (e.g. `feature`, `bug`, `chore`, `adr`). |
| `summary` | **yes** | The mandatory one-liner: fills the `{summary}` title placeholder and is the required Summary field. |
| `repo` | falls back to env | Target repo as `owner/name`; defaults to `GITHUB_REPOSITORY` when omitted (the in-runner case). |
| `body` | no | Verbatim body; when omitted the body is assembled from the type's skeleton + `sections`. |
| `sections` / `title_vars` | no | Per-skeleton-section content and extra title placeholders (e.g. `epic`, `n`). An unresolved title placeholder fails the filing. For a child type whose `title_format` is `[E{epic}.{n}]`, BOTH `{epic}` AND the `{n}` child number are auto-derived from the `parent_epic` relation server-side (#1958), so `title_vars` can be omitted entirely — supply `n` only to override the auto-derived child number. |
| `labels` / `complexity` / `status` | no | Merged on / overriding the type's defaults; `complexity` must be a declared level. |
| `relations` | no | `{parent_epic, supersedes[], companion_to[], evidence_runs[], depends_on[]}` — resolved into the provider's link operations. `depends_on` is the issue-level dependency edge (issue refs among the epic's children) a campaign reads to assemble its wave DAG (ADR-047); it is persisted as a `Depends on: #X, #Y` body marker and validated format-only at file time (cycle/existence checks deferred to campaign-assembly time). |
| `existing_numbers` | no | Numbers already in use for a numbered type (e.g. `adr`), so the next sequential number can be allocated. |
| `run_id` | falls back to env | Optional in-flight run UUID; defaults to `FISHHAWK_RUN_ID`. When set and non-terminal a best-effort `work_item_filed` audit entry is appended to it. |

Audit-on-active-run is **best-effort**: filing still succeeds with no run in flight, and the response's `audited` flag reports whether an entry was written. Returns the created item — `type`, `title`, `number`, `url`, `provider`, the resolved `applied_labels` / `complexity` / `status` / `board_column`, and `audited`.

**Auth:** a write tool — the backend requires an authenticated caller (anonymous requests are rejected). Error surfaces propagated as tool errors: `validation_failed` (400 — repo not `owner/name`, missing `type`/`summary`, unknown fields; the empty `type`/`summary`/`repo` cases are also caught locally before the HTTP hop), `authentication_required` (401), `work_item_invalid` (422 — the request violates the type's conventions), `provider_unimplemented` (501 — the configured provider id is not registered, e.g. the interface-only `jira`; details name it), `work_item_filing_failed` (502 — the provider rejected the filing). The CLI mirror is `fishhawk file-issue`.

## Product feedback (`fishhawk_report_product_issue`, [#1006](https://github.com/kuhlman-labs/fishhawk/issues/1006))

`fishhawk_report_product_issue` files an upstream **Fishhawk product** bug or feature request — when you hit friction with Fishhawk itself, not the repo you're working in — carrying an auto-collected diagnostic bundle. It wraps `POST /v0/runs/{run_id}/product-reports`. The destination is the **fixed** upstream product repo; it is not caller-controlled. The backend collects the run's product-facts bundle, fingerprints the failure `(error code, failing surface, failure detail class, version family)` — the detail class is a closed-enum normalization of the failure reason (`auth-401` | `bad-object-ref` | `target-unreachable`) that splits distinct root causes sharing a surface (#1962), included only when classified — searches the product repo for an open report already carrying that fingerprint marker, and either appends an occurrence comment (dedup hit — nothing new is created) or files a new fingerprint-marked report (dedup miss). A source-side `product_report_filed` audit entry records what left the boundary.

**The redaction boundary is the hard contract.** By default the report carries **product-level facts only** — no diffs, paths, prompts, or free text. Operator free text (`description`) crosses the boundary **only** when `include_free_text=true`, and even then it is run through the backend's secret-redaction machinery first. Treat `include_free_text` as the operator's explicit consent; it defaults off.

Inputs:

| Field | Required | Notes |
|---|---|---|
| `run_id` | falls back to env | The run whose product-facts bundle to attach; defaults to `FISHHAWK_RUN_ID` (the in-runner case). |
| `kind` | no | `bug` (default — attaches the diagnostic bundle) or `feature` (an enhancement request; lighter workflow context). |
| `description` | no | Operator free text. Crosses the boundary **only** with `include_free_text=true`, redacted server-side first; otherwise ignored. |
| `include_free_text` | no | Explicit consent: when true, `description` crosses **after** server-side redaction. Default false. |

Returns the egress outcome (`report.action` `created`\|`occurrence`, `fingerprint`, upstream `number`/`url`, `destination`), a transparency preview of the product facts that were attached (`diagnostics`), and `free_text_included`.

**Auth:** the first **write** tool that drives an egress on the run's chain — the backend requires the run's **own** run-bound agent token (an operator token or a foreign run's token is rejected with `run_not_entitled`). Error surfaces propagated as tool errors: `validation_failed` (400), `authentication_required` (401), `run_not_entitled` (403 — only the run's own run-bound token may file), `product_feedback_disabled` (403 — the per-repo kill-switch), `run_not_found` (404), `provider_unimplemented` (501), `product_report_failed` (502). The CLI mirror is `fishhawk report-issue`.

## Auth split (runner `fhm_*` vs operator `fhk_*` tokens)

Runner-side `fhm_*` tokens carry the `mcp:read` scope only: a write tool called from the runner side hits the bearer
middleware, resolves a read-only identity, and the handler-side role check returns 403 — the SDK surfaces it as a tool
error, no code change needed. Operator-side `fhk_*` apitokens carry the `write:runs` / `write:approvals` /
`write:stages` scopes.

## Schema-reflection trap (#371)

The `github.com/modelcontextprotocol/go-sdk/mcp` SDK (v1.6.1) auto-generates output schemas via Go reflection over the
tool's return type. `uuid.UUID` (which is `[16]byte`) renders as `type: array`, and `json.RawMessage` (which is
`[]byte`) does the same — but on the wire UUIDs are strings and per-category audit payloads are JSON objects, so schema
validation rejects every response. `client.go` therefore types UUIDs as `string` and `Artifact.Content` /
`AuditEntry.Payload` as `any`; callers `uuid.Parse` at the API-client boundary, and the plan decoder re-marshals +
unmarshals into the typed `PlanContent`.

## Cross-component E2E test (#371)

`backend/internal/integration/mcp/e2e_test.go` spins up Postgres + the real backend HTTP server, then builds + spawns
the actual `fishhawk-mcp` binary as a subprocess, fetches an `fhm_` token via the runner's HTTP shape, and exercises a
tool call end-to-end. The revocation + malformed-token assertions assert at the `mcptoken.Repository.Authenticate`
layer rather than at the tool call, because v0 reads don't enforce identity — the bearer middleware falls through to
anonymous; per-handler enforcement is out of scope for v0.

## `fishhawk_start_run` field parity (#426)

The start_run tool accepts the same local-runner convenience inputs the CLI's `fishhawk run start` does:

- `working_dir` walks for `.fishhawk/workflows.yaml` and ships the bytes inline (#411).
- `issue` shells to `gh issue view` and caches the payload (#415).
- `runner_kind=local` tags the run for the local-runner backend (ADR-022 / #388).

Spec discovery / gh fetch live in `spec_discover.go` / `issue_fetch.go` — **local copies** of the CLI's helpers rather
than a shared package (the cli → backend import direction precludes the inverse). Auto-flip rule: when `trigger_source`
is defaulted (empty) and an issue resolves via `issue` or `trigger_ref=issue:N`, the MCP server flips it to
`github_issue`; an explicit `trigger_source` is preserved.

## Review-status internals (#600, count-gated #1127)

Each `ReviewStatus{Stage, Status, Reviews[], PollIntervalSeconds}` (`Status` one of `none` | `pending` | `complete` |
`skipped` | `failed`) is derived **entirely from the audit trail**.

**Completeness is count-gated (#1127).** The round is terminal only once `landed_terminal >= configured_agents` (the
latest `*_review_started` entry's `ConfiguredAgents`; ANY terminal kind — `reviewed`/`review_failed`/`review_skipped` —
counts). That is the SAME rule `checkPlanReviewSettled`/`checkImplementReviewSettled` use for the approval/merge gates,
so a poll catching the heterogeneous partial-landing window (reviewers run sequentially, each minutes long) reports
`pending` rather than `complete` with only the first reviewer's verdict. Once the count threshold is met, precedence
resolves the status (`reviewed → complete`, else `review_skipped → skipped`, else `review_failed → failed`) and
`Reviews[]` is the UNION of every decoded terminal row — one per configured reviewer. Below the threshold (or with no
terminal entry) the status is `review_started → pending`, else `none`. An absent/non-positive `ConfiguredAgents`
(old/malformed started payload) degrades to the prior complete-on-first-verdict predicate so the surface never strands
on `pending`. The `pending` state is the gap the existing `Reviews[]`/`ImplementReviews[]` slices couldn't express — it
subsumes a still-running review, a silently-failed/timed-out one, and the partial-landing window; those fields stay
populated unchanged (additive, no driver regression).

**Poll-authoritative contract (#879).** The 15s `poll_interval_seconds` hint is populated ONLY on `pending` (the one
state worth re-polling) and rides in via the shared `ReviewStatus` to every poll surface (`fishhawk_get_run_status`,
`fishhawk_get_plan`) from one edit.

**`fishhawk_await_review` internals.** It polls the existing `GET /v0/runs/{id}/audit` endpoint server-side on an
injectable interval (no new backend long-poll, no wall-clock sleeps in tests) until a terminal entry lands, the run
itself reaches a terminal state (the ADR-036 #874 non-stranding backstop — an inline `succeeded`/`failed`/`cancelled`
comparison against the fishhawk-mcp-local `Run.State`, so a verdict that will never land never holds the session open),
or the timeout fires. A `pending`-on-timeout result carries the `poll_interval_seconds` hint plus an actionable message
framing the re-call (or a switch to `get_run_status` polling) as the documented next step and naming
`FISHHAWKD_PLAN_REVIEW_TIMEOUT`. A 360s synchronous call may still hit a client/transport per-call timeout — acceptable
precisely because poll-the-handle is the blessed primary path and a cut-short await is a no-op the caller can re-issue.
Native MCP Tasks (`invocationMode:async`) is a deferred follow-up — the pinned go-sdk has no Tasks symbols; this ships
the sync/poll fallback only. The #894 fix-up-boundary floor lives in `reviewStatusFor`. Implementation: `review.go`
(`reviewStatusFor`, the shared `decodeReviewVerdicts`/`decodeSkippedReviews`, the `awaitReview` handler).

## Await-audit internals (#962)

`fishhawk_await_audit`'s `run_terminal` status is distinct from `timeout` because some categories land at/after the
terminal transition, so the backstop resolves the wait only after ONE final anchored read that must win. Beneath it,
`GET /v0/runs/{id}/audit` gained an additive `since_sequence` query param — a strictly-greater filter applied before
pagination, in-memory like the #215 `stage_id` filter (`handleListRunAudit` in `backend/internal/server/reads.go`).
Implementation: `await_audit.go` (`awaitAudit`, `nextAuditEntry`, `awaitAuditRunTerminalBackstop`), reusing
`clampAwaitTimeout` / `reviewPollInterval` / `runStateIsTerminal`; the cross-boundary seam test is
`backend/internal/integration/mcp/await_audit_test.go`.

## Stage-wait internals (ADR-037 / #880; local dispatch default #1247)

Implementation: `stage_wait.go` (`stageStateIsTerminal`, `classifyStageWaitStatus`, `stageWaitStatusFor`) — a LOCAL
terminal classifier that does NOT import `backend/internal/run`, mirroring `review.go`'s `runStateIsTerminal`. Callers
pass the already-fetched stage slice, so no extra `ListRunStages` round-trip is issued. `fishhawk_run_stage` adds
`stage_wait_status` on its post-run output.

**`next_actions` defaults a parked LOCAL implement stage to `fishhawk_dispatch_stage`** (with `fishhawk_run_stage`
demoted to an explicit blocking opt-in second entry), because the implement stage is the one stage type that can file a
mid-stage amendment a blocking call cannot decide in-band; the plan-local branch (no amendments) keeps the single
`run_stage` action and the `github_actions` poll branch is unchanged (#1247).

## `fishhawk_run_stage` internals and cancellation (ADR-024 / #434, runner half #435, compact #647)

The tool mirrors the CLI's `fishhawk runner start` argv composition; stdout is parsed line-by-line as JSONL and either
streamed as `notifications/progress` (when the client supplied a progress token) or accumulated for the final tool
result. The audit log carries the durable record.

`summarizeRunStageEvents` (the #647 compaction) walks the accumulated events once. `outcome`/`tokens_used` come from
the terminal `{"event":"runner_completed","outcome":…,"tokens_used":N}` event — the only runner-level terminal event
relayed on the JSONL **stderr** stream the relay reads; the bundle-only `kind=="invocation_end"` is deliberately NOT
keyed on, as it never reaches this stream. All summary scalars are `omitempty` so they reflect as plain JSON scalars
(no #371 array-reflection regression).

**Cancellation.** Tool-context cancellation sends `SIGTERM`, waits `runStageGracePeriod` (default 30s), then escalates
to `SIGKILL`. The runner-side half (#435) lives in `runner/cmd/fishhawk-runner/main.go::newRunnerContext` —
`signal.NotifyContext` registers SIGINT + SIGTERM, and the deferred cancel-emit at the top of `run()` overrides the
exit code to 130 (`exitCancelled`) and writes a `runner_cancelled` JSONL line so the MCP tool's progress stream sees a
clean terminator. The plumbed `ctx` reaches the long-running calls (`Invoke`, `IssueKey`, `FetchPrompt`, `ShipTrace`,
`ShipPlan`, `FetchMCPToken`, `openPRAndShipArtifact`) so cooperative cancellation works upstream and partial-trace
bytes ship best-effort when the cancel lands during agent invocation.

**Binary resolution:** `runner_binary` input > `FISHHAWK_RUNNER_BIN` env > `exec.LookPath("fishhawk-runner")` — matches
the CLI's resolver. The tool is registered unconditionally on every `fishhawk-mcp` deployment and returns a clean error
when the binary can't resolve; ADR-024 Q5 defers splitting into a `fishhawk-mcp-local` binary until hosted MCP becomes
a real concern.

**Transport implementation map:** `main.go` (`parseFlags`, transport dispatch in `run`) +
`http_transport.go` (`validateLoopbackAddr`, `bearerAuthMiddleware`, `serveHTTP`). `next_actions.go` holds
`nextActionsFor`, the pure classifier generalizing `review_action_hint`.

## Intake refinement internals (`fishhawk_draft_epic`, ADR-052, E34.4 / #1595)

`draft_epic.go` — the SINGLE operator MCP verb over the E34 refinement loop (reuse-first per E31.9: every existing
decision verb is stage-gated and resolves a run/stage, but a refinement session is neither a run nor a stage, so one
tool with arms keeps the registry at +1). The arms' 1:1 endpoint mapping:

| Arm | Trigger fields | Endpoint |
|---|---|---|
| open | `brief` alone | `POST /v0/refinement/sessions` |
| preview | `session_id` alone | `GET /v0/refinement/sessions/{id}` |
| edit | `session_id` + exactly one of `brief_amendment` \| `draft` | `PATCH /v0/refinement/sessions/{id}/draft` |
| decide | `session_id` + `decision` (`approved` \| `rejected`) + required `reason` | `POST /v0/refinement/sessions/{id}/decision` |
| file | `session_id` + `repo` | `POST /v0/refinement/sessions/{id}/file` |

`brief_amendment` is the agent re-draft, bounded by a per-session budget of **3**; `draft` is a direct strict-decoded
`EpicDraft` field edit, unbudgeted. Arm classification lives in `draftEpic`: it fails closed with NO HTTP call
(`armError` + the `legalArmsHelp` enumeration) when zero arms or an illegal combination is populated (e.g. `brief` with
any other field, both edit sub-arms, or >1 session sub-arm).

Every session-view result (open/preview/edit/decide) carries the `RefinementSession` mirror; the file arm carries
`RefinementFilingResult`; exactly one is set. Every result also carries a `session_guidance` block
(`guidanceForSession`/`guidanceForFiling`) — a next_actions-STYLE, tool-LOCAL block (deliberately NOT the run-scoped
`next_actions.go` machinery, since a refinement session has no run UUID) naming the exact next arm + arguments for the
derived state; the `awaiting_approval` guidance names any criteria-flagged child ordinals via
`flaggedCriteriaOrdinals`/`formatOrdinals`.

Backend error codes surface verbatim through typed `apiError` unwraps: `amendment_budget_exhausted`,
`decision_already_recorded`, `refinement_not_approved`, `refinement_draft_drifted`, `refinement_filing_repo_mismatch`,
and `refinement_filing_failed` — the last carrying the filed-so-far ordinals for a resumable re-invoke via
`filedSoFarDetail`. Wire mirrors (`RefinementSession`/`RefinementFilingResult`/`CriteriaPrecheck`) live in `client.go`
as #371-safe shapes (UUIDs typed `string`, no reflect-array pitfalls). Registered via `registerDraftEpic` in
`tools.go`, bumping the tool-count guard to **40** (`tools_test.go` `wantToolCount`).

**Auth:** `write:approvals` — NO new scope (the E34.2 precedent), so the operator token already driving
`fishhawk_approve_plan` works unchanged; a runner-side `fhm_` token (`mcp:read` only, per the auth split above) is
refused 403 — this is why the live draft→file walk is operator work, not implement-agent work.

**Agent-backed drafting is decoupled from the request lifetime (E37.4 / #1637).** The open + `brief_amendment` arms are
minutes-long drafting-agent calls, so `client.go` routes ONLY those two arms through a second `httpLong` client
(`refinementDraftClientTimeout` = 22m) while read/decide/file/direct-edit stay on the 30s short client, and
`backend/internal/server/refinement.go` runs the drafter+persist under `context.WithoutCancel` +
`WithTimeout(refinementDraftBudget = 20m)` (the #584 detached-review pattern) — so a mid-draft client disconnect
neither SIGKILLs the drafter nor strands a half-created session. The 22m client timeout sits above the 20m server
budget so the server's bounded error surfaces first.

## Campaign tool internals (ADR-047 / #1437, E25.8 / #1447, #1461; `operator_agent` override E25.12 / #1451)

`campaign.go` holds the operator-agent's campaign verbs over the E25.4 REST API — the campaign counterparts to the
single-run `fishhawk_start_run`/`fishhawk_get_run_status`/`fishhawk_resume_run` (see the Status list above for the
per-tool contract). Internals not covered there:

- **`operator_agent` override (E25.12 / #1451).** `fishhawk_start_campaign`'s optional `operator_agent` — the
  campaign-level delegation override — is typed `map[string]any` on `StartCampaignInput` so the SDK's reflection-built
  input schema sees an unconstrained object; it is marshalled to opaque JSON the backend validates against
  `spec.OperatorAgent`. It wins WHOLESALE over every issue-run's per-workflow `operator_agent`. The `Campaign` wire
  mirror (`client.go`) carries `operator_agent` as `map[string]any` for the same unconstrained-object reason, so the
  create + status surfaces round-trip the override back.
- **Gate-code mapping.** Each verb maps the backend gate codes onto operator-actionable tool errors;
  `fishhawk_start_campaign_item_run` (E26.2 / #1481; `campaign_id` + `issue_ref` + `workflow_id` required, optional
  `workflow_ref` + `runner_kind` — pass `local` for the local loop) maps `item_not_eligible` / `item_human_led`
  (#1697) / `campaign_item_not_found` / `campaign_not_startable` / `campaign_run_start_failed`.
- **`next_actions` mapping.** `fishhawk_get_campaign_status` maps `next_action` onto a legal operator move via
  `campaignNextActionsFor`, so the agent never reads an unclassified state. `fishhawk_resume_campaign` is legal only
  when `next_action` is `resume`.
- The campaign-scoped operator procedure lives in the operator role spec's `conventions.campaign`
  (`docs/spec/operator-role.md`), not hard-coded here.

## Acceptance operator surface (E31.9 / #1537, ADR-049)

**No new MCP tools or endpoints** — the acceptance stage is exposed to the operator loop by REUSING the existing verbs
(the tool registry did not grow for it). Dispatch rides the ordinary agent path: `fishhawk_dispatch_stage` /
`fishhawk_run_stage` accept `stage=acceptance` (their jsonschema enums + `composeRunnerArgv` already pass `--stage`
through generically; acceptance takes neither `--plan-out` nor `--check-base-ref`, and its egress hosts + criteria ids
arrive via `--fetch-prompt`, not argv). Awaiting the verdict rides the category-generic `fishhawk_await_audit` on
`acceptance_outcome_recorded` / `acceptance_triage_decided` (`fishhawk_await_review` does NOT fit — it is
ReviewStatus-shaped and acceptance has no reviewers). There is NO operator approve/reject acceptance gate (ADR-049
decision #2 makes failure routing deterministic server-side triage, E31.8; decision #6 gates the MERGE via the
`acceptance_passed` condition).

**The real surface is `next_actions`**: `implementStageNextActions`' settled path branches to
`acceptanceStageNextActions` before the merge ritual when the run declares an acceptance stage — the per-state arms are
enumerated in the [Server-suggested next actions](#server-suggested-next-actions-next_actions-1024) section above. The
verdict + disposition are read from the `acceptance_outcome_recorded` / `acceptance_triage_decided` audit payloads (a
FAILED verdict leaves the stage `succeeded`, so stage state is never inferred from) via `latestAcceptanceVerdict` /
`latestAcceptanceTriageDisposition` over the recent-audit slice; the verdict/disposition vocabulary is **mirrored, not
imported** from `backend/internal/server/acceptance.go` (the #875 compile trap), pinned by
`TestAcceptanceVocabularyMatchesBackend`. `fishhawk_get_run_status` adds `acceptance_stage_wait_status`
(`stageWaitStatusFor(…, "acceptance", …)`, omitted for non-acceptance runs). The acceptance playbook
(verdict-vs-stage-state, the deterministic triage table, the LOCAL-runner explicit-re-dispatch rule, paged arbitration)
lives in the `fishhawk://runbook` resource + the server `instructions`.

## Runner integration

E19.8 / future wires `fishhawk-mcp` into the runner's container image. Until then the MCP surface is interactive-Claude-Code-only.

## See also

- `docs/api/v0.openapi.yaml` — every tool wraps a `/v0/*` endpoint from this surface.
- `cli/internal/httpclient` — typed wrappers the MCP server reuses (or a thin local copy if cross-module reuse becomes awkward — final call inside individual tool PRs).
- [ADR-021](https://github.com/kuhlman-labs/fishhawk/issues/322) — the model-decision ADR.
