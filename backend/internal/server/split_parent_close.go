package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/splitfiling"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// This file is the CLOSE-PARENT-WHEN-CONTRACT-CHILD-LANDS watcher (E50.6 /
// #2062): when the contract-phase child of a filed split proposal is closed as
// landed, the parent issue is linked and closed automatically, retiring the
// manual step the #2057 acceptance-carrier comment currently asks the operator
// to perform.
//
// THE SHAPE, and why it is this one. The FORGE IS THE SOLE AUTHORITY for every
// decision with a side effect:
//
//   - "is the parent already closed?" is answered by GetIssue, not by a marker
//     of ours.
//   - "has the linking comment already been posted?" is answered by re-reading
//     the parent thread for a stamped splitfiling marker, not by a marker of
//     ours.
//
// Consequently there is NO lock, NO cross-run fold, and NO split_parent_closed
// idempotency record. The audit entry this file writes is a pure OBSERVATION —
// nothing anywhere reads it to decide whether to act. An append failure changes
// nothing.
//
// LINKAGE IS A PURE PAYLOAD READ. The split_children_filed completion marker
// carries parent_repo + parent_issue + contract_child_number (#2062 widened it
// additively), so resolving "which parent does this closed issue belong to?"
// needs NO run and NO forge call. That is what structurally eliminates the
// *run.InstallationID nil-deref class rather than merely guarding it: the
// installation identity comes off webhook.Event.InstallationID, an int64 that
// is ZERO when the event isn't installation-scoped, so the pointer never exists
// on this path.
//
// ORDERING IS LOAD-BEARING, TWICE.
//
//  1. LINKAGE BEFORE EVERY LINKAGE-DEPENDENT OUTCOME. The installation and
//     state_reason gates run AFTER linkage resolves, never before. An unrelated
//     issue in the repo closed as not_planned, or any close lacking installation
//     data, must write ZERO audit entries — an observation about a split it has
//     nothing to do with would be a false record. Nothing forces the other
//     order: the installation id is needed only for the forge calls, which come
//     last.
//  2. COMMENT FIRST, THEN CLOSE. The CLOSE is what stops future deliveries
//     reaching the forge (a closed parent short-circuits at already_closed), so
//     closing first would make a transient comment failure PERMANENT. With
//     comment-first, a torn delivery leaves the parent OPEN with the comment
//     present, and the next redelivery finds the marker, skips the post, and
//     closes.
//
// NAMED RESIDUAL, ACCEPTED. Two GENUINELY CONCURRENT deliveries for the same
// contract child can interleave between the ListIssueComments read and the
// CreateIssueComment write, both see no marker, and both post — a DUPLICATE
// LINKING COMMENT. The close does not duplicate (the second UpdateIssue is a
// no-op close on an already-closed issue). This window is deliberately left
// open: a duplicate comment is cosmetic, and a distributed lock's failure mode
// — a leaked lock wedging every later delivery — is strictly worse. Only the
// SEQUENTIAL-redelivery exactly-once property is claimed, and that is what the
// tests prove.
//
// GITLAB PARITY IS DEFERRED to #2900: s.cfg.GitHub is a *githubclient.Client
// and cannot serve a GitLab project, so a non-GitHub event is a defined skip.

// splitParentClosedCategory is the audit category for the watcher's pure
// OBSERVATION of what one issues.closed delivery did to a split parent. It
// gates nothing — see the file comment.
const splitParentClosedCategory = "split_parent_closed"

// forgeNameGitHub is the explicit Event.Forge value for a GitHub-sourced event.
// webhook.ParseEvent leaves Forge EMPTY on the GitHub path (the legacy
// default), so both the empty string and this value mean GitHub; anything else
// (e.g. webhook.ForgeGitLab) is a defined skip.
const forgeNameGitHub = "github"

