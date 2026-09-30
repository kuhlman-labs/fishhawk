package delegationconfirm

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
)

const repo = "acme/app"

func capEntry(seq int64, cat, subject string) ChainEntry {
	raw, _ := json.Marshal(map[string]any{"repo": repo, "subject": subject})
	return ChainEntry{Sequence: seq, EntryHash: "h" + cat, Category: cat, Payload: raw}
}

func confEntry(seq int64, cat, subject, workflow, hash string) ChainEntry {
	raw, _ := json.Marshal(map[string]any{"repo": repo, "subject": subject, "workflow": workflow, "content_hash": hash})
	return ChainEntry{Sequence: seq, EntryHash: "c", Category: cat, Payload: raw}
}

func statusOf(t *testing.T, st State, wf string) WorkflowStatus {
	t.Helper()
	return Statuses(st, []string{wf})[0]
}

// TestDerive_HandoverInvalidatesEarlierConfirmation (C6): a confirmation at
// sequence 5 followed by a captain_assigned at 9 is VOID.
func TestDerive_HandoverInvalidatesEarlierConfirmation(t *testing.T) {
	st := Derive(repo,
		[]ChainEntry{capEntry(1, captain.CategoryAssigned, "github:old"), capEntry(9, captain.CategoryAssigned, "github:new")},
		[]ChainEntry{confEntry(5, CategoryDelegationConfirmed, "github:old", "feature_change", "h1")})
	ws := statusOf(t, st, "feature_change")
	if ws.Status != StatusUnconfirmed || ws.Reason != ReasonHandover {
		t.Fatalf("status = %+v, want unconfirmed/handover after a later captain_assigned", ws)
	}
	if st.SeatSequence != 9 || st.Captain != "github:new" {
		t.Errorf("seat = %d captain = %q, want 9 github:new", st.SeatSequence, st.Captain)
	}
}

// TestDerive_ConfirmAfterHandoverConfirms: the incoming captain's
// confirmation after the seat change counts.
func TestDerive_ConfirmAfterHandoverConfirms(t *testing.T) {
	st := Derive(repo,
		[]ChainEntry{capEntry(9, captain.CategoryAssigned, "github:new")},
		[]ChainEntry{confEntry(12, CategoryDelegationConfirmed, "github:new", "feature_change", "h1")})
	ws := statusOf(t, st, "feature_change")
	if ws.Status != StatusConfirmed || ws.Confirmation == nil || ws.Confirmation.ContentHash != "h1" {
		t.Fatalf("status = %+v, want confirmed with h1", ws)
	}
}

// TestDerive_OutgoingCaptainConfirmAfterHandoverIgnored (approval condition
// 3): an outgoing captain's confirmation appended AFTER a newer
// captain_assigned leaves the workflow unconfirmed and is counted as ignored.
func TestDerive_OutgoingCaptainConfirmAfterHandoverIgnored(t *testing.T) {
	st := Derive(repo,
		[]ChainEntry{capEntry(1, captain.CategoryAssigned, "github:old"), capEntry(9, captain.CategoryAssigned, "github:new")},
		[]ChainEntry{
			confEntry(10, CategoryDelegationConfirmed, "github:old", "feature_change", "h1"),
			confEntry(11, CategoryDelegationLowerProposed, "github:old", "feature_change", ""),
		})
	ws := statusOf(t, st, "feature_change")
	if ws.Status != StatusUnconfirmed || ws.Reason != ReasonHandover {
		t.Fatalf("status = %+v, want unconfirmed: the outgoing captain's confirm must not count", ws)
	}
	if ws.LowerProposal != nil {
		t.Errorf("an outgoing captain's lower proposal was folded: %+v", ws.LowerProposal)
	}
	if st.IgnoredEntries != 2 {
		t.Errorf("IgnoredEntries = %d, want 2", st.IgnoredEntries)
	}
}

