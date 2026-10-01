package protocol

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"

	"github.com/Yukiho0287/assay/server/internal/probe"
)

// Shape 请求形态的兼容开关。各家对「关思考 / 生成上限字段 / 流式 usage / 缓存断点」的写法不一、
// 对不认识的参数有的 400 有的静默忽略，由稳定性准备期的预检按上游实际反应定下后全任务一致。
// 零值 = 最通用的默认形态（与兼容开关上线前逐字节一致）。
type Shape struct {
	Thinking            string                     // 关思考写法 ID（见 thinkingVariants）；空 = 不发任何思考参数
	MaxCompletionTokens bool                       // chat：生成上限用 max_completion_tokens（OpenAI 推理模型拒收 max_tokens）
	NoStreamUsage       bool                       // chat：不发 stream_options（个别上游不认识它直接 400）
	ChatCacheControl    bool                       // chat：共享前缀拆成带 cache_control 的 content part（OpenRouter→Claude/Gemini、阿里云显式缓存）
	PlainContent        bool                       // anthropic：上游不认 content block / cache_control，共享前缀与唯一段拼成纯文本（缓存只能靠上游自动前缀缓存）
	MaxTokensCap        int                        // 模型允许的最大生成上限（预检从上游报错读出）；0 = 不钳
	BearerAuth          bool                       // anthropic：认证改用 Authorization: Bearer（部分兼容网关不认 x-api-key）
	ExtraBody           map[string]json.RawMessage // 合并进请求体顶层的自定义字段（最后合并，同名覆盖）
	ExtraHeaders        map[string]string          // 追加的请求头（认证头之后设置，同名覆盖）
}

// 关思考写法 ID
const (
	ThinkingDisabled       = "thinking_disabled"        // thinking.type=disabled：DeepSeek / GLM / Kimi / 豆包 / Anthropic 官方
	ThinkingEffortNone     = "reasoning_effort_none"    // OpenAI 官方 effort=none（gpt-5.1 起）、Gemini OpenAI 兼容、DeepSeek chat
	ThinkingEffortMinimal  = "reasoning_effort_minimal" // effort=minimal：不支持 none 的 OpenAI 推理模型的最低档（只减不关）
	ThinkingEnableFalse    = "enable_thinking_false"    // enable_thinking=false：通义千问 Qwen3（阿里云百炼 / 硅基流动）
	ThinkingTemplateKwargs = "chat_template_kwargs"     // chat_template_kwargs：vLLM / SGLang 自部署（Qwen3 认 enable_thinking、DeepSeek 认 thinking）
	ThinkingReasoningOff   = "reasoning_enabled_false"  // reasoning.enabled=false：OpenRouter 统一推理参数
)

// thinkingVariant 一种关思考写法：各协议下并进请求体顶层的字段（缺某协议 = 该协议不适用）
type thinkingVariant struct {
	fields map[string]map[string]json.RawMessage
	label  map[string]string // 报告展示：该协议下实际发的字段原文
}

var thinkingVariants = map[string]thinkingVariant{}

// thinkingCandidates 自动探测时各协议逐个尝试的顺序：最通行、最官方的在前，只减不关的 minimal 垫底
var thinkingCandidates = map[string][]string{
	ProtocolOpenAIChat:        {ThinkingDisabled, ThinkingEffortNone, ThinkingEnableFalse, ThinkingTemplateKwargs, ThinkingReasoningOff, ThinkingEffortMinimal},
	ProtocolOpenAIResponses:   {ThinkingEffortNone, ThinkingEffortMinimal, ThinkingDisabled, ThinkingEnableFalse, ThinkingTemplateKwargs},
	ProtocolAnthropicMessages: {ThinkingDisabled},
}

