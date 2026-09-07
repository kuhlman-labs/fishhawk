package server

import (
	"context"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
)

// TestMCPToolScopeTable_CoversRegistry is the DONE-MEANS drift test for
// mcpToolScopes: a config-shaped surface no compiler enforces, where a tool
// added without an entry would be refused at runtime and a stale entry would
// linger forever. It asserts SET EQUALITY IN BOTH DIRECTIONS against the LIVE
// registry — mcpserver.NewServer over the SDK's in-memory transport, the same
// mcpToolNamesDirect helper TestMCPRoute_ToolRegistryParity uses.
//
// A presence-only check would pass a table missing the 54th tool; the
// both-directions set comparison is what makes that RED.
func TestMCPToolScopeTable_CoversRegistry(t *testing.T) {
	registered := mcpToolNamesDirect(t, context.Background())
	if len(registered) == 0 {
		t.Fatal("mcpserver.NewServer advertised zero tools; the comparison would be vacuous")
	}
	for name := range registered {
		if _, ok := mcpToolScopeFor(name); !ok {
			t.Errorf("tool %q is registered by mcpserver.NewServer but has NO mcpToolScopes entry; "+
				"it would be refused mcp_tool_not_authorized at runtime", name)
		}
	}
	for name := range mcpToolScopes {
		if !registered[name] {
			t.Errorf("mcpToolScopes carries a stale entry %q that mcpserver.NewServer does not register", name)
		}
	}
}

// TestMCPToolScopeTable_VocabularyIsNotParallel pins every anyOf member to a
// scope this product already speaks: the ratified operator vocabulary
// (oauthas.SupportedScopes, #2391), the three run-bound fhm_ scopes
// (mcptoken.go), or a named constant an existing handler enforces. Without it
// a typo ("write:stage") or an invented MCP-only scope would silently make a
// tool unreachable for every real token.
func TestMCPToolScopeTable_VocabularyIsNotParallel(t *testing.T) {
	allowed := map[string]string{}
	for _, sc := range oauthas.SupportedScopes {
		allowed[sc] = "oauthas.SupportedScopes"
	}
	allowed[scopeRunBoundRead] = "run-bound fhm_ scope (mcptoken.go)"
	allowed[scopeRunBoundRetry] = "run-bound fhm_ scope (mcptoken.go)"
	allowed[scopeRunBoundScopeAmendments] = "run-bound fhm_ scope (mcptoken.go)"
	allowed[scopeGateViewRead] = "named constant (gateview.go)"
	allowed[scopeAuditExport] = "named constant (audit_export.go)"
	allowed[scopeRefinementGate] = "named constant (refinement.go)"
	allowed[scopeFixupAlternate] = "handler alternate (fixup.go/waive.go/defer_concern.go)"

	for name, rule := range mcpToolScopes {
		for _, sc := range rule.anyOf {
			if _, ok := allowed[sc]; !ok {
				t.Errorf("tool %q requires scope %q, which is not in the product's scope vocabulary "+
					"(oauthas.SupportedScopes, the run-bound fhm_ scopes, or an existing named constant); "+
					"a parallel vocabulary makes the tool unreachable for every real token", name, sc)
			}
		}
	}
}

// TestMCPToolScopeFor covers the lookup's two outcomes: a known tool resolves,
// and an unmapped name reports ok=false so the gate can fail closed rather
// than defaulting an unknown tool to the sentinel.
func TestMCPToolScopeFor(t *testing.T) {
	if _, ok := mcpToolScopeFor("fishhawk_start_run"); !ok {
		t.Error("fishhawk_start_run did not resolve; the table lookup is broken")
	}
	if _, ok := mcpToolScopeFor("fishhawk_definitely_not_a_tool"); ok {
		t.Error("an unmapped tool name resolved; the gate would admit an unknown tool")
	}
	if _, ok := mcpToolScopeFor(""); ok {
		t.Error("the empty tool name resolved")
	}
}

