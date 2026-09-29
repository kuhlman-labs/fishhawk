package handoverbrief

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/digest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

func partOf(t *testing.T, b Brief, sec SectionKind, part PartKind) Part {
	t.Helper()
	for _, s := range b.Sections {
		if s.Kind != sec {
			continue
		}
		for _, p := range s.Parts {
			if p.Kind == part {
				return p
			}
		}
	}
	t.Fatalf("brief has no %s/%s part: %+v", sec, part, b.Sections)
	return Part{}
}

func sectionOf(t *testing.T, b Brief, sec SectionKind) Section {
	t.Helper()
	for _, s := range b.Sections {
		if s.Kind == sec {
			return s
		}
	}
	t.Fatalf("brief has no %s section", sec)
	return Section{}
}

func hasDegradation(b Brief, kind string, part PartKind) bool {
	for _, d := range b.Degradations {
		if d.Kind == kind && d.Part == part {
			return true
		}
	}
	return false
}

func cites(items []digest.Item, seq int64, hash string) bool {
	for _, it := range items {
		if it.SourceSequence == seq && it.SourceEntryHash == hash {
			return true
		}
	}
	return false
}

func compose(t *testing.T, deps Deps, req Request) Brief {
	t.Helper()
	if req.Repo == "" {
		req.Repo = testRepo
	}
	b, err := Compose(context.Background(), deps, req)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	return b
}

// TestHandoverBrief_SeededRepositoryListsEveryItem is the cross-boundary
// end-to-end: the REAL digest.Build, captain store and campaign/run
// repositories over one seeded repository. Every item appears WITH the chain
// sequence and entry hash of the entry it is about.
func TestHandoverBrief_SeededRepositoryListsEveryItem(t *testing.T) {
	f := newFixture(t)
	planStage := f.seedStage(t, f.run, 1, "plan", "awaiting_approval")
	planGate := f.appendEntry(t, f.run, &planStage, "plan_generated")
	clarRun := f.seedRun(t, "running")
	clarStage := f.seedStage(t, clarRun, 1, "implement", "awaiting_input")
	clar := f.appendEntry(t, clarRun, &clarStage, "clarification_requested")
	w1 := f.appendEntry(t, f.run, nil, "concern_waived")
	f.index(t, w1, decisionindex.ClassConcernWaive)
	w2 := f.appendEntry(t, f.run, nil, "concern_waived")
	f.index(t, w2, decisionindex.ClassConcernWaive)
	camp := f.campaign(t, campaign.StateRunning)

	b := compose(t, f.deps(), Request{})

	if b.Window.Basis != BasisFirstCaptain || b.Window.FromSequence != 1 {
		t.Errorf("window = %+v, want first_captain from 1", b.Window)
	}
	waivers := partOf(t, b, SectionWhatChanged, PartWaiversAndDeferrals).Items
	if !cites(waivers, w1.Sequence, w1.EntryHash) || !cites(waivers, w2.Sequence, w2.EntryHash) {
		t.Errorf("waivers %+v missing a citation (%d/%s, %d/%s)", waivers, w1.Sequence, w1.EntryHash, w2.Sequence, w2.EntryHash)
	}
	if open := partOf(t, b, SectionNeedsDecision, PartOpenDecisions).Items; !cites(open, planGate.Sequence, planGate.EntryHash) {
		t.Errorf("open decisions %+v missing parked plan gate %d/%s", open, planGate.Sequence, planGate.EntryHash)
	}
	pages := partOf(t, b, SectionNeedsDecision, PartUnansweredPages).Items
	if !cites(pages, clar.Sequence, clar.EntryHash) {
		t.Errorf("unanswered pages %+v missing clarification %d/%s", pages, clar.Sequence, clar.EntryHash)
	}
	found := false
	for _, it := range partOf(t, b, SectionInFlight, PartCampaigns).InFlight {
		found = found || it.ID == camp.ID
	}
	if !found {
		t.Errorf("in_flight campaigns missing %s", camp.ID)
	}
	if b.BriefHash == "" || len(b.BriefHash) != 64 {
		t.Errorf("brief hash = %q, want a hex sha256", b.BriefHash)
	}
}

