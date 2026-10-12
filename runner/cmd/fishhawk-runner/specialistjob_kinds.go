package main

// Specialist-job kinds executed by this file (E85.9 / #4300):
// counterfactual_battery (the E85.3 counterfactual executor plus structured
// probes), inventory (a closed command-id → argv table), and the shared probe
// evaluator plan_crosscheck reuses. Contract: runner/README.md § "Specialist
// jobs".

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

func init() {
	registerSpecialistJobExecutor("counterfactual_battery", executeBatteryJob)
	registerSpecialistJobExecutor("inventory", executeInventoryJob)
}

// Inventory output bounds (the schema's evidence-inventory caps).
const (
	inventoryMaxLines     = 2000
	inventoryMaxLineChars = 1000
	// probeFileReadLimit caps a file_contains read.
	probeFileReadLimit = 8 << 20
)

// Probe observation vocabulary and reasons.
const (
	probeObservedPass  = "pass"
	probeObservedFail  = "fail"
	probeObservedError = "error"

	probeReasonInvalid         = "invalid_probe"
	probeReasonCloneFailed     = "clone_failed"
	probeReasonUnresolvableSHA = "unresolvable_sha"
	probeReasonReadFailed      = "read_failed"
)

var sjSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// sjTests is the battery / go_test probe test selector.
type sjTests struct {
	Run     string `json:"run"`
	Package string `json:"package"`
}

// sjBatteryMutation is plan-standard-v1 $defs.counterfactual's field set.
type sjBatteryMutation struct {
	ControlPath string `json:"control_path"`
	Mutation    struct {
		Find    *string `json:"find,omitempty"`
		Replace *string `json:"replace,omitempty"`
		Patch   *string `json:"patch,omitempty"`
	} `json:"mutation"`
	Tests  sjTests `json:"tests"`
	Expect string  `json:"expect"`
}

type sjBatteryInputs struct {
	Mutations []sjBatteryMutation `json:"mutations,omitempty"`
	Probes    []sjProbeQuestion   `json:"probes,omitempty"`
}

// sjProbeQuestion is $defs.probe-question; Probe carries exactly one form.
type sjProbeQuestion struct {
	ID       string  `json:"id"`
	Question string  `json:"question"`
	Probe    sjProbe `json:"probe"`
	Expect   string  `json:"expect"`
}

type sjProbe struct {
	PathExists *struct {
		Path string `json:"path"`
	} `json:"path_exists,omitempty"`
	FileContains *struct {
		Path    string `json:"path"`
		Literal string `json:"literal"`
	} `json:"file_contains,omitempty"`
	GoTest *struct {
		Tests sjTests `json:"tests"`
	} `json:"go_test,omitempty"`
}

// sjProbeEvidence is $defs.probe-evidence.
type sjProbeEvidence struct {
	ID       string `json:"id"`
	Observed string `json:"observed"`
	Expected string `json:"expected"`
	Matched  bool   `json:"matched"`
	Reason   string `json:"reason,omitempty"`
}

type sjBatteryMutationEvidence struct {
	ControlPath    string `json:"control_path"`
	Observed       string `json:"observed"`
	Baseline       string `json:"baseline"`
	Reason         string `json:"reason,omitempty"`
	MutationSHA256 string `json:"mutation_sha256"`
}

type sjBatteryEvidence struct {
	Mutations []sjBatteryMutationEvidence `json:"mutations"`
	Probes    []sjProbeEvidence           `json:"probes"`
}

type sjInventoryInputs struct {
	Command string          `json:"command"`
	Args    json.RawMessage `json:"args"`
}

type sjGitLogArgs struct {
	Paths    []string `json:"paths,omitempty"`
	MaxCount int      `json:"max_count"`
}

type sjLsFilesArgs struct {
	Pathspec   []string `json:"pathspec,omitempty"`
	MaxEntries int      `json:"max_entries"`
}

type sjGoListArgs struct {
	Packages []string `json:"packages"`
}

type sjInventoryEvidence struct {
	Command   string   `json:"command"`
	Lines     []string `json:"lines"`
	Truncated bool     `json:"truncated"`
	ExitCode  int      `json:"exit_code"`
}

