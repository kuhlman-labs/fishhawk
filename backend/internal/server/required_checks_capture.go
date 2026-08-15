package server

import (
	"context"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// requiredChecksCaptureTimeout bounds the whole required-checks capture
// (default-branch + classic protection + rulesets list + per-ruleset
// fetches) on the operator-facing run-create path (#2506). CreateRunForTrigger
// is ALSO the campaign driver's per-item chokepoint, so an unreachable forge
// can add up to this much sequential latency to a create an operator waits on
// — a single child-context deadline covers every capture round-trip so the
// worst case is bounded regardless of how many rulesets the repo has. 10s sits
// within the same order as the spec-fetch + issue-hydration GitHub round-trips
// the create path already performs per item, so the campaign ticker tolerates
// it; it is a var (not a const) only so a test can shrink it to exercise the
// timeout degrade. Do NOT remove the bound — an unbounded lookup here would
// hang every create behind one slow repo.
var requiredChecksCaptureTimeout = 10 * time.Second

// captureRequiredChecks resolves the run's required-status-check snapshot at
// run-create time for the local / MCP / CLI / campaign create paths (#2506),
// closing the gap that left those runs permanently at checks_unresolved after
// #2497. It resolves the repo's REAL default branch and threads it into the
// shared webhook.ResolveRequiredChecks so a repo defaulting to e.g. `master`
// captures its `~DEFAULT_BRANCH` rulesets correctly (CONDITION 1(a)).
//
// FAIL-SAFE INVARIANT — this is the invariant the whole design rests on: the
// returned snapshot is NIL on EVERY degrade, and NEVER a present-but-empty one.
// A present-but-empty snapshot is a POSITIVE CLAIM that the repo genuinely
// requires nothing (the #2497 vacuous-green arm) and may be written ONLY when
// the evaluation was AUTHORITATIVE for this branch — both protection surfaces
// answered definitively AND every ruleset condition was one the matcher could
// evaluate. Any of: no GitHub client, no attributable installation, an
// unsplittable repo slug, a default-branch lookup failure, a missing
// administration:read scope, a transport error, or a ruleset carrying an
// include token the matcher cannot evaluate (an unknown ~TOKEN / an fnmatch
// glob — CONDITION 1(b)) leaves the snapshot NIL, so #2497's checks_unresolved
// path engages instead of a vacuous green. It NEVER returns an error: run
// creation must not fail because a forge lookup was slow or unreachable.
//
// FRESHNESS CONTRACT: the snapshot is resolved ONCE here at run-create and is
// never re-resolved for the life of the run; a ruleset edit mid-run is not
// reflected. Re-resolution at the gate is deliberately out of scope because
// GitHub's own protected-branch enforcement remains the merge-time authority —
// a stale-green snapshot cannot force a merge (the API refuses it, which
// merge_run classifies as a wait per #2722).
func (s *Server) captureRequiredChecks(ctx context.Context, repo string, scope forge.CredentialScope) *run.RequiredChecksSnapshot {
	if s.cfg.GitHub == nil {
		return nil
	}
	if scope.IsZero() {
		// No attributable App installation → no credential to query branch
		// protection with. Distinct from "nothing required": greenness is
		// unknown, so leave the snapshot nil. (An early-exit for a clear
		// WARN reason — a zero scope also fails downstream at
		// GetRepository's scope resolution, so the nil outcome is
		// guaranteed either way; this just names the cause precisely.)
		s.cfg.Logger.Warn("required-checks capture skipped: no installation",
			"repo", repo, "reason", "no_installation")
		return nil
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		s.cfg.Logger.Warn("required-checks capture skipped: repo not owner/name",
			"repo", repo, "reason", "unsplittable_repo")
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, requiredChecksCaptureTimeout)
	defer cancel()

	repoRef := forge.RepoRef{Owner: owner, Name: name}

	// Resolve the repo's REAL default branch. Guessing "main" on failure
	// would silently mis-target a non-main-default repo's ~DEFAULT_BRANCH
	// ruleset and manufacture an authoritative-looking empty snapshot — the
	// vacuous green #2497 closed — so ANY error here leaves the snapshot nil.
	repoInfo, err := s.cfg.GitHub.GetRepository(ctx, scope, repoRef)
	if err != nil {
		s.cfg.Logger.Warn("required-checks capture skipped: default-branch lookup failed",
			"repo", repo, "reason", "default_branch_lookup_failed", "error", err.Error())
		return nil
	}
	defaultBranch := repoInfo.DefaultBranch

	snap, authoritative, err := webhook.ResolveRequiredChecks(ctx, s.cfg.GitHub, scope, repoRef, defaultBranch, defaultBranch)
	if err != nil {
		s.cfg.Logger.Warn("required-checks capture skipped: protection lookup failed",
			"repo", repo, "branch", defaultBranch, "reason", "protection_lookup_failed", "error", err.Error())
		return nil
	}
	if !authoritative {
		// A surface was unqueryable (rulesets 404) or a ruleset carried a
		// condition the matcher could not evaluate. Empty here would be an
		// unearned "nothing required" — fail closed to nil so #2497 engages.
		s.cfg.Logger.Warn("required-checks capture skipped: non-authoritative evaluation",
			"repo", repo, "branch", defaultBranch, "reason", "non_authoritative")
		return nil
	}
	return snap
}
