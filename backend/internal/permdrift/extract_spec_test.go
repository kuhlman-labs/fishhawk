package permdrift

import (
	"reflect"
	"strings"
	"testing"
)

// specTemplate is a minimal valid workflow-v2 document; specDoc substitutes
// its %TOKENS%. Defaults reproduce the base fixture every case diffs from.
const specTemplate = `version: "2"
workflows:
  feature_change:
%AUTONOMY%
%ESCALATIONS%
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvals:
              count: 1
%GATE%
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
        constraints:
          forbidden_paths: %FORBIDDEN%
%PERMISSIONS%
`

var specDefaults = map[string]string{
	"%AUTONOMY%": "    autonomy: high",
	"%ESCALATIONS%": `    escalations:
      - match:
          paths: ["a/**", "b/**"]
        require:
          max_autonomy: low`,
	"%GATE%":      "",
	"%FORBIDDEN%": `[".github/workflows/**", ".fishhawk/**"]`,
	"%PERMISSIONS%": `        permissions:
          network:
            target_hosts: ["api.example.com"]
          write: ["src/**"]
          shell: restricted`,
}

// specDoc renders specTemplate with the defaults, overridden by kv pairs.
func specDoc(kv ...string) string {
	vals := map[string]string{}
	for k, v := range specDefaults {
		vals[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		vals[kv[i]] = kv[i+1]
	}
	out := specTemplate
	for k, v := range vals {
		out = strings.ReplaceAll(out, k, v)
	}
	return out
}

type specExtractor func([]byte) (Grants, error)

func diffSpec(t *testing.T, ex specExtractor, base, head string) []Change {
	t.Helper()
	b, err := ex([]byte(base))
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	h, err := ex([]byte(head))
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	return Compare(b, h)
}

const (
	fpKey   = "workflows.feature_change.stages.implement.forbidden_paths"
	escKey  = `workflows.feature_change.escalations[{"paths":["a/**","b/**"]}]`
	implKey = "workflows.feature_change.stages.implement"
)

// TestExtractSpec covers the four workflows.yaml extractors.
//
// COUNTERFACTUAL (forbidden_paths keyed by VALUE): key the forbidden_paths
// entries by list INDEX instead of glob value — the "reordered forbidden_paths"
// row swaps [.github/workflows/**, .fishhawk/**] to the reverse order, so
// index keys [0] and [1] would each carry a different glob on each side and
// the zero-change assertion goes RED.
func TestExtractSpec(t *testing.T) {
	base := specDoc()
	cases := []struct {
		name string
		ex   specExtractor
		head string
		want []Change
	}{
		{"forbidden_paths entry removed", ExtractSpecForbiddenPaths,
			specDoc("%FORBIDDEN%", `[".github/workflows/**"]`),
			[]Change{{Key: fpKey + "[.fishhawk/**]", Before: Present, After: Absent, Direction: Widened}}},
		{"reordered forbidden_paths", ExtractSpecForbiddenPaths,
			specDoc("%FORBIDDEN%", `[".fishhawk/**", ".github/workflows/**"]`), nil},
		{"forbidden_paths entry added is a narrowing", ExtractSpecForbiddenPaths,
			specDoc("%FORBIDDEN%", `[".github/workflows/**", ".fishhawk/**", "LICENSE"]`),
			[]Change{{Key: fpKey + "[LICENSE]", Before: Absent, After: Present, Direction: Narrowed}}},
		{"escalation max_autonomy raised", ExtractSpecEscalations,
			specDoc("%ESCALATIONS%", "    escalations:\n      - match:\n          paths: [\"a/**\", \"b/**\"]\n        require:\n          max_autonomy: medium"),
			[]Change{{Key: escKey + ".max_autonomy", Before: "low", After: "medium", Direction: Widened}}},
		{"escalation removed", ExtractSpecEscalations, specDoc("%ESCALATIONS%", ""),
			[]Change{
				{Key: escKey, Before: Present, After: Absent, Direction: Widened},
				{Key: escKey + ".max_autonomy", Before: "low", After: Absent, Direction: Widened},
			}},
		{"reordered match.paths", ExtractSpecEscalations,
			specDoc("%ESCALATIONS%", "    escalations:\n      - match:\n          paths: [\"b/**\", \"a/**\"]\n        require:\n          max_autonomy: low"), nil},
		{"egress host added", ExtractSpecStagePermissions,
			specDoc("%PERMISSIONS%", "        permissions:\n          network:\n            target_hosts: [\"api.example.com\", \"evil.example.net\"]\n          write: [\"src/**\"]\n          shell: restricted"),
			[]Change{{Key: implKey + ".egress[evil.example.net]", Before: Absent, After: Present, Direction: Widened}}},
		{"egress via the legacy egress key is the same grant", ExtractSpecStagePermissions,
			specDoc("%PERMISSIONS%", "        egress:\n          target_hosts: [\"api.example.com\"]\n        permissions:\n          write: [\"src/**\"]\n          shell: restricted"), nil},
		{"shell posture raised", ExtractSpecStagePermissions,
			specDoc("%PERMISSIONS%", "        permissions:\n          network:\n            target_hosts: [\"api.example.com\"]\n          write: [\"src/**\"]\n          shell: unrestricted"),
			[]Change{{Key: implKey + ".permissions.shell", Before: "restricted", After: "unrestricted", Direction: Widened}}},
		{"write glob added", ExtractSpecStagePermissions,
			specDoc("%PERMISSIONS%", "        permissions:\n          network:\n            target_hosts: [\"api.example.com\"]\n          write: [\"src/**\", \"**\"]\n          shell: restricted"),
			[]Change{{Key: implKey + ".permissions.write[**]", Before: Absent, After: Present, Direction: Widened}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := diffSpec(t, c.ex, base, c.head); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("changes =\n  %+v\nwant\n  %+v", got, c.want)
			}
		})
	}
}

