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
}

// anthropicMessage content 为纯文本串，或有共享前缀时的内容块数组
type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

func (anthropic) ID() string   { return ProtocolAnthropicMessages }
func (anthropic) Path() string { return "/messages" }

func (anthropic) Auth(req *http.Request, apiKey string) {
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
}

// LoadBody Anthropic 不做自动前缀缓存，只缓存显式 cache_control 断点之前的内容：共享前缀单独成块并打断点；
// 上游不认内容块时（Shape.PlainContent）退回纯文本
func (anthropic) LoadBody(l Load) ([]byte, error) {
	var content any = l.Prompt.Text()
	if !l.Shape.PlainContent {
		content = splitContent(l.Prompt)
	}
	b := anthropicBody{
		Model:     l.Model,
		MaxTokens: l.maxTokens(),
		Messages:  []anthropicMessage{{Role: "user", Content: content}},
		Stream:    true,
	}
	body, err := probe.MarshalNoEscape(b)
	if err != nil {
		return nil, err
	}
	return applyShape(body, ProtocolAnthropicMessages, l.Shape)
}

func (anthropic) ScanStream(r io.Reader, h Hooks) (Result, error) {
	var res Result
	tr := tracker{h: h, res: &res}
	_, err := scanSSE(r, func(m map[string]any) error {
		typ, _ := strField(m, "type")
		switch typ {
		case "message_start":
			// 初始 usage：输入计量全量给出，output_tokens 起始（后续在 message_delta 累计）
			msg, _ := objField(m, "message")
			if uv, ok := objField(msg, "usage"); ok {
				anthropicInput(uv, &res.Usage)
			}
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
				anthropicStop(d, &res)
			}
			// 终帧顶层 usage 带累计 output_tokens；部分兼容网关到这里才给输入计量
			uv, ok := objField(m, "usage")
			if !ok {
				return nil
			}
			if p, ok := intField(uv, "input_tokens"); ok && p > 0 && res.Prompt == 0 {
				anthropicInput(uv, &res.Usage)
			}
			if c, ok := intField(uv, "output_tokens"); ok {
				res.Completion = c
				res.Ok = true
			}
			// 新款模型默认不推送思考文本（display=omitted），思考 token 只在终帧明细里
			if d, ok := objField(uv, "output_tokens_details"); ok {
				res.Reasoning, _ = intField(d, "thinking_tokens")
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
	tr.finish()
	return res, err
}

// ParseBody 上游无视 stream=true 回了整块 message 对象
func (anthropic) ParseBody(raw []byte) (Result, error) {
	var res Result
	m, err := decodeObject(raw)
	if err != nil {
		return res, err
	}
	tr := tracker{res: &res}
	blocks, _ := m["content"].([]any)
	for _, b := range blocks {
		blk, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch typ, _ := strField(blk, "type"); typ {
		case "text":
			txt, _ := strField(blk, "text")
			tr.content(txt)
		case "thinking":
			txt, _ := strField(blk, "thinking")
			tr.reasoning(txt)
		}
	}
	anthropicStop(m, &res)
	if uv, ok := objField(m, "usage"); ok {
		anthropicInput(uv, &res.Usage)
		if c, ok := intField(uv, "output_tokens"); ok {
			res.Completion = c
			res.Ok = true
		}
	}
	tr.finish()
	res.Completed = true
	return res, nil
}

func anthropicStop(m map[string]any, res *Result) {
	if sr, _ := strField(m, "stop_reason"); sr == "max_tokens" {
		res.HitMaxTokens = true
	}
}

// anthropicInput 输入计量。anthropic 的 input_tokens 不含缓存读写（总输入 = input + cache_creation + cache_read），
// 加回后与 openai 两协议「prompt/input_tokens 含 cached」同口径
func anthropicInput(uv map[string]any, u *Usage) {
	p, ok := intField(uv, "input_tokens")
	if !ok {
		return
	}
	created, _ := intField(uv, "cache_creation_input_tokens")
	read, readOk := intField(uv, "cache_read_input_tokens")
	u.Prompt = p + created + read
	u.Cached, u.CachedOk = read, readOk
	u.Ok = true
}
