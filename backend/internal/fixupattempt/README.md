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

A resolved candidate is then kept only if it has at least one mention **not
introduced by an explicit negative instruction**. Routed text names files to
FORBID them as often as to require them — the reference incident's own routed
reason says *"Do not touch onboarding.go or either test file's logic"* — and
reporting such a path as untouched accuses the agent of dropping work precisely
when it obeyed. `prohibitiveMarkers` is a small imperative set (`do not`, `don't`,
`must not`, `should not`, `never`, `avoid`, `no need to`, `refrain from`,
`no change`, `leave unchanged`, …) matched against the clause text **preceding**
the mention. Two details are load-bearing:

- **Before-only, within the clause.** `Fix docs/onboarding.md but do not touch
  onboarding.go` must keep the first path and drop the second. Scanning the whole
  clause would suppress both and mask the very drop this package detects.
- **A clause boundary is a newline, a `;`, or `.`/`!`/`?` FOLLOWED BY
  WHITESPACE.** A bare `.` is a path byte, so an unconditional dot boundary would
  cut the clause at the dot inside a preceding filename and lose the `do not` for
  the path after it.

Over-matching is the safe direction here: a wrongly suppressed mention only
weakens an advisory signal, while a wrongly surfaced one puts a correctly-untouched
file into a durable operator-facing record.

`Unattempted(routed, touched)` returns a `Finding` for each routed concern whose
implicated set is NON-EMPTY and DISJOINT from the touched set, plus a count of
the concerns it could not decide. A **partially** touched implicated set counts
as ATTEMPTED — the pass demonstrably opened one of the named files, which is all
this signal claims to detect.

`Untouched(paths, touched)` backs the SHARED-TEXT half: paths the routed
instruction text as a whole (the operator's `reason` / `operator_concern`)
MENTIONED, rather than paths named inside one concern's own note. The caller
reports these as `mentioned_untouched_files` — a name chosen to claim exactly
what is provable and nothing more (see the fourth trade below).

## Four deliberate trades

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

**The shared-text half claims a MENTION, not an obligation.** Suppressing
explicit prohibitions is tractable; separating a CITATION from a requirement is
not. `"the RED landed at onboarding_test.go:564"` carries no negative marker and
no lexical rule distinguishes it from `"fix onboarding_test.go"`. Rather than
pretend to a precision it does not have, the surface CLAIMS LESS: the payload
member is `mentioned_untouched_files` and the reviewer prompt says *"MENTIONED in
the routed instructions as a whole … and NOT touched"*, never *"evidence of an
unattempted concern"*. The durable audit record an operator reads later, without
the prompt's framing, therefore states only what is true. `TestImplicated`'s
`a bare citation mention is NOT suppressed (known residual)` case pins that
residual so it stays visible rather than drifting.

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
- **Two of the four candidates the reason mentions are suppressed.** Pass 1's
  reason says *"Do not touch onboarding.go or either test file's logic"* and
  *"The implementation and tests are correct and need NO change … at
  onboarding_test.go:564"*, so the pass-1 resolution is exactly
  `[backend/internal/server/README.md, docs/onboarding.md]` and the reported
  untouched list is exactly `[docs/onboarding.md]`. Both lists are pinned in
  FULL, not searched for the true positive: a test that checks only the true
  positive cannot detect the signal growing new false positives.
- **The residual is visible in pass 2.** That reason suppresses `onboarding.go`
  the same way but keeps `onboarding_test.go`, which it cites as evidence with no
  negative marker in front of it. `TestImplicated_RealIncidentNotes` pins that
  list in full too.

That is why the caller resolves the shared routed text as well as each note, and
why a path from shared text is reported **unattributed** when several concerns
are routed: attributing it to one of them would be a guess.
