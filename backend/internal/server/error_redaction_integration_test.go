package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/mcpserver"
)

// updateGolden regenerates the cross-module 5xx-envelope golden consumed by the
// CLI httpclient test. Run with:
//
//	go test ./backend/internal/server/ -run TestErrorRedaction_ServerToMCPConsumer -update
//
// from the repo root (or any cwd — the golden path is resolved from go.work, not
// a relative ../ chain, so it never goes stale silently: CONDITION 3).
var updateGolden = flag.Bool("update", false, "regenerate the cross-module 5xx envelope golden")

// Planted internals: substrings that stand in for the classes go-github's
// *github.ErrorResponse.Error() and the storage layer can embed in a raw cause
// — a GHES hostname, a pgx/Postgres driver fragment, and a payload fragment.
// None may reach the caller.
const (
	plantedHost    = "ghe.internal.example.com"
	plantedDriver  = "pgx: SQLSTATE 42P01 relation does not exist"
	plantedPayload = "SECRET_PAYLOAD_TOKEN_abc123"
)

// TestErrorRedaction_ServerToMCPConsumer is the cross-boundary / producer-to-
// consumer test (FIX 3). It stands up the REAL server.Server on httptest with a
// work-item provider stubbed to fail with a cause carrying planted internals,
// then drives that live endpoint through the SHIPPED mcpserver
// fishhawk_file_issue consumer and asserts on the string the operator actually
// sees: it carries the code and a non-empty error_ref equal to the response's
// X-Request-ID, and it contains NONE of the planted internals. The same test
// golden-captures the raw 5xx envelope for the CLI half.
func TestErrorRedaction_ServerToMCPConsumer(t *testing.T) {
	fp := &fakeWorkProvider{fileErr: errors.New(
		"workmgmt/github: create issue: POST https://" + plantedHost +
			"/api/v3/repos/o/n/issues: 500 " + plantedPayload + " [" + plantedDriver + "]")}
	registerFakeProvider(t, fp)

	s, token := mcpTestServer(t, Config{})
	// RESIDUAL 2 (E67.29 / #2631): capture the X-Request-ID the server minted for
	// EACH request, so the consumer assertion below can be bound to the ref of
	// the CONSUMER's own request rather than merely being non-empty.
	var refMu sync.Mutex
	var lastRef string
	ts := httptest.NewServer(captureRequestID(s.Handler(), &refMu, &lastRef))
	defer ts.Close()

	// (1) Raw HTTP call: capture the shipped 5xx envelope bytes, prove error_ref
	// equals the X-Request-ID header, and prove no planted internal leaks into
	// the body. A fixed X-Request-ID makes the golden deterministic across
	// -update regenerations.
	reqBody := []byte(`{"repo":"kuhlman-labs/fishhawk","type":"chore","summary":"x","title_vars":{"epic":"22","n":"7"}}`)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v0/work-items", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "e2e-fixed-ref")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("raw POST: %v", err)
	}
	rawEnvelope, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", resp.StatusCode, rawEnvelope)
	}
	var env errorEnvelope
	if err := json.Unmarshal(rawEnvelope, &env); err != nil {
		t.Fatalf("decode envelope: %v (body=%s)", err, rawEnvelope)
	}
	if env.Error.Code != "work_item_filing_failed" {
		t.Errorf("code = %q, want work_item_filing_failed", env.Error.Code)
	}
	if env.Error.ErrorRef == "" {
		t.Error("error_ref must be set on a 5xx body")
	}
	if got := resp.Header.Get("X-Request-ID"); got != env.Error.ErrorRef {
		t.Errorf("error_ref %q != X-Request-ID header %q", env.Error.ErrorRef, got)
	}
	assertNoPlanted(t, "raw 5xx envelope", string(rawEnvelope))

	// Golden capture for the CLI consumer, path-resolved from go.work.
	//
	// RESIDUAL 1 (E67.29 / #2631): the golden is now COMPARED on a normal run,
	// not merely rewritten under -update. Go's internal-package rule bars cli/
	// from importing backend/internal/server, so this artifact is the only thing
	// standing in for a live import on the CLI side — and an artifact that is
	// only ever written is a hand-mirror the two halves can agree on while the
	// server silently diverges. The envelope is deterministic (fixed
	// X-Request-ID, no details on this 502, json.Encoder emits struct fields in
	// declaration order with a trailing newline), so the compare is stable; a
	// future change that adds a detail key to this 502 SHOULD fail here.
	goldenPath := filepath.Join(repoRoot(t), "cli", "internal", "httpclient", "testdata", "5xx_envelope.golden.json")
	if *updateGolden {
		if err := os.WriteFile(goldenPath, rawEnvelope, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", goldenPath, err)
		}
		t.Logf("regenerated golden %s", goldenPath)
	} else {
		want, err := os.ReadFile(goldenPath)
		if err != nil {
			t.Fatalf("read golden %s: %v\nrefresh with: go test ./backend/internal/server/ -run TestErrorRedaction_ServerToMCPConsumer -update", goldenPath, err)
		}
		if !bytes.Equal(want, rawEnvelope) {
			t.Errorf("cross-module 5xx golden is stale.\n golden: %s\n  live: %s\nrefresh with: go test ./backend/internal/server/ -run TestErrorRedaction_ServerToMCPConsumer -update",
				want, rawEnvelope)
		}
	}

	// (2) Shipped consumer: drive the live endpoint through the real mcpserver
	// fishhawk_file_issue tool over the SDK's in-memory transport.
	ctx := context.Background()
	mcpSrv := mcpserver.NewServer(mcpserver.Config{BackendURL: ts.URL, APIToken: token})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := mcpSrv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("mcp server connect: %v", err)
	}
	defer func() { _ = ss.Close() }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "redaction-e2e", Version: "0"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("mcp client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	result, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_file_issue",
		Arguments: map[string]any{
			"repo": "kuhlman-labs/fishhawk", "type": "chore", "summary": "x",
			"title_vars": map[string]any{"epic": "22", "n": "7"},
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_file_issue: %v", err)
	}
	if !result.IsError {
		t.Fatalf("want a tool error on a 502, got success: %+v", result.StructuredContent)
	}
	operatorVisible := toolText(result)
	if !strings.Contains(operatorVisible, "work_item_filing_failed") {
		t.Errorf("operator-visible string missing the code: %q", operatorVisible)
	}
	// RESIDUAL 2 (E67.29 / #2631): the ref the operator sees must be THAT
	// request's X-Request-ID, not a placeholder and not merely non-empty. The
	// consumer call is a SEPARATE request from the raw one above, so the server
	// mints it a fresh uuid; asserting it differs from the raw call's fixed
	// "e2e-fixed-ref" proves the assertion is bound to the consumer's own
	// request. Previously only `contains "error_ref="` was asserted, which any
	// placeholder would satisfy.
	refMu.Lock()
	consumerRef := lastRef
	refMu.Unlock()
	if consumerRef == "" {
		t.Fatal("capture middleware recorded no X-Request-ID for the consumer request")
	}
	if consumerRef == "e2e-fixed-ref" {
		t.Fatalf("captured ref %q is the RAW call's fixed id — the consumer request did not mint its own", consumerRef)
	}
	if !strings.Contains(operatorVisible, "error_ref="+consumerRef) {
		t.Errorf("operator-visible string must carry THIS request's ref %q: %q", consumerRef, operatorVisible)
	}
	// The "server log" pointer is unique to file_issue.go's error_ref extraction
	// (apiError.Error() alone already renders "(error_ref=…)"), so asserting it
	// makes deleting that extraction an attainable counterfactual rather than a
	// vacuous one.
	if !strings.Contains(operatorVisible, "server log") {
		t.Errorf("operator-visible string missing the where-to-look pointer: %q", operatorVisible)
	}
	assertNoPlanted(t, "operator-visible tool error", operatorVisible)
}

// captureRequestID records the X-Request-ID the requestID middleware set on each
// response, so a test can bind an assertion to the ref of a SPECIFIC request
// (RESIDUAL 2, E67.29 / #2631). It reads the header AFTER next.ServeHTTP returns,
// which is when the middleware has set it; the mutex covers the httptest server's
// per-request goroutines.
func captureRequestID(next http.Handler, mu *sync.Mutex, dst *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		mu.Lock()
		*dst = w.Header().Get("X-Request-ID")
		mu.Unlock()
	})
}

func assertNoPlanted(t *testing.T, where, s string) {
	t.Helper()
	for _, planted := range []string{plantedHost, plantedDriver, plantedPayload} {
		if strings.Contains(s, planted) {
			t.Errorf("%s leaked a planted internal %q: %s", where, planted, s)
		}
	}
}

func toolText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// repoRoot walks up from the working directory to the directory containing
// go.work, so a cross-module golden path is pinned robustly rather than by a
// brittle relative ../ chain (CONDITION 3).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.work not found walking up from working directory")
		}
		dir = parent
	}
}
