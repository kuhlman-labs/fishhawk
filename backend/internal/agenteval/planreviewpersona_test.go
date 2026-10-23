package agenteval

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionrecord"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
)

// personaTestRepoRoot is the checkout root from this package directory: the
// persona tests read the REAL docs/review/architect-remit.md and
// docs/adr/index.json.
const personaTestRepoRoot = "../../.."

// personaRemitLine is a line of the real architect remit, quoted verbatim by
// the severity-cap test so quote verification passes and cannot mask the
// clamp.
const personaRemitLine = "Reserve category `decision_record` for this lens."

func personaTestPlan(steps []string) map[string]any {
	approach := make([]any, 0, len(steps))
	for i, s := range steps {
		approach = append(approach, map[string]any{"step": i + 1, "description": s})
	}
	return map[string]any{
		"plan_version": "standard_v1",
		"ticket_reference": map[string]any{
			"type": "github_issue", "url": "https://github.com/kuhlman-labs/fishhawk/issues/1", "id": "kuhlman-labs/fishhawk#1",
		},
		"generated_by": map[string]any{"agent": "claude-code", "model": "fixture", "timestamp": "2026-10-01T00:00:00Z"},
		"summary":      "Add a widgets tool.",
		"scope": map[string]any{"files": []any{
			map[string]any{"path": "backend/internal/mcpserver/widgets.go", "operation": "create"},
		}},
		"approach": approach,
		"verification": map[string]any{
			"test_strategy": "Unit tests.",
			"rollback_plan": "Revert the PR.",
			"acceptance_criteria": []any{
				map[string]any{"id": "ac-tool-listed", "statement": "The widgets tool is listed by tools/list.", "source": "explicit"},
			},
		},
		"predicted_runtime_minutes":    10,
		"predicted_runtime_confidence": "medium",
	}
}

// validPersonaCatchCase is a superseded_reliance catch case anchored at
// approach step 2.
func validPersonaCatchCase() map[string]any {
	return map[string]any{
		"persona":     "architect",
		"kind":        "superseded_reliance",
		"expect":      "catch",
		"issue_title": "Add a widgets MCP tool",
		"issue_body":  "Expose widget listing to MCP clients.",
		"plan": personaTestPlan([]string{
			"Add the widgets tool handler.",
			"Serve it over the stdio transport only, per ADR-033.",
			"Register it in the tool table.",
		}),
		"governing_records":   []any{map[string]any{"id": "ADR-033", "status": "accepted", "superseded_by": []any{"ADR-076"}}},
		"defect_phrases":      []any{"stdio transport only"},
		"anchor":              map[string]any{"kind": "approach_step", "step": 2},
		"catch_probes":        []any{"superseded", "no longer governs"},
		"attribution_phrases": []any{"stdio transport"},
		"catching_examples": []any{
			map[string]any{"severity": "medium", "category": "decision_record", "note": "step 2 relies on ADR-033, which is superseded by ADR-076"},
			map[string]any{"severity": "medium", "category": "decision_record", "note": "keeping the stdio transport cites a record that no longer governs"},
		},
		"non_catching_examples": []any{
			map[string]any{"severity": "medium", "category": "decision_record", "note": "step 2 conflicts with ADR-013's licensing boundary"},
			map[string]any{"severity": "low", "category": "acceptance_criteria", "note": "ac-tool-listed is vague"},
		},
	}
}

// validPersonaCleanCase is a clean_area case whose scope matches ADR-002 and
// ADR-013 in the real index.
func validPersonaCleanCase() map[string]any {
	p := personaTestPlan([]string{"Add a ListWidgets sqlc query.", "Expose it from the CLI."})
	p["scope"] = map[string]any{"files": []any{
		map[string]any{"path": "backend/internal/widgets/queries.sql", "operation": "modify"},
		map[string]any{"path": "cli/cmd/fishhawk/widgets.go", "operation": "create"},
	}}
	return map[string]any{
		"persona":     "architect",
		"kind":        "clean_area",
		"expect":      "clean",
		"issue_title": "List widgets from the CLI",
		"issue_body":  "Add a CLI command listing widgets.",
		"plan":        p,
		"governing_records": []any{
			map[string]any{"id": "ADR-002", "status": "accepted", "superseded_by": []any{}},
			map[string]any{"id": "ADR-013", "status": "accepted", "superseded_by": []any{}},
		},
		"defect_phrases":       []any{"sqlc query"},
		"false_alarm_examples": []any{map[string]any{"category": "decision_record", "note": "the query conflicts with ADR-002"}},
		"benign_examples":      []any{map[string]any{"category": "acceptance_criteria", "note": "ac-tool-listed is vague"}},
		"min_matched_records":  2,
	}
}

