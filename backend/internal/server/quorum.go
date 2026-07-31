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
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// effectiveApprovalSubject maps the recording subject for an approval: a
// DELEGATED approval by a human-kind operator subject is recorded under the
// distinct operatorrole.DelegatedApprovalActorSubject identity (#2381), so the
// vote that #1709 records-but-never-counts no longer occupies the operator's own
// approver slot — leaving their later, real, non-delegated approve free to insert
// a fresh COUNTING row instead of colliding as a #986 duplicate against a gate
// with no un-approve verb. It is the SINGLE source of truth for the mapping, so
// the handler duplicate pre-check and the service Submit can never disagree.
//
// An already-agent subject (the campaign auto-driver's
// operatorrole.CampaignActorSubject) is returned UNCHANGED: it already attributes
// to an agent identity, keeps its own provenance, and stays byte-identical to
// today. A non-delegated approval is likewise returned unchanged — the remap fires
// only for delegated + human-kind.
func effectiveApprovalSubject(subject string, delegated bool) string {
	if delegated && actorKindForSubject(subject) != audit.ActorAgent {
		return operatorrole.DelegatedApprovalActorSubject
	}
	return subject
}

// Consumers of the one-decision-per-subject invariant, walked (#2381) so the
// distinct-identity remap is provably safe at each — NAMED, not asserted:
//
//   - findPriorApproval / the #986 duplicate pre-check — now keyed on the
//     effective subject, so the operator's real approve is no longer a duplicate.
//     That IS the fix.
//   - ApprovalRepo.Submit's (stage_id, approver_subject) ON CONFLICT DO NOTHING —
//     UNCHANGED, no migration; two delegated submissions still collapse to one
//     idempotent row under the synthetic subject.
//   - delegatedApproverSubjects — now records the SYNTHETIC subject, so the
//     operator's own subject can never land in the delegated set, which is what
//     lets countDistinctEligibleApprovers count their real vote once.
//   - distinctEligibleApproverSubjects / countDistinctEligibleApprovers — the
//     synthetic subject is excluded twice over (the unconditional agent floor in
//     eligibleApprover AND the delegated set) while the human is deduped by the
//     existing `seen` map — no double-count, no under-count.
//   - predicate_snapshot — snapshotIdentityFor records the synthetic subject and
//     submitterClass labels it "agent", the honest label for a delegated act.
//   - resolveChangeAuthor — its authorshipCategories allow-list holds only
//     CategoryOperatorCommitVouched, so an approval_submitted row never resolves
//     an author; the remap cannot shift author attribution.
//   - issuecomment renderApproverIdentity — already renders an operatorrole-
//     prefixed subject as "the operator agent (`…`, delegated: `…`)" with no
//     @-mention (notifier.go), so the synthetic subject surfaces mention-free.
//
// delegatedApproveWouldAdvance answers "could a delegated approve actually
// advance THIS gate?" for the auto-drive may_approve arm (#2381). It deliberately
// MIRRORS approveStageAs's own advance rule (reached := !delegated &&
// eligibleCount >= required, approvals.go) so the pre-check and the post-Submit
// path can never disagree, declining instead of recording a delegated vote that
// can only ever wedge the gate. It returns true ONLY for a genuine legacy
// no-approvals, no-escalation gate — the one gate a delegated approve still
// advances byte-for-byte as today.
//
// The four DECLINE modes, each fail-toward-the-operator:
//
//	(a) fetchApprovalsForStage returns a non-nil approvals block — a delegated
//	    submission is UNCONDITIONALLY uncounted (eligibleApprover's agent floor),
//	    so `reached` is always false on such a gate; it can never be advanced by a
//	    delegated vote.
//	(b) fetchApprovalsForStage errors — the requirement is unknown; fail closed.
//	(c) the gate declares no block but resolveStageEscalations reports a firing
//	    escalation — the same not-advancing rule the post-Submit nil-block branch
//	    applies (#2374).
//	(d) resolveStageEscalations errors — unknowable; fail closed.
func (s *Server) delegatedApproveWouldAdvance(ctx context.Context, stage *run.Stage) (bool, string) {
	approvals, ferr := s.fetchApprovalsForStage(ctx, stage)
	if ferr != nil {
		return false, "the gate's approvals requirement could not be read, so a delegated approval cannot be shown to advance it; handing the gate to the operator (fail-closed)"
	}
	if approvals != nil {
		return false, "the gate declares an approvals block requiring distinct human approvers; a delegated approval is recorded but never counted toward a human quorum, so it cannot advance this gate"
	}
	escReq, escErr := s.resolveStageEscalations(ctx, stage)
	if escErr != nil {
		return false, "the gate's escalation state could not be evaluated, so a delegated approval cannot be shown to advance it; handing the gate to the operator (fail-closed)"
	}
	if !escReq.IsZero() {
		return false, "an escalation is firing on this gate; a delegated approval does not advance an escalated gate (the same not-advancing rule the post-Submit nil-block path applies)"
	}
	return true, ""
}

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

