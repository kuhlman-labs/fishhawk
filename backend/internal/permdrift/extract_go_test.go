package permdrift

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type goExtractor func([]byte) (GoExtraction, error)

func mustGo(t *testing.T, ex goExtractor, src string) GoExtraction {
	t.Helper()
	x, err := ex([]byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return x
}

// diffGo extracts both sides, asserts neither holds an unresolved construct,
// and compares.
func diffGo(t *testing.T, ex goExtractor, base, head string) []Change {
	t.Helper()
	b, h := mustGo(t, ex, base), mustGo(t, ex, head)
	if len(b.Unresolved)+len(h.Unresolved) > 0 {
		t.Fatalf("unexpected unresolved: base %v head %v", b.Unresolved, h.Unresolved)
	}
	return Compare(b.Grants, h.Grants)
}

func keysOf(g Grants) []string {
	out := make([]string, 0, len(g))
	for k := range g {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestExtractGo_RealProductFiles pins that the four product files are in the
// shape the Go extractors recognize: zero unresolved constructs, every anchor
// found, and the load-bearing grants present. If this fails after a refactor
// of one of those files, update backend/internal/permdrift/extract_go.go (and
// its README) in the same change — otherwise every later change to that file
// is raised as an unevaluable permission-drift concern.
func TestExtractGo_RealProductFiles(t *testing.T) {
	cases := []struct {
		file    string
		ex      goExtractor
		anchors []string
		want    []string
		exact   bool
	}{
		{"../server/manifest.go", ExtractGoManifest,
			[]string{ManifestPermissionPrefix, ManifestEventPrefix},
			[]string{"default_permissions.contents", "default_permissions.administration", "default_events.push"}, false},
		{"../server/mcpscopes.go", ExtractGoMCPScopes,
			[]string{"mcp_tool.mcpToolScopes"},
			[]string{
				"mcp_tool.fishhawk_get_plan.admitted",
				"mcp_tool.fishhawk_get_plan.any_of.*",
				"mcp_tool.fishhawk_retry_stage.any_of.write:retries",
				"mcp_tool.fishhawk_get_gate_view.any_of.scopeGateViewRead",
				"mcp_tool.fishhawk_get_gate_view.run_bound_subject_ok",
				"mcp_tool.fishhawk_send_crew_message.any_of.scopeWriteMessages",
			}, false},
		{"../server/mcptoken.go", ExtractGoRunTokenScopes,
			[]string{"run_token.func.handleIssueMCPToken"},
			[]string{
				"run_token.mcp:read@any_stage",
				"run_token.scopeWriteMessages@plan",
				"run_token.scopeWriteMessages@review",
				"run_token.write:retries@any_stage",
				"run_token.write:scope-amendments@implement",
			}, true},
		{"../reviewsandbox/env.go", ExtractGoEnvAllow,
			[]string{"env_allow.BaseAllow", "env_allow.ClaudeAllow", "env_allow.CodexAllow"},
			[]string{"env_allow.BaseAllow.PATH", "env_allow.ClaudeAllow.PATH",
				"env_allow.ClaudeAllow.ANTHROPIC_API_KEY", "env_allow.CodexAllow.CODEX_HOME"}, false},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			src, err := os.ReadFile(c.file)
			if err != nil {
				t.Fatalf("read %s: %v", c.file, err)
			}
			x := mustGo(t, c.ex, string(src))
			if len(x.Unresolved) > 0 {
				t.Fatalf("%s holds constructs the extractor cannot resolve: %v — update extract_go.go", c.file, x.Unresolved)
			}
			for _, a := range c.anchors {
				if !x.Anchors[a] {
					t.Errorf("anchor %q not found in %s (anchors %v)", a, c.file, x.Anchors)
				}
			}
			for _, k := range c.want {
				if _, ok := x.Grants[k]; !ok {
					t.Errorf("grant %q not found in %s", k, c.file)
				}
			}
			if c.exact && !reflect.DeepEqual(keysOf(x.Grants), c.want) {
				t.Errorf("%s grants = %v, want exactly %v", c.file, keysOf(x.Grants), c.want)
			}
		})
	}
}

const manifestGoTemplate = `package server

func buildManifest() map[string]any {
	return map[string]any{
		"name": "Fishhawk",
		"default_permissions": map[string]string{
%PERMS%
		},
		"default_events": []string{
%EVENTS%
		},
	}
}
`

func manifestGo(perms, events string) string {
	return strings.NewReplacer("%PERMS%", perms, "%EVENTS%", events).Replace(manifestGoTemplate)
}

func TestExtractGoManifest(t *testing.T) {
	const perms = `"contents": "write",
			"metadata": "read",`
	const events = `"issues",
			"push",`
	base := manifestGo(perms, events)

	t.Run("gaining an App permission is one widening, keyed like the JSON template", func(t *testing.T) {
		got := diffGo(t, ExtractGoManifest, base, manifestGo(perms+"\n\t\t\t\"administration\": \"read\",", events))
		want := []Change{{Key: "default_permissions.administration", Before: Absent, After: "read", Direction: Widened}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})
	t.Run("a level raise is a widening", func(t *testing.T) {
		got := diffGo(t, ExtractGoManifest, base, manifestGo(`"contents": "write",
			"metadata": "admin",`, events))
		want := []Change{{Key: "default_permissions.metadata", Before: "read", After: "admin", Direction: Widened}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})
	t.Run("an event added is a widening", func(t *testing.T) {
		got := diffGo(t, ExtractGoManifest, base, manifestGo(perms, events+"\n\t\t\t\"workflow_run\","))
		want := []Change{{Key: "default_events.workflow_run", Before: Absent, After: Present, Direction: Widened}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})
	t.Run("reorder is no change", func(t *testing.T) {
		got := diffGo(t, ExtractGoManifest, base, manifestGo(`"metadata": "read",
			"contents": "write",`, `"push",
			"issues",`))
		if len(got) != 0 {
			t.Errorf("got %+v, want none", got)
		}
	})
	t.Run("a same-file const permission name resolves to its value", func(t *testing.T) {
		src := manifestGo(`permContents: "write",`, events) + "\nconst permContents = \"contents\"\n"
		x := mustGo(t, ExtractGoManifest, src)
		if _, ok := x.Grants["default_permissions.contents"]; !ok || len(x.Unresolved) > 0 {
			t.Errorf("grants %v unresolved %v; want default_permissions.contents resolved", keysOf(x.Grants), x.Unresolved)
		}
	})
	t.Run("a default_permissions value built by a call is unresolved", func(t *testing.T) {
		src := strings.Replace(base, "map[string]string{\n"+perms+"\n\t\t}", "defaultPerms()", 1)
		if src == base {
			t.Fatal("fixture substitution did not apply")
		}
		x := mustGo(t, ExtractGoManifest, src)
		if len(x.withPrefix(ManifestPermissionPrefix).Unresolved) != 1 || len(x.withPrefix(ManifestEventPrefix).Unresolved) != 0 {
			t.Errorf("unresolved = %v; want one, on the permissions part only", x.Unresolved)
		}
	})
	t.Run("an unrecognized level is an error", func(t *testing.T) {
		if _, err := ExtractGoManifest([]byte(manifestGo(`"contents": "owner",`, events))); err == nil {
			t.Error("want an error for level owner")
		}
	})
}

const mcpScopesTemplate = `package server

type mcpToolScopeRule struct {
	anyOf             []string
	runBoundSubjectOK bool
}

var mcpScopeAuthenticatedOnly = mcpToolScopeRule{}

const scopeRunBoundRetry = "write:retries"

var mcpToolScopes = map[string]mcpToolScopeRule{
	"fishhawk_get_plan":    mcpScopeAuthenticatedOnly,
	"fishhawk_retry_stage": {anyOf: []string{"write:stages", scopeRunBoundRetry}},
	"fishhawk_start_run":   %START%,
	"fishhawk_gate_view":   {anyOf: []string{scopeGateViewRead}, runBoundSubjectOK: %RBS%},
%EXTRA%
}
`

func mcpScopes(kv ...string) string {
	r := map[string]string{"%START%": `{anyOf: []string{"write:runs"}}`, "%RBS%": "false", "%EXTRA%": ""}
	for i := 0; i+1 < len(kv); i += 2 {
		r[kv[i]] = kv[i+1]
	}
	out := mcpScopesTemplate
	for k, v := range r {
		out = strings.ReplaceAll(out, k, v)
	}
	return out
}

func TestExtractGoMCPScopes(t *testing.T) {
	base := mcpScopes()
	cases := []struct {
		name string
		head string
		want []Change
	}{
		{"adding a tool", mcpScopes("%EXTRA%", `	"fishhawk_new": {anyOf: []string{"write:runs"}},`),
			[]Change{
				{Key: "mcp_tool.fishhawk_new.admitted", Before: Absent, After: Present, Direction: Widened},
				{Key: "mcp_tool.fishhawk_new.any_of.write:runs", Before: Absent, After: Present, Direction: Widened},
			}},
		{"adding an anyOf member", mcpScopes("%START%", `{anyOf: []string{"write:runs", "write:stages"}}`),
			[]Change{{Key: "mcp_tool.fishhawk_start_run.any_of.write:stages", Before: Absent, After: Present, Direction: Widened}}},
		// {write:runs} -> the sentinel: the new any_of.* wildcard is the one
		// widening, and it SUBSUMES the vanished write:runs member.
		{"{write:runs} -> authenticated-only is one widening", mcpScopes("%START%", "mcpScopeAuthenticatedOnly"),
			[]Change{{Key: "mcp_tool.fishhawk_start_run.any_of.*", Before: Absent, After: "authenticated only", Direction: Widened}}},
		{"runBoundSubjectOK false -> true", mcpScopes("%RBS%", "true"),
			[]Change{{Key: "mcp_tool.fishhawk_gate_view.run_bound_subject_ok", Before: Absent, After: Present, Direction: Widened}}},
		{"reorder is no change", strings.Replace(base, `{"write:stages", scopeRunBoundRetry}`, `{scopeRunBoundRetry, "write:stages"}`, 1), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := diffGo(t, ExtractGoMCPScopes, base, c.head)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
	t.Run("authenticated-only -> {write:runs} is a narrowing only", func(t *testing.T) {
		got := diffGo(t, ExtractGoMCPScopes, mcpScopes("%START%", "mcpScopeAuthenticatedOnly"), base)
		want := []Change{{Key: "mcp_tool.fishhawk_start_run.any_of.*", Before: "authenticated only", After: Absent, Direction: Narrowed}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})
	t.Run("an in-file const resolves; a cross-file name is kept by name", func(t *testing.T) {
		x := mustGo(t, ExtractGoMCPScopes, base)
		for _, k := range []string{"mcp_tool.fishhawk_retry_stage.any_of.write:retries", "mcp_tool.fishhawk_gate_view.any_of.scopeGateViewRead"} {
			if _, ok := x.Grants[k]; !ok {
				t.Errorf("missing %q in %v", k, keysOf(x.Grants))
			}
		}
	})
	// Item 10 wildcard half: an anyOf member or a tool name is FILE-DERIVED,
	// so a member literally "*" keys as `any_of.%2A`, an ordinary key, never
	// the extractor's own authenticated-only wildcard `any_of.*`.
	// COUNTERFACTUAL: make keySegment return s unchanged (body mutation) —
	// the member then keys `any_of.*` and both assertions go RED (observed:
	// `want mcp_tool.fishhawk_start_run.any_of.%2A`).
	t.Run("a file-derived member named * is not a wildcard", func(t *testing.T) {
		x := mustGo(t, ExtractGoMCPScopes, mcpScopes("%START%", `{anyOf: []string{"*"}}`, "%EXTRA%", `	"a.b": {anyOf: []string{"write:runs"}},`))
		if _, ok := x.Grants["mcp_tool.fishhawk_start_run.any_of.%2A"]; !ok {
			t.Errorf("want mcp_tool.fishhawk_start_run.any_of.%%2A in %v", keysOf(x.Grants))
		}
		if _, ok := x.Grants["mcp_tool.fishhawk_start_run.any_of"+WildcardSuffix]; ok {
			t.Errorf("a member named * minted the authenticated-only wildcard: %v", keysOf(x.Grants))
		}
		if _, ok := x.Grants["mcp_tool.a%2Eb.admitted"]; !ok {
			t.Errorf("want the dotted tool name escaped to one segment in %v", keysOf(x.Grants))
		}
	})
	t.Run("unrecognized rule shapes are unresolved", func(t *testing.T) {
		for name, head := range map[string]string{
			"anyOf built by a call":   mcpScopes("%START%", `{anyOf: scopesFor("start")}`),
			"anyOf element a call":    mcpScopes("%START%", `{anyOf: []string{"write:runs", extra()}}`),
			"rule value a call":       mcpScopes("%START%", `ruleFor("start")`),
			"unknown rule field":      mcpScopes("%START%", `{anyOf: []string{"write:runs"}, deny: false}`),
			"runBoundSubjectOK a var": mcpScopes("%RBS%", "flag"),
		} {
			if x := mustGo(t, ExtractGoMCPScopes, head); len(x.Unresolved) == 0 {
				t.Errorf("%s: want an unresolved construct, got none", name)
			}
		}
	})
}

const tokenTemplate = `package server

import "github.com/kuhlman-labs/fishhawk/backend/internal/run"

func (s *Server) handleIssueMCPToken(w http.ResponseWriter, r *http.Request) {
	scopes := []string{"mcp:read"}
	if agentSelfRetry := s.resolveAgentSelfRetry(r); agentSelfRetry {
		scopes = append(scopes, "write:retries")
	}
	stageType := s.resolveExecutingStageType(r)
	if stageType == run.StageTypeImplement {
		scopes = append(scopes, "write:scope-amendments")
	}
%MESSAGES%
%EXTRA%
	s.issue(scopes)
}

func stageTypeMayMessage(st run.StageType) bool {
	return st == run.StageTypePlan || st == run.StageTypeReview%PRED%
}
`

const tokenMessagesDefault = `	if stageTypeMayMessage(stageType) {
		scopes = append(scopes, scopeWriteMessages)
	}`

func tokenSrc(kv ...string) string {
	r := map[string]string{"%MESSAGES%": tokenMessagesDefault, "%EXTRA%": "", "%PRED%": ""}
	for i := 0; i+1 < len(kv); i += 2 {
		r[kv[i]] = kv[i+1]
	}
	out := tokenTemplate
	for k, v := range r {
		out = strings.ReplaceAll(out, k, v)
	}
	return out
}

func TestExtractGoRunTokenScopes(t *testing.T) {
	base := tokenSrc()
	// COUNTERFACTUAL (one-level predicate resolution): make collect's
	// *ast.CallExpr branch return without resolving (body mutation). The
	// fixture's message grant is guarded ONLY by the call
	// stageTypeMayMessage(stageType), which carries no stage selector of its
	// own, so without resolution the grant keys @any_stage on BOTH sides and
	// the predicate gaining `|| st == run.StageTypeImplement` yields no
	// change — the first row goes RED.
	cases := []struct {
		name string
		head string
		want []Change
	}{
		{"predicate gains implement", tokenSrc("%PRED%", " || st == run.StageTypeImplement"),
			[]Change{{Key: "run_token.scopeWriteMessages@implement", Before: Absent, After: Present, Direction: Widened}}},
		{"unconditional append keys @any_stage", tokenSrc("%EXTRA%", `	scopes = append(scopes, "write:runs")`),
			[]Change{{Key: "run_token.write:runs@any_stage", Before: Absent, After: Present, Direction: Widened}}},
		// COUNTERFACTUAL (negation): drop collect's NEQ branch (body
		// mutation). The fixture's guard `stageType != run.StageTypeImplement`
		// carries the implement selector, so without negation the grant keys
		// @implement where the row asserts @any_stage — RED.
		{"a negated stage comparison keys @any_stage", tokenSrc("%EXTRA%", `	if stageType != run.StageTypeImplement {
		scopes = append(scopes, "write:runs")
	}`),
			[]Change{{Key: "run_token.write:runs@any_stage", Before: Absent, After: Present, Direction: Widened}}},
		{"an else branch keys @any_stage", tokenSrc("%EXTRA%", `	if stageType == run.StageTypePlan {
	} else {
		scopes = append(scopes, "write:runs")
	}`),
			[]Change{{Key: "run_token.write:runs@any_stage", Before: Absent, After: Present, Direction: Widened}}},
		{"a switch case keys its stages", tokenSrc("%EXTRA%", `	switch stageType {
	case run.StageTypeDeploy, run.StageTypeAcceptanceGate:
		scopes = append(scopes, "write:runs")
	}`),
			[]Change{
				{Key: "run_token.write:runs@acceptance_gate", Before: Absent, After: Present, Direction: Widened},
				{Key: "run_token.write:runs@deploy", Before: Absent, After: Present, Direction: Widened},
			}},
		{"a spread append of a literal", tokenSrc("%EXTRA%", `	scopes = append(scopes, []string{"a", "b"}...)`),
			[]Change{
				{Key: "run_token.a@any_stage", Before: Absent, After: Present, Direction: Widened},
				{Key: "run_token.b@any_stage", Before: Absent, After: Present, Direction: Widened},
			}},
		{"moving the grant to a different stage", tokenSrc("%MESSAGES%", `	if stageType == run.StageTypePlan {
		scopes = append(scopes, scopeWriteMessages)
	}`),
			[]Change{{Key: "run_token.scopeWriteMessages@review", Before: Present, After: Absent, Direction: Narrowed}}},
		// COUNTERFACTUAL (`||` operand rule, item 4a): delete collect's
		// `case token.LOR:` arm body (body mutation). Each of the next three
		// fixtures has exactly ONE stage selector, in one operand, so without
		// the rule the grant keys only that stage and the `@any_stage`
		// assertion goes RED (observed: `got [{Key:run_token.write:runs@plan
		// ...}] want [{Key:run_token.write:runs@any_stage ...}]`). The
		// "predicate gains implement" row above (`|| st ==
		// run.StageTypeImplement`, both operands staged) is the
		// no-over-trigger control.
		{"`|| true` keys @any_stage", tokenSrc("%EXTRA%", `	if stageType == run.StageTypePlan || true {
		scopes = append(scopes, "write:runs")
	}`),
			[]Change{{Key: "run_token.write:runs@any_stage", Before: Absent, After: Present, Direction: Widened}}},
		{"`|| other.Pred(st)` keys @any_stage", tokenSrc("%EXTRA%", `	if stageType == run.StageTypePlan || other.Pred(stageType) {
		scopes = append(scopes, "write:runs")
	}`),
			[]Change{{Key: "run_token.write:runs@any_stage", Before: Absent, After: Present, Direction: Widened}}},
		{"a two-level same-file predicate keys @any_stage", tokenSrc("%PRED%", " || mayAlso(st)\n}\n\nfunc mayAlso(st run.StageType) bool {\n\treturn st == run.StageTypeImplement"),
			[]Change{
				{Key: "run_token.scopeWriteMessages@any_stage", Before: Absent, After: Present, Direction: Widened},
				{Key: "run_token.scopeWriteMessages@plan", Before: Present, After: Absent, Direction: Narrowed},
				{Key: "run_token.scopeWriteMessages@review", Before: Present, After: Absent, Direction: Narrowed},
			}},
		// COUNTERFACTUAL (fallthrough guard carry, item 4b): make
		// endsInFallthrough return false (body mutation) — the plan case's
		// body is only `fallthrough`, so without the carry the implement body
		// keys only @implement and the @plan row goes RED (observed: `got
		// [{Key:run_token.write:runs@implement ...}]`).
		{"a fallthrough case carries its stages into the next body", tokenSrc("%EXTRA%", `	switch stageType {
	case run.StageTypePlan:
		fallthrough
	case run.StageTypeImplement:
		scopes = append(scopes, "write:runs")
	}`),
			[]Change{
				{Key: "run_token.write:runs@implement", Before: Absent, After: Present, Direction: Widened},
				{Key: "run_token.write:runs@plan", Before: Absent, After: Present, Direction: Widened},
			}},
		// #3935 review: Go reads the last NON-EMPTY statement, so a trailing
		// empty statement (`fallthrough;;`) and a labeled `L: fallthrough`
		// still fall through. COUNTERFACTUALS (body mutations of
		// endsInFallthrough, one at a time): drop the *ast.EmptyStmt skip —
		// the `;;` row keys only @implement and goes RED; drop the
		// *ast.LabeledStmt unwrap — the labeled row goes RED.
		{"a fallthrough followed by an empty statement still carries", tokenSrc("%EXTRA%", `	switch stageType {
	case run.StageTypePlan:
		fallthrough;;
	case run.StageTypeImplement:
		scopes = append(scopes, "write:runs")
	}`),
			[]Change{
				{Key: "run_token.write:runs@implement", Before: Absent, After: Present, Direction: Widened},
				{Key: "run_token.write:runs@plan", Before: Absent, After: Present, Direction: Widened},
			}},
		{"a labeled fallthrough still carries", tokenSrc("%EXTRA%", `	switch stageType {
	case run.StageTypePlan:
	L:
		fallthrough
	case run.StageTypeImplement:
		scopes = append(scopes, "write:runs")
	}`),
			[]Change{
				{Key: "run_token.write:runs@implement", Before: Absent, After: Present, Direction: Widened},
				{Key: "run_token.write:runs@plan", Before: Absent, After: Present, Direction: Widened},
			}},
		{"a fallthrough from default keys the next body @any_stage", tokenSrc("%EXTRA%", `	switch stageType {
	default:
		fallthrough
	case run.StageTypeImplement:
		scopes = append(scopes, "write:runs")
	}`),
			[]Change{{Key: "run_token.write:runs@any_stage", Before: Absent, After: Present, Direction: Widened}}},
		{"no fallthrough: the carry does not leak", tokenSrc("%EXTRA%", `	switch stageType {
	case run.StageTypePlan:
		scopes = append(scopes, "write:a")
	case run.StageTypeImplement:
		scopes = append(scopes, "write:runs")
	}`),
			[]Change{
				{Key: "run_token.write:a@plan", Before: Absent, After: Present, Direction: Widened},
				{Key: "run_token.write:runs@implement", Before: Absent, After: Present, Direction: Widened},
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := diffGo(t, ExtractGoRunTokenScopes, base, c.head)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
	// COUNTERFACTUAL (stray-use tracking): make checkStrayUses a no-op (body
	// mutation). Each fixture below keeps every RECOGNIZED grant intact and
	// writes the slice only through the stray construct, so with the check
	// gone the extraction has zero unresolved entries and the subtest goes
	// RED.
	t.Run("unrecognized writes to scopes are unresolved", func(t *testing.T) {
		for name, extra := range map[string]string{
			"append moved into a helper":     `	scopes = addMessageScope(scopes, stageType)`,
			"append argument a call":         `	scopes = append(scopes, extraScope())`,
			"append argument a local":        "\textra := pick()\n\tscopes = append(scopes, extra)",
			"append not assigned back":       `	granted := append(scopes, "write:runs")`,
			"address taken":                  `	addScopes(&scopes)`,
			"indexed write":                  `	scopes[0] = "write:runs"`,
			"assignment inside a closure":    `	func() { scopes = append(scopes, "write:runs") }()`,
			"spread append of a call result": `	scopes = append(scopes, more()...)`,
		} {
			if x := mustGo(t, ExtractGoRunTokenScopes, tokenSrc("%EXTRA%", extra)); len(x.Unresolved) == 0 {
				t.Errorf("%s: want an unresolved construct, got none", name)
			}
		}
	})
	// Item 4c + approval condition 5: aliases of the slice, `&scopes` (bare
	// and as a call argument) and copy() into it write the shared backing
	// array without touching a recognized grant, so each must be unresolved
	// (Detect then reports shape_unrecognized). Each fixture is ISOLATED:
	// exactly one rule in checkStrayUses can fire on it.
	//
	// COUNTERFACTUALS (body mutations, one at a time):
	//   - alias rule: make aliasesScopes return false — the two alias rows
	//     and the copy row resolve cleanly with identical grants and go RED
	//     (observed: `alias := scopes: want an unresolved construct, got none`);
	//   - address rule: delete the `*ast.UnaryExpr` arm's body — the four
	//     address-of rows go RED; narrow it back to `isIdentNamed(n.X, ...)`
	//     (the pre-#3935-review form) — the two element/reslice address rows
	//     go RED;
	//   - reslice append rule: narrow the append arm's first-argument test
	//     back to `isIdentNamed` — the `append(scopes[:0], ...)` row goes RED
	//     (it writes scopes[0] in place through the spare capacity).
	t.Run("aliases, address-of and copy are stray uses", func(t *testing.T) {
		for name, extra := range map[string]string{
			"alias := scopes":       "\talias := scopes\n\talias[0] = \"admin\"",
			"var alias = scopes[:]": "\tvar alias = scopes[:]\n\talias[0] = \"admin\"",
			"alias = (scopes[1:])":  "\tvar alias []string\n\talias = (scopes[1:])\n\talias[0] = \"admin\"",
			"p := &scopes":          "\tp := &scopes\n\t*p = append(*p, \"admin\")",
			"mutate(&scopes)":       `	mutate(&scopes)`,
			"p := &scopes[0]":       "\tp := &scopes[0]\n\t*p = \"admin\"",
			"p := &scopes[0:1]":     "\tp := &scopes[0:1]\n\t(*p)[0] = \"admin\"",
			"append(scopes[:0], …)": `	_ = append(scopes[:0], "admin")`,
			"copy(scopes, ...)":     `	copy(scopes, []string{"admin"})`,
			"copy(scopes[1:], ...)": `	copy(scopes[1:], []string{"admin"})`,
		} {
			x := mustGo(t, ExtractGoRunTokenScopes, tokenSrc("%EXTRA%", extra))
			if len(x.Unresolved) == 0 {
				t.Errorf("%s: want an unresolved construct, got none (grants %v)", name, keysOf(x.Grants))
				continue
			}
			base := mustGo(t, ExtractGoRunTokenScopes, tokenSrc())
			if r := Detect(surfaceByID(t, "run-token-scope-grants"), side(tokenSrc()), side(tokenSrc("%EXTRA%", extra))); r.Unevaluable != ReasonShapeUnrecognized || len(base.Unresolved) != 0 {
				t.Errorf("%s: Detect = %+v, want shape_unrecognized", name, r)
			}
		}
	})
	// CONTROL: passing scopes BY VALUE as a call argument or a composite
	// field (the real handler's shape) stays resolvable, and a self-append
	// is not an alias. TestExtractGo_RealProductFiles is the end-to-end
	// control on the real mcptoken.go.
	t.Run("by-value uses stay resolvable", func(t *testing.T) {
		for name, extra := range map[string]string{
			"call argument":   `	s.audit(scopes)`,
			"composite field": `	_ = token{Scopes: scopes}`,
			"range read":      "\tfor _, sc := range scopes {\n\t\t_ = sc\n\t}",
		} {
			if x := mustGo(t, ExtractGoRunTokenScopes, tokenSrc("%EXTRA%", extra)); len(x.Unresolved) != 0 {
				t.Errorf("%s: unresolved %v, want none", name, x.Unresolved)
			}
		}
	})
}

const envTemplate = `package reviewsandbox

var BaseAllow = []string{
	"PATH", "HOME",%BASE%
}

var ClaudeAllow = extend(BaseAllow,
	"ANTHROPIC_API_KEY",
)

var CodexAllow = %CODEX%

func extend(base []string, extra ...string) []string {
	out := make([]string, 0, len(base)+len(extra))
	out = append(out, base...)
	out = append(out, extra...)%EXTEND%
	return out
}
`

func envSrc(kv ...string) string {
	r := map[string]string{"%BASE%": "", "%CODEX%": `extend(BaseAllow, "OPENAI_API_KEY")`, "%EXTEND%": ""}
	for i := 0; i+1 < len(kv); i += 2 {
		r[kv[i]] = kv[i+1]
	}
	out := envTemplate
	for k, v := range r {
		out = strings.ReplaceAll(out, k, v)
	}
	return out
}

func TestExtractGoEnvAllow(t *testing.T) {
	base := envSrc()
	t.Run("a name added to BaseAllow widens every list built from it", func(t *testing.T) {
		got := diffGo(t, ExtractGoEnvAllow, base, envSrc("%BASE%", ` "GITHUB_TOKEN",`))
		want := []Change{
			{Key: "env_allow.BaseAllow.GITHUB_TOKEN", Before: Absent, After: Present, Direction: Widened},
			{Key: "env_allow.ClaudeAllow.GITHUB_TOKEN", Before: Absent, After: Present, Direction: Widened},
			{Key: "env_allow.CodexAllow.GITHUB_TOKEN", Before: Absent, After: Present, Direction: Widened},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})
	t.Run("reorder is no change", func(t *testing.T) {
		got := diffGo(t, ExtractGoEnvAllow, base, strings.Replace(base, `"PATH", "HOME",`, `"HOME", "PATH",`, 1))
		if len(got) != 0 {
			t.Errorf("got %+v, want none", got)
		}
	})
	t.Run("a removed name is a narrowing only", func(t *testing.T) {
		got := diffGo(t, ExtractGoEnvAllow, base, envSrc("%CODEX%", "extend(BaseAllow)"))
		want := []Change{{Key: "env_allow.CodexAllow.OPENAI_API_KEY", Before: Present, After: Absent, Direction: Narrowed}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})
	// COUNTERFACTUAL (extend purity): make extendIsPure return true (body
	// mutation). The fixture's extend body appends a literal "GITHUB_TOKEN"
	// that no list names, so with the purity check gone the call resolves
	// with no unresolved construct and the subtest goes RED.
	t.Run("an extend that injects a name is unresolved", func(t *testing.T) {
		x := mustGo(t, ExtractGoEnvAllow, envSrc("%EXTEND%", "\n\tout = append(out, \"GITHUB_TOKEN\")"))
		if len(x.Unresolved) == 0 {
			t.Error("want an unresolved construct for an impure extend")
		}
	})
	t.Run("a slice element that is a call is unresolved", func(t *testing.T) {
		x := mustGo(t, ExtractGoEnvAllow, envSrc("%BASE%", " secretName(),"))
		if len(x.Unresolved) == 0 {
			t.Error("want an unresolved construct for a call element")
		}
	})
}

func TestExtractGo_UnparseableIsError(t *testing.T) {
	for name, ex := range map[string]goExtractor{
		"manifest": ExtractGoManifest, "mcp": ExtractGoMCPScopes,
		"token": ExtractGoRunTokenScopes, "env": ExtractGoEnvAllow,
	} {
		if _, err := ex([]byte("package x\nfunc {")); err == nil {
			t.Errorf("%s: want a parse error", name)
		}
		if x, err := ex(nil); err != nil || len(x.Grants) != 0 {
			t.Errorf("%s: absent file = %+v, %v; want empty, nil", name, x, err)
		}
	}
}
