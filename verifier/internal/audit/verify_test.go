package audit_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/verifier/internal/audit"
)

// makeChain builds a 3-entry valid chain for a single run. Each
// entry's hash is computed via the canonical algorithm, and
// prev_hash linkage is set correctly. Used as the happy-path
// fixture; mutate it to simulate tampering.
func makeChain(t *testing.T, runID uuid.UUID) []audit.Entry {
	t.Helper()

	mkPayload := func(s string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"event": s})
		return b
	}

	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	entries := make([]audit.Entry, 3)
	var prev *string

	for i := range entries {
		ts := now.Add(time.Duration(i) * time.Second)
		payload := mkPayload(string(rune('a' + i)))
		rid := runID
		hash, err := audit.ComputeEntryHash(audit.HashInputs{
			RunID:     &rid,
			Timestamp: ts,
			Category:  "step",
			Payload:   payload,
			PrevHash:  prev,
		})
		if err != nil {
			t.Fatal(err)
		}
		entries[i] = audit.Entry{
			ID:        uuid.New(),
			Sequence:  int64(i + 1),
			RunID:     &rid,
			Timestamp: ts,
			Category:  "step",
			Payload:   payload,
			PrevHash:  prev,
			EntryHash: hash,
		}
		h := hash
		prev = &h
	}
	return entries
}

func makeExport(t *testing.T) *audit.Export {
	t.Helper()
	runID := uuid.MustParse("00000000-0000-0000-0000-000000000099")
	return &audit.Export{
		Schema:     audit.ExportSchemaV1,
		ExportedAt: time.Now().UTC(),
		Runs: map[string]audit.RunData{
			runID.String(): {
				AuditEntries: makeChain(t, runID),
			},
		},
	}
}

// --- ParseExport ---

func TestParseExport_HappyPath(t *testing.T) {
	ex := makeExport(t)
	body, err := json.Marshal(ex)
	if err != nil {
		t.Fatal(err)
	}
	got, err := audit.ParseExport(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("ParseExport: %v", err)
	}
	if got.Schema != audit.ExportSchemaV1 {
		t.Errorf("Schema = %q", got.Schema)
	}
	if len(got.Runs) != 1 {
		t.Errorf("got %d runs, want 1", len(got.Runs))
	}
}

func TestParseExport_RejectsUnknownSchema(t *testing.T) {
	body := []byte(`{"schema":"v999","exported_at":"2026-05-01T00:00:00Z","runs":{}}`)
	_, err := audit.ParseExport(bytes.NewReader(body))
	if err == nil {
		t.Fatal("expected error on unknown schema")
	}
	if !strings.Contains(err.Error(), "v999") {
		t.Errorf("error should name the bad schema: %v", err)
	}
}

func TestParseExport_RejectsUnknownTopLevelField(t *testing.T) {
	body := []byte(`{"schema":"v1","exported_at":"2026-05-01T00:00:00Z","runs":{},"surprise":"hi"}`)
	_, err := audit.ParseExport(bytes.NewReader(body))
	if err == nil {
		t.Fatal("expected error on unknown field")
	}
}

func TestParseExport_MalformedJSON(t *testing.T) {
	_, err := audit.ParseExport(strings.NewReader(`{not json`))
	if err == nil {
		t.Fatal("expected error on malformed JSON")
	}
}

// --- VerifyExport happy path ---

func TestVerify_HappyPath(t *testing.T) {
	ex := makeExport(t)
	res := audit.VerifyExport(ex)
	if !res.OK() {
		t.Errorf("expected no issues; got %+v", res.Issues)
	}
	if res.RunsVerified != 1 {
		t.Errorf("RunsVerified = %d", res.RunsVerified)
	}
	if res.EntriesChecked != 3 {
		t.Errorf("EntriesChecked = %d", res.EntriesChecked)
	}
}

// --- VerifyExport tamper detection ---

func TestVerify_HashMismatch(t *testing.T) {
	ex := makeExport(t)
	for k, run := range ex.Runs {
		// Tamper with the second entry's payload without updating
		// its EntryHash. Recompute should disagree.
		run.AuditEntries[1].Payload = json.RawMessage(`{"event":"TAMPERED"}`)
		ex.Runs[k] = run
	}
	res := audit.VerifyExport(ex)
	if res.OK() {
		t.Fatal("expected hash_mismatch issue; got OK")
	}
	found := false
	for _, iss := range res.Issues {
		if iss.Kind == audit.IssueHashMismatch {
			found = true
		}
	}
	if !found {
		t.Errorf("missing IssueHashMismatch in %+v", res.Issues)
	}
}

func TestVerify_ChainBroken(t *testing.T) {
	ex := makeExport(t)
	for k, run := range ex.Runs {
		// Replace the third entry's prev_hash with garbage. The
		// entry's own hash is still valid (recomputed from its
		// fields including the now-bogus prev_hash), but the
		// chain check finds the link doesn't point at the prior
		// entry.
		bogus := "deadbeef"
		run.AuditEntries[2].PrevHash = &bogus
		// Recompute entry hash so we don't also surface
		// IssueHashMismatch — we want the test to isolate the
		// chain-break case.
		newHash, err := audit.ComputeEntryHash(run.AuditEntries[2].ToHashInputs())
		if err != nil {
			t.Fatal(err)
		}
		run.AuditEntries[2].EntryHash = newHash
		ex.Runs[k] = run
	}
	res := audit.VerifyExport(ex)
	if res.OK() {
		t.Fatal("expected chain_broken issue; got OK")
	}
	found := false
	for _, iss := range res.Issues {
		if iss.Kind == audit.IssueChainBroken {
			found = true
		}
	}
	if !found {
		t.Errorf("missing IssueChainBroken in %+v", res.Issues)
	}
}

