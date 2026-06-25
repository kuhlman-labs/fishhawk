package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/securityscan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// This file is the wave-2 end-to-end seam test for the code-scanning gate
// (#1096). The per-slice unit tests each prove ONE layer against an
// isolated fake: the webhook ingest (codescanning_test.go), the merge gate
// (auditcomplete_test.go), the REST surface (runs_get_test.go), the prompt
// section (prompt_test.go), and the typed REST decode (githubclient). None
// of them crosses a slice boundary, so a payload-shape DRIFT between the
// writer (the webhook records securityScanAuditPayload) and the three
// readers (the gate decodes {findings}, the REST run response decodes it,
// the MCP run-status decodes it off the audit endpoint) would pass every
// unit test yet break in production.
//
// This test wires ONE shared audit store and drives the genuine signed
// HTTP webhook endpoint through the real *githubclient REST decode, then
// reads that SAME store back through the real gate (auditcomplete.Compute)
// and the real REST surfaces (GET /v0/runs/{id} and the
// GET /v0/runs/{id}/audit feed the MCP run-status consumes). It asserts the
// done-means of #1096 end-to-end: a new high-severity finding surfaces at
// the review gate (StateFail, security_findings_unresolved) and on the run
// response — NOT first as a blocked required check at merge — and a clean
// re-scan after a stage_fixup_triggered clears the gate.

// --- shared chained audit store ------------------------------------------

// codescanChainAudit is an in-memory audit.Repository that computes REAL
// chained hashes the way the production AppendChained does, so the gate's
// chain-verify rule (auditcomplete Rule 4) agrees with the entries the
// webhook ingest writes. It is the SINGLE store wired into the Server
// config (the webhook ingest writes here) AND auditcomplete.Deps (the gate
// reads here), so the cross-slice payload contract is exercised, not faked
// twice. Only the methods this seam touches are implemented; the rest are
// inherited from the embedded interface (nil) and would panic if reached —
// a guard that the test stays on the intended path.
type codescanChainAudit struct {
	audit.Repository
	mu      sync.Mutex
	entries []*audit.Entry
}

// AppendChained mirrors the production integrity layer: compute the
// canonical per-run-chained hash (prev → entry), assign a monotonic
// sequence, append. Used by both the webhook ingest (via s.cfg.AuditRepo)
// and the test's own chain seeding, so every entry the gate verifies was
// produced by one append path.
func (a *codescanChainAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	var prev *string
	for i := len(a.entries) - 1; i >= 0; i-- {
		e := a.entries[i]
		if e.RunID != nil && *e.RunID == p.RunID {
			ph := e.EntryHash
			prev = &ph
			break
		}
	}
	r := p.RunID
	hash, err := audit.ComputeEntryHash(audit.HashInputs{
		RunID:        &r,
		StageID:      p.StageID,
		Timestamp:    p.Timestamp,
		Category:     p.Category,
		ActorKind:    p.ActorKind,
		ActorSubject: p.ActorSubject,
		Payload:      p.Payload,
		PrevHash:     prev,
	})
	if err != nil {
		return nil, err
	}
	entry := &audit.Entry{
		ID:           uuid.New(),
		Sequence:     int64(len(a.entries) + 1),
		RunID:        &r,
		StageID:      p.StageID,
		Timestamp:    p.Timestamp,
		Category:     p.Category,
		ActorKind:    p.ActorKind,
		ActorSubject: p.ActorSubject,
		Payload:      p.Payload,
		PrevHash:     prev,
		EntryHash:    hash,
	}
	a.entries = append(a.entries, entry)
	return entry, nil
}

func (a *codescanChainAudit) ListForRun(_ context.Context, runID uuid.UUID) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []*audit.Entry{}
	for _, e := range a.entries {
		if e.RunID != nil && *e.RunID == runID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (a *codescanChainAudit) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []*audit.Entry{}
	for _, e := range a.entries {
		if e.Category != category {
			continue
		}
		if e.RunID != nil && *e.RunID == runID {
			out = append(out, e)
		}
	}
	return out, nil
}

// seedChain appends one chain entry directly (the trace bundles every
// non-review stage must ship, and the fix-up marker the gate floors on),
// keeping the synthetic chain identical in shape to production.
func (a *codescanChainAudit) seedChain(t *testing.T, runID uuid.UUID, stageID *uuid.UUID, category string, payload []byte) {
	t.Helper()
	actor := audit.ActorSystem
	if _, err := a.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runID,
		StageID:   stageID,
		Timestamp: time.Now().UTC(),
		Category:  category,
		ActorKind: &actor,
		Payload:   payload,
	}); err != nil {
		t.Fatalf("seed chain entry %q: %v", category, err)
	}
}

func (a *codescanChainAudit) countCategory(runID uuid.UUID, category string) int {
	entries, _ := a.ListForRunByCategory(context.Background(), runID, category)
	return len(entries)
}

// --- token provider stub -------------------------------------------------

// codescanTokenProvider satisfies githubapp.TokenProvider so a real
// *githubclient.Client can authenticate against the httptest GitHub server.
type codescanTokenProvider struct{}

