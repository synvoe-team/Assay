import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createBrowserRouter, RouterProvider } from 'react-router'
import '@fontsource-variable/inter'
import './index.css'
import { AppLayout } from '@/layouts/app-layout'
import { I18nProvider } from '@/lib/i18n'
import { applyStoredTheme } from '@/lib/theme'
import ChannelDetailPage from '@/pages/channel-detail'
import ChannelsPage from '@/pages/channels'
import DashboardPage from '@/pages/dashboard'
import LoginPage from '@/pages/login'
import QualityPage from '@/pages/quality'
import QualityTaskDetailPage from '@/pages/quality-task-detail'
import SettingsPage from '@/pages/settings'
import StabilityPage from '@/pages/stability'
import StabilityTaskDetailPage from '@/pages/stability-task-detail'

// 渲染前恢复持久化主题，避免暗色用户首屏闪白
applyStoredTheme()

const router = createBrowserRouter([
  { path: '/login', Component: LoginPage },
  {
    path: '/',
    Component: AppLayout,
    children: [
      { index: true, Component: DashboardPage },
      { path: 'channels', Component: ChannelsPage },
      { path: 'channels/:id', Component: ChannelDetailPage },
      { path: 'quality', Component: QualityPage },
      { path: 'quality/:taskId', Component: QualityTaskDetailPage },
      { path: 'stability', Component: StabilityPage },
      { path: 'stability/:taskId', Component: StabilityTaskDetailPage },
      { path: 'settings', Component: SettingsPage },
    ],
  },
])

const queryClient = new QueryClient()

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <I18nProvider>
        <RouterProvider router={router} />
      </I18nProvider>
    </QueryClientProvider>
  </StrictMode>,
)
