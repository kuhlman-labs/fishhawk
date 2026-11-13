package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/agentpaths"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/redaction"
)

// Conflict-resolution re-review (#4795). A conflict_resolution_pushed report
// of a CODE resolution starts an ordinary full-diff implement-review round at
// the merge head (origin conflict_resolution), whose diff is the forge compare
// base_ref...merge-head. The runner's remerge-diff summary, when present, is
// secret-scanned raw, stored REDACTED as a conflict_resolution_diff artifact,
// and rendered as an additional prompt section read through ONE loader.
// The contract is in docs/review.md § Conflict-resolution re-review.

// CategoryConflictResolutionReviewSkipped records that a conflict-resolution
// report's automatic review started no round, with a closed reason.
const CategoryConflictResolutionReviewSkipped = "conflict_resolution_review_skipped"

// Skip reasons (closed set) for CategoryConflictResolutionReviewSkipped.
const (
	conflictResolutionSkipDocsOnly         = "resolution_docs_only"
	conflictResolutionSkipDecomposedParent = "decomposed_parent"
)

// Wire bounds for the six conflict-resolution summary fields.
const (
	maxResolvedPaths                  = 200
	maxResolvedPathBytes              = 512
	maxResolutionUnavailableReason    = 64
	maxResolutionUnavailableDetail    = 1024
	maxStoredResolutionDetailBytes    = 256
	conflictResolutionNoVerifyReason  = "conflict_resolution_pass_runs_no_verify"
	conflictResolutionDiffStoreFailed = "resolution_diff_store_failed"
	conflictResolutionSlugBaseUnknown = "conflict_base_unresolved"
)

// conflictReviewDispatchObserved is a PACKAGE-LEVEL test seam, nil in
// production: maybeDispatchConflictResolutionReview calls it first, so a test
// can count dispatcher entries without a Server field. Tests that set it are
// not t.Parallel.
var conflictReviewDispatchObserved func(runID, stageID uuid.UUID, headSHA string)

// hasConflictResolutionSummaryField reports whether any of the six
// conflict-resolution summary fields is present.
func (p *pullRequestBody) hasConflictResolutionSummaryField() bool {
	return len(p.ResolvedPaths) > 0 || p.ResolvedPathsTruncated || p.ResolutionPatch != "" ||
		p.ResolutionPatchTruncated || p.ResolutionPatchUnavailableReason != "" || p.ResolutionPatchUnavailableDetail != ""
}

// validateConflictResolutionSummary validates the six summary fields of a
// conflict_resolution_pushed report. Every message names the field and index
// only and never echoes a value (D9).
func (p *pullRequestBody) validateConflictResolutionSummary() error {
	if !p.hasConflictResolutionSummaryField() {
		return nil
	}
	if len(p.ResolvedPaths) == 0 {
		return errors.New("resolved_paths must be non-empty when any resolution_* field or resolved_paths_truncated is set")
	}
	for i, rp := range p.ResolvedPaths {
		if err := validateResolvedPath(rp); err != nil {
			return fmt.Errorf("resolved_paths[%d] %w", i, err)
		}
	}
	switch {
	case p.ResolutionPatchTruncated && p.ResolutionPatch == "":
		return errors.New("resolution_patch_truncated requires a non-empty resolution_patch")
	case p.ResolutionPatch != "" && p.ResolutionPatchUnavailableReason != "":
		return errors.New("resolution_patch_unavailable_reason must be absent when resolution_patch is set")
	case p.ResolutionPatch != "" && p.ResolutionPatchUnavailableDetail != "":
		return errors.New("resolution_patch_unavailable_detail must be absent when resolution_patch is set")
	case len(p.ResolutionPatchUnavailableReason) > maxResolutionUnavailableReason:
		return fmt.Errorf("resolution_patch_unavailable_reason exceeds %d bytes", maxResolutionUnavailableReason)
	case len(p.ResolutionPatchUnavailableDetail) > maxResolutionUnavailableDetail:
		return fmt.Errorf("resolution_patch_unavailable_detail exceeds %d bytes", maxResolutionUnavailableDetail)
	case strings.IndexByte(p.ResolutionPatchUnavailableDetail, 0) >= 0:
		return errors.New("resolution_patch_unavailable_detail contains a NUL byte")
	}
	return nil
}

