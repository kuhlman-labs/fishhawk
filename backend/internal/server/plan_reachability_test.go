package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// reachabilityHeaderJSON is a populated reachability sweep payload in the exact
// runner wire shape (reachability.Result json tags), used by the decode + audit
// tests. It carries one phase with disagreeing declared/derived counts and one
// construction-site violation.
func reachabilityHeaderJSON() string {
	return `{"available":true,` +
		`"phases":[{"index":0,"title":"expand","declared_count":2,"derived_count":3}],` +
		`"violations":[{"kind":"construction_site","symbol":"Widget",` +
		`"def_file":"pkg/a/widget.go","def_phase":0,"use_file":"pkg/b/use.go","use_phase":1}]}`
}

func newReachabilityServer(au *auditFake) *Server {
	return New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
}

// TestRunPlanReachability_DecodesAndRecords covers the happy path: a well-formed
// header decodes into the mirroring struct AND records one advisory
// plan_reachability_sweep audit entry whose payload round-trips every field —
// the server leg of the exact-wire-key transport contract.
func TestRunPlanReachability_DecodesAndRecords(t *testing.T) {
	au := newAuditFake()
	s := newReachabilityServer(au)
	runID, stageID := uuid.New(), uuid.New()

	got := s.runPlanReachability(context.Background(), runID, stageID, reachabilityHeaderJSON())
	if got == nil {
		t.Fatal("expected a decoded payload")
	}
	if !got.Available {
		t.Error("Available should be true")
	}
	if len(got.Phases) != 1 || got.Phases[0].DeclaredCount != 2 || got.Phases[0].DerivedCount != 3 {
		t.Errorf("phases = %+v", got.Phases)
	}
	if len(got.Violations) != 1 || got.Violations[0].Kind != "construction_site" ||
		got.Violations[0].Symbol != "Widget" || got.Violations[0].UsePhase != 1 {
		t.Errorf("violations = %+v", got.Violations)
	}

	if len(au.appended) != 1 {
		t.Fatalf("appended = %d, want 1", len(au.appended))
	}
	if au.appended[0].Category != reachabilitySweepAuditKind {
		t.Errorf("category = %q, want %q", au.appended[0].Category, reachabilitySweepAuditKind)
	}
	var decoded PlanReachabilityPayload
	if err := json.Unmarshal(au.appended[0].Payload, &decoded); err != nil {
		t.Fatalf("decode recorded payload: %v", err)
	}
	if len(decoded.Phases) != 1 || decoded.Phases[0].DerivedCount != 3 {
		t.Errorf("recorded phases = %+v", decoded.Phases)
	}
	if len(decoded.Violations) != 1 || decoded.Violations[0].DefFile != "pkg/a/widget.go" {
		t.Errorf("recorded violations = %+v", decoded.Violations)
	}
}

// TestRunPlanReachability_NilAuditRepo covers the nil-AuditRepo fail-open branch.
func TestRunPlanReachability_NilAuditRepo(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	if got := s.runPlanReachability(context.Background(), uuid.New(), uuid.New(), reachabilityHeaderJSON()); got != nil {
		t.Errorf("expected nil with no AuditRepo, got %+v", got)
	}
}

// TestRunPlanReachability_EmptyHeader_NoEntry covers the empty-header branch (a
// plan with no split_proposal, or an older runner): no decode, no entry.
func TestRunPlanReachability_EmptyHeader_NoEntry(t *testing.T) {
	au := newAuditFake()
	s := newReachabilityServer(au)
	if got := s.runPlanReachability(context.Background(), uuid.New(), uuid.New(), ""); got != nil {
		t.Errorf("expected nil for empty header, got %+v", got)
	}
	if len(au.appended) != 0 {
		t.Errorf("appended = %d, want 0", len(au.appended))
	}
}

// TestRunPlanReachability_MalformedHeader_FailOpen covers the malformed-JSON
// branch: log and skip, no entry, no error.
func TestRunPlanReachability_MalformedHeader_FailOpen(t *testing.T) {
	au := newAuditFake()
	s := newReachabilityServer(au)
	if got := s.runPlanReachability(context.Background(), uuid.New(), uuid.New(), "not json"); got != nil {
		t.Errorf("expected nil for malformed header, got %+v", got)
	}
	if len(au.appended) != 0 {
		t.Errorf("appended = %d, want 0", len(au.appended))
	}
}

// TestRunPlanReachability_AppendError_ReturnsPayload covers the audit-append
// failure branch: WARN-log and continue, still returning the decoded payload
// (fail-open — the advisory never fails the upload).
func TestRunPlanReachability_AppendError_ReturnsPayload(t *testing.T) {
	au := newAuditFake()
	au.appendErr = errors.New("audit store down")
	s := newReachabilityServer(au)
	got := s.runPlanReachability(context.Background(), uuid.New(), uuid.New(), reachabilityHeaderJSON())
	if got == nil {
		t.Fatal("payload should still be returned on an append failure (fail-open)")
	}
	if len(au.appended) != 0 {
		t.Errorf("appended = %d, want 0 (append failed)", len(au.appended))
	}
}
