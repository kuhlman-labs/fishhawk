package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"

	"github.com/kuhlman-labs/fishhawk/runner/internal/netsandbox"
)

// acceptanceNetSandboxEnvVar is the operator policy for the OS-level
// acceptance egress confinement (#3393): auto (default — apply when the
// host provides it, otherwise proceed LOUDLY), require (fail the stage
// category-C pre-spawn when unavailable), off (never apply; the kill
// switch). It is runner-process config and is dropped from the agent env by
// acceptenv's default-deny allow-list.
const acceptanceNetSandboxEnvVar = "FISHHAWK_ACCEPTANCE_NET_SANDBOX"

// probeNetSandbox is the host probe configureAcceptanceNetSandbox consults;
// a package-level var so tests can swap it (mirrors gateiso's probes seam).
var probeNetSandbox = netsandbox.Probe

// configureAcceptanceNetSandbox decides whether the acceptance agent spawn
// is wrapped in the Seatbelt net sandbox and logs exactly ONE event per
// branch. proxyURL is the running egress proxy's URL (the profile admits its
// port); allowHosts is the proxy's composed allow-list, of which only the
// loopback entries are admitted direct. A non-empty failReason is a
// category-C stage failure the caller reports BEFORE any spawn.
//
// Branches (each asserted by acceptancesandbox_test.go):
//   - invalid mode              -> acceptance_net_sandbox_config (fail)
//   - off                       -> acceptance_net_sandbox_disabled, no wrapper
//   - auto + unavailable        -> acceptance_net_sandbox_unavailable
//     {reason, enforcement:"env-only"}, no wrapper — loud, never silent
//   - require + unavailable     -> acceptance_net_sandbox_required (fail)
//   - available, profile error  -> acceptance_net_sandbox_profile (fail;
//     never spawn under a malformed or over-broad profile)
//   - available                 -> acceptance_net_sandbox_applied
//     {mechanism:"seatbelt", admitted_ports}, wrapper = sandbox-exec -p <profile>
func configureAcceptanceNetSandbox(ctx context.Context, getenv func(string) string, proxyURL string, allowHosts []string, probe func(context.Context) (bool, string), logSink io.Writer) (wrapper []string, failReason, failDetail string) {
	mode, err := netsandbox.ParseMode(getenv(acceptanceNetSandboxEnvVar))
	if err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"acceptance_net_sandbox_config","var":%q,"detail":%q}`+"\n",
			acceptanceNetSandboxEnvVar, err.Error())
		return nil, "acceptance_net_sandbox_config", err.Error()
	}
	if mode == netsandbox.ModeOff {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"acceptance_net_sandbox_disabled","mode":%q,"enforcement":"env-only"}`+"\n", mode)
		return nil, "", ""
	}
	available, reason := probe(ctx)
	if !available {
		if mode == netsandbox.ModeRequire {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"acceptance_net_sandbox_required","mode":%q,"reason":%q}`+"\n", mode, reason)
			return nil, "acceptance_net_sandbox_required", reason
		}
		_, _ = fmt.Fprintf(logSink,
			`{"event":"acceptance_net_sandbox_unavailable","mode":%q,"reason":%q,"enforcement":"env-only"}`+"\n",
			mode, reason)
		return nil, "", ""
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" {
		detail := fmt.Sprintf("proxy URL %q has no host", proxyURL)
		if err != nil {
			detail = fmt.Sprintf("proxy URL %q: %v", proxyURL, err)
		}
		_, _ = fmt.Fprintf(logSink,
			`{"event":"acceptance_net_sandbox_profile","detail":%q}`+"\n", detail)
		return nil, "acceptance_net_sandbox_profile", detail
	}
	profile, err := netsandbox.Profile(u.Host, allowHosts)
	if err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"acceptance_net_sandbox_profile","detail":%q}`+"\n", err.Error())
		return nil, "acceptance_net_sandbox_profile", err.Error()
	}
	ports, _ := json.Marshal(netsandbox.AdmittedPorts(profile))
	_, _ = fmt.Fprintf(logSink,
		`{"event":"acceptance_net_sandbox_applied","mode":%q,"mechanism":"seatbelt","admitted_ports":%s}`+"\n",
		mode, ports)
	// The adapter appends the binary + args (agent.WrapArgv), so the wrapper
	// is the sandbox-exec prefix with NO argv.
	return netsandbox.Wrap(nil, profile), "", ""
}
