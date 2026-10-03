package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// scheduledAdmissionKey is the UNEXPORTED context key that admits
// trigger_source=scheduled through handleCreateRun (E79.1 / #3725). Only
// StartScheduledRun sets it. It is the reservation guard's control: an HTTP
// request arriving through the mux cannot carry a value under an unexported
// key of this package, so no token — whatever its scopes or subject — can
// submit the reserved source.
type scheduledAdmissionKey struct{}

// withScheduledAdmission marks ctx as the in-process scheduler's request.
func withScheduledAdmission(ctx context.Context) context.Context {
	return context.WithValue(ctx, scheduledAdmissionKey{}, true)
}

// isScheduledAdmission reports whether ctx carries the scheduler's marker.
func isScheduledAdmission(ctx context.Context) bool {
	v, _ := ctx.Value(scheduledAdmissionKey{}).(bool)
	return v
}

// ScheduledRunSubject is the audit subject the in-process scheduler acts
// under. It carries the operator-agent token prefix so actorKindForSubject
// attributes it as an agent (the product acting), and — like
// operatorrole.CampaignActorSubject — it is attribution only, never an
// issuable token subject.
const ScheduledRunSubject = operatorrole.TokenSubjectPrefix + "scheduler"

// scheduledRunIdentity is the Identity StartScheduledRun's in-process request
// carries. TokenID is NON-empty so requireWriteScope applies the same scope
// check it applies to an HTTP bearer token (the campaignOperatorIdentity
// scope-parity pattern) rather than the cookie-session bypass. AccountID is
// empty, so identityAccountID yields nil and admission audits land in the
// untenanted partition, as the campaign driver's runs already do.
func scheduledRunIdentity() Identity {
	return Identity{
		Subject: ScheduledRunSubject,
		TokenID: "fishhawkd-scheduler",
		Scopes:  []string{"write:runs"},
	}
}

// ScheduledRunParams carries what the scheduler resolved for one due window.
// Fields travel in a struct because several adjacent same-typed strings are a
// transposition hazard the compiler cannot catch.
type ScheduledRunParams struct {
	// Repo is owner/name.
	Repo string
	// WorkflowID names the scheduled workflow in WorkflowSpec.
	WorkflowID string
	// WorkflowSHA is the fetched spec's blob SHA.
	WorkflowSHA string
	// WorkflowSpec is the spec YAML the scheduler fetched; it is shipped
	// inline so handleCreateRun's inline-spec path resolves it.
	WorkflowSpec []byte
	// IdempotencyKey is the per-window key
	// (`scheduled:<workflow_id>:<window start UTC RFC3339>`). It is what makes
	// a window exactly-once across ticks and restarts: handleCreateRun replays
	// a (repo, key) it has seen as 200. An EMPTY key is passed through as
	// "not idempotent" exactly as the HTTP header is — the caller owns the
	// key's derivation.
	IdempotencyKey string
	// IssueNumber is the schedule's optional anchor issue. > 0 sets
	// trigger_ref `issue:N` and best-effort hydrates issue_context; <= 0
	// starts an un-anchored run.
	IssueNumber int
	// RunnerKind selects the execution backend; empty applies the
	// repo-layer default (github_actions).
	RunnerKind string
}

// ScheduledStartKind classifies a StartScheduledRun outcome.
type ScheduledStartKind string

const (
	// ScheduledStartStarted means handleCreateRun minted a new run (201).
	ScheduledStartStarted ScheduledStartKind = "started"
	// ScheduledStartAlreadyStarted means the Idempotency-Key replayed an existing
	// run (200) — this window was already started, e.g. before a restart.
	ScheduledStartAlreadyStarted ScheduledStartKind = "already_started"
	// ScheduledStartRefused means an admission or validation gate refused the
	// window definitively (any 4xx), e.g. 402 budget_exhausted or 422
	// workflow_not_applicable. No run row exists.
	ScheduledStartRefused ScheduledStartKind = "refused"
)

// ScheduledKeyOccupiedCode is the refusal code StartScheduledRun synthesizes
// when the window's Idempotency-Key replays (200) a run that is NOT a scheduled
// run of this workflow — a run an ordinary POST /v0/runs minted under the
// upcoming window's key (`scheduled:<workflow_id>:<window UTC>`) before the
// scheduler reached it. handleCreateRun itself answers 200 for that replay; the
// code is a scheduler-domain classification, never an HTTP response.
const ScheduledKeyOccupiedCode = "scheduled_key_occupied"

