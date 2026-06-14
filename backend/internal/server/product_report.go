package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/diagnostics"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// maxProductReportRequestBytes caps the product-report request body. The
// slice-2 egress carries product facts only, so the body is tiny; the cap
// is generous headroom for the slice-3 free-text/consent fields layered
// on later.
const maxProductReportRequestBytes = 16 * 1024

// productRepo is the FIXED upstream Fishhawk product repo every product
// report is filed against (#1006, ADR-029 egress posture). The
// destination is not caller-controlled: a product report always lands
// here, regardless of which repo the source run operates on.
const productRepo = "kuhlman-labs/fishhawk"

// categoryProductReportFiled is the source-side audit category written
// when a product report leaves the boundary. It names what crossed
// (fingerprint, destination, created-vs-occurrence) without copying any
// free text. Documented in docs/issue-comment-surfaces.md.
const categoryProductReportFiled = "product_report_filed"

// productReportRequest is the POST /v0/runs/{run_id}/product-reports body.
// Slice 2 (the egress path) carries product facts ONLY: `kind` selects
// the report flavor (bug default; feature for an enhancement request) and
// nothing else crosses. The operator free-text description plus its
// include_free_text consent + redaction are added by slice 3 on top of
// this same endpoint.
type productReportRequest struct {
	Kind string `json:"kind,omitempty"`
}

// productReportResponse echoes what left the boundary so the caller can
// render the outcome without a second fetch.
type productReportResponse struct {
	Fingerprint string `json:"fingerprint"`
	// Action is "created" on a dedup miss (a new fingerprint-marked
	// report) or "occurrence" on a dedup hit (a comment on the existing
	// report; nothing new is created).
	Action      string `json:"action"`
	Number      int    `json:"number"`
	URL         string `json:"url"`
	Destination string `json:"destination"`
}

