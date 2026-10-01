package stability

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Yukiho0287/assay/server/internal/probe/protocol"
)

// 稳定性检测以「跑通」为第一要务：要测的是渠道稳不稳，不是它支持哪些参数（那是质量检测的事）。
// 几百个渠道各有各的不按规范来——不认识的字段有的 400、有的静默忽略，关思考的写法五花八门，
// 有的连关都关不掉。所以任务开头先发预检：按上游的实际反应把请求形态（protocol.Shape）定下来，
// 能兼容的都兼容，兼容不了的降级照跑，并把做过的调整写进报告；只有认证/模型/路径这类「根本连不上」
// 的错误才中止任务。

// Compat 请求兼容选项（三个 probe 共用）。默认全自动：预检按上游实际反应调整，也可手动指定。
type Compat struct {
	MaxTokensField   string                     `json:"maxTokensField,omitempty"`   // auto | max_tokens | max_completion_tokens（仅 openai_chat）
	StreamUsage      string                     `json:"streamUsage,omitempty"`      // auto | on | off（仅 openai_chat 的 stream_options.include_usage）
	ChatCacheControl bool                       `json:"chatCacheControl,omitempty"` // openai_chat 也给共享前缀打 cache_control 断点
	ExtraBody        map[string]json.RawMessage `json:"extraBody,omitempty"`        // 并进请求体顶层的自定义字段（同名覆盖）
	ExtraHeaders     map[string]string          `json:"extraHeaders,omitempty"`     // 追加的请求头（同名覆盖）
}

// 兼容选项取值
const (
	CompatAuto              = "auto"
	FieldMaxTokens          = "max_tokens"
	FieldMaxCompletionToken = "max_completion_tokens"
	StreamUsageOn           = "on"
	StreamUsageOff          = "off"
)

// 思考控制（Workload.Thinking）：default 不干预；auto 自动探测关思考写法；其余为 protocol 包里的具体写法 ID
const (
	ThinkingDefault = "default"
	ThinkingAuto    = "auto"
)

const (
	stagePreflight = "preflight"
	stageThinking  = "thinking"
	// 准备期各段的 stage_index（样本表按它排序）
	prepIndexPreflight = 0
	prepIndexThinking  = 1
	prepIndexCalib     = 2
	prepIndexCache     = 3

	// preflightQuestion 预检问题：会思考的模型几乎必想一下，不思考的几个 token 就答完
	preflightShared   = "Answer the arithmetic question below.\n\n"
	preflightQuestion = "计算 4837 × 2963 等于多少？只回答数字。"
	// thinkingTrialMaxTokens 试关思考写法时的生成上限：关掉了几个 token 就答完，没关掉的会把它想满
	thinkingTrialMaxTokens = 256
	// prepMaxTokens 定标/写缓存的生成上限：只要 usage，不要答案（不用 1：部分上游有下限）
	prepMaxTokens = 16
	// maxAdjustments 预检自动调整的总次数上限（防止与上游来回拉锯）
	maxAdjustments = 8

	maxExtraFields  = 32
	maxExtraBodyLen = 16 * 1024
)

// reservedBodyKeys 自定义字段不得覆盖的键：模型、prompt、流式与生成上限归负载画像管
var reservedBodyKeys = []string{"model", "messages", "input", "stream", "max_tokens", "max_completion_tokens", "max_output_tokens"}

// reservedHeaders 自定义请求头不得覆盖的传输层头
var reservedHeaders = []string{"host", "content-length", "content-type", "transfer-encoding", "connection"}

var headerName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func (c *Compat) applyDefaults() {
	if c.MaxTokensField == "" {
		c.MaxTokensField = CompatAuto
	}
	if c.StreamUsage == "" {
		c.StreamUsage = CompatAuto
	}
}

