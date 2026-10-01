import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Loader2, Pencil, Plus, Trash2 } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import {
  authApi,
  RequestError,
  rolesApi,
  usersApi,
  type PermissionMap,
  type Role,
  type User,
} from '@/lib/api'
import { type DictKey, useI18n } from '@/lib/i18n'

const permKeys = ['channels', 'quality', 'stability', 'users', 'system'] as const

function errText(err: unknown): string {
  return err instanceof RequestError ? err.message : String(err)
}

// ---------- 个人设置：改密码 ----------

function PersonalSection() {
  const { t } = useI18n()
  const [mismatch, setMismatch] = useState(false)

  const change = useMutation({ mutationFn: (v: { current: string; next: string }) =>
    authApi.changePassword(v.current, v.next),
  })

  function handleSubmit(e: React.SubmitEvent<HTMLFormElement>) {
    e.preventDefault()
    const form = e.currentTarget
    const data = new FormData(form)
    const next = String(data.get('newPassword') ?? '')
    if (next !== String(data.get('confirmPassword') ?? '')) {
      setMismatch(true)
      return
    }
    setMismatch(false)
    change.mutate(
      { current: String(data.get('currentPassword') ?? ''), next },
      { onSuccess: () => form.reset() },
    )
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('personal.title')}</CardTitle>
        <CardDescription>{t('personal.desc')}</CardDescription>
      </CardHeader>
      <CardContent>
        <form className="grid max-w-sm gap-4" onSubmit={handleSubmit}>
          <div className="grid gap-2">
            <Label htmlFor="currentPassword">{t('personal.currentPassword')}</Label>
            <Input
              id="currentPassword"
              name="currentPassword"
              type="password"
              autoComplete="current-password"
              required
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="newPassword">{t('personal.newPassword')}</Label>
            <Input
              id="newPassword"
              name="newPassword"
              type="password"
              autoComplete="new-password"
              minLength={8}
              required
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="confirmPassword">{t('personal.confirmPassword')}</Label>
            <Input
              id="confirmPassword"
              name="confirmPassword"
              type="password"
              autoComplete="new-password"
              minLength={8}
              required
            />
          </div>
          {mismatch && <p className="text-sm text-destructive">{t('personal.mismatch')}</p>}
          {change.isError && <p className="text-sm text-destructive">{errText(change.error)}</p>}
          {change.isSuccess && <p className="text-sm text-green-600">{t('personal.success')}</p>}
          <div>
            <Button type="submit" disabled={change.isPending}>
              {change.isPending && <Loader2 className="animate-spin" />}
              {t('personal.submit')}
            </Button>
          </div>
        </form>
      </CardContent>
    </Card>
  )
}

// ---------- 用户管理 ----------

