import { useEffect, useState } from 'react'
import { AdminTokenError } from '../../api/devices'
import { readDeviceUsage, type DeviceUsage as UsageData, type UsageSource } from '../../api/usage'
import { messageFor } from '../../useWorkspace'

const number = new Intl.NumberFormat('zh-CN')
const sourceLabels = { codex_exec: 'Codex', dsh_headless: 'dsh' }
const groupChoices = [{ id: 'projects', label: '按项目' }, { id: 'devices', label: '按设备' }] as const

/**
 * 查看设备用量和各 CLI 来源的统计覆盖。
 * @param props 当前管理员与会话锁定操作。
 * @returns 设备用量、读取状态与分组切换界面。
 */
export function DeviceUsage({ token, onLock }: { token: string; onLock: (reason?: string) => void }) {
  const [dimension, setDimension] = useState<keyof UsageData>('projects')
  const [data, setData] = useState<UsageData>()
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)
  const [refreshCount, setRefreshCount] = useState(0)

  useEffect(() => {
    const controller = new AbortController()
    void readDeviceUsage(token, controller.signal).then((result) => {
      if (!controller.signal.aborted) setData(result)
    }).catch((failure: unknown) => {
      if (controller.signal.aborted) return
      if (failure instanceof AdminTokenError) onLock(failure.message)
      else setError(messageFor(failure))
    }).finally(() => {
      if (!controller.signal.aborted) setLoading(false)
    })
    return () => controller.abort()
  }, [onLock, refreshCount, token])

  function refresh() {
    setError('')
    setLoading(true)
    setRefreshCount((count) => count + 1)
  }

  const groups = data?.[dimension] ?? []
  return (
    <section className="usage-view" aria-labelledby="usage-heading" aria-busy={loading}>
      <div className="view-heading">
        <h1 id="usage-heading">设备 Token 用量</h1>
        <button type="button" className="quiet-button" disabled={loading} onClick={refresh}>刷新</button>
      </div>
      <div className="usage-dimensions" role="group" aria-label="汇总维度">
        {groupChoices.map(({ id, label }) => <button type="button" className="quiet-button" key={id}
          aria-pressed={dimension === id} onClick={() => setDimension(id)}>{label}</button>)}
      </div>
      {error && <p className="read-error" role="status">{error}{data && ' 当前显示上次读取的用量。'}</p>}
      {loading && !data && <p className="panel-placeholder" role="status">正在读取用量…</p>}
      {data && groups.length === 0 && <p className="panel-placeholder">暂无已结束的运行</p>}
      <ul className="usage-groups">
        {groups.map((group) => <li key={group.id}>
          <div className="usage-group-heading">
            <h2>{group.name}</h2>
            <span>已统计 {number.format(group.measured_runs)} / {number.format(group.finished_runs)} 轮</span>
          </div>
          {(group.sources ?? []).map((source) => <SourceUsage key={source.source} source={source} />)}
        </li>)}
      </ul>
    </section>
  )
}

function SourceUsage({ source }: { source: UsageSource }) {
  const quantities = [
    { label: '输入', value: source.input_tokens, runs: source.measured_runs },
    { label: '输出', value: source.output_tokens, runs: source.measured_runs },
    { label: '读取缓存', value: source.cached_input_tokens, runs: source.cached_input_runs },
    { label: '写入缓存', value: source.cache_write_input_tokens, runs: source.cache_write_input_runs },
    { label: '推理输出', value: source.reasoning_output_tokens, runs: source.reasoning_output_runs },
    { label: '总计', value: source.total_tokens, runs: source.total_runs },
  ]
  return (
    <div className="usage-source">
      <h3>{sourceLabels[source.source]} <span>{number.format(source.measured_runs)} 轮</span></h3>
      <dl>
        {quantities.map(({ label, value, runs }) => value == null ? null : <div key={label}>
          <dt>{label}{runs < source.measured_runs && <span> · {number.format(runs)} 轮</span>}</dt>
          <dd>{number.format(BigInt(value))}</dd>
        </div>)}
      </dl>
    </div>
  )
}
