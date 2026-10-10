package mcpserver

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/refinement"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// draft_epic_contract_test.go is the cross-boundary contract test for the
// detached refinement file arm (#4153, approval condition C2): the REAL
// apiClient, over HTTP, into the REAL server.Handler() — the real refinement
// file handler and session GET over a REAL Postgres refinement repo — decoding
// the ACTUAL 202 body and session JSON into this package's mirror types
// (RefinementFilingResult, RefinementFilingProgress). The draft_epic_test.go
// fake backend hand-writes the JSON, so a server-side JSON tag rename would
// leave every one of those tests green; this one decodes what the server
// really emits, and asserts the load-bearing fields decode NON-zero.

// contractProvider is a gated in-memory work-management provider registered
// under a unique name, so the real handler's detached filing files through it
// without a forge. Its FIRST File blocks until the gate is released (respecting
// ctx), which pins the detached filing in flight while the test reads the
// in-progress views. It fires the write-ahead hook like a real provider.
type contractProvider struct {
	name    string
	entered chan struct{}
	gate    chan struct{}
	once    sync.Once

	mu      sync.Mutex
	next    int
	creates int
}

func (p *contractProvider) Name() string { return p.name }

func (p *contractProvider) File(ctx context.Context, req workmgmt.ProviderRequest) (*workmgmt.CreatedItem, error) {
	var gateErr error
	p.once.Do(func() {
		close(p.entered)
		select {
		case <-p.gate:
		case <-ctx.Done():
			gateErr = ctx.Err()
		}
	})
	if gateErr != nil {
		return nil, gateErr
	}
	p.mu.Lock()
	p.creates++
	num := p.next
	p.next++
	p.mu.Unlock()
	created := &workmgmt.CreatedItem{Provider: p.name, Number: num, URL: fmt.Sprintf("https://tracker.test/o/r/issues/%d", num)}
	req.NotifyCreated(ctx, created)
	return created, nil
}

// DiscoverNumbers reports an empty tracker, so the numbered epic allocates its
// first number server-side (#1269).
func (*contractProvider) DiscoverNumbers(context.Context, workmgmt.DiscoverNumbersRequest) ([]int, error) {
	return nil, nil
}

// contractConventions is Default() rerouted to the provider, with the feature
// title reduced to {summary}: the default [E{epic}.{n}] needs the epic's title
// read back over a GitHub client this test does not wire. The Types map is
// COPIED — Default() shares its maps with the process-wide default.
func contractConventions(provider string) workmgmt.Conventions {
	conv := workmgmt.Default()
	conv.Provider = provider
	types := make(map[string]workmgmt.ItemType, len(conv.Types))
	for k, v := range conv.Types {
		types[k] = v
	}
	feature := types["feature"]
	feature.TitleFormat = "{summary}"
	types["feature"] = feature
	conv.Types = types
	return conv
}

