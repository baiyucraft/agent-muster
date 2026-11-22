import { readdirSync, readFileSync, realpathSync, statSync } from 'node:fs'
import { dirname, extname, isAbsolute, relative, resolve, sep } from 'node:path'
import { pathToFileURL } from 'node:url'
import GithubSlugger from 'github-slugger'
import remarkGfm from 'remark-gfm'
import remarkParse from 'remark-parse'
import { unified } from 'unified'
import { parseDocument } from 'yaml'

const wikiFields = ['title', 'description', 'updated', 'owner']
const skillFields = ['name', 'description']
const frontmatter = /^\uFEFF?---[ \t]*\r?\n([\s\S]*?)^---[ \t]*(?:\r?\n|$)/mu
const markdown = unified().use(remarkParse).use(remarkGfm)

function pagesUnder(directory) {
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = resolve(directory, entry.name)
    if (entry.isDirectory()) return pagesUnder(path)
    return entry.isFile() && isMarkdown(path) ? [path] : []
  }).sort()
}

function isMarkdown(path) {
  return extname(path).toLowerCase() === '.md'
}

function countLines(source) {
  if (source.length === 0) return 0
  const lines = source.split('\n')
  return lines.length - Number(lines.at(-1) === '')
}

function readDocument(path) {
  const content = readFileSync(path, 'utf8')
  const match = frontmatter.exec(content)
  const header = match?.index === 0 ? match : null
  const metadata = header ? parseDocument(header[1]) : null
  const body = header ? content.slice(header[0].length) : content
  const firstLine = header ? countLines(header[0]) : 0
  const anchors = new Set()
  const definitions = new Map()
  const links = []
  const slugger = new GithubSlugger()

  function visit(node) {
    switch (node.type) {
      case 'heading':
        anchors.add(slugger.slug(headingText(node)))
        break
      case 'html': {
        const html = node.value.replace(/<!--[\s\S]*?-->/gu, '')
        for (const anchor of html.matchAll(/<a\b[^>]*?\s(?:id|name)\s*=\s*["']([^"']+)["'][^>]*>/giu)) {
          anchors.add(anchor[1])
        }
        break
      }
      case 'definition':
        // Markdown 使用同名引用定义的首次出现。
        if (!definitions.has(node.identifier)) definitions.set(node.identifier, node.url)
        break
      case 'link':
      case 'image':
      case 'linkReference':
      case 'imageReference':
        links.push(node)
        break
    }
    for (const child of node.children ?? []) visit(child)
  }

  visit(markdown.parse(body))
  return {
    metadata,
    lines: countLines(body),
    anchors,
    links: links.map((link) => ({
      url: link.url ?? definitions.get(link.identifier),
      line: firstLine + link.position.start.line,
      navigation: link.type === 'link' || link.type === 'linkReference',
    })),
  }
}

function headingText(node) {
  if (node.type === 'html') return ''
  if (typeof node.value === 'string') return node.value
  if (typeof node.alt === 'string') return node.alt
  return (node.children ?? []).map(headingText).join('')
}

function validateFields(metadata, required) {
  if (!metadata) return '缺少 YAML 前置元数据'
  if (metadata.errors.length) return `YAML 前置元数据无效：${metadata.errors[0].message}`
  const fields = metadata.toJS()
  const missing = required.filter((field) => typeof fields?.[field] !== 'string' || !fields[field].trim())
  return missing.length ? `前置元数据缺少非空字段 ${missing.join('、')}` : null
}

function within(root, path) {
  const fromRoot = relative(root, path)
  return fromRoot !== '..' && !fromRoot.startsWith(`..${sep}`) && !isAbsolute(fromRoot)
}

function stat(path) {
  try {
    return statSync(path, { throwIfNoEntry: false })
  } catch (error) {
    if (error.code === 'ENOTDIR') return undefined
    throw error
  }
}

function resolveLink(root, from, url) {
  if (/^(?:[a-z]:|file:)/iu.test(url)) return { error: '链接必须使用仓库相对路径' }
  if (/^(?:[a-z][a-z\d+.-]*:|\/\/)/iu.test(url)) return {}

  const hash = url.indexOf('#')
  const target = hash < 0 ? url : url.slice(0, hash)
  let path
  let fragment
  try {
    const pathname = decodeURIComponent(target.split('?')[0])
    fragment = hash < 0 ? '' : decodeURIComponent(url.slice(hash + 1))
    if (/^(?:[a-z]:|[/\\])/iu.test(pathname)) return { error: '链接必须使用仓库相对路径' }
    if (pathname.includes('\0')) return { error: '链接 URL 编码无效' }
    path = pathname ? resolve(dirname(from), pathname) : from
  } catch {
    return { error: '链接 URL 编码无效' }
  }

  if (!within(root, path)) return { error: '链接目标在仓库之外' }
  if (stat(path)?.isDirectory()) path = resolve(path, 'INDEX.md')
  if (!stat(path)?.isFile()) return { error: '链接目标不存在' }
  const actual = realpathSync.native(path)
  if (!within(root, actual)) return { error: '链接目标在仓库之外' }
  if (actual !== path) return { error: '链接必须使用目标的实际仓库路径' }
  return { path, fragment }
}

