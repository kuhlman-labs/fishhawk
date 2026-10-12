package main

// Specialist-job scan kinds (E85.9 / #4300): fanin_build, overlap_scan and
// plan_crosscheck, registered into the specialist-job executor registry.
//
// Every kind starts from a fresh scratch clone of the subject's head SHA
// (specialistJobRun.freshClone) and runs git only through jobGit. The C3
// posture of the counterfactual executor holds here too: every go test runs
// through execInClone (gate exec seam, verify lock) in its OWN freshly
// materialized clone, and no git call ever runs in a clone after code has
// executed in it. fanin_build merges in a clone nothing ever executes in, and
// re-clones that merged tree once per package; overlap_scan executes no code;
// plan_crosscheck reads presence and diffs in its own clone and hands its
// questions to the shared probe evaluator, whose go_test probes each take a
// clone of their own. Contract: runner/README.md § "Specialist jobs".

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
)

func init() {
	registerSpecialistJobExecutor("fanin_build", executeFaninJob)
	registerSpecialistJobExecutor("overlap_scan", executeOverlapJob)
	registerSpecialistJobExecutor("plan_crosscheck", executeCrosscheckJob)
}

// Scan bounds (the schema's evidence caps).
const (
	scanMaxPaths      = 500
	scanMaxPathLen    = 1024
	scanMaxSlices     = 16
	scanMaxPackages   = 16
	scanMaxRuns       = 16
	scanMaxWatchPaths = 32
	scanMaxQuestions  = 16
	scanMaxFocusFiles = 64
	scanMaxCompare    = 8
)

// The fan-in merge commits are scratch-only: they live in a throwaway clone
// that is never pushed (jobGit refuses push), so the identity is a fixed
// runner-local one rather than any operator's.
const (
	faninCommitterName  = "fishhawk-specialist-job"
	faninCommitterEmail = "specialist-job@fishhawk.invalid"
)

// fan-in evidence vocabulary ($defs.evidence-fanin_build).
const (
	faninSliceMerged   = "merged"
	faninSliceConflict = "conflict"
	faninSliceError    = "error"

	faninPkgPass    = "pass"
	faninPkgFail    = "fail"
	faninPkgTimeout = "timeout"
	faninPkgError   = "error"
	faninPkgSkipped = "skipped"

	faninReasonMergeIncomplete = "merge_incomplete"
	faninReasonExecRefused     = "exec_refused"
)

// plan_crosscheck focus vocabulary ($defs.evidence-plan_crosscheck).
const (
	focusPresent = "present"
	focusAbsent  = "absent"
	focusError   = "error"
)

// sjRunRef is $defs.run-ref: a run and the commit range its diff spans.
type sjRunRef struct {
	RunID   string `json:"run_id"`
	HeadSHA string `json:"head_sha"`
	BaseSHA string `json:"base_sha"`
}

type sjFaninSlice struct {
	Name    string `json:"name"`
	HeadSHA string `json:"head_sha"`
}

type sjFaninInputs struct {
	Slices   []sjFaninSlice `json:"slices"`
	Packages []string       `json:"packages"`
}

type sjFaninSliceEvidence struct {
	Name          string   `json:"name"`
	HeadSHA       string   `json:"head_sha"`
	Outcome       string   `json:"outcome"`
	ConflictPaths []string `json:"conflict_paths"`
}

type sjFaninPackageEvidence struct {
	Package string `json:"package"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
}

type sjFaninEvidence struct {
	Slices   []sjFaninSliceEvidence   `json:"slices"`
	Packages []sjFaninPackageEvidence `json:"packages"`
}

type sjOverlapInputs struct {
	Runs       []sjRunRef `json:"runs"`
	WatchPaths []string   `json:"watch_paths,omitempty"`
}

type sjOverlapRunEvidence struct {
	RunID        string   `json:"run_id"`
	Resolved     bool     `json:"resolved"`
	ChangedFiles []string `json:"changed_files"`
	Truncated    bool     `json:"truncated"`
}

type sjOverlapPairEvidence struct {
	RunA      string   `json:"run_a"`
	RunB      string   `json:"run_b"`
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated"`
}

type sjWatchHitEvidence struct {
	RunID     string   `json:"run_id"`
	WatchPath string   `json:"watch_path"`
	Paths     []string `json:"paths"`
}

type sjOverlapEvidence struct {
	Runs      []sjOverlapRunEvidence  `json:"runs"`
	Overlaps  []sjOverlapPairEvidence `json:"overlaps"`
	WatchHits []sjWatchHitEvidence    `json:"watch_hits"`
}

