/** Hub 分给中心会话的消息或汇报，携带当前领取租约和尝试次数。 */
export interface Delivery {
  id: string
  lease: string
  kind: 'message' | 'report'
  attempt: number
  text: string
  message_id?: string
  task_id?: string
  project_id?: string
  /** Hub 声明接受完成回执中的中心轮次用量。 */
  usage_supported?: boolean
  /** Hub 声明当前派发接口接受包含前后依赖的任务组。 */
  plan_supported?: boolean
}

/** 创建或续接单项工作的输入，续接时以 task_id 指定目标。 */
interface SingleDispatch {
  project_id?: string
  binding_id?: string
  task_id?: string
  title?: string
  prompt: string
  git_delivery?: boolean
  /** 用户明确要求交付到项目预设远端时设为 true，仅用于新建根任务。 */
  git_publish?: boolean
}

/** 任务组中的设备步骤，执行绑定在首次派发时冻结。 */
export interface DispatchStep {
  project_id: string
  binding_id?: string
  title: string
  prompt: string
  /** 从 1 开始编号，只能引用本组更早的步骤。 */
  depends_on?: number[]
}

/** 创建包含二至八项设备步骤的根任务，标题用于看板和最终汇报。 */
export interface PlanDispatch {
  title: string
  steps: DispatchStep[]
}

/** 一次投递对应一个派发操作，可提交单项工作或任务组。 */
export type Dispatch = SingleDispatch | PlanDispatch

/** dsh 对本次已完成原生轮次中所有模型请求确认的 Token 计数。 */
export interface CenterTurnUsage {
  source: 'dsh_center_turn'
  /** 未计入缓存读写的输入 Token 数。 */
  input_tokens: number
  output_tokens: number
  cached_input_tokens?: number
  cache_write_input_tokens?: number
  reasoning_output_tokens?: number
  total_tokens: number
}

/** 中心会话完成投递时提交的回复、结束状态和可确认的用量。 */
export interface Outcome {
  text: string
  status: 'handled' | 'interrupted'
  /** 原生会话缺少完整计量时省略。 */
  usage?: CenterTurnUsage
}

/** 保留 Hub API 返回的 HTTP 状态以判断租约或认证是否有效。 */
export class HubError extends Error {
  /**
   * 生成 Hub 请求失败的错误。
   * @param status Hub 返回的 HTTP 状态码。
   */
  constructor(readonly status: number) {
    super(`Hub HTTP ${status}`)
  }
}

/** 中心 dsh 插件的 Hub 客户端，统一鉴权和请求取消。 */
export class Hub {
  private readonly base: URL

  /**
   * 校验插件连接地址并初始化 Hub 客户端。
   * @param address Hub 的 HTTP 或 HTTPS origin，不能带路径、查询或凭据。
   * @param token Hub 签发给中心插件的访问令牌。
   */
  constructor(address: string, private readonly token: string) {
    this.base = new URL(address)
    if (!['http:', 'https:'].includes(this.base.protocol) ||
      this.base.username || this.base.password || this.base.search || this.base.hash ||
      this.base.pathname !== '/') {
      throw new Error('MUSTER_DSH_HUB_URL must be an HTTP or HTTPS origin')
    }
  }

  /**
   * 尝试从 Hub 领取一件待处理投递并校验最基本的租约字段。
   * @param signal 取消领取请求。
   * @returns 当前领到的投递，Hub 无待处理投递时为 undefined。
   */
  async claim(signal: AbortSignal): Promise<Delivery | undefined> {
    const response = await this.request('POST', 'deliveries/claim', signal)
    if (response.status === 204) return undefined
    const delivery = await response.json() as Delivery
    if (typeof delivery.id !== 'string' || !delivery.id || typeof delivery.lease !== 'string' || !delivery.lease ||
      typeof delivery.text !== 'string' || !Number.isSafeInteger(delivery.attempt) || delivery.attempt < 1 ||
      !['message', 'report'].includes(delivery.kind)) {
      throw new Error('Hub returned an invalid delivery')
    }
    return delivery
  }

  /**
   * 延长中心正在处理的投递租约。
   * @param delivery 已领取并带当前租约的投递。
   * @param signal 取消续租请求。
   * @returns Hub 确认续租后完成，租约无效或请求失败时拒绝。
   */
  async renew(delivery: Delivery, signal: AbortSignal): Promise<void> {
    await this.request('PUT', this.route(delivery, 'lease'), signal, { lease: delivery.lease })
  }

  /**
   * 把中心会话结果交回 Hub 并结束本轮投递。
   * @param delivery 已领取并带当前租约的投递。
   * @param outcome 可向用户回报的文本和处理状态。
   * @param signal 取消确认请求。
   * @returns Hub 确认结果后完成，请求失败时拒绝。
   */
  async complete(delivery: Delivery, outcome: Outcome, signal: AbortSignal): Promise<void> {
    const { text, status, usage } = outcome
    await this.request('PUT', this.route(delivery, 'complete'), signal, {
      lease: delivery.lease, text, status, usage: delivery.usage_supported === true ? usage : undefined,
    })
  }

  /**
   * 读取中心可用的项目和执行绑定。
   * @param signal 取消列表请求。
   * @returns Hub 返回的项目列表 JSON。
   */
  async projects(signal: AbortSignal): Promise<unknown> {
    return (await this.request('GET', 'projects', signal)).json()
  }

  /**
   * 在当前投递租约下向 Hub 创建或续接任务。
   * @param delivery 允许发起操作的投递及租约。
   * @param input 单项任务、续接要求或包含各自目标绑定的任务组。
   * @param signal 取消任务提交请求。
   * @returns Hub 受理后返回的任务 JSON。
   */
  async dispatch(delivery: Delivery, input: Dispatch, signal: AbortSignal): Promise<unknown> {
    if ('steps' in input && delivery.plan_supported !== true) {
      throw new Error('当前 Hub 支持单项派发，请使用 muster_dispatch。')
    }
    const response = await this.request('POST', this.route(delivery, 'dispatch'), signal, { lease: delivery.lease, ...input })
    return response.json()
  }

  /**
   * 查询指定 Hub 任务及相关执行状态。
   * @param id Hub 分配的任务 ID。
   * @param signal 取消详情请求。
   * @returns Hub 返回的任务详情 JSON。
   */
  async task(id: string, signal: AbortSignal): Promise<unknown> {
    return (await this.request('GET', `tasks/${encodeURIComponent(id)}`, signal)).json()
  }

  private route(delivery: Delivery, action: string): string {
    return `deliveries/${encodeURIComponent(delivery.id)}/${action}`
  }

  private async request(method: string, route: string, signal: AbortSignal, body?: object): Promise<Response> {
    const headers = new Headers({ Authorization: `Bearer ${this.token}`, Accept: 'application/json' })
    if (body) headers.set('Content-Type', 'application/json')
    const response = await fetch(new URL(`/api/v1/plugin/${route}`, this.base), {
      method, headers, body: body && JSON.stringify(body),
      redirect: 'error', signal: AbortSignal.any([signal, AbortSignal.timeout(5_000)]),
    })
    if (!response.ok) {
      await response.body?.cancel()
      throw new HubError(response.status)
    }
    return response
  }
}
