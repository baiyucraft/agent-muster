import type { Agent } from '@deepseek-ai/dsh-agent'
import { type Delivery, type Hub, HubError } from './hub.js'

interface ActiveDelivery {
  delivery: Delivery
  signal: AbortSignal
}

type Input = Record<string, unknown>
const musterToolNames = ['muster_list_projects', 'muster_dispatch', 'muster_resume_task', 'muster_get_task']

/**
 * 检查中心会话是否明确允许该工具。
 * @param name 会话请求调用的工具名称。
 * @returns 名称在 Muster 工具清单中时为 true。
 */
export function toolAllowed(name: string): boolean {
  return musterToolNames.includes(name)
}

const string = { type: 'string' }
const output = {
  schema: { type: 'object' as const, additionalProperties: true },
  render: (_args: unknown, value: unknown) => [{ type: 'text' as const, text: JSON.stringify(value) }],
}

/**
 * 在中心会话注册任务查询、派发和续接操作，并校验当前投递是否允许写入。
 * @param agent 应接收工具的中心 dsh Agent。
 * @param hub 提供任务和项目 API 的客户端。
 * @param active 获取会话当前持有的投递及取消信号。
 * @returns 释放本次注册的工具和授权守卫的回调。
 */
export function bindTools(agent: Agent, hub: Hub, active: () => ActiveDelivery | undefined): () => void {
  // The center can inspect and route work using these agent-local tools.
  const disposers: (() => void)[] = []

  function register(
    name: string, description: string, fields: string[],
    execute: (input: Input, delivery: Delivery, signal: AbortSignal) => Promise<unknown>,
    optional: string[] = [],
  ) {
    const properties = Object.fromEntries([...fields, ...optional].map(field => [field, string]))
    disposers.push(agent.ctx.tools.register({
      name,
      description,
      parameters: { type: 'object', properties, required: fields, additionalProperties: false },
      output,
      async execute(input, execution) {
        const current = active()
        if (!current || execution.agent !== agent) throw new Error('Muster 没有可处理的当前投递')
        for (const field of fields) {
          if (!input || typeof input !== 'object' || typeof (input as Input)[field] !== 'string' ||
            !(input as Record<string, string>)[field].trim()) {
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
            ? 'Hub 状态已变化或这条消息已经派发。请选择查询工具核对。'
            : `Hub 无法处理该请求（HTTP ${error.status}）。`)
        }
      },
    }))
  }

  function requireMessage(delivery: Delivery) {
    if (delivery.kind !== 'message') throw new Error('交付报告只允许查询，不能发起任务')
  }

  try {
    const names = ['muster_list_projects', 'muster_dispatch', 'muster_resume_task', 'muster_get_task']
    register(names[0], '查看项目、设备和可选 Agent 绑定，派发前找到项目及绑定 ID。', [],
      (_input, _delivery, signal) => hub.projects(signal))
    register(names[1], '把工作派到已绑定项目，立即返回任务 ID。用 binding_id 选择指定设备和 Agent。用户明确要求 Git 候选提交及项目测试且项目有 git_policy 时，workflow 用 git_verify。git_verify 会报告受测提交，Review 和推送另有交付流程。', ['project_id', 'title', 'prompt'],
      (input, delivery, signal) => {
        requireMessage(delivery)
        const { project_id, binding_id, title, prompt, workflow } = input as Record<string, string>
        if (workflow !== undefined && workflow !== 'git_verify') throw new Error('未知的项目工作流')
        return hub.dispatch(delivery, { project_id, binding_id, title, prompt, git_delivery: workflow === 'git_verify' || undefined }, signal)
      }, ['binding_id', 'workflow'])
    register(names[2], '把明确追加的要求交给该任务的原设备和原生会话。', ['task_id', 'prompt'],
      (input, delivery, signal) => {
        requireMessage(delivery)
        const { task_id, prompt } = input as Record<string, string>
        return hub.dispatch(delivery, { task_id, prompt }, signal)
      })
    register(names[3], '查看已有任务状态、执行记录和已登记结果。', ['task_id'],
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
