package wirecontract

// Mode selects how a Pair's two endpoints are compared.
type Mode int

const (
	// ModeExact requires the sorted (json name, options) tuple lists to be
	// EQUAL. Used for leaf structs that genuinely round-trip both ways, so
	// options (including omitempty) must match.
	ModeExact Mode = iota
	// ModeSubset requires every CONSUMER json name to exist on the EMITTER
	// (names only). Used when one side decodes a subset of the other's fields.
	// See checkSubset for the condition-3 option handling.
	ModeSubset
)

// Endpoint names one side of a wire contract: a repo-relative source file and
// the struct type declared in it.
type Endpoint struct {
	File string
	Type string
}

// Allowance is one emitter-only json name a ModeSubset pair tolerates, with the
// reason it is not a consumer-side omission.
type Allowance struct {
	JSONName string
	Reason   string
}

// Pair is one cross-module wire contract: an emitter struct and a consumer
// struct that must agree by json tag.
type Pair struct {
	// Name is a human label; Anchor is the tracking issue/ADR for the contract.
	Name   string
	Anchor string
	// Emitter marshals the wire bytes; Consumer decodes them.
	Emitter  Endpoint
	Consumer Endpoint
	Mode     Mode
	// AllowedEmitterOnly lists emitter json names the consumer legitimately
	// does not decode (ModeSubset only), each with a reason.
	AllowedEmitterOnly []Allowance
	// EmitterOnlyUnchecked skips the emitter-only allow-list entirely
	// (ModeSubset only): use when the emitter is a UNION whose extra names
	// belong to sibling consumers and an allow-list would restate the union.
	EmitterOnlyUnchecked bool
}

// Manifest is the declarative seed: the paired contracts, the files the
// completeness sweep scans, and the marker-bearing declarations that have no
// struct counterpart.
type Manifest struct {
	Pairs        []Pair
	CoveredFiles []string
	// UnpairedExemptions are marker-bearing declarations with no cross-module
	// struct counterpart (e.g. a marker about a field's referenced type rather
	// than the enclosing struct's own wire shape).
	UnpairedExemptions []Endpoint
}

const (
	promptFile    = "backend/internal/server/prompt.go"
	bindingsFile  = "backend/internal/server/binding_assertions.go"
	pullRequestGo = "backend/internal/server/pullrequest.go"
	scopeCompFile = "backend/internal/server/scope_completeness.go"
	acceptanceGo  = "backend/internal/server/acceptance.go"
	uploadFile    = "runner/internal/upload/upload.go"
	scenarioFile  = "runner/internal/scenario/scenario.go"
)

