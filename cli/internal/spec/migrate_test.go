package spec

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// updateGolden regenerates the committed v2 golden fixture from the v1
// one: `go test ./internal/spec -run TestMigrate_OwnSpecGolden
// -update-golden`. The golden is the codemod's done-means proof, so it is
// regenerated deliberately and reviewed in the diff, never recomputed at
// assert time.
var updateGolden = flag.Bool("update-golden", false, "rewrite the committed v2 golden fixture from the v1 fixture")

const (
	goldenV1 = "testdata/migrate/fishhawk-own-spec.v1.yaml"
	goldenV2 = "testdata/migrate/fishhawk-own-spec.v2.yaml"
)

// minimalV1 is the smallest schema-valid v1 document the translation
// tests build on. Each test rewrites the parts it exercises.
const minimalV1 = `version: "1.6"
roles:
  founder:
    members: ["@kuhlman-labs"]
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
        gates:
          - type: approval
            approvers:
              any_of: [founder]
`

func migrateOK(t *testing.T, src string) *MigrationResult {
	t.Helper()
	res, err := MigrateBytes([]byte(src))
	if err != nil {
		t.Fatalf("MigrateBytes: unexpected error: %v", err)
	}
	if res.Refused() {
		t.Fatalf("MigrateBytes refused unexpectedly: %v", res.Refusals)
	}
	if len(res.Migrated) == 0 {
		t.Fatal("MigrateBytes returned no bytes on a clean migration")
	}
	return res
}

// migrateRefused runs MigrateBytes and asserts the named branch fired AND
// that NO bytes were produced — the invariant every refusal shares.
func migrateRefused(t *testing.T, src, wantBranch string) Refusal {
	t.Helper()
	res, err := MigrateBytes([]byte(src))
	if err != nil {
		t.Fatalf("MigrateBytes: unexpected error: %v", err)
	}
	if !res.Refused() {
		t.Fatalf("expected refusal %s, got a clean migration:\n%s", wantBranch, res.Migrated)
	}
	if len(res.Migrated) != 0 {
		t.Fatalf("refusal %s produced %d bytes; a refusal must write nothing", wantBranch, len(res.Migrated))
	}
	if res.Report == nil {
		t.Fatalf("refusal %s produced no eligibility report; the report is emitted on every path", wantBranch)
	}
	for _, r := range res.Refusals {
		if r.Branch == wantBranch {
			return r
		}
	}
	t.Fatalf("expected refusal branch %s, got %v", wantBranch, res.Refusals)
	return Refusal{}
}

// ---------------------------------------------------------------------
// Translation table: one case per E52.2-E52.10 grammar change. Each
// asserts the emitted YAML TEXT, not merely that the output validates, so
// a no-op or comment-only edit to migrate.go fails here.
// ---------------------------------------------------------------------

