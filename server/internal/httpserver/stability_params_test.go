package httpserver

import (
	"testing"

	"github.com/Yukiho0287/assay/server/internal/api"
	"github.com/Yukiho0287/assay/server/internal/db"
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

// TestResolveWorkload 负载画像 api → 领域参数：逐字段映射；命中率 float32 去掉二进制尾差（0.3 不能变成 0.30000001）
func TestResolveWorkload(t *testing.T) {
	proto := api.Protocol("openai_chat")
	value, output, rate, off := 8000, 64, float32(0.3), true
	p, errMsg := resolveStabilityParams(api.StabilityTaskParams{
		Protocol: &proto,
		Workload: &api.StabilityWorkload{
			Input:           &api.StabilityWorkloadInput{Mode: api.Fixed, Value: &value},
			CacheHitRate:    &rate,
			Output:          &output,
			DisableThinking: &off,
		},
	})
	if errMsg != "" {
		t.Fatalf("不应报错，得 %q", errMsg)
	}
	want := stability.Workload{
		Input:           stability.InputSpec{Mode: stability.InputFixed, Value: 8000},
		CacheHitRate:    0.3,
		Output:          64,
		DisableThinking: true,
	}
	if p.Workload != want {
		t.Errorf("workload = %+v，期望 %+v", p.Workload, want)
	}

	// 没传负载画像 = 现行为：不塑形、h=0、输出沿用各 probe 默认
	p, errMsg = resolveStabilityParams(api.StabilityTaskParams{Protocol: &proto})
	if errMsg != "" || p.Workload != (stability.Workload{Input: stability.InputSpec{Mode: stability.InputNone}}) {
		t.Errorf("缺省 workload = %+v（err=%q）", p.Workload, errMsg)
	}

	// 非法组合在创建时就拒（fail-fast），不带进执行期
	lo, hi := 1000, 3000
	bad := float32(0.55)
	_, errMsg = resolveStabilityParams(api.StabilityTaskParams{
		Protocol: &proto,
		Workload: &api.StabilityWorkload{
			Input:        &api.StabilityWorkloadInput{Mode: api.Ramp, Min: &lo, Max: &hi},
			CacheHitRate: &bad,
		},
	})
	if errMsg == "" {
		t.Error("最小请求装不下共享前缀应被拒")
	}
}

// TestStabilityTaskToAPIWorkload 快照里的负载画像原样回给前端（含迁移 0009 改写后的旧任务形态）
func TestStabilityTaskToAPIWorkload(t *testing.T) {
	cases := []struct {
		name   string
		params string
		check  func(t *testing.T, w *api.StabilityWorkload)
	}{
		{"迁移改写的旧任务：只有输出目标", `{"protocol":"openai_chat","workload":{"output":64}}`, func(t *testing.T, w *api.StabilityWorkload) {
			if w == nil || w.Output == nil || *w.Output != 64 || w.Input != nil {
				t.Errorf("workload = %+v，期望只有 output=64", w)
			}
		}},
		{"迁移前默认的旧任务：无负载画像", `{"protocol":"openai_chat"}`, func(t *testing.T, w *api.StabilityWorkload) {
			if w != nil {
				t.Errorf("workload = %+v，期望缺省", w)
			}
		}},
		{"新任务", `{"protocol":"openai_chat","workload":{"input":{"mode":"jitter","min":1000,"max":3000},"cacheHitRate":0.3,"disableThinking":true}}`, func(t *testing.T, w *api.StabilityWorkload) {
			if w == nil || w.Input == nil || w.Input.Mode != api.Jitter || *w.Input.Min != 1000 || *w.Input.Max != 3000 ||
				w.CacheHitRate == nil || *w.CacheHitRate != float32(0.3) || w.Output != nil || !*w.DisableThinking {
				t.Errorf("workload = %+v", w)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			task, err := stabilityTaskToAPI(db.GetTaskRow{Target: []byte(`{}`), Params: []byte(c.params)})
			if err != nil {
				t.Fatalf("stabilityTaskToAPI: %v", err)
			}
			c.check(t, task.Params.Workload)
		})
	}
}
