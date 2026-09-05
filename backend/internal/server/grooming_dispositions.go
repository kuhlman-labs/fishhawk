package server

// Per-entry grooming disposition CAPTURE (E54.30 / #2843).
//
// An operator records a verdict — approved / rejected / amended, plus an
// optional close_target — against an INDIVIDUAL grooming-report entry, keyed by
// the entry's stable DERIVED id, and it persists as an auditable fact under its
// own audit category.
//
// THE CATEGORY IS DELIBERATELY DISTINCT. grooming_disposition_recorded is what
// the OPERATOR DECIDED; grooming_mutation_applied (workmgmt/grooming_apply.go)
// is what was APPLIED. The second derives from the first, and collapsing them
// would make "the operator approved this and the apply then failed" indistinguish-
// able from "the operator never decided", which is exactly the state the churn
// guard's baseline reads.
//
// THESE ROWS ARE NOW CONSUMED (E54.48 / #2991). The on-approval apply hook
// (grooming_apply.go) reads the dispositions recorded here and applies them: an
// explicit `approved` populates the per-entry gate approval that unlocks a gated
// destructive class, `rejected` skips, `amended` is not an approval path, and an
// undispositioned HYGIENE entry still auto-applies while an undispositioned
// gated one does not. The capture WINDOW is closed at settlement: the apply hook
// appends an artifact-bound grooming_apply_window_closed WATERMARK (audit layer),
// after which a capture for that artifact is refused 409 grooming_window_closed.
// The capture path here appends the whole batch in ONE transaction under the
// run-row lock (audit.GroomingWindowAppender) so a capture returns success only
// if its dispositions are still consumable; it falls back to a per-row loop for
// in-memory repositories that lack the capability.
//
// CAPTURE IS OPERATOR-ONLY, AND THAT MEANS TWO REFUSALS:
//
//   - a RUN-BOUND agent token ("mcp:run:<uuid>" subject) is refused outright,
//     even for its own run — the posture merge_run.go and vouch.go take;
//   - a DELEGATED OPERATOR-AGENT token ("operator-agent/" subject prefix) is
//     ALSO refused. This one goes past what the issue asked and is deliberate:
//     the grooming report is authored by an agent, so an agent that could also
//     disposition it would convert an operator gate into a self-approval.
//
// The READ-BACK (GET) requires read access only. The issue states the
// operator-only requirement for CAPTURE, and the underlying grooming_report
// artifact is already agent-readable, so refusing the read would add a failure
// mode with no matching hazard.
//
// The long-form contract — the eight-rung ladder, batch-atomic validation, the
// newest-artifact resolution rule, last-wins supersession — is in
// backend/internal/server/README.md.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// CategoryGroomingDispositionRecorded is the audit category appended once per
// entry disposition an operator records (#2843). What is load-bearing is that
// the VALUE is registered in audit.KnownCategories — GET /v0/runs/{id}/audit?category=
// and fishhawk_await_audit reject an unregistered category with a 400, and
// categories_completeness_test.go's AST sweep fails the build on any category
// backend code emits but the registry omits.
//
// It ALIASES audit.GroomingDispositionRecordedCategory so the capture handler
// and the #2991 audit-layer settlement scan (which lists this category to build
// a window's consumed set) share ONE source of truth rather than two literals
// that could drift.
const CategoryGroomingDispositionRecorded = audit.GroomingDispositionRecordedCategory

// groomingDispositionInput is one requested disposition.
type groomingDispositionInput struct {
	EntryID     string `json:"entry_id"`
	Verdict     string `json:"verdict"`
	CloseTarget string `json:"close_target,omitempty"`
}

// groomingDispositionRequest is the POST body: a batch of dispositions,
// validated ATOMICALLY (see handleRecordGroomingDispositions).
type groomingDispositionRequest struct {
	Dispositions []groomingDispositionInput `json:"dispositions"`
}