func TestMigrateBytes_Translations(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		want    []string
		notWant []string
	}{
		{
			name:    "version collapses to the single token 2",
			src:     minimalV1,
			want:    []string{`version: "2"`},
			notWant: []string{`version: "1.6"`, `version: "2.0"`},
		},
		{
			name: "drive renames to auto_advance",
			src:  strings.Replace(minimalV1, "  feature_change:\n", "  feature_change:\n    drive: true\n", 1),
			want: []string{"auto_advance: true"},
			// `drive:` must be gone, but "auto_advance" contains no
			// "drive" substring, so a plain check is unambiguous.
			notWant: []string{"drive:"},
		},
		{
			name: "budget max_runtime_minutes becomes a duration and max_tokens survives",
			src: strings.Replace(minimalV1, "        produces:\n",
				"        budget:\n          max_tokens: 200000\n          max_runtime_minutes: 15\n          enforcement: advisory\n        produces:\n", 1),
			want:    []string{"max_runtime: 15m", "max_tokens: 200000", "enforcement: advisory"},
			notWant: []string{"max_runtime_minutes"},
		},
		{
			name: "constraints list folds into one object preserving order and comments",
			src: strings.Replace(minimalV1, "        produces:\n          - artifact: plan\n",
				"        produces:\n          - artifact: pull_request\n        constraints:\n"+
					"          # raised 30 -> 45\n          - max_files_changed: 45\n"+
					"          - forbidden_paths: [\".github/workflows/**\"]\n", 1),
			want: []string{
				"constraints:\n          # raised 30 -> 45\n          max_files_changed: 45",
				"forbidden_paths:",
			},
			notWant: []string{"- max_files_changed"},
		},
		{
			name: "must_page_human becomes page_human_on with reviewer_reject rewritten",
			src: strings.Replace(minimalV1, "  feature_change:\n",
				"  feature_change:\n    operator_agent:\n      must_page_human:\n        - reviewer_reject\n        - plan_rejection\n", 1),
			want:    []string{"page_human_on:", "- gating_reviewer_reject", "- plan_rejection"},
			notWant: []string{"must_page_human", "- reviewer_reject", "operator_agent"},
		},
		{
			name:    "roles plus any_of becomes approvals count 1 over the union",
			src:     minimalV1,
			want:    []string{"approvals:", "count: 1", "members: [kuhlman-labs]"},
			notWant: []string{"approvers:", "any_of", "roles:", "@kuhlman-labs"},
		},
		{
			name: "all_of over three single-member roles gives count 3 over the union",
			src: strings.NewReplacer(
				"  founder:\n    members: [\"@kuhlman-labs\"]\n",
				"  a:\n    members: [\"@alice\"]\n  b:\n    members: [\"@bob\"]\n  c:\n    members: [\"@carol\"]\n",
				"              any_of: [founder]", "              all_of: [a, b, c]",
			).Replace(minimalV1),
			want: []string{"count: 3", "members: [alice, bob, carol]"},
		},
		{
			name: "operator_agent matching a tier exactly collapses to the shorthand",
			src: strings.Replace(minimalV1, "  feature_change:\n",
				"  feature_change:\n    operator_agent:\n      may_approve: clean_dual_approval\n"+
					"      may_route_fixup: convergent_concerns\n      may_retry: infra_flake\n"+
					"      must_page_human:\n        - reviewer_reject\n        - plan_rejection\n        - scope_amendment\n"+
					"        - budget_override\n        - policy_override\n        - exception_request\n        - requirement_arbitration\n", 1),
			want:    []string{"autonomy: medium"},
			notWant: []string{"actions:", "operator_agent", "may_approve"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(migrateOK(t, tc.src).Migrated)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("migrated output is missing %q\n---\n%s", w, got)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(got, nw) {
					t.Errorf("migrated output still carries %q\n---\n%s", nw, got)
				}
			}
		})
	}
}

// TestMigrateBytes_AllOfCountsDistinctMembers pins all three shapes of the
// all_of cardinality rule. `count` is the number of DISTINCT members in
// the deduplicated union, NOT the number of named roles: two singleton
// roles naming the SAME person are one human, and emitting count 2 over a
// one-member list would ship a gate that can never clear — which nothing
// downstream would catch, because v2's approvals predicate requires only
// that count be an integer >= 1 and never cross-checks it against
// members.
func TestMigrateBytes_AllOfCountsDistinctMembers(t *testing.T) {
	tests := []struct {
		name        string
		roles       string
		allOf       string
		wantCount   string
		wantMembers string
	}{
		{
			name:        "disjoint singletons still yield count = N",
			roles:       "  a:\n    members: [\"@alice\"]\n  b:\n    members: [\"@bob\"]\n",
			allOf:       "[a, b]",
			wantCount:   "count: 2",
			wantMembers: "members: [alice, bob]",
		},
		{
			name:        "two singleton roles naming the same person collapse to count 1",
			roles:       "  a:\n    members: [\"@alice\"]\n  b:\n    members: [\"@alice\"]\n",
			allOf:       "[a, b]",
			wantCount:   "count: 1",
			wantMembers: "members: [alice]",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.NewReplacer(
				"  founder:\n    members: [\"@kuhlman-labs\"]\n", tc.roles,
				"              any_of: [founder]", "              all_of: "+tc.allOf,
			).Replace(minimalV1)
			got := string(migrateOK(t, src).Migrated)
			if !strings.Contains(got, tc.wantCount) {
				t.Errorf("want %q in:\n%s", tc.wantCount, got)
			}
			if !strings.Contains(got, tc.wantMembers) {
				t.Errorf("want %q in:\n%s", tc.wantMembers, got)
			}
		})
	}
}

