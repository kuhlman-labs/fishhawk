package main

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// Verify-failure helpers for the committed-tree verify-fix loop (#4036).
//
// The classifiers here are PURE — no git, no I/O — so each branch is
// table-testable apart from runVerifyFixLoop (only the two log helpers at the
// bottom write, to the caller's sink). Two consumers:
//
//   - the NO-OP fix detection + once-per-stage FLAKE RE-RUN in
//     runVerifyFixLoop: isNoopFixIteration decides "the fix agent changed
//     nothing", parseVerifyFailures + verifyFailureScopeRelation decide whether
//     the failure the agent saw lies entirely OUTSIDE the change's packages;
//   - boundVerifyFixOutput's signal retention (retainFailureSignalLines): the
//     `--- FAIL:` / `FAIL<TAB><pkg>` / `panic:` / `WARNING: DATA RACE` lines in
//     the elided middle of an oversized verify output are kept, so the fix
//     prompt can no longer drop the failing test name.
//
// The verify output is UNTRUSTED (the diff under test influences it). A
// fabricated FAIL line can only relabel a red outcome from category A to C, or
// annotate a pass; it can never turn a red tree green, because a pass still
// requires the real full-form verify to exit 0 on that tree (#960).

// The trace-event kinds and reason/detail leads, as named constants so the
// loop, the renderers and the tests cannot drift.
const (
	// verifyFixNoopEvent is emitted for EVERY detected no-op fix iteration:
	// the fix agent returned and the next iteration's scope-only tree is
	// byte-identical to the last FAILED one.
	verifyFixNoopEvent = "verify_fix_noop"

	// verifyFlakeRerunEvent records the outcome of the once-per-stage flake
	// re-run (passed, or a reproduced outside-change failure).
	verifyFlakeRerunEvent = "verify_flake_rerun"

	// verifyFlakeRerunLead leads the verify_summary detail of a PASSED flake
	// re-run, so the review does not read it as a fix.
	verifyFlakeRerunLead = "verify_flake_rerun:"

	// verifyFailureOutsideChangeLead leads the category-C FailureReason of a
	// reproduced outside-change failure on an identical tree.
	verifyFailureOutsideChangeLead = "verify_failure_outside_change:"
)

// The three verifyFailureScopeRelation results.
const (
	verifyRelationOutside = "outside"
	verifyRelationInside  = "inside"
	verifyRelationUnknown = "unknown"
)

const (
	// verifyFailureParseMaxEntries caps each of parseVerifyFailures' lists.
	verifyFailureParseMaxEntries = 10
	// verifyFailureParseMaxEntryBytes caps each parsed entry.
	verifyFailureParseMaxEntryBytes = 200
	// verifyFixSignalLineMaxBytes caps one retained signal/context line.
	verifyFixSignalLineMaxBytes = 1 << 10
	// verifyFixSignalContextBefore / After are the context lines kept around
	// each retained signal line.
	verifyFixSignalContextBefore = 2
	verifyFixSignalContextAfter  = 4
	// verifyFixSignalSeparator separates two disjoint retained windows.
	verifyFixSignalSeparator = "...\n"
)

var (
	signalTestFailRe = regexp.MustCompile(`^\s*--- FAIL: `)
	signalPkgFailRe  = regexp.MustCompile(`^FAIL(\t|\s|$)`)
	parseTestFailRe  = regexp.MustCompile(`^\s*--- FAIL: (\S+)`)
	parsePkgFailRe   = regexp.MustCompile(`^FAIL\t(\S+)(.*)$`)
)

// isFailureSignalLine reports whether one output line (no trailing newline)
// is a go-test failure signal: a `--- FAIL: ` test line (subtests indented),
// a `FAIL` package line (`FAIL\t<pkg>\t0.1s`, `FAIL\t<pkg> [build failed]`, a
// bare `FAIL`), a `panic: ` line or a `WARNING: DATA RACE` banner.
func isFailureSignalLine(line string) bool {
	line = strings.TrimSuffix(line, "\r")
	return signalTestFailRe.MatchString(line) ||
		signalPkgFailRe.MatchString(line) ||
		strings.HasPrefix(line, "panic: ") ||
		strings.Contains(line, "WARNING: DATA RACE")
}

