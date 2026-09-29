package captain

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

const testRepo = "kuhlman-labs/fishhawk"

// chain builds hand-made chain entries with ascending sequences and
// deterministic hashes ("h<seq>").
type chain struct {
	entries []ChainEntry
	seq     int64
}

func (c *chain) add(category string, p map[string]any) string {
	c.seq++
	if _, ok := p["repo"]; !ok {
		p["repo"] = testRepo
	}
	b, _ := json.Marshal(p)
	hash := fmt.Sprintf("h%d", c.seq)
	c.entries = append(c.entries, ChainEntry{
		Sequence:  c.seq,
		EntryHash: hash,
		Category:  category,
		Timestamp: time.Unix(1_700_000_000+c.seq, 0).UTC(),
		Payload:   b,
	})
	return hash
}

func (c *chain) raw(category string, payload string) {
	c.seq++
	c.entries = append(c.entries, ChainEntry{Sequence: c.seq, EntryHash: fmt.Sprintf("h%d", c.seq), Category: category, Payload: json.RawMessage(payload)})
}

func TestDerive_EmptyIsVacant(t *testing.T) {
	st := Derive(testRepo, nil)
	if st.Current != nil || st.PendingOffer != nil || st.LastCaptain != nil || st.SkippedEntries != 0 {
		t.Fatalf("empty chain derived %+v, want zero state", st)
	}
}

func TestDerive_OfferedSetsPendingOfferOnly(t *testing.T) {
	var c chain
	c.add(CategoryAssigned, map[string]any{"subject": "github:alice", "identity_verified": true})
	h := c.add(CategoryHandoverOffered, map[string]any{"subject": "github:alice", "successor": "github:carol", "successor_identity_verified": true})
	st := Derive(testRepo, c.entries)
	if st.PendingOffer == nil {
		t.Fatal("no pending offer")
	}
	o := st.PendingOffer
	if o.Successor != "github:carol" || o.OfferedBy != "github:alice" || o.EntryHash != h || o.Sequence != 2 || !o.SuccessorIdentityVerified {
		t.Errorf("pending offer = %+v", o)
	}
	if st.Current == nil || st.Current.Subject != "github:alice" {
		t.Errorf("offer must not move the seat; current = %+v", st.Current)
	}
}

func TestDerive_OfferedOnVacantSeatHasNoCaptain(t *testing.T) {
	var c chain
	c.add(CategoryHandoverOffered, map[string]any{"subject": "github:alice", "successor": "github:carol"})
	st := Derive(testRepo, c.entries)
	if st.Current != nil || st.PendingOffer == nil {
		t.Fatalf("offered-only chain derived %+v, want pending offer and no captain", st)
	}
}

func TestDerive_OfferedThenWithdrawnIsCleared(t *testing.T) {
	var c chain
	c.add(CategoryAssigned, map[string]any{"subject": "github:alice"})
	h := c.add(CategoryHandoverOffered, map[string]any{"subject": "github:alice", "successor": "github:carol"})
	c.add(CategoryHandoverWithdrawn, map[string]any{"subject": "github:alice", "offer_entry_hash": h})
	st := Derive(testRepo, c.entries)
	if st.PendingOffer != nil {
		t.Errorf("withdrawn offer still pending: %+v", st.PendingOffer)
	}
	if st.Current == nil || st.Current.Subject != "github:alice" {
		t.Errorf("withdraw moved the seat: %+v", st.Current)
	}
}

// A withdraw naming an offer that is not the pending one is SKIPPED and
// counted, never guessed at — the pending offer survives.
func TestDerive_WithdrawOfOtherOfferIsSkipped(t *testing.T) {
	var c chain
	c.add(CategoryAssigned, map[string]any{"subject": "github:alice"})
	c.add(CategoryHandoverOffered, map[string]any{"subject": "github:alice", "successor": "github:carol"})
	c.add(CategoryHandoverWithdrawn, map[string]any{"subject": "github:alice", "offer_entry_hash": "not-the-offer"})
	st := Derive(testRepo, c.entries)
	if st.PendingOffer == nil || st.SkippedEntries != 1 {
		t.Errorf("mismatched withdraw: offer=%+v skipped=%d, want offer kept and 1 skipped", st.PendingOffer, st.SkippedEntries)
	}
}

func TestDerive_OfferedThenAssignedMovesSeat(t *testing.T) {
	var c chain
	c.add(CategoryAssigned, map[string]any{"subject": "github:alice", "identity_verified": true})
	h := c.add(CategoryHandoverOffered, map[string]any{"subject": "github:alice", "successor": "carol@local-mcp"})
	a := c.add(CategoryAssigned, map[string]any{"subject": "carol@local-mcp", "identity_verified": false, "offer_entry_hash": h, "previous_captain": "github:alice"})
	st := Derive(testRepo, c.entries)
	if st.Current == nil {
		t.Fatal("no captain after assignment")
	}
	cur := st.Current
	if cur.Subject != "carol@local-mcp" || cur.Basis != BasisAssigned || cur.IdentityVerified || cur.ClaimVerified != nil || cur.AssignedEntryHash != a || cur.AssignedSequence != 3 {
		t.Errorf("current = %+v", cur)
	}
	if st.PendingOffer != nil {
		t.Errorf("assignment must clear the offer: %+v", st.PendingOffer)
	}
	if st.LastCaptain == nil || *st.LastCaptain != "github:alice" {
		t.Errorf("LastCaptain = %v, want github:alice (the outgoing captain)", st.LastCaptain)
	}
}

