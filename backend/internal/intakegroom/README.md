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

## The latency bound, stated honestly

`DefaultDeadline` (3s) bounds the hook's own derived context. A context
deadline does **not** preempt a callee that never consults the context, so
this bounds the read for a **cancellation-cooperative** reader — which the
production path is: `githubclient` builds every request with
`http.NewRequestWithContext`, and Go's HTTP client honours a context
deadline, so the real provider read returns at the deadline. A reader that
blocks without consulting `ctx` is not bounded by this mechanism, and a claim
that it is would be a claim about a fiction. The hook slice's wedged-reader
test therefore uses a ctx-respecting fake and tests the plumbing, which is
what is actually there.

Because the bound is conditional on that production property, the property is
pinned behaviourally rather than asserted in prose:
`TestIntakeHook_ProductionReadPathCancelsInFlightAtDeadline` drives the real
`workmgmt/github` reader through the real `githubclient` against a hanging HTTP
server and asserts the server's own in-flight request is cancelled at the
caller's deadline. An edit that stopped threading the context anywhere in that
chain reddens it.

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
| `DefaultDeadline` | 3s | see above |

Moving any of them touches no wiring.

## Marker format

```
<!-- fishhawk-intake:v1 {"duplicates":[...],"score":{...},...} -->
```

One HTML comment, one line, a version token — mirroring the existing
`fishhawk-fingerprint` marker convention in
`backend/internal/workmgmt/github/feedback.go`. The payload's `<` and `>` are
`\u`-escaped, so a title containing `-->` cannot close the comment early and
spill the rest of the payload into the rendered body. `ParseBody` reads the
LAST marker in the body (so a body quoting an earlier one cannot shadow the
real one) and returns `ok=false` — never a partial value — for a missing,
unterminated, or undecodable marker.
