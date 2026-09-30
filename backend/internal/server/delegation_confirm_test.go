package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationconfirm"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Delegation confirmation on handover (E76.5 / #3768).

const confirmStages = `    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
`

// confirmSpecA is the served spec: `ship` is high with a medium ceiling on
// docs/**, `guarded` is low.
const confirmSpecA = `version: "2"
workflows:
  ship:
    autonomy: high
    escalations:
      - match:
          paths:
            - "docs/**"
        require:
          max_autonomy: medium
` + confirmStages + `  guarded:
    autonomy: low
` + confirmStages

// confirmSpecTightened is an INDEPENDENTLY authored spec whose `ship`
// escalation ceiling is tightened to low — a genuinely different resolved
// ceiling matrix, so its hash differs from confirmSpecA's by construction.
const confirmSpecTightened = `version: "2"
workflows:
  ship:
    autonomy: high
    escalations:
      - match:
          paths:
            - "docs/**"
        require:
          max_autonomy: low
` + confirmStages + `  guarded:
    autonomy: low
` + confirmStages

// hashOfWorkflow projects specYAML and returns id's content hash.
func hashOfWorkflow(t *testing.T, specYAML, id string) string {
	t.Helper()
	parsed, err := spec.ParseBytes([]byte(specYAML))
	if err != nil {
		t.Fatalf("parse fixture spec: %v", err)
	}
	for _, wf := range delegationview.Project(parsed) {
		if wf.ID == id {
			return wf.ContentHash
		}
	}
	t.Fatalf("fixture declares no workflow %q", id)
	return ""
}

// memConfirmStore is an in-memory DelegationConfirmStore running the SAME
// pure fold and transitions as delegationconfirm.Store, serialized by a
// mutex.
type memConfirmStore struct {
	mu      sync.Mutex
	seq     int64
	captain []delegationconfirm.ChainEntry
	confirm []delegationconfirm.ChainEntry
	readErr error
}

func (m *memConfirmStore) seat(t *testing.T, repo, subject string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	raw, _ := json.Marshal(map[string]any{"repo": repo, "subject": subject})
	m.captain = append(m.captain, delegationconfirm.ChainEntry{Sequence: m.seq, EntryHash: "cap", Category: captain.CategoryAssigned, Payload: raw})
}

func (m *memConfirmStore) count(kind string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, e := range m.confirm {
		if e.Category == kind {
			n++
		}
	}
	return n
}

func (m *memConfirmStore) Read(_ context.Context, _ *uuid.UUID, repo string) (*delegationconfirm.Snapshot, error) {
	if m.readErr != nil {
		return nil, m.readErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return &delegationconfirm.Snapshot{State: delegationconfirm.Derive(repo, m.captain, m.confirm)}, nil
}

func (m *memConfirmStore) Append(_ context.Context, p delegationconfirm.AppendParams, decide func(delegationconfirm.State) (delegationconfirm.Event, error)) (*delegationconfirm.Applied, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev, err := decide(delegationconfirm.Derive(p.Repo, m.captain, m.confirm))
	if err != nil {
		return nil, err
	}
	body, err := ev.Payload()
	if err != nil {
		return nil, err
	}
	m.seq++
	e := delegationconfirm.ChainEntry{Sequence: m.seq, EntryHash: "e", Category: ev.Kind, Timestamp: time.Now().UTC(), Payload: body}
	m.confirm = append(m.confirm, e)
	return &delegationconfirm.Applied{Event: ev, State: delegationconfirm.Derive(p.Repo, m.captain, m.confirm),
		Entry: &audit.Entry{Sequence: e.Sequence, EntryHash: e.EntryHash, Category: e.Category, Timestamp: e.Timestamp, Payload: e.Payload}}, nil
}

// confirmServer serves specYAML as acme/app's cached spec.
func confirmServer(specYAML string, store *memConfirmStore) *Server {
	cfg := Config{Addr: "127.0.0.1:0", RunRepo: newDelegationFixture(specYAML).runs}
	if store != nil {
		cfg.DelegationConfirmStore = store
	}
	return New(cfg)
}

// serveDelegationConfirm drives one of the three handlers with a path-valued
// request carrying subject's identity.
func serveDelegationConfirm(t *testing.T, s *Server, verb, body, subject string) *httptest.ResponseRecorder {
	t.Helper()
	method := http.MethodPost
	target := "/v0/repos/acme/app/delegation/" + verb
	if verb == "confirmation" || verb == "" {
		method = http.MethodGet
		target += "?source=run_cache"
	}
	req := httptest.NewRequest(method, strings.TrimSuffix(target, "/"), strings.NewReader(body))
	req.SetPathValue("owner", "acme")
	req.SetPathValue("name", "app")
	if subject != "" {
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{Subject: subject}))
	}
	w := httptest.NewRecorder()
	switch verb {
	case "confirmation":
		s.handleGetDelegationConfirmation(w, req)
	case "confirm":
		s.handleDelegationConfirm(w, req)
	case "lower":
		s.handleDelegationLower(w, req)
	case "":
		s.handleGetRepoDelegation(w, req)
	default:
		t.Fatalf("unknown verb %q", verb)
	}
	return w
}

