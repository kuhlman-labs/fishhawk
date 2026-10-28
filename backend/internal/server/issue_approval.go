package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegation"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// CategoryApprovalCommentRefused is the audit category emitted when the
// issue-comment approval channel refuses a comment before Submit. Two reasons
// share it, named by the payload's `reason`: `over_cap_approve_comment` (#2583's
// over-cap approve refusal, made observable by E67.24 / #2621) and
// `comment_channel_refused` (ADR-123 D4 / #4755: the channel itself refused,
// with a `cause` — a workflow-v2 gate, or a gate whose approval rules could not
// be verified). It exists for the
// SILENT branch above all: a reply-comment ("+1") refusal deliberately posts no
// reply, so without this entry an operator's approval evaporated with no reply,
// no approval row, and no trace anywhere. The entry is written on the non-silent
// slash channel too, so the server-side record of a refusal is identical on both
// surfaces and an audit consumer needn't know which one refused — the payload's
// `silent` flag says whether a reply also went out. Same posture as
// CategoryApprovalConditionsTruncated (prompt.go): a best-effort breadcrumb for
// a drop that would otherwise be invisible.
const CategoryApprovalCommentRefused = "approval_comment_refused"

// HandleApprovalCommand implements webhook.ApprovalCommandHandler
// for /fishhawk approve and /fishhawk reject (#238), the reply-comment
// "+1" (E17.3 / #338) and reaction approvals the reactionpoller forwards.
// Steps: (1) resolve the run's awaiting-approval stage; (2) authorize the
// CHANNEL and the approver via authorizeCommentApproval — the ADR-123 D4 /
// #4755 refusal on every workflow-v2 gate (approve and reject) and on any
// gate whose approval rules cannot be verified, then the legacy v0/v1
// `approvers` role check; (3) the over-cap comment refusal; (4) Submit,
// advance, orchestrator dispatch. The result is addressed in the issue
// conversation rather than an HTTP response.
//
// The handler is intentionally conservative on errors: every path
// posts a reply comment so the reviewer always sees what happened,
// and returns nil to the caller so the webhook receiver doesn't
// surface 5xx for a slash-command flap. Best-effort throughout —
// the SPA approval path remains the canonical surface; this is a
// companion for the demo loop.
func (s *Server) HandleApprovalCommand(ctx context.Context, p webhook.ApprovalCommandParams) error {
	if !s.approvalCommandConfigured() {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"slash-command approval skipped: deps incomplete",
			slog.String("repo", p.Repo),
			slog.Int("issue", p.IssueNumber),
		)
		return nil
	}

	decision, ok := decodeMatchAction(p.Decision)
	if !ok {
		// Defense-in-depth: matchIssueComment shouldn't pass
		// anything other than approve / reject through to here.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"slash-command approval received unrecognized decision",
			slog.String("decision", string(p.Decision)))
		return nil
	}

	subject := namespacedApproverSubject(p.Forge, p.SenderLogin)
	if p.SenderLogin == "" {
		s.replyApproval(ctx, p, "Cannot approve: comment is missing sender identity (no GitHub login).")
		return nil
	}

	// Reply-comment approvals (E17.3 / #338) skip silently on every
	// "no, this comment isn't an approval" branch — an operator who
	// types "+1 yes I agree" on an issue thread that happens not to
	// have a Fishhawk plan should not get an unsolicited reply.
	// Slash-command approvals keep their explicit help replies; the
	// reviewer typed a deliberate command.
	silent := p.Source == webhook.ApprovalSourceReplyComment

	runRow, stage, found, err := s.findAwaitingApprovalStage(ctx, p.Forge, p.Repo, p.IssueNumber)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"slash-command approval: run lookup failed",
			slog.String("repo", p.Repo),
			slog.Int("issue", p.IssueNumber),
			slog.String("error", err.Error()))
		if silent {
			return nil
		}
		s.replyApproval(ctx, p, "Could not look up the run for this issue. Try the dashboard.")
		return nil
	}
	if !found {
		if silent {
			return nil
		}
		s.replyApproval(ctx, p, fmt.Sprintf("No stage on this issue's run is awaiting approval. (Subject: @%s)", subject))
		return nil
	}

	// ADR-018 (#311, #313): review-stage approval is owned by GitHub.
	// The PR merge event (#312) advances the stage; branch protection's
	// required-reviewers enforces the approver list. Reply with a help
	// message pointing at the PR rather than submitting an approval.
	// Plan-stage slash approvals continue to work. Reply-comment
	// approvals skip silently: the operator wasn't necessarily talking
	// about the review stage either.
	if stage.Type == run.StageTypeReview {
		if silent {
			return nil
		}
		s.replyApproval(ctx, p, reviewStageHelpReply(runRow, subject))
		return nil
	}

	// ADR-123 D4 (#4755): the issue-comment channel cannot evaluate a
	// workflow-v2 `approvals` predicate, so it is REFUSED on every v2 gate and
	// fails closed wherever the gate's approval rules cannot be verified. A
	// refusal records nothing and transitions nothing; it is written as an
	// approval_comment_refused audit entry on both channels (the reply-comment
	// one stays silent) so the drop is visible in the run record. It precedes
	// captureDelegationShadow, so a refused comment stamps no shadow either.
	authz := s.authorizeCommentApproval(ctx, stage, subject)
	if authz.refusal != "" {
		extra := map[string]any{"cause": authz.refusal}
		if authz.refusal != commentRefusalSpecUnreadable {
			extra["spec_major"] = authz.specMajor
		}
		s.writeApprovalCommentRefusedAudit(ctx, stage, subject, decision, p.Source, silent, approvalCommentRefusedReasonChannel, extra)
		if !silent {
			s.replyApproval(ctx, p, commentChannelRefusedReply(authz.refusal, subject))
		}
		return nil
	}
	if !authz.allowed {
		msg := authz.denyMsg
		if silent {
			// Reply-comment approvals (E17.4 / #339) from a
			// non-approver are silently dropped: a random "+1"
			// from a passerby shouldn't get a help reply explaining
			// the role-resolution failure. The slash path keeps its
			// help reply — the reviewer typed a deliberate command.
			s.cfg.Logger.LogAttrs(ctx, slog.LevelDebug,
				"reply-comment approval: unauthorized approver, skipping",
				slog.String("repo", p.Repo),
				slog.Int("issue", p.IssueNumber),
				slog.String("subject", subject))
			return nil
		}
		s.replyApproval(ctx, p, msg)
		return nil
	}

	// ADR-017 (#249, #253): the approval gate no longer reads
	// stage_check state. Reviewers approve based on plan + diff;
	// GitHub branch protection blocks the merge until the repo's
	// required checks report green. fishhawk_audit_complete is
	// PUBLISHED (#231); whether it is among those required checks
	// is the repository's own branch-protection configuration,
	// reported by the merge_gate readiness rung (E64.44 / #3161).

	// Over-cap approve-comment refusal (#2583). The slash-command channel
	// records p.Comment as the approve decision's comment, which is injected
	// verbatim as a binding approval condition (#558) and capped at
	// prompt.MaxApprovalConditionBytes at implement-prompt-build time. Refuse an
	// over-cap approve BEFORE Submit — recording no approval row — so a binding
	// instruction is never silently cut on this channel either, keeping the two
	// operator-facing approval channels consistent. A reject is not refused (its
	// comment feeds advisory PriorRejectionFeedback, not binding conditions).
	// Honors the reply-comment silent suppression: a passer-by "+1" that happens
	// to exceed the cap should not draw an unsolicited reply.
	//
	// E67.24 (#2621): the refusal is recorded as an approval_comment_refused
	// audit entry on BOTH channels, carrying a `silent` flag, so the drop is
	// visible in the run record rather than invisible. The suppression itself is
	// unchanged — a silent refusal still posts NO comment; the breadcrumb is a
	// trace, not a reply.
	if ok, msg, details := validateApprovalComment(decision, p.Comment); !ok {
		s.writeApprovalCommentRefusedAudit(ctx, stage, subject, decision, p.Source, silent, approvalCommentRefusedReasonOverCap, overCapRefusalExtra(details))
		if !silent {
			s.replyApproval(ctx, p, msg)
		}
		return nil
	}

	var commentPtr *string
	if p.Comment != "" {
		c := p.Comment
		commentPtr = &c
	}

	// Delegation shadow stamp (E82.1 / #3778): this slash-command / reply
	// channel is a HUMAN approval path outside approveStageAs, so it is hooked
	// here — captured on the PRE-decision state just before Submit, recorded
	// below only on a fresh, advanced decision (never a duplicate).
	shadow := s.captureDelegationShadow(ctx, stage.RunID, delegation.ActionApprove, subject, false)
	res, err := s.cfg.ApprovalRepo.Submit(ctx, approval.SubmitParams{
		StageID:         stage.ID,
		ApproverSubject: subject,
		Decision:        decision,
		Comment:         commentPtr,
		Surface:         approvalSurfaceForSource(p.Source),
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"slash-command approval: submit failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
		if silent {
			return nil
		}
		s.replyApproval(ctx, p, "Could not record the approval. Try the dashboard.")
		return nil
	}
	if !res.Inserted {
		// A previous submission from the same approver already
		// settled the gate. Reply-comment approvals (E17.4 / #339)
		// rely on the existing dedup as an idempotency guarantee:
		// a duplicate "+1" from the same reviewer is a no-op.
		// Slash commands surface the prior decision so the
		// reviewer who deliberately typed the command knows what
		// happened.
		if silent {
			return nil
		}
		s.replyApproval(ctx, p, fmt.Sprintf("@%s already submitted a `%s` decision on this stage; the prior decision wins.", subject, res.Approval.Decision))
		return nil
	}

	advanced, err := s.advanceStage(ctx, stage, decision)
	if err != nil {
		// Raced second decision (E50.15 / #2656): the approve advance is a
		// compare-and-swap anchored on the state this handler observed, so a
		// concurrent decision that already moved the stage refuses here rather
		// than silently re-running the run's post-approval side effects. That is
		// NOT a failure of this approver's action — it was superseded — so it
		// logs at WARN and the reply says the gate was ALREADY DECIDED rather
		// than that the approval could not be applied.
		var sce run.StageStateChangedError
		if errors.As(err, &sce) {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"slash-command approval: gate already decided by a concurrent decision",
				slog.String("stage_id", stage.ID.String()),
				slog.String("observed_state", string(sce.Expected)),
				slog.String("actual_state", string(sce.Actual)))
			if silent {
				return nil
			}
			s.replyApproval(ctx, p, fmt.Sprintf(
				"This gate was already decided by another approval; the stage is now `%s`. Your `%s` is recorded, but it did not advance the run.",
				sce.Actual, decision))
			return nil
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"slash-command approval: advance stage failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
		if silent {
			return nil
		}
		s.replyApproval(ctx, p, "Approval recorded but the run could not advance. Check the dashboard.")
		return nil
	}

	s.writeSlashApprovalAudit(ctx, advanced, res.Approval, p.Comment)
	s.recordDelegationShadow(ctx, shadow, &stage.ID, string(decision), "approval_submitted", nil)

	if s.cfg.Orchestrator != nil {
		if _, err := s.cfg.Orchestrator.Advance(ctx, advanced.RunID); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
				"slash-command approval: orchestrator advance failed",
				slog.String("run_id", advanced.RunID.String()),
				slog.String("stage_id", advanced.ID.String()),
				slog.String("error", err.Error()))
			// Don't unwind — the gate decision is recorded.
		}
	}

	// Plan-comment re-render (#377): a plan-stage approve or
	// reject re-fires the plan-on-issue hook, which edits the
	// existing comment in place and appends a `_Status:_` footer
	// naming the actor. Replaces the retired NotifyPlanApproved
	// broadcast (#274). The per-sender slash reply below still
	// covers the sender's own typed-command confirmation.
	if advanced.Type == run.StageTypePlan {
		s.notifyPlanReady(ctx, advanced.RunID, advanced)
	}

	if !silent {
		// Reply-comment approvals (E17.4 / #339) skip the per-call
		// success reply because the typed `+1` IS the user's
		// confirmation. Slash-command callers still get the
		// named-stage reply since their command had no inherent
		// "you said it" feedback. Plan-stage approvals also get
		// the plan-comment edit above (the broadcast equivalent).
		s.replyApproval(ctx, p, formatSuccessReply(decision, subject, runRow.ID, advanced))
	}

	// Sticky status comment (E20.4 / #330). Mirrors the HTTP
	// approval path: every approval (approve / reject, any stage
	// type) updates the sticky comment.
	s.notifyStatusUpdate(ctx, advanced.RunID, "slash_approval")
	return nil
}

