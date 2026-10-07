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
| `store.go` | `Store` over `user_report_cursors` (migration 0096): `Get`, `Init`, `Advance`; the closed `Source` set (`issues` plus its `issue_notes` note-floor row) |
| `scan.go` | `Scan`, `Report`, the report-level degradation set, `Recorder` |
| `comms.go` | The comms draft marker (E81.5 / #4011): `CommsMarkerName`, `CommsMarkerPrefix`, `MarkedReport`, `DraftMarker`, `ParseDraftMarkers`, `ContentHash` |

## Classification

Precedence: **`fishhawk_filed` > `bot` > `internal` > `external`**.

| Class | Rule |
|---|---|
| `fishhawk_filed` | The body carries a recognised Fishhawk marker **and** the author would otherwise classify `bot` or `internal` **and** the item is not a system note. |
| `bot` | The provider's forge-evidenced `Author.Bot`. GitHub: `user.type == "Bot"` or a `[bot]` login suffix. GitLab: a system note (`item.System`, basis `system_note`), or an access-token-bot username that a member lookup **corroborates**. |
| `internal` | The provider's `Author.Internal` (GitHub `OWNER`/`MEMBER`/`COLLABORATOR`; GitLab access level Developer (30) and above), or a login equal, case-insensitively, to the current captain's login on the **same** forge. |
| `external` | Everyone else, including an author whose association could not be resolved. |

A body marker is attacker-writable text. An external author's marker therefore
stays `external` with `MarkerFromExternal` set, so a consumer can flag the
forgery. An unresolved association is never `internal` (the safe direction).
A GitLab **system note** is forge-rendered around actor-controlled text (a
title edit quotes the new title), so it is always `bot` and never
`fishhawk_filed`; a marker in it sets `MarkerFromExternal`.

Recognised markers (closed list, `recognisedMarkers`): `fishhawk-intake:v1`
(`intakegroom.MarkerPrefix`, imported), `fishhawk-upkeep:v1` (pinned against
`upkeep.FindingMarker`), `fishhawk-fingerprint` and `fishhawk-sticky`
(literals; their renderers are unexported), and `fishhawk-comms:v1`
(`CommsMarkerPrefix`, pinned against `DraftMarker`).
`TestClassify_MarkerPrefixesMatchProducers` fails when the list grows without a
producer check. `markerIn` reports the FIRST matching marker and comms is
last, so a draft also carrying the intake marker reports basis
`marker:fishhawk-intake:v1`; its class is `fishhawk_filed` either way, and a
consumer reads `ParseDraftMarkers`, never the basis string.

The captain counts only when its subject is provider-qualified
(`captain.IdentityVerified`) with the page's own forge prefix
(`CaptainLoginFor`). The captain record is read with the scan's `Repo` string,
so the cursor and the captain record must be keyed by the same repo string.

## Comms draft marker

A captain-approved user-report draft (E81.5 / #3775) carries one marker naming
the reports it answers and each report's content hash at proposal time, so a
later scan can suppress re-proposing a report that has not materially changed.

**Format.** One line: `<!-- fishhawk-comms:v1 {"reports":[{"id":"UR-issue-7","content_hash":"<64 hex>"}]} -->`.
`CommsMarkerPrefix` is the full HTML-comment opening `<!-- fishhawk-comms:v1 `,
the `intakegroom.MarkerPrefix` convention, so prose quoting the bare token does
not match. `DraftMarker` keeps only valid entries, dedupes on `(id,
content_hash)`, sorts by id then hash, and returns `""` when nothing valid
remains. `json.Marshal` escapes `<`, `>` and `&`, and a valid entry holds only
`[A-Za-z0-9-]`, so no payload can contain `-->` or a line break.

**Entry validity.** `content_hash` matches `^[0-9a-f]{64}$`. `id` is CANONICAL:
it parses as `UR-issue-<n>` (n ≥ 1) or `UR-comment-<n>-<c>` (n, c ≥ 1) and
equals what `prompt.UserReportID` renders for those values, which rejects
leading zeros, signs and spaces. `UR-unknown-*` is refused because
`workmgmt.UserReportKind` is closed to `issue` and `comment`. An unparsable id
loses suppression and is re-proposed — never hidden.

**Strict parsing.** `ParseDraftMarkers(body)` walks EVERY occurrence of the
prefix and returns the deduped, sorted union of valid entries plus a
`malformed` count. A whole marker is dropped and counted when it is
unterminated, spans a line break before its ` -->`, fails strict decoding
(`DisallowUnknownFields` at every depth, a non-object payload), lacks the
`reports` key or carries it as `null`, or carries a second JSON value or
trailing data. Inside a well-formed marker each invalid entry is dropped
individually and NOT counted, so `{"reports":[]}` is well-formed with no
entries. Go's `encoding/json` matches field names case-insensitively and
keeps the last of duplicate keys; neither widens what an entry can carry,
because every entry is re-validated.

**`ContentHash(kind, title, body)`** is the lowercase-hex sha256 of the
normalized text, where `normalize(s) = TrimSpace(ReplaceAll(s, "\r\n",
"\n"))`. An issue hashes `normalize(title) + "\n" + normalize(body)`; a
comment (or any non-issue kind) hashes `normalize(body)` and ignores the
title. A known-answer vector in `comms_test.go` pins the exact input. Phases
4 and 7 call this function rather than re-deriving it.

**Trust rule.** The marker is attacker-writable body text. It is trusted ONLY
on an item `Classify` reports as `fishhawk_filed`; a `Result` with
`MarkerFromExternal` set is never trusted. The consumers — the scan's
suppression read (phase 4, #4014) and the on-approval filing that renders the
marker (phase 7, #4017) — enforce that; this package only renders and parses.

## Cursor

One row per `(account, repo, source)` in `user_report_cursors` (0096, forced
RLS, the 0089 predicate). `Source` is a closed set (`issues` today), so a
source that is unreachable never drags another source's cursor past unread
items. The row holds a read position only, never a chain fact. `issue_notes`
is not a scannable source: it is the `issues` scan's **note floor** row (see
below), and `ScanParams` refuses it as `Source`.

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
- **Note floor (`issue_notes`).** GitLab finds notes only through their
  issue, so when the issue listing truncates, the cursor advances to the
  `ResumeAt` past issues the scan never read, whose notes older than
  `ResumeAt` were never reported. `Scan` therefore passes a separate
  `NoteSince` (the `issue_notes` row; absent, or later than the cursor, means
  the cursor) and the provider filters notes against it, never `Since`. A
  truncated GitLab scan HOLDS the floor (`NextNoteCursor`) at the
  pre-truncation value; an untruncated scan reported every note at or after
  it, so the floor catches up to the cursor. GitHub's comment listing is
  independent and its `NextNoteCursor` always equals `NextCursor`. A page
  with no `NextNoteCursor` holds the floor; one beyond `NextCursor` is capped
  there. The floor is written through the same monotonic `Advance`.

## Scan ordering

1. Validate. A nil `Record` is refused before any read, because a scan with
   nowhere to record must not advance.
2. `Get`, then `Init` when absent. Then `Get` the note floor.
3. `Reader.ListUserReports(since, noteSince)`. Any error returns with both
   rows untouched. `workmgmt.ErrUserReportUnresumable` stays
   `errors.Is`-matchable through `Scan`, distinct from a transient failure.
4. Resolve the captain. This degrades and never fails.
5. Classify, build the `Report`, call `Record`. An error returns with the
   cursor untouched, so the next scan re-reads the same window.
6. `Advance` the note floor to `NextNoteCursor` FIRST, then the cursor to
   `NextCursor`. The order matters: an absent floor row reads as the cursor,
   so the held floor must be durable before the cursor moves past it. A
   failure in either returns `ErrCursorNotAdvanced`: the report exists and
   the next scan re-reads it. Duplicates are the safe direction; skips are
   not.

**Transaction scoping.** `Get`/`Init` and each `Advance` run in their OWN short
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
  code and no captain arm. A read error's `Detail` is a fixed string; the raw
  error goes to `ScanParams.Logger` (default `slog.Default()`), never into
  the report a comms renderer may surface.

## Residuals

- **Reactions are a snapshot.** Adding a reaction does not bump `updated_at`
  on either forge, so an item is re-listed only when its text, state, labels
  or (GitLab) notes change. Counts reflect the last update a scan observed.
  Read them as a lower bound.
- **The deletion residual is closed, including inside an equal-timestamp
  run.** Listings walk oldest-first under a keyset bound (the next request
  moves `since`/`updated_after` to the last item's `updated_at`), so no page
  offset exists for a deleted or transferred item to shift — except inside
  one equal-timestamp run longer than a page, which must be read by offset.
  There, removing an already-read item (deleted, transferred, or updated to
  the tail) between two offset requests shifts the next item off the page
  boundary. The client walk therefore re-walks such a pass from page 1 until
  two CONSECUTIVE passes observe the identical (id, `updated_at`) set, and
  only then moves the bound past the run. Because the set under a past bound
  only shrinks, identical consecutive passes prove no survivor was skipped
  (`keysetWalk` in both `githubclient` and `gitlabclient` carries the
  argument). A page cap hit before a run is confirmed truncates with
  `ResumeAt` AT the run, so the cursor cannot pass it and the next scan
  re-walks it; a cap that never confirms a run sitting at `since` fails
  closed with `workmgmt.ErrUserReportUnresumable`. A cap hit anywhere else
  truncates with `scan_truncated`, and the cursor stops at the last
  fully-read `updated_at`. The cost: a confirmed run takes at least two
  passes of requests.
- **GitLab confidentiality flips after the issue listing.** Confidentiality
  is decided over every occurrence the listing returned: an issue seen
  confidential once is excluded wholesale and no notes are read for it, and a
  note seen internal in any listing is excluded by id. An issue that turns
  confidential AFTER the listing's last read of it but BEFORE its notes are
  listed is not seen confidential, so its issue item and notes still reach
  the page; a note that turns internal after its last listing likewise. The
  window is one scan's duration; the next scan's listing sees the flag and
  excludes the identity from then on, but a page already recorded is not
  retracted.
- **One issue's notes past the client page cap fail the scan.** GitLab notes
  are read per issue by `gitlabclient.ListIssueNotes`, which pages to
  exhaustion under the pre-existing `maxListPages = 100` cap (100 notes per
  page, so 10 000 notes) and fails CLOSED past it rather than return a
  partial note set (`gitlabclient/issue_ops.go`). An issue with more notes
  than that fails every scan that lists it with a plain wrapped error (not
  `ErrUserReportUnresumable`), and the cursor holds. No behaviour change is
  planned; the cap is the existing convention.
- **First-window skew.** A host clock running ahead of the forge by more than
  the lookback empties the first window (see Cursor).
- **Concurrent scans** of one repository are harmless: `Init` and `Advance`
  are race-safe and monotonic, and duplicates are handled downstream. E81.5's
  scheduler is expected to serialise scans.
