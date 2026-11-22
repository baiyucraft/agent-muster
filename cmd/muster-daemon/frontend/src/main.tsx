import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import musterIcon from '../../../../ui/muster-icon.svg'
import * as DesktopService from './bindings/github.com/baiyucraft/agent-muster/cmd/muster-daemon/desktopservice'
import { App } from './App'
import type { DeviceApi } from './types'
import '../../../../ui/token.css'
import './styles.css'

document.querySelector('link[rel="icon"]')?.setAttribute('href', musterIcon)

const api: DeviceApi = {
  getStatus: async () => {
    const status = await DesktopService.GetStatus()
    return { ...status, agents: status.agents ?? [], directories: status.directories ?? [], latestRun: status.latestRun ?? null }
  },
  pair: async (hubUrl, code) => await DesktopService.Pair(hubUrl, code),
  refreshAgents: async () => await DesktopService.RefreshAgents(),
  addWorkDirectory: async () => {
    const directory = await DesktopService.AddWorkDirectory()
    return directory.id ? directory : null
  },
  setPeerAddress: async (address) => await DesktopService.SetPeerAddress(address),
}

const root = document.getElementById('root')
if (!root) throw new Error('找不到设备状态界面的挂载节点')

createRoot(root).render(
  <StrictMode>
    <App api={api} />
  </StrictMode>,
)
