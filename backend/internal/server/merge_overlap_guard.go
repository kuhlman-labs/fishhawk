package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Merge overlap guard (E83.61 / #4119): POST /v0/runs/{run_id}/merge refuses
// while ANOTHER live run in the same repository and tenant, outside this run's
// lineage, has an implement-type stage dispatched or running whose effective
// scope intersects the paths THIS pull request changes at the head being
// merged. Merging then moves the base under that agent, which lands it in the
// category-B conflict class. Unlike the advisory #4118 surfaces it FAILS
// CLOSED: every undeterminable state refuses once a live implement exists
// elsewhere. allow_overlap + overlap_reason overrides any refusal and is
// recorded record-before-commit as a merge_overlap_overridden audit row.
// Contract: backend/internal/server/README.md § "Merge overlap guard".
//
// #4582: the delegated may_merge arm of AutoDriveRunGate runs the SAME guard
// (mergeOverlapAutoDriveHold) with no override, and a 409 refusal names other
// runs only to a caller that may read cross-run overlap (overlapDetailsVisible).

// CategoryDelegatedMergeOverlapRefused is the chained audit row the delegated
// may_merge arm appends when the merge overlap guard refuses it (#4582,
// approval condition C5). The payload carries counts plus code, reason and
// head only, NEVER another run's id, stage id, issue ref or path, so it needs
// no run-bound withholding. One row per distinct decision: a refusal equal to
// the latest row (same code, reason, head and counts) appends nothing, so a
// polling campaign driver does not write a row per tick. INTERNAL: registered
// in audit/categories.go, not an issue-comment surface.
const CategoryDelegatedMergeOverlapRefused = "delegated_merge_overlap_refused"

// CategoryMergeOverlapOverridden is the chained audit row handleMergeRun
// appends when an operator overrides a refusing merge overlap guard. It names
// other runs' ids, issue refs and paths, so it is withheld from run-bound
// subjects (withholdScopeOverlapFromRunBound). INTERNAL: registered in
// audit/categories.go, not an issue-comment surface.
const CategoryMergeOverlapOverridden = "merge_overlap_overridden"

// Merge overlap guard error codes. The two 409s are operator-visible
// preconditions; the 502 is a retryable read failure; the 503 is a failed
// record-before-commit override append.
const (
	mergeCodeOverlappingImplement      = "overlapping_implement_in_flight"
	mergeCodeOverlapUndetermined       = "overlap_undetermined"
	mergeCodeOverlapCheckFailed        = "overlap_check_failed"
	mergeCodeOverlapOverrideUnrecorded = "overlap_override_unrecorded"
)

// Reason tokens carried under the allow-listed `reason` detail key. The first
// three ride the 502 overlap_check_failed; the rest the 409
// overlap_undetermined.
const (
	overlapReasonForgeRead               = "forge_read"
	overlapReasonRunList                 = "run_list"
	overlapReasonStageRead               = "stage_read"
	overlapReasonChangedFilesCapped      = "changed_files_capped"
	overlapReasonRenameSourceUnknown     = "rename_source_unknown"
	overlapReasonRunListTruncated        = "run_list_truncated"
	overlapReasonChangedPathsUnavailable = "changed_paths_unavailable"
	overlapReasonScopeUnresolved         = "scope_unresolved"
)

// mergeOverlapReasonMaxBytes caps overlap_reason (approval condition C4): the
// value is stored verbatim on the audit chain, so an unbounded body is refused
// 400 before anything is read.
const mergeOverlapReasonMaxBytes = 2000

// overlapBlockingStageState is the stage-state filter of the guard: only an
// implement-type stage row actually running an agent blocks. pending,
// awaiting_host_dispatch, awaiting_approval, awaiting_scope_decision and every
// terminal state do not — a planned or parked run is not writing files.
func overlapBlockingStageState(st run.StageState) bool {
	return st == run.StageStateDispatched || st == run.StageStateRunning
}

// mergeOverlapBlocker is one other run in a refusal: a confirmed blocker
// (Paths = the shared paths) or an undetermined candidate (Paths empty).
type mergeOverlapBlocker struct {
	RunID          string   `json:"run_id"`
	IssueRef       string   `json:"issue_ref,omitempty"`
	DecomposedFrom string   `json:"decomposed_from,omitempty"`
	StageID        string   `json:"stage_id"`
	StageType      string   `json:"stage_type"`
	StageState     string   `json:"stage_state"`
	Paths          []string `json:"paths"`
}

// mergeOverlapResult is mergeOverlapGuard's answer. Its ZERO VALUE ADMITS.
// Cause is the raw read error on overlap_check_failed: logged, never shipped.
type mergeOverlapResult struct {
	Status       int
	Code         string
	Reason       string
	Message      string
	Details      map[string]any
	Cause        error
	HeadSHA      string
	Blockers     []mergeOverlapBlocker
	Undetermined []mergeOverlapBlocker
}

func (o mergeOverlapResult) refused() bool { return o.Code != "" }

// overlapDetailsVisible reports whether a refusal may name other runs to id
// (#4582). It mirrors GET /v0/runs/{run_id}/scope-overlap's read posture — a
// run-bound subject is refused, and every other caller needs read:audit
// through requireWriteScope's cookie-session bypass — so the merge refusal
// never shows a caller more of other runs than the #4118 surfaces do.
func overlapDetailsVisible(id Identity) bool {
	return !isRunBoundSubject(id.Subject) && (id.TokenID == "" || hasScope(id, scopeGateViewRead))
}

// withheld returns the refusal a caller WITHOUT cross-run read access sees
// (#4582): for the two 409 codes the Details are rebuilt as counts, the head,
// the reason token (overlap_undetermined) or next_step
// (overlapping_implement_in_flight), details_withheld and required_scope, with
// no blocking / undetermined / blocking_run_id / stage_id key, and the
// overlapping_implement_in_flight Message is re-rendered naming no run, stage
// or path (the overlap_undetermined Message already names only a count). A
// 502 overlap_check_failed and an admitting zero value are returned unchanged:
// their details are already only reason / retryable. Blockers, Undetermined,
// HeadSHA, Code, Reason and Status are copied unchanged — the override
// fingerprint and the merge_overlap_overridden payload still read them.
func (o mergeOverlapResult) withheld() mergeOverlapResult {
	if o.Code != mergeCodeOverlappingImplement && o.Code != mergeCodeOverlapUndetermined {
		return o
	}
	out := o
	details := map[string]any{
		"blocking_count":     len(o.Blockers),
		"undetermined_count": len(o.Undetermined),
		"details_withheld":   true,
		"required_scope":     scopeGateViewRead,
	}
	if o.HeadSHA != "" {
		details["head_sha"] = o.HeadSHA
	}
	if o.Code == mergeCodeOverlapUndetermined {
		details["reason"] = o.Reason
	} else {
		details["next_step"] = "fishhawk_await_stage"
		out.Message = fmt.Sprintf("%d live run(s) elsewhere have an implement-type stage dispatched or running on path(s) this pull request changes at head %s. Merging now moves the base under that agent and risks a category-B conflict for its run. The blocking runs, their stages and the shared paths are withheld: reading them requires read:audit (GET /v0/runs/{run_id}/scope-overlap). Wait for the stage to settle with fishhawk_await_stage, then re-invoke the merge; or pass allow_overlap:true with an overlap_reason to merge deliberately (audited as merge_overlap_overridden). No new merge verdict was recorded.",
			len(o.Blockers), o.HeadSHA)
	}
	out.Details = details
	return out
}

// liveOverlapCandidate is a candidate run with a blocking implement stage.
type liveOverlapCandidate struct {
	run   *run.Run
	stage *run.Stage
}

func (c liveOverlapCandidate) blocker(paths []string) mergeOverlapBlocker {
	b := mergeOverlapBlocker{
		RunID:      c.run.ID.String(),
		StageID:    c.stage.ID.String(),
		StageType:  string(c.stage.Type),
		StageState: string(c.stage.State),
		Paths:      paths,
	}
	if b.Paths == nil {
		b.Paths = []string{}
	}
	if c.run.TriggerRef != nil {
		b.IssueRef = *c.run.TriggerRef
	}
	if c.run.DecomposedFrom != nil {
		b.DecomposedFrom = c.run.DecomposedFrom.String()
	}
	return b
}

// overlapCheckFailed builds the 502 refusal. The message is static per
// reason; the raw cause rides Cause (logged) and never reaches the caller.
func overlapCheckFailed(reason string, cause error) mergeOverlapResult {
	what := "the pull request from the forge"
	switch reason {
	case overlapReasonRunList:
		what = "the repository's live runs"
	case overlapReasonStageRead:
		what = "another live run's stages"
	}
	return mergeOverlapResult{
		Status: http.StatusBadGateway,
		Code:   mergeCodeOverlapCheckFailed,
		Reason: reason,
		Message: "the merge overlap guard could not read " + what +
			", so the merge is refused (fail closed). No new merge verdict was recorded and the call is retryable; re-invoke the merge, or pass allow_overlap:true with an overlap_reason to merge deliberately (audited as merge_overlap_overridden).",
		Details: map[string]any{"reason": reason, "retryable": true},
		Cause:   cause,
	}
}

// overlapUndetermined builds the 409 overlap_undetermined refusal.
func overlapUndetermined(reason, headSHA string, undetermined []mergeOverlapBlocker) mergeOverlapResult {
	why := map[string]string{
		overlapReasonChangedFilesCapped:      "the forge truncated this pull request's comparison (the changed-file list reached GitHub's compare cap, or a patch body was omitted), so its changed-path set may be incomplete",
		overlapReasonRenameSourceUnknown:     "the forge reported a renamed file without its source path, so a changed path is unknown",
		overlapReasonRunListTruncated:        fmt.Sprintf("the live-run listing reached its %d-row cap, so a live run may have gone unscanned", scopeOverlapListLimit),
		overlapReasonChangedPathsUnavailable: "this deployment cannot read this run's changed paths (no GitHub client, no installation, an unparseable repo or no pull request number; GitLab runs land here)",
		overlapReasonScopeUnresolved:         "a live run's effective scope could not be resolved (no readable plan, or a read failed)",
	}[reason]
	if undetermined == nil {
		undetermined = []mergeOverlapBlocker{}
	}
	details := map[string]any{
		"reason":       reason,
		"blocking":     []mergeOverlapBlocker{},
		"undetermined": undetermined,
	}
	if headSHA != "" {
		details["head_sha"] = headSHA
	}
	return mergeOverlapResult{
		Status: http.StatusConflict,
		Code:   mergeCodeOverlapUndetermined,
		Reason: reason,
		Message: fmt.Sprintf("the merge overlap guard cannot rule out that this pull request overlaps an implement stage live on another run (%d undetermined): %s. It fails closed. Clear the cause and re-invoke the merge, or pass allow_overlap:true with an overlap_reason to merge deliberately (an audited operator judgement, recorded as merge_overlap_overridden). No new merge verdict was recorded.",
			len(undetermined), why),
		Details:      details,
		HeadSHA:      headSHA,
		Undetermined: undetermined,
	}
}

// mergeOverlapGuard decides whether runRow's merge must be refused because
// another live run's implement stage overlaps the PR's changed paths. Order:
//
//	(a) overlapCandidates (the #4118 enumeration): a ListRuns error → 502
//	    run_list; a truncated listing → 409 run_list_truncated;
//	(b) every candidate's stages: a read error → 502 stage_read; a candidate
//	    is LIVE when an implement-type stage is overlapBlockingStageState;
//	(c) no live candidate → admit with NO forge read;
//	(d) the PR's changed paths: GitHub anchors unresolved → 409
//	    changed_paths_unavailable; GetPullRequest error, empty head/base or a
//	    ComparePatch(base_ref, head) error → 502 forge_read; a merged PR
//	    admits (the #3622 already_merged resume); every file Path plus every
//	    rename PreviousPath; a truncated comparison or a rename with no
//	    source leaves the set INCOMPLETE;
//	(e) each live candidate's effective scope (resolveOverlapScope):
//	    unresolved → undetermined; a non-empty intersection → blocker;
//	(f) blockers → 409 overlapping_implement_in_flight (wins; undetermined is
//	    still carried); else an incomplete changed set (every non-blocking
//	    live candidate undetermined) → 409 changed_files_capped /
//	    rename_source_unknown; else any unresolved scope → 409
//	    scope_unresolved; else admit.
func (s *Server) mergeOverlapGuard(ctx context.Context, runRow *run.Run) mergeOverlapResult {
	cands, truncated, err := s.overlapCandidates(ctx, runRow)
	if err != nil {
		return overlapCheckFailed(overlapReasonRunList, err)
	}
	if truncated {
		return overlapUndetermined(overlapReasonRunListTruncated, "", nil)
	}

	var live []liveOverlapCandidate
	for _, cand := range cands {
		stages, err := s.cfg.RunRepo.ListStagesForRun(ctx, cand.ID)
		if err != nil {
			return overlapCheckFailed(overlapReasonStageRead, err)
		}
		var blocking *run.Stage
		for _, st := range stages {
			if st == nil || st.Type != run.StageTypeImplement || !overlapBlockingStageState(st.State) {
				continue
			}
			if blocking == nil || st.Sequence < blocking.Sequence {
				blocking = st
			}
		}
		if blocking != nil {
			live = append(live, liveOverlapCandidate{run: cand, stage: blocking})
		}
	}
	if len(live) == 0 {
		return mergeOverlapResult{}
	}
	sort.Slice(live, func(i, j int) bool {
		ri, rj := "", ""
		if live[i].run.TriggerRef != nil {
			ri = *live[i].run.TriggerRef
		}
		if live[j].run.TriggerRef != nil {
			rj = *live[j].run.TriggerRef
		}
		if ri != rj {
			return ri < rj
		}
		return live[i].run.ID.String() < live[j].run.ID.String()
	})

	unavailable := func() mergeOverlapResult {
		und := make([]mergeOverlapBlocker, 0, len(live))
		for _, c := range live {
			und = append(und, c.blocker(nil))
		}
		return overlapUndetermined(overlapReasonChangedPathsUnavailable, "", und)
	}
	if s.cfg.GitHub == nil || runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return unavailable()
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return unavailable()
	}
	prNumber := parsePRNumberFromURL(runRow.PullRequestURL)
	if prNumber <= 0 {
		return unavailable()
	}
	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)
	pr, err := s.cfg.GitHub.GetPullRequest(ctx, scope, repo, prNumber)
	if err != nil {
		return overlapCheckFailed(overlapReasonForgeRead, err)
	}
	if pr.Merged {
		return mergeOverlapResult{}
	}
	if pr.HeadSHA == "" || pr.BaseRef == "" {
		return overlapCheckFailed(overlapReasonForgeRead, errors.New("the forge returned an empty head sha or base ref"))
	}
	cmp, err := s.cfg.GitHub.ComparePatch(ctx, scope, repo, pr.BaseRef, pr.HeadSHA)
	if err != nil {
		return overlapCheckFailed(overlapReasonForgeRead, err)
	}
	changed := map[string]struct{}{}
	incomplete := ""
	if cmp.Truncated {
		incomplete = overlapReasonChangedFilesCapped
	}
	for _, f := range cmp.Files {
		changed[f.Path] = struct{}{}
		if f.PreviousPath != "" {
			changed[f.PreviousPath] = struct{}{}
		} else if f.Status == "renamed" && incomplete == "" {
			// An empty rename source is "source unknown", not "no source"
			// (githubclient/README.md): fail closed (approval condition C2).
			incomplete = overlapReasonRenameSourceUnknown
		}
	}

	out := mergeOverlapResult{HeadSHA: pr.HeadSHA}
	for _, c := range live {
		otherScope, ok := s.resolveOverlapScope(ctx, c.run)
		if !ok {
			out.Undetermined = append(out.Undetermined, c.blocker(nil))
			continue
		}
		var paths []string
		for p := range otherScope {
			if _, hit := changed[p]; hit {
				paths = append(paths, p)
			}
		}
		if len(paths) > 0 {
			sort.Strings(paths)
			out.Blockers = append(out.Blockers, c.blocker(paths))
		} else if incomplete != "" {
			out.Undetermined = append(out.Undetermined, c.blocker(nil))
		}
	}

	switch {
	case len(out.Blockers) > 0:
		first := out.Blockers[0]
		und := out.Undetermined
		if und == nil {
			und = []mergeOverlapBlocker{}
		}
		more := ""
		if n := len(out.Blockers) - 1; n > 0 {
			more = fmt.Sprintf(" (and %d more overlapping run(s))", n)
		}
		out.Status = http.StatusConflict
		out.Code = mergeCodeOverlappingImplement
		out.Message = fmt.Sprintf("run %s has its %s stage %s %s on %d path(s) this pull request changes at head %s%s. Merging now moves the base under that agent and risks a category-B conflict for its run. Wait for the stage to settle with fishhawk_await_stage, then re-invoke the merge; or pass allow_overlap:true with an overlap_reason to merge deliberately (audited as merge_overlap_overridden). No new merge verdict was recorded.",
			first.RunID, first.StageType, first.StageID, first.StageState, len(first.Paths), pr.HeadSHA, more)
		out.Details = map[string]any{
			"blocking":        out.Blockers,
			"undetermined":    und,
			"head_sha":        pr.HeadSHA,
			"next_step":       "fishhawk_await_stage",
			"blocking_run_id": first.RunID,
			"stage_id":        first.StageID,
		}
		return out
	case len(out.Undetermined) > 0:
		// An incomplete changed set is the broader gap, so it names the reason
		// even when a scope also failed to resolve.
		reason := overlapReasonScopeUnresolved
		if incomplete != "" {
			reason = incomplete
		}
		return overlapUndetermined(reason, pr.HeadSHA, out.Undetermined)
	}
	return mergeOverlapResult{}
}

