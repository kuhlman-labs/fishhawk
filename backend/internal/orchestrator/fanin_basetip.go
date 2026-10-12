package orchestrator

// Fan-in on the current base tip (ADR-096 D4 / #4294).
//
// integrateSlices creates the consolidated branch from the base tip only when
// the branch is ABSENT. Before #4294, once an earlier wave boundary
// (IntegrateCompletedWave, /integrate-wave) or a partial pass had created it, a
// later pass merged the remaining slices onto that OLD base, so the final head
// lagged main and the ADR-090 D5 fan-in merge-candidate pass verified a head
// the merge gate then refused as behind. The base fold below closes that: on a pass that has a
// PENDING slice and finds the consolidated branch already present, the freshly
// resolved base tip SHA is merged into the consolidated branch BEFORE the
// pending slices, so the final head is built on the current tip.
//
// Conflict attribution is a best-effort heuristic layered on top. GitHub's 409
// names no paths, so "conflicting base paths" are the files the base delta and
// the slice delta both touch, read from two three-dot compares. Attribution
// NEVER changes routing: an absent compare capability or a failed compare
// records base_attribution "unavailable:<reason>" with no paths.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Base-fold outcomes recorded on slices_integrated.base_fold_outcome.
const (
	// baseFoldCreatedAtTip: the consolidated branch was absent and was
	// created at the resolved base tip, so no fold is needed.
	baseFoldCreatedAtTip = "created_at_tip"
	// baseFoldMerged: the fold created a merge commit (recorded as an
	// integration_commit_recorded row of kind base_fold).
	baseFoldMerged = "merged"
	// baseFoldAlreadyContained: the forge answered 204 — the consolidated
	// branch already contains the base tip.
	baseFoldAlreadyContained = "already_contained"
	// baseFoldConflict: an already-integrated slice conflicts with the new
	// base. The fold is skipped and the pending slices integrate on the old
	// base; the merge-time rebase / conflict-resolution path owns it.
	baseFoldConflict = "conflict"
	// baseFoldNotAttempted: the branch pre-existed but no slice was pending,
	// so the pass must not move a possibly-verified head.
	baseFoldNotAttempted = "not_attempted"
)

// integrationCommitKindBaseFold is the kind stamped on the
// integration_commit_recorded row the base fold writes. It is "base_fold", not
// "base_advance", so it cannot be confused with the merge-candidate
// base_advance cause (approval condition C2).
const integrationCommitKindBaseFold = "base_fold"

// base_attribution values on a slice_integration_conflict payload and on
// slices_integrated.base_fold_conflict.
const (
	// baseAttributionPathsOverlap: at least one file is touched by both the
	// base delta and the slice delta.
	baseAttributionPathsOverlap = "paths_overlap"
	// baseAttributionNoBaseOverlap: both compares were complete and share no
	// file — a slice-vs-slice conflict, not a slice-vs-main one.
	baseAttributionNoBaseOverlap = "no_base_overlap"
	// baseAttributionUnavailablePrefix prefixes every undecidable outcome.
	baseAttributionUnavailablePrefix = "unavailable:"
)

// Reasons appended to baseAttributionUnavailablePrefix.
const (
	attributionReasonUnsupported = "compare_unsupported"
	attributionReasonFailed      = "compare_failed"
	// attributionReasonTruncated is used when no overlap was found but a
	// compare was truncated, so an absence of overlap is NOT confirmed
	// (approval condition C5: never no_base_overlap on an incomplete compare).
	attributionReasonTruncated = "compare_truncated"
)

// compareFilesReader is the OPTIONAL forge capability conflict attribution
// uses. It is type-asserted on o.GitHub rather than added to GitHubAPI, so the
// interface and its test fakes are unchanged; the production
// *githubclient.Client implements it.
type compareFilesReader interface {
	ComparePatch(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, base, head string) (*forge.ComparePatchResult, error)
}

// baseAttribution is the best-effort attribution of a merge conflict to base
// paths. paths is never nil, so the payload always carries an array.
type baseAttribution struct {
	paths            []string
	attribution      string
	compareTruncated bool
}

// foldConflictAttribution is recorded on slices_integrated.base_fold_conflict
// when the fold itself conflicts. sliceIndex/childRunID name the lowest-index
// already-integrated slice whose files overlap the base delta, and are unset
// when no slice could be attributed.
type foldConflictAttribution struct {
	sliceIndex *int
	childRunID string
	baseAttribution
}

