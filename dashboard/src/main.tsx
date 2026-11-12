import './index.css'
import { QueryClientProvider } from '@tanstack/react-query'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router'
import { App } from './app.tsx'
import { ConfirmProvider } from './components/confirm.tsx'
import { Toaster } from './components/ui/toast.tsx'
import { TooltipProvider } from './components/ui/tooltip.tsx'
import { queryClient } from './queries.ts'

const root = document.getElementById('root')
if (root) {
  createRoot(root).render(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <BrowserRouter>
          <TooltipProvider>
            <Toaster>
              <ConfirmProvider>
                <App />
              </ConfirmProvider>
            </Toaster>
          </TooltipProvider>
        </BrowserRouter>
      </QueryClientProvider>
    </StrictMode>,
  )
}
