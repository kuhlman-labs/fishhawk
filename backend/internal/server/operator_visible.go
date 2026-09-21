package server

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
)

// notifyOperatorVisible is the explicit writer-side intent marker from
// #3406: "I just appended audit category `category` and intend the operator
// to see it on the anchor / status comment timeline." It exists because
// nothing else binds a writer's intent to the renderer's registration —
// issuecomment.activityCategories is a closed set in another package, and a
// category a writer appends but never registers there renders NOTHING, in
// total silence (the acceptance_scenario_retirement_dropped shape that
// motivated the issue).
//
// Contract:
//
//   - The category MUST be admitted by issuecomment.RendersActivity. When it
//     is not, this logs at Error naming the category and the file to edit —
//     runtime defence in depth. The check runs BEFORE and independently of
//     notifyStatusUpdate's issueNotifier nil-guard, because the intent
//     mismatch is a defect whether or not a notifier is configured.
//   - The static gate in operator_visible_gate_test.go resolves every call
//     site's argument through go/types (a string literal or a string-kinded
//     constant; anything else fails closed) and asserts it is admitted by
//     both issuecomment.RendersActivity and audit.IsKnownCategory, so the
//     Error branch below is the last line, not the first.
//   - The refresh itself delegates to notifyStatusUpdate with the category as
//     its `source` tag, so the anchor / status comment behaviour is unchanged
//     from a direct notifyStatusUpdate call.
//
// notifyStatusUpdate's `source` is a call-site TRANSITION tag and must never
// be an audit category the writer intends the operator to see; that is the
// other half the gate enforces (see auditOnlyStatusRefreshSources there).
func (s *Server) notifyOperatorVisible(ctx context.Context, runID uuid.UUID, category string) {
	if !issuecomment.RendersActivity(category) {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"operator-visible audit category is not renderable on the anchor; register it in issuecomment.activityCategories (backend/internal/issuecomment/status_template.go)",
			slog.String("category", category),
			slog.String("run_id", runID.String()),
		)
	}
	s.notifyStatusUpdate(ctx, runID, category)
}