// approvalCommandConfigured returns true when every dependency the
// slash-command handler needs is wired. Skipping cleanly when
// something's missing keeps the dispatcher's "ApprovalHandler is
// nil → silently skip" promise honest even if the handler is
// wired but its deps aren't.
func (s *Server) approvalCommandConfigured() bool {
	return s.cfg.RunRepo != nil &&
		s.cfg.ApprovalRepo != nil &&
		s.cfg.AuditRepo != nil &&
		s.issueNotifier != nil
}

// findAwaitingApprovalStage resolves (repo, issue_number) to the
// most-recent non-terminal run + its single awaiting-approval
// stage. Returns (run, stage, true, nil) on a hit; (nil, nil,
// false, nil) when no eligible stage exists. v0 has at most one
// awaiting-approval stage per run; the issue body's "out of scope:
// per-stage targeting" note documents that decision.
//
// Implementation pulls a bounded slice of recent runs via
// ListRuns(filter{Repo}) and filters in Go for the matching
// trigger_ref. For v0 cardinality (small handful of runs per repo
// per issue) this is faster than another sqlc query + index. If
// repo cardinality grows past 200 active runs, swap in a focused
// query.
func (s *Server) findAwaitingApprovalStage(ctx context.Context, approvalForge, repo string, issueNumber int) (*run.Run, *run.Stage, bool, error) {
	triggerRef := fmt.Sprintf("issue:%d", issueNumber)
	runs, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
		Repo:  repo,
		Limit: 200,
	})
	if err != nil {
		return nil, nil, false, fmt.Errorf("list runs: %w", err)
	}
	for _, r := range runs {
		if r.TriggerRef == nil || *r.TriggerRef != triggerRef {
			continue
		}
		if !forgeMatchesRun(approvalForge, r) {
			// Cross-forge refusal (E45.19 / #2036, made two-signal and
			// fail-closed by E45.22 / #2043): an approval sourced from a
			// different forge than the run must not act on its gate. The
			// run's forge is now derived from its own installation_ref
			// scheme AND its runner_kind, so this refuses in BOTH
			// directions — a GitLab approval on a colliding GitHub run,
			// and a GitHub approval on a gitlab_ci run.
			continue
		}
		if r.State.IsTerminal() {
			continue
		}
		stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, r.ID)
		if err != nil {
			return nil, nil, false, fmt.Errorf("list stages: %w", err)
		}
		for _, st := range stages {
			if st.State == run.StageStateAwaitingApproval {
				return r, st, true, nil
			}
		}
	}
	return nil, nil, false, nil
}