func (c Compat) validate() error {
	if !slices.Contains([]string{CompatAuto, FieldMaxTokens, FieldMaxCompletionToken}, c.MaxTokensField) {
		return fmt.Errorf("生成上限字段需 auto / max_tokens / max_completion_tokens，收到 %q", c.MaxTokensField)
	}
	if !slices.Contains([]string{CompatAuto, StreamUsageOn, StreamUsageOff}, c.StreamUsage) {
		return fmt.Errorf("流式 usage 需 auto / on / off，收到 %q", c.StreamUsage)
	}
	if len(c.ExtraBody) > maxExtraFields || len(c.ExtraHeaders) > maxExtraFields {
		return fmt.Errorf("自定义请求体字段 / 请求头各至多 %d 个", maxExtraFields)
	}
	size := 0
	for k, v := range c.ExtraBody {
		if slices.Contains(reservedBodyKeys, k) {
			return fmt.Errorf("自定义请求体字段不能覆盖 %q（模型、prompt、流式与生成上限由负载画像控制）", k)
		}
		if !json.Valid(v) {
			return fmt.Errorf("自定义请求体字段 %q 的值不是合法 JSON", k)
		}
		size += len(k) + len(v)
	}
	if size > maxExtraBodyLen {
		return fmt.Errorf("自定义请求体字段合计超过 %d 字节", maxExtraBodyLen)
	}
	for k, v := range c.ExtraHeaders {
		if !headerName.MatchString(k) {
			return fmt.Errorf("请求头名 %q 不合法", k)
		}
		if slices.Contains(reservedHeaders, strings.ToLower(k)) {
			return fmt.Errorf("请求头 %q 由传输层管理，不能自定义", k)
		}
		if strings.ContainsAny(v, "\r\n\x00") {
			return fmt.Errorf("请求头 %q 的值不能含换行或空字符", k)
		}
	}
	return nil
}

// validateThinking 思考控制取值：default / auto / 适用于本协议的具体写法
func validateThinking(mode, proto string) error {
	switch {
	case mode == ThinkingDefault || mode == ThinkingAuto:
		return nil
	case protocol.ThinkingApplies(mode, proto):
		return nil
	case protocol.ThinkingLabel(mode, protocol.ProtocolOpenAIChat) != "" || protocol.ThinkingLabel(mode, protocol.ProtocolAnthropicMessages) != "":
		return fmt.Errorf("关思考写法 %q 不适用于 %s 协议", mode, proto)
	}
	return fmt.Errorf("未知的思考控制 %q", mode)
}

// initialShape 用户手动指定的部分先落进形态；auto 的项由预检按上游反应再调
func initialShape(p StabilityParams) protocol.Shape {
	c := p.Compat
	s := protocol.Shape{
		MaxCompletionTokens: c.MaxTokensField == FieldMaxCompletionToken,
		NoStreamUsage:       c.StreamUsage == StreamUsageOff,
		ChatCacheControl:    c.ChatCacheControl,
		ExtraBody:           c.ExtraBody,
		ExtraHeaders:        c.ExtraHeaders,
	}
	if m := p.Workload.Thinking; m != ThinkingDefault && m != ThinkingAuto {
		s.Thinking = m
	}
	return s
}

// Preflight 预检结论，写进各 probe 的 __overall__ 指标（报告展示「预检做了哪些兼容」）
type Preflight struct {
	Requests      int             `json:"requests"`                // 预检共发请求数（含自适应重发与关思考试探）
	Passed        bool            `json:"passed"`                  // 预检请求拿到了 HTTP 200
	Detail        string          `json:"detail,omitempty"`        // 没通过时最后一次的错误（限流/5xx/断流，照常往下跑）
	UsageReported bool            `json:"usageReported"`           // 上游响应带不带 usage
	NonStream     bool            `json:"nonStream,omitempty"`     // 上游无视 stream=true 回整块 JSON（TTFT 测不到）
	Reasoning     bool            `json:"reasoning"`               // 预检请求里模型有没有思考
	Thinking      string          `json:"thinking,omitempty"`      // 最终采用的关思考写法；空 = 不发
	ThinkingField string          `json:"thinkingField,omitempty"` // 该写法实发的字段原文
	Trials        []ThinkingTrial `json:"trials,omitempty"`        // 自动探测逐个试的结果
	Adjustments   []string        `json:"adjustments,omitempty"`   // 按上游报错做的兼容调整（人话）
}

