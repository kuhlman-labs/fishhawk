package planreview_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// --- ResolveAuthority ---

func TestResolveAuthority_Gating(t *testing.T) {
	r := spec.ReviewersConfig{Agent: 2, Human: 0}
	if got := planreview.ResolveAuthority(r); got != planreview.AuthorityGating {
		t.Errorf("ResolveAuthority(%+v) = %q, want %q", r, got, planreview.AuthorityGating)
	}
}

func TestResolveAuthority_Advisory(t *testing.T) {
	r := spec.ReviewersConfig{Agent: 1, Human: 1}
	if got := planreview.ResolveAuthority(r); got != planreview.AuthorityAdvisory {
		t.Errorf("ResolveAuthority(%+v) = %q, want %q", r, got, planreview.AuthorityAdvisory)
	}
}

func TestResolveAuthority_Gateless(t *testing.T) {
	r := spec.ReviewersConfig{Agent: 0, Human: 1}
	if got := planreview.ResolveAuthority(r); got != planreview.AuthorityGateless {
		t.Errorf("ResolveAuthority(%+v) = %q, want %q", r, got, planreview.AuthorityGateless)
	}
}

func TestResolveAuthority_GatelessZero(t *testing.T) {
	r := spec.ReviewersConfig{}
	if got := planreview.ResolveAuthority(r); got != planreview.AuthorityGateless {
		t.Errorf("ResolveAuthority(zero) = %q, want %q", got, planreview.AuthorityGateless)
	}
}

// TestResolveAuthority_AgentsList pins that the heterogeneous agents list
// (#955) feeds the ADR-027 decision table through AgentCount(): the list's
// length is the effective agent count for all three modes, and it
// supersedes the bare agent integer.
func TestResolveAuthority_AgentsList(t *testing.T) {
	agents := []spec.AgentReviewer{
		{Provider: "anthropic", Model: "claude-opus-4-8"},
		{Provider: "codex"},
	}
	cases := []struct {
		name string
		r    spec.ReviewersConfig
		want planreview.AuthorityMode
	}{
		{"gating: agents list, human 0", spec.ReviewersConfig{Agents: agents}, planreview.AuthorityGating},
		{"advisory: agents list, human 1", spec.ReviewersConfig{Agents: agents, Human: 1}, planreview.AuthorityAdvisory},
		{"gateless: no list, agent 0, human 1", spec.ReviewersConfig{Human: 1}, planreview.AuthorityGateless},
		{"gating: list supersedes agent 0", spec.ReviewersConfig{Agent: 0, Agents: agents[:1]}, planreview.AuthorityGating},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := planreview.ResolveAuthority(tc.r); got != tc.want {
				t.Errorf("ResolveAuthority(%+v) = %q, want %q", tc.r, got, tc.want)
			}
		})
	}
}

