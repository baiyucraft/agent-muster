import { request } from './devices'
import type { RunUsage } from './workspace'

/** 保存同一 CLI 数据来源的用量总和与各可选字段的覆盖。Token 用十进制字符串表示。 */
export type UsageSource = {
  source: RunUsage['source']
  measured_runs: number
  input_tokens: string
  output_tokens: string
  cached_input_tokens?: string
  cached_input_runs: number
  cache_write_input_tokens?: string
  cache_write_input_runs: number
  reasoning_output_tokens?: string
  reasoning_output_runs: number
  total_tokens?: string
  total_runs: number
}

/** 汇总一个项目或设备已结束的运行与可取得的 Token 用量。 */
export type UsageGroup = {
  id: string
  name: string
  finished_runs: number
  measured_runs: number
  sources: UsageSource[]
}

/** 以项目和设备两个维度返回同一批设备运行。 */
export type DeviceUsage = { projects: UsageGroup[]; devices: UsageGroup[] }

/**
 * 读取管理员当前可查的设备 Token 用量。
 * @param token 当前管理员会话令牌。
 * @param signal 终止用量请求。
 * @returns 分项目与设备汇总的用量和覆盖轮数。
 */
export async function readDeviceUsage(token: string, signal: AbortSignal): Promise<DeviceUsage> {
  const response = await request(token, '/api/v1/workspace/usage', { signal })
  return (await response.json()) as DeviceUsage
}
