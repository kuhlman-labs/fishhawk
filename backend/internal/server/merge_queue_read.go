package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
)

// The per-base merge-queue read (ADR-093 slice 1 D7, E87.4 / #4295):
// GET /v0/repos/{owner}/{name}/merge-queue[?base_ref=]. README.md § "Merge
// queue read" is the contract.

// mergeQueueETASamples bounds the line-turn history the ETA median reads: the
// base's newest N merged entries (mergequeue.Store.RecentLineTurns).
const mergeQueueETASamples = 10

// scopeMergeQueueRead is the read's scope tier: the gate view's read:audit,
// the same tier as the cross-run scope-overlap read, because the payload names
// other runs' ids and heads.
const scopeMergeQueueRead = scopeGateViewRead

// mergeQueueETABasisNote is the constant statement of what eta_seconds
// measures (approval condition C9).
const mergeQueueETABasisNote = "eta_seconds = position x the median line-turn time (admission to merge) of the base's newest merged entries; until ADR-093 slice 2's merge-on-pass lands a line turn includes the operator's merge step, so the estimate is as fast as the operator merges"

// Head-of-line current-head degrade reasons: the forge read is best-effort,
// so each names why current_head_sha is absent and the read still answers 200.
const (
	mergeQueueHeadRunReadFailed      = "run_read_failed"
	mergeQueueHeadForgeUnwired       = "forge_unwired"
	mergeQueueHeadPullRequestUnknown = "pull_request_unresolvable"
	mergeQueueHeadForgeReadFailed    = "forge_read_failed"
)

// mergeQueueReadResponse is the read's body. The MCP mirror
// (backend/internal/mcpserver/client.go) decodes it; the json tags MUST
// byte-match or a field silently decodes to its zero value.
type mergeQueueReadResponse struct {
	Repo string `json:"repo"`
	// Bases is one block per base ref: the requested one, or every base of
	// the repository with an active entry in the caller's account. Never nil.
	Bases []mergeQueueBaseView `json:"bases"`
}

// mergeQueueBaseView is one (account, repository, base ref) queue.
type mergeQueueBaseView struct {
	BaseRef string `json:"base_ref"`
	// Entries are the base's active entries in the store's queue order
	// (ListForBase): the live entry first, then unmarked held entries by
	// (COALESCE(order_key, enqueued_at), id) — FIFO by eligibility time
	// unless an operator promote set the order key (ADR-093 D2, promoted) —
	// then YIELDED held entries (ADR-112 R3: admission skips them until a
	// re-probe clears the marker). Never nil.
	Entries []mergeQueueEntryView `json:"entries"`
	// HeadOfLine is the first entry (the live one, or the next held entry to
	// admit when none is live; a yielded entry only when every held entry is
	// yielded) with its PR's current head; omitted when the base has no
	// active entry.
	HeadOfLine *mergeQueueHeadOfLine `json:"head_of_line,omitempty"`
	ETABasis   mergeQueueETABasis    `json:"eta_basis"`
	// Ejections are the base's UNRESOLVED ejections (ADR-093 D4, slice 3):
	// ejected rows still their run's newest row (no re-entry since), newest
	// settle first, at most mergeQueueEjectionLimit, in the caller's account,
	// each projected by mergeQueueEjectionProjectionFor. Never nil; empty
	// when the store lacks the mergequeue.EjectionReader capability.
	Ejections []mergeQueueEjectionView `json:"ejections"`
}

