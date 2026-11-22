package main

import (
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// Slice gate: the classification core (#5498).
//
// A decomposition child's non-fix-up implement gate can run ONE scoped verify
// over the slice's reverse-import reach instead of the #3315 scoped pre-pass
// plus the mandatory full re-verify. The parent's fan-in merge-candidate pass
// stays the single full verify and the merge precondition (ADR-090 D3/D5), so
// the slice gate is a deliberately WEAKER consumer of the repository's
// declared verdict_transfer surface (ADR-124, ADR-110): it reuses ADR-124 D3's
// class order but departs from D3's inert rule and has no ADR-098 D2 data-read
// edges. Contract: runner/README.md § "Slice gate".
//
// This file is pure: phase 1 (planSliceGate) decides eligibility from the
// served config and the scope; phase 2 (sliceGateResolveReach) maps the scope
// onto a primitive import graph the reach loader builds. Every rule that cannot
// decide faithfully WIDENS to the full form; none narrows. Every package-level
// identifier here starts with `sliceGate` (the parallel-slice collision rule).

const (
	// sliceGateShapeScoped is the gate shape of a verify restricted to the
	// slice's reverse-import reach.
	sliceGateShapeScoped = "slice_scoped"
	// sliceGateShapeFull is the gate shape of an unrestricted verify.
	sliceGateShapeFull = "full"

	// sliceGateEventKind is the trace event appended once per hooked gate
	// decision.
	sliceGateEventKind = "verify_slice_gate"

	// Phase-1 reasons.
	sliceGateReasonNotOffered    = "slice_gate_not_offered"
	sliceGateReasonEmptyScope    = "empty_scope"
	sliceGateReasonUndecodable   = "undecodable_path"
	sliceGateReasonNonConcrete   = "non_concrete_scope_entry"
	sliceGateReasonGoModuleFile  = "go_module_file"
	sliceGateReasonGlobalSurface = "global_surface"

	// Phase-2 mapping reasons.
	sliceGateReasonUnmappedGoPath  = "unmapped_go_path"
	sliceGateReasonUnclassified    = "unclassified_path"
	sliceGateReasonNoReachPackages = "no_reach_packages"
	sliceGateReasonReachEverything = "reach_is_everything"
	sliceGateReasonReachTooLarge   = "reach_too_large"

	// sliceGateEventMaxPackages caps the package dirs one event carries.
	sliceGateEventMaxPackages = 50
	// sliceGateReasonMaxBytes bounds every reason the gate records.
	sliceGateReasonMaxBytes         = 256
	sliceGateNonConcreteMetaChars   = "*?[]{}"
	sliceGateUndecodablePathChars   = ",\t\n\r\x00\\"
	sliceGateUndecodablePackageRune = ",\t\n\r"
)

// sliceGateMaxPackagesBytes caps the comma-joined package value handed to
// FISHHAWK_VERIFY_PACKAGES. Linux caps one env string at MAX_ARG_STRLEN
// (128 KiB); the gate widens to full well below that. A var so a test can
// lower it.
var sliceGateMaxPackagesBytes = 64 << 10

// sliceGateDecision is one gate decision. Shape is sliceGateShapeScoped or
// sliceGateShapeFull. A full decision carries the widening Reason; a scoped
// one carries the reach Packages (repo-relative dirs) and an empty Reason.
// Phase 1 returns an ELIGIBLE decision (Shape scoped, no Reason, no Packages)
// when phase 2 should run.
type sliceGateDecision struct {
	Shape    string
	Reason   string
	Packages []string
}

// eligible reports whether a phase-1 decision admits phase 2.
func (d sliceGateDecision) eligible() bool {
	return d.Shape == sliceGateShapeScoped && d.Reason == ""
}

// sliceGateFull builds a full decision with a bounded reason.
func sliceGateFull(reason string) sliceGateDecision {
	return sliceGateDecision{Shape: sliceGateShapeFull, Reason: sliceGateBoundReason(reason)}
}

// sliceGateBoundReason truncates reason to sliceGateReasonMaxBytes on a UTF-8
// boundary, so a long scope path cannot grow an event or log line unbounded.
func sliceGateBoundReason(reason string) string {
	if len(reason) <= sliceGateReasonMaxBytes {
		return reason
	}
	cut := reason[:sliceGateReasonMaxBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// sliceGateApplies reports whether the slice gate applies to a stage at all:
// only a decomposition child's NON-fix-up implement stage. A standalone run,
// a child fix-up and every other stage type keep today's gate byte-for-byte
// and emit no verify_slice_gate event.
func sliceGateApplies(stageType, decomposedFromRunID string, fixup bool) bool {
	return stageType == "implement" && decomposedFromRunID != "" && !fixup
}

// planSliceGate is phase 1: it decides from the served config and the scope
// whether the reach mapping may run. Rules apply in order, each over the whole
// scope, and the first that matches decides:
//
//  1. not offered (nil config)            -> full/slice_gate_not_offered
//  2. an unavailable reason               -> full/<reason>
//  3. an empty scope (git add -A fallback) -> full/empty_scope
//  4. an undecodable scope path           -> full/undecodable_path
//  5. a non-concrete entry (dir prefix, glob) -> full/non_concrete_scope_entry:<entry>
//  6. go.work, go.work.sum, go.mod, go.sum at any depth -> full/go_module_file:<p>
//  7. a doublestar match on fullGatePaths -> full/global_surface:<p>
//
// Anything else is eligible.
func planSliceGate(offered bool, unavailableReason string, fullGatePaths, scopePaths []string) sliceGateDecision {
	if !offered {
		return sliceGateFull(sliceGateReasonNotOffered)
	}
	if unavailableReason != "" {
		return sliceGateFull(unavailableReason)
	}
	if len(scopePaths) == 0 {
		return sliceGateFull(sliceGateReasonEmptyScope)
	}
	for _, p := range scopePaths {
		if sliceGateUndecodablePath(p) {
			return sliceGateFull(sliceGateReasonUndecodable)
		}
	}
	for _, p := range scopePaths {
		if sliceGateNonConcrete(p) {
			return sliceGateFull(sliceGateReasonNonConcrete + ":" + p)
		}
	}
	for _, p := range scopePaths {
		if sliceGateGoModuleFile(p) {
			return sliceGateFull(sliceGateReasonGoModuleFile + ":" + p)
		}
	}
	for _, p := range scopePaths {
		if sliceGateMatchesAny(fullGatePaths, p) {
			return sliceGateFull(sliceGateReasonGlobalSurface + ":" + p)
		}
	}
	return sliceGateDecision{Shape: sliceGateShapeScoped}
}

// sliceGateUndecodablePath reports a scope path the mapping cannot present
// faithfully: empty, carrying a comma, tab, newline, CR, NUL or backslash,
// absolute, or not in clean form (a `..` or `.` segment, a doubled slash).
// A trailing slash is left to the non-concrete rule. A non-clean path would
// otherwise miss a fullGatePaths match (`./scripts/test` against `scripts/**`)
// and narrow the gate.
func sliceGateUndecodablePath(p string) bool {
	if p == "" || strings.ContainsAny(p, sliceGateUndecodablePathChars) || strings.HasPrefix(p, "/") {
		return true
	}
	trimmed := strings.TrimSuffix(p, "/")
	if trimmed == "" || trimmed == "." {
		return true
	}
	for _, seg := range strings.Split(trimmed, "/") {
		if seg == ".." {
			return true
		}
	}
	return path.Clean(trimmed) != trimmed
}

// sliceGateNonConcrete reports a scope entry that names more than one file: a
// trailing-slash directory prefix (the folded-directory form gitops treats as
// a directory) or any glob metacharacter. The reach mapping seeds only
// concrete paths, so such an entry could otherwise narrow the gate silently.
func sliceGateNonConcrete(p string) bool {
	return strings.HasSuffix(p, "/") || strings.ContainsAny(p, sliceGateNonConcreteMetaChars)
}

// sliceGateGoModuleFile reports a Go module or workspace metadata file at any
// depth: a change there can rewire the build graph the reach is computed on.
func sliceGateGoModuleFile(p string) bool {
	switch path.Base(p) {
	case "go.work", "go.work.sum", "go.mod", "go.sum":
		return true
	}
	return false
}

// sliceGateMatchesAny reports whether p matches any declared full-gate
// pattern, root-anchored with the same doublestar.Match semantics as
// backend/internal/basedelta matchAny (pinned by the shared golden cases
// table, testdata/wire/slice_gate_prompt.json). A malformed pattern never
// matches, as there; the server only serves a surface whose Refusal is empty.
func sliceGateMatchesAny(patterns []string, p string) bool {
	for _, pat := range patterns {
		if ok, err := doublestar.Match(pat, p); err == nil && ok {
			return true
		}
	}
	return false
}

// sliceGateResolveReach is phase 2: it maps the scope onto a primitive import
// graph and returns the scoped package dirs, or nil with a widening reason.
//
// Keys are repo-relative dirs ("." for the root). imports holds every node,
// traversal-only ones included, mapped to the nodes it imports; emittable
// holds the dirs the go command reported (the only dirs a `./<dir>` pattern
// can test-run).
//
//   - A `.go` path seeds its own dir when that dir is a node; otherwise the
//     result is full/unmapped_go_path:<p>.
//   - Any other path seeds every ancestor dir that is a node, own dir first.
//     With none, a path matching inertPaths contributes nothing (the deliberate
//     ADR-124 D3 departure); otherwise the result is full/unclassified_path:<p>.
//
// The reach is the reverse-import fixed point of the seeds, restricted to
// emittable and sorted. Guards: an empty result (no_reach_packages), a result
// covering every emittable dir (reach_is_everything), a joined value over
// sliceGateMaxPackagesBytes (reach_too_large) and a dir carrying a comma, tab,
// newline or CR (undecodable_path) all widen to full.
func sliceGateResolveReach(imports map[string][]string, emittable map[string]bool, scopePaths, inertPaths []string) ([]string, string) {
	isNode := func(dir string) bool {
		if _, ok := imports[dir]; ok {
			return true
		}
		return emittable[dir]
	}

	ordered := append([]string(nil), scopePaths...)
	sort.Strings(ordered)

	seeds := map[string]bool{}
	for _, p := range ordered {
		dir := path.Dir(p)
		if strings.HasSuffix(p, ".go") {
			if !isNode(dir) {
				return nil, sliceGateBoundReason(sliceGateReasonUnmappedGoPath + ":" + p)
			}
			seeds[dir] = true
			continue
		}
		seeded := false
		for {
			if isNode(dir) {
				seeds[dir] = true
				seeded = true
			}
			if dir == "." {
				break
			}
			dir = path.Dir(dir)
		}
		if seeded || sliceGateMatchesAny(inertPaths, p) {
			continue
		}
		return nil, sliceGateBoundReason(sliceGateReasonUnclassified + ":" + p)
	}

	closure := sliceGateReverseClosure(imports, seeds)

	var pkgs []string
	for dir := range closure {
		if emittable[dir] {
			pkgs = append(pkgs, dir)
		}
	}
	sort.Strings(pkgs)

	if len(pkgs) == 0 {
		return nil, sliceGateReasonNoReachPackages
	}
	total := 0
	for _, ok := range emittable {
		if ok {
			total++
		}
	}
	if len(pkgs) == total {
		return nil, sliceGateReasonReachEverything
	}
	if len(strings.Join(pkgs, verifyPackagesSeparator)) > sliceGateMaxPackagesBytes {
		return nil, sliceGateReasonReachTooLarge
	}
	for _, dir := range pkgs {
		if strings.ContainsAny(dir, sliceGateUndecodablePackageRune) {
			return nil, sliceGateReasonUndecodable
		}
	}
	return pkgs, ""
}

// sliceGateReverseClosure returns every node with an import path into a seed,
// seeds included: the reverse-import fixed point.
func sliceGateReverseClosure(imports map[string][]string, seeds map[string]bool) map[string]bool {
	importers := map[string][]string{}
	for from, tos := range imports {
		for _, to := range tos {
			importers[to] = append(importers[to], from)
		}
	}
	closure := map[string]bool{}
	queue := make([]string, 0, len(seeds))
	for s := range seeds {
		closure[s] = true
		queue = append(queue, s)
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, imp := range importers[n] {
			if !closure[imp] {
				closure[imp] = true
				queue = append(queue, imp)
			}
		}
	}
	return closure
}

// sliceGateEvent builds the verify_slice_gate trace event for one gate
// decision: the shape, a bounded reason, at most sliceGateEventMaxPackages
// package dirs plus the full count, and the loop iteration (0 on the
// single-shot gate).
func sliceGateEvent(d sliceGateDecision, iteration int) agent.Event {
	pkgs := d.Packages
	truncated := false
	if len(pkgs) > sliceGateEventMaxPackages {
		pkgs = pkgs[:sliceGateEventMaxPackages]
		truncated = true
	}
	payload := map[string]any{
		"shape":              d.Shape,
		"reason":             sliceGateBoundReason(d.Reason),
		"packages_count":     len(d.Packages),
		"packages_truncated": truncated,
		"iteration":          iteration,
	}
	if len(pkgs) > 0 {
		payload["packages"] = append([]string(nil), pkgs...)
	}
	return agent.Event{
		Kind:    sliceGateEventKind,
		Payload: agent.MakePayload(payload),
	}
}
