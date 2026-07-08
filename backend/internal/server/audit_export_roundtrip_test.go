package server

// audit_export_roundtrip_test.go is the cross-boundary
// external-verification test for E9 (#1607): it proves that an export
// produced by the REAL `fishhawk` CLI binary, following the
// header-based continuation over the REAL server handler stack against
// REAL Postgres-persisted chained+signed audit data, verifies with the
// REAL `fishhawk-verify` binary at exit 0 — and that a single tampered
// entry_hash flips the verdict to exit 1 / kind=hash_mismatch, proving
// the test can fail.
//
// This is the mechanical form of the §13 done-means: seed → export →
// verify, with every layer this change spans crossed for real (Postgres
// via pgtest → server.New over httptest → the built CLI's merge path
// with --limit 1 forcing multi-page assembly → the verifier's
// ParseExport + VerifyExport). It uses pgtest.NewPool (the shared-
// container discipline per AGENTS.md), never a hand-rolled tcpostgres.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

func TestAuditExport_RoundTripExternalVerify(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	ctx := context.Background()
	pool := pgtest.NewPool(t)
	auditRepo := audit.NewPostgresRepository(pool)
	runRepo := run.NewPostgresRepository(pool)
	signingRepo := signing.NewPostgresRepository(pool)

	// Seed two runs, each with a signing key and a genuinely-chained
	// audit trail written through the production AppendChained path.
	const repo = "acme/roundtrip"
	runCats := []string{"run_created", "plan_generated", "plan_approved"}
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
			Repo:          repo,
			WorkflowID:    "feature_change",
			WorkflowSHA:   "deadbeef",
			TriggerSource: run.TriggerCLI,
		})
		if err != nil {
			t.Fatalf("create run %d: %v", i, err)
		}
		if _, err := signingRepo.Issue(ctx, r.ID, signing.DefaultTTL); err != nil {
			t.Fatalf("issue signing key for run %d: %v", i, err)
		}
		for j, cat := range runCats {
			if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
				RunID:     r.ID,
				Timestamp: base.Add(time.Duration(i*10+j) * time.Minute),
				Category:  cat,
				Payload:   json.RawMessage(`{"cat":"` + cat + `"}`),
			}); err != nil {
				t.Fatalf("append chained (run %d, %s): %v", i, cat, err)
			}
		}
		// E39.4 (#1709): the first run's trail also carries BOTH
		// approval_submitted row shapes, so the round-trip proves the
		// additive payload enrichment (identity/channel/predicate_snapshot,
		// riding inside the hashed payload JSONB) does not break the hash
		// chain or the strict Export v1 decode:
		//   - a LEGACY-gate row: identity{provider,subject} + channel +
		//     auth_method, NO predicate_snapshot (operator binding conditions
		//     1 & 2);
		//   - an ENRICHED quorum-gate row: adds the predicate_snapshot object.
		if i == 0 {
			user := audit.ActorUser
			legacy := `{"stage_id":"s1","decision":"approve","surface":"api","approver":"github:alice",` +
				`"auth_method":"static","identity":{"provider":"github","subject":"github:alice"},"channel":"api"}`
			enriched := `{"stage_id":"s1","decision":"approve","surface":"api","approver":"github:bob",` +
				`"auth_method":"oauth","identity":{"provider":"github","subject":"github:bob"},"channel":"api",` +
				`"predicate_snapshot":{"count_required":2,"count_eligible":2,` +
				`"identity":{"provider":"github","subject":"github:bob"},"submitter_class":"eligible",` +
				`"auth_method":"oauth","channel":"api","min_permission":"write","member_of":"acme/reviewers",` +
				`"quorum_reached":true}}`
			for k, payload := range []string{legacy, enriched} {
				subject := "github:approver"
				if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
					RunID:        r.ID,
					Timestamp:    base.Add(time.Duration(i*10+len(runCats)+k) * time.Minute),
					Category:     "approval_submitted",
					ActorKind:    &user,
					ActorSubject: &subject,
					Payload:      json.RawMessage(payload),
				}); err != nil {
					t.Fatalf("append approval row %d: %v", k, err)
				}
			}
		}
	}

	// Global (run-less) chain partition: two genuinely-chained entries.
	globalCats := []string{"token_issued", "token_revoked"}
	for j, cat := range globalCats {
		if _, err := auditRepo.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
			Timestamp: base.Add(time.Duration(100+j) * time.Minute),
			Category:  cat,
			Payload:   json.RawMessage(`{"cat":"` + cat + `"}`),
		}); err != nil {
			t.Fatalf("append global chained (%s): %v", cat, err)
		}
	}

	// The real handler stack over httptest, including the REAL
	// Postgres-backed token authentication: the export surfaces enforce
	// read:audit-export (E9.5/#1608), so the CLI authenticates with a
	// genuinely-issued scoped token — the full production auth path
	// (bearer header → apitoken.Authenticate → requireWriteScope).
	tokenRepo := apitoken.NewPostgresRepository(pool)
	exportToken, err := tokenRepo.Issue(ctx, "roundtrip-test", []string{"read:audit-export"})
	if err != nil {
		t.Fatalf("issue export token: %v", err)
	}
	s := New(Config{
		AuditRepo:    auditRepo,
		RunRepo:      runRepo,
		SigningRepo:  signingRepo,
		APITokenRepo: tokenRepo,
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// Build both real binaries once, resolved by go.work workspace mode.
	binDir := t.TempDir()
	cliBin := buildSiblingBinary(t, binDir, "fishhawk",
		filepath.Join("..", "..", "..", "cli", "cmd", "fishhawk"))
	verifyBin := buildSiblingBinary(t, binDir, "fishhawk-verify",
		filepath.Join("..", "..", "..", "verifier", "cmd", "fishhawk-verify"))

	// Export through the REAL CLI. --limit 1 forces one run per page, so
	// the CLI's multi-page continuation + merge is what the verifier
	// checks (three logical partitions: two runs + the global chain).
	exportPath := filepath.Join(t.TempDir(), "export.json")
	exportOut, err := exec.Command(cliBin, "export",
		"--backend-url", srv.URL,
		"--token", exportToken.PlainText,
		"--repo", repo,
		"--limit", "1",
		"--out", exportPath,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("fishhawk export failed: %v\n%s", err, exportOut)
	}

	// Auth negative through the same real stack: a tokenless CLI call is
	// rejected (401 → non-zero exit) and leaves no partial --out file —
	// the fail-loud contract holds for the auth failure mode too.
	deniedPath := filepath.Join(t.TempDir(), "denied.json")
	deniedOut, deniedErr := exec.Command(cliBin, "export",
		"--backend-url", srv.URL,
		"--repo", repo,
		"--out", deniedPath,
	).CombinedOutput()
	if deniedErr == nil {
		t.Fatalf("tokenless fishhawk export succeeded; want non-zero exit (read:audit-export enforced)\n%s", deniedOut)
	}
	if !strings.Contains(string(deniedOut), "authentication_required") {
		t.Errorf("tokenless export stderr = %s, want authentication_required surfaced", deniedOut)
	}
	if _, statErr := os.Stat(deniedPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("tokenless export left a file at --out (stat err = %v); want absent", statErr)
	}

	// Happy path: the verifier accepts the assembled export at exit 0.
	code, vout := runVerify(t, verifyBin, exportPath)
	if code != 0 {
		t.Fatalf("fishhawk-verify exit = %d, want 0\n%s", code, vout)
	}
	if !strings.Contains(vout, "PASS") {
		t.Errorf("verify stdout missing PASS:\n%s", vout)
	}
	// Deterministic counts: 2 runs + the global partition = 3 verified;
	// (3+2 approval) run-0 entries + 3 run-1 entries + 2 global entries =
	// 10 audit entries.
	if !strings.Contains(vout, "3 run(s)") {
		t.Errorf("verify stdout missing '3 run(s)':\n%s", vout)
	}
	if !strings.Contains(vout, "10 audit entries") {
		t.Errorf("verify stdout missing '10 audit entries':\n%s", vout)
	}

	// Tamper case: flip one character of one entry's entry_hash and prove
	// the verifier catches it — exit 1 with kind=hash_mismatch.
	tamperedPath := filepath.Join(t.TempDir(), "tampered.json")
	tamperOneEntryHash(t, exportPath, tamperedPath)
	tcode, tout := runVerify(t, verifyBin, tamperedPath)
	if tcode != 1 {
		t.Fatalf("tampered verify exit = %d, want 1\n%s", tcode, tout)
	}
	if !strings.Contains(tout, "kind=hash_mismatch") {
		t.Errorf("tampered verify stdout missing kind=hash_mismatch:\n%s", tout)
	}
}

// buildSiblingBinary compiles the package at pkgRel (relative to this
// test file's directory) into binDir/name and returns the binary path.
// go.work workspace mode resolves the sibling module from the backend
// module's test process (precedent: fishhawk-mcp/run_children_test.go
// building the runner binary in-test).
func buildSiblingBinary(t *testing.T, binDir, name, pkgRel string) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	pkgDir := filepath.Join(filepath.Dir(thisFile), pkgRel)
	bin := filepath.Join(binDir, name)
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = pkgDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", name, err, out)
	}
	return bin
}

