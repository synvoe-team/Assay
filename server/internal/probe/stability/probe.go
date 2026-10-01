package stability

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Yukiho0287/assay/server/internal/probe"
	"github.com/Yukiho0287/assay/server/internal/probe/protocol"
)

// Info 稳定性检测项元数据，与 api.StabilityProbeInfo 对应。无 Checkpoints
// （稳定性产出的是指标非 pass/fail），registry 只校验 ID 唯一/非空。
type Info struct {
	ID          string
	Name        string
	Description string
	Protocols   []string // 适用协议（nil = 全适用）
	// EstRequests 给定参数的最坏预估请求数（算 progress_total + 成本预估上界）
	EstRequests func(p StabilityParams) int
}

// Probe 一个稳定性检测项：元数据 + 运行入口（对齐 quality 的纯结构体范式）。
type Probe struct {
	Info Info
	Run  func(ctx context.Context, in RunInput) error
}

// RunInput 稳定性 probe 运行所需输入；回调由 worker 提供（落库 + 进度）。
type RunInput struct {
	Probe  string // 当前 probe ID，填入 Sample/StageMetrics
	Target probe.Target
	APIKey string // 执行时现读，绝不进快照
	// Nonce 任务级随机前缀（取任务 ID 前 8 位），与档位/序号一起拼进每条 prompt，
	// 让整个任务内无两条请求字节相同——破渠道的整条响应缓存/去重，否则 TTFT 不真实
	Nonce  string
	Params StabilityParams
	// Plan 准备期（PrepareWorkload）产出的字符/token 比与共享前缀；不塑形时为零值
	Plan   Plan
	Client *http.Client
	Codec  protocol.Codec
	Caps   *CapGuard // 全局硬闸（跨 probe 共享），nil = 不限

	// Sample 每次压测请求出结果立即回调（逐样本落 stability_samples，实时证据）
	Sample func(ctx context.Context, s Sample) error
	// Metric 每档（及 __overall__）评估完回调（落 stability_metrics）
	Metric func(ctx context.Context, m StageMetrics) error
	// Progress 每完成一个请求回调（写进度 + pg_notify）
	Progress func(ctx context.Context, done, total int)
}

// Sample 一次压测请求的观测结果，对应 stability_samples 一行。
// 失败样本的延迟/计量字段取负值（<0），落库时转 SQL NULL —— 报告里
// 区分「没测到」与「测到 0」。
type Sample struct {
	Stage        string
	StageIndex   int
	Seq          int
	Protocol     string
	DispatchedAt time.Time
	Warmup       bool

	Ok         bool
	HTTPStatus int    // 0 = 传输层未拿到状态 → NULL
	HTTPProto  string // 实际协商的协议版本；"" = 传输层未拿到响应 → NULL
	ErrorClass string // "" = 成功 → NULL
	Error      string // "" → NULL

	TTFBms  int // <0 → NULL
	TTFDms  int // 首个非空增量（推理或正文）耗时；<0 → NULL
	TTFTms  int // 首个非空正文增量耗时；<0 → NULL
	TotalMs int // <0 → NULL

	InputTokens  int // <0 = 无 usage → NULL
	OutputTokens int // <0 = 无 usage → NULL
	CachedTokens int // 输入中命中缓存的 token；<0 = 无 usage 或上游不报缓存字段 → NULL
	// ReasoningTokens 输出中的推理 token（usage 明细）；<0 = 无 usage → NULL。隐藏推理的模型只有这一个信号
	ReasoningTokens int

	TargetInputTokens  int // 负载画像的输入 token 目标；0 = 不塑形 → NULL
	TargetOutputTokens int // 输出 token 目标；0 = 无目标 → NULL
}

// 错误分类（与迁移 check 约束一致）
const (
	ErrTransport     = "transport"      // 连接/超时，未拿到 HTTP 状态
	ErrHTTP4xx       = "http_4xx"       // 4xx（非 429）
	ErrRateLimited   = "rate_limited"   // 429
	ErrHTTP5xx       = "http_5xx"       // 5xx
	ErrStreamAnomaly = "stream_anomaly" // 流式分片坏损/读流中断/无结束帧/流内错误事件
	ErrSemanticEmpty = "semantic_empty" // 200 但无任何增量

	// ErrBudgetExhausted 输出上限（max_tokens）被思考用完、还没写出正文：渠道正常应答，
	// 只是这条测不到 TTFT。不算错误（见 served），单独计数提醒调大输出上限
	ErrBudgetExhausted = "budget_exhausted"
	// ErrReasoningOnly 只有推理增量、没有正文就正常结束（上限之内）：渠道/模型问题，期望 0
	ErrReasoningOnly = "reasoning_only"
)