// Outcomes recorded on a split_parent_closed observation. Each names exactly
// what the delivery did, so an operator reading the global chain can tell a
// landed auto-close from every defined skip without re-deriving anything.
const (
	// splitParentOutcomeClosed: the parent was linked and closed.
	splitParentOutcomeClosed = "closed"
	// splitParentOutcomeAlreadyClosed: the forge reported the parent already
	// closed, so nothing was posted and nothing was patched.
	splitParentOutcomeAlreadyClosed = "already_closed"
	// splitParentOutcomeChildNotLanded: the contract child was closed as
	// not_planned/duplicate, so it did NOT land and the parent is left open.
	splitParentOutcomeChildNotLanded = "child_not_landed"
	// splitParentOutcomeCloseFailed: the comment landed but UpdateIssue failed;
	// the next delivery converges.
	splitParentOutcomeCloseFailed = "close_failed"
	// splitParentOutcomeNoInstallation: the event carries no installation, so
	// no credential scope can be built for the forge calls.
	splitParentOutcomeNoInstallation = "no_installation"
	// splitParentOutcomeAmbiguousLinkage: two or more surviving linkage entries
	// in this repo name the same contract child but DISAGREE on parent_issue.
	// Picking one arbitrarily could close the WRONG parent — an unrecoverable,
	// operator-visible error — so the watcher skips and audits the reason.
	splitParentOutcomeAmbiguousLinkage = "ambiguous_linkage"
)

// splitParentCloseIssuePayload is the subset of the `issues` webhook body this
// watcher needs. It mirrors issueLifecyclePayload rather than sharing it so the
// two consumers on the same event can evolve independently.
type splitParentCloseIssuePayload struct {
	Issue struct {
		Number      int    `json:"number"`
		StateReason string `json:"state_reason"`
	} `json:"issue"`
}

// splitParentLinkage is the resolved outcome of the pure-payload linkage read.
type splitParentLinkage struct {
	// parentIssue is the agreed parent issue number (valid only when found).
	parentIssue int
	// found reports whether exactly one parent_issue value survived.
	found bool
	// ambiguous reports that two or more surviving matches disagreed on
	// parent_issue. candidates names them for the audit.
	ambiguous  bool
	candidates []int
	// readErr reports that the audit read itself failed.
	readErr error
}