// validateResolvedPath rejects a resolved path that is empty, over 512 bytes,
// absolute, has a `..` segment, contains a backslash, or contains a control
// byte. The error never carries the path.
func validateResolvedPath(rp string) error {
	switch {
	case rp == "":
		return errors.New("is empty")
	case len(rp) > maxResolvedPathBytes:
		return fmt.Errorf("exceeds %d bytes", maxResolvedPathBytes)
	case strings.HasPrefix(rp, "/"):
		return errors.New("is absolute")
	case strings.Contains(rp, "\\"):
		return errors.New("contains a backslash")
	}
	for _, seg := range strings.Split(rp, "/") {
		if seg == ".." {
			return errors.New("has a .. segment")
		}
	}
	for i := 0; i < len(rp); i++ {
		c := rp[i]
		if c < 0x20 || c == 0x7f {
			return errors.New("contains a control character")
		}
	}
	return nil
}

// conflictResolutionClosedSlugs is the closed set of patch-unavailable slugs a
// runner reason is kept as; anything else maps to resolution_patch_unavailable.
var conflictResolutionClosedSlugs = map[string]bool{
	prompt.ConflictResolutionRemergeDiffFailed:        true,
	prompt.ConflictResolutionRemergeCustomMergeDriver: true,
	prompt.ConflictResolutionPatchExceedsReportCap:    true,
}

// resolutionPatchUnavailableSlug maps the runner's reason onto the closed set.
// "" when a patch is present.
func resolutionPatchUnavailableSlug(patch, reason string) string {
	if patch != "" {
		return ""
	}
	if conflictResolutionClosedSlugs[reason] {
		return reason
	}
	return prompt.ConflictResolutionPatchUnavailable
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// conflictResolutionSummaryResult is what recordConflictResolutionSummary
// hands the report handler: the row payload additions and the report-time
// secret-scan group count.
type conflictResolutionSummaryResult struct {
	payload      map[string]any
	secretGroups int
}

// recordConflictResolutionSummary persists a conflict_resolution_pushed
// report's summary (#4795). It runs only when resolved_paths is non-empty. It
// FIRST scans the RAW patch for secrets, then stores the REDACTED patch as a
// conflict_resolution_diff artifact on the implement stage, and returns the
// row additions: the paths, the artifact id, its sha256 and its byte count —
// or resolution_diff_unavailable on a store failure, which never fails the
// push. No patch text and no detail reach the row, a log line or an error.
func (s *Server) recordConflictResolutionSummary(ctx context.Context, runID uuid.UUID, stage *run.Stage, pr *pullRequestBody) conflictResolutionSummaryResult {
	if len(pr.ResolvedPaths) == 0 {
		return conflictResolutionSummaryResult{}
	}
	paths := pr.ResolvedPaths
	truncated := pr.ResolvedPathsTruncated
	if len(paths) > maxResolvedPaths {
		paths = paths[:maxResolvedPaths]
		truncated = true
	}
	pr.ResolvedPaths, pr.ResolvedPathsTruncated = paths, truncated

	res := conflictResolutionSummaryResult{}
	if pr.ResolutionPatch != "" {
		changed := make([]policy.ChangedFile, 0, len(paths))
		for _, p := range paths {
			changed = append(changed, policy.ChangedFile{Path: p, Status: policy.StatusModified})
		}
		groups := s.raiseDiffSecretConcerns(ctx, runID, stage.ID, policy.Diff{Patch: pr.ResolutionPatch, ChangedFiles: changed}, pr.HeadSHA)
		res.secretGroups = len(groups)
	}

	redactedPatch, _ := redaction.RedactDefault([]byte(pr.ResolutionPatch))
	redactedDetail, _ := redaction.RedactDefault([]byte(pr.ResolutionPatchUnavailableDetail))
	detail := ""
	if pr.ResolutionPatch == "" {
		detail = truncateUTF8(string(redactedDetail), maxStoredResolutionDetailBytes)
	}
	content := artifact.ConflictResolutionDiffContent{
		Schema:                 artifact.ConflictResolutionDiffSchema,
		HeadSHA:                pr.HeadSHA,
		BaseSHA:                pr.BaseSHA,
		ResolvedPaths:          paths,
		ResolvedPathsTruncated: truncated,
		Patch:                  string(redactedPatch),
		PatchTruncated:         pr.ResolutionPatchTruncated,
		PatchUnavailable:       resolutionPatchUnavailableSlug(pr.ResolutionPatch, pr.ResolutionPatchUnavailableReason),
		PatchUnavailableDetail: detail,
	}
	res.payload = map[string]any{
		"resolved_paths":           paths,
		"resolved_paths_truncated": truncated,
	}
	body, err := json.Marshal(content)
	if err != nil {
		res.payload["resolution_diff_unavailable"] = conflictResolutionDiffStoreFailed
		return res
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	a, err := s.cfg.ArtifactRepo.GetByHash(ctx, stage.ID, hash)
	if err != nil {
		schema := artifact.ConflictResolutionDiffSchema
		a, err = s.cfg.ArtifactRepo.Create(ctx, artifact.CreateParams{
			StageID:       stage.ID,
			Kind:          artifact.KindConflictResolutionDiff,
			SchemaVersion: &schema,
			Content:       body,
			ContentHash:   hash,
		})
	}
	if err != nil || a == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"conflict-resolution push report: store the resolution summary artifact failed; the round renders without it",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stage.ID.String()))
		res.payload["resolution_diff_unavailable"] = conflictResolutionDiffStoreFailed
		return res
	}
	res.payload["resolution_diff_artifact_id"] = a.ID.String()
	res.payload["resolution_diff_sha256"] = hash
	res.payload["resolution_diff_bytes"] = len(body)
	return res
}

