package refinement

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// storedRev builds a StoredDraft revision wrapping draft under a fresh id at an
// ascending created_at, for composing ResolveState / ApprovedDraft tables.
func storedRev(t *testing.T, draft EpicDraft, order int) *StoredDraft {
	t.Helper()
	return &StoredDraft{
		ID:        uuid.New(),
		SessionID: uuid.Nil,
		Draft:     draft,
		Origin:    OriginBrief,
		CreatedAt: time.Unix(int64(order), 0).UTC(),
	}
}

// decisionOn builds a decision pinning draftID with the recomputed hash of
// draft (so it counts) unless overrideHash is non-empty.
func decisionOn(t *testing.T, draftID uuid.UUID, verdict string, draft EpicDraft, overrideHash string) *Decision {
	t.Helper()
	hash := overrideHash
	if hash == "" {
		h, err := ContentHash(draft)
		if err != nil {
			t.Fatalf("ContentHash: %v", err)
		}
		hash = h
	}
	return &Decision{
		ID:               uuid.New(),
		DraftID:          draftID,
		Decision:         verdict,
		Reason:           "because",
		DraftContentHash: hash,
	}
}

func TestContentHash_DeterministicAndDistinct(t *testing.T) {
	a := validDraft()
	b := validDraft()
	ha, err := ContentHash(a)
	if err != nil {
		t.Fatalf("ContentHash a: %v", err)
	}
	hb, err := ContentHash(b)
	if err != nil {
		t.Fatalf("ContentHash b: %v", err)
	}
	if ha != hb {
		t.Errorf("identical drafts hashed differently: %s != %s", ha, hb)
	}
	// A field change moves the hash.
	b.Epic.Summary = "a different summary"
	hb2, err := ContentHash(b)
	if err != nil {
		t.Fatalf("ContentHash b2: %v", err)
	}
	if ha == hb2 {
		t.Error("distinct drafts hashed identically")
	}
}

func TestResolveState_Table(t *testing.T) {
	latest := validDraft()

	t.Run("no decisions -> awaiting_approval", func(t *testing.T) {
		rev := storedRev(t, latest, 1)
		res, err := ResolveState([]*StoredDraft{rev}, nil)
		if err != nil {
			t.Fatalf("ResolveState: %v", err)
		}
		if res.State != StateAwaitingApproval || res.Drifted || res.Decision != nil {
			t.Errorf("res = %+v, want awaiting_approval, no drift, no decision", res)
		}
		if res.LatestDraftID != rev.ID {
			t.Errorf("LatestDraftID = %s, want %s", res.LatestDraftID, rev.ID)
		}
	})

	t.Run("approved on latest -> approved", func(t *testing.T) {
		rev := storedRev(t, latest, 1)
		dec := decisionOn(t, rev.ID, DecisionApproved, latest, "")
		res, err := ResolveState([]*StoredDraft{rev}, []*Decision{dec})
		if err != nil {
			t.Fatalf("ResolveState: %v", err)
		}
		if res.State != StateApproved || res.Drifted || res.Decision == nil {
			t.Errorf("res = %+v, want approved with decision, no drift", res)
		}
	})

	t.Run("rejected on latest -> rejected", func(t *testing.T) {
		rev := storedRev(t, latest, 1)
		dec := decisionOn(t, rev.ID, DecisionRejected, latest, "")
		res, err := ResolveState([]*StoredDraft{rev}, []*Decision{dec})
		if err != nil {
			t.Fatalf("ResolveState: %v", err)
		}
		if res.State != StateRejected {
			t.Errorf("state = %s, want rejected", res.State)
		}
	})

	t.Run("decision on superseded revision -> awaiting_approval", func(t *testing.T) {
		rev1 := storedRev(t, latest, 1)
		// A newer revision supersedes rev1; the approval on rev1 no longer counts.
		edited := validDraft()
		edited.Epic.Summary = "edited"
		rev2 := storedRev(t, edited, 2)
		dec := decisionOn(t, rev1.ID, DecisionApproved, latest, "")
		res, err := ResolveState([]*StoredDraft{rev1, rev2}, []*Decision{dec})
		if err != nil {
			t.Fatalf("ResolveState: %v", err)
		}
		if res.State != StateAwaitingApproval || res.Decision != nil {
			t.Errorf("res = %+v, want awaiting_approval with no decision on latest", res)
		}
	})

	t.Run("hash mismatch on latest -> awaiting_approval + drifted", func(t *testing.T) {
		rev := storedRev(t, latest, 1)
		dec := decisionOn(t, rev.ID, DecisionApproved, latest, "deadbeef-not-the-real-hash")
		res, err := ResolveState([]*StoredDraft{rev}, []*Decision{dec})
		if err != nil {
			t.Fatalf("ResolveState: %v", err)
		}
		if res.State != StateAwaitingApproval || !res.Drifted {
			t.Errorf("res = %+v, want awaiting_approval + drifted", res)
		}
	})

	t.Run("empty drafts -> error", func(t *testing.T) {
		if _, err := ResolveState(nil, nil); err == nil {
			t.Fatal("ResolveState on empty drafts accepted; want error")
		}
	})
}

