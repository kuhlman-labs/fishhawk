# runner/internal/netsandbox

OS-level confinement of the acceptance agent's WHOLE process tree (E72 / [#3393](https://github.com/kuhlman-labs/fishhawk/issues/3393)). Stdlib-only leaf; mirrors `gateiso/sandbox.go`'s probe/wrap shape. Consumed at ONE site: `runner/cmd/fishhawk-runner/acceptancesandbox.go::configureAcceptanceNetSandbox`, wired into `main.go`'s acceptance block directly after `egressproxy.Start`, threaded to the adapters via `agent.Invocation.ExecWrapper`.

## Why an OS layer

The ADR-050 containment was carried ENTIRELY in environment variables — `HTTP(S)_PROXY` → the egress proxy, `NO_PROXY` cleared, `FISHHAWK_FORGE_WRITES=deny` (#3500), forge credentials withheld by `acceptenv`. Env binds cooperating processes only. In the #3393 incident a nested `fishhawk-runner` spawned with `env -i` inherited none of it; the runner's credential ladder (`gh auth token` reads the operator keyring, the backend installation-token endpoint is signature-authed off a run id) is NOT env-sourced, so withholding credentials from the env did not stop it, and it pushed to the real repository. A Seatbelt profile applied by `sandbox-exec(1)` is inherited by every descendant regardless of its env: a direct `connect()` to anything but the admitted loopback ports fails with `EPERM` in the kernel.

The network deny stops the push; it does not stop a descendant READING the host's forge credentials (`~/.config/gh`, `~/.git-credentials`, `~/.ssh/*`, `gh auth token` against the keychain, the ssh-agent socket) and carrying them out through any channel the proxy admits. `WithCredentialDeny` (E72.40 / [#3792](https://github.com/kuhlman-labs/fishhawk/issues/3792)) closes those surfaces in the same profile; the runner's synthetic HOME and git pins (`acceptenv.IsolateCredentials`) are the cooperating-process half, this is the half a descendant cannot opt out of.

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

### Credential denies: `Profile(proxyAddr, allowHosts, WithCredentialDeny(d))`

`CredentialDeny{Homes, Files, Keychain, SSHAgent, AgentSockets, EvalSymlinks}` renders, between `(allow default)` and the IP deny (never after the port allows — see below), in this fixed order with homes, filters and sockets de-duplicated and sorted:

```
(deny mach-lookup (global-name "com.apple.SecurityServer"))            ; Keychain
(deny file-read* (subpath "<home>/Library/Keychains") [resolved])       ; Keychain, one per home
(deny file-read*                                                        ; Files, one per home
  (literal "<home>/.config/git/credentials") (literal "<home>/.git-credentials")
  (literal "<home>/.gitconfig") (literal "<home>/.netrc")
  (subpath "<home>/.config/gh") (subpath "<home>/.config/glab-cli") (subpath "<home>/.ssh")
  [(literal|subpath "<resolved target>") ...])
(deny network-outbound (remote unix-socket (subpath "<home>")))         ; SSHAgent, one per home
(deny network-outbound (remote unix-socket (path-literal "<socket>")))  ; SSHAgent, one per AgentSockets entry
(deny network-outbound (remote unix-socket (path-regex #"^/private/tmp/com\.apple\.launchd\.[^/]+/Listeners$")))  ; SSHAgent
```

- **Directory entries** render as `subpath` (the path itself and everything under it, including a directory created later); **file entries** as `literal`.
- **Per-path symlink resolution.** For each `p = home/rel`, `EvalSymlinks(p)` (injectable; nil = `filepath.EvalSymlinks`): not-exist → deny `p` only; resolves to `r != p` → validate `r` like an input, deny BOTH; any other error → the render FAILS naming `p`. Seatbelt checks each vnode a lookup meets against that vnode's OWN path: an unresolved-only deny of a final-component symlink (`~/.ssh → elsewhere`) still blocks `cat ~/.ssh/id`, but leaves the target readable directly and through any other alias, and an intermediate-component symlink (`~/.config → elsewhere`) bypasses it entirely. The resolved filter closes all three (e2e case `h`).
- **Fail-closed inputs**: a home, socket or resolved target that is not absolute, not `filepath.Clean`-equal, or carries `"`, `\` or a control character returns an error — never a render. Homes and sockets must already be canonical (the runner resolves them).
- **Why the section precedes the IP rules.** A `(deny network-outbound (remote unix-socket ...))` rendered AFTER `(allow network-outbound (remote ip "localhost:<p>"))` also denies the TCP connect to `<p>` for a port-dependent subset of ports — measured 12/80 ephemeral ports on Darwin 25.6, i.e. the proxy path fails at random per stage. Rendered before the IP deny, 0/300 ports failed with every socket deny still binding. `TestSeatbelt_…CredentialSurfaces…/g2_admitted_port_survives_across_ports` pins it over 40 fresh ports.
- A zero-option render is byte-identical to the network-only profile (`TestProfile_Golden` is unchanged).

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

With `WithCredentialDeny` (Darwin 25.6, `TestSeatbelt_DeniesCredentialSurfaces_ProxyPathSurvives`; every case first asserted to SUCCEED unsandboxed):

| Under the credential profile | Result |
|---|---|
| `security find-generic-password -s <svc> -w <kc>` against a hermetic unlocked keychain OUTSIDE any `Library/Keychains` (case `a`) | refused (exit 44; planning probe: `gh auth token` exits 1) |
| `cat <home>/.ssh/id_test`, `.config/gh/hosts.yml`, `.git-credentials` (case `b`) | `Operation not permitted` |
| unix-socket connect under a home / to the declared `SSH_AUTH_SOCK` / to `/private/tmp/com.apple.launchd.*/Listeners` (cases `c`–`e`) | curl exit 7 |
| unix-socket connect to an undeclared path outside the homes (case `f`) | ALLOWED — the deny is targeted |
| `http_proxy=<proxy> curl <forge>` and a direct admitted port, over 40 fresh ports (cases `g`, `g2`) | 200 — the sanctioned path survives |
| symlinked `.ssh` / `.git-credentials` / `.config` under a second home: via the home path, via the resolved target directly, via a fresh alias symlink (case `h`) | `Operation not permitted` |
| TLS to `api.github.com` from Go `net/http` under the mach-lookup deny (planning probe) | 200 — the system trust store is unaffected |

Declared non-isolation: the `<home>/Library/Keychains` file-read clause is not behaviourally separable from the mach-lookup clause (each alone refused the read in planning probes); only the golden render (`TestProfile_CredentialDeny_Golden`) pins it.
| `sandbox-exec -p '(version 1)(allow default)' true` nested inside | `sandbox_apply: Operation not permitted`, exit 71 (residual 1) |

The e2e's case (d) is **NON-HERMETIC** (it dials a public IP literal); its assertion is the FAST exit-7, so a host without egress cannot green it by timing out — and the same `-m 3` fast assertion is kept on the counterfactual run.

## Policy: `FISHHAWK_ACCEPTANCE_NET_SANDBOX`

| Value | Behaviour | Event (exactly one per stage) |
|---|---|---|
| `auto` (default, empty) | apply when `Probe` says available; otherwise proceed env-only, LOUDLY | `acceptance_net_sandbox_applied {mechanism:"seatbelt", admitted_ports}` / `acceptance_net_sandbox_unavailable {reason, enforcement:"env-only"}` |
| `require` | unavailable fails the stage category-C BEFORE any spawn | `acceptance_net_sandbox_required {reason}` → `runner_failed` |
| `off` | never probe, never wrap — the kill switch | `acceptance_net_sandbox_disabled` |
| anything else | category-C pre-spawn | `acceptance_net_sandbox_config` naming the valid values |

A profile render error (`acceptance_net_sandbox_profile`) is also category-C: never spawn under a malformed profile — including a `WithCredentialDeny` resolution error (a non-not-exist `EvalSymlinks` failure on a credential path). The runner passes `WithCredentialDeny` only while `FISHHAWK_ACCEPTANCE_CREDENTIAL_ISOLATION` is not `off` (decision, event fields and kill-switch contract: `runner/README.md` § "Acceptance-stage egress containment"); under `off` the profile is byte-identical to the network-only render. The variable is runner-process config and is dropped from the agent env by `acceptenv`'s default-deny allow-list (`TestAcceptenv_ExcludesPreviewVars`).

## Residuals (stated, not closed here)

1. **Nested `sandbox_apply` is refused inside the profile.** If the operator's Claude Code settings enable the built-in Bash sandbox, EVERY Bash tool call in the acceptance agent fails under this change. Set `FISHHAWK_ACCEPTANCE_NET_SANDBOX=off` on the runner env (no rebuild) or disable that setting. Probe command: `sandbox-exec -p '(version 1)(allow default)(deny network-outbound (remote ip "*:*"))' sandbox-exec -p '(version 1)(allow default)' /usr/bin/true` exits 71.
2. **Unix-domain sockets are NOT denied outside the credential denies** — `(remote ip "*:*")` does not cover them. With `WithCredentialDeny`'s `SSHAgent`, sockets under the real home (Docker Desktop's `~/.docker/run/docker.sock`, gpg-agent, the 1Password agent), the runner's `SSH_AUTH_SOCK` and the launchd `Listeners` pattern ARE denied; a socket elsewhere (`/var/run/docker.sock`) stays reachable, and is an escape path (a container with network). Closing the rest needs an allow-list of the agent's own IPC sockets; follow-up. Behaviour change: an acceptance agent that relied on the home-dir docker socket breaks — the kill switch is `FISHHAWK_ACCEPTANCE_CREDENTIAL_ISOLATION=off`.
3. **Linux is unavailable** (`UnavailableNonDarwin`): gateiso's `unshare -n` creates a namespace with NO route to the host loopback the proxy binds — the verify gate wants "no network at all", acceptance wants "host loopback only, everything else denied" — so the mechanism does not transfer and enforcement there stays env-only, reported by the loud `acceptance_net_sandbox_unavailable` event. A Linux equivalent (e.g. a per-process nftables/cgroup rule) is a follow-up.
4. **`sandbox-exec(1)` is documented as deprecated by Apple** (`man sandbox-exec`) yet ships on current macOS and is the same mechanism Claude Code's own macOS sandbox uses (<https://github.com/anthropic-experimental/sandbox-runtime>). The darwin e2e fails if a future macOS removes or changes it; `Probe` then reports unavailable and `auto` degrades loudly.
5. **DNS inside the sandbox.** Hostnames must reach the proxy by name via CONNECT (Node's undici `ProxyAgent` forwards the hostname without a local lookup), which is how the model API is reached today. If the agent CLI performs a non-proxied pre-flight lookup that now hard-fails, acceptance stages on macOS break under `auto` — validated by the operator's first live acceptance dispatch after the #3086 runner rebuild, with `off` as the kill switch.
6. **The `.gitconfig` deny and a re-pointed HOME.** Under `Files`, `<real home>/.gitconfig` is unreadable. A descendant that drops the git pins (`GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`) AND keeps or restores `HOME=<real home>` — `env -i HOME=<real> git status` — exits 128 (`fatal: unable to access '<real>/.gitconfig': Operation not permitted`); with HOME unset, or with the pins, git exits 0. It bites mainly on `auto`'s non-isolated path (no env-carried model credential, so HOME stays real). Kill switch: `FISHHAWK_ACCEPTANCE_CREDENTIAL_ISOLATION=off` (no rebuild).
7. **A pre-existing hard link** to a credential file elsewhere is not covered (a hard link has no path relation to the denied one). An agent cannot MANUFACTURE one: `ln` and `cp` of a denied file fail `Operation not permitted`.
8. **Symlinks are resolved once, at render.** A new alias the agent creates is covered (it resolves to a denied vnode path); a symlink the OPERATOR re-points mid-stage is not re-resolved.
9. **Linux**: none of the credential denies apply (residual 3); the synthetic HOME is the only control there, and absolute-path reads, OpenSSH (which reads the passwd home, not `$HOME`) and Secret Service over D-Bus remain reachable.

## Relationship to ADR-063

`gateiso.SandboxUnavailableNonLinux` says macOS has no unprivileged no-network sandbox primitive. That statement concerns the VERIFY GATE's fresh-namespace need (no loopback at all) and stands; the acceptance case is the opposite shape, which Seatbelt expresses and `unshare -n` cannot.
