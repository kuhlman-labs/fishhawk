package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// hotfixSpecYAML declares hotfix_change plus two other workflows.
const hotfixSpecYAML = `version: "0.3"
workflows:
  hotfix_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
  docs_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

type hotfixHarness struct {
	*appendHarness
	gh   *fakeGitHubForRuns
	runs *fakeRepo
}

// newHotfixHarness wires the append harness with a GitHub stub serving specYAML
// and a run repo. A cfg.AuditRepo override is honoured.
func newHotfixHarness(t *testing.T, specYAML string, cfg Config) *hotfixHarness {
	t.Helper()
	gh := newFakeGitHubForRuns(specYAML)
	cfg.GitHub = screenGitHub(t, gh)
	runs := newFakeRepo()
	cfg.RunRepo = runs
	return &hotfixHarness{appendHarness: newAppendHarness(t, cfg), gh: gh, runs: runs}
}

func postHotfix(t *testing.T, s *Server, campaignID, body string, ident Identity) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/campaigns/"+campaignID+"/hotfix", strings.NewReader(body))
	req.SetPathValue("campaign_id", campaignID)
	w := httptest.NewRecorder()
	s.handleCampaignHotfix(w, withIdentity(req, ident))
	return w
}

type hotfixBody struct {
	Campaign     campaignResponse     `json:"campaign"`
	Item         campaignItemResponse `json:"item"`
	DispatchHold *dispatchHoldPayload `json:"dispatch_hold"`
}

func decodeHotfix(t *testing.T, w *httptest.ResponseRecorder) hotfixBody {
	t.Helper()
	var b hotfixBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode hotfix: %v (%s)", err, w.Body.String())
	}
	return b
}

func (h *hotfixHarness) holdOf(id uuid.UUID) *campaign.DispatchHold {
	h.repo.mu.Lock()
	defer h.repo.mu.Unlock()
	return h.repo.holds[id]
}

// assertHotfixNothingWritten pins a refusal's committed effect: no new item,
// no hold change, no campaign_item_added row.
func (h *hotfixHarness) assertHotfixNothingWritten(t *testing.T, id uuid.UUID, wantItems int, wantHold *campaign.DispatchHold) {
	t.Helper()
	if n := h.itemCount(id); n != wantItems {
		t.Errorf("item count = %d, want %d (nothing written)", n, wantItems)
	}
	if got := h.holdOf(id); !reflect.DeepEqual(got, wantHold) {
		t.Errorf("hold = %+v, want %+v", got, wantHold)
	}
	if n := h.aud.count(categoryCampaignItemAdded); n != 0 {
		t.Errorf("campaign_item_added rows = %d, want 0", n)
	}
}

func hotfixAuditPayload(t *testing.T, aud *campaignAuditRecorder, category string) (map[string]any, audit.GlobalChainAppendParams) {
	t.Helper()
	aud.mu.Lock()
	defer aud.mu.Unlock()
	for _, e := range aud.entries {
		if e.Category == category {
			var p map[string]any
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatalf("payload: %v", err)
			}
			return p, e
		}
	}
	t.Fatalf("no %s row", category)
	return nil, audit.GlobalChainAppendParams{}
}

func TestCampaignHotfix_Created201HeadsQueueWithHold(t *testing.T) {
	h := newHotfixHarness(t, hotfixSpecYAML, Config{})
	fixed := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	h.s.nowFunc = func() time.Time { return fixed }
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending), cItem("issue:11", nil, campaign.ItemStatePending))
	w := postHotfix(t, h.s, c.ID.String(), `{"issue":"#42","hold_dispatch":true}`, testOperatorIdentity())
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	b := decodeHotfix(t, w)
	if b.Item.IssueRef != "issue:42" || b.Item.Position != -1 || !b.Item.Expedited ||
		b.Item.ExpediteKind != campaign.ExpediteKindHotfix || b.Item.Workflow != defaultHotfixWorkflow {
		t.Fatalf("item = %+v", b.Item)
	}
	if b.DispatchHold == nil || b.DispatchHold.HotfixItemID != b.Item.ID || b.DispatchHold.HotfixIssueRef != "issue:42" ||
		!b.DispatchHold.HeldAt.Equal(fixed) || !b.DispatchHold.ExpiresAt.Equal(fixed.Add(campaign.MaxDispatchHold)) {
		t.Fatalf("dispatch_hold = %+v", b.DispatchHold)
	}
	if hold := h.holdOf(c.ID); hold == nil || hold.HotfixItemID != b.Item.ID {
		t.Fatalf("committed hold = %+v", hold)
	}
	if n := h.aud.count(categoryCampaignItemAdded); n != 1 {
		t.Fatalf("campaign_item_added rows = %d, want exactly 1", n)
	}
	if n := h.aud.count(categoryCampaignItemsAppended); n != 0 {
		t.Errorf("campaign_items_appended rows = %d, want 0 (the verb binds its own recorder)", n)
	}
	p, e := hotfixAuditPayload(t, h.aud, categoryCampaignItemAdded)
	for _, k := range []string{"campaign_id", "repo", "item_id", "issue_ref", "expedited", "kind", "workflow", "reason", "position", "depends_on", "hold_dispatch", "hold_held_at", "hold_expires_at", "actor"} {
		if _, ok := p[k]; !ok {
			t.Errorf("campaign_item_added payload missing %q: %v", k, p)
		}
	}
	if p["expedited"] != true || p["workflow"] != defaultHotfixWorkflow || p["hold_dispatch"] != true {
		t.Errorf("payload = %v", p)
	}
	if e.ActorKind == nil || *e.ActorKind != audit.ActorUser {
		t.Errorf("actor kind = %v, want user", e.ActorKind)
	}
	// The expedited item heads the engine's eligible queue and the status read
	// reports the hold.
	st := getCampaignStatusBody(t, h.s, c.ID)
	if len(st.Rollup.Eligible) == 0 || st.Rollup.Eligible[0] != "issue:42" {
		t.Errorf("eligible = %v, want issue:42 first", st.Rollup.Eligible)
	}
}

func TestCampaignHotfix_ValidationRefusals400(t *testing.T) {
	h := newHotfixHarness(t, hotfixSpecYAML, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	long := strings.Repeat("x", maxHotfixReasonBytes+1)
	// The workflow cases carry a reason, so without the workflow check they
	// pass the reason rule and reach the 422 workflow_not_registered echo; the
	// refusal must not echo the raw value either.
	longWorkflow := strings.Repeat("w", maxHotfixWorkflowBytes+1)
	cases := map[string]struct {
		id, body, field, noEcho string
	}{
		"unknown field":      {c.ID.String(), `{"issue":"42","bogus":1}`, "", ""},
		"missing issue":      {c.ID.String(), `{"workflow":"hotfix_change"}`, "issue", ""},
		"blank issue":        {c.ID.String(), `{"issue":"  "}`, "issue", ""},
		"reason too big":     {c.ID.String(), `{"issue":"42","reason":"` + long + `"}`, "reason", ""},
		"workflow too big":   {c.ID.String(), `{"issue":"42","workflow":"` + longWorkflow + `","reason":"r"}`, "workflow", longWorkflow},
		"workflow bad shape": {c.ID.String(), `{"issue":"42","workflow":"Feature-Change<x>","reason":"r"}`, "workflow", "Feature-Change"},
		"bad uuid":           {"not-a-uuid", `{"issue":"42"}`, "campaign_id", ""},
	}
	for name, tc := range cases {
		w := postHotfix(t, h.s, tc.id, tc.body, testOperatorIdentity())
		if w.Code != http.StatusBadRequest || decodeCampaignError(t, w) != "validation_failed" {
			t.Errorf("%s = %d %s, want 400 validation_failed", name, w.Code, w.Body.String())
			continue
		}
		if tc.field != "" && !strings.Contains(w.Body.String(), `"field":"`+tc.field+`"`) {
			t.Errorf("%s: body %s does not name field %s", name, w.Body.String(), tc.field)
		}
		if tc.noEcho != "" && strings.Contains(w.Body.String(), tc.noEcho) {
			t.Errorf("%s: body %s echoes the requested workflow", name, w.Body.String())
		}
	}
	h.assertHotfixNothingWritten(t, c.ID, 1, nil)
	if h.gh.specCalls != 0 {
		t.Errorf("spec fetches = %d, want 0", h.gh.specCalls)
	}
}

// The reason rule (#4234 / FO ruling, generalized by C10): any workflow other
// than hotfix_change needs a non-empty reason.
func TestCampaignHotfix_FeatureChangeRequiresReason(t *testing.T) {
	h := newHotfixHarness(t, hotfixSpecYAML, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	for _, body := range []string{
		`{"issue":"42","workflow":"feature_change"}`,
		`{"issue":"42","workflow":"feature_change","reason":"   "}`,
		`{"issue":"42","workflow":"docs_change"}`,
	} {
		w := postHotfix(t, h.s, c.ID.String(), body, testOperatorIdentity())
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"field":"reason"`) {
			t.Fatalf("%s = %d %s, want 400 field=reason", body, w.Code, w.Body.String())
		}
	}
	h.assertHotfixNothingWritten(t, c.ID, 1, nil)
	w := postHotfix(t, h.s, c.ID.String(), `{"issue":"42","workflow":"feature_change","reason":"prod outage"}`, testOperatorIdentity())
	if w.Code != http.StatusCreated {
		t.Fatalf("with reason = %d %s, want 201", w.Code, w.Body.String())
	}
	b := decodeHotfix(t, w)
	if b.Item.Workflow != "feature_change" || b.Item.ExpediteReason != "prod outage" || b.DispatchHold != nil {
		t.Errorf("item = %+v hold = %+v", b.Item, b.DispatchHold)
	}
	if h.holdOf(c.ID) != nil {
		t.Errorf("hold placed without hold_dispatch")
	}
}

