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
type recordingAppender struct {
	entries []audit.ChainAppendParams
	failOn  int
	err     error
}

func (a *recordingAppender) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.entries = append(a.entries, p)
	if a.failOn > 0 && len(a.entries) == a.failOn {
		return nil, a.err
	}
	return &audit.Entry{ID: uuid.New(), Category: p.Category, Payload: p.Payload}, nil
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
	if a.entries[0].Category != "document_injected" || a.entries[1].Category != "document_truncated" {
		t.Fatalf("categories = %q, %q; want document_injected then document_truncated",
			a.entries[0].Category, a.entries[1].Category)
	}
	if got := a.payload(t, 0)["truncated"]; got != true {
		t.Errorf("document_injected payload truncated = %v, want true", got)
	}
	p := a.payload(t, 1)
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
		a := &recordingAppender{failOn: 2, err: boom}
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
}
