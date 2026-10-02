# decisionrecord — decision-record selection for reviewer personas

`backend/internal/decisionrecord` selects the repository decision records that
bear on a change and assembles them for injection into a reviewer persona's OWN
prompt (ADR-084 D4(b) / binding rule 4, E78.5 / #3756). It reads and renders
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
| `Select(index, changePaths)` | The records whose `applies_to` matches a change path, ranked. |
| `Assemble(ctx, resolver, Request)` | Fetch the index at the admission commit, parse, select, and fit the index plus whole records into the cap. Returns a `*Selection`. |
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
`supersedes` / `superseded_by` entry not matching the id pattern; an
`applies_to` glob `spec.Predicate.Validate` refuses; a duplicate id; a duplicate
path. `TestParseIndex_AcceptsTheShippedIndex` parses `docs/adr/index.json` so the
parser cannot drift stricter than the index `scripts/check-adr` generates.

## Selection and ranking (`Select`)

A record with an EMPTY `applies_to` never matches: it governs no named path, and
`spec.Predicate.Match` refuses an empty predicate (it is not match-all). Every
other record is matched with `spec.Predicate{Paths: applies_to}` — the shared
doublestar matcher — against each DISTINCT change path; a `Match` error fails
the whole selection (`ErrInvalidIndex`), never reads as a non-match.

Ranking is deterministic: **accepted first**; then **more distinct change paths
matched**; then the **higher record number** (newer) first, compared as a
digit string so a long number cannot overflow; then **id ascending**. Rank is
1-based.

The change paths are the caller's: plan scope at plan review; approved plan
scope union diff at implement review.

## Byte budget (`Assemble`)

**Cap domain.** The budget is the resolver's effective cap,
`repodoc.Resolver.CapBytes()` (`DefaultMaxBytes` = 32768 unless overridden).
**`cap_bytes` counts the index's rendered bytes plus the selected records'
rendered bytes** — `repodoc.Document.RenderedBytes`, the shown body between the
delimiters, recorded as `rendered_bytes` on each `document_injected`. **The
persona remit and all fixed system framing (headings, preambles, trust notes,
the data clause, Source lines, the dropped-records notice) are OUTSIDE it**: the
remit is its own repodoc document under its own cap. `Selection.IncludedBytes`
is that sum and is always `<= CapBytes`.

1. **The index is always included.** Shaped under the cap; when its rendered
   size (including repodoc's truncation marker, which is appended AFTER the
   cut) exceeds the cap, it is reshaped under a cap reduced by the overflow.
   The loop is bounded (`maxIndexShrinkSteps`), the cap is clamped at zero and
   never negative, and an index that does not fit even at a zero-byte cut (a
   budget smaller than the marker) fails the assembly (`ErrUnresolvable`).
2. **Each match, in rank order,** is fetched at the SAME commit and shaped
   under a cap **at least its raw size** — so what is measured is its
   UNTRUNCATED rendered size, never a size under the shrunk budget — and is
   included only if that fits the remaining budget.
3. **Strict prefix.** The first match that does not fit, and EVERY lower-ranked
   match, is dropped, even if a smaller lower-ranked record would fit: a
   lower-ranked record is never shown while a higher-ranked one is hidden.
   Records below the first drop are never fetched.
4. **A record is never shown truncated.** One whose own untruncated size
   exceeds the remaining budget — including one larger than the whole cap — is
   dropped and named (`TestAssemble_OversizedRecordDroppedNeverTruncated`).

Dropped matches are named twice: in the index framing (a loud `NOT INCLUDED
because of the injection cap` list of id, path, status) and in one
`document_truncated` entry of the selection shape (`selection:
"decision_record"`, `path` = index path, `commit`, `cap_bytes`,
`included_bytes`, `dropped: [{id, path, status, rank}]`, `dropped_count`),
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
  records matched, which ids are shown in full and in what order, that every
  other record is NOT shown — readable from the review tree when the reviewer is
  grounded (with the warning that the tree is the change's HEAD, not the
  admission commit, so a record the change edits reads there as edited), not
  readable at all when ungrounded — and the dropped list when anything was cut.
- **Record** — heading `Decision record <id> — status: <status>`; the preamble
  states the status's meaning: `accepted` = settled; `unknown` = acceptance
  never confirmed, NOT a settled decision; `proposed` = not decided;
  `superseded` = no longer governing (names `superseded_by`); `rejected` =
  decided against. A status outside the closed set renders as `unknown`, never
  as accepted.

## Errors

| Sentinel | When | Persona degrade detail (server) |
|---|---|---|
| `ErrIndexMissing` | the index path is absent at the commit (also wraps `repodoc.ErrMissingDocument`) | `decision_record_index_missing` |
| `ErrInvalidIndex` | strict parse refusal, or a `Select` match error | `decision_record_invalid` |
| `ErrUnresolvable` | a listed record absent at the commit, any fetch / pinning error, no resolver, or a cap smaller than the truncation marker | `decision_record_unresolvable` |

Any failure fails the WHOLE assembly: no partial `Selection` is returned.

## Fail-closed matrix

| Mode | Behavior | Test |
|---|---|---|
| each strict-parse rung | `ErrInvalidIndex` naming the record | `TestParseIndex_FailsClosed` |
| empty `applies_to` | never matches; never reaches `Predicate.Match` | `TestSelect_MatchesOnlyGovernedRecords` |
| `Predicate.Match` error | whole selection fails | `TestSelect_MatchErrorFailsWholeSelection` |
| matches exceed the budget | strict prefix included, rest dropped and named | `TestAssemble_FitsTheCap`, `TestAssemble_StrictPrefix` |
| record larger than the remaining budget / the cap | dropped whole, never truncated | `TestAssemble_OversizedRecordDroppedNeverTruncated` |
| raw index over the cap | index shown truncated within the cap; every match dropped | `TestAssemble_IndexOverCap` |
| cap smaller than the marker | `ErrUnresolvable` (zero-cap guard) | `TestAssemble_CapSmallerThanMarker_FailsClosed` |
| branch ref handed as the commit | `repodoc.ErrUnpinnedBaseRef`, zero fetches | `TestAssemble_BasePinning` |
| index missing / fetch error / malformed / listed record missing / record fetch error / no resolver | typed error, no selection | `TestAssemble_FailureModes` |
| non-accepted or unrecognized status | framed as NOT settled, never `status: accepted` | `TestPromptDocuments_StatusFraming`, `TestPromptDocuments_UnknownRecordFromIndex` |
| control character in a dropped path | sanitized, cannot start a line | `TestPromptDocuments_DroppedNoticeSanitized` |

## Residuals

- **Index size.** `docs/adr/index.json` is ~17 KB for 55 records (measured
  2026-10-02), so roughly half the 32 KiB budget goes to the index before any
  record. Past ~110 records the index alone exceeds the cap: it is shown
  truncated (loudly) and no record fits. A compact index rendering is out of
  scope here.
- **Grounding reads the head.** Records not injected are reachable only through
  read-only grounding, which reads the REVIEW TREE (the change's head), not the
  admission commit; injection is base-pinned, grounding is not. The index
  preamble says so.
- **`applies_to` is sparse.** 11 of 55 records declare a glob today, so most changes
  select few or no records; the index is injected regardless.
- **The shrink step bound is defense-in-depth.** The cap strictly decreases to
  the zero-cap guard, so the step bound is unreachable on any fixture;
  removing it was run and stays green.
