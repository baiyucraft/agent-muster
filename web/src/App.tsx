import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react'
import musterIcon from '../../ui/muster-icon.svg'
import { readAdminSession, signIn, signOut } from './api/auth'
import { AdminSessionError, HubRequestError } from './api/request'
import { answerApproval, approvalAnswerBytes, controlTask, decideBindingProposal, messageBytes, newID, sendMessage, withinTextLimit, type Task, type TaskControlRequest } from './api/workspace'
import { DeviceInventory } from './features/devices/DeviceInventory'
import type { ApprovalActions } from './features/workspace/ApprovalCard'
import type { SubmitBindingDecision } from './features/workspace/BindingProposalCard'
import { Conversation } from './features/workspace/Conversation'
import { DeviceUsage } from './features/workspace/DeviceUsage'
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
  { id: 'usage', label: '用量' },
] as const

type View = typeof views[number]['id']

type AdminSession =
  | { state: 'checking' | 'active' }
  | { state: 'signed-out', message?: string }
  | { state: 'unavailable', message: string }

function readRoute(): { view: View; taskID?: string } {
  const [page, taskID] = window.location.hash.slice(1).split('/')
  const view = views.find(({ id }) => id === page)?.id ?? 'conversation'
  return { view, taskID: view === 'board' ? taskID : undefined }
}

function Brand() {
  return <a href="#conversation" className="workspace-brand"><img className="brand-mark" src={musterIcon} alt="" />Agent Muster</a>
}

export function App() {
  const [session, setSession] = useState<AdminSession>({ state: 'checking' })
  const [checks, setChecks] = useState(0)
  const [signingIn, setSigningIn] = useState(false)
  const [signInError, setSignInError] = useState('')
  const [invalidCredentials, setInvalidCredentials] = useState(false)
  const loginRequest = useRef<AbortController | null>(null)
  const lock = useCallback((message?: string) => {
    setSession({ state: 'signed-out', message })
  }, [])

  useEffect(() => {
    const controller = new AbortController()
    const { signal } = controller
    void readAdminSession(signal).then((active) => {
      if (!signal.aborted) setSession({ state: active ? 'active' : 'signed-out' })
    }).catch((failure: unknown) => {
      if (!signal.aborted) setSession({ state: 'unavailable', message: messageFor(failure) })
    })
    return () => controller.abort()
  }, [checks])
  useEffect(() => () => loginRequest.current?.abort(), [])

  async function submitLogin(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = event.currentTarget
    if (loginRequest.current) return
    const fields = new FormData(form)
    const username = fields.get('username')
    const password = fields.get('password')
    if (typeof username !== 'string' || typeof password !== 'string') return

    const controller = new AbortController()
    loginRequest.current = controller
    setSigningIn(true)
    setInvalidCredentials(false)
    setSignInError('')
    lock()
    try {
      await signIn(username.trim(), password, controller.signal)
      if (!controller.signal.aborted) setSession({ state: 'active' })
    } catch (failure) {
      if (controller.signal.aborted) return
      setInvalidCredentials(failure instanceof AdminSessionError)
      setSignInError(messageFor(failure))
    } finally {
      loginRequest.current = null
      if (!controller.signal.aborted) setSigningIn(false)
    }
  }

  if (session.state === 'active') return <WorkspaceSession onLock={lock} />

  const loginMessage = signInError || (session.state === 'signed-out' ? session.message : '')

  return (
    <div className="workspace-login">
      <header><Brand /></header>
      <main className="workspace-login-main">
        <h1>进入工作台</h1>
        {session.state === 'checking' && <p className="login-status" role="status">正在检查登录状态…</p>}
        {session.state === 'unavailable' && <div className="login-form">
          <p className="auth-error" role="alert">{session.message}</p>
          <button type="button" className="primary-button" onClick={() => {
            setSession({ state: 'checking' })
            setChecks((previous) => previous + 1)
          }}>重试</button>
        </div>}
        {session.state === 'signed-out' && <form className="login-form" method="post" onSubmit={submitLogin} aria-busy={signingIn}>
          <label className="login-field">
            用户名
            <input name="username" autoComplete="username" autoCapitalize="none" spellCheck={false} readOnly={signingIn} aria-invalid={invalidCredentials || undefined} aria-describedby={invalidCredentials ? 'auth-error' : undefined} required />
          </label>
          <label className="login-field">
            密码
            <input name="password" type="password" autoComplete="current-password" readOnly={signingIn} aria-invalid={invalidCredentials || undefined} aria-describedby={invalidCredentials ? 'auth-error' : undefined} required />
          </label>
          {loginMessage && <p id="auth-error" className="auth-error" role="alert">{loginMessage}</p>}
          <button type="submit" className="primary-button" disabled={signingIn}>{signingIn ? '登录中…' : '登录'}</button>
        </form>}
      </main>
    </div>
  )
}

