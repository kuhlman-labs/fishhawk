// Package mergequeue is the persisted per-(repository, base ref) held-pass
// queue behind ADR-092 D1 (#4200): at most ONE merge-candidate verify pass is
// live per base, and every other eligible pass is held FIFO by eligibility
// time, consuming no runner slot. Every mutation is one transaction under a
// per-base advisory lock, scoped by account through runs.account_id. ADR-093's
// merge queue reuses the rows, FIFO order and lock order unchanged, adding
// only the operator's merge intent (merge_intent_at, 0107), its recorded
// audit sequence and the admission epoch (0108), the per-run merge-dispatch
// ledger (0108) and the line-turn read. ADR-112 R3 adds the yield marker and
// the persisted approval-wait anchor (0109): a marked held entry is skipped by
// AdmitNext until ClearYield clears it. ADR-093 slice 3 adds the operator
// promote's order_key (0111) and the ejection/re-entry reads. docs/adr/112
// Amendment A3 adds the yield revision, the row-4 flag, the per-epoch
// merge-gate window and its hold budget, and the approval-wait open bit
// (0115). ADR-112 R3's withdraw-intent verb (#4685) settles a yielded entry
// dropped intent_withdrawn (IntentWithdrawer). README.md is the contract.
package mergequeue

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// State is a merge_candidate_queue_entries.state value.
type State string

// Entry states. Held and live are ACTIVE; the rest are TERMINAL.
const (
	StateHeld    State = "held"
	StateLive    State = "live"
	StateMerged  State = "merged"
	StateEjected State = "ejected"
	StateDropped State = "dropped"
)

// Active reports whether s is held or live.
func (s State) Active() bool { return s == StateHeld || s == StateLive }

// Terminal reports whether s is merged, ejected or dropped.
func (s State) Terminal() bool {
	return s == StateMerged || s == StateEjected || s == StateDropped
}

// Phase is a live entry's progress: the LAST RECORDED step of its head-of-line
// cycle, not a liveness signal (a process that died mid-step leaves the phase
// it last wrote; ADR-093 D8 resumes from it). The column has no CHECK: these
// are the phases ADR-092 writes, and ADR-093 adds its own without a migration.
type Phase string

// ADR-092 phases.
const (
	// PhaseAdmitted is stamped by AdmitNext: the admitting pump owns the
	// admission work (advance, re-anchor, pass start) until it moves on.
	PhaseAdmitted   Phase = "admitted"
	PhaseVerifying  Phase = "verifying"
	PhasePassed     Phase = "passed"
	PhaseUnverified Phase = "unverified"
)

// ADR-093 D8 phases (#4298): the head-of-line steps a restart resumes from.
const (
	// PhaseAdvancing is stamped by the admission right before its
	// behind-probe and base advance. Like PhaseAdmitted it is re-claimed
	// (AdmissionStaleAfter, and Resumer.AdmitNextReclaiming's boot instant).
	PhaseAdvancing Phase = "advancing"
	// PhaseMerging is stamped by merge-on-pass in-lock, after its verdict row
	// and BEFORE its merge-dispatch claim: the forge merge may be in flight.
	PhaseMerging Phase = "merging"
	// PhaseSettling is stamped after the forge accepted the merge, before the
	// merged settle.
	PhaseSettling Phase = "settling"
)

// ReasonBaseChanged is the drop reason Enqueue records when a run with an
// active entry re-enters on a different base ref. Every other eject/drop
// reason is the caller's, except ReasonIntentWithdrawn (README.md §
// "Reasons").
const ReasonBaseChanged = "base_changed"

// ReasonIntentWithdrawn is the drop reason WithdrawIntent records when the
// operator withdraws a yielded entry's merge intent (ADR-112 R3, #4685).
const ReasonIntentWithdrawn = "intent_withdrawn"

// ReentryReason is the eject reason ReentryCandidates matches: a red pass
// (the server's verify_failed), the only ejection ADR-093 D4 re-enters
// automatically, after a delegated fix-up. The store never writes it.
const ReentryReason = "verify_failed"

// AdmissionStaleAfter bounds how long a live entry may stay in
// PhaseAdmitted or PhaseAdvancing: past it AdmitNext re-claims the admission
// (a crash mid-admission recovers on the next pump). README.md § "Constants".
const AdmissionStaleAfter = 10 * time.Minute

// IntentFnTimeout is the deadline WithMergeIntentLock puts on its fn (the
// ADR-108 merge-intent lock fn deadline). Exported for the ADR-093 D8 resume,
// whose crash-left queue-claim age window is derived from it (README.md §
// "Merge-dispatch ledger", "Crash-left claims").
const IntentFnTimeout = intentFnTimeout

