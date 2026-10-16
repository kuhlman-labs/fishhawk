package server

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Merge-candidate staleness (E85.4 / #4147, the Captain's 2026-10-15 ruling,
// option b: detect and surface). An open decomposed parent's consolidated PR
// head goes STALE when its base branch advances past it: the ADR-090 merge
// gate then refuses merge_base_behind (D1), and the operator's move is
// fishhawk_rebase_run_branch. This file reports that state LAZILY at read time
// on GET /v0/runs/{run_id} and the gate view, so the operator sees it before
// reaching for the merge.
//
// It is a bounded, READ-ONLY extension of the ADR-090 merge-candidate surface:
// three forge GETs (GetPullRequest, GetBranchSHA, CompareCommits), no forge
// write, no audit append, no stage transition, no background sweep, and no
// verify command. Advancing the branch stays the operator's call; an
// auto-advance would need its own ADR-090 amendment.
//
// Cost bound: once an entry is stored, a (run, chain-recorded head) pair costs
// at most one read set per mergeCandidateStalenessTTL, each computation under
// a mergeCandidateStalenessReadTimeout deadline. It is NOT a hard rate limit:
// concurrent first misses for the same parent may each issue the reads (there
// is no singleflight), bounded by request concurrency.

// Staleness states.
const (
	mergeCandidateStalenessFresh        = "fresh"
	mergeCandidateStalenessStale        = "stale"
	mergeCandidateStalenessUndetermined = "undetermined"
)

// Undetermined reasons: a closed enum carrying no error text, so no
// forge-supplied string reaches a caller.
const (
	// mergeCandidateStalenessForgeNotProbeable: no GitHub client, no
	// installation (every GitLab-family run), an unparseable repo or no PR
	// number — the merge gate's D7 "cannot probe" anchors.
	mergeCandidateStalenessForgeNotProbeable = "forge_not_probeable"
	// mergeCandidateStalenessPRReadFailed: GetPullRequest failed, or the
	// forge answered an empty head sha or base ref.
	mergeCandidateStalenessPRReadFailed = "pr_read_failed"
	// mergeCandidateStalenessBaseTipUnresolved: the base ref's tip could not
	// be read, or the ref does not exist.
	mergeCandidateStalenessBaseTipUnresolved = "base_tip_unresolved"
	// mergeCandidateStalenessCompareFailed: the head...base-tip compare failed.
	mergeCandidateStalenessCompareFailed = "compare_failed"
	// mergeCandidateStalenessReadTimeout: a read hit the computation deadline.
	mergeCandidateStalenessReadTimeoutReason = "read_timeout"
)

// mergeCandidateStalenessTTL is the cache entry lifetime: a base advance (or a
// head push Fishhawk never recorded) shows within one TTL; checked_at says how
// old the observation is.
const mergeCandidateStalenessTTL = 60 * time.Second

// defaultMergeCandidateStalenessReadTimeout bounds one computation's forge
// reads (all three together).
const defaultMergeCandidateStalenessReadTimeout = 5 * time.Second

// Constant-shape notes, one per state; the values ride the block's fields.
const (
	mergeCandidateStalenessStaleNote        = "The pull request head does not contain the current tip of its base branch, so fishhawk_merge_run would refuse merge_base_behind (ADR-090 D1). fishhawk_rebase_run_branch merges the base into the run branch and authorizes the ADR-090 verify-only merge-candidate pass for the new head. Fishhawk does not advance the branch itself. Read-time observation, cached per run and recorded head for up to 60s (checked_at)."
	mergeCandidateStalenessFreshNote        = "The pull request head contains the current tip of its base branch as of checked_at. Read-time observation, cached per run and recorded head for up to 60s; a later base advance shows within that window."
	mergeCandidateStalenessUndeterminedNote = "Whether the pull request head contains the current tip of its base branch could not be determined (see reason), so it is not reported fresh. The merge gate re-reads the forge at merge time and is the authority."
)

// mergeCandidateStalenessNextStep is the verb a stale block names.
const mergeCandidateStalenessNextStep = "fishhawk_rebase_run_branch"

// mergeCandidateStalenessPayload is the merge_candidate_staleness block on
// GET /v0/runs/{run_id} and the gate view. The MCP mirror
// (backend/internal/mcpserver/client.go runMergeCandidateStaleness) decodes
// it; the json tags MUST byte-match or a field silently decodes to its zero
// value.
type mergeCandidateStalenessPayload struct {
	// State is fresh | stale | undetermined. A failed read is undetermined,
	// never fresh.
	State string `json:"state"`
	// HeadSHA is the LIVE pull request head the forge reported.
	HeadSHA string `json:"head_sha,omitempty"`
	// BaseRef is the pull request's base branch.
	BaseRef string `json:"base_ref,omitempty"`
	// BaseSHA is the base tip HeadSHA was compared against, so BehindBy and
	// BaseSHA are one consistent snapshot.
	BaseSHA string `json:"base_sha,omitempty"`
	// BehindBy is the number of base commits the head lacks: 0 on fresh,
	// positive on stale, omitted on undetermined.
	BehindBy *int `json:"behind_by,omitempty"`
	// BehindByLowerBound is true when the compare hit GitHub's un-paginated
	// 250-commit cap, so BehindBy is a lower bound.
	BehindByLowerBound bool `json:"behind_by_lower_bound"`
	// Reason is set on undetermined only (the closed reason set above).
	Reason string `json:"reason,omitempty"`
	// NextStep is set on stale only: fishhawk_rebase_run_branch.
	NextStep string `json:"next_step,omitempty"`
	// CheckedAt is when the observation was computed (a cache hit returns
	// the stored value).
	CheckedAt time.Time `json:"checked_at"`
	// Note is a constant-shape sentence per state.
	Note string `json:"note"`
}

// mergeCandidateStalenessEntry is one cache entry. A nil block is a cached
// "omit" (the PR is merged or closed).
type mergeCandidateStalenessEntry struct {
	chainHead  string
	block      *mergeCandidateStalenessPayload
	computedAt time.Time
}

// mergeCandidateStalenessReadDeadline is the per-computation forge deadline:
// the test seam when set, else the 5s default.
func (s *Server) mergeCandidateStalenessReadDeadline() time.Duration {
	if s.mergeCandidateStalenessReadTimeout > 0 {
		return s.mergeCandidateStalenessReadTimeout
	}
	return defaultMergeCandidateStalenessReadTimeout
}

// mergeCandidateStalenessFor builds the merge_candidate_staleness block for
// runRow, or (nil, nil) when the run is not an eligible merge candidate:
//
//   - a nil or terminal run, a run with no pull request URL, or no wired
//     RunRepo / AuditRepo;
//   - a run with no decomposition children (the DispatchConsolidatedReview
//     parent predicate). A child run has no children of its own, so this one
//     check excludes it too;
//   - a pull request the forge reports merged or closed.
//
// A children-list or chain-head read error is RETURNED for the caller's
// degrade (the gate view records a history gap; the run read omits the
// block). Forge failures never return an error: they fail closed to an
// undetermined block.
func (s *Server) mergeCandidateStalenessFor(ctx context.Context, runRow *run.Run) (*mergeCandidateStalenessPayload, error) {
	if runRow == nil || runRow.State.IsTerminal() {
		return nil, nil
	}
	if runRow.PullRequestURL == nil || *runRow.PullRequestURL == "" {
		return nil, nil
	}
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		return nil, nil
	}
	children, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{DecomposedFrom: &runRow.ID, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(children) == 0 {
		return nil, nil
	}
	// The cache key's head is the chain-recorded one (audit only, no forge):
	// a Fishhawk-written head change (branch_rebased, fixup_pushed, ...)
	// busts the entry immediately.
	chainHead, err := s.gateViewCurrentHead(ctx, runRow.ID)
	if err != nil {
		return nil, err
	}
	if block, ok := s.mergeCandidateStalenessCached(runRow.ID, chainHead); ok {
		return block, nil
	}
	now := s.nowFunc().UTC()
	block := s.computeMergeCandidateStaleness(ctx, runRow, now)
	s.storeMergeCandidateStaleness(runRow.ID, mergeCandidateStalenessEntry{chainHead: chainHead, block: block, computedAt: now})
	return copyMergeCandidateStaleness(block), nil
}

// mergeCandidateStalenessCached answers a hit only when the stored entry was
// computed for the same chain head and is younger than the TTL. The mutex
// guards the map access only, never a forge read.
func (s *Server) mergeCandidateStalenessCached(runID uuid.UUID, chainHead string) (*mergeCandidateStalenessPayload, bool) {
	s.mergeCandidateStalenessMu.Lock()
	defer s.mergeCandidateStalenessMu.Unlock()
	e, ok := s.mergeCandidateStalenessCache[runID]
	if !ok || e.chainHead != chainHead || s.nowFunc().Sub(e.computedAt) >= mergeCandidateStalenessTTL {
		return nil, false
	}
	return copyMergeCandidateStaleness(e.block), true
}

// storeMergeCandidateStaleness stores e and prunes every expired entry, so the
// map stays bounded by the runs read within one TTL.
func (s *Server) storeMergeCandidateStaleness(runID uuid.UUID, e mergeCandidateStalenessEntry) {
	s.mergeCandidateStalenessMu.Lock()
	defer s.mergeCandidateStalenessMu.Unlock()
	if s.mergeCandidateStalenessCache == nil {
		s.mergeCandidateStalenessCache = map[uuid.UUID]mergeCandidateStalenessEntry{}
	}
	now := s.nowFunc()
	for id, old := range s.mergeCandidateStalenessCache {
		if now.Sub(old.computedAt) >= mergeCandidateStalenessTTL {
			delete(s.mergeCandidateStalenessCache, id)
		}
	}
	s.mergeCandidateStalenessCache[runID] = e
}

// copyMergeCandidateStaleness returns a shallow copy so a caller never holds
// the cached value (BehindBy is never mutated after construction).
func copyMergeCandidateStaleness(b *mergeCandidateStalenessPayload) *mergeCandidateStalenessPayload {
	if b == nil {
		return nil
	}
	c := *b
	return &c
}

// computeMergeCandidateStaleness performs the forge reads, mirroring
// mergeCandidateGate's D7 anchors. The reads run under a deadline derived from
// a context DETACHED from the caller's cancellation, so a client disconnect
// cannot cache a spurious failure; the deadline still bounds the work. It
// calls only GetPullRequest, GetBranchSHA and CompareCommits on cfg.GitHub.
func (s *Server) computeMergeCandidateStaleness(ctx context.Context, runRow *run.Run, now time.Time) *mergeCandidateStalenessPayload {
	block := &mergeCandidateStalenessPayload{CheckedAt: now}
	undetermined := func(reason string) *mergeCandidateStalenessPayload {
		block.State, block.Reason, block.Note = mergeCandidateStalenessUndetermined, reason, mergeCandidateStalenessUndeterminedNote
		block.BehindBy, block.BehindByLowerBound, block.NextStep = nil, false, ""
		return block
	}
	if s.cfg.GitHub == nil {
		return undetermined(mergeCandidateStalenessForgeNotProbeable)
	}
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return undetermined(mergeCandidateStalenessForgeNotProbeable)
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return undetermined(mergeCandidateStalenessForgeNotProbeable)
	}
	prNumber := parsePRNumberFromURL(runRow.PullRequestURL)
	if prNumber <= 0 {
		return undetermined(mergeCandidateStalenessForgeNotProbeable)
	}
	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)

	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.mergeCandidateStalenessReadDeadline())
	defer cancel()
	readFailed := func(reason string) *mergeCandidateStalenessPayload {
		if errors.Is(fctx.Err(), context.DeadlineExceeded) {
			return undetermined(mergeCandidateStalenessReadTimeoutReason)
		}
		return undetermined(reason)
	}

	pr, err := s.cfg.GitHub.GetPullRequest(fctx, scope, repo, prNumber)
	if err != nil {
		return readFailed(mergeCandidateStalenessPRReadFailed)
	}
	// A merged PR has nothing left to go stale. Omitting a CLOSED-unmerged PR
	// too is a deliberate addition beyond mergeCandidateGate, which checks
	// only Merged (a closed PR cannot be merged, so there is no merge to
	// warn about). Do not "fix" the gate to match this, or this to match it.
	if pr.Merged || pr.State == "closed" {
		return nil
	}
	if pr.HeadSHA == "" || pr.BaseRef == "" {
		return undetermined(mergeCandidateStalenessPRReadFailed)
	}
	block.HeadSHA, block.BaseRef = pr.HeadSHA, pr.BaseRef

	baseSHA, found, err := s.cfg.GitHub.GetBranchSHA(fctx, scope, repo, pr.BaseRef)
	if err != nil {
		return readFailed(mergeCandidateStalenessBaseTipUnresolved)
	}
	if !found || baseSHA == "" {
		return undetermined(mergeCandidateStalenessBaseTipUnresolved)
	}
	block.BaseSHA = baseSHA

	// The merge gate's D1 call order: base = the head, head = the base tip,
	// so the three-dot compare lists the base commits the head lacks. It is
	// compared against the RESOLVED base sha, not the ref, so BehindBy and
	// BaseSHA describe one snapshot.
	behind, err := s.cfg.GitHub.CompareCommits(fctx, scope, repo, pr.HeadSHA, baseSHA)
	if err != nil {
		return readFailed(mergeCandidateStalenessCompareFailed)
	}
	n := len(behind)
	block.BehindBy = &n
	if n > 0 {
		block.State, block.Note, block.NextStep = mergeCandidateStalenessStale, mergeCandidateStalenessStaleNote, mergeCandidateStalenessNextStep
		block.BehindByLowerBound = vouchProbeAtCap(n)
		return block
	}
	block.State, block.Note = mergeCandidateStalenessFresh, mergeCandidateStalenessFreshNote
	return block
}
