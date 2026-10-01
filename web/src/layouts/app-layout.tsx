import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Languages, Moon, ShieldX, Sun } from 'lucide-react'
import { Navigate, Outlet, useLocation, useNavigate } from 'react-router'
import { AppSidebar } from '@/components/app-sidebar'
import { UpdateDialog } from '@/components/update-dialog'
import { Button } from '@/components/ui/button'
import { SidebarInset, SidebarProvider } from '@/components/ui/sidebar'
import { TooltipProvider } from '@/components/ui/tooltip'
import { authApi, type PermissionMap } from '@/lib/api'
import { type DictKey, useI18n } from '@/lib/i18n'
import { isDark, toggleTheme } from '@/lib/theme'

const pageTitleKeys: Record<string, DictKey> = {
  '/': 'nav.overview',
  '/channels': 'nav.channels',
  '/quality': 'nav.quality',
  '/stability': 'nav.stability',
  '/settings': 'nav.settings',
}

// 页面级权限门禁：路径 → 所需模块权限（总览与设置对所有登录用户开放）
const pathModule: Record<string, keyof PermissionMap> = {
  '/channels': 'channels',
  '/quality': 'quality',
  '/stability': 'stability',
}

// 标题与门禁都按首段路径匹配，子路由（如 /channels/:id）继承所属模块
function topPath(pathname: string): string {
  return '/' + (pathname.split('/')[1] ?? '')
}

// lucide 已移除品牌图标，GitHub mark 用官方 SVG path 内联
function GithubIcon() {
  return (
    <svg viewBox="0 0 16 16" fill="currentColor" className="size-4" aria-hidden="true">
      <path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27s1.36.09 2 .27c1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8Z" />
    </svg>
  )
}

function Forbidden() {
  const { t } = useI18n()
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-2 py-24 text-center">
      <ShieldX className="size-10 text-muted-foreground" />
      <h1 className="text-xl font-semibold">{t('forbidden.title')}</h1>
      <p className="text-sm text-muted-foreground">{t('forbidden.desc')}</p>
    </div>
  )
}

export function AppLayout() {
  const { pathname } = useLocation()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { t, lang, setLang } = useI18n()
  const [dark, setDark] = useState(isDark)
  const [updateOpen, setUpdateOpen] = useState(false)

  // 路由守卫：未登录（/auth/me 401）重定向到登录页，并带上原本想去的路径，
  // 登录完直接回那儿——开了自动跳转后深链接会很常见，不能一律甩回首页
  const { data: user, isPending, isError } = useQuery({
    queryKey: ['auth', 'me'],
    queryFn: authApi.me,
    retry: false,
    staleTime: 5 * 60_000,
  })

  if (isPending) return null
  if (isError || !user) {
    return <Navigate to={`/login?next=${encodeURIComponent(pathname + window.location.search)}`} replace />
  }

  async function handleLogout() {
    await authApi.logout()
    queryClient.clear()
    // manual=1 抑制自动跳转：否则刚退出就被静默登回来，「退出」等于没反应
    navigate('/login?manual=1', { replace: true })
  }

  const requiredModule = pathModule[topPath(pathname)]
  const allowed = !requiredModule || user.permissions[requiredModule]

  return (
    <TooltipProvider>
      <SidebarProvider>
        <AppSidebar
          user={user}
          onLogout={handleLogout}
          onOpenUpdate={() => setUpdateOpen(true)}
        />
        <SidebarInset>
          <header className="flex h-14 shrink-0 items-center gap-2 border-b px-4">
            <span className="text-sm font-medium">
              {pageTitleKeys[topPath(pathname)] ? t(pageTitleKeys[topPath(pathname)]) : 'Assay'}
            </span>
            <div className="ml-auto flex items-center gap-1">
              <Button variant="ghost" size="icon" className="size-8" asChild>
                <a
                  href="https://github.com/Yukiho0287/Assay"
                  target="_blank"
                  rel="noreferrer"
                  title={t('header.github')}
                >
                  <GithubIcon />
                </a>
              </Button>
              <Button
                variant="ghost"
                size="icon"
                className="size-8"
                onClick={() => setDark(toggleTheme())}
                title={t('header.theme')}
              >
                {dark ? <Sun /> : <Moon />}
              </Button>
              <Button
                variant="ghost"
                size="sm"
                className="h-8 gap-1 px-2"
                onClick={() => setLang(lang === 'zh' ? 'en' : 'zh')}
                title={t('header.lang')}
              >
                <Languages />
                <span className="text-xs">{lang === 'zh' ? 'EN' : '中'}</span>
              </Button>
            </div>
          </header>
          <main className="flex flex-1 flex-col gap-4 p-6">
            {allowed ? <Outlet /> : <Forbidden />}
          </main>
        </SidebarInset>
        {user.permissions.system && (
          <UpdateDialog open={updateOpen} onOpenChange={setUpdateOpen} />
        )}
      </SidebarProvider>
    </TooltipProvider>
  )
}
