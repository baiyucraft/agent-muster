import { request } from './request'

/** 设备探测 CLI 后给出的本机可用状态。 */
export type AgentStatus = 'supported' | 'missing' | 'unsupported' | 'probe_failed'

/** 设备上报的 Agent 版本、探测状态及本机调用能力。 */
export type Agent = {
  id: 'codex' | 'dsh'
  version: string
  status: AgentStatus
  capabilities: {
    prompt: boolean
    final_reply: boolean
    resume: boolean
  }
}

/** 管理员设备清单中的登记和在线快照。尚未观察到的时间为 null。 */
export type Device = {
  id: string
  name: string
  hostname: string
  os: string
  arch: string
  registered_at: string
  last_seen_at: string | null
  probed_at: string | null
  revoked_at: string | null
  online: boolean
  agents: Agent[]
}

/** Hub 签发的一次性凭据及其失效时间。 */
export type Pairing = {
  code: string
  expires_at: string
}

/**
 * 读取 Hub 已登记的设备清单并验证列表结构。
 * @param signal 终止正在进行的清单请求。
 * @returns Hub 当前返回的设备数组，包含离线和已撤销设备。
 */
export async function readDevices(signal: AbortSignal): Promise<Device[]> {
  const response = await request('/api/v1/devices', { signal })
  const { devices } = (await response.json()) as { devices: Device[] }
  if (!Array.isArray(devices)) throw new Error('Hub 返回了无法读取的设备清单。')
  return devices
}

/**
 * 请求 Hub 为给定显示名称签发一次性配对码。
 * @param name 将显示在 Hub 清单中的设备名称。
 * @param signal 终止当前请求。
 * @returns 新配对码与过期时间。
 */
export async function createPairingCode(name: string, signal: AbortSignal): Promise<Pairing> {
  const response = await request('/api/v1/pairing-codes', {
    method: 'POST',
    body: JSON.stringify({ name }),
    headers: { 'Content-Type': 'application/json' },
    signal,
  })
  return (await response.json()) as Pairing
}

/**
 * 请求 Hub 撤销设备凭据。
 * @param deviceId 要撤销的 Hub 设备 ID。
 * @param signal 终止当前请求。
 * @returns Hub 返回成功后完成，请求失败时拒绝。
 */
export async function revokeDevice(deviceId: string, signal: AbortSignal): Promise<void> {
  await request(`/api/v1/devices/${encodeURIComponent(deviceId)}`, { method: 'DELETE', signal })
}