// mergeQueueEntryView is one active entry.
type mergeQueueEntryView struct {
	RunID   string `json:"run_id"`
	EntryID string `json:"entry_id"`
	State   string `json:"state"`
	// Phase is a live entry's progress; omitted while held (the store
	// clears it on every held row).
	Phase string `json:"phase,omitempty"`
	// Position is the 1-based place among the base's held entries; omitted
	// (0) for the live entry.
	Position int `json:"position,omitempty"`
	// MergeIntent is whether the operator's merge_run recorded merge intent
	// on the entry (ADR-093 D2); false for a rebase-path entry. Always on
	// the wire.
	MergeIntent   bool       `json:"merge_intent"`
	MergeIntentAt *time.Time `json:"merge_intent_at,omitempty"`
	// Promoted is whether an operator promote (ADR-093 D2,
	// fishhawk_promote_merge_queue_entry) set the entry's order key, so it
	// sorts ahead of its enqueue time. Always on the wire.
	Promoted        bool       `json:"promoted"`
	EnqueuedAt      time.Time  `json:"enqueued_at"`
	AdmittedAt      *time.Time `json:"admitted_at,omitempty"`
	AnchoredHeadSHA string     `json:"anchored_head_sha"`
	AnchoredBaseSHA string     `json:"anchored_base_sha,omitempty"`
	// ETASeconds is a held entry's estimated wait: Position x the median
	// line turn. Omitted for the live entry, for a YIELDED entry (it waits on
	// the operator, not the line) and when the base has no merged history.
	ETASeconds *int64 `json:"eta_seconds,omitempty"`
	// The ADR-112 R5 yield fields (mergeQueueYieldProjection): omitted on an
	// entry with no yield marker and no approval-wait anchor.
	YieldReason           string     `json:"yield_reason,omitempty"`
	YieldDetail           string     `json:"yield_detail,omitempty"`
	YieldedAt             *time.Time `json:"yielded_at,omitempty"`
	NextStep              string     `json:"next_step,omitempty"`
	ApprovalWaitStartedAt *time.Time `json:"approval_wait_started_at,omitempty"`
	// DispatchClaim is the run's merge-dispatch claim (ADR-093 approval
	// condition D1, ruling R2): present while a claim exists, so a
	// dispatch_failed claim holding the line is visible. Omitted with none.
	DispatchClaim *mergeQueueDispatchClaimView `json:"dispatch_claim,omitempty"`
}

// mergeQueueDispatchClaimView is one run's merge_dispatch_claims row.
type mergeQueueDispatchClaimView struct {
	// State is dispatched or dispatch_failed.
	State string `json:"state"`
	// Path is the claiming dispatch path: inline, delegated or queue.
	Path      string    `json:"path"`
	HeadSHA   string    `json:"head_sha"`
	ClaimedAt time.Time `json:"claimed_at"`
	// AgeSeconds is the claim's age at read time.
	AgeSeconds int64      `json:"age_seconds"`
	FailedAt   *time.Time `json:"failed_at,omitempty"`
	// Reason is a dispatch_failed claim's recorded forge failure.
	Reason string `json:"reason,omitempty"`
	// Orphan labels a crash-left QUEUE claim (ADR-109 A1, #4711):
	// orphaned or orphaned_by_restart (mergeQueueDispatchClaimOrphan).
	// Omitted otherwise, and "orphaned" is omitted when the database clock
	// cannot be read.
	Orphan string `json:"orphan,omitempty"`
}

// The dispatch-claim orphan labels (ADR-109 A1, #4711), shared by the
// merge-queue read's dispatch_claim.orphan and the run read's
// merge_candidate_queue.dispatch_claim_orphan.
const (
	// mergeQueueClaimOrphaned: a queue claim still dispatched past
	// mergeQueueCrashLeftClaimAge on the database clock. The next
	// merge-on-pass decision whose forge-approval fold approves releases it
	// in step 8 and merges through the ordinary path; while the fold does not
	// approve, the claim holds the head (ADR-109: a claimed dispatch is never
	// yielded).
	mergeQueueClaimOrphaned = "orphaned"
	// mergeQueueClaimOrphanedByRestart: a queue claim the restart resume
	// moved to dispatch_failed (orphaned_by_restart, the unreadable-PR arm).
	// It holds for the operator re-arm; nothing releases it automatically.
	mergeQueueClaimOrphanedByRestart = "orphaned_by_restart"
)

