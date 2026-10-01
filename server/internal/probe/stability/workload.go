package stability

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Yukiho0287/assay/server/internal/probe"
	"github.com/Yukiho0287/assay/server/internal/probe/protocol"
)

// 负载画像：每条压测请求的输入 token（定量/递增/抖动）、缓存命中率、输出 token。
// 输入按「填充文本 × 实测字符/token 比」塑形：任务开头发 1 条定标请求读 usage.prompt_tokens
// 测出比例；h>0 时 prompt = 任务内字节相同的共享前缀 S（≈h×输入目标）+ 每请求唯一段，
// 定标后再串行发写缓存请求把 S 写进渠道缓存。

// 输入塑形模式
const (
	InputNone   = "none"   // 不塑形：沿用一句话小 prompt
	InputFixed  = "fixed"  // 定量：每条都是 Value
	InputRamp   = "ramp"   // 递增：按本档排定顺序从 Min 匀速涨到 Max
	InputJitter = "jitter" // 抖动：[Min,Max] 均匀随机，以任务 nonce 为种子（可复现）
)

const (
	maxInputTokens  = 1_000_000
	maxOutputTokens = 32768
	maxCacheHitRate = 0.95
	// minCacheableTokens 主流渠道前缀缓存的最小可缓存长度（OpenAI 自动缓存 / Anthropic cache_control 均为 1024）
	minCacheableTokens = 1024
	// minUniqueTokens h>0 时每请求唯一段的下限：DeepSeek 按 64 token 块对齐缓存，唯一段再短会被并进共享块
	minUniqueTokens = 64
	// 塑形时单请求超时下限：大输入的 prefill 本身就慢，按每千 token 1 秒、不低于 60 秒
	minShapedTimeoutMs = 60000
	timeoutMsPerToken  = 1

	// nominalCharsPerToken 填充文本的名义字符/token 比（英文常见词约 4），定标前的先验
	nominalCharsPerToken = 4.0
	// calibChars 定标请求的填充字符数（≈3000 token）：够长让问题与标记的估算误差可忽略
	calibChars = 12000
	// cacheWarmupRequests h>0 时定标后串行发的写缓存请求数：第 1 条写入、第 2 条验证命中
	cacheWarmupRequests = 2
	// prepAttempts 定标/写缓存遇瞬时失败（传输/限流/5xx/断流）的最多尝试次数
	prepAttempts = 3

	// cacheTolerance 实测命中率与目标 h 的容差（绝对值）
	cacheTolerance = 0.10
	// deviationLimitPct 目标 vs 实测 token 偏差阈值：|偏差| p95 超过即该档标黄
	deviationLimitPct = 10.0

	// WorkloadProbeID 定标/写缓存样本挂的伪检测项（不出指标，只留证据）
	WorkloadProbeID = "workload"
	stageCalib      = "calib"
	stageCacheWrite = "cache"

	paragraphBreak = "\n\n"
)

// 压测问题（放在 prompt 最后一句）
const (
	// introPrompt 不塑形、无输出目标时的默认问题（与上线前的固定 prompt 逐字一致）
	introPrompt = "用一句话简要介绍你自己。"
	// countPrompt 顶格数数：诱导模型一直生成到 max_tokens，让输出 token 打满目标
	countPrompt = "请从 1 开始逐个数数：1、2、3、4 …… 一直数下去，数字之间用顿号分隔，不要停、不要重复、不要输出任何多余说明。"
	// paragraphQuestion 有填充、无输出目标时的问题：答案就是一个数字，输出可控
	paragraphQuestion = "以上文本共多少段？只回答数字。"
)

// InputSpec 输入 token 目标
type InputSpec struct {
	Mode  string `json:"mode"`
	Value int    `json:"value,omitempty"` // fixed 用
	Min   int    `json:"min,omitempty"`   // ramp/jitter 用
	Max   int    `json:"max,omitempty"`
}

// shaped 是否塑形输入（none 与未填都不塑形）
func (s InputSpec) shaped() bool { return s.Mode != "" && s.Mode != InputNone }

func (s InputSpec) validate() error {
	switch s.Mode {
	case "", InputNone:
		return nil
	case InputFixed:
		if s.Value < 1 || s.Value > maxInputTokens {
			return fmt.Errorf("输入 token 目标需 1-%d", maxInputTokens)
		}
	case InputRamp, InputJitter:
		if s.Min < 1 || s.Max > maxInputTokens || s.Min >= s.Max {
			return fmt.Errorf("输入 token 区间需 1 ≤ 下限 < 上限 ≤ %d", maxInputTokens)
		}
	default:
		return fmt.Errorf("未知输入模式 %q（需 none/fixed/ramp/jitter）", s.Mode)
	}
	return nil
}