// recordedGroomingDisposition is one projected disposition — the read-back row.
// AuditSequence is the chain sequence of the row that WON the last-wins
// collapse, so a reader can order captures without re-listing the chain.
type recordedGroomingDisposition struct {
	EntryID       string `json:"entry_id"`
	EntryClass    string `json:"entry_class"`
	Verdict       string `json:"verdict"`
	CloseTarget   string `json:"close_target,omitempty"`
	RecordedAt    string `json:"recorded_at"`
	RecordedBy    string `json:"recorded_by"`
	AuditSequence int64  `json:"audit_sequence"`
}

// groomingDispositionsResponse is the body BOTH verbs return: the resolved
// report artifact the dispositions attach to, plus the full projected set. The
// POST returns the projection too, so the MCP verb gets its read-back in one
// call.
type groomingDispositionsResponse struct {
	RunID       string `json:"run_id"`
	ArtifactID  string `json:"artifact_id"`
	StageID     string `json:"stage_id"`
	ContentHash string `json:"content_hash"`
	// WindowClosed is true once the apply hook has settled this artifact's
	// capture window (#2991). After it a capture for this artifact is refused.
	WindowClosed bool `json:"window_closed"`
	// Settlement carries the watermark's facts when WindowClosed is true, so an
	// operator who recorded a rejection reads back the settlement rather than an
	// empty set and concludes their capture failed. Nil while the window is open.
	Settlement   *groomingWindowSettlement     `json:"settlement,omitempty"`
	Dispositions []recordedGroomingDisposition `json:"dispositions"`
}

// groomingWindowSettlement is the read-back projection of the artifact's closing
// watermark (#2991).
type groomingWindowSettlement struct {
	Settlement    string `json:"settlement"`
	ClosedAt      string `json:"closed_at"`
	AuditSequence int64  `json:"audit_sequence"`
}

// groomingWindowPayload is the audit-row payload of a grooming_apply_window_closed
// watermark. Marshalled BARE, matching the disposition rows' shape, so the audit
// layer decodes artifact_id / settlement straight off the payload
// (audit.groomingWatermarkArtifactID / groomingWatermarkSettlement).
type groomingWindowPayload struct {
	RunID      string `json:"run_id"`
	StageID    string `json:"stage_id"`
	ArtifactID string `json:"artifact_id"`
	Settlement string `json:"settlement"`
	ClosedAt   string `json:"closed_at"`
}

// groomingDispositionPayload is the audit-row payload. Marshalled BARE (its own
// json tags, no run/stage envelope) so a reader decodes entry_id / verdict /
// close_target straight off the payload, matching the shape
// priorGroomingDispositions already reads for the apply family.
type groomingDispositionPayload struct {
	RunID       string `json:"run_id"`
	StageID     string `json:"stage_id"`
	ArtifactID  string `json:"artifact_id"`
	ContentHash string `json:"content_hash"`
	EntryID     string `json:"entry_id"`
	EntryClass  string `json:"entry_class"`
	Verdict     string `json:"verdict"`
	CloseTarget string `json:"close_target,omitempty"`
}

// groomingVerdicts is the CLOSED set a disposition verdict must name. It is the
// workmgmt verdict domain verbatim — the same three constants the apply layer
// keys on — so capture and (eventually) apply cannot drift on the vocabulary.
var groomingVerdicts = []workmgmt.GroomingVerdict{
	workmgmt.GroomingApproved,
	workmgmt.GroomingRejected,
	workmgmt.GroomingAmended,
}

// groomingVerdictNames renders the closed set for an error's details block.
func groomingVerdictNames() []string {
	out := make([]string, 0, len(groomingVerdicts))
	for _, v := range groomingVerdicts {
		out = append(out, string(v))
	}
	return out
}

// isGroomingVerdict reports whether raw names one of the three verdicts.
func isGroomingVerdict(raw string) bool {
	for _, v := range groomingVerdicts {
		if raw == string(v) {
			return true
		}
	}
	return false
}

// errGroomingReportAbsent is the sentinel for "this run genuinely shipped no
// grooming_report artifact" — distinct from a read/parse FAILURE, which returns
// a real error. Collapsing the two is how a capture would silently attach to
// the wrong report (or to none) while reporting success.
var errGroomingReportAbsent = errors.New("run carries no grooming_report artifact")