func TestApprovedDraft_Sentinels(t *testing.T) {
	latest := validDraft()

	t.Run("approved+matching -> returns latest", func(t *testing.T) {
		rev := storedRev(t, latest, 1)
		dec := decisionOn(t, rev.ID, DecisionApproved, latest, "")
		got, err := ApprovedDraft([]*StoredDraft{rev}, []*Decision{dec})
		if err != nil {
			t.Fatalf("ApprovedDraft: %v", err)
		}
		if got.ID != rev.ID {
			t.Errorf("returned draft %s, want latest %s", got.ID, rev.ID)
		}
	})

	t.Run("no decision -> ErrNotApproved", func(t *testing.T) {
		rev := storedRev(t, latest, 1)
		_, err := ApprovedDraft([]*StoredDraft{rev}, nil)
		if !errors.Is(err, ErrNotApproved) {
			t.Fatalf("err = %v, want ErrNotApproved", err)
		}
	})

	t.Run("rejected latest -> ErrNotApproved", func(t *testing.T) {
		rev := storedRev(t, latest, 1)
		dec := decisionOn(t, rev.ID, DecisionRejected, latest, "")
		_, err := ApprovedDraft([]*StoredDraft{rev}, []*Decision{dec})
		if !errors.Is(err, ErrNotApproved) {
			t.Fatalf("err = %v, want ErrNotApproved for a rejected latest revision", err)
		}
	})

	t.Run("approval on superseded revision -> ErrNotApproved", func(t *testing.T) {
		rev1 := storedRev(t, latest, 1)
		edited := validDraft()
		edited.Epic.Summary = "edited"
		rev2 := storedRev(t, edited, 2)
		dec := decisionOn(t, rev1.ID, DecisionApproved, latest, "")
		_, err := ApprovedDraft([]*StoredDraft{rev1, rev2}, []*Decision{dec})
		if !errors.Is(err, ErrNotApproved) {
			t.Fatalf("err = %v, want ErrNotApproved (approval targets a superseded revision)", err)
		}
	})

	t.Run("drifted hash -> ErrDraftDrifted", func(t *testing.T) {
		rev := storedRev(t, latest, 1)
		dec := decisionOn(t, rev.ID, DecisionApproved, latest, "deadbeef-not-the-real-hash")
		_, err := ApprovedDraft([]*StoredDraft{rev}, []*Decision{dec})
		if !errors.Is(err, ErrDraftDrifted) {
			t.Fatalf("err = %v, want ErrDraftDrifted", err)
		}
	})

	t.Run("empty drafts -> ErrNotApproved", func(t *testing.T) {
		_, err := ApprovedDraft(nil, nil)
		if !errors.Is(err, ErrNotApproved) {
			t.Fatalf("err = %v, want ErrNotApproved", err)
		}
	})
}
