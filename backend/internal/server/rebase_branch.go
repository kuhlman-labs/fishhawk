package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryBranchRebased is the audit-log category for the entry the
// rebase-branch handler writes when it advances a run/PR branch onto its
// declared base (E64.23 / #3125). It is the durable record of an
// operator-gated, runner-performed lineage write, and it drives a sticky
// status-comment refresh (an issue-comment surface).
//
// MECHANISM, stated plainly because the verb's NAME is misleading: the
// forge REST API exposes NO rebase primitive, so the handler performs a
// forge-side MERGE OF THE BASE INTO THE RUN BRANCH
// (POST /repos/{o}/{r}/merges with base=<run branch>, head=<base ref>).
// That leaves a MERGE COMMIT on the run branch and does NOT produce linear
// history. No force-push and no operator write is involved — the App
// installation stays the sole writer under ADR-035.
const CategoryBranchRebased = "branch_rebased"

// rebaseMechanismNote is the constant sentence shipped on EVERY 200 from
// the rebase handler (and echoed into the audit payload), so a reader of
// the response cannot infer linear history from the verb's name. It is
// also asserted verbatim-by-substring by the MCP tool description pin, so
// the mechanism claim cannot drift between the API and the tool surface.
const rebaseMechanismNote = "fishhawk_rebase_run_branch merges the declared base INTO the run branch server-side, leaving a merge commit; this is not a literal rebase and does not produce linear history."

// rebaseBranchRequest is the JSON body of POST /v0/runs/{run_id}/rebase-branch.
// Confirm MUST be true — the verb moves the run branch's head (leaving a
// merge commit and dismissing head-bound approvals), so it is never
// silent/auto: a missing or false confirm returns 400. Reason is an
// operator note recorded on the audit entry.
type rebaseBranchRequest struct {
	Reason  string `json:"reason"`
	Confirm bool   `json:"confirm"`
}