// normalizeForge canonicalizes an approval's source forge for
// comparison. Empty and "github" both mean GitHub — the legacy
// default the dispatcher leaves empty for GitHub events — so they
// normalize to the same canonical value. Any other value (e.g.
// webhook.ForgeGitLab) passes through verbatim.
func normalizeForge(f string) string {
	if f == "" || f == "github" {
		return "github"
	}
	return f
}

// forgeMatchesRun reports whether an approval sourced from
// approvalForge may act on run r.
//
// It FAILS CLOSED (E45.22 / #2043, HIGH 4). The prior implementation ignored
// the run entirely and returned normalizeForge(approvalForge) == "github", so
// a GitHub-sourced approval matched EVERY run — including a gitlab_ci one, the
// moment #1861's dormant plumbing started minting them. The refusal now
// compares the approval's forge against the RUN's own forge, derived from TWO
// INDEPENDENT SIGNALS, and a mismatch on EITHER refuses:
//
//   - runForgeFromRef(r.InstallationRef) — the scheme of the ADR-057 /
//     ADR-058 credential ref (migration 0076).
//   - r.RunnerKind == run.RunnerKindGitLabCI — the execution channel.
//
// Two signals rather than one because each covers the other's blind spot. A
// row minted before 0076, or any future mint that forgets to stamp the ref,
// carries a nil ref that necessarily normalizes to "github" — there the
// runner_kind signal carries the refusal ALONE. Conversely runner_kind is an
// UN-LOCKED creation-time hint until a signed runner self-report resolves it
// (ADR-045), so a run whose hint still reads github_actions while its ref
// already says gitlab has the refusal carried by the ref signal ALONE. The
// control does not depend on either signal being right, only on either one
// saying GitLab.
//
// The GitHub path is byte-unchanged: a GitHub approval on a run with a
// numeric-or-absent ref and a non-gitlab_ci runner kind still matches.
func forgeMatchesRun(approvalForge string, r *run.Run) bool {
	return normalizeForge(approvalForge) == runForge(r)
}

