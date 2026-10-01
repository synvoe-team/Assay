package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/Yukiho0287/assay/server/internal/db"
	"github.com/Yukiho0287/assay/server/internal/probe"
	"github.com/Yukiho0287/assay/server/internal/probe/protocol"
	"github.com/Yukiho0287/assay/server/internal/probe/stability"
	stabreg "github.com/Yukiho0287/assay/server/internal/probe/stability/registry"
)

// StabilityArgs 稳定性检测任务的 River 载荷：只带任务 ID，其余全部执行时从 tasks 行现读。
type StabilityArgs struct {
	TaskID uuid.UUID `json:"taskId"`
}

func (StabilityArgs) Kind() string { return "stability_task" }

func (StabilityArgs) InsertOpts() river.InsertOpts {
	// 路由到独立 stability 队列；总尝试 2 次，实例被杀后重启的实例自动重跑一次。
	return river.InsertOpts{MaxAttempts: 2, Queue: QueueStability}
}

// StabilityWorker 稳定性检测执行器。跑在独立 "stability" 队列（MaxWorkers=1），
// 与质量任务并行、各自内部串行；状态机守卫与质量一致（MarkTaskRunning/FinishTask 前置条件）。
type StabilityWorker struct {
	river.WorkerDefaults[StabilityArgs]
	pool   *pgxpool.Pool
	q      *db.Queries
	log    *slog.Logger
	client *http.Client
}

// Timeout -1 关闭 job 级超时：压测任务可能跑几十分钟，超时控制在每请求 ctx（params.RequestTimeoutMs）。
func (w *StabilityWorker) Timeout(*river.Job[StabilityArgs]) time.Duration { return -1 }

func (w *StabilityWorker) Work(ctx context.Context, job *river.Job[StabilityArgs]) error {
	taskID := job.Args.TaskID
	err := w.work(ctx, taskID)
	if err != nil && job.Attempt >= job.MaxAttempts {
		// 最后一次尝试也失败：job 即将 discard，必须在这里落任务终态。
		// 用 WithoutCancel：即使失败原因是 ctx 取消，终态也要写进 DB。
		w.failTask(context.WithoutCancel(ctx), taskID, err)
	}
	return err
}

func (w *StabilityWorker) work(ctx context.Context, taskID uuid.UUID) error {
	rows, err := w.q.MarkTaskRunning(ctx, taskID)
	if err != nil {
		return fmt.Errorf("标记任务运行中: %w", err)
	}
	if rows == 0 {
		// 已被取消或已终结（入队后、执行前用户取消）：静默跳过
		w.log.Info("任务已终结，跳过执行", "task", taskID)
		return nil
	}

	task, err := w.q.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("读取任务: %w", err)
	}
	var target probe.Target
	if err := json.Unmarshal(task.Target, &target); err != nil {
		return fmt.Errorf("解析任务快照: %w", err)
	}
	var params stability.StabilityParams
	if err := json.Unmarshal(task.Params, &params); err != nil {
		return fmt.Errorf("解析任务参数: %w", err)
	}
	params.ApplyDefaults()
	if err := params.Validate(); err != nil {
		return fmt.Errorf("任务参数非法: %w", err)
	}
	total := int(task.ProgressTotal)
	NotifyEvent(ctx, w.pool, w.log, Event{TaskID: taskID, Status: "running", Done: 0, Total: total})

	// 重试幂等：清掉上次中断留下的半截时序 + 聚合，整个任务从头重跑
	if err := w.q.DeleteStabilitySamples(ctx, taskID); err != nil {
		return fmt.Errorf("清理旧样本: %w", err)
	}
	if err := w.q.DeleteStabilityMetrics(ctx, taskID); err != nil {
		return fmt.Errorf("清理旧指标: %w", err)
	}
	if task.ProgressDone != 0 {
		if err := w.q.UpdateTaskProgress(ctx, db.UpdateTaskProgressParams{ID: taskID, ProgressTotal: task.ProgressTotal}); err != nil {
			return fmt.Errorf("重置进度: %w", err)
		}
	}

	// API key 执行时现读（绝不进快照）；渠道已删则任务失败
	if !task.ChannelID.Valid {
		return errors.New("渠道已被删除，无法执行")
	}
	secret, err := w.q.GetChannelSecret(ctx, uuid.UUID(task.ChannelID.Bytes))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("渠道已被删除，无法执行")
		}
		return fmt.Errorf("读取渠道密钥: %w", err)
	}

	codec, ok := protocol.Get(params.Protocol)
	if !ok {
		return fmt.Errorf("未知协议 %q", params.Protocol)
	}

	// 全局成本硬闸跨所有 probe 共享：任一 probe 打满总请求/总 token/整任务时长上限即收敛。
	caps := stability.NewCapGuard(params.MaxTotalRequests, params.MaxTotalTokens, time.Duration(params.MaxDurationSec)*time.Second)

	// 按注册顺序执行各检测项，offset 把各 probe 的局部进度串成全局进度
	offset := 0
	for _, id := range task.Probes {
		p, ok := stabreg.Get(id)
		if !ok {
			return fmt.Errorf("未知检测项 %q", id)
		}
		curOffset := offset
		in := stability.RunInput{
			Probe:  id,
			Target: target,
			APIKey: secret.ApiKey,
			Nonce:  taskID.String()[:8],
			Params: params,
			Client: w.client,
			Codec:  codec,
			Caps:   caps,
			Sample: func(ctx context.Context, s stability.Sample) error {
				return w.q.InsertStabilitySample(ctx, sampleParams(taskID, id, s))
			},
			Metric: func(ctx context.Context, m stability.StageMetrics) error {
				blob, err := json.Marshal(m.Metrics)
				if err != nil {
					return fmt.Errorf("序列化指标: %w", err)
				}
				return w.q.UpsertStabilityMetric(ctx, db.UpsertStabilityMetricParams{
					TaskID:     taskID,
					Probe:      m.Probe,
					Stage:      m.Stage,
					StageIndex: int32(m.StageIndex),
					Metrics:    blob,
				})
			},
			Progress: func(ctx context.Context, done, _ int) {
				global := curOffset + done
				if err := w.q.UpdateTaskProgress(ctx, db.UpdateTaskProgressParams{
					ID: taskID, ProgressTotal: task.ProgressTotal, ProgressDone: int32(global),
				}); err != nil {
					w.log.Warn("写进度失败", "task", taskID, "err", err)
					return
				}
				NotifyEvent(ctx, w.pool, w.log, Event{TaskID: taskID, Status: "running", Done: global, Total: total})
			},
		}
		if err := p.Run(ctx, in); err != nil {
			return fmt.Errorf("检测项 %s: %w", id, err)
		}
		// probe 提前收敛（碰硬闸）时实发请求可能少于预估，offset 仍按预估推进，
		// 保证多 probe 的进度不重叠；任务照常 succeeded（total 不必打满）。
		offset += p.Info.EstRequests(params)
	}

	if _, err := w.q.FinishTask(ctx, db.FinishTaskParams{ID: taskID, Status: "succeeded"}); err != nil {
		return fmt.Errorf("落任务终态: %w", err)
	}
	NotifyEvent(ctx, w.pool, w.log, Event{TaskID: taskID, Status: "succeeded", Done: total, Total: total})
	w.log.Info("稳定性任务完成", "task", taskID)
	return nil
}

