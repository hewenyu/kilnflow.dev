import js from '@eslint/js'
import reactHooks from 'eslint-plugin-react-hooks'
import globals from 'globals'
import tseslint from 'typescript-eslint'
import kiln from './eslint/no-hardcoded-ui-text.js'

export default tseslint.config(
  { ignores: ['node_modules', '../internal/server/web/ui/dist'] },
  js.configs.recommended,
  ...tseslint.configs.strictTypeChecked,
  reactHooks.configs.flat.recommended,
  {
    languageOptions: {
      globals: globals.browser,
      parserOptions: { projectService: true, tsconfigRootDir: import.meta.dirname },
    },
    rules: {
      '@typescript-eslint/consistent-type-assertions': ['error', { assertionStyle: 'never' }],
      '@typescript-eslint/restrict-template-expressions': ['error', { allowNumber: true }],
      'no-console': 'error',
      '@typescript-eslint/no-confusing-void-expression': ['error', { ignoreArrowShorthand: true }],
    },
  },
  // 界面文字都来自词典（docs/I18N.md）：只查业务源码，词典、测试和开发用的预览不查
  {
    files: ['src/**/*.{ts,tsx}'],
    ignores: ['src/i18n/**', 'src/test/**', 'src/**/*.test.{ts,tsx}'],
    plugins: { kiln },
    rules: { 'kiln/no-hardcoded-ui-text': 'error' },
  },
  {
    files: ['eslint.config.js', 'eslint/**/*.js'],
    ...tseslint.configs.disableTypeChecked,
    languageOptions: { ...tseslint.configs.disableTypeChecked.languageOptions, globals: globals.node },
  },
)
