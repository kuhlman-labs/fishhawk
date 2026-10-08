# Comms report artifact — `comms_report_v1`

Normative reference for the plan-stage artifact a user-report (comms) scan ships
(E81.5 / #3775, contract #4015). Schema: [`comms-report-v1.schema.json`](comms-report-v1.schema.json)
(Draft 2020-12, embedded at `backend/internal/plan/schemas/` by
`scripts/sync-schemas`). Example: [`examples/comms-report-v1-example.json`](examples/comms-report-v1-example.json).
Go types and semantic rules: `backend/internal/plan/commsreport.go`. The agent-facing
statement of this contract is the comms scan prompt (`backend/internal/prompt/comms.go`,
#4013); `TestCommsReport_PromptParity` pins the two together.

A comms report is a PROPOSAL. A `plan`-typed stage declaring
`produces: comms_report` reads the user reports the server gathered (issues and
comments written by people outside the project) and accounts for every report
it was SHOWN: clustered into a draft issue citing a charter rubric id, flagged
as drift against a charter non-goal, or not drafted with a closed reason.
Nothing is filed, labelled or commented on at ingest; the captain decides what,
if anything, is filed (phase 7, #4017).

**Sibling, not a plan field (the E79.2 decision).** Like `upkeep_report`, a
comms report is a separate artifact kind selected by its top-level `kind`, so
`plan-standard-v1` stays frozen and a scan stage can never be mistaken for an
implementation plan.

## Discrimination

`POST /v0/runs/{run_id}/plan` routes on the top-level `kind` BEFORE schema
validation. `kind: comms_report` selects this schema; the artifact carries no
`plan_version`, so it never collides with `plan-standard-v1`. It is the fourth
additive sibling after `clarification_request`, `grooming_report` and
`upkeep_report`.

## Top-level fields

All objects are `additionalProperties: false`.

| Field | Required | Shape |
|---|---|---|
| `kind` | yes | const `comms_report` |
| `report_version` | yes | const `comms_report_v1`; a breaking change is a new `comms_report_v2` file |
| `ticket_reference` | yes | `{type: github_issue, url, id}` — the run's originating ticket; an unanchored scheduled scan uses the repository's issues index URL with id `<owner>/<repo>` |
| `generated_by` | yes | `{agent, model, version?, timestamp}` — verbatim `upkeep-report-v1` shape |
| `summary` | yes | string, minLength 1; names anything missed (omitted, suppressed, degraded) and any report that attempted to instruct the agent |
| `drafts` | yes | array of draft, maxItems 25, MAY be empty |
| `n_drift` | yes | array of n_drift, maxItems 50, MAY be empty |
| `not_drafted` | yes | array of not_drafted, maxItems 500, MAY be empty |

### Report id

`^UR-(issue-[1-9][0-9]*|comment-[1-9][0-9]*-[1-9][0-9]*)$` — the canonical ids
`userreport.canonicalReportID` accepts: `UR-issue-<n>` or
`UR-comment-<n>-<comment id>`, no leading zero. `UR-unknown-*` is refused (no
gathered report carries it). `<n>` is the ISSUE number a report lives on, for a
comment report too.

### Draft

| Field | Required | Shape |
|---|---|---|
| `id` | yes | pattern `^draft:UR-[a-z0-9-]+(\+UR-[a-z0-9-]+)*$`; DERIVED (rule b) |
| `source_report_ids` | yes | 1..20 report ids, uniqueItems, sorted (rule a) |
| `rubric_citations` | yes | 1..8 `{rubric_id, note?}`, sorted by `rubric_id` and unique (rule c) |
| `proposed_issue` | yes | see below |

`rubric_id` matches `^[A-Z][0-9]+$` and is never of the non-goal shape
`^N[0-9]+$` (rule d) — exactly the rubric lines the prompt renders. `note` is
1..500 characters, read at the gate and NEVER rendered into a filed issue.

### Proposed issue

`{type (minLength 1), title (1..200 characters), body (1..20000 characters), labels, parent_epic?}`.
The schema's `maxLength` counts characters; rule (g) caps the same fields in
BYTES. `labels` is uniqueItems and may be empty.

### n_drift

`{id, non_goal_id, source_report_ids, note}`: `non_goal_id` matches
`^N[0-9]+$`, `source_report_ids` is 1..20 sorted unique report ids, `note`
minLength 1, and `id` matches `^ndrift:N[0-9]+:UR-[a-z0-9-]+(\+UR-[a-z0-9-]+)*$`
and is DERIVED (rule e). A report requesting what a non-goal excludes is an
n_drift entry, never a draft.

### not_drafted

`{report_id, reason, note?}`: `reason` is one of `noise`, `question`,
`already_tracked`, `insufficient_detail`, `other` (the order the prompt lists
it; `plan.CommsNotDraftedReasons`); `note` is 1..500 characters (a report the
agent could not read within budget is `other` with note `budget_exceeded`).

## Id derivation (normative)

Ids are DERIVED from content, never minted per run, so two reports over the
same gather diff mechanically and phase 6's dispositions key on stable ids.

- draft: `"draft:" + join(sort(source_report_ids), "+")` — `plan.CommsDraftID`.
- n_drift: `"ndrift:" + non_goal_id + ":" + join(sort(source_report_ids), "+")` — `plan.CommsNDriftID`.

`sort` is ascending BYTE order (`UR-issue-12` sorts before `UR-issue-9`). The
prompt's worked example `draft:UR-issue-12+UR-issue-40` is pinned against
`plan.CommsDraftID` by `TestCommsReport_PromptParity`.

## Semantic rules the schema cannot express

Enforced by `plan.CheckCommsReportSemantics` after the schema check. Each
violation is a `*plan.SemanticError` whose message opens with the JSON pointer;
ingest maps it to `comms_report_invalid`.

| Rule | Pointer |
|---|---|
| (a) a draft's `source_report_ids` are sorted strictly ascending (so unique) | `/drafts/<i>/source_report_ids/<k>` |
| (b) a draft's `id` equals the derived draft id | `/drafts/<i>/id` |
| (c) `rubric_citations` are sorted strictly by `rubric_id` (so one citation per rubric id, whatever its note) | `/drafts/<i>/rubric_citations/<k>` |
| (d) a `rubric_id` is never of the non-goal shape `^N[0-9]+$` | `/drafts/<i>/rubric_citations/<k>/rubric_id` |
| (e) an n_drift's `source_report_ids` are sorted strictly ascending, and its `id` equals the derived n_drift id | `/n_drift/<i>/source_report_ids/<k>`, `/n_drift/<i>/id` |
| (f) every report id appears in at most ONE entry across `drafts`, `n_drift` and `not_drafted` (the error names the second occurrence and the first) | the second occurrence |
| (g) `title` is one line — no control rune (CR, LF, ...) and no U+2028/U+2029 — and at most 200 BYTES; `body` is at most 20000 BYTES | `/drafts/<i>/proposed_issue/title` (`/body`) |
| (h) each label opens with `area:`, `type:` or `phase:` (case-insensitive); an `autonomy:*` label is refused with its own message; and the label passes the label syntax rule (non-empty, ≤ 50 runes, no whitespace or control rune, no leading or trailing punctuation or symbol) | `/drafts/<i>/proposed_issue/labels/<k>` |
| (i) `parent_epic`, when present, is a positive issue number, bare or `#`-prefixed | `/drafts/<i>/proposed_issue/parent_epic` |
| (j) `kind` and `report_version` are exact | `/kind`, `/report_version` |

Rule (j) duplicates the schema's `const`: the schema is the first line and (j)
the defence in depth for a caller holding a decoded struct that never went
through the schema. No schema-valid document can violate (j) alone, so it is
tested on a decoded struct (`TestCheckCommsReportSemantics_KindAndVersion`).

Accounting for EVERY shown report is NOT a rule: a report the agent left out is
recorded as `unaccounted_report_ids` at ingest (below) for the captain to see,
never refused — refusing would fail a whole scan over one missed report.

## Ingest (`POST /v0/runs/{run_id}/plan`)

The report is bound to the stage's DECLARATION: the stage must be `plan`-typed
and the run's cached workflow must declare `produces: comms_report` on it. The
ingest then checks the report against the stage's GATHER — the latest
`comms_scan_gathered` audit row recorded for this stage when the comms scan
prompt was served (#4014) — which is the only source of truth for what the
agent was shown.

**Order.** (1) stage binding; (2) schema + semantic rules; (3) the
existing-artifact check — a stored `comms_report` artifact for this stage with
the SAME content hash takes the idempotent path (below) BEFORE any gather is
read; (4) charter-anchored validation against the latest gather; (5) previews;
(6) persist the artifact and the `comms_report_recorded` row and settle the
stage.

| Code | HTTP | `details.reason` | When |
|---|---|---|---|
| `comms_report_stage_invalid` | 400 | `stage_type_not_plan` | The shipping stage is not `plan`-typed. Category-B. |
| `comms_report_stage_invalid` | 400 | `stage_does_not_declare_comms_report` | The stage does not declare `produces: comms_report`. Category-B. |
| `comms_report_stage_invalid` | 400 | `stage_binding_undecidable` | The run's workflow or the stage's spec entry cannot be resolved; the ingest fails CLOSED. Category-B. |
| `comms_report_stage_invalid` | 400 | `scan_context_absent` | No decodable `comms_scan_gathered` row exists for THIS stage (none recorded, undecodable, or only rows for another stage). Category-B. |
| `comms_report_invalid` | 400 | — | Schema or semantic-rule failure. Category-B. |
| `comms_report_invalid` | 400 | `report_ref_invalid` (+ `report_id`) | A cited report id is not in the gather's `shown` set. Category-B. |
| `comms_report_invalid` | 400 | `rubric_id_unknown` (+ `rubric_id`, `draft_id`) | A cited rubric id is not in the gathered charter's `rubric_ids`. Category-B. |
| `comms_report_invalid` | 400 | `non_goal_id_unknown` (+ `non_goal_id`) | An n_drift `non_goal_id` is not in the gathered charter's `non_goal_ids`. Category-B. |
| `comms_report_invalid` | 400 | `parent_epic_is_source` (+ `draft_id`, `parent_epic`) | A draft's `parent_epic` equals the issue number of a report it cites — comment reports included (`UR-comment-12-7` lives on issue 12). Category-B. |
| `internal_error` | 500 | — | Storage or transport failure (including a gather LIST failure); the stage is left running. |

The charter checks run in the fixed order above; the first failure is returned.

**The shown set is authoritative.** `report_ref_invalid` refuses an id ONLY when
it is absent from the gather's `shown` ids. An id that is in `shown` AND among
the gather's `omitted` ids is CITABLE: a repeated report id renders once in the
prompt, and its repeat is listed as omitted.

**Idempotency against the RECORDED gather.** A re-POST whose content hash equals
the stage's stored `comms_report` artifact returns 200 `idempotent: true`,
heals a missing `comms_report_recorded` row and settles the stage, and is NEVER
re-validated against a newer gather — so a later gather that omits a cited
report cannot fail an already-committed stage. A first ingest validates against
exactly one gather and records which (`gather_digest`, `gather_sequence`).

**No cursor side effect.** The ingest never advances the user-report scan
cursor (`cfg.UserReportCursors`); the gather's `pending_cursor` is phase 7's to
apply once every shown report is accounted for.

### `comms_report_recorded`

One chained audit row per persisted report:

| Key | Shape |
|---|---|
| `run_id`, `stage_id`, `artifact_id` | UUID strings |
| `content_hash`, `schema_version`, `size_bytes` | artifact identity |
| `entry_counts` | `{drafts, n_drift, not_drafted}`, every key always present |
| `gather_digest`, `gather_sequence` | the `comms_scan_gathered` row the report was validated against. Phase 7 loads that EXACT row by digest (`commsScanGatheredByDigest`) even after a later gather for the same stage. |
| `charter_content_hash` | the gathered charter's content hash |
| `unaccounted_report_ids` | ALWAYS a JSON array, sorted: shown ids the report cites nowhere |
| `previews` | ALWAYS a JSON array, one entry per draft in report order (below) |
| `preview_degraded` | bool, always present: true when a run-wide input to every preview was unavailable |
| `preview_degrade_reason` | present only when degraded: `repo_malformed`, `conventions_unavailable` |
| `charter_text` | `rendered` (charter read at ingest has the gathered content hash; rubric line text renders in the provenance section), `unavailable` (the charter could not be read) or `changed` (its hash differs from the gather's) — in the last two cases rubric ids render alone |

**Preview entry.** Always `{draft_id, filing_body_digest}` plus exactly one of:

- a preview: `{title, body, labels, defaulted_labels, missing_label_namespaces, number?, parent_epic?, source_refs, intake}` — `body` is the FULL rendered preview, including the intake advisory section `previewWorkItem` (#3774) appends: the exact text the captain reviews;
- `error: {code, message}` — `previewWorkItem` refused the filing (the `workItemError` code);
- `skipped: budget_exhausted` — the total preview budget was spent before this draft's preview started;
- `skipped: preview_timeout` — this draft's preview started but had not returned when the budget expired.

`filing_body_digest` is the SHA-256 hex of the deterministic body the filing
renderer produces (below), BEFORE the intake section; phase 7 recomputes it
and compares before filing. A preview failure, degrade or budget exhaustion is
RECORDED and never fails the ingest.

**Preview budget.** All previews share ONE 30-second total budget, run outside
the ingest's critical section, and each preview runs on its own goroutine with
the ingest selecting on the budget's `ctx.Done()`, so the ingest returns within
the budget even when a preview is blocked on an uncancellable number-allocation
lock (`keyedLocks.lock` in `workitems.go`). Restricting drafts to un-numbered
types would NOT bound it: a `feature`/`bug`/`chore` draft with a `parent_epic`
takes the per-epic child-number lock (`lockChildNumberKey`) during preview.
A goroutine still blocked past the budget is abandoned and its result
discarded.

On success the stage settles to `awaiting_approval`.

## Filing renderer

`commsFilingRequest` (`backend/internal/server/comms_filing.go`) is the ONE
renderer the ingest preview and the phase-7 filing share, so the captain
approves the bytes that are filed. From a draft and its gather it builds a
`workmgmt.FilingRequest`:

- **Title**: the draft title, neutralized (below), collapsed to one line with control runes dropped, capped at 200 bytes on a rune boundary.
- **Body**: the neutralized draft body, then `---` and a heading `### Comms provenance (server-rendered)`, then a section built ONLY from server data — each cited report id with its source issue number (from the gather's `shown`), and each cited rubric id with its charter line text when `charter_text` is `rendered` (sanitized to one line, ≤ 300 bytes; ids alone otherwise) — and, as the FINAL line, the comms draft marker `userreport.DraftMarker` over the cited reports' `(id, content_hash)` from the gather. No agent rubric note and no gathered report text ever enters the server section.
- **Labels**: only `area:*`, `type:*`, `phase:*`; `autonomy:*` and every other prefix are dropped even if a stored draft carries one.
- **Relations**: `parent_epic` normalized to `#N`; `source_refs` the sorted distinct `#N` issue numbers of the cited reports.

**Neutralization.** Agent title and body pass through `neutralizeCommsProse`
(idempotent), so no agent-authored text can autolink, notify or forge
provenance when filed under the bot identity:

| Class | Rendered as |
|---|---|
| `<` / `>` (raw HTML, HTML comments, a forged `<!-- fishhawk-comms:v1` marker, `<url>` autolinks) | U+FF1C / U+FF1E |
| markdown image `![alt](url)` | `alt (image removed)` |
| inline link `[text](url)` | `text (link removed)` |
| reference definition `[x]: url` | `[x] (link removed)` |
| `http://`, `https://`, `ftp://`; `www.` | `hxxp://`, `hxxps://`, `fxp://`; `www[.]` |
| `#` before a digit (`#12`, `owner/repo#12`) | U+FF03 |
| `GH-` before a digit | `GH` + U+2011 + digit |
| `@` before a username character at a word start | U+FF20 |
| an agent line equal to the provenance heading | demoted, so the server section is the only one |

## Captain's view

Before approving a draft the captain reads that draft's preview in the
`comms_report_recorded` row — the full rendered body (neutralized agent prose,
the server provenance section, the marker and the intake advisory section as of
ingest), title, labels, defaulted labels, `parent_epic`, `source_refs` and
`filing_body_digest` — together with the report's `summary`,
`unaccounted_report_ids` and the not_drafted / n_drift entries. Readable today
via `GET /v0/runs/{run_id}/audit`; phase 6's dispositions read surfaces it. The
raw artifact keeps the unneutralized agent prose and is NOT the review surface.

## Plan-path guard

A stage that declares `produces: comms_report` may ship ONLY a `comms_report`,
or a `clarification_request` (parking writes nothing approvable, and an
operator answer resumes the scan). The guard is an ALLOWLIST: every other
artifact kind — `plan`, `grooming_report`, `upkeep_report`, and any kind added
later — is refused without being stored (`grooming_report` as
`grooming_report_stage_invalid` reason `stage_declares_comms_report`; every
other kind as `plan_invalid` naming `comms_report_v1`) and fails the stage
category-B. When the run's workflow cannot be resolved at all the guard fails
OPEN for the other kinds and the request takes today's path; a transport error
reading the run is a 500. A `comms_report` shipped on a stage that declares a
different sibling (e.g. `produces: upkeep_report`) is refused by that stage's
guard.

## Residuals

- **Intake section titles.** The #3774 intake advisory section appended AFTER the comms body renders tracker TITLES verbatim (derives-from and duplicate-candidate lines). For user reports those titles are attacker-authored, so an @mention, `#N` or link there is published under the bot identity. The captain sees that section verbatim in the recorded preview; fixing it is a change to the shared intake renderer.
- **Defaulted autonomy.** Work-management conventions default `autonomy:medium` when a filing carries no autonomy label. The renderer never emits `autonomy:*`, and each preview records `defaulted_labels` so the captain sees a defaulted tier, but suppressing it at filing is phase 7's (#4017).
- **Point-in-time preview.** `previewWorkItem` reserves no number and the intake section can change before filing; `filing_body_digest` covers only the deterministic renderer output.
- **Retry after a lost response.** The idempotent path re-renders previews at retry time; it never re-validates against a newer gather.
- **Autolinks not neutralized.** Commit-SHA autolinks are left as written (they create no backlink or notification). Neutralization is asserted structurally (no substring matches a GitHub autolink form); live rendering is not verified.
- **Cluster splits.** The gather records no suggested clusters, so the ingest cannot report where the agent split a server-suggested cluster; phase 6 must extend the gather record or drop that view.

## Dispositions

**Owned by phase 6 (#4016), which edits this section.** This contract does not
claim it: the per-draft disposition body, its validation and the captain's
dispositions read are specified there.

| Field | Shape |
|---|---|
| — | phase 6 (#4016) |