// mergeQueueDispatchClaimOrphan labels c: orphaned for a queue claim still
// dispatched whose non-zero claimed_at is older than
// mergeQueueCrashLeftClaimAge against dbNow (only when clockOK: the same
// predicate as mergeQueueClaimCrashLeft, so the label never names a claim the
// decision would not release); orphaned_by_restart for a queue dispatch_failed
// claim whose cause is orphaned_by_restart; "" otherwise (inline and delegated
// claims are never labelled). The decision never reads the label.
func mergeQueueDispatchClaimOrphan(c mergequeue.DispatchClaim, dbNow time.Time, clockOK bool) string {
	if c.Path != mergeDispatchPathQueue {
		return ""
	}
	switch c.State {
	case mergequeue.DispatchDispatched:
		if clockOK && !c.ClaimedAt.IsZero() && c.ClaimedAt.Before(dbNow.Add(-mergeQueueCrashLeftClaimAge)) {
			return mergeQueueClaimOrphaned
		}
	case mergequeue.DispatchFailed:
		if c.FailureCause == mergeQueueResumeReasonOrphanedByRestart {
			return mergeQueueClaimOrphanedByRestart
		}
	}
	return ""
}

// mergeQueueReadClock returns a read-once database clock for the orphan
// label: the first call reads Resumer.DatabaseNow, later calls reuse it. Only
// a queue dispatched claim calls it (mergeQueueDispatchClaimOrphanFor), so a
// read with no such claim never touches the clock. A store without the
// Resumer capability or a failed read yields (zero, false): the label is
// omitted and the caller's read never fails on it.
func (s *Server) mergeQueueReadClock(ctx context.Context) func() (time.Time, bool) {
	var (
		read bool
		now  time.Time
		ok   bool
	)
	return func() (time.Time, bool) {
		if read {
			return now, ok
		}
		read = true
		rs, has := s.cfg.MergeQueue.(mergequeue.Resumer)
		if !has {
			return now, false
		}
		n, err := rs.DatabaseNow(ctx)
		if err != nil {
			s.cfg.Logger.Warn("merge queue: database clock read failed; omitting the orphaned dispatch-claim label", "error", err.Error())
			return now, false
		}
		now, ok = n, true
		return now, ok
	}
}

// mergeQueueDispatchClaimOrphanFor labels c, reading clock only for a queue
// dispatched claim.
func mergeQueueDispatchClaimOrphanFor(c mergequeue.DispatchClaim, clock func() (time.Time, bool)) string {
	var (
		now time.Time
		ok  bool
	)
	if c.Path == mergeDispatchPathQueue && c.State == mergequeue.DispatchDispatched {
		now, ok = clock()
	}
	return mergeQueueDispatchClaimOrphan(c, now, ok)
}

// mergeQueueHeadOfLine is the base's first entry plus its PR's live head.
type mergeQueueHeadOfLine struct {
	RunID           string `json:"run_id"`
	EntryID         string `json:"entry_id"`
	State           string `json:"state"`
	Phase           string `json:"phase,omitempty"`
	AnchoredHeadSHA string `json:"anchored_head_sha"`
	// CurrentHeadSHA is the PR head read from the forge now; when it
	// differs from AnchoredHeadSHA the admission re-anchors before the pass.
	CurrentHeadSHA string `json:"current_head_sha,omitempty"`
	// CurrentHeadUnavailable names why CurrentHeadSHA is absent:
	// run_read_failed, forge_unwired, pull_request_unresolvable or
	// forge_read_failed.
	CurrentHeadUnavailable string `json:"current_head_unavailable,omitempty"`
	// The ADR-112 R5 yield fields, as on mergeQueueEntryView.
	YieldReason           string     `json:"yield_reason,omitempty"`
	YieldDetail           string     `json:"yield_detail,omitempty"`
	YieldedAt             *time.Time `json:"yielded_at,omitempty"`
	NextStep              string     `json:"next_step,omitempty"`
	ApprovalWaitStartedAt *time.Time `json:"approval_wait_started_at,omitempty"`
}