// TestExtractSpecAutonomy pins that the extractor reads the RESOLVED value
// spec.ParseBytes produces and never invents a rank for an absent
// declaration (approval condition 4).
func TestExtractSpecAutonomy(t *testing.T) {
	const wf = "workflows.feature_change"
	gate0 := wf + ".stages.plan.gates[0].actions."

	t.Run("raised tier widens the tier and every newly delegated class", func(t *testing.T) {
		got := diffSpec(t, ExtractSpecAutonomy, specDoc("%AUTONOMY%", "    autonomy: medium", "%ESCALATIONS%", ""), specDoc("%ESCALATIONS%", ""))
		want := []Change{
			{Key: wf + ".actions.merge", Before: Absent, After: "auto", Direction: Widened},
			{Key: wf + ".actions.waive", Before: Absent, After: "auto", Direction: Widened},
			{Key: wf + ".autonomy", Before: "medium", After: "high", Direction: Widened},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("changes =\n  %+v\nwant\n  %+v", got, want)
		}
	})

	// COUNTERFACTUAL (never invent an absent rank): emit a `low` tier entry
	// when wf.Autonomy is empty — this fixture declares NO autonomy and NO
	// actions, so any `.autonomy` or `.actions.` key in the extracted set is
	// the invented entry and the zero-entry assertion goes RED.
	t.Run("absent autonomy yields no entry", func(t *testing.T) {
		g, err := ExtractSpecAutonomy([]byte(specDoc("%AUTONOMY%", "", "%ESCALATIONS%", "")))
		if err != nil {
			t.Fatal(err)
		}
		if len(g) != 0 {
			t.Fatalf("extracted %+v from a workflow declaring no autonomy; want none", g)
		}
	})

	t.Run("resolved mode is read: an explicit tightening of a tier class", func(t *testing.T) {
		tight := specDoc("%AUTONOMY%", "    autonomy: high\n    actions:\n      merge:\n        mode: gated", "%ESCALATIONS%", "")
		g, err := ExtractSpecAutonomy([]byte(tight))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := g[wf+".actions.merge"]; ok {
			t.Fatalf("merge is gated by an explicit override but was extracted as auto: %+v", g)
		}
		got := diffSpec(t, ExtractSpecAutonomy, tight, specDoc("%ESCALATIONS%", ""))
		want := []Change{{Key: wf + ".actions.merge", Before: Absent, After: "auto", Direction: Widened}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("removing the tightening: changes =\n  %+v\nwant\n  %+v", got, want)
		}
	})

	t.Run("removing a restricting gate block widens that gate", func(t *testing.T) {
		withGate := specDoc("%ESCALATIONS%", "", "%GATE%", "            autonomy: low")
		got := diffSpec(t, ExtractSpecAutonomy, withGate, specDoc("%ESCALATIONS%", ""))
		var keys []string
		for _, c := range got {
			if c.Direction != Widened {
				t.Errorf("unexpected %s change %+v", c.Direction, c)
			}
			keys = append(keys, c.Key)
		}
		want := []string{gate0 + "approve", gate0 + "fixup", gate0 + "merge", gate0 + "retry", gate0 + "waive"}
		if !reflect.DeepEqual(keys, want) {
			t.Fatalf("widened keys = %v, want %v", keys, want)
		}
	})

	t.Run("a gate granting beyond the workflow is a gate-scoped widening", func(t *testing.T) {
		low := specDoc("%AUTONOMY%", "    autonomy: low", "%ESCALATIONS%", "")
		got := diffSpec(t, ExtractSpecAutonomy, low, specDoc("%AUTONOMY%", "    autonomy: low", "%ESCALATIONS%", "", "%GATE%",
			"            actions:\n              merge:\n                mode: auto\n                when: gates_resolved_ci_green"))
		want := []Change{{Key: gate0 + "merge", Before: Absent, After: "auto", Direction: Widened}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("changes =\n  %+v\nwant\n  %+v", got, want)
		}
	})

	t.Run("a workflow raise does not multiply across inheriting gates", func(t *testing.T) {
		g, err := ExtractSpecAutonomy([]byte(specDoc("%ESCALATIONS%", "")))
		if err != nil {
			t.Fatal(err)
		}
		for k := range g {
			if strings.Contains(k, ".gates[") {
				t.Errorf("inheriting gate produced its own key %q", k)
			}
		}
	})
}

