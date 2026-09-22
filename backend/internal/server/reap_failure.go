package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryDispatchReaperFailed is the audit-log category for the chained entry
// the reap-failure endpoint writes when the MCP host's detached reaper reports
// a spawn-phase runner failure (#1747). Defined LOCALLY here rather than in the
// audit package (per the operator's binding approval condition): audit.category
// is a free-form TEXT column with no enum/CHECK and there is no central
// category registry, so a new category needs no registry coupling. Mirrors the
// dispatchwatchdog.CategoryDispatchWatchdogElapsed precedent — a stable string
// so log scrapers can index on it.
const CategoryDispatchReaperFailed = "dispatch_reaper_failed"

// errReapRepoNotCAS is the sentinel failStageForReap returns when the run
// repository does NOT implement run.StageCASTransitioner (#2672). The reap path
// hard-REQUIRES that capability: its only alternative, run.FailStage, re-anchors
// its walk through whatever live state a concurrent advance produced and would
// take the legal park → failed edge on the four non-children parks — exactly the
// live-park destruction GUARD 2 exists to prevent. Rather than degrade to that
// hazard, the reap path REFUSES loudly. handleReapStageFailure classifies this
// sentinel BEFORE its post-transition re-load so a concurrent park cannot mask a
// misconfiguration as the benign {transitioned:false} no-op. The condition is
// unreachable in a deployed daemon: postgresRepo implements the capability (the
// compile-time assertion in run/postgres.go guarantees it), and serve.go's boot
// check (runRepoCASWiringError) refuses startup for any non-nil RunRepo lacking
// it.
var errReapRepoNotCAS = errors.New("reap path requires a run repository implementing run.StageCASTransitioner")

// errReapRepoNotAttemptCAS is the sentinel failStageForReap returns when an
// ATTEMPT-PINNED reap (a non-empty expected_attempt — the runner's self-report,
// #3598) meets a run repository that does NOT implement
// run.StageAttemptCASTransitioner. The handler maps it to a DISTINCT 503
// attempt_pinned_reap_unsupported and transitions NOTHING: a self-report that
// cannot be attempt-pinned inside the write's own predicate must reap nothing,
// never degrade to a state-only write that a full running(A) → … → running(B)
// re-dispatch cycle would satisfy. Unreachable in a deployed daemon (postgresRepo
// carries the capability, pinned by its compile-time assertion); it exists so a
// partial or in-memory repo cannot quietly weaken the anchor.
var errReapRepoNotAttemptCAS = errors.New("attempt-pinned reap requires a run repository implementing run.StageAttemptCASTransitioner")

// maxReapFailureBodyBytes caps the request body. The reap-failure report is a
// handful of small fields (category, reason, detail, exit_code), so 32 KB is
// well above any realistic payload and well below trace's 64 MiB cap.
const maxReapFailureBodyBytes = 32 * 1024

// reapProtectedParkStates is the set of NON-terminal stage states the reap
// handler must NEVER collapse to failed — the ONE shared predicate behind BOTH
// reap guards (#2630). Each is a LIVE park owned by a resolver OTHER than the
// reaper: a runner legitimately exits 0 into it, or a concurrent advance drives
// the stage into it. This handler refuses a terminal stage and these five
// protected parks, so the ENDPOINT's reap authority spans {pending, dispatched,
// running} — TestReapStageFailure_PendingAnchorSingleCAS (reap_failure_test.go)
// pins that 'pending' is a first-class reapable anchor (one pending → failed
// CAS). The DETACHED reaper only ever REPORTS from {dispatched, running} because
// its own runner-side allow-list (reapStrandAllowList, mcpserver run_stage.go) is
// that narrower set — the states that mean a spawned runner exited WITHOUT
// settling the stage — which is what an "authority is exactly {dispatched,
// running}" phrasing used to conflate with the endpoint's own reach. The
// fishhawk_reap_stage MCP VERB likewise narrows to {dispatched, running} by
// refusing a stable pending stage client-side (E67.52 / #2700), but the endpoint
// stays the unrestricted escape hatch and accepts pending. Every non-terminal
// state that is not one of these five parks is reapable here.
//
//   - awaiting_children       — decomposition fan-in park (#1891/#1903)
//   - awaiting_approval       — plan/gate park
//   - awaiting_input          — clarification park
//   - awaiting_scope_decision — scope-completeness park
//   - awaiting_host_dispatch  — local-spawn park (#1912)
//
// This GENERALIZES the pre-#2630 awaiting_children-only protection to all five
// through one predicate: a sixth park state is protected by being added HERE
// rather than by remembering to touch a fifth open-coded comparison. Keeping the
// set closed (an explicit allow-list of parks, not !IsTerminal) is deliberate —
// a future non-terminal state a runner exits into that is NOT a park (a new
// reapable phase) must fail CLOSED to reapable, not silently protected.
var reapProtectedParkStates = map[run.StageState]bool{
	run.StageStateAwaitingChildren:      true,
	run.StageStateAwaitingApproval:      true,
	run.StageStateAwaitingInput:         true,
	run.StageStateAwaitingScopeDecision: true,
	run.StageStateAwaitingHostDispatch:  true,
}

// isReapProtectedPark reports whether state is a live park the reap handler must
// never reap. It is the shared predicate for GUARD 1 (the load-time fast path)
// and GUARD 2 (the reap-scoped CAS re-anchor refusal in reapFailCAS).
func isReapProtectedPark(state run.StageState) bool {
	return reapProtectedParkStates[state]
}

// reapConditionalAnchors is the CLOSED set of states an OPTIONAL expected_state
// precondition may name (E67.51 / #2699) — exactly the ENDPOINT's reap authority
// {pending, dispatched, running}, i.e. the complement of the terminal states and
// reapProtectedParkStates. A precondition naming anything outside it could never
// be honoured (the handler would refuse that stage regardless), so the endpoint
// answers 400 up front: a precondition the endpoint can never satisfy is a
// CALLER bug, and a 400 naming the accepted set is more actionable than a 409
// the caller cannot act on.
var reapConditionalAnchors = map[run.StageState]bool{
	run.StageStatePending:    true,
	run.StageStateDispatched: true,
	run.StageStateRunning:    true,
}

