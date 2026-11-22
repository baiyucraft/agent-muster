import { useEffect, useRef, type FormEvent } from 'react'
import musterIcon from '../../../../ui/muster-icon.svg'
import { messageBytes, withinTextLimit, type Message, type Task } from '../../api/workspace'

const messageLabel = { user: '你', reply: '中心助手', report: '任务汇报' }
const messageStatus = { accepted: '已受理', handled: '', interrupted: '处理已中断' }
const timestamp = new Intl.DateTimeFormat('zh-CN', { dateStyle: 'short', timeStyle: 'short' })

export function Time({ value }: { value: string }) {
  return <time dateTime={value}>{timestamp.format(new Date(value))}</time>
}

export function MessageItem({ message, task }: { message: Message; task?: Task }) {
  const status = message.status && messageStatus[message.status]
  return (
    <article className="message" data-kind={message.kind}>
      {message.kind === 'user'
        ? <div className="message-mark" aria-hidden="true">你</div>
        : <img className="message-mark" src={musterIcon} alt="" />}
      <div className="message-content">
        <div className="message-meta">
          <strong>{messageLabel[message.kind]}</strong>
          <Time value={message.created_at} />
          {status && <span className="message-status" data-status={message.status}>{status}</span>}
        </div>
        <p className="message-text">{message.text}</p>
        {message.task_id && <a className="task-link" href={`#board/${message.task_id}`}>{task?.title ?? '查看关联任务'} <span aria-hidden="true">↗</span></a>}
      </div>
    </article>
  )
}

type ConversationProps = {
  messages?: Message[]
  tasks: Task[]
  canSend: boolean
  draft: string
  followTask?: Task
  sending: boolean
  sendError: string
  onDraftChange: (value: string) => void
  onClearFollowTask: () => void
  onSubmit: (event: FormEvent<HTMLFormElement>) => void
}

export function Conversation({ messages, tasks, canSend, draft, followTask, sending, sendError, onDraftChange, onClearFollowTask, onSubmit }: ConversationProps) {
  const composer = useRef<HTMLTextAreaElement>(null)
  const thread = useRef<HTMLDivElement>(null)
  const followLatest = useRef(true)
  const tasksByID = new Map(tasks.map((task) => [task.id, task]))
  const validDraft = withinTextLimit(draft, messageBytes)

  useEffect(() => {
    if (followTask) composer.current?.focus()
  }, [followTask])

  useEffect(() => {
    if (followLatest.current && thread.current) thread.current.scrollTop = thread.current.scrollHeight
  }, [messages])

  return (
    <section className="conversation-view" aria-labelledby="conversation-heading">
      <div className="view-heading"><h1 id="conversation-heading">主对话</h1></div>
      <div className="conversation-panel">
        <div className="conversation-thread" ref={thread} role="log" aria-label="主对话消息" tabIndex={0} onScroll={(event) => {
          const viewer = event.currentTarget
          followLatest.current = viewer.scrollHeight - viewer.scrollTop - viewer.clientHeight < 72
        }}>
          {!messages && <p className="panel-placeholder">正在读取对话…</p>}
          {messages?.length === 0 && <div className="conversation-empty"><img src={musterIcon} alt="" /><h2>交办第一件事</h2></div>}
          {messages?.map((message) => <MessageItem key={message.id} message={message} task={message.task_id ? tasksByID.get(message.task_id) : undefined} />)}
        </div>

        <form className="composer" onSubmit={onSubmit}>
          {followTask && <div className="composer-context"><span>追加要求 <strong>{followTask.title}</strong></span><button type="button" className="quiet-button" disabled={sending} onClick={onClearFollowTask}>取消关联</button></div>}
          <label htmlFor="message-text" className="screen-reader">{followTask ? '追加要求' : '发给中心助手'}</label>
          <textarea id="message-text" ref={composer} value={draft} onChange={(event) => onDraftChange(event.target.value)} placeholder={followTask ? '补充这项任务的要求…' : '发给中心助手…'} maxLength={messageBytes} rows={3} disabled={sending} aria-invalid={!validDraft} aria-describedby={!validDraft ? 'message-length-error' : undefined} required />
          {!validDraft && <p id="message-length-error" className="read-error" role="status">消息最多 {messageBytes} 个 UTF-8 字节，且不能包含空字符。</p>}
          {sendError && <p className="read-error" role="alert">{sendError}</p>}
          <div className="composer-footer"><button className="primary-button" disabled={!canSend || sending || !draft.trim() || !validDraft} type="submit">{sending ? '正在发送…' : '发送消息'}<span aria-hidden="true">↑</span></button></div>
        </form>
      </div>
    </section>
  )
}