// TestResolveAuthorityWithSource_Declared is the E53.2 (#2225) table: an
// explicit reviewers.authority WINS over the count-derived rule and reports
// SourceDeclared, the zero-agent defence branch always yields
// gateless/derived even for a declared value (unreachable for a validated
// spec — spec.Validate rejects it — but defended for campaign-override
// bytes), an out-of-enum Authority string falls THROUGH to the count table
// (never honoured) and reports SourceDerived, and ResolveAuthority agrees
// with ResolveAuthorityWithSource's mode on every row.
func TestResolveAuthorityWithSource_Declared(t *testing.T) {
	oneAgent := []spec.AgentReviewer{{Provider: "anthropic"}}
	threeAgents := []spec.AgentReviewer{{Provider: "anthropic"}, {Provider: "codex"}, {Provider: "claudecode"}}
	cases := []struct {
		name       string
		r          spec.ReviewersConfig
		wantMode   planreview.AuthorityMode
		wantSource planreview.AuthoritySource
	}{
		{
			"declared gating wins over human>0",
			spec.ReviewersConfig{Agents: oneAgent, Human: 1, Authority: "gating"},
			planreview.AuthorityGating, planreview.SourceDeclared,
		},
		{
			"declared advisory wins over human==0",
			spec.ReviewersConfig{Agents: oneAgent, Human: 0, Authority: "advisory"},
			planreview.AuthorityAdvisory, planreview.SourceDeclared,
		},
		{
			"declared gating, single agent",
			spec.ReviewersConfig{Agents: oneAgent, Authority: "gating"},
			planreview.AuthorityGating, planreview.SourceDeclared,
		},
		{
			"declared advisory, many agents",
			spec.ReviewersConfig{Agents: threeAgents, Authority: "advisory"},
			planreview.AuthorityAdvisory, planreview.SourceDeclared,
		},
		{
			"zero agents + declared gating -> gateless/derived (defence branch)",
			spec.ReviewersConfig{Human: 1, Authority: "gating"},
			planreview.AuthorityGateless, planreview.SourceDerived,
		},
		{
			"zero agents + declared advisory -> gateless/derived (defence branch)",
			spec.ReviewersConfig{Human: 0, Authority: "advisory"},
			planreview.AuthorityGateless, planreview.SourceDerived,
		},
		{
			"out-of-enum authority falls through to count table (gating)",
			spec.ReviewersConfig{Agents: oneAgent, Human: 0, Authority: "sometimes"},
			planreview.AuthorityGating, planreview.SourceDerived,
		},
		{
			"out-of-enum authority falls through to count table (advisory)",
			spec.ReviewersConfig{Agents: oneAgent, Human: 1, Authority: "sometimes"},
			planreview.AuthorityAdvisory, planreview.SourceDerived,
		},
		{
			"empty authority + counts -> gating/derived (unchanged default)",
			spec.ReviewersConfig{Agents: oneAgent, Human: 0},
			planreview.AuthorityGating, planreview.SourceDerived,
		},
		{
			"empty authority + counts -> advisory/derived (unchanged default)",
			spec.ReviewersConfig{Agents: oneAgent, Human: 1},
			planreview.AuthorityAdvisory, planreview.SourceDerived,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode, source := planreview.ResolveAuthorityWithSource(tc.r)
			if mode != tc.wantMode {
				t.Errorf("ResolveAuthorityWithSource(%+v) mode = %q, want %q", tc.r, mode, tc.wantMode)
			}
			if source != tc.wantSource {
				t.Errorf("ResolveAuthorityWithSource(%+v) source = %q, want %q", tc.r, source, tc.wantSource)
			}
			// ResolveAuthority must agree with the sibling's mode on every row.
			if got := planreview.ResolveAuthority(tc.r); got != mode {
				t.Errorf("ResolveAuthority(%+v) = %q, want %q (must agree with ResolveAuthorityWithSource)", tc.r, got, mode)
			}
		})
	}
}

// --- ResolveAuthority over a genuinely v2-parsed spec (E52.3 / #2215) ---

// v2SpecWithReviewers renders a workflow-v2 document whose plan stage
// carries the given reviewers YAML block (indented to the stage), so the
// authority rows below are driven from real schema-validated bytes rather
// than a hand-built struct.
func v2SpecWithReviewers(reviewersBlock string) []byte {
	return []byte(`version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
` + reviewersBlock + `        produces:
          - artifact: plan
            schema: standard_v1
`)
}

// parseV2Reviewers parses a v2 document through spec.ParseBytes and returns
// its plan stage's reviewers block (possibly nil).
func parseV2Reviewers(t *testing.T, reviewersBlock string) *spec.ReviewersConfig {
	t.Helper()
	s, err := spec.ParseBytes(v2SpecWithReviewers(reviewersBlock))
	if err != nil {
		t.Fatalf("spec.ParseBytes(v2): %v", err)
	}
	return s.Workflows["feature_change"].Stages[0].Reviewers
}

// TestResolveAuthority_V2ParsedSpec is the cross-boundary seam test for
// E52.3 / #2215. workflow-v2 removed the bare `reviewers.agent` integer —
// an INPUT this resolver reads through AgentCount() — so per-layer units
// over hand-built structs would all still pass even if the schema change
// had severed the schema -> typed-struct -> authority-resolution path.
// This drives all three ADR-027 rows from real v2 YAML end to end.
func TestResolveAuthority_V2ParsedSpec(t *testing.T) {
	cases := []struct {
		name      string
		reviewers string
		want      planreview.AuthorityMode
	}{
		{
			name: "gating: one agent, no human",
			reviewers: `        reviewers:
          agents:
            - provider: anthropic
`,
			want: planreview.AuthorityGating,
		},
		{
			name: "advisory: two agents plus a human",
			reviewers: `        reviewers:
          agents:
            - provider: anthropic
            - provider: codex
          human: 1
`,
			want: planreview.AuthorityAdvisory,
		},
		{
			name: "gateless: no agents, one human",
			reviewers: `        reviewers:
          human: 1
`,
			want: planreview.AuthorityGateless,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rv := parseV2Reviewers(t, tc.reviewers)
			if rv == nil {
				t.Fatal("parsed Reviewers = nil, want the declared block")
			}
			if got := planreview.ResolveAuthority(*rv); got != tc.want {
				t.Errorf("ResolveAuthority(%+v) = %q, want %q", *rv, got, tc.want)
			}
		})
	}
}

