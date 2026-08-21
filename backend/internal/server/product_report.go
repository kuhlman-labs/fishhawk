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
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	"github.com/kuhlman-labs/fishhawk/redaction"
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

// productReportBoardStatus is the board column a newly filed product
// report is placed in. Backlog matches the fishhawk_file_issue path's
// creation default (AGENTS.md: Status MUST be set on creation — a null
// Status breaks the kanban view).
const productReportBoardStatus = "Backlog"

// productReportRequest is the POST /v0/runs/{run_id}/product-reports body.
// `kind` selects the report flavor (bug default; feature for an enhancement
// request). The egress carries product facts ONLY unless the caller sets
// the explicit consent flag: `description` (operator free text) crosses the
// boundary ONLY when `include_free_text` is true, and even then it is run
// through the shared redaction module FIRST (the hard consent/redaction
// contract, #1006 slice 3). Without the flag, `description` is ignored and
// nothing but product facts leaves the boundary.
type productReportRequest struct {
	Kind            string `json:"kind,omitempty"`
	Description     string `json:"description,omitempty"`
	IncludeFreeText bool   `json:"include_free_text,omitempty"`
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
	// Boarded reports whether a NEWLY filed report was placed on the
	// configured project board (#1737). Board placement is best-effort:
	// the report is the durable result, so a placement failure still
	// returns 201.
	Boarded bool `json:"boarded"`
	// BoardingStatus disambiguates boarded=false, which alone cannot tell
	// an operator whether there was nothing to board or whether placement
	// was tried and failed — two different actions for them. One of
	// "boarded", "not_attempted_no_project" (no project is configured;
	// a configuration state, NOT an error), "not_attempted_no_report" (a
	// dedup hit appended an occurrence comment, so nothing was created to
	// board), or "failed".
	BoardingStatus string `json:"boarding_status"`
	// BoardingError carries the placement failure cause. It is set ONLY
	// when BoardingStatus is "failed" — an empty value on the
	// not_attempted_* statuses means "not attempted", never "attempted
	// and succeeded silently".
	BoardingError string `json:"boarding_error,omitempty"`
}