// Errors. Each is returned wrapped; match with errors.Is.
var (
	// ErrInvalidRequest is a request refused before any SQL runs.
	ErrInvalidRequest = errors.New("mergequeue: invalid request")
	// ErrNotFound is an unknown run or entry. It wraps run.ErrNotFound, so
	// errors.Is(err, run.ErrNotFound) also matches.
	ErrNotFound = fmt.Errorf("mergequeue: %w", run.ErrNotFound)
	// ErrStateConflict is an entry not in a state the call accepts (for
	// example Unadmit on a held entry, or Reanchor on a terminal one).
	ErrStateConflict = errors.New("mergequeue: entry state conflict")
	// ErrIntentUnrecorded is a merge-intent call whose RecordIntentFunc
	// failed: the whole call was rolled back and wrote nothing. It wraps the
	// callback's error.
	ErrIntentUnrecorded = errors.New("mergequeue: merge intent not recorded")
	// ErrIntentLockLapsed is a WithMergeIntentLock whose fn RAN and returned
	// nil, but whose lock could not be confirmed held through its release
	// (the lock session failed, or the unlock found no lock), so another
	// holder may have overlapped fn. fn's writes stand; the caller decides.
	// It also wraps pglock.ErrLockLapsed.
	ErrIntentLockLapsed = errors.New("mergequeue: merge-intent lock lapsed")
	// ErrAlreadyDispatched is a ClaimMergeDispatch for a run that already
	// holds a dispatch claim (dispatched or dispatch_failed): the UNIQUE
	// run_id of merge_dispatch_claims refused the insert, and the caller must
	// not call the forge (approval condition D1).
	ErrAlreadyDispatched = errors.New("mergequeue: merge already dispatched for this run")
	// ErrNotQueued is a Promote for a run with no active entry (none, or only
	// terminal rows).
	ErrNotQueued = errors.New("mergequeue: run has no held merge-queue entry")
	// ErrEntryLive is a Promote for a run whose entry is live: the entry
	// holding the line is never preempted, and promoting it means nothing.
	ErrEntryLive = errors.New("mergequeue: entry is live; the live entry is never preempted")
	// ErrEntryYielded is a Promote for a yielded (marked) held entry: AdmitNext
	// skips it whatever its order, so the marker must clear first.
	ErrEntryYielded = errors.New("mergequeue: entry is yielded; clear its yield before promoting")
	// ErrNoIntent is a WithdrawIntent on an active entry that carries no
	// merge intent: there is nothing to withdraw.
	ErrNoIntent = errors.New("mergequeue: entry carries no merge intent")
	// ErrEntryNotYielded is a WithdrawIntent on a held but UNMARKED entry:
	// only a yielded entry's intent is withdrawable (an unmarked one is
	// progressing to the queue's own merge).
	ErrEntryNotYielded = errors.New("mergequeue: entry is not yielded; only a yielded entry's merge intent can be withdrawn")
	// ErrDispatchClaimed is a WithdrawIntent for a run that holds an ADR-109
	// merge-dispatch claim in any state (dispatched or dispatch_failed): the
	// withdrawal never reads the claim as permission to clear it and never
	// writes the ledger.
	ErrDispatchClaimed = errors.New("mergequeue: run holds a merge-dispatch claim")
	// ErrReclaimUnrecorded is an AdmitNextReclaiming whose ReclaimRecordFunc
	// failed: the whole call was rolled back (no epoch bump, no admitted_at
	// change). It wraps the callback's error.
	ErrReclaimUnrecorded = errors.New("mergequeue: re-claim not recorded")
)

// Base is one queue: a repository base ref inside one account. AccountID is
// the run's runs.account_id as a string ("" for an untenanted run), the same
// shape run.Run.AccountID carries.
type Base struct {
	AccountID string
	Repo      string
	BaseRef   string
}

