import type { components } from '@/api/schema'

export type CurrentUser = components['schemas']['CurrentUser']
export type AuthMethods = components['schemas']['AuthMethods']
export type AuthSource = components['schemas']['AuthSource']
export type VersionInfo = components['schemas']['VersionInfo']
export type PermissionMap = components['schemas']['PermissionMap']
export type User = components['schemas']['User']
export type UserCreate = components['schemas']['UserCreate']
export type UserUpdate = components['schemas']['UserUpdate']
export type Role = components['schemas']['Role']
export type RoleCreate = components['schemas']['RoleCreate']
export type RoleUpdate = components['schemas']['RoleUpdate']
export type UpdateStatus = components['schemas']['UpdateStatus']
export type ApiError = components['schemas']['Error']
export type Channel = components['schemas']['Channel']
export type ChannelDetail = components['schemas']['ChannelDetail']
export type ChannelCreate = components['schemas']['ChannelCreate']
export type ChannelUpdate = components['schemas']['ChannelUpdate']
export type ModelEntry = components['schemas']['ModelEntry']
export type ModelEntryUpsert = components['schemas']['ModelEntryUpsert']
export type ConnectivityTest = components['schemas']['ConnectivityTest']
export type ConnectivityResult = components['schemas']['ConnectivityResult']
export type Protocol = components['schemas']['Protocol']
export type Currency = components['schemas']['Currency']
export type ProbeInfo = components['schemas']['ProbeInfo']
export type QualityTask = components['schemas']['QualityTask']
export type QualityTaskCreate = components['schemas']['QualityTaskCreate']
export type QualityTaskList = components['schemas']['QualityTaskList']
export type QualityCaseResult = components['schemas']['QualityCaseResult']
export type TaskStatus = components['schemas']['TaskStatus']
export type CaseStatus = components['schemas']['CaseStatus']
export type CaseMode = components['schemas']['CaseMode']
export type TaskStatBucket = components['schemas']['TaskStatBucket']
export type TaskProgressEvent = components['schemas']['TaskProgressEvent']
export type QualityReport = components['schemas']['QualityReport']
export type ProbeScore = components['schemas']['ProbeScore']
export type CheckpointScore = components['schemas']['CheckpointScore']
export type OverviewChannel = components['schemas']['OverviewChannel']
export type OverviewModelScore = components['schemas']['OverviewModelScore']
export type ConnectivityHistoryPoint = components['schemas']['ConnectivityHistoryPoint']
export type StabilityProbeInfo = components['schemas']['StabilityProbeInfo']
export type StabilityTask = components['schemas']['StabilityTask']
export type StabilityTaskCreate = components['schemas']['StabilityTaskCreate']
export type StabilityTaskList = components['schemas']['StabilityTaskList']
export type StabilityTaskParams = components['schemas']['StabilityTaskParams']
export type StabilityReport = components['schemas']['StabilityReport']
export type StabilityStageMetric = components['schemas']['StabilityStageMetric']
export type StabilityMetrics = components['schemas']['StabilityMetrics']
export type StabilityPercentiles = components['schemas']['StabilityPercentiles']
export type StabilityWorkload = components['schemas']['StabilityWorkload']
export type StabilityDeviation = components['schemas']['StabilityDeviation']
export type StabilityCalibration = components['schemas']['StabilityCalibration']
export type StabilityThinking = components['schemas']['StabilityThinking']
export type StabilityCompat = components['schemas']['StabilityCompat']
export type StabilityPreflight = components['schemas']['StabilityPreflight']

// 统一请求封装：非 2xx 抛出带后端 error 文案的异常
export class RequestError extends Error {
  status: number

  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    headers: init?.body ? { 'Content-Type': 'application/json' } : undefined,
    ...init,
  })
  if (!res.ok) {
    let message = `请求失败（${res.status}）`
    try {
      const data = (await res.json()) as ApiError
      if (data.error) message = data.error
    } catch {
      // 响应无 JSON body 时保留默认文案
    }
    throw new RequestError(res.status, message)
  }
  if (res.status === 204) return undefined as T
  const body = await res.text()
  if (!body.trim()) return undefined as T
  return JSON.parse(body) as T
}

export const systemApi = {
  version: () => request<VersionInfo>('/api/version'),
  updateStatus: () => request<UpdateStatus>('/api/system/update'),
  triggerDeploy: (tag: string) =>
    request<void>('/api/system/update/deploy', {
      method: 'POST',
      body: JSON.stringify({ tag }),
    }),
}

export const authApi = {
  methods: () => request<AuthMethods>('/api/auth/methods'),
  // 飞书登录整程由后端接管：这里只做整页跳转，授权码不经过前端。
  // next 是登录后要回到的站内路径，服务端会校验只放行相对路径。
  feishuLoginUrl: (next?: string) =>
    next && next !== '/'
      ? `/api/auth/feishu/authorize?next=${encodeURIComponent(next)}`
      : '/api/auth/feishu/authorize',
  login: (username: string, password: string) =>
    request<void>('/api/auth/login', {
      method: 'POST',
      body: JSON.stringify({ username, password }),
    }),
  logout: () => request<void>('/api/auth/logout', { method: 'POST' }),
  me: () => request<CurrentUser>('/api/auth/me'),
  changePassword: (currentPassword: string, newPassword: string) =>
    request<void>('/api/auth/password', {
      method: 'PUT',
      body: JSON.stringify({ currentPassword, newPassword }),
    }),
}