// verifyFailures is what parseVerifyFailures reads out of a verify output.
type verifyFailures struct {
	// tests are the `--- FAIL: <name>` names (subtests included).
	tests []string
	// pkgs are the `FAIL\t<import path>` package import paths.
	pkgs []string
	// buildFailed are the pkgs whose FAIL line carried `[build failed]` or
	// `[setup failed]` — a deterministic artifact defect, never a flake.
	buildFailed []string
	// pkgsLossy is set when pkgs is NOT the complete, exact set of failing
	// packages: a distinct package was dropped past the entry cap, or an
	// import path was cut at the byte cap (which can remove the suffix that
	// names a scope dir). verifyFailureScopeRelation never answers outside
	// from a lossy list.
	pkgsLossy bool
	// lossy is set when ANY of the three lists lost information (an entry
	// dropped past the entry cap or cut at the byte cap) — a superset of
	// pkgsLossy. Only the merge-candidate failure summary reads it (#4250);
	// verifyFailureScopeRelation keeps reading pkgsLossy alone.
	lossy bool
}

// parseVerifyFailures extracts the failing test names and package import
// paths from a go-test verify output. Each list is deduplicated in order of
// appearance, capped at verifyFailureParseMaxEntries entries, and each entry
// is capped at verifyFailureParseMaxEntryBytes. A package that failed to build
// or set up is recorded in buildFailed as well as pkgs. Either cap firing on a
// package sets pkgsLossy.
func parseVerifyFailures(out string) verifyFailures {
	var f verifyFailures
	seenT := map[string]bool{}
	seenP := map[string]bool{}
	seenB := map[string]bool{}
	// add reports whether v lost information: cut at the byte cap, or a new
	// entry dropped because the list is full.
	add := func(list *[]string, seen map[string]bool, v string) (lossy bool) {
		if len(v) > verifyFailureParseMaxEntryBytes {
			v = v[:verifyFailureParseMaxEntryBytes]
			lossy = true
		}
		if seen[v] {
			return lossy
		}
		if len(*list) >= verifyFailureParseMaxEntries {
			return true
		}
		seen[v] = true
		*list = append(*list, v)
		return lossy
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if m := parseTestFailRe.FindStringSubmatch(line); m != nil {
			if add(&f.tests, seenT, m[1]) {
				f.lossy = true
			}
			continue
		}
		if m := parsePkgFailRe.FindStringSubmatch(line); m != nil {
			if add(&f.pkgs, seenP, m[1]) {
				f.pkgsLossy = true
				f.lossy = true
			}
			if strings.Contains(m[2], "[build failed]") || strings.Contains(m[2], "[setup failed]") {
				if add(&f.buildFailed, seenB, m[1]) {
					f.lossy = true
				}
			}
		}
	}
	return f
}

// verifyFailureScopeRelation classifies where a verify failure lies relative
// to the change's scoped package dirs (verifyScopePackages):
//
//   - inside when ANY package failed to build or set up (#4036 approval
//     condition 1): a compile break this change caused in a dependent package
//     is a deterministic artifact defect, never a flake — today's category-A
//     path;
//   - unknown when no failing package was parsed, the scope set is empty, or
//     it contains the module-root "." (no suffix can name it);
//   - inside when ANY failing import path equals a scope dir or ends with
//     "/"+dir — a suffix collision errs toward inside, today's behaviour and
//     the fail-safe direction;
//   - unknown when no parsed package is inside but the list is lossy
//     (pkgsLossy): a package dropped past the entry cap, or a path cut at the
//     byte cap, may be the in-scope one, so "every package is outside" cannot
//     be decided;
//   - outside otherwise: EVERY failing package is outside the change.
func verifyFailureScopeRelation(f verifyFailures, scopePkgs []string) string {
	if len(f.buildFailed) > 0 {
		return verifyRelationInside
	}
	if len(f.pkgs) == 0 || len(scopePkgs) == 0 {
		return verifyRelationUnknown
	}
	for _, dir := range scopePkgs {
		if dir == "." {
			return verifyRelationUnknown
		}
	}
	for _, pkg := range f.pkgs {
		for _, dir := range scopePkgs {
			if pkg == dir || strings.HasSuffix(pkg, "/"+dir) {
				return verifyRelationInside
			}
		}
	}
	if f.pkgsLossy {
		return verifyRelationUnknown
	}
	return verifyRelationOutside
}

