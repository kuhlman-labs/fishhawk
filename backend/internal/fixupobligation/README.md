# fixupobligation

Pure, dependency-free classifier + join for the REPORTING obligations routed
with an implement-stage fix-up pass (#2737 / E67.66).

## Why this exists

A fix-up pass can be routed an instruction that asks the agent to **record**
something rather than to change code — "record the per-deletion counterfactual
results in the PR body's `## Notes`". Before this package, such an instruction
could be declined with **nothing marking the omission**:

- the committed-tree verify gate certifies the tree, not the PR body;
- the implement review is diff-only, so a missing paragraph is invisible;
- the concern re-opens each round with unchanged text, so a decline looks
  identical to a not-yet-done.

The root cause is structural, not merely unreported: the slim fix-up prompt
deliberately renders **no PR-description block** — the pull request already
exists and a fix-up must never clobber its title/body
(`backend/internal/prompt/prompt.go`, `FixupCommitMessagePath`'s doc comment and
`writeFixupCommitMessage`'s rendered "Do NOT write a PR description here" rule).
So a routed PR-body reporting obligation had **no sanctioned transport at all**
on a fix-up pass.

This package is the pure half of the fix. It gives the obligation a structured
transport (the existing fix-up self-report sidecar, #1210) and a deterministic
undelivered signal (an advisory pre-review audit entry + a distinct
gate-evidence block, modeled on `operator_scope_path_undelivered`, #1407).

## Where it is called

Both call sites read the **same** `stage_fixup_triggered` audit entry, via one
shared resolver, so the declared set is identical at both by construction — no
persisted duplicate to drift:

| Site | File | Role |
|---|---|---|
| Fix-up prompt render | `backend/internal/server/prompt.go::resolveFixupReportObligations` → `prompt.writeFixupReportObligations` | names each obligation to the agent by id, routes the record into the self-report sidecar |
| Implement-review re-derivation | `backend/internal/server/trace.go::runImplementReviews` (adjacent to the #1407 block) | re-derives the declared set, subtracts the agent's `met` reports, signals the remainder |

### Entry identity: the serve-time anchor

"Same entry" cannot rest on a shared *predicate*, because the two sites are
separated in time. The prompt site selects `resolveFixupConcerns`'s predicate
verbatim — scan newest-first, take the first entry bound to this stage with a
**non-empty concern set**. If the review re-ran that predicate and a **second
fix-up were triggered for the same stage in between**, the review would derive
its declared set from an entry the agent's prompt never named: `ob-N` ids bound
to different text, so a spurious "unreported", or a real one missed. Either
outcome discredits the signal, which is worse than not having it.

So the serve **pins** its entry and the review **resolves that exact entry**:

1. `handleGetStagePrompt` (the runner-facing serve — *not* the SPA
   `prompt-render` preview, which stays a pure read) resolves the newest entry,
   renders the block, then appends a `fixup_report_obligations_declared` audit
   entry via `recordFixupReportObligationsDeclared`, carrying that entry's audit
   `Sequence` (`{trigger_sequence, obligation_ids}`).
2. `runImplementReviews` reads the newest stage-bound anchor
   (`resolveFixupReportObligationAnchor`) and passes its `trigger_sequence` to
   `resolveFixupReportObligations`, which then matches **only** the entry with
   that exact `Sequence`.

A trigger entry appended after the serve has no anchor of its own until *its*
prompt is served, so it cannot capture an in-flight review. The anchor stores
only the pointer plus the ids for observability — the review re-derives the
authoritative set from the pinned entry, so there is still no persisted duplicate
that can drift.

**No anchor → no signal.** A stage that never served an obligation-bearing fix-up
prompt, or whose anchor append failed, emits nothing rather than falling back to
a newest-first read. That is the fail-safe direction for an evidence-only
surface.

Pinned by `TestResolveFixupReportObligations_NewerTriggerAppendedAfterServe_ReviewJoinsTheServedEntry`
(the intervening-append path, driven in real order: serve → append → review),
`TestResolveFixupReportObligations_SecondServeRepinsTheAnchor` (the anchor tracks
the serve rather than freezing on the oldest entry),
`TestRunImplementReviews_FixupObligationWithoutAnchor_NoSignal`, and
`TestGetStagePrompt_Implement_FixupReportObligations_ServeAnchorsTheTriggerEntry`
(the serve writes it; the preview does not).

## Classifier grammar

Deliberately **narrow and conjunctive**. `IsObligation(text)` is true only when
the case-folded text contains **both**:

| Half | Terms |
|---|---|
| Recording verb | `record`, `report`, `document`, `attest`, `attestation`, `write up`, `list out` |
| Report-surface noun | `pr body`, `pull request body`, `pull-request body`, `pr description`, `pull request description`, `pull-request description`, `## notes`, `notes section`, `run log` |

The narrowness is the anti-noise property: a signal that fires on every pass is
one operators learn to ignore, which is the failure mode this change must not
create. A routine concern ("guard the nil pool in the retry path") matches
neither half. "Record the retry count in a metric" matches only the verb and is
correctly rejected.

This is a lexical, English-only heuristic over operator-authored text. A
reporting obligation phrased without any listed term is a **false negative** —
which is today's exact behavior, so nothing regresses when it misses. A concern
that mentions the PR body in passing while also using a recording verb can
produce a **false positive** advisory entry; that entry never fails or
re-budgets anything.

## ID assignment

`Detect` assigns `ob-1`..`ob-N` in a **fixed kind order** that does not depend
on the caller's interleaving: every `concern` in payload order, then
`operator_concern`, then `reason`. An `operator_concern` whose trimmed text
already appeared as a concern note is **skipped** — since #2623 the free-text
`operator_concern` is also minted as a durable concern, so it arrives on both
channels and would otherwise draw two ids for one instruction.

Every stored excerpt is capped at `MaxExcerptBytes` (400) with a truncation
marker, cut on a rune boundary, so a 4000-byte `operator_concern` cannot swamp
the prompt, the gate-evidence payload, or the audit entry.

## Fail-closed drop table (runner side)

The agent's per-obligation reports arrive in the `obligations` array of the
fix-up self-report sidecar. `runner/cmd/fishhawk-runner/main.go::loadFixupSelfReport`
validates them. The **whole-sidecar** rules are unchanged and dominate:

| Condition | Result | Log |
|---|---|---|
| Absent / unreadable sidecar | zero result, no reports | (silent) |
| Malformed JSON | whole sidecar discarded | `fixup_selfreport_invalid` |
| `run_id`/`stage_id` mismatch | whole sidecar discarded | `fixup_selfreport_stale` |
| Unrecognized `verify_status` | claim zeroed, **obligations still parsed** | `fixup_selfreport_status_ignored` |

Then, **per entry** (`validateFixupObligationReports`):

| Condition | Result | `reason` on `fixup_obligation_report_dropped` |
|---|---|---|
| `id` not exactly the `ob-N` shape (`validFixupObligationID`) | dropped | `invalid_id` |
| `status` not exactly `met` or `declined` | dropped | `unknown_status` |
| `met` with empty/whitespace `record` | dropped | `met_without_record` |
| `declined` with empty/whitespace `reason` | dropped | `declined_without_reason` |

Dropping is the **safe direction**: a dropped `met` leaves the obligation
undelivered and the signal fires. Admitting a malformed `met` would falsely
satisfy the obligation and silence the very signal this exists to raise.

The surviving entry is reduced to `{id, status}`. The `record`/`reason` text is
**required but not retained** — see the trust boundary below. The drop log
echoes neither field verbatim: an out-of-shape id or status logs `<invalid>`.

## Trust boundary on the routed instruction excerpt

An obligation's `Text` is trusted **only when it is operator-authored**. One of
the three routed channels — the concern note — can carry an *acceptance-derived*
concern: free text an automated acceptance validator wrote while driving the
change against a running instance, i.e. attacker-influenceable content (ADR-050 /
E31.8 / #1613). The classifier will happily detect a "reporting obligation"
inside such a note, and the excerpt is a **mirror** of the very bytes
`writeFixupConcerns` already quarantines — so rendering the mirror inline under
the binding fix-up framing would be a second, unquarantined path for the same
text, at both the agent prompt and the reviewer prompt.

`Source.Untrusted` / `Obligation.Untrusted` therefore carries the concern's trust
provenance end to end. `resolveFixupReportObligations` sets it on the SAME
predicate `resolveFixupConcerns` uses for `prompt.FixupConcern.AcceptanceDerived`
(`Concern.Provenance == acceptance`); `Detect` carries it onto the obligation
(the `operator_concern` dedupe retains the surviving concern's flag, so the
dedupe can only ever keep the more-quarantined of the two); the trace handler
copies it onto `prompt.GateFixupReportingObligation` and onto the audit detail's
`untrusted` field. `operator_concern` and `reason` are operator-authored by
construction and are never marked.

| Layer | Control | Pinned by |
|---|---|---|
| Provenance carry-through | `Source.Untrusted` → `Obligation.Untrusted` → prompt/gate mirrors + audit `untrusted` | `TestDetect_CarriesUntrustedProvenance`, `TestResolveFixupReportObligations_AcceptanceDerivedConcernIsUntrusted` |
| Fix-up agent render | `writeUntrustedObligationExcerpts` — the id/source line stays trusted, the excerpt goes through `sanitizeUntrustedComment` inside a `<<<BEGIN/END UNTRUSTED OBLIGATION TEXT>>>` envelope, capped at `maxFixupObligationTextBytes` | `TestBuild_ImplementFixup_ReportObligations_UntrustedTextQuarantined` (+ `_TrustedTextStillInline`) |
| Reviewer render | the same helper, called from `writeGateEvidence`'s undelivered block | `TestBuild_ImplementReview_GateEvidence_UntrustedObligationTextQuarantined` (+ `_TrustedTextStillInline`), `TestImplementReview_AcceptanceDerivedObligation_TextQuarantined` (end to end) |

An operator-authored obligation renders **byte-identically to before** at both
sites: the partition is on the flag alone.

## The agent's free text does not cross the upload boundary

The fix-up agent executes arbitrary repository commands, so it controls every
byte of the `record`/`reason` it writes. An earlier shape of this change carried
that text over the trace bundle into the implement reviewer's prompt, quarantined
inside a `<<<BEGIN/END UNTRUSTED AGENT DECLINE REASON>>>` envelope with its
structure flattened at upload.

**That was the wrong control.** Quarantining and flattening bound what the text
can *impersonate*; they do nothing about what it can *carry*. The field remained
an arbitrary channel for repository content — a secret, a private file — from the
command-running agent to the reviewer, *without ever appearing in the committed
diff*. The channel is therefore removed rather than hardened:

| Layer | What crosses | Pinned by |
|---|---|---|
| Sidecar → runner | `{id, status, record, reason}`; text is validated (`met` needs a record, `declined` needs a reason) and then **discarded** | `TestLoadFixupSelfReport_ObligationTextNeverLeavesTheRunner`, `TestLoadFixupSelfReport_ObligationsHappyPath` |
| Runner → backend (gate evidence) | `{id, status}` only — `fixupReportingObligationEvidence` / `bundle.FixupReportingObligationEvidence` have no text field | `TestComposeGateEvidence_FixupReportingObligationsWireShape`, `TestExtractGateEvidence_FixupReportingObligationCarriesNoAgentText` |
| Backend → reviewer prompt | the *operator's* instruction excerpt only; the block states plainly that the agent's stated reason is not carried | `TestBuild_ImplementReview_GateEvidence_FixupObligation_NoAgentTextChannel` |
| Audit payload | `{id, source, status, text_excerpt, untrusted}` — the excerpt is the operator's instruction | `TestImplementReview_FixupReportingObligationUndelivered_AppendsAuditEntryAndRendersEvidence` |

The one agent-authored value that must still cross is the **join key**, and it is
constrained to a closed shape (`validFixupObligationID`: `ob-N`, no leading zero,
≤ 4 digits) rather than merely checked non-empty — an unconstrained id string
would reinstate exactly the channel the text fields were removed to close.

What is lost: an operator reading the gate no longer sees *why* the agent
declined. What is kept: they see *that* it declined, against which operator
instruction. That is the trade the evidence-only posture can afford; an egress
channel is not.

## Evidence-only posture

The signal **never fails, re-opens, or re-budgets a pass**. The runner branch
never touches `res.OK` / `res.FailureCategory` / the budget; the backend branch
appends an advisory audit entry and sets a prompt field, nothing more. That
invariant is pinned in the firing direction as well as the absent one:
`TestRunImplementReviews_FixupReportingObligation_SignalDoesNotAlterOutcome`
runs both arms and asserts the review dispatch result, the stage status, and the
fix-up budget input (`countFixupPasses`) are identical.

The RUNNER half of the same invariant needed a different vehicle. Running the
prescribed counterfactual — adding `res.OK = false` to the obligation branch —
left the whole runner package GREEN: `run()` has no seam for driving that branch
with a self-report sidecar present, so the claim was unpinned exactly where it
mattered. `TestFixupObligationsBranch_NeverTouchesStageOutcome` closes it
structurally instead: it parses `main.go` and fails if the branch body assigns
`res.OK` or `res.FailureCategory` at all. It goes RED under that same mutation.

This is deliberate. A signal that could fail or re-budget a pass would make
operators route *fewer* reporting obligations, not more — and it would reopen
the #1150 budget-wedge family.

## Known weakness

An agent that writes a fabricated `met` record satisfies the signal without
doing the work. An attestation about one's own work is weak evidence **by
construction**. This change makes the *absence* visible and puts the claimed
text in front of the reviewer and the operator; it does not make the claim
trustworthy. Closing that gap needs harness-captured evidence rather than a
self-report — named as the real closure in #2737 and left out of scope here.

## Wire contract

`fixup_reporting_obligations` crosses the runner↔backend module seam with no
shared type. The json tags on
`runner/cmd/fishhawk-runner/gateevidence.go::fixupReportingObligationEvidence`
and `backend/internal/bundle/bundle.go::FixupReportingObligationEvidence` MUST
stay identical — a one-sided edit silently **disables** the signal. Both sides
are pinned against one shared literal JSON fixture
(`TestComposeGateEvidence_FixupReportingObligationsWireShape` in the runner,
`TestExtractGateEvidence_DecodesFixupReportingObligations` and
`TestGateEvidenceForReview_DecodesFixupReportingObligations` in the backend).