// decodeJobInputs strictly decodes raw into v (unknown members refused).
func decodeJobInputs(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after inputs")
	}
	return nil
}

func inconclusive(reason string) specialistJobOutcome {
	return specialistJobOutcome{Verdict: sjVerdictInconclusive, Reason: reason}
}

// headSHA returns the job's validated subject head SHA.
func (r *specialistJobRun) headSHA() (string, bool) {
	sha := r.job.Subject.HeadSHA
	return sha, sjSHAPattern.MatchString(sha)
}

// batterySpec converts one battery mutation to the counterfactual executor's
// spec (control path trimmed here, so the evidence hash equals the one
// executeCounterfactual computes).
func batterySpec(i int, m sjBatteryMutation) upload.CounterfactualSpec {
	s := upload.CounterfactualSpec{
		Index:       i,
		ControlPath: strings.TrimSpace(m.ControlPath),
		TestRun:     m.Tests.Run,
		TestPackage: m.Tests.Package,
		Expect:      m.Expect,
	}
	if m.Mutation.Patch != nil {
		s.MutationKind = upload.CounterfactualMutationPatch
		s.Patch = *m.Mutation.Patch
	} else {
		s.MutationKind = upload.CounterfactualMutationReplace
	}
	if m.Mutation.Find != nil {
		s.Find = *m.Mutation.Find
	}
	if m.Mutation.Replace != nil {
		s.Replace = *m.Mutation.Replace
	}
	return s
}

// executeBatteryJob runs every mutation through runCounterfactualPhase (fresh
// clone per spec, baseline-must-be-green, verify lock, test2json
// classification) and every probe through the shared probe evaluator.
func executeBatteryJob(ctx context.Context, r *specialistJobRun) specialistJobOutcome {
	var in sjBatteryInputs
	if err := decodeJobInputs(r.job.Inputs, &in); err != nil {
		return inconclusive(sjReasonInputsInvalid)
	}
	head, ok := r.headSHA()
	if !ok {
		return inconclusive(sjReasonUnresolvableSHA)
	}
	ev := sjBatteryEvidence{Mutations: []sjBatteryMutationEvidence{}, Probes: []sjProbeEvidence{}}
	if len(in.Mutations) > 0 {
		specs := make([]upload.CounterfactualSpec, len(in.Mutations))
		for i, m := range in.Mutations {
			specs[i] = batterySpec(i, m)
		}
		obs := runCounterfactualPhase(ctx, r.x, r.source, head, "", specs)
		for i, o := range obs {
			ev.Mutations = append(ev.Mutations, sjBatteryMutationEvidence{
				ControlPath:    specs[i].ControlPath,
				Observed:       o.Observed,
				Baseline:       o.Baseline,
				Reason:         o.Reason,
				MutationSHA256: counterfactualMutationSHA256(specs[i]),
			})
		}
	}
	ev.Probes = r.evaluateProbes(ctx, head, in.Probes)
	verdict, reason := batteryVerdict(ev)
	return specialistJobOutcome{Verdict: verdict, Reason: reason, Evidence: ev}
}

// batteryVerdict: fail on any GREEN mutation (the control did not hold) or
// probe mismatch; else inconclusive on any timeout / error or probe error;
// else pass.
func batteryVerdict(ev sjBatteryEvidence) (string, string) {
	for _, m := range ev.Mutations {
		if m.Observed == cfOutcomeGreen {
			return sjVerdictFail, ""
		}
	}
	if probesMismatched(ev.Probes) {
		return sjVerdictFail, ""
	}
	for _, m := range ev.Mutations {
		switch m.Observed {
		case cfOutcomeTimeout:
			return sjVerdictInconclusive, sjReasonTimeout
		case cfOutcomeError:
			return sjVerdictInconclusive, sjReasonExecutionError
		}
	}
	if probesErrored(ev.Probes) {
		return sjVerdictInconclusive, sjReasonProbeError
	}
	return sjVerdictPass, ""
}

