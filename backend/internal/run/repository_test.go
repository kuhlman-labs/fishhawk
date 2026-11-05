package run

import (
	"context"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// DecideScopeParkForTest exposes the helper behind the ScopeParkDecider verbs to
// postgres_test.go, which is an external test package. Both verbs always pass a
// valid completion, so only a direct call can reach the helper's own validation.
func DecideScopeParkForTest(ctx context.Context, repo Repository, stageID uuid.UUID, expectedAttempt string, to StageState, completion *StageCompletion, p audit.ChainAppendParams) (*Stage, bool, error) {
	return repo.(*postgresRepo).decideScopePark(ctx, stageID, expectedAttempt, to, completion, p)
}
