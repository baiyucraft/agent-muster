import { parse, tokenizers } from 'comment-parser'

const functions = new Set([
  'ArrowFunctionExpression', 'FunctionDeclaration', 'FunctionExpression',
  'TSDeclareFunction', 'TSEmptyBodyFunctionExpression',
  'TSCallSignatureDeclaration', 'TSConstructSignatureDeclaration',
  'TSFunctionType', 'TSConstructorType', 'TSMethodSignature',
])

const tokenizeName = tokenizers.name()
const documentationOptions = {
  tokenizers: [
    tokenizers.tag(),
    tokenizers.type('compact'),
    (spec) => spec.tag === 'param' ? tokenizeName(spec) : spec,
    tokenizers.description('compact'),
  ],
}

function adjacentDocumentation(sourceCode, declaration) {
  const comment = sourceCode.getTokenBefore(declaration, { includeComments: true })
  if (comment?.type !== 'Block' || !comment.value.startsWith('*')) return null
  if (declaration.loc.start.line - comment.loc.end.line > 1) return null

  // 上一条声明的行尾注释不属于下一条声明。
  const previous = sourceCode.getTokenBefore(comment, { includeComments: true })
  if (previous?.loc.end.line === comment.loc.start.line &&
      comment.loc.end.line < declaration.loc.start.line) return null

  return parse(`/*${comment.value}*/`, documentationOptions)[0] ?? null
}

function parameterName(parameter) {
  if (parameter.type === 'TSParameterProperty') return parameterName(parameter.parameter)
  if (parameter.type === 'AssignmentPattern') return parameterName(parameter.left)
  if (parameter.type === 'RestElement') return parameterName(parameter.argument)
  if (parameter.type === 'Identifier') return parameter.name
  // 解构参数在签名中没有根名称，由说明选定并按位置校验。
  return null
}

function isValue(node) {
  return Boolean(node) &&
    !(node.type === 'Identifier' && node.name === 'undefined') &&
    !(node.type === 'UnaryExpression' && node.operator === 'void')
}

function hasValueReturn(node, visitorKeys) {
  if (functions.has(node.type)) return false
  if (node.type === 'ReturnStatement') return isValue(node.argument)
  return (visitorKeys[node.type] ?? []).some((key) => {
    const child = node[key]
    return Array.isArray(child)
      ? child.some((element) => element && hasValueReturn(element, visitorKeys))
      : Boolean(child) && hasValueReturn(child, visitorKeys)
  })
}

function needsReturnDescription(node, visitorKeys) {
  if (['constructor', 'set'].includes(node.parent?.kind) ||
      ['TSConstructSignatureDeclaration', 'TSConstructorType'].includes(node.type)) return false
  if (node.async || node.generator) return true
  const annotation = node.returnType?.typeAnnotation
  if (annotation) return !['TSVoidKeyword', 'TSUndefinedKeyword', 'TSNeverKeyword'].includes(annotation.type)
  if (!node.body) return false
  if (node.body.type !== 'BlockStatement') return isValue(node.body)
  return hasValueReturn(node.body, visitorKeys)
}

function functionContract(declaration) {
  if (functions.has(declaration.type)) return declaration
  if (functions.has(declaration.init?.type)) return declaration.init
  if (functions.has(declaration.value?.type)) return declaration.value
  const annotation = declaration.typeAnnotation
  if (functions.has(annotation?.type)) return annotation
  if (functions.has(annotation?.typeAnnotation?.type)) return annotation.typeAnnotation
  return null
}

const publicContract = {
  meta: {
    type: 'problem',
    schema: [],
    messages: {
      description: '公开声明需要紧邻的 JSDoc 正文说明用途和约束。',
      params: 'JSDoc @param 应按签名顺序说明各参数，预期为 {{expected}}。',
      returns: '在 JSDoc @returns 中说明返回值或 Promise 的完成语义。',
      extraReturns: '同步无值返回的签名应将副作用写在 JSDoc 正文中。',
    },
  },
  create(context) {
    const sourceCode = context.sourceCode
    const inspected = new Set()

    function check(declaration, node = declaration) {
      if (inspected.has(node)) return
      inspected.add(node)
      const documentation = adjacentDocumentation(sourceCode, declaration)
      if (!documentation?.description.trim()) {
        context.report({ node, messageId: 'description' })
        return
      }

      const fn = functionContract(node)
      if (!fn) return
      const names = fn.params.map(parameterName)
      const params = documentation.tags
        .filter(({ tag }) => tag === 'param')
        .map((param) => ({ ...param, name: param.name.replace(/^\.\.\./u, '') }))
        .filter(({ name }) => !name.includes('.'))
      if (names.length !== params.length || names.some((name, i) =>
        !params[i].name || !params[i].description.trim() || (name !== null && name !== params[i].name))) {
        context.report({ node, messageId: 'params', data: { expected: names.map((name) => name ?? '解构参数').join('、') || '无参数' } })
      }

      const returns = documentation.tags.find(({ tag }) => tag === 'returns' || tag === 'return')
      if (needsReturnDescription(fn, sourceCode.visitorKeys)) {
        if (!returns?.description.trim()) context.report({ node, messageId: 'returns' })
      } else if (returns) {
        context.report({ node, messageId: 'extraReturns' })
      }
    }

    function checkExport(declaration, owner = declaration) {
      if (declaration.type === 'VariableDeclaration') {
        for (const binding of declaration.declarations) check(owner, binding)
        return
      }
      if (declaration.type === 'VariableDeclarator') {
        check(owner, declaration)
        return
      }

      check(owner, declaration)
      let members = []
      if (['ClassDeclaration', 'ClassExpression', 'TSInterfaceDeclaration'].includes(declaration.type)) {
        members = declaration.body.body
      } else if (declaration.type === 'TSTypeAliasDeclaration') {
        members = declaration.typeAnnotation.members ?? []
      }
      for (const member of members) {
        if (functionContract(member) && member.key?.type !== 'PrivateIdentifier' &&
            !['private', 'protected'].includes(member.accessibility)) check(member)
      }
    }

    function checkLocalExport(identifier) {
      const variable = sourceCode.getScope(identifier).set.get(identifier.name)
      for (const definition of variable?.defs ?? []) {
        if (definition.type === 'ImportBinding') continue
        const declaration = definition.node
        const statement = declaration.type === 'VariableDeclarator' ? declaration.parent : declaration
        const owner = statement.parent?.type === 'ExportNamedDeclaration' ? statement.parent : statement
        checkExport(declaration, owner)
      }
    }

    return {
      ExportNamedDeclaration(node) {
        if (node.source) return
        if (node.declaration) checkExport(node.declaration, node)
        for (const specifier of node.specifiers) checkLocalExport(specifier.local)
      },
      ExportDefaultDeclaration(node) {
        if (node.declaration.type === 'Identifier') checkLocalExport(node.declaration)
        else checkExport(node.declaration, node)
      },
    }
  },
}

export default publicContract
