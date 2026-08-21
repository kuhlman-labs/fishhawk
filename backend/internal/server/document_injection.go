package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// resolveInjectedDocuments resolves, attributes and renders the repo-authored
// documents declared for this run/stage (E55.1 / #2242), returning them as the
// plain-data prompt.InjectedDocument values buildPlan / buildPlanReview /
// buildImplement / buildImplementReview render at the head of the cache-stable
// prefix.
//
// INERT BY DEFAULT. Config.DocumentDeclarations and Config.DocumentResolver are
// both nil in production today — no consumer declares a document yet (E55's
// review_conventions[] and #2234's charter.path are the two that will) — so
// this returns (nil, nil) and every served prompt is byte-identical to the
// pre-#2242 render. The seam exists so both consumers attach at ONE point
// rather than each growing its own resolution path.
//
// FAILS CLOSED. A declaration that cannot be resolved, or an injection that
// cannot be attributed, fails the prompt request rather than serving a prompt
// with a governance document silently missing. Attribution failure in
// particular returns NO documents at all: an un-attributed injection is exactly
// what the attribution property forbids, so the caller must not fall back to
// injecting the resolved-but-unattributed document.
func (s *Server) resolveInjectedDocuments(ctx context.Context, runRow *run.Run, stage *run.Stage) ([]prompt.InjectedDocument, error) {
	if s.cfg.DocumentDeclarations == nil || s.cfg.DocumentResolver == nil || runRow == nil || stage == nil {
		return nil, nil
	}
	decls, baseRef, err := s.cfg.DocumentDeclarations(ctx, runRow, stage)
	if err != nil {
		return nil, fmt.Errorf("resolve document declarations: %w", err)
	}
	if len(decls) == 0 {
		return nil, nil
	}

	repo, err := parseRepoRef(runRow.Repo)
	if err != nil {
		return nil, err
	}
	var scope forge.CredentialScope
	if s.cfg.DocumentScope != nil {
		scope, err = s.cfg.DocumentScope(ctx, repo)
		if err != nil {
			return nil, fmt.Errorf("resolve credential scope for %s: %w", repo, err)
		}
	}

	out := make([]prompt.InjectedDocument, 0, len(decls))
	for _, decl := range decls {
		doc, err := s.cfg.DocumentResolver.Resolve(ctx, repodoc.Request{
			Repo:        repo,
			Scope:       scope,
			BaseRef:     baseRef,
			Declaration: decl,
		})
		if err != nil {
			return nil, err
		}
		// Attribute BEFORE the document joins the returned slice, and return
		// nothing on failure — the injection and its audit entry ship together
		// or not at all.
		if err := repodoc.Attribute(ctx, s.cfg.AuditRepo, runRow.ID, stage.ID, *doc); err != nil {
			return nil, err
		}
		out = append(out, repodoc.ToPromptDocument(*doc, decl.Framing))
	}
	return out, nil
}

// documentInjectionErrorDetails extracts the operator-actionable identifiers
// from a repodoc failure. A *repodoc.ResolveError names the path and the
// declaration site; any other error yields the message alone.
func documentInjectionErrorDetails(err error) map[string]any {
	details := map[string]any{"error": err.Error()}
	var re *repodoc.ResolveError
	if errors.As(err, &re) {
		details["path"] = re.Path
		details["declaration_site"] = re.DeclarationSite
	}
	return details
}
