# Decision records (`docs/adr/`)

The in-repo record of settled architectural decisions (E78.1 / #3722). One file
per ADR, `NNN-slug.md`: a machine-readable front matter followed by the tracker
issue body **transcribed verbatim** — the record transcribes, it never
corrects, so a path or option named in a body is kept as named even if it has
since moved. Enforced by `scripts/check-adr` in `scripts/test verify` (gate
contract: `scripts/README.md` § "ADR record gate").

## Record layout

```
---
id: ADR-002
title: "Postgres access layer (sqlc + pgx vs. ORM)"
status: accepted
date: 2026-04-30
issue: https://github.com/kuhlman-labs/fishhawk/issues/66
supersedes: []
superseded_by: []
applies_to: ["backend/internal/*/queries.sql", "backend/internal/*/db/**"]
---

# ADR-002: Postgres access layer (sqlc + pgx vs. ORM)

<issue body, verbatim>
```

The body below the H1 is the issue body byte-for-byte; the only normalisation
is CRLF → LF and exactly one trailing newline. Every section is kept in source
order (Context, Options, Recommendation, Decision, Consequences, and any
Addendum / Resolution / Related / Spec reference) — dropping one would be an
editorial act.

## Front-matter grammar (a restricted YAML subset)

- Line 1 is `---`; the block ends at the next line that is exactly `---`.
- Every non-blank line is `key: value` (one space after the colon).
- A value is one of: a JSON double-quoted string (parsed as JSON, so it is also
  a valid YAML double-quoted scalar); a bare token matching
  `^[A-Za-z0-9][A-Za-z0-9._:/#-]*$`; or, for list keys, a JSON array of strings.
- Known keys: `id`, `title`, `status`, `date`, `issue`, `supersedes`,
  `superseded_by`, `applies_to`. Unknown and duplicate keys are violations.
- Required: every key except `date`. List keys (`supersedes`, `superseded_by`,
  `applies_to`) are present even when empty (`[]`), and must be lists; the
  other keys must be scalars.
- `date` is optional and, when present, a real `YYYY-MM-DD` date.

## Field semantics

| Key | Meaning |
|---|---|
| `id` | `ADR-NNN`; equals `ADR-` + the filename's 3-digit prefix. |
| `title` | The issue title without its `[ADR-NNN] ` prefix. |
| `status` | One of `proposed`, `accepted`, `rejected`, `superseded`, `unknown` — see the inference rule below. |
| `date` | The date the Decision section or its heading states (`Recorded 2026-04-30`, `Accepted (2026-07-26)`, `Decided 2026-06-09`, `## Decision (2026-05-19)`, `Resolved on 2026-05-26`). Omitted when none is stated. A separate Resolution section's or an implementation-landed date is NOT a decision date. |
| `issue` | The tracker issue URL the body is transcribed from. |
| `supersedes` / `superseded_by` | ADR ids, only when a source STATES the supersession. Reciprocal: `A.supersedes ∋ B ⇔ B.superseded_by ∋ A`. Every id must name an existing record other than itself, so a link to a not-yet-backfilled ADR is impossible by construction — the batch that backfills the superseding record adds both sides. |
| `applies_to` | Repository path globs the decision governs (see below). |

### Status inference rule (fail-closed)

- `accepted` — the Decision section records acceptance or ratification: an
  explicit decision statement such as `Recorded <date>`, `Accepted`,
  `Approved as drafted`, `Decided`, or `Adopt option X` (ratified by the
  operator for this batch: "Adopt option X" counts as acceptance).
- `superseded` / `rejected` — only when a source states it.
- `unknown` — anything else, including a placeholder Decision
  (`_To be recorded._`) or a Decision that states a design with no acceptance
  or ratification statement. A `**Status: ...**` line ABOVE Context sits outside
  the Decision section and is not read by this rule; such records are listed
  below with that line quoted. Unknown records are listed below for captain
  confirmation; never guess upward.

The gate enforces a NECESSARY condition for `accepted` (C-accepted): SOME
section whose heading matches `^#{1,6}\s+(?:[^/\n]*/\s*)*Decision\b`
(case-insensitive: `Decision` starts the heading or starts a slash-separated
part of it, so `## Decision (2026-05-19)`, a nested `### Decision` under an
Addendum and `## Recommendation / Decision` all count, while `## Decisions
already made` and `## The deferred decision (this ADR)` do not) must contain one
of the words `accepted`, `approved`, `ratified`, `recorded`, `decided`, `adopt`,
`adopted` (case-insensitive, whole word), after removing every line that is
entirely ONE italic span with no inner delimiter (`_x_` or `*x*`; bold `**x**`
is NOT a placeholder, and neither is `_Note:_ Accepted as drafted _(see
addendum)_`, which holds two spans around text and is kept). The acceptance
word may appear in ANY
Decision-headed section — e.g. a placeholder first Decision and an accepting
Addendum `### Decision` passes. A section runs to the next heading of the same
or higher level; `#` lines inside fenced code blocks are not headings. The
placeholder strip is what stops `_To be recorded._` satisfying `recorded`; its
cost is that an italic placeholder containing an inner `_` or `*` is no longer
stripped. This is a backstop, NOT proof: a Decision reading "Not accepted" passes it. The
authoritative control is the per-record application of the rule above plus
reviewer spot-checks.

### `applies_to` rule

Empty (`[]`) unless the Decision or Consequences section names a repository
path as the decision's SUBJECT — where the decided thing lives or what it
governs. Citations (a Spec reference to `docs/MVP_SPEC.md`, a Related link, a
"mirrors the resolver at …" pointer) do not count. A `<placeholder>` path
segment is written as `*`; a named directory takes a trailing `**`. When in
doubt, `[]`. Globs are transcribed as named, so they may match nothing in
today's tree (e.g. ADR-016's `infra/terraform/**`, retired by ADR-073);
consumers must tolerate that. The gate checks that every entry's literal stem
(text before the first of `* ? [ {`, minus one trailing `/`) occurs in the body
as a BOUNDED path token — a backstop against an invented path, not a tree
match. Bounded means the occurrence is not inside a longer word (the next
character is not `[A-Za-z0-9_-]`, so `cli` is not satisfied by `clients`) and is
not the tail of a longer path (the previous character is not `[A-Za-z0-9_./-]`,
so `cli` is not satisfied by `backend/internal/cli/`), except that a `/` which
itself starts a root-anchored path is allowed (`` `/cli/**` `` names `cli`, as
ADR-002's `/backend/sqlc.yaml` and ADR-013's `/cli/**` do). The match is
case-sensitive.

