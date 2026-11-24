/** Hub 拒绝当前管理员会话时抛出，界面据此返回登录页。 */
export class AdminSessionError extends Error {}

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
 * 发送同源且不使用缓存的请求，由浏览器携带管理员会话 Cookie。
 * @param path 同源 Hub API 路径。
 * @param options 请求方法、请求体及调用方取消信号等选项。
 * @param timeout 等待响应的最长时间，单位为毫秒。
 * @returns Hub 成功响应，调用方按对应接口读取响应体。
 * @throws 会话未通过认证时抛出 AdminSessionError，其他 HTTP 失败保留状态码。
 */
export async function request(
  path: string,
  options: RequestInit & { signal: AbortSignal },
  timeout = 8_000,
): Promise<Response> {
  let response: Response
  try {
    response = await fetch(path, {
      ...options,
      signal: AbortSignal.any([options.signal, AbortSignal.timeout(timeout)]),
      credentials: 'same-origin',
      cache: 'no-store',
      redirect: 'error',
    })
  } catch (error) {
    if (options.signal.aborted) throw error
    if (error instanceof DOMException && error.name === 'TimeoutError') {
      throw new Error('Hub 响应超时。')
    }
    throw new Error('无法连接 Hub，请确认服务已启动。')
  }
  if (response.status === 401) {
    throw new AdminSessionError('登录已失效，请重新登录。')
  }
  if (response.status === 403) {
    throw new HubRequestError(403, 'Hub 拒绝了此操作。')
  }
  if (response.status === 503) {
    throw new HubRequestError(503, 'Hub 暂时不可用，请检查服务配置及数据库状态。')
  }
  if (!response.ok) {
    throw new HubRequestError(response.status, `Hub 未能完成请求（HTTP ${response.status}）。`)
  }
  return response
}