type sjFocusFile struct {
	Path   string `json:"path"`
	Expect string `json:"expect"`
}

type sjCrosscheckInputs struct {
	Questions   []sjProbeQuestion `json:"questions,omitempty"`
	FocusFiles  []sjFocusFile     `json:"focus_files,omitempty"`
	CompareRuns []sjRunRef        `json:"compare_runs,omitempty"`
}

type sjFocusEvidence struct {
	Path      string   `json:"path"`
	Expect    string   `json:"expect"`
	Observed  string   `json:"observed"`
	Matched   bool     `json:"matched"`
	TouchedBy []string `json:"touched_by"`
}

type sjCompareRunEvidence struct {
	RunID        string   `json:"run_id"`
	Resolved     bool     `json:"resolved"`
	OverlapPaths []string `json:"overlap_paths"`
}

type sjCrosscheckEvidence struct {
	Questions   []sjProbeEvidence      `json:"questions"`
	FocusFiles  []sjFocusEvidence      `json:"focus_files"`
	CompareRuns []sjCompareRunEvidence `json:"compare_runs"`
}

// ---- shared git helpers ----

// commitResolves reports whether sha names a commit in clone.
func (r *specialistJobRun) commitResolves(ctx context.Context, clone, sha string) bool {
	out, code, err := r.git.Run(ctx, clone, "rev-parse", "--verify", "--quiet", sha+"^{commit}")
	return err == nil && code == 0 && strings.TrimSpace(out) == sha
}

// ensureCommit makes sha resolvable in clone. A local clone already carries
// the source's whole object store, so the common case resolves at once; a
// commit the source gained after the clone is fetched BY SHA from the source
// through jobGit (protocol v2 serves an unadvertised want). It returns false
// when sha is malformed or still unresolvable.
func (r *specialistJobRun) ensureCommit(ctx context.Context, clone, sha string) bool {
	if !sjSHAPattern.MatchString(sha) {
		return false
	}
	if r.commitResolves(ctx, clone, sha) {
		return true
	}
	// --no-auto-maintenance: a detached `git maintenance run --auto` child
	// would outlive the job and race the scratch parent's removal (#3503).
	if _, code, err := r.git.Run(ctx, clone, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-auto-maintenance", "--", r.source, sha); err != nil || code != 0 {
		return false
	}
	return r.commitResolves(ctx, clone, sha)
}

// diffNames lists the paths base..head changes, renames split into a delete
// and an add so both names count, sorted. reason is "" on success,
// unresolvable_sha when either commit cannot be resolved, execution_error
// when the diff itself fails; truncated reports a capped git stdout.
func (r *specialistJobRun) diffNames(ctx context.Context, clone, base, head string) (paths []string, truncated bool, reason string) {
	if !r.ensureCommit(ctx, clone, base) || !r.ensureCommit(ctx, clone, head) {
		return nil, false, sjReasonUnresolvableSHA
	}
	out, code, err := r.git.Run(ctx, clone, "diff", "--name-only", "--no-renames", "--no-ext-diff", "-z", base, head, "--")
	if err != nil || code != 0 {
		return nil, false, sjReasonExecutionError
	}
	paths, truncated = splitNULPaths(out)
	sort.Strings(paths)
	return paths, truncated, ""
}

// splitNULPaths splits `-z` output. A final fragment with no NUL terminator
// is what a capped stdout leaves behind, so it is dropped and reported.
func splitNULPaths(out string) ([]string, bool) {
	if out == "" {
		return []string{}, false
	}
	parts := strings.Split(out, "\x00")
	truncated := parts[len(parts)-1] != ""
	parts = parts[:len(parts)-1]
	paths := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, truncated
}

// capPaths bounds a path list for evidence: at most scanMaxPaths entries,
// each within the schema's 1..1024 length (an over-long path is dropped).
// truncated reports any drop.
func capPaths(paths []string) ([]string, bool) {
	out := make([]string, 0, min(len(paths), scanMaxPaths))
	truncated := false
	for _, p := range paths {
		switch {
		case p == "" || len(p) > scanMaxPathLen:
			truncated = true
		case len(out) == scanMaxPaths:
			return out, true
		default:
			out = append(out, p)
		}
	}
	return out, truncated
}

// intersectSorted returns the paths in both sorted lists.
func intersectSorted(a, b []string) []string {
	out := []string{}
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return out
}

// ---- fanin_build ----

