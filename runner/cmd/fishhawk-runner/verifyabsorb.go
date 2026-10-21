package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"path"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// Outside-only verify flake absorption at the verify-fix loop's terminal
// failure (#4229, maintainer ruling (b)).
//
// When the fix budget is spent and the deciding FULL-form verify failed only
// in packages the change did not touch, runVerifyFixLoop re-runs those
// packages ONCE, scoped, and accepts the tree if that re-run passes and every
// precondition below holds. The helpers here are PURE apart from
// workspaceModuleMap's `git` reads and the two log helpers, so each refusal
// mode is table-testable apart from the loop.
//
// The verify output is UNTRUSTED. The facts the loop cannot read from it — the
// change's patch-coverage verdict over the failed run's profiles, each failed
// module's BUILD/TEST class, and the change's own package dirs — come from
// scripts/test's attest line, gated by a per-invocation crypto/rand nonce that
// scripts/test captures and unsets before any child runs (contract:
// scripts/README.md § "Flake-absorption attest"). Every precondition fails
// CLOSED: a refusal keeps today's terminal failure byte-for-byte.

const (
	// verifyAbsorbNonceEnvVar carries the runner's nonce into the deciding
	// full-form verify. scripts/test reads and unsets it before any child.
	verifyAbsorbNonceEnvVar = "FISHHAWK_VERIFY_ABSORB_NONCE"

	// verifyAbsorbAttestPrefix leads scripts/test's attest line.
	verifyAbsorbAttestPrefix = "fishhawk-verify-absorb-attest"

	// verifyFlakeAbsorbRefusedEvent is the log line + trace event of a
	// refused absorption of an outside-only candidate.
	verifyFlakeAbsorbRefusedEvent = "verify_flake_absorb_refused"

	// verifyFlakeAbsorbedLead leads the verify_summary detail of an absorbed
	// pass, so the review does not read it as a full-loop pass.
	verifyFlakeAbsorbedLead = "verify_flake_absorbed:"
)

// Refusal reasons, one per mode. Each keeps today's terminal failure.
const (
	absorbRefusedAlreadySpent             = "already_spent"
	absorbRefusedNotFullForm              = "not_full_form"
	absorbRefusedParseLossy               = "parse_lossy"
	absorbRefusedAttestMissing            = "attest_missing"
	absorbRefusedPatchCoverageFail        = "patch_coverage_fail"
	absorbRefusedPatchCoverageUnavailable = "patch_coverage_unavailable"
	absorbRefusedModuleNotTestFailure     = "module_not_test_failure"
	absorbRefusedPackageUnmapped          = "package_unmapped"
	absorbRefusedChangedDirsUndecodable   = "changed_dirs_undecodable"
	absorbRefusedChangedPackageFailed     = "changed_package_failed"
	absorbRefusedModuleSetMismatch        = "module_set_mismatch"
	absorbRefusedRecurred                 = "recurred"
	absorbRefusedTreeChanged              = "tree_changed"
	absorbRefusedRerunFailed              = "rerun_failed"
	absorbRefusedRerunNotExecuted         = "rerun_not_executed"
)

// The attest's fail-closed tokens and the one admissible module class.
const (
	attestPatchCoveragePass = "pass"
	attestUndecodable       = "undecodable"
	attestClassTests        = "tests"
)

// newVerifyAbsorbNonce returns a fresh 128-bit nonce as 32 lowercase hex
// characters, or "" when crypto/rand fails — an empty nonce injects nothing,
// so scripts/test prints no attest and the absorb is refused (fail closed).
func newVerifyAbsorbNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// absorbModuleClass is one `<module>:<class>` attest entry.
type absorbModuleClass struct {
	module string
	class  string
}

// absorbAttest is one parsed attest line.
type absorbAttest struct {
	patchCoverage string // pass | fail | unavailable
	// failedModules is nil when modulesUndecodable.
	failedModules      []absorbModuleClass
	modulesUndecodable bool
	// changedDirs is nil when dirsUndecodable.
	changedDirs     []string
	dirsUndecodable bool
}

// parseAbsorbAttest reads the ONE attest line carrying nonce out of a verify
// output. Lines carrying another nonce are ignored (test output, a harness
// fixture or a stale line cannot be the verdict). None, a duplicate
// right-nonce line, or a malformed one (wrong field count or order, an unknown
// verdict or class, an empty entry) returns ok=false. An empty nonce never
// matches.
func parseAbsorbAttest(out, nonce string) (absorbAttest, bool) {
	if nonce == "" {
		return absorbAttest{}, false
	}
	var (
		found absorbAttest
		n     int
		bad   bool
	)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSuffix(line, "\r")
		f := strings.Split(line, " ")
		if len(f) < 2 || f[0] != verifyAbsorbAttestPrefix || f[1] != nonce {
			continue
		}
		n++
		a, ok := parseAbsorbAttestFields(f[2:])
		if !ok {
			bad = true
			continue
		}
		found = a
	}
	if n != 1 || bad {
		return absorbAttest{}, false
	}
	return found, true
}