// conflictResolutionFraming is a conflict_resolution round's framing, read
// ONLY through loadConflictResolutionFraming.
type conflictResolutionFraming struct {
	ResolvedPaths          []string
	ResolvedPathsTruncated bool
	Patch                  string
	PatchTruncated         bool
	UnavailableSlug        string
}

// promptContext maps the framing onto the prompt's section input.
func (fr conflictResolutionFraming) promptContext() *prompt.ConflictResolutionContext {
	return &prompt.ConflictResolutionContext{
		ResolvedPaths:          fr.ResolvedPaths,
		ResolvedPathsTruncated: fr.ResolvedPathsTruncated,
		RemergePatch:           fr.Patch,
		RemergePatchTruncated:  fr.PatchTruncated,
		UnavailableReason:      fr.UnavailableSlug,
	}
}

// conflictResolutionPushedRow is the slice of a conflict_resolution_pushed
// payload the framing loader reads.
type conflictResolutionPushedRow struct {
	HeadSHA                   string   `json:"head_sha"`
	ResolvedPaths             []string `json:"resolved_paths"`
	ResolvedPathsTruncated    bool     `json:"resolved_paths_truncated"`
	ResolutionDiffArtifactID  string   `json:"resolution_diff_artifact_id"`
	ResolutionDiffSHA256      string   `json:"resolution_diff_sha256"`
	ResolutionDiffUnavailable string   `json:"resolution_diff_unavailable"`
}

