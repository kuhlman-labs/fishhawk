package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Tests the wiring between the server's check-state read endpoint
// and the auditcomplete derivation (#229). Unit-level rule coverage
// lives in internal/auditcomplete; this file only asserts that the
// special-cased name flows through GET /v0/stages/{id}/checks
// correctly.
//
// The pre-#253 approval-gate behavior (a failing audit-complete
// check refused approval with a 409) is gone — ADR-017 (#249, #253)
// moved the gating to GitHub branch protection. The audit-complete
// state is still derived and published as a Check Run (#231); it's
// just not the API gate.

func TestListStageChecks_InjectsAuditCompleteWithMissing(t *testing.T) {
	rr := newOrchestratorRepo()
	r := rr.seedRun()
	plan := rr.seedStage(r.ID, 0, run.StageStateSucceeded)
	plan.Type = run.StageTypePlan
	impl := rr.seedStage(r.ID, 1, run.StageStateSucceeded)
	impl.Type = run.StageTypeImplement
	rev := rr.seedStage(r.ID, 2, run.StageStateAwaitingApproval)
	rev.Type = run.StageTypeReview
	rev.Gate = &run.Gate{
		Kind: run.GateKindApproval,
	}

	au := newAuditCompleteAuditFake()
	// Plan traces only — implement stage traces missing → trace_missing.
	au.appendTrace(t, r.ID, plan.ID, "raw")
	au.appendTrace(t, r.ID, plan.ID, "redacted")

	arts := newFakeArtifactRepo()
	seedPlanArtifact(arts, plan.ID)
	// No pull_request artifact → pr_missing too.

	scs := newFakeStageCheckRepo()

	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: rr,
		AuditRepo: au, ArtifactRepo: arts, StageCheckRepo: scs,
	})

	url := "/v0/stages/" + rev.ID.String() + "/checks"
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d want 200:\n%s", w.Code, w.Body.String())
	}
	var resp stageChecksListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, w.Body.String())
	}
	var found *stageCheckResponse
	for i := range resp.Items {
		if resp.Items[i].Name == AuditCompleteCheckName {
			found = &resp.Items[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("response did not include fishhawk_audit_complete row: %+v", resp)
	}
	if found.State != "fail" {
		t.Errorf("state = %q want fail", found.State)
	}
	if len(found.Missing) == 0 {
		t.Errorf("missing list should be populated; got empty")
	}
	hasTrace, hasPR := false, false
	for _, m := range found.Missing {
		if m.Kind == auditcomplete.MissingTrace {
			hasTrace = true
		}
		if m.Kind == auditcomplete.MissingPullRequest {
			hasPR = true
		}
	}
	if !hasTrace || !hasPR {
		t.Errorf("expected both trace_missing and pr_missing; got %+v", found.Missing)
	}
}

// --- helpers ---

var errFakeNotImplemented = errors.New("auditcomplete_test: fake method not implemented")

func seedPlanArtifact(arts *fakeArtifactRepo, stageID uuid.UUID) {
	v := "standard_v1"
	arts.all = append(arts.all, &artifact.Artifact{
		ID:            uuid.New(),
		StageID:       stageID,
		Kind:          artifact.KindPlan,
		SchemaVersion: &v,
		Content:       json.RawMessage(`{}`),
		CreatedAt:     time.Now().UTC(),
	})
}

func seedPullRequestArtifact(arts *fakeArtifactRepo, stageID uuid.UUID) {
	arts.all = append(arts.all, &artifact.Artifact{
		ID:        uuid.New(),
		StageID:   stageID,
		Kind:      artifact.KindPullRequest,
		Content:   json.RawMessage(`{}`),
		CreatedAt: time.Now().UTC(),
	})
}

// auditCompleteAuditFake is a tiny audit.Repository fake that
// produces a real, hash-chained audit log so verifyChain (inside
// auditcomplete) accepts it. Only ListForRun, ListForRunByCategory,
// and AppendChained matter for these tests; other methods return
// a sentinel error so a stray call shows up clearly in failures.
type auditCompleteAuditFake struct {
	mu      sync.Mutex
	entries []*audit.Entry
}

func newAuditCompleteAuditFake() *auditCompleteAuditFake {
	return &auditCompleteAuditFake{}
}

func (f *auditCompleteAuditFake) appendTrace(t *testing.T, runID, stageID uuid.UUID, variant string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"variant": variant})
	f.appendChained(t, runID, &stageID, "trace_uploaded", payload)
}

func (f *auditCompleteAuditFake) appendChained(t *testing.T, runID uuid.UUID, stageID *uuid.UUID, category string, payload json.RawMessage) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if payload == nil {
		payload = json.RawMessage(`{}`)
	}
	var prev *string
	if n := len(f.entries); n > 0 {
		ph := f.entries[n-1].EntryHash
		prev = &ph
	}
	r := runID
	ts := time.Date(2026, 5, 7, 12, 0, int(len(f.entries)), 0, time.UTC)
	hash, err := audit.ComputeEntryHash(audit.HashInputs{
		RunID:     &r,
		StageID:   stageID,
		Timestamp: ts,
		Category:  category,
		Payload:   payload,
		PrevHash:  prev,
	})
	if err != nil {
		t.Fatalf("ComputeEntryHash: %v", err)
	}
	f.entries = append(f.entries, &audit.Entry{
		ID:        uuid.New(),
		Sequence:  int64(len(f.entries) + 1),
		RunID:     &r,
		StageID:   stageID,
		Timestamp: ts,
		Category:  category,
		Payload:   payload,
		PrevHash:  prev,
		EntryHash: hash,
	})
}

func (f *auditCompleteAuditFake) Append(context.Context, audit.AppendParams) (*audit.Entry, error) {
	return nil, errFakeNotImplemented
}

// AppendChained is called by writeApprovalAudit on a successful
// approve. Record-and-return is enough; the entry shape isn't
// asserted here.
func (f *auditCompleteAuditFake) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	rid := p.RunID
	return &audit.Entry{ID: uuid.New(), RunID: &rid}, nil
}

func (f *auditCompleteAuditFake) AppendGlobalChained(context.Context, audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return nil, errFakeNotImplemented
}

func (f *auditCompleteAuditFake) ListGlobal(context.Context) ([]*audit.Entry, error) {
	return nil, nil
}

func (f *auditCompleteAuditFake) ListAll(context.Context, audit.ListAllParams) ([]*audit.Entry, error) {
	return nil, nil
}

func (f *auditCompleteAuditFake) Get(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, errFakeNotImplemented
}

func (f *auditCompleteAuditFake) LastForRun(context.Context, uuid.UUID) (*audit.Entry, error) {
	return nil, errFakeNotImplemented
}

func (f *auditCompleteAuditFake) ListForRun(_ context.Context, _ uuid.UUID) ([]*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*audit.Entry{}, f.entries...), nil
}

func (f *auditCompleteAuditFake) ListForRunByCategory(_ context.Context, _ uuid.UUID, category string) ([]*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []*audit.Entry{}
	for _, e := range f.entries {
		if e.Category == category {
			out = append(out, e)
		}
	}
	return out, nil
}
