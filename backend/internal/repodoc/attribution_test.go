package repodoc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// recordingAppender captures every append; failOn makes the Nth append (1-based)
// fail, so both the injected-entry and the truncated-entry failure branches are
// reachable.
//
// A FAILED append PERSISTS NOTHING — entries holds only the appends that
// succeeded. That is what the real chained appender does, and modelling it is
// what lets these tests assert the audit STATE a failure leaves behind rather
// than only the error that came back.
type recordingAppender struct {
	entries []audit.ChainAppendParams
	calls   int
	failOn  int
	err     error
}

func (a *recordingAppender) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.calls++
	if a.failOn > 0 && a.calls == a.failOn {
		return nil, a.err
	}
	a.entries = append(a.entries, p)
	return &audit.Entry{ID: uuid.New(), Category: p.Category, Payload: p.Payload}, nil
}

// categories returns the persisted categories in append order.
func (a *recordingAppender) categories() []string {
	out := make([]string, 0, len(a.entries))
	for _, e := range a.entries {
		out = append(out, e.Category)
	}
	return out
}

// countCategory counts persisted entries of one category.
func (a *recordingAppender) countCategory(c string) int {
	n := 0
	for _, e := range a.entries {
		if e.Category == c {
			n++
		}
	}
	return n
}

func (a *recordingAppender) payload(t *testing.T, i int) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(a.entries[i].Payload, &m); err != nil {
		t.Fatalf("decode payload %d: %v", i, err)
	}
	return m
}

func attributedDoc() Document {
	return Document{
		Path:            declaredPath,
		Commit:          pinnedCommit,
		ContentHash:     "sha256:abc123",
		Content:         "body",
		OriginalBytes:   400,
		RenderedBytes:   120,
		CapBytes:        DefaultMaxBytes,
		DeclarationSite: declSite,
	}
}

func TestAttribute_RecordsPathCommitAndHash(t *testing.T) {
	a := &recordingAppender{}
	runID, stageID := uuid.New(), uuid.New()

	if err := Attribute(context.Background(), a, runID, stageID, attributedDoc()); err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	if len(a.entries) != 1 {
		t.Fatalf("appended %d entries, want 1 (untruncated document)", len(a.entries))
	}
	e := a.entries[0]
	if e.Category != "document_injected" {
		t.Errorf("Category = %q, want document_injected", e.Category)
	}
	if e.RunID != runID {
		t.Errorf("RunID = %s, want %s", e.RunID, runID)
	}
	if e.StageID == nil || *e.StageID != stageID {
		t.Errorf("StageID = %v, want %s", e.StageID, stageID)
	}
	if e.ActorKind == nil || *e.ActorKind != audit.ActorSystem {
		t.Errorf("ActorKind = %v, want system", e.ActorKind)
	}
	p := a.payload(t, 0)
	for k, want := range map[string]any{
		"declaration_site": declSite,
		"path":             declaredPath,
		"commit":           pinnedCommit,
		"content_hash":     "sha256:abc123",
		"truncated":        false,
	} {
		if p[k] != want {
			t.Errorf("payload[%q] = %v, want %v", k, p[k], want)
		}
	}
	if p["original_bytes"] != float64(400) || p["rendered_bytes"] != float64(120) {
		t.Errorf("payload byte counts = %v / %v, want 400 / 120", p["original_bytes"], p["rendered_bytes"])
	}
	// The commit must be the pinned COMMIT sha, never the forge's blob id.
	if p["commit"] == fakeBlobSHA {
		t.Errorf("payload commit is the forge BLOB sha")
	}
}

