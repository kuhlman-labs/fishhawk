package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// AppendCampaignItems is the in-memory append for handler tests (E86.47 /
// #4358). It enforces the PRODUCTION rules — campaign.ValidateAppend (which
// runs ValidDependencyRef, CheckDuplicates, CheckExternal and
// ValidateAppendClosure) and campaign.MergeDependsOn — so a body mutation of
// any of them reaches these handler tests, and it honours record-before-commit:
// a Record error commits nothing.
func (f *fakeCampaignRepo) AppendCampaignItems(ctx context.Context, p campaign.AppendCampaignItemsParams) (*campaign.AppendOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.campaigns[p.CampaignID]
	if !ok {
		return nil, campaign.ErrCampaignNotFound
	}
	if c.State.IsTerminal() {
		return nil, campaign.InvalidTransitionError{Kind: "campaign", From: string(c.State), To: "append"}
	}
	existing := f.itemsByCmp[p.CampaignID]
	if err := campaign.ValidateAppend(existing, p); err != nil {
		return nil, err
	}
	outcome := campaign.AppendOutcome{Campaign: c}
	staged := map[*campaign.Item][]string{}
	maxPos := -1
	for _, it := range existing {
		if it.Position > maxPos {
			maxPos = it.Position
		}
		add, ok := p.AddDependencies[it.ID]
		if !ok {
			continue
		}
		if (it.State != campaign.ItemStatePending && it.State != campaign.ItemStateBlocked) || it.RunID != nil {
			outcome.SkippedStarted = append(outcome.SkippedStarted, it.IssueRef)
			continue
		}
		merged := campaign.MergeDependsOn(it.DependsOn, add)
		if len(merged) == len(it.DependsOn) {
			continue
		}
		staged[it] = merged
		outcome.Added = append(outcome.Added, campaign.AppliedAddition{ItemID: it.ID, IssueRef: it.IssueRef, Refs: merged[len(it.DependsOn):]})
	}
	now := time.Now().UTC()
	for i, ni := range p.NewItems {
		outcome.Created = append(outcome.Created, &campaign.Item{
			ID: uuid.New(), CampaignID: p.CampaignID, IssueRef: ni.IssueRef, DependsOn: ni.DependsOn,
			Autonomy: ni.Autonomy, State: campaign.ItemStatePending, Position: maxPos + 1 + i,
			CreatedAt: now, UpdatedAt: now,
		})
	}
	if p.Record != nil {
		if err := p.Record(ctx, outcome); err != nil {
			return nil, fmt.Errorf("%w: %w", campaign.ErrAppendUnrecorded, err)
		}
	}
	for it, deps := range staged {
		it.DependsOn = deps
	}
	f.itemsByCmp[p.CampaignID] = append(f.itemsByCmp[p.CampaignID], outcome.Created...)
	return &outcome, nil
}

// appendResolver is an IssueSetDependencyResolver whose tracker graph is
// configured per issue: deps[n] are n's blocked-by targets, satisfied[t]
// marks a target closed-as-completed. It counts calls and records the refs it
// was asked to resolve.
type appendResolver struct {
	name      string
	mu        sync.Mutex
	calls     int
	asked     [][]string
	deps      map[int][]int
	autonomy  map[int]string
	satisfied map[int]bool
	err       error
}

func (p *appendResolver) Name() string { return p.name }

func (p *appendResolver) File(context.Context, workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	return &workmgmt.CreatedItem{Provider: p.name}, nil
}

func (p *appendResolver) ResolveDependencies(_ context.Context, req workmgmt.IssueSetRequest) (*workmgmt.EpicChildrenResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.asked = append(p.asked, append([]string{}, req.Items...))
	if p.err != nil {
		return nil, p.err
	}
	res := &workmgmt.EpicChildrenResult{}
	set := map[int]bool{}
	for _, ref := range req.Items {
		n, err := workmgmt.ParseIssueRef(ref)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", workmgmt.ErrInvalidItemRef, ref)
		}
		set[n] = true
		res.Children = append(res.Children, workmgmt.EpicChild{Number: n, Autonomy: p.autonomy[n]})
	}
	for _, c := range res.Children {
		for _, to := range p.deps[c.Number] {
			switch {
			case set[to]:
				res.Edges = append(res.Edges, workmgmt.DependsEdge{From: c.Number, To: to})
			case p.satisfied[to]:
				res.SatisfiedEdges = append(res.SatisfiedEdges, workmgmt.SatisfiedEdge{From: c.Number, To: to, State: "closed", StateReason: "completed"})
			default:
				res.DroppedEdges = append(res.DroppedEdges, workmgmt.DependsEdge{From: c.Number, To: to, Reason: workmgmt.DropNotChild})
			}
		}
	}
	return res, nil
}

