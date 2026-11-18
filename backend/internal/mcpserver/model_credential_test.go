package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// fixedModelNow is the clock every model_credential arm reads.
var fixedModelNow = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// withFakeClaude swaps the package-level exec seams (non-parallel: the seams
// are package state). lookErr makes claude absent from PATH; otherwise run
// returns out/err for `claude auth status --json` and records the argv path.
func withFakeClaude(t *testing.T, lookErr error, out string, runErr error) *int {
	t.Helper()
	origLook, origRun, origNow := claudeLookPath, claudeAuthStatusOutput, modelCredentialNow
	calls := 0
	claudeLookPath = func(name string) (string, error) {
		if name != "claude" {
			t.Errorf("looked up %q, want claude", name)
		}
		if lookErr != nil {
			return "", lookErr
		}
		return "/fake/claude", nil
	}
	claudeAuthStatusOutput = func(_ context.Context, path string) ([]byte, error) {
		calls++
		if path != "/fake/claude" {
			t.Errorf("exec path = %q, want the looked-up /fake/claude", path)
		}
		return []byte(out), runErr
	}
	modelCredentialNow = func() time.Time { return fixedModelNow }
	t.Cleanup(func() {
		claudeLookPath, claudeAuthStatusOutput, modelCredentialNow = origLook, origRun, origNow
	})
	return &calls
}

func authStatusJSON(t *testing.T, fields map[string]any) string {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestModelCredential_NotOnPath(t *testing.T) {
	calls := withFakeClaude(t, exec.ErrNotFound, "", nil)
	rung := modelCredential(context.Background())
	if rung.Status != modelCredentialNotAvailable {
		t.Errorf("status = %q, want not_available", rung.Status)
	}
	if *calls != 0 {
		t.Errorf("auth status exec'd %d times with claude absent, want 0", *calls)
	}
}

func TestModelCredential_CommandError(t *testing.T) {
	withFakeClaude(t, nil, "", errors.New("boom"))
	rung := modelCredential(context.Background())
	if rung.Status != modelCredentialUnknown {
		t.Errorf("status = %q, want unknown", rung.Status)
	}
	if rung.LoggedIn != nil {
		t.Errorf("logged_in = %v, want absent on a failed exec", *rung.LoggedIn)
	}
}

// realExitError returns a genuine *exec.ExitError from a process that exits
// 1, the shape exec's Output returns beside stdout on a non-zero exit.
func realExitError(t *testing.T) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit 1").Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("sh exit 1 returned %v (%T), want *exec.ExitError", err, err)
	}
	return err
}

// TestModelCredential_LoggedOutNonZeroExit is the shape the real CLI
// produces signed out: exit 1 with {"loggedIn":false} on stdout. Dropping the
// ExitError decode reads unknown and goes RED.
func TestModelCredential_LoggedOutNonZeroExit(t *testing.T) {
	withFakeClaude(t, nil, authStatusJSON(t, map[string]any{"loggedIn": false, "authMethod": "none"}), realExitError(t))
	rung := modelCredential(context.Background())
	if rung.Status != modelCredentialWarn {
		t.Errorf("status = %q (note %q), want warn for a signed-out exit 1", rung.Status, rung.Note)
	}
	if rung.LoggedIn == nil || *rung.LoggedIn {
		t.Errorf("logged_in = %v, want false", rung.LoggedIn)
	}
	assertLoginRemediation(t, rung)
}

// TestModelCredential_NonZeroExitNotSignedOut: a non-zero exit whose stdout
// is empty, undecodable, or claims loggedIn:true stays unknown; only a
// loggedIn:false body agrees with the exit.
func TestModelCredential_NonZeroExitNotSignedOut(t *testing.T) {
	for name, out := range map[string]string{
		"empty":       "",
		"undecodable": "error: not a json body",
		"logged_in":   authStatusJSON(t, map[string]any{"loggedIn": true, "loginExpiresAt": fixedModelNow.Add(30 * 24 * time.Hour).Format(time.RFC3339)}),
	} {
		t.Run(name, func(t *testing.T) {
			withFakeClaude(t, nil, out, realExitError(t))
			rung := modelCredential(context.Background())
			if rung.Status != modelCredentialUnknown || !strings.Contains(rung.Note, "exited exit status 1") {
				t.Errorf("rung = %+v, want unknown naming the exit status", rung)
			}
			if rung.LoggedIn != nil {
				t.Errorf("logged_in = %v, want absent", *rung.LoggedIn)
			}
		})
	}
}

// TestModelCredential_Timeout: a deadline-exceeded exec stays unknown and
// names the timeout, even when partial stdout reads signed out.
func TestModelCredential_Timeout(t *testing.T) {
	withFakeClaude(t, nil, authStatusJSON(t, map[string]any{"loggedIn": false}), context.DeadlineExceeded)
	rung := modelCredential(context.Background())
	if rung.Status != modelCredentialUnknown || !strings.Contains(rung.Note, "did not answer within "+modelCredentialTimeout.String()) {
		t.Errorf("rung = %+v, want unknown naming the %s timeout", rung, modelCredentialTimeout)
	}
}

func TestModelCredential_Undecodable(t *testing.T) {
	withFakeClaude(t, nil, "Logged in as someone (text mode)", nil)
	rung := modelCredential(context.Background())
	if rung.Status != modelCredentialUnknown {
		t.Errorf("status = %q, want unknown (a renamed or non-JSON shape degrades, never claims ok)", rung.Status)
	}
}

