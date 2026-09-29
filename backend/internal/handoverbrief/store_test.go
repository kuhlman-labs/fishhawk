package handoverbrief

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/digest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

const testRepo = "org/brief"

// fixture seeds ONE repository against the REAL digest store, decision index,
// captain store and campaign/run repositories.
type fixture struct {
	pool      *pgxpool.Pool
	audit     audit.Repository
	idx       *decisionindex.Store
	digest    *digest.Store
	captain   *captain.Store
	campaigns campaign.Repository
	runs      run.Repository
	run       uuid.UUID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := pgtest.NewPool(t)
	f := &fixture{
		pool: pool, audit: audit.NewPostgresRepository(pool), idx: decisionindex.NewStore(pool),
		digest: digest.NewStore(pool), captain: captain.NewStore(pool),
		campaigns: campaign.NewPostgresRepository(pool), runs: run.NewPostgresRepository(pool),
	}
	f.run = f.seedRun(t, "running")
	return f
}

func (f *fixture) deps() Deps {
	return Deps{
		Digest:   digest.Deps{Store: f.digest, Index: f.idx},
		Captain:  f.captain,
		InFlight: NewStore(campaignLister{f.campaigns}, f.runs),
	}
}

func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("seed %q: %v", sql, err)
	}
}

func (f *fixture) seedRun(t *testing.T, state string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.exec(t, `INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind)
	           VALUES ($1, $2, 'feature_change', 'sha', 'cli', $3, 'local')`, id, testRepo, state)
	return id
}

func (f *fixture) seedStage(t *testing.T, runID uuid.UUID, seq int, kind, state string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.exec(t, `INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
	           VALUES ($1, $2, $3, $4, 'agent', 'claude-code', $5)`, id, runID, seq, kind, state)
	return id
}

func (f *fixture) appendEntry(t *testing.T, runID uuid.UUID, stageID *uuid.UUID, category string) *audit.Entry {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"reason": "r"})
	kind, subject := audit.ActorUser, "operator"
	e, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: stageID, Timestamp: time.Now().UTC(), Category: category,
		ActorKind: &kind, ActorSubject: &subject, Payload: raw,
	})
	if err != nil {
		t.Fatalf("append %s: %v", category, err)
	}
	return e
}

func (f *fixture) index(t *testing.T, e *audit.Entry, class decisionindex.DecisionClass) {
	t.Helper()
	r := decisionindex.Row{
		SourceSequence: e.Sequence, SourceEntryHash: e.EntryHash, RunID: *e.RunID, StageID: e.StageID,
		Repo: testRepo, WorkflowID: "feature_change", DoctrineVersion: "sha", DecisionClass: class,
		StageKind: "implement", Outcome: "merged", ConcernCategoryRaw: "correctness", Severity: "high",
		DecidedAt: e.Timestamp, ReasonSequence: e.Sequence, ReasonKey: "reason",
	}
	if err := f.idx.Upsert(context.Background(), r); err != nil {
		t.Fatalf("index %d: %v", e.Sequence, err)
	}
}

// merge appends and indexes a merge verdict on a FRESH run (the category is
// once-per-run).
func (f *fixture) merge(t *testing.T) *audit.Entry {
	t.Helper()
	e := f.appendEntry(t, f.seedRun(t, "succeeded"), nil, "merge_verdict_recorded")
	f.index(t, e, decisionindex.ClassMergeVerdict)
	return e
}

// handover drives claim -> offer -> accept through the REAL captain store, so
// the chain carries a captain_assigned entry; it returns that entry.
func (f *fixture) handover(t *testing.T, from, to string) *audit.Entry {
	t.Helper()
	ctx := context.Background()
	apply := func(actor string, decide func(captain.State) (captain.Event, error)) *captain.Applied {
		a, err := f.captain.Apply(ctx, captain.ApplyParams{Repo: testRepo, Actor: actor, ActorKind: audit.ActorUser, Timestamp: time.Now().UTC()}, decide)
		if err != nil {
			t.Fatalf("captain apply by %s: %v", actor, err)
		}
		return a
	}
	apply(from, func(s captain.State) (captain.Event, error) {
		return captain.Claim(s, captain.Params{Repo: testRepo, Actor: from, Predicate: captain.PredicateTrivial, PredicateBasis: "trivial:test"})
	})
	apply(from, func(s captain.State) (captain.Event, error) {
		return captain.Offer(s, captain.Params{Repo: testRepo, Actor: from, Successor: to})
	})
	return apply(to, func(s captain.State) (captain.Event, error) {
		return captain.Accept(s, captain.Params{Repo: testRepo, Actor: to})
	}).Entry
}

func (f *fixture) campaign(t *testing.T, state campaign.State) *campaign.Campaign {
	t.Helper()
	ctx := context.Background()
	c, err := f.campaigns.CreateCampaign(ctx, campaign.CreateCampaignParams{Repo: testRepo, EpicRef: "issue:" + uuid.NewString()[:6]})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	if state != c.State {
		if c, err = f.campaigns.TransitionCampaign(ctx, c.ID, state); err != nil {
			t.Fatalf("transition campaign to %s: %v", state, err)
		}
	}
	return c
}