// personaPair returns an undeclared_direction catch case and its
// declared_direction clean twin.
func personaPair() (undeclared, declared map[string]any) {
	steps := []string{
		"Add a persisted widget_lifecycle state machine shared by the runner and the backend.",
		"Drive its transitions from the runner.",
	}
	undeclared = map[string]any{
		"persona":             "architect",
		"kind":                "undeclared_direction",
		"expect":              "catch",
		"issue_title":         "Track widget lifecycle",
		"issue_body":          "Persist widget lifecycle across components.",
		"plan":                personaTestPlan(steps),
		"governing_records":   []any{map[string]any{"id": "ADR-013", "status": "accepted", "superseded_by": []any{}}},
		"defect_phrases":      []any{"widget_lifecycle state machine"},
		"anchor":              map[string]any{"kind": "approach_step", "step": 1},
		"catch_probes":        []any{"undeclared", "new_architectural_decision"},
		"attribution_phrases": []any{"widget_lifecycle"},
		"catching_examples": []any{
			map[string]any{"category": "decision_record", "note": "step 1 sets an undeclared cross-component direction"},
		},
		"non_catching_examples": []any{
			map[string]any{"category": "decision_record", "note": "step 1 conflicts with ADR-002"},
		},
	}
	dp := personaTestPlan(steps)
	dp["new_architectural_decision"] = map[string]any{
		"rationale": "A shared lifecycle needs one owner.", "related_adrs": []any{}, "decision_summary": "Persist widget lifecycle in the backend.",
	}
	declared = map[string]any{
		"persona":              "architect",
		"kind":                 "declared_direction",
		"expect":               "clean",
		"issue_title":          "Track widget lifecycle",
		"issue_body":           "Persist widget lifecycle across components.",
		"plan":                 dp,
		"governing_records":    []any{map[string]any{"id": "ADR-013", "status": "accepted", "superseded_by": []any{}}},
		"defect_phrases":       []any{"widget_lifecycle state machine"},
		"false_alarm_examples": []any{map[string]any{"category": "decision_record", "note": "an undeclared direction"}},
		"benign_examples":      []any{map[string]any{"category": "scope", "note": "scope is fine"}},
		"pair_of":              "undeclared",
	}
	return undeclared, declared
}