// ScheduledStartOutcome is the classified result of one StartScheduledRun.
type ScheduledStartOutcome struct {
	Kind ScheduledStartKind
	// RunID is the minted or replayed run (started / already_started). A
	// refusal for an occupied window key (scheduled_key_occupied) also sets it,
	// to the OCCUPANT run; every other refusal leaves it nil.
	RunID uuid.UUID
	// Status is the HTTP status handleCreateRun answered with.
	Status int
	// Code and Message are the error envelope's code and message (refused).
	Code    string
	Message string
}

// StartScheduledRun starts (or replays) the run for one scheduled window by
// driving the EXISTING handleCreateRun in-process (E79.1 / #3725). Every
// admission control — request validation, the Idempotency-Key replay, the
// plan-reviewer capability gate, the blocking periodic budget, applies_to and
// the charter gate — therefore applies byte-for-byte, with no parallel copy
// that could drift as gates are added. The request carries the scheduler's
// Identity and the unexported scheduledAdmissionKey marker, the only thing
// that admits trigger_source=scheduled.
//
// Outcome mapping: 201 → started; 200 → already_started ONLY when the
// replayed run is a scheduled run of THIS workflow (a pure (repo, key) lookup
// answers any run holding the key, so a run an ordinary POST /v0/runs minted
// under the window's key would otherwise be recorded as the window started and
// the window would never run) — a replay of any other run is refused 409
// scheduled_key_occupied, carrying the occupant's id; any other 4xx → refused,
// carrying the envelope's code, message and status. A 5xx — or any status
// this mapping does not recognise — is returned as an ERROR: it is transient, and the scheduler retries the
// window on its next tick. The honest residual of reading the handler's
// status: a future handler path answering 4xx for a transient condition would
// be read as a definitive refusal for that window.
func (s *Server) StartScheduledRun(ctx context.Context, p ScheduledRunParams) (ScheduledStartOutcome, error) {
	req := createRunRequest{
		Repo:          p.Repo,
		WorkflowID:    p.WorkflowID,
		WorkflowSHA:   p.WorkflowSHA,
		TriggerSource: string(run.TriggerScheduled),
		RunnerKind:    p.RunnerKind,
		WorkflowSpec:  string(p.WorkflowSpec),
	}
	if p.IssueNumber > 0 {
		ref := "issue:" + strconv.Itoa(p.IssueNumber)
		req.TriggerRef = &ref
		req.IssueContext = s.hydrateScheduledIssueContext(ctx, p.Repo, ref)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return ScheduledStartOutcome{}, fmt.Errorf("scheduled run start: encode request: %w", err)
	}

	actx := withScheduledAdmission(context.WithValue(ctx, ctxKeyIdentity, scheduledRunIdentity()))
	hr, err := http.NewRequestWithContext(actx, http.MethodPost, "/v0/runs", bytes.NewReader(body))
	if err != nil {
		return ScheduledStartOutcome{}, fmt.Errorf("scheduled run start: build request: %w", err)
	}
	hr.Header.Set("Content-Type", "application/json")
	if p.IdempotencyKey != "" {
		hr.Header.Set("Idempotency-Key", p.IdempotencyKey)
	}

	cw := &capturingResponseWriter{header: http.Header{}}
	s.handleCreateRun(cw, hr)
	return classifyScheduledStart(cw.status(), cw.body.Bytes(), p.WorkflowID)
}

