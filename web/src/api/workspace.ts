import { request } from './devices'

/** 主对话中的消息，可能关联任务及中心 Agent 的处理状态。 */
export type Message = {
  id: string
  kind: 'user' | 'reply' | 'report' | 'approval'
  text: string
  task_id?: string
  status?: 'accepted' | 'handled' | 'interrupted'
  created_at: string
}

type RunStatus = 'queued' | 'running' | 'waiting' | 'waiting_delivery' | 'completed' | 'failed' | 'paused' | 'stopped' | 'cancelled' | 'interrupted' | 'approval_requested'

/** 任务生命周期操作。详情只展示 Hub 返回的可用操作。 */
export type TaskControlAction = 'pause' | 'stop_current' | 'resume' | 'cancel'

/** 重试同一次操作时保持请求 ID，新操作使用新的 ID。 */
export type TaskControlRequest = { action: TaskControlAction; request_id: string }

/** 看板的任务摘要，包含关联设备、Agent、依赖等待及待汇报状态。 */
export type Task = {
  id: string
  project_id: string
  project_name: string
  device_id: string
  status: Exclude<RunStatus, 'waiting' | 'stopped' | 'approval_requested'> | 'waiting_dependencies' | 'waiting_reply' | 'pausing' | 'stopping' | 'cancelling'
  title: string
  updated_at: string
  result?: string
  report_pending: boolean
  parent_task_id?: string
  root_task_id?: string
  binding_id?: string
  agent_kind?: string
  waiting_reason?: 'children' | 'delivery' | 'transfers'
  current_run_generation?: number
  wait_generation?: number
  control_actions?: TaskControlAction[]
}

/** 设备登记后对 Hub 可见的工作目录 ID 与展示名称。 */
export type Directory = { id: string; label: string; device_id: string }
/** 项目到单台设备工作目录及 Agent 的绑定，含是否为默认绑定。 */
export type ProjectBinding = { id: string; device_id: string; directory_id: string; directory_label: string; agent_kind: string; is_default: boolean }
/** 管理员为项目保存的 Git 交付规则。revision 由 Hub 管理，target_branch 为空时按任务生成。 */
export type GitPolicy = {
  revision?: number
  base_ref: string
  remote: string
  /** 目标分支的完整 refs/heads 引用。 */
  target_branch?: string
  /** 测试程序及按原顺序传入的独立参数，超时单位为秒。 */
  test: { program: string; args: string[]; timeout_seconds: number }
  review_agent?: 'dsh' | 'codex'
  max_repairs: number
}
/** 工作区快照内的项目及绑定信息，旧记录可能只提供设备和目录字段。 */
export type Project = { id: string; name: string; device_id: string; directory_id: string; directory_label: string; agent_kind?: string; bindings?: ProjectBinding[]; git_policy?: GitPolicy }
/** 保存项目名称和执行绑定。省略 git_policy 时保留规则，null 清除规则。 */
export type ProjectInput = { name: string; agent_kind: string; bindings: ProjectBinding[]; device_id: string; directory_id: string; git_policy?: Omit<GitPolicy, 'revision'> | null }

/** 一次任务执行记录，取得原生会话后可带会话 ID。 */
export type Run = {
  id: string
  task_id: string
  directory_id: string
  status: RunStatus
  session_id?: string
  text?: string
  diagnostics?: RunDiagnostics
  created_at: string
  updated_at: string
}

/** 设备为该轮运行上报的诊断摘要。缺失字段表示设备未能确认对应数据。 */
type RunDiagnostics = {
  stdout_bytes?: number
  stderr_bytes?: number
  exit_code?: number
  capture_incomplete?: boolean
  error_kind?: 'native_exit' | 'interrupted' | 'output' | 'setup'
  usage?: RunUsage
}

/** CLI 为本轮执行报告的 Token 计数。可选字段的统计口径由来源决定。 */
type RunUsage = {
  source: 'dsh_headless' | 'codex_exec'
  input_tokens: number
  output_tokens: number
  cached_input_tokens?: number
  cache_write_input_tokens?: number
  reasoning_output_tokens?: number
  total_tokens?: number
}

/** 设备交接的 Hub 元数据，size 为内容字节数，sha256 为内容摘要。 */
export type Transfer = {
  id: string
  run_id: string
  task_id: string
  source_device_id: string
  target_device_id: string
  name: string
  kind: string
  size: number
  sha256: string
  status: string
  attempts: number
  last_error?: string
  created_at: string
  received_at?: string
}

