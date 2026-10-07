package server

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// These tests pin the orchestratorRepo fake's copy-on-return contract (#4033):
// every run-returning method hands out a shallow copy taken under r.mu, never
// the map-held pointer, mirroring the postgres repo's fresh row per read. A
// caller reading a returned field must not share memory with a concurrent
// TransitionRun writing it under the lock (the #2586 / #3226 fake-race class).

const fakeCallerSentinel = "mutated/by-caller"

// mapHeldRepo reads the map-held run for id under the fake's lock.
func mapHeldRepo(rr *orchestratorRepo, id uuid.UUID) (*run.Run, string) {
	rr.mu.Lock()
	defer rr.mu.Unlock()
	held := rr.runs[id]
	return held, held.Repo
}

// TestOrchestratorRepo_RunReadsReturnCopies: for each run-returning method,
// mutating the RETURNED pointer must not reach the map-held run, and the
// returned pointer must not be the stored one.
func TestOrchestratorRepo_RunReadsReturnCopies(t *testing.T) {
	ctx := context.Background()
	rows := []struct {
		name string
		// seedState is the state the run is seeded in (RetryRun needs failed).
		seedState run.State
		call      func(rr *orchestratorRepo, id uuid.UUID) (*run.Run, error)
	}{
		{"GetRun", run.StateRunning, func(rr *orchestratorRepo, id uuid.UUID) (*run.Run, error) {
			return rr.GetRun(ctx, id)
		}},
		{"TransitionRun", run.StateRunning, func(rr *orchestratorRepo, id uuid.UUID) (*run.Run, error) {
			return rr.TransitionRun(ctx, id, run.StateSucceeded)
		}},
		{"RetryRun", run.StateFailed, func(rr *orchestratorRepo, id uuid.UUID) (*run.Run, error) {
			return rr.RetryRun(ctx, id, run.StateRunning)
		}},
		{"SetRunPullRequestURL", run.StateRunning, func(rr *orchestratorRepo, id uuid.UUID) (*run.Run, error) {
			return rr.SetRunPullRequestURL(ctx, id, "https://example.test/pr/1")
		}},
		{"AddRunCost", run.StateRunning, func(rr *orchestratorRepo, id uuid.UUID) (*run.Run, error) {
			return rr.AddRunCost(ctx, id, 1.5, "m")
		}},
		{"SetRunPredictedRuntimeMinutes", run.StateRunning, func(rr *orchestratorRepo, id uuid.UUID) (*run.Run, error) {
			return rr.SetRunPredictedRuntimeMinutes(ctx, id, 7)
		}},
		{"ListRuns", run.StateRunning, func(rr *orchestratorRepo, id uuid.UUID) (*run.Run, error) {
			out, err := rr.ListRuns(ctx, run.ListRunsFilter{})
			if err != nil {
				return nil, err
			}
			for _, row := range out {
				if row.ID == id {
					return row, nil
				}
			}
			return nil, run.ErrNotFound
		}},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			rr := newOrchestratorRepo()
			seeded := rr.seedRun()
			rr.mu.Lock()
			rr.runs[seeded.ID].State = tc.seedState
			rr.mu.Unlock()

			got, err := tc.call(rr, seeded.ID)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			held, _ := mapHeldRepo(rr, seeded.ID)
			if got == held {
				t.Fatalf("%s returned the map-held *run.Run pointer; want a copy taken under the lock", tc.name)
			}

			got.Repo = fakeCallerSentinel

			if _, repo := mapHeldRepo(rr, seeded.ID); repo != "x/y" {
				t.Errorf("%s: map-held Repo = %q after the caller mutated its returned run, want %q (caller mutation leaked into the store)",
					tc.name, repo, "x/y")
			}
			fresh, err := rr.GetRun(ctx, seeded.ID)
			if err != nil {
				t.Fatalf("fresh GetRun: %v", err)
			}
			if fresh.Repo != "x/y" {
				t.Errorf("%s: fresh GetRun Repo = %q, want %q", tc.name, fresh.Repo, "x/y")
			}
		})
	}
}

