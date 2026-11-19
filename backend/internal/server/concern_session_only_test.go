package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mcptoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ADR-143 D1 (#5228) through the REAL middleware: every request below goes
// through s.Handler(), so bearerAuth resolves the credential and the CSRF
// middleware runs, exactly as on a deployment. Each arm seeds a raised (or,
// for resolve, addressed_pending) server_check row BY CONSTRUCTION and reads
// the row AND the audit fake back after the call returns, so a deleted control
// surfaces as committed state (the row cleared, a clear entry appended), never
// only as a matching error identity.
//
// The refused subject is the SAME non-agent subject on every bearer kind
// (github:alice), and the requests carry no delegated flag, so the
// pre-ADR-143 agent-token and delegated arms do not fire: the D1 arm is the
// only control in the path.
//
// COUNTERFACTUALS (hand-run, #5228 slice 0; each restored byte-identically):
//
//  1. D1 condition made permissive (refuseServerCheckClearOffSession's
//     `if kind == identity.CredentialSession {` → `... || kind != "" {`, so
//     every kind is admitted): every bearer arm of
//     TestServerCheckClearSession_RefusesBearerKinds went RED — status 200
//     instead of 403, the row read back waived/deferred/addressed and one
//     clear entry was appended per arm — as did
//     TestServerCheckClearSession_ForgedChannelClaimsGrantNothing,
//     TestRefuseNonHumanServerCheckClear_D1ArmDirect and
//     TestResolveConcerns_ServerCheckRefusedOnStaticBearer.
//  2. resolve_concerns.go per-row call prefixed `false &&`: the resolve arm of
//     TestServerCheckClearSession_RefusesBearerKinds went RED (status 200,
//     row addressed, one concern_resolved_with_evidence row) — the pre-body
//     isAgentSubject guard does not fire for github:alice.
//  3. registry mark `Enforced: true,` → `Enforced: false,`: the same bearer
//     arms went RED, and TestHumanOnlyActions_EnforcedSetPinned went RED.
//  4. bulk_waive.go per-row call deleted:
//     TestServerCheckClearSession_BulkRefusedWhole went RED — both rows read
//     back waived and two concern_waived rows were appended.
//  5. isAgentSubject case deleted from refuseNonHumanServerCheckClear:
//     TestServerCheckClearSession_PreservesAgentRefusal went RED — the
//     operator-agent bearer got concern_requires_session instead of
//     concern_requires_human (D1 alone still refused it, state unchanged).
//  6. delegated case deleted: TestServerCheckClearSession_PreservesDelegatedRefusal
//     went RED — the session request with delegated:true was admitted (200,
//     row waived), proving D1 never admits what the present guard refuses.

// sessionOnlyFixture is a server with every credential store wired behind the
// real middleware: a cookie session store, the api-token store, the run-bound
// MCP token store and an enabled OAuth AS with an access-token store.
type sessionOnlyFixture struct {
	s    *Server
	runs *approvalRunRepo
	// gau is the fixture's AuditRepo: the guarded test wrapper (#5229), so the
	// waive-proposal decisions reach audit.GuardedChainAppender. au is its
	// embedded fake, which every existing verb and assertion reads.
	gau     *guardedAuditWrapper
	au      *auditFake
	cr      *fakeConcernRepo
	fp      *fakeWorkProvider
	tokens  *fakeTokenRepo
	mcp     *fakeMCPTokenRepo
	oauth   *fakeOAuthStore
	authDB  *fakeAuthRepo
	session *http.Cookie
	csrf    string
}

// clearScopes is every write scope the four verbs accept.
var clearScopes = []string{"read:runs", "write:stages", "write:fixups"}

func newSessionOnlyFixture(t *testing.T) *sessionOnlyFixture {
	t.Helper()
	fp := &fakeWorkProvider{}
	registerFakeProvider(t, fp)
	gau := newGuardedAuditWrapper()
	fx := &sessionOnlyFixture{
		runs:   newApprovalRunRepo(),
		gau:    gau,
		au:     gau.auditFake,
		cr:     newFakeConcernRepo(),
		fp:     fp,
		tokens: newFakeTokenRepo(),
		mcp:    newFakeMCPTokenRepo(),
		oauth:  newFakeOAuthStore(),
		authDB: newFakeAuthRepo(),
	}
	fx.s = New(Config{
		Addr:             "127.0.0.1:0",
		RunRepo:          fx.runs,
		AuditRepo:        fx.gau,
		ConcernRepo:      fx.cr,
		GitHub:           newEpicGitHubClient(t, 7788, "[E22] The parent epic"),
		APITokenRepo:     fx.tokens,
		MCPTokenRepo:     fx.mcp,
		AuthRepo:         fx.authDB,
		OAuthASIssuer:    testIssuer,
		OAuthStore:       fx.oauth,
		OAuthCIMDFetcher: newCIMDFetcher(newCIMD()),
	})
	// A forge sign-in for github:alice: the only session creator (ADR-123 D1).
	// The session is bound to a workspace account (a write tier refuses an
	// unbound one); the seeded runs are untenanted, so ownership admits every
	// credential and the D2 guard is the only refusal in the path.
	_, sess, err := fx.authDB.SignIn(context.Background(), "github", auth.GitHubProfile{ID: 4242, Login: "alice", Name: "Alice"}, uuid.New())
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	csrfTok, err := generateCSRFToken()
	if err != nil {
		t.Fatalf("generateCSRFToken: %v", err)
	}
	fx.session = &http.Cookie{Name: auth.SessionCookieName, Value: sess.PlainText}
	fx.csrf = csrfTok
	return fx
}

// bearerWithAuthMethod mints an api token and STAMPS its stored auth_method
// directly (fakeTokenRepo.Issue stores an empty auth_method, which classifies
// as unrecorded), returning the plaintext.
func (fx *sessionOnlyFixture) bearerWithAuthMethod(t *testing.T, subject, authMethod string) string {
	t.Helper()
	tok, err := fx.tokens.Issue(context.Background(), subject, clearScopes)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	fx.tokens.mu.Lock()
	fx.tokens.rows[tok.ID].AuthMethod = authMethod
	fx.tokens.mu.Unlock()
	return tok.PlainText
}

// bearerFor returns a bearer of the named credential kind for subject
// github:alice (mcp_run is bound to runID and carries its own subject).
func (fx *sessionOnlyFixture) bearerFor(t *testing.T, kind string, runID uuid.UUID) string {
	t.Helper()
	const subject = "github:alice"
	switch kind {
	case identity.CredentialStatic:
		return fx.bearerWithAuthMethod(t, subject, "static")
	case identity.CredentialUnrecorded:
		return fx.bearerWithAuthMethod(t, subject, "")
	case identity.CredentialUnknown:
		return fx.bearerWithAuthMethod(t, subject, "saml-assertion")
	case identity.CredentialDeviceFlow:
		tok, err := fx.tokens.IssueOAuth(context.Background(), subject, clearScopes, "github")
		if err != nil {
			t.Fatalf("issue oauth token: %v", err)
		}
		return tok.PlainText
	case identity.CredentialOAuthAS:
		plain := "fho_sessiononly" + strings.ReplaceAll(uuid.NewString(), "-", "")
		fx.oauth.seedAccessToken(plain, subject, testResource, "", clearScopes)
		return plain
	case identity.CredentialMCPRun:
		tok, err := fx.mcp.Issue(context.Background(), mcptoken.IssueParams{RunID: runID, Scopes: []string{"mcp:read", "write:fixups", "write:stages"}})
		if err != nil {
			t.Fatalf("issue mcp token: %v", err)
		}
		return tok.PlainText
	}
	t.Fatalf("unknown credential kind %q", kind)
	return ""
}

// clearVerbCase is one of the D2 verbs driven over HTTP: the four direct
// clears plus the waive-proposal confirm (ADR-143 A1, #5229). Reject clears
// nothing, so it has no case here; waive_proposal_decide_test.go drives it.
type clearVerbCase struct {
	verb     string
	category string
	cleared  concern.State
	// before is the state the seeded row starts in.
	before concern.State
	// seed seeds the run and one server_check row in the verb's start state.
	seed func(t *testing.T, fx *sessionOnlyFixture, runID uuid.UUID) *concern.Concern
	// request builds the path and JSON body.
	request func(row *concern.Concern, delegated bool) (string, string)
}

func seedRaisedServerCheck(t *testing.T, fx *sessionOnlyFixture, runID uuid.UUID) *concern.Concern {
	t.Helper()
	seedDeferRun(fx.runs, runID)
	return seedServerCheckRow(t, fx.cr, runID, uuid.New(), "high")
}

func seedRoutedServerCheck(t *testing.T, fx *sessionOnlyFixture, runID uuid.UUID) *concern.Concern {
	t.Helper()
	row := seedRaisedServerCheck(t, fx, runID)
	if err := fx.cr.MarkAddressedPending(context.Background(), []uuid.UUID{row.ID}, "routed by fix-up"); err != nil {
		t.Fatalf("route concern: %v", err)
	}
	return row
}

// confirmProposalSeqs keeps, per seeded concern id, the proposal sequence the
// confirm case's request builder names.
var confirmProposalSeqs sync.Map

// seedProposedServerCheck seeds a raised server_check row, a
// pull_request_opened row naming head A and a concern_waive_proposed row at
// that head (actor: the operator agent), so a session confirm is admitted.
func seedProposedServerCheck(t *testing.T, fx *sessionOnlyFixture, runID uuid.UUID) *concern.Concern {
	t.Helper()
	row := seedRaisedServerCheck(t, fx, runID)
	fx.gau.seed(t, runID, "pull_request_opened", map[string]any{"head_sha": proposalHeadA})
	p := seedAgentProposal(t, fx.gau, row, proposalHeadA)
	confirmProposalSeqs.Store(row.ID, p.Sequence)
	return row
}

// confirmProposalSeq returns the proposal sequence seeded for row.
func confirmProposalSeq(row *concern.Concern) int64 {
	v, _ := confirmProposalSeqs.Load(row.ID)
	seq, _ := v.(int64)
	return seq
}

func jsonBody(v map[string]any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

var clearVerbCases = []clearVerbCase{
	{
		verb: clearVerbWaive, category: CategoryConcernWaived, before: concern.StateRaised, cleared: concern.StateWaived,
		seed: seedRaisedServerCheck,
		request: func(row *concern.Concern, delegated bool) (string, string) {
			return "/v0/concerns/" + row.ID.String() + "/waive",
				jsonBody(map[string]any{"reason": "known test fixture", "judgement": "no_defect", "delegated": delegated})
		},
	},
	{
		verb: clearVerbBulkWaive, category: CategoryConcernWaived, before: concern.StateRaised, cleared: concern.StateWaived,
		seed: seedRaisedServerCheck,
		request: func(row *concern.Concern, delegated bool) (string, string) {
			return "/v0/runs/" + row.RunID.String() + "/concerns/waive",
				jsonBody(map[string]any{"concern_ids": []string{row.ID.String()}, "reason": "known test fixture", "judgement": "no_defect", "delegated": delegated})
		},
	},
	{
		verb: clearVerbDefer, category: CategoryConcernDeferred, before: concern.StateRaised, cleared: concern.StateDeferred,
		seed: seedRaisedServerCheck,
		request: func(row *concern.Concern, _ bool) (string, string) {
			return "/v0/concerns/" + row.ID.String() + "/defer",
				jsonBody(map[string]any{"parent_epic": "#389", "n": "3", "judgement": "no_defect"})
		},
	},
	{
		verb: clearVerbResolve, category: CategoryConcernResolvedWithEvidence, before: concern.StateAddressedPending, cleared: concern.StateAddressed,
		seed: seedRoutedServerCheck,
		request: func(row *concern.Concern, _ bool) (string, string) {
			return "/v0/runs/" + row.RunID.String() + "/concerns/resolve",
				jsonBody(map[string]any{"concern_ids": []string{row.ID.String()}, "evidence": "re-ran the reproduction", "judgement": "defect"})
		},
	},
	{
		verb: clearVerbConfirmWaiveProposal, category: CategoryConcernWaived, before: concern.StateRaised, cleared: concern.StateWaived,
		seed: seedProposedServerCheck,
		request: func(row *concern.Concern, delegated bool) (string, string) {
			return "/v0/concerns/" + row.ID.String() + "/waive-proposal/confirm",
				jsonBody(map[string]any{"proposal_sequence": confirmProposalSeq(row), "delegated": delegated})
		},
	},
}

// do sends one POST through the real middleware. bearer and session are
// mutually optional; a session request carries the CSRF double-submit pair.
func (fx *sessionOnlyFixture) do(t *testing.T, path, body, bearer string, session bool, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if session {
		req.AddCookie(fx.session)
		req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: fx.csrf})
		req.Header.Set(CSRFHeaderName, fx.csrf)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	fx.s.Handler().ServeHTTP(w, req)
	return w
}

// assertCommitted reads the row and the audit fake back after the call.
func (fx *sessionOnlyFixture) assertCommitted(t *testing.T, row *concern.Concern, state concern.State, category string, wantEntries int) {
	t.Helper()
	assertState(t, fx.cr, row.ID, state)
	if n := len(auditEntriesByCategory(fx.au, category)); n != wantEntries {
		t.Errorf("%s entries = %d, want %d", category, n, wantEntries)
	}
}

// assertRequiresSession checks the 403 concern_requires_session envelope.
// A wrong status is an Errorf so the committed-state read-back still runs.
func assertRequiresSession(t *testing.T, w *httptest.ResponseRecorder, row *concern.Concern, verb, kind string) {
	t.Helper()
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 concern_requires_session:\n%s", w.Code, w.Body.String())
		return
	}
	env := decodeErrorEnvelope(t, w)
	if env.Code != errCodeConcernRequiresSession {
		t.Errorf("code = %q, want %q:\n%s", env.Code, errCodeConcernRequiresSession, w.Body.String())
		return
	}
	for k, want := range map[string]string{
		"concern_id":      row.ID.String(),
		"provenance":      concern.ProvenanceServerCheck,
		"verb":            verb,
		"credential_kind": kind,
		"remedy":          "web_ui_session",
	} {
		if got, _ := env.Details[k].(string); got != want {
			t.Errorf("details.%s = %q, want %q", k, got, want)
		}
	}
	for _, want := range []string{"Fishhawk Web UI", "attention queue", "No CLI or MCP path exists"} {
		if !strings.Contains(env.Message, want) {
			t.Errorf("message = %q, want it to name %q (the remedy)", env.Message, want)
		}
	}
}

