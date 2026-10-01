package stability

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// pacedStageConfig 一档开环发压的配置。
type pacedStageConfig struct {
	TargetRate float64       // 目标到达率 req/s
	MaxTokens  int           // 每请求生成上限
	Prompt     string        // 压测 prompt
	Duration   time.Duration // 发压时长（排定新请求的窗口，尾部请求可越窗跑完）
	// Warmup 档前段热身时长：排定时刻落在其内的样本标 warmup，不参与限速判定与指标。
	// 渠道限流器有记忆（上一档没清零的计数、令牌桶攒下的突发额度），前段的表现不代表稳态。
	Warmup      time.Duration
	MaxInFlight int // 在途请求上限（防雪崩兜底）
}

// pacedStageResult 一档开环发压的产出。
type pacedStageResult struct {
	Samples     []Sample          // 本档全部样本（按 seq 升序）
	Dispatched  int               // 实际派发的请求数
	Stopped     bool              // 是否因全局硬闸提前收敛
	RateHeaders map[string]string // 最近一次响应携带的限速头快照
	DurationSec float64           // 实际发压窗口时长（算 achievedRate）
	MeasuredSec float64           // 发压窗口扣掉热身后的时长（算判定段的 token 吞吐）
}

// runPacedStage 以恒定到达率 cfg.TargetRate 开环发压 cfg.Duration 时长。
//
// 协调遗漏（coordinated omission）正确性的关键：ticker 按 1/rate 间隔排定发起时刻，
// 到点即起 goroutine 发请求，**不等前一个完成**；dispatched_at 记「排定时刻」而非
// goroutine 实际起飞时刻——系统卡顿时排定时刻照常推进，不会把慢尾伪装成低延迟。
//
// 已派发的请求用外层 ctx（非发压窗口 deadline）：窗口只控「何时停止派发新请求」，
// 已发出的请求要跑完（含慢尾），丢尾等于又一种协调遗漏。
//
// 在途封顶：在途已满则跳过该 tick（背压，不消耗预算、不阻塞排期，达成率自然下降
// 反映瓶颈）。全局硬闸（in.Caps）碰请求/token 上限即停派发，本档收敛。
func runPacedStage(ctx context.Context, in RunInput, stageIndex int, stage string, cfg pacedStageConfig, afterEach func()) (pacedStageResult, error) {
	rate := cfg.TargetRate
	if rate <= 0 {
		rate = 1
	}
	interval := time.Duration(float64(time.Second) / rate)
	if interval <= 0 {
		interval = time.Millisecond
	}
	maxInFlight := cfg.MaxInFlight
	if maxInFlight <= 0 {
		maxInFlight = 1
	}
	dur := cfg.Duration
	if dur <= 0 {
		dur = time.Second
	}

	inflight := make(chan struct{}, maxInFlight)
	var wg sync.WaitGroup
	var mu sync.Mutex // 护 samples / firstErr / lastHeaders（跨 goroutine）
	samples := make([]Sample, 0)
	var firstErr error
	var lastHeaders map[string]string

	// 以下计数仅在派发主 goroutine 内读写，无需加锁
	seq := 0
	dispatched := 0
	stopped := false

	stageStart := time.Now()

	// launch 排定一次发起：先抢在途槽（满则跳过、不耗预算），再过全局硬闸，最后起 goroutine。
	// scheduledAt 为「排定时刻」，写入 dispatched_at。返回 false = 撞硬闸，应停止本档。
	launch := func(scheduledAt time.Time) bool {
		select {
		case inflight <- struct{}{}:
		default:
			return true // 在途满：跳过本 tick，继续排期
		}
		if !in.Caps.Reserve() {
			<-inflight // 释放刚占的在途槽
			stopped = true
			return false
		}
		sq := seq
		seq++
		dispatched++
		wg.Add(1)
		go func(sched time.Time, sqN int) {
			defer wg.Done()
			defer func() { <-inflight }()
			o := doRequest(ctx, in.Client, in.Codec, in.Target.BaseURL, in.APIKey,
				in.Target.Model, uniquePrompt(in.Nonce, stage, sqN, cfg.Prompt), cfg.MaxTokens, in.Params.RequestTimeoutMs)
			if ctx.Err() != nil {
				return // 取消/关停：不落污染样本
			}
			if in.Caps != nil && o.Usage.Ok {
				in.Caps.AddTokens(o.Usage.Prompt + o.Usage.Completion)
			}
			warmup := sched.Sub(stageStart) < cfg.Warmup
			s := sampleFrom(stage, stageIndex, sqN, warmup, sched, in.Codec.ID(), o)
			hdrs := extractRateLimitHeaders(o.Header)

			mu.Lock()
			samples = append(samples, s)
			if hdrs != nil {
				lastHeaders = hdrs
			}
			var serr error
			if in.Sample != nil {
				serr = in.Sample(ctx, s)
			}
			if serr != nil && firstErr == nil {
				firstErr = serr
			}
			mu.Unlock()

			if serr == nil && afterEach != nil {
				afterEach()
			}
		}(scheduledAt, sq)
		return true
	}

	deadline := time.NewTimer(dur)
	defer deadline.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// 立即发第一个（tick 0 = stageStart），dispatched_at 从零点起、不丢首请求
	launch(stageStart)
	tickN := 0

loop:
	for !stopped {
		select {
		case <-ctx.Done():
			break loop
		case <-deadline.C:
			break loop
		case <-ticker.C:
			tickN++
			// 排定时刻按 tick 计数等距推进（跳过的 tick 也占位），保证间距恒为 interval
			launch(stageStart.Add(time.Duration(tickN) * interval))
		}
	}
	dispatchElapsed := time.Since(stageStart)

	wg.Wait()
	sort.Slice(samples, func(i, j int) bool { return samples[i].Seq < samples[j].Seq })
	return pacedStageResult{
		Samples:     samples,
		Dispatched:  dispatched,
		Stopped:     stopped,
		RateHeaders: lastHeaders,
		DurationSec: dispatchElapsed.Seconds(),
		MeasuredSec: max(0, (dispatchElapsed - cfg.Warmup).Seconds()),
	}, firstErr
}

// extractRateLimitHeaders 从响应头挑出限速相关字段（openai x-ratelimit-*、
// anthropic anthropic-ratelimit-*、通用 retry-after），键统一小写。无则返回 nil。
func extractRateLimitHeaders(h http.Header) map[string]string {
	if h == nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range h {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "ratelimit") || strings.Contains(lk, "rate-limit") || lk == "retry-after" {
			if len(v) > 0 {
				out[lk] = v[0]
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
