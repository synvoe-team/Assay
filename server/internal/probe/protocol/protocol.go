// Package protocol 三协议（openai_chat / openai_responses / anthropic_messages）的
// 请求构造 + 流式解析共享包。稳定性压测需要逐帧扫 SSE 打 TTFT 点、提尾帧 usage，
// 质量检测现有 runner 不具备此能力；此包一处收口三协议差异，稳定性 probe 只按接口取用。
// 为搁置的质量三协议批次预留扩展点（后续加 tool_call 相关方法）。
package protocol

import (
	"io"
	"net/http"
	"strings"
)

// Usage 一次响应的 token 计量（TPM 加权与实际吞吐计算用）。
// Ok=false 表示该响应未携带 usage —— 是「缺失」而非「0」，评估时区别对待。
type Usage struct {
	Prompt     int64 // 总输入 token，三协议同口径：含命中缓存的部分（anthropic 解析时已加回缓存读写）
	Completion int64
	Cached     int64 // 输入中命中缓存的 token 数
	CachedOk   bool  // 上游报了缓存字段（哪怕是 0）；false = 不知道命中多少，不能当 0 命中
	Reasoning  int64 // 输出中的推理 token（隐藏推理的模型只有这一个信号；渠道不报为 0）
	Ok         bool
}

// Hooks 流式扫描的打点回调（均可为 nil，各自至多触发一次）。
type Hooks struct {
	// OnFirstDelta 首个非空增量到达（推理或正文皆算）：推理模型「开始干活」的时刻
	OnFirstDelta func()
	// OnFirstContent 首个非空正文增量到达（TTFT）：推理/thinking 增量不触发
	OnFirstContent func()
}

// Result 一次流式响应的扫描结论。
type Result struct {
	Usage
	Completed    bool // 见到协议结束帧（流正常走完）；false = 流在中途被掐断
	HitMaxTokens bool // 结束原因是生成上限（chat length / responses max_output_tokens / anthropic max_tokens）
	SawReasoning bool // 出现过非空推理增量
	SawContent   bool // 出现过非空正文增量
}

// tracker 收口三协议共用的增量记账：置位 Saw* 并保证两个回调各只触发一次。
// 正文开头的 <think>…</think> 按推理计：不少中转与自部署（未开 reasoning parser 的 vLLM/SGLang）把思考
// 原样塞进正文，不拆出来 TTFT 就成了「首个思考 token」。只认正文开头（容许前导空白）的标签，标签可被任意切开。
// 「首个非空增量 / 正文」都不含纯空白增量（推理结束后常先吐一个 "\n\n"）。
type tracker struct {
	h       Hooks
	res     *Result
	delta   bool      // 已触发 OnFirstDelta
	mode    thinkMode // 正文开头的 <think> 判定状态
	pending string    // 未定夺的正文开头，或思考段里可能是半个 </think> 的尾巴
}

type thinkMode int

const (
	headUndecided thinkMode = iota // 还没见到非空白正文：攒着判断是不是 <think> 开头
	insideThink                    // 在 <think> 段里：增量按推理计
	plainText                      // 普通正文
)

const (
	openThink  = "<think>"
	closeThink = "</think>"
	blanks     = " \t\r\n"
)

func (t *tracker) reasoning(s string) {
	if strings.TrimLeft(s, blanks) == "" {
		return
	}
	t.firstDelta()
	t.res.SawReasoning = true
}

// content 正文增量。纯空白不触发首增量：否则普通模型先吐一个 "\n" 再吐正文，TTFD<TTFT 会被误读成「有思考」
func (t *tracker) content(s string) {
	if s == "" {
		return
	}
	if strings.TrimLeft(s, blanks) != "" {
		t.firstDelta()
	}
	switch t.mode {
	case plainText:
		t.text(s)
	case insideThink:
		t.think(s)
	default:
		t.pending += s
		head := strings.TrimLeft(t.pending, blanks)
		switch {
		case strings.HasPrefix(head, openThink):
			t.mode, t.pending = insideThink, ""
			t.think(head[len(openThink):])
		case strings.HasPrefix(openThink, head): // 纯空白或半个开标签：再等
		default:
			t.mode, t.pending = plainText, ""
			t.text(head)
		}
	}
}

// think <think> 段内的增量：遇闭合标签后回到「开头未定夺」（跳过闭合后的空白），其余按推理计；
// 末尾可能是半个 </think> 的部分留到下个增量再判
func (t *tracker) think(s string) {
	buf := t.pending + s
	if i := strings.Index(buf, closeThink); i >= 0 {
		t.markReasoning(buf[:i])
		t.mode, t.pending = headUndecided, ""
		t.content(buf[i+len(closeThink):])
		return
	}
	keep := partialSuffix(buf, closeThink)
	t.markReasoning(buf[:len(buf)-keep])
	t.pending = buf[len(buf)-keep:]
}