function WorkspaceSession({ onLock }: { onLock: (reason?: string) => void }) {
  const { workspace, streamState, error, refresh, acceptMessage, acceptApproval, acceptBindingProposal } = useWorkspace(onLock)
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
  const logoutRequest = useRef<AbortController | null>(null)
  const [signingOut, setSigningOut] = useState(false)
  const [signOutError, setSignOutError] = useState('')

  useEffect(() => {
    const routeChanged = () => setRoute(readRoute())
    window.addEventListener('hashchange', routeChanged)
    return () => window.removeEventListener('hashchange', routeChanged)
  }, [])
  useEffect(() => () => sendingRequest.current?.abort(), [])
  useEffect(() => () => logoutRequest.current?.abort(), [])
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

  async function logout() {
    if (logoutRequest.current) return
    const controller = new AbortController()
    logoutRequest.current = controller
    setSigningOut(true)
    setSignOutError('')
    try {
      await signOut(controller.signal)
      if (!controller.signal.aborted) onLock()
    } catch (failure) {
      if (!controller.signal.aborted) setSignOutError(`退出登录失败。${messageFor(failure)}`)
    } finally {
      logoutRequest.current = null
      if (!controller.signal.aborted) setSigningOut(false)
    }
  }

  async function submitApproval(id: string, value: string) {
    const answer = value.trim()
    const requests = approvalRequests.current
    if (!answer || !withinTextLimit(answer, approvalAnswerBytes) || requests.has(id)) return
    const controller = new AbortController()
    requests.set(id, controller)
    setApprovalSubmissions((current) => new Map(current).set(id, { pending: true, error: '' }))
    let submissionError = ''
    try {
      const approval = await answerApproval(id, answer, controller.signal)
      if (!controller.signal.aborted) acceptApproval(approval)
    } catch (failure) {
      if (controller.signal.aborted) return
      if (failure instanceof AdminSessionError) onLock(failure.message)
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
      const message = await sendMessage(submission, controller.signal)
      if (controller.signal.aborted) return
      acceptMessage(message)
      setDraft('')
      setFollowTask(undefined)
      lastSubmission.current = undefined
    } catch (failure) {
      if (controller.signal.aborted) return
      if (failure instanceof AdminSessionError) onLock(failure.message)
      else setSendError(messageFor(failure))
    } finally {
      sendingRequest.current = null
      if (!controller.signal.aborted) setSending(false)
    }
  }

  const submitBindingDecision: SubmitBindingDecision = async (id, decision, signal) => {
    try {
      const proposal = await decideBindingProposal(id, decision, signal)
      if (!signal.aborted) acceptBindingProposal(proposal)
    } catch (failure) {
      if (signal.aborted) return
      if (failure instanceof AdminSessionError) {
        onLock(failure.message)
        return
      }
      if (failure instanceof HubRequestError && failure.status === 409) refresh()
      throw failure
    }
  }

  async function submitTaskControl(id: string, input: TaskControlRequest) {
    const requests = taskControlRequests.current
    if (requests.has(id)) return
    const controller = new AbortController()
    requests.set(id, controller)
    setTaskControlSubmissions((current) => new Map(current).set(id, { pending: true, input }))
    try {
      const result = await controlTask(id, input, controller.signal)
      if (controller.signal.aborted) return
      setTaskControlSubmissions((current) => new Map(current).set(id, { pending: false, input, result }))
      refresh()
    } catch (failure) {
      if (controller.signal.aborted) return
      if (failure instanceof AdminSessionError) {
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
          <button type="button" className="quiet-button" onClick={() => void logout()} disabled={signingOut}>{signingOut ? '退出中…' : '退出登录'}</button>
        </div>
      </header>

      <main className="workspace-content" id="workspace-content">
        {signOutError && <p className="read-error" role="alert">{signOutError}</p>}
        {route.view !== 'devices' && route.view !== 'usage' && error && <p className="read-error" role="status">{error}{workspace && ' 当前显示上次读取的状态。'}</p>}
        {route.view === 'devices' && <DeviceInventory onLock={onLock} />}
        {route.view === 'conversation' && (
          <Conversation
            messages={workspace?.messages}
            tasks={workspace?.tasks ?? []}
            approvals={workspace?.approvals ?? []}
            approvalActions={approvalActions}
            bindingProposals={workspace?.binding_proposals ?? []}
            onBindingDecision={submitBindingDecision}
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
        {route.view === 'board' && <TaskBoard tasks={workspace?.tasks} approvals={workspace?.approvals ?? []} approvalActions={approvalActions} taskControlActions={taskControlActions} cursor={workspace?.cursor} taskID={route.taskID} onFollow={attachTask} onLock={onLock} onRefresh={refresh} />}
        {route.view === 'projects' && <Projects workspace={workspace} onSaved={refresh} onLock={onLock} />}
        {route.view === 'usage' && <DeviceUsage onLock={onLock} />}
      </main>
    </div>
  )
}