func confirmBody(workflow, hash string) string {
	b, _ := json.Marshal(map[string]any{"workflow": workflow, "content_hash": hash, "source": "run_cache"})
	return string(b)
}

func lowerBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	fields["source"] = "run_cache"
	if _, ok := fields["title_vars"]; !ok {
		fields["title_vars"] = map[string]string{"epic": "76", "n": "9"}
	}
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func wantCode(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body %s", w.Code, status, w.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v; body %s", err, w.Body.String())
	}
	if env.Error.Code != code {
		t.Fatalf("code = %q, want %q; body %s", env.Error.Code, code, w.Body.String())
	}
}

func readConfirmation(t *testing.T, s *Server) delegationConfirmationResponse {
	t.Helper()
	w := serveDelegationConfirm(t, s, "confirmation", "", "github:reader")
	if w.Code != http.StatusOK {
		t.Fatalf("GET confirmation = %d: %s", w.Code, w.Body.String())
	}
	var out delegationConfirmationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func statusFor(t *testing.T, ws []delegationconfirm.WorkflowStatus, id string) delegationconfirm.WorkflowStatus {
	t.Helper()
	for _, s := range ws {
		if s.Workflow == id {
			return s
		}
	}
	t.Fatalf("workflow %q absent from %+v", id, ws)
	return delegationconfirm.WorkflowStatus{}
}

const confirmCaptain = "github:captain"

// TestDelegationConfirmation_FirstHandoverListsEveryWorkflow (approval
// condition 1): with a captain seated and no confirmation entries, every
// workflow of the VIEW is listed unconfirmed.
func TestDelegationConfirmation_FirstHandoverListsEveryWorkflow(t *testing.T) {
	store := &memConfirmStore{}
	store.seat(t, "acme/app", confirmCaptain)
	got := readConfirmation(t, confirmServer(confirmSpecA, store))
	if strings.Join(got.UnconfirmedWorkflows, ",") != "guarded,ship" {
		t.Fatalf("unconfirmed = %v, want every workflow of the spec [guarded ship]", got.UnconfirmedWorkflows)
	}
	if got.Captain == nil || *got.Captain != confirmCaptain || got.Source != "run_cache" {
		t.Errorf("captain/source = %v/%q", got.Captain, got.Source)
	}
}

// TestConfirm_ConfirmsAndSurfacesOnBothReads: a confirm of the current hash
// is recorded and shows on the confirmation read AND the delegation read;
// a later handover flips both back to unconfirmed.
func TestConfirm_ConfirmsAndSurfacesOnBothReads(t *testing.T) {
	store := &memConfirmStore{}
	store.seat(t, "acme/app", confirmCaptain)
	s := confirmServer(confirmSpecA, store)
	hash := hashOfWorkflow(t, confirmSpecA, "ship")

	w := serveDelegationConfirm(t, s, "confirm", confirmBody("ship", hash), confirmCaptain)
	if w.Code != http.StatusOK {
		t.Fatalf("confirm = %d: %s", w.Code, w.Body.String())
	}
	var resp delegationVerbResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Workflow.Status != delegationconfirm.StatusConfirmed || resp.Event.Category != delegationconfirm.CategoryDelegationConfirmed {
		t.Fatalf("response = %+v", resp)
	}
	if st := statusFor(t, readConfirmation(t, s).Workflows, "ship"); st.Status != delegationconfirm.StatusConfirmed || st.Confirmation.ContentHash != hash {
		t.Fatalf("confirmation read = %+v, want confirmed with %s", st, hash)
	}
	dr := readDelegationBody(t, s)
	if dr.Confirmation == nil || strings.Join(dr.Confirmation.UnconfirmedWorkflows, ",") != "guarded" {
		t.Fatalf("delegation read summary = %+v, want [guarded]", dr.Confirmation)
	}
	if c := delegationRespWorkflow(t, dr, "ship").Confirmation; c == nil || c.Status != delegationconfirm.StatusConfirmed {
		t.Fatalf("delegation read ship block = %+v, want confirmed", c)
	}

	store.seat(t, "acme/app", "github:successor")
	if st := statusFor(t, readConfirmation(t, s).Workflows, "ship"); st.Status != delegationconfirm.StatusUnconfirmed || st.Reason != delegationconfirm.ReasonHandover {
		t.Fatalf("after handover confirmation read = %+v, want unconfirmed/handover", st)
	}
	if c := delegationRespWorkflow(t, readDelegationBody(t, s), "ship").Confirmation; c.Reason != delegationconfirm.ReasonHandover {
		t.Fatalf("after handover delegation read = %+v, want handover", c)
	}
}

func readDelegationBody(t *testing.T, s *Server) delegationResponse {
	t.Helper()
	w := serveDelegationConfirm(t, s, "", "", "github:reader")
	if w.Code != http.StatusOK {
		t.Fatalf("GET delegation = %d: %s", w.Code, w.Body.String())
	}
	var out delegationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func delegationRespWorkflow(t *testing.T, r delegationResponse, id string) delegationWorkflowResponse {
	t.Helper()
	for _, wf := range r.Workflows {
		if wf.ID == id {
			return wf
		}
	}
	t.Fatalf("workflow %q absent", id)
	return delegationWorkflowResponse{}
}

// TestConfirm_StaleHashRefused (C1): the confirmed hash is taken from a
// SECOND independently authored spec, so it differs by construction. The
// control's effect is COMMITTED STATE: zero delegation_confirmed entries.
func TestConfirm_StaleHashRefused(t *testing.T) {
	store := &memConfirmStore{}
	store.seat(t, "acme/app", confirmCaptain)
	s := confirmServer(confirmSpecA, store)
	stale := hashOfWorkflow(t, confirmSpecTightened, "ship")
	wantCode(t, serveDelegationConfirm(t, s, "confirm", confirmBody("ship", stale), confirmCaptain),
		http.StatusConflict, "delegation_hash_stale")
	if n := store.count(delegationconfirm.CategoryDelegationConfirmed); n != 0 {
		t.Fatalf("delegation_confirmed entries = %d, want 0", n)
	}
	if st := statusFor(t, readConfirmation(t, s).Workflows, "ship"); st.Status != delegationconfirm.StatusUnconfirmed {
		t.Errorf("read-back = %+v, want unconfirmed", st)
	}
}

// TestConfirm_HashStaleAfterSpecTightened: a confirmation recorded against
// spec A reads hash_stale once the served spec is the tightened one — on the
// two delegation-confirmation reads (approval condition 2).
func TestConfirm_HashStaleAfterSpecTightened(t *testing.T) {
	store := &memConfirmStore{}
	store.seat(t, "acme/app", confirmCaptain)
	a := confirmServer(confirmSpecA, store)
	if w := serveDelegationConfirm(t, a, "confirm", confirmBody("ship", hashOfWorkflow(t, confirmSpecA, "ship")), confirmCaptain); w.Code != http.StatusOK {
		t.Fatalf("confirm = %d: %s", w.Code, w.Body.String())
	}
	b := confirmServer(confirmSpecTightened, store)
	if st := statusFor(t, readConfirmation(t, b).Workflows, "ship"); st.Reason != delegationconfirm.ReasonHashStale {
		t.Fatalf("confirmation read = %+v, want hash_stale", st)
	}
	if c := delegationRespWorkflow(t, readDelegationBody(t, b), "ship").Confirmation; c.Reason != delegationconfirm.ReasonHashStale {
		t.Fatalf("delegation read = %+v, want hash_stale", c)
	}
}

// TestConfirm_RunTokenRefused (C3): the caller is an mcp:run:<uuid> subject
// that IS the seated captain, so the captain check would ADMIT it — only the
// token-class guard stands between it and an append.
func TestConfirm_RunTokenRefused(t *testing.T) {
	runSubject := "mcp:run:" + uuid.NewString()
	store := &memConfirmStore{}
	store.seat(t, "acme/app", runSubject)
	s := confirmServer(confirmSpecA, store)
	wantCode(t, serveDelegationConfirm(t, s, "confirm", confirmBody("ship", hashOfWorkflow(t, confirmSpecA, "ship")), runSubject),
		http.StatusForbidden, "delegation_agent_identity_refused")
	if n := store.count(delegationconfirm.CategoryDelegationConfirmed); n != 0 {
		t.Fatalf("entries = %d, want 0", n)
	}
}

// TestConfirm_AgentSubjectRefused: the operator-agent token family, asserted
// separately from the run-token arm; and the ADR-040 delegated opt-in.
func TestConfirm_AgentSubjectRefused(t *testing.T) {
	agent := "operator-agent/v1"
	store := &memConfirmStore{}
	store.seat(t, "acme/app", agent)
	s := confirmServer(confirmSpecA, store)
	wantCode(t, serveDelegationConfirm(t, s, "confirm", confirmBody("ship", hashOfWorkflow(t, confirmSpecA, "ship")), agent),
		http.StatusForbidden, "delegation_agent_identity_refused")

	store.seat(t, "acme/app", confirmCaptain)
	body := `{"workflow":"ship","content_hash":"` + hashOfWorkflow(t, confirmSpecA, "ship") + `","source":"run_cache","delegated":true}`
	wantCode(t, serveDelegationConfirm(t, s, "confirm", body, confirmCaptain), http.StatusForbidden, "delegation_agent_identity_refused")
	wantCode(t, serveDelegationConfirm(t, s, "lower", lowerBody(t, map[string]any{"workflow": "ship", "proposed_tier": "low", "reason": "r"}), agent),
		http.StatusForbidden, "delegation_agent_identity_refused")
	if n := store.count(delegationconfirm.CategoryDelegationConfirmed) + store.count(delegationconfirm.CategoryDelegationLowerProposed); n != 0 {
		t.Fatalf("entries = %d, want 0", n)
	}
}

// TestConfirm_NonCaptainRefused (C4): the seat is held by a freshly
// generated operator subject, so the caller is definitionally not captain.
func TestConfirm_NonCaptainRefused(t *testing.T) {
	store := &memConfirmStore{}
	store.seat(t, "acme/app", "github:"+uuid.NewString())
	s := confirmServer(confirmSpecA, store)
	wantCode(t, serveDelegationConfirm(t, s, "confirm", confirmBody("ship", hashOfWorkflow(t, confirmSpecA, "ship")), confirmCaptain),
		http.StatusForbidden, "delegation_not_captain")
	if n := store.count(delegationconfirm.CategoryDelegationConfirmed); n != 0 {
		t.Fatalf("entries = %d after the call returned, want 0", n)
	}
}

func TestConfirm_NoCaptainRefused(t *testing.T) {
	store := &memConfirmStore{}
	s := confirmServer(confirmSpecA, store)
	wantCode(t, serveDelegationConfirm(t, s, "confirm", confirmBody("ship", hashOfWorkflow(t, confirmSpecA, "ship")), confirmCaptain),
		http.StatusConflict, "delegation_no_captain")
}

func TestConfirm_UnknownWorkflowIs404(t *testing.T) {
	store := &memConfirmStore{}
	store.seat(t, "acme/app", confirmCaptain)
	s := confirmServer(confirmSpecA, store)
	wantCode(t, serveDelegationConfirm(t, s, "confirm", confirmBody("nope", "h"), confirmCaptain), http.StatusNotFound, "workflow_not_found")
	wantCode(t, serveDelegationConfirm(t, s, "lower", lowerBody(t, map[string]any{"workflow": "nope", "proposed_tier": "low", "reason": "r"}), confirmCaptain),
		http.StatusNotFound, "workflow_not_found")
}

func TestConfirm_RequestShapeRefusals(t *testing.T) {
	store := &memConfirmStore{}
	store.seat(t, "acme/app", confirmCaptain)
	s := confirmServer(confirmSpecA, store)
	wantCode(t, serveDelegationConfirm(t, s, "confirm", `{"workflow":"ship"}`, confirmCaptain), http.StatusBadRequest, "validation_failed")
	wantCode(t, serveDelegationConfirm(t, s, "confirm", `{"workflow":"ship","content_hash":"h","bogus":1}`, confirmCaptain), http.StatusBadRequest, "validation_failed")
	wantCode(t, serveDelegationConfirm(t, s, "confirm", `{"workflow":"ship","content_hash":"h","source":"nope"}`, confirmCaptain), http.StatusBadRequest, "validation_failed")
	wantCode(t, serveDelegationConfirm(t, s, "lower", `{"reason":"r"}`, confirmCaptain), http.StatusBadRequest, "validation_failed")
	wantCode(t, serveDelegationConfirm(t, s, "confirm", confirmBody("ship", "h"), ""), http.StatusUnauthorized, "authentication_required")
}

// TestDelegationConfirm_UnconfiguredIs501: a nil store degrades all three
// routes, and the delegation read carries NO confirmation block.
func TestDelegationConfirm_UnconfiguredIs501(t *testing.T) {
	s := confirmServer(confirmSpecA, nil)
	wantCode(t, serveDelegationConfirm(t, s, "confirmation", "", "github:reader"), http.StatusNotImplemented, "delegation_confirm_unconfigured")
	wantCode(t, serveDelegationConfirm(t, s, "confirm", confirmBody("ship", "h"), confirmCaptain), http.StatusNotImplemented, "delegation_confirm_unconfigured")
	wantCode(t, serveDelegationConfirm(t, s, "lower", lowerBody(t, map[string]any{"workflow": "ship", "proposed_tier": "low", "reason": "r"}), confirmCaptain),
		http.StatusNotImplemented, "delegation_confirm_unconfigured")
	w := serveDelegationConfirm(t, s, "", "", "github:reader")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"confirmation"`) {
		t.Fatalf("nil-store delegation read = %d, must carry no confirmation block: %s", w.Code, w.Body.String())
	}
}

// TestDelegationRead_StoreReadFailureReportsUnavailable: a failed chain read
// reports confirmation.unavailable and NO per-workflow verdict.
func TestDelegationRead_StoreReadFailureReportsUnavailable(t *testing.T) {
	s := confirmServer(confirmSpecA, &memConfirmStore{readErr: errors.New("db down")})
	dr := readDelegationBody(t, s)
	if dr.Confirmation == nil || dr.Confirmation.Unavailable == "" {
		t.Fatalf("summary = %+v, want unavailable", dr.Confirmation)
	}
	for _, wf := range dr.Workflows {
		if wf.Confirmation != nil {
			t.Errorf("%s carries a verdict despite the failed read", wf.ID)
		}
	}
	wantCode(t, serveDelegationConfirm(t, s, "confirmation", "", "github:reader"), http.StatusInternalServerError, "internal_error")
}

// TestDelegationRead_ConfirmationAfterFilter: the view-level list covers the
// ?workflow-retained set only.
func TestDelegationRead_ConfirmationAfterFilter(t *testing.T) {
	store := &memConfirmStore{}
	store.seat(t, "acme/app", confirmCaptain)
	s := confirmServer(confirmSpecA, store)
	req := httptest.NewRequest(http.MethodGet, "/v0/repos/acme/app/delegation?source=run_cache&workflow=ship", nil)
	req.SetPathValue("owner", "acme")
	req.SetPathValue("name", "app")
	w := httptest.NewRecorder()
	s.handleGetRepoDelegation(w, req)
	var dr delegationResponse
	_ = json.Unmarshal(w.Body.Bytes(), &dr)
	if dr.Confirmation == nil || strings.Join(dr.Confirmation.UnconfirmedWorkflows, ",") != "ship" {
		t.Fatalf("filtered summary = %+v, want [ship]", dr.Confirmation)
	}
}

// TestLower_RaiseRefusedFilesNothing (C2): guarded is low; proposing high
// must reach neither the provider nor the chain.
func TestLower_RaiseRefusedFilesNothing(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)
	store := &memConfirmStore{}
	store.seat(t, "acme/app", confirmCaptain)
	s := confirmServer(confirmSpecA, store)
	wantCode(t, serveDelegationConfirm(t, s, "lower", lowerBody(t, map[string]any{"workflow": "guarded", "proposed_tier": "high", "reason": "r"}), confirmCaptain),
		http.StatusBadRequest, "delegation_raise_refused")
	if fp.called || store.count(delegationconfirm.CategoryDelegationLowerProposed) != 0 {
		t.Fatalf("a raise filed an issue (%v) or appended an entry", fp.called)
	}
}

// TestLower_EscalationCeilingAboveProposedTierRefused (C7): lower ship to low
// while declaring an escalation ceiling of high.
func TestLower_EscalationCeilingAboveProposedTierRefused(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)
	store := &memConfirmStore{}
	store.seat(t, "acme/app", confirmCaptain)
	s := confirmServer(confirmSpecA, store)
	body := lowerBody(t, map[string]any{"workflow": "ship", "proposed_tier": "low", "reason": "r",
		"proposed_escalation": map[string]any{"paths": []string{"x/**"}, "max_autonomy": "high"}})
	wantCode(t, serveDelegationConfirm(t, s, "lower", body, confirmCaptain), http.StatusBadRequest, "delegation_raise_refused")
	if fp.called || store.count(delegationconfirm.CategoryDelegationLowerProposed) != 0 {
		t.Fatal("an escalation above the proposed tier filed an issue or appended an entry")
	}
}

func TestLower_NothingProposedAndEmptyReason(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)
	store := &memConfirmStore{}
	store.seat(t, "acme/app", confirmCaptain)
	s := confirmServer(confirmSpecA, store)
	wantCode(t, serveDelegationConfirm(t, s, "lower", lowerBody(t, map[string]any{"workflow": "ship", "reason": "r"}), confirmCaptain),
		http.StatusBadRequest, "delegation_nothing_proposed")
	wantCode(t, serveDelegationConfirm(t, s, "lower", lowerBody(t, map[string]any{"workflow": "ship", "proposed_tier": "low", "reason": "  "}), confirmCaptain),
		http.StatusBadRequest, "validation_failed")
	wantCode(t, serveDelegationConfirm(t, s, "lower", lowerBody(t, map[string]any{"workflow": "ship", "reason": "r",
		"proposed_escalation": map[string]any{"paths": []string{}, "max_autonomy": "low"}}), confirmCaptain),
		http.StatusBadRequest, "validation_failed")
	if fp.called {
		t.Fatal("a refused proposal filed an issue")
	}
}

// TestLower_NonCaptainFilesNothing: the captain PRE-check runs before the
// filing.
func TestLower_NonCaptainFilesNothing(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)
	store := &memConfirmStore{}
	store.seat(t, "acme/app", "github:"+uuid.NewString())
	s := confirmServer(confirmSpecA, store)
	wantCode(t, serveDelegationConfirm(t, s, "lower", lowerBody(t, map[string]any{"workflow": "ship", "proposed_tier": "low", "reason": "r"}), confirmCaptain),
		http.StatusForbidden, "delegation_not_captain")
	if fp.called {
		t.Fatal("a non-captain's proposal filed an issue")
	}
}

// TestLower_FilesAutonomyLowWithTheEdit is the done-means test: exactly one
// filing, labelled autonomy:low (any caller autonomy label replaced), whose
// body carries the proposed workflows.yaml edit; one entry recorded naming
// the filed item; the workflow stays UNCONFIRMED (a proposal is not a
// confirmation).
func TestLower_FilesAutonomyLowWithTheEdit(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)
	store := &memConfirmStore{}
	store.seat(t, "acme/app", confirmCaptain)
	s := confirmServer(confirmSpecA, store)
	body := lowerBody(t, map[string]any{"workflow": "ship", "proposed_tier": "medium", "reason": "new captain, new crew",
		"labels":              []string{"autonomy:high", "area:backend"},
		"proposed_escalation": map[string]any{"paths": []string{"backend/**"}, "max_autonomy": "low"}})
	w := serveDelegationConfirm(t, s, "lower", body, confirmCaptain)
	if w.Code != http.StatusOK {
		t.Fatalf("lower = %d: %s", w.Code, w.Body.String())
	}
	if !fp.called {
		t.Fatal("no work item was filed")
	}
	labels := strings.Join(fp.captured.Item.Classification.Labels, ",")
	if !strings.Contains(labels, delegationLowerLabel) || strings.Contains(labels, "autonomy:high") || strings.Contains(labels, "autonomy:medium") {
		t.Fatalf("labels = %s, want autonomy:low and no other autonomy label", labels)
	}
	for _, want := range []string{"workflows:", `"ship":`, `autonomy: "medium"`, `- "backend/**"`, `max_autonomy: "low"`, "new captain, new crew"} {
		if !strings.Contains(fp.captured.Item.Body, want) {
			t.Errorf("filed body missing %q:\n%s", want, fp.captured.Item.Body)
		}
	}
	if n := store.count(delegationconfirm.CategoryDelegationLowerProposed); n != 1 {
		t.Fatalf("lower entries = %d, want 1", n)
	}
	var resp delegationVerbResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Filed == nil || resp.Filed.URL == "" || resp.Event.Category != delegationconfirm.CategoryDelegationLowerProposed {
		t.Fatalf("response = %+v", resp)
	}
	if resp.Workflow.Status != delegationconfirm.StatusUnconfirmed || resp.Workflow.LowerProposal == nil || resp.Workflow.LowerProposal.FiledRef != resp.Filed.URL {
		t.Fatalf("workflow = %+v, want unconfirmed carrying the proposal", resp.Workflow)
	}
}

// TestLower_FilingFailureAppendsNothing.
func TestLower_FilingFailureAppendsNothing(t *testing.T) {
	fp := &fakeWorkProvider{fileErr: errors.New("tracker down")}
	registerFakeProvider(t, fp)
	store := &memConfirmStore{}
	store.seat(t, "acme/app", confirmCaptain)
	s := confirmServer(confirmSpecA, store)
	w := serveDelegationConfirm(t, s, "lower", lowerBody(t, map[string]any{"workflow": "ship", "proposed_tier": "low", "reason": "r"}), confirmCaptain)
	if w.Code < 400 {
		t.Fatalf("status = %d, want a filing failure", w.Code)
	}
	if n := store.count(delegationconfirm.CategoryDelegationLowerProposed); n != 0 {
		t.Fatalf("entries = %d, want 0 after a failed filing", n)
	}
}

// TestLower_TouchesNoRepositoryFile (C5): with source=ref the forge is a
// recorder that counts every non-GET request (the only way anything could
// write a repository file). The lower path dials it for reads only, while
// the work-item provider is dialled exactly once. The mis-wired arm proves
// the recorder is not vacuous.
func TestLower_TouchesNoRepositoryFile(t *testing.T) {
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)
	gh := newFakeGitHubForRuns(confirmSpecA)
	var mu sync.Mutex
	writes := 0
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mu.Lock()
			writes++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/installation") {
			_, _ = w.Write([]byte(gh.installationBody))
			return
		}
		_, _ = w.Write([]byte(gh.specBody))
	}))
	t.Cleanup(ghSrv.Close)
	store := &memConfirmStore{}
	store.seat(t, "acme/app", confirmCaptain)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: newDelegationFixture(confirmSpecA).runs,
		GitHub: newServerWithGitHub(t, newFakeRepo(), ghSrv).cfg.GitHub, DelegationConfirmStore: store})

	body := `{"workflow":"ship","proposed_tier":"low","reason":"r","title_vars":{"epic":"76","n":"9"}}`
	w := serveDelegationConfirm(t, s, "lower", body, confirmCaptain)
	if w.Code != http.StatusOK {
		t.Fatalf("lower = %d: %s", w.Code, w.Body.String())
	}
	if !fp.called {
		t.Fatal("the work-item provider was not dialled")
	}
	if writes != 0 {
		t.Fatalf("the lower path made %d non-GET forge request(s); it must touch no repository file", writes)
	}
	// Mis-wired arm: a content write against the same recorder fires it.
	req, _ := http.NewRequest(http.MethodPut, ghSrv.URL+"/repos/acme/app/contents/.fishhawk/workflows.yaml", strings.NewReader("{}"))
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
	if writes != 1 {
		t.Fatalf("the recorder did not observe a deliberate content write (writes=%d): the assertion above is vacuous", writes)
	}
}

// TestDelegationConfirmRefusals_EachDistinct: no two refusal modes share a
// code, and none collides with the handler-written codes.
func TestDelegationConfirmRefusals_EachDistinct(t *testing.T) {
	seen := map[string]bool{"workflow_not_found": true, "delegation_hash_stale": true, "delegation_confirm_unconfigured": true}
	for _, m := range delegationConfirmRefusals {
		if seen[m.code] {
			t.Errorf("code %q is shared by two refusal modes", m.code)
		}
		seen[m.code] = true
		if got, ok := delegationConfirmErrorStatus(m.err); !ok || got.code != m.code {
			t.Errorf("%v maps to %+v", m.err, got)
		}
	}
	if _, ok := delegationConfirmErrorStatus(errors.New("other")); ok {
		t.Error("an unrecognized error mapped to a refusal")
	}
}

// TestDelegationConfirm_RoutesRegistered drives the REAL mux so a dropped
// handlers.go line is observable.
func TestDelegationConfirm_RoutesRegistered(t *testing.T) {
	store := &memConfirmStore{}
	store.seat(t, "acme/app", confirmCaptain)
	s := confirmServer(confirmSpecA, store)
	if w := dashGET(t, s, "/v0/repos/acme/app/delegation/confirmation?source=run_cache"); w.Code != http.StatusOK {
		t.Fatalf("GET confirmation via mux = %d: %s", w.Code, w.Body.String())
	}
	for _, verb := range []string{"confirm", "lower"} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v0/repos/acme/app/delegation/"+verb, strings.NewReader("{}")))
		if w.Code == http.StatusNotFound || w.Code == http.StatusMethodNotAllowed {
			t.Errorf("POST %s via mux = %d: route not registered", verb, w.Code)
		}
	}
}
