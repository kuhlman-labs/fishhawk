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
		ReviewerKind:  "agent",
		ReviewerModel: "claude-opus-4-7",
		Authority:     planreview.AuthorityGating,
		Verdict:       planreview.VerdictApprove,
		Concerns:      nil,
		FreeForm:      "plan is sound",
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
		FreeForm: "diff implements the plan",
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