// probesMismatched reports a probe that DECIDED against its expectation (an
// error observation is not a decision).
func probesMismatched(ps []sjProbeEvidence) bool {
	for _, p := range ps {
		if p.Observed != probeObservedError && !p.Matched {
			return true
		}
	}
	return false
}

func probesErrored(ps []sjProbeEvidence) bool {
	for _, p := range ps {
		if p.Observed == probeObservedError {
			return true
		}
	}
	return false
}

// evaluateProbes is the shared structured-probe evaluator (battery
// missing-control probes and plan_crosscheck questions). path_exists and
// file_contains read ONE clone of head in which nothing ever executes; each
// go_test probe runs in its OWN fresh clone, discarded after (C3). It returns
// one evidence entry per question, in order.
func (r *specialistJobRun) evaluateProbes(ctx context.Context, head string, qs []sjProbeQuestion) []sjProbeEvidence {
	out := make([]sjProbeEvidence, 0, len(qs))
	var readClone string
	var readErr error
	for _, q := range qs {
		e := sjProbeEvidence{ID: q.ID, Expected: q.Expect}
		var observed, reason string
		switch p := q.Probe; {
		case p.PathExists != nil && p.FileContains == nil && p.GoTest == nil,
			p.FileContains != nil && p.PathExists == nil && p.GoTest == nil:
			if readClone == "" && readErr == nil {
				readClone, readErr = r.freshClone(ctx, head)
			}
			if readErr != nil {
				observed, reason = probeObservedError, probeCloneReason(readErr)
				break
			}
			if p.PathExists != nil {
				observed, reason = probePathExists(readClone, p.PathExists.Path)
			} else {
				observed, reason = probeFileContains(readClone, p.FileContains.Path, p.FileContains.Literal)
			}
		case p.GoTest != nil && p.PathExists == nil && p.FileContains == nil:
			observed, reason = r.probeGoTest(ctx, head, p.GoTest.Tests)
		default:
			observed, reason = probeObservedError, probeReasonInvalid
		}
		e.Observed, e.Reason = observed, reason
		e.Matched = observed != probeObservedError && observed == q.Expect
		out = append(out, e)
	}
	return out
}

func probeCloneReason(err error) string {
	if materializeReason(err) == sjReasonUnresolvableSHA {
		return probeReasonUnresolvableSHA
	}
	return probeReasonCloneFailed
}

// probeRelPath validates a probe path: contained, relative, no leading `-`,
// and never inside the clone's own .git metadata.
func probeRelPath(p string) (string, bool) {
	if !containedRelPath(p) || strings.HasPrefix(p, "-") {
		return "", false
	}
	first := strings.SplitN(filepath.ToSlash(filepath.Clean(p)), "/", 2)[0]
	if first == ".git" || first == "." {
		return "", false
	}
	return filepath.FromSlash(p), true
}

// probePathExists observes pass when path exists in clone, through an
// os.Root so a symlink cannot escape the clone.
func probePathExists(clone, path string) (string, string) {
	rel, ok := probeRelPath(path)
	if !ok {
		return probeObservedError, probeReasonInvalid
	}
	root, err := os.OpenRoot(clone)
	if err != nil {
		return probeObservedError, probeReasonReadFailed
	}
	defer func() { _ = root.Close() }()
	switch _, err := root.Stat(rel); {
	case err == nil:
		return probeObservedPass, ""
	case errors.Is(err, fs.ErrNotExist):
		return probeObservedFail, ""
	}
	return probeObservedError, probeReasonReadFailed
}

// probeFileContains observes pass when the file exists and contains literal
// (a byte substring); a missing file is fail.
func probeFileContains(clone, path, literal string) (string, string) {
	rel, ok := probeRelPath(path)
	if !ok || literal == "" {
		return probeObservedError, probeReasonInvalid
	}
	root, err := os.OpenRoot(clone)
	if err != nil {
		return probeObservedError, probeReasonReadFailed
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return probeObservedFail, ""
	}
	if err != nil {
		return probeObservedError, probeReasonReadFailed
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, probeFileReadLimit))
	if err != nil {
		return probeObservedError, probeReasonReadFailed
	}
	if bytes.Contains(data, []byte(literal)) {
		return probeObservedPass, ""
	}
	return probeObservedFail, ""
}