// floor / ceil / mean 输入目标的最小值、最大值、期望值（不塑形为 0）
func (s InputSpec) floor() int {
	if s.Mode == InputFixed {
		return s.Value
	}
	return s.Min
}

func (s InputSpec) ceil() int {
	if s.Mode == InputFixed {
		return s.Value
	}
	return s.Max
}

func (s InputSpec) mean() float64 {
	switch s.Mode {
	case InputFixed:
		return float64(s.Value)
	case InputRamp, InputJitter:
		return float64(s.Min+s.Max) / 2
	}
	return 0
}

// target 某档第 seq 条请求的输入 token 目标；n 为本档排定的请求总数（递增用）。0 = 不塑形。
func (s InputSpec) target(nonce, stage string, seq, n int) int {
	switch s.Mode {
	case InputFixed:
		return s.Value
	case InputRamp:
		if n <= 1 {
			return s.Min
		}
		frac := min(float64(seq)/float64(n-1), 1) // 实发超出排定数（开环尾部）时钳在 Max
		return s.Min + int(math.Round(float64(s.Max-s.Min)*frac))
	case InputJitter:
		h := sha256.Sum256([]byte(nonce + "|" + stage + "|" + strconv.Itoa(seq)))
		return s.Min + int(binary.BigEndian.Uint64(h[:8])%uint64(s.Max-s.Min+1))
	}
	return 0
}

// Workload 负载画像（三个 probe 共用）
type Workload struct {
	Input        InputSpec `json:"input"`
	CacheHitRate float64   `json:"cacheHitRate,omitempty"` // 目标缓存命中率 h ∈ [0,0.95]
	Output       int       `json:"output,omitempty"`       // 输出 token 目标；0 = 各 probe 默认生成上限
	// Thinking 思考控制：default 不干预 / auto 预检自动探测关思考写法 / 指定某种写法（protocol.Thinking*）
	Thinking string `json:"thinking,omitempty"`
}

// sharedTokens 共享前缀 token 数 ≈ h × 输入期望值（任务内固定，递增/抖动也按均值）
func (w Workload) sharedTokens() int {
	return int(math.Round(w.CacheHitRate * w.Input.mean()))
}

// maxTokens 生成上限：设了输出目标一律用它，否则用该 probe 的默认值
func (w Workload) maxTokens(def outputDefault) int {
	if w.Output > 0 {
		return w.Output
	}
	return def.MaxTokens
}

// validateCache 命中率的可行性：共享前缀要够缓存最小长度，最小那条请求还要装得下唯一段
func (w Workload) validateCache() error {
	h := w.CacheHitRate
	if h < 0 || h > maxCacheHitRate {
		return fmt.Errorf("缓存命中率需 0-%v", maxCacheHitRate)
	}
	if h == 0 {
		return nil
	}
	if !w.Input.shaped() {
		return errors.New("缓存命中率 > 0 需先设定输入 token 目标（共享前缀按输入目标的比例构造）")
	}
	shared := w.sharedTokens()
	if shared < minCacheableTokens {
		return fmt.Errorf("共享前缀约 %d token（命中率 %v × 输入均值 %v），低于渠道前缀缓存的最小长度 %d，缓存不会生效",
			shared, h, w.Input.mean(), minCacheableTokens)
	}
	if unique := w.Input.floor() - shared; unique < minUniqueTokens {
		return fmt.Errorf("最小输入 %d token 装不下共享前缀 %d token + 至少 %d token 的唯一段，请调低命中率或收窄输入区间",
			w.Input.floor(), shared, minUniqueTokens)
	}
	return nil
}

