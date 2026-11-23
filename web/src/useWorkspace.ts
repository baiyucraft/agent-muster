import { useCallback, useEffect, useState } from 'react'
import { AdminTokenError, HubRequestError } from './api/devices'
import { latestApproval, readWorkspace, waitForWorkspaceEvent, type Approval, type Message, type Workspace } from './api/workspace'

/**
 * 将请求失败转换为界面可展示的说明。
 * @param error 捕获的失败值。
 * @returns 已知错误的消息，或无法读取 Hub 结果的通用说明。
 */
export function messageFor(error: unknown): string {
  return error instanceof Error && !(error instanceof SyntaxError)
    ? error.message
    : 'Hub 返回了无法读取的结果。'
}

function pause(signal: AbortSignal, milliseconds: number): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) {
      resolve()
      return
    }
    const timer = window.setTimeout(finish, milliseconds)
    function finish() {
      window.clearTimeout(timer)
      signal.removeEventListener('abort', finish)
      resolve()
    }
    signal.addEventListener('abort', finish, { once: true })
  })
}

/**
 * 管理工作区快照、SSE 重连与主对话消息回填，卸载时关闭请求。
 * @param token 当前管理员会话的令牌。
 * @param onLock Hub 拒绝令牌时通知调用方锁定会话。
 * @returns 工作区快照、流状态、错误以及刷新和接收 Hub 写入结果的操作。
 */
export function useWorkspace(token: string, onLock: (reason: string) => void) {
  const [workspace, setWorkspace] = useState<Workspace | null>(null)
  const [streamState, setStreamState] = useState<'loading' | 'live' | 'retry' | 'unavailable'>('loading')
  const [error, setError] = useState('')
  const [revision, setRevision] = useState(0)
  const refresh = useCallback(() => setRevision((value) => value + 1), [])
  const acceptMessage = useCallback((message: Message) => {
    setWorkspace((current) => current && !current.messages.some((item) => item.id === message.id)
      ? { ...current, messages: [...current.messages, message] }
      : current)
    refresh()
  }, [refresh])
  const acceptApproval = useCallback((approval: Approval) => {
    setWorkspace((current) => {
      if (!current) return current
      return {
        ...current,
        approvals: current.approvals.some((item) => item.id === approval.id)
          ? current.approvals.map((item) => item.id === approval.id ? latestApproval(approval, item) : item)
          : [...current.approvals, approval],
      }
    })
    refresh()
  }, [refresh])

  useEffect(() => {
    const controller = new AbortController()
    const { signal } = controller

    async function synchronize() {
      let cursor: number | undefined
      while (!signal.aborted) {
        try {
          if (cursor === undefined) {
            const snapshot = await readWorkspace(token, signal)
            if (signal.aborted) return
            setWorkspace((current) => {
              if (!current?.approvals.length || !snapshot.approvals.length) return snapshot
              // 快照请求可能先于答复提交发出，保留已经从 Hub 确认的新状态。
              const confirmed = new Map(current.approvals.map((approval) => [approval.id, approval]))
              return {
                ...snapshot,
                approvals: snapshot.approvals.map((approval) => latestApproval(approval, confirmed.get(approval.id))),
              }
            })
            cursor = snapshot.cursor
          }
          setError('')
          setStreamState('live')
          if (await waitForWorkspaceEvent(token, cursor, signal)) cursor = undefined
        } catch (failure) {
          if (signal.aborted) return
          if (failure instanceof AdminTokenError) {
            onLock(failure.message)
            return
          }
          cursor = undefined
          if (failure instanceof HubRequestError && failure.status === 409) continue
          const unavailable = failure instanceof HubRequestError && failure.status === 503
          setError(unavailable ? '主对话和任务服务暂不可用，设备管理仍可打开。' : messageFor(failure))
          setStreamState(unavailable ? 'unavailable' : 'retry')
          await pause(signal, 2_000)
        }
      }
    }

    void synchronize()
    return () => controller.abort()
  }, [token, onLock, revision])

  return { workspace, streamState, error, refresh, acceptMessage, acceptApproval }
}