// runForge derives a run's canonical forge id from the two independent signals
// forgeMatchesRun cross-checks. GitLab wins on EITHER signal: the derivation is
// fail-closed, so an ambiguous run (a gitlab ref with a github_actions hint, or
// a gitlab_ci hint with no ref at all) is treated as GitLab and refuses a
// GitHub approval rather than admitting one.
//
// A nil run yields "github", preserving the legacy default for the
// never-observed nil case rather than panicking inside an HTTP handler.
func runForge(r *run.Run) string {
	if r == nil {
		return "github"
	}
	if r.RunnerKind == run.RunnerKindGitLabCI {
		return webhook.ForgeGitLab
	}
	return runForgeFromRef(r.InstallationRef)
}

// runForgeFromRef reads the forge out of an ADR-057 / ADR-058 installation_ref
// (migration 0076). The canonical GitHub ref is a BARE base-10 decimal
// (forge.FromGitHubInstallationID), so a ref with no "<scheme>:" prefix — and
// equally a nil or empty ref, the pre-0076 and un-stamped shapes — normalizes
// to "github". A ref of the form "<scheme>:<rest>" yields that scheme, which is
// what makes "gitlab:5" a GitLab run.
//
// An EMPTY scheme (a ref that starts with ":") is deliberately NOT treated as
// GitHub-by-default: it is a malformed ref, and returning the empty string
// would let normalizeForge's empty-means-github rule launder it into a match.
// It is returned verbatim so it matches no approval forge and refuses.
func runForgeFromRef(ref *string) string {
	if ref == nil || *ref == "" {
		return "github"
	}
	scheme, _, ok := strings.Cut(*ref, ":")
	if !ok {
		return "github"
	}
	return scheme
}

