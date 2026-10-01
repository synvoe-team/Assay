package protocol

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

// TestShapeBodyGolden 兼容形态的请求体：关思考写法与自定义字段只并进顶层（合并后键按字典序），
// 生成上限字段 / stream_options / 显式缓存断点由 codec 结构体直出（不合并时保持原字段顺序）
func TestShapeBodyGolden(t *testing.T) {
	cases := []struct {
		name  string
		proto string
		load  Load
		want  string
	}{
		{
			name: "chat thinking.disabled", proto: ProtocolOpenAIChat,
			load: Load{Model: "m", Prompt: Prompt{Unique: "q"}, MaxTokens: 8, Shape: Shape{Thinking: ThinkingDisabled}},
			want: `{"max_tokens":8,"messages":[{"role":"user","content":"q"}],"model":"m","stream":true,"stream_options":{"include_usage":true},"thinking":{"type":"disabled"}}`,
		},
		{
			name: "chat reasoning_effort=none", proto: ProtocolOpenAIChat,
			load: Load{Model: "m", Prompt: Prompt{Unique: "q"}, MaxTokens: 8, Shape: Shape{Thinking: ThinkingEffortNone}},
			want: `{"max_tokens":8,"messages":[{"role":"user","content":"q"}],"model":"m","reasoning_effort":"none","stream":true,"stream_options":{"include_usage":true}}`,
		},
		{
			name: "chat enable_thinking=false", proto: ProtocolOpenAIChat,
			load: Load{Model: "m", Prompt: Prompt{Unique: "q"}, MaxTokens: 8, Shape: Shape{Thinking: ThinkingEnableFalse}},
			want: `{"enable_thinking":false,"max_tokens":8,"messages":[{"role":"user","content":"q"}],"model":"m","stream":true,"stream_options":{"include_usage":true}}`,
		},
		{
			name: "chat chat_template_kwargs", proto: ProtocolOpenAIChat,
			load: Load{Model: "m", Prompt: Prompt{Unique: "q"}, MaxTokens: 8, Shape: Shape{Thinking: ThinkingTemplateKwargs}},
			want: `{"chat_template_kwargs":{"enable_thinking":false,"thinking":false},"max_tokens":8,"messages":[{"role":"user","content":"q"}],"model":"m","stream":true,"stream_options":{"include_usage":true}}`,
		},
		{
			name: "responses reasoning.effort=minimal", proto: ProtocolOpenAIResponses,
			load: Load{Model: "m", Prompt: Prompt{Unique: "q"}, MaxTokens: 32, Shape: Shape{Thinking: ThinkingEffortMinimal}},
			want: `{"input":"q","max_output_tokens":32,"model":"m","reasoning":{"effort":"minimal"},"stream":true}`,
		},
		{
			name: "responses 顶层 thinking.disabled（网关透传给 DeepSeek）", proto: ProtocolOpenAIResponses,
			load: Load{Model: "m", Prompt: Prompt{Unique: "q"}, MaxTokens: 32, Shape: Shape{Thinking: ThinkingDisabled}},
			want: `{"input":"q","max_output_tokens":32,"model":"m","stream":true,"thinking":{"type":"disabled"}}`,
		},
		{
			name: "anthropic thinking.disabled", proto: ProtocolAnthropicMessages,
			load: Load{Model: "c", Prompt: Prompt{Unique: "q"}, MaxTokens: 8, Shape: Shape{Thinking: ThinkingDisabled}},
			want: `{"max_tokens":8,"messages":[{"role":"user","content":"q"}],"model":"c","stream":true,"thinking":{"type":"disabled"}}`,
		},
		{
			// OpenAI 推理模型在 chat 上拒收 max_tokens
			name: "chat max_completion_tokens", proto: ProtocolOpenAIChat,
			load: Load{Model: "m", Prompt: Prompt{Unique: "q"}, MaxTokens: 8, Shape: Shape{MaxCompletionTokens: true}},
			want: `{"model":"m","messages":[{"role":"user","content":"q"}],"max_completion_tokens":8,"stream":true,"stream_options":{"include_usage":true}}`,
		},
		{
			name: "chat 不发 stream_options", proto: ProtocolOpenAIChat,
			load: Load{Model: "m", Prompt: Prompt{Unique: "q"}, MaxTokens: 8, Shape: Shape{NoStreamUsage: true}},
			want: `{"model":"m","messages":[{"role":"user","content":"q"}],"max_tokens":8,"stream":true}`,
		},
		{
			// OpenRouter→Claude/Gemini、阿里云显式缓存：chat 也要显式断点
			name: "chat 显式缓存断点", proto: ProtocolOpenAIChat,
			load: Load{Model: "m", Prompt: Prompt{Shared: "SSS", Unique: "U"}, MaxTokens: 8, Shape: Shape{ChatCacheControl: true}},
			want: `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"SSS","cache_control":{"type":"ephemeral"}},{"type":"text","text":"U"}]}],"max_tokens":8,"stream":true,"stream_options":{"include_usage":true}}`,
		},
		{
			name: "chat 显式缓存断点但无共享前缀 → 仍是纯文本", proto: ProtocolOpenAIChat,
			load: Load{Model: "m", Prompt: Prompt{Unique: "U"}, MaxTokens: 8, Shape: Shape{ChatCacheControl: true}},
			want: `{"model":"m","messages":[{"role":"user","content":"U"}],"max_tokens":8,"stream":true,"stream_options":{"include_usage":true}}`,
		},
		{
			// 合并序：基础体 < 关思考写法 < 自定义字段（用户显式给的最优先）；prompt 字节不被改写
			name: "自定义字段覆盖关思考写法", proto: ProtocolOpenAIChat,
			load: Load{Model: "m", Prompt: Prompt{Unique: "a<b>&c"}, MaxTokens: 8, Shape: Shape{
				Thinking:  ThinkingDisabled,
				ExtraBody: map[string]json.RawMessage{"thinking": raw(`{"type":"enabled"}`), "temperature": raw(`0`)},
			}},
			want: `{"max_tokens":8,"messages":[{"role":"user","content":"a<b>&c"}],"model":"m","stream":true,"stream_options":{"include_usage":true},"temperature":0,"thinking":{"type":"enabled"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mustGet(t, tc.proto).LoadBody(tc.load)
			if err != nil {
				t.Fatalf("LoadBody 出错: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("请求体不符\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestShapeThinkingNotApplicable 写法不适用于该协议（anthropic 没有 reasoning_effort）必须报错，不能静默发出
func TestShapeThinkingNotApplicable(t *testing.T) {
	_, err := mustGet(t, ProtocolAnthropicMessages).LoadBody(Load{Model: "c", Prompt: Prompt{Unique: "q"}, MaxTokens: 8, Shape: Shape{Thinking: ThinkingEffortNone}})
	if err == nil {
		t.Fatal("anthropic + reasoning_effort 应报错")
	}
	_, err = mustGet(t, ProtocolOpenAIChat).LoadBody(Load{Model: "m", Prompt: Prompt{Unique: "q"}, MaxTokens: 8, Shape: Shape{Thinking: "no-such"}})
	if err == nil {
		t.Fatal("未知写法应报错")
	}
}

// TestThinkingCandidates 每个候选写法都适用于对应协议，且 ThinkingApplies 与之一致
func TestThinkingCandidates(t *testing.T) {
	for _, proto := range All() {
		cands := ThinkingCandidates(proto)
		if len(cands) == 0 {
			t.Errorf("%s 没有关思考候选写法", proto)
		}
		for _, id := range cands {
			if !ThinkingApplies(id, proto) {
				t.Errorf("%s 的候选 %s 不适用", proto, id)
			}
		}
	}
	if ThinkingApplies(ThinkingEffortNone, ProtocolAnthropicMessages) {
		t.Error("anthropic 不应适用 reasoning_effort")
	}
}

// TestThinkTags 正文开头的 <think>…</think> 按推理计（未开 reasoning parser 的 vLLM/SGLang、部分中转），
// 标签可被任意切开；首正文回调落在闭合标签之后第一个非空白正文上
func TestThinkTags(t *testing.T) {
	chat := func(parts ...string) string {
		var b strings.Builder
		for _, p := range parts {
			q, _ := json.Marshal(p)
			b.WriteString(`data: {"choices":[{"delta":{"content":` + string(q) + `}}]}` + "\n\n")
		}
		b.WriteString("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n")
		return b.String()
	}
	cases := []struct {
		name          string
		stream        string
		wantReasoning bool
		wantContent   bool
		contentAt     int // 首正文回调时已收到第几个分片（1 起）；0 = 不触发
	}{
		{"整段在一个分片里", chat("<think>想一想</think>\n\n答案"), true, true, 1},
		{"标签被切碎", chat("<thi", "nk>想", "一想</th", "ink>", "\n\n", "答案"), true, true, 6},
		{"前导空白", chat("\n", "<think>想</think>答"), true, true, 2},
		{"没闭合 = 只有推理", chat("<think>想", "啊想"), true, false, 0},
		{"普通正文", chat("Hello"), false, true, 1},
		{"以 < 开头的正文", chat("<", "b>粗体"), false, true, 2},
		{"整条回复就是半个标签", chat("<thi"), false, true, -1},
		{"纯空白不算正文", chat("\n\n"), false, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks, contentAt, deltas := 0, 0, 0
			// 用分片计数定位首正文回调：包一层 reader 不方便，这里按「data: 行」逐行喂
			var res Result
			var err error
			lines := strings.SplitAfter(tc.stream, "\n\n")
			pr := &countingReader{lines: lines, onLine: func() { chunks++ }}
			res, err = mustGet(t, ProtocolOpenAIChat).ScanStream(pr, Hooks{
				OnFirstDelta:   func() { deltas++ },
				OnFirstContent: func() { contentAt = chunks },
			})
			if err != nil {
				t.Fatalf("出错: %v", err)
			}
			if res.SawReasoning != tc.wantReasoning || res.SawContent != tc.wantContent {
				t.Errorf("SawReasoning=%v SawContent=%v，期望 %v/%v", res.SawReasoning, res.SawContent, tc.wantReasoning, tc.wantContent)
			}
			if tc.contentAt > 0 && contentAt != tc.contentAt {
				t.Errorf("首正文回调在第 %d 个分片，期望第 %d 个", contentAt, tc.contentAt)
			}
			if tc.contentAt == 0 && contentAt != 0 {
				t.Errorf("不应触发首正文回调（在第 %d 个分片触发了）", contentAt)
			}
			if want := btoi(tc.wantReasoning || tc.wantContent); deltas != want {
				t.Errorf("首增量回调 %d 次，期望 %d（纯空白不算增量）", deltas, want)
			}
		})
	}
}

// countingReader 一次只吐一个 SSE 事件，供测试定位回调发生在第几个分片
type countingReader struct {
	lines  []string
	cur    string
	onLine func()
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.cur == "" {
		if len(r.lines) == 0 {
			return 0, io.EOF
		}
		r.cur, r.lines = r.lines[0], r.lines[1:]
		r.onLine()
	}
	n := copy(p, r.cur)
	r.cur = r.cur[n:]
	return n, nil
}

// TestUsageVariants 各家 usage 字段写法：推理 token、DeepSeek 原生缓存字段、Kimi 顶层 cached_tokens、
// Moonshot 把 usage 放在 choice 里
func TestUsageVariants(t *testing.T) {
	cases := []struct {
		name                string
		proto               string
		stream              string
		prompt, cached, rsn int64
	}{
		{"chat reasoning_tokens", ProtocolOpenAIChat, `data: {"choices":[{"delta":{"content":"a"},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":30,"completion_tokens_details":{"reasoning_tokens":25}}}

data: [DONE]
`, 10, 0, 25},
		{"DeepSeek 原生 prompt_cache_hit_tokens", ProtocolOpenAIChat, `data: {"choices":[{"delta":{"content":"a"},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1,"prompt_cache_hit_tokens":6,"prompt_cache_miss_tokens":4}}

data: [DONE]
`, 10, 6, 0},
		{"Kimi 顶层 cached_tokens", ProtocolOpenAIChat, `data: {"choices":[{"delta":{"content":"a"},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1,"cached_tokens":5}}

data: [DONE]
`, 10, 5, 0},
		{"Moonshot usage 在 choice 里", ProtocolOpenAIChat, `data: {"choices":[{"delta":{"content":"a"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop","usage":{"prompt_tokens":10,"completion_tokens":1,"cached_tokens":3}}]}

data: [DONE]
`, 10, 3, 0},
		{"anthropic thinking_tokens（思考文本不推送）", ProtocolAnthropicMessages, `data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":40,"output_tokens_details":{"thinking_tokens":35}}}

data: {"type":"message_stop"}
`, 10, 0, 35},
		{"responses reasoning_tokens", ProtocolOpenAIResponses, `data: {"type":"response.output_text.delta","delta":"a"}

data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":30,"output_tokens_details":{"reasoning_tokens":20}}}}
`, 10, 0, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := mustGet(t, tc.proto).ScanStream(strings.NewReader(tc.stream), Hooks{})
			if err != nil {
				t.Fatalf("出错: %v", err)
			}
			if !r.Usage.Ok || r.Usage.Prompt != tc.prompt || r.Usage.Cached != tc.cached || r.Usage.Reasoning != tc.rsn {
				t.Errorf("usage=%+v，期望 prompt=%d cached=%d reasoning=%d", r.Usage, tc.prompt, tc.cached, tc.rsn)
			}
			// 缓存字段报了才算「知道命中多少」；没报 = 未知，不能当成 0 命中
			if wantOk := tc.cached > 0; r.Usage.CachedOk != wantOk {
				t.Errorf("CachedOk=%v，期望 %v", r.Usage.CachedOk, wantOk)
			}
		})
	}
}

// TestParseBody 上游无视 stream=true 回整块 JSON：三协议都能取出正文/推理/usage，Completed=true
func TestParseBody(t *testing.T) {
	cases := []struct {
		name, proto, body       string
		content, reasoning, max bool
		prompt, cached          int64
	}{
		{"chat", ProtocolOpenAIChat, `{"choices":[{"message":{"role":"assistant","reasoning_content":"想","content":"答"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":2}}}`, true, true, false, 9, 2},
		{"chat <think> 在正文里且被截断", ProtocolOpenAIChat, `{"choices":[{"message":{"content":"<think>想啊想"},"finish_reason":"length"}],"usage":{"prompt_tokens":9,"completion_tokens":64}}`, false, true, true, 9, 0},
		{"responses", ProtocolOpenAIResponses, `{"status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"想"}]},{"type":"message","content":[{"type":"output_text","text":"答"}]}],"usage":{"input_tokens":9,"output_tokens":4}}`, true, true, false, 9, 0},
		{"anthropic", ProtocolAnthropicMessages, `{"content":[{"type":"thinking","thinking":"想"},{"type":"text","text":"答"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"cache_read_input_tokens":6,"output_tokens":4}}`, true, true, false, 9, 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := mustGet(t, tc.proto).ParseBody([]byte(tc.body))
			if err != nil {
				t.Fatalf("出错: %v", err)
			}
			if !r.Completed || r.SawContent != tc.content || r.SawReasoning != tc.reasoning || r.HitMaxTokens != tc.max {
				t.Errorf("Completed=%v content=%v reasoning=%v hitMax=%v，期望 true/%v/%v/%v", r.Completed, r.SawContent, r.SawReasoning, r.HitMaxTokens, tc.content, tc.reasoning, tc.max)
			}
			if !r.Usage.Ok || r.Usage.Prompt != tc.prompt || r.Usage.Cached != tc.cached {
				t.Errorf("usage=%+v，期望 prompt=%d cached=%d", r.Usage, tc.prompt, tc.cached)
			}
		})
	}
	if _, err := mustGet(t, ProtocolOpenAIChat).ParseBody([]byte(`{"error":{"type":"invalid_request_error","message":"bad"}}`)); err == nil {
		t.Error("整块 error 对象应返回 err")
	}
	// 不少网关回 {"error":"…"} 纯字符串：同样是错误，不能当成「无正文」
	if _, err := mustGet(t, ProtocolOpenAIChat).ParseBody([]byte(`{"error":"quota exceeded"}`)); err == nil || !strings.Contains(err.Error(), "quota exceeded") {
		t.Errorf("字符串 error 应返回带原文的 err，得到 %v", err)
	}
	_, err := mustGet(t, ProtocolOpenAIChat).ScanStream(strings.NewReader("data: {\"error\":\"upstream timeout\"}\n\n"), Hooks{})
	if err == nil || !strings.Contains(err.Error(), "upstream timeout") {
		t.Errorf("流内字符串 error 应返回 err，得到 %v", err)
	}
}

// TestAnthropicPlainContent 上游不认内容块时，共享前缀与唯一段拼成纯文本、不带 cache_control
func TestAnthropicPlainContent(t *testing.T) {
	l := Load{Model: "m", Prompt: Prompt{Shared: "S", Unique: "U"}, MaxTokens: 8}
	blocks, _ := mustGet(t, ProtocolAnthropicMessages).LoadBody(l)
	if !strings.Contains(string(blocks), `"cache_control"`) {
		t.Fatalf("缺省应分块打断点: %s", blocks)
	}
	l.Shape.PlainContent = true
	plain, _ := mustGet(t, ProtocolAnthropicMessages).LoadBody(l)
	if want := `"content":"SU"`; !strings.Contains(string(plain), want) || strings.Contains(string(plain), "cache_control") {
		t.Errorf("PlainContent 应为 %s 且无 cache_control: %s", want, plain)
	}
}

// TestMaxTokensCap 预检读到模型允许的最大生成上限后，各协议实发值钳到该上限
func TestMaxTokensCap(t *testing.T) {
	l := Load{Model: "m", Prompt: Prompt{Unique: "q"}, MaxTokens: 32768, Shape: Shape{MaxTokensCap: 8192}}
	for proto, want := range map[string]string{
		ProtocolOpenAIChat:        `"max_tokens":8192`,
		ProtocolOpenAIResponses:   `"max_output_tokens":8192`,
		ProtocolAnthropicMessages: `"max_tokens":8192`,
	} {
		got, err := mustGet(t, proto).LoadBody(l)
		if err != nil || !strings.Contains(string(got), want) {
			t.Errorf("%s: %s（err=%v），应含 %s", proto, got, err, want)
		}
	}
}

// TestApplyHeaders Bearer 认证替换 x-api-key（保留 anthropic-version），自定义头最后覆盖
func TestApplyHeaders(t *testing.T) {
	req := httptest.NewRequest("POST", "http://x/v1/messages", nil)
	mustGet(t, ProtocolAnthropicMessages).Auth(req, "k")
	Shape{BearerAuth: true, ExtraHeaders: map[string]string{"anthropic-beta": "x", "X-Title": "assay"}}.ApplyHeaders(req, "k")
	if req.Header.Get("x-api-key") != "" || req.Header.Get("Authorization") != "Bearer k" || req.Header.Get("anthropic-version") == "" {
		t.Errorf("头不符: %v", req.Header)
	}
	if req.Header.Get("Anthropic-Beta") != "x" || req.Header.Get("X-Title") != "assay" {
		t.Errorf("自定义头未设置: %v", req.Header)
	}
}

// TestCachedReportedZero 上游报了缓存字段但值为 0：CachedOk=true（确实没命中），区别于压根不报
func TestCachedReportedZero(t *testing.T) {
	const s = `data: {"choices":[{"delta":{"content":"a"},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":0}}}

data: [DONE]
`
	r, err := mustGet(t, ProtocolOpenAIChat).ScanStream(strings.NewReader(s), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if !r.Usage.CachedOk || r.Usage.Cached != 0 {
		t.Errorf("CachedOk=%v Cached=%d，期望 true/0", r.Usage.CachedOk, r.Usage.Cached)
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
