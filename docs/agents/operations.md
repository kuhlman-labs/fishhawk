# Local operations and shell traps

Use the relevant operational skill; its operator guard is mandatory. Read only
the subsection relevant to the failure. [scripts/README.md](../../scripts/README.md)
owns the rebuild/activation tables, guards, flags and their residuals.

## Stack lifecycle

- `deploy-local` owns up/reload/k8s; `sync-main` owns post-merge pulls;
  `teardown-local` stops services; `dev-clean` lists and removes litter.
  Data-volume destruction (`make nuke`) requires an explicit request.
- Reload/post-merge refuse live runners and restart-blocking reviews. Post-merge
  also refuses undispatched decomposition children; reload does not pull and
  therefore ignores that blocker. Respect both process and daemon checks;
  passing a process check alone is insufficient. Force requires explicit
  authorization. `reload --when-quiet` is the bounded waiting route.
- Post-merge pulls ff-only, cleans merged work, optionally checks the issue,
  trims an oversized cache under guards and reloads last. Reload forces all
  binaries because the main diff after a merge is empty.
- Rebuilt does not mean activated. Verify GitSHA and listener identity, not only
  health status. Runner invocations pick up the new binary per stage; MCP child
  swapping must be confirmed by a version-returning call. A shim binary update
  requires reconnect. Read the rebuild/activation tables and MCP banner rules
  in scripts/README. Schema-major changes can need reconnect even without a build.
- `up` refuses occupied ports and verifies the per-spawn nonce. Do not weaken
  readiness to a generic healthy endpoint. `down` must identify its listener.
- Never `go clean -cache` beside live builds. Operator scratch worktrees should
  use GOFLAGS=-trimpath. Guarded `scripts/dev gocache --trim` removes aged entries;
  it refuses when toolchain/run liveness is unknown or active (#3901/#4180).
- A runner self-fix can bootstrap-deadlock: build the fixed runner from a detached
  scratch worktree, verify it and re-dispatch; never switch the run's lineage
  checkout underneath it. [runner bootstrap procedure](../../runner/README.md), #3086.

## Host and container diagnostics

- Check uptime/core count before attributing unrelated test failures to code.
  PID-1 orphan CPU consumers require ownership checks before removal. Runner
  orphan sweeping is sampled, not complete; its host-overload classification
  does not prove that every failure is environmental. Use `diagnose-red`.
- A newer host Go can differ semantically from the pinned toolchain (e.g.
  jsonv2). Run the host checker and use the pinned tools or image. Host Go env
  files do not repair a network-isolated container's baked-in toolchain.
- A locked macOS screen can block credential helpers during public Docker pulls.
  The runner uses its own credential-free config; interactive pulls and the
  host `--in-gate-image` path can still block. Unlock, or follow the public-image
  credential-free config procedure (nonempty dummy auth entry, no helpers and
  explicit socket); private images need the separate bounded credential path.
  [Self-hosted deployment](../deploy/self-hosted.md),
  [gate isolation](../../runner/internal/gateiso/README.md), #4046/#4337.
- TLS-inspecting egress can make container go mod download reject the proxy CA
  while host downloads work. Follow the CA procedure in
  [Kubernetes deployment](../deploy/kubernetes.md), not a TLS-verification bypass.
- Verify pinned image tags exist before landing. Tags and whole repositories
  can disappear. A registry-probe sentinel must be a missing tag in an existing
  repository: a nonexistent repository can return 401 and make the probe
  indeterminate. Report skipped registry checks; inspect hook Jobs on Helm failure.
- Docker Desktop's kind node can use a separate image store. Preserve the
  fail-closed k8s provisioner/load/rollout/config-digest + GitSHA identity check.
  Dirty builds can share a GitSHA. `docker image inspect .Id` can name an index
  or manifest, not a config digest; resolve the saved archive's Config entry.
  Skip buildx attestation entries in an index walk. See Kubernetes deployment
  § Image identity (#3344/#3530); bypass only with explicit authorization.
- iCloud eviction/conflict copies matter only for a checkout actually located
  in a synced directory. Check cwd; do not assume ~/Documents. Rehydrate a
  dataless file; list conflict-copy candidates before any deletion.

## Shell and image authoring

- macOS ships bash 3.2: use zsh/awk when associative arrays are needed.
- Under zsh set -e use `(( ++i ))` or assignment, not `(( i++ ))` from zero;
  use an if statement instead of a failing condition-and-assignment chain.
  scripts/dev's ZERR diagnostic and scripts/test-dev pin this behavior.
- Never pipe into quiet grep under pipefail: its early exit can SIGPIPE the
  writer. Use a here-string in production scripts. Harness `grep_q` helpers
  drain pipes and must be defined locally; do not invoke them on a terminal.
  `scripts/check-pipefail-grep` enforces the rule; documented exceptions need
  the same-line allow annotation with a reason.
- Container builds derive GOARCH from BuildKit TARGETARCH declared in the using
  stage, never a literal amd64 (#2912).
