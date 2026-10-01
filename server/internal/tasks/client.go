package tasks

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/Yukiho0287/assay/server/internal/db"
)

// QueueStability 稳定性任务的独立 River 队列名（与质量任务队列并行、各自内部串行）。
const QueueStability = "stability"

// Client 封装 River 队列：入队（与任务行同事务）、取消、启动/停机、孤儿清扫。
type Client struct {
	river *river.Client[pgx.Tx]
	pool  *pgxpool.Pool
	q     *db.Queries
	log   *slog.Logger
}

// New 构建队列客户端 + 进程内 worker。MaxWorkers=1：单实例同时只跑一个任务
// （任务内部再按 params.concurrency 扇出请求），蓝绿双实例期间靠 River 的
// job 级锁保证同一任务不被双跑。
func New(pool *pgxpool.Pool, log *slog.Logger) (*Client, error) {
	q := db.New(pool)
	workers := river.NewWorkers()
	river.AddWorker(workers, &QualityWorker{
		pool:   pool,
		q:      q,
		log:    log,
		client: workerHTTPClient(),
	})
	river.AddWorker(workers, &StabilityWorker{
		pool:   pool,
		q:      q,
		log:    log,
		client: stabilityHTTPClient(),
	})

	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		// 质量 / 稳定性两条独立队列各 MaxWorkers=1：两大类可并行，各自内部串行。
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 1},
			QueueStability:     {MaxWorkers: 1},
		},
		Workers: workers,
		Logger:  log.WithGroup("river"),
		// 停机时给在跑任务 5s 收尾，超时取消其 ctx 硬停；
		// 被打断的任务由 MaxAttempts=2 在重启后的实例上自动重跑
		SoftStopTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("初始化 River: %w", err)
	}
	return &Client{river: rc, pool: pool, q: q, log: log}, nil
}

// Start 先清扫孤儿任务再开始取活。
func (c *Client) Start(ctx context.Context) error {
	if err := c.SweepOrphans(ctx); err != nil {
		return err
	}
	return c.river.Start(ctx)
}

// Stop 优雅停机（SoftStopTimeout 到点自动升级为硬停），等到 worker 完全停止才返回。
// 若停机已由 Start 的 ctx 取消触发，river.Stop 会立即返回，这里再等 Stopped 兜底。
func (c *Client) Stop(ctx context.Context) error {
	if err := c.river.Stop(ctx); err != nil {
		return err
	}
	select {
	case <-c.river.Stopped():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// EnqueueQualityTaskTx 在调用方事务里入队，返回 river job ID（写回 tasks.river_job_id）。
// 与 CreateTask 同一事务：任务行与队列条目要么都在要么都不在。
func (c *Client) EnqueueQualityTaskTx(ctx context.Context, tx pgx.Tx, taskID uuid.UUID) (int64, error) {
	res, err := c.river.InsertTx(ctx, tx, QualityArgs{TaskID: taskID}, nil)
	if err != nil {
		return 0, fmt.Errorf("任务入队: %w", err)
	}
	return res.Job.ID, nil
}

// EnqueueStabilityTaskTx 在调用方事务里入队稳定性任务，返回 river job ID。
// 与 CreateTask 同一事务；StabilityArgs.InsertOpts 把它路由到 stability 队列。
func (c *Client) EnqueueStabilityTaskTx(ctx context.Context, tx pgx.Tx, taskID uuid.UUID) (int64, error) {
	res, err := c.river.InsertTx(ctx, tx, StabilityArgs{TaskID: taskID}, nil)
	if err != nil {
		return 0, fmt.Errorf("稳定性任务入队: %w", err)
	}
	return res.Job.ID, nil
}

// CancelJob 取消 river job：排队中的原子取消；运行中的经 LISTEN/NOTIFY 通知执行端
// （跨进程，蓝绿双实例也可达）取消其 work ctx——尽力而为，job 恰好跑完则保持 completed。
// job 已不存在视为无需取消（任务行状态才是权威）。
func (c *Client) CancelJob(ctx context.Context, jobID int64) error {
	_, err := c.river.JobCancel(ctx, jobID)
	if errors.Is(err, rivertype.ErrNotFound) {
		return nil
	}
	return err
}

// SweepOrphans 启动时兜底：任务挂在 running 但对应 river job 已终结（completed/
// cancelled/discarded）或不存在的，标记 failed。
// ⚠️ 只认终结态——job 还在 running/retryable 的绝不动：蓝绿切换期间可能是对端实例
// 在正常执行（卡死的 job 由 River rescuer 处理），误清会把活任务标死。
func (c *Client) SweepOrphans(ctx context.Context) error {
	// river_job 表不在 sqlc schema 里（River 自管迁移），用裸 SQL 联查
	rows, err := c.pool.Query(ctx, `
		select t.id
		from tasks t
		left join river_job j on j.id = t.river_job_id
		where t.status = 'running'
		  and (t.river_job_id is null or j.id is null
		       or j.state in ('completed', 'cancelled', 'discarded'))`)
	if err != nil {
		return fmt.Errorf("查询孤儿任务: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return fmt.Errorf("收集孤儿任务: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	msg := "服务重启导致任务中断"
	n, err := c.q.FailOrphanTasks(ctx, db.FailOrphanTasksParams{Error: &msg, Ids: ids})
	if err != nil {
		return fmt.Errorf("清扫孤儿任务: %w", err)
	}
	c.log.Warn("孤儿任务已标记失败", "count", n, "ids", ids)
	for _, id := range ids {
		NotifyEvent(ctx, c.pool, c.log, Event{TaskID: id, Status: "failed"})
	}
	return nil
}

// workerHTTPClient 质量检测请求专用连接池：质量任务并发上限 16，空闲连接留到 32 足以复用、
// 避免高并发下反复握手。超时不设全局值，由每请求 ctx 控制。
func workerHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = 64
	tr.MaxIdleConnsPerHost = 32
	return &http.Client{Transport: tr}
}

// stabilityHTTPClient 稳定性压测专用连接池，固定 HTTP/1.1、一请求一连接：
//   - 对齐官方 Python/Node SDK 的默认行为与手工压测基线，测量口径一致；
//   - h2 会把全部并发复用进一条 TCP 连接——对端按连接分流时压力全落在一台后端，
//     大请求体上传还会被 h2 流控窗口拖慢，测出来的是我们的传输方式而不是渠道。
//
// TLSNextProto 置为非 nil 空表即关闭 h2 协商（ForceAttemptHTTP2 也一并关掉）。
// 空闲连接上限对齐在途上限的校验上限（4096），高在途时不因连接被回收而反复握手。
func stabilityHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ForceAttemptHTTP2 = false
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	tr.MaxIdleConns = 4096
	tr.MaxIdleConnsPerHost = 4096
	return &http.Client{Transport: tr}
}
