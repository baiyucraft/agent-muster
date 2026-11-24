import { useEffect, useId, useRef, useState } from 'react'
import { HubRequestError } from '../../api/request'
import type { BindingProposal, BindingProposalDecision } from '../../api/workspace'
import { messageFor } from '../../useWorkspace'
import './approval.css'

/** 提交该卡片的决定，等待 Hub 确认后更新工作区。 */
export type SubmitBindingDecision = (id: string, decision: BindingProposalDecision, signal: AbortSignal) => Promise<void>

const statusText: Record<BindingProposal['status'], string> = {
  pending: '待确认',
  accepted: '已创建项目',
  rejected: '已拒绝',
}

/**
 * 展示中心建议的项目绑定，以及管理员可执行的决定或已保存的结果。
 * @param props 提案快照和提交决定的回调。离开卡片时停止等待请求，由工作区刷新确认结果。
 * @returns 主对话中的项目绑定提案卡片。
 */
export function BindingProposalCard({ proposal, onDecision }: { proposal: BindingProposal; onDecision: SubmitBindingDecision }) {
  const title = useId()
  const request = useRef<AbortController | null>(null)
  const [submitting, setSubmitting] = useState<BindingProposalDecision>()
  const [error, setError] = useState('')
  useEffect(() => () => request.current?.abort(), [])

  async function submit(decision: BindingProposalDecision) {
    if (request.current || proposal.status !== 'pending') return
    const controller = new AbortController()
    request.current = controller
    setSubmitting(decision)
    setError('')
    try {
      await onDecision(proposal.id, decision, controller.signal)
    } catch (failure) {
      if (controller.signal.aborted) return
      const conflict = failure instanceof HubRequestError && failure.status === 409
      setError(conflict ? '提案当前无法保存，可能已处理或绑定已变更。' : messageFor(failure))
    } finally {
      request.current = null
      if (!controller.signal.aborted) setSubmitting(undefined)
    }
  }

  return (
    <section className="approval-card binding-proposal" data-status={proposal.status} aria-labelledby={title}>
      <header className="binding-proposal-heading">
        <h2 id={title} className="approval-question">{proposal.name}</h2>
        <span className="approval-status" role="status">{statusText[proposal.status]}</span>
      </header>
      <ul className="binding-proposal-list" aria-label="项目绑定">
        {proposal.bindings.map((binding) => (
          <li key={`${binding.device_id}/${binding.directory_id}/${binding.agent_kind}`}>
            <div className="binding-proposal-target">
              <strong>{binding.directory_label}</strong>
              <span>{binding.device_name} · {binding.agent_kind === 'codex' ? 'Codex' : binding.agent_kind}</span>
            </div>
            {binding.is_default && <span className="binding-proposal-default">默认绑定</span>}
          </li>
        ))}
      </ul>
      {proposal.status === 'pending' && (
        <div className="binding-proposal-footer" aria-busy={Boolean(submitting)}>
          {error && <p className="approval-error" role="alert">{error}</p>}
          <div className="approval-options binding-proposal-actions">
            <button className="outline-button" type="button" disabled={Boolean(submitting)} onClick={() => void submit('rejected')}>{submitting === 'rejected' ? '正在提交…' : '拒绝'}</button>
            <button className="primary-button" type="button" disabled={Boolean(submitting)} onClick={() => void submit('accepted')}>{submitting === 'accepted' ? '正在创建…' : '接受并创建'}</button>
          </div>
        </div>
      )}
      {proposal.status === 'accepted' && proposal.project_id && <a className="text-link binding-proposal-projects" href="#projects">打开项目列表 <span aria-hidden="true">↗</span></a>}
    </section>
  )
}