func (codescanTokenProvider) Token(_ context.Context, _ int64) (string, error) {
	return "test-installation-token", nil
}

// --- fake GitHub code-scanning REST endpoint -----------------------------

// codeScanningGitHub stands up an httptest server that serves the
// code-scanning alerts REST endpoint the real *githubclient reads. The
// returned alert set is swappable so the dirty scan and the post-fix-up
// clean re-scan can be driven through the SAME client without rebuilding it.
type codeScanningGitHub struct {
	mu     sync.Mutex
	alerts []map[string]any
}

func (g *codeScanningGitHub) set(alerts []map[string]any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.alerts = alerts
}

func (g *codeScanningGitHub) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/app/code-scanning/alerts", func(w http.ResponseWriter, _ *http.Request) {
		g.mu.Lock()
		alerts := g.alerts
		g.mu.Unlock()
		if alerts == nil {
			alerts = []map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(alerts); err != nil {
			t.Errorf("encode alerts: %v", err)
		}
	})
	return mux
}

// highSeverityAlert builds one open, high-severity code-scanning alert JSON
// in GitHub's REST shape, located on the given repo-relative path so it
// intersects the run's implement diff.
func highSeverityAlert(number int, path string, line int, headSHA string) map[string]any {
	return map[string]any{
		"number":   number,
		"state":    "open",
		"html_url": fmt.Sprintf("https://github.com/octo/app/security/code-scanning/%d", number),
		"rule": map[string]any{
			"id":                      "go/sql-injection",
			"name":                    "Database query built from user-controlled sources",
			"description":             "Building a SQL query from user-controlled sources is vulnerable to insertion of malicious SQL code.",
			"security_severity_level": "high",
		},
		"tool": map[string]any{"name": "CodeQL"},
		"most_recent_instance": map[string]any{
			"ref":        "refs/pull/42/merge",
			"commit_sha": headSHA,
			"location": map[string]any{
				"path":       path,
				"start_line": line,
			},
		},
	}
}

// --- the end-to-end test -------------------------------------------------

