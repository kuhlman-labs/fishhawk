package server

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/devfixtures"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
	"github.com/kuhlman-labs/fishhawk/pricing"
)

// The CROSS-BOUNDARY end-to-end for the seeded preview (E72.2 / #3326,
// closing #1874): HTTP → devfixtures → Postgres repositories → signing
// repo → trace handler → tracestore.MemStorage → recordCost →
// checkUnpricedModel / checkSpendAlert → GET /v0/audit. Every request is
// credential-less (no Authorization header, no cookie) from a loopback
// peer — exactly what the acceptance sandbox sends through the runner's
// egress proxy — so the tests also prove the anonymous-identity path
// (untenanted seeded run, OIDC no-op without a verifier, csrf pass on a
// session-less identity) holds at every hop.

// devPGDeps is the wired server plus the Deps the counterfactual arm
// re-applies a stripped scenario through.
type devPGDeps struct {
	s    *Server
	deps devfixtures.Deps
}

func newDevPGServer(t *testing.T) devPGDeps {
	t.Helper()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	deps := devfixtures.Deps{
		Runs:      runRepo,
		Artifacts: artifact.NewPostgresRepository(pool),
		Audit:     audit.NewPostgresRepository(pool),
		Approvals: approval.NewPostgresRepository(pool),
		Now:       time.Now,
	}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      runRepo,
		SigningRepo:  signing.NewPostgresRepository(pool),
		AuditRepo:    deps.Audit.(audit.Repository),
		ApprovalRepo: deps.Approvals.(approval.Repository),
		ArtifactRepo: deps.Artifacts.(artifact.Repository),
		TraceStore:   tracestore.NewMem(),
		DevFixtures:  devfixtures.NewApplier(deps),
	})
	return devPGDeps{s: s, deps: deps}
}

// seedTraceUploadTarget POSTs the trace-upload-target scenario and
// returns the minted run id and plan stage id.
func seedTraceUploadTarget(t *testing.T, s *Server) (runID, stageID uuid.UUID) {
	t.Helper()
	w := devRequest(t, s, http.MethodPost, "/v0/dev/fixtures", []byte(`{"scenario":"trace-upload-target"}`), devLoopbackPeer, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /v0/dev/fixtures: status = %d, want 201\n%s", w.Code, w.Body.String())
	}
	var res devfixtures.Result
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rr, ok := res.Runs["target-run"]
	if !ok {
		t.Fatalf("result carries no target-run: %+v", res)
	}
	st, ok := rr.Stages["plan"]
	if !ok {
		t.Fatalf("target-run carries no plan stage: %+v", rr)
	}
	return rr.ID, st
}

