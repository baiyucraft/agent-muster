import { Window } from '@wailsio/runtime'
import { Check, ChevronDown, CircleAlert, Command, Download, Folder, History, Link2, Minus, Monitor, Moon, Plus, RefreshCw, Square, Sun, Terminal, X, type LucideIcon } from 'lucide-react'
import { useCallback, useEffect, useRef, useState, type FormEvent, type ReactNode } from 'react'
import musterIcon from '../../../../ui/muster-icon.svg'
import type { DeviceApi, DeviceStatus, WorkDirectory } from './types'

const refreshInterval = 5_000
const themeKey = 'muster-device-theme'
const dateTime = new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' })

const agentName = { codex: 'Codex', dsh: 'dsh' } as const
const probeText: Record<string, string> = {
  supported: '能力已识别',
  missing: '未找到 CLI',
  unsupported: '能力不完整',
  probe_failed: '探测失败',
}
const probeTone: Record<string, string> = {
  supported: 'success',
  unsupported: 'warning',
  probe_failed: 'error',
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
  const [peerError, setPeerError] = useState('')
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
    setPeerError('')
    try {
      await api.setPeerAddress((peerAddress ?? status.peerAddress).trim())
      await loadStatus()
      setPeerAddress(null)
    } catch (error) {
      setPeerError(errorMessage(error))
    } finally {
      setSavingPeer(false)
    }
  }

  const connection = connectionText[status.connection] ?? status.connection
  const connected = status.connection === 'online' || status.connection === 'connected'
  const revoked = status.connection === 'revoked'
  const needsPairing = !status.paired || revoked
  const peer = peerText[status.peerTransport] ?? '自动连接'
  const update = status.update ? updateText[status.update.state] ?? status.update.state : status.version || '等待更新'

  return (
    <div className="device-app">
      <header className="topbar">
        <div className="brand-lockup">
          <img className="brand-mark" src={musterIcon} alt="" />
          <span>Agent Muster</span>
        </div>
        <div className="topbar-actions">
          {!loading && !needsPairing && (
            <div className="toolbar-statuses" role="group" aria-label="设备状态">
              <ToolbarStatus id="connection-status" icon={Monitor} label="中心连接" status={connection}
                tone={connected ? 'success' : status.connection === 'offline' ? 'warning' : undefined}>
                <dl className="status-details">
                  <div><dt>中心地址</dt><dd><code>{status.hubUrl}</code></dd></div>
                  <div><dt>最近心跳</dt><dd><time dateTime={status.lastHeartbeat ?? undefined}>{formatTime(status.lastHeartbeat)}</time></dd></div>
                  <div><dt>设备 ID</dt><dd><code>{status.deviceId || '—'}</code></dd></div>
                </dl>
              </ToolbarStatus>
              <ToolbarStatus id="peer-status" icon={Link2} label="设备互联" status={peer}
                tone={status.peerTransport === 'direct' || status.peerTransport === 'relay' ? 'success' : status.peerTransport === 'unavailable' ? 'warning' : undefined}>
                <form className="peer-address-form" onSubmit={savePeerAddress}>
                  <label htmlFor="peer-address">可选的直连地址</label>
                  <input id="peer-address" type="url" inputMode="url" placeholder="https://100.64.0.12:13012" value={peerAddress ?? status.peerAddress} onChange={(event) => setPeerAddress(event.target.value)} spellCheck={false} aria-describedby="peer-hint" />
                  <p id="peer-hint" className="field-hint">留空使用自动连接。修改后重新打开设备端生效。</p>
                  {peerError && <p className="field-error" role="alert">{peerError}</p>}
                  <button className="secondary-button" type="submit" disabled={savingPeer || peerAddress === null || peerAddress.trim() === status.peerAddress}>{savingPeer ? '正在保存' : '保存地址'}</button>
                </form>
              </ToolbarStatus>
              <ToolbarStatus id="update-status" icon={Download} label="设备端更新" status={update}
                tone={status.update?.state === 'failed' || status.update?.state === 'rolled_back' ? 'error' : status.update?.state === 'succeeded' ? 'success' : status.update ? 'warning' : undefined}>
                <dl className="status-details">
                  <div><dt>当前版本</dt><dd><code>{status.version || '—'}</code></dd></div>
                  {status.update && <div><dt>目标版本</dt><dd><code>{status.update.version}</code></dd></div>}
                  {status.update?.error && <div><dt>更新结果</dt><dd>{status.update.error}</dd></div>}
                </dl>
              </ToolbarStatus>
            </div>
          )}
          <button className="icon-button toolbar-button" type="button" onClick={toggleTheme}
            aria-label={dark ? '切换到浅色' : '切换到深色'} title={dark ? '切换到浅色' : '切换到深色'}>
            {dark ? <Sun size={16} aria-hidden="true" /> : <Moon size={16} aria-hidden="true" />}
          </button>
          <div className="window-controls" role="group" aria-label="窗口操作">
            <button className="window-button" type="button" onClick={() => void Window.Minimise()}
              aria-label="最小化" title="最小化">
              <Minus size={14} aria-hidden="true" />
            </button>
            <button className="window-button" type="button" onClick={() => void Window.ToggleMaximise()}
              aria-label="最大化或还原" title="最大化或还原">
              <Square size={12} aria-hidden="true" />
            </button>
            <button className="window-button window-close" type="button" onClick={() => void Window.Close()}
              aria-label="关闭到托盘" title="关闭到托盘">
              <X size={14} aria-hidden="true" />
            </button>
          </div>
        </div>
      </header>

      <main className="content-scroll" aria-label="设备工作台" tabIndex={0}>
        <div className="content-wrap">
          {(actionError || status.lastError) && (
            <div className="error-banner" role="alert">
              <CircleAlert size={16} aria-hidden="true" /><p>{actionError || status.lastError}</p>
            </div>
          )}

          {loading ? (
            <p className="loading-state" role="status"><span className="loader" aria-hidden="true" />正在读取设备状态</p>
          ) : needsPairing ? (
            <section className="pairing-section" aria-labelledby="pairing-heading">
              <div className="pairing-heading">
                <span className="item-icon" aria-hidden="true"><Link2 size={18} /></span>
                <div>
                  <h1 id="pairing-heading">{revoked ? '重新配对' : '配对设备'}</h1>
                  <p>{revoked ? '凭据已撤销，请使用新的配对码。' : '从中心设备页获取配对码。'}</p>
                </div>
              </div>
              <form className="pairing-form panel" onSubmit={pairDevice}>
                <label htmlFor="hub-url">中心地址</label>
                <input
                  id="hub-url"
                  type="url"
                  inputMode="url"
                  placeholder="http://服务器IP:13011"
                  value={hubUrl}
                  onChange={(event) => setHubUrl(event.target.value)}
                  autoComplete="url"
                  spellCheck={false}
                  required
                />
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
                  <button className="primary-button" type="submit" disabled={pairing || !hubUrl.trim() || !code.trim()}>
                    {pairing ? <><span className="loader" aria-hidden="true" />正在配对</> : <><Link2 size={15} aria-hidden="true" />配对设备</>}
                  </button>
                </div>
              </form>
            </section>
          ) : (
            <>
              <section aria-labelledby="workspace-heading">
                <div className="page-heading">
                  <h1 id="workspace-heading">工作目录</h1>
                  <button className="secondary-button add-directory" type="button" onClick={() => void addWorkDirectory()} disabled={addingDirectory}>
                    <Plus size={15} aria-hidden="true" />
                    {addingDirectory ? '正在选择' : '添加目录'}
                  </button>
                </div>
                {status.directories.length ? (
                  <ul className="directory-list panel" aria-label="已授权工作目录">
                    {status.directories.map((directory) => <DirectoryRow directory={directory} key={directory.id} />)}
                  </ul>
                ) : (
                  <div className="empty-row panel"><span className="item-icon" aria-hidden="true"><Folder size={16} /></span>尚未登记工作目录</div>
                )}
                {status.workError && <p className="work-error" role="status"><CircleAlert size={14} aria-hidden="true" />{status.workError}</p>}
              </section>

              <section aria-labelledby="agent-heading">
                <div className="page-heading">
                  <h2 id="agent-heading">Agents</h2>
                  <div className="agent-actions">
                    {status.probedAt && <time dateTime={status.probedAt} title="上次探测时间">{formatTime(status.probedAt)}</time>}
                    <button className="icon-button" type="button" onClick={() => void refreshInventory()} disabled={probing}
                      aria-label={probing ? '正在探测 Agents' : '重新探测 Agents'} title="重新探测 Agents">
                      <RefreshCw size={15} className={probing ? 'spinning' : undefined} aria-hidden="true" />
                    </button>
                  </div>
                </div>
                <div className="agent-list panel" aria-busy={probing}>
                  {status.agents.length ? status.agents.map((agent) => (
                    <details className="agent-row" key={agent.id}>
                      <summary>
                        <span className="item-icon" data-agent={agent.id} aria-hidden="true">{agent.id === 'dsh' ? <Command size={16} /> : <Terminal size={16} />}</span>
                        <span className="item-name">
                          <strong>{agentName[agent.id as keyof typeof agentName] ?? agent.id}</strong>
                          {agent.version && <code title={agent.version}>{agent.version}</code>}
                        </span>
                        <span className="item-status" data-state={probeTone[agent.status]}>{probeText[agent.status] ?? agent.status}</span>
                        <ChevronDown className="chevron" size={14} aria-hidden="true" />
                      </summary>
                      <div className="agent-details">
                        <ul className="capability-list" aria-label={agent.id + ' CLI 能力'}>
                          <Capability supported={agent.capabilities.prompt} label="接收提示词" />
                          <Capability supported={agent.capabilities.final_reply} label="最终回复" />
                          <Capability supported={agent.capabilities.resume} label="续接会话" />
                        </ul>
                        <p>模型账号未验证</p>
                      </div>
                    </details>
                  )) : <p className="empty-row"><span className="item-icon" aria-hidden="true"><Terminal size={16} /></span>尚无探测结果</p>}
                </div>
              </section>

              <RunSummary run={status.latestRun} />
            </>
          )}
        </div>
      </main>
    </div>
  )
}

