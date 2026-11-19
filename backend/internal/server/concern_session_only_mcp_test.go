package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mcpserver"
)

// ADR-143 D1 (#5228) across the MCP tool layer. Each of the four server-check
// clearing tools stays registered and passes the backend refusal through
// unchanged (ADR-143 Enablement), so a static human-subject bearer driving it
// over EITHER MCP transport — stdio (mcpserver.NewServer over the SDK's
// in-memory transport, dialling the real s.Handler() on an httptest listener)
// or HTTP /mcp (the route's per-request tool server re-dialling fishhawkd's
// own listener) — gets a tool error naming concern_requires_session, and the
// seeded row and the audit fake read back unchanged.
//
// The positive control on each transport waives a REVIEWER-raised concern
// with the SAME bearer through the SAME tool: it succeeds and the row reads
// back waived, so a transport that never reached the handler (or reached it
// with a different credential) cannot pass the refusal arm.
//
// COUNTERFACTUAL (hand-run, #5228 slice 1; restored byte-identically):
//
//  1. D1 condition made permissive (refuseServerCheckClearOffSession's
//     `if kind == identity.CredentialSession {` → `... || kind != "" {`):
//     every arm of TestServerCheckClearSession_MCPStdioRefused and
//     TestServerCheckClearSession_MCPRouteRefused went RED — the tool call
//     succeeded (no concern_requires_session in the result) and the row read
//     back waived/deferred/addressed with one clear entry appended.

// concernClearTool is one of the four MCP tools that clears a concern, with
// the clearVerbCase whose seed, start state and audit category it shares.
type concernClearTool struct {
	tool string
	vc   clearVerbCase
	args func(row *concern.Concern) map[string]any
}

var concernClearTools = []concernClearTool{
	{tool: "fishhawk_waive_concern", vc: clearVerbCases[0], args: func(row *concern.Concern) map[string]any {
		return map[string]any{"concern_id": row.ID.String(), "reason": "known test fixture", "judgement": "no_defect"}
	}},
	{tool: "fishhawk_waive_concerns", vc: clearVerbCases[1], args: func(row *concern.Concern) map[string]any {
		return map[string]any{"run_id": row.RunID.String(), "concern_ids": []string{row.ID.String()},
			"reason": "known test fixture", "judgement": "no_defect"}
	}},
	{tool: "fishhawk_defer_concern", vc: clearVerbCases[2], args: func(row *concern.Concern) map[string]any {
		return map[string]any{"concern_id": row.ID.String(), "parent_epic": "#389", "n": "3", "judgement": "no_defect"}
	}},
	{tool: "fishhawk_resolve_concerns", vc: clearVerbCases[3], args: func(row *concern.Concern) map[string]any {
		return map[string]any{"run_id": row.RunID.String(), "concern_ids": []string{row.ID.String()},
			"evidence": "re-ran the reproduction", "judgement": "defect"}
	}},
}

// webOnlyClearVerbs are the D2 verbs with NO MCP tool (ADR-143 A1, #5229):
// the waive-proposal decisions. Only a human on a Web UI session confirms or
// rejects a proposal, so no tool exists to pass a refusal through.
var webOnlyClearVerbs = map[string]bool{
	clearVerbConfirmWaiveProposal: true,
	clearVerbRejectWaiveProposal:  true,
}

func TestConcernClearTools_TableCoversEveryVerb(t *testing.T) {
	// The web-only set is pinned: growing it removes a verb from this
	// MCP-refusal coverage, so it is a visible change here.
	if len(webOnlyClearVerbs) != 2 || !webOnlyClearVerbs[clearVerbConfirmWaiveProposal] || !webOnlyClearVerbs[clearVerbRejectWaiveProposal] {
		t.Fatalf("webOnlyClearVerbs = %v, want exactly {%s, %s}", webOnlyClearVerbs, clearVerbConfirmWaiveProposal, clearVerbRejectWaiveProposal)
	}
	var toolVerbs []string
	for _, v := range serverCheckClearAction.Verbs {
		if !webOnlyClearVerbs[v] {
			toolVerbs = append(toolVerbs, v)
		}
	}
	if len(concernClearTools) != len(toolVerbs) {
		t.Fatalf("MCP clear tools = %d, want one per D2 verb with an MCP tool %v", len(concernClearTools), toolVerbs)
	}
	for i, ct := range concernClearTools {
		if ct.vc.verb != toolVerbs[i] {
			t.Errorf("tool %s drives verb %q, want %q", ct.tool, ct.vc.verb, toolVerbs[i])
		}
	}
}