/** 任务依赖的代次与完成状态，代次用于区分不同等待轮次。 */
export type Dependency = { task_id: string; depends_on_task_id: string; generation: number; status: string; active?: boolean }
/** 关联到任务的输入记录，可来自消息、子任务依赖或执行操作。 */
export type TaskInput = {
  id: string
  kind: 'message' | 'delegation' | 'dependency' | 'execution' | 'approval'
  status: string
  created_at: string
  message_id?: string
  run_id?: string
  generation?: number
  claimed_at?: string
  transfer_ids?: string[]
}
/** 某次等待代次所需的交接记录，接收后可带确认时间。 */
export type WaitedTransfer = { transfer_id: string; generation: number; active: boolean; status: 'waiting' | 'received'; received_at?: string }
/** 请示及已保存的答复。任务、根任务和运行 ID 由 Hub 记录。 */
export type Approval = {
  id: string
  task_id: string
  root_task_id: string
  run_id: string
  question: string
  options: string[]
  impact?: string
  status: 'pending' | 'answered' | 'handled' | 'cancelled'
  answer?: string
  created_at: string
  answered_at?: string
  handled_at?: string
}

const approvalStage: Record<Approval['status'], number> = { pending: 0, answered: 1, handled: 2, cancelled: 3 }

/**
 * 比较同一条请示的两份记录，保留已确认的最新状态。
 * @param candidate 收到的记录，可能来自较早发出的快照请求。
 * @param known 已显示的同一条请示，可为空。
 * @returns 处理进度最新的记录，状态相同则返回收到的记录。
 */
export function latestApproval(candidate: Approval, known?: Approval): Approval {
  return known && approvalStage[known.status] > approvalStage[candidate.status] ? known : candidate
}

/** 主对话和看板的同一版本快照，cursor 用于后续 SSE 订阅。 */
export type Workspace = { cursor: number; messages: Message[]; tasks: Task[]; projects: Project[]; directories: Directory[]; approvals: Approval[] }
/** 任务详情快照，包含执行、输入、子任务、依赖和交接记录。 */
export type TaskDetail = {
  task: Task
  runs: Run[]
  messages: Message[]
  children: Task[]
  dependencies: Dependency[]
  transfers: Transfer[]
  inputs: TaskInput[]
  waited_transfers: WaitedTransfer[]
  approvals: Approval[]
}

/** 一条用户消息的 UTF-8 字节上限。 */
export const messageBytes = 6_000
/** 一条请示答复的 UTF-8 字节上限。 */
export const approvalAnswerBytes = 2_000
/** 项目名称的 UTF-8 字节上限。 */
export const projectNameBytes = 120

const textEncoder = new TextEncoder()

/**
 * 用去除首尾空白后的 UTF-8 长度预检 Hub 文本限制。
 * @param value 待校验文本。
 * @param maxBytes 允许的最大字节数。
 * @returns 文本没有 NUL 且编码长度未超过上限时为 true。
 */
export function withinTextLimit(value: string, maxBytes: number): boolean {
  return !value.includes('\0') && textEncoder.encode(value.trim()).length <= maxBytes
}

/**
 * 读取带更新游标的工作区快照，补齐缺省列表。
 * @param token 当前管理员会话的令牌。
 * @param signal 终止快照请求。
 * @returns 游标校验成功的工作区快照。
 */
export async function readWorkspace(token: string, signal: AbortSignal): Promise<Workspace> {
  const response = await request(token, '/api/v1/workspace', { signal })
  const snapshot = (await response.json()) as Workspace
  if (!Number.isSafeInteger(snapshot.cursor)) throw new Error('Hub 返回了无效的更新游标。')
  return {
    ...snapshot, messages: snapshot.messages ?? [], tasks: snapshot.tasks ?? [], projects: snapshot.projects ?? [],
    directories: snapshot.directories ?? [], approvals: snapshot.approvals ?? [],
  }
}

/**
 * 读取任务详情并将未提供的相关列表统一为空数组。
 * @param token 当前管理员会话的令牌。
 * @param id Hub 分配的任务 ID。
 * @param signal 终止详情请求。
 * @returns 任务、运行及关联记录。
 */
export async function readTask(token: string, id: string, signal: AbortSignal): Promise<TaskDetail> {
  const response = await request(token, `/api/v1/tasks/${encodeURIComponent(id)}`, { signal })
  const detail = (await response.json()) as TaskDetail
  return {
    ...detail, runs: detail.runs ?? [], messages: detail.messages ?? [], children: detail.children ?? [],
    dependencies: detail.dependencies ?? [], transfers: detail.transfers ?? [],
    inputs: detail.inputs ?? [], waited_transfers: detail.waited_transfers ?? [], approvals: detail.approvals ?? [],
  }
}