// newestGroomingReportArtifact resolves the run's NEWEST grooming_report
// artifact and parses it.
//
// The selection is TOTAL and deterministic: maximum by (CreatedAt, ID string),
// so two artifacts written in the same clock tick still order stably regardless
// of repository return order. The tiebreak matters because BOTH verbs resolve
// through this function — that is what makes POST and GET agree on WHICH report
// a capture attaches to by construction rather than by convention.
//
// Three outcomes, kept distinct exactly as priorGroomingReport keeps them:
// found; genuinely absent (errGroomingReportAbsent); read-or-parse failure (a
// wrapped error).
func (s *Server) newestGroomingReportArtifact(ctx context.Context, runID uuid.UUID) (*artifact.Artifact, *plan.GroomingReport, *run.Stage, error) {
	stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, runID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list stages for run %s: %w", runID, err)
	}
	var (
		best      *artifact.Artifact
		bestStage *run.Stage
	)
	for _, st := range stages {
		if st == nil || st.Type != run.StageTypePlan {
			continue
		}
		arts, aerr := s.cfg.ArtifactRepo.ListForStage(ctx, st.ID)
		if aerr != nil {
			return nil, nil, nil, fmt.Errorf("list artifacts for stage %s: %w", st.ID, aerr)
		}
		for _, a := range arts {
			if a == nil || a.Kind != artifact.KindGroomingReport {
				continue
			}
			if best == nil || newerGroomingArtifact(a, best) {
				best, bestStage = a, st
			}
		}
	}
	if best == nil {
		return nil, nil, nil, errGroomingReportAbsent
	}
	report, perr := plan.ParseGroomingReport(best.Content)
	if perr != nil {
		return nil, nil, nil, fmt.Errorf("parse grooming_report artifact %s: %w", best.ID, perr)
	}
	return best, report, bestStage, nil
}

// newerGroomingArtifact is the total order the resolver maximizes over:
// CreatedAt first, then the id string as a deterministic tiebreak.
func newerGroomingArtifact(candidate, incumbent *artifact.Artifact) bool {
	if candidate.CreatedAt.After(incumbent.CreatedAt) {
		return true
	}
	if candidate.CreatedAt.Before(incumbent.CreatedAt) {
		return false
	}
	return candidate.ID.String() > incumbent.ID.String()
}

