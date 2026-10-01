package protocol

import (
	"io"
	"net/http"

	"github.com/Yukiho0287/assay/server/internal/probe"
)

// ProtocolOpenAIResponses OpenAI Responses 协议 ID
const ProtocolOpenAIResponses = "openai_responses"

// responsesMinMaxTokens Responses API 的 max_output_tokens 下限
const responsesMinMaxTokens = 16

type openaiResponses struct{}

func init() { register(openaiResponses{}) }

type responsesBody struct {
	Model           string              `json:"model"`
	Input           string              `json:"input"`
	MaxOutputTokens int                 `json:"max_output_tokens"`
	Stream          bool                `json:"stream"`
	Reasoning       *responsesReasoning `json:"reasoning,omitempty"`
}

// responsesReasoning 官方推理参数；effort=none 即不推理（OpenAI gpt-5.1 起、DeepSeek responses 均支持）
type responsesReasoning struct {
	Effort string `json:"effort"`
}

func (openaiResponses) ID() string   { return ProtocolOpenAIResponses }
func (openaiResponses) Path() string { return "/responses" }

func (openaiResponses) Auth(req *http.Request, apiKey string) {
	req.Header.Set("Authorization", "Bearer "+apiKey)
}

func (openaiResponses) LoadBody(l Load) ([]byte, error) {
	b := responsesBody{
		Model:           l.Model,
		Input:           l.Prompt.Text(),
		MaxOutputTokens: max(l.MaxTokens, responsesMinMaxTokens), // 低于下限上游直接 400
		Stream:          true,
	}
	if l.DisableThinking {
		b.Reasoning = &responsesReasoning{Effort: "none"}
	}
	return probe.MarshalNoEscape(b)
}

func (openaiResponses) ScanStream(r io.Reader, h Hooks) (Result, error) {
	var res Result
	tr := tracker{h: h, res: &res}
	_, err := scanSSE(r, func(m map[string]any) error {
		typ, _ := strField(m, "type")
		switch typ {
		case "response.output_text.delta":
			txt, _ := strField(m, "delta")
			tr.content(txt)
		case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			txt, _ := strField(m, "delta")
			tr.reasoning(txt)
		case "response.completed", "response.incomplete":
			// max_output_tokens 打满时终态常是 incomplete 而非 completed，usage 同样带；
			// 两者都是正常结束，否则小 max_tokens 压测会大面积误判为断流。
			res.Completed = true
			resp, ok := objField(m, "response")
			if !ok {
				return nil
			}
			if d, ok := objField(resp, "incomplete_details"); ok {
				if reason, _ := strField(d, "reason"); reason == "max_output_tokens" {
					res.HitMaxTokens = true
				}
			}
			uv, ok := objField(resp, "usage")
			if !ok {
				return nil
			}
			if p, ok := intField(uv, "input_tokens"); ok {
				res.Prompt = p
				res.Ok = true
			}
			if c, ok := intField(uv, "output_tokens"); ok {
				res.Completion = c
				res.Ok = true
			}
			if d, ok := objField(uv, "input_tokens_details"); ok {
				res.Cached, _ = intField(d, "cached_tokens")
			}
		case "response.failed":
			resp, _ := objField(m, "response")
			ev, _ := objField(resp, "error")
			code, _ := strField(ev, "code")
			msg, _ := strField(ev, "message")
			return streamError(code, msg)
		case "error":
			code, _ := strField(m, "code")
			msg, _ := strField(m, "message")
			return streamError(code, msg)
		}
		return nil
	})
	return res, err
}
