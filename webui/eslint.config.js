import js from '@eslint/js';
import globals from 'globals';
import reactHooks from 'eslint-plugin-react-hooks';
import reactRefresh from 'eslint-plugin-react-refresh';
import tseslint from 'typescript-eslint';

/**
 * The one file allowed to know that @tanstack/charts exists.
 *
 * TanStack Charts is pre-alpha and has historically broken through import
 * specifiers, option renames and callback signatures rather than through
 * component props - so a props-only wrapper is not a sufficient seam. The
 * seam owns every import specifier, the renderer choice, our point type and
 * our accessor convention; the rule below is what keeps it that way.
 */
const CHART_SEAM = 'src/charts/seam.tsx';

const RESTRICTED_IMPORTS = [
  'error',
  {
    patterns: [
      {
        group: ['@tanstack/charts', '@tanstack/charts/*', '@tanstack/charts/**'],
        message:
          'Import charts through src/charts/seam.tsx. @tanstack/charts is pre-alpha; only the seam may name it.',
      },
    ],
  },
];

export default tseslint.config(
  {
    ignores: ['dist/**', 'node_modules/**', '../internal/web/dist/**'],
  },
  {
    files: ['**/*.js'],
    extends: [js.configs.recommended],
    languageOptions: {
      ecmaVersion: 2022,
      sourceType: 'module',
      globals: globals.node,
    },
  },
  {
    files: ['**/*.{ts,tsx}'],
    extends: [js.configs.recommended, ...tseslint.configs.recommended],
    languageOptions: {
      ecmaVersion: 2022,
      sourceType: 'module',
      globals: globals.browser,
    },
    plugins: {
      'react-hooks': reactHooks,
      'react-refresh': reactRefresh,
    },
    rules: {
      ...reactHooks.configs['recommended-latest'].rules,
      'react-refresh/only-export-components': ['warn', { allowConstantExport: true }],
      // The typescript-eslint variant also sees `import type`, which the core
      // rule would let through the seam boundary.
      'no-restricted-imports': 'off',
      '@typescript-eslint/no-restricted-imports': RESTRICTED_IMPORTS,
    },
  },
  {
    files: [CHART_SEAM],
    rules: {
      '@typescript-eslint/no-restricted-imports': 'off',
    },
  },
);
