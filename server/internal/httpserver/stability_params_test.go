package httpserver

import (
	"testing"

	"github.com/Yukiho0287/assay/server/internal/api"
	"github.com/Yukiho0287/assay/server/internal/probe/stability"
)

// TestResolveWarmupPerStage 每档预热数：没传 = 契约默认 2（线上经 API 建的任务曾被当成 0），
// 显式传 0 = 不预热（0 是合法取值，不能被默认值吞掉）
func TestResolveWarmupPerStage(t *testing.T) {
	proto := api.Protocol("openai_chat")
	zero, five := 0, 5
	cases := []struct {
		name string
		in   *int
		want int
	}{
		{"缺省取默认", nil, stability.DefaultWarmupPerStage},
		{"显式 0 不预热", &zero, 0},
		{"显式 5", &five, 5},
	}
	for _, c := range cases {
		p, errMsg := resolveStabilityParams(api.StabilityTaskParams{Protocol: &proto, WarmupPerStage: c.in})
		if errMsg != "" {
			t.Fatalf("%s: 不应报错，得 %q", c.name, errMsg)
		}
		if p.WarmupPerStage != c.want {
			t.Errorf("%s: warmupPerStage = %d，期望 %d", c.name, p.WarmupPerStage, c.want)
		}
	}
}
