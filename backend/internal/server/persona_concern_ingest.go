package server

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
)

// Persona concern ingest (ADR-084 D3 / rule 3 / E55.10 / #3755). A reviewer
// persona's verdict passes the same convention clamp as every reviewer's, and
// then THREE persona controls the standard reviewers never see:
//
//   - QUOTE VERIFICATION: a concern carrying quoted_passage + document_ref is
//     checked against the exact text the server injected into THAT persona's
//     prompt (its remit, the stage's injected documents and its rendered
//     review conventions). A quote not found is demoted to low and marked
//     quote_unverified; a found one records the matched document's
//     content_hash.
//   - SEVERITY CAP: every concern above the persona remit's severity_cap is
//     lowered to it (severity_clamped_from, persona_severity_cap).
//   - The shared reject-downgrade rule: a persona reject resting only on
//     lowered highs becomes approve_with_concerns (verdict_clamped_from).
//
// Two controls apply to EVERY verdict, standard included: the server-internal
// ingest markers are scrubbed first (a reviewer on the unconstrained decode
// path cannot pre-set them), and every concern is then stamped with the
// reviewer_role of the invocation that raised it, so the *_reviewed payload and
// the persisted concern row agree. Persona concerns enter the existing concern
// state machine unchanged — no new authority mode (ADR-084 rule 4).

// reviewerRole returns the reviewer_role this invocation's concerns are
// attributed to: the persona's name, or concern.ReviewerRoleStandard for a
// standard reviewer.
func (inv reviewerInvocation) reviewerRole() string {
	if n := inv.personaName(); n != "" {
		return n
	}
	return concern.ReviewerRoleStandard
}

// personaQuotedDocuments maps the documents injected into a persona prompt to
// the set VerifyQuotedPassages checks quotes against. Text is the exact
// delimiter-bracketed document text the reviewer was shown
// (repodoc.InjectedContent) — never the server's framing; a block carrying no
// delimiter pair (a withheld notice) contributes an empty Text, which
// verification reports as document_text_unavailable.
func personaQuotedDocuments(docs []prompt.InjectedDocument) []planreview.QuotedDocument {
	out := make([]planreview.QuotedDocument, 0, len(docs))
	for _, d := range docs {
		text, _ := repodoc.InjectedContent(d)
		out = append(out, planreview.QuotedDocument{
			Path:        d.Path,
			Commit:      d.Commit,
			ContentHash: d.ContentHash,
			Text:        text,
		})
	}
	return out
}

// applyPersonaConcernControls runs the persona concern-ingest controls on ONE
// successful verdict, in place (every planreview helper copies v.Concerns
// before its first write, so the reviewer adapter's own slice is never
// written). site names the loop for logs ("plan review" / "implement review");
// model is the reviewer model the verdict came back with. It returns the
// verdict the persona controls downgraded FROM (planreview.VerdictReject), or
// "" — the caller merges it with the convention clamp's (first non-empty
// wins).
//
// Order is load-bearing: (1) ClearPersonaIngestMarkers on EVERY verdict, so no
// reviewer-supplied marker survives; (2) for a persona invocation only,
// VerifyQuotedPassages against the persona's injected document set, then
// ClampPersonaSeverities against its remit's severity_cap; (3) the
// reviewer_role stamp on every concern, AFTER the scrub so the stamped value is
// the server's. A standard verdict therefore gets only the scrub and the
// `standard` stamp: its severities and verdict are never touched here.
func (s *Server) applyPersonaConcernControls(ctx context.Context, site string, runID, stageID uuid.UUID, inv reviewerInvocation, model string, v *planreview.ReviewVerdict) planreview.Verdict {
	if v == nil {
		return ""
	}
	if n := planreview.ClearPersonaIngestMarkers(v); n > 0 {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, site+": reviewer verdict pre-set server-internal concern markers — scrubbed at ingest",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("reviewer_model", model),
			slog.String("reviewer_role", inv.reviewerRole()),
			slog.Int("concerns", n),
		)
	}
	var clampedFrom planreview.Verdict
	if p := inv.persona; p != nil {
		qr := planreview.VerifyQuotedPassages(v, personaQuotedDocuments(p.quoteDocs))
		for _, d := range qr.Demoted {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, site+": persona quoted passage unverified — concern demoted to low",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("persona", p.selected.Name),
				slog.String("document_ref", d.DocumentRef),
				slog.String("failure", d.Failure),
				slog.String("from", string(d.From)),
				slog.String("to", string(d.To)),
			)
		}
		cr := planreview.ClampPersonaSeverities(v, planreview.ConcernSeverity(p.severityCap))
		for _, c := range cr.Clamped {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, site+": persona concern severity clamped to the remit's severity_cap",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("persona", p.selected.Name),
				slog.String("category", c.Category),
				slog.String("from", string(c.From)),
				slog.String("to", string(c.To)),
			)
		}
		clampedFrom = qr.VerdictClampedFrom
		if clampedFrom == "" {
			clampedFrom = cr.VerdictClampedFrom
		}
		if clampedFrom != "" {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, site+": persona verdict downgraded at ingest",
				slog.String("run_id", runID.String()),
				slog.String("stage_id", stageID.String()),
				slog.String("persona", p.selected.Name),
				slog.String("verdict_clamped_from", string(clampedFrom)),
				slog.String("verdict", string(v.Verdict)),
			)
		}
	}
	stampReviewerRole(v, inv.reviewerRole())
	return clampedFrom
}

// stampReviewerRole sets ReviewerRole on every concern of v that does not
// already carry role, copying v.Concerns before the first write. Idempotent;
// a nil verdict or an empty role is a no-op.
func stampReviewerRole(v *planreview.ReviewVerdict, role string) {
	if v == nil || role == "" {
		return
	}
	copied := false
	for i := range v.Concerns {
		if v.Concerns[i].ReviewerRole == role {
			continue
		}
		if !copied {
			v.Concerns = append([]planreview.Concern(nil), v.Concerns...)
			copied = true
		}
		v.Concerns[i].ReviewerRole = role
	}
}

// mergeVerdictClampedFrom returns the first non-empty of the convention clamp's
// and the persona controls' verdict_clamped_from.
func mergeVerdictClampedFrom(convention, persona planreview.Verdict) planreview.Verdict {
	if convention != "" {
		return convention
	}
	return persona
}

// reviewerIdentity is a reviewer's identity for the implement-round veto
// (E55.10 / #3755, carried from #3753 item 1): the model string AND the
// normalized reviewer_role. Keying on the model alone let a persona configured
// on the standard reviewer's model confirm-retire the standard reviewer's
// concern while the standard reviewer was rejecting the same round.
type reviewerIdentity struct {
	model string
	role  string
}

// newReviewerIdentity builds a reviewerIdentity with role normalized through
// concern.NormalizedReviewerRole (the empty role of an unattributed legacy row reads as
// standard, so a row raised before personas existed keeps today's behaviour).
func newReviewerIdentity(model, role string) reviewerIdentity {
	return reviewerIdentity{model: model, role: concern.NormalizedReviewerRole(role)}
}

// rowReviewerIdentity is the RAISING reviewer's identity of a concern row.
func rowReviewerIdentity(row *concern.Concern) reviewerIdentity {
	return newReviewerIdentity(derefStr(row.ReviewerModel), row.ReviewerRole)
}
