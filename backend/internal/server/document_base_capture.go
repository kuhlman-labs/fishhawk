package server

import (
	"context"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// documentBaseCaptureTimeout bounds the whole run-admission document-base
// capture (credential scope + default-branch lookup + branch-to-commit pin) on
// the run-create path (E55.7 / #3746). It mirrors requiredChecksCaptureTimeout
// (#2506) for the same reason: CreateRunForTrigger is ALSO the campaign
// driver's per-item chokepoint, so an unreachable forge can add up to this much
// sequential latency to a create, and a single child-context deadline bounds
// every round-trip regardless of how slow the forge is. It is a var (not a
// const) only so a test can shrink it to exercise the timeout degrade. Do NOT
// remove the bound — an unbounded lookup here would hang every create behind
// one slow repo.
var documentBaseCaptureTimeout = 10 * time.Second

// Reasons the document-base capture logs when it degrades to NIL. Each is a
// distinct WARN reason so an operator reading a withheld-document notice can
// tell WHY no commit was recorded for the run.
const (
	documentBaseReasonSeamUnwired     = "document_seam_unwired"
	documentBaseReasonUnparseableRepo = "unparseable_repo"
	documentBaseReasonScopeFailed     = "credential_scope_failed"
	documentBaseReasonBaseRefFailed   = "base_ref_lookup_failed"
	documentBaseReasonEmptyBaseRef    = "empty_base_ref"
	documentBaseReasonPinFailed       = "pin_commit_failed"
)

// CaptureDocumentBaseCommit is the exported entry point the webhook dispatcher
// is wired to after server.New (webhook.Dispatcher.DocumentBaseCommit): the
// dispatcher is constructed before the Server exists and server.New copies its
// Config by value, so the hook is a method value bound to the constructed
// Server rather than a Config field. Same contract as captureDocumentBaseCommit.
func (s *Server) CaptureDocumentBaseCommit(ctx context.Context, repo string) *string {
	return s.captureDocumentBaseCommit(ctx, repo)
}

// captureDocumentBaseCommit resolves the commit a run's run-admission document
// declarations (repodoc.BaseSourceRunAdmission) are later resolved at, ONCE, at
// run admission (E55.7 / #3746). It pins the repository's DEFAULT-BRANCH head
// through the existing document seam — DocumentScope → DocumentBaseRef →
// repodoc.Resolver.PinCommit — and returns the lowercase 40-hex commit, which
// the caller persists on runs.document_base_commit. Children inherit it through
// run.ChildParamsFrom and a fix-up re-serves from the same row, so every later
// resolution for the run reads the admission commit, never a newer head and
// never the run's own branch.
//
// Why the default-branch head and not the run's own base branch: at admission
// the server cannot know the --base-branch an operator later passes to
// dispatch_stage, and the webhook dispatcher dispatches against its
// DefaultRef. This repository bases every run on the default branch; capturing
// a non-default base branch is tracked as #3902.
//
// NIL ON EVERY DEGRADE, NEVER AN ERROR. An unwired seam, an unparseable repo
// slug, a scope / base-ref / pin failure, an empty base ref and the deadline
// each return nil with a WARN naming the reason. Run creation must not fail
// because a forge lookup was slow or unreachable, and nil is the honest value:
// it means "no commit recorded", which withholds the run's run-admission
// documents with a named degradation at prompt serve rather than reading them
// at a mutable ref. A non-nil return is always a PinCommit output, so it is a
// lowercase 40-hex SHA the runs CHECK constraint accepts.
func (s *Server) captureDocumentBaseCommit(ctx context.Context, repo string) *string {
	if s.cfg.DocumentResolver == nil || s.cfg.DocumentBaseRef == nil {
		s.cfg.Logger.Warn("document-base capture skipped: document seam unwired",
			"repo", repo, "reason", documentBaseReasonSeamUnwired)
		return nil
	}
	repoRef, err := parseRepoRef(repo)
	if err != nil {
		s.cfg.Logger.Warn("document-base capture skipped: repo not owner/name",
			"repo", repo, "reason", documentBaseReasonUnparseableRepo, "error", err.Error())
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, documentBaseCaptureTimeout)
	defer cancel()

	var scope forge.CredentialScope
	if s.cfg.DocumentScope != nil {
		scope, err = s.cfg.DocumentScope(ctx, repoRef)
		if err != nil {
			s.cfg.Logger.Warn("document-base capture skipped: credential scope failed",
				"repo", repo, "reason", documentBaseReasonScopeFailed, "error", err.Error())
			return nil
		}
	}
	baseRef, err := s.cfg.DocumentBaseRef(ctx, repoRef)
	if err != nil {
		s.cfg.Logger.Warn("document-base capture skipped: base ref lookup failed",
			"repo", repo, "reason", documentBaseReasonBaseRefFailed, "error", err.Error())
		return nil
	}
	if strings.TrimSpace(baseRef) == "" {
		// A forge reads an empty ref as the default branch — a mutable read.
		// PinCommit refuses it too; this names the cause precisely.
		s.cfg.Logger.Warn("document-base capture skipped: empty base ref",
			"repo", repo, "reason", documentBaseReasonEmptyBaseRef)
		return nil
	}
	commit, err := s.cfg.DocumentResolver.PinCommit(ctx, repoRef, scope, baseRef)
	if err != nil {
		s.cfg.Logger.Warn("document-base capture skipped: pin to commit failed",
			"repo", repo, "base_ref", baseRef, "reason", documentBaseReasonPinFailed, "error", err.Error())
		return nil
	}
	return &commit
}
