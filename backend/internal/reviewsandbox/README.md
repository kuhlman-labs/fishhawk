# reviewsandbox

Constrains what a local review adapter (`backend/internal/codex`,
`backend/internal/claudecode`) hands its reviewer subprocess: a scrubbed child
environment (`Env`) and an exported, read-only source tree (`ExportTree`).
Both exist to bound what a tool-enabled reviewer — which processes untrusted
diff and issue text — can reach. Introduced by #2486 (E48.59).

## Env — the environment scrub (lands first, stands alone)

Both review adapters used to seed the child with a wholesale `os.Environ()`, so a
tool-enabled reviewer held `FISHHAWKD_DATABASE_URL`, `GITHUB_TOKEN`, and every
API key in the daemon's environment. `Env(parent, allow, passthrough)` filters
that down to entries whose **exact** name appears in `allow ∪ passthrough` — no
prefix matching, no wildcards, so a secret whose name merely shares a prefix with
an allowed name (`PATH_EXTRA` vs `PATH`) is never leaked. Input order and
last-wins duplicate semantics are preserved.

Per-adapter allow-lists (exported vars):

- `BaseAllow`: `PATH HOME USER LOGNAME SHELL TMPDIR TERM LANG LC_ALL TZ` and the
  proxy vars (`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` + lowercase). PATH resolves
  helpers; HOME/USER/LOGNAME/SHELL locate host config and identify the user;
  TMPDIR/TERM/LANG/LC_ALL/TZ shape scratch/locale/terminal; the proxy vars are
  corporate egress.
- `ClaudeAllow` = Base + `ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN
  ANTHROPIC_BASE_URL CLAUDE_CONFIG_DIR` (credential, endpoint, config dir).
- `CodexAllow` = Base + `OPENAI_API_KEY OPENAI_BASE_URL CODEX_HOME`.

Anything a specific deployment additionally needs — Bedrock/Vertex routing vars,
an unusual CA-bundle var — is named EXPLICITLY via
`FISHHAWKD_REVIEWER_ENV_PASSTHROUGH` (comma-separated exact names), never by
widening a list to a prefix. The failure mode of an over-tight list is a loud
reviewer auth error, not a silent wrong verdict.

## ExportTree — export, do not mount

`ExportTree(ctx, repoDir, ref, limits)` resolves `ref` to a commit and streams
`git -C repoDir archive --format=tar <sha>` through the stdlib `archive/tar`
reader into a throwaway `os.MkdirTemp` dir. It returns that dir, the **resolved
commit SHA** (C4 — resolved ONCE here and handed back so the caller names in the
prompt the exact commit that was archived, never a second HEAD resolution), the
extraction `Stats`, and a cleanup closure the caller MUST call.

Export-not-mount gives the reviewer a cleaner WORKING DIRECTORY than pointing it
at the live checkout: only **tracked files at that commit** — no `.git`, no
untracked files, no other branches, no operator-personal working-tree state.
Read this precisely: the export bounds what is CONVENIENT, **not** what is
REACHABLE. It is not a jail — see "Accepted residual risks" below and #2522.
An earlier draft of this file (and ADR-078) claimed export-not-mount gives
"LESS exposure"; that claim was wrong and is corrected here. Both
git children run under `Env(os.Environ(), BaseAllow, nil)`, inheriting no repo
credentials. On ANY error (unresolvable ref, git absent, not a work tree, a
bounds violation, a traversal entry, a non-zero git exit) it removes its own
partial dir and returns a no-op cleanup; the caller degrades to an ungrounded,
diff-only review.

### Named extractor guards (each counts or refuses)

- **path traversal**: an absolute path or one escaping the root via `..` is
  REFUSED (error, nothing written).
- **symlink / hard link**: `tar.TypeSymlink`/`tar.TypeLink` entries are SKIPPED
  and counted (`Stats.Symlinks`) — a symlink could point outside the tree and a
  link misrepresents the commit.