// executeFaninJob merges each slice head onto subject.head_sha, in order, in
// a scratch clone nothing executes in, recording conflicts; when every slice
// merged it tests each declared package in its own fresh clone of the merged
// tree. fail on any conflict or failing package; inconclusive on any
// timeout / error; else pass.
func executeFaninJob(ctx context.Context, r *specialistJobRun) specialistJobOutcome {
	var in sjFaninInputs
	if err := decodeJobInputs(r.job.Inputs, &in); err != nil || !validFaninInputs(in) {
		return inconclusive(sjReasonInputsInvalid)
	}
	head, ok := r.headSHA()
	if !ok {
		return inconclusive(sjReasonUnresolvableSHA)
	}
	mergeClone, err := r.freshClone(ctx, head)
	if err != nil {
		return inconclusive(materializeReason(err))
	}
	ev := sjFaninEvidence{Slices: []sjFaninSliceEvidence{}, Packages: []sjFaninPackageEvidence{}}
	merged, unresolved := r.mergeSlices(ctx, mergeClone, in.Slices, &ev)
	for _, p := range in.Packages {
		e := sjFaninPackageEvidence{Package: p, Outcome: faninPkgSkipped, Reason: faninReasonMergeIncomplete}
		if merged != "" {
			e.Outcome, e.Reason = r.faninTestPackage(ctx, mergeClone, merged, p)
		}
		ev.Packages = append(ev.Packages, e)
	}
	verdict, reason := faninVerdict(ev, unresolved)
	return specialistJobOutcome{Verdict: verdict, Reason: reason, Evidence: ev}
}

func validFaninInputs(in sjFaninInputs) bool {
	if len(in.Slices) == 0 || len(in.Slices) > scanMaxSlices || len(in.Packages) == 0 || len(in.Packages) > scanMaxPackages {
		return false
	}
	for _, p := range in.Packages {
		if strings.HasPrefix(strings.TrimPrefix(strings.TrimSpace(p), "./"), "-") {
			return false
		}
	}
	return true
}

// mergeSlices merges every slice into clone and fills ev.Slices. It returns
// the merged HEAD when EVERY slice merged ("" otherwise) and whether a slice
// SHA was unresolvable. A conflicting slice records its unmerged paths and is
// aborted, so the next slice merges onto the tree before it; a merge that
// neither merges nor aborts cleanly leaves the remaining slices error.
func (r *specialistJobRun) mergeSlices(ctx context.Context, clone string, slices []sjFaninSlice, ev *sjFaninEvidence) (string, bool) {
	broken := false
	// maintenance.auto=false: `git merge` has no --no-auto-maintenance flag,
	// and its detached maintenance child would outlive the job (#3503).
	for _, kv := range [][2]string{{"user.name", faninCommitterName}, {"user.email", faninCommitterEmail}, {"maintenance.auto", "false"}} {
		if _, code, err := r.git.Run(ctx, clone, "config", kv[0], kv[1]); err != nil || code != 0 {
			broken = true
		}
	}
	allMerged, unresolved := !broken, false
	for _, s := range slices {
		e := sjFaninSliceEvidence{Name: s.Name, HeadSHA: s.HeadSHA, Outcome: faninSliceError, ConflictPaths: []string{}}
		switch {
		case broken:
		case !r.ensureCommit(ctx, clone, s.HeadSHA):
			unresolved = true
		default:
			e.Outcome, e.ConflictPaths, broken = r.mergeOne(ctx, clone, s)
		}
		if e.Outcome != faninSliceMerged {
			allMerged = false
		}
		ev.Slices = append(ev.Slices, e)
	}
	if !allMerged {
		return "", unresolved
	}
	out, code, err := r.git.Run(ctx, clone, "rev-parse", "--verify", "HEAD^{commit}")
	if sha := strings.TrimSpace(out); err == nil && code == 0 && sjSHAPattern.MatchString(sha) {
		return sha, unresolved
	}
	return "", unresolved
}

// mergeOne merges one slice. It returns the slice outcome, the conflicting
// paths, and whether the clone is left unusable for further merges.
func (r *specialistJobRun) mergeOne(ctx context.Context, clone string, s sjFaninSlice) (string, []string, bool) {
	_, code, err := r.git.Run(ctx, clone, "merge", "--no-ff", "--no-edit", "-m", "fishhawk fan-in: "+s.Name, s.HeadSHA)
	if err != nil {
		return faninSliceError, []string{}, true
	}
	if code == 0 {
		return faninSliceMerged, []string{}, false
	}
	conflicts := []string{}
	if out, dcode, derr := r.git.Run(ctx, clone, "diff", "--name-only", "--no-ext-diff", "-z", "--diff-filter=U"); derr == nil && dcode == 0 {
		paths, _ := splitNULPaths(out)
		sort.Strings(paths)
		conflicts, _ = capPaths(paths)
	}
	_, acode, aerr := r.git.Run(ctx, clone, "merge", "--abort")
	broken := aerr != nil || acode != 0
	if len(conflicts) == 0 {
		return faninSliceError, conflicts, broken
	}
	return faninSliceConflict, conflicts, broken
}

