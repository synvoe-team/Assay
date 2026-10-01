package protocol

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLoadBodyGolden 锁死三协议请求体的字节级形态（byte-exact 铁律：payload 不得二次序列化）。
// 无共享前缀、不关思考时与负载画像上线前逐字节一致。
func TestLoadBodyGolden(t *testing.T) {
	cases := []struct {
		name  string
		proto string
		load  Load
		want  string
	}{
		{
			name: "chat", proto: ProtocolOpenAIChat,
			load: Load{Model: "gpt-4", Prompt: Prompt{Unique: "ping"}, MaxTokens: 4},
			want: `{"model":"gpt-4","messages":[{"role":"user","content":"ping"}],"max_tokens":4,"stream":true,"stream_options":{"include_usage":true}}`,
		},
		{
			// max_output_tokens 低于下限 16 应被抬到 16
			name: "responses", proto: ProtocolOpenAIResponses,
			load: Load{Model: "gpt-4", Prompt: Prompt{Unique: "ping"}, MaxTokens: 4},
			want: `{"model":"gpt-4","input":"ping","max_output_tokens":16,"stream":true}`,
		},
		{
			name: "anthropic", proto: ProtocolAnthropicMessages,
			load: Load{Model: "claude-3", Prompt: Prompt{Unique: "ping"}, MaxTokens: 4},
			want: `{"model":"claude-3","max_tokens":4,"messages":[{"role":"user","content":"ping"}],"stream":true}`,
		},
		{
			// 特殊字符不做 HTML 转义（MarshalNoEscape 保字节保真）
			name: "chat 特殊字符", proto: ProtocolOpenAIChat,
			load: Load{Model: "m", Prompt: Prompt{Unique: "a<b>&c"}, MaxTokens: 32},
			want: `{"model":"m","messages":[{"role":"user","content":"a<b>&c"}],"max_tokens":32,"stream":true,"stream_options":{"include_usage":true}}`,
		},
		{
			// 自动前缀缓存的协议：共享前缀直接拼在最前面
			name: "chat 共享前缀", proto: ProtocolOpenAIChat,
			load: Load{Model: "m", Prompt: Prompt{Shared: "SSS\n\n", Unique: "[n c1-0] q"}, MaxTokens: 8},
			want: `{"model":"m","messages":[{"role":"user","content":"SSS\n\n[n c1-0] q"}],"max_tokens":8,"stream":true,"stream_options":{"include_usage":true}}`,
		},
		{
			name: "responses 共享前缀", proto: ProtocolOpenAIResponses,
			load: Load{Model: "m", Prompt: Prompt{Shared: "SSS", Unique: "U"}, MaxTokens: 32},
			want: `{"model":"m","input":"SSSU","max_output_tokens":32,"stream":true}`,
		},
		{
			// Anthropic 只缓存显式断点之前的内容：共享前缀单独成块并带 cache_control
			name: "anthropic 共享前缀", proto: ProtocolAnthropicMessages,
			load: Load{Model: "c", Prompt: Prompt{Shared: "SSS", Unique: "U"}, MaxTokens: 8},
			want: `{"model":"c","max_tokens":8,"messages":[{"role":"user","content":[{"type":"text","text":"SSS","cache_control":{"type":"ephemeral"}},{"type":"text","text":"U"}]}],"stream":true}`,
		},
		{
			name: "chat 关思考", proto: ProtocolOpenAIChat,
			load: Load{Model: "m", Prompt: Prompt{Unique: "q"}, MaxTokens: 8, DisableThinking: true},
			want: `{"model":"m","messages":[{"role":"user","content":"q"}],"max_tokens":8,"stream":true,"stream_options":{"include_usage":true},"thinking":{"type":"disabled"}}`,
		},
		{
			name: "responses 关思考", proto: ProtocolOpenAIResponses,
			load: Load{Model: "m", Prompt: Prompt{Unique: "q"}, MaxTokens: 32, DisableThinking: true},
			want: `{"model":"m","input":"q","max_output_tokens":32,"stream":true,"reasoning":{"effort":"none"}}`,
		},
		{
			name: "anthropic 关思考", proto: ProtocolAnthropicMessages,
			load: Load{Model: "c", Prompt: Prompt{Unique: "q"}, MaxTokens: 8, DisableThinking: true},
			want: `{"model":"c","max_tokens":8,"messages":[{"role":"user","content":"q"}],"stream":true,"thinking":{"type":"disabled"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := Get(tc.proto)
			if !ok {
				t.Fatalf("协议未注册: %s", tc.proto)
			}
			got, err := c.LoadBody(tc.load)
			if err != nil {
				t.Fatalf("LoadBody 出错: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("请求体不符\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func TestPromptText(t *testing.T) {
	if got := (Prompt{Shared: "ab", Unique: "cd"}).Text(); got != "abcd" {
		t.Errorf("Text()=%q", got)
	}
}

// TestAuthHeaders 校验三协议认证头范式
func TestAuthHeaders(t *testing.T) {
	t.Run("openai bearer", func(t *testing.T) {
		req := httptest.NewRequest("POST", "http://x/v1/chat/completions", nil)
		mustGet(t, ProtocolOpenAIChat).Auth(req, "sk-123")
		if got := req.Header.Get("Authorization"); got != "Bearer sk-123" {
			t.Errorf("Authorization=%q", got)
		}
	})
	t.Run("anthropic x-api-key", func(t *testing.T) {
		req := httptest.NewRequest("POST", "http://x/v1/messages", nil)
		mustGet(t, ProtocolAnthropicMessages).Auth(req, "sk-ant")
		if got := req.Header.Get("x-api-key"); got != "sk-ant" {
			t.Errorf("x-api-key=%q", got)
		}
		if got := req.Header.Get("anthropic-version"); got != anthropicVersion {
			t.Errorf("anthropic-version=%q", got)
		}
		if req.Header.Get("Authorization") != "" {
			t.Error("anthropic 不应带 Authorization 头")
		}
	})
}

const chatStream = `data: {"choices":[{"delta":{"role":"assistant"}}]}

data: {"choices":[{"delta":{"content":""}}]}

data: {"choices":[{"delta":{"content":"Hello"}}]}

data: {"choices":[{"delta":{"content":" world"}}]}

data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: [DONE]
`

const responsesCompleted = `data: {"type":"response.created","response":{"id":"resp_1"}}

data: {"type":"response.output_text.delta","delta":""}

data: {"type":"response.output_text.delta","delta":"Hi"}

data: {"type":"response.completed","response":{"usage":{"input_tokens":7,"output_tokens":3}}}
`

const responsesIncomplete = `data: {"type":"response.output_text.delta","delta":"Yo"}

data: {"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":8,"output_tokens":16}}}
`

// 带真实 event: 行的 anthropic 六段式，兼验非 data: 行被跳过
const anthropicStream = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":12,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}

event: message_stop
data: {"type":"message_stop"}
`

// TestScanStream 校验流式 TTFT 首非空 delta 识别 + 尾帧 usage 提取 + 结束帧识别（三协议 + responses 双终态）
func TestScanStream(t *testing.T) {
	cases := []struct {
		name           string
		proto          string
		stream         string
		wantPrompt     int64
		wantCompletion int64
		wantHitMax     bool
	}{
		{"chat include_usage", ProtocolOpenAIChat, chatStream, 10, 5, false},
		{"responses completed", ProtocolOpenAIResponses, responsesCompleted, 7, 3, false},
		// max_output_tokens 打满时终态是 incomplete：仍是正常结束，且标记打满上限
		{"responses incomplete", ProtocolOpenAIResponses, responsesIncomplete, 8, 16, true},
		{"anthropic message_delta", ProtocolAnthropicMessages, anthropicStream, 12, 9, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustGet(t, tc.proto)
			deltas, contents := 0, 0
			r, err := c.ScanStream(strings.NewReader(tc.stream), Hooks{
				OnFirstDelta:   func() { deltas++ },
				OnFirstContent: func() { contents++ },
			})
			if err != nil {
				t.Fatalf("ScanStream 出错: %v", err)
			}
			if deltas != 1 || contents != 1 {
				t.Errorf("首增量回调 %d 次、首正文回调 %d 次，期望各 1 次（空 delta 不得触发）", deltas, contents)
			}
			if !r.Completed {
				t.Error("见到结束帧时 Completed 应为 true")
			}
			if !r.SawContent || r.SawReasoning {
				t.Errorf("SawContent=%v SawReasoning=%v，期望 true/false", r.SawContent, r.SawReasoning)
			}
			if r.HitMaxTokens != tc.wantHitMax {
				t.Errorf("HitMaxTokens=%v，期望 %v", r.HitMaxTokens, tc.wantHitMax)
			}
			if !r.Usage.Ok {
				t.Fatal("usage.Ok 应为 true")
			}
			if r.Usage.Prompt != tc.wantPrompt || r.Usage.Completion != tc.wantCompletion {
				t.Errorf("usage = {%d,%d}，期望 {%d,%d}", r.Usage.Prompt, r.Usage.Completion, tc.wantPrompt, tc.wantCompletion)
			}
		})
	}
}