// mergeQueueYieldProjection is the ADR-112 R5 yield projection of one entry,
// shared by every read surface (the queue read's entries and head_of_line,
// and runMergeCandidateQueuePayload on the run read and the gate view) so
// they cannot disagree. ON A YIELDED ENTRY: yield_reason (the closed
// vocabulary, merge_queue_yield.go), yield_detail passed through THE
// sanitiser mergeQueueYieldDetail (forge-derived, untrusted: tokens
// redacted, cut at the first line break, control characters dropped,
// bounded), yielded_at, and next_step DERIVED from the reason through the
// total mapping mergeQueueYieldNextStep (never stored). approval_wait_started_at
// is the episode's persisted head-of-line wait anchor, carried whenever set
// (a live entry waiting at the head, or a yielded one). An entry with neither
// carries the zero value, so every field is omitted.
type mergeQueueYieldProjection struct {
	Reason                string
	Detail                string
	YieldedAt             *time.Time
	NextStep              string
	ApprovalWaitStartedAt *time.Time
}

// mergeQueueYieldProjectionFor projects e's yield marker and wait anchor.
func mergeQueueYieldProjectionFor(e mergequeue.Entry) mergeQueueYieldProjection {
	p := mergeQueueYieldProjection{ApprovalWaitStartedAt: e.ApprovalWaitStartedAt}
	if !e.Yielded() {
		return p
	}
	p.Reason = e.YieldReason
	p.Detail = mergeQueueYieldDetail(e.YieldDetail)
	p.YieldedAt = e.YieldedAt
	p.NextStep = mergeQueueYieldNextStep(e.YieldReason)
	return p
}

// mergeQueueETABasis is what every eta_seconds on the base is derived from.
type mergeQueueETABasis struct {
	// Samples is how many merged line turns the median read (at most
	// mergeQueueETASamples).
	Samples int `json:"samples"`
	// MedianSeconds is their median, rounded to the second; omitted with no
	// samples.
	MedianSeconds *int64 `json:"median_seconds,omitempty"`
	Note          string `json:"note"`
}

