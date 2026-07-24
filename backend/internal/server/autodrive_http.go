package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryRunAutoDriven is the audit-log category for the SUPPLEMENTARY
// driver-attribution record the local auto-driver (fishhawk_drive_run,
// #1700) lands for every mechanical act it performs between human gates:
// a gate action it dispatched (act:"gate") or a stage it dispatched
// (act:"dispatch"). It is the run-level record that the local drive verb
// — not the campaign ticker — walked this run one mechanical step under
// ADR-040 delegation.
//
// IMPORTANT — this is NOT the authoritative delegation record. The
// AUTHORITATIVE record of any delegated action is that action's OWN audit
// row (approval_submitted with its delegated rule, stage_fixup_triggered,
// stage_retried, …), which the action path writes TRANSACTIONALLY with the
// state transition. A delegated act can therefore never land on the chain
// without delegation context even if this supplementary row fails to
// append. run_auto_driven exists purely for driver attribution — "which
// driver acted, and what kind of act" — on top of the action's own row
// (binding approval condition 3, #1700).
const CategoryRunAutoDriven = "run_auto_driven"

// run_auto_driven act discriminators + the fields each act shape carries.
const (
	autoDriveActGate     = "gate"
	autoDriveActDispatch = "dispatch"

	// autoDriveSourceEndpoint tags the gate-act row: the act was dispatched
	// by the POST /v0/runs/{run_id}/auto-drive endpoint.
	autoDriveSourceEndpoint = "run_auto_drive_endpoint"

	// autoDriveDispatchAction is the only recognised record-act action: the
	// local drive verb recording a stage dispatch it is about to perform.
	autoDriveDispatchAction = "dispatch_stage"
)

// autoDriveDispatchStages is the closed set of stage discriminators a
// record-act request may name. fixup_redispatch is the re-opened implement
// stage a delegated fixup/retry produced (the local "fixup re-opens but
// nothing spawns" gap this driver closes).
var autoDriveDispatchStages = map[string]struct{}{
	"plan":             {},
	"implement":        {},
	"acceptance":       {},
	"fixup_redispatch": {},
}

// autoDriveResponse is the POST /v0/runs/{run_id}/auto-drive body: the
// AutoDriveRunGate outcome, flattened for the drive verb to switch on. The
// handler serializes it via the direct conversion autoDriveResponse(out), so
// its fields MUST stay in the SAME order as AutoDriveOutcome (tags are ignored
// by the conversion; a field-order mismatch fails to compile).
//
// decision_required / decision_state carry the #2091 decision-required outcome
// (an exhausted fix-up budget / ceiling on the delegated route_fixup arm): the
// driver STOPS and hands the gate to the operator rather than the endpoint
// returning a 500.
type autoDriveResponse struct {
	Acted            bool   `json:"acted"`
	Action           string `json:"action,omitempty"`
	Paged            bool   `json:"paged"`
	PageEvent        string `json:"page_event,omitempty"`
	DecisionRequired bool   `json:"decision_required"`
	DecisionState    string `json:"decision_state,omitempty"`
	Note             string `json:"note"`
}