// baseFoldRecord carries the base fields recorded on slices_integrated.
type baseFoldRecord struct {
	baseRef  string
	baseSHA  string
	outcome  string
	conflict *foldConflictAttribution
}

// pendingSlices returns the succeeded children NOT covered by the newest
// slices_integrated child_run_ids. A nil integratedIDs (no record, or an
// unreadable one) makes every succeeded child pending — the fold-and-integrate
// direction, matching IntegrateCompletedWave's audit-read posture: a moved
// head is unverified, so the D5 hold and the merge gate require a new pass.
func pendingSlices(succeeded []*run.Run, integratedIDs []string) []*run.Run {
	covered := make(map[string]struct{}, len(integratedIDs))
	for _, id := range integratedIDs {
		covered[id] = struct{}{}
	}
	var out []*run.Run
	for _, c := range succeeded {
		if _, ok := covered[c.ID.String()]; !ok {
			out = append(out, c)
		}
	}
	return out
}

// foldBaseTip merges the resolved base tip SHA into the consolidated branch.
// The head is the SHA, not the branch name, so the fold pins exactly the tip
// the pass read. Returns baseFoldMerged with the merge SHA on a 201,
// baseFoldAlreadyContained on a 204, baseFoldConflict on a 409, and a non-nil
// error on any other forge failure (the caller parks before any slice merge).
func (o *Orchestrator) foldBaseTip(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, consolidated, baseRef, baseSHA string) (string, string, error) {
	msg := fmt.Sprintf("Advance %s onto %s@%s before integrating slices (ADR-096 D4)", consolidated, baseRef, shortSHA(baseSHA))
	mergeSHA, err := o.GitHub.MergeBranch(ctx, scope, repo, consolidated, baseSHA, msg)
	switch {
	case err == nil && mergeSHA != "":
		return baseFoldMerged, mergeSHA, nil
	case err == nil:
		return baseFoldAlreadyContained, "", nil
	case errors.Is(err, forge.ErrMergeConflict):
		return baseFoldConflict, "", nil
	default:
		return "", "", err
	}
}

// shortSHA trims a commit SHA to 12 characters for a commit message.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// compareFiles returns the set of paths a three-dot base...head compare
// reports (a rename contributes its source too) and whether the forge
// truncated the comparison.
func compareFiles(ctx context.Context, cmp compareFilesReader, scope forge.CredentialScope, repo forge.RepoRef, base, head string) (map[string]struct{}, bool, error) {
	res, err := cmp.ComparePatch(ctx, scope, repo, base, head)
	if err != nil {
		return nil, false, err
	}
	if res == nil {
		return nil, false, errors.New("compare returned no result")
	}
	files := make(map[string]struct{}, len(res.Files))
	for _, f := range res.Files {
		if f.Path != "" {
			files[f.Path] = struct{}{}
		}
		if f.PreviousPath != "" {
			files[f.PreviousPath] = struct{}{}
		}
	}
	return files, res.Truncated, nil
}

// boundToScope keeps only the slice-side files inside the child's own scope
// (approval condition C6). A later-wave slice branch is based on the
// consolidated branch, so its three-dot compare against main also lists the
// earlier slices' files; the sub-plan scope removes them. A nil scope (no
// sub-plan scope declared, or the plan is unreadable) leaves files unbounded.
func boundToScope(files, scope map[string]struct{}) map[string]struct{} {
	if scope == nil {
		return files
	}
	out := make(map[string]struct{}, len(files))
	for p := range files {
		if _, ok := scope[p]; ok {
			out[p] = struct{}{}
		}
	}
	return out
}

