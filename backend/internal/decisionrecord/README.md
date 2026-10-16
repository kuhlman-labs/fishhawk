# decisionrecord — decision-record selection for reviewer personas

`backend/internal/decisionrecord` selects the repository decision records that
bear on a change and assembles them for injection into a reviewer persona's OWN
prompt (ADR-084 D4(b) / binding rule 4, E78.5 / #3756; ADR-084 has no
`docs/adr` record yet, so it is read from the tracker). It reads and renders
through `backend/internal/repodoc`, so base-ref pinning, content hashing,
delimiter neutralization and attribution are repodoc's; this package adds the
strict index parse, the selection, the ranking, the byte budget and the
status-labelled framing.

The caller (a persona declaring `decision_record: {index: <path>}`) wires it in
`backend/internal/server/reviewer_persona.go`; this package has no server
dependency.

## Entry points

| Function | Does |
|---|---|
| `ParseIndex(raw)` | Strict `adr-index-v1` decode (below). Every refusal wraps `ErrInvalidIndex` and names the record. |
| `Select(index, changePaths)` | The records whose `applies_to` matches a change path, plus the records that supersede or amend them (transitively), ranked. |
| `Assemble(ctx, resolver, Request)` | Fetch the index at the admission commit, parse, select, shape the index under its own cap, and fit whole records into a separate record budget. Returns a `*Selection`. |
| `Selection.PromptDocuments(grounded)` | The index, then each included record, as `prompt.InjectedDocument`s. |
| `Selection.Attribution()` | The `repodoc.InjectionSet` for the selection: its documents plus, when anything was dropped, one selection-level truncation. A caller injecting other documents in the same prompt (the persona remit) merges this into ONE `repodoc.AttributeSet` call. |

## Strict parse (`ParseIndex`)

The index decides which governance text a reviewer is shown, so nothing in it is
ignored. Refused, each with its own test case in `TestParseIndex_FailsClosed`:
malformed JSON; trailing data after the object; an unknown field at any level
(`DisallowUnknownFields`); `schema_version` other than `adr-index-v1`; a record
`id` not matching `^ADR-[0-9]{3,}$`; a `path` failing `repodoc.ValidatePath`
(absolute, `..`, control character, …) — validated BEFORE it is ever fetched; a
`status` outside `proposed | accepted | rejected | superseded | unknown`; a
`supersedes` / `superseded_by` / `amends` / `amended_by` entry not matching the
id pattern (or not a list); an `applies_to` glob `spec.Predicate.Validate`
refuses; a duplicate id; a duplicate path; a link id (any of the four keys)
that names no record in the index, or that names the record itself
(`checkLinks`, after the per-record pass — the `docs/adr/README.md` "Every id
must name an existing record other than itself" contract `scripts/check-adr`
enforces); and an id one record lists in BOTH `supersedes` and `amends`, or in
both `superseded_by` and `amended_by` (`checkLinks`: a record cannot both
supersede and amend the same record). That exclusivity is the operator's
precedence rule, ratified in #4471's plan approval (condition 5), not a rule
the issue implies: a record that states both an amendment and a (partial)
supersession of the same ADR is encoded as `supersedes` only.

