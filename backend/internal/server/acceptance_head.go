package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
)

// acceptanceHeadResolution is acceptanceHeadResolved's answer: the resolved
// acceptance head and whether the seed actually chosen was a consolidated
// fan-in integration row (so the prompt resolver can keep its existing INFO
// line naming that arm). It is unexported and has one producer, so the
// wrapper acceptanceHeadAsOf keeps every consumer's (string, error) shape.
type acceptanceHeadResolution struct {
	head      string
	fanInSeed bool
}

// acceptanceHeadAsOf is THE acceptance head resolution (#4874): the head the
// acceptance stage is served (acceptance_expected_head_sha), the head its
// verdict binds to (acceptanceValidatedHeadSHA, bounded by the dispatch
// anchor) and the head Option C's retry compares against all read it.
//
// The seed is the newest row by CHAIN SEQUENCE across the run's reported-head
// ledger rows (lineageLedgerCategories) AND its integration_commit_recorded
// rows; the resolver never reads a timestamp. The seed is then advanced
// through the acceptanceBaseMergeCategories rows recorded after it
// (walkRecordedBaseMerges, the walk the fix-up head also runs): every CLEAN
// branch_rebased row, every on-chain branch_reset and every server-VERIFIED
// operator_commit_vouched step (#4856's arm, isVerifiedVouchAdvance). So a
// Fishhawk-performed base advance moves the acceptance head to the merge
// commit it produced, and a verified fast-forward vouch moves it to the
// vouched commit (#4874). Every other vouch row (legacy, unverified, the rebase
// verb's attribution row, a stale prior) is inert: acceptance stays on the
// recorded head.
//
// When bounded is true every candidate set — both seed sets and the walk rows,
// vouch rows included — is cut to Sequence <= maxSeq BEFORE any selection, so
// a row recorded after the acceptance dispatch (a rebase or a vouch) can never
// re-bind that dispatch's verdict.
//
// Answers "" with a nil error when the AuditRepo is unconfigured or no seed
// exists (a rebase alone never invents a head). Any read error is RETURNED:
// an empty head makes the dispatch verb and the runner refuse with
// acceptance_expected_head_unresolved, whereas a stale seed would silently
// validate the wrong tree. This deliberately differs from the fix-up seed
// (resolveFixupExpectedHeadSHA), which keeps its timestamp ordering and its
// seed-on-read-error posture unchanged.
func (s *Server) acceptanceHeadAsOf(ctx context.Context, runID uuid.UUID, maxSeq int64, bounded bool) (string, error) {
	res, err := s.acceptanceHeadResolved(ctx, runID, maxSeq, bounded)
	if err != nil {
		return "", err
	}
	return res.head, nil
}

// acceptanceHeadResolved is acceptanceHeadAsOf's one code path, also
// reporting whether the chosen seed was an integration row.
func (s *Server) acceptanceHeadResolved(ctx context.Context, runID uuid.UUID, maxSeq int64, bounded bool) (acceptanceHeadResolution, error) {
	seed, sha, err := s.acceptanceHeadSeed(ctx, runID, maxSeq, bounded)
	if err != nil {
		return acceptanceHeadResolution{}, err
	}
	if seed == nil {
		return acceptanceHeadResolution{}, nil
	}
	head, err := s.advanceAcceptanceSeed(ctx, runID, seed, sha, maxSeq, bounded)
	if err != nil {
		return acceptanceHeadResolution{}, err
	}
	return acceptanceHeadResolution{
		head:      head,
		fanInSeed: seed.Category == lineageIntegrationCommitCategory,
	}, nil
}

// acceptanceHeadSeed ALWAYS reads both seed sets: every lineageLedgerCategories
// row and every integration_commit_recorded row. The integration category is
// read beside that list, never added to it, so lineageLedgerCategories
// membership is unchanged. A read error on EITHER set fails closed even when
// the other set already holds a candidate, because the unreadable set may hold
// the newer head.
func (s *Server) acceptanceHeadSeed(ctx context.Context, runID uuid.UUID, maxSeq int64, bounded bool) (*audit.Entry, string, error) {
	if s.cfg.AuditRepo == nil {
		return nil, "", nil
	}
	var reported []*audit.Entry
	for _, cat := range lineageLedgerCategories {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			return nil, "", fmt.Errorf("list reported-head rows (%s): %w", cat, err)
		}
		reported = append(reported, entries...)
	}
	integrations, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, lineageIntegrationCommitCategory)
	if err != nil {
		return nil, "", fmt.Errorf("list integration rows: %w", err)
	}
	if bounded {
		reported = rowsAtOrBefore(reported, maxSeq)
		integrations = rowsAtOrBefore(integrations, maxSeq)
	}
	rep, repSHA := newestReportedHeadBySequence(reported)
	fan, fanSHA := newestFanInHead(integrations)
	seed, sha := newerAcceptanceSeed(rep, repSHA, fan, fanSHA)
	return seed, sha, nil
}

// advanceAcceptanceSeed walks the seed through the run's recorded base merges
// and verified vouch steps after it (clean rebases, on-chain resets and
// verified vouches advance; every other vouch row is inert). A read error on
// any walk category is returned (fail closed), unlike the fix-up path's seed
// fallback (advanceFixupHeadThroughBaseMerges).
func (s *Server) advanceAcceptanceSeed(ctx context.Context, runID uuid.UUID, seed *audit.Entry, sha string, maxSeq int64, bounded bool) (string, error) {
	rows, err := s.acceptanceBaseMergeRows(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("list base-merge rows: %w", err)
	}
	if bounded {
		rows = rowsAtOrBefore(rows, maxSeq)
	}
	return walkRecordedBaseMerges(rows, seed.Sequence, sha), nil
}

