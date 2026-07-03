package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// ---------------------------------------------------------------------
// Strict-decode mirror of the verifier's Export structs.
//
// These list EXACTLY the field set of verifier/internal/audit/export.go
// (schema/exported_at/runs; signing_key/audit_entries;
// public_key/issued_at/expires_at; id/sequence/run_id/stage_id/ts/
// category/actor_kind/actor_subject/payload/prev_hash/entry_hash).
// Decoding the handler's raw body into them with DisallowUnknownFields
// is the byte-compatibility assertion: any producer field the verifier
// would not recognize fails the decode here. The verifier package is
// `internal` and cannot be imported, so this mirror + the shared
// canonical hash fixture stand in for a direct import.
// ---------------------------------------------------------------------

type verifierExport struct {
	Schema     string                     `json:"schema"`
	ExportedAt time.Time                  `json:"exported_at"`
	Runs       map[string]verifierRunData `json:"runs"`
}

type verifierRunData struct {
	SigningKey   *verifierSigningKey `json:"signing_key,omitempty"`
	AuditEntries []verifierEntry     `json:"audit_entries"`
}

type verifierSigningKey struct {
	PublicKey string    `json:"public_key"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type verifierEntry struct {
	ID           uuid.UUID       `json:"id"`
	Sequence     int64           `json:"sequence"`
	RunID        *uuid.UUID      `json:"run_id"`
	StageID      *uuid.UUID      `json:"stage_id"`
	Timestamp    time.Time       `json:"ts"`
	Category     string          `json:"category"`
	ActorKind    *string         `json:"actor_kind"`
	ActorSubject *string         `json:"actor_subject"`
	Payload      json.RawMessage `json:"payload"`
	PrevHash     *string         `json:"prev_hash"`
	EntryHash    string          `json:"entry_hash"`
}

// strictDecodeAndVerify decodes the raw export body into the mirror
// structs with DisallowUnknownFields, asserts schema == "v1", and
// recomputes every entry's hash + prev-link across the decoded wire
// values via audit.ComputeEntryHash (pinned to the verifier's
// algorithm by the canonical fixture). This crosses the handler →
// serialize → decode → verify boundary in one assertion.
func strictDecodeAndVerify(t *testing.T, body []byte) verifierExport {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var ex verifierExport
	if err := dec.Decode(&ex); err != nil {
		t.Fatalf("strict decode (unknown/mismatched field?): %v\nbody: %s", err, body)
	}
	if ex.Schema != "v1" {
		t.Fatalf("schema = %q, want v1", ex.Schema)
	}
	if ex.Runs == nil {
		t.Fatal("runs map is nil")
	}
	for id, rd := range ex.Runs {
		if _, err := uuid.Parse(id); err != nil {
			t.Fatalf("runs key %q is not a valid UUID: %v", id, err)
		}
		verifyChain(t, id, rd.AuditEntries)
	}
	return ex
}

// verifyChain recomputes each entry's hash and prev-link, mirroring the
// verifier's verifyRun.
func verifyChain(t *testing.T, runID string, entries []verifierEntry) {
	t.Helper()
	for i, e := range entries {
		var ak *audit.ActorKind
		if e.ActorKind != nil {
			v := audit.ActorKind(*e.ActorKind)
			ak = &v
		}
		got, err := audit.ComputeEntryHash(audit.HashInputs{
			RunID:        e.RunID,
			StageID:      e.StageID,
			Timestamp:    e.Timestamp,
			Category:     e.Category,
			ActorKind:    ak,
			ActorSubject: e.ActorSubject,
			Payload:      e.Payload,
			PrevHash:     e.PrevHash,
		})
		if err != nil {
			t.Fatalf("run %s entry %d: compute hash: %v", runID, i, err)
		}
		if got != e.EntryHash {
			t.Fatalf("run %s entry %d: hash mismatch: recomputed %s, wire %s", runID, i, got, e.EntryHash)
		}
		if i == 0 {
			if e.PrevHash != nil {
				t.Fatalf("run %s first entry has non-nil prev_hash %q", runID, *e.PrevHash)
			}
			continue
		}
		prior := entries[i-1]
		if e.PrevHash == nil || *e.PrevHash != prior.EntryHash {
			t.Fatalf("run %s entry %d: chain broken (prev_hash != prior entry_hash)", runID, i)
		}
	}
}

// ---------------------------------------------------------------------
// Fakes with full control over the export inputs.
// ---------------------------------------------------------------------

type exportAuditFake struct {
	audit.BaseFake
	perRun        map[uuid.UUID][]*audit.Entry
	global        []*audit.Entry
	listForRunErr error
	listGlobalErr error
}

func (f *exportAuditFake) ListForRun(_ context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	if f.listForRunErr != nil {
		return nil, f.listForRunErr
	}
	return f.perRun[runID], nil
}

func (f *exportAuditFake) ListGlobal(_ context.Context) ([]*audit.Entry, error) {
	if f.listGlobalErr != nil {
		return nil, f.listGlobalErr
	}
	return f.global, nil
}

type exportSigningFake struct {
	keys   map[uuid.UUID]*signing.Key
	getErr error
}

func (f *exportSigningFake) Issue(_ context.Context, _ uuid.UUID, _ time.Duration) (*signing.IssuedKey, error) {
	return nil, errors.New("exportSigningFake: Issue not used")
}

func (f *exportSigningFake) Get(_ context.Context, runID uuid.UUID) (*signing.Key, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	k, ok := f.keys[runID]
	if !ok {
		return nil, signing.ErrNotFound
	}
	return k, nil
}

func (f *exportSigningFake) Verify(_ context.Context, _ uuid.UUID, _, _ []byte) error {
	return errors.New("exportSigningFake: Verify not used")
}

// chainEntries builds genuinely-chained audit entries (real prev_hash /
// entry_hash via audit.ComputeEntryHash) for the given run — nil runID
// for the global-chain partition, so its entries carry run_id:null.
func chainEntries(t *testing.T, runID *uuid.UUID, categories ...string) []*audit.Entry {
	t.Helper()
	var out []*audit.Entry
	var prev *string
	actor := audit.ActorSystem
	subject := "system@fishhawk"
	for i, cat := range categories {
		ts := time.Date(2026, 5, 1, 12, i, 30, 0, time.UTC)
		payload := json.RawMessage(fmt.Sprintf(`{"i":%d,"cat":%q}`, i, cat))
		// Exercise the nullable hashed fields (actor_kind/actor_subject)
		// so their JSON round-trip is part of the chain-verify.
		h, err := audit.ComputeEntryHash(audit.HashInputs{
			RunID:        runID,
			Timestamp:    ts,
			Category:     cat,
			ActorKind:    &actor,
			ActorSubject: &subject,
			Payload:      payload,
			PrevHash:     prev,
		})
		if err != nil {
			t.Fatalf("compute hash: %v", err)
		}
		out = append(out, &audit.Entry{
			ID:           uuid.New(),
			Sequence:     int64(i + 1),
			RunID:        runID,
			Timestamp:    ts,
			Category:     cat,
			ActorKind:    &actor,
			ActorSubject: &subject,
			Payload:      payload,
			PrevHash:     prev,
			EntryHash:    h,
		})
		hh := h
		prev = &hh
	}
	return out
}

func makeSigningKey(t *testing.T, runID uuid.UUID) *signing.Key {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &signing.Key{
		RunID:     runID,
		PublicKey: pub,
		IssuedAt:  time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 5, 2, 11, 0, 0, 0, time.UTC),
	}
}

// seedExportRun inserts a run with an explicit created_at (reusing the
// package's seedRun helper) and returns its generated id.
func seedExportRun(fr *fakeRepo, repo string, createdAt time.Time) uuid.UUID {
	return seedRun(fr, repo, "", run.StatePending, createdAt).ID
}

func doExport(s *Server, query string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/audit/export"+query, nil)
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func configuredExportServer(af *exportAuditFake, fr *fakeRepo, sf *exportSigningFake) *Server {
	return New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: sf})
}

func realRunIDs(ex verifierExport) []string {
	var out []string
	for id := range ex.Runs {
		if id == exportGlobalChainKey {
			continue
		}
		out = append(out, id)
	}
	return out
}

// ---------------------------------------------------------------------
// Primary end-to-end test: chain verification across the boundary.
// ---------------------------------------------------------------------

func TestAuditExport_EndToEndChainVerify(t *testing.T) {
	fr := newFakeRepo()
	runA := seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	runB := seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC))

	af := &exportAuditFake{
		perRun: map[uuid.UUID][]*audit.Entry{
			runA: chainEntries(t, &runA, "run_created", "plan_generated", "plan_approved"),
			runB: chainEntries(t, &runB, "run_created", "implement_reviewed"),
		},
		// Global chain entries carry run_id:null.
		global: chainEntries(t, nil, "token_issued", "token_revoked"),
	}
	sf := &exportSigningFake{keys: map[uuid.UUID]*signing.Key{
		runA: makeSigningKey(t, runA),
		runB: makeSigningKey(t, runB),
	}}
	s := configuredExportServer(af, fr, sf)

	rec := doExport(s, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Fishhawk-Export-Complete"); got != "true" {
		t.Errorf("Complete header = %q, want true", got)
	}
	if got := rec.Header().Get("X-Fishhawk-Export-Next-Cursor"); got != "" {
		t.Errorf("Next-Cursor header = %q, want empty on complete export", got)
	}

	ex := strictDecodeAndVerify(t, rec.Body.Bytes())

	if _, ok := ex.Runs[runA.String()]; !ok {
		t.Error("run A missing from export")
	}
	if _, ok := ex.Runs[runB.String()]; !ok {
		t.Error("run B missing from export")
	}
	gd, ok := ex.Runs[exportGlobalChainKey]
	if !ok {
		t.Fatal("global partition missing under nil-UUID key")
	}
	if len(gd.AuditEntries) != 2 {
		t.Fatalf("global chain: got %d entries, want 2", len(gd.AuditEntries))
	}
	for i, e := range gd.AuditEntries {
		if e.RunID != nil {
			t.Errorf("global entry %d run_id = %v, want null", i, e.RunID)
		}
	}

	rd := ex.Runs[runA.String()]
	if rd.SigningKey == nil {
		t.Fatal("run A signing_key absent")
	}
	pub, err := base64.StdEncoding.DecodeString(rd.SigningKey.PublicKey)
	if err != nil {
		t.Fatalf("run A public_key not base64: %v", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		t.Errorf("run A public key len = %d, want %d", len(pub), ed25519.PublicKeySize)
	}
}

// ---------------------------------------------------------------------
// Failure / defensive modes.
// ---------------------------------------------------------------------

// (1) 503 audit_export_unconfigured for each nil repo.
func TestAuditExport_Unconfigured(t *testing.T) {
	af := &exportAuditFake{}
	fr := newFakeRepo()
	sf := &exportSigningFake{}
	cases := []struct {
		name string
		cfg  Config
	}{
		{"nil audit", Config{RunRepo: fr, SigningRepo: sf}},
		{"nil run", Config{AuditRepo: af, SigningRepo: sf}},
		{"nil signing", Config{AuditRepo: af, RunRepo: fr}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(tc.cfg)
			rec := doExport(s, "")
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body.String())
			}
			if body := rec.Body.String(); !strings.Contains(body, "audit_export_unconfigured") {
				t.Errorf("body = %s, want audit_export_unconfigured", body)
			}
		})
	}
}

// (12) route-registered guard: all-nil Config reaches handleAuditExport
// (503), proving the mux wiring. An UNregistered route would 404.
func TestAuditExportRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := doExport(s, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (route reaches handler)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "audit_export_unconfigured") {
		t.Errorf("body = %s, want audit_export_unconfigured", rec.Body.String())
	}
}

// (2) 400 on malformed from/to and on from > to.
func TestAuditExport_BadDateRange(t *testing.T) {
	s := configuredExportServer(&exportAuditFake{}, newFakeRepo(), &exportSigningFake{})
	cases := []struct{ name, query string }{
		{"bad from", "?from=not-a-time"},
		{"bad to", "?to=nope"},
		{"from after to", "?from=2026-05-02T00:00:00Z&to=2026-05-01T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doExport(s, tc.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "validation_failed") {
				t.Errorf("body = %s, want validation_failed", rec.Body.String())
			}
		})
	}
}

// (3) 400 on a non-UUID run_id.
func TestAuditExport_BadRunID(t *testing.T) {
	s := configuredExportServer(&exportAuditFake{}, newFakeRepo(), &exportSigningFake{})
	rec := doExport(s, "?run_id=not-a-uuid")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "validation_failed") {
		t.Errorf("body = %s, want validation_failed", rec.Body.String())
	}
}

// (4) 404 run_not_found when an explicitly requested run does not exist.
func TestAuditExport_RunNotFound(t *testing.T) {
	s := configuredExportServer(&exportAuditFake{}, newFakeRepo(), &exportSigningFake{})
	rec := doExport(s, "?run_id="+uuid.New().String())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "run_not_found") {
		t.Errorf("body = %s, want run_not_found", rec.Body.String())
	}
}

// (5) 400 when run_id is combined with repo/from/to.
func TestAuditExport_MutualExclusion(t *testing.T) {
	s := configuredExportServer(&exportAuditFake{}, newFakeRepo(), &exportSigningFake{})
	id := uuid.New().String()
	cases := []struct{ name, query string }{
		{"run_id + repo", "?run_id=" + id + "&repo=acme/app"},
		{"run_id + from", "?run_id=" + id + "&from=2026-05-01T00:00:00Z"},
		{"run_id + to", "?run_id=" + id + "&to=2026-05-01T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doExport(s, tc.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "validation_failed") {
				t.Errorf("body = %s, want validation_failed", rec.Body.String())
			}
		})
	}
}

// (6) 400 on out-of-range limit and on an undecodable cursor.
func TestAuditExport_BadLimitAndCursor(t *testing.T) {
	s := configuredExportServer(&exportAuditFake{}, newFakeRepo(), &exportSigningFake{})
	t.Run("limit zero", func(t *testing.T) {
		rec := doExport(s, "?limit=0")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "validation_failed") {
			t.Fatalf("status %d body %s, want 400 validation_failed", rec.Code, rec.Body.String())
		}
	})
	t.Run("limit over max", func(t *testing.T) {
		rec := doExport(s, "?limit=9999")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "validation_failed") {
			t.Fatalf("status %d body %s, want 400 validation_failed", rec.Code, rec.Body.String())
		}
	})
	t.Run("undecodable cursor", func(t *testing.T) {
		rec := doExport(s, "?cursor=not!base64!")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "cursor_invalid") {
			t.Fatalf("status %d body %s, want 400 cursor_invalid", rec.Code, rec.Body.String())
		}
	})
	t.Run("wrong-shape cursor", func(t *testing.T) {
		bogus := base64.URLEncoding.EncodeToString([]byte(`{"unexpected":1}`))
		rec := doExport(s, "?cursor="+bogus)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "cursor_invalid") {
			t.Fatalf("status %d body %s, want 400 cursor_invalid", rec.Code, rec.Body.String())
		}
	})
	// A decodable-but-incomplete cursor must be rejected, not silently
	// treated as a zero-value keyset that mis-orders into an empty
	// complete page. Both the empty object and each single-field shape
	// leave one required component zero.
	t.Run("empty-object cursor", func(t *testing.T) {
		bogus := base64.URLEncoding.EncodeToString([]byte(`{}`))
		rec := doExport(s, "?cursor="+bogus)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "cursor_invalid") {
			t.Fatalf("status %d body %s, want 400 cursor_invalid", rec.Code, rec.Body.String())
		}
	})
	t.Run("missing-id cursor", func(t *testing.T) {
		bogus := base64.URLEncoding.EncodeToString([]byte(`{"created_at":"2026-01-01T00:00:00Z"}`))
		rec := doExport(s, "?cursor="+bogus)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "cursor_invalid") {
			t.Fatalf("status %d body %s, want 400 cursor_invalid", rec.Code, rec.Body.String())
		}
	})
	t.Run("missing-created-at cursor", func(t *testing.T) {
		bogus := base64.URLEncoding.EncodeToString([]byte(`{"id":"11111111-1111-1111-1111-111111111111"}`))
		rec := doExport(s, "?cursor="+bogus)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "cursor_invalid") {
			t.Fatalf("status %d body %s, want 400 cursor_invalid", rec.Code, rec.Body.String())
		}
	})
}

// (7) signing key ErrNotFound → run exported with signing_key omitted
// while other runs keep theirs.
func TestAuditExport_SigningKeyOmitted(t *testing.T) {
	fr := newFakeRepo()
	runA := seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	runB := seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC))
	af := &exportAuditFake{perRun: map[uuid.UUID][]*audit.Entry{
		runA: chainEntries(t, &runA, "run_created"),
		runB: chainEntries(t, &runB, "run_created"),
	}}
	// Only run A has a key; run B's Get returns ErrNotFound.
	sf := &exportSigningFake{keys: map[uuid.UUID]*signing.Key{runA: makeSigningKey(t, runA)}}
	s := configuredExportServer(af, fr, sf)

	rec := doExport(s, "?include_global=false")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	ex := strictDecodeAndVerify(t, rec.Body.Bytes())
	if ex.Runs[runA.String()].SigningKey == nil {
		t.Error("run A should have a signing_key")
	}
	if ex.Runs[runB.String()].SigningKey != nil {
		t.Error("run B should have signing_key omitted (ErrNotFound)")
	}
	// Wire-level: exactly one signing_key across the two runs.
	if n := strings.Count(rec.Body.String(), `"signing_key"`); n != 1 {
		t.Errorf("expected exactly 1 signing_key on the wire, got %d", n)
	}
}

// (8) global partition: present by default (incl. empty array when the
// chain is empty), absent when include_global=false, run_id:null
// entries chain-verify.
func TestAuditExport_GlobalPartition(t *testing.T) {
	t.Run("present with entries by default", func(t *testing.T) {
		fr := newFakeRepo()
		id := seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
		af := &exportAuditFake{
			perRun: map[uuid.UUID][]*audit.Entry{id: chainEntries(t, &id, "run_created")},
			global: chainEntries(t, nil, "token_issued"),
		}
		s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})
		rec := doExport(s, "")
		ex := strictDecodeAndVerify(t, rec.Body.Bytes())
		gd, ok := ex.Runs[exportGlobalChainKey]
		if !ok || len(gd.AuditEntries) != 1 {
			t.Fatalf("global partition missing or wrong size: %+v (ok=%v)", gd, ok)
		}
		if gd.AuditEntries[0].RunID != nil {
			t.Error("global entry run_id must be null")
		}
	})

	t.Run("present as empty array when chain empty", func(t *testing.T) {
		fr := newFakeRepo()
		id := seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
		af := &exportAuditFake{
			perRun: map[uuid.UUID][]*audit.Entry{id: chainEntries(t, &id, "run_created")},
			global: nil,
		}
		s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})
		rec := doExport(s, "")
		ex := strictDecodeAndVerify(t, rec.Body.Bytes())
		gd, ok := ex.Runs[exportGlobalChainKey]
		if !ok {
			t.Fatal("global partition must be present even when empty (never silently dropped)")
		}
		if gd.AuditEntries == nil {
			t.Error("empty global chain must serialize audit_entries as [], got null")
		}
		if len(gd.AuditEntries) != 0 {
			t.Errorf("global chain should be empty, got %d", len(gd.AuditEntries))
		}
	})

	t.Run("absent when include_global=false", func(t *testing.T) {
		fr := newFakeRepo()
		id := seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
		af := &exportAuditFake{
			perRun: map[uuid.UUID][]*audit.Entry{id: chainEntries(t, &id, "run_created")},
			global: chainEntries(t, nil, "token_issued"),
		}
		s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})
		rec := doExport(s, "?include_global=false")
		ex := strictDecodeAndVerify(t, rec.Body.Bytes())
		if _, ok := ex.Runs[exportGlobalChainKey]; ok {
			t.Error("global partition present despite include_global=false")
		}
	})
}

// (9) continuation round-trip: limit=1 over 3 runs → disjoint whole-run
// pages whose union equals the full export; final page Complete:true,
// no cursor; each page strict-decodes and chain-verifies. Global
// partition appears on page 1 only.
func TestAuditExport_Continuation(t *testing.T) {
	fr := newFakeRepo()
	ids := []uuid.UUID{
		seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)),
		seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC)),
		seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)),
	}
	perRun := map[uuid.UUID][]*audit.Entry{}
	for _, id := range ids {
		perRun[id] = chainEntries(t, &id, "run_created", "plan_generated")
	}
	af := &exportAuditFake{perRun: perRun, global: chainEntries(t, nil, "token_issued")}
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})

	seen := map[string]int{}
	cursor := ""
	pages := 0
	globalPages := 0
	for {
		pages++
		if pages > 10 {
			t.Fatal("continuation did not terminate")
		}
		q := "?limit=1"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		rec := doExport(s, q)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status %d: %s", pages, rec.Code, rec.Body.String())
		}
		ex := strictDecodeAndVerify(t, rec.Body.Bytes())
		realThisPage := 0
		for id := range ex.Runs {
			if id == exportGlobalChainKey {
				globalPages++
				continue
			}
			seen[id]++
			realThisPage++
		}
		if realThisPage != 1 {
			t.Errorf("page %d has %d real runs, want 1 (limit=1)", pages, realThisPage)
		}
		complete := rec.Header().Get("X-Fishhawk-Export-Complete")
		next := rec.Header().Get("X-Fishhawk-Export-Next-Cursor")
		if complete == "true" {
			if next != "" {
				t.Errorf("final page carries a next cursor %q", next)
			}
			break
		}
		if complete != "false" || next == "" {
			t.Fatalf("partial page %d: complete=%q next=%q", pages, complete, next)
		}
		cursor = next
	}

	if pages != 3 {
		t.Errorf("got %d pages, want 3 (limit=1 over 3 runs)", pages)
	}
	if globalPages != 1 {
		t.Errorf("global partition appeared on %d pages, want 1 (first page only)", globalPages)
	}
	if len(seen) != 3 {
		t.Errorf("union covered %d distinct runs, want 3", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("run %s appeared on %d pages, want exactly 1 (disjoint)", id, n)
		}
	}
}

// (10) keyset stability: a run created between page 1 and page 2 (newer
// created_at) does not shift or duplicate page-2 contents.
func TestAuditExport_KeysetStability(t *testing.T) {
	fr := newFakeRepo()
	orig := []uuid.UUID{
		seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)),
		seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC)),
		seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)),
	}
	perRun := map[uuid.UUID][]*audit.Entry{}
	for _, id := range orig {
		perRun[id] = chainEntries(t, &id, "run_created")
	}
	af := &exportAuditFake{perRun: perRun}
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})

	// Page 1 (limit=1) → newest run (12:00 → orig[0]).
	rec1 := doExport(s, "?limit=1&include_global=false")
	ex1 := strictDecodeAndVerify(t, rec1.Body.Bytes())
	page1 := realRunIDs(ex1)
	if len(page1) != 1 {
		t.Fatalf("page1 has %d runs, want 1", len(page1))
	}
	if page1[0] != orig[0].String() {
		t.Fatalf("page1 = %s, want newest orig[0] %s", page1[0], orig[0].String())
	}
	next := rec1.Header().Get("X-Fishhawk-Export-Next-Cursor")
	if next == "" {
		t.Fatal("page1 must carry a next cursor")
	}

	// A brand-new run appears with the NEWEST created_at — it sorts
	// before the cursor position and must not shift/duplicate page 2.
	newer := seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 13, 0, 0, 0, time.UTC))
	perRun[newer] = chainEntries(t, &newer, "run_created")

	rec2 := doExport(s, "?limit=1&include_global=false&cursor="+next)
	ex2 := strictDecodeAndVerify(t, rec2.Body.Bytes())
	page2 := realRunIDs(ex2)
	if len(page2) != 1 {
		t.Fatalf("page2 has %d runs, want 1", len(page2))
	}
	if page2[0] == page1[0] {
		t.Error("page2 duplicated page1's run")
	}
	if page2[0] == newer.String() {
		t.Error("newly inserted run leaked into page2 (keyset by index, not value)")
	}
	if page2[0] != orig[1].String() {
		t.Errorf("page2 = %s, want orig[1] %s (stable keyset)", page2[0], orig[1].String())
	}
}

// (11) repo filter resolves through the runs join and date-range
// selects by run created_at, always emitting each selected run's FULL
// chain.
func TestAuditExport_RepoAndDateFilter(t *testing.T) {
	fr := newFakeRepo()
	inRepo := seedExportRun(fr, "acme/app", time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	otherRepo := seedExportRun(fr, "other/repo", time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	oldRun := seedExportRun(fr, "acme/app", time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC))
	af := &exportAuditFake{perRun: map[uuid.UUID][]*audit.Entry{
		inRepo:    chainEntries(t, &inRepo, "run_created", "plan_generated", "plan_approved"),
		otherRepo: chainEntries(t, &otherRepo, "run_created"),
		oldRun:    chainEntries(t, &oldRun, "run_created"),
	}}
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})

	rec := doExport(s, "?repo=acme/app&from=2026-05-01T00:00:00Z&to=2026-05-31T00:00:00Z&include_global=false")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	ex := strictDecodeAndVerify(t, rec.Body.Bytes())
	ids := realRunIDs(ex)
	if len(ids) != 1 || ids[0] != inRepo.String() {
		t.Fatalf("selected runs = %v, want only inRepo %s (repo join + date range)", ids, inRepo.String())
	}
	if got := len(ex.Runs[inRepo.String()].AuditEntries); got != 3 {
		t.Errorf("inRepo chain length = %d, want 3 (full chain)", got)
	}
	if fr.lastListFilter.Repo != "acme/app" {
		t.Errorf("ListRuns repo filter = %q, want acme/app", fr.lastListFilter.Repo)
	}
}

// (extra) explicit run-id mode exports exactly the requested set.
func TestAuditExport_ExplicitRunIDs(t *testing.T) {
	fr := newFakeRepo()
	a := seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	b := seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC))
	c := seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC))
	af := &exportAuditFake{perRun: map[uuid.UUID][]*audit.Entry{
		a: chainEntries(t, &a, "run_created"),
		b: chainEntries(t, &b, "run_created"),
		c: chainEntries(t, &c, "run_created"),
	}}
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})

	rec := doExport(s, "?run_id="+a.String()+"&run_id="+c.String()+"&include_global=false")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	ex := strictDecodeAndVerify(t, rec.Body.Bytes())
	ids := realRunIDs(ex)
	if len(ids) != 2 {
		t.Fatalf("got %d runs, want exactly 2 (a and c)", len(ids))
	}
	if _, ok := ex.Runs[b.String()]; ok {
		t.Error("run b was not requested but is present")
	}
}

// (extra) a ListForRun error surfaces as 500 (defensive assembly path).
func TestAuditExport_AssembleError(t *testing.T) {
	fr := newFakeRepo()
	id := seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	af := &exportAuditFake{
		perRun:        map[uuid.UUID][]*audit.Entry{id: chainEntries(t, &id, "run_created")},
		listForRunErr: errors.New("boom"),
	}
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})
	rec := doExport(s, "?include_global=false")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "internal_error") {
		t.Errorf("body = %s, want internal_error", rec.Body.String())
	}
}

// (extra) a ListGlobal error surfaces as 500 (defensive global path).
func TestAuditExport_GlobalError(t *testing.T) {
	fr := newFakeRepo()
	id := seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	af := &exportAuditFake{
		perRun:        map[uuid.UUID][]*audit.Entry{id: chainEntries(t, &id, "run_created")},
		listGlobalErr: errors.New("global boom"),
	}
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})
	rec := doExport(s, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "internal_error") {
		t.Errorf("body = %s, want internal_error", rec.Body.String())
	}
}

// (extra) a GetRun error other than ErrNotFound in explicit mode
// surfaces as 500 (defensive selectExplicitRuns path).
func TestAuditExport_ExplicitGetRunError(t *testing.T) {
	fr := newFakeRepo()
	fr.getErr = errors.New("db down")
	s := New(Config{AuditRepo: &exportAuditFake{}, RunRepo: fr, SigningRepo: &exportSigningFake{}})
	rec := doExport(s, "?run_id="+uuid.New().String())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "internal_error") {
		t.Errorf("body = %s, want internal_error", rec.Body.String())
	}
}

// (extra) a ListRuns error in filter mode surfaces as 500 (defensive
// materializeRuns path).
func TestAuditExport_ListRunsError(t *testing.T) {
	fr := newFakeRepo()
	fr.listErr = errors.New("list boom")
	s := New(Config{AuditRepo: &exportAuditFake{}, RunRepo: fr, SigningRepo: &exportSigningFake{}})
	rec := doExport(s, "?repo=acme/app")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "internal_error") {
		t.Errorf("body = %s, want internal_error", rec.Body.String())
	}
}

// (extra) a SigningRepo.Get error other than ErrNotFound surfaces as 500.
func TestAuditExport_SigningError(t *testing.T) {
	fr := newFakeRepo()
	id := seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	af := &exportAuditFake{perRun: map[uuid.UUID][]*audit.Entry{id: chainEntries(t, &id, "run_created")}}
	sf := &exportSigningFake{getErr: errors.New("signing boom")}
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: sf})
	rec := doExport(s, "?include_global=false")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "internal_error") {
		t.Errorf("body = %s, want internal_error", rec.Body.String())
	}
}