// TestTranslateApprovers_RepeatedRoleReference is the third all_of shape
// CONDITION 1 names: `all_of: [founder, founder]` must yield count 1, not
// count 2 over a one-member list.
//
// It is asserted at the translator because the shape is unreachable
// through MigrateBytes: v1's approvers.all_of declares uniqueItems, so
// the R0 source gate rejects a repeated reference before translation. The
// dedup that makes the shape safe still has to hold — an all_of whose
// roles OVERLAP reaches the same code path and IS reachable (the case
// above).
func TestTranslateApprovers_RepeatedRoleReference(t *testing.T) {
	var approvers yaml.Node
	if err := yaml.Unmarshal([]byte("all_of: [founder, founder]\n"), &approvers); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	roles := map[string][]string{"founder": {"kuhlman-labs"}}
	pred, before, ref := translateApprovers(documentRoot(&approvers), roles, "/g")
	if ref != nil {
		t.Fatalf("a repeated singleton reference must translate, got refusal %v", ref)
	}
	if pred.Count != 1 {
		t.Errorf("count = %d, want 1 (the number of DISTINCT members, not of named roles)", pred.Count)
	}
	if len(pred.Members) != 1 || pred.Members[0] != "kuhlman-labs" {
		t.Errorf("members = %v, want [kuhlman-labs]", pred.Members)
	}
	if !strings.Contains(before, "all_of") {
		t.Errorf("the report's before-rendering must name the source form: %s", before)
	}
}

// TestMigrateBytes_TierRounding pins the no-rounding rule: only an EXACT
// match across all five classes AND the page list collapses to a tier.
func TestMigrateBytes_TierRounding(t *testing.T) {
	tests := []struct {
		name    string
		block   string
		want    []string
		notWant []string
	}{
		{
			name:    "a may_approve-only block emits the explicit matrix and no autonomy key",
			block:   "    operator_agent:\n      may_approve: clean_dual_approval\n",
			want:    []string{"actions:", "approve:", "mode: auto", "when: clean_dual_approval"},
			notWant: []string{"autonomy:", "fixup:", "retry:"},
		},
		{
			name:    "an absent operator_agent emits neither key",
			block:   "",
			notWant: []string{"autonomy:", "actions:"},
		},
		{
			name: "route_fixup_min_severity blocks the tier collapse and rides on the fixup class",
			block: "    operator_agent:\n      may_approve: clean_dual_approval\n      may_route_fixup: convergent_concerns\n" +
				"      route_fixup_min_severity: low\n      may_retry: infra_flake\n" +
				"      must_page_human:\n        - reviewer_reject\n        - plan_rejection\n        - scope_amendment\n" +
				"        - budget_override\n        - policy_override\n        - exception_request\n        - requirement_arbitration\n",
			want:    []string{"actions:", "min_severity: low"},
			notWant: []string{"autonomy:"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(minimalV1, "  feature_change:\n", "  feature_change:\n"+tc.block, 1)
			got := string(migrateOK(t, src).Migrated)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("want %q in:\n%s", w, got)
				}
			}
			for _, nw := range tc.notWant {
				if strings.Contains(got, nw) {
					t.Errorf("did not want %q in:\n%s", nw, got)
				}
			}
		})
	}
}

// TestMigrateBytes_NoFabrication is the negative-fabrication assertion:
// limit_usd, min_permission, member_of and not: are never invented. There
// is no token-to-USD rate anywhere in this repo, and the legacy approvers
// form carries no source for the three predicate keys.
func TestMigrateBytes_NoFabrication(t *testing.T) {
	src := strings.Replace(minimalV1, "        produces:\n",
		"        budget:\n          max_tokens: 200000\n          max_runtime_minutes: 15\n        produces:\n", 1)
	got := string(migrateOK(t, src).Migrated)
	for _, key := range []string{"limit_usd", "min_permission", "member_of", "not:"} {
		if strings.Contains(got, key) {
			t.Errorf("migrated output fabricated %q, which the source never licensed:\n%s", key, got)
		}
	}
}