// TestResolveAuthority_V2ParsedSpec_NilReviewersBlock is the RESOLVED half
// of the absent-block contract. An absent reviewers block configures NO
// reviewers: Stage.Reviewers stays nil and each consumer interprets that nil
// directly — nothing materializes a {human:1} literal, and since E52.12 /
// #2322 the documentation no longer claims one. What this asserts is that
// nil and {Human:1} are OBSERVATIONALLY EQUIVALENT at the resolver: both are
// gateless, because with zero agent reviewers the human count cannot change
// the mode. That equivalence is why a materialized default would have been
// inert, and it is why the correction is documentation-only.
func TestResolveAuthority_V2ParsedSpec_NilReviewersBlock(t *testing.T) {
	if rv := parseV2Reviewers(t, ""); rv != nil {
		t.Fatalf("parsed Reviewers = %+v, want nil for an absent block", rv)
	}
	documentedDefault := spec.ReviewersConfig{Human: 1}
	if got := planreview.ResolveAuthority(documentedDefault); got != planreview.AuthorityGateless {
		t.Errorf("ResolveAuthority(documented {human:1} default) = %q, want %q", got, planreview.AuthorityGateless)
	}
	if got := planreview.ResolveAuthority(spec.ReviewersConfig{}); got != planreview.AuthorityGateless {
		t.Errorf("ResolveAuthority(zero value, the nil substitute) = %q, want %q — nil and {human:1} must stay indistinguishable", got, planreview.AuthorityGateless)
	}
}

// specWithReviewersAtVersion renders a workflow document at the given major
// whose plan stage carries the given reviewers YAML block. The v0/v1/v2
// grammars agree on this stage shape, so one template drives every major.
func specWithReviewersAtVersion(version, reviewersBlock string) []byte {
	return []byte(`version: "` + version + `"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
` + reviewersBlock + `        produces:
          - artifact: plan
            schema: standard_v1
`)
}

// TestResolveAuthority_ParsedSpec_NoReviewersBlockGatelessOnEveryMajor is the
// cross-layer done-means test for E52.12 / #2322. The correction this issue
// ships asserts, on every documented surface, that an absent `reviewers`
// block configures no reviewers and the stage resolves GATELESS — on v0 and
// v1 as well as v2. Documentation is not enforced by compilation, so that
// claim is pinned here behaviorally, end to end: real schema-validated bytes
// at each major go through spec.ParseBytes (the parse layer) and the parsed
// result's nil substitute goes through ResolveAuthority (the authority layer).
//
// This is the integration test the cross-boundary rule requires — the change
// spans the schema, parse and authority-resolution layers, and asserting each
// in isolation would not catch a severed seam between them. It goes RED if
// anyone materializes a {human:1} default at any major, which is exactly the
// regression the corrected documentation now forbids.
func TestResolveAuthority_ParsedSpec_NoReviewersBlockGatelessOnEveryMajor(t *testing.T) {
	// Each major is exercised at a version string its own enum accepts:
	// v0/v1 require a minor-qualified version, v2 takes the bare major.
	for _, version := range []string{"0.7", "1.6", "2"} {
		t.Run("v"+version, func(t *testing.T) {
			s, err := spec.ParseBytes(specWithReviewersAtVersion(version, ""))
			if err != nil {
				t.Fatalf("spec.ParseBytes(v%s): %v", version, err)
			}
			rv := s.Workflows["feature_change"].Stages[0].Reviewers
			if rv != nil {
				t.Fatalf("parsed Reviewers = %+v, want nil for an absent block at major %s", rv, version)
			}
			// nil is interpreted directly by every consumer; the zero value
			// is what dereferencing it yields. Zero agent reviewers is what
			// makes the stage gateless — no human approval is conferred.
			if got := planreview.ResolveAuthority(spec.ReviewersConfig{}); got != planreview.AuthorityGateless {
				t.Errorf("ResolveAuthority(nil substitute at major %s) = %q, want %q", version, got, planreview.AuthorityGateless)
			}
		})
	}
}

// --- Verdict JSON round-trip ---