// writeMergeOverlapRefusal renders a refusing guard: 502 for
// overlap_check_failed (only the allow-listed reason/retryable details plus
// run_id; the raw cause is logged), 409 otherwise, with run_id and pr_url
// merged into the details (mirroring writeMergeCandidateRefusal).
func (s *Server) writeMergeOverlapRefusal(w http.ResponseWriter, r *http.Request, runID uuid.UUID, prURL string, o mergeOverlapResult) {
	details := map[string]any{"run_id": runID.String(), "pr_url": prURL}
	for k, v := range o.Details {
		details[k] = v
	}
	attrs := []slog.Attr{slog.String("run_id", runID.String()), slog.String("code", o.Code), slog.String("reason", o.Reason)}
	if o.Cause != nil {
		attrs = append(attrs, slog.String("error", o.Cause.Error()))
	}
	s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, "merge: overlap guard refused before verdict", attrs...)
	status := o.Status
	if status == 0 {
		status = http.StatusConflict
	}
	s.writeError(w, r, status, o.Code, o.Message, details)
}

// mergeOverlapFingerprint keys override dedupe: the overridden code and
// reason, the head, the sorted run_id:stage_id pairs of every blocking and
// undetermined run, and the operator's reason. A changed blocker set, head or
// reason is a new judgement and gets a new row.
func mergeOverlapFingerprint(o mergeOverlapResult, overlapReason string) string {
	pairs := make([]string, 0, len(o.Blockers)+len(o.Undetermined))
	for _, b := range o.Blockers {
		pairs = append(pairs, "b:"+b.RunID+":"+b.StageID)
	}
	for _, u := range o.Undetermined {
		pairs = append(pairs, "u:"+u.RunID+":"+u.StageID)
	}
	sort.Strings(pairs)
	raw, _ := json.Marshal([]any{o.Code, o.Reason, o.HeadSHA, pairs, overlapReason})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// recordMergeOverlapOverride appends the merge_overlap_overridden row for an
// operator override of a refusing guard, RECORD-BEFORE-COMMIT: the caller
// invokes it before any verdict or merge-intent append and treats an error as
// 503 with nothing merged. overlapReason is stored VERBATIM (approval
// condition C3) as JSON data — never interpolated into a message, prompt or
// log line. A row already carrying the same fingerprint returns nil without
// appending, so fishhawk_merge_run's re-POST loop writes one row per distinct
// judgement; a read error is returned (fail closed).
func (s *Server) recordMergeOverlapOverride(ctx context.Context, runID uuid.UUID, prURL, subject string, o mergeOverlapResult, overlapReason string) error {
	fp := mergeOverlapFingerprint(o, overlapReason)
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryMergeOverlapOverridden)
	if err != nil {
		return err
	}
	for _, row := range rows {
		var p struct {
			Fingerprint string `json:"fingerprint"`
		}
		if row != nil && json.Unmarshal(row.Payload, &p) == nil && p.Fingerprint == fp {
			return nil
		}
	}
	blocking, undetermined := o.Blockers, o.Undetermined
	if blocking == nil {
		blocking = []mergeOverlapBlocker{}
	}
	if undetermined == nil {
		undetermined = []mergeOverlapBlocker{}
	}
	payload, err := json.Marshal(map[string]any{
		"run_id":            runID.String(),
		"pr_url":            prURL,
		"head_sha":          o.HeadSHA,
		"overridden_code":   o.Code,
		"overridden_reason": o.Reason,
		"blocking":          blocking,
		"undetermined":      undetermined,
		"overlap_reason":    overlapReason,
		"fingerprint":       fp,
	})
	if err != nil {
		return err
	}
	actorKind := audit.ActorUser
	params := audit.ChainAppendParams{
		RunID:        runID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryMergeOverlapOverridden,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}
	if _, appendErr := s.cfg.AuditRepo.AppendChained(ctx, params); appendErr != nil {
		return appendErr
	}
	return nil
}

