package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	forgegithub "github.com/kuhlman-labs/fishhawk/backend/internal/forge/github"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// RunBranchesSweptCategory is the audit-log category for the single chained
// entry a terminal run-branch sweep appends (E68.67 / #3562). It names every
// Fishhawk-owned ref the sweep deleted, skipped, found already absent, or
// failed on, so "what was deleted" is answerable from the chain without a
// forge read. Registered in audit.KnownCategories.
const RunBranchesSweptCategory = "run_branches_swept"

// Sweep triggers recorded on the run_branches_swept payload.
const (
	sweepTriggerPRMerged  = "pr_merged"
	sweepTriggerCancelled = "cancelled"
)

// runBranchNamespacePrefix is the namespace every sweepable ref must carry.
// The sweep is a DESTRUCTIVE primitive, so a candidate outside it — the repo
// default branch, a release branch, a human's branch — is dropped before any
// forge call, whatever the derivation produced.
const runBranchNamespacePrefix = "fishhawk/run-"

// runBranchSweepBudget bounds the detached cancel-path sweep (approval
// condition 3 of #3562): the cancel response never waits on it, and a wedged
// forge cannot hold the goroutine past this budget.
const runBranchSweepBudget = 2 * time.Minute

// Skip reasons on the run_branches_swept payload's skipped_open_pr entries.
const (
	sweepSkipOpenPRHead = "open_pr_head" // the branch is the HEAD of an open PR
	sweepSkipOpenPRBase = "open_pr_base" // the branch is the BASE of an open PR
)

// sweepErrorMaxLen caps each recorded error message so a forge error body
// cannot bloat the audit row.
const sweepErrorMaxLen = 300

// runBranchForge is what the sweep needs from a forge: the ref delete plus
// the open-PR read the preservation gates consult. Both registered forge
// adapters satisfy it; a forge that does not is a nil resolution (no sweep),
// never a delete without the PR gates.
type runBranchForge interface {
	forge.RefDeleter
	ListOpenPullRequestsByHead(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, headBranch, base string) ([]forge.PullRequest, error)
}

// branchExistenceProber is the optional existence probe (forge.Forge's
// GetBranchSHA). RefDeleter maps an absent branch to nil, so without the
// probe a delete of an absent ref is indistinguishable from a real deletion;
// with it the sweep classifies the ref already_absent and skips the PR reads.
type branchExistenceProber interface {
	GetBranchSHA(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, branch string) (string, bool, error)
}

// runSweepCandidates derives the run's Fishhawk-owned branch set from the
// SINGLE sources of truth — orchestrator.ConsolidatedBranch /
// orchestrator.SliceBranch and the per-stage form fixupBranchFor uses — never
// by listing refs and never by re-hardcoding the literals (#1245). Pure:
// deduplicated, sorted, and namespace-guarded.
//
//   - A decomposed CHILD contributes nothing: its slice branch is derived from
//     the PARENT's id and belongs to the parent's fan-in, so sweeping on the
//     child's own terminal state would delete a branch the parent still needs.
//   - Every other run contributes its per-stage branches
//     fishhawk/run-<short(run)>/stage-<short(stage)>.
//   - A decomposed PARENT (decomposedParent) additionally contributes its
//     consolidated branch and one slice branch per child (SliceIndex, default
//     0 — fixupBranchFor's runner-matching default).
//
// Every candidate not under runBranchNamespacePrefix is dropped.
func runSweepCandidates(runRow *run.Run, stages []*run.Stage, children []*run.Run, decomposedParent bool) []string {
	if runRow == nil || runRow.DecomposedFrom != nil {
		return nil
	}
	set := map[string]struct{}{}
	for _, st := range stages {
		if st == nil {
			continue
		}
		set[fmt.Sprintf("fishhawk/run-%s/stage-%s", shortID(runRow.ID), shortID(st.ID))] = struct{}{}
	}
	if decomposedParent {
		set[orchestrator.ConsolidatedBranch(runRow.ID)] = struct{}{}
		for _, c := range children {
			if c == nil {
				continue
			}
			idx := 0
			if c.SliceIndex != nil {
				idx = *c.SliceIndex
			}
			set[orchestrator.SliceBranch(runRow.ID, idx)] = struct{}{}
		}
	}
	return namespaceGuardBranches(set)
}

