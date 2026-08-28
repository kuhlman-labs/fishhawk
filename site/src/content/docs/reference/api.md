---
title: API
description: The v0 REST API, generated per-operation from the OpenAPI document.
---

`fishhawkd` serves a versioned REST API under `/v0`. Runs, stages, plans,
approvals, scope amendments, and the audit log are all reachable through it —
the CLI, the Web UI, and the MCP server are three clients of this one surface.

The source of truth is
[`docs/api/v0.openapi.yaml`](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/api/v0.openapi.yaml);
the human companion is
[`docs/api/v0.md`](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/api/v0.md).

`GET /healthz` needs no credential and reports the build's commit and its
embedded schema hashes. Everything else requires a bearer token.

## Generated operation reference

<!-- BEGIN GENERATED api -->

_Generated from the canonical sources by `scripts/gen-site-reference`; do not edit between the markers. Description-only edits to a source are not diffed — the delta tables below compare shape (type, requiredness, enum members, default)._

> **See also:** [Driving a run](/fishhawk/operating/driving-a-run/) — the operator loop these reference surfaces serve. This page is the field reference, not a restatement of that guide.

## Operations

The v0 REST API exposes **113 operations** across the paths below, generated from [`docs/api/v0.openapi.yaml`](https://github.com/kuhlman-labs/fishhawk/blob/main/docs/api/v0.openapi.yaml). That document is the source of truth; this table is its published rendering.

| Method | Path | Summary |
|---|---|---|
| `POST` | `/mcp` | MCP streamable-HTTP endpoint (client-to-server messages) |
| `GET` | `/mcp` | MCP standalone event stream (not offered) |
| `DELETE` | `/mcp` | MCP session termination (not offered) |
| `GET` | `/.well-known/oauth-authorization-server` | OAuth 2.1 authorization-server metadata (RFC 8414) |
| `GET` | `/.well-known/oauth-protected-resource` | RFC 9728 protected-resource metadata (bare well-known path) |
| `GET` | `/.well-known/oauth-protected-resource/{resource_path}` | RFC 9728 protected-resource metadata (RFC 9728 §3.1 path-suffixed) |
| `GET` | `/v0/oauth/authorize` | OAuth 2.1 authorization endpoint (consent) |
| `POST` | `/v0/oauth/authorize` | OAuth 2.1 authorization endpoint (consent decision) |
| `POST` | `/v0/oauth/token` | OAuth 2.1 token endpoint |
| `GET` | `/healthz` | Liveness probe |
| `GET` | `/v0/auth/github/login` | Begin GitHub OAuth sign-in |
| `GET` | `/v0/auth/github/callback` | GitHub OAuth callback |
| `GET` | `/v0/auth/gitlab/login` | Begin GitLab OAuth sign-in |
| `GET` | `/v0/auth/gitlab/callback` | GitLab OAuth callback |
| `GET` | `/v0/auth/github/manifest-flow-start` | Begin GitHub App manifest registration |
| `GET` | `/v0/auth/github/manifest-callback` | Receive GitHub App manifest-flow callback |
| `GET` | `/v0/auth/me` | Current user |
| `POST` | `/v0/auth/logout` | Sign out |
| `GET` | `/v0/onboarding/readiness` | First-run readiness for a repository |
| `GET` | `/v0/onboarding/start` | Cell entry point for a directory-routed caller |
| `GET` | `/v0/tokens` | List API tokens for the current user |
| `POST` | `/v0/tokens` | Mint a new scoped API token |
| `GET` | `/v0/tokens/login` | OAuth token-login discovery |
| `POST` | `/v0/tokens/login` | Mint a user-bound token from an OAuth device-flow access token |
| `DELETE` | `/v0/tokens/{token_id}` | Revoke an API token |
| `GET` | `/v0/runs` | List workflow runs |
| `POST` | `/v0/runs` | Create a workflow run |
| `GET` | `/v0/runs/{run_id}` | Get a run |
| `POST` | `/v0/runs/{run_id}/cancel` | Cancel a run |
| `POST` | `/v0/runs/{run_id}/consolidate` | Run the decomposed-parent fan-in (E24.2) on demand |
| `POST` | `/v0/runs/{run_id}/integrate-wave` | Non-settling per-wave fan-in of a decomposed parent (E24.X / |
| `POST` | `/v0/runs/{run_id}/recover` | Recover a category-B-failed run against its approved plan |
| `POST` | `/v0/runs/{run_id}/redrive` | Re-drive a failed decomposition child run |
| `POST` | `/v0/runs/{run_id}/revive` | Revive a terminal-failed run (one-verb batch re-park) |
| `POST` | `/v0/runs/{run_id}/reset-branch` | Force-reset a run branch off a foreign on-top commit |
| `POST` | `/v0/runs/{run_id}/vouch-commit` | Vouch a foreign commit as run-authored lineage |
| `POST` | `/v0/runs/{run_id}/merge` | Record an operator merge verdict and queue the squash merge |
| `POST` | `/v0/runs/{run_id}/grooming-dispositions` | Record per-entry grooming dispositions (operator-only) |
| `GET` | `/v0/runs/{run_id}/grooming-dispositions` | Read back the recorded grooming dispositions for a run |
| `POST` | `/v0/runs/{run_id}/acceptance-arbitration` | Record the operator arbitration that discharges a paged acceptance triage |
| `POST` | `/v0/runs/{run_id}/auto-drive` | Drive the run's parked gate under ADR-040 delegation |
| `POST` | `/v0/runs/{run_id}/auto-drive/acts` | Record a driver stage dispatch (record-before-dispatch) |
| `GET` | `/v0/runs/{run_id}/stages` | List stages for a run |
| `GET` | `/v0/runs/{run_id}/stages/{stage_id}` | Read one stage by its (run, stage) handle, with optional terminal-wait long-poll |
| `POST` | `/v0/runs/{run_id}/reviews/reconcile` | Terminate a review round orphaned by a fishhawkd restart |
| `POST` | `/v0/runs/{run_id}/stages/{stage_id}/reap-failure` | Report a spawn-phase runner failure so a stuck 'dispatched' stage fails |
| `POST` | `/v0/runs/{run_id}/stages/{stage_id}/host-dispatch` | Mark a host-local spawn attempt so a parked stage becomes 'dispatched' |
| `POST` | `/v0/runs/{run_id}/stages/{stage_id}/progress` | Record a stage's mid-execution progress heartbeat |
| `GET` | `/v0/runs/{run_id}/audit` | List audit entries for a run |
| `GET` | `/v0/runs/{run_id}/gate-view` | Gate-scoped decision view for a run |
| `GET` | `/v0/runs/{run_id}/budget` | Current periodic-budget status for a run's workflow |
| `GET` | `/v0/runs/{run_id}/cache-efficiency` | Per-run prompt-cache efficiency metric |
| `GET` | `/v0/runs/{run_id}/cost` | Per-run estimated cost with per-stage and per-merged-PR rollup |
| `GET` | `/v0/runs/{run_id}/latency` | Per-run gate-latency (wait-on-human) rollup |
| `GET` | `/v0/runs/{run_id}/diagnostics` | Product-facts-only diagnostic bundle for a run |
| `POST` | `/v0/runs/{run_id}/product-reports` | File a deduped, audited upstream product report for a run |
| `GET` | `/v0/runs/{run_id}/status-comment` | Get rendered sticky-comment body and stored GitHub comment ID |
| `POST` | `/v0/runs/{run_id}/status-comment` | Record the GitHub comment ID after posting the sticky comment |
| `GET` | `/v0/audit` | Search the cross-chain audit log |
| `GET` | `/v0/audit/export` | Compliance export in the verifier's Export v1 wire shape |
| `GET` | `/v0/audit/export.csv` | Flat CSV rendering of the compliance export for spreadsheet workflows |
| `GET` | `/v0/reports/agent-changes` | Canned compliance report of all agent changes in a date range (JSON) |
| `GET` | `/v0/reports/agent-changes.md` | Canned compliance report of all agent changes (human-readable markdown) |
| `GET` | `/v0/releases/notes/preview` | Preview evidence-derived release notes for a ref range |
| `POST` | `/v0/releases/notes` | Persist evidence-derived release notes as an artifact |
| `POST` | `/v0/releases/cut` | Record the operator's ratified release-version decision |
| `POST` | `/v0/releases/publish` | Publish persisted release notes to a GitHub Release |
| `GET` | `/v0/campaigns` | List campaigns |
| `POST` | `/v0/campaigns` | Create a campaign from an epic ref |
| `GET` | `/v0/campaigns/{campaign_id}` | Get a campaign |
| `GET` | `/v0/campaigns/{campaign_id}/items` | List a campaign's items |
| `GET` | `/v0/campaigns/{campaign_id}/status` | Get a campaign's rollup status + next action |
| `POST` | `/v0/campaigns/{campaign_id}/runs` | Start a run for an eligible campaign item |
| `POST` | `/v0/campaigns/{campaign_id}/resume` | Resume a paused campaign |
| `POST` | `/v0/campaigns/{campaign_id}/cancel` | Cancel a campaign |
| `POST` | `/v0/work-items` | File a work item via the repo's work-management conventions |
| `POST` | `/v0/refinement/sessions` | Draft an epic/children preview from a natural-language brief |
| `GET` | `/v0/refinement/sessions/{session_id}` | Get a refinement session's preview + approval state |
| `PATCH` | `/v0/refinement/sessions/{session_id}/draft` | Edit a refinement draft (agent amendment or direct field edit) |
| `POST` | `/v0/refinement/sessions/{session_id}/decision` | Approve or reject the latest refinement draft revision |
| `POST` | `/v0/refinement/sessions/{session_id}/file` | File an approved refinement draft into tracker items |
| `GET` | `/v0/calibration` | Get runtime calibration statistics |
| `GET` | `/v0/acceptance-triage/stats` | Get acceptance-triage statistics |
| `GET` | `/v0/stages/{stage_id}` | Get a stage |
| `GET` | `/v0/stages/{stage_id}/artifacts` | List artifacts for a stage |
| `GET` | `/v0/stages/{stage_id}/prompt` | Fetch the constructed prompt for a stage |
| `GET` | `/v0/stages/{stage_id}/prompt-render` | Fetch the constructed prompt for a stage (SPA-readable) |
| `GET` | `/v0/stages/{stage_id}/trace` | Stream the redacted trace bundle for a stage |
| `GET` | `/v0/stages/{stage_id}/checks` | Latest state per blocking check on a stage |
| `POST` | `/v0/stages/{stage_id}/approvals` | Approve or reject the gate on a stage |
| `POST` | `/v0/stages/{stage_id}/clarification` | Answer a plan stage parked at awaiting_input |
| `POST` | `/v0/stages/{stage_id}/revise` | Re-plan a plan stage in place against a binding operator constraint |
| `POST` | `/v0/stages/{stage_id}/retry` | Retry a failed stage |
| `POST` | `/v0/stages/{stage_id}/acceptance-admission` | Pre-spawn acceptance short-circuit admission |
| `POST` | `/v0/stages/{stage_id}/fixup` | Route advisory implement-review concerns back to the agent |
| `POST` | `/v0/concerns/{concern_id}/waive` | Waive an open review concern with an audited reason |
| `POST` | `/v0/concerns/{concern_id}/defer` | Defer an open review concern into a follow-up work item |
| `GET` | `/v0/artifacts/{artifact_id}` | Get an artifact |
| `POST` | `/v0/runs/{run_id}/signing-key` | Issue a per-run Ed25519 signing key |
| `POST` | `/v0/runs/{run_id}/trace` | Upload a signed trace bundle |
| `POST` | `/v0/runs/{run_id}/plan` | Upload a plan-stage artifact (plan or a discriminated sibling) |
| `POST` | `/v0/runs/{run_id}/pull-request` | Upload a pull-request artifact for an implement stage |
| `POST` | `/v0/runs/{run_id}/deployment` | Record a deployment artifact for a deploy stage |
| `POST` | `/v0/runs/{run_id}/acceptance` | Record an acceptance-evidence artifact for an acceptance stage |
| `POST` | `/v0/runs/{run_id}/deployment/rollback` | Re-dispatch a delegating deploy's rollback path |
| `POST` | `/v0/runs/{run_id}/installation-token` | Mint a GitHub App installation token for the run's repo |
| `POST` | `/v0/runs/{run_id}/mcp-token` | Mint a short-lived MCP bearer token for the run |
| `POST` | `/v0/runs/{run_id}/scope-amendments` | Request a mid-stage scope amendment (implement agent) |
| `GET` | `/v0/runs/{run_id}/scope-amendments` | List a run's scope amendments |
| `POST` | `/v0/runs/{run_id}/scope-amendments/{amendment_id}/decision` | Approve or deny a scope amendment (operator) |
| `POST` | `/v0/runs/{run_id}/scope-completeness/decision` | Exempt, amend or fail a parked scope-completeness shortfall (operator) |
| `POST` | `/webhooks/github` | GitHub App webhook receiver |
| `POST` | `/webhooks/gitlab` | GitLab webhook receiver |

<!-- END GENERATED api -->
