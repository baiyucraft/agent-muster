import { useEffect, useState } from 'react'
import { AdminSessionError, HubRequestError, request } from '../../api/request'
import type { RunReviewAttempt } from '../../api/workspace'
import { messageFor } from '../../useWorkspace'
import { Time } from './Conversation'

type LogFile = {
  invocation: string
  stream: 'stdout' | 'stderr'
  text: string
  truncated: boolean
  updated_at?: string
}

type LogResult = { status: 'available' | 'missing' | 'failed'; files?: LogFile[] }

const requestErrors: Record<number, string> = {
  404: '此运行的设备日志不可用。',
  409: '此运行暂时无法读取日志。',
  429: '正在读取的日志过多，请稍后重试。',
  504: '设备未在限时内回应，请确认设备在线并重试。',
}

/**
 * 按需读取已结束运行的设备日志，收起时释放正文。
 * @param props 任务和运行标识、可选的 Review 调用引用及会话锁定回调。
 * @returns 按需展开的设备日志与读取错误。
 */
export function RunLogs({ taskID, runID, invocation, onLock }: {
  taskID: string
  runID: string
  invocation?: RunReviewAttempt['log_invocation']
  onLock: (reason: string) => void
}) {
  const [open, setOpen] = useState(false)
  const [result, setResult] = useState<LogResult>()
  const [error, setError] = useState('')
  const [retry, setRetry] = useState(0)

  useEffect(() => {
    if (!open) return
    const controller = new AbortController()
    const path = `/api/v1/tasks/${encodeURIComponent(taskID)}/runs/${encodeURIComponent(runID)}/logs`
    const query = invocation ? `?${new URLSearchParams({ invocation })}` : ''
    void request(path + query, { signal: controller.signal }, 15_000)
      .then((response) => response.json() as Promise<LogResult>)
      .then((data) => {
        if (!controller.signal.aborted) setResult(data)
      }).catch((failure: unknown) => {
        if (controller.signal.aborted) return
        if (failure instanceof AdminSessionError) onLock(failure.message)
        else setError((failure instanceof HubRequestError && requestErrors[failure.status]) || messageFor(failure))
      })
    return () => controller.abort()
  }, [open, retry, taskID, runID, invocation, onLock])

  function clearRead() {
    setResult(undefined)
    setError('')
  }

  const logTarget = invocation ? '本次 Review' : '此运行'

  return (
    <details className="run-logs" onToggle={(event) => {
      setOpen(event.currentTarget.open)
      if (!event.currentTarget.open) clearRead()
    }}>
      <summary>{invocation ? 'Review 原始日志' : '设备原始日志'}</summary>
      {open && <div className="run-log-content">
        {!result && !error && <p role="status">正在从设备读取…</p>}
        {error && <p role="status">{error}</p>}
        {result?.status === 'missing' && <p role="status">设备未找到{logTarget}的日志。</p>}
        {result?.status === 'failed' && <p role="status">设备无法读取{logTarget}的日志。</p>}
        {(error || result?.status === 'failed' || result?.status === 'missing') &&
          <button type="button" className="quiet-button" onClick={() => { clearRead(); setRetry((count) => count + 1) }}>重新读取</button>}
        {result?.status === 'available' && (result.files ?? []).map((file) => <div className="run-log-file" key={`${file.invocation}/${file.stream}`}>
          <header>
            <strong>{!invocation && file.invocation.startsWith('review-') ? `${file.invocation} · ${file.stream}` : file.stream}</strong>
            <span>{file.updated_at && <Time value={file.updated_at} />}{file.truncated && ' · 已截取末尾'}</span>
          </header>
          <pre tabIndex={0}>{file.text || '无输出'}</pre>
        </div>)}
      </div>}
    </details>
  )
}