// TestNeedsDecision_AnsweredPagesAreFiltered: an answered clarification is not
// a page needing a decision; the unanswered one is.
func TestNeedsDecision_AnsweredPagesAreFiltered(t *testing.T) {
	f := newFixture(t)
	st := f.seedStage(t, f.run, 1, "implement", "running")
	asked := f.appendEntry(t, f.run, &st, "clarification_requested")
	f.appendEntry(t, f.run, &st, "clarification_answered")
	b := compose(t, f.deps(), Request{})
	if pages := partOf(t, b, SectionNeedsDecision, PartUnansweredPages).Items; cites(pages, asked.Sequence, asked.EntryHash) {
		t.Errorf("answered page %d listed as needing a decision: %+v", asked.Sequence, pages)
	}
}

// TestWindow_StartsAtLastCaptainAssigned: a merge BEFORE the captain_assigned
// entry is absent from what_changed; one AFTER is present. The earlier merge
// is the fixture state that makes the from = assigned+1 clamp observable.
func TestWindow_StartsAtLastCaptainAssigned(t *testing.T) {
	f := newFixture(t)
	before := f.merge(t)
	assigned := f.handover(t, "github:alice", "github:bob")
	after := f.merge(t)

	b := compose(t, f.deps(), Request{})
	if b.Window.Basis != BasisSinceLastHandover || b.Window.FromSequence != assigned.Sequence+1 ||
		b.Window.AssignedSequence != assigned.Sequence || b.Window.AssignedEntryHash != assigned.EntryHash {
		t.Errorf("window = %+v, want since_last_handover from %d", b.Window, assigned.Sequence+1)
	}
	merges := partOf(t, b, SectionWhatChanged, PartMerges).Items
	if cites(merges, before.Sequence, before.EntryHash) {
		t.Errorf("pre-handover merge %d present in what_changed: %+v", before.Sequence, merges)
	}
	if !cites(merges, after.Sequence, after.EntryHash) {
		t.Errorf("post-handover merge %d absent from what_changed: %+v", after.Sequence, merges)
	}
	if b.CaptainSubject != "github:bob" {
		t.Errorf("captain = %q, want github:bob", b.CaptainSubject)
	}
}

// TestWindow_FirstCaptainIncludesFullHistory: with no captain_assigned entry
// the window starts at 1 and earlier events are present.
func TestWindow_FirstCaptainIncludesFullHistory(t *testing.T) {
	f := newFixture(t)
	early := f.merge(t)
	b := compose(t, f.deps(), Request{})
	if b.Window.Basis != BasisFirstCaptain || b.Window.FromSequence != 1 || b.Window.AssignedSequence != 0 {
		t.Errorf("window = %+v, want first_captain from 1", b.Window)
	}
	if !cites(partOf(t, b, SectionWhatChanged, PartMerges).Items, early.Sequence, early.EntryHash) {
		t.Errorf("merge %d absent under first_captain", early.Sequence)
	}
}

// TestDeriveWindow_Pure: the LAST captain_assigned wins regardless of order;
// other captain kinds do not move the window.
func TestDeriveWindow_Pure(t *testing.T) {
	entries := []captain.ChainEntry{
		{Sequence: 9, EntryHash: "h9", Category: captain.CategoryAssigned},
		{Sequence: 4, EntryHash: "h4", Category: captain.CategoryAssigned},
		{Sequence: 12, EntryHash: "h12", Category: captain.CategoryClaimed},
	}
	w := DeriveWindow(entries, 20)
	if w.FromSequence != 10 || w.ToSequence != 20 || w.AssignedEntryHash != "h9" || w.Basis != BasisSinceLastHandover {
		t.Errorf("window = %+v, want from 10 to 20 after h9", w)
	}
	if w := DeriveWindow(nil, 7); w.FromSequence != 1 || w.Basis != BasisFirstCaptain || w.ChainHead != 7 {
		t.Errorf("empty window = %+v", w)
	}
}