// SeedManifest is the repo's cross-module wire contract manifest. Verified
// GREEN ON ARRIVAL against the current tree (the six ModeExact pairs are
// byte-equal today including options; the four ModeSubset consumers are name
// subsets of their emitters). A drift at test time means the guard found a real
// divergence — report it, do not weaken the check.
func SeedManifest() Manifest {
	return Manifest{
		Pairs: []Pair{
			// ---- Six ModeExact leaf pairs (round-trip both ways) ----
			{
				Name: "scope_file", Anchor: "#824",
				Emitter:  Endpoint{File: promptFile, Type: "scopeFile"},
				Consumer: Endpoint{File: uploadFile, Type: "ScopeFile"},
				Mode:     ModeExact,
			},
			{
				Name: "binding_assertion", Anchor: "#1171",
				Emitter:  Endpoint{File: bindingsFile, Type: "bindingAssertion"},
				Consumer: Endpoint{File: uploadFile, Type: "BindingAssertion"},
				Mode:     ModeExact,
			},
			{
				Name: "scope_exemption", Anchor: "#1229",
				Emitter:  Endpoint{File: promptFile, Type: "scopeExemption"},
				Consumer: Endpoint{File: uploadFile, Type: "ScopeExemption"},
				Mode:     ModeExact,
			},
			{
				Name: "diff_coverage_config", Anchor: "#1888",
				Emitter:  Endpoint{File: promptFile, Type: "diffCoverageConfig"},
				Consumer: Endpoint{File: uploadFile, Type: "DiffCoverageConfig"},
				Mode:     ModeExact,
			},
			{
				Name: "fixup_apply_patch", Anchor: "#1165",
				Emitter:  Endpoint{File: promptFile, Type: "fixupApplyPatch"},
				Consumer: Endpoint{File: uploadFile, Type: "FixupApplyPatch"},
				Mode:     ModeExact,
			},
			{
				Name: "unsatisfied_assertion", Anchor: "#2501",
				Emitter:  Endpoint{File: scopeCompFile, Type: "unsatisfiedAssertion"},
				Consumer: Endpoint{File: uploadFile, Type: "BindingAssertionReport"},
				Mode:     ModeExact,
			},
			// ---- E72.4 / #3328 replayable scenario corpus (ModeExact) ----
			// The runner PRODUCES ReplaySet / ReplayedScenario (injected into the
			// validated verdict) and CONSUMES RetiredEntry / AcceptanceCriterionEntry
			// (served on the acceptance prompt). Exact mode pins the cap fields
			// (cap / corpus_size / served / sampled_out) so a renamed tag cannot
			// silently drop the replay-cap evidence from the recorded outcome.
			{
				Name: "acceptance_replay_set", Anchor: "#3328",
				Emitter:  Endpoint{File: scenarioFile, Type: "ReplaySet"},
				Consumer: Endpoint{File: acceptanceGo, Type: "acceptanceReplay"},
				Mode:     ModeExact,
			},
			{
				Name: "acceptance_replayed_scenario", Anchor: "#3328",
				Emitter:  Endpoint{File: scenarioFile, Type: "ReplayedScenario"},
				Consumer: Endpoint{File: acceptanceGo, Type: "acceptanceReplayedScenario"},
				Mode:     ModeExact,
			},
			{
				Name: "acceptance_retired_scenario", Anchor: "#3328",
				Emitter:  Endpoint{File: promptFile, Type: "retiredScenarioEntry"},
				Consumer: Endpoint{File: scenarioFile, Type: "RetiredEntry"},
				Mode:     ModeExact,
			},
			{
				Name: "acceptance_criterion_entry", Anchor: "#3328",
				Emitter:  Endpoint{File: promptFile, Type: "acceptanceCriterionEntry"},
				Consumer: Endpoint{File: uploadFile, Type: "AcceptanceCriterionEntry"},
				Mode:     ModeExact,
			},
			// ---- ModeSubset: prompt response -> fetched prompt ----
			{
				Name: "prompt_response", Anchor: "#2501/#2596",
				Emitter:  Endpoint{File: promptFile, Type: "promptResponse"},
				Consumer: Endpoint{File: uploadFile, Type: "FetchedPrompt"},
				Mode:     ModeSubset,
				AllowedEmitterOnly: []Allowance{{
					JSONName: "scope_constraint",
					Reason:   "decomposed-child agent-facing narrowing (#2596) read by the SPA only; grep confirms no ScopeConstraint/scope_constraint reference anywhere under runner/",
				}},
			},
			// ---- ModeSubset: pullRequestBody union -> the three ship bodies ----
			// pullRequestBody is the UNION of every ship outcome, so its extra
			// names belong to sibling bodies; an allow-list would have to
			// restate the whole union, hence EmitterOnlyUnchecked.
			{
				Name: "ship_scope_park", Anchor: "#1231/#2501",
				Emitter:              Endpoint{File: pullRequestGo, Type: "pullRequestBody"},
				Consumer:             Endpoint{File: uploadFile, Type: "pullRequestScopeParkBody"},
				Mode:                 ModeSubset,
				EmitterOnlyUnchecked: true,
			},
			{
				Name: "ship_failure", Anchor: "#742/#2169",
				Emitter:              Endpoint{File: pullRequestGo, Type: "pullRequestBody"},
				Consumer:             Endpoint{File: uploadFile, Type: "pullRequestFailureBody"},
				Mode:                 ModeSubset,
				EmitterOnlyUnchecked: true,
			},
			{
				Name: "ship_child_push", Anchor: "#771",
				Emitter:              Endpoint{File: pullRequestGo, Type: "pullRequestBody"},
				Consumer:             Endpoint{File: uploadFile, Type: "pullRequestChildPushBody"},
				Mode:                 ModeSubset,
				EmitterOnlyUnchecked: true,
			},
		},
		CoveredFiles: []string{
			promptFile,
			bindingsFile,
			pullRequestGo,
			scopeCompFile,
			acceptanceGo,
			uploadFile,
			scenarioFile,
		},
		UnpairedExemptions: []Endpoint{
			// ShipPlanArgs carries the marker on its Reachability field, but the
			// contract is about reachability.Result's tags (already pinned by
			// upload_test.go's exact-wire-key test), not the args struct's own
			// wire shape — so it has no struct counterpart to pair with.
			{File: uploadFile, Type: "ShipPlanArgs"},
		},
	}
}