// reapConditionalAnchorList renders reapConditionalAnchors for an error body, in
// a STABLE order (map iteration is randomized, and a 400's details must not
// shuffle between identical requests).
func reapConditionalAnchorList() []string {
	return []string{
		string(run.StageStatePending),
		string(run.StageStateDispatched),
		string(run.StageStateRunning),
	}
}

// reapFailureRequest is the wire shape the MCP host's detached reaper POSTs
// (#1747). category is exactly "B" or "C" (mirroring pullrequest.go's
// failed-outcome validation); reason is required; detail and exit_code are
// optional diagnostics carrying the parsed runner_failed line and the child's
// process exit code.
//
// ExpectedState is the OPTIONAL compare-and-set precondition (E67.51 / #2699).
// It is a json.RawMessage, NOT a string and NOT a *string, and that is
// load-bearing: the contract distinguishes ABSENCE from every PRESENT value at
// the DECODE layer. A field OMITTED entirely (nil raw) is the UNCONDITIONAL
// request — today's absorbing, idempotent behaviour, byte-for-byte, for the
// detached reaper and run_children's spawn-error compensation. A field PRESENT
// carrying ANYTHING that is not a string naming a reapConditionalAnchors state —
// an empty string, an explicit JSON null, a number, an object — is a 400
// validation_failed.
//
// A *string is NOT sufficient (E67.51 fix-up, review concern): encoding/json
// decodes an explicit `"expected_state": null` to a NIL pointer, which is
// byte-identical to omission, so a malformed conditional request would silently
// lose its pin and take the UNCONDITIONAL absorbing path — reaping a stage a
// concurrent dispatch may have just brought to life. The raw bytes are the only
// decode-layer representation that keeps `null` distinguishable from absence, so
// the presence check reads them directly (validateReapExpectedState).
//
// ExpectedAttempt is the OPTIONAL attempt pin (#3598) — REQUIRED on the
// run-bound arm, where the runner reports its own terminal failure. It carries
// the run.StageAttemptToken the runner received in its prompt envelope
// (stage_attempt) and is a json.RawMessage for the SAME absent-vs-null-vs-empty
// reason as ExpectedState (validateReapExpectedAttempt). An attempt pin implies
// a state pin: it is only honoured together with expected_state.
//
// CROSS-MODULE WIRE CONTRACT: the json tags MUST stay byte-identical to the
// runner's upload.reapFailureRequestBody (runner/internal/upload/upload.go),
// registered as the reap_failure_request pair in backend/internal/wirecontract;
// the shared golden testdata/wire/reap_failure_self_report.json is read by both
// modules' tests.
type reapFailureRequest struct {
	Category        string          `json:"category"`
	Reason          string          `json:"reason"`
	Detail          string          `json:"detail,omitempty"`
	ExitCode        int             `json:"exit_code,omitempty"`
	ExpectedState   json.RawMessage `json:"expected_state,omitempty"`
	ExpectedAttempt json.RawMessage `json:"expected_attempt,omitempty"`
}

// validateReapExpectedAttempt resolves the raw expected_attempt bytes (#3598)
// with the same PRESENCE discipline as validateReapExpectedState: nil raw is
// ABSENT (unpinned); a present value must decode to a NON-EMPTY JSON string.
// `null`, `""`, `7` and `{}` are PRESENT-and-invalid (ok=false) — never the
// unpinned path, or a malformed pin would silently lose its anchor.
func validateReapExpectedAttempt(raw json.RawMessage) (pinned bool, attempt string, got string, ok bool) {
	if raw == nil {
		return false, "", "", true
	}
	trimmed := bytes.TrimSpace(raw)
	var decoded string
	if !bytes.Equal(trimmed, []byte("null")) && json.Unmarshal(trimmed, &decoded) == nil {
		if decoded != "" {
			return true, decoded, decoded, true
		}
		return true, "", decoded, false
	}
	return true, "", string(trimmed), false
}

// validateReapExpectedState resolves the raw expected_state bytes into the
// (conditional, expected) pair the handler acts on, plus the rendering of the
// supplied value for a 400's details.
//
// PRESENCE is decided by the raw bytes being nil (absent) or not; a present
// value must decode to a JSON STRING naming a reapConditionalAnchors state.
// `null`, `""`, `7`, `{}` and `"awaiting_children"` are all PRESENT-and-invalid
// and yield ok=false — never the unconditional path.
func validateReapExpectedState(raw json.RawMessage) (conditional bool, expected run.StageState, got string, ok bool) {
	if raw == nil {
		return false, "", "", true
	}
	trimmed := bytes.TrimSpace(raw)
	var decoded string
	// json.Unmarshal of the `null` literal into a string is a NO-OP that reports
	// no error, so the literal is rejected explicitly rather than by decode error.
	if !bytes.Equal(trimmed, []byte("null")) && json.Unmarshal(trimmed, &decoded) == nil {
		if reapConditionalAnchors[run.StageState(decoded)] {
			return true, run.StageState(decoded), decoded, true
		}
		return true, "", decoded, false
	}
	// A non-string (or the null literal) has no string rendering — echo the raw
	// JSON text so the caller can see exactly what it sent.
	return true, "", string(trimmed), false
}

// reapFailureResponse is the 200 body. Transitioned is false on the idempotent
// no-op path (the stage was already terminal — a double-report or a race with
// the dispatch watchdog), true when this call drove the stage to failed.
type reapFailureResponse struct {
	Transitioned bool   `json:"transitioned"`
	StageState   string `json:"stage_state"`
	// AcceptanceVerdictUnshippedRecorded is true ONLY on the already-terminal
	// no-op path when this call appended an acceptance_verdict_unshipped
	// marker (E72.11 / #3447): the stage is a `succeeded` acceptance stage
	// with no stage-scoped acceptance_outcome_recorded entry newer than its
	// latest dispatch/reopen anchor and no live marker yet. omitempty so every
	// other response body is byte-for-byte what it was.
	AcceptanceVerdictUnshippedRecorded bool `json:"acceptance_verdict_unshipped_recorded,omitempty"`
}

