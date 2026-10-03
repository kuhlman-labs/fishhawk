package server

// This file carries the upkeep-report DEDUPE ADAPTER (E79 / #3921): it reads
// the bounded window of existing work items through the intake hook's
// intakeCandidates seam and hands it to the pure upkeep.MarkDuplicates.
//
// Like the intake hook it rides beside, the adapter DEGRADES rather than
// failing: an upkeep report is ingested whether or not the tracker could be
// read, and a degraded dedupe is reported (a named reason, an empty duplicates
// set) rather than hidden. A missed duplicate costs one redundant proposal the
// captain can see; refusing the report would cost the whole scan.
//
// intakeCandidates is reused UNCHANGED, so its own degrade log lines read as
// intake-groom degrades. That misattribution is cosmetic and accepted to keep
// the intake hook untouched; the reasons the adapter adds itself log under
// their own upkeep message. The long-form contract is
// backend/internal/upkeep/README.md.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/intakegroom"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	workmgmtgithub "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/github"
)

// The two degrade reasons only the adapter can produce. Every other reason is
// intakegroom's typed DegradeReason, carried verbatim.
const (
	// upkeepDedupeReasonRepoMalformed: the run's Repo is not owner/name, so
	// there is no tracker target to read.
	upkeepDedupeReasonRepoMalformed = "repo_malformed"
	// upkeepDedupeReasonConventionsUnavailable: the repo's work-management
	// conventions could not be loaded, so the provider is unknown.
	upkeepDedupeReasonConventionsUnavailable = "conventions_unavailable"
)

// upkeepDedupeResult is what the adapter reports for one report's proposals.
//
// Duplicates is NEVER nil — an empty set is a non-nil empty slice on every
// path, degraded or not — so a caller serializing it always emits a JSON
// array. Degraded and DegradeReason are set together.
type upkeepDedupeResult struct {
	Duplicates      []upkeep.Duplicate
	Degraded        bool
	DegradeReason   string
	ScannedItems    int
	WindowTruncated bool
}

// upkeepDedupeDegraded is the result for a dedupe that could not run.
func upkeepDedupeDegraded(reason string) upkeepDedupeResult {
	return upkeepDedupeResult{
		Duplicates:    []upkeep.Duplicate{},
		Degraded:      true,
		DegradeReason: reason,
	}
}

// upkeepDuplicates marks which of proposals an OPEN work item in runRow's repo
// already covers.
//
// It NEVER returns an error and never panics out: every failure becomes a
// degraded result carrying a named reason. The whole read — conventions,
// installation-scope resolution and the candidate enumeration — runs on ONE
// child context bounded by intakeGroomDeadline, so the caller (the upkeep
// ingest, which calls this before taking its mutex) holds no forge read past
// that bound for a cancellation-cooperative reader (the same honest statement
// of the bound as intake_hook.go's).
func (s *Server) upkeepDuplicates(ctx context.Context, runRow *run.Run, proposals []upkeep.Proposal) (res upkeepDedupeResult) {
	defer func() {
		if rec := recover(); rec != nil {
			reason := string(intakegroom.DegradeReasonHookPanic)
			s.logUpkeepDedupeDegrade(ctx, runRow, reason, fmt.Sprintf("panic recovered in upkeep dedupe: %v", rec))
			res = upkeepDedupeDegraded(reason)
		}
	}()

	if len(proposals) == 0 {
		// A report with no findings has nothing to dedupe; skip the forge
		// read rather than spend it (or degrade on it) for nothing.
		return upkeepDedupeResult{Duplicates: []upkeep.Duplicate{}}
	}

	owner, name, ok := splitRepoFullName(runRow.Repo)
	if !ok {
		s.logUpkeepDedupeDegrade(ctx, runRow, upkeepDedupeReasonRepoMalformed,
			fmt.Sprintf("run repo %q is not owner/name", runRow.Repo))
		return upkeepDedupeDegraded(upkeepDedupeReasonRepoMalformed)
	}

	dctx, cancel := context.WithTimeout(ctx, s.intakeGroomDeadline())
	defer cancel()

	conv, err := conventionsLoader(dctx, runRow.Repo)
	if err != nil {
		reason := upkeepDedupeDeadlineOr(dctx, upkeepDedupeReasonConventionsUnavailable)
		s.logUpkeepDedupeDegrade(ctx, runRow, reason, err.Error())
		return upkeepDedupeDegraded(reason)
	}

	// The run-scoped target, built the way handleFileWorkItem's run-scoped
	// path builds it: coordinates from the run, provider connections from the
	// conventions, the credential scope from the run's installation.
	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Project: conv.Project,
		Jira:    conv.Jira,
		GitLab:  conv.GitLab,
	}
	if runRow.InstallationID != nil {
		target.Scope = forge.FromGitHubInstallationID(*runRow.InstallationID)
	}
	if target.Scope.IsZero() && s.cfg.GitHub != nil && conv.Provider == workmgmtgithub.ProviderName {
		scope, rerr := s.resolveRepoScope(dctx, owner, name)
		if rerr != nil {
			// A failed installation lookup is a failed forge read: the
			// candidate window cannot be enumerated without the scope.
			reason := upkeepDedupeDeadlineOr(dctx, string(intakegroom.DegradeReasonReaderError))
			s.logUpkeepDedupeDegrade(ctx, runRow, reason, "resolve repo installation: "+rerr.Error())
			return upkeepDedupeDegraded(reason)
		}
		target.Scope = scope
	}

	candidates, truncated, reason := s.intakeCandidates(dctx, conv, target)
	if reason != "" {
		// intakeCandidates logged it already.
		return upkeepDedupeDegraded(string(reason))
	}
	return upkeepDedupeResult{
		Duplicates:      upkeep.MarkDuplicates(proposals, candidates),
		ScannedItems:    len(candidates),
		WindowTruncated: truncated,
	}
}

// upkeepDedupeDeadlineOr attributes a failed step to the dedupe's own budget:
// when dctx's deadline has passed the reason is budget_exceeded, whichever
// step was running, so a slow conventions load or installation lookup reads
// the same as a slow candidate enumeration (which intakeCandidates already
// attributes). Any other failure keeps the step's own reason.
func upkeepDedupeDeadlineOr(dctx context.Context, reason string) string {
	if errors.Is(dctx.Err(), context.DeadlineExceeded) {
		return string(intakegroom.DegradeReasonBudgetExceeded)
	}
	return reason
}

// logUpkeepDedupeDegrade WARN-logs one adapter-originated degradation.
func (s *Server) logUpkeepDedupeDegrade(ctx context.Context, runRow *run.Run, reason, detail string) {
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
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "upkeep dedupe degraded; report ingested without duplicate marks", attrs...)
}