func TestVerify_FirstEntryHasPrevHash(t *testing.T) {
	ex := makeExport(t)
	for k, run := range ex.Runs {
		bogus := "deadbeef"
		run.AuditEntries[0].PrevHash = &bogus
		newHash, _ := audit.ComputeEntryHash(run.AuditEntries[0].ToHashInputs())
		run.AuditEntries[0].EntryHash = newHash
		// Also fix entries[1].prev_hash so we don't get a chain-
		// break in addition to the first-entry issue.
		run.AuditEntries[1].PrevHash = &newHash
		newHash2, _ := audit.ComputeEntryHash(run.AuditEntries[1].ToHashInputs())
		run.AuditEntries[1].EntryHash = newHash2
		// And entries[2] for the same reason.
		run.AuditEntries[2].PrevHash = &newHash2
		newHash3, _ := audit.ComputeEntryHash(run.AuditEntries[2].ToHashInputs())
		run.AuditEntries[2].EntryHash = newHash3
		ex.Runs[k] = run
	}
	res := audit.VerifyExport(ex)
	if res.OK() {
		t.Fatal("expected first_entry_has_prev_hash issue; got OK")
	}
	found := false
	for _, iss := range res.Issues {
		if iss.Kind == audit.IssueFirstEntryHasPrevHash {
			found = true
		}
	}
	if !found {
		t.Errorf("missing IssueFirstEntryHasPrevHash in %+v", res.Issues)
	}
}

func TestVerify_SequenceNotMonotonic(t *testing.T) {
	ex := makeExport(t)
	for k, run := range ex.Runs {
		// Swap entries[0] and entries[1] sequence values without
		// reordering — reads as out-of-order.
		run.AuditEntries[0].Sequence, run.AuditEntries[1].Sequence =
			run.AuditEntries[1].Sequence, run.AuditEntries[0].Sequence
		ex.Runs[k] = run
	}
	res := audit.VerifyExport(ex)
	found := false
	for _, iss := range res.Issues {
		if iss.Kind == audit.IssueSequenceNotMonotonic {
			found = true
		}
	}
	if !found {
		t.Errorf("missing IssueSequenceNotMonotonic in %+v", res.Issues)
	}
}

func TestVerify_NilExport(t *testing.T) {
	res := audit.VerifyExport(nil)
	if res.OK() {
		t.Error("nil export should produce at least one issue")
	}
}

func TestVerify_BadRunIDInExport(t *testing.T) {
	ex := &audit.Export{
		Schema: audit.ExportSchemaV1,
		Runs: map[string]audit.RunData{
			"not-a-uuid": {AuditEntries: nil},
		},
	}
	res := audit.VerifyExport(ex)
	if res.OK() {
		t.Error("export with malformed run id should produce an issue")
	}
}

// --- VerifyBundleSignature ---

func TestVerifyBundleSignature_HappyPath(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	bundle := []byte("some gzipped bundle bytes")
	sum := sha256.Sum256(bundle)
	sig := ed25519.Sign(priv, sum[:])

	if err := audit.VerifyBundleSignature(pub, bundle, sig); err != nil {
		t.Errorf("VerifyBundleSignature: %v", err)
	}
}

func TestVerifyBundleSignature_DetectsTampering(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	bundle := []byte("legit bundle")
	sum := sha256.Sum256(bundle)
	sig := ed25519.Sign(priv, sum[:])

	tampered := []byte("tampered bundle")
	if err := audit.VerifyBundleSignature(pub, tampered, sig); err == nil {
		t.Fatal("expected signature error on tampered bundle")
	}
}

func TestVerifyBundleSignature_RejectsBadKeyLength(t *testing.T) {
	if err := audit.VerifyBundleSignature(make([]byte, 16), []byte("x"), make([]byte, 64)); err == nil {
		t.Fatal("expected error on short public key")
	}
}

func TestVerifyBundleSignature_RejectsBadSignatureLength(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := audit.VerifyBundleSignature(pub, []byte("x"), make([]byte, 32)); err == nil {
		t.Fatal("expected error on short signature")
	}
}

// --- DecodePublicKey ---

func TestDecodePublicKey_RoundTrip(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	enc := base64.StdEncoding.EncodeToString(pub)
	got, err := (audit.SigningKey{PublicKey: enc}).DecodePublicKey()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pub) {
		t.Errorf("round-trip mismatch")
	}
}

func TestDecodePublicKey_RejectsNonBase64(t *testing.T) {
	_, err := (audit.SigningKey{PublicKey: "not!base64@"}).DecodePublicKey()
	if err == nil {
		t.Fatal("expected error on invalid base64")
	}
}