// D1: cap_bytes must carry the CONFIGURED cap, not a hardcoded default. This
// test uses a NON-DEFAULT cap, so a hardcoded DefaultMaxBytes fails it.
func TestAttribute_Truncated_EmitsPairedEntryWithConfiguredCap(t *testing.T) {
	const configuredCap = 4096
	if configuredCap == DefaultMaxBytes {
		t.Fatal("fixture cap must differ from DefaultMaxBytes for this test to discriminate")
	}
	doc := attributedDoc()
	doc.Truncated = true
	doc.CapBytes = configuredCap
	doc.DroppedBytes = 280

	a := &recordingAppender{}
	if err := Attribute(context.Background(), a, uuid.New(), uuid.New(), doc); err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	if len(a.entries) != 2 {
		t.Fatalf("appended %d entries, want 2 (injected + truncated)", len(a.entries))
	}
	// ORDER: the truncation entry is written FIRST, so document_injected — the
	// only entry that CLAIMS an injection — is the last append for the
	// document and hence its commit point.
	if a.entries[0].Category != "document_truncated" || a.entries[1].Category != "document_injected" {
		t.Fatalf("categories = %q, %q; want document_truncated then document_injected",
			a.entries[0].Category, a.entries[1].Category)
	}
	if got := a.payload(t, 1)["truncated"]; got != true {
		t.Errorf("document_injected payload truncated = %v, want true", got)
	}
	p := a.payload(t, 0)
	if p["cap_bytes"] != float64(configuredCap) {
		t.Errorf("cap_bytes = %v, want the configured cap %d", p["cap_bytes"], configuredCap)
	}
	if p["dropped_bytes"] != float64(280) {
		t.Errorf("dropped_bytes = %v, want 280", p["dropped_bytes"])
	}
	for k, want := range map[string]any{"path": declaredPath, "commit": pinnedCommit, "content_hash": "sha256:abc123"} {
		if p[k] != want {
			t.Errorf("document_truncated payload[%q] = %v, want %v", k, p[k], want)
		}
	}
}

// ---------------------------------------------------------------------------
// M8: attribution fails closed.
// ---------------------------------------------------------------------------

func TestAttribute_AppendFailure_FailsClosed(t *testing.T) {
	boom := errors.New("audit: chain append failed")

	t.Run("document_injected append fails", func(t *testing.T) {
		a := &recordingAppender{failOn: 1, err: boom}
		err := Attribute(context.Background(), a, uuid.New(), uuid.New(), attributedDoc())
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the append error wrapped", err)
		}
	})

	t.Run("document_truncated append fails", func(t *testing.T) {
		doc := attributedDoc()
		doc.Truncated = true
		// The truncation entry is append #1 under the phase ordering.
		a := &recordingAppender{failOn: 1, err: boom}
		err := Attribute(context.Background(), a, uuid.New(), uuid.New(), doc)
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the truncation-append error wrapped", err)
		}
	})

	t.Run("nil appender", func(t *testing.T) {
		if err := Attribute(context.Background(), nil, uuid.New(), uuid.New(), attributedDoc()); err == nil {
			t.Fatal("err = nil, want a fail-closed error with no audit appender")
		}
	})

	t.Run("no documents is a no-op", func(t *testing.T) {
		a := &recordingAppender{}
		if err := Attribute(context.Background(), a, uuid.New(), uuid.New()); err != nil {
			t.Fatalf("Attribute with no documents: %v", err)
		}
		if len(a.entries) != 0 {
			t.Errorf("appended %d entries for an empty set, want 0", len(a.entries))
		}
	})
}

// ---------------------------------------------------------------------------
// A FAILED attribution must not leave a SUCCESSFUL-INJECTION claim behind.
//
// The audit log is append-only and hash-chained: an entry cannot be withdrawn.
// So the property is enforced by APPEND ORDER — every document_truncated entry
// is written before any document_injected entry — and these tests assert the
// persisted STATE after a failure, not the error that came back. The failing
// append is seeded by construction (recordingAppender.failOn), not by driving
// the ordering control the tests are here to be independent of.
// ---------------------------------------------------------------------------

// PAIRED-ENTRY FAILURE. A truncated document needs two appends. Whichever one
// fails, no document_injected entry may survive claiming an injection the
// caller then refused to make.
func TestAttribute_PairedEntryFailure_LeavesNoInjectionClaim(t *testing.T) {
	boom := errors.New("audit: chain append failed")
	truncated := func() Document {
		d := attributedDoc()
		d.Truncated = true
		d.DroppedBytes = 280
		return d
	}

	for _, failOn := range []int{1, 2} {
		a := &recordingAppender{failOn: failOn, err: boom}
		if err := Attribute(context.Background(), a, uuid.New(), uuid.New(), truncated()); !errors.Is(err, boom) {
			t.Fatalf("failOn=%d: err = %v, want the append error wrapped", failOn, err)
		}
		if n := a.countCategory("document_injected"); n != 0 {
			t.Errorf("failOn=%d: %d document_injected entries persisted after a failed pair, want 0 (persisted: %v)",
				failOn, n, a.categories())
		}
	}
}