// ThinkingTrial 自动探测里一种写法的试探结果
type ThinkingTrial struct {
	Variant string `json:"variant"`
	Field   string `json:"field"`            // 实发字段原文
	Outcome string `json:"outcome"`          // disabled | still_reasoning | rejected | failed
	Detail  string `json:"detail,omitempty"` // rejected/failed 时的上游报错
}

// 试探结果
const (
	TrialDisabled       = "disabled"        // 关掉了：采用
	TrialStillReasoning = "still_reasoning" // 上游收下了但照样思考（静默忽略或模型不支持关）
	TrialRejected       = "rejected"        // 上游 4xx 拒收该字段
	TrialFailed         = "failed"          // 限流/5xx/断流等，没法判断
)

// preflight 预检：先按当前形态发一条（4xx 时按报错自适应调整再发），再视需要自动探测关思考写法。
// 返回定下来的请求形态；只有调整穷尽后仍被 4xx 拒（渠道配置错误，跑了也是满屏 4xx）才返回 error。
func (in RunInput) preflight(ctx context.Context, step func()) (protocol.Shape, *Preflight, error) {
	shape := initialShape(in.Params)
	pf := &Preflight{}
	o, err := in.negotiate(ctx, &shape, pf)
	step()
	if err != nil {
		return shape, pf, err
	}
	pf.UsageReported = o.Usage.Ok
	pf.NonStream = o.NonStream
	pf.Reasoning = o.Reasoned || o.ErrorClass == ErrBudgetExhausted
	mode := in.Params.Workload.Thinking
	// 指定写法被上游拒收（预检已剔除）而模型会思考：退回自动探测其余写法——用户要的是「关掉思考」，写法只是手段。
	// 进度预估没算这部分试探，不计步
	fallback := mode != ThinkingAuto && mode != ThinkingDefault && shape.Thinking == ""
	if pf.Reasoning && (mode == ThinkingAuto || fallback) {
		probeStep := step
		if fallback {
			probeStep = func() {}
		}
		if err := in.probeThinking(ctx, &shape, pf, probeStep, mode); err != nil {
			return shape, pf, err
		}
	}
	pf.Thinking = shape.Thinking
	pf.ThinkingField = protocol.ThinkingLabel(shape.Thinking, in.Codec.ID())
	return shape, pf, nil
}

// preflightMaxTokens 预检用本任务会发的最大生成上限：上游对上限有约束（如 ≤8192）时在这里就暴露出来
func preflightMaxTokens(w Workload) int {
	if w.Output > 0 {
		return w.Output
	}
	return max(ladderOutput.MaxTokens, rpmOutput.MaxTokens, tpmOutput.MaxTokens)
}

// negotiate 发预检请求，4xx 时按上游报错调整形态再发，直到 HTTP 200 或无可再调。
// 限流/5xx/断流不中止（稳定性检测本身就要测这些），照当前形态往下跑。
func (in RunInput) negotiate(ctx context.Context, shape *protocol.Shape, pf *Preflight) (outcome, error) {
	maxTokens := preflightMaxTokens(in.Params.Workload)
	var strips []blindStrip
	for {
		o, err := in.preflightRequest(ctx, *shape, pf, maxTokens)
		if err != nil {
			return o, err
		}
		if o.HasBody {
			pf.Passed = true
			if len(strips) > 1 {
				return in.restoreStrips(ctx, shape, pf, strips[:len(strips)-1], maxTokens, o)
			}
			return o, nil
		}
		if o.ErrorClass != ErrHTTP4xx {
			pf.Detail = o.Error
			return o, nil
		}
		if len(pf.Adjustments) >= maxAdjustments {
			return o, rejectedErr(o, pf)
		}
		if note, ok := adapt(shape, in.Params.Compat, in.Codec.ID(), o, maxTokens); ok {
			pf.Adjustments = append(pf.Adjustments, note)
			continue
		}
		if o.HTTPStatus == 401 || o.HTTPStatus == 403 || o.HTTPStatus == 404 {
			return o, rejectedErr(o, pf) // 认证 / 模型 / 路径问题，跟请求字段无关，剔字段也没用
		}
		st, ok := stripUnnamed(shape, in.Params.Compat, in.Codec.ID(), in.Params.Workload.CacheHitRate > 0)
		if !ok {
			return o, rejectedErr(o, pf)
		}
		st.note = len(pf.Adjustments)
		strips = append(strips, st)
		pf.Adjustments = append(pf.Adjustments, "报错未点名字段，去掉"+st.what)
	}
}

