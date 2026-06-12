package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// fakeBackend is a thin httptest server that records the last
// /v0/runs query (so tests can assert filter forwarding) and a
// /v0/runs/{id} fetch path (so the FISHHAWK_RUN_ID branch has
// somewhere to land). E19.4 / #344 added the per-run stage list
// and per-stage artifact list endpoints so the get_plan tests can
// drive the parent-walk loop without a full backend.
type fakeBackend struct {
	mu sync.Mutex

	lastListQuery string
	listResp      listRunsResult
	listStatus    int

	// /v0/runs/{run_id} fetches consult getRunByID first; the
	// fallback getResp is the default when the id isn't keyed.
	getRunByID map[uuid.UUID]Run
	getResp    Run
	getStatus  int

	// getRunExtraByID overlays extra top-level JSON fields onto a
	// keyed getRunByID response — response fields the thin client.go
	// Run mirror doesn't carry, like the drive read surfaces (#1023)
	// runDriveView decodes from the same body.
	getRunExtraByID map[uuid.UUID]map[string]any

	// Per-call response overrides keyed by query string for tests
	// that exercise multiple resolution paths in one server.
	listByQuery map[string]listRunsResult

	// E19.4 fixtures: stages keyed by run id, artifacts keyed by
	// stage id. Empty map → 200 with empty items list (mirrors the
	// backend's behavior for runs that haven't created stages yet).
	stagesByRun       map[uuid.UUID][]Stage
	artifactsByStage  map[uuid.UUID][]Artifact
	stagesStatus      int
	artifactsStatus   int
	stagesCalledByID  map[uuid.UUID]int
	artifactsCalledID map[uuid.UUID]int

	// E19.5 fixtures: per-run audit responses. Captured limit lets
	// tests verify clamping behavior end-to-end.
	auditByRun      map[uuid.UUID][]AuditEntry
	auditStatus     int
	lastAuditLimit  string
	auditCalledByID map[uuid.UUID]int

	// E19.6 fixtures: per-run audit responses + recorded query
	// state for the /v0/runs/{id}/audit endpoint. Distinct from
	// the cross-chain capture above so tests can verify which
	// surface a tool routed to (and let the same backend serve
	// both shapes for the test suite that mixes them).
	perRunAuditByRun         map[uuid.UUID][]AuditEntry
	perRunAuditNextByRun     map[uuid.UUID]string
	perRunAuditStatus        int
	perRunAuditLastQueryByID map[uuid.UUID]string

	// reviewFlip, when non-nil, is invoked under fb.mu on every per-run
	// audit request with the requested category. The fishhawk_await_review
	// poll-resolve test uses it to flip a pending review to complete
	// mid-poll without wall-clock sleeps — it mutates perRunAuditByRun
	// directly (the caller already holds fb.mu, so it must not re-lock).
	reviewFlip func(category string)

	// E22.1 fixtures: POST /v0/runs.
	// createRunBody captures the last decoded request body so tests
	// can assert what fields were sent.
	// createRunIdempKey captures the last Idempotency-Key header.
	// createRunResp drives the response Run when set; the fake
	// allocates a fresh UUID when CreateRunResp.ID is empty so the
	// dominant test pattern doesn't have to seed it.
	// createRunStatus drives the HTTP status code returned (default
	// 201 Created; tests overriding to 200 simulate the idempotent-
	// replay path).
	// createRunErrBody, when set, is written verbatim as the
	// response body — used to drive 4xx error-envelope tests.
	createRunBody     createRunRequest
	createRunIdempKey string
	createRunResp     Run
	createRunStatus   int
	createRunErrBody  string

	// #978 fixtures: POST /v0/runs/{run_id}/recover. Same shape as the
	// createRun fixtures; recoverParentID captures the path run_id.
	recoverBody     recoverRunRequest
	recoverParentID uuid.UUID
	recoverIdempKey string
	recoverResp     Run
	recoverStatus   int
	recoverErrBody  string

	// E22.2 fixtures: POST /v0/runs/{id}/cancel.
	// cancelResp lets a test seed the post-cancel Run body. When
	// empty the fake echoes the run from getRunByID (if seeded) or
	// builds a minimal Run with State="cancelled".
	// cancelStatus drives the HTTP status code (default 200).
	// cancelErrBody, when set, is written verbatim as the response
	// body so tests can drive the 404 / 409 paths.
	// cancelCalledByID counts cancel calls per run id for idempotency
	// / dedup tests.
	cancelResp       map[uuid.UUID]Run
	cancelStatus     int
	cancelErrBody    string
	cancelCalledByID map[uuid.UUID]int

	// E22.3 fixtures: POST /v0/stages/{id}/retry.
	// retryResp seeds the post-retry Stage body keyed by stage id;
	// when not seeded the fake builds a minimal Stage with
	// State="pending" (the dominant category-A/C outcome).
	// retryStatus drives the HTTP status code (default 200).
	// retryErrBody, when set, is written verbatim — used for the
	// 404 / 422 error-path tests.
	// retryCalledByID counts retry calls per stage id so tests can
	// verify short-circuits.
	retryResp       map[uuid.UUID]Stage
	retryStatus     int
	retryErrBody    string
	retryCalledByID map[uuid.UUID]int

	// E22.X fixtures: POST /v0/stages/{id}/fixup (#762).
	// fixupBody captures the last decoded request body so tests can
	// assert the selected concern indices + reason threading.
	// fixupResp seeds the post-fixup Stage keyed by stage id; default is
	// a minimal Stage with State="pending" (the re-opened outcome).
	// fixupStatus drives the HTTP status (default 200).
	// fixupErrBody, when set, is written verbatim — drives the 400 / 403
	// / 404 / 422 error-path tests.
	// fixupCalledByID counts fixup calls per stage id.
	fixupBody       fixupRequest
	fixupResp       map[uuid.UUID]Stage
	fixupStatus     int
	fixupErrBody    string
	fixupCalledByID map[uuid.UUID]int

	// #984 fixtures: POST /v0/concerns/{id}/waive.
	// waiveBody captures the last decoded request body (the reason).
	// waiveResp seeds the waived-concern response keyed by concern id;
	// default is a minimal WaivedConcern with State="waived".
	// waiveStatus drives the HTTP status (default 200).
	// waiveErrBody, when set, is written verbatim — drives the 400 / 403
	// / 404 / 422 error-path tests.
	// waiveCalledByID counts waive calls per concern id.
	waiveBody       waiveConcernRequest
	waiveResp       map[uuid.UUID]WaivedConcern
	waiveStatus     int
	waiveErrBody    string
	waiveCalledByID map[uuid.UUID]int

	// #961 fixtures: GET /v0/runs/{id}/scope-amendments + the decision
	// POST. amendmentsByRun seeds the list response; decideResp seeds the
	// decided row keyed by amendment id; decideBody captures the last
	// decoded decision body; *Status / *ErrBody drive error paths.
	amendmentsByRun      map[uuid.UUID][]ScopeAmendmentItem
	amendmentsStatus     int
	amendmentsErrBody    string
	decideAmendmentResp  map[uuid.UUID]ScopeAmendmentItem
	decideAmendmentBody  scopeAmendmentDecisionRequest
	decideAmendmentState int
	decideAmendmentErr   string
	decideCalledByID     map[uuid.UUID]int

	// E22.4 fixtures: POST /v0/stages/{id}/approvals.
	// approvalsBody captures the last decoded body so tests can
	// assert decision + comment threading.
	// approvalsResp seeds the post-approve Stage keyed by stage id;
	// default is a minimal Stage with State="succeeded".
	// approvalsStatus drives the HTTP status (default 200).
	// approvalsErrBody, when set, is written verbatim — drives the
	// 404 / 422 error-path tests.
	// approvalsCalledByID counts approval calls per stage id.
	// approvalsRespBody, when set, is written verbatim on the 200 —
	// used to serve the #986 duplicate-labeled shape with the literal
	// duplicate_submission/prior_decision/prior_submitted_at keys so
	// the client-decode seam is pinned from this side of the wire.
	approvalsBody       approvalRequest
	approvalsResp       map[uuid.UUID]Stage
	approvalsStatus     int
	approvalsErrBody    string
	approvalsRespBody   string
	approvalsCalledByID map[uuid.UUID]int

	// Calibration fixtures: GET /v0/calibration.
	// calibrationResp drives the response body.
	// calibrationStatus drives the HTTP status code (default 200).
	// lastCalibrationQuery records the raw query string for assertion.
	calibrationResp      CalibrationResult
	calibrationStatus    int
	lastCalibrationQuery string

	// Budget fixtures: GET /v0/runs/{run_id}/budget (#693).
	// budgetByRun seeds the status per run; an unseeded run returns the
	// empty object {} — mirroring the backend's no-budget 200.
	// budgetStatus drives the HTTP status code (default 200).
	// budgetCalledByID counts fetches per run id.
	budgetByRun      map[uuid.UUID]BudgetStatus
	budgetStatus     int
	budgetCalledByID map[uuid.UUID]int
}