// handleGetMergeQueue implements GET /v0/repos/{owner}/{name}/merge-queue.
//
// Read authorization: the payload names other runs' ids and heads, so a
// run-bound mcp:run:<uuid> token is refused 403 run_token_forbidden
// (isRunBoundSubject). Every other caller must clear scopeMergeQueueRead
// (read:audit) via requireWriteScope: 401 anonymous, 403 insufficient_scope
// for a token missing it; a cookie-session operator passes. The repository is
// a point read (403 repo_forbidden when not visible). Both authorization
// checks run BEFORE the unwired-queue 503 (merge_queue_unconfigured), as on
// the sibling merge verb, so an unauthenticated or under-scoped caller cannot
// learn whether the deployment wires a merge queue.
//
// Account scope: the caller identity's AccountID is the Base account, so
// another account's queue on the same repository and base ref is structurally
// unreadable; an untenanted caller reads the untenanted queue. A non-UUID
// account fails closed 500 (callerAccountUUID).
//
// Store reads fail closed (500); the head-of-line forge read is best-effort
// (current_head_unavailable names the degrade, the read answers 200).
func (s *Server) handleGetMergeQueue(w http.ResponseWriter, r *http.Request) {
	if isRunBoundSubject(IdentityFrom(r.Context()).Subject) {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not read the merge queue; it names other runs and their heads", nil)
		return
	}
	if !s.requireWriteScope(w, r, scopeMergeQueueRead) {
		return
	}
	if s.cfg.MergeQueue == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "merge_queue_unconfigured",
			"the merge-queue read requires a configured merge queue", nil)
		return
	}
	acct, ok := s.callerAccountUUID(w, r)
	if !ok {
		return
	}
	account := ""
	if acct != nil {
		account = acct.String()
	}
	repo := r.PathValue("owner") + "/" + r.PathValue("name")
	if !s.enforceRepoVisibility(w, r, repo) {
		return
	}
	ctx := r.Context()

	var bases []mergequeue.Base
	if ref := r.URL.Query().Get("base_ref"); ref != "" {
		bases = []mergequeue.Base{{AccountID: account, Repo: repo, BaseRef: ref}}
	} else {
		all, err := s.cfg.MergeQueue.ActiveBases(ctx)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"list merge-queue bases failed", nil)
			return
		}
		for _, b := range all {
			if b.AccountID == account && b.Repo == repo {
				bases = append(bases, b)
			}
		}
	}

	resp := mergeQueueReadResponse{Repo: repo, Bases: make([]mergeQueueBaseView, 0, len(bases))}
	for _, b := range bases {
		view, err := s.mergeQueueBaseViewFor(ctx, b)
		if err != nil {
			s.cfg.Logger.Warn("merge-queue read failed", "repo", repo, "base_ref", b.BaseRef, "error", err.Error())
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"read the merge queue failed", map[string]any{"base_ref": b.BaseRef})
			return
		}
		resp.Bases = append(resp.Bases, view)
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// mergeQueueBaseViewFor projects one base: its entries in queue order, the
// ETA basis and the head of line. A store read error is returned.
func (s *Server) mergeQueueBaseViewFor(ctx context.Context, b mergequeue.Base) (mergeQueueBaseView, error) {
	entries, err := s.cfg.MergeQueue.ListForBase(ctx, b)
	if err != nil {
		return mergeQueueBaseView{}, err
	}
	turns, err := s.cfg.MergeQueue.RecentLineTurns(ctx, b, mergeQueueETASamples)
	if err != nil {
		return mergeQueueBaseView{}, err
	}
	view := mergeQueueBaseView{
		BaseRef:  b.BaseRef,
		Entries:  make([]mergeQueueEntryView, 0, len(entries)),
		ETABasis: mergeQueueETABasis{Samples: len(turns), Note: mergeQueueETABasisNote},
	}
	median, haveMedian := medianLineTurnSeconds(turns)
	if haveMedian {
		view.ETABasis.MedianSeconds = &median
	}
	held := 0
	clock := s.mergeQueueReadClock(ctx)
	for _, e := range entries {
		ev := mergeQueueEntryView{
			RunID:           e.RunID.String(),
			EntryID:         e.ID.String(),
			State:           string(e.State),
			Phase:           string(e.Phase),
			MergeIntent:     e.MergeIntentAt != nil,
			MergeIntentAt:   e.MergeIntentAt,
			Promoted:        e.Promoted(),
			EnqueuedAt:      e.EnqueuedAt,
			AdmittedAt:      e.AdmittedAt,
			AnchoredHeadSHA: e.AnchoredHeadSHA,
			AnchoredBaseSHA: e.AnchoredBaseSHA,
		}
		y := mergeQueueYieldProjectionFor(e)
		ev.YieldReason, ev.YieldDetail, ev.YieldedAt, ev.NextStep, ev.ApprovalWaitStartedAt =
			y.Reason, y.Detail, y.YieldedAt, y.NextStep, y.ApprovalWaitStartedAt
		claim, found, err := s.cfg.MergeQueue.MergeDispatch(ctx, e.RunID)
		if err != nil {
			return mergeQueueBaseView{}, err
		}
		if found {
			ev.DispatchClaim = &mergeQueueDispatchClaimView{
				State: string(claim.State), Path: claim.Path, HeadSHA: claim.HeadSHA, ClaimedAt: claim.ClaimedAt,
				AgeSeconds: int64(time.Since(claim.ClaimedAt) / time.Second), FailedAt: claim.FailedAt, Reason: claim.FailureCause,
				Orphan: mergeQueueDispatchClaimOrphanFor(claim, clock),
			}
		}
		if e.State == mergequeue.StateHeld {
			held++
			ev.Position = held
			// A yielded entry waits on the operator's remedy, not on the
			// line, so it carries no ETA (ADR-112 R5).
			if haveMedian && !e.Yielded() {
				eta := int64(held) * median
				ev.ETASeconds = &eta
			}
		}
		view.Entries = append(view.Entries, ev)
	}
	if len(entries) > 0 {
		view.HeadOfLine = s.mergeQueueHeadOfLineFor(ctx, entries[0])
	}
	view.Ejections, err = s.mergeQueueEjectionsFor(ctx, b)
	if err != nil {
		return mergeQueueBaseView{}, err
	}
	return view, nil
}

