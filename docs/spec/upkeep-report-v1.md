# Upkeep report artifact — `upkeep_report_v1`

Normative reference for the plan-stage artifact an upkeep scan ships (E79 /
#3726, contract #3921). Schema: [`upkeep-report-v1.schema.json`](upkeep-report-v1.schema.json)
(Draft 2020-12, embedded at `backend/internal/plan/schemas/` by
`scripts/sync-schemas`). Examples: [`examples/upkeep-report-v1-example.json`](examples/upkeep-report-v1-example.json),
[`examples/upkeep-report-v1-advisory-example.json`](examples/upkeep-report-v1-advisory-example.json) (#3750).
Dedupe and Dependabot-coverage contracts and residuals: `backend/internal/upkeep/README.md`.

An upkeep report is a PROPOSAL. It lists maintenance findings — flaky tests,
toolchain drift, deprecations, vulnerability advisories — each with evidence and
a proposed issue. Nothing is filed on ingest; filing is a later, gated step
(#3924).

## Discrimination

`POST /v0/runs/{run_id}/plan` routes on the top-level `kind` BEFORE schema
validation. `kind: upkeep_report` selects this schema; the artifact carries no
`plan_version`, so it never collides with `plan-standard-v1`. It is the third
additive sibling after `clarification_request` and `grooming_report`.

## Top-level fields

All objects are `additionalProperties: false`.

| Field | Required | Shape |
|---|---|---|
| `kind` | yes | const `upkeep_report` |
| `report_version` | yes | const `upkeep_report_v1`; a breaking change is a new `upkeep_report_v2` file |
| `ticket_reference` | yes | `{type: github_issue, url, id}` — the run's originating ticket; verbatim `grooming-report-v1` shape |
| `generated_by` | yes | `{agent, model, version?, timestamp}` — verbatim `grooming-report-v1` shape |
| `summary` | yes | string, minLength 1 |
| `sources_scanned` | yes | array of `flake` / `toolchain_drift` / `deprecation` / `advisory`, minItems 1, uniqueItems |
| `findings` | yes | array of finding, maxItems 200, MAY be empty |
| `source_degrades` | no (#3750) | array of [source degrade](#source-degrades-3750), maxItems 8 |

`sources_scanned` records what the scan COVERED. An empty `findings` array with
`sources_scanned: [flake]` reads "scanned for flakes, none found", never "not
scanned". A source the scan could not run is left OUT of `sources_scanned` and
NAMED in `source_degrades`.

### Finding

| Field | Required | Shape |
|---|---|---|
| `id` | yes | pattern `^(flake\|toolchain_drift\|deprecation\|advisory):[^\s<>]+$` |
| `source` | yes | `flake` / `toolchain_drift` / `deprecation` / `advisory` |
| `subject` | yes | 1..256 chars, pattern `^[^\s<>]+$` (test name, tool name, API; for an advisory `<primary id>:<package>`) |
| `evidence` | yes | 1..50 evidence refs |
| `advisory` | on, and only on, an `advisory` finding (rule l) | see [Advisory object](#advisory-object-3750) |
| `proposed_issue` | yes | see below |

### Evidence ref (`oneOf`, discriminated by `kind`)

- **run**: `{kind: "run", run_id, stage_id?, trace_ref?, detail?}`.
- **file**: `{kind: "file", path (minLength 1), line? (integer ≥ 1), value?, detail?}`.

### Proposed issue

`{title (1..256), body (minLength 1), type (minLength 1), labels, parent_epic?}`.
`labels` is an array of non-empty strings, uniqueItems, may be empty.
`parent_epic` is a non-empty string.

**`autonomy:*` carve-out.** A proposed `autonomy:` label is STRIPPED at apply
unless the captain authorizes it (`authorize_delegation_tier`, #3924). The scan
may suggest a tier; it never sets one. The repository's work-management
conventions may still add their DEFAULT tier (`label_defaults.autonomy`, e.g.
`autonomy:medium`) when the filed labels carry none — that is the conventions'
choice, not the scan's. See [Apply](#apply-on-gate-decision-3924).

### Advisory object (#3750)

The facts of one vulnerability advisory. The scan agent copies them from the
scanner output (`govulncheck -json` per Go module, `pnpm audit --json` per pnpm
lockfile directory). They are AGENT-ASSERTED: the server cannot re-run the
scanner, so the semantic rules BOUND the claim instead of re-deriving it.

| Field | Required | Shape |
|---|---|---|
| `ecosystem` | yes | `go` / `npm` |
| `package` | yes | 1..214 chars, no whitespace or `<` `>`. **Go: the MODULE path** (`golang.org/x/net`), never the vulnerable package inside it (`golang.org/x/net/http2`); npm: the package name. |
| `version` | yes | the in-use version (the LOWEST across merged manifests, below) |
| `advisory_ids` | yes | 1..20 unique ids; **index 0 is the PRIMARY id** (`GO-` for Go, `GHSA-` for npm), aliases (CVE, GHSA) follow |
| `fixed_version` | yes, nullable | ONE bare version — the lowest patched version on the in-use major line — or an explicit `null` for "no fix". Never a range (rule s). The key is required, so "no fix" is always stated, never implied. |
| `scanner` | yes | `govulncheck` (pairs with `go`) / `pnpm_audit` (pairs with `npm`) / `osv` (either; **RESERVED** — no OSV command is instructed yet) |
| `reachability` | yes | `called` / `imported` / `required` (govulncheck's symbol / package / module levels) / `unanalyzed` (every scanner that reports none) |
| `call_path` | govulncheck only, non-empty there | ≤ 32 frames copied VERBATIM from govulncheck's `trace`, field names and tags govulncheck's own (`module`, `version?`, `package?`, `function?`, `receiver?`, `position?{filename?, offset, line, column}`). Ordered from the VULNERABLE end (index 0) toward the entry point. **A longer trace keeps index 0 onward and truncates the FAR end**, never the vulnerable end. |
| `severity` | yes | `high` / `medium` / `low`, capped by reachability (rule p) |

**Multi-module merge (normative).** One finding per `(primary id, package)`. An
advisory that appears in several manifests (two `go.work` modules pinning the
same module) is ONE finding that cites EVERY manifest where it appears as a file
ref; `version` is the LOWEST in-use version across them, and `reachability` /
`call_path` come from the module with the STRONGEST reachability
(`called` > `imported` > `required`). A call-site source file MAY also be cited
as evidence, but only a manifest (basename `go.mod`, `pnpm-lock.yaml`,
`package.json`) is ever a Dependabot-coverage directory.

**Version comparison.** Coverage compares `fixed_version` against a Dependabot
bump by semver §11 precedence after trimming a leading `v`. Go pseudo-versions
are valid semver prereleases and compare by that precedence; anything that does
not parse (a range, a partial version, empty) is NOT comparable and never covers.

### Source degrades (#3750)

`{source, reason, detail?}` — one entry per source that could not run at all
(any reason but `partial`; the source is then NOT in `sources_scanned`), or that
ran only in part (`partial`; the source IS in `sources_scanned`). `reason` is
`network_unavailable` (the source's database or registry was unreachable),
`tool_unavailable` (the scanner and its pinned fallback could not run),
`tool_failed` (the scanner ran and failed — NOT `pnpm audit` exiting non-zero
because it FOUND vulnerabilities, which is its normal result), `budget_exceeded`,
or `partial`. `detail` is free text, ≤ 500 chars. A scan whose advisory database
was unreachable reports `{advisory, network_unavailable}`, never an empty clean
advisory scan.

## Finding id derivation (normative)

`id = <source> + ":" + <subject>`. Derived, never minted per run, so two reports
over the same tree diff mechanically and a filed issue's marker keeps matching.
The id and subject patterns exclude whitespace and `<` / `>`.

## Hidden marker

An issue filed for a finding carries, in its body:

```
<!-- fishhawk-upkeep:v1 finding_id=<id> -->
```

The match includes the closing ` -->`, so it is exact: the marker of
`flake:TestAB` never matches `flake:TestA`. Pinned by
`TestFindingMarker_Format` (`backend/internal/upkeep`).

## Semantic rules the schema cannot express

Enforced by `plan.CheckUpkeepReportSemantics` after the schema check. Each
violation is a `*plan.SemanticError` naming the JSON pointer; ingest maps it to
`upkeep_report_invalid`.

| Rule | Pointer |
|---|---|
| (a) `id == source + ":" + subject` | `/findings/<i>/id` |
| (b) ids are unique report-wide | `/findings/<i>/id` |
| (c) every finding's `source` is in `sources_scanned` | `/findings/<i>/source` |
| (d) `subject` carries no control rune | `/findings/<i>/subject` |
| (e) a run ref's `run_id` (and `stage_id` when present) parses as a UUID and is not the nil UUID | `/findings/<i>/evidence/<j>/run_id` (`/stage_id`) |
| (f) a `flake` finding cites at least one run ref | `/findings/<i>/evidence` |
| (g) a `toolchain_drift` finding's file refs that carry a `line` name at least 2 DISTINCT paths | `/findings/<i>/evidence` |
| (h) a `deprecation` finding cites at least one file ref | `/findings/<i>/evidence` |
| (i) each label is non-empty, ≤ 50 runes, has no whitespace or control rune, and no leading or trailing punctuation or symbol rune (the `workmgmt` grooming label rule) | `/findings/<i>/proposed_issue/labels/<k>` |
| (j) `parent_epic`, when present, is a positive integer, bare or `#`-prefixed | `/findings/<i>/proposed_issue/parent_epic` |
| (k) at most 200 distinct `run_id`s across the report | `/findings` |
| (l) `source: advisory` ⇔ an `advisory` object is present | `/findings/<i>/advisory` |
| (m) an advisory's `subject == advisory_ids[0] + ":" + package` | `/findings/<i>/subject` |
| (n) scanner/ecosystem pairing: `govulncheck` ⇒ `go`, `pnpm_audit` ⇒ `npm`, `osv` ⇒ either | `/findings/<i>/advisory/scanner` |
| (o) reachability consistency: `govulncheck` ⇒ a non-empty `call_path`, `reachability` equals the level derived from `call_path[0]` (a function ⇒ `called`, else a package ⇒ `imported`, else a module ⇒ `required`) and `call_path[0].module == package`; `pnpm_audit` / `osv` ⇒ `unanalyzed` with no `call_path` | `/findings/<i>/advisory/reachability` (`/advisory/package` for the module) |
| (p) severity cap: `called` ⇒ any; `imported` / `required` ⇒ `low`; `unanalyzed` ⇒ `medium` or `low` | `/findings/<i>/advisory/severity` |
| (q) an advisory cites at least one MANIFEST file ref (basename `go.mod`, `pnpm-lock.yaml` or `package.json`) | `/findings/<i>/evidence` |
| (r) `source_degrades`: at most one entry per source; `partial` ⇒ the source IS in `sources_scanned`, any other reason ⇒ it is NOT | `/source_degrades/<k>` |
| (s) a non-null `fixed_version` is ONE bare version, never a range: none of `<` `>` `=` `^` `~` `\|` `*`, whitespace or a comma | `/findings/<i>/advisory/fixed_version` |
| (t) a report carrying an advisory finding or ANY `source_degrades` entry accounts for `advisory` in exactly one place: in `sources_scanned` (it ran; a partial run adds a `partial` degrade) or named by a non-`partial` degrade (it did not run) | `/sources_scanned` |

Evidence COUNT (minItems 1) is the schema's job and is deliberately not
re-checked by the semantic layer.

Rules (l)–(t) fire only on an advisory finding, an advisory object or a
`source_degrades` entry, so every pre-#3750 stored report (re-parsed by the
dispositions capture and the apply) still parses.

**Residual of rule (t).** A report with NO advisory finding and NO
`source_degrades` entry carries no signal that the advisory source exists, so
zero advisory findings from a scan that never ran the scanners is
indistinguishable from a pre-#3750 report. The rule closes every other shape of
"the tool never ran but the report reads clean"; this one remains, and the
captain reading `sources_scanned` at the gate is its only backstop.

## Dedupe

On ingest each finding's proposed issue is checked against the repo's bounded
window of existing work items (`backend/internal/upkeep.MarkDuplicates`):

1. CLOSED issues never mark, by either basis.
2. **marker**: the lowest-numbered OPEN issue whose body carries the finding's
   exact marker.
3. **similarity**: otherwise the highest-scoring OPEN issue whose title scores
   `medium` or higher on `intakegroom`'s lexical similarity.

A marked finding is reported, not dropped: the apply step skips its filing. A
failed or timed-out tracker read DEGRADES to no duplicates with a named reason
(`reader_error`, `budget_exceeded`, …) and the report is still ingested.

## Dependabot coverage (#3750)

On ingest every advisory finding WITH a `fixed_version` is checked against the
repository's open pull requests (GitHub only, at most 300, one 10s budget;
`backend/internal/upkeep.MarkCovered`). A finding is COVERED iff it cites at
least one manifest and, for EVERY cited manifest directory, some open pull
request authored by `dependabot[bot]`, on a head ref of the finding's ecosystem
(`dependabot/go_modules/…` ⇒ `go`, `dependabot/npm_and_yarn/…` ⇒ `npm`), bumps
the same package in that exact directory to a version at least `fixed_version`.

A covered finding is still PROPOSED and visible at the gate; the apply skips its
filing (`covered_by_dependabot_pr`). Every rule fails toward NOT covered — a
`null` fix, an unparseable version, a grouped pull request spanning several
directories, an unknown ecosystem, a truncated listing — so these residuals only
leave a finding to be filed.

**FALSE-COVER risk (the unsafe direction).** Coverage reaches only the
directories of the manifests the finding CITES. A finding that under-cites (the
module is also pinned in a manifest it does not name) can be marked covered by a
bump that misses that manifest, and is then not filed. Covered marks are only as
complete as the agent's manifest citations; the multi-module merge rule above is
what the scan prompt instructs to prevent it.

Coverage is a snapshot at INGEST: a Dependabot pull request closed between
ingest and the gate still suppresses filing (the next scan re-proposes the
finding), and one opened after ingest does not cover.

## Ingest (`POST /v0/runs/{run_id}/plan`)

The report is bound to the stage's DECLARATION: the stage must be `plan`-typed
and the run's cached workflow must declare `produces: upkeep_report` on it.

| Code | HTTP | `details.reason` | When |
|---|---|---|---|
| `upkeep_report_stage_invalid` | 400 | `stage_type_not_plan` | The shipping stage is not `plan`-typed. Category-B. |
| `upkeep_report_stage_invalid` | 400 | `stage_does_not_declare_upkeep_report` | The stage does not declare `produces: upkeep_report`. Category-B. |
| `upkeep_report_stage_invalid` | 400 | `stage_binding_undecidable` | The run's workflow or the stage's spec entry cannot be resolved; the ingest fails CLOSED. Category-B. |
| `upkeep_report_invalid` | 400 | — | Schema or semantic-rule failure. Category-B. |
| `upkeep_report_invalid` | 400 | `run_ref_invalid` (+ `run_id`) | A cited run is unknown, in another repository, or in another account. One reason for all three. Category-B. |
| `internal_error` | 500 | — | Storage or transport failure; the stage is left untouched. |

**Run-ref tenancy.** A cited `run_id` must name a run in the reporting run's
repository (compared case-insensitively), and in the same account when BOTH
rows carry an account id; otherwise the check is repository-only. Which of
unknown / foreign it was goes to the server WARN log only.

**Unverified pointers.** Only a run ref's `run_id` is checked. Its `stage_id`
and `trace_ref` are AGENT-ASSERTED and UNVERIFIED: `stage_id` is parsed as a
UUID (rule e) but never confirmed to exist or to belong to the cited run, and
`trace_ref` is never resolved.

**Idempotency.** A byte-identical re-POST returns 200 `idempotent: true`, heals a
missing recorded row, and settles the stage; it writes no second artifact.

### `upkeep_report_recorded`

One chained audit row per persisted report:

| Key | Shape |
|---|---|
| `run_id`, `stage_id`, `artifact_id` | UUID strings |
| `content_hash`, `schema_version`, `size_bytes` | artifact identity |
| `entry_counts` | `{findings, flake, toolchain_drift, deprecation, advisory}` — every key always present (one per `sources_scanned` enum member) |
| `duplicates` | ALWAYS a JSON array (`[]` when none or degraded); each `{finding_id, issue_number, issue_url?, basis, score?, confidence?}` |
| `dedupe_degraded` | bool, always present |
| `dedupe_degrade_reason` | present only when degraded |
| `dedupe_scanned_items`, `dedupe_window_truncated` | the window the marks were drawn from |
| `covered` (#3750) | ALWAYS a JSON array (`[]` when none, nothing coverable, or degraded); each `{finding_id, package, pulls:[{number, url?, directory, bumps_to}]}`, one pull per cited manifest directory |
| `coverage_degraded` | bool, always present |
| `coverage_degrade_reason` | present only when degraded: `forge_unsupported`, `github_unwired`, `repo_malformed`, `scope_unavailable`, `pull_list_failed`, `budget_exceeded`, `coverage_panic` |
| `coverage_scanned_pulls`, `coverage_window_truncated` | the open pull requests the marks were drawn from |
| `source_degrades` | ALWAYS a JSON array, copied verbatim from the report (`[]` when it carries none) |

A report with no advisory finding carrying a `fixed_version` records
`covered: []` with no forge read. A degraded coverage read records `covered: []`
and the report is still ingested. On success the stage settles to
`awaiting_approval`.

## Dispositions (`POST/GET /v0/runs/{run_id}/upkeep-dispositions`, #3923)

The captain records a per-finding verdict. Body:
`{dispositions:[{finding_id, verdict, authorize_delegation_tier?, parent_epic?}]}`,
at most 200 entries (the `findings` maxItems). `verdict` is `approved` or
`rejected`. `authorize_delegation_tier` is a BOOLEAN captain authorization for
the finding's OWN proposed `autonomy:*` label; the #3924 apply consumes it. It is
recorded verbatim and is inert when the finding proposes no tier label.
`parent_epic` overrides the proposed issue's parent and is validated with rule
(j). Both are refused on a `rejected` verdict. Each accepted entry is ONE
`upkeep_disposition_recorded` row: `{run_id, stage_id, artifact_id,
content_hash, finding_id, source, verdict, authorize_delegation_tier (always
present), parent_epic (omitempty)}`, actor `user` plus the token subject.
Repeats on one finding collapse last-wins by audit sequence, and both rows stay
in the chain.

**Binding rule.** Dispositions bind to the artifact named by the
HIGHEST-sequence `upkeep_report_recorded` row on the run's chain, never to the
newest artifact. That row is what settles the stage and carries the dedupe
verdict the apply consumes. An artifact whose recorded row never landed is not
bindable until an idempotent retry heals its row. The heal appends a new row, so
a healed older report becomes current. An undecodable newest row, an unreadable
artifact, a wrong kind or a parse failure is a 500, never a fallback to an older
report. The #3924 apply resolves through the same function
(`server.latestUpkeepReport`).

**Capture guarantee.** A capture appends its whole batch in ONE transaction
under the run-row lock. Inside that transaction it re-checks that its artifact is
still named by the highest-sequence `upkeep_report_recorded` row, and that the
artifact's window is still open. A capture that returns 200 therefore landed
against the CURRENT report, below any watermark, and the apply will consume it.
A capture that lost a race to a newer report is refused 409
`upkeep_report_superseded`, and a capture that lost a race to the apply is
refused 409 `upkeep_window_closed`. Both refusals append nothing. A capture is
NOT consumable after the fact if a later report supersedes it: the apply decides
the report that is current when it runs.

**Window.** The window for one artifact closes only when the apply appends an
artifact-bound `upkeep_apply_window_closed` watermark
(`audit.UpkeepWindowAppender.AppendChainedUpkeepWindowClose`) on approve AND
reject of the plan stage carrying it (see [Apply](#apply-on-gate-decision-3924)).
Dispositions below the watermark against that artifact are the consumed set, and
the first watermark is permanent. A reject settles `rejected` before any report
body is read, so an unreadable body cannot keep a rejected window open. An
approve on a contested or ungranted gate (C3) degrades WITHOUT settling, so the
window stays open. Residual: a run that is never decided keeps its window open.

| Code | HTTP | When |
|---|---|---|
| `upkeep_dispositions_unconfigured` | 503 | Run, artifact or audit repository not wired. |
| `authentication_required` | 401 | Anonymous. |
| `run_token_forbidden` | 403 | A run-bound agent token, even for its own run. |
| `operator_agent_forbidden` | 403 | A delegated operator-agent token. |
| `insufficient_scope` | 403 | Missing `write:approvals` (unconditional). |
| `validation_failed` | 400 | Bad `run_id`; unparseable body or trailing content; an unknown key at any depth (the decode is STRICT, #3924: a misspelled `authorise_delegation_tier` is refused, not dropped) or a wrongly-typed value; empty batch or more than 200 entries; empty or duplicate `finding_id`; invalid `parent_epic`; `parent_epic` or `authorize_delegation_tier: true` on `rejected`. |
| `run_not_found` | 404 | Unknown run. |
| `upkeep_verdict_invalid` | 400 | Verdict outside `{approved, rejected}` (`details.allowed`). The body rungs run BEFORE the report lookup, so a bad verdict on a run with no recorded report is this 400, not 409 `upkeep_report_absent`. |
| `upkeep_report_absent` | 409 | No `upkeep_report_recorded` row on the run. |
| `upkeep_finding_unknown` | 422 | A `finding_id` the bound report does not declare (`details.unknown_finding_ids`). Checked for the WHOLE batch first, so nothing is recorded. |
| `upkeep_report_superseded` | 409 | A newer report was recorded after resolution (`details.current_artifact_id`). Re-capture. |
| `upkeep_window_closed` | 409 | The apply settled this artifact's window (`artifact_id`, `settlement`, `watermark_sequence`). |
| `internal_error` | 500 | Storage failure, told apart by `details.recorded` / `details.requested`. An unreadable or unparseable bound report, or a failed window check, fails BEFORE any append: nothing recorded. An ATOMIC batch failure records NOTHING (`recorded: 0`). A read-back failure AFTER the batch COMMITTED leaves every row DURABLE (`recorded` = `requested`; the message says the batch was recorded). A repeat POST is safe in every case because capture is last-wins; `GET` reads back what landed. |

`GET` requires read access only and answers 404, 409 `upkeep_report_absent` and
503 like `POST`. Both verbs return `200 {run_id, artifact_id, stage_id,
content_hash, window_closed, settlement?, dispositions:[{finding_id, source,
verdict, authorize_delegation_tier, parent_epic?, recorded_at, recorded_by,
audit_sequence}]}`, sorted by `finding_id`.

## Apply (on gate decision, #3924)

The plan stage's gate DECISION is the apply trigger. `finishApprovalAdvance`
calls `server.applyApprovedUpkeep` on approve AND reject; no agent stage runs
between the captain's decision and the tracker write. It NEVER creates a run: an
approved finding becomes a tracker issue, not a dispatched change. Long-form
contract: `backend/internal/server/README.md` § "On-approval upkeep apply".

**Ladder** (each rung writes nothing it does not name):

| Rung | Rule |
|---|---|
| E0 | No `upkeep_report` artifact on the decided stage, no `upkeep_report_recorded` row, or the recorded row names an artifact on another stage → write NOTHING. An ordinary plan approval or reject is untouched. An undecodable newest recorded row degrades `upkeep_apply_report_unreadable` on either decision (window stays open). |
| C1 | Reject → settle the window `rejected` from the recorded row's `artifact_id` and file nothing. Runs before any report-body read. |
| C3 | Approve → re-read the stage's approval rows: ≥ 1 grant and 0 rejections, else degrade `upkeep_apply_not_ratified` (window stays OPEN). |
| settle | Append the `approved` watermark; the dispositions below it for the artifact are CONSUMED, collapsed last-wins per `finding_id`. A failed append degrades `upkeep_apply_window_unsettled` (window stays open). Every later degrade leaves the window CLOSED. |

**Per finding, in report order**, exactly one row:

| Outcome | Row | When |
|---|---|---|
| `not_approved` | `upkeep_finding_skipped` | No consumed disposition, or a `rejected` one. |
| `duplicate_of_open_issue` | `upkeep_finding_skipped` | The finding is in the recorded row's `duplicates` (marker or similarity). Carries `duplicate_issue_number`, `duplicate_issue_url`, `duplicate_basis`. |
| `covered_by_dependabot_pr` | `upkeep_finding_skipped` | The finding is in the recorded row's `covered` (#3750). Carries `covering_pr_numbers`, `covering_pr_urls`. |
| `already_filed` | `upkeep_finding_skipped` | An `upkeep_finding_filed` row for this artifact and finding exists (a re-apply). Carries `prior_issue_number`. |
| `apply_budget_exhausted` | `upkeep_finding_skipped` | The detached loop's budget expired before this finding. |
| `filing_failed` | `upkeep_finding_skipped` | The work-item core refused the filing (`code`, `message`); the loop continues. |
| filed | `upkeep_finding_filed` | `{run_id, stage_id, artifact_id, finding_id, source, issue_number, issue_url, provider, title, parent_epic, applied_labels, stripped_labels, idempotency_key, server_rendered?}`. |

`covered` is read from the SAME highest-sequence recorded row as `duplicates`,
on the raw JSON: key ABSENT ⇒ no marks (a row recorded before #3750, whose
report cannot carry an advisory finding); present but `null` or not an array ⇒
degrade `upkeep_apply_coverage_unreadable` (window closed, nothing filed); an
array ⇒ marks by `finding_id`.

A filed issue's body is the proposed body plus the [hidden marker](#hidden-marker)
on its own line, and is stamped with an idempotency key minted from
`(upkeep_finding, run_id, artifact_id, finding_id)`. Its parent epic is the
disposition's `parent_epic` override, else the proposal's, normalized to `#N`.
Its labels are the proposal's minus every `autonomy:*` label unless
`authorize_delegation_tier` is true; `applied_labels` are the labels actually
filed (after the conventions' label completeness), `stripped_labels` the removed
ones.

**Advisory filings are SERVER-RENDERED (#3750).** For an `advisory` finding the
apply IGNORES the agent's `proposed_issue.title` and `proposed_issue.body`. The
title is `<primary id>: <package> <version> (<severity> severity)`; the body is
a `### Advisory facts (server-rendered)` block listing every advisory id, the
ecosystem, package, in-use version, fixed version (or `no fix published`),
reachability, severity and the cited manifest paths — then the hidden marker. No
call-path frame and no agent prose reaches the tracker, and a structured value
that is not a plain token is withheld. The filed row carries
`server_rendered: true`. Type, labels and parent epic follow the ordinary rules.

**Summary.** ONE `upkeep_apply_completed` row per apply: `{artifact_id,
findings, filed, skipped, failed, budget_exhausted, degraded, degrade_reason?}`.
`skipped` counts `not_approved` / `duplicate_of_open_issue` /
`covered_by_dependabot_pr` / `already_filed`; `failed` counts `filing_failed`. A degraded row carries zero counts and one
`degrade_reason`: `upkeep_apply_report_unreadable`, `upkeep_apply_not_ratified`,
`upkeep_apply_window_unsettled` (all three leave the window open),
`upkeep_apply_duplicates_unreadable`, `upkeep_apply_coverage_unreadable`,
`upkeep_apply_prior_filings_unreadable`,
`upkeep_apply_run_unreadable`, `upkeep_apply_repo_unresolvable`,
`upkeep_apply_conventions_unavailable`, `upkeep_apply_prelaunch_timeout`. A
reject writes the `rejected` watermark only — no per-finding or summary row —
unless E0's recorded row is undecodable (one degraded row, no watermark).

**Budgets.** The synchronous half (on the approve request) is bounded by 30s;
the per-finding loop runs detached, drained by server shutdown, under
`max(3m, 3s × findings)`.

**Residuals.** No re-drive: an approved finding left unfiled by a crash, a
degrade, `filing_failed` or budget exhaustion is recovered by the NEXT scan
re-proposing it, whose marker and `already_filed` dedupe prevent a double
filing. A marker match suppresses filing with NO provenance check (a planted
marker suppresses; it never files or escalates). These apply rows are internal
audit categories, visible through `GET /v0/runs/{id}/audit` only. Details in the
server README.

## Plan-path guard

A stage that declares `produces: upkeep_report` may ship ONLY an
`upkeep_report`, or a `clarification_request` (parking writes nothing
approvable, and an operator answer resumes the scan). The guard is an
ALLOWLIST: every other artifact kind — `plan`, `grooming_report`, and any kind
added later — is refused without being stored. When the run's workflow cannot
be resolved at all (no run row, no cached spec, no configured run repository)
the guard fails OPEN for the other kinds and the request takes today's path; a
transport error reading the run is a 500.