function UsersSection({ selfId, roles }: { selfId: string; roles: Role[] }) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [createOpen, setCreateOpen] = useState(false)
  const [editing, setEditing] = useState<User | null>(null)
  const [deleting, setDeleting] = useState<User | null>(null)

  const users = useQuery({ queryKey: ['users'], queryFn: usersApi.list })

  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['users'] })

  const create = useMutation({
    mutationFn: usersApi.create,
    onSuccess: () => {
      invalidate()
      setCreateOpen(false)
    },
  })
  const update = useMutation({
    mutationFn: (v: { id: string; roleId?: string; password?: string }) =>
      usersApi.update(v.id, { roleId: v.roleId, password: v.password }),
    onSuccess: () => {
      invalidate()
      setEditing(null)
    },
  })
  const remove = useMutation({
    mutationFn: usersApi.remove,
    onSuccess: () => {
      invalidate()
      setDeleting(null)
    },
  })

  function handleCreate(e: React.SubmitEvent<HTMLFormElement>) {
    e.preventDefault()
    const data = new FormData(e.currentTarget)
    create.mutate({
      username: String(data.get('username') ?? ''),
      password: String(data.get('password') ?? ''),
      roleId: String(data.get('roleId') ?? ''),
    })
  }

  function handleEdit(e: React.SubmitEvent<HTMLFormElement>) {
    e.preventDefault()
    if (!editing) return
    const data = new FormData(e.currentTarget)
    const roleId = String(data.get('roleId') ?? '')
    const password = String(data.get('password') ?? '')
    update.mutate({
      id: editing.id,
      // 自己的角色不可改（防自锁），只提交密码重置
      roleId: editing.id === selfId || roleId === editing.roleId ? undefined : roleId,
      password: password || undefined,
    })
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('users.title')}</CardTitle>
        <CardDescription>{t('users.desc')}</CardDescription>
        <CardAction>
          <Button size="sm" onClick={() => setCreateOpen(true)}>
            <Plus />
            {t('users.create')}
          </Button>
        </CardAction>
      </CardHeader>
      <CardContent>
        {users.isPending ? (
          <p className="text-sm text-muted-foreground">{t('common.loading')}</p>
        ) : users.isError ? (
          <p className="text-sm text-destructive">{errText(users.error)}</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t('users.username')}</TableHead>
                <TableHead>{t('users.role')}</TableHead>
                <TableHead>{t('users.authSource')}</TableHead>
                <TableHead>{t('users.createdAt')}</TableHead>
                <TableHead className="w-24 text-right">{t('users.actions')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {users.data.map((u) => (
                <TableRow key={u.id}>
                  <TableCell className="font-medium">{u.username}</TableCell>
                  <TableCell>{u.roleName}</TableCell>
                  <TableCell className="text-muted-foreground">
                    {u.authSource === 'feishu' ? t('users.authFeishu') : t('users.authPassword')}
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {new Date(u.createdAt).toLocaleString()}
                  </TableCell>
                  <TableCell className="text-right">
                    <Button
                      variant="ghost"
                      size="icon"
                      className="size-7"
                      onClick={() => setEditing(u)}
                      aria-label={t('common.edit')}
                    >
                      <Pencil />
                    </Button>
                    <Button
                      variant="ghost"
                      size="icon"
                      className="size-7 text-destructive"
                      disabled={u.id === selfId}
                      onClick={() => setDeleting(u)}
                      aria-label={t('common.delete')}
                    >
                      <Trash2 />
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>

      {/* 新建用户 */}
      <Dialog open={createOpen} onOpenChange={(o) => { setCreateOpen(o); create.reset() }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t('users.create')}</DialogTitle>
          </DialogHeader>
          <form className="grid gap-4" onSubmit={handleCreate}>
            <div className="grid gap-2">
              <Label htmlFor="create-username">{t('users.username')}</Label>
              <Input id="create-username" name="username" maxLength={64} required />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="create-password">{t('users.password')}</Label>
              <Input
                id="create-password"
                name="password"
                type="password"
                autoComplete="new-password"
                minLength={8}
                required
              />
            </div>
            <div className="grid gap-2">
              <Label>{t('users.role')}</Label>
              <RoleSelect name="roleId" roles={roles} defaultValue={roles.find((r) => r.name === 'member')?.id ?? roles[0]?.id} />
            </div>
            {create.isError && <p className="text-sm text-destructive">{errText(create.error)}</p>}
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => setCreateOpen(false)}>
                {t('common.cancel')}
              </Button>
              <Button type="submit" disabled={create.isPending}>
                {create.isPending && <Loader2 className="animate-spin" />}
                {t('common.create')}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {/* 编辑用户：改角色 / 重置密码 */}
      <Dialog open={editing !== null} onOpenChange={(o) => { if (!o) { setEditing(null); update.reset() } }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>
              {t('users.editTitle')} — {editing?.username}
            </DialogTitle>
          </DialogHeader>
          {editing && (
            <form className="grid gap-4" onSubmit={handleEdit}>
              <div className="grid gap-2">
                <Label>{t('users.role')}</Label>
                <RoleSelect
                  name="roleId"
                  roles={roles}
                  defaultValue={editing.roleId}
                  disabled={editing.id === selfId}
                />
              </div>
              {editing.authSource === 'feishu' ? (
                <p className="text-sm text-muted-foreground">{t('users.feishuNoPassword')}</p>
              ) : (
                <div className="grid gap-2">
                  <Label htmlFor="edit-password">{t('users.resetPassword')}</Label>
                  <Input
                    id="edit-password"
                    name="password"
                    type="password"
                    autoComplete="new-password"
                    minLength={8}
                  />
                </div>
              )}
              {update.isError && <p className="text-sm text-destructive">{errText(update.error)}</p>}
              <DialogFooter>
                <Button type="button" variant="outline" onClick={() => setEditing(null)}>
                  {t('common.cancel')}
                </Button>
                <Button type="submit" disabled={update.isPending}>
                  {update.isPending && <Loader2 className="animate-spin" />}
                  {t('common.save')}
                </Button>
              </DialogFooter>
            </form>
          )}
        </DialogContent>
      </Dialog>

      {/* 删除确认 */}
      <Dialog open={deleting !== null} onOpenChange={(o) => { if (!o) { setDeleting(null); remove.reset() } }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>
              {t('users.deleteTitle')} — {deleting?.username}
            </DialogTitle>
            <DialogDescription>{t('users.deleteDesc')}</DialogDescription>
          </DialogHeader>
          {remove.isError && <p className="text-sm text-destructive">{errText(remove.error)}</p>}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)}>
              {t('common.cancel')}
            </Button>
            <Button
              variant="destructive"
              disabled={remove.isPending}
              onClick={() => deleting && remove.mutate(deleting.id)}
            >
              {remove.isPending && <Loader2 className="animate-spin" />}
              {t('common.delete')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  )
}

function RoleSelect({
  name,
  roles,
  defaultValue,
  disabled,
}: {
  name: string
  roles: Role[]
  defaultValue?: string
  disabled?: boolean
}) {
  return (
    <Select name={name} defaultValue={defaultValue} disabled={disabled}>
      <SelectTrigger>
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {roles.map((r) => (
          <SelectItem key={r.id} value={r.id}>
            {r.name}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}

// ---------- 角色管理 ----------

function RoleFormDialog({
  open,
  title,
  initial,
  pending,
  error,
  onClose,
  onSubmit,
}: {
  open: boolean
  title: string
  initial?: Role
  pending: boolean
  error: unknown
  onClose: () => void
  onSubmit: (name: string, permissions: PermissionMap) => void
}) {
  const { t } = useI18n()

  function handleSubmit(e: React.SubmitEvent<HTMLFormElement>) {
    e.preventDefault()
    const data = new FormData(e.currentTarget)
    const permissions = Object.fromEntries(
      permKeys.map((k) => [k, data.get(`perm-${k}`) === 'on']),
    ) as unknown as PermissionMap
    onSubmit(String(data.get('name') ?? ''), permissions)
  }

  return (
    <Dialog open={open} onOpenChange={(o) => { if (!o) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
        </DialogHeader>
        {/* key 强制在切换编辑对象时重置非受控表单默认值 */}
        <form key={initial?.id ?? 'new'} className="grid gap-4" onSubmit={handleSubmit}>
          <div className="grid gap-2">
            <Label htmlFor="role-name">{t('roles.name')}</Label>
            <Input id="role-name" name="name" maxLength={64} defaultValue={initial?.name} required />
          </div>
          <div className="grid gap-3">
            <Label>{t('roles.permissions')}</Label>
            {permKeys.map((k) => (
              <div key={k} className="flex items-center justify-between">
                <span className="text-sm">{t(`perm.${k}` as DictKey)}</span>
                <Switch name={`perm-${k}`} defaultChecked={initial?.permissions[k] ?? false} />
              </div>
            ))}
          </div>
          {error != null && <p className="text-sm text-destructive">{errText(error)}</p>}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={onClose}>
              {t('common.cancel')}
            </Button>
            <Button type="submit" disabled={pending}>
              {pending && <Loader2 className="animate-spin" />}
              {initial ? t('common.save') : t('common.create')}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function RolesSection({ roles }: { roles: Role[] }) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [createOpen, setCreateOpen] = useState(false)
  const [editing, setEditing] = useState<Role | null>(null)
  const [deleting, setDeleting] = useState<Role | null>(null)

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: ['roles'] })
    queryClient.invalidateQueries({ queryKey: ['users'] })
  }

  const create = useMutation({
    mutationFn: rolesApi.create,
    onSuccess: () => {
      invalidate()
      setCreateOpen(false)
    },
  })
  const update = useMutation({
    mutationFn: (v: { id: string; name: string; permissions: PermissionMap }) =>
      rolesApi.update(v.id, { name: v.name, permissions: v.permissions }),
    onSuccess: () => {
      invalidate()
      setEditing(null)
    },
  })
  const remove = useMutation({
    mutationFn: rolesApi.remove,
    onSuccess: () => {
      invalidate()
      setDeleting(null)
    },
  })

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('roles.title')}</CardTitle>
        <CardDescription>{t('roles.desc')}</CardDescription>
        <CardAction>
          <Button size="sm" onClick={() => setCreateOpen(true)}>
            <Plus />
            {t('roles.create')}
          </Button>
        </CardAction>
      </CardHeader>
      <CardContent>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t('roles.name')}</TableHead>
              <TableHead>{t('roles.permissions')}</TableHead>
              <TableHead className="w-24 text-right">{t('users.actions')}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {roles.map((r) => (
              <TableRow key={r.id}>
                <TableCell className="font-medium">
                  {r.name}
                  {r.builtIn && (
                    <Badge variant="secondary" className="ml-2">
                      {t('roles.builtIn')}
                    </Badge>
                  )}
                </TableCell>
                <TableCell>
                  <div className="flex flex-wrap gap-1">
                    {permKeys
                      .filter((k) => r.permissions[k])
                      .map((k) => (
                        <Badge key={k} variant="outline">
                          {t(`perm.${k}` as DictKey)}
                        </Badge>
                      ))}
                  </div>
                </TableCell>
                <TableCell className="text-right">
                  <Button
                    variant="ghost"
                    size="icon"
                    className="size-7"
                    disabled={r.builtIn}
                    onClick={() => setEditing(r)}
                    aria-label={t('common.edit')}
                  >
                    <Pencil />
                  </Button>
                  <Button
                    variant="ghost"
                    size="icon"
                    className="size-7 text-destructive"
                    disabled={r.builtIn}
                    onClick={() => setDeleting(r)}
                    aria-label={t('common.delete')}
                  >
                    <Trash2 />
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </CardContent>

      <RoleFormDialog
        open={createOpen}
        title={t('roles.create')}
        pending={create.isPending}
        error={create.isError ? create.error : null}
        onClose={() => { setCreateOpen(false); create.reset() }}
        onSubmit={(name, permissions) => create.mutate({ name, permissions })}
      />
      <RoleFormDialog
        open={editing !== null}
        title={t('roles.editTitle')}
        initial={editing ?? undefined}
        pending={update.isPending}
        error={update.isError ? update.error : null}
        onClose={() => { setEditing(null); update.reset() }}
        onSubmit={(name, permissions) =>
          editing && update.mutate({ id: editing.id, name, permissions })}
      />

      <Dialog open={deleting !== null} onOpenChange={(o) => { if (!o) { setDeleting(null); remove.reset() } }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>
              {t('roles.deleteTitle')} — {deleting?.name}
            </DialogTitle>
            <DialogDescription>{t('roles.deleteDesc')}</DialogDescription>
          </DialogHeader>
          {remove.isError && <p className="text-sm text-destructive">{errText(remove.error)}</p>}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)}>
              {t('common.cancel')}
            </Button>
            <Button
              variant="destructive"
              disabled={remove.isPending}
              onClick={() => deleting && remove.mutate(deleting.id)}
            >
              {remove.isPending && <Loader2 className="animate-spin" />}
              {t('common.delete')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </Card>
  )
}

// ---------- 页面 ----------

export default function SettingsPage() {
  const { t } = useI18n()
  const { data: me } = useQuery({
    queryKey: ['auth', 'me'],
    queryFn: authApi.me,
    retry: false,
    staleTime: 5 * 60_000,
  })
  const canManageUsers = me?.permissions.users ?? false
  const roles = useQuery({
    queryKey: ['roles'],
    queryFn: rolesApi.list,
    enabled: canManageUsers,
  })

  return (
    <div className="grid gap-6">
      <div>
        <h1 className="text-2xl font-semibold">{t('nav.settings')}</h1>
        <p className="mt-1 text-sm text-muted-foreground">{t('settings.desc')}</p>
      </div>
      <PersonalSection />
      {canManageUsers && me && (
        <>
          <UsersSection selfId={me.id} roles={roles.data ?? []} />
          <RolesSection roles={roles.data ?? []} />
        </>
      )}
    </div>
  )
}
