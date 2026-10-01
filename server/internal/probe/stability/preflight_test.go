package stability

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Yukiho0287/assay/server/internal/probe/protocol"
)

// quirky 可编程的「不按规范来」的 chat 上游：reject 返回非 0 状态码即按该状态回错误；
// think 决定是否先吐推理增量；jsonReply 时无视 stream=true 回整块 JSON
type quirky struct {
	mu        sync.Mutex
	bodies    []map[string]any
	reject    func(r *http.Request, b map[string]any) (int, string)
	think     func(b map[string]any) bool
	jsonReply bool
}

func (q *quirky) handle(w http.ResponseWriter, r *http.Request) {
	var b map[string]any
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	q.mu.Lock()
	q.bodies = append(q.bodies, b)
	q.mu.Unlock()
	if q.reject != nil {
		if code, msg := q.reject(r, b); code != 0 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(code)
			fmt.Fprint(w, msg)
			return
		}
	}
	thinking := q.think != nil && q.think(b)
	if q.jsonReply {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"14332031"},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":5}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	if thinking {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"先算一下\"}}]}\n\n")
	}
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"14332031\"},\"finish_reason\":\"stop\"}]}\n\n")
	fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":5}}\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func (q *quirky) sent() []map[string]any {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]map[string]any(nil), q.bodies...)
}

func has(b map[string]any, key string) bool {
	_, ok := b[key]
	return ok
}

// prepareOn 在 quirky 上游上跑准备期
func prepareOn(t *testing.T, q *quirky, params StabilityParams) (Plan, error, RunInput) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(q.handle))
	t.Cleanup(srv.Close)
	if params.Protocol == "" {
		params.Protocol = protocol.ProtocolOpenAIChat
	}
	params.RequestTimeoutMs = 5000
	params.ApplyDefaults()
	if err := params.Validate(); err != nil {
		t.Fatalf("参数校验: %v", err)
	}
	in, _, _, _ := ladderInput(t, srv.URL, params, NewCapGuard(0, 0, 0))
	in.Codec, _ = protocol.Get(params.Protocol)
	in.Nonce = "n0nce123"
	plan, err := PrepareWorkload(context.Background(), in)
	in.Plan = plan
	return plan, err, in
}

// TestPreflightAdaptsChat OpenAI 推理模型式上游：拒收 max_tokens、不认 stream_options。
// 预检按报错改用 max_completion_tokens、去掉 stream_options，之后整个任务都用这套形态
func TestPreflightAdaptsChat(t *testing.T) {
	q := &quirky{reject: func(_ *http.Request, b map[string]any) (int, string) {
		switch {
		case has(b, "max_tokens"):
			return 400, `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.","code":"unsupported_parameter"}}`
		case has(b, "stream_options"):
			return 400, `{"error":{"message":"json: unknown field \"stream_options\""}}`
		}
		return 0, ""
	}}
	plan, err, in := prepareOn(t, q, StabilityParams{ConcurrencyLadder: []int{1}, RequestsPerStage: 3})
	if err != nil {
		t.Fatalf("应自适应通过，却 err = %v", err)
	}
	pf := plan.Preflight
	if !plan.Shape.MaxCompletionTokens || !plan.Shape.NoStreamUsage || len(pf.Adjustments) != 2 || pf.Requests != 3 || !pf.Passed {
		t.Fatalf("形态 = %+v，预检 = %+v，期望改字段 + 去 stream_options、共 3 条", plan.Shape, pf)
	}
	before := len(q.sent())
	if err := runLadder(context.Background(), in); err != nil {
		t.Fatalf("runLadder: %v", err)
	}
	for _, b := range q.sent()[before:] {
		if has(b, "max_tokens") || has(b, "stream_options") || !has(b, "max_completion_tokens") {
			t.Fatalf("压测请求没沿用预检定下的形态: %v", b)
		}
	}
}

// TestPreflightTokenCap 生成上限超过模型允许值：从报错读出上限并钳住
func TestPreflightTokenCap(t *testing.T) {
	q := &quirky{reject: func(_ *http.Request, b map[string]any) (int, string) {
		if n, _ := b["max_tokens"].(float64); n > 8192 {
			return 400, `{"error":{"message":"Invalid max_tokens value, the valid range of max_tokens is [1, 8192]","code":400}}`
		}
		return 0, ""
	}}
	plan, err, _ := prepareOn(t, q, StabilityParams{Workload: Workload{Output: 16384}})
	if err != nil || plan.Shape.MaxTokensCap != 8192 {
		t.Fatalf("cap = %d err = %v，期望钳到 8192", plan.Shape.MaxTokensCap, err)
	}
}