// served 渠道是否正常应答了这条请求：成功，或输出上限被思考用完（我们给的上限小，不是渠道的错）。
// 正常应答的样本首字节/首增量/耗时/吞吐/token 消耗都是真实测量，一律计入；只有 TTFT 因无正文而缺测。
func served(s Sample) bool {
	return s.Ok || s.ErrorClass == ErrBudgetExhausted
}

// StageOverall __overall__ 档标识 + 其排序序号（排在所有真实档之后）
const (
	StageOverall      = "__overall__"
	StageOverallIndex = -1
)

// Percentiles 一组延迟观测的分位数摘要（评估期确定性计算）
type Percentiles struct {
	P50 int     `json:"p50"`
	P95 int     `json:"p95"`
	P99 int     `json:"p99"`
	Min int     `json:"min"`
	Max int     `json:"max"`
	Avg float64 `json:"avg"`
}

// Metrics 一个（probe × 档位）的指标集，序列化进 stability_metrics.metrics jsonb。
type Metrics struct {
	Requests  int     `json:"requests"`
	Errors    int     `json:"errors"`
	ErrorRate float64 `json:"errorRate"`

	TTFTms  *Percentiles `json:"ttftMs,omitempty"`
	TTFDms  *Percentiles `json:"ttfdMs,omitempty"` // 首个增量（推理或正文）；非推理模型≈TTFT
	TTFBms  *Percentiles `json:"ttfbMs,omitempty"`
	TotalMs *Percentiles `json:"totalMs,omitempty"`

	// BudgetExhausted 输出上限被思考用完、没写出正文的条数：渠道正常应答，不计入 Errors；
	// 这些请求测不到 TTFT，阶梯并发里 >0 即提示调大输出上限
	BudgetExhausted int `json:"budgetExhausted,omitempty"`
	// CacheHits 输入命中缓存的正常应答条数。h=0 时是意外命中，不进延迟分位（TTFT 不代表渠道真实 prefill）；
	// h>0 时命中是负载画像的一部分，照常计入
	CacheHits int `json:"cacheHits,omitempty"`

	// —— 负载画像（不塑形 / 无目标时留空）——
	InputDeviation  *Deviation `json:"inputDeviation,omitempty"`  // 目标输入 vs 实测 prompt_tokens
	OutputDeviation *Deviation `json:"outputDeviation,omitempty"` // 目标输出 vs 实测 completion
	// CacheHitRate 实测命中率 Σcached/Σinput（仅 h>0 时算，只取上游报了缓存字段的样本）；偏离目标 CacheExpected
	// 超容差 → CacheMiss。上游压根不报缓存字段时为空（无从判断，不当未命中）
	CacheHitRate  *float64 `json:"cacheHitRate,omitempty"`
	CacheExpected float64  `json:"cacheExpected,omitempty"`
	CacheMiss     bool     `json:"cacheMiss,omitempty"`
	// ReasoningSeen 正常应答里出现推理的条数（首增量早于首正文、usage 报了推理 token，或输出上限被思考用完）
	ReasoningSeen int `json:"reasoningSeen,omitempty"`
	// Preflight 预检结论（仅 __overall__）：上游实际接受的请求形态、做过的兼容调整、关思考探测结果
	Preflight *Preflight `json:"preflight,omitempty"`
	// Calibration 定标结果（仅 __overall__）
	Calibration *Calibration `json:"calibration,omitempty"`

	ThroughputRps float64 `json:"throughputRps,omitempty"`
	TokensPerSec  float64 `json:"tokensPerSec,omitempty"`

	ByErrorClass map[string]int `json:"byErrorClass,omitempty"`

	// Concurrency 阶梯并发档的并发数（其它 probe 留 0，omitempty）
	Concurrency int `json:"concurrency,omitempty"`

	// —— RPM/TPM 开环速率发现（阶梯并发 probe 留空）——
	TargetRate   float64 `json:"targetRate,omitempty"`   // 档级：目标到达率 req/s
	AchievedRate float64 `json:"achievedRate,omitempty"` // 档级：实际达成到达率 req/s（在途封顶会低于目标）
	RateLimited  bool    `json:"rateLimited,omitempty"`  // 档级：本档 429 占比是否判定为触发限速
	ConvergedRpm float64 `json:"convergedRpm,omitempty"` // __overall__：收敛的可持续 RPM 边界（req/min）
	ReachedCap   bool    `json:"reachedCap,omitempty"`   // __overall__：探到速率护栏顶仍未限速（真实边界≥护栏）
	// Truncated __overall__：搜索被全局硬闸截断。未出现过限速档时收敛值只是下界（真实边界≥它），
	// 出现过则二分未完成、精度不足；TruncatedBy 为先触发的那道闸（requests|tokens|duration）
	Truncated   bool   `json:"truncated,omitempty"`
	TruncatedBy string `json:"truncatedBy,omitempty"`

	// —— TPM 开环 token 速率发现（仅 tpm_probe）——
	TargetTokenRate   float64 `json:"targetTokenRate,omitempty"`   // 档级：目标 token 到达率 token/s
	AchievedTokenRate float64 `json:"achievedTokenRate,omitempty"` // 档级：实测 token 吞吐 token/s（输入+输出）
	ConvergedTpm      float64 `json:"convergedTpm,omitempty"`      // __overall__：收敛的可持续 TPM 边界（token/min）

	// RateLimitHeaders 最近一次响应携带的限速头快照（x-ratelimit-*/anthropic-ratelimit-*/retry-after）
	RateLimitHeaders map[string]string `json:"rateLimitHeaders,omitempty"`
}

