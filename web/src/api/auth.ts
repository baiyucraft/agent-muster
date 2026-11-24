import { AdminSessionError, HubRequestError, request } from './request'

/**
 * 核对浏览器现有的管理员会话。
 * @param signal 终止当前会话检查。
 * @returns 会话有效时为 true，未登录或会话失效时为 false。
 * @throws Hub 暂不可用或网络异常时拒绝请求，调用方可以重试。
 */
export async function readAdminSession(signal: AbortSignal): Promise<boolean> {
  try {
    await request('/api/v1/auth/session', { signal })
    return true
  } catch (failure) {
    if (failure instanceof AdminSessionError) return false
    throw failure
  }
}

/**
 * 使用管理员凭据换取浏览器管理的会话 Cookie。
 * @param username 管理员用户名。
 * @param password 原样发送的管理员密码，允许包含空格。
 * @param signal 终止当前登录请求。
 * @returns Hub 确认会话创建后完成，凭据无效或请求失败时拒绝。
 */
export async function signIn(username: string, password: string, signal: AbortSignal): Promise<void> {
  try {
    await request('/api/v1/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username, password }),
      signal,
    })
  } catch (failure) {
    if (failure instanceof AdminSessionError) throw new AdminSessionError('用户名或密码错误。')
    if (failure instanceof HubRequestError && failure.status === 429) {
      throw new HubRequestError(429, '登录尝试过于频繁，请稍后再试。')
    }
    throw failure
  }
}

/**
 * 请求 Hub 撤销当前会话并清除浏览器 Cookie。
 * @param signal 终止当前退出请求。
 * @returns Hub 确认退出或当前会话已失效时完成，其他请求失败时拒绝。
 */
export async function signOut(signal: AbortSignal): Promise<void> {
  try {
    await request('/api/v1/auth/logout', { method: 'POST', signal })
  } catch (failure) {
    if (!(failure instanceof AdminSessionError)) throw failure
  }
}