// handleFileProductReport implements POST /v0/runs/{run_id}/product-reports.
//
// It is the deduped, audited egress path (#1006, slice 2). It collects the
// run's product-facts diagnostic bundle, fingerprints the failure,
// searches the fixed upstream product repo for an open report already
// carrying that fingerprint, and either appends an occurrence comment
// (dedup hit) or files a new fingerprint-marked report (dedup miss) — then
// writes a source-side product_report_filed audit entry naming what left
// the boundary. Egress is gated two ways: only the run's own run-bound
// agent token may drive a report on its chain (the runBoundTokenRunID
// entitlement the work-item path enforces), and a per-repo
// product_feedback kill-switch returns 403 and files nothing.
func (s *Server) handleFileProductReport(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "run_repo_unconfigured",
			"product-report endpoint requires a configured run repository", nil)
		return
	}
	if s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "audit_repo_unconfigured",
			"product-report endpoint requires a configured audit repository", nil)
		return
	}

	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"filing a product report requires an authenticated caller", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	// Entitlement: a product report is an egress on the run's hash chain,
	// so only the run's own run-bound agent token may drive it. A token
	// bound to a different run, an operator token, or a cookie session is
	// rejected — the same gate the work-item audit path enforces.
	tokenRunID, runBound := runBoundTokenRunID(id)
	if !runBound || tokenRunID != runID {
		s.writeError(w, r, http.StatusForbidden, "run_not_entitled",
			"a product report may only be filed by that run's own run-bound agent token",
			map[string]any{"run_id": runID.String()})
		return
	}

	req, ok := s.decodeProductReportRequest(w, r)
	if !ok {
		return
	}

	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return
	}

	conv, err := conventionsLoader(runRow.Repo)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not load work-management conventions", map[string]any{"error": err.Error()})
		return
	}
	// Kill-switch: a per-repo product_feedback.enabled=false files nothing.
	if !conv.ProductFeedbackEnabled() {
		s.writeError(w, r, http.StatusForbidden, "product_feedback_disabled",
			"product feedback is disabled for this repository",
			map[string]any{"repo": runRow.Repo})
		return
	}

	provider, err := workmgmt.GetFeedback(conv.Provider)
	if err != nil {
		var unk *workmgmt.UnknownProviderError
		if errors.As(err, &unk) {
			s.writeError(w, r, http.StatusNotImplemented, "provider_unimplemented",
				unk.Error(),
				map[string]any{"provider": unk.ID, "registered": unk.Known})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not resolve feedback provider", map[string]any{"error": err.Error()})
		return
	}

	stages, err := s.cfg.RunRepo.ListStagesForRun(r.Context(), runID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list stages failed", map[string]any{"error": err.Error()})
		return
	}
	auditEntries, err := s.cfg.AuditRepo.ListForRun(r.Context(), runID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list audit failed", map[string]any{"error": err.Error()})
		return
	}

	bundle := diagnostics.Collect(runRow, stages, auditEntries, currentVersionFacts())
	fingerprint := bundleFingerprint(bundle)

	owner, name, _ := splitRepoFullName(productRepo)
	target := workmgmt.Target{Repo: workmgmt.Repo{Owner: owner, Name: name}}
	// Installation: the source run supplies the installation that can act
	// on the product repo (in dogfooding the run repo IS the product repo).
	if runRow.InstallationID != nil {
		target.InstallationID = *runRow.InstallationID
	}

	existing, err := provider.SearchOpenByFingerprint(r.Context(), target, fingerprint)
	if err != nil {
		s.writeError(w, r, http.StatusBadGateway, "product_report_failed",
			"dedup search against the product repo failed", map[string]any{"error": err.Error()})
		return
	}

	var (
		action    string
		resultNum int
		resultURL string
	)
	if existing != nil {
		// Dedup hit: append an occurrence comment, create nothing.
		if err := provider.AppendOccurrence(r.Context(), target, existing.Number,
			renderOccurrenceComment(bundle, fingerprint)); err != nil {
			s.writeError(w, r, http.StatusBadGateway, "product_report_failed",
				"could not append occurrence to the existing report", map[string]any{"error": err.Error()})
			return
		}
		action, resultNum, resultURL = "occurrence", existing.Number, existing.URL
	} else {
		// Dedup miss: file a new fingerprint-marked report.
		created, err := provider.File(r.Context(), target, workmgmt.FeedbackReport{
			Title:       renderReportTitle(bundle, req.Kind),
			Body:        renderReportBody(bundle, fingerprint),
			Labels:      reportLabels(req.Kind),
			Fingerprint: fingerprint,
		})
		if err != nil {
			s.writeError(w, r, http.StatusBadGateway, "product_report_failed",
				"could not file the product report", map[string]any{"error": err.Error()})
			return
		}
		action, resultNum, resultURL = "created", created.Number, created.URL
	}

	s.auditProductReport(r, runRow, fingerprint, owner+"/"+name, action, resultNum, resultURL, id.Subject)

	s.writeJSON(w, r, http.StatusCreated, productReportResponse{
		Fingerprint: fingerprint,
		Action:      action,
		Number:      resultNum,
		URL:         resultURL,
		Destination: owner + "/" + name,
	})
}

// decodeProductReportRequest reads and validates the request body. An
// empty body is valid (kind defaults to bug). Returns ok=false after
// writing an error response.
func (s *Server) decodeProductReportRequest(w http.ResponseWriter, r *http.Request) (productReportRequest, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxProductReportRequestBytes+1))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return productReportRequest{}, false
	}
	if len(raw) > maxProductReportRequestBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
			"request body exceeds size cap", map[string]any{"limit_bytes": maxProductReportRequestBytes})
		return productReportRequest{}, false
	}
	var req productReportRequest
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body is not valid JSON for a product report",
				map[string]any{"error": err.Error()})
			return productReportRequest{}, false
		}
	}
	switch strings.TrimSpace(req.Kind) {
	case "", "bug":
		req.Kind = "bug"
	case "feature":
		req.Kind = "feature"
	default:
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"kind must be bug or feature", map[string]any{"field": "kind", "got": req.Kind})
		return productReportRequest{}, false
	}
	return req, true
}

// auditProductReport writes the source-side product_report_filed entry.
// It names what left the boundary — fingerprint, destination,
// created-vs-occurrence, and the upstream number/URL — and carries no
// free text. Best-effort: the egress already happened, so a write failure
// is logged, not surfaced.
func (s *Server) auditProductReport(r *http.Request, runRow *run.Run, fingerprint, destination, action string, number int, url, subject string) {
	payload, _ := json.Marshal(map[string]any{
		"fingerprint":  fingerprint,
		"destination":  destination,
		"action":       action,
		"upstream_url": url,
		"upstream_num": number,
	})
	kind := actorKindForSubject(subject)
	subj := subject
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runRow.ID,
		Timestamp:    time.Now().UTC(),
		Category:     categoryProductReportFiled,
		ActorKind:    &kind,
		ActorSubject: &subj,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "append product_report_filed audit",
			slog.String("error", err.Error()),
			slog.String("run_id", runRow.ID.String()),
		)
	}
}

