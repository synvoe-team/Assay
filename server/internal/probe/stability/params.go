// Package stability 稳定性检测：并发/RPM/TPM 等性能指标。与质量检测共享任务编排层，
// 但自成一套 probe / 结果模型 —— 产出的是时序性能指标，装不进「用例×模式→passed 占比」
// 的合规判定模型，故不复用 quality 的 probe.Probe/CaseResult/Checkpoint。
package stability

import (
	"errors"
	"fmt"
)

// StabilityParams 稳定性任务运行参数（落 tasks.params jsonb）：协议 + 负载画像 + 各 probe 档位 + 全局硬闸。
type StabilityParams struct {
	// Protocol 本任务实选协议（三协议择一），写进快照
	Protocol string `json:"protocol"`

	// Workload 负载画像：每条请求的输入 token / 缓存命中率 / 输出 token，三个 probe 共用
	Workload Workload `json:"workload"`

	// —— 阶梯并发（闭环，测延迟曲线）——
	ConcurrencyLadder []int `json:"concurrencyLadder"` // 各并发档，默认 [1,2,4,8,16]
	RequestsPerStage  int   `json:"requestsPerStage"`  // 每档计入统计的请求数，默认 20
	WarmupPerStage    int   `json:"warmupPerStage"`    // 每档预热请求数（评估剔除），默认 2

	// —— RPM 实测（开环，恒定到达率二分收敛速率边界）——
	RpmStartRate      float64 `json:"rpmStartRate"`      // 起始到达率 req/s，默认 2
	RpmMaxRate        float64 `json:"rpmMaxRate"`        // 探测速率护栏上限 req/s，默认 20
	RpmStageSec       int     `json:"rpmStageSec"`       // 每档发压时长秒：前一半热身、只用后一半判限速（后一半≈对限流窗口的假设），默认 120
	RpmMaxInFlight    int     `json:"rpmMaxInFlight"`    // 在途请求上限（防雪崩兜底），默认 128
	RpmLimitThreshold float64 `json:"rpmLimitThreshold"` // 判定本档触发限速的 429 占比阈值，默认 0.1
	RpmBinarySteps    int     `json:"rpmBinarySteps"`    // 找到限速档后二分细化步数，默认 4

	// —— TPM 实测（开环，恒定 token 到达率二分收敛 token 速率边界；输入+输出都计）——
	TpmStartRate      float64 `json:"tpmStartRate"`      // 起始 token 到达率 token/s，默认 200
	TpmMaxRate        float64 `json:"tpmMaxRate"`        // 探测 token 速率护栏上限 token/s，默认 2000
	TpmStageSec       int     `json:"tpmStageSec"`       // 每档发压时长秒：前一半热身、只用后一半判限速（后一半≈对限流窗口的假设），默认 120
	TpmMaxInFlight    int     `json:"tpmMaxInFlight"`    // 在途请求上限（防雪崩兜底），默认 128
	TpmLimitThreshold float64 `json:"tpmLimitThreshold"` // 判定本档触发限速的 429 占比阈值，默认 0.1
	TpmBinarySteps    int     `json:"tpmBinarySteps"`    // 找到限速档后二分细化步数，默认 4

	// —— 全局硬闸（成本护栏，碰任一即停）——
	MaxTotalRequests int `json:"maxTotalRequests"` // 累计请求上限，默认 10000
	MaxTotalTokens   int `json:"maxTotalTokens"`   // 累计 token 上限，默认 2_000_000
	MaxDurationSec   int `json:"maxDurationSec"`   // 整任务墙钟上限秒（到点停派新请求，在途的跑完），默认 3600
	RequestTimeoutMs int `json:"requestTimeoutMs"` // 单请求超时，默认 60000
}