// TestScanStreamNoEndFrame 流在结束帧之前被掐断（CF 524 / 连接重置的典型形态）：Completed=false
func TestScanStreamNoEndFrame(t *testing.T) {
	cases := []struct{ name, proto, stream string }{
		{"chat 无 finish_reason 无 [DONE]", ProtocolOpenAIChat, `data: {"choices":[{"delta":{"content":"Hel"}}]}
`},
		{"responses 无终态事件", ProtocolOpenAIResponses, `data: {"type":"response.output_text.delta","delta":"Hel"}
`},
		{"anthropic 无 message_stop", ProtocolAnthropicMessages, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := mustGet(t, tc.proto).ScanStream(strings.NewReader(tc.stream), Hooks{})
			if err != nil {
				t.Fatalf("出错: %v", err)
			}
			if r.Completed {
				t.Error("没有结束帧时 Completed 应为 false")
			}
			if !r.SawContent {
				t.Error("已收到正文，SawContent 应为 true")
			}
		})
	}
}

// TestScanStreamReasoning 推理模型：先推理增量后正文 / 只有推理被上限截断。
// 首增量回调在推理增量时触发、首正文回调只认正文。
func TestScanStreamReasoning(t *testing.T) {
	cases := []struct {
		name        string
		proto       string
		stream      string
		wantContent bool
		wantHitMax  bool
	}{
		{"chat reasoning_content 后正文", ProtocolOpenAIChat, `data: {"choices":[{"delta":{"reasoning_content":"想"}}]}

data: {"choices":[{"delta":{"content":"答"},"finish_reason":"stop"}]}

data: [DONE]
`, true, false},
		{"chat reasoning 字段被 length 截断", ProtocolOpenAIChat, `data: {"choices":[{"delta":{"reasoning":"想"}}]}

data: {"choices":[{"delta":{},"finish_reason":"length"}]}

data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":64}}

data: [DONE]
`, false, true},
		{"responses reasoning_text 被截断", ProtocolOpenAIResponses, `data: {"type":"response.reasoning_text.delta","delta":"想"}

data: {"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":8,"output_tokens":16}}}
`, false, true},
		{"responses reasoning_summary 后正文", ProtocolOpenAIResponses, `data: {"type":"response.reasoning_summary_text.delta","delta":"想"}

data: {"type":"response.output_text.delta","delta":"答"}

data: {"type":"response.completed","response":{"usage":{"input_tokens":8,"output_tokens":20}}}
`, true, false},
		{"anthropic thinking 被 max_tokens 截断", ProtocolAnthropicMessages, `data: {"type":"message_start","message":{"usage":{"input_tokens":12,"output_tokens":1}}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"想"}}

data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":64}}

data: {"type":"message_stop"}
`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deltas, contents := 0, 0
			r, err := mustGet(t, tc.proto).ScanStream(strings.NewReader(tc.stream), Hooks{
				OnFirstDelta:   func() { deltas++ },
				OnFirstContent: func() { contents++ },
			})
			if err != nil {
				t.Fatalf("出错: %v", err)
			}
			if !r.SawReasoning || !r.Completed {
				t.Errorf("SawReasoning=%v Completed=%v，期望均为 true", r.SawReasoning, r.Completed)
			}
			if deltas != 1 {
				t.Errorf("首增量回调 %d 次，期望 1（推理增量即触发）", deltas)
			}
			if r.SawContent != tc.wantContent || (contents == 1) != tc.wantContent {
				t.Errorf("SawContent=%v 首正文回调 %d 次，期望正文=%v", r.SawContent, contents, tc.wantContent)
			}
			if r.HitMaxTokens != tc.wantHitMax {
				t.Errorf("HitMaxTokens=%v，期望 %v", r.HitMaxTokens, tc.wantHitMax)
			}
		})
	}
}