// TestExtractSpec_ResolvesV2Reuse: a deriving workflow's inherited stage
// carries the base's forbidden_paths through spec.ParseBytes' `extends`
// resolution, so a glob removed from the BASE workflow widens BOTH.
func TestExtractSpec_ResolvesV2Reuse(t *testing.T) {
	doc := func(fp string) string {
		return specDoc("%AUTONOMY%", "", "%ESCALATIONS%", "", "%FORBIDDEN%", fp) +
			"  routine_change:\n    extends: feature_change\n"
	}
	got := diffSpec(t, ExtractSpecForbiddenPaths, doc(`[".github/workflows/**", ".fishhawk/**"]`), doc(`[".github/workflows/**"]`))
	want := []Change{
		{Key: fpKey + "[.fishhawk/**]", Before: Present, After: Absent, Direction: Widened},
		{Key: "workflows.routine_change.stages.implement.forbidden_paths[.fishhawk/**]", Before: Present, After: Absent, Direction: Widened},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes =\n  %+v\nwant\n  %+v", got, want)
	}
}

func TestExtractSpec_EscalationApprovalsAndReviewers(t *testing.T) {
	esc := func(count, extra string) string {
		return specDoc("%ESCALATIONS%", "    escalations:\n      - match:\n          paths: [\"a/**\", \"b/**\"]\n        require:\n          approvals:\n            count: "+count+extra)
	}
	got := diffSpec(t, ExtractSpecEscalations, esc("3", ""), esc("2", ""))
	want := []Change{{Key: escKey + ".approvals.count", Before: "3", After: "2", Direction: Widened}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lowered count: changes =\n  %+v\nwant\n  %+v", got, want)
	}
	if got := diffSpec(t, ExtractSpecEscalations, esc("2", ""), esc("3", "")); len(got) != 1 || got[0].Direction != Narrowed {
		t.Fatalf("raised count: changes = %+v, want one narrowing", got)
	}

	// member_of / min_permission / reviewers are presence Restrictions:
	// dropping each from an otherwise-identical escalation is a widening.
	const personas = "reviewer_personas:\n  security:\n    agent:\n      provider: codex\n    remit:\n      path: docs/review/security-remit.md\n"
	withPersonas := func(doc string) string {
		doc = strings.Replace(doc, "workflows:", personas+"workflows:", 1)
		return strings.Replace(doc, "        permissions:\n", "        reviewers:\n          agents:\n            - provider: codex\n        permissions:\n", 1)
	}
	full := withPersonas(esc("2", "\n            member_of: platform\n            min_permission: maintain\n          reviewers: [security]"))
	bare := esc("2", "")
	got = diffSpec(t, ExtractSpecEscalations, full, bare)
	want = []Change{
		{Key: escKey + ".approvals.member_of[platform]", Before: Present, After: Absent, Direction: Widened},
		{Key: escKey + ".approvals.min_permission[maintain]", Before: Present, After: Absent, Direction: Widened},
		{Key: escKey + ".reviewers[security]", Before: Present, After: Absent, Direction: Widened},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dropped approvals/reviewers: changes =\n  %+v\nwant\n  %+v", got, want)
	}

	// Two escalations sharing one canonical match fold to the STRICTEST:
	// the lower ceiling and the higher count. COUNTERFACTUAL: make
	// putStrictest always overwrite — the STRICTER escalation is declared
	// FIRST here, so last-writer-wins would keep the looser medium/2 and both
	// assertions go RED.
	dup := specDoc("%ESCALATIONS%", "    escalations:\n"+
		"      - match: {paths: [\"a/**\", \"b/**\"]}\n        require: {max_autonomy: low, approvals: {count: 3}}\n"+
		"      - match: {paths: [\"b/**\", \"a/**\"]}\n        require: {max_autonomy: medium, approvals: {count: 2}}")
	g, err := ExtractSpecEscalations([]byte(dup))
	if err != nil {
		t.Fatal(err)
	}
	if e := g[escKey+".max_autonomy"]; e.Value != "low" {
		t.Errorf("folded ceiling = %+v, want low", e)
	}
	if e := g[escKey+".approvals.count"]; e.Value != "3" {
		t.Errorf("folded count = %+v, want 3", e)
	}
}

func TestExtractSpec_Errors(t *testing.T) {
	for _, ex := range []specExtractor{ExtractSpecForbiddenPaths, ExtractSpecEscalations, ExtractSpecAutonomy, ExtractSpecStagePermissions} {
		if g, err := ex([]byte("version: \"2\"\nworkflows: [nope\n")); err == nil {
			t.Errorf("unparseable spec extracted %+v, want an error", g)
		}
		if g, err := ex([]byte("version: \"2\"\nworkflows:\n  x:\n    autonomy: extreme\n    stages: []\n")); err == nil {
			t.Errorf("schema-invalid spec extracted %+v, want an error", g)
		}
		if g, err := ex(nil); err != nil || len(g) != 0 {
			t.Errorf("absent spec = %+v, %v; want empty, nil", g, err)
		}
	}
}
