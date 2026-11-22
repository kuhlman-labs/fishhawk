package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gateiso"
)

// Slice gate: the verify-gate hook (#5498).
//
// slicegate.go decides eligibility (phase 1) and maps a scope onto an import
// graph (phase 2); slicereach.go loads that graph from a materialized clone.
// This file composes the two at the ONE point where a clone exists: inside
// runVerifyCommittedTreeEnv, after materializeGateCheckoutFn and before
// phases.startVerify(), so the load is neither charged to nor clipped by the
// #4023 gate budget (its bound is sliceReachLoadTimeout).
//
// The hook rides a CALL-LOCAL ctx value (withSliceGateReach). Only the
// deciding verify in runVerifyFixLoop's eligible arm and the single-shot
// runVerifyGateCommitted build one; it is never assigned back to the stage
// ctx, cfg or a loop variable. Every other caller of runVerifyCommittedTree —
// the #960 pre-push strict re-verify and its re-runs, the #4229 absorb re-run,
// every child fix-up, the merge-candidate pass, diff coverage and
// counterfactuals — sees no hook, keeps its scope byte-for-byte and emits no
// verify_slice_gate event. Contract: runner/cmd/fishhawk-runner/README.md
// § "Committed-tree verify-fix loop (#651)" and runner/README.md § slice gate.

const (
	// sliceGateReasonIsolationNotHost widens a gate whose isolation selection
	// is not a host path (ADR-063): the host reach load never runs there.
	sliceGateReasonIsolationNotHost = "gate_isolation_not_host"
	// sliceGateIsolationTokenMax bounds the selection token in that reason.
	sliceGateIsolationTokenMax = 64
	// sliceGateScopedDetail is the verify_summary detail sentence a
	// slice-scoped pass carries into the implement review (gate_evidence's
	// verify_summary digest reads only outcome/iterations/max_iterations/
	// detail, so this sentence is the review's only view of the shape).
	sliceGateScopedDetail = "slice gate: the deciding verify ran SCOPED to the slice's reverse-import reach; patch coverage and the full test loop run in the parent's fan-in merge-candidate pass"
)

// sliceGateLoadFn is the reach loader seam. Production is sliceReachLoad; a
// test wraps it to count loads or stubs a graph.
var sliceGateLoadFn = sliceReachLoad

// sliceGateSelectionFn reads the gate-isolation selection the same memoized
// way runBoundedGateArgvDisposed does (gateIsolation.selection). It is the
// non-exec read runVerifyCommittedTreeEnv's lock-path decision already makes,
// so it records no gate evidence. A test substitutes a synthetic selection.
var sliceGateSelectionFn = func(ctx context.Context) gateiso.Selection {
	return gateIsolation.selection(ctx)
}

// sliceGateIsolationRefusal returns "" when the reach load may run on sel: a
// host path (gateiso.PathClone, which includes the nil-state
// hostExecSelection, or gateiso.PathCloneSandbox) under a non-hosted profile.
// Anything else widens with gate_isolation_not_host:<selection>: a hosted
// profile first whatever the path, then the path (container, refused or any
// other value, bounded, `unknown` when empty). An allowlist, so a new
// non-host path fails closed.
func sliceGateIsolationRefusal(sel gateiso.Selection) string {
	if sel.Profile == gateiso.ProfileHosted {
		return sliceGateReasonIsolationNotHost + ":" + string(gateiso.ProfileHosted)
	}
	switch sel.Path {
	case gateiso.PathClone, gateiso.PathCloneSandbox:
		return ""
	}
	tok := string(sel.Path)
	if tok == "" {
		tok = "unknown"
	}
	if len(tok) > sliceGateIsolationTokenMax {
		tok = tok[:sliceGateIsolationTokenMax]
	}
	return sliceGateReasonIsolationNotHost + ":" + tok
}

// sliceGateReach is one gate's reach state: the scope it maps, the served
// inert patterns, a one-entry (tree SHA, scope digest) cache and the last
// decision. One value lives per runVerifyFixLoop and per runVerifyGateCommitted
// call. Safe for the sequential use the gates make; the mutex only guards a
// concurrent read of the counters by a test.
type sliceGateReach struct {
	mu    sync.Mutex
	scope []string
	inert []string

	cacheTree   string
	cacheScope  string
	cached      bool
	cachePkgs   []string
	cacheReason string
	carryNext   bool

	last    sliceGateDecision
	decided bool // a decision was recorded since the last take()
	loads   int  // sliceGateLoadFn calls
}

// newSliceGateReach builds the reach state for one gate.
func newSliceGateReach(scope, inert []string) *sliceGateReach {
	return &sliceGateReach{scope: append([]string(nil), scope...), inert: append([]string(nil), inert...)}
}

type sliceGateReachKey struct{}

// withSliceGateReach returns a CALL-LOCAL ctx carrying sg; never store it.
func withSliceGateReach(ctx context.Context, sg *sliceGateReach) context.Context {
	return context.WithValue(ctx, sliceGateReachKey{}, sg)
}

// sliceGateReachFrom returns the hook ctx's reach state, or nil.
func sliceGateReachFrom(ctx context.Context) *sliceGateReach {
	sg, _ := ctx.Value(sliceGateReachKey{}).(*sliceGateReach)
	return sg
}

// setScope replaces the mapped scope (a mid-loop re-fold can grow it). The
// cache key carries a scope digest, so a changed scope never reuses a result.
func (sg *sliceGateReach) setScope(scope []string) {
	sg.mu.Lock()
	defer sg.mu.Unlock()
	sg.scope = append([]string(nil), scope...)
}