// 默认值常量（前端估算与后端 fail-fast 共用同一套口径）
const (
	DefaultRequestsPerStage = 20
	DefaultWarmupPerStage   = 2
	DefaultMaxTotalRequests = 10000
	DefaultMaxTotalTokens   = 2_000_000
	DefaultMaxDurationSec   = 3600
	DefaultRequestTimeoutMs = 60000

	DefaultRpmStartRate      = 2.0
	DefaultRpmMaxRate        = 20.0
	DefaultRpmStageSec       = 120
	DefaultRpmMaxInFlight    = 128
	DefaultRpmLimitThreshold = 0.1
	DefaultRpmBinarySteps    = 4

	DefaultTpmStartRate      = 200.0
	DefaultTpmMaxRate        = 2000.0
	DefaultTpmStageSec       = 120
	DefaultTpmMaxInFlight    = 128
	DefaultTpmLimitThreshold = 0.1
	DefaultTpmBinarySteps    = 4

	// 未设 workload.output 时各 probe 的生成上限：阶梯并发要容得下推理模型先思考再写正文（太小测不到 TTFT）；
	// RPM 只关心请求速率取小；TPM 是每请求 token 砝码（顶格 prompt 打满）
	DefaultLadderMaxTokens = 2048
	DefaultRpmMaxTokens    = 16
	DefaultTpmMaxTokens    = 256
)

// DefaultConcurrencyLadder 默认并发阶梯
func DefaultConcurrencyLadder() []int { return []int{1, 2, 4, 8, 16} }

// ApplyDefaults 补齐零值为默认值。WarmupPerStage=0 是合法配置（不预热），不补默认。
func (p *StabilityParams) ApplyDefaults() {
	if len(p.ConcurrencyLadder) == 0 {
		p.ConcurrencyLadder = DefaultConcurrencyLadder()
	}
	if p.RequestsPerStage == 0 {
		p.RequestsPerStage = DefaultRequestsPerStage
	}
	if p.Workload.Input.Mode == "" {
		p.Workload.Input.Mode = InputNone
	}
	if p.RpmStartRate == 0 {
		p.RpmStartRate = DefaultRpmStartRate
	}
	if p.RpmMaxRate == 0 {
		p.RpmMaxRate = DefaultRpmMaxRate
	}
	if p.RpmStageSec == 0 {
		p.RpmStageSec = DefaultRpmStageSec
	}
	if p.RpmMaxInFlight == 0 {
		p.RpmMaxInFlight = DefaultRpmMaxInFlight
	}
	if p.RpmLimitThreshold == 0 {
		p.RpmLimitThreshold = DefaultRpmLimitThreshold
	}
	if p.RpmBinarySteps == 0 {
		p.RpmBinarySteps = DefaultRpmBinarySteps
	}
	if p.TpmStartRate == 0 {
		p.TpmStartRate = DefaultTpmStartRate
	}
	if p.TpmMaxRate == 0 {
		p.TpmMaxRate = DefaultTpmMaxRate
	}
	if p.TpmStageSec == 0 {
		p.TpmStageSec = DefaultTpmStageSec
	}
	if p.TpmMaxInFlight == 0 {
		p.TpmMaxInFlight = DefaultTpmMaxInFlight
	}
	if p.TpmLimitThreshold == 0 {
		p.TpmLimitThreshold = DefaultTpmLimitThreshold
	}
	if p.TpmBinarySteps == 0 {
		p.TpmBinarySteps = DefaultTpmBinarySteps
	}
	if p.MaxTotalRequests == 0 {
		p.MaxTotalRequests = DefaultMaxTotalRequests
	}
	if p.MaxTotalTokens == 0 {
		p.MaxTotalTokens = DefaultMaxTotalTokens
	}
	if p.MaxDurationSec == 0 {
		p.MaxDurationSec = DefaultMaxDurationSec
	}
	if p.RequestTimeoutMs == 0 {
		p.RequestTimeoutMs = DefaultRequestTimeoutMs
	}
}