// TestDerive_ClaimResetsAndVacantIgnores: a fallback claim is a seat change
// too, and a confirmation while the seat is vacant never counts.
func TestDerive_ClaimResetsAndVacantIgnores(t *testing.T) {
	st := Derive(repo,
		[]ChainEntry{
			capEntry(1, captain.CategoryAssigned, "github:a"),
			capEntry(3, captain.CategoryRelinquished, "github:a"),
			capEntry(6, captain.CategoryClaimed, "github:b"),
		},
		[]ChainEntry{
			confEntry(2, CategoryDelegationConfirmed, "github:a", "wf", "h"),
			confEntry(4, CategoryDelegationConfirmed, "github:a", "wf", "h"),
		})
	if ws := statusOf(t, st, "wf"); ws.Status != StatusUnconfirmed {
		t.Fatalf("status = %+v, want unconfirmed after the claim", ws)
	}
	if st.IgnoredEntries != 1 {
		t.Errorf("IgnoredEntries = %d, want 1 (the confirm during the vacancy)", st.IgnoredEntries)
	}
}

// TestDerive_SkipsAndCountsMalformed: undecodable, foreign-repo,
// workflow-less and subject-less entries are skipped and COUNTED.
func TestDerive_SkipsAndCountsMalformed(t *testing.T) {
	foreign, _ := json.Marshal(map[string]any{"repo": "other/x", "subject": "github:a", "workflow": "wf", "content_hash": "h"})
	noWF, _ := json.Marshal(map[string]any{"repo": repo, "subject": "github:a", "content_hash": "h"})
	noSubj, _ := json.Marshal(map[string]any{"repo": repo, "workflow": "wf"})
	st := Derive(repo,
		[]ChainEntry{capEntry(1, captain.CategoryAssigned, "github:a")},
		[]ChainEntry{
			{Sequence: 2, Category: CategoryDelegationConfirmed, Payload: json.RawMessage(`{not json`)},
			{Sequence: 3, Category: CategoryDelegationConfirmed, Payload: foreign},
			{Sequence: 4, Category: CategoryDelegationConfirmed, Payload: noWF},
			{Sequence: 5, Category: CategoryDelegationConfirmed, Payload: noSubj},
		})
	if st.SkippedEntries != 4 {
		t.Fatalf("SkippedEntries = %d, want 4", st.SkippedEntries)
	}
	if ws := statusOf(t, st, "wf"); ws.Status != StatusUnconfirmed {
		t.Errorf("status = %+v, want unconfirmed", ws)
	}
}

// TestDerive_WorkflowsIndependentAndLatestWins.
func TestDerive_WorkflowsIndependentAndLatestWins(t *testing.T) {
	st := Derive(repo,
		[]ChainEntry{capEntry(1, captain.CategoryAssigned, "github:a")},
		[]ChainEntry{
			confEntry(3, CategoryDelegationConfirmed, "github:a", "wf1", "old"),
			confEntry(2, CategoryDelegationConfirmed, "github:a", "wf2", "x"),
			confEntry(5, CategoryDelegationConfirmed, "github:a", "wf1", "new"),
		})
	got := Statuses(st, []string{"wf1", "wf2", "wf3"})
	if got[0].Confirmation == nil || got[0].Confirmation.ContentHash != "new" {
		t.Errorf("wf1 = %+v, want latest confirmation (new)", got[0])
	}
	if got[1].Status != StatusConfirmed {
		t.Errorf("wf2 = %+v, want confirmed", got[1])
	}
	if got[2].Status != StatusUnconfirmed || got[2].Reason != ReasonHandover {
		t.Errorf("wf3 (never confirmed) = %+v, want unconfirmed/handover", got[2])
	}
	if u := Unconfirmed(got); !reflect.DeepEqual(u, []string{"wf3"}) {
		t.Errorf("Unconfirmed = %v, want [wf3]", u)
	}
}

// TestStatuses_FirstHandoverListsEveryWorkflowUnconfirmed (approval
// condition 1): with no confirmation entries at all, every workflow of the
// inventory is unconfirmed.
func TestStatuses_FirstHandoverListsEveryWorkflowUnconfirmed(t *testing.T) {
	st := Derive(repo, []ChainEntry{capEntry(4, captain.CategoryAssigned, "github:a")}, nil)
	inv := []string{"bug_fix", "feature_change", "docs"}
	if u := Unconfirmed(Statuses(st, inv)); !reflect.DeepEqual(u, inv) {
		t.Fatalf("Unconfirmed = %v, want every workflow %v", u, inv)
	}
}

