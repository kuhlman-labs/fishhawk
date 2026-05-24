// Package audit provides the append-only audit log and chain verification.
//
// Test authors that need an audit.Repository should embed BaseFake and override
// only the methods their test exercises. This avoids writing interface-
// completeness stubs that break every time Repository gains a new method.
package audit

import (
	"context"

	"github.com/google/uuid"
)

// BaseFake is a no-op implementation of Repository. Embed it in a test fake
// and override only the methods the test exercises.
//
// Write/single-entity methods return nil, ErrNotFound. Slice methods return
// nil, nil.
type BaseFake struct{}

// compile-time check: BaseFake must satisfy Repository.
var _ Repository = BaseFake{}

// Append returns nil, ErrNotFound.
func (BaseFake) Append(_ context.Context, _ AppendParams) (*Entry, error) {
	return nil, ErrNotFound
}

// AppendChained returns nil, ErrNotFound.
func (BaseFake) AppendChained(_ context.Context, _ ChainAppendParams) (*Entry, error) {
	return nil, ErrNotFound
}

// AppendGlobalChained returns nil, ErrNotFound.
func (BaseFake) AppendGlobalChained(_ context.Context, _ GlobalChainAppendParams) (*Entry, error) {
	return nil, ErrNotFound
}

// Get returns nil, ErrNotFound.
func (BaseFake) Get(_ context.Context, _ uuid.UUID) (*Entry, error) {
	return nil, ErrNotFound
}

// ListForRun returns nil, nil.
func (BaseFake) ListForRun(_ context.Context, _ uuid.UUID) ([]*Entry, error) {
	return nil, nil
}

// ListGlobal returns nil, nil.
func (BaseFake) ListGlobal(_ context.Context) ([]*Entry, error) {
	return nil, nil
}

// LastForRun returns nil, ErrNotFound.
func (BaseFake) LastForRun(_ context.Context, _ uuid.UUID) (*Entry, error) {
	return nil, ErrNotFound
}

// ListForRunByCategory returns nil, nil.
func (BaseFake) ListForRunByCategory(_ context.Context, _ uuid.UUID, _ string) ([]*Entry, error) {
	return nil, nil
}

// ListAll returns nil, nil.
func (BaseFake) ListAll(_ context.Context, _ ListAllParams) ([]*Entry, error) {
	return nil, nil
}

// ChainsByParent returns nil, nil.
func (BaseFake) ChainsByParent(_ context.Context, _ uuid.UUID, _ bool) ([]*Entry, error) {
	return nil, nil
}