// Entry is one merge_candidate_queue_entries row. Phase, EjectReason and
// AnchoredBaseSHA are "" for NULL; MergeIntentAt is nil for NULL (no
// recorded merge intent). AccountID is read through runs, never stored on the
// row.
type Entry struct {
	ID              uuid.UUID
	RunID           uuid.UUID
	AccountID       string
	Repo            string
	BaseRef         string
	State           State
	Phase           Phase
	EjectReason     string
	EnqueuedAt      time.Time
	AnchoredHeadSHA string
	AnchoredBaseSHA string
	AnchoredAt      time.Time
	AdmittedAt      *time.Time
	SettledAt       *time.Time
	UpdatedAt       time.Time
	// MergeIntentAt is when the operator's merge intent was recorded on this
	// episode (0107, ADR-093 D2); set once, never cleared.
	MergeIntentAt *time.Time
	// MergeIntentSequence is the audit sequence of the merge_queue_entered
	// row that recorded the intent (0108), stamped with MergeIntentAt and
	// never overwritten; 0 for NULL (no intent, or one recorded before 0108).
	MergeIntentSequence int64
	// AdmissionEpoch is bumped by every admission (AdmitNext's admit and its
	// stale re-claim, 0108; AdmitNextReclaiming's boot re-claim). The admission-owned writes (Unadmit, SetPhase,
	// ReanchorAt, SettleAt, ApprovalWait, GateWait, Yield) are compare-and-swaps on it.
	AdmissionEpoch int64
	// YieldReason and YieldDetail are the yield marker (0109, ADR-112 R3):
	// "" for NULL. YieldReason is the server's closed vocabulary;
	// YieldDetail is untrusted forge-derived text every read surface must
	// sanitise.
	YieldReason string
	YieldDetail string
	// YieldedAt is when the entry yielded (nil: not marked). A marked entry
	// is held and AdmitNext skips it.
	YieldedAt *time.Time
	// YieldRevision is bumped by EVERY marker write (0115, docs/adr/112
	// A3.6): Yield, Enqueue's mark (and an insert carrying a marker),
	// UpdateYield, ClearYield and ReanchorYielded. (AnchoredHeadSHA,
	// YieldRevision) is the compare-and-swap key of ClearYield, UpdateYield
	// and ReanchorYielded.
	YieldRevision int64
	// Row4Flag is the persisted row-4 obligation (0115, A3.6): set by a
	// marker write whose YieldMarker.Row4 is true (OR: no reason transition
	// clears it), cleared only by ClearYield(clearRow4=true).
	Row4Flag bool
	// ApprovalWaitStartedAt is the persisted head-of-line approval-wait
	// anchor (nil: no wait started this episode). Set once by ApprovalWait,
	// never reset by Unadmit, a re-admission, a stale re-claim or (A3.7) a
	// ClearYield: it is RETAINED for the whole episode.
	ApprovalWaitStartedAt *time.Time
	// ApprovalWaitOpen reports an approval wait is open (0115, A3.7): set by
	// ApprovalWait, cleared by ClearYield. A retained anchor with no open
	// wait never expires a non-approval hold.
	ApprovalWaitOpen bool
	// GateWaitStartedAt and GateWaitEpoch are the per-admission-epoch
	// merge-gate window (0115, A3.2): its start and the admission epoch that
	// opened it (both nil, or both set). GateWait opens and judges it.
	GateWaitStartedAt *time.Time
	GateWaitEpoch     *int64
	// GateHoldTokens is the merge-gate hold budget (0115, A3.7: 0 to
	// GateHoldBudget) and GateTokensRefilledAt its refill timestamp (nil:
	// never spent).
	GateHoldTokens       int
	GateTokensRefilledAt *time.Time
	// YieldAge is clock_timestamp() - yielded_at, computed in SQL when the
	// row was read (database clock domain); 0 when unmarked.
	YieldAge time.Duration
	// OrderKey is the operator promote's ordering key (0111, ADR-093 slice
	// 3): nil when the row is ordered by EnqueuedAt. The effective queue key
	// is COALESCE(OrderKey, EnqueuedAt).
	OrderKey *time.Time
	// SettledAge is clock_timestamp() - settled_at, computed in SQL when the
	// row was read (database clock domain); 0 on an active row. The
	// re-entry sweep's backoff is keyed on it, so no Go clock is compared
	// against SettledAt.
	SettledAge time.Duration
}

// Yielded reports whether the entry carries a yield marker.
func (e Entry) Yielded() bool { return e.YieldedAt != nil }

// Promoted reports whether an operator promote set the entry's order key.
func (e Entry) Promoted() bool { return e.OrderKey != nil }

// Base returns the queue the entry belongs to.
func (e Entry) Base() Base {
	return Base{AccountID: e.AccountID, Repo: e.Repo, BaseRef: e.BaseRef}
}

