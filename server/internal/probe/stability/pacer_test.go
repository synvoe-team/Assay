package stability

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Yukiho0287/assay/server/internal/probe"
	"github.com/Yukiho0287/assay/server/internal/probe/protocol"
)

// sseChatServer 返回一个最小 openai_chat SSE 上游：可选每请求延迟、可选前 N 个请求返回 429。
// hits 记录累计收到的请求数（原子）。
func sseChatServer(t *testing.T, delay time.Duration, reject func(n int64) bool) (*httptest.Server, *int64) {
	t.Helper()
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		if reject != nil && reject(n) {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("X-RateLimit-Remaining-Requests", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		fl, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-RateLimit-Remaining-Requests", "99")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		if fl != nil {
			fl.Flush()
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// pacerInput 组装一个跑 pacer 的最小 RunInput（openai_chat codec）。
func pacerInput(baseURL string, caps *CapGuard, sample func(Sample)) RunInput {
	codec, _ := protocol.Get(protocol.ProtocolOpenAIChat)
	var params StabilityParams
	params.ApplyDefaults()
	params.Protocol = protocol.ProtocolOpenAIChat
	in := RunInput{
		Probe:  "test",
		Target: probe.Target{BaseURL: baseURL, Model: "m"},
		Params: params,
		Client: &http.Client{},
		Codec:  codec,
		Caps:   caps,
	}
	if sample != nil {
		in.Sample = func(_ context.Context, s Sample) error { sample(s); return nil }
	}
	return in
}

// 达成到达率：快上游下，派发数应接近 rate×duration（含 tick0）。
func TestPacer_ArrivalRate(t *testing.T) {
	srv, _ := sseChatServer(t, 0, nil)
	in := pacerInput(srv.URL, nil, nil)
	cfg := pacedStageConfig{TargetRate: 100, MaxTokens: 16, Prompt: "hi", Duration: 300 * time.Millisecond, MaxInFlight: 256}

	res, err := runPacedStage(context.Background(), in, 0, "r100", cfg, nil)
	if err != nil {
		t.Fatalf("runPacedStage error: %v", err)
	}
	// 理论 ~30（100/s × 0.3s）+ tick0；ticker 抖动给宽容区间
	if res.Dispatched < 18 || res.Dispatched > 45 {
		t.Fatalf("dispatched=%d 偏离目标到达率（期望 ~30）", res.Dispatched)
	}
	if len(res.Samples) != res.Dispatched {
		t.Fatalf("samples=%d != dispatched=%d", len(res.Samples), res.Dispatched)
	}
	if res.RateHeaders["x-ratelimit-remaining-requests"] != "99" {
		t.Fatalf("限速头未采集到: %+v", res.RateHeaders)
	}
}

// 全局硬闸：请求数上限 5，派发到 5 即停、stopped=true。
func TestPacer_HardGateRequests(t *testing.T) {
	srv, _ := sseChatServer(t, 0, nil)
	caps := NewCapGuard(5, 0, 0)
	in := pacerInput(srv.URL, caps, nil)
	cfg := pacedStageConfig{TargetRate: 200, MaxTokens: 16, Prompt: "hi", Duration: 2 * time.Second, MaxInFlight: 256}

	res, err := runPacedStage(context.Background(), in, 0, "r200", cfg, nil)
	if err != nil {
		t.Fatalf("runPacedStage error: %v", err)
	}
	if res.Dispatched != 5 {
		t.Fatalf("硬闸应恰好派发 5 个，实际 %d", res.Dispatched)
	}
	if !res.Stopped {
		t.Fatalf("撞请求上限应 stopped=true")
	}
}

// 协调遗漏正确性：dispatched_at 记「排定时刻」，即使上游慢，相邻排定时刻间距恒为 interval。
func TestPacer_CoordinatedOmission(t *testing.T) {
	// 上游每请求慢 50ms，但排定间距应仍为 10ms（1/100）——证明 dispatched_at 用排定时刻而非起飞时刻
	srv, _ := sseChatServer(t, 50*time.Millisecond, nil)
	in := pacerInput(srv.URL, nil, nil)
	cfg := pacedStageConfig{TargetRate: 100, MaxTokens: 16, Prompt: "hi", Duration: 200 * time.Millisecond, MaxInFlight: 512}

	res, err := runPacedStage(context.Background(), in, 0, "r100", cfg, nil)
	if err != nil {
		t.Fatalf("runPacedStage error: %v", err)
	}
	if len(res.Samples) < 3 {
		t.Fatalf("样本过少，无法验证排定间距: %d", len(res.Samples))
	}
	interval := 10 * time.Millisecond
	// 相邻样本排定时刻差应是 interval 的整数倍（跳过的 tick 会造成整数倍间隔），且不受 50ms 慢响应影响
	for i := 1; i < len(res.Samples); i++ {
		gap := res.Samples[i].DispatchedAt.Sub(res.Samples[i-1].DispatchedAt)
		if gap < interval-2*time.Millisecond {
			t.Fatalf("排定间距 %v < interval %v：dispatched_at 似乎用了起飞时刻而非排定时刻", gap, interval)
		}
		// 若用起飞时刻，慢响应会把间距压到接近 0 或推到 50ms+ 抖动；排定时刻应是 interval 的近整数倍
		mult := float64(gap) / float64(interval)
		if mult-float64(int(mult+0.5)) > 0.25 && float64(int(mult+0.5))-mult > 0.25 {
			// 容忍调度抖动：只要接近某个整数倍即可
			if gap > 60*time.Millisecond {
				t.Fatalf("排定间距 %v 不是 interval 的整数倍（疑似协调遗漏未修正）", gap)
			}
		}
	}
}

// 在途封顶：慢上游 + 低在途上限，派发被背压，不会雪崩到目标速率全量。
func TestPacer_InFlightCap(t *testing.T) {
	srv, _ := sseChatServer(t, 100*time.Millisecond, nil)
	in := pacerInput(srv.URL, nil, nil)
	// 目标 500/s，但在途封顶 4 + 每请求 100ms → 稳态最多 ~4 在途，派发被背压远低于 500×0.3=150
	cfg := pacedStageConfig{TargetRate: 500, MaxTokens: 16, Prompt: "hi", Duration: 300 * time.Millisecond, MaxInFlight: 4}

	res, err := runPacedStage(context.Background(), in, 0, "r500", cfg, nil)
	if err != nil {
		t.Fatalf("runPacedStage error: %v", err)
	}
	if res.Dispatched > 40 {
		t.Fatalf("在途封顶失效：派发 %d 远超背压上界（期望 ≲ 20）", res.Dispatched)
	}
}

// 热身：排定时刻落在档前段热身窗口内的样本标 warmup（不参与判定与指标），之后的不标
func TestPacer_WarmupMarksEarlySamples(t *testing.T) {
	srv, _ := sseChatServer(t, 0, nil)
	in := pacerInput(srv.URL, nil, nil)
	cfg := pacedStageConfig{TargetRate: 50, MaxTokens: 16, Prompt: "hi", Duration: 400 * time.Millisecond, Warmup: 200 * time.Millisecond, MaxInFlight: 256}

	res, err := runPacedStage(context.Background(), in, 0, "r50", cfg, nil)
	if err != nil {
		t.Fatalf("runPacedStage error: %v", err)
	}
	stageStart := res.Samples[0].DispatchedAt // tick0 即档起点
	var warm, judged int
	for _, s := range res.Samples {
		inWarmup := s.DispatchedAt.Sub(stageStart) < cfg.Warmup
		if s.Warmup != inWarmup {
			t.Fatalf("seq %d 排定于 +%v，warmup=%v 与热身窗口 %v 不符", s.Seq, s.DispatchedAt.Sub(stageStart), s.Warmup, cfg.Warmup)
		}
		if s.Warmup {
			warm++
		} else {
			judged++
		}
	}
	if warm == 0 || judged == 0 {
		t.Fatalf("热身 %d 条、判定 %d 条，两段都应有样本", warm, judged)
	}
	if res.MeasuredSec <= 0 || res.MeasuredSec >= res.DurationSec {
		t.Errorf("MeasuredSec=%v 应为发压窗口扣掉热身后的时长（DurationSec=%v）", res.MeasuredSec, res.DurationSec)
	}
}