func TestDraftEpicFileArm_ContractAgainstRealServer(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	repo := refinement.NewPostgresRepository(pool)

	provider := &contractProvider{
		name:    "draft_epic_contract_" + uuid.NewString(),
		entered: make(chan struct{}),
		gate:    make(chan struct{}),
		next:    500,
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(provider.gate) }) }
	workmgmt.Register(provider)
	server.SetConventionsLoader(func(context.Context, string) (workmgmt.Conventions, error) {
		return contractConventions(provider.name), nil
	})
	t.Cleanup(func() {
		server.SetConventionsLoader(func(context.Context, string) (workmgmt.Conventions, error) {
			return workmgmt.Default(), nil
		})
	})

	// An approved two-child draft, seeded straight into the repository.
	draft := refinement.EpicDraft{
		Epic: refinement.EpicSpec{Summary: "contract epic", Scope: "the contract", OutOfScope: "nothing"},
		Children: []refinement.ChildDraft{
			{Summary: "contract child 1", Proposal: "p", DoneMeans: "d", AcceptanceCriteria: []string{"a"}, Labels: []string{"area:backend", "autonomy:medium"}},
			{Summary: "contract child 2", Proposal: "p", DoneMeans: "d", AcceptanceCriteria: []string{"a"}, Labels: []string{"area:backend", "autonomy:medium"}, DependsOn: []int{1}},
		},
	}
	sessionID := uuid.New()
	stored, err := repo.CreateDraft(ctx, refinement.CreateParams{SessionID: sessionID, Brief: "b", Draft: draft})
	if err != nil {
		t.Fatalf("CreateDraft: %v", err)
	}
	hash, err := refinement.ContentHash(draft)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if _, err := repo.RecordDecision(ctx, refinement.DecisionParams{
		SessionID: sessionID, DraftID: stored.ID, Decision: refinement.DecisionApproved,
		Reason: "ok", DraftContentHash: hash,
	}); err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}

	const bearer = "fhk_draft_epic_contract"
	tokRepo := &stubMCPAPITokens{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "github:operator",
		Scopes: []string{"write:approvals", "read:runs"}, PlainText: bearer,
	}}
	s := server.New(server.Config{RefinementRepo: repo, AuditRepo: audit.NewPostgresRepository(pool), APITokenRepo: tokRepo})
	httpSrv := httptest.NewServer(s.Handler())
	// Registered AFTER pgtest.NewPool, so this runs FIRST: release the gate and
	// drain the detached filing before the pool closes, even on a failure.
	t.Cleanup(func() {
		release()
		httpSrv.Close()
		sctx, cancel := context.WithTimeout(context.Background(), timescale.D(10*time.Second))
		defer cancel()
		_ = s.Shutdown(sctx)
	})
	r := &runResolver{api: newAPIClient(config{backendURL: httpSrv.URL, apiToken: bearer})}
	const target = "kuhlman-labs/fishhawk"

	// --- the 202 launch body decodes into RefinementFilingResult ---
	_, out, err := r.draftEpic(ctx, nil, DraftEpicInput{SessionID: sessionID.String(), Repo: target})
	if err != nil {
		t.Fatalf("file arm (launch): %v", err)
	}
	if out.Filing == nil || out.Filing.Status != "filing_in_progress" {
		t.Fatalf("launch filing = %+v, want status filing_in_progress", out.Filing)
	}
	if out.Filing.AlreadyInProgress || out.Filing.ChildCount != 2 || out.Filing.BudgetSeconds <= 0 || out.Filing.Repo != target {
		t.Errorf("launch filing = %+v, want already_in_progress=false, child_count=2, budget_seconds>0, repo %s", out.Filing, target)
	}
	if len(out.SessionGuidance) != 1 || out.SessionGuidance[0].Arm != "preview" {
		t.Errorf("launch guidance = %+v, want preview", out.SessionGuidance)
	}

	select {
	case <-provider.entered:
	case <-time.After(timescale.D(10 * time.Second)):
		t.Fatal("the detached filing never reached the provider")
	}

	// --- a concurrent call: 202 already_in_progress decodes NON-zero ---
	_, out, err = r.draftEpic(ctx, nil, DraftEpicInput{SessionID: sessionID.String(), Repo: target})
	if err != nil {
		t.Fatalf("file arm (concurrent): %v", err)
	}
	if out.Filing == nil || out.Filing.Status != "already_in_progress" || !out.Filing.AlreadyInProgress {
		t.Fatalf("concurrent filing = %+v, want status already_in_progress, already_in_progress=true", out.Filing)
	}
	if len(out.SessionGuidance) != 1 || out.SessionGuidance[0].Arm != "preview" {
		t.Errorf("concurrent guidance = %+v, want preview", out.SessionGuidance)
	}

	// --- the session GET's filing block decodes into RefinementFilingProgress ---
	_, out, err = r.draftEpic(ctx, nil, DraftEpicInput{SessionID: sessionID.String()})
	if err != nil {
		t.Fatalf("preview (in progress): %v", err)
	}
	f := out.Session.Filing
	if f == nil || f.State != "in_progress" || !f.InFlight || f.ChildCount != 2 || f.Repo != target || f.StartedAt == nil {
		t.Fatalf("in-progress filing block = %+v, want state in_progress, in_flight, child_count 2, repo %s, started_at", f, target)
	}
	if len(out.SessionGuidance) != 1 || out.SessionGuidance[0].State != "filing_in_progress" || out.SessionGuidance[0].Arm != "preview" {
		t.Errorf("in-progress guidance = %+v, want filing_in_progress -> preview", out.SessionGuidance)
	}

	// --- release the forge; the filing completes and the session reads filed ---
	release()
	deadline := time.Now().Add(timescale.D(10 * time.Second))
	for {
		_, out, err = r.draftEpic(ctx, nil, DraftEpicInput{SessionID: sessionID.String()})
		if err != nil {
			t.Fatalf("preview (polling): %v", err)
		}
		if f := out.Session.Filing; f != nil && f.State != "in_progress" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the detached filing did not settle within the bound; last filing block = %+v", out.Session.Filing)
		}
		time.Sleep(20 * time.Millisecond)
	}
	f = out.Session.Filing
	if out.Session.State != "filed" || f.State != "filed" {
		t.Fatalf("settled session state=%q filing=%+v, want filed/filed", out.Session.State, f)
	}
	if f.FiledCount != 3 || f.Epic == nil || f.Epic.Number == 0 || f.Epic.URL == "" || len(f.Children) != 2 || f.CompletedAt == nil {
		t.Errorf("filed block = %+v, want 3 items (epic + 2 children) with numbers/urls and completed_at", f)
	}
	for i, c := range f.Children {
		if c.Ordinal != i+1 || c.Number == 0 || c.URL == "" {
			t.Errorf("child[%d] = %+v, want ordinal %d with a number and url", i, c, i+1)
		}
	}
	if len(out.SessionGuidance) != 1 || out.SessionGuidance[0].Arm != "terminal" {
		t.Errorf("filed guidance = %+v, want terminal", out.SessionGuidance)
	}

	// --- a completed session's file arm replays 200 status filed, no creates ---
	provider.mu.Lock()
	createsBefore := provider.creates
	provider.mu.Unlock()
	_, out, err = r.draftEpic(ctx, nil, DraftEpicInput{SessionID: sessionID.String(), Repo: target})
	if err != nil {
		t.Fatalf("file arm (replay): %v", err)
	}
	if out.Filing == nil || out.Filing.Status != "filed" || !out.Filing.AlreadyCompleted || out.Filing.Epic == nil || len(out.Filing.Children) != 2 {
		t.Fatalf("replay filing = %+v, want status filed, already_completed, the epic + 2 children", out.Filing)
	}
	if len(out.SessionGuidance) != 1 || out.SessionGuidance[0].Arm != "terminal" {
		t.Errorf("replay guidance = %+v, want terminal", out.SessionGuidance)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.creates != createsBefore || provider.creates != 3 {
		t.Errorf("provider creates = %d (before replay %d), want exactly 3 and none on the replay", provider.creates, createsBefore)
	}
}
