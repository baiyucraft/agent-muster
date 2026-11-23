import { useRef } from 'react'
import { newID, type Task, type TaskControlAction, type TaskControlRequest } from '../../api/workspace'

type TaskControlSubmission = {
  pending: boolean
  input: TaskControlRequest
  result?: Task
  error?: string
  retryable?: boolean
}

/** 当前管理员会话内的任务操作。每项任务最多有一个正在提交的请求。 */
export type TaskControlActions = {
  submissions: ReadonlyMap<string, TaskControlSubmission>
  onSubmit: (taskID: string, input: TaskControlRequest) => Promise<void>
}

const labels: Record<TaskControlAction, string> = {
  pause: '暂停任务',
  stop_current: '停止本轮',
  resume: '恢复任务',
  cancel: '取消任务',
}
const actionOrder: TaskControlAction[] = ['pause', 'stop_current', 'resume', 'cancel']

/** 按 Hub 确认的可用操作渲染控件，重试沿用首次提交的请求 ID。 */
export function TaskControls({ task, actions, disabled }: { task: Task; actions: TaskControlActions; disabled: boolean }) {
  const confirmation = useRef<HTMLDialogElement>(null)
  const submission = actions.submissions.get(task.id)
  const available = actionOrder.filter((action) => task.control_actions?.includes(action))
  const busy = submission?.pending ?? false
  const canCancel = available.includes('cancel')

  function submit(action: TaskControlAction) {
    if (busy || disabled || !available.includes(action)) return
    void actions.onSubmit(task.id, { action, request_id: newID() })
  }

  if (!available.length && !busy && !submission?.error) return null

  return (
    <div className="task-controls">
      {available.length > 0 && (
        <div className="task-control-buttons" role="group" aria-label="任务操作" aria-busy={busy}>
          {available.map((action) => (
            <button key={action} type="button" className={action === 'cancel' ? 'revoke-button' : action === 'resume' ? 'primary-button' : 'outline-button'}
              disabled={disabled || busy} onClick={() => action === 'cancel' ? confirmation.current?.showModal() : submit(action)}>
              {labels[action]}
            </button>
          ))}
        </div>
      )}
      {submission?.pending && <p className="task-control-feedback" role="status">正在提交「{labels[submission.input.action]}」…</p>}
      {submission?.error && (
        <div className="task-control-feedback" data-error="true">
          <p role="alert">「{labels[submission.input.action]}」{submission.retryable ? '未收到确认。' : '未受理。'}{submission.error}</p>
          {submission.retryable && <button type="button" className="outline-button" disabled={busy}
            onClick={() => void actions.onSubmit(task.id, submission.input)}>重试</button>}
        </div>
      )}
      {canCancel && (
        <dialog ref={confirmation} className="task-cancel-dialog" aria-labelledby="task-cancel-heading" aria-describedby="task-cancel-impact">
          <h2 id="task-cancel-heading">取消任务</h2>
          <p id="task-cancel-impact">取消后将停止安排「{task.title}」及其子任务的后续执行，已开始的轮次需等设备确认退出。此操作无法恢复。</p>
          <div className="task-control-buttons">
            <button type="button" className="outline-button" autoFocus onClick={() => confirmation.current?.close()}>返回</button>
            <button type="button" className="revoke-button" disabled={disabled || busy} onClick={() => {
              confirmation.current?.close()
              submit('cancel')
            }}>确认取消任务</button>
          </div>
        </dialog>
      )}
    </div>
  )
}