func newFakeBackend(t *testing.T) (*fakeBackend, *httptest.Server) {
	t.Helper()
	fb := &fakeBackend{
		listStatus:               http.StatusOK,
		getStatus:                http.StatusOK,
		stagesStatus:             http.StatusOK,
		artifactsStatus:          http.StatusOK,
		auditStatus:              http.StatusOK,
		perRunAuditStatus:        http.StatusOK,
		listByQuery:              map[string]listRunsResult{},
		getRunByID:               map[uuid.UUID]Run{},
		getRunExtraByID:          map[uuid.UUID]map[string]any{},
		stagesByRun:              map[uuid.UUID][]Stage{},
		artifactsByStage:         map[uuid.UUID][]Artifact{},
		stagesCalledByID:         map[uuid.UUID]int{},
		artifactsCalledID:        map[uuid.UUID]int{},
		auditByRun:               map[uuid.UUID][]AuditEntry{},
		auditCalledByID:          map[uuid.UUID]int{},
		perRunAuditByRun:         map[uuid.UUID][]AuditEntry{},
		perRunAuditNextByRun:     map[uuid.UUID]string{},
		perRunAuditLastQueryByID: map[uuid.UUID]string{},
		createRunStatus:          http.StatusCreated,
		recoverStatus:            http.StatusCreated,
		cancelResp:               map[uuid.UUID]Run{},
		cancelStatus:             http.StatusOK,
		cancelCalledByID:         map[uuid.UUID]int{},
		retryResp:                map[uuid.UUID]Stage{},
		retryStatus:              http.StatusOK,
		retryCalledByID:          map[uuid.UUID]int{},
		fixupResp:                map[uuid.UUID]Stage{},
		fixupStatus:              http.StatusOK,
		fixupCalledByID:          map[uuid.UUID]int{},
		waiveResp:                map[uuid.UUID]WaivedConcern{},
		waiveStatus:              http.StatusOK,
		waiveCalledByID:          map[uuid.UUID]int{},
		amendmentsByRun:          map[uuid.UUID][]ScopeAmendmentItem{},
		amendmentsStatus:         http.StatusOK,
		decideAmendmentResp:      map[uuid.UUID]ScopeAmendmentItem{},
		decideAmendmentState:     http.StatusOK,
		decideCalledByID:         map[uuid.UUID]int{},
		approvalsResp:            map[uuid.UUID]Stage{},
		approvalsStatus:          http.StatusOK,
		approvalsCalledByID:      map[uuid.UUID]int{},
		calibrationStatus:        http.StatusOK,
		budgetByRun:              map[uuid.UUID]BudgetStatus{},
		budgetStatus:             http.StatusOK,
		budgetCalledByID:         map[uuid.UUID]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/stages/{stage_id}/approvals", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("stage_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body approvalRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.approvalsCalledByID[id]++
		fb.approvalsBody = body
		status := fb.approvalsStatus
		errBody := fb.approvalsErrBody
		rawBody := fb.approvalsRespBody
		resp, ok := fb.approvalsResp[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if rawBody != "" {
			_, _ = w.Write([]byte(rawBody))
			return
		}
		if !ok {
			defaultState := "succeeded"
			if body.Decision == "reject" {
				defaultState = "failed"
			}
			resp = Stage{ID: id.String(), Type: "plan", State: defaultState}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/stages/{stage_id}/retry", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("stage_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.retryCalledByID[id]++
		status := fb.retryStatus
		errBody := fb.retryErrBody
		resp, ok := fb.retryResp[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			resp = Stage{ID: id.String(), State: "pending"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/stages/{stage_id}/fixup", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("stage_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body fixupRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.fixupCalledByID[id]++
		fb.fixupBody = body
		status := fb.fixupStatus
		errBody := fb.fixupErrBody
		resp, ok := fb.fixupResp[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			resp = Stage{ID: id.String(), State: "pending"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/concerns/{concern_id}/waive", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("concern_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body waiveConcernRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.waiveCalledByID[id]++
		fb.waiveBody = body
		status := fb.waiveStatus
		errBody := fb.waiveErrBody
		resp, ok := fb.waiveResp[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			resp = WaivedConcern{ID: id.String(), State: "waived", StateReason: body.Reason}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/scope-amendments", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("run_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		status := fb.amendmentsStatus
		errBody := fb.amendmentsErrBody
		items := fb.amendmentsByRun[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if items == nil {
			items = []ScopeAmendmentItem{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
	})
	mux.HandleFunc("POST /v0/runs/{run_id}/scope-amendments/{amendment_id}/decision", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, perr := uuid.Parse(r.PathValue("run_id")); perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		amendmentID, perr := uuid.Parse(r.PathValue("amendment_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body scopeAmendmentDecisionRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.decideCalledByID[amendmentID]++
		fb.decideAmendmentBody = body
		status := fb.decideAmendmentState
		errBody := fb.decideAmendmentErr
		resp, ok := fb.decideAmendmentResp[amendmentID]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			resp = ScopeAmendmentItem{ID: amendmentID.String(), Status: "approved"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/runs/{run_id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("run_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.cancelCalledByID[id]++
		status := fb.cancelStatus
		errBody := fb.cancelErrBody
		resp, ok := fb.cancelResp[id]
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if !ok {
			resp = Run{ID: id.String(), State: "cancelled"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/runs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body createRunRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.createRunBody = body
		fb.createRunIdempKey = r.Header.Get("Idempotency-Key")
		status := fb.createRunStatus
		errBody := fb.createRunErrBody
		resp := fb.createRunResp
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if resp.ID == "" {
			resp.ID = uuid.NewString()
		}
		if resp.Repo == "" {
			resp.Repo = body.Repo
		}
		if resp.WorkflowID == "" {
			resp.WorkflowID = body.WorkflowID
		}
		if resp.State == "" {
			resp.State = "pending"
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("POST /v0/runs/{run_id}/recover", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("run_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body recoverRunRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.recoverBody = body
		fb.recoverParentID = id
		fb.recoverIdempKey = r.Header.Get("Idempotency-Key")
		status := fb.recoverStatus
		errBody := fb.recoverErrBody
		resp := fb.recoverResp
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if resp.ID == "" {
			resp.ID = uuid.NewString()
		}
		if resp.ParentRunID == nil {
			pid := id.String()
			resp.ParentRunID = &pid
		}
		if resp.State == "" {
			resp.State = "pending"
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /v0/runs", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.lastListQuery = r.URL.RawQuery
		resp, override := fb.listByQuery[r.URL.RawQuery]
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.listStatus)
		if override {
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		_ = json.NewEncoder(w).Encode(fb.listResp)
	})
	mux.HandleFunc("GET /v0/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(r.PathValue("run_id"))
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		row, ok := fb.getRunByID[id]
		extra := fb.getRunExtraByID[id]
		fb.mu.Unlock()
		w.WriteHeader(fb.getStatus)
		if ok {
			if len(extra) > 0 {
				// Overlay the extra top-level fields the typed Run
				// mirror can't express (drive read surfaces, #1023).
				b, _ := json.Marshal(row)
				var m map[string]any
				_ = json.Unmarshal(b, &m)
				for k, v := range extra {
					m[k] = v
				}
				_ = json.NewEncoder(w).Encode(m)
				return
			}
			_ = json.NewEncoder(w).Encode(row)
			return
		}
		_ = json.NewEncoder(w).Encode(fb.getResp)
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/stages", func(w http.ResponseWriter, r *http.Request) {
		id, perr := uuid.Parse(r.PathValue("run_id"))
		w.Header().Set("Content-Type", "application/json")
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.stagesCalledByID[id]++
		items := fb.stagesByRun[id]
		fb.mu.Unlock()
		w.WriteHeader(fb.stagesStatus)
		_ = json.NewEncoder(w).Encode(listStagesResult{Items: items})
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/audit", func(w http.ResponseWriter, r *http.Request) {
		id, perr := uuid.Parse(r.PathValue("run_id"))
		w.Header().Set("Content-Type", "application/json")
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.perRunAuditLastQueryByID[id] = r.URL.RawQuery
		if fb.reviewFlip != nil {
			fb.reviewFlip(r.URL.Query().Get("category"))
		}
		all := fb.perRunAuditByRun[id]
		next := fb.perRunAuditNextByRun[id]
		fb.mu.Unlock()
		// Mirror the backend's category filter: when a category query
		// param is set, return only entries of that category. The
		// production endpoint filters server-side; loadPlanReviews
		// relies on this to query plan_reviewed and plan_review_skipped
		// independently (#574).
		items := all
		if cat := r.URL.Query().Get("category"); cat != "" {
			items = nil
			for _, e := range all {
				if e.Category == cat {
					items = append(items, e)
				}
			}
		}
		// Mirror the backend's since_sequence filter (#962): entries
		// with Sequence strictly greater than the anchor, applied
		// before the limit — the contract fishhawk_await_audit's
		// sequence-anchored poll relies on.
		if rawSince := r.URL.Query().Get("since_sequence"); rawSince != "" {
			since, serr := strconv.ParseInt(rawSince, 10, 64)
			if serr != nil || since < 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			filtered := make([]AuditEntry, 0, len(items))
			for _, e := range items {
				if e.Sequence > since {
					filtered = append(filtered, e)
				}
			}
			items = filtered
		}
		w.WriteHeader(fb.perRunAuditStatus)
		_ = json.NewEncoder(w).Encode(listAuditResult{Items: items, NextCursor: next})
	})
	mux.HandleFunc("GET /v0/runs/{run_id}/budget", func(w http.ResponseWriter, r *http.Request) {
		id, perr := uuid.Parse(r.PathValue("run_id"))
		w.Header().Set("Content-Type", "application/json")
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.budgetCalledByID[id]++
		bs, ok := fb.budgetByRun[id]
		status := fb.budgetStatus
		fb.mu.Unlock()
		w.WriteHeader(status)
		if !ok {
			// No budget configured — empty object, as the backend does.
			_, _ = w.Write([]byte("{}"))
			return
		}
		_ = json.NewEncoder(w).Encode(bs)
	})
	mux.HandleFunc("GET /v0/audit", func(w http.ResponseWriter, r *http.Request) {
		runIDQ := r.URL.Query().Get("run_id")
		w.Header().Set("Content-Type", "application/json")
		id, perr := uuid.Parse(runIDQ)
		if perr != nil {
			// /v0/audit allows missing run_id (global feed); the
			// MCP tool always sets it, so a missing one in tests
			// is a programming error.
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.lastAuditLimit = r.URL.Query().Get("limit")
		fb.auditCalledByID[id]++
		items := fb.auditByRun[id]
		fb.mu.Unlock()
		w.WriteHeader(fb.auditStatus)
		_ = json.NewEncoder(w).Encode(listAuditResult{Items: items})
	})
	mux.HandleFunc("GET /v0/stages/{stage_id}/artifacts", func(w http.ResponseWriter, r *http.Request) {
		id, perr := uuid.Parse(r.PathValue("stage_id"))
		w.Header().Set("Content-Type", "application/json")
		if perr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		fb.artifactsCalledID[id]++
		items := fb.artifactsByStage[id]
		fb.mu.Unlock()
		w.WriteHeader(fb.artifactsStatus)
		_ = json.NewEncoder(w).Encode(listArtifactsResult{Items: items})
	})
	mux.HandleFunc("GET /v0/calibration", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.lastCalibrationQuery = r.URL.RawQuery
		status := fb.calibrationStatus
		resp := fb.calibrationResp
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

func newResolver(srv *httptest.Server, env map[string]string) *runResolver {
	return &runResolver{
		api: newAPIClient(config{
			backendURL: srv.URL,
			apiToken:   "tok-test",
		}),
		getenv: envFuncFromMap(env),
	}
}

func envFuncFromMap(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

func sampleRun(id uuid.UUID, repo string, age time.Duration) Run {
	pr := "https://github.com/" + repo + "/pull/42"
	tr := "issue:42"
	return Run{
		ID: id.String(), Repo: repo, WorkflowID: "feature_change",
		TriggerSource:  "github_issue",
		TriggerRef:     &tr,
		State:          "running",
		PullRequestURL: &pr,
		CreatedAt:      time.Now().UTC().Add(-age),
		UpdatedAt:      time.Now().UTC().Add(-age),
	}
}

func TestGetActiveRun_ByPRNumber_QueriesPullRequestURL(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.listResp = listRunsResult{Items: []Run{sampleRun(id, "x/y", time.Hour)}}
	r := newResolver(srv, nil)

	_, out, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		Repo:     "x/y",
		PRNumber: 42,
	})
	if err != nil {
		t.Fatalf("getActiveRun: %v", err)
	}
	if out.Run.ID != id.String() {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, id.String())
	}
	// Verify the filter actually hit the backend.
	for _, want := range []string{
		"repo=x%2Fy",
		"pull_request_url=https%3A%2F%2Fgithub.com%2Fx%2Fy%2Fpull%2F42",
	} {
		if !strings.Contains(fb.lastListQuery, want) {
			t.Errorf("query missing %q: %s", want, fb.lastListQuery)
		}
	}
}

func TestGetActiveRun_ByPRNumber_RequiresRepo(t *testing.T) {
	// pr_number set, repo missing, GITHUB_REPOSITORY unset → the
	// tool can't build the canonical pull_request_url. Surface a
	// clean error rather than silently scoping the search to all
	// installations.
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		PRNumber: 42,
	})
	if err == nil {
		t.Fatal("expected error when repo and GITHUB_REPOSITORY are both unset")
	}
	if !strings.Contains(err.Error(), "repo required") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetActiveRun_ByPRNumber_FallsBackToGitHubRepositoryEnv(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.listResp = listRunsResult{Items: []Run{sampleRun(id, "x/y", time.Hour)}}
	r := newResolver(srv, map[string]string{"GITHUB_REPOSITORY": "x/y"})

	_, out, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		PRNumber: 42,
	})
	if err != nil {
		t.Fatalf("getActiveRun: %v", err)
	}
	if out.Run.ID != id.String() {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, id.String())
	}
}

func TestGetActiveRun_ByTriggerRef_QueriesTriggerRefFilter(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.listResp = listRunsResult{Items: []Run{sampleRun(id, "x/y", time.Hour)}}
	r := newResolver(srv, map[string]string{"GITHUB_REPOSITORY": "x/y"})

	_, out, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		TriggerRef: "issue:42",
	})
	if err != nil {
		t.Fatalf("getActiveRun: %v", err)
	}
	if out.Run.ID != id.String() {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, id.String())
	}
	for _, want := range []string{"repo=x%2Fy", "trigger_ref=issue%3A42"} {
		if !strings.Contains(fb.lastListQuery, want) {
			t.Errorf("query missing %q: %s", want, fb.lastListQuery)
		}
	}
}

func TestGetActiveRun_ByEnvRunID_DirectFetch(t *testing.T) {
	// The runner case: FISHHAWK_RUN_ID stamped on the env →
	// fetch the run directly without a list scan.
	fb, srv := newFakeBackend(t)
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	fb.getResp = sampleRun(id, "x/y", time.Hour)
	r := newResolver(srv, map[string]string{"FISHHAWK_RUN_ID": id.String()})

	_, out, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{})
	if err != nil {
		t.Fatalf("getActiveRun: %v", err)
	}
	if out.Run.ID != id.String() {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, id.String())
	}
}

func TestGetActiveRun_ByEnvRunID_RejectsInvalidUUID(t *testing.T) {
	// Defensive: if the runner stamps a malformed env, surface a
	// clear error rather than a generic 4xx from the GET path.
	_, srv := newFakeBackend(t)
	r := newResolver(srv, map[string]string{"FISHHAWK_RUN_ID": "not-a-uuid"})

	_, _, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{})
	if err == nil {
		t.Fatal("expected error on malformed FISHHAWK_RUN_ID")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetActiveRun_NoResolutionPath_ReturnsStructuredError(t *testing.T) {
	// No pr_number, no trigger_ref, no FISHHAWK_RUN_ID. The error
	// message must list every input the caller could supply so an
	// agent reading it can ask the human for the missing piece.
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{})
	if err == nil {
		t.Fatal("expected error when no resolution path is available")
	}
	for _, want := range []string{"pr_number", "trigger_ref", "FISHHAWK_RUN_ID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q as an option: %v", want, err)
		}
	}
}

func TestGetActiveRun_PRNumber_NoMatchingRun(t *testing.T) {
	// Empty list response → friendly error naming the repo + PR
	// number so the caller knows the lookup itself worked but
	// nothing matched.
	fb, srv := newFakeBackend(t)
	fb.listResp = listRunsResult{Items: nil}
	r := newResolver(srv, nil)

	_, _, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		Repo:     "x/y",
		PRNumber: 42,
	})
	if err == nil {
		t.Fatal("expected not-found error")
	}
	if !strings.Contains(err.Error(), "x/y") || !strings.Contains(err.Error(), "pull/42") {
		t.Errorf("error should name the repo + PR: %v", err)
	}
}

func TestGetActiveRun_PicksMostRecentByCreatedAt(t *testing.T) {
	// Two runs on the same PR (e.g., a retry chain). The resolver
	// returns the newer one. Defensive sort — even if the
	// backend ever stops ordering, we still pick correctly.
	fb, srv := newFakeBackend(t)
	older := uuid.New()
	newer := uuid.New()
	fb.listResp = listRunsResult{Items: []Run{
		sampleRun(older, "x/y", 24*time.Hour),
		sampleRun(newer, "x/y", time.Hour),
	}}
	r := newResolver(srv, nil)

	_, out, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		Repo:     "x/y",
		PRNumber: 42,
	})
	if err != nil {
		t.Fatalf("getActiveRun: %v", err)
	}
	if out.Run.ID != newer.String() {
		t.Errorf("Run.ID = %s, want newer %s", out.Run.ID, newer.String())
	}
}

func TestGetActiveRun_BackendError_Surfaced(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.listStatus = http.StatusInternalServerError
	r := newResolver(srv, nil)

	_, _, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		Repo:     "x/y",
		PRNumber: 42,
	})
	if err == nil {
		t.Fatal("expected backend error")
	}
	// Both wrapped error and the underlying *apiError reach the
	// caller; just verify the surface message is helpful.
	if !strings.Contains(err.Error(), "list runs") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetActiveRun_ResolutionOrder_PRNumberBeatsTriggerRef(t *testing.T) {
	// Both pr_number and trigger_ref provided — the spec's
	// resolution order says pr_number wins. Verify the trigger_ref
	// branch isn't even consulted (it would otherwise hit the
	// backend with a different query).
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.listResp = listRunsResult{Items: []Run{sampleRun(id, "x/y", time.Hour)}}
	r := newResolver(srv, nil)

	_, out, err := r.getActiveRun(context.Background(), nil, GetActiveRunInput{
		Repo:       "x/y",
		PRNumber:   42,
		TriggerRef: "issue:42",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Run.ID != id.String() {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, id.String())
	}
	if !strings.Contains(fb.lastListQuery, "pull_request_url=") {
		t.Errorf("expected pull_request_url filter (pr_number wins); got %s", fb.lastListQuery)
	}
	if strings.Contains(fb.lastListQuery, "trigger_ref=") {
		t.Errorf("trigger_ref filter should not have been used: %s", fb.lastListQuery)
	}
}

func TestRegisterTools_RegistersGetActiveRun(t *testing.T) {
	// Smoke test: registerTools doesn't panic and the SDK accepts
	// the tool definition. Full handshake verification lives in
	// the SDK; we just assert the registration call sequence
	// completes for v0's tool set.
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{
		api:    newAPIClient(cfg),
		getenv: envFuncFromMap(nil),
	}
	registerTools(srv, resolver)
}

// TestToolDescriptions_ConformToHouseStyle is the #778 guardrail: it
// enumerates the FULL registered tool set over an in-memory MCP ListTools
// session (the same path a real client sees) and asserts every tool's
// description meets the structural bar — non-empty, above a minimum length
// FLOOR (a stub/empty-description catch, NOT a target to pad toward), and
// leading with a when/eligibility trigger token so the description tells the
// driving agent WHEN to reach for the tool. Adding a tool without a
// conformant description fails this test.
func TestToolDescriptions_ConformToHouseStyle(t *testing.T) {
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{
		api:    newAPIClient(cfg),
		getenv: envFuncFromMap(nil),
	}
	registerTools(srv, resolver)

	// Drive the server's tool list through a real ListTools round-trip over
	// an in-memory transport, so the assertions run against the wire-visible
	// descriptions rather than the in-process registration structs.
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer clientSession.Close()

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	// when/eligibility trigger tokens (case-insensitive). A conformant
	// description leads with one so the agent reads WHEN to use the tool.
	triggerTokens := []string{"use this when", "when ", "after ", "once ", "before "}
	// Minimum description length is a FLOOR that catches an empty/stub
	// description, not a target — the #778 density guard wants dense, not
	// padded, prose.
	const minDescriptionLen = 80
	// The registered tool set is the fishhawk_* tools swept in #778. Bump
	// this and give the new tool a conformant description when adding one.
	const wantToolCount = 21

	if len(res.Tools) != wantToolCount {
		t.Errorf("registered tool count = %d, want %d (a new tool must be added here with a when/eligibility-leading description)",
			len(res.Tools), wantToolCount)
	}

	for _, tool := range res.Tools {
		if !strings.HasPrefix(tool.Name, "fishhawk_") {
			t.Errorf("tool %q: name does not start with fishhawk_", tool.Name)
		}
		desc := strings.TrimSpace(tool.Description)
		if desc == "" {
			t.Errorf("tool %q: empty description", tool.Name)
			continue
		}
		if len(desc) < minDescriptionLen {
			t.Errorf("tool %q: description length %d is below the %d floor; it must state WHEN to use the tool and name sibling tools",
				tool.Name, len(desc), minDescriptionLen)
		}
		lower := strings.ToLower(desc)
		hasTrigger := false
		for _, tok := range triggerTokens {
			if strings.Contains(lower, tok) {
				hasTrigger = true
				break
			}
		}
		if !hasTrigger {
			t.Errorf("tool %q: description has no when/eligibility trigger token (one of %v); lead with WHEN to reach for the tool",
				tool.Name, triggerTokens)
		}
	}
}

// --- get_plan (E19.4 / #344) ---

// samplePlanContent returns a small but complete standard_v1
// fixture. Used as the inline content on the plan artifact rows
// the fake backend serves.
func samplePlanContent() PlanContent {
	return PlanContent{
		PlanVersion: "standard_v1",
		TicketReference: PlanTicketRef{
			Type: "github_issue",
			URL:  "https://github.com/x/y/issues/42",
			ID:   "x/y#42",
		},
		GeneratedBy: PlanGeneratedBy{
			Agent:     "claude-code",
			Model:     "claude-opus-4-7",
			Timestamp: time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC),
		},
		Summary: "Add a dryRun flag to the dispatcher.",
		Scope: PlanScope{
			Files: []PlanScopeFile{
				{Path: "backend/internal/webhook/dispatcher.go", Operation: "modify"},
			},
			EstimatedLinesChanged: 40,
		},
		Approach: []PlanApproachStep{
			{Step: 1, Description: "Plumb dryRun through Handle."},
			{Step: 2, Description: "Add a unit test."},
		},
		Verification: PlanVerification{
			TestStrategy: "Run the dispatcher tests.",
			RollbackPlan: "Revert the PR.",
		},
		RisksAndAssumptions: []string{
			"Operators set dryRun via a feature flag.",
		},
		PredictedRuntimeMinutes:    20,
		PredictedRuntimeConfidence: "high",
	}
}

// seedPlanArtifact attaches a plan artifact to a stage in the fake
// backend. createdAge sets the artifact's CreatedAt so tests can
// distinguish older vs newer when the most-recent-wins rule fires.
func seedPlanArtifact(fb *fakeBackend, stageID uuid.UUID, content PlanContent, createdAge time.Duration) Artifact {
	v := "standard_v1"
	// Round-trip through JSON so Content holds the same shape it
	// would on the wire (decoded objects/arrays, not the typed
	// struct). The Artifact.Content field is `any` to match the
	// MCP SDK's schema reflection.
	body, _ := json.Marshal(content)
	var decoded any
	_ = json.Unmarshal(body, &decoded)
	art := Artifact{
		ID:            uuid.New().String(),
		StageID:       stageID.String(),
		Kind:          "plan",
		SchemaVersion: &v,
		ContentHash:   "h",
		Content:       decoded,
		CreatedAt:     time.Now().UTC().Add(-createdAge),
	}
	fb.mu.Lock()
	fb.artifactsByStage[stageID] = append(fb.artifactsByStage[stageID], art)
	fb.mu.Unlock()
	return art
}

func TestGetPlan_RejectsInvalidUUID(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: "not-a-uuid"})
	if err == nil {
		t.Fatal("expected error on malformed run_id")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetPlan_FromCurrentRun_StatusAvailableResolvedViaSelf(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
		{ID: uuid.New().String(), RunID: runID.String(), Type: "implement", State: "pending"},
	}
	expectedSummary := "Add a dryRun flag to the dispatcher."
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "available" {
		t.Errorf("Status = %q, want available", out.Status)
	}
	if out.ResolvedVia != "self" {
		t.Errorf("ResolvedVia = %q, want self", out.ResolvedVia)
	}
	if out.Plan == nil {
		t.Fatal("Plan should be non-nil when Status=available")
	}
	if out.Plan.Summary != expectedSummary {
		t.Errorf("summary = %q", out.Plan.Summary)
	}
	if got := len(out.Plan.Scope.Files); got != 1 {
		t.Errorf("scope.files count = %d", got)
	}
}

func TestGetPlan_PicksMostRecentArtifactWhenMultipleExist(t *testing.T) {
	// Same plan stage carries two standard_v1 artifacts (a re-upload
	// after a plan edit). The resolver must pick the newer one.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"}}

	older := samplePlanContent()
	older.Summary = "stale plan"
	seedPlanArtifact(fb, planStageID, older, 24*time.Hour)

	newer := samplePlanContent()
	newer.Summary = "fresh plan"
	seedPlanArtifact(fb, planStageID, newer, time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Plan == nil || out.Plan.Summary != "fresh plan" {
		t.Errorf("Plan.Summary = %v, want 'fresh plan'", out.Plan)
	}
}

func TestGetPlan_RetryChain_WalksParentRunID(t *testing.T) {
	// Child run has no plan stage (CI-retry shape per #279 / E16);
	// parent run has the plan. The walk should resolve the parent's
	// plan and stamp ResolvedVia=parent:<id>.
	fb, srv := newFakeBackend(t)
	parentID := uuid.New()
	childID := uuid.New()
	parentPlanStage := uuid.New()

	parentIDStr := parentID.String()
	fb.getRunByID[childID] = Run{
		ID:          childID.String(),
		ParentRunID: &parentIDStr,
		State:       "running",
		Repo:        "x/y",
	}
	fb.getRunByID[parentID] = Run{ID: parentID.String(), State: "running", Repo: "x/y"}
	// Child has only an implement stage (the retry's shape).
	fb.stagesByRun[childID] = []Stage{
		{ID: uuid.New().String(), RunID: childID.String(), Type: "implement", State: "running"},
	}
	// Parent has the plan stage carrying the artifact.
	fb.stagesByRun[parentID] = []Stage{
		{ID: parentPlanStage.String(), RunID: parentID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, parentPlanStage, samplePlanContent(), time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: childID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "available" {
		t.Errorf("Status = %q, want available", out.Status)
	}
	if out.ResolvedVia != "parent:"+parentID.String() {
		t.Errorf("ResolvedVia = %q, want parent:%s", out.ResolvedVia, parentID)
	}
	if out.Plan == nil {
		t.Fatal("Plan should be non-nil")
	}
}

func TestGetPlan_NoPlanYet_ChainRootReached(t *testing.T) {
	// Run has no plan stage AND no parent. The structured
	// no_plan_yet response names the chain depth searched (0,
	// since the root is the requested run itself).
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "running", Repo: "x/y"}
	fb.stagesByRun[runID] = nil // no stages — plan stage absent

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "no_plan_yet" {
		t.Errorf("Status = %q, want no_plan_yet", out.Status)
	}
	if out.Plan != nil {
		t.Errorf("Plan should be nil on no_plan_yet; got %+v", out.Plan)
	}
	if !strings.Contains(out.Message, "chain root reached") {
		t.Errorf("Message should explain the chain shape: %q", out.Message)
	}
}

func TestGetPlan_NoPlanYet_PlanStagePending(t *testing.T) {
	// Plan stage exists but has no terminal plan artifact yet
	// (mid-upload race). Same no_plan_yet response shape so the
	// agent can branch without parsing prose.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "running"}
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "running"},
	}
	// Artifacts map: empty — no plan uploaded yet.

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "no_plan_yet" {
		t.Errorf("Status = %q, want no_plan_yet", out.Status)
	}
}

func TestGetPlan_RetryChain_DepthCap_NoPlanYet(t *testing.T) {
	// Build a chain of 10 runs, no plan stage on any of them. The
	// walk stops at retryPlanChainDepth (8) and returns
	// no_plan_yet with a "depth cap" message rather than looping
	// forever.
	fb, srv := newFakeBackend(t)
	const chainLen = 10
	ids := make([]uuid.UUID, chainLen)
	for i := range ids {
		ids[i] = uuid.New()
	}
	for i := 0; i < chainLen; i++ {
		row := Run{ID: ids[i].String(), Repo: "x/y", State: "running"}
		if i+1 < chainLen {
			parentStr := ids[i+1].String()
			row.ParentRunID = &parentStr
		}
		fb.getRunByID[ids[i]] = row
		fb.stagesByRun[ids[i]] = nil
	}

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: ids[0].String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "no_plan_yet" {
		t.Errorf("Status = %q, want no_plan_yet", out.Status)
	}
	if !strings.Contains(out.Message, "chain depth cap") {
		t.Errorf("Message should mention chain depth cap: %q", out.Message)
	}
	// Defensive: the walk visited at most retryPlanChainDepth
	// stages-fetches, never the 9th id in the chain.
	if got := fb.stagesCalledByID[ids[retryPlanChainDepth]]; got != 0 {
		t.Errorf("walk visited id[%d] %d times; expected 0 (past the cap)",
			retryPlanChainDepth, got)
	}
}

func TestGetPlan_BackendError_StagesList_Surfaced(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String()}
	fb.stagesStatus = http.StatusInternalServerError

	r := newResolver(srv, nil)
	_, _, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected error on stages 500")
	}
	if !strings.Contains(err.Error(), "list stages") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetPlan_WithDecomposition_FieldsSurfaced(t *testing.T) {
	// Plan artifact carries decomposition.sub_plans (ADR-025 D2 /
	// #476). The tool must surface Decomposition, its Rationale, the
	// sub-plans slice, and the runtime prediction fields.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}

	content := samplePlanContent()
	content.Decomposition = &PlanDecomposition{
		Rationale: "Two independent file areas allow parallel execution.",
		SubPlans: []PlanSubPlan{
			{Title: "Add dispatcher flag", ScopeHint: "backend/internal/webhook/", PredictedRuntimeMinutes: 10, PredictedRuntimeConfidence: "high"},
			{Title: "Add unit tests", ScopeHint: "backend/internal/webhook/dispatcher_test.go", PredictedRuntimeMinutes: 8, PredictedRuntimeConfidence: "medium"},
		},
	}
	seedPlanArtifact(fb, planStageID, content, time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "available" {
		t.Errorf("Status = %q, want available", out.Status)
	}
	if out.Plan == nil {
		t.Fatal("Plan should be non-nil when Status=available")
	}
	if out.Plan.Decomposition == nil {
		t.Fatal("Plan.Decomposition should be non-nil for a decomposed plan")
	}
	if out.Plan.Decomposition.Rationale == "" {
		t.Error("Plan.Decomposition.Rationale should be non-empty")
	}
	if got := len(out.Plan.Decomposition.SubPlans); got != 2 {
		t.Errorf("len(Plan.Decomposition.SubPlans) = %d, want 2", got)
	}
	if out.Plan.PredictedRuntimeMinutes <= 0 {
		t.Errorf("Plan.PredictedRuntimeMinutes = %d, want > 0", out.Plan.PredictedRuntimeMinutes)
	}
}

func TestGetPlan_WithoutDecomposition_RuntimeFieldsPresent(t *testing.T) {
	// Plan artifact has no decomposition (standalone plan). The D2
	// runtime-prediction fields must still be surfaced; Decomposition
	// must be nil so it is omitted from the JSON response.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Plan == nil {
		t.Fatal("Plan should be non-nil")
	}
	if out.Plan.PredictedRuntimeMinutes <= 0 {
		t.Errorf("Plan.PredictedRuntimeMinutes = %d, want > 0", out.Plan.PredictedRuntimeMinutes)
	}
	if out.Plan.PredictedRuntimeConfidence == "" {
		t.Error("Plan.PredictedRuntimeConfidence should be non-empty")
	}
	if out.Plan.Decomposition != nil {
		t.Errorf("Plan.Decomposition should be nil for a non-decomposed plan; got %+v", out.Plan.Decomposition)
	}
}

// --- get_plan reviews field (ADR-027 / #560 sub-plan E) ---

// seedPlanReviewAudit adds a plan_reviewed audit entry to the fake's
// per-run audit map. The payload is round-tripped through JSON so the
// handler's re-marshal + unmarshal decodes to the same value.
func seedPlanReviewAudit(fb *fakeBackend, runID uuid.UUID, review PlanReview) {
	payload, _ := json.Marshal(review)
	var decoded any
	_ = json.Unmarshal(payload, &decoded)
	entry := AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: "plan_reviewed",
		Payload:  decoded,
	}
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], entry)
}