// TestMigrateBytes_MinimalGateValidates pins that a migrated gate
// declaring only count + members passes ValidateBytes. min_permission and
// member_of are annotated x-intended-required in the v2 schema but were
// deliberately NOT promoted; this test fails if that promotion ever lands
// without the codemod being updated.
func TestMigrateBytes_MinimalGateValidates(t *testing.T) {
	out := migrateOK(t, minimalV1).Migrated
	if err := ValidateBytes(out); err != nil {
		t.Fatalf("a migrated count+members gate no longer validates: %v", err)
	}
}

// TestMigrateBytes_AlreadyV2IsNoOp: nothing to migrate is not an error.
func TestMigrateBytes_AlreadyV2IsNoOp(t *testing.T) {
	src, err := os.ReadFile(goldenV2)
	if err != nil {
		t.Fatalf("read v2 golden: %v", err)
	}
	res, err := MigrateBytes(src)
	if err != nil {
		t.Fatalf("MigrateBytes: %v", err)
	}
	if !res.NoOp {
		t.Fatal("an already-v2 document must report NoOp")
	}
	if res.Refused() || len(res.Migrated) != 0 {
		t.Fatalf("a no-op must produce no refusals and no bytes; got %v / %d bytes", res.Refusals, len(res.Migrated))
	}
	if res.Report == nil || len(res.Report.Gates) == 0 {
		t.Fatal("the no-op path must still emit the eligibility report")
	}
}

func TestMigrateBytes_ParseErrors(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"empty", "   \n"},
		{"not yaml", "version: \"1.6\"\n\tbad: [\n"},
		{"not a mapping", "- a\n- b\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := MigrateBytes([]byte(tc.src)); err == nil {
				t.Fatal("expected a *ParseError")
			} else if _, ok := err.(*ParseError); !ok {
				t.Fatalf("expected *ParseError, got %T: %v", err, err)
			}
		})
	}
}

// ---------------------------------------------------------------------
// The refusal taxonomy: one behavioural test per named branch. Each
// asserts the branch fires, names the offending construct, and produces
// NO bytes (migrateRefused checks the last two for every case).
// ---------------------------------------------------------------------

func TestMigrateBytes_R0_InvalidSource(t *testing.T) {
	// A v1 document with an unknown stage type: schema-invalid, so
	// migrating it would produce garbage.
	src := strings.Replace(minimalV1, "        type: plan\n", "        type: teleport\n", 1)
	r := migrateRefused(t, src, RefusalSourceInvalid)
	if !strings.Contains(r.Message, "does not validate") {
		t.Errorf("R0 message should name the source-validation failure: %s", r.Message)
	}
}

func TestMigrateBytes_R1_AllOfMultiMemberRole(t *testing.T) {
	src := strings.NewReplacer(
		"  founder:\n    members: [\"@kuhlman-labs\"]\n",
		"  role_a:\n    members: [\"@x\", \"@y\"]\n  role_b:\n    members: [\"@z\"]\n",
		"              any_of: [founder]", "              all_of: [role_a, role_b]",
	).Replace(minimalV1)
	r := migrateRefused(t, src, RefusalAllOfMultiMemberRole)
	if !strings.Contains(r.Message, `"role_a"`) || !strings.Contains(r.Message, "2 members") {
		t.Errorf("R1 must name the offending role and its member count: %s", r.Message)
	}
}

// TestMigrateBytes_R1_SingleMemberControl is R1's passing control: the
// faithful case must NOT be over-refused.
func TestMigrateBytes_R1_SingleMemberControl(t *testing.T) {
	src := strings.NewReplacer(
		"  founder:\n    members: [\"@kuhlman-labs\"]\n",
		"  role_a:\n    members: [\"@x\"]\n  role_b:\n    members: [\"@z\"]\n",
		"              any_of: [founder]", "              all_of: [role_a, role_b]",
	).Replace(minimalV1)
	got := string(migrateOK(t, src).Migrated)
	if !strings.Contains(got, "count: 2") || !strings.Contains(got, "members: [x, z]") {
		t.Errorf("the all-single-member case must translate faithfully:\n%s", got)
	}
}

