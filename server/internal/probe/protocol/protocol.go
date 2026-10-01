// Package protocol 三协议（openai_chat / openai_responses / anthropic_messages）的
// 请求构造 + 流式解析共享包。稳定性压测需要逐帧扫 SSE 打 TTFT 点、提尾帧 usage，
// 质量检测现有 runner 不具备此能力；此包一处收口三协议差异，稳定性 probe 只按接口取用。
// 为搁置的质量三协议批次预留扩展点（后续加 tool_call 相关方法）。
package protocol

import (
	"io"
	"net/http"
)

// Usage 一次响应的 token 计量（TPM 加权与实际吞吐计算用）。
// Ok=false 表示该响应未携带 usage —— 是「缺失」而非「0」，评估时区别对待。
type Usage struct {
	Prompt     int64
	Completion int64
	Cached     int64 // 输入中命中缓存的 token 数（破缓存断言用；渠道不报则为 0）
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
type tracker struct {
	h   Hooks
	res *Result
}

func (t *tracker) reasoning(s string) {
	if s == "" {
		return
	}
	t.firstDelta()
	t.res.SawReasoning = true
}

func (t *tracker) content(s string) {
	if s == "" {
		return
	}
	t.firstDelta()
	if !t.res.SawContent {
		t.res.SawContent = true
		if t.h.OnFirstContent != nil {
			t.h.OnFirstContent()
		}
	}
}

// firstDelta 必须在置位 Saw* 之前调用：两者皆 false 即「此前从未见过增量」
func (t *tracker) firstDelta() {
	if !t.res.SawReasoning && !t.res.SawContent && t.h.OnFirstDelta != nil {
		t.h.OnFirstDelta()
	}
}

// Codec 单一协议的请求构造 + 流式解析。
type Codec interface {
	// ID 协议标识，与 api.Protocol / 渠道 protocols 声明一致
	ID() string
	// Path base_url（填到版本段，如 …/v1）之后拼接的固定末段路径
	Path() string
	// Auth 按协议标准写认证头（Bearer / x-api-key + anthropic-version）
	Auth(req *http.Request, apiKey string)
	// LoadBody 构造最小压测请求体：content 顶格 prompt，maxTokens 砝码，
	// stream 恒 true（TTFT 需流式逐帧）。返回裸字节直接发送，绝不二次序列化。
	LoadBody(model, content string, maxTokens int) ([]byte, error)
	// ScanStream 扫描 SSE 流：首个非空增量 / 首个非空正文增量到达时分别回调 h 的两个钩子，
	// 并给出 usage、是否见到结束帧、是否因生成上限结束、是否出现推理/正文增量。
	// 分片解析失败或流内错误事件（上游在 200 之后于流里报错）返回 err（判 stream_anomaly）。
	ScanStream(r io.Reader, h Hooks) (Result, error)
}

// message OpenAI chat 与 Anthropic messages 共用的消息体（role + 纯文本 content）
type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
