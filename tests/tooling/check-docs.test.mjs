import assert from 'node:assert/strict'
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { test } from 'node:test'
import { checkDocs } from '../../scripts/check-docs.mjs'

const header = '---\ntitle: 文档\ndescription: 测试页面\nupdated: 2026-11-22\nowner: project\n---\n'

function workspace(t) {
  const root = mkdtempSync(join(tmpdir(), 'muster-docs-'))
  t.after(() => rmSync(root, { recursive: true, force: true }))
  return {
    root,
    write(path, source) {
      const file = join(root, path)
      mkdirSync(dirname(file), { recursive: true })
      writeFileSync(file, source, 'utf8')
    },
  }
}

test('解析 Wiki、Skill 和设计稿之间的链接及 GitHub 锚点', (t) => {
  const repository = workspace(t)
  const index = header + [
    '# Wiki',
    '',
    '| 入口 |',
    '| --- |',
    '| [开发章][page] |',
    '',
    '[设计](../.docs/设计.md#手动-id)',
    '[Skill](../.agents/skills/sample/SKILL.md)',
    '[站外](https://example.invalid/broken#fragment)',
    '[本页](#wiki)',
    '',
    '```markdown',
    '[演示](missing.md)',
    '## 非标题',
    '```',
    '',
    '<!-- [注释](missing.md) -->',
    '',
    '[page]: <开发 指南/INDEX.md#开发-指南-1>',
  ].join('\r\n')
  repository.write('.wiki/INDEX.md', index)
  repository.write('.wiki/开发 指南/INDEX.md', header + [
    '## 开发 **指南**',
    '## 开发 **指南**',
    '[子页](测试.md#%E6%AD%A3%E6%96%87)',
    '![样例](图片.png)',
  ].join('\n'))
  repository.write('.wiki/开发 指南/测试.md', header + '# 正文\n')
  repository.write('.wiki/开发 指南/图片.png', '样例')
  repository.write('.docs/设计.md', '<a id="手动-id"></a>\n\n# 设计\n')
  repository.write('.agents/skills/sample/SKILL.md', '---\nname: sample\ndescription: 样例\n---\n# Skill\n\n[范围](references/范围.md#范围)\n')
  repository.write('.agents/skills/sample/references/范围.md', '# 范围\n\n[Wiki](../../../../.wiki/INDEX.md)\n')

  const result = checkDocs(repository.root)
  assert.deepEqual(result, { errors: [], warnings: [], wikiPages: 3, skillPages: 2 })
  assert.equal(readFileSync(join(repository.root, '.wiki/INDEX.md'), 'utf8'), index)
})

test('报告缺失索引、不可达页面、无效元数据和失效链接的来源', (t) => {
  const repository = workspace(t)
  repository.write('.wiki/INDEX.md', header + [
    '# Wiki',
    '',
    '[缺失](missing.md)',
    '[锚点](可达.md#不存在)',
    '[非法编码](%FF.md)',
    '[仓库外](../../outside.md)',
    '[ Skill ](../.agents/skills/sample/SKILL.md)',
    '[普通文件不能用作目录](可达.md/page.md)',
    '[绝对路径](%2Fx.md)',
    '[NUL](%00.md)',
    '[注释中定义的锚点](可达.md#隐藏)',
  ].join('\n'))
  repository.write('.wiki/可达.md', header + '# 现有锚点\n\n<!-- <a id="隐藏"></a> -->')
  repository.write('.wiki/a/b/c/d/e/page.md', '---\ntitle: 重复\ntitle: 字段\n---\n# Page')
  repository.write('.wiki/blank.md', header.replace('owner: project', 'owner: " "'))
  repository.write('.agents/skills/sample/SKILL.md', '---\nname: sample\ndescription: 样例\n---\n[失效](references/missing.md)\n')

  const { errors } = checkDocs(repository.root)
  const output = errors.join('\n')
  assert.match(output, /\.wiki\/INDEX\.md:9 链接目标不存在 missing\.md/u)
  assert.match(output, /\.wiki\/INDEX\.md:10 Markdown 锚点不存在/u)
  assert.match(output, /\.wiki\/INDEX\.md:11 链接 URL 编码无效/u)
  assert.match(output, /\.wiki\/INDEX\.md:12 链接目标在仓库之外/u)
  assert.match(output, /\.wiki\/INDEX\.md:14 链接目标不存在/u)
  assert.match(output, /\.wiki\/INDEX\.md:15 链接必须使用仓库相对路径/u)
  assert.match(output, /\.wiki\/INDEX\.md:16 链接 URL 编码无效/u)
  assert.match(output, /\.wiki\/INDEX\.md:17 Markdown 锚点不存在/u)
  assert.match(output, /\.wiki\/blank\.md:1 前置元数据缺少非空字段 owner/u)
  assert.match(output, /\.wiki\/a\/b\/c\/d\/e\/page\.md:1 YAML 前置元数据无效/u)
  assert.match(output, /\.wiki\/a\/b\/c\/d\/e\/page\.md 目录深度 5/u)
  assert.match(output, /\.wiki\/a\/b\/c\/d\/INDEX\.md 缺少目录索引/u)
  assert.match(output, /\.wiki\/可达\.md 无法从 \.wiki\/INDEX\.md 到达/u)
  assert.match(output, /\.agents\/skills\/sample\/SKILL\.md:5 链接目标不存在/u)
})

test('按正文行数处理 799、800、999 和 1000 的边界', (t) => {
  const repository = workspace(t)
  const sizes = [799, 800, 999, 1000]
  repository.write('.wiki/INDEX.md', header + sizes.map((size) => `[${size}](${size}.md)`).join('\n'))
  for (const size of sizes) {
    const endOfLine = size % 2 === 0 ? '\r\n' : '\n'
    const body = '# 规模\n' + '内容\n'.repeat(size - 1)
    repository.write(`.wiki/${size}.md`, (header + body).replaceAll('\n', endOfLine))
  }
  const { errors, warnings } = checkDocs(repository.root)
  assert.deepEqual(errors, ['.wiki/1000.md 正文 1000 行，达到 1000 行错误阈值'])
  assert.deepEqual(warnings, [
    '.wiki/800.md 正文 800 行，请评估是否按主题拆分',
    '.wiki/999.md 正文 999 行，请评估是否按主题拆分',
  ])
})
