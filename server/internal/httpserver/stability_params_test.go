package httpserver

import (
	"strings"
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
	value, output, rate, think := 8000, 64, float32(0.3), api.StabilityThinkingAuto
	p, errMsg := resolveStabilityParams(api.StabilityTaskParams{
		Protocol: &proto,
		Workload: &api.StabilityWorkload{
			Input:        &api.StabilityWorkloadInput{Mode: api.Fixed, Value: &value},
			CacheHitRate: &rate,
			Output:       &output,
			Thinking:     &think,
		},
	})
	if errMsg != "" {
		t.Fatalf("不应报错，得 %q", errMsg)
	}
	want := stability.Workload{
		Input:        stability.InputSpec{Mode: stability.InputFixed, Value: 8000},
		CacheHitRate: 0.3,
		Output:       64,
		Thinking:     stability.ThinkingAuto,
	}
	if p.Workload != want {
		t.Errorf("workload = %+v，期望 %+v", p.Workload, want)
	}

	// 没传负载画像 = 现行为：不塑形、h=0、输出沿用各 probe 默认、不干预思考
	p, errMsg = resolveStabilityParams(api.StabilityTaskParams{Protocol: &proto})
	if errMsg != "" || p.Workload != (stability.Workload{Input: stability.InputSpec{Mode: stability.InputNone}, Thinking: stability.ThinkingDefault}) {
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
		{"新任务", `{"protocol":"openai_chat","workload":{"input":{"mode":"jitter","min":1000,"max":3000},"cacheHitRate":0.3,"thinking":"auto"}}`, func(t *testing.T, w *api.StabilityWorkload) {
			if w == nil || w.Input == nil || w.Input.Mode != api.Jitter || *w.Input.Min != 1000 || *w.Input.Max != 3000 ||
				w.CacheHitRate == nil || *w.CacheHitRate != float32(0.3) || w.Output != nil || *w.Thinking != api.StabilityThinkingAuto {
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

// TestResolveCompat 兼容选项 api → 领域参数：自定义字段的值原样转成 JSON 字节；缺省全自动；越界字段创建时就拒
func TestResolveCompat(t *testing.T) {
	proto := api.Protocol("openai_chat")
	field, usage := api.StabilityCompatMaxTokensFieldMaxCompletionTokens, api.StabilityCompatStreamUsageOff
	body := map[string]interface{}{"ignore_eos": true, "temperature": 0, "extra": map[string]interface{}{"a": "<b>"}}
	headers := map[string]string{"X-Title": "assay"}
	cc := true
	p, errMsg := resolveStabilityParams(api.StabilityTaskParams{
		Protocol: &proto,
		Compat:   &api.StabilityCompat{MaxTokensField: &field, StreamUsage: &usage, ChatCacheControl: &cc, ExtraBody: &body, ExtraHeaders: &headers},
	})
	if errMsg != "" {
		t.Fatalf("不应报错，得 %q", errMsg)
	}
	c := p.Compat
	if c.MaxTokensField != stability.FieldMaxCompletionToken || c.StreamUsage != stability.StreamUsageOff || !c.ChatCacheControl || c.ExtraHeaders["X-Title"] != "assay" {
		t.Errorf("compat = %+v", c)
	}
	if string(c.ExtraBody["ignore_eos"]) != "true" || string(c.ExtraBody["temperature"]) != "0" || string(c.ExtraBody["extra"]) != `{"a":"<b>"}` {
		t.Errorf("extraBody = %s / %s / %s", c.ExtraBody["ignore_eos"], c.ExtraBody["temperature"], c.ExtraBody["extra"])
	}

	p, _ = resolveStabilityParams(api.StabilityTaskParams{Protocol: &proto})
	if p.Compat.MaxTokensField != stability.CompatAuto || p.Compat.StreamUsage != stability.CompatAuto {
		t.Errorf("缺省 compat = %+v，期望全自动", p.Compat)
	}

	bad := map[string]interface{}{"model": "x"}
	if _, errMsg = resolveStabilityParams(api.StabilityTaskParams{Protocol: &proto, Compat: &api.StabilityCompat{ExtraBody: &bad}}); errMsg == "" {
		t.Error("自定义字段覆盖 model 应被拒")
	}
}

// TestPreflightFootnotes 预检结论 → 报告脚注：自动调整、关不掉思考（降级、不当配置错误）、缺 usage、非流式、未定标
func TestPreflightFootnotes(t *testing.T) {
	auto := api.StabilityThinkingAuto
	params := api.StabilityTaskParams{Workload: &api.StabilityWorkload{Thinking: &auto}}
	adj := []string{"上游要求 max_completion_tokens，生成上限改用该字段"}
	trials := []api.StabilityThinkingTrial{{Variant: "thinking_disabled", Field: `{"thinking":{"type":"disabled"}}`, Outcome: "still_reasoning"}}
	nonStream := true
	uncal, reason := true, "上游响应不带 usage"
	stages := []api.StabilityStageMetric{{Stage: stability.StageOverall, Metrics: api.StabilityMetrics{
		Preflight: &api.StabilityPreflight{Passed: true, Reasoning: true, UsageReported: false, NonStream: &nonStream, Adjustments: &adj, Trials: &trials},
		Calibration: &api.StabilityCalibration{Uncalibrated: &uncal, Reason: &reason},
	}}}
	notes := strings.Join(preflightFootnotes(params, stages), "\n")
	for _, want := range []string{"max_completion_tokens", "都没关掉", "官方就不支持", "不带 usage", "整块 JSON", "未定标"} {
		if !strings.Contains(notes, want) {
			t.Errorf("脚注缺「%s」：\n%s", want, notes)
		}
	}
	// 旧任务没有预检结论：不出任何预检脚注
	if got := preflightFootnotes(api.StabilityTaskParams{}, []api.StabilityStageMetric{{Stage: stability.StageOverall}}); len(got) != 0 {
		t.Errorf("旧任务不应有预检脚注：%v", got)
	}
	// 思考不干预但模型在思考：提示 TTFT 含思考耗时并给出出路；没思考则不提
	seen := 18
	think := []api.StabilityStageMetric{{Stage: stability.StageOverall, Metrics: api.StabilityMetrics{
		Preflight: &api.StabilityPreflight{Passed: true, Reasoning: true, UsageReported: true}, ReasoningSeen: &seen,
	}}}
	if notes := strings.Join(preflightFootnotes(api.StabilityTaskParams{}, think), "\n"); !strings.Contains(notes, "18 条") || !strings.Contains(notes, "自动探测") {
		t.Errorf("不干预时应提示思考条数与出路：\n%s", notes)
	}
	think[0].Metrics.ReasoningSeen = nil
	if got := preflightFootnotes(api.StabilityTaskParams{}, think); len(got) != 0 {
		t.Errorf("不干预且没思考不该出思考脚注：%v", got)
	}
}
