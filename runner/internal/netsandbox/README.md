# runner/internal/netsandbox

OS-level confinement of an agent's WHOLE process tree (E72 / [#3393](https://github.com/kuhlman-labs/fishhawk/issues/3393)). Stdlib-only leaf; mirrors `gateiso/sandbox.go`'s probe/wrap shape. Two consumers, both threaded to the adapters via `agent.Invocation.ExecWrapper`:

- `runner/cmd/fishhawk-runner/acceptancesandbox.go::configureAcceptanceNetSandbox` — the COMPOSED acceptance profile (`Profile` + `WithSignalConfinement` + `WithCredentialDeny`), wired into `main.go`'s acceptance block directly after `egressproxy.Start`.
- `runner/cmd/fishhawk-runner/implementsandbox.go` — the signal-only `SignalProfile()` on every implement-stage agent spawn (implement, fix-up, conflict resolution; [#4858](https://github.com/kuhlman-labs/fishhawk/issues/4858)), § "Signal-only profile" below.

## Why an OS layer

The ADR-050 containment was carried ENTIRELY in environment variables — `HTTP(S)_PROXY` → the egress proxy, `NO_PROXY` cleared, `FISHHAWK_FORGE_WRITES=deny` (#3500), forge credentials withheld by `acceptenv`. Env binds cooperating processes only. In the #3393 incident a nested `fishhawk-runner` spawned with `env -i` inherited none of it; the runner's credential ladder (`gh auth token` reads the operator keyring, the backend installation-token endpoint is signature-authed off a run id) is NOT env-sourced, so withholding credentials from the env did not stop it, and it pushed to the real repository. A Seatbelt profile applied by `sandbox-exec(1)` is inherited by every descendant regardless of its env: a direct `connect()` to anything but the admitted loopback ports fails with `EPERM` in the kernel.

Neither control stops a descendant SIGNALLING a process outside its tree: in the #4855 incident an acceptance agent's `pkill` by name stopped the host `fishhawkd` and its own preview mid-stage. `WithSignalConfinement` closes that in the same profile (§ "Signal confinement" below).

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

### Signal confinement: `Profile(proxyAddr, allowHosts, WithSignalConfinement())` (#4855)

Renders immediately after `(allow default)`, BEFORE the credential section and the IP rules, whatever order the options are passed in:

```
(deny signal)
(allow signal (target same-sandbox))
```

A process under the profile can signal only processes in its OWN sandbox instance: everything it started (child, double-forked grandchild re-parented to launchd, a process group it created with `setpgid`) stays killable; the host `fishhawkd`, the acceptance preview, another stage's runner or agent, and a process under a SEPARATE `sandbox-exec` instance of the same profile are refused with `EPERM`. The allow is exactly the App Sandbox base profile's own (`/System/Library/Sandbox/Profiles/application.sb`) — no pid, process-name or path exception. Two rejected spellings: `(deny signal (target others))` did NOT deny an outside kill on Darwin 27.0.1 (the victim died, `kill` exited 0), and `(allow signal (target pgrp))` admitted an outside victim in the agent's process group. The runner applies this option on every applied acceptance profile; `Probe` carries the same clause, so a host that rejects the grammar reports `unavailable` (pre-spawn, loud) instead of failing `sandbox-exec` at agent spawn.

### Signal-only profile: `SignalProfile()` (#4858)

**Decision record.** This EXTENDS the accepted ADR-050 / [#4855](https://github.com/kuhlman-labs/fishhawk/issues/4855) acceptance containment to the implement-stage agents; the FO ruled it an extension, not a new ADR (campaign 6, [#4858](https://github.com/kuhlman-labs/fishhawk/issues/4858)). It is reversible at runtime with no rebuild: `FISHHAWK_IMPLEMENT_SIGNAL_SANDBOX=off`.

```
(version 1)
(allow default)
(deny signal)
(allow signal (target same-sandbox))
```

The same signal section as `WithSignalConfinement`, alone over `(allow default)`. There are NO network, file or mach rules: implement, fix-up and conflict-resolution agents need the network (model API, module proxies, the forge through their own credentials) and the whole toolchain, and their containment is the worktree and the workflow's path constraints, not egress. The only thing this profile takes away is signalling a process outside the agent's own sandbox instance — the #4855 incident shape (`pkill` by name stopping the host `fishhawkd`) on the stages that run most often. `TestSignalProfile_Golden` pins the four lines and the absence of `network`, `file-read` and `mach-lookup`.

`ProbeSignalConfinement(ctx)` proves ENFORCEMENT, not just presence; it reuses the package's one probe exec (`probeExec`), so the runner's spawn ratchet sees no new exec site. Its reasons (`TestProbeSignalConfinement_Rows`, one row each), all prefixed `signal-sandbox unavailable:`:

| Branch | Reason |
|---|---|
| non-darwin | `UnavailableSignalNonDarwin` (Seatbelt is macOS-only; a PID namespace, `unshare --pid --fork`, is the candidate) |
| `sandbox-exec` not on PATH | `sandbox-exec not on PATH (<err>)` |
| `sandbox-exec -p <SignalProfile> /usr/bin/true` fails | `sandbox-exec could not apply the signal profile (<err>)` — the exit-65 grammar rejection, and the exit-71 nested `sandbox_apply: Operation not permitted` when the runner already runs under a DIFFERENT profile |
| `/bin/kill -0 <runner pid>` under the profile SUCCEEDS | `profile applied but kill -0 of the runner's own pid was not refused: the host does not enforce (target same-sandbox), or this process already runs inside an identical sandbox instance (a nested identical apply is a no-op)` |
| the kill fails without `Operation not permitted` | `could not prove enforcement (<err>)` |
| the kill fails with `Operation not permitted` | available |

Signal 0 delivers nothing, so the probe is harmless on a host that does not enforce. The apply probe is load-bearing: without it, the exit-71 nested-apply text (which itself contains `Operation not permitted`) would read as a refused kill.

**Policy: `FISHHAWK_IMPLEMENT_SIGNAL_SANDBOX`** (runner-process config, dropped from the agent env by `agentenv`'s `FISHHAWK_` deny prefix):

| Value | Behaviour | Event (exactly one per stage configuration) |
|---|---|---|
| `auto` (default, empty) | wrap when `ProbeSignalConfinement` says available; otherwise spawn unwrapped, LOUDLY, reason printed | `implement_signal_sandbox_applied {mode, mechanism:"seatbelt", signals_confined:true}` / `implement_signal_sandbox_unavailable {mode, reason, signals_confined:false}` |
| `require` | unavailable fails the stage category-C BEFORE any spawn (the conflict-resolution stage too) | `implement_signal_sandbox_required {mode, reason}` → `runner_failed` |
| `off` | never probe, never wrap — the kill switch | `implement_signal_sandbox_disabled {mode, signals_confined:false}` |
| anything else | category-C pre-spawn | `implement_signal_sandbox_config {var, detail}` |

The implement stage also carries ONE `implement_signal_sandbox` event (`outcome`, `mode`, `reason`, `signals_confined`) in its uploaded raw trace bundle, so an operator can see a stage that ran UNCONFINED under `auto`; the stderr events above reach only the local runner log. Event wiring, the spawns wrapped and the conflict-resolution path: `runner/README.md` § "Implement-stage signal confinement (#4858)".

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

`Probe(ctx)`: darwin, `sandbox-exec` on PATH, and a deny-all profile carrying the signal section applies against `/usr/bin/true`. Anything else is `unavailable` with a reason naming what is missing. Non-darwin reports `UnavailableNonDarwin` (below).

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

With `WithSignalConfinement` composed with the credential and network rules (Darwin 27.0.1, `TestSeatbelt_SignalConfinement_RefusesOutsideProcesses`; every victim is a throwaway `/bin/sleep 3600.<unique>` — or, for `killall`, which matches by process name only, a uniquely named symlink to `/bin/sleep` — that the test started and first asserted alive; every by-name pattern is anchored to that throwaway):

| Under the composed profile | Result |
|---|---|
| `kill -TERM <outside pid>` (case `a`, the safe by-pid probe, run first) | `Operation not permitted`; victim alive |
| `kill -0 <outside pid>` (case `b`) | `Operation not permitted` (exists-but-forbidden, not `ESRCH`) |
| `pkill -TERM -f '^/bin/sleep 3600\.<unique>$'` (case `c`) | `signalling pid <victim>: Operation not permitted`; victim alive |
| `killall -TERM <unique name>` (case `d`) | non-zero; victim alive |
| kill / pkill of a victim under a SEPARATE `sandbox-exec` instance of the same profile (case `e`) | refused; victim alive |
| own child, double-forked grandchild, own `setpgid` group (case `f`) | killed (`SIGTERM`, exit 143) |
| admitted preview port; forge via the proxy (case `g`) | 200 |
| the same kill / pkill / killall under the profile rendered WITHOUT the signal section (case `h`, in-test counterfactual) | victim dies |
| real `claude -p` (2.1.314) and `codex exec` (0.153.4) each running one shell tool call under the composed profile, through the egress proxy (implement-stage smoke, #4855) | exit 0 |

With `SignalProfile()` alone (Darwin 27.0.1, the `signal_only` arm of the same test; same victims, same assertions):

| Under the signal-only profile | Result |
|---|---|
| cases `a`–`e` (outside kill, `kill -0`, `pkill`, `killall`, a separate instance) | refused; victim alive — identical to the composed arm |
| own child, grandchild, own `setpgid` group (case `f`) | killed (exit 143) |
| `curl` DIRECT to a loopback port the acceptance profile does not admit (case `g`) | 200 — the network is kept |
| the same kills under `(version 1)(allow default)` (case `h`) | victim dies |
| `ProbeSignalConfinement` on the same host | available |

Real-CLI smoke under `SignalProfile()` (#4858 step 9; run by hand, not committed). Prompt: one shell call that starts `sleep 30 &`, kills it and echoes the wait status.

| CLI (2026-11-12, macOS 27.0.1) | Runner flags | Own child killed | Session exit; EPERM text in stream | Direct parent-signal check |
|---|---|---|---|---|
| `claude` 2.1.323 | `--print --verbose --output-format stream-json --dangerously-skip-permissions` | `wait_status=143` | exit 0, result `success`; no `Operation not permitted` | UNOBSERVABLE |
| `codex` 0.166.0 | `exec --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check` | `wait_status=143` | exit 0; no `Operation not permitted` | UNOBSERVABLE |

Claude's first attempt never reached the sandbox: its own tool policy blocks a Bash command that STARTS with `sleep N`, so the recorded row used `true; /bin/sleep 30 & …`. The direct parent-signal check is UNOBSERVABLE, not "no parent signal": the calibration denial (`sandbox-exec -p <SignalProfile> /bin/kill -0 $$` → `Operation not permitted`) did not appear in `/usr/bin/log show --last 1m --style compact --predicate 'eventMessage CONTAINS "deny(1) signal"'`; the predicate's one match was `log`'s own argv line. (Use `/usr/bin/log`: zsh shadows the bare `log` with a builtin; `(deny signal (with report))` is rejected on a deny action.) The behavioural proxy is the normal, EPERM-free session exit above. The live walk's `verify_hint` names the adapter the operator verifies; the other adapter is intentionally unverified live, covered by this smoke and the darwin spawn e2e.

The setuid `/bin/ps` cannot be exec'd under ANY `sandbox-exec` profile on this host (`execvp … Operation not permitted`, the network-only profile included) — pre-existing and unrelated to the signal section; `pgrep` works.

Declared non-isolation: the `<home>/Library/Keychains` file-read clause is not behaviourally separable from the mach-lookup clause (each alone refused the read in planning probes); only the golden render (`TestProfile_CredentialDeny_Golden`) pins it.
| `sandbox-exec -p '(version 1)(allow default)' true` nested inside | `sandbox_apply: Operation not permitted`, exit 71 (residual 1) |

The e2e's case (d) is **NON-HERMETIC** (it dials a public IP literal); its assertion is the FAST exit-7, so a host without egress cannot green it by timing out — and the same `-m 3` fast assertion is kept on the counterfactual run.

## Policy: `FISHHAWK_ACCEPTANCE_NET_SANDBOX`

| Value | Behaviour | Event (exactly one per stage) |
|---|---|---|
| `auto` (default, empty) | apply when `Probe` says available; otherwise proceed env-only, LOUDLY | `acceptance_net_sandbox_applied {mechanism:"seatbelt", admitted_ports, signals_confined:true}` / `acceptance_net_sandbox_unavailable {reason, enforcement:"env-only", signals_confined:false}` |
| `require` | unavailable fails the stage category-C BEFORE any spawn | `acceptance_net_sandbox_required {reason}` → `runner_failed` |
| `off` | never probe, never wrap — the kill switch (for the signal section too: it has no separate switch) | `acceptance_net_sandbox_disabled {signals_confined:false}` |
| anything else | category-C pre-spawn | `acceptance_net_sandbox_config` naming the valid values |

A profile render error (`acceptance_net_sandbox_profile`) is also category-C: never spawn under a malformed profile — including a `WithCredentialDeny` resolution error (a non-not-exist `EvalSymlinks` failure on a credential path). The runner passes `WithCredentialDeny` only while `FISHHAWK_ACCEPTANCE_CREDENTIAL_ISOLATION` is not `off` (decision, event fields and kill-switch contract: `runner/README.md` § "Acceptance-stage egress containment"); under `off` the profile is byte-identical to the signal-only render (`WithSignalConfinement` plus the network rules). The variable is runner-process config and is dropped from the agent env by `acceptenv`'s default-deny allow-list (`TestAcceptenv_ExcludesPreviewVars`).

## Residuals (stated, not closed here)

1. **A nested `sandbox_apply` of a DIFFERENT profile is refused inside the profile** (exit 71); a nested apply of the IDENTICAL profile exits 0 but is a no-op — it runs in the same sandbox instance (re-observed on Darwin 27.0.1 in #4858 planning). If the operator's Claude Code settings enable the built-in Bash sandbox, EVERY Bash tool call in the acceptance agent — and, under #4858, in the implement, fix-up and conflict-resolution agents — fails. Set `FISHHAWK_ACCEPTANCE_NET_SANDBOX=off` / `FISHHAWK_IMPLEMENT_SIGNAL_SANDBOX=off` on the runner env (no rebuild) or disable that setting. Probe command: `sandbox-exec -p '(version 1)(allow default)(deny network-outbound (remote ip "*:*"))' sandbox-exec -p '(version 1)(allow default)' /usr/bin/true` exits 71.
2. **Unix-domain sockets are NOT denied outside the credential denies** — `(remote ip "*:*")` does not cover them. With `WithCredentialDeny`'s `SSHAgent`, sockets under the real home (Docker Desktop's `~/.docker/run/docker.sock`, gpg-agent, the 1Password agent), the runner's `SSH_AUTH_SOCK` and the launchd `Listeners` pattern ARE denied; a socket elsewhere (`/var/run/docker.sock`) stays reachable, and is an escape path (a container with network). Closing the rest needs an allow-list of the agent's own IPC sockets; follow-up. Behaviour change: an acceptance agent that relied on the home-dir docker socket breaks — the kill switch is `FISHHAWK_ACCEPTANCE_CREDENTIAL_ISOLATION=off`.
3. **Linux is unavailable** (`UnavailableNonDarwin`): gateiso's `unshare -n` creates a namespace with NO route to the host loopback the proxy binds — the verify gate wants "no network at all", acceptance wants "host loopback only, everything else denied" — so the mechanism does not transfer and enforcement there stays env-only, reported by the loud `acceptance_net_sandbox_unavailable` event. A Linux equivalent (e.g. a per-process nftables/cgroup rule) is a follow-up.
4. **`sandbox-exec(1)` is documented as deprecated by Apple** (`man sandbox-exec`) yet ships on current macOS and is the same mechanism Claude Code's own macOS sandbox uses (<https://github.com/anthropic-experimental/sandbox-runtime>). The darwin e2e fails if a future macOS removes or changes it; `Probe` then reports unavailable and `auto` degrades loudly.
5. **DNS inside the sandbox.** Hostnames must reach the proxy by name via CONNECT (Node's undici `ProxyAgent` forwards the hostname without a local lookup), which is how the model API is reached today. If the agent CLI performs a non-proxied pre-flight lookup that now hard-fails, acceptance stages on macOS break under `auto` — validated by the operator's first live acceptance dispatch after the #3086 runner rebuild, with `off` as the kill switch.
6. **The `.gitconfig` deny and a re-pointed HOME.** Under `Files`, `<real home>/.gitconfig` is unreadable. A descendant that drops the git pins (`GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`) AND keeps or restores `HOME=<real home>` — `env -i HOME=<real> git status` — exits 128 (`fatal: unable to access '<real>/.gitconfig': Operation not permitted`); with HOME unset, or with the pins, git exits 0. It bites mainly on `auto`'s non-isolated path (no env-carried model credential, so HOME stays real). Kill switch: `FISHHAWK_ACCEPTANCE_CREDENTIAL_ISOLATION=off` (no rebuild).
7. **A pre-existing hard link** to a credential file elsewhere is not covered (a hard link has no path relation to the denied one). An agent cannot MANUFACTURE one: `ln` and `cp` of a denied file fail `Operation not permitted`.
8. **Symlinks are resolved once, at render.** A new alias the agent creates is covered (it resolves to a denied vnode path); a symlink the OPERATOR re-points mid-stage is not re-resolved.
9. **Linux**: none of the credential denies apply (residual 3); the synthetic HOME is the only control there, and absolute-path reads, OpenSSH (which reads the passwd home, not `$HOME`) and Secret Service over D-Bus remain reachable.
10. **Signal confinement is macOS-only.** On macOS it covers the acceptance stage (#4855) and, through `SignalProfile()`, the implement, fix-up and conflict-resolution shells (#4858). On Linux (residual 3) every one of those agents can still signal any same-user process, including the host `fishhawkd` and its preview; `UnavailableSignalNonDarwin` is printed on every Linux implement spawn. A PID namespace (`unshare --user --pid --fork --mount-proc`) is the candidate, but some distros restrict unprivileged user namespaces (Ubuntu 24.04's AppArmor restriction), so it needs its own probe and design; follow-up.

Implement-stage residuals (#4858), each with the kill switch `FISHHAWK_IMPLEMENT_SIGNAL_SANDBOX=off`:

11. **`ps` cannot exec in a confined implement shell.** The setuid `/bin/ps` is refused (`execvp … Operation not permitted`, exit 71) while `pgrep` works. Tests that exec `ps` skip loudly on a REAL exec probe (procsweep, lockholder, the lineage-lock integration test); `scripts/dev`'s live-run scan fails closed and its port-owner diagnostics print `?`. List: `runner/README.md` § "Expected confined-shell failures".
12. **Nested sandboxes** (residual 1): an operator-enabled Claude Code built-in Bash sandbox breaks, and the acceptance-profile e2es in this package Probe-skip inside a confined shell (a different profile cannot be applied), while a nested IDENTICAL apply is a no-op — so `ProbeSignalConfinement` reports the "not refused" reason there, never a false available.
13. **Cross-invocation liveness in `scripts/dev` and `scripts/test` reads only `ESRCH` as dead**: a confined shell gets `EPERM` for every pid outside its sandbox, which must read live. Contract: `scripts/README.md`.
14. **Signal confinement does not protect pid, lease or lock FILES** — the profile has no file rules, so a confined agent can still write them; it only stops the agent signalling the processes they name.

## Relationship to ADR-063

`gateiso.SandboxUnavailableNonLinux` says macOS has no unprivileged no-network sandbox primitive. That statement concerns the VERIFY GATE's fresh-namespace need (no loopback at all) and stands; the acceptance case is the opposite shape, which Seatbelt expresses and `unshare -n` cannot.