// EnqueueRequest asks to put a run's pass in its base's queue. The
// repository and account come from the run row, never the caller.
type EnqueueRequest struct {
	RunID   uuid.UUID
	BaseRef string
	// HeadSHA is the PR head the pass is anchored to (required).
	HeadSHA string
	// BaseSHA is the base tip the head was produced against ("" if unknown).
	BaseSHA string
	// MergeIntent records the operator's merge intent on the entry (ADR-093
	// D2): a new row is stamped at insert, a same-base active entry only
	// when it carries none yet. False never clears an existing intent.
	MergeIntent bool
	// RecordIntent, when non-nil, is called INSIDE the transaction when this
	// call records merge intent (EnqueueResult.IntentRecorded), after every
	// write and before commit. A non-nil error rolls the whole call back and
	// Enqueue returns it wrapped in ErrIntentUnrecorded. Never called when
	// MergeIntent is false or the intent was already recorded.
	RecordIntent RecordIntentFunc
	// IntentSequence is the merge_queue_entered row's audit sequence, stamped
	// as merge_intent_sequence in the same statement as merge_intent_at and
	// only when THIS call records the intent (0 stamps NULL).
	IntentSequence int64
	// Yield, when non-nil, enqueues the entry YIELDED (ADR-112 R3, the
	// merge_run queue arm): a new row is inserted held with the marker; a
	// same-base HELD entry is re-anchored and marked, keeping an existing
	// yielded_at (the same episode); a same-base LIVE entry is never marked
	// (EnqueueResult.Marked=false). A nil Yield never clears or moves an
	// existing marker.
	Yield *YieldMarker
}

// YieldMarker is a yield marker's reason (required, the server's closed
// vocabulary) and detail (optional, untrusted).
type YieldMarker struct {
	Reason string
	Detail string
	// Row4, when true, sets the entry's row4_flag (docs/adr/112 A3.6) in
	// the same statement as the marker: the server sets it for a
	// forge_review_required yield. False never clears the flag.
	Row4 bool
	// Continue, when non-nil, is the entry's last CLEARED yield episode
	// (docs/adr/112 A3.7, #4780): a re-yield on the same base ref keeps that
	// episode's yielded_at, WHATEVER the reason or head, so the re-probe
	// backoff keeps growing instead of restarting. A write that would stamp
	// a fresh yielded_at (Yield, or Enqueue marking an unmarked held entry)
	// stamps Continue.YieldedAt instead ONLY when the entry's base_ref equals
	// Continue.BaseRef; any other base is a new episode on the database
	// clock. An already-marked entry keeps its own yielded_at whatever
	// Continue says.
	Continue *YieldEpisode
}

// YieldEpisode is a cleared yield episode: its yielded_at and the base ref
// it was cleared on.
type YieldEpisode struct {
	YieldedAt time.Time
	BaseRef   string
}

// continueArgs are the SQL arguments of m's episode continuation: NULLs
// when there is none.
func (m YieldMarker) continueArgs() (yieldedAt *time.Time, baseRef string) {
	if m.Continue == nil || m.Continue.YieldedAt.IsZero() || m.Continue.BaseRef == "" {
		return nil, ""
	}
	t := m.Continue.YieldedAt
	return &t, m.Continue.BaseRef
}

// validate refuses an empty reason before any SQL runs.
func (m YieldMarker) validate() error {
	if m.Reason == "" {
		return fmt.Errorf("%w: a yield marker requires a reason", ErrInvalidRequest)
	}
	return nil
}

// ApprovalWaitResult is ApprovalWait's verdict, judged on the database clock.
type ApprovalWaitResult struct {
	// Entry is the row after the call (ApprovalWaitStartedAt set).
	Entry Entry
	// Expired reports the window has elapsed since ApprovalWaitStartedAt.
	Expired bool
	// Remaining is the window left (0 when Expired).
	Remaining time.Duration
}

// GateHoldBudget is the merge-gate hold budget N (docs/adr/112 A3.7): a
// full bucket, and the 0115 CHECK's upper bound.
const GateHoldBudget = 3

// GateHoldRefill is the budget's refill period R (A3.7): one token per R the
// row spends not holding.
const GateHoldRefill = time.Hour

// GateWaitResult is GateWait's verdict, judged on the database clock.
type GateWaitResult struct {
	// Entry is the row after the call.
	Entry Entry
	// Opened reports THIS call opened the epoch's window (one token spent).
	Opened bool
	// Expired reports the window has elapsed since GateWaitStartedAt.
	Expired bool
	// NoToken reports the epoch had no window and the budget is empty after
	// the refill: nothing was written and the caller yields at once.
	NoToken bool
	// Remaining is the window left (0 when Expired or NoToken).
	Remaining time.Duration
}

// MergeIntentRequest is MarkMergeIntent's request; a nil request records
// the intent with no sequence and no callback.
type MergeIntentRequest struct {
	// IntentSequence is stamped as merge_intent_sequence with
	// merge_intent_at, only on the NULL-to-set transition (0 stamps NULL).
	IntentSequence int64
	// Record is EnqueueRequest.RecordIntent's callback for this call.
	Record RecordIntentFunc
}

// DispatchState is a merge_dispatch_claims.state value.
type DispatchState string