// seedImplementReviewAudit adds an implement_reviewed audit entry to the
// fake's per-run audit map (ADR-027 impl 2/2). Mirrors
// seedPlanReviewAudit but for the implement-review category.
func seedImplementReviewAudit(fb *fakeBackend, runID uuid.UUID, review PlanReview) {
	payload, _ := json.Marshal(review)
	var decoded any
	_ = json.Unmarshal(payload, &decoded)
	entry := AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: "implement_reviewed",
		Payload:  decoded,
	}
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], entry)
}

func TestGetRunStatus_WithImplementReviews_PopulatesField(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}

	seedImplementReviewAudit(fb, runID, PlanReview{
		ReviewerKind:  "agent",
		ReviewerModel: "claude-opus-4-7",
		Authority:     "advisory",
		Verdict:       "approve_with_concerns",
		Concerns: []PlanReviewConcern{
			{Severity: "low", Category: "scope", Note: "touched a file outside scope.files"},
		},
		FreeForm: "diff implements the plan",
	})

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if got := len(out.ImplementReviews); got != 1 {
		t.Fatalf("len(ImplementReviews) = %d, want 1", got)
	}
	rev := out.ImplementReviews[0]
	if rev.Verdict != "approve_with_concerns" {
		t.Errorf("Verdict = %q, want approve_with_concerns", rev.Verdict)
	}
	if rev.Authority != "advisory" {
		t.Errorf("Authority = %q, want advisory", rev.Authority)
	}
	if len(rev.Concerns) != 1 || rev.Concerns[0].Category != "scope" {
		t.Errorf("Concerns = %+v, want one scope concern", rev.Concerns)
	}
}

func TestGetRunStatus_NoImplementReviews_NilField(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String()}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.ImplementReviews != nil {
		t.Errorf("ImplementReviews should be nil with no entries; got %+v", out.ImplementReviews)
	}
}

func TestGetRunStatus_ImplementReviewSkipped_SurfacesSkippedVerdict(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String()}

	// implement_review_skipped entry (reviewer not wired).
	payload, _ := json.Marshal(map[string]any{
		"reason":            "reviewer_not_configured",
		"configured_agents": 1,
		"authority":         "gating",
	})
	var decoded any
	_ = json.Unmarshal(payload, &decoded)
	fb.perRunAuditByRun[runID] = []AuditEntry{{
		ID:       uuid.New().String(),
		Sequence: 1,
		RunID:    runID.String(),
		Category: "implement_review_skipped",
		Payload:  decoded,
	}}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if got := len(out.ImplementReviews); got != 1 {
		t.Fatalf("len(ImplementReviews) = %d, want 1", got)
	}
	if out.ImplementReviews[0].Verdict != "skipped" {
		t.Errorf("Verdict = %q, want skipped", out.ImplementReviews[0].Verdict)
	}
	if out.ImplementReviews[0].Reason != "reviewer_not_configured" {
		t.Errorf("Reason = %q, want reviewer_not_configured", out.ImplementReviews[0].Reason)
	}
}

func TestGetPlan_WithReviews_PopulatesField(t *testing.T) {
	// Two plan-review agent verdicts recorded on the plan's run.
	// Both must appear in Reviews with correct fields.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	seedPlanReviewAudit(fb, runID, PlanReview{
		ReviewerKind:  "agent",
		ReviewerModel: "claude-opus-4-7",
		Authority:     "advisory",
		Verdict:       "approve",
	})
	seedPlanReviewAudit(fb, runID, PlanReview{
		ReviewerKind:  "agent",
		ReviewerModel: "claude-opus-4-7",
		Authority:     "advisory",
		Verdict:       "approve_with_concerns",
		Concerns: []PlanReviewConcern{
			{Severity: "medium", Category: "scope", Note: "touching too many files"},
		},
		FreeForm: "Consider narrowing scope.",
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "available" {
		t.Fatalf("Status = %q, want available", out.Status)
	}
	if got := len(out.Reviews); got != 2 {
		t.Fatalf("len(Reviews) = %d, want 2", got)
	}
	if out.Reviews[0].Verdict != "approve" {
		t.Errorf("Reviews[0].Verdict = %q, want approve", out.Reviews[0].Verdict)
	}
	if out.Reviews[0].Authority != "advisory" {
		t.Errorf("Reviews[0].Authority = %q, want advisory", out.Reviews[0].Authority)
	}
	if out.Reviews[1].Verdict != "approve_with_concerns" {
		t.Errorf("Reviews[1].Verdict = %q, want approve_with_concerns", out.Reviews[1].Verdict)
	}
	if got := len(out.Reviews[1].Concerns); got != 1 {
		t.Fatalf("len(Reviews[1].Concerns) = %d, want 1", got)
	}
	if out.Reviews[1].Concerns[0].Severity != "medium" {
		t.Errorf("Concerns[0].Severity = %q, want medium", out.Reviews[1].Concerns[0].Severity)
	}
	if out.Reviews[1].FreeForm != "Consider narrowing scope." {
		t.Errorf("Reviews[1].FreeForm = %q", out.Reviews[1].FreeForm)
	}
}

func TestGetPlan_WithSkippedReview_SurfacesSkippedVerdict(t *testing.T) {
	// A plan_review_skipped audit entry (#574) surfaces as a synthesized
	// review with verdict "skipped" and the recorded reason/authority.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	payload, _ := json.Marshal(map[string]any{
		"reason":            "reviewer_not_configured",
		"configured_agents": 1,
		"authority":         "gating",
	})
	var decoded any
	_ = json.Unmarshal(payload, &decoded)
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: 1,
		RunID:    runID.String(),
		Category: "plan_review_skipped",
		Payload:  decoded,
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "available" {
		t.Fatalf("Status = %q, want available", out.Status)
	}
	if len(out.Reviews) != 1 {
		t.Fatalf("len(Reviews) = %d, want 1", len(out.Reviews))
	}
	got := out.Reviews[0]
	if got.Verdict != "skipped" {
		t.Errorf("Verdict = %q, want skipped", got.Verdict)
	}
	if got.Reason != "reviewer_not_configured" {
		t.Errorf("Reason = %q, want reviewer_not_configured", got.Reason)
	}
	if got.Authority != "gating" {
		t.Errorf("Authority = %q, want gating", got.Authority)
	}
	if got.ReviewerKind != "agent" {
		t.Errorf("ReviewerKind = %q, want agent", got.ReviewerKind)
	}
}

func TestGetPlan_NoReviewAuditEntries_ReviewsAbsent(t *testing.T) {
	// No plan_reviewed audit entries — Reviews should be nil so
	// it is omitted from the JSON response (omitempty).
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)
	// perRunAuditByRun[runID] left empty.

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "available" {
		t.Fatalf("Status = %q, want available", out.Status)
	}
	if out.Reviews != nil {
		t.Errorf("Reviews should be nil when no plan_reviewed entries exist; got %+v", out.Reviews)
	}
}

// seedScopePrecheckAudit marshals a SERVER-side ScopePrecheckPayload and
// feeds it back through the fake backend as a plan_scope_precheck audit
// entry. Using the real server type is the point of the seam test: it
// exercises the backend-write -> mcp-read JSON contract end to end, so a
// drift in either side's struct tags fails here rather than silently in
// production.
func seedScopePrecheckAudit(fb *fakeBackend, runID uuid.UUID, payload server.ScopePrecheckPayload) {
	raw, _ := json.Marshal(payload)
	var decoded any
	_ = json.Unmarshal(raw, &decoded)
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: "plan_scope_precheck",
		Payload:  decoded,
	})
	fb.mu.Unlock()
}

