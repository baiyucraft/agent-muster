import { useEffect, useState } from 'react'
import { AdminTokenError, readDevices } from '../../api/devices'
import { latestApproval, readTask, type Approval, type Run, type Task, type TaskDetail, type TaskInput, type Transfer } from '../../api/workspace'
import { messageFor } from '../../useWorkspace'
import { ApprovalCard, type ApprovalActions } from './ApprovalCard'
import { MessageItem, Time } from './Conversation'
import { TaskControls, type TaskControlActions } from './TaskControls'

const statusText: Record<Task['status'] | Run['status'], string> = {
  queued: '排队', running: '运行中', waiting: '等待', waiting_dependencies: '等待子任务', waiting_delivery: '等待交付',
  waiting_reply: '等待答复', approval_requested: '已发起请示',
  completed: '已完成', failed: '执行失败', pausing: '正在暂停', paused: '已暂停', stopping: '正在停止', stopped: '已停止',
  cancelling: '正在取消', cancelled: '已取消', interrupted: '待核对',
}
const groups: { stage: string; name: string; states: Task['status'][] }[] = [
  { stage: 'queued', name: '排队', states: ['queued'] },
  { stage: 'running', name: '运行中', states: ['running', 'pausing', 'stopping', 'cancelling'] },
  { stage: 'waiting', name: '等待', states: ['waiting_reply', 'waiting_dependencies', 'waiting_delivery', 'paused', 'interrupted'] },
  { stage: 'finished', name: '已结束', states: ['completed', 'failed', 'cancelled'] },
]

function TaskStatus({ status }: { status: Task['status'] | Run['status'] }) {
  return <span className="task-status" data-state={status}>{statusText[status] ?? status}</span>
}

const waitingReasonText: Record<string, string> = {
  delivery: '接收设备尚未确认收到任务输入',
  children: '等待所依赖的子任务结束并交付结果',
  transfers: '等待接收设备确认交接',
}

const taskInputText: Record<TaskInput['kind'], string> = {
  message: '主对话要求',
  delegation: '派发输入',
  dependency: '依赖续接',
  execution: '运行输入',
  approval: '请示答复',
}

const inputStatusText: Record<string, string> = {
  ...statusText,
  accepted: '已受理',
  handled: '已处理',
}

type TaskBoardProps = {
  token: string
  tasks?: Task[]
  approvals: Approval[]
  approvalActions: ApprovalActions
  taskControlActions: TaskControlActions
  cursor?: number
  taskID?: string
  onFollow: (task: Task) => void
  onLock: (reason: string) => void
}

export function TaskBoard({ token, tasks, approvals, approvalActions, taskControlActions, cursor, taskID, onFollow, onLock }: TaskBoardProps) {
  const [deviceNames, setDeviceNames] = useState<Map<string, string>>(() => new Map())
  useEffect(() => {
    const controller = new AbortController()
    void readDevices(token, controller.signal).then((devices) => {
      if (!controller.signal.aborted) setDeviceNames(new Map(devices.map((device) => [device.id, device.name])))
    }).catch((failure: unknown) => {
      if (!controller.signal.aborted && failure instanceof AdminTokenError) onLock(failure.message)
    })
    return () => controller.abort()
  }, [token, onLock])

  if (taskID) return <TaskDetails key={taskID} token={token} taskID={taskID} cursor={cursor} tasks={tasks} approvals={approvals}
    approvalActions={approvalActions} taskControlActions={taskControlActions} deviceNames={deviceNames} onFollow={onFollow} onLock={onLock} />

  const roots = tasks?.filter((task) => !task.parent_task_id)
  const childrenByParent = new Map<string, Task[]>()
  for (const task of tasks ?? []) {
    if (!task.parent_task_id) continue
    const children = childrenByParent.get(task.parent_task_id) ?? []
    children.push(task)
    childrenByParent.set(task.parent_task_id, children)
  }

  return (
    <section aria-labelledby="board-heading">
      <div className="view-heading"><h1 id="board-heading">任务看板</h1>{roots && <span className="view-count">{roots.length} 项任务</span>}</div>
      <BoardApprovals approvals={approvals} tasks={tasks} actions={approvalActions} />
      {!roots && <p className="panel-placeholder">正在读取看板…</p>}
      {roots?.length === 0 && <p className="panel-placeholder">暂无任务。<a className="text-link" href="#conversation">在主对话交办</a></p>}
      {roots && roots.length > 0 && <div className="board-grid">
        {groups.map((group) => {
          const entries = roots.filter((task) => group.states.includes(task.status))
          if (group.stage === 'waiting') entries.sort((left, right) => Number(right.status === 'waiting_reply') - Number(left.status === 'waiting_reply'))
          return (
            <section key={group.stage} className="board-column" data-stage={group.stage} aria-label={group.name}>
              <h2>{group.name}<span>{entries.length}</span></h2>
              {entries.length === 0 && <p className="column-empty">暂无任务</p>}
              {entries.map((task) => <TaskCard key={task.id} task={task} children={childrenByParent.get(task.id) ?? []} deviceName={deviceNames.get(task.device_id)} />)}
            </section>
          )
        })}
      </div>}
    </section>
  )
}