// loadConflictResolutionFraming is the ONE framing loader (#4795 D9). It never
// refuses a round: a missing row or summary yields resolution_summary_unavailable,
// and any artifact miss or cross-check failure yields resolution_diff_unavailable
// with the cause WARN-logged. The hash is compared column to row, never
// recomputed over the JSONB read-back (artifact.CheckConflictResolutionDiff).
func (s *Server) loadConflictResolutionFraming(ctx context.Context, runID, stageID uuid.UUID, headSHA string) conflictResolutionFraming {
	none := conflictResolutionFraming{UnavailableSlug: prompt.ConflictResolutionSummaryUnavailable}
	if s.cfg.AuditRepo == nil {
		return none
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryConflictResolutionPushed)
	if err != nil {
		return none
	}
	var row *conflictResolutionPushedRow
	for _, e := range entries {
		if e.StageID == nil || *e.StageID != stageID {
			continue
		}
		var r conflictResolutionPushedRow
		if json.Unmarshal(e.Payload, &r) != nil || r.HeadSHA != headSHA {
			continue
		}
		row = &r
	}
	if row == nil || len(row.ResolvedPaths) == 0 {
		return none
	}
	fr := conflictResolutionFraming{
		ResolvedPaths:          row.ResolvedPaths,
		ResolvedPathsTruncated: row.ResolvedPathsTruncated,
	}
	unavailable := func(cause string) conflictResolutionFraming {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "conflict-resolution review: resolution summary unavailable",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("head_sha", headSHA),
			slog.String("cause", cause))
		fr.UnavailableSlug = prompt.ConflictResolutionDiffUnavailable
		return fr
	}
	if row.ResolutionDiffUnavailable != "" {
		return unavailable(conflictResolutionDiffStoreFailed)
	}
	id, err := uuid.Parse(row.ResolutionDiffArtifactID)
	if err != nil || s.cfg.ArtifactRepo == nil {
		return unavailable(artifact.ConflictResolutionDiffCauseArtifactMissing)
	}
	a, err := s.cfg.ArtifactRepo.Get(ctx, id)
	if err != nil {
		return unavailable(artifact.ConflictResolutionDiffCauseArtifactMissing)
	}
	content, cause := artifact.CheckConflictResolutionDiff(a, stageID, row.ResolutionDiffSHA256, headSHA)
	if cause != "" {
		return unavailable(cause)
	}
	fr.Patch = content.Patch
	fr.PatchTruncated = content.PatchTruncated
	if content.Patch == "" {
		fr.UnavailableSlug = resolutionPatchUnavailableSlug("", content.PatchUnavailable)
	}
	return fr
}

// conflictResolutionClass is the D3 classification of a conflict-resolution
// report: docs_only or code. There is no third class.
type conflictResolutionClass int

const (
	conflictResolutionClassCode conflictResolutionClass = iota
	conflictResolutionClassDocsOnly
)

// docExtensions are the documentation extensions, matched case-SENSITIVELY.
var docExtensions = []string{".md", ".mdx", ".markdown", ".rst", ".adoc"}

// reviewGoverningPrefixes are the review-governing documentation trees,
// matched under EqualFold.
var reviewGoverningPrefixes = []string{"docs/review/", "docs/spec/", "docs/adr/", "docs/compliance/"}

// isDocumentationPath reports whether p is documentation that governs no
// review: a doc extension (case-sensitive), not an instruction path, no
// .github / .fishhawk segment, no review-governing prefix, and not a declared
// review-convention path. The path is canonicalised (path.Clean) first, so a
// `./`, `.` or empty segment validateResolvedPath admits cannot hide a
// review-governing prefix.
func isDocumentationPath(p string, declared []string) bool {
	p = path.Clean(p)
	ext := false
	for _, e := range docExtensions {
		if strings.HasSuffix(p, e) {
			ext = true
		}
	}
	if !ext {
		return false
	}
	if agentpaths.IsInstructionPath(p) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if strings.EqualFold(seg, ".github") || strings.EqualFold(seg, ".fishhawk") {
			return false
		}
	}
	for _, pre := range reviewGoverningPrefixes {
		if len(p) >= len(pre) && strings.EqualFold(p[:len(pre)], pre) {
			return false
		}
	}
	for _, d := range declared {
		if strings.EqualFold(p, path.Clean(d)) {
			return false
		}
	}
	return true
}

// classifyConflictResolution is D3: docs_only ONLY when a summary is present,
// the list is not truncated, the spec read succeeded, and every path is
// documentation. Everything else is code.
func classifyConflictResolution(paths []string, truncated bool, declared []string, specErr error) conflictResolutionClass {
	if len(paths) == 0 || truncated || specErr != nil {
		return conflictResolutionClassCode
	}
	for _, p := range paths {
		if !isDocumentationPath(p, declared) {
			return conflictResolutionClassCode
		}
	}
	return conflictResolutionClassDocsOnly
}

