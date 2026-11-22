import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import musterIcon from '../../ui/muster-icon.svg'
import '../../ui/token.css'
import { App } from './App'

document.querySelector('link[rel="icon"]')?.setAttribute('href', musterIcon)

const container = document.getElementById('root')
if (!container) {
  throw new Error('找不到工作台挂载节点')
}

createRoot(container).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
