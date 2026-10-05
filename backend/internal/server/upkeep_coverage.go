package server

// This file carries the upkeep-report DEPENDABOT COVERAGE ADAPTER (#3750): it
// lists the repository's open pull requests through the GitHub client and
// hands them, with the report's advisory findings, to the pure
// upkeep.MarkCovered. A covered finding is still proposed at the gate; the
// apply skips filing it (covered_by_dependabot_pr) because an open Dependabot
// pull request already fixes it.
//
// Like the dedupe adapter it rides beside (upkeep_dedupe.go), it DEGRADES
// rather than failing: the report is ingested whether or not the forge could
// be read, and a degraded coverage read is recorded (a named reason, an empty
// covered set) rather than hidden. A missed cover costs one redundant filing
// the captain can see; refusing the report would cost the whole scan. Every
// degrade fails toward NOT covered, so a finding is never silently dropped.
//
// Long-form contract: backend/internal/server/README.md § "On-approval upkeep
// apply" (the coverage adapter degrade table) and backend/internal/upkeep/
// README.md § "Dependabot coverage and advisory filing (#3750)".

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
)

// The coverage adapter's named degrade reasons: the closed set the
// upkeep_report_recorded row's coverage_degrade_reason carries.
const (
	// upkeepCoverageForgeUnsupported: the run is not on GitHub, the only forge
	// Dependabot coverage reads.
	upkeepCoverageForgeUnsupported = "forge_unsupported"
	// upkeepCoverageGitHubUnwired: no GitHub client is configured.
	upkeepCoverageGitHubUnwired = "github_unwired"
	// upkeepCoverageRepoMalformed: the run's Repo is not owner/name.
	upkeepCoverageRepoMalformed = "repo_malformed"
	// upkeepCoverageScopeUnavailable: the repository's installation scope
	// could not be resolved (lookup error, or the App is not installed).
	upkeepCoverageScopeUnavailable = "scope_unavailable"
	// upkeepCoveragePullListFailed: the open pull-request listing failed.
	upkeepCoveragePullListFailed = "pull_list_failed"
	// upkeepCoverageBudgetExceeded: upkeepCoverageBudget passed, whichever
	// step was running.
	upkeepCoverageBudgetExceeded = "budget_exceeded"
	// upkeepCoveragePanic: a panic was recovered inside the adapter.
	upkeepCoveragePanic = "coverage_panic"
)

// upkeepCoverageMaxPulls caps the open pull requests one coverage read lists
// (three pages of 100). A longer listing is recorded window-truncated and
// covers from the pull requests it did read: an unread Dependabot pull
// request can only leave a finding uncovered, never falsely cover one.
const upkeepCoverageMaxPulls = 300

// upkeepCoverageBudget bounds the whole forge read — scope resolution and the
// paged listing — on one child context. A var ONLY so a test can shrink it.
var upkeepCoverageBudget = 10 * time.Second

// The production listing's own failure classes, mapped to named degrades by
// upkeepCoverage. Wrapped so the listing's detail survives into the log.
var (
	errUpkeepCoverageGitHubUnwired    = errors.New("upkeep coverage: no GitHub client is configured")
	errUpkeepCoverageScopeUnavailable = errors.New("upkeep coverage: the repository's installation scope is unavailable")
)

// upkeepListOpenPulls lists runRow's open pull requests, at most maxPulls of
// them. A package var so a test substitutes the listing without a GitHub
// server (the newUpkeepPinSource / upkeepListWorkflowDir pattern: no new
// Server or Config field).
var upkeepListOpenPulls = func(ctx context.Context, s *Server, runRow *run.Run, repo forge.RepoRef, maxPulls int) ([]githubclient.OpenPullRequest, bool, error) {
	return s.upkeepGitHubOpenPulls(ctx, runRow, repo, maxPulls)
}

// upkeepGitHubOpenPulls is the production listing: the run's installation
// scope (else the repository's, resolved through the App), then the paged
// open pull-request listing.
func (s *Server) upkeepGitHubOpenPulls(ctx context.Context, runRow *run.Run, repo forge.RepoRef, maxPulls int) ([]githubclient.OpenPullRequest, bool, error) {
	if s.cfg.GitHub == nil {
		return nil, false, errUpkeepCoverageGitHubUnwired
	}
	var scope forge.CredentialScope
	if runRow.InstallationID != nil {
		scope = forge.FromGitHubInstallationID(*runRow.InstallationID)
	} else {
		resolved, err := s.resolveRepoScope(ctx, repo.Owner, repo.Name)
		if err != nil {
			return nil, false, fmt.Errorf("%w: %v", errUpkeepCoverageScopeUnavailable, err)
		}
		scope = resolved
	}
	if scope.IsZero() {
		return nil, false, fmt.Errorf("%w: the GitHub App is not installed on %s/%s", errUpkeepCoverageScopeUnavailable, repo.Owner, repo.Name)
	}
	return s.cfg.GitHub.ListOpenPullRequests(ctx, scope, repo, maxPulls)
}

// upkeepCoverageResult is what the adapter reports for one report.
//
// Covered is NEVER nil — an empty set is a non-nil empty slice on every path,
// degraded or not — so a caller serializing it always emits a JSON array.
// Degraded and DegradeReason are set together.
type upkeepCoverageResult struct {
	Covered         []upkeep.Covered
	Degraded        bool
	DegradeReason   string
	ScannedPulls    int
	WindowTruncated bool
}