// namespacedApproverSubject renders the approver identity used for
// authorization, persistence, and audit. A GitHub approval (empty /
// "github" forge) keeps the bare login so nothing about the existing
// GitHub path changes. Any other forge prefixes the login with the
// forge id (e.g. "gitlab:username") so a GitLab user sharing a GitHub
// approver's login cannot string-collide in RoleResolver.CanApprove
// (E45.19 / #2036).
func namespacedApproverSubject(forgeID, login string) string {
	if normalizeForge(forgeID) == "github" {
		return login
	}
	return forgeID + ":" + login
}

// replyScope resolves the notifier scope for an approval reply.
// Mirrors Event.credentialScope(): a non-empty CredentialRef (the
// GitLab path) wraps verbatim via forge.FromRef; otherwise the GitHub
// installation-id wrapper is used, leaving the GitHub reply path
// byte-for-byte unchanged (E45.19 / #2036).
func replyScope(p webhook.ApprovalCommandParams) forge.CredentialScope {
	if p.CredentialRef != "" {
		return forge.FromRef(p.CredentialRef)
	}
	return forge.FromGitHubInstallationID(p.InstallationID)
}

// approvalCommentRefusedReasonChannel is the approval_comment_refused payload
// `reason` for an ADR-123 D4 (#4755) channel refusal: the issue-comment channel
// itself refused the decision, and the payload's `cause` names why.
const approvalCommentRefusedReasonChannel = "comment_channel_refused"

// approvalCommentRefusedReasonOverCap is the approval_comment_refused payload
// `reason` for the over-cap approve-comment refusal (#2583 / E67.24 / #2621).
const approvalCommentRefusedReasonOverCap = "over_cap_approve_comment"

// The channel-refusal causes (ADR-123 D4 / #4755), recorded as the
// approval_comment_refused payload's `cause` and named in the refusal reply.
const (
	// commentRefusalWorkflowV2Gate: the run's cached spec is workflow-v2 (or a
	// later major). The channel cannot evaluate a v2 `approvals` predicate, so
	// it refuses approve AND reject, whether or not the gate declares one.
	commentRefusalWorkflowV2Gate = "workflow_v2_gate"
	// commentRefusalSpecUnreadable: the run's cached spec could not be read —
	// the run read failed, no spec is cached, it does not parse, or the stage's
	// gate cannot be resolved from it.
	commentRefusalSpecUnreadable = "spec_unreadable"
	// commentRefusalResolverUnconfigured: a v0/v1 gate, but no RoleResolver is
	// wired, so the legacy `approvers` check cannot run.
	commentRefusalResolverUnconfigured = "role_resolver_unconfigured"
	// commentRefusalRoleResolutionFailed: the legacy `approvers` check errored
	// (for example a team-API failure).
	commentRefusalRoleResolutionFailed = "role_resolution_failed"
)

