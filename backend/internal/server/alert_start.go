package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// alertAdmissionKey is the UNEXPORTED context key that admits
// trigger_source=alert through handleCreateRun (E35.4 / #1601, ADR-053).
// Only StartAlertRun sets it. Like scheduledAdmissionKey it is the
// reservation guard's control: an HTTP request arriving through the mux
// cannot carry a value under an unexported key of this package, so no token —
// whatever its scopes or subject — can submit the reserved source.
type alertAdmissionKey struct{}

// withAlertAdmission marks ctx as the alert ingress's in-process request.
func withAlertAdmission(ctx context.Context) context.Context {
	return context.WithValue(ctx, alertAdmissionKey{}, true)
}

// isAlertAdmission reports whether ctx carries the alert ingress's marker.
func isAlertAdmission(ctx context.Context) bool {
	v, _ := ctx.Value(alertAdmissionKey{}).(bool)
	return v
}

// AlertSpecSource is Config.AlertSpecSource: it fetches repo's
// .fishhawk/workflows.yaml at the default branch, returning the spec bytes and
// its blob SHA (which becomes the auto-started run's workflow_sha). fishhawkd
// wires the scheduler's GitHub App adapter; tests substitute a fake.
type AlertSpecSource interface {
	FetchSpec(ctx context.Context, repo string) (spec []byte, sha string, err error)
}

// AlertRunSubject is the audit subject the alert ingress's auto-start acts
// under. It carries the operator-agent token prefix so actorKindForSubject
// attributes it as an agent (the product acting) and — like
// ScheduledRunSubject — it is attribution only, never an issuable token
// subject.
const AlertRunSubject = operatorrole.TokenSubjectPrefix + "alert-trigger"

// alertRunIdentity is the Identity StartAlertRun's in-process request carries,
// mirroring scheduledRunIdentity: TokenID is NON-empty so requireWriteScope
// applies the bearer-token scope check rather than the cookie-session bypass,
// and AccountID is empty so admission audits land in the untenanted partition.
func alertRunIdentity() Identity {
	return Identity{
		Subject: AlertRunSubject,
		TokenID: "fishhawkd-alert-trigger",
		Scopes:  []string{"write:runs"},
	}
}

// AlertRunParams carries what the alert ingress resolved for one auto-start.
// Fields travel in a struct because several adjacent same-typed strings are a
// transposition hazard the compiler cannot catch.
type AlertRunParams struct {
	// Repo is owner/name — the alert source's configured repository, where the
	// incident issue was filed.
	Repo string
	// WorkflowID names the source's workflow (default hotfix_change) in
	// WorkflowSpec.
	WorkflowID string
	// WorkflowSHA is the fetched spec's blob SHA.
	WorkflowSHA string
	// WorkflowSpec is the spec YAML the ingress fetched; shipped inline so
	// handleCreateRun's inline-spec path resolves it.
	WorkflowSpec []byte
	// IssueNumber is the filed incident issue. It must be > 0: an alert run is
	// ALWAYS issue-anchored (run.IsIssueAnchored), so StartAlertRun refuses a
	// non-positive number with an error before driving the handler.
	IssueNumber int
	// RunnerKind selects the execution backend; empty applies the repo-layer
	// default (github_actions).
	RunnerKind string
	// IdempotencyKey makes the auto-start exactly-once for one filed incident
	// (`alert:<issue>:<digest>`): handleCreateRun replays a (repo, key) it has
	// seen as 200. An EMPTY key is passed through as "not idempotent"; the
	// caller owns the key's derivation.
	IdempotencyKey string
}

// AlertStartKind classifies a StartAlertRun outcome.
type AlertStartKind string

const (
	// AlertStartStarted means handleCreateRun minted a new run (201).
	AlertStartStarted AlertStartKind = "started"
	// AlertStartAlreadyStarted means the Idempotency-Key replayed an existing
	// alert run of this workflow (200).
	AlertStartAlreadyStarted AlertStartKind = "already_started"
	// AlertStartRefused means an admission or validation gate refused the
	// start definitively (any 4xx), e.g. 422 workflow_not_applicable or a
	// spec lacking the workflow. No run row exists.
	AlertStartRefused AlertStartKind = "refused"
)

// AlertKeyOccupiedCode is the refusal code StartAlertRun synthesizes when the
// auto-start's Idempotency-Key replays (200) a run that is NOT an alert run of
// this workflow. handleCreateRun itself answers 200 for that replay; the code
// is an ingress-domain classification, never an HTTP response from
// POST /v0/runs.
const AlertKeyOccupiedCode = "alert_key_occupied"

// AlertStartOutcome is the classified result of one StartAlertRun.
type AlertStartOutcome struct {
	Kind AlertStartKind
	// RunID is the minted or replayed run (started / already_started). A
	// refusal for an occupied key (alert_key_occupied) also sets it, to the
	// OCCUPANT run; every other refusal leaves it nil.
	RunID uuid.UUID
	// Status is the HTTP status handleCreateRun answered with (409 for a
	// synthesized alert_key_occupied).
	Status int
	// Code and Message are the error envelope's code and message (refused).
	Code    string
	Message string
}

