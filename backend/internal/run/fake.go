// Package run provides run and stage persistence and state-machine logic.
//
// Test authors that need a run.Repository should embed BaseFake and override
// only the methods their test exercises. This avoids writing interface-
// completeness stubs that break every time Repository gains a new method.
package run

import (
	"context"

	"github.com/google/uuid"
)

// BaseFake is a no-op implementation of Repository. Embed it in a test fake
// and override only the methods the test exercises.
//
// Single-entity methods return nil, ErrNotFound. Slice methods return nil, nil.
type BaseFake struct{}

// compile-time check: BaseFake must satisfy Repository.
var _ Repository = BaseFake{}

// CreateRun returns nil, ErrNotFound.
func (BaseFake) CreateRun(_ context.Context, _ CreateRunParams) (*Run, error) {
	return nil, ErrNotFound
}

// GetRun returns nil, ErrNotFound.
func (BaseFake) GetRun(_ context.Context, _ uuid.UUID) (*Run, error) {
	return nil, ErrNotFound
}

// GetRunByIdempotencyKey returns nil, ErrNotFound.
func (BaseFake) GetRunByIdempotencyKey(_ context.Context, _, _ string) (*Run, error) {
	return nil, ErrNotFound
}

// ListRuns returns nil, nil.
func (BaseFake) ListRuns(_ context.Context, _ ListRunsFilter) ([]*Run, error) {
	return nil, nil
}

// TransitionRun returns nil, ErrNotFound.
func (BaseFake) TransitionRun(_ context.Context, _ uuid.UUID, _ State) (*Run, error) {
	return nil, ErrNotFound
}

// RetryRun returns nil, ErrNotFound.
func (BaseFake) RetryRun(_ context.Context, _ uuid.UUID, _ State) (*Run, error) {
	return nil, ErrNotFound
}

// SetRunPullRequestURL returns nil, ErrNotFound.
func (BaseFake) SetRunPullRequestURL(_ context.Context, _ uuid.UUID, _ string) (*Run, error) {
	return nil, ErrNotFound
}

// CreateStage returns nil, ErrNotFound.
func (BaseFake) CreateStage(_ context.Context, _ CreateStageParams) (*Stage, error) {
	return nil, ErrNotFound
}

// GetStage returns nil, ErrNotFound.
func (BaseFake) GetStage(_ context.Context, _ uuid.UUID) (*Stage, error) {
	return nil, ErrNotFound
}

// ListStagesForRun returns nil, nil.
func (BaseFake) ListStagesForRun(_ context.Context, _ uuid.UUID) ([]*Stage, error) {
	return nil, nil
}

// ListStagesAwaitingApproval returns nil, nil.
func (BaseFake) ListStagesAwaitingApproval(_ context.Context) ([]*Stage, error) {
	return nil, nil
}

// ListReviewStagesAwaitingApproval returns nil, nil.
func (BaseFake) ListReviewStagesAwaitingApproval(_ context.Context) ([]*Stage, error) {
	return nil, nil
}

// ListDeployStagesAwaitingDeployment returns nil, nil.
func (BaseFake) ListDeployStagesAwaitingDeployment(_ context.Context) ([]*Stage, error) {
	return nil, nil
}

// ListDeployStagesRollbackPending returns nil, nil.
func (BaseFake) ListDeployStagesRollbackPending(_ context.Context) ([]*Stage, error) {
	return nil, nil
}

// ListStagesAwaitingChildren returns nil, nil.
func (BaseFake) ListStagesAwaitingChildren(_ context.Context) ([]*Stage, error) {
	return nil, nil
}

// ListStagesDispatched returns nil, nil.
func (BaseFake) ListStagesDispatched(_ context.Context) ([]*Stage, error) {
	return nil, nil
}

// TransitionStage returns nil, ErrNotFound.
func (BaseFake) TransitionStage(_ context.Context, _ uuid.UUID, _ StageState, _ *StageCompletion) (*Stage, error) {
	return nil, ErrNotFound
}

// RetryStage returns nil, ErrNotFound.
func (BaseFake) RetryStage(_ context.Context, _ uuid.UUID, _ StageState) (*Stage, error) {
	return nil, ErrNotFound
}