// authorshipCategories is the CLOSED ALLOW-LIST of audit categories whose
// user-kind actor is the change AUTHOR (#2358). A user-kind audit row makes
// its actor the author only when its category records the operator putting
// change CONTENT into the run — today exactly one does: vouching a
// hand-pushed commit into the run's lineage (CategoryOperatorCommitVouched),
// which is the issue's own definition of authorship ("the identity that
// pushed the commits").
//
// Every OTHER user-kind category is operator GATE PARTICIPATION, not
// authorship: run_auto_driven (fishhawk_drive_run's mechanical
// record-before-dispatch attribution row, autodrive_http.go / #1961 — the
// least author-like event on a run), clarification_answered,
// approval_submitted (an approval reason carrying binding conditions shapes
// the implementation at least as much as a clarification answer does),
// scope-amendment decisions, concern waivers/deferrals, merge verdicts,
// retries and branch resets. Governance is not authorship.
//
// ALLOW-list, not a governance DENY-list, deliberately: the audit category
// set grows with every verb added, so a deny-list re-opens this wedge on the
// next one, while an allow-list's failure mode is "no author resolved" — the
// already-supported, already-tested fail-open branch. The discipline that
// implies: a future category recording operator-authored CONTENT must be
// registered HERE; a new gate verb needs no change. Keyed to the exported
// constant, never a string literal, so a rename that misses this map is a
// compile error rather than a silent behavior change.
var authorshipCategories = map[string]struct{}{
	CategoryOperatorCommitVouched: {},
}

// resolveChangeAuthor returns the originating human's subject: the
// ActorSubject of the run's earliest user-kind audit entry whose category is
// in authorshipCategories (ListForRun is sequence-ascending, so the earliest
// match over the FILTERED set wins). ok is false when no such actor exists
// yet — author separation-of-duties is then skipped (logged) while agent-SoD
// and quorum still apply. There is no run-level author field on run.Run, so
// the earliest authorship-category user actor is the deterministic,
// forge-neutral, stage-agnostic stand-in. Fail-open (ok=false) on a read
// error.
//
// Honest consequence: on a run that has reached only its PLAN gate, no
// authorship-category entry exists yet, so author separation-of-duties does
// not bite there at all. The plan-gate controls are the unconditional
// agent floor in eligibleApprover and the distinct-human quorum count.
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
		if _, ok := authorshipCategories[e.Category]; !ok {
			continue
		}
		if e.ActorKind != nil && *e.ActorKind == audit.ActorUser &&
			e.ActorSubject != nil && *e.ActorSubject != "" {
			return *e.ActorSubject, true
		}
	}
	return "", false
}

// approvalsNotAuthor is the spec.Approvals.Not token that turns author
// separation-of-duties ON for a gate. The schema closes Not to the lowercase
// enum ["author", "agent"], so an exact match needs no case-folding.
const approvalsNotAuthor = "author"

// approvalsExcludeAuthor reports whether the gate's approvals block declares
// `not:` including "author" (#2358). Before this, the author-SoD 403 fired on
// "the gate has an approvals block" and never read Not, so a declared `not:`
// was grammar rather than enforcement. A nil block, a nil/empty Not, or a Not
// without "author" is false; an unrecognised member is ignored, not an error.
//
// There is deliberately NO agent counterpart that could turn the agent leg
// OFF: agent exclusion is an UNCONDITIONAL floor enforced by eligibleApprover
// via actorKindForSubject (actor.go), so `not: [agent]` merely restates the
// floor and omitting it does not permit an automated identity to satisfy a
// human quorum. That asymmetry is #1709's binding acceptance criterion, not
// an oversight.
func approvalsExcludeAuthor(a *spec.Approvals) bool {
	if a == nil {
		return false
	}
	for _, n := range a.Not {
		if n == approvalsNotAuthor {
			return true
		}
	}
	return false
}

