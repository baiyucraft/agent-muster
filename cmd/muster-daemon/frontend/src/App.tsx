import { Window } from '@wailsio/runtime'
import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react'
import type { DeviceApi, DeviceStatus, WorkDirectory } from './types'

const refreshInterval = 5_000
const themeKey = 'muster-device-theme'
const dateTime = new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' })

const agentName = { codex: 'Codex', dsh: 'dsh' } as const
const probeText: Record<string, string> = {
  supported: '能力完整',
  missing: '未安装',
  unsupported: '能力不完整',
  probe_failed: '探测失败',
}
const connectionText: Record<string, string> = {
  online: '已连接',
  connected: '已连接',
  checking: '正在连接',
  offline: '连接中断',
  revoked: '凭据已撤销',
  unpaired: '尚未配对',
}
const updateText: Record<string, string> = {
  downloading: '正在下载并校验', waiting: '等待当前任务完成', applying: '正在安装',
  succeeded: '更新成功', failed: '更新失败', rolled_back: '已回滚',
}
const peerText: Record<string, string> = {
  connecting: '正在登记', ready: '等待设备间交接', direct: '最近通过直连交接',
  relay: '最近通过加密中继交接', unavailable: '暂不可用',
}

const emptyStatus: DeviceStatus = {
  paired: false,
  hubUrl: '',
  deviceId: '',
  connection: 'unpaired',
  lastHeartbeat: null,
  agents: [],
  probedAt: null,
  lastError: '',
  directories: [],
  latestRun: null,
  workError: '',
  peerAddress: '',
  peerTransport: '',
  version: '',
  update: null,
}

