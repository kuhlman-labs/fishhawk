package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
	"github.com/kuhlman-labs/fishhawk/backend/internal/specialistjob"
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
	allowed[scopeRunBoundMessages] = "run-bound fhm_ scope (mcptoken.go)"
	allowed[scopeGateViewRead] = "named constant (gateview.go)"
	allowed[scopeAuditExport] = "named constant (audit_export.go)"
	allowed[scopeRefinementGate] = "named constant (refinement.go)"
	allowed[scopeFixupAlternate] = "handler alternate (fixup.go/waive.go/defer_concern.go)"
	// specialistjob.RequestScope is product vocabulary (the seat's REST-only
	// request scope) but is deliberately on NO /mcp tool; that boundary is
	// pinned by TestFirstOfficerSeat_MCPRefusesRunnerSpawningTools, not here.
	allowed[specialistjob.RequestScope] = "named constant (specialistjob.RequestScope, seat-only alternative in specialist_job.go)"

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
// stages, +write:messages on plan and review stages). Every tool the in-run agent actually calls must remain reachable
// with exactly that scope set, or this gate breaks the agent loop with no
// compile error and no failing REST test.
func TestMCPToolScopeTable_RunBoundAgentLoopIntact(t *testing.T) {
	selfRetryImplement := Identity{
		Subject: "mcp:run:22222222-2222-2222-2222-222222222222",
		TokenID: "tok",
		Scopes:  []string{scopeRunBoundRead, scopeRunBoundRetry, scopeRunBoundScopeAmendments},
	}
	// The plan/review arm of the issued vocabulary: write:messages replaces
	// write:scope-amendments (mcptoken.go mints each for disjoint stage types).
	selfRetryPlanReview := Identity{
		Subject: "mcp:run:22222222-2222-2222-2222-222222222222",
		TokenID: "tok",
		Scopes:  []string{scopeRunBoundRead, scopeRunBoundRetry, scopeRunBoundMessages},
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
		for _, id := range []Identity{selfRetryImplement, selfRetryPlanReview} {
			if !rule.satisfiedBy(id) {
				t.Errorf("%s: a run-bound self-retrying token (%s) is refused by the gate, "+
					"but the REST endpoint admits it — this breaks the in-run agent loop",
					tool, strings.Join(id.Scopes, " "))
			}
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
		Scopes:  []string{scopeRunBoundRead, scopeRunBoundRetry, scopeRunBoundScopeAmendments, scopeRunBoundMessages},
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

// TestMCPToolScopeTable_FileIssueRequiresWriteRuns pins the fishhawk_file_issue
// row (#4481) to {anyOf: [write:runs], runBoundSubjectOK: true} and proves the
// gate admits and refuses exactly who POST /v0/work-items does: the handler
// calls requireWriteScope(w, r, "write:runs") for every NON-run-bound caller
// and admits a run-bound subject by its own-run entitlement (the
// product_report.go switch). A read-only operator bearer is REFUSED, a
// write:runs operator bearer is ADMITTED, and a run-bound mcp:read token is
// ADMITTED. A comment-only touch of mcpscopes.go fails it.
func TestMCPToolScopeTable_FileIssueRequiresWriteRuns(t *testing.T) {
	rule, ok := mcpToolScopeFor("fishhawk_file_issue")
	if !ok {
		t.Fatal("fishhawk_file_issue has no mcpToolScopes entry; it would be refused mcp_tool_not_authorized at runtime")
	}
	if len(rule.anyOf) != 1 || rule.anyOf[0] != "write:runs" {
		t.Errorf("rule.anyOf = %v, want exactly [write:runs]: workitems.go requires write:runs of every "+
			"non-run-bound caller (#4481)", rule.anyOf)
	}
	if !rule.runBoundSubjectOK {
		t.Error("runBoundSubjectOK is unset, but workitems.go admits a run-bound subject by its own-run " +
			"entitlement without write:runs: the in-run agent's filing would be refused at the gate")
	}
	// Disjoint from the requirement by construction.
	readOnlyOperator := Identity{Subject: "github:reader", TokenID: "tok", Scopes: []string{"read:runs"}}
	if rule.satisfiedBy(readOnlyOperator) {
		t.Errorf("a read-scoped operator bearer (%s) is ADMITTED by the gate, but POST /v0/work-items refuses "+
			"it 403 insufficient_scope", strings.Join(readOnlyOperator.Scopes, " "))
	}
	writeRunsOperator := Identity{Subject: "github:operator", TokenID: "tok", Scopes: []string{"write:runs"}}
	if !rule.satisfiedBy(writeRunsOperator) {
		t.Error("an operator bearer holding write:runs is REFUSED by the gate, but POST /v0/work-items files for it")
	}
	runBound := Identity{
		Subject: "mcp:run:44444444-4444-4444-4444-444444444444",
		TokenID: "tok",
		Scopes:  []string{scopeRunBoundRead},
	}
	if !rule.satisfiedBy(runBound) {
		t.Error("a run-bound mcp:read token is REFUSED by the gate, but POST /v0/work-items admits it on its own run")
	}
}

// TestHandleMCP_FileIssueRequiresWriteRuns is the CROSS-BOUNDARY assertion for
// the #4481 row: the table entry, the gate predicate and the wire envelope on
// ONE path, over the real HTTP /mcp transport. A bearer minted read:runs draws
// 403 insufficient_scope naming write:runs and NEVER reaches the per-request
// tool registry; a bearer minted write:runs passes the gate into the SDK.
func TestHandleMCP_FileIssueRequiresWriteRuns(t *testing.T) {
	const tool = "fishhawk_file_issue"
	t.Run("refused", func(t *testing.T) {
		s, bearer, f := mcpGateServer(t, "read:runs")
		body := assertMCPGateRefusal(t, s, f, newMCPToolCall(tool, bearer),
			http.StatusForbidden, "insufficient_scope")
		if !strings.Contains(body, "required_scope") || !strings.Contains(body, "write:runs") {
			t.Errorf("body = %s, want details.required_scope naming write:runs", body)
		}
	})
	t.Run("admitted", func(t *testing.T) {
		s, bearer, f := mcpGateServer(t, "write:runs")
		status, body := mcpGateResult(t, s, newMCPToolCall(tool, bearer))
		if status == http.StatusForbidden {
			t.Fatalf("a granted-scope tools/call was refused %d: %s", status, body)
		}
		if strings.Contains(body, "insufficient_scope") {
			t.Errorf("granted-scope call drew insufficient_scope: %s", body)
		}
		if f.calls.Load() == 0 {
			t.Error("the granted-scope call never reached the per-request tool registry")
		}
	})
}

// TestMCPToolScopeTable_AuthenticatedOnlyRowsEnumerated pins the EXACT set of
// rows whose anyOf is empty (mcpScopeAuthenticatedOnly), so a new
// authenticated-only row, or a write row regressing back to one (the #4481
// fishhawk_file_issue shape), fails loudly and is reviewed. Every row here
// must front a handler that enforces no scope beyond authentication, or one
// whose write arm the inner handler enforces itself ("gate admits, handler
// refuses"): fishhawk_release_notes (prepare: write:runs),
// fishhawk_delegation_confirm (confirm/lower: write:approvals) and
// fishhawk_preview_issue (writes nothing; refuses run-bound tokens).
func TestMCPToolScopeTable_AuthenticatedOnlyRowsEnumerated(t *testing.T) {
	want := []string{
		"fishhawk_await_audit",
		"fishhawk_await_children",
		"fishhawk_await_review",
		"fishhawk_await_stage",
		"fishhawk_delegation",
		"fishhawk_delegation_confirm",
		"fishhawk_doctor",
		"fishhawk_get_active_run",
		"fishhawk_get_campaign_status",
		"fishhawk_get_plan",
		"fishhawk_get_run_status",
		"fishhawk_init",
		"fishhawk_list_audit",
		"fishhawk_list_runs",
		"fishhawk_list_schedules",
		"fishhawk_list_scope_amendments",
		"fishhawk_precedent",
		"fishhawk_preview_issue",
		"fishhawk_release_notes",
		"fishhawk_runtime_calibration",
		"fishhawk_validate",
		"fishhawk_verify_run",
	}
	var got []string
	for tool, rule := range mcpToolScopes {
		if len(rule.anyOf) == 0 {
			got = append(got, tool)
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("authenticated-only rows changed:\n got  %v\n want %v\n"+
			"a row added here must front a handler that checks no scope (or whose inner handler enforces its "+
			"write arm); a write route must name its scope instead (#4481)", got, want)
	}
}

// TestMCPToolScopeTable_HandoverBriefMirrorsReadAudit pins the
// fishhawk_handover_brief row (E76.4 / #3767) to the endpoint it fronts:
// handleGetHandoverBrief enforces requireWriteScope(scopeHandoverBriefRead),
// which IS scopeDigestRead (read:audit), and admits no run-bound subject. A
// read:audit bearer is ADMITTED; a write:approvals-only bearer and a run-bound
// fhm_ token at its full vocabulary are REFUSED — the row is neither stricter
// nor looser than the handler.
func TestMCPToolScopeTable_HandoverBriefMirrorsReadAudit(t *testing.T) {
	rule, ok := mcpToolScopeFor("fishhawk_handover_brief")
	if !ok {
		t.Fatal("fishhawk_handover_brief has no mcpToolScopes entry; it would be refused mcp_tool_not_authorized at runtime")
	}
	if len(rule.anyOf) != 1 || rule.anyOf[0] != scopeHandoverBriefRead || scopeHandoverBriefRead != "read:audit" {
		t.Errorf("rule.anyOf = %v, want exactly [%s] (read:audit) — the scope handleGetHandoverBrief enforces", rule.anyOf, scopeHandoverBriefRead)
	}
	if rule.runBoundSubjectOK {
		t.Error("runBoundSubjectOK is set, but handleGetHandoverBrief authorizes no run-bound token by subject")
	}
	cases := []struct {
		name string
		id   Identity
		want bool
	}{
		{"read:audit operator admitted", Identity{Subject: "svc:operator", TokenID: "tok", Scopes: []string{"read:audit"}}, true},
		{"write:approvals-only operator refused", Identity{Subject: "svc:operator", TokenID: "tok", Scopes: []string{"write:approvals"}}, false},
		{"run-bound token refused", Identity{
			Subject: "mcp:run:33333333-3333-3333-3333-333333333333", TokenID: "tok",
			Scopes: []string{scopeRunBoundRead, scopeRunBoundRetry, scopeRunBoundScopeAmendments, scopeRunBoundMessages},
		}, false},
	}
	for _, c := range cases {
		if got := rule.satisfiedBy(c.id); got != c.want {
			t.Errorf("%s: satisfiedBy = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestMCPToolScopeTable_CrewMessageRowsMirrorHandlers pins the three E77.3
// (#3737) crew-message rows to the handlers in crewmessage.go, in BOTH
// directions: each identity below is admitted by the row exactly when the
// mirrored handler's scope predicate admits it. The implement-stage run-bound
// token (never minted write:messages) is refused by EVERY row — the gate-level
// face of ARCHITECTURE.md §6 invariant #8.
func TestMCPToolScopeTable_CrewMessageRowsMirrorHandlers(t *testing.T) {
	const runSubject = "mcp:run:44444444-4444-4444-4444-444444444444"
	implementToken := Identity{Subject: runSubject, TokenID: "tok",
		Scopes: []string{scopeRunBoundRead, scopeRunBoundRetry, scopeRunBoundScopeAmendments}}
	planToken := Identity{Subject: runSubject, TokenID: "tok",
		Scopes: []string{scopeRunBoundRead, scopeRunBoundRetry, scopeRunBoundMessages}}
	writeStagesOp := Identity{Subject: "svc:operator", TokenID: "tok", Scopes: []string{"write:stages"}}
	readAuditOp := Identity{Subject: "svc:operator", TokenID: "tok", Scopes: []string{"read:audit"}}
	neitherOp := Identity{Subject: "svc:operator", TokenID: "tok", Scopes: []string{"write:runs", "write:approvals"}}

	cases := []struct {
		tool string
		id   Identity
		name string
		want bool
	}{
		// handleSendCrewMessage: run-bound needs write:messages; else write:stages.
		{"fishhawk_send_crew_message", implementToken, "implement token", false},
		{"fishhawk_send_crew_message", planToken, "plan token", true},
		{"fishhawk_send_crew_message", writeStagesOp, "write:stages operator", true},
		{"fishhawk_send_crew_message", readAuditOp, "read:audit-only operator", false},
		{"fishhawk_send_crew_message", neitherOp, "write:runs+approvals operator", false},
		// authorizeCrewRead: run-bound needs write:messages; else read:audit.
		{"fishhawk_read_crew_messages", implementToken, "implement token", false},
		{"fishhawk_read_crew_messages", planToken, "plan token", true},
		{"fishhawk_read_crew_messages", readAuditOp, "read:audit operator", true},
		{"fishhawk_read_crew_messages", writeStagesOp, "write:stages-only operator", false},
		{"fishhawk_read_crew_messages", neitherOp, "write:runs+approvals operator", false},
		// handleDecideCrewEscalation: run-bound refused self_decision; else write:stages.
		{"fishhawk_decide_crew_escalation", implementToken, "implement token", false},
		{"fishhawk_decide_crew_escalation", planToken, "plan token", false},
		{"fishhawk_decide_crew_escalation", writeStagesOp, "write:stages operator", true},
		{"fishhawk_decide_crew_escalation", readAuditOp, "read:audit-only operator", false},
		// handleConvertCrewFindingToConcern: run-bound refused self_decision; else write:stages.
		{"fishhawk_convert_crew_finding", implementToken, "implement token", false},
		{"fishhawk_convert_crew_finding", planToken, "plan token", false},
		{"fishhawk_convert_crew_finding", writeStagesOp, "write:stages operator", true},
		{"fishhawk_convert_crew_finding", readAuditOp, "read:audit-only operator", false},
		{"fishhawk_convert_crew_finding", neitherOp, "write:runs+approvals operator", false},
	}
	for _, c := range cases {
		rule, ok := mcpToolScopeFor(c.tool)
		if !ok {
			t.Errorf("%s: no mcpToolScopes entry; it would be refused mcp_tool_not_authorized at runtime", c.tool)
			continue
		}
		if rule.runBoundSubjectOK {
			t.Errorf("%s: runBoundSubjectOK is set, but no crew-message handler admits a run-bound token by subject alone", c.tool)
		}
		if got := rule.satisfiedBy(c.id); got != c.want {
			t.Errorf("%s / %s (%s): satisfiedBy = %v, want %v — the row must mirror its handler in both directions",
				c.tool, c.name, strings.Join(c.id.Scopes, " "), got, c.want)
		}
	}
	if got := []string{scopeRunBoundMessages, scopeWriteMessages}; got[0] != "write:messages" || got[1] != got[0] {
		t.Errorf("scopeRunBoundMessages / scopeWriteMessages = %q, want both the write:messages spelling", got)
	}
}

// TestMCPToolScopeTable_AnswerDivergenceMirrorsHandler (E75.5 / #3733):
// fishhawk_answer_divergence carries EXACTLY handleAnswerDivergence's
// predicate — write:stages OR write:fixups, the waive/defer posture — so a
// fixups-only token the handler admits is not refused at the MCP gate, and a
// read-only token the handler refuses is not admitted.
func TestMCPToolScopeTable_AnswerDivergenceMirrorsHandler(t *testing.T) {
	rule, ok := mcpToolScopeFor("fishhawk_answer_divergence")
	if !ok {
		t.Fatal("fishhawk_answer_divergence has no scope rule")
	}
	defer_, _ := mcpToolScopeFor("fishhawk_defer_concern")
	if strings.Join(rule.anyOf, ",") != strings.Join(defer_.anyOf, ",") {
		t.Errorf("answer_divergence anyOf = %v, want the defer posture %v", rule.anyOf, defer_.anyOf)
	}
	if strings.Join(rule.anyOf, ",") != "write:stages,"+scopeFixupAlternate {
		t.Errorf("answer_divergence anyOf = %v, want [write:stages %s]", rule.anyOf, scopeFixupAlternate)
	}
}

// TestMCPToolScopeTable_CommsDispositionsMirrorsHandler (#4016):
// fishhawk_record_comms_dispositions carries EXACTLY the predicate
// handleRecordCommsDispositions enforces through requireOperatorCapture —
// write:approvals, unconditionally, with no run-bound subject admitted (the
// handler refuses every run-bound token run_token_forbidden). A
// write:approvals operator is ADMITTED; an operator holding only other write
// scopes and a run-bound fhm_ token at its full vocabulary are REFUSED, so the
// row is neither stricter nor looser than the handler.
func TestMCPToolScopeTable_CommsDispositionsMirrorsHandler(t *testing.T) {
	rule, ok := mcpToolScopeFor("fishhawk_record_comms_dispositions")
	if !ok {
		t.Fatal("fishhawk_record_comms_dispositions has no mcpToolScopes entry; it would be refused mcp_tool_not_authorized at runtime")
	}
	if len(rule.anyOf) != 1 || rule.anyOf[0] != "write:approvals" {
		t.Errorf("rule.anyOf = %v, want exactly [write:approvals] — the scope requireOperatorCapture enforces", rule.anyOf)
	}
	if rule.runBoundSubjectOK {
		t.Error("runBoundSubjectOK is set, but handleRecordCommsDispositions refuses every run-bound token")
	}
	cases := []struct {
		name string
		id   Identity
		want bool
	}{
		{"write:approvals operator admitted", Identity{Subject: "svc:operator", TokenID: "tok", Scopes: []string{"write:approvals"}}, true},
		{"write:stages+runs operator refused", Identity{Subject: "svc:operator", TokenID: "tok", Scopes: []string{"write:stages", "write:runs", "read:audit"}}, false},
		{"run-bound token refused", Identity{
			Subject: "mcp:run:55555555-5555-5555-5555-555555555555", TokenID: "tok",
			Scopes: []string{scopeRunBoundRead, scopeRunBoundRetry, scopeRunBoundScopeAmendments, scopeRunBoundMessages},
		}, false},
	}
	for _, c := range cases {
		if got := rule.satisfiedBy(c.id); got != c.want {
			t.Errorf("%s: satisfiedBy = %v, want %v", c.name, got, c.want)
		}
	}
	upkeep, _ := mcpToolScopeFor("fishhawk_record_upkeep_dispositions")
	if strings.Join(rule.anyOf, ",") != strings.Join(upkeep.anyOf, ",") {
		t.Errorf("comms anyOf = %v, want the upkeep capture posture %v (both captures share requireOperatorCapture)", rule.anyOf, upkeep.anyOf)
	}
}

// TestMCPToolScopeTable_AppendCampaignItemsMirrorsHandler (E86.47 / #4358):
// fishhawk_append_campaign_items carries EXACTLY the predicate
// handleAppendCampaignItems enforces — requireWriteScope("write:campaigns").
// A write:campaigns token passes the handler's scope guard (it reaches the
// campaign lookup, 404) and the row; a token holding only other write scopes
// is refused by both, so the row is neither stricter nor looser.
func TestMCPToolScopeTable_AppendCampaignItemsMirrorsHandler(t *testing.T) {
	rule, ok := mcpToolScopeFor("fishhawk_append_campaign_items")
	if !ok {
		t.Fatal("fishhawk_append_campaign_items has no scope rule")
	}
	if strings.Join(rule.anyOf, ",") != "write:campaigns" || rule.runBoundSubjectOK {
		t.Errorf("rule = %+v, want anyOf [write:campaigns], no run-bound subject", rule)
	}
	s := New(Config{Addr: "127.0.0.1:0", CampaignRepo: newFakeCampaignRepo()})
	id := uuid.NewString()
	post := func(scopes ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v0/campaigns/"+id+"/items", strings.NewReader(`{"items":["20"]}`))
		req.SetPathValue("campaign_id", id)
		w := httptest.NewRecorder()
		s.handleAppendCampaignItems(w, withIdentity(req, Identity{Subject: "github:op", TokenID: "tok", Scopes: scopes}))
		return w
	}
	if w := post("write:campaigns"); w.Code != http.StatusNotFound {
		t.Errorf("write:campaigns = %d %s, want past the scope guard (404)", w.Code, w.Body.String())
	}
	if !rule.satisfiedBy(Identity{Subject: "github:op", TokenID: "tok", Scopes: []string{"write:campaigns"}}) {
		t.Error("row refuses write:campaigns, which the handler admits")
	}
	other := []string{"write:runs", "write:approvals", "write:stages"}
	if w := post(other...); w.Code != http.StatusForbidden {
		t.Errorf("other write scopes = %d, want 403 insufficient_scope", w.Code)
	}
	if rule.satisfiedBy(Identity{Subject: "github:op", TokenID: "tok", Scopes: other}) {
		t.Error("row admits a token the handler refuses")
	}
}

// TestMCPToolScopeTable_GetMergeQueueMirrorsHandler (E87.4 / #4295):
// fishhawk_get_merge_queue carries EXACTLY the predicate handleGetMergeQueue
// enforces — a run-bound subject refused 403 run_token_forbidden (approval
// condition C10, so no runBoundSubjectOK), otherwise
// requireWriteScope(scopeMergeQueueRead) = read:audit. A read:audit token
// passes the handler (200) and the row; a token holding only other scopes,
// and a run-bound token even carrying read:audit, are refused by both.
func TestMCPToolScopeTable_GetMergeQueueMirrorsHandler(t *testing.T) {
	rule, ok := mcpToolScopeFor("fishhawk_get_merge_queue")
	if !ok {
		t.Fatal("fishhawk_get_merge_queue has no scope rule")
	}
	if strings.Join(rule.anyOf, ",") != scopeGateViewRead || rule.runBoundSubjectOK {
		t.Errorf("rule = %+v, want anyOf [%s], no run-bound subject", rule, scopeGateViewRead)
	}
	store := &mcqFaultStore{activeBases: func() ([]mergequeue.Base, error) { return nil, nil }}
	s := New(Config{Addr: "127.0.0.1:0", MergeQueue: store})
	get := func(id Identity) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v0/repos/x/y/merge-queue", nil)
		req.SetPathValue("owner", "x")
		req.SetPathValue("name", "y")
		w := httptest.NewRecorder()
		s.handleGetMergeQueue(w, withIdentity(req, id))
		return w
	}
	audit := Identity{Subject: "github:op", TokenID: "tok", Scopes: []string{scopeGateViewRead}}
	if w := get(audit); w.Code != http.StatusOK {
		t.Errorf("read:audit = %d %s, want 200", w.Code, w.Body.String())
	}
	if !rule.satisfiedBy(audit) {
		t.Error("row refuses read:audit, which the handler admits")
	}
	other := Identity{Subject: "github:op", TokenID: "tok", Scopes: []string{"read:runs", "write:runs", "write:approvals"}}
	if w := get(other); w.Code != http.StatusForbidden {
		t.Errorf("other scopes = %d, want 403 insufficient_scope", w.Code)
	}
	if rule.satisfiedBy(other) {
		t.Error("row admits a token the handler refuses")
	}
	runBound := Identity{Subject: "mcp:run:" + uuid.NewString(), TokenID: "tok-run", Scopes: []string{scopeRunBoundRead}}
	if w := get(runBound); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "run_token_forbidden") {
		t.Errorf("run-bound = %d %s, want 403 run_token_forbidden", w.Code, w.Body.String())
	}
	if rule.satisfiedBy(runBound) {
		t.Error("row admits a run-bound token the handler refuses")
	}
}