// Dispatch claim states.
const (
	DispatchDispatched DispatchState = "dispatched"
	DispatchFailed     DispatchState = "dispatch_failed"
	// DispatchUnknown is never stored: ClaimMergeDispatch returns it with
	// ErrAlreadyDispatched when the existing claim could not be read back.
	DispatchUnknown DispatchState = "unknown"
)

// DispatchUnknownField fills a claim's Path and HeadSHA alongside
// DispatchUnknown.
const DispatchUnknownField = "unknown"

// Dispatch paths: the three merge dispatch paths that claim (approval
// condition D1).
const (
	DispatchPathInline    = "inline"
	DispatchPathDelegated = "delegated"
	DispatchPathQueue     = "queue"
)

// DispatchClaim is one merge_dispatch_claims row: the run's ONE merge
// dispatch claim (0108). FailureCause and FailedAt are set only on
// dispatch_failed.
type DispatchClaim struct {
	RunID        uuid.UUID
	State        DispatchState
	Path         string
	HeadSHA      string
	ClaimedAt    time.Time
	FailedAt     *time.Time
	FailureCause string
}

// RecordIntentFunc is the record-before-commit callback of a merge-intent
// call: st is the entry's Status read inside the call's transaction (its
// writes visible, nothing committed). It runs while the call holds its
// advisory locks and the entry's row lock, so it must not call back into the
// Store.
type RecordIntentFunc func(ctx context.Context, st Status) error

// EnqueueOutcome says what Enqueue did.
type EnqueueOutcome string

// Enqueue outcomes.
const (
	// EnqueueInserted: a new held row at the tail (the run had no active
	// entry, or only terminal ones).
	EnqueueInserted EnqueueOutcome = "inserted"
	// EnqueueReanchored: the run's active entry on the same base was
	// re-anchored in place, keeping its state, phase and enqueued_at.
	EnqueueReanchored EnqueueOutcome = "reanchored"
	// EnqueueBaseChanged: the run's active entry on another base was dropped
	// base_changed and a new held row inserted at the new base's tail.
	EnqueueBaseChanged EnqueueOutcome = "base_changed"
)

// EnqueueResult is Enqueue's verdict.
type EnqueueResult struct {
	Entry   Entry
	Outcome EnqueueOutcome
	// AnchorChanged reports a re-anchor moved the head or base SHA (false on
	// a same-anchor re-enqueue, and on the insert outcomes).
	AnchorChanged bool
	// Dropped is the entry dropped base_changed (EnqueueBaseChanged only).
	// When it was live its base has no live entry now; pump that base.
	Dropped *Entry
	// IntentRecorded reports THIS call moved the entry's merge_intent_at
	// from NULL to set (MergeIntent only; false on a repeat).
	IntentRecorded bool
	// Marked reports the entry carries the request's Yield marker after the
	// call (Yield only; false when the entry is live, which is never marked).
	Marked bool
}

// MergeIntentResult is MarkMergeIntent's verdict.
type MergeIntentResult struct {
	// Entry is the row after the call.
	Entry Entry
	// IntentRecorded reports THIS call moved merge_intent_at from NULL to
	// set; false (and nothing written) when it was already set.
	IntentRecorded bool
}

// Admission is AdmitNext's verdict.
type Admission struct {
	// Admitted reports THIS call made Entry the base's live entry: either a
	// held entry was admitted, or (Readmitted) a stale admission, or one
	// admitted before AdmitNextReclaiming's boot instant, was re-claimed. The
	// caller owns the admission work.
	Admitted   bool
	Readmitted bool
	// Entry is the admitted entry when Admitted; otherwise the base's live
	// entry holding the line, or nil when the base has no live and no held
	// entry.
	Entry *Entry
}

// SettleResult is Settle's verdict.
type SettleResult struct {
	// Entry is the row after the call. On an already-terminal row it is that
	// row unchanged, carrying its FIRST terminal state and reason.
	Entry Entry
	// Settled reports this call moved the row to a terminal state.
	Settled bool
}

// Status is a run's active entry for read surfaces.
type Status struct {
	Entry Entry
	// Position is the 1-based place among the base's held entries (same
	// account) in ListForBase's order (unmarked before yielded, then
	// COALESCE(order_key, enqueued_at), id); 0 for a live entry.
	Position int
	// LiveRunID is the run holding the base's line (the entry's own run when
	// it is live), or nil when the base has no live entry.
	LiveRunID *uuid.UUID
}

