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
	Model         string            `json:"model"`
	Messages      []message         `json:"messages"`
	MaxTokens     int               `json:"max_tokens"`
	Stream        bool              `json:"stream"`
	StreamOptions chatStreamOptions `json:"stream_options"`
	// Thinking 关思考：chat 协议没有官方关思考参数（reasoning_effort 各家取值互不兼容，OpenAI 官方推理模型
	// 请走 responses 协议），取 DeepSeek/GLM/Kimi/豆包通行的 thinking.type=disabled；严格校验参数的上游会 400
	Thinking *thinkingConfig `json:"thinking,omitempty"`
}

func (openaiChat) ID() string   { return ProtocolOpenAIChat }
func (openaiChat) Path() string { return "/chat/completions" }

func (openaiChat) Auth(req *http.Request, apiKey string) {
	req.Header.Set("Authorization", "Bearer "+apiKey)
}

func (openaiChat) LoadBody(l Load) ([]byte, error) {
	b := chatBody{
		Model:         l.Model,
		Messages:      []message{{Role: "user", Content: l.Prompt.Text()}},
		MaxTokens:     l.MaxTokens,
		Stream:        true,
		StreamOptions: chatStreamOptions{IncludeUsage: true},
	}
	if l.DisableThinking {
		b.Thinking = thinkingDisabled
	}
	return probe.MarshalNoEscape(b)
}

func (openaiChat) ScanStream(r io.Reader, h Hooks) (Result, error) {
	var res Result
	tr := tracker{h: h, res: &res}
	done, err := scanSSE(r, func(m map[string]any) error {
		// 部分 OpenAI 兼容上游在 200 之后于流里下发 {"error":{...}} 分片
		if ev, ok := objField(m, "error"); ok {
			kind, _ := strField(ev, "type")
			if kind == "" {
				kind, _ = strField(ev, "code")
			}
			msg, _ := strField(ev, "message")
			return streamError(kind, msg)
		}
		// 尾帧 usage：include_usage 生效时末帧 choices 为空、usage 非空
		if uv, ok := objField(m, "usage"); ok {
			if p, ok := intField(uv, "prompt_tokens"); ok {
				res.Prompt = p
				res.Ok = true
			}
			if c, ok := intField(uv, "completion_tokens"); ok {
				res.Completion = c
				res.Ok = true
			}
			if d, ok := objField(uv, "prompt_tokens_details"); ok {
				res.Cached, _ = intField(d, "cached_tokens")
			}
		}
		choices, _ := m["choices"].([]any)
		for _, ch := range choices {
			c, ok := ch.(map[string]any)
			if !ok {
				continue
			}
			if delta, ok := objField(c, "delta"); ok {
				// 推理增量字段各家不一：DeepSeek/Kimi/Qwen 用 reasoning_content，OpenRouter/vLLM 用 reasoning
				rc, _ := strField(delta, "reasoning_content")
				tr.reasoning(rc)
				rs, _ := strField(delta, "reasoning")
				tr.reasoning(rs)
				txt, _ := strField(delta, "content")
				tr.content(txt)
			}
			if fr, ok := strField(c, "finish_reason"); ok && fr != "" {
				res.Completed = true
				if fr == "length" {
					res.HitMaxTokens = true
				}
			}
		}
		return nil
	})
	if done {
		res.Completed = true
	}
	return res, err
}
