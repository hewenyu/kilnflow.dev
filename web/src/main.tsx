import { BaseStyles } from '@primer/react'
import { ThemeProvider } from '@primer/react/next'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './app/App'
import { embeddedPage } from './lib/api'
import './styles/global.css'

const root = document.getElementById('root')
if (!root) throw new Error('index.html 中没有 #root')

createRoot(root).render(
  <StrictMode>
    <ThemeProvider colorMode="auto" dayScheme="light" nightScheme="dark">
      <BaseStyles>
        <App initial={embeddedPage()} />
      </BaseStyles>
    </ThemeProvider>
  </StrictMode>,
)