// namespaceGuardBranches returns the sorted members of set that carry the
// runBranchNamespacePrefix. Split out so the guard is the ONE filter every
// candidate passes through, including one injected by a future derivation.
func namespaceGuardBranches(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for b := range set {
		if !strings.HasPrefix(b, runBranchNamespacePrefix) {
			continue
		}
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// runBranchForgeFor resolves the sweep's forge through a per-family ladder
// identical in shape to prStateReaderFor / issueOpsFor:
//   - cfg.RunBranchDeleter, when set, OVERRIDES the ladder (the test seam).
//   - a github-family run resolves ONLY cfg.GitHub (wrapped by
//     forgegithub.New); it never falls through to the registry.
//   - any other family resolves cfg.ForgeResolver (default forge.Get),
//     guarded by isNilForge.
//
// A forge that does not carry the delete AND the open-PR read is nil.
func (s *Server) runBranchForgeFor(forgeID string) runBranchForge {
	if s.cfg.RunBranchDeleter != nil {
		f, _ := s.cfg.RunBranchDeleter.(runBranchForge)
		return f
	}
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
	rf, ok := f.(runBranchForge)
	if !ok {
		return nil
	}
	return rf
}

// sweepBranchSkip is one preserved branch and why.
type sweepBranchSkip struct {
	Branch string `json:"branch"`
	Reason string `json:"reason"`
}

// sweepBranchError is one branch whose read or delete failed.
type sweepBranchError struct {
	Branch string `json:"branch"`
	Error  string `json:"error"`
}

// runBranchesSweptPayload is the run_branches_swept audit payload.
type runBranchesSweptPayload struct {
	Trigger              string             `json:"trigger"`
	Deleted              []string           `json:"deleted"`
	AlreadyAbsent        []string           `json:"already_absent"`
	SkippedOpenPR        []sweepBranchSkip  `json:"skipped_open_pr"`
	SkippedPRReadError   []sweepBranchError `json:"skipped_pr_read_error"`
	Errors               []sweepBranchError `json:"errors"`
	ChildrenUnenumerated string             `json:"children_unenumerated,omitempty"`
	StagesUnenumerated   string             `json:"stages_unenumerated,omitempty"`
}

// sweepRunBranchesDetached runs sweepRunBranches on a goroutine tracked by
// bgBranchSweeps, on a context detached from the caller's cancellation
// (context.WithoutCancel) and bounded by runBranchSweepBudget — the cancel
// handler's shape (approval condition 3 of #3562), so the response never
// waits on forge round-trips. The run row is copied so the goroutine never
// shares the caller's pointer.
func (s *Server) sweepRunBranchesDetached(parent context.Context, runRow *run.Run, trigger string) {
	if runRow == nil {
		return
	}
	cp := *runRow
	s.bgBranchSweeps.Add(1)
	go func() {
		defer s.bgBranchSweeps.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), runBranchSweepBudget)
		defer cancel()
		s.sweepRunBranches(ctx, &cp, trigger)
	}()
}