// preflightRequest 按给定形态发一条预检请求（每条带不同唯一前缀）。
// 本任务带共享前缀时预检也带一小段，让内容块 / cache_control 这种形态在预检里就暴露出来（太短不会真被缓存）
func (in RunInput) preflightRequest(ctx context.Context, shape protocol.Shape, pf *Preflight, maxTokens int) (outcome, error) {
	seq := pf.Requests
	sp := reqSpec{Stage: stagePreflight, Seq: seq, MaxTokens: maxTokens}
	p := protocol.Prompt{Unique: uniquePrompt(in.Nonce, stagePreflight, seq, preflightQuestion)}
	if in.Params.Workload.CacheHitRate > 0 {
		p.Shared = preflightShared
	}
	o, err := in.prepRequest(ctx, sp, prepIndexPreflight, p, shape)
	if err == nil {
		pf.Requests++
	}
	return o, err
}

// rejectedErr 调整穷尽后仍被 4xx 拒：渠道配置错误，跑了也是满屏 4xx，中止任务并带上游原文
func rejectedErr(o outcome, pf *Preflight) error {
	tried := "未做任何调整"
	if len(pf.Adjustments) > 0 {
		tried = "已自动调整：" + strings.Join(pf.Adjustments, "；")
	}
	return fmt.Errorf("预检请求被上游拒绝（%s），%s。多半是渠道 key / 模型名 / 协议配错了，或上游要求的参数需在「兼容选项」里手动设置", o.Error, tried)
}

// adapt 按上游 4xx 报错里点了名的字段针对性调整一处形态，返回人话说明；没点名返回 false（交给 stripUnnamed）。
// 只动「自动」档或可选的部分：用户手动定死的生成上限字段 / 流式 usage 不改。
func adapt(s *protocol.Shape, c Compat, proto string, o outcome, maxTokens int) (string, bool) {
	msg := strings.ToLower(o.Error)
	chat := proto == protocol.ProtocolOpenAIChat
	switch {
	case proto == protocol.ProtocolAnthropicMessages && (o.HTTPStatus == 401 || o.HTTPStatus == 403) && !s.BearerAuth:
		s.BearerAuth = true
		return "x-api-key 认证被拒，改用 Authorization: Bearer", true
	case o.HTTPStatus == 401 || o.HTTPStatus == 403 || o.HTTPStatus == 404:
		return "", false
	case chat && c.MaxTokensField == CompatAuto && !s.MaxCompletionTokens && strings.Contains(msg, "max_completion_tokens"):
		s.MaxCompletionTokens = true
		return "上游要求 max_completion_tokens，生成上限改用该字段", true
	case s.MaxTokensCap == 0 && tokenLimit(msg, maxTokens) > 0:
		s.MaxTokensCap = tokenLimit(msg, maxTokens)
		return fmt.Sprintf("生成上限超过模型允许的 %d，已钳到 %d", s.MaxTokensCap, s.MaxTokensCap), true
	case chat && c.StreamUsage == CompatAuto && !s.NoStreamUsage && strings.Contains(msg, "stream_options"):
		s.NoStreamUsage = true
		return "上游不认 stream_options，已去掉（可能拿不到 usage）", true
	case s.ChatCacheControl && strings.Contains(msg, "cache_control"):
		s.ChatCacheControl = false
		return "上游不认 cache_control 内容块，改回纯文本（走自动前缀缓存）", true
	case proto == protocol.ProtocolAnthropicMessages && !s.PlainContent && strings.Contains(msg, "cache_control"):
		s.PlainContent = true
		return "上游不认 cache_control 内容块，共享前缀改拼成纯文本（缓存只能靠上游自动前缀缓存）", true
	}
	if s.Thinking != "" && mentions(msg, protocol.ThinkingFieldNames(s.Thinking, proto)) {
		return dropThinking(s, proto), true
	}
	for k := range s.ExtraBody {
		if strings.Contains(msg, strings.ToLower(k)) {
			s.ExtraBody = without(s.ExtraBody, k)
			return fmt.Sprintf("上游不认自定义字段 %s，已剔除", k), true
		}
	}
	return "", false
}

