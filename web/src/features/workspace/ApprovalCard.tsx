import { useId, useState, type FormEvent } from 'react'
import { approvalAnswerBytes, withinTextLimit, type Approval, type Task } from '../../api/workspace'
import './approval.css'

/** 同一管理员会话内各页面共用的请示提交状态与操作。 */
export type ApprovalActions = {
  /** 按请示 ID 保存本会话提交中的请求与最近一次错误。 */
  submissions: ReadonlyMap<string, { pending: boolean; error: string }>
  /** 等到当前答复提交结束并更新工作区。 */
  onAnswer: (id: string, answer: string) => Promise<void>
}

const statusText: Record<Approval['status'], string> = {
  pending: '待答复',
  answered: '已答复',
  handled: '已处理',
  cancelled: '已取消',
}

/**
 * 显示可答复的请示或 Hub 已确认的答复。
 * @param props 请示记录、关联任务及当前会话的提交操作。
 * @returns 供主对话、看板和任务详情复用的请示卡片。
 */
export function ApprovalCard({ approval, task, actions }: { approval: Approval; task?: Task; actions: ApprovalActions }) {
  const id = useId()
  const [draft, setDraft] = useState('')
  const submission = actions.submissions.get(approval.id)
  const invalidDraft = !withinTextLimit(draft, approvalAnswerBytes)
  const pending = submission?.pending ?? false
  const status = approval.status === 'answered' && approval.waiting_device
    ? '已答复，等待设备上线'
    : statusText[approval.status]

  function sendDraft(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!draft.trim() || invalidDraft) return
    void actions.onAnswer(approval.id, draft)
  }

  return (
    <article className="approval-card" data-status={approval.status} aria-labelledby={`${id}-question`}>
      <header className="approval-meta">
        <span className="approval-status" aria-live="polite">{status}</span>
        {task && <a className="text-link approval-task" href={`#board/${task.id}`}>{task.title}</a>}
      </header>
      <p id={`${id}-question`} className="approval-question">{approval.question}</p>
      {approval.impact && <p className="approval-impact">{approval.impact}</p>}
      {approval.status === 'pending' ? (
        <form className="approval-form" onSubmit={sendDraft} aria-busy={pending}>
          {approval.options?.length > 0 && (
            <div className="approval-options" role="group" aria-label="答复选项">
              {approval.options.map((option) => (
                <button key={option} className="outline-button" type="button" disabled={pending} onClick={() => void actions.onAnswer(approval.id, option)}>{option}</button>
              ))}
            </div>
          )}
          <div className="approval-entry">
            <label className="screen-reader" htmlFor={`${id}-answer`}>答复请示</label>
            <textarea id={`${id}-answer`} value={draft} rows={2} placeholder="填写答复…" onChange={(event) => setDraft(event.target.value)} disabled={pending} aria-invalid={invalidDraft || undefined} aria-describedby={invalidDraft ? `${id}-limit` : undefined} />
            <button className="primary-button" type="submit" disabled={pending || !draft.trim() || invalidDraft}>{pending ? '正在提交…' : '提交答复'}</button>
          </div>
          {invalidDraft && <p id={`${id}-limit`} className="approval-error" role="status">答复最多 {approvalAnswerBytes} 个 UTF-8 字节，且不能包含空字符。</p>}
          {submission?.error && <p className="approval-error" role="alert">{submission.error}</p>}
        </form>
      ) : approval.answer && (
        <div className="approval-answer" role="status">
          <p>{approval.answer}</p>
        </div>
      )}
    </article>
  )
}