function BoardApprovals({ approvals, tasks, actions }: { approvals: Approval[]; tasks?: Task[]; actions: ApprovalActions }) {
  const [answeredHere, setAnsweredHere] = useState(() => new Set<string>())
  const visible = approvals.filter((approval) => approval.status === 'pending' || answeredHere.has(approval.id))
  const pendingCount = visible.filter((approval) => approval.status === 'pending').length
  const tasksByID = new Map(tasks?.map((task) => [task.id, task]))
  const boardActions: ApprovalActions = {
    ...actions,
    onAnswer: (id, answer) => {
      setAnsweredHere((current) => new Set(current).add(id))
      return actions.onAnswer(id, answer)
    },
  }
  if (!visible.length) return null
  return (
    <section className="approval-board" aria-labelledby="board-approvals">
      <h2 id="board-approvals">{pendingCount > 0 ? '待答复请示' : '请示答复'}</h2>
      {visible.map((approval) => <ApprovalCard key={approval.id} approval={approval} task={tasksByID.get(approval.task_id)} actions={boardActions} />)}
    </section>
  )
}

function TaskCard({ task, children, deviceName }: { task: Task; children: Task[]; deviceName?: string }) {
  return (
    <article className="task-tree-card">
      <a href={`#board/${task.id}`} className="task-card">
        <h3>{task.title}</h3>
        <div className="task-card-meta"><span>{task.project_name}</span><TaskStatus status={task.status} /></div>
        {task.waiting_reason && <p className="task-waiting-reason">{waitingReasonText[task.waiting_reason] ?? task.waiting_reason}</p>}
        <div className="task-card-facts"><span>{deviceName ?? task.device_id}</span><span>{agentName(task.agent_kind)}</span></div>
        <div className="task-card-footer"><Time value={task.updated_at} />{task.report_pending && <span>待汇报</span>}</div>
      </a>
      {children.length > 0 && <details><summary className="tree-toggle">{children.length} 项子任务</summary><ChildTasks tasks={children} /></details>}
    </article>
  )
}

function ChildTasks({ tasks }: { tasks: Task[] }) {
  return <div className="child-task-list">{tasks.map((task) => <a href={`#board/${task.id}`} className="child-task-row" key={task.id}><span>{task.title}</span><TaskStatus status={task.status} /></a>)}</div>
}

type TaskDetailsProps = Required<Pick<TaskBoardProps, 'token' | 'taskID' | 'onFollow' | 'onLock'>> & Pick<TaskBoardProps, 'cursor' | 'tasks' | 'approvals' | 'approvalActions' | 'taskControlActions'> & {
  deviceNames: Map<string, string>
}