// TestModelCredential_MissingLoggedIn is the S3 schema-drift arm: a shape
// without loggedIn must read unknown, not signed-out (the zero value).
func TestModelCredential_MissingLoggedIn(t *testing.T) {
	withFakeClaude(t, nil, authStatusJSON(t, map[string]any{"authMethod": "claude.ai", "loginExpiresAt": "2026-12-30T00:00:00.000Z"}), nil)
	rung := modelCredential(context.Background())
	if rung.Status != modelCredentialUnknown {
		t.Errorf("status = %q, want unknown when loggedIn is absent", rung.Status)
	}
	if rung.LoggedIn != nil {
		t.Errorf("logged_in = %v, want absent", *rung.LoggedIn)
	}
}

func TestModelCredential_LoggedOut(t *testing.T) {
	withFakeClaude(t, nil, authStatusJSON(t, map[string]any{"loggedIn": false}), nil)
	rung := modelCredential(context.Background())
	if rung.Status != modelCredentialWarn {
		t.Errorf("status = %q, want warn", rung.Status)
	}
	if rung.LoggedIn == nil || *rung.LoggedIn {
		t.Errorf("logged_in = %v, want false", rung.LoggedIn)
	}
	assertLoginRemediation(t, rung)
}

func TestModelCredential_Expired(t *testing.T) {
	exp := fixedModelNow.Add(-time.Hour).Format("2006-01-02T15:04:05.000Z07:00")
	withFakeClaude(t, nil, authStatusJSON(t, map[string]any{"loggedIn": true, "loginExpiresAt": exp}), nil)
	rung := modelCredential(context.Background())
	if rung.Status != modelCredentialWarn || !strings.Contains(rung.Note, "expired") {
		t.Errorf("rung = %+v, want warn naming expired", rung)
	}
	assertLoginRemediation(t, rung)
}

// TestModelCredential_Expiring: now+1h is inside the 72h window, so a
// threshold comparison mutated to never warn returns ok and goes RED.
func TestModelCredential_Expiring(t *testing.T) {
	exp := fixedModelNow.Add(time.Hour).Format("2006-01-02T15:04:05.000Z07:00")
	withFakeClaude(t, nil, authStatusJSON(t, map[string]any{"loggedIn": true, "loginExpiresAt": exp}), nil)
	rung := modelCredential(context.Background())
	if rung.Status != modelCredentialWarn || !strings.Contains(rung.Note, "72h") {
		t.Errorf("rung = %+v, want warn naming the 72h window", rung)
	}
	assertLoginRemediation(t, rung)
}

func TestModelCredential_Healthy(t *testing.T) {
	exp := fixedModelNow.Add(30 * 24 * time.Hour).Format("2006-01-02T15:04:05.000Z07:00")
	withFakeClaude(t, nil, authStatusJSON(t, map[string]any{"loggedIn": true, "authMethod": "claude.ai", "loginExpiresAt": exp}), nil)
	rung := modelCredential(context.Background())
	if rung.Status != modelCredentialOK {
		t.Errorf("status = %q, want ok", rung.Status)
	}
	if rung.AuthMethod != "claude.ai" || rung.ExpiresAt != fixedModelNow.Add(30*24*time.Hour).Format(time.RFC3339) {
		t.Errorf("rung = %+v, want auth_method claude.ai and the parsed expiry", rung)
	}
	if rung.Remediation != "" {
		t.Errorf("remediation = %q on ok, want empty", rung.Remediation)
	}
}

// TestModelCredential_LoggedInNoExpiry is the S3 arm for loggedIn true with
// loginExpiresAt absent or unparseable: ok, noting no expiry reported.
func TestModelCredential_LoggedInNoExpiry(t *testing.T) {
	for name, fields := range map[string]map[string]any{
		"absent":      {"loggedIn": true},
		"unparseable": {"loggedIn": true, "loginExpiresAt": "next tuesday"},
	} {
		t.Run(name, func(t *testing.T) {
			withFakeClaude(t, nil, authStatusJSON(t, fields), nil)
			rung := modelCredential(context.Background())
			if rung.Status != modelCredentialOK || !strings.Contains(rung.Note, "no expiry reported") {
				t.Errorf("rung = %+v, want ok noting no expiry reported", rung)
			}
			if rung.ExpiresAt != "" {
				t.Errorf("expires_at = %q, want absent", rung.ExpiresAt)
			}
		})
	}
}

// TestModelCredential_RedactsAccountFields: the account fields the CLI
// prints are never held, so none reaches the rung JSON.
func TestModelCredential_RedactsAccountFields(t *testing.T) {
	exp := fixedModelNow.Add(30 * 24 * time.Hour).Format(time.RFC3339)
	withFakeClaude(t, nil, authStatusJSON(t, map[string]any{
		"loggedIn": true, "authMethod": "claude.ai", "loginExpiresAt": exp,
		"email": "SENTINEL_EMAIL@example.test", "orgId": "SENTINEL_ORG_ID", "orgName": "SENTINEL_ORG_NAME",
		"apiKeySource": "SENTINEL_KEY_SOURCE",
	}), nil)
	raw, err := json.Marshal(modelCredential(context.Background()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "SENTINEL") {
		t.Errorf("rung JSON carries an account field:\n%s", raw)
	}
}

func assertLoginRemediation(t *testing.T, rung *modelCredentialRung) {
	t.Helper()
	if !strings.Contains(rung.Remediation, "claude auth login") || !strings.Contains(rung.Remediation, "Local runners") {
		t.Errorf("remediation = %q, want it to name `claude auth login` and that local runners fail until then", rung.Remediation)
	}
}