// TestStatuses_NoCaptainIsNotUnconfirmed: no seat has ever been taken, so no
// handover awaits confirmation.
func TestStatuses_NoCaptainIsNotUnconfirmed(t *testing.T) {
	got := Statuses(Derive(repo, nil, nil), []string{"wf"})
	if got[0].Status != StatusNoCaptain || len(Unconfirmed(got)) != 0 {
		t.Fatalf("got %+v, want no_captain and nothing unconfirmed", got)
	}
}

// TestApplyCurrentHashes flips a stale confirmation and keeps a current one.
func TestApplyCurrentHashes(t *testing.T) {
	st := Derive(repo,
		[]ChainEntry{capEntry(1, captain.CategoryAssigned, "github:a")},
		[]ChainEntry{
			confEntry(2, CategoryDelegationConfirmed, "github:a", "same", "h1"),
			confEntry(3, CategoryDelegationConfirmed, "github:a", "changed", "h1"),
		})
	got := ApplyCurrentHashes(Statuses(st, []string{"same", "changed"}), map[string]string{"same": "h1", "changed": "h2"})
	if got[0].Status != StatusConfirmed || got[0].CurrentContentHash != "h1" {
		t.Errorf("same = %+v, want confirmed", got[0])
	}
	if got[1].Status != StatusUnconfirmed || got[1].Reason != ReasonHashStale || got[1].CurrentContentHash != "h2" {
		t.Errorf("changed = %+v, want unconfirmed/hash_stale", got[1])
	}
}

// TestValidateLower_TierMatrix is the nine-pair (current x proposed) matrix:
// ONLY strictly-lower pairs are admitted.
func TestValidateLower_TierMatrix(t *testing.T) {
	tiers := []string{"low", "medium", "high"}
	for ci, cur := range tiers {
		for pi, prop := range tiers {
			err := ValidateLower(cur, LowerRequest{ProposedTier: prop, Reason: "r"})
			if pi < ci && err != nil {
				t.Errorf("%s -> %s: %v, want admitted", cur, prop, err)
			}
			if pi >= ci && !errors.Is(err, ErrRaiseRefused) {
				t.Errorf("%s -> %s: %v, want ErrRaiseRefused", cur, prop, err)
			}
		}
	}
}

