package server

// audit_export_auth_test.go — the E9.5/#1608 authorization matrix for
// the bulk export surfaces (GET /v0/audit/export, /v0/audit/export.csv,
// /v0/reports/agent-changes and .md), plus the ADR-054 raw-bundle
// non-inlining invariant: exports carry content-hash POINTERS only,
// never trace bundle bytes.
//
// The matrix tests drive the FULL middleware chain (Handler()) so the
// bearerAuth token resolution → requireWriteScope path is exercised
// end-to-end; the cookie-session bypass is asserted at the handler
// layer with an injected TokenID=="" identity, the same convention the
// package's other scope tests use (see runs_fake_test.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
)

// exportAuthSurfaces enumerates every route the read:audit-export scope
// gates. A surface added to the export family must be added here (and
// given the same guard) or the matrix goes stale.
var exportAuthSurfaces = []struct{ name, path string }{
	{"export-json", "/v0/audit/export"},
	{"export-csv", "/v0/audit/export.csv"},
	{"report-json", "/v0/reports/agent-changes"},
	{"report-md", "/v0/reports/agent-changes.md"},
}

// stubAPITokenRepo satisfies apitoken.Repository for the one method
// bearerAuth calls. Every other method panics through the embedded nil
// interface so an accidental call is loud.
type stubAPITokenRepo struct {
	apitoken.Repository
	tok *apitoken.Token
}

func (s *stubAPITokenRepo) Authenticate(_ context.Context, plaintext string) (*apitoken.Token, error) {
	if s.tok != nil && plaintext == s.tok.PlainText {
		return s.tok, nil
	}
	return nil, apitoken.ErrNotFound
}

func stubToken(scopes ...string) *stubAPITokenRepo {
	return &stubAPITokenRepo{tok: &apitoken.Token{
		ID:        uuid.New(),
		Subject:   "github:op",
		Scopes:    scopes,
		PlainText: "fhk_test_export_matrix",
	}}
}

func exportAuthGET(s *Server, path, bearer string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// Anonymous callers are 401 on every export surface — before the
// configuration probe, so an unauthenticated caller cannot even learn
// whether export is configured.
func TestExportSurfaces_Anonymous_401(t *testing.T) {
	s := New(Config{})
	for _, sf := range exportAuthSurfaces {
		t.Run(sf.name, func(t *testing.T) {
			rec := exportAuthGET(s, sf.path, "")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "authentication_required") {
				t.Errorf("body = %s, want authentication_required", rec.Body.String())
			}
		})
	}
}

// A real bearer token resolved through the full middleware chain that
// lacks read:audit-export is 403 with the required scope named.
func TestExportSurfaces_TokenMissingScope_403(t *testing.T) {
	repo := stubToken("read:runs", "read:audit", "write:runs")
	s := New(Config{APITokenRepo: repo})
	for _, sf := range exportAuthSurfaces {
		t.Run(sf.name, func(t *testing.T) {
			rec := exportAuthGET(s, sf.path, repo.tok.PlainText)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, "insufficient_scope") {
				t.Errorf("body = %s, want insufficient_scope", body)
			}
			if !strings.Contains(body, `"required_scope":"read:audit-export"`) {
				t.Errorf("body = %s, want details.required_scope=read:audit-export", body)
			}
		})
	}
}

// A bearer token carrying read:audit-export passes the gate on every
// surface: on an unconfigured server the next stop is the 503 config
// probe, which proves the auth gate admitted the call (the gate sits
// in front of the probe).
func TestExportSurfaces_ScopedToken_PassesGate(t *testing.T) {
	repo := stubToken("read:audit-export")
	s := New(Config{APITokenRepo: repo})
	for _, sf := range exportAuthSurfaces {
		t.Run(sf.name, func(t *testing.T) {
			rec := exportAuthGET(s, sf.path, repo.tok.PlainText)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 (past the auth gate); body %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "audit_export_unconfigured") {
				t.Errorf("body = %s, want audit_export_unconfigured", rec.Body.String())
			}
		})
	}
}

// Cookie-session operators (TokenID == "", GitHub OAuth, no scope list)
// bypass scope enforcement per requireWriteScope's established
// contract. Asserted at the handler layer with an injected identity.
func TestExportSurfaces_CookieSession_BypassesScope(t *testing.T) {
	s := New(Config{})
	session := Identity{Subject: "github:op", UserID: "user-1", SessionID: "sess-1"}
	handlers := map[string]http.HandlerFunc{
		"/v0/audit/export":             s.handleAuditExport,
		"/v0/audit/export.csv":         s.handleAuditExportCSV,
		"/v0/reports/agent-changes":    s.handleAgentChangesReport,
		"/v0/reports/agent-changes.md": s.handleAgentChangesReportMarkdown,
	}
	for path, h := range handlers {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, session))
		h(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503 (cookie session past the auth gate); body %s",
				path, rec.Code, rec.Body.String())
		}
	}
}

