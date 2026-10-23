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
| `RenderIndex(index)` | The compact index rendering (`IndexRendering` = `adr-index-lines-v2`, below) the persona is shown. |
| `RenderedTitle(title)` | A title as `RenderIndex` renders it: unchanged up to `MaxRenderedTitleChars` = 100 code points, else its first 99 plus `…` (#4699, below). |
| `Assemble(ctx, resolver, Request)` | Fetch the index at the admission commit, parse, select, shape the index's compact rendering under its own cap, and fit whole records, then Decision-section excerpts, into a separate record budget. Returns a `*Selection`. |
| `DecisionExcerpt(content)` | A record's Decision section(s), verbatim, and whether it has any (the excerpt `Assemble` falls back to, #4489, below). |
| `Selection.PromptDocuments(grounded)` | The index, then each included record, then each excerpt, as `prompt.InjectedDocument`s. |
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
governs, and the record budget is a strict prefix at two fidelities (whole,
then Decision-section excerpt, #4489), so it is never shown less fully than an
amendment of it: when the budget cannot fit them all, amendments are excerpted,
then dropped, first. An amendment cycle (only a hand-built index can
carry one) leaves no eligible record; the highest-ranked remaining record is
then taken, so the pass terminates.

The change paths are the caller's: plan scope at plan review; approved plan
scope union diff at implement review.

## Index rendering (`RenderIndex`, #4478)

The persona is shown a COMPACT rendering of the index, not the committed file:
`RenderIndex` writes one compact JSON object per record, `\n`-terminated, in
the index's order, with no `schema_version` wrapper. `Record`'s slice tags all
carry `omitempty`, so every empty list (`supersedes`, `superseded_by`,
`amends`, `amended_by`, `applies_to`) is left out of its line; `omitempty`
changes encoding only, so `ParseIndex`'s strict decode still requires the
three non-optional lists. Because `Record` is marshalled directly, a field
added to it renders with no further code; `TestRenderIndex_RendersEveryRecordField`
reflects over `Record`'s json tags and fails when a field is missing from the
rendering or a slice tag lacks `omitempty`. `TestRenderIndex_EmptyListsOmitted`
pins the exact lines, an amended record's `amends` / `amended_by` included.

The encoder runs with `SetEscapeHTML(false)` (a `<` stays raw).
`encoding/json` escapes every byte below 0x20, U+2028 and U+2029, but NOT
U+0085 NEL, which repodoc honours as a line break; `RenderIndex` rewrites every
raw NEL (only ever inside a JSON string) as `\u0085`, so the rendering holds
exactly one repodoc line break per record and each line decodes back to its
record (`TestRenderIndex_OneLinePerRecordRoundTrips`).

**Title cap (`adr-index-lines-v2`, #4699).** Each title is rendered through
`RenderedTitle`: a title over `MaxRenderedTitleChars` = 100 characters (Unicode
code points, `utf8.RuneCountInString`) is cut to its first 99 code points plus
U+2026 `…`, so a rendered title is never over 100 characters. The cut is on a
code-point boundary (valid UTF-8 stays valid; a combining sequence can be
split), and the `…` is written raw (`encoding/json` does not escape it). A line
therefore decodes to its record with `Title == RenderedTitle(Title)`; the
caller's `Index` is not modified, and the stored title in `docs/adr/index.json`
and the record file is unchanged. Measured on the 113-record index
(2026-10-23), 53 titles are over the cap and the rendering drops from 27,143 to
24,200 bytes (2,943 saved). New records cannot add a cut title:
`scripts/check-adr`'s `C-title-length` refuses a title over 100 characters from
ADR-119 on (`TITLE_MAX_CHARS`, `TITLE_RULE_FROM`; ADR-001 to ADR-118 are
grandfathered), and `TestTitleCapMatchesCheckADR` pins the two constants
together. v1 rendered every title in full.

`Assemble` shows it through `repodoc.Fetched.Derive(IndexRendering, …)`: the
index document's `content_hash` and `original_bytes` stay the committed file's,
while `Content`, `rendered_bytes`, the cut and the marker are the rendering's,
and its audit entries carry `rendering` / `rendering_bytes` (repodoc README §
"Derived renderings"). The framing says the index is a compact rendering and
how to read it, including that a title over 100 characters is shown as its
first 99 followed by `…` and that the record file carries the full title, and
the Source line says `shown as its adr-index-lines-v2 rendering, not
verbatim`.

## Byte budget (`Assemble`)

**Two budgets (#4399).** The index and the records are budgeted SEPARATELY,
each at the resolver's effective cap, `repodoc.Resolver.CapBytes()`
(`DefaultMaxBytes` = 32768 unless overridden), so the index's size can never
starve the record bodies. Before #4399 one budget counted both, and the
shipped index alone (~31 KiB) left no room for any record: the architect
persona reviewed from index titles only.

- **Index** — its compact rendering, shaped under its own per-document cap, the
  resolver's cap (shrink-to-fit, below). Its own `document_injected` (and, when cut, its
  per-document `document_truncated`) split its numbers: `document_injected`
  carries `rendered_bytes` and `truncated` but no `cap_bytes`; only the
  per-document `document_truncated` carries `cap_bytes`. That `cap_bytes` is
  the cap it was actually cut under, so it is below the resolver's cap
  whenever the index was reshaped.
- **Records** — fitted, in rank order, whole and then as Decision-section
  excerpts (#4489), against a separate **record budget** = the resolver's cap.
  `Selection.CapBytes` is that record budget and `Selection.IncludedBytes` the
  sum of the rendered bytes of every record document SHOWN, whole or excerpt
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

1. **The index is always included.** Its compact rendering is shaped under
   its own cap; when its rendered size (including repodoc's truncation marker,
   which is appended AFTER the cut) exceeds the cap, it is reshaped under a cap
   reduced by the overflow.
   The loop is bounded (`maxIndexShrinkSteps`), the cap is clamped at zero and
   never negative, and an index that does not fit even at a zero-byte cut (a
   budget smaller than the marker) fails the assembly (`ErrUnresolvable`).
2. **Each match, in rank order,** is fetched at the SAME commit and shaped
   under a cap **at least its raw size** — so what is measured is its
   UNTRUNCATED rendered size, never a size under the shrunk budget — and is
   included WHOLE only if that fits what remains of the record budget. The
   index draws nothing from it (`TestAssemble_IndexNearCapDoesNotStarveRecords`).
   The first match that does not fit whole, and every lower-ranked match, is
   offered only as its Decision-section excerpt, likewise measured untruncated
   and shown when it fits what remains (below).
3. **Strict prefix, at two fidelities.** The records SHOWN (whole or excerpt)
   are a strict prefix of the ranking: the first match with no Decision
   section, or whose excerpt does not fit, and EVERY lower-ranked match, is
   dropped, even if a smaller lower-ranked record would fit, and records below
   the first drop are never fetched. The records shown WHOLE are a strict
   prefix of those shown: once a record is excerpted, no lower-ranked record is
   shown whole, even one that would fit. So a lower-ranked record is never
   shown more fully than a higher-ranked one.
4. **A record is never shown truncated.** It is shown whole, or as its
   complete Decision section(s), or not at all; one whose untruncated size
   exceeds the remaining record budget and that has no fitting excerpt —
   including one larger than the whole budget with no Decision section — is
   dropped and named (`TestAssemble_OversizedRecordDroppedNeverTruncated`).

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
`included_bytes` — the record budget and the shown records' bytes, whole and
excerpts — `dropped: [{id, path, status, rank}]`, `dropped_count`),
written before any `document_injected` of the set (repodoc `AttributeSet`).

## Decision-section excerpt (`DecisionExcerpt`, #4489)

A record that does not fit the record budget whole is offered as its Decision
section(s) instead of being dropped, so a large record — ADR-101 (~23 KB) on
ADR-040's scope — still reaches the reviewer as its decision.

- **Sections.** A section boundary is a level-1 or level-2 ATX heading at
  column 0 (`^#{1,2}(?:[ \t]|\r?\n?$)`, an empty heading included). A section
  runs from its heading to the next boundary or EOF; a level-3+ heading never
  starts or ends one. A section is a Decision section when its heading matches
  `(?i)^##[ \t]+decisions?\b`: `## Decision`, `## Decisions`,
  `## Decision (2026-05-21)` and `## Decision addendum …` match;
  `## Decisionmaking`, `### Decision` and `## Recommendation / Decision` do not.
  EVERY Decision section is kept, verbatim and concatenated in file order;
  line terminators (CRLF included) are kept as written.
- **Fences.** A heading-shaped line inside a fenced code block (CommonMark
  0.31.2 §4.5) is code: it neither ends nor starts a section. An opening fence
  is 0-3 spaces then a run of at least three backticks or tildes (a backtick
  fence's info string may not contain a backtick); a closing fence is 0-3
  spaces, a run of the same character at least as long, then only spaces or
  tabs; an unclosed fence runs to EOF.
- **Rendering and hash domain.** `Assemble` shows the excerpt through
  `repodoc.Fetched.Derive(DecisionExcerptRendering, …)`
  (`adr-decision-section-v1`): `content_hash` and `original_bytes` stay the
  committed FILE's, while `Content` and `rendered_bytes` are the excerpt's, and
  its `document_injected` carries `rendering` / `rendering_bytes` (repodoc
  README § "Derived renderings"). The excerpt is shaped under a cap at least
  its own size, so it is never cut; a quoted passage verifies against the
  excerpt the reviewer saw, recording the file's hash.
- **Two-fidelity strict prefix.** Byte budget items 2-4 above:
  `Selection.Included` (whole) then `Selection.Excerpted` (excerpts) is a
  strict prefix of the ranking, and whole bodies are a strict prefix of the
  shown set. With the amended-first placement (#4471), an amendment is never
  shown more fully than the record it amends.
- **Fallthrough.** A record with no Decision section, or whose excerpt does not
  fit what remains, is dropped with everything below it, as before, and named
  in the `NOT INCLUDED` list and the selection-level `document_truncated`.

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

- **Index** — heading `Decision record index`; after its opening sentence the
  preamble says the index is shown as a compact rendering, one JSON object per
  record per line in the file's order, with an empty list field left out
  (#4478; the byte-identical claims below hold apart from that sentence); it
  then states M of N
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
  pre-#4471 text. When any record is shown as an excerpt
  (`len(Selection.Excerpted) > 0`), the preamble adds, after the shown-in-full
  sentence (or `none of them is shown in full in this prompt.`), `<n> of them
  did not fit the record budget whole and are shown below as excerpts, their
  Decision section only, in rank order: <ids>.`; the not-shown clause becomes
  `Every other record, and every section of an excerpted record outside its
  Decision section, is NOT shown in this prompt` (grounded / ungrounded tails
  unchanged); and the dropped list's header says the record budget is for
  `the records shown in full or as excerpts`. With no excerpt every one of
  these strings is byte-identical to the pre-#4489 text.
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
- **Excerpt** (#4489) — the record framing above, unchanged (status, amended
  by, `Via`, `AmendsVia`), with ` (Decision section only)` appended to the
  heading and a note appended to the preamble: only the Decision section
  (every section whose heading begins with Decision) is shown, verbatim; its
  other sections are NOT shown; and its full text, NAMED BY ITS PATH in the
  preamble itself, including any Recommendation or Options section the
  Decision adopts by reference (such as "D1–D10 as written"), was omitted for
  budget. Grounded, the note says to read text the Decision refers to from the
  review tree at that path rather than infer it (with the same HEAD-not-
  admission-commit warning as the index); ungrounded, that it cannot be read in
  this review and must not be inferred — the framing the architect remit's
  "Records not shown in full" rule keys on. The Source line says
  `shown as its adr-decision-section-v1 rendering, not verbatim`.

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
| index rendering over the cap | index shown truncated within its own cap (the cut, marker and per-document `document_truncated` count rendering bytes and carry `rendering` / `rendering_bytes`); records still shown under the record budget | `TestAssemble_IndexOverCap`, `TestAssemble_IndexOverCap_AttributionCapBytes` |
| raw index over the cap, rendering under it | the rendering shown whole; hash and `original_bytes` the raw file's | `TestAssemble_IndexShownAsCompactRendering` |
| index rendering fills >= 95% of the cap | every record that fits the record budget still shown | `TestAssemble_IndexNearCapDoesNotStarveRecords` |
| shipped index rendering past the production cap, or below the measured record-count floor | verify fails naming the overflow | `TestAssemble_ShippedIndex_RenderingFitsCap`, `TestShippedIndex_WithinInjectionCap`; the check's RED is pinned by `TestAssemble_IndexRenderingOverflow_IsCaught` |
| shipped rendering's headroom under the production cap below `minHeadroomLines` = 10 of its largest line (#4699 AC4) | verify fails naming the numbers and the tiering remedy | `TestAssemble_ShippedIndex_HeadroomFloor`; its RED (floor one line above the available headroom) is pinned by `TestAssemble_HeadroomFloor_IsCaught` |
| title over / exactly at the cap (ASCII and 2-byte runes), a cut next to `\n` / NEL | cut to 99 code points plus a raw `…`, exact-100 unchanged, valid UTF-8, one line break per record, strict decode to `RenderedTitle` | `TestRenderIndex_CapsLongTitles`, `TestRenderedTitle` |
| shipped index: a rendered title other than `RenderedTitle(stored)`, or the cap saving under `minTitleCapSavings` = 2,500 bytes (#4699 AC2, summed per title from `RenderIndex`'s own output) | verify fails | `TestShippedIndex_TitleCapShrinksRendering` |
| `MaxRenderedTitleChars` and `scripts/check-adr`'s `TITLE_MAX_CHARS` / `TITLE_RULE_FROM` drift apart | verify fails | `TestTitleCapMatchesCheckADR` |
| record field missing from the rendering, a slice tag without `omitempty`, a raw NEL, an HTML-escaped `<` | rendering refused by test | `TestRenderIndex_RendersEveryRecordField`, `TestRenderIndex_OneLinePerRecordRoundTrips` |
| shipped index, typical scope, production cap | at least `minTypicalRecordBodies` governing bodies shown in full | `TestAssemble_ShippedIndex_TypicalScopesReachRecordBodies` |
| cap smaller than the marker | `ErrUnresolvable` (zero-cap guard) | `TestAssemble_CapSmallerThanMarker_FailsClosed` |
| branch ref handed as the commit | `repodoc.ErrUnpinnedBaseRef`, zero fetches | `TestAssemble_BasePinning` |
| index missing / fetch error / malformed / listed record missing / record fetch error / no resolver | typed error, no selection | `TestAssemble_FailureModes` |
| non-accepted or unrecognized status | framed as NOT settled, never `status: accepted` | `TestPromptDocuments_StatusFraming`, `TestPromptDocuments_UnknownRecordFromIndex` |
| non-accepted amended record | named as amended, never said to still govern | `TestPromptDocuments_AmendmentFraming` |
| control character in a dropped path | sanitized, cannot start a line | `TestPromptDocuments_DroppedNoticeSanitized` |
| heading / fence rules of the excerpt parser (no Decision heading, EOF, H1 and empty-heading boundaries, level-3 kept, `### Decision` / indented / `## Decisionmaking` / `## Recommendation / Decision` not a start, dated / plural / lower-case match, multiple sections, fenced headings, fence close rules, two-character runs, CRLF) | exact excerpt bytes, `nil, false` when none | `TestDecisionExcerpt` |
| shipped ADR-040 / ADR-097 / ADR-101 Decision heading form unrecognized, or the cut leaks Recommendation / Context / Consequences text | verify fails | `TestDecisionExcerpt_ShippedRecords` |
| record over the budget whole, excerpt fits | shown as its excerpt (`adr-decision-section-v1`, the file's hash and size, never truncated) | `TestAssemble_DecisionExcerptFallback` (excerpt fits) |
| record over the budget whole with no Decision section | dropped with every lower-ranked record, which is never fetched | `TestAssemble_DecisionExcerptFallback` (no Decision section) |
| excerpt over the remaining record budget | dropped with every lower-ranked record; `IncludedBytes` stays `<= CapBytes` | `TestAssemble_DecisionExcerptFallback` (excerpt over the remaining budget) |
| lower-ranked record that would fit whole after an excerpt | shown only as its excerpt (fidelity never increases) | `TestAssemble_DecisionExcerptFallback` (fidelity), `TestShippedIndex_AssembleKeepsAmendedBodyAndFramesAmendments` |
| excerpt framing (index sentence, widened not-shown clause, dropped header, heading, note naming the record path with its grounded / ungrounded tail, Source line, a forged delimiter in the Decision section) | framed as an excerpt; delimiter neutralized | `TestPromptDocuments_ExcerptFraming`; on the shipped ADR-101 excerpt, `TestShippedIndex_ADR040ScopeShowsADR101DecisionExcerpt` |
| shipped ADR-040 scope at the production cap | ADR-040 and ADR-097 whole, ADR-101 as its Decision excerpt | `TestShippedIndex_ADR040ScopeShowsADR101DecisionExcerpt`, `TestPersonaPrompt_AmendmentFramingCrossesTheBoundary` (agenteval), `TestDecisionRecordPersona_OverCap_ShowsDecisionExcerpt` (server) |

## Residuals

- **Index size.** The persona is shown the index's compact rendering
  (`RenderIndex`, #4478), not the committed file. Measured 2026-10-23 on 113
  records with the #4699 title cap (`adr-index-lines-v2`):
  `docs/adr/index.json` is 41,625 raw bytes and its rendering 24,200 bytes
  (27,143 uncapped), leaving 8,568 bytes under the 32 KiB cap — 21 more lines
  at the largest current line (405 bytes).
  `TestAssemble_ShippedIndex_HeadroomFloor` fails the ADR landing that leaves
  fewer than 10 such lines (#4699 AC4): the early warning to tier the index
  (collapse superseded or older records), which #4699 deferred until about 150
  ADRs, or to raise the cap — an operator decision, not a flake.
  `TestAssemble_ShippedIndex_RenderingFitsCap` still fails the change whose
  record makes the rendering overflow the cap (and keeps its 2026-10-16
  record-count floor: 25 more records at 428 bytes beyond the 95 measured then),
  as does `TestShippedIndex_WithinInjectionCap`. Concurrent ADR merges can still
  overflow on main between their verifies, as with any shared-file budget. The
  raw file's size no longer reaches the prompt; how the writer emits the
  optional amendment keys is `docs/adr/README.md`'s.
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
  ADR-040 and ADR-097 fit whole and ADR-101 (~23 KB) is shown as its ~1.5 KB
  Decision-section excerpt (#4489;
  `TestShippedIndex_ADR040ScopeShowsADR101DecisionExcerpt`,
  `TestPersonaPrompt_AmendmentFramingCrossesTheBoundary`). That excerpt is a
  summary ("Adopt Option C with D1–D10 as written" plus the Q1–Q8 answers):
  the D1–D10 text lives under ADR-101's Recommendation and is still not shown.
- **Records with no Decision-prefixed heading have no excerpt.** ADR-026,
  ADR-030 and ADR-071 carry no `## Decision…` heading today, so one that does
  not fit whole is dropped as before. Setext headings and indented ATX headings
  are not recognized (a deliberate narrowing of CommonMark §4.2).
- **An excerpt-only selection writes no selection-level row.** The
  selection-level `document_truncated` is written only when something is
  dropped; an excerpted record is marked by its own `document_injected`'s
  `rendering` / `rendering_bytes`.
- **`PersonaArmRender.MatchedIDs` under-reports excerpts.** agenteval's
  persona-arm render lists included and dropped ids only; its tests read
  `Selection.Excerpted` directly. Follow-up filed by the operator (#4489).
- **The shrink step bound is defense-in-depth.** The cap strictly decreases to
  the zero-cap guard, so the step bound is unreachable on any fixture;
  removing it was run and stays green.