**Optional amendment keys (E78.13 / #4471).** `amends` and `amended_by` are
OPTIONAL per record: an absent key parses exactly like an empty list
(`Record.Amends` / `Record.AmendedBy` nil), so an index without them — every
index before #4471 — still parses, and an index carrying them only on the
linked records parses too (`TestParseIndex_Valid`,
`TestParseIndex_Valid_AmendmentKeys`). Every other key stays required and
every unknown key is still refused. The parser does not check reciprocity;
`scripts/check-adr` does, on the records. `TestParseIndex_AcceptsTheShippedIndex` parses `docs/adr/index.json` so the
parser cannot drift stricter than the index `scripts/check-adr` generates.

## Selection and ranking (`Select`)

A record with an EMPTY `applies_to` never matches: it governs no named path, and
`spec.Predicate.Match` refuses an empty predicate (it is not match-all). Every
other record is matched with `spec.Predicate{Paths: applies_to}` — the shared
doublestar matcher — against each DISTINCT change path; a `Match` error fails
the whole selection (`ErrInvalidIndex`), never reads as a non-match.

**Successor following (E86.58 / #4398).** Every directly matched record pulls
in its successors, transitively, so a reviewer shown a superseded record is also
shown what replaced it. A successor is any id in the record's `superseded_by`,
or any record whose `supersedes` names it — the union, so a non-reciprocal
hand-built index is still followed. Each walk is seeded from ONE direct match
with its own visited set, so a cycle terminates; a record reached twice (a
diamond) is selected once. Records not reachable from a direct match are never
selected, and predecessors are never followed. A `superseded_by` id naming no
record fails the whole selection (`ErrInvalidIndex`) — a hand-built index can
reach `Select` without `ParseIndex`. Each `Match` carries:

- `MatchedPaths` — distinct change paths the record's OWN `applies_to` matched
  (0 for a record selected only as a successor);
- `RankPaths` — the ranking key: the larger of `MatchedPaths` and every selected
  predecessor's `RankPaths`, so a successor ranks next to the record it
  replaces (and, being newer, ahead of it);
- `Via` — the ids of the selected records it supersedes, sorted; empty for a
  record no selected record links to.

**Amendment following (E78.13 / #4471).** The same walk follows amendments: an
amendment is any id in a record's `amended_by`, or any record whose `amends`
names it (the union again). One walk per direct match traverses both edge
kinds, so a mixed chain (an amendment of a successor) is followed; amended
records, like predecessors, are never followed. A reached amendment inherits
the root's `RankPaths` like a successor and records its provenance in
`AmendsVia` — the ids of the selected records it amends, sorted — never in
`Via`, so it is never framed as a successor. A dangling `amended_by` id
reached by the walk fails the whole selection (`ErrInvalidIndex`).

Ranking is deterministic: **accepted first**; then **higher `RankPaths`**; then
the **higher record number** (newer) first, compared as a digit string so a
long number cannot overflow; then **id ascending**. Rank is 1-based.
`RankPaths` equals `MatchedPaths` for a record with no selected predecessor or
amended record.

**An amended record ranks ahead of its amendments (approval condition 2 of
#4471).** On top of that order, `placeAmendmentsAfterAmended` takes, at each
position, the highest-ranked remaining record whose `AmendsVia` records are
all already placed, so no amendment ranks ahead of a selected record it amends
— whatever its status, `RankPaths` or number. The amended record still
governs, and the record budget is a strict prefix, so its body is never
dropped while an amendment of it is shown: when the budget cannot fit them all,
amendments are dropped first. An amendment cycle (only a hand-built index can
carry one) leaves no eligible record; the highest-ranked remaining record is
then taken, so the pass terminates.

The change paths are the caller's: plan scope at plan review; approved plan
scope union diff at implement review.

## Byte budget (`Assemble`)

**Two budgets (#4399).** The index and the records are budgeted SEPARATELY,
each at the resolver's effective cap, `repodoc.Resolver.CapBytes()`
(`DefaultMaxBytes` = 32768 unless overridden), so the index's size can never
starve the record bodies. Before #4399 one budget counted both, and the
shipped index alone (~31 KiB) left no room for any record: the architect
persona reviewed from index titles only.

- **Index** — shaped under its own per-document cap, the resolver's cap
  (shrink-to-fit, below). Its own `document_injected` (and, when cut, its
  per-document `document_truncated`) split its numbers: `document_injected`
  carries `rendered_bytes` and `truncated` but no `cap_bytes`; only the
  per-document `document_truncated` carries `cap_bytes`. That `cap_bytes` is
  the cap it was actually cut under, so it is below the resolver's cap
  whenever the index was reshaped.
- **Records** — fitted, whole and in rank order, against a separate **record
  budget** = the resolver's cap. `Selection.CapBytes` is that record budget and
  `Selection.IncludedBytes` the sum of the INCLUDED RECORDS' rendered bytes
  (the index excluded), always `<= CapBytes`; the selection-level
  `document_truncated`'s `cap_bytes` / `included_bytes` carry exactly those two
  values.

**Two `cap_bytes` on one injection set (#3930).**

1. When the index is cut, its per-document `document_truncated` `cap_bytes` is
   the shrunk **cut cap**: below the resolver cap, and equal to the cap named
   by the body's `at the <c>-byte cap` marker and the `TRUNCATED at <c> bytes`
   Source line.
2. When a record is dropped, the selection-level `document_truncated`
   `cap_bytes` is the **record budget** (`Selection.CapBytes`).
3. For an over-cap index that also drops a record, the two therefore differ by
   design and must not be compared as one budget.
4. Stamping the resolver cap onto the index Document was rejected: it would
   desync the marker and the Source line (both derive from the cut) from the
   audit row, and break `repodoc.Document.CapBytes`'s contract (the effective
   cap the document was resolved under).

Pinned by `TestAssemble_IndexOverCap` (exact cut cap, marker, Source line) and
`TestAssemble_IndexOverCap_AttributionCapBytes` (both persisted rows).

Bytes are `repodoc.Document.RenderedBytes`, the shown body between the
delimiters, recorded as `rendered_bytes` on each `document_injected`. **The
persona remit and all fixed system framing (headings, preambles, trust notes,
the data clause, Source lines, the dropped-records notice) are OUTSIDE both
budgets**: the remit is its own repodoc document under its own cap. The
decision-record injection is therefore at most twice the cap (index <= cap,
records <= cap), in the persona prompt only.

1. **The index is always included.** Shaped under its own cap; when its
   rendered size (including repodoc's truncation marker, which is appended
   AFTER the cut) exceeds the cap, it is reshaped under a cap reduced by the
   overflow.
   The loop is bounded (`maxIndexShrinkSteps`), the cap is clamped at zero and
   never negative, and an index that does not fit even at a zero-byte cut (a
   budget smaller than the marker) fails the assembly (`ErrUnresolvable`).
2. **Each match, in rank order,** is fetched at the SAME commit and shaped
   under a cap **at least its raw size** — so what is measured is its
   UNTRUNCATED rendered size, never a size under the shrunk budget — and is
   included only if that fits what remains of the record budget. The index
   draws nothing from it (`TestAssemble_IndexNearCapDoesNotStarveRecords`).
3. **Strict prefix.** The first match that does not fit, and EVERY lower-ranked
   match, is dropped, even if a smaller lower-ranked record would fit: a
   lower-ranked record is never shown while a higher-ranked one is hidden.
   Records below the first drop are never fetched.
4. **A record is never shown truncated.** One whose own untruncated size
   exceeds the remaining record budget — including one larger than the whole
   budget — is dropped and named
   (`TestAssemble_OversizedRecordDroppedNeverTruncated`).

**Typical scopes reach record bodies.** `minTypicalRecordBodies` (2) is the
floor `TestAssemble_ShippedIndex_TypicalScopesReachRecordBodies` holds on the
SHIPPED `docs/adr` at the production cap: a migration scope must show ADR-006
and ADR-002 in full, a workflow-v2 schema-change scope ADR-067 and ADR-046. It
reddens when a governing record's body grows past the record budget or its
`applies_to` stops matching; the remedy is to narrow `applies_to`, split the
record, or raise the cap.

Dropped matches are named twice: in the index framing (a loud `NOT INCLUDED
because of the injection cap` list of id, path, status, stating the record
budget and that the index is capped separately) and in one
`document_truncated` entry of the selection shape (`selection:
"decision_record"`, `path` = index path, `commit`, `cap_bytes`,
`included_bytes` — the record budget and the included records' bytes —
`dropped: [{id, path, status, rank}]`, `dropped_count`),
written before any `document_injected` of the set (repodoc `AttributeSet`).

## Pinning

Every read — index and records — is a `repodoc.BaseSourceRunAdmission`
declaration at `Request.Commit`, the run's recorded admission commit. repodoc
refuses any ref that is not a 40-hex commit BEFORE branch resolution or any
fetch (`ErrUnpinnedBaseRef`), so a branch edit — including one on the run's
own branch — is never read (`TestAssemble_BasePinning`, which wires a commit
resolver that CAN resolve the branch, so only the base source refuses it). The
caller guards a nil / empty admission commit before calling.

## Framing

All framing is system-authored. Every interpolated value is a count, an id
`ParseIndex` validated, or a repo-authored path/status passed through
`repodoc.SanitizeMetadata`, so repo-chosen text cannot start a line.

- **Index** — heading `Decision record index`; the preamble states M of N
  records matched (and, when any successor was followed, how many more were
  selected because they supersede a selected record — with nothing followed
  the preamble is byte-identical to the pre-#4398 text), which ids are shown
  in full and in what order, that every
  other record is NOT shown — readable from the review tree when the reviewer is
  grounded (with the warning that the tree is the change's HEAD, not the
  admission commit, so a record the change edits reads there as edited), not
  readable at all when ungrounded — and the dropped list when anything was cut
  (a dropped successor's line adds `successor of <Via ids>`, a dropped
  amendment's `amendment of <AmendsVia ids>`). When any selected record amends
  another (`Selection.Amendments > 0`), the preamble says the extra records
  were selected because they "supersede or amend a selected record", states
  how many of the selected records amend a selected record and that an amended
  record still governs except where an amendment decides otherwise, and its
  order clause adds that an amendment ranks with the records it amends, after
  them; with successors but no amendment it is byte-identical to the
  pre-#4471 text.
- **Record** — heading `Decision record <id> — status: <status>`; the preamble
  states the status's meaning: `accepted` = settled; `unknown` = acceptance
  never confirmed, NOT a settled decision; `proposed` = not decided;
  `superseded` = no longer governing (names `superseded_by`); `rejected` =
  decided against. A status outside the closed set renders as `unknown`, never
  as accepted. A record with a non-empty `amended_by` adds, after its status
  meaning, `Amended by <ids>: it still governs, except where an amendment
  decides otherwise.` when it is accepted, and only `Amended by <ids>.`
  otherwise (a record that is not accepted governs nothing). A record with a
  non-empty `Via` adds a sentence that it was `selected because it supersedes
  <Via ids>` (and, when `MatchedPaths` is 0, that its own `applies_to` matches
  no path the change touches). A record with a non-empty `AmendsVia` adds that
  it was `selected because it amends <AmendsVia ids>; the amended record still
  governs except where this record decides otherwise.` (with the same
  `applies_to` clause when `MatchedPaths` is 0, and as `It is also selected
  because it amends …` after a `Via` sentence).

## Errors

| Sentinel | When | Persona degrade detail (server) |
|---|---|---|
| `ErrIndexMissing` | the index path is absent at the commit (also wraps `repodoc.ErrMissingDocument`) | `decision_record_index_missing` |
| `ErrInvalidIndex` | strict parse refusal, or a `Select` match error or dangling successor / amendment link | `decision_record_invalid` |
| `ErrUnresolvable` | a listed record absent at the commit, any fetch / pinning error, no resolver, or a cap smaller than the truncation marker | `decision_record_unresolvable` |

Any failure fails the WHOLE assembly: no partial `Selection` is returned.

## Fail-closed matrix

| Mode | Behavior | Test |
|---|---|---|
| each strict-parse rung | `ErrInvalidIndex` naming the record | `TestParseIndex_FailsClosed` |
| empty `applies_to` | never matches; never reaches `Predicate.Match` | `TestSelect_MatchesOnlyGovernedRecords` |
| `Predicate.Match` error | whole selection fails | `TestSelect_MatchErrorFailsWholeSelection` |
| dangling `supersedes` / `superseded_by` id | `ErrInvalidIndex` naming the record | `TestParseIndex_FailsClosed` (dangling cases) |
| self `supersedes` / `superseded_by` id | `ErrInvalidIndex` naming the record | `TestParseIndex_FailsClosed` (self cases) |
| hand-built index with a dangling `superseded_by` reached by the walk | whole selection fails | `TestSelect_DanglingLinkFailsWholeSelection` |
| malformed / non-list / dangling / self `amends` or `amended_by` id | `ErrInvalidIndex` naming the record and key | `TestParseIndex_FailsClosed` (amendment cases) |
| same id in `supersedes` + `amends`, or `superseded_by` + `amended_by` | `ErrInvalidIndex` naming the record and both keys | `TestParseIndex_FailsClosed` (exclusivity cases) |
| hand-built index with a dangling `amended_by` reached by the walk | whole selection fails | `TestSelect_DanglingLinkFailsWholeSelection` (`amended_by`) |
| supersession cycle | walk terminates, each record once | `TestSelect_SupersessionCycleTerminates` |
| amendment cycle | walk and placement terminate, each record once | `TestSelect_AmendmentCycleTerminates` |
| amendment reached by a reverse-only or forward-only link; amended record of a direct match | followed / not followed | `TestSelect_FollowsAmendments` |
| amendment would outrank the record it amends (newer, more paths, accepted over unknown) | amended record placed first | `TestSelect_FollowsAmendments` |
| record budget fits the amended record but not all its amendments | amendments dropped first, amended body kept | `TestAssemble_AmendedRecordBodyKeptAheadOfAmendments`, `TestPersonaPrompt_AmendmentFramingCrossesTheBoundary` (agenteval) |
| diamond (two selected predecessors, one successor) | successor selected once, `Via` holds both | `TestSelect_FollowsSupersessionToSuccessors` |
| successor of an unselected record | not selected | `TestSelect_FollowsSupersessionToSuccessors` |
| matches exceed the record budget | strict prefix included, rest dropped and named | `TestAssemble_FitsTheCap`, `TestAssemble_StrictPrefix` |
| record larger than the remaining record budget / the whole budget | dropped whole, never truncated | `TestAssemble_OversizedRecordDroppedNeverTruncated` |
| raw index over the cap | index shown truncated within its own cap; records still shown under the record budget | `TestAssemble_IndexOverCap`, `TestAssemble_IndexOverCap_AttributionCapBytes` |
| index fills >= 95% of the cap | every record that fits the record budget still shown | `TestAssemble_IndexNearCapDoesNotStarveRecords` |
| shipped index, typical scope, production cap | at least `minTypicalRecordBodies` governing bodies shown in full | `TestAssemble_ShippedIndex_TypicalScopesReachRecordBodies` |
| cap smaller than the marker | `ErrUnresolvable` (zero-cap guard) | `TestAssemble_CapSmallerThanMarker_FailsClosed` |
| branch ref handed as the commit | `repodoc.ErrUnpinnedBaseRef`, zero fetches | `TestAssemble_BasePinning` |
| index missing / fetch error / malformed / listed record missing / record fetch error / no resolver | typed error, no selection | `TestAssemble_FailureModes` |
| non-accepted or unrecognized status | framed as NOT settled, never `status: accepted` | `TestPromptDocuments_StatusFraming`, `TestPromptDocuments_UnknownRecordFromIndex` |
| non-accepted amended record | named as amended, never said to still govern | `TestPromptDocuments_AmendmentFraming` |
| control character in a dropped path | sanitized, cannot start a line | `TestPromptDocuments_DroppedNoticeSanitized` |

## Residuals

- **Index size.** `docs/adr/index.json` is 32,432 bytes for 95 records
  (measured 2026-10-16), 336 bytes under its own 32 KiB cap. The next record
  or two and the index itself is shown truncated (loudly), cutting its
  newest entries; the records are no longer affected, since #4399 gave them a
  separate budget. The optional amendment keys cost bytes only on the records
  that carry them; how the writer emits them is `docs/adr/README.md`'s. A compact index rendering stays out of scope: it would need
  a repodoc derived-document attribution contract (the rendered bytes would no
  longer be the fetched file's).
- **Grounding reads the head.** Records not injected are reachable only through
  read-only grounding, which reads the REVIEW TREE (the change's head), not the
  admission commit; injection is base-pinned, grounding is not. The index
  preamble says so.
- **`applies_to` is sparse.** 16 of 95 records declare a glob (counted from
  `docs/adr/index.json`, 2026-10-16), so most changes select few or no records;
  the index is injected regardless.
- **Which shipped links following reaches.** Two linked records declare
  `applies_to`. ADR-098 (`backend/internal/basedelta/**`) is a successor whose
  predecessor ADR-096 is never followed backwards. ADR-040
  (`docs/spec/operator-role.schema.json`, `.fishhawk/operator.yaml`) is the one
  amended record with a glob: a change to either path reaches ADR-097 and
  ADR-101 through `amended_by` once the records encode them as amendments
  (E78.13 / #4471; before that, ADR-101 as a successor). Every other link is
  reached only as records gain globs. A followed record competes for the
  record budget like any other match, and the amended-first rule decides who
  is cut: on the `.fishhawk/operator.yaml` scope at the production cap,
  ADR-040 and ADR-097 fit and ADR-101 (~23 KB) is dropped
  (`TestPersonaPrompt_AmendmentFramingCrossesTheBoundary`).
- **The shrink step bound is defense-in-depth.** The cap strictly decreases to
  the zero-cap guard, so the step bound is unreachable on any fixture;
  removing it was run and stays green.