// handleContractChildClosed is the `issues.closed` consumer that closes a split
// PARENT when its contract-phase child lands (#2062). It is best-effort: every
// exit is a silent return, a WARN log, or an audited defined skip — never a
// panic and never a 5xx. See the file comment for the design and its residual.
func (s *Server) handleContractChildClosed(ctx context.Context, ev webhook.Event) {
	if s.cfg.AuditRepo == nil {
		return // no audit store: linkage is unreadable, so there is nothing to do.
	}
	// GitLab parity is deferred to #2900 — s.cfg.GitHub cannot serve a GitLab
	// project, so a non-GitHub event is a defined skip BEFORE any work.
	if ev.Forge != "" && ev.Forge != forgeNameGitHub {
		return
	}
	// Explicit empty-repo skip: the linkage filter compares against ev.Repo, and
	// a legacy entry carries an empty parent_repo. Without this, the legacy
	// no-op would rest on an empty-vs-empty comparison FAILING rather than on a
	// stated rule.
	if ev.Repo == "" {
		return
	}

	var p splitParentCloseIssuePayload
	if err := json.Unmarshal(ev.RawBody, &p); err != nil || p.Issue.Number <= 0 {
		return // malformed payload or no issue number: nothing to link.
	}
	closedNumber := p.Issue.Number
	stateReason := p.Issue.StateReason

	// LINKAGE FIRST (see the file comment): a pure payload read needing no
	// installation and no forge call. Every outcome below this point is ABOUT a
	// real split; an unrelated issue closing exits here having written nothing.
	link := s.resolveSplitParentLinkage(ctx, ev.Repo, closedNumber)
	switch {
	case link.readErr != nil:
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "split parent close: list split_children_filed failed",
			slog.String("repo", ev.Repo),
			slog.Int("closed_issue", closedNumber),
			slog.String("error", link.readErr.Error()))
		return // changes nothing.
	case link.ambiguous:
		// Issue-number uniqueness proves the CHILD is unique; it does not prove
		// two payloads agree on the PARENT. Skip rather than close one at random.
		s.recordSplitParentClose(ctx, ev.Repo, 0, closedNumber, stateReason,
			splitParentOutcomeAmbiguousLinkage, false, link.candidates)
		return
	case !link.found:
		return // not a contract child of any filed split: silent, zero writes.
	}
	parentIssue := link.parentIssue

	// From here the installation identity is needed, because everything left is
	// a forge call.
	if ev.InstallationID == 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "split parent close: event carries no installation; skipping",
			slog.String("repo", ev.Repo),
			slog.Int("parent_issue", parentIssue),
			slog.Int("contract_child", closedNumber))
		s.recordSplitParentClose(ctx, ev.Repo, parentIssue, closedNumber, stateReason,
			splitParentOutcomeNoInstallation, false, nil)
		return
	}
	if s.cfg.GitHub == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "split parent close: no GitHub client configured; skipping",
			slog.String("repo", ev.Repo),
			slog.Int("parent_issue", parentIssue),
			slog.Int("contract_child", closedNumber))
		return // server misconfiguration, not a fact about this split: no audit.
	}

	// STATE_REASON, decided explicitly. The linking comment asserts the parent is
	// closed BECAUSE the contract child LANDED. A child closed as
	// not_planned/duplicate did not land, so closing the parent would falsely
	// assert completion — the same disposition the sibling #1817 board-sync
	// reconciler takes on the same event. Every other value, including
	// "completed" and the missing/null form GitHub sends routinely for a plain
	// close, PROCEEDS.
	if stateReason == issueStateReasonNotPlanned || stateReason == issueStateReasonDuplicate {
		s.recordSplitParentClose(ctx, ev.Repo, parentIssue, closedNumber, stateReason,
			splitParentOutcomeChildNotLanded, false, nil)
		return
	}

	owner, name, ok := splitRepoFullName(ev.Repo)
	if !ok {
		return // unsplittable repo full name: no forge target.
	}
	scope := forge.FromGitHubInstallationID(ev.InstallationID)
	repo := forge.RepoRef{Owner: owner, Name: name}

	// (a) Is the parent already closed? The forge answers, not a marker of ours.
	issue, err := s.cfg.GitHub.GetIssue(ctx, scope, repo, parentIssue)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "split parent close: get parent issue failed",
			slog.String("repo", ev.Repo),
			slog.Int("parent_issue", parentIssue),
			slog.String("error", err.Error()))
		return // transient: the next delivery, or the operator, retries.
	}
	if issue != nil && issue.State == "closed" {
		s.recordSplitParentClose(ctx, ev.Repo, parentIssue, closedNumber, stateReason,
			splitParentOutcomeAlreadyClosed, false, nil)
		return
	}

	// (b) Has the linking comment already been posted? Again the forge answers.
	comments, err := s.cfg.GitHub.ListIssueComments(ctx, scope, repo, parentIssue)
	if err != nil {
		// Deliberately fail-CLOSED, the OPPOSITE of splitParentThreadHasComment's
		// documented fail-OPEN posture in split_filing.go. There, a missing
		// operator-facing comment was worse than a duplicate. Here this read IS
		// the entire idempotency record, so posting blind would duplicate the
		// comment on every redelivery. The parent stays open; the next delivery
		// retries. Do NOT "fix" one to match the other.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "split parent close: list parent comments failed; not posting",
			slog.String("repo", ev.Repo),
			slog.Int("parent_issue", parentIssue),
			slog.String("error", err.Error()))
		return
	}
	bodies := make([]string, 0, len(comments))
	for _, c := range comments {
		bodies = append(bodies, c.Body)
	}
	key := splitfiling.ParentCloseCommentKey(ev.Repo, parentIssue, closedNumber)

	// COMMENT FIRST, THEN CLOSE — see the file comment for why the inverse would
	// make a transient comment failure permanent.
	commented := false
	if !splitfiling.ThreadHasComment(bodies, key) {
		body := splitfiling.StampComment(splitParentCloseCommentBody(parentIssue, closedNumber), key)
		if _, err := s.cfg.GitHub.CreateIssueComment(ctx, scope, repo, parentIssue, body); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "split parent close: post linking comment failed; not closing",
				slog.String("repo", ev.Repo),
				slog.Int("parent_issue", parentIssue),
				slog.String("error", err.Error()))
			return // NEVER close after a failed comment.
		}
		commented = true
	}

	closedState := "closed"
	closedReason := "completed"
	if _, err := s.cfg.GitHub.UpdateIssue(ctx, scope, repo, parentIssue, githubclient.UpdateIssueParams{
		State:       &closedState,
		StateReason: &closedReason,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "split parent close: close parent failed",
			slog.String("repo", ev.Repo),
			slog.Int("parent_issue", parentIssue),
			slog.String("error", err.Error()))
		s.recordSplitParentClose(ctx, ev.Repo, parentIssue, closedNumber, stateReason,
			splitParentOutcomeCloseFailed, commented, nil)
		return
	}
	s.recordSplitParentClose(ctx, ev.Repo, parentIssue, closedNumber, stateReason,
		splitParentOutcomeClosed, commented, nil)
}

