import * as React from 'react'
import * as RechartsPrimitive from 'recharts'
import { cn } from '@/lib/utils'

// 轻量 shadcn 风格图表容器：为每个系列注入 --color-<key> CSS 变量供 recharts 引用，
// 统一暗色/亮色下的坐标轴、网格、tooltip 视觉。适配 recharts 3（上游组件仅兼容 2.x）。
export type ChartConfig = Record<string, { label?: React.ReactNode; color?: string }>

interface ChartContextValue {
  config: ChartConfig
}

const ChartContext = React.createContext<ChartContextValue | null>(null)

function useChart(): ChartContextValue {
  const ctx = React.useContext(ChartContext)
  if (!ctx) throw new Error('useChart 必须在 <ChartContainer> 内使用')
  return ctx
}

function ChartContainer({
  id,
  className,
  children,
  config,
  ...props
}: React.ComponentProps<'div'> & {
  config: ChartConfig
  children: React.ComponentProps<typeof RechartsPrimitive.ResponsiveContainer>['children']
}) {
  const uniqueId = React.useId()
  const chartId = `chart-${(id ?? uniqueId).replace(/:/g, '')}`

  return (
    <ChartContext.Provider value={{ config }}>
      <div
        data-slot="chart"
        data-chart={chartId}
        className={cn(
          "flex aspect-video justify-center text-xs [&_.recharts-cartesian-axis-tick_text]:fill-muted-foreground [&_.recharts-cartesian-grid_line[stroke='#ccc']]:stroke-border/50 [&_.recharts-curve.recharts-tooltip-cursor]:stroke-border [&_.recharts-layer]:outline-hidden [&_.recharts-surface]:outline-hidden",
          className,
        )}
        {...props}
      >
        <ChartStyle id={chartId} config={config} />
        <RechartsPrimitive.ResponsiveContainer>{children}</RechartsPrimitive.ResponsiveContainer>
      </div>
    </ChartContext.Provider>
  )
}

// 把 config 里各系列的颜色编译成 [data-chart=id] 作用域下的 CSS 变量
function ChartStyle({ id, config }: { id: string; config: ChartConfig }) {
  const colored = Object.entries(config).filter(([, c]) => c.color)
  if (colored.length === 0) return null
  return (
    <style
      dangerouslySetInnerHTML={{
        __html: `[data-chart=${id}] {\n${colored
          .map(([key, c]) => `  --color-${key}: ${c.color};`)
          .join('\n')}\n}`,
      }}
    />
  )
}

const ChartTooltip = RechartsPrimitive.Tooltip

// recharts 在运行时注入 active/payload/label；这里全部按可选处理，避免 3.x 严格 payload 类型摩擦
interface TooltipPayloadItem {
  name?: string | number
  value?: string | number
  color?: string
  dataKey?: string | number
  payload?: Record<string, unknown>
}

function ChartTooltipContent({
  active,
  payload,
  label,
  labelFormatter,
  valueFormatter,
  hideLabel = false,
  className,
}: {
  active?: boolean
  payload?: TooltipPayloadItem[]
  label?: React.ReactNode
  labelFormatter?: (label: React.ReactNode) => React.ReactNode
  valueFormatter?: (value: number | string, name: string) => React.ReactNode
  hideLabel?: boolean
  className?: string
}) {
  const { config } = useChart()
  if (!active || !payload || payload.length === 0) return null

  return (
    <div
      className={cn(
        'grid min-w-36 gap-1.5 rounded-lg border bg-background px-3 py-2 text-xs shadow-md',
        className,
      )}
    >
      {!hideLabel && (
        <p className="font-medium">{labelFormatter ? labelFormatter(label) : label}</p>
      )}
      <div className="grid gap-1">
        {payload.map((item, i) => {
          const key = String(item.dataKey ?? item.name ?? i)
          const name = config[key]?.label ?? item.name ?? key
          const color = config[key]?.color ?? item.color
          return (
            <div key={key + i} className="flex items-center justify-between gap-3">
              <span className="flex items-center gap-1.5 text-muted-foreground">
                <span
                  className="size-2 shrink-0 rounded-[2px]"
                  style={{ backgroundColor: color }}
                />
                {name}
              </span>
              <span className="font-mono font-medium tabular-nums text-foreground">
                {item.value != null && valueFormatter
                  ? valueFormatter(item.value, String(name))
                  : item.value}
              </span>
            </div>
          )
        })}
      </div>
    </div>
  )
}

export { ChartContainer, ChartTooltip, ChartTooltipContent, useChart }