- **agent-instruction files (C1)**: SKIPPED and counted (`Stats.Instructions`).
  Enumerated against both CLIs' documented discovery: `AGENTS.md` and
  `AGENTS.override.md` (codex — the `.override.` variant is codex's local-only
  override, checked BEFORE `AGENTS.md` at each directory level, so it loads as an
  instruction exactly like `AGENTS.md`; its omission was the reopened C1 bypass
  in the #2486 fix-up), `CLAUDE.md` and `CLAUDE.local.md` (claude-code), at ANY
  depth, plus everything under a `.claude/` or `.codex/` directory (the CLIs'
  config/state dirs: settings, commands, agents, skills, `config.toml`). This
  closes an
  approval-laundering channel: this repo's conventions REQUIRE ordinary PRs to
  edit `AGENTS.md`, so without the skip a PR editing it would boot its own
  reviewer running the text under review — no attacker needed, only an agent
  optimizing for a green review. The reviewer still sees any change to these
  paths in the DIFF as data to judge; the tree simply never loads them as
  instructions.
- **file-count / byte bounds**: refuse once the running total exceeds
  `Limits` (defaults 50000 files, 512 MiB), degrading a runaway monorepo to
  ungrounded rather than filling the daemon's disk.

The skip counters make an incomplete tree observable: the review prompt discloses
them (C3) so a reviewer never treats a tree-wide search over a tree missing
symlinks or agent-instruction files as exhaustive.

## Grounding is a per-loop, all-or-nothing decision

The server (`backend/internal/server/review_grounding.go`) grounds a review loop
only when the export succeeded AND **every** resolved reviewer implements the
`ReviewGrounded` capability (`allInvocationsGrounded`). A mixed panel — one
grounding-capable reviewer and one not (the anthropic SDK adapter cannot be
handed a local dir) — is ungrounded for EVERYONE, so the prompt is never
asymmetric (claiming a tree half the panel cannot read) and the review is never
partially grounded. Cleanup ownership follows the dispatch shape: the synchronous
gating path defers cleanup in the dispatch scope; the DETACHED advisory path
hands cleanup into the goroutine (C6) so the export survives the detached
reviewers' lifetime and is removed exactly when the loop returns.

## Accepted residual risks (advisory-verdicts-only)

- **Grounding is NOT a filesystem jail — out-of-tree reads are not denied.**
  Neither adapter confines the reviewer's reads to `treeDir`. `cmd.Dir` and
  `--add-dir` SELECT/ADD a workspace; they do not fence the filesystem. On the
  codex path `--sandbox read-only` denies writes and network but PERMITS reads
  filesystem-wide (codex has no read-confining sandbox mode). On the claude path
  `--tools Read,Grep,Glob` + `--allowed-tools` pre-approve those tools with no
  per-path restriction, so a Read of an absolute out-of-tree path is not
  prompted. An untrusted diff can therefore steer either reviewer toward a
  host-readable file outside `treeDir` (e.g. `~/.ssh`, a sibling checkout), and
  the content can surface in the ADVISORY verdict. This is why the contract
  tests assert argv/cwd but do NOT assert that an out-of-tree read is denied:
  it is not denied, and a test claiming otherwise would be false. What actually
  bounds the exposure is defence-in-depth, not confinement:
    - **export-not-mount** minimises what is discoverable *in*-tree (no `.git`,
      no untracked files, no other branches, no operator working-tree state);
    - the **env scrub** removes env-resident daemon secrets (it does NOT protect
      file-resident secrets — see below);
    - **no network** — codex's read-only sandbox and the claude adapter's
      empty-MCP set + absence of Bash/WebFetch/WebSearch from `--tools` — cuts
      the EXFILTRATION leg of the lethal trifecta, so out-of-tree content can
      reach only the advisory verdict (read by the operator and the model
      provider), not an attacker-controlled sink;
    - **advisory-only verdicts** with operator arbitration of every split.
  A true out-of-tree read DENIAL needs OS-level sandboxing (seccomp/Landlock/
  Seatbelt bind-mount jail), which is infrastructure this package does not own —
  tracked with the runner-sandbox work (#611). Tolerable ONLY while verdicts are
  advisory; revisit before any move to binding reviewer verdicts.
- A grounded reviewer reads untrusted diff content and repo-resident files. This
  is tolerable ONLY while reviewer verdicts are ADVISORY and an operator
  arbitrates every split. Revisit before any move to binding reviewer verdicts.
- The per-adapter config-dir passthroughs (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`)
  are KEPT, not redirected to a throwaway dir: redirecting them can break
  subscription-based host auth (the dogfood loop's common posture). The residual
  is that a grounded reviewer may load the operator's personal agent
  INSTRUCTIONS from those dirs. Kept deliberately; recorded here as the trade-off.
- codex's read-only sandbox grants read-only SHELL execution (no network), not
  merely file reads — the narrowest posture the codex CLI can express. "Never
  shell" is met only in the sense that nothing can write or reach the network.

## claude-code: operator MCP tools no longer inherited (#2486 fix-up) — CLOSED

For the claude-code adapter, inheriting the operator's MCP TOOLS was formerly a
residual and is now CLOSED — do NOT describe it as an accepted residual. The
grounded claude-code adapter's `--tools Read,Grep,Glob` bounds only the BUILT-IN
toolset; MCP tools are not built-ins, so they loaded from the operator's config
and survived the restriction (verified live — a grounded child enumerated
browser/Gmail/GitHub MCP tools, which are network egress and data exfiltration
that defeat the never-network property). The grounded claude argv now ALSO pins
an EMPTY MCP server set: `--strict-mcp-config` makes `--mcp-config` the sole
source of MCP config (ignoring `~/.claude` and project `.mcp.json`) and the
empty `{"mcpServers":{}}` document loads zero servers; grounding (the tree read)
is preserved with both flags on.

## codex: operator MCP not adapter-neutralized — RESIDUAL (no-network bounds it)

The codex adapter CANNOT close the MCP channel the same way, and it is a mistake
to claim it does (the overclaim the #2486 fix-up corrected). codex offers no
reliable per-invocation MCP-clear: a `[mcp_servers.*]` block in the operator's
`~/.codex/config.toml` (reached via the passed-through `CODEX_HOME`) is not
overridable through `-c` today (openai/codex#13076), and the empty-table trick
the claude path uses has no codex equivalent. So the codex adapter adds NO MCP
flag, and a grounded codex argv test can pin only that the ADAPTER injects no
MCP config of its own — it does NOT, and with a subprocess-level fake CANNOT,
prove the operator's config-resident MCP servers are absent from the child.

What bounds the exposure instead is the read-only sandbox's **no-network**
guarantee (`--sandbox read-only`): even if codex loaded a config-resident MCP
server, a network-egress tool it exposed is denied the network. That is the
load-bearing control the codex grounded-argv test asserts (the `--sandbox
read-only` pin), NOT the argv MCP scan. Two empirical facts keep the residual
small today — `codex exec` reports NONE for MCP tools, and codex-run tooling is
inside the no-network sandbox — but if a future codex-cli begins honoring
config MCP servers with out-of-sandbox egress, this becomes a real gap. Kept
deliberately as a residual pending a codex-side MCP-clear (openai/codex#13076)
or the OS-sandbox confinement tracked with #611.

## Grounding ships DORMANT — `FISHHAWKD_REVIEW_GROUNDING` defaults to FALSE

Grounding is **off by default** (#2522). Reviewer reads are not confined to the
export, so enabling it hands a reviewer that is processing untrusted diff content
the ability to read any file the daemon user can read, with its verdict text as
an egress path into the audit log and PR comments. Verified live against the
shipped argv: claude read an out-of-tree absolute path with `permission_denials:
[]`, and codex read the same file AND listed `~/.ssh`, returning `id_ed25519`.

Set `FISHHAWKD_REVIEW_GROUNDING=true` to opt in. That is a deliberate operator
choice appropriate only for a trusted single-tenant host, and it should be
revisited before multi-tenancy (E44). #2522 adds the per-adapter READ BOUNDS
described below, but **the default STAYS FALSE**: nothing in this file is
load-bearing at merge, and the flip is a SEPARATE operator-filed follow-up gated
on a recorded operator run of the live harness passing on BOTH adapters. The
`serve_test.go` "unset: grounding OFF" subtest pins the current default so it
cannot be undone silently.

The environment scrub is INDEPENDENT of this flag and is ALWAYS applied — it has
no plausible reason to roll back, and it stands on its own merits.

## Reviewer read bounds (#2522) — TWO mechanisms, NOT the same strength

`Env` scrubs the child ENVIRONMENT and `ExportTree` bounds what the reviewer is
POINTED at. Neither bounds what a tool-enabled reviewer can ASK the filesystem
for. `confine.go` adds that bound — **per adapter, and the two are not
equivalent**. This asymmetry is load-bearing and is never collapsed into one
word here or in `docs/ARCHITECTURE.md`:

| Adapter | Mechanism | Enforcement | Honest label |
|---|---|---|---|
| codex | `confined` permission profile in a synthesized `CODEX_HOME`, selected by `--profile confined` | OS-level, deny-by-default ALLOWLIST (an out-of-tree read returns EPERM, `Operation not permitted` — not a model refusal) | **confinement** |
| claude | `--disallowed-tools` rules over a FIXED set of credential roots | TOOL layer, BLOCKLIST | **defence-in-depth, NOT confinement** |

### codex: the synthesized confined `CODEX_HOME`

`--sandbox read-only` means *write nowhere, read anywhere* — operator-verified:
the reviewer read an out-of-tree file straight through it, and `-c
sandbox_permissions=[]` did not change that. The bound that holds is codex-cli's
permission-profile subsystem. `CodexConfinedHome` synthesizes a throwaway
`CODEX_HOME` holding the operator's own `config.toml` **verbatim** (so
`model_provider` / `base_url` / `model` survive) plus this block, written into
BOTH `config.toml` and `confined.config.toml`:

```toml
default_permissions = "confined"

[permissions.confined.filesystem]
":minimal" = "read"
"<canonical export dir>" = "read"
"<canonical verdict schema path>" = "read"
```

`":minimal"` is the base system read set. **Without it every shell command
aborts with SIGABRT (exit 134)** — including reads of ALLOWED paths — because
`/bin/zsh` and its dylibs become unreadable. It excludes `$HOME`, so the
operator's home is out by DEFAULT rather than by enumeration.

**PLACEMENT — why the synthesized home is created INSIDE the real
`CODEX_HOME`.** It holds a COPY of the operator's `auth.json`. A bare
`os.MkdirTemp("")` would put that copy under `${TMPDIR}` — the one region the
claude blocklist deliberately does not deny — moving a live credential OUT of a
denied root. So it is created inside the resolved real `CODEX_HOME` at 0700, and
`errConfinedHomeOutsideDeniedRoot` REFUSES the grounded codex path outright when
that resolves outside every applicable deny root (an operator with
`CODEX_HOME=/tmp/foo` gets a loud refusal, never a silent egress path).

**THE CREDENTIAL IS NOT IN THE GRANT.** The `filesystem` table above lists the
export and the schema — **not** the synthesized home. codex-cli reads its own
`CODEX_HOME` config and auth as the PROCESS, outside the profile's tool layer
(operator-verified against 0.144.1: "codex read its own `CODEX_HOME` auth
normally under the profile"), so authentication survives while the copied
credential stays OUTSIDE the reviewer-readable set.

Two tests carry that claim, and they prove DIFFERENT things — read the split
literally, because the first one alone would be a documented protection that does
not hold:

- `TestCodexConfinedHome_CredentialNotGrantedToToolLayer` (hermetic, blocking)
  asserts that no emitted grant covers `auth.json` while the export and schema
  grants ARE present. That is a statement about what the BUILDER writes, and
  says nothing about what codex-cli enforces.
- `TestLive_CodexCredentialUnreadableWhileAuthenticated` (opt-in, NON-blocking)
  is the behavioural half: it asks a real confined reviewer to read the copied
  credential by its literal path AND through `$CODEX_HOME/auth.json` in the
  shell, and asserts BOTH that the invocation authenticated and ran a model turn
  AND that no value from the operator's real `auth.json` came back in the
  response (the response is withheld from the log on failure — it is the artifact
  carrying the credential). It requires `FISHHAWK_LIVE_CONFINEMENT=1`, the codex
  binary and host auth, so it is **UNPROVEN at merge**; recording a passing run
  of it is one of the gates on the separate default-flip follow-up.

**Credential copy-back — what it guarantees and what it does not.** The source
hash is snapshotted at copy-in. On cleanup, if the confined copy changed, the
write-back takes an `O_EXCL` lock beside the credential, RE-VERIFIES the source
hash inside the lock, and lands via a same-directory temp file plus `rename`.

- GUARANTEED: an independently newer credential written by another
  LOCK-RESPECTING Fishhawk invocation is never clobbered — it either lands
  before the re-verify (mismatch → the copy-back SKIPS with a warning) or is
  excluded until we finish.
- **NOT guaranteed (named residual):** POSIX offers no compare-and-swap on a
  file. An EXTERNAL writer that does not take this lock — codex-cli refreshing
  the credential itself — can land between the re-verify and the `rename` and be
  overwritten. The window is a hash compare plus a rename, but it is real.
  `TestCredentialCopyBack_ExternalWriterResidual` pins exactly that path as an
  ASSERTION, not a log line: it drives an external write into the window and
  requires the rename to clobber it, so a protocol change that made the write
  survive goes RED and this residual text gets narrowed instead of drifting.
- A lock left behind by a SIGKILLed process wedges the copy-back permanently.
  That fails SAFE: the operator's on-disk bytes are left untouched.

### claude: a BOUNDED credential-root blocklist

`ClaudeDenyRules` emits `Read/Grep/Glob` rules over a FIXED root set — the
operator home and `/etc` universally, plus `/private/etc`, `/var/root`,
`/private/var/root` on darwin and `/root` on linux — in BOTH forms
(`Tool(//P)` and `Tool(//P/**)`; the bare form covers a plain FILE sitting AT a
denied root) and in BOTH the raw and canonical spellings (a deny rule matches the
path as the reviewer ASKS for it, so a rule naming only `/private/etc` does not
cover a read of `/etc/passwd`).

**An arbitrary out-of-tree read is STILL PERMITTED.** Anything outside those
roots — `/opt`, `/srv`, a mounted volume, `${TMPDIR}` — stays readable. That is
asserted, not merely admitted: `TestClaudeDenyRules_OutsideDeniedRootsStillPermitted`
is a CI control that goes red if the docs and the mechanism ever drift apart, and
a live case probes the same property against a real reviewer. Real claude
confinement needs an OS sandbox, which is #611's scope and is NOT in this change.

**Why a fixed list and not ancestor-sibling enumeration.** `${TMPDIR}` on the
operator host holds **111,026 entries**. At the two-rule-form x three-tool shape
that is ~666,000 rules and, at ~60 bytes each, an argv on the order of **40 MB**
against a macOS `ARG_MAX` of ~1 MiB — every grounded claude review would fail to
spawn with `E2BIG` (`execve(2)`). So the rule set is deterministic and small, and
`TestClaudeDenyRules_DeterministicRuleCount` pins the exact counts (30 on darwin,
18 on linux) so a future root addition is a DELIBERATE edit rather than silent
growth back toward that failure.

### Canonicalization is load-bearing everywhere

`os.MkdirTemp` on darwin returns a path under `/var/folders/...` whose
`filepath.EvalSymlinks` real path is `/private/var/folders/...`. An
un-canonicalized codex grant would not cover the child's actual working
directory — **blinding the reviewer silently rather than failing loudly** — and a
raw-vs-canonical mismatch makes a deny rule and a grant disagree about the same
directory. So every load-bearing path (the deny roots, the export for `--add-dir`
and `cmd.Dir`, the codex grants, the overlap guard) goes through
`HostPaths.Canonical`.

### Fail-closed, uniformly

Every named failure mode returns an error and the ADAPTER FAILS the invocation
rather than spawning a grounded reviewer with no bound (the #955 per-invocation
failure path degrades the advisory review gracefully, so failing is strictly
better than silently unconfining): `errDenyRootUnresolvable`,
`errDenyRuleMetacharacter` (applied to EVERY emitted spelling, raw and canonical
— a raw `HOME` can carry whitespace or a parenthesis while canonicalizing clean),
`errDenyOverlapsExport`, `errConfinedHomeOutsideDeniedRoot`,
`errOperatorConfigDeclaresPermissions`, `errCredentialLockUnavailable`. A root
that is merely ABSENT on this host is skipped silently and rules are still
produced for the rest.

### Version pinning and what is UNPROVEN at merge

The `confined` profile was operator-verified on **2026-08-07 against codex-cli
0.144.1** with `--profile confined`. `--ignore-rules` is documented by `codex
exec --help` as *"Do not load user or project execpolicy .rules files"* —
execpolicy layer only, orthogonal to permissions — so it is kept alongside the
profile. The claude rule shape was verified live against **Claude Code 2.1.224**.

The profile mechanism is **undocumented in the shipped npm package**, so it can
change without notice. The exact shipped flag combination is pinned by argv tests,
but **its live denial behaviour is NOT proven at merge** — that is what
`live_confinement_test.go` measures, opt-in via `FISHHAWK_LIVE_CONFINEMENT=1`,
and a recorded operator run of it is the gate on the default-flip follow-up.

Two profile keys the operator flagged and this change does NOT set: `network` and
`workspace_roots`. They are absent from the verified configuration, and inventing
an unverified value could fail the CLI outright; setting `network` explicitly is
worth doing on the follow-up once the harness can measure it.

### Concurrent-export residual (E44)

Two simultaneous grounded reviews each synthesize their OWN confined home under
the same real `CODEX_HOME` and each canonicalize their own export, so their
grants never cross and neither can read the other's tree. What they SHARE is the
operator's `auth.json`: a second review whose credential refresh lands after the
first review's snapshot is SKIPPED with a warning rather than merged, so a
refresh can be silently DROPPED (never clobbered — the operator's newest bytes
win). On a multi-tenant host neither half of this makes grounded review safe. The
default stays FALSE and E44 remains the real answer.

### Host litter

A SIGKILLed daemon can leave a stale 0700 `fishhawk-confined-*` directory inside
the operator's real `CODEX_HOME` holding a copy of their own config and auth. It
inherits owner-only permissions and sits under a claude-denied root, so it is not
an exposure — but it is litter this change does not sweep. Remove by hand.