// faninTestPackage runs `go test -json -count=1 <pkg>` in a FRESH clone of
// the merged tree (C3), discarded after, and maps the test2json
// classification: green → pass, red → fail, a build failure of the merged
// tree → fail (the slices do not compile together), timeout → timeout,
// anything else → error with its reason.
func (r *specialistJobRun) faninTestPackage(ctx context.Context, mergeClone, merged, pkg string) (string, string) {
	clone, err := r.freshCloneFrom(ctx, mergeClone, merged)
	if err != nil {
		return faninPkgError, cfReasonCloneFailed
	}
	// The clone executes repository code below, so it is discarded with a
	// pure-filesystem RemoveAll; no host git touches it again.
	defer func() { _ = os.RemoveAll(clone) }()
	runDir, pkgArg, err := resolveCounterfactualPackage(clone, pkg)
	if err != nil {
		return faninPkgError, cfReasonInvalidSpec
	}
	out, code, disp, err := r.execInClone(ctx, clone, runDir, []string{"go", "test", "-json", "-count=1", pkgArg})
	if err != nil {
		if errors.Is(err, errSpecialistJobNotScratch) {
			return faninPkgError, faninReasonExecRefused
		}
		return faninPkgError, cfReasonLockUnavailable
	}
	switch observed, reason := classifyGoTestJSON(out, code, disp); {
	case observed == cfOutcomeGreen:
		return faninPkgPass, ""
	case observed == cfOutcomeRed:
		return faninPkgFail, ""
	case observed == cfOutcomeTimeout:
		return faninPkgTimeout, ""
	case reason == cfReasonBuildFailed:
		return faninPkgFail, cfReasonBuildFailed
	default:
		return faninPkgError, reason
	}
}

// freshCloneFrom materializes a new clone of sha from src, one of this job's
// own scratch clones. src must be untainted: cloning runs host git against
// src's repository, which after code executed in it could carry a planted
// config or attribute (the #4143 H1 lesson).
func (r *specialistJobRun) freshCloneFrom(ctx context.Context, src, sha string) (string, error) {
	if err := requireScratchDir(src, r.parent); err != nil {
		return "", err
	}
	if r.git.isTainted(src) {
		return "", fmt.Errorf("%w: clone source %s", errSpecialistJobCloneTainted, src)
	}
	sub, err := os.MkdirTemp(r.parent, "clone-")
	if err != nil {
		return "", err
	}
	dir, err := r.x.materialize(ctx, src, sha, sub)
	if err != nil {
		return "", err
	}
	if err := requireScratchDir(dir, r.parent); err != nil {
		return "", err
	}
	return dir, nil
}

// faninVerdict: fail on any slice conflict or package fail; else
// inconclusive on any slice error or package timeout / error; else pass.
func faninVerdict(ev sjFaninEvidence, unresolved bool) (string, string) {
	for _, s := range ev.Slices {
		if s.Outcome == faninSliceConflict {
			return sjVerdictFail, ""
		}
	}
	for _, p := range ev.Packages {
		if p.Outcome == faninPkgFail {
			return sjVerdictFail, ""
		}
	}
	for _, s := range ev.Slices {
		if s.Outcome == faninSliceError {
			if unresolved {
				return sjVerdictInconclusive, sjReasonUnresolvableSHA
			}
			return sjVerdictInconclusive, sjReasonExecutionError
		}
	}
	for _, p := range ev.Packages {
		switch p.Outcome {
		case faninPkgTimeout:
			return sjVerdictInconclusive, sjReasonTimeout
		case faninPkgError, faninPkgSkipped:
			return sjVerdictInconclusive, sjReasonExecutionError
		}
	}
	return sjVerdictPass, ""
}

// ---- overlap_scan ----