export const usersApi = {
  list: () => request<User[]>('/api/users'),
  create: (body: UserCreate) =>
    request<User>('/api/users', { method: 'POST', body: JSON.stringify(body) }),
  update: (id: string, body: UserUpdate) =>
    request<User>(`/api/users/${id}`, { method: 'PATCH', body: JSON.stringify(body) }),
  remove: (id: string) => request<void>(`/api/users/${id}`, { method: 'DELETE' }),
}

export const channelsApi = {
  list: () => request<Channel[]>('/api/channels'),
  create: (body: ChannelCreate) =>
    request<Channel>('/api/channels', { method: 'POST', body: JSON.stringify(body) }),
  get: (id: string) => request<ChannelDetail>(`/api/channels/${id}`),
  update: (id: string, body: ChannelUpdate) =>
    request<Channel>(`/api/channels/${id}`, { method: 'PATCH', body: JSON.stringify(body) }),
  remove: (id: string) => request<void>(`/api/channels/${id}`, { method: 'DELETE' }),
  addModel: (id: string, body: ModelEntryUpsert) =>
    request<ModelEntry>(`/api/channels/${id}/models`, { method: 'POST', body: JSON.stringify(body) }),
  updateModel: (id: string, modelId: string, body: ModelEntryUpsert) =>
    request<ModelEntry>(`/api/channels/${id}/models/${modelId}`, {
      method: 'PUT',
      body: JSON.stringify(body),
    }),
  removeModel: (id: string, modelId: string) =>
    request<void>(`/api/channels/${id}/models/${modelId}`, { method: 'DELETE' }),
  test: (id: string, modelId: string) =>
    request<ConnectivityTest>(`/api/channels/${id}/test`, {
      method: 'POST',
      body: JSON.stringify({ modelId }),
    }),
  connectivityHistory: (id: string, hours?: number) =>
    request<{ items: ConnectivityHistoryPoint[] }>(
      `/api/channels/${id}/connectivity/history${hours ? `?hours=${hours}` : ''}`,
    ),
}

export const overviewApi = {
  channels: () => request<{ items: OverviewChannel[] }>('/api/overview/channels'),
}

export const probesApi = {
  list: () => request<ProbeInfo[]>('/api/probes'),
}

export const qualityApi = {
  createTask: (body: QualityTaskCreate) =>
    request<QualityTask>('/api/quality/tasks', { method: 'POST', body: JSON.stringify(body) }),
  listTasks: (opts?: { limit?: number; offset?: number; status?: TaskStatus; channelId?: string }) => {
    const q = new URLSearchParams()
    q.set('limit', String(opts?.limit ?? 50))
    q.set('offset', String(opts?.offset ?? 0))
    if (opts?.status) q.set('status', opts.status)
    if (opts?.channelId) q.set('channelId', opts.channelId)
    return request<QualityTaskList>(`/api/quality/tasks?${q}`)
  },
  getTask: (id: string) => request<QualityTask>(`/api/quality/tasks/${id}`),
  cancelTask: (id: string) =>
    request<QualityTask>(`/api/quality/tasks/${id}/cancel`, { method: 'POST' }),
  listResults: (id: string, status?: CaseStatus) =>
    request<QualityCaseResult[]>(
      `/api/quality/tasks/${id}/results${status ? `?status=${status}` : ''}`,
    ),
  getReport: (id: string) => request<QualityReport>(`/api/quality/tasks/${id}/report`),
  // 导出走浏览器原生下载（会话 cookie 自动携带），不走 request 封装
  exportUrl: (id: string, format: 'json' | 'junit') =>
    `/api/quality/tasks/${id}/export?format=${format}`,
}

export const stabilityProbesApi = {
  list: () => request<StabilityProbeInfo[]>('/api/stability/probes'),
}

export const stabilityApi = {
  createTask: (body: StabilityTaskCreate) =>
    request<StabilityTask>('/api/stability/tasks', { method: 'POST', body: JSON.stringify(body) }),
  listTasks: (opts?: { limit?: number; offset?: number; status?: TaskStatus; channelId?: string }) => {
    const q = new URLSearchParams()
    q.set('limit', String(opts?.limit ?? 50))
    q.set('offset', String(opts?.offset ?? 0))
    if (opts?.status) q.set('status', opts.status)
    if (opts?.channelId) q.set('channelId', opts.channelId)
    return request<StabilityTaskList>(`/api/stability/tasks?${q}`)
  },
  getTask: (id: string) => request<StabilityTask>(`/api/stability/tasks/${id}`),
  cancelTask: (id: string) =>
    request<StabilityTask>(`/api/stability/tasks/${id}/cancel`, { method: 'POST' }),
  getMetrics: (id: string) => request<StabilityReport>(`/api/stability/tasks/${id}/metrics`),
  // 导出走浏览器原生下载（会话 cookie 自动携带），不走 request 封装
  exportUrl: (id: string) => `/api/stability/tasks/${id}/export?format=json`,
}

export const rolesApi = {
  list: () => request<Role[]>('/api/roles'),
  create: (body: RoleCreate) =>
    request<Role>('/api/roles', { method: 'POST', body: JSON.stringify(body) }),
  update: (id: string, body: RoleUpdate) =>
    request<Role>(`/api/roles/${id}`, { method: 'PATCH', body: JSON.stringify(body) }),
  remove: (id: string) => request<void>(`/api/roles/${id}`, { method: 'DELETE' }),
}