// blindStrip 报错没点名时剔掉的一项可选部分；restore 把它装回去（回补验证用）
type blindStrip struct {
	what    string // 人话：剔掉的是什么
	note    int    // 对应调整说明在 Adjustments 里的下标
	restore func(*protocol.Shape)
}

// stripUnnamed 报错没点名：按「关思考参数 → 自定义字段 → stream_options → 显式缓存断点」剔掉下一项可选部分。
// cached = 本任务带共享前缀（预检请求也带，内容块形态才会被测到）
func stripUnnamed(s *protocol.Shape, c Compat, proto string, cached bool) (blindStrip, bool) {
	switch {
	case s.Thinking != "":
		v := s.Thinking
		s.Thinking = ""
		return blindStrip{what: "关思考参数 " + protocol.ThinkingLabel(v, proto), restore: func(t *protocol.Shape) { t.Thinking = v }}, true
	case len(s.ExtraBody) > 0:
		v := s.ExtraBody
		s.ExtraBody = nil
		return blindStrip{what: "全部自定义请求体字段", restore: func(t *protocol.Shape) { t.ExtraBody = v }}, true
	case proto == protocol.ProtocolOpenAIChat && c.StreamUsage == CompatAuto && !s.NoStreamUsage:
		s.NoStreamUsage = true
		return blindStrip{what: "stream_options（可能拿不到 usage）", restore: func(t *protocol.Shape) { t.NoStreamUsage = false }}, true
	case cached && s.ChatCacheControl:
		s.ChatCacheControl = false
		return blindStrip{what: "cache_control 内容块", restore: func(t *protocol.Shape) { t.ChatCacheControl = true }}, true
	case proto == protocol.ProtocolAnthropicMessages && cached && !s.PlainContent:
		s.PlainContent = true
		return blindStrip{what: "cache_control 内容块（共享前缀改拼成纯文本）", restore: func(t *protocol.Shape) { t.PlainContent = false }}, true
	}
	return blindStrip{}, false
}

// restoreStrips 盲剔通过后逐项回补：报错没点名时只能按顺序剔，最后剔的那项才确定是元凶，
// 前面剔掉的未必有问题。逐个装回去单独再发一条，上游收下就保留（别白丢 usage / 关思考），
// 并从调整说明里撤掉它；回补请求被拒（含限流/5xx 没法判断）则维持剔除。
func (in RunInput) restoreStrips(ctx context.Context, shape *protocol.Shape, pf *Preflight, strips []blindStrip, maxTokens int, o outcome) (outcome, error) {
	var kept []string
	restored := map[int]bool{}
	for _, st := range strips {
		trial := *shape
		st.restore(&trial)
		r, err := in.preflightRequest(ctx, trial, pf, maxTokens)
		if err != nil {
			return o, err
		}
		if r.HasBody {
			*shape, o = trial, r
			kept = append(kept, st.what)
			restored[st.note] = true
		}
	}
	if len(kept) == 0 {
		return o, nil
	}
	notes := make([]string, 0, len(pf.Adjustments))
	for i, n := range pf.Adjustments {
		if !restored[i] {
			notes = append(notes, n)
		}
	}
	pf.Adjustments = append(notes, "逐项回补验证："+strings.Join(kept, "、")+" 上游能收，已保留")
	return o, nil
}