// executeOverlapJob diffs every run's base..head in one scratch clone (no
// code executes), then reports the pairwise intersections and the watch_paths
// glob hits. fail on any overlap or hit; inconclusive when any run's range
// could not be resolved; else pass.
func executeOverlapJob(ctx context.Context, r *specialistJobRun) specialistJobOutcome {
	var in sjOverlapInputs
	if err := decodeJobInputs(r.job.Inputs, &in); err != nil || !validOverlapInputs(in) {
		return inconclusive(sjReasonInputsInvalid)
	}
	head, ok := r.headSHA()
	if !ok {
		return inconclusive(sjReasonUnresolvableSHA)
	}
	clone, err := r.freshClone(ctx, head)
	if err != nil {
		return inconclusive(materializeReason(err))
	}
	ev := sjOverlapEvidence{Runs: []sjOverlapRunEvidence{}, Overlaps: []sjOverlapPairEvidence{}, WatchHits: []sjWatchHitEvidence{}}
	changed := make([][]string, len(in.Runs))
	unresolvedReason := ""
	for i, run := range in.Runs {
		e := sjOverlapRunEvidence{RunID: run.RunID, ChangedFiles: []string{}}
		paths, gitTruncated, reason := r.diffNames(ctx, clone, run.BaseSHA, run.HeadSHA)
		if reason != "" {
			if unresolvedReason == "" {
				unresolvedReason = reason
			}
		} else {
			e.Resolved = true
			changed[i] = paths
			var capTruncated bool
			e.ChangedFiles, capTruncated = capPaths(paths)
			e.Truncated = gitTruncated || capTruncated
		}
		ev.Runs = append(ev.Runs, e)
	}
	for i := range in.Runs {
		for j := i + 1; j < len(in.Runs); j++ {
			if !ev.Runs[i].Resolved || !ev.Runs[j].Resolved {
				continue
			}
			if both := intersectSorted(changed[i], changed[j]); len(both) > 0 {
				paths, truncated := capPaths(both)
				ev.Overlaps = append(ev.Overlaps, sjOverlapPairEvidence{RunA: in.Runs[i].RunID, RunB: in.Runs[j].RunID, Paths: paths, Truncated: truncated})
			}
		}
	}
	for i, run := range in.Runs {
		for _, glob := range in.WatchPaths {
			if hits := watchMatches(glob, changed[i]); len(hits) > 0 {
				paths, _ := capPaths(hits)
				ev.WatchHits = append(ev.WatchHits, sjWatchHitEvidence{RunID: run.RunID, WatchPath: glob, Paths: paths})
			}
		}
	}
	verdict, reason := overlapVerdict(ev, unresolvedReason)
	return specialistJobOutcome{Verdict: verdict, Reason: reason, Evidence: ev}
}

// validOverlapInputs bounds the runs and refuses a malformed glob
// (path.ErrBadPattern), which path.Match would otherwise report only on a
// path that reaches the bad segment.
func validOverlapInputs(in sjOverlapInputs) bool {
	if len(in.Runs) < 2 || len(in.Runs) > scanMaxRuns || len(in.WatchPaths) > scanMaxWatchPaths {
		return false
	}
	for _, g := range in.WatchPaths {
		if g == "" {
			return false
		}
		if _, err := path.Match(g, ""); err != nil {
			return false
		}
	}
	return true
}

// watchMatches returns the paths glob matches (path.Match semantics: `*`
// never crosses a `/`).
func watchMatches(glob string, paths []string) []string {
	var hits []string
	for _, p := range paths {
		if ok, err := path.Match(glob, p); err == nil && ok {
			hits = append(hits, p)
		}
	}
	return hits
}

// overlapVerdict: fail on any overlap or watch hit; else inconclusive with
// the first unresolved run's reason; else pass.
func overlapVerdict(ev sjOverlapEvidence, unresolvedReason string) (string, string) {
	if len(ev.Overlaps) > 0 || len(ev.WatchHits) > 0 {
		return sjVerdictFail, ""
	}
	if unresolvedReason != "" {
		return sjVerdictInconclusive, unresolvedReason
	}
	return sjVerdictPass, ""
}

// ---- plan_crosscheck ----

