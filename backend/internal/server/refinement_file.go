package server

// refinement_file.go implements the E34.3 filing executor endpoint (ADR-052
// filing half, #1594): POST /v0/refinement/sessions/{session_id}/file turns an
// approved, hash-pinned refinement draft into real tracker items (epic first,
// then children in wave order) over the EXISTING conventions/provider pipeline
// (applyAndFileWorkItem), then asserts the filed epic round-trips through the
// provider's EpicChildren + campaign.Assemble.
//
// It is gated behind the existing write:approvals scope (no new scope, empty
// auth-checklist impact inventory — E34.2's scopeRefinementGate precedent).
//
// DETACHED ARM (#4153). The handler runs a SYNCHRONOUS gate — scope, session,
// strict body, the ApprovedDraft approval/drift check, repo parse,
// conventions, installation resolution, then the two ledger short-circuits
// (a pinned repo different from the request is 409
// refinement_filing_repo_mismatch; a completed session replays 200
// already_completed with zero forge calls) — and then launches
// refinement.ExecuteFilingWith on a server-lifetime goroutine
// (Server.bgRefinementFiling, drained by Shutdown) under a child-count-scaled
// budget (refinementFilingBudgetFor), returning 202 filing_in_progress at
// once. No request is held open across the forge round-trips, so a client
// timeout can no longer cancel a filing mid-sequence. Progress and a
// mid-sequence failure are observed on GET /v0/refinement/sessions/{id}'s
// `filing` block, not as a 502 body.
//
// SINGLE-FLIGHT. refinementFilingTracker is the in-process per-draft guard: a
// second POST while a filing for the draft is in flight returns 202
// already_in_progress and launches nothing. The per-draft advisory lock
// (refinement.ExecuteFilingWith -> WithFilingLock) remains the cross-process
// guard, and the executor is idempotent (write-ahead ledger rows, per-item
// idempotency keys, resume adoption and the link pass), so a re-invoke after a
// failure resumes without duplicating.
//
// AUDIT ORDERING (the E34.2 durable-before-state-change discipline): the
// refinement_filing_completed audit entry is appended on the GLOBAL chain
// BEFORE CompleteFilingSession flips completed_at, so an audit-append failure
// leaves completed_at NULL (reported as filing.state failed) and the re-invoke
// retries the close. Both happen inside the advisory lock (finalize), so the
// append-then-flip pair is atomic against a concurrent filer of the same
// draft. No new audit category is introduced.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/refinement"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// The POST /file response statuses.
const (
	// refinementFilingStatusInProgress: this call launched the detached filing.
	refinementFilingStatusInProgress = "filing_in_progress"
	// refinementFilingStatusAlreadyInProgress: a filing for the draft was
	// already in flight in this process; nothing new was launched.
	refinementFilingStatusAlreadyInProgress = "already_in_progress"
	// refinementFilingStatusFiled: the session was already complete (200
	// replay, no forge calls).
	refinementFilingStatusFiled = "filed"
)

// The GET session view's filing.state values.
const (
	refinementFilingStateInProgress = "in_progress"
	refinementFilingStateFailed     = "failed"
	refinementFilingStateIncomplete = "incomplete"
	refinementFilingStateFiled      = "filed"
)

// The finalize steps a detached filing can stop at, beside the executor's
// refinement.FilingStepCreate / FilingStepReconcile / FilingStepLink.
const (
	refinementFilingStepVerify   = "verify"
	refinementFilingStepAudit    = "audit"
	refinementFilingStepComplete = "complete"
)

// refinementFilingBudget is the FLOOR of the detached filing's budget (#4153);
// refinementFilingBudgetFor scales it by child count. The filing no longer
// runs on the POST request, so the budget bounds a wedged forge's hold on the
// goroutine, not on the operator; on expiry the executor stops before its next
// item (or its in-flight forge call returns the context error), the failure is
// recorded in the tracker, and a re-invoke resumes. A var, not a const, ONLY so
// a test can shrink it; production never reassigns it.
var refinementFilingBudget = 5 * time.Minute

