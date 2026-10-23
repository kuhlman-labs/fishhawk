# Decision records (`docs/adr/`)

The in-repo record of settled architectural decisions (E78.1 / #3722). One file
per ADR, `NNN-slug.md`: a machine-readable front matter followed by the tracker
issue body **transcribed verbatim** — the record transcribes, it never
corrects, so a path or option named in a body is kept as named even if it has
since moved. Enforced by `scripts/check-adr` in `scripts/test verify` (gate
contract: `scripts/README.md` § "ADR record gate").

## When a change needs an ADR

Drafted by the first officer at the Captain's direction (2026-10-23); it is the Captain's to accept in the PR that adds it. A change needs an ADR (filed with `fishhawk_file_issue` type `adr`, ratified by the Captain under ADR-113) when ANY of these holds. Otherwise an ordinary issue is enough.

1. **Boundary.** It creates or changes a component boundary, a persistence shape (a new table, state or state machine), a protocol or wire contract (a REST, MCP or cross-module shape), or a trust or authorization boundary. These are the `new_architectural_decision` triggers of `docs/spec/plan-standard-v1.schema.json`.
2. **A ratified ADR.** It contradicts, reverses or departs from a ratified ADR, or a plan condition would fill a gap in one. A plan gate brings such a condition to the first officer BEFORE approval, and only the Captain ratifies its resolution.
3. **Cross-cutting policy.** It sets a rule that governs many future changes. Examples: stage parallelism (ADR-115), review measurement (ADR-117, ADR-118), repository-agnostic design (ADR-110).
4. **Safety posture.** It changes the merge path, approvals, the dispatch ledger, credentials or another security control, or it is hard to reverse.
5. **A real choice.** Several viable options with material trade-offs, where the choice belongs to the Captain.
6. **Product behaviour.** It changes how Fishhawk behaves for every repository, not just this one (ADR-110).

**An issue is enough** for:
- implementation inside existing decisions, including the children of an epic that implements an ADR;
- a bug fix that restores intended behaviour;
- tests, documentation, refactors and follow-ups.

**Which form an ADR takes:**
- **An in-body amendment** (an "Amendment A<n>" section added to the ratified ADR's tracker body, then re-transcribed) clarifies or narrows that ADR's own rules. Examples: ADR-112 A1, ADR-114 A1.
- **A new ADR that `amends`** adds a decision of its own within another ADR's area. Examples: ADR-116, ADR-118, ADR-119, ADR-120.
- **`supersedes`** is for a FULL replacement only. A partial replacement is an amendment, or a clarifying line on the old record (see the status rule).

**Titles** are at most 100 characters from ADR-119 onward (`C-title-length`).

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
  `superseded_by`, `amends`, `amended_by`, `applies_to`. Unknown and duplicate
  keys are violations.
- Required: every key except `date`, `amends` and `amended_by`. The required
  list keys (`supersedes`, `superseded_by`, `applies_to`) are present even
  when empty (`[]`). `amends` and `amended_by` are OPTIONAL list keys: absent
  means `[]`, so only a record that states an amendment carries them, written
  after `superseded_by`. Every list key must be a list; the other keys must be
  scalars.
- `date` is optional and, when present, a real `YYYY-MM-DD` date.

## Field semantics

| Key | Meaning |
|---|---|
| `id` | `ADR-NNN`; equals `ADR-` + the filename's 3-digit prefix. |
| `title` | The issue title without its `[ADR-NNN] ` prefix. From ADR-119 on, at most 100 characters (Unicode code points, Python `len`): a longer issue title is shortened for the record, and `scripts/check-adr` refuses it as `C-title-length` (#4699). ADR-001 – ADR-118 are grandfathered and keep their full titles. |
| `status` | One of `proposed`, `accepted`, `rejected`, `superseded`, `unknown` — see the inference rule below. |
| `date` | The date the Decision section or its heading states (`Recorded 2026-04-30`, `Accepted (2026-07-26)`, `Decided 2026-06-09`, `## Decision (2026-05-19)`, `Resolved on 2026-05-26`). Omitted when none is stated. A separate Resolution section's or an implementation-landed date is NOT a decision date. |
| `issue` | The tracker issue URL the body is transcribed from. |
| `supersedes` / `superseded_by` | ADR ids, only when a source STATES the supersession. Reciprocal: `A.supersedes ∋ B ⇔ B.superseded_by ∋ A`. Every id must name an existing record other than itself, so a link to a not-yet-backfilled ADR is impossible by construction — the batch that backfills the superseding record adds both sides. |
| `amends` / `amended_by` | ADR ids, only when a source STATES an amendment of that record and states no supersession of it (E78.13 / #4471). Reciprocal: `A.amends ∋ B ⇔ B.amended_by ∋ A`, and a record without the key reads `[]`. Every id must name an existing record other than itself. An amended record still governs, except where an amendment decides otherwise; it is not superseded and keeps its status. **Precedence:** a source that states BOTH an amendment and a supersession (even partial) of the same ADR is encoded as `supersedes` only, and one id never appears in both relations of one record (`supersedes` + `amends`, `superseded_by` + `amended_by`). This rule is the operator's decision, ratified in the #4471 plan approval (condition 5), not something the issue implies. |
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
section whose heading matches `^#{1,6}\s(?:[^/\n]*/)*\s*Decision\b`
(case-insensitive, and written unambiguously so a failing match is linear —
E78.3 / #3724, pinned by `scripts/test-adr` h36/h36b: `Decision` starts the heading or starts a slash-separated
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

`path` is repo-relative. `amends` / `amended_by` are optional entry fields,
written by `--write-index` ONLY when non-empty, between `superseded_by` and
`applies_to`, so the records without an amendment cost the committed file no
bytes (always-present keys would add ~4 KB). An absent optional field means
`[]` to the gate and to `backend/internal/decisionrecord`'s strict parser
alike.

The architect persona is shown a COMPACT rendering of this file, not the file
itself (`decisionrecord.RenderIndex`, #4478): one JSON object per record, one
record per line, with every empty list left out, and (`adr-index-lines-v2`,
#4699) every title over 100 characters cut to its first 99 plus `…` at render
time; the stored title here and in the record is unchanged. It is that
rendering, not the committed file, that must fit the 32,768-byte (32 KiB)
decision-record index cap; `TestAssemble_ShippedIndex_RenderingFitsCap` (and
`TestShippedIndex_WithinInjectionCap`) in `backend/internal/decisionrecord`
fail the change whose record makes it overflow, and
`TestAssemble_ShippedIndex_HeadroomFloor` fails the change that leaves less
than 10 of the largest rendered line of headroom — the signal to tier the
index or raise the cap, an operator decision. Measured 2026-10-23: 113 records
render to 24,200 bytes (27,143 uncapped), leaving 8,568 bytes, about 21 more
at the largest current line (405 bytes). The gate compares the
index to the records semantically, keyed by path: a record with no entry, an entry naming no record
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
  ADR-031 itself was recorded `unknown` (`accepted` since the Captain's
  2026-10-23 status rulings). ADR-040 is transcribed from the
  decision record #997, not from the five implementation issues that also
  carry an `[ADR-040]` title.
- **Batch 3 (E78.3 / #3724):** ADR-056 – ADR-080, the last batch. Landed with
  the two cross-batch supersessions the sources state, each front matter only
  on the older record (its body is untouched):
  - ADR-061 supersedes ADR-058's tenant definition ("Supersedes ADR-058's
    tenant definition."), so ADR-058 carries `superseded_by: ["ADR-061"]`.
    ADR-058's record is titled for GitLab second-forge support but carries the
    "tenant = forge account" definition that ADR-061 supersedes. ADR-058 stays
    `accepted` (only its tenant definition is replaced) and ADR-061 itself was
    recorded `unknown` (`superseded` by ADR-057 since the Captain's 2026-10-23
    status rulings).
  - ADR-076 supersedes ADR-033's loopback gate ("This **supersedes ADR-033's
    gate**"), so ADR-033 carries `superseded_by: ["ADR-076"]`. ADR-033 stays
    `accepted`: ADR-076 states "ADR-033's other decisions (stdio default;
    transport-agnostic tool registration; loopback hard-enforcement for the
    bare-bearer HTTP mode) stand" — the ADR-018/ADR-031 partial-supersession
    precedent.

  Deliberately NOT linked, because no source states an ADR supersession:
  ADR-066's "This **supersedes** the `operator_agent.may_*` shape" names a
  grammar shape, not ADR-040; ADR-073 retires the ECS path without stating that
  it supersedes ADR-009, ADR-016 or ADR-034 (it names ADR-034 only as the local
  Docker-Desktop path); ADR-072 says of ADR-011 "That ADR stays parked for
  market signal"; ADR-064's and ADR-078's "supersedes" refer to their own
  Recommendation; ADR-067's "which ADR-055 already superseded as the preset
  default" names a spec surface, not an ADR. `applies_to` near-misses, also
  left out: ADR-056 names `.fishhawk/checkpoints` in its Decision only as
  Option B's as-written assumption; ADR-067 names its schema mirrors only as
  directories, so only the canonical `docs/spec/workflow-v2.schema.json` is
  listed; ADR-073 names `deploy/helm/fishhawk/` only in Context. ADR-063 and
  ADR-078 are `accepted` with no `date`, because their Decision sections state
  none (the batch-2 ADR-038 / ADR-046 precedent).

## Upkeep rule

The record covers ADR-001 – ADR-080. From ADR-081 on, records are added one by
one, never in batches:

- A ratified ADR lands its `docs/adr/NNN-slug.md` record in the same change
  that records the ratification, or in the change immediately after.
- The adding change regenerates `index.json` (`scripts/check-adr
  --write-index`), adds the row under "Records" below and, when the new ADR
  states a supersession, edits the superseded record's `superseded_by` — or,
  when it states an amendment and no supersession, the amended record's
  `amended_by` — (front matter only) in the same change.
- A record whose status the inference rule leaves `unknown` adds a bullet to
  "Status unknown" below, quoting its source verbatim.
- Until its record lands, an ADR is read from the tracker (`gh issue list
  --label adr`).

### `applies_to` review of superseding records (E86.58 / #4398)

Decision-record selection follows supersession from a selected record to its
successors, so the 11 records that supersede another were re-read against the
`applies_to` rule above, applied unchanged. Only ADR-098 gained a glob:

- ADR-031: `[]` — names no repository path.
- ADR-061: `[]` — names no repository path.
- ADR-076: `[]` — `cli/internal/credstore` and `cli/cmd/fishhawk/run.go`
  appear only in Context's inventory.
- ADR-090: `[]` — `merge_run.go` appears only in Context; the Decision names
  routes, and the Consequences cite `.fishhawk/workflows.yaml` (a timeout) and
  `backend/internal/audit/categories.go` (a registration).
- ADR-092: `[]` — names no repository path.
- ADR-093: `[]` — names only the `categories.go` registration.
- ADR-094: `[]` — its Consequences name `docs/spec/workflow-v2.md` only as a
  document to update, the same documentation-update class as ADR-095's
  README; when in doubt, `[]`. A glob there would also attach this
  concurrency-admission record to unrelated workflow-spec changes.
- ADR-095: `[]` — names only the `categories.go` registration and
  `backend/internal/server/README.md` (documentation).
- ADR-096: `[]` — its Decision and Consequences name no path; the D3
  global-surface list is the set's CONTENTS, in the Recommendation.
- ADR-098: `["backend/internal/basedelta/**"]` — its Consequences
  "Implementation" bullet places the G/D split and data-read edges there.
- ADR-099: `[]` — `docs/BRAND_FOUNDATIONS.md` appears only in Context and in
  Recommendation D4.

A path named only in a Recommendation that a Decision adopts "as written"
(ADR-096, ADR-099) does not count under the current rule; widening the rule to
count it is an operator decision.

Since E78.13 / #4471, ADR-095 is encoded as an AMENDMENT of ADR-091, not a
supersession, and selection follows amendments (`amended_by`) as it follows
successors, so the same `applies_to` reasoning covers the amending records
below.

### Amendment relation (E78.13 / #4471)

The record had no amends relation, so ADR-095, ADR-100 and ADR-101 encoded a
stated amendment as `supersedes`, against the "only when a source STATES the
supersession" rule. They now carry `amends` (front matter only; no body
touched):

- ADR-095: `amends: [ADR-091]` ("This amends ADR-091 D6 and leaves D6 itself
  unchanged"); ADR-091 `superseded_by: []`, `amended_by: [ADR-095]`.
- ADR-100: `amends: [ADR-087, ADR-094]` ("explicit amendments to
  ADR-087/094"; "ADR-087: amends its ... rule", "ADR-094: amends D4 ... and
  Q1"); ADR-087 keeps `superseded_by: [ADR-094]` and gains `amended_by:
  [ADR-100]`; ADR-094 `superseded_by: []`, `amended_by: [ADR-100]`.
- ADR-101: `amends: [ADR-040, ADR-089, ADR-097]` ("ADR-040: amended",
  "ADR-089: amended for charter only", "ADR-097: amended for charter only");
  ADR-040, ADR-089 and ADR-097 drop ADR-101 from `superseded_by` and list it
  in `amended_by`.
- Added by the audit: ADR-097 `amends: [ADR-040, ADR-089]`. Its Relations
  section states "Amends ADR-089 (#4149) D1 and ADR-040 (#997)", its Q4 asks
  whether it amends both "without superseding either", and it states no
  supersession. ADR-040 and ADR-089 therefore carry `amended_by: [ADR-097,
  ADR-101]`.
- ADR-101's "ADR-085: amended" was not encodable while no ADR-085 record
  existed. The change that backfills ADR-085 adds both sides: ADR-101
  `amends` gains ADR-085, and ADR-085 carries `amended_by: [ADR-101]`. ADR-085
  stays `accepted`; ADR-101 states no supersession of it.

Amendments recorded under the upkeep rule since:

- ADR-103: `amends: [ADR-050]` ("Adopt **Option D**, as a new ADR that amends
  ADR-050"; "It supersedes nothing. It amends ADR-050 D4 (a new Rule-of-Two
  row) and tightens D1's allow-list for this variant"); ADR-050 `amended_by:
  [ADR-103]`. ADR-050 keeps its `accepted` status.
- ADR-104: `amends: [ADR-022, ADR-024]` ("**Amends:** ADR-024 (Q4, Q5) and
  the ADR-022 addendum (a third identity: host dispatcher)"); ADR-022 and
  ADR-024 each carry `amended_by: [ADR-104]`. ADR-022 kept its status (then
  `unknown`): an amendment does not change the amended record's status.
  ADR-022 is `accepted` since the Captain's 2026-10-23 status rulings.
- ADR-105: `amends: [ADR-057, ADR-061]` ("Amends ADR-057 (the repo-ACL key)
  and ADR-061 (workspace ↔ installation, the two-sided adopt)"); ADR-057 and
  ADR-061 each carry `amended_by: [ADR-105]` and kept their status (then
  `unknown`); ADR-061 keeps `supersedes: [ADR-058]`. ADR-105 states no
  supersession. Since the 2026-10-23 rulings ADR-057 is `accepted` and
  ADR-061 is `superseded` by ADR-057; ADR-105 keeps `amends: [ADR-061]`.
- ADR-107: `amends: [ADR-025]` ("It amends ADR-025 and does not supersede
  it"); ADR-025 `amended_by: [ADR-107]`.
- ADR-109: `amends: [ADR-093]` ("It amends ADR-093"; "ADR-093 D1 says an
  up-to-date PR with a valid pass "may merge inline, as today". This amendment
  narrows that."); ADR-093 keeps `supersedes: [ADR-090, ADR-092]` and gains
  `amended_by: [ADR-109]`. ADR-109 states no supersession of ADR-093.
- ADR-084 is recorded with this batch because ADR-118 amends it. Its tracker body is transcribed after removing the automatically appended "Intake signals" section, as for ADR-113 and ADR-114.
- ADR-112: `amends: [ADR-093]` ("it **amends ADR-093** (D4, D6, D7, and the mechanics of Q1(a)…)"; "It does not supersede either"); ADR-093 gains `amended_by: [ADR-112]`. Its in-body "Amendment A1" (2026-10-23) is part of the transcribed body, and adds no edge.
- ADR-113: `amends: [ADR-055]` (Record metadata); ADR-055 `amended_by: [ADR-113]`.
- ADR-114: no edge. Its in-body "Amendment A1: token callers" (2026-10-23) is part of the transcribed body.
- ADR-115: `amends: [ADR-063, ADR-092, ADR-093, ADR-094, ADR-112]` (Record metadata). Each gains `amended_by: [ADR-115]`. ADR-092 keeps `superseded_by: [ADR-093]`, and ADR-094 keeps `amended_by` ADR-100.
- ADR-116: `amends: [ADR-041, ADR-115]` ("It amends ADR-041 decision 4 … and ADR-115 D5"); both gain `amended_by: [ADR-116]`.
- ADR-117: no edge (`amends: []` in its Record metadata).
- ADR-118: `amends: [ADR-068, ADR-084, ADR-117]` ("It amends ADR-068 Decision 3, ADR-084 … and ADR-117 D4"); each gains `amended_by: [ADR-118]`. Its `applies_to` names `backend/internal/agenteval/**` and `scripts/check-review-prompt-eval`, as its Record metadata states.
- ADR-119: `amends: [ADR-042]` ("It amends ADR-042: fix-up session resume is permitted on the local runner only under these rules"); ADR-042 gains `amended_by: [ADR-119]`. Per ADR-119 D7, ADR-042's tracker body now names `runner/cmd/fishhawk-runner/fixupresume.go` and `runner/internal/agent/claudecode/**`, and its `applies_to` matches ADR-119's, so a change to resume selects both records (`Select` follows amendments forward only).
- ADR-120: `amends: [ADR-084, ADR-113]` ("It amends ADR-084 (D4 and rule 4's whole-index injection) and ADR-113 (D4's live index-size checks and frozen-tree contents)"); both gain `amended_by: [ADR-120]`. Its `applies_to` names the paths its Record metadata states.
- ADR-090: its tracker body (#4170) now carries the in-body "Amendment 1 (2026-10-14)" the record already held, so the record is again a verbatim transcription.
- ADR-109: its tracker body (#4601) gained "## Amendment A1 (2026-10-23)" (ratified by the Captain), an in-body amendment of its own ledger rules (an orphaned queue dispatch claim is released on forge evidence), so the body is re-transcribed and no `amends`/`amended_by` edge is added.
- ADR-051: its tracker body gained "## Amendment (2026-10-16)", an in-place amendment of its own text, so the body is re-transcribed and no `amends`/`amended_by` edge is added.
- ADR-041: `amends: [ADR-032]`. Its Consequences state "Revises **ADR-032**
  (#714)", and the Captain confirmed on 2026-10-23 that this is an amendment,
  not a supersession (now stated in #1140's body: "**Amends ADR-032**").
  ADR-032 carries `amended_by: [ADR-041]` and is `accepted`.
- Not an amendment: ADR-057 → ADR-061 is a SUPERSESSION. The Captain
  confirmed on 2026-10-23 that ADR-061 (#2069, closed as a duplicate) was
  folded into ADR-057 as its Amendment A1, and both tracker bodies now state
  it (#1823: "Amendment A1 supersedes ADR-061 (#2069)"). So ADR-057
  `supersedes: [ADR-061]` and ADR-061 is `superseded` (`superseded_by:
  [ADR-057]`). ADR-061 keeps `supersedes: [ADR-058]` and `amended_by:
  [ADR-105]`.

Decision-record selection (`backend/internal/decisionrecord`) follows
`amended_by` from a selected record to its amendments and ranks a
still-governing amended record AHEAD of its amendments, so when the record
budget cannot fit them all it drops amendments first and never keeps an
amendment's body without the body of the record it amends (pinned on these
records by `TestShippedIndex_AssembleKeepsAmendedBodyAndFramesAmendments`).

Kept as supersession, because each source states a supersession (partial
ones included) of that record, which takes precedence under the rule above:

- ADR-031 → ADR-018: "Amends/supersedes the review-gate-resolution
  *mechanism* of ADR-018".
- ADR-061 → ADR-058: "Supersedes ADR-058's tenant definition."
- ADR-076 → ADR-033: "This **supersedes ADR-033's gate**".
- ADR-090 → ADR-043: "**This ADR partially supersedes ADR-043**".
- ADR-092 → ADR-090: "**This ADR partially supersedes ADR-090:**".
- ADR-093 → ADR-090, ADR-092: "**This ADR partially supersedes ADR-092** ...
  **and ADR-090**".
- ADR-094 → ADR-087: "This partially supersedes ADR-087".
- ADR-096 → ADR-090: "This partially supersedes ADR-090 D2 (independent
  deltas only) and D5 (fan-in base)".
- ADR-098 → ADR-096: "**Supersession.** This amends ADR-096: D3 is replaced
  by D1–D2 here". The borderline case: the source labels the relation
  "Supersession" and states a replacement.
- ADR-099 → ADR-080: "This partially supersedes ADR-080's vocabulary table".
- ADR-057 → ADR-061: "Amendment A1 supersedes ADR-061 (#2069)" (added to
  #1823 with the Captain's 2026-10-23 rulings).

The audit scanned every record body for `amend*` and `supersed*`. Every
record whose `amend*` wording sits in a sentence naming another ADR id, and
its disposition:

- ADR-031, ADR-076, ADR-092, ADR-094, ADR-096, ADR-098, ADR-099: amendment
  wording beside a stated supersession of the same ADR → `supersedes` kept.
  ADR-076's "provider discriminator per the ADR-057 amendment" names an
  amendment WITHIN ADR-057's own history, not one ADR-076 makes → no edge.
- ADR-032: "Amends the decomposition fan-out design (the per-child PR
  behaviour that emerged from ADR-031 Phase 1)" → NO edge. It amends emergent
  behaviour, not a decision ADR-031 states: ADR-031's body decides the
  `github_merge` review-gate resolution and never mentions decomposition,
  fan-out or per-child PRs.
- ADR-095, ADR-097, ADR-100, ADR-101: stated amendment, no supersession of
  that record → `amends` (above).
- ADR-099's "Neither amended ADR-080's glossary" and ADR-100's "Precedent:
  ADR-098 (... a new ADR amending an accepted one)" are context and citation
  → no edge beyond ADR-099's kept supersession.

The other records using `amend*` name no other ADR as the amended thing (a
self-amendment section, an issue, a charter, `MVP_SPEC`, a scope amendment):
ADR-039, ADR-040, ADR-042, ADR-053, ADR-061, ADR-062, ADR-063, ADR-066,
ADR-079, ADR-080, ADR-088, ADR-089, ADR-090, ADR-091 → no edge.

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
| [ADR-010](010-marketplace-billing-path-github-vs-direct.md) | Marketplace billing path (GitHub vs. direct) | `proposed` | [#74](https://github.com/kuhlman-labs/fishhawk/issues/74) |
| [ADR-011](011-pricing-model-per-engineer-vs-per-run.md) | Pricing model (per-engineer vs. per-run) | `proposed` | [#75](https://github.com/kuhlman-labs/fishhawk/issues/75) |
| [ADR-012](012-design-partner-sourcing-strategy.md) | Design partner sourcing strategy | `proposed` | [#76](https://github.com/kuhlman-labs/fishhawk/issues/76) |
| [ADR-013](013-apache-bsl-enterprise-boundary.md) | Apache 2.0 / BSL boundary for enterprise modules | `accepted` | [#77](https://github.com/kuhlman-labs/fishhawk/issues/77) |
| [ADR-014](014-linter-formatter-ci-config-for-go-and-ts.md) | Linter/formatter/CI config for Go and TS | `accepted` | [#78](https://github.com/kuhlman-labs/fishhawk/issues/78) |
| [ADR-015](015-slack-notification-approach.md) | Slack notification approach (v0.x scope) | `accepted` | [#79](https://github.com/kuhlman-labs/fishhawk/issues/79) |
| [ADR-016](016-iac-tool-terraform-vs-aws-cdk.md) | IaC tool — Terraform vs AWS CDK | `accepted` | [#165](https://github.com/kuhlman-labs/fishhawk/issues/165) |
| [ADR-017](017-ci-gating-defer-to-github-branch-protection.md) | CI gating: defer to GitHub branch protection, decouple approval from merge | `accepted` | [#249](https://github.com/kuhlman-labs/fishhawk/issues/249) |
| [ADR-018](018-review-stage-approval-defers-to-github-pr-merge.md) | Review-stage approval defers to GitHub PR merge | `accepted` | [#311](https://github.com/kuhlman-labs/fishhawk/issues/311) |
| [ADR-019](019-fishhawk-as-coordination-layer-not-destination.md) | Fishhawk as coordination layer, not destination | `accepted` | [#320](https://github.com/kuhlman-labs/fishhawk/issues/320) |
| [ADR-020](020-plan-review-surface-on-originating-issue.md) | Plan review surface: post the plan on the originating issue | `accepted` | [#321](https://github.com/kuhlman-labs/fishhawk/issues/321) |
| [ADR-021](021-claude-code-integration-mcp-server.md) | Claude Code integration: ship a Fishhawk MCP server | `accepted` | [#322](https://github.com/kuhlman-labs/fishhawk/issues/322) |
| [ADR-022](022-pluggable-runner-backends-runner-kind-audit.md) | Pluggable runner backends + runner_kind audit dimension | `accepted` | [#388](https://github.com/kuhlman-labs/fishhawk/issues/388) |
| [ADR-023](023-agent-self-retry-on-transient-stage-failure.md) | Agent self-retry on transient stage failure | `accepted` | [#401](https://github.com/kuhlman-labs/fishhawk/issues/401) |
| [ADR-024](024-mcp-server-spawning-runner-subprocesses.md) | MCP server spawning runner subprocesses | `accepted` | [#433](https://github.com/kuhlman-labs/fishhawk/issues/433) |
| [ADR-025](025-scope-policy-timeouts-runtime-decomposition.md) | Scope-policy: workflow-spec timeouts, predicted runtime, and plan-driven decomposition | `accepted` | [#451](https://github.com/kuhlman-labs/fishhawk/issues/451) |
| [ADR-026](026-backward-compatibility-for-schema-extension-prs.md) | Backward-compatibility discipline for schema-extension PRs | `accepted` | [#472](https://github.com/kuhlman-labs/fishhawk/issues/472) |
| [ADR-027](027-review-agent-stage-primitive.md) | Review-agent stage primitive: per-stage agent + human reviewer counts | `accepted` | [#545](https://github.com/kuhlman-labs/fishhawk/issues/545) |
| [ADR-028](028-agent-filesystem-confinement-os-level-sandbox.md) | Agent filesystem confinement requires an OS-level sandbox | `accepted` | [#642](https://github.com/kuhlman-labs/fishhawk/issues/642) |
| [ADR-029](029-rule-of-two-audit-network-egress-allow-listing.md) | Rule-of-Two / lethal-trifecta audit + network egress allow-listing | `accepted` | [#650](https://github.com/kuhlman-labs/fishhawk/issues/650) |
| [ADR-030](030-periodic-cost-budgets-in-workflow-spec.md) | Periodic cost budgets in the workflow spec (advisory + admission-time blocking) | `accepted` | [#687](https://github.com/kuhlman-labs/fishhawk/issues/687) |
| [ADR-031](031-review-gate-succeeds-only-on-verified-landing.md) | Review gate succeeds only on verified landing (platform-neutral, delivery-agnostic) | `accepted` | [#710](https://github.com/kuhlman-labs/fishhawk/issues/710) |
| [ADR-032](032-decomposition-one-consolidated-pr.md) | Decomposition delivery model: one consolidated PR per decomposition | `accepted` | [#719](https://github.com/kuhlman-labs/fishhawk/issues/719) |
| [ADR-033](033-mcp-transport-stdio-vs-streamable-http.md) | MCP transport: stdio-only vs. add streamable-HTTP (relationship to the #655 gateway) | `accepted` | [#843](https://github.com/kuhlman-labs/fishhawk/issues/843) |
| [ADR-034](034-kubernetes-deployment-model.md) | Kubernetes deployment model — relationship to ECS, deps, secrets, worker model | `accepted` | [#845](https://github.com/kuhlman-labs/fishhawk/issues/845) |
| [ADR-035](035-run-git-contract-declared-base-sole-writer.md) | Own and enforce the git contract of a run (declared base + sole-writer branch lineage) | `accepted` | [#857](https://github.com/kuhlman-labs/fishhawk/issues/857) |
| [ADR-036](036-gate-plan-approval-on-agent-review-completion.md) | Gate plan approval on agent-review completion (block-on-completion, unblock-on-terminal) | `accepted` | [#874](https://github.com/kuhlman-labs/fishhawk/issues/874) |
| [ADR-037](037-agent-facing-waits-follow-mcp-tasks-contract.md) | Agent-facing waits follow the MCP Tasks / long-running-operation contract (durable handle + authoritative poll) | `accepted` | [#879](https://github.com/kuhlman-labs/fishhawk/issues/879) |
| [ADR-038](038-deploy-stage-type-post-merge-release-governance.md) | Add a deploy stage type for post-merge release governance | `accepted` | [#925](https://github.com/kuhlman-labs/fishhawk/issues/925) |
| [ADR-039](039-agentic-deploy-execution.md) | Agentic deploy execution (skills perform the rollout) | `proposed` | [#937](https://github.com/kuhlman-labs/fishhawk/issues/937) |
| [ADR-040](040-operator-agent-role-contract.md) | Operator agent as a product-defined role contract: versioned playbook, gate-delegation knobs, thin-by-design | `accepted` | [#997](https://github.com/kuhlman-labs/fishhawk/issues/997) |
| [ADR-041](041-per-child-branch-isolation-fan-in-integration.md) | Per-child branch isolation + fan-in integration for parallel decomposition | `accepted` | [#1140](https://github.com/kuhlman-labs/fishhawk/issues/1140) |
| [ADR-042](042-fixup-session-continuity.md) | Fix-up session continuity: resume the prior implement agent session vs. prompt-level continuity | `accepted` | [#1264](https://github.com/kuhlman-labs/fishhawk/issues/1264) |
| [ADR-043](043-stale-base-integration-policy.md) | Stale-base integration policy for concurrent agent branches (runner merges base before PR; clean verdict-transfer vs conflict re-invoke+re-review; merge not rebase) | `accepted` | [#1293](https://github.com/kuhlman-labs/fishhawk/issues/1293) |
| [ADR-044](044-model-cost-observability.md) | model cost observability — warn-not-fail price drift-check, 3-bucket fresh/read/write cache cost model with separate capture, split cost-accuracy from efficiency-metric, daily cadence | `accepted` | [#1336](https://github.com/kuhlman-labs/fishhawk/issues/1336) |
| [ADR-045](045-runner-backend-identification.md) | runner-backend identification — derive runner_kind from the execution channel (host-dispatch ⇒ local, runner self-report, mismatch guardrail), not an operator declaration | `accepted` | [#1347](https://github.com/kuhlman-labs/fishhawk/issues/1347) |
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
| [ADR-056](056-external-anchoring-of-the-audit-head.md) | External anchoring of the audit head: tamper-evidence beyond the exported chain | `accepted` | [#1699](https://github.com/kuhlman-labs/fishhawk/issues/1699) |
| [ADR-057](057-multi-tenancy-model-tenant-isolation.md) | Multi-tenancy model and tenant isolation for hosted deployment | `accepted` | [#1823](https://github.com/kuhlman-labs/fishhawk/issues/1823) |
| [ADR-058](058-second-forge-support-gitlab.md) | Second-forge support: GitLab repositories, work items, and GitLab CI | `accepted` | [#1851](https://github.com/kuhlman-labs/fishhawk/issues/1851) |
| [ADR-059](059-evidence-gated-correctness-lens-implement-review.md) | Evidence-gated correctness lens in implement-review | `accepted` | [#1883](https://github.com/kuhlman-labs/fishhawk/issues/1883) |
| [ADR-060](060-mcp-session-survival-shim.md) | MCP session-survival shim: stdio supervisor as gateway phase 0 | `accepted` | [#1920](https://github.com/kuhlman-labs/fishhawk/issues/1920) |
| [ADR-061](061-multi-tenancy-foundation-workspace-rls.md) | Multi-tenancy foundation: workspace-scoped tenancy with Postgres RLS isolation | `superseded` | [#2069](https://github.com/kuhlman-labs/fishhawk/issues/2069) |
| [ADR-062](062-regional-cells-control-plane.md) | Regional-cells control plane: fishhawk-directory service, directory-first region pinning, per-cell inference config | `accepted` | [#2099](https://github.com/kuhlman-labs/fishhawk/issues/2099) |
| [ADR-063](063-isolate-untrusted-gate-command-execution.md) | Isolate untrusted gate-command execution: container sandbox for .git-metadata, egress, and host-filesystem containment | `accepted` | [#2127](https://github.com/kuhlman-labs/fishhawk/issues/2127) |
| [ADR-064](064-forge-project-board-work-queue-input.md) | Forge project board as declarative work-queue input and derived status projection | `accepted` | [#2139](https://github.com/kuhlman-labs/fishhawk/issues/2139) |
| [ADR-065](065-backlog-grooming-prioritization-agent.md) | Backlog grooming / prioritization agent with workflow-declared tunable autonomy | `accepted` | [#2161](https://github.com/kuhlman-labs/fishhawk/issues/2161) |
| [ADR-066](066-workflow-declared-autonomy-tier.md) | Workflow-declared autonomy tier and path-scoped control surface | `accepted` | [#2209](https://github.com/kuhlman-labs/fishhawk/issues/2209) |
| [ADR-067](067-workflow-spec-grammar-consolidation.md) | Workflow spec grammar consolidation: break the duplicate surfaces and add reuse before the first external consumer | `accepted` | [#2210](https://github.com/kuhlman-labs/fishhawk/issues/2210) |
| [ADR-068](068-repo-declared-review-conventions.md) | Repo-declared review conventions: workflow-declared, server-injected supplemental criteria for plan and implement review | `accepted` | [#2211](https://github.com/kuhlman-labs/fishhawk/issues/2211) |
| [ADR-069](069-separate-execution-eligibility-from-autonomy.md) | Separate execution eligibility from autonomy: an `execution:` namespace, type-derived workflow eligibility, and the emergency bypass as an audited action | `accepted` | [#2268](https://github.com/kuhlman-labs/fishhawk/issues/2268) |
| [ADR-070](070-cost-forecasting-as-planning-input.md) | Cost forecasting as a planning input: interval forecasts at the plan gate, an in-flight circuit-breaker, and budget-aware grooming | `accepted` | [#2276](https://github.com/kuhlman-labs/fishhawk/issues/2276) |
| [ADR-071](071-beta-hardening-for-external-operators.md) | Beta hardening for external operators: intake sanitization, gate notification and SLA, and per-tenant limits | `accepted` | [#2288](https://github.com/kuhlman-labs/fishhawk/issues/2288) |
| [ADR-072](072-customers-bring-own-model-credentials-byok.md) | Customers bring their own model credentials (BYOK): inference cost sits with the customer, Fishhawk charges for the governance layer | `accepted` | [#2296](https://github.com/kuhlman-labs/fishhawk/issues/2296) |
| [ADR-073](073-kubernetes-helm-single-deployment-substrate.md) | Kubernetes and the Helm chart are the single deployment substrate; retire the unexercised ECS path | `accepted` | [#2298](https://github.com/kuhlman-labs/fishhawk/issues/2298) |
| [ADR-074](074-reviewer-agents-run-on-customer-infrastructure.md) | Reviewer agents run on customer infrastructure: fishhawkd orchestrates and records but never executes an agent | `accepted` | [#2306](https://github.com/kuhlman-labs/fishhawk/issues/2306) |
| [ADR-075](075-kubernetes-runner-backend.md) | Kubernetes runner backend: run agent stages in the customer's own cluster instead of their CI platform | `accepted` | [#2310](https://github.com/kuhlman-labs/fishhawk/issues/2310) |
| [ADR-076](076-mcp-over-http-fishhawkd-oauth-authorization-server.md) | MCP over HTTP served by fishhawkd, with fishhawkd as the OAuth authorization server — supersedes ADR-033's loopback gate | `accepted` | [#2387](https://github.com/kuhlman-labs/fishhawk/issues/2387) |
| [ADR-077](077-bounding-get-run-status-response.md) | Bounding the get_run_status response: what limit, which boundary, and what an elision pointer honestly promises | `accepted` | [#2507](https://github.com/kuhlman-labs/fishhawk/issues/2507) |
| [ADR-078](078-ground-review-agents-widen-context-narrow-capability.md) | Ground the review agents by widening context and narrowing capability: export the reviewed tree, scrub the inherited environment, grant read and search only | `accepted` | [#2519](https://github.com/kuhlman-labs/fishhawk/issues/2519) |
| [ADR-079](079-product-and-program-management-agent.md) | Product and program management agent for human-ratified charters, release strategy, and readiness | `accepted` | [#3238](https://github.com/kuhlman-labs/fishhawk/issues/3238) |
| [ADR-080](080-solo-captain-alpha.md) | Solo-captain alpha: the repository is the unit, one developer commands the full crew locally from the bridge | `accepted` | [#3693](https://github.com/kuhlman-labs/fishhawk/issues/3693) |
| [ADR-084](084-reviewer-personas.md) | Reviewer personas: a separate reviewer invocation with a declared remit, attached per stage or by an escalation, advisory with ingest-time checks, plus deterministic security checks as concern sources | `accepted` | [#3752](https://github.com/kuhlman-labs/fishhawk/issues/3752) |
| [ADR-085](085-earned-autonomy-recommendations-only.md) | Earned autonomy: recommendations only, from server-attested evidence and a blind shadow record, with asymmetric thresholds (lower fast, raise slowly) and captain ratification through a human-authored spec change | `accepted` | [#3777](https://github.com/kuhlman-labs/fishhawk/issues/3777) |
| [ADR-087](087-local-stage-concurrency-groups.md) | Local stage concurrency groups: server-coordinated admission of host-dispatched stages | `accepted` | [#3968](https://github.com/kuhlman-labs/fishhawk/issues/3968) |
| [ADR-088](088-declarative-gate-services.md) | Declarative gate services: a closed, Fishhawk-owned `services:` schema under workflow-v2 `gate_container`, joined to the gate by a shared --network=none namespace (not raw compose, not a bridge network) | `accepted` | [#4042](https://github.com/kuhlman-labs/fishhawk/issues/4042) |
| [ADR-089](089-first-officer-read-only-crew-seat.md) | First officer: a structurally read-only crew seat whose evidence-backed findings gate actions | `accepted` | [#4149](https://github.com/kuhlman-labs/fishhawk/issues/4149) |
| [ADR-090](090-verify-the-merge-candidate.md) | Verify the merge candidate: a Fishhawk-dispatched merge requires an up-to-date head, and a base-advanced head must pass the project's declared verify command in a runner verify-only pass | `accepted` | [#4170](https://github.com/kuhlman-labs/fishhawk/issues/4170) |
| [ADR-091](091-restart-recovery-redispatches-advisory-review-rounds.md) | Restart recovery re-dispatches orphaned advisory review rounds from a durable round-source descriptor instead of terminating them as failed | `accepted` | [#4171](https://github.com/kuhlman-labs/fishhawk/issues/4171) |
| [ADR-092](092-serialize-merge-candidate-verify-passes.md) | Serialize merge-candidate verify passes per repository, re-anchor a queued pass at admission, and admit passes through a local-verify group (amends ADR-090) | `accepted` | [#4202](https://github.com/kuhlman-labs/fishhawk/issues/4202) |
| [ADR-093](093-fishhawk-owned-merge-queue.md) | A Fishhawk-owned merge queue: extend ADR-092's per-base serializer so the queue advances, verifies and MERGES the head of line, provider-agnostic, batch size 1 | `accepted` | [#4224](https://github.com/kuhlman-labs/fishhawk/issues/4224) |
| [ADR-094](094-resource-keyed-admission.md) | Resource-keyed admission: one admission path for every spawning verb, stages hold a scoped SET of resource groups including a host capacity group, and review rounds admit through provider-account groups that park on quota (amends ADR-087) | `accepted` | [#4239](https://github.com/kuhlman-labs/fishhawk/issues/4239) |
| [ADR-095](095-review-coverage-hold-and-operator-rerun.md) | Review-coverage hold for decomposed parents, plus an operator re-run verb with its own same-head dedup bypass marker, distinct from ADR-091 D6's boot-only marker (amends ADR-091) | `accepted` | [#4287](https://github.com/kuhlman-labs/fishhawk/issues/4287) |
| [ADR-096](096-merge-candidate-verdict-transfer.md) | Merge-candidate verdict transfer across an independent base delta, plus a single verification point for decomposed parents (amends ADR-090) | `accepted` | [#4290](https://github.com/kuhlman-labs/fishhawk/issues/4290) |
| [ADR-097](097-operator-as-orchestrator.md) | Operator as orchestrator: the driver delegates evidence work to stateless specialist jobs; the first officer keeps a standing trigger-driven watch the operator cannot silence (amends ADR-089, ADR-040) | `accepted` | [#4299](https://github.com/kuhlman-labs/fishhawk/issues/4299) |
| [ADR-098](098-split-global-surface-shadow-transfer.md) | Amend ADR-096 D3: split the global surface into verdict-wide and doc-check inputs, add data-read edges, and enable verdict transfer only after a shadow measurement | `accepted` | [#4316](https://github.com/kuhlman-labs/fishhawk/issues/4316) |
| [ADR-099](099-crew-role-naming.md) | Crew role naming: operator drives, first officer oversees (amends ADR-080 glossary), nautical titles only for command and oversight, functional names for working roles | `accepted` | [#4331](https://github.com/kuhlman-labs/fishhawk/issues/4331) |
| [ADR-100](100-adaptive-bounded-concurrency.md) | Adaptive, bounded concurrency for host-dispatched stages: a product-owned headroom controller beside the integer limit, operator override, staged shadow→canary→enforce (amends ADR-087 limit rule and ADR-094 D4/Q1) | `accepted` | [#4403](https://github.com/kuhlman-labs/fishhawk/issues/4403) |
| [ADR-101](101-charter-authorization-mode.md) | Charter authorization mode: lease-based, bounded autonomous operation by a product-spawned operator under a deterministic engine, intersected with delegation; captain seat stays human (amends ADR-040/085/089/097) | `accepted` | [#4412](https://github.com/kuhlman-labs/fishhawk/issues/4412) |
| [ADR-103](103-adversarial-security-probe.md) | Adversarial security probe: a scheduled acceptance-stage variant drives a disposable target under ADR-050 containment, emits a runner-replayed security_probe artifact, and files findings through a captain gate on a confidential channel (amends ADR-050) | `accepted` | [#4518](https://github.com/kuhlman-labs/fishhawk/issues/4518) |
| [ADR-104](104-host-dispatcher.md) | Host dispatcher: a host-side, dispatch-only process spawns the parked local stages of auto-started campaign runs | `accepted` | [#4524](https://github.com/kuhlman-labs/fishhawk/issues/4524) |
| [ADR-105](105-repository-identity-forge-id.md) | Repository identity keyed on the forge's immutable repository id; names resolve through a time-bounded history; an owner change re-establishes trust (amends ADR-057, ADR-061) | `accepted` | [#4550](https://github.com/kuhlman-labs/fishhawk/issues/4550) |
| [ADR-106](106-run-bound-subject-filtered-audit-read.md) | Run-bound agent tokens get a subject-filtered audit read: how cross-run audit categories are withheld | `accepted` | [#4575](https://github.com/kuhlman-labs/fishhawk/issues/4575) |
| [ADR-107](107-split-stage-timeout-agent-and-gate-budgets.md) | Amend ADR-025: split the stage timeout into an agent budget and a project-derived verify-gate budget | `accepted` | [#4581](https://github.com/kuhlman-labs/fishhawk/issues/4581) |
| [ADR-108](108-shared-postgres-lock-holder-budget.md) | Shared Postgres lock-holder budget: one pool-wide pglock budget with reserved headroom and enforced no-nesting for every connection-holding advisory-lock helper | `accepted` | [#4594](https://github.com/kuhlman-labs/fishhawk/issues/4594) |
| [ADR-109](109-merge-queue-owns-the-merge.md) | Amend ADR-093 D1: the merge queue owns the merge — one database-enforced per-run dispatch ledger across inline, delegated and queue merges, plus the admission-epoch CAS protocol | `accepted` | [#4601](https://github.com/kuhlman-labs/fishhawk/issues/4601) |
| [ADR-110](110-repository-agnostic-product-features.md) | Product features are repository-agnostic: repository specifics are declared configuration, and this repository is a configured instance, not a special case | `accepted` | [#4605](https://github.com/kuhlman-labs/fishhawk/issues/4605) |
| [ADR-111](111-documentation-freshness-for-the-published-site.md) | Documentation freshness for the published site: a PR-time site job, a reference-integrity check, a stated site obligation, and a narrow definition of up to date | `accepted` | [#4611](https://github.com/kuhlman-labs/fishhawk/issues/4611) |
| [ADR-112](112-queue-merge-forge-approval-predicate.md) | Amend ADR-093 D6: a transferred approval needs a fresh, trusted forge approval at the verified head; an undeterminable or missing one HOLDS rather than ejects on a non-blocked PR | `accepted` | [#4622](https://github.com/kuhlman-labs/fishhawk/issues/4622) |
| [ADR-113](113-forge-verified-adr-ratification.md) | ADR ratification as a forge-verified, hash-bound approval in the plan-gate approvals shape, a deterministic record writer, and catch-rate decoupling (Option B′) | `accepted` | [#4628](https://github.com/kuhlman-labs/fishhawk/issues/4628) |
| [ADR-114](114-repo-scoped-refinement-sessions.md) | Refinement sessions are optionally repo-scoped at open, and drafting/preview resolve that repo's conventions per repo (not the shipped default) | `accepted` | [#4642](https://github.com/kuhlman-labs/fishhawk/issues/4642) |
| [ADR-115](115-parallel-stages-up-to-merge.md) | Every stage runs in parallel up to the merge: the merge queue is the only serial point, capacity is the only bound, and a moving base is reconciled at merge time | `accepted` | [#4646](https://github.com/kuhlman-labs/fishhawk/issues/4646) |
| [ADR-116](116-fan-in-conflict-recovered-in-place.md) | A slice conflict at fan-in parks and is recovered in place (conflict probe, mechanical union, then a hunk-confined resolution pass, then a scoped parent fix-up, with a verified operator resolution always available), never by restarting the whole run | `accepted` | [#4680](https://github.com/kuhlman-labs/fishhawk/issues/4680) |
| [ADR-117](117-live-review-effectiveness.md) | Live review effectiveness is measured: concern outcomes and human judgements, child-slice concerns imported into the parent, an escape record attributed to the rounds that saw the code, and a presence-conditioned per-reviewer scorecard | `accepted` | [#4690](https://github.com/kuhlman-labs/fishhawk/issues/4690) |
| [ADR-118](118-review-changes-judged-not-repinned.md) | Review changes are judged, not re-pinned: a per-arm eval gate (standing, experiment and monitor arms; model changes as paired experiments), reviewer configuration as a gate trigger, and one pre-registered offline+live decision rule | `accepted` | [#4698](https://github.com/kuhlman-labs/fishhawk/issues/4698) |
| [ADR-119](119-fixup-session-resume-opt-in.md) | Fix-up session resume is opt-in and local-only, bound to the reported head and a signed anchor | `accepted` | [#4708](https://github.com/kuhlman-labs/fishhawk/issues/4708) |
| [ADR-120](120-decision-record-scaling.md) | The decision record scales: lean index lines, a topic map beside the record, a partitioned index | `accepted` | [#4709](https://github.com/kuhlman-labs/fishhawk/issues/4709) |

## Status unknown — needs captain confirmation

None. The Captain resolved all eighteen records listed here on 2026-10-23 ("as
recommended", from the first officer's ruling sheet). Each ruling is stated in
the tracker body, and the record is re-transcribed from it:

- `accepted` (13): ADR-022, ADR-026, ADR-028, ADR-031, ADR-032, ADR-034,
  ADR-035, ADR-036, ADR-037, ADR-042, ADR-044, ADR-045 and ADR-057. Each body
  gained a `## Decision` section quoting the source that states the decision.
  ADR-034 and ADR-042 had their Decision placeholder replaced instead.
  `date` is the decision date the source states. ADR-022, ADR-044 and ADR-057
  state none ("date not stated").
- `proposed` (4): ADR-010, ADR-011, ADR-012 and ADR-039. They are open and
  deferred. The placeholder stays, followed by the Captain's confirmation.
- `superseded` (1): ADR-061, by ADR-057 (see "Amendment relation").

The same rulings appended a partial-supersession clarification to the bodies
of ADR-018, ADR-033, ADR-043, ADR-058, ADR-080, ADR-087, ADR-090, ADR-092 and
ADR-096. They stay `accepted` and keep their links. The automatically appended
"Intake signals" sections of ADR-080 and ADR-087 were removed from their
tracker bodies before re-transcription. ADR-090's record keeps the in-repo
"Amendment 1 (2026-10-14)" section added by #4424, which its tracker body
does not carry, and takes the clarification after it.

A record the inference rule leaves `unknown` from here on is listed here
again, quoting its source verbatim (Upkeep rule).
