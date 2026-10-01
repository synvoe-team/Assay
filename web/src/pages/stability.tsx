import { useEffect, useState } from 'react'
import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ChevronDown, ChevronLeft, ChevronRight, Loader2, Play, TriangleAlert, X } from 'lucide-react'
import { useNavigate } from 'react-router'
import { errText } from '@/components/channel-form-dialog'
import { TaskStatusBadge } from '@/components/quality-badges'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Progress } from '@/components/ui/progress'
import { Slider } from '@/components/ui/slider'
import { Switch } from '@/components/ui/switch'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { isTerminalStatus } from '@/hooks/use-task-events'
import {
  channelsApi,
  stabilityApi,
  stabilityProbesApi,
  type Protocol,
  type StabilityProbeInfo,
  type StabilityTask,
  type StabilityWorkload,
  type TaskStatus,
} from '@/lib/api'
import { type DictKey, useI18n } from '@/lib/i18n'

const PAGE_SIZE = 20
const TASK_STATUSES: TaskStatus[] = ['queued', 'running', 'succeeded', 'failed', 'canceled']
const ALL_PROTOCOLS: Protocol[] = ['openai_chat', 'openai_responses', 'anthropic_messages']

// 某 probe 对某协议是否适用：protocols 为空 = 三协议通用
function probeSupports(p: StabilityProbeInfo, proto: Protocol): boolean {
  return p.protocols.length === 0 || p.protocols.includes(proto)
}

// 解析并发档文本为正整数数组（逗号分隔）；非法 token 直接丢弃，单值上限 512 对齐后端校验
function parseLadder(text: string): number[] {
  return text
    .split(',')
    .map((s) => s.trim())
    .filter((s) => s !== '')
    .map(Number)
    .filter((n) => Number.isInteger(n) && n >= 1 && n <= 512)
}

// rampRates 几何倍增到达率序列（含上限档），对齐后端 rpm_probe 的爬坡口径
function rampRates(start: number, max: number): number[] {
  if (start <= 0 || max <= 0) return max > 0 ? [max] : []
  const out: number[] = []
  for (let r = start; r < max; r *= 2) out.push(r)
  out.push(max)
  return out
}

// estRpmRequests 复刻后端最坏预估：Σ 爬坡各档 + 二分步数 × 上限档；每档 ⌈rate×sec⌉+1
function estRpmRequests(start: number, max: number, stageSec: number, binarySteps: number): number {
  if (start <= 0 || stageSec <= 0) return 0
  const perStage = (rate: number) => Math.ceil(rate * stageSec) + 1
  const ramp = rampRates(start, max).reduce((sum, r) => sum + perStage(r), 0)
  return ramp + binarySteps * perStage(max)
}

// estTpmRequests 复刻后端 TPM 最坏预估：token 速率各档按 reqRate=tokenRate/权重 换算请求速率后 ⌈reqRate×sec⌉+1 求和
function estTpmRequests(
  start: number,
  max: number,
  stageSec: number,
  binarySteps: number,
  weight: number,
): number {
  if (start <= 0 || stageSec <= 0 || weight <= 0) return 0
  const perStage = (tokenRate: number) => Math.ceil((tokenRate / weight) * stageSec) + 1
  const ramp = rampRates(start, max).reduce((sum, r) => sum + perStage(r), 0)
  return ramp + binarySteps * perStage(max)
}

// —— 负载画像（口径对齐后端 stability/workload.go）——
type InputMode = NonNullable<StabilityWorkload['input']>['mode']
const INPUT_MODES: InputMode[] = ['none', 'fixed', 'ramp', 'jitter']
// 未设输出目标时各检测项的生成上限
const DEFAULT_OUTPUT: Record<string, number> = { concurrency_ladder: 2048, rpm_probe: 16, tpm_probe: 256 }
// SMALL_PROMPT_TOKENS 不塑形时每请求输入的名义 token：顶格数数 prompt + 唯一标记（后端 tpmWeightPerReq 同值）
const SMALL_PROMPT_TOKENS = 57
const MAX_INPUT_TOKENS = 1_000_000
const MAX_OUTPUT_TOKENS = 32768
const MIN_CACHEABLE_TOKENS = 1024 // 渠道前缀缓存最小可缓存长度
const MIN_UNIQUE_TOKENS = 64 // h>0 时每请求唯一段下限
const CALIB_TOKENS = 3000 // 定标请求 ≈ 12000 字符
const CACHE_WARMUP_REQUESTS = 2

