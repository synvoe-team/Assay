package stability

import (
	"testing"
	"time"
)

// TestPercentile 线性插值分位数确定性（对齐 numpy type-7）
func TestPercentile(t *testing.T) {
	sorted := []int{10, 20, 30, 40, 50}
	cases := []struct {
		q    float64
		want int
	}{
		{50, 30}, // rank 2.0 → 30
		{95, 48}, // rank 3.8 → 40 + 0.8*10
		{99, 50}, // rank 3.96 → 49.6 → round 50
		{0, 10},
		{100, 50},
	}
	for _, c := range cases {
		if got := percentile(sorted, c.q); got != c.want {
			t.Errorf("percentile(%.0f) = %d，期望 %d", c.q, got, c.want)
		}
	}
	if got := percentile([]int{42}, 95); got != 42 {
		t.Errorf("单元素 p95 = %d，期望 42", got)
	}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("空切片 p50 = %d，期望 0", got)
	}
}

// TestAggregate 档级聚合：错误率/分类/分位数/吞吐确定性
func TestAggregate(t *testing.T) {
	base := time.Unix(1000, 0)
	samples := []Sample{
		{Ok: true, DispatchedAt: base, TTFBms: 5, TTFTms: 10, TotalMs: 100, OutputTokens: 5},
		{Ok: true, DispatchedAt: base, TTFBms: 6, TTFTms: 20, TotalMs: 100, OutputTokens: 5},
		{Ok: true, DispatchedAt: base, TTFBms: 7, TTFTms: 30, TotalMs: 100, OutputTokens: 5},
		{Ok: false, DispatchedAt: base, ErrorClass: ErrRateLimited, TTFBms: -1, TTFTms: -1, TotalMs: -1},
	}
	m := aggregate(samples)
	if m.Requests != 4 || m.Errors != 1 {
		t.Fatalf("requests/errors = %d/%d，期望 4/1", m.Requests, m.Errors)
	}
	if m.ErrorRate != 0.25 {
		t.Errorf("errorRate = %v，期望 0.25", m.ErrorRate)
	}
	if m.ByErrorClass[ErrRateLimited] != 1 {
		t.Errorf("byErrorClass = %v", m.ByErrorClass)
	}
	if m.TTFTms == nil || m.TTFTms.P50 != 20 || m.TTFTms.Min != 10 || m.TTFTms.Max != 30 {
		t.Errorf("ttft 分位数 = %+v", m.TTFTms)
	}
	// 3 个成功样本、同起点、total 100ms → wall=0.1s，rps=30、tokens 15/0.1=150
	if m.ThroughputRps != 30 {
		t.Errorf("throughputRps = %v，期望 30", m.ThroughputRps)
	}
	if m.TokensPerSec != 150 {
		t.Errorf("tokensPerSec = %v，期望 150", m.TokensPerSec)
	}
}

// TestMeasuredExcludesWarmup 预热样本被剔除
func TestMeasuredExcludesWarmup(t *testing.T) {
	samples := []Sample{
		{Seq: 0, Warmup: true, Ok: true},
		{Seq: 1, Warmup: false, Ok: true},
		{Seq: 2, Warmup: false, Ok: true},
	}
	got := measured(samples)
	if len(got) != 2 {
		t.Fatalf("measured 剩 %d 条，期望 2", len(got))
	}
	if len(samples) != 3 {
		t.Errorf("measured 不应改动入参，现 len=%d", len(samples))
	}
}

// TestAggregateNoErrors 无错误时 byErrorClass 不落空对象
func TestAggregateNoErrors(t *testing.T) {
	base := time.Unix(1000, 0)
	m := aggregate([]Sample{{Ok: true, DispatchedAt: base, TTFTms: 5, TotalMs: 50, TTFBms: 1}})
	if m.ByErrorClass != nil {
		t.Errorf("无错误时 byErrorClass 应为 nil，得 %v", m.ByErrorClass)
	}
	if m.ErrorRate != 0 {
		t.Errorf("errorRate = %v，期望 0", m.ErrorRate)
	}
}

// TestAggregateBudgetAndCache 预算耗尽不计入错误率（砝码不足非渠道故障）、单独计数；
// 缓存命中的成功样本不进延迟分位、单独计数；TTFD 与 TTFT 各自分位
func TestAggregateBudgetAndCache(t *testing.T) {
	base := time.Unix(1000, 0)
	samples := []Sample{
		{Ok: true, DispatchedAt: base, TTFBms: 5, TTFDms: 8, TTFTms: 10, TotalMs: 100, CachedTokens: 0},
		{Ok: true, DispatchedAt: base, TTFBms: 6, TTFDms: 9, TTFTms: 20, TotalMs: 100, CachedTokens: -1},
		// 命中缓存：不进延迟分位（TTFT=1 若被计入，min 会变成 1）
		{Ok: true, DispatchedAt: base, TTFBms: 1, TTFDms: 1, TTFTms: 1, TotalMs: 2, CachedTokens: 7},
		{Ok: false, DispatchedAt: base, ErrorClass: ErrBudgetExhausted, TTFDms: 30, TTFTms: -1, TotalMs: 900},
		{Ok: false, DispatchedAt: base, ErrorClass: ErrBudgetExhausted, TTFDms: 31, TTFTms: -1, TotalMs: 901},
		{Ok: false, DispatchedAt: base, ErrorClass: ErrRateLimited, TTFBms: -1, TTFTms: -1, TotalMs: -1},
	}
	m := aggregate(samples)
	if m.Requests != 6 || m.Errors != 1 || m.BudgetExhausted != 2 || m.CacheHits != 1 {
		t.Fatalf("requests/errors/budget/cache = %d/%d/%d/%d，期望 6/1/2/1", m.Requests, m.Errors, m.BudgetExhausted, m.CacheHits)
	}
	// 错误率分母剔除预算耗尽：1 / (6-2) = 0.25
	if m.ErrorRate != 0.25 {
		t.Errorf("errorRate = %v，期望 0.25", m.ErrorRate)
	}
	if m.ByErrorClass[ErrBudgetExhausted] != 2 {
		t.Errorf("byErrorClass 仍应列出预算耗尽，得 %v", m.ByErrorClass)
	}
	if m.TTFTms == nil || m.TTFTms.Min != 10 || m.TTFTms.Max != 20 {
		t.Errorf("ttft 分位应只含 2 个未命中缓存的成功样本，得 %+v", m.TTFTms)
	}
	if m.TTFDms == nil || m.TTFDms.Min != 8 || m.TTFDms.Max != 9 {
		t.Errorf("ttfd 分位 = %+v", m.TTFDms)
	}
}

// TestAggregateAllBudgetExhausted 全部预算耗尽：分母为 0 时错误率记 0，不除零
func TestAggregateAllBudgetExhausted(t *testing.T) {
	m := aggregate([]Sample{{ErrorClass: ErrBudgetExhausted}, {ErrorClass: ErrBudgetExhausted}})
	if m.ErrorRate != 0 || m.Errors != 0 || m.BudgetExhausted != 2 {
		t.Errorf("errorRate/errors/budget = %v/%d/%d", m.ErrorRate, m.Errors, m.BudgetExhausted)
	}
}