/**
 * 请求 Hub 改变任务的调度状态。对运行中的任务，设备确认后才进入最终状态。
 * @param token 当前管理员会话的令牌。
 * @param id Hub 分配的任务 ID。
 * @param input 操作和用于重试去重的客户端请求 ID。
 * @param signal 停止等待响应。
 * @returns Hub 当前确认的任务记录。
 */
export async function controlTask(token: string, id: string, input: TaskControlRequest, signal: AbortSignal): Promise<Task> {
  const response = await request(token, `/api/v1/tasks/${encodeURIComponent(id)}/control`, {
    signal,
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
  return (await response.json()) as Task
}

/**
 * 提交请示的答复。并发答复成功时，Hub 返回首条有效的已保存答复。
 * @param token 当前管理员会话的令牌。
 * @param id Hub 分配的请示 ID。
 * @param answer 去除首尾空白后的非空答复。
 * @param signal 终止等待 HTTP 响应。
 * @returns Hub 已保存的请示记录。
 */
export async function answerApproval(token: string, id: string, answer: string, signal: AbortSignal): Promise<Approval> {
  const response = await request(token, `/api/v1/approvals/${encodeURIComponent(id)}/answer`, {
    signal,
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ answer }),
  })
  return (await response.json()) as Approval
}

/**
 * 提交一条带客户端 ID 的主对话消息，按需关联既有任务。
 * @param token 当前管理员会话的令牌。
 * @param input 待提交的消息 ID、文本和可选任务 ID。
 * @param signal 终止发送请求。
 * @returns Hub 受理后返回的消息记录。
 */
export async function sendMessage(token: string, input: Pick<Message, 'id' | 'text' | 'task_id'>, signal: AbortSignal): Promise<Message> {
  const response = await request(token, '/api/v1/conversation/messages', {
    signal,
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
  return (await response.json()) as Message
}

/**
 * 写入指定项目及其执行绑定。
 * @param token 当前管理员会话的令牌。
 * @param id 要创建或更新的项目 ID。
 * @param input 项目信息及各执行目标。
 * @param signal 终止保存请求。
 * @returns Hub 保存后返回的项目。
 */
export async function saveProject(token: string, id: string, input: ProjectInput, signal: AbortSignal): Promise<Project> {
  const response = await request(token, `/api/v1/projects/${encodeURIComponent(id)}`, {
    signal,
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(input),
  })
  return (await response.json()) as Project
}

/**
 * 从已有游标订阅 SSE，遇到新工作区事件后释放读取流。
 * @param token 当前管理员会话的令牌。
 * @param cursor 最近一次工作区快照的游标。
 * @param signal 停止等待更新的信号。
 * @returns 观察到更新游标时为 true，连接结束或等待取消而无更新时可为 false。
 */
export async function waitForWorkspaceEvent(token: string, cursor: number, signal: AbortSignal): Promise<boolean> {
  const response = await request(token, `/api/v1/events?after=${cursor}`, {
    signal,
    headers: { Accept: 'text/event-stream' },
  }, 18_000)
  const reader = response.body?.getReader()
  if (!reader || !response.headers.get('Content-Type')?.startsWith('text/event-stream')) {
    throw new Error('Hub 返回了无法读取的实时更新。')
  }

  const decoder = new TextDecoder()
  let buffer = ''
  let event = ''
  let eventID = 0
  try {
    while (!signal.aborted) {
      const { done, value } = await reader.read()
      if (done) return false
      buffer += decoder.decode(value, { stream: true })
      let lineBreak = buffer.indexOf('\n')
      while (lineBreak !== -1) {
        const line = buffer.slice(0, lineBreak).replace(/\r$/, '')
        buffer = buffer.slice(lineBreak + 1)
        if (line === '') {
          if (event === 'workspace' && eventID > cursor) return true
          event = ''
          eventID = 0
        } else if (line.startsWith('event:')) {
          event = line.slice(6).trimStart()
        } else if (line.startsWith('id:')) {
          eventID = Number(line.slice(3).trim())
        }
        lineBreak = buffer.indexOf('\n')
      }
      if (buffer.length > 65_536) throw new Error('Hub 返回了过长的实时更新。')
    }
    return false
  } finally {
    await reader.cancel().catch(() => {})
  }
}

/**
 * 生成客户端提交 ID，适用于 HTTPS 与普通 HTTP 工作台。
 * @returns 基于浏览器随机字节的 UUID v4 字符串。
 */
export function newID(): string {
  // randomUUID is unavailable in browsers on plain HTTP origins.
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  bytes[6] = (bytes[6] & 15) | 64
  bytes[8] = (bytes[8] & 63) | 128
  const hex = Array.from(bytes, (value) => value.toString(16).padStart(2, '0')).join('')
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
}