// handleReapStageFailure implements
// POST /v0/runs/{run_id}/stages/{stage_id}/reap-failure (#1747).
//
// The detached fishhawk_dispatch_stage reaper (backend/cmd/fishhawk-mcp
// run_stage.go::spawnRunnerStageDetached) calls this over HTTP when a spawned
// runner exits non-zero BEFORE reporting a terminal stage state (e.g. an
// acceptance pre-flight provision failure). Without it the stage stays
// 'dispatched' forever: retry_stage 422s and no audit entry is written. This is
// the eager, event-driven complement to the off-by-default ~1h dispatch
// watchdog, and it mirrors that watchdog's contract exactly: run.FailStage
// (category C is the retryable infrastructure class) -> AppendChained the
// dispatch_reaper_failed audit entry -> orchestrator.Advance, with the same
// best-effort logging order.
//
// Idempotent: a report against an already-terminal stage is a benign no-op
// (200 {transitioned:false}) with NO audit entry and NO advance — with ONE
// named exception (E72.11 / #3447): an unconditional report against a
// `succeeded` ACCEPTANCE stage whose verdict never shipped for the current
// validation episode appends a stage-scoped acceptance_verdict_unshipped
// marker (still no transition, no advance; see
// recordAcceptanceVerdictUnshipped). A report against an awaiting_children
// stage is the same benign no-op (#1891): that state is a live decomposition
// park owned by its children, and failing it would destroy the fan-in park a
// doomed mis-dispatched runner never owned.
func (s *Server) handleReapStageFailure(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "reap_failure_unconfigured",
			"reap-failure endpoint requires run and audit repositories", nil)
		return
	}

	// Auth: anonymous → 401 first. Then the mutually-exclusive entitlement
	// switch handleFileProductReport uses (#1274), widened here for the
	// runner's terminal-failure self-report (#3598):
	//   (a) a run-bound fhm_ token (subject mcp:run:<id>) for ANOTHER run →
	//       403 run_not_entitled;
	//   (b) a run-bound token for ITS OWN run → admitted WITHOUT write:runs
	//       (run-bound tokens carry mcp:read, never write:runs), but the arm
	//       REQUIRES both expected_state and expected_attempt below;
	//   (c) any other bearer → unchanged write:runs requirement (403
	//       insufficient_scope);
	//   (d) an empty-TokenID cookie session → admitted as before.
	// Nothing is tightened, so the auth-change impact inventory is empty.
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	tokenRunID, runBound := runBoundTokenRunID(id)
	switch {
	case runBound:
		if tokenRunID != runID {
			s.writeError(w, r, http.StatusForbidden, "run_not_entitled",
				"a run-bound token may only report a failure for its own run",
				map[string]any{"run_id": runID.String()})
			return
		}
	case id.TokenID != "":
		if !hasScope(id, "write:runs") {
			s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
				"token is missing required scope: write:runs",
				map[string]any{"required_scope": "write:runs"})
			return
		}
	default:
		// Empty TokenID == session-cookie operator → admit.
	}
	stageID, err := uuid.Parse(r.PathValue("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.PathValue("stage_id")})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxReapFailureBodyBytes+1))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return
	}
	if len(body) > maxReapFailureBodyBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
			"reap-failure body exceeds size cap",
			map[string]any{"limit_bytes": maxReapFailureBodyBytes})
		return
	}

	var req reapFailureRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"reap-failure body could not be decoded",
			map[string]any{"error": err.Error()})
		return
	}

	// Category is exactly B or C (mirroring pullrequest.go's failed-outcome
	// validation). C is the retryable infrastructure class the reaper always
	// sends for a process-level non-zero exit; B is accepted for completeness.
	// A or an empty/unknown value is a 400.
	var cat run.FailureCategory
	switch req.Category {
	case "B":
		cat = run.FailureB
	case "C":
		cat = run.FailureC
	default:
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			`category must be "B" or "C"`,
			map[string]any{"field": "category", "got": req.Category})
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"reason is required", map[string]any{"field": "reason"})
		return
	}

	// OPTIONAL compare-and-set precondition (E67.51 / #2699). PRESENCE, not
	// emptiness and not decodability, is what makes the call conditional: only an
	// OMITTED field is the unconditional request, and any PRESENT value —
	// including "", an explicit null, or a non-string — must name one of the
	// endpoint's three reapable anchors or the request is a 400. See
	// reapFailureRequest's doc comment for why treating "" or null as
	// unconditional would be a silent downgrade rather than a nit.
	conditional, expected, gotExpected, ok := validateReapExpectedState(req.ExpectedState)
	attemptPinned, expectedAttempt, gotAttempt, attemptOK := validateReapExpectedAttempt(req.ExpectedAttempt)
	// RUN-BOUND ARM (#3598): the runner's self-report MUST pin both the state
	// and its own attempt. A state pin alone is insufficient — a retried
	// attempt is ALSO `running` — so an unanchored self-report is refused
	// rather than admitted onto the absorbing unconditional walk.
	if runBound {
		if !conditional || !ok {
			s.writeError(w, r, http.StatusBadRequest, "expected_state_required",
				"a run-bound self-report must pin expected_state to one of the reapable stage states",
				map[string]any{
					"field":    "expected_state",
					"got":      gotExpected,
					"accepted": reapConditionalAnchorList(),
				})
			return
		}
		if !attemptPinned || !attemptOK {
			s.writeError(w, r, http.StatusBadRequest, "expected_attempt_required",
				"a run-bound self-report must pin expected_attempt to the non-empty stage_attempt token from its prompt envelope",
				map[string]any{"field": "expected_attempt", "got": gotAttempt})
			return
		}
	}
	if !ok {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"expected_state must name one of the reapable stage states this endpoint can honour",
			map[string]any{
				"field":    "expected_state",
				"got":      gotExpected,
				"accepted": reapConditionalAnchorList(),
			})
		return
	}
	// An attempt pin on the operator arm is honoured too, under the same rules:
	// it must be a non-empty string and must accompany a state pin (the attempt
	// predicate only exists on the conditional, non-absorbing walk).
	if !attemptOK {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"expected_attempt must be a non-empty stage attempt token",
			map[string]any{"field": "expected_attempt", "got": gotAttempt})
		return
	}
	if attemptPinned && !conditional {
		s.writeError(w, r, http.StatusBadRequest, "expected_state_required",
			"expected_attempt requires expected_state",
			map[string]any{"field": "expected_state", "accepted": reapConditionalAnchorList()})
		return
	}

	// Load the stage and validate the (run_id, stage_id) handle: a stage whose
	// run_id differs from the path does not exist AT THIS PATH → 404.
	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "stage_not_found",
			"stage does not exist", map[string]any{"stage_id": stageID.String()})
		return
	}
	if stage.RunID != runID {
		s.writeError(w, r, http.StatusNotFound, "stage_not_found",
			"stage does not belong to the supplied run",
			map[string]any{"stage_id": stageID.String(), "run_id": runID.String()})
		return
	}

	// LOAD-TIME PRECONDITION (E67.51 / #2699) — checked AHEAD of the terminal
	// no-op and the protected-park fast path, so a conditional caller learns its
	// precondition lost rather than reading a success-shaped body. The DELIBERATE
	// divergence: a CONDITIONAL report against an already-terminal or parked stage
	// is a 409, NOT the 200 {transitioned:false} no-op the unconditional path
	// returns. A caller that pinned a state wants the refusal; the idempotent no-op
	// stays the contract for the (unconditional) detached reaper, whose bounded
	// backoff retry loop depends on a duplicate being benign.
	//
	// Nothing transitions on this path: no CAS, no dispatch_reaper_failed audit
	// entry, no orchestrator advance.
	if conditional && stage.State != expected {
		s.writeError(w, r, http.StatusConflict, "stage_state_precondition_failed",
			"the stage is not in the state the reap was pinned to",
			map[string]any{
				"stage_id":       stageID.String(),
				"expected_state": string(expected),
				"actual_state":   string(stage.State),
			})
		return
	}
	// Attempt-pin FAST PATH ONLY (#3598). Comparing the pin against the LOADED
	// row saves a doomed write and answers the common non-racing case cleanly —
	// but it is NOT the control. The handler's load and the write are two
	// separate reads, and a full running(A) → failed → retry → pending →
	// dispatched → running(B) cycle between them satisfies a state-only
	// `from == running` predicate; only the attempt predicate evaluated INSIDE
	// the transition's own row-locked transaction (failStageForReap →
	// run.StageAttemptCASTransitioner) closes that. Deleting this pre-check must
	// leave behaviour identical — same 409 body, same stage, no audit — because
	// the in-transaction predicate answers it the same way.
	if attemptPinned {
		if actual := run.StageAttemptToken(stage.DispatchedAt); actual != expectedAttempt {
			s.writeStageAttemptSuperseded(w, r, stageID, expectedAttempt, actual)
			return
		}
	}

	// Idempotent no-op: a stage that already reached a terminal state (a
	// double-report, or a race with the dispatch watchdog reaping the same
	// stuck stage) needs no transition, no audit entry, and no advance. Return
	// 200 {transitioned:false} so the reaper treats a duplicate as benign. This
	// is the pre-check the plan calls for — reaching FailStage on a terminal
	// stage would return a transition error, which we would otherwise have to
	// classify here.
	if stage.State.IsTerminal() {
		// E72.11 / #3447: a `succeeded` ACCEPTANCE stage whose runner then
		// exited non-zero is the verdict-unshipped shape — the trace upload
		// settled the stage before the verdict ship failed (a 413). Record a
		// NAMED, stage-scoped marker instead of the bare no-op so the gate,
		// the drive observer and the MCP surface can classify it. No
		// transition, no Advance. Type-gated: every other terminal stage keeps
		// the byte-identical no-op. FAIL-CLOSED on the audit reads: a read
		// error 500s so the reaper's bounded retry re-attempts rather than
		// silently degrading to the pre-#3447 no-op.
		recorded, rerr := s.recordAcceptanceVerdictUnshipped(r.Context(), runID, stage, req, cat)
		if rerr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "acceptance_verdict_unshipped_record_failed",
				"could not decide or record the acceptance_verdict_unshipped marker",
				map[string]any{"stage_id": stageID.String(), "error": rerr.Error()})
			return
		}
		s.writeJSON(w, r, http.StatusOK, reapFailureResponse{
			Transitioned:                       false,
			StageState:                         string(stage.State),
			AcceptanceVerdictUnshippedRecorded: recorded,
		})
		return
	}

	// Protected-park FAST PATH — GUARD 1 of 2 (#1891/#1903, generalized for the
	// #2630 stale-probe TOCTOU): a stage already in ANY protected park at load
	// time is a LIVE park owned by a resolver other than the reaper, not a stuck
	// spawn. Return the benign no-op WITHOUT reaching the transition: 200
	// {transitioned:false}, NO dispatch_reaper_failed audit entry, NO orchestrator
	// advance.
	//
	// This is the AUTHORITATIVE close of the introduced TOCTOU: the detached
	// reaper's zero-exit strand probe (mcpserver run_stage.go) is read BEFORE this
	// POST is sent, so the stage can move running/dispatched → a park in the
	// seconds between that probe and this handler processing the report. Each
	// retry is a FRESH POST re-checked here, so a park that arrives between report
	// attempts is caught too. The runner-side allow-list probe narrows but cannot
	// close that race — no re-probe is atomic with a transition happening on the
	// server; only this handler, re-reading the CURRENT state, is. So a stale
	// positive probe never reaps a park that is live when the report lands.
	//
	// GUARD 1 only catches a park VISIBLE at load. A park landing AFTER this
	// pre-check but before the transition (the mid-transition half of the TOCTOU)
	// is refused by GUARD 2 (failStageForReap), whose refusal surfaces in the
	// post-transition error branch below and re-loads to the same benign no-op.
	// Pre-#2630 only awaiting_children was protected here (and only it was refused
	// mid-flight by run.FailStage); the four other parks were reaped.
	if isReapProtectedPark(stage.State) {
		s.writeJSON(w, r, http.StatusOK, reapFailureResponse{
			Transitioned: false,
			StageState:   string(stage.State),
		})
		return
	}

	// Fail the stage → append the dispatch_reaper_failed audit entry → advance
	// the run, in the exact order and with the exact best-effort logging the
	// dispatch watchdog uses (dispatchwatchdog.go). failStageForReap walks the
	// canonical path from whichever reapable state the stage is in (e.g.
	// dispatched → running → failed), so the spawn-phase 'dispatched' case is
	// handled — but, UNLIKE run.FailStage, it refuses to re-anchor into any
	// protected park that lands mid-transition (GUARD 2, #2630).
	if _, err := failStageForReap(r.Context(), s.cfg.RunRepo, stageID, stage.State, cat, req.Reason, conditional, expectedAttempt); err != nil {
		// (d) WIRING FAULT — the run repository does not implement
		// run.StageCASTransitioner (#2672). This is classified FIRST, ahead of
		// the re-load below, and is load-bearing: the re-load's terminal-or-park
		// check would otherwise report a misconfiguration as the benign
		// {transitioned:false} 200 whenever a concurrent writer parked the stage,
		// collapsing an uncertain configuration fault into a definite no-op
		// answer. 503 matches the endpoint's existing wiring-fault class
		// (reap_failure_unconfigured); the distinct CODE is what pins this arm. No
		// audit entry and no orchestrator advance are written — the early return
		// precedes both. Unreachable in a booted daemon (postgres has the
		// capability and the boot check refuses a repo that does not).
		if errors.Is(err, errReapRepoNotCAS) {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError,
				"reap-failure: run repository does not implement run.StageCASTransitioner",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("repo_type", fmt.Sprintf("%T", s.cfg.RunRepo)))
			s.writeError(w, r, http.StatusServiceUnavailable, "reap_failure_repo_not_cas",
				"reap-failure endpoint requires a run repository implementing run.StageCASTransitioner",
				map[string]any{"stage_id": stageID.String()})
			return
		}
		// (d2) ATTEMPT-PIN WIRING FAULT (#3598): the repo cannot evaluate the
		// attempt inside the write's predicate, so the self-report is refused
		// with a DISTINCT 503 and NOTHING transitioned — never a degrade to a
		// state-only write.
		if errors.Is(err, errReapRepoNotAttemptCAS) {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError,
				"reap-failure: run repository does not implement run.StageAttemptCASTransitioner",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("repo_type", fmt.Sprintf("%T", s.cfg.RunRepo)))
			s.writeError(w, r, http.StatusServiceUnavailable, "attempt_pinned_reap_unsupported",
				"an attempt-pinned reap requires a run repository implementing run.StageAttemptCASTransitioner",
				map[string]any{"stage_id": stageID.String()})
			return
		}
		// (d3) ATTEMPT SUPERSEDED inside the write (#3598): the row-locked
		// predicate saw the pinned state but a DIFFERENT attempt — the stage was
		// re-dispatched between this handler's load and its write. This is THE
		// control; the load-time pre-check above is only its fast path, and both
		// answer the identical 409 body. No audit entry, no advance.
		var ace run.StageAttemptChangedError
		if errors.As(err, &ace) {
			s.writeStageAttemptSuperseded(w, r, stageID, expectedAttempt, ace.Actual)
			return
		}
		// This branch fires for a NARROW, well-classified set, because
		// failStageForReap's re-anchor loop ABSORBS every benign concurrent
		// ADVANCE to another still-live, REAPABLE state (e.g. a dispatched →
		// running flip landing mid-window): those no longer reach here as a
		// refusal — the loop re-anchors and lands failed, so this call returns
		// success. What still lands here is:
		//
		//   (a) A concurrent writer SETTLED the stage terminal (a double-report
		//       or a race with the dispatch watchdog / runner's own terminal
		//       report) — the typed StageStateChangedError is returned unchanged.
		//       Re-load: the stage is terminal, the winner did the work, so return
		//       the benign {transitioned:false} no-op — no audit entry, no advance.
		//   (b) A concurrent writer PARKED the stage in any protected park — the
		//       #1903 decomposed-parent race (awaiting_children), OR the #2630
		//       running/dispatched → awaiting_approval/input/scope_decision race
		//       (the mid-transition half of the stale-probe TOCTOU).
		//       failStageForReap REFUSES the park rather than taking the legal
		//       park → failed edge and destroying it — either up-front (park
		//       visible at its load) or via the row-locked CAS (park landing
		//       mid-flight OR after a re-anchor). Re-load: the stage is a live
		//       park, so return the same benign no-op. Never fail a live park
		//       (#1891/#1903/#2630).
		//   (c) Retry EXHAUSTION under pathological livelock, or a genuine repo
		//       error — the stage is still non-terminal and non-park, so the
		//       re-load falls through to the 500 below. That 500 is the DELIBERATE,
		//       retryable contract (#1907): the detached reaper may re-POST, and
		//       the ~1h dispatch watchdog is the eventual backstop for a genuinely
		//       stuck stage.
		//
		// The re-load's terminal-or-park check is exactly the benign set (a)+(b),
		// using the same shared isReapProtectedPark predicate GUARD 2 refuses on.
		//
		// (e) CONDITIONAL CALLER, precondition LOST mid-flight (E67.51 / #2699).
		// Classified AFTER the errReapRepoNotCAS wiring fault (a misconfiguration
		// must never be reported as a lost precondition) and INSTEAD OF the re-load
		// below: a conditional mismatch must never surface as the benign 200. The
		// actual state is taken from the row-locked sce.Actual — the authoritative
		// mid-flight value the CAS itself observed — rather than from a second,
		// already-stale read. A NON-typed repo error still falls through to the
		// documented retryable 500.
		//
		// WHAT THE 409 GUARANTEES, stated at the strength the code delivers: this
		// call never drove the stage to FAILED, and no dispatch_reaper_failed audit
		// entry and no orchestrator advance were written. It does NOT guarantee that
		// nothing at all was committed. For a DISPATCHED anchor the walk is
		// dispatched → running → failed (unchanged from the unconditional walk), so
		// the reap itself commits the intermediate dispatched → running hop before
		// attempting the second leg; a concurrent transition landing BETWEEN the
		// legs yields this 409 with that hop already committed and the stage left in
		// (or moved on from) 'running'. Exit from 'running' mid-walk is REACHABLE —
		// backend/internal/run/transition.go's StageStateRunning row admits
		// awaiting_approval / awaiting_input / awaiting_scope_decision /
		// awaiting_deployment / succeeded / failed / cancelled — so this is a real
		// interleaving, not a theoretical one, and it is pinned by
		// TestReapStageFailure_ConditionalSecondLegFlipIs409. "Nothing transitioned"
		// is exact for the load-time refusal and for a pending- or running-anchored
		// first-CAS refusal.
		if conditional {
			var sce run.StageStateChangedError
			if errors.As(err, &sce) {
				s.writeError(w, r, http.StatusConflict, "stage_state_precondition_failed",
					"the stage left the state the reap was pinned to before the transition committed",
					map[string]any{
						"stage_id":       stageID.String(),
						"expected_state": string(expected),
						"actual_state":   string(sce.Actual),
					})
				return
			}
		} else if cur, gerr := s.cfg.RunRepo.GetStage(r.Context(), stageID); gerr == nil &&
			(cur.State.IsTerminal() || isReapProtectedPark(cur.State)) {
			s.writeJSON(w, r, http.StatusOK, reapFailureResponse{
				Transitioned: false,
				StageState:   string(cur.State),
			})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not transition the stage to failed",
			map[string]any{"stage_id": stageID.String(), "state": string(stage.State), "error": err.Error()})
		return
	}

	stageIDCopy := stageID
	systemKind := audit.ActorSystem
	payload := reapFailureAuditPayload(runID, stageID, cat, req)
	// expected_state is added ONLY for a conditional call, so the UNCONDITIONAL
	// payload's key set is byte-for-byte what it was before #2699 — "unconditional
	// callers are unaffected" stays a fact rather than an assertion. An
	// unconditionally-present "expected_state":"" would silently change an existing
	// observable payload; TestReapStageFailure_UnconditionalAuditPayloadKeySet pins
	// the key set against the recorded pre-change list.
	if conditional {
		payload["expected_state"] = string(expected)
	}
	// Run-bound self-report provenance (#3598) — added on that arm ONLY, so the
	// operator payload's key set (shared builder, pinned by
	// _UnconditionalAuditPayloadKeySet) does not grow. Both values derive from
	// the VERIFIED identity and the VERIFIED attempt precondition, never from a
	// caller-supplied provenance field.
	if runBound {
		payload["reported_by"] = "runner"
		payload["reported_attempt"] = expectedAttempt
	}
	auditPayload, _ := json.Marshal(payload)
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageIDCopy,
		Timestamp: time.Now().UTC(),
		Category:  CategoryDispatchReaperFailed,
		ActorKind: &systemKind,
		Payload:   auditPayload,
	}); err != nil {
		// State is already failed; surface the audit gap loudly but do NOT
		// unwind the transition — mirrors the watchdog's chain-integrity posture.
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelError,
			"reap-failure: append audit entry failed (state changed without entry)",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()))
	}

	// Walk the run's state machine so a run whose only dispatched stage is now
	// failed doesn't sit in pending/running forever. Best-effort, like the
	// watchdog.
	if s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(r.Context(), runID); err != nil {
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"reap-failure: orchestrator advance failed",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("error", err.Error()))
		}
	}

	s.writeJSON(w, r, http.StatusOK, reapFailureResponse{
		Transitioned: true,
		StageState:   string(run.StageStateFailed),
	})
}

