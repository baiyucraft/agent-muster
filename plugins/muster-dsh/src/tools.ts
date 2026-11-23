import type { Agent } from '@deepseek-ai/dsh-agent'
import type { ObjectJsonSchema } from '@deepseek-ai/dsh-tools'
import { type Delivery, type DispatchStep, type Hub, HubError, type ProposedBinding } from './hub.js'

interface ActiveDelivery {
  delivery: Delivery
  signal: AbortSignal
}

type Input = Record<string, unknown>
const musterToolNames = [
  'muster_list_projects', 'muster_dispatch', 'muster_dispatch_plan', 'muster_resume_task', 'muster_get_task',
  'muster_binding_candidates', 'muster_propose_binding',
]

/**
 * 检查中心会话是否明确允许该工具。
 * @param name 会话请求调用的工具名称。
 * @returns 名称在 Muster 工具清单中时为 true。
 */
export function toolAllowed(name: string): boolean {
  return musterToolNames.includes(name)
}

const string = { type: 'string' } as const
const output = {
  schema: { type: 'object' as const, additionalProperties: true },
  render: (_args: unknown, value: unknown) => [{ type: 'text' as const, text: JSON.stringify(value) }],
}

const dispatchStepSchema: ObjectJsonSchema = {
  type: 'object',
  required: ['project_id', 'title', 'prompt'],
  additionalProperties: false,
  properties: {
    project_id: { ...string, description: '已登记的项目 ID。' },
    binding_id: { ...string, description: '可选的项目绑定 ID，省略时使用项目默认绑定。' },
    title: { ...string, description: '这一步在看板上的标题。' },
    prompt: { ...string, description: '交给设备 Agent 的独立工作指令。' },
    depends_on: {
      type: 'array',
      items: { type: 'integer' },
      description: '前置步骤的序号，从 1 开始，且只能引用更早的步骤。省略表示可以独立执行。',
    },
  },
}

const proposedBindingSchema: ObjectJsonSchema = {
  type: 'object',
  required: ['device_id', 'directory_id', 'agent_kind', 'is_default'],
  additionalProperties: false,
  properties: {
    device_id: { ...string, description: '候选设备 ID。' },
    directory_id: { ...string, description: '该设备已登记的目录 ID。' },
    agent_kind: { type: 'string', enum: ['dsh', 'codex'], description: '该设备已探测到完整调用能力的 Agent。' },
    is_default: { type: 'boolean', description: '这一组中只能有一项为 true。' },
  },
}

/**
 * 为使用字符串入参的工具生成声明，额外字段由调用点定义。
 * @param required 每次调用都要提供非空值的字段。
 * @param optional 模型可省略的字符串字段。
 * @returns 供中心 dsh 注册工具使用的参数声明。
 */
function strings(required: string[], optional: string[] = []): ObjectJsonSchema {
  return {
    type: 'object',
    required,
    additionalProperties: false,
    properties: Object.fromEntries([...required, ...optional].map(field => [field, string])),
  }
}

/**
 * 检查嵌套工具参数以及仅能引用前序步骤的依赖约束。
 * @param steps 模型提交的步骤参数。
 * @returns 通过检查的二至八项步骤，保留原顺序和依赖序号。
 * @throws 步骤结构无效或包含重复、越界的依赖时拒绝派发。
 */
function parseSteps(steps: unknown): DispatchStep[] {
  if (!Array.isArray(steps) || steps.length < 2 || steps.length > 8) {
    throw new Error('任务组需要填写 2 至 8 个步骤。')
  }
  return steps.map((step: unknown, index) => {
    if (!step || typeof step !== 'object' || Array.isArray(step)) {
      throw new Error(`第 ${index + 1} 步需要填写项目、标题和提示词。`)
    }
    const { project_id, binding_id, title, prompt, depends_on } = step as Input
    if (typeof project_id !== 'string' || !project_id.trim() ||
      typeof title !== 'string' || !title.trim() || typeof prompt !== 'string' || !prompt.trim() ||
      (binding_id !== undefined && typeof binding_id !== 'string')) {
      throw new Error(`第 ${index + 1} 步的项目、绑定、标题或提示词无效。`)
    }
    if (depends_on !== undefined && (!Array.isArray(depends_on) ||
      depends_on.some(value => !Number.isSafeInteger(value) || value < 1 || value > index) ||
      new Set(depends_on).size !== depends_on.length)) {
      throw new Error(`第 ${index + 1} 步的依赖必须为互不重复的前序步骤编号。`)
    }
    return { project_id, binding_id, title, prompt, depends_on }
  })
}

