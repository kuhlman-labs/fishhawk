import js from '@eslint/js';
import globals from 'globals';
import reactHooks from 'eslint-plugin-react-hooks';
import reactRefresh from 'eslint-plugin-react-refresh';
import tseslint from 'typescript-eslint';

export default tseslint.config(
  {
    ignores: ['dist', 'coverage', 'node_modules'],
  },
  {
    extends: [js.configs.recommended, ...tseslint.configs.recommended],
    files: ['**/*.{ts,tsx}'],
    languageOptions: {
      ecmaVersion: 2022,
      globals: globals.browser,
    },
    plugins: {
      'react-hooks': reactHooks,
      'react-refresh': reactRefresh,
    },
    rules: {
      // Enumerated, NOT `...reactHooks.configs.recommended.rules`. That preset
      // changes meaning across plugin majors: in eslint-plugin-react-hooks 5.x
      // it is exactly these two rules, while 7.x builds it as
      // `{...basicRuleConfigs, ...recommendedCompilerRuleConfigs}` — the same
      // two PLUS the whole React Compiler family (set-state-in-effect,
      // set-state-in-render, purity, static-components, …). Spreading the
      // preset therefore lets a version bump widen the enforced rule set as a
      // side effect, which is how the 5.2.0 → 7.1.1 bump (#2191) red-lined CI
      // on six pre-existing violations in files it never touched. Listing the
      // rules keeps "upgrade the linter" and "adopt a new rule family" as
      // separate, reviewable decisions. Adopting the compiler rules is tracked
      // by #2200 — do that there, not by restoring the spread.
      'react-hooks/rules-of-hooks': 'error',
      'react-hooks/exhaustive-deps': 'warn',
      'react-refresh/only-export-components': ['warn', { allowConstantExport: true }],
      // We import types alongside values; verbatimModuleSyntax in tsconfig
      // already enforces `import type` where needed at the type-check layer.
      '@typescript-eslint/consistent-type-imports': 'error',
    },
  },
);
