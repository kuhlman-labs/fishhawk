/** @vitest-environment node */
// Node environment (not the suite's jsdom default): this file imports Vite's
// node API, whose jsdom build would be wrong.
import { describe, expect, it } from 'vitest';
import { resolveConfig } from 'vite';
import { ANCESTOR_GIT_DENY_RULE, isNestedUnderGitDir, resolveFsDeny } from './vite-fs-deny';

// A constructed path with a `.git` segment. Synthetic + non-existent on
// purpose: `resolveConfig` tolerates a non-existent root, so these cases
// discriminate the nested branch in a NORMAL checkout, not only in-stage.
const SYNTHETIC_NESTED = '/synthetic-repo/.git/fishhawk-worktrees/run-x/frontend';
const SYNTHETIC_NORMAL = '/synthetic-repo/frontend';

/** Vite's LIVE resolved default `server.fs.deny`, fetched — never hardcoded. */
async function liveDefaultDeny(): Promise<string[]> {
  const cfg = await resolveConfig(
    { configFile: false, root: import.meta.dirname, logLevel: 'silent' },
    'serve',
  );
  return cfg.server.fs.deny;
}

describe('isNestedUnderGitDir', () => {
  it('is true for a `.git` path SEGMENT (posix and Windows separators)', () => {
    expect(isNestedUnderGitDir('/repo/.git/fishhawk-worktrees/run-x/frontend')).toBe(true);
    expect(isNestedUnderGitDir('C:\\repo\\.git\\wt\\frontend')).toBe(true);
  });

  it('is false for a plain checkout and for near-misses a substring match would accept', () => {
    expect(isNestedUnderGitDir('/repo/frontend')).toBe(false);
    // `.github` shares the `.git` prefix but is a different segment.
    expect(isNestedUnderGitDir('/repo/.github/workflows')).toBe(false);
    // `my.git` contains `.git` as a substring but is not the `.git` segment.
    expect(isNestedUnderGitDir('/repo/my.git/frontend')).toBe(false);
    // `.gitignore-dir` likewise contains `.git` only as a substring.
    expect(isNestedUnderGitDir('/repo/.gitignore-dir/frontend')).toBe(false);
  });
});

describe('resolveFsDeny', () => {
  // Control case (b): the NORMAL branch. Guards a normal checkout's dev-server
  // posture — Vite keeps its full stock deny list, untouched. Discriminates in
  // every environment (the input is a synthetic normal path).
  it('resolves to undefined for a non-nested path, leaving Vite defaults intact', async () => {
    expect(await resolveFsDeny(SYNTHETIC_NORMAL)).toBeUndefined();
  });

  // Case (c): the NESTED branch, against a SYNTHETIC nested path so it
  // discriminates in a normal checkout too. Anti-drift: compare against Vite's
  // LIVE default list minus exactly one rule, never a hardcoded copy.
  it("drops exactly `**/.git/**` from Vite's live default deny list for a nested path", async () => {
    const live = await liveDefaultDeny();
    // Non-vacuous fixture: the rule we remove must actually be present.
    expect(live).toContain(ANCESTOR_GIT_DENY_RULE);

    const result = await resolveFsDeny(SYNTHETIC_NESTED);
    expect(result).toEqual(live.filter((rule) => rule !== ANCESTOR_GIT_DENY_RULE));
    expect(result).not.toContain(ANCESTOR_GIT_DENY_RULE);
  });
});

describe('vite.config.ts server.fs.deny wiring', () => {
  // WIRING guard (case d): the shipped config must actually CALL resolveFsDeny.
  //
  // NESTED-ENVIRONMENT-ONLY: the shipped config bakes in `import.meta.dirname`,
  // so this test cannot inject a synthetic path. In a NORMAL checkout both the
  // config and the helper produce `undefined`, so a dropped `server.fs` wiring
  // still passes here — this guard does NOT bite in CI's TypeScript lane. It
  // discriminates only when the test itself runs from a `.git`-nested path (the
  // in-stage case). OBSERVED there (#3030 counterfactual iii, wiring reverted):
  // this node-env file still loads and the assertion fires as a GENUINE
  // MISMATCH — `expected undefined to deeply equal [ '.env', … ]` — while the 29
  // jsdom src suites die earlier at a MODULE-LOAD refusal of the denied
  // setupFiles (`Cannot find module '/src/test-setup.ts'`). The synthetic-path
  // discrimination that also bites in a normal checkout lives in the
  // `resolveFsDeny` cases above; this asserts the config is wired to it.
  it('shipped config wires server.fs.deny to resolveFsDeny for its own directory', async () => {
    const mod = await import('./vite.config.ts');
    const exported = mod.default;
    const cfg =
      typeof exported === 'function'
        ? await (exported as (env: { command: string; mode: string }) => Promise<unknown>)({
            command: 'serve',
            mode: 'test',
          })
        : exported;
    const deny = (cfg as { server?: { fs?: { deny?: string[] } } }).server?.fs?.deny;
    expect(deny).toEqual(await resolveFsDeny(import.meta.dirname));
  });
});
