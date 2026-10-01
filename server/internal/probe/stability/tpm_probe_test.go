package stability

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/Yukiho0287/assay/server/internal/probe"
	"github.com/Yukiho0287/assay/server/internal/probe/protocol"
)

// tpmInput 组装跑 tpm_probe 的 RunInput，并收集各档 StageMetrics。
// 令 TpmMaxTokensPerReq=76 → 每请求 token 权重 weight=24+76=100，token 速率 ÷100 即请求速率，
// 从而复用 RPM 测试同一套令牌桶上游（按请求限速）与相同的请求速率行为。
func tpmInput(t *testing.T, baseURL string, tune func(*StabilityParams)) (RunInput, *[]StageMetrics) {
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
		Probe:  tpmProbeID,
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

// 二分收敛：令牌桶按请求限速 limit=15/s，weight=100 → token 速率 2000/4000 对应 20/40 req/s。
// 断言 TPM 收敛到真实边界附近（token 速率 × 60），且触发过限速（非触顶护栏）。
func TestTpm_ConvergesToBoundary(t *testing.T) {
	srv := tokenBucketServer(t, 15)
	in, metrics := tpmInput(t, srv.URL, func(p *StabilityParams) {
		p.TpmMaxTokensPerReq = 76 // weight = 24 + 76 = 100
		p.TpmStartRate = 500      // 5 req/s
		p.TpmMaxRate = 4000       // 40 req/s
		p.TpmStageSec = 1
		p.TpmBinarySteps = 4
		p.TpmMaxInFlight = 256
	})

	if err := runTpm(context.Background(), in); err != nil {
		t.Fatalf("runTpm error: %v", err)
	}
	overall := overallOf(t, *metrics)
	if overall.Metrics.ReachedCap {
		t.Fatalf("limit=15 req/s 远低于护栏，不应 reachedCap")
	}
	tpm := overall.Metrics.ConvergedTpm
	// 收敛的可持续请求速率 lo∈[5,30] req/s（RPM 测试已证）；token 速率=lo×100，TPM=lo×100×60 → [30000,180000]
	if tpm < 30000 || tpm > 180000 {
		t.Fatalf("收敛 TPM=%.0f 偏离真实边界（期望 30000-180000）", tpm)
	}

	// 最低 token 速率档不应限速；护栏顶 token 速率档应限速
	var sawLowPass, sawHighLimited bool
	for _, m := range *metrics {
		if m.Stage == StageOverall {
			continue
		}
		if m.Metrics.TargetTokenRate == 500 && !m.Metrics.RateLimited {
			sawLowPass = true
		}
		if m.Metrics.TargetTokenRate == 4000 && m.Metrics.RateLimited {
			sawHighLimited = true
		}
	}
	if !sawLowPass {
		t.Fatalf("500 token/s（5 req/s）档不应被判限速")
	}
	if !sawHighLimited {
		t.Fatalf("4000 token/s（40 req/s）档应被判限速")
	}
}

// 触顶护栏：上游不限速，ramp 升到护栏顶仍无 429 → reachedCap=true，收敛 TPM=护栏 token 速率×60。
func TestTpm_ReachedCap(t *testing.T) {
	srv, _ := sseChatServer(t, 0, nil) // 永不 429
	in, metrics := tpmInput(t, srv.URL, func(p *StabilityParams) {
		p.TpmMaxTokensPerReq = 76 // weight = 100
		p.TpmStartRate = 400      // 4 req/s
		p.TpmMaxRate = 1600       // 16 req/s
		p.TpmStageSec = 1
		p.TpmBinarySteps = 4
		p.TpmMaxInFlight = 256
	})

	if err := runTpm(context.Background(), in); err != nil {
		t.Fatalf("runTpm error: %v", err)
	}
	overall := overallOf(t, *metrics)
	if !overall.Metrics.ReachedCap {
		t.Fatalf("上游不限速应 reachedCap=true")
	}
	if overall.Metrics.ConvergedTpm != 1600*60 {
		t.Fatalf("触顶护栏收敛 TPM 应为 %d，实际 %.0f", 1600*60, overall.Metrics.ConvergedTpm)
	}
	// 触顶时不应进入二分：档数 = ramp 档数（400/800/1600）+ overall
	nonOverall := 0
	for _, m := range *metrics {
		if m.Stage != StageOverall {
			nonOverall++
		}
	}
	if nonOverall != 3 {
		t.Fatalf("触顶应只有 3 个 ramp 档（400/800/1600），实际 %d", nonOverall)
	}

	// 实测 token 吞吐应被记录（每 ok 样本 usage=输入3+输出2）
	if overall.Metrics.Requests == 0 {
		t.Fatalf("overall 应聚合到样本")
	}
}

// estTpmRequests 最坏预估：ramp 各档按 token 速率换算请求数 + binary 各档按护栏顶换算，确定性求和。
func TestEstTpmRequests(t *testing.T) {
	var p StabilityParams
	p.ApplyDefaults()
	p.TpmMaxTokensPerReq = 76 // weight = 100
	p.TpmStartRate = 200
	p.TpmMaxRate = 800
	p.TpmStageSec = 10
	p.TpmBinarySteps = 4
	// rampRates(200,800)=[200,400,800]；perStageEst(tr/100,10)=ceil(tr/10)+1 → 21+41+81=143
	// binary: 4 × perStageEst(800/100,10)=4×81=324；合计 467
	if got := estTpmRequests(p); got != 467 {
		t.Fatalf("estTpmRequests=%d，期望 467", got)
	}
}

// sumTokens 累计渠道正常应答样本的输入+输出 token，跳过失败样本与缺 usage 的样本。
// 输出上限被思考用完的样本也是真实消耗的 token，必须计入（推理模型在 TPM 砝码下大多如此）。
func TestSumTokens(t *testing.T) {
	samples := []Sample{
		{Ok: true, InputTokens: 3, OutputTokens: 2},                                     // 5
		{Ok: true, InputTokens: 10, OutputTokens: 0},                                    // 10（输出缺省不计）
		{Ok: false, InputTokens: 100, OutputTokens: 100},                                // 失败不计
		{Ok: true, InputTokens: 0, OutputTokens: 7},                                     // 7
		{Ok: false, ErrorClass: ErrBudgetExhausted, InputTokens: 20, OutputTokens: 256}, // 276
	}
	if got := sumTokens(samples); got != 298 {
		t.Fatalf("sumTokens=%d，期望 298", got)
	}
	if got := sumTokens(nil); got != 0 {
		t.Fatalf("空样本 sumTokens 应为 0，得 %d", got)
	}
}
