import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react'
import musterIcon from '../../ui/muster-icon.svg'
import { AdminTokenError, HubRequestError } from './api/devices'
import { answerApproval, approvalAnswerBytes, controlTask, messageBytes, newID, sendMessage, withinTextLimit, type Task, type TaskControlRequest } from './api/workspace'
import { DeviceInventory } from './features/devices/DeviceInventory'
import type { ApprovalActions } from './features/workspace/ApprovalCard'
import { Conversation } from './features/workspace/Conversation'
import { Projects } from './features/workspace/Projects'
import { TaskBoard } from './features/workspace/TaskBoard'
import type { TaskControlActions } from './features/workspace/TaskControls'
import { messageFor, useWorkspace } from './useWorkspace'
import './workspace.css'

const views = [
  { id: 'conversation', label: '对话' },
  { id: 'board', label: '看板' },
  { id: 'devices', label: '设备' },
  { id: 'projects', label: '项目' },
] as const

type View = typeof views[number]['id']

function readRoute(): { view: View; taskID?: string } {
  const [page, taskID] = window.location.hash.slice(1).split('/')
  const view = views.find(({ id }) => id === page)?.id ?? 'conversation'
  return { view, taskID: view === 'board' ? taskID : undefined }
}

function Brand() {
  return <a href="#conversation" className="workspace-brand"><img className="brand-mark" src={musterIcon} alt="" />Agent Muster</a>
}

export function App() {
  const [token, setToken] = useState('')
  const [draft, setDraft] = useState('')
  const [authError, setAuthError] = useState('')
  const lock = useCallback((reason = '') => {
    setToken('')
    setAuthError(reason)
  }, [])

  function unlock(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const candidate = draft.trim()
    if (!candidate) return
    setToken(candidate)
    setDraft('')
    setAuthError('')
  }

  if (token) return <WorkspaceSession token={token} onLock={lock} />

  return (
    <div className="workspace-login">
      <header><Brand /></header>
      <main className="workspace-login-main">
        <h1>进入工作台</h1>
        <form className="token-form" onSubmit={unlock}>
          <label htmlFor="admin-token">管理员令牌</label>
          <input id="admin-token" type="password" value={draft} onChange={(event) => setDraft(event.target.value)} spellCheck={false} autoComplete="off" aria-invalid={authError ? true : undefined} aria-describedby={authError ? 'auth-error' : undefined} required />
          {authError && <p id="auth-error" className="auth-error" role="alert">{authError}</p>}
          <button type="submit" className="primary-button">解锁工作台</button>
        </form>
      </main>
    </div>
  )
}

