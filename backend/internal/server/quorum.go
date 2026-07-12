package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
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

// delegatedApproverSubjects returns the set of approver subjects whose prior
// approval_submitted audit row on this run recorded a non-empty delegated rule
// (the ADR-040 delegated path, #1026). The approval row itself does NOT retain
// delegated status, so the audit payload is the single source of truth: a
// delegated approval is recorded but must NEVER count toward the human quorum
// (#1709 binding acceptance criterion). Without this, a prior delegated
// non-agent (human) approver — which eligibleApprover cannot distinguish from a
// normal human — would be counted when the next non-delegated approver submits.
// Fail-open to an empty set on a nil repo or a read error: an unreadable audit
// history is treated as "no known delegations", matching the rest of the quorum
// path's best-effort posture (an over-count risk here is bounded by the same
// audit history the gate itself is derived from).
func (s *Server) delegatedApproverSubjects(ctx context.Context, runID uuid.UUID) map[string]struct{} {
	out := make(map[string]struct{})
	if s.cfg.AuditRepo == nil {
		return out
	}
	entries, err := s.cfg.AuditRepo.ListForRun(ctx, runID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"quorum: delegated approvers: list run audit failed",
			slog.String("run_id", runID.String()),
			slog.String("error", err.Error()),
		)
		return out
	}
	for _, e := range entries {
		if e.Category != "approval_submitted" || e.ActorSubject == nil {
			continue
		}
		var payload struct {
			Delegated string `json:"delegated"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		if payload.Delegated != "" {
			out[*e.ActorSubject] = struct{}{}
		}
	}
	return out
}

// countDistinctEligibleApprovers counts the DISTINCT eligible-approver
// subjects among the stage's approve-decision rows (the just-inserted
// submission included). A subject whose prior approval was delegated
// (delegatedApproverSubjects) is excluded — delegated approvals are recorded
// but never counted toward the human quorum, and the approval row does not
// retain delegated status. Fail-open to 0 on a list error — an unreadable
// approval history never spuriously advances the gate.
//
// Read-after-write quorum, #1734 (deliberately NOT serialized).
//
// This count is a read-after-write against the approval rows with no
// serialization (no in-process mutex, no advisory lock, no SELECT FOR UPDATE),
// and that is correct today by construction:
//
//   - Commit-before-count ordering. ApprovalRepo.Submit (approval/postgres.go)
//     runs CreateApproval through approvaldb.New(r.pool) in AUTOCOMMIT, so the
//     approver's row is committed before Submit returns. approveStageAs then
//     calls this count strictly AFTER Submit returns — never before.
//   - Single primary, READ COMMITTED. Every count read hits the one
//     FISHHAWKD_DATABASE_URL pool; there is no read-replica / lagging-standby
//     routing. Under READ COMMITTED each statement sees a fresh snapshot as of
//     its own start, so a later count observes its own just-committed row plus
//     every previously-committed eligible row.
//
// Therefore the reviewer's two-approver liveness stall ("A and B each observe
// only their own row, so neither reaches count=2 and the gate never advances")
// is a TEMPORAL CONTRADICTION and cannot occur. The stall needs both counts to
// precede the other approver's commit: commitA<countA, commitB<countB,
// countA<commitB, countB<commitA — which chains to commitA<commitA. Whichever
// approver commits LAST always counts after both rows are durable and observes
// the full quorum, so the gate is always reachable under concurrency.
//
// Safety on the double-advance path is preserved independently: if a second
// (or racing) submission reaches an already-advanced stage, advanceStage's
// TransitionStage InvalidTransition guard fires and approveStageAs surfaces it
// (approveActionError failedAt gateActionAdvance → 409), so no stage is
// advanced twice. (The "SELECT FOR UPDATE on the stage row" line in
// approval/approval.go's package doc is stale — the current path holds no such
// lock; the transition guard is the actual safety mechanism.)
//
// Invariants a future change MUST preserve, or this analysis reopens:
//   - Never route this count through a read-replica / lagging read path.
//   - Never evaluate the count before Submit's row commits (never reorder count
//     ahead of Submit).
//   - Never wrap Submit in an uncommitted long-lived transaction still open at
//     count time.
//
// Pinned by TestPostgres_Submit_ConcurrentQuorum (approval/postgres_test.go —
// real Postgres commit semantics: N concurrent distinct approvers, the last
// committer observes all N) and the server-layer no-stall / no-double-advance
// tests in quorum_test.go. An in-process mutex would be wrong regardless — the
// API tier runs multiple replicas — and a Postgres advisory lock would only
// over-serialize a race that provably does not occur.
func (s *Server) countDistinctEligibleApprovers(ctx context.Context, runID, stageID uuid.UUID, changeAuthor string) int {
	rows, err := s.cfg.ApprovalRepo.ListForStage(ctx, stageID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"quorum: count eligible approvers: list approvals failed",
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return 0
	}
	delegated := s.delegatedApproverSubjects(ctx, runID)
	seen := make(map[string]struct{})
	for _, a := range rows {
		if a.Decision != approval.DecisionApprove {
			continue
		}
		if !eligibleApprover(a.ApproverSubject, changeAuthor) {
			continue
		}
		if _, ok := delegated[a.ApproverSubject]; ok {
			// Delegated approval: recorded but never counted (#1709).
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
	// Forge-resolution fields (E39.5 / #1710). MinPermission/MemberOf above
	// carry the REQUIRED tier + ref; these record what the forge RESOLVED
	// for the submitter and the predicate verdict. Additive + omitempty so a
	// snapshot with no forge resolution (the campaign auto-driver / agent
	// path, and legacy count-only gates) is byte-identical to today, keeping
	// the E9 Export v1 hash chain and strict decode unaffected (#1709).
	ResolvedPermission string `json:"resolved_permission,omitempty"`
	MemberResolved     *bool  `json:"member_resolved,omitempty"`
	PredicateResult    string `json:"predicate_result,omitempty"`
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

// predicateOutcome is the discriminated result of resolvePredicates.
type predicateOutcome int

const (
	// predicateSatisfied: every configured predicate resolved and passed.
	predicateSatisfied predicateOutcome = iota
	// predicateRejected: a predicate resolved but the submitter did NOT
	// meet it (insufficient permission tier or non-membership).
	predicateRejected
	// predicateUnavailable: a predicate could not be resolved (a forge
	// error / rate-limit, an empty repo, or an unparseable required tier)
	// — the gate fails CLOSED and the caller returns a retryable 503.
	predicateUnavailable
)

// predicateResolution carries the forge-resolved values for the snapshot on
// a satisfied evaluation: the resolved permission tier (when min_permission
// was configured) and the resolved membership (when member_of was
// configured; nil otherwise).
type predicateResolution struct {
	ResolvedPermission string
	MemberResolved     *bool
}

// resolvePredicates evaluates the approvals block's forge predicates against
// the submitter, calling IdentityProvider.PermissionLevel when MinPermission
// is set and ResolveMembership when MemberOf is set. Each configured
// predicate is evaluated EXACTLY ONCE per call (no caching / memoization) so
// mock call-count assertions hold and every approval event makes its own
// forge calls. It returns one of three discriminated outcomes:
//
//   - predicateUnavailable: any non-nil forge error (including
//     identity.ErrRateLimited), an empty repo when a permission tier is
//     required, or an unparseable MinPermission (fail-closed — never waved
//     through). The returned *predicateResolution carries whatever resolved
//     before the failure (best-effort provenance).
//   - predicateRejected: a resolved permission below the required tier OR a
//     resolved membership of false. The resolution carries the resolved
//     value(s) for the rejection snapshot.
//   - predicateSatisfied: every configured predicate passed; the resolution
//     carries the resolved value(s) for the approval snapshot.
//
// The returned predicate string names which predicate produced a
// rejected/unavailable outcome ("min_permission" | "member_of"); it is empty
// on satisfied.
func (s *Server) resolvePredicates(ctx context.Context, repo, subject string, approvals *spec.Approvals) (predicateOutcome, *predicateResolution, string) {
	res := &predicateResolution{}
	if approvals.MinPermission != "" {
		// A repo permission tier cannot be resolved without a repo (a
		// non-GitHub / ad-hoc trigger leaves run.Repo empty). Fail closed
		// rather than wave the approver through.
		if repo == "" {
			return predicateUnavailable, res, "min_permission"
		}
		required, ok := identity.ParsePermission(approvals.MinPermission)
		if !ok {
			// Should not happen post-schema-validation (the enum is closed);
			// treat an unparseable required tier as unavailable, never
			// satisfied.
			return predicateUnavailable, res, "min_permission"
		}
		perm, err := s.cfg.IdentityProvider.PermissionLevel(ctx, repo, subject)
		if err != nil {
			return predicateUnavailable, res, "min_permission"
		}
		res.ResolvedPermission = string(perm)
		if !perm.AtLeast(required) {
			return predicateRejected, res, "min_permission"
		}
	}
	if approvals.MemberOf != "" {
		member, err := s.cfg.IdentityProvider.ResolveMembership(ctx, approvals.MemberOf, subject)
		if err != nil {
			return predicateUnavailable, res, "member_of"
		}
		res.MemberResolved = &member
		if !member {
			return predicateRejected, res, "member_of"
		}
	}
	return predicateSatisfied, res, ""
}