// TestOrchestratorRepo_MutationsStillLandOnMapHeldRun guards against the
// over-correction of cloning BEFORE mutating: the writes must land on the stored
// row, observable through a fresh GetRun and through the seedRun live handle.
func TestOrchestratorRepo_MutationsStillLandOnMapHeldRun(t *testing.T) {
	ctx := context.Background()
	rr := newOrchestratorRepo()
	seeded := rr.seedRun()

	if _, err := rr.AddRunCost(ctx, seeded.ID, 2.25, "model-x"); err != nil {
		t.Fatalf("AddRunCost: %v", err)
	}
	if _, err := rr.SetRunPullRequestURL(ctx, seeded.ID, "https://example.test/pr/9"); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}
	if _, err := rr.SetRunPredictedRuntimeMinutes(ctx, seeded.ID, 11); err != nil {
		t.Fatalf("SetRunPredictedRuntimeMinutes: %v", err)
	}
	if _, err := rr.TransitionRun(ctx, seeded.ID, run.StateSucceeded); err != nil {
		t.Fatalf("TransitionRun: %v", err)
	}

	// Single goroutine: no lock needed to read the seedRun live handle.
	fresh, err := rr.GetRun(ctx, seeded.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	for name, r := range map[string]*run.Run{"fresh GetRun": fresh, "seedRun handle": seeded} {
		if r.State != run.StateSucceeded {
			t.Errorf("%s: State = %q, want succeeded", name, r.State)
		}
		if r.CostUSDTotal != 2.25 || r.ResolvedModel != "model-x" {
			t.Errorf("%s: cost = %v model = %q, want 2.25 / model-x", name, r.CostUSDTotal, r.ResolvedModel)
		}
		if r.PullRequestURL == nil || *r.PullRequestURL != "https://example.test/pr/9" {
			t.Errorf("%s: PullRequestURL = %v, want https://example.test/pr/9", name, r.PullRequestURL)
		}
		if r.PredictedRuntimeMinutes != 11 {
			t.Errorf("%s: PredictedRuntimeMinutes = %d, want 11", name, r.PredictedRuntimeMinutes)
		}
	}

	// The failed -> running reopen also lands on the stored row.
	seeded.State = run.StateFailed
	if _, err := rr.RetryRun(ctx, seeded.ID, run.StateRunning); err != nil {
		t.Fatalf("RetryRun: %v", err)
	}
	if seeded.State != run.StateRunning {
		t.Errorf("RetryRun: stored State = %q, want running", seeded.State)
	}
}

// TestOrchestratorRepo_ConcurrentGetRunAndTransitionRun pins the exact #4033
// reader/writer shape: a reader inspecting State on the run GetRun returned
// while TransitionRun writes the stored row's State under the lock. It passes
// trivially without -race; scripts/test runs go test -race, where an aliased
// GetRun pointer makes the detector report the unsynchronized read.
func TestOrchestratorRepo_ConcurrentGetRunAndTransitionRun(t *testing.T) {
	ctx := context.Background()
	rr := newOrchestratorRepo()
	seeded := rr.seedRun()

	const reads = 2000
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
	)
	start.Add(1)
	done.Add(2)
	go func() {
		defer done.Done()
		start.Wait()
		for i := 0; i < reads; i++ {
			got, err := rr.GetRun(ctx, seeded.ID)
			if err != nil {
				t.Errorf("GetRun: %v", err)
				return
			}
			_ = got.State.IsTerminal()
		}
	}()
	go func() {
		defer done.Done()
		start.Wait()
		if _, err := rr.TransitionRun(ctx, seeded.ID, run.StateSucceeded); err != nil {
			t.Errorf("TransitionRun: %v", err)
		}
	}()
	start.Done()
	done.Wait()

	got, err := rr.GetRun(ctx, seeded.ID)
	if err != nil {
		t.Fatalf("final GetRun: %v", err)
	}
	if got.State != run.StateSucceeded {
		t.Errorf("final State = %q, want succeeded", got.State)
	}
}