// commentApprovalAuthz is authorizeCommentApproval's outcome (ADR-123 D4 /
// #4755). A non-empty refusal means the CHANNEL is refused (one of the
// commentRefusal* causes): nothing is recorded and the refusal is audited.
// Otherwise allowed/denyMsg is the legacy v0/v1 `approvers` verdict, whose deny
// is a reply (or a silent drop) with no audit row. specMajor is the cached
// spec's major when it was read; it is zero for a spec_unreadable refusal
// raised before the spec parsed.
type commentApprovalAuthz struct {
	allowed   bool
	denyMsg   string
	refusal   string
	specMajor int
}

// commentGateSpecMajor re-reads the stage's run and returns the major version
// of its cached workflow spec (ADR-123 D4 / #4755). It errors on a run read
// failure, an empty cached spec (errNoCachedWorkflowSpec) and a spec that does
// not parse. It deliberately requires neither an installation_id nor the run's
// workflow id in the spec: the version decision rests on the spec bytes alone,
// so a v2 spec refuses even where the legacy gate lookup could not run.
// spec.ParseBytes rejects a missing or unsupported version, so a parsed spec's
// major is authoritative.
func (s *Server) commentGateSpecMajor(ctx context.Context, stage *run.Stage) (int, error) {
	runRow, err := s.cfg.RunRepo.GetRun(ctx, stage.RunID)
	if err != nil {
		return 0, fmt.Errorf("get run: %w", err)
	}
	if len(runRow.WorkflowSpec) == 0 {
		return 0, errNoCachedWorkflowSpec
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		return 0, fmt.Errorf("parse workflow spec: %w", err)
	}
	return spec.VersionMajor(parsed.Version), nil
}

// authorizeCommentApproval decides whether an issue-comment decision (slash
// command, reply comment, or forwarded reaction) may reach Submit. It FAILS
// CLOSED (ADR-123 D4 / #4755), replacing the prior best-effort allow, under
// which a spec-read, gate-resolution or role-resolution failure — and every
// workflow-v2 gate, whose `approvals` predicate this channel never evaluated —
// admitted the decision. In order:
//
//  1. the cached spec cannot be read → spec_unreadable;
//  2. the spec is workflow-v2 or later → workflow_v2_gate, for approve AND
//     reject and whether or not the gate declares `approvals`. `>= 2` so a
//     future major refuses too;
//  3. no RoleResolver → role_resolver_unconfigured;
//  4. the stage's gate cannot be resolved (for example a workflow id missing
//     from the spec, or a run without an installation_id) → spec_unreadable;
//  5. a stage with no legacy `approvers` block → allowed (the legacy
//     no-approvers outcome, kept);
//  6. role resolution errors → role_resolution_failed;
//  7. otherwise the legacy `approvers` verdict: allowed, or a deny message.
//
// Only a channel refusal (non-empty refusal) is audited; the legacy deny keeps
// its unaudited reply / silent drop. The MCP, CLI and web-UI channels
// (POST /v0/stages/{id}/approvals) are unaffected.
func (s *Server) authorizeCommentApproval(ctx context.Context, stage *run.Stage, subject string) commentApprovalAuthz {
	specMajor, err := s.commentGateSpecMajor(ctx, stage)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"comment approval refused: cached workflow spec unreadable",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
		return commentApprovalAuthz{refusal: commentRefusalSpecUnreadable}
	}
	if specMajor >= 2 {
		return commentApprovalAuthz{refusal: commentRefusalWorkflowV2Gate, specMajor: specMajor}
	}
	if s.cfg.RoleResolver == nil {
		return commentApprovalAuthz{refusal: commentRefusalResolverUnconfigured, specMajor: specMajor}
	}
	gate, err := s.fetchGateForStage(ctx, stage)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"comment approval refused: fetch gate failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
		return commentApprovalAuthz{refusal: commentRefusalSpecUnreadable, specMajor: specMajor}
	}
	if gate == nil || gate.approvers == nil {
		// No legacy approvers block on the stage — the v0/v1 no-approvers
		// outcome, matching the HTTP handler's posture.
		return commentApprovalAuthz{allowed: true, specMajor: specMajor}
	}
	allowed, err := s.cfg.RoleResolver.CanApprove(ctx, gate.scope, gate.approvers, gate.roles, subject)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"comment approval refused: role resolution failed",
			slog.String("stage_id", stage.ID.String()),
			slog.String("subject", subject),
			slog.String("error", err.Error()))
		return commentApprovalAuthz{refusal: commentRefusalRoleResolutionFailed, specMajor: specMajor}
	}
	if !allowed {
		return commentApprovalAuthz{denyMsg: fmt.Sprintf("Cannot approve: @%s is not in this stage's approvers list.", subject), specMajor: specMajor}
	}
	return commentApprovalAuthz{allowed: allowed, specMajor: specMajor}
}

