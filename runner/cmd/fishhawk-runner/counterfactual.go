package main

// Runner-executed, plan-declared counterfactuals (E85.3 / #4143, Tier 2a).
//
// After an implement (or fix-up) pass's committed-tree verify gate passes, the
// runner executes each counterfactual the approved plan declared against a
// SCRATCH clone of the verified head — never the lineage worktree — and records
// one counterfactual_observed event per delivered spec. The per-spec execution
// (executeCounterfactual) returns a typed observation with no agent.Event or
// trace dependency, so a later battery (#4300) can call it directly; only
// runPlanCounterfactuals wraps the observations into trace events, and
// gate_evidence folds the observations it RETURNS, never a stream event.
//
// Classification reads the `go test -json` (test2json) action stream and the
// gate exec disposition, never free text, and an unmutated baseline must be
// GREEN before the mutated run counts — so an environment failure can never
// masquerade as RED. Long-form contract: runner/README.md § "Plan-declared
// counterfactuals".

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/modfile"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// Counterfactual phase bounds (approved plan step 8 / binding condition C4).
const (
	// counterfactualRunTimeout bounds ONE go test (or go list) run.
	counterfactualRunTimeout = 5 * time.Minute
	// counterfactualPhaseBudget bounds the whole phase: a spec not STARTED
	// inside it is recorded timeout/phase_budget_exhausted.
	counterfactualPhaseBudget = 20 * time.Minute
	// counterfactualLockWait is scripts/test's runner wait budget
	// (VERIFY_LOCK_WAIT_SECONDS) at its poll interval.
	counterfactualLockWait = 600 * time.Second
	counterfactualLockPoll = 2 * time.Second
	// counterfactualLockReacquireDelay (C4) is the minimum gap between
	// releasing the verify lock and taking it again, so a polling
	// scripts/test runner holder can claim it in between.
	counterfactualLockReacquireDelay = 5 * time.Second
)

// Observation vocabulary. Observed is red | green | timeout | error; Baseline
// adds skipped (no baseline ran).
const (
	cfOutcomeRed     = "red"
	cfOutcomeGreen   = "green"
	cfOutcomeTimeout = "timeout"
	cfOutcomeError   = "error"
	cfOutcomeSkipped = "skipped"
)

// Closed reason vocabulary (empty for red and green).
const (
	cfReasonInvalidSpec        = "invalid_spec"
	cfReasonNoVerifiedTree     = "no_verified_tree"
	cfReasonCloneFailed        = "clone_failed"
	cfReasonRestoreFailed      = "restore_failed"
	cfReasonControlNotBuilt    = "control_not_built"
	cfReasonMutationNotApplied = "mutation_not_applied"
	cfReasonMutationAmbiguous  = "mutation_ambiguous"
	cfReasonMutationOutside    = "mutation_outside_control_path"
	cfReasonBaselineNotGreen   = "baseline_not_green"
	cfReasonBuildFailed        = "build_failed"
	cfReasonNoTestsMatched     = "no_tests_matched"
	cfReasonTestCommandFailed  = "test_command_failed"
	cfReasonGateRefused        = "gate_refused"
	cfReasonGateUnavailable    = "gate_unavailable"
	cfReasonLockUnavailable    = "verify_lock_unavailable"
	cfReasonPhaseBudget        = "phase_budget_exhausted"
)

// errVerifyLockUnavailable is the bounded-wait expiry: a holder kept the lock
// for the whole budget. The Go side never prunes or steals.
var errVerifyLockUnavailable = errors.New("verify lock held for the whole wait budget")

// counterfactualExecutor carries the seams and bounds one counterfactual
// phase runs under. Zero values are never used: newCounterfactualExecutor
// fills the production defaults and tests override single fields.
type counterfactualExecutor struct {
	// lockPath is the PRIMARY repository's verify lock; "" when it could not
	// be resolved, which records error/verify_lock_unavailable rather than
	// running unlocked.
	lockPath       string
	pid            int
	runTimeout     time.Duration
	phaseBudget    time.Duration
	lockWait       time.Duration
	lockPoll       time.Duration
	reacquireDelay time.Duration
	// exec is the gate exec seam (runBoundedGateArgvDisposed in production):
	// sanitized env, process-group kill, container path when selected.
	exec func(ctx context.Context, argv []string, dir, lintCacheDir string, timeout time.Duration) (string, int, gateDisposition)
	// materialize builds the scratch clone (materializeGateCheckout).
	materialize func(ctx context.Context, repoDir, headSHA, parent string) (string, error)
	now         func() time.Time
	lastRelease time.Time
}

