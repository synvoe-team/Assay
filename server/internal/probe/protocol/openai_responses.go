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
	Model           string `json:"model"`
	Input           string `json:"input"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	Stream          bool   `json:"stream"`
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
		MaxOutputTokens: max(l.maxTokens(), responsesMinMaxTokens), // 低于下限上游直接 400
		Stream:          true,
	}
	body, err := probe.MarshalNoEscape(b)
	if err != nil {
		return nil, err
	}
	return applyShape(body, ProtocolOpenAIResponses, l.Shape)
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
			if resp, ok := objField(m, "response"); ok {
				responsesTerminal(resp, &res)
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
	tr.finish()
	return res, err
}

// ParseBody 上游无视 stream=true 回了整块 response 对象：从 output 里取推理与正文
func (openaiResponses) ParseBody(raw []byte) (Result, error) {
	var res Result
	m, err := decodeObject(raw)
	if err != nil {
		return res, err
	}
	tr := tracker{res: &res}
	output, _ := m["output"].([]any)
	for _, it := range output {
		item, ok := it.(map[string]any)
		if !ok {
			continue
		}
		parts, _ := item["content"].([]any)
		summary, _ := item["summary"].([]any)
		for _, p := range append(parts, summary...) {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			txt, _ := strField(part, "text")
			switch typ, _ := strField(part, "type"); typ {
			case "output_text":
				tr.content(txt)
			case "reasoning_text", "summary_text":
				tr.reasoning(txt)
			}
		}
	}
	responsesTerminal(m, &res)
	tr.finish()
	res.Completed = true
	return res, nil
}

// responsesTerminal 终态 response 对象：是否因上限截断 + usage（input_tokens 含 cached）
func responsesTerminal(resp map[string]any, res *Result) {
	if d, ok := objField(resp, "incomplete_details"); ok {
		if reason, _ := strField(d, "reason"); reason == "max_output_tokens" {
			res.HitMaxTokens = true
		}
	}
	uv, ok := objField(resp, "usage")
	if !ok {
		return
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
		if c, ok := intField(d, "cached_tokens"); ok {
			res.Cached, res.CachedOk = c, true
		}
	}
	if d, ok := objField(uv, "output_tokens_details"); ok {
		res.Reasoning, _ = intField(d, "reasoning_tokens")
	}
}
