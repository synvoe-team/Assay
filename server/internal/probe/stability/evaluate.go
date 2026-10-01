package stability

import (
	"math"
	"sort"
	"time"
)

// percentile 线性插值分位数（对齐 numpy.percentile 默认 type-7，与 vLLM bench 口径一致）。
// sorted 须已升序；q ∈ [0,100]。结果四舍五入到整毫秒（亚毫秒精度对延迟无意义）。
func percentile(sorted []int, q float64) int {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	rank := q / 100 * float64(n-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return int(math.Round(float64(sorted[lo]) + frac*float64(sorted[hi]-sorted[lo])))
}

// summarize 把一组延迟观测（毫秒）聚合成分位数摘要；空则返回 nil（指标里 omitempty）。
func summarize(v []int) *Percentiles {
	if len(v) == 0 {
		return nil
	}
	s := append([]int(nil), v...)
	sort.Ints(s)
	var sum int
	for _, x := range s {
		sum += x
	}
	return &Percentiles{
		P50: percentile(s, 50),
		P95: percentile(s, 95),
		P99: percentile(s, 99),
		Min: s[0],
		Max: s[len(s)-1],
		Avg: math.Round(float64(sum)/float64(len(s))*100) / 100,
	}
}

// measured 剔除预热样本
func measured(samples []Sample) []Sample {
	out := samples[:0:0]
	for _, s := range samples {
		if !s.Warmup {
			out = append(out, s)
		}
	}
	return out
}

// aggregate 把一组（已剔除预热的）样本聚合成 Metrics（延迟分位数 + 错误分类 + 吞吐 + 负载画像核对）。
// 延迟分位只统计正常应答（served）的样本（失败无延迟）；h=0 时命中缓存是意外，其延迟不代表渠道真实 prefill，
// 剔出分位，h>0 时命中是负载画像的一部分，照常计入。
// 错误率 = 真错误 / 全部请求；输出上限用尽另计条数（渠道正常应答，只是测不到 TTFT）；吞吐按样本时间跨度确定性计算。
func aggregate(samples []Sample, h float64) Metrics {
	m := Metrics{Requests: len(samples), ByErrorClass: map[string]int{}}
	var ttfb, ttfd, ttft, total []int
	for _, s := range samples {
		if s.ErrorClass != "" {
			m.ByErrorClass[s.ErrorClass]++
		}
		if s.ErrorClass == ErrBudgetExhausted {
			m.BudgetExhausted++
		}
		if !served(s) {
			m.Errors++
			continue
		}
		if reasoned(s) {
			m.ReasoningSeen++
		}
		if s.CachedTokens > 0 {
			m.CacheHits++
			if h == 0 {
				continue
			}
		}
		ttfb = appendMeasured(ttfb, s.TTFBms)
		ttfd = appendMeasured(ttfd, s.TTFDms)
		ttft = appendMeasured(ttft, s.TTFTms) // 输出上限用尽的样本无正文，TTFT 本就缺测
		total = appendMeasured(total, s.TotalMs)
	}
	if m.Requests > 0 {
		m.ErrorRate = math.Round(float64(m.Errors)/float64(m.Requests)*10000) / 10000
	}
	if len(m.ByErrorClass) == 0 {
		m.ByErrorClass = nil // 无错误时不落空对象
	}
	m.TTFBms = summarize(ttfb)
	m.TTFDms = summarize(ttfd)
	m.TTFTms = summarize(ttft)
	m.TotalMs = summarize(total)
	m.ThroughputRps, m.TokensPerSec = throughput(samples)
	m.InputDeviation = deviation(samples, func(s Sample) (int, int) { return s.TargetInputTokens, s.InputTokens })
	m.OutputDeviation = deviation(samples, func(s Sample) (int, int) { return s.TargetOutputTokens, s.OutputTokens })
	if h > 0 {
		m.CacheExpected = h
		m.CacheHitRate = cacheHitRate(samples)
		m.CacheMiss = m.CacheHitRate != nil && math.Abs(*m.CacheHitRate-h) > cacheTolerance+1e-9
	}
	return m
}

// reasoned 正常应答里出现过推理：输出上限被思考用完、usage 报了推理 token（隐藏推理的模型连增量都不给），
// 或首增量早于首正文（纯空白增量不算首增量，见 protocol.tracker）
func reasoned(s Sample) bool {
	return s.ErrorClass == ErrBudgetExhausted || s.ReasoningTokens > 0 || (s.TTFDms >= 0 && s.TTFTms >= 0 && s.TTFDms < s.TTFTms)
}

// deviation 目标 vs 实测的偏差分布：只取正常应答、有目标且有 usage 的样本。
// 以万分点整数复用 percentile 的插值口径，再换回百分比（两位小数）。
func deviation(samples []Sample, pick func(Sample) (target, actual int)) *Deviation {
	var dev, abs []int
	for _, s := range samples {
		target, actual := pick(s)
		if !served(s) || target <= 0 || actual < 0 {
			continue
		}
		bp := int(math.Round(float64(actual-target) / float64(target) * 10000))
		dev = append(dev, bp)
		abs = append(abs, max(bp, -bp))
	}
	if len(dev) == 0 {
		return nil
	}
	sort.Ints(dev)
	sort.Ints(abs)
	pct := func(bp int) float64 { return float64(bp) / 100 }
	d := &Deviation{
		Samples: len(dev),
		P50:     pct(percentile(dev, 50)),
		Min:     pct(dev[0]),
		Max:     pct(dev[len(dev)-1]),
		AbsP95:  pct(percentile(abs, 95)),
	}
	d.Exceeded = d.AbsP95 > deviationLimitPct
	return d
}

// cacheHitRate 实测命中率 = Σcached / Σinput（正常应答且有 usage 的样本）；无可算样本返回 nil
func cacheHitRate(samples []Sample) *float64 {
	var cached, input int64
	for _, s := range samples {
		if !served(s) || s.InputTokens <= 0 || s.CachedTokens < 0 {
			continue
		}
		cached += int64(s.CachedTokens)
		input += int64(s.InputTokens)
	}
	if input == 0 {
		return nil
	}
	r := math.Round(float64(cached)/float64(input)*10000) / 10000
	return &r
}

// appendMeasured 只收测到的值（<0 = 没测到）
func appendMeasured(v []int, ms int) []int {
	if ms < 0 {
		return v
	}
	return append(v, ms)
}

// throughput 按正常应答样本的时间跨度算吞吐：请求/秒 与 生成 token/秒。
// 跨度 = 最早排定到最晚完成（dispatched_at + total_ms）。
func throughput(samples []Sample) (rps float64, tokPerSec float64) {
	var minD, maxC time.Time
	ok := 0
	var outTokens int64
	for _, s := range samples {
		if !served(s) || s.TotalMs < 0 {
			continue
		}
		d := s.DispatchedAt
		c := s.DispatchedAt.Add(time.Duration(s.TotalMs) * time.Millisecond)
		if ok == 0 {
			minD, maxC = d, c
		} else {
			if d.Before(minD) {
				minD = d
			}
			if c.After(maxC) {
				maxC = c
			}
		}
		ok++
		if s.OutputTokens > 0 {
			outTokens += int64(s.OutputTokens)
		}
	}
	if ok == 0 {
		return 0, 0
	}
	wall := maxC.Sub(minD).Seconds()
	if wall <= 0 {
		return 0, 0
	}
	rps = math.Round(float64(ok)/wall*100) / 100
	tokPerSec = math.Round(float64(outTokens)/wall*100) / 100
	return rps, tokPerSec
}

// evaluateStage 计算某并发档的档级指标（含并发数标注）。
func evaluateStage(probeID, stage string, stageIndex, concurrency int, h float64, samples []Sample) StageMetrics {
	m := aggregate(measured(samples), h)
	m.Concurrency = concurrency
	return StageMetrics{Probe: probeID, Stage: stage, StageIndex: stageIndex, Metrics: m}
}

// evaluateOverall 汇总全部真实档的样本为 __overall__ 行（不标并发、不算跨档吞吐）。
func evaluateOverall(probeID string, h float64, samples []Sample) StageMetrics {
	m := aggregate(samples, h) // 传入的已是各档剔除预热后的样本
	m.ThroughputRps = 0        // 跨档混合吞吐无意义
	m.TokensPerSec = 0
	return StageMetrics{Probe: probeID, Stage: StageOverall, StageIndex: StageOverallIndex, Metrics: m}
}

// evaluatePacedStage 开环速率档评估：聚合判定段样本（剔除档前段热身）+ 标注目标/达成到达率、限速判定与限速头。
func evaluatePacedStage(probeID, stage string, stageIndex int, h, targetRate, achievedRate float64, rateLimited bool, rateHeaders map[string]string, samples []Sample) StageMetrics {
	m := aggregate(measured(samples), h)
	m.TargetRate = math.Round(targetRate*100) / 100
	m.AchievedRate = achievedRate
	m.RateLimited = rateLimited
	m.RateLimitHeaders = rateHeaders
	return StageMetrics{Probe: probeID, Stage: stage, StageIndex: stageIndex, Metrics: m}
}

// evaluateRpmOverall RPM probe 的 __overall__ 行：收敛的可持续 RPM 边界 + 是否触顶护栏/被截断 + 限速头快照。
func evaluateRpmOverall(probeID string, h float64, samples []Sample, b boundary, maxRate float64, truncatedBy string, rateHeaders map[string]string) StageMetrics {
	m := evaluateBoundaryOverall(h, samples, b, maxRate, truncatedBy, rateHeaders)
	m.ConvergedRpm = math.Round(b.lo*60*100) / 100
	return StageMetrics{Probe: probeID, Stage: StageOverall, StageIndex: StageOverallIndex, Metrics: m}
}

// evaluateBoundaryOverall RPM/TPM 共用的 overall 口径：跨档混合吞吐无意义置零；
// 触顶 = 一路升到护栏顶都没限速且没被截断（被截断时 lo 没跑到顶，不能说「≥ 护栏」）。
func evaluateBoundaryOverall(h float64, samples []Sample, b boundary, maxRate float64, truncatedBy string, rateHeaders map[string]string) Metrics {
	m := aggregate(samples, h)
	m.ThroughputRps = 0
	m.TokensPerSec = 0
	m.ReachedCap = !b.haveHi && !b.truncated && b.lo >= maxRate-1e-9
	m.Truncated = b.truncated
	if b.truncated {
		m.TruncatedBy = truncatedBy
	}
	m.RateLimitHeaders = rateHeaders
	return m
}

// evaluateTpmStage 开环 token 速率档评估：在开环请求档基础上叠加目标/实测 token 到达率标注。
// achievedTokenRate 为实测 token 吞吐（输入+输出），已由调用方按发压窗口算好。
func evaluateTpmStage(probeID, stage string, stageIndex int, h, targetRate, achievedRate, targetTokenRate, achievedTokenRate float64, rateLimited bool, rateHeaders map[string]string, samples []Sample) StageMetrics {
	sm := evaluatePacedStage(probeID, stage, stageIndex, h, targetRate, achievedRate, rateLimited, rateHeaders, samples)
	sm.Metrics.TargetTokenRate = math.Round(targetTokenRate*100) / 100
	sm.Metrics.AchievedTokenRate = math.Round(achievedTokenRate*100) / 100
	return sm
}

// evaluateTpmOverall TPM probe 的 __overall__ 行：收敛的可持续 TPM 边界（token/min）+ 是否触顶护栏/被截断 + 限速头。
func evaluateTpmOverall(probeID string, h float64, samples []Sample, b boundary, maxRate float64, truncatedBy string, rateHeaders map[string]string) StageMetrics {
	m := evaluateBoundaryOverall(h, samples, b, maxRate, truncatedBy, rateHeaders)
	m.ConvergedTpm = math.Round(b.lo*60*100) / 100
	return StageMetrics{Probe: probeID, Stage: StageOverall, StageIndex: StageOverallIndex, Metrics: m}
}