// handleAutoDrive implements POST /v0/runs/{run_id}/auto-drive (#1700): it
// exposes the in-process Server.AutoDriveRunGate — the GHA campaign driver's
// delegated approve/route_fixup/retry/merge contract with double-gating,
// fail-closed observe-only, and must_page_human refusal — to the local
// auto-driver (fishhawk_drive_run) under the caller's operator-agent
// identity. The delegated action's OWN audit row (approval_submitted with
// its delegated rule, etc.) is written transactionally by AutoDriveRunGate;
// on an ACTED outcome this handler ALSO appends the supplementary
// run_auto_driven act:"gate" row for driver attribution.
//
// FAIL-LOUD (binding approval condition 1): if that supplementary append
// fails after the gate act, the handler returns a 500 error envelope naming
// the recording failure rather than a silent acted:true, so the drive loop
// surfaces it and STOPS acting. The action's own row is already durable, so
// the primary delegation record is intact even in this window (condition 2).
func (s *Server) handleAutoDrive(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	// write:approvals is the strictest action the gate endpoint can dispatch
	// (approve); operator and operator-agent tokens already carry it.
	if id.TokenID != "" && !hasScope(id, "write:approvals") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:approvals",
			map[string]any{"required_scope": "write:approvals"})
		return
	}
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "auto_drive_unconfigured",
			"auto-drive endpoint requires run + audit repositories", nil)
		return
	}

	runRow, ok := s.autoDriveResolveRun(w, r)
	if !ok {
		return
	}

	out, err := s.AutoDriveRunGate(r.Context(), runRow, id, s.cfg.GateMerger, nil)
	if err != nil {
		// A genuine dispatch failure (the action was attempted and the
		// service method errored) is surfaced, never swallowed as
		// observe-only (binding approval condition 1).
		s.writeError(w, r, http.StatusInternalServerError, "auto_drive_dispatch_failed",
			"auto-drive gate action failed", map[string]any{
				"error":  err.Error(),
				"action": out.Action,
			})
		return
	}

	// On an ACTED outcome append the supplementary run_auto_driven act:"gate"
	// row. FAIL-LOUD: if the append fails, return an error naming the
	// recording failure — the drive loop stops acting rather than continuing
	// on a silent acted:true (binding approval condition 1).
	if out.Acted {
		// Recover the delegated CONDITION (the ADR-040 rule) that governed the
		// acted gate so the attribution row carries delegation provenance
		// (never a rule-less gate row).
		rule := s.autoDriveGateRule(r.Context(), runRow, out.Action)
		if aerr := s.appendRunAutoDrivenGate(r.Context(), runRow, id, out, rule); aerr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "auto_drive_record_failed",
				"gate action landed but the supplementary run_auto_driven attribution row failed to append; the authoritative delegated-action audit row is durable, but the driver must stop and not continue acting",
				map[string]any{"error": aerr.Error(), "action": out.Action})
			return
		}
	}

	s.writeJSON(w, r, http.StatusOK, autoDriveResponse(out))
}

// recordAutoDriveActRequest is the POST /v0/runs/{run_id}/auto-drive/acts
// body: the local drive verb's record-BEFORE-dispatch call. It records that
// the driver is about to host-spawn a stage, so no mechanical act can occur
// unaudited — the audit chain stays server-owned; the MCP host never writes
// a chain entry itself.
type recordAutoDriveActRequest struct {
	Action string `json:"action"`
	Stage  string `json:"stage"`
	Source string `json:"source"`
	Note   string `json:"note"`
}

// recordAutoDriveActResponse echoes the appended row's identifying fields.
type recordAutoDriveActResponse struct {
	RunID    string `json:"run_id"`
	Category string `json:"category"`
	Act      string `json:"act"`
	Action   string `json:"action"`
	Stage    string `json:"stage"`
	Source   string `json:"source"`
	Sequence int64  `json:"sequence"`
}

// handleAutoDriveRecordAct implements POST /v0/runs/{run_id}/auto-drive/acts
// (#1700): the sibling record-act endpoint the local drive verb calls to
// record a stage dispatch BEFORE it host-spawns the runner. Validation fails
// CLOSED — an unknown run 404s and every missing/bad field 400s, appending
// NOTHING — so a bogus record never lands. On success it appends a
// run-chained run_auto_driven act:"dispatch" entry under the caller's
// operator-agent identity (delegated:true, the ADR-040 acting-identity
// context). This is the SUPPLEMENTARY driver-attribution record; see
// CategoryRunAutoDriven.
func (s *Server) handleAutoDriveRecordAct(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	if id.TokenID != "" && !hasScope(id, "write:approvals") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:approvals",
			map[string]any{"required_scope": "write:approvals"})
		return
	}
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "auto_drive_unconfigured",
			"auto-drive record-act endpoint requires run + audit repositories", nil)
		return
	}

	runRow, ok := s.autoDriveResolveRun(w, r)
	if !ok {
		return
	}

	var body recordAutoDriveActRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(r.Body).Decode(&body); decErr != nil && !errors.Is(decErr, io.EOF) {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {action, stage, source}",
				map[string]any{"error": decErr.Error()})
			return
		}
	}
	action := strings.TrimSpace(body.Action)
	stage := strings.TrimSpace(body.Stage)
	source := strings.TrimSpace(body.Source)

	// Fail-closed validation, naming the offending field. No append happens
	// on any rejection.
	switch {
	case action == "":
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"action is required", map[string]any{"field": "action"})
		return
	case action != autoDriveDispatchAction:
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"action must be "+autoDriveDispatchAction,
			map[string]any{"field": "action", "got": action})
		return
	case stage == "":
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage is required", map[string]any{"field": "stage"})
		return
	case !autoDriveKnownDispatchStage(stage):
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage is not a recognised dispatch stage",
			map[string]any{"field": "stage", "got": stage})
		return
	case source == "":
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"source is required", map[string]any{"field": "source"})
		return
	}

	entry, err := s.appendRunAutoDrivenDispatch(r.Context(), runRow, id, action, stage, source, strings.TrimSpace(body.Note))
	if err != nil {
		// FAIL-LOUD: the record-before-dispatch contract means the driver
		// must NOT spawn if the record failed. Surface it so the verb stops.
		s.writeError(w, r, http.StatusInternalServerError, "auto_drive_record_failed",
			"failed to append the run_auto_driven dispatch record; the driver must not dispatch",
			map[string]any{"error": err.Error(), "stage": stage})
		return
	}

	s.writeJSON(w, r, http.StatusOK, recordAutoDriveActResponse{
		RunID:    runRow.ID.String(),
		Category: CategoryRunAutoDriven,
		Act:      autoDriveActDispatch,
		Action:   action,
		Stage:    stage,
		Source:   source,
		Sequence: entry.Sequence,
	})
}