// Deviation 目标 vs 实测 token 的偏差分布（百分比，正 = 实测多于目标）
type Deviation struct {
	Samples  int     `json:"samples"`
	P50      float64 `json:"p50"`
	Min      float64 `json:"min"`
	Max      float64 `json:"max"`
	AbsP95   float64 `json:"absP95"`   // |偏差| 的 p95
	Exceeded bool    `json:"exceeded"` // AbsP95 超过 ±10% 阈值
}

// StageMetrics 一档评估结果 + 定位信息，worker 据此落 stability_metrics。
type StageMetrics struct {
	Probe      string
	Stage      string
	StageIndex int
	Metrics    Metrics
}

// 全局硬闸触发原因（Metrics.TruncatedBy 取值）
const (
	CapRequests = "requests"
	CapTokens   = "tokens"
	CapDuration = "duration"
)

// CapGuard 全局成本硬闸：跨 probe 累计请求数、token 数并限定整任务墙钟时长，碰任一上限即令 probe 收敛停止。
// token 按「输入目标 + 输出目标」在派发前预扣、出结果后按实测补差：大输入时在途一批不会把总 token 冲穿。
// 尽力而为上界（检查与预扣之间无锁），非精确配额；墙钟到点只停派新请求，在途的跑完。
type CapGuard struct {
	maxReq   int64
	maxTok   int64
	deadline time.Time // 零值 = 不限时长
	reqs     int64     // atomic
	toks     int64     // atomic
	reason   atomic.Pointer[string]
}

// NewCapGuard 建硬闸，墙钟从此刻起算；上限 <=0 视为不限。
func NewCapGuard(maxReq, maxTok int, maxDur time.Duration) *CapGuard {
	big := int64(1) << 62
	mr, mt := int64(maxReq), int64(maxTok)
	if mr <= 0 {
		mr = big
	}
	if mt <= 0 {
		mt = big
	}
	c := &CapGuard{maxReq: mr, maxTok: mt}
	if maxDur > 0 {
		c.deadline = time.Now().Add(maxDur)
	}
	return c
}

// Reserve 占用一个请求配额并预扣 est 个名义 token；返回 false 表示已撞某道闸（时长/token/请求数），不应再发。
func (c *CapGuard) Reserve(est int64) bool {
	if c == nil {
		return true
	}
	switch {
	case !c.deadline.IsZero() && time.Now().After(c.deadline):
		return c.trip(CapDuration)
	case atomic.LoadInt64(&c.toks)+est > c.maxTok:
		return c.trip(CapTokens)
	case atomic.AddInt64(&c.reqs, 1) > c.maxReq:
		return c.trip(CapRequests)
	}
	atomic.AddInt64(&c.toks, est)
	return true
}

// trip 记下首个触发原因（后续触发不覆盖），恒返回 false 供 Reserve 直接 return
func (c *CapGuard) trip(reason string) bool {
	c.reason.CompareAndSwap(nil, &reason)
	return false
}

// Reason 首个触发的硬闸原因；未触发为空串
func (c *CapGuard) Reason() string {
	if c == nil {
		return ""
	}
	if r := c.reason.Load(); r != nil {
		return *r
	}
	return ""
}

// Settle 请求结束后按实测消耗补差（actual − 预扣的 est；actual=0 即全额退回）
func (c *CapGuard) Settle(est, actual int64) {
	if c == nil {
		return
	}
	atomic.AddInt64(&c.toks, actual-est)
}
