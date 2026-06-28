package server

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// ensureGovernanceAuditEntry idempotently heals the chained governance audit
// entry that pairs with a just-persisted artifact (#1396).
//
// The artifact-ship handlers persist an artifact then append a chained
// governance audit entry as two non-atomic steps guarded by a GetByHash
// idempotency short-circuit. If Create succeeds but AppendChained fails the
// handler 500s with the artifact already persisted; an identical retry hits
// the GetByHash branch and — before this helper — returned 200 without
// re-attempting the missing audit append, leaving an artifact with no
// governance record. server.Config exposes only the separate audit.Repository
// and artifact.Repository interfaces (no shared *pgxpool.Pool / tx handle), so
// a true shared transaction is impractical here; instead the idempotent-retry
// path self-heals.
//
// On the GetByHash hit the handler calls this helper with the artifact's id
// and a closure that appends the required entry. It lists the run's entries of
// the given category and checks whether any carries a matching artifact_id in
// its payload:
//
//   - present  → returns (false, nil); appendEntry is NOT invoked (idempotent).
//   - missing  → invokes appendEntry() and returns (true, appendErr); an append
//     failure surfaces as the error so the caller 500s and a further retry can
//     re-heal.
//   - read err → returns (false, err) so the caller fails closed with a 500;
//     a 500 (with a re-heal on retry) beats a possibly-gapped 200.
//
// Payload construction stays in the per-handler closure so each handler keeps
// its distinct payload shape; presence detection keys only on the audit
// payload's artifact_id field, which both create-path payloads embed.
func (s *Server) ensureGovernanceAuditEntry(ctx context.Context, runID uuid.UUID, category, artifactID string, appendEntry func() error) (healed bool, err error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, category)
	if err != nil {
		// Fail closed: governance integrity beats a possibly-gapped 200.
		return false, err
	}
	for _, e := range entries {
		var p struct {
			ArtifactID string `json:"artifact_id"`
		}
		if json.Unmarshal(e.Payload, &p) == nil && p.ArtifactID == artifactID {
			// The governance entry for this artifact is already present —
			// no-op (the common clean-retry case).
			return false, nil
		}
	}
	// No governance entry pairs with this artifact: a prior partial write
	// (Create succeeded, AppendChained failed) left it gapped. Append it now.
	return true, appendEntry()
}