func (p *appendResolver) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// failingAuditRepo fails every global-chain append.
type failingAuditRepo struct{ audit.BaseFake }

func (failingAuditRepo) AppendGlobalChained(context.Context, audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return nil, errors.New("audit chain unavailable")
}

type appendHarness struct {
	s    *Server
	repo *fakeCampaignRepo
	res  *appendResolver
	aud  *campaignAuditRecorder
}

func newAppendHarness(t *testing.T, cfg Config) *appendHarness {
	t.Helper()
	h := &appendHarness{
		repo: newFakeCampaignRepo(),
		res:  &appendResolver{name: workmgmt.Default().Provider, deps: map[int][]int{}, autonomy: map[int]string{}, satisfied: map[int]bool{}},
		aud:  &campaignAuditRecorder{},
	}
	workmgmt.Register(h.res)
	cfg.Addr = "127.0.0.1:0"
	cfg.CampaignRepo = h.repo
	if cfg.AuditRepo == nil {
		cfg.AuditRepo = h.aud
	}
	h.s = New(cfg)
	return h
}

func (h *appendHarness) seed(items ...*campaign.Item) *campaign.Campaign {
	for i, it := range items {
		it.Position = i
	}
	return h.repo.seedCampaignWithItems("kuhlman-labs/fishhawk", "", items)
}

func (h *appendHarness) itemCount(id uuid.UUID) int {
	h.repo.mu.Lock()
	defer h.repo.mu.Unlock()
	return len(h.repo.itemsByCmp[id])
}

func postAppend(t *testing.T, s *Server, campaignID, body string, ident Identity) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/campaigns/"+campaignID+"/items", strings.NewReader(body))
	req.SetPathValue("campaign_id", campaignID)
	w := httptest.NewRecorder()
	s.handleAppendCampaignItems(w, withIdentity(req, ident))
	return w
}

type appendBody struct {
	Campaign campaignResponse               `json:"campaign"`
	Appended []appendedCampaignItemResponse `json:"appended"`
}

func decodeAppend(t *testing.T, w *httptest.ResponseRecorder) appendBody {
	t.Helper()
	var b appendBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode append: %v (%s)", err, w.Body.String())
	}
	return b
}

// assertNothingWritten pins a refusal's committed effect: no item rows, no
// forge read, no audit row.
func (h *appendHarness) assertNothingWritten(t *testing.T, id uuid.UUID, wantItems int) {
	t.Helper()
	if n := h.itemCount(id); n != wantItems {
		t.Errorf("item count = %d, want %d (nothing written)", n, wantItems)
	}
	if n := h.res.callCount(); n != 0 {
		t.Errorf("resolver calls = %d, want 0", n)
	}
	if n := h.aud.count(categoryCampaignItemsAppended); n != 0 {
		t.Errorf("campaign_items_appended rows = %d, want 0", n)
	}
}