// commentChannelRefusedReply renders the slash-channel reply for an ADR-123 D4
// (#4755) channel refusal. It names the channels that CAN decide the gate, so
// the reviewer's next action is clear.
func commentChannelRefusedReply(cause, subject string) string {
	channels := "the MCP verbs `fishhawk_approve_plan` / `fishhawk_reject_plan`, the CLI `fishhawk plan approve` / `fishhawk plan reject`, or the web UI"
	if cause == commentRefusalWorkflowV2Gate {
		return fmt.Sprintf("Comment approvals and rejections are not accepted on workflow-v2 gates (ADR-123). Nothing was recorded; the stage is still awaiting approval. Decide the gate with %s. (Caller: @%s)", channels, subject)
	}
	return fmt.Sprintf("Fishhawk could not verify this gate's approval rules (`%s`), so nothing was recorded; the stage is still awaiting approval. Decide the gate with %s. (Caller: @%s)", cause, channels, subject)
}

// writeSlashApprovalAudit mirrors writeApprovalAudit's chain entry
// — same category, same payload shape — so audit consumers don't
// care which surface produced the row.
func (s *Server) writeSlashApprovalAudit(ctx context.Context, stage *run.Stage, app *approval.Approval, comment string) {
	systemKind := audit.ActorKind("user")
	approver := app.ApproverSubject
	auditPayload := map[string]any{
		"stage_id": stage.ID.String(),
		"decision": string(app.Decision),
		"surface":  string(app.Surface),
		"approver": approver,
	}
	if app.Decision == approval.DecisionReject && comment != "" {
		auditPayload["rejection_comment"] = comment
	}
	if app.Decision == approval.DecisionApprove && comment != "" {
		auditPayload["comment"] = comment
	}
	payload, _ := json.Marshal(auditPayload)
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        stage.RunID,
		StageID:      &stage.ID,
		Timestamp:    time.Now().UTC(),
		Category:     "approval_submitted",
		ActorKind:    &systemKind,
		ActorSubject: &approver,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"slash-command approval: audit append failed",
			slog.String("run_id", stage.RunID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
	}
}

// writeApprovalCommentRefusedAudit records a refusal on the issue-comment
// approval channel: the over-cap approve-comment refusal (E67.24 / #2621) and
// the ADR-123 D4 channel refusal (#4755), told apart by `reason`. Modelled on
// writeSlashApprovalAudit's chain entry and appendApprovalConditionsTruncatedAudit's
// best-effort posture: an append failure WARN-logs and never changes the
// handler's return value or its reply behavior — the refusal already happened,
// and unwinding it because a breadcrumb failed would trade a visible drop for an
// invisible admission. The payload is {stage_id, decision, surface, approver,
// silent, reason} plus every key of `extra`.
func (s *Server) writeApprovalCommentRefusedAudit(ctx context.Context, stage *run.Stage, subject string, decision approval.Decision, source webhook.ApprovalSource, silent bool, reason string, extra map[string]any) {
	if s.cfg.AuditRepo == nil {
		// Defence-in-depth: approvalCommandConfigured already requires an
		// AuditRepo, so this branch is unreachable from the handler.
		return
	}
	actorKind := audit.ActorKind("user")
	approver := subject
	auditPayload := map[string]any{
		"stage_id": stage.ID.String(),
		"decision": string(decision),
		"surface":  string(approvalSurfaceForSource(source)),
		"approver": approver,
		"silent":   silent,
		"reason":   reason,
	}
	for k, v := range extra {
		auditPayload[k] = v
	}
	payload, _ := json.Marshal(auditPayload)
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        stage.RunID,
		StageID:      &stage.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryApprovalCommentRefused,
		ActorKind:    &actorKind,
		ActorSubject: &approver,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"slash-command approval: append approval_comment_refused audit failed",
			slog.String("run_id", stage.RunID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.Bool("silent", silent),
			slog.String("error", err.Error()))
	}
}