// handleFileProductReport implements POST /v0/runs/{run_id}/product-reports.
//
// It is the deduped, audited egress path (#1006, slice 2). It collects the
// run's product-facts diagnostic bundle, fingerprints the failure,
// searches the fixed upstream product repo for an open report already
// carrying that fingerprint, and either appends an occurrence comment
// (dedup hit) or files a new fingerprint-marked report (dedup miss) — then
// writes a source-side product_report_filed audit entry naming what left
// the boundary.
//
// Egress is gated by a mutually-exclusive entitlement switch (widened in
// #1274 from the run-bound-only gate) plus a per-repo product_feedback
// kill-switch (403, files nothing):
//   - a run-bound agent token (mcp:run:<uuid> subject) may file ONLY on
//     its own run — a token bound to a DIFFERENT run is rejected
//     (run_not_entitled). The run-bound arm is terminal and does NOT
//     require write:runs: run-bound MCP tokens carry mcp:read (never
//     write:runs), so requiring it would break the run's own happy path.
//   - a non-run-bound operator/operator-agent bearer token (TokenID set)
//     must hold write:runs (rejected insufficient_scope otherwise) and may
//     then file for ANY run — operator scope is the deployment (ADR-040).
//   - a cookie-session operator (empty TokenID) is admitted.
//
// The product_report_filed audit names the acting caller via id.Subject +
// actorKindForSubject, so operator/operator-agent provenance is honest
// with no actor.go change.
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

	// Entitlement (mutually-exclusive switch, #1274): the run-bound and
	// operator arms never overlap. A run-bound token may ONLY drive its own
	// run (terminal arm, NOT requiring write:runs — run-bound tokens carry
	// mcp:read, never write:runs); a non-run-bound bearer (operator/
	// operator-agent) must hold write:runs and may then file for any run; an
	// empty-TokenID cookie session is the operator admitted directly.
	tokenRunID, runBound := runBoundTokenRunID(id)
	switch {
	case runBound:
		// A run-bound token may ONLY drive its own run; do NOT require write:runs.
		if tokenRunID != runID {
			s.writeError(w, r, http.StatusForbidden, "run_not_entitled",
				"a product report may only be filed by that run's own run-bound agent token",
				map[string]any{"run_id": runID.String()})
			return
		}
		// else admit: the run's own run-bound agent token (unchanged #1006 happy path).
	case id.TokenID != "":
		// A NON-run-bound bearer token (operator/operator-agent) must hold write:runs.
		if !hasScope(id, "write:runs") {
			s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
				"token is missing required scope: write:runs",
				map[string]any{"required_scope": "write:runs"})
			return
		}
		// else admit: operator/operator-agent token, any run (operator scope = deployment).
	default:
		// Empty TokenID == session-cookie operator -> admit.
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

	conv, err := conventionsLoader(r.Context(), runRow.Repo)
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
			// Static literal message (E67.15 / #2587): the same product-owned
			// facts ride the allow-listed provider/registered detail keys, so
			// nothing is lost, and unk.Error() as the message is a raw-cause
			// syntax the AST guard flags.
			s.writeError(w, r, http.StatusNotImplemented, "provider_unimplemented",
				"the resolved feedback provider is not implemented",
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

	// CollectWithWedge (not Collect): a product report's whole job is to
	// describe WHY a run is stuck, so the auto-drafted body carries the
	// same structured wedge facts the diagnostics read surfaces — red
	// required checks, campaign item state + blocked dependents, a fan-in
	// conflict marker. collectWedgeFacts is best-effort throughout, so a
	// missing fact yields a thinner report, never a failed egress. The
	// dedup fingerprint is deliberately UNAFFECTED (see bundleFingerprint).
	bundle := diagnostics.CollectWithWedge(runRow, stages, auditEntries,
		currentVersionFacts(), s.collectWedgeFacts(r.Context(), runRow, stages))
	fingerprint := bundleFingerprint(bundle)

	// Consent/redaction boundary (binding condition 2): operator free text
	// crosses ONLY when include_free_text is set, and even then it is run
	// through redaction.RedactDefault FIRST. Without the flag, freeText stays
	// empty and the report carries product facts only.
	freeText := redactedFreeText(req)

	owner, name, _ := splitRepoFullName(productRepo)
	// Project: the conventions' board, so a filed report lands on the
	// tracker the same way fishhawk_file_issue's work items do (#1737) —
	// but ONLY when those coordinates are authorized for this fixed
	// destination. See productReportBoard.
	boardProject, boardWithheld := productReportBoard(runRow.Repo, conv.Project)
	target := workmgmt.Target{Repo: workmgmt.Repo{Owner: owner, Name: name}, Project: boardProject}
	// Installation: the source run supplies the installation that can act
	// on the product repo (in dogfooding the run repo IS the product repo).
	if runRow.InstallationID != nil {
		target.Scope = forge.FromGitHubInstallationID(*runRow.InstallationID)
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
		created   *workmgmt.CreatedItem
	)
	if existing != nil {
		// Dedup hit: append an occurrence comment, create nothing.
		if err := provider.AppendOccurrence(r.Context(), target, existing.Number,
			renderOccurrenceComment(bundle, fingerprint, freeText)); err != nil {
			s.writeError(w, r, http.StatusBadGateway, "product_report_failed",
				"could not append occurrence to the existing report", map[string]any{"error": err.Error()})
			return
		}
		action, resultNum, resultURL = "occurrence", existing.Number, existing.URL
	} else {
		// Dedup miss: file a new fingerprint-marked report.
		created, err = provider.File(r.Context(), target, workmgmt.FeedbackReport{
			Title:          renderReportTitle(bundle, req.Kind),
			Body:           renderReportBody(bundle, fingerprint, freeText),
			Labels:         reportLabels(req.Kind),
			Fingerprint:    fingerprint,
			BoardPlacement: workmgmt.BoardPlacement{Status: productReportBoardStatus},
		})
		if err != nil {
			s.writeError(w, r, http.StatusBadGateway, "product_report_failed",
				"could not file the product report", map[string]any{"error": err.Error()})
			return
		}
		action, resultNum, resultURL = "created", created.Number, created.URL
	}

	s.auditProductReport(r, runRow, fingerprint, owner+"/"+name, action, resultNum, resultURL, id.Subject)

	resp := productReportResponse{
		Fingerprint:    fingerprint,
		Action:         action,
		Number:         resultNum,
		URL:            resultURL,
		Destination:    owner + "/" + name,
		BoardingStatus: workmgmt.BoardingStatusOf(target, created),
	}
	rawBoardingCause := ""
	if created != nil {
		resp.Boarded = created.Boarded
		rawBoardingCause = created.BoardingError
	}
	if boardWithheld && created != nil {
		// A board WAS configured; this handler declined to use it. Say so
		// rather than reporting "no project configured", which would send
		// an operator to add a board they already have.
		resp.BoardingStatus = workmgmt.BoardingStatusNotAttemptedProjectNotAuthorized
	}
	if resp.BoardingStatus == workmgmt.BoardingStatusFailed {
		// Amendment condition 3: a best-effort failure that is swallowed is
		// indistinguishable from silently-broken. Name the cause server-side
		// too, so an operator seeing boarded=false has a log line as well as
		// the response field. The log gets the RAW cause; the wire does not
		// (see safeBoardingCause).
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "product report board placement failed",
			slog.String("error", rawBoardingCause),
			slog.String("cause", safeBoardingCause(rawBoardingCause)),
			slog.String("run_id", runRow.ID.String()),
			slog.Int("upstream_num", resultNum),
		)
		resp.BoardingError = safeBoardingCause(rawBoardingCause)
	}
	// Every other status stays cause-free by construction: an empty cause
	// is only meaningful on `failed`, so an operator never has to guess
	// which reading applies. (resp.BoardingError is only ever assigned on
	// the failed arm above, so this is a positive re-assertion of that
	// invariant rather than a cleanup of a value already set.)
	if resp.BoardingStatus != workmgmt.BoardingStatusFailed {
		resp.BoardingError = ""
	}
	s.writeJSON(w, r, http.StatusCreated, resp)
}