// delegatedOverlapDecision is the decision a delegated_merge_overlap_refused
// row records: counts plus code, reason and head, never another run's id or
// path (#4582, approval condition C5).
type delegatedOverlapDecision struct {
	HeadSHA           string `json:"head_sha"`
	RefusedCode       string `json:"refused_code"`
	RefusedReason     string `json:"refused_reason"`
	BlockingCount     int    `json:"blocking_count"`
	UndeterminedCount int    `json:"undetermined_count"`
}

// recordDelegatedMergeOverlapRefusal appends the delegated_merge_overlap_refused
// row for a delegated may_merge the guard refused (#4582, approval condition
// C5), unless the run's LATEST such row records the same decision: the
// campaign driver re-evaluates a parked run every tick, so one row per
// distinct decision, not per poll. A read or append error is returned; the
// caller logs it and still refuses (the refusal, not the record, is the
// control).
func (s *Server) recordDelegatedMergeOverlapRefusal(ctx context.Context, runRow *run.Run, subject string, o mergeOverlapResult) error {
	decision := delegatedOverlapDecision{
		HeadSHA:           o.HeadSHA,
		RefusedCode:       o.Code,
		RefusedReason:     o.Reason,
		BlockingCount:     len(o.Blockers),
		UndeterminedCount: len(o.Undetermined),
	}
	rows, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, CategoryDelegatedMergeOverlapRefused)
	if err != nil {
		return err
	}
	var latest *audit.Entry
	for _, row := range rows {
		if row != nil && (latest == nil || row.Sequence >= latest.Sequence) {
			latest = row
		}
	}
	if latest != nil {
		var prior delegatedOverlapDecision
		if json.Unmarshal(latest.Payload, &prior) == nil && prior == decision {
			return nil
		}
	}
	prURL := ""
	if runRow.PullRequestURL != nil {
		prURL = *runRow.PullRequestURL
	}
	payload, err := json.Marshal(map[string]any{
		"run_id":             runRow.ID.String(),
		"pr_url":             prURL,
		"head_sha":           decision.HeadSHA,
		"refused_code":       decision.RefusedCode,
		"refused_reason":     decision.RefusedReason,
		"blocking_count":     decision.BlockingCount,
		"undetermined_count": decision.UndeterminedCount,
	})
	if err != nil {
		return err
	}
	kind := actorKindForSubject(subject)
	_, err = s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:        runRow.ID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryDelegatedMergeOverlapRefused,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	})
	return err
}

// writeMergeOverlapOverrideUnrecorded renders the 503 for a failed override
// record: static message, the cause logged only.
func (s *Server) writeMergeOverlapOverrideUnrecorded(w http.ResponseWriter, r *http.Request, runID uuid.UUID, prURL string, cause error) {
	s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn, "merge: merge_overlap_overridden could not be recorded; refusing (record before commit)",
		slog.String("run_id", runID.String()), slog.String("error", cause.Error()))
	s.writeError(w, r, http.StatusServiceUnavailable, mergeCodeOverlapOverrideUnrecorded,
		"the merge overlap override could not be recorded on the audit chain, so nothing was merged or enqueued and no new merge verdict was recorded (record before commit); re-invoke the merge to retry.",
		map[string]any{"run_id": runID.String(), "pr_url": prURL, "retryable": true})
}