// C3: the WHOLE verb refuses run-bound and delegated operator-agent tokens,
// with or without hold_dispatch. Each fixture carries write:campaigns, so the
// token-class refusal is the only thing between it and a 201.
func TestCampaignHotfix_RunTokenForbidden(t *testing.T) {
	h := newHotfixHarness(t, hotfixSpecYAML, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	tok := Identity{Subject: "mcp:run:" + uuid.NewString(), TokenID: "tok", Scopes: []string{"write:campaigns"}, AccountID: testOperatorAccountID}
	for _, body := range []string{`{"issue":"42"}`, `{"issue":"42","hold_dispatch":true}`} {
		w := postHotfix(t, h.s, c.ID.String(), body, tok)
		if w.Code != http.StatusForbidden || decodeCampaignError(t, w) != "run_token_forbidden" {
			t.Fatalf("%s = %d %s, want 403 run_token_forbidden", body, w.Code, w.Body.String())
		}
	}
	h.assertHotfixNothingWritten(t, c.ID, 1, nil)
}

func TestCampaignHotfix_OperatorAgentForbidden(t *testing.T) {
	h := newHotfixHarness(t, hotfixSpecYAML, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	tok := Identity{Subject: "operator-agent/v1:campaign", TokenID: "tok", Scopes: []string{"write:campaigns"}, AccountID: testOperatorAccountID}
	for _, body := range []string{`{"issue":"42"}`, `{"issue":"42","hold_dispatch":true}`} {
		w := postHotfix(t, h.s, c.ID.String(), body, tok)
		if w.Code != http.StatusForbidden || decodeCampaignError(t, w) != "operator_agent_forbidden" {
			t.Fatalf("%s = %d %s, want 403 operator_agent_forbidden", body, w.Code, w.Body.String())
		}
	}
	h.assertHotfixNothingWritten(t, c.ID, 1, nil)
	// The same scope on an ordinary operator token is admitted.
	op := Identity{Subject: "github:op", TokenID: "tok", Scopes: []string{"write:campaigns"}, AccountID: testOperatorAccountID}
	if w := postHotfix(t, h.s, c.ID.String(), `{"issue":"42"}`, op); w.Code != http.StatusCreated {
		t.Fatalf("operator token = %d %s, want 201", w.Code, w.Body.String())
	}
}

func TestCampaignHotfix_WriteScopeAndAccess(t *testing.T) {
	h := newHotfixHarness(t, hotfixSpecYAML, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	if w := postHotfix(t, h.s, c.ID.String(), `{"issue":"42"}`, Identity{}); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", w.Code)
	}
	noScope := Identity{Subject: "github:op", TokenID: "tok", Scopes: []string{"read:runs"}, AccountID: testOperatorAccountID}
	if w := postHotfix(t, h.s, c.ID.String(), `{"issue":"42"}`, noScope); w.Code != http.StatusForbidden || decodeCampaignError(t, w) != "insufficient_scope" {
		t.Errorf("no scope = %d %s, want 403 insufficient_scope", w.Code, w.Body.String())
	}
	h.repo.accounts[c.ID] = uuid.NewString()
	if w := postHotfix(t, h.s, c.ID.String(), `{"issue":"42"}`, testOperatorIdentity()); w.Code != http.StatusForbidden || decodeCampaignError(t, w) != "account_forbidden" {
		t.Errorf("cross-account = %d %s, want 403 account_forbidden", w.Code, w.Body.String())
	}
	h.assertHotfixNothingWritten(t, c.ID, 1, nil)

	inv := newHotfixHarness(t, hotfixSpecYAML, Config{
		AccountRoles:   fakeAccountRoles{role: account.RoleMember},
		RepoVisibility: newFakeRepoVisibility(map[string]bool{}),
	})
	ic := inv.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	if w := postHotfix(t, inv.s, ic.ID.String(), `{"issue":"42"}`, memberIdentity()); w.Code != http.StatusForbidden || decodeCampaignError(t, w) != "repo_forbidden" {
		t.Errorf("repo-invisible = %d %s, want 403 repo_forbidden", w.Code, w.Body.String())
	}
	inv.assertHotfixNothingWritten(t, ic.ID, 1, nil)
	if h.gh.specCalls+inv.gh.specCalls != 0 {
		t.Errorf("spec fetches = %d, want 0 before access passes", h.gh.specCalls+inv.gh.specCalls)
	}
}

func TestCampaignHotfix_NotFoundUnconfiguredAndReadErrors(t *testing.T) {
	h := newHotfixHarness(t, hotfixSpecYAML, Config{})
	if w := postHotfix(t, h.s, uuid.NewString(), `{"issue":"42"}`, testOperatorIdentity()); w.Code != http.StatusNotFound || decodeCampaignError(t, w) != "campaign_not_found" {
		t.Errorf("missing campaign = %d %s, want 404", w.Code, w.Body.String())
	}
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	h.repo.itemsErr = errInjected
	if w := postHotfix(t, h.s, c.ID.String(), `{"issue":"42"}`, testOperatorIdentity()); w.Code != http.StatusInternalServerError {
		t.Errorf("list error = %d, want 500", w.Code)
	}
	h.repo.itemsErr = nil
	h.repo.getErr = errInjected
	if w := postHotfix(t, h.s, uuid.NewString(), `{"issue":"42"}`, testOperatorIdentity()); w.Code != http.StatusInternalServerError {
		t.Errorf("get error = %d, want 500", w.Code)
	}
	if w := postHotfix(t, New(Config{}), uuid.NewString(), `{"issue":"42"}`, testOperatorIdentity()); w.Code != http.StatusServiceUnavailable {
		t.Errorf("nil repo = %d, want 503", w.Code)
	}
}

func TestCampaignHotfix_TerminalDuplicateAndInvalidRef(t *testing.T) {
	h := newHotfixHarness(t, hotfixSpecYAML, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	if w := postHotfix(t, h.s, c.ID.String(), `{"issue":"#10"}`, testOperatorIdentity()); w.Code != http.StatusConflict || decodeCampaignError(t, w) != "campaign_item_duplicate" {
		t.Errorf("duplicate = %d %s, want 409 campaign_item_duplicate", w.Code, w.Body.String())
	}
	if w := postHotfix(t, h.s, c.ID.String(), `{"issue":"abc"}`, testOperatorIdentity()); w.Code != http.StatusUnprocessableEntity || decodeCampaignError(t, w) != "campaign_item_ref_invalid" {
		t.Errorf("invalid ref = %d %s, want 422", w.Code, w.Body.String())
	}
	if h.gh.specCalls != 0 {
		t.Errorf("spec fetches = %d, want 0 (decided before any forge read)", h.gh.specCalls)
	}
	c.State = campaign.StateSucceeded
	if w := postHotfix(t, h.s, c.ID.String(), `{"issue":"42"}`, testOperatorIdentity()); w.Code != http.StatusConflict || decodeCampaignError(t, w) != "campaign_not_appendable" {
		t.Errorf("terminal = %d %s, want 409 campaign_not_appendable", w.Code, w.Body.String())
	}
	h.assertHotfixNothingWritten(t, c.ID, 1, nil)
}

// C10 workflow gate: the stub spec declares only feature_change and
// docs_change, and the request takes the hotfix_change default.
func TestCampaignHotfix_WorkflowNotRegistered(t *testing.T) {
	h := newHotfixHarness(t, screenSpecYAML, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	w := postHotfix(t, h.s, c.ID.String(), `{"issue":"42"}`, testOperatorIdentity())
	if w.Code != http.StatusUnprocessableEntity || decodeCampaignError(t, w) != "workflow_not_registered" {
		t.Fatalf("status = %d %s, want 422 workflow_not_registered", w.Code, w.Body.String())
	}
	var env struct {
		Error struct {
			Details struct {
				Requested  string   `json:"requested"`
				Registered []string `json:"registered"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error.Details.Requested != defaultHotfixWorkflow || !reflect.DeepEqual(env.Error.Details.Registered, []string{"docs_change", "feature_change"}) {
		t.Errorf("details = %+v (%s)", env.Error.Details, w.Body.String())
	}
	// A well-shaped id exactly at maxHotfixWorkflowBytes passes the cap and
	// reaches this gate.
	atCap := strings.Repeat("w", maxHotfixWorkflowBytes)
	w = postHotfix(t, h.s, c.ID.String(), `{"issue":"42","workflow":"`+atCap+`","reason":"r"}`, testOperatorIdentity())
	if w.Code != http.StatusUnprocessableEntity || decodeCampaignError(t, w) != "workflow_not_registered" {
		t.Errorf("at-cap workflow = %d %s, want 422 workflow_not_registered", w.Code, w.Body.String())
	}
	h.assertHotfixNothingWritten(t, c.ID, 1, nil)
}

func TestCampaignHotfix_WorkflowSpecFailuresFailClosed(t *testing.T) {
	cases := map[string]struct {
		mutate func(*fakeGitHubForRuns)
		status int
		code   string
	}{
		"spec fetch error": {func(f *fakeGitHubForRuns) {
			f.specStatus, f.specBody = http.StatusInternalServerError, `{"message":"boom"}`
		}, http.StatusBadGateway, "workflow_spec_unavailable"},
		"installation error": {func(f *fakeGitHubForRuns) {
			f.installationStatus, f.installationBody = http.StatusInternalServerError, `{"message":"boom"}`
		}, http.StatusBadGateway, "workflow_spec_unavailable"},
		"no spec file": {func(f *fakeGitHubForRuns) {
			f.specStatus, f.specBody = http.StatusNotFound, `{"message":"Not Found"}`
		}, http.StatusUnprocessableEntity, "workflow_not_registered"},
		"unparseable spec": {func(f *fakeGitHubForRuns) { *f = *newFakeGitHubForRuns("version: [not: a spec") },
			http.StatusUnprocessableEntity, "workflow_spec_invalid"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHotfixHarness(t, hotfixSpecYAML, Config{})
			tc.mutate(h.gh)
			c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
			w := postHotfix(t, h.s, c.ID.String(), `{"issue":"42"}`, testOperatorIdentity())
			if w.Code != tc.status || decodeCampaignError(t, w) != tc.code {
				t.Fatalf("%d %s, want %d %s", w.Code, w.Body.String(), tc.status, tc.code)
			}
			h.assertHotfixNothingWritten(t, c.ID, 1, nil)
		})
	}
	t.Run("nil github client", func(t *testing.T) {
		h := newHotfixHarness(t, hotfixSpecYAML, Config{})
		h.s.cfg.GitHub = nil
		c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
		w := postHotfix(t, h.s, c.ID.String(), `{"issue":"42"}`, testOperatorIdentity())
		if w.Code != http.StatusServiceUnavailable || decodeCampaignError(t, w) != "workflow_spec_unavailable" {
			t.Fatalf("%d %s, want 503 workflow_spec_unavailable", w.Code, w.Body.String())
		}
		h.assertHotfixNothingWritten(t, c.ID, 1, nil)
	})
}

// seedHold places an active hold naming item directly in the fake store.
func (h *hotfixHarness) seedHold(c *campaign.Campaign, item *campaign.Item, heldAt time.Time) *campaign.DispatchHold {
	hold := &campaign.DispatchHold{CampaignID: c.ID, HotfixItemID: item.ID, HeldAt: heldAt, ExpiresAt: heldAt.Add(campaign.MaxDispatchHold)}
	h.repo.mu.Lock()
	h.repo.holds[c.ID] = hold
	h.repo.mu.Unlock()
	cp := *hold
	return &cp
}

// C2: a second hotfix WITH hold_dispatch during an active (non-due) hold is
// refused campaign_hold_active and writes nothing; WITHOUT hold_dispatch it
// is added as a second expedited item and the first hold stands.
func TestCampaignHotfix_HoldActive409(t *testing.T) {
	h := newHotfixHarness(t, hotfixSpecYAML, Config{})
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	h.s.nowFunc = func() time.Time { return now }
	first := cItem("issue:10", nil, campaign.ItemStateRunning)
	c := h.seed(first)
	hold := h.seedHold(c, first, now.Add(-time.Minute))
	w := postHotfix(t, h.s, c.ID.String(), `{"issue":"42","hold_dispatch":true}`, testOperatorIdentity())
	if w.Code != http.StatusConflict || decodeCampaignError(t, w) != "campaign_hold_active" {
		t.Fatalf("status = %d %s, want 409 campaign_hold_active", w.Code, w.Body.String())
	}
	h.assertHotfixNothingWritten(t, c.ID, 1, hold)
	w = postHotfix(t, h.s, c.ID.String(), `{"issue":"43"}`, testOperatorIdentity())
	if w.Code != http.StatusCreated {
		t.Fatalf("second expedited without hold = %d %s, want 201", w.Code, w.Body.String())
	}
	if got := h.holdOf(c.ID); !reflect.DeepEqual(got, hold) {
		t.Errorf("hold = %+v, want the first hold untouched", got)
	}
	if n := h.aud.count(categoryCampaignHoldReleased); n != 0 {
		t.Errorf("campaign_hold_released rows = %d, want 0 (hold not due)", n)
	}
}

// A due hold (its hotfix item terminal) is released by the verb before the
// new hold is placed, recorded under the system actor with trigger
// hotfix_verb.
func TestCampaignHotfix_ReleasesDueHoldFirst(t *testing.T) {
	h := newHotfixHarness(t, hotfixSpecYAML, Config{})
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	h.s.nowFunc = func() time.Time { return now }
	first := cItem("issue:10", nil, campaign.ItemStateSucceeded)
	c := h.seed(first, cItem("issue:11", nil, campaign.ItemStatePending))
	h.seedHold(c, first, now.Add(-time.Minute))
	w := postHotfix(t, h.s, c.ID.String(), `{"issue":"42","hold_dispatch":true}`, testOperatorIdentity())
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d %s, want 201", w.Code, w.Body.String())
	}
	b := decodeHotfix(t, w)
	if hold := h.holdOf(c.ID); hold == nil || hold.HotfixItemID != b.Item.ID {
		t.Fatalf("hold = %+v, want the new hold", hold)
	}
	p, e := hotfixAuditPayload(t, h.aud, categoryCampaignHoldReleased)
	if p["reason"] != campaign.HoldReleasedHotfixTerminal || p["trigger"] != holdReleaseTriggerHotfixVerb || p["hotfix_issue_ref"] != "issue:10" {
		t.Errorf("release payload = %v", p)
	}
	if e.ActorKind == nil || *e.ActorKind != audit.ActorSystem || e.ActorSubject == nil || *e.ActorSubject != campaignHoldSystemSubject {
		t.Errorf("release actor = %v/%v, want system", e.ActorKind, e.ActorSubject)
	}
}

// hotfixFailCategoryAudit records every entry except one category, which fails.
type hotfixFailCategoryAudit struct {
	campaignAuditRecorder
	fail string
}

func (a *hotfixFailCategoryAudit) AppendGlobalChained(ctx context.Context, p audit.GlobalChainAppendParams) (*audit.Entry, error) {
	if p.Category == a.fail {
		return nil, errors.New("audit chain unavailable")
	}
	return a.campaignAuditRecorder.AppendGlobalChained(ctx, p)
}

// An unrecordable release refuses 503 with the still-active hold's expires_at
// and writes nothing: the hold stays, no item is added.
func TestCampaignHotfix_ReleaseUnrecorded503(t *testing.T) {
	aud := &hotfixFailCategoryAudit{fail: categoryCampaignHoldReleased}
	h := newHotfixHarness(t, hotfixSpecYAML, Config{AuditRepo: aud})
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	h.s.nowFunc = func() time.Time { return now }
	first := cItem("issue:10", nil, campaign.ItemStateSucceeded)
	c := h.seed(first)
	hold := h.seedHold(c, first, now.Add(-time.Minute))
	w := postHotfix(t, h.s, c.ID.String(), `{"issue":"42"}`, testOperatorIdentity())
	if w.Code != http.StatusServiceUnavailable || decodeCampaignError(t, w) != "campaign_audit_unavailable" {
		t.Fatalf("status = %d %s, want 503 campaign_audit_unavailable", w.Code, w.Body.String())
	}
	if want := "expires_at " + hold.ExpiresAt.UTC().Format(time.RFC3339); !strings.Contains(w.Body.String(), want) {
		t.Errorf("503 body does not carry %q: %s", want, w.Body.String())
	}
	if r := holdReleaseRefusal(c, hold, campaign.ErrHoldReleaseUnrecorded); r.Details["expires_at"] != hold.ExpiresAt {
		t.Errorf("refusal details expires_at = %v, want %v", r.Details["expires_at"], hold.ExpiresAt)
	}
	if n := h.itemCount(c.ID); n != 1 {
		t.Errorf("item count = %d, want 1", n)
	}
	if got := h.holdOf(c.ID); !reflect.DeepEqual(got, hold) {
		t.Errorf("hold = %+v, want it kept", got)
	}
	if n := aud.count(categoryCampaignItemAdded); n != 0 {
		t.Errorf("campaign_item_added rows = %d, want 0", n)
	}
}

// A hold read failure answers 500 dependency_check_failed, nothing written.
func TestCampaignHotfix_HoldReadError500(t *testing.T) {
	h := newHotfixHarness(t, hotfixSpecYAML, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	h.repo.holdErr = errInjected
	w := postHotfix(t, h.s, c.ID.String(), `{"issue":"42"}`, testOperatorIdentity())
	if w.Code != http.StatusInternalServerError || decodeCampaignError(t, w) != "dependency_check_failed" {
		t.Fatalf("status = %d %s, want 500 dependency_check_failed", w.Code, w.Body.String())
	}
	h.repo.holdErr = nil
	h.assertHotfixNothingWritten(t, c.ID, 1, nil)
}

// The verb's own record failure: 503 and no item, expedite or hold.
func TestCampaignHotfix_RecordFailure503(t *testing.T) {
	h := newHotfixHarness(t, hotfixSpecYAML, Config{AuditRepo: failingAuditRepo{}})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	w := postHotfix(t, h.s, c.ID.String(), `{"issue":"42","hold_dispatch":true}`, testOperatorIdentity())
	if w.Code != http.StatusServiceUnavailable || decodeCampaignError(t, w) != "campaign_audit_unavailable" {
		t.Fatalf("status = %d %s, want 503", w.Code, w.Body.String())
	}
	h.assertHotfixNothingWritten(t, c.ID, 1, nil)
}

// --- releaseCampaignHoldIfDue ---

// C1 vehicle: the hotfix ITEM is still running but its RUN succeeded, and the
// hold's expires_at is 2 h ahead of the fixed clock, so only the terminal arm
// can release it.
func TestReleaseCampaignHoldIfDue_ReleasesOnHotfixRunTerminal(t *testing.T) {
	h := newHotfixHarness(t, hotfixSpecYAML, Config{})
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	h.s.nowFunc = func() time.Time { return now }
	runID := uuid.New()
	h.runs.runs[runID] = &run.Run{ID: runID, State: run.StateSucceeded}
	hot := cItemWithRun("issue:10", nil, campaign.ItemStateRunning, runID)
	c := h.seed(hot)
	h.seedHold(c, hot, now)
	got, err := h.s.releaseCampaignHoldIfDue(context.Background(), c, holdReleaseTriggerHostDispatch)
	if err != nil || got != nil {
		t.Fatalf("got %+v err %v, want released (nil, nil)", got, err)
	}
	if h.holdOf(c.ID) != nil {
		t.Errorf("hold row survived")
	}
	p, _ := hotfixAuditPayload(t, h.aud, categoryCampaignHoldReleased)
	if p["reason"] != campaign.HoldReleasedHotfixTerminal || p["trigger"] != holdReleaseTriggerHostDispatch {
		t.Errorf("payload = %v", p)
	}
}

// C2 vehicle: the hotfix item and run are non-terminal and the clock is
// advanced 2h+1s, so only the timeout can release the hold.
func TestReleaseCampaignHoldIfDue_ReleasesOnTimeout(t *testing.T) {
	h := newHotfixHarness(t, hotfixSpecYAML, Config{})
	held := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	runID := uuid.New()
	h.runs.runs[runID] = &run.Run{ID: runID, State: run.StateRunning}
	hot := cItemWithRun("issue:10", nil, campaign.ItemStateRunning, runID)
	c := h.seed(hot)
	h.seedHold(c, hot, held)
	h.s.nowFunc = func() time.Time { return held.Add(campaign.MaxDispatchHold - time.Second) }
	if got, err := h.s.releaseCampaignHoldIfDue(context.Background(), c, holdReleaseTriggerStatusRead); err != nil || got == nil {
		t.Fatalf("before expiry: got %+v err %v, want the hold still active", got, err)
	}
	h.s.nowFunc = func() time.Time { return held.Add(campaign.MaxDispatchHold + time.Second) }
	if got, err := h.s.releaseCampaignHoldIfDue(context.Background(), c, holdReleaseTriggerStatusRead); err != nil || got != nil {
		t.Fatalf("after expiry: got %+v err %v, want released", got, err)
	}
	if h.holdOf(c.ID) != nil {
		t.Errorf("hold row survived the timeout")
	}
	p, _ := hotfixAuditPayload(t, h.aud, categoryCampaignHoldReleased)
	if p["reason"] != campaign.HoldReleasedTimeout {
		t.Errorf("reason = %v, want timeout", p["reason"])
	}
}

// The release row is stamped with the CAMPAIGN's account partition, not the
// caller's.
func TestReleaseCampaignHoldIfDue_StampsCampaignAccount(t *testing.T) {
	h := newHotfixHarness(t, hotfixSpecYAML, Config{})
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	h.s.nowFunc = func() time.Time { return now }
	hot := cItem("issue:10", nil, campaign.ItemStateFailed)
	c := h.seed(hot)
	acct := uuid.New()
	h.repo.accounts[c.ID] = acct.String()
	h.seedHold(c, hot, now)
	if _, err := h.s.releaseCampaignHoldIfDue(context.Background(), c, holdReleaseTriggerStatusRead); err != nil {
		t.Fatalf("release: %v", err)
	}
	_, e := hotfixAuditPayload(t, h.aud, categoryCampaignHoldReleased)
	if e.AccountID == nil || *e.AccountID != acct {
		t.Errorf("account = %v, want %s", e.AccountID, acct)
	}
	// An unparseable campaign account is an unrecordable release.
	h2 := newHotfixHarness(t, hotfixSpecYAML, Config{})
	h2.s.nowFunc = h.s.nowFunc
	hot2 := cItem("issue:10", nil, campaign.ItemStateFailed)
	c2 := h2.seed(hot2)
	h2.repo.accounts[c2.ID] = "not-a-uuid"
	h2.seedHold(c2, hot2, now)
	if _, err := h2.s.releaseCampaignHoldIfDue(context.Background(), c2, holdReleaseTriggerStatusRead); !errors.Is(err, campaign.ErrHoldReleaseUnrecorded) {
		t.Errorf("bad account err = %v, want ErrHoldReleaseUnrecorded", err)
	}
	if h2.holdOf(c2.ID) == nil {
		t.Errorf("hold released under an unstampable account")
	}
}

// Read failures are returned (callers fail closed), and the hold reported.
func TestReleaseCampaignHoldIfDue_ReadErrors(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	t.Run("hotfix item missing", func(t *testing.T) {
		h := newHotfixHarness(t, hotfixSpecYAML, Config{})
		c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
		ghost := &campaign.Item{ID: uuid.New()}
		h.seedHold(c, ghost, now)
		got, err := h.s.releaseCampaignHoldIfDue(context.Background(), c, holdReleaseTriggerStatusRead)
		if err == nil || got == nil {
			t.Fatalf("got %+v err %v, want the hold and an error", got, err)
		}
		if r := holdReleaseRefusal(c, got, err); r.Status != http.StatusInternalServerError || r.Code != "dependency_check_failed" {
			t.Errorf("refusal = %d %s", r.Status, r.Code)
		}
	})
	t.Run("hotfix run read fails", func(t *testing.T) {
		h := newHotfixHarness(t, hotfixSpecYAML, Config{})
		h.s.nowFunc = func() time.Time { return now }
		hot := cItemWithRun("issue:10", nil, campaign.ItemStateRunning, uuid.New()) // run not in the repo
		c := h.seed(hot)
		h.seedHold(c, hot, now)
		if got, err := h.s.releaseCampaignHoldIfDue(context.Background(), c, holdReleaseTriggerStatusRead); err == nil || got == nil {
			t.Fatalf("got %+v err %v, want the hold and an error", got, err)
		}
		if h.holdOf(c.ID) == nil {
			t.Errorf("hold released on an unreadable run")
		}
	})
	t.Run("release write fails", func(t *testing.T) {
		h := newHotfixHarness(t, hotfixSpecYAML, Config{})
		h.s.nowFunc = func() time.Time { return now }
		hot := cItem("issue:10", nil, campaign.ItemStateSucceeded)
		c := h.seed(hot)
		h.seedHold(c, hot, now)
		h.repo.releaseErr = errInjected
		got, err := h.s.releaseCampaignHoldIfDue(context.Background(), c, holdReleaseTriggerStatusRead)
		if err == nil || got == nil {
			t.Fatalf("got %+v err %v", got, err)
		}
		if r := holdReleaseRefusal(c, got, err); r.Status != http.StatusInternalServerError {
			t.Errorf("refusal = %d %s, want 500 (not an audit failure)", r.Status, r.Code)
		}
	})
}

// Concurrent evaluations of one due hold release it exactly once: the loser
// is the repository's idempotent no-op.
func TestReleaseCampaignHoldIfDue_ConcurrentReleaseOnce(t *testing.T) {
	h := newHotfixHarness(t, hotfixSpecYAML, Config{})
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	h.s.nowFunc = func() time.Time { return now }
	hot := cItem("issue:10", nil, campaign.ItemStateSucceeded)
	c := h.seed(hot)
	h.seedHold(c, hot, now)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.s.releaseCampaignHoldIfDue(context.Background(), c, holdReleaseTriggerHostDispatch); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent release err: %v", err)
	}
	if n := h.aud.count(categoryCampaignHoldReleased); n != 1 {
		t.Errorf("campaign_hold_released rows = %d, want exactly 1", n)
	}
}

// --- cross-layer (X1) ---

// TestCampaignHotfix_PGCrossLayer drives the REAL handler over the real
// campaign Postgres repository and reads the committed state back.
func TestCampaignHotfix_PGCrossLayer(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	repo := campaign.NewPostgresRepository(pool)
	c, err := repo.CreateCampaign(ctx, campaign.CreateCampaignParams{Repo: "kuhlman-labs/fishhawk"})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	for i, ref := range []string{"issue:10", "issue:11"} {
		if _, err := repo.CreateCampaignItem(ctx, campaign.CreateCampaignItemParams{CampaignID: c.ID, IssueRef: ref, Position: i}); err != nil {
			t.Fatalf("create item: %v", err)
		}
	}
	gh := newFakeGitHubForRuns(hotfixSpecYAML)
	aud := &campaignAuditRecorder{}
	h := newAppendHarness(t, Config{AuditRepo: aud, GitHub: screenGitHub(t, gh)})
	h.s.cfg.CampaignRepo = repo
	w := postHotfix(t, h.s, c.ID.String(), `{"issue":"42","workflow":"feature_change","reason":"sev1","hold_dispatch":true}`, testOperatorIdentity())
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d %s, want 201", w.Code, w.Body.String())
	}
	b := decodeHotfix(t, w)
	items, err := repo.ListCampaignItemsForCampaign(ctx, c.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 3 || items[0].ID != b.Item.ID || items[0].Position != -1 {
		t.Fatalf("committed items = %+v, want the hotfix first at -1", items)
	}
	if e := items[0].Expedite; e == nil || e.Kind != campaign.ExpediteKindHotfix || e.Workflow != "feature_change" || e.Reason != "sev1" {
		t.Errorf("committed expedite = %+v", items[0].Expedite)
	}
	hold, err := repo.GetDispatchHold(ctx, c.ID)
	if err != nil || hold == nil || hold.HotfixItemID != b.Item.ID {
		t.Fatalf("committed hold = %+v err %v", hold, err)
	}
	if n := aud.count(categoryCampaignItemAdded); n != 1 {
		t.Errorf("campaign_item_added rows = %d, want 1", n)
	}
}
