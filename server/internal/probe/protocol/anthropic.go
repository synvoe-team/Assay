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
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	Messages  []message `json:"messages"`
	Stream    bool      `json:"stream"`
}

func (anthropic) ID() string   { return ProtocolAnthropicMessages }
func (anthropic) Path() string { return "/messages" }

func (anthropic) Auth(req *http.Request, apiKey string) {
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
}

func (anthropic) LoadBody(model, content string, maxTokens int) ([]byte, error) {
	return probe.MarshalNoEscape(anthropicBody{
		Model:     model,
		MaxTokens: maxTokens,
		Messages:  []message{{Role: "user", Content: content}},
		Stream:    true,
	})
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