// issueDevSigningKey drives POST /v0/runs/{id}/signing-key
// credential-less and returns the base64 private_key.
func issueDevSigningKey(t *testing.T, s *Server, runID uuid.UUID) string {
	t.Helper()
	w := devRequest(t, s, http.MethodPost, "/v0/runs/"+runID.String()+"/signing-key", nil, devLoopbackPeer, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST signing-key: status = %d, want 201\n%s", w.Code, w.Body.String())
	}
	var got struct {
		PrivateKey string `json:"private_key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.PrivateKey == "" {
		t.Fatal("private_key empty")
	}
	return got.PrivateKey
}

// devSignBody drives POST /v0/dev/sign and returns the hex signature.
func devSignBody(t *testing.T, s *Server, privateKey string, body []byte) string {
	t.Helper()
	w := devRequest(t, s, http.MethodPost, "/v0/dev/sign", body, devLoopbackPeer,
		map[string]string{devSignPrivateKeyHeader: privateKey, "Content-Type": "application/octet-stream"})
	if w.Code != http.StatusOK {
		t.Fatalf("POST /v0/dev/sign: status = %d, want 200\n%s", w.Code, w.Body.String())
	}
	var got devSignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := hex.DecodeString(got.Signature); err != nil {
		t.Fatalf("signature not hex: %v", err)
	}
	return got.Signature
}

// uploadRawTrace POSTs a raw-variant bundle signed via the dev sign
// route and returns the recorder.
func uploadRawTrace(t *testing.T, s *Server, runID, stageID uuid.UUID, privateKey string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	sig := devSignBody(t, s, privateKey, body)
	path := "/v0/runs/" + runID.String() + "/trace?stage_id=" + stageID.String() + "&variant=raw"
	return devRequest(t, s, http.MethodPost, path, body, devLoopbackPeer,
		map[string]string{"X-Fishhawk-Signature": sig, "Content-Type": "application/gzip"})
}

// listAuditByCategory reads GET /v0/audit?category=<cat> credential-less
// and returns the items.
func listAuditByCategory(t *testing.T, s *Server, category string) []auditEntryResponse {
	t.Helper()
	w := devRequest(t, s, http.MethodGet, "/v0/audit?category="+category+"&limit=500", nil, devLoopbackPeer, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v0/audit?category=%s: status = %d\n%s", category, w.Code, w.Body.String())
	}
	var got struct {
		Items []auditEntryResponse `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got.Items
}

func manifestBundle(t *testing.T, runID, stageID uuid.UUID, model string, inTok, outTok int, generatedAt string) []byte {
	t.Helper()
	return packManifestBundle(t, bundle.Manifest{
		BundleSchema: "trace-bundle-v0",
		RunID:        runID.String(),
		StageID:      stageID.String(),
		Agent:        "claude-code",
		Model:        model,
		InputTokens:  inTok,
		OutputTokens: outTok,
		GeneratedAt:  generatedAt,
	})
}

// TestDevFixtures_TraceUploadPath_EmitsUnpricedModelAlert_EndToEnd:
// seed → signing-key → dev sign → raw trace upload of an UNPRICED model
// → cost_recorded row present → exactly ONE unpriced_model_alert naming
// the model → STILL one after a second same-model upload (the
// once-per-window dedup). The seed-nothing arm: a trace POST on a run
// id nothing minted is refused and emits no alert.
func TestDevFixtures_TraceUploadPath_EmitsUnpricedModelAlert_EndToEnd(t *testing.T) {
	d := newDevPGServer(t)
	s := d.s
	const model = "acme-unpriced-9000"
	if _, ok := pricing.Cost(model, 1, 1); ok {
		t.Fatalf("pricing prices %q — the fixture model must be unpriced", model)
	}

	runID, stageID := seedTraceUploadTarget(t, s)
	priv := issueDevSigningKey(t, s, runID)

	w := uploadRawTrace(t, s, runID, stageID, priv, manifestBundle(t, runID, stageID, model, 1000, 100, "first"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("first upload: status = %d, want 202\n%s", w.Code, w.Body.String())
	}

	costRows := listAuditByCategory(t, s, "cost_recorded")
	var sawUpload bool
	for _, e := range costRows {
		var p struct {
			Model      string `json:"model"`
			KnownModel bool   `json:"known_model"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		if p.Model == model && e.RunID != nil && *e.RunID == runID {
			sawUpload = true
			if p.KnownModel {
				t.Errorf("cost_recorded for %q carries known_model=true", model)
			}
		}
	}
	if !sawUpload {
		t.Fatalf("no cost_recorded row for %q on run %s among %d rows", model, runID, len(costRows))
	}

	alerts := listAuditByCategory(t, s, "unpriced_model_alert")
	assertOneUnpricedAlert := func(when string) {
		t.Helper()
		if len(alerts) != 1 {
			t.Fatalf("%s: unpriced_model_alert rows = %d, want exactly 1", when, len(alerts))
		}
		var p struct {
			Unpriced []string `json:"unpriced_models"`
		}
		if err := json.Unmarshal(alerts[0].Payload, &p); err != nil {
			t.Fatalf("decode alert payload: %v", err)
		}
		if len(p.Unpriced) != 1 || p.Unpriced[0] != model {
			t.Fatalf("%s: unpriced_models = %v, want [%s]", when, p.Unpriced, model)
		}
		if alerts[0].RunID == nil || *alerts[0].RunID != runID {
			t.Fatalf("%s: alert run_id = %v, want %s", when, alerts[0].RunID, runID)
		}
	}
	assertOneUnpricedAlert("after first upload")

	// Second same-model upload (a distinct bundle): still exactly one alert.
	w = uploadRawTrace(t, s, runID, stageID, priv, manifestBundle(t, runID, stageID, model, 2000, 200, "second"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("second upload: status = %d, want 202\n%s", w.Code, w.Body.String())
	}
	alerts = listAuditByCategory(t, s, "unpriced_model_alert")
	assertOneUnpricedAlert("after second upload (dedup)")

	t.Run("seed_nothing_refused", func(t *testing.T) {
		unseeded := uuid.New()
		body := manifestBundle(t, unseeded, uuid.New(), model, 10, 10, "unseeded")
		w := uploadRawTrace(t, s, unseeded, uuid.New(), priv, body)
		if w.Code != http.StatusNotFound {
			t.Fatalf("unseeded run upload: status = %d, want 404 (no signing key issued)\n%s", w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w); code != "signing_key_not_found" {
			t.Errorf("code = %q, want signing_key_not_found", code)
		}
		if got := listAuditByCategory(t, s, "unpriced_model_alert"); len(got) != 1 {
			t.Errorf("unpriced_model_alert rows = %d after the refused upload, want still 1", len(got))
		}
	})
}

// TestDevFixtures_TraceUploadPath_EmitsSpendAlert_EndToEnd: the seeded
// four-row backdated baseline plus a 1,000,000-input-token
// claude-opus-4-8 upload trips exactly one spend_alert naming the
// model. Control arm: a 200-input-token bundle first trips nothing.
// Counterfactual arm: the same scenario with its cost_recorded rows
// STRIPPED before Apply, then the same big upload → NO spend_alert —
// proving the seeded baseline is what makes the alert drivable.
func TestDevFixtures_TraceUploadPath_EmitsSpendAlert_EndToEnd(t *testing.T) {
	const model = "claude-opus-4-8"
	const bigIn = 1_000_000
	bigUSD, ok := pricing.Cost(model, bigIn, 0)
	if !ok || bigUSD < 1 {
		t.Fatalf("pricing.Cost(%q, %d) ok=%v usd=%v — the big upload must price at >= $1 to dwarf the $0.001 baseline", model, bigIn, ok, bigUSD)
	}

	t.Run("seeded_baseline_trips", func(t *testing.T) {
		d := newDevPGServer(t)
		s := d.s
		runID, stageID := seedTraceUploadTarget(t, s)
		priv := issueDevSigningKey(t, s, runID)

		// Control: a small upload in the seed hour is on the order of the
		// baseline and trips nothing.
		w := uploadRawTrace(t, s, runID, stageID, priv, manifestBundle(t, runID, stageID, model, 200, 0, "small"))
		if w.Code != http.StatusAccepted {
			t.Fatalf("control upload: status = %d, want 202\n%s", w.Code, w.Body.String())
		}
		if got := listAuditByCategory(t, s, "spend_alert"); len(got) != 0 {
			t.Fatalf("control: spend_alert rows = %d after a 200-token upload, want 0: %s", len(got), got[0].Payload)
		}

		w = uploadRawTrace(t, s, runID, stageID, priv, manifestBundle(t, runID, stageID, model, bigIn, 0, "big"))
		if w.Code != http.StatusAccepted {
			t.Fatalf("big upload: status = %d, want 202\n%s", w.Code, w.Body.String())
		}
		alerts := listAuditByCategory(t, s, "spend_alert")
		if len(alerts) != 1 {
			t.Fatalf("spend_alert rows = %d, want exactly 1", len(alerts))
		}
		var p struct {
			TriggeringModel string  `json:"triggering_model"`
			PriorHours      int     `json:"prior_hours"`
			Ratio           float64 `json:"ratio"`
		}
		if err := json.Unmarshal(alerts[0].Payload, &p); err != nil {
			t.Fatalf("decode spend_alert payload: %v", err)
		}
		if p.TriggeringModel != model {
			t.Errorf("triggering_model = %q, want %q", p.TriggeringModel, model)
		}
		if p.PriorHours < 1 {
			t.Errorf("prior_hours = %d, want >= 1 (the seeded baseline)", p.PriorHours)
		}
		if p.Ratio <= 1 {
			t.Errorf("ratio = %v, want > 1", p.Ratio)
		}
		if alerts[0].RunID == nil || *alerts[0].RunID != runID {
			t.Errorf("alert run_id = %v, want %s", alerts[0].RunID, runID)
		}
	})

	t.Run("stripped_baseline_does_not_trip", func(t *testing.T) {
		d := newDevPGServer(t)
		s := d.s
		sc, err := devfixtures.Load("trace-upload-target")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(sc.Audit) != 4 {
			t.Fatalf("scenario carries %d audit rows, want the four-row baseline", len(sc.Audit))
		}
		sc.Audit = nil
		res, err := devfixtures.Apply(t.Context(), d.deps, sc)
		if err != nil {
			t.Fatalf("Apply stripped scenario: %v", err)
		}
		runID := res.Runs["target-run"].ID
		stageID := res.Runs["target-run"].Stages["plan"]
		if got := listAuditByCategory(t, s, "cost_recorded"); len(got) != 0 {
			t.Fatalf("stripped scenario left %d cost_recorded rows", len(got))
		}

		priv := issueDevSigningKey(t, s, runID)
		w := uploadRawTrace(t, s, runID, stageID, priv, manifestBundle(t, runID, stageID, model, bigIn, 0, "big"))
		if w.Code != http.StatusAccepted {
			t.Fatalf("big upload: status = %d, want 202\n%s", w.Code, w.Body.String())
		}
		if got := listAuditByCategory(t, s, "spend_alert"); len(got) != 0 {
			t.Fatalf("spend_alert rows = %d without a baseline, want 0 — the seeded baseline was not load-bearing: %s", len(got), got[0].Payload)
		}
	})
}

// TestShipTrace_UnconfiguredWithoutTraceStore_503 pins the #1874
// symptom: a server carrying signing + audit but NO TraceStore answers
// 503 trace_upload_unconfigured on a seeded run — the exact state a
// preview without --dev-fixtures (and without FISHHAWKD_S3_BUCKET) is
// in. It stays as the visible control for the in-memory store wiring.
func TestShipTrace_UnconfiguredWithoutTraceStore_503(t *testing.T) {
	d := newDevPGServer(t)
	// Rebuild the server from the same repositories with TraceStore nil.
	cfg := d.s.cfg
	cfg.TraceStore = nil
	s := New(cfg)

	runID, stageID := seedTraceUploadTarget(t, s)
	priv := issueDevSigningKey(t, s, runID)
	w := uploadRawTrace(t, s, runID, stageID, priv, manifestBundle(t, runID, stageID, "claude-opus-4-8", 100, 0, "x"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 with TraceStore nil\n%s", w.Code, w.Body.String())
	}
	if code := decodeErrorCode(t, w); code != "trace_upload_unconfigured" {
		t.Errorf("code = %q, want trace_upload_unconfigured", code)
	}
}
