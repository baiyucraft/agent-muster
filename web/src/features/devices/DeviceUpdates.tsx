import { useEffect, useRef, useState, type FormEvent } from 'react'
import { AdminTokenError, HubRequestError, request } from '../../api/devices'

const labels: Record<string, string> = {
  idle: '暂无指派', pending: '等待设备领取', downloading: '下载并验证', waiting: '等待工作完成',
  applying: '正在更新', succeeded: '已更新', failed: '更新失败', rolled_back: '已回滚',
}

type DeviceUpdate = {
  device_id: string
  /** 设备最近上报的版本。首次上报前为空字符串。 */
  current_version: string
  /** 当前指派。Hub 尚未为设备指派更新时为 null。 */
  target: { id: string; version: string; manifest_url: string } | null
  status: string
  /** 最近一次设备报告的失败原因，缺省表示当前没有错误详情。 */
  error?: string
}

export function DeviceUpdates({ token, deviceID, onLock }: { token: string; deviceID: string; onLock: (message: string) => void }) {
  const [item, setItem] = useState<DeviceUpdate>()
  const [version, setVersion] = useState('')
  const [manifestURL, setManifestURL] = useState('')
  const [loadError, setLoadError] = useState('')
  const [actionError, setActionError] = useState('')
  const [saving, setSaving] = useState(false)
  const [revision, setRevision] = useState(0)
  const action = useRef<AbortController | null>(null)
  const details = useRef<HTMLDetailsElement>(null)

  useEffect(() => () => action.current?.abort(), [])
  useEffect(() => {
    const controller = new AbortController()
    let timer: number | undefined
    async function load() {
      try {
        const response = await request(token, '/api/v1/device-updates', { signal: controller.signal })
        const { devices } = (await response.json()) as { devices: DeviceUpdate[] }
        if (controller.signal.aborted) return
        setItem(devices.find((device) => device.device_id === deviceID))
        setLoadError('')
      } catch (error) {
        if (controller.signal.aborted) return
        if (error instanceof AdminTokenError) {
          onLock(error.message)
          return
        }
        setLoadError(error instanceof HubRequestError && error.status === 404 ? '当前 Hub 暂不支持设备更新。' : '暂时无法获取更新状态。')
      }
      timer = window.setTimeout(load, 5_000)
    }
    void load()
    return () => {
      controller.abort()
      window.clearTimeout(timer)
    }
  }, [token, deviceID, onLock, revision])

  async function assign(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (action.current) return
    const controller = new AbortController()
    action.current = controller
    setSaving(true)
    setActionError('')
    try {
      const url = manifestURL.trim()
      const targetVersion = version.trim()
      // 同一版本和清单进入失败或回滚状态后，重新提交时显式申请重试。
      const retry = item?.target?.version === targetVersion && item.target.manifest_url === url &&
        (item.status === 'failed' || item.status === 'rolled_back')
      const response = await request(token, `/api/v1/device-updates/${encodeURIComponent(deviceID)}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ version: targetVersion, manifest_url: url, retry }),
        signal: controller.signal,
      })
      if (controller.signal.aborted) return
      if (response.status !== 204) setItem((await response.json()) as DeviceUpdate)
      setRevision((value) => value + 1)
      if (details.current) details.current.open = false
    } catch (error) {
      if (controller.signal.aborted) return
      if (error instanceof AdminTokenError) onLock(error.message)
      else if (error instanceof HubRequestError && error.status === 409) setActionError('此版本无法指派。请检查当前版本和更新状态。')
      else setActionError('未能指派更新。请检查版本格式、清单地址和 Hub 状态。')
    } finally {
      action.current = null
      if (!controller.signal.aborted) setSaving(false)
    }
  }

  return (
    <section className="device-update" aria-labelledby="device-update-heading">
      <header className="device-agents-heading">
        <h3 id="device-update-heading">设备端更新</h3>
        {item && <span role="status">{labels[item.status] ?? item.status}</span>}
      </header>
      {loadError && <p className="device-update-note" role="status">{loadError}</p>}
      {item && <div className="device-update-summary">
        <span>当前版本 <code>{item.current_version || '待上报'}</code></span>
        {item.target && <span>目标版本 <code>{item.target.version}</code></span>}
      </div>}
      {item?.error && <p className="device-update-note" role="status">{item.error}</p>}
      {item && item.status !== 'applying' && <details ref={details}>
        <summary className="quiet-button" onClick={() => {
          if (details.current?.open) return
          setActionError('')
          setVersion(item.target?.version ?? '')
          setManifestURL(item.target?.manifest_url ?? '')
        }}>指派更新</summary>
        <form className="device-update-form" onSubmit={assign}>
          <label>目标版本 <input value={version} onChange={(event) => setVersion(event.target.value)} placeholder="0.2.1" maxLength={30} required /></label>
          <label>签名清单 URL <input type="url" value={manifestURL} onChange={(event) => setManifestURL(event.target.value)} maxLength={2048} required /></label>
          <button className="primary-button" type="submit" disabled={saving}>{saving ? '正在指派' : '指派到此设备'}</button>
          {actionError && <p className="read-error" role="alert">{actionError}</p>}
        </form>
      </details>}
    </section>
  )
}