// writeStageAttemptSuperseded writes the 409 stage_attempt_superseded refusal
// (#3598). ONE writer for both the load-time fast path and the in-transaction
// StageAttemptChangedError, so the two are indistinguishable to the caller.
func (s *Server) writeStageAttemptSuperseded(w http.ResponseWriter, r *http.Request, stageID uuid.UUID, expected, actual string) {
	s.writeError(w, r, http.StatusConflict, "stage_attempt_superseded",
		"the stage attempt the report was pinned to has been superseded by a re-dispatch",
		map[string]any{
			"stage_id":         stageID.String(),
			"expected_attempt": expected,
			"actual_attempt":   actual,
		})
}

// recordAcceptanceVerdictUnshipped is the already-terminal branch's
// acceptance arm (E72.11 / #3447). It appends ONE chained, stage-scoped
// acceptance_verdict_unshipped marker and returns true when ALL of:
//   - stage.Type == acceptance and stage.State == succeeded (type gate: a
//     terminal non-acceptance stage, or a failed/cancelled acceptance stage,
//     is the unchanged no-op);
//   - no stage-scoped acceptance_outcome_recorded entry is newer than the
//     stage's latest dispatch/reopen anchor (the verdict for THIS episode never
//     shipped — an outcome after the anchor means it did, and the reaper's
//     non-zero exit was something else);
//   - no LIVE marker already exists (idempotence: the reaper's bounded retry
//     and a watchdog race must not stack markers).
//
// The payload is the dispatch_reaper_failed key set (run_id, stage_id,
// failure_category, reason, detail, exit_code, reported_at, auth_method) so
// the reaper's failure line is preserved verbatim on the chain — built by the
// SAME reapFailureAuditPayload builder the dispatch_reaper_failed append uses,
// so the two cannot drift (E72.12 / #3458). Every audit read error is
// PROPAGATED (never a silent false) so the handler fails closed.
func (s *Server) recordAcceptanceVerdictUnshipped(ctx context.Context, runID uuid.UUID, stage *run.Stage, req reapFailureRequest, cat run.FailureCategory) (bool, error) {
	if stage.Type != run.StageTypeAcceptance || stage.State != run.StageStateSucceeded {
		return false, nil
	}
	// Serialize the check-then-append under the per-stage admission fence
	// (E72.12 / #3458). This is the SAME single-process lock host_dispatch.go
	// and TryShortCircuitAcceptance take (#1936), and the ship handler's
	// acceptance_outcome_recorded append goes through it too
	// (appendAcceptanceOutcomeSerialized, narrow hold). Holding it across the
	// anchor / outcome / live-marker reads AND the AppendChained below means a
	// concurrent second report sees the live marker and appends nothing, and
	// an outcome cannot land between this path's read and its append — the
	// interleaving that would leave a LIVE marker over a shipped verdict. The
	// mutex is non-reentrant: handleReapStageFailure's already-terminal branch
	// holds no other lock when it calls here, and nothing below re-enters the
	// admission walk. No lock when Orchestrator is nil (same as host_dispatch).
	// Residual: process-local, so the multi-replica hazard is the one
	// orchestrator.go already states for the fence.
	if s.cfg.Orchestrator != nil {
		unlock := s.cfg.Orchestrator.LockStageAdmission(stage.ID)
		defer unlock()
	}
	// Anchor: the newest validation episode for this stage. A read error on the
	// dispatched anchor must not collapse into "no anchor" here (that is
	// latestAcceptanceDispatchSeq's fail-closed-for-head-sha contract, not
	// ours), so both anchors are read with the error surfaced.
	dispatchSeq, err := s.newestStageScopedSeq(ctx, runID, stage.ID, CategoryAcceptanceDispatched)
	if err != nil {
		return false, err
	}
	reopenSeq, _, err := s.latestAcceptanceEpisodeRestartSeq(ctx, runID, stage.ID)
	if err != nil {
		return false, err
	}
	anchor := max(dispatchSeq, reopenSeq)
	outcomeSeq, err := s.newestStageScopedSeq(ctx, runID, stage.ID, CategoryAcceptanceOutcomeRecorded)
	if err != nil {
		return false, err
	}
	// Outcome-after-anchor predicate: the verdict for this episode DID ship.
	if outcomeSeq > anchor {
		return false, nil
	}
	// Live-marker idempotence: one marker per episode.
	if _, live, err := s.acceptanceVerdictUnshippedLive(ctx, runID, stage.ID); err != nil {
		return false, err
	} else if live {
		return false, nil
	}
	stageIDCopy := stage.ID
	systemKind := audit.ActorSystem
	payload, _ := json.Marshal(reapFailureAuditPayload(runID, stage.ID, cat, req))
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageIDCopy,
		Timestamp: time.Now().UTC(),
		Category:  CategoryAcceptanceVerdictUnshipped,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		return false, err
	}
	s.notifyStatusUpdate(ctx, runID, CategoryAcceptanceVerdictUnshipped)
	return true, nil
}