// bundleFingerprint computes the dedup fingerprint from the bundle's
// product facts. The failing stage supplies the error code (failure
// category) and surface; the run state stands in when there is no failing
// stage (e.g. a feature request off a green run). The version family is
// the fishhawkd major.minor.
func bundleFingerprint(b diagnostics.DiagnosticBundle) string {
	errorCode, surface := b.RunState, ""
	if b.FailingStage != nil {
		errorCode = b.FailingStage.FailureCategory
		surface = b.FailingStage.FailureSurface
	}
	return diagnostics.Fingerprint(errorCode, surface, diagnostics.VersionFamily(b.Versions.Fishhawkd.Version))
}

// reportLabels maps the report kind onto upstream labels.
func reportLabels(kind string) []string {
	if kind == "feature" {
		return []string{"type:feature"}
	}
	return []string{"type:bug"}
}

// renderReportTitle builds a product-facts-only title. No operator free
// text is carried in slice 2.
func renderReportTitle(b diagnostics.DiagnosticBundle, kind string) string {
	if b.FailingStage != nil {
		surface := b.FailingStage.FailureSurface
		if surface == "" {
			surface = "category-" + b.FailingStage.FailureCategory
		}
		return fmt.Sprintf("Diagnostic report: %s failure at %s", b.FailingStage.Type, surface)
	}
	if kind == "feature" {
		return fmt.Sprintf("Product feedback: %s run", b.WorkflowID)
	}
	return fmt.Sprintf("Diagnostic report: %s run (%s)", b.WorkflowID, b.RunState)
}

// renderReportBody renders the product facts as a markdown body. It draws
// only from the bundle (no free text by construction) plus the
// fingerprint. The provider appends the hidden fingerprint marker.
func renderReportBody(b diagnostics.DiagnosticBundle, fingerprint string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Auto-collected Fishhawk diagnostic bundle (product facts only).\n\n")
	fmt.Fprintf(&sb, "- run: `%s`\n", b.RunID)
	fmt.Fprintf(&sb, "- workflow: `%s` (spec `%s`)\n", b.WorkflowID, b.WorkflowSpecHash)
	fmt.Fprintf(&sb, "- runner: `%s`\n", b.RunnerKind)
	fmt.Fprintf(&sb, "- run state: `%s`\n", b.RunState)
	if b.FailingStage != nil {
		fmt.Fprintf(&sb, "- failing stage: `%s` (category `%s`",
			b.FailingStage.Type, b.FailingStage.FailureCategory)
		if b.FailingStage.FailureSurface != "" {
			fmt.Fprintf(&sb, ", surface `%s`", b.FailingStage.FailureSurface)
		}
		sb.WriteString(")\n")
	}
	if b.AuditSequenceRange != nil {
		fmt.Fprintf(&sb, "- audit sequence range: `[%d, %d]`\n",
			b.AuditSequenceRange.Min, b.AuditSequenceRange.Max)
	}
	fmt.Fprintf(&sb, "- versions: fishhawkd `%s` (`%s`), min runner `%s`\n",
		b.Versions.Fishhawkd.Version, b.Versions.Fishhawkd.GitSHA, b.Versions.MinRunnerVersion)
	fmt.Fprintf(&sb, "- fingerprint: `%s`\n", fingerprint)
	return sb.String()
}

// renderOccurrenceComment is the body of an occurrence comment appended to
// an existing report on a dedup hit. Product facts only.
func renderOccurrenceComment(b diagnostics.DiagnosticBundle, fingerprint string) string {
	return fmt.Sprintf(
		"Another occurrence of fingerprint `%s`.\n\n- run: `%s`\n- run state: `%s`\n- observed: %s",
		fingerprint, b.RunID, b.RunState, time.Now().UTC().Format(time.RFC3339))
}