// TestCompose_WindowOverrides: to_sequence pins the upper bound, beyond the
// head is invalid, from_sequence sets basis requested.
func TestCompose_WindowOverrides(t *testing.T) {
	f := newFixture(t)
	m1 := f.merge(t)
	f.merge(t)
	b := compose(t, f.deps(), Request{FromSequence: m1.Sequence, ToSequence: m1.Sequence})
	if b.Window.Basis != BasisRequested || b.Window.ToSequence != m1.Sequence {
		t.Errorf("window = %+v", b.Window)
	}
	if items := partOf(t, b, SectionWhatChanged, PartMerges).Items; len(items) != 1 || items[0].SourceSequence != m1.Sequence {
		t.Errorf("merges = %+v, want only %d", items, m1.Sequence)
	}
	if _, err := Compose(context.Background(), f.deps(), Request{Repo: testRepo, ToSequence: b.Window.ChainHead + 100}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("beyond head err = %v, want ErrInvalidRequest", err)
	}
	for _, r := range []Request{{}, {Repo: testRepo, FromSequence: -1}} {
		if _, err := Compose(context.Background(), f.deps(), r); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Compose(%+v) err = %v, want ErrInvalidRequest", r, err)
		}
	}
}

func projectedView(t *testing.T) *delegationview.View {
	t.Helper()
	doc := "version: \"2\"\nworkflows:\n  wf:\n    autonomy: high\n    escalations:\n      - match:\n          paths: [\"backend/**\"]\n        require:\n          max_autonomy: medium\n" +
		"    stages:\n      - id: plan\n        type: plan\n        executor:\n          agent: claude-code\n        produces:\n          - artifact: plan\n            schema: standard_v1\n"
	parsed, err := spec.ParseBytes([]byte(doc))
	if err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	ws := delegationview.Project(parsed)
	return &delegationview.View{Repo: testRepo, Source: "default_branch", WorkflowSHA: "abc123", SpecVersion: "2", SchemaMajor: 2,
		ContentHash: delegationview.HashWorkflows(ws), Workflows: ws}
}

// TestDelegationSection_ConfirmationIsUnavailable: on a REAL projected view,
// each workflow's confirmation is exactly `unavailable`, and standing orders
// carry the spec identity.
func TestDelegationSection_ConfirmationIsUnavailable(t *testing.T) {
	f := newFixture(t)
	deps := f.deps()
	deps.Delegation = projectedView(t)
	b := compose(t, deps, Request{})
	ws := partOf(t, b, SectionDelegationInForce, PartWorkflows).Workflows
	if len(ws) != 1 || ws[0].WorkflowID != "wf" || ws[0].Autonomy != "high" || ws[0].Confirmation != "unavailable" ||
		ws[0].ContentHash != deps.Delegation.Workflows[0].ContentHash {
		t.Fatalf("delegation = %+v, want wf/high with confirmation unavailable", ws)
	}
	if len(ws[0].Escalations) != 1 || ws[0].Escalations[0].MaxAutonomy != "medium" {
		t.Errorf("escalations = %+v, want one medium ceiling", ws[0].Escalations)
	}
	so := sectionOf(t, b, SectionStandingOrders)
	if so.StandingOrders == nil || so.StandingOrders.WorkflowSHA != "abc123" || so.StandingOrders.SchemaMajor != 2 ||
		so.StandingOrders.DelegationContentHash != deps.Delegation.ContentHash || so.Unavailable {
		t.Errorf("standing orders = %+v", so)
	}
}

// TestAbsentSections_NameCharterRevisionAndAdrIndex (condition 3):
// charter_revision, adr_index and doctrine_changes are DECLARED absent, each
// with a reason and anchor.
func TestAbsentSections_NameCharterRevisionAndAdrIndex(t *testing.T) {
	f := newFixture(t)
	b := compose(t, f.deps(), Request{})
	want := map[string]string{"charter_revision": "#3242", "adr_index": "E78", "doctrine_changes": "#3733"}
	if len(b.Absent) != len(want) {
		t.Fatalf("absent = %+v, want %d entries", b.Absent, len(want))
	}
	for _, a := range b.Absent {
		if want[a.Section] != a.Anchor || a.Reason == "" {
			t.Errorf("absent %+v, want anchor %q and a reason", a, want[a.Section])
		}
	}
	for _, s := range b.Sections {
		if s.Kind == "doctrine_changes" {
			t.Error("doctrine_changes emitted as a section despite having no source")
		}
	}
}

// ---- degradations: one behavioural assertion per named branch ----

type failingIndex struct{}

func (failingIndex) List(context.Context, decisionindex.ListFilter) ([]decisionindex.Row, error) {
	return nil, errors.New("index down")
}

func (failingIndex) GapsInWindow(context.Context, decisionindex.GapFilter) (*decisionindex.GapReport, error) {
	return nil, errors.New("index down")
}