func TestValidateLower_Refusals(t *testing.T) {
	esc := func(max string, paths ...string) *Escalation { return &Escalation{Paths: paths, MaxAutonomy: max} }
	cases := []struct {
		name string
		cur  string
		req  LowerRequest
		want error
	}{
		{"no reason", "high", LowerRequest{ProposedTier: "low"}, ErrReasonRequired},
		{"nothing proposed", "high", LowerRequest{Reason: "r"}, ErrNothingProposed},
		{"undeclared current tier", "", LowerRequest{ProposedTier: "low", Reason: "r"}, ErrRaiseRefused},
		{"unknown proposed tier", "high", LowerRequest{ProposedTier: "max", Reason: "r"}, ErrRaiseRefused},
		{"escalation ceiling above proposed tier", "high", LowerRequest{ProposedTier: "low", ProposedEscalation: esc("high", "a/**"), Reason: "r"}, ErrRaiseRefused},
		{"escalation-only ceiling equal to current", "medium", LowerRequest{ProposedEscalation: esc("medium", "a/**"), Reason: "r"}, ErrNothingProposed},
		{"escalation without paths", "high", LowerRequest{ProposedEscalation: esc("low"), Reason: "r"}, ErrEscalationPathsRequired},
		{"escalation-only lower ceiling", "high", LowerRequest{ProposedEscalation: esc("medium", "a/**"), Reason: "r"}, nil},
		{"tier + ceiling at proposed tier", "high", LowerRequest{ProposedTier: "medium", ProposedEscalation: esc("medium", "a/**"), Reason: "r"}, nil},
	}
	for _, tc := range cases {
		if err := ValidateLower(tc.cur, tc.req); !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// TestActions_ClosedSet pins the action set: there is no raise action.
func TestActions_ClosedSet(t *testing.T) {
	if got := Actions(); !reflect.DeepEqual(got, []string{"read", "confirm", "lower"}) {
		t.Fatalf("Actions() = %v, want exactly [read confirm lower]", got)
	}
}

// TestCategories_Registered pins the two emitted strings and their
// registration.
func TestCategories_Registered(t *testing.T) {
	if got := Categories(); !reflect.DeepEqual(got, []string{"delegation_confirmed", "delegation_lower_proposed"}) {
		t.Fatalf("Categories() = %v", got)
	}
	for _, c := range Categories() {
		if !audit.IsKnownCategory(c) {
			t.Errorf("%s is not registered in audit.KnownCategories", c)
		}
	}
}

func TestTransitions(t *testing.T) {
	st := Derive(repo, []ChainEntry{capEntry(1, captain.CategoryAssigned, "github:a")}, nil)
	if _, err := Confirm(st, Params{Repo: repo, Workflow: "wf", Actor: "github:b", ContentHash: "h"}); !errors.Is(err, ErrNotCaptain) {
		t.Errorf("non-captain confirm: %v", err)
	}
	if _, err := Confirm(Derive(repo, nil, nil), Params{Repo: repo, Workflow: "wf", Actor: "github:a", ContentHash: "h"}); !errors.Is(err, ErrNoCaptain) {
		t.Errorf("vacant confirm: %v", err)
	}
	if _, err := Confirm(st, Params{Repo: repo, Actor: "github:a", ContentHash: "h"}); !errors.Is(err, ErrWorkflowRequired) {
		t.Errorf("no workflow: %v", err)
	}
	if _, err := Confirm(st, Params{Repo: repo, Workflow: "wf", Actor: "github:a"}); !errors.Is(err, ErrContentHashRequired) {
		t.Errorf("no hash: %v", err)
	}
	ev, err := Confirm(st, Params{Repo: repo, Workflow: "wf", Actor: "github:a", ContentHash: "h", WorkflowSHA: "s"})
	if err != nil || ev.Kind != CategoryDelegationConfirmed || !ev.IdentityVerified {
		t.Fatalf("confirm = %+v, %v", ev, err)
	}
	if _, err := ProposeLower(st, Params{Repo: repo, Workflow: "wf", Actor: "github:a"}); !errors.Is(err, ErrFiledRefRequired) {
		t.Errorf("no filed ref: %v", err)
	}
	if _, err := ProposeLower(st, Params{Repo: repo, Workflow: "wf", Actor: "github:b", FiledRef: "u"}); !errors.Is(err, ErrNotCaptain) {
		t.Errorf("non-captain lower: %v", err)
	}
	if _, err := ProposeLower(st, Params{Repo: repo, Actor: "github:a", FiledRef: "u"}); !errors.Is(err, ErrWorkflowRequired) {
		t.Errorf("lower no workflow: %v", err)
	}
	lev, err := ProposeLower(st, Params{Repo: repo, Workflow: "wf", Actor: "github:a", FiledRef: "u", Lower: LowerRequest{ProposedTier: "low", Reason: "r"}})
	if err != nil || lev.Kind != CategoryDelegationLowerProposed {
		t.Fatalf("lower = %+v, %v", lev, err)
	}
	for _, e := range []Event{ev, lev} {
		raw, err := e.Payload()
		if err != nil {
			t.Fatalf("payload: %v", err)
		}
		back := Derive(repo, []ChainEntry{capEntry(1, captain.CategoryAssigned, "github:a")},
			[]ChainEntry{{Sequence: 2, Category: e.Kind, Payload: raw}})
		if back.SkippedEntries != 0 || back.IgnoredEntries != 0 {
			t.Errorf("%s payload did not round-trip: %+v", e.Kind, back)
		}
	}
	if _, err := (Event{Kind: "captain_assigned", Repo: repo, Workflow: "w", Subject: "s"}).Payload(); err == nil {
		t.Error("a foreign kind produced a payload")
	}
	if _, err := (Event{Kind: CategoryDelegationConfirmed, Repo: repo, Subject: "s"}).Payload(); err == nil {
		t.Error("a workflow-less event produced a payload")
	}
}

func TestGuardActor(t *testing.T) {
	if err := GuardActor(Params{}); !errors.Is(err, ErrActorRequired) {
		t.Errorf("empty actor: %v", err)
	}
	if err := GuardActor(Params{Actor: "a", ActorIsAgent: true}); !errors.Is(err, ErrAgentIdentity) {
		t.Errorf("agent: %v", err)
	}
	if err := GuardActor(Params{Actor: "a", ActorIsDelegated: true}); !errors.Is(err, ErrAgentIdentity) {
		t.Errorf("delegated: %v", err)
	}
	if err := GuardActor(Params{Actor: "a"}); err != nil {
		t.Errorf("operator: %v", err)
	}
}