// parseAbsorbAttestFields parses the three key=value fields after the nonce.
func parseAbsorbAttestFields(f []string) (absorbAttest, bool) {
	var a absorbAttest
	if len(f) != 3 {
		return a, false
	}
	pc, ok := strings.CutPrefix(f[0], "patch_coverage=")
	if !ok {
		return a, false
	}
	switch pc {
	case "pass", "fail", "unavailable":
		a.patchCoverage = pc
	default:
		return a, false
	}
	fm, ok := strings.CutPrefix(f[1], "failed_modules=")
	if !ok {
		return a, false
	}
	if fm == attestUndecodable {
		a.modulesUndecodable = true
	} else {
		for _, e := range strings.Split(fm, ",") {
			mod, class, cut := strings.Cut(e, ":")
			if !cut || mod == "" {
				return a, false
			}
			switch class {
			case "tests", "build", "other", "unclassified":
			default:
				return a, false
			}
			a.failedModules = append(a.failedModules, absorbModuleClass{module: mod, class: class})
		}
	}
	cd, ok := strings.CutPrefix(f[2], "changed_dirs=")
	if !ok {
		return a, false
	}
	if cd == attestUndecodable {
		a.dirsUndecodable = true
	} else {
		for _, d := range strings.Split(cd, ",") {
			if d == "" {
				return a, false
			}
			a.changedDirs = append(a.changedDirs, d)
		}
	}
	return a, true
}

// wsModule is one workspace module: its module path and its repo-relative dir
// ("." for a root module), normalised the way scripts/test names failed
// modules (no leading "./", no trailing "/").
type wsModule struct {
	path string
	dir  string
}

// workspaceModuleMap reads the module layout FROM COMMIT sha — go.work and
// each used go.mod, or a lone root go.mod — so the map describes exactly the
// tree whose verify failed, never the working tree.
func workspaceModuleMap(ctx context.Context, repoDir, sha string) ([]wsModule, error) {
	show := func(p string) ([]byte, error) {
		out, err := exec.CommandContext(ctx, "git", "-C", repoDir, "show", sha+":"+p).Output() //nolint:gosec // fixed git argv; sha is a runner-resolved commit
		if err != nil {
			return nil, fmt.Errorf("git show %s:%s: %w", sha, p, err)
		}
		return out, nil
	}
	ls, err := exec.CommandContext(ctx, "git", "-C", repoDir, "ls-tree", "--name-only", sha, "--", "go.work", "go.mod").Output() //nolint:gosec // fixed git argv
	if err != nil {
		return nil, fmt.Errorf("git ls-tree %s: %w", sha, err)
	}
	present := map[string]bool{}
	for _, l := range strings.Split(strings.TrimSpace(string(ls)), "\n") {
		present[strings.TrimSpace(l)] = true
	}
	modulePath := func(gomod string) (string, error) {
		data, err := show(gomod)
		if err != nil {
			return "", err
		}
		mp := modfile.ModulePath(data)
		if mp == "" {
			return "", fmt.Errorf("%s declares no module path", gomod)
		}
		return mp, nil
	}
	switch {
	case present["go.work"]:
		data, err := show("go.work")
		if err != nil {
			return nil, err
		}
		wf, err := modfile.ParseWork("go.work", data, nil)
		if err != nil {
			return nil, fmt.Errorf("parse go.work: %w", err)
		}
		var mods []wsModule
		for _, u := range wf.Use {
			dir := normModuleDir(u.Path)
			if dir == "" {
				return nil, fmt.Errorf("go.work use %q is not a repo-relative dir", u.Path)
			}
			gomod := "go.mod"
			if dir != "." {
				gomod = dir + "/go.mod"
			}
			mp, err := modulePath(gomod)
			if err != nil {
				return nil, err
			}
			mods = append(mods, wsModule{path: mp, dir: dir})
		}
		if len(mods) == 0 {
			return nil, fmt.Errorf("go.work uses no module")
		}
		return mods, nil
	case present["go.mod"]:
		mp, err := modulePath("go.mod")
		if err != nil {
			return nil, err
		}
		return []wsModule{{path: mp, dir: "."}}, nil
	default:
		return nil, fmt.Errorf("commit %s has neither go.work nor go.mod at its root", sha)
	}
}

