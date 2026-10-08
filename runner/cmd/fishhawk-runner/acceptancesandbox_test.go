package main

import (
	"context"
	"io/fs"
	"strings"
	"syscall"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/netsandbox"
)

// Credential-isolation fixtures for the configureAcceptanceNetSandbox rows
// (#3792). The resolver is injected and answers not-exist for every path, so
// the rendered credential clauses are filesystem-free.
const (
	testCredHome   = "/Users/fh-test"
	testCredSocket = "/private/tmp/fh-agent.sock"
)

func enoentResolver(string) (string, error) { return "", fs.ErrNotExist }

func testCredDeny(keychain bool) netsandbox.CredentialDeny {
	return netsandbox.CredentialDeny{
		Homes: []string{testCredHome}, Files: true, Keychain: keychain, SSHAgent: true,
		AgentSockets: []string{testCredSocket}, EvalSymlinks: enoentResolver,
	}
}

var (
	// isoAutoNoCred is the default production posture under the test
	// harness: auto, active, real HOME kept, keychain NOT denied.
	isoAutoNoCred = acceptanceCredentialIsolation{mode: netsandbox.ModeAuto, active: true,
		skipReason: skipReasonModelCredentialNotEnvCarried, deny: testCredDeny(false)}
	isoAutoCred = acceptanceCredentialIsolation{mode: netsandbox.ModeAuto, active: true,
		homeIsolated: true, deny: testCredDeny(true)}
	isoOff = acceptanceCredentialIsolation{mode: netsandbox.ModeOff}
)

// Event-field fragments credentialIsolationFields renders for the fixtures.
const (
	fieldsNoCredEnvOnly = `"credential_isolation":"auto","home_isolated":false,"keychain_denied":false,"ssh_agent_denied":false,"credential_files_denied":false,"home_isolation_skipped":"model_credential_not_env_carried"}`
	fieldsNoCredApplied = `"credential_isolation":"auto","home_isolated":false,"keychain_denied":false,"ssh_agent_denied":true,"credential_files_denied":true,"home_isolation_skipped":"model_credential_not_env_carried"}`
	fieldsCredEnvOnly   = `"credential_isolation":"auto","home_isolated":true,"keychain_denied":false,"ssh_agent_denied":false,"credential_files_denied":false}`
	fieldsCredApplied   = `"credential_isolation":"auto","home_isolated":true,"keychain_denied":true,"ssh_agent_denied":true,"credential_files_denied":true}`
	fieldsOff           = `"credential_isolation":"off","home_isolated":false,"keychain_denied":false,"ssh_agent_denied":false,"credential_files_denied":false}`
)

// Clauses WithCredentialDeny adds for the fixtures.
const (
	machLookupClause = `(deny mach-lookup (global-name "com.apple.SecurityServer"))`
	sshHomeClause    = `(deny network-outbound (remote unix-socket (subpath "/Users/fh-test")))`
	sshSocketClause  = `(deny network-outbound (remote unix-socket (path-literal "/private/tmp/fh-agent.sock")))`
	credFileClause   = `(subpath "/Users/fh-test/.ssh")`
)