// acceptanceBaseMergeCategories are the categories the acceptance walk reads
// after its seed: the recorded base merges and the operator vouches (#4874).
// They are the fix-up walk's set (fixupHeadWalkCategories), so reset, the
// fix-up base check and the acceptance expected head give ONE answer to "is a
// vouched commit run lineage": a verified vouch is the head, any other vouch
// is membership only. TestAcceptanceBaseMergeCategories_MatchFixupWalk pins
// the agreement.
var acceptanceBaseMergeCategories = []string{CategoryBranchRebased, CategoryBranchReset, CategoryOperatorCommitVouched}

// acceptanceBaseMergeRows reads the run's acceptanceBaseMergeCategories rows
// for the acceptance walk, returning the first read error. A vouch-row read
// error fails closed exactly like a rebase-row one: acceptanceHeadAsOf then
// resolves no head rather than a head that may predate a verified vouch.
func (s *Server) acceptanceBaseMergeRows(ctx context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	var rows []*audit.Entry
	for _, cat := range acceptanceBaseMergeCategories {
		entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, cat)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", cat, err)
		}
		rows = append(rows, entries...)
	}
	return rows, nil
}

// rowsAtOrBefore keeps the entries with Sequence <= maxSeq. A nil slice
// returns an empty result and a nil entry is dropped.
func rowsAtOrBefore(entries []*audit.Entry, maxSeq int64) []*audit.Entry {
	out := make([]*audit.Entry, 0, len(entries))
	for _, e := range entries {
		if e == nil || e.Sequence > maxSeq {
			continue
		}
		out = append(out, e)
	}
	return out
}

// newestReportedHeadBySequence selects the highest-sequence row carrying a
// decodable, non-empty head_sha. Within an equal sequence the first row
// visited is kept. No timestamp is read.
func newestReportedHeadBySequence(entries []*audit.Entry) (*audit.Entry, string) {
	var newest *audit.Entry
	var newestSHA string
	for _, e := range entries {
		if e == nil {
			continue
		}
		var payload struct {
			HeadSHA string `json:"head_sha"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil || payload.HeadSHA == "" {
			continue
		}
		if newest == nil || e.Sequence > newest.Sequence {
			newest = e
			newestSHA = payload.HeadSHA
		}
	}
	return newest, newestSHA
}

// newestFanInHead selects the highest-sequence integration_commit_recorded row
// carrying a decodable, non-empty merge_sha. No timestamp is read.
func newestFanInHead(entries []*audit.Entry) (*audit.Entry, string) {
	var best *audit.Entry
	var bestSHA string
	for _, e := range entries {
		if e == nil {
			continue
		}
		var payload struct {
			MergeSHA string `json:"merge_sha"`
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil || payload.MergeSHA == "" {
			continue
		}
		if best == nil || e.Sequence > best.Sequence {
			best = e
			bestSHA = payload.MergeSHA
		}
	}
	return best, bestSHA
}

// newerAcceptanceSeed chooses across the two seed sets: the newest chain
// sequence wins, and a reported row wins only an equal-sequence tie, which
// only a test fake produces (production sequences are unique on a chain).
func newerAcceptanceSeed(rep *audit.Entry, repSHA string, fan *audit.Entry, fanSHA string) (*audit.Entry, string) {
	if fan != nil && (rep == nil || fan.Sequence > rep.Sequence) {
		return fan, fanSHA
	}
	return rep, repSHA
}

// acceptanceFanInBranch answers a decomposed parent's consolidated branch when
// the run has any integration_commit_recorded row. The branch is server-derived
// (orchestrator.ConsolidatedBranch), never taken from a report. "" with a nil
// error when the AuditRepo is unconfigured or the run has no integration row.
func (s *Server) acceptanceFanInBranch(ctx context.Context, runID uuid.UUID) (string, error) {
	if s.cfg.AuditRepo == nil {
		return "", nil
	}
	integrations, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, lineageIntegrationCommitCategory)
	if err != nil {
		return "", fmt.Errorf("list integration rows for the run branch: %w", err)
	}
	if len(integrations) > 0 {
		return orchestrator.ConsolidatedBranch(runID), nil
	}
	return "", nil
}

// acceptanceRunPRNumber reads the run row's pull-request URL for a known
// branch whose PR number the ledger did not resolve (a decomposed parent's
// consolidated PR is set through SetRunPullRequestURL and writes no
// pull_request_opened row). A missing RunRepo, a read error (run.ErrNotFound
// included) and a nil run are errors; a run with no URL answers 0 with a nil
// error.
func (s *Server) acceptanceRunPRNumber(ctx context.Context, runID uuid.UUID) (int, error) {
	if s.cfg.RunRepo == nil {
		return 0, errors.New("read run for pull request number: run repository not configured")
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		return 0, fmt.Errorf("read run for pull request number: %w", err)
	}
	if runRow == nil {
		return 0, errors.New("read run for pull request number: run not found")
	}
	return parsePRNumberFromURL(runRow.PullRequestURL), nil
}