// refinementFilingPerItemBudget is the per-item scale of the detached filing's
// budget: (children + 1 epic) items. The #4153 incident filed about 4 items in
// 30s, so 45s per item is a wide margin; it holds no request open because the
// filing is detached. A var so a test can shrink it.
var refinementFilingPerItemBudget = 45 * time.Second

// refinementFilingBudgetFor is the PURE budget for filing a draft with
// `children` children: the floor, or (children+1) * the per-item budget,
// whichever is larger.
func refinementFilingBudgetFor(children int) time.Duration {
	scaled := time.Duration(children+1) * refinementFilingPerItemBudget
	if scaled > refinementFilingBudget {
		return scaled
	}
	return refinementFilingBudget
}

// refinementFilingEntry is one draft's in-process filing status.
//
// STATE MACHINE (C4, #4153). The entry is {in_flight, last_error,
// failed_ordinal, step} plus launch metadata (repo, child count, start time,
// budget):
//   - absent: no filing has run in this process since the last success (or
//     since a restart); the GET view falls back to the durable ledger
//     (`incomplete` for an open filing session, `filed` once completed).
//   - LAUNCH (tryStart on an absent or not-in-flight entry): the entry is
//     OVERWRITTEN with in_flight=true and any previous last_error /
//     failed_ordinal / step cleared. A re-invoke after a failure therefore
//     LAUNCHES a resume (202 filing_in_progress), never already_in_progress.
//   - tryStart on an in-flight entry launches nothing (202
//     already_in_progress).
//   - SUCCESS clears the entry: completion is durable in the ledger
//     (completed_at), which the GET view reads as `filed`.
//   - FAILURE sets in_flight=false and keeps last_error / failed_ordinal /
//     step until the next launch overwrites them (GET view: `failed`).
type refinementFilingEntry struct {
	inFlight      bool
	lastError     string
	failedOrdinal *int
	step          string

	repo       string
	childCount int
	startedAt  time.Time
	budget     time.Duration
}

// refinementFilingTracker is the in-process per-draft single-flight guard and
// status store for the detached filing arm. Its zero value is ready to use. It
// is in-process only: another replica, or this one after a restart, sees no
// entry and the GET view reports an open filing session as `incomplete`; the
// pg advisory lock still serializes cross-process executors.
type refinementFilingTracker struct {
	mu      sync.Mutex
	entries map[uuid.UUID]*refinementFilingEntry
}

// tryStart launches a filing for draftID unless one is already in flight. It
// returns started=true after overwriting the entry (in_flight=true, the
// previous failure cleared); on an in-flight entry it returns started=false
// and a copy of that entry.
func (t *refinementFilingTracker) tryStart(draftID uuid.UUID, repo string, childCount int, budget time.Duration) (refinementFilingEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.entries[draftID]; ok && e.inFlight {
		return *e, false
	}
	if t.entries == nil {
		t.entries = make(map[uuid.UUID]*refinementFilingEntry)
	}
	e := &refinementFilingEntry{
		inFlight:   true,
		repo:       repo,
		childCount: childCount,
		startedAt:  time.Now().UTC(),
		budget:     budget,
	}
	t.entries[draftID] = e
	return *e, true
}

// succeed clears draftID's entry: the completion is durable in the ledger.
func (t *refinementFilingTracker) succeed(draftID uuid.UUID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.entries, draftID)
}

// fail records a failed filing: in_flight=false, and last_error /
// failed_ordinal / step kept until the next launch overwrites them.
func (t *refinementFilingTracker) fail(draftID uuid.UUID, lastError string, failedOrdinal *int, step string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[draftID]
	if !ok {
		e = &refinementFilingEntry{}
		if t.entries == nil {
			t.entries = make(map[uuid.UUID]*refinementFilingEntry)
		}
		t.entries[draftID] = e
	}
	e.inFlight = false
	e.lastError = lastError
	e.failedOrdinal = failedOrdinal
	e.step = step
}

// snapshot returns a copy of draftID's entry, if any.
func (t *refinementFilingTracker) snapshot(draftID uuid.UUID) (refinementFilingEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entries[draftID]
	if !ok {
		return refinementFilingEntry{}, false
	}
	return *e, true
}