function ToolbarStatus({ id, icon: Icon, label, status, tone, children }: {
  id: string
  icon: LucideIcon
  label: string
  status: string
  tone?: 'success' | 'warning' | 'error'
  children: ReactNode
}) {
  const description = `${label}：${status}`
  return (
    <>
      <button type="button" className="icon-button toolbar-button status-trigger" popoverTarget={id} data-state={tone} aria-label={description} title={description}>
        <Icon size={16} aria-hidden="true" />
      </button>
      <div className="status-popover panel" id={id} popover="auto" role="group" aria-labelledby={`${id}-heading`}>
        <div className="status-popover-heading">
          <h2 id={`${id}-heading`}>{label}</h2>
          <span className="item-status" data-state={tone}>{status}</span>
        </div>
        {children}
      </div>
    </>
  )
}

function DirectoryRow({ directory }: { directory: WorkDirectory }) {
  return (
    <li className="directory-row">
      <span className="item-icon" aria-hidden="true"><Folder size={16} /></span>
      <span className="item-name">
        <strong>{directory.label}</strong>
        <code title={directory.path}>{directory.path}</code>
      </span>
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
  if (!run) return (
    <div className="run-heading panel">
      <span className="item-icon" aria-hidden="true"><History size={16} /></span>
      <strong>最近运行</strong><span className="run-empty">暂无记录</span>
    </div>
  )

  const tone = run.state === 'completed' ? 'success' : run.state === 'failed' ? 'error' : run.state === 'interrupted' ? 'warning' : undefined
  return (
    <details className="run-summary panel">
      <summary className="run-heading">
        <span className="item-icon" aria-hidden="true"><History size={16} /></span>
        <strong>最近运行</strong>
        <span className="item-status" role="status" data-state={tone}>{stateLabels[run.state] ?? run.state}</span>
        <ChevronDown className="chevron" size={14} aria-hidden="true" />
      </summary>
      <div className="run-body">
        <dl>
          <div><dt>任务</dt><dd><code>{run.taskId}</code></dd></div>
          {run.sessionId && <div><dt>Agent 会话</dt><dd><code>{run.sessionId}</code></dd></div>}
          {run.pending && <div><dt>回执</dt><dd>等待中心确认，联网后自动补送</dd></div>}
        </dl>
        {run.text && <pre>{run.text}</pre>}
      </div>
    </details>
  )
}

function Capability({ supported, label }: { supported: boolean; label: string }) {
  return (
    <li data-supported={supported}>
      {supported ? <Check className="capability-check" size={13} aria-hidden="true" /> : <Minus className="capability-check" size={13} aria-hidden="true" />}
      <span className="screen-reader">{supported ? '支持' : '未确认'}</span>
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