// PromoteRequest asks to move a run's held entry to the head of its base's
// line (ADR-093 D2, the operator promote).
type PromoteRequest struct {
	RunID uuid.UUID
	// Record is REQUIRED. It runs inside the promotion transaction after the
	// order key is written and before COMMIT, and only when the order
	// changed; a non-nil error rolls the whole promotion back. The server
	// appends merge_queue_promoted through it (on tx), so a promotion never
	// commits unaudited.
	Record func(ctx context.Context, tx pgx.Tx, p Promotion) error
}

// Promotion is Promote's verdict.
type Promotion struct {
	// Entry is the run's held entry after the call.
	Entry Entry
	// Changed is false when the entry was already first among the base's
	// unmarked held entries (a no-op: nothing written, Record not called,
	// OldPosition = NewPosition = 1).
	Changed bool
	// OldPosition and NewPosition are 1-based places among the base's
	// unmarked held entries (same account), before and after the write.
	OldPosition int
	NewPosition int
	// LiveRunID is the run holding the base's line, or nil when none.
	LiveRunID *uuid.UUID
}

// Promoter is the operator promote (ADR-093 slice 3). PostgresStore
// implements it; it is a separate capability, not part of Store, so a caller
// type-asserts it and a fake that does not implement it degrades instead of
// panicking (README.md § "Promote").
type Promoter interface {
	Promote(ctx context.Context, req PromoteRequest) (Promotion, error)
}

// WithdrawIntentRequest asks to withdraw the merge intent of a run's YIELDED
// held entry (ADR-112 R3, #4685).
type WithdrawIntentRequest struct {
	RunID uuid.UUID
	// Record is REQUIRED. It runs inside the withdrawal transaction after the
	// settle and before COMMIT; a non-nil error rolls the whole withdrawal
	// back. The server appends merge_queue_intent_withdrawn through it (on
	// tx), so a withdrawal never commits unaudited.
	Record func(ctx context.Context, tx pgx.Tx, w Withdrawal) error
}

// Withdrawal is WithdrawIntent's verdict.
type Withdrawal struct {
	// Entry is the row after the settle: dropped with ReasonIntentWithdrawn,
	// its merge_intent_at, merge_intent_sequence and yield columns retained.
	Entry Entry
	// LiveRunID is the run holding the base's line, or nil when none.
	LiveRunID *uuid.UUID
}

// IntentWithdrawer is the withdraw-intent verb (ADR-112 R3, #4685): a
// separate capability like Promoter, so every existing fake keeps compiling
// and a fake without it degrades instead of panicking (README.md § "Withdraw
// intent").
type IntentWithdrawer interface {
	WithdrawIntent(ctx context.Context, req WithdrawIntentRequest) (Withdrawal, error)
}

// EjectionReader is the ejection visibility reads (ADR-093 slice 3): a
// separate capability like Promoter.
type EjectionReader interface {
	// LatestForRun returns the run's NEWEST row in any state (found=false:
	// the run never had one).
	LatestForRun(ctx context.Context, runID uuid.UUID) (Entry, bool, error)
	// UnresolvedEjections returns the base's ejected rows that are still
	// their run's newest row, newest settle first, at most limit.
	UnresolvedEjections(ctx context.Context, base Base, limit int) ([]Entry, error)
}

// ReentryReader is the automatic re-entry sweep's read (ADR-093 D4): a
// separate capability like Promoter.
type ReentryReader interface {
	// ReentryCandidates returns ejected ReentryReason rows that carry merge
	// intent, are their run's newest row and belong to a non-terminal run,
	// oldest settle first by (settled_at, id), strictly after the exclusive
	// keyset cursor after (nil starts at the oldest), at most limit, across
	// every account.
	ReentryCandidates(ctx context.Context, after *ReentryCursor, limit int) ([]Entry, error)
}

// Resumer is the restart resume's capability (ADR-093 D8, #4298): a separate
// interface like Promoter, so every existing fake keeps compiling and a fake
// without it degrades (the boot re-claim is off; the stale re-claim remains).
type Resumer interface {
	// DatabaseNow reads the database clock (clock_timestamp()): the boot
	// instant a caller passes back as ReclaimRequest.AdmittedBefore, so no Go
	// clock is ever compared against a stored instant.
	DatabaseNow(ctx context.Context) (time.Time, error)
	// AdmitNextReclaiming is AdmitNext, in the SAME admit transaction, that
	// also re-claims a live admitted/advancing entry admitted before
	// req.AdmittedBefore, calling req.Record inside the transaction on every
	// re-claim (README.md § "AdmitNextReclaiming").
	AdmitNextReclaiming(ctx context.Context, req ReclaimRequest) (Admission, error)
}