// executeCrosscheckJob observes each focus file's presence at head and the
// compare runs whose diff touches it, in one scratch clone where no code
// executes, then evaluates every question through the shared probe evaluator
// (each go_test probe in its own fresh clone). fail on any decided question
// or focus mismatch, or a focus file a compare run touches; inconclusive on an
// unresolvable compare run, a focus read error or a probe error; else pass.
func executeCrosscheckJob(ctx context.Context, r *specialistJobRun) specialistJobOutcome {
	var in sjCrosscheckInputs
	if err := decodeJobInputs(r.job.Inputs, &in); err != nil || !validCrosscheckInputs(in) {
		return inconclusive(sjReasonInputsInvalid)
	}
	head, ok := r.headSHA()
	if !ok {
		return inconclusive(sjReasonUnresolvableSHA)
	}
	ev := sjCrosscheckEvidence{Questions: []sjProbeEvidence{}, FocusFiles: []sjFocusEvidence{}, CompareRuns: []sjCompareRunEvidence{}}
	unresolvedReason := ""
	if len(in.FocusFiles) > 0 || len(in.CompareRuns) > 0 {
		clone, err := r.freshClone(ctx, head)
		if err != nil {
			return inconclusive(materializeReason(err))
		}
		unresolvedReason = r.crosscheckFocus(ctx, clone, in, &ev)
	}
	ev.Questions = r.evaluateProbes(ctx, head, in.Questions)
	verdict, reason := crosscheckVerdict(ev, unresolvedReason)
	return specialistJobOutcome{Verdict: verdict, Reason: reason, Evidence: ev}
}

func validCrosscheckInputs(in sjCrosscheckInputs) bool {
	if len(in.Questions) == 0 && len(in.FocusFiles) == 0 {
		return false
	}
	if len(in.Questions) > scanMaxQuestions || len(in.FocusFiles) > scanMaxFocusFiles || len(in.CompareRuns) > scanMaxCompare {
		return false
	}
	for _, f := range in.FocusFiles {
		if f.Expect != focusPresent && f.Expect != focusAbsent {
			return false
		}
	}
	return true
}

// crosscheckFocus fills ev.FocusFiles and ev.CompareRuns from clone and
// returns the first unresolved compare run's reason ("" when all resolved).
func (r *specialistJobRun) crosscheckFocus(ctx context.Context, clone string, in sjCrosscheckInputs, ev *sjCrosscheckEvidence) string {
	for _, f := range in.FocusFiles {
		e := sjFocusEvidence{Path: f.Path, Expect: f.Expect, TouchedBy: []string{}}
		switch observed, _ := probePathExists(clone, f.Path); observed {
		case probeObservedPass:
			e.Observed = focusPresent
		case probeObservedFail:
			e.Observed = focusAbsent
		default:
			e.Observed = focusError
		}
		e.Matched = e.Observed == f.Expect
		ev.FocusFiles = append(ev.FocusFiles, e)
	}
	unresolvedReason := ""
	for _, run := range in.CompareRuns {
		e := sjCompareRunEvidence{RunID: run.RunID, OverlapPaths: []string{}}
		changed, _, reason := r.diffNames(ctx, clone, run.BaseSHA, run.HeadSHA)
		if reason != "" {
			if unresolvedReason == "" {
				unresolvedReason = reason
			}
			ev.CompareRuns = append(ev.CompareRuns, e)
			continue
		}
		e.Resolved = true
		touched := make(map[string]bool, len(changed))
		for _, p := range changed {
			touched[p] = true
		}
		for i := range ev.FocusFiles {
			fp := ev.FocusFiles[i].Path
			if !touched[path.Clean(strings.TrimPrefix(fp, "./"))] {
				continue
			}
			e.OverlapPaths = append(e.OverlapPaths, fp)
			if len(ev.FocusFiles[i].TouchedBy) < scanMaxCompare {
				ev.FocusFiles[i].TouchedBy = append(ev.FocusFiles[i].TouchedBy, run.RunID)
			}
		}
		e.OverlapPaths, _ = capPaths(e.OverlapPaths)
		ev.CompareRuns = append(ev.CompareRuns, e)
	}
	return unresolvedReason
}

// crosscheckVerdict applies the plan_crosscheck verdict rule.
func crosscheckVerdict(ev sjCrosscheckEvidence, unresolvedReason string) (string, string) {
	if probesMismatched(ev.Questions) {
		return sjVerdictFail, ""
	}
	for _, f := range ev.FocusFiles {
		if (f.Observed != focusError && !f.Matched) || len(f.TouchedBy) > 0 {
			return sjVerdictFail, ""
		}
	}
	if unresolvedReason != "" {
		return sjVerdictInconclusive, unresolvedReason
	}
	for _, f := range ev.FocusFiles {
		if f.Observed == focusError {
			return sjVerdictInconclusive, sjReasonProbeError
		}
	}
	if probesErrored(ev.Questions) {
		return sjVerdictInconclusive, sjReasonProbeError
	}
	return sjVerdictPass, ""
}