// sweepRunBranches deletes a terminal run's Fishhawk-owned branches from the
// forge (E68.67 / #3562) and appends ONE run_branches_swept row. Best-effort
// throughout, on the stampEconomicsIntoPRBody model: every failure logs or
// returns and NEVER unwinds the caller's already-committed transition.
//
// Preservation gates, each failing CLOSED toward keeping the branch:
//   - the branch is the HEAD of an open PR (ListOpenPullRequestsByHead with an
//     empty base, which omits the base filter on both forges);
//   - the branch is the BASE of an open PR whose head is one of this run's
//     candidates (deleting a consolidated branch would auto-close its open
//     slice PRs — approval condition 1). Residual: a PR into a fishhawk/run-*
//     branch from a head OUTSIDE the candidate set is not seen, because the
//     forge PR list is head-keyed on GitHub;
//   - the PR state is unreadable (an unreadable read never authorizes a delete).
//
// A terminal-FAILED run is never swept: POST /v0/runs/{run_id}/revive
// re-admits it and resumes on exactly these branches, so a sweep there would
// destroy recoverable work (deferred to #3678).
//
// A nil forge, a zero credential scope, or an unparseable repo returns
// WITHOUT an audit row — nothing was attempted. A nil AuditRepo skips only
// the append; the deletions still happen.
func (s *Server) sweepRunBranches(ctx context.Context, runRow *run.Run, trigger string) {
	if runRow == nil || runRow.State == run.StateFailed {
		return
	}
	if runRow.DecomposedFrom != nil || s.cfg.RunRepo == nil {
		return
	}
	f := s.runBranchForgeFor(observationForgeID(runRow.InstallationRef))
	if f == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"run branch sweep: no ref-delete-capable forge; skipped",
			slog.String("run_id", runRow.ID.String()))
		return
	}
	scope := mergeObservationScope(runRow)
	if scope.IsZero() {
		return
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return
	}

	payload := runBranchesSweptPayload{
		Trigger:            trigger,
		Deleted:            []string{},
		AlreadyAbsent:      []string{},
		SkippedOpenPR:      []sweepBranchSkip{},
		SkippedPRReadError: []sweepBranchError{},
		Errors:             []sweepBranchError{},
	}
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runRow.ID)
	if err != nil {
		payload.StagesUnenumerated = truncateSweepError(err)
		stages = nil
	}
	children, err := s.listAllDecomposedChildren(ctx, runRow.ID)
	decomposedParent := len(children) > 0
	if err != nil {
		// Degrade to the parent-only set: the consolidated branch is still
		// derivable; the slice branches are not.
		payload.ChildrenUnenumerated = truncateSweepError(err)
		children = nil
		decomposedParent = true
	}
	candidates := runSweepCandidates(runRow, stages, children, decomposedParent)
	if len(candidates) == 0 {
		return
	}

	prober, _ := f.(branchExistenceProber)
	var live, heads []string
	for _, b := range candidates {
		if prober != nil {
			if _, exists, perr := prober.GetBranchSHA(ctx, scope, repo, b); perr == nil && !exists {
				payload.AlreadyAbsent = append(payload.AlreadyAbsent, b)
				continue
			}
		}
		prs, lerr := f.ListOpenPullRequestsByHead(ctx, scope, repo, b, "")
		if lerr != nil {
			payload.SkippedPRReadError = append(payload.SkippedPRReadError,
				sweepBranchError{Branch: b, Error: truncateSweepError(lerr)})
			heads = append(heads, b)
			continue
		}
		if len(prs) > 0 {
			payload.SkippedOpenPR = append(payload.SkippedOpenPR,
				sweepBranchSkip{Branch: b, Reason: sweepSkipOpenPRHead})
			heads = append(heads, b)
			continue
		}
		live = append(live, b)
	}

	for _, b := range live {
		if skip, rerr := openPRTargetsBranch(ctx, f, scope, repo, heads, b); rerr != nil {
			payload.SkippedPRReadError = append(payload.SkippedPRReadError,
				sweepBranchError{Branch: b, Error: truncateSweepError(rerr)})
			continue
		} else if skip {
			payload.SkippedOpenPR = append(payload.SkippedOpenPR,
				sweepBranchSkip{Branch: b, Reason: sweepSkipOpenPRBase})
			continue
		}
		if derr := f.DeleteRef(ctx, scope, repo, b); derr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
				"run branch sweep: delete failed; continuing",
				slog.String("run_id", runRow.ID.String()),
				slog.String("branch", b),
				slog.String("error", derr.Error()))
			payload.Errors = append(payload.Errors,
				sweepBranchError{Branch: b, Error: truncateSweepError(derr)})
			continue
		}
		payload.Deleted = append(payload.Deleted, b)
	}

	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
		"run branch sweep: done",
		slog.String("run_id", runRow.ID.String()),
		slog.String("trigger", trigger),
		slog.Int("deleted", len(payload.Deleted)),
		slog.Int("already_absent", len(payload.AlreadyAbsent)),
		slog.Int("skipped_open_pr", len(payload.SkippedOpenPR)),
		slog.Int("skipped_pr_read_error", len(payload.SkippedPRReadError)),
		slog.Int("errors", len(payload.Errors)))

	if s.cfg.AuditRepo == nil {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	systemKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runRow.ID,
		Timestamp: time.Now().UTC(),
		Category:  RunBranchesSweptCategory,
		ActorKind: &systemKind,
		Payload:   body,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"run branch sweep: audit append failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()))
	}
}

// openPRTargetsBranch reports whether an open PR from any of heads targets
// base — the BASE-branch gate (approval condition 1 of #3562). It reuses the
// forge's PR list keyed on base. heads are the candidates that are themselves
// the head of an open PR or whose PR state was unreadable; a candidate with no
// open PR cannot be the head of one targeting base.
func openPRTargetsBranch(ctx context.Context, f runBranchForge, scope forge.CredentialScope,
	repo forge.RepoRef, heads []string, base string) (bool, error) {
	for _, h := range heads {
		if h == base {
			continue
		}
		prs, err := f.ListOpenPullRequestsByHead(ctx, scope, repo, h, base)
		if err != nil {
			return false, err
		}
		if len(prs) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// truncateSweepError renders err capped at sweepErrorMaxLen bytes.
func truncateSweepError(err error) string {
	msg := err.Error()
	if len(msg) > sweepErrorMaxLen {
		msg = msg[:sweepErrorMaxLen]
	}
	return msg
}
