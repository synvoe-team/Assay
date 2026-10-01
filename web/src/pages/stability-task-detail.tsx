import { Fragment, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ArrowLeft, FileJson, Loader2, RotateCcw, TriangleAlert, X } from 'lucide-react'
import { useNavigate, useParams } from 'react-router'
import { CartesianGrid, Line, LineChart, ReferenceLine, XAxis, YAxis } from 'recharts'
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
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from '@/components/ui/chart'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Progress } from '@/components/ui/progress'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { isTerminalStatus, useTaskEvents } from '@/hooks/use-task-events'
import {
  stabilityApi,
  stabilityProbesApi,
  type StabilityCalibration,
  type StabilityCompat,
  type StabilityDeviation,
  type StabilityMetrics,
  type StabilityPreflight,
  type StabilityReport,
  type StabilityStageMetric,
  type StabilityTask,
  type StabilityWorkload,
} from '@/lib/api'
import { type DictKey, useI18n } from '@/lib/i18n'
import { cn } from '@/lib/utils'

const OVERALL_STAGE = '__overall__'

// 三分位数配色语义化：越高分位越告警（蓝→琥珀→红）；主题内置 --chart-* 为灰阶不便区分，故直给色值
const CHART_CONFIG: ChartConfig = {
  p50: { label: 'p50', color: '#2563eb' },
  p95: { label: 'p95', color: '#d97706' },
  p99: { label: 'p99', color: '#dc2626' },
  ttfdP50: { label: 'TTFD p50', color: '#7c3aed' },
}

// hasReasoningGap 首增量带来 TTFT 之外的信息时才单列 TTFD：推理模型先吐思考（首增量早于首正文），
// 或整档没测到首正文（输出上限被思考用完，首增量是唯一的首响应延迟）；非推理模型两者相等，多画一条只是噪音
function hasReasoningGap(stages: StabilityStageMetric[]): boolean {
  return stages.some((s) => {
    const { ttfdMs, ttftMs } = s.metrics
    return ttfdMs != null && (ttftMs == null || ttfdMs.p50 < ttftMs.p50)
  })
}

// boundaryState RPM/TPM 收敛值的可信度：触顶护栏或「被截断且从没见过限速档」时只是下界（显示 ≥），
// 截断但已见过限速档则二分未完成
function boundaryState(stages: StabilityStageMetric[], overall?: StabilityStageMetric) {
  const m = overall?.metrics
  const sawLimited = stages.some((s) => s.metrics.rateLimited)
  const truncated = m?.truncated ?? false
  const reachedCap = m?.reachedCap ?? false
  return {
    lowerBound: reachedCap || (truncated && !sawLimited),
    reachedCap,
    truncated,
    sawLimited,
    truncatedBy: m?.truncatedBy,
  }
}

// BoundaryNotes 收敛值旁的琥珀色说明：触顶护栏 / 被硬闸截断（下界或二分未完成）
function BoundaryNotes({ state }: { state: ReturnType<typeof boundaryState> }) {
  const { t } = useI18n()
  return (
    <>
      {state.reachedCap && (
        <span className="text-xs text-amber-600 dark:text-amber-400">· {t('stab.reachedCapNote')}</span>
      )}
      {state.truncated && (
        <span className="text-xs text-amber-600 dark:text-amber-400">
          · {t('stab.truncated')}
          {state.truncatedBy && `（${t(`stab.capBy.${state.truncatedBy}` as DictKey)}）`}：
          {t(state.sawLimited ? 'stab.truncatedBinaryNote' : 'stab.truncatedLowerNote')}
        </span>
      )}
    </>
  )
}

