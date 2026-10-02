package spec

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"testing"
)

// The CLI half of the reviewer_personas parity proof (ADR-084 / E55.8 /
// #3753). The per-rule rows live in the shared corpus
// docs/spec/review-conventions-fixtures.json (the persona-* rows), run here by
// TestReviewConventionsCorpus_CLIMatches and in backend/internal/spec by
// TestReviewConventionsCorpus_BackendMatches. This file holds the message
// constants byte-identical to the backend's and pins the collect-mode shape a
// one-rule corpus row cannot.

// rpConstDecl matches a single-line exported reviewer-persona message or path
// constant declaration.
var rpConstDecl = regexp.MustCompile(`(?m)^const ((?:Msg|MsgFmt|PathFmt)\w*ReviewerPersona\w*) = (".*")$`)

// TestReviewerPersonasMessageParity holds every reviewer-persona message and
// path constant BYTE-IDENTICAL across the two modules (the
// TestReviewConventionsMessageParity idiom): the declaration set is extracted
// from BOTH reviewer_personas.go files, must be the same set, each declaration
// must be the same line, and each CLI constant's VALUE must be what that line
// declares. DeclarationSiteFmtReviewerPersonaRemit is backend-only (the CLI
// resolves no remit) and deliberately outside the regex.
func TestReviewerPersonasMessageParity(t *testing.T) {
	read := func(path string) map[string]string {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		out := map[string]string{}
		for _, m := range rpConstDecl.FindAllStringSubmatch(string(src), -1) {
			out[m[1]] = m[0]
		}
		return out
	}
	backend := read("../../../backend/internal/spec/reviewer_personas.go")
	cli := read("reviewer_personas.go")

	values := map[string]string{
		"PathFmtReviewerPersona":                PathFmtReviewerPersona,
		"PathFmtStageReviewerPersonas":          PathFmtStageReviewerPersonas,
		"PathFmtStageReviewerPersonaItem":       PathFmtStageReviewerPersonaItem,
		"MsgFmtReviewerPersonaRemitPathInvalid": MsgFmtReviewerPersonaRemitPathInvalid,
		"MsgFmtReviewerPersonaStageType":        MsgFmtReviewerPersonaStageType,
		"MsgFmtReviewerPersonaUnknown":          MsgFmtReviewerPersonaUnknown,
		"MsgFmtReviewerPersonaNoAgents":         MsgFmtReviewerPersonaNoAgents,
		"MsgFmtReviewerPersonaUnreferenced":     MsgFmtReviewerPersonaUnreferenced,
	}
	if len(backend) != len(values) || len(cli) != len(values) {
		t.Fatalf("extracted %d backend / %d cli declarations, want %d each — a constant was added, dropped, or made multi-line on one side", len(backend), len(cli), len(values))
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		want := `const ` + name + ` = ` + strconv.Quote(values[name])
		if cli[name] != want {
			t.Errorf("cli reviewer_personas.go: %s is not declared as the single line\n%s", name, want)
		}
		if backend[name] != want {
			t.Errorf("backend/internal/spec/reviewer_personas.go does not declare %s verbatim;\nwant the line: %s\n got: %s\n"+
				"The two modules cannot share a constant, so the copies must stay byte-identical or `fishhawk validate` and the backend report different text for the same spec error.",
				name, want, backend[name])
		}
	}
}