func TestMigrateBytes_R2_UndefinedRole(t *testing.T) {
	src := strings.Replace(minimalV1, "              any_of: [founder]", "              any_of: [reviewers]", 1)
	r := migrateRefused(t, src, RefusalUndefinedRole)
	if !strings.Contains(r.Message, `"reviewers"`) {
		t.Errorf("R2 must name the undefined role: %s", r.Message)
	}
}

func TestMigrateBytes_R3_TeamMemberRef(t *testing.T) {
	tests := []struct{ name, members string }{
		{"a pure team ref", `["@acme/reviewers"]`},
		{"a role mixing a user and a team", `["@kuhlman-labs", "@acme/reviewers"]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := strings.Replace(minimalV1, `    members: ["@kuhlman-labs"]`, "    members: "+tc.members, 1)
			r := migrateRefused(t, src, RefusalTeamMemberRef)
			if !strings.Contains(r.Message, "@acme/reviewers") {
				t.Errorf("R3 must name the offending team ref: %s", r.Message)
			}
		})
	}
}

func TestMigrateBytes_R4_ReviewersAgentCount(t *testing.T) {
	src := strings.Replace(minimalV1, "        produces:\n", "        reviewers:\n          agent: 2\n          human: 1\n        produces:\n", 1)
	r := migrateRefused(t, src, RefusalReviewersAgentCount)
	if !strings.Contains(r.Message, `"plan"`) || !strings.Contains(r.Message, "provider") {
		t.Errorf("R4 must name the stage and point at explicit providers: %s", r.Message)
	}
}

func TestMigrateBytes_R5_DuplicateConstraintKind(t *testing.T) {
	src := strings.Replace(minimalV1, "        produces:\n          - artifact: plan\n",
		"        produces:\n          - artifact: pull_request\n        constraints:\n"+
			"          - max_files_changed: 45\n          - max_files_changed: 20\n", 1)
	r := migrateRefused(t, src, RefusalDuplicateConstraint)
	if !strings.Contains(r.Message, "max_files_changed") {
		t.Errorf("R5 must name the duplicated kind: %s", r.Message)
	}
}

// TestTranslatePageEvent_R6 covers the out-of-enum page-token branch.
//
// It is asserted at the helper rather than through MigrateBytes because
// the branch is UNREACHABLE from a schema-valid v1 source by
// construction: v1's must_page_human enum is v2's page_human_on enum plus
// the bare reviewer_reject token, which this function rewrites, so the R0
// source gate rejects every token that would reach it. The guard still
// has to hold if either enum ever diverges — which is what this asserts.
func TestTranslatePageEvent_R6(t *testing.T) {
	if got, ok := translatePageEvent(legacyReviewerRejectToken); !ok || got != gatingReviewerRejectToken {
		t.Errorf("the bare reviewer_reject token must rewrite to the gating spelling, got %q ok=%v", got, ok)
	}
	if got, ok := translatePageEvent("plan_rejection"); !ok || got != "plan_rejection" {
		t.Errorf("an in-enum token must pass through, got %q ok=%v", got, ok)
	}
	if _, ok := translatePageEvent("teleport_requested"); ok {
		t.Error("an out-of-enum page token must be rejected")
	}

	// And the refusal the rejection produces, driven through the real
	// operator_agent translator.
	var oa yaml.Node
	if err := yaml.Unmarshal([]byte("must_page_human: [teleport_requested]\n"), &oa); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	_, refusals := translateOperatorAgent(documentRoot(&oa), "/workflows/w/operator_agent")
	if len(refusals) != 1 || refusals[0].Branch != RefusalUnknownPageEvent {
		t.Fatalf("expected one %s refusal, got %v", RefusalUnknownPageEvent, refusals)
	}
	if !strings.Contains(refusals[0].Message, "teleport_requested") {
		t.Errorf("R6 must name the offending token: %s", refusals[0].Message)
	}
}

// TestMigrateBytes_R7_OutputFailsValidation drives the fail-closed output
// gate through the real MigrateBytes using the test-only postTranslateHook.
//
// R7 is unreachable from any schema-valid v1 source — that is the point
// of the codemod being correct — so the ONLY way to prove the last line
// of defense actually holds is to inject a corruption after translation.
// Without this the gate would be prose.
func TestMigrateBytes_R7_OutputFailsValidation(t *testing.T) {
	postTranslateHook = func(root *yaml.Node) {
		// Re-introduce the removed top-level roles map: v2's root is
		// additionalProperties:false, so the output must be rejected.
		root.Content = append(root.Content, scalar("roles"), &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
	}
	t.Cleanup(func() { postTranslateHook = nil })

	r := migrateRefused(t, minimalV1, RefusalOutputInvalid)
	if !strings.Contains(r.Message, "nothing was written") {
		t.Errorf("R7 must state that nothing was written: %s", r.Message)
	}
	if !strings.Contains(r.Message, "roles") {
		t.Errorf("R7 must carry the schema errors: %s", r.Message)
	}
}

// TestTranslateOperatorAgent_R8 covers the unknown-knob / unknown-condition
// / unknown-model_policy-field branch.
//
// Like R6 this is unreachable through MigrateBytes: v1's operator_agent is
// additionalProperties:false with a singleton value enum per may_* knob,
// and v1's model_policy property set is identical to v2's, so the R0 gate
// rejects every input that would reach it. The guard is asserted at the
// translator, which is where it lives.
func TestTranslateOperatorAgent_R8(t *testing.T) {
	tests := []struct {
		name, src, wantIn string
	}{
		{"an unknown knob", "may_teleport: whenever\n", "may_teleport"},
		{"a condition the class is not bound to", "may_approve: solo_low\n", "clean_dual_approval"},
		{"an unknown model_policy field", "model_policy:\n  strategy: explicit_defaults\n  temperature: 0.4\n", "temperature"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var oa yaml.Node
			if err := yaml.Unmarshal([]byte(tc.src), &oa); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			block, refusals := translateOperatorAgent(documentRoot(&oa), "/workflows/w/operator_agent")
			if block != nil {
				t.Error("a refusing translation must return no block")
			}
			if len(refusals) == 0 || refusals[0].Branch != RefusalUnknownDelegationKnob {
				t.Fatalf("expected an %s refusal, got %v", RefusalUnknownDelegationKnob, refusals)
			}
			if !strings.Contains(refusals[0].Message, tc.wantIn) {
				t.Errorf("R8 must name the offending construct (%s): %s", tc.wantIn, refusals[0].Message)
			}
		})
	}
}

func TestMigrateBytes_R9_VersionMajorZero(t *testing.T) {
	src := strings.Replace(minimalV1, `version: "1.6"`, `version: "0.7"`, 1)
	r := migrateRefused(t, src, RefusalSourceVersionMajor0)
	if !strings.Contains(r.Message, "version major 1 only") {
		t.Errorf("R9 must state that only major 1 is accepted: %s", r.Message)
	}
	// The v0 branch must fire on its OWN name, not collapse into R0: a
	// perfectly VALID v0 document is still refused, because what parses
	// and what is faithfully translatable are different questions.
	if err := ValidateBytes([]byte(src)); err != nil {
		t.Fatalf("fixture should be a VALID v0 document so the refusal is provably not R0: %v", err)
	}
}

// ---------------------------------------------------------------------
// The golden round-trip: this repo's own 360-line commented spec.
// ---------------------------------------------------------------------

// TestMigrate_OwnSpecGolden is the done-means test. The v1 fixture is a
// FROZEN byte snapshot of this repo's governing spec at the time this
// landed — deliberately NOT tied to the live file, which the out-of-band
// operator migration will rewrite to v2. It proves the codemod's behavior
// on a realistic, heavily commented document; it is not a mirror.
//
// It is also the comment/ordering-preservation proof: a node edit that
// drops or relocates a comment shows up as a byte diff here.
func TestMigrate_OwnSpecGolden(t *testing.T) {
	src, err := os.ReadFile(goldenV1)
	if err != nil {
		t.Fatalf("read v1 golden: %v", err)
	}
	res := migrateOK(t, string(src))

	if *updateGolden {
		if err := os.WriteFile(goldenV2, res.Migrated, 0o600); err != nil {
			t.Fatalf("write v2 golden: %v", err)
		}
		t.Log("wrote " + goldenV2)
		return
	}

	want, err := os.ReadFile(goldenV2)
	if err != nil {
		t.Fatalf("read v2 golden: %v", err)
	}
	if string(res.Migrated) != string(want) {
		t.Errorf("migrated output does not match the committed v2 golden.\n--- got ---\n%s", res.Migrated)
	}
	if err := ValidateBytes(want); err != nil {
		t.Fatalf("the committed v2 golden does not validate: %v", err)
	}
}

// TestMigrate_OwnSpecSemantics pins the four workflows' translations by
// meaning rather than by bytes, so a golden regeneration that quietly
// changed one of them fails here too.
func TestMigrate_OwnSpecSemantics(t *testing.T) {
	src, err := os.ReadFile(goldenV1)
	if err != nil {
		t.Fatalf("read v1 golden: %v", err)
	}
	out := string(migrateOK(t, string(src)).Migrated)

	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("migrated output is not YAML: %v", err)
	}
	if doc["version"] != "2" {
		t.Errorf("version = %v, want \"2\"", doc["version"])
	}
	if _, ok := doc["roles"]; ok {
		t.Error("the top-level roles map must be deleted")
	}
	wfs, _ := doc["workflows"].(map[string]any)
	fc, _ := wfs["feature_change"].(map[string]any)

	// feature_change's operator_agent matches the medium tier exactly.
	if fc["autonomy"] != "medium" {
		t.Errorf("feature_change autonomy = %v, want medium", fc["autonomy"])
	}
	if _, ok := fc["actions"]; ok {
		t.Error("an exact tier match must not also emit an actions matrix")
	}
	if fc["auto_advance"] != true {
		t.Errorf("feature_change auto_advance = %v, want true", fc["auto_advance"])
	}

	// release keeps auto_advance and emits NO actions block: it declares
	// no operator_agent, and absence is faithful — it is deliberately not
	// rounded to `autonomy: low`.
	rel, _ := wfs["release"].(map[string]any)
	if rel["auto_advance"] != true {
		t.Errorf("release auto_advance = %v, want true", rel["auto_advance"])
	}
	if _, ok := rel["actions"]; ok {
		t.Error("release declares no operator_agent, so it must emit no actions block")
	}
	if _, ok := rel["autonomy"]; ok {
		t.Error("an absent operator_agent must NOT be rounded to autonomy: low")
	}

	// Every any_of: [founder] gate resolves to the same single member.
	if n := strings.Count(out, "members: [kuhlman-labs]"); n != 4 {
		t.Errorf("want 4 migrated approvals gates, got %d", n)
	}
	// The three max_runtime_minutes values become durations.
	for _, want := range []string{"max_runtime: 15m", "max_runtime: 30m", "max_runtime: 20m"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in the migrated own-spec", want)
		}
	}
	// The acceptance stage's egress block is untouched.
	if !strings.Contains(out, "target_hosts:") || !strings.Contains(out, "- localhost:8090") {
		t.Error("the acceptance stage's egress block must survive untouched")
	}
	// Three constraints lists became objects.
	if strings.Contains(out, "          - max_files_changed") || strings.Contains(out, "          - allowed_environments") {
		t.Error("a constraints list survived the fold")
	}
}

// TestMigrate_GoldenFixturePathsExist guards the fixture pair against a
// rename that would leave the golden tests silently reading nothing.
func TestMigrate_GoldenFixturePathsExist(t *testing.T) {
	for _, p := range []string{goldenV1, goldenV2} {
		if _, err := os.Stat(filepath.Clean(p)); err != nil {
			t.Errorf("fixture %s is missing: %v", p, err)
		}
	}
}
