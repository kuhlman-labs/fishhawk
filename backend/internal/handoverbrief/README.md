# backend/internal/handoverbrief

The handover brief (E76.4 / #3767, under ADR-083 #3751 and ADR-082 #3728 rule 7): a bounded, fully-cited summary of everything an incoming captain needs since the last handover. This package is the composition only — no HTTP route, no MCP tool, no audit category. The REST surface (`GET /v0/handover-brief`), the offer-time `brief_hash` stamp and the `fishhawk_handover_brief` tool are composed on top of it by `backend/internal/server` and `backend/internal/mcpserver`.

## Composition — existing readers, nothing re-derived

| Section | Part | Source |
|---|---|---|
| `what_changed` | `merges` | `digest.Build(section=merges)` over the window |
| `what_changed` | `waivers_and_deferrals` | `digest.Build(section=waivers_and_deferrals)` over the window |
| `needs_decision` | `unanswered_pages` | `digest.Build(section=pages)` over the window, filtered to `answered` false/unset |
| `needs_decision` | `open_decisions` | `digest.Build(section=open_decisions)` — every CURRENTLY parked gate (not window-limited: a gate parked before the handover is still open) |
| `in_flight` | `campaigns` | `Store.Campaigns` → `campaign.Repository.ListCampaigns`, states `pending`/`running`/`paused`/`awaiting_human` |
| `in_flight` | `runs` | `Store.Runs` → `run.Repository.ListRuns`, states `pending`/`running` |
| `delegation_in_force` | `workflows` | the caller's `delegationview.View` — tier, `must_page_human`, escalation ceilings, per-workflow content hash, `confirmation` |
| `standing_orders` | — | `workflow_sha`, `spec_version`, `schema_major`, source/ref and the view content hash from that same view |

Every digest item keeps the digest's citation (`source_sequence` + `source_entry_hash`), so the brief's citations ARE the digest's. In-flight items cite their row id (they are store rows, not chain facts). The window's gaps come from two further digest calls: `section=gaps` (only the `unindexed_decision` gaps — item-derived gaps already arrived with their section) and `section=open_decisions_uncited` (parked stages with no identifiable parking entry, continued via `uncited_next`).

`confirmation` is the constant `unavailable` until E76.5 (#3768) lands a confirmation read; `TestDelegationSection_ConfirmationIsUnavailable` is the edit point.

## Window

`DeriveWindow` is pure over the captain record's entries (`captain.Store.Read` — one derivation of "who is captain", not two): `from_sequence` = last `captain_assigned` sequence + 1 with basis `since_last_handover`, or 1 with basis `first_captain` when there is none; `to_sequence` = the repository's chain head (`digest.Store.ChainHead`). A fallback claim (`captain_claimed`) does not move the window — it is not a handover. `Request.FromSequence`/`ToSequence` override it (basis `requested`; a `to_sequence` above the head is `ErrInvalidRequest`). When `from_sequence > to_sequence` the window is empty and what_changed is honestly empty.

## Failure vs degradation

- **Failure** — only when the brief cannot be ESTABLISHED: the captain record read (`captain_record_read_failed`), the window/chain-head read (`window_read_failed`), or a missing required dependency (`dependency_unconfigured`). Compose returns an `*UnavailableError` wrapping `ErrUnavailable`; `UnavailableReason(err)` is the machine-readable marker a caller records (the offer path records `brief_unavailable` with an empty hash).
- **Degradation** — every section read error. The part is marked `unavailable` with a reason, a named `Degradation` is appended, the section is marked `unavailable` when all its parts are, and the brief STILL composes and carries a `brief_hash`. Kinds: `digest_section_failed`, `campaign_store_unconfigured`, `run_store_unconfigured`, `campaign_read_failed`, `run_read_failed`, `delegation_unavailable`, and `scan_limit` (a bounded read hit its LIMIT; the limit+1 probe row IS the first omitted item). A campaigns / runs part is one list query PER non-terminal state (`inFlightStates`: campaigns pending, running, paused, awaiting_human; runs pending, running), so EVERY overflowing state appends its own `continuations` entry at offset LIMIT, in state order, and `next` is a copy of the first (#3862). The part's `omitted_count` counts overflowing states — a lower bound on the rows. The `scan_limit` degradation detail still says "follow the part cursor"; it is a hash input, so it is deliberately left unchanged.

## Declared absences

Sections the issue names with no source on main are listed in `absent` with a reason and anchor — never emitted empty, never silently omitted:

| Section | Anchor | Why |
|---|---|---|
| `charter_revision` | #3242 | no charter revision identifier is recorded anywhere on main |
| `adr_index` | E78 | no ADR index exists |
| `doctrine_changes` | #3733 | the digest declares doctrine changes absent and ships no doctrine-change query |

`TestAbsentSections_NameCharterRevisionAndAdrIndex` is the edit point when any of these gains a source.

## brief_hash — one canonical form

`Hash` is hex(sha256) over `json.Marshal(canonical(b))`: the full, UNBOUNDED composition with `brief_hash` itself, the `section` selector and every truncation marker / bounding field (`truncated`, `next`, per-part `complete`/`truncated`/`omitted_count`/`next`/`continuations`, `gaps_truncated`/`gaps_omitted_count`/`gaps_next`/`uncited_next`, `fields_truncated`) zeroed. The window and degradations stay IN — they are composition facts. Every type is a plain struct with ordered slices and no map, so the bytes are a function of the content alone (the `delegationview` precedent).

`Compose` computes the hash ONCE. `Select` (one section) and `Bound` copy it through and NEVER re-hash: every render, bounded or not, section-selected or not, reports the hash of the canonical whole brief plus its own truncation markers.

**The brief is a point-in-time composition.** The hash commits to what was composed at that moment (for an offer: what the successor was shown at offer time). Live sections — parked gates, campaigns, runs — and the chain head may differ on a later read, so a later composition may hash differently; that is expected, not drift.

## Bound (ADR-077)

`Bound(b, budget)` mirrors `digest.Bound`: size is MEASURED (json.Marshal of the whole value with markers attached); variable-length strings are capped at `MaxFieldBytes` (`fields_truncated`); a constant-size floor (every section/part marker, standing orders, the declared absences, the sequence-less gaps/degradations, and for a single-section brief that section's first element) is never trimmed, and a floor over budget is `ErrBudgetTooSmall`; a prefix grows in section/part order, then the gaps stream. Every cut collection carries `truncated`, `omitted_count` and a `Cursor` whose `call` names the exact UNDERLYING query at the first omitted element:

- digest parts / gaps → `GET /v0/digest?from_sequence=…&repo=…&section=…&to_sequence=…`
- campaigns / runs → `GET /v0/campaigns?…` / `GET /v0/runs?…` with `state` and the offset `cursor` (base64url of `offset:N`, the server's list cursor). A cut in_flight part carries `continuations` — ONE cursor per state with omitted rows, in state order, `next` the first (#3862) — rebuilt on every cut from per-state kept/total counts: a state the cut reaches is continued at its ABSOLUTE kept offset (every composition reads each state from offset 0), including offset 0 for a state cut entirely; a state the cut leaves untouched carries its existing continuation through (the scan-limit one, or an earlier bound's, including one for a state an earlier bound cut entirely and so has no items). That carry-through is what lets the MCP tool re-bound a REST-bounded body without losing a state. A body from a pre-continuations fishhawkd (`next` only) is read as one continuation. States are walked canonical-order first, then any state seen only in items or continuations, so nothing is dropped. A part the cut does not reach is left untouched, so its composed continuations survive a fitting render.
- workflows → `GET /v0/repos/{owner}/{name}/delegation?source=…`, carrying the `source` the brief's `standing_orders` recorded (`run_cache` for an offer's composition). The route DEFAULTS to `source=ref`, so the bare path is a DIFFERENT read — following it can 502 `forge_unavailable` where the composed run-cache read succeeded. A brief with no standing orders (delegation unavailable) has no source to name and keeps the bare path.

`DefaultByteBudget` is the digest's ADR-077 number; the MCP tool re-bounds at its session budget.

## Tests

- `handoverbrief_test.go` (pgtest, REAL `digest.Build` / `captain.Store` / campaign + run repos): every seeded item cited; window cases; one test per degradation and per unavailable reason; hash stability / change / survival across `Select` + `Bound`.
- `store_test.go` (pgtest): repo/state scoping, the limit+1 bite with its cursor, and `TestStore_EveryOverflowingStateHasAContinuation` (two overflowing states in both in_flight parts, each continuation followed through the REAL repositories).
- `bound_test.go` (pure): fit, floor refusal, descending-budget progress with exact cursor calls, per-collection cursors, field caps; the #3862 per-state continuation tests against a simulated per-state backing store — `TestBound_InFlightContinuationPerOverflowingState` (descending-budget follow-and-cover sweep with a regime anti-vacuity check), `TestBound_ReboundKeepsContinuationOnlyStates` (REST bound → JSON → MCP re-bound), `TestBound_LegacyNextOnlyInFlightPartKeepsItsState`, `TestBound_InFlightContinuationsKeepNonCanonicalStates`, and `TestHash_IgnoresContinuations` (pins `Hash(synthetic(3))` to the golden computed before the field existed).
- `backend/internal/server` `TestHandoverBrief_ContinuationsFollowThroughRealListRoutes` follows every continuation through the REAL `/v0/campaigns` / `/v0/runs` handlers; `backend/internal/mcpserver` `TestHandoverBrief_RebindKeepsEveryStateContinuation` pins the tool re-bound.