// TestPreflightAutoThinking 自动关思考：thinking.type 被静默忽略、reasoning_effort 被 400、enable_thinking 生效 → 采用第三种
func TestPreflightAutoThinking(t *testing.T) {
	q := &quirky{
		reject: func(_ *http.Request, b map[string]any) (int, string) {
			if has(b, "reasoning_effort") {
				return 400, `{"error":{"message":"Unrecognized request argument supplied: reasoning_effort"}}`
			}
			return 0, ""
		},
		think: func(b map[string]any) bool { return b["enable_thinking"] != false },
	}
	params := StabilityParams{Workload: Workload{Thinking: ThinkingAuto}}
	plan, err, _ := prepareOn(t, q, params)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	pf := plan.Preflight
	want := []string{TrialStillReasoning, TrialRejected, TrialDisabled}
	if len(pf.Trials) != len(want) {
		t.Fatalf("试探 %d 次：%+v，期望 %d 次", len(pf.Trials), pf.Trials, len(want))
	}
	for i, w := range want {
		if pf.Trials[i].Outcome != w {
			t.Errorf("第 %d 次 %s = %s，期望 %s", i, pf.Trials[i].Variant, pf.Trials[i].Outcome, w)
		}
	}
	if plan.Shape.Thinking != protocol.ThinkingEnableFalse || pf.Thinking != protocol.ThinkingEnableFalse || pf.ThinkingField != `{"enable_thinking":false}` {
		t.Errorf("采用 = %q / %q / %q", plan.Shape.Thinking, pf.Thinking, pf.ThinkingField)
	}
	if !pf.Reasoning || pf.Requests != 4 {
		t.Errorf("reasoning=%v requests=%d，期望 true/4", pf.Reasoning, pf.Requests)
	}
}

// TestPreflightThinkingCannotDisable 怎么都关不掉（模型官方就不支持）：不报错，不发思考参数照跑
func TestPreflightThinkingCannotDisable(t *testing.T) {
	q := &quirky{think: func(map[string]any) bool { return true }}
	plan, err, _ := prepareOn(t, q, StabilityParams{Workload: Workload{Thinking: ThinkingAuto}})
	if err != nil {
		t.Fatalf("关不掉思考不该中止任务：%v", err)
	}
	pf := plan.Preflight
	if plan.Shape.Thinking != "" || len(pf.Trials) != len(protocol.ThinkingCandidates(protocol.ProtocolOpenAIChat)) {
		t.Fatalf("形态 = %+v，试探 %d 次", plan.Shape, len(pf.Trials))
	}
	for _, tr := range pf.Trials {
		if tr.Outcome != TrialStillReasoning {
			t.Errorf("%s = %s，期望 still_reasoning", tr.Variant, tr.Outcome)
		}
	}
}

// TestPreflightAutoSkipsWhenNotThinking 模型本来就不思考：auto 不试探、不发任何思考参数
func TestPreflightAutoSkipsWhenNotThinking(t *testing.T) {
	q := &quirky{}
	plan, err, _ := prepareOn(t, q, StabilityParams{Workload: Workload{Thinking: ThinkingAuto}})
	if err != nil || len(plan.Preflight.Trials) != 0 || plan.Shape.Thinking != "" || len(q.sent()) != 1 {
		t.Fatalf("err=%v 试探=%d 形态=%+v 实发=%d，期望只发 1 条预检", err, len(plan.Preflight.Trials), plan.Shape, len(q.sent()))
	}
}

// TestPreflightExplicitThinkingRejected 手动选的关思考写法被上游点名拒收（如新款 Anthropic 模型拒 thinking.disabled）：剔除照跑
func TestPreflightExplicitThinkingRejected(t *testing.T) {
	q := &quirky{reject: func(_ *http.Request, b map[string]any) (int, string) {
		if has(b, "thinking") {
			return 400, `{"type":"error","error":{"type":"invalid_request_error","message":"thinking.type: disabled is not supported for this model"}}`
		}
		return 0, ""
	}}
	plan, err, _ := prepareOn(t, q, StabilityParams{Workload: Workload{Thinking: protocol.ThinkingDisabled}})
	if err != nil || plan.Shape.Thinking != "" || len(plan.Preflight.Adjustments) != 1 {
		t.Fatalf("err=%v 形态=%+v 调整=%v，期望剔除关思考参数后通过", err, plan.Shape, plan.Preflight.Adjustments)
	}
}