// sortedIntersection returns the sorted paths present in both sets, never nil.
func sortedIntersection(a, b map[string]struct{}) []string {
	out := []string{}
	for p := range a {
		if _, ok := b[p]; ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// classifyAttribution maps an overlap and the truncation flag to a
// baseAttribution. Found paths are reported even on a truncated compare (they
// are real overlaps; there may be more). An empty overlap on a truncated
// compare is undecidable, never no_base_overlap (approval condition C5).
func classifyAttribution(paths []string, truncated bool) baseAttribution {
	switch {
	case len(paths) > 0:
		return baseAttribution{paths: paths, attribution: baseAttributionPathsOverlap, compareTruncated: truncated}
	case truncated:
		return baseAttribution{paths: []string{}, attribution: baseAttributionUnavailablePrefix + attributionReasonTruncated, compareTruncated: true}
	default:
		return baseAttribution{paths: []string{}, attribution: baseAttributionNoBaseOverlap}
	}
}

// unavailableAttribution is the empty-paths attribution for an undecidable
// comparison.
func unavailableAttribution(reason string) baseAttribution {
	return baseAttribution{paths: []string{}, attribution: baseAttributionUnavailablePrefix + reason}
}

// attributeSliceConflict attributes a pending slice's merge conflict to base
// paths: the files on main since its merge-base with the slice
// (ComparePatch(sliceHead, baseSHA)) intersected with the files on the slice
// branch since its merge-base with main (ComparePatch(baseSHA, sliceHead)),
// the latter bounded by the child's own scope when declared. Never fails: an
// absent capability or a compare error yields an unavailable attribution.
func (o *Orchestrator) attributeSliceConflict(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, baseSHA, sliceHead string, sliceScope map[string]struct{}) baseAttribution {
	cmp, ok := o.GitHub.(compareFilesReader)
	if !ok {
		return unavailableAttribution(attributionReasonUnsupported)
	}
	baseFiles, baseTrunc, err := compareFiles(ctx, cmp, scope, repo, sliceHead, baseSHA)
	if err != nil {
		o.logAttributionFailure(ctx, sliceHead, baseSHA, err)
		return unavailableAttribution(attributionReasonFailed)
	}
	sliceFiles, sliceTrunc, err := compareFiles(ctx, cmp, scope, repo, baseSHA, sliceHead)
	if err != nil {
		o.logAttributionFailure(ctx, baseSHA, sliceHead, err)
		return unavailableAttribution(attributionReasonFailed)
	}
	return classifyAttribution(sortedIntersection(baseFiles, boundToScope(sliceFiles, sliceScope)), baseTrunc || sliceTrunc)
}

// attributeFoldConflict attributes a base-fold conflict to the LOWEST-index
// already-integrated slice whose files (ComparePatch(baseSHA, sliceHead),
// scope-bounded) overlap the base delta since the consolidated branch's
// merge-base with main (ComparePatch(consolidatedTip, baseSHA)). Unattributed
// (no slice index) when none overlaps or attribution is unavailable.
func (o *Orchestrator) attributeFoldConflict(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, parentID uuid.UUID, consolidatedTip, baseSHA string, integrated []*run.Run, sliceScopes map[int]map[string]struct{}) *foldConflictAttribution {
	cmp, ok := o.GitHub.(compareFilesReader)
	if !ok {
		return &foldConflictAttribution{baseAttribution: unavailableAttribution(attributionReasonUnsupported)}
	}
	baseDelta, truncated, err := compareFiles(ctx, cmp, scope, repo, consolidatedTip, baseSHA)
	if err != nil {
		o.logAttributionFailure(ctx, consolidatedTip, baseSHA, err)
		return &foldConflictAttribution{baseAttribution: unavailableAttribution(attributionReasonFailed)}
	}
	anyFailed := false
	for _, c := range integrated {
		head := sliceBranch(parentID, *c.SliceIndex)
		sliceFiles, sliceTrunc, err := compareFiles(ctx, cmp, scope, repo, baseSHA, head)
		if err != nil {
			o.logAttributionFailure(ctx, baseSHA, head, err)
			anyFailed = true
			continue
		}
		truncated = truncated || sliceTrunc
		paths := sortedIntersection(baseDelta, boundToScope(sliceFiles, sliceScopes[*c.SliceIndex]))
		if len(paths) > 0 {
			idx := *c.SliceIndex
			return &foldConflictAttribution{
				sliceIndex:      &idx,
				childRunID:      c.ID.String(),
				baseAttribution: classifyAttribution(paths, truncated),
			}
		}
	}
	if anyFailed {
		a := unavailableAttribution(attributionReasonFailed)
		a.compareTruncated = truncated
		return &foldConflictAttribution{baseAttribution: a}
	}
	return &foldConflictAttribution{baseAttribution: classifyAttribution(nil, truncated)}
}

func (o *Orchestrator) logAttributionFailure(ctx context.Context, base, head string, err error) {
	o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: conflict attribution compare failed; recording base_attribution unavailable",
		slog.String("base", base),
		slog.String("head", head),
		slog.String("error", err.Error()))
}

// sliceScopeFiles reads each sub-plan's declared scope.files from the parent's
// approved plan, keyed by slice index. Slices with no declared scope are
// absent (unbounded). Returns nil when the plan is unavailable — attribution
// then runs unbounded, never fails.
func (o *Orchestrator) sliceScopeFiles(ctx context.Context, parentID uuid.UUID) map[int]map[string]struct{} {
	if o.Artifacts == nil || o.Runs == nil {
		return nil
	}
	stages, err := o.Runs.ListStagesForRun(ctx, parentID)
	if err != nil {
		return nil
	}
	p, _, err := o.loadApprovedPlan(ctx, stages)
	if err != nil || p == nil || p.Decomposition == nil {
		return nil
	}
	out := make(map[int]map[string]struct{}, len(p.Decomposition.SubPlans))
	for i, sp := range p.Decomposition.SubPlans {
		if sp.Scope == nil || len(sp.Scope.Files) == 0 {
			continue
		}
		set := make(map[string]struct{}, len(sp.Scope.Files))
		for _, f := range sp.Scope.Files {
			set[f.Path] = struct{}{}
		}
		out[i] = set
	}
	return out
}

// sliceConflictDetail renders the human-display conflict message. The stable
// "slice integration conflict: " prefix is unchanged; base paths are appended
// when attribution found any.
func sliceConflictDetail(sliceIndex int, childRunID uuid.UUID, consolidated string, paths []string) string {
	detail := fmt.Sprintf("slice integration conflict: slice %d (child run %s) could not merge onto %s",
		sliceIndex, childRunID, consolidated)
	if len(paths) > 0 {
		detail += "; conflicting base paths: " + strings.Join(paths, ", ")
	}
	return detail
}

// emitBaseFoldRecorded writes the integration_commit_recorded row for a base
// fold merge commit (kind base_fold). It carries merge_sha like every slice
// row — the only field the lineage ledger, auditcomplete, the consolidated
// fan-in head reader and permission drift decode — but no slice_index or
// child_run_id. It is appended before the pass's slice rows, so the newest row
// stays the consolidated tip. Best-effort, mirroring
// emitIntegrationCommitRecorded.
func (o *Orchestrator) emitBaseFoldRecorded(ctx context.Context, parentRunID uuid.UUID, mergeSHA, consolidatedBranch, baseRef, baseSHA string) {
	if o.Audit == nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: Audit not configured; skipping base_fold integration_commit_recorded entry",
			slog.String("parent_run_id", parentRunID.String()))
		return
	}
	payload, err := json.Marshal(map[string]any{
		"merge_sha":           mergeSHA,
		"consolidated_branch": consolidatedBranch,
		"base_ref":            baseRef,
		"base_sha":            baseSHA,
		"kind":                integrationCommitKindBaseFold,
	})
	if err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: marshal base_fold integration_commit_recorded payload failed",
			slog.String("error", err.Error()))
		return
	}
	systemKind := audit.ActorSystem
	if _, err := o.Audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     parentRunID,
		Timestamp: time.Now().UTC(),
		Category:  "integration_commit_recorded",
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		o.logger().LogAttrs(ctx, slog.LevelWarn, "orchestrator: append base_fold integration_commit_recorded failed",
			slog.String("error", err.Error()))
	}
}

// payload renders base_fold_conflict for slices_integrated.
func (f *foldConflictAttribution) payload() map[string]any {
	m := map[string]any{
		"conflicting_base_paths": f.paths,
		"base_attribution":       f.attribution,
		"compare_truncated":      f.compareTruncated,
	}
	if f.sliceIndex != nil {
		m["conflicting_slice_index"] = *f.sliceIndex
	}
	if f.childRunID != "" {
		m["conflicting_child_run_id"] = f.childRunID
	}
	return m
}

// containsRun reports whether runs holds a run with r's id.
func containsRun(runs []*run.Run, r *run.Run) bool {
	for _, x := range runs {
		if x.ID == r.ID {
			return true
		}
	}
	return false
}