// overCapRefusalExtra filters validateApprovalComment's details down to the
// byte figures the over-cap approval_comment_refused payload carries, so the
// recorded figures are the exact ones the refusal message names.
func overCapRefusalExtra(details map[string]any) map[string]any {
	extra := map[string]any{}
	for _, k := range []string{"bytes", "max_bytes", "overflow_bytes"} {
		if v, ok := details[k]; ok {
			extra[k] = v
		}
	}
	return extra
}

// replyApproval posts a reply comment to the issue using the
// existing issuecomment notifier. Best-effort: a GitHub failure
// here is logged but doesn't unwind — the approval (or refusal)
// is already recorded server-side.
func (s *Server) replyApproval(ctx context.Context, p webhook.ApprovalCommandParams, body string) {
	if s.issueNotifier == nil {
		return
	}
	if err := s.issueNotifier.NotifySlashApprovalReply(ctx, issuecomment.SlashApprovalReply{
		Repo:        p.Repo,
		Scope:       replyScope(p),
		IssueNumber: p.IssueNumber,
		Body:        body,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"slash-command approval: reply comment failed",
			slog.String("repo", p.Repo),
			slog.Int("issue", p.IssueNumber),
			slog.String("error", err.Error()))
	}
}

// approvalSurfaceForSource maps the dispatcher's ApprovalSource onto
// the approval.Surface stamped on the persisted row. The two paths
// stay distinguishable in the audit log so a post-hoc reviewer can
// tell a deliberate slash command from a reply-pattern "+1".
//
// Empty / unknown sources default to SurfaceGitHubComment to match
// pre-E17.4 behavior — defensive against any future caller that
// fails to set Source.
func approvalSurfaceForSource(src webhook.ApprovalSource) approval.Surface {
	if src == webhook.ApprovalSourceReplyComment {
		return approval.SurfaceGitHubReplyComment
	}
	return approval.SurfaceGitHubComment
}

// decodeMatchAction translates the dispatcher's tagged action into
// an approval.Decision. Returns false for anything that isn't an
// approve / reject — defense in depth against the dispatcher
// passing an unexpected value.
func decodeMatchAction(a webhook.MatchAction) (approval.Decision, bool) {
	switch a {
	case webhook.MatchActionApprove:
		return approval.DecisionApprove, true
	case webhook.MatchActionReject:
		return approval.DecisionReject, true
	}
	return "", false
}

// reviewStageHelpReply is the slash-approval response when the
// reviewer targets a review stage. ADR-018 / #313 moved review-
// stage approval onto GitHub; the help text points at the PR
// (when the run row has one stamped) so the reviewer's next action
// is one click away.
func reviewStageHelpReply(runRow *run.Run, subject string) string {
	if runRow != nil && runRow.PullRequestURL != nil && *runRow.PullRequestURL != "" {
		return fmt.Sprintf("Review-stage approval is recorded from GitHub's PR surface. Approve or merge the PR to advance the stage: %s (caller: @%s)",
			*runRow.PullRequestURL, subject)
	}
	return fmt.Sprintf("Review-stage approval is recorded from GitHub's PR surface — approve or merge the PR to advance the stage. (Caller: @%s)", subject)
}

// formatSuccessReply renders the celebratory reply on approve /
// reject success. Includes the run ID's short prefix so a
// reviewer scrolling old issues sees which run a comment refers
// to without cross-referencing.
func formatSuccessReply(decision approval.Decision, subject string, runID uuid.UUID, stage *run.Stage) string {
	verb := "Approved"
	if decision == approval.DecisionReject {
		verb = "Rejected"
	}
	short := runID.String()
	if len(short) >= 8 {
		short = short[:8]
	}
	return fmt.Sprintf("%s by @%s. Stage `%s` advanced for run `%s`.", verb, subject, stage.Type, short)
}