// refinementFinalizeError tags a finalize failure with the step that stopped
// (verify | audit | complete), so the tracker's step names it.
type refinementFinalizeError struct {
	step string
	err  error
}

func (e *refinementFinalizeError) Error() string { return e.step + ": " + e.err.Error() }
func (e *refinementFinalizeError) Unwrap() error { return e.err }

// fileRefinementSessionRequest is the POST body: the target repo (owner/name)
// the approved draft files into. Strict-decoded + bounded via decodeRefinementBody.
type fileRefinementSessionRequest struct {
	Repo string `json:"repo"`
}

// refinementFiledEpicView / refinementFiledChildView echo what landed.
type refinementFiledEpicView struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

type refinementFiledChildView struct {
	Ordinal int    `json:"ordinal"`
	Number  int    `json:"number"`
	URL     string `json:"url"`
}

// refinementFileResponse is the POST response. A launch (202) carries status
// filing_in_progress, the child count, the budget and the epic/children filed
// so far; a concurrent call (202) carries status already_in_progress and
// already_in_progress=true; a completed session's replay (200) carries status
// filed, already_completed=true and the recorded epic + children.
type refinementFileResponse struct {
	Status            string                     `json:"status"`
	SessionID         string                     `json:"session_id"`
	DraftID           string                     `json:"draft_id"`
	Repo              string                     `json:"repo"`
	ChildCount        int                        `json:"child_count"`
	BudgetSeconds     int                        `json:"budget_seconds,omitempty"`
	AlreadyInProgress bool                       `json:"already_in_progress"`
	Epic              *refinementFiledEpicView   `json:"epic,omitempty"`
	Children          []refinementFiledChildView `json:"children"`
	Resumed           bool                       `json:"resumed"`
	AlreadyCompleted  bool                       `json:"already_completed"`
	Verified          bool                       `json:"verified"`
}

// refinementFilingJob is everything the detached filing goroutine needs,
// resolved on the request: it never closes over the request itself.
type refinementFilingJob struct {
	sessionID uuid.UUID
	approved  *refinement.StoredDraft
	repo      string
	owner     string
	name      string
	conv      workmgmt.Conventions
	target    workmgmt.Target
}

