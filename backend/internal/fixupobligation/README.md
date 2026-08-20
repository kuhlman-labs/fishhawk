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

Entry selection is `resolveFixupConcerns`'s predicate verbatim: scan
newest-first, take the first entry bound to this stage with a **non-empty
concern set**. That identity is asserted directly rather than argued from
sequencing — `TestResolveFixupReportObligations_TwoTriggerEntries_BothSitesResolveNewest`
seeds two stage-bound trigger entries and pins which one both sites resolve, so
a later fork of the two sites fails loudly instead of reporting ids the agent's
prompt never named.

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
| Empty / whitespace `id` | dropped | `empty_id` |
| `status` not exactly `met` or `declined` | dropped | `unknown_status` |
| `met` with empty/whitespace `record` | dropped | `met_without_record` |
| `declined` with empty/whitespace `reason` | dropped | `declined_without_reason` |

Dropping is the **safe direction**: a dropped `met` leaves the obligation
undelivered and the signal fires. Admitting a malformed `met` would falsely
satisfy the obligation and silence the very signal this exists to raise.

## Evidence-only posture

The signal **never fails, re-opens, or re-budgets a pass**. The runner branch
never touches `res.OK` / `res.FailureCategory` / the budget; the backend branch
appends an advisory audit entry and sets a prompt field, nothing more. That
invariant is pinned in the firing direction as well as the absent one:
`TestRunImplementReviews_FixupReportingObligation_SignalDoesNotAlterOutcome`
runs both arms and asserts the review dispatch result, the stage status, and the
fix-up budget input (`countFixupPasses`) are identical.

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