// TestMCPToolScopeRule_SatisfiedBy tables the rule predicate: the sentinel, a
// single-scope rule, the any-of shape (satisfied by EITHER member and by
// neither of a disjoint set), and the run-bound-subject shape both ways.
func TestMCPToolScopeRule_SatisfiedBy(t *testing.T) {
	operator := func(scopes ...string) Identity {
		return Identity{Subject: "svc:operator", TokenID: "tok", Scopes: scopes}
	}
	runBound := func(scopes ...string) Identity {
		return Identity{Subject: "mcp:run:11111111-1111-1111-1111-111111111111", TokenID: "tok", Scopes: scopes}
	}

	for _, tc := range []struct {
		name string
		rule mcpToolScopeRule
		id   Identity
		want bool
	}{
		{"sentinel admits a scopeless authenticated identity", mcpScopeAuthenticatedOnly, operator(), true},
		{"single scope held", mcpToolScopeRule{anyOf: []string{"write:runs"}}, operator("write:runs"), true},
		{"single scope not held", mcpToolScopeRule{anyOf: []string{"write:runs"}}, operator("read:runs"), false},
		{"any-of satisfied by first member", mcpToolScopeRule{anyOf: []string{"write:stages", "write:retries"}}, operator("write:stages"), true},
		{"any-of satisfied by second member", mcpToolScopeRule{anyOf: []string{"write:stages", "write:retries"}}, operator("write:retries"), true},
		{"any-of unsatisfied by a disjoint set", mcpToolScopeRule{anyOf: []string{"write:stages", "write:retries"}}, operator("read:runs", "write:campaigns"), false},
		{"run-bound subject admitted where the handler admits it", mcpToolScopeRule{anyOf: []string{"read:audit"}, runBoundSubjectOK: true}, runBound("mcp:read"), true},
		{"run-bound subject NOT admitted where the handler does not", mcpToolScopeRule{anyOf: []string{"read:audit"}}, runBound("mcp:read"), false},
		{"operator identity still needs the scope on a run-bound-lenient rule", mcpToolScopeRule{anyOf: []string{"read:audit"}, runBoundSubjectOK: true}, operator("read:runs"), false},
	} {
		if got := tc.rule.satisfiedBy(tc.id); got != tc.want {
			t.Errorf("%s: satisfiedBy = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestMCPToolScopeTable_RunBoundAgentLoopIntact is the fhm_ NON-REGRESSION
// assertion. A run-bound token carries ONLY mcp:read (+write:retries when the
// stage sets executor.agent_self_retry, +write:scope-amendments on implement
// stages). Every tool the in-run agent actually calls must remain reachable
// with exactly that scope set, or this gate breaks the agent loop with no
// compile error and no failing REST test.
func TestMCPToolScopeTable_RunBoundAgentLoopIntact(t *testing.T) {
	selfRetryImplement := Identity{
		Subject: "mcp:run:22222222-2222-2222-2222-222222222222",
		TokenID: "tok",
		Scopes:  []string{scopeRunBoundRead, scopeRunBoundRetry, scopeRunBoundScopeAmendments},
	}
	for _, tool := range []string{
		"fishhawk_get_run_status",
		"fishhawk_get_gate_view",
		"fishhawk_list_scope_amendments",
		"fishhawk_await_stage",
		"fishhawk_await_audit",
		"fishhawk_retry_stage",
		"fishhawk_file_issue",
		"fishhawk_report_product_issue",
	} {
		rule, ok := mcpToolScopeFor(tool)
		if !ok {
			t.Errorf("%s: no table entry", tool)
			continue
		}
		if !rule.satisfiedBy(selfRetryImplement) {
			t.Errorf("%s: a run-bound self-retrying implement token (%s) is refused by the gate, "+
				"but the REST endpoint admits it — this breaks the in-run agent loop",
				tool, strings.Join(selfRetryImplement.Scopes, " "))
		}
	}
}