export default function StabilityPage() {
  const { t } = useI18n()
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const [channelId, setChannelId] = useState('')
  const [modelId, setModelId] = useState('')
  // null = 默认全选（检测项列表加载后）；勾选过一次即接管
  const [pickedProbes, setPickedProbes] = useState<string[] | null>(null)
  const [protocol, setProtocol] = useState('')
  const [showAdvanced, setShowAdvanced] = useState(false)
  const [ladderText, setLadderText] = useState('1,2,4,8,16')
  const [requestsPerStage, setRequestsPerStage] = useState('20')
  const [warmupPerStage, setWarmupPerStage] = useState('2')
  // 负载画像：全缺省 = 小 prompt、不共享前缀、输出按各检测项默认
  const [inputMode, setInputMode] = useState<InputMode>('none')
  const [inputValue, setInputValue] = useState('8000')
  const [inputMin, setInputMin] = useState('2000')
  const [inputMax, setInputMax] = useState('8000')
  const [cacheHitRate, setCacheHitRate] = useState(0)
  const [outputTarget, setOutputTarget] = useState('') // 空 = 各检测项默认
  const [disableThinking, setDisableThinking] = useState(false)
  // RPM 实测参数（开环恒定到达率爬坡 + 二分收敛）
  const [rpmStartRate, setRpmStartRate] = useState('2')
  const [rpmMaxRate, setRpmMaxRate] = useState('20')
  const [rpmStageSec, setRpmStageSec] = useState('120')
  const [rpmMaxInFlight, setRpmMaxInFlight] = useState('128')
  const [rpmLimitThreshold, setRpmLimitThreshold] = useState('0.1')
  const [rpmBinarySteps, setRpmBinarySteps] = useState('4')
  // TPM 实测参数（开环恒定 token 到达率爬坡 + 二分收敛；输入+输出都计）
  const [tpmStartRate, setTpmStartRate] = useState('200')
  const [tpmMaxRate, setTpmMaxRate] = useState('2000')
  const [tpmStageSec, setTpmStageSec] = useState('120')
  const [tpmMaxInFlight, setTpmMaxInFlight] = useState('128')
  const [tpmLimitThreshold, setTpmLimitThreshold] = useState('0.1')
  const [tpmBinarySteps, setTpmBinarySteps] = useState('4')
  const [maxTotalRequests, setMaxTotalRequests] = useState('10000')
  const [maxTotalTokens, setMaxTotalTokens] = useState('2000000')
  const [maxDurationSec, setMaxDurationSec] = useState('3600')
  const [requestTimeoutMs, setRequestTimeoutMs] = useState('60000')
  const [filterStatus, setFilterStatus] = useState('all')
  const [filterChannel, setFilterChannel] = useState('all')
  const [page, setPage] = useState(1)

  const channels = useQuery({ queryKey: ['channels'], queryFn: channelsApi.list })
  const probes = useQuery({ queryKey: ['stability-probes'], queryFn: stabilityProbesApi.list })
  const channelDetail = useQuery({
    queryKey: ['channels', channelId],
    queryFn: () => channelsApi.get(channelId),
    enabled: channelId !== '',
  })
  const tasks = useQuery({
    queryKey: ['stability-tasks', filterStatus, filterChannel, page],
    queryFn: () =>
      stabilityApi.listTasks({
        limit: PAGE_SIZE,
        offset: (page - 1) * PAGE_SIZE,
        status: filterStatus === 'all' ? undefined : (filterStatus as TaskStatus),
        channelId: filterChannel === 'all' ? undefined : filterChannel,
      }),
    placeholderData: keepPreviousData,
    refetchInterval: (q) =>
      q.state.data?.items.some((task) => !isTerminalStatus(task.status)) ? 3000 : false,
  })
  const totalPages = Math.max(1, Math.ceil((tasks.data?.total ?? 0) / PAGE_SIZE))

  const create = useMutation({
    mutationFn: stabilityApi.createTask,
    onSuccess: (task) => {
      queryClient.invalidateQueries({ queryKey: ['stability-tasks'] })
      navigate(`/stability/${task.id}`)
    },
  })
  const cancel = useMutation({
    mutationFn: stabilityApi.cancelTask,
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['stability-tasks'] }),
  })

  const selectableChannels = (channels.data ?? []).filter((c) => !c.disabled)
  const models = channelDetail.data?.models ?? []
  const model = models.find((m) => m.id === modelId)
  const currencySymbol = channelDetail.data?.currency === 'CNY' ? '¥' : '$'
  const checked = pickedProbes ?? (probes.data ?? []).map((p) => p.id)
  const checkedProbes = (probes.data ?? []).filter((p) => checked.includes(p.id))

  // 可选协议 = 渠道声明协议 ∩ 所有已选 probe 都支持的协议
  const channelProtocols = (channelDetail.data?.protocols ?? []) as Protocol[]
  const allowedProtocols = ALL_PROTOCOLS.filter(
    (proto) =>
      channelProtocols.includes(proto) && checkedProbes.every((p) => probeSupports(p, proto)),
  )
  const allowedKey = allowedProtocols.join(',')
  // 可选协议集变化时把选择收敛到合法项（默认首项），无交集则清空
  useEffect(() => {
    if (allowedProtocols.length === 0) {
      if (protocol !== '') setProtocol('')
    } else if (!allowedProtocols.includes(protocol as Protocol)) {
      setProtocol(allowedProtocols[0])
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [allowedKey])

  const toggleProbe = (id: string) => {
    setPickedProbes(checked.includes(id) ? checked.filter((p) => p !== id) : [...checked, id])
  }

  const ladderNums = parseLadder(ladderText)
  const rps = Number(requestsPerStage) || 0
  const warmup = Number(warmupPerStage) || 0
  const rpmStart = Number(rpmStartRate) || 0
  const rpmMax = Number(rpmMaxRate) || 0
  const rpmSec = Number(rpmStageSec) || 0
  const rpmInFlight = Number(rpmMaxInFlight) || 0
  const rpmThreshold = Number(rpmLimitThreshold) || 0
  const rpmSteps = Number(rpmBinarySteps) || 0
  const tpmStart = Number(tpmStartRate) || 0
  const tpmMax = Number(tpmMaxRate) || 0
  const tpmSec = Number(tpmStageSec) || 0
  const tpmInFlight = Number(tpmMaxInFlight) || 0
  const tpmThreshold = Number(tpmLimitThreshold) || 0
  const tpmSteps = Number(tpmBinarySteps) || 0
  const totalReqCap = Number(maxTotalRequests) || 0
  const totalTokCap = Number(maxTotalTokens) || 0
  const rpmChecked = checked.includes('rpm_probe')
  const tpmChecked = checked.includes('tpm_probe')
  const timeoutMs = Number(requestTimeoutMs) || 0

  // 负载画像派生量：输入目标的下限/上限/期望（递增与抖动取区间均值），共享前缀 ≈ h × 期望
  const shaped = inputMode !== 'none'
  const inVal = Number(inputValue) || 0
  const inLo = inputMode === 'fixed' ? inVal : Number(inputMin) || 0
  const inHi = inputMode === 'fixed' ? inVal : Number(inputMax) || 0
  const inputMean = shaped ? (inLo + inHi) / 2 : 0
  const h = shaped ? cacheHitRate : 0
  const sharedTokens = Math.round(h * inputMean)
  const outTarget = outputTarget.trim() === '' ? 0 : Number(outputTarget)
  const outFor = (probeId: string) => (outTarget > 0 ? outTarget : (DEFAULT_OUTPUT[probeId] ?? 2048))
  const timeoutFloor = Math.max(60000, inHi)
  // 复刻后端 validateWorkload 的 fail-fast：创建前就说清哪里不行
  const workloadIssue = ((): string | null => {
    if (inputMode === 'fixed' && !(Number.isInteger(inVal) && inVal >= 1 && inVal <= MAX_INPUT_TOKENS))
      return t('stab.wl.errValue')
    if ((inputMode === 'ramp' || inputMode === 'jitter') && !(inLo >= 1 && inLo < inHi && inHi <= MAX_INPUT_TOKENS))
      return t('stab.wl.errRange')
    if (outputTarget.trim() !== '' && !(Number.isInteger(outTarget) && outTarget >= 1 && outTarget <= MAX_OUTPUT_TOKENS))
      return t('stab.wl.errOutput')
    if (h > 0 && sharedTokens < MIN_CACHEABLE_TOKENS) return `${t('stab.wl.errShared')}（≈ ${sharedTokens} token）`
    if (h > 0 && inLo - sharedTokens < MIN_UNIQUE_TOKENS) return t('stab.wl.errUnique')
    if (shaped && totalTokCap > 0 && inHi + (outTarget || 2048) > totalTokCap) return t('stab.wl.errTotalTokens')
    if (shaped && timeoutMs < timeoutFloor) return `${t('stab.wl.errTimeout')} ${timeoutFloor} ms`
    return null
  })()
  // 准备期请求：塑形时 1 条定标，h>0 再加写缓存
  const prepRequests = shaped ? 1 + (h > 0 ? CACHE_WARMUP_REQUESTS : 0) : 0
  // TPM 每请求 token 权重 = 输入期望 + 输出目标（对齐后端 tpmWeightPerReq）
  const tpmWeight = (shaped ? inputMean : SMALL_PROMPT_TOKENS) + outFor('tpm_probe')

  // 最坏预估请求数：各 probe 按实选参数求和。不再被总请求硬闸钳制后展示——
  // 钳制会把「超上限、会被截断」藏起来；超出时单独告警。
  // 三 probe 都有精确公式（阶梯 / RPM 爬坡+二分 / TPM token 速率爬坡+二分）。
  const estPerProbe = (p: StabilityProbeInfo): number => {
    if (p.id === 'concurrency_ladder') return ladderNums.length * (rps + warmup)
    if (p.id === 'rpm_probe') return estRpmRequests(rpmStart, rpmMax, rpmSec, rpmSteps)
    if (p.id === 'tpm_probe') return estTpmRequests(tpmStart, tpmMax, tpmSec, tpmSteps, tpmWeight)
    return p.estRequests
  }
  const estRequests = prepRequests + checkedProbes.reduce((sum, p) => sum + estPerProbe(p), 0)
  const overRequestCap = totalReqCap > 0 && estRequests > totalReqCap
  // RPM/TPM 最长耗时 = 最坏档数（爬坡档 + 二分步数）× 每档时长；阶梯取决于渠道延迟，不计入
  const pacedSec = (start: number, max: number, steps: number, sec: number) =>
    (rampRates(start, max).length + steps) * sec
  const estPacedSec =
    (rpmChecked ? pacedSec(rpmStart, rpmMax, rpmSteps, rpmSec) : 0) +
    (tpmChecked ? pacedSec(tpmStart, tpmMax, tpmSteps, tpmSec) : 0)
  const overDurationCap = estPacedSec > (Number(maxDurationSec) || 0)
  // 预估费用上界：输入侧按输入期望（含定标/写缓存），其中 h 部分按缓存读价、其余按输入价；
  // 输出侧按各 probe 的生成上限；总量受总 token 硬闸钳制（按比例缩放）
  const inputPerReq = shaped ? inputMean : SMALL_PROMPT_TOKENS
  let estIn = shaped ? CALIB_TOKENS + (h > 0 ? CACHE_WARMUP_REQUESTS * sharedTokens : 0) : 0
  let estOut = 0
  for (const p of checkedProbes) {
    const n = Math.min(estPerProbe(p), totalReqCap || Infinity)
    estIn += n * inputPerReq
    estOut += n * outFor(p.id)
  }
  const tokScale = totalTokCap > 0 ? Math.min(1, totalTokCap / Math.max(estIn + estOut, 1)) : 1
  const inPrice = model?.inputPrice ?? (shaped ? undefined : 0) // 不塑形时输入极小，缺输入价按 0 计
  const cachedPrice = model?.cachedInputPrice ?? inPrice
  const estCost =
    model?.outputPrice != null && inPrice != null && cachedPrice != null
      ? (tokScale * (estIn * ((1 - h) * inPrice + h * cachedPrice) + estOut * model.outputPrice)) / 1_000_000
      : null

  const paramsValid =
    ladderNums.length > 0 &&
    rps >= 1 &&
    workloadIssue == null &&
    (!rpmChecked || (rpmStart >= 0.1 && rpmMax >= rpmStart && rpmSec >= 2)) &&
    (!tpmChecked || (tpmStart >= 1 && tpmMax >= tpmStart && tpmSec >= 2))

  const workload: StabilityWorkload = {
    input:
      inputMode === 'fixed'
        ? { mode: 'fixed', value: inVal }
        : inputMode === 'none'
          ? { mode: 'none' }
          : { mode: inputMode, min: inLo, max: inHi },
    cacheHitRate: h,
    ...(outTarget > 0 ? { output: outTarget } : {}),
    disableThinking,
  }

  const submit = () => {
    create.mutate({
      channelId,
      modelEntryId: modelId,
      probes: checked,
      params: {
        protocol: protocol as Protocol,
        concurrencyLadder: ladderNums,
        requestsPerStage: rps,
        warmupPerStage: warmup,
        workload,
        rpmStartRate: rpmStart,
        rpmMaxRate: rpmMax,
        rpmStageSec: rpmSec,
        rpmMaxInFlight: rpmInFlight,
        rpmLimitThreshold: rpmThreshold,
        rpmBinarySteps: rpmSteps,
        tpmStartRate: tpmStart,
        tpmMaxRate: tpmMax,
        tpmStageSec: tpmSec,
        tpmMaxInFlight: tpmInFlight,
        tpmLimitThreshold: tpmThreshold,
        tpmBinarySteps: tpmSteps,
        maxTotalRequests: totalReqCap,
        maxTotalTokens: totalTokCap,
        maxDurationSec: Number(maxDurationSec) || 3600,
        requestTimeoutMs: timeoutMs || 60000,
      },
    })
  }

  const probeName = (id: string) => probes.data?.find((p) => p.id === id)?.name ?? id

  return (
    <div className="grid gap-6">
      <div>
        <h1 className="text-2xl font-semibold">{t('nav.stability')}</h1>
        <p className="mt-1 text-sm text-muted-foreground">{t('stability.desc')}</p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>{t('quality.target')}</CardTitle>
          <CardDescription>{t('quality.targetDesc')}</CardDescription>
        </CardHeader>
        <CardContent>
          <div className="flex flex-wrap items-end gap-3">
            <div className="grid gap-2">
              <Label>{t('quality.channel')}</Label>
              <Select
                value={channelId}
                onValueChange={(v) => {
                  setChannelId(v)
                  setModelId('')
                }}
                disabled={selectableChannels.length === 0}
              >
                <SelectTrigger className="w-56">
                  <SelectValue placeholder={t('quality.selectChannel')} />
                </SelectTrigger>
                <SelectContent>
                  {selectableChannels.map((c) => (
                    <SelectItem key={c.id} value={c.id}>
                      {c.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="grid gap-2">
              <Label>{t('quality.model')}</Label>
              <Select
                value={modelId}
                onValueChange={setModelId}
                disabled={channelId === '' || models.length === 0}
              >
                <SelectTrigger className="w-56">
                  <SelectValue placeholder={t('quality.selectModel')} />
                </SelectTrigger>
                <SelectContent>
                  {models.map((m) => (
                    <SelectItem key={m.id} value={m.id}>
                      {m.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            {selectableChannels.length === 0 && !channels.isPending && (
              <p className="text-sm text-muted-foreground">{t('quality.noChannels')}</p>
            )}
            {channelId !== '' && channelDetail.isSuccess && models.length === 0 && (
              <p className="text-sm text-muted-foreground">{t('quality.noModels')}</p>
            )}
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t('quality.probes')}</CardTitle>
          <CardDescription>{t('stab.probesDesc')}</CardDescription>
        </CardHeader>
        <CardContent className="grid gap-4">
          {probes.isPending ? (
            <p className="text-sm text-muted-foreground">{t('common.loading')}</p>
          ) : (
            <div className="grid gap-2">
              {(probes.data ?? []).map((p) => (
                <label
                  key={p.id}
                  className="flex cursor-pointer items-start gap-3 rounded-md border p-3"
                >
                  <Checkbox
                    checked={checked.includes(p.id)}
                    onCheckedChange={() => toggleProbe(p.id)}
                    className="mt-0.5"
                  />
                  <div className="grid gap-1">
                    <span className="flex flex-wrap items-center gap-2 text-sm font-medium">
                      {p.name}
                      <span className="text-xs font-normal text-muted-foreground">
                        ≈ {p.estRequests} {t('quality.requests')}
                      </span>
                      <span className="flex flex-wrap gap-1">
                        {(p.protocols.length === 0 ? ALL_PROTOCOLS : p.protocols).map((proto) => (
                          <Badge key={proto} variant="outline" className="font-normal">
                            {t(`proto.${proto}` as DictKey)}
                          </Badge>
                        ))}
                      </span>
                    </span>
                    <span className="text-xs text-muted-foreground">{p.description}</span>
                  </div>
                </label>
              ))}
            </div>
          )}

          <div className="flex flex-wrap items-end gap-3">
            <div className="grid gap-2">
              <Label>{t('stab.protocol')}</Label>
              <Select
                value={protocol}
                onValueChange={setProtocol}
                disabled={allowedProtocols.length === 0}
              >
                <SelectTrigger className="w-56">
                  <SelectValue placeholder={t('stab.protocol')} />
                </SelectTrigger>
                <SelectContent>
                  {allowedProtocols.map((proto) => (
                    <SelectItem key={proto} value={proto}>
                      {t(`proto.${proto}` as DictKey)}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <p className="pb-2 text-xs text-muted-foreground">
              {channelId !== '' && checkedProbes.length > 0 && allowedProtocols.length === 0
                ? t('stab.noProtocol')
                : t('stab.protocolHint')}
            </p>
          </div>

          <div className="grid gap-3 rounded-md border p-3">
            <div className="grid gap-1">
              <p className="text-sm font-medium">{t('stab.wl.title')}</p>
              <p className="text-xs text-muted-foreground">{t('stab.wl.desc')}</p>
            </div>
            <div className="flex flex-wrap items-end gap-3">
              <div className="grid gap-2">
                <Label>{t('stab.wl.inputMode')}</Label>
                <Select value={inputMode} onValueChange={(v) => setInputMode(v as InputMode)}>
                  <SelectTrigger className="w-40">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {INPUT_MODES.map((m) => (
                      <SelectItem key={m} value={m}>
                        {t(`stab.wl.mode.${m}` as DictKey)}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              {inputMode === 'fixed' && (
                <div className="grid gap-2">
                  <Label htmlFor="s-wl-value">{t('stab.wl.inputValue')}</Label>
                  <Input
                    id="s-wl-value"
                    type="number"
                    min={1}
                    max={MAX_INPUT_TOKENS}
                    className="w-36"
                    value={inputValue}
                    onChange={(e) => setInputValue(e.target.value)}
                  />
                </div>
              )}
              {(inputMode === 'ramp' || inputMode === 'jitter') && (
                <>
                  <div className="grid gap-2">
                    <Label htmlFor="s-wl-min">{t('stab.wl.inputMin')}</Label>
                    <Input
                      id="s-wl-min"
                      type="number"
                      min={1}
                      max={MAX_INPUT_TOKENS}
                      className="w-36"
                      value={inputMin}
                      onChange={(e) => setInputMin(e.target.value)}
                    />
                  </div>
                  <div className="grid gap-2">
                    <Label htmlFor="s-wl-max">{t('stab.wl.inputMax')}</Label>
                    <Input
                      id="s-wl-max"
                      type="number"
                      min={1}
                      max={MAX_INPUT_TOKENS}
                      className="w-36"
                      value={inputMax}
                      onChange={(e) => setInputMax(e.target.value)}
                    />
                  </div>
                </>
              )}
              <div className="grid gap-2">
                <Label htmlFor="s-wl-cache">
                  {t('stab.wl.cacheHitRate')}
                  <span className="tabular-nums text-muted-foreground">{Math.round(h * 100)}%</span>
                </Label>
                <Slider
                  id="s-wl-cache"
                  className="h-9 w-48"
                  min={0}
                  max={0.95}
                  step={0.05}
                  disabled={!shaped}
                  value={[h]}
                  onValueChange={([v]) => setCacheHitRate(Math.round(v * 100) / 100)}
                />
              </div>
              <div className="grid gap-2">
                <Label htmlFor="s-wl-output">{t('stab.wl.output')}</Label>
                <Input
                  id="s-wl-output"
                  type="number"
                  min={1}
                  max={MAX_OUTPUT_TOKENS}
                  className="w-36"
                  placeholder={t('stab.wl.outputDefault')}
                  value={outputTarget}
                  onChange={(e) => setOutputTarget(e.target.value)}
                />
              </div>
              <div className="flex h-9 items-center gap-2">
                <Switch id="s-wl-think" checked={disableThinking} onCheckedChange={setDisableThinking} />
                <Label htmlFor="s-wl-think">{t('stab.wl.disableThinking')}</Label>
              </div>
            </div>
            {workloadIssue ? (
              <p className="text-xs text-destructive">{workloadIssue}</p>
            ) : (
              <p className="flex flex-wrap gap-x-2 text-xs text-muted-foreground">
                <span>{shaped ? t(`stab.wl.modeHint.${inputMode}` as DictKey) : t('stab.wl.noneHint')}</span>
                {h > 0 && (
                  <span className="tabular-nums">
                    {t('stab.wl.sharedPrefix')} ≈ {sharedTokens} token
                  </span>
                )}
              </p>
            )}
          </div>

          <div className="grid gap-3">
            <button
              type="button"
              className="flex w-fit items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
              onClick={() => setShowAdvanced((v) => !v)}
            >
              <ChevronDown
                className={`size-4 transition-transform ${showAdvanced ? '' : '-rotate-90'}`}
              />
              {t('quality.advanced')}
            </button>
            {showAdvanced && (
              <div className="flex flex-wrap gap-3">
                <div className="grid gap-2">
                  <Label htmlFor="s-ladder">{t('stab.ladder')}</Label>
                  <Input
                    id="s-ladder"
                    className="w-48"
                    placeholder={t('stab.ladderHint')}
                    value={ladderText}
                    onChange={(e) => setLadderText(e.target.value)}
                  />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="s-rps">{t('stab.requestsPerStage')}</Label>
                  <Input
                    id="s-rps"
                    type="number"
                    min={1}
                    max={1000}
                    className="w-36"
                    value={requestsPerStage}
                    onChange={(e) => setRequestsPerStage(e.target.value)}
                  />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="s-warmup">{t('stab.warmupPerStage')}</Label>
                  <Input
                    id="s-warmup"
                    type="number"
                    min={0}
                    max={100}
                    className="w-36"
                    value={warmupPerStage}
                    onChange={(e) => setWarmupPerStage(e.target.value)}
                  />
                </div>
                {rpmChecked && (
                  <>
                    <div className="grid gap-2">
                      <Label htmlFor="s-rpm-start">{t('stab.rpmStartRate')}</Label>
                      <Input
                        id="s-rpm-start"
                        type="number"
                        min={0.1}
                        step={0.1}
                        className="w-36"
                        value={rpmStartRate}
                        onChange={(e) => setRpmStartRate(e.target.value)}
                      />
                    </div>
                    <div className="grid gap-2">
                      <Label htmlFor="s-rpm-max">{t('stab.rpmMaxRate')}</Label>
                      <Input
                        id="s-rpm-max"
                        type="number"
                        min={0.1}
                        step={0.1}
                        className="w-36"
                        value={rpmMaxRate}
                        onChange={(e) => setRpmMaxRate(e.target.value)}
                      />
                    </div>
                    <div className="grid gap-2">
                      <Label htmlFor="s-rpm-sec">{t('stab.rpmStageSec')}</Label>
                      <Input
                        id="s-rpm-sec"
                        type="number"
                        min={2}
                        max={600}
                        className="w-36"
                        value={rpmStageSec}
                        onChange={(e) => setRpmStageSec(e.target.value)}
                      />
                    </div>
                    <div className="grid gap-2">
                      <Label htmlFor="s-rpm-inflight">{t('stab.rpmMaxInFlight')}</Label>
                      <Input
                        id="s-rpm-inflight"
                        type="number"
                        min={1}
                        className="w-36"
                        value={rpmMaxInFlight}
                        onChange={(e) => setRpmMaxInFlight(e.target.value)}
                      />
                    </div>
                    <div className="grid gap-2">
                      <Label htmlFor="s-rpm-threshold">{t('stab.rpmLimitThreshold')}</Label>
                      <Input
                        id="s-rpm-threshold"
                        type="number"
                        min={0}
                        max={1}
                        step={0.01}
                        className="w-36"
                        value={rpmLimitThreshold}
                        onChange={(e) => setRpmLimitThreshold(e.target.value)}
                      />
                    </div>
                    <div className="grid gap-2">
                      <Label htmlFor="s-rpm-steps">{t('stab.rpmBinarySteps')}</Label>
                      <Input
                        id="s-rpm-steps"
                        type="number"
                        min={0}
                        max={12}
                        className="w-36"
                        value={rpmBinarySteps}
                        onChange={(e) => setRpmBinarySteps(e.target.value)}
                      />
                    </div>
                  </>
                )}
                {tpmChecked && (
                  <>
                    <div className="grid gap-2">
                      <Label htmlFor="s-tpm-start">{t('stab.tpmStartRate')}</Label>
                      <Input
                        id="s-tpm-start"
                        type="number"
                        min={1}
                        className="w-36"
                        value={tpmStartRate}
                        onChange={(e) => setTpmStartRate(e.target.value)}
                      />
                    </div>
                    <div className="grid gap-2">
                      <Label htmlFor="s-tpm-max">{t('stab.tpmMaxRate')}</Label>
                      <Input
                        id="s-tpm-max"
                        type="number"
                        min={1}
                        className="w-36"
                        value={tpmMaxRate}
                        onChange={(e) => setTpmMaxRate(e.target.value)}
                      />
                    </div>
                    <div className="grid gap-2">
                      <Label htmlFor="s-tpm-sec">{t('stab.tpmStageSec')}</Label>
                      <Input
                        id="s-tpm-sec"
                        type="number"
                        min={2}
                        max={600}
                        className="w-36"
                        value={tpmStageSec}
                        onChange={(e) => setTpmStageSec(e.target.value)}
                      />
                    </div>
                    <div className="grid gap-2">
                      <Label htmlFor="s-tpm-inflight">{t('stab.tpmMaxInFlight')}</Label>
                      <Input
                        id="s-tpm-inflight"
                        type="number"
                        min={1}
                        className="w-36"
                        value={tpmMaxInFlight}
                        onChange={(e) => setTpmMaxInFlight(e.target.value)}
                      />
                    </div>
                    <div className="grid gap-2">
                      <Label htmlFor="s-tpm-threshold">{t('stab.tpmLimitThreshold')}</Label>
                      <Input
                        id="s-tpm-threshold"
                        type="number"
                        min={0}
                        max={1}
                        step={0.01}
                        className="w-36"
                        value={tpmLimitThreshold}
                        onChange={(e) => setTpmLimitThreshold(e.target.value)}
                      />
                    </div>
                    <div className="grid gap-2">
                      <Label htmlFor="s-tpm-steps">{t('stab.tpmBinarySteps')}</Label>
                      <Input
                        id="s-tpm-steps"
                        type="number"
                        min={0}
                        max={12}
                        className="w-36"
                        value={tpmBinarySteps}
                        onChange={(e) => setTpmBinarySteps(e.target.value)}
                      />
                    </div>
                  </>
                )}
                <div className="grid gap-2">
                  <Label htmlFor="s-maxreq">{t('stab.maxTotalRequests')}</Label>
                  <Input
                    id="s-maxreq"
                    type="number"
                    min={1}
                    className="w-36"
                    value={maxTotalRequests}
                    onChange={(e) => setMaxTotalRequests(e.target.value)}
                  />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="s-maxtoktotal">{t('stab.maxTotalTokens')}</Label>
                  <Input
                    id="s-maxtoktotal"
                    type="number"
                    min={1}
                    className="w-40"
                    value={maxTotalTokens}
                    onChange={(e) => setMaxTotalTokens(e.target.value)}
                  />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="s-maxdur">{t('stab.maxDurationSec')}</Label>
                  <Input
                    id="s-maxdur"
                    type="number"
                    min={60}
                    max={7200}
                    className="w-32"
                    value={maxDurationSec}
                    onChange={(e) => setMaxDurationSec(e.target.value)}
                  />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="s-timeout">{t('stab.requestTimeout')}</Label>
                  <Input
                    id="s-timeout"
                    type="number"
                    min={1000}
                    max={600000}
                    className="w-36"
                    value={requestTimeoutMs}
                    onChange={(e) => setRequestTimeoutMs(e.target.value)}
                  />
                </div>
              </div>
            )}
          </div>

          {create.isError && <p className="text-sm text-destructive">{errText(create.error)}</p>}
          <div className="flex flex-wrap items-center gap-4">
            <Button
              onClick={submit}
              disabled={
                create.isPending ||
                channelId === '' ||
                modelId === '' ||
                checked.length === 0 ||
                protocol === '' ||
                !paramsValid
              }
            >
              {create.isPending ? <Loader2 className="animate-spin" /> : <Play />}
              {create.isPending ? t('quality.starting') : t('quality.launch')}
            </Button>
            <span className="text-sm text-muted-foreground">
              {t('quality.estRequests')}：{estRequests}
            </span>
            {estPacedSec > 0 && (
              <span className="text-sm text-muted-foreground">
                {t('stab.estDuration')}：≈ {Math.ceil(estPacedSec / 60)} {t('stab.minutes')}
              </span>
            )}
            {estCost != null && (
              <span className="text-sm text-muted-foreground">
                {t('stab.estCost')}：≤ {currencySymbol}
                {estCost.toFixed(estCost < 1 ? 4 : 2)}
              </span>
            )}
          </div>
          {(overRequestCap || overDurationCap) && (
            <p className="mt-3 flex items-center gap-1.5 text-sm text-amber-600 dark:text-amber-400">
              <TriangleAlert className="size-4 shrink-0" />
              {overRequestCap ? t('stab.overRequestCap') : t('stab.overDurationCap')}
            </p>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t('quality.history')}</CardTitle>
        </CardHeader>
        <CardContent className="space-y-4">
          <div className="flex items-center gap-2">
            <Select
              value={filterStatus}
              onValueChange={(v) => {
                setFilterStatus(v)
                setPage(1)
              }}
            >
              <SelectTrigger className="w-36">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">{t('quality.filterAllStatuses')}</SelectItem>
                {TASK_STATUSES.map((s) => (
                  <SelectItem key={s} value={s}>
                    {t(`taskStatus.${s}`)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            {/* 渠道筛选列表不过滤停用渠道：历史任务可能属于已停用渠道 */}
            <Select
              value={filterChannel}
              onValueChange={(v) => {
                setFilterChannel(v)
                setPage(1)
              }}
            >
              <SelectTrigger className="w-48">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="all">{t('quality.filterAllChannels')}</SelectItem>
                {(channels.data ?? []).map((c) => (
                  <SelectItem key={c.id} value={c.id}>
                    {c.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          {tasks.isPending ? (
            <p className="text-sm text-muted-foreground">{t('common.loading')}</p>
          ) : tasks.isError ? (
            <p className="text-sm text-destructive">{errText(tasks.error)}</p>
          ) : tasks.data.items.length === 0 ? (
            <p className="py-8 text-center text-sm text-muted-foreground">
              {t('stab.historyEmpty')}
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t('quality.status')}</TableHead>
                  <TableHead>{t('quality.target')}</TableHead>
                  <TableHead>{t('quality.probes')}</TableHead>
                  <TableHead className="w-44">{t('quality.progress')}</TableHead>
                  <TableHead>{t('stab.protocol')}</TableHead>
                  <TableHead>{t('quality.createdBy')}</TableHead>
                  <TableHead>{t('quality.createdAt')}</TableHead>
                  <TableHead className="w-10" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {tasks.data.items.map((task) => (
                  <TaskRow
                    key={task.id}
                    task={task}
                    probeName={probeName}
                    canceling={cancel.isPending && cancel.variables === task.id}
                    onCancel={() => cancel.mutate(task.id)}
                  />
                ))}
              </TableBody>
            </Table>
          )}
          {totalPages > 1 && (
            <div className="flex items-center justify-end gap-2">
              <Button
                variant="outline"
                size="icon"
                className="size-7"
                title={t('quality.prevPage')}
                disabled={page <= 1}
                onClick={() => setPage(page - 1)}
              >
                <ChevronLeft />
              </Button>
              <span className="text-sm tabular-nums text-muted-foreground">
                {t('quality.pagePrefix')}
                {page} / {totalPages}
                {t('quality.pageSuffix')}
              </span>
              <Button
                variant="outline"
                size="icon"
                className="size-7"
                title={t('quality.nextPage')}
                disabled={page >= totalPages}
                onClick={() => setPage(page + 1)}
              >
                <ChevronRight />
              </Button>
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  )
}

function TaskRow({
  task,
  probeName,
  canceling,
  onCancel,
}: {
  task: StabilityTask
  probeName: (id: string) => string
  canceling: boolean
  onCancel: () => void
}) {
  const { t } = useI18n()
  const navigate = useNavigate()
  const pct = task.progressTotal > 0 ? (task.progressDone / task.progressTotal) * 100 : 0
  const proto = task.params.protocol

  return (
    <TableRow className="cursor-pointer" onClick={() => navigate(`/stability/${task.id}`)}>
      <TableCell>
        <TaskStatusBadge status={task.status} />
      </TableCell>
      <TableCell className="font-medium">
        {task.target.channelName}
        <span className="text-muted-foreground"> · {task.target.model}</span>
      </TableCell>
      <TableCell className="max-w-56 truncate text-muted-foreground">
        {task.probes.map(probeName).join('、')}
      </TableCell>
      <TableCell>
        {task.status === 'queued' ? (
          <span className="text-muted-foreground">—</span>
        ) : (
          <div className="flex items-center gap-2">
            <Progress value={pct} className="w-20" />
            <span className="text-xs tabular-nums text-muted-foreground">
              {task.progressDone}/{task.progressTotal}
            </span>
          </div>
        )}
      </TableCell>
      <TableCell className="text-muted-foreground">
        {proto ? t(`proto.${proto}` as DictKey) : '—'}
      </TableCell>
      <TableCell className="text-muted-foreground">{task.createdBy ?? '—'}</TableCell>
      <TableCell className="text-muted-foreground">
        {new Date(task.createdAt).toLocaleString()}
      </TableCell>
      <TableCell>
        {task.status === 'queued' && (
          <Button
            variant="ghost"
            size="icon"
            className="size-7"
            title={t('quality.cancelTask')}
            disabled={canceling}
            onClick={(e) => {
              e.stopPropagation()
              onCancel()
            }}
          >
            {canceling ? <Loader2 className="animate-spin" /> : <X />}
          </Button>
        )}
      </TableCell>
    </TableRow>
  )
}
