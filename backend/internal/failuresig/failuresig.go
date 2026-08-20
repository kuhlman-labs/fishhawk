// Package failuresig matches a failed stage's already-held evidence against
// an ordered catalog of named failure signatures, each carrying what the
// failure MEANS and the recommended verb sequence to recover from it (#1703).
//
// It converts operator memory — a failure string or counter shape, and the
// playbook that follows from it — into a value the product can hand back
// through next_actions, so an operator with no dogfood history gets the
// recovery the product already had the evidence to produce.
//
// Three standing invariants:
//
//   - FAIL-OPEN. Match returns nil whenever nothing in the catalog fires. A
//     non-match must leave the caller byte-for-byte as it was without the
//     registry; a registry that could change behaviour on a NON-match would be
//     strictly worse than no registry.
//   - DISPLAY-ONLY. Nothing here gates a run, reorders a caller's actions, or
//     participates in an applicability decision. A hint that gates a run is a
//     bug.
//   - CONSTANT-SIZE. A Hint echoes only registry-owned strings — never the
//     caller's failure reason or any other caller-supplied text — so a matched
//     hint cannot grow with run data and cannot blow a response budget.
//
// Matching is a pure function: no I/O, no reflection, no backend round-trip.
package failuresig

import "strings"

// RegistryVersion stamps every Hint with the catalog revision that produced
// it, so an operator reading a hint knows which catalog they are looking at
// and a future breaking re-cut of the ids is distinguishable on the wire.
const RegistryVersion = "v1"

// stageStateFailed is the one stage state that describes a failure. Evidence
// carrying any OTHER non-empty state describes a live or settled-healthy
// stage, which is never a failure to diagnose.
const stageStateFailed = "failed"

// Evidence is the narrow, caller-independent value a consumer adapts a failed
// stage into. Every field mirrors data the caller already holds — no field
// requires a new read.
//
// Field provenance (backend/internal/mcpserver mirrors of the REST surface):
//
//	StageType, StageState, FailureCategory, FailureReason  <- the stage row
//	ProgressReported, TurnsThisAttempt, TokensThisAttempt  <- stage.progress
//	                                                          (the runner's
//	                                                          stage_progress
//	                                                          heartbeat)
//	RetryAttempt, RunnerKind                               <- the run row
//	IsDecompositionChild                                   <- derived from the
//	                                                          run's minted-child
//	                                                          shape
//
// ProgressReported distinguishes "the heartbeat reported zero activity" from
// "no heartbeat arrived at all": an ABSENT heartbeat leaves the counters at
// their zero values, which must never be read as observed inactivity.
//
// RunnerKind and IsDecompositionChild are carried because they are part of the
// evidence contract a consumer populates and are cheap to supply; no v1
// signature keys on them yet.
type Evidence struct {
	StageType            string
	StageState           string
	FailureCategory      string
	FailureReason        string
	RetryAttempt         int
	ProgressReported     bool
	TurnsThisAttempt     int
	TokensThisAttempt    int
	RunnerKind           string
	IsDecompositionChild bool
}

// describesFailure is the healthy-evidence guard: it admits only evidence that
// actually describes a FAILED stage. Two clauses:
//
//   - a non-empty StageState other than "failed" is a live or healthy stage.
//     This clause is load-bearing: a stage retried in place can carry the
//     PREVIOUS attempt's failure_reason on its row while running again, and
//     without this clause a reason-anchored signature would fire on a live
//     stage and hand the operator a recovery playbook for a failure that is
//     not happening.
//   - evidence carrying neither a category nor a reason names no failure at
//     all. This clause is defence in depth against a future counter-anchored
//     signature; with the v1 catalog every matcher independently requires a
//     non-empty anchor or a category, so no v1 matcher can fire on it.
func (ev Evidence) describesFailure() bool {
	if ev.StageState != "" && ev.StageState != stageStateFailed {
		return false
	}
	return ev.FailureCategory != "" || ev.FailureReason != ""
}

// Signature is one catalog entry: what the failure is, and what to do about
// it. match is unexported so the catalog owns its own predicates — a consumer
// reads a Hint, never a matcher.
type Signature struct {
	// ID is the stable lowercase slug an operator and the published catalog
	// both key on. It is the join key TestCatalogDocumentsEverySignature pins.
	ID string
	// Title is the one-phrase human name.
	Title string
	// Means is one sentence on what the failure IS — the diagnosis, not the
	// remedy.
	Means string
	// Playbook is the ORDERED recommended recovery sequence, each step a verb
	// or a concrete operator action.
	Playbook []string

	match func(Evidence) bool
}

// Hint is the surfaced match — the value a consumer renders. It carries json +
// jsonschema tags so an MCP SDK reflection schema generator renders it, and it
// deliberately echoes NO caller-supplied text: that is what makes the block
// constant-size BY CONSTRUCTION rather than by a length check.
type Hint struct {
	RegistryVersion string   `json:"registry_version" jsonschema:"the failure-signature catalog revision that produced this hint"`
	ID              string   `json:"id" jsonschema:"the stable signature slug, e.g. lineage_lock_contention; the key the published catalog (docs/architecture/failure-signatures.md) documents"`
	Title           string   `json:"title" jsonschema:"the one-phrase human name for this failure signature"`
	Means           string   `json:"means" jsonschema:"one sentence on what this failure IS — the diagnosis, not the remedy"`
	Playbook        []string `json:"playbook" jsonschema:"the ordered recommended recovery steps. Display-only: taking them is the operator's call, and nothing here gates the run"`
}

// Match walks the catalog in precedence order and returns the FIRST matching
// signature as a Hint, or nil when nothing matches (the fail-open contract).
//
// First-match-wins is load-bearing wherever evidence can satisfy two entries:
// a category-A failure whose detail cites BOTH a terminal external API error
// and an absorbed infra flake must classify as the incident, because the
// recoveries differ (back off vs retry immediately) and the wrong one burns
// retry budget against a live upstream incident.
func Match(ev Evidence) *Hint {
	if !ev.describesFailure() {
		return nil
	}
	for _, sig := range Registry() {
		if !sig.match(ev) {
			continue
		}
		return &Hint{
			RegistryVersion: RegistryVersion,
			ID:              sig.ID,
			Title:           sig.Title,
			Means:           sig.Means,
			Playbook:        append([]string(nil), sig.Playbook...),
		}
	}
	return nil
}

// cites reports whether the failure reason carries the given anchor. The
// anchors are SUBSTRING contracts: the runner and backend embed them inside a
// longer rendered line, so a prefix or equality test would miss real failures.
func cites(reason, anchor string) bool {
	return strings.Contains(reason, anchor)
}