function TaskDetails({ token, cursor, tasks, approvals, approvalActions, taskControlActions, taskID, deviceNames, onFollow, onLock }: TaskDetailsProps) {
  const [detail, setDetail] = useState<TaskDetail>()
  const [error, setError] = useState('')
  const [retry, setRetry] = useState(0)

  useEffect(() => {
    const controller = new AbortController()
    void readTask(token, taskID, controller.signal).then((value) => {
      if (controller.signal.aborted) return
      setDetail(value)
      setError('')
    }).catch((failure: unknown) => {
      if (controller.signal.aborted) return
      if (failure instanceof AdminTokenError) onLock(failure.message)
      else setError(messageFor(failure))
    })
    return () => controller.abort()
  }, [token, taskID, cursor, onLock, retry])

  const controlResult = taskControlActions.submissions.get(taskID)?.result
  const snapshotTask = tasks?.find((task) => task.id === taskID)
  const task = latestTask(detail?.task, latestTask(snapshotTask, controlResult))
  const canFollow = detail && task && !error && !task.parent_task_id && task.status !== 'cancelled' && task.status !== 'cancelling' && (
    task.status === 'queued' || task.status === 'running' || task.status === 'paused' || detail.runs.some((run) => run.session_id)
  )
  const taskName = (id: string) => tasks?.find((task) => task.id === id)?.title ?? id
  const taskApprovals = new Map(detail?.approvals.map((approval) => [approval.id, approval]))
  for (const approval of approvals) {
    if (approval.task_id !== taskID && approval.root_task_id !== taskID) continue
    taskApprovals.set(approval.id, latestApproval(approval, taskApprovals.get(approval.id)))
  }
  const relatedMessages = detail?.messages.filter((message) => message.kind !== 'approval' || !taskApprovals.has(message.id))

  return (
    <section aria-labelledby="task-heading">
      <a className="text-link back-link" href="#board">← 返回看板</a>
      <div className="view-heading">
        <h1 id="task-heading">{task?.title ?? '任务详情'}</h1>
        {canFollow && <button type="button" className="primary-button" onClick={() => onFollow(task)}>追加要求</button>}
      </div>
      {error && <div className="request-error" role="alert"><p>{error}</p><button type="button" className="outline-button" onClick={() => setRetry((current) => current + 1)}>重试</button></div>}
      {!detail && !error && <p className="panel-placeholder">正在读取详情…</p>}
      {taskApprovals.size > 0 && (
        <section className="approval-board" aria-labelledby="task-approvals">
          <h2 id="task-approvals">请示</h2>
          {[...taskApprovals.values()].map((approval) => (
            <ApprovalCard key={approval.id} approval={approval} actions={approvalActions}
              task={approval.task_id === taskID ? undefined : tasks?.find((task) => task.id === approval.task_id)} />
          ))}
        </section>
      )}
      {detail && task && <div className="task-detail-grid">
        <div className="task-detail-panel">
          {task.parent_task_id && <div className="task-ancestry">
            <a className="text-link" href={`#board/${task.parent_task_id}`}>父任务 {taskName(task.parent_task_id)}</a>
            {task.root_task_id && task.root_task_id !== task.parent_task_id && <a className="text-link" href={`#board/${task.root_task_id}`}>根任务 {taskName(task.root_task_id)}</a>}
          </div>}
          <dl className="task-facts">
            <div><dt>状态</dt><dd aria-live="polite"><TaskStatus status={task.status} />{task.report_pending && ' · 待汇报'}</dd></div>
            {task.waiting_reason && <div><dt>等待原因</dt><dd>{waitingReasonText[task.waiting_reason] ?? task.waiting_reason}</dd></div>}
            <div><dt>项目</dt><dd>{task.project_name}</dd></div>
            <div><dt>设备</dt><dd>{deviceNames.get(task.device_id) ?? task.device_id}</dd></div>
            <div><dt>Agent</dt><dd>{agentName(task.agent_kind)}</dd></div>
            <div><dt>更新时间</dt><dd><Time value={task.updated_at} /></dd></div>
          </dl>
          <TaskControls task={task} actions={taskControlActions} disabled={Boolean(error)} />
          {task.result && <div className="detail-result"><h2>结果</h2><p className="message-text">{task.result}</p></div>}
          <details className="task-technical">
            <summary>标识</summary>
            <dl className="task-facts">
              <div><dt>任务编号</dt><dd><code>{task.id}</code></dd></div>
              {task.binding_id && <div><dt>执行绑定</dt><dd><code>{task.binding_id}</code></dd></div>}
            </dl>
          </details>
        </div>
        <details className="task-detail-panel">
          <summary className="task-section-title">运行记录<span>{detail.runs.length}</span></summary>
          {detail.runs.length === 0 && <p className="standalone-note">等待首次运行。</p>}
          {detail.runs.map((run) => <article className="run-row" key={run.id}>
            <header><Time value={run.created_at} /><TaskStatus status={run.status} /></header>
            <p><span>运行</span> <code>{run.id}</code></p>
            {run.session_id && <p><span>会话</span> <code>{run.session_id}</code></p>}
            <RunDiagnostics diagnostics={run.diagnostics} />
            {run.text && <p className="message-text">{run.text}</p>}
          </article>)}
        </details>
        {detail.children.length > 0 && <section className="task-detail-panel" aria-labelledby="children-heading"><h2 id="children-heading">子任务</h2><ChildTasks tasks={detail.children} /></section>}
        {detail.dependencies.length > 0 && <section className="task-detail-panel" aria-labelledby="dependencies-heading">
          <h2 id="dependencies-heading">依赖任务</h2>
          <div className="child-task-list">{detail.dependencies.map((dependency) => <a className="child-task-row" href={`#board/${dependency.depends_on_task_id}`} key={`${dependency.depends_on_task_id}/${dependency.generation}`}>
            <span>{taskName(dependency.depends_on_task_id)}</span><span>{dependencyStatus(dependency.status)} · 第 {dependency.generation} 次等待{dependency.active ? '（当前）' : ''}</span>
          </a>)}</div>
        </section>}
        {(detail.inputs.length > 0 || detail.waited_transfers.length > 0) && <section className="task-detail-panel" aria-labelledby="inputs-heading">
          <h2 id="inputs-heading">任务输入与等待</h2>
          {detail.inputs.map((input) => <article className="run-row" key={input.id}>
            <header><strong>{taskInputText[input.kind]}</strong><span>{inputStatusText[input.status] ?? input.status}</span></header>
            <p><Time value={input.created_at} />{input.generation ? ` · 执行第 ${input.generation} 轮` : ''}{input.transfer_ids?.length ? ` · ${input.transfer_ids.length} 项交接` : ''}</p>
          </article>)}
          {detail.waited_transfers.map((wait) => <article className="run-row" key={`${wait.transfer_id}/${wait.generation}`}>
            <header><strong>{detail.transfers.find((transfer) => transfer.id === wait.transfer_id)?.name ?? '产物交接'}</strong><span>{wait.status === 'received' ? '已确认接收' : wait.active ? '等待接收' : '已结束等待'}</span></header>
            <p>第 {wait.generation} 次等待{wait.received_at && <> · <Time value={wait.received_at} /></>}</p>
          </article>)}
        </section>}
        {detail.transfers.length > 0 && <details className="task-detail-panel related-messages">
          <summary className="task-section-title">产物交接<span>{detail.transfers.length}</span></summary>
          <div className="transfer-list">{detail.transfers.map((transfer) => <article className="transfer-row" key={transfer.id}>
            <div className="transfer-file"><strong>{transfer.name}</strong><span>{transferKind(transfer.kind)} · {formatBytes(transfer.size)}</span></div>
            <div className="transfer-route"><span>{deviceNames.get(transfer.source_device_id) ?? transfer.source_device_id}</span><span aria-label="发送至">→</span><span>{deviceNames.get(transfer.target_device_id) ?? transfer.target_device_id}</span></div>
            <span className="transfer-state" data-state={transfer.status}>{transferStatus(transfer)}{transfer.attempts > 1 ? ` · ${transfer.attempts} 次尝试` : ''}</span>
            <code className="transfer-hash">SHA-256 {transfer.sha256 || '待确认'}</code>
            <Time value={transfer.received_at ?? transfer.created_at} />
            {transfer.last_error && <p className="transfer-error">{transfer.last_error}</p>}
          </article>)}</div>
        </details>}
        {relatedMessages && relatedMessages.length > 0 && <section className="task-detail-panel related-messages" aria-labelledby="related-heading"><h2 id="related-heading">关联对话</h2>{relatedMessages.map((message) => <MessageItem key={message.id} message={message} task={task} />)}</section>}
      </div>}
    </section>
  )
}