// upkeepCoverageDegraded is the result for a coverage read that could not run.
func upkeepCoverageDegraded(reason string) upkeepCoverageResult {
	return upkeepCoverageResult{
		Covered:       []upkeep.Covered{},
		Degraded:      true,
		DegradeReason: reason,
	}
}

// upkeepAdvisoryProposals adapts every advisory finding WITH a fixed version
// to the coverage matcher's input. A finding with no fix published can never
// be covered, so it is not adapted (and does not cost a forge read). The
// directories are the finding's cited MANIFEST directories only
// (plan.UpkeepAdvisoryManifestDirs): a call-site source file cited as
// evidence never becomes a directory coverage must reach.
func upkeepAdvisoryProposals(r *plan.UpkeepReport) []upkeep.AdvisoryProposal {
	out := []upkeep.AdvisoryProposal{}
	for i := range r.Findings {
		f := &r.Findings[i]
		if f.Source != plan.UpkeepSourceAdvisory || f.Advisory == nil || f.Advisory.FixedVersion == nil {
			continue
		}
		out = append(out, upkeep.AdvisoryProposal{
			FindingID:    f.ID,
			Ecosystem:    f.Advisory.Ecosystem,
			Package:      f.Advisory.Package,
			Version:      f.Advisory.Version,
			FixedVersion: *f.Advisory.FixedVersion,
			Directories:  plan.UpkeepAdvisoryManifestDirs(f),
		})
	}
	return out
}

// upkeepPullRequests adapts the forge listing to the matcher's input.
func upkeepPullRequests(pulls []githubclient.OpenPullRequest) []upkeep.PullRequest {
	out := make([]upkeep.PullRequest, 0, len(pulls))
	for _, p := range pulls {
		out = append(out, upkeep.PullRequest{
			Number: p.Number, URL: p.HTMLURL, Title: p.Title, Body: p.Body,
			Author: p.UserLogin, HeadRef: p.HeadRef,
			BaseRef: p.BaseRef, DefaultBranch: p.DefaultBranch,
		})
	}
	return out
}

// upkeepCoverage marks which of report's advisory findings an open
// Dependabot pull request in runRow's repository already fixes.
//
// It NEVER returns an error and never panics out: every failure becomes a
// degraded result carrying a named reason. A report with no coverable
// advisory finding (none, or none with a fixed version) is a healthy empty
// result with NO forge read. Otherwise the whole read runs on ONE child
// context bounded by upkeepCoverageBudget, so the caller (the upkeep ingest,
// which calls this before taking its mutex) holds no forge read past that
// bound for a cancellation-cooperative reader.
func (s *Server) upkeepCoverage(ctx context.Context, runRow *run.Run, report *plan.UpkeepReport) (res upkeepCoverageResult) {
	defer func() {
		if rec := recover(); rec != nil {
			s.logUpkeepCoverageDegrade(ctx, runRow, upkeepCoveragePanic, fmt.Sprintf("panic recovered in upkeep coverage: %v", rec))
			res = upkeepCoverageDegraded(upkeepCoveragePanic)
		}
	}()

	advisories := upkeepAdvisoryProposals(report)
	if len(advisories) == 0 {
		return upkeepCoverageResult{Covered: []upkeep.Covered{}}
	}
	if forgeID := runForge(runRow); forgeID != "github" {
		s.logUpkeepCoverageDegrade(ctx, runRow, upkeepCoverageForgeUnsupported,
			fmt.Sprintf("run forge %q has no Dependabot coverage read", forgeID))
		return upkeepCoverageDegraded(upkeepCoverageForgeUnsupported)
	}
	owner, name, ok := splitRepoFullName(runRow.Repo)
	if !ok {
		s.logUpkeepCoverageDegrade(ctx, runRow, upkeepCoverageRepoMalformed,
			fmt.Sprintf("run repo %q is not owner/name", runRow.Repo))
		return upkeepCoverageDegraded(upkeepCoverageRepoMalformed)
	}

	cctx, cancel := context.WithTimeout(ctx, upkeepCoverageBudget)
	defer cancel()

	pulls, truncated, err := upkeepListOpenPulls(cctx, s, runRow, forge.RepoRef{Owner: owner, Name: name}, upkeepCoverageMaxPulls)
	if err != nil {
		reason := upkeepCoveragePullListFailed
		switch {
		case errors.Is(cctx.Err(), context.DeadlineExceeded):
			// Whichever step was running when the budget passed.
			reason = upkeepCoverageBudgetExceeded
		case errors.Is(err, errUpkeepCoverageGitHubUnwired):
			reason = upkeepCoverageGitHubUnwired
		case errors.Is(err, errUpkeepCoverageScopeUnavailable):
			reason = upkeepCoverageScopeUnavailable
		}
		s.logUpkeepCoverageDegrade(ctx, runRow, reason, err.Error())
		return upkeepCoverageDegraded(reason)
	}
	return upkeepCoverageResult{
		Covered:         upkeep.MarkCovered(advisories, upkeepPullRequests(pulls)),
		ScannedPulls:    len(pulls),
		WindowTruncated: truncated,
	}
}

// logUpkeepCoverageDegrade WARN-logs one coverage degradation.
func (s *Server) logUpkeepCoverageDegrade(ctx context.Context, runRow *run.Run, reason, detail string) {
	if s.cfg.Logger == nil {
		return
	}
	attrs := []slog.Attr{
		slog.String("degrade_reason", reason),
		slog.String("detail", detail),
	}
	if runRow != nil {
		attrs = append(attrs,
			slog.String("run_id", runRow.ID.String()),
			slog.String("repo", runRow.Repo))
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "upkeep coverage degraded; report ingested without Dependabot coverage marks", attrs...)
}
