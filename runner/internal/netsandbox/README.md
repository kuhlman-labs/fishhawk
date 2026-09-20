# runner/internal/netsandbox

OS-level confinement of the acceptance agent's WHOLE process tree (E72 / [#3393](https://github.com/kuhlman-labs/fishhawk/issues/3393)). Stdlib-only leaf; mirrors `gateiso/sandbox.go`'s probe/wrap shape. Consumed at ONE site: `runner/cmd/fishhawk-runner/acceptancesandbox.go::configureAcceptanceNetSandbox`, wired into `main.go`'s acceptance block directly after `egressproxy.Start`, threaded to the adapters via `agent.Invocation.ExecWrapper`.

## Why an OS layer

The ADR-050 containment was carried ENTIRELY in environment variables — `HTTP(S)_PROXY` → the egress proxy, `NO_PROXY` cleared, `FISHHAWK_FORGE_WRITES=deny` (#3500), forge credentials withheld by `acceptenv`. Env binds cooperating processes only. In the #3393 incident a nested `fishhawk-runner` spawned with `env -i` inherited none of it; the runner's credential ladder (`gh auth token` reads the operator keyring, the backend installation-token endpoint is signature-authed off a run id) is NOT env-sourced, so withholding credentials from the env did not stop it, and it pushed to the real repository. A Seatbelt profile applied by `sandbox-exec(1)` is inherited by every descendant regardless of its env: a direct `connect()` to anything but the admitted loopback ports fails with `EPERM` in the kernel.

## Mechanism and profile grammar

`Profile(proxyAddr, allowHosts)` renders, byte-deterministically:

```
(version 1)
(allow default)
(deny network-outbound (remote ip "*:*"))
(allow network-outbound (remote ip "localhost:<port>"))   ; one per admitted port, numeric ascending, de-duplicated
```

Admitted ports: the proxy's own port ALWAYS; plus each `allowHosts` entry whose host is `localhost` or a loopback IP literal (`127.0.0.0/8`, `::1`) — a host-only loopback entry expands to 80 + 443, matching `egressproxy`'s host-only rule. Every NON-loopback entry (`api.anthropic.com`, the forge host, a LAN IP) is silently NOT admitted direct: it is reachable only via the proxy (`HTTPS_PROXY` CONNECT), which is today's sanctioned path. Fail-closed inputs: a non-loopback `proxyAddr`, a proxy address without a port, or a non-numeric / out-of-range port anywhere returns an error — never an over-broad or malformed profile.

Two Seatbelt facts the grammar is built on (verified on Darwin 25.6): `remote ip` accepts ONLY `localhost` or `*` as the host — an IP literal such as `127.0.0.1:8090` or `::1:8090` is a profile SYNTAX error (`host must be * or localhost in network address`, exit 65) — and `localhost:<port>` matches BOTH `127.0.0.1` and `::1`. So every admitted endpoint renders as `localhost:<port>`; `TestSeatbelt_…/ipv6_loopback_admitted_via_localhost` pins the IPv6 half.

`Wrap(argv, profile)` returns `["sandbox-exec", "-p", profile, argv...]` — argv appended verbatim, never interpolated. The runner passes `Wrap(nil, profile)` as `Invocation.ExecWrapper`; the adapter appends the agent binary + args through `agent.WrapArgv`, so the sandboxed process is still the direct child and process-group leader (kill-tree, Setpgid, out-of-tree detector unchanged).

`Probe(ctx)`: darwin, `sandbox-exec` on PATH, and a deny-all profile applies against `/usr/bin/true`. Anything else is `unavailable` with a reason naming what is missing. Non-darwin reports `UnavailableNonDarwin` (below).

## Empirical matrix (Darwin 25.6, `TestSeatbelt_DeniesDirectEgress_ProxyPathSurvives` + planning probes)

| Under the profile | Result |
|---|---|
| `curl http://localhost:<admitted>` (v4 and `[::1]`) | 200 |
| `env -i curl http://127.0.0.1:<NOT admitted>` (the incident shape) | curl exit 7, upstream records ZERO hits |
| `http_proxy=<proxy> curl <not-admitted host>` | 200 through the proxy — the sanctioned path survives |
| `curl https://140.82.112.3/` (public IP literal, `-m 3`) | exit 7 in ~10 ms (EPERM, not a timeout) |
| `curl https://github.com/` (hostname, direct) | exit 7 |
| `ssh git@github.com`; `go run` `net.Dial` to a public IP | `Operation not permitted` |
| `curl --unix-socket /var/run/docker.sock` | ALLOWED (residual 2) |
| `sandbox-exec -p '(version 1)(allow default)' true` nested inside | `sandbox_apply: Operation not permitted`, exit 71 (residual 1) |

The e2e's case (d) is **NON-HERMETIC** (it dials a public IP literal); its assertion is the FAST exit-7, so a host without egress cannot green it by timing out — and the same `-m 3` fast assertion is kept on the counterfactual run.

## Policy: `FISHHAWK_ACCEPTANCE_NET_SANDBOX`

| Value | Behaviour | Event (exactly one per stage) |
|---|---|---|
| `auto` (default, empty) | apply when `Probe` says available; otherwise proceed env-only, LOUDLY | `acceptance_net_sandbox_applied {mechanism:"seatbelt", admitted_ports}` / `acceptance_net_sandbox_unavailable {reason, enforcement:"env-only"}` |
| `require` | unavailable fails the stage category-C BEFORE any spawn | `acceptance_net_sandbox_required {reason}` → `runner_failed` |
| `off` | never probe, never wrap — the kill switch | `acceptance_net_sandbox_disabled` |
| anything else | category-C pre-spawn | `acceptance_net_sandbox_config` naming the valid values |

A profile render error (`acceptance_net_sandbox_profile`) is also category-C: never spawn under a malformed profile. The variable is runner-process config and is dropped from the agent env by `acceptenv`'s default-deny allow-list (`TestAcceptenv_ExcludesPreviewVars`).

## Residuals (stated, not closed here)

1. **Nested `sandbox_apply` is refused inside the profile.** If the operator's Claude Code settings enable the built-in Bash sandbox, EVERY Bash tool call in the acceptance agent fails under this change. Set `FISHHAWK_ACCEPTANCE_NET_SANDBOX=off` on the runner env (no rebuild) or disable that setting. Probe command: `sandbox-exec -p '(version 1)(allow default)(deny network-outbound (remote ip "*:*"))' sandbox-exec -p '(version 1)(allow default)' /usr/bin/true` exits 71.
2. **Unix-domain sockets are NOT denied** — `(remote ip "*:*")` does not cover them, so a reachable docker daemon socket is an escape path (a container with network). Deliberate: closing it needs an allow-list of the agent's own IPC sockets; follow-up.
3. **Linux is unavailable** (`UnavailableNonDarwin`): gateiso's `unshare -n` creates a namespace with NO route to the host loopback the proxy binds — the verify gate wants "no network at all", acceptance wants "host loopback only, everything else denied" — so the mechanism does not transfer and enforcement there stays env-only, reported by the loud `acceptance_net_sandbox_unavailable` event. A Linux equivalent (e.g. a per-process nftables/cgroup rule) is a follow-up.
4. **`sandbox-exec(1)` is documented as deprecated by Apple** (`man sandbox-exec`) yet ships on current macOS and is the same mechanism Claude Code's own macOS sandbox uses (<https://github.com/anthropic-experimental/sandbox-runtime>). The darwin e2e fails if a future macOS removes or changes it; `Probe` then reports unavailable and `auto` degrades loudly.
5. **DNS inside the sandbox.** Hostnames must reach the proxy by name via CONNECT (Node's undici `ProxyAgent` forwards the hostname without a local lookup), which is how the model API is reached today. If the agent CLI performs a non-proxied pre-flight lookup that now hard-fails, acceptance stages on macOS break under `auto` — validated by the operator's first live acceptance dispatch after the #3086 runner rebuild, with `off` as the kill switch.

## Relationship to ADR-063

`gateiso.SandboxUnavailableNonLinux` says macOS has no unprivileged no-network sandbox primitive. That statement concerns the VERIFY GATE's fresh-namespace need (no loopback at all) and stands; the acceptance case is the opposite shape, which Seatbelt expresses and `unshare -n` cannot.