// rebaseBranchResponse summarizes a successful base advance.
//
// Both PriorHeadSHA and NewHeadSHA are reported. NewHeadSHA is resolved by
// PROVENANCE (#4199): when MergeBranch decoded the merge commit sha
// (MergeCommitSHA), that sha IS the new head unless the bounded post-merge
// PR re-read observed a genuine concurrent push (a head that is neither the
// pre-merge head nor the merge commit), in which case the observed head is
// reported. MergeCommitSHA may legitimately be EMPTY (the deliberately benign
// undecodable-201 shape pinned by githubclient's
// TestMergeBranch_MergedMissingSHAIsBenign); only then does the re-read
// supply the head, and only when it differs from PriorHeadSHA — a read stuck
// at the pre-merge head leaves NewHeadSHA empty. An empty MergeCommitSHA is
// never read as "already up to date": the behind-probe decides that.
type rebaseBranchResponse struct {
	RunID          string `json:"run_id"`
	PRNumber       int    `json:"pr_number"`
	Branch         string `json:"branch"`
	BaseRef        string `json:"base_ref"`
	PriorHeadSHA   string `json:"prior_head_sha"`
	NewHeadSHA     string `json:"new_head_sha"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	// AlreadyUpToDate reports that the run branch ALREADY contained the
	// declared base, so no merge was attempted on this invocation. It is
	// still a 200 that re-parks the gate and republishes the check at the
	// current head — that is what makes the retry the republish warning
	// advertises genuinely true.
	AlreadyUpToDate       bool   `json:"already_up_to_date"`
	ReparkedReviewStageID string `json:"reparked_review_stage_id,omitempty"`
	// MechanismNote is a constant sentence shipped on every 200 stating the
	// merge-commit mechanism, so no reader of this response can infer that a
	// literal rebase produced linear history.
	MechanismNote string `json:"mechanism_note"`
	// AuditCheckRepublished reports whether the fishhawk_audit_complete Check
	// Run was successfully re-posted at the new head. FALSE when the re-post
	// did not land — it errored, no publisher is wired, or the new head could
	// not be resolved at all, which happens only when the merge sha did not
	// decode (see AuditCheckRepublishWarning).
	AuditCheckRepublished bool `json:"audit_check_republished"`
	// AuditCheckRepublishWarning, when non-empty, names why the re-post did
	// not land and names re-invoking this verb as the idempotent retry.
	AuditCheckRepublishWarning string `json:"audit_check_republish_warning,omitempty"`
	// LineageAttributionWarning, when non-empty, reports that the merge
	// SUCCEEDED but its ADR-035 reported-head attribution is INCOMPLETE, so
	// this 200 must NOT be read as a clean recovery. It covers three cases,
	// each of which leaves the run wedged on the lineage check until the
	// operator acts:
	//
	//   - a CONCURRENT PUSH: after the bounded post-merge re-read the head is
	//     neither the pre-merge head nor the merge commit this invocation
	//     created, so the divergent head was deliberately NOT attributed
	//     (attributing it would launder a foreign commit into the ledger). A
	//     re-read stuck at the pre-merge head is read-after-write lag, not a
	//     concurrent push (#4199), and warns nothing;
	//   - the attribution APPEND FAILED to persist;
	//   - NOTHING was attributable at all (an undecodable merge sha AND a
	//     post-merge re-read that failed or stayed at the pre-merge head).
	//
	// In the last two cases re-invoking this verb does NOT repair the
	// attribution — the retry takes the already-contains-base arm, which
	// deliberately attributes nothing — so the warning names
	// fishhawk_vouch_commit as the required step instead.
	LineageAttributionWarning string `json:"lineage_attribution_warning,omitempty"`
	// PostMergeHeadRead classifies the bounded post-merge PR head re-read
	// (#4199): converged, read_after_write_lag, concurrent_push, unreadable or
	// read_back. Set only when THIS call performed a merge.
	PostMergeHeadRead string `json:"post_merge_head_read,omitempty"`
	// PostMergeHeadReadNote explains a degraded classification
	// (read_after_write_lag, unreadable): what was observed and what the head
	// was anchored on.
	PostMergeHeadReadNote string `json:"post_merge_head_read_note,omitempty"`

	// --- 202 conflict-resolution trigger arm (E64.62 / #3202) ---
	//
	// These three fields are what make a 202 LEGIBLE. The response body of a
	// triggered pass is otherwise shaped exactly like a 200 with an empty
	// new_head_sha, and an MCP client decodes 202 and 200 identically — so
	// without them a triggered pass would read as a silent success that
	// advanced nothing, which is strictly worse than the loud refusal it
	// replaces.

	// ConflictResolutionTriggered is true ONLY on the 202: the base merge
	// CONFLICTED, nothing was written to the branch, and a bounded
	// agent-driven conflict-resolution pass was authorized instead.
	ConflictResolutionTriggered bool `json:"conflict_resolution_triggered,omitempty"`
	// ConflictResolutionStageID is the re-opened implement stage to await.
	ConflictResolutionStageID string `json:"conflict_resolution_stage_id,omitempty"`
	// ConflictResolutionPass is the 1-based pass ordinal, against a ceiling of
	// conflictResolutionCeiling.
	ConflictResolutionPass int `json:"conflict_resolution_pass,omitempty"`
	// ConflictResolutionNote is the constant sentence stating that nothing was
	// written by this call and naming the await-then-re-invoke route.
	ConflictResolutionNote string `json:"conflict_resolution_note,omitempty"`

	// --- merge-candidate verify (ADR-090 D3 / #4018) ---
	//
	// A performed base merge produces a head no runner gated on the
	// committed tree, so the 200 authorizes a verify-only pass for it; an
	// already-up-to-date invocation re-triggers one when the live head is a
	// base-advance or conflict-resolution head with no passing verdict. A
	// pass that cannot start never fails the rebase: the refusal rides on
	// this 200 and the merge gate later answers merge_candidate_unverified.

	// MergeCandidateVerifyState is the live head's merge-candidate state
	// after this call: not_required, unverified, in_flight, passed or failed.
	MergeCandidateVerifyState string `json:"merge_candidate_verify_state,omitempty"`
	// MergeCandidateVerifyTriggered is true when THIS call appended a
	// verify-only pass trigger and re-opened the implement stage.
	MergeCandidateVerifyTriggered bool `json:"merge_candidate_verify_triggered,omitempty"`
	// MergeCandidateVerifyStageID is the re-opened implement stage to
	// dispatch and await, set whenever a pass is live for the head.
	MergeCandidateVerifyStageID string `json:"merge_candidate_verify_stage_id,omitempty"`
	// MergeCandidateVerifyNote names the next step for a live pass.
	MergeCandidateVerifyNote string `json:"merge_candidate_verify_note,omitempty"`
	// MergeCandidateVerifyRefusal names why no pass could be started or why
	// the head's state could not be read.
	MergeCandidateVerifyRefusal string `json:"merge_candidate_verify_refusal,omitempty"`
}

// mergeCandidateVerifyTriggeredNote is the constant sentence shipped whenever
// a verify-only pass is live for the head after this call.
const mergeCandidateVerifyTriggeredNote = "A verify-only merge-candidate pass (ADR-090) is authorized for this head: the implement stage is re-opened and the runner fetches the run-branch tip, runs ONLY the declared verify command in full form in the isolated gate, and reports the result. The pass writes NOTHING — no commit, no push. On a local runner, dispatch the stage with fishhawk_dispatch_stage and await it with fishhawk_await_stage; fishhawk_merge_run refuses this head until the pass reports passed."

// mergeCandidateUnreadableHeadNote is shipped when a performed merge's new
// head could not be resolved — only when the merge sha did not decode and the
// post-merge re-read failed or stayed at the pre-merge head — so no pass could
// be anchored.
const mergeCandidateUnreadableHeadNote = "the base merge SUCCEEDED but its resulting head could not be resolved (the merge sha did not decode and the post-merge re-read returned no new head), so no merge-candidate verify pass was anchored; re-invoke fishhawk_rebase_run_branch — the retry takes the already-up-to-date arm and triggers the pass for the live head"

// rebaseMergeCandidateFields is the merge-candidate block of a 200.
type rebaseMergeCandidateFields struct {
	State     string
	Triggered bool
	StageID   string
	Note      string
	Refusal   string
}

// handleRebaseRunBranch implements POST /v0/runs/{run_id}/rebase-branch.
//
// It closes the half of #3109's Done-means that #3125 left open: an
// operator whose run branch has fallen BEHIND its declared base no longer
// has to resolve in a worktree and PUSH to a runner-owned branch. The
// RUNNER (the App installation, sole writer under ADR-035) advances its own
// lineage branch; the operator authorizes rather than performs the write.
//
// MECHANISM (see CategoryBranchRebased): a forge-side merge of the base
// INTO the run branch. It leaves a merge commit; it is not a literal
// rebase. Every operator-facing surface — the tool description, this
// response's mechanism_note, the OpenAPI text and both READMEs — states
// this, so the verb's name cannot mislead.
//
// Auth is operator-token-only, mirroring handleVouchCommit rather than
// reset-branch's softer subject-binding:
//
//   - anonymous → 401 authentication_required;
//   - a run-bound MCP token ("mcp:run:<uuid>") is REJECTED OUTRIGHT (403
//     run_token_forbidden), EVEN FOR ITS OWN RUN. Advancing a branch onto a
//     new base is a lineage-moving write the ADR-035 sole-writer invariant
//     reserves to an operator authorization; an agent self-authorizing it
//     would defeat exactly the invariant this verb exists to preserve;
//   - any identity without write:stages → 403 insufficient_scope, enforced
//     UNCONDITIONALLY with no cookie-session bypass.
//
// A CONFLICTING merge no longer fails closed outright (E64.62 / #3202).
// It takes the BUDGETED 202 TRIGGER ARM: still no write to the branch, but
// a bounded, agent-driven conflict-resolution pass (ceiling 1, its OWN
// audit counter — never the fix-up budget) is authorized, the implement
// stage is re-opened, and the response carries
// conflict_resolution_triggered plus the stage to await. At or over the
// ceiling — and whenever no pass can be started at all — the response is
// TODAY'S fail-closed 422 rebase_conflict, now naming the FAILED pass, its
// refusal reason and the resolve-push-vouch route, and still cross-linking
// fishhawk_reset_run_branch (the sibling verb for a foreign commit pushed
// ON TOP, a different problem).
//
// Two structural properties are load-bearing and deliberately NOT
// shortcuts:
//
//   - THE BEHIND-PROBE. "The branch already contains the base" is decided
//     BEFORE any merge, by CompareCommits(base=<live run branch head>,
//     head=<base ref>). GitHub's three-dot compare returns the commits on
//     head since its merge base with base, so zero commits ⟺ the base ref
//     is already an ancestor of the run branch head. Nothing here reads
//     MergeBranch's ambiguous ("", nil) as a discriminator, and
//     githubclient.MergeBranch / forge.Forge / the GitLab stub are
//     untouched.
//   - THE SHARED TAIL. The already-contains-base arm is a 200 that STILL
//     re-parks the gate and republishes the check at the current head. Both
//     arms fall into ONE re-park + audit + republish + notify path with a
//     resolved (newHead, mergeSHA, alreadyUpToDate) triple, so the retry the
//     republish warning advertises is real: invocation 1 merges and fails to
//     publish; the operator re-invokes; the probe short-circuits here and the
//     required check IS published at the correct post-merge head.
//
// THE POST-MERGE HEAD (#4199). The forge's PR head lags the branch-ref
// update, so a single post-merge read routinely returns the PRE-merge head.
// The handler therefore re-reads boundedly (readPostMergeHead), classifies
// what it saw, and resolves the new head by provenance
// (resolvePostMergeHead): a decoded merge sha anchors the check, the
// branch_rebased row and the verify pass unless a genuine concurrent push was
// observed. The classification ships as post_merge_head_read.
//
// After the shared tail a 200 also authorizes the ADR-090 merge-candidate
// verify pass (rebaseMergeCandidateVerify): a performed merge triggers a
// verify-only pass for the resolved new head, and an already-up-to-date call
// re-triggers one for an unverified base-advance or conflict-resolution head.
// The pass re-opens the implement stage but writes nothing to the branch, and
// a pass that cannot start rides on the 200 as merge_candidate_verify_refusal.
//
// THE SPLIT (ADR-092 / #4200). This handler is the HTTP SHELL: auth, the
// body, confirm, the run read, the refusal → status mapping, the conflict
// 202/422 arm and the merge-candidate producer. Everything between the run
// read and the response — the determinability ladder, the behind-probe, the
// lease re-check, MergeBranch, the bounded post-merge read, the detached tail,
// the re-park, branch_rebased, the lineage attribution, the republish and the
// notify — is s.advanceRunBranch, which takes the audit ACTOR as a parameter
// so the merge-candidate queue can auto-advance an admitted PR under the
// system actor through the SAME machinery the operator verb uses.
func (s *Server) handleRebaseRunBranch(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	// Operator-token-only: a run-bound agent token may NEVER advance its own
	// branch onto a new base, not even for its own run.
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not rebase its own run branch; advancing a run branch onto a new base is an operator action (ADR-035 sole-writer invariant)",
			nil)
		return
	}
	// Enforced UNCONDITIONALLY — no `id.TokenID != ""` cookie-session bypass.
	// A cookie session or any authenticated-but-unscoped identity must not be
	// able to move a run branch's head either.
	if !hasScope(id, "write:stages") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:stages",
			map[string]any{"required_scope": "write:stages"})
		return
	}

	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil || s.cfg.GitHub == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "rebase_unconfigured",
			"rebase-branch endpoint requires run + audit repositories and a GitHub client", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	var reqBody rebaseBranchRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(r.Body).Decode(&reqBody); decErr != nil && !errors.Is(decErr, io.EOF) {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {reason, confirm}",
				map[string]any{"error": decErr.Error()})
			return
		}
	}
	if !reqBody.Confirm {
		s.writeError(w, r, http.StatusBadRequest, "confirmation_required",
			"rebase-branch merges the declared base INTO the run branch, leaving a merge commit and moving the PR head; resend with confirm=true to proceed",
			map[string]any{"field": "confirm"})
		return
	}

	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return
	}

	out, refusal := s.advanceRunBranch(r.Context(), runRow, rebaseOperatorActor(r.Context()), reqBody.Reason)
	if refusal != nil {
		s.writeRebaseAdvanceRefusal(w, r, runID, refusal)
		return
	}

	// The merge-candidate producer below runs on the SAME detached tail the
	// core used: once THIS call performed the merge, the pass trigger must
	// not depend on the caller still listening either. Re-deriving the
	// context at the core's tail deadline keeps the ONE rebasePostMergeTailBudget
	// bound across the core's tail and this producer.
	if !out.TailDeadline.IsZero() {
		tailCtx, cancelTail := context.WithDeadline(context.WithoutCancel(r.Context()), out.TailDeadline)
		defer cancelTail()
		r = r.WithContext(tailCtx)
	}

	// MERGE-CANDIDATE VERIFY (ADR-090 D3), after the shared tail so the
	// branch_rebased row the state predicate classifies on is already on the
	// chain and the review gate is already re-parked.
	mc := s.rebaseMergeCandidateVerify(r, runRow, out.Branch, out.BaseRef, out.PriorHeadSHA, out.NewHeadSHA, out.MergePerformed)

	s.writeJSON(w, r, http.StatusOK, rebaseBranchResponse{
		RunID:                      runID.String(),
		PRNumber:                   out.PRNumber,
		Branch:                     out.Branch,
		BaseRef:                    out.BaseRef,
		PriorHeadSHA:               out.PriorHeadSHA,
		NewHeadSHA:                 out.NewHeadSHA,
		MergeCommitSHA:             out.MergeCommitSHA,
		AlreadyUpToDate:            out.AlreadyUpToDate,
		ReparkedReviewStageID:      out.ReparkedReviewStageID,
		MechanismNote:              rebaseMechanismNote,
		AuditCheckRepublished:      out.AuditCheckRepublished,
		AuditCheckRepublishWarning: out.AuditCheckRepublishWarning,
		LineageAttributionWarning:  out.LineageAttributionWarning,
		PostMergeHeadRead:          out.PostMergeHeadRead,
		PostMergeHeadReadNote:      out.PostMergeHeadReadNote,

		MergeCandidateVerifyState:     mc.State,
		MergeCandidateVerifyTriggered: mc.Triggered,
		MergeCandidateVerifyStageID:   mc.StageID,
		MergeCandidateVerifyNote:      mc.Note,
		MergeCandidateVerifyRefusal:   mc.Refusal,
	})
}

// writeRebaseAdvanceRefusal maps a typed advanceRunBranch refusal onto the
// verb's HTTP answers:
//
//   - not_determinable → 422 rebase_not_determinable naming the reason;
//   - conflict → THE 202 TRIGGER ARM (E64.62 / #3202). A conflict is no
//     longer an outright refusal: under a ceiling of ONE, the implement stage
//     is re-opened for a bounded, agent-driven conflict-resolution pass that
//     performs the merge LOCALLY on the run branch and pushes through the App
//     installation, so the operator never has to push to a branch ADR-035
//     declares runner-owned. NOTHING is written to the branch by THIS call
//     either way. When the budget is spent — or no pass can be started at
//     all — the fail-closed 422 rebase_conflict is today's behaviour, naming
//     the failed pass and its reason;
//   - merge_failed → 502 rebase_merge_failed (nothing was written).
func (s *Server) writeRebaseAdvanceRefusal(w http.ResponseWriter, r *http.Request,
	runID uuid.UUID, ref *rebaseAdvanceRefusal) {
	switch ref.Kind {
	case rebaseAdvanceRefusalConflict:
		start, refusal := s.startConflictResolutionPass(r, runID, ref.Branch, ref.BaseRef, ref.HeadSHA)
		if refusal != nil {
			s.writeRebaseConflictRefusal(w, r, refusal, ref.Branch, ref.BaseRef, ref.Err)
			return
		}
		s.writeJSON(w, r, http.StatusAccepted, rebaseBranchResponse{
			RunID:                       runID.String(),
			PRNumber:                    ref.PRNumber,
			Branch:                      ref.Branch,
			BaseRef:                     ref.BaseRef,
			PriorHeadSHA:                ref.HeadSHA,
			MechanismNote:               rebaseMechanismNote,
			ConflictResolutionTriggered: true,
			ConflictResolutionStageID:   start.StageID.String(),
			ConflictResolutionPass:      start.Pass,
			ConflictResolutionNote:      conflictResolutionTriggeredNote,
		})
	case rebaseAdvanceRefusalMergeFailed:
		s.writeError(w, r, http.StatusBadGateway, "rebase_merge_failed",
			"merging the declared base into the run branch failed; nothing was written",
			map[string]any{"branch": ref.Branch, "base_ref": ref.BaseRef, "error": ref.Err.Error()})
	default:
		s.writeRebaseNotDeterminable(w, r, ref.Reason)
	}
}

// rebaseOperatorActor is the audit actor of an operator-invoked rebase: the
// authenticated subject (anonymous when absent) under the user kind.
func rebaseOperatorActor(ctx context.Context) mergeCandidateActor {
	subject := IdentityFrom(ctx).Subject
	if subject == "" {
		subject = "anonymous"
	}
	return mergeCandidateActor{Kind: audit.ActorUser, Subject: subject}
}

// Typed advanceRunBranch refusal kinds. Every refusal is classified BEFORE
// anything is written: not_determinable before any merge, conflict and
// merge_failed when the merges endpoint itself refuses.
const (
	// rebaseAdvanceRefusalNotDeterminable: an anchor could not be resolved
	// with certainty (installation, repo, PR, head/branch/base, the
	// behind-probe or the lease re-check), so no merge was attempted.
	rebaseAdvanceRefusalNotDeterminable = "not_determinable"
	// rebaseAdvanceRefusalConflict: the base merge CONFLICTED; nothing was
	// written to the branch.
	rebaseAdvanceRefusalConflict = "conflict"
	// rebaseAdvanceRefusalMergeFailed: the merges endpoint failed for a
	// non-conflict reason; nothing was written to the branch.
	rebaseAdvanceRefusalMergeFailed = "merge_failed"
)

// rebaseAdvanceRefusal is a typed advanceRunBranch refusal. Reason is set on
// not_determinable; PRNumber, Branch, BaseRef, HeadSHA (the live head the
// merge was attempted against) and Err (the MergeBranch error) are set on
// conflict and merge_failed, so a caller can start a conflict-resolution pass
// or name the failure without re-reading the forge.
type rebaseAdvanceRefusal struct {
	Kind     string
	Reason   string
	PRNumber int
	Branch   string
	BaseRef  string
	HeadSHA  string
	Err      error
}

func rebaseNotDeterminable(reason string) *rebaseAdvanceRefusal {
	return &rebaseAdvanceRefusal{Kind: rebaseAdvanceRefusalNotDeterminable, Reason: reason}
}

// rebaseAdvanceOutcome is a successful advanceRunBranch: the run branch now
// contains its declared base, either because THIS call merged it
// (MergePerformed) or because it already did (AlreadyUpToDate). The fields
// mirror the 200's rebaseBranchResponse block of the same names.
type rebaseAdvanceOutcome struct {
	PRNumber       int
	Branch         string
	BaseRef        string
	PriorHeadSHA   string
	NewHeadSHA     string
	MergeCommitSHA string
	// AlreadyUpToDate: the behind-probe found the base already contained, so
	// no merge was attempted.
	AlreadyUpToDate bool
	// MergePerformed: THIS call merged the base into the run branch.
	MergePerformed             bool
	ReparkedReviewStageID      string
	AuditCheckRepublished      bool
	AuditCheckRepublishWarning string
	LineageAttributionWarning  string
	PostMergeHeadRead          string
	PostMergeHeadReadNote      string
	// TailDeadline is the deadline of the detached post-merge tail, set only
	// when MergePerformed. A caller that does further post-merge work (the
	// handler's merge-candidate producer) re-derives a detached context at
	// this SAME deadline, so one rebasePostMergeTailBudget bounds it all.
	TailDeadline time.Time
}

// advanceRunBranch is the actor-parameterized core of the rebase verb: it
// advances runRow's PR branch onto its declared base by a forge-side MERGE OF
// THE BASE INTO THE RUN BRANCH (see CategoryBranchRebased) and records the
// advance under actor. handleRebaseRunBranch calls it with the invoking
// operator; the ADR-092 merge-candidate queue calls it with
// mergeCandidateSystemActor() to auto-advance an admitted PR that fell
// behind. reason is recorded on the branch_rebased entry.
//
// It returns EITHER an outcome OR a typed refusal, never both. A refusal is
// always classified BEFORE anything is written (see the refusal kinds); the
// conflict-resolution 202 arm is the CALLER's decision, so the core never
// re-opens a stage. ctx bounds every read up to and including the post-merge
// re-read; once a merge has been performed the tail runs on a context
// detached from ctx's cancellation (WithoutCancel keeps its values), bounded
// by rebasePostMergeTailBudget.
func (s *Server) advanceRunBranch(ctx context.Context, runRow *run.Run,
	actor mergeCandidateActor, reason string) (*rebaseAdvanceOutcome, *rebaseAdvanceRefusal) {
	// A non-HTTP caller has no 503 in front of it, so the core refuses an
	// unwired server or a missing run itself rather than panicking.
	if runRow == nil || s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil || s.cfg.GitHub == nil {
		return nil, rebaseNotDeterminable("a base advance requires a run, run + audit repositories and a GitHub client")
	}
	// Mirrors startMergeCandidateVerifyPass: every row this core appends is
	// attributed, so an actor with no subject is refused before any write.
	if actor.Subject == "" {
		return nil, rebaseNotDeterminable("no actor was supplied for the base advance")
	}
	runID := runRow.ID

	// Determinability ladder. Every unresolvable anchor is a fail-CLOSED
	// refusal — never a merge on an uncertain read.
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return nil, rebaseNotDeterminable("run has no installation to authorize a GitHub merge")
	}
	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return nil, rebaseNotDeterminable("run repo is unparseable: " + err.Error())
	}
	prNumber := parsePRNumberFromURL(runRow.PullRequestURL)
	if prNumber <= 0 {
		return nil, rebaseNotDeterminable("run has no tracked pull request to rebase")
	}
	pr, err := s.cfg.GitHub.GetPullRequest(ctx, scope, repo, prNumber)
	if err != nil {
		return nil, rebaseNotDeterminable("resolve live PR head failed: " + err.Error())
	}
	headSHA, branch, baseRef := pr.HeadSHA, pr.HeadRef, pr.BaseRef
	if headSHA == "" || branch == "" || baseRef == "" {
		return nil, rebaseNotDeterminable("PR returned an empty head sha, branch or base ref")
	}

	// THE BEHIND-PROBE, taken BEFORE any merge. Three-dot compare
	// base=<live run branch head> ... head=<base ref> returns exactly the
	// commits the base advanced by that the run branch does not yet contain.
	// An error fails CLOSED — never a merge on an uncertain read.
	behind, err := s.cfg.GitHub.CompareCommits(ctx, scope, repo, headSHA, baseRef)
	if err != nil {
		return nil, rebaseNotDeterminable("behind-probe compare failed: " + err.Error())
	}

	out := &rebaseAdvanceOutcome{
		PRNumber:        prNumber,
		Branch:          branch,
		BaseRef:         baseRef,
		PriorHeadSHA:    headSHA,
		NewHeadSHA:      headSHA,
		AlreadyUpToDate: len(behind) == 0,
	}
	// postMerge is the classified post-merge head read; nil unless THIS call
	// performed a merge.
	var postMerge *postMergeHeadRead

	if !out.AlreadyUpToDate {
		// LEASE RE-CHECK — the only TOCTOU guard (the merges API has no
		// compare-and-swap). Re-read the live head and abort if it moved
		// since the probe, so a racing push is never silently merged over.
		livePR, lerr := s.cfg.GitHub.GetPullRequest(ctx, scope, repo, prNumber)
		if lerr != nil {
			return nil, rebaseNotDeterminable("lease re-check: re-read live PR head failed: " + lerr.Error())
		}
		if livePR.HeadSHA != headSHA {
			return nil, rebaseNotDeterminable(
				"lease re-check: the live PR head changed since the behind-probe (concurrent push); rebase aborted")
		}

		// DIRECTION: the merges API's `base` is the branch that RECEIVES the
		// merge and `head` is the branch merged in. So base is the RUN BRANCH
		// and head is the BASE REF. An inversion here would merge the run
		// branch into the base branch — the worst failure available to this
		// verb — which is why the captured request body is asserted in test.
		msg := fmt.Sprintf("Advance run branch %s onto %s (fishhawk_rebase_run_branch, run %s)",
			branch, baseRef, runID.String())
		sha, merr := s.cfg.GitHub.MergeBranch(ctx, scope, repo, branch, baseRef, msg)
		if merr != nil {
			kind := rebaseAdvanceRefusalMergeFailed
			if errors.Is(merr, forge.ErrMergeConflict) {
				kind = rebaseAdvanceRefusalConflict
			}
			return nil, &rebaseAdvanceRefusal{Kind: kind, PRNumber: prNumber,
				Branch: branch, BaseRef: baseRef, HeadSHA: headSHA, Err: merr}
		}
		out.MergePerformed = true
		out.MergeCommitSHA = sha

		// THE POST-MERGE HEAD (#4199): a bounded, classified re-read, with the
		// new head resolved by PROVENANCE rather than taken from one read. The
		// forge's PR head lags the branch-ref update, so trusting a single
		// read anchored everything on the PRE-merge head and misreported the
		// lag as a concurrent push.
		pm := readPostMergeHead(ctx, func(rctx context.Context) (string, error) {
			p, rerr := s.cfg.GitHub.GetPullRequest(rctx, scope, repo, prNumber)
			if rerr != nil {
				return "", rerr
			}
			return p.HeadSHA, nil
		}, headSHA, sha, s.postMergeHeadReadSchedule())
		postMerge = &pm
		out.NewHeadSHA = resolvePostMergeHead(pm, sha)
		if pm.Outcome == postMergeHeadReadReadAfterWriteLag || pm.Outcome == postMergeHeadReadUnreadable {
			lastErr := ""
			if pm.LastErr != nil {
				lastErr = pm.LastErr.Error()
			}
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"branch rebase: post-merge PR head read did not converge on the merge commit",
				slog.String("run_id", runID.String()),
				slog.String("outcome", pm.Outcome),
				slog.String("prior_head_sha", headSHA),
				slog.String("merge_commit_sha", sha),
				slog.Int("attempts", pm.Attempts),
				slog.String("last_error", lastErr))
		}
		if out.NewHeadSHA == "" {
			// Reachable ONLY when the merge sha did not decode: the merge
			// ALREADY happened, so a refusal here would misreport a completed
			// write. Return 200 with a warning instead — and deliberately do
			// NOT fall back to publishing at "no override" or at a read stuck
			// on the pre-merge head: both resolve to the pre-merge head, which
			// is precisely the staleness this verb exists to remove. Skipping
			// publication and relying on the idempotent retry is strictly
			// safer than pinning the required check to a stale head.
			out.AuditCheckRepublishWarning = "the base merge SUCCEEDED, but the resulting head could not be read back (" +
				postMergeHeadUnresolvedReason(pm, headSHA) +
				"), so the fishhawk_audit_complete check was NOT re-posted — publishing at the pre-merge head would pin the required check to a stale sha. Re-invoke fishhawk_rebase_run_branch to retry the re-post; the branch now contains the base, so the retry short-circuits the merge and publishes at the correct head."
		}
	}

	// DETACH THE POST-MERGE TAIL. Once THIS call performed the merge, the
	// commit is on the branch and nothing unwinds it, so the re-park, the
	// branch_rebased row, the lineage attribution, the republish and the
	// merge-candidate pass must not depend on the caller still listening. The
	// MCP client's 30s timeout can cancel the request while the bounded
	// post-merge re-read runs against a degraded forge, and on a cancelled
	// context every append below would fail AFTER the installation-authored
	// merge landed — no branch_rebased row and no attribution, the
	// wedged-FOREIGN state the attribution exists to prevent. WithoutCancel
	// keeps the identity values; rebasePostMergeTailBudget bounds the tail.
	// Every call below reads ctx, so this one rebind covers them all. The
	// re-read above stays on the caller's context, so a departed caller ends
	// the reads early rather than extending them. The deadline is returned on
	// the outcome so the caller's own post-merge work shares the bound.
	if out.MergePerformed {
		out.TailDeadline = time.Now().Add(rebasePostMergeTailBudget)
		tailCtx, cancelTail := context.WithDeadline(context.WithoutCancel(ctx), out.TailDeadline)
		defer cancelTail()
		ctx = tailCtx
	}

	// --- SHARED TAIL: re-park → audit → attribute → republish → notify ---
	// Reachable WITHOUT a merge on this invocation, which is what makes the
	// advertised retry true.

	if reparked, rerr := s.reparkReviewGateAfterHeadMove(ctx, runID); rerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch rebase: re-park review gate failed (best-effort)",
			slog.String("run_id", runID.String()),
			slog.String("error", rerr.Error()))
	} else if reparked != nil {
		out.ReparkedReviewStageID = reparked.ID.String()
	}

	// The branch_rebased entry is appended BEFORE the recompute so the
	// recompute observes it.
	s.writeBranchRebasedAudit(ctx, actor, runID, prNumber, branch, baseRef,
		headSHA, out.NewHeadSHA, out.MergeCommitSHA, out.AlreadyUpToDate, reason, out.ReparkedReviewStageID, postMerge)

	// LINEAGE ATTRIBUTION (E64.23 / #3125). The merge commit this verb
	// creates is authored by the App installation but appears in NO
	// head-report audit category, so the ADR-035 reported-head ledger
	// (lineage.go buildReportedHeadLedger) would read it as FOREIGN and wedge
	// the very run this verb just un-wedged — verified by executing
	// TestRebaseRunBranch_LedgerAttributesTheMergeCommit against the REAL
	// ReverifyBranchLineage recompute, which goes RED without this call. The
	// fix is the confined one the vouch path already uses: union the SHAs into
	// the ledger via an operator_commit_vouched declaration, which
	// addVouchedSHAs reads.
	//
	// Attribution is written ONLY when THIS invocation performed the merge,
	// and covers EXACTLY ONE sha: the merge commit whose provenance this call
	// can prove. The already-contains-base arm deliberately attributes
	// NOTHING, and a post-merge head the bounded re-read classified as a
	// concurrent push is likewise not attributed — vouching whatever head
	// happens to be live
	// would silently launder a genuinely foreign pushed commit and defeat the
	// fail-closed property that makes reset-branch and vouch-commit
	// meaningful. An incomplete attribution is surfaced on the response as
	// lineage_attribution_warning, so a 200 is never read as a clean recovery
	// while the run is still wedged on the lineage check.
	if out.MergePerformed && postMerge != nil {
		out.LineageAttributionWarning = s.writeRebaseLineageAttribution(ctx, actor, runID, branch, baseRef,
			out.MergeCommitSHA, out.NewHeadSHA, *postMerge)
		out.PostMergeHeadRead = postMerge.Outcome
		out.PostMergeHeadReadNote = postMergeHeadReadNote(*postMerge, headSHA, out.MergeCommitSHA)
	}

	if out.NewHeadSHA != "" {
		republished, pubErr := s.recomputeAndPublishAuditCompleteAtHead(ctx, runID, out.NewHeadSHA)
		out.AuditCheckRepublished = republished
		if pubErr != nil {
			out.AuditCheckRepublishWarning = "the base advance is recorded and durable, but re-posting the fishhawk_audit_complete check at the new head failed; the required check may be absent from the merge head — re-invoke fishhawk_rebase_run_branch to retry the re-post: " + pubErr.Error()
		}
	}

	s.notifyStatusUpdate(ctx, runID, "branch_rebased")
	return out, nil
}

// rebaseMergeCandidateVerify is the rebase verb's merge-candidate producer.
//
//   - A PERFORMED merge triggers a pass for the resolved new head (cause
//     base_advance): the decoded merge sha, or the observed head on a genuine
//     concurrent push (#4199). Only when the merge sha did not decode AND the
//     post-merge re-read returned no new head is nothing anchored; the retry
//     then takes the already-up-to-date arm below.
//   - An ALREADY-UP-TO-DATE invocation reads mergeCandidateVerifyState for
//     the live head and re-triggers only when it is unverified (a
//     conflict-resolution push, or a base advance whose pass never reported
//     a verdict); passed, failed, in_flight and not_required start nothing.
//
// The actor is the OPERATOR who invoked the verb. Nothing here fails the
// rebase: a refusal is carried on the 200.
func (s *Server) rebaseMergeCandidateVerify(r *http.Request, runRow *run.Run,
	branch, baseRef, priorHead, newHead string, mergePerformed bool) rebaseMergeCandidateFields {
	ctx := r.Context()
	if mergePerformed && newHead == "" {
		return rebaseMergeCandidateFields{Refusal: mergeCandidateUnreadableHeadNote}
	}
	head, cause := newHead, mergeCandidateCauseBaseAdvance
	if !mergePerformed {
		head = priorHead
		st, err := s.mergeCandidateVerifyState(ctx, runRow, head)
		if err != nil {
			return rebaseMergeCandidateFields{Refusal: "the merge-candidate verify state of " + head +
				" could not be read (" + err.Error() + "), so no pass was triggered; re-invoke fishhawk_rebase_run_branch to retry"}
		}
		if st.State != mergeCandidateStateUnverified {
			out := rebaseMergeCandidateFields{State: st.State}
			if st.State == mergeCandidateStateInFlight {
				out.StageID = st.StageID.String()
				out.Note = mergeCandidateVerifyTriggeredNote
			}
			return out
		}
		cause = st.Cause
	}

	start, refusal := s.startMergeCandidateVerifyPass(ctx, runRow.ID, mergeCandidateVerifyParams{
		Branch:  branch,
		BaseRef: baseRef,
		HeadSHA: head,
		Cause:   cause,
	}, rebaseOperatorActor(ctx))
	if refusal != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch rebase: merge-candidate verify pass could not be started",
			slog.String("run_id", runRow.ID.String()),
			slog.String("head_sha", head),
			slog.String("reason", refusal.Reason))
		return rebaseMergeCandidateFields{State: mergeCandidateStateUnverified, Refusal: refusal.Reason}
	}
	out := rebaseMergeCandidateFields{State: start.State, Triggered: start.Triggered}
	if start.State == mergeCandidateStateInFlight {
		out.StageID = start.StageID.String()
		out.Note = mergeCandidateVerifyTriggeredNote
	}
	return out
}

// writeRebaseNotDeterminable is the fail-CLOSED refusal: a 422 carrying the
// reason no merge could be attempted with certainty, so the operator learns
// WHY nothing was written. Mirrors writeResetNotDeterminable.
func (s *Server) writeRebaseNotDeterminable(w http.ResponseWriter, r *http.Request, reason string) {
	s.writeError(w, r, http.StatusUnprocessableEntity, "rebase_not_determinable",
		"cannot determine a safe base advance with certainty; refusing to write: "+reason,
		nil)
}

// writeBranchRebasedAudit appends the branch_rebased audit entry recording
// the full action — the prior head, the resolved new head, the merge commit
// (which may legitimately be empty), whether the branch already contained the
// base, the reason, and the mechanism note — so the advance is auditable. On
// a performed merge (postMerge non-nil) it also records the post-merge read
// classification, the last observed head and the read count (#4199). The
// entry is attributed to actor: the invoking operator on the verb, the system
// actor on a merge-candidate queue auto-advance — never an unattributed
// action. Best-effort like branch_reset: the write already happened, so an
// append failure WARNs rather than unwinding the advance.
func (s *Server) writeBranchRebasedAudit(ctx context.Context, actor mergeCandidateActor, runID uuid.UUID, prNumber int,
	branch, baseRef, priorHeadSHA, newHeadSHA, mergeCommitSHA string,
	alreadyUpToDate bool, reason, reparkedReviewStageID string, postMerge *postMergeHeadRead) {
	actorKind, subject := actor.Kind, actor.Subject

	fields := map[string]any{
		"run_id":             runID.String(),
		"pr_number":          prNumber,
		"branch":             branch,
		"base_ref":           baseRef,
		"prior_head_sha":     priorHeadSHA,
		"new_head_sha":       newHeadSHA,
		"merge_commit_sha":   mergeCommitSHA,
		"already_up_to_date": alreadyUpToDate,
		"reason":             reason,
		"mechanism_note":     rebaseMechanismNote,
	}
	if reparkedReviewStageID != "" {
		fields["reparked_review_stage_id"] = reparkedReviewStageID
	}
	if postMerge != nil {
		fields["post_merge_head_read"] = postMerge.Outcome
		fields["post_merge_observed_head_sha"] = postMerge.Observed
		fields["post_merge_read_attempts"] = postMerge.Attempts
	}
	payload, _ := json.Marshal(fields)

	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryBranchRebased,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch rebase: append branch_rebased audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()))
	}
}

// rebaseVouchRequiredNote is the shipped sentence appended to every
// LineageAttributionWarning whose case re-invoking this verb CANNOT repair.
// Re-invocation takes the already-contains-base arm, which deliberately
// attributes nothing, so advertising it here would be the same false-retry
// defect the republish warning was rejected for. fishhawk_vouch_commit is the
// verb that actually admits a sha into the ADR-035 reported-head ledger.
const rebaseVouchRequiredNote = " Re-invoking fishhawk_rebase_run_branch will NOT repair this: the retry takes the already-contains-base arm, which deliberately attributes nothing. Run fishhawk_vouch_commit against the run branch head to admit it into the ADR-035 ledger."

// writeRebaseLineageAttribution admits THIS INVOCATION'S merge commit into
// the ADR-035 reported-head ledger, using the SAME mechanism the vouch path
// uses (an operator_commit_vouched entry whose lineageVouchedSHAField
// addVouchedSHAs reads). Without it the installation-authored merge commit is
// in no head-report category and buildReportedHeadLedger flags it foreign,
// wedging the run on the very check this verb republishes.
//
// EXACTLY ONE SHA IS EVER ATTRIBUTED, and which one is decided by PROVENANCE
// rather than by availability. This is the fix for the post-merge attribution
// race: the lease re-check runs only BEFORE the merge, so a foreign push
// landing in the window between MergeBranch and the post-merge re-read
// becomes the observed head. Vouching it would launder into the ledger precisely the
// foreign commit the ledger exists to catch — the same laundering the
// already-contains-base arm refuses, and that
// TestRebaseRunBranch_LedgerStillFlagsAnUnattributedForeignCommit exists to
// prevent.
//
//   - mergeCommitSHA NON-EMPTY: attribute ONLY mergeCommitSHA. It is the sha
//     the merges endpoint returned for the commit THIS call created, so it is
//     the only sha whose provenance this invocation can prove. The
//     concurrent-push warning fires ONLY when the bounded re-read classified
//     the post-merge head as concurrent_push — a head that is neither the
//     pre-merge head nor the merge commit, which is positive in-band evidence
//     that something else landed in the window: it is NOT attributed, and the
//     divergence is logged AND surfaced on the response. A re-read stuck at
//     the pre-merge head (read_after_write_lag, #4199) or an unreadable one
//     warns nothing: newHeadSHA is then the merge commit itself.
//   - mergeCommitSHA EMPTY (the deliberately benign undecodable-201 shape
//     pinned by githubclient's TestMergeBranch_MergedMissingSHAIsBenign):
//     fall back to attributing newHeadSHA alone — non-empty only on read_back,
//     a re-read head that differs from the pre-merge head — because the merge
//     provably happened and there is nothing else to attribute.
//   - BOTH empty: nothing is attributable at all.
//
// The returned string is EMPTY on a clean attribution and otherwise names why
// the attribution is incomplete. It is surfaced on the response as
// lineage_attribution_warning, because a completed merge whose attribution
// did not land must NOT be reported as a clean recovery: the run stays wedged
// on the lineage check. "Load-bearing" and "best-effort with only a Warn log"
// are contradictory, so the append failure is still non-fatal to the already
// completed merge but is no longer SILENT.
//
// The entry is attributed to actor. Its reason states the authorization the
// commit was created under: the operator's for the verb (byte-identical to the
// pre-#4200 text), the merge-candidate queue's automatic advance for the
// system actor — a system auto-advance must never claim an operator
// authorization no operator gave.
func (s *Server) writeRebaseLineageAttribution(ctx context.Context, actor mergeCandidateActor, runID uuid.UUID,
	branch, baseRef, mergeCommitSHA, newHeadSHA string, postMerge postMergeHeadRead) string {
	// Nothing attributable: an undecodable merge sha AND a post-merge re-read
	// that failed or stayed at the pre-merge head. Re-invocation cannot repair
	// this — say so rather than advertising a retry that cannot deliver.
	if mergeCommitSHA == "" && newHeadSHA == "" {
		warning := "the base merge SUCCEEDED, but NEITHER the merge commit sha nor the post-merge head could be resolved, so NO lineage attribution was recorded; the merge commit is authored by the App installation and carries no head-report entry, so the ADR-035 ledger classifies it as FOREIGN and the run stays wedged." + rebaseVouchRequiredNote
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch rebase: no sha available to attribute; run is left un-attributed",
			slog.String("run_id", runID.String()))
		return warning
	}

	warning := ""
	// PROVENANCE, not availability: prefer the merge sha, and treat a
	// classified concurrent push as evidence of a foreign commit rather than
	// as a second sha to vouch.
	sha := mergeCommitSHA
	if sha == "" {
		sha = newHeadSHA
	} else if postMerge.Outcome == postMergeHeadReadConcurrentPush {
		warning = "the base merge SUCCEEDED and its merge commit " + mergeCommitSHA +
			" was attributed, but the post-merge head read back as " + postMerge.Observed +
			", which DIFFERS from it — a concurrent push landed after the merge. That head was deliberately NOT attributed: vouching a commit this invocation did not create would launder a foreign commit into the ADR-035 ledger. Review the pushed commit and, if it is legitimate, admit it with fishhawk_vouch_commit."
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch rebase: post-merge head diverged from the merge commit; the divergent head was NOT attributed (concurrent push)",
			slog.String("run_id", runID.String()),
			slog.String("merge_commit_sha", mergeCommitSHA),
			slog.String("post_merge_head_sha", postMerge.Observed))
	}

	actorKind, subject := actor.Kind, actor.Subject
	reason := "fishhawk_rebase_run_branch advanced " + branch + " onto " + baseRef +
		"; this commit was created by the App installation on the operator's authorization (ADR-035 sole writer), not by a foreign pusher"
	if actorKind == audit.ActorSystem {
		reason = "the merge-candidate queue (ADR-092) auto-advanced " + branch + " onto " + baseRef +
			" at admission; this commit was created by the App installation under the system actor (ADR-035 sole writer), not by a foreign pusher"
	}

	payload, _ := json.Marshal(map[string]any{
		"run_id":               runID.String(),
		lineageVouchedSHAField: sha,
		"reason":               reason,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryOperatorCommitVouched,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"branch rebase: append lineage attribution failed; run is left un-attributed",
			slog.String("run_id", runID.String()),
			slog.String("sha", sha),
			slog.String("error", err.Error()))
		// The merge already happened, so this does not unwind the response —
		// but it is NOT reported as a clean success either.
		appendWarning := "the base merge SUCCEEDED, but persisting the operator_commit_vouched lineage attribution for " + sha +
			" FAILED (" + err.Error() + "), so the ADR-035 ledger still classifies the merge commit as FOREIGN and the run stays wedged." + rebaseVouchRequiredNote
		if warning != "" {
			return warning + " " + appendWarning
		}
		return appendWarning
	}
	return warning
}
