/** @vitest-environment node */
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import tseslint from 'typescript-eslint';
import { expect, it } from 'vitest';

// Exercise the parser's minimatch consumer: ordinary linting does not enable
// projectService, so it would miss a broken named export or changed matching.
it('preserves project-service allowlist matching with the minimatch override', () => {
  const root = mkdtempSync(join(tmpdir(), 'fishhawk-parser-globs-'));
  try {
    writeFileSync(join(root, 'tsconfig.json'), JSON.stringify({ compilerOptions: {} }));
    const parse = (filename: string) =>
      tseslint.parser.parseForESLint('const value: number = 1;', {
        filePath: join(root, filename),
        tsconfigRootDir: root,
        projectService: { allowDefaultProject: ['*.ts'] },
      });

    for (const filename of ['allowed.ts', '.hidden.ts']) {
      const result = parse(filename);
      expect(result.ast.body).toHaveLength(1);
      expect(result.services.program).toBeDefined();
    }
    expect(() => parse('excluded.tsx')).toThrow(/not found by the project service/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
