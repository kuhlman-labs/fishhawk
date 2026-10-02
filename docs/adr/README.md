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
  or ratification statement. Unknown records are listed below for captain
  confirmation; never guess upward.

The gate enforces a NECESSARY condition for `accepted` (C-accepted): SOME
section whose heading matches `^#{1,6}\s+Decision\b` (case-insensitive,
including `## Decision (2026-05-19)` and a nested `### Decision` under an
Addendum) must contain one of the words `accepted`, `approved`, `ratified`,
`recorded`, `decided`, `adopt`, `adopted` (case-insensitive, whole word), after
removing every line that is entirely one italic span (`_..._` or `*...*`; bold
`**...**` is NOT a placeholder). The acceptance word may appear in ANY
Decision-headed section — e.g. a placeholder first Decision and an accepting
Addendum `### Decision` passes. A section runs to the next heading of the same
or higher level; `#` lines inside fenced code blocks are not headings. The
placeholder strip is what stops `_To be recorded._` satisfying `recorded`. This
is a backstop, NOT proof: a Decision reading "Not accepted" passes it. The
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
(text before the first of `* ? [ {`, minus one trailing `/`) occurs verbatim in
the body — a backstop against an invented path, not a tree match.

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

- **Batch 1 (this change, E78.1):** ADR-001 – ADR-025.
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

## Status unknown — needs captain confirmation

- **ADR-010** — Decision section is the placeholder `_To be recorded._`.
- **ADR-011** — Decision section is the placeholder `_To be recorded._`.
- **ADR-012** — Decision section is the placeholder `_To be recorded._`.
- **ADR-022** — Decision (and the addendum's Decision) state a design with no
  acceptance or ratification statement.
