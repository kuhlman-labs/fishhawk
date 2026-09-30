# delegationconfirm

Delegation confirmation on handover (E76.5 / #3768, ADR-083 #3751 rule 7):
the incoming captain confirms or proposes to lower each workflow's delegation.
Nothing here can raise delegation.

## Chain entries

Two run-less (global-chain) categories, registered in
`audit.KnownCategories`, keyed by `payload.repo` + `payload.workflow`. Neither
is an issue-comment surface nor decision-bearing.

| Category | Payload |
|---|---|
| `delegation_confirmed` | `repo, workflow, subject, identity_verified, content_hash, workflow_sha` |
| `delegation_lower_proposed` | `repo, workflow, subject, identity_verified, proposed_tier?, proposed_escalation?{paths, max_autonomy}, reason, filed_ref` |

There is no derived table and no migration: the chain is the sole authority.

## The fold (`Derive`)

Pure, ascending-sequence fold over the repo's captain entries
(`captain.Categories()`) and these two categories together:

- **Reset at every seat change.** A `captain_assigned` or `captain_claimed`
  that seats a captain clears every confirmation and lower proposal. A claim is
  a seat change in substance, so it resets too (the stricter reading).
- **Captain-in-force rule.** An entry counts only if its `subject` is the
  captain in force at that point in chain order (derived by `captain.Derive`
  over the captain entries before it). Anything else — an outgoing captain's
  confirmation landing after a newer `captain_assigned`, a confirmation while
  the seat is vacant — is IGNORED and counted in `IgnoredEntries`.
- **Skip, never guess.** An undecodable payload, another repo, or a missing
  workflow/subject is skipped and counted in `SkippedEntries`.
- Latest counted confirmation per workflow wins.

## Verdicts

The workflow INVENTORY is the caller's: the delegation view's workflow ids at
the relevant ref, so a never-confirmed workflow is reported, not omitted.

- `Statuses(st, ids)` — CHAIN-ONLY: `confirmed`; `unconfirmed` reason
  `handover` (no counted confirmation since the last seat change); or
  `no_captain` (no seat ever taken — no handover awaits confirmation, so it is
  not listed by `Unconfirmed`).
- `ApplyCurrentHashes(statuses, current)` — the staleness half: a confirmed
  workflow whose recorded `content_hash` differs from its current
  `delegationview` hash becomes `unconfirmed` reason `hash_stale`. Kept separate
  so a surface with no spec read (the captain hand-off surface) can report the
  chain-only verdict without claiming hash-currency.
- A lower proposal does NOT confirm: the delegation in force is unchanged until
  the filed human-authored edit lands and is itself confirmed.

## Transitions and validation

- `GuardActor` — the single agent / run-bound / delegated refusal
  (`ErrAgentIdentity`); the server calls it before any spec read or filing.
- `CheckCaptain` — `ErrNoCaptain` (vacant) / `ErrNotCaptain`.
- `Confirm`, `ProposeLower` — build the event after `CheckCaptain`.
- `ValidateLower(currentTier, req)` — `ErrReasonRequired`,
  `ErrNothingProposed`, `ErrEscalationPathsRequired`, `ErrRaiseRefused`. A
  proposed tier must rank STRICTLY below the current tier; a proposed
  escalation's `max_autonomy` must be at or below the effective proposed tier
  (and strictly below the current tier when no tier is proposed); an
  undeclared/unknown current tier admits nothing (fail closed).
- `Actions()` is the closed set `{read, confirm, lower}`. There is no raise.

## Store

`Store.Append` runs lock → read → derive → decide → append in ONE
`postgres.WithTenant` transaction holding the **captain record's** advisory
lock (`lockKey` reproduces `captain.captainLockKey` byte-for-byte;
`TestAppend_SharesCaptainLock` proves it by blocking a real
`captain.Store.Apply`). That is what makes the captain check and the append one
serialized path: an outgoing captain's confirm either commits before the
handover (and the fold's reset voids it) or is refused after it. Lock order
matches captain's (captain key, then the global-chain key inside
`AppendGlobalChainedTx`), so no deadlock cycle exists. A duplicate confirmation
is benign — latest wins. `Store.Read` is the lock-free GET path.

Served by `backend/internal/server/delegation_confirm.go`; contract in
`backend/internal/server/README.md` § "Delegation confirmation".