// reapFailureAuthMethod is the reap endpoint's recorded auth method — the
// value carried as `auth_method` by BOTH the dispatch_reaper_failed row and
// the acceptance_verdict_unshipped marker (the endpoint admits bearer tokens
// only). One const so the two payloads cannot disagree (E72.12 / #3458).
const reapFailureAuthMethod = "bearer"

// reapFailureAuditPayload builds the UNCONDITIONAL reap-failure audit payload
// (run_id, stage_id, failure_category, reason, detail, exit_code, reported_at,
// auth_method) shared by the dispatch_reaper_failed append and the
// acceptance_verdict_unshipped marker (E72.12 / #3458). The conditional
// caller adds expected_state on top; the key set here is exactly what
// TestReapStageFailure_UnconditionalAuditPayloadKeySet pins.
func reapFailureAuditPayload(runID, stageID uuid.UUID, cat run.FailureCategory, req reapFailureRequest) map[string]any {
	return map[string]any{
		"run_id":           runID.String(),
		"stage_id":         stageID.String(),
		"failure_category": string(cat),
		"reason":           req.Reason,
		"detail":           req.Detail,
		"exit_code":        req.ExitCode,
		"reported_at":      time.Now().UTC().Format(time.RFC3339Nano),
		"auth_method":      reapFailureAuthMethod,
	}
}