func TestCodeScanningAlert_EndToEnd_WebhookGateSurface(t *testing.T) {
	const (
		prNumber  = 42
		diffFile  = "backend/internal/server/codescanning.go"
		dirtySHA  = "feedface00000000000000000000000000000001"
		cleanSHA  = "0badf00d00000000000000000000000000000002"
		alertLine = 142
	)

	// Shared backing stores: the run + artifact fakes (reused from
	// codescanning_test.go) and the one chained audit store both the
	// webhook ingest and the gate read/write.
	rr := &codeScanRunRepo{}
	ar := &codeScanArtifactRepo{}
	au := &codescanChainAudit{}

	rn := seedRunWithPlan(t, rr, ar, []string{diffFile})
	runID := rn.ID

	// The plan stage must be terminal with its trace bundles shipped so the
	// gate reaches Rule 7 (security findings) cleanly rather than tripping
	// the mid-flight / missing-trace rules — isolating the assertion to the
	// security signal.
	planStageID := rr.stages[runID][0].ID
	rr.stages[runID][0].State = run.StageStateSucceeded
	au.seedChain(t, runID, &planStageID, "trace_uploaded", []byte(`{"variant":"raw"}`))
	au.seedChain(t, runID, &planStageID, "trace_uploaded", []byte(`{"variant":"redacted"}`))

	// Real GitHub code-scanning endpoint + real typed client.
	gh := &codeScanningGitHub{}
	gh.set([]map[string]any{highSeverityAlert(1, diffFile, alertLine, dirtySHA)})
	ghSrv := httptest.NewServer(gh.handler(t))
	defer ghSrv.Close()
	client := &githubclient.Client{
		BaseURL: ghSrv.URL,
		Tokens:  codescanTokenProvider{},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}

	store := webhook.NewMemoryStore(0)
	s := New(Config{
		Addr:                "127.0.0.1:0",
		GitHubWebhookSecret: []byte(testSecret),
		WebhookDeliveries:   store,
		RunRepo:             rr,
		ArtifactRepo:        ar,
		AuditRepo:           au,
		GitHub:              client,
	})

	gateDeps := auditcomplete.Deps{Runs: rr, Artifacts: ar, Audit: au}

	// --- deliver the dirty code_scanning_alert over the real HTTP seam ---
	body := codeScanPayload(prNumber, dirtySHA)
	w := postWebhook(t, s, map[string]string{
		"X-GitHub-Event":      "code_scanning_alert",
		"X-GitHub-Delivery":   "11111111-1111-1111-1111-111111111111",
		"X-Hub-Signature-256": sign(body),
		"Content-Type":        "application/json",
	}, body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("dirty delivery status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	// 1) The audit entry lands exactly once.
	if n := au.countCategory(runID, securityscan.AuditCategorySecurityFindings); n != 1 {
		t.Fatalf("securityscan audit entries after dirty delivery = %d, want 1", n)
	}

	// 2) The gate fails on the cross-slice contract MissingKind.
	state, missing, err := auditcomplete.Compute(context.Background(), runID, gateDeps)
	if err != nil {
		t.Fatalf("Compute (dirty): unexpected error: %v", err)
	}
	if state != stagecheck.StateFail {
		t.Fatalf("gate state (dirty) = %q, want fail; missing=%+v", state, missing)
	}
	if !hasMissingKind(missing, auditcomplete.MissingSecurityFindings) {
		t.Fatalf("gate missing (dirty) = %+v, want %q", missing, auditcomplete.MissingSecurityFindings)
	}
	if string(auditcomplete.MissingSecurityFindings) != securityscan.GateMissingKind {
		t.Fatalf("gate MissingKind drift: %q != contract %q",
			auditcomplete.MissingSecurityFindings, securityscan.GateMissingKind)
	}

	// 3) The finding surfaces on the REST run response (the shape the MCP
	//    run-status mirrors), not first at merge.
	resp, _ := getRunResponse(t, s, runID)
	if len(resp.SecurityFindings) != 1 {
		t.Fatalf("run response security_findings = %d, want 1: %+v", len(resp.SecurityFindings), resp.SecurityFindings)
	}
	if f := resp.SecurityFindings[0]; f.RuleID != "go/sql-injection" || f.Path != diffFile || f.Severity != securityscan.SeverityHigh {
		t.Errorf("surfaced finding = %+v, want high go/sql-injection on %s", f, diffFile)
	}

	// 4) The MCP run-status consumes the finding off the audit feed
	//    (GET /v0/runs/{id}/audit?category=...). Decode it exactly the way
	//    the MCP tool does to prove that surface too.
	if got := mcpSecurityFindings(t, s, runID); len(got) != 1 || got[0].Number != 1 {
		t.Fatalf("MCP audit-feed findings = %+v, want the high finding #1", got)
	}

	// --- a fix-up runs, then a clean re-scan ----------------------------
	au.seedChain(t, runID, nil, CategoryStageFixupTriggered, []byte(`{"pass_ordinal":1}`))

	gh.set([]map[string]any{}) // the high finding is resolved; no open alerts
	body2 := codeScanPayload(prNumber, cleanSHA)
	w2 := postWebhook(t, s, map[string]string{
		"X-GitHub-Event":      "code_scanning_alert",
		"X-GitHub-Delivery":   "22222222-2222-2222-2222-222222222222",
		"X-Hub-Signature-256": sign(body2),
		"Content-Type":        "application/json",
	}, body2)
	if w2.Code != http.StatusAccepted {
		t.Fatalf("clean delivery status = %d, want 202:\n%s", w2.Code, w2.Body.String())
	}

	// A clean re-scan above the fix-up floor records no NEW entry (absence
	// already means not-blocking); the dirty entry remains for the trail.
	if n := au.countCategory(runID, securityscan.AuditCategorySecurityFindings); n != 1 {
		t.Fatalf("securityscan audit entries after clean re-scan = %d, want 1 (no noise entry)", n)
	}

	// 5) The gate clears: the dirty entry is now below the latest
	//    stage_fixup_triggered floor, so a clean re-scan clears the gate.
	state2, missing2, err := auditcomplete.Compute(context.Background(), runID, gateDeps)
	if err != nil {
		t.Fatalf("Compute (clean): unexpected error: %v", err)
	}
	if hasMissingKind(missing2, auditcomplete.MissingSecurityFindings) {
		t.Fatalf("gate still holds security finding after fix-up floor: %+v", missing2)
	}
	if state2 != stagecheck.StatePass {
		t.Fatalf("gate state (clean) = %q, want pass; missing=%+v", state2, missing2)
	}
}

// --- helpers -------------------------------------------------------------

func hasMissingKind(missing []auditcomplete.MissingItem, kind auditcomplete.MissingKind) bool {
	for _, m := range missing {
		if m.Kind == kind {
			return true
		}
	}
	return false
}

// mcpSecurityFindings reads the run's audit feed exactly the way the MCP
// run-status tool's securityFindingsFor does — GET the
// implement_security_findings category, take the newest entry, decode its
// payload as the cross-slice {findings:[...]} shape — so this seam test
// covers the MCP surface without importing the fishhawk-mcp main package.
func mcpSecurityFindings(t *testing.T, s *Server, runID uuid.UUID) []securityscan.Finding {
	t.Helper()
	url := fmt.Sprintf("/v0/runs/%s/audit?category=%s", runID, securityscan.AuditCategorySecurityFindings)
	req := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET audit feed status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var page struct {
		Items []struct {
			Category string          `json:"category"`
			Payload  json.RawMessage `json:"payload"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode audit page: %v", err)
	}
	if len(page.Items) == 0 {
		return nil
	}
	newest := page.Items[len(page.Items)-1]
	var payload struct {
		Findings []securityscan.Finding `json:"findings"`
	}
	if err := json.Unmarshal(newest.Payload, &payload); err != nil {
		t.Fatalf("decode securityscan payload off audit feed: %v", err)
	}
	return payload.Findings
}