// handleFileRefinementSession implements POST
// /v0/refinement/sessions/{session_id}/file.
func (s *Server) handleFileRefinementSession(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, scopeRefinementGate) {
		return
	}
	if !s.refinementRepoConfigured(w, r) {
		return
	}
	sessionID, ok := s.parseSessionID(w, r)
	if !ok {
		return
	}

	var req fileRefinementSessionRequest
	if !s.decodeRefinementBody(w, r, &req) {
		return
	}

	drafts, ok := s.loadRefinementSession(w, r, sessionID)
	if !ok {
		return
	}
	decisions, err := s.cfg.RefinementRepo.ListDecisions(r.Context(), sessionID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not load decisions", map[string]any{"error": err.Error()})
		return
	}

	// The E34.2 gate: a draft cannot be filed in any state except approved, and
	// the executor refuses a drifted draft (its content no longer matches the
	// pinned approval hash).
	approved, err := refinement.ApprovedDraft(drafts, decisions)
	if err != nil {
		switch {
		case errors.Is(err, refinement.ErrNotApproved):
			s.writeError(w, r, http.StatusConflict, "refinement_not_approved",
				"the session's latest revision is not approved", nil)
		case errors.Is(err, refinement.ErrDraftDrifted):
			s.writeError(w, r, http.StatusConflict, "refinement_draft_drifted",
				"the approved draft content has drifted from the decision; re-approve before filing", nil)
		default:
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"could not resolve the approved draft", map[string]any{"error": err.Error()})
		}
		return
	}

	owner, name, valid := splitRepoFullName(req.Repo)
	if !valid {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo must be in owner/name form", map[string]any{"field": "repo", "got": req.Repo})
		return
	}

	conv, err := conventionsLoader(r.Context(), req.Repo)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not load work-management conventions", map[string]any{"error": err.Error()})
		return
	}

	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Project: conv.Project,
		Jira:    conv.Jira,
	}
	// Resolve the App installation for the target repo, exactly like
	// handleFileWorkItem's run-absent operator path: an ErrNotInstalled leaves
	// a zero scope so the provider fails closed with its own actionable error;
	// a transient resolution failure is surfaced as 502 here.
	if s.cfg.GitHub != nil {
		scope, rerr := s.resolveRepoScope(r.Context(), owner, name)
		if rerr != nil {
			s.writeError(w, r, http.StatusBadGateway, "refinement_filing_failed",
				"could not resolve the GitHub App installation for the target repo",
				map[string]any{"error": rerr.Error()})
			return
		}
		target.Scope = scope
	}

	// The synchronous ledger short-circuits, BEFORE anything is launched: a
	// pinned repo different from the request fails closed (409), and a
	// completed session replays the recorded result (200) with zero forge
	// calls. The executor re-checks both under its lock (the cross-process
	// guard); these make the common cases synchronous answers.
	sess, err := s.cfg.RefinementRepo.GetFilingSession(r.Context(), approved.ID)
	switch {
	case errors.Is(err, refinement.ErrNotFound):
		sess = nil
	case err != nil:
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not load the filing session", map[string]any{"error": err.Error()})
		return
	}
	if sess != nil && sess.Repo != req.Repo {
		s.writeError(w, r, http.StatusConflict, "refinement_filing_repo_mismatch",
			fmt.Sprintf("%v: session pins %q, requested %q", refinement.ErrFilingRepoMismatch, sess.Repo, req.Repo),
			map[string]any{"requested_repo": req.Repo, "pinned_repo": sess.Repo})
		return
	}
	var recorded []*refinement.FiledItem
	if sess != nil {
		if recorded, err = s.cfg.RefinementRepo.ListFiledItems(r.Context(), approved.ID); err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"could not load the filed items", map[string]any{"error": err.Error()})
			return
		}
	}
	childCount := len(approved.Draft.Children)
	resp := refinementFileResponse{
		SessionID:  sessionID.String(),
		DraftID:    approved.ID.String(),
		Repo:       req.Repo,
		ChildCount: childCount,
		Resumed:    len(recorded) > 0,
	}
	resp.Epic, resp.Children = filedItemViews(recorded)
	if sess != nil && sess.CompletedAt != nil {
		resp.Status = refinementFilingStatusFiled
		resp.AlreadyCompleted = true
		s.writeJSON(w, r, http.StatusOK, resp)
		return
	}

	// Single-flight: a filing for this draft already in flight in this process
	// launches nothing. One for a DIFFERENT repo is the same mismatch the
	// pinned session would report once its goroutine opens it.
	budget := refinementFilingBudgetFor(childCount)
	entry, started := s.refinementFiling.tryStart(approved.ID, req.Repo, childCount, budget)
	if !started {
		if entry.repo != req.Repo {
			s.writeError(w, r, http.StatusConflict, "refinement_filing_repo_mismatch",
				fmt.Sprintf("%v: an in-flight filing targets %q, requested %q", refinement.ErrFilingRepoMismatch, entry.repo, req.Repo),
				map[string]any{"requested_repo": req.Repo, "pinned_repo": entry.repo})
			return
		}
		resp.Status = refinementFilingStatusAlreadyInProgress
		resp.AlreadyInProgress = true
		resp.BudgetSeconds = int(entry.budget / time.Second)
		s.writeJSON(w, r, http.StatusAccepted, resp)
		return
	}

	// DETACHED and BOUNDED (the #3232 grooming-apply precedent): the filing
	// context keeps the request's values (the identity the completion audit
	// names) but neither its cancellation nor its deadline, bounded by the
	// child-scaled budget. The goroutine closes over only resolved values.
	job := refinementFilingJob{
		sessionID: sessionID,
		approved:  approved,
		repo:      req.Repo,
		owner:     owner,
		name:      name,
		conv:      conv,
		target:    target,
	}
	base := context.WithoutCancel(r.Context())
	s.bgRefinementFiling.Add(1)
	go func() {
		defer s.bgRefinementFiling.Done()
		ctx, cancel := context.WithTimeout(base, budget)
		defer cancel()
		s.runDetachedRefinementFiling(ctx, job)
	}()

	resp.Status = refinementFilingStatusInProgress
	resp.BudgetSeconds = int(budget / time.Second)
	s.writeJSON(w, r, http.StatusAccepted, resp)
}