// isNoopFixIteration reports whether this iteration re-verifies a tree the fix
// agent left unchanged: true only when both tree ids are known and equal AND a
// successful fix re-invoke ran since the last verify. The fixed conjunct is
// what separates a no-op FIX from an absorb / lock-contention re-run, which
// repeats the verify on an identical tree WITHOUT invoking the agent.
func isNoopFixIteration(prevFailedTree, curTree string, fixedSinceVerify bool) bool {
	return fixedSinceVerify && prevFailedTree != "" && curTree != "" && prevFailedTree == curTree
}

// capSignalLine caps one retained line at verifyFixSignalLineMaxBytes with an
// explicit truncation suffix, returning the rendered line and the number of
// source bytes it reproduces.
func capSignalLine(line string) (rendered string, kept int) {
	if len(line) <= verifyFixSignalLineMaxBytes {
		return line, len(line)
	}
	return fmt.Sprintf("%s [... line truncated, %d more bytes]", line[:verifyFixSignalLineMaxBytes], len(line)-verifyFixSignalLineMaxBytes), verifyFixSignalLineMaxBytes
}

// retainFailureSignalLines selects, from the region boundVerifyFixOutput would
// elide, every failure-signal line plus a little context, within budget bytes.
//
// Pass 1 reserves EVERY signal line, in order, until the budget is spent; the
// rest are counted as omitted. Each reservation also pays for one worst-case
// window separator, so the rendered block can never exceed budget. Pass 2
// spends what remains on context — up to verifyFixSignalContextBefore lines
// before and verifyFixSignalContextAfter after each kept signal line, whole
// lines with the same per-line cap. Overlapping windows merge; disjoint ones
// are separated by a `...` line.
//
// It returns the block (empty when the region carries no signal line, or none
// fits), the number of region source bytes the block reproduces (so the caller
// can report an exact elided count), the number of signal lines kept, and the
// number omitted over budget.
func retainFailureSignalLines(region string, budget int) (block string, retainedSrcBytes, kept, omitted int) {
	return retainSignalLines(region, budget, isFailureSignalLine)
}

