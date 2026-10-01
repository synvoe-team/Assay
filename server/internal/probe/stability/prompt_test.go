package stability

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Yukiho0287/assay/server/internal/probe/protocol"
)

func TestUniquePrompt(t *testing.T) {
	got := uniquePrompt("ab12cd34", "r2", 7, "用一句话简要介绍你自己。")
	want := "[ab12cd34 r2-7] 用一句话简要介绍你自己。"
	if got != want {
		t.Errorf("uniquePrompt = %q，期望 %q", got, want)
	}
}

// promptRecorder 记录每个请求 messages[0].content 的 chat 上游
func promptRecorder(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var prompts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) > 0 {
			mu.Lock()
			prompts = append(prompts, body.Messages[0].Content)
			mu.Unlock()
		}
		mockChatStream(w)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), prompts...)
	}
}

// assertUniquePrompts 每条 prompt 带 nonce 前缀、互不相同（破渠道整条响应缓存/去重）
func assertUniquePrompts(t *testing.T, prompts []string, nonce string, wantN int) {
	t.Helper()
	if len(prompts) != wantN {
		t.Fatalf("收到 %d 个请求，期望 %d", len(prompts), wantN)
	}
	seen := map[string]bool{}
	for _, p := range prompts {
		if !strings.HasPrefix(p, "["+nonce+" ") {
			t.Errorf("prompt %q 缺少 nonce 前缀", p)
		}
		if seen[p] {
			t.Errorf("prompt 重复: %q", p)
		}
		seen[p] = true
	}
}

// TestLadderPromptsUnique 阶梯并发：预热 + 计入的每条请求 prompt 都唯一
func TestLadderPromptsUnique(t *testing.T) {
	srv, prompts := promptRecorder(t)
	params := StabilityParams{
		Protocol:          protocol.ProtocolOpenAIChat,
		ConcurrencyLadder: []int{1, 2},
		RequestsPerStage:  3,
		WarmupPerStage:    1,
		RequestTimeoutMs:  5000,
	}
	params.ApplyDefaults()
	in, _, _, _ := ladderInput(t, srv.URL, params, nil)
	in.Nonce = "n0nce123"
	if err := runLadder(context.Background(), in); err != nil {
		t.Fatalf("runLadder 出错: %v", err)
	}
	assertUniquePrompts(t, prompts(), "n0nce123", 8)
}

// TestPacerPromptsUnique 开环档：每条请求 prompt 都唯一
func TestPacerPromptsUnique(t *testing.T) {
	srv, prompts := promptRecorder(t)
	in := pacerInput(srv.URL, NewCapGuard(6, 0), nil)
	in.Nonce = "n0nce123"
	cfg := pacedStageConfig{TargetRate: 100, MaxTokens: 16, Prompt: "hi", Duration: time.Second, MaxInFlight: 64}
	if _, err := runPacedStage(context.Background(), in, 0, "r100", cfg, nil); err != nil {
		t.Fatalf("runPacedStage 出错: %v", err)
	}
	assertUniquePrompts(t, prompts(), "n0nce123", 6)
}