// TestReviewerPersonas_CollectsOneEntryPerSiteInBackendOrder pins the
// collect-mode shape: each persona and each stage reports at most ONE entry,
// persona declarations follow the review_conventions declarations, the stage
// rung follows the conventions stage rung, and the persona reference rung
// follows the conventions reference rung — so the list leads with the error
// the backend returns first.
func TestReviewerPersonas_CollectsOneEntryPerSiteInBackendOrder(t *testing.T) {
	doc := `version: "2"
review_conventions:
  backend:
    path: /b.md
  unused:
    path: docs/unused.md
reviewer_personas:
  zeta:
    agent:
      provider: codex
      agent_version: bogus
    remit:
      path: /z.md
  alpha:
    agent:
      provider: codex
      agent_version: bogus
    remit:
      path: docs/a.md
  orphan:
    agent:
      provider: anthropic
    remit:
      path: docs/o.md
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agents:
            - provider: anthropic
          conventions: [backend, missing]
          personas: [alpha, zeta, ghost, spook]
`
	entries := rcEntries(t, ValidateBytes([]byte(doc)))
	want := []ValidationErrorEntry{
		{Path: "/review_conventions/backend/path", Message: fmt.Sprintf(MsgFmtReviewConventionPathInvalid, "backend", "/b.md", MsgReviewConventionPathAbsolute)},
		{Path: "/reviewer_personas/alpha/agent/agent_version", Message: `agent_version comparator "bogus" must start with one of >=, >, <=, <, =, ==`},
		// zeta violates BOTH declaration rules; only the first (remit path) is reported.
		{Path: "/reviewer_personas/zeta/remit/path", Message: fmt.Sprintf(MsgFmtReviewerPersonaRemitPathInvalid, "zeta", "/z.md", MsgReviewConventionPathAbsolute)},
		{Path: "/workflows/feature_change/stages/0/reviewers/conventions/1", Message: fmt.Sprintf(MsgFmtReviewConventionUnknown, "plan", "missing", "missing")},
		// Two undeclared attachments; only the first is reported for the stage.
		{Path: "/workflows/feature_change/stages/0/reviewers/personas/2", Message: fmt.Sprintf(MsgFmtReviewerPersonaUnknown, "plan", "ghost", "ghost")},
		{Path: "/review_conventions/unused", Message: fmt.Sprintf(MsgFmtReviewConventionUnreferenced, "unused")},
		{Path: "/reviewer_personas/orphan", Message: fmt.Sprintf(MsgFmtReviewerPersonaUnreferenced, "orphan")},
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d:\n%v", len(entries), len(want), entries)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entry[%d] = %+v\nwant %+v", i, entries[i], want[i])
		}
	}
}

// TestReviewerPersonas_ShapeTolerance pins the skip-on-shape-mismatch branches
// the schema keeps unreachable from ValidateBytes, by driving the checks over
// hand-built raw trees: each yields no entry rather than a panic or an
// invented rejection.
func TestReviewerPersonas_ShapeTolerance(t *testing.T) {
	var errs []ValidationErrorEntry
	checkReviewerPersonaDeclarations(map[string]any{"reviewer_personas": []any{"x"}}, &errs)
	checkReviewerPersonaDeclarations(map[string]any{"reviewer_personas": map[string]any{
		"a": "not-a-map",
		"b": map[string]any{"remit": "not-a-map", "agent": "not-a-map"},
		"c": map[string]any{"remit": map[string]any{"path": 7}, "agent": map[string]any{"agent_version": 7}},
	}}, &errs)
	checkStageReviewerPersonas(map[string]any{"reviewers": "not-a-map"}, "wf", 0, nil, &errs)
	checkStageReviewerPersonas(map[string]any{"reviewers": map[string]any{"personas": []any{7}}}, "wf", 0, nil, &errs)
	checkReviewerPersonasReferenced(map[string]any{"reviewer_personas": "not-a-map"}, &errs)
	if len(errs) != 0 {
		t.Fatalf("shape-mismatched nodes produced entries: %+v", errs)
	}

	// A declared persona with no readable workflows is unattached — the
	// reference rung still runs, as the backend's does.
	checkReviewerPersonasReferenced(map[string]any{"reviewer_personas": map[string]any{
		"a": map[string]any{"remit": map[string]any{"path": "docs/a.md"}},
	}}, &errs)
	if len(errs) != 1 || errs[0].Path != "/reviewer_personas/a" {
		t.Fatalf("unattached with no workflows: got %+v, want one entry at /reviewer_personas/a", errs)
	}

	// A stage attaching a name when NO reviewer_personas map is declared:
	// every attached name is undeclared (declared is nil).
	errs = nil
	checkStageReviewerPersonas(map[string]any{
		"id": "plan", "type": "plan",
		"reviewers": map[string]any{"agents": []any{map[string]any{"provider": "anthropic"}}, "personas": []any{"security"}},
	}, "wf", 0, nil, &errs)
	if len(errs) != 1 || errs[0].Path != "/workflows/wf/stages/0/reviewers/personas/0" {
		t.Fatalf("attachment with no declared map: got %+v, want one entry at /workflows/wf/stages/0/reviewers/personas/0", errs)
	}
}