// declaredConventionsForRun reads the run's declared review-convention paths.
// A nil snapshot declares nothing; an unparseable spec is an error.
func declaredConventionsForRun(runRow *run.Run) ([]string, error) {
	if runRow == nil {
		return nil, errors.New("run row unavailable")
	}
	if len(runRow.WorkflowSpec) == 0 {
		return nil, nil
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		return nil, err
	}
	return declaredConventionPaths(parsed), nil
}

// conflictResolutionCompareBase returns the BaseRef of the stage's newest
// stage_conflict_resolution_triggered row ("" when unresolved). The row is
// consumed by the report but still readable.
func (s *Server) conflictResolutionCompareBase(ctx context.Context, runID, stageID uuid.UUID) string {
	entry, ok := s.newestStageEntry(ctx, runID, stageID, CategoryStageConflictResolutionTriggered)
	if !ok || entry == nil {
		return ""
	}
	var trigger conflictResolutionTrigger
	if json.Unmarshal(entry.Payload, &trigger) != nil || !trigger.populated() {
		return ""
	}
	return trigger.BaseRef
}

// docsOnlyConfirmedByCompare is E5: before a docs-only skip, the forge
// compare's changed-file list for base...head is read and the classification
// is re-run over the UNION of that list and the runner's paths. A failed or
// truncated compare, or a listed path the runner did not claim, fails toward
// review (false). A rename's SOURCE is a listed path too (it vanished at
// head), so moving a review-governing file out of its tree is never skipped
// on the destination's claim alone; a rename whose source the forge did not
// name fails toward review (permission_drift.go reads sources the same way).
func (s *Server) docsOnlyConfirmedByCompare(ctx context.Context, runRow *run.Run, base, headSHA string, paths []string) bool {
	if base == "" || runRow == nil {
		return false
	}
	comparer, scope, repo, reason := s.forgeCompareFor(runRow)
	if reason != "" {
		return false
	}
	cmp, err := comparer.ComparePatch(ctx, scope, repo, base, headSHA)
	if err != nil || cmp == nil || cmp.Truncated {
		return false
	}
	claimed := make(map[string]bool, len(paths))
	for _, p := range paths {
		claimed[p] = true
	}
	for _, f := range cmp.Files {
		if !claimed[f.Path] {
			return false
		}
		if f.Status == "renamed" && f.PreviousPath == "" {
			return false
		}
		if f.PreviousPath != "" && !claimed[f.PreviousPath] {
			return false
		}
	}
	// Every compare path, rename sources included, is claimed, so the union IS
	// the runner's list, which the caller already classified docs_only.
	return true
}

// maybeDispatchConflictResolutionReview is the automatic dispatch of a
// conflict_resolution_pushed report. It runs after the row append, so the
// stage-keyed replay dedup has already returned on a replay. A decomposed
// parent records a skip (decomposed_parent); a confirmed docs-only report
// records a skip (resolution_docs_only), falling through to review when the
// skip row cannot be appended; every other report starts a full-diff round on
// a detached context tracked by bgReviews.
func (s *Server) maybeDispatchConflictResolutionReview(ctx context.Context, runID uuid.UUID, stage *run.Stage, pr *pullRequestBody, secretGroups int) {
	if conflictReviewDispatchObserved != nil {
		conflictReviewDispatchObserved(runID, stage.ID, pr.HeadSHA)
	}
	stageID := stage.ID
	var runRow *run.Run
	if s.cfg.RunRepo != nil {
		if r, err := s.cfg.RunRepo.GetRun(ctx, runID); err == nil {
			runRow = r
		}
	}
	if runRow != nil && s.isDecomposedParentRun(ctx, runRow) {
		if err := s.recordConflictResolutionReviewSkipped(ctx, runID, stageID, pr, conflictResolutionSkipDecomposedParent, secretGroups); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelError, "conflict-resolution review: decomposed-parent skip row not recorded",
				slog.String("run_id", runID.String()), slog.String("stage_id", stageID.String()))
		}
		return
	}
	base := s.conflictResolutionCompareBase(ctx, runID, stageID)
	declared, specErr := declaredConventionsForRun(runRow)
	if classifyConflictResolution(pr.ResolvedPaths, pr.ResolvedPathsTruncated, declared, specErr) == conflictResolutionClassDocsOnly &&
		s.docsOnlyConfirmedByCompare(ctx, runRow, base, pr.HeadSHA, pr.ResolvedPaths) {
		err := s.recordConflictResolutionReviewSkipped(ctx, runID, stageID, pr, conflictResolutionSkipDocsOnly, secretGroups)
		if err == nil {
			return
		}
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError, "conflict-resolution review: skip row not recorded — reviewing instead",
			slog.String("run_id", runID.String()), slog.String("stage_id", stageID.String()))
	}
	head := pr.HeadSHA
	reviewCtx := context.WithoutCancel(ctx)
	s.bgReviews.Add(1)
	go func() {
		defer s.bgReviews.Done()
		if slug := s.startConflictResolutionRound(reviewCtx, runID, stageID, base, head); slug != "" {
			s.cfg.Logger.LogAttrs(reviewCtx, slog.LevelWarn, "conflict-resolution review: round not started",
				slog.String("run_id", runID.String()), slog.String("stage_id", stageID.String()),
				slog.String("base", base), slog.String("head_sha", head), slog.String("reason", slug))
		}
	}()
}

