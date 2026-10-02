---
id: ADR-063
title: "Isolate untrusted gate-command execution: container sandbox for .git-metadata, egress, and host-filesystem containment"
status: accepted
issue: https://github.com/kuhlman-labs/fishhawk/issues/2127
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-063: Isolate untrusted gate-command execution: container sandbox for .git-metadata, egress, and host-filesystem containment

## Context

Every runner gate that executes untrusted repository-supplied code does so in a `git worktree add --detach` checkout, running the command via `sh -c` with `cmd.Dir = wt`. The command is contained for THREE things — credentials (the default-deny env allow-list, ADR-029 / #650 item 4), wall-clock (bounded timeout), and process tree (process-group kill) — and for NOTHING ELSE. That leaves three untrusted-execution vectors open, all rooted in the same gap: the command runs directly on the host as the same OS user as the runner.

1. **`.git` metadata sharing (the originating finding).** A LINKED worktree shares refs, config, and hooks with the primary repository. An untrusted command can write `.git/hooks/*`, rewrite `config` (`core.hooksPath`, url rewrites, credential helpers), or manipulate refs and locks — and the runner's SUBSEQUENT TRUSTED operations (commit, push) then execute that content while holding an installation token. This is a privilege escalation, not merely a containment inconvenience.

2. **Network egress.** The gate subprocess has unrestricted network access AND read access to the (private) repository contents it is measuring. That completes the untrusted-input + sensitive-data + exfiltration-egress triangle even with runner credentials stripped from the env. (Added as an amendment to the original metadata-only framing — see the issue comment thread.)

3. **Host-filesystem read.** Because the command runs on the host, it can read files OUTSIDE its checkout — `~/.ssh/*`, other repositories, ambient environment — regardless of worktree or clone isolation. Neither a linked worktree nor a throwaway clone closes this; a clone only gives the command its own `.git`.

AFFECTED CALL SITES (all pre-existing, verified in-tree at 7d008c6a):
- `runner/cmd/fishhawk-runner/main.go:4324` — committed-tree verify gate: `git worktree add --detach` then `sh -c verifyCmd`.
- `runner/cmd/fishhawk-runner/main.go:5008` — the second committed-tree gate, same pattern.
- `runner/cmd/fishhawk-runner/acceptancetree.go` — `git worktree add --detach` against the operator's dispatch checkout.
- The `diff_coverage` constraint (E46.3 / #1888), which reuses this posture for parity.

DISCOVERED: raised as a high-severity finding by gpt-5.6-sol in run `4d0bfef2-...` (PR for #1888); a test planted a lock in the PRIMARY repo's `.git/refs/heads` from the throwaway worktree, demonstrating reach into shared metadata. NOT introduced by #1888 — the verify-gate precedent was verified in-tree before merge; #1888 achieves parity with the established posture, and this ADR is about the posture itself.

Filed at `autonomy:low`: a containment/security posture change touching every gate execution path warrants human-led review per METHODOLOGY, and the options differ materially in cost.

## Options

1. **ACCEPT (status quo).** Document the boundary; contain credentials/wall-clock/process-tree only. Cheapest; leaves the hooks-to-trusted-push escalation, egress, and host-fs-read all open.

2. **HARDEN IN PLACE.** Keep linked worktrees; neutralize the metadata vectors (`core.hooksPath` → empty dir, `GIT_CONFIG_GLOBAL`/`GIT_CONFIG_SYSTEM` → /dev/null — the idiom `scripts/test` already uses — plus fail-closed on any post-command ref/config/hook delta). Blocklist-shaped: relies on enumerating every vector. Does not address egress or host-fs-read.

3. **FULL ISOLATION (throwaway clone per gate).** Each gate command gets its own clone with an independent `.git`, so metadata is unshared by construction. Not vector-enumeration for the metadata axis. Does NOT close egress or host-fs-read — the command still runs on the host with host network and host filesystem visibility.

4. **VERIFY-AFTER.** Snapshot primary refs/config/hooks before, compare after, fail on delta. Detection not prevention; racy while the command runs. Complements 2/3.

5. **CONTAINER ISOLATION (chosen).** Run the gate command in a `--network=none` container with ONLY the checkout-to-measure mounted (report dir bind-mounted writable for the result). Closes all three vectors at once: no primary `.git` is visible (metadata), `--network=none` is a uniform cross-platform egress cut, and nothing outside the mount is readable (host-fs-read). Resource limits come for free. Fit is good — the repo already depends on a container runtime (testcontainers Postgres #1174, CI docker-build, `scripts/dev k8s`). Two real caveats: the runner must be able to invoke a container runtime WITHOUT handing the untrusted container the host Docker socket (a container-escape path); and the customer `diff_coverage` case needs the customer's toolchain in the image.

## Recommendation

Adopt Option 5 (container isolation) as the primary mechanism, with Option 3 + a no-network sandbox as the documented fallback for runner topologies that cannot safely nest a container. Apply it at ONE shared helper covering all four call sites — the current per-gate duplication is why the diff-coverage gate inherited the posture silently. The hooks vector deserves closing first regardless, as it is a token-holding privilege escalation.

## Decision

**ACCEPTED — container isolation primary, clone+sandbox fallback.**

**Primary: container isolation.** The shared gate-exec helper (`runBoundedGateCommand`, the seam all four call sites route through) runs the untrusted command in a container:
- `--network=none` — uniform egress cut, identical on Linux and macOS (Docker Desktop). This RESOLVES the per-platform egress problem that the OS-level-sandbox fallback carries (see below): no `unshare -n` vs `sandbox-exec` divergence.
- Only the checkout-to-measure is mounted (read-only where the command permits; the WIP/commit dance that produces the committed tree stays runner-side, outside the container). A writable bind mount carries the coverage/verify report back out. No primary `.git`, no host home, no sibling repos are visible.
- Existing containment (bounded timeout, process-group semantics via the container lifecycle, default-deny env) is preserved; the container is an additional boundary, not a replacement.
- Fishhawk's own gates run in a known build image (extend `backend/Dockerfile`'s toolchain, or a dedicated gate image). The customer `diff_coverage` case requires a new **image** field alongside the already-declared coverage command — until that field ships, containerized `diff_coverage` is Fishhawk-image-only, and a customer command with no image declared uses the fallback.

**Fallback: clone-per-gate + no-network sandbox.** For runner topologies that cannot safely nest a container — the runner is itself containerized with no access to a nesting-safe runtime — fall back to a throwaway clone (metadata isolation by construction) plus an OS-level no-network sandbox. In this path egress control is OS-dependent and NOT uniform: clean on Linux (network namespace / `unshare -n`), a documented gap on the macOS local dogfood runner (no namespace equivalent; `sandbox-exec` is deprecated). The fallback also does not close host-fs-read. It is strictly weaker than the container path and exists only so a gate never silently reverts to the fully-unisolated status quo.

**Hard requirement on the container path — never the host Docker socket.** The gate container must be launched by a runtime the runner can invoke without granting the untrusted container access to the host daemon: a host-direct runtime, rootless Podman, or a nesting-safe runtime (e.g. sysbox). Mounting `/var/run/docker.sock` into (or making it reachable from) an untrusted-command container is a container-escape path and is worse than today's posture — it is explicitly forbidden. If no safe launch path exists on a given runner, that runner takes the fallback.

**Selection is runner-capability-driven, at the shared helper:** container path when a safe runtime is detected; fallback otherwise; and the selection plus which path ran is recorded on the gate evidence so an operator can tell an isolated run from a fallback run.

## Consequences

- A container runtime becomes a runner requirement wherever the container path is used; the fallback keeps gates functional where it is absent, at reduced isolation. This must be reflected in the self-hosted / single-tenant distribution profile (#1833) and the runner setup docs.
- The two coverage gates upgrade from **tamper-evident** to **isolated**: #2124 (patch-coverage, tamper-evident via a parent-memory digest) and #2129 (diff_coverage's residual measured-zero shapes) both cite "full prevention needs the process/filesystem isolation ADR-063 will decide." That prevention is this decision. Their waived residuals (profile forgery, same-user snapshot tamper) close once the gate command can no longer reach those paths.
- A new customer-facing `diff_coverage` schema field (the container image) is required for the customer case; scope it as an additive `workflow-v1.x` change per the schema-change checklist, following the same shape #1888 used.
- Any FUTURE gate that executes repository-supplied commands inherits isolation for free by routing through the shared helper — the copy-an-existing-call-site drift that let diff_coverage inherit the unisolated posture is closed.
- Related: ADR-029 / #650 (env-sanitization half of gate containment); this ADR is the filesystem/metadata/egress half that was never specified. E46 follow-ups #2124, #2125, #2129 depend on this for their full close.

**Implementation** is a follow-up (or set of follow-ups) filed against this ADR: the shared-helper container path + runtime detection + fallback, the gate-evidence path-recording, the `diff_coverage` image field, and the self-hosted-profile doc update. Human-led review per `autonomy:low`.