// handleRecordGroomingDispositions implements
// POST /v0/runs/{run_id}/grooming-dispositions.
//
// EVERY rung is evaluated BEFORE any write, and the WHOLE batch is validated
// before ANY audit row is appended. That batch-atomic ordering is the control
// that makes a partially-recorded capture unreachable: a request naming one
// unknown entry id records NOTHING, rather than landing the ids that happened
// to precede the bad one.
//
// The ladder, in order:
//
//	G0 503 grooming_dispositions_unconfigured — repositories not wired.
//	G1 401 authentication_required           — anonymous.
//	G2 403 run_token_forbidden               — a run-bound agent token, even
//	                                           for its OWN run.
//	G3 403 operator_agent_forbidden          — a delegated operator-agent token.
//	G4 403 insufficient_scope                — missing write:approvals, enforced
//	                                           UNCONDITIONALLY (no cookie-session
//	                                           bypass), the merge_run.go posture.
//	G5 400 validation_failed                 — unparseable body, trailing
//	                                           content after the object, empty
//	                                           batch, empty entry_id,
//	                                           intra-batch dup.
//	G6 400 grooming_verdict_invalid          — verdict outside the closed set.
//	G7 409 grooming_report_absent / 500      — no report vs unreadable report.
//	G8 422 grooming_entry_unknown            — an id the report does not declare.
func (s *Server) handleRecordGroomingDispositions(w http.ResponseWriter, r *http.Request) {
	// G0: unconfigured. Checked first so an unwired deployment reports the
	// wiring fault rather than an auth verdict it cannot act on.
	if s.cfg.RunRepo == nil || s.cfg.ArtifactRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "grooming_dispositions_unconfigured",
			"grooming-dispositions endpoint requires run + artifact + audit repositories", nil)
		return
	}

	id := IdentityFrom(r.Context())
	// G1.
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	// G2: a run-bound agent token may NEVER record a disposition — not even for
	// its own run. The agent authored the report; dispositioning it would be a
	// self-approval. Mirrors merge_run.go / vouch.go.
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not record a grooming disposition; deciding a grooming proposal is an operator action",
			nil)
		return
	}
	// G3: a DELEGATED operator-agent token is refused too. Keyed on
	// operatorrole.IsTokenSubject — the same predicate actor.go already uses to
	// classify a delegated writer as actor_kind=agent — so this is a reuse of
	// the existing notion of agent identity, not a new one.
	if operatorrole.IsTokenSubject(id.Subject) {
		s.writeError(w, r, http.StatusForbidden, "operator_agent_forbidden",
			"a delegated operator-agent token may not record a grooming disposition; a per-entry grooming verdict is a human judgment, and an agent recording it would convert the operator gate into a self-approval",
			map[string]any{"subject": id.Subject})
		return
	}
	// G4: write:approvals, enforced UNCONDITIONALLY. This deliberately does NOT
	// mirror the sibling `id.TokenID != ""` guard that waves operator
	// cookie-session identities past the scope gate — a disposition is an
	// approval-class judgment, the same reasoning merge_run.go states for the
	// merge verdict.
	if !hasScope(id, "write:approvals") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:approvals",
			map[string]any{"required_scope": "write:approvals"})
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	// G5: body shape.
	var reqBody groomingDispositionRequest
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		decErr := dec.Decode(&reqBody)
		switch {
		case decErr != nil && !errors.Is(decErr, io.EOF):
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {dispositions:[{entry_id, verdict, close_target}]}",
				map[string]any{"error": decErr.Error()})
			return
		case decErr == nil:
			// G5': the body must be ONE JSON document and nothing else.
			//
			// json.Decoder stops after the first value, so without this a body
			// of two concatenated batches decodes the first, DISCARDS the
			// second, and returns 200 — a success response for a capture that
			// recorded half of what the operator sent, which is precisely the
			// silent-partial-capture failure this endpoint exists to prevent.
			// The refusal sits inside G5, ahead of every write, so a rejected
			// body still records nothing.
			//
			// Trailing WHITESPACE is accepted: a trailing newline is what every
			// curl heredoc and most HTTP clients send. A second Decode returns
			// io.EOF for whitespace-only remainder, a nil error for a second
			// value, and a syntax error for garbage — so io.EOF is exactly the
			// accept condition.
			var trailing json.RawMessage
			if tErr := dec.Decode(&trailing); !errors.Is(tErr, io.EOF) {
				s.writeError(w, r, http.StatusBadRequest, "validation_failed",
					"request body must be a single JSON document; trailing content after the dispositions object is refused because a decoder that stopped at the first value would silently discard it and report success",
					map[string]any{"field": "body"})
				return
			}
		}
	}
	if len(reqBody.Dispositions) == 0 {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"dispositions must name at least one entry; an empty capture records nothing and is ambiguous intent",
			map[string]any{"field": "dispositions"})
		return
	}
	seen := make(map[string]struct{}, len(reqBody.Dispositions))
	for i := range reqBody.Dispositions {
		entryID := strings.TrimSpace(reqBody.Dispositions[i].EntryID)
		if entryID == "" {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"entry_id is required on every disposition",
				map[string]any{"field": fmt.Sprintf("dispositions[%d].entry_id", i)})
			return
		}
		if _, dup := seen[entryID]; dup {
			// An intra-batch duplicate is ambiguous intent, NOT a supersession:
			// one request carrying two verdicts for one entry states no verdict.
			// (Supersession across SEPARATE requests is supported — last wins.)
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"entry_id appears more than once in this batch; one request carrying two verdicts for one entry is ambiguous — record the correcting verdict as a separate request, which supersedes the earlier one",
				map[string]any{"field": fmt.Sprintf("dispositions[%d].entry_id", i), "entry_id": entryID})
			return
		}
		seen[entryID] = struct{}{}
		reqBody.Dispositions[i].EntryID = entryID

		// G6: the verdict is one of the three.
		verdict := strings.TrimSpace(reqBody.Dispositions[i].Verdict)
		if !isGroomingVerdict(verdict) {
			s.writeError(w, r, http.StatusBadRequest, "grooming_verdict_invalid",
				"verdict must name one of the three grooming verdicts",
				map[string]any{
					"field": fmt.Sprintf("dispositions[%d].verdict", i),
					"got":   verdict, "allowed": groomingVerdictNames(),
				})
			return
		}
		reqBody.Dispositions[i].Verdict = verdict
		reqBody.Dispositions[i].CloseTarget = strings.TrimSpace(reqBody.Dispositions[i].CloseTarget)
	}

	// G7: resolve the report. Absent and unreadable stay DISTINCT.
	art, report, stage, rerr := s.newestGroomingReportArtifact(r.Context(), runID)
	if rerr != nil {
		if errors.Is(rerr, errGroomingReportAbsent) {
			s.writeError(w, r, http.StatusConflict, "grooming_report_absent",
				"this run carries no grooming_report artifact; there is nothing to disposition",
				map[string]any{"run_id": runID.String()})
			return
		}
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"grooming-dispositions: resolving the newest grooming_report failed",
			slog.String("run_id", runID.String()), slog.String("error", rerr.Error()))
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"the run's grooming_report artifact could not be read or parsed",
			map[string]any{"error": rerr.Error()})
		return
	}

	// G8: every entry id must be one the report DECLARES. Validated for the
	// WHOLE batch before any append — this loop is what makes the capture
	// batch-atomic.
	classes := plan.GroomingEntryClasses(report)
	var unknown []string
	for _, d := range reqBody.Dispositions {
		if _, ok := classes[d.EntryID]; !ok {
			unknown = append(unknown, d.EntryID)
		}
	}
	if len(unknown) > 0 {
		s.writeError(w, r, http.StatusUnprocessableEntity, "grooming_entry_unknown",
			"one or more entry_id values are not declared by this run's newest grooming_report; NO disposition was recorded",
			map[string]any{"unknown_entry_ids": unknown, "artifact_id": art.ID.String()})
		return
	}

	// Past all eight rungs: MARSHAL EVERY payload first (so a marshal failure
	// happens before any append), then drive the ATOMIC batch path when the repo
	// carries the #2991 capability, falling back to the per-row loop for
	// in-memory repositories that do not.
	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := audit.ActorUser
	stageID := stage.ID
	now := time.Now().UTC()
	params := make([]audit.ChainAppendParams, 0, len(reqBody.Dispositions))
	for _, d := range reqBody.Dispositions {
		payload, merr := json.Marshal(groomingDispositionPayload{
			RunID: runID.String(), StageID: stageID.String(),
			ArtifactID: art.ID.String(), ContentHash: art.ContentHash,
			EntryID: d.EntryID, EntryClass: classes[d.EntryID],
			Verdict: d.Verdict, CloseTarget: d.CloseTarget,
		})
		if merr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"marshal disposition payload failed", map[string]any{"error": merr.Error()})
			return
		}
		params = append(params, audit.ChainAppendParams{
			RunID:        runID,
			StageID:      &stageID,
			Timestamp:    now,
			Category:     CategoryGroomingDispositionRecorded,
			ActorKind:    &actorKind,
			ActorSubject: &subject,
			Payload:      payload,
		})
	}

	if appender, ok := s.cfg.AuditRepo.(audit.GroomingWindowAppender); ok {
		// ATOMIC BATCH: one capture is one transaction. A closed window refuses
		// the whole batch (nothing recorded); any other append failure rolls the
		// WHOLE batch back, so the 500 body says nothing was recorded rather than
		// reporting a partial count.
		_, aerr := appender.AppendChainedGroomingDispositionBatch(r.Context(), art.ID.String(), params)
		var closed *audit.GroomingWindowClosedError
		switch {
		case errors.As(aerr, &closed):
			s.writeError(w, r, http.StatusConflict, "grooming_window_closed",
				"this grooming report's disposition-capture window has been settled; NO disposition was recorded and the dispositions you sent are not consumable",
				map[string]any{
					"artifact_id":        closed.ArtifactID,
					"settlement":         closed.Settlement,
					"watermark_sequence": closed.Sequence,
				})
			return
		case aerr != nil:
			s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
				"grooming-dispositions: atomic disposition batch failed",
				slog.String("run_id", runID.String()), slog.String("error", aerr.Error()))
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"recording the disposition batch failed; the capture is atomic, so NOTHING was recorded and a repeat POST is safe",
				map[string]any{"error": aerr.Error()})
			return
		}
	} else {
		// FALLBACK (in-memory repos without the capability): per-row loop. This
		// path is non-atomic, so a mid-batch failure can leave durable partial
		// rows; `recorded`/`requested` reach the caller via errors.go's 5xx
		// allow-list and are the operator's only evidence of what survived. A
		// repeat POST is safe because capture is last-wins.
		for n := range params {
			if _, aerr := s.cfg.AuditRepo.AppendChained(r.Context(), params[n]); aerr != nil {
				s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
					"grooming-dispositions: append grooming_disposition_recorded failed",
					slog.String("run_id", runID.String()), slog.String("error", aerr.Error()))
				s.writeError(w, r, http.StatusInternalServerError, "internal_error",
					"recording the disposition batch failed part-way; the rows already appended are durable and a repeat POST is safe (capture is last-wins)",
					map[string]any{"recorded": n, "requested": len(params), "error": aerr.Error()})
				return
			}
		}
	}

	s.respondGroomingDispositions(w, r, runID, art, stage, report)
}