/**
 * 校验中心模型选中的项目绑定，设备、目录和 Agent 的实际归属由 Hub 核对。
 * @param bindings 模型为提案提交的绑定数组。
 * @returns 一至三十二项有效绑定，恰有一项为默认项。
 * @throws 绑定字段无效，或默认项数量错误时拒绝。
 */
function parseProposedBindings(bindings: unknown): ProposedBinding[] {
  if (!Array.isArray(bindings) || bindings.length < 1 || bindings.length > 32) {
    throw new Error('项目提案需要 1 至 32 项绑定。')
  }
  const result = bindings.map((binding: unknown): ProposedBinding => {
    if (!binding || typeof binding !== 'object' || Array.isArray(binding)) {
      throw new Error('项目绑定需要填写设备、目录、Agent 和默认标记。')
    }
    const { device_id, directory_id, agent_kind, is_default } = binding as Input
    if (typeof device_id !== 'string' || !device_id.trim() || typeof directory_id !== 'string' || !directory_id.trim() ||
      (agent_kind !== 'dsh' && agent_kind !== 'codex') || typeof is_default !== 'boolean') {
      throw new Error('项目绑定的设备、目录、Agent 或默认标记无效。')
    }
    return { device_id, directory_id, agent_kind, is_default }
  })
  if (result.filter(binding => binding.is_default).length !== 1) throw new Error('项目提案需要选定唯一的默认绑定。')
  return result
}

function requireBindingProposalSupport(delivery: Delivery): void {
  if (delivery.kind !== 'message' || delivery.task_id || delivery.binding_proposals_supported !== true) {
    throw new Error('当前投递未启用项目绑定提案。可以从项目管理页完成绑定。')
  }
}

/**
 * 在中心会话注册项目提案及任务查询、派发和续接操作，并校验当前投递是否允许写入。
 * @param agent 应接收工具的中心 dsh Agent。
 * @param hub 提供任务和项目 API 的客户端。
 * @param active 获取会话当前持有的投递及取消信号。
 * @returns 释放本次注册的工具和授权守卫的回调。
 */