// TestStore_CampaignsAndRunsAreRepoAndStateScoped: the in-flight reads return
// only the repo's rows in the asked state, mapped with their identity.
func TestStore_CampaignsAndRunsAreRepoAndStateScoped(t *testing.T) {
	f := newFixture(t)
	running := f.campaign(t, campaign.StateRunning)
	f.campaign(t, campaign.StatePending)
	st := NewStore(campaignLister{f.campaigns}, f.runs)
	ctx := context.Background()

	got, err := st.Campaigns(ctx, testRepo, nil, "running", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != running.ID || got[0].Kind != "campaign" || got[0].State != "running" || got[0].Ref != running.EpicRef {
		t.Errorf("running campaigns = %+v, want exactly %s", got, running.ID)
	}
	runs, err := st.Runs(ctx, testRepo, nil, "running", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != f.run || runs[0].Kind != "run" || runs[0].WorkflowID != "feature_change" {
		t.Errorf("running runs = %+v, want exactly the fixture run %s", runs, f.run)
	}
	other, err := st.Runs(ctx, "org/other", nil, "running", 10)
	if err != nil || len(other) != 0 {
		t.Errorf("other repo runs = %+v (%v), want none", other, err)
	}
}

// TestStore_LimitPlusOneBite: seeding limit+1 running campaigns, the composed
// part carries exactly limit items, a scan_limit degradation, and a cursor
// whose offset is the FIRST OMITTED row's.
func TestStore_LimitPlusOneBite(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 3; i++ {
		f.campaign(t, campaign.StateRunning)
	}
	all, err := NewStore(campaignLister{f.campaigns}, f.runs).Campaigns(context.Background(), testRepo, nil, "running", 10)
	if err != nil || len(all) != 3 {
		t.Fatalf("all running = %d (%v), want 3", len(all), err)
	}
	b, err := Compose(context.Background(), f.deps(), Request{Repo: testRepo, ScanLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	p := partOf(t, b, SectionInFlight, PartCampaigns)
	if len(p.InFlight) != 2 || !p.Truncated || p.Complete || p.Next == nil || p.Next.Offset != 2 {
		t.Fatalf("campaigns part = %+v, want 2 items truncated with offset-2 cursor", p)
	}
	if p.InFlight[0].ID != all[0].ID || p.InFlight[1].ID != all[1].ID {
		t.Errorf("kept %v, want the first two of %v", p.InFlight, all)
	}
	if !strings.HasPrefix(p.Next.Call, "GET /v0/campaigns?") || !strings.Contains(p.Next.Call, "state=running") ||
		!strings.Contains(p.Next.Call, "cursor="+url.QueryEscape(encodeOffset(2))) {
		t.Errorf("cursor call = %q, want the running-campaigns list at offset 2", p.Next.Call)
	}
	if !hasDegradation(b, DegradationScanLimit, PartCampaigns) {
		t.Errorf("degradations = %+v, want a scan_limit on campaigns", b.Degradations)
	}
	if b.BriefHash == "" {
		t.Error("brief hash empty on a degraded brief")
	}
}

// campaignLister is the test-side campaign.Repository adapter (production's
// lives in server, keeping campaign out of this package's non-test closure).
type campaignLister struct{ repo campaign.Repository }

func (l campaignLister) ListInFlightCampaigns(ctx context.Context, repo, accountID, state string, limit int) ([]InFlightItem, error) {
	rows, err := l.repo.ListCampaigns(ctx, campaign.ListCampaignsFilter{Repo: repo, State: state, AccountID: accountID, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]InFlightItem, 0, len(rows))
	for _, c := range rows {
		if c == nil {
			continue
		}
		out = append(out, InFlightItem{Kind: "campaign", ID: c.ID, State: string(c.State), Ref: c.EpicRef, CreatedAt: c.CreatedAt.UTC()})
	}
	return out, nil
}

type errCampaigns struct{}

func (errCampaigns) ListInFlightCampaigns(context.Context, string, string, string, int) ([]InFlightItem, error) {
	return nil, errors.New("boom")
}

type errRuns struct{}

func (errRuns) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, errors.New("boom")
}

// TestStore_UnconfiguredListersError: a nil lister is an error at the Store,
// never a nil dereference.
func TestStore_UnconfiguredListersError(t *testing.T) {
	st := NewStore(nil, nil)
	if _, err := st.Campaigns(context.Background(), testRepo, nil, "running", 1); err == nil {
		t.Error("nil campaign lister: want error")
	}
	if _, err := st.Runs(context.Background(), testRepo, nil, "running", 1); err == nil {
		t.Error("nil run lister: want error")
	}
	id := uuid.New()
	if accountArg(&id) != id.String() || accountArg(nil) != "" {
		t.Error("accountArg mapping wrong")
	}
}
