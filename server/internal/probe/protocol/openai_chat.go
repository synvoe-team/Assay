package protocol

import (
	"io"
	"net/http"

	"github.com/Yukiho0287/assay/server/internal/probe"
)

// ProtocolOpenAIChat OpenAI chat/completions 协议 ID
const ProtocolOpenAIChat = "openai_chat"

type openaiChat struct{}

func init() { register(openaiChat{}) }

// chatStreamOptions 显式要求流式尾帧带 usage（否则 chat 流不返回计量）
type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatBody struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	// 二选一：OpenAI 推理模型在 chat 上拒收 max_tokens、只认 max_completion_tokens
	MaxTokens           int                `json:"max_tokens,omitempty"`
	MaxCompletionTokens int                `json:"max_completion_tokens,omitempty"`
	Stream              bool               `json:"stream"`
	StreamOptions       *chatStreamOptions `json:"stream_options,omitempty"`
}

// chatMessage content 为纯文本串，或显式缓存时的内容块数组
type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func (openaiChat) ID() string   { return ProtocolOpenAIChat }
func (openaiChat) Path() string { return "/chat/completions" }

func (openaiChat) Auth(req *http.Request, apiKey string) {
	req.Header.Set("Authorization", "Bearer "+apiKey)
}

func (openaiChat) LoadBody(l Load) ([]byte, error) {
	s := l.Shape
	// 自动前缀缓存（OpenAI/DeepSeek 等）直接拼；要显式断点的上游拆成带 cache_control 的内容块
	var content any = l.Prompt.Text()
	if s.ChatCacheControl {
		content = splitContent(l.Prompt)
	}
	b := chatBody{
		Model:    l.Model,
		Messages: []chatMessage{{Role: "user", Content: content}},
		Stream:   true,
	}
	if s.MaxCompletionTokens {
		b.MaxCompletionTokens = l.maxTokens()
	} else {
		b.MaxTokens = l.maxTokens()
	}
	if !s.NoStreamUsage {
		b.StreamOptions = &chatStreamOptions{IncludeUsage: true}
	}
	body, err := probe.MarshalNoEscape(b)
	if err != nil {
		return nil, err
	}
	return applyShape(body, ProtocolOpenAIChat, s)
}

func (openaiChat) ScanStream(r io.Reader, h Hooks) (Result, error) {
	var res Result
	tr := tracker{h: h, res: &res}
	done, err := scanSSE(r, func(m map[string]any) error {
		// 部分 OpenAI 兼容上游在 200 之后于流里下发 {"error":{...}} / {"error":"…"} 分片
		if err := errorField(m); err != nil {
			return err
		}
		// 尾帧 usage：include_usage 生效时末帧 choices 为空、usage 非空
		if uv, ok := objField(m, "usage"); ok {
			chatUsage(uv, &res.Usage)
		}
		choices, _ := m["choices"].([]any)
		for _, ch := range choices {
			c, ok := ch.(map[string]any)
			if !ok {
				continue
			}
			if delta, ok := objField(c, "delta"); ok {
				chatDelta(delta, &tr)
			}
			// Moonshot 把 usage 放在末个 choice 里
			if uv, ok := objField(c, "usage"); ok {
				chatUsage(uv, &res.Usage)
			}
			chatFinish(c, &res)
		}
		return nil
	})
	tr.finish()
	if done {
		res.Completed = true
	}
	return res, err
}

// ParseBody 上游无视 stream=true 回了整块 JSON：按非流式响应解析（拿得到正文与 usage，测不到 TTFT）
func (openaiChat) ParseBody(raw []byte) (Result, error) {
	var res Result
	m, err := decodeObject(raw)
	if err != nil {
		return res, err
	}
	tr := tracker{res: &res}
	if uv, ok := objField(m, "usage"); ok {
		chatUsage(uv, &res.Usage)
	}
	choices, _ := m["choices"].([]any)
	for _, ch := range choices {
		c, ok := ch.(map[string]any)
		if !ok {
			continue
		}
		if msg, ok := objField(c, "message"); ok {
			chatDelta(msg, &tr)
		}
		chatFinish(c, &res)
	}
	tr.finish()
	res.Completed = true // 整块 JSON 即完整响应
	return res, nil
}

// chatDelta 一段增量（或非流式的整条 message）：推理字段各家不一——DeepSeek/Kimi/Qwen 用 reasoning_content，
// OpenRouter/vLLM 用 reasoning；正文开头的 <think> 段由 tracker 拆成推理
func chatDelta(d map[string]any, tr *tracker) {
	rc, _ := strField(d, "reasoning_content")
	tr.reasoning(rc)
	rs, _ := strField(d, "reasoning")
	tr.reasoning(rs)
	txt, _ := strField(d, "content")
	tr.content(txt)
}

func chatFinish(c map[string]any, res *Result) {
	if fr, ok := strField(c, "finish_reason"); ok && fr != "" {
		res.Completed = true
		if fr == "length" {
			res.HitMaxTokens = true
		}
	}
}

// chatUsage 解析 chat usage。缓存命中字段各家不一：OpenAI 系 prompt_tokens_details.cached_tokens、
// DeepSeek 原生 prompt_cache_hit_tokens、Kimi 顶层 cached_tokens；prompt_tokens 均已含命中部分
func chatUsage(uv map[string]any, u *Usage) {
	if p, ok := intField(uv, "prompt_tokens"); ok {
		u.Prompt = p
		u.Ok = true
	}
	if c, ok := intField(uv, "completion_tokens"); ok {
		u.Completion = c
		u.Ok = true
	}
	if c, ok := chatCached(uv); ok {
		u.Cached, u.CachedOk = c, true
	}
	if d, ok := objField(uv, "completion_tokens_details"); ok {
		u.Reasoning, _ = intField(d, "reasoning_tokens")
	}
}

func chatCached(uv map[string]any) (int64, bool) {
	if d, ok := objField(uv, "prompt_tokens_details"); ok {
		if c, ok := intField(d, "cached_tokens"); ok {
			return c, true
		}
	}
	for _, k := range []string{"prompt_cache_hit_tokens", "cached_tokens"} {
		if c, ok := intField(uv, k); ok {
			return c, true
		}
	}
	return 0, false
}