// TestScanStreamCached 三协议缓存命中 token 提取，且 Prompt 统一为「含缓存的总输入」：
// openai 两协议的 prompt/input_tokens 本就含 cached；anthropic 的 input_tokens 不含缓存读写，须加回
func TestScanStreamCached(t *testing.T) {
	cases := []struct{ name, proto, stream string }{
		{"chat prompt_tokens_details", ProtocolOpenAIChat, `data: {"choices":[{"delta":{"content":"a"},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":7}}}

data: [DONE]
`},
		{"responses input_tokens_details", ProtocolOpenAIResponses, `data: {"type":"response.output_text.delta","delta":"a"}

data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":1,"input_tokens_details":{"cached_tokens":7}}}}
`},
		{"anthropic cache_read_input_tokens", ProtocolAnthropicMessages, `data: {"type":"message_start","message":{"usage":{"input_tokens":3,"cache_read_input_tokens":7,"output_tokens":1}}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}

data: {"type":"message_stop"}
`},
		{"anthropic 缓存读写都要加回总输入", ProtocolAnthropicMessages, `data: {"type":"message_start","message":{"usage":{"input_tokens":1,"cache_creation_input_tokens":2,"cache_read_input_tokens":7,"output_tokens":1}}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}

data: {"type":"message_stop"}
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := mustGet(t, tc.proto).ScanStream(strings.NewReader(tc.stream), Hooks{})
			if err != nil {
				t.Fatalf("出错: %v", err)
			}
			if r.Usage.Cached != 7 || r.Usage.Prompt != 10 {
				t.Errorf("Cached/Prompt=%d/%d，期望 7/10（Prompt 为含缓存的总输入）", r.Usage.Cached, r.Usage.Prompt)
			}
		})
	}
}

// TestScanStreamErrorEvent 流内错误事件（200 之后上游在流里报错）必须返回 err，且保留错误类型
func TestScanStreamErrorEvent(t *testing.T) {
	cases := []struct{ name, proto, stream, wantInErr string }{
		{"chat 顶层 error 分片", ProtocolOpenAIChat, `data: {"choices":[{"delta":{"content":"a"}}]}

data: {"error":{"type":"server_error","message":"upstream reset"}}
`, "server_error"},
		{"responses response.failed", ProtocolOpenAIResponses, `data: {"type":"response.failed","response":{"error":{"code":"server_error","message":"boom"}}}
`, "server_error"},
		{"responses error 事件", ProtocolOpenAIResponses, `data: {"type":"error","code":"rate_limit_exceeded","message":"slow down"}
`, "rate_limit_exceeded"},
		{"anthropic overloaded_error", ProtocolAnthropicMessages, `event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}
`, "overloaded_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mustGet(t, tc.proto).ScanStream(strings.NewReader(tc.stream), Hooks{})
			if err == nil {
				t.Fatal("流内错误事件应返回 err")
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("err=%q 应包含错误类型 %q", err, tc.wantInErr)
			}
		})
	}
}

// TestScanStreamNoUsage 无 usage 尾帧时 Ok=false（缺失 ≠ 0）
func TestScanStreamNoUsage(t *testing.T) {
	const s = `data: {"choices":[{"delta":{"content":"hi"}}]}

data: [DONE]
`
	r, err := mustGet(t, ProtocolOpenAIChat).ScanStream(strings.NewReader(s), Hooks{})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if r.Usage.Ok {
		t.Error("无 usage 帧时 Ok 应为 false")
	}
	if !r.Completed {
		t.Error("[DONE] 即结束帧，Completed 应为 true")
	}
}

// TestScanStreamBadChunk 分片非 JSON 判传输层失败
func TestScanStreamBadChunk(t *testing.T) {
	const s = `data: {not json}
`
	_, err := mustGet(t, ProtocolOpenAIChat).ScanStream(strings.NewReader(s), Hooks{})
	if err == nil {
		t.Error("坏损分片应返回 err")
	}
}

// TestGetFallback 空 ID 兜底 openai_chat
func TestGetFallback(t *testing.T) {
	c, ok := Get("")
	if !ok || c.ID() != ProtocolOpenAIChat {
		t.Errorf("空 ID 应兜底 openai_chat，得到 ok=%v id=%v", ok, c)
	}
	if _, ok := Get("nonexistent"); ok {
		t.Error("未知协议应返回 ok=false")
	}
}

func mustGet(t *testing.T, id string) Codec {
	t.Helper()
	c, ok := Get(id)
	if !ok {
		t.Fatalf("协议未注册: %s", id)
	}
	return c
}