// medianLineTurnSeconds is the median of turns rounded to the second (the
// mean of the middle two for an even count); false with no samples.
func medianLineTurnSeconds(turns []time.Duration) (int64, bool) {
	if len(turns) == 0 {
		return 0, false
	}
	sorted := slices.Clone(turns)
	slices.Sort(sorted)
	mid := len(sorted) / 2
	m := sorted[mid]
	if len(sorted)%2 == 0 {
		m = (sorted[mid-1] + sorted[mid]) / 2
	}
	return int64(m.Round(time.Second) / time.Second), true
}

// mergeQueueHeadOfLineFor projects the base's first entry and reads its PR's
// live head, best-effort: every uncertainty leaves CurrentHeadSHA empty and
// names the reason in CurrentHeadUnavailable.
func (s *Server) mergeQueueHeadOfLineFor(ctx context.Context, e mergequeue.Entry) *mergeQueueHeadOfLine {
	out := &mergeQueueHeadOfLine{
		RunID:           e.RunID.String(),
		EntryID:         e.ID.String(),
		State:           string(e.State),
		Phase:           string(e.Phase),
		AnchoredHeadSHA: e.AnchoredHeadSHA,
	}
	y := mergeQueueYieldProjectionFor(e)
	out.YieldReason, out.YieldDetail, out.YieldedAt, out.NextStep, out.ApprovalWaitStartedAt =
		y.Reason, y.Detail, y.YieldedAt, y.NextStep, y.ApprovalWaitStartedAt
	head, reason := s.mergeQueueCurrentHead(ctx, e)
	out.CurrentHeadSHA, out.CurrentHeadUnavailable = head, reason
	return out
}

// mergeQueueCurrentHead reads the entry's PR head from the forge, or returns
// the degrade reason.
func (s *Server) mergeQueueCurrentHead(ctx context.Context, e mergequeue.Entry) (string, string) {
	if s.cfg.RunRepo == nil {
		return "", mergeQueueHeadRunReadFailed
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, e.RunID)
	if err != nil {
		return "", mergeQueueHeadRunReadFailed
	}
	if s.cfg.GitHub == nil || runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return "", mergeQueueHeadForgeUnwired
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	prNumber := parsePRNumberFromURL(runRow.PullRequestURL)
	if err != nil || prNumber <= 0 {
		return "", mergeQueueHeadPullRequestUnknown
	}
	pr, err := s.cfg.GitHub.GetPullRequest(ctx, forge.FromGitHubInstallationID(*runRow.InstallationID), repo, prNumber)
	if err == nil && pr.HeadSHA == "" {
		err = errors.New("the forge returned an empty head sha")
	}
	if err != nil {
		s.cfg.Logger.Warn("merge-queue read: head-of-line PR read failed; omitting current_head_sha",
			"run_id", e.RunID.String(), "error", err.Error())
		return "", mergeQueueHeadForgeReadFailed
	}
	return pr.HeadSHA, ""
}

// mergeQueueEjectionLimit bounds a base's ejections[] on the queue read.
const mergeQueueEjectionLimit = 10

// The closed reentry vocabulary of a merge_queue_ejection projection: how the
// ejected run gets back into the queue (always at the TAIL, ADR-093 Q2).
const (
	// mergeQueueEjectionReentryOperator: the operator's next
	// fishhawk_merge_run re-enters it.
	mergeQueueEjectionReentryOperator = "operator_merge_run"
	// mergeQueueEjectionReentryAutomatic: ADR-090 D6 routed a delegated
	// fix-up, so the re-entry sweep re-enters it once that fix-up has pushed
	// and the run is merge-queue eligible again (fishhawk_merge_run still
	// re-enters it sooner).
	mergeQueueEjectionReentryAutomatic = "automatic_after_delegated_fixup"
)

// The next_step_basis vocabulary: where the projection's fixup_routed and
// forge reason came from.
const (
	// mergeQueueEjectionBasisRecorded: the entry's merge_queue_ejected row.
	mergeQueueEjectionBasisRecorded = "recorded"
	// mergeQueueEjectionBasisReasonDefault: the row is absent (a lost
	// best-effort append) or unreadable, so next_step is the eject reason's
	// default (verify_failed reads as not routed: fail closed to the
	// operator's fishhawk_merge_run) and fixup_routed is omitted.
	mergeQueueEjectionBasisReasonDefault = "reason_default"
)