## `index.json` — the machine index

Reviewers and the architect responder read decisions through repodoc, which can
fetch a named file but cannot list a directory, so `index.json` names every
record:

```json
{
  "schema_version": "adr-index-v1",
  "records": [
    {"id": "ADR-001", "title": "...", "path": "docs/adr/001-....md", "status": "accepted",
     "supersedes": [], "superseded_by": [], "applies_to": []}
  ]
}
```

`path` is repo-relative. The gate compares the index to the records
semantically, keyed by path: a record with no entry, an entry naming no record
file, or an entry field disagreeing with the front matter each fail verify.
Never hand-edit it — regenerate with:

```sh
scripts/check-adr --write-index
```

which refuses (exit 1, writes nothing) while any record check fails.

## Batch plan

- **Batch 1 (E78.1 / #3722):** ADR-001 – ADR-025.
- **Batch 2 (E78.2 / #3723):** ADR-026 – ADR-055. Landed with the one
  cross-batch supersession the sources state: ADR-031 supersedes ADR-018's
  review-gate-resolution *mechanism*, so ADR-018 carries
  `superseded_by: ["ADR-031"]` (front matter only; its body is untouched).
  ADR-018 stays `accepted` — ADR-031 states it "keeps its invariant" — and
  ADR-031 itself is `unknown` (see below). ADR-040 is transcribed from the
  decision record #997, not from the five implementation issues that also
  carry an `[ADR-040]` title.
- Later batches backfill the remaining ADRs; until an ADR's batch lands, read
  it from the tracker (`gh issue list --label adr`). A batch that backfills a
  superseding record also edits the older record's `superseded_by`.

## Records

| ID | Title | Status | Issue |
|---|---|---|---|
| [ADR-001](001-cloud-target-aws-gcp-other.md) | Cloud target (AWS / GCP / Other) | `accepted` | [#65](https://github.com/kuhlman-labs/fishhawk/issues/65) |
| [ADR-002](002-postgres-access-layer-sqlc-pgx-vs-orm.md) | Postgres access layer (sqlc + pgx vs. ORM) | `accepted` | [#66](https://github.com/kuhlman-labs/fishhawk/issues/66) |
| [ADR-003](003-object-storage-choice-dev-loop-strategy.md) | Object storage choice + dev-loop strategy | `accepted` | [#67](https://github.com/kuhlman-labs/fishhawk/issues/67) |
| [ADR-004](004-frontend-styling-system.md) | Frontend styling system | `accepted` | [#68](https://github.com/kuhlman-labs/fishhawk/issues/68) |
| [ADR-005](005-api-auth-session-model.md) | API auth/session model | `accepted` | [#69](https://github.com/kuhlman-labs/fishhawk/issues/69) |
| [ADR-006](006-db-migration-tool.md) | DB migration tool | `accepted` | [#70](https://github.com/kuhlman-labs/fishhawk/issues/70) |
| [ADR-007](007-trace-bundle-wire-format.md) | Trace bundle wire format | `accepted` | [#71](https://github.com/kuhlman-labs/fishhawk/issues/71) |
| [ADR-008](008-signing-scheme-ed25519.md) | Signing scheme (Ed25519, canonicalization, issuance protocol) | `accepted` | [#72](https://github.com/kuhlman-labs/fishhawk/issues/72) |
| [ADR-009](009-hosted-deployment-target.md) | Hosted deployment target | `accepted` | [#73](https://github.com/kuhlman-labs/fishhawk/issues/73) |
| [ADR-010](010-marketplace-billing-path-github-vs-direct.md) | Marketplace billing path (GitHub vs. direct) | `unknown` | [#74](https://github.com/kuhlman-labs/fishhawk/issues/74) |
| [ADR-011](011-pricing-model-per-engineer-vs-per-run.md) | Pricing model (per-engineer vs. per-run) | `unknown` | [#75](https://github.com/kuhlman-labs/fishhawk/issues/75) |
| [ADR-012](012-design-partner-sourcing-strategy.md) | Design partner sourcing strategy | `unknown` | [#76](https://github.com/kuhlman-labs/fishhawk/issues/76) |
| [ADR-013](013-apache-bsl-enterprise-boundary.md) | Apache 2.0 / BSL boundary for enterprise modules | `accepted` | [#77](https://github.com/kuhlman-labs/fishhawk/issues/77) |
| [ADR-014](014-linter-formatter-ci-config-for-go-and-ts.md) | Linter/formatter/CI config for Go and TS | `accepted` | [#78](https://github.com/kuhlman-labs/fishhawk/issues/78) |
| [ADR-015](015-slack-notification-approach.md) | Slack notification approach (v0.x scope) | `accepted` | [#79](https://github.com/kuhlman-labs/fishhawk/issues/79) |
| [ADR-016](016-iac-tool-terraform-vs-aws-cdk.md) | IaC tool — Terraform vs AWS CDK | `accepted` | [#165](https://github.com/kuhlman-labs/fishhawk/issues/165) |
| [ADR-017](017-ci-gating-defer-to-github-branch-protection.md) | CI gating: defer to GitHub branch protection, decouple approval from merge | `accepted` | [#249](https://github.com/kuhlman-labs/fishhawk/issues/249) |
| [ADR-018](018-review-stage-approval-defers-to-github-pr-merge.md) | Review-stage approval defers to GitHub PR merge | `accepted` | [#311](https://github.com/kuhlman-labs/fishhawk/issues/311) |
| [ADR-019](019-fishhawk-as-coordination-layer-not-destination.md) | Fishhawk as coordination layer, not destination | `accepted` | [#320](https://github.com/kuhlman-labs/fishhawk/issues/320) |
| [ADR-020](020-plan-review-surface-on-originating-issue.md) | Plan review surface: post the plan on the originating issue | `accepted` | [#321](https://github.com/kuhlman-labs/fishhawk/issues/321) |
| [ADR-021](021-claude-code-integration-mcp-server.md) | Claude Code integration: ship a Fishhawk MCP server | `accepted` | [#322](https://github.com/kuhlman-labs/fishhawk/issues/322) |
| [ADR-022](022-pluggable-runner-backends-runner-kind-audit.md) | Pluggable runner backends + runner_kind audit dimension | `unknown` | [#388](https://github.com/kuhlman-labs/fishhawk/issues/388) |
| [ADR-023](023-agent-self-retry-on-transient-stage-failure.md) | Agent self-retry on transient stage failure | `accepted` | [#401](https://github.com/kuhlman-labs/fishhawk/issues/401) |
| [ADR-024](024-mcp-server-spawning-runner-subprocesses.md) | MCP server spawning runner subprocesses | `accepted` | [#433](https://github.com/kuhlman-labs/fishhawk/issues/433) |
| [ADR-025](025-scope-policy-timeouts-runtime-decomposition.md) | Scope-policy: workflow-spec timeouts, predicted runtime, and plan-driven decomposition | `accepted` | [#451](https://github.com/kuhlman-labs/fishhawk/issues/451) |
| [ADR-026](026-backward-compatibility-for-schema-extension-prs.md) | Backward-compatibility discipline for schema-extension PRs | `unknown` | [#472](https://github.com/kuhlman-labs/fishhawk/issues/472) |
| [ADR-027](027-review-agent-stage-primitive.md) | Review-agent stage primitive: per-stage agent + human reviewer counts | `accepted` | [#545](https://github.com/kuhlman-labs/fishhawk/issues/545) |
| [ADR-028](028-agent-filesystem-confinement-os-level-sandbox.md) | Agent filesystem confinement requires an OS-level sandbox | `unknown` | [#642](https://github.com/kuhlman-labs/fishhawk/issues/642) |
| [ADR-029](029-rule-of-two-audit-network-egress-allow-listing.md) | Rule-of-Two / lethal-trifecta audit + network egress allow-listing | `accepted` | [#650](https://github.com/kuhlman-labs/fishhawk/issues/650) |
| [ADR-030](030-periodic-cost-budgets-in-workflow-spec.md) | Periodic cost budgets in the workflow spec (advisory + admission-time blocking) | `accepted` | [#687](https://github.com/kuhlman-labs/fishhawk/issues/687) |
| [ADR-031](031-review-gate-succeeds-only-on-verified-landing.md) | Review gate succeeds only on verified landing (platform-neutral, delivery-agnostic) | `unknown` | [#710](https://github.com/kuhlman-labs/fishhawk/issues/710) |
| [ADR-032](032-decomposition-one-consolidated-pr.md) | Decomposition delivery model: one consolidated PR per decomposition | `unknown` | [#719](https://github.com/kuhlman-labs/fishhawk/issues/719) |
| [ADR-033](033-mcp-transport-stdio-vs-streamable-http.md) | MCP transport: stdio-only vs. add streamable-HTTP (relationship to the #655 gateway) | `accepted` | [#843](https://github.com/kuhlman-labs/fishhawk/issues/843) |
| [ADR-034](034-kubernetes-deployment-model.md) | Kubernetes deployment model — relationship to ECS, deps, secrets, worker model | `unknown` | [#845](https://github.com/kuhlman-labs/fishhawk/issues/845) |
| [ADR-035](035-run-git-contract-declared-base-sole-writer.md) | Own and enforce the git contract of a run (declared base + sole-writer branch lineage) | `unknown` | [#857](https://github.com/kuhlman-labs/fishhawk/issues/857) |
| [ADR-036](036-gate-plan-approval-on-agent-review-completion.md) | Gate plan approval on agent-review completion (block-on-completion, unblock-on-terminal) | `unknown` | [#874](https://github.com/kuhlman-labs/fishhawk/issues/874) |
| [ADR-037](037-agent-facing-waits-follow-mcp-tasks-contract.md) | Agent-facing waits follow the MCP Tasks / long-running-operation contract (durable handle + authoritative poll) | `unknown` | [#879](https://github.com/kuhlman-labs/fishhawk/issues/879) |
| [ADR-038](038-deploy-stage-type-post-merge-release-governance.md) | Add a deploy stage type for post-merge release governance | `accepted` | [#925](https://github.com/kuhlman-labs/fishhawk/issues/925) |
| [ADR-039](039-agentic-deploy-execution.md) | Agentic deploy execution (skills perform the rollout) | `unknown` | [#937](https://github.com/kuhlman-labs/fishhawk/issues/937) |
| [ADR-040](040-operator-agent-role-contract.md) | Operator agent as a product-defined role contract: versioned playbook, gate-delegation knobs, thin-by-design | `accepted` | [#997](https://github.com/kuhlman-labs/fishhawk/issues/997) |
| [ADR-041](041-per-child-branch-isolation-fan-in-integration.md) | Per-child branch isolation + fan-in integration for parallel decomposition | `accepted` | [#1140](https://github.com/kuhlman-labs/fishhawk/issues/1140) |
| [ADR-042](042-fixup-session-continuity.md) | Fix-up session continuity: resume the prior implement agent session vs. prompt-level continuity | `unknown` | [#1264](https://github.com/kuhlman-labs/fishhawk/issues/1264) |
| [ADR-043](043-stale-base-integration-policy.md) | Stale-base integration policy for concurrent agent branches (runner merges base before PR; clean verdict-transfer vs conflict re-invoke+re-review; merge not rebase) | `accepted` | [#1293](https://github.com/kuhlman-labs/fishhawk/issues/1293) |
| [ADR-044](044-model-cost-observability.md) | model cost observability — warn-not-fail price drift-check, 3-bucket fresh/read/write cache cost model with separate capture, split cost-accuracy from efficiency-metric, daily cadence | `unknown` | [#1336](https://github.com/kuhlman-labs/fishhawk/issues/1336) |
| [ADR-045](045-runner-backend-identification.md) | runner-backend identification — derive runner_kind from the execution channel (host-dispatch ⇒ local, runner self-report, mismatch guardrail), not an operator declaration | `unknown` | [#1347](https://github.com/kuhlman-labs/fishhawk/issues/1347) |
| [ADR-046](046-workflow-schema-major-version-coexistence.md) | Workflow schema major-version coexistence model — how workflow-v1 (deploy stage) coexists with and routes against workflow-v0 | `accepted` | [#1380](https://github.com/kuhlman-labs/fishhawk/issues/1380) |
| [ADR-047](047-campaign-primitive.md) | Campaign primitive — dependency-ordered multi-run sprint driving, generalizing the decomposition fan-out engine to the epic/issue level | `accepted` | [#1437](https://github.com/kuhlman-labs/fishhawk/issues/1437) |
| [ADR-048](048-repository-onboarding-init-doctor.md) | Repository onboarding — `fishhawk init` + `doctor` (CLI engine), MCP/skill wrapper, and App-PR scaffolding, preset-driven from typed specs | `accepted` | [#1499](https://github.com/kuhlman-labs/fishhawk/issues/1499) |
| [ADR-049](049-acceptance-validation-stage.md) | Acceptance-validation stage: agent-driven behavioral validation against intent before release | `accepted` | [#1519](https://github.com/kuhlman-labs/fishhawk/issues/1519) |
| [ADR-050](050-acceptance-agent-egress-credential-posture.md) | Acceptance-agent egress + credential posture (runner-embedded default-deny proxy) | `accepted` | [#1540](https://github.com/kuhlman-labs/fishhawk/issues/1540) |
| [ADR-051](051-release-governance-release-notes-delegating-publish.md) | Release governance: evidence-derived release notes + delegating publish | `accepted` | [#1579](https://github.com/kuhlman-labs/fishhawk/issues/1579) |
| [ADR-052](052-intake-and-refinement-brief-to-drafts.md) | Intake and refinement: natural-language brief to gated epic/children drafts | `accepted` | [#1580](https://github.com/kuhlman-labs/fishhawk/issues/1580) |
| [ADR-053](053-post-deploy-verification-incident-intake.md) | Post-deploy verification + incident intake (closing the ops-to-dev loop) | `accepted` | [#1581](https://github.com/kuhlman-labs/fishhawk/issues/1581) |
| [ADR-054](054-compliance-export-surface-posture.md) | Compliance export surface posture (auth scope, redaction, filtering semantics) | `accepted` | [#1582](https://github.com/kuhlman-labs/fishhawk/issues/1582) |
| [ADR-055](055-approval-identity-quorum-eligibility-forge-verified.md) | Approval identity: quorum + eligibility predicates + forge-verified identity (GitHub/GitLab-agnostic) | `accepted` | [#1698](https://github.com/kuhlman-labs/fishhawk/issues/1698) |

## Status unknown — needs captain confirmation

- **ADR-010** — Decision section is the placeholder `_To be recorded._`.
- **ADR-011** — Decision section is the placeholder `_To be recorded._`.
- **ADR-012** — Decision section is the placeholder `_To be recorded._`.
- **ADR-022** — Decision (and the addendum's Decision) state a design with no
  acceptance or ratification statement.
- **ADR-026** — No Decision section: the body ends Context / Options /
  `## Recommendation (subject to ADR review)` / Consequences, so no decision is
  recorded. No date.
- **ADR-028** (date 2026-06-01, from `## Decision (2026-06-01)`) — the Decision
  states a design (local: detection-only; hosted: container mount-confinement)
  with no acceptance or ratification statement; the only acceptance wording is
  "Accepted for v0" in Consequences, covering the local residual risk, not the
  decision. The gate cannot judge this one.
- **ADR-031** — Opens `**Status: Decided (2026-06-03).**` above Context, but the
  Decision section carries no acceptance statement. Records the supersession of
  ADR-018's resolution mechanism (ADR-018 `superseded_by: ["ADR-031"]`, ADR-018
  stays `accepted`: "keeps its invariant"). No date recorded.
- **ADR-032** — Opens `**Status: Decided (2026-06-03).**` above Context; the
  Decision section carries no acceptance statement. No date recorded.
- **ADR-034** — Decision section is the placeholder `_TBD._`.
- **ADR-035** — Opens `**Status: Decided (2026-06-07).**` above Context; the
  Decision section carries no acceptance statement. No date recorded.
- **ADR-036** — Opens `**Status: Decided (2026-06-08).**` above Context; the
  Decision section carries no acceptance statement. No date recorded.
- **ADR-037** — Opens `**Status: Decided (2026-06-08).**` above Context; the
  Decision section carries no acceptance statement. No date recorded.
- **ADR-039** (date 2026-08-25, from `Recorded 2026-08-25`) — status line
  `**Status:** Proposed — 2026-06-09`; the Decision is "Deferred. Charter N4 is
  the standing answer until this ADR is revisited", and the issue is still
  OPEN. Neither accepted nor a stated rejection.
- **ADR-042** — Decision section is the placeholder
  `_TBD — operator. (Agent proposes, human disposes.)_`.
- **ADR-044** — The Decision states a design with no acceptance or ratification
  statement; "accepted" appears only about models ("newly-released models are
  accepted"), not the decision. No date.
- **ADR-045** — The Decision states a design with no acceptance or ratification
  statement; the resolution sits in a separate `## Status — RESOLVED
  (2026-06-24)` section, which is outside the Decision section (and so
  contributes no date). A candidate rule extension — also read a leading
  `**Status: Decided/Accepted (<date>)**` line or a Status/Resolution section —
  would settle ADR-031, -032, -035, -036, -037 and -045 together; it is not part
  of the current rule.
