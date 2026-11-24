import { request } from './request'
import type { RunUsage } from './workspace'

/** 保存同一 CLI 数据来源的用量总和与各可选字段的覆盖。Token 用十进制字符串表示。 */
export type UsageSource = {
  source: RunUsage['source'] | 'dsh_center_turn'
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

/** 汇总已结束的设备运行或中心 dsh 投递，以及可取得的 Token 用量。 */
export type UsageGroup = {
  id: string
  name: string
  finished_runs: number
  measured_runs: number
  sources: UsageSource[]
}

/** 按项目与设备汇总设备运行，支持中心统计的 Hub 还会返回中心 dsh 投递用量。 */
export type DeviceUsage = { projects: UsageGroup[]; devices: UsageGroup[]; center?: UsageGroup }

/**
 * 读取管理员当前可查的 Token 用量。
 * @param signal 终止用量请求。
 * @returns 设备运行与中心 dsh 投递的用量和覆盖轮数。旧版 Hub 可能缺少中心统计。
 */
export async function readDeviceUsage(signal: AbortSignal): Promise<DeviceUsage> {
  const response = await request('/api/v1/workspace/usage', { signal })
  return (await response.json()) as DeviceUsage
}