// retainSignalLines is retainFailureSignalLines over an arbitrary signal
// predicate. retainFailureSignalLines passes isFailureSignalLine, so the
// verify-fix prompt's #4036 selection is byte-identical; failureSignalExcerpt
// passes the wider isMergeCandidateSignalLine.
func retainSignalLines(region string, budget int, isSignal func(string) bool) (block string, retainedSrcBytes, kept, omitted int) {
	lines := strings.SplitAfter(region, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	type pick struct {
		rendered string
		srcBytes int
	}
	picked := map[int]pick{}
	render := func(raw string) pick {
		body := strings.TrimSuffix(raw, "\n")
		r, k := capSignalLine(body)
		if len(body) < len(raw) {
			k++ // the newline the rendered line reproduces
		}
		return pick{rendered: r + "\n", srcBytes: k}
	}

	used := 0
	var signals []int
	full := false
	for i, raw := range lines {
		if !isSignal(strings.TrimSuffix(raw, "\n")) {
			continue
		}
		p := render(raw)
		if full || used+len(p.rendered)+len(verifyFixSignalSeparator) > budget {
			full = true
			omitted++
			continue
		}
		used += len(p.rendered) + len(verifyFixSignalSeparator)
		picked[i] = p
		signals = append(signals, i)
	}
	if len(signals) == 0 {
		return "", 0, 0, omitted
	}

	addContext := func(j int) bool {
		if j < 0 || j >= len(lines) {
			return true
		}
		if _, ok := picked[j]; ok {
			return true
		}
		p := render(lines[j])
		if used+len(p.rendered) > budget {
			return false
		}
		used += len(p.rendered)
		picked[j] = p
		return true
	}
context:
	for _, i := range signals {
		for j := i - verifyFixSignalContextBefore; j <= i+verifyFixSignalContextAfter; j++ {
			if !addContext(j) {
				break context
			}
		}
	}

	idx := make([]int, 0, len(picked))
	for i := range picked {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	var b strings.Builder
	for n, i := range idx {
		if n > 0 && i > idx[n-1]+1 {
			b.WriteString(verifyFixSignalSeparator)
		}
		b.WriteString(picked[i].rendered)
		retainedSrcBytes += picked[i].srcBytes
	}
	return b.String(), retainedSrcBytes, len(signals), omitted
}

// compileErrorLineRe matches a Go compiler / vet diagnostic at column 0:
// `<path>.go:<line>:<col>: <message>`. A `t.Log`/`t.Error` assertion line is
// indented and carries no column (`    x_test.go:42: msg`), so it never
// matches and stays CONTEXT.
var compileErrorLineRe = regexp.MustCompile(`^\S+\.go:\d+:\d+: `)

// isMergeCandidateSignalLine is the merge-candidate excerpt's signal set: the
// shared go-test set (isFailureSignalLine) plus a column-0 compiler / vet
// diagnostic. go test prints a `[build failed]` package's compiler errors
// where the BUILD finished, which under a parallel `go test ./...` can be far
// above that package's `FAIL<TAB><pkg> [build failed]` line, outside its
// 2-line context window (#4250 approval condition C1). The wider set is used
// ONLY by failureSignalExcerpt; the verify-fix prompt keeps the #4036 set. A
// golangci-lint diagnostic shares the format and is retained too — on a
// failed merge-candidate pass it names a real failure.
func isMergeCandidateSignalLine(line string) bool {
	return isFailureSignalLine(line) || compileErrorLineRe.MatchString(strings.TrimSuffix(line, "\r"))
}

// failureSignalExcerptMarker renders the trusted-shape line that closes an
// excerpt whose signal lines did not all fit.
func failureSignalExcerptMarker(omitted, budget int) string {
	return fmt.Sprintf("[... %d more failure-signal line(s) omitted over the %d-byte budget ...]\n", omitted, budget)
}

// failureSignalExcerpt extracts, from the WHOLE (already-redacted) verify
// output, every failure-signal line plus its context, within budget bytes
// (#4250). The signal set is the shared go-test set — `--- FAIL:`,
// `FAIL<TAB><pkg>`, `panic:`, `WARNING: DATA RACE` — widened by a column-0
// compiler/vet diagnostic (isMergeCandidateSignalLine), each with
// verifyFixSignalContextBefore (2) lines of context before and
// verifyFixSignalContextAfter (4) after, which captures the
// `<file>_test.go:NN:` assertion lines go test prints immediately after a
// non-verbose `--- FAIL:` line. Selection is retainSignalLines (#4036):
// signals in order until the budget is spent, then context.
//
// When signal lines did not all fit, the excerpt ends with a marker naming how
// many were omitted. The marker's worst case (every signal line omitted) is
// RESERVED out of the budget before selection, and retainSignalLines' block
// never exceeds what it was given (it reserves one worst-case separator per
// kept signal line), so the result is always <= budget; it fills the budget
// exactly only when every reserved separator is rendered. The omitted count
// is an upper bound: an omitted signal line that falls inside a kept line's
// context window is still shown, as context.
//
// It returns "" when the output carries no signal line.
func failureSignalExcerpt(out string, budget int) string {
	signals := 0
	for _, line := range strings.Split(out, "\n") {
		if isMergeCandidateSignalLine(line) {
			signals++
		}
	}
	room := budget - len(failureSignalExcerptMarker(signals, budget))
	if room <= 0 {
		return ""
	}
	block, _, _, omitted := retainSignalLines(out, room, isMergeCandidateSignalLine)
	if omitted == 0 {
		return block
	}
	return block + failureSignalExcerptMarker(omitted, budget)
}

// renderFailureList renders parsed tests and packages as `A, B (p1, p2)` for a
// reason/detail line; "(none parsed)" when both are empty.
func renderFailureList(f verifyFailures) string {
	tests := strings.Join(f.tests, ", ")
	if tests == "" {
		tests = "(none parsed)"
	}
	if len(f.pkgs) == 0 {
		return tests
	}
	return tests + " (" + strings.Join(f.pkgs, ", ") + ")"
}

// verifyFlakeRerunDetail is the verify_summary detail of a PASSED flake
// re-run: it keeps outcome=passed but states the earlier failure did not
// reproduce, naming the flaky test(s), so it no longer reads as a fix.
func verifyFlakeRerunDetail(f verifyFailures) string {
	return verifyFlakeRerunLead + " the fix agent made no change and the identical tree passed on re-run; the earlier failure did not reproduce. Flaky test(s) outside this change's packages: " + renderFailureList(f)
}

// verifyFailureOutsideChangeReason is the category-C lead of a reproduced
// outside-change failure on an identical tree.
func verifyFailureOutsideChangeReason(verifyCmd string, f verifyFailures) string {
	return fmt.Sprintf("%s verify command %q failed twice on an identical tree; the fix agent made no change and every failing package is outside this change's packages. Failing test(s): %s. Likely a pre-existing or flaky failure, not this change: retry the stage, or fix the named test on the base branch; if this change did cause it, route a fix-up.",
		verifyFailureOutsideChangeLead, verifyCmd, renderFailureList(f))
}

// logVerifyFixNoop records one detected no-op fix iteration (#4036) as a log
// line and returns the matching trace event.
func logVerifyFixNoop(logSink io.Writer, cfg config, iteration int, tree, relation string, f verifyFailures, admitted bool) agent.Event {
	_, _ = fmt.Fprintf(logSink,
		`{"event":%q,"run_id":%q,"stage_id":%q,"iteration":%d,"tree_sha":%q,"relation":%q,"failing_tests":%q,"failing_packages":%q,"admitted":%t}`+"\n",
		verifyFixNoopEvent, cfg.runID, cfg.stageID, iteration, tree, relation, strings.Join(f.tests, ","), strings.Join(f.pkgs, ","), admitted)
	return agent.Event{
		Kind: verifyFixNoopEvent,
		Payload: agent.MakePayload(map[string]any{
			"iteration":        iteration,
			"tree_sha":         tree,
			"relation":         relation,
			"failing_tests":    nonNilStrings(f.tests),
			"failing_packages": nonNilStrings(f.pkgs),
			"admitted":         admitted,
		}),
	}
}

// logVerifyFlakeRerun records how the flake re-run settled (#4036) — passed,
// or failed again entirely outside the change — as a log line and returns the
// matching trace event.
func logVerifyFlakeRerun(logSink io.Writer, cfg config, iteration int, outcome string, f verifyFailures) agent.Event {
	_, _ = fmt.Fprintf(logSink,
		`{"event":%q,"run_id":%q,"stage_id":%q,"iteration":%d,"outcome":%q,"failing_tests":%q,"failing_packages":%q}`+"\n",
		verifyFlakeRerunEvent, cfg.runID, cfg.stageID, iteration, outcome, strings.Join(f.tests, ","), strings.Join(f.pkgs, ","))
	return agent.Event{
		Kind: verifyFlakeRerunEvent,
		Payload: agent.MakePayload(map[string]any{
			"iteration":        iteration,
			"outcome":          outcome,
			"failing_tests":    nonNilStrings(f.tests),
			"failing_packages": nonNilStrings(f.pkgs),
		}),
	}
}

// nonNilStrings renders a nil list as an empty JSON array rather than null.
func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