func newCounterfactualExecutor(ctx context.Context, repoDir string) *counterfactualExecutor {
	lockPath, _ := verifyLockPathFor(ctx, repoDir)
	return &counterfactualExecutor{
		lockPath:       lockPath,
		pid:            os.Getpid(),
		runTimeout:     counterfactualRunTimeout,
		phaseBudget:    counterfactualPhaseBudget,
		lockWait:       counterfactualLockWait,
		lockPoll:       counterfactualLockPoll,
		reacquireDelay: counterfactualLockReacquireDelay,
		exec: func(ctx context.Context, argv []string, dir, lintCacheDir string, timeout time.Duration) (string, int, gateDisposition) {
			return runBoundedGateArgvDisposed(ctx, argv, dir, lintCacheDir, timeout)
		},
		materialize: materializeGateCheckout,
		now:         time.Now,
	}
}

// runPlanCounterfactuals is the main.go entry point: it executes every
// delivered spec against a scratch clone of the verified head and returns
// exactly one observation per spec plus its counterfactual_observed trace
// event (none for zero specs, so gate_evidence stays byte-identical). The
// caller hands the OBSERVATIONS to composeGateEvidence as a runner-owned
// runnerCounterfactualObservations record; the events are trace-only, so a
// counterfactual_observed line in the agent's stream can neither shadow nor
// displace them (the gate_isolation precedent).
// The verified head is the LAST verify_run head whose tree equals
// verifiedTreeSHA — the tree the PR head carries; when there is none every
// spec records error/no_verified_tree.
func runPlanCounterfactuals(ctx context.Context, cfg config, repoDir string, events []agent.Event, verifiedTreeSHA string, specs []upload.CounterfactualSpec, logSink io.Writer) ([]agent.Event, []counterfactualObservationEvidence) {
	if len(specs) == 0 {
		return nil, nil
	}
	head, tree := lastVerifyRunHead(events)
	if verifiedTreeSHA == "" || tree != verifiedTreeSHA {
		head, tree = "", ""
	}
	x := newCounterfactualExecutor(ctx, repoDir)
	obs := runCounterfactualPhase(ctx, x, repoDir, head, tree, specs)
	out := make([]agent.Event, 0, len(obs))
	for _, o := range obs {
		logEvent(logSink, "counterfactual_observed", map[string]string{
			"run_id": cfg.runID, "stage_id": cfg.stageID,
			"index": fmt.Sprint(o.Index), "control_path": o.ControlPath,
			"baseline": o.Baseline, "observed": o.Observed, "reason": o.Reason,
		})
		out = append(out, counterfactualObservedEvent(cfg, o))
	}
	return out, obs
}

// runCounterfactualPhase executes specs strictly sequentially and returns
// exactly one observation per spec, in spec order. It materializes ONE scratch
// clone at headSHA (never repoDir), restores it between specs, and removes it
// at the end. A failed restore poisons the clone, so every later spec records
// error/restore_failed instead of running on a mutated tree.
func runCounterfactualPhase(ctx context.Context, x *counterfactualExecutor, repoDir, headSHA, treeSHA string, specs []upload.CounterfactualSpec) []counterfactualObservationEvidence {
	out := make([]counterfactualObservationEvidence, 0, len(specs))
	all := func(observed, reason string) []counterfactualObservationEvidence {
		for _, s := range specs {
			o := x.baseObservation(s, headSHA, treeSHA)
			o.Observed, o.Reason = observed, reason
			out = append(out, o)
		}
		return out
	}
	if headSHA == "" {
		return all(cfOutcomeError, cfReasonNoVerifiedTree)
	}
	parent, err := os.MkdirTemp("", "fishhawk-counterfactual-")
	if err != nil {
		return all(cfOutcomeError, cfReasonCloneFailed)
	}
	defer func() { _ = os.RemoveAll(parent) }()
	clone, err := x.materialize(ctx, repoDir, headSHA, parent)
	if err != nil {
		return all(cfOutcomeError, cfReasonCloneFailed)
	}
	phaseStart := x.now()
	restoreBroken := false
	for _, s := range specs {
		o := x.baseObservation(s, headSHA, treeSHA)
		switch {
		case restoreBroken:
			o.Observed, o.Reason = cfOutcomeError, cfReasonRestoreFailed
		case x.now().Sub(phaseStart) >= x.phaseBudget:
			o.Observed, o.Reason = cfOutcomeTimeout, cfReasonPhaseBudget
		default:
			o = x.executeCounterfactual(ctx, clone, parent, headSHA, treeSHA, s)
			if err := restoreCounterfactualClone(ctx, clone, headSHA); err != nil {
				restoreBroken = true
			}
		}
		out = append(out, o)
	}
	return out
}

