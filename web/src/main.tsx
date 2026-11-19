import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'

const container = document.getElementById('root')
if (!container) {
  throw new Error('找不到工作台挂载节点')
}

createRoot(container).render(
  <StrictMode>
    <main>
      <h1>Agent Muster 工作台</h1>
      <p>Web 工作区已初始化。</p>
    </main>
  </StrictMode>,
)