func TestReviewVerdict_JSONRoundTrip_Approve(t *testing.T) {
	v := planreview.ReviewVerdict{
		Verdict:  planreview.VerdictApprove,
		FreeForm: "looks good",
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got planreview.ReviewVerdict
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Verdict != planreview.VerdictApprove {
		t.Errorf("Verdict = %q, want %q", got.Verdict, planreview.VerdictApprove)
	}
	if got.FreeForm != v.FreeForm {
		t.Errorf("FreeForm = %q, want %q", got.FreeForm, v.FreeForm)
	}
	if len(got.Concerns) != 0 {
		t.Errorf("Concerns should be empty, got %d", len(got.Concerns))
	}
}

func TestReviewVerdict_JSONRoundTrip_ApproveWithConcerns(t *testing.T) {
	v := planreview.ReviewVerdict{
		Verdict: planreview.VerdictApproveWithConcerns,
		Concerns: []planreview.Concern{
			{Severity: planreview.SeverityMedium, Category: "scope", Note: "touches more files than expected"},
		},
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got planreview.ReviewVerdict
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Verdict != planreview.VerdictApproveWithConcerns {
		t.Errorf("Verdict = %q, want %q", got.Verdict, planreview.VerdictApproveWithConcerns)
	}
	if len(got.Concerns) != 1 {
		t.Fatalf("Concerns len = %d, want 1", len(got.Concerns))
	}
	if got.Concerns[0].Severity != planreview.SeverityMedium {
		t.Errorf("Concerns[0].Severity = %q, want %q", got.Concerns[0].Severity, planreview.SeverityMedium)
	}
	if got.Concerns[0].Category != "scope" {
		t.Errorf("Concerns[0].Category = %q, want %q", got.Concerns[0].Category, "scope")
	}
}

func TestReviewVerdict_JSONRoundTrip_Reject(t *testing.T) {
	v := planreview.ReviewVerdict{
		Verdict: planreview.VerdictReject,
		Concerns: []planreview.Concern{
			{Severity: planreview.SeverityHigh, Category: "correctness", Note: "approach will break the auth flow"},
			{Severity: planreview.SeverityLow, Category: "style", Note: "minor naming inconsistency"},
		},
		FreeForm: "reject: fundamental approach is wrong",
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got planreview.ReviewVerdict
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Verdict != planreview.VerdictReject {
		t.Errorf("Verdict = %q, want %q", got.Verdict, planreview.VerdictReject)
	}
	if len(got.Concerns) != 2 {
		t.Fatalf("Concerns len = %d, want 2", len(got.Concerns))
	}
	if got.Concerns[0].Severity != planreview.SeverityHigh {
		t.Errorf("Concerns[0].Severity = %q, want %q", got.Concerns[0].Severity, planreview.SeverityHigh)
	}
	if got.FreeForm != v.FreeForm {
		t.Errorf("FreeForm = %q, want %q", got.FreeForm, v.FreeForm)
	}
}

// --- PlanReviewedPayload JSON round-trip ---

func TestPlanReviewedPayload_JSONRoundTrip(t *testing.T) {
	p := planreview.PlanReviewedPayload{
		ReviewerKind:    "agent",
		ReviewerModel:   "claude-opus-4-7",
		Authority:       planreview.AuthorityGating,
		Verdict:         planreview.VerdictApprove,
		Concerns:        nil,
		FreeForm:        "plan is sound",
		ReviewerVersion: "0.30.5 (codex-cli)",
		ReviewerBinary:  "codex",
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got planreview.PlanReviewedPayload
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ReviewerKind != p.ReviewerKind {
		t.Errorf("ReviewerKind = %q, want %q", got.ReviewerKind, p.ReviewerKind)
	}
	if got.ReviewerModel != p.ReviewerModel {
		t.Errorf("ReviewerModel = %q, want %q", got.ReviewerModel, p.ReviewerModel)
	}
	if got.Authority != p.Authority {
		t.Errorf("Authority = %q, want %q", got.Authority, p.Authority)
	}
	if got.Verdict != p.Verdict {
		t.Errorf("Verdict = %q, want %q", got.Verdict, p.Verdict)
	}
	// Reviewer provenance survives the round-trip (#1768).
	if got.ReviewerVersion != p.ReviewerVersion {
		t.Errorf("ReviewerVersion = %q, want %q", got.ReviewerVersion, p.ReviewerVersion)
	}
	if got.ReviewerBinary != p.ReviewerBinary {
		t.Errorf("ReviewerBinary = %q, want %q", got.ReviewerBinary, p.ReviewerBinary)
	}
}

// TestPlanReviewedPayload_ProvenanceOmittedFromWire pins the #1768 additive-field
// contract: a provenance-free payload marshals WITHOUT reviewer_version /
// reviewer_binary (byte-identical to pre-#1768 entries), and an old stored
// payload (no keys) decodes with both fields empty.
func TestPlanReviewedPayload_ProvenanceOmittedFromWire(t *testing.T) {
	p := planreview.PlanReviewedPayload{
		ReviewerKind: "agent",
		Authority:    planreview.AuthorityAdvisory,
		Verdict:      planreview.VerdictApprove,
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), "reviewer_version") {
		t.Errorf("provenance-free payload must omit reviewer_version (omitempty): %s", b)
	}
	if strings.Contains(string(b), "reviewer_binary") {
		t.Errorf("provenance-free payload must omit reviewer_binary (omitempty): %s", b)
	}

	var got planreview.PlanReviewedPayload
	if err := json.Unmarshal([]byte(`{"reviewer_kind":"agent","authority":"advisory","verdict":"approve"}`), &got); err != nil {
		t.Fatalf("Unmarshal pre-#1768 payload: %v", err)
	}
	if got.ReviewerVersion != "" || got.ReviewerBinary != "" {
		t.Errorf("ReviewerVersion=%q ReviewerBinary=%q, want both empty decoding a pre-#1768 payload", got.ReviewerVersion, got.ReviewerBinary)
	}
}

// --- ImplementReviewedPayload JSON round-trip (ADR-027 impl 2/2) ---

func TestImplementReviewedPayload_JSONRoundTrip(t *testing.T) {
	p := planreview.ImplementReviewedPayload{
		ReviewerKind:  "agent",
		ReviewerModel: "claude-opus-4-7",
		Authority:     planreview.AuthorityAdvisory,
		Verdict:       planreview.VerdictApproveWithConcerns,
		Concerns: []planreview.Concern{
			{Severity: planreview.SeverityLow, Category: "scope", Note: "touched a file outside scope.files"},
		},
		FreeForm:        "diff implements the plan",
		ReviewerVersion: "0.30.5 (codex-cli)",
		ReviewerBinary:  "codex",
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got planreview.ImplementReviewedPayload
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ReviewerKind != p.ReviewerKind {
		t.Errorf("ReviewerKind = %q, want %q", got.ReviewerKind, p.ReviewerKind)
	}
	if got.Authority != p.Authority {
		t.Errorf("Authority = %q, want %q", got.Authority, p.Authority)
	}
	if got.Verdict != p.Verdict {
		t.Errorf("Verdict = %q, want %q", got.Verdict, p.Verdict)
	}
	if len(got.Concerns) != 1 || got.Concerns[0].Category != "scope" {
		t.Errorf("Concerns = %+v, want one scope concern", got.Concerns)
	}
	// Reviewer provenance survives the round-trip (#1768).
	if got.ReviewerVersion != p.ReviewerVersion {
		t.Errorf("ReviewerVersion = %q, want %q", got.ReviewerVersion, p.ReviewerVersion)
	}
	if got.ReviewerBinary != p.ReviewerBinary {
		t.Errorf("ReviewerBinary = %q, want %q", got.ReviewerBinary, p.ReviewerBinary)
	}
}

// TestImplementReviewedPayload_ProvenanceOmittedFromWire pins the #1768
// additive-field contract for the implement payload: a provenance-free payload
// marshals WITHOUT reviewer_version / reviewer_binary, and an old stored payload
// decodes with both fields empty.
func TestImplementReviewedPayload_ProvenanceOmittedFromWire(t *testing.T) {
	p := planreview.ImplementReviewedPayload{
		ReviewerKind: "agent",
		Authority:    planreview.AuthorityAdvisory,
		Verdict:      planreview.VerdictApprove,
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), "reviewer_version") {
		t.Errorf("provenance-free payload must omit reviewer_version (omitempty): %s", b)
	}
	if strings.Contains(string(b), "reviewer_binary") {
		t.Errorf("provenance-free payload must omit reviewer_binary (omitempty): %s", b)
	}

	var got planreview.ImplementReviewedPayload
	if err := json.Unmarshal([]byte(`{"reviewer_kind":"agent","authority":"advisory","verdict":"approve"}`), &got); err != nil {
		t.Fatalf("Unmarshal pre-#1768 payload: %v", err)
	}
	if got.ReviewerVersion != "" || got.ReviewerBinary != "" {
		t.Errorf("ReviewerVersion=%q ReviewerBinary=%q, want both empty decoding a pre-#1768 payload", got.ReviewerVersion, got.ReviewerBinary)
	}
}

// TestResolveAuthority_ImplementParity confirms the authority table is
// identical for the implement stage — the same ReviewersConfig inputs
// produce the same authority modes (ADR-027 impl 2/2 reuses the table).
func TestResolveAuthority_ImplementParity(t *testing.T) {
	cases := []struct {
		agent, human int
		want         planreview.AuthorityMode
	}{
		{1, 0, planreview.AuthorityGating},
		{1, 1, planreview.AuthorityAdvisory},
		{0, 1, planreview.AuthorityGateless},
	}
	for _, c := range cases {
		got := planreview.ResolveAuthority(spec.ReviewersConfig{Agent: c.agent, Human: c.human})
		if got != c.want {
			t.Errorf("ResolveAuthority(agent=%d,human=%d) = %q, want %q", c.agent, c.human, got, c.want)
		}
	}
}

// TestReviewVerdict_UsageIsolatedFromAgentJSON asserts the json:"-" tag on
// ReviewVerdict.Usage keeps it out of the agent-emitted verdict decode
// (#681): a verdict body WITHOUT a usage key decodes with a zero-value,
// Known=false Usage, and even a body that DOES carry a "usage" key cannot
// populate it — usage comes from the API/CLI envelope the adapter attaches,
// never from the model's response, so a model can't spoof the cost figure.
func TestReviewVerdict_UsageIsolatedFromAgentJSON(t *testing.T) {
	// (a) No usage key in the agent JSON — Usage stays zero-value.
	var v planreview.ReviewVerdict
	if err := json.Unmarshal([]byte(`{"verdict":"approve"}`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v.Usage != (planreview.Usage{}) {
		t.Errorf("Usage = %+v, want zero-value (json:\"-\" must isolate it)", v.Usage)
	}
	if v.Usage.Known {
		t.Error("Usage.Known = true, want false for an agent JSON with no usage")
	}

	// (b) A spoofed usage key in the agent JSON must NOT populate Usage.
	var spoofed planreview.ReviewVerdict
	if err := json.Unmarshal(
		[]byte(`{"verdict":"approve","usage":{"InputTokens":999,"OutputTokens":888,"Known":true}}`),
		&spoofed,
	); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if spoofed.Usage != (planreview.Usage{}) {
		t.Errorf("spoofed Usage = %+v, want zero-value — json:\"-\" must reject a model-supplied usage key", spoofed.Usage)
	}
}

// TestUsage_CachedInputTokensAccessor pins the back-compat accessor (#1343):
// after splitting the former CachedInputTokens field into separate cache
// read / write buckets, the CachedInputTokens() method must return their sum
// so every prior reader of the summed total keeps working. The zero value
// returns 0, and Usage stays a comparable struct (asserted by the zero-value
// equality checks above) so adding the method changed no other contract.
func TestUsage_CachedInputTokensAccessor(t *testing.T) {
	u := planreview.Usage{
		InputTokens:           1000,
		CacheReadInputTokens:  400,
		CacheWriteInputTokens: 150,
		OutputTokens:          2000,
		Known:                 true,
	}
	if got := u.CachedInputTokens(); got != 550 {
		t.Errorf("CachedInputTokens() = %d, want 550 (read 400 + write 150)", got)
	}
	if got := (planreview.Usage{}).CachedInputTokens(); got != 0 {
		t.Errorf("zero-value CachedInputTokens() = %d, want 0", got)
	}
}

// --- ConcernResolutions on the wire (#984) ---

// TestImplementReviewedPayload_JSONRoundTrip_WithResolutions covers the
// #984 delta-verification additions: concern_resolutions ride on the
// authoritative implement_reviewed audit payload and round-trip intact.
func TestImplementReviewedPayload_JSONRoundTrip_WithResolutions(t *testing.T) {
	p := planreview.ImplementReviewedPayload{
		ReviewerKind:  "agent",
		ReviewerModel: "claude-opus-4-8",
		Authority:     planreview.AuthorityAdvisory,
		Verdict:       planreview.VerdictApprove,
		ConcernResolutions: []planreview.ConcernResolution{
			{ID: "11111111-1111-1111-1111-111111111111", Resolution: "confirmed", Note: "resolved by the fixup"},
			{ID: "22222222-2222-2222-2222-222222222222", Resolution: "reopened"},
		},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got planreview.ImplementReviewedPayload
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(got.ConcernResolutions) != 2 {
		t.Fatalf("ConcernResolutions = %d entries, want 2", len(got.ConcernResolutions))
	}
	if got.ConcernResolutions[0] != p.ConcernResolutions[0] || got.ConcernResolutions[1] != p.ConcernResolutions[1] {
		t.Errorf("ConcernResolutions = %+v, want %+v", got.ConcernResolutions, p.ConcernResolutions)
	}
}

// TestImplementReviewedPayload_NoResolutions_OmittedFromWire pins the
// additive-field contract in both directions: a resolutions-free payload
// marshals WITHOUT the concern_resolutions key (byte-identical to
// pre-#984 entries), and an old stored payload (no key) unmarshals with
// a nil slice.
func TestImplementReviewedPayload_NoResolutions_OmittedFromWire(t *testing.T) {
	p := planreview.ImplementReviewedPayload{
		ReviewerKind: "agent",
		Authority:    planreview.AuthorityAdvisory,
		Verdict:      planreview.VerdictApprove,
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), "concern_resolutions") {
		t.Errorf("resolutions-free payload must omit the key (omitempty): %s", b)
	}

	var got planreview.ImplementReviewedPayload
	if err := json.Unmarshal([]byte(`{"reviewer_kind":"agent","authority":"advisory","verdict":"approve"}`), &got); err != nil {
		t.Fatalf("Unmarshal pre-#984 payload: %v", err)
	}
	if got.ConcernResolutions != nil {
		t.Errorf("ConcernResolutions = %+v, want nil decoding a pre-#984 payload", got.ConcernResolutions)
	}
}

// TestImplementReviewedPayload_OriginHeadSHA_OmittedFromWire pins the #1250
// additive-field contract: an Origin/HeadSHA-free payload (the first review
// and the parent-decomposition consolidated review) marshals WITHOUT either
// key (byte-identical to pre-#1250 entries), and an old stored payload (no
// keys) decodes with both fields empty.
func TestImplementReviewedPayload_OriginHeadSHA_OmittedFromWire(t *testing.T) {
	p := planreview.ImplementReviewedPayload{
		ReviewerKind: "agent",
		Authority:    planreview.AuthorityAdvisory,
		Verdict:      planreview.VerdictApprove,
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), "origin") {
		t.Errorf("origin-free payload must omit the key (omitempty): %s", b)
	}
	if strings.Contains(string(b), "head_sha") {
		t.Errorf("head_sha-free payload must omit the key (omitempty): %s", b)
	}

	var got planreview.ImplementReviewedPayload
	if err := json.Unmarshal([]byte(`{"reviewer_kind":"agent","authority":"advisory","verdict":"approve"}`), &got); err != nil {
		t.Fatalf("Unmarshal pre-#1250 payload: %v", err)
	}
	if got.Origin != "" || got.HeadSHA != "" {
		t.Errorf("Origin=%q HeadSHA=%q, want both empty decoding a pre-#1250 payload", got.Origin, got.HeadSHA)
	}
}

// TestImplementReviewedPayload_SupplementalProvenance_RoundTrips pins that the
// base-rebase re-invoke supplemental verdict (#1250) carries BOTH provenance
// fields on the wire and round-trips — the binding-condition-1 idempotency key
// (stage_id, Origin, HeadSHA) depends on both surviving marshal/unmarshal.
func TestImplementReviewedPayload_SupplementalProvenance_RoundTrips(t *testing.T) {
	p := planreview.ImplementReviewedPayload{
		ReviewerKind: "agent",
		Authority:    planreview.AuthorityAdvisory,
		Verdict:      planreview.VerdictApprove,
		Origin:       planreview.OriginBaseRebaseReinvoke,
		HeadSHA:      "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"origin":"base_rebase_reinvoke"`) {
		t.Errorf("supplemental payload must carry origin: %s", b)
	}
	if !strings.Contains(string(b), `"head_sha":"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"`) {
		t.Errorf("supplemental payload must carry head_sha: %s", b)
	}

	var got planreview.ImplementReviewedPayload
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Origin != planreview.OriginBaseRebaseReinvoke {
		t.Errorf("Origin = %q, want %q", got.Origin, planreview.OriginBaseRebaseReinvoke)
	}
	if got.HeadSHA != p.HeadSHA {
		t.Errorf("HeadSHA = %q, want %q", got.HeadSHA, p.HeadSHA)
	}
}

// TestSettled pins the N-of-N verdicts-settled detection (#1023) for
// the configurations the dogfood loop runs: 1-of-1 (single reviewer)
// and 2-of-2 (heterogeneous dual review, live since 2026-06-09).
func TestSettled(t *testing.T) {
	cases := []struct {
		name                 string
		configured, terminal int
		want                 bool
	}{
		{"1-of-1 pending", 1, 0, false},
		{"1-of-1 settled", 1, 1, true},
		{"2-of-2 one landed", 2, 1, false},
		{"2-of-2 settled", 2, 2, true},
		{"zero configured never settles", 0, 0, false},
		{"zero configured ignores stray entries", 0, 3, false},
		{"extra terminal entries still settled", 2, 3, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := planreview.Settled(tc.configured, tc.terminal); got != tc.want {
				t.Errorf("Settled(%d, %d) = %v, want %v", tc.configured, tc.terminal, got, tc.want)
			}
		})
	}
}

// TestConcern_Provenance_JSONRoundTrip proves the server-internal Provenance
// marker (ADR-050 / E31.8 / #1613) survives the JSON round-trip through the
// stage_fixup_triggered audit payload when set, and — being json:omitempty — is
// absent from the wire and decodes to the zero value when unset, so an
// already-persisted concern (predating the field) renders on the unchanged
// trusted path.
func TestConcern_Provenance_JSONRoundTrip(t *testing.T) {
	t.Run("present when set", func(t *testing.T) {
		c := planreview.Concern{
			Severity:   planreview.SeverityHigh,
			Category:   "acceptance",
			Note:       "criterion failed",
			Provenance: planreview.ConcernProvenanceAcceptance,
		}
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if !strings.Contains(string(b), `"provenance":"acceptance"`) {
			t.Errorf("marshaled concern = %s, want it to carry \"provenance\":\"acceptance\"", b)
		}
		var got planreview.Concern
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if got.Provenance != planreview.ConcernProvenanceAcceptance {
			t.Errorf("round-tripped Provenance = %q, want %q", got.Provenance, planreview.ConcernProvenanceAcceptance)
		}
	})

	t.Run("omitted when empty", func(t *testing.T) {
		c := planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "n"}
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if strings.Contains(string(b), "provenance") {
			t.Errorf("marshaled concern = %s, want no \"provenance\" key (json:omitempty)", b)
		}
		var got planreview.Concern
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if got.Provenance != "" {
			t.Errorf("Provenance decoded to %q, want empty (trusted path)", got.Provenance)
		}
	})
}

// TestConcern_SettledRefNewEvidence_JSONRoundTrip pins the #1913 additive
// fields on the authoritative audit payload: because ImplementReviewedPayload
// embeds []Concern, settled_ref/new_evidence ride the payload for free via
// omitempty. A concern carrying both marshals them and round-trips; a concern
// without them emits neither key (byte-identical to a pre-#1913 payload) and
// decodes with both empty.
func TestConcern_SettledRefNewEvidence_JSONRoundTrip(t *testing.T) {
	t.Run("present on the payload when set", func(t *testing.T) {
		p := planreview.ImplementReviewedPayload{
			ReviewerKind: "agent",
			Verdict:      planreview.VerdictApproveWithConcerns,
			Concerns: []planreview.Concern{{
				Severity:    planreview.SeverityHigh,
				Category:    "correctness",
				Note:        "regressed",
				SettledRef:  "22222222-2222-2222-2222-222222222222",
				NewEvidence: "the fixup reverted the guard",
			}},
		}
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if !strings.Contains(string(b), `"settled_ref":"22222222-2222-2222-2222-222222222222"`) {
			t.Errorf("payload = %s, want it to carry settled_ref", b)
		}
		if !strings.Contains(string(b), `"new_evidence":"the fixup reverted the guard"`) {
			t.Errorf("payload = %s, want it to carry new_evidence", b)
		}
		var got planreview.ImplementReviewedPayload
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if len(got.Concerns) != 1 || got.Concerns[0].SettledRef != "22222222-2222-2222-2222-222222222222" ||
			got.Concerns[0].NewEvidence != "the fixup reverted the guard" {
			t.Errorf("round-tripped concern = %+v, want settled_ref/new_evidence preserved", got.Concerns)
		}
	})

	t.Run("omitted when empty", func(t *testing.T) {
		c := planreview.Concern{Severity: planreview.SeverityLow, Category: "scope", Note: "n"}
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if strings.Contains(string(b), "settled_ref") || strings.Contains(string(b), "new_evidence") {
			t.Errorf("marshaled concern = %s, want no settled_ref/new_evidence keys (json:omitempty)", b)
		}
	})
}

// TestVerdictSchema_OmitsProvenance is the binding guard for the plan's CRITICAL
// condition: the reviewer-facing verdict schema must NOT expose `provenance`, so
// a review agent cannot smuggle the server-internal trust marker in through the
// closed (additionalProperties:false) verdict schema. If VerdictSchema() were
// reflection-generated from the Concern struct this assertion would fail and the
// field would have to be excluded explicitly.
func TestVerdictSchema_OmitsProvenance(t *testing.T) {
	schema := planreview.VerdictSchema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no properties object")
	}
	concerns, ok := props["concerns"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no concerns property")
	}
	items, ok := concerns["items"].(map[string]any)
	if !ok {
		t.Fatalf("concerns has no items object")
	}
	concernProps, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("concerns.items has no properties object")
	}
	if _, exists := concernProps["provenance"]; exists {
		t.Errorf("VerdictSchema() concern properties include \"provenance\"; it MUST stay server-internal so a reviewer cannot populate it")
	}
}
