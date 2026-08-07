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

Export-not-mount gives the reviewer MORE context and LESS exposure than pointing
it at the live checkout: only **tracked files at that commit** — no `.git`, no
untracked files, no other branches, no operator-personal working-tree state. Both
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
  Enumerated against both CLIs' documented discovery: `AGENTS.md` (codex),
  `CLAUDE.md` and `CLAUDE.local.md` (claude-code), at ANY depth, plus everything
  under a `.claude/` or `.codex/` directory (the CLIs' config/state dirs:
  settings, commands, agents, skills, `config.toml`). This closes an
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

## Closed — operator MCP tools no longer inherited (#2486 fix-up)

Inheriting the operator's MCP TOOLS was formerly a residual and is now CLOSED —
do NOT describe it as an accepted residual. The grounded claude-code adapter's
`--tools Read,Grep,Glob` bounds only the BUILT-IN toolset; MCP tools are not
built-ins, so they loaded from the operator's config and survived the
restriction (verified live — a grounded child enumerated browser/Gmail/GitHub
MCP tools, which are network egress and data exfiltration that defeat the
never-network property). The grounded claude argv now ALSO pins an EMPTY MCP
server set: `--strict-mcp-config` makes `--mcp-config` the sole source of MCP
config (ignoring `~/.claude` and project `.mcp.json`) and the empty
`{"mcpServers":{}}` document loads zero servers; grounding (the tree read) is
preserved with both flags on. The codex adapter adds NO such flags — `codex
exec` reports NONE for MCP tools today — but a grounded codex argv test pins
that no MCP server config reaches the codex child so a future codex-cli change
fails a test rather than silently regaining egress.

## Kill switch

`FISHHAWKD_REVIEW_GROUNDING=false` disables grounding (both adapters revert to
their diff-only posture and the prompts to diff-only wording) without a rollback.
The environment scrub is INDEPENDENT of this flag and is always applied — it has
no plausible reason to roll back.