// runDetachedRefinementFiling is the body of the detached filing goroutine: it
// runs the executor under ctx and records the outcome in the tracker (success
// clears the entry; a failure keeps last_error / failed_ordinal / step for the
// GET view).
func (s *Server) runDetachedRefinementFiling(ctx context.Context, job refinementFilingJob) {
	_, err := refinement.ExecuteFilingWith(ctx, job.approved, job.repo, s.cfg.RefinementRepo, s.refinementFilingDeps(job))
	if err == nil {
		s.refinementFiling.succeed(job.approved.ID)
		return
	}
	var (
		failedOrdinal *int
		step          string
		partial       *refinement.FilingPartialError
		finalizeErr   *refinementFinalizeError
	)
	switch {
	case errors.As(err, &partial):
		ord := partial.FailedOrdinal
		failedOrdinal, step = &ord, partial.Step
	case errors.As(err, &finalizeErr):
		step = finalizeErr.step
	}
	s.refinementFiling.fail(job.approved.ID, err.Error(), failedOrdinal, step)
	attrs := []slog.Attr{
		slog.String("session_id", job.sessionID.String()),
		slog.String("draft_id", job.approved.ID.String()),
		slog.String("repo", job.repo),
		slog.String("step", step),
		slog.String("error", err.Error()),
	}
	if failedOrdinal != nil {
		attrs = append(attrs, slog.Int("failed_ordinal", *failedOrdinal))
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
		"refinement filing failed; the filed items are durable — re-invoke to resume", attrs...)
}

// refinementFilingDeps wires the executor's seams for job: File rides the
// hand-filed pipeline (applyAndFileWorkItem, unchanged), Children and Link are
// the provider's optional EpicChildrenQuerier / EpicLinker capabilities (nil
// when unimplemented, which skips resume adoption / the link pass), and
// Finalize runs the session-closing side effects.
func (s *Server) refinementFilingDeps(job refinementFilingJob) refinement.FilingDeps {
	// The FileItem seam: draft items ride exactly the hand-filed pipeline. The
	// executor leaves {epic} UNSET (FilingRequestForChild passes "" for the title
	// var), so deriveEpicTitleVar derives the epic's DISCOVERED ordinal from the
	// just-filed parent epic's [E<n>] title (one GetIssue per child) — fixing the
	// #1644 issue-number-vs-ordinal bug.
	deps := refinement.FilingDeps{
		File: func(ctx context.Context, filing workmgmt.FilingRequest) (int, string, error) {
			_, created, werr := s.applyAndFileWorkItem(ctx, filing, job.conv, job.target, job.owner, job.name)
			if werr != nil {
				return 0, "", errors.New(werr.msg)
			}
			return created.Number, created.URL, nil
		},
		Finalize: func(ctx context.Context, outcome *refinement.FilingOutcome) error {
			return s.finalizeRefinementFiling(ctx, job, outcome)
		},
	}
	provider, err := workmgmt.Get(job.conv.Provider)
	if err != nil {
		return deps // File reports the unresolvable provider itself.
	}
	if q, ok := provider.(workmgmt.EpicChildrenQuerier); ok {
		deps.Children = func(ctx context.Context, epicNumber int) ([]workmgmt.EpicChild, error) {
			res, err := q.EpicChildren(ctx, workmgmt.EpicChildrenRequest{Target: job.target, Epic: "#" + strconv.Itoa(epicNumber)})
			if err != nil {
				return nil, err
			}
			return res.Children, nil
		}
	}
	if l, ok := provider.(workmgmt.EpicLinker); ok {
		deps.Link = func(ctx context.Context, epicNumber, childNumber int) error {
			return l.LinkToEpic(ctx, workmgmt.EpicLinkRequest{Target: job.target, Epic: "#" + strconv.Itoa(epicNumber), Child: childNumber})
		}
	}
	return deps
}