// TestConfigureAcceptanceNetSandbox_Rows: one row per branch of
// configureAcceptanceNetSandbox (#3393), plus the credential-isolation rows
// (#3792). Each asserts the single event logged, the category-C fail reason
// where the branch fails, the wrapper prefix (or its absence), the
// credential-isolation event fields on every non-failing branch, and which
// credential clauses the profile carries.
func TestConfigureAcceptanceNetSandbox_Rows(t *testing.T) {
	avail := func(context.Context) (bool, string) { return true, "" }
	unavail := func(context.Context) (bool, string) { return false, netsandbox.UnavailableNonDarwin }
	const proxyURL = "http://127.0.0.1:8090"
	hosts := []string{"localhost:3000", "api.anthropic.com", "api.github.com", "api.fishhawk.test"}

	// A deny that is NOT zero yet carried by an INACTIVE decision isolates
	// the iso.active guard: only that guard keeps it out of the profile.
	offWithDeny := acceptanceCredentialIsolation{mode: netsandbox.ModeOff, deny: testCredDeny(true)}
	// A resolver that fails with something other than not-exist must fail
	// the render closed.
	eacces := isoAutoCred
	eacces.deny.EvalSymlinks = func(p string) (string, error) {
		return "", &fs.PathError{Op: "lstat", Path: p, Err: syscall.EACCES}
	}

	rows := []struct {
		name        string
		mode        string
		proxyURL    string
		iso         acceptanceCredentialIsolation
		probe       func(context.Context) (bool, string)
		wantEvent   string
		wantFail    string
		wantWrapper bool
		wantLog     []string
		// wantClauses / wantNoClauses are checked against the profile when
		// a wrapper is expected.
		wantClauses   []string
		wantNoClauses []string
		// wantNoOptionProfile asserts the profile is byte-identical to a
		// zero-option netsandbox.Profile render.
		wantNoOptionProfile bool
	}{
		{name: "invalid mode fails config", mode: "strict", proxyURL: proxyURL, iso: isoAutoNoCred, probe: avail,
			wantEvent: "acceptance_net_sandbox_config", wantFail: "acceptance_net_sandbox_config",
			wantLog: []string{`"var":"FISHHAWK_ACCEPTANCE_NET_SANDBOX"`, `\"auto\"`, `\"require\"`, `\"off\"`}},
		{name: "off logs disabled, no wrapper", mode: "off", proxyURL: proxyURL, iso: isoAutoNoCred, probe: avail,
			wantEvent: "acceptance_net_sandbox_disabled",
			wantLog:   []string{`"enforcement":"env-only"`, fieldsNoCredEnvOnly}},
		{name: "auto + unavailable is loud, no wrapper", mode: "auto", proxyURL: proxyURL, iso: isoAutoNoCred, probe: unavail,
			wantEvent: "acceptance_net_sandbox_unavailable",
			wantLog:   []string{`"enforcement":"env-only"`, `"reason":"` + netsandbox.UnavailableNonDarwin, fieldsNoCredEnvOnly}},
		{name: "empty mode is auto", mode: "", proxyURL: proxyURL, iso: isoAutoNoCred, probe: unavail,
			wantEvent: "acceptance_net_sandbox_unavailable", wantLog: []string{`"mode":"auto"`, fieldsNoCredEnvOnly}},
		{name: "require + unavailable fails", mode: "require", proxyURL: proxyURL, iso: isoAutoNoCred, probe: unavail,
			wantEvent: "acceptance_net_sandbox_required", wantFail: "acceptance_net_sandbox_required",
			wantLog: []string{`"reason":"` + netsandbox.UnavailableNonDarwin}},
		{name: "available + non-loopback proxy fails profile", mode: "auto", proxyURL: "http://10.0.0.5:8090", iso: isoAutoNoCred, probe: avail,
			wantEvent: "acceptance_net_sandbox_profile", wantFail: "acceptance_net_sandbox_profile",
			wantLog: []string{"is not loopback"}},
		{name: "available + unparseable proxy URL fails profile", mode: "auto", proxyURL: "://nohost", iso: isoAutoNoCred, probe: avail,
			wantEvent: "acceptance_net_sandbox_profile", wantFail: "acceptance_net_sandbox_profile"},
		{name: "available applies seatbelt", mode: "auto", proxyURL: proxyURL, iso: isoAutoNoCred, probe: avail,
			wantEvent: "acceptance_net_sandbox_applied", wantWrapper: true,
			wantLog: []string{`"mechanism":"seatbelt"`, `"admitted_ports":[3000,8090]`, fieldsNoCredApplied}},
		{name: "require + available applies seatbelt", mode: "require", proxyURL: proxyURL, iso: isoAutoNoCred, probe: avail,
			wantEvent: "acceptance_net_sandbox_applied", wantWrapper: true,
			wantLog: []string{`"mode":"require"`, fieldsNoCredApplied}},

		// --- credential isolation (#3792) ---
		{name: "auto + credential, applied: keychain, files and ssh-agent denied", mode: "auto", proxyURL: proxyURL,
			iso: isoAutoCred, probe: avail, wantEvent: "acceptance_net_sandbox_applied", wantWrapper: true,
			wantLog:     []string{fieldsCredApplied},
			wantClauses: []string{machLookupClause, credFileClause, sshHomeClause, sshSocketClause}},
		{name: "auto without credential, applied: no mach-lookup clause", mode: "auto", proxyURL: proxyURL,
			iso: isoAutoNoCred, probe: avail, wantEvent: "acceptance_net_sandbox_applied", wantWrapper: true,
			wantLog:       []string{fieldsNoCredApplied},
			wantClauses:   []string{credFileClause, sshHomeClause, sshSocketClause},
			wantNoClauses: []string{machLookupClause}},
		{name: "credential isolation off: profile byte-identical to the no-option render", mode: "auto", proxyURL: proxyURL,
			iso: isoOff, probe: avail, wantEvent: "acceptance_net_sandbox_applied", wantWrapper: true,
			wantLog:             []string{fieldsOff},
			wantNoClauses:       []string{machLookupClause, credFileClause, sshHomeClause, "unix-socket"},
			wantNoOptionProfile: true},
		{name: "inactive decision carrying a deny: the active guard keeps it out", mode: "auto", proxyURL: proxyURL,
			iso: offWithDeny, probe: avail, wantEvent: "acceptance_net_sandbox_applied", wantWrapper: true,
			wantNoClauses:       []string{machLookupClause, credFileClause, "unix-socket"},
			wantNoOptionProfile: true},
		{name: "credential + sandbox unavailable: home isolated, nothing kernel-denied", mode: "auto", proxyURL: proxyURL,
			iso: isoAutoCred, probe: unavail, wantEvent: "acceptance_net_sandbox_unavailable",
			wantLog: []string{fieldsCredEnvOnly}},
		{name: "credential + net sandbox off: disabled carries the fields", mode: "off", proxyURL: proxyURL,
			iso: isoAutoCred, probe: avail, wantEvent: "acceptance_net_sandbox_disabled",
			wantLog: []string{fieldsCredEnvOnly}},
		{name: "credential path resolver error fails profile", mode: "auto", proxyURL: proxyURL,
			iso: eacces, probe: avail, wantEvent: "acceptance_net_sandbox_profile", wantFail: "acceptance_net_sandbox_profile",
			wantLog: []string{"resolving credential path", "permission denied"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			var log strings.Builder
			wrapper, failReason, failDetail := configureAcceptanceNetSandbox(context.Background(),
				fakeEnv(map[string]string{acceptanceNetSandboxEnvVar: r.mode}), r.proxyURL, hosts, r.iso, r.probe, &log)
			out := log.String()
			if strings.Count(out, `"event":"`) != 1 || !strings.Contains(out, `"event":"`+r.wantEvent+`"`) {
				t.Fatalf("want exactly one %s event, got: %s", r.wantEvent, out)
			}
			for _, want := range r.wantLog {
				if !strings.Contains(out, want) {
					t.Errorf("log missing %s: %s", want, out)
				}
			}
			if r.wantFail != "" && strings.Contains(out, `"credential_isolation"`) {
				t.Errorf("a failing branch must not carry the credential-isolation fields: %s", out)
			}
			if failReason != r.wantFail {
				t.Fatalf("failReason = %q, want %q (detail %q)", failReason, r.wantFail, failDetail)
			}
			if r.wantFail != "" && failDetail == "" {
				t.Fatal("a failing branch must carry a detail")
			}
			if !r.wantWrapper {
				if wrapper != nil {
					t.Fatalf("wrapper = %q, want none", wrapper)
				}
				return
			}
			if len(wrapper) != 3 || wrapper[0] != "sandbox-exec" || wrapper[1] != "-p" {
				t.Fatalf("wrapper = %q, want sandbox-exec -p <profile>", wrapper)
			}
			profile := wrapper[2]
			for _, clause := range []string{
				`(deny network-outbound (remote ip "*:*"))`,
				`(allow network-outbound (remote ip "localhost:8090"))`,
				`(allow network-outbound (remote ip "localhost:3000"))`,
			} {
				if !strings.Contains(profile, clause) {
					t.Errorf("profile lacks %s:\n%s", clause, profile)
				}
			}
			for _, direct := range []string{"anthropic", "github", "fishhawk.test"} {
				if strings.Contains(profile, direct) {
					t.Errorf("profile admits a non-loopback host direct (%s):\n%s", direct, profile)
				}
			}
			for _, clause := range r.wantClauses {
				if !strings.Contains(profile, clause) {
					t.Errorf("profile lacks credential clause %s:\n%s", clause, profile)
				}
			}
			for _, clause := range r.wantNoClauses {
				if strings.Contains(profile, clause) {
					t.Errorf("profile carries %s, want it absent:\n%s", clause, profile)
				}
			}
			if r.wantNoOptionProfile {
				plain, err := netsandbox.Profile("127.0.0.1:8090", hosts)
				if err != nil {
					t.Fatalf("no-option render: %v", err)
				}
				if profile != plain {
					t.Errorf("profile is not byte-identical to the no-option render:\n got: %s\nwant: %s", profile, plain)
				}
			}
		})
	}
}