func TestGetPlan_ScopePrecheck_CrossBoundarySeam(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	seedScopePrecheckAudit(fb, runID, server.ScopePrecheckPayload{
		WorkflowID:       "feature_change",
		ImplementStageID: "implement",
		ScannedFiles:     2,
		Violations: []policy.Violation{
			{
				Constraint: "forbidden_paths",
				Detail:     `pattern ".github/workflows/**" matched`,
				Files:      []string{".github/workflows/ci.yml"},
			},
		},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.ScopePrecheck == nil {
		t.Fatal("ScopePrecheck is nil; want populated")
	}
	if out.ScopePrecheck.ScannedFiles != 2 {
		t.Errorf("ScannedFiles = %d, want 2", out.ScopePrecheck.ScannedFiles)
	}
	if got := len(out.ScopePrecheck.Violations); got != 1 {
		t.Fatalf("len(Violations) = %d, want 1", got)
	}
	v := out.ScopePrecheck.Violations[0]
	if v.Constraint != "forbidden_paths" {
		t.Errorf("Constraint = %q, want forbidden_paths", v.Constraint)
	}
	if len(v.Files) != 1 || v.Files[0] != ".github/workflows/ci.yml" {
		t.Errorf("Files = %v, want [.github/workflows/ci.yml]", v.Files)
	}
}

func TestGetPlan_ScopePrecheck_NewestEntryWins(t *testing.T) {
	// A schema-retry run writes two plan_scope_precheck entries; the
	// authoritative one is the newest (last, sequence-ascending). The
	// first carries a violation; the second (the re-upload) is clean.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	seedScopePrecheckAudit(fb, runID, server.ScopePrecheckPayload{
		WorkflowID:   "feature_change",
		ScannedFiles: 1,
		Violations: []policy.Violation{
			{Constraint: "forbidden_paths", Detail: "stale", Files: []string{".github/workflows/ci.yml"}},
		},
	})
	seedScopePrecheckAudit(fb, runID, server.ScopePrecheckPayload{
		WorkflowID:   "feature_change",
		ScannedFiles: 2,
		Violations:   []policy.Violation{},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.ScopePrecheck == nil {
		t.Fatal("ScopePrecheck is nil; want populated")
	}
	if out.ScopePrecheck.ScannedFiles != 2 {
		t.Errorf("ScannedFiles = %d, want 2 (newest entry)", out.ScopePrecheck.ScannedFiles)
	}
	if len(out.ScopePrecheck.Violations) != 0 {
		t.Errorf("newest entry is clean; want zero violations, got %+v", out.ScopePrecheck.Violations)
	}
}

func TestGetPlan_ScopePrecheck_AbsentWhenNoEntry(t *testing.T) {
	// An older run predating the pre-check has no plan_scope_precheck
	// entry — ScopePrecheck must be nil so the field is omitted.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.ScopePrecheck != nil {
		t.Errorf("ScopePrecheck should be nil with no entry; got %+v", out.ScopePrecheck)
	}
}

// seedSurfaceSweepAudit marshals a SERVER-side SurfaceSweepPayload and
// feeds it back through the fake backend as a plan_surface_sweep audit
// entry. Using the real server type is the point of the seam test (#618):
// it exercises the backend-write -> mcp-read JSON contract end to end, so a
// drift in either side's struct tags fails here rather than silently in
// production.
func seedSurfaceSweepAudit(fb *fakeBackend, runID uuid.UUID, payload server.SurfaceSweepPayload) {
	raw, _ := json.Marshal(payload)
	var decoded any
	_ = json.Unmarshal(raw, &decoded)
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: "plan_surface_sweep",
		Payload:  decoded,
	})
	fb.mu.Unlock()
}

func TestGetPlan_SurfaceSweep_CrossBoundarySeam(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	seedSurfaceSweepAudit(fb, runID, server.SurfaceSweepPayload{
		ScannedFiles: 1,
		Findings: []server.SurfaceSweepFinding{
			{
				Pattern:         "actor @-mention render surfaces",
				TriggerPath:     "backend/internal/issuecomment/status_template.go",
				MissingSiblings: []string{"backend/internal/issuecomment/notifier.go"},
			},
		},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.SurfaceSweep == nil {
		t.Fatal("SurfaceSweep is nil; want populated")
	}
	if out.SurfaceSweep.ScannedFiles != 1 {
		t.Errorf("ScannedFiles = %d, want 1", out.SurfaceSweep.ScannedFiles)
	}
	if got := len(out.SurfaceSweep.Findings); got != 1 {
		t.Fatalf("len(Findings) = %d, want 1", got)
	}
	f := out.SurfaceSweep.Findings[0]
	if f.Pattern != "actor @-mention render surfaces" {
		t.Errorf("Pattern = %q", f.Pattern)
	}
	if f.TriggerPath != "backend/internal/issuecomment/status_template.go" {
		t.Errorf("TriggerPath = %q", f.TriggerPath)
	}
	if len(f.MissingSiblings) != 1 || f.MissingSiblings[0] != "backend/internal/issuecomment/notifier.go" {
		t.Errorf("MissingSiblings = %v, want [notifier.go]", f.MissingSiblings)
	}
}

func TestGetPlan_SurfaceSweep_NewestEntryWins(t *testing.T) {
	// A schema-retry run writes two plan_surface_sweep entries; the
	// authoritative one is the newest (last, sequence-ascending). The first
	// carries a finding; the second (the re-upload) is clean.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	seedSurfaceSweepAudit(fb, runID, server.SurfaceSweepPayload{
		ScannedFiles: 1,
		Findings: []server.SurfaceSweepFinding{
			{Pattern: "stale", TriggerPath: "x", MissingSiblings: []string{"y"}},
		},
	})
	seedSurfaceSweepAudit(fb, runID, server.SurfaceSweepPayload{
		ScannedFiles: 2,
		Findings:     []server.SurfaceSweepFinding{},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.SurfaceSweep == nil {
		t.Fatal("SurfaceSweep is nil; want populated")
	}
	if out.SurfaceSweep.ScannedFiles != 2 {
		t.Errorf("ScannedFiles = %d, want 2 (newest entry)", out.SurfaceSweep.ScannedFiles)
	}
	if len(out.SurfaceSweep.Findings) != 0 {
		t.Errorf("newest entry is clean; want zero findings, got %+v", out.SurfaceSweep.Findings)
	}
}

func TestGetPlan_SurfaceSweep_AbsentWhenNoEntry(t *testing.T) {
	// An older run predating the sweep has no plan_surface_sweep entry —
	// SurfaceSweep must be nil so the field is omitted.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.SurfaceSweep != nil {
		t.Errorf("SurfaceSweep should be nil with no entry; got %+v", out.SurfaceSweep)
	}
}

// seedTestSweepAudit marshals a SERVER-side TestSweepPayload and feeds it
// back through the fake backend as a plan_test_sweep audit entry. Using
// the real server type is the point of the seam test (#618): it exercises
// the backend-write -> mcp-read JSON contract end to end, so a drift in
// either side's struct tags fails here rather than silently in production.
func seedTestSweepAudit(fb *fakeBackend, runID uuid.UUID, payload server.TestSweepPayload) {
	raw, _ := json.Marshal(payload)
	var decoded any
	_ = json.Unmarshal(raw, &decoded)
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: int64(len(fb.perRunAuditByRun[runID]) + 1),
		RunID:    runID.String(),
		Category: "plan_test_sweep",
		Payload:  decoded,
	})
	fb.mu.Unlock()
}

func TestGetPlan_TestSweep_CrossBoundarySeam(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	seedTestSweepAudit(fb, runID, server.TestSweepPayload{
		ScannedFiles: 3,
		ListedDirs:   2,
		Findings: []server.TestSweepFinding{
			{
				Rule:         "stem_sibling",
				TriggerPath:  "backend/internal/server/upload.go",
				MissingTests: []string{"backend/internal/server/upload_test.go"},
			},
			{
				Rule:         "new_test_in_tested_package",
				TriggerPath:  "backend/internal/server/feature_test.go",
				MissingTests: []string{"backend/internal/server/a_test.go"},
				OmittedCount: 3,
			},
		},
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.TestSweep == nil {
		t.Fatal("TestSweep is nil; want populated")
	}
	if out.TestSweep.ScannedFiles != 3 || out.TestSweep.ListedDirs != 2 {
		t.Errorf("ScannedFiles/ListedDirs = %d/%d, want 3/2", out.TestSweep.ScannedFiles, out.TestSweep.ListedDirs)
	}
	if got := len(out.TestSweep.Findings); got != 2 {
		t.Fatalf("len(Findings) = %d, want 2", got)
	}
	f := out.TestSweep.Findings[0]
	if f.Rule != "stem_sibling" || f.TriggerPath != "backend/internal/server/upload.go" {
		t.Errorf("Findings[0] = %+v", f)
	}
	if len(f.MissingTests) != 1 || f.MissingTests[0] != "backend/internal/server/upload_test.go" {
		t.Errorf("MissingTests = %v, want [upload_test.go]", f.MissingTests)
	}
	if f.OmittedCount != 0 {
		t.Errorf("Findings[0].OmittedCount = %d, want 0", f.OmittedCount)
	}
	if out.TestSweep.Findings[1].OmittedCount != 3 {
		t.Errorf("Findings[1].OmittedCount = %d, want 3", out.TestSweep.Findings[1].OmittedCount)
	}
}

func TestGetPlan_TestSweep_AbsentWhenNoEntry(t *testing.T) {
	// An older run predating the test sweep (or a fail-open no-op: non-
	// GitHub trigger, no GitHub client) has no plan_test_sweep entry —
	// TestSweep must be nil so the field is omitted.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.TestSweep != nil {
		t.Errorf("TestSweep should be nil with no entry; got %+v", out.TestSweep)
	}
}

func TestGetPlan_ReviewAuditError_Surfaced(t *testing.T) {
	// The per-run audit endpoint returns 500 → the error propagates
	// through loadPlanReviews and surfaces as a getPlan error.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)
	fb.perRunAuditStatus = http.StatusInternalServerError

	r := newResolver(srv, nil)
	_, _, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected error when plan_reviewed audit query fails")
	}
	if !strings.Contains(err.Error(), "load plan reviews") {
		t.Errorf("error should mention 'load plan reviews': %v", err)
	}
}

func TestGetPlan_MalformedReviewPayload_Skipped(t *testing.T) {
	// An audit entry whose payload doesn't decode to PlanReview shape
	// is silently skipped; a valid entry that follows still appears.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	// Malformed entry: payload is a string, not an object.
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: 1,
		RunID:    runID.String(),
		Category: "plan_reviewed",
		Payload:  "not-a-review-object",
	})
	// Valid entry follows.
	seedPlanReviewAudit(fb, runID, PlanReview{
		ReviewerKind: "agent",
		Authority:    "gating",
		Verdict:      "reject",
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "available" {
		t.Fatalf("Status = %q, want available", out.Status)
	}
	// The malformed entry is skipped; only the valid entry appears.
	if got := len(out.Reviews); got != 1 {
		t.Fatalf("len(Reviews) = %d, want 1 (malformed entry skipped)", got)
	}
	if out.Reviews[0].Verdict != "reject" {
		t.Errorf("Reviews[0].Verdict = %q, want reject", out.Reviews[0].Verdict)
	}
}

func TestGetPlan_ReviewsQueryUsesResolvedRunID(t *testing.T) {
	// CI-retry: child run has no plan stage; parent has the plan.
	// Reviews should be queried against the PARENT run (the one
	// where the plan artifact lives), not the child.
	fb, srv := newFakeBackend(t)
	parentID := uuid.New()
	childID := uuid.New()
	parentPlanStage := uuid.New()

	parentIDStr := parentID.String()
	fb.getRunByID[childID] = Run{
		ID:          childID.String(),
		ParentRunID: &parentIDStr,
		State:       "running",
		Repo:        "x/y",
	}
	fb.getRunByID[parentID] = Run{ID: parentID.String(), State: "running", Repo: "x/y"}
	fb.stagesByRun[childID] = []Stage{
		{ID: uuid.New().String(), RunID: childID.String(), Type: "implement", State: "running"},
	}
	fb.stagesByRun[parentID] = []Stage{
		{ID: parentPlanStage.String(), RunID: parentID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, parentPlanStage, samplePlanContent(), time.Hour)

	// Seed a review on the PARENT run.
	seedPlanReviewAudit(fb, parentID, PlanReview{
		ReviewerKind: "agent",
		Authority:    "advisory",
		Verdict:      "approve",
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: childID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.Status != "available" {
		t.Fatalf("Status = %q, want available", out.Status)
	}
	if out.ResolvedVia != "parent:"+parentID.String() {
		t.Errorf("ResolvedVia = %q", out.ResolvedVia)
	}
	// Reviews come from the parent (the resolved run), not the child.
	if got := len(out.Reviews); got != 1 {
		t.Fatalf("len(Reviews) = %d, want 1 (review seeded on parent)", got)
	}
	if out.Reviews[0].Verdict != "approve" {
		t.Errorf("Reviews[0].Verdict = %q, want approve", out.Reviews[0].Verdict)
	}
}

// --- get_run_status (E19.5 / #345) ---

func auditFixture(seq int64, runID uuid.UUID, category, actor string, offset time.Duration) AuditEntry {
	body, _ := json.Marshal(map[string]any{"actor": actor})
	return AuditEntry{
		ID:           uuid.New().String(),
		Sequence:     seq,
		RunID:        runID.String(),
		Timestamp:    time.Now().UTC().Add(-offset),
		Category:     category,
		ActorSubject: &actor,
		Payload:      body,
		EntryHash:    "h",
	}
}

func TestGetRunStatus_HappyPath_BundlesThreeReads(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{
		ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change",
		State: "running",
	}
	planStageID := uuid.New()
	implStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded",
			Executor: StageExecutor{Kind: "agent", Ref: "claude-code"}},
		{ID: implStageID.String(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "running",
			Executor: StageExecutor{Kind: "agent", Ref: "claude-code"}},
	}
	fb.auditByRun[runID] = []AuditEntry{
		// Returned time-descending — the fake serves what's there
		// without re-sorting; the production /v0/audit endpoint
		// orders so. Tests load these in the expected order.
		auditFixture(3, runID, "approval_submitted", "alice", 1*time.Minute),
		auditFixture(2, runID, "plan_generated", "system", 10*time.Minute),
		auditFixture(1, runID, "run_dispatched", "github-webhook", 15*time.Minute),
	}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}

	if out.Run.ID != runID.String() {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, runID)
	}
	if len(out.Stages) != 2 {
		t.Fatalf("expected 2 stages; got %d", len(out.Stages))
	}
	if out.Stages[0].Type != "plan" || out.Stages[1].Type != "implement" {
		t.Errorf("stages not in sequence order: %+v", out.Stages)
	}
	if len(out.RecentAudit) != 3 {
		t.Fatalf("expected 3 audit rows; got %d", len(out.RecentAudit))
	}
	if out.RecentAudit[0].Category != "approval_submitted" {
		t.Errorf("first audit row should be newest (approval_submitted); got %q", out.RecentAudit[0].Category)
	}
}

// TestGetRunStatus_StageWaitStatus_PropagatesEndToEnd drives the full
// getRunStatus handler against the fake backend to cover the cross-layer seam
// (#879/#880, cf. #618): backend Stage.State -> stageWaitStatusFor derivation
// -> tool output rendering, as one flow. A running implement stage propagates
// status=="running" + poll_interval_seconds==30; a terminal (succeeded) plan
// stage omits the interval.
func TestGetRunStatus_StageWaitStatus_PropagatesEndToEnd(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "running"}
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.New().String(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: uuid.New().String(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "running"},
	}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}

	if out.PlanStageWaitStatus == nil {
		t.Fatal("PlanStageWaitStatus is nil")
	}
	if out.PlanStageWaitStatus.Status != "succeeded" {
		t.Errorf("plan status = %q, want succeeded", out.PlanStageWaitStatus.Status)
	}
	if out.PlanStageWaitStatus.PollIntervalSeconds != 0 {
		t.Errorf("plan poll_interval_seconds = %d, want 0 (terminal omits it)", out.PlanStageWaitStatus.PollIntervalSeconds)
	}

	if out.ImplementStageWaitStatus == nil {
		t.Fatal("ImplementStageWaitStatus is nil")
	}
	if out.ImplementStageWaitStatus.Status != "running" {
		t.Errorf("implement status = %q, want running", out.ImplementStageWaitStatus.Status)
	}
	if out.ImplementStageWaitStatus.PollIntervalSeconds != 30 {
		t.Errorf("implement poll_interval_seconds = %d, want 30", out.ImplementStageWaitStatus.PollIntervalSeconds)
	}
}

// TestGetRunStatus_StageWaitStatus_RunTerminalBackstop asserts the ADR-036
// (#874) backstop propagates through the handler: a running stage under a
// terminal run keeps 'running' but drops the poll interval.
func TestGetRunStatus_StageWaitStatus_RunTerminalBackstop(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "failed"}
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.New().String(), RunID: runID.String(), Sequence: 1, Type: "implement", State: "running"},
	}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.ImplementStageWaitStatus == nil {
		t.Fatal("ImplementStageWaitStatus is nil")
	}
	if out.ImplementStageWaitStatus.Status != "running" {
		t.Errorf("status = %q, want running", out.ImplementStageWaitStatus.Status)
	}
	if out.ImplementStageWaitStatus.PollIntervalSeconds != 0 {
		t.Errorf("poll_interval_seconds = %d, want 0 (run terminal -> backstop drops it)", out.ImplementStageWaitStatus.PollIntervalSeconds)
	}
}