func TestAppendCampaignItems_Created201VisibleInStatus(t *testing.T) {
	h := newAppendHarness(t, Config{})
	h.res.autonomy[21] = "low"
	h.res.satisfied[500] = true
	h.res.deps[20] = []int{500}
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	w := postAppend(t, h.s, c.ID.String(), `{"items":["20","#21"]}`, testOperatorIdentity())
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", w.Code, w.Body.String())
	}
	b := decodeAppend(t, w)
	if len(b.Appended) != 2 || b.Appended[0].IssueRef != "issue:20" || b.Appended[0].Position != 1 || b.Appended[1].Position != 2 {
		t.Fatalf("appended = %+v", b.Appended)
	}
	if !strings.Contains(w.Body.String(), `"satisfied_dependencies"`) {
		t.Errorf("satisfied_dependencies missing: %s", w.Body.String())
	}
	if got := h.res.asked; len(got) != 1 || !reflect.DeepEqual(got[0], []string{"issue:20", "issue:21"}) {
		t.Errorf("resolver asked %v, want one call over the NEW refs only", got)
	}
	// The record-before-commit audit row is written with its payload keys.
	if n := h.aud.count(categoryCampaignItemsAppended); n != 1 {
		t.Fatalf("campaign_items_appended rows = %d, want 1", n)
	}
	var payload map[string]any
	for _, e := range h.aud.entries {
		if e.Category == categoryCampaignItemsAppended {
			if err := json.Unmarshal(e.Payload, &payload); err != nil {
				t.Fatalf("payload: %v", err)
			}
			if e.ActorKind == nil || *e.ActorKind != audit.ActorUser || e.ActorSubject == nil || *e.ActorSubject != testOperatorIdentity().Subject {
				t.Errorf("actor = %v/%v", e.ActorKind, e.ActorSubject)
			}
		}
	}
	for _, k := range []string{"campaign_id", "repo", "appended", "allow_external_blockers", "satisfied_dependencies", "actor"} {
		if _, ok := payload[k]; !ok {
			t.Errorf("audit payload missing %q: %v", k, payload)
		}
	}
	rows, _ := payload["appended"].([]any)
	if len(rows) != 2 {
		t.Fatalf("payload appended = %v", payload["appended"])
	}
	first, _ := rows[0].(map[string]any)
	for _, k := range []string{"issue_ref", "depends_on", "external", "autonomy"} {
		if _, ok := first[k]; !ok {
			t.Errorf("appended row missing %q: %v", k, first)
		}
	}
	// The unchanged status handler shows both: issue:20 eligible, issue:21 human-led.
	st := getCampaignStatusBody(t, h.s, c.ID)
	if !containsRef(st.Rollup.Eligible, "issue:20") || !containsRef(st.Rollup.HumanLed, "issue:21") {
		t.Errorf("rollup = %+v", st.Rollup)
	}
}

func TestAppendCampaignItems_AutonomyLowRoutesHumanLed(t *testing.T) {
	h := newAppendHarness(t, Config{})
	h.res.autonomy[30] = "low"
	c := h.seed(cItem("issue:10", nil, campaign.ItemStateRunning))
	if w := postAppend(t, h.s, c.ID.String(), `{"items":["issue:30"]}`, testOperatorIdentity()); w.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	st := getCampaignStatusBody(t, h.s, c.ID)
	if !containsRef(st.Rollup.HumanLed, "issue:30") || containsRef(st.Rollup.Eligible, "issue:30") {
		t.Errorf("rollup = %+v, want issue:30 human_led", st.Rollup)
	}
}

