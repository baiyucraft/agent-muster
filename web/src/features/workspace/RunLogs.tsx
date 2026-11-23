import { useEffect, useState } from 'react'
import { AdminTokenError, HubRequestError, request } from '../../api/devices'
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
 * @param props 管理员令牌、任务和运行标识以及会话锁定回调。
 * @returns 读取状态和设备返回的日志片段。
 */
export function RunLogs({ token, taskID, runID, onLock }: {
  token: string
  taskID: string
  runID: string
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
    void request(token, path, { signal: controller.signal }, 15_000)
      .then((response) => response.json() as Promise<LogResult>)
      .then((data) => {
        if (!controller.signal.aborted) setResult(data)
      }).catch((failure: unknown) => {
        if (controller.signal.aborted) return
        if (failure instanceof AdminTokenError) onLock(failure.message)
        else setError((failure instanceof HubRequestError && requestErrors[failure.status]) || messageFor(failure))
      })
    return () => controller.abort()
  }, [open, retry, token, taskID, runID, onLock])

  function clearRead() {
    setResult(undefined)
    setError('')
  }

  return (
    <details className="run-logs" onToggle={(event) => {
      setOpen(event.currentTarget.open)
      if (!event.currentTarget.open) clearRead()
    }}>
      <summary>设备原始日志</summary>
      {open && <div className="run-log-content">
        {!result && !error && <p role="status">正在从设备读取…</p>}
        {error && <p role="status">{error}</p>}
        {result?.status === 'missing' && <p role="status">设备未找到此运行的日志。</p>}
        {result?.status === 'failed' && <p role="status">设备无法读取此运行的日志。</p>}
        {(error || result?.status === 'failed' || result?.status === 'missing') &&
          <button type="button" className="quiet-button" onClick={() => { clearRead(); setRetry((count) => count + 1) }}>重新读取</button>}
        {result?.status === 'available' && (result.files ?? []).map((file) => <div className="run-log-file" key={`${file.invocation}/${file.stream}`}>
          <header><strong>{file.stream}</strong><span>{file.updated_at && <Time value={file.updated_at} />}{file.truncated && ' · 已截取末尾'}</span></header>
          <pre tabIndex={0}>{file.text || '无输出'}</pre>
        </div>)}
      </div>}
    </details>
  )
}
