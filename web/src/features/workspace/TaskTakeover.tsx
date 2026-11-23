import { useEffect, useId, useRef, useState, type FormEvent } from 'react'
import { AdminTokenError, HubRequestError } from '../../api/devices'
import { newID, takeoverSummaryBytes, takeoverTask, withinTextLimit, type TaskTakeoverRequest } from '../../api/workspace'
import { messageFor } from '../../useWorkspace'

type TaskTakeoverProps = {
  token: string
  taskID: string
  allowed: boolean
  onRefresh: () => void
  onLock: (reason: string) => void
}

/**
 * 在原会话不可用时，用管理员确认的摘要另开任务。失败后保留输入及提交编号。
 * @param props 已认证的原任务、接手许可及快照更新回调。
 * @returns 会话提示和按 Hub 当前许可显示的接手操作。
 */
export function TaskTakeover({ token, taskID, allowed, onRefresh, onLock }: TaskTakeoverProps) {
  const dialog = useRef<HTMLDialogElement>(null)
  const summaryField = useRef<HTMLTextAreaElement>(null)
  const request = useRef<AbortController | null>(null)
  const lastSubmission = useRef<TaskTakeoverRequest | undefined>(undefined)
  const [summary, setSummary] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState('')
  const titleID = useId()
  const summaryID = useId()
  const invalidSummary = !withinTextLimit(summary, takeoverSummaryBytes)
  useEffect(() => () => request.current?.abort(), [])

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const text = summary.trim()
    if (!allowed || request.current || invalidSummary || !text) return
    const submission = lastSubmission.current?.summary === text ? lastSubmission.current : { request_id: newID(), summary: text }
    lastSubmission.current = submission
    const controller = new AbortController()
    request.current = controller
    setSubmitting(true)
    setError('')
    try {
      const next = await takeoverTask(token, taskID, submission, controller.signal)
      if (controller.signal.aborted) return
      dialog.current?.close()
      window.location.hash = `board/${next.id}`
      onRefresh()
    } catch (failure) {
      if (controller.signal.aborted) return
      if (failure instanceof AdminTokenError) {
        onLock(failure.message)
      } else if (failure instanceof HubRequestError && failure.status === 409) {
        setError('接手请求未获确认，已请求刷新任务状态。')
        onRefresh()
      } else {
        setError(messageFor(failure))
      }
    } finally {
      request.current = null
      if (!controller.signal.aborted) setSubmitting(false)
    }
  }

  return (
    <div className="task-session-warning">
      <p>原会话不可用</p>
      {allowed && <button type="button" className="outline-button" onClick={() => {
        dialog.current?.showModal()
        summaryField.current?.focus()
      }}>重新接手</button>}
      <dialog ref={dialog} className="task-takeover-dialog" aria-labelledby={titleID} aria-describedby={`${titleID}-impact`} onCancel={(event) => {
        if (request.current) event.preventDefault()
      }}>
        <h2 id={titleID}>重新接手</h2>
        <p id={`${titleID}-impact`}>使用摘要创建任务，并启动新的 Agent 会话。</p>
        <form onSubmit={(event) => void submit(event)} aria-busy={submitting}>
          <label htmlFor={summaryID}>新任务摘要</label>
          <textarea ref={summaryField} id={summaryID} value={summary} rows={5} onChange={(event) => setSummary(event.target.value)} readOnly={submitting}
            aria-invalid={invalidSummary || undefined} aria-describedby={invalidSummary ? `${summaryID}-limit` : undefined} required />
          {invalidSummary && <p id={`${summaryID}-limit`} className="approval-error" role="status">摘要最多 {takeoverSummaryBytes} 个 UTF-8 字节，且不能包含空字符。</p>}
          {error && <p className="approval-error" role="alert">{error}</p>}
          {!allowed && <p className="approval-error" role="status">当前无法重新接手此任务。</p>}
          <div className="task-control-buttons">
            <button type="button" className="outline-button" onClick={() => dialog.current?.close()} disabled={submitting}>返回</button>
            <button type="submit" className="primary-button" disabled={!allowed || submitting || !summary.trim() || invalidSummary}>{submitting ? '正在创建…' : '确认并创建新任务'}</button>
          </div>
        </form>
      </dialog>
    </div>
  )
}
