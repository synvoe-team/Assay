package stability

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

const (
	tpmProbeID = "tpm_probe"
	// tpmRateTol 二分收敛精度：档间 token 速率差窄于此即停（≈1200 TPM 分辨率）
	tpmRateTol = 20.0
)

// NewTpmProbe TPM 实测检测项（开环）：恒定 token 到达率阶梯升压，出现持续 429 后二分收敛真实 TPM 边界。
// 保守假设「输入+输出都计」：输入按负载画像塑形、输出用 max_tokens 砝码 + 顶格数数 prompt 打满，每请求 token 权重可预估。
func NewTpmProbe() Probe {
	return Probe{
		Info: Info{
			ID:          tpmProbeID,
			Name:        "TPM 实测",
			Description: "开环恒定 token 到达率发压：以「输入+输出都计」的每请求 token 权重换算请求速率，从起始 token 速率几何递增，出现持续 429 后二分收敛渠道可持续的每分钟 token 数（TPM）边界。",
			Protocols:   nil, // 三协议通用
			EstRequests: estTpmRequests,
		},
		Run: runTpm,
	}
}

// tpmWeightPerReq 每请求 token 权重（名义）：输入目标期望值（递增/抖动取区间均值；不塑形按小 prompt 名义估）
// + 输出目标。仅用于「目标 token 速率 → 请求速率」的先验换算；实测 token 吞吐仍以响应 usage 为准。
func tpmWeightPerReq(p StabilityParams) float64 {
	w := p.Workload
	input := w.Input.mean()
	if !w.Input.shaped() {
		input = float64(nominalTokens(uniquePrompt("xxxxxxxx", "t0", 0, countPrompt)))
	}
	return input + float64(w.maxTokens(tpmOutput))
}

// tpmStageLabel token 速率档标识（图表 x 轴），如 t200 / t1500。单次运行内各档 token 速率互异。
func tpmStageLabel(tokenRate float64) string { return fmt.Sprintf("t%g", tokenRate) }

// estTpmRequests 最坏预估：把各 token 速率档换算成请求速率后按 ⌈reqRate×秒⌉+1 求和，binary 各档按护栏顶换算。
func estTpmRequests(p StabilityParams) int {
	w := tpmWeightPerReq(p)
	total := 0
	for _, tr := range rampRates(p.TpmStartRate, p.TpmMaxRate) {
		total += perStageEst(tr/w, p.TpmStageSec)
	}
	total += p.TpmBinarySteps * perStageEst(p.TpmMaxRate/w, p.TpmStageSec)
	return total
}

// sumTokens 累计一档正常应答样本的真实 token 消耗（输入+输出，缺 usage 的样本记 0；输出上限被思考用完的也是真实消耗）。
func sumTokens(samples []Sample) int {
	total := 0
	for _, s := range samples {
		if !served(s) {
			continue
		}
		if s.InputTokens > 0 {
			total += s.InputTokens
		}
		if s.OutputTokens > 0 {
			total += s.OutputTokens
		}
	}
	return total
}

func runTpm(ctx context.Context, in RunInput) error {
	p := in.Params
	weight := tpmWeightPerReq(p)
	total := estTpmRequests(p)
	var pmu sync.Mutex
	done := 0
	afterEach := func() {
		pmu.Lock()
		done++
		if in.Progress != nil {
			in.Progress(ctx, done, total)
		}
		pmu.Unlock()
	}
	stageDur := time.Duration(p.TpmStageSec) * time.Second

	var (
		overall     []Sample
		lastHeaders map[string]string
		stageIndex  int
	)

	// runOne 跑一档：token 速率换算为请求速率发压→评估→落档级指标→并入 overall；返回本档是否限速、是否撞硬闸。
	runOne := func(tokenRate float64) (stageOutcome, error) {
		reqRate := tokenRate / weight
		stage := tpmStageLabel(tokenRate)
		cfg := pacedStageConfig{
			TargetRate:  reqRate,
			Output:      tpmOutput,
			Duration:    stageDur,
			Warmup:      stageDur / 2, // 前一半热身，只用后一半判定（见 pacedStageConfig.Warmup）
			MaxInFlight: p.TpmMaxInFlight,
		}
		res, err := runPacedStage(ctx, in, stageIndex, stage, cfg, afterEach)
		if err != nil {
			return stageOutcome{}, err
		}
		judged := measured(res.Samples)
		limited := isRateLimited(judged, p.TpmLimitThreshold)
		achievedReq, achievedTok := 0.0, 0.0
		if res.DurationSec > 0 {
			achievedReq = math.Round(float64(res.Dispatched)/res.DurationSec*100) / 100
		}
		if res.MeasuredSec > 0 {
			achievedTok = float64(sumTokens(judged)) / res.MeasuredSec
		}
		if res.RateHeaders != nil {
			lastHeaders = res.RateHeaders
		}
		sm := evaluateTpmStage(in.Probe, stage, stageIndex, p.Workload.CacheHitRate, reqRate, achievedReq, tokenRate, achievedTok, limited, res.RateHeaders, res.Samples)
		if err := in.emit(ctx, sm); err != nil {
			return stageOutcome{}, err
		}
		overall = append(overall, judged...)
		stageIndex++
		return stageOutcome{limited: limited, stopped: res.Stopped}, nil
	}

	b, err := searchBoundary(ctx, rampRates(p.TpmStartRate, p.TpmMaxRate), p.TpmBinarySteps, tpmRateTol, runOne)
	if err != nil {
		return err
	}
	return in.emit(ctx, evaluateTpmOverall(in.Probe, p.Workload.CacheHitRate, overall, b, p.TpmMaxRate, in.Caps.Reason(), lastHeaders))
}