// TestServerCheckClearSession_AdmitsSession: a non-agent subject on a
// `session` credential (forge sign-in cookie + CSRF pair) clears a
// server_check concern by each of the four verbs: 200, the row reads back
// cleared and exactly one clear entry is appended.
func TestServerCheckClearSession_AdmitsSession(t *testing.T) {
	for _, vc := range clearVerbCases {
		t.Run(vc.verb, func(t *testing.T) {
			fx := newSessionOnlyFixture(t)
			row := vc.seed(t, fx, uuid.New())
			path, body := vc.request(row, false)

			w := fx.do(t, path, body, "", true, nil)

			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 (session admitted):\n%s", w.Code, w.Body.String())
			}
			fx.assertCommitted(t, row, vc.cleared, vc.category, 1)
		})
	}
}

// TestServerCheckClearSession_RefusesBearerKinds: the SAME non-agent subject
// github:alice on every non-session bearer kind — static, oauth (device
// flow), oauth_as (fho_, audience-validated), unrecorded (auth_method ”) and
// unknown (an unrecognised auth_method) — is refused by each of the four
// verbs with 403 concern_requires_session naming the kind; the row stays in
// its start state and no clear entry is appended. See COUNTERFACTUALS 1–3.
func TestServerCheckClearSession_RefusesBearerKinds(t *testing.T) {
	kinds := []string{
		identity.CredentialStatic,
		identity.CredentialDeviceFlow,
		identity.CredentialOAuthAS,
		identity.CredentialUnrecorded,
		identity.CredentialUnknown,
	}
	for _, vc := range clearVerbCases {
		for _, kind := range kinds {
			t.Run(vc.verb+"/"+kind, func(t *testing.T) {
				fx := newSessionOnlyFixture(t)
				runID := uuid.New()
				row := vc.seed(t, fx, runID)
				path, body := vc.request(row, false)

				w := fx.do(t, path, body, fx.bearerFor(t, kind, runID), false, nil)

				assertRequiresSession(t, w, row, vc.verb, kind)
				fx.assertCommitted(t, row, vc.before, vc.category, 0)
				if vc.verb == clearVerbDefer && fx.fp.called {
					t.Errorf("provider.File was called; a refused defer must file no work item")
				}
			})
		}
	}
}

