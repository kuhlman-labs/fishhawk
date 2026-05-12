package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// synchronizeRunRepo is a minimal run.Repository for the
// synchronize-webhook handler tests. Records ListRuns calls so the
// test can assert on the PR-URL filter, and stubs out the rest of
// the repository surface so the embedded auditcomplete.Compute path
// can walk without blowing up.
type synchronizeRunRepo struct {
	run.Repository
	mu             sync.Mutex
	listURLs       []string
	listResult     []*run.Run
	listErr        error
	stagesByRunID  map[uuid.UUID][]*run.Stage
	runsByID       map[uuid.UUID]*run.Run
	getStageByID   map[uuid.UUID]*run.Stage
	transitionsLog []run.StageState
}

func (r *synchronizeRunRepo) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f.PullRequestURL != nil {
		r.listURLs = append(r.listURLs, *f.PullRequestURL)
	}
	return r.listResult, r.listErr
}
func (r *synchronizeRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	if rn, ok := r.runsByID[id]; ok {
		return rn, nil
	}
	return &run.Run{ID: id}, nil
}
func (r *synchronizeRunRepo) ListStagesForRun(_ context.Context, id uuid.UUID) ([]*run.Stage, error) {
	return r.stagesByRunID[id], nil
}
func (r *synchronizeRunRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	if s, ok := r.getStageByID[id]; ok {
		return s, nil
	}
	return nil, run.ErrNotFound
}
func (r *synchronizeRunRepo) TransitionStage(_ context.Context, _ uuid.UUID, to run.StageState, _ *run.StageCompletion) (*run.Stage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transitionsLog = append(r.transitionsLog, to)
	return nil, nil
}

type synchronizeArtifactRepo struct {
	artifact.Repository
	byStage map[uuid.UUID][]*artifact.Artifact
}

func (r *synchronizeArtifactRepo) ListForStage(_ context.Context, stageID uuid.UUID) ([]*artifact.Artifact, error) {
	return r.byStage[stageID], nil
}

type synchronizeAuditRepo struct {
	audit.Repository
	entries []*audit.Entry
}

func (r *synchronizeAuditRepo) ListForRunByCategory(_ context.Context, _ uuid.UUID, _ string) ([]*audit.Entry, error) {
	return r.entries, nil
}
func (r *synchronizeAuditRepo) ListForRun(_ context.Context, _ uuid.UUID) ([]*audit.Entry, error) {
	return r.entries, nil
}
func (r *synchronizeAuditRepo) AppendChained(_ context.Context, _ audit.ChainAppendParams) (*audit.Entry, error) {
	return nil, nil
}

func TestRepublishOnSynchronize_NoMatchingRun_NoOp(t *testing.T) {
	// PR not managed by Fishhawk: ListRuns returns empty; the
	// handler short-circuits without computing or publishing. The
	// receiver still returns 202 to GitHub; the assertion here is
	// purely that we don't crash and don't reach into the compute
	// path with no data.
	rr := &synchronizeRunRepo{listResult: nil}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ArtifactRepo: &synchronizeArtifactRepo{},
		AuditRepo:    &synchronizeAuditRepo{},
	})

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url": "https://github.com/x/y/pull/42",
			"number":   42,
			"head":     map[string]any{"sha": "abc"},
		},
	})
	s.republishOnSynchronize(context.Background(), payload)

	rr.mu.Lock()
	defer rr.mu.Unlock()
	if len(rr.listURLs) != 1 {
		t.Errorf("expected 1 ListRuns call; got %d", len(rr.listURLs))
	}
	if rr.listURLs[0] != "https://github.com/x/y/pull/42" {
		t.Errorf("ListRuns filter = %q, want PR url", rr.listURLs[0])
	}
}

func TestRepublishOnSynchronize_MatchingRun_LooksUpAndComputes(t *testing.T) {
	// Fishhawk-managed PR: ListRuns returns the run; the handler
	// passes it into auditcomplete.Compute. The end-to-end happy
	// path here is light — auditcomplete has its own thorough
	// tests; this case asserts the handler routes correctly.
	runID := uuid.New()
	installID := int64(99)
	matchingRun := &run.Run{
		ID:             runID,
		Repo:           "x/y",
		InstallationID: &installID,
	}
	rr := &synchronizeRunRepo{
		listResult: []*run.Run{matchingRun},
		runsByID:   map[uuid.UUID]*run.Run{runID: matchingRun},
	}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ArtifactRepo: &synchronizeArtifactRepo{},
		AuditRepo:    &synchronizeAuditRepo{},
	})

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url": "https://github.com/x/y/pull/275",
			"number":   275,
			"head":     map[string]any{"sha": "feedface"},
		},
	})
	s.republishOnSynchronize(context.Background(), payload)

	rr.mu.Lock()
	defer rr.mu.Unlock()
	if len(rr.listURLs) != 1 || rr.listURLs[0] != "https://github.com/x/y/pull/275" {
		t.Errorf("expected 1 ListRuns call for PR url; got %+v", rr.listURLs)
	}
}

func TestRepublishOnSynchronize_MalformedPayload_NoCrash(t *testing.T) {
	// GitHub redelivery or hand-crafted payload missing the PR
	// object: handler logs + returns. No 5xx, no panic.
	rr := &synchronizeRunRepo{listErr: errors.New("never called")}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		ArtifactRepo: &synchronizeArtifactRepo{},
		AuditRepo:    &synchronizeAuditRepo{},
	})

	s.republishOnSynchronize(context.Background(), []byte(`{not valid json`))

	if len(rr.listURLs) != 0 {
		t.Errorf("malformed payload should NOT trigger a run lookup; got %+v", rr.listURLs)
	}
}

func TestRepublishOnSynchronize_MissingDeps_NoOp(t *testing.T) {
	// Unconfigured server (no RunRepo / Artifact / Audit) must
	// still tolerate the call cleanly. The dev posture is "GitHub
	// isn't wired"; the synchronize handler shouldn't crash when
	// every dep is nil.
	s := New(Config{Addr: "127.0.0.1:0"})

	payload, _ := json.Marshal(map[string]any{
		"pull_request": map[string]any{
			"html_url": "https://github.com/x/y/pull/1",
			"number":   1,
			"head":     map[string]any{"sha": "abc"},
		},
	})
	s.republishOnSynchronize(context.Background(), payload)
	// No assertion needed — the test passes if we don't panic.
}
