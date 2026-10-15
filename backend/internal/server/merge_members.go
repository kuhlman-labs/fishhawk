package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryMergeSettleMembersRefused records that a merge on a review gate
// declaring approvals.members was HELD because no accountable forge identity
// is listed (E39.17 / #4132). The review stage stays parked at
// awaiting_approval and no pr_merged row is written. INTERNAL: deliberately
// not an issue-comment activity category.
const CategoryMergeSettleMembersRefused = "merge_settle_members_refused"

// mergeSettleMembersActor is the ActorSubject of a refusal row.
const mergeSettleMembersActor = "merge-settle-members"

// The three hold reasons a refusal row's payload.reason carries.
const (
	// membersHoldUnevaluable: the run's cached spec cannot be read (a parse
	// failure, or the run's workflow is missing from it), so whether the gate
	// declares members is unknown.
	membersHoldUnevaluable = "members_unevaluable"
	// membersHoldIdentityUnavailable: the merger leg or the GitHub approver
	// leg could not be read, so a listed identity might be unseen.
	membersHoldIdentityUnavailable = "identity_unavailable"
	// membersHoldNoListedIdentity: every leg was read and no identity is
	// listed.
	membersHoldNoListedIdentity = "no_listed_identity"
)

// Merger provenance values recorded on a refusal row.
const (
	mergerSourceForge   = "forge"
	mergerSourceWebhook = "webhook"
)

// mergeSettledMembersAdmit is the ONE members check for a review gate that
// settles on PR merge (E39.17 / #4132). resolveReviewStageOnMerge calls it in
// its merged arm, after the ADR-036 implement-review hold and before the
// pr_merged row, so all three callers (the GitHub pull_request webhook, the
// merge-status reconciler poll and the GitLab merge_request webhook) share it.
//
// It returns true (admit, nothing written) when:
//   - the review stage is already terminal;
//   - the run has no cached spec, or its workflow has no stage of the review
//     type — neither can carry a members gate — with no forge call;
//   - the gate declares no (or empty) members, with no forge call;
//   - any accountable forge identity is listed. The identities are the merger
//     read from the FORGE (GitHub merged_by, GitLab merge_user then
//     merged_by) — or, ONLY when the forge yields none, the caller's
//     user-kind webhook actor — plus, on GitHub, every reviewer whose latest
//     non-COMMENTED/PENDING review is APPROVED. Each is qualified
//     github:<login> / gitlab:<username> and matched byte-exact by
//     identity.SubjectListed.
//
// Otherwise it HOLDS: it returns false and appends a deduplicated
// merge_settle_members_refused row naming the reason, so the check never
// silently succeeds. The reconciler re-polls a parked review stage every tick,
// so a later listed approval or a recovered forge read resolves it.
func (s *Server) mergeSettledMembersAdmit(ctx context.Context, target *run.Run, reviewStage *run.Stage, meta reviewMergeMeta) bool {
	if reviewStage.State.IsTerminal() {
		return true
	}
	forgeID := observationForgeID(target.InstallationRef)
	approvals, err := approvalsForRun(target, reviewStage.Type)
	if err != nil {
		if errors.Is(err, errNoCachedWorkflowSpec) || stageTypeMissing(err) {
			return true
		}
		s.holdMergeSettleMembers(ctx, target, reviewStage, meta, membersHold{
			reason: membersHoldUnevaluable,
			forge:  forgeID,
			detail: err.Error(),
		})
		return false
	}
	if approvals == nil || len(approvals.Members) == 0 {
		return true
	}
	members := approvals.Members

	var details []string
	merger, mergerSource := "", ""
	pr, readErr := s.readMergeSettledPR(ctx, target)
	if readErr == nil && pr != nil {
		merger = pr.MergedBy
	}
	if readErr != nil {
		details = append(details, "pull request read: "+readErr.Error())
	}
	if merger != "" {
		mergerSource = mergerSourceForge
	}
	if merger == "" && meta.actorKind == audit.ActorKind("user") {
		merger = meta.actorLogin
		mergerSource = mergerSourceWebhook
	}
	if merger == "" {
		mergerSource = ""
	}

	approverUnavailable := false
	approvers := []string{}
	if forgeID == observationForgeGitHub {
		reviews, lerr := s.listMergeSettledReviews(ctx, target)
		if lerr != nil {
			approverUnavailable = true
			details = append(details, "reviews read: "+lerr.Error())
		} else {
			for _, login := range approvingReviewers(reviews) {
				approvers = append(approvers, identity.CanonicalMember(login, forgeID))
			}
		}
	}

	qualifiedMerger := identity.CanonicalMember(merger, forgeID)
	identities := make([]string, 0, len(approvers)+1)
	if qualifiedMerger != "" {
		identities = append(identities, qualifiedMerger)
	}
	identities = append(identities, approvers...)
	if listedIdentity(members, identities, forgeID) != "" {
		return true
	}

	reason := membersHoldNoListedIdentity
	if merger == "" || approverUnavailable {
		reason = membersHoldIdentityUnavailable
	}
	s.holdMergeSettleMembers(ctx, target, reviewStage, meta, membersHold{
		reason:       reason,
		forge:        forgeID,
		members:      members,
		merger:       qualifiedMerger,
		mergerSource: mergerSource,
		approvers:    approvers,
		detail:       strings.Join(details, "; "),
	})
	return false
}