// TestServerCheckClearSession_PreservesAgentRefusal: an agent subject keeps
// the pre-ADR-143 refusal, not concern_requires_session (ADR-143 C2). An
// operator-agent static bearer and a run-bound mcp_run bearer on its OWN run
// get concern_requires_human (refused_actor agent_token) on waive, bulk waive
// and defer, and resolve_requires_human from resolve's pre-body guard; state
// is unchanged both ways. See COUNTERFACTUAL 5.
func TestServerCheckClearSession_PreservesAgentRefusal(t *testing.T) {
	for _, vc := range clearVerbCases {
		for _, agent := range []string{"operator_agent", identity.CredentialMCPRun} {
			t.Run(vc.verb+"/"+agent, func(t *testing.T) {
				fx := newSessionOnlyFixture(t)
				runID := uuid.New()
				row := vc.seed(t, fx, runID)
				path, body := vc.request(row, false)
				bearer := ""
				if agent == "operator_agent" {
					bearer = fx.bearerWithAuthMethod(t, operatorAgentSubject, "static")
				} else {
					bearer = fx.bearerFor(t, identity.CredentialMCPRun, runID)
				}

				w := fx.do(t, path, body, bearer, false, nil)

				fx.assertCommitted(t, row, vc.before, vc.category, 0)
				if vc.verb == clearVerbResolve {
					if w.Code != http.StatusForbidden || decodeErrorEnvelope(t, w).Code != errCodeResolveRequiresHuman {
						t.Errorf("resolve by %s = %d %s, want 403 %s", agent, w.Code, w.Body.String(), errCodeResolveRequiresHuman)
					}
					return
				}
				assertRequiresHuman(t, w, row, vc.verb, refusedActorAgentToken)
			})
		}
	}
}