// handleListGroomingDispositions implements
// GET /v0/runs/{run_id}/grooming-dispositions: the read-back of every recorded
// disposition for the run's NEWEST grooming_report artifact.
//
// Read access only — see the file header for why the operator-only posture is
// scoped to capture.
func (s *Server) handleListGroomingDispositions(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RunRepo == nil || s.cfg.ArtifactRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "grooming_dispositions_unconfigured",
			"grooming-dispositions endpoint requires run + artifact + audit repositories", nil)
		return
	}
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}
	art, report, stage, rerr := s.newestGroomingReportArtifact(r.Context(), runID)
	if rerr != nil {
		if errors.Is(rerr, errGroomingReportAbsent) {
			s.writeError(w, r, http.StatusConflict, "grooming_report_absent",
				"this run carries no grooming_report artifact; there is nothing to disposition",
				map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"the run's grooming_report artifact could not be read or parsed",
			map[string]any{"error": rerr.Error()})
		return
	}
	s.respondGroomingDispositions(w, r, runID, art, stage, report)
}

// respondGroomingDispositions reads the run's disposition rows, projects them,
// and writes the 200. Shared by both verbs so the POST's echo and the GET's
// read-back are the SAME bytes by construction.
func (s *Server) respondGroomingDispositions(w http.ResponseWriter, r *http.Request,
	runID uuid.UUID, art *artifact.Artifact, stage *run.Stage, report *plan.GroomingReport) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(r.Context(), runID, CategoryGroomingDispositionRecorded)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"listing recorded dispositions failed", map[string]any{"error": err.Error()})
		return
	}
	// The capture window (#2991). Pre-watermark dispositions are still projected
	// UNCHANGED below — nothing is voided or hidden — so an operator who recorded
	// a rejection reads back their capture plus the settlement, never an empty set.
	settlement, serr := s.groomingWindowSettlementFor(r.Context(), runID, art.ID.String())
	if serr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"listing the grooming capture window failed", map[string]any{"error": serr.Error()})
		return
	}
	s.writeJSON(w, r, http.StatusOK, groomingDispositionsResponse{
		RunID:        runID.String(),
		ArtifactID:   art.ID.String(),
		StageID:      stage.ID.String(),
		ContentHash:  art.ContentHash,
		WindowClosed: settlement != nil,
		Settlement:   settlement,
		Dispositions: projectGroomingDispositions(entries, art.ID.String(), plan.GroomingEntryClasses(report)),
	})
}

