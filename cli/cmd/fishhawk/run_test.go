package main

// Coverage division for the applies_to escape hatch (E53.3 / #2226, wired
// into the CLI by E53.11 / #2364).
//
// THESE tests own exactly one seam: flag-to-wire. They drive the real
// `runStart` call site and assert on the JSON the backend RECEIVES — the
// literal keys `applies_to_override` and `applies_to_override_reason`, as
// the backend parses them. The key STRINGS are asserted rather than a
// struct field on purpose: the key is the contract between the two halves,
// and it is the one thing a reader can verify from neither side alone.
//
// The far side is owned elsewhere and is deliberately NOT re-tested here (a
// CLI-side re-test would duplicate it and add a cross-module dependency this
// workspace does not otherwise have):
//   - Admission and the plan gate:
//     backend/internal/server/applies_to_plan_gate_test.go, including
//     TestAppliesToPlanGate_OverrideEntry_SuppressesRejection and
//     TestAppliesToPlanGate_OverrideAbsent_Rejects.
//   - The run_admitted_applies_to_override audit grant on the admission
//     path: the exact-count assertions in backend/internal/server (#2366).
//   - Marshalling from an already-populated CreateRunInput:
//     cli/internal/httpclient's TestStartRun_W2_AppliesToOverride_Serializes.
//
// One local rule has no wire counterpart: passing a reason WITHOUT the
// boolean is rejected here as a usage error. That is a deliberate LOCAL
// strictness, not a wire-contract divergence — the API silently ignores a
// lone reason, so the invocation can only ever be an operator mistake.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

// startRunCapture is an httptest backend that records the decoded body of
// every POST /v0/runs it receives, plus the total request count so a test
// can assert ZERO round-trips on a local usage error.
type startRunCapture struct {
	srv      *httptest.Server
	requests atomic.Int64
	body     map[string]any
}

func newStartRunCapture(t *testing.T) *startRunCapture {
	t.Helper()
	c := &startRunCapture{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.requests.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &c.body); err != nil {
			t.Errorf("backend received a body that is not JSON: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"` + uuid.NewString() + `","repo":"x/y","workflow_id":"trivial","state":"pending","runner_kind":"github_actions"}`))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// specDir writes runStartSpecYAML (from issue_fetch_test.go) into a temp
// dir so runStart's discovery finds a spec and proceeds to the round-trip.
func specDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".fishhawk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".fishhawk", "workflows.yaml"), []byte(runStartSpecYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestRunStart_AppliesToOverride_ReachesTheRequestBody is the headline
// flag-to-wire test. The hand-off from the two flags into CreateRunInput
// could be dropped entirely with every other CLI and httpclient test green:
// the httpclient's W2 test starts from an already-populated struct, and no
// cli/cmd/fishhawk test set the pair before this one. The operator would
// then pass --applies-to-override, see it accepted, and still be refused.
func TestRunStart_AppliesToOverride_ReachesTheRequestBody(t *testing.T) {
	fake := newStartRunCapture(t)
	// Surrounding whitespace is part of the fixture: the reason travels
	// UNTRIMMED so the backend stays the single normalization point.
	const reason = "  one-off backport of the 2.1 hotfix  "

	var stdout, stderr bytes.Buffer
	code := runStart([]string{
		"--repo", "x/y", "--workflow", "trivial",
		"--working-dir", specDir(t),
		"--backend-url", fake.srv.URL,
		"--token", "tok-test",
		"--applies-to-override",
		"--applies-to-override-reason", reason,
	}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("runStart exit = %d, want exitOK\nstderr: %s", code, stderr.String())
	}
	if got := fake.body["applies_to_override"]; got != true {
		t.Errorf("wire key applies_to_override = %v, want true — body: %+v", got, fake.body)
	}
	if got := fake.body["applies_to_override_reason"]; got != reason {
		t.Errorf("wire key applies_to_override_reason = %q, want %q untrimmed (the backend owns normalization)", got, reason)
	}
}

// TestRunStart_AppliesToOverride_DefaultsOff machine-enforces the omitempty
// contract: an ordinary `run start` must be byte-identical on the wire to
// its pre-change self, so NEITHER key may appear.
func TestRunStart_AppliesToOverride_DefaultsOff(t *testing.T) {
	fake := newStartRunCapture(t)

	var stdout, stderr bytes.Buffer
	code := runStart([]string{
		"--repo", "x/y", "--workflow", "trivial",
		"--working-dir", specDir(t),
		"--backend-url", fake.srv.URL,
		"--token", "tok-test",
	}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("runStart exit = %d, want exitOK\nstderr: %s", code, stderr.String())
	}
	if _, ok := fake.body["applies_to_override"]; ok {
		t.Errorf("applies_to_override present on an ordinary run start: %+v", fake.body)
	}
	if _, ok := fake.body["applies_to_override_reason"]; ok {
		t.Errorf("applies_to_override_reason present on an ordinary run start: %+v", fake.body)
	}
}

// TestRunStart_AppliesToOverride_EmptyReasonIsUsageError covers failure mode
// (a): the override with a whitespace-only reason. The ZERO-request
// assertion is the load-bearing one — a bypass that reached the backend
// unexplained is the actual defect class here. It runs WITHOUT a discovered
// spec, pinning that validation precedes discoverSpec so the refusal is
// identical from any directory.
func TestRunStart_AppliesToOverride_EmptyReasonIsUsageError(t *testing.T) {
	fake := newStartRunCapture(t)

	var stdout, stderr bytes.Buffer
	code := runStart([]string{
		"--repo", "x/y", "--workflow", "trivial",
		"--working-dir", t.TempDir(), // no .fishhawk/workflows.yaml
		"--backend-url", fake.srv.URL,
		"--token", "tok-test",
		"--applies-to-override",
		"--applies-to-override-reason", "   \t  ",
	}, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("runStart exit = %d, want exitUsage\nstderr: %s", code, stderr.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("applies-to-override-reason")) {
		t.Errorf("stderr does not name the offending flag: %s", stderr.String())
	}
	if n := fake.requests.Load(); n != 0 {
		t.Errorf("backend received %d requests, want 0 — a reasonless override must never round-trip", n)
	}
}

// TestRunStart_AppliesToOverrideReason_WithoutFlagIsUsageError covers
// failure mode (b): a reason with no --applies-to-override. Same three
// properties, same no-spec directory.
func TestRunStart_AppliesToOverrideReason_WithoutFlagIsUsageError(t *testing.T) {
	fake := newStartRunCapture(t)

	var stdout, stderr bytes.Buffer
	code := runStart([]string{
		"--repo", "x/y", "--workflow", "trivial",
		"--working-dir", t.TempDir(), // no .fishhawk/workflows.yaml
		"--backend-url", fake.srv.URL,
		"--token", "tok-test",
		"--applies-to-override-reason", "one-off backport",
	}, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("runStart exit = %d, want exitUsage\nstderr: %s", code, stderr.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("applies-to-override-reason")) {
		t.Errorf("stderr does not name the offending flag: %s", stderr.String())
	}
	if n := fake.requests.Load(); n != 0 {
		t.Errorf("backend received %d requests, want 0 — a lone reason must never round-trip", n)
	}
}