func (x *counterfactualExecutor) baseObservation(s upload.CounterfactualSpec, headSHA, treeSHA string) counterfactualObservationEvidence {
	return counterfactualObservationEvidence{
		Index:          s.Index,
		ControlPath:    s.ControlPath,
		TestRun:        s.TestRun,
		TestPackage:    s.TestPackage,
		HeadSHA:        headSHA,
		TreeSHA:        treeSHA,
		Baseline:       cfOutcomeSkipped,
		TimeoutSeconds: int(x.runTimeout / time.Second),
	}
}

// executeCounterfactual runs ONE spec in clone (a clean checkout of headSHA)
// and returns its typed observation. It is the battery-callable unit (C5): no
// agent.Event, no trace. scratch is a directory OUTSIDE clone for the patch
// file and lint cache. The caller restores clone afterwards.
func (x *counterfactualExecutor) executeCounterfactual(ctx context.Context, clone, scratch, headSHA, treeSHA string, s upload.CounterfactualSpec) counterfactualObservationEvidence {
	start := x.now()
	o := x.baseObservation(s, headSHA, treeSHA)
	finish := func(observed, reason string) counterfactualObservationEvidence {
		o.Observed, o.Reason = observed, reason
		o.DurationMS = x.now().Sub(start).Milliseconds()
		return o
	}
	if !validCounterfactualSpec(s) {
		return finish(cfOutcomeError, cfReasonInvalidSpec)
	}
	// C2: every component of control_path must be a real (non-symlink)
	// directory/file inside the clone before anything runs. The baseline below
	// executes repository code, so applyCounterfactualMutation re-walks it at
	// the mutation boundary too.
	if _, err := containedControlFile(clone, s.ControlPath); err != nil {
		return finish(cfOutcomeError, cfReasonInvalidSpec)
	}
	runDir, pkgArg, err := resolveCounterfactualPackage(clone, s.TestPackage)
	if err != nil {
		return finish(cfOutcomeError, cfReasonInvalidSpec)
	}
	lintCache := filepath.Join(scratch, "golangci-lint-cache")

	// C6: a control the matched packages do not compile reads as a false
	// GREEN, so prove it is built before running anything.
	if observed, reason := x.controlBuilt(ctx, clone, runDir, pkgArg, s.ControlPath, lintCache); observed != "" {
		return finish(observed, reason)
	}

	baseline, baseReason := x.goTest(ctx, runDir, pkgArg, s.TestRun, lintCache)
	o.Baseline = baseline
	if baseline != cfOutcomeGreen {
		switch baseReason {
		case cfReasonLockUnavailable, cfReasonGateRefused, cfReasonGateUnavailable:
			return finish(cfOutcomeError, baseReason)
		}
		return finish(cfOutcomeError, cfReasonBaselineNotGreen)
	}

	sum, reason := applyCounterfactualMutation(ctx, clone, scratch, s)
	if reason != "" {
		return finish(cfOutcomeError, reason)
	}
	o.MutationSHA256 = sum

	observed, mReason := x.goTest(ctx, runDir, pkgArg, s.TestRun, lintCache)
	return finish(observed, mReason)
}

