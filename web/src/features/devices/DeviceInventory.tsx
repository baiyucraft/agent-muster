import { useEffect, useRef, useState, type FormEvent } from 'react'
import { createPairingCode, readDevices, revokeDevice, type Agent, type Device, type Pairing } from '../../api/devices'
import { AdminSessionError } from '../../api/request'
import './device-inventory.css'
import { DeviceUpdates } from './DeviceUpdates'

const refreshInterval = 5_000
const timeFormat = new Intl.DateTimeFormat('zh-CN', {
  dateStyle: 'short',
  timeStyle: 'medium',
})

const agentLabel = { codex: 'Codex', dsh: 'dsh' }
const probeLabel = {
  supported: '能力已识别',
  missing: '未找到 CLI',
  unsupported: '能力不完整',
  probe_failed: '探测失败',
}
const capabilityLabels = [
  ['prompt', '接收任务'],
  ['final_reply', '输出最终回复'],
  ['resume', '续接会话'],
] as const

export function DeviceInventory({ onLock }: { onLock: (error: string) => void }) {
  const [devices, setDevices] = useState<Device[] | null>(null)
  const [loadError, setLoadError] = useState('')
  const [actionError, setActionError] = useState('')
  const [refresh, setRefresh] = useState(0)
  const [revoking, setRevoking] = useState(false)
  const [selectedID, setSelectedID] = useState('')
  const revocation = useRef<AbortController | null>(null)

  useEffect(() => () => revocation.current?.abort(), [])

  useEffect(() => {
    const controller = new AbortController()
    let timer: number | undefined

    async function load() {
      try {
        const snapshot = await readDevices(controller.signal)
        if (controller.signal.aborted) return
        setDevices(snapshot)
        setLoadError('')
      } catch (error) {
        if (controller.signal.aborted) return
        if (error instanceof AdminSessionError) {
          onLock(error.message)
          return
        }
        setLoadError(messageFor(error))
      }
      timer = window.setTimeout(load, refreshInterval)
    }

    void load()
    return () => {
      controller.abort()
      window.clearTimeout(timer)
    }
  }, [refresh, onLock])

  async function confirmRevocation(device: Device) {
    if (revocation.current || !window.confirm(`撤销「${device.name}」的接入权限？这台设备需要重新配对才能上报。`)) return

    const controller = new AbortController()
    revocation.current = controller
    setRevoking(true)
    setActionError('')
    try {
      await revokeDevice(device.id, controller.signal)
      if (!controller.signal.aborted) setRefresh((value) => value + 1)
    } catch (error) {
      if (controller.signal.aborted) return
      if (error instanceof AdminSessionError) {
        onLock(error.message)
      } else {
        setActionError(messageFor(error))
      }
    } finally {
      revocation.current = null
      if (!controller.signal.aborted) setRevoking(false)
    }
  }

  const onlineCount = devices?.filter((device) => device.online && !device.revoked_at).length ?? 0
  const selected = devices?.find((device) => device.id === selectedID) ?? devices?.[0]

  return (
    <section className="device-inventory" aria-labelledby="device-heading">
      <header className="view-heading device-view-heading">
        <div><h1 id="device-heading">设备</h1>{devices && devices.length > 0 && <p className="view-count">{onlineCount} / {devices.length} 在线</p>}</div>
        <div className="form-actions">
          <button type="button" className="quiet-button" onClick={() => setRefresh((value) => value + 1)}>刷新</button>
          <DevicePairing disabled={!devices} onLock={onLock} />
        </div>
      </header>
      {actionError && <p className="read-error" role="alert">{actionError}</p>}
      {loadError && (
        <p className="read-error" role="status">
          {loadError}
          {devices && ' 当前显示上次读取的状态。'}
        </p>
      )}
      {!devices && !loadError && <p className="empty-inventory" role="status">正在读取设备清单…</p>}
      {devices?.length === 0 && <p className="panel-placeholder">尚无已配对设备。</p>}
      {devices && selected && (
        <div className="device-workspace">
          <section className="device-list" aria-label="设备清单">
            {devices.map((device) => <button className="device-list-item" type="button" aria-pressed={device.id === selected.id} key={device.id} onClick={() => setSelectedID(device.id)}>
              <span className="device-glyph" aria-hidden="true" />
              <span className="device-list-label">
                <strong>{device.name}</strong>
                {device.os && <span>{platformFor(device)}</span>}
              </span>
              <span className="device-status" data-online={device.online && !device.revoked_at}>{deviceStatus(device)}</span>
            </button>)}
          </section>
          <DeviceCard key={selected.id} device={selected} disabled={revoking} onRevoke={() => confirmRevocation(selected)}>
            {!selected.revoked_at && selected.os === 'windows' && <DeviceUpdates deviceID={selected.id} onLock={onLock} />}
          </DeviceCard>
        </div>
      )}
    </section>
  )
}