// newestStageScopedSeq returns the highest audit sequence among the run's
// entries of the given category scoped to stageID (0 when none), propagating
// the read error.
func (s *Server) newestStageScopedSeq(ctx context.Context, runID, stageID uuid.UUID, category string) (int64, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, category)
	if err != nil {
		return 0, err
	}
	var seq int64
	for _, e := range entries {
		if e.StageID != nil && *e.StageID == stageID && e.Sequence > seq {
			seq = e.Sequence
		}
	}
	return seq, nil
}

// reapFailMaxAttempts bounds reapFailCAS's re-anchor loop, mirroring run's
// failStageCASMaxAttempts: each attempt absorbs one benign concurrent advance to
// another reapable state; four is far beyond any realistic interleaving yet caps
// a pathological livelock so the loop terminates and the handler returns the
// documented 500-and-retry contract.
const reapFailMaxAttempts = 4

// failStageForReap is the reap-scoped compare-and-swap fail — GUARD 2 of 2
// (#2630). It is the reap path's replacement for run.FailStage. run.FailStage
// RE-ANCHORS its walk through whatever live state a concurrent advance produced
// and, for the four non-children parks (awaiting_approval / awaiting_input /
// awaiting_scope_decision / awaiting_host_dispatch), takes the legal park →
// failed edge — its reanchorTarget refuses ONLY awaiting_children, because those
// four ARE legitimately failable by their gate owners (the approval-SLA path, the
// deploy trigger, the trace-policy path). That absorption is correct for THOSE
// callers but WRONG for the reaper: a park landing between this handler's load
// and the transition is a LIVE park the reaper must not collapse (the
// mid-transition half of the #2630 stale-probe TOCTOU). So this walk refuses to
// re-anchor into ANY protected park (isReapProtectedPark) — awaiting_children as
// before, plus the four others — while still ABSORBING a benign advance to
// another reapable state (dispatched → running) exactly as FailStage does, so a
// genuinely-stuck stage is still reaped. A refusal surfaces the typed
// StageStateChangedError unchanged for the handler's post-transition re-load to
// classify as the benign no-op.
//
// run.FailStage is intentionally left UNCHANGED: reversing its #1907 absorption
// globally would break the callers that MUST fail an awaiting_approval /
// awaiting_input / awaiting_scope_decision stage (the approval-SLA, deploy, and
// trace paths). Only the reap path takes this stricter, park-refusing policy.
//
// It anchors the walk to `from` — the state the HANDLER already loaded (never a
// park: GUARD 1 short-circuits a park visible at that load, and the terminal
// pre-check a terminal state, so `from` is a reapable {pending, dispatched,
// running}). It deliberately does NOT re-read the stage: a park landing between
// the handler's load and the CAS is the mid-transition window GUARD 2 owns, and
// the CAS itself observes it (Actual=park → reapReanchor refuses). Re-reading
// here would instead make GUARD 1 redundant (a load-visible park would be caught
// twice) and leave GUARD 1 unable to serve as an independent, counterfactual-able
// guard.
//
// The walk no longer DEGRADES when the repo lacks the CAS capability. run.FailStage
// (the former fallback) re-anchors through whatever live state a concurrent advance
// produced and would take the legal park → failed edge on the four non-children
// parks (awaiting_approval / awaiting_input / awaiting_scope_decision /
// awaiting_host_dispatch) — i.e. exactly the live-park destruction GUARD 2 exists
// to prevent. So a repo without run.StageCASTransitioner makes the reap FAIL LOUDLY
// with errReapRepoNotCAS instead. run.FailStage is now UNREFERENCED from this file
// (the reap path can no longer call it), so the dangerous re-anchor-into-a-live-park
// edge no longer exists here rather than being merely unreached. The compile-time
// assertion in run/postgres.go plus serve.go's boot check (runRepoCASWiringError)
// make this refusal unreachable in a deployed daemon: production (postgresRepo)
// always has the capability.
//
// CONDITIONAL MODE (E67.51 / #2699). When conditional is true the caller pinned
// `from` as an expected_state precondition, and the walk runs with RE-ANCHORING
// DISABLED: any CAS refusal is returned UNCHANGED instead of being absorbed. The
// #1907 benign dispatched → running absorption is precisely the interleaving
// #2699 names (a concurrent dispatch spawns a runner and advances the stage
// while the reap is in flight), so a conditional caller must LOSE it rather than
// absorb it — that is what turns the verb's best-effort narrowing into a
// server-side compare-and-set. Unconditional callers (the detached reaper,
// run_children's spawn-error compensation) pass false and keep the absorbing
// loop verbatim.
//
// ATTEMPT-PINNED MODE (#3598). A non-empty expectedAttempt (the runner's
// self-report) routes EVERY write of the walk — both the dispatched → running
// leg and the → failed leg — through run.StageAttemptCASTransitioner, so the
// attempt is compared inside each write's own row-locked predicate and no
// re-dispatch can land between the legs either. A repo lacking that capability
// returns errReapRepoNotAttemptCAS and transitions NOTHING. An empty
// expectedAttempt leaves every write on TransitionStageFrom, byte-for-byte as
// before (the detached reaper and run_children paths). An attempt pin is by
// construction conditional; a pin with conditional=false is a programming error
// and is refused rather than run on the absorbing walk.
func failStageForReap(ctx context.Context, repo run.Repository, stageID uuid.UUID, from run.StageState, cat run.FailureCategory, reason string, conditional bool, expectedAttempt string) (*run.Stage, error) {
	cas, ok := repo.(run.StageCASTransitioner)
	if !ok {
		return nil, fmt.Errorf("%w (repo type %T)", errReapRepoNotCAS, repo)
	}
	transition := func(from, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
		return cas.TransitionStageFrom(ctx, stageID, from, to, c)
	}
	if expectedAttempt != "" {
		if !conditional {
			return nil, errors.New("reap: an attempt pin requires the conditional (non-absorbing) walk")
		}
		acas, ok := repo.(run.StageAttemptCASTransitioner)
		if !ok {
			return nil, fmt.Errorf("%w (repo type %T)", errReapRepoNotAttemptCAS, repo)
		}
		transition = func(from, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
			return acas.TransitionStageFromAttempt(ctx, stageID, from, to, expectedAttempt, c)
		}
	}
	return reapFailCAS(transition, from, cat, reason, conditional)
}