// groomingWindowSettlementFor resolves the LOWEST-sequence grooming_apply_window_closed
// watermark bound to artifactID (the permanent one), or nil when the window is
// still open. It reads the same rows the audit-layer settlement writes, so the
// read-back reflects a settlement whatever recorded it.
func (s *Server) groomingWindowSettlementFor(ctx context.Context, runID uuid.UUID, artifactID string) (*groomingWindowSettlement, error) {
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, audit.GroomingApplyWindowClosedCategory)
	if err != nil {
		return nil, err
	}
	var best *audit.Entry
	for _, e := range rows {
		if e == nil {
			continue
		}
		var p groomingWindowPayload
		if json.Unmarshal(e.Payload, &p) != nil || p.ArtifactID != artifactID {
			continue
		}
		if best == nil || e.Sequence < best.Sequence {
			best = e
		}
	}
	if best == nil {
		return nil, nil
	}
	var p groomingWindowPayload
	_ = json.Unmarshal(best.Payload, &p)
	return &groomingWindowSettlement{
		Settlement:    p.Settlement,
		ClosedAt:      best.Timestamp.UTC().Format(time.RFC3339Nano),
		AuditSequence: best.Sequence,
	}, nil
}

// projectGroomingDispositions collapses the run's grooming_disposition_recorded
// rows into the current disposition set for ONE artifact.
//
//   - Each row is decoded through a TOLERANT struct; an undecodable row is
//     SKIPPED, contributing no disposition. That is the fail-safe direction and
//     mirrors priorGroomingDispositions: a junk row must not manufacture a
//     verdict the operator never recorded.
//   - Only rows whose artifact_id matches the resolved artifact are kept, so a
//     capture against an older report never leaks into a newer report's
//     read-back.
//   - Repeats on one entry_id collapse LAST-WINS by audit sequence. Both rows
//     stay in the chain, so the supersession is itself auditable; refusing the
//     repeat instead would make an operator's corrected verdict unrecordable.
//
// Output is sorted by entry_id so the body is deterministic.
func projectGroomingDispositions(entries []*audit.Entry, artifactID string, classes map[string]string) []recordedGroomingDisposition {
	byEntry := make(map[string]recordedGroomingDisposition)
	for _, e := range entries {
		if e == nil {
			continue
		}
		var rec struct {
			ArtifactID  string `json:"artifact_id"`
			EntryID     string `json:"entry_id"`
			EntryClass  string `json:"entry_class"`
			Verdict     string `json:"verdict"`
			CloseTarget string `json:"close_target"`
		}
		if json.Unmarshal(e.Payload, &rec) != nil || rec.EntryID == "" {
			continue
		}
		if rec.ArtifactID != artifactID {
			continue
		}
		// LAST WINS: the chain is listed sequence-ascending, but the comparison
		// is made explicitly so the projection does not depend on that ordering.
		if prev, ok := byEntry[rec.EntryID]; ok && prev.AuditSequence > e.Sequence {
			continue
		}
		class := rec.EntryClass
		if class == "" {
			class = classes[rec.EntryID]
		}
		subject := ""
		if e.ActorSubject != nil {
			subject = *e.ActorSubject
		}
		byEntry[rec.EntryID] = recordedGroomingDisposition{
			EntryID: rec.EntryID, EntryClass: class,
			Verdict: rec.Verdict, CloseTarget: rec.CloseTarget,
			RecordedAt: e.Timestamp.UTC().Format(time.RFC3339Nano),
			RecordedBy: subject, AuditSequence: e.Sequence,
		}
	}
	out := make([]recordedGroomingDisposition, 0, len(byEntry))
	for _, d := range byEntry {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EntryID < out[j].EntryID })
	return out
}