// TestGetRunStatus_DriveStatus_PropagatesEndToEnd drives the full
// getRunStatus handler against the fake backend to cover the
// cross-layer seam (#1023, cf. #618): backend drive read surfaces
// (drive / derived_status / next_action / auto_advanced on
// GET /v0/runs/{id}) -> runDriveView decode -> drive_status rendering,
// as one flow.
func TestGetRunStatus_DriveStatus_PropagatesEndToEnd(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	fb.getRunExtraByID[runID] = map[string]any{
		"drive":          true,
		"derived_status": "awaiting_merge",
		"next_action": map[string]any{
			"action": "merge_pr",
			"detail": "all gates resolved and required checks are green",
			"pr_url": "https://github.com/x/y/pull/42",
		},
		"auto_advanced": []map[string]any{
			{"rule": "plan_approved_dispatch", "from": "plan:approved", "to": "implement:dispatched", "ts": "2026-06-12T10:00:00Z"},
			{"rule": "checks_green_awaiting_merge", "from": "review:awaiting_approval", "to": "awaiting_merge", "ts": "2026-06-12T10:30:00Z"},
		},
	}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}

	ds := out.DriveStatus
	if ds == nil {
		t.Fatal("DriveStatus is nil, want the drive read view")
	}
	if !ds.Drive {
		t.Error("DriveStatus.Drive = false, want true")
	}
	if ds.DerivedStatus != "awaiting_merge" {
		t.Errorf("derived_status = %q, want awaiting_merge", ds.DerivedStatus)
	}
	if ds.NextAction == nil || ds.NextAction.Action != "merge_pr" || ds.NextAction.PRURL != "https://github.com/x/y/pull/42" {
		t.Errorf("next_action = %+v, want merge_pr with the PR URL", ds.NextAction)
	}
	if len(ds.AutoAdvanced) != 2 {
		t.Fatalf("auto_advanced = %+v, want 2 entries", ds.AutoAdvanced)
	}
	if ds.AutoAdvanced[0].Rule != "plan_approved_dispatch" || ds.AutoAdvanced[1].Rule != "checks_green_awaiting_merge" {
		t.Errorf("auto_advanced rules = [%q %q], want oldest-first order preserved",
			ds.AutoAdvanced[0].Rule, ds.AutoAdvanced[1].Rule)
	}
	if ds.AutoAdvanced[0].Timestamp.IsZero() {
		t.Error("auto_advanced[0].ts is zero, want the backend timestamp decoded")
	}
	// The Run mirror still rides along untouched.
	if out.Run.ID != runID.String() {
		t.Errorf("Run.ID = %s, want %s", out.Run.ID, runID)
	}
	// #1024: the drive next_action folds into next_actions as the FIRST
	// entry, so the two surfaces never point different ways.
	if out.NextActions == nil || len(out.NextActions.Actions) == 0 {
		t.Fatalf("NextActions = %+v, want the drive action folded in", out.NextActions)
	}
	if out.NextActions.Actions[0].Action != "merge_pr" {
		t.Errorf("next_actions.actions[0] = %q, want the drive merge_pr first", out.NextActions.Actions[0].Action)
	}
}

// TestGetRunStatus_NextActions_PlanGateParked is the get_run_status half
// of the #1024 wiring: the snapshot carries the next_actions block
// computed from the same run/stage/review reads, here at the parked plan
// gate (no review entries → review status none → approve/reject offered).
func TestGetRunStatus_NextActions_PlanGateParked(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 1, Type: "plan", State: "awaiting_approval"},
		{ID: uuid.NewString(), RunID: runID.String(), Sequence: 2, Type: "implement", State: "pending"},
	}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.NextActions == nil {
		t.Fatal("NextActions is nil; want the #1024 block on every run")
	}
	if out.NextActions.State != "plan_gate_parked" {
		t.Errorf("next_actions.state = %q, want plan_gate_parked", out.NextActions.State)
	}
	names := make([]string, 0, len(out.NextActions.Actions))
	for _, a := range out.NextActions.Actions {
		names = append(names, a.Action)
	}
	if len(names) != 2 || names[0] != "fishhawk_approve_plan" || names[1] != "fishhawk_reject_plan" {
		t.Errorf("next_actions.actions = %v, want [fishhawk_approve_plan fishhawk_reject_plan]", names)
	}
}

// TestGetRunStatus_NextActions_TerminalRunNamesState pins the terminal
// shape: the block is still present naming the state, with no actions.
func TestGetRunStatus_NextActions_TerminalRunNamesState(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "cancelled"}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.NextActions == nil || out.NextActions.State != "cancelled" {
		t.Fatalf("NextActions = %+v, want a block naming the terminal state cancelled", out.NextActions)
	}
	if len(out.NextActions.Actions) != 0 {
		t.Errorf("terminal run carries actions %+v, want none", out.NextActions.Actions)
	}
}

// TestGetRunStatus_NonDriveRun_OmitsDriveStatus is the control: a run
// without drive surfaces (legacy / drive:false) renders no
// drive_status block at all.
func TestGetRunStatus_NonDriveRun_OmitsDriveStatus(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.DriveStatus != nil {
		t.Errorf("DriveStatus = %+v, want nil on a non-drive run", out.DriveStatus)
	}
}

// TestGetRunStatus_DriveRun_NoAdvancesYet pins the early-run shape: a
// drive:true run with no recorded transitions still gets the block
// (drive:true) with the lists empty — the operator can tell drive is
// armed before anything has advanced.
func TestGetRunStatus_DriveRun_NoAdvancesYet(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}
	fb.getRunExtraByID[runID] = map[string]any{"drive": true}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.DriveStatus == nil || !out.DriveStatus.Drive {
		t.Fatalf("DriveStatus = %+v, want drive:true with empty surfaces", out.DriveStatus)
	}
	if out.DriveStatus.NextAction != nil || len(out.DriveStatus.AutoAdvanced) != 0 || out.DriveStatus.DerivedStatus != "" {
		t.Errorf("DriveStatus = %+v, want empty surfaces before any advance", out.DriveStatus)
	}
}

func TestGetRunStatus_StagesReSortedBySequence(t *testing.T) {
	// Defensive sort: even if the backend ever stops ordering by
	// sequence, the agent still sees the pipeline in order.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String()}
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.New().String(), Sequence: 3, Type: "review", State: "pending"},
		{ID: uuid.New().String(), Sequence: 1, Type: "plan", State: "succeeded"},
		{ID: uuid.New().String(), Sequence: 2, Type: "implement", State: "running"},
	}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	got := []int{out.Stages[0].Sequence, out.Stages[1].Sequence, out.Stages[2].Sequence}
	if got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("stage sequences = %v, want [1,2,3]", got)
	}
}

func TestGetRunStatus_AuditLimit_DefaultsToFive(t *testing.T) {
	// audit_limit unset → request goes out with limit=5.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String()}

	r := newResolver(srv, nil)
	_, _, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if fb.lastAuditLimit != "5" {
		t.Errorf("audit request limit = %q, want 5", fb.lastAuditLimit)
	}
}

func TestGetRunStatus_AuditLimit_ClampedToFifty(t *testing.T) {
	// audit_limit > 50 → request goes out with limit=50.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String()}

	r := newResolver(srv, nil)
	_, _, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{
		RunID:      runID.String(),
		AuditLimit: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fb.lastAuditLimit != "50" {
		t.Errorf("audit request limit = %q, want 50 (clamped)", fb.lastAuditLimit)
	}
}

func TestGetRunStatus_AuditLimit_ExplicitValueForwarded(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String()}

	r := newResolver(srv, nil)
	_, _, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{
		RunID:      runID.String(),
		AuditLimit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fb.lastAuditLimit != "20" {
		t.Errorf("audit request limit = %q, want 20", fb.lastAuditLimit)
	}
}

func TestGetRunStatus_RejectsInvalidUUID(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: "not-a-uuid"})
	if err == nil {
		t.Fatal("expected error on malformed run_id")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetRunStatus_MissingRun_404Surfaced(t *testing.T) {
	// GetRun returns 404 → the wrapped error reaches the caller.
	fb, srv := newFakeBackend(t)
	fb.getStatus = http.StatusNotFound

	r := newResolver(srv, nil)
	_, _, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: uuid.New().String()})
	if err == nil {
		t.Fatal("expected 404 to surface")
	}
	if !strings.Contains(err.Error(), "get run") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetRunStatus_StagesEndpointError_Surfaced(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String()}
	fb.stagesStatus = http.StatusInternalServerError

	r := newResolver(srv, nil)
	_, _, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected stages 500 to surface")
	}
	if !strings.Contains(err.Error(), "list stages") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetRunStatus_AuditEndpointError_Surfaced(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String()}
	fb.auditStatus = http.StatusInternalServerError

	r := newResolver(srv, nil)
	_, _, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected audit 500 to surface")
	}
	if !strings.Contains(err.Error(), "list recent audit") {
		t.Errorf("error wording: %v", err)
	}
}

func TestGetRunStatus_EmptyStagesAndAudit_OK(t *testing.T) {
	// Brand-new run before any stages or audit rows landed —
	// still returns Status=ok with empty arrays rather than erroring.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), State: "pending"}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Run.ID != runID.String() {
		t.Errorf("Run.ID = %s", out.Run.ID)
	}
	if got := len(out.Stages); got != 0 {
		t.Errorf("Stages length = %d, want 0", got)
	}
	if got := len(out.RecentAudit); got != 0 {
		t.Errorf("RecentAudit length = %d, want 0", got)
	}
}

func TestGetPlan_IgnoresNonStandardV1PlanArtifacts(t *testing.T) {
	// A plan stage might carry future-schema artifacts. The
	// resolver only returns standard_v1 — anything else is invisible
	// to v0's MCP tools.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"}}

	v := "future_v2"
	body, _ := json.Marshal(map[string]any{"plan_version": "future_v2"})
	fb.artifactsByStage[planStageID] = []Artifact{{
		ID: uuid.New().String(), StageID: planStageID.String(), Kind: "plan",
		SchemaVersion: &v, Content: body,
		CreatedAt: time.Now().UTC(),
	}}

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "no_plan_yet" {
		t.Errorf("Status = %q, want no_plan_yet (future schema is invisible)", out.Status)
	}
}

// --- list_audit (E19.6 / #346) ---

func TestListAudit_HappyPath_DefaultsLimit(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.perRunAuditByRun[runID] = []AuditEntry{
		auditFixture(1, runID, "run_dispatched", "github-webhook", 30*time.Minute),
		auditFixture(2, runID, "plan_generated", "system", 15*time.Minute),
		auditFixture(3, runID, "approval_submitted", "alice", 5*time.Minute),
	}

	r := newResolver(srv, nil)
	_, out, err := r.listAudit(context.Background(), nil, ListAuditInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("listAudit: %v", err)
	}
	if got := len(out.Items); got != 3 {
		t.Errorf("Items length = %d, want 3", got)
	}
	q := fb.perRunAuditLastQueryByID[runID]
	if !strings.Contains(q, "limit=50") {
		t.Errorf("expected default limit=50; got %q", q)
	}
	// No filters → no category / stage_id / cursor in the query.
	for _, unwanted := range []string{"category=", "stage_id=", "cursor="} {
		if strings.Contains(q, unwanted) {
			t.Errorf("unfiltered call should not carry %q; got %q", unwanted, q)
		}
	}
}

func TestListAudit_FiltersForwarded(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	stageID := uuid.New()

	r := newResolver(srv, nil)
	_, _, err := r.listAudit(context.Background(), nil, ListAuditInput{
		RunID:    runID.String(),
		Category: "approval_submitted",
		StageID:  stageID.String(),
		Limit:    25,
		Cursor:   "tok-abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	q := fb.perRunAuditLastQueryByID[runID]
	for _, want := range []string{
		"category=approval_submitted",
		"stage_id=" + stageID.String(),
		"limit=25",
		"cursor=tok-abc",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query missing %q: %s", want, q)
		}
	}
}

func TestListAudit_Limit_ClampedTo200(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()

	r := newResolver(srv, nil)
	_, _, err := r.listAudit(context.Background(), nil, ListAuditInput{
		RunID: runID.String(),
		Limit: 5000,
	})
	if err != nil {
		t.Fatal(err)
	}
	q := fb.perRunAuditLastQueryByID[runID]
	if !strings.Contains(q, "limit=200") {
		t.Errorf("limit should clamp to 200; got %q", q)
	}
}

func TestListAudit_NextCursorPropagated(t *testing.T) {
	// Page 1 returns a next_cursor; the tool surfaces it so the
	// agent can call again with cursor=<token>. Verify both the
	// outbound forwarding and the inbound round-trip.
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.perRunAuditByRun[runID] = []AuditEntry{
		auditFixture(1, runID, "run_dispatched", "github-webhook", time.Hour),
	}
	fb.perRunAuditNextByRun[runID] = "tok-page2"

	r := newResolver(srv, nil)
	_, out, err := r.listAudit(context.Background(), nil, ListAuditInput{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if out.NextCursor != "tok-page2" {
		t.Errorf("NextCursor = %q, want tok-page2", out.NextCursor)
	}

	// Round-trip: feed the cursor back in.
	_, _, err = r.listAudit(context.Background(), nil, ListAuditInput{
		RunID:  runID.String(),
		Cursor: "tok-page2",
	})
	if err != nil {
		t.Fatal(err)
	}
	q := fb.perRunAuditLastQueryByID[runID]
	if !strings.Contains(q, "cursor=tok-page2") {
		t.Errorf("page-2 call should forward cursor; got %q", q)
	}
}

func TestListAudit_BadRunUUID(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.listAudit(context.Background(), nil, ListAuditInput{RunID: "not-a-uuid"})
	if err == nil {
		t.Fatal("expected error on malformed run_id")
	}
	if !strings.Contains(err.Error(), "run_id") || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("error wording: %v", err)
	}
}

func TestListAudit_BadStageUUID_RejectedBeforeAPICall(t *testing.T) {
	// stage_id parses locally so a malformed input surfaces as a
	// clean tool error rather than a confusing backend 400.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.listAudit(context.Background(), nil, ListAuditInput{
		RunID:   uuid.New().String(),
		StageID: "nope",
	})
	if err == nil {
		t.Fatal("expected error on malformed stage_id")
	}
	if !strings.Contains(err.Error(), "stage_id") {
		t.Errorf("error should name the stage_id field: %v", err)
	}
	// Defensive: the backend must NOT have been hit when local
	// validation failed.
	if len(fb.perRunAuditLastQueryByID) != 0 {
		t.Errorf("backend hit despite local validation failure: %v", fb.perRunAuditLastQueryByID)
	}
}

func TestListAudit_BackendError_Surfaced(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.perRunAuditStatus = http.StatusInternalServerError

	r := newResolver(srv, nil)
	_, _, err := r.listAudit(context.Background(), nil, ListAuditInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected 500 to surface")
	}
	if !strings.Contains(err.Error(), "list audit") {
		t.Errorf("error wording: %v", err)
	}
}

func TestListAudit_MissingRun_404Surfaced(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.perRunAuditStatus = http.StatusNotFound

	r := newResolver(srv, nil)
	_, _, err := r.listAudit(context.Background(), nil, ListAuditInput{RunID: uuid.New().String()})
	if err == nil {
		t.Fatal("expected 404 to surface")
	}
}