// reapTransitionFunc is one compare-and-swap write of reapFailCAS's walk: a
// state-only TransitionStageFrom, or its attempt-pinned sibling (#3598).
type reapTransitionFunc func(from, to run.StageState, c *run.StageCompletion) (*run.Stage, error)

// reapFailCAS is failStageForReap's bounded re-anchor loop, mirroring run's
// failStageCAS but with a reap-scoped re-anchor guard (reapReanchor): it ABSORBS
// a benign advance to another reapable state and REFUSES any protected park.
// The dispatched → running → failed walk matches FailStage's so the spawn-phase
// 'dispatched' case and the #1907 benign dispatched → running absorption both
// hold; only the park-refusal diverges.
func reapFailCAS(transition reapTransitionFunc, from run.StageState, cat run.FailureCategory, reason string, conditional bool) (*run.Stage, error) {
	var lastErr error
	for attempt := 0; attempt < reapFailMaxAttempts; attempt++ {
		if from == run.StageStateDispatched {
			running, err := transition(run.StageStateDispatched, run.StageStateRunning, nil)
			if err != nil {
				// CONDITIONAL: no re-anchor, no second attempt — the caller pinned a
				// state and a mid-flight change means the precondition LOST.
				if conditional {
					return nil, err
				}
				next, retry := reapReanchor(err)
				if !retry {
					return nil, err
				}
				from, lastErr = next, err
				continue
			}
			from = running.State
		}
		out, err := transition(from, run.StageStateFailed, &run.StageCompletion{
			FailureCategory: &cat,
			FailureReason:   &reason,
		})
		if err != nil {
			if conditional {
				return nil, err
			}
			next, retry := reapReanchor(err)
			if !retry {
				return nil, err
			}
			from, lastErr = next, err
			continue
		}
		return out, nil
	}
	// Retry exhaustion under pathological livelock: surface the last typed
	// refusal so the handler's re-load yields the documented 500.
	return nil, lastErr
}

// reapReanchor classifies a CAS refusal for reapFailCAS. It returns
// (Actual, true) — re-anchor and retry — ONLY when the row-locked Actual state is
// still reapable (non-terminal AND not a protected park). A terminal state, or
// ANY protected park (isReapProtectedPark: awaiting_children as before, plus the
// four the #2630 concern adds), returns ("", false) so the error propagates
// unchanged for the handler to classify as the benign no-op. This single
// predicate swap — isReapProtectedPark in place of run.reanchorTarget's
// awaiting_children-only check — is the ONLY behavioral divergence from
// run.FailStage, and it is what refuses the four non-children parks the reaper
// must never collapse.
func reapReanchor(err error) (run.StageState, bool) {
	var sce run.StageStateChangedError
	if !errors.As(err, &sce) {
		return "", false
	}
	if sce.Actual.IsTerminal() || isReapProtectedPark(sce.Actual) {
		return "", false
	}
	return sce.Actual, true
}