export function App({ api }: { api: DeviceApi }) {
  const [status, setStatus] = useState<DeviceStatus>(emptyStatus)
  const [loading, setLoading] = useState(true)
  const [hubUrl, setHubUrl] = useState('')
  const [code, setCode] = useState('')
  const [pairing, setPairing] = useState(false)
  const [probing, setProbing] = useState(false)
  const [addingDirectory, setAddingDirectory] = useState(false)
  const [savingPeer, setSavingPeer] = useState(false)
  const [peerAddress, setPeerAddress] = useState<string | null>(null)
  const [actionError, setActionError] = useState('')
  const [dark, setDark] = useState(() => {
    const saved = localStorage.getItem(themeKey)
    return saved ? saved === 'dark' : window.matchMedia('(prefers-color-scheme: dark)').matches
  })
  const loadingRef = useRef(false)

  useEffect(() => {
    document.documentElement.dataset.theme = dark ? 'dark' : 'light'
  }, [dark])

  function toggleTheme() {
    localStorage.setItem(themeKey, dark ? 'light' : 'dark')
    setDark(!dark)
  }

  const loadStatus = useCallback(async () => {
    if (loadingRef.current) return
    loadingRef.current = true
    try {
      const next = await api.getStatus()
      setStatus(next)
      setHubUrl((current) => current || next.hubUrl)
      setActionError('')
    } catch (error) {
      setActionError(errorMessage(error))
    } finally {
      loadingRef.current = false
      setLoading(false)
    }
  }, [api])

  useEffect(() => {
    void loadStatus()
    const timer = window.setInterval(() => void loadStatus(), refreshInterval)
    return () => window.clearInterval(timer)
  }, [loadStatus])

  async function pairDevice(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const normalizedHub = hubUrl.trim().replace(/\/+$/, '')
    const pairingCode = code.trim()
    if (!normalizedHub || !pairingCode || pairing) return

    setPairing(true)
    setActionError('')
    try {
      await api.pair(normalizedHub, pairingCode)
      await loadStatus()
    } catch (error) {
      setActionError(errorMessage(error))
    } finally {
      setCode('')
      setPairing(false)
    }
  }

  async function refreshInventory() {
    if (probing) return
    setProbing(true)
    setActionError('')
    try {
      await api.refreshAgents()
      await loadStatus()
    } catch (error) {
      setActionError(errorMessage(error))
    } finally {
      setProbing(false)
    }
  }

  async function addWorkDirectory() {
    if (addingDirectory) return
    setAddingDirectory(true)
    setActionError('')
    try {
      const directory = await api.addWorkDirectory()
      if (directory) {
        setStatus((current) => ({ ...current, directories: [...current.directories.filter((item) => item.id !== directory.id), directory] }))
      }
    } catch (error) {
      setActionError(errorMessage(error))
    } finally {
      setAddingDirectory(false)
    }
  }

  async function savePeerAddress(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setSavingPeer(true)
    setActionError('')
    try {
      await api.setPeerAddress((peerAddress ?? status.peerAddress).trim())
      await loadStatus()
      setPeerAddress(null)
    } catch (error) {
      setActionError(errorMessage(error))
    } finally {
      setSavingPeer(false)
    }
  }

  const connection = connectionText[status.connection] ?? status.connection
  const connected = status.connection === 'online' || status.connection === 'connected'
  const revoked = status.connection === 'revoked'
  const needsPairing = !status.paired || revoked

  return (
    <div className="device-app">
      <header className="topbar">
        <div className="brand-lockup">
          <span className="brand-mark" aria-hidden="true"><i /><i /><i /></span>
          <span>Agent Muster</span>
          <span className="topbar-section">设备端</span>
        </div>
        <div className="topbar-actions">
          <button className="theme-button" type="button" onClick={toggleTheme}
            aria-label={dark ? '切换到浅色' : '切换到深色'} title={dark ? '切换到浅色' : '切换到深色'}>
            <span aria-hidden="true">{dark ? '☼' : '☾'}</span>
          </button>
          <div className="window-controls" role="group" aria-label="窗口操作">
            <button className="window-button" type="button" onClick={() => void Window.Minimise()}
              aria-label="最小化" title="最小化">
              <span className="window-icon minimise-icon" aria-hidden="true" />
            </button>
            <button className="window-button" type="button" onClick={() => void Window.ToggleMaximise()}
              aria-label="最大化或还原" title="最大化或还原">
              <span className="window-icon maximise-icon" aria-hidden="true" />
            </button>
            <button className="window-button window-close" type="button" onClick={() => void Window.Close()}
              aria-label="关闭到托盘" title="关闭到托盘">
              <span className="window-icon close-icon" aria-hidden="true" />
            </button>
          </div>
        </div>
      </header>

      <main className="content-wrap">
        {(actionError || status.lastError) && (
          <div className="error-banner" role="alert">
            <span aria-hidden="true">!</span><p>{actionError || status.lastError}</p>
          </div>
        )}

        {loading ? (
          <p className="loading-state" role="status"><span className="loader" aria-hidden="true" />正在读取设备状态</p>
        ) : needsPairing ? (
          <section aria-labelledby="pairing-heading">
            <div className="page-heading">
              <div>
                <h1 id="pairing-heading">{revoked ? '重新配对' : '配对设备'}</h1>
                <p>从 Hub 管理页生成配对码，再添加这台电脑。</p>
              </div>
              <span className="status-badge" data-state={revoked ? 'warning' : undefined}>{revoked ? connection : '尚未配对'}</span>
            </div>
            <form className="pairing-form panel" onSubmit={pairDevice}>
              <label htmlFor="hub-url">Hub 地址</label>
              <input
                id="hub-url"
                type="url"
                inputMode="url"
                placeholder="http://服务器IP:13011"
                value={hubUrl}
                onChange={(event) => setHubUrl(event.target.value)}
                autoComplete="url"
                spellCheck={false}
                aria-describedby="hub-address-hint"
                required
              />
              <p id="hub-address-hint" className="field-hint">填写工作台的 HTTP 或 HTTPS 地址。</p>
              <label htmlFor="pairing-code">一次性配对码</label>
              <input
                id="pairing-code"
                type="password"
                placeholder="粘贴 24 位配对码"
                value={code}
                onChange={(event) => setCode(event.target.value)}
                autoComplete="off"
                spellCheck={false}
                maxLength={24}
                required
              />
              <div className="form-actions">
                <p>凭据保存在 Windows 凭据管理器</p>
                <button className="primary-button" type="submit" disabled={pairing || !hubUrl.trim() || !code.trim()}>
                  {pairing ? <><span className="loader" aria-hidden="true" />正在配对</> : '配对设备'}
                </button>
              </div>
            </form>
          </section>
        ) : (
          <>
            <section className="workspace-section" aria-labelledby="workspace-heading">
              <div className="page-heading">
                <div>
                  <h1 id="workspace-heading">工作目录与运行</h1>
                  <p>{status.directories.length ? `${status.directories.length} 个已授权目录` : '登记目录后，Hub 才能安全地派发本机任务'}</p>
                </div>
                <button className="primary-button" type="button" onClick={() => void addWorkDirectory()} disabled={addingDirectory}>
                  {addingDirectory ? '正在选择' : '添加工作目录'}
                </button>
              </div>
              {status.directories.length ? (
                <ul className="directory-list panel" aria-label="已授权工作目录">
                  {status.directories.map((directory) => <DirectoryRow directory={directory} key={directory.id} />)}
                </ul>
              ) : (
                <div className="workspace-empty panel">尚未登记工作目录</div>
              )}
              {status.workError && <p className="work-error" role="status">{status.workError}</p>}
              <RunSummary run={status.latestRun} />
            </section>

            <section aria-labelledby="agent-heading">
              <div className="page-heading">
                <div>
                  <h1 id="agent-heading">本机 Agents</h1>
                  <p>{status.probedAt ? <>上次探测 <time dateTime={status.probedAt}>{formatTime(status.probedAt)}</time></> : '等待首次 CLI 探测'}</p>
                </div>
                <button className="secondary-button" type="button" onClick={() => void refreshInventory()} disabled={probing}>
                  <span className={probing ? 'refresh-mark spinning' : 'refresh-mark'} aria-hidden="true">↻</span>
                  {probing ? '探测中' : '重新探测'}
                </button>
              </div>
              <div className="agent-list panel" aria-busy={probing}>
                {status.agents.length ? status.agents.map((agent) => (
                  <details className="agent-row" key={agent.id}>
                    <summary>
                      <span className="agent-symbol" data-agent={agent.id} aria-hidden="true">{agent.id === 'dsh' ? 'd' : '›_'}</span>
                      <span className="agent-name">
                        <strong>{agentName[agent.id as keyof typeof agentName] ?? agent.id}</strong>
                        {agent.version && <code>{agent.version}</code>}
                      </span>
                      <span className="status-badge" data-state={
                        agent.status === 'supported' ? 'success' : agent.status === 'missing' ? undefined : 'warning'
                      }>{probeText[agent.status] ?? agent.status}</span>
                      <span className="chevron" aria-hidden="true" />
                    </summary>
                    <ul className="capability-list" aria-label={agent.id + ' CLI 能力'}>
                      <Capability supported={agent.capabilities.prompt} label="接收提示词" />
                      <Capability supported={agent.capabilities.final_reply} label="最终回复" />
                      <Capability supported={agent.capabilities.resume} label="续接会话" />
                    </ul>
                  </details>
                )) : <p className="empty-agents">尚无探测结果</p>}
              </div>
              <p className="account-state">模型账号未验证</p>
            </section>

            <details className="hub-panel panel">
              <summary><span className="hub-name"><strong>设备互联</strong><span>{peerText[status.peerTransport] ?? '自动连接'}</span></span><span className="chevron" aria-hidden="true" /></summary>
              <form className="peer-address-form" onSubmit={savePeerAddress}>
                <label htmlFor="peer-address">可选的直连地址</label>
                <input id="peer-address" type="url" inputMode="url" placeholder="https://100.64.0.12:13012" value={peerAddress ?? status.peerAddress} onChange={(event) => setPeerAddress(event.target.value)} spellCheck={false} aria-describedby="peer-hint" />
                <p id="peer-hint" className="field-hint">留空时使用自动连接。已有局域网或 VPN 直连地址可保留，修改后重新打开设备端生效。</p>
                <button className="secondary-button" type="submit" disabled={savingPeer || peerAddress === null || peerAddress.trim() === status.peerAddress}>{savingPeer ? '正在保存' : '保存地址'}</button>
              </form>
            </details>
            <details className="hub-panel panel" open={status.update ? true : undefined}>
              <summary>
                <span className="hub-name"><strong>设备端更新</strong><span>{status.update ? updateText[status.update.state] ?? status.update.state : '由 Hub 管理'}</span></span>
                <span className="chevron" aria-hidden="true" />
              </summary>
              <dl className="hub-details">
                <div><dt>当前版本</dt><dd><code>{status.version || '—'}</code></dd></div>
                {status.update && <div><dt>目标版本</dt><dd><code>{status.update.version}</code></dd></div>}
                {status.update?.error && <div><dt>更新结果</dt><dd>{status.update.error}</dd></div>}
              </dl>
            </details>

            <details className="hub-panel panel">
              <summary>
                <span className="hub-symbol" aria-hidden="true"><i /></span>
                <span className="hub-name"><strong>Hub 连接</strong><code>{status.hubUrl}</code></span>
                <span className="status-badge" data-state={connected ? 'success' : status.connection === 'offline' ? 'warning' : undefined}>{connection}</span>
                <span className="chevron" aria-hidden="true" />
              </summary>
              <dl className="hub-details">
                <div><dt>设备 ID</dt><dd><code>{status.deviceId || '—'}</code></dd></div>
                <div><dt>最近心跳</dt><dd><time dateTime={status.lastHeartbeat ?? undefined}>{formatTime(status.lastHeartbeat)}</time></dd></div>
              </dl>
            </details>
          </>
        )}
      </main>
    </div>
  )
}