func TestAppendCampaignItems_ValidationFailed400(t *testing.T) {
	h := newAppendHarness(t, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	fifty1 := make([]string, 51)
	for i := range fifty1 {
		fifty1[i] = fmt.Sprintf(`"%d"`, i+1)
	}
	for name, tc := range map[string]struct{ id, body string }{
		"_BadUUID400":      {"not-a-uuid", `{"items":["20"]}`},
		"_UnknownField400": {c.ID.String(), `{"items":["20"],"bogus":true}`},
		"_EmptyItems400":   {c.ID.String(), `{"items":[]}`},
		"too many":         {c.ID.String(), `{"items":[` + strings.Join(fifty1, ",") + `]}`},
		"bad json":         {c.ID.String(), `{`},
	} {
		w := postAppend(t, h.s, tc.id, tc.body, testOperatorIdentity())
		if w.Code != http.StatusBadRequest || decodeCampaignError(t, w) != "validation_failed" {
			t.Errorf("%s: %d %s, want 400 validation_failed", name, w.Code, w.Body.String())
		}
	}
	h.assertNothingWritten(t, c.ID, 1)
}

func TestAppendCampaignItems_WriteScope(t *testing.T) {
	h := newAppendHarness(t, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	if w := postAppend(t, h.s, c.ID.String(), `{"items":["20"]}`, Identity{}); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", w.Code)
	}
	noScope := Identity{Subject: "github:op", TokenID: "tok", Scopes: []string{"read:runs"}, AccountID: testOperatorAccountID}
	if w := postAppend(t, h.s, c.ID.String(), `{"items":["20"]}`, noScope); w.Code != http.StatusForbidden || decodeCampaignError(t, w) != "insufficient_scope" {
		t.Errorf("no scope = %d %s, want 403 insufficient_scope", w.Code, w.Body.String())
	}
	h.assertNothingWritten(t, c.ID, 1)
}

func TestAppendCampaignItems_CrossAccountRefusedWritesNothing(t *testing.T) {
	h := newAppendHarness(t, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	h.repo.accounts[c.ID] = uuid.NewString() // tenanted to account X
	caller := testOperatorIdentity()         // account Y, write-capable, repo-visible
	w := postAppend(t, h.s, c.ID.String(), `{"items":["20"]}`, caller)
	if w.Code != http.StatusForbidden || decodeCampaignError(t, w) != "account_forbidden" {
		t.Fatalf("cross-account = %d %s, want 403 account_forbidden", w.Code, w.Body.String())
	}
	h.assertNothingWritten(t, c.ID, 1)
}

func TestAppendCampaignItems_RepoInvisibleRefusedWritesNothing(t *testing.T) {
	h := newAppendHarness(t, Config{
		AccountRoles:   fakeAccountRoles{role: account.RoleMember},
		RepoVisibility: newFakeRepoVisibility(map[string]bool{}),
	})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	w := postAppend(t, h.s, c.ID.String(), `{"items":["20"]}`, memberIdentity())
	if w.Code != http.StatusForbidden || decodeCampaignError(t, w) != "repo_forbidden" {
		t.Fatalf("repo-invisible = %d %s, want 403 repo_forbidden", w.Code, w.Body.String())
	}
	h.assertNothingWritten(t, c.ID, 1)
}

func TestAppendCampaignItems_TerminalCampaign409(t *testing.T) {
	h := newAppendHarness(t, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStateCancelled))
	c.State = campaign.StateCancelled
	w := postAppend(t, h.s, c.ID.String(), `{"items":["20"]}`, testOperatorIdentity())
	if w.Code != http.StatusConflict || decodeCampaignError(t, w) != "campaign_not_appendable" {
		t.Fatalf("terminal = %d %s, want 409 campaign_not_appendable", w.Code, w.Body.String())
	}
	h.assertNothingWritten(t, c.ID, 1)
}

func TestAppendCampaignItems_DuplicateRefusedBeforeForgeRead(t *testing.T) {
	h := newAppendHarness(t, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	for _, body := range []string{`{"items":["#10"]}`, `{"items":["20","issue:20"]}`} {
		w := postAppend(t, h.s, c.ID.String(), body, testOperatorIdentity())
		if w.Code != http.StatusConflict || decodeCampaignError(t, w) != "campaign_item_duplicate" {
			t.Fatalf("%s: %d %s, want 409 campaign_item_duplicate", body, w.Code, w.Body.String())
		}
	}
	h.assertNothingWritten(t, c.ID, 1)
}

func TestAppendCampaignItems_InvalidRef422(t *testing.T) {
	h := newAppendHarness(t, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	w := postAppend(t, h.s, c.ID.String(), `{"items":["not-an-issue"]}`, testOperatorIdentity())
	if w.Code != http.StatusUnprocessableEntity || decodeCampaignError(t, w) != "campaign_item_ref_invalid" {
		t.Fatalf("invalid ref = %d %s, want 422 campaign_item_ref_invalid", w.Code, w.Body.String())
	}
	h.assertNothingWritten(t, c.ID, 1)
}

func TestAppendCampaignItems_ExternalRefused422(t *testing.T) {
	h := newAppendHarness(t, Config{})
	h.res.deps[20] = []int{999}
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	w := postAppend(t, h.s, c.ID.String(), `{"items":["20"]}`, testOperatorIdentity())
	code, details := decodeCampaignErrorDetails(t, w)
	if w.Code != http.StatusUnprocessableEntity || code != "campaign_external_dependency" {
		t.Fatalf("external = %d %s, want 422 campaign_external_dependency", w.Code, w.Body.String())
	}
	if _, ok := details["edges"]; !ok || !strings.Contains(fmt.Sprint(details["remedy"]), "allow_external_blockers") {
		t.Errorf("details = %v", details)
	}
	if n := h.itemCount(c.ID); n != 1 {
		t.Errorf("item count = %d, want 1", n)
	}
	if n := h.aud.count(categoryCampaignItemsAppended); n != 0 {
		t.Errorf("audit rows = %d, want 0", n)
	}
}

func TestAppendCampaignItems_AllowExternalBlockersAdmitsHeld(t *testing.T) {
	h := newAppendHarness(t, Config{})
	h.res.deps[20] = []int{999}
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	w := postAppend(t, h.s, c.ID.String(), `{"items":["20"],"allow_external_blockers":true}`, testOperatorIdentity())
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	b := decodeAppend(t, w)
	if !reflect.DeepEqual(b.Appended[0].DependsOn, []string{"issue:999"}) || !reflect.DeepEqual(b.Appended[0].ExternalDependsOn, []string{"issue:999"}) {
		t.Errorf("appended = %+v", b.Appended[0])
	}
	st := getCampaignStatusBody(t, h.s, c.ID)
	if !containsRef(st.Rollup.Blocked, "issue:20") {
		t.Errorf("rollup = %+v, want issue:20 held blocked", st.Rollup)
	}
}

func TestAppendCampaignItems_RefusesCycle(t *testing.T) {
	t.Run("new-new", func(t *testing.T) {
		h := newAppendHarness(t, Config{})
		h.res.deps[20] = []int{21}
		h.res.deps[21] = []int{20}
		c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
		w := postAppend(t, h.s, c.ID.String(), `{"items":["20","21"]}`, testOperatorIdentity())
		code, details := decodeCampaignErrorDetails(t, w)
		if w.Code != http.StatusUnprocessableEntity || code != "campaign_dependency_cycle" || details["cycle"] == nil {
			t.Fatalf("cycle = %d %s, want 422 campaign_dependency_cycle", w.Code, w.Body.String())
		}
		if n := h.itemCount(c.ID); n != 1 {
			t.Errorf("item count = %d, want 1", n)
		}
	})
	t.Run("new-existing", func(t *testing.T) {
		h := newAppendHarness(t, Config{})
		h.res.deps[2] = []int{1}
		// Seeded BY CONSTRUCTION: issue:1 already depends on non-member issue:2.
		c := h.seed(cItem("issue:1", []string{"issue:2"}, campaign.ItemStatePending))
		w := postAppend(t, h.s, c.ID.String(), `{"items":["2"]}`, testOperatorIdentity())
		if w.Code != http.StatusUnprocessableEntity || decodeCampaignError(t, w) != "campaign_dependency_cycle" {
			t.Fatalf("cycle = %d %s, want 422 campaign_dependency_cycle", w.Code, w.Body.String())
		}
		if n := h.itemCount(c.ID); n != 1 {
			t.Errorf("item count = %d, want 1", n)
		}
	})
}

func TestAppendCampaignItems_ExistingDriftDoesNotRefuseUnrelatedAppend(t *testing.T) {
	h := newAppendHarness(t, Config{})
	h.res.deps[10] = []int{999} // existing member drifted onto a non-member blocker
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	w := postAppend(t, h.s, c.ID.String(), `{"items":["20"]}`, testOperatorIdentity())
	if w.Code != http.StatusCreated {
		t.Fatalf("unrelated append = %d %s, want 201", w.Code, w.Body.String())
	}
	for _, asked := range h.res.asked {
		if containsRef(asked, "issue:10") {
			t.Errorf("existing member issue:10 was resolved on append: %v", asked)
		}
	}
}

func TestAppendCampaignItems_AuditFailureFailsClosed503(t *testing.T) {
	for name, cfg := range map[string]Config{
		"append error": {AuditRepo: failingAuditRepo{}},
	} {
		h := newAppendHarness(t, cfg)
		c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
		w := postAppend(t, h.s, c.ID.String(), `{"items":["20"]}`, testOperatorIdentity())
		if w.Code != http.StatusServiceUnavailable || decodeCampaignError(t, w) != "campaign_audit_unavailable" {
			t.Fatalf("%s: %d %s, want 503 campaign_audit_unavailable", name, w.Code, w.Body.String())
		}
		if n := h.itemCount(c.ID); n != 1 {
			t.Errorf("%s: item count = %d, want 1 (nothing written)", name, n)
		}
	}
	// A nil AuditRepo is "cannot record" too.
	h := newAppendHarness(t, Config{})
	h.s.cfg.AuditRepo = nil
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	if w := postAppend(t, h.s, c.ID.String(), `{"items":["20"]}`, testOperatorIdentity()); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil audit = %d %s, want 503", w.Code, w.Body.String())
	}
	if n := h.itemCount(c.ID); n != 1 {
		t.Errorf("nil audit: item count = %d, want 1", n)
	}
}

func TestAppendCampaignItems_ResolverRefusalPassesThrough(t *testing.T) {
	h := newAppendHarness(t, Config{})
	h.res.err = errors.New("forge down")
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	w := postAppend(t, h.s, c.ID.String(), `{"items":["20"]}`, testOperatorIdentity())
	if w.Code != http.StatusBadGateway || decodeCampaignError(t, w) != "issue_set_resolution_failed" {
		t.Fatalf("resolver failure = %d %s, want 502 issue_set_resolution_failed", w.Code, w.Body.String())
	}
	if n := h.itemCount(c.ID); n != 1 {
		t.Errorf("item count = %d, want 1", n)
	}
}

func TestAppendCampaignItems_NotFoundAndUnconfigured(t *testing.T) {
	h := newAppendHarness(t, Config{})
	if w := postAppend(t, h.s, uuid.NewString(), `{"items":["20"]}`, testOperatorIdentity()); w.Code != http.StatusNotFound {
		t.Errorf("missing campaign = %d, want 404", w.Code)
	}
	h.repo.getErr = errInjected
	if w := postAppend(t, h.s, uuid.NewString(), `{"items":["20"]}`, testOperatorIdentity()); w.Code != http.StatusInternalServerError {
		t.Errorf("get error = %d, want 500", w.Code)
	}
	s := New(Config{})
	if w := postAppend(t, s, uuid.NewString(), `{"items":["20"]}`, testOperatorIdentity()); w.Code != http.StatusServiceUnavailable {
		t.Errorf("nil repo = %d, want 503", w.Code)
	}
}

func TestAppendCampaignItems_ListFailure500(t *testing.T) {
	h := newAppendHarness(t, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	h.repo.itemsErr = errInjected
	if w := postAppend(t, h.s, c.ID.String(), `{"items":["20"]}`, testOperatorIdentity()); w.Code != http.StatusInternalServerError {
		t.Fatalf("list failure = %d %s, want 500", w.Code, w.Body.String())
	}
	if n := h.res.callCount(); n != 0 {
		t.Errorf("resolver calls = %d, want 0", n)
	}
}

func TestAppendCampaignItems_AwaitingHumanRederives(t *testing.T) {
	h := newAppendHarness(t, Config{})
	low := cItem("issue:10", nil, campaign.ItemStatePending)
	low.Autonomy = "low"
	c := h.seed(low)
	c.State = campaign.StateAwaitingHuman
	w := postAppend(t, h.s, c.ID.String(), `{"items":["20"]}`, testOperatorIdentity())
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	if b := decodeAppend(t, w); b.Campaign.State == string(campaign.StateAwaitingHuman) {
		t.Errorf("campaign state = %s, want re-derived off awaiting_human", b.Campaign.State)
	}
	if n := h.aud.count(categoryCampaignAdvanced); n != 1 {
		t.Errorf("campaign_advanced rows = %d, want 1", n)
	}
}

// TestAppendCampaignItemsCore_NoRecord pins the core's record=false contract:
// the write happens and NO audit row is written by the core (the caller owns
// recording).
func TestAppendCampaignItemsCore_NoRecord(t *testing.T) {
	h := newAppendHarness(t, Config{})
	c := h.seed(cItem("issue:10", nil, campaign.ItemStatePending))
	res, refusal := h.s.appendCampaignItems(context.Background(), c, appendCampaignItemsRequest{Items: []string{"20"}}, false)
	if refusal != nil {
		t.Fatalf("refusal = %+v", refusal)
	}
	if len(res.Appended) != 1 || h.itemCount(c.ID) != 2 {
		t.Fatalf("appended = %+v", res.Appended)
	}
	if n := h.aud.count(categoryCampaignItemsAppended); n != 0 {
		t.Errorf("audit rows = %d, want 0 for record=false", n)
	}
}

func TestAppendRepositoryRefusal_Mapping(t *testing.T) {
	c := &campaign.Campaign{ID: uuid.New(), State: campaign.StateRunning}
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: x", campaign.ErrAppendUnrecorded), 503, "campaign_audit_unavailable"},
		{&campaign.DuplicateItemError{Refs: []string{"issue:1"}}, 409, "campaign_item_duplicate"},
		{&campaign.ExternalDependencyError{Edges: []campaign.ExternalEdge{{From: "issue:1", To: "issue:2"}}}, 422, "campaign_external_dependency"},
		{&campaign.CycleError{Cycle: []string{"issue:1", "issue:1"}}, 422, "campaign_dependency_cycle"},
		{campaign.InvalidTransitionError{Kind: "campaign", From: "succeeded", To: "append"}, 409, "campaign_not_appendable"},
		{fmt.Errorf("%w: unparsable:x", campaign.ErrInvalidDependencyRef), 422, "campaign_item_ref_invalid"},
		{campaign.ErrCampaignNotFound, 404, "campaign_not_found"},
		{errors.New("db down"), 500, "internal_error"},
	}
	for _, tc := range cases {
		r := appendRepositoryRefusal(c, tc.err)
		if r.Status != tc.status || r.Code != tc.code {
			t.Errorf("%v -> %d %s, want %d %s", tc.err, r.Status, r.Code, tc.status, tc.code)
		}
	}
}