// writePersonaCase writes dir/<name>/persona_case.json. A string is written
// raw; nil writes the case dir with no file.
func writePersonaCase(t *testing.T, dir, name string, v any) {
	t.Helper()
	cdir := filepath.Join(dir, name)
	if err := os.MkdirAll(cdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if v == nil {
		return
	}
	var raw []byte
	switch x := v.(type) {
	case string:
		raw = []byte(x)
	default:
		var err error
		if raw, err = json.MarshalIndent(x, "", "  "); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cdir, "persona_case.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func personaPlanOf(c map[string]any) map[string]any { return c["plan"].(map[string]any) }

func TestLoadPersonaCatchCorpus_Valid(t *testing.T) {
	dir := t.TempDir()
	writePersonaCase(t, dir, "superseded", validPersonaCatchCase())
	writePersonaCase(t, dir, "clean", validPersonaCleanCase())
	u, d := personaPair()
	writePersonaCase(t, dir, "undeclared", u)
	writePersonaCase(t, dir, "declared", d)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("not a case"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases, err := LoadPersonaCatchCorpus(dir, personaTestRepoRoot, ArmArchitect)
	if err != nil {
		t.Fatalf("LoadPersonaCatchCorpus: %v", err)
	}
	var names []string
	for _, c := range cases {
		names = append(names, c.Name)
		if c.Plan == nil || len(c.Raw) == 0 {
			t.Errorf("case %q: plan or raw bytes missing", c.Name)
		}
	}
	if want := []string{"clean", "declared", "superseded", "undeclared"}; !slices.Equal(names, want) {
		t.Errorf("cases = %v, want %v", names, want)
	}
	byName := map[string]PersonaCatchCase{}
	for _, c := range cases {
		byName[c.Name] = c
	}
	if r := byName["superseded"].CatchRule(); r.AnchorStep != 2 || r.Category != "decision_record" || r.Clean || len(r.CriterionIDs) != 0 {
		t.Errorf("catch rule = %+v", r)
	}
	if r := byName["clean"].CatchRule(); !r.Clean || r.Category != "decision_record" {
		t.Errorf("clean rule = %+v", r)
	}
	if byName["clean"].Expect() != CatchExpectClean || byName["superseded"].Expect() != CatchExpectCatch {
		t.Error("Expect misreports the class")
	}
}

func TestLoadPersonaCatchCorpus_CriterionAnchor(t *testing.T) {
	dir := t.TempDir()
	c := validPersonaCatchCase()
	c["anchor"] = map[string]any{"kind": "criterion", "criterion_id": "ac-tool-listed"}
	c["attribution_phrases"] = []any{"listed by tools/list"}
	c["catching_examples"] = []any{map[string]any{"category": "decision_record", "note": "ac-tool-listed is superseded"}}
	c["non_catching_examples"] = []any{map[string]any{"category": "decision_record", "note": "ac-tool-listed conflicts with ADR-013"}}
	writePersonaCase(t, dir, "crit", c)
	cases, err := LoadPersonaCatchCorpus(dir, personaTestRepoRoot, ArmArchitect)
	if err != nil {
		t.Fatalf("LoadPersonaCatchCorpus: %v", err)
	}
	if r := cases[0].CatchRule(); !slices.Equal(r.CriterionIDs, []string{"ac-tool-listed"}) || r.AnchorStep != 0 {
		t.Errorf("criterion rule = %+v", r)
	}
}

// TestLoadPersonaCatchCorpus_FailClosed has one row per loader mode; each
// asserts the case-named error.
func TestLoadPersonaCatchCorpus_FailClosed(t *testing.T) {
	type mutate func(c map[string]any)
	catchRow := func(m mutate) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			c := validPersonaCatchCase()
			m(c)
			writePersonaCase(t, dir, "bad", c)
		}
	}
	cleanRow := func(m mutate) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			c := validPersonaCleanCase()
			m(c)
			writePersonaCase(t, dir, "bad", c)
		}
	}
	pairRow := func(mu, md mutate) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			u, d := personaPair()
			mu(u)
			md(d)
			writePersonaCase(t, dir, "undeclared", u)
			writePersonaCase(t, dir, "bad", d)
		}
	}
	noop := func(map[string]any) {}
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  string
	}{
		{"zero cases", func(*testing.T, string) {}, "holds zero cases"},
		{"missing persona_case.json", func(t *testing.T, dir string) { writePersonaCase(t, dir, "bad", nil) }, "read persona_case.json"},
		{"malformed JSON", func(t *testing.T, dir string) { writePersonaCase(t, dir, "bad", "{") }, "parse persona_case.json"},
		{"unknown field", catchRow(func(c map[string]any) { c["surprise"] = 1 }), "unknown field"},
		{"trailing JSON", func(t *testing.T, dir string) {
			raw, _ := json.Marshal(validPersonaCatchCase())
			writePersonaCase(t, dir, "bad", string(raw)+"{}")
		}, "trailing content"},
		{"persona differs from the arm", catchRow(func(c map[string]any) { c["persona"] = "security" }), `persona "security" is not the "architect" arm's persona`},
		{"unknown kind", catchRow(func(c map[string]any) { c["kind"] = "vibes" }), `unknown kind "vibes"`},
		{"unknown expect", catchRow(func(c map[string]any) { c["expect"] = "maybe" }), `unknown expect "maybe"`},
		{"kind/expect mismatch", catchRow(func(c map[string]any) { c["expect"] = "clean" }), `kind "superseded_reliance" expects "catch"`},
		{"empty issue title", catchRow(func(c map[string]any) { c["issue_title"] = " " }), "issue_title and issue_body must be non-empty"},
		{"invalid plan", catchRow(func(c map[string]any) { delete(personaPlanOf(c), "summary") }), "not a valid standard_v1 plan"},
		{"empty defect_phrases", catchRow(func(c map[string]any) { delete(c, "defect_phrases") }), "defect_phrases must be non-empty"},
		{"blank defect phrase", catchRow(func(c map[string]any) { c["defect_phrases"] = []any{" "} }), "defect_phrases[0] is empty"},
		{"defect phrase absent from issue and plan", catchRow(func(c map[string]any) { c["defect_phrases"] = []any{"zeppelin hangar"} }), `defect phrase "zeppelin hangar" occurs in neither`},
		{"empty governing_records", catchRow(func(c map[string]any) { c["governing_records"] = []any{} }), "governing_records must be non-empty"},
		{"governing record absent from the real index", catchRow(func(c map[string]any) {
			c["governing_records"] = []any{map[string]any{"id": "ADR-9999", "status": "accepted", "superseded_by": []any{}}}
		}), "governing record ADR-9999 is absent from the real index"},
		{"governing record status differs", catchRow(func(c map[string]any) {
			c["governing_records"] = []any{map[string]any{"id": "ADR-033", "status": "superseded", "superseded_by": []any{"ADR-999"}}}
		}), `governing record ADR-033: the case declares status "superseded", the real index says "accepted"`},
		{"governing record superseded_by differs", catchRow(func(c map[string]any) {
			c["governing_records"] = []any{map[string]any{"id": "ADR-033", "status": "accepted", "superseded_by": []any{"ADR-999"}}}
		}), "governing record ADR-033: the case declares superseded_by [ADR-999]"},
		// Status and superseded_by agree with the real index; only amended_by
		// differs, so only the amended_by comparison can refuse it.
		{"governing record amended_by differs", catchRow(func(c map[string]any) {
			c["governing_records"] = []any{map[string]any{"id": "ADR-033", "status": "accepted", "superseded_by": []any{"ADR-076"}, "amended_by": []any{"ADR-999"}}}
		}), "governing record ADR-033: the case declares amended_by [ADR-999], the real index says []"},
		{"clean-only field on a catch case", catchRow(func(c map[string]any) { c["min_matched_records"] = 1 }), "a catch case must not carry clean-case fields"},
		{"catch-only field on a clean case", cleanRow(func(c map[string]any) { c["catch_probes"] = []any{"x"} }), "a clean case must not carry catch-case fields"},
		{"absent anchor", catchRow(func(c map[string]any) { delete(c, "anchor") }), "a catch case needs an anchor"},
		{"unknown anchor kind", catchRow(func(c map[string]any) { c["anchor"] = map[string]any{"kind": "file"} }), `unknown anchor kind "file"`},
		{"anchored step absent from the plan", catchRow(func(c map[string]any) { c["anchor"] = map[string]any{"kind": "approach_step", "step": 9} }), "anchored approach step 9 is absent from the plan"},
		{"anchored criterion absent from the plan", catchRow(func(c map[string]any) {
			c["anchor"] = map[string]any{"kind": "criterion", "criterion_id": "ac-nope"}
		}), `anchored criterion "ac-nope" is absent from the plan`},
		{"approach_step anchor with a criterion id", catchRow(func(c map[string]any) {
			c["anchor"] = map[string]any{"kind": "approach_step", "step": 2, "criterion_id": "ac-tool-listed"}
		}), "an approach_step anchor needs step >= 1 and no criterion_id"},
		{"criterion anchor with a step", catchRow(func(c map[string]any) {
			c["anchor"] = map[string]any{"kind": "criterion", "step": 2, "criterion_id": "ac-tool-listed"}
		}), "a criterion anchor needs a criterion_id and no step"},
		{"empty catch_probes", catchRow(func(c map[string]any) { c["catch_probes"] = []any{} }), "catch_probes and attribution_phrases must be non-empty"},
		{"blank probe", catchRow(func(c map[string]any) { c["catch_probes"] = []any{"superseded", " "} }), "catch_probes[1] is empty"},
		{"blank attribution phrase", catchRow(func(c map[string]any) { c["attribution_phrases"] = []any{" "} }), "attribution_phrases[0] is empty"},
		{"attribution phrase absent from the anchored step", catchRow(func(c map[string]any) { c["attribution_phrases"] = []any{"tool table"} }), `attribution phrase "tool table" is absent from the anchored approach_step`},
		{"attribution phrase in another step", catchRow(func(c map[string]any) { c["attribution_phrases"] = []any{"the"} }), `attribution phrase "the" also occurs in approach step 1`},
		{"attribution phrase in a criterion statement", catchRow(func(c map[string]any) {
			personaPlanOf(c)["approach"].([]any)[1].(map[string]any)["description"] = "Serve it over the stdio transport only, per ADR-033; it is listed by tools/list."
			c["attribution_phrases"] = []any{"listed by tools/list"}
		}), `attribution phrase "listed by tools/list" also occurs in criterion "ac-tool-listed"`},
		{"empty catching_examples", catchRow(func(c map[string]any) { c["catching_examples"] = []any{} }), "catching_examples and non_catching_examples must both be non-empty"},
		{"catching example not caught (wrong category)", catchRow(func(c map[string]any) {
			c["catching_examples"] = []any{map[string]any{"category": "acceptance_criteria", "note": "step 2 relies on a superseded record"}}
		}), "catching_examples[0]"},
		{"non_catching example caught", catchRow(func(c map[string]any) {
			c["non_catching_examples"] = []any{map[string]any{"category": "decision_record", "note": "step 2 is superseded"}}
		}), "non_catching_examples[0] (\"step 2 is superseded\") is caught"},
		{"probe matches a non_catching example (v4 mode j)", catchRow(func(c map[string]any) {
			c["non_catching_examples"] = []any{
				map[string]any{"category": "acceptance_criteria", "note": "step 2 is superseded"},
				map[string]any{"category": "decision_record", "note": "step 2 conflicts with ADR-013"},
			}
		}), `catch probe "superseded" matches non_catching_examples[0]`},
		{"no attributed non_catching example (v4 mode n)", catchRow(func(c map[string]any) {
			c["non_catching_examples"] = []any{map[string]any{"category": "acceptance_criteria", "note": "ac-tool-listed is vague"}}
		}), "no non_catching_example names the anchor"},
		{"probe in the plan text (v4 mode m)", catchRow(func(c map[string]any) {
			c["catch_probes"] = []any{"superseded", "no longer governs", "per adr-033"}
		}), `catch probe "per adr-033" occurs in the issue or plan`},
		{"empty false_alarm_examples", cleanRow(func(c map[string]any) { c["false_alarm_examples"] = []any{} }), "false_alarm_examples and benign_examples must both be non-empty"},
		{"false_alarm example not a false alarm", cleanRow(func(c map[string]any) {
			c["false_alarm_examples"] = []any{map[string]any{"category": "scope", "note": "x"}}
		}), "false_alarm_examples[0]"},
		{"benign example is a false alarm", cleanRow(func(c map[string]any) {
			c["benign_examples"] = []any{map[string]any{"category": "decision_record", "note": "x"}}
		}), "benign_examples[0]"},
		{"clean_area without min_matched_records", cleanRow(func(c map[string]any) { delete(c, "min_matched_records") }), "a clean_area case needs min_matched_records >= 1"},
		{"clean_area matching fewer records than declared", cleanRow(func(c map[string]any) { c["min_matched_records"] = 5 }), "the plan scope matches 2 index records, fewer than min_matched_records 5"},
		{"pair_of on a clean_area case", cleanRow(func(c map[string]any) { c["pair_of"] = "x" }), "pair_of applies only to a declared_direction case"},
		{"declared_direction without pair_of", pairRow(noop, func(d map[string]any) { delete(d, "pair_of") }), "a declared_direction case needs pair_of"},
		{"min_matched_records on a declared_direction case", pairRow(noop, func(d map[string]any) { d["min_matched_records"] = 1 }), "min_matched_records applies only to a clean_area case"},
		{"pair_of names an absent case", pairRow(noop, func(d map[string]any) { d["pair_of"] = "ghost" }), `pair_of "ghost" names no case in the corpus`},
		{"pair_of names a case of the wrong kind", pairRow(func(u map[string]any) { u["kind"] = "accepted_conflict" }, noop), "a pair must be undeclared_direction (catch) vs declared_direction (clean)"},
		{"paired plans differ outside the declaration", pairRow(noop, func(d map[string]any) {
			personaPlanOf(d)["approach"].([]any)[1].(map[string]any)["description"] = "Drive its transitions from the backend."
		}), "the two plans differ in a field other than new_architectural_decision"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			_, err := LoadPersonaCatchCorpus(dir, personaTestRepoRoot, ArmArchitect)
			if err == nil {
				t.Fatalf("LoadPersonaCatchCorpus accepted the corpus, want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
			if tc.name != "zero cases" && !strings.Contains(err.Error(), `persona catch case "bad"`) {
				t.Errorf("error does not name the case: %v", err)
			}
		})
	}
	t.Run("absent dir", func(t *testing.T) {
		_, err := LoadPersonaCatchCorpus(filepath.Join(t.TempDir(), "nope"), personaTestRepoRoot, ArmArchitect)
		if err == nil || !strings.Contains(err.Error(), "is absent") {
			t.Errorf("error = %v, want an absent-dir error", err)
		}
	})
	t.Run("no real index under repoRoot", func(t *testing.T) {
		dir := t.TempDir()
		writePersonaCase(t, dir, "ok", validPersonaCatchCase())
		_, err := LoadPersonaCatchCorpus(dir, t.TempDir(), ArmArchitect)
		if err == nil || !strings.Contains(err.Error(), "read the real decision-record index") {
			t.Errorf("error = %v, want an index-read error", err)
		}
	})
	t.Run("an unparseable index", func(t *testing.T) {
		dir, root := t.TempDir(), t.TempDir()
		writePersonaCase(t, dir, "ok", validPersonaCatchCase())
		if err := os.MkdirAll(filepath.Join(root, "docs", "adr"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "docs", "adr", "index.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadPersonaCatchCorpus(dir, root, ArmArchitect)
		if err == nil || !strings.Contains(err.Error(), "parse the real decision-record index") {
			t.Errorf("error = %v, want an index-parse error", err)
		}
	})
}

func TestFSFetcher_RefusesOtherRef(t *testing.T) {
	f := personaFSFetcher{root: personaTestRepoRoot}
	ctx := context.Background()
	if _, err := f.FetchFile(ctx, forge.CredentialScope{}, forge.RepoRef{}, personaArchitectRemitPath, "main"); err == nil || !strings.Contains(err.Error(), "reads only at the fixture commit") {
		t.Errorf("FetchFile at ref main = %v, want a refusal", err)
	}
	got, err := f.FetchFile(ctx, forge.CredentialScope{}, forge.RepoRef{}, personaArchitectRemitPath, personaFixtureCommit)
	if err != nil || len(got.Content) == 0 {
		t.Fatalf("FetchFile at the fixture commit = %v, %v", got, err)
	}
	if _, err := f.FetchFile(ctx, forge.CredentialScope{}, forge.RepoRef{}, "docs/no-such.md", personaFixtureCommit); !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("a missing file = %v, want forge.ErrNotFound", err)
	}
	if _, err := (personaFSFetcher{root: "/dev/null"}).FetchFile(ctx, forge.CredentialScope{}, forge.RepoRef{}, "x/y.md", personaFixtureCommit); err == nil || errors.Is(err, forge.ErrNotFound) {
		t.Errorf("a non-ENOENT read error = %v, want a wrapped non-not-found error", err)
	}
	_, err = personaResolver(personaTestRepoRoot).Resolve(ctx, repodoc.Request{
		BaseRef:     personaFixtureCommit,
		Declaration: repodoc.Declaration{Path: "docs/no-such.md", DeclarationSite: "test", Base: repodoc.BaseSourceRunAdmission},
	})
	if !errors.Is(err, repodoc.ErrMissingDocument) {
		t.Errorf("a missing file through the resolver = %v, want repodoc.ErrMissingDocument", err)
	}
	if r := personaResolver(personaTestRepoRoot); r.CapBytes() != repodoc.DefaultMaxBytes {
		t.Errorf("resolver cap = %d, want the production DefaultMaxBytes %d", r.CapBytes(), repodoc.DefaultMaxBytes)
	}
}

func loadTestPersonaCases(t *testing.T) map[string]PersonaCatchCase {
	t.Helper()
	dir := t.TempDir()
	writePersonaCase(t, dir, "superseded", validPersonaCatchCase())
	writePersonaCase(t, dir, "clean", validPersonaCleanCase())
	cases, err := LoadPersonaCatchCorpus(dir, personaTestRepoRoot, ArmArchitect)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]PersonaCatchCase{}
	for _, c := range cases {
		out[c.Name] = c
	}
	return out
}