// MULTI-DOCUMENT FAILURE. With more than one document, an append failure on a
// LATER document must not leave the earlier documents claimed as injected —
// and where it structurally can (phase 2 is not atomic), the surviving entries
// must be readable as an INCOMPLETE set rather than as successful injections.
func TestAttribute_MultiDocumentFailure_AuditStateIsHonest(t *testing.T) {
	boom := errors.New("audit: chain append failed")
	docAt := func(p string, truncated bool) Document {
		d := attributedDoc()
		d.Path = p
		d.Truncated = truncated
		return d
	}

	t.Run("failure in the truncation phase claims nothing at all", func(t *testing.T) {
		// Two documents, the SECOND truncated: appends are [truncated(b)],
		// then [injected(a), injected(b)]. Failing append 1 fails before any
		// claim exists.
		a := &recordingAppender{failOn: 1, err: boom}
		err := Attribute(context.Background(), a, uuid.New(), uuid.New(), docAt("a.md", false), docAt("b.md", true))
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the append error wrapped", err)
		}
		if n := a.countCategory("document_injected"); n != 0 {
			t.Errorf("%d document_injected entries persisted, want 0 (persisted: %v)", n, a.categories())
		}
	})

	t.Run("failure on a later injection leaves a visibly SHORT set", func(t *testing.T) {
		// No truncation, so appends are [injected(a), injected(b)]. Failing
		// append 2 is the residual case that cannot be made atomic without a
		// transactional batch append; the set fields are what keep the
		// survivor from reading as a successful injection.
		a := &recordingAppender{failOn: 2, err: boom}
		err := Attribute(context.Background(), a, uuid.New(), uuid.New(), docAt("a.md", false), docAt("b.md", false))
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the append error wrapped", err)
		}
		if n := a.countCategory("document_injected"); n != 1 {
			t.Fatalf("%d document_injected entries persisted, want exactly 1 (persisted: %v)", n, a.categories())
		}
		p := a.payload(t, 0)
		if p["document_count"] != float64(2) {
			t.Errorf("document_count = %v, want 2 — a survivor that does not name its set size reads as a complete injection", p["document_count"])
		}
		if p["document_index"] != float64(0) {
			t.Errorf("document_index = %v, want 0", p["document_index"])
		}
		if id, _ := p["injection_set_id"].(string); id == "" {
			t.Errorf("injection_set_id = %v, want a non-empty set id so completeness is countable per set", p["injection_set_id"])
		}
	})
}

// A SUCCESSFUL set is the control for the short-set assertion above: every
// entry shares one injection_set_id and the indices cover 0..count-1, so
// "count the entries for this set id" is the completeness test.
func TestAttribute_SuccessfulSet_IsCompleteAndSharesOneSetID(t *testing.T) {
	a := &recordingAppender{}
	d1, d2 := attributedDoc(), attributedDoc()
	d2.Path = "second.md"
	d2.Truncated = true
	if err := Attribute(context.Background(), a, uuid.New(), uuid.New(), d1, d2); err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	want := []string{"document_truncated", "document_injected", "document_injected"}
	got := a.categories()
	if len(got) != len(want) {
		t.Fatalf("categories = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("categories = %v, want %v", got, want)
		}
	}
	setID, _ := a.payload(t, 0)["injection_set_id"].(string)
	if setID == "" {
		t.Fatal("injection_set_id is empty on the truncation entry")
	}
	seen := map[float64]bool{}
	for i := 1; i < 3; i++ {
		p := a.payload(t, i)
		if id, _ := p["injection_set_id"].(string); id != setID {
			t.Errorf("entry %d injection_set_id = %v, want the shared %q", i, p["injection_set_id"], setID)
		}
		if p["document_count"] != float64(2) {
			t.Errorf("entry %d document_count = %v, want 2", i, p["document_count"])
		}
		idx, _ := p["document_index"].(float64)
		seen[idx] = true
	}
	if !seen[0] || !seen[1] {
		t.Errorf("document_index values = %v, want 0 and 1", seen)
	}

	// Two Attribute calls are two SETS (attribution is per serve), so the set
	// ids must differ — otherwise completeness is not countable across the
	// repeated entries a retry accumulates.
	b := &recordingAppender{}
	if err := Attribute(context.Background(), b, uuid.New(), uuid.New(), d1); err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	if id, _ := b.payload(t, 0)["injection_set_id"].(string); id == setID {
		t.Errorf("a second Attribute call reused injection_set_id %q", id)
	}
}
