package agenteval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
)

// securityRemitLine and securityConventionLine are verbatim lines of the REAL
// security remit and convention: the render tests look for them, and the
// classification tests quote them so quote verification passes.
const (
	securityRemitLine      = "What can an attacker newly do, or what can a defender no longer see, because of this change?"
	securityConventionLine = "Approver gates use the forge identity `github:<login>`, not an MCP token subject."
)

// securityFailOpenDiff makes a policy evaluator error proceed (the planted
// fail_closed defect) and touches a test file beside it.
const securityFailOpenDiff = `diff --git a/backend/internal/policy/evaluate.go b/backend/internal/policy/evaluate.go
index 1111111..2222222 100644
--- a/backend/internal/policy/evaluate.go
+++ b/backend/internal/policy/evaluate.go
@@ -10,7 +10,9 @@ func Evaluate(ctx context.Context, in Input) (Decision, error) {
-	d, err := evaluator.Run(ctx, in)
-	if err != nil {
-		return Deny, err
-	}
+	start := time.Now()
+	d, err := evaluator.Run(ctx, in)
+	log.Printf("policy evaluated in %s", time.Since(start))
+	if err != nil {
+		return Allow, nil
+	}
diff --git a/backend/internal/policy/evaluate_test.go b/backend/internal/policy/evaluate_test.go
index 3333333..4444444 100644
--- a/backend/internal/policy/evaluate_test.go
+++ b/backend/internal/policy/evaluate_test.go
@@ -1,3 +1,4 @@
+// The timing line is covered by the log assertion below.
`

// securityHelperDiff is a behaviour-preserving rename inside a policy helper
// plus a new helper file.
const securityHelperDiff = `diff --git a/backend/internal/policy/helpers.go b/backend/internal/policy/helpers.go
index 5555555..6666666 100644
--- a/backend/internal/policy/helpers.go
+++ b/backend/internal/policy/helpers.go
@@ -3,3 +3,3 @@
-func normRule(r string) string { return strings.ToLower(r) }
+func normalizeRule(r string) string { return strings.ToLower(r) }
diff --git a/backend/internal/policy/rules.go b/backend/internal/policy/rules.go
new file mode 100644
index 0000000..7777777
--- /dev/null
+++ b/backend/internal/policy/rules.go
@@ -0,0 +1,1 @@
+package policy
`

func securityTestPlan(steps []string, paths ...string) map[string]any {
	p := personaTestPlan(steps)
	files := make([]any, 0, len(paths))
	for _, path := range paths {
		files = append(files, map[string]any{"path": path, "operation": "modify"})
	}
	p["scope"] = map[string]any{"files": files}
	return p
}

// validSecurityDefectCase is a fail_closed defect case anchored at
// backend/internal/policy/evaluate.go.
func validSecurityDefectCase() map[string]any {
	return map[string]any{
		"persona":     "security",
		"kind":        "defect",
		"expect":      "catch",
		"lens":        "fail_closed",
		"issue_title": "Log policy evaluation latency",
		"issue_body":  "Add a latency log line around the policy evaluator.",
		"plan": securityTestPlan([]string{"Wrap the evaluator call with a timer.", "Log the elapsed time."},
			"backend/internal/policy/evaluate.go", "backend/internal/policy/evaluate_test.go"),
		"diff":                securityFailOpenDiff,
		"defect_phrases":      []any{"return Allow, nil"},
		"anchor":              map[string]any{"kind": "diff_file", "path": "backend/internal/policy/evaluate.go"},
		"catch_probes":        []any{"fails open", "fail open"},
		"attribution_phrases": []any{"evaluate.go"},
		"catching_examples": []any{
			map[string]any{"severity": "medium", "category": "security", "note": "evaluate.go now fails open: an evaluator error returns Allow"},
		},
		"non_catching_examples": []any{
			map[string]any{"category": "correctness", "note": "evaluate.go logs with log.Printf instead of slog"},
			map[string]any{"category": "verification", "note": "the test file adds only a comment"},
		},
	}
}

// validSecurityCleanCase is a clean_sensitive case renaming a policy helper.
func validSecurityCleanCase() map[string]any {
	return map[string]any{
		"persona":     "security",
		"kind":        "clean_sensitive",
		"expect":      "clean",
		"issue_title": "Rename the policy rule normalizer",
		"issue_body":  "normRule reads poorly; rename it and start a rules file.",
		"plan": securityTestPlan([]string{"Rename the normalizer.", "Add the rules file."},
			"backend/internal/policy/helpers.go", "backend/internal/policy/rules.go"),
		"diff":                 securityHelperDiff,
		"defect_phrases":       []any{"normalizeRule"},
		"false_alarm_examples": []any{map[string]any{"category": "security", "note": "helpers.go weakens a check"}},
	}
}

// validSecurityMarkers names three controls of the REAL convention that the
// REAL remit never names.
func validSecurityMarkers() map[string]any {
	return map[string]any{
		"repo_specific_markers":   []any{"gateenv.go", "requireStageAccount", "categories.go"},
		"generic_control_phrases": []any{"allow-list", "scope check", "audit", "pin"},
		"absence_phrases":         []any{"is missing", "does not add", "no longer", "without a"},
	}
}

