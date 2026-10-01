package stability

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Yukiho0287/assay/server/internal/probe/protocol"
)

// replayServer 原样回放一段 SSE 正文（HTTP 200）。
func replayServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDoRequestClassify 单请求判定：HTTP 200 之后按「结束帧 → 正文 → 预算 → 推理 → 空」逐级分类。
func TestDoRequestClassify(t *testing.T) {
	const maxTokens = 64
	cases := []struct {
		name      string
		body      string
		wantOk    bool
		wantClass string
		wantInMsg string
	}{
		{"正常：正文 + 结束帧", `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}

data: [DONE]
`, true, "", ""},
		{"正文后被生成上限截断仍算成功", `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"length"}]}

data: [DONE]
`, true, "", ""},
		{"200 后流被掐断（无结束帧）", `data: {"choices":[{"delta":{"content":"hi"}}]}
`, false, ErrStreamAnomaly, "无结束帧"},
		{"流内错误事件", `data: {"choices":[{"delta":{"content":"hi"}}]}

data: {"error":{"type":"server_error","message":"upstream reset"}}
`, false, ErrStreamAnomaly, "server_error"},
		{"推理把预算烧光（finish_reason=length）", `data: {"choices":[{"delta":{"reasoning_content":"想"}}]}

data: {"choices":[{"delta":{},"finish_reason":"length"}]}

data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":64}}

data: [DONE]
`, false, ErrBudgetExhausted, "64"},
		{"隐藏推理烧光预算（无增量，靠 completion 计数兜底）", `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":64}}

data: [DONE]
`, false, ErrBudgetExhausted, "64"},
		{"只有推理没有正文就结束（Error-Only-Reasoning）", `data: {"choices":[{"delta":{"reasoning_content":"想"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":20}}

data: [DONE]
`, false, ErrReasoningOnly, ""},
		{"无任何增量", `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":0}}

data: [DONE]
`, false, ErrSemanticEmpty, ""},
	}
	codec, _ := protocol.Get(protocol.ProtocolOpenAIChat)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replayServer(t, tc.body)
			o := doRequest(context.Background(), srv.Client(), codec, srv.URL, "sk", "m", "ping", maxTokens, 5000)
			if o.Ok != tc.wantOk || o.ErrorClass != tc.wantClass {
				t.Fatalf("Ok=%v class=%q，期望 Ok=%v class=%q（error=%q）", o.Ok, o.ErrorClass, tc.wantOk, tc.wantClass, o.Error)
			}
			if tc.wantInMsg != "" && !strings.Contains(o.Error, tc.wantInMsg) {
				t.Errorf("error=%q 应包含 %q", o.Error, tc.wantInMsg)
			}
			if o.HTTPProto != "HTTP/1.1" {
				t.Errorf("HTTPProto=%q，期望 HTTP/1.1", o.HTTPProto)
			}
		})
	}
}

// TestDoRequestTimings 推理模型的首增量（TTFD）先于首正文（TTFT），两者都要打点；cached 透传到样本。
func TestDoRequestTimings(t *testing.T) {
	srv := replayServer(t, `data: {"choices":[{"delta":{"reasoning_content":"想"}}]}

data: {"choices":[{"delta":{"content":"答"},"finish_reason":"stop"}]}

data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4}}}

data: [DONE]
`)
	codec, _ := protocol.Get(protocol.ProtocolOpenAIChat)
	o := doRequest(context.Background(), srv.Client(), codec, srv.URL, "sk", "m", "ping", 64, 5000)
	if !o.Ok || !o.HasTTFD || !o.HasTTFT {
		t.Fatalf("Ok=%v HasTTFD=%v HasTTFT=%v，期望均为 true（error=%q）", o.Ok, o.HasTTFD, o.HasTTFT, o.Error)
	}
	if o.TTFD > o.TTFT {
		t.Errorf("TTFD=%v 不应晚于 TTFT=%v", o.TTFD, o.TTFT)
	}
	s := sampleFrom("c1", 0, 0, false, time.Now(), protocol.ProtocolOpenAIChat, o)
	if s.CachedTokens != 4 || s.TTFDms < 0 || s.HTTPProto != "HTTP/1.1" {
		t.Errorf("样本 CachedTokens=%d TTFDms=%d HTTPProto=%q", s.CachedTokens, s.TTFDms, s.HTTPProto)
	}
}

// TestSampleKeepsEvidenceOnFailure 失败但拿到 200 的样本（如预算耗尽）保留已测到的计量与时序，作证据链
func TestSampleKeepsEvidenceOnFailure(t *testing.T) {
	srv := replayServer(t, `data: {"choices":[{"delta":{"reasoning_content":"想"}}]}

data: {"choices":[{"delta":{},"finish_reason":"length"}]}

data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":64}}

data: [DONE]
`)
	codec, _ := protocol.Get(protocol.ProtocolOpenAIChat)
	o := doRequest(context.Background(), srv.Client(), codec, srv.URL, "sk", "m", "ping", 64, 5000)
	s := sampleFrom("c1", 0, 0, false, time.Now(), protocol.ProtocolOpenAIChat, o)
	if s.Ok || s.ErrorClass != ErrBudgetExhausted {
		t.Fatalf("Ok=%v class=%q", s.Ok, s.ErrorClass)
	}
	if s.OutputTokens != 64 || s.TTFDms < 0 || s.TotalMs < 0 {
		t.Errorf("预算耗尽样本应保留 OutputTokens=64 与时序，得 out=%d ttfd=%d total=%d", s.OutputTokens, s.TTFDms, s.TotalMs)
	}
	if s.TTFTms >= 0 {
		t.Errorf("无正文时 TTFTms 应缺省（<0），得 %d", s.TTFTms)
	}
}