// mergeQueueEjectionView is the ONE ejection projection (ADR-093 D4, slice
// 3), shared by the queue read's ejections[], GET /v0/runs/{id} and the gate
// view (merge_queue_ejection), so the surfaces cannot disagree. The MCP
// mirror (backend/internal/mcpserver/client.go mergeQueueEjection) decodes
// it; the json tags MUST byte-match.
type mergeQueueEjectionView struct {
	RunID   string `json:"run_id"`
	EntryID string `json:"entry_id"`
	Repo    string `json:"repo"`
	BaseRef string `json:"base_ref"`
	// Reason is the store's eject reason: verify_failed, advance_conflict,
	// forge_merge_refused, pass_not_startable, or another settle reason.
	Reason string `json:"reason"`
	// NextStep is DERIVED through the total mapping mergeQueueEjectNextStep
	// from Reason and the recorded fixup_routed, never copied from the row.
	NextStep string `json:"next_step"`
	// Remedy is prose built from the closed vocabulary only
	// (mergeQueueEjectRemedy), never from forge text.
	Remedy string `json:"remedy"`
	// NextStepBasis is recorded or reason_default (the row was absent or
	// unreadable).
	NextStepBasis   string     `json:"next_step_basis"`
	EjectedAt       *time.Time `json:"ejected_at,omitempty"`
	AnchoredHeadSHA string     `json:"anchored_head_sha"`
	MergeIntent     bool       `json:"merge_intent"`
	// FixupRouted is set for a verify_failed ejection whose row was read:
	// whether ADR-090 D6 routed a delegated fix-up. Omitted otherwise.
	FixupRouted *bool `json:"fixup_routed,omitempty"`
	// ForgeReason is a forge_merge_refused ejection's typed code
	// (not_mergeable, unprocessable, checks_not_settled, merge_method_refused);
	// a recorded value outside that closed set is dropped.
	ForgeReason string `json:"forge_reason,omitempty"`
	// ForgeDetail is the forge's refusal text, UNTRUSTED: sanitised AGAIN on
	// read (mergeQueueYieldDetail) on top of the writer's pass.
	ForgeDetail string `json:"forge_detail,omitempty"`
	// Reentry is operator_merge_run or automatic_after_delegated_fixup.
	Reentry string `json:"reentry"`
}

// mergeQueueEjectionRecord is the subset of a merge_queue_ejected payload the
// projection reads.
type mergeQueueEjectionRecord struct {
	EntryID     string `json:"entry_id"`
	FixupRouted *bool  `json:"fixup_routed"`
	ForgeReason string `json:"forge_reason"`
	ForgeDetail string `json:"forge_detail"`
}

// mergeQueueEjectionRecordFor reads e's NEWEST merge_queue_ejected row
// (matched by entry_id); false when the audit repository is unwired, the read
// fails or no row names the entry. A failure never fails the caller's read.
func (s *Server) mergeQueueEjectionRecordFor(ctx context.Context, e mergequeue.Entry) (mergeQueueEjectionRecord, bool) {
	if s.cfg.AuditRepo == nil {
		return mergeQueueEjectionRecord{}, false
	}
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, e.RunID, CategoryMergeQueueEjected)
	if err != nil {
		s.cfg.Logger.Warn("merge queue: read merge_queue_ejected failed; the ejection projection degrades to the reason default",
			"run_id", e.RunID.String(), "entry_id", e.ID.String(), "error", err.Error())
		return mergeQueueEjectionRecord{}, false
	}
	for i := len(rows) - 1; i >= 0; i-- {
		var rec mergeQueueEjectionRecord
		if json.Unmarshal(rows[i].Payload, &rec) == nil && rec.EntryID == e.ID.String() {
			return rec, true
		}
	}
	return mergeQueueEjectionRecord{}, false
}