function WorkspaceSession({ token, onLock }: { token: string; onLock: (reason?: string) => void }) {
  const { workspace, streamState, error, refresh, acceptMessage, acceptApproval } = useWorkspace(token, onLock)
  const [route, setRoute] = useState(readRoute)
  const [draft, setDraft] = useState('')
  const [followTask, setFollowTask] = useState<Task>()
  const [sending, setSending] = useState(false)
  const [sendError, setSendError] = useState('')
  const sendingRequest = useRef<AbortController | null>(null)
  const lastSubmission = useRef<{ id: string; text: string; task_id?: string } | undefined>(undefined)
  const approvalRequests = useRef(new Map<string, AbortController>())
  const [approvalSubmissions, setApprovalSubmissions] = useState<ApprovalActions['submissions']>(() => new Map())
  const taskControlRequests = useRef(new Map<string, AbortController>())
  const [taskControlSubmissions, setTaskControlSubmissions] = useState<TaskControlActions['submissions']>(() => new Map())

  useEffect(() => {
    const routeChanged = () => setRoute(readRoute())
    window.addEventListener('hashchange', routeChanged)
    return () => window.removeEventListener('hashchange', routeChanged)
  }, [])
  useEffect(() => () => sendingRequest.current?.abort(), [])
  useEffect(() => {
    const approvals = approvalRequests.current
    const controls = taskControlRequests.current
    return () => {
      for (const controller of approvals.values()) controller.abort()
      for (const controller of controls.values()) controller.abort()
      approvals.clear()
      controls.clear()
    }
  }, [])

  async function submitApproval(id: string, value: string) {
    const answer = value.trim()
    const requests = approvalRequests.current
    if (!answer || !withinTextLimit(answer, approvalAnswerBytes) || requests.has(id)) return
    const controller = new AbortController()
    requests.set(id, controller)
    setApprovalSubmissions((current) => new Map(current).set(id, { pending: true, error: '' }))
    let submissionError = ''
    try {
      const approval = await answerApproval(token, id, answer, controller.signal)
      if (!controller.signal.aborted) acceptApproval(approval)
    } catch (failure) {
      if (controller.signal.aborted) return
      if (failure instanceof AdminTokenError) onLock(failure.message)
      else submissionError = messageFor(failure)
    } finally {
      requests.delete(id)
      if (!controller.signal.aborted) {
        setApprovalSubmissions((current) => {
          const next = new Map(current)
          if (submissionError) next.set(id, { pending: false, error: submissionError })
          else next.delete(id)
          return next
        })
      }
    }
  }

  async function submitMessage(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const text = draft.trim()
    if (!text || !withinTextLimit(text, messageBytes) || sendingRequest.current) return
    const task_id = followTask?.id
    const previous = lastSubmission.current
    const submission = previous?.text === text && previous.task_id === task_id
      ? previous
      : { id: newID(), text, task_id }
    lastSubmission.current = submission
    const controller = new AbortController()
    sendingRequest.current = controller
    setSending(true)
    setSendError('')
    try {
      const message = await sendMessage(token, submission, controller.signal)
      if (controller.signal.aborted) return
      acceptMessage(message)
      setDraft('')
      setFollowTask(undefined)
      lastSubmission.current = undefined
    } catch (failure) {
      if (controller.signal.aborted) return
      if (failure instanceof AdminTokenError) onLock(failure.message)
      else setSendError(messageFor(failure))
    } finally {
      sendingRequest.current = null
      if (!controller.signal.aborted) setSending(false)
    }
  }

  async function submitTaskControl(id: string, input: TaskControlRequest) {
    const requests = taskControlRequests.current
    if (requests.has(id)) return
    const controller = new AbortController()
    requests.set(id, controller)
    setTaskControlSubmissions((current) => new Map(current).set(id, { pending: true, input }))
    try {
      const result = await controlTask(token, id, input, controller.signal)
      if (controller.signal.aborted) return
      setTaskControlSubmissions((current) => new Map(current).set(id, { pending: false, input, result }))
      refresh()
    } catch (failure) {
      if (controller.signal.aborted) return
      if (failure instanceof AdminTokenError) {
        onLock(failure.message)
        return
      }
      const conflict = failure instanceof HubRequestError && failure.status === 409
      const retryable = !(failure instanceof HubRequestError) || failure.status >= 500
      const error = conflict ? '任务状态已变化，正在更新可用操作。' : messageFor(failure)
      setTaskControlSubmissions((current) => new Map(current).set(id, { pending: false, input, error, retryable }))
      refresh()
    } finally {
      requests.delete(id)
    }
  }

  function attachTask(task: Task) {
    if (sendingRequest.current) return
    setFollowTask(task)
    setSendError('')
    window.location.hash = 'conversation'
  }

  const streamLabel = { loading: '连接中', live: '实时更新', retry: '正在重连', unavailable: '服务暂不可用' }[streamState]
  const approvalActions: ApprovalActions = { submissions: approvalSubmissions, onAnswer: submitApproval }
  const taskControlActions: TaskControlActions = { submissions: taskControlSubmissions, onSubmit: submitTaskControl }

  return (
    <div className="workspace-shell" data-view={route.view}>
      <header className="workspace-bar">
        <Brand />
        <nav className="workspace-navigation" aria-label="主导航">
          {views.map(({ id, label }) => <a key={id} href={`#${id}`} aria-current={route.view === id ? 'page' : undefined}>{label}</a>)}
        </nav>
        <div className="workspace-actions">
          <span className="stream-indicator" data-state={streamState} role="status" title={streamLabel}><span>{streamLabel}</span></span>
          <button type="button" className="quiet-button" onClick={() => onLock()}>锁定工作台</button>
        </div>
      </header>

      <main className="workspace-content" id="workspace-content">
        {route.view !== 'devices' && error && <p className="read-error" role="status">{error}{workspace && ' 当前显示上次读取的状态。'}</p>}
        {route.view === 'devices' && <DeviceInventory token={token} onLock={onLock} />}
        {route.view === 'conversation' && (
          <Conversation
            messages={workspace?.messages}
            tasks={workspace?.tasks ?? []}
            approvals={workspace?.approvals ?? []}
            approvalActions={approvalActions}
            canSend={workspace !== null && streamState !== 'unavailable'}
            draft={draft}
            followTask={followTask}
            sending={sending}
            sendError={sendError}
            onDraftChange={setDraft}
            onClearFollowTask={() => setFollowTask(undefined)}
            onSubmit={submitMessage}
          />
        )}
        {route.view === 'board' && <TaskBoard token={token} tasks={workspace?.tasks} approvals={workspace?.approvals ?? []} approvalActions={approvalActions} taskControlActions={taskControlActions} cursor={workspace?.cursor} taskID={route.taskID} onFollow={attachTask} onLock={onLock} />}
        {route.view === 'projects' && <Projects token={token} workspace={workspace} onSaved={refresh} onLock={onLock} />}
      </main>
    </div>
  )
}