const tokenCount = new Intl.NumberFormat('zh-CN')
const runErrorText = {
  native_exit: 'CLI 执行失败', interrupted: '运行中断', output: '输出处理失败', setup: '启动准备失败',
}
const usageSourceText = { dsh_headless: 'dsh', codex_exec: 'Codex' }

/** 展示 CLI 已确认的本轮诊断。缺失用量单独标记，已知零值照常显示。 */
function RunDiagnostics({ diagnostics }: { diagnostics?: Run['diagnostics'] }) {
  const usage = diagnostics?.usage
  const executionFacts = [
    ['退出码', diagnostics?.exit_code],
    ['标准输出', diagnostics?.stdout_bytes == null ? undefined : formatBytes(diagnostics.stdout_bytes)],
    ['标准错误', diagnostics?.stderr_bytes == null ? undefined : formatBytes(diagnostics.stderr_bytes)],
  ] as const
  const usageFacts = usage ? [
    ['输入', usage.input_tokens],
    ['输出', usage.output_tokens],
    ['缓存读取', usage.cached_input_tokens],
    ['缓存写入', usage.cache_write_input_tokens],
    ['推理输出', usage.reasoning_output_tokens],
    ['总计', usage.total_tokens],
  ] as const : []
  const error = diagnostics?.error_kind && runErrorText[diagnostics.error_kind]

  return (
    <div className="run-diagnostics">
      {executionFacts.some(([, value]) => value != null) && <dl className="run-diagnostic-facts" aria-label="进程状态">
        {executionFacts.map(([label, value]) => value == null ? null : <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}
      </dl>}
      {(error || diagnostics?.capture_incomplete) && <p className="run-diagnostic-warning">
        {[error, diagnostics?.capture_incomplete && '输出采集不完整'].filter(Boolean).join(' · ')}
      </p>}
      {usage ? <dl className="run-diagnostic-facts" aria-label="Token 用量">
        <div><dt>Token 用量</dt><dd>{usageSourceText[usage.source]}</dd></div>
        {usageFacts.map(([label, value]) => value == null ? null : <div key={label}><dt>{label}</dt><dd>{tokenCount.format(value)}</dd></div>)}
      </dl> : <p>Token 用量未知</p>}
    </div>
  )
}

function latestTask(candidate?: Task, known?: Task): Task | undefined {
  if (!candidate || !known) return candidate ?? known
  // PostgreSQL 时间精确到微秒，补足 Date.parse 截断的小数位。
  const micros = (value: string) => (value.match(/\.(\d+)/u)?.[1] ?? '').padEnd(6, '0').slice(3, 6)
  const difference = Date.parse(candidate.updated_at) - Date.parse(known.updated_at)
  if (difference) return difference > 0 ? candidate : known
  return micros(candidate.updated_at) >= micros(known.updated_at) ? candidate : known
}

function formatBytes(size: number): string {
  if (size < 1_024) return `${size} B`
  const unit = size < 1_048_576 ? 'KiB' : 'MiB'
  const divisor = size < 1_048_576 ? 1_024 : 1_048_576
  return `${(size / divisor).toFixed(1)} ${unit}`
}

function agentName(kind?: string): string {
  return kind === 'codex' ? 'Codex' : kind || 'dsh'
}

function dependencyStatus(status: string): string {
  const labels: Record<string, string> = {
    waiting: '等待完成', delivering: '等待交付', completed: '已完成', failed: '已失败', interrupted: '待核对', superseded: '已替换',
  }
  return labels[status] ?? status
}

function transferKind(kind: string): string {
  const labels: Record<string, string> = { prompt: '任务输入', file: '文件', message: '消息', result: '任务结果' }
  return labels[kind] ?? kind
}

function transferStatus({ status, received_at }: Transfer): string {
  const labels: Record<string, string> = { pending: '待传输', sending: '正在传输', waiting_delivery: '等待接收', retrying: '重试中', received: '已确认接收', failed: '传输失败' }
  return labels[status] ?? (received_at ? '已确认接收' : '等待接收')
}