/**
 * 只读核验 Wiki 和仓库 Skill。Markdown 链接的锚点按 GitHub 标题规则解析。
 *
 * @param projectRoot 仓库目录，可用独立目录测试。
 * @returns 有路径和行号的诊断，以及已扫描的 Wiki 和 Skill 页面数量。
 */
export function checkDocs(projectRoot) {
  const root = realpathSync.native(projectRoot)
  const wiki = resolve(root, '.wiki')
  const index = resolve(wiki, 'INDEX.md')
  const skills = resolve(root, '.agents/skills')
  const errors = []
  const warnings = []
  const wikiPages = stat(wiki)?.isDirectory() ? pagesUnder(wiki) : []
  const skillPages = stat(skills)?.isDirectory() ? pagesUnder(skills) : []
  const indexedPages = new Set(wikiPages)
  const directories = new Set([wiki])
  const documents = new Map()
  const edges = new Map()
  const display = (path) => relative(root, path).split(sep).join('/')

  function document(path) {
    if (!documents.has(path)) documents.set(path, readDocument(path))
    return documents.get(path)
  }

  for (const page of [...wikiPages, ...skillPages]) {
    const { metadata, lines, links } = document(page)
    const file = display(page)
    if (indexedPages.has(page)) {
      const missing = validateFields(metadata, wikiFields)
      if (missing) errors.push(`${file}:1 ${missing}`)
      if (lines >= 1000) errors.push(`${file} 正文 ${lines} 行，达到 1000 行错误阈值`)
      else if (lines >= 800) warnings.push(`${file} 正文 ${lines} 行，请评估是否按主题拆分`)
      const depth = relative(wiki, dirname(page)).split(sep).filter(Boolean).length
      if (depth > 4) errors.push(`${file} 目录深度 ${depth}，最多允许四级`)
      for (let parent = dirname(page); parent !== wiki; parent = dirname(parent)) directories.add(parent)
    } else if (page.endsWith(`${sep}SKILL.md`)) {
      const missing = validateFields(metadata, skillFields)
      if (missing) errors.push(`${file}:1 ${missing}`)
    }

    const targets = []
    for (const { url, line, navigation } of links) {
      if (url === undefined) continue
      const { path, fragment, error } = resolveLink(root, page, url)
      const location = `${file}:${line}`
      if (error) {
        errors.push(`${location} ${error} ${url}`)
      } else if (path) {
        if (fragment && isMarkdown(path) && !document(path).anchors.has(fragment)) {
          errors.push(`${location} Markdown 锚点不存在 ${url}`)
        } else if (navigation && indexedPages.has(path)) {
          targets.push(path)
        }
      }
    }
    edges.set(page, targets)
  }

  for (const directory of directories) {
    const path = resolve(directory, 'INDEX.md')
    if (!indexedPages.has(path)) errors.push(`${display(path)} 缺少目录索引`)
  }
  if (indexedPages.has(index)) {
    const reachable = new Set([index])
    for (const page of reachable) {
      for (const target of edges.get(page)) reachable.add(target)
    }
    for (const page of wikiPages) {
      if (!reachable.has(page)) errors.push(`${display(page)} 无法从 .wiki/INDEX.md 到达`)
    }
  }
  return { errors, warnings, wikiPages: wikiPages.length, skillPages: skillPages.length }
}

/**
 * 输出检查结果。只有错误会使命令以非零状态退出。
 *
 * @param projectRoot 默认检查本脚本所在仓库。
 * @returns 进程退出码。
 */
export function main(projectRoot = resolve(import.meta.dirname, '..')) {
  const result = checkDocs(projectRoot)
  for (const warning of result.warnings) console.warn(`docs-check: ${warning}`)
  for (const error of result.errors) console.error(`docs-check: ${error}`)
  if (result.errors.length > 0) return 1
  console.log(`docs-check: ${result.wikiPages} 个 Wiki 页面、${result.skillPages} 个 Skill 页面通过检查`)
  return 0
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  process.exitCode = main()
}
