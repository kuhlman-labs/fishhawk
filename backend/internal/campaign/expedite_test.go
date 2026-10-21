package campaign

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

var expediteNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func expediteAppend(e *ExpediteParams, n int) AppendCampaignItemsParams {
	p := AppendCampaignItemsParams{CampaignID: uuid.New(), Expedite: e}
	for i := 0; i < n; i++ {
		p.NewItems = append(p.NewItems, CreateCampaignItemParams{IssueRef: issueRef(100 + i)})
	}
	return p
}

func TestValidateExpedite(t *testing.T) {
	ok := func() *ExpediteParams {
		return &ExpediteParams{Kind: ExpediteKindHotfix, Workflow: "hotfix_change", Reason: "prod down"}
	}
	hold := func(d time.Duration) *DispatchHoldParams {
		return &DispatchHoldParams{HeldAt: expediteNow, ExpiresAt: expediteNow.Add(d)}
	}
	withHold := func(h *DispatchHoldParams) AppendCampaignItemsParams {
		e := ok()
		e.Hold = h
		return expediteAppend(e, 1)
	}
	cases := []struct {
		name  string
		p     AppendCampaignItemsParams
		valid bool
	}{
		{"nil expedite is an ordinary append", expediteAppend(nil, 3), true},
		{"one item, no hold", expediteAppend(ok(), 1), true},
		{"empty reason is the verb's rule, not this one", expediteAppend(&ExpediteParams{Kind: ExpediteKindHotfix, Workflow: "w"}, 1), true},
		{"zero items", expediteAppend(ok(), 0), false},
		{"two items", expediteAppend(ok(), 2), false},
		{"unknown kind", expediteAppend(&ExpediteParams{Kind: "urgent", Workflow: "w"}, 1), false},
		{"empty kind", expediteAppend(&ExpediteParams{Workflow: "w"}, 1), false},
		{"empty workflow", expediteAppend(&ExpediteParams{Kind: ExpediteKindHotfix}, 1), false},
		{"blank workflow", expediteAppend(&ExpediteParams{Kind: ExpediteKindHotfix, Workflow: "  "}, 1), false},
		{"hold at the cap", withHold(hold(MaxDispatchHold)), true},
		{"hold one second", withHold(hold(time.Second)), true},
		{"hold past the cap", withHold(hold(MaxDispatchHold + time.Nanosecond)), false},
		{"expires == held", withHold(hold(0)), false},
		{"expires before held", withHold(hold(-time.Minute)), false},
		{"zero held_at", withHold(&DispatchHoldParams{ExpiresAt: expediteNow}), false},
		{"addition alongside an expedited item", func() AppendCampaignItemsParams {
			p := expediteAppend(ok(), 1)
			p.AddDependencies = map[uuid.UUID][]string{uuid.New(): {"issue:100"}}
			return p
		}(), false},
	}
	for _, tc := range cases {
		err := ValidateExpedite(tc.p)
		if tc.valid && err != nil {
			t.Errorf("%s: err = %v, want nil", tc.name, err)
		}
		if !tc.valid && !errors.Is(err, ErrExpediteInvalid) {
			t.Errorf("%s: err = %v, want ErrExpediteInvalid", tc.name, err)
		}
	}
}

func TestHoldReleaseReason(t *testing.T) {
	h := &DispatchHold{HeldAt: expediteNow, ExpiresAt: expediteNow.Add(MaxDispatchHold)}
	cases := []struct {
		name       string
		hold       *DispatchHold
		terminal   bool
		now        time.Time
		wantReason string
		wantDue    bool
	}{
		{"before expiry, live hotfix", h, false, expediteNow.Add(time.Hour), "", false},
		{"one ns before expiry", h, false, h.ExpiresAt.Add(-time.Nanosecond), "", false},
		{"exactly at expiry is timeout", h, false, h.ExpiresAt, HoldReleasedTimeout, true},
		{"past expiry is timeout", h, false, h.ExpiresAt.Add(time.Second), HoldReleasedTimeout, true},
		{"terminal before expiry", h, true, expediteNow, HoldReleasedHotfixTerminal, true},
		{"terminal wins over timeout", h, true, h.ExpiresAt.Add(time.Hour), HoldReleasedHotfixTerminal, true},
		{"nil hold is never due", nil, true, h.ExpiresAt.Add(time.Hour), "", false},
	}
	for _, tc := range cases {
		reason, due := HoldReleaseReason(tc.hold, tc.terminal, tc.now)
		if reason != tc.wantReason || due != tc.wantDue {
			t.Errorf("%s: HoldReleaseReason = (%q, %v), want (%q, %v)", tc.name, reason, due, tc.wantReason, tc.wantDue)
		}
	}
}

func TestValidateReleaseDispatchHold(t *testing.T) {
	rec := func(context.Context, DispatchHold) error { return nil }
	for _, reason := range []string{HoldReleasedHotfixTerminal, HoldReleasedTimeout} {
		if err := ValidateReleaseDispatchHold(ReleaseDispatchHoldParams{Reason: reason, Record: rec}); err != nil {
			t.Errorf("reason %q: err = %v, want nil", reason, err)
		}
	}
	for _, reason := range []string{"", "operator", "Timeout"} {
		if err := ValidateReleaseDispatchHold(ReleaseDispatchHoldParams{Reason: reason, Record: rec}); !errors.Is(err, ErrExpediteInvalid) {
			t.Errorf("reason %q: err = %v, want ErrExpediteInvalid", reason, err)
		}
	}
	if err := ValidateReleaseDispatchHold(ReleaseDispatchHoldParams{Reason: HoldReleasedTimeout}); !errors.Is(err, ErrHoldReleaseUnrecorded) {
		t.Errorf("nil Record: err = %v, want ErrHoldReleaseUnrecorded", err)
	}
}

func TestExpeditedHeadPosition(t *testing.T) {
	items := func(pos ...int) []*Item {
		out := make([]*Item, 0, len(pos))
		for _, p := range pos {
			out = append(out, &Item{Position: p})
		}
		return out
	}
	cases := []struct {
		name string
		in   []*Item
		want int
	}{
		{"empty roster", nil, 0},
		{"dense from zero", items(0, 1, 2), -1},
		{"unsorted", items(3, 1, 2), 0},
		{"after an earlier expedite", items(-1, 0, 1), -2},
		{"min not first", items(5, -3, 4), -4},
	}
	for _, tc := range cases {
		if got := ExpeditedHeadPosition(tc.in); got != tc.want {
			t.Errorf("%s: ExpeditedHeadPosition = %d, want %d", tc.name, got, tc.want)
		}
	}
}
