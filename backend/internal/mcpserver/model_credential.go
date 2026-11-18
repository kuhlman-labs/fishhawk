package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// The model_credential doctor rung (#5227): is the model CLI a LOCAL runner
// spawns from this host signed in? A local implement or plan runner execs
// `claude`, so a signed-out or expired sign-in fails the stage after dispatch.
// Nothing else reads it ahead of the run.
//
// The rung is host-local. It runs only when Config.LocalHostRungs is set (the
// operator's own stdio fishhawk-mcp process) and the bearer is not run-bound;
// otherwise it reports not_applicable and nothing is exec'd.

// modelCredentialStatus values. not_available means the model CLI is not on
// PATH; unknown means it ran but its answer could not be read; not_applicable
// means the rung did not run in this posture.
const (
	modelCredentialOK            = "ok"
	modelCredentialWarn          = "warn"
	modelCredentialUnknown       = "unknown"
	modelCredentialNotAvailable  = "not_available"
	modelCredentialNotApplicable = "not_applicable"
)

// modelCredentialExpiryWindow is how close to expiry a sign-in warns. The
// claude sign-in (loginExpiresAt) is long-lived, so 72h fires ahead of the
// lapse without warning on every call.
const modelCredentialExpiryWindow = 72 * time.Hour

// modelCredentialTimeout bounds the `claude auth status` exec so a hung CLI
// cannot stall fishhawk_doctor.
const modelCredentialTimeout = 5 * time.Second

// modelCLI is the model CLI binary the rung probes.
const modelCLI = "claude"

// modelCredentialLoginRemediation is the shared warn remediation.
const modelCredentialLoginRemediation = "Run `claude auth login` on this host. Local runners spawned from this process fail until the model CLI is signed in."

// Package-level exec seams, swapped by tests (the issue_fetch.go ghLookPath
// pattern). claudeLookPath resolves the binary; claudeAuthStatusOutput runs
// `<path> auth status --json` and returns its stdout; modelCredentialNow is
// the clock the expiry comparison reads.
var (
	claudeLookPath         = exec.LookPath
	claudeAuthStatusOutput = func(ctx context.Context, path string) ([]byte, error) {
		return exec.CommandContext(ctx, path, "auth", "status", "--json").Output() //nolint:gosec // path is exec.LookPath("claude"), not caller input.
	}
	modelCredentialNow = time.Now
)

// claudeAuthStatus decodes ONLY the three fields the rung reads from
// `claude auth status --json`. Account fields (email, orgId, orgName and the
// rest) have no struct field, so they are never held. Pointer fields keep a
// missing key distinct from its zero value: an undocumented third-party shape
// that drops loggedIn must read unknown, not signed-out.
type claudeAuthStatus struct {
	LoggedIn       *bool   `json:"loggedIn"`
	AuthMethod     *string `json:"authMethod"`
	LoginExpiresAt *string `json:"loginExpiresAt"`
}

// modelCredentialRung is the LOCALLY-computed doctor rung for the model CLI
// sign-in. Unexported for the export-surface reason runnerCredentialsRung
// documents; jsonschema reaches it through DoctorOutput.
type modelCredentialRung struct {
	Status      string `json:"status" jsonschema:"ok (signed in, not expiring within 72h), warn (signed out, expired, or expiring within 72h), unknown (the CLI ran but its answer could not be read, or it omitted loggedIn), not_available (claude is not on PATH), or not_applicable (the rung did not run: not the operator's stdio fishhawk-mcp process, or a run-bound token)"`
	Reason      string `json:"reason,omitempty" jsonschema:"on not_applicable: not_local_stdio_process or run_bound_token"`
	LoggedIn    *bool  `json:"logged_in,omitempty" jsonschema:"the CLI's loggedIn answer; absent when it was not read"`
	AuthMethod  string `json:"auth_method,omitempty" jsonschema:"the CLI's authMethod answer, e.g. claude.ai; absent when not reported"`
	ExpiresAt   string `json:"expires_at,omitempty" jsonschema:"the CLI's loginExpiresAt (RFC 3339); absent when not reported or unparseable"`
	Note        string `json:"note" jsonschema:"the human sentence for the verdict"`
	Remediation string `json:"remediation,omitempty" jsonschema:"on warn: the operator next step (claude auth login on this host)"`
}

