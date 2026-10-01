package stability

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Yukiho0287/assay/server/internal/probe"
	"github.com/Yukiho0287/assay/server/internal/probe/protocol"
)

// mockChatStream 返回一个最小 openai_chat SSE 流（含 include_usage 尾帧）
func mockChatStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
	fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func ladderInput(t *testing.T, baseURL string, params StabilityParams, caps *CapGuard) (RunInput, *[]Sample, *[]StageMetrics, *int) {
	t.Helper()
	codec, _ := protocol.Get(protocol.ProtocolOpenAIChat)
	var mu sync.Mutex
	samples := &[]Sample{}
	metrics := &[]StageMetrics{}
	progressMax := new(int)
	in := RunInput{
		Probe:  ladderProbeID,
		Target: probe.Target{BaseURL: baseURL, Model: "m"},
		APIKey: "sk-test",
		Params: params,
		Client: http.DefaultClient,
		Codec:  codec,
		Caps:   caps,
		Sample: func(_ context.Context, s Sample) error {
			mu.Lock()
			*samples = append(*samples, s)
			mu.Unlock()
			return nil
		},
		Metric: func(_ context.Context, m StageMetrics) error {
			mu.Lock()
			*metrics = append(*metrics, m)
			mu.Unlock()
			return nil
		},
		Progress: func(_ context.Context, done, _ int) {
			mu.Lock()
			if done > *progressMax {
				*progressMax = done
			}
			mu.Unlock()
		},
	}
	return in, samples, metrics, progressMax
}

// TestLadderHappyPath 阶梯并发跑通：样本数、档级+overall 指标、进度封顶
func TestLadderHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mockChatStream(w)
	}))
	defer srv.Close()

	params := StabilityParams{
		Protocol:          protocol.ProtocolOpenAIChat,
		ConcurrencyLadder: []int{1, 2},
		RequestsPerStage:  3,
		WarmupPerStage:    1,
		LadderMaxTokens:   16,
		RequestTimeoutMs:  5000,
	}
	params.ApplyDefaults()
	in, samples, metrics, progressMax := ladderInput(t, srv.URL, params, nil)

	if err := runLadder(context.Background(), in); err != nil {
		t.Fatalf("runLadder 出错: %v", err)
	}

	// 2 档 ×（1 预热 + 3 计入）= 8 样本
	if len(*samples) != 8 {
		t.Errorf("样本数 = %d，期望 8", len(*samples))
	}
	for _, s := range *samples {
		if !s.Ok {
			t.Errorf("样本 %s#%d 非 ok: %s", s.Stage, s.Seq, s.Error)
		}
	}
	// 2 档级 + 1 overall
	if len(*metrics) != 3 {
		t.Fatalf("指标行数 = %d，期望 3", len(*metrics))
	}
	last := (*metrics)[2]
	if last.Stage != StageOverall || last.StageIndex != StageOverallIndex {
		t.Errorf("末行应为 overall，得 %s/%d", last.Stage, last.StageIndex)
	}
	// overall 剔除预热：2 档 × 3 计入 = 6
	if last.Metrics.Requests != 6 {
		t.Errorf("overall requests = %d，期望 6", last.Metrics.Requests)
	}
	// 档级 requests 应为 3（剔除 1 预热）、并发标注正确
	stage0 := (*metrics)[0]
	if stage0.Metrics.Requests != 3 {
		t.Errorf("档0 requests = %d，期望 3", stage0.Metrics.Requests)
	}
	if stage0.Metrics.Concurrency != 1 {
		t.Errorf("档0 并发标注 = %d，期望 1", stage0.Metrics.Concurrency)
	}
	if *progressMax != 8 {
		t.Errorf("进度封顶 = %d，期望 8", *progressMax)
	}
}

// TestLadderCapStops 请求硬闸触发时提前收敛
func TestLadderCapStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mockChatStream(w)
	}))
	defer srv.Close()

	params := StabilityParams{
		Protocol:          protocol.ProtocolOpenAIChat,
		ConcurrencyLadder: []int{1},
		RequestsPerStage:  10,
		WarmupPerStage:    0,
		LadderMaxTokens:   16,
		RequestTimeoutMs:  5000,
	}
	params.ApplyDefaults()
	caps := NewCapGuard(2, 0, 0) // 只准 2 个请求
	in, samples, _, _ := ladderInput(t, srv.URL, params, caps)

	if err := runLadder(context.Background(), in); err != nil {
		t.Fatalf("runLadder 出错: %v", err)
	}
	if len(*samples) != 2 {
		t.Errorf("硬闸下样本数 = %d，期望 2", len(*samples))
	}
}

// TestLadderRateLimited 上游 429 → rate_limited 分类，任务不报错
func TestLadderRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":"rate limited"}`)
	}))
	defer srv.Close()

	params := StabilityParams{
		Protocol:          protocol.ProtocolOpenAIChat,
		ConcurrencyLadder: []int{1},
		RequestsPerStage:  2,
		WarmupPerStage:    0,
		LadderMaxTokens:   16,
		RequestTimeoutMs:  5000,
	}
	params.ApplyDefaults()
	in, samples, metrics, _ := ladderInput(t, srv.URL, params, nil)

	if err := runLadder(context.Background(), in); err != nil {
		t.Fatalf("runLadder 不应因 429 报错: %v", err)
	}
	for _, s := range *samples {
		if s.Ok || s.ErrorClass != ErrRateLimited {
			t.Errorf("样本应为 rate_limited，得 ok=%v class=%s", s.Ok, s.ErrorClass)
		}
		if s.HTTPStatus != http.StatusTooManyRequests {
			t.Errorf("状态码 = %d，期望 429", s.HTTPStatus)
		}
	}
	stage0 := (*metrics)[0]
	if stage0.Metrics.ErrorRate != 1 {
		t.Errorf("错误率 = %v，期望 1", stage0.Metrics.ErrorRate)
	}
	if stage0.Metrics.ByErrorClass[ErrRateLimited] != 2 {
		t.Errorf("byErrorClass = %v", stage0.Metrics.ByErrorClass)
	}
}
