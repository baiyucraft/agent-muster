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

/** Hub 拒绝当前管理员令牌时抛出，界面可据此锁定会话。 */
export class AdminTokenError extends Error {}

/** 保留 Hub HTTP 状态，供调用方区分暂不可用、游标冲突及其他错误。 */
export class HubRequestError extends Error {
  /**
   * 以 HTTP 状态与可展示描述构造请求错误。
   * @param status Hub 返回的 HTTP 状态码。
   * @param message 可展示给管理员的错误描述。
   */
  constructor(readonly status: number, message: string) {
    super(message)
  }
}

/**
 * 发送无缓存的管理员请求，并按授权状态归类失败。
 * @param token 当前管理员会话的令牌。
 * @param path 同源 Hub API 路径。
 * @param options 请求方法、请求体及调用方取消信号等选项。
 * @param timeout 等待响应的最长时间，单位为毫秒。
 * @returns Hub 成功响应，调用方按对应接口读取响应体。
 */
export async function request(
  token: string,
  path: string,
  options: RequestInit & { signal: AbortSignal },
  timeout = 8_000,
): Promise<Response> {
  const headers = new Headers(options.headers)
  headers.set('Authorization', `Bearer ${token}`)
  let response: Response
  try {
    response = await fetch(path, {
      ...options,
      signal: AbortSignal.any([options.signal, AbortSignal.timeout(timeout)]),
      headers,
      cache: 'no-store',
      redirect: 'error',
    })
  } catch (error) {
    if (error instanceof DOMException && error.name === 'TimeoutError') {
      throw new Error('Hub 响应超时。')
    }
    throw new Error('无法连接 Hub，请确认服务已启动。')
  }
  if (response.status === 401 || response.status === 403) {
    throw new AdminTokenError('管理员令牌无效，请重新输入。')
  }
  if (response.status === 503) {
    throw new HubRequestError(503, 'Hub 暂时不可用，请检查服务配置及数据库状态。')
  }
  if (!response.ok) {
    throw new HubRequestError(response.status, `Hub 未能完成请求（HTTP ${response.status}）。`)
  }
  return response
}

/**
 * 读取 Hub 已登记的设备清单并验证列表结构。
 * @param token 当前管理员会话的令牌。
 * @param signal 终止正在进行的清单请求。
 * @returns Hub 当前返回的设备数组，包含离线和已撤销设备。
 */
export async function readDevices(token: string, signal: AbortSignal): Promise<Device[]> {
  const response = await request(token, '/api/v1/devices', { signal })
  const { devices } = (await response.json()) as { devices: Device[] }
  if (!Array.isArray(devices)) throw new Error('Hub 返回了无法读取的设备清单。')
  return devices
}

/**
 * 请求 Hub 为给定显示名称签发一次性配对码。
 * @param token 当前管理员会话的令牌。
 * @param name 将显示在 Hub 清单中的设备名称。
 * @param signal 终止当前请求。
 * @returns 新配对码与过期时间。
 */
export async function createPairingCode(token: string, name: string, signal: AbortSignal): Promise<Pairing> {
  const response = await request(token, '/api/v1/pairing-codes', {
    method: 'POST',
    body: JSON.stringify({ name }),
    headers: { 'Content-Type': 'application/json' },
    signal,
  })
  return (await response.json()) as Pairing
}

/**
 * 请求 Hub 撤销设备凭据。
 * @param token 当前管理员会话的令牌。
 * @param deviceId 要撤销的 Hub 设备 ID。
 * @param signal 终止当前请求。
 * @returns Hub 返回成功后完成，请求失败时拒绝。
 */
export async function revokeDevice(token: string, deviceId: string, signal: AbortSignal): Promise<void> {
  await request(token, `/api/v1/devices/${encodeURIComponent(deviceId)}`, { method: 'DELETE', signal })
}