// carry lets the NEXT resolve reuse the cached result once for a different
// tree: the auto-format absorb (#3316) rewrites only in-scope .go files, each
// of whose dirs is a seed, so the reverse-import closure cannot change.
func (sg *sliceGateReach) carry() {
	if sg == nil {
		return
	}
	sg.mu.Lock()
	defer sg.mu.Unlock()
	sg.carryNext = sg.cached
}

// loadCount reports how many reach loads ran.
func (sg *sliceGateReach) loadCount() int {
	sg.mu.Lock()
	defer sg.mu.Unlock()
	return sg.loads
}

// take returns the decision recorded since the last take, if any, and clears
// the flag. A nil receiver has none.
func (sg *sliceGateReach) take() (sliceGateDecision, bool) {
	if sg == nil {
		return sliceGateDecision{}, false
	}
	sg.mu.Lock()
	defer sg.mu.Unlock()
	d, ok := sg.last, sg.decided
	sg.decided = false
	return d, ok
}

func (sg *sliceGateReach) record(d sliceGateDecision) {
	sg.last = d
	sg.decided = true
}

// sliceGateScopeDigest keys the cache on the exact scope.
func sliceGateScopeDigest(scope []string) string {
	h := sha256.Sum256([]byte(strings.Join(scope, "\n")))
	return hex.EncodeToString(h[:])
}

// resolve decides the deciding verify's package set on the materialized clone
// wt of committed tree treeSHA. It returns the scoped reach dirs, or nil (the
// FULL form) on any refusal. Order: the isolation check (no load off a host
// path), then the per-tree cache, then ONE load plus the mapping. An empty
// treeSHA never caches.
func (sg *sliceGateReach) resolve(ctx context.Context, wt, treeSHA string) []string {
	sg.mu.Lock()
	defer sg.mu.Unlock()

	if refusal := sliceGateIsolationRefusal(sliceGateSelectionFn(ctx)); refusal != "" {
		sg.record(sliceGateFull(refusal))
		return nil
	}

	digest := sliceGateScopeDigest(sg.scope)
	carried := sg.carryNext
	sg.carryNext = false
	if treeSHA != "" && sg.cached && sg.cacheScope == digest && (sg.cacheTree == treeSHA || carried) {
		sg.cacheTree = treeSHA
		return sg.decideLocked(sg.cachePkgs, sg.cacheReason)
	}

	imports, emittable, reason := sliceGateLoadFn(ctx, wt)
	sg.loads++
	var pkgs []string
	if reason == "" {
		pkgs, reason = sliceGateResolveReach(imports, emittable, sg.scope, sg.inert)
	}
	if reason != "" {
		pkgs = nil
	}
	if treeSHA != "" {
		sg.cacheTree, sg.cacheScope, sg.cached = treeSHA, digest, true
		sg.cachePkgs, sg.cacheReason = pkgs, reason
	}
	return sg.decideLocked(pkgs, reason)
}

func (sg *sliceGateReach) decideLocked(pkgs []string, reason string) []string {
	if reason != "" || len(pkgs) == 0 {
		if reason == "" {
			reason = sliceGateReasonNoReachPackages
		}
		sg.record(sliceGateFull(reason))
		return nil
	}
	out := append([]string(nil), pkgs...)
	sg.record(sliceGateDecision{Shape: sliceGateShapeScoped, Packages: out})
	return append([]string(nil), out...)
}

// sliceGateForm is the verify form a gate decision ran: scoped when the hook
// returned packages, full otherwise.
func sliceGateForm(d sliceGateDecision) string {
	if d.Shape == sliceGateShapeScoped && len(d.Packages) > 0 {
		return verifyFormScoped
	}
	return verifyFormFull
}

// logSliceGateDecision logs verify_slice_gate_resolved and returns the
// verify_slice_gate trace event for one gate decision.
func logSliceGateDecision(logSink io.Writer, cfg config, iteration int, d sliceGateDecision, loads int) agent.Event {
	_, _ = fmt.Fprintf(logSink,
		`{"event":"verify_slice_gate_resolved","run_id":%q,"stage_id":%q,"iteration":%d,"shape":%q,"reason":%q,"packages_count":%d,"reach_loads":%d}`+"\n",
		cfg.runID, cfg.stageID, iteration, d.Shape, sliceGateBoundReason(d.Reason), len(d.Packages), loads)
	return sliceGateEvent(d, iteration)
}

// sliceGatePlanFor is phase 1 over the served config for scope.
func sliceGatePlanFor(cfg config, scope []string) sliceGateDecision {
	if cfg.sliceGate == nil {
		return planSliceGate(false, "", nil, scope)
	}
	return planSliceGate(true, cfg.sliceGate.UnavailableReason, cfg.sliceGate.FullGatePaths, scope)
}

// sliceGateInert returns the served inert patterns, nil when not offered.
func sliceGateInert(cfg config) []string {
	if cfg.sliceGate == nil {
		return nil
	}
	return cfg.sliceGate.InertPaths
}

// sliceGateSummary stamps the trace-only verify_summary gate-shape fields and,
// on a slice-scoped pass, the scoped-shape detail sentence (set when the slot
// is empty, APPENDED otherwise).
func sliceGateSummary(summary map[string]any, d sliceGateDecision) {
	summary["gate_shape"] = d.Shape
	summary["gate_shape_reason"] = sliceGateBoundReason(d.Reason)
	summary["gate_packages_count"] = len(d.Packages)
	if summary["outcome"] != "passed" || sliceGateForm(d) != verifyFormScoped {
		return
	}
	if existing, _ := summary["detail"].(string); existing != "" {
		summary["detail"] = existing + "; " + sliceGateScopedDetail
		return
	}
	summary["detail"] = sliceGateScopedDetail
}