// autoDriveResolveRun parses the run_id path value and loads the run,
// writing the 400/404/500 envelope and returning ok=false on any failure.
func (s *Server) autoDriveResolveRun(w http.ResponseWriter, r *http.Request) (*run.Run, bool) {
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return nil, false
	}
	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return nil, false
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return nil, false
	}
	return runRow, true
}

// autoDriveKnownDispatchStage reports whether stage is in the closed
// dispatch-stage set the record-act endpoint accepts.
func autoDriveKnownDispatchStage(stage string) bool {
	_, ok := autoDriveDispatchStages[stage]
	return ok
}

// autoDriveGateRule re-derives the delegated CONDITION (the ADR-040 rule)
// that governed an acted gate outcome so the supplementary run_auto_driven
// row records delegation provenance alongside act/action/source. The
// condition is STATIC operator_agent config, so re-evaluating with the same
// (nil) override the gate endpoint used recovers the identical rule the
// dispatch site applied — independent of the post-action state change. This
// is only reached on an ACTED outcome, where the in-line AutoDriveRunGate
// evaluation already succeeded, so the re-evaluation is expected to resolve.
// Returns "" only if the contract is momentarily unevaluable (a transient
// repo error): the row then omits the rule, and the AUTHORITATIVE rule
// remains on the action's own transactional audit row (binding condition 2).
func (s *Server) autoDriveGateRule(ctx context.Context, runRow *run.Run, action string) string {
	res, _, ok := s.evaluateRunDelegation(ctx, runRow, nil)
	if !ok || res == nil {
		return ""
	}
	if d, found := res.Decision(action); found {
		return string(d.Condition)
	}
	return ""
}

// appendRunAutoDrivenGate appends the supplementary run_auto_driven
// act:"gate" attribution row for an ACTED gate outcome under id, recording
// the delegated rule (when derivable) for provenance. It returns the append
// error so the caller can FAIL-LOUD (binding approval condition 1) — this is
// deliberately NOT best-effort.
func (s *Server) appendRunAutoDrivenGate(ctx context.Context, runRow *run.Run, id Identity, out AutoDriveOutcome, rule string) error {
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	kind := actorKindForSubject(subject)
	fields := map[string]any{
		"act":    autoDriveActGate,
		"action": out.Action,
		"source": autoDriveSourceEndpoint,
		"note":   out.Note,
	}
	// Record the delegated rule so the driver-attribution row keeps delegation
	// provenance; omitted only when momentarily unevaluable (the action's own
	// audit row remains the authoritative delegation record).
	if rule != "" {
		fields["delegated_rule"] = rule
	}
	payload, _ := json.Marshal(fields)
	_, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runRow.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryRunAutoDriven,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	})
	return err
}

// appendRunAutoDrivenDispatch appends the run_auto_driven act:"dispatch"
// record-before-dispatch row under id, returning the created entry (for its
// sequence) or the append error.
func (s *Server) appendRunAutoDrivenDispatch(ctx context.Context, runRow *run.Run, id Identity, action, stage, source, note string) (*audit.Entry, error) {
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	kind := actorKindForSubject(subject)
	payload, _ := json.Marshal(map[string]any{
		"act":    autoDriveActDispatch,
		"action": action,
		"stage":  stage,
		"source": source,
		"note":   note,
	})
	return s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runRow.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryRunAutoDriven,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	})
}