// newModelCredentialNotApplicable renders the rung for a posture it does not run
// in. Nothing is exec'd.
func newModelCredentialNotApplicable(reason string) *modelCredentialRung {
	return &modelCredentialRung{
		Status: modelCredentialNotApplicable,
		Reason: reason,
		Note:   notApplicableNote(reason, "the model CLI sign-in is not read"),
	}
}

// modelCredential runs the probe and renders the rung. Every failure degrades
// to a status; it never errors the doctor call.
func modelCredential(ctx context.Context) *modelCredentialRung {
	path, err := claudeLookPath(modelCLI)
	if err != nil {
		return &modelCredentialRung{
			Status: modelCredentialNotAvailable,
			Note:   "claude is not on PATH for this process, so a local runner spawned from here cannot exec the model CLI either. Install Claude Code or fix PATH in the environment that launches fishhawk-mcp.",
		}
	}
	ctx, cancel := context.WithTimeout(ctx, modelCredentialTimeout)
	defer cancel()
	out, err := claudeAuthStatusOutput(ctx, path)
	if err != nil {
		note := "`claude auth status --json` failed, so the sign-in state could not be read."
		var exitErr *exec.ExitError
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			note = "`claude auth status --json` did not answer within " + modelCredentialTimeout.String() + ", so the sign-in state could not be read."
		} else if errors.As(err, &exitErr) {
			note = "`claude auth status --json` exited " + exitErr.String() + ", so the sign-in state could not be read. This is not a finding that it is signed out."
		}
		return &modelCredentialRung{Status: modelCredentialUnknown, Note: note}
	}
	var st claudeAuthStatus
	if err := json.Unmarshal(out, &st); err != nil {
		return &modelCredentialRung{
			Status: modelCredentialUnknown,
			Note:   "`claude auth status --json` printed output this version cannot decode, so the sign-in state could not be read. This is not a finding that it is signed out.",
		}
	}
	return modelCredentialFromStatus(st, modelCredentialNow())
}

// modelCredentialFromStatus is the pure verdict over a decoded status.
func modelCredentialFromStatus(st claudeAuthStatus, now time.Time) *modelCredentialRung {
	rung := &modelCredentialRung{LoggedIn: st.LoggedIn}
	if st.AuthMethod != nil {
		rung.AuthMethod = strings.TrimSpace(*st.AuthMethod)
	}
	if st.LoggedIn == nil {
		rung.Status = modelCredentialUnknown
		rung.Note = "`claude auth status --json` did not report loggedIn, so the sign-in state could not be read. This is not a finding that it is signed out."
		return rung
	}
	if !*st.LoggedIn {
		rung.Status = modelCredentialWarn
		rung.Note = "The model CLI is signed out on this host."
		rung.Remediation = modelCredentialLoginRemediation
		return rung
	}
	var expires time.Time
	parsed := false
	if st.LoginExpiresAt != nil {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(*st.LoginExpiresAt)); err == nil {
			expires, parsed = t, true
		}
	}
	if !parsed {
		rung.Status = modelCredentialOK
		rung.Note = "The model CLI is signed in; no expiry reported."
		return rung
	}
	rung.ExpiresAt = expires.UTC().Format(time.RFC3339)
	switch {
	case !expires.After(now):
		rung.Status = modelCredentialWarn
		rung.Note = "The model CLI sign-in expired at " + rung.ExpiresAt + "."
		rung.Remediation = modelCredentialLoginRemediation
	case expires.Sub(now) <= modelCredentialExpiryWindow:
		rung.Status = modelCredentialWarn
		rung.Note = "The model CLI sign-in expires at " + rung.ExpiresAt + ", within 72h."
		rung.Remediation = modelCredentialLoginRemediation
	default:
		rung.Status = modelCredentialOK
		rung.Note = "The model CLI is signed in until " + rung.ExpiresAt + "."
	}
	return rung
}

// Host-local rung not_applicable reasons, shared by the credential and
// model_credential rungs.
const (
	hostRungNotLocalStdio = "not_local_stdio_process"
	hostRungRunBoundToken = "run_bound_token"
)

// notApplicableNote is the shared not_applicable sentence. what names what
// the rung did not do.
func notApplicableNote(reason, what string) string {
	if reason == hostRungRunBoundToken {
		return "This server authenticates with a run-bound in-runner agent token, so " + what + ". Host-local rungs run only for an operator credential."
	}
	return "Host-local rungs run only in the operator's own stdio fishhawk-mcp process; this server is not one (for example fishhawkd's /mcp route, whose host is the daemon's), so " + what + "."
}