// probeGoTest runs the named tests in their own fresh clone through
// execInClone (gate exec seam, verify lock) and maps the test2json
// classification: green → pass, red → fail, anything else → error.
func (r *specialistJobRun) probeGoTest(ctx context.Context, head string, t sjTests) (string, string) {
	if strings.TrimSpace(t.Run) == "" {
		return probeObservedError, probeReasonInvalid
	}
	clone, err := r.freshClone(ctx, head)
	if err != nil {
		return probeObservedError, probeCloneReason(err)
	}
	// Discard with a pure-filesystem RemoveAll: the clone executed
	// repository code, so no host git may touch it again.
	defer func() { _ = os.RemoveAll(clone) }()
	runDir, pkgArg, err := resolveCounterfactualPackage(clone, t.Package)
	if err != nil {
		return probeObservedError, probeReasonInvalid
	}
	out, code, disp, err := r.execInClone(ctx, clone, runDir, []string{"go", "test", "-json", "-count=1", "-run=" + t.Run, pkgArg})
	if err != nil {
		return probeObservedError, cfReasonLockUnavailable
	}
	switch observed, reason := classifyGoTestJSON(out, code, disp); observed {
	case cfOutcomeGreen:
		return probeObservedPass, ""
	case cfOutcomeRed:
		return probeObservedFail, ""
	case cfOutcomeTimeout:
		return probeObservedError, "timeout"
	default:
		return probeObservedError, reason
	}
}

// executeInventoryJob maps the closed command id onto a fixed argv, runs it
// in a fresh scratch clone of head (git through jobGit, go list through
// execInClone), and returns the bounded, redacted output lines. pass on exit
// 0, otherwise inconclusive.
func executeInventoryJob(ctx context.Context, r *specialistJobRun) specialistJobOutcome {
	var in sjInventoryInputs
	if err := decodeJobInputs(r.job.Inputs, &in); err != nil {
		return inconclusive(sjReasonInputsInvalid)
	}
	head, ok := r.headSHA()
	if !ok {
		return inconclusive(sjReasonUnresolvableSHA)
	}
	limit, err := inventoryLimit(in)
	if err != nil {
		return inconclusive(sjReasonInputsInvalid)
	}
	clone, err := r.freshClone(ctx, head)
	if err != nil {
		return inconclusive(materializeReason(err))
	}
	out, code, timedOut, runErr := r.runInventoryCommand(ctx, clone, head, in)
	if runErr != nil && code == 0 {
		code = -1
	}
	lines, truncated := boundInventoryLines(out, limit)
	ev := sjInventoryEvidence{Command: in.Command, Lines: lines, Truncated: truncated, ExitCode: code}
	switch {
	case timedOut:
		return specialistJobOutcome{Verdict: sjVerdictInconclusive, Reason: sjReasonTimeout, Evidence: ev}
	case runErr != nil || code != 0:
		return specialistJobOutcome{Verdict: sjVerdictInconclusive, Reason: sjReasonExecutionError, Evidence: ev}
	}
	return specialistJobOutcome{Verdict: sjVerdictPass, Evidence: ev}
}

// inventoryLimit validates the per-command args and returns the line cap.
func inventoryLimit(in sjInventoryInputs) (int, error) {
	switch in.Command {
	case "git_log":
		var a sjGitLogArgs
		if err := decodeJobInputs(in.Args, &a); err != nil {
			return 0, err
		}
		if a.MaxCount < 1 || a.MaxCount > 200 || !allRepoPaths(a.Paths) {
			return 0, errors.New("git_log args out of bounds")
		}
		return a.MaxCount, nil
	case "ls_files":
		var a sjLsFilesArgs
		if err := decodeJobInputs(in.Args, &a); err != nil {
			return 0, err
		}
		if a.MaxEntries < 1 || a.MaxEntries > inventoryMaxLines || !allRepoPaths(a.Pathspec) {
			return 0, errors.New("ls_files args out of bounds")
		}
		return a.MaxEntries, nil
	case "go_list":
		var a sjGoListArgs
		if err := decodeJobInputs(in.Args, &a); err != nil {
			return 0, err
		}
		if len(a.Packages) == 0 || len(a.Packages) > 16 {
			return 0, errors.New("go_list needs 1..16 packages")
		}
		for _, p := range a.Packages {
			if strings.HasPrefix(strings.TrimPrefix(strings.TrimSpace(p), "./"), "-") {
				return 0, fmt.Errorf("package %q", p)
			}
		}
		return inventoryMaxLines, nil
	}
	return 0, fmt.Errorf("command %q is not in the inventory table", in.Command)
}

