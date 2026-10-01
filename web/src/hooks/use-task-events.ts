import { useEffect, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import type { TaskProgressEvent, TaskStatus } from '@/lib/api'

export function isTerminalStatus(s: TaskStatus): boolean {
  return s === 'succeeded' || s === 'failed' || s === 'canceled'
}

// 质量 / 稳定性两大类共用同一进度契约（tasks.Event），差别只在 SSE 路径与终态失效的
// query key，故用可选 opts 泛化；缺省保持质量语义，稳定性详情传入自己的 basePath/keys。
interface TaskEventsOptions {
  basePath?: string // SSE 端点前缀，如 '/api/stability/tasks'
  invalidateKeys?: readonly unknown[][] // 终态到达后需失效的 query key 列表
}

// 订阅任务进度 SSE：EventSource 同源自带 cookie；服务端连上先推 DB 快照帧，
// 断线后浏览器自动重连、重连后再收一帧快照补平，前端无需自管重连。
// 终态帧到达即关闭连接并失效相关查询。
export function useTaskEvents(
  taskId: string,
  enabled: boolean,
  opts?: TaskEventsOptions,
): TaskProgressEvent | null {
  const queryClient = useQueryClient()
  const [live, setLive] = useState<TaskProgressEvent | null>(null)
  const basePath = opts?.basePath ?? '/api/quality/tasks'
  // 默认失效质量三键；useEffect 依赖不含 keys 本身，用 JSON 稳定标识避免每渲染重订阅
  const keys = opts?.invalidateKeys ?? [
    ['quality-task', taskId],
    ['quality-task-results', taskId],
    ['quality-tasks'],
  ]
  const keysId = JSON.stringify(keys)

  useEffect(() => {
    if (!enabled || !taskId) return
    const es = new EventSource(`${basePath}/${taskId}/events`)
    es.onmessage = (e) => {
      let ev: TaskProgressEvent
      try {
        ev = JSON.parse(e.data) as TaskProgressEvent
      } catch {
        return
      }
      // 并发槽位各自上报，帧序可能轻微乱序：同状态下 done 只增不减，避免进度条回跳
      setLive((prev) =>
        prev && prev.status === ev.status && prev.done > ev.done ? prev : ev,
      )
      if (isTerminalStatus(ev.status)) {
        es.close()
        for (const queryKey of keys) queryClient.invalidateQueries({ queryKey })
      }
    }
    return () => es.close()
    // keysId 稳定标识 keys 内容变化；basePath 变化也需重订阅
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [taskId, enabled, basePath, keysId, queryClient])

  return live
}
