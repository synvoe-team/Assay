package stability

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Yukiho0287/assay/server/internal/probe"
	"github.com/Yukiho0287/assay/server/internal/probe/protocol"
)

// tokenBucketServer 令牌桶限速上游：refill=limit tokens/s、桶容量=limit。到达率超过 limit
// 时按 (R-limit)/R 比例返回 429 —— 给 RPM 二分一个清晰的真实速率边界（≈limit req/s）。
func tokenBucketServer(t *testing.T, limit float64) *httptest.Server {
	t.Helper()
	var (
		mu     sync.Mutex
		tokens = limit
		last   = time.Now()
	)
	allow := func() bool {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		tokens += now.Sub(last).Seconds() * limit
		if tokens > limit {
			tokens = limit
		}
		last = now
		if tokens >= 1 {
			tokens--
			return true
		}
		return false
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allow() {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("X-RateLimit-Remaining-Requests", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// rpmInput 组装跑 rpm_probe 的 RunInput，并收集各档 StageMetrics。
func rpmInput(t *testing.T, baseURL string, tune func(*StabilityParams)) (RunInput, *[]StageMetrics) {
	t.Helper()
	codec, _ := protocol.Get(protocol.ProtocolOpenAIChat)
	var params StabilityParams
	params.Protocol = protocol.ProtocolOpenAIChat
	params.ApplyDefaults()
	if tune != nil {
		tune(&params)
	}
	metrics := &[]StageMetrics{}
	var mu sync.Mutex
	in := RunInput{
		Probe:  rpmProbeID,
		Target: probe.Target{BaseURL: baseURL, Model: "m"},
		Params: params,
		Client: &http.Client{},
		Codec:  codec,
		Caps:   NewCapGuard(params.MaxTotalRequests, params.MaxTotalTokens, 0),
		Metric: func(_ context.Context, m StageMetrics) error {
			mu.Lock()
			*metrics = append(*metrics, m)
			mu.Unlock()
			return nil
		},
	}
	return in, metrics
}

func overallOf(t *testing.T, metrics []StageMetrics) StageMetrics {
	t.Helper()
	for _, m := range metrics {
		if m.Stage == StageOverall {
			return m
		}
	}
	t.Fatalf("未产出 __overall__ 指标")
	return StageMetrics{}
}

// 二分收敛：令牌桶 limit=15/s，断言 RPM 收敛到真实边界附近，且触发过限速（非触顶护栏）。
func TestRpm_ConvergesToBoundary(t *testing.T) {
	srv := tokenBucketServer(t, 15)
	in, metrics := rpmInput(t, srv.URL, func(p *StabilityParams) {
		p.RpmStartRate = 5
		p.RpmMaxRate = 40
		p.RpmStageSec = 1
		p.RpmBinarySteps = 4
		p.RpmMaxInFlight = 256
	})

	if err := runRpm(context.Background(), in); err != nil {
		t.Fatalf("runRpm error: %v", err)
	}
	overall := overallOf(t, *metrics)
	if overall.Metrics.ReachedCap {
		t.Fatalf("limit=15 远低于护栏 40，不应 reachedCap")
	}
	rpm := overall.Metrics.ConvergedRpm
	// 真实边界 ~15 req/s（900 RPM）；令牌桶抖动 + 阈值偏移给宽容区间
	if rpm < 300 || rpm > 1800 {
		t.Fatalf("收敛 RPM=%.0f 偏离真实边界 ~900（期望 300-1800）", rpm)
	}

	// 最低速率档不应限速；护栏顶速率档应限速
	var sawLowPass, sawHighLimited bool
	for _, m := range *metrics {
		if m.Stage == StageOverall {
			continue
		}
		if m.Metrics.TargetRate == 5 && !m.Metrics.RateLimited {
			sawLowPass = true
		}
		if m.Metrics.TargetRate == 40 && m.Metrics.RateLimited {
			sawHighLimited = true
		}
	}
	if !sawLowPass {
		t.Fatalf("5 req/s 档不应被判限速")
	}
	if !sawHighLimited {
		t.Fatalf("40 req/s 档应被判限速")
	}
}

// 触顶护栏：上游不限速，ramp 升到护栏顶仍无 429 → reachedCap=true，收敛 RPM=护栏×60。
func TestRpm_ReachedCap(t *testing.T) {
	srv, _ := sseChatServer(t, 0, nil) // 永不 429
	in, metrics := rpmInput(t, srv.URL, func(p *StabilityParams) {
		p.RpmStartRate = 4
		p.RpmMaxRate = 16
		p.RpmStageSec = 1
		p.RpmBinarySteps = 4
		p.RpmMaxInFlight = 256
	})

	if err := runRpm(context.Background(), in); err != nil {
		t.Fatalf("runRpm error: %v", err)
	}
	overall := overallOf(t, *metrics)
	if !overall.Metrics.ReachedCap {
		t.Fatalf("上游不限速应 reachedCap=true")
	}
	if overall.Metrics.ConvergedRpm != 16*60 {
		t.Fatalf("触顶护栏收敛 RPM 应为 %d，实际 %.0f", 16*60, overall.Metrics.ConvergedRpm)
	}
	// 触顶时不应进入二分（无 hi）：档数 = ramp 档数（4,8,16）+ overall
	nonOverall := 0
	for _, m := range *metrics {
		if m.Stage != StageOverall {
			nonOverall++
		}
	}
	if nonOverall != 3 {
		t.Fatalf("触顶应只有 3 个 ramp 档（4/8/16），实际 %d", nonOverall)
	}
}

func TestRampRates(t *testing.T) {
	got := rampRates(2, 20)
	want := []float64{2, 4, 8, 16, 20}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rampRates(2,20)=%v，期望 %v", got, want)
	}
	if r := rampRates(5, 5); !reflect.DeepEqual(r, []float64{5}) {
		t.Fatalf("start==max 应单档，得 %v", r)
	}
}

func TestIsRateLimited(t *testing.T) {
	s := []Sample{{ErrorClass: ErrRateLimited}, {Ok: true}, {Ok: true}, {Ok: true}, {Ok: true}}
	if isRateLimited(s, 0.1) != true { // 1/5 = 0.2 ≥ 0.1
		t.Fatalf("1/5 429 应判限速")
	}
	if isRateLimited(s, 0.5) != false { // 0.2 < 0.5
		t.Fatalf("0.2 < 0.5 不应判限速")
	}
	if isRateLimited(nil, 0.1) != false {
		t.Fatalf("空样本不应判限速")
	}
}

// 修复的 bug：升压途中撞请求硬闸、从没见过 429 → 必须标截断，收敛值只是下界，且不能算「触顶护栏」。
// 修复前这里报 convergedRpm=240、truncated 无从表达，前端会当成真实边界展示。
func TestRpm_TruncatedByCaps(t *testing.T) {
	srv, _ := sseChatServer(t, 0, nil) // 永不 429
	in, metrics := rpmInput(t, srv.URL, func(p *StabilityParams) {
		p.RpmStartRate = 4
		p.RpmMaxRate = 16
		p.RpmStageSec = 1
		p.RpmMaxInFlight = 256
		p.MaxTotalRequests = 7 // r4 档约 5 条跑完，r8 档中途撞闸
	})
	in.Caps = NewCapGuard(in.Params.MaxTotalRequests, 0, 0)

	if err := runRpm(context.Background(), in); err != nil {
		t.Fatalf("runRpm error: %v", err)
	}
	m := overallOf(t, *metrics).Metrics
	if !m.Truncated || m.TruncatedBy != CapRequests {
		t.Fatalf("truncated=%v by=%q，期望 true/%q", m.Truncated, m.TruncatedBy, CapRequests)
	}
	if m.ReachedCap {
		t.Fatalf("被截断不能算触顶护栏")
	}
	if m.ConvergedRpm != 4*60 {
		t.Fatalf("下界应为完整跑完的 r4 档（240 RPM），得 %.0f", m.ConvergedRpm)
	}
}

// 只看每档后一半：429 全落在前一半热身期（渠道消化上一档残留/突发的阶段）时，不判限速。
// 修复前整档一起算，8/41≈20% ≥ 阈值 10% 会被误判限速。
func TestRpm_JudgesOnlySecondHalf(t *testing.T) {
	srv, _ := sseChatServer(t, 0, func(n int64) bool { return n <= 8 }) // 只拒最早 8 条（40/s 下约前 0.2s）
	in, metrics := rpmInput(t, srv.URL, func(p *StabilityParams) {
		p.RpmStartRate = 40
		p.RpmMaxRate = 40
		p.RpmStageSec = 1 // 前 0.5s 热身
		p.RpmMaxInFlight = 256
	})

	if err := runRpm(context.Background(), in); err != nil {
		t.Fatalf("runRpm error: %v", err)
	}
	for _, m := range *metrics {
		if m.Stage != StageOverall && m.Metrics.RateLimited {
			t.Fatalf("429 全在热身期，档 %s 不应判限速", m.Stage)
		}
		if m.Stage != StageOverall && m.Metrics.ByErrorClass[ErrRateLimited] != 0 {
			t.Errorf("热身期的 429 不应计入档级指标，得 %v", m.Metrics.ByErrorClass)
		}
	}
	if !overallOf(t, *metrics).Metrics.ReachedCap {
		t.Errorf("唯一一档未限速，应触顶护栏")
	}
}
