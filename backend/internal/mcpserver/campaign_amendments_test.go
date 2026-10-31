package mcpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// campaignAmendmentEntry builds one campaign-pending-amendment entry filed by
// filingRun's stage for campaign item issueRef.
func campaignAmendmentEntry(filingRun, stageID uuid.UUID, issueRef string, live bool) campaignPendingAmendment {
	state := "running"
	if !live {
		state = "failed"
	}
	return campaignPendingAmendment{
		IssueRef:     issueRef,
		ItemRunID:    filingRun.String(),
		StageState:   state,
		DecisionLive: live,
		Amendment: ScopeAmendmentItem{
			ID:      uuid.NewString(),
			RunID:   filingRun.String(),
			StageID: stageID.String(),
			Paths:   []ScopeAmendmentPath{{Path: "pkg/widget_test.go", Operation: "create"}},
			Reason:  "the coupled test must be created",
			Status:  "pending",
		},
	}
}

// seedCampaignAmendments answers the anchor's campaign-pending-amendments read
// with the given campaign and entries.
func seedCampaignAmendments(fb *fakeBackend, anchor uuid.UUID, campaignID string, items ...campaignPendingAmendment) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	cid := campaignID
	fb.campaignAmendmentsByAnchor[anchor] = runCampaignPendingAmendments{
		RunID: anchor.String(), CampaignID: &cid, Items: items,
	}
}

func campaignAmendmentReads(fb *fakeBackend, anchor uuid.UUID) int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.campaignAmendmentsReads[anchor]
}

// TestCampaignLiveAmendment_SkipsNotLive is counterfactual (4)'s vehicle: the
// route returns ONE entry, on a settled stage (decision_live false), so only the
// live filter stands between it and a returned match.
func TestCampaignLiveAmendment_SkipsNotLive(t *testing.T) {
	fb, srv := newFakeBackend(t)
	anchor, other := uuid.New(), uuid.New()
	seedCampaignAmendments(fb, anchor, uuid.NewString(), campaignAmendmentEntry(other, uuid.New(), "acme/widgets#2", false))
	r := newResolver(srv, nil)

	e, cid := r.campaignLiveAmendment(context.Background(), anchor, nil)
	if e != nil || cid != "" {
		t.Fatalf("campaignLiveAmendment = (%+v, %q), want (nil, \"\") — a not-live entry must never be returned", e, cid)
	}
	if got := campaignAmendmentReads(fb, anchor); got != 1 {
		t.Errorf("campaign reads = %d, want 1 (the probe ran)", got)
	}
}

// TestCampaignLiveAmendment_HonorsSkip: the skip predicate excludes an entry,
// and the next live survivor is returned.
func TestCampaignLiveAmendment_HonorsSkip(t *testing.T) {
	fb, srv := newFakeBackend(t)
	anchor, own, other := uuid.New(), uuid.New(), uuid.New()
	campaignID := uuid.NewString()
	skipped := campaignAmendmentEntry(own, uuid.New(), "acme/widgets#1", true)
	want := campaignAmendmentEntry(other, uuid.New(), "acme/widgets#2", true)
	seedCampaignAmendments(fb, anchor, campaignID, skipped, want)
	r := newResolver(srv, nil)

	e, cid := r.campaignLiveAmendment(context.Background(), anchor, func(c *campaignPendingAmendment) bool {
		return c.Amendment.RunID == own.String()
	})
	if e == nil || e.Amendment.ID != want.Amendment.ID {
		t.Fatalf("campaignLiveAmendment = %+v, want the unskipped entry %s", e, want.Amendment.ID)
	}
	if cid != campaignID {
		t.Errorf("campaign id = %q, want %s", cid, campaignID)
	}

	// Skip everything: nothing survives.
	e, cid = r.campaignLiveAmendment(context.Background(), anchor, func(*campaignPendingAmendment) bool { return true })
	if e != nil || cid != "" {
		t.Errorf("campaignLiveAmendment with everything skipped = (%+v, %q), want (nil, \"\")", e, cid)
	}
}

// TestCampaignLiveAmendment_FirstLiveInServerOrder: the server's oldest-first
// order is kept — the first LIVE entry wins, not a later one.
func TestCampaignLiveAmendment_FirstLiveInServerOrder(t *testing.T) {
	fb, srv := newFakeBackend(t)
	anchor := uuid.New()
	notLive := campaignAmendmentEntry(uuid.New(), uuid.New(), "acme/widgets#1", false)
	first := campaignAmendmentEntry(uuid.New(), uuid.New(), "acme/widgets#2", true)
	second := campaignAmendmentEntry(uuid.New(), uuid.New(), "acme/widgets#3", true)
	seedCampaignAmendments(fb, anchor, uuid.NewString(), notLive, first, second)
	r := newResolver(srv, nil)

	e, _ := r.campaignLiveAmendment(context.Background(), anchor, nil)
	if e == nil || e.Amendment.ID != first.Amendment.ID {
		t.Fatalf("campaignLiveAmendment = %+v, want the FIRST live entry %s (server order is oldest-first)", e, first.Amendment.ID)
	}
	if e.IssueRef != "acme/widgets#2" || e.StageState != "running" {
		t.Errorf("entry issue_ref/stage_state = %q/%q, want acme/widgets#2/running decoded from the wire", e.IssueRef, e.StageState)
	}
}

