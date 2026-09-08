package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	forgegithub "github.com/kuhlman-labs/fishhawk/backend/internal/forge/github"
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
//   - "is the parent already closed?" is answered by FetchIssue, not by a
//     marker of ours.
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
// credential identity comes off the webhook.Event itself — InstallationID (an
// int64 that is ZERO when the event isn't installation-scoped) for GitHub,
// CredentialRef ("gitlab:<project-id>", EMPTY when the payload carried no
// project id) for GitLab — so the pointer never exists on this path. Both
// collapse to the ZERO forge.CredentialScope, which is the one gate.
//
// ORDERING IS LOAD-BEARING, TWICE.
//
//  1. LINKAGE BEFORE EVERY LINKAGE-DEPENDENT OUTCOME. The credential-scope and
//     state_reason gates run AFTER linkage resolves, never before. An unrelated
//     issue in the repo closed as not_planned, or any close lacking credential
//     data, must write ZERO audit entries — an observation about a split it has
//     nothing to do with would be a false record. Nothing forces the other
//     order: the credential scope is needed only for the forge calls, which
//     come last.
//  2. COMMENT FIRST, THEN CLOSE. The CLOSE is what stops future deliveries
//     reaching the forge (a closed parent short-circuits at already_closed), so
//     closing first would make a transient comment failure PERMANENT. With
//     comment-first, a torn delivery leaves the parent OPEN with the comment
//     present, and the next redelivery finds the marker, skips the post, and
//     closes.
//
// NAMED RESIDUAL, ACCEPTED. Two GENUINELY CONCURRENT deliveries for the same
// contract child can interleave between the FetchIssueComments read and the
// PostIssueComment write, both see no marker, and both post — a DUPLICATE
// LINKING COMMENT. The close does not duplicate (the second SetIssueState is a
// no-op close on an already-closed issue). This window is deliberately left
// open: a duplicate comment is cosmetic, and a distributed lock's failure mode
// — a leaked lock wedging every later delivery — is strictly worse. Only the
// SEQUENTIAL-redelivery exactly-once property is claimed, and that is what the
// tests prove.
//
// FORGE-NEUTRAL SINCE E50.17 / #2900. Every forge call goes through the
// standalone forge.IssueOperations capability (FetchIssue / FetchIssueComments
// / PostIssueComment / SetIssueState), resolved per forge FAMILY by
// splitParentIssueOpsFor: a github-family delivery resolves ONLY through
// s.cfg.GitHub, any other family ONLY through s.cfg.ForgeResolver (defaulting
// to forge.Get) plus a type assertion to the capability — so registry
// availability can never change a GitHub outcome. A GitLab issue close arrives
// as object_kind "issue" / action "close" with its credential scope in
// ev.CredentialRef and its issue number at object_attributes.iid; the routing
// predicate isIssueClosedDelivery (webhook.go) admits both vocabularies.
//
// NAMED RESIDUAL, GITLAB. GitLab's issue object has NO state_reason field, so
// the child-not-landed gate below is INERT on GitLab: a GitLab contract-child
// close always proceeds to the parent close. That is not a hole minted here —
// it is exactly the rule the GitHub path already applies to the missing/null
// state_reason form GitHub sends for a plain close — and no GitLab equivalent
// is fabricated (a "closed as duplicate" on GitLab is a label or a quick
// action, not a lifecycle field).

// splitParentClosedCategory is the audit category for the watcher's pure
// OBSERVATION of what one issues.closed delivery did to a split parent. It
// gates nothing — see the file comment.
const splitParentClosedCategory = "split_parent_closed"

// forgeNameGitHub is the explicit Event.Forge value for a GitHub-sourced event.
// webhook.ParseEvent leaves Forge EMPTY on the GitHub path (the legacy
// default), so both the empty string and this value mean GitHub;
// splitParentForgeID normalizes the two. Any other value (e.g.
// webhook.ForgeGitLab) resolves through the forge registry ladder.
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
	// splitParentOutcomeCloseFailed: the comment landed but SetIssueState
	// failed; the next delivery converges.
	splitParentOutcomeCloseFailed = "close_failed"
	// splitParentOutcomeNoInstallation: the event carries no credential
	// identity (GitHub: InstallationID == 0; GitLab: empty CredentialRef), so
	// the forge.CredentialScope is ZERO and no forge call can be made. The
	// name predates GitLab parity and is kept so existing readers of the
	// chain keep matching.
	splitParentOutcomeNoInstallation = "no_installation"
	// splitParentOutcomeAmbiguousLinkage: two or more surviving linkage entries
	// in this repo name the same contract child but DISAGREE on parent_issue.
	// Picking one arbitrarily could close the WRONG parent — an unrecoverable,
	// operator-visible error — so the watcher skips and audits the reason.
	splitParentOutcomeAmbiguousLinkage = "ambiguous_linkage"
)