// stdioSession connects an in-memory MCP client to a stdio-shaped
// mcpserver.NewServer whose backend is baseURL and whose token is bearer.
func stdioSession(t *testing.T, baseURL, bearer string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := mcpserver.NewServer(mcpserver.Config{BackendURL: baseURL, APIToken: bearer})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("mcp server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "session-only-stdio", Version: "0"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("mcp client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// assertToolRefusedSession checks a tool call failed with the backend's
// concern_requires_session refusal surfaced verbatim.
func assertToolRefusedSession(t *testing.T, tool string, res *mcp.CallToolResult, err error) {
	t.Helper()
	if err == nil && !res.IsError {
		t.Errorf("%s SUCCEEDED on a static bearer; want a tool error naming %s", tool, errCodeConcernRequiresSession)
		return
	}
	if text := toolFailureText(res, err); !strings.Contains(text, errCodeConcernRequiresSession) {
		t.Errorf("%s failure = %q, want it to name %s", tool, text, errCodeConcernRequiresSession)
	}
}

// assertReviewerWaiveSucceeds is the transport's positive control: the SAME
// bearer through fishhawk_waive_concern on a reviewer-raised concern is
// admitted and the row reads back waived.
func assertReviewerWaiveSucceeds(t *testing.T, cs *mcp.ClientSession, cr *fakeConcernRepo, runID uuid.UUID) {
	t.Helper()
	row := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "naming nit")
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "fishhawk_waive_concern",
		Arguments: map[string]any{"concern_id": row.ID.String(), "reason": "accepted trade-off", "judgement": "no_defect"},
	})
	if err != nil || res.IsError {
		t.Fatalf("positive control: reviewer waive over MCP failed: %s", toolFailureText(res, err))
	}
	assertState(t, cr, row.ID, concern.StateWaived)
}

// TestServerCheckClearSession_MCPStdioRefused: the stdio MCP server (the
// fishhawk-mcp binary's tool library) on a static human-subject bearer is
// refused by each of the four tools with concern_requires_session; the row
// stays in its start state and no clear entry is appended.
func TestServerCheckClearSession_MCPStdioRefused(t *testing.T) {
	for _, ct := range concernClearTools {
		t.Run(ct.tool, func(t *testing.T) {
			fx := newSessionOnlyFixture(t)
			ts := httptest.NewServer(fx.s.Handler())
			t.Cleanup(ts.Close)
			runID := uuid.New()
			row := ct.vc.seed(t, fx, runID)
			cs := stdioSession(t, ts.URL, fx.bearerFor(t, identity.CredentialStatic, runID))

			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: ct.tool, Arguments: ct.args(row)})

			assertToolRefusedSession(t, ct.tool, res, err)
			fx.assertCommitted(t, row, ct.vc.before, ct.vc.category, 0)
			if ct.vc.verb == clearVerbDefer && fx.fp.called {
				t.Errorf("provider.File was called; a refused defer must file no work item")
			}
			assertReviewerWaiveSucceeds(t, cs, fx.cr, runID)
		})
	}
}

// concernMCPFixture is the HTTP /mcp harness (newMCPFixture) with the concern
// store, an approval-capable run repo, the work provider, the audit fake and a
// cookie session store wired, so a tool call's inner re-dial reaches the real
// clearing handlers.
type concernMCPFixture struct {
	*mcpFixture
	runs    *approvalRunRepo
	au      *auditFake
	cr      *fakeConcernRepo
	fp      *fakeWorkProvider
	session *http.Cookie
}

