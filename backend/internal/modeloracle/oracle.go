// Package modeloracle defines the provider-agnostic seam the model-id
// validation layer (#1339) queries to decide whether a model id named in a
// workflow spec is a real, currently-served model.
//
// The seam is intentionally minimal: a single Snapshot(ctx, provider) method
// returning the set of known model ids for the provider plus two booleans —
// fresh (the snapshot is current, so an absence is authoritative) and ok (a
// snapshot is available at all). This slice ships exactly ONE production
// implementation, NoData, whose Snapshot returns ok=false for EVERY provider —
// so validation is fail-open and inert in production today. The real snapshot
// source (a cached /v1/models poll) is #1335's scope; when it lands, it
// implements this same interface and the validation logic upstream changes not
// at all.
//
// The Snapshot contract carries no deprecation channel: a deprecated/sunset
// model and a typo both manifest as absence-from-a-fresh-list. Distinguishing
// them (deprecated → warn, typo → reject) needs a richer contract and is
// deferred to #1335.
package modeloracle

import "context"

// ModelOracle is the binding, provider-agnostic snapshot contract the
// validation layer queries. An implementation answers, for a given provider,
// the set of model ids it currently serves.
//
// The two booleans encode confidence:
//
//   - ok=false  — no snapshot is available for this provider; the caller MUST
//     fail open (accept any model, warn that it could not be verified).
//   - fresh=false — a snapshot exists but is stale; absence from a stale list
//     is NOT authoritative, so the caller MUST fail open as well.
//   - fresh && ok — the returned set is authoritative: a model present in it is
//     valid; a model ABSENT from it is definitively rejected.
type ModelOracle interface {
	Snapshot(ctx context.Context, provider string) (models []string, fresh bool, ok bool)
}

// NoData is the wired production default for this slice: it has no snapshot for
// any provider, so Snapshot returns (nil, false, false) universally. Every
// model is therefore "unverifiable" and the validation layer accepts it with a
// warning — the safe degraded state until #1335 populates a real snapshot, at
// which point swapping the wired oracle makes validation live with zero
// validation-code change.
type NoData struct{}

// Snapshot reports no data for every provider (the universal fail-open
// contract). The ctx and provider are accepted to satisfy the interface and
// ignored — NoData has nothing to key on.
func (NoData) Snapshot(_ context.Context, _ string) (models []string, fresh bool, ok bool) {
	return nil, false, false
}

// NewNoData returns the wired no-data oracle as a ModelOracle. It is the single
// constructor serve.go uses, so the production wiring never depends on the
// concrete type.
func NewNoData() ModelOracle { return NoData{} }

// Static is a map-backed oracle used by tests and fixtures: Models maps a
// provider to the set of model ids it serves, and Fresh declares whether those
// sets are authoritative (fresh && ok) or stale (fail-open). A provider absent
// from the map returns ok=false — same as NoData for that provider — so a
// fixture need only populate the providers a test exercises.
type Static struct {
	// Models maps provider -> served model ids.
	Models map[string][]string
	// Fresh declares whether the snapshots are current. When false, Snapshot
	// returns fresh=false even for a populated provider, so the caller fails
	// open (stale lists cannot authoritatively reject).
	Fresh bool
}

// Snapshot returns the configured set for provider with ok=true when the
// provider is present in Models, else (nil, Fresh, false) — a provider with no
// configured set has no snapshot (ok=false), regardless of Fresh.
func (s Static) Snapshot(_ context.Context, provider string) (models []string, fresh bool, ok bool) {
	set, present := s.Models[provider]
	if !present {
		return nil, s.Fresh, false
	}
	return set, s.Fresh, true
}
