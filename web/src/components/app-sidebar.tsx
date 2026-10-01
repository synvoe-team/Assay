import {
  Activity,
  Cable,
  FlaskConical,
  LayoutDashboard,
  LogOut,
  PanelLeftClose,
  PanelLeftOpen,
  Settings,
} from 'lucide-react'
import { NavLink, useLocation } from 'react-router'
import { Button } from '@/components/ui/button'
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  useSidebar,
} from '@/components/ui/sidebar'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import type { CurrentUser, PermissionMap } from '@/lib/api'
import { type DictKey, useI18n } from '@/lib/i18n'

const navItems: Array<{
  key: DictKey
  url: string
  icon: typeof LayoutDashboard
  module?: keyof PermissionMap
}> = [
  { key: 'nav.overview', url: '/', icon: LayoutDashboard },
  { key: 'nav.channels', url: '/channels', icon: Cable, module: 'channels' },
  { key: 'nav.quality', url: '/quality', icon: FlaskConical, module: 'quality' },
  { key: 'nav.stability', url: '/stability', icon: Activity, module: 'stability' },
  { key: 'nav.settings', url: '/settings', icon: Settings },
]

interface AppSidebarProps {
  user: CurrentUser
  onLogout: () => void
  onOpenUpdate: () => void
}

export function AppSidebar({ user, onLogout, onOpenUpdate }: AppSidebarProps) {
  const { pathname } = useLocation()
  const { toggleSidebar } = useSidebar()
  const { t } = useI18n()

  const logo = <img src="/logo.png" alt="Assay" className="size-8 shrink-0 rounded-lg" />

  return (
    <Sidebar collapsible="icon">
      <SidebarHeader>
        <div className="flex items-center gap-2 px-2 py-1.5 group-data-[collapsible=icon]:flex-col group-data-[collapsible=icon]:px-0">
          {user.permissions.system ? (
            <Tooltip>
              <TooltipTrigger asChild>
                <button
                  type="button"
                  onClick={onOpenUpdate}
                  className="cursor-pointer rounded-lg outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  aria-label={t('update.title')}
                >
                  {logo}
                </button>
              </TooltipTrigger>
              <TooltipContent side="right">{t('update.title')}</TooltipContent>
            </Tooltip>
          ) : (
            logo
          )}
          <div className="grid leading-tight group-data-[collapsible=icon]:hidden">
            <span className="font-semibold">Assay</span>
            <span className="text-xs text-muted-foreground">{t('brand.subtitle')}</span>
          </div>
          {/* 收起按钮贴在品牌文字右侧、留在侧栏容器内；展开按钮在收起态垂直排在 logo 下方 */}
          <Button
            variant="ghost"
            size="icon"
            className="ml-auto size-7 text-muted-foreground group-data-[collapsible=icon]:hidden"
            onClick={toggleSidebar}
            title={t('sidebar.collapse')}
          >
            <PanelLeftClose />
          </Button>
          <Button
            variant="ghost"
            size="icon"
            className="hidden size-7 text-muted-foreground group-data-[collapsible=icon]:flex"
            onClick={toggleSidebar}
            title={t('sidebar.expand')}
          >
            <PanelLeftOpen />
          </Button>
        </div>
      </SidebarHeader>
      <SidebarContent>
        <SidebarGroup>
          <SidebarMenu>
            {navItems
              .filter((item) => !item.module || user.permissions[item.module])
              .map((item) => (
                <SidebarMenuItem key={item.url}>
                  <SidebarMenuButton
                    asChild
                    isActive={pathname === item.url}
                    tooltip={t(item.key)}
                  >
                    <NavLink to={item.url}>
                      <item.icon />
                      <span>{t(item.key)}</span>
                    </NavLink>
                  </SidebarMenuButton>
                </SidebarMenuItem>
              ))}
          </SidebarMenu>
        </SidebarGroup>
      </SidebarContent>
      <SidebarFooter>
        <div className="flex items-center gap-2 px-2 py-1.5 group-data-[collapsible=icon]:justify-center group-data-[collapsible=icon]:px-0">
          <div className="flex size-8 shrink-0 items-center justify-center overflow-hidden rounded-full bg-muted text-sm font-medium uppercase group-data-[collapsible=icon]:hidden">
            {/* 飞书账号用真实头像，密码账号退回首字母 */}
            {user.avatarUrl ? (
              <img src={user.avatarUrl} alt="" className="size-full object-cover" />
            ) : (
              user.username.slice(0, 1)
            )}
          </div>
          <div className="grid min-w-0 flex-1 leading-tight group-data-[collapsible=icon]:hidden">
            <span className="truncate text-sm font-medium">{user.username}</span>
            <span className="truncate text-xs text-muted-foreground">{user.role}</span>
          </div>
          <Tooltip>
            <TooltipTrigger asChild>
              <Button
                variant="ghost"
                size="icon"
                className="size-7 text-muted-foreground"
                onClick={onLogout}
                aria-label={t('sidebar.logout')}
              >
                <LogOut />
              </Button>
            </TooltipTrigger>
            <TooltipContent side="right">{t('sidebar.logout')}</TooltipContent>
          </Tooltip>
        </div>
      </SidebarFooter>
    </Sidebar>
  )
}