// mergeQueueEjectionProjectionFor projects the EJECTED entry e. The row
// contributes only fixup_routed (verify_failed), a forge reason inside the
// closed set and the re-sanitised forge detail; next_step, remedy and reentry
// are derived from the closed vocabulary. An absent or unreadable row
// degrades to the reason default (next_step_basis reason_default).
func (s *Server) mergeQueueEjectionProjectionFor(ctx context.Context, e mergequeue.Entry) mergeQueueEjectionView {
	v := mergeQueueEjectionView{
		RunID:           e.RunID.String(),
		EntryID:         e.ID.String(),
		Repo:            e.Repo,
		BaseRef:         e.BaseRef,
		Reason:          e.EjectReason,
		NextStepBasis:   mergeQueueEjectionBasisReasonDefault,
		EjectedAt:       e.SettledAt,
		AnchoredHeadSHA: e.AnchoredHeadSHA,
		MergeIntent:     e.MergeIntentAt != nil,
	}
	routed := false
	if rec, ok := s.mergeQueueEjectionRecordFor(ctx, e); ok {
		v.NextStepBasis = mergeQueueEjectionBasisRecorded
		if e.EjectReason == mergeCandidateQueueReasonVerifyFailed {
			routed = rec.FixupRouted != nil && *rec.FixupRouted
			v.FixupRouted = &routed
		}
		switch rec.ForgeReason {
		case mergeQueueForgeReasonNotMergeable, mergeQueueForgeReasonUnprocessable, mergeQueueForgeReasonChecksNotSettled,
			mergeQueueForgeReasonMergeMethodRefused:
			v.ForgeReason = rec.ForgeReason
		}
		v.ForgeDetail = mergeQueueYieldDetail(rec.ForgeDetail)
	}
	v.NextStep = mergeQueueEjectNextStep(e.EjectReason, routed)
	v.Remedy = mergeQueueEjectRemedy(v.NextStep, v.ForgeReason)
	v.Reentry = mergeQueueEjectionReentryOperator
	if v.NextStep == mergeQueueEjectNextAwaitDelegatedFixup {
		v.Reentry = mergeQueueEjectionReentryAutomatic
	}
	return v
}

// mergeQueueEjectionReader is the store's OPTIONAL ejection-read capability
// (approval condition M1): false when the queue is unwired or the store does
// not implement mergequeue.EjectionReader (a test fake embedding only
// mergequeue.Store), so every ejection surface degrades to absent / empty
// instead of panicking.
func (s *Server) mergeQueueEjectionReader() (mergequeue.EjectionReader, bool) {
	if s.cfg.MergeQueue == nil {
		return nil, false
	}
	er, ok := s.cfg.MergeQueue.(mergequeue.EjectionReader)
	return er, ok
}

// mergeQueueEjectionsFor projects base b's unresolved ejections, newest
// first. Never nil; empty without the capability. A store read error is
// returned (the queue read fails closed on store errors).
func (s *Server) mergeQueueEjectionsFor(ctx context.Context, b mergequeue.Base) ([]mergeQueueEjectionView, error) {
	out := []mergeQueueEjectionView{}
	er, ok := s.mergeQueueEjectionReader()
	if !ok {
		return out, nil
	}
	rows, err := er.UnresolvedEjections(ctx, b, mergeQueueEjectionLimit)
	if err != nil {
		return nil, err
	}
	for _, e := range rows {
		out = append(out, s.mergeQueueEjectionProjectionFor(ctx, e))
	}
	return out, nil
}

// mergeQueueEjectionFor is the run read's and the gate view's
// merge_queue_ejection block: present only when the run's NEWEST queue row is
// ejected, which also means it has no active entry (an active entry is
// always the run's newest row) and has not re-entered since. (nil, nil)
// without the capability, with no row, or with a newer non-ejected row; a
// store read error is returned for the caller's degrade (run read: omit;
// gate view: omit plus a history_gaps entry).
func (s *Server) mergeQueueEjectionFor(ctx context.Context, runID uuid.UUID) (*mergeQueueEjectionView, error) {
	er, ok := s.mergeQueueEjectionReader()
	if !ok {
		return nil, nil
	}
	latest, found, err := er.LatestForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if !found || latest.State != mergequeue.StateEjected {
		return nil, nil
	}
	v := s.mergeQueueEjectionProjectionFor(ctx, latest)
	return &v, nil
}