// classifyScheduledStart maps handleCreateRun's answer onto the outcome
// contract documented on StartScheduledRun. workflowID is the workflow the
// window belongs to: a 200 replay counts as already_started only when the
// replayed run is a scheduled run of that workflow.
func classifyScheduledStart(status int, body []byte, workflowID string) (ScheduledStartOutcome, error) {
	switch {
	case status == http.StatusCreated || status == http.StatusOK:
		var rr runResponse
		if err := json.Unmarshal(body, &rr); err != nil || rr.ID == uuid.Nil {
			return ScheduledStartOutcome{}, fmt.Errorf("scheduled run start: status %d carried no decodable run: %v", status, err)
		}
		kind := ScheduledStartStarted
		if status == http.StatusOK {
			// A replay: an existing run holds the key. Only a scheduled run of
			// THIS workflow means the window was already started; any other
			// occupant is refused. An empty trigger_source or workflow_id fails
			// the comparison, so a short body fails closed to refused, never
			// already_started.
			if rr.TriggerSource != string(run.TriggerScheduled) || rr.WorkflowID != workflowID {
				return ScheduledStartOutcome{
					Kind:   ScheduledStartRefused,
					Status: http.StatusConflict,
					RunID:  rr.ID,
					Code:   ScheduledKeyOccupiedCode,
					Message: fmt.Sprintf("Idempotency-Key is occupied by run %s (trigger_source %s, workflow_id %s), not a scheduled run of workflow %s; the window is not started",
						rr.ID, rr.TriggerSource, rr.WorkflowID, workflowID),
				}, nil
			}
			kind = ScheduledStartAlreadyStarted
		}
		return ScheduledStartOutcome{Kind: kind, RunID: rr.ID, Status: status}, nil
	case status >= 400 && status < 500:
		var env errorEnvelope
		if err := json.Unmarshal(body, &env); err != nil || env.Error.Code == "" {
			return ScheduledStartOutcome{}, fmt.Errorf("scheduled run start: status %d carried no decodable error envelope: %v", status, err)
		}
		return ScheduledStartOutcome{
			Kind:    ScheduledStartRefused,
			Status:  status,
			Code:    env.Error.Code,
			Message: env.Error.Message,
		}, nil
	default:
		var env errorEnvelope
		_ = json.Unmarshal(body, &env)
		return ScheduledStartOutcome{Status: status}, fmt.Errorf("scheduled run start: transient status %d (%s): %s",
			status, env.Error.Code, env.Error.Message)
	}
}

// hydrateScheduledIssueContext best-effort fetches the schedule's anchor issue
// into the request's inline issue_context, reusing the campaign path's
// hydrateCampaignIssueContext. It returns nil — and the run starts with only
// its `issue:N` trigger_ref — when no GitHub client is wired, the repo is not
// owner/name, or the installation/issue fetch fails: hydration never blocks a
// scheduled start.
func (s *Server) hydrateScheduledIssueContext(ctx context.Context, repo, issueRef string) *issueContextPayload {
	if s.cfg.GitHub == nil {
		return nil
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return nil
	}
	repoRef := forge.RepoRef{Owner: owner, Name: name}
	instID, err := s.cfg.GitHub.GetRepoInstallation(ctx, repoRef)
	if err != nil {
		if !errors.Is(err, forge.ErrNotInstalled) {
			s.cfg.Logger.Warn("scheduled run start: resolve installation failed; proceeding without issue context",
				"repo", repo, "issue_ref", issueRef, "error", err.Error())
		}
		return nil
	}
	ic := s.hydrateCampaignIssueContext(ctx, instID, repoRef, issueRef)
	if ic == nil {
		return nil
	}
	out := &issueContextPayload{
		Title:  ic.Title,
		Body:   ic.Body,
		URL:    ic.URL,
		Number: ic.Number,
		Labels: ic.Labels,
	}
	for _, c := range ic.Comments {
		out.Comments = append(out.Comments, issueCommentPayload{
			Author:    c.Author,
			Body:      c.Body,
			CreatedAt: c.CreatedAt,
		})
	}
	return out
}

// capturingResponseWriter is the minimal in-package http.ResponseWriter
// StartScheduledRun hands handleCreateRun, so the handler's status and body
// can be classified without an HTTP round trip.
type capturingResponseWriter struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (c *capturingResponseWriter) Header() http.Header { return c.header }

func (c *capturingResponseWriter) WriteHeader(code int) {
	if c.code == 0 {
		c.code = code
	}
}

func (c *capturingResponseWriter) Write(b []byte) (int, error) {
	if c.code == 0 {
		c.code = http.StatusOK
	}
	return c.body.Write(b)
}

// status is the recorded status, defaulting to 200 as net/http does for a
// handler that wrote nothing.
func (c *capturingResponseWriter) status() int {
	if c.code == 0 {
		return http.StatusOK
	}
	return c.code
}