// TestCampaignLiveAmendment_NoCampaignReturnsNil: an anchor in no campaign
// (campaign_id null, items []) yields nothing — and so does a null campaign
// carrying a stray live entry, because campaign_id is the authority.
func TestCampaignLiveAmendment_NoCampaignReturnsNil(t *testing.T) {
	fb, srv := newFakeBackend(t)
	anchor := uuid.New()
	fb.mu.Lock()
	fb.campaignAmendmentsByAnchor[anchor] = runCampaignPendingAmendments{
		RunID: anchor.String(), CampaignID: nil,
		Items: []campaignPendingAmendment{campaignAmendmentEntry(uuid.New(), uuid.New(), "acme/widgets#2", true)},
	}
	fb.mu.Unlock()
	r := newResolver(srv, nil)

	if e, cid := r.campaignLiveAmendment(context.Background(), anchor, nil); e != nil || cid != "" {
		t.Fatalf("campaignLiveAmendment = (%+v, %q), want (nil, \"\") for a run in no campaign", e, cid)
	}
}

// TestCampaignLiveAmendment_ErrorReturnsNil: every read failure is best-effort
// nil — a 500, a 403 (a token without read:audit), and the unrouted 404 of an
// older backend.
func TestCampaignLiveAmendment_ErrorReturnsNil(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		seed   bool
	}{
		{"server_error", http.StatusInternalServerError, true},
		{"forbidden", http.StatusForbidden, true},
		{"older_backend_unrouted", http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb, srv := newFakeBackend(t)
			anchor := uuid.New()
			if tc.seed {
				seedCampaignAmendments(fb, anchor, uuid.NewString(), campaignAmendmentEntry(uuid.New(), uuid.New(), "acme/widgets#2", true))
			}
			fb.campaignAmendmentsStatus = tc.status
			r := newResolver(srv, nil)

			if e, cid := r.campaignLiveAmendment(context.Background(), anchor, nil); e != nil || cid != "" {
				t.Fatalf("campaignLiveAmendment = (%+v, %q), want (nil, \"\") on a failed read", e, cid)
			}
			if got := campaignAmendmentReads(fb, anchor); got != 1 {
				t.Errorf("campaign reads = %d, want 1 (the read was attempted)", got)
			}
		})
	}
}

// TestCampaignAmendmentNextStep_NamesFilingRun: next_step decides the
// amendment on the FILING run, never the awaited anchor.
func TestCampaignAmendmentNextStep_NamesFilingRun(t *testing.T) {
	fb, srv := newFakeBackend(t)
	anchor, filing := uuid.New(), uuid.New()
	campaignID := uuid.NewString()
	entry := campaignAmendmentEntry(filing, uuid.New(), "acme/widgets#2", true)
	seedCampaignAmendments(fb, anchor, campaignID, entry)
	r := newResolver(srv, nil)

	e, cid := r.campaignLiveAmendment(context.Background(), anchor, nil)
	if e == nil {
		t.Fatal("campaignLiveAmendment = nil, want the live entry")
	}
	ns := campaignAmendmentNextStep(e)
	if ns.Action != "fishhawk_decide_scope_amendment" {
		t.Errorf("action = %q, want fishhawk_decide_scope_amendment", ns.Action)
	}
	if ns.Params["run_id"] != filing.String() {
		t.Errorf("next_step run_id = %q, want the FILING run %s (not the anchor %s)", ns.Params["run_id"], filing, anchor)
	}
	if ns.Params["amendment_id"] != entry.Amendment.ID {
		t.Errorf("next_step amendment_id = %q, want %s", ns.Params["amendment_id"], entry.Amendment.ID)
	}
	if !strings.Contains(ns.Reason, amendmentPollWindowText()) {
		t.Errorf("reason = %q, want the poll window %q", ns.Reason, amendmentPollWindowText())
	}
	msg := campaignAmendmentMessage(e, cid, "the awaited stage")
	for _, want := range []string{filing.String(), "acme/widgets#2", campaignID, entry.Amendment.ID, "pkg/widget_test.go (create)", "the coupled test must be created", "has NOT released for its own reasons", "re-arm"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message must carry %q; got %q", want, msg)
		}
	}
}
