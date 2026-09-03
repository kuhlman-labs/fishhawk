// Vite's dev server carries a default `server.fs.deny` list. Its Vite-8
// default is:
//   ['.env', '.env.*', '*.{crt,pem,key,p12,pfx,cer,der}',
//    '.npmrc', '.yarnrc.yml', '**/.git/**']
// The last rule, `**/.git/**`, is fatal for a checkout that itself lives
// UNDER a `.git/` path segment — e.g. a Fishhawk run worktree at
// `<repo>/.git/fishhawk-worktrees/run-<id>/frontend`. Because the served
// root (`server.fs.allow[0]`) is that frontend directory and its absolute
// path contains a `.git` segment, `**/.git/**` matches EVERY file under the
// allowed root, so every module load is refused.
//
// The refusal surfaces as a MISLEADING message. In this toolchain
// (vite 8.2.2 / vitest 4.1.11) the vitest `setupFiles` refusal renders as
// `Error: Cannot find module '/src/test-setup.ts'` and component imports
// render as `Failed to load url … Does the file exist?`. The file exists and
// the resolved config root is the FULL nested path (confirmed via
// `resolveConfig` — the `.git` segment is NOT stripped); the id is denied,
// not missing. That is why the originating issue's proposed absolute-path
// `setupFiles` fix does not work: the path was never the problem.
//
// The fix is to drop exactly the `**/.git/**` rule, and ONLY when the config
// directory is itself nested under a `.git/` path segment. In a normal
// checkout `resolveFsDeny` returns `undefined`, so Vite applies its own
// resolved defaults verbatim and the dev server's posture is unchanged.
//
// SECURITY RESIDUAL (deliberate, narrow): this narrows a default-deny rule.
// The reachable surface is dev-server file reads within the allowed root. A
// git worktree carries a `.git` FILE, not a `.git` directory, so there is no
// worktree-local repository metadata this rule was protecting. But the rule
// ALSO denied any DESCENDANT `.git` directory inside the allowed root — a
// nested repository, a vendored checkout, or one a developer clones under the
// tree later — and dropping it removes that protection too within a
// `.git`-nested checkout. `server.fs.allow` still bounds reads to the served
// root, but a nested `.git` directory created later would no longer be denied
// by THIS rule. The carve-out is gated as tightly as possible: it fires only
// when the checkout root itself sits under a `.git` path segment, and never
// in a normal checkout.
//
// The remaining rules are NOT hardcoded: they are read from Vite's own
// resolved defaults at config time via `resolveConfig({configFile:false})`,
// so a future Vite upgrade that widens the list (v6 → v8 already added
// `.npmrc`, `.yarnrc.yml` and widened the cert glob) is inherited
// automatically, and a future RENAME of the git rule makes the nested case
// fail loudly rather than silently lose protection.
//
// See frontend/README.md ("Running from a run worktree") and #3030.

import { resolveConfig } from 'vite';

/** The single Vite default `server.fs.deny` rule this module removes. */
export const ANCESTOR_GIT_DENY_RULE = '**/.git/**';

/**
 * True when `dir` has a `.git` path SEGMENT (never a substring match). Matches
 * both POSIX (`/`) and Windows (`\`) separators. `.github`, `my.git` and
 * `.gitignore-dir` are NOT segments and do not trigger it.
 */
export function isNestedUnderGitDir(dir: string): boolean {
  return dir.split(/[\\/]+/).includes('.git');
}

/**
 * Resolve the `server.fs.deny` list for a config directory.
 *
 * Returns `undefined` when `configDir` is not nested under a `.git/` segment,
 * so Vite's own resolved defaults apply verbatim and nothing is hardcoded.
 * When nested, returns Vite's live resolved default deny list with exactly
 * `ANCESTOR_GIT_DENY_RULE` removed.
 */
export async function resolveFsDeny(configDir: string): Promise<string[] | undefined> {
  if (!isNestedUnderGitDir(configDir)) {
    return undefined;
  }
  const resolved = await resolveConfig(
    { configFile: false, root: configDir, logLevel: 'silent' },
    'serve',
  );
  return resolved.server.fs.deny.filter((rule) => rule !== ANCESTOR_GIT_DENY_RULE);
}