func TestPersonaArmPrompt_RendersThroughTheProductionBuilder(t *testing.T) {
	cases := loadTestPersonaCases(t)
	r, err := PersonaArmPrompt(cases["clean"], personaTestRepoRoot)
	if err != nil {
		t.Fatalf("PersonaArmPrompt: %v", err)
	}
	for _, want := range []string{"Reviewer persona remit: architect", personaRemitLine, "ADR-033", "kuhlman-labs/fishhawk"} {
		if !strings.Contains(r.Prompt, want) {
			t.Errorf("persona prompt lacks %q", want)
		}
	}
	if r.Selection == nil || len(r.QuoteDocs) < 2 {
		t.Fatalf("selection %v, %d quote docs: want the remit and the index", r.Selection, len(r.QuoteDocs))
	}
	if r.QuoteDocs[0].Path != personaArchitectRemitPath || r.QuoteDocs[0].Commit != personaFixtureCommit {
		t.Errorf("first quote doc = %s@%s, want the remit at the fixture commit", r.QuoteDocs[0].Path, r.QuoteDocs[0].Commit)
	}
	for _, id := range []string{"ADR-002", "ADR-013"} {
		if !slices.Contains(r.MatchedIDs, id) {
			t.Errorf("matched ids %v lack %s", r.MatchedIDs, id)
		}
	}
	if len(r.MatchedIDs) != len(r.IncludedIDs)+len(r.DroppedIDs) {
		t.Errorf("matched %v != included %v + dropped %v", r.MatchedIDs, r.IncludedIDs, r.DroppedIDs)
	}
	t.Logf("selection: included %v dropped %v", r.IncludedIDs, r.DroppedIDs)

	t.Run("a missing remit fails the render", func(t *testing.T) {
		if _, err := PersonaArmPrompt(cases["clean"], t.TempDir()); err == nil || !strings.Contains(err.Error(), "resolve remit") {
			t.Errorf("render under an empty root = %v, want a remit error", err)
		}
	})
	t.Run("a missing index fails the render", func(t *testing.T) {
		root := t.TempDir()
		raw := mustReadCatch(t, filepath.Join(personaTestRepoRoot, personaArchitectRemitPath))
		if err := os.MkdirAll(filepath.Join(root, "docs", "review"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(personaArchitectRemitPath)), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := PersonaArmPrompt(cases["clean"], root); err == nil || !strings.Contains(err.Error(), "assemble decision record") {
			t.Errorf("render without an index = %v, want an assembly error", err)
		}
	})
}

func freshToken(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "tok-" + hex.EncodeToString(b)
}

// TestClassifyPersonaTrial_RunsIngestControls: a medium decision_record
// concern quoting a fresh random token is stored low with quote_unverified,
// and a high concern quoting the remit verbatim is stored medium with
// severity_clamped_from high.
func TestClassifyPersonaTrial_RunsIngestControls(t *testing.T) {
	cases := loadTestPersonaCases(t)
	r, err := PersonaArmPrompt(cases["superseded"], personaTestRepoRoot)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := json.Marshal(map[string]any{"verdict": "approve_with_concerns", "concerns": []any{
		map[string]any{"severity": "medium", "category": "decision_record", "note": "fabricated", "quoted_passage": freshToken(t), "document_ref": personaArchitectRemitPath},
		map[string]any{"severity": "high", "category": "decision_record", "note": "verbatim", "quoted_passage": personaRemitLine, "document_ref": personaArchitectRemitPath, "quote_unverified": true, "reviewer_role": "forged"},
		// No quote, so verification leaves it alone: only the marker scrub
		// clears the forged quote_unverified.
		map[string]any{"severity": "low", "category": "decision_record", "note": "forged marker", "quote_unverified": true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	trial := ClassifyPersonaTrial(string(resp), CatchRule{Clean: true, Category: "decision_record"}, r.QuoteDocs, personaArchitectSeverityCap)
	if trial.Outcome != CatchFalseAlarm || len(trial.Concerns) != 3 {
		t.Fatalf("trial = %+v, want a false alarm storing all three concerns", trial)
	}
	fab, verb, forged := trial.Concerns[0], trial.Concerns[1], trial.Concerns[2]
	if forged.QuoteUnverified || forged.Severity != "low" {
		t.Errorf("forged-marker concern = %+v, want the reviewer-set quote_unverified scrubbed", forged)
	}
	if fab.Severity != "low" || !fab.QuoteUnverified || fab.SeverityClampedFrom != "medium" || fab.DocumentRef != personaArchitectRemitPath {
		t.Errorf("fabricated-quote concern = %+v, want low, quote_unverified, clamped from medium", fab)
	}
	if verb.Severity != "medium" || verb.SeverityClampedFrom != "high" || verb.QuoteUnverified {
		t.Errorf("verbatim-quote concern = %+v, want medium clamped from high, quote verified (the forged marker scrubbed)", verb)
	}
}

// TestClassifyPersonaTrialForRound_RunsConventionClamp (CF1): ONE medium
// repo_convention concern with no quote. With NIL caps (no convention
// rendered) the convention clamp stores it low, clamped from medium; the
// persona's medium cap would never lower a medium concern, so only the clamp
// can. With the convention rendered at medium it keeps medium; at low it is
// held to low. ClassifyPersonaTrial is the nil-caps round.
func TestClassifyPersonaTrialForRound_RunsConventionClamp(t *testing.T) {
	resp := `{"verdict":"approve_with_concerns","concerns":[{"severity":"medium","category":"repo_convention","convention":"security-conventions","note":"breaks a convention"}]}`
	rule := CatchRule{Clean: true}
	tests := []struct {
		name     string
		caps     planreview.ConventionCaps
		sev      string
		clampedF string
	}{
		{"nil caps", nil, "low", "medium"},
		{"convention rendered at medium", planreview.ConventionCaps{"security-conventions": "medium"}, "medium", ""},
		{"convention rendered at low", planreview.ConventionCaps{"security-conventions": "low"}, "low", "medium"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			trial := ClassifyPersonaTrialForRound(resp, rule, nil, "medium", tc.caps)
			if trial.Outcome != CatchFalseAlarm || len(trial.Concerns) != 1 {
				t.Fatalf("trial = %+v, want one stored false alarm", trial)
			}
			if got := trial.Concerns[0]; got.Severity != tc.sev || got.SeverityClampedFrom != tc.clampedF {
				t.Errorf("stored concern = %+v, want severity %q clamped from %q", got, tc.sev, tc.clampedF)
			}
		})
	}
	if got := ClassifyPersonaTrial(resp, rule, nil, "medium").Concerns[0]; got.Severity != "low" || got.SeverityClampedFrom != "medium" {
		t.Errorf("ClassifyPersonaTrial stored %+v, want the nil-caps clamp to low", got)
	}
	// A non-convention category is never touched by the clamp.
	plain := ClassifyPersonaTrialForRound(`{"verdict":"approve_with_concerns","concerns":[{"severity":"medium","category":"decision_record","note":"x"}]}`, rule, nil, "medium", nil)
	if got := plain.Concerns[0]; got.Severity != "medium" || got.SeverityClampedFrom != "" {
		t.Errorf("decision_record concern = %+v, want untouched", got)
	}
}

func TestClassifyPersonaTrial_Undecodable(t *testing.T) {
	catchRule := CatchRule{Probes: []string{"superseded"}, AnchorStep: 2, Category: "decision_record"}
	for _, resp := range []string{"not json", `{"verdict":"maybe","concerns":[]}`, `{"verdict":"approve",`} {
		trial := ClassifyPersonaTrial(resp, catchRule, nil, personaArchitectSeverityCap)
		if trial.Outcome != CatchUndecodable || trial.Response != resp {
			t.Errorf("ClassifyPersonaTrial(%q) = %+v, want undecodable carrying the response", resp, trial)
		}
	}
	caught := ClassifyPersonaTrial("Verdict:\n```json\n"+`{"verdict":"reject","concerns":[{"severity":"medium","category":"decision_record","note":"step 2 is superseded"}]}`+"\n```", catchRule, nil, "")
	if caught.Outcome != CatchCaught || caught.Attribution != CatchAttributionStep {
		t.Errorf("a fenced persona verdict = %+v, want caught by step", caught)
	}
}

// fakePersonaSender returns resp (or err) and records its calls.
type fakePersonaSender struct {
	resp    string
	err     error
	calls   int
	systems []string
}

func (f *fakePersonaSender) Messages(_ context.Context, system, _ string) (string, string, int, int, int, int, error) {
	f.calls++
	f.systems = append(f.systems, system)
	if f.err != nil {
		return "", "", 0, 0, 0, 0, f.err
	}
	return f.resp, "fake-model", 0, 0, 0, 0, nil
}

func TestRunPersonaCatchArm_Counts(t *testing.T) {
	m := loadTestPersonaCases(t)
	cases := []PersonaCatchCase{m["clean"], m["superseded"]}
	sender := &fakePersonaSender{resp: `{"verdict":"approve_with_concerns","concerns":[{"severity":"medium","category":"decision_record","note":"step 2 relies on a superseded record"}]}`}
	report, err := RunPersonaCatchArm(context.Background(), sender, cases, personaTestRepoRoot, 3)
	if err != nil {
		t.Fatalf("RunPersonaCatchArm: %v", err)
	}
	if sender.calls != 6 {
		t.Errorf("sender calls = %d, want 6", sender.calls)
	}
	for _, s := range sender.systems {
		if s != personaCatchGeneratorSystemPrompt {
			t.Fatalf("system prompt = %q, want the persona generator system prompt", s)
		}
	}
	if report.Arm != ArmArchitect || report.SamplesPerCase != 3 {
		t.Errorf("report header = %q/%d", report.Arm, report.SamplesPerCase)
	}
	if got, want := report.PerCase["superseded"], (CaseCatchCounts{Expect: CatchExpectCatch, Trials: 3, Caught: 3}); got != want {
		t.Errorf("catch counts = %+v, want %+v", got, want)
	}
	if got, want := report.PerCase["clean"], (CaseCatchCounts{Expect: CatchExpectClean, Trials: 3, FalseAlarms: 3}); got != want {
		t.Errorf("clean counts = %+v, want %+v", got, want)
	}
	if len(report.Trials["clean"]) != 3 || len(report.Trials["superseded"]) != 3 {
		t.Errorf("trials not kept per sample: %d/%d", len(report.Trials["clean"]), len(report.Trials["superseded"]))
	}
	for name, c := range report.PerCase {
		if err := c.validate(); err != nil {
			t.Errorf("case %q counts invalid: %v", name, err)
		}
	}

	t.Run("undecodable counts per class", func(t *testing.T) {
		s := &fakePersonaSender{resp: "not json"}
		rep, err := RunPersonaCatchArm(context.Background(), s, cases, personaTestRepoRoot, 2)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := rep.PerCase["superseded"], (CaseCatchCounts{Expect: CatchExpectCatch, Trials: 2, Undecodable: 2}); got != want {
			t.Errorf("catch counts = %+v, want %+v (undecodable is a miss)", got, want)
		}
		if got, want := rep.PerCase["clean"], (CaseCatchCounts{Expect: CatchExpectClean, Trials: 2, Undecodable: 2}); got != want {
			t.Errorf("clean counts = %+v, want %+v (undecodable counts toward the false-alarm rate)", got, want)
		}
	})
}

func TestRunPersonaCatchArm_FailsClosed(t *testing.T) {
	m := loadTestPersonaCases(t)
	cases := []PersonaCatchCase{m["superseded"]}
	ok := `{"verdict":"approve"}`
	boom := errors.New("transport down")
	failRender := func(PersonaCatchCase, string) (PersonaArmRender, error) {
		return PersonaArmRender{}, errors.New("render exploded")
	}
	tests := []struct {
		name      string
		sender    *fakePersonaSender
		nilSender bool
		cases     []PersonaCatchCase
		samples   int
		render    personaArmRenderer
		want      string
		wantCalls int
	}{
		{"no cases", &fakePersonaSender{resp: ok}, false, nil, 1, PersonaArmPrompt, "no cases", 0},
		{"samples < 1", &fakePersonaSender{resp: ok}, false, cases, 0, PersonaArmPrompt, "samples must be >= 1", 0},
		{"nil sender", nil, true, cases, 1, PersonaArmPrompt, "sender is required", 0},
		{"render error", &fakePersonaSender{resp: ok}, false, cases, 1, failRender, "render exploded", 0},
		{"transport error", &fakePersonaSender{err: boom}, false, cases, 2, PersonaArmPrompt, "transport down", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var sender MessageSender
			if !tc.nilSender {
				sender = tc.sender
			}
			report, err := runPersonaCatchArm(context.Background(), sender, tc.cases, personaTestRepoRoot, tc.samples, tc.render)
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

func TestSamplesPerCaseForPersona_Derived(t *testing.T) {
	tests := []struct{ nCatch, nClean, want int }{
		{3, 3, 46},
		{3, 0, 46},
		{0, 4, 34},
		{4, 2, 68},
		{0, 0, 0},
		{-1, 3, 0},
	}
	for _, tc := range tests {
		if got := SamplesPerCaseForPersona(tc.nCatch, tc.nClean); got != tc.want {
			t.Errorf("SamplesPerCaseForPersona(%d, %d) = %d, want %d", tc.nCatch, tc.nClean, got, tc.want)
		}
	}
	// Each present class clears the floor at the derived count.
	if n := SamplesPerCaseForPersona(3, 3); 3*n < MinCatchRateTrialsPerArm {
		t.Errorf("3 cases x %d samples = %d < the %d floor", n, 3*n, MinCatchRateTrialsPerArm)
	}
}

func TestPersonaGeneratorSystemPrompt_NamesQuoteFields(t *testing.T) {
	for _, want := range []string{"quoted_passage", "document_ref", "verdict", "concerns"} {
		if !strings.Contains(personaCatchGeneratorSystemPrompt, want) {
			t.Errorf("persona generator system prompt lacks %q", want)
		}
	}
}

// TestPersonaArmPrompt_GoverningIndexEntriesSurviveTruncation is an early
// tripwire for approval condition C1: decisionrecord.Assemble truncates an
// index whose compact rendering (decisionrecord.RenderIndex, #4478) is over the
// DefaultMaxBytes cap, which would silently drop the tail entries the persona
// cases rest on. The
// in-test superseded case rests on ADR-033 and its successor ADR-076; both
// must appear in the RENDERED, post-truncation index text. ADR-094 is a later
// index entry that no committed case rests on, kept here as a further
// truncation sentinel; it is not free to drop, because its presence is also a
// premise of TestPersonaCatchCorpus_NotLeakedIntoPersonaPrompt and of the
// adr006-production-migrate-down case.md. The committed corpus's governing
// records and their successors (ids and titles) are asserted per case by
// TestPersonaCatchCorpus_RenderContainsRemitAndSelectionInEnvelopes.
func TestPersonaArmPrompt_GoverningIndexEntriesSurviveTruncation(t *testing.T) {
	cases := loadTestPersonaCases(t)
	r, err := PersonaArmPrompt(cases["superseded"], personaTestRepoRoot)
	if err != nil {
		t.Fatal(err)
	}
	var indexText string
	for _, d := range r.QuoteDocs {
		if d.Path == personaArchitectIndexPath {
			indexText, _ = repodoc.InjectedContent(d)
		}
	}
	if indexText == "" {
		t.Fatalf("no rendered index text among the quote docs %v", r.QuoteDocs)
	}
	for _, id := range []string{"ADR-033", "ADR-076", "ADR-094"} {
		if !strings.Contains(indexText, `"id":"`+id+`"`) {
			t.Errorf("the rendered index lacks %s's entry: an index truncation dropped a record the persona cases depend on", id)
		}
	}
	t.Logf("index rendered %d bytes (cap %d)", r.Selection.Index.RenderedBytes, r.Selection.CapBytes)
}

// jsonEncodedTitle is title as decisionrecord.RenderIndex writes it inside the
// compact index rendering: capped by decisionrecord.RenderedTitle (#4699; a
// title over decisionrecord.MaxRenderedTitleChars is cut, so the full stored
// title is not in the rendering), then JSON-string encoded with
// SetEscapeHTML(false), the surrounding quotes stripped.
func jsonEncodedTitle(t *testing.T, title string) string {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(decisionrecord.RenderedTitle(title)); err != nil {
		t.Fatalf("encode title %q: %v", title, err)
	}
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSuffix(b.String(), "\n"), `"`), `"`)
}