// productReportBoard decides which project a filed product report may be
// placed on, and reports whether a configured board was WITHHELD.
//
// A product report always lands in the FIXED product tracker
// (productRepo), but the board coordinates come from the SOURCE run's
// repo conventions — a different repo, whose `.fishhawk` config this
// deployment does not own. Handing those coordinates straight to
// placeIssueOnBoard would let any repo Fishhawk runs against name an
// arbitrary Projects-v2 board and have this server write to it under a
// privileged credential — and for a user-owned board that credential is
// the operator's static FISHHAWKD_PROJECTS_TOKEN (#1114), not the run's
// scoped installation token. So the destination must be independently
// authorized, and the binding is the narrowest one that still serves the
// dogfood case the feature was built for: the coordinates are honored
// only when they came from the DESTINATION repo's own conventions, i.e.
// the source run's repo IS the product tracker.
//
// Returns (nil, true) when a board was configured but refused, so the
// caller can report that distinctly from "no board configured" — those
// are different facts and only one of them is worth acting on.
func productReportBoard(sourceRepo string, project *workmgmt.Project) (*workmgmt.Project, bool) {
	if project == nil {
		return nil, false
	}
	// GitHub owner/name comparison is case-insensitive.
	if !strings.EqualFold(strings.TrimSpace(sourceRepo), productRepo) {
		return nil, true
	}
	return project, false
}

