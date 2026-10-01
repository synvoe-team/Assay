package protocol

import (
	"io"
	"net/http"

	"github.com/Yukiho0287/assay/server/internal/probe"
)

// ProtocolAnthropicMessages Anthropic messages 协议 ID
const ProtocolAnthropicMessages = "anthropic_messages"

// anthropicVersion Anthropic 必带的版本头（与 connectivity 一致）
const anthropicVersion = "2023-06-01"

type anthropic struct{}

func init() { register(anthropic{}) }

type anthropicBody struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []anthropicMessage `json:"messages"`
	Stream    bool               `json:"stream"`
	// Thinking 关思考：官方 thinking.type=disabled；兼容端点（如 DeepSeek /anthropic）默认开思考，必须显式关
	Thinking *thinkingConfig `json:"thinking,omitempty"`
}

// anthropicMessage content 为纯文本串，或有共享前缀时的内容块数组
type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// anthropicBlock 文本内容块；CacheControl 标出缓存断点（断点之前的内容被缓存）
type anthropicBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"`
}

func (anthropic) ID() string   { return ProtocolAnthropicMessages }
func (anthropic) Path() string { return "/messages" }

func (anthropic) Auth(req *http.Request, apiKey string) {
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
}

func (anthropic) LoadBody(l Load) ([]byte, error) {
	b := anthropicBody{
		Model:     l.Model,
		MaxTokens: l.MaxTokens,
		Messages:  []anthropicMessage{{Role: "user", Content: anthropicContent(l.Prompt)}},
		Stream:    true,
	}
	if l.DisableThinking {
		b.Thinking = thinkingDisabled
	}
	return probe.MarshalNoEscape(b)
}

// anthropicContent Anthropic 不做自动前缀缓存，只缓存显式 cache_control 断点之前的内容：
// 共享前缀单独成块并打断点，唯一段另起一块；无共享前缀时保持纯文本串
func anthropicContent(p Prompt) any {
	if p.Shared == "" {
		return p.Unique
	}
	return []anthropicBlock{
		{Type: "text", Text: p.Shared, CacheControl: &cacheControl{Type: "ephemeral"}},
		{Type: "text", Text: p.Unique},
	}
}

func (anthropic) ScanStream(r io.Reader, h Hooks) (Result, error) {
	var res Result
	tr := tracker{h: h, res: &res}
	_, err := scanSSE(r, func(m map[string]any) error {
		typ, _ := strField(m, "type")
		switch typ {
		case "message_start":
			// 初始 usage：input_tokens 全量给出，output_tokens 起始（后续在 message_delta 累计）
			msg, _ := objField(m, "message")
			uv, ok := objField(msg, "usage")
			if !ok {
				return nil
			}
			if p, ok := intField(uv, "input_tokens"); ok {
				res.Prompt = p
				res.Ok = true
			}
			res.Cached, _ = intField(uv, "cache_read_input_tokens")
		case "content_block_delta":
			delta, _ := objField(m, "delta")
			// input_json_delta（工具入参）既非正文也非推理，不计
			switch dt, _ := strField(delta, "type"); dt {
			case "text_delta":
				txt, _ := strField(delta, "text")
				tr.content(txt)
			case "thinking_delta":
				txt, _ := strField(delta, "thinking")
				tr.reasoning(txt)
			}
		case "message_delta":
			if d, ok := objField(m, "delta"); ok {
				if sr, _ := strField(d, "stop_reason"); sr == "max_tokens" {
					res.HitMaxTokens = true
				}
			}
			// 终帧顶层 usage 带累计 output_tokens
			uv, ok := objField(m, "usage")
			if !ok {
				return nil
			}
			if c, ok := intField(uv, "output_tokens"); ok {
				res.Completion = c
				res.Ok = true
			}
		case "message_stop":
			res.Completed = true
		case "error":
			ev, _ := objField(m, "error")
			kind, _ := strField(ev, "type")
			msg, _ := strField(ev, "message")
			return streamError(kind, msg)
		}
		return nil
	})
	return res, err
}