// splitParentCloseIssuePayload is the subset of the issue-closed webhook body
// this watcher needs, across BOTH forge shapes. GitHub (`issues` event) carries
// `issue.number` + `issue.state_reason`; GitLab (object_kind `issue`) carries
// `object_attributes.iid` and NO state_reason field at all. It mirrors
// issueLifecyclePayload rather than sharing it so the two consumers on the same
// event can evolve independently.
type splitParentCloseIssuePayload struct {
	Issue struct {
		Number      int    `json:"number"`
		StateReason string `json:"state_reason"`
	} `json:"issue"`
	ObjectAttributes struct {
		IID int `json:"iid"`
	} `json:"object_attributes"`
}

// splitParentForgeID normalizes the event's forge family: webhook.ParseEvent
// leaves Forge EMPTY on the GitHub path, so the empty string IS GitHub.
func splitParentForgeID(ev webhook.Event) string {
	if ev.Forge == "" {
		return forgeNameGitHub
	}
	return ev.Forge
}

// splitParentClosedIssue decodes the closed issue's number and (GitHub-only)
// state_reason from the raw body for the given forge family. ok is false for
// an undecodable body, a non-positive number, or a forge family whose payload
// shape this watcher does not know — every one a silent, zero-write skip.
func splitParentClosedIssue(forgeID string, raw []byte) (number int, stateReason string, ok bool) {
	var p splitParentCloseIssuePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0, "", false
	}
	switch forgeID {
	case forgeNameGitHub:
		number, stateReason = p.Issue.Number, p.Issue.StateReason
	case webhook.ForgeGitLab:
		number = p.ObjectAttributes.IID // GitLab carries no state_reason.
	default:
		return 0, "", false
	}
	if number <= 0 {
		return 0, "", false
	}
	return number, stateReason, true
}

// splitParentCredentialScope derives the forge.CredentialScope for the event's
// forge family, forge-neutrally: GitHub from the App installation id, GitLab
// from the "gitlab:<project-id>" ref webhook.ParseGitLabEvent stamped. Both
// yield the ZERO scope when the event carried no identity, which is the single
// gate the caller applies.
func splitParentCredentialScope(forgeID string, ev webhook.Event) forge.CredentialScope {
	switch forgeID {
	case forgeNameGitHub:
		return forge.FromGitHubInstallationID(ev.InstallationID)
	default:
		return forge.FromRef(ev.CredentialRef)
	}
}

// splitParentRepoRef splits a repo full name on its LAST slash. GitLab project
// paths may nest (group/subgroup/project), which the shared splitRepoFullName
// rejects because it forbids a slash in the name half — and that helper has
// other callers whose two-segment GitHub assumption is correct, so it stays
// untouched. Either half empty is unsplittable (ok=false). The GitLab adapter
// addresses projects by the scope's numeric project id and ignores the
// RepoRef entirely, so on that path the split is only cosmetic today.
func splitParentRepoRef(full string) (forge.RepoRef, bool) {
	full = strings.TrimSpace(full)
	i := strings.LastIndex(full, "/")
	if i <= 0 || i == len(full)-1 {
		return forge.RepoRef{}, false
	}
	return forge.RepoRef{Owner: full[:i], Name: full[i+1:]}, true
}