// readMergeSettledPR reads the run's pull/merge request through the server's
// per-family PR-read ladder — the one the merge-observation verb uses and the
// reconciler mirrors: resolveObservationTarget (a malformed or mismatched
// target is an error), prStateReaderFor (cfg.PRStateReader override; cfg.GitHub
// only for github; cfg.ForgeResolver / forge.Get for any other family), then
// GetPullRequest under mergeObservationScope.
func (s *Server) readMergeSettledPR(ctx context.Context, target *run.Run) (*forge.PullRequest, error) {
	forgeID, repo, number, reason := resolveObservationTarget(target)
	if reason != obsTargetOK {
		return nil, fmt.Errorf("the run's %s pull request target is malformed or mismatched", forgeID)
	}
	reader, err := s.prStateReaderFor(forgeID)
	if err != nil {
		return nil, fmt.Errorf("resolve %s pull request reader: %w", forgeID, err)
	}
	if reader == nil {
		return nil, fmt.Errorf("no %s pull request reader is wired", forgeID)
	}
	return reader.GetPullRequest(ctx, mergeObservationScope(target), repo, number)
}

// listMergeSettledReviews lists a GitHub run's pull request reviews. An unwired
// client, an unresolvable target, a read error or a truncated listing is an
// error: the approver leg is then unavailable.
func (s *Server) listMergeSettledReviews(ctx context.Context, target *run.Run) ([]githubclient.PullRequestReview, error) {
	if s.cfg.GitHub == nil {
		return nil, errors.New("no GitHub client is wired")
	}
	_, repo, number, reason := resolveObservationTarget(target)
	if reason != obsTargetOK {
		return nil, errors.New("the run's github pull request target is malformed or mismatched")
	}
	reviews, truncated, err := s.cfg.GitHub.ListPullRequestReviews(ctx, mergeObservationScope(target),
		repo, number)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, errors.New("the reviews listing was truncated")
	}
	return reviews, nil
}

// approvingReviewers folds a chronological reviews listing to each reviewer's
// LATEST review, ignoring COMMENTED and PENDING (neither grants nor withdraws
// an approval) — merge_readiness.go's latestReviewVerdict fold — and returns
// the logins whose latest review is APPROVED, in first-seen order. An empty
// login (a deleted account) is skipped.
func approvingReviewers(reviews []githubclient.PullRequestReview) []string {
	latest := map[string]string{}
	var order []string
	for _, rv := range reviews {
		switch rv.State {
		case "COMMENTED", "PENDING":
			continue
		}
		if rv.UserLogin == "" {
			continue
		}
		if _, seen := latest[rv.UserLogin]; !seen {
			order = append(order, rv.UserLogin)
		}
		latest[rv.UserLogin] = rv.State
	}
	out := []string{}
	for _, login := range order {
		if latest[login] == "APPROVED" {
			out = append(out, login)
		}
	}
	return out
}

// listedIdentity returns the first identity on the members allow-list, or ""
// when none is listed.
func listedIdentity(members, identities []string, forgeID string) string {
	for _, id := range identities {
		if identity.SubjectListed(members, id, forgeID) {
			return id
		}
	}
	return ""
}

// membersHold is what a refusal row records.
type membersHold struct {
	reason       string
	forge        string
	members      []string
	merger       string
	mergerSource string
	approvers    []string
	detail       string
}

// fingerprint identifies a hold's identity set: the reason, the merger and the
// sorted approvers. A re-poll that sees the same set appends no new row.
func (h membersHold) fingerprint() string {
	sorted := append([]string(nil), h.approvers...)
	sort.Strings(sorted)
	return h.reason + "|" + h.merger + "|" + strings.Join(sorted, ",")
}

// holdMergeSettleMembers logs a hold and appends a merge_settle_members_refused
// row, skipping the append when a row for the same stage already carries the
// same fingerprint (the reconciler re-polls a parked stage every tick). A list
// error appends anyway — a duplicate is preferred to a silent hold — and an
// append error logs and the merge still holds.
func (s *Server) holdMergeSettleMembers(ctx context.Context, target *run.Run, reviewStage *run.Stage, meta reviewMergeMeta, h membersHold) {
	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
		"pull_request merged: held by approvals.members; review stage left parked",
		slog.String("run_id", target.ID.String()),
		slog.String("stage_id", reviewStage.ID.String()),
		slog.String("pr_url", meta.prURL),
		slog.String("reason", h.reason),
	)
	fp := h.fingerprint()
	stageIDStr := reviewStage.ID.String()
	if prior, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, target.ID, CategoryMergeSettleMembersRefused); err == nil {
		for _, e := range prior {
			var p struct {
				StageID     string `json:"stage_id"`
				Fingerprint string `json:"fingerprint"`
			}
			if json.Unmarshal(e.Payload, &p) == nil && p.StageID == stageIDStr && p.Fingerprint == fp {
				return
			}
		}
	}
	members := h.members
	if members == nil {
		members = []string{}
	}
	approvers := h.approvers
	if approvers == nil {
		approvers = []string{}
	}
	payload, _ := json.Marshal(map[string]any{
		"stage_id":      stageIDStr,
		"pr_url":        meta.prURL,
		"forge":         h.forge,
		"members":       members,
		"merger":        h.merger,
		"merger_source": h.mergerSource,
		"approvers":     approvers,
		"reason":        h.reason,
		"detail":        h.detail,
		"fingerprint":   fp,
	})
	kind := audit.ActorSystem
	subject := mergeSettleMembersActor
	stageID := reviewStage.ID
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        target.ID,
		StageID:      &stageID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryMergeSettleMembersRefused,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError,
			"pull_request merged: merge_settle_members_refused audit append failed",
			slog.String("run_id", target.ID.String()),
			slog.String("error", err.Error()))
	}
}