function messageFor(error: unknown): string {
  return error instanceof Error && !(error instanceof SyntaxError)
    ? error.message
    : 'Hub 返回了无法读取的结果。'
}

// 将配对请求与本次弹窗绑定。关闭或离开设备页时取消请求并清空表单。
function DevicePairing({ disabled, onLock }: { disabled: boolean; onLock: (error: string) => void }) {
  const [name, setName] = useState('')
  const [pairing, setPairing] = useState<Pairing | null>(null)
  const [generating, setGenerating] = useState(false)
  const [error, setError] = useState('')
  const dialog = useRef<HTMLDialogElement>(null)
  const nameInput = useRef<HTMLInputElement>(null)
  const request = useRef<AbortController | null>(null)

  useEffect(() => () => request.current?.abort(), [])

  function closePairing() {
    request.current?.abort()
    request.current = null
    setGenerating(false)
    setName('')
    setPairing(null)
    setError('')
    dialog.current?.close()
  }

  async function generatePairing() {
    const deviceName = name.trim()
    if (!deviceName || request.current || !dialog.current?.open) return

    const controller = new AbortController()
    request.current = controller
    setGenerating(true)
    setError('')
    try {
      const next = await createPairingCode(deviceName, controller.signal)
      if (controller.signal.aborted) return
      if (typeof next?.code !== 'string' || !next.code || !Number.isFinite(Date.parse(next.expires_at))) {
        throw new Error('Hub 返回了无法读取的配对码。')
      }
      setPairing(next)
    } catch (failure) {
      if (controller.signal.aborted) return
      if (failure instanceof AdminSessionError) onLock(failure.message)
      else setError(messageFor(failure))
    } finally {
      if (!controller.signal.aborted) {
        request.current = null
        setGenerating(false)
      }
    }
  }

  function submitPairing(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    void generatePairing()
  }

  return (
    <>
      <button type="button" className="primary-button" disabled={disabled} onClick={() => {
        dialog.current?.showModal()
        nameInput.current?.focus()
      }}>配对设备</button>
      <dialog ref={dialog} className="pairing-panel" aria-labelledby="pairing-heading" onCancel={(event) => {
        event.preventDefault()
        closePairing()
      }}>
        <header className="pairing-header">
          <h2 id="pairing-heading">配对设备</h2>
          <button type="button" className="quiet-button" onClick={closePairing}>关闭</button>
        </header>
        {pairing ? (
          <PairingDetails key={pairing.code + pairing.expires_at} pairing={pairing} generating={generating} onRenew={() => void generatePairing()} />
        ) : (
          <form className="pairing-form" onSubmit={submitPairing}>
            <label htmlFor="device-name">设备名称</label>
            <input id="device-name" ref={nameInput} value={name} onChange={(event) => setName(event.target.value)} maxLength={80} required readOnly={generating} />
            <button type="submit" className="primary-button" disabled={generating || !name.trim()}>{generating ? '正在生成…' : '生成配对码'}</button>
          </form>
        )}
        {error && <p className="read-error" role="alert">{error}</p>}
      </dialog>
    </>
  )
}

// 有效期以 Hub 返回的时间为准。过期后移除旧码，同名设备可重新申请。
function PairingDetails({ pairing, generating, onRenew }: { pairing: Pairing; generating: boolean; onRenew: () => void }) {
  const expiry = Date.parse(pairing.expires_at)
  const [expired, setExpired] = useState(() => expiry <= Date.now())
  const [copyFeedback, setCopyFeedback] = useState('')
  const result = useRef<HTMLParagraphElement>(null)
  const codeInput = useRef<HTMLInputElement>(null)
  // 开发服务器代理 API。远端设备需要使用其可访问的 Hub 地址。
  const hubURL = import.meta.env.DEV ? 'http://127.0.0.1:8080' : window.location.origin

  useEffect(() => result.current?.focus(), [])

  useEffect(() => {
    if (expired) return
    const timer = window.setTimeout(() => {
      if (result.current?.parentElement?.contains(document.activeElement)) result.current.focus()
      setExpired(true)
    }, Math.max(expiry - Date.now(), 0))
    return () => window.clearTimeout(timer)
  }, [expiry, expired])

  async function copyCode(button: HTMLButtonElement) {
    if (Date.now() >= expiry) {
      setExpired(true)
      return
    }
    if (navigator.clipboard) {
      try {
        await navigator.clipboard.writeText(pairing.code)
        setCopyFeedback('已复制配对码')
        return
      } catch {
        // 浏览器拒绝剪贴板权限时仍可通过文本选区复制。
      }
    }

    const field = codeInput.current
    if (!field) return
    field.focus()
    field.select()
    // IP 形式的 HTTP Hub 通常没有 Clipboard API，复制仍由本次点击触发。
    try {
      if (document.execCommand?.('copy')) {
        button.focus()
        setCopyFeedback('已复制配对码')
        return
      }
    } catch {
      // 复制受浏览器策略阻止时，保留文本选区。
    }
    setCopyFeedback('配对码已选中，自动复制失败')
  }

  return (
    <div className="pairing-details">
      <p className="pairing-result" ref={result} tabIndex={-1} role="status">
        {expired ? '配对码已过期。' : <>有效期至 <Timestamp value={pairing.expires_at} /></>}
      </p>
      {expired ? (
        <button type="button" className="primary-button" onClick={onRenew} disabled={generating}>{generating ? '正在生成…' : '重新生成配对码'}</button>
      ) : (
        <>
          <dl className="pairing-address"><dt>{import.meta.env.DEV ? '本机 Hub 地址' : 'Hub 地址'}</dt><dd><code>{hubURL}</code></dd></dl>
          <label className="pairing-code-label" htmlFor="pairing-code">一次性配对码</label>
          <div className="pairing-code">
            <input id="pairing-code" ref={codeInput} readOnly value={pairing.code} spellCheck={false} />
            <button type="button" className="outline-button" onClick={(event) => void copyCode(event.currentTarget)}>复制配对码</button>
          </div>
          <p className="pairing-feedback" role="status">{copyFeedback}</p>
          <details className="pairing-cli"><summary>命令行配对</summary><code>muster-daemon enroll -hub-url {hubURL}</code></details>
        </>
      )}
    </div>
  )
}