// TestDegradation_DigestSectionFailed (condition 2): a digest section read
// error marks that part unavailable, the rest composes, and the brief STILL
// carries a hash.
func TestDegradation_DigestSectionFailed(t *testing.T) {
	f := newFixture(t)
	st := f.seedStage(t, f.run, 1, "plan", "awaiting_approval")
	gate := f.appendEntry(t, f.run, &st, "plan_generated")
	deps := f.deps()
	deps.Digest.Index = failingIndex{}
	b := compose(t, deps, Request{})
	for _, k := range []PartKind{PartMerges, PartWaiversAndDeferrals} {
		p := partOf(t, b, SectionWhatChanged, k)
		if !p.Unavailable || p.UnavailableReason != DegradationDigestSectionFailed {
			t.Errorf("%s part = %+v, want unavailable digest_section_failed", k, p)
		}
		if !hasDegradation(b, DegradationDigestSectionFailed, k) {
			t.Errorf("no digest_section_failed degradation for %s", k)
		}
	}
	if !sectionOf(t, b, SectionWhatChanged).Unavailable {
		t.Error("what_changed not marked unavailable with every part failed")
	}
	if !cites(partOf(t, b, SectionNeedsDecision, PartOpenDecisions).Items, gate.Sequence, gate.EntryHash) {
		t.Error("open decisions did not compose beside the failed digest section")
	}
	if b.BriefHash == "" {
		t.Error("degraded brief has no hash")
	}
}

func TestDegradation_StoresUnconfigured(t *testing.T) {
	f := newFixture(t)
	deps := f.deps()
	deps.InFlight = nil
	b := compose(t, deps, Request{})
	if p := partOf(t, b, SectionInFlight, PartCampaigns); !p.Unavailable || !hasDegradation(b, DegradationCampaignStoreUnconfigured, PartCampaigns) {
		t.Errorf("campaigns = %+v degradations %+v, want campaign_store_unconfigured", p, b.Degradations)
	}
	if p := partOf(t, b, SectionInFlight, PartRuns); !p.Unavailable || !hasDegradation(b, DegradationRunStoreUnconfigured, PartRuns) {
		t.Errorf("runs = %+v, want run_store_unconfigured", p)
	}
	if !sectionOf(t, b, SectionInFlight).Unavailable || b.BriefHash == "" {
		t.Error("in_flight should be unavailable and the brief still hashed")
	}
	// A campaign-only store degrades runs alone.
	deps.InFlight = NewStore(f.campaigns, nil)
	b = compose(t, deps, Request{})
	if partOf(t, b, SectionInFlight, PartCampaigns).Unavailable || !partOf(t, b, SectionInFlight, PartRuns).Unavailable {
		t.Error("nil run lister should degrade only runs")
	}
}

func TestDegradation_ReadFailures(t *testing.T) {
	f := newFixture(t)
	deps := f.deps()
	deps.InFlight = NewStore(errCampaigns{}, errRuns{})
	b := compose(t, deps, Request{})
	if !partOf(t, b, SectionInFlight, PartCampaigns).Unavailable || !hasDegradation(b, DegradationCampaignReadFailed, PartCampaigns) {
		t.Errorf("want campaign_read_failed: %+v", b.Degradations)
	}
	if !partOf(t, b, SectionInFlight, PartRuns).Unavailable || !hasDegradation(b, DegradationRunReadFailed, PartRuns) {
		t.Errorf("want run_read_failed: %+v", b.Degradations)
	}
	if partOf(t, b, SectionWhatChanged, PartMerges).Unavailable {
		t.Error("an in-flight read failure took down what_changed")
	}
}

func TestDegradation_DelegationUnavailable(t *testing.T) {
	f := newFixture(t)
	deps := f.deps()
	deps.DelegationUnavailableReason = "spec unparseable"
	b := compose(t, deps, Request{})
	if p := partOf(t, b, SectionDelegationInForce, PartWorkflows); !p.Unavailable || p.UnavailableReason != DegradationDelegationUnavailable {
		t.Errorf("workflows part = %+v", p)
	}
	found := false
	for _, d := range b.Degradations {
		found = found || (d.Kind == DegradationDelegationUnavailable && d.Detail == "spec unparseable")
	}
	if !found {
		t.Errorf("degradations = %+v, want delegation_unavailable carrying the reason", b.Degradations)
	}
	if so := sectionOf(t, b, SectionStandingOrders); !so.Unavailable || so.StandingOrders != nil {
		t.Errorf("standing orders = %+v, want unavailable", so)
	}
}

