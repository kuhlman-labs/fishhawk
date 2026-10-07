# backend/internal/intakegroom

Pure signal derivation for the event-driven intake micro-groom that runs on
each filed work item (#2239, E54.7).

## What it is

Three advisory signals derived from a filing and a bounded window of the
target repository's existing items:

- **Duplicate candidates** — lexical title-token overlap, confidence-banded,
  at most `MaxDuplicates`.
- **A parent-epic suggestion** — only when the filing declared no parent.
- **A provisional charter-anchored score** — citing only the rubric lines
  that are decidable from the filing's own structure.

Plus one piece of provenance that is NOT a signal: **derives-from**. A filing
may declare the same-repo items it was written from (`Filing.SourceNumbers`,
parsed from the filer's `source_refs` by `ParseSourceRefs`). `Duplicates`
excludes those items before scoring — a draft derived from `#X` is never
reported as `#X`'s duplicate, while an unrelated near-duplicate `#Y` still is —
and `Evaluate` reports them as `Signals.DerivesFrom` (resolved against the
window by `DerivesFromWindow`: title/url/closed with `in_window: true`, or
number-only with `in_window: false`). The epic suggestion is unaffected: a
source item may legitimately be the right parent (#3774, E81.4).

Plus the body surface: `RenderBody` appends a human-readable advisory
section and a hidden single-line marker, and `ParseBody` recovers the
signals from that marker so #2236's periodic sweep can read the intake
analysis back instead of redoing it.

## What it deliberately is NOT

**Not a decision.** Everything here is a candidate for a human. Nothing
closes, merges, relabels or transitions anything; the dedup decision stays a
workflow action class (charter §5.5, "nothing destructive by default"). A
duplicate candidate is a line on a body and a field on a response, and that
is the whole of its effect.

**Not semantic judgement.** Scoring cites exactly three rubric lines — `S2`
(a duplicate candidate at medium confidence or better exists), `S4` (a
missing label namespace or an unlinked parent epic), `U4` (no declared
`depends_on` edge). Each is decidable from the filing's own structure. The
whole V and R groups, and `S1`/`S3`/`S5`, require knowing what the item means
and where the phase stands — that is the periodic sweep's job (#2236), and a
structural proxy for it would produce confident citations nobody could
defend.

**Not authoritative about the charter.** A rule whose rubric id the charter
does not declare drops its citation *and* its weight; the charter is the
authority, and charter §6 says ids are retired rather than recycled, so
citing an id that is no longer there would silently rewrite what the citation
means. When no citation survives, the result is `Unscored` with an explicit
`CharterGap` — charter §6.6 makes a gap a finding, and a fabricated citation
to satisfy the shape is the one thing this package must never emit.

**Not a forge or workmgmt consumer.** The package declares its own input
vocabulary (`Filing`, `Candidate`, `Charter`) and imports neither. The caller
adapts provider records at the call site. That is what keeps every derivation
unit-testable against literals.

## Contract

- `Evaluate(Filing, []Candidate, Charter) Signals` is **pure and total** — it
  never returns an error, because there is no decision a caller could make on
  one. A partial or empty input yields a degraded `Signals`, and the caller
  files the work item regardless.
- `Signals.WindowTruncated` and `Signals.DurationMS` are set by the CALLER.
  `Evaluate` cannot know whether the slice it was handed is the whole set, and
  it does not measure the read it did not perform.
- `Degrade(reason)` is the one constructor for a swallowed failure, so every
  degradation has the same shape.
- `RenderBody` is a **no-op** when the signals are degraded and carry no
  findings: the filed body is then byte-identical to the one it would have
  had before this feature existed.
- `HasFindings` deliberately ignores `DerivesFrom`: it echoes what the filer
  declared, not anything grooming found, so a degraded filing that declared
  source refs still files a byte-identical body. When the section does render,
  a non-empty `DerivesFrom` adds a **Derives from** block between the
  duplicates and the epic suggestion, stating the items were excluded from the
  duplicate candidates.
- `ParseSourceRefs` accepts `#N` or `N` (`N > 0`, ASCII digits, trimmed),
  dedupes in first-seen order, and fails the WHOLE list with a nil result and a
  `*SourceRefError` naming the first malformed ref. A cross-repo
  `owner/repo#N` ref is malformed, not ignored: the window only scans the
  target repo, so honouring it would be a silent no-op.

## Why degradation, not fail-closed

Everywhere else in E54 a missing or unresolvable charter fails closed,
because a grooming run's entire output is charter-anchored ranking and an
unanchored ranking is worse than none.

Intake grooming is the deliberate exception, and the reason is written next
to the `DegradeReason` set in `intakegroom.go` rather than only here. It
rides on work-item **filing** — a load-bearing write path used by operator
follow-up filing (#1005), product-issue reporting (#1006), deferred review
concerns and refinement filing. Making a filing depend on grooming health
would convert an advisory enhancement into a new failure mode on a path whose
job is to record work reliably. So every failure is swallowed into a named
`DegradeReason`, the item is filed anyway, and the reason is reported rather
than hidden.

The reason set is closed (`DegradeReasons()`), so a degradation is always
attributable to a named cause rather than to an unexplained empty result.

## The budget is spent CONCURRENTLY by two independent reads

`DefaultDeadline` (3s) bounds the hook's own derived context, and the hook
spends it on two **independent** forge reads — the duplicate-candidate scan
and the charter read — running **concurrently** on that one context, joined
before either result is read.

That makes the deadline a **per-read bound, not a shared pool**. It used to be
a pool spent sequentially, scan first (#2827): on a real backlog the scan alone
cost most of the 3s, so the charter read inherited whatever was left and the
outcome flipped on ordinary latency variance — three degraded filings at
3001-3002ms against one scored filing at 2529ms. The hook's wall clock is now
`max(scan, charter)` rather than `scan + charter`, so neither read can starve
the other.

**`DefaultDeadline` was NOT raised, and that is the point.** With the reads
concurrent the constant already caps each read on its own, so buying the
charter read more time by nudging a constant would be paying latency on the
filing path for something the shape change already delivers.

**Concurrency does not rescue a scan that alone exceeds the whole budget.**
That filing still degrades. What changes is that the reason names the read that
actually failed instead of blaming the charter parser for a document that was
never fetched.

Each read goroutine carries its **own** deferred `recover()`. A `recover()` in
the frame that started a goroutine cannot catch that goroutine's panic — the
runtime terminates the program ([Go spec, "Handling
panics"](https://go.dev/ref/spec#Handling_panics)) — so the per-goroutine guard
is what keeps a panic on this load-bearing write path a swallowed degradation
rather than a process crash. The hook **joins** rather than abandoning a
goroutine at the deadline, so none outlives the frame and no abandoned result is
held: that is exactly the failure mode the alternative design was rejected for.

## The latency bound, stated honestly — and what concurrency changed

A context deadline does **not** preempt a callee that never consults the
context, so this bounds the read for a **cancellation-cooperative** reader — the
production path is one: `githubclient` builds every request with
`http.NewRequestWithContext`, and Go's HTTP client honours a context deadline,
so the real provider read returns at the deadline. A reader that blocks without
consulting `ctx` is not bounded by this mechanism, and a claim that it is would
be a claim about a fiction. The hook slice's wedged-reader test therefore uses a
ctx-respecting fake and tests the plumbing, which is what is actually there.

Running the reads concurrently **preserves** that bound for the candidate read
and **extends the same conditional exposure to the charter read** — an honest
new exposure path, not a preserved one. Under the sequential shape a scan that
exhausted the budget meant the charter read was never dialed at all, so a
non-cooperative charter reader could not block a filing it was never asked to
serve. It is now always dialed, so it can. That is an accepted consequence of
the change.

Because the bound is conditional on that production property, the property is
pinned behaviourally rather than asserted in prose, and for **both** seams:
`TestIntakeHook_ProductionReadPathCancelsInFlightAtDeadline` drives the real
`workmgmt/github` reader AND the real `repodoc` charter resolver through the real
`githubclient` against a hanging HTTP server and asserts the server's own
in-flight request is cancelled at the caller's deadline. An edit that stopped
threading the context anywhere in either chain reddens it.

## `Charter.Resolved`: a never-read charter is not an unparsable one

`Charter.Resolved` is true **only** when a charter document was actually
fetched and its content handed to `ParseRubricIDs`. Every early-return path in
the hook — undeclared, seam unwired, base-ref failure, credential-scope
failure, resolver failure — leaves it false.

Without it an empty `RubricIDs` had two indistinguishable causes, and both
surfaced as `charter_rubric_unparsed` with a gap blaming the parser for a
document that was never read. The flag splits them:

| `Charter` state | `Score.CharterGap` | `DegradeReason` from `Evaluate` |
|---|---|---|
| not resolved, a path declared | `the charter at <path> was not read, so no citation is available` | `charter_unresolved` |
| not resolved, no path declared | `no charter is declared for this repository, so no citation is available` | `charter_unresolved` |
| resolved, no rubric rows parsed | `no rubric lines could be parsed from <path>, so no citation is available` | `charter_rubric_unparsed` |

The hook may still **override** the reason with the more specific cause it
observed upstream (`charter_undeclared`, `seam_unwired`, `budget_exceeded`);
those name causes only the caller can see. The closed `DegradeReason` set is
unchanged — `charter_unresolved` was already a member.

`ScoreFiling` therefore takes the whole `Charter` rather than only its
`Rubric`: the gap has to name which of the three happened.

## Charter parsing: rubric rows and non-goal bullets

Both parsers read the same resolved document and share one line loop
(`parseCharterLines`): charter order is kept, the FIRST definition of a
duplicated id wins, and a document with no match returns the safe zero value
(`Len() == 0`, `Has` false for every id). Neither guesses an id the charter
does not declare.

| Parser | Matches only | Quote |
|---|---|---|
| `ParseRubricIDs` → `Rubric` | a §4 table row `\| **<id>** \| <line> \|` | the line cell |
| `ParseNonGoals` → `NonGoals` | a §3 bullet `- **N<k> — <statement>**` (em dash, en dash or hyphen, spaced; id `^N[0-9]+$`) | the bolded statement, without the prose after it |

The non-goal shape is anchored on the list dash AND the opening bold span, so
running prose that names `N3`, an inline bold span, a table row carrying an
N-id, and an unbolded bullet all fail to parse. It matches anywhere in the
document rather than under a §3 heading, so renumbering the section does not
silently empty it. `TestParseNonGoals_ParsesTheShippedCharter` pins N1..N8 of
this repository's `.fishhawk/charter.md` and fails (never skips) when the file
is unreadable.

`Charter.NonGoals` is set from the same content as `RubricIDs`, so the two can
never come from different charter revisions. Intake scoring ignores it; the
comms scan (#4014) renders it so an `n_drift` entry cites only a declared
non-goal.

The server reads the charter through ONE log-free core,
`(*Server).resolveCharterDocument` (`backend/internal/server/intake_hook.go`),
which returns `(Charter, DegradeReason, detail)` and never logs. The intake
hook calls it through the `intakeCharter` wrapper, which WARN-logs each
degradation as `intake groom degraded; work item filed without signals`
exactly as before the split; the comms scan calls the core directly and logs
under its own message, because a comms charter failure refuses the prompt
rather than filing anything. `TestResolveCharterDocument_LogFree` pins both
halves for every degrade branch.

## The recency approximation, and what it misses

The duplicate window is the newest `DefaultMaxScanned` items by **creation**
time, not by close time. An item filed long ago and closed yesterday can fall
outside the window, so a duplicate against it is missed. That limitation is
stated rather than hidden: `ScannedItems` and `WindowTruncated` ride on the
signals and are rendered into the advisory section, so a reader can tell a
bounded scan from an exhaustive one.

## Tuning

The thresholds are named constants with unit tests precisely because lexical
matching's false-positive rate is a tuning question, not a correctness one:

| constant | value | what it does |
|---|---|---|
| `ThresholdHigh` / `ThresholdMedium` / `ThresholdLow` | 0.60 / 0.45 / 0.30 | duplicate confidence bands; below `ThresholdLow` is not a candidate |
| `EpicThreshold` | 0.15 | epic suggestion floor — deliberately BELOW the duplicate floor, because an epic title is short and thematic while a child title is long and specific, so their overlap is structurally small even when the parentage is obvious |
| `typeLabelBonus` / `areaLabelBonus` | 0.05 / 0.10 | same-`type:` and same-`area:` label bonuses, capped at 1.0 |
| `MaxDuplicates` | 3 | a list a human reads |
| `DefaultMaxScanned` | 300 | ~three GraphQL pages |
| `DefaultDeadline` | 3s | the hook's budget, spent CONCURRENTLY by the two reads — so it is a per-read cap, not a pool; see above for why it was not raised |

Moving any of them touches no wiring.

## Marker format

```
<!-- fishhawk-intake:v1 {"duplicates":[...],"derives_from":[...],"score":{...},...} -->
```

`derives_from` is additive and `omitempty`, so the marker stays `v1` and a
marker written without it parses unchanged. A `fishhawkd` built before #3774
rejects a marker carrying it (unknown field), which fails closed (`ok=false`),
never a misparse.

One HTML comment, one line, a version token — mirroring the existing
`fishhawk-fingerprint` marker convention in
`backend/internal/workmgmt/github/feedback.go`. The payload's `<` and `>` are
`\u`-escaped, so a title containing `-->` cannot close the comment early and
spill the rest of the payload into the rendered body. `ParseBody` reads the
LAST marker in the body (so a body quoting an earlier one cannot shadow the
real one) and returns `ok=false` — never a partial value — for a missing,
unterminated, or undecodable marker.

Read-back acceptance is EXACT, not best-effort, because an issue body is
mutable user-editable input and this marker is what #2236 reads instead of
redoing the analysis. A marker is accepted only when its payload is exactly
one complete JSON object with no second value and no trailing non-whitespace
after it; it names every field the renderer always emits (`score`,
`degraded`, `scanned_items`, `window_truncated`, `duration_ms`) and no field
the shape does not declare; and the decoded `Signals` satisfies the
invariants every rendered `Signals` satisfies — a non-negative count,
duration and score value, `degraded` set together with a `degrade_reason`
from the closed set, `unscored` exactly when no citation survived (with the
gap stated), a rubric id and quote on every citation, at most
`MaxDuplicates` duplicates, and a 1-based tracker number, non-empty title,
`(0,1]` score and known confidence band on every duplicate and epic
suggestion, and a 1-based, unique number on every `derives_from` entry (a
title is NOT required even in-window — the window echoes the tracker's title
verbatim, and an untitled source is one the writer can emit). Anything short of that is rejected: a hand-edited `{}`, a
truncated candidate object or a payload with a second JSON value appended
returns `ok=false` rather than a partially-populated analysis a consumer
would trust.