func TestListAudit_EmptyPage_OK(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	// perRunAuditByRun left empty for this id.
	_ = fb

	r := newResolver(srv, nil)
	_, out, err := r.listAudit(context.Background(), nil, ListAuditInput{RunID: runID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(out.Items); got != 0 {
		t.Errorf("expected empty items; got %d", got)
	}
	if out.NextCursor != "" {
		t.Errorf("empty page should have empty cursor; got %q", out.NextCursor)
	}
}

func TestClampListAuditLimit(t *testing.T) {
	// Centralized clamp logic — test directly without the full
	// tool flow so future tweaks have a fast feedback loop.
	cases := []struct {
		in, want int
	}{
		{0, listAuditLimitDefault},
		{-1, listAuditLimitDefault},
		{1, 1},
		{50, 50},
		{200, 200},
		{201, listAuditLimitMax},
		{99999, listAuditLimitMax},
	}
	for _, tc := range cases {
		if got := clampListAuditLimit(tc.in); got != tc.want {
			t.Errorf("clampListAuditLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// --- fishhawk_start_run (E22.1 / #390) ---

func TestStartRun_HappyPath_PostsBodyReturnsRun(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, out, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:          "x/y",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: "cli",
		TriggerRef:    "issue:42",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if out.Run.ID == "" {
		t.Errorf("Run.ID empty; expected the fake to allocate one")
	}
	if out.Run.Repo != "x/y" || out.Run.WorkflowID != "feature_change" || out.Run.State != "pending" {
		t.Errorf("Run = %+v, want repo=x/y workflow=feature_change state=pending", out.Run)
	}
	if out.Idempotent {
		t.Errorf("Idempotent = true, want false (fresh create returns 201)")
	}
	// Backend received the right body.
	if fb.createRunBody.Repo != "x/y" {
		t.Errorf("backend got Repo = %q", fb.createRunBody.Repo)
	}
	if fb.createRunBody.WorkflowSHA != "deadbeef" {
		t.Errorf("backend got WorkflowSHA = %q", fb.createRunBody.WorkflowSHA)
	}
	if fb.createRunBody.TriggerSource != "cli" {
		t.Errorf("backend got TriggerSource = %q", fb.createRunBody.TriggerSource)
	}
	if fb.createRunBody.TriggerRef == nil || *fb.createRunBody.TriggerRef != "issue:42" {
		t.Errorf("backend got TriggerRef = %+v, want pointer to 'issue:42'", fb.createRunBody.TriggerRef)
	}
	if fb.createRunIdempKey != "" {
		t.Errorf("Idempotency-Key set without input: %q", fb.createRunIdempKey)
	}
}

func TestStartRun_TriggerSourceDefault_CLI(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:        "x/y",
		WorkflowID:  "feature_change",
		WorkflowSHA: "deadbeef",
		// TriggerSource omitted — defaults to "cli"
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunBody.TriggerSource != "cli" {
		t.Errorf("default TriggerSource = %q, want cli", fb.createRunBody.TriggerSource)
	}
}

func TestStartRun_IdempotencyKey_SetsHeader(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:           "x/y",
		WorkflowID:     "feature_change",
		WorkflowSHA:    "deadbeef",
		TriggerSource:  "cli",
		IdempotencyKey: "abc-123",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunIdempKey != "abc-123" {
		t.Errorf("Idempotency-Key header = %q, want abc-123", fb.createRunIdempKey)
	}
}

func TestStartRun_IdempotentReplay_FlagsTrue(t *testing.T) {
	// Backend returns 200 (instead of 201) to signal idempotent
	// replay. The MCP tool surfaces this on the Idempotent output
	// field so callers can branch.
	fb, srv := newFakeBackend(t)
	fb.createRunStatus = http.StatusOK
	r := newResolver(srv, nil)

	_, out, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:           "x/y",
		WorkflowID:     "feature_change",
		WorkflowSHA:    "deadbeef",
		TriggerSource:  "cli",
		IdempotencyKey: "abc-123",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if !out.Idempotent {
		t.Errorf("Idempotent = false, want true (backend served 200)")
	}
}

func TestStartRun_BackendValidationError_PropagatesAsToolError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.createRunStatus = http.StatusBadRequest
	fb.createRunErrBody = `{"error":{"code":"validation_failed","message":"repo is required","details":{"field":"repo"}}}`
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:          "x/y", // input passes our local validation
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: "cli",
	})
	if err == nil {
		t.Fatal("expected error from backend 400; got nil")
	}
	// Backend's typed error code should bubble through the wrap.
	if !strings.Contains(err.Error(), "validation_failed") {
		t.Errorf("err = %v, want it to mention validation_failed", err)
	}
}

func TestStartRun_LocalValidationCatchesBadInputs(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	cases := []struct {
		name string
		in   StartRunInput
		want string
	}{
		{
			name: "missing repo",
			in:   StartRunInput{WorkflowID: "x", WorkflowSHA: "y", TriggerSource: "cli"},
			want: "repo is required",
		},
		{
			name: "missing workflow_id",
			in:   StartRunInput{Repo: "x/y", WorkflowSHA: "y", TriggerSource: "cli"},
			want: "workflow_id is required",
		},
		{
			name: "missing workflow_sha",
			in:   StartRunInput{Repo: "x/y", WorkflowID: "x", TriggerSource: "cli"},
			want: "workflow_sha is required",
		},
		{
			name: "bad trigger_source",
			in:   StartRunInput{Repo: "x/y", WorkflowID: "x", WorkflowSHA: "y", TriggerSource: "bogus"},
			want: "trigger_source",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := r.startRun(context.Background(), nil, tc.in)
			if err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// --- fishhawk_start_run field parity (#426) ---

// validTrivialSpec is a minimal workflow YAML that passes the
// backend/internal/spec parser. Used across the parity tests so
// the auto-discover and inline-spec branches have a real bytes
// payload to ship.
const validTrivialSpec = "version: \"0.3\"\nworkflows:\n  trivial:\n    stages:\n      - id: implement\n        type: implement\n        executor:\n          agent: claude-code\n        produces:\n          - artifact: pull_request\n"

// TestStartRun_AutoDiscoversSpecFromWorkingDir exercises the
// headline #426 flow: an agent passes working_dir, the MCP server
// walks for .fishhawk/workflows.yaml, ships the bytes inline, and
// pre-computes workflow_sha so the agent doesn't need to.
func TestStartRun_AutoDiscoversSpecFromWorkingDir(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".fishhawk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".fishhawk", "workflows.yaml"),
		[]byte(validTrivialSpec), 0o600); err != nil {
		t.Fatal(err)
	}

	_, out, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:       "x/y",
		WorkflowID: "trivial",
		WorkingDir: dir,
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if out.Run.ID == "" {
		t.Error("Run.ID empty")
	}
	if fb.createRunBody.WorkflowSpec != validTrivialSpec {
		t.Errorf("WorkflowSpec not forwarded; got %q", fb.createRunBody.WorkflowSpec)
	}
	if fb.createRunBody.WorkflowSHA != gitBlobSHA([]byte(validTrivialSpec)) {
		t.Errorf("WorkflowSHA = %q, want auto-computed %q",
			fb.createRunBody.WorkflowSHA, gitBlobSHA([]byte(validTrivialSpec)))
	}
}

// TestStartRun_InlineWorkflowSpec_SkipsDiscovery covers the
// "agent already has the bytes" path — the MCP server still
// validates + computes the SHA but doesn't touch the disk.
func TestStartRun_InlineWorkflowSpec_SkipsDiscovery(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:         "x/y",
		WorkflowID:   "trivial",
		WorkflowSpec: validTrivialSpec,
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunBody.WorkflowSpec != validTrivialSpec {
		t.Errorf("inline spec not forwarded")
	}
	if fb.createRunBody.WorkflowSHA == "" {
		t.Error("SHA should be auto-computed when not provided")
	}
}

// TestStartRun_InlineSpec_InvalidYAMLFailsLocally surfaces the
// schema error without a backend round-trip. Matches the CLI's
// fast-fail UX.
func TestStartRun_InlineSpec_InvalidYAMLFailsLocally(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:         "x/y",
		WorkflowID:   "trivial",
		WorkflowSpec: "not: valid: yaml: ::\n",
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "workflow_spec") {
		t.Errorf("err should reference workflow_spec: %v", err)
	}
}

// TestStartRun_RunnerKindLocal_ForwardedToBackend covers the
// ADR-022 dimension: an agent minting a local-runner run passes
// runner_kind=local, the MCP forwards it verbatim.
func TestStartRun_RunnerKindLocal_ForwardedToBackend(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:         "x/y",
		WorkflowID:   "trivial",
		WorkflowSpec: validTrivialSpec,
		RunnerKind:   "local",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunBody.RunnerKind != "local" {
		t.Errorf("RunnerKind = %q, want local", fb.createRunBody.RunnerKind)
	}
}

// TestStartRun_BudgetOverride_ForwardedToBackend covers the #688
// admission-override dimension: an agent forcing a run past a blocking
// periodic budget passes budget_override=true, the MCP forwards it
// verbatim into the createRun request body.
func TestStartRun_BudgetOverride_ForwardedToBackend(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:           "x/y",
		WorkflowID:     "trivial",
		WorkflowSpec:   validTrivialSpec,
		BudgetOverride: true,
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if !fb.createRunBody.BudgetOverride {
		t.Errorf("BudgetOverride = false, want true")
	}
}

// TestStartRun_IssueFetch_AutoFlipsTriggerSource exercises the
// gh-fetch convenience: when the agent passes issue, the MCP
// server fetches via gh and ships the payload inline, AND flips
// trigger_source to github_issue.
func TestStartRun_IssueFetch_AutoFlipsTriggerSource(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeGh(t, `{"title":"Add foo","body":"We need foo helpers.","url":"https://github.com/x/y/issues/42","number":42}`)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:         "x/y",
		WorkflowID:   "trivial",
		WorkflowSpec: validTrivialSpec,
		Issue:        "42",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunBody.IssueContext == nil {
		t.Fatal("IssueContext not forwarded")
	}
	if fb.createRunBody.IssueContext.Body != "We need foo helpers." {
		t.Errorf("body mismatch: %q", fb.createRunBody.IssueContext.Body)
	}
	if fb.createRunBody.TriggerSource != "github_issue" {
		t.Errorf("TriggerSource = %q, want github_issue", fb.createRunBody.TriggerSource)
	}
	if fb.createRunBody.TriggerRef == nil || *fb.createRunBody.TriggerRef != "issue:42" {
		t.Errorf("TriggerRef = %v, want issue:42", fb.createRunBody.TriggerRef)
	}
}

// TestStartRun_IssueContextInline_NoFetch confirms that when the
// agent already has an IssueContext (e.g. fetched once and reused
// across replays), the MCP server doesn't re-shell to gh.
func TestStartRun_IssueContextInline_NoFetch(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	// Wire a gh that would explode if called.
	ghIssueCommand = func(_ string, _ ...string) *exec.Cmd {
		t.Fatal("gh should NOT have been called when IssueContext is inline")
		return nil
	}
	t.Cleanup(func() { ghIssueCommand = exec.Command })

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:          "x/y",
		WorkflowID:    "trivial",
		WorkflowSpec:  validTrivialSpec,
		TriggerSource: "github_issue",
		IssueContext: &IssueContext{
			Title:  "Pre-fetched",
			Body:   "Inline body.",
			URL:    "https://github.com/x/y/issues/99",
			Number: 99,
		},
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunBody.IssueContext == nil || fb.createRunBody.IssueContext.Number != 99 {
		t.Errorf("IssueContext not forwarded: %+v", fb.createRunBody.IssueContext)
	}
}

// TestStartRun_IssueContextRequiresGithubIssueSource mirrors the
// backend validation: issue_context only valid with
// trigger_source=github_issue. The MCP server fails locally with
// a clean message instead of round-tripping to a 422.
func TestStartRun_IssueContextRequiresGithubIssueSource(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:          "x/y",
		WorkflowID:    "trivial",
		WorkflowSpec:  validTrivialSpec,
		TriggerSource: "ui",
		IssueContext: &IssueContext{
			Title: "X", Body: "Y", URL: "https://github.com/x/y/issues/1", Number: 1,
		},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "issue_context") {
		t.Errorf("err should mention issue_context: %v", err)
	}
}

// TestStartRun_GhMissing_DoesNotFail keeps the pre-#415 behavior
// alive: a missing gh emits a warning on the tool result but the
// run still mints. trigger_source still flips to github_issue
// because the agent asked for an issue-triggered run.
func TestStartRun_GhMissing_DoesNotFail(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeGhMissing(t)

	meta, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:         "x/y",
		WorkflowID:   "trivial",
		WorkflowSpec: validTrivialSpec,
		Issue:        "42",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunBody.IssueContext != nil {
		t.Errorf("IssueContext should be nil when gh missing")
	}
	if fb.createRunBody.TriggerSource != "github_issue" {
		t.Errorf("TriggerSource = %q, want github_issue", fb.createRunBody.TriggerSource)
	}
	if meta == nil {
		t.Fatal("expected warning metadata on the tool result")
	}
}

// TestStartRun_TriggerRefIssue_AutoDerivesNumber: when the agent
// passes trigger_ref=issue:7 without issue, the MCP server still
// fetches via gh.
func TestStartRun_TriggerRefIssue_AutoDerivesNumber(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	withFakeGh(t, `{"title":"Auto","body":"Auto-derived.","url":"https://github.com/x/y/issues/7","number":7}`)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:         "x/y",
		WorkflowID:   "trivial",
		WorkflowSpec: validTrivialSpec,
		TriggerRef:   "issue:7",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunBody.IssueContext == nil || fb.createRunBody.IssueContext.Number != 7 {
		t.Errorf("IssueContext.Number not 7: %+v", fb.createRunBody.IssueContext)
	}
}

// TestStartRun_NoSpecNoSHA_FailsWithRemediation echoes the CLI's
// dual-remediation error when neither the discovery path nor an
// explicit workflow_sha can produce a SHA.
func TestStartRun_NoSpecNoSHA_FailsWithRemediation(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:       "x/y",
		WorkflowID: "trivial",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "workflow_sha") {
		t.Errorf("err should mention workflow_sha: %v", err)
	}
	if !strings.Contains(err.Error(), "working_dir") {
		t.Errorf("err should suggest working_dir: %v", err)
	}
}

// TestStartRun_LegacyStagelessSeed_StillWorks documents the
// "test fixture / no checkout" path: pass repo + workflow_id +
// workflow_sha, no spec, no issue — backend creates a stage-less
// row. This is the pre-#411 behavior the MCP tool MUST preserve
// so integration tests that seed rows directly don't break.
func TestStartRun_LegacyStagelessSeed_StillWorks(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo:        "x/y",
		WorkflowID:  "trivial",
		WorkflowSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if fb.createRunBody.WorkflowSpec != "" {
		t.Errorf("WorkflowSpec set when none provided: %q", fb.createRunBody.WorkflowSpec)
	}
	if fb.createRunBody.WorkflowSHA != "deadbeef" {
		t.Errorf("WorkflowSHA = %q, want deadbeef", fb.createRunBody.WorkflowSHA)
	}
}

// --- fishhawk_cancel_run (E22.2 / #391) ---

func TestCancelRun_HappyPath_TransitionsToCancelled(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.cancelResp[runID] = Run{ID: runID.String(), State: "cancelled", Repo: "x/y"}

	_, out, err := r.cancelRun(context.Background(), nil, CancelRunInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("cancelRun: %v", err)
	}
	if out.Run.State != "cancelled" {
		t.Errorf("State = %q, want cancelled", out.Run.State)
	}
	if out.Run.ID != runID.String() {
		t.Errorf("ID = %q, want %s", out.Run.ID, runID.String())
	}
	if fb.cancelCalledByID[runID] != 1 {
		t.Errorf("cancel called %d times, want 1", fb.cancelCalledByID[runID])
	}
}

func TestCancelRun_InvalidUUID_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.cancelRun(context.Background(), nil, CancelRunInput{RunID: "not-a-uuid"})
	if err == nil {
		t.Fatal("expected validation error for bad UUID")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("err = %v, want UUID parse error", err)
	}
	// The backend was never called — invalid UUID short-circuits.
	if len(fb.cancelCalledByID) != 0 {
		t.Errorf("backend cancel called %d times, want 0 (local validation should short-circuit)", len(fb.cancelCalledByID))
	}
}

func TestCancelRun_NotFound_PropagatesAsToolError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.cancelStatus = http.StatusNotFound
	fb.cancelErrBody = `{"error":{"code":"run_not_found","message":"no run with that id"}}`
	r := newResolver(srv, nil)

	_, _, err := r.cancelRun(context.Background(), nil, CancelRunInput{RunID: uuid.NewString()})
	if err == nil {
		t.Fatal("expected error from backend 404; got nil")
	}
	if !strings.Contains(err.Error(), "run_not_found") {
		t.Errorf("err = %v, want it to mention run_not_found", err)
	}
}

func TestCancelRun_AlreadyTerminal_PropagatesConflict(t *testing.T) {
	// Cancelling a run that's already terminal (succeeded / failed)
	// surfaces the backend's `invalid_state_transition` code as a
	// tool error. The state machine is the source of truth.
	fb, srv := newFakeBackend(t)
	fb.cancelStatus = http.StatusConflict
	fb.cancelErrBody = `{"error":{"code":"invalid_state_transition","message":"cannot transition succeeded → cancelled","details":{"from":"succeeded","to":"cancelled"}}}`
	r := newResolver(srv, nil)

	_, _, err := r.cancelRun(context.Background(), nil, CancelRunInput{RunID: uuid.NewString()})
	if err == nil {
		t.Fatal("expected error from backend 409; got nil")
	}
	if !strings.Contains(err.Error(), "invalid_state_transition") {
		t.Errorf("err = %v, want invalid_state_transition", err)
	}
}

func TestCancelRun_Idempotent_ReCancelSucceeds(t *testing.T) {
	// The backend treats re-cancel as idempotent (200 with the
	// cancelled run). The MCP tool surfaces both calls' results
	// without error.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	fb.cancelResp[runID] = Run{ID: runID.String(), State: "cancelled"}

	_, out1, err1 := r.cancelRun(context.Background(), nil, CancelRunInput{RunID: runID.String()})
	if err1 != nil {
		t.Fatalf("first cancel: %v", err1)
	}
	_, out2, err2 := r.cancelRun(context.Background(), nil, CancelRunInput{RunID: runID.String()})
	if err2 != nil {
		t.Fatalf("second cancel: %v", err2)
	}
	if out1.Run.State != "cancelled" || out2.Run.State != "cancelled" {
		t.Errorf("states = %q/%q, want cancelled/cancelled", out1.Run.State, out2.Run.State)
	}
	if fb.cancelCalledByID[runID] != 2 {
		t.Errorf("cancel called %d times, want 2", fb.cancelCalledByID[runID])
	}
}