func allRepoPaths(ps []string) bool {
	if len(ps) > 16 {
		return false
	}
	for _, p := range ps {
		if !containedRelPath(p) || strings.HasPrefix(p, "-") {
			return false
		}
	}
	return true
}

// runInventoryCommand is the closed command-id → argv table. It returns
// stdout, the exit code, whether the gate timed out, and a run error.
func (r *specialistJobRun) runInventoryCommand(ctx context.Context, clone, head string, in sjInventoryInputs) (string, int, bool, error) {
	switch in.Command {
	case "git_log":
		var a sjGitLogArgs
		_ = decodeJobInputs(in.Args, &a)
		argv := []string{"log", "--format=%H%x09%s", "-n", fmt.Sprint(a.MaxCount), head, "--"}
		out, code, err := r.git.Run(ctx, clone, append(argv, a.Paths...)...)
		return out, code, false, err
	case "ls_files":
		var a sjLsFilesArgs
		_ = decodeJobInputs(in.Args, &a)
		out, code, err := r.git.Run(ctx, clone, append([]string{"ls-files", "--"}, a.Pathspec...)...)
		return out, code, false, err
	case "go_list":
		var a sjGoListArgs
		_ = decodeJobInputs(in.Args, &a)
		return r.goListInventory(ctx, clone, a.Packages)
	}
	return "", -1, false, fmt.Errorf("command %q is not in the inventory table", in.Command)
}

// goListInventory runs `go list -f {{.ImportPath}}` once per run directory
// the packages resolve to, under the verify lock through the gate exec seam.
func (r *specialistJobRun) goListInventory(ctx context.Context, clone string, pkgs []string) (string, int, bool, error) {
	var order []string
	byDir := map[string][]string{}
	for _, p := range pkgs {
		runDir, arg, err := resolveCounterfactualPackage(clone, p)
		if err != nil {
			return "", -1, false, err
		}
		if _, seen := byDir[runDir]; !seen {
			order = append(order, runDir)
		}
		byDir[runDir] = append(byDir[runDir], arg)
	}
	var all strings.Builder
	for _, dir := range order {
		argv := append([]string{"go", "list", "-f", "{{.ImportPath}}"}, byDir[dir]...)
		out, code, disp, err := r.execInClone(ctx, clone, dir, argv)
		all.WriteString(out)
		if err != nil {
			return all.String(), -1, false, err
		}
		switch disp {
		case gateTimedOut:
			return all.String(), code, true, nil
		case gateRefused, gateCheckoutRefused, gateUnavailable:
			return all.String(), code, false, errors.New("gate refused or unavailable")
		}
		if code != 0 {
			return all.String(), code, false, nil
		}
	}
	return all.String(), 0, false, nil
}

// boundInventoryLines splits out into at most limit (≤ inventoryMaxLines)
// non-empty lines, each redacted and cut to inventoryMaxLineChars runes.
func boundInventoryLines(out string, limit int) ([]string, bool) {
	if limit <= 0 || limit > inventoryMaxLines {
		limit = inventoryMaxLines
	}
	lines := []string{}
	truncated := false
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		if l == "" {
			continue
		}
		if len(lines) == limit {
			truncated = true
			break
		}
		red, _ := redactString(l)
		if !utf8.ValidString(red) {
			red = strings.ToValidUTF8(red, "�")
		}
		if utf8.RuneCountInString(red) > inventoryMaxLineChars {
			red = string([]rune(red)[:inventoryMaxLineChars])
			truncated = true
		}
		lines = append(lines, red)
	}
	return lines, truncated
}