// TestServerCheckClearSession_PreservesDelegatedRefusal: a SESSION request
// marked delegated:true on waive and bulk waive (defer and resolve carry no
// delegated flag) keeps concern_requires_human (refused_actor delegated): D1
// never admits what the present guard refuses. See COUNTERFACTUAL 6.
//
// The fixture ISOLATES the delegated arm: the server_check row is LOW and the
// run's ONLY open concern under an operator_agent block delegating may_waive
// under solo_low, so with the arm deleted checkDelegation PASSES and the row
// reads back waived. A high row or no spec would make checkDelegation refuse
// on its own and mask the deletion.
func TestServerCheckClearSession_PreservesDelegatedRefusal(t *testing.T) {
	for _, vc := range clearVerbCases[:2] {
		t.Run(vc.verb, func(t *testing.T) {
			fx := newSessionOnlyFixture(t)
			runID := uuid.New()
			fx.runs.seedRun(&run.Run{
				ID:           runID,
				State:        run.StateRunning,
				WorkflowID:   "feature_change",
				WorkflowSpec: []byte(delegatedActionSpecYAML),
			})
			row := seedServerCheckRow(t, fx.cr, runID, uuid.New(), "low")
			path, body := vc.request(row, true)

			w := fx.do(t, path, body, "", true, nil)

			assertRequiresHuman(t, w, row, vc.verb, refusedActorDelegated)
			fx.assertCommitted(t, row, vc.before, vc.category, 0)
		})
	}
}