// --- fishhawk_retry_stage (E22.3 / #392) ---

func TestRetryStage_HappyPath_CategoryA_PendingTransition(t *testing.T) {
	// Category-A retry (agent failure): backend flips failed →
	// pending and the orchestrator advances it. Test fixture
	// returns a Stage in State="pending" (the orchestrator advance
	// is a backend-internal concern).
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	stageID := uuid.New()
	runID := uuid.New()
	fb.retryResp[stageID] = Stage{
		ID:    stageID.String(),
		RunID: runID.String(),
		Type:  "implement",
		State: "pending",
	}

	_, out, err := r.retryStage(context.Background(), nil, RetryStageInput{StageID: stageID.String()})
	if err != nil {
		t.Fatalf("retryStage: %v", err)
	}
	if out.Stage.State != "pending" {
		t.Errorf("State = %q, want pending", out.Stage.State)
	}
	if out.Stage.ID != stageID.String() {
		t.Errorf("ID = %q, want %s", out.Stage.ID, stageID.String())
	}
	if fb.retryCalledByID[stageID] != 1 {
		t.Errorf("retry called %d times, want 1", fb.retryCalledByID[stageID])
	}
}

func TestRetryStage_HappyPath_CategoryD_SLATimeout_BackToAwaitingApproval(t *testing.T) {
	// Category-D SLA-timeout retry flips the stage back to
	// awaiting_approval (no workflow_dispatch, just re-opens the
	// gate). Backend returns the stage in that state.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	stageID := uuid.New()
	fb.retryResp[stageID] = Stage{
		ID:    stageID.String(),
		Type:  "plan",
		State: "awaiting_approval",
	}

	_, out, err := r.retryStage(context.Background(), nil, RetryStageInput{StageID: stageID.String()})
	if err != nil {
		t.Fatalf("retryStage: %v", err)
	}
	if out.Stage.State != "awaiting_approval" {
		t.Errorf("State = %q, want awaiting_approval", out.Stage.State)
	}
}

func TestRetryStage_InvalidUUID_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.retryStage(context.Background(), nil, RetryStageInput{StageID: "not-a-uuid"})
	if err == nil {
		t.Fatal("expected validation error for bad UUID")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("err = %v, want UUID parse error", err)
	}
	// Local validation short-circuits — backend never called.
	if len(fb.retryCalledByID) != 0 {
		t.Errorf("backend retry called %d times, want 0", len(fb.retryCalledByID))
	}
}

func TestRetryStage_NotFound_PropagatesAsToolError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.retryStatus = http.StatusNotFound
	fb.retryErrBody = `{"error":{"code":"stage_not_found","message":"no stage with that id"}}`
	r := newResolver(srv, nil)

	_, _, err := r.retryStage(context.Background(), nil, RetryStageInput{StageID: uuid.NewString()})
	if err == nil {
		t.Fatal("expected error from backend 404; got nil")
	}
	if !strings.Contains(err.Error(), "stage_not_found") {
		t.Errorf("err = %v, want it to mention stage_not_found", err)
	}
}

func TestRetryStage_NotApplicable_CategoryB_PropagatesAs422(t *testing.T) {
	// Category-B (constraint / policy) retries are explicitly NOT
	// applicable — the workflow or spec needs to change first. The
	// backend surfaces this as a 422 with code retry_not_applicable;
	// the MCP tool propagates the error envelope verbatim.
	fb, srv := newFakeBackend(t)
	fb.retryStatus = http.StatusUnprocessableEntity
	fb.retryErrBody = `{"error":{"code":"retry_not_applicable","message":"category B failures require a workflow change"}}`
	r := newResolver(srv, nil)

	_, _, err := r.retryStage(context.Background(), nil, RetryStageInput{StageID: uuid.NewString()})
	if err == nil {
		t.Fatal("expected error from backend 422; got nil")
	}
	if !strings.Contains(err.Error(), "retry_not_applicable") {
		t.Errorf("err = %v, want retry_not_applicable", err)
	}
}

// --- fishhawk_approve_plan + fishhawk_reject_plan (E22.4 / #393) ---

// seedPlanStage installs a plan stage on the fakeBackend's stages-
// for-run map so the resolver finds it. Returns the stage id for
// downstream assertions.
func seedPlanStage(fb *fakeBackend, runID uuid.UUID) uuid.UUID {
	stageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: stageID.String(), RunID: runID.String(), Type: "plan", State: "awaiting_approval", Sequence: 0},
		{ID: uuid.NewString(), RunID: runID.String(), Type: "implement", State: "pending", Sequence: 1},
	}
	return stageID
}

func TestApprovePlan_HappyPath_ResolvesAndPostsApprove(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	stageID := seedPlanStage(fb, runID)
	// gh resolves the operator's real login (#751); the approve tool
	// threads it through as approver_github_login.
	withFakeGh(t, "kuhlman-labs")

	_, out, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:  runID.String(),
		Reason: "looks good",
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if fb.approvalsBody.ApproverGithubLogin != "kuhlman-labs" {
		t.Errorf("approver_github_login = %q, want kuhlman-labs", fb.approvalsBody.ApproverGithubLogin)
	}
	if out.StageID != stageID.String() {
		t.Errorf("resolved StageID = %q, want %s", out.StageID, stageID.String())
	}
	if out.Stage.State != "succeeded" {
		t.Errorf("State = %q, want succeeded", out.Stage.State)
	}
	if fb.approvalsBody.Decision != "approve" {
		t.Errorf("decision = %q, want approve", fb.approvalsBody.Decision)
	}
	if fb.approvalsBody.Comment != "looks good" {
		t.Errorf("comment = %q, want 'looks good'", fb.approvalsBody.Comment)
	}
	if fb.approvalsCalledByID[stageID] != 1 {
		t.Errorf("approvals call count = %d, want 1", fb.approvalsCalledByID[stageID])
	}
}

// TestApprovePlan_AddScopeFiles_PlumbedToSubmitApproval pins the #824 wire
// seam: ApprovePlanInput.AddScopeFiles must reach the approvals request body
// the backend decodes (MCP input -> client approvalRequest -> HTTP body).
func TestApprovePlan_AddScopeFiles_PlumbedToSubmitApproval(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	paths := []string{"backend/internal/agenteval/testdata/corpus/newcase/", "go.work"}
	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:         runID.String(),
		Reason:        "fold the fixture dir",
		AddScopeFiles: paths,
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if !reflect.DeepEqual(fb.approvalsBody.AddScopeFiles, paths) {
		t.Errorf("add_scope_files = %v, want %v", fb.approvalsBody.AddScopeFiles, paths)
	}
}

func TestRejectPlan_HappyPath_ResolvesAndPostsReject(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	stageID := seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	_, out, err := r.rejectPlan(context.Background(), nil, RejectPlanInput{
		RunID:  runID.String(),
		Reason: "scope is too wide",
	})
	if err != nil {
		t.Fatalf("rejectPlan: %v", err)
	}
	if fb.approvalsBody.ApproverGithubLogin != "kuhlman-labs" {
		t.Errorf("approver_github_login = %q, want kuhlman-labs", fb.approvalsBody.ApproverGithubLogin)
	}
	if out.StageID != stageID.String() {
		t.Errorf("resolved StageID = %q, want %s", out.StageID, stageID.String())
	}
	if out.Stage.State != "failed" {
		t.Errorf("State = %q, want failed", out.Stage.State)
	}
	if fb.approvalsBody.Decision != "reject" {
		t.Errorf("decision = %q, want reject", fb.approvalsBody.Decision)
	}
	if fb.approvalsBody.Comment != "scope is too wide" {
		t.Errorf("comment = %q, want 'scope is too wide'", fb.approvalsBody.Comment)
	}
}

func TestApprovePlan_DuplicateSubmission_LabeledOutputAndLeadText(t *testing.T) {
	// #986: a duplicate-labeled 200 must reach the tool output as
	// duplicate_submission/prior_decision AND lead the result text with
	// an explicit no-op banner — the operator loop must never mistake a
	// duplicate for an effective approval.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	stageID := seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	fb.approvalsRespBody = fmt.Sprintf(
		`{"id":%q,"run_id":%q,"type":"plan","state":"succeeded","duplicate_submission":true,"prior_decision":"approve","prior_submitted_at":"2026-06-10T12:00:00Z"}`,
		stageID.String(), runID.String())

	res, out, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:  runID.String(),
		Reason: "second try without override",
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if !out.DuplicateSubmission {
		t.Errorf("DuplicateSubmission = false, want true")
	}
	if out.PriorDecision != "approve" {
		t.Errorf("PriorDecision = %q, want approve", out.PriorDecision)
	}
	if out.Stage.State != "succeeded" {
		t.Errorf("State = %q, want succeeded (unchanged)", out.Stage.State)
	}
	if res == nil || len(res.Content) == 0 {
		t.Fatal("expected a tool result carrying the duplicate banner; got nil/empty")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("Content[0] = %T, want *mcp.TextContent", res.Content[0])
	}
	if !strings.HasPrefix(text.Text, "duplicate submission — your prior approve decision") {
		t.Errorf("result text must lead with the duplicate banner, got %q", text.Text)
	}
	if !strings.Contains(text.Text, "gates were NOT re-run") {
		t.Errorf("result text must state gates did not re-run, got %q", text.Text)
	}
}

func TestRejectPlan_DuplicateSubmission_LabeledOutputAndLeadText(t *testing.T) {
	// Same #986 labeling on the reject tool: the prior decision named
	// in the banner is the EXISTING row's (approve), not this call's.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	stageID := seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	fb.approvalsRespBody = fmt.Sprintf(
		`{"id":%q,"run_id":%q,"type":"plan","state":"succeeded","duplicate_submission":true,"prior_decision":"approve","prior_submitted_at":"2026-06-10T12:00:00Z"}`,
		stageID.String(), runID.String())

	res, out, err := r.rejectPlan(context.Background(), nil, RejectPlanInput{
		RunID:  runID.String(),
		Reason: "changed my mind",
	})
	if err != nil {
		t.Fatalf("rejectPlan: %v", err)
	}
	if !out.DuplicateSubmission || out.PriorDecision != "approve" {
		t.Errorf("duplicate labeling = (%v, %q), want (true, approve)", out.DuplicateSubmission, out.PriorDecision)
	}
	if res == nil || len(res.Content) == 0 {
		t.Fatal("expected a tool result carrying the duplicate banner; got nil/empty")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("Content[0] = %T, want *mcp.TextContent", res.Content[0])
	}
	if !strings.HasPrefix(text.Text, "duplicate submission — your prior approve decision") {
		t.Errorf("result text must lead with the duplicate banner, got %q", text.Text)
	}
}

func TestApprovePlan_NonDuplicate_NoBannerNoFlags(t *testing.T) {
	// The non-duplicate path is unchanged: a bare Stage 200 (no #986
	// keys) decodes to zero-valued duplicate fields and, with gh
	// resolving cleanly, a nil tool result.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	res, out, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{
		RunID:  runID.String(),
		Reason: "looks good",
	})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if out.DuplicateSubmission || out.PriorDecision != "" {
		t.Errorf("duplicate labeling = (%v, %q), want zero values on a first submission", out.DuplicateSubmission, out.PriorDecision)
	}
	if res != nil {
		t.Errorf("tool result = %+v, want nil (no banner, no warning)", res)
	}
}

func TestApprovePlan_NoReason_PassesEmptyComment(t *testing.T) {
	// Reason is optional on approve; absent comment threads through
	// as an empty string. Backend treats empty comment as "no
	// comment recorded" per the existing approval row's nullable
	// comment column.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGhMissing(t)

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if fb.approvalsBody.Comment != "" {
		t.Errorf("comment = %q, want empty", fb.approvalsBody.Comment)
	}
}

// TestApprovePlan_ForwardsResolvedGithubLogin pins the #751 thread:
// the resolved gh login lands in the approval body's
// approver_github_login field for issue-thread `@`-mention rendering.
func TestApprovePlan_ForwardsResolvedGithubLogin(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGh(t, "kuhlman-labs")

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("approvePlan: %v", err)
	}
	if fb.approvalsBody.ApproverGithubLogin != "kuhlman-labs" {
		t.Errorf("approver_github_login = %q, want kuhlman-labs", fb.approvalsBody.ApproverGithubLogin)
	}
}

// TestApprovePlan_GhMissing_StillApprovesWithoutLogin keeps the
// approval best-effort (#751): a missing gh yields an empty login and
// a warning on the tool result, never a blocked approval.
func TestApprovePlan_GhMissing_StillApprovesWithoutLogin(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGhMissing(t)

	meta, out, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("approvePlan should not fail when gh is missing: %v", err)
	}
	if out.Stage.State != "succeeded" {
		t.Errorf("State = %q, want succeeded", out.Stage.State)
	}
	if fb.approvalsBody.ApproverGithubLogin != "" {
		t.Errorf("approver_github_login = %q, want empty when gh missing", fb.approvalsBody.ApproverGithubLogin)
	}
	if meta == nil || len(meta.Content) == 0 {
		t.Error("expected a warning on the tool result when gh is missing")
	}
}

func TestApprovePlan_NoPlanStage_FailsWithCleanError(t *testing.T) {
	// A run with no plan stage (e.g. a routine_change workflow that
	// skips planning) surfaces a clean tool error rather than a
	// generic "not found" from the backend.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	// Seed only an implement stage — no plan.
	fb.stagesByRun[runID] = []Stage{
		{ID: uuid.NewString(), RunID: runID.String(), Type: "implement", State: "pending"},
	}

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected error for run without plan stage")
	}
	if !strings.Contains(err.Error(), "no plan stage") {
		t.Errorf("err = %v, want it to mention 'no plan stage'", err)
	}
	// No approvals call should have fired — short-circuit on resolver failure.
	if len(fb.approvalsCalledByID) != 0 {
		t.Errorf("approvals called %d times after resolver failure, want 0", len(fb.approvalsCalledByID))
	}
}

func TestApprovePlan_InvalidUUID_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{RunID: "not-a-uuid"})
	if err == nil {
		t.Fatal("expected validation error for bad UUID")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("err = %v, want UUID parse error", err)
	}
	// No stages call should have fired either.
	if len(fb.stagesCalledByID) != 0 {
		t.Errorf("list-stages called %d times, want 0", len(fb.stagesCalledByID))
	}
}

func TestApprovePlan_BackendStateMachineRefusal_PropagatesAsToolError(t *testing.T) {
	// E.g. plan stage isn't in awaiting_approval anymore. The
	// backend's state-machine rejects the approve; we surface the
	// error envelope verbatim.
	fb, srv := newFakeBackend(t)
	fb.approvalsStatus = http.StatusConflict
	fb.approvalsErrBody = `{"error":{"code":"invalid_state_transition","message":"plan stage not in awaiting_approval"}}`
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGhMissing(t)

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected error from backend 409")
	}
	if !strings.Contains(err.Error(), "invalid_state_transition") {
		t.Errorf("err = %v, want invalid_state_transition", err)
	}
}

// TestApprovePlan_AgentReviewPending_SurfacesPollUntilLanded pins the ADR-036
// (#875) consumer boundary: when the backend refuses the approve with a 409
// agent_review_pending (a configured agent plan review still in-flight), the
// tool surfaces a typed, operator-actionable poll-until-landed message that
// carries the landed/configured counts from the error details — not a generic
// wrap — and does NOT auto-retry.
func TestApprovePlan_AgentReviewPending_SurfacesPollUntilLanded(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.approvalsStatus = http.StatusConflict
	fb.approvalsErrBody = `{"error":{"code":"agent_review_pending","message":"a configured agent plan review is still in-flight","details":{"stage_id":"x","configured_agents":2,"landed_terminal":1}}}`
	r := newResolver(srv, nil)
	runID := uuid.New()
	stageID := seedPlanStage(fb, runID)
	withFakeGhMissing(t)

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected error from backend 409 agent_review_pending")
	}
	msg := err.Error()
	if !strings.Contains(msg, "agent_review_pending") {
		t.Errorf("err = %v, want agent_review_pending code", err)
	}
	// Carries the landed/configured counts from the details.
	if !strings.Contains(msg, "1 of 2") {
		t.Errorf("err = %v, want '1 of 2' landed/configured counts", err)
	}
	// Operator-actionable: points at the poll-until-terminal verbs and retry.
	if !strings.Contains(msg, "fishhawk_get_plan") || !strings.Contains(msg, "retry") {
		t.Errorf("err = %v, want poll-until-landed guidance", err)
	}
	// No auto-retry inside the tool: exactly one approvals call.
	if fb.approvalsCalledByID[stageID] != 1 {
		t.Errorf("approvals call count = %d, want 1 (no auto-retry)", fb.approvalsCalledByID[stageID])
	}
}