// failTask 把任务标记为 failed 并广播终态。FinishTask 有 running 守卫，
// 已终结的任务不会被覆盖。
func (w *StabilityWorker) failTask(ctx context.Context, taskID uuid.UUID, cause error) {
	msg := truncateError(cause.Error(), 500)
	rows, err := w.q.FinishTask(ctx, db.FinishTaskParams{ID: taskID, Status: "failed", Error: &msg})
	if err != nil {
		w.log.Error("落任务失败终态出错", "task", taskID, "err", err)
		return
	}
	if rows == 0 {
		return
	}
	task, err := w.q.GetTask(ctx, taskID)
	if err != nil {
		w.log.Warn("读任务进度失败，终态通知用零值", "task", taskID, "err", err)
	}
	NotifyEvent(ctx, w.pool, w.log, Event{TaskID: taskID, Status: "failed", Done: int(task.ProgressDone), Total: int(task.ProgressTotal)})
	w.log.Error("稳定性任务失败", "task", taskID, "err", cause)
}

// sampleParams stability.Sample → sqlc 参数：负值/空串 → SQL NULL，
// 报告里区分「没测到」与「测到 0」。
func sampleParams(taskID uuid.UUID, probeID string, s stability.Sample) db.InsertStabilitySampleParams {
	return db.InsertStabilitySampleParams{
		TaskID:       taskID,
		Probe:        probeID,
		Stage:        s.Stage,
		StageIndex:   int32(s.StageIndex),
		Seq:          int32(s.Seq),
		Protocol:     s.Protocol,
		DispatchedAt: s.DispatchedAt,
		TtfbMs:       int4OrNull(s.TTFBms),
		TtfdMs:       int4OrNull(s.TTFDms),
		TtftMs:       int4OrNull(s.TTFTms),
		TotalMs:      int4OrNull(s.TotalMs),
		Ok:           s.Ok,
		HttpStatus:   int4Positive(s.HTTPStatus), // 0（传输层未拿到状态）→ NULL
		HttpProto:    strOrNull(s.HTTPProto),
		ErrorClass:   strOrNull(s.ErrorClass),
		Error:        strOrNull(s.Error),
		InputTokens:  int4OrNull(s.InputTokens),
		OutputTokens: int4OrNull(s.OutputTokens),
		CachedTokens: int4OrNull(s.CachedTokens),
		Warmup:       s.Warmup,
	}
}

// int4OrNull v<0 → SQL NULL（「没测到」），否则有效值（含 0 = 「测到 0」）。
func int4OrNull(v int) pgtype.Int4 {
	if v < 0 {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(v), Valid: true}
}

// int4Positive v<=0 → SQL NULL，否则有效值。HTTP 状态用：0 表示传输层未拿到状态。
func int4Positive(v int) pgtype.Int4 {
	if v <= 0 {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(v), Valid: true}
}

func strOrNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