// exportTraceStoreProbe is a tracestore.Storage whose read methods
// record being called and whose bundle bytes are distinctive. The
// export surfaces must never read the store (they serialize audit-chain
// POINTER entries only), so any Get/Stat/List call is itself a failure.
type exportTraceStoreProbe struct {
	rawBody   string
	readCalls int
}

func (p *exportTraceStoreProbe) Put(_ context.Context, _ tracestore.BundleRef, body io.Reader) error {
	_, err := io.Copy(io.Discard, body)
	return err
}

func (p *exportTraceStoreProbe) Get(_ context.Context, _ tracestore.BundleRef) (io.ReadCloser, error) {
	p.readCalls++
	return io.NopCloser(strings.NewReader(p.rawBody)), nil
}

func (p *exportTraceStoreProbe) Stat(_ context.Context, _ tracestore.BundleRef) (tracestore.Stat, error) {
	p.readCalls++
	return tracestore.Stat{Size: int64(len(p.rawBody))}, nil
}

func (p *exportTraceStoreProbe) List(_ context.Context, _ uuid.UUID) ([]tracestore.BundleRef, error) {
	p.readCalls++
	return nil, nil
}

// tracePointerEntries builds a chained pair of trace_uploaded entries
// (raw + redacted) whose payloads carry the production pointer shape
// ({variant, content_hash, size_bytes}) — the exact rows a real run
// with raw traces present exports.
func tracePointerEntries(t *testing.T, runID uuid.UUID, rawHash, redactedHash string) []*audit.Entry {
	t.Helper()
	actor := audit.ActorSystem
	subject := "system@fishhawk"
	var out []*audit.Entry
	var prev *string
	for i, variantHash := range []struct{ variant, hash string }{
		{"raw", rawHash}, {"redacted", redactedHash},
	} {
		ts := time.Date(2026, 6, 1, 10, i, 0, 0, time.UTC)
		payload := json.RawMessage(fmt.Sprintf(
			`{"variant":%q,"content_hash":%q,"size_bytes":64}`, variantHash.variant, variantHash.hash))
		h, err := audit.ComputeEntryHash(audit.HashInputs{
			RunID:        &runID,
			Timestamp:    ts,
			Category:     "trace_uploaded",
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
			RunID:        &runID,
			Timestamp:    ts,
			Category:     "trace_uploaded",
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

// The done-means invariant (E9.5/#1608, ADR-054): over a run WITH raw
// traces present, no export render path inlines a raw bundle — the
// responses carry the content-hash pointers and the trace store is
// never read.
func TestExportSurfaces_NeverInlineRawBundle(t *testing.T) {
	const (
		rawHash      = "raw0hash0deadbeef0raw0hash0deadbeef0raw0hash0deadbeef0raw0hash00"
		redactedHash = "red0hash0cafef00d0red0hash0cafef00d0red0hash0cafef00d0red0hash00"
		rawMarker    = "RAW-BUNDLE-BYTES-MUST-NEVER-LEAVE-THE-STORE"
	)
	probe := &exportTraceStoreProbe{rawBody: rawMarker}

	fr := newFakeRepo()
	runID := seedExportRun(fr, "acme/app", time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC))
	af := &exportAuditFake{perRun: map[uuid.UUID][]*audit.Entry{
		runID: tracePointerEntries(t, runID, rawHash, redactedHash),
	}}
	s := New(Config{
		AuditRepo:   af,
		RunRepo:     fr,
		SigningRepo: &exportSigningFake{},
		TraceStore:  probe,
	})

	responses := map[string]*httptest.ResponseRecorder{
		"export-json": doExport(s, ""),
		"export-csv":  doExportCSV(s, ""),
		"report-json": doReport(s, ""),
		"report-md":   doReportMarkdown(s, ""),
	}
	for name, rec := range responses {
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body %s", name, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), rawMarker) {
			t.Errorf("%s: response inlines raw bundle bytes — the export must carry pointers only", name)
		}
	}
	// The pointer rows themselves are exported (hashes present in both
	// export renders) — pointers out, bytes never.
	for _, name := range []string{"export-json", "export-csv"} {
		if !strings.Contains(responses[name].Body.String(), rawHash) {
			t.Errorf("%s: raw-variant content-hash pointer missing from the exported chain", name)
		}
	}
	if probe.readCalls != 0 {
		t.Errorf("trace store read %d time(s) by export surfaces; want 0 (pointer-only contract)", probe.readCalls)
	}
}