// normModuleDir cleans a go.work use path to scripts/test's module naming, or
// returns "" for a path outside the repository (absolute or escaping).
func normModuleDir(p string) string {
	c := path.Clean(strings.TrimPrefix(p, "./"))
	if strings.HasPrefix(c, "/") || c == ".." || strings.HasPrefix(c, "../") {
		return ""
	}
	return c
}

// mapImportPath maps an import path to its repo-relative package dir and the
// owning module, by longest module-path prefix.
func mapImportPath(mods []wsModule, importPath string) (dir string, mod wsModule, ok bool) {
	for _, m := range mods {
		if importPath != m.path && !strings.HasPrefix(importPath, m.path+"/") {
			continue
		}
		if ok && len(m.path) <= len(mod.path) {
			continue
		}
		mod, ok = m, true
	}
	if !ok {
		return "", wsModule{}, false
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(importPath, mod.path), "/")
	return path.Clean(path.Join(mod.dir, rest)), mod, true
}

// flakeAbsorbInput is everything decideFlakeAbsorb reads.
type flakeAbsorbInput struct {
	output    string          // the deciding verify's output
	form      string          // its form; only verifyFormFull is eligible
	disp      gateDisposition // its disposition; only gateExecuted is eligible
	nonce     string          // the nonce injected into it ("" = none)
	scopePkgs []string        // the plan scope's package dirs
	modules   []wsModule      // workspaceModuleMap of the deciding commit
	modErr    error           // its error, if any
	earlier   []verifyFailures
	spent     bool // the once-per-pass absorb was already admitted
	tree      string
}

// flakeAbsorbPlan is an admitted absorption: the scoped re-run's package dirs
// plus the evidence the loop records.
type flakeAbsorbPlan struct {
	dirs          []string // sorted, deduplicated repo-relative dirs (--packages)
	pkgs          []string // the failing import paths
	tests         []string // the failing test names
	failedModules []string // the attested failed modules, sorted
	patchCoverage string
	tree          string // the failed full verify's tree; the re-run must see the same
	output        string // the failed full verify's output
}

// decideFlakeAbsorb decides one exhaustion-time absorb. candidate=false (no
// event, today's path untouched) unless every failing package lies OUTSIDE the
// plan scope (verifyFailureScopeRelation, which already excludes build/setup
// failures, an empty or "." scope set and a lossy package list). For a
// candidate, reason names the refusal ("" = admitted, plan filled).
func decideFlakeAbsorb(in flakeAbsorbInput) (plan flakeAbsorbPlan, reason string, candidate bool) {
	failures := parseVerifyFailures(in.output)
	if verifyFailureScopeRelation(failures, in.scopePkgs) != verifyRelationOutside {
		return flakeAbsorbPlan{}, "", false
	}
	refuse := func(r string) (flakeAbsorbPlan, string, bool) { return flakeAbsorbPlan{}, r, true }
	if in.spent {
		return refuse(absorbRefusedAlreadySpent)
	}
	if in.form != verifyFormFull || in.disp != gateExecuted {
		return refuse(absorbRefusedNotFullForm)
	}
	if failures.lossy {
		return refuse(absorbRefusedParseLossy)
	}
	att, ok := parseAbsorbAttest(in.output, in.nonce)
	if !ok {
		return refuse(absorbRefusedAttestMissing)
	}
	switch att.patchCoverage {
	case attestPatchCoveragePass:
	case "fail":
		return refuse(absorbRefusedPatchCoverageFail)
	default:
		return refuse(absorbRefusedPatchCoverageUnavailable)
	}
	if att.modulesUndecodable || len(att.failedModules) == 0 {
		return refuse(absorbRefusedModuleNotTestFailure)
	}
	attested := map[string]bool{}
	for _, mc := range att.failedModules {
		if mc.class != attestClassTests || mc.module == attestUndecodable {
			return refuse(absorbRefusedModuleNotTestFailure)
		}
		attested[mc.module] = true
	}
	if in.modErr != nil || len(in.modules) == 0 {
		return refuse(absorbRefusedPackageUnmapped)
	}
	scope := map[string]bool{}
	for _, d := range in.scopePkgs {
		scope[d] = true
	}
	owning := map[string]bool{}
	dirSet := map[string]bool{}
	for _, pkg := range failures.pkgs {
		dir, mod, ok := mapImportPath(in.modules, pkg)
		if !ok || dir == "" || strings.ContainsAny(dir, ",\t\n") || strings.HasPrefix(dir, "../") || dir == ".." {
			return refuse(absorbRefusedPackageUnmapped)
		}
		owning[mod.dir] = true
		dirSet[dir] = true
	}
	// C1: "outside" is decided from the DIFF too, not only the plan scope.
	if att.dirsUndecodable {
		return refuse(absorbRefusedChangedDirsUndecodable)
	}
	changed := map[string]bool{}
	for _, d := range att.changedDirs {
		changed[d] = true
	}
	for d := range dirSet {
		if changed[d] || scope[d] {
			return refuse(absorbRefusedChangedPackageFailed)
		}
	}
	if !sameStringSet(owning, attested) {
		return refuse(absorbRefusedModuleSetMismatch)
	}
	// C3: a test or package that already failed earlier in this pass is
	// not a one-off flake.
	for _, e := range in.earlier {
		if intersects(e.tests, failures.tests) || intersects(e.pkgs, failures.pkgs) {
			return refuse(absorbRefusedRecurred)
		}
	}
	plan = flakeAbsorbPlan{
		dirs:          sortedKeysOf(dirSet),
		pkgs:          append([]string{}, failures.pkgs...),
		tests:         append([]string{}, failures.tests...),
		failedModules: sortedKeysOf(attested),
		patchCoverage: att.patchCoverage,
		tree:          in.tree,
		output:        in.output,
	}
	return plan, "", true
}

func sameStringSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func intersects(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func sortedKeysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// absorbRerunRefusal names why a scoped absorb re-run did not absorb:
// rerun_failed when it executed and exited non-zero on a judged tree,
// rerun_not_executed otherwise (skipped clone, refusal, unavailable, timeout,
// an exit-75 temporary failure (gateTempFail), or a verify-lock refusal —
// exit 73 (gateLockRefused); the #3948 text classifier is deleted, #4022). The
// `disp == gateExecuted` conjunct is what keeps both exit-code dispositions
// out of rerun_failed: neither judged the tree. The output parameter is unused
// and kept for the call sites' shape.
func absorbRerunRefusal(outcome string, disp gateDisposition, _ string) string {
	if outcome == "failed" && disp == gateExecuted {
		return absorbRefusedRerunFailed
	}
	return absorbRefusedRerunNotExecuted
}

// logVerifyFlakeAbsorbRefused records a refused absorption as a log line and
// returns the matching trace event.
func logVerifyFlakeAbsorbRefused(logSink io.Writer, cfg config, iteration int, reason string, f verifyFailures) agent.Event {
	_, _ = fmt.Fprintf(logSink,
		`{"event":%q,"run_id":%q,"stage_id":%q,"iteration":%d,"reason":%q,"failing_tests":%q,"failing_packages":%q}`+"\n",
		verifyFlakeAbsorbRefusedEvent, cfg.runID, cfg.stageID, iteration, reason, strings.Join(f.tests, ","), strings.Join(f.pkgs, ","))
	return agent.Event{
		Kind: verifyFlakeAbsorbRefusedEvent,
		Payload: agent.MakePayload(map[string]any{
			"iteration":        iteration,
			"reason":           reason,
			"failing_tests":    nonNilStrings(f.tests),
			"failing_packages": nonNilStrings(f.pkgs),
		}),
	}
}

// logVerifyFlakeAbsorbed records an absorbed flake as a log line and returns
// the verify_flake_absorbed trace event gate_evidence folds.
func logVerifyFlakeAbsorbed(logSink io.Writer, cfg config, iteration int, p flakeAbsorbPlan) agent.Event {
	_, _ = fmt.Fprintf(logSink,
		`{"event":%q,"run_id":%q,"stage_id":%q,"iteration":%d,"tree_sha":%q,"packages":%q,"tests":%q,"patch_coverage":%q}`+"\n",
		verifyFlakeAbsorbedEvent, cfg.runID, cfg.stageID, iteration, p.tree, strings.Join(p.dirs, ","), strings.Join(p.tests, ","), p.patchCoverage)
	return flakeAbsorbedEvent(flakeAbsorbedEvidence{
		Iteration:     iteration,
		TreeSHA:       p.tree,
		Packages:      nonNilStrings(p.pkgs),
		PackageDirs:   nonNilStrings(p.dirs),
		Tests:         nonNilStrings(p.tests),
		FailedModules: nonNilStrings(p.failedModules),
		PatchCoverage: p.patchCoverage,
	})
}

// verifyFlakeAbsorbedDetail is the verify_summary detail of an absorbed pass.
func verifyFlakeAbsorbedDetail(p flakeAbsorbPlan) string {
	tests := strings.Join(p.tests, ", ")
	if tests == "" {
		tests = "(none parsed)"
	}
	return fmt.Sprintf("%s the full verify failed only in package(s) outside this change (%s) and those package(s) passed one scoped re-run on the identical tree; this tree did NOT pass the full test loop. Absorbed test(s): %s. Patch coverage over the failed full run's profiles: %s.",
		verifyFlakeAbsorbedLead, strings.Join(p.dirs, ", "), tests, p.patchCoverage)
}