func init() {
	openai := func(chat, responses string) map[string]string {
		m := map[string]string{ProtocolOpenAIChat: chat, ProtocolOpenAIResponses: responses}
		if chat == "" {
			delete(m, ProtocolOpenAIChat)
		}
		return m
	}
	defs := map[string]map[string]string{
		ThinkingDisabled: {
			ProtocolOpenAIChat:        `{"thinking":{"type":"disabled"}}`,
			ProtocolOpenAIResponses:   `{"thinking":{"type":"disabled"}}`,
			ProtocolAnthropicMessages: `{"thinking":{"type":"disabled"}}`,
		},
		ThinkingEffortNone:     openai(`{"reasoning_effort":"none"}`, `{"reasoning":{"effort":"none"}}`),
		ThinkingEffortMinimal:  openai(`{"reasoning_effort":"minimal"}`, `{"reasoning":{"effort":"minimal"}}`),
		ThinkingEnableFalse:    openai(`{"enable_thinking":false}`, `{"enable_thinking":false}`),
		ThinkingTemplateKwargs: openai(`{"chat_template_kwargs":{"enable_thinking":false,"thinking":false}}`, `{"chat_template_kwargs":{"enable_thinking":false,"thinking":false}}`),
		ThinkingReasoningOff:   openai(`{"reasoning":{"enabled":false}}`, `{"reasoning":{"enabled":false}}`),
	}
	for id, byProto := range defs {
		v := thinkingVariant{fields: map[string]map[string]json.RawMessage{}, label: map[string]string{}}
		for proto, js := range byProto {
			var f map[string]json.RawMessage
			if err := json.Unmarshal([]byte(js), &f); err != nil {
				panic(fmt.Sprintf("protocol: 关思考写法 %s/%s 不是合法 JSON: %v", id, proto, err))
			}
			v.fields[proto], v.label[proto] = f, js
		}
		thinkingVariants[id] = v
	}
	for proto, ids := range thinkingCandidates {
		for _, id := range ids {
			if !ThinkingApplies(id, proto) {
				panic(fmt.Sprintf("protocol: %s 的自动探测候选 %s 不适用", proto, id))
			}
		}
	}
}

// ThinkingApplies 某关思考写法是否适用于该协议
func ThinkingApplies(id, proto string) bool {
	_, ok := thinkingVariants[id].fields[proto]
	return ok
}

// ThinkingCandidates 自动探测关思考写法时该协议的尝试顺序
func ThinkingCandidates(proto string) []string { return thinkingCandidates[proto] }

// ThinkingLabel 该写法在该协议下实际发的字段原文（报告展示）；不适用返回空
func ThinkingLabel(id, proto string) string { return thinkingVariants[id].label[proto] }

// ThinkingFieldNames 该写法在该协议下占用的顶层字段名（预检按上游报错点名剔除用）
func ThinkingFieldNames(id, proto string) []string {
	return keys(thinkingVariants[id].fields[proto])
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// applyShape 把关思考写法与自定义字段并进请求体顶层：只动顶层键，prompt 等原有值按原字节保留。
// 合并序：基础体 < 关思考写法 < 自定义字段（用户显式给的最优先）。无字段可合并时原样返回（保持字段顺序）。
func applyShape(base []byte, proto string, s Shape) ([]byte, error) {
	if s.Thinking == "" && len(s.ExtraBody) == 0 {
		return base, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(base, &m); err != nil {
		return nil, fmt.Errorf("合并兼容字段: %w", err)
	}
	if s.Thinking != "" {
		f, ok := thinkingVariants[s.Thinking].fields[proto]
		if !ok {
			return nil, fmt.Errorf("关思考写法 %q 不适用于 %s", s.Thinking, proto)
		}
		maps.Copy(m, f)
	}
	maps.Copy(m, s.ExtraBody)
	return probe.MarshalNoEscape(m)
}

// ApplyHeaders 认证头之后再按形态改头：Bearer 认证替换 x-api-key，自定义请求头最后设置（同名覆盖）
func (s Shape) ApplyHeaders(req *http.Request, apiKey string) {
	if s.BearerAuth {
		req.Header.Del("x-api-key")
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for k, v := range s.ExtraHeaders {
		req.Header.Set(k, v)
	}
}
