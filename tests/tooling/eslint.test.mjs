import assert from 'node:assert/strict'
import test from 'node:test'
import { ESLint } from 'eslint'

const workspaceAPI = 'web/src/api/fixture.ts'

async function lint(text, filePath = workspaceAPI, fix = false) {
  const eslint = new ESLint({ fix })
  const [result] = await eslint.lintText(text, { filePath })
  return result
}

async function contractMessages(text, filePath) {
  const result = await lint(text, filePath)
  assert.equal(result.fatalErrorCount, 0)
  return result.messages
    .filter(({ ruleId }) => ruleId === 'muster/public-contract')
    .map(({ messageId }) => messageId)
}

test('公开说明覆盖直接导出和本地导出列表，跨模块导出沿用来源契约', async () => {
  const input = [
    'export type Response = { id: string }',
    'const limit = 42',
    'export { limit }',
    'export { fromOther } from "./other"',
    'import { fromDependency } from "./dependency"',
    'export { fromDependency }',
  ].join('\n')

  assert.deepEqual(await contractMessages(input), ['description', 'description'])
  assert.deepEqual(await contractMessages(input, 'web/src/api/fixture.test.ts'), [])
  assert.deepEqual(await contractMessages(input, 'web/src/features/fixture.ts'), [])
})

test('说明必须紧贴声明并包含正文', async () => {
  const input = [
    '/** 与另一段内容相隔。 */',
    '',
    'export const limit = 42',
    '/**',
    ' * @returns 当前标识。',
    ' */',
    'export function identify() { return "id" }',
    'const local = 1 /** 局部值的行尾说明。 */',
    'export const exported = Number(local)',
  ].join('\r\n')

  assert.deepEqual(await contractMessages(input), ['description', 'description', 'description'])
})

test('核对签名的参数名称、顺序和说明', async () => {
  const input = [
    '/**',
    ' * 配置执行器。',
    ' * @param signal 用于取消。',
    ' * @param name',
    ' */',
    'export function configure(name: string, signal: AbortSignal): void { void [name, signal] }',
  ].join('\n')

  assert.deepEqual(await contractMessages(input), ['params'])
})

test('检查接口与类方法的异步完成语义，忽略私有方法及普通回调', async () => {
  const input = [
    '/** 设备的配置请求入口。 */',
    'export interface Device {',
    '  /** 写入设备配置。 */',
    '  configure(): Promise<void>',
    '}',
    '/** 设备客户端。 */',
    'export class Client {',
    '  private cache() { return false }',
    '  /** 刷新设备状态。 */',
    '  async refresh(): Promise<void> {}',
    '}',
    '/** 更新请求。 */',
    'export function start() { [1].map((item) => item + 1) }',
  ].join('\n')

  assert.deepEqual(await contractMessages(input), ['returns', 'returns'])
})

test('同步空返回不添加返回标签，普通值需要说明结果', async () => {
  const input = [
    '/**',
    ' * 取消当前请求。',
    ' * @returns 完成。',
    ' */',
    'export function cancel(): void {}',
    '/** 读取状态。 */',
    'export const status = () => 200',
  ].join('\n')

  assert.deepEqual(await contractMessages(input), ['extraReturns', 'returns'])
})

test('围栏内的示例保留为示例，真实标签可写可选参数和 rest 参数', async () => {
  const input = [
    '/**',
    ' * 批量同步设备设置。',
    ' * @param [name] 设备的可选显示名称。',
    ' * @param ...flags 控制写入的选项。',
    ' * @returns 同步完成后兑现。',
    ' * @example',
    ' * ```js',
    ' * @param example',
    ' * @returns example',
    ' * ```',
    ' */',
    'export async function sync(name?: string, ...flags: string[]): Promise<void> { void [name, flags] }',
  ].join('\n')

  const result = await lint(input)
  assert.deepEqual(result.messages, [])
})

test('lint 修复保留模板内容和 JSX 文本，只修剪非内容空白', async () => {
  const input = 'const text = `原始  \n文本`  \nconst element = <span>原始  \n文本</span>  \n'
  const expected = 'const text = `原始  \n文本`\nconst element = <span>原始  \n文本</span>\n'

  const result = await lint(input, 'web/src/fixture.tsx', true)
  assert.equal(result.output, expected)
  assert.equal(result.errorCount, 0)

  const missing = await lint('export type Response = { id: string }\n', workspaceAPI, true)
  assert.equal(missing.output, undefined)
  assert.deepEqual(missing.messages.map(({ messageId }) => messageId), ['description'])
})

test('React 检查 Hook 条件调用和缺失依赖', async () => {
  const result = await lint([
    'import { useEffect, useState } from "react"',
    'export function Panel({ count }: { count: number }) {',
    '  if (count > 0) useState(0)',
    '  useEffect(() => { void count }, [])',
    '  return <span />',
    '}',
  ].join('\n'), 'web/src/fixture.tsx')

  assert.deepEqual(result.messages.map(({ ruleId }) => ruleId), [
    'react-hooks/rules-of-hooks',
    'react-hooks/exhaustive-deps',
  ])
})