// validCounterfactualSpec re-validates a delivered spec (the backend validated
// the plan, but the runner never trusts the wire): a contained relative
// control path, exactly one well-formed mutation form, a non-blank selector
// and package, and expect == red.
func validCounterfactualSpec(s upload.CounterfactualSpec) bool {
	if !containedRelPath(s.ControlPath) || strings.HasSuffix(s.ControlPath, "_test.go") {
		return false
	}
	if strings.TrimSpace(s.TestRun) == "" || strings.TrimSpace(s.TestPackage) == "" || s.Expect != "red" {
		return false
	}
	switch s.MutationKind {
	case upload.CounterfactualMutationReplace:
		return s.Find != "" && s.Patch == "" && s.Replace != s.Find
	case upload.CounterfactualMutationPatch:
		return strings.TrimSpace(s.Patch) != "" && s.Find == "" && s.Replace == ""
	}
	return false
}

// containedRelPath reports whether p is a clean, relative, slash-separated
// path with no `..` segment and no backslash.
func containedRelPath(p string) bool {
	if strings.TrimSpace(p) == "" || strings.Contains(p, `\`) || strings.HasPrefix(p, "/") || filepath.IsAbs(p) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// containedControlFile is binding condition C2: it Lstats EVERY component of
// rel under clone, refusing any symlink component, a non-directory
// intermediate, or a non-regular leaf, and then requires the EvalSymlinks
// result to stay inside the clone. It returns the absolute control path.
func containedControlFile(clone, rel string) (string, error) {
	parts := strings.Split(filepath.Clean(filepath.FromSlash(rel)), string(filepath.Separator))
	cur := clone
	for i, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", fmt.Errorf("control path %q: bad component %q", rel, p)
		}
		cur = filepath.Join(cur, p)
		fi, err := os.Lstat(cur)
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("control path %q: component %q is a symlink", rel, p)
		}
		last := i == len(parts)-1
		if !last && !fi.IsDir() {
			return "", fmt.Errorf("control path %q: component %q is not a directory", rel, p)
		}
		if last && !fi.Mode().IsRegular() {
			return "", fmt.Errorf("control path %q is not a regular file", rel)
		}
	}
	realClone, err := filepath.EvalSymlinks(clone)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return "", err
	}
	r, err := filepath.Rel(realClone, real)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) || filepath.IsAbs(r) {
		return "", fmt.Errorf("control path %q resolves outside the clone", rel)
	}
	return cur, nil
}

// resolveCounterfactualPackage maps tests.package onto a run directory and a
// `./`-anchored package argument. A clone with a root go.work or go.mod runs
// from the clone root (so the container path, which mounts only the run
// directory, still sees the whole workspace); otherwise it runs from the
// nearest go.mod above the package, with the package made relative to it.
// The `./` anchor is load-bearing: it keeps a bare path from being read as an
// import path and a leading `-` from being read as a flag.
func resolveCounterfactualPackage(clone, pkg string) (runDir, pkgArg string, err error) {
	p := strings.TrimSpace(pkg)
	p = strings.TrimPrefix(p, "./")
	if p == "" {
		p = "."
	}
	if p != "." && p != "..." && !containedRelPath(p) {
		return "", "", fmt.Errorf("package %q is not a contained relative path", pkg)
	}
	recursive := p == "..." || strings.HasSuffix(p, "/...")
	base := strings.TrimSuffix(strings.TrimSuffix(p, "..."), "/")
	if base == "" {
		base = "."
	}
	anchor := func(rel string) string {
		rel = filepath.ToSlash(rel)
		arg := "./" + rel
		if rel == "." {
			arg = "."
		}
		if recursive {
			if rel == "." {
				return "./..."
			}
			return arg + "/..."
		}
		return arg
	}
	if fileExists(filepath.Join(clone, "go.work")) || fileExists(filepath.Join(clone, "go.mod")) {
		return clone, anchor(base), nil
	}
	dir := filepath.Join(clone, filepath.FromSlash(base))
	for {
		if fileExists(filepath.Join(dir, "go.mod")) {
			rel, rerr := filepath.Rel(dir, filepath.Join(clone, filepath.FromSlash(base)))
			if rerr != nil {
				return "", "", rerr
			}
			return dir, anchor(rel), nil
		}
		if dir == clone {
			return "", "", fmt.Errorf("no go.mod above package %q", pkg)
		}
		dir = filepath.Dir(dir)
		if !strings.HasPrefix(dir, clone) {
			return "", "", fmt.Errorf("no go.mod above package %q", pkg)
		}
	}
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// controlImportPath is the import path of the package directory holding
// control (from the nearest go.mod's module directive, read on the HOST side
// so the container path's mount location never matters).
func controlImportPath(clone, control string) (string, error) {
	pkgDir := filepath.Join(clone, filepath.FromSlash(filepath.ToSlash(filepath.Dir(control))))
	dir := pkgDir
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			mod := modfile.ModulePath(data)
			if mod == "" {
				return "", fmt.Errorf("go.mod in %s has no module directive", dir)
			}
			rel, rerr := filepath.Rel(dir, pkgDir)
			if rerr != nil {
				return "", rerr
			}
			if rel == "." {
				return mod, nil
			}
			return mod + "/" + filepath.ToSlash(rel), nil
		}
		if dir == clone || !strings.HasPrefix(filepath.Dir(dir), clone) {
			return "", fmt.Errorf("no go.mod above %s", control)
		}
		dir = filepath.Dir(dir)
	}
}

// counterfactualGoListTemplate prints one "<import path>\t<file>" line per
// compiled Go file, test files included (C6). IgnoredGoFiles is deliberately
// absent: a build-excluded control is exactly what this check refuses.
const counterfactualGoListTemplate = `{{$p := .ImportPath}}` +
	`{{range .GoFiles}}{{$p}}{{"\t"}}{{.}}{{"\n"}}{{end}}` +
	`{{range .CgoFiles}}{{$p}}{{"\t"}}{{.}}{{"\n"}}{{end}}` +
	`{{range .TestGoFiles}}{{$p}}{{"\t"}}{{.}}{{"\n"}}{{end}}` +
	`{{range .XTestGoFiles}}{{$p}}{{"\t"}}{{.}}{{"\n"}}{{end}}`

// controlBuilt is binding condition C6: `go list` over tests.package must
// name control as a compiled file of a matched package. It returns ("", "")
// when it is, else the observation to record (error/control_not_built, or the
// exec disposition's timeout / gate error).
func (x *counterfactualExecutor) controlBuilt(ctx context.Context, clone, runDir, pkgArg, control, lintCache string) (string, string) {
	want, err := controlImportPath(clone, control)
	if err != nil {
		return cfOutcomeError, cfReasonControlNotBuilt
	}
	out, code, disp := x.exec(ctx, []string{"go", "list", "-f", counterfactualGoListTemplate, pkgArg}, runDir, lintCache, x.runTimeout)
	switch disp {
	case gateTimedOut:
		return cfOutcomeTimeout, ""
	case gateRefused, gateCheckoutRefused:
		return cfOutcomeError, cfReasonGateRefused
	case gateUnavailable:
		return cfOutcomeError, cfReasonGateUnavailable
	}
	if code != 0 {
		return cfOutcomeError, cfReasonControlNotBuilt
	}
	wantLine := want + "\t" + filepath.Base(control)
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if strings.TrimRight(sc.Text(), "\r") == wantLine {
			return "", ""
		}
	}
	return cfOutcomeError, cfReasonControlNotBuilt
}

// goTest runs `go test -json -count=1 -run=<run> <pkg>` under the verify
// lock and classifies it. A lock that cannot be taken records
// error/verify_lock_unavailable and runs nothing.
func (x *counterfactualExecutor) goTest(ctx context.Context, runDir, pkgArg, run, lintCache string) (string, string) {
	release, err := x.acquireLock(ctx)
	if err != nil {
		return cfOutcomeError, cfReasonLockUnavailable
	}
	out, code, disp := x.exec(ctx, []string{"go", "test", "-json", "-count=1", "-run=" + run, pkgArg}, runDir, lintCache, x.runTimeout)
	release()
	return classifyGoTestJSON(out, code, disp)
}

// test2jsonEvent is the subset of a `go test -json` event the classifier
// reads (https://pkg.go.dev/cmd/test2json). FailedBuild (Go 1.24) names the
// package whose build failed on a package-level fail action.
type test2jsonEvent struct {
	Action      string `json:"Action"`
	Package     string `json:"Package"`
	Test        string `json:"Test"`
	Output      string `json:"Output"`
	FailedBuild string `json:"FailedBuild"`
}

// classifyGoTestJSON maps one run onto (observed, reason) from the exec
// disposition and the test2json action stream only. Order matters: a timeout
// or gate failure is never read as a verdict; a build failure is error, never
// RED; a test-level fail is RED; no test-level pass/fail at all is
// no_tests_matched (never GREEN); exit 0 with a pass is GREEN; anything else
// is error/test_command_failed.
func classifyGoTestJSON(out string, code int, disp gateDisposition) (string, string) {
	switch disp {
	case gateTimedOut:
		return cfOutcomeTimeout, ""
	case gateRefused, gateCheckoutRefused:
		return cfOutcomeError, cfReasonGateRefused
	case gateUnavailable:
		return cfOutcomeError, cfReasonGateUnavailable
	}
	var buildFail, pkgFail, setupMarker bool
	var testPass, testFail int
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev test2jsonEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch {
		case ev.Action == "build-fail":
			buildFail = true
		case ev.Test == "" && ev.Action == "fail":
			pkgFail = true
			if ev.FailedBuild != "" {
				buildFail = true
			}
		case ev.Test == "" && ev.Action == "output" &&
			(strings.Contains(ev.Output, "[build failed]") || strings.Contains(ev.Output, "[setup failed]")):
			setupMarker = true
		case ev.Test != "" && ev.Action == "fail":
			testFail++
		case ev.Test != "" && ev.Action == "pass":
			testPass++
		}
	}
	switch {
	case buildFail || (pkgFail && setupMarker && testFail == 0):
		return cfOutcomeError, cfReasonBuildFailed
	case testFail > 0:
		return cfOutcomeRed, ""
	case testPass == 0 && code == 0:
		return cfOutcomeError, cfReasonNoTestsMatched
	case testPass > 0 && code == 0:
		return cfOutcomeGreen, ""
	}
	return cfOutcomeError, cfReasonTestCommandFailed
}

// counterfactualMutationSHA256 identifies the applied mutation (C5): the
// SHA-256 of a canonical JSON encoding of its kind, control path and
// find/replace or patch text.
func counterfactualMutationSHA256(s upload.CounterfactualSpec) string {
	b, _ := json.Marshal(struct {
		Kind        string `json:"kind"`
		ControlPath string `json:"control_path"`
		Find        string `json:"find"`
		Replace     string `json:"replace"`
		Patch       string `json:"patch"`
	}{s.MutationKind, s.ControlPath, s.Find, s.Replace, s.Patch})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// applyCounterfactualMutation applies s's mutation inside clone and returns
// its SHA-256, or a closed reason. find/replace requires EXACTLY one
// occurrence of find in the control file. A patch must touch only the
// control path as a plain text modification (C3).
//
// C2 holds HERE, at the mutation boundary, not only before the baseline: the
// baseline ran the repository's own test code, which (through the container
// path's writable checkout mount) may have replaced the control or an
// intermediate directory with a symlink out of the clone. The component walk
// is therefore repeated immediately before anything reads or writes, and the
// find/replace I/O goes through an os.Root on the clone, so a component
// swapped after the walk still cannot redirect it to a host file.
func applyCounterfactualMutation(ctx context.Context, clone, scratch string, s upload.CounterfactualSpec) (string, string) {
	if _, err := containedControlFile(clone, s.ControlPath); err != nil {
		return "", cfReasonInvalidSpec
	}
	switch s.MutationKind {
	case upload.CounterfactualMutationReplace:
		return replaceInControlFile(clone, s)
	case upload.CounterfactualMutationPatch:
		pf, err := os.CreateTemp(scratch, "counterfactual-*.patch")
		if err != nil {
			return "", cfReasonMutationNotApplied
		}
		patchPath := pf.Name()
		_, werr := pf.WriteString(s.Patch)
		cerr := pf.Close()
		if werr != nil || cerr != nil {
			return "", cfReasonMutationNotApplied
		}
		stat, err := gitOut(ctx, clone, "apply", "--numstat", "--summary", patchPath)
		if err != nil {
			return "", cfReasonMutationNotApplied
		}
		if reason := checkPatchConfinedToControl(stat, s.ControlPath); reason != "" {
			return "", reason
		}
		if err := gitRun(ctx, clone, "apply", patchPath); err != nil {
			return "", cfReasonMutationNotApplied
		}
		return counterfactualMutationSHA256(s), ""
	}
	return "", cfReasonInvalidSpec
}

// replaceInControlFile performs the find/replace form through an os.Root
// opened on clone: every component is resolved inside the root, and one that
// resolves outside it (a symlink planted after the C2 walk) fails the read or
// the write rather than following it to a host file.
func replaceInControlFile(clone string, s upload.CounterfactualSpec) (string, string) {
	root, err := os.OpenRoot(clone)
	if err != nil {
		return "", cfReasonMutationNotApplied
	}
	defer func() { _ = root.Close() }()
	rel := filepath.FromSlash(s.ControlPath)
	fi, err := root.Lstat(rel)
	if err != nil || !fi.Mode().IsRegular() {
		return "", cfReasonInvalidSpec
	}
	data, err := root.ReadFile(rel)
	if err != nil {
		return "", cfReasonMutationNotApplied
	}
	switch n := strings.Count(string(data), s.Find); {
	case n == 0:
		return "", cfReasonMutationNotApplied
	case n > 1:
		return "", cfReasonMutationAmbiguous
	}
	mutated := strings.Replace(string(data), s.Find, s.Replace, 1)
	if err := root.WriteFile(rel, []byte(mutated), fi.Mode().Perm()); err != nil {
		return "", cfReasonMutationNotApplied
	}
	return counterfactualMutationSHA256(s), ""
}

// checkPatchConfinedToControl reads `git apply --numstat --summary` output
// (C3): every numstat entry must be a TEXT change (`-\t-` marks binary) to
// exactly control; any summary line (create / delete / rename / copy / mode
// change) is refused. No entry at all means nothing would be applied.
func checkPatchConfinedToControl(stat, control string) string {
	entries := 0
	for _, line := range strings.Split(stat, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(line, " ") {
			// --summary lines are space-indented: create mode, delete mode,
			// rename, copy, mode change. All change more than control's text.
			return cfReasonMutationOutside
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			return cfReasonMutationOutside
		}
		if fields[0] == "-" || fields[1] == "-" {
			return cfReasonMutationOutside
		}
		if fields[2] != control {
			return cfReasonMutationOutside
		}
		entries++
	}
	if entries == 0 {
		return cfReasonMutationNotApplied
	}
	return ""
}

// restoreCounterfactualClone puts clone back to a clean checkout of headSHA
// and PROVES it: a non-empty `git status --porcelain` afterwards is an error.
func restoreCounterfactualClone(ctx context.Context, clone, headSHA string) error {
	if err := gitRun(ctx, clone, "checkout", "--force", headSHA, "--", "."); err != nil {
		return err
	}
	if err := gitRun(ctx, clone, "clean", "-fdx"); err != nil {
		return err
	}
	st, err := gitOut(ctx, clone, "status", "--porcelain")
	if err != nil {
		return err
	}
	if st != "" {
		return fmt.Errorf("clone not clean after restore: %s", st)
	}
	return nil
}

// acquireLock takes the per-repository verify lock as a `<pid>:runner`
// holder — a protocol-compatible SUBSET of scripts/test's lock: one atomic
// symlink(2) (EEXIST while held), a bounded poll, and NEVER a prune or a
// steal. It first honours the C4 re-acquire gap so a polling scripts/test
// holder gets its turn between two of this phase's runs. The returned
// release removes the link only while it still names this holder.
func (x *counterfactualExecutor) acquireLock(ctx context.Context) (func(), error) {
	if x.lockPath == "" {
		return nil, errVerifyLockUnavailable
	}
	if !x.lastRelease.IsZero() {
		if gap := x.reacquireDelay - x.now().Sub(x.lastRelease); gap > 0 {
			if !sleepCtx(ctx, gap) {
				return nil, ctx.Err()
			}
		}
	}
	target := fmt.Sprintf("%d:%s", x.pid, verifyLockOwnerRunner)
	deadline := x.now().Add(x.lockWait)
	for {
		err := os.Symlink(target, x.lockPath)
		if err == nil {
			return func() {
				releaseVerifyLock(x.lockPath, target)
				x.lastRelease = x.now()
			}, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if !x.now().Before(deadline) {
			return nil, errVerifyLockUnavailable
		}
		if !sleepCtx(ctx, x.lockPoll) {
			return nil, ctx.Err()
		}
	}
}

// releaseVerifyLock removes path only while it is still a symlink naming
// target — a lock some other holder now owns is left alone.
func releaseVerifyLock(path, target string) {
	if got, err := os.Readlink(path); err == nil && got == target {
		_ = os.Remove(path)
	}
}