// resolveSplitParentLinkage answers "is closedNumber the contract child of a
// split filed in repoFullName, and which parent does it belong to?" as a PURE
// AUDIT-PAYLOAD READ: no run resolution, no installation, no forge call.
//
// A split_children_filed entry survives the filter only when ALL of these hold:
//
//   - parent_repo equals the closed issue's repo. LOAD-BEARING: issue numbers
//     are PER-REPO, so matching on number alone would let an unrelated repo's
//     issue #N trigger (or, via the ambiguity rule below, suppress) a close in
//     this one.
//   - contract_child_number equals the closed issue's number.
//   - parent_issue is > 0 (a legacy pre-#2062 entry carries neither field and so
//     matches nothing — the correct fail-quiet direction).
//   - parent_issue != the closed number: a cheap self-close guard with defined
//     behavior against a corrupt entry.
//
// When the survivors DISAGREE on parent_issue the result is AMBIGUOUS, not a
// pick: a same-repo, same-child pair naming different parents cannot both be
// right, and closing the wrong parent is unrecoverable and operator-visible. A
// defined skip with an audited reason beats an arbitrary choice.
func (s *Server) resolveSplitParentLinkage(ctx context.Context, repoFullName string, closedNumber int) splitParentLinkage {
	cat := splitChildrenFiledCategory
	entries, err := s.cfg.AuditRepo.ListAll(ctx, audit.ListAllParams{Category: &cat})
	if err != nil {
		return splitParentLinkage{readErr: err}
	}
	var candidates []int
	seen := map[int]bool{}
	for _, e := range entries {
		if e == nil {
			continue
		}
		var p splitChildrenFiledPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			continue // undecodable entry: skip, never abort the whole read.
		}
		if p.ParentRepo != repoFullName || p.ContractChildNumber != closedNumber {
			continue
		}
		if p.ParentIssue <= 0 || p.ParentIssue == closedNumber {
			continue
		}
		if seen[p.ParentIssue] {
			continue
		}
		seen[p.ParentIssue] = true
		candidates = append(candidates, p.ParentIssue)
	}
	switch len(candidates) {
	case 0:
		return splitParentLinkage{}
	case 1:
		return splitParentLinkage{parentIssue: candidates[0], found: true}
	default:
		return splitParentLinkage{ambiguous: true, candidates: candidates}
	}
}

// splitParentCloseCommentBody renders the linking comment posted on the parent
// immediately BEFORE it is closed. It names the landed contract child, states
// why the parent is being closed, and links back — so the close is never a bare
// state change an operator has to reverse-engineer.
func splitParentCloseCommentBody(parentIssue, contractChildNumber int) string {
	return fmt.Sprintf(
		"Closing this issue: the contract-phase child #%d of its approved split proposal has landed.\n\n"+
			"That child carried this issue's acceptance criteria, so its landing is what completes #%d. "+
			"Fishhawk posted this comment before closing so the reason travels with the issue (E50.6 / #2062).\n\n"+
			"Reopen this issue if the contract child was closed for some other reason — nothing here is irreversible.",
		contractChildNumber, parentIssue)
}

// recordSplitParentClose appends the watcher's OBSERVATION to the GLOBAL audit
// chain (a webhook-driven issue close has no run and no request Identity, so
// the untenanted partition is correct — matching auditIssueBoardTransition).
//
// It is BEST-EFFORT and always runs AFTER the forge writes: this entry is a
// record of what happened, NEVER a gate on what happens next. Nothing anywhere
// reads it to decide whether to act, so an append failure logs and changes
// nothing.
func (s *Server) recordSplitParentClose(ctx context.Context, repoFullName string, parentIssue, contractChild int, stateReason, outcome string, commented bool, candidates []int) {
	if s.cfg.AuditRepo == nil {
		return
	}
	fields := map[string]any{
		"parent_repo":    repoFullName,
		"parent_issue":   parentIssue,
		"contract_child": contractChild,
		"state_reason":   stateReason,
		"outcome":        outcome,
		"commented":      commented,
	}
	if len(candidates) > 0 {
		fields["parent_candidates"] = candidates
	}
	payload, _ := json.Marshal(fields)
	systemKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(),
		Category:  splitParentClosedCategory,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "split parent close: append observation failed",
			slog.String("repo", repoFullName),
			slog.Int("parent_issue", parentIssue),
			slog.String("outcome", outcome),
			slog.String("error", err.Error()))
	}
}