// finalizeRefinementFiling runs the session-closing side effects (verify →
// completion audit → completed_at flip) INSIDE the executor's per-draft
// advisory lock, so a concurrent filer cannot enter after the items are
// recorded but before completed_at is set and append a duplicate completion
// audit. It writes no HTTP response (the filing is detached): every failure is
// returned as a *refinementFinalizeError naming its step, which the goroutine
// records for the GET view. It is invoked only for a fresh/resumed full fill —
// never for an already-completed replay.
func (s *Server) finalizeRefinementFiling(ctx context.Context, job refinementFilingJob, outcome *refinement.FilingOutcome) error {
	verified, err := s.verifyFiledEpic(ctx, job.conv, job.target, outcome)
	if err != nil {
		return &refinementFinalizeError{step: refinementFilingStepVerify,
			err: fmt.Errorf("the filed epic #%d did not round-trip through provider verification (the items are durable — re-invoke to re-verify): %w", outcome.Epic.IssueNumber, err)}
	}
	hash, err := refinement.ContentHash(job.approved.Draft)
	if err != nil {
		return &refinementFinalizeError{step: refinementFilingStepAudit, err: fmt.Errorf("hash the approved draft: %w", err)}
	}
	if err := s.appendRefinementAuditCtx(ctx, "refinement_filing_completed", map[string]any{
		"session_id":    job.sessionID.String(),
		"draft_id":      job.approved.ID.String(),
		"content_hash":  hash,
		"repo":          job.repo,
		"epic_number":   outcome.Epic.IssueNumber,
		"child_numbers": childNumbers(outcome),
		"verified":      verified,
	}); err != nil {
		return &refinementFinalizeError{step: refinementFilingStepAudit,
			err: fmt.Errorf("append the refinement_filing_completed audit entry (the session stays open — re-invoke to retry the close): %w", err)}
	}
	if err := s.cfg.RefinementRepo.CompleteFilingSession(ctx, job.approved.ID); err != nil {
		return &refinementFinalizeError{step: refinementFilingStepComplete, err: fmt.Errorf("close the filing session: %w", err)}
	}
	return nil
}

// verifyFiledEpic resolves the provider and, when it implements the optional
// EpicChildrenQuerier capability, runs the filed-epic round-trip assertion. A
// provider WITHOUT the capability skips verification (verified=false, fail-open
// on a pure read-back — never on filing). A verification error is returned so
// finalize records it before the audit/complete close.
func (*Server) verifyFiledEpic(ctx context.Context, conv workmgmt.Conventions, target workmgmt.Target, outcome *refinement.FilingOutcome) (bool, error) {
	provider, err := workmgmt.Get(conv.Provider)
	if err != nil {
		// The provider must exist (filing just succeeded through it), but if it
		// cannot be resolved, skip verification fail-open — the items are durable.
		return false, nil
	}
	querier, ok := provider.(workmgmt.EpicChildrenQuerier)
	if !ok {
		return false, nil
	}
	if err := refinement.VerifyFiledEpic(ctx, querier, target, outcome.Epic.IssueNumber, outcome.FiledMap()); err != nil {
		return false, err
	}
	return true, nil
}

// filedItemViews renders recorded ledger rows (ordinal ASC) as the epic view
// (nil when the epic is unrecorded) and the child views.
func filedItemViews(items []*refinement.FiledItem) (*refinementFiledEpicView, []refinementFiledChildView) {
	var epic *refinementFiledEpicView
	children := make([]refinementFiledChildView, 0, len(items))
	for _, it := range items {
		if it.Ordinal == 0 {
			epic = &refinementFiledEpicView{Number: it.IssueNumber, URL: it.IssueURL}
			continue
		}
		children = append(children, refinementFiledChildView{Ordinal: it.Ordinal, Number: it.IssueNumber, URL: it.IssueURL})
	}
	return epic, children
}

// childNumbers is the child issue numbers (ordinal order) for the audit payload.
func childNumbers(o *refinement.FilingOutcome) []int {
	out := make([]int, 0, len(o.Children))
	for _, c := range o.Children {
		out = append(out, c.IssueNumber)
	}
	return out
}