function formatDuration(ms: number): string {
  const s = Math.round(ms / 1000)
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`
}

// 数值格式化：错误率是 0..1 分数 → 百分比；吞吐保留一位小数
function pct(v: number): string {
  return `${(v * 100).toFixed(1)}%`
}
function num(v: number | undefined): string {
  return v == null ? '—' : v.toFixed(1)
}
function ms(v: number | undefined): string {
  return v == null ? '—' : `${Math.round(v)}`
}
// signedPct 已是百分数的偏差值带符号显示
function signedPct(v: number): string {
  return `${v > 0 ? '+' : ''}${v.toFixed(1)}%`
}

// workloadCols 本检测项各档有哪些负载画像列：不塑形 / 无输出目标 / h=0 时整列不出
function workloadCols(rows: StabilityStageMetric[]) {
  return {
    input: rows.some((s) => s.metrics.inputDeviation != null),
    output: rows.some((s) => s.metrics.outputDeviation != null),
    cache: rows.some((s) => s.metrics.cacheHitRate != null),
  }
}
type WorkloadCols = ReturnType<typeof workloadCols>

// rowTone 输入偏差超阈值的档整行标黄（该档延迟不代表目标负载）
function rowTone(m: StabilityMetrics, bold?: boolean): string | undefined {
  return cn(bold && 'font-medium', m.inputDeviation?.exceeded && 'bg-amber-500/10') || undefined
}

function WorkloadHeads({ cols }: { cols: WorkloadCols }) {
  const { t } = useI18n()
  return (
    <>
      {cols.input && <TableHead className="text-right">{t('stab.wl.inputDev')}</TableHead>}
      {cols.output && <TableHead className="text-right">{t('stab.wl.outputDev')}</TableHead>}
      {cols.cache && <TableHead className="text-right">{t('stab.wl.hitRate')}</TableHead>}
    </>
  )
}

// DeviationValue 偏差 p50，悬停看 |偏差| p95 与范围；超阈值琥珀色
function DeviationValue({ d }: { d?: StabilityDeviation }) {
  if (d == null) return <span className="text-muted-foreground">—</span>
  return (
    <span
      title={`p50 ${signedPct(d.p50)} · |p95| ${d.absP95.toFixed(1)}% · ${signedPct(d.min)} ~ ${signedPct(d.max)} · n=${d.samples}`}
      className={d.exceeded ? 'text-amber-700 dark:text-amber-300' : undefined}
    >
      {signedPct(d.p50)}
    </span>
  )
}

function WorkloadCells({ cols, m }: { cols: WorkloadCols; m: StabilityMetrics }) {
  return (
    <>
      {cols.input && (
        <TableCell className="text-right tabular-nums">
          <DeviationValue d={m.inputDeviation} />
        </TableCell>
      )}
      {cols.output && (
        <TableCell className="text-right tabular-nums">
          <DeviationValue d={m.outputDeviation} />
        </TableCell>
      )}
      {cols.cache && (
        <TableCell className={cn('text-right tabular-nums', m.cacheMiss && 'text-destructive')}>
          {m.cacheHitRate == null ? '—' : pct(m.cacheHitRate)}
        </TableCell>
      )}
    </>
  )
}

// CalibrationNote 定标结果：名义比 / 实测比 / 偏差，h>0 时附共享前缀与写缓存命中；没测成时写明原因与降级口径
function CalibrationNote({ c }: { c: StabilityCalibration }) {
  const { t } = useI18n()
  const items = c.uncalibrated
    ? [`${t('stab.wl.uncalibrated')}${c.reason ? `（${c.reason}）` : ''}`, `${t('stab.wl.nominalRatio')} ${c.nominalRatio.toFixed(2)} ${t('stab.wl.charsPerToken')}`]
    : [
        `${t('stab.wl.nominalRatio')} ${c.nominalRatio.toFixed(2)} ${t('stab.wl.charsPerToken')}`,
        `${t('stab.wl.measuredRatio')} ${c.measuredRatio.toFixed(2)} ${t('stab.wl.charsPerToken')}`,
        `${t('stab.wl.deviation')} ${signedPct(c.deviationPct)}`,
        `${t('stab.wl.calibPrompt')} ${c.chars} → ${c.promptTokens} token`,
      ]
  if (c.sharedTokens) items.push(`${t('stab.wl.sharedPrefix')} ${c.sharedTokens} token`)
  if (c.cacheWarmCached != null) items.push(`${t('stab.wl.cacheWarm')} ${c.cacheWarmCached} token`)
  return (
    <div className="grid gap-1">
      <p className="text-sm font-medium">{t('stab.wl.calibration')}</p>
      <p className="text-xs text-muted-foreground">{items.join(' · ')}</p>
    </div>
  )
}

// 关思考试探结果的配色：关掉了绿、照样思考琥珀、被拒/失败灰
const TRIAL_TONE: Record<string, string> = {
  disabled: 'text-emerald-700 dark:text-emerald-400',
  still_reasoning: 'text-amber-700 dark:text-amber-300',
  rejected: 'text-muted-foreground line-through',
  failed: 'text-muted-foreground',
}

// PreflightNote 预检结论：上游实际接受的请求形态、做过的兼容调整、关思考探测——让「跑通靠的是什么」可审计
function PreflightNote({ pf }: { pf: StabilityPreflight }) {
  const { t } = useI18n()
  const facts = [
    pf.passed ? `${t('stab.pf.passed')}（${pf.requests} ${t('stab.pf.requests')}）` : `${t('stab.pf.notPassed')}：${pf.detail ?? '—'}`,
  ]
  if (pf.passed && !pf.usageReported) facts.push(t('stab.pf.noUsage'))
  if (pf.nonStream) facts.push(t('stab.pf.nonStream'))
  if (pf.reasoning) facts.push(t('stab.pf.reasoning'))
  if (pf.thinkingField) facts.push(`${t('stab.pf.thinkingAdopted')} ${pf.thinkingField}`)
  return (
    <div className="grid gap-1">
      <p className="text-sm font-medium">{t('stab.pf.title')}</p>
      <p className="text-xs text-muted-foreground">{facts.join(' · ')}</p>
      {pf.adjustments?.map((a, i) => (
        <p key={i} className="text-xs text-amber-700 dark:text-amber-300">
          {t('stab.pf.adjusted')}：{a}
        </p>
      ))}
      {pf.trials && pf.trials.length > 0 && (
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
          <span className="text-muted-foreground">{t('stab.pf.trials')}</span>
          {pf.trials.map((tr) => (
            <span key={tr.variant} className={TRIAL_TONE[tr.outcome]} title={tr.detail}>
              <code>{tr.field}</code> {t(`stab.pf.outcome.${tr.outcome}` as DictKey)}
            </span>
          ))}
        </div>
      )}
    </div>
  )
}

// compatSummary 参数快照里的兼容选项一行；全自动且无自定义字段时为空（不展示）
function compatSummary(c: StabilityCompat | undefined, t: (k: DictKey) => string): string {
  if (!c) return ''
  const parts: string[] = []
  if (c.maxTokensField && c.maxTokensField !== 'auto') parts.push(c.maxTokensField)
  if (c.streamUsage && c.streamUsage !== 'auto') parts.push(`${t('stab.compat.streamUsage')} ${t(`stab.compat.usage.${c.streamUsage}` as DictKey)}`)
  if (c.chatCacheControl) parts.push(t('stab.compat.chatCacheControl'))
  if (c.extraBody && Object.keys(c.extraBody).length > 0) parts.push(`${t('stab.compat.extraBody')} ${JSON.stringify(c.extraBody)}`)
  if (c.extraHeaders && Object.keys(c.extraHeaders).length > 0)
    parts.push(`${t('stab.compat.extraHeaders')} ${Object.keys(c.extraHeaders).join(', ')}`)
  return parts.join(' · ')
}

// workloadSummary 参数快照里的负载画像一行
function workloadSummary(w: StabilityWorkload | undefined, t: (k: DictKey) => string): string {
  const input = w?.input
  const parts = [
    !input || input.mode === 'none'
      ? `${t('stab.wl.inputMode')} ${t('stab.wl.mode.none')}`
      : input.mode === 'fixed'
        ? `${t('stab.wl.inputMode')} ${t('stab.wl.mode.fixed')} ${input.value}`
        : `${t('stab.wl.inputMode')} ${t(`stab.wl.mode.${input.mode}` as DictKey)} ${input.min} ~ ${input.max}`,
  ]
  if (w?.cacheHitRate) parts.push(`${t('stab.wl.cacheHitRate')} ${pct(w.cacheHitRate)}`)
  parts.push(`${t('stab.wl.output')} ${w?.output ?? t('stab.wl.outputDefault')}`)
  if (w?.thinking && w.thinking !== 'default') parts.push(`${t('stab.wl.thinking')} ${t(`stab.wl.think.${w.thinking}` as DictKey)}`)
  return parts.join(' · ')
}

export default function StabilityTaskDetailPage() {
  const { taskId = '' } = useParams()
  const { t } = useI18n()
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const taskQ = useQuery({
    queryKey: ['stability-task', taskId],
    queryFn: () => stabilityApi.getTask(taskId),
    retry: false,
    refetchInterval: (q) =>
      q.state.data != null && !isTerminalStatus(q.state.data.status) ? 3000 : false,
  })
  const probes = useQuery({ queryKey: ['stability-probes'], queryFn: stabilityProbesApi.list })

  const task = taskQ.data
  const active = task != null && !isTerminalStatus(task.status)
  // 稳定性 SSE 走独立端点与 query key，泛化 hook 传入 basePath/invalidateKeys
  const live = useTaskEvents(taskId, active, {
    basePath: '/api/stability/tasks',
    invalidateKeys: [
      ['stability-task', taskId],
      ['stability-tasks'],
    ],
  })
  const status = live?.status ?? task?.status
  const done = live?.done ?? task?.progressDone ?? 0
  const total = live?.total ?? task?.progressTotal ?? 0

  // 指标报告仅终态可用（运行中分位数会随采集漂移）；SSE 终态帧翻转 enabled 后自动拉取
  const terminal = status != null && isTerminalStatus(status)
  const report = useQuery({
    queryKey: ['stability-task-metrics', taskId],
    queryFn: () => stabilityApi.getMetrics(taskId),
    enabled: task != null && terminal,
  })

  const [cancelOpen, setCancelOpen] = useState(false)
  const cancel = useMutation({
    mutationFn: () => stabilityApi.cancelTask(taskId),
    onSuccess: () => {
      setCancelOpen(false)
      queryClient.invalidateQueries({ queryKey: ['stability-task', taskId] })
      queryClient.invalidateQueries({ queryKey: ['stability-tasks'] })
    },
  })
  const closeCancelDialog = () => {
    setCancelOpen(false)
    cancel.reset()
  }
  const rerun = useMutation({
    mutationFn: stabilityApi.createTask,
    onSuccess: (created) => {
      queryClient.invalidateQueries({ queryKey: ['stability-tasks'] })
      navigate(`/stability/${created.id}`)
    },
  })

  if (taskQ.isPending) {
    return <p className="text-sm text-muted-foreground">{t('common.loading')}</p>
  }
  if (taskQ.isError || task == null) {
    return (
      <div className="grid gap-4">
        <p className="text-sm text-destructive">{t('quality.notFound')}</p>
        <Button variant="outline" className="w-fit" onClick={() => navigate('/stability')}>
          <ArrowLeft />
          {t('common.back')}
        </Button>
      </div>
    )
  }

  const probeName = (id: string) => probes.data?.find((p) => p.id === id)?.name ?? id
  const pctDone = total > 0 ? (done / total) * 100 : 0
  const canRerun = task.target.channelId != null && task.target.modelEntryId != null

  return (
    <div className="grid gap-6">
      <div className="flex flex-wrap items-center gap-3">
        <Button
          variant="ghost"
          size="icon"
          className="size-8"
          title={t('common.back')}
          onClick={() => navigate('/stability')}
        >
          <ArrowLeft />
        </Button>
        <h1 className="text-2xl font-semibold">{t('stab.taskTitle')}</h1>
        {status && <TaskStatusBadge status={status} />}
        {(status === 'queued' || status === 'running') && (
          <Button variant="outline" className="ml-auto" onClick={() => setCancelOpen(true)}>
            <X />
            {t('quality.cancelTask')}
          </Button>
        )}
        {terminal && (
          <span className="ml-auto" title={canRerun ? undefined : t('quality.rerunUnavailable')}>
            <Button
              variant="outline"
              disabled={!canRerun || rerun.isPending}
              onClick={() =>
                rerun.mutate({
                  channelId: task.target.channelId!,
                  modelEntryId: task.target.modelEntryId!,
                  probes: task.probes,
                  params: task.params,
                })
              }
            >
              {rerun.isPending ? <Loader2 className="animate-spin" /> : <RotateCcw />}
              {t('quality.rerun')}
            </Button>
          </span>
        )}
      </div>
      {rerun.isError && <p className="text-sm text-destructive">{errText(rerun.error)}</p>}

      {status != null && !isTerminalStatus(status) && (
        <div className="flex items-center gap-3">
          <Progress value={pctDone} className="max-w-96" />
          <span className="text-sm tabular-nums text-muted-foreground">
            {done}/{total}
          </span>
        </div>
      )}
      {task.error && (
        <p className="text-sm text-destructive">
          {t('quality.taskError')}：{task.error}
        </p>
      )}

      {terminal &&
        (report.isPending ? (
          <p className="text-sm text-muted-foreground">{t('common.loading')}</p>
        ) : report.data != null ? (
          <MetricsCard taskId={taskId} report={report.data} probeName={probeName} />
        ) : null)}
      <SnapshotCard task={task} probeName={probeName} />

      <Dialog open={cancelOpen} onOpenChange={(o) => { if (!o) closeCancelDialog() }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t('quality.cancelTask')}</DialogTitle>
            <DialogDescription>{t('stab.cancelConfirmDesc')}</DialogDescription>
          </DialogHeader>
          {cancel.isError && <p className="text-sm text-destructive">{errText(cancel.error)}</p>}
          <DialogFooter>
            <Button type="button" variant="outline" onClick={closeCancelDialog}>
              {t('quality.keepRunning')}
            </Button>
            <Button variant="destructive" disabled={cancel.isPending} onClick={() => cancel.mutate()}>
              {cancel.isPending && <Loader2 className="animate-spin" />}
              {t('quality.cancelConfirm')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

// 稳定性指标卡：逐 probe 分区展示——阶梯折线（TTFT 分位数）+ 分档指标表 + 错误分类 + 脚注
function MetricsCard({
  taskId,
  report,
  probeName,
}: {
  taskId: string
  report: StabilityReport
  probeName: (id: string) => string
}) {
  const { t } = useI18n()

  // 按出现顺序归拢 probe，保留注册序即展示序
  const probeIds: string[] = []
  for (const s of report.stages) if (!probeIds.includes(s.probe)) probeIds.push(s.probe)
  // 预检与定标结果写在各检测项的 __overall__ 上，取值相同，取第一份展示
  const preflight = report.stages.find((s) => s.metrics.preflight != null)?.metrics.preflight
  const calibration = report.stages.find((s) => s.metrics.calibration != null)?.metrics.calibration

  return (
    <Card>
      <CardHeader>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="grid gap-1.5">
            <CardTitle>{t('stab.metrics')}</CardTitle>
            <CardDescription>{t('stab.metricsDesc')}</CardDescription>
          </div>
          <Button asChild variant="outline" size="sm">
            <a href={stabilityApi.exportUrl(taskId)} download>
              <FileJson />
              {t('quality.exportJson')}
            </a>
          </Button>
        </div>
      </CardHeader>
      <CardContent className="grid gap-8">
        {report.incomplete && (
          <p className="text-sm text-amber-600 dark:text-amber-400">{t('stab.incompleteNote')}</p>
        )}
        {preflight && <PreflightNote pf={preflight} />}
        {calibration && <CalibrationNote c={calibration} />}
        {report.stages.length === 0 ? (
          <p className="text-sm text-muted-foreground">{t('stab.noMetrics')}</p>
        ) : (
          probeIds.map((pid) => (
            <ProbeSection
              key={pid}
              name={probeName(pid)}
              stages={report.stages
                .filter((s) => s.probe === pid && s.stage !== OVERALL_STAGE)
                .sort((a, b) => a.stageIndex - b.stageIndex)}
              overall={report.stages.find((s) => s.probe === pid && s.stage === OVERALL_STAGE)}
            />
          ))
        )}
        {report.footnotes && report.footnotes.length > 0 && (
          <div className="grid gap-1.5">
            <p className="text-sm font-medium">{t('stab.footnotesTitle')}</p>
            <ul className="list-inside list-disc text-xs text-muted-foreground">
              {report.footnotes.map((f, i) => (
                <li key={i}>{f}</li>
              ))}
            </ul>
          </div>
        )}
      </CardContent>
    </Card>
  )
}

function stageLabel(s: StabilityStageMetric): string {
  return s.metrics.concurrency != null ? String(s.metrics.concurrency) : s.stage
}

// ProbeSection 按 probe 类型分派：TPM（标记 targetTokenRate）与 RPM（标记 targetRate）走各自收敛视图，
// 其余走阶梯视图。TPM 档同样带 targetRate（换算出的请求速率），故须先判 TPM 再判 RPM。
function ProbeSection({
  name,
  stages,
  overall,
}: {
  name: string
  stages: StabilityStageMetric[]
  overall?: StabilityStageMetric
}) {
  const isTpm =
    overall?.metrics.convergedTpm != null || stages.some((s) => s.metrics.targetTokenRate != null)
  const isRpm =
    !isTpm &&
    (overall?.metrics.convergedRpm != null || stages.some((s) => s.metrics.targetRate != null))
  const { t } = useI18n()
  const budgetExhausted = overall?.metrics.budgetExhausted ?? 0
  const om = overall?.metrics
  const deviationExceeded = stages.some((s) => s.metrics.inputDeviation?.exceeded)
  return (
    <div className="grid gap-4">
      <p className="text-sm font-medium">{name}</p>
      {budgetExhausted > 0 && !isTpm && !isRpm && (
        <p className="flex items-start gap-1.5 rounded-md border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-sm text-amber-700 dark:text-amber-300">
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          {t('stab.budgetExhaustedBanner')}
        </p>
      )}
      {om?.cacheMiss && (
        <p className="flex items-start gap-1.5 rounded-md border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-sm text-amber-700 dark:text-amber-300">
          <TriangleAlert className="mt-0.5 size-4 shrink-0" />
          {t('stab.wl.cacheMissBanner')}：{t('stab.wl.target')} {pct(om.cacheExpected ?? 0)}，
          {t('stab.wl.measured')} {om.cacheHitRate == null ? '—' : pct(om.cacheHitRate)}
        </p>
      )}
      {isTpm ? (
        <TpmView stages={stages} overall={overall} />
      ) : isRpm ? (
        <RpmView stages={stages} overall={overall} />
      ) : (
        <LadderView stages={stages} overall={overall} />
      )}
      {deviationExceeded && (
        <p className="text-xs text-amber-700 dark:text-amber-300">{t('stab.wl.deviationExceeded')}</p>
      )}
      {(om?.inputDeviation || om?.outputDeviation) && (
        <p className="text-xs text-muted-foreground">{t('stab.wl.devHint')}</p>
      )}
      <ErrorClassBadges overall={overall} />
      {om?.cacheExpected != null && om.cacheHitRate == null && (
        <p className="text-xs text-muted-foreground">{t('stab.wl.cacheUnknown')}</p>
      )}
      {(om?.cacheHits ?? 0) > 0 && om?.cacheExpected == null && (
        <p className="text-xs text-muted-foreground">
          {t('stab.cacheHitsNote')}：{om!.cacheHits}
        </p>
      )}
    </div>
  )
}

// 阶梯并发视图：TTFT 分位数折线（x=并发档）+ 分档指标表
function LadderView({
  stages,
  overall,
}: {
  stages: StabilityStageMetric[]
  overall?: StabilityStageMetric
}) {
  const { t } = useI18n()
  const cols = workloadCols(overall ? [...stages, overall] : stages)

  const showTtfd = hasReasoningGap(stages)
  // 折线数据：有 TTFT、或单列 TTFD 时有 TTFD 的档入图（整档输出上限用尽时 TTFT 缺测但 TTFD 是真实测量）；
  // 缺测的系列在该档留空（不 connectNulls，不编造点）；x 轴用并发数（缺省回退档标识）
  const chartData = stages
    .filter((s) => s.metrics.ttftMs != null || (showTtfd && s.metrics.ttfdMs != null))
    .map((s) => ({
      label: stageLabel(s),
      p50: s.metrics.ttftMs?.p50,
      p95: s.metrics.ttftMs?.p95,
      p99: s.metrics.ttftMs?.p99,
      ttfdP50: s.metrics.ttfdMs?.p50,
    }))
  const series = showTtfd ? (['ttfdP50', 'p50', 'p95', 'p99'] as const) : (['p50', 'p95', 'p99'] as const)

  return (
    <>
      {chartData.length > 0 && (
        <div className="grid gap-2">
          <p className="text-xs text-muted-foreground">{t('stab.ladderChart')}</p>
          <ChartContainer config={CHART_CONFIG} className="aspect-[3/1] w-full">
            <LineChart data={chartData} margin={{ top: 8, right: 12, bottom: 4, left: 4 }}>
              <CartesianGrid vertical={false} />
              <XAxis
                dataKey="label"
                tickLine={false}
                axisLine={false}
                tickMargin={8}
                label={{ value: t('stab.concurrency'), position: 'insideBottom', offset: -2, fontSize: 11 }}
              />
              <YAxis width={44} tickLine={false} axisLine={false} tickMargin={4} />
              <ChartTooltip
                content={<ChartTooltipContent valueFormatter={(v) => `${v} ms`} />}
              />
              {series.map((k) => (
                <Line
                  key={k}
                  type="monotone"
                  dataKey={k}
                  stroke={`var(--color-${k})`}
                  strokeWidth={2}
                  dot={{ r: 3 }}
                  isAnimationActive={false}
                />
              ))}
            </LineChart>
          </ChartContainer>
        </div>
      )}

      <div className="grid gap-2">
        <p className="text-xs text-muted-foreground">{t('stab.stageTable')}</p>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t('stab.concurrency')}</TableHead>
              <TableHead className="text-right">{t('stab.requests')}</TableHead>
              <TableHead className="text-right">{t('stab.errorRate')}</TableHead>
              <TableHead className="text-right">{t('stab.throughput')}</TableHead>
              <TableHead className="text-right">{t('stab.tokensPerSec')}</TableHead>
              {showTtfd && <TableHead className="text-right">{t('stab.ttfd')} p50</TableHead>}
              <TableHead className="text-right">{t('stab.ttft')} p50</TableHead>
              <TableHead className="text-right">p95</TableHead>
              <TableHead className="text-right">p99</TableHead>
              <WorkloadHeads cols={cols} />
            </TableRow>
          </TableHeader>
          <TableBody>
            {stages.map((s) => (
              <MetricRow key={s.stage} label={stageLabel(s)} m={s.metrics} showTtfd={showTtfd} cols={cols} />
            ))}
            {overall && (
              <MetricRow key="__overall__" label={t('stab.overall')} m={overall.metrics} showTtfd={showTtfd} cols={cols} bold />
            )}
          </TableBody>
        </Table>
      </div>
    </>
  )
}

// RPM 实测视图：错误率随目标到达率变化折线 + 收敛边界竖线，收敛 RPM 数字 + 限速头面板 + 分档表
function RpmView({
  stages,
  overall,
}: {
  stages: StabilityStageMetric[]
  overall?: StabilityStageMetric
}) {
  const { t } = useI18n()
  const cols = workloadCols(overall ? [...stages, overall] : stages)
  const rpmConfig: ChartConfig = { errorRate: { label: t('stab.errorRate'), color: '#dc2626' } }

  // 二分会回访爬坡区间内的速率 → 档序非速率序，按目标速率升序重排画曲线
  const chartData = stages
    .filter((s) => s.metrics.targetRate != null)
    .map((s) => ({
      rate: s.metrics.targetRate!,
      errorRate: Math.round(s.metrics.errorRate * 1000) / 10, // 0..1 分数 → 百分比（一位小数）
    }))
    .sort((a, b) => a.rate - b.rate)

  const convergedRpm = overall?.metrics.convergedRpm
  const boundaryRps = convergedRpm != null ? convergedRpm / 60 : undefined
  const bstate = boundaryState(stages, overall)
  const headerEntries = Object.entries(overall?.metrics.rateLimitHeaders ?? {})

  const sortedStages = [...stages].sort(
    (a, b) => (a.metrics.targetRate ?? 0) - (b.metrics.targetRate ?? 0),
  )

  return (
    <>
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
        <span className="text-xs text-muted-foreground">{t('stab.convergedRpm')}</span>
        <span className="text-lg font-semibold tabular-nums">
          {convergedRpm != null ? `${bstate.lowerBound ? '≥ ' : ''}${Math.round(convergedRpm)}` : '—'}
        </span>
        {boundaryRps != null && (
          <span className="text-xs text-muted-foreground">RPM ≈ {num(boundaryRps)} req/s</span>
        )}
        <BoundaryNotes state={bstate} />
      </div>

      {chartData.length > 0 && (
        <div className="grid gap-2">
          <p className="text-xs text-muted-foreground">{t('stab.rpmChart')}</p>
          <ChartContainer config={rpmConfig} className="aspect-[3/1] w-full">
            <LineChart data={chartData} margin={{ top: 12, right: 12, bottom: 4, left: 4 }}>
              <CartesianGrid vertical={false} />
              <XAxis
                type="number"
                dataKey="rate"
                domain={[0, 'dataMax']}
                tickLine={false}
                axisLine={false}
                tickMargin={8}
                label={{ value: t('stab.targetRate'), position: 'insideBottom', offset: -2, fontSize: 11 }}
              />
              <YAxis width={44} tickLine={false} axisLine={false} tickMargin={4} unit="%" />
              <ChartTooltip content={<ChartTooltipContent valueFormatter={(v) => `${v}%`} />} />
              {boundaryRps != null && !bstate.lowerBound && (
                <ReferenceLine
                  x={boundaryRps}
                  stroke="#16a34a"
                  strokeDasharray="4 4"
                  label={{
                    value: `${Math.round(convergedRpm!)} RPM`,
                    position: 'top',
                    fontSize: 11,
                    fill: '#16a34a',
                  }}
                />
              )}
              <Line
                type="monotone"
                dataKey="errorRate"
                stroke="var(--color-errorRate)"
                strokeWidth={2}
                dot={{ r: 3 }}
                isAnimationActive={false}
              />
            </LineChart>
          </ChartContainer>
        </div>
      )}

      {headerEntries.length > 0 && (
        <div className="grid gap-1.5">
          <p className="text-xs text-muted-foreground">{t('stab.rateLimitHeaders')}</p>
          <div className="flex flex-wrap gap-2">
            {headerEntries.map(([k, v]) => (
              <Badge key={k} variant="outline" className="font-mono text-xs font-normal">
                {k}: {v}
              </Badge>
            ))}
          </div>
        </div>
      )}

      <div className="grid gap-2">
        <p className="text-xs text-muted-foreground">{t('stab.stageTable')}</p>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t('stab.targetRate')}</TableHead>
              <TableHead className="text-right">{t('stab.achievedRate')}</TableHead>
              <TableHead className="text-right">{t('stab.requests')}</TableHead>
              <TableHead className="text-right">{t('stab.errorRate')}</TableHead>
              <TableHead className="text-center">{t('stab.rateLimited')}</TableHead>
              <TableHead className="text-right">{t('stab.ttft')} p50</TableHead>
              <TableHead className="text-right">p95</TableHead>
              <TableHead className="text-right">p99</TableHead>
              <WorkloadHeads cols={cols} />
            </TableRow>
          </TableHeader>
          <TableBody>
            {sortedStages.map((s) => (
              <RpmRow key={s.stage} label={num(s.metrics.targetRate)} m={s.metrics} cols={cols} />
            ))}
            {overall && <RpmRow key="__overall__" label={t('stab.overall')} m={overall.metrics} cols={cols} bold />}
          </TableBody>
        </Table>
      </div>
    </>
  )
}

// ERR_CLASS_TONE 错误分类徽章配色：只有推理是渠道/模型问题（红）
const ERR_CLASS_TONE: Record<string, string> = {
  reasoning_only: 'border-destructive/40 text-destructive',
}

// ErrorClassBadges 错误分类徽章：两类视图共用（源自 __overall__ 的 byErrorClass）。
// 输出上限用尽是渠道正常应答（不算错误），单独一枚琥珀徽章放在「错误分类」之外，免得被读成报错
function ErrorClassBadges({ overall }: { overall?: StabilityStageMetric }) {
  const { t } = useI18n()
  const byClass = overall?.metrics.byErrorClass ?? {}
  const capped = byClass.budget_exhausted ?? 0
  const errorClasses = Object.entries(byClass).filter(([cls, n]) => n > 0 && cls !== 'budget_exhausted')
  if (errorClasses.length === 0 && capped === 0) return null
  return (
    <div className="flex flex-wrap items-center gap-2">
      {capped > 0 && (
        <Tooltip>
          <TooltipTrigger asChild>
            <Badge variant="outline" className="border-amber-500/40 font-normal text-amber-700 dark:text-amber-300">
              {t('errClass.budget_exhausted')} · {capped}
            </Badge>
          </TooltipTrigger>
          <TooltipContent className="max-w-80">{t('errClass.budget_exhausted.hint')}</TooltipContent>
        </Tooltip>
      )}
      {errorClasses.length > 0 && (
        <span className="text-xs text-muted-foreground">{t('stab.byErrorClass')}：</span>
      )}
      {errorClasses.map(([cls, n]) => (
        <Badge key={cls} variant="outline" className={cn('font-normal', ERR_CLASS_TONE[cls])}>
          {t(`errClass.${cls}` as DictKey)} · {n}
        </Badge>
      ))}
    </div>
  )
}

function MetricRow({
  label,
  m,
  bold,
  showTtfd,
  cols,
}: {
  label: string
  m: StabilityMetrics
  bold?: boolean
  showTtfd?: boolean
  cols: WorkloadCols
}) {
  return (
    <TableRow className={rowTone(m, bold)}>
      <TableCell>{label}</TableCell>
      <TableCell className="text-right tabular-nums">{m.requests}</TableCell>
      <TableCell className="text-right tabular-nums">{pct(m.errorRate)}</TableCell>
      <TableCell className="text-right tabular-nums">{num(m.throughputRps)}</TableCell>
      <TableCell className="text-right tabular-nums">{num(m.tokensPerSec)}</TableCell>
      {showTtfd && <TableCell className="text-right tabular-nums">{ms(m.ttfdMs?.p50)}</TableCell>}
      <TableCell className="text-right tabular-nums">{ms(m.ttftMs?.p50)}</TableCell>
      <TableCell className="text-right tabular-nums">{ms(m.ttftMs?.p95)}</TableCell>
      <TableCell className="text-right tabular-nums">{ms(m.ttftMs?.p99)}</TableCell>
      <WorkloadCells cols={cols} m={m} />
    </TableRow>
  )
}

// RpmRow 开环速率档行：目标/达成到达率 + 限速判定 + TTFT 分位数（__overall__ 无速率标注）
function RpmRow({ label, m, bold, cols }: { label: string; m: StabilityMetrics; bold?: boolean; cols: WorkloadCols }) {
  const { t } = useI18n()
  return (
    <TableRow className={rowTone(m, bold)}>
      <TableCell>{label}</TableCell>
      <TableCell className="text-right tabular-nums">{num(m.achievedRate)}</TableCell>
      <TableCell className="text-right tabular-nums">{m.requests}</TableCell>
      <TableCell className="text-right tabular-nums">{pct(m.errorRate)}</TableCell>
      <TableCell className="text-center">
        {m.targetRate == null ? (
          <span className="text-muted-foreground">—</span>
        ) : m.rateLimited ? (
          <Badge variant="outline" className="border-destructive/40 font-normal text-destructive">
            {t('stab.limited')}
          </Badge>
        ) : (
          <span className="text-muted-foreground">—</span>
        )}
      </TableCell>
      <TableCell className="text-right tabular-nums">{ms(m.ttftMs?.p50)}</TableCell>
      <TableCell className="text-right tabular-nums">{ms(m.ttftMs?.p95)}</TableCell>
      <TableCell className="text-right tabular-nums">{ms(m.ttftMs?.p99)}</TableCell>
      <WorkloadCells cols={cols} m={m} />
    </TableRow>
  )
}

// TPM 实测视图：错误率随目标 token 到达率变化折线 + 收敛 TPM 边界竖线 + 实测 token 吞吐分档表
function TpmView({
  stages,
  overall,
}: {
  stages: StabilityStageMetric[]
  overall?: StabilityStageMetric
}) {
  const { t } = useI18n()
  const cols = workloadCols(overall ? [...stages, overall] : stages)
  const tpmConfig: ChartConfig = { errorRate: { label: t('stab.errorRate'), color: '#dc2626' } }

  // 二分会回访 token 速率区间 → 档序非速率序，按目标 token 速率升序重排画曲线
  const chartData = stages
    .filter((s) => s.metrics.targetTokenRate != null)
    .map((s) => ({
      rate: s.metrics.targetTokenRate!,
      errorRate: Math.round(s.metrics.errorRate * 1000) / 10, // 0..1 分数 → 百分比（一位小数）
    }))
    .sort((a, b) => a.rate - b.rate)

  const convergedTpm = overall?.metrics.convergedTpm
  const boundaryTokenRate = convergedTpm != null ? convergedTpm / 60 : undefined
  const bstate = boundaryState(stages, overall)
  const headerEntries = Object.entries(overall?.metrics.rateLimitHeaders ?? {})

  const sortedStages = [...stages].sort(
    (a, b) => (a.metrics.targetTokenRate ?? 0) - (b.metrics.targetTokenRate ?? 0),
  )

  return (
    <>
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
        <span className="text-xs text-muted-foreground">{t('stab.convergedTpm')}</span>
        <span className="text-lg font-semibold tabular-nums">
          {convergedTpm != null ? `${bstate.lowerBound ? '≥ ' : ''}${Math.round(convergedTpm)}` : '—'}
        </span>
        {boundaryTokenRate != null && (
          <span className="text-xs text-muted-foreground">TPM ≈ {num(boundaryTokenRate)} token/s</span>
        )}
        <BoundaryNotes state={bstate} />
      </div>

      {chartData.length > 0 && (
        <div className="grid gap-2">
          <p className="text-xs text-muted-foreground">{t('stab.tpmChart')}</p>
          <ChartContainer config={tpmConfig} className="aspect-[3/1] w-full">
            <LineChart data={chartData} margin={{ top: 12, right: 12, bottom: 4, left: 4 }}>
              <CartesianGrid vertical={false} />
              <XAxis
                type="number"
                dataKey="rate"
                domain={[0, 'dataMax']}
                tickLine={false}
                axisLine={false}
                tickMargin={8}
                label={{ value: t('stab.targetTokenRate'), position: 'insideBottom', offset: -2, fontSize: 11 }}
              />
              <YAxis width={44} tickLine={false} axisLine={false} tickMargin={4} unit="%" />
              <ChartTooltip content={<ChartTooltipContent valueFormatter={(v) => `${v}%`} />} />
              {boundaryTokenRate != null && !bstate.lowerBound && (
                <ReferenceLine
                  x={boundaryTokenRate}
                  stroke="#16a34a"
                  strokeDasharray="4 4"
                  label={{
                    value: `${Math.round(convergedTpm!)} TPM`,
                    position: 'top',
                    fontSize: 11,
                    fill: '#16a34a',
                  }}
                />
              )}
              <Line
                type="monotone"
                dataKey="errorRate"
                stroke="var(--color-errorRate)"
                strokeWidth={2}
                dot={{ r: 3 }}
                isAnimationActive={false}
              />
            </LineChart>
          </ChartContainer>
        </div>
      )}

      {headerEntries.length > 0 && (
        <div className="grid gap-1.5">
          <p className="text-xs text-muted-foreground">{t('stab.rateLimitHeaders')}</p>
          <div className="flex flex-wrap gap-2">
            {headerEntries.map(([k, v]) => (
              <Badge key={k} variant="outline" className="font-mono text-xs font-normal">
                {k}: {v}
              </Badge>
            ))}
          </div>
        </div>
      )}

      <div className="grid gap-2">
        <p className="text-xs text-muted-foreground">{t('stab.stageTable')}</p>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t('stab.targetTokenRate')}</TableHead>
              <TableHead className="text-right">{t('stab.achievedTokenRate')}</TableHead>
              <TableHead className="text-right">{t('stab.requests')}</TableHead>
              <TableHead className="text-right">{t('stab.errorRate')}</TableHead>
              <TableHead className="text-center">{t('stab.rateLimited')}</TableHead>
              <TableHead className="text-right">{t('stab.ttft')} p50</TableHead>
              <TableHead className="text-right">p95</TableHead>
              <TableHead className="text-right">p99</TableHead>
              <WorkloadHeads cols={cols} />
            </TableRow>
          </TableHeader>
          <TableBody>
            {sortedStages.map((s) => (
              <TpmRow key={s.stage} label={num(s.metrics.targetTokenRate)} m={s.metrics} cols={cols} />
            ))}
            {overall && <TpmRow key="__overall__" label={t('stab.overall')} m={overall.metrics} cols={cols} bold />}
          </TableBody>
        </Table>
      </div>
    </>
  )
}

// TpmRow 开环 token 速率档行：目标/达成 token 到达率 + 限速判定 + TTFT 分位数（__overall__ 无速率标注）
function TpmRow({ label, m, bold, cols }: { label: string; m: StabilityMetrics; bold?: boolean; cols: WorkloadCols }) {
  const { t } = useI18n()
  return (
    <TableRow className={rowTone(m, bold)}>
      <TableCell>{label}</TableCell>
      <TableCell className="text-right tabular-nums">{num(m.achievedTokenRate)}</TableCell>
      <TableCell className="text-right tabular-nums">{m.requests}</TableCell>
      <TableCell className="text-right tabular-nums">{pct(m.errorRate)}</TableCell>
      <TableCell className="text-center">
        {m.targetTokenRate == null ? (
          <span className="text-muted-foreground">—</span>
        ) : m.rateLimited ? (
          <Badge variant="outline" className="border-destructive/40 font-normal text-destructive">
            {t('stab.limited')}
          </Badge>
        ) : (
          <span className="text-muted-foreground">—</span>
        )}
      </TableCell>
      <TableCell className="text-right tabular-nums">{ms(m.ttftMs?.p50)}</TableCell>
      <TableCell className="text-right tabular-nums">{ms(m.ttftMs?.p95)}</TableCell>
      <TableCell className="text-right tabular-nums">{ms(m.ttftMs?.p99)}</TableCell>
      <WorkloadCells cols={cols} m={m} />
    </TableRow>
  )
}

// SnapshotCard 参数快照卡：检测对象与任务参数在创建时定格，展示历史事实（含实测协议）
function SnapshotCard({
  task,
  probeName,
}: {
  task: StabilityTask
  probeName: (id: string) => string
}) {
  const { t } = useI18n()
  const tg = task.target
  const p = task.params
  const startedAt = task.startedAt ? new Date(task.startedAt) : null
  const finishedAt = task.finishedAt ? new Date(task.finishedAt) : null

  const rows: [string, React.ReactNode][] = [
    [t('quality.channel'), tg.channelName],
    [t('channels.baseUrl'), tg.baseUrl],
    [t('quality.model'), tg.model],
    [
      t('stab.protocol'),
      p.protocol ? (
        <Badge key="protocol" variant="outline">
          {t(`proto.${p.protocol}` as DictKey)}
        </Badge>
      ) : (
        '—'
      ),
    ],
    [t('quality.probes'), task.probes.map(probeName).join('、')],
    [
      t('quality.paramsLabel'),
      `${t('stab.ladder')} [${p.concurrencyLadder.join(', ')}] · ${t('stab.requestsPerStage')} ${p.requestsPerStage} · ${t('stab.warmupPerStage')} ${p.warmupPerStage}`,
    ],
    [t('stab.wl.title'), workloadSummary(p.workload, t)],
    ...(compatSummary(p.compat, t) ? [[t('stab.compat.title'), compatSummary(p.compat, t)] as [string, string]] : []),
    [
      t('stab.maxTotalRequests'),
      `${p.maxTotalRequests} · ${t('stab.maxTotalTokens')} ${p.maxTotalTokens} · ${t('stab.maxDurationSec')} ${p.maxDurationSec} · ${t('stab.requestTimeout')} ${p.requestTimeoutMs}`,
    ],
  ]
  // RPM 实测参数仅在勾选 rpm_probe 时定格展示
  if (task.probes.includes('rpm_probe')) {
    rows.push([
      t('stab.rpmParams'),
      `${p.rpmStartRate} → ${p.rpmMaxRate} req/s · ${t('stab.rpmStageSec')} ${p.rpmStageSec}s · ${t('stab.rpmMaxInFlight')} ${p.rpmMaxInFlight} · ${t('stab.rpmLimitThreshold')} ${pct(p.rpmLimitThreshold)} · ${t('stab.rpmBinarySteps')} ${p.rpmBinarySteps}`,
    ])
  }
  // TPM 实测参数仅在勾选 tpm_probe 时定格展示
  if (task.probes.includes('tpm_probe')) {
    rows.push([
      t('stab.tpmParams'),
      `${p.tpmStartRate} → ${p.tpmMaxRate} token/s · ${t('stab.tpmStageSec')} ${p.tpmStageSec}s · ${t('stab.tpmMaxInFlight')} ${p.tpmMaxInFlight} · ${t('stab.tpmLimitThreshold')} ${pct(p.tpmLimitThreshold)} · ${t('stab.tpmBinarySteps')} ${p.tpmBinarySteps}`,
    ])
  }
  rows.push([
    t('quality.createdAt'),
    `${new Date(task.createdAt).toLocaleString()}${task.createdBy ? ` · ${task.createdBy}` : ''}`,
  ])
  if (startedAt) rows.push([t('quality.startedAt'), startedAt.toLocaleString()])
  if (finishedAt) {
    rows.push([
      t('quality.finishedAt'),
      `${finishedAt.toLocaleString()}${
        startedAt
          ? ` · ${t('quality.duration')} ${formatDuration(finishedAt.getTime() - startedAt.getTime())}`
          : ''
      }`,
    ])
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle>{t('quality.snapshotTitle')}</CardTitle>
        <CardDescription>{t('quality.snapshotDesc')}</CardDescription>
      </CardHeader>
      <CardContent>
        <dl className="grid gap-x-8 gap-y-2 text-sm sm:grid-cols-[auto_1fr]">
          {rows.map(([label, value]) => (
            <Fragment key={label}>
              <dt className="text-muted-foreground">{label}</dt>
              <dd className="break-all">{value}</dd>
            </Fragment>
          ))}
        </dl>
      </CardContent>
    </Card>
  )
}
