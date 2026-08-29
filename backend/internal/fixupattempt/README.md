# fixupattempt

Pure classifier behind the advisory `fixup_concern_unattempted` pre-review signal
(#2896): which concerns routed back to an implement fix-up pass name repository
files the pass left **entirely untouched**.

No I/O, no server types. Its two callers live in
`backend/internal/server/trace.go::runImplementReviews` (the review-time check)
and are unit-testable in isolation, like the sibling `fixupobligation`.

## Why it exists

A fix-up pass reports `succeeded` with no mechanical check that each routed
concern was even **attempted**. The verify gate certifies only that the tree
builds; the implement re-review is diff-only. So a silently dropped concern is
indistinguishable from an addressed one.

That is not hypothetical. Run `925addab` / PR #2895 routed two concerns. The pass
edited the file the LOW named, never opened the file the MEDIUM named, and the
stage reported clean. The drop was caught only because the operator diffed the
fix-up commit by hand, and the second (forced) pass exists only because of that
manual catch.

## What it decides

`Implicated(note, candidates)` resolves the repo-relative paths a routed
instruction NAMES, **anchored on a caller-supplied candidate set** (the approved
plan's `scope.files` UNION the paths the fix-up commit touched). Anchoring is
what stops the function minting a phantom path the pass could not have touched.

A note token resolves in exactly two ways:

- **EXACT** — the normalized token equals a candidate.
- **SUFFIX** — the reviewer omitted the repository prefix (a note citing
  `internal/server/README.md` for `backend/internal/server/README.md`). The match
  must fall on a `/` boundary, and a token that suffix-matches TWO OR MORE
  candidates is **ambiguous** and is discarded rather than guessed.

Tokenization strips the decorations reviewer prose puts around a path: a leading
`./`, trailing sentence punctuation, and a `path:LINE` (or `path:LINE-RANGE`)
citation, whose `:` is a token boundary rather than a rejection. Backticks,
quotes, parens and brackets are boundaries too.

Both rules are boundary-anchored on purpose. A note naming `docs/onboarding.md.bak`
or `xdocs/onboarding.md` must NOT implicate `docs/onboarding.md`, and a note
naming `board.go` must NOT implicate `site/dashboard.go` — a **spurious match
marks an untouched file as touched and MASKS a genuine drop**, which is the exact
failure this package exists to detect.

`Unattempted(routed, touched)` returns a `Finding` for each routed concern whose
implicated set is NON-EMPTY and DISJOINT from the touched set, plus a count of
the concerns it could not decide. A **partially** touched implicated set counts
as ATTEMPTED — the pass demonstrably opened one of the named files, which is all
this signal claims to detect.

`Untouched(paths, touched)` backs the UNATTRIBUTED half: paths named in the
routed instruction text as a whole (the operator's `reason` /
`operator_concern`) rather than inside one concern's own note.

## Three deliberate trades

**Untouched means NOT ATTEMPTED, never NOT ADDRESSED.** A concern can be
legitimately resolved by editing a different file than the reviewer named, or
legitimately declined, and a routed instruction sometimes names a file precisely
to say *do not touch it*. The surface therefore warns and asks the reviewer to
arbitrate. It never fails, re-opens, or re-budgets a pass.

**Undeterminable is a COUNT, not a per-concern finding.** A concern whose routed
text names no candidate path would otherwise fire on nearly every routine fix-up,
and a signal that fires always is a signal that is ignored. The residual is
honest and visible in the payload's `undeterminable_count`: a concern dropped
without its routed text naming a file is NOT caught. The caller emits the audit
record whenever that count is non-zero, so silence from the surface means
"checked and clean" and never "could not check".

**Position is captured at ROUTING time and never re-derived.** A Position
computed from the filtered finding set would label the surviving finding of two
id-less routed concerns "concern 1" when the concern actually dropped was the
second — sending an operator to inspect a concern that WAS attempted, which
spends their trust worse than no signal would. The same refusal-to-guess governs
the ambiguous-suffix rule.

## What the incident's real text proves

The reality check is pinned by `TestImplicated_RealIncidentNotes` against the
verbatim routed text of run `925addab` (concerns `f5c464c6` and `9955251a`):

- **Neither concern NOTE resolves a path.** The medium describes its target as
  "the repository's long-form security documentation"; the low names a test
  function and a Go identifier. A note-only derivation would have bucketed both
  as undeterminable and detected nothing — shipping inert.
- **The operator's routed REASON names both files.** Against it,
  `docs/onboarding.md` resolves and, with only
  `backend/internal/server/README.md` committed, surfaces as untouched. The
  incident IS detected.

That is why the caller resolves the shared routed text as well as each note, and
why a path from shared text is reported **unattributed** when several concerns
are routed: attributing it to one of them would be a guess.