func dropThinking(s *protocol.Shape, proto string) string {
	field := protocol.ThinkingLabel(s.Thinking, proto)
	s.Thinking = ""
	return fmt.Sprintf("上游不认关思考参数 %s，已剔除", field)
}

func mentions(msg string, names []string) bool {
	for _, n := range names {
		if strings.Contains(msg, strings.ToLower(n)) {
			return true
		}
	}
	return false
}

func without(m map[string]json.RawMessage, key string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		if k != key {
			out[k] = v
		}
	}
	return out
}

var numberRe = regexp.MustCompile(`\d+`)

// tokenLimit 从「生成上限超限」报错里读出模型允许的最大值：报错须提到 token 上限字段，
// 取其中小于当前上限的最大数（如 DeepSeek「valid range of max_tokens is [1, 8192]」、OpenAI「supports at most
// 16384 completion tokens」、Anthropic「64000 > 32000, which is the maximum」）；读不出返回 0。
// 「HTTP 400: 」前缀与常见状态码不算（避免把错误码当上限）。
func tokenLimit(msg string, current int) int {
	if !strings.Contains(msg, "token") || !strings.Contains(msg, "max") {
		return 0
	}
	if i := strings.Index(msg, ": "); strings.HasPrefix(msg, "http ") && i >= 0 {
		msg = msg[i+2:]
	}
	best := 0
	for _, s := range numberRe.FindAllString(msg, -1) {
		n, err := strconv.Atoi(s)
		if err != nil || n < 256 || n >= current || isStatusCode(n) {
			continue
		}
		best = max(best, n)
	}
	return best
}

func isStatusCode(n int) bool {
	return slices.Contains([]int{400, 401, 403, 404, 413, 422, 429, 500, 502, 503, 504}, n)
}

// probeThinking 自动关思考：按协议的候选顺序逐个试，第一个「上游收下且不再思考」的写法即采用。
// 全都关不掉就不发思考参数、按渠道默认测——很可能是该模型官方就不支持关思考，属服务端能力，不是配置问题。
func (in RunInput) probeThinking(ctx context.Context, shape *protocol.Shape, pf *Preflight, step func(), skip string) error {
	proto := in.Codec.ID()
	for i, id := range protocol.ThinkingCandidates(proto) {
		if id == skip {
			continue // 指定写法已被拒收，不再试
		}
		s := *shape
		s.Thinking = id
		sp := reqSpec{Stage: stageThinking, Seq: i, MaxTokens: thinkingTrialMaxTokens}
		p := protocol.Prompt{Unique: uniquePrompt(in.Nonce, stageThinking, i, preflightQuestion)}
		o, err := in.prepRequest(ctx, sp, prepIndexThinking, p, s)
		if err != nil {
			return err
		}
		step()
		pf.Requests++
		t := ThinkingTrial{Variant: id, Field: protocol.ThinkingLabel(id, proto), Outcome: trialOutcome(o)}
		if t.Outcome == TrialRejected || t.Outcome == TrialFailed {
			t.Detail = o.Error
		}
		pf.Trials = append(pf.Trials, t)
		if t.Outcome == TrialDisabled {
			shape.Thinking = id
			return nil
		}
	}
	return nil
}

func trialOutcome(o outcome) string {
	switch {
	case o.ErrorClass == ErrHTTP4xx:
		return TrialRejected
	case o.Reasoned || o.ErrorClass == ErrBudgetExhausted || o.ErrorClass == ErrReasoningOnly:
		return TrialStillReasoning
	case !o.Ok:
		return TrialFailed
	}
	return TrialDisabled
}