// safeBoardingCause maps a raw placement error onto the closed
// workmgmt.BoardingCause* set for the RESPONSE body.
//
// The 201 body is not covered by writeError's default-deny detail
// allow-list, so without this a raw provider error would cross to the
// caller on a success response — carrying third-party API text and, on
// the unknown-status path, the project's Status field OPTION NAMES,
// which are private board metadata reachable only through the operator's
// privileged token. The raw cause still reaches the operator through the
// WARN log this function's caller emits.
//
// Classification is by the marker substrings placeIssueOnBoard's own
// wrapper messages carry, and the default arm is FAIL-SAFE: anything
// unrecognized reports BoardingCauseUnclassified rather than falling
// back to echoing the input, so a reworded or newly added provider error
// degrades to a vaguer literal and never to a disclosure.
func safeBoardingCause(raw string) string {
	switch {
	case strings.TrimSpace(raw) == "":
		// A `failed` verdict with no cause recorded: the provider
		// misbehaved. Synthesize a cause so `failed` never reaches the
		// wire cause-free, which the schema and the README both promise.
		return workmgmt.BoardingCauseUnreported
	case strings.Contains(raw, "resolve project fields"):
		return workmgmt.BoardingCauseProjectFieldsUnavailable
	case strings.Contains(raw, "add project item"):
		return workmgmt.BoardingCauseAddItemFailed
	case strings.Contains(raw, "option on the project"):
		return workmgmt.BoardingCauseStatusOptionUnknown
	case strings.Contains(raw, "set status field"):
		return workmgmt.BoardingCauseSetStatusFailed
	default:
		return workmgmt.BoardingCauseUnclassified
	}
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
// category), surface, and detail class; the run state stands in for the
// error code when there is no failing stage (e.g. a feature request off a
// green run, where the detail class is "" — the surface fallback). The
// detail class distinguishes distinct root causes sharing a surface
// (#1962); an empty class reproduces the pre-change fingerprint. The
// version family is the fishhawkd major.minor.
func bundleFingerprint(b diagnostics.DiagnosticBundle) string {
	errorCode, surface, detailClass := b.RunState, "", ""
	if b.FailingStage != nil {
		errorCode = b.FailingStage.FailureCategory
		surface = b.FailingStage.FailureSurface
		detailClass = b.FailingStage.FailureDetailClass
	}
	return diagnostics.Fingerprint(errorCode, surface, detailClass, diagnostics.VersionFamily(b.Versions.Fishhawkd.Version))
}

// redactedFreeText returns the operator free text that may cross the egress
// boundary. It returns "" unless include_free_text consent is set AND the
// description is non-empty; on consent the description is run through
// redaction.RedactDefault so any embedded secrets are scrubbed before they
// leave the boundary. This is the single chokepoint for the consent +
// redaction contract — both render paths draw from its output, never from
// req.Description directly.
func redactedFreeText(req productReportRequest) string {
	if !req.IncludeFreeText {
		return ""
	}
	if strings.TrimSpace(req.Description) == "" {
		return ""
	}
	scrubbed, _ := redaction.RedactDefault([]byte(req.Description))
	return string(scrubbed)
}

// reportLabels maps the report kind onto upstream labels. This path files
// through the feedback provider (workmgmt.GetFeedback), NOT workmgmt.Apply, so
// the label-completeness guarantee (#1616) cannot reach it there — the
// autonomy:medium default is applied HERE, at the label source, so no product
// report (feature or bug) is ever filed autonomy-unset, matching the Apply-pass
// guarantee for the file_issue / defer_concern paths.
func reportLabels(kind string) []string {
	if kind == "feature" {
		return []string{"type:feature", "autonomy:medium"}
	}
	return []string{"type:bug", "autonomy:medium"}
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
// from the bundle (product facts) plus the fingerprint, and appends the
// redaction-scrubbed operator free text ONLY when freeText is non-empty
// (the caller passes "" unless include_free_text consent was given). The
// provider appends the hidden fingerprint marker.
func renderReportBody(b diagnostics.DiagnosticBundle, fingerprint, freeText string) string {
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
		if b.FailingStage.FailureDetailClass != "" {
			fmt.Fprintf(&sb, ", detail class `%s`", b.FailingStage.FailureDetailClass)
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
	sb.WriteString(renderWedgeLines(b.WedgeContext))
	if freeText != "" {
		fmt.Fprintf(&sb, "\n## Operator notes (redacted)\n\n%s\n", freeText)
	}
	return sb.String()
}

// renderWedgeLines renders the bundle's wedge block as markdown list
// items under a heading, or "" when the run carries no wedge context (the
// healthy-run case — the body is then byte-identical to the pre-#1737
// rendering). The block names WHY the run is stuck: which required checks
// are red, the campaign item's state and how many siblings it blocks, and
// a fan-in conflict marker. Every value is a structured product fact the
// diagnostics package built from typed state, never free text.
func renderWedgeLines(wc *diagnostics.WedgeContext) string {
	if wc == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n## Wedge context\n\n")
	if len(wc.BlockingChecks) > 0 {
		fmt.Fprintf(&sb, "- blocking required checks: `%s`\n", strings.Join(wc.BlockingChecks, "`, `"))
	}
	if wc.CampaignItemState != "" {
		fmt.Fprintf(&sb, "- campaign item state: `%s`\n", wc.CampaignItemState)
	}
	if wc.BlockedDependents > 0 {
		fmt.Fprintf(&sb, "- blocked dependents: `%d`\n", wc.BlockedDependents)
	}
	if wc.IntegrateWaveError != "" {
		fmt.Fprintf(&sb, "- fan-in error: `%s`\n", wc.IntegrateWaveError)
	}
	return sb.String()
}

// wedgeSummaryLine is the ONE-line wedge summary an occurrence comment
// carries — enough for an operator scanning recurrences to see whether
// this occurrence is wedged the same way, without repeating the full
// block. Empty when there is no wedge context.
func wedgeSummaryLine(wc *diagnostics.WedgeContext) string {
	if wc == nil {
		return ""
	}
	parts := make([]string, 0, 4)
	if len(wc.BlockingChecks) > 0 {
		parts = append(parts, fmt.Sprintf("blocking checks `%s`", strings.Join(wc.BlockingChecks, "`, `")))
	}
	if wc.CampaignItemState != "" {
		parts = append(parts, fmt.Sprintf("campaign item `%s`", wc.CampaignItemState))
	}
	if wc.BlockedDependents > 0 {
		parts = append(parts, fmt.Sprintf("`%d` blocked dependents", wc.BlockedDependents))
	}
	if wc.IntegrateWaveError != "" {
		parts = append(parts, fmt.Sprintf("fan-in `%s`", wc.IntegrateWaveError))
	}
	if len(parts) == 0 {
		return ""
	}
	return "\n- wedge: " + strings.Join(parts, "; ")
}

// renderOccurrenceComment is the body of an occurrence comment appended to
// an existing report on a dedup hit. Product facts only, plus the
// redaction-scrubbed operator free text when consent was given (freeText
// non-empty).
func renderOccurrenceComment(b diagnostics.DiagnosticBundle, fingerprint, freeText string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb,
		"Another occurrence of fingerprint `%s`.\n\n- run: `%s`\n- run state: `%s`\n- observed: %s",
		fingerprint, b.RunID, b.RunState, time.Now().UTC().Format(time.RFC3339))
	sb.WriteString(wedgeSummaryLine(b.WedgeContext))
	if freeText != "" {
		fmt.Fprintf(&sb, "\n\n## Operator notes (redacted)\n\n%s\n", freeText)
	}
	return sb.String()
}