// TestDerive_RelinquishSetsLastCaptain is the pure-fold vehicle for the
// previous_captain derivation on the relinquish-then-claim path (C14).
func TestDerive_RelinquishSetsLastCaptain(t *testing.T) {
	var c chain
	c.add(CategoryAssigned, map[string]any{"subject": "github:alice"})
	c.add(CategoryHandoverOffered, map[string]any{"subject": "github:alice", "successor": "github:carol"})
	c.add(CategoryRelinquished, map[string]any{"subject": "github:alice"})
	st := Derive(testRepo, c.entries)
	if st.Current != nil {
		t.Errorf("relinquished seat still held: %+v", st.Current)
	}
	if st.PendingOffer != nil {
		t.Errorf("relinquish must clear the pending offer: %+v", st.PendingOffer)
	}
	if st.LastCaptain == nil || *st.LastCaptain != "github:alice" {
		t.Fatalf("LastCaptain = %v, want github:alice (the relinquisher)", st.LastCaptain)
	}
}

func TestDerive_RelinquishedThenClaimed(t *testing.T) {
	var c chain
	c.add(CategoryAssigned, map[string]any{"subject": "github:alice"})
	c.add(CategoryRelinquished, map[string]any{"subject": "github:alice"})
	c.add(CategoryClaimed, map[string]any{"subject": "github:bob", "identity_verified": true, "claim_verified": false, "previous_captain": "github:alice"})
	st := Derive(testRepo, c.entries)
	cur := st.Current
	if cur == nil || cur.Subject != "github:bob" || cur.Basis != BasisClaimed || !cur.IdentityVerified {
		t.Fatalf("current = %+v, want claimed github:bob", cur)
	}
	if cur.ClaimVerified == nil || *cur.ClaimVerified {
		t.Errorf("ClaimVerified = %v, want false (read from the payload)", cur.ClaimVerified)
	}
	if st.LastCaptain == nil || *st.LastCaptain != "github:alice" {
		t.Errorf("LastCaptain = %v, want github:alice", st.LastCaptain)
	}
}

func TestDerive_ClaimVerifiedTrueSurvivesFold(t *testing.T) {
	var c chain
	c.add(CategoryClaimed, map[string]any{"subject": "github:bob", "claim_verified": true})
	st := Derive(testRepo, c.entries)
	if st.Current == nil || st.Current.ClaimVerified == nil || !*st.Current.ClaimVerified {
		t.Fatalf("claim_verified true lost in fold: %+v", st.Current)
	}
}

func TestDerive_LatestOfferWins(t *testing.T) {
	var c chain
	c.add(CategoryAssigned, map[string]any{"subject": "github:alice"})
	c.add(CategoryHandoverOffered, map[string]any{"subject": "github:alice", "successor": "github:carol"})
	h := c.add(CategoryHandoverOffered, map[string]any{"subject": "github:alice", "successor": "github:dave"})
	st := Derive(testRepo, c.entries)
	if st.PendingOffer == nil || st.PendingOffer.Successor != "github:dave" || st.PendingOffer.EntryHash != h {
		t.Errorf("pending offer = %+v, want the latest (dave, %s)", st.PendingOffer, h)
	}
}

// Undecodable payloads, another repo's entries, empty subjects, an offer with
// no successor and an unknown category are all SKIPPED and counted; the
// record is unchanged.
func TestDerive_SkipsUnusableEntries(t *testing.T) {
	var c chain
	c.add(CategoryAssigned, map[string]any{"subject": "github:alice"})
	c.raw(CategoryClaimed, `{not json`)
	c.add(CategoryRelinquished, map[string]any{"subject": "github:alice", "repo": "other/repo"})
	c.add(CategoryRelinquished, map[string]any{"subject": ""})
	c.add(CategoryHandoverOffered, map[string]any{"subject": "github:alice"})
	c.add("digest_marked_read", map[string]any{"subject": "github:alice"})
	st := Derive(testRepo, c.entries)
	if st.SkippedEntries != 5 {
		t.Errorf("SkippedEntries = %d, want 5", st.SkippedEntries)
	}
	if st.Current == nil || st.Current.Subject != "github:alice" || st.PendingOffer != nil {
		t.Errorf("skipped entries changed the record: %+v", st)
	}
}