// TestApprovePlan_AgentReviewPending_DegradesWhenDetailsAbsent covers the
// display edge flagged in the #875 implement review: when the backend's 409
// agent_review_pending carries no details (or unparseable counts), the tool
// must NOT print a misleading "0 of 0 landed" — it drops the count phrase but
// keeps the poll-until-landed guidance and the typed code.
func TestApprovePlan_AgentReviewPending_DegradesWhenDetailsAbsent(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.approvalsStatus = http.StatusConflict
	// No details object at all — the degraded path.
	fb.approvalsErrBody = `{"error":{"code":"agent_review_pending","message":"a configured agent plan review is still in-flight"}}`
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGhMissing(t)

	_, _, err := r.approvePlan(context.Background(), nil, ApprovePlanInput{RunID: runID.String()})
	if err == nil {
		t.Fatal("expected error from backend 409 agent_review_pending")
	}
	msg := err.Error()
	if !strings.Contains(msg, "agent_review_pending") {
		t.Errorf("err = %v, want agent_review_pending code", err)
	}
	// Must NOT claim a bogus "0 of 0" count when details are missing.
	if strings.Contains(msg, "0 of 0") {
		t.Errorf("err = %v, must not print misleading '0 of 0' count", err)
	}
	// Still operator-actionable: poll verbs + retry guidance present.
	if !strings.Contains(msg, "fishhawk_get_plan") || !strings.Contains(msg, "retry") {
		t.Errorf("err = %v, want poll-until-landed guidance", err)
	}
}

func TestRejectPlan_NoReason_PassesEmptyComment(t *testing.T) {
	// Reason is recommended on reject (CLI warns when missing) but
	// the MCP tool doesn't enforce — the audit log records the
	// absence and that's the source of truth.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	runID := uuid.New()
	seedPlanStage(fb, runID)
	withFakeGhMissing(t)

	_, _, err := r.rejectPlan(context.Background(), nil, RejectPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("rejectPlan: %v", err)
	}
	if fb.approvalsBody.Decision != "reject" {
		t.Errorf("decision = %q, want reject", fb.approvalsBody.Decision)
	}
	if fb.approvalsBody.Comment != "" {
		t.Errorf("comment = %q, want empty", fb.approvalsBody.Comment)
	}
}

// --- fishhawk_list_runs (E22.5 / #394) ---

func TestListRuns_HappyPath_ReturnsItemsAndCursor(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	id1, id2 := uuid.New(), uuid.New()
	fb.listResp = listRunsResult{
		Items: []Run{
			sampleRun(id1, "x/y", time.Hour),
			sampleRun(id2, "x/y", 2*time.Hour),
		},
		NextCursor: "b2Zmc2V0OjEw",
	}

	_, out, err := r.listRuns(context.Background(), nil, ListRunsInput{})
	if err != nil {
		t.Fatalf("listRuns: %v", err)
	}
	if len(out.Items) != 2 {
		t.Errorf("got %d items, want 2", len(out.Items))
	}
	if out.NextCursor != "b2Zmc2V0OjEw" {
		t.Errorf("NextCursor = %q, want passthrough", out.NextCursor)
	}
}

func TestListRuns_NoFilters_DefaultsLimit(t *testing.T) {
	// Limit=0 input should clamp to listRunsLimitDefault (50) so
	// the agent doesn't accidentally fetch the entire chain.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	fb.listResp = listRunsResult{Items: []Run{}}

	_, _, err := r.listRuns(context.Background(), nil, ListRunsInput{})
	if err != nil {
		t.Fatalf("listRuns: %v", err)
	}
	if !strings.Contains(fb.lastListQuery, "limit=50") {
		t.Errorf("query missing default limit: %s", fb.lastListQuery)
	}
}

func TestListRuns_ForwardsAllFilters(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	fb.listResp = listRunsResult{Items: []Run{}}

	_, _, err := r.listRuns(context.Background(), nil, ListRunsInput{
		Repo:       "x/y",
		WorkflowID: "feature_change",
		State:      "running",
		Limit:      25,
		Cursor:     "abc",
	})
	if err != nil {
		t.Fatalf("listRuns: %v", err)
	}
	for _, want := range []string{
		"repo=x%2Fy",
		"workflow_id=feature_change",
		"state=running",
		"limit=25",
		"cursor=abc",
	} {
		if !strings.Contains(fb.lastListQuery, want) {
			t.Errorf("query missing %q: %s", want, fb.lastListQuery)
		}
	}
}

func TestListRuns_BadState_FailsLocallyWithoutHTTPCall(t *testing.T) {
	// Closed-set check before the wire hop; backend would 400
	// either way, but local validation gives the agent a clearer
	// error.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.listRuns(context.Background(), nil, ListRunsInput{State: "bogus"})
	if err == nil {
		t.Fatal("expected validation error for bad state")
	}
	if !strings.Contains(err.Error(), "state") {
		t.Errorf("err = %v, want it to mention state", err)
	}
	if fb.lastListQuery != "" {
		t.Errorf("backend should not be called on bad state; query = %q", fb.lastListQuery)
	}
}

func TestListRuns_LimitClamp(t *testing.T) {
	for _, tc := range []struct {
		in   int
		want int
	}{
		{0, listRunsLimitDefault},
		{1, 1},
		{50, 50},
		{200, 200},
		{500, listRunsLimitMax},
		{-1, listRunsLimitDefault},
	} {
		if got := clampListRunsLimit(tc.in); got != tc.want {
			t.Errorf("clampListRunsLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestListRuns_CursorRoundTrip_WalksPagination(t *testing.T) {
	// Two-call pagination loop: first call returns a cursor;
	// second call feeds that cursor back and gets the next page.
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)
	id1, id2 := uuid.New(), uuid.New()

	// Seed two distinct responses keyed by query string.
	page1 := listRunsResult{
		Items:      []Run{sampleRun(id1, "x/y", time.Hour)},
		NextCursor: "next-page-cursor",
	}
	page2 := listRunsResult{
		Items:      []Run{sampleRun(id2, "x/y", 2*time.Hour)},
		NextCursor: "",
	}
	fb.listByQuery[`limit=50`] = page1
	fb.listByQuery[`cursor=next-page-cursor&limit=50`] = page2

	_, first, err := r.listRuns(context.Background(), nil, ListRunsInput{})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if first.NextCursor != "next-page-cursor" {
		t.Fatalf("page 1 cursor = %q, want next-page-cursor", first.NextCursor)
	}
	_, second, err := r.listRuns(context.Background(), nil, ListRunsInput{Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if second.NextCursor != "" {
		t.Errorf("page 2 NextCursor = %q, want empty (last page)", second.NextCursor)
	}
	if len(second.Items) != 1 || second.Items[0].ID != id2.String() {
		t.Errorf("page 2 items = %+v, want a single run with id %s", second.Items, id2.String())
	}
}

// ── runtime_calibration tool ──────────────────────────────────────────────────

func TestRuntimeCalibration_HappyPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	fb.calibrationResp = CalibrationResult{
		Samples:             10,
		PredictedP50Minutes: 15.0,
		ActualP50Minutes:    12.0,
		ActualP95Minutes:    20.0,
		CalibrationRatio:    0.8,
		ConfidenceBandAccuracy: map[string]any{
			"medium": map[string]any{"samples": float64(10), "within_1.5x": float64(8)},
		},
	}

	_, out, err := r.runtimeCalibration(context.Background(), nil, RuntimeCalibrationInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Samples != 10 {
		t.Errorf("Samples = %d, want 10", out.Samples)
	}
	if out.CalibrationRatio != 0.8 {
		t.Errorf("CalibrationRatio = %f, want 0.8", out.CalibrationRatio)
	}
	if out.ActualP95Minutes != 20.0 {
		t.Errorf("ActualP95Minutes = %f, want 20.0", out.ActualP95Minutes)
	}
}

func TestRuntimeCalibration_FiltersForwarded(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.runtimeCalibration(context.Background(), nil, RuntimeCalibrationInput{
		WorkflowID: "my-workflow",
		StageType:  "implement",
		Since:      "2026-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	q := fb.lastCalibrationQuery
	if !strings.Contains(q, "workflow_id=my-workflow") {
		t.Errorf("query %q missing workflow_id", q)
	}
	if !strings.Contains(q, "stage_type=implement") {
		t.Errorf("query %q missing stage_type", q)
	}
	if !strings.Contains(q, "since=") {
		t.Errorf("query %q missing since", q)
	}
}

func TestRuntimeCalibration_BackendError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	fb.calibrationStatus = http.StatusInternalServerError

	_, _, err := r.runtimeCalibration(context.Background(), nil, RuntimeCalibrationInput{})
	if err == nil {
		t.Fatal("expected error on 500, got nil")
	}
}

// --- budget status (#693 / ADR-030) ---
//
// Cross-boundary wire-to-tool seam: a stub backend serves
// GET /v0/runs/{id}/budget and the three consuming tools surface the
// same block (and omit it when the backend returns the empty no-budget
// object). Per-layer unit tests alone would pass while the field
// silently dropped at the seam (cf. #618), so these drive the full
// apiClient.GetRunBudget -> tool-output path.

func seedBudget(fb *fakeBackend, runID uuid.UUID, bs BudgetStatus) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.budgetByRun[runID] = bs
}

func warnFloat(f float64) *float64 { return &f }

func TestStartRun_SurfacesBudgetBlock(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.createRunResp = Run{ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: "pending"}
	seedBudget(fb, runID, BudgetStatus{
		Period: "weekly", PeriodStart: "2026-06-01T00:00:00Z",
		LimitUSD: 50, SpentUSD: 165.86, Fraction: 3.3172,
		WarnAt: warnFloat(0.8), Tier: "over", Enforcement: "advisory",
	})

	_, out, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "deadbeef",
		WorkflowSpec: validTrivialSpec,
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if out.Budget == nil {
		t.Fatal("expected budget block surfaced from backend; got nil")
	}
	if out.Budget.Tier != "over" || out.Budget.Enforcement != "advisory" {
		t.Errorf("budget = %+v, want tier=over enforcement=advisory", out.Budget)
	}
	if out.Budget.SpentUSD != 165.86 || out.Budget.LimitUSD != 50 {
		t.Errorf("budget spend/limit = %g/%g, want 165.86/50", out.Budget.SpentUSD, out.Budget.LimitUSD)
	}
}

func TestStartRun_OmitsBudgetWhenNoBudget(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.createRunResp = Run{ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: "pending"}
	// No seedBudget → backend returns {} → GetRunBudget yields nil.

	_, out, err := r.startRun(context.Background(), nil, StartRunInput{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "deadbeef",
		WorkflowSpec: validTrivialSpec,
	})
	if err != nil {
		t.Fatalf("startRun: %v", err)
	}
	if out.Budget != nil {
		t.Errorf("expected no budget block; got %+v", out.Budget)
	}
	// The no-budget block must omit the JSON key entirely (nil pointer
	// + omitempty), not serialize a null.
	raw, _ := json.Marshal(out)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	if _, ok := m["budget"]; ok {
		t.Errorf("marshaled output must omit the budget key when no budget; got %s", raw)
	}
}

func TestGetRunStatus_SurfacesBudgetBlock(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: "running"}
	seedBudget(fb, runID, BudgetStatus{
		Period: "weekly", LimitUSD: 100, SpentUSD: 60, Fraction: 0.6,
		WarnAt: warnFloat(0.5), Tier: "warn", Enforcement: "advisory",
	})

	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.Budget == nil {
		t.Fatal("expected budget block surfaced; got nil")
	}
	if out.Budget.Tier != "warn" {
		t.Errorf("budget tier = %q, want warn", out.Budget.Tier)
	}
}

func TestGetRunStatus_OmitsBudgetWhenNoBudget(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change", State: "running"}

	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.Budget != nil {
		t.Errorf("expected no budget block; got %+v", out.Budget)
	}
}

// TestGetRunStatus_ConcernsBlock_PropagatesEndToEnd (#964, cf. #618):
// the backend run row's concerns block — open count, by_state, and the
// stable concern IDs fixup's concern_ids addressing needs — must cross
// the real HTTP + JSON-decode path into the tool output, not just exist
// as a struct field.
func TestGetRunStatus_ConcernsBlock_PropagatesEndToEnd(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	concernID := uuid.NewString()
	fb.getRunByID[runID] = Run{
		ID: runID.String(), Repo: "x/y", WorkflowID: "feature_change",
		State: "running",
		Concerns: &RunConcerns{
			Open:    2,
			ByState: map[string]int{"raised": 1, "addressed_pending": 1},
			Items: []RunConcernItem{
				{ID: concernID, StageKind: "implement", Severity: "medium", Category: "scope", State: "raised"},
				{ID: uuid.NewString(), StageKind: "plan", Severity: "low", Category: "verification", State: "addressed_pending"},
			},
		},
	}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	got := out.Run.Concerns
	if got == nil {
		t.Fatal("Run.Concerns = nil, want the decoded block")
	}
	if got.Open != 2 {
		t.Errorf("Open = %d, want 2", got.Open)
	}
	if got.ByState["raised"] != 1 || got.ByState["addressed_pending"] != 1 {
		t.Errorf("ByState = %v", got.ByState)
	}
	if len(got.Items) != 2 || got.Items[0].ID != concernID || got.Items[0].StageKind != "implement" {
		t.Errorf("Items = %+v, want the stable IDs decoded through", got.Items)
	}
}

// TestGetRunStatus_NoConcernsBlock_NilField: a run with no open concerns
// (backend omits the key) decodes to a nil pointer, never a zero block.
func TestGetRunStatus_NoConcernsBlock_NilField(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	fb.getRunByID[runID] = Run{ID: runID.String(), Repo: "x/y", State: "running"}

	r := newResolver(srv, nil)
	_, out, err := r.getRunStatus(context.Background(), nil, GetRunStatusInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getRunStatus: %v", err)
	}
	if out.Run.Concerns != nil {
		t.Errorf("Run.Concerns = %+v, want nil when the backend omits the block", out.Run.Concerns)
	}
}

// TestGetPlan_ScopePrecheck_MaxFilesChangedCrossesSeam asserts the #983
// cap field rides the backend-write -> mcp-read JSON contract: a
// server-side payload with MaxFilesChanged set surfaces on the tool
// output so the approver can read headroom.
func TestGetPlan_ScopePrecheck_MaxFilesChangedCrossesSeam(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	seedScopePrecheckAudit(fb, runID, server.ScopePrecheckPayload{
		WorkflowID:       "feature_change",
		ImplementStageID: "implement",
		ScannedFiles:     29,
		Violations:       []policy.Violation{},
		MaxFilesChanged:  30,
	})

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.ScopePrecheck == nil {
		t.Fatal("ScopePrecheck is nil; want populated")
	}
	if out.ScopePrecheck.MaxFilesChanged != 30 {
		t.Errorf("MaxFilesChanged = %d, want 30", out.ScopePrecheck.MaxFilesChanged)
	}
	if out.ScopePrecheck.ScannedFiles != 29 {
		t.Errorf("ScannedFiles = %d, want 29", out.ScopePrecheck.ScannedFiles)
	}
}

// TestGetPlan_ScopePrecheck_OlderBackendWithoutCapDecodes asserts
// forward/backward compat: a pre-#983 payload lacking the
// max_files_changed key decodes cleanly with the field at zero.
func TestGetPlan_ScopePrecheck_OlderBackendWithoutCapDecodes(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	planStageID := uuid.New()
	fb.stagesByRun[runID] = []Stage{
		{ID: planStageID.String(), RunID: runID.String(), Type: "plan", State: "succeeded"},
	}
	seedPlanArtifact(fb, planStageID, samplePlanContent(), time.Hour)

	// Raw map rather than the server type: older backends never wrote
	// the max_files_changed key at all.
	fb.mu.Lock()
	fb.perRunAuditByRun[runID] = append(fb.perRunAuditByRun[runID], AuditEntry{
		ID:       uuid.New().String(),
		Sequence: 1,
		RunID:    runID.String(),
		Category: "plan_scope_precheck",
		Payload: map[string]any{
			"workflow_id":        "feature_change",
			"implement_stage_id": "implement",
			"violations":         []any{},
			"scanned_files":      2,
		},
	})
	fb.mu.Unlock()

	r := newResolver(srv, nil)
	_, out, err := r.getPlan(context.Background(), nil, GetPlanInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("getPlan: %v", err)
	}
	if out.ScopePrecheck == nil {
		t.Fatal("ScopePrecheck is nil; want populated")
	}
	if out.ScopePrecheck.MaxFilesChanged != 0 {
		t.Errorf("MaxFilesChanged = %d, want 0 for an older-backend payload", out.ScopePrecheck.MaxFilesChanged)
	}
}
