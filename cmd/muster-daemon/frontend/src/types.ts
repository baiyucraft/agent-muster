/** 设备端展示所需的本地 CLI 探测结果及能力标记。 */
export type Agent = {
  id: string
  version: string
  status: 'supported' | 'missing' | 'unsupported' | 'probe_failed' | string
  capabilities: {
    prompt: boolean
    final_reply: boolean
    resume: boolean
  }
}

/** Wails 查询返回的最新本地配对、执行和更新状态。 */
export type DeviceStatus = {
  paired: boolean
  hubUrl: string
  deviceId: string
  connection: string
  lastHeartbeat: string | null
  agents: Agent[]
  probedAt: string | null
  lastError: string
  directories: WorkDirectory[]
  latestRun: RunStatus | null
  workError: string
  peerAddress: string
  peerTransport: string
  version: string
  /** 当前连接最近一次观测到的更新进度或结果。尚无观测时为 null。 */
  update: { version: string; state: string; error?: string } | null
}

/** 在设备端登记的可派发目录，含实际本机路径。 */
export type WorkDirectory = { id: string; label: string; path: string }
/** 桌面端最近一次运行的本地状态及原生会话引用。 */
export type RunStatus = { id: string; taskId: string; state: string; sessionId: string; text: string; pending: boolean }

/** 设备工作台调用的 Wails 桥接契约，异步错误交由调用方处理。 */
export type DeviceApi = {
  /**
   * 读取本地设备服务的状态快照。
   * @returns 当前设备快照。
   */
  getStatus(): Promise<DeviceStatus>
  /**
   * 将本机设备与 Hub 配对。
   * @param hubUrl 用于管理该设备的 Hub 地址。
   * @param code 管理员签发的一次性配对码。
   * @returns 配对成功后完成，连接或凭据校验失败时拒绝。
   */
  pair(hubUrl: string, code: string): Promise<void>
  /**
   * 要求设备重新探测本机 Agent。
   * @returns Hub 确认新的探测清单后完成，探测清单未能上报时拒绝。
   */
  refreshAgents(): Promise<void>
  /**
   * 打开原生目录选择器并将所选目录登记到本机。
   * @returns 登记后的目录，用户取消时为 null。
   */
  addWorkDirectory(): Promise<WorkDirectory | null>
  /**
   * 更新设备服务用于互联的对端地址设置。
   * @param address 设备间交接使用的地址，空字符串清除地址并恢复自动互联偏好。
   * @returns 地址设置保存到本机后完成，设备服务拒绝时返回失败。
   */
  setPeerAddress(address: string): Promise<void>
}