// ---- total failures: only these fail Compose ----

type failingCaptain struct{}

func (failingCaptain) Read(context.Context, *uuid.UUID, string) (*captain.Snapshot, error) {
	return nil, errors.New("captain read failed")
}

// failingDB makes the digest store's chain-head read fail.
type failingDB struct{}

func (failingDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("db down")
}
func (failingDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("db down")
}
func (failingDB) QueryRow(context.Context, string, ...any) pgx.Row { return errRow{} }

type errRow struct{}

func (errRow) Scan(...any) error { return errors.New("db down") }

// TestCompose_UnavailableOnlyWhenBriefCannotBeEstablished (condition 2): a
// captain record or window read failure is the ONLY I/O failure of Compose,
// named by reason.
func TestCompose_UnavailableOnlyWhenBriefCannotBeEstablished(t *testing.T) {
	f := newFixture(t)
	cases := map[string]Deps{
		ReasonCaptainRecordReadFail:  {Digest: digest.Deps{Store: f.digest, Index: f.idx}, Captain: failingCaptain{}},
		ReasonWindowReadFailed:       {Digest: digest.Deps{Store: digest.NewStore(failingDB{}), Index: f.idx}, Captain: f.captain},
		ReasonDependencyUnconfigured: {Captain: f.captain},
	}
	for reason, deps := range cases {
		_, err := Compose(context.Background(), deps, Request{Repo: testRepo})
		if !errors.Is(err, ErrUnavailable) || UnavailableReason(err) != reason {
			t.Errorf("%s: err = %v (reason %q), want ErrUnavailable with that reason", reason, err, UnavailableReason(err))
		}
	}
	if UnavailableReason(errors.New("x")) != "" {
		t.Error("non-unavailable error has a reason")
	}
}

// ---- hash ----

// TestBriefHash_IsStableAcrossComposures: the same content composed twice
// hashes identically, and a PRE-SET BriefHash (plus a selector and truncation
// markers) on the input does not move the hash.
func TestBriefHash_IsStableAcrossComposures(t *testing.T) {
	f := newFixture(t)
	f.merge(t)
	a := compose(t, f.deps(), Request{})
	b := compose(t, f.deps(), Request{})
	if a.BriefHash != b.BriefHash {
		t.Fatalf("hashes differ across composures: %s vs %s", a.BriefHash, b.BriefHash)
	}
	dirty := b
	dirty.BriefHash = "not-a-hash"
	dirty.Section = SectionWhatChanged
	dirty.Truncated = true
	if Hash(dirty) != a.BriefHash {
		t.Error("hash depends on BriefHash / selector / truncation markers")
	}
}

// TestBriefHash_ChangesOnContent: different content, different hash — pairs
// with the stability test so a constant hash fails the pair.
func TestBriefHash_ChangesOnContent(t *testing.T) {
	f := newFixture(t)
	a := compose(t, f.deps(), Request{})
	f.merge(t)
	b := compose(t, f.deps(), Request{})
	if a.BriefHash == b.BriefHash {
		t.Error("hash unchanged after a merge entered what_changed")
	}
}

// TestBriefHash_SurvivesSelectAndBound (condition 1): a section-selected and a
// bounded render report the canonical hash, never a re-hash of their body.
func TestBriefHash_SurvivesSelectAndBound(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 5; i++ {
		f.merge(t)
	}
	b := compose(t, f.deps(), Request{})
	sel, err := Select(b, SectionWhatChanged)
	if err != nil {
		t.Fatal(err)
	}
	if sel.BriefHash != b.BriefHash || len(sel.Sections) != 1 {
		t.Errorf("select = %d sections hash %s, want 1 section hash %s", len(sel.Sections), sel.BriefHash, b.BriefHash)
	}
	raw, err := json.Marshal(sel)
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := Bound(sel, len(raw)-200)
	if err != nil {
		t.Fatal(err)
	}
	if !bounded.Truncated {
		t.Fatal("fixture did not force truncation; the hash-preservation arm is vacuous")
	}
	if bounded.BriefHash != b.BriefHash {
		t.Errorf("bounded hash %s, want canonical %s", bounded.BriefHash, b.BriefHash)
	}
	if _, err := Select(b, "nope"); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("unknown section err = %v", err)
	}
}