// TestServerCheckClearSession_ForgedChannelClaimsGrantNothing: nothing
// caller-set is read. A static human bearer sending every header a browser or
// an MCP client might carry is still refused, and a session sending none of
// them is still admitted. This is a property check, not a deletable control:
// there is no header-reading code to delete.
func TestServerCheckClearSession_ForgedChannelClaimsGrantNothing(t *testing.T) {
	forged := map[string]string{
		"X-Fishhawk-Channel": "web_ui",
		"User-Agent":         "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 Safari/605.1.15",
		"Origin":             "https://fishhawk.example",
		"Sec-Fetch-Site":     "same-origin",
		"Mcp-Session-Id":     "",
	}
	fx := newSessionOnlyFixture(t)
	row := seedRaisedServerCheck(t, fx, uuid.New())
	path, body := clearVerbCases[0].request(row, false)

	w := fx.do(t, path, body, fx.bearerFor(t, identity.CredentialStatic, row.RunID), false, forged)
	assertRequiresSession(t, w, row, clearVerbWaive, identity.CredentialStatic)
	fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 0)

	// The same request with an Mcp-Session-Id value (an MCP marker) and with
	// no channel header at all: both still refused.
	forged["Mcp-Session-Id"] = "mcp-session-1"
	w = fx.do(t, path, body, fx.bearerFor(t, identity.CredentialStatic, row.RunID), false, forged)
	assertRequiresSession(t, w, row, clearVerbWaive, identity.CredentialStatic)
	w = fx.do(t, path, body, fx.bearerFor(t, identity.CredentialStatic, row.RunID), false, nil)
	assertRequiresSession(t, w, row, clearVerbWaive, identity.CredentialStatic)
	fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 0)

	// A session with no channel claim is admitted.
	w = fx.do(t, path, body, "", true, nil)
	if w.Code != http.StatusOK {
		t.Errorf("session with no channel header: status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	fx.assertCommitted(t, row, concern.StateWaived, CategoryConcernWaived, 1)
}

// TestServerCheckClearSession_BulkRefusedWhole: a bulk batch of [reviewer
// concern, server_check concern] on a static human bearer is refused WHOLE:
// both rows stay raised and no concern_waived row is appended. See
// COUNTERFACTUAL 4.
func TestServerCheckClearSession_BulkRefusedWhole(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	runID := uuid.New()
	seedDeferRun(fx.runs, runID)
	reviewer := seedConcernRow(t, fx.cr, runID, uuid.New(), concern.StageKindImplement, 1, "naming nit")
	check := seedServerCheckRow(t, fx.cr, runID, uuid.New(), "high")
	body := jsonBody(map[string]any{
		"concern_ids": []string{reviewer.ID.String(), check.ID.String()},
		"reason":      "batch clear", "judgement": "no_defect",
	})

	w := fx.do(t, "/v0/runs/"+runID.String()+"/concerns/waive", body, fx.bearerFor(t, identity.CredentialStatic, runID), false, nil)

	assertRequiresSession(t, w, check, clearVerbBulkWaive, identity.CredentialStatic)
	assertConcernStates(t, fx.cr, map[uuid.UUID]concern.State{
		reviewer.ID: concern.StateRaised,
		check.ID:    concern.StateRaised,
	})
	assertNoWaivedEntries(t, fx.au)
}

// TestServerCheckClearSession_ReviewerConcernUnaffected: D1 is
// provenance-scoped. A reviewer-raised concern on the same static human
// bearer is still waivable (and resolvable, ADR-143 exclusion (vi)).
func TestServerCheckClearSession_ReviewerConcernUnaffected(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	runID := uuid.New()
	seedDeferRun(fx.runs, runID)
	row := seedConcernRow(t, fx.cr, runID, uuid.New(), concern.StageKindImplement, 1, "naming nit")
	routed := seedResolvableConcern(t, fx.cr, runID, "routed nit")
	bearer := fx.bearerFor(t, identity.CredentialStatic, runID)

	w := fx.do(t, "/v0/concerns/"+row.ID.String()+"/waive",
		jsonBody(map[string]any{"reason": "accepted trade-off", "judgement": "no_defect"}), bearer, false, nil)
	if w.Code != http.StatusOK {
		t.Errorf("reviewer waive on static bearer: status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	assertState(t, fx.cr, row.ID, concern.StateWaived)

	w = fx.do(t, "/v0/runs/"+runID.String()+"/concerns/resolve",
		jsonBody(map[string]any{"concern_ids": []string{routed.ID.String()}, "evidence": "re-ran", "judgement": "defect"}), bearer, false, nil)
	if w.Code != http.StatusOK {
		t.Errorf("reviewer resolve on static bearer: status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	assertState(t, fx.cr, routed.ID, concern.StateAddressed)
}
