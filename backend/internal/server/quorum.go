package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// quorum.go wires the forge-neutral spec.Approvals block (E39.2 / #1707)
// into the approval endpoint (E39.4 / #1709). When a gate carries an
// approvals block, an approve advances the stage only once a distinct
// eligible-approver quorum is reached, the change author may not
// self-approve, and delegated / agent-kind submissions are recorded but
// never counted. Gates with no approvals block keep the first-vote-advances
// path byte-for-byte; the only change on that path is the ADR-055 additive
// identity/channel enrichment on every approval_submitted audit row.

// splitProviderSubject parses a provider-qualified subject
// ("github:<login>" -> provider "github", rest "<login>"). An empty or
// prefixless subject yields provider "" and rest == subject, so a bare
// login or the "anonymous" fallback is never mis-attributed to a provider.
func splitProviderSubject(subject string) (provider, rest string) {
	i := strings.Index(subject, ":")
	if i <= 0 {
		return "", subject
	}
	return subject[:i], subject[i+1:]
}

// approvalChannel classifies how the approval reached the server, for the
// additive identity enrichment on the audit row (ADR-055):
//   - "delegated" when the submission opted into the ADR-040 delegated path
//     (a met may_approve condition), OR the caller elects it for an
//     agent-kind submitter that never counts toward human quorum;
//   - "interactive" for a cookie-session identity (TokenID == "");
//   - "api" for a bearer-token identity.
func approvalChannel(id Identity, delegated bool) string {
	if delegated {
		return "delegated"
	}
	if id.TokenID == "" {
		return "interactive"
	}
	return "api"
}

// resolveChangeAuthor returns the originating human's subject: the
// ActorSubject of the run's earliest user-kind audit entry (ListForRun is
// sequence-ascending). ok is false when no such actor exists yet — author
// separation-of-duties is then skipped (logged) while agent-SoD and quorum
// still apply. There is no run-level author field on run.Run, so the
// earliest user actor is the deterministic, forge-neutral, stage-agnostic
// stand-in ratified at the plan gate. Fail-open (ok=false) on a read error.
func (s *Server) resolveChangeAuthor(ctx context.Context, runID uuid.UUID) (string, bool) {
	if s.cfg.AuditRepo == nil {
		return "", false
	}
	entries, err := s.cfg.AuditRepo.ListForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"quorum: resolve change author: list run audit failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return "", false
	}
	for _, e := range entries {
		if e.ActorKind != nil && *e.ActorKind == audit.ActorUser &&
			e.ActorSubject != nil && *e.ActorSubject != "" {
			return *e.ActorSubject, true
		}
	}
	return "", false
}

// eligibleApprover reports whether subject counts toward the human quorum:
// it is neither the change author (separation of duties) nor an agent-kind
// subject (operator-agent / delegated submissions are excluded from human
// quorum). A "" changeAuthor (unresolved) disables only the author leg.
func eligibleApprover(subject, changeAuthor string) bool {
	if changeAuthor != "" && subject == changeAuthor {
		return false
	}
	return actorKindForSubject(subject) != audit.ActorAgent
}

// countDistinctEligibleApprovers counts the DISTINCT eligible-approver
// subjects among the stage's approve-decision rows (the just-inserted
// submission included). Fail-open to 0 on a list error — an unreadable
// approval history never spuriously advances the gate.
func (s *Server) countDistinctEligibleApprovers(ctx context.Context, stageID uuid.UUID, changeAuthor string) int {
	rows, err := s.cfg.ApprovalRepo.ListForStage(ctx, stageID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"quorum: count eligible approvers: list approvals failed",
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return 0
	}
	seen := make(map[string]struct{})
	for _, a := range rows {
		if a.Decision != approval.DecisionApprove {
			continue
		}
		if !eligibleApprover(a.ApproverSubject, changeAuthor) {
			continue
		}
		seen[a.ApproverSubject] = struct{}{}
	}
	return len(seen)
}

// snapshotIdentity is the provider-qualified submitter identity recorded on
// the approval audit row and inside the predicate snapshot.
type snapshotIdentity struct {
	Provider string `json:"provider"`
	Subject  string `json:"subject"`
}

// snapshotIdentityFor builds the identity object from a subject, splitting
// the provider prefix while keeping the full provider-qualified subject as
// provenance.
func snapshotIdentityFor(subject string) snapshotIdentity {
	provider, _ := splitProviderSubject(subject)
	return snapshotIdentity{Provider: provider, Subject: subject}
}

// predicateSnapshot records the inputs and outcome of a quorum-gate
// evaluation. It is marshaled into the approval_submitted payload under the
// key predicate_snapshot ONLY when the gate declares an approvals block;
// legacy-gate rows omit it (operator binding condition 2).
type predicateSnapshot struct {
	CountRequired  int              `json:"count_required"`
	CountEligible  int              `json:"count_eligible"`
	Identity       snapshotIdentity `json:"identity"`
	SubmitterClass string           `json:"submitter_class"`
	AuthMethod     string           `json:"auth_method,omitempty"`
	Channel        string           `json:"channel"`
	MinPermission  string           `json:"min_permission,omitempty"`
	MemberOf       string           `json:"member_of,omitempty"`
	QuorumReached  bool             `json:"quorum_reached"`
}

// submitterClass labels the submitter relative to the quorum: "author" when
// it is the change author, "agent" for an agent-kind subject, otherwise
// "eligible".
func submitterClass(subject, changeAuthor string, agent bool) string {
	if changeAuthor != "" && subject == changeAuthor {
		return "author"
	}
	if agent {
		return "agent"
	}
	return "eligible"
}

// fetchApprovalsForStage loads the workflow spec from the run row's cached
// bytes (#283) and returns the matched stage's approval-gate Approvals block
// — the single source of truth for "does this stage's gate require quorum".
// Returns (nil, nil) when the stage's gate uses the legacy Approvers form or
// has no approval gate; (nil, err) when the spec is unreadable (legacy run
// with no cached spec, parse failure), on which the caller falls open to the
// legacy first-vote path, matching checkApproverAuthorization's posture.
func (s *Server) fetchApprovalsForStage(ctx context.Context, stage *run.Stage) (*spec.Approvals, error) {
	runRow, err := s.cfg.RunRepo.GetRun(ctx, stage.RunID)
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	if len(runRow.WorkflowSpec) == 0 {
		return nil, errors.New("run has no cached workflow spec (legacy or non-dispatcher run)")
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		return nil, fmt.Errorf("parse workflow spec: %w", err)
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return nil, fmt.Errorf("workflow %q not in spec", runRow.WorkflowID)
	}
	for _, stg := range wf.Stages {
		if string(stg.Type) != string(stage.Type) {
			continue
		}
		for _, gate := range stg.Gates {
			if gate.Type == spec.GateTypeApproval && gate.Approvals != nil {
				return gate.Approvals, nil
			}
		}
		// Stage exists but carries no approvals-block gate.
		return nil, nil
	}
	return nil, fmt.Errorf("stage_type %q not in workflow %q", stage.Type, runRow.WorkflowID)
}