// Validate 入口即验（fail-fast）：非法参数在创建任务时立即拒绝，不带进执行期。
func (p StabilityParams) Validate() error {
	if p.Protocol == "" {
		return errors.New("必须指定协议")
	}
	if len(p.ConcurrencyLadder) == 0 || len(p.ConcurrencyLadder) > 20 {
		return errors.New("并发阶梯需 1-20 档")
	}
	for _, c := range p.ConcurrencyLadder {
		if c < 1 || c > 512 {
			return fmt.Errorf("并发档 %d 越界（需 1-512）", c)
		}
	}
	if p.RequestsPerStage < 1 || p.RequestsPerStage > 1000 {
		return errors.New("每档请求数需 1-1000")
	}
	if p.WarmupPerStage < 0 || p.WarmupPerStage > 100 {
		return errors.New("每档预热数需 0-100")
	}
	if p.RpmStartRate <= 0 || p.RpmStartRate > 1000 {
		return errors.New("RPM 起始速率需 0-1000 req/s")
	}
	if p.RpmMaxRate < p.RpmStartRate || p.RpmMaxRate > 1000 {
		return errors.New("RPM 速率上限需 ≥ 起始速率且 ≤1000 req/s")
	}
	if p.RpmStageSec < 2 || p.RpmStageSec > 600 {
		return errors.New("RPM 每档时长需 2-600s（前一半热身、后一半判定）")
	}
	if p.RpmMaxInFlight < 1 || p.RpmMaxInFlight > 4096 {
		return errors.New("RPM 在途上限需 1-4096")
	}
	if p.RpmLimitThreshold <= 0 || p.RpmLimitThreshold > 1 {
		return errors.New("RPM 限速阈值需 0-1（429 占比）")
	}
	if p.RpmBinarySteps < 0 || p.RpmBinarySteps > 12 {
		return errors.New("RPM 二分步数需 0-12")
	}
	if p.TpmStartRate <= 0 || p.TpmStartRate > 1_000_000 {
		return errors.New("TPM 起始 token 速率需 0-1000000 token/s")
	}
	if p.TpmMaxRate < p.TpmStartRate || p.TpmMaxRate > 1_000_000 {
		return errors.New("TPM token 速率上限需 ≥ 起始速率且 ≤1000000 token/s")
	}
	if p.TpmStageSec < 2 || p.TpmStageSec > 600 {
		return errors.New("TPM 每档时长需 2-600s（前一半热身、后一半判定）")
	}
	if p.TpmMaxInFlight < 1 || p.TpmMaxInFlight > 4096 {
		return errors.New("TPM 在途上限需 1-4096")
	}
	if p.TpmLimitThreshold <= 0 || p.TpmLimitThreshold > 1 {
		return errors.New("TPM 限速阈值需 0-1（429 占比）")
	}
	if p.TpmBinarySteps < 0 || p.TpmBinarySteps > 12 {
		return errors.New("TPM 二分步数需 0-12")
	}
	if p.MaxTotalRequests < 1 {
		return errors.New("总请求上限须为正")
	}
	if p.MaxTotalTokens < 1 {
		return errors.New("总 token 上限须为正")
	}
	if p.MaxDurationSec < 60 || p.MaxDurationSec > 7200 {
		return errors.New("整任务时长上限需 60-7200s")
	}
	if p.RequestTimeoutMs < 1000 || p.RequestTimeoutMs > 600000 {
		return errors.New("单请求超时需 1000-600000ms")
	}
	return p.validateWorkload()
}

// validateWorkload 负载画像的 fail-fast：取值范围、缓存命中率的可行性、单请求名义 token 与超时下限。
func (p StabilityParams) validateWorkload() error {
	w := p.Workload
	if err := w.Input.validate(); err != nil {
		return err
	}
	if w.Output < 0 || w.Output > maxOutputTokens {
		return fmt.Errorf("输出 token 目标需 1-%d（不填 = 各检测项默认）", maxOutputTokens)
	}
	if err := w.validateCache(); err != nil {
		return err
	}
	if !w.Input.shaped() {
		return nil
	}
	// 单请求名义 token = 最大输入 + 输出上限（未设输出目标时按各 probe 默认里最大的阶梯并发 2048 计）
	output := w.Output
	if output == 0 {
		output = DefaultLadderMaxTokens
	}
	if nominal := w.Input.ceil() + output; nominal > p.MaxTotalTokens {
		return fmt.Errorf("单条请求名义 %d token（输入 %d + 输出 %d）已超过总 token 上限 %d，一条都发不出去", nominal, w.Input.ceil(), output, p.MaxTotalTokens)
	}
	// 超时下限：大输入的 prefill 本身就慢，按每千 token 1 秒、不低于 60 秒
	if floor := max(minShapedTimeoutMs, w.Input.ceil()*timeoutMsPerToken); p.RequestTimeoutMs < floor {
		return fmt.Errorf("输入最大 %d token 时单请求超时至少 %dms（每千 token 1 秒、不低于 60 秒），当前 %dms", w.Input.ceil(), floor, p.RequestTimeoutMs)
	}
	return nil
}