// nominalTokens 名义 token 估算：ASCII 每 4 字符 1 token（向上取整），非 ASCII（中文等）每字 1 token。
// 只用于预扣、权重与扣除标记/问题的开销；真实输入以响应 usage 为准。
func nominalTokens(s string) int {
	ascii, other := 0, 0
	for _, r := range s {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}

// outputDefault 某 probe 未设输出目标时的生成口径
type outputDefault struct {
	MaxTokens int
	Fill      bool // 默认就要写满（TPM 砝码）：用顶格数数 prompt，目标输出 = MaxTokens
}

var (
	ladderOutput = outputDefault{MaxTokens: DefaultLadderMaxTokens}
	rpmOutput    = outputDefault{MaxTokens: DefaultRpmMaxTokens}
	tpmOutput    = outputDefault{MaxTokens: DefaultTpmMaxTokens, Fill: true}
)

// reqSpec 一条压测请求的排定：先算（便宜，派发前过硬闸预扣用），prompt 到请求 goroutine 里再拼
type reqSpec struct {
	Stage        string
	Seq          int
	Question     string
	MaxTokens    int
	TargetInput  int // 0 = 不塑形
	TargetOutput int // 0 = 无输出目标
}

// spec 排定某档第 seq 条请求；n 为本档排定的请求总数。
// 问题：有输出目标或 probe 默认要写满 → 顶格数数；否则有填充 → 数段落（答案一个数字）；否则一句话自我介绍。
func (in RunInput) spec(stage string, seq, n int, def outputDefault) reqSpec {
	w := in.Params.Workload
	sp := reqSpec{
		Stage:       stage,
		Seq:         seq,
		Question:    introPrompt,
		MaxTokens:   w.maxTokens(def),
		TargetInput: w.Input.target(in.Nonce, stage, seq, n),
	}
	// 预检从上游报错读出了模型上限：输出目标随之钳住，偏差按实发上限算、预扣也不虚高
	if c := in.Plan.Shape.MaxTokensCap; c > 0 && sp.MaxTokens > c {
		sp.MaxTokens = c
	}
	switch {
	case w.Output > 0 || def.Fill:
		sp.Question, sp.TargetOutput = countPrompt, sp.MaxTokens
	case sp.TargetInput > 0:
		sp.Question = paragraphQuestion
	}
	return sp
}

// prompt 拼出请求 prompt：唯一标记打头（h>0 时紧跟共享前缀之后）破缓存，问题在最后一句；
// 填充字符数 = (输入目标 − 共享前缀 − 标记与问题的名义开销) × 实测字符/token 比。
func (in RunInput) prompt(sp reqSpec) protocol.Prompt {
	tag := uniquePrompt(in.Nonce, sp.Stage, sp.Seq, "")
	if sp.TargetInput == 0 {
		return protocol.Prompt{Unique: tag + sp.Question}
	}
	tail := paragraphBreak + sp.Question
	fill := max(sp.TargetInput-in.Plan.SharedTokens-nominalTokens(tag+tail), 0)
	chars := int(math.Round(float64(fill) * in.Plan.Ratio))
	seed := in.Nonce + "|" + sp.Stage + "|" + strconv.Itoa(sp.Seq)
	return protocol.Prompt{Shared: in.Plan.Shared, Unique: tag + fillerText(seed, chars) + tail}
}

// nominal 单条请求的名义 token（输入目标 + 生成上限）：硬闸按它预扣
func (in RunInput) nominal(sp reqSpec) int64 {
	input := sp.TargetInput
	if input == 0 {
		input = nominalTokens(uniquePrompt(in.Nonce, sp.Stage, sp.Seq, sp.Question))
	}
	return int64(input + sp.MaxTokens)
}

// exec 按预检定下的请求形态发一条已过硬闸预扣的请求
func (in RunInput) exec(ctx context.Context, sp reqSpec, p protocol.Prompt, est int64) outcome {
	return in.send(ctx, sp, p, in.Plan.Shape, est)
}

// send 发一条已过硬闸预扣的请求，并按实测 usage 结算预扣。
// 没拿到 2xx 响应体（传输失败/HTTP 错误）退回预扣；拿到了却没带 usage 的按名义值保守计。
func (in RunInput) send(ctx context.Context, sp reqSpec, p protocol.Prompt, shape protocol.Shape, est int64) outcome {
	o := doRequest(ctx, in.Client, in.Codec, in.Target.BaseURL, in.APIKey, protocol.Load{
		Model:     in.Target.Model,
		Prompt:    p,
		MaxTokens: sp.MaxTokens,
		Shape:     shape,
	}, in.Params.RequestTimeoutMs)
	actual := est
	switch {
	case o.Usage.Ok:
		actual = o.Usage.Prompt + o.Usage.Completion
	case !o.HasBody:
		actual = 0
	}
	in.Caps.Settle(est, actual)
	return o
}

// emit 落一档指标；__overall__ 行附上预检与定标结果（做过的兼容调整、名义比/实测比/偏差进报告）
func (in RunInput) emit(ctx context.Context, sm StageMetrics) error {
	if sm.Stage == StageOverall {
		sm.Metrics.Preflight = in.Plan.Preflight
		sm.Metrics.Calibration = in.Plan.Calibration
	}
	if in.Metric == nil {
		return nil
	}
	return in.Metric(ctx, sm)
}

// Plan 准备期产出：预检定下的请求形态 + 填充文本的字符/token 比与共享前缀（不塑形时后几项为零值）
type Plan struct {
	Shape        protocol.Shape // 全任务一致的请求兼容形态
	Preflight    *Preflight
	Ratio        float64 // 填充文本字符/token（定标实测；定标失败时取名义比）
	Shared       string  // 共享前缀 S，任务内字节相同；h=0 为空
	SharedTokens int
	Calibration  *Calibration
}

// Calibration 定标结果，写进各 probe 的 __overall__ 指标
type Calibration struct {
	Chars           int     `json:"chars"`        // 定标填充字符数
	PromptTokens    int     `json:"promptTokens"` // 渠道回报的定标 prompt_tokens
	NominalRatio    float64 `json:"nominalRatio"` // 名义字符/token
	MeasuredRatio   float64 `json:"measuredRatio"`
	DeviationPct    float64 `json:"deviationPct"`              // (实测比 − 名义比) / 名义比 × 100
	SharedTokens    int     `json:"sharedTokens,omitempty"`    // 共享前缀 token 数（h>0）
	CacheWarmCached *int    `json:"cacheWarmCached,omitempty"` // 最后一条写缓存请求的 cached_tokens（应≈共享前缀）
	// Uncalibrated 定标没测成（上游不报 usage / 定标请求失败）：按名义比塑形照跑，输入偏差无从实测或不准
	Uncalibrated bool   `json:"uncalibrated,omitempty"`
	Reason       string `json:"reason,omitempty"` // 没测成的原因
}

// buildPlan 按字符/token 比构造共享前缀（种子 = 任务 nonce：任务内字节相同、跨任务不同，不吃上个任务的缓存）
func buildPlan(w Workload, nonce string, ratio float64) Plan {
	if !w.Input.shaped() {
		return Plan{}
	}
	p := Plan{Ratio: ratio, SharedTokens: w.sharedTokens()}
	if p.SharedTokens > 0 {
		p.Shared = fillerText(nonce+"|shared", int(math.Round(float64(p.SharedTokens)*ratio))) + paragraphBreak
	}
	return p
}

// EstPrepRequests 准备期请求数（进度分母）：预检 1 条（自适应重发不另计）+ 自动关思考的候选数上界
// + 塑形时 1 条定标 + h>0 时写缓存
func EstPrepRequests(p StabilityParams) int {
	n := 1
	if p.Workload.Thinking == ThinkingAuto {
		n += len(protocol.ThinkingCandidates(p.Protocol))
	}
	w := p.Workload
	switch {
	case !w.Input.shaped():
	case w.sharedTokens() > 0:
		n += 1 + cacheWarmupRequests
	default:
		n++
	}
	return n
}

// PrepareWorkload 任务开头的准备期：预检定请求形态（自适应兼容 + 自动关思考）→ 塑形时定标（测字符/token 比）
// → h>0 时串行写缓存。样本均标 warmup、不进统计；进度按逻辑请求计（重试与自适应重发不重复计）。
// 只有预检确认渠道根本连不上（4xx 调整穷尽）才返回 error；定标/写缓存失败都降级照跑。
func PrepareWorkload(ctx context.Context, in RunInput) (Plan, error) {
	total, done := EstPrepRequests(in.Params), 0
	step := func() {
		done++
		if in.Progress != nil {
			in.Progress(ctx, done, total)
		}
	}
	shape, pf, err := in.preflight(ctx, step)
	if err != nil {
		return Plan{}, err
	}
	in.Plan = Plan{Shape: shape, Preflight: pf}
	w := in.Params.Workload
	if !w.Input.shaped() {
		return in.Plan, nil
	}
	cal, err := in.calibrate(ctx)
	if err != nil {
		return Plan{}, err
	}
	step()
	plan := buildPlan(w, in.Nonce, cal.MeasuredRatio)
	plan.Shape, plan.Preflight, plan.Calibration = shape, pf, cal
	cal.SharedTokens = plan.SharedTokens
	if plan.SharedTokens == 0 {
		return plan, nil
	}
	// 写缓存：失败不致命，命中与否由各档实测命中率评判
	for i := range cacheWarmupRequests {
		p := protocol.Prompt{Shared: plan.Shared, Unique: uniquePrompt(in.Nonce, stageCacheWrite, i, paragraphQuestion)}
		o, err := in.prepRequest(ctx, reqSpec{Stage: stageCacheWrite, Seq: i, MaxTokens: prepMaxTokens}, prepIndexCache, p, shape)
		if err != nil {
			return Plan{}, err
		}
		step()
		if o.Usage.CachedOk {
			cached := int(o.Usage.Cached)
			cal.CacheWarmCached = &cached
		}
	}
	return plan, nil
}

// calibrate 定标：发一条 calibChars 字符的填充文本，比例 = 填充字符 / (prompt_tokens − 标记与问题的名义开销)。
// 测不成（上游不报 usage、请求失败、计量异常）不中止：按名义比塑形照跑，报告标「未定标」并写明原因。
func (in RunInput) calibrate(ctx context.Context) (*Calibration, error) {
	tag := uniquePrompt(in.Nonce, stageCalib, 0, "")
	tail := paragraphBreak + paragraphQuestion
	p := protocol.Prompt{Unique: tag + fillerText(in.Nonce+"|"+stageCalib, calibChars) + tail}
	o, err := in.prepRequest(ctx, reqSpec{Stage: stageCalib, MaxTokens: prepMaxTokens}, prepIndexCalib, p, in.Plan.Shape)
	if err != nil {
		return nil, err
	}
	nominal := &Calibration{Chars: calibChars, NominalRatio: nominalCharsPerToken, MeasuredRatio: nominalCharsPerToken, Uncalibrated: true}
	switch {
	case !o.Usage.Ok && o.HasBody:
		nominal.Reason = "上游响应不带 usage（prompt_tokens），测不了字符/token 比"
		return nominal, nil
	case !o.Usage.Ok:
		nominal.Reason = "定标请求失败：" + o.Error
		return nominal, nil
	}
	tokens := o.Usage.Prompt - int64(nominalTokens(tag+tail))
	if tokens <= 0 {
		nominal.PromptTokens = int(o.Usage.Prompt)
		nominal.Reason = fmt.Sprintf("上游回报 prompt_tokens=%d，小于标记与问题本身，计量异常", o.Usage.Prompt)
		return nominal, nil
	}
	ratio := float64(calibChars) / float64(tokens)
	return &Calibration{
		Chars:         calibChars,
		PromptTokens:  int(o.Usage.Prompt),
		NominalRatio:  nominalCharsPerToken,
		MeasuredRatio: math.Round(ratio*1000) / 1000,
		DeviationPct:  math.Round((ratio/nominalCharsPerToken-1)*10000) / 100,
	}, nil
}

// prepRequest 按给定形态发一条准备期请求：过硬闸预扣 → 发送 → 落 warmup 样本。
// 传输/限流/5xx/断流这类瞬时失败按退避重试，至多 prepAttempts 次；返回最后一次的观测。
// error 只在 ctx 取消、样本落库失败、被全局硬闸拦下时返回。
func (in RunInput) prepRequest(ctx context.Context, sp reqSpec, stageIndex int, p protocol.Prompt, shape protocol.Shape) (outcome, error) {
	est := int64(nominalTokens(p.Text()) + sp.MaxTokens)
	var o outcome
	for attempt := 1; ; attempt++ {
		if !in.Caps.Reserve(est) {
			return o, fmt.Errorf("准备期请求被全局硬闸（%s）拦下，请调大上限", in.Caps.Reason())
		}
		at := time.Now()
		o = in.send(ctx, sp, p, shape, est)
		if err := ctx.Err(); err != nil {
			return o, err
		}
		if in.Sample != nil {
			if err := in.Sample(ctx, sampleFrom(sp, stageIndex, true, at, in.Codec.ID(), o)); err != nil {
				return o, fmt.Errorf("样本落库失败: %w", err)
			}
		}
		if o.Usage.Ok || !transient(o.ErrorClass) || attempt == prepAttempts {
			return o, nil
		}
		if err := probe.SleepUntil(ctx, time.Now().Add(probe.RetryDelay(attempt, o.Header))); err != nil {
			return o, err
		}
	}
}

// transient 值得重试的瞬时失败
func transient(class string) bool {
	switch class {
	case ErrTransport, ErrRateLimited, ErrHTTP5xx, ErrStreamAnomaly:
		return true
	}
	return false
}

// fillerText 确定性填充文本：沿用 sha256 链生成伪随机字节的思路，但映射成「常见英文词 + 空格 + 标点」，
// 每句 6-17 词、偶有逗号，每段 3-7 句、段间空行——像自然文本而不是整块 base64（后者的 token 比极不稳定）。
// 同种子输出是同一条确定性流的前缀：长度 n 精确截断。
func fillerText(seed string, n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(n + 128)
	r := byteStream{seed: seed}
	for b.Len() < n {
		if b.Len() > 0 {
			b.WriteString(paragraphBreak)
		}
		writeParagraph(&b, &r)
	}
	return b.String()[:n]
}

func writeParagraph(b *strings.Builder, r *byteStream) {
	sentences := 3 + int(r.next()%5)
	for i := range sentences {
		if i > 0 {
			b.WriteByte(' ')
		}
		words := 6 + int(r.next()%12)
		for j := range words {
			w := fillerWords[r.next()]
			if j == 0 {
				b.WriteString(strings.ToUpper(w[:1]))
				w = w[1:]
			} else {
				b.WriteByte(' ')
			}
			b.WriteString(w)
			if j < words-1 && r.next()%10 == 0 {
				b.WriteByte(',')
			}
		}
		b.WriteByte('.')
	}
}

// byteStream sha256(seed|计数器) 串成的确定性字节流
type byteStream struct {
	seed string
	buf  []byte
	ctr  uint64
}

func (s *byteStream) next() byte {
	if len(s.buf) == 0 {
		h := sha256.Sum256([]byte(s.seed + "|" + strconv.FormatUint(s.ctr, 10)))
		s.ctr++
		s.buf = h[:]
	}
	c := s.buf[0]
	s.buf = s.buf[1:]
	return c
}

// fillerWords 256 个常见英文词：一个随机字节直接选一个词
var fillerWords = [...]string{
	"the", "of", "and", "to", "in", "is", "was", "that", "for", "on", "with", "as", "by", "at", "from", "this",
	"are", "be", "have", "not", "but", "they", "which", "one", "all", "were", "can", "there", "been", "has", "more", "when",
	"will", "would", "their", "what", "about", "some", "into", "time", "only", "other", "could", "these", "two", "may", "then", "first",
	"any", "like", "over", "such", "our", "also", "new", "most", "after", "many", "before", "must", "through", "where", "much", "should",
	"well", "people", "between", "under", "never", "same", "another", "know", "while", "last", "might", "great", "old", "year", "off", "come",
	"since", "against", "came", "right", "used", "take", "three", "small", "large", "place", "world", "house", "water", "light", "river", "city",
	"morning", "evening", "garden", "window", "table", "letter", "market", "village", "mountain", "forest", "valley", "harbor", "bridge", "station", "school", "library",
	"teacher", "student", "farmer", "doctor", "writer", "sailor", "painter", "builder", "traveler", "neighbor", "friend", "family", "child", "mother", "father", "brother",
	"sister", "story", "history", "music", "picture", "journey", "season", "summer", "winter", "spring", "autumn", "weather", "cloud", "rain", "wind", "snow",
	"road", "path", "field", "stone", "wall", "door", "room", "kitchen", "bread", "coffee", "paper", "book", "page", "word", "line", "number",
	"walked", "talked", "opened", "closed", "carried", "watched", "listened", "noticed", "remembered", "decided", "answered", "returned", "finished", "started", "followed", "painted",
	"quiet", "bright", "warm", "cold", "early", "late", "slow", "quick", "simple", "careful", "gentle", "narrow", "wide", "long", "short", "heavy",
	"always", "often", "slowly", "quietly", "nearly", "almost", "together", "outside", "inside", "again", "later", "today", "still", "perhaps", "usually", "finally",
	"across", "along", "around", "behind", "beside", "beyond", "toward", "during", "without", "within", "above", "below", "near", "upon", "among", "until",
	"because", "although", "though", "unless", "whether", "however", "therefore", "meanwhile", "instead", "otherwise", "every", "each", "both", "either", "several", "few",
	"make", "made", "find", "found", "keep", "kept", "give", "gave", "bring", "brought", "leave", "left", "hold", "held", "turn", "built",
}

// 编译期断言词表恰好 256 个（随机字节直接作下标）
var (
	_ [len(fillerWords) - 256]struct{}
	_ [256 - len(fillerWords)]struct{}
)
