package server

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// closeRecorder is a fake GitHub backing server that records the
// PATCH .../pulls/{number} close call and the explanatory comment POST,
// so the #877 seam test can assert the dangling PR was actually closed.
type closeRecorder struct {
	mu           sync.Mutex
	closeCalls   int
	closedNumber string
	commentCalls int
}

func (c *closeRecorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/pulls/{number}",
		func(w http.ResponseWriter, r *http.Request) {
			c.mu.Lock()
			c.closeCalls++
			c.closedNumber = r.PathValue("number")
			c.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"number":42,"state":"closed","head":{"sha":"abc"}}`))
		})
	mux.HandleFunc("POST /repos/{owner}/{repo}/issues/{number}/comments",
		func(w http.ResponseWriter, r *http.Request) {
			c.mu.Lock()
			c.commentCalls++
			c.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1}`))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newPRServerWithGitHub builds a PR-upload server wired to a GitHub
// client pointing at ghSrv, mirroring newPRServer otherwise.
func newPRServerWithGitHub(t *testing.T, runID, stageID uuid.UUID, ghSrv *httptest.Server) (*Server, *signingFake, *fakeArtifactRepo, *auditFake, *promptRunRepo) {
	t.Helper()
	sf := newSigningFake()
	ar := newFakeArtifactRepo()
	au := newAuditFake()
	rr := newPromptRunRepo()
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID}
	gh := &githubclient.Client{
		BaseURL: ghSrv.URL,
		Tokens:  &ghTokensStub{tok: "ghs_test"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "gha_app_jwt_test", nil },
	}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		SigningRepo:  sf,
		ArtifactRepo: ar,
		AuditRepo:    au,
		RunRepo:      rr,
		GitHub:       gh,
	})
	return s, sf, ar, au, rr
}

func gatingRejectStage(runID, stageID uuid.UUID) *run.Stage {
	cat := run.FailureB
	reason := implementReviewGatingRejectReason
	return &run.Stage{
		ID:              stageID,
		RunID:           runID,
		Type:            run.StageTypeImplement,
		State:           run.StageStateFailed,
		FailureCategory: &cat,
		FailureReason:   &reason,
	}
}

func seedGitHubRun(rr *promptRunRepo, runID uuid.UUID, installationID *int64) {
	rr.getRuns[runID] = &run.Run{
		ID:             runID,
		Repo:           "o/r",
		State:          run.StateRunning,
		InstallationID: installationID,
	}
}

func auditCategoryCount(au *auditFake, category string) int {
	n := 0
	for _, e := range au.appended {
		if e.Category == category {
			n++
		}
	}
	return n
}

// TestShipPullRequest_ClosesDanglingPROnGatingReject is the #877 seam
// test: when the implement stage is already failed category-B with the
// gating-reject reason, the just-opened PR is closed and the
// pull_request_closed_after_review_reject audit entry lands.
func TestShipPullRequest_ClosesDanglingPROnGatingReject(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	rec := &closeRecorder{}
	ghSrv := rec.server(t)
	s, sf, ar, au, rr := newPRServerWithGitHub(t, runID, stageID, ghSrv)
	rr.getStages[stageID] = gatingRejectStage(runID, stageID)
	inst := int64(12345)
	seedGitHubRun(rr, runID, &inst)
	priv, _ := sf.issue(t, runID)

	w := shipPRRequest(t, s, runID, stageID, priv, validPRBytes(t), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	// The honest open is still recorded: the PR artifact + the
	// pull_request_opened audit entry stay in place.
	if len(ar.all) != 1 {
		t.Errorf("artifacts = %d, want 1", len(ar.all))
	}
	if got := auditCategoryCount(au, "pull_request_opened"); got != 1 {
		t.Errorf("pull_request_opened audit entries = %d, want 1", got)
	}

	// The PR was closed on GitHub.
	rec.mu.Lock()
	closeCalls, closedNumber := rec.closeCalls, rec.closedNumber
	rec.mu.Unlock()
	if closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", closeCalls)
	}
	if closedNumber != "42" {
		t.Errorf("closed PR number = %q, want 42", closedNumber)
	}

	// And the close was audited.
	if got := auditCategoryCount(au, "pull_request_closed_after_review_reject"); got != 1 {
		t.Errorf("pull_request_closed_after_review_reject audit entries = %d, want 1", got)
	}
}

// TestShipPullRequest_NoCloseOnRunningStage proves the advisory/normal
// path is untouched: a running (not failed) implement stage takes no
// close action and drives the existing advanceImplementStageAfterPR
// terminal transition.
func TestShipPullRequest_NoCloseOnRunningStage(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	rec := &closeRecorder{}
	ghSrv := rec.server(t)
	s, sf, _, au, rr := newPRServerWithGitHub(t, runID, stageID, ghSrv)
	rr.getStages[stageID] = &run.Stage{
		ID:               stageID,
		RunID:            runID,
		Type:             run.StageTypeImplement,
		State:            run.StageStateRunning,
		RequiresApproval: true,
	}
	inst := int64(12345)
	seedGitHubRun(rr, runID, &inst)
	priv, _ := sf.issue(t, runID)

	w := shipPRRequest(t, s, runID, stageID, priv, validPRBytes(t), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	rec.mu.Lock()
	closeCalls := rec.closeCalls
	rec.mu.Unlock()
	if closeCalls != 0 {
		t.Errorf("close calls = %d, want 0", closeCalls)
	}
	if got := auditCategoryCount(au, "pull_request_closed_after_review_reject"); got != 0 {
		t.Errorf("unexpected close audit entries = %d, want 0", got)
	}
	// The existing terminal transition still fired (running →
	// awaiting_approval for a gated stage).
	sawAwaiting := false
	for _, c := range rr.transitionStageCalls {
		if c.StageID == stageID && c.To == run.StageStateAwaitingApproval {
			sawAwaiting = true
		}
	}
	if !sawAwaiting {
		t.Error("expected running → awaiting_approval transition on the unchanged path")
	}
}

// TestShipPullRequest_GatingRejectFailOpen proves the close is fail-open:
// a nil InstallationID skips the close (and the close audit) without
// panicking or failing the response.
func TestShipPullRequest_GatingRejectFailOpen(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	rec := &closeRecorder{}
	ghSrv := rec.server(t)
	s, sf, _, au, rr := newPRServerWithGitHub(t, runID, stageID, ghSrv)
	rr.getStages[stageID] = gatingRejectStage(runID, stageID)
	seedGitHubRun(rr, runID, nil) // no installation → fail-open skip
	priv, _ := sf.issue(t, runID)

	w := shipPRRequest(t, s, runID, stageID, priv, validPRBytes(t), "")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	rec.mu.Lock()
	closeCalls := rec.closeCalls
	rec.mu.Unlock()
	if closeCalls != 0 {
		t.Errorf("close calls = %d, want 0 (fail-open skip)", closeCalls)
	}
	if got := auditCategoryCount(au, "pull_request_closed_after_review_reject"); got != 0 {
		t.Errorf("close audit entries = %d, want 0 (close skipped)", got)
	}
}