// splitParentIssueOpsFor resolves the forge.IssueOperations for a forge FAMILY,
// mirroring the UNAMBIGUOUS per-family ladder prStateReaderFor codifies for the
// merge-observation verb (E64.40 / #3151):
//
//   - a github-family delivery resolves ONLY through s.cfg.GitHub, wrapped by
//     forgegithub.New. It NEVER falls through to ForgeResolver or the process
//     registry, so registry availability can never change a GitHub outcome. A
//     nil client is a nil result.
//   - any other family resolves through s.cfg.ForgeResolver (defaulting to
//     forge.Get) and a type assertion to forge.IssueOperations. A resolver
//     error, a nil forge (nil interface OR typed-nil pointer — isNilForge), or
//     a forge that does not implement the capability is a nil result, never a
//     fabricated no-op.
//
// A nil result keeps the pre-#2900 posture for the nil-GitHub case: the caller
// logs at INFO and returns WITHOUT an audit observation, because an
// unconfigured forge is a server misconfiguration, not a fact about the split.
func (s *Server) splitParentIssueOpsFor(forgeID string) forge.IssueOperations {
	if forgeID == forgeNameGitHub {
		if s.cfg.GitHub == nil {
			return nil
		}
		return forgegithub.New(s.cfg.GitHub)
	}
	resolver := s.cfg.ForgeResolver
	if resolver == nil {
		resolver = forge.Get
	}
	f, err := resolver(forgeID)
	if err != nil || isNilForge(f) {
		return nil
	}
	ops, ok := f.(forge.IssueOperations)
	if !ok {
		return nil
	}
	return ops
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

// handleContractChildClosed is the issue-closed consumer that closes a split
// PARENT when its contract-phase child lands (#2062). It is best-effort: every
// exit is a silent return, a WARN log, or an audited defined skip — never a
// panic and never a 5xx. See the file comment for the design and its residual.
func (s *Server) handleContractChildClosed(ctx context.Context, ev webhook.Event) {
	if s.cfg.AuditRepo == nil {
		return // no audit store: linkage is unreadable, so there is nothing to do.
	}
	forgeID := splitParentForgeID(ev)
	// Explicit empty-repo skip: the linkage filter compares against ev.Repo, and
	// a legacy entry carries an empty parent_repo. Without this, the legacy
	// no-op would rest on an empty-vs-empty comparison FAILING rather than on a
	// stated rule.
	if ev.Repo == "" {
		return
	}

	closedNumber, stateReason, ok := splitParentClosedIssue(forgeID, ev.RawBody)
	if !ok {
		return // malformed payload, no issue number, or unknown forge shape: nothing to link.
	}

	// LINKAGE FIRST (see the file comment): a pure payload read needing no
	// credential and no forge call. Every outcome below this point is ABOUT a
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

	// From here the credential identity is needed, because everything left is
	// a forge call. ZERO-SCOPE GATE, forge-neutral: GitHub's InstallationID == 0
	// and GitLab's empty CredentialRef both land here.
	scope := splitParentCredentialScope(forgeID, ev)
	if scope.IsZero() {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "split parent close: event carries no credential scope; skipping",
			slog.String("forge", forgeID),
			slog.String("repo", ev.Repo),
			slog.Int("parent_issue", parentIssue),
			slog.Int("contract_child", closedNumber))
		s.recordSplitParentClose(ctx, ev.Repo, parentIssue, closedNumber, stateReason,
			splitParentOutcomeNoInstallation, false, nil)
		return
	}
	ops := s.splitParentIssueOpsFor(forgeID)
	if ops == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "split parent close: no issue-operations forge configured for family; skipping",
			slog.String("forge", forgeID),
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
	// close, PROCEEDS. On GitLab the value is ALWAYS empty (no such field), so
	// this gate is inert there — the named residual in the file comment.
	if stateReason == issueStateReasonNotPlanned || stateReason == issueStateReasonDuplicate {
		s.recordSplitParentClose(ctx, ev.Repo, parentIssue, closedNumber, stateReason,
			splitParentOutcomeChildNotLanded, false, nil)
		return
	}

	repo, ok := splitParentRepoRef(ev.Repo)
	if !ok {
		return // unsplittable repo full name: no forge target.
	}

	// (a) Is the parent already closed? The forge answers, not a marker of ours.
	issue, err := ops.FetchIssue(ctx, scope, repo, parentIssue)
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
	comments, err := ops.FetchIssueComments(ctx, scope, repo, parentIssue)
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
		if err := ops.PostIssueComment(ctx, scope, repo, parentIssue, body); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "split parent close: post linking comment failed; not closing",
				slog.String("repo", ev.Repo),
				slog.Int("parent_issue", parentIssue),
				slog.String("error", err.Error()))
			return // NEVER close after a failed comment.
		}
		commented = true
	}

	// StateReason is BEST-EFFORT per the capability contract: GitHub transmits
	// it, GitLab records the close and drops it.
	closedState := "closed"
	closedReason := "completed"
	if err := ops.SetIssueState(ctx, scope, repo, parentIssue, forge.IssueStateUpdate{
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
