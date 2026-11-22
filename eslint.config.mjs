import tsParser from '@typescript-eslint/parser'
import reactHooks from 'eslint-plugin-react-hooks'
import publicContract from './scripts/eslint-rules/public-contract.mjs'

const commentLineBreak = /\r\n|[\n\r\u2028\u2029]/u
const trackingReference = /https?:\/\/\S+|(?:^|[\s(])(?:#\d+|[A-Z][A-Z0-9]+-\d+)\b/u
const todo = /^(?:TODO|FIXME)\b/u
const englishProse = /^[A-Z][a-z]+(?: [a-z]{2,}){2}\b/u
const duplicateType = /^@(?:(?:type|typedef)\b|(?:param|property|returns?)\s+\{)/u

const muster = {
  rules: {
    'public-contract': publicContract,
    'comment-contract': {
      meta: {
        type: 'problem',
        schema: [],
        messages: {
          todo: 'TODO 或 FIXME 需要关联可追踪的事项编号或链接。',
          language: 'JSDoc 的说明正文默认使用中文，技术术语可以保留原文。',
          type: 'TypeScript 的类型由声明定义，JSDoc 用于说明业务语义。',
        },
      },
      create(context) {
        return {
          Program() {
            for (const comment of context.sourceCode.getAllComments()) {
              const jsdoc = comment.type === 'Block' && comment.value.startsWith('*')
              const isTypeScript = /\.(?:[cm]?ts|tsx)$/iu.test(context.filename)
              const lines = comment.value.split(commentLineBreak)
              let inFence = false
              let inExample = false
              for (const [offset, content] of lines.entries()) {
                const text = content.trim().replace(/^\*\s*/u, '')
                if (text.startsWith('\x60\x60\x60')) {
                  inFence = !inFence
                  continue
                }
                if (text.startsWith('@')) inExample = /^@example\b/u.test(text)
                if (inFence || inExample) continue

                let messageId
                if (todo.test(text) && !lines.slice(offset).join(' ').match(trackingReference)) {
                  messageId = 'todo'
                } else if (jsdoc && isTypeScript && duplicateType.test(text)) {
                  messageId = 'type'
                } else if (jsdoc && englishProse.test(text) && !/\p{Script=Han}/u.test(text)) {
                  messageId = 'language'
                }
                if (messageId) {
                  const line = comment.loc.start.line + offset
                  const column = offset === 0 ? comment.loc.start.column : 0
                  context.report({ loc: { start: { line, column }, end: { line, column } }, messageId })
                }
              }
            }
          },
        }
      },
    },
    'trailing-whitespace': {
      meta: {
        type: 'layout',
        fixable: 'whitespace',
        schema: [],
        messages: { trailing: '请删除行尾空白。' },
      },
      create(context) {
        const { sourceCode } = context
        return {
          Program() {
            for (const [offset, text] of sourceCode.lines.entries()) {
              const match = /[ \t]+$/u.exec(text)
              if (!match) continue
              const line = offset + 1
              const start = sourceCode.getIndexFromLoc({ line, column: match.index })
              const end = start + match[0].length
              // 多行字面量与 JSX 文本的空格可能进入运行时，保留原值。
              const token = sourceCode.getNodeByRangeIndex(start)
              if (token && ['Literal', 'TemplateElement', 'JSXText'].includes(token.type)) continue
              context.report({
                loc: { start: { line, column: match.index }, end: { line, column: text.length } },
                messageId: 'trailing',
                fix: (fixer) => fixer.removeRange([start, end]),
              })
            }
          },
        }
      },
    },
  },
}

export default [
  {
    ignores: [
      '**/node_modules/**',
      '**/dist/**',
      'cmd/muster-daemon/frontend/src/bindings/**',
      '**/.tmp/**',
      '**/.upstream/**',
      '**/.worktree/**',
      '**/.codegraph/**',
    ],
  },
  {
    files: ['**/*.{js,jsx,cjs,mjs,ts,tsx,cts,mts}'],
    plugins: { muster },
    languageOptions: { parserOptions: { ecmaFeatures: { jsx: true } } },
    rules: {
      'muster/comment-contract': 'error',
      'muster/trailing-whitespace': 'error',
      'no-debugger': 'error',
      'no-dupe-keys': 'error',
      'no-duplicate-imports': ['error', { allowSeparateTypeImports: true }],
      'no-unsafe-finally': 'error',
      'no-unreachable': 'error',
      'eqeqeq': ['error', 'always', { null: 'ignore' }],
      'no-async-promise-executor': 'error',
      'no-case-declarations': 'error',
      'no-prototype-builtins': 'error',
    },
  },
  {
    files: ['**/*.{ts,tsx,cts,mts}'],
    languageOptions: { parser: tsParser },
  },
  {
    files: ['web/src/**/*.{ts,tsx}', 'cmd/muster-daemon/frontend/src/**/*.{ts,tsx}'],
    plugins: { 'react-hooks': reactHooks },
    rules: {
      'react-hooks/rules-of-hooks': 'error',
      'react-hooks/exhaustive-deps': 'error',
    },
  },
  {
    files: [
      'web/src/api/**/*.ts',
      'web/src/useWorkspace.ts',
      'cmd/muster-daemon/frontend/src/types.ts',
      'plugins/muster-dsh/src/**/*.ts',
    ],
    ignores: ['**/*.{test,spec}.ts', '**/{tests,__tests__}/**'],
    rules: { 'muster/public-contract': 'error' },
  },
]