func newConcernMCPFixture(t *testing.T) *concernMCPFixture {
	t.Helper()
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)
	fx := &concernMCPFixture{
		runs: newApprovalRunRepo(),
		au:   newAuditFake(),
		cr:   newFakeConcernRepo(),
		fp:   fp,
	}
	authDB := newFakeAuthRepo()
	fx.mcpFixture = newMCPFixture(t, func(c *Config) {
		c.RunRepo = fx.runs
		c.AuditRepo = fx.au
		c.ConcernRepo = fx.cr
		c.AuthRepo = authDB
		c.GitHub = newEpicGitHubClient(t, 7788, "[E22] The parent epic")
	})
	_, sess, err := authDB.SignIn(context.Background(), "github", auth.GitHubProfile{ID: 4242, Login: "alice", Name: "Alice"}, uuid.New())
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	fx.session = &http.Cookie{Name: auth.SessionCookieName, Value: sess.PlainText}
	return fx
}

// staticBearer mints a github:alice api token on the /mcp fixture's token
// store and STAMPS auth_method static (fakeTokenRepo.Issue stores an empty auth_method, which
// classifies as unrecorded).
func (fx *concernMCPFixture) staticBearer(t *testing.T) string {
	t.Helper()
	tok, err := fx.tokens.Issue(context.Background(), "github:alice", clearScopes)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	fx.tokens.mu.Lock()
	fx.tokens.rows[tok.ID].AuthMethod = "static"
	fx.tokens.mu.Unlock()
	return tok.PlainText
}

// seed seeds the run and one server_check row in the tool's start state.
func (fx *concernMCPFixture) seed(t *testing.T, ct concernClearTool, runID uuid.UUID) *concern.Concern {
	t.Helper()
	seedDeferRun(fx.runs, runID)
	row := seedServerCheckRow(t, fx.cr, runID, uuid.New(), "high")
	if ct.vc.before == concern.StateAddressedPending {
		if err := fx.cr.MarkAddressedPending(context.Background(), []uuid.UUID{row.ID}, "routed by fix-up"); err != nil {
			t.Fatalf("route concern: %v", err)
		}
	}
	return row
}

func (fx *concernMCPFixture) assertCommitted(t *testing.T, row *concern.Concern, state concern.State, category string, wantEntries int) {
	t.Helper()
	assertState(t, fx.cr, row.ID, state)
	if n := len(auditEntriesByCategory(fx.au, category)); n != wantEntries {
		t.Errorf("%s entries = %d, want %d", category, n, wantEntries)
	}
}

// TestServerCheckClearSession_MCPRouteRefused: HTTP /mcp on a static
// human-subject bearer is refused by each of the four tools with
// concern_requires_session. The recorded inner re-dial to the clearing route
// carried the SAME bearer, so the refusal is the handler's D1 arm on that
// credential, and the row and audit read back unchanged.
func TestServerCheckClearSession_MCPRouteRefused(t *testing.T) {
	for _, ct := range concernClearTools {
		t.Run(ct.tool, func(t *testing.T) {
			fx := newConcernMCPFixture(t)
			runID := uuid.New()
			row := fx.seed(t, ct, runID)
			bearer := fx.staticBearer(t)
			ctx, cancel := context.WithTimeout(context.Background(), mcpRouteDeadline)
			defer cancel()
			cs := mcpClientSession(t, ctx, fx.ts.URL, bearer, nil)

			fx.recorder.reset()
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: ct.tool, Arguments: ct.args(row)})

			assertToolRefusedSession(t, ct.tool, res, err)
			fx.assertCommitted(t, row, ct.vc.before, ct.vc.category, 0)
			if ct.vc.verb == clearVerbDefer && fx.fp.called {
				t.Errorf("provider.File was called; a refused defer must file no work item")
			}
			path, _ := ct.vc.request(row, false)
			if inner := lastInboundFor(t, fx.recorder.snapshot(), path); inner.auth != "Bearer "+bearer {
				t.Errorf("inner re-dial to %s carried %q, want the outer static bearer", path, inner.auth)
			}
			assertReviewerWaiveSucceeds(t, cs, fx.cr, runID)
		})
	}
}
