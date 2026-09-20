package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/netsandbox"
)

// TestConfigureAcceptanceNetSandbox_Rows: one row per branch of
// configureAcceptanceNetSandbox (#3393). Each asserts the single event
// logged, the category-C fail reason where the branch fails, and the
// wrapper prefix (or its absence).
func TestConfigureAcceptanceNetSandbox_Rows(t *testing.T) {
	avail := func(context.Context) (bool, string) { return true, "" }
	unavail := func(context.Context) (bool, string) { return false, netsandbox.UnavailableNonDarwin }
	const proxyURL = "http://127.0.0.1:8090"
	hosts := []string{"localhost:3000", "api.anthropic.com", "api.github.com", "api.fishhawk.test"}

	rows := []struct {
		name        string
		mode        string
		proxyURL    string
		probe       func(context.Context) (bool, string)
		wantEvent   string
		wantFail    string
		wantWrapper bool
		wantLog     []string
	}{
		{"invalid mode fails config", "strict", proxyURL, avail,
			"acceptance_net_sandbox_config", "acceptance_net_sandbox_config", false,
			[]string{`"var":"FISHHAWK_ACCEPTANCE_NET_SANDBOX"`, `\"auto\"`, `\"require\"`, `\"off\"`}},
		{"off logs disabled, no wrapper", "off", proxyURL, avail,
			"acceptance_net_sandbox_disabled", "", false, []string{`"enforcement":"env-only"`}},
		{"auto + unavailable is loud, no wrapper", "auto", proxyURL, unavail,
			"acceptance_net_sandbox_unavailable", "", false,
			[]string{`"enforcement":"env-only"`, `"reason":"` + netsandbox.UnavailableNonDarwin}},
		{"empty mode is auto", "", proxyURL, unavail,
			"acceptance_net_sandbox_unavailable", "", false, []string{`"mode":"auto"`}},
		{"require + unavailable fails", "require", proxyURL, unavail,
			"acceptance_net_sandbox_required", "acceptance_net_sandbox_required", false,
			[]string{`"reason":"` + netsandbox.UnavailableNonDarwin}},
		{"available + non-loopback proxy fails profile", "auto", "http://10.0.0.5:8090", avail,
			"acceptance_net_sandbox_profile", "acceptance_net_sandbox_profile", false,
			[]string{"is not loopback"}},
		{"available + unparseable proxy URL fails profile", "auto", "://nohost", avail,
			"acceptance_net_sandbox_profile", "acceptance_net_sandbox_profile", false, nil},
		{"available applies seatbelt", "auto", proxyURL, avail,
			"acceptance_net_sandbox_applied", "", true,
			[]string{`"mechanism":"seatbelt"`, `"admitted_ports":[3000,8090]`}},
		{"require + available applies seatbelt", "require", proxyURL, avail,
			"acceptance_net_sandbox_applied", "", true, []string{`"mode":"require"`}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			var log strings.Builder
			wrapper, failReason, failDetail := configureAcceptanceNetSandbox(context.Background(),
				fakeEnv(map[string]string{acceptanceNetSandboxEnvVar: r.mode}), r.proxyURL, hosts, r.probe, &log)
			out := log.String()
			if strings.Count(out, `"event":"`) != 1 || !strings.Contains(out, `"event":"`+r.wantEvent+`"`) {
				t.Fatalf("want exactly one %s event, got: %s", r.wantEvent, out)
			}
			for _, want := range r.wantLog {
				if !strings.Contains(out, want) {
					t.Errorf("log missing %s: %s", want, out)
				}
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
		})
	}
}
