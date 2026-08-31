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
      empty-MCP set (pinned in BOTH postures, #2524) + absence of
      Bash/WebFetch/WebSearch from `--tools` — cuts
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
claude-code adapter's `--tools Read,Grep,Glob` bounds only the BUILT-IN toolset;
MCP tools are not built-ins, so they loaded from the operator's config and
survived the restriction (verified live — a grounded child enumerated
browser/Gmail/GitHub MCP tools, which are network egress and data exfiltration
that defeat the never-network property). The claude argv pins an EMPTY MCP server
set: `--strict-mcp-config` makes `--mcp-config` the sole source of MCP config
(ignoring `~/.claude` and project `.mcp.json`) and the empty `{"mcpServers":{}}`
document loads zero servers.

The pin applies in BOTH postures (#2524, refining the initial #2486 fix-up),
not just the grounded argv. Originally the two flags were appended only inside
the grounded (`treeDir != ""`) block, so the UNGROUNDED (diff-only) posture — the
SHIPPING posture, since grounding ships dormant behind `FISHHAWKD_REVIEW_GROUNDING`
(#2522) — omitted them and the child still loaded the operator's MCP servers.

Keep the claim inside the evidence: this is defence-in-depth, not the closure of
a live egress channel. In ungrounded print mode with no `--allowed-tools`, MCP
tool INVOCATION was already permission-DENIED (measured on #2524:
`mcp__MCP_DOCKER__get_me` and `WebFetch` both recorded in `permission_denials`);
only ENUMERATION leaked. The reason to pin unconditionally is that today's safety
then rests on the CLI's default-deny permission behavior REMAINING the default —
a CLI change that auto-approves MCP tools, or a deployment that pre-approves them
in settings, would turn a listing into an egress channel with no change on our
side. The unconditional pin removes that dependence and additionally means the
operator-configured MCP tools are no longer DISCLOSED to the child through CLI
enumeration (built-in tools remain enumerated, and an attacker can still name MCP
tools in untrusted text regardless — the effect is non-disclosure of the operator
tool list, not an impossibility of referencing tools).

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
revisited before multi-tenancy (E44). The default flips to on once #2522 lands
confinement; the `serve_test.go` "unset: grounding OFF" subtest pins the current
default so it cannot be undone silently.

The environment scrub is INDEPENDENT of this flag and is ALWAYS applied — it has
no plausible reason to roll back, and it stands on its own merits.
