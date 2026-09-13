package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Acceptance transcript (E72.5 / #3329): the per-criterion record the
// acceptance agent writes beside its boolean verdict — every request sent,
// every response observed, the assertion evaluated, its outcome and wall
// time. The runner ships it to POST /v0/runs/{run_id}/acceptance/transcript
// BEFORE the verdict, persists it as an `acceptance_transcript` artifact
// (migration 0083), and injects a runner-authored {artifact_id, content_hash}
// ref into the verdict, which handleShipAcceptance cross-checks against the
// stored artifact (resolveAcceptanceTranscriptRef) and summarizes from the
// STORED content onto acceptance_outcome_recorded as `transcript`.
//
// WHY THE GRAMMAR IS ENFORCED HERE, on the write path, and not only in the
// runner: this endpoint is independently callable by any run signer or
// write:runs bearer, and three prompt/comment surfaces (the implement-review
// gate evidence, the anchor comment, fishhawk_get_run_status) render values
// taken from the stored transcript WITHOUT an untrusted-intake envelope. The
// render-safety property — no whitespace, control byte, backtick or '|' can
// reach a prompt or a markdown row — therefore has to hold for EVERY stored
// transcript regardless of which client wrote it. Every value any surface
// renders (criteria[].id, seed, method, path, status) is grammar- AND
// length-bounded below, and anything outside is REJECTED with 400 — never
// truncated, never sanitised — so a stored transcript is render-safe by
// construction. Assertion text, seed prose and request/response bodies are
// stored-only: no surface renders them, and a reader reaches them through
// GET /v0/artifacts/{id}.
//
// VERDICT AUTHORITY (approval condition 1): the VERDICT is authoritative and
// the transcript is DESCRIPTIVE. Its ref is bound to the stage, the kind and
// the content hash, and its per-criterion outcomes are additionally
// cross-checked against the verdict's own rows at summary-derivation time
// (acceptanceTranscriptAgreement): a transcript row whose outcome disagrees
// with the verdict row of the same id, or that names a criterion the verdict
// did not report, SUPPRESSES the rendered per-criterion summary (the outcome
// payload keeps the artifact ref and names the suppression reason) so no
// consumer can publish a per-criterion story that contradicts the verdict
// headline. It NEVER changes the verdict: the verdict is recorded exactly as
// it would have been without the transcript.

// maxAcceptanceTranscriptBytes caps the transcript request body: 256 KiB, the
// runner's post-redaction ship bound. A plain const, never test-injectable
// (the #3106 rule); pinned by TestAcceptanceTranscriptCapValue.
const maxAcceptanceTranscriptBytes = 256 * 1024

// Per-field bounds. Each is a value any render surface prints (id, seed,
// path) or a stored-only cap (assertion, bodies, counts). All plain consts.
const (
	acceptanceTranscriptMaxCriteria      = 100
	acceptanceTranscriptMaxRequests      = 200
	acceptanceTranscriptMaxIDBytes       = 128
	acceptanceTranscriptMaxSeedBytes     = 200
	acceptanceTranscriptMaxPathBytes     = 512
	acceptanceTranscriptMaxBodyBytes     = 4096
	acceptanceTranscriptMaxAssertionByte = 2000
)

var (
	// acceptanceTranscriptIDRe is the plan-schema criterion-id pattern
	// (docs/spec/plan-standard-v1.schema.json: ^[a-z0-9][a-z0-9-]*$),
	// optionally under the runner's replayed-scenario prefix
	// (runner/internal/scenario mints `scenario:issue-<N>/<criterion-id>`).
	acceptanceTranscriptIDRe = regexp.MustCompile(`^(scenario:issue-[0-9]+/)?[a-z0-9][a-z0-9-]*$`)
	// acceptanceTranscriptSeedRe is the catalog-name charset
	// (backend/internal/devfixtures/catalog).
	acceptanceTranscriptSeedRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// acceptanceTranscriptPathRe is RFC 3986 §3.3/§3.4: pchar plus '/' and
	// '?'. It admits NO whitespace, control byte, backtick or '|', so a
	// validated path cannot break a markdown table row or a backtick span.
	acceptanceTranscriptPathRe = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@%/?-]*$`)
)

// acceptanceTranscriptMethods is the closed request-method enum.
var acceptanceTranscriptMethods = map[string]struct{}{
	"GET": {}, "HEAD": {}, "POST": {}, "PUT": {}, "PATCH": {}, "DELETE": {}, "OPTIONS": {},
}

// acceptanceTranscriptOutcomes is the closed per-criterion outcome enum —
// the same vocabulary as the verdict's criteria[].result, so the agreement
// check compares like with like.
var acceptanceTranscriptOutcomes = map[string]struct{}{
	"passed": {}, "failed": {}, "skipped": {}, "undecidable": {},
}

// acceptanceTranscriptBody is the wire shape the runner POSTs (and the
// artifact's stored content, verbatim).
//
// CROSS-MODULE WIRE CONTRACT: the json tags MUST stay byte-identical to
// runner/internal/upload.AcceptanceTranscript (runner/internal/upload/
// upload.go); the wirecontract manifest pins the pair ModeExact.
type acceptanceTranscriptBody struct {
	// Criteria carries one entry per criterion row the validator reported.
	// 1..100 entries, ids unique.
	Criteria []acceptanceTranscriptCriterion `json:"criteria"`
	// TargetURL is the instance driven, when declared. Optional, http(s).
	TargetURL string `json:"target_url,omitempty"`
}

// acceptanceTranscriptCriterion is one criterion's transcript.
//
// CROSS-MODULE WIRE CONTRACT: the json tags MUST stay byte-identical to
// runner/internal/upload.AcceptanceTranscriptCriterion; the wirecontract
// manifest pins the pair ModeExact.
type acceptanceTranscriptCriterion struct {
	ID string `json:"id"`
	// Seed names the fixture scenario materialized for this criterion, when
	// one was. Optional; catalog-name charset.
	Seed string `json:"seed,omitempty"`
	// Requests are recorded in the order sent; <= 200.
	Requests []acceptanceTranscriptRequest `json:"requests"`
	// Assertion is the check evaluated. Stored-only, <= 2000 bytes.
	Assertion string `json:"assertion"`
	Outcome   string `json:"outcome"`
	WallMs    int    `json:"wall_ms"`
}

// acceptanceTranscriptRequest is one request/response pair.
//
// CROSS-MODULE WIRE CONTRACT: the json tags MUST stay byte-identical to
// runner/internal/upload.AcceptanceTranscriptRequest; the wirecontract
// manifest pins the pair ModeExact.
type acceptanceTranscriptRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	// RequestBody / ResponseBody are redacted by the runner and stored-only
	// (never rendered); <= 4096 bytes each.
	RequestBody  string `json:"request_body,omitempty"`
	Status       int    `json:"status"`
	ResponseBody string `json:"response_body,omitempty"`
	ElapsedMs    int    `json:"elapsed_ms"`
}

// acceptanceTranscriptRef is the runner-injected verdict field naming the
// already-persisted transcript. Both values are backend-minted (the ship
// response's id and hash), which is why injecting it AFTER the verdict's
// redaction is safe.
//
// CROSS-MODULE WIRE CONTRACT: the json tags MUST stay byte-identical to
// runner/internal/upload.AcceptanceTranscriptRef; the wirecontract manifest
// pins the pair ModeExact.
type acceptanceTranscriptRef struct {
	ArtifactID  string `json:"artifact_id"`
	ContentHash string `json:"content_hash"`
}

// acceptanceTranscriptResponse is the ship response.
type acceptanceTranscriptResponse struct {
	ID          uuid.UUID `json:"id"`
	StageID     uuid.UUID `json:"stage_id"`
	ContentHash string    `json:"content_hash"`
	Idempotent  bool      `json:"idempotent"`
}

// validate enforces the CLOSED transcript shape. Every rule is fail-closed —
// REJECT, never truncate or sanitise — and each returns an error naming the
// field, so the 400's details.error tells the runner which rule it broke.
func (b *acceptanceTranscriptBody) validate() error {
	if len(b.Criteria) == 0 {
		return errors.New("criteria: required, at least one entry")
	}
	if len(b.Criteria) > acceptanceTranscriptMaxCriteria {
		return fmt.Errorf("criteria: %d entries exceeds the %d cap", len(b.Criteria), acceptanceTranscriptMaxCriteria)
	}
	if b.TargetURL != "" && !strings.HasPrefix(b.TargetURL, "http://") && !strings.HasPrefix(b.TargetURL, "https://") {
		return errors.New("target_url: must be an http(s) URL")
	}
	seen := make(map[string]struct{}, len(b.Criteria))
	for i, c := range b.Criteria {
		at := fmt.Sprintf("criteria[%d]", i)
		if c.ID == "" {
			return fmt.Errorf("%s.id: required", at)
		}
		if len(c.ID) > acceptanceTranscriptMaxIDBytes {
			return fmt.Errorf("%s.id: %d bytes exceeds the %d cap", at, len(c.ID), acceptanceTranscriptMaxIDBytes)
		}
		if !acceptanceTranscriptIDRe.MatchString(c.ID) {
			return fmt.Errorf("%s.id: must match %s", at, acceptanceTranscriptIDRe.String())
		}
		if _, dup := seen[c.ID]; dup {
			return fmt.Errorf("%s.id: duplicate id %q", at, c.ID)
		}
		seen[c.ID] = struct{}{}
		if c.Seed != "" {
			if len(c.Seed) > acceptanceTranscriptMaxSeedBytes {
				return fmt.Errorf("%s.seed: %d bytes exceeds the %d cap", at, len(c.Seed), acceptanceTranscriptMaxSeedBytes)
			}
			if !acceptanceTranscriptSeedRe.MatchString(c.Seed) {
				return fmt.Errorf("%s.seed: must match %s", at, acceptanceTranscriptSeedRe.String())
			}
		}
		if len(c.Requests) > acceptanceTranscriptMaxRequests {
			return fmt.Errorf("%s.requests: %d entries exceeds the %d cap", at, len(c.Requests), acceptanceTranscriptMaxRequests)
		}
		for j, r := range c.Requests {
			rat := fmt.Sprintf("%s.requests[%d]", at, j)
			if _, ok := acceptanceTranscriptMethods[r.Method]; !ok {
				return fmt.Errorf("%s.method: %q is not one of GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS", rat, r.Method)
			}
			if len(r.Path) > acceptanceTranscriptMaxPathBytes {
				return fmt.Errorf("%s.path: %d bytes exceeds the %d cap", rat, len(r.Path), acceptanceTranscriptMaxPathBytes)
			}
			if !acceptanceTranscriptPathRe.MatchString(r.Path) {
				return fmt.Errorf("%s.path: must match %s", rat, acceptanceTranscriptPathRe.String())
			}
			if len(r.RequestBody) > acceptanceTranscriptMaxBodyBytes {
				return fmt.Errorf("%s.request_body: %d bytes exceeds the %d cap", rat, len(r.RequestBody), acceptanceTranscriptMaxBodyBytes)
			}
			if r.Status < 100 || r.Status > 599 {
				return fmt.Errorf("%s.status: %d is outside 100..599", rat, r.Status)
			}
			if len(r.ResponseBody) > acceptanceTranscriptMaxBodyBytes {
				return fmt.Errorf("%s.response_body: %d bytes exceeds the %d cap", rat, len(r.ResponseBody), acceptanceTranscriptMaxBodyBytes)
			}
			if r.ElapsedMs < 0 {
				return fmt.Errorf("%s.elapsed_ms: must be >= 0", rat)
			}
		}
		if len(c.Assertion) > acceptanceTranscriptMaxAssertionByte {
			return fmt.Errorf("%s.assertion: %d bytes exceeds the %d cap", at, len(c.Assertion), acceptanceTranscriptMaxAssertionByte)
		}
		if _, ok := acceptanceTranscriptOutcomes[c.Outcome]; !ok {
			return fmt.Errorf("%s.outcome: %q is not one of passed|failed|skipped|undecidable", at, c.Outcome)
		}
		if c.WallMs < 0 {
			return fmt.Errorf("%s.wall_ms: must be >= 0", at)
		}
	}
	return nil
}

// decodeAcceptanceTranscript strictly decodes ONE transcript object
// (DisallowUnknownFields, no trailing data) and validates it.
func decodeAcceptanceTranscript(body []byte) (acceptanceTranscriptBody, error) {
	var tr acceptanceTranscriptBody
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&tr); err != nil {
		return tr, fmt.Errorf("decode: %w", err)
	}
	if dec.More() {
		return tr, errors.New("body must contain a single JSON object")
	}
	if err := tr.validate(); err != nil {
		return tr, err
	}
	return tr, nil
}

// handleShipAcceptanceTranscript persists a runner-shipped acceptance
// transcript as an `acceptance_transcript` artifact. The guard ORDER mirrors
// handleShipAcceptance exactly: dependencies → ids → stage exists / belongs
// to the run / is an acceptance stage → size cap → signature-or-bearer →
// strict decode + validate → idempotency on (stage_id, content_hash) →
// create. No new audit category: the durable reference is the `transcript`
// block handleShipAcceptance records on acceptance_outcome_recorded, and
// GET /v0/stages/{stage_id}/artifacts lists the artifact by kind.
func (s *Server) handleShipAcceptanceTranscript(w http.ResponseWriter, r *http.Request) {
	if s.cfg.SigningRepo == nil || s.cfg.ArtifactRepo == nil || s.cfg.RunRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "acceptance_transcript_upload_unconfigured",
			"acceptance transcript upload requires signing, artifact, and run repositories", nil)
		return
	}
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}
	stageID, err := uuid.Parse(r.URL.Query().Get("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id query parameter must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.URL.Query().Get("stage_id")})
		return
	}
	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		s.writeError(w, r, http.StatusNotFound, "stage_not_found",
			"stage does not exist", map[string]any{"stage_id": stageID.String()})
		return
	}
	if stage.RunID != runID {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage does not belong to the supplied run",
			map[string]any{"stage_id": stageID.String(), "run_id": runID.String()})
		return
	}
	if stage.Type != run.StageTypeAcceptance {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"acceptance transcripts may only be attached to an acceptance stage",
			map[string]any{"stage_id": stageID.String(), "stage_type": string(stage.Type)})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAcceptanceTranscriptBytes+1))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return
	}
	if len(body) > maxAcceptanceTranscriptBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
			"acceptance transcript exceeds size cap",
			map[string]any{"limit_bytes": maxAcceptanceTranscriptBytes})
		return
	}
	if _, _, _, ok := s.authorizeAcceptance(w, r, runID, body); !ok {
		return
	}
	if _, err := decodeAcceptanceTranscript(body); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "acceptance_transcript_invalid",
			"acceptance transcript missing or malformed fields",
			map[string]any{"error": err.Error()})
		return
	}
	contentHash := sha256Hex(body)
	if existing, err := s.cfg.ArtifactRepo.GetByHash(r.Context(), stageID, contentHash); err == nil {
		s.writeJSON(w, r, http.StatusOK, acceptanceTranscriptResponse{
			ID: existing.ID, StageID: existing.StageID, ContentHash: existing.ContentHash, Idempotent: true,
		})
		return
	} else if !errors.Is(err, artifact.ErrNotFound) {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"check existing acceptance transcript failed", map[string]any{"error": err.Error()})
		return
	}
	created, err := s.cfg.ArtifactRepo.Create(r.Context(), artifact.CreateParams{
		StageID:     stageID,
		Kind:        artifact.KindAcceptanceTranscript,
		Content:     json.RawMessage(body),
		ContentHash: contentHash,
		// SchemaVersion intentionally nil for v0 (mirroring acceptance).
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"create acceptance transcript artifact failed", map[string]any{"error": err.Error()})
		return
	}
	s.writeJSON(w, r, http.StatusCreated, acceptanceTranscriptResponse{
		ID: created.ID, StageID: created.StageID, ContentHash: created.ContentHash, Idempotent: false,
	})
}

// Named cross-check failures, surfaced verbatim as the verdict 400's
// details.error so the runner can tell which binding broke.
const (
	transcriptRefInvalid            = "transcript_ref_invalid"
	transcriptArtifactNotFound      = "transcript_artifact_not_found"
	transcriptArtifactKindMismatch  = "transcript_artifact_kind_mismatch"
	transcriptArtifactStageMismatch = "transcript_artifact_stage_mismatch"
	transcriptContentHashMismatch   = "transcript_content_hash_mismatch"
)

// resolveAcceptanceTranscriptRef is the fail-closed cross-check of a
// verdict's runner-injected transcript ref against the stored artifact: it
// must exist, be an acceptance_transcript, belong to THIS acceptance stage,
// and carry exactly the referenced content hash. Any miss returns one of the
// named errors above and the caller persists NOTHING — a ref that does not
// resolve is a malformed verdict body (a runner defect or a forged ref), the
// same class as any other invalid field, not a transcript-content problem.
func (s *Server) resolveAcceptanceTranscriptRef(ctx context.Context, stageID uuid.UUID, ref acceptanceTranscriptRef) (*artifact.Artifact, error) {
	id, err := uuid.Parse(ref.ArtifactID)
	if err != nil {
		return nil, fmt.Errorf("%s: artifact_id %q is not a UUID", transcriptRefInvalid, ref.ArtifactID)
	}
	if !isLowerHex64(ref.ContentHash) {
		return nil, fmt.Errorf("%s: content_hash must be 64 lowercase hex chars", transcriptRefInvalid)
	}
	a, err := s.cfg.ArtifactRepo.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("%s: %s", transcriptArtifactNotFound, id)
	}
	if a.Kind != artifact.KindAcceptanceTranscript {
		return nil, fmt.Errorf("%s: artifact %s is kind %q", transcriptArtifactKindMismatch, id, a.Kind)
	}
	if a.StageID != stageID {
		return nil, fmt.Errorf("%s: artifact %s belongs to stage %s, not %s", transcriptArtifactStageMismatch, id, a.StageID, stageID)
	}
	if a.ContentHash != ref.ContentHash {
		return nil, fmt.Errorf("%s: artifact %s has hash %s, ref carries %s", transcriptContentHashMismatch, id, a.ContentHash, ref.ContentHash)
	}
	return a, nil
}

func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// acceptanceTranscriptSummary is the bounded `transcript` block recorded on
// acceptance_outcome_recorded, derived from the STORED artifact content
// (never from the runner-injected ref) by summarizeAcceptanceTranscript. It
// is the ONE payload shape every consumer reads: fishhawk_get_run_status,
// the anchor comment's outcome line and the implement-review gate evidence.
//
// Criteria is nil (JSON null) with SummarySuppressed set when the transcript
// disagrees with the verdict (see acceptanceTranscriptAgreement): the
// artifact ref is kept so the transcript stays one fetch away, but no
// per-criterion story that could contradict the verdict headline is rendered.
type acceptanceTranscriptSummary struct {
	ArtifactID  string                                 `json:"artifact_id"`
	ContentHash string                                 `json:"content_hash"`
	Criteria    []acceptanceTranscriptCriterionSummary `json:"criteria"`
	// SummarySuppressed names WHY Criteria is null: `criterion_not_in_verdict`
	// or `criterion_outcome_disagrees`. Omitted on an agreeing transcript.
	SummarySuppressed string `json:"summary_suppressed,omitempty"`
	// DisagreeingIDs lists the (grammar-bounded) transcript ids that broke the
	// agreement, sorted. Omitted on an agreeing transcript.
	DisagreeingIDs []string `json:"disagreeing_ids,omitempty"`
}

// acceptanceTranscriptCriterionSummary is one criterion's bounded summary.
// FailingRequest is INSIDE criteria[] — there is no top-level failing request.
type acceptanceTranscriptCriterionSummary struct {
	ID           string `json:"id"`
	Outcome      string `json:"outcome"`
	RequestCount int    `json:"request_count"`
	// FailingRequest applies ONE rule everywhere: for a `failed` criterion it
	// is the LAST recorded request — the one whose response the failing
	// assertion evaluated, since requests are recorded in the order sent —
	// REGARDLESS of its status code (an assertion failure on a 200 is the
	// common shape). It is null ONLY when the failed criterion recorded zero
	// requests, and always null for a non-failed criterion.
	FailingRequest *acceptanceTranscriptFailingRequest `json:"failing_request"`
}

// acceptanceTranscriptFailingRequest carries the three grammar-bounded
// fields a surface may render. Bodies are never carried here.
type acceptanceTranscriptFailingRequest struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
}

// summarizeAcceptanceTranscript derives the bounded per-criterion block from
// stored transcript content. Content that no longer decodes/validates (it
// was validated at ingest, so this is a storage anomaly) yields a nil slice
// and an error the caller reports; it never invents rows.
func summarizeAcceptanceTranscript(content []byte) ([]acceptanceTranscriptCriterionSummary, error) {
	tr, err := decodeAcceptanceTranscript(content)
	if err != nil {
		return nil, err
	}
	out := make([]acceptanceTranscriptCriterionSummary, 0, len(tr.Criteria))
	for _, c := range tr.Criteria {
		row := acceptanceTranscriptCriterionSummary{ID: c.ID, Outcome: c.Outcome, RequestCount: len(c.Requests)}
		if c.Outcome == "failed" && len(c.Requests) > 0 {
			last := c.Requests[len(c.Requests)-1]
			row.FailingRequest = &acceptanceTranscriptFailingRequest{Method: last.Method, Path: last.Path, Status: last.Status}
		}
		out = append(out, row)
	}
	return out, nil
}

// Suppression reasons for acceptanceTranscriptSummary.SummarySuppressed.
const (
	transcriptSuppressedNotInVerdict     = "criterion_not_in_verdict"
	transcriptSuppressedOutcomeDisagrees = "criterion_outcome_disagrees"
)

// acceptanceTranscriptAgreement cross-checks the derived summary rows against
// the verdict's OWN rows (approval condition 1, option (a)): every transcript
// id must name a verdict row and carry the same outcome as that row's result.
// It returns the suppression reason and the offending ids (sorted), or "" and
// nil when the transcript agrees. Comparison is against what the AGENT
// shipped (the rows), not the ladder-rewritten recorded verdict, because the
// rows are what the transcript describes; a transcript agreeing with the rows
// cannot contradict a headline the rows themselves produced. The check NEVER
// touches the verdict — its only effect is on the rendered summary.
//
// A verdict with NO rows at all (a legacy, un-itemized verdict) has nothing to
// agree with; every transcript id then fails membership, and the summary is
// suppressed rather than rendered against a verdict that itemized nothing.
func acceptanceTranscriptAgreement(rows []acceptanceTranscriptCriterionSummary, verdictRows []acceptanceCriterionResult) (reason string, ids []string) {
	results := make(map[string]string, len(verdictRows))
	for _, v := range verdictRows {
		results[v.ID] = v.Result
	}
	var missing, disagree []string
	for _, r := range rows {
		res, ok := results[r.ID]
		switch {
		case !ok:
			missing = append(missing, r.ID)
		case res != r.Outcome:
			disagree = append(disagree, r.ID)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return transcriptSuppressedNotInVerdict, missing
	}
	if len(disagree) > 0 {
		sort.Strings(disagree)
		return transcriptSuppressedOutcomeDisagrees, disagree
	}
	return "", nil
}

// buildAcceptanceTranscriptSummary assembles the outcome-payload block from
// the resolved artifact and the verdict rows: the ref (backend-minted values
// read back from the STORED artifact), the derived rows, and — when the
// agreement check fails — a null criteria list plus the named reason.
func buildAcceptanceTranscriptSummary(a *artifact.Artifact, verdictRows []acceptanceCriterionResult) (acceptanceTranscriptSummary, error) {
	sum := acceptanceTranscriptSummary{ArtifactID: a.ID.String(), ContentHash: a.ContentHash}
	rows, err := summarizeAcceptanceTranscript(a.Content)
	if err != nil {
		return sum, err
	}
	if reason, ids := acceptanceTranscriptAgreement(rows, verdictRows); reason != "" {
		sum.SummarySuppressed = reason
		sum.DisagreeingIDs = ids
		return sum, nil
	}
	sum.Criteria = rows
	return sum, nil
}

// decodeAcceptanceTranscriptSummary reads the `transcript` block back off an
// acceptance_outcome_recorded payload: nil on null, absent or undecodable —
// the consumer-side twin of buildAcceptanceTranscriptSummary that
// latestAcceptanceOutcome exposes as acceptanceOutcome.Transcript.
func decodeAcceptanceTranscriptSummary(raw json.RawMessage) *acceptanceTranscriptSummary {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var sum acceptanceTranscriptSummary
	if err := json.Unmarshal(raw, &sum); err != nil || sum.ArtifactID == "" {
		return nil
	}
	return &sum
}
