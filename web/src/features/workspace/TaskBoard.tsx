import { useEffect, useRef, useState } from 'react'
import { AdminTokenError, HubRequestError, readDevices } from '../../api/devices'
import { latestApproval, readArtifact, readTask, type Approval, type Artifact, type InvocationDiagnostics, type Run, type RunReviewAttempt, type Task, type TaskDetail, type TaskInput, type Transfer } from '../../api/workspace'
import { messageFor } from '../../useWorkspace'
import { ApprovalCard, type ApprovalActions } from './ApprovalCard'
import { MessageItem, Time } from './Conversation'
import { RunLogs } from './RunLogs'
import { TaskControls, type TaskControlActions } from './TaskControls'

const statusText: Record<Task['status'] | Run['status'], string> = {
  queued: '排队', running: '运行中', waiting: '等待', waiting_dependencies: '等待依赖', waiting_delivery: '等待交付',
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
  prerequisites: '等待前置任务完成',
}

const taskInputText: Record<TaskInput['kind'], string> = {
  message: '主对话要求',
  delegation: '派发输入',
  dependency: '依赖续接',
  execution: '运行输入',
  approval: '请示答复',
  transfer: '交接文件',
}

const inputStatusText: Record<string, string> = {
  ...statusText,
  accepted: '已受理',
  handled: '已处理',
}