// ReclaimRequest is AdmitNextReclaiming's request.
type ReclaimRequest struct {
	Base Base
	// AdmittedBefore is a DATABASE-clock instant read back from DatabaseNow
	// (required; the zero time is ErrInvalidRequest): a live entry in
	// PhaseAdmitted or PhaseAdvancing whose admitted_at is strictly before it
	// is re-claimed even when it is not yet AdmissionStaleAfter old.
	AdmittedBefore time.Time
	// Record is REQUIRED. It runs inside the admit transaction on EVERY
	// re-claim (the boot instant or AdmissionStaleAfter), after the re-claim
	// write and before COMMIT; a non-nil error rolls the whole call back and
	// AdmitNextReclaiming returns it wrapped in ErrReclaimUnrecorded. It is
	// never called for an admission of a held entry, or when nothing is
	// re-claimed.
	Record ReclaimRecordFunc
}

// ReclaimRecordFunc is AdmitNextReclaiming's record-before-commit callback:
// reclaimed is the entry after the re-claim (its new admission epoch and
// admitted_at, its phase unchanged), read inside tx. The re-claim UPDATE
// touches no key column, so an audit.AppendChainedTx on tx takes no
// foreign-key lock on the runs row (the PromoteRequest.Record precedent). It
// runs under the call's base lock and row lock and must not call back into
// the Store.
type ReclaimRecordFunc func(ctx context.Context, tx pgx.Tx, reclaimed Entry) error

// ReentryCursor is an exclusive keyset position in ReentryCandidates' order
// (settled_at, id): the sweep pages past a full page of candidates that never
// pass a gate instead of re-reading them every tick (#4297).
type ReentryCursor struct {
	SettledAt time.Time
	ID        uuid.UUID
}

// ReentryCursorAt is e's position in ReentryCandidates' order, nil when e has
// no settle time (never a candidate).
func ReentryCursorAt(e Entry) *ReentryCursor {
	if e.SettledAt == nil {
		return nil
	}
	return &ReentryCursor{SettledAt: *e.SettledAt, ID: e.ID}
}

// Store is the held-pass queue. README.md § "Contract" defines every method.
type Store interface {
	Enqueue(ctx context.Context, req EnqueueRequest) (EnqueueResult, error)
	AdmitNext(ctx context.Context, base Base) (Admission, error)
	Unadmit(ctx context.Context, entryID uuid.UUID, epoch int64) (Entry, error)
	SetPhase(ctx context.Context, entryID uuid.UUID, epoch int64, phase Phase) (Entry, error)
	Reanchor(ctx context.Context, entryID uuid.UUID, headSHA, baseSHA string) (Entry, error)
	ReanchorAt(ctx context.Context, entryID uuid.UUID, epoch int64, headSHA, baseSHA string) (Entry, error)
	Settle(ctx context.Context, entryID uuid.UUID, state State, reason string) (SettleResult, error)
	SettleAt(ctx context.Context, entryID uuid.UUID, epoch int64, state State, reason string) (SettleResult, error)
	ActiveForRun(ctx context.Context, runID uuid.UUID) (Status, bool, error)
	ListForBase(ctx context.Context, base Base) ([]Entry, error)
	ActiveBases(ctx context.Context) ([]Base, error)
	MarkMergeIntent(ctx context.Context, entryID uuid.UUID, req *MergeIntentRequest) (MergeIntentResult, error)
	RecentLineTurns(ctx context.Context, base Base, limit int) ([]time.Duration, error)
	// ApprovalWait starts (once per episode) or continues the head-of-line
	// approval wait of a live entry at epoch and judges window's expiry on the
	// database clock (README.md § "Yield").
	ApprovalWait(ctx context.Context, entryID uuid.UUID, epoch int64, window time.Duration) (ApprovalWaitResult, error)
	// Yield moves a live entry at epoch to held with the marker, in one
	// statement (README.md § "Yield").
	Yield(ctx context.Context, entryID uuid.UUID, epoch int64, marker YieldMarker) (Entry, error)
	// GateWait opens (once per admission epoch, spending one hold token) or
	// continues the merge-gate window of a live entry at epoch and judges
	// window's expiry on the database clock; it never touches the approval
	// wait (README.md § "Yield", docs/adr/112 A3.2/A3.7).
	GateWait(ctx context.Context, entryID uuid.UUID, epoch int64, window time.Duration) (GateWaitResult, error)
	// ClearYield clears a held entry's marker and closes its approval wait
	// (the anchor is retained), a compare-and-swap on (anchored_head_sha,
	// yield_revision); clearRow4 also clears row4_flag.
	ClearYield(ctx context.Context, entryID uuid.UUID, headSHA string, revision int64, clearRow4 bool) (Entry, error)
	// UpdateYield rewrites a marked entry's reason and detail, the same
	// compare-and-swap.
	UpdateYield(ctx context.Context, entryID uuid.UUID, headSHA string, revision int64, marker YieldMarker) (Entry, error)
	// ReanchorYielded moves a marked entry's anchor to the PR's current head,
	// keeping the marker and the episode, the same compare-and-swap on the
	// OLD head.
	ReanchorYielded(ctx context.Context, entryID uuid.UUID, headSHA string, revision int64, newHeadSHA, newBaseSHA string) (Entry, error)
	// WithMergeIntentLock runs fn under the run's merge-intent lock; fn's
	// context is bounded by the intent fn deadline (even when ctx has none)
	// and must not take another pglock lock (README.md § "Merge-intent lock").
	WithMergeIntentLock(ctx context.Context, runID uuid.UUID, fn func(context.Context) error) error
	// ClaimMergeDispatch claims the run's ONE merge dispatch (approval
	// condition D1); a run already holding a claim is ErrAlreadyDispatched,
	// returned with that existing claim. Call it under WithMergeIntentLock,
	// BEFORE the forge (README.md § "Merge-dispatch ledger").
	ClaimMergeDispatch(ctx context.Context, runID uuid.UUID, path, headSHA string) (DispatchClaim, error)
	// MarkMergeDispatchFailed moves the run's claim to dispatch_failed with
	// cause, for an UNCERTAIN forge outcome; that claim is never deleted by
	// Fishhawk (reconciliation or the operator re-arm clears it).
	MarkMergeDispatchFailed(ctx context.Context, runID uuid.UUID, cause string) error
	// ReleaseMergeDispatch deletes claim, a compare-and-swap on the exact
	// claim, for a forge outcome that applied nothing; a claim that no
	// longer matches is ErrStateConflict and is left alone.
	ReleaseMergeDispatch(ctx context.Context, claim DispatchClaim) error
	// MergeDispatch reads the run's dispatch claim (found=false: none).
	MergeDispatch(ctx context.Context, runID uuid.UUID) (DispatchClaim, bool, error)
}