// recordConflictResolutionReviewSkipped appends the skip row and RETURNS the
// append error, so the docs-only caller can fail toward review.
func (s *Server) recordConflictResolutionReviewSkipped(ctx context.Context, runID, stageID uuid.UUID, pr *pullRequestBody, reason string, secretGroups int) error {
	if s.cfg.AuditRepo == nil {
		return errors.New("audit repo not wired")
	}
	payload := map[string]any{
		"run_id":         runID.String(),
		"stage_id":       stageID.String(),
		"head_sha":       pr.HeadSHA,
		"base_sha":       pr.BaseSHA,
		"reason":         reason,
		"resolved_paths": pr.ResolvedPaths,
	}
	if secretGroups > 0 {
		payload["diff_secret_groups"] = secretGroups
	}
	body, _ := json.Marshal(payload)
	systemKind := audit.ActorKind("system")
	_, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryConflictResolutionReviewSkipped,
		ActorKind: &systemKind,
		Payload:   body,
	})
	return err
}

// startConflictResolutionRound is the round body shared by the automatic
// dispatch, the boot rebuild and the re-run arm. It is SYNCHRONOUS, owns no
// stage edge, and returns "" or a failure slug. The diff is the full
// ComparePatch(base, head); the framing is read through the one loader; the
// dispatch goes through the ordinary #797 same-head dedup.
func (s *Server) startConflictResolutionRound(ctx context.Context, runID, stageID uuid.UUID, base, headSHA string) string {
	if base == "" {
		return conflictResolutionSlugBaseUnknown
	}
	if s.cfg.RunRepo == nil {
		return redispatchSlugForgeCompareUnavail
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		return redispatchSlugForgeCompareUnavail
	}
	comparer, scope, repo, reason := s.forgeCompareFor(runRow)
	if reason != "" {
		return redispatchSlugForgeCompareUnavail
	}
	cmp, err := comparer.ComparePatch(ctx, scope, repo, base, headSHA)
	if err != nil || cmp == nil {
		return redispatchSlugCompareFailed
	}
	diff := consolidatedReviewDiff(cmp)
	framing := s.loadConflictResolutionFraming(ctx, runID, stageID, headSHA)
	gateEvidence := &prompt.GateEvidence{VerifyEvidenceUnavailableReason: conflictResolutionNoVerifyReason}
	ctx = withReviewRoundSource(ctx, reviewRoundSource{Origin: reviewRoundOriginConflictResolution, BaseSHA: base})
	ctx = withConflictResolutionFraming(ctx, framing)
	s.runImplementReviews(ctx, runID, stageID, diff, nil, headSHA, gateEvidence)
	return ""
}

// isConflictResolutionRound reports whether ctx dispatches a
// conflict_resolution round (E3: such a round always reviews the full
// base...merge-head compare, never the #1725 fix-up delta).
func isConflictResolutionRound(ctx context.Context) bool {
	src, ok := reviewRoundSourceFrom(ctx)
	return ok && src.Origin == reviewRoundOriginConflictResolution
}