const inputReceiptText: Record<NonNullable<TaskInput['receipt_status']>, string> = {
  awaiting_delivery: '待送达',
  delivered: '已送达',
  handled: '已处理',
  needs_review: '待核对',
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
              {entries.map((task) => <TaskCard key={task.id} task={task} children={childrenByParent.get(task.id) ?? []} deviceNames={deviceNames} />)}
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

function TaskCard({ task, children, deviceNames }: { task: Task; children: Task[]; deviceNames: Map<string, string> }) {
  return (
    <article className="task-tree-card">
      <a href={`#board/${task.id}`} className="task-card">
        <h3>{task.title}</h3>
        <div className="task-card-meta">{task.project_name && <span>{task.project_name}</span>}<TaskStatus status={task.status} /></div>
        {task.waiting_reason && <p className="task-waiting-reason">{waitingReasonText[task.waiting_reason] ?? task.waiting_reason}</p>}
        {!task.center_group && <div className="task-card-facts"><span>{deviceNames.get(task.device_id) ?? task.device_id}</span><span>{agentName(task.agent_kind)}</span></div>}
        <div className="task-card-footer"><Time value={task.updated_at} />{task.report_pending && <span>待汇报</span>}</div>
      </a>
      {children.length > 0 && <details><summary className="tree-toggle">{children.length} 项子任务</summary><ChildTasks tasks={children} deviceNames={task.center_group ? deviceNames : undefined} /></details>}
    </article>
  )
}

function ChildTasks({ tasks, deviceNames }: { tasks: Task[]; deviceNames?: Map<string, string> }) {
  return <div className="child-task-list">{tasks.map((task) => (
    <a href={`#board/${task.id}`} className="child-task-row" key={task.id}>
      <span className="child-task-name">
        <span>{task.title}</span>
        {deviceNames && <small>{deviceNames.get(task.device_id) ?? task.device_id} · {agentName(task.agent_kind)}</small>}
      </span>
      <TaskStatus status={task.status} />
    </a>
  ))}</div>
}

type TaskDetailsProps = Required<Pick<TaskBoardProps, 'token' | 'taskID' | 'onFollow' | 'onLock'>> & Pick<TaskBoardProps, 'cursor' | 'tasks' | 'approvals' | 'approvalActions' | 'taskControlActions'> & {
  deviceNames: Map<string, string>
}

function TaskDetails({ token, cursor, tasks, approvals, approvalActions, taskControlActions, taskID, deviceNames, onFollow, onLock }: TaskDetailsProps) {
  const [detail, setDetail] = useState<TaskDetail>()
  const [runsOpen, setRunsOpen] = useState(false)
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
  const canFollow = detail && task && !error && !task.parent_task_id && !task.center_group && task.status !== 'cancelled' && task.status !== 'cancelling' && (
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
            {!task.center_group && <>
              <div><dt>项目</dt><dd>{task.project_name}</dd></div>
              <div><dt>设备</dt><dd>{deviceNames.get(task.device_id) ?? task.device_id}</dd></div>
              <div><dt>Agent</dt><dd>{agentName(task.agent_kind)}</dd></div>
            </>}
            <div><dt>更新时间</dt><dd><Time value={task.updated_at} /></dd></div>
          </dl>
          <TaskControls task={task} actions={taskControlActions} disabled={Boolean(error)} />
          {task.result && <div className="detail-result"><h2>结果</h2><p className="message-text">{task.result}</p></div>}
          <details className="task-technical">
            <summary>标识</summary>
            <dl className="task-facts">
              <div><dt>任务编号</dt><dd><code>{task.id}</code></dd></div>
              {task.binding_id && <div><dt>执行绑定</dt><dd><code>{task.binding_id}</code></dd></div>}
              {task.git_policy_revision != null && <div><dt>Git 策略版本</dt><dd>{task.git_policy_revision}</dd></div>}
            </dl>
          </details>
        </div>
        {!task.center_group && <details className="task-detail-panel" onToggle={(event) => setRunsOpen(event.currentTarget.open)}>
          <summary className="task-section-title">运行记录<span>{detail.runs.length}</span></summary>
          {detail.runs.length === 0 && <p className="standalone-note">等待首次运行。</p>}
          {detail.runs.map((run) => <article className="run-row" key={run.id}>
            <header><Time value={run.created_at} /><TaskStatus status={run.status} /></header>
            <p><span>运行</span> <code>{run.id}</code></p>
            {run.session_id && <p><span>会话</span> <code>{run.session_id}</code></p>}
            <GitRunEvidence verification={run.git_verification} publication={run.git_publication} />
            <RunDiagnostics diagnostics={run.diagnostics} />
            <RunReviewAttempts reviews={run.diagnostics?.reviews} token={token} taskID={taskID} runID={run.id}
              logsEnabled={runsOpen && run.status !== 'queued' && run.status !== 'running'} onLock={onLock} />
            {runsOpen && run.status !== 'queued' && run.status !== 'running' &&
              <RunLogs token={token} taskID={taskID} runID={run.id} onLock={onLock} />}
            {run.text && <p className="message-text">{run.text}</p>}
          </article>)}
        </details>}
        {detail.children.length > 0 && <section className="task-detail-panel" aria-labelledby="children-heading"><h2 id="children-heading">子任务</h2><ChildTasks tasks={detail.children} deviceNames={task.center_group ? deviceNames : undefined} /></section>}
        {task.depends_on_task_ids && task.depends_on_task_ids.length > 0 && <section className="task-detail-panel" aria-labelledby="prerequisites-heading">
          <h2 id="prerequisites-heading">前置任务</h2>
          <div className="child-task-list">{task.depends_on_task_ids.map((id) => {
            const prerequisite = tasks?.find((candidate) => candidate.id === id)
            return <a className="child-task-row" href={`#board/${id}`} key={id}>
              <span>{prerequisite?.title ?? id}</span>{prerequisite && <TaskStatus status={prerequisite.status} />}
            </a>
          })}</div>
        </section>}
        {detail.dependencies.length > 0 && <section className="task-detail-panel" aria-labelledby="dependencies-heading">
          <h2 id="dependencies-heading">依赖任务</h2>
          <div className="child-task-list">{detail.dependencies.map((dependency) => <a className="child-task-row" href={`#board/${dependency.depends_on_task_id}`} key={`${dependency.depends_on_task_id}/${dependency.generation}`}>
            <span>{taskName(dependency.depends_on_task_id)}</span><span>{dependencyStatus(dependency.status)} · 第 {dependency.generation} 次等待{dependency.active ? '（当前）' : ''}</span>
          </a>)}</div>
        </section>}
        {(detail.inputs.length > 0 || detail.waited_transfers.length > 0) && <section className="task-detail-panel" aria-labelledby="inputs-heading">
          <h2 id="inputs-heading">任务输入与等待</h2>
          {detail.inputs.map((input) => {
            const { receipt_status: receipt, receipt_run_id: receiptRunID, handled_at: handledAt } = input
            const transfer = input.kind === 'transfer' ? detail.transfers.find((item) => item.id === input.transfer_ids?.[0]) : undefined
            const generation = receiptRunID ? detail.runs.find((run) => run.id === receiptRunID)?.generation : input.generation
            return <article className="run-row task-input-row" key={input.id}>
              <header>
                <strong>{transfer?.name ?? taskInputText[input.kind]}</strong>
                {receipt ? <span className="task-status" data-input-receipt={receipt}>{inputReceiptText[receipt]}</span> : <span>{inputStatusText[input.status] ?? input.status}</span>}
              </header>
              <p><Time value={handledAt ?? input.created_at} />{generation ? ` · 执行第 ${generation} 轮` : ''}{input.kind !== 'transfer' && input.transfer_ids?.length ? ` · ${input.transfer_ids.length} 项交接` : ''}</p>
            </article>
          })}
          {detail.waited_transfers.map((wait) => <article className="run-row" key={`${wait.transfer_id}/${wait.generation}`}>
            <header><strong>{detail.transfers.find((transfer) => transfer.id === wait.transfer_id)?.name ?? '产物交接'}</strong><span>{wait.status === 'received' ? '已确认接收' : wait.active ? '等待接收' : '已结束等待'}</span></header>
            <p>第 {wait.generation} 次等待{wait.received_at && <> · <Time value={wait.received_at} /></>}</p>
          </article>)}
        </section>}
        {detail.artifacts.length > 0 && <TaskArtifacts token={token} artifacts={detail.artifacts} deviceNames={deviceNames} onLock={onLock} />}
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

const artifactErrors: Partial<Record<number, string>> = {
  404: '设备上找不到该产物。',
  409: '执行设备暂不可用，请确认设备在线。',
  429: '有一项产物正在下载，请稍后重试。',
  502: '设备无法读取产物，或文件未通过校验。',
  504: '设备读取超时，请重试。',
}

/**
 * 列出已登记产物，点击后以当前会话请求设备副本。离开详情时取消请求。
 * @param props 产物快照、设备名称和管理员会话凭据及锁定回调。
 * @returns 带下载操作和逐项错误反馈的产物列表。
 */
function TaskArtifacts({ token, artifacts, deviceNames, onLock }: {
  token: string
  artifacts: Artifact[]
  deviceNames: Map<string, string>
  onLock: (reason: string) => void
}) {
  const controllerRef = useRef<AbortController | null>(null)
  const [active, setActive] = useState('')
  const [failure, setFailure] = useState<{ id: string; message: string }>()
  useEffect(() => () => controllerRef.current?.abort(), [])

  async function download(artifact: Artifact) {
    if (controllerRef.current) return
    const controller = new AbortController()
    controllerRef.current = controller
    setActive(artifact.id)
    setFailure(undefined)
    try {
      const body = await readArtifact(token, artifact, controller.signal)
      if (controller.signal.aborted) return
      const url = URL.createObjectURL(body)
      const link = document.createElement('a')
      link.href = url
      link.download = artifact.path.split('/').at(-1) || 'artifact'
      link.click()
      window.setTimeout(() => URL.revokeObjectURL(url), 30_000)
    } catch (error) {
      if (controller.signal.aborted) return
      if (error instanceof AdminTokenError) onLock(error.message)
      else setFailure({ id: artifact.id, message: (error instanceof HubRequestError && artifactErrors[error.status]) || messageFor(error) })
    } finally {
      controllerRef.current = null
      if (!controller.signal.aborted) setActive('')
    }
  }

  return (
    <section className="task-detail-panel" aria-labelledby="artifact-heading">
      <h2 id="artifact-heading">任务产物</h2>
      <ul className="artifact-list">
        {artifacts.map((artifact) => (
          <li className="artifact-row" key={artifact.id}>
            <div className="artifact-meta">
              <strong>{artifact.path}</strong>
              <span>{formatBytes(artifact.size)} · {deviceNames.get(artifact.device_id) ?? artifact.device_id}</span>
              <Time value={artifact.created_at} />
            </div>
            <button type="button" className="outline-button" aria-label={`下载 ${artifact.path}`} disabled={Boolean(active)} onClick={() => void download(artifact)}>
              {active === artifact.id ? '下载中…' : '下载'}
            </button>
            {failure?.id === artifact.id && <p className="artifact-error" role="alert">{failure.message}</p>}
          </li>
        ))}
      </ul>
    </section>
  )
}

const gitTestStatus = { passed: '测试通过', failed: '测试失败', unverified: '未验证' }
const gitReviewStatus = { passed: '通过', changes_requested: '需返修', unverified: '未验证' }
const gitPublicationStatus = {
  pending: '待核对',
  same_commit: '已确认推送',
  contains_commit: '远端已包含',
  missing: '远端分支不存在',
  diverged: '远端历史分叉',
  unavailable: '远端暂不可读',
}
/**
 * 展示每轮独立 Review 的候选、执行 Agent、诊断和本次调用日志。
 * @param props Review 记录、关联运行与令牌、外层展开状态及会话锁定回调。
 * @returns Review 证据列表，没有记录时返回空值。
 */
function RunReviewAttempts({ reviews, token, taskID, runID, logsEnabled, onLock }: {
  reviews?: RunReviewAttempt[]
  token: string
  taskID: string
  runID: string
  logsEnabled: boolean
  onLock: (reason: string) => void
}) {
  if (!reviews?.length) return null
  return (
    <section className="run-review-attempts" aria-label="独立 Review">
      <h3>独立 Review</h3>
      {reviews.map((review) => (
        <article className="run-review-attempt" key={`${review.candidate_oid}/${review.repair_index}`}>
          <header>
            <strong data-status={review.status}>{gitReviewStatus[review.status] ?? gitReviewStatus.unverified}</strong>
            <span>{agentName(review.agent_kind)} · {review.repair_index === 0 ? '首审' : `返修 ${review.repair_index}`}</span>
          </header>
          <div className="run-review-candidate"><span>候选</span>
            <details>
              <summary><code>{review.candidate_oid.slice(0, 12)}</code></summary>
              <code>{review.candidate_oid}</code>
            </details>
          </div>
          <RunDiagnostics diagnostics={review.diagnostics} />
          {logsEnabled && review.log_invocation && <RunLogs key={review.log_invocation}
            token={token} taskID={taskID} runID={runID} invocation={review.log_invocation} onLock={onLock} />}
        </article>
      ))}
    </section>
  )
}

/** 在对应运行内展示设备报告的测试、Review 和远端读回证据。 */
function GitRunEvidence({ verification, publication }: { verification?: Run['git_verification']; publication?: Run['git_publication'] }) {
  if (!verification && !publication) return null
  return (
    <dl className="run-git-verification" aria-label="Git 交付证据">
      {verification && <>
        <div><dt>Git 验证</dt><dd data-status={verification.test_status}>
          {gitTestStatus[verification.test_status] ?? gitTestStatus.unverified}
          {verification.test_exit_code != null && <span> · 退出码 {verification.test_exit_code}</span>}
        </dd></div>
        {verification.base_oid && <div><dt>基准提交</dt><dd><code>{verification.base_oid}</code></dd></div>}
        {verification.candidate_oid && <div><dt>候选提交</dt><dd><code>{verification.candidate_oid}</code></dd></div>}
        {verification.test_log_ref && <div><dt>测试日志</dt><dd><code>{verification.test_log_ref}</code></dd></div>}
        {(verification.review_agent || verification.review_status) && <div><dt>Review</dt><dd data-status={verification.review_status}>
          {gitReviewStatus[verification.review_status ?? 'unverified'] ?? gitReviewStatus.unverified}
          {verification.review_agent && <span> · {agentName(verification.review_agent)}</span>}
        </dd></div>}
        {verification.repair_attempts != null && <div><dt>返修</dt><dd>{verification.repair_attempts} 次</dd></div>}
      </>}
      {publication && <>
        <div><dt>推送</dt><dd data-status={publication.status}>{gitPublicationStatus[publication.status] ?? gitPublicationStatus.pending}</dd></div>
        <div><dt>目标分支</dt><dd><code>{publication.remote} · {publication.target_ref}</code></dd></div>
        {publication.candidate_oid !== verification?.candidate_oid && <div><dt>推送提交</dt><dd><code>{publication.candidate_oid}</code></dd></div>}
        {publication.observed_oid && <div><dt>远端提交</dt><dd><code>{publication.observed_oid}</code></dd></div>}
      </>}
    </dl>
  )
}

const tokenCount = new Intl.NumberFormat('zh-CN')
const runErrorText = {
  native_exit: 'CLI 执行失败', interrupted: '运行中断', output: '输出处理失败', setup: '启动准备失败',
}
const usageSourceText = { dsh_headless: 'dsh', codex_exec: 'Codex' }

/** 展示本次 CLI 调用的诊断。缺失用量单独标记，已知零值照常显示。 */
function RunDiagnostics({ diagnostics }: { diagnostics?: InvocationDiagnostics }) {
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