// TestPreflightUnnamedRejection 报错没点名是哪个字段：逐项剔除可选部分（关思考 → 自定义字段）直到通过
func TestPreflightUnnamedRejection(t *testing.T) {
	q := &quirky{reject: func(_ *http.Request, b map[string]any) (int, string) {
		if has(b, "thinking") || has(b, "foo") {
			return 400, `{"error":{"message":"invalid request"}}`
		}
		return 0, ""
	}}
	params := StabilityParams{
		Workload: Workload{Thinking: protocol.ThinkingDisabled},
		Compat:   Compat{ExtraBody: map[string]json.RawMessage{"foo": json.RawMessage(`1`)}},
	}
	plan, err, _ := prepareOn(t, q, params)
	if err != nil || plan.Shape.Thinking != "" || len(plan.Shape.ExtraBody) != 0 || len(plan.Preflight.Adjustments) != 2 {
		t.Fatalf("err=%v 形态=%+v 调整=%v", err, plan.Shape, plan.Preflight.Adjustments)
	}
}

// TestPreflightExplicitFallsBackToAuto 指定的 enable_thinking 被上游点名拒收、模型又会思考：
// 剔除后自动探测其余写法（跳过已被拒的那个），thinking.type=disabled 能关就采用
func TestPreflightExplicitFallsBackToAuto(t *testing.T) {
	q := &quirky{
		reject: func(_ *http.Request, b map[string]any) (int, string) {
			if has(b, "enable_thinking") {
				return 400, `{"error":{"message":"json: unknown field \"enable_thinking\""}}`
			}
			return 0, ""
		},
		think: func(b map[string]any) bool {
			th, _ := b["thinking"].(map[string]any)
			return th["type"] != "disabled"
		},
	}
	plan, err, _ := prepareOn(t, q, StabilityParams{Workload: Workload{Thinking: protocol.ThinkingEnableFalse}})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	pf := plan.Preflight
	if plan.Shape.Thinking != protocol.ThinkingDisabled || len(pf.Adjustments) != 1 || len(pf.Trials) != 1 || pf.Requests != 3 {
		t.Fatalf("形态=%+v 调整=%v 试探=%+v 请求=%d，期望剔 enable_thinking 后改用 thinking_disabled、共 3 条", plan.Shape, pf.Adjustments, pf.Trials, pf.Requests)
	}
	if tr := pf.Trials[0]; tr.Variant != protocol.ThinkingDisabled || tr.Outcome != TrialDisabled {
		t.Errorf("试探 = %+v，期望跳过已被拒的 enable_thinking、首个就试 thinking_disabled 并采用", tr)
	}
}

// TestPreflightStreamErrorNamesField 有的网关不在状态码上报参数错误：HTTP 200 后流里吐 error 事件点名
// stream_options。照样针对性去掉；没点名的流内错误（上游过载等）不乱剔，照跑并记下原因
func TestPreflightStreamErrorNamesField(t *testing.T) {
	q := &quirky{reject: func(_ *http.Request, b map[string]any) (int, string) {
		if has(b, "stream_options") {
			return 200, "data: {\"error\":{\"message\":\"unsupported parameter: stream_options\"}}\n\n"
		}
		return 0, ""
	}}
	plan, err, _ := prepareOn(t, q, StabilityParams{})
	if err != nil || !plan.Shape.NoStreamUsage || !plan.Preflight.Passed || len(plan.Preflight.Adjustments) != 1 {
		t.Fatalf("err=%v 形态=%+v 预检=%+v，期望点名后去掉 stream_options 通过", err, plan.Shape, plan.Preflight)
	}

	overloaded := &quirky{reject: func(*http.Request, map[string]any) (int, string) {
		return 200, "data: {\"error\":{\"message\":\"upstream overloaded\"}}\n\n"
	}}
	plan, err, _ = prepareOn(t, overloaded, StabilityParams{})
	if err != nil || plan.Preflight.Passed || len(plan.Preflight.Adjustments) != 0 || !strings.Contains(plan.Preflight.Detail, "overloaded") {
		t.Fatalf("err=%v 预检=%+v，期望不调整、照跑并记下原因", err, plan.Preflight)
	}
}