// validate refuses a malformed Base before any SQL runs.
func (b Base) validate() error {
	if b.Repo == "" || b.BaseRef == "" {
		return fmt.Errorf("%w: repo and base ref are required", ErrInvalidRequest)
	}
	if b.AccountID != "" {
		if _, err := uuid.Parse(b.AccountID); err != nil {
			return fmt.Errorf("%w: account id %q: %v", ErrInvalidRequest, b.AccountID, err)
		}
	}
	return nil
}

// lockKey derives the transaction-scoped advisory-lock key for one base in
// one account: the first 8 bytes of a sha256 over a "fishhawk:mergequeue:"
// namespaced input, a domain disjoint from every other advisory lock
// (concurrency.lockKey uses "fishhawk:concurrency:"). A collision only
// serializes two unrelated bases.
func lockKey(accountID, repo, baseRef string) int64 {
	h := sha256.New()
	h.Write([]byte("fishhawk:mergequeue:"))
	if id, err := uuid.Parse(accountID); err == nil {
		h.Write(id[:])
	} else {
		h.Write([]byte("untenanted"))
	}
	h.Write([]byte{0})
	h.Write([]byte(repo))
	h.Write([]byte{0})
	h.Write([]byte(baseRef))
	sum := h.Sum(nil)
	return int64(binary.BigEndian.Uint64(sum[:8])) //nolint:gosec // deliberate wrap: advisory-lock keys are opaque int64s
}

// intentLockKey derives WithMergeIntentLock's per-run key in its own
// "fishhawk:mergequeue-intent:" domain, disjoint from runLockKey: the lock is
// held on one connection while fn's Enqueue takes runLockKey on another, so
// a shared key would make the holder wait on itself.
func intentLockKey(runID uuid.UUID) int64 {
	h := sha256.New()
	h.Write([]byte("fishhawk:mergequeue-intent:"))
	h.Write(runID[:])
	sum := h.Sum(nil)
	return int64(binary.BigEndian.Uint64(sum[:8])) //nolint:gosec // deliberate wrap: advisory-lock keys are opaque int64s
}

// runLockKey derives the per-run advisory-lock key Enqueue takes before any
// base lock, so two enqueues of one run never race the base_changed path.
func runLockKey(runID uuid.UUID) int64 {
	h := sha256.New()
	h.Write([]byte("fishhawk:mergequeue-run:"))
	h.Write(runID[:])
	sum := h.Sum(nil)
	return int64(binary.BigEndian.Uint64(sum[:8])) //nolint:gosec // deliberate wrap: advisory-lock keys are opaque int64s
}