func TestDerive_FoldsInSequenceOrderRegardlessOfInput(t *testing.T) {
	var c chain
	c.add(CategoryAssigned, map[string]any{"subject": "github:alice"})
	c.add(CategoryRelinquished, map[string]any{"subject": "github:alice"})
	reversed := []ChainEntry{c.entries[1], c.entries[0]}
	st := Derive(testRepo, reversed)
	if st.Current != nil {
		t.Errorf("reverse-ordered input folded as assign-after-relinquish: %+v", st.Current)
	}
}

func TestIdentityVerified(t *testing.T) {
	cases := map[string]bool{
		"github:alice":    true,
		"gitlab:alice":    true,
		"brett@local-mcp": false,
		"alice":           false,
		"":                false,
		"github:":         false,
		"GITHUB:alice":    false,
	}
	for subj, want := range cases {
		if got := IdentityVerified(subj); got != want {
			t.Errorf("IdentityVerified(%q) = %v, want %v", subj, got, want)
		}
	}
}

func TestEventPayload_RefusesMalformedEvents(t *testing.T) {
	for name, ev := range map[string]Event{
		"unknown kind": {Kind: "digest_marked_read", Repo: testRepo, Subject: "github:a"},
		"no repo":      {Kind: CategoryClaimed, Subject: "github:a"},
		"no subject":   {Kind: CategoryClaimed, Repo: testRepo},
	} {
		if _, err := ev.Payload(); err == nil {
			t.Errorf("%s: Payload() accepted %+v", name, ev)
		}
	}
}

func TestCategories_AreTheFiveCaptainKinds(t *testing.T) {
	got := Categories()
	if len(got) != 5 {
		t.Fatalf("Categories() = %v, want 5", got)
	}
	for _, c := range got {
		if !isCategory(c) {
			t.Errorf("%s not recognized", c)
		}
	}
}

// TestDerive_OfferSurfacesBrief: the five brief keys on a captain_handover_offered
// payload surface on the derived pending offer (E76.4 / #3767) — a hashed
// brief with its window, and separately an unavailable marker with its reason.
func TestDerive_OfferSurfacesBrief(t *testing.T) {
	var c chain
	c.add(CategoryAssigned, map[string]any{"subject": "github:alice", "identity_verified": true})
	c.add(CategoryHandoverOffered, map[string]any{"subject": "github:alice", "successor": "github:carol",
		"brief_hash": "abc123", "brief_from_sequence": 2, "brief_to_sequence": 9})
	st := Derive(testRepo, c.entries)
	if st.PendingOffer == nil {
		t.Fatal("no pending offer")
	}
	want := OfferBrief{Hash: "abc123", FromSequence: 2, ToSequence: 9}
	if st.PendingOffer.Brief != want {
		t.Errorf("hashed brief = %+v, want %+v", st.PendingOffer.Brief, want)
	}

	var u chain
	u.add(CategoryAssigned, map[string]any{"subject": "github:alice", "identity_verified": true})
	u.add(CategoryHandoverOffered, map[string]any{"subject": "github:alice", "successor": "github:carol",
		"brief_unavailable": true, "brief_unavailable_reason": "window_read_failed"})
	st = Derive(testRepo, u.entries)
	want = OfferBrief{Unavailable: true, UnavailableReason: "window_read_failed"}
	if st.PendingOffer == nil || st.PendingOffer.Brief != want {
		t.Errorf("unavailable brief = %+v, want %+v", st.PendingOffer, want)
	}
}

// TestDerive_PreBriefOfferHasZeroBrief: an offer written before E76.4 carries
// no brief keys and derives the zero OfferBrief (the additive-payload claim).
func TestDerive_PreBriefOfferHasZeroBrief(t *testing.T) {
	var c chain
	c.add(CategoryAssigned, map[string]any{"subject": "github:alice", "identity_verified": true})
	c.add(CategoryHandoverOffered, map[string]any{"subject": "github:alice", "successor": "github:carol"})
	st := Derive(testRepo, c.entries)
	if st.PendingOffer == nil || st.PendingOffer.Brief != (OfferBrief{}) {
		t.Errorf("pre-brief offer = %+v, want zero brief", st.PendingOffer)
	}
}

// TestEventPayload_BriefKeysOnlyOnOffer: the brief keys are emitted on
// captain_handover_offered and on NO other captain category, even when an
// event of another kind carries a Brief.
func TestEventPayload_BriefKeysOnlyOnOffer(t *testing.T) {
	brief := OfferBrief{Hash: "abc", FromSequence: 1, ToSequence: 4, Unavailable: true, UnavailableReason: "r"}
	keys := []string{"brief_hash", "brief_from_sequence", "brief_to_sequence", "brief_unavailable", "brief_unavailable_reason"}
	for _, kind := range Categories() {
		ev := Event{Kind: kind, Repo: testRepo, Subject: "github:alice", Successor: "github:carol", Brief: brief}
		raw, err := ev.Payload()
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		for _, k := range keys {
			_, present := m[k]
			if want := kind == CategoryHandoverOffered; present != want {
				t.Errorf("%s: key %s present=%v, want %v (payload %s)", kind, k, present, want, raw)
			}
		}
	}
}