// TestPreflightRestoresInnocent 报错不点名：按顺序先剔了关思考参数（无辜）、再剔自定义字段（元凶）才通过；
// 回补验证把关思考参数装回去照样通过 → 保留它，调整说明里只剩元凶 + 回补结论
func TestPreflightRestoresInnocent(t *testing.T) {
	q := &quirky{reject: func(_ *http.Request, b map[string]any) (int, string) {
		if has(b, "foo") {
			return 400, `{"error":{"message":"invalid request"}}`
		}
		return 0, ""
	}}
	params := StabilityParams{
		Workload: Workload{Thinking: protocol.ThinkingDisabled},
		Compat:   Compat{ExtraBody: map[string]json.RawMessage{"foo": json.RawMessage(`1`)}},
	}
	plan, err, _ := prepareOn(t, q, params)
	if err != nil || plan.Shape.Thinking != protocol.ThinkingDisabled || len(plan.Shape.ExtraBody) != 0 {
		t.Fatalf("err=%v 形态=%+v，期望保留关思考、只剔 foo", err, plan.Shape)
	}
	adj := plan.Preflight.Adjustments
	if len(adj) != 2 || !strings.Contains(adj[0], "自定义请求体字段") || !strings.Contains(adj[1], "回补") || plan.Preflight.Requests != 4 {
		t.Fatalf("调整=%v 请求数=%d，期望 [剔自定义字段, 回补关思考] 共 4 条", adj, plan.Preflight.Requests)
	}
	if plan.Preflight.Thinking != protocol.ThinkingDisabled {
		t.Errorf("预检结论的关思考写法 = %q", plan.Preflight.Thinking)
	}
}

// TestAdaptAnthropicCacheControl anthropic 兼容网关点名不认 cache_control：改拼纯文本
func TestAdaptAnthropicCacheControl(t *testing.T) {
	var s protocol.Shape
	o := outcome{HTTPStatus: 400, ErrorClass: ErrHTTP4xx, Error: `HTTP 400: {"error":{"message":"messages.0.content.0.cache_control: Extra inputs are not permitted"}}`}
	if note, ok := adapt(&s, Compat{}, protocol.ProtocolAnthropicMessages, o, 16); !ok || !s.PlainContent {
		t.Fatalf("ok=%v note=%q 形态=%+v，期望改纯文本", ok, note, s)
	}
	// 没带共享前缀的任务不剔内容块（预检请求里本来就没有）
	var bare protocol.Shape
	if _, ok := stripUnnamed(&bare, Compat{}, protocol.ProtocolAnthropicMessages, false); ok {
		t.Errorf("无共享前缀时不该盲剔内容块: %+v", bare)
	}
}

// TestPreflightFatal 401/404：渠道根本连不上（key/模型/路径错），快速失败并带上游原文；不去剔字段
func TestPreflightFatal(t *testing.T) {
	for _, code := range []int{401, 404} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			q := &quirky{reject: func(*http.Request, map[string]any) (int, string) {
				return code, `{"error":{"message":"model not found or bad key"}}`
			}}
			_, err, _ := prepareOn(t, q, StabilityParams{Workload: Workload{Thinking: protocol.ThinkingDisabled}})
			if err == nil || !strings.Contains(err.Error(), fmt.Sprint(code)) {
				t.Fatalf("err = %v，期望带 HTTP %d 的快速失败", err, code)
			}
			if n := len(q.sent()); n != 1 {
				t.Errorf("认证/模型错误不该反复剔字段重试（实发 %d）", n)
			}
		})
	}
}

// TestPreflightAnthropicBearer anthropic 兼容网关不认 x-api-key：改用 Bearer 后通过
func TestPreflightAnthropicBearer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":{"message":"missing bearer"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":1}}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"1\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(srv.Close)
	params := StabilityParams{Protocol: protocol.ProtocolAnthropicMessages, RequestTimeoutMs: 5000}
	params.ApplyDefaults()
	in, _, _, _ := ladderInput(t, srv.URL, params, nil)
	in.Codec, _ = protocol.Get(protocol.ProtocolAnthropicMessages)
	plan, err := PrepareWorkload(context.Background(), in)
	if err != nil || !plan.Shape.BearerAuth {
		t.Fatalf("err=%v 形态=%+v，期望改用 Bearer 通过", err, plan.Shape)
	}
}