// StartAlertRun starts (or replays) the hotfix run for one filed incident by
// driving the EXISTING handleCreateRun in-process (E35.4 / #1601, the
// StartScheduledRun precedent). Every admission control — request validation,
// the Idempotency-Key replay, the plan-reviewer capability gate, the blocking
// periodic budget, applies_to (an alert run's trigger form is
// spec.TriggerDiff) and the charter gate — therefore applies byte-for-byte.
// The request carries the alert ingress's Identity and the unexported
// alertAdmissionKey marker, the only thing that admits trigger_source=alert.
// The run is anchored on the filed issue: trigger_ref `issue:N`, with
// issue_context best-effort hydrated from the forge.
//
// Outcome mapping: 201 → started; 200 → already_started ONLY when the
// replayed run is an alert run of THIS workflow — a replay of any other run
// is refused 409 alert_key_occupied, carrying the occupant's id; any other
// 4xx → refused, carrying the envelope's code, message and status. A 5xx — or
// any status this mapping does not recognise — is returned as an ERROR: the
// caller reports it and never fails the incident filing on it.
func (s *Server) StartAlertRun(ctx context.Context, p AlertRunParams) (AlertStartOutcome, error) {
	if p.IssueNumber <= 0 {
		return AlertStartOutcome{}, fmt.Errorf("alert run start: issue number %d is not positive; an alert run is always anchored on its filed incident issue", p.IssueNumber)
	}
	ref := "issue:" + strconv.Itoa(p.IssueNumber)
	req := createRunRequest{
		Repo:          p.Repo,
		WorkflowID:    p.WorkflowID,
		WorkflowSHA:   p.WorkflowSHA,
		TriggerSource: string(run.TriggerAlert),
		TriggerRef:    &ref,
		RunnerKind:    p.RunnerKind,
		WorkflowSpec:  string(p.WorkflowSpec),
		IssueContext:  s.hydrateScheduledIssueContext(ctx, p.Repo, ref),
	}
	body, err := json.Marshal(req)
	if err != nil {
		return AlertStartOutcome{}, fmt.Errorf("alert run start: encode request: %w", err)
	}

	actx := withAlertAdmission(context.WithValue(ctx, ctxKeyIdentity, alertRunIdentity()))
	hr, err := http.NewRequestWithContext(actx, http.MethodPost, "/v0/runs", bytes.NewReader(body))
	if err != nil {
		return AlertStartOutcome{}, fmt.Errorf("alert run start: build request: %w", err)
	}
	hr.Header.Set("Content-Type", "application/json")
	if p.IdempotencyKey != "" {
		hr.Header.Set("Idempotency-Key", p.IdempotencyKey)
	}

	cw := &capturingResponseWriter{header: http.Header{}}
	s.handleCreateRun(cw, hr)
	return classifyAlertStart(cw.status(), cw.body.Bytes(), p.WorkflowID)
}

// classifyAlertStart maps handleCreateRun's answer onto the outcome contract
// documented on StartAlertRun. workflowID is the source's workflow: a 200
// replay counts as already_started only when the replayed run is an alert run
// of that workflow.
func classifyAlertStart(status int, body []byte, workflowID string) (AlertStartOutcome, error) {
	switch {
	case status == http.StatusCreated || status == http.StatusOK:
		var rr runResponse
		if err := json.Unmarshal(body, &rr); err != nil || rr.ID == uuid.Nil {
			return AlertStartOutcome{}, fmt.Errorf("alert run start: status %d carried no decodable run: %v", status, err)
		}
		kind := AlertStartStarted
		if status == http.StatusOK {
			// A replay: an existing run holds the key. Only an alert run of
			// THIS workflow means the incident's run was already started; any
			// other occupant is refused. An empty trigger_source or
			// workflow_id fails the comparison, so a short body fails closed
			// to refused, never already_started.
			if rr.TriggerSource != string(run.TriggerAlert) || rr.WorkflowID != workflowID {
				return AlertStartOutcome{
					Kind:   AlertStartRefused,
					Status: http.StatusConflict,
					RunID:  rr.ID,
					Code:   AlertKeyOccupiedCode,
					Message: fmt.Sprintf("Idempotency-Key is occupied by run %s (trigger_source %s, workflow_id %s), not an alert run of workflow %s; no run was started",
						rr.ID, rr.TriggerSource, rr.WorkflowID, workflowID),
				}, nil
			}
			kind = AlertStartAlreadyStarted
		}
		return AlertStartOutcome{Kind: kind, RunID: rr.ID, Status: status}, nil
	case status >= 400 && status < 500:
		var env errorEnvelope
		if err := json.Unmarshal(body, &env); err != nil || env.Error.Code == "" {
			return AlertStartOutcome{}, fmt.Errorf("alert run start: status %d carried no decodable error envelope: %v", status, err)
		}
		return AlertStartOutcome{
			Kind:    AlertStartRefused,
			Status:  status,
			Code:    env.Error.Code,
			Message: env.Error.Message,
		}, nil
	default:
		var env errorEnvelope
		_ = json.Unmarshal(body, &env)
		return AlertStartOutcome{Status: status}, fmt.Errorf("alert run start: transient status %d (%s): %s",
			status, env.Error.Code, env.Error.Message)
	}
}