// writeSecurityMarkers writes dir/markers.json; a string is written raw, nil
// writes nothing.
func writeSecurityMarkers(t *testing.T, dir string, v any) {
	t.Helper()
	if v == nil {
		return
	}
	raw, ok := v.(string)
	if !ok {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		raw = string(b)
	}
	if err := os.WriteFile(filepath.Join(dir, securityMarkersFile), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeSecurityCorpus writes the defect case "failopen", the clean case
// "cleanhelper" and markers.json under a fresh dir.
func writeSecurityCorpus(t *testing.T, defect, clean, markers any) string {
	t.Helper()
	dir := t.TempDir()
	writePersonaCase(t, dir, "failopen", defect)
	writePersonaCase(t, dir, "cleanhelper", clean)
	writeSecurityMarkers(t, dir, markers)
	return dir
}

func loadTestSecurityCases(t *testing.T) (map[string]SecurityCatchCase, SecurityMarkers) {
	t.Helper()
	dir := writeSecurityCorpus(t, validSecurityDefectCase(), validSecurityCleanCase(), validSecurityMarkers())
	cases, markers, err := LoadSecurityCatchCorpus(dir, personaTestRepoRoot)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]SecurityCatchCase{}
	for _, c := range cases {
		out[c.Name] = c
	}
	return out, markers
}

func TestLoadSecurityCatchCorpus_Valid(t *testing.T) {
	dir := writeSecurityCorpus(t, validSecurityDefectCase(), validSecurityCleanCase(), validSecurityMarkers())
	cases, markers, err := LoadSecurityCatchCorpus(dir, personaTestRepoRoot)
	if err != nil {
		t.Fatalf("LoadSecurityCatchCorpus: %v", err)
	}
	if len(cases) != 2 || cases[0].Name != "cleanhelper" || cases[1].Name != "failopen" {
		t.Fatalf("cases = %v, want [cleanhelper failopen] sorted", cases)
	}
	clean, defect := cases[0], cases[1]
	if defect.Expect() != CatchExpectCatch || defect.Lens() != SecurityLensFailClosed || clean.Expect() != CatchExpectClean || clean.Lens() != "" {
		t.Errorf("expect/lens = %s/%s and %s/%s", defect.Expect(), defect.Lens(), clean.Expect(), clean.Lens())
	}
	if want := (CatchRule{Probes: []string{"fails open", "fail open"}, AttributionPhrases: []string{"evaluate.go"}}); !reflect.DeepEqual(defect.CatchRule(), want) {
		t.Errorf("defect rule = %+v, want %+v (no category predicate)", defect.CatchRule(), want)
	}
	if want := (CatchRule{Clean: true}); !reflect.DeepEqual(clean.CatchRule(), want) {
		t.Errorf("clean rule = %+v, want %+v (no category predicate)", clean.CatchRule(), want)
	}
	wantFiles := []SecurityDiffFile{{"backend/internal/policy/helpers.go", "M"}, {"backend/internal/policy/rules.go", "A"}}
	if !reflect.DeepEqual(clean.Files, wantFiles) {
		t.Errorf("clean files = %+v, want %+v", clean.Files, wantFiles)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "failopen", "persona_case.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(defect.Raw) != string(raw) {
		t.Error("case Raw is not the committed persona_case.json bytes")
	}
	mraw, err := os.ReadFile(filepath.Join(dir, securityMarkersFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(markers.Raw) != string(mraw) || !slices.Equal(markers.RepoSpecificMarkers, []string{"gateenv.go", "requireStageAccount", "categories.go"}) {
		t.Errorf("markers = %+v, want the decoded file with its raw bytes", markers)
	}
}

// TestLoadSecurityCatchCorpus_FailClosed: one fixture per loader mode, each
// violating only that mode.
func TestLoadSecurityCatchCorpus_FailClosed(t *testing.T) {
	type mut struct {
		defect  func(m map[string]any)
		clean   func(m map[string]any)
		markers func(m map[string]any)
	}
	set := func(k string, v any) func(map[string]any) { return func(m map[string]any) { m[k] = v } }
	del := func(k string) func(map[string]any) { return func(m map[string]any) { delete(m, k) } }
	tests := []struct {
		name string
		mut  mut
		want string
	}{
		// (c)
		{"persona", mut{defect: set("persona", "architect")}, "is not the security arms' persona"},
		// (d)
		{"unknown kind", mut{defect: set("kind", "mystery")}, `unknown kind "mystery"`},
		{"unknown expect", mut{defect: set("expect", "maybe")}, `unknown expect "maybe"`},
		{"kind expect mismatch", mut{defect: set("expect", "clean")}, `kind "defect" expects "catch"`},
		{"defect without lens", mut{defect: del("lens")}, "a defect case needs a lens"},
		{"defect unknown lens", mut{defect: set("lens", "vibes")}, "a defect case needs a lens"},
		{"clean with lens", mut{clean: set("lens", "fail_closed")}, "a clean_sensitive case carries no lens"},
		// (e)
		{"blank issue title", mut{defect: set("issue_title", "  ")}, "issue_title and issue_body must be non-empty"},
		{"blank issue body", mut{clean: set("issue_body", "")}, "issue_title and issue_body must be non-empty"},
		{"invalid plan", mut{defect: func(m map[string]any) { personaPlanOf(m)["plan_version"] = "standard_v0" }}, "plan is not a valid standard_v1 plan"},
		// (f)
		{"empty diff", mut{defect: set("diff", " \n")}, "diff is empty"},
		{"no file header", mut{clean: set("diff", "--- a/x\n+++ b/x\n@@ -1 +1 @@\n+func normalizeRule() {}\n")}, "no `diff --git` file header"},
		{"malformed header", mut{defect: set("diff", strings.Replace(securityFailOpenDiff, "b/backend/internal/policy/evaluate.go", "b/backend/internal/policy/other.go", 1))}, "malformed diff file header"},
		{"path outside scope", mut{clean: func(m map[string]any) {
			m["diff"] = securityHelperDiff + "diff --git a/backend/internal/auth/token.go b/backend/internal/auth/token.go\n"
		}}, "backend/internal/auth/token.go, which is outside the plan's scope.files"},
		{"diff touches remit", mut{clean: func(m map[string]any) {
			m["plan"] = securityTestPlan([]string{"Rename.", "Add."}, "backend/internal/policy/helpers.go", "backend/internal/policy/rules.go", personaSecurityRemitPath)
			m["diff"] = securityHelperDiff + "diff --git a/" + personaSecurityRemitPath + " b/" + personaSecurityRemitPath + "\n"
		}}, "a diff touching the remit or the convention"},
		{"diff touches convention", mut{clean: func(m map[string]any) {
			m["plan"] = securityTestPlan([]string{"Rename.", "Add."}, "backend/internal/policy/helpers.go", "backend/internal/policy/rules.go", personaSecurityConventionPath)
			m["diff"] = securityHelperDiff + "diff --git a/" + personaSecurityConventionPath + " b/" + personaSecurityConventionPath + "\n"
		}}, "a diff touching the remit or the convention"},
		// (g)
		{"empty defect phrases", mut{defect: set("defect_phrases", []any{})}, "defect_phrases must be non-empty"},
		{"blank defect phrase", mut{clean: set("defect_phrases", []any{"normalizeRule", " "})}, "defect_phrases[1] is empty"},
		{"defect phrase absent", mut{defect: set("defect_phrases", []any{"skips the deny branch"})}, "occurs in neither the issue, the plan nor the diff"},
		// (h)
		{"clean field on catch", mut{defect: set("false_alarm_examples", []any{map[string]any{"category": "security", "note": "x"}})}, "must not carry the clean-case field"},
		{"catch field on clean", mut{clean: set("anchor", map[string]any{"kind": "diff_file", "path": "backend/internal/policy/helpers.go"})}, "must not carry catch-case fields"},
		// (i)
		{"absent anchor", mut{defect: del("anchor")}, "a catch case needs an anchor"},
		{"anchor kind", mut{defect: set("anchor", map[string]any{"kind": "approach_step", "path": "backend/internal/policy/evaluate.go"})}, `unknown anchor kind "approach_step"`},
		{"anchor path not in diff", mut{defect: set("anchor", map[string]any{"kind": "diff_file", "path": "backend/internal/policy/rules.go"})}, "is not among the diff's changed files"},
		// (j)
		{"empty probes", mut{defect: set("catch_probes", []any{})}, "catch_probes and attribution_phrases must be non-empty"},
		{"blank probe", mut{defect: set("catch_probes", []any{"fails open", "  "})}, "catch_probes[1] is empty"},
		{"empty attribution", mut{defect: set("attribution_phrases", []any{})}, "catch_probes and attribution_phrases must be non-empty"},
		{"blank attribution", mut{defect: set("attribution_phrases", []any{"evaluate.go", ""})}, "attribution_phrases[1] is empty"},
		// (k)
		{"attribution absent from anchor", mut{defect: set("attribution_phrases", []any{"evaluate.go", "evaluator error"})}, `"evaluator error" is absent from the anchored path`},
		{"attribution in another path", mut{defect: set("attribution_phrases", []any{"evaluate"})}, "also occurs in the changed path backend/internal/policy/evaluate_test.go"},
		// (l)
		{"empty catching examples", mut{defect: set("catching_examples", []any{})}, "catching_examples and non_catching_examples must both be non-empty"},
		{"catching example not caught", mut{defect: set("catching_examples", []any{map[string]any{"category": "security", "note": "evaluate.go logs too much"}})}, "is not a catch under catch rule v5"},
		{"non-catching example caught", mut{defect: set("non_catching_examples", []any{
			map[string]any{"category": "security", "note": "evaluate.go fails open"},
		})}, `non_catching_examples[0] ("evaluate.go fails open") is caught`},
		{"probe matches non-catching", mut{defect: set("non_catching_examples", []any{
			map[string]any{"category": "correctness", "note": "evaluate.go logs with log.Printf"},
			map[string]any{"category": "security", "note": "a cache that fails open"},
		})}, `catch probe "fails open" matches non_catching_examples[1]`},
		{"no non-catching names anchor", mut{defect: set("non_catching_examples", []any{map[string]any{"category": "verification", "note": "the test file adds only a comment"}})}, "no non_catching_example names the anchored file"},
		// (m)
		{"probe in issue", mut{defect: set("issue_body", "Add a latency log line; the evaluator must never fail open.")}, `catch probe "fail open" occurs in the issue, the plan or the diff`},
		{"probe in plan", mut{defect: func(m map[string]any) {
			personaPlanOf(m)["summary"] = "Time the evaluator; it fails open never."
		}}, `catch probe "fails open" occurs in the issue, the plan or the diff`},
		// The probe occurs ONLY on a removed '-' line of the diff, so only the
		// diff half of the check can refuse it (CF3).
		{"probe_in_diff", mut{defect: set("catch_probes", []any{"fails open", "return deny, err"})}, `catch probe "return deny, err" occurs in the issue, the plan or the diff`},
		// (n)
		{"empty false alarm examples", mut{clean: set("false_alarm_examples", []any{})}, "false_alarm_examples must be non-empty"},
		// (o)
		{"markers malformed", mut{markers: func(m map[string]any) { m["repo_specific_markers"] = "gateenv.go" }}, "markers.json: parse"},
		{"markers unknown field", mut{markers: set("extra", []any{"x"})}, "markers.json: parse"},
		{"markers empty list", mut{markers: set("absence_phrases", []any{})}, "absence_phrases must be non-empty"},
		{"markers blank entry", mut{markers: set("absence_phrases", []any{"is missing", " "})}, "absence_phrases[1] is blank"},
		{"markers duplicate", mut{markers: set("absence_phrases", []any{"is missing", "Is Missing"})}, `absence_phrases lists "Is Missing" twice`},
		{"generic phrase inside another", mut{markers: set("generic_control_phrases", []any{"pin", "pinned to", "audit"})}, `generic control phrase "pin" occurs inside "pinned to"`},
		{"marker absent from convention", mut{markers: set("repo_specific_markers", []any{"gateenv.go", "not_a_control.go"})}, `"not_a_control.go" is absent from docs/review/security-conventions.md`},
		{"marker in remit", mut{markers: set("repo_specific_markers", []any{"gateenv.go", "allow-list"})}, `"allow-list" occurs in the generic remit`},
		{"marker in clean case", mut{clean: set("issue_body", "normRule reads poorly; rename it like gateenv.go does.")}, `"gateenv.go" occurs in clean case "cleanhelper"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, c, m := validSecurityDefectCase(), validSecurityCleanCase(), validSecurityMarkers()
			if tc.mut.defect != nil {
				tc.mut.defect(d)
			}
			if tc.mut.clean != nil {
				tc.mut.clean(c)
			}
			if tc.mut.markers != nil {
				tc.mut.markers(m)
			}
			dir := writeSecurityCorpus(t, d, c, m)
			cases, markers, err := LoadSecurityCatchCorpus(dir, personaTestRepoRoot)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
			if cases != nil || !reflect.DeepEqual(markers, SecurityMarkers{}) {
				t.Errorf("a refused load returned cases %v / markers %+v", cases, markers)
			}
		})
	}

	// The modes that need a different corpus shape.
	shape := []struct {
		name  string
		setup func(t *testing.T) (dir, root string)
		want  string
	}{
		{"absent dir", func(t *testing.T) (string, string) {
			return filepath.Join(t.TempDir(), "nope"), personaTestRepoRoot
		}, "is absent"}, // (a)
		{"zero cases", func(t *testing.T) (string, string) {
			dir := t.TempDir()
			writeSecurityMarkers(t, dir, validSecurityMarkers())
			return dir, personaTestRepoRoot
		}, "holds zero cases"}, // (a)
		{"missing persona_case.json", func(t *testing.T) (string, string) {
			return writeSecurityCorpus(t, nil, validSecurityCleanCase(), validSecurityMarkers()), personaTestRepoRoot
		}, `"failopen": read persona_case.json`}, // (b)
		{"malformed json", func(t *testing.T) (string, string) {
			return writeSecurityCorpus(t, "{not json", validSecurityCleanCase(), validSecurityMarkers()), personaTestRepoRoot
		}, `"failopen": parse persona_case.json`}, // (b)
		{"unknown field", func(t *testing.T) (string, string) {
			d := validSecurityDefectCase()
			d["governing_records"] = []any{}
			return writeSecurityCorpus(t, d, validSecurityCleanCase(), validSecurityMarkers()), personaTestRepoRoot
		}, `unknown field "governing_records"`}, // (b)
		{"trailing content", func(t *testing.T) (string, string) {
			raw, err := json.Marshal(validSecurityDefectCase())
			if err != nil {
				t.Fatal(err)
			}
			return writeSecurityCorpus(t, string(raw)+"\n{}", validSecurityCleanCase(), validSecurityMarkers()), personaTestRepoRoot
		}, "trailing content"}, // (b)
		{"markers absent", func(t *testing.T) (string, string) {
			return writeSecurityCorpus(t, validSecurityDefectCase(), validSecurityCleanCase(), nil), personaTestRepoRoot
		}, "markers.json: read"}, // (o)
		{"markers trailing content", func(t *testing.T) (string, string) {
			raw, err := json.Marshal(validSecurityMarkers())
			if err != nil {
				t.Fatal(err)
			}
			return writeSecurityCorpus(t, validSecurityDefectCase(), validSecurityCleanCase(), string(raw)+"{}"), personaTestRepoRoot
		}, "markers.json: parse"}, // (o)
		{"convention unreadable", func(t *testing.T) (string, string) {
			return writeSecurityCorpus(t, validSecurityDefectCase(), validSecurityCleanCase(), validSecurityMarkers()), t.TempDir()
		}, "read docs/review/security-conventions.md"}, // (o)
		{"remit unreadable", func(t *testing.T) (string, string) {
			root := t.TempDir()
			copyRepoFile(t, root, personaSecurityConventionPath)
			return writeSecurityCorpus(t, validSecurityDefectCase(), validSecurityCleanCase(), validSecurityMarkers()), root
		}, "read docs/review/security-remit.md"}, // (o)
	}
	for _, tc := range shape {
		t.Run(tc.name, func(t *testing.T) {
			dir, root := tc.setup(t)
			cases, _, err := LoadSecurityCatchCorpus(dir, root)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
			if cases != nil {
				t.Errorf("a refused load returned cases %v", cases)
			}
		})
	}
}

// copyRepoFile copies the checkout file rel into root at the same path.
func copyRepoFile(t *testing.T, root, rel string) {
	t.Helper()
	raw := mustReadCatch(t, filepath.Join(personaTestRepoRoot, filepath.FromSlash(rel)))
	dst := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseSecurityDiff(t *testing.T) {
	files, err := parseSecurityDiff("diff --git a/x/a.go b/x/a.go\nnew file mode 100644\n" +
		"diff --git a/x/b.go b/x/b.go\nindex 1..2\n" +
		"diff --git a/x/c.go b/x/c.go\ndeleted file mode 100644\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []SecurityDiffFile{{"x/a.go", "A"}, {"x/b.go", "M"}, {"x/c.go", "D"}}
	if !reflect.DeepEqual(files, want) {
		t.Errorf("files = %+v, want %+v", files, want)
	}
	if got := renderSecurityChangedFiles(files); got != "- A x/a.go\n- M x/b.go\n- D x/c.go\n" {
		t.Errorf("changed-files render = %q", got)
	}
	for _, bad := range []struct{ diff, want string }{
		{"", "diff is empty"},
		{"+just a line\n", "no `diff --git` file header"},
		{"diff --git a/x.go b/y.go\n", "malformed"},
		{"diff --git x.go x.go\n", "malformed"},
		{"diff --git a/ b/\n", "malformed"},
		{"diff --git a//x b//x\n", "malformed"},
		{"diff --git a/x.go b/x.go\ndiff --git a/x.go b/x.go\n", "changes x.go twice"},
	} {
		if _, err := parseSecurityDiff(bad.diff); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("parseSecurityDiff(%q) = %v, want an error containing %q", bad.diff, err, bad.want)
		}
	}
}

// TestSecurityArmPrompt_RendersThroughTheProductionBuilder: the security arm's
// implement_review prompt carries the remit and the convention section, the
// portability prompt the remit alone; QuoteDocs, caps and truncation follow.
func TestSecurityArmPrompt_RendersThroughTheProductionBuilder(t *testing.T) {
	cases, _ := loadTestSecurityCases(t)
	c := cases["failopen"]
	sec, err := SecurityArmPrompt(c, personaTestRepoRoot, ArmSecurity)
	if err != nil {
		t.Fatalf("SecurityArmPrompt(security): %v", err)
	}
	port, err := SecurityArmPrompt(c, personaTestRepoRoot, ArmSecurityPortability)
	if err != nil {
		t.Fatalf("SecurityArmPrompt(portability): %v", err)
	}
	for name, r := range map[string]SecurityArmRender{"security": sec, "portability": port} {
		if !strings.HasPrefix(r.Prompt, "You are an implement-review agent for the repository") {
			t.Errorf("%s: prompt is not an implement_review render", name)
		}
		for _, want := range []string{"Reviewer persona remit: security", securityRemitLine, "return Allow, nil", "- M backend/internal/policy/evaluate.go"} {
			if !strings.Contains(r.Prompt, want) {
				t.Errorf("%s: prompt lacks %q", name, want)
			}
		}
		if r.QuoteDocs[0].Path != personaSecurityRemitPath || r.QuoteDocs[0].Commit != personaFixtureCommit {
			t.Errorf("%s: first quote doc = %s@%s, want the remit at the fixture commit", name, r.QuoteDocs[0].Path, r.QuoteDocs[0].Commit)
		}
		if r.RemitTruncated || r.ConventionTruncated {
			t.Errorf("%s: a document was truncated (remit %v, convention %v)", name, r.RemitTruncated, r.ConventionTruncated)
		}
	}
	for _, want := range []string{"### " + prompt.ReviewConventionsHeading, "#### Convention security-conventions (severity cap: medium)", securityConventionLine} {
		if !strings.Contains(sec.Prompt, want) {
			t.Errorf("security prompt lacks %q", want)
		}
	}
	if len(sec.QuoteDocs) != 2 || sec.QuoteDocs[1].Path != personaSecurityConventionPath {
		t.Errorf("security quote docs = %v, want the remit then the convention", sec.QuoteDocs)
	}
	if !reflect.DeepEqual(sec.Caps, planreview.ConventionCaps{"security-conventions": "medium"}) {
		t.Errorf("security caps = %v", sec.Caps)
	}
	if len(sec.StandardTrigger.ReviewConventions) != 1 || len(sec.StandardTrigger.InjectedDocuments) != 0 {
		t.Errorf("security standard trigger: %d conventions, %d injected docs; want 1 and 0", len(sec.StandardTrigger.ReviewConventions), len(sec.StandardTrigger.InjectedDocuments))
	}
	if strings.Contains(port.Prompt, prompt.ReviewConventionsHeading) || strings.Contains(port.Prompt, securityConventionLine) {
		t.Error("the portability prompt carries the conventions section")
	}
	if len(port.QuoteDocs) != 1 || port.Caps != nil || len(port.StandardTrigger.ReviewConventions) != 0 {
		t.Errorf("portability: %d quote docs, caps %v, %d conventions; want 1, nil, 0", len(port.QuoteDocs), port.Caps, len(port.StandardTrigger.ReviewConventions))
	}

	t.Run("unknown arm", func(t *testing.T) {
		if _, err := SecurityArmPrompt(c, personaTestRepoRoot, "architect"); err == nil || !strings.Contains(err.Error(), "unknown security arm") {
			t.Errorf("err = %v, want an unknown-arm error", err)
		}
	})
	t.Run("a missing remit fails the render", func(t *testing.T) {
		if _, err := SecurityArmPrompt(c, t.TempDir(), ArmSecurityPortability); err == nil || !strings.Contains(err.Error(), "resolve remit") {
			t.Errorf("err = %v, want a remit error", err)
		}
	})
	t.Run("a missing convention fails the security render only", func(t *testing.T) {
		root := t.TempDir()
		copyRepoFile(t, root, personaSecurityRemitPath)
		if _, err := SecurityArmPrompt(c, root, ArmSecurity); err == nil || !strings.Contains(err.Error(), "resolve convention") {
			t.Errorf("err = %v, want a convention error", err)
		}
		if _, err := SecurityArmPrompt(c, root, ArmSecurityPortability); err != nil {
			t.Errorf("portability without a convention: %v", err)
		}
	})
}

// TestSecurityArmPrompt_StandardRenderByteIdentical: per arm, the standard
// implement_review render is byte-identical before and after the persona build
// and equals a direct prompt.Build of a field-by-field trigger; the persona
// render is it plus ONE contiguous insertion carrying the remit envelope.
func TestSecurityArmPrompt_StandardRenderByteIdentical(t *testing.T) {
	cases, _ := loadTestSecurityCases(t)
	c := cases["failopen"]
	conv, _, err := SecurityConvention(personaTestRepoRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		arm   string
		convs []prompt.ReviewConvention
	}{{ArmSecurity, []prompt.ReviewConvention{conv}}, {ArmSecurityPortability, nil}} {
		before, err := prompt.Build("implement_review", SecurityStandardTrigger(c, tc.convs))
		if err != nil {
			t.Fatal(err)
		}
		r, err := SecurityArmPrompt(c, personaTestRepoRoot, tc.arm)
		if err != nil {
			t.Fatal(err)
		}
		after, err := prompt.Build("implement_review", SecurityStandardTrigger(c, tc.convs))
		if err != nil {
			t.Fatal(err)
		}
		if before != after {
			t.Errorf("%s: the standard render changed across the persona build", tc.arm)
		}
		direct, err := prompt.Build("implement_review", prompt.Trigger{
			Source:            "github_issue",
			IssueTitle:        c.Input.IssueTitle,
			IssueBody:         c.Input.IssueBody,
			Repo:              "kuhlman-labs/fishhawk",
			ApprovedPlan:      c.Plan,
			Diff:              "- M backend/internal/policy/evaluate.go\n- M backend/internal/policy/evaluate_test.go\n",
			DiffPatch:         securityFailOpenDiff,
			ReviewConventions: tc.convs,
		})
		if err != nil {
			t.Fatal(err)
		}
		if before != direct {
			t.Errorf("%s: the standard render differs from a direct prompt.Build of the standard trigger", tc.arm)
		}
		if strings.Contains(before, "Reviewer persona remit:") {
			t.Errorf("%s: the standard render carries a persona remit", tc.arm)
		}
		pre := commonPrefixLen(before, r.Prompt)
		suf := commonSuffixLen(before[pre:], r.Prompt[pre:])
		if pre+suf != len(before) {
			t.Errorf("%s: the persona render is not the standard render plus one contiguous insertion (prefix %d + suffix %d != %d)", tc.arm, pre, suf, len(before))
			continue
		}
		inserted := r.Prompt[pre : len(r.Prompt)-suf]
		if !strings.Contains(inserted, "### Reviewer persona remit: security\n") || !strings.Contains(inserted, securityRemitLine) {
			t.Errorf("%s: the inserted span lacks the remit envelope", tc.arm)
		}
	}
}

func securityVerdict(t *testing.T, concerns ...map[string]any) string {
	t.Helper()
	list := make([]any, 0, len(concerns))
	for _, c := range concerns {
		list = append(list, c)
	}
	raw, err := json.Marshal(map[string]any{"verdict": "approve_with_concerns", "concerns": list})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestClassifySecurityTrial_ConventionCitationVerifies (CF2): a medium concern
// quoting the REAL convention verbatim with document_ref naming it stays
// medium and verified — the convention is in the quote set — while the same
// quote against the remit-only portability quote set is demoted to low.
func TestClassifySecurityTrial_ConventionCitationVerifies(t *testing.T) {
	cases, markers := loadTestSecurityCases(t)
	c := cases["cleanhelper"]
	sec, err := SecurityArmPrompt(c, personaTestRepoRoot, ArmSecurity)
	if err != nil {
		t.Fatal(err)
	}
	resp := securityVerdict(t, map[string]any{"severity": "medium", "category": "security", "note": "the approver gate is keyed wrong", "quoted_passage": securityConventionLine, "document_ref": personaSecurityConventionPath})
	trial := ClassifySecurityTrial(resp, c.CatchRule(), sec.QuoteDocs, sec.Caps, markers)
	if trial.Outcome != CatchFalseAlarm || len(trial.Concerns) != 1 {
		t.Fatalf("trial = %+v, want one stored false alarm", trial)
	}
	if got := trial.Concerns[0]; got.Severity != "medium" || got.QuoteUnverified || got.SeverityClampedFrom != "" || got.DocumentRef != personaSecurityConventionPath {
		t.Errorf("convention citation = %+v, want medium and verified", got)
	}
	port, err := SecurityArmPrompt(c, personaTestRepoRoot, ArmSecurityPortability)
	if err != nil {
		t.Fatal(err)
	}
	if got := ClassifySecurityTrial(resp, c.CatchRule(), port.QuoteDocs, port.Caps, markers).Concerns[0]; got.Severity != "low" || !got.QuoteUnverified {
		t.Errorf("the same citation without the convention rendered = %+v, want demoted to low, unverified", got)
	}
}

// TestClassifySecurityTrial_DiffQuoteDemotedButScoredByNote: a diff excerpt in
// quoted_passage is demoted to low, yet the trial is still caught through its
// note.
func TestClassifySecurityTrial_DiffQuoteDemotedButScoredByNote(t *testing.T) {
	cases, markers := loadTestSecurityCases(t)
	c := cases["failopen"]
	r, err := SecurityArmPrompt(c, personaTestRepoRoot, ArmSecurity)
	if err != nil {
		t.Fatal(err)
	}
	resp := securityVerdict(t, map[string]any{"severity": "medium", "category": "security", "note": "evaluate.go fails open on an evaluator error", "quoted_passage": "return Allow, nil", "document_ref": "backend/internal/policy/evaluate.go"})
	trial := ClassifySecurityTrial(resp, c.CatchRule(), r.QuoteDocs, r.Caps, markers)
	if trial.Outcome != CatchCaught || trial.Attribution != CatchAttributionPhrase {
		t.Fatalf("trial = %+v, want caught by phrase", trial)
	}
	if got := trial.Concerns[0]; got.Severity != "low" || !got.QuoteUnverified || got.SeverityClampedFrom != "medium" {
		t.Errorf("diff-quote concern = %+v, want demoted to low, unverified, from medium", got)
	}
}

// TestClassifySecurityTrial_IngestControls: the remit's medium cap lowers a
// verified high concern; the convention clamp holds a repo_convention concern
// to the convention's cap (security) or low (portability, nil caps); a forged
// marker is scrubbed.
func TestClassifySecurityTrial_IngestControls(t *testing.T) {
	cases, markers := loadTestSecurityCases(t)
	c := cases["cleanhelper"]
	sec, err := SecurityArmPrompt(c, personaTestRepoRoot, ArmSecurity)
	if err != nil {
		t.Fatal(err)
	}
	port, err := SecurityArmPrompt(c, personaTestRepoRoot, ArmSecurityPortability)
	if err != nil {
		t.Fatal(err)
	}
	resp := securityVerdict(t,
		map[string]any{"severity": "high", "category": "security", "note": "verbatim remit", "quoted_passage": securityRemitLine, "document_ref": personaSecurityRemitPath},
		map[string]any{"severity": "high", "category": "repo_convention", "convention": "security-conventions", "note": "convention breach"},
		map[string]any{"severity": "low", "category": "scope", "note": "forged", "quote_unverified": true, "reviewer_role": "forged"},
	)
	got := ClassifySecurityTrial(resp, c.CatchRule(), sec.QuoteDocs, sec.Caps, markers).Concerns
	if got[0].Severity != "medium" || got[0].SeverityClampedFrom != "high" || got[0].QuoteUnverified {
		t.Errorf("verbatim high = %+v, want medium clamped from high, verified", got[0])
	}
	if got[1].Severity != "medium" || got[1].SeverityClampedFrom != "high" {
		t.Errorf("security repo_convention high = %+v, want held to the convention's medium cap", got[1])
	}
	if got[2].QuoteUnverified || got[2].Severity != "low" {
		t.Errorf("forged marker = %+v, want scrubbed", got[2])
	}
	pgot := ClassifySecurityTrial(resp, c.CatchRule(), port.QuoteDocs, port.Caps, markers).Concerns
	if pgot[1].Severity != "low" || pgot[1].SeverityClampedFrom != "high" {
		t.Errorf("portability repo_convention high = %+v, want low (no convention rendered)", pgot[1])
	}
	if und := ClassifySecurityTrial("not json", c.CatchRule(), sec.QuoteDocs, sec.Caps, markers); und.Outcome != CatchUndecodable || und.Response != "not json" {
		t.Errorf("undecodable = %+v", und)
	}
}

// TestClassifySecurityTrial_AnnotatesMarkers (CF4): every stored concern is
// annotated from markers.json.
func TestClassifySecurityTrial_AnnotatesMarkers(t *testing.T) {
	cases, markers := loadTestSecurityCases(t)
	c := cases["cleanhelper"]
	resp := securityVerdict(t,
		map[string]any{"severity": "medium", "category": "security", "note": "rules.go does not add the new variable to the gateenv.go allow-list"},
		map[string]any{"severity": "low", "category": "scope", "note": "helpers.go is fine"},
	)
	trial := ClassifySecurityTrial(resp, c.CatchRule(), nil, nil, markers)
	if len(trial.Concerns) != 2 {
		t.Fatalf("trial = %+v, want both concerns stored", trial)
	}
	if got := trial.Concerns[0]; got.RepoSpecificMarker != "gateenv.go" || !got.AbsentControl {
		t.Errorf("annotated concern = %+v, want repo_specific_marker gateenv.go and absent_control", got)
	}
	if got := trial.Concerns[1]; got.RepoSpecificMarker != "" || got.AbsentControl {
		t.Errorf("plain concern = %+v, want no annotation", got)
	}
}

// TestSecurityMarkers_Annotate pins the annotation rule row by row, including
// approval condition C4's correctly-pinned-bump note: generic control phrases
// are WORD-BOUNDED, so "pin" does not match "pinned".
func TestSecurityMarkers_Annotate(t *testing.T) {
	_, markers := loadTestSecurityCases(t)
	tests := []struct {
		name, category, note string
		marker               string
		absent               bool
	}{
		{"repo marker with absence", "security", "the change does not add requireStageAccount to the route", "requireStageAccount", true},
		{"generic control with absence", "security", "the route is missing a scope check: is missing", "", true},
		{"repo marker without absence", "security", "categories.go registers the new category", "categories.go", false},
		{"absence without a control", "correctness", "the error path no longer logs the retry count", "", false},
		{"correctly pinned bump (C4)", "supply_chain", "the install script is pinned to the v2.9.0 tag and no longer references v2.8.0", "", false},
		{"word-bounded absence", "security", "the blacksmith audit runs", "", false},
		{"marker named in the category", "gateenv.go", "an allow-list entry is missing", "gateenv.go", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := CatchTrialConcern{Category: tc.category, Note: tc.note}
			markers.Annotate(&c)
			if c.RepoSpecificMarker != tc.marker || c.AbsentControl != tc.absent {
				t.Errorf("Annotate(%q, %q) = marker %q absent %v, want %q %v", tc.category, tc.note, c.RepoSpecificMarker, c.AbsentControl, tc.marker, tc.absent)
			}
		})
	}
}

func TestContainsWordBounded(t *testing.T) {
	tests := []struct {
		text, phrase string
		want         bool
	}{
		{"pinned to v2", "pin", false},
		{"a pin gap", "pin", true},
		{"PIN.", "pin", true},
		{"spin", "pin", false},
		{"pin_x", "pin", false},
		{"the allow-list grew", "allow-list", true},
		{"spin then pin", "pin", true},
		{"anything", "  ", false},
		{"émpin", "pin", false},
	}
	for _, tc := range tests {
		if got := containsWordBounded(tc.text, tc.phrase); got != tc.want {
			t.Errorf("containsWordBounded(%q, %q) = %v, want %v", tc.text, tc.phrase, got, tc.want)
		}
	}
}

func TestSecurityLensCategory(t *testing.T) {
	for _, cat := range []string{"security", "Security", "security/authz", "authn_authz", "secret-custody", "Supply Chain", "authz", "permissions", "fail_open"} {
		if !SecurityLensCategory(cat) {
			t.Errorf("SecurityLensCategory(%q) = false, want true", cat)
		}
	}
	for _, cat := range []string{"", "scope", "correctness", "regression", "verification", "repo_convention", "decision_record"} {
		if SecurityLensCategory(cat) {
			t.Errorf("SecurityLensCategory(%q) = true, want false", cat)
		}
	}
}

// fakeSecuritySender returns resp (or err) and records every call.
type fakeSecuritySender struct {
	resp    string
	err     error
	calls   int
	systems []string
	users   []string
}

func (f *fakeSecuritySender) Messages(_ context.Context, system, user string) (string, string, int, int, int, int, error) {
	f.calls++
	f.systems = append(f.systems, system)
	f.users = append(f.users, user)
	if f.err != nil {
		return "", "", 0, 0, 0, 0, f.err
	}
	return f.resp, "fake-model", 0, 0, 0, 0, nil
}

func TestRunSecurityCatchArm_Counts(t *testing.T) {
	m, markers := loadTestSecurityCases(t)
	cases := []SecurityCatchCase{m["cleanhelper"], m["failopen"]}
	resp := `{"verdict":"approve_with_concerns","concerns":[{"severity":"medium","category":"security","note":"evaluate.go fails open and is missing the gateenv.go audit"}]}`
	sender := &fakeSecuritySender{resp: resp}
	report, err := RunSecurityCatchArm(context.Background(), sender, cases, markers, personaTestRepoRoot, ArmSecurity, 2)
	if err != nil {
		t.Fatalf("RunSecurityCatchArm: %v", err)
	}
	if sender.calls != 4 || report.Arm != ArmSecurity || report.SamplesPerCase != 2 {
		t.Errorf("calls %d, arm %q, samples %d; want 4, security, 2", sender.calls, report.Arm, report.SamplesPerCase)
	}
	for i, s := range sender.systems {
		if s != securityCatchGeneratorSystemPrompt {
			t.Fatalf("call %d system prompt = %q, want the security generator system prompt", i, s)
		}
		if !strings.Contains(sender.users[i], prompt.ReviewConventionsHeading) {
			t.Errorf("call %d: the security arm's prompt lacks the conventions section", i)
		}
	}
	if got, want := report.PerCase["failopen"], (CaseCatchCounts{Expect: CatchExpectCatch, Trials: 2, Caught: 2}); got != want {
		t.Errorf("catch counts = %+v, want %+v", got, want)
	}
	if got, want := report.PerCase["cleanhelper"], (CaseCatchCounts{Expect: CatchExpectClean, Trials: 2, FalseAlarms: 2}); got != want {
		t.Errorf("clean counts = %+v, want %+v", got, want)
	}
	if got := report.Trials["cleanhelper"][0].Concerns[0]; got.RepoSpecificMarker != "gateenv.go" || !got.AbsentControl {
		t.Errorf("stored clean-case concern = %+v, want annotated", got)
	}

	t.Run("portability runs the clean cases only", func(t *testing.T) {
		s := &fakeSecuritySender{resp: resp}
		rep, err := RunSecurityCatchArm(context.Background(), s, cases, markers, personaTestRepoRoot, ArmSecurityPortability, 3)
		if err != nil {
			t.Fatal(err)
		}
		if s.calls != 3 || len(rep.PerCase) != 1 || rep.Arm != ArmSecurityPortability {
			t.Fatalf("calls %d, per-case %v, arm %q; want 3 calls on the clean case only", s.calls, rep.PerCase, rep.Arm)
		}
		if got, want := rep.PerCase["cleanhelper"], (CaseCatchCounts{Expect: CatchExpectClean, Trials: 3, FalseAlarms: 3}); got != want {
			t.Errorf("clean counts = %+v, want %+v", got, want)
		}
		for i, u := range s.users {
			if strings.Contains(u, prompt.ReviewConventionsHeading) {
				t.Errorf("call %d: the portability prompt carries the conventions section", i)
			}
		}
	})
	t.Run("undecodable counts per class", func(t *testing.T) {
		rep, err := RunSecurityCatchArm(context.Background(), &fakeSecuritySender{resp: "not json"}, cases, markers, personaTestRepoRoot, ArmSecurity, 1)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := rep.PerCase["failopen"], (CaseCatchCounts{Expect: CatchExpectCatch, Trials: 1, Undecodable: 1}); got != want {
			t.Errorf("catch counts = %+v, want %+v", got, want)
		}
	})
}

func TestSecurityArmCases(t *testing.T) {
	m, _ := loadTestSecurityCases(t)
	cases := []SecurityCatchCase{m["cleanhelper"], m["failopen"]}
	if got := SecurityArmCases(cases, ArmSecurity); len(got) != 2 {
		t.Errorf("security arm cases = %d, want 2", len(got))
	}
	if got := SecurityArmCases(cases, ArmSecurityPortability); len(got) != 1 || got[0].Name != "cleanhelper" {
		t.Errorf("portability arm cases = %v, want [cleanhelper]", got)
	}
	if got := SecurityArmCases(cases, "architect"); got != nil {
		t.Errorf("unknown arm cases = %v, want nil", got)
	}
}

func TestRunSecurityCatchArm_FailsClosed(t *testing.T) {
	m, markers := loadTestSecurityCases(t)
	all := []SecurityCatchCase{m["failopen"]}
	ok := `{"verdict":"approve"}`
	failRender := func(SecurityCatchCase, string, string) (SecurityArmRender, error) {
		return SecurityArmRender{}, errors.New("render exploded")
	}
	tests := []struct {
		name      string
		sender    *fakeSecuritySender
		nilSender bool
		cases     []SecurityCatchCase
		markers   SecurityMarkers
		arm       string
		samples   int
		render    securityArmRenderer
		want      string
		wantCalls int
	}{
		{"unknown arm", &fakeSecuritySender{resp: ok}, false, all, markers, "architect", 1, SecurityArmPrompt, "unknown security arm", 0},
		{"empty markers", &fakeSecuritySender{resp: ok}, false, all, SecurityMarkers{RepoSpecificMarkers: []string{"gateenv.go"}}, ArmSecurity, 1, SecurityArmPrompt, "markers are required", 0},
		{"no cases", &fakeSecuritySender{resp: ok}, false, nil, markers, ArmSecurity, 1, SecurityArmPrompt, "no cases", 0},
		{"portability without clean cases", &fakeSecuritySender{resp: ok}, false, all, markers, ArmSecurityPortability, 1, SecurityArmPrompt, "no cases", 0},
		{"samples < 1", &fakeSecuritySender{resp: ok}, false, all, markers, ArmSecurity, 0, SecurityArmPrompt, "samples must be >= 1", 0},
		{"nil sender", nil, true, all, markers, ArmSecurity, 1, SecurityArmPrompt, "sender is required", 0},
		{"render error", &fakeSecuritySender{resp: ok}, false, all, markers, ArmSecurity, 1, failRender, "render exploded", 0},
		{"transport error", &fakeSecuritySender{err: errors.New("transport down")}, false, all, markers, ArmSecurity, 2, SecurityArmPrompt, "transport down", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var sender MessageSender
			if !tc.nilSender {
				sender = tc.sender
			}
			report, err := runSecurityCatchArm(context.Background(), sender, tc.cases, tc.markers, personaTestRepoRoot, tc.arm, tc.samples, tc.render)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want one containing %q", err, tc.want)
			}
			if !reflect.DeepEqual(report, CatchRateArmReport{}) {
				t.Errorf("report = %+v, want the zero report", report)
			}
			if tc.sender != nil && tc.sender.calls != tc.wantCalls {
				t.Errorf("sender calls = %d, want %d", tc.sender.calls, tc.wantCalls)
			}
		})
	}
}

func TestSecurityGeneratorSystemPrompt_NamesQuoteFieldsAndNoCase(t *testing.T) {
	for _, want := range []string{"implement-review", "quoted_passage", "document_ref", "verdict", "concerns"} {
		if !strings.Contains(securityCatchGeneratorSystemPrompt, want) {
			t.Errorf("security generator system prompt lacks %q", want)
		}
	}
	m, _ := loadTestSecurityCases(t)
	for _, c := range m {
		for _, p := range c.Input.DefectPhrases {
			if strings.Contains(strings.ToLower(securityCatchGeneratorSystemPrompt), strings.ToLower(p)) {
				t.Errorf("the system prompt carries case %q's defect phrase %q", c.Name, p)
			}
		}
	}
}
