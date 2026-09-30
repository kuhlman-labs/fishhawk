# backend/internal/captain

The captain record for a repository (E76.2 / #3765), implementing ADR-083
(#3751) rules 2-5. The chain is the **sole authority**: there is no derived
table and no migration. The current captain, any pending handover offer and the
last captain before a vacancy are recomputed from global-chain entries on every
read. Nothing here is read by approval quorum or eligibility (ADR-083 rule 1).

## Files

| File | Holds |
|---|---|
| `captain.go` | The five categories, `Event` + `Payload()`, `ChainEntry`, `Record`, `HandoverOffer`, `State`, the pure fold `Derive`, `IdentityVerified` |
| `transition.go` | The five pure verbs `Offer`/`Withdraw`/`Accept`/`Relinquish`/`Claim`, the `PredicateOutcome` enum, one sentinel per refusal mode |
| `store.go` | `Store.Apply` (the atomic write path), `Store.Read`/`Entries` (the lock-free read path) |

## Events

Each verb appends exactly one run-less (global-chain, `run_id IS NULL`) entry to
the acting account's partition, keyed by `payload.repo`:

| Category | Written by | Payload keys beyond `repo`, `subject`, `identity_verified` |
|---|---|---|
| `captain_handover_offered` | `Offer` (sitting captain) | `successor`, `successor_identity_verified`; the handover brief: `brief_hash` + `brief_from_sequence` + `brief_to_sequence`, or `brief_unavailable` + `brief_unavailable_reason` |
| `captain_handover_withdrawn` | `Withdraw` (the offering captain) | `successor`, `offer_entry_hash` |
| `captain_assigned` | `Accept` (the named successor) | `offer_entry_hash`, `previous_captain` |
| `captain_relinquished` | `Relinquish` (sitting captain) | — |
| `captain_claimed` | `Claim` (fallback claim at a vacant seat) | `claim_verified`, `predicate_basis`, `previous_captain` (when one exists), `page_pending: true` |

`offer_entry_hash` is the `entry_hash` of the `captain_handover_offered` entry a
withdraw or accept resolves — the offer's identity. `subject` is always the
acting identity; the entry's `actor_subject` carries the same value.

**The handover brief on an offer (E76.4 / #3767).** `Params.Brief` /
`Event.Brief` (an `OfferBrief`) carry the brief the caller composed; `Offer`
copies it verbatim (a transition is pure — composition is the server's job,
done BEFORE `Store.Apply` because the decide callback runs under the advisory
lock). `Payload` emits the five `brief_*` keys ONLY on
`captain_handover_offered` (`TestEventPayload_BriefKeysOnlyOnOffer`), and
`Derive` surfaces them on `HandoverOffer.Brief`. `brief_hash` is the hash of
the canonical, unbounded brief (`handoverbrief.Hash`) with its window;
`brief_unavailable` + reason (and NO hash) records a brief that could not be
established at all — a section degradation still hashes. The brief is a
point-in-time composition: the hash commits to what was composed at offer
time; live sections may differ on a later read. The keys are additive: a
pre-E76.4 offer derives the zero `OfferBrief`
(`TestDerive_PreBriefOfferHasZeroBrief`), and pre-change code ignores them
(`encoding/json` drops unknown keys).

None is an issue-comment surface or a decision-bearing category (see
`docs/issue-comment-surfaces.md`). `page_pending` RECORDS the obligation to
page the previous captain; delivery is E77.6 #3740 / E60.3 #2292.

## Consumers: the current-captain read contract (E76.3 / #3766)

Every surface that is ENRICHED by the captain (ADR-083 rule 6) reads it through
ONE server helper, `(*server.Server).currentCaptain(ctx, accountID, repo)
(subject, identityVerified, basis)` in `backend/internal/server/captain.go`,
which wraps `Store.Read` and folds the outcome into a trichotomy every consumer
branches on identically:

| `basis` | Meaning | Consumer obligation |
|---|---|---|
| `captain` | a seat is held; `subject` / `identityVerified` are the derived `Record`'s | use it |
| `vacant` | the read succeeded and `State.Current` is nil | STATE the vacancy; do not imply it |
| `unavailable` | `CaptainStore` not wired, or the read failed (logged at WARN) | degrade to the pre-E76.3 behaviour; never report it as a vacancy |

It never returns an error: every consumer is a best-effort enrichment that must
not fail its request. `accountID` is EXPLICIT: a request handler passes
`identityAccountID(ctx)`; the issue-comment notifier passes the RUN's account
(`run.Run.AccountID`, through `issueCommentCaptainResolver`, which maps an
unparseable non-empty id to `unavailable` rather than the untenanted partition)
because it fires from transition hooks whose ctx carries no request identity.
The result is never an input to approval quorum or eligibility (ADR-083 rule 1).
The notifier consumes it through the narrow `issuecomment.CaptainResolver` seam
(nil = today's surfaces byte-for-byte); see `docs/issue-comment-surfaces.md`.
Later consumers — the push channel's page delivery (E60.3 #2292) and crew-message
paging (E77.6 #3740) — call the same helper rather than reading the store.

## Derive

`Derive(repo, entries)` is a pure fold in ascending `sequence` order:

- `offered` sets/replaces the pending offer (latest wins).
- `withdrawn` clears the pending offer — only when its `offer_entry_hash` names
  the pending one; otherwise the entry is skipped.
- `assigned` sets the captain (basis `assigned`, `ClaimVerified` nil), clears the
  offer, and sets `LastCaptain` to the outgoing captain.
- `relinquished` vacates the seat, clears the offer, and sets `LastCaptain` to
  the relinquisher.
- `claimed` sets the captain (basis `claimed`, `ClaimVerified` from the payload)
  and clears the offer.

An entry whose payload does not decode, names another repo, has no subject, or
is of an unknown category is **skipped and counted** (`SkippedEntries`), never
guessed at — the decisionindex posture. `LastCaptain` is what makes
`previous_captain` derivable on a claim on every vacancy path, including
relinquish-then-claim.

## Two separate verification flags

- `identity_verified` — the subject is provider-qualified (`github:` /
  `gitlab:` prefix, `IdentityVerified`). A static token subject such as
  `brett@local-mcp` carries `false`, on the captain, the pending offer's
  successor, and every entry payload.
- `claim_verified` — set ONLY on a claimed seat: `true` when a non-trivial repo
  predicate was checked and satisfied, `false` when the predicate was
  positively trivial. `nil` for an assigned (handed-over) seat.

They never stand in for one another.

## Transitions and refusals

Every verb calls `guardActor` first — the ONE authoritative agent/delegated
refusal (`ErrAgentIdentity`); callers compute `ActorIsAgent` /
`ActorIsDelegated` and pass them in but do not refuse independently. Every
refusal returns its own sentinel and the zero `Event`:

| Sentinel | Verb(s) |
|---|---|
| `ErrActorRequired` | all (empty actor) |
| `ErrAgentIdentity` | all |
| `ErrNoCaptain` | offer, relinquish (vacant) |
| `ErrNotCaptain` | offer, relinquish (actor is not the captain) |
| `ErrSuccessorRequired`, `ErrSelfHandover` | offer |
| `ErrNoOffer` | accept, withdraw |
| `ErrNotOfferer` | withdraw — the SINGLE withdraw-authorization check (only the sitting captain can offer, so offerer and captain coincide) |
| `ErrOfferSuccessorMismatch` | accept |
| `ErrCaptainExists` | claim (seat held) |
| `ErrPredicateRejected`, `ErrPredicateUndeterminable`, `ErrPredicateOutcomeInvalid` | claim |

### Claim predicate trichotomy

`Claim` takes a resolved `PredicateOutcome` (the server's `classifyRepoPredicate`
produces it):

| Outcome | Result |
|---|---|
| `PredicateNonTrivialSatisfied` | admitted, `claim_verified: true` |
| `PredicateTrivial` (spec parsed, declares no `min_permission` and no `member_of`) | admitted, `claim_verified: false` |
| `PredicateNonTrivialRejected` | `ErrPredicateRejected` |
| `PredicateUndeterminable` (no run, no cached spec, unparseable spec, identity provider unavailable/unconfigured) | `ErrPredicateUndeterminable` — fails CLOSED; an unreadable predicate is never trivial |
| any other value (incl. the zero value) | `ErrPredicateOutcomeInvalid` |

## Atomicity (`Store.Apply`)

`Apply(ctx, ApplyParams, decide)` runs entirely inside ONE
`postgres.WithTenant` transaction:

1. `pg_advisory_xact_lock(captainLockKey(account, repo))` — the first 8 bytes of
   sha256 over `"fishhawk:captain:"` + account UUID (or an untenanted marker) +
   `0x00` + repo, disjoint from audit's `globalChainLockKey` and refinement's
   filing locks.
2. Read the repo's captain entries inside the tx; `Derive`.
3. `decide(state)` — the transition — on state that cannot change before the
   append. A decide error returns and the tx ROLLS BACK: nothing is appended.
4. `audit.AppendGlobalChainedTx` in the SAME tx.

Lock order is fixed — captain key, then the global-chain partition key inside
`AppendGlobalChainedTx` — and no other path takes the captain key, so no
deadlock cycle exists. Both locks are xact-scoped. Consequence: two vacant-seat
claims cannot both succeed, and an accept cannot commit after its offer was
withdrawn (or vice versa).

Pinned by `TestStore_ConcurrentClaimsAtVacantSeat_ExactlyOneWins` and
`TestStore_AcceptRacesWithdraw_NeverAssignsAWithdrawnOffer`. Both use the
unexported test-only `afterRead` seam (called right after the state is read and
derived) to park every caller on a barrier: with the read correctly under the
lock only one caller is ever past the read (the barrier releases by timeout and
the test asserts `maxIn == 1`); with the read moved outside the lock every
caller holds the same stale state, so the mutation commits N claims and the
tests go red on every run, not by scheduling luck.

## Read path and its cost

`Read` / `Entries` are lock-free:

```sql
SELECT sequence, entry_hash, category, ts, payload FROM audit_entries
WHERE run_id IS NULL AND category = ANY($captain_categories)
  AND payload->>'repo' = $repo AND account_id IS NOT DISTINCT FROM $acct
ORDER BY sequence ASC
```

Only `category = ANY(...)` is index-served (`audit_entries_category_idx`,
migration 0002). The repo predicate is a **JSON filter** (`payload->>'repo'`)
applied to the category-indexed rows, **not index-served**; so is the
`account_id` narrowing. The scan therefore touches every captain-category row
on the table, across repos and accounts. Expected volume: handovers and claims
are human-rate events — on the order of a few per repository per month, so
roughly 50–100 rows per repository per year, and low thousands of rows per
account per year for an account with tens of repositories. At that volume the
filter is cheap; a `(category, (payload->>'repo'))` expression index is the
remedy if a deployment's captain rows ever reach the tens of thousands.

The whole history is folded on every read (no limit), because `Derive` needs
every entry to be correct.