func (t *tracker) markReasoning(s string) {
	if strings.TrimLeft(s, blanks) != "" {
		t.res.SawReasoning = true
	}
}

func (t *tracker) text(s string) {
	if !t.res.SawContent {
		t.res.SawContent = true
		if t.h.OnFirstContent != nil {
			t.h.OnFirstContent()
		}
	}
}

// finish 流结束：没定夺完的开头若有非空白内容（如整条回复就是「<thi」）按正文算；没闭合的思考段只算推理
func (t *tracker) finish() {
	if t.mode == headUndecided && strings.TrimLeft(t.pending, blanks) != "" {
		t.text(t.pending)
	}
	t.pending = ""
}

func (t *tracker) firstDelta() {
	if !t.delta {
		t.delta = true
		if t.h.OnFirstDelta != nil {
			t.h.OnFirstDelta()
		}
	}
}

// partialSuffix s 的最长「是 tag 真前缀」的后缀长度（标签被切在两个增量之间时留着等下一段）
func partialSuffix(s, tag string) int {
	for n := min(len(s), len(tag)-1); n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return n
		}
	}
	return 0
}

// Codec 单一协议的请求构造 + 流式解析。
type Codec interface {
	// ID 协议标识，与 api.Protocol / 渠道 protocols 声明一致
	ID() string
	// Path base_url（填到版本段，如 …/v1）之后拼接的固定末段路径
	Path() string
	// Auth 按协议标准写认证头（Bearer / x-api-key + anthropic-version）
	Auth(req *http.Request, apiKey string)
	// LoadBody 构造压测请求体：stream 恒 true（TTFT 需流式逐帧）；共享前缀按协议的缓存机制摆放，
	// 兼容形态（关思考写法 / 自定义字段等）只并进顶层键。返回裸字节直接发送，prompt 字节绝不改写。
	LoadBody(l Load) ([]byte, error)
	// ScanStream 扫描 SSE 流：首个非空增量 / 首个非空正文增量到达时分别回调 h 的两个钩子，
	// 并给出 usage、是否见到结束帧、是否因生成上限结束、是否出现推理/正文增量。
	// 分片解析失败或流内错误事件（上游在 200 之后于流里报错）返回 err（判 stream_anomaly）。
	ScanStream(r io.Reader, h Hooks) (Result, error)
	// ParseBody 上游无视 stream=true 回了整块非流式 JSON 时的兜底解析：正文/推理/usage 照取，
	// Completed 恒 true（整块即完整响应）；测不到首字时序，由调用方标注。
	ParseBody(raw []byte) (Result, error)
}

// Prompt 压测 prompt 的结构化形态。Shared 为任务内所有请求字节相同的共享前缀（缓存命中段，可空），
// Unique 为每请求唯一段（唯一标记 + 填充 + 问题）。Shared 为空时与旧的单串 prompt 逐字节一致。
type Prompt struct {
	Shared string
	Unique string
}

// Text 拼成单串：自动前缀缓存的协议（openai_chat / openai_responses）共享前缀直接拼在最前面
func (p Prompt) Text() string { return p.Shared + p.Unique }

// Load 一次压测请求的构造参数
type Load struct {
	Model     string
	Prompt    Prompt
	MaxTokens int   // 生成上限（Shape.MaxTokensCap 生效时再钳一次）
	Shape     Shape // 兼容形态
}

// maxTokens 实发生成上限：预检读到模型允许的最大值后钳住
func (l Load) maxTokens() int {
	if c := l.Shape.MaxTokensCap; c > 0 && l.MaxTokens > c {
		return c
	}
	return l.MaxTokens
}

// textBlock 文本内容块；CacheControl 标出缓存断点（断点之前的内容被缓存）。anthropic 与 chat 显式缓存共用
type textBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"`
}

// splitContent 共享前缀单独成块并打断点、唯一段另起一块；无共享前缀时保持纯文本串
func splitContent(p Prompt) any {
	if p.Shared == "" {
		return p.Unique
	}
	return []textBlock{
		{Type: "text", Text: p.Shared, CacheControl: &cacheControl{Type: "ephemeral"}},
		{Type: "text", Text: p.Unique},
	}
}

// codecs 协议注册表，由各实现的 init() 填充
var codecs = map[string]Codec{}

func register(c Codec) {
	if _, dup := codecs[c.ID()]; dup {
		panic("protocol: 重复注册 " + c.ID())
	}
	codecs[c.ID()] = c
}

// Get 按 ID 取 codec；空 ID 兜底 openai_chat（最通用协议）。
func Get(id string) (Codec, bool) {
	if id == "" {
		id = ProtocolOpenAIChat
	}
	c, ok := codecs[id]
	return c, ok
}

// All 返回全部已注册协议 ID（无序，供测试枚举）。
func All() []string {
	ids := make([]string, 0, len(codecs))
	for id := range codecs {
		ids = append(ids, id)
	}
	return ids
}
