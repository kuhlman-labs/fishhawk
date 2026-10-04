# backend/internal/userreport

The user-report source reader's classification, cursor and scan (E81.1 /
#3771). It reads every issue and issue comment created or updated since a
stored per-repository cursor through the `workmgmt.UserReportReader`
capability, classifies each item's author, hands the result to a caller's
`Recorder`, and advances the cursor only after that record succeeds.

**Nothing in production calls `Scan` yet.** E81.5 (#3775) wires it into the
comms stage. No HTTP route, MCP tool, CLI verb or audit category exists for it,
and the forge is only read.

| File | Holds |
|---|---|
| `classify.go` | `Classify`, the closed recognised-marker list, `CaptainLoginFor` |
| `store.go` | `Store` over `user_report_cursors` (migration 0096): `Get`, `Init`, `Advance`; the closed `Source` set |
| `scan.go` | `Scan`, `Report`, the report-level degradation set, `Recorder` |

## Classification

Precedence: **`fishhawk_filed` > `bot` > `internal` > `external`**.

| Class | Rule |
|---|---|
| `fishhawk_filed` | The body carries a recognised Fishhawk marker **and** the author would otherwise classify `bot` or `internal`. |
| `bot` | The provider's forge-evidenced `Author.Bot`. GitHub: `user.type == "Bot"` or a `[bot]` login suffix. GitLab: a system note, or an access-token-bot username that a member lookup **corroborates**. |
| `internal` | The provider's `Author.Internal` (GitHub `OWNER`/`MEMBER`/`COLLABORATOR`; GitLab access level Developer (30) and above), or a login equal, case-insensitively, to the current captain's login on the **same** forge. |
| `external` | Everyone else, including an author whose association could not be resolved. |

A body marker is attacker-writable text. An external author's marker therefore
stays `external` with `MarkerFromExternal` set, so a consumer can flag the
forgery. An unresolved association is never `internal` (the safe direction).

Recognised markers (closed list, `recognisedMarkers`): `fishhawk-intake:v1`
(`intakegroom.MarkerPrefix`, imported), `fishhawk-upkeep:v1` (pinned against
`upkeep.FindingMarker`), `fishhawk-fingerprint` and `fishhawk-sticky`
(literals; their renderers are unexported). E81.5 appends its comms marker.
`TestClassify_MarkerPrefixesMatchProducers` fails when the list grows without a
producer check.

The captain counts only when its subject is provider-qualified
(`captain.IdentityVerified`) with the page's own forge prefix
(`CaptainLoginFor`). The captain record is read with the scan's `Repo` string,
so the cursor and the captain record must be keyed by the same repo string.

## Cursor

One row per `(account, repo, source)` in `user_report_cursors` (0096, forced
RLS, the 0089 predicate). `Source` is a closed set (`issues` today), so a
source that is unreachable never drags another source's cursor past unread
items. The row holds a read position only, never a chain fact.

- **Anchor.** Every advance is `workmgmt.NextUserReportCursor`: the forge's
  own `Date` header from the scan's first response, minus a 2-minute overlap.
  That is the forge's clock domain, never fishhawkd's (the AGENTS.md
  cross-clock-domain trap, #3048). A missing `Date` holds the cursor.
- **First scan.** With no row, `Scan` computes `Now − InitialLookback` once
  (default 14 days) and stores it through `Store.Init`, an insert-if-absent that
  returns the **stored** value. `Scan` reads with that stored value, so a
  failed first scan retried with an advanced clock passes the identical
  `since`.
- **The lookback is a host-clock window bound only.** It sets only the first
  window. Every advance after it is forge-anchored. A host-clock skew larger
  than the lookback can only shorten that first window. A negative lookback
  is refused, so the first `since` is always earlier than `Now`.
- **Monotonic.** `Advance` is an upsert whose `ON CONFLICT ... WHERE
  cursor_at < EXCLUDED.cursor_at` refuses a backwards move. It returns the
  committed value and whether this call moved it.

## Scan ordering

1. Validate. A nil `Record` is refused before any read, because a scan with
   nowhere to record must not advance.
2. `Get`, then `Init` when absent.
3. `Reader.ListUserReports(since)`. Any error returns with the cursor
   untouched. `workmgmt.ErrUserReportUnresumable` stays `errors.Is`-matchable
   through `Scan`, distinct from a transient failure.
4. Resolve the captain. This degrades and never fails.
5. Classify, build the `Report`, call `Record`. An error returns with the
   cursor untouched, so the next scan re-reads the same window.
6. `Advance(NextCursor)`. A failure here returns `ErrCursorNotAdvanced`: the
   report exists and the next scan re-reads it. Duplicates are the safe
   direction; skips are not.

**Transaction scoping.** `Get`/`Init` and `Advance` each run in their OWN short
`postgres.WithTenant` transaction. No transaction is held across the forge
listing or the recorder. Record-then-Advance ordering is the only atomicity
guarantee between a recorded report and the cursor. The GitHub end-to-end test
asserts zero acquired pool connections while the forge fake serves and while
`Record` runs.

## Degradations

`Report.Degradations` carries two closed sets. `Degradation.Source` names
which one a code belongs to, and the two sets never share a value
(`TestReportDegradationCodesDisjointFromPageCodes`).

- **`page`**: the provider's `workmgmt.UserReportDegradationCode`, copied from
  the page: `reactions_partial`, `comment_reactions_unavailable`,
  `comments_via_issue_activity`, `bot_detection_heuristic`,
  `association_unresolved`, `cursor_anchor_unavailable`, `scan_truncated`,
  `confidential_excluded`. Meanings: `docs/board-capability-matrix.md`.
- **`report`**: this package's `ReportDegradationCode`: `captain_unavailable`.
  It is set when no captain reader is configured, the captain read fails, the
  seated subject is not identity-verified, or it is qualified for a different
  forge. The captain arm is then off. A **vacant** seat is a normal state: no
  code and no captain arm.

## Residuals

- **Reactions are a snapshot.** Adding a reaction does not bump `updated_at`
  on either forge, so an item is re-listed only when its text, state, labels
  or (GitLab) notes change. Counts reflect the last update a scan observed.
  Read them as a lower bound.
- **The deletion residual is closed.** Listings walk oldest-first under a
  keyset bound (the next request moves `since`/`updated_after` to the last
  item's `updated_at`). No page offset exists for a deleted or transferred
  item to shift, except inside one equal-timestamp run. A page cap hit inside
  such a run at `since` fails closed with
  `workmgmt.ErrUserReportUnresumable`. A cap hit anywhere else truncates with
  `scan_truncated`, and the cursor stops at the last fully-read `updated_at`.
- **First-window skew.** A host clock running ahead of the forge by more than
  the lookback empties the first window (see Cursor).
- **Concurrent scans** of one repository are harmless: `Init` and `Advance`
  are race-safe and monotonic, and duplicates are handled downstream. E81.5's
  scheduler is expected to serialise scans.
