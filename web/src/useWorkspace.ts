import { useCallback, useEffect, useState } from 'react'
import { AdminSessionError, HubRequestError } from './api/request'
import { latestApproval, readWorkspace, waitForWorkspaceEvent, type Approval, type BindingProposal, type Message, type Workspace } from './api/workspace'

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
 * @param onLock 管理员会话失效时通知调用方返回登录页。
 * @returns 工作区快照、流状态、错误以及刷新和接收 Hub 写入结果的操作。
 */
export function useWorkspace(onLock: (reason: string) => void) {
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
  const acceptBindingProposal = useCallback((proposal: BindingProposal) => {
    setWorkspace((current) => current && {
      ...current,
      binding_proposals: current.binding_proposals.map((item) => item.id === proposal.id ? proposal : item),
    })
    refresh()
  }, [refresh])

  const hasAnsweredApproval = workspace?.approvals.some((approval) => approval.status === 'answered') ?? false
  useEffect(() => {
    if (!hasAnsweredApproval || streamState !== 'live') return
    // 设备心跳和离线超时不产生工作区事件。请示仍待处理时定期刷新快照。
    const timer = window.setInterval(refresh, 15_000)
    return () => window.clearInterval(timer)
  }, [hasAnsweredApproval, streamState, refresh])

  useEffect(() => {
    const controller = new AbortController()
    const { signal } = controller

    async function synchronize() {
      let cursor: number | undefined
      while (!signal.aborted) {
        try {
          if (cursor === undefined) {
            const snapshot = await readWorkspace(signal)
            if (signal.aborted) return
            setWorkspace((current) => {
              if (!current) return snapshot
              // 快照请求可能先于决定提交发出，保留已经从 Hub 确认的新状态。
              const approvals = new Map(current.approvals.map((approval) => [approval.id, approval]))
              const decidedProposals = new Map(current.binding_proposals.filter((proposal) => proposal.status !== 'pending').map((proposal) => [proposal.id, proposal]))
              return {
                ...snapshot,
                approvals: snapshot.approvals.map((approval) => latestApproval(approval, approvals.get(approval.id))),
                binding_proposals: snapshot.binding_proposals.map((proposal) => proposal.status === 'pending'
                  ? decidedProposals.get(proposal.id) ?? proposal
                  : proposal),
              }
            })
            cursor = snapshot.cursor
          }
          setError('')
          setStreamState('live')
          if (await waitForWorkspaceEvent(cursor, signal)) cursor = undefined
        } catch (failure) {
          if (signal.aborted) return
          if (failure instanceof AdminSessionError) {
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
  }, [onLock, revision])

  return { workspace, streamState, error, refresh, acceptMessage, acceptApproval, acceptBindingProposal }
}