// runVerify runs fishhawk-verify against path and returns its exit code
// and combined output.
func runVerify(t *testing.T, verifyBin, path string) (int, string) {
	t.Helper()
	out, err := exec.Command(verifyBin, "--export", path).CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run verify: non-exit error: %v\n%s", err, out)
	}
	return exitErr.ExitCode(), string(out)
}

// tamperOneEntryHash reads the export at src, flips one character of the
// first run entry's entry_hash, and writes the result to dst. It fails
// the test if the export has no tamperable entry.
func tamperOneEntryHash(t *testing.T, src, dst string) {
	t.Helper()
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	var ex struct {
		Schema     json.RawMessage            `json:"schema"`
		ExportedAt json.RawMessage            `json:"exported_at"`
		Runs       map[string]json.RawMessage `json:"runs"`
	}
	if err := json.Unmarshal(raw, &ex); err != nil {
		t.Fatalf("parse export: %v", err)
	}

	for key, rawRun := range ex.Runs {
		var rd struct {
			SigningKey   json.RawMessage   `json:"signing_key,omitempty"`
			AuditEntries []json.RawMessage `json:"audit_entries"`
		}
		if err := json.Unmarshal(rawRun, &rd); err != nil {
			t.Fatalf("parse run %s: %v", key, err)
		}
		if len(rd.AuditEntries) == 0 {
			continue
		}
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(rd.AuditEntries[0], &entry); err != nil {
			t.Fatalf("parse entry: %v", err)
		}
		var hash string
		if err := json.Unmarshal(entry["entry_hash"], &hash); err != nil {
			t.Fatalf("parse entry_hash: %v", err)
		}
		entry["entry_hash"], _ = json.Marshal(flipFirstHexChar(hash))
		rd.AuditEntries[0], _ = json.Marshal(entry)
		ex.Runs[key], _ = json.Marshal(rd)

		out, err := json.Marshal(ex)
		if err != nil {
			t.Fatalf("marshal tampered export: %v", err)
		}
		if err := os.WriteFile(dst, out, 0o600); err != nil {
			t.Fatalf("write tampered export: %v", err)
		}
		return
	}
	t.Fatal("no audit entry available to tamper")
}

// flipFirstHexChar returns s with its first hex digit changed to a
// different one, guaranteeing the recomputed hash no longer matches.
func flipFirstHexChar(s string) string {
	if s == "" {
		return "0"
	}
	b := []byte(s)
	if b[0] == '0' {
		b[0] = '1'
	} else {
		b[0] = '0'
	}
	return string(b)
}