// TestPreflightRateLimitedContinues 预检一直被限流：不中止（稳定性检测本身就要测限流），记下原因照跑
func TestPreflightRateLimitedContinues(t *testing.T) {
	q := &quirky{reject: func(*http.Request, map[string]any) (int, string) { return 429, "slow down" }}
	plan, err, _ := prepareOn(t, q, StabilityParams{})
	if err != nil || plan.Preflight.Passed || !strings.Contains(plan.Preflight.Detail, "429") {
		t.Fatalf("err=%v 预检=%+v，期望未通过但照跑", err, plan.Preflight)
	}
}

// TestPreflightNonStream 上游无视 stream=true 回整块 JSON：照样算正常应答，记下 nonStream
func TestPreflightNonStream(t *testing.T) {
	q := &quirky{jsonReply: true}
	plan, err, in := prepareOn(t, q, StabilityParams{ConcurrencyLadder: []int{1}, RequestsPerStage: 2})
	if err != nil || !plan.Preflight.NonStream || !plan.Preflight.Passed || !plan.Preflight.UsageReported {
		t.Fatalf("err=%v 预检=%+v", err, plan.Preflight)
	}
	var samples []Sample
	in.Sample = func(_ context.Context, s Sample) error { samples = append(samples, s); return nil }
	if err := runLadder(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	for _, s := range samples {
		if !s.Ok || s.TTFTms >= 0 || s.TotalMs < 0 || s.InputTokens != 30 {
			t.Errorf("非流式样本 = %+v，期望成功、无 TTFT、有耗时与 usage", s)
		}
	}
}

func TestTokenLimit(t *testing.T) {
	cases := []struct {
		msg     string
		current int
		want    int
	}{
		{`http 400: {"error":{"message":"invalid max_tokens value, the valid range of max_tokens is [1, 8192]","code":400}}`, 16384, 8192},
		{`http 400: {"error":{"message":"max_tokens is too large: 50000. this model supports at most 16384 completion tokens, whereas you provided 50000."}}`, 50000, 16384},
		{`http 400: {"type":"error","error":{"message":"max_tokens: 64000 > 32000, which is the maximum allowed number of output tokens"}}`, 64000, 32000},
		{`http 400: {"error":{"message":"max_tokens too large","code":400}}`, 2048, 0}, // 只有状态码，读不出上限
		{`http 400: {"error":{"message":"unknown field foo 9000"}}`, 16384, 0},         // 跟 token 上限无关
	}
	for _, c := range cases {
		if got := tokenLimit(c.msg, c.current); got != c.want {
			t.Errorf("tokenLimit(%q) = %d，期望 %d", c.msg, got, c.want)
		}
	}
}

func TestCompatValidate(t *testing.T) {
	bad := []Compat{
		{MaxTokensField: "max_len", StreamUsage: CompatAuto},
		{MaxTokensField: CompatAuto, StreamUsage: CompatAuto, ExtraBody: map[string]json.RawMessage{"model": json.RawMessage(`"x"`)}},
		{MaxTokensField: CompatAuto, StreamUsage: CompatAuto, ExtraBody: map[string]json.RawMessage{"x": json.RawMessage(`{bad`)}},
		{MaxTokensField: CompatAuto, StreamUsage: CompatAuto, ExtraHeaders: map[string]string{"Content-Length": "1"}},
		{MaxTokensField: CompatAuto, StreamUsage: CompatAuto, ExtraHeaders: map[string]string{"X-A": "a\r\nb"}},
		{MaxTokensField: CompatAuto, StreamUsage: CompatAuto, ExtraHeaders: map[string]string{"bad name": "a"}},
	}
	for i, c := range bad {
		if c.validate() == nil {
			t.Errorf("第 %d 个非法配置应被拒：%+v", i, c)
		}
	}
	ok := Compat{MaxTokensField: CompatAuto, StreamUsage: StreamUsageOff,
		ExtraBody:    map[string]json.RawMessage{"ignore_eos": json.RawMessage(`true`), "temperature": json.RawMessage(`0`)},
		ExtraHeaders: map[string]string{"anthropic-beta": "x", "Authorization": "Bearer y"}}
	if err := ok.validate(); err != nil {
		t.Errorf("合法配置被拒：%v", err)
	}
	if validateThinking(protocol.ThinkingEffortNone, protocol.ProtocolAnthropicMessages) == nil {
		t.Error("anthropic 选 reasoning_effort 应被拒")
	}
	if validateThinking("whatever", protocol.ProtocolOpenAIChat) == nil {
		t.Error("未知思考控制应被拒")
	}
}