// eligibleApprover reports whether subject counts toward the human quorum:
// it is neither the change author (separation of duties, only when the gate
// declares `not:` including "author" — excludeAuthor) nor an agent-kind
// subject (operator-agent / delegated submissions are excluded from human
// quorum, UNCONDITIONALLY, whether or not `not:` names "agent"). A ""
// changeAuthor (unresolved) disables only the author leg.
func eligibleApprover(subject, changeAuthor string, excludeAuthor bool) bool {
	if excludeAuthor && changeAuthor != "" && subject == changeAuthor {
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
// Read-after-write serialization (#1734 — deliberately NOT serialized). This
// count is a read-after-write: approveStageAs calls ApprovalRepo.Submit
// (which autocommits the approver's row via approvaldb.New(r.pool) — a
// pool-backed, single-statement INSERT that commits BEFORE Submit returns) and
// only THEN calls this count, whose ListForStage runs on the SAME single
// Postgres primary under READ COMMITTED. PR #1733 deferred a concern that two
// concurrent approvers could each observe only their own row and stall below
// quorum. That stall is a temporal contradiction and cannot occur today:
//
//   - Each approver's own committed row is visible to its own later count
//     (commit_X < count_X), and READ COMMITTED shows every statement a fresh
//     snapshot including all rows committed before the statement started.
//   - The stall requires BOTH counts to precede the OTHER approver's commit:
//     count_A < commit_B AND count_B < commit_A. Combined with commit_A <
//     count_A and commit_B < count_B, that implies commit_A < commit_A — a
//     contradiction. So whichever approver commits LAST always observes the
//     full quorum in its own count, and the gate is always reachable.
//
// Because the race provably does not occur, an in-process mutex (incorrect
// anyway — the API tier runs multiple replicas) or a Postgres advisory lock
// would only over-serialize. The safety leg on the two-approvers-reach-quorum
// path is advanceStage's TransitionStage InvalidTransition guard: a losing /
// late approve that reaches a stage already off its gate cannot double-advance
// it (surfaced as an advance-phase error / HTTP 409). NOTE the stale package
// doc in backend/internal/approval/approval.go still describes a "SELECT FOR
// UPDATE on the stage row" — the current path holds no such lock; the
// transition guard is the actual safeguard.
//
// Invariants a future change MUST preserve to keep this reasoning sound:
//   - NEVER route this count (ListForStage) through a read-replica / lagging
//     read path — a committed row must be immediately visible to the count.
//   - NEVER evaluate this count BEFORE Submit commits the approver's row.
//   - NEVER wrap Submit in an uncommitted long-lived transaction still open at
//     count time (the count would not see the uncommitted sibling row).
//
// Pinned by TestPostgres_Submit_ConcurrentQuorum (approval package, pgtest —
// the last committer always sees full quorum under real Postgres commit
// interleaving) and TestApproveStageAs_Quorum_* (server package — the happy
// two-approver advance-once path and the InvalidTransition safety guard).
func (s *Server) countDistinctEligibleApprovers(ctx context.Context, runID, stageID uuid.UUID, changeAuthor string, excludeAuthor bool) int {
	return len(s.distinctEligibleApproverSubjects(ctx, runID, stageID, changeAuthor, excludeAuthor))
}

// distinctEligibleApproverSubjects is the enumeration countDistinctEligibleApprovers
// counts and the escalation re-validation (#2227) re-resolves against the forge.
// It applies the identical eligibility rules — approve decision only, the
// author/agent exclusions of eligibleApprover, the delegated-subject exclusion —
// and the same fail-open-to-empty posture on a list error, so a caller that only
// needs the COUNT sees behavior byte-identical to before this split. See
// countDistinctEligibleApprovers's doc comment for the read-after-write
// serialization reasoning and the invariants a future change must preserve.
func (s *Server) distinctEligibleApproverSubjects(ctx context.Context, runID, stageID uuid.UUID, changeAuthor string, excludeAuthor bool) []string {
	rows, err := s.cfg.ApprovalRepo.ListForStage(ctx, stageID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"quorum: count eligible approvers: list approvals failed",
			slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		)
		return nil
	}
	delegated := s.delegatedApproverSubjects(ctx, runID)
	seen := make(map[string]struct{})
	var subjects []string
	for _, a := range rows {
		if a.Decision != approval.DecisionApprove {
			continue
		}
		if !eligibleApprover(a.ApproverSubject, changeAuthor, excludeAuthor) {
			continue
		}
		if _, ok := delegated[a.ApproverSubject]; ok {
			// Delegated approval: recorded but never counted (#1709).
			continue
		}
		if _, dup := seen[a.ApproverSubject]; dup {
			continue
		}
		seen[a.ApproverSubject] = struct{}{}
		subjects = append(subjects, a.ApproverSubject)
	}
	return subjects
}

// countEscalatedForgeApprovers closes the cross-request TOCTOU (#2227) that a
// distinct-approver count alone cannot. When a fired escalation RAISES a forge
// predicate (member_of / min_permission), an approval recorded while the
// escalation was NOT firing was never membership-checked at Submit
// (checkApprovalPredicates enforces the forge predicate at INSERT time), yet the
// plain count would still credit it toward the raised quorum — letting a matching
// change clear on an approval that never satisfied its current membership
// constraint. This re-resolves the ESCALATED predicate against the forge for
// EACH distinct eligible approver and returns how many satisfy it NOW.
//
// The caller invokes it ONLY when the fired escalation itself carries a forge
// predicate, so a run with no escalation, a count-only escalation, or a gate
// whose forge predicate is purely BASELINE (already enforced at every Submit, no
// TOCTOU) pays no count-time forge calls — the E39.5 native member_of path stays
// byte-identical to today.
//
// FAIL CLOSED: any approver the forge cannot resolve (predicateUnavailable — a
// forge error / rate-limit, an empty repo, a nil IdentityProvider) returns
// ok=false, and the caller makes the gate unreachable this pass, the same
// not-advancing posture the escErr branch takes (the post-Submit path has no 503
// to return). A resolved-but-non-satisfying approver (predicateRejected) is
// simply not counted — that is the TOCTOU closure, not a failure.
func (s *Server) countEscalatedForgeApprovers(ctx context.Context, stage *run.Stage, subjects []string, effective escalatedApprovals) (int, bool) {
	if s.cfg.IdentityProvider == nil {
		return 0, false
	}
	var repo string
	if s.cfg.RunRepo != nil {
		if runRow, err := s.cfg.RunRepo.GetRun(ctx, stage.RunID); err == nil {
			repo = runRow.Repo
		}
	}
	n := 0
	for _, subject := range subjects {
		outcome, _, _ := s.resolvePredicates(ctx, repo, subject, effective)
		switch outcome {
		case predicateSatisfied:
			n++
		case predicateRejected:
			// Recorded but does not satisfy the escalated predicate now —
			// excluded from the raised quorum. This is the TOCTOU closure.
		default: // predicateUnavailable
			return 0, false
		}
	}
	return n, true
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
	// Escalation fields (E53.4 / #2227). CountRequired and MinPermission above
	// already carry the EFFECTIVE (escalated) values, so a stricter gate is
	// explainable from this row alone; these two record that an escalation is
	// what made them stricter, and the full membership CONJUNCTION the gate
	// enforced (MemberOf above stays the gate's own declared group, so the
	// baseline remains readable next to the raise). Additive + omitempty: a
	// run on a workflow declaring no escalations serialises byte-identically,
	// keeping the E9 Export v1 hash chain and strict decode unaffected.
	Escalated         bool     `json:"escalated,omitempty"`
	EscalatedMemberOf []string `json:"escalated_member_of,omitempty"`
}

// submitterClass labels the submitter relative to the quorum: "author" when
// it is the change author, "agent" for an agent-kind subject, otherwise
// "eligible".
//
// This is DESCRIPTIVE PROVENANCE for predicate_snapshot, not an eligibility
// verdict: on a gate whose `not:` omits "author" a resolved author is
// PERMITTED and COUNTED yet still labeled "author" here (#2358), so
// submitter_class:"author" can now co-occur with quorum_reached:true. Do not
// infer eligibility from the label — eligibleApprover owns that decision.
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
// The `approvals` argument is the ESCALATED requirement (escalatedApprovals),
// not the raw spec block: a fired escalation (E53.4 / #2227) may have raised
// the permission tier and ADDED groups. Membership is a CONJUNCTION — ALL
// groups must resolve true — so two escalations naming disjoint groups produce
// a gate no single approver can clear, which is the correct fail-closed reading
// for a control that may only raise. Any membership error still yields
// predicateUnavailable, so the conjunction inherits the fail-closed posture
// unchanged.
func (s *Server) resolvePredicates(ctx context.Context, repo, subject string, approvals escalatedApprovals) (predicateOutcome, *predicateResolution, string) {
	res := &predicateResolution{}
	if approvals.minPermission != "" {
		// A repo permission tier cannot be resolved without a repo (a
		// non-GitHub / ad-hoc trigger leaves run.Repo empty). Fail closed
		// rather than wave the approver through.
		if repo == "" {
			return predicateUnavailable, res, "min_permission"
		}
		required, ok := identity.ParsePermission(approvals.minPermission)
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
	for _, group := range approvals.memberOf {
		member, err := s.cfg.IdentityProvider.ResolveMembership(ctx, group, subject)
		if err != nil {
			return predicateUnavailable, res, "member_of"
		}
		// The LAST resolved membership is recorded on the snapshot; on a
		// rejection that is the group that failed, which is the actionable one.
		res.MemberResolved = &member
		if !member {
			return predicateRejected, res, "member_of"
		}
	}
	return predicateSatisfied, res, ""
}
