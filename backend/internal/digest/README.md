# digest

The "since you last looked" digest (E75.6 / #3734, ADR-082 #3728 rule 7): a per-captain, per-repository summary of what the audit chain recorded since that captain's READ WATERMARK, computed on demand from `decision_index` (`backend/internal/decisionindex`, E75.2 / #3730) and the chain. Storage: migration 0089 (`captain_read_watermarks`, forced RLS byte-identical to 0088's policy, plus `decision_index_repo_sequence_idx`). This package has NO HTTP surface and registers NO audit category; the REST routes, the `digest_marked_read` registration and the MCP tool are separate slices of #3734.

## Mark-read ordering (load-bearing)

`Store.MarkRead(ctx, appender, MarkReadParams)`:

1. Resolve the current watermark. `to_sequence` AT OR BELOW it → no-op (`Advanced=false`), NOTHING appended.
2. `to_sequence` above the repository's chain head → `*BeyondChainHeadError` naming the head (a surface maps it to 400).
3. Append the `digest_marked_read` entry through the injected `Appender` FIRST.
4. ONLY on that append's success, upsert the watermark. An append error returns with the row untouched.

The upsert is monotonic (`ON CONFLICT … DO UPDATE … WHERE captain_read_watermarks.sequence < EXCLUDED.sequence`), so a concurrent lower write never regresses the row. **`MarkRead` reports what was COMMITTED, not what was requested**: the upsert returns a row only when THIS call raised the watermark; when its monotonic WHERE refuses, the row is re-read in a SEPARATE statement, so a lower request that read the old watermark before a concurrent higher one committed reports the higher `sequence` with `advanced=false` — never a false `advanced=true` at its own lower `to_sequence` (#3734 fix-up condition 4). The re-read MUST be a separate statement (#3852): every sub-statement of one query shares its snapshot, and `ON CONFLICT DO UPDATE` waits on a concurrent writer's row lock under READ COMMITTED, so a same-statement fallback read predates that commit — it reported the STALE pre-commit `sequence` when a concurrent higher mark won the row, and errored (`no rows in result set`) when a concurrent FIRST insert won it. The separate re-read takes a fresh snapshot and reports the latest committed `sequence`; a refused upsert whose re-read finds NO row (only a concurrent manual DELETE) fails closed with `ErrWatermarkRowVanished`, and any other upsert error is an error, never read as a refusal. `previous_sequence` / `had_previous` are the watermark the call READ before appending (what its entry records), which under a concurrent advance can sit below the value the upsert compared against. **Idempotence is a property of the WATERMARK, not of the chain**: a retry after an append succeeded but the advance failed appends a SECOND entry (the chain is append-only, and that is an honest record of the second attempt) and converges the watermark. Retrieval (`Build`) never writes.

The `Appender` is injected so the category literal lives with its registration: the surface maps `MarkedReadEvent` (and its `Payload()`: `repo`, `captain_subject`, `marked_by`, `previous_sequence`, `had_previous`, `to_sequence`, plus `captain_subject_basis` when set) to a GLOBAL-chain `digest_marked_read` entry partitioned by `AccountID` — the mark is repo- and subject-scoped and belongs to no run. `captain_subject` is the watermark KEY that moved; `marked_by` is the subject that performed the mark (E76.3 / #3766) and falls back to `captain_subject` when a caller leaves it empty, so the key is never silently absent.

**Whose watermark (E76.3 / #3766, ADR-083 rule 6).** This package keys on whatever `CaptainSubject` its caller passes; the REST surface (`backend/internal/server/digest.go`) chooses it. GET reads the repository's SEATED captain's watermark by default, falling back to the caller when the seat is vacant or the captain record is unreadable, with an optional read-only `captain_subject` override; mark-read ALWAYS advances the caller's OWN watermark (only the seated captain advances the seat's, because for them the keys coincide). Every response names the choice as `captain_subject_basis` (`captain` | `caller` | `caller_vacant` | `caller_unavailable` | `explicit`); `docs/api/v0.md` § Digest is the wire contract.

## Window

`from_sequence` and `to_sequence` are INCLUSIVE chain positions (`audit_entries.sequence`, a table-wide BIGSERIAL). Defaults: `from = watermark + 1`, `to = chain head` (max sequence over every run in the repo). The watermark is the last sequence read.

## Sections (closed set)

| Section | Source | Item cites |
|---|---|---|
| `merges` | `decision_index` class `merge_verdict` in the window | the `merge_verdict_recorded` entry |
| `waivers_and_deferrals` | classes `concern_waive` + `concern_defer` | the decision entry; `reason_sequence`/`reason_key` POINT at the reason (prose never copied). A row missing either → `reason_missing` degradation |
| `pages` | `clarification_requested`, `scope_amendment_requested`, `escalation_fired` entries in the window | the page entry, with `answered` + `answered_sequence` |
| `open_decisions` | stages parked awaiting a captain on non-terminal runs, ordered by the parking entry's sequence | the entry that PARKED the stage |

Deliberately ABSENT until their dependencies ship: `doctrine_changes` (#3733) and `scheduled_runs` (#3725). `campaign_gate_paged` is not a page here: condition 4 defines pages by their pairing, and it has none.

### `answered` is PAIRED, never proximate

| Page | Answered by |
|---|---|
| `clarification_requested` | `clarification_answered` on the SAME stage |
| `scope_amendment_requested` | `scope_amendment_decided` with the SAME `amendment_id` |
| `escalation_fired` | `approval_submitted` on the SAME stage |

The answer must be later on the chain than the page and at or below `to_sequence` (a digest is a snapshot). An unrelated later decision on the run never answers a page.

### Parking entry per state

`open_decisions` lists every CURRENTLY parked gate cited at or below `to_sequence` (without a cursor it starts at sequence 1, not the watermark — a gate parked before the last mark-read is still open). Each cites the LATEST entry on that stage among its state's parking categories:

| Stage state | Parking categories |
|---|---|
| `awaiting_approval` | `plan_generated`, `implement_reviewed`, `acceptance_outcome_recorded`, `escalation_fired` |
| `awaiting_input` | `clarification_requested` |
| `awaiting_scope_decision` | `scope_completeness_parked` (the #1151 park), `scope_amendment_requested` |
| `awaiting_deploy_approval` | `escalation_fired` (the deploy gate emits no dedicated parking entry) |

A parked stage with no such entry is a `parked_without_citation` GAP, never an item with a made-up sequence.

## Gaps and degradations

Gaps: `unindexed_decision` (a decision-bearing entry in the window with no index row — `decisionindex.GapsInWindow`), `source_entry_missing` (an index row citing a sequence with no chain entry; the item is still emitted with `source_missing`), `source_hash_mismatch`, `parked_without_citation`. Degradations: `scan_limit` (a bounded read hit its LIMIT; the collection's cursor retrieves the rest) and `reason_missing`. Nothing is silently dropped.

**An item-derived gap rides every response that carries its item.** `source_entry_missing` / `source_hash_mismatch` are reported on the full digest, on the owning content-section call (e.g. `section=merges`) BESIDE the item, and on the `gaps` selector — a section call no longer strips them (#3734 fix-up condition 1). When a content section hits its scan limit, its item-derived gaps PAST the limit are reached by following `gaps_next`: a `section=gaps` request folds the earliest such section continuation into the gaps cursor so re-scanning from there reaches them (#3734 fix-up condition 2/3).

**`parked_without_citation` gaps have no chain sequence**, so they ride a constant-size floor (the first `uncitedLimit` = 16) on the full digest and the initial `gaps` call, and the rest are OFFSET-paged by `uncited_next` (`section=open_decisions_uncited`, ordered by `(created_at, id)`) — following its cursor retrieves every uncited parked stage and terminates (#3734 fix-up condition 3). **Over-budget-floor escape** (#3734 condition 5): a floor of many uncited-parked gaps can exceed a small byte budget (`ErrBudgetTooSmall`); passing an explicit `from_sequence` (e.g. `watermark + 1`) on a continuation skips the uncited-parked floor, and those stages are retrieved separately via `section=open_decisions_uncited`.

## Reads

Hand-written pgx over `DBTX` (the decisionindex precedent). Every read is LIMIT-bounded (`DefaultScanLimit`, each read fetches limit+1 so the extra row IS the first omitted item) and index-served; none uses `audit.Repository.ListAll`. `ChainHead` and `pages` join `runs` on repo; `ParkedStages` reads stages by state (`stages_state_idx`) joined to non-terminal runs (`runs_repo_state_idx`) with a LATERAL latest-parking-entry lookup on `audit_entries_run_seq_idx`.

## The one bound

`Bound(d, budget)` is the single serialized-size bound for every surface (REST at `DefaultByteBudget` = 32768; the MCP tool at its resolved response budget). It MEASURES `json.Marshal` of the whole value with markers attached:

1. Caps every variable-length string field to `MaxFieldBytes` (256, measured ENCODED), ending it in `…` and setting `fields_truncated` — so any single item fits.
2. Keeps a constant-size floor: every section's markers and cursors, the sequence-less gaps/degradations (bounded at Build to 16 uncited stages), and for a `section` call that collection's FIRST element. A floor over budget is `ErrBudgetTooSmall` — never an over-budget or empty non-advancing page.
3. Grows a prefix in section order, then the gaps stream (sequenced gaps + degradations merged by sequence).
4. Every cut collection carries `truncated`, `omitted_count` and a cursor whose `from_sequence` is the FIRST OMITTED element's sequence, with `call` naming the exact next request (`GET /v0/digest?repo=…&section=…&from_sequence=…&to_sequence=…`). `complete` is true when nothing was elided.

The `section` selector accepts the four content sections plus `gaps` (the Gaps + Degradations collections) and `open_decisions_uncited` (the offset-paged `parked_without_citation` remainder). Following cursors always progresses: each call returns at least one element or reports the collection complete.

## Tests

pgtest-backed: `digest_test.go` (sections, each gap/degradation kind, pairing, parking citations, window defaults, cursor termination against real `Build`, two-captain independence, a `section=merges` call retaining its citation gap, a `section=gaps` cursor reaching an item-derived gap past the scan limit, and the uncited-parked remainder retrieved in full through `open_decisions_uncited`), `watermark_test.go` (each MarkRead branch, monotonic upsert, untenanted single row, an interleaved MarkRead reporting the committed higher sequence with `advanced=false`, two REAL two-connection overlaps through `MarkRead` — an uncommitted higher upsert and an uncommitted first insert held by `tx1` while `MarkRead` is observed in a `pg_stat_activity` Lock wait, both reporting the latest committed `sequence` (#3852) — the refused-upsert re-read branches over a pure fake `DBTX` (re-read error, vanished row, value reported), a non-`ErrNoRows` advance error never read as a refusal, and `marked_by` / `captain_subject_basis` threaded onto the payload), `rls_test.go` (NOBYPASSRLS probe; the in-test counterfactual swaps the policy to `USING (true)` and asserts account B's row becomes visible — FORCE RLS with NO policy denies everything, so dropping it proves nothing). `bound_test.go` is pure.