export function bindTools(agent: Agent, hub: Hub, active: () => ActiveDelivery | undefined): () => void {
  const disposers: (() => void)[] = []

  function register(
    name: string, description: string, parameters: ObjectJsonSchema,
    execute: (input: Input, delivery: Delivery, signal: AbortSignal) => Promise<unknown>,
  ) {
    disposers.push(agent.ctx.tools.register({
      name,
      description,
      parameters: { ...parameters },
      output,
      async execute(input, execution) {
        const current = active()
        if (!current || execution.agent !== agent) throw new Error('Muster 没有可处理的当前投递')
        const args = input as Input | undefined
        for (const field of parameters.required ?? []) {
          const value = args?.[field]
          if (value === undefined || (parameters.properties?.[field]?.type === 'string' &&
            (typeof value !== 'string' || !value.trim()))) {
            throw new Error(`缺少工具参数 ${field}`)
          }
        }
        const signal = AbortSignal.any([execution.signal, current.signal])
        signal.throwIfAborted()
        try {
          return await execute(input as Input, current.delivery, signal)
        } catch (error) {
          if (!(error instanceof HubError)) throw error
          throw new Error(error.status === 409
            ? 'Hub 状态已变化，或这条消息已经提交过任务或项目绑定提案。请选择查询工具核对。'
            : `Hub 无法处理该请求（HTTP ${error.status}）。`)
        }
      },
    }))
  }

  function requireMessage(delivery: Delivery) {
    if (delivery.kind !== 'message') throw new Error('交付报告只允许查询，不能发起任务')
    if (delivery.binding_proposal) throw new Error('已为本条消息提交项目绑定提案。请在主对话查看确认状态。')
  }

  try {
    register('muster_list_projects', '查看项目、设备和可选 Agent 绑定，派发前找到项目及绑定 ID。', strings([]),
      (_input, _delivery, signal) => hub.projects(signal))
    register('muster_binding_candidates', '为当前主对话消息查找已登记的设备、目录和 Agent 能力。用设备和目录名称匹配用户目标，使用返回的 ID。', strings([]),
      (_input, delivery, signal) => {
        requireBindingProposalSupport(delivery)
        return hub.bindingCandidates(signal)
      })
    register('muster_propose_binding', '为未绑定的明确目标提交项目绑定提案。先查候选，只选择已登记目录和调用能力完整的 Agent。用户在主对话确认后才会建立项目，任务需由用户另发消息安排。', {
      type: 'object',
      required: ['name', 'bindings'],
      additionalProperties: false,
      properties: {
        name: { ...string, description: '用户指定的项目名称。' },
        bindings: { type: 'array', description: '一至三十二项绑定。', items: proposedBindingSchema },
      },
    }, async (input, delivery, signal) => {
      requireBindingProposalSupport(delivery)
      const proposal = await hub.proposeBinding(delivery, {
        name: (input.name as string).trim(),
        bindings: parseProposedBindings(input.bindings),
      }, signal)
      delivery.binding_proposal = proposal
      return proposal
    })
    register('muster_dispatch', '把工作派到已绑定项目，立即返回任务 ID。用 binding_id 选择设备和 Agent。项目有 git_policy 时，用户要求 Git 候选提交和项目测试可选 git_verify，用户明确要求推送到项目预设目标才可选 git_publish。后者按项目配置测试、审查并核对推送结果。', strings(['project_id', 'title', 'prompt'], ['binding_id', 'workflow']),
      (input, delivery, signal) => {
        requireMessage(delivery)
        const { project_id, binding_id, title, prompt, workflow } = input as Record<string, string>
        if (workflow !== undefined && workflow !== 'git_verify' && workflow !== 'git_publish') throw new Error('未知的项目工作流')
        if (workflow === 'git_publish' && delivery.task_id) throw new Error('Git 推送交付只能在创建新任务时启用')
        return hub.dispatch(delivery, {
          project_id, binding_id, title, prompt,
          git_delivery: workflow !== undefined || undefined,
          git_publish: workflow === 'git_publish' || undefined,
        }, signal)
      })
    register('muster_dispatch_plan', '当前消息注明支持分组派发时，可为同一目标创建根任务和 2 至 8 个可独立验收的常规设备步骤。先查询项目及绑定。按顺序填写步骤，用 depends_on 指定可选依赖。返回一个根任务 ID。项目配置的 Git 验证和推送使用 muster_dispatch。', {
      type: 'object',
      required: ['title', 'steps'],
      additionalProperties: false,
      properties: { title: string, steps: { type: 'array', items: dispatchStepSchema } },
    }, (input, delivery, signal) => {
      requireMessage(delivery)
      if (delivery.task_id) throw new Error('已关联任务的追加要求请使用 muster_resume_task。')
      if (delivery.plan_supported !== true) throw new Error('当前 Hub 支持单项派发，请使用 muster_dispatch。')
      return hub.dispatch(delivery, { title: input.title as string, steps: parseSteps(input.steps) }, signal)
    })
    register('muster_resume_task', '把明确追加的要求交给已有设备执行的任务，沿用它的原设备和原生会话。', strings(['task_id', 'prompt']),
      (input, delivery, signal) => {
        requireMessage(delivery)
        const { task_id, prompt } = input as Record<string, string>
        return hub.dispatch(delivery, { task_id, prompt }, signal)
      })
    register('muster_get_task', '查看已有任务状态、执行记录和已登记结果。', strings(['task_id']),
      (input, _delivery, signal) => hub.task(input.task_id as string, signal))
    disposers.unshift(
      agent.ctx.tools.restrict({ allow: [] }),
      agent.ctx.tools.guard(execution => toolAllowed(execution.name) ? undefined : '该工具未获 Muster 主会话授权'),
    )
  } catch (error) {
    disposers.reverse().forEach(dispose => dispose())
    throw error
  }
  return () => disposers.reverse().forEach(dispose => dispose())
}
