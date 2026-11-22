import { useEffect, useRef, useState, type FormEvent } from 'react'
import { AdminTokenError, createPairingCode, readDevices, revokeDevice, type Agent, type Device, type Pairing } from '../../api/devices'
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

export function DeviceInventory({ token, onLock }: { token: string; onLock: (error: string) => void }) {
  const [devices, setDevices] = useState<Device[] | null>(null)
  const [loadError, setLoadError] = useState('')
  const [actionError, setActionError] = useState('')
  const [refresh, setRefresh] = useState(0)
  const [name, setName] = useState('')
  const [pairing, setPairing] = useState<Pairing | null>(null)
  const [acting, setActing] = useState(false)
  const [pairingOpen, setPairingOpen] = useState(false)
  const [selectedID, setSelectedID] = useState('')
  const action = useRef<AbortController | null>(null)
  const pairingDialog = useRef<HTMLDialogElement>(null)

  useEffect(() => () => action.current?.abort(), [])

  useEffect(() => {
    const controller = new AbortController()
    let timer: number | undefined

    async function load() {
      try {
        const snapshot = await readDevices(token, controller.signal)
        if (controller.signal.aborted) return
        setDevices(snapshot)
        setLoadError('')
      } catch (error) {
        if (controller.signal.aborted) return
        if (error instanceof AdminTokenError) {
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
  }, [token, refresh, onLock])

  async function runAction<T>(request: (signal: AbortSignal) => Promise<T>, onSuccess: (value: T) => void) {
    if (action.current) return

    const controller = new AbortController()
    action.current = controller
    setActing(true)
    setActionError('')
    try {
      const value = await request(controller.signal)
      if (!controller.signal.aborted) onSuccess(value)
    } catch (error) {
      if (controller.signal.aborted) return
      if (error instanceof AdminTokenError) {
        onLock(error.message)
      } else {
        setActionError(messageFor(error))
      }
    } finally {
      action.current = null
      if (!controller.signal.aborted) setActing(false)
    }
  }

  function submitPairing(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const deviceName = name.trim()
    if (!deviceName) return

    void runAction((signal) => createPairingCode(token, deviceName, signal), (code) => {
      setPairing(code)
      setName('')
    })
  }

  function confirmRevocation(device: Device) {
    if (action.current || !window.confirm(`撤销「${device.name}」的接入权限？这台设备需要重新配对才能上报。`)) return

    void runAction((signal) => revokeDevice(token, device.id, signal), () => setRefresh((value) => value + 1))
  }

  function openPairing() {
    setActionError('')
    setPairingOpen(true)
    pairingDialog.current?.showModal()
  }

  const onlineCount = devices?.filter((device) => device.online && !device.revoked_at).length ?? 0
  const selected = devices?.find((device) => device.id === selectedID) ?? devices?.[0]

  return (
    <section className="device-inventory" aria-labelledby="device-heading">
      <header className="view-heading device-view-heading">
        <div><h1 id="device-heading">设备</h1>{devices && <p className="view-count">{devices.length} 台设备 · {onlineCount} 台在线</p>}</div>
        <div className="form-actions">
          <button type="button" className="outline-button" onClick={() => setRefresh((value) => value + 1)}>刷新清单</button>
          <button type="button" className="primary-button" onClick={openPairing} disabled={!devices}>配对设备</button>
        </div>
      </header>
      <dialog ref={pairingDialog} className="pairing-panel" aria-labelledby="pairing-heading" onClose={() => {
        setPairingOpen(false)
        setActionError('')
      }}>
          <div className="pairing-header"><h2 id="pairing-heading">配对设备</h2><button type="button" className="quiet-button" onClick={() => pairingDialog.current?.close()}>关闭</button></div>
          <form className="pairing-form" onSubmit={submitPairing}>
            <div className="pairing-input">
              <label htmlFor="device-name">设备名称</label>
              <input id="device-name" value={name} onChange={(event) => setName(event.target.value)} maxLength={80} required />
            </div>
            <button type="submit" className="primary-button" disabled={acting}>生成配对码</button>
          </form>
          {actionError && <p className="read-error" role="alert">{actionError}</p>}
          {pairing && <PairingDetails key={pairing.expires_at + pairing.code} pairing={pairing} />}
      </dialog>
      {actionError && !pairingOpen && <p className="read-error" role="alert">{actionError}</p>}
      {loadError && (
        <p className="read-error" role="status">
          {loadError}
          {devices && ' 当前显示上次读取的状态。'}
        </p>
      )}
      {!devices && !loadError && <p className="empty-inventory" role="status">正在读取设备清单…</p>}
      {devices?.length === 0 && <p className="panel-placeholder">尚无设备配对。生成配对码后，在设备上打开 Agent Muster 并输入配对码。</p>}
      {devices && selected && (
        <div className="device-workspace">
          <section className="device-list" aria-label="设备清单">
            {devices.map((device) => <button className="device-list-item" type="button" aria-pressed={device.id === selected.id} key={device.id} onClick={() => setSelectedID(device.id)}>
              <span className="device-glyph" aria-hidden="true" />
              <span className="device-list-label"><strong>{device.name}</strong><span>{device.os === 'windows' ? 'Windows' : device.os} · {device.agents.filter((agent) => agent.status !== 'missing').length} 个 Agents</span></span>
              <span className="device-status" data-online={device.online && !device.revoked_at}>{deviceStatus(device)}</span>
            </button>)}
          </section>
          <DeviceCard device={selected} disabled={acting} onRevoke={() => confirmRevocation(selected)}>
            {!selected.revoked_at && selected.os === 'windows' && <DeviceUpdates key={selected.id} token={token} deviceID={selected.id} onLock={onLock} />}
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

function PairingDetails({ pairing }: { pairing: Pairing }) {
  const expiry = Date.parse(pairing.expires_at)
  const [expired, setExpired] = useState(() => expiry <= Date.now())
  // 开发服务器代理 API。远端设备需要使用其可访问的 Hub 地址。
  const hubURL = import.meta.env.DEV ? 'http://127.0.0.1:8080' : window.location.origin

  useEffect(() => {
    if (expired) return
    const timer = window.setTimeout(() => setExpired(true), Math.max(expiry - Date.now(), 0))
    return () => window.clearTimeout(timer)
  }, [expiry, expired])

  return (
    <div className="pairing-details">
      {expired ? (
        <p role="status">配对码已过期，请生成新的配对码。</p>
      ) : (
        <>
          <div className="pairing-code">
            <output aria-label="一次性配对码">{pairing.code}</output>
            <span>有效期至 <Timestamp value={pairing.expires_at} /></span>
          </div>
          <p>在目标设备上运行下列命令，按提示粘贴配对码。</p>
          <p>也可在 Agent Muster 客户端输入 Hub 地址 <code>{hubURL}</code> 和配对码。</p>
          <code className="pairing-command">muster-daemon enroll -hub-url {hubURL}</code>
          {import.meta.env.DEV && <p>远程设备请把示例地址替换为设备可访问的 Hub 地址。</p>}
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

function DeviceCard({ device, disabled, onRevoke, children }: { device: Device; disabled: boolean; onRevoke: () => void; children?: React.ReactNode }) {
  const status = deviceStatus(device)

  return (
    <article className="device-card" aria-labelledby={`device-${device.id}`}>
      <header className="device-header">
        <div className="device-identity">
          <h2 id={`device-${device.id}`}>{device.name}</h2>
          <div className="device-meta">
            <code>{device.id}</code>
            {device.hostname && <span>{device.hostname}</span>}
            {device.os && device.arch && <span>{device.os} / {device.arch}</span>}
          </div>
        </div>
        <div className="device-presence">
          <span className="device-status" data-online={status === '在线'}>{status}</span>
          {!device.revoked_at && (
            <button type="button" className="revoke-button" onClick={onRevoke} disabled={disabled}>撤销设备</button>
          )}
        </div>
      </header>
      <div className="device-times">
        <span>登记于 <Timestamp value={device.registered_at} /></span>
        <span>最近上报 <Timestamp value={device.last_seen_at} /></span>
        <span>CLI 探测 <Timestamp value={device.probed_at} /></span>
        {device.revoked_at && <span>撤销于 <Timestamp value={device.revoked_at} /></span>}
      </div>
      <div className="device-agents-heading"><h3>Agents</h3><span>账号认证未验证</span></div>
      {device.agents.length > 0 ? (
        <div className="agent-table-wrap"><AgentTable agents={device.agents} /></div>
      ) : (
        <p className="empty-agents">暂无 CLI 探测记录。</p>
      )}
      {children}
    </article>
  )
}

function AgentTable({ agents }: { agents: Agent[] }) {
  return (
    <table className="agent-table">
      <thead>
        <tr>
          <th scope="col">Agent</th>
          <th scope="col">版本</th>
          <th scope="col">探测结果</th>
          <th scope="col">CLI 能力</th>
        </tr>
      </thead>
      <tbody>
        {agents.map((agent) => (
          <tr key={agent.id}>
            <th scope="row">{agentLabel[agent.id]}</th>
            <td><code>{agent.version || '—'}</code></td>
            <td><span className="probe-label" data-status={agent.status}>{probeLabel[agent.status]}</span></td>
            <td>
              {agent.status === 'missing' || agent.status === 'probe_failed' ? (
                <span className="unknown-capability">— <span className="screen-reader">能力未知</span></span>
              ) : (
                <ul className="capabilities">
                  {capabilityLabels.map(([capability, label]) => (
                    <li key={capability} data-supported={agent.capabilities[capability]}>
                      <span aria-hidden="true">{agent.capabilities[capability] ? '✓' : '−'}</span>{' '}
                      <span className="screen-reader">{agent.capabilities[capability] ? '支持' : '未确认'}</span>{label}
                    </li>
                  ))}
                </ul>
              )}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}