function Timestamp({ value }: { value: string | null }) {
  if (!value) return <>—</>
  return <time dateTime={value}>{timeFormat.format(new Date(value))}</time>
}

function deviceStatus(device: Device) {
  return device.revoked_at ? '已撤销' : !device.last_seen_at ? '待上报' : device.online ? '在线' : '离线'
}

function platformFor(device: Device) {
  return device.os === 'windows' ? 'Windows' : device.os
}

function DeviceCard({ device, disabled, onRevoke, children }: { device: Device; disabled: boolean; onRevoke: () => void; children?: React.ReactNode }) {
  const status = deviceStatus(device)

  return (
    <article className="device-card" aria-labelledby={`device-${device.id}`}>
      <header className="device-header">
        <div className="device-identity">
          <div className="device-title">
            <h2 id={`device-${device.id}`}>{device.name}</h2>
            <span className="device-status" data-online={status === '在线'}>{status}</span>
          </div>
          <div className="device-meta">
            {device.os && <span>{platformFor(device)}{device.arch && ` · ${device.arch}`}</span>}
            {device.last_seen_at && <span>最近心跳 <Timestamp value={device.last_seen_at} /></span>}
          </div>
        </div>
      </header>
      <section aria-labelledby="device-agents-title">
        <header className="device-agents-heading">
          <h3 id="device-agents-title">Agents</h3>
          {device.probed_at && <span>{status === '在线' ? '最近探测' : '上次探测'} <Timestamp value={device.probed_at} /></span>}
        </header>
        {device.agents.length > 0 ? <AgentList agents={device.agents} /> : (
          <p className="empty-agents">暂无 CLI 探测记录。</p>
        )}
      </section>
      {children}
      <details className="device-facts">
        <summary>设备信息</summary>
        <dl>
          {device.hostname && <div><dt>主机名</dt><dd>{device.hostname}</dd></div>}
          <div><dt>设备 ID</dt><dd><code>{device.id}</code></dd></div>
          <div><dt>登记时间</dt><dd><Timestamp value={device.registered_at} /></dd></div>
          {device.agents.length > 0 && <div><dt>模型账号</dt><dd>未验证</dd></div>}
          {device.revoked_at && <div><dt>撤销时间</dt><dd><Timestamp value={device.revoked_at} /></dd></div>}
        </dl>
        {!device.revoked_at && <button type="button" className="revoke-button" onClick={onRevoke} disabled={disabled}>撤销设备</button>}
      </details>
    </article>
  )
}

function AgentList({ agents }: { agents: Agent[] }) {
  return (
    <ul className="agent-list">
      {agents.map((agent) => (
        <li key={agent.id} className="agent-entry">
          <div className="agent-entry-top">
            <h4>{agentLabel[agent.id]} {agent.version && <code>{agent.version}</code>}</h4>
            <span className="probe-label" data-status={agent.status}>{probeLabel[agent.status]}</span>
          </div>
          {(agent.status === 'supported' || agent.status === 'unsupported') && <details className="agent-capabilities">
            <summary aria-label={`${agentLabel[agent.id]} CLI 能力`}>CLI 能力</summary>
            <ul className="capabilities">
              {capabilityLabels.map(([capability, label]) => (
                <li key={capability} data-supported={agent.capabilities[capability]}>
                  <span aria-hidden="true">{agent.capabilities[capability] ? '✓' : '−'}</span>{' '}
                  <span className="screen-reader">{agent.capabilities[capability] ? '支持' : '未确认'}</span>{label}
                </li>
              ))}
            </ul>
          </details>}
        </li>
      ))}
    </ul>
  )
}
