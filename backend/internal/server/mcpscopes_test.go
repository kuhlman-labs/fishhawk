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

// TestMCPToolScopeTable_MergeRecoveryPairRequiresWriteRuns pins the two
// merge-recovery entries to {anyOf: [write:runs]} and proves the gate admits
// and refuses exactly who the endpoints do.
//
// THE DERIVATION, stated positively: since E45.95 / #3635 BOTH
// handleRecordMergeObservation (merge_observation.go) and handleReconcileMerge
// (merge_supersede.go) call requireWriteScope(w, r, "write:runs") as their rung
// 0, ahead of every other refusal. write:runs is therefore the predicate those
// endpoints enforce, and this table mirrors it.
//
// WHY THE ASSERTION RUNS IN BOTH DIRECTIONS. The derivation rule forbids the
// gate being STRICTER than the endpoint (a needless scope here would refuse a
// token the REST endpoint still admits, breaking the recovery path with no
// failing REST test) AND forbids it being LOOSER (a sentinel here would let an
// under-scoped call construct a per-request tool registry only to be refused by
// the inner REST hop, relocating the enforcement point back where #2459 moved
// it from). So this pins the rule exactly: a read-only operator bearer is
// REFUSED, a run-bound mcp:read token is REFUSED, and a write:runs bearer is
// ADMITTED. A comment-only touch of mcpscopes.go fails it.
func TestMCPToolScopeTable_MergeRecoveryPairRequiresWriteRuns(t *testing.T) {
	// A read-only operator bearer: the identity the endpoints admitted before
	// #3635 and refuse now. Its scope set is DISJOINT from the requirement, so
	// the refusal is produced by construction, not by a fixture guard.
	readOnlyOperator := Identity{Subject: "svc:operator", TokenID: "tok", Scopes: []string{"read:runs"}}
	// A run-bound fhm_ token at its FULL issued vocabulary (mcptoken.go). Not
	// one of these scopes is write:runs, and neither handler authorizes such a
	// token by SUBJECT, so the gate must refuse it exactly as the REST layer
	// does — admitting it would make this gate LOOSER than the endpoint.
	runBound := Identity{
		Subject: "mcp:run:33333333-3333-3333-3333-333333333333",
		TokenID: "tok",
		Scopes:  []string{scopeRunBoundRead, scopeRunBoundRetry, scopeRunBoundScopeAmendments},
	}
	// The operator identity the endpoints admit.
	writeRunsOperator := Identity{Subject: "svc:operator", TokenID: "tok", Scopes: []string{"write:runs"}}

	for _, tool := range []string{"fishhawk_record_merge_observation", "fishhawk_reconcile_merge"} {
		rule, ok := mcpToolScopeFor(tool)
		if !ok {
			t.Errorf("%s: no mcpToolScopes entry; it would be refused mcp_tool_not_authorized at runtime", tool)
			continue
		}
		if len(rule.anyOf) != 1 || rule.anyOf[0] != "write:runs" {
			t.Errorf("%s: rule.anyOf = %v, want exactly [write:runs] — the mirrored handler enforces "+
				"requireWriteScope(\"write:runs\") as its rung 0 (E45.95 / #3635), and this table must mirror "+
				"the endpoint in BOTH directions: neither stricter nor looser", tool, rule.anyOf)
			continue
		}
		if rule.runBoundSubjectOK {
			t.Errorf("%s: runBoundSubjectOK is set, but neither handler authorizes a run-bound token by "+
				"SUBJECT — a run-bound fhm_ token is refused at the REST layer too, so this makes the gate "+
				"LOOSER than the endpoint it mirrors", tool)
		}
		if rule.satisfiedBy(readOnlyOperator) {
			t.Errorf("%s: a read-scoped operator bearer (%s) is ADMITTED by the gate, but the REST endpoint "+
				"now refuses it 403 insufficient_scope", tool, strings.Join(readOnlyOperator.Scopes, " "))
		}
		if rule.satisfiedBy(runBound) {
			t.Errorf("%s: a run-bound fhm_ token (%s) is ADMITTED by the gate, but the REST endpoint refuses "+
				"it 403 insufficient_scope", tool, strings.Join(runBound.Scopes, " "))
		}
		if !rule.satisfiedBy(writeRunsOperator) {
			t.Errorf("%s: an operator bearer holding write:runs is REFUSED by the gate, but the REST endpoint "+
				"admits it — the merge-recovery verb completion_blocked hands an operator would be unreachable",
				tool)
		}
	}
}