function DirectoryRow({ directory }: { directory: WorkDirectory }) {
  return (
    <li className="directory-row">
      <span className="directory-mark" aria-hidden="true"><span /></span>
      <span className="directory-name">
        <strong>{directory.label}</strong>
        <code title={directory.path}>{directory.path}</code>
      </span>
      <span className="status-badge" data-state="success">已授权</span>
    </li>
  )
}

function RunSummary({ run }: { run: DeviceStatus['latestRun'] }) {
  const stateLabels: Record<string, string> = {
    claimed: '已领取',
    starting: '正在启动',
    started: '运行中',
    completed: '已完成',
    waiting: '等待协作结果',
    failed: '执行失败',
    interrupted: '待核对',
  }
  return (
    <div className="run-summary" aria-live="polite">
      <div className="run-heading">
        <span>最近运行</span>
        {run && <span className="status-badge" data-state={run.state === 'completed' ? 'success' : run.state === 'failed' || run.state === 'interrupted' ? 'warning' : undefined}>
          {stateLabels[run.state] ?? run.state}
        </span>}
      </div>
      {run ? (
        <div className="run-body">
          <dl>
            <div><dt>任务</dt><dd><code>{run.taskId}</code></dd></div>
            {run.sessionId && <div><dt>Agent 会话</dt><dd><code>{run.sessionId}</code></dd></div>}
            {run.pending && <div><dt>回执</dt><dd>等待 Hub 确认，联网后自动补送</dd></div>}
          </dl>
          {run.text && <pre>{run.text}</pre>}
        </div>
      ) : <p className="workspace-empty">暂无运行记录</p>}
    </div>
  )
}

function Capability({ supported, label }: { supported: boolean; label: string }) {
  return (
    <li data-supported={supported}>
      <span className="capability-check" aria-hidden="true">{supported ? '✓' : '–'}</span>
      {label}
    </li>
  )
}

function formatTime(value: string | null): string {
  if (!value) return '尚未记录'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '时间未知' : dateTime.format(date)
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : '操作失败，请检查连接后重试。'
}
