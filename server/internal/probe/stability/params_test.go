package stability

import (
	"strings"
	"testing"
)

func TestApplyDefaults(t *testing.T) {
	var p StabilityParams
	p.ApplyDefaults()
	if len(p.ConcurrencyLadder) != 5 {
		t.Errorf("默认阶梯 = %v", p.ConcurrencyLadder)
	}
	if p.RequestsPerStage != DefaultRequestsPerStage {
		t.Errorf("默认值未落: %+v", p)
	}
	// 负载画像缺省 = 不塑形输入、h=0、输出沿用各 probe 默认
	if p.Workload.Input.Mode != InputNone || p.Workload.CacheHitRate != 0 || p.Workload.Output != 0 {
		t.Errorf("负载画像默认 = %+v", p.Workload)
	}
	if p.MaxTotalRequests != DefaultMaxTotalRequests || p.RequestTimeoutMs != DefaultRequestTimeoutMs {
		t.Errorf("硬闸默认未落: %+v", p)
	}
	// WarmupPerStage=0 合法，不应被补默认
	if p.WarmupPerStage != 0 {
		t.Errorf("WarmupPerStage 应保持 0，得 %d", p.WarmupPerStage)
	}
}

func TestValidate(t *testing.T) {
	good := StabilityParams{Protocol: "openai_chat"}
	good.ApplyDefaults()
	if err := good.Validate(); err != nil {
		t.Errorf("默认参数应合法: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*StabilityParams)
	}{
		{"缺协议", func(p *StabilityParams) { p.Protocol = "" }},
		{"空阶梯", func(p *StabilityParams) { p.ConcurrencyLadder = nil }},
		{"并发越界", func(p *StabilityParams) { p.ConcurrencyLadder = []int{0} }},
		{"每档请求越界", func(p *StabilityParams) { p.RequestsPerStage = 0 }},
		{"预热为负", func(p *StabilityParams) { p.WarmupPerStage = -1 }},
		{"超时越界", func(p *StabilityParams) { p.RequestTimeoutMs = 10 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := StabilityParams{Protocol: "openai_chat"}
			p.ApplyDefaults()
			c.mut(&p)
			if err := p.Validate(); err == nil {
				t.Errorf("%s 应校验失败", c.name)
			}
		})
	}
}

func TestEstLadderRequests(t *testing.T) {
	p := StabilityParams{ConcurrencyLadder: []int{1, 2, 4}, RequestsPerStage: 20, WarmupPerStage: 2}
	if got := estLadderRequests(p); got != 3*22 {
		t.Errorf("est = %d，期望 66", got)
	}
}

// TestValidateWorkload 负载画像 fail-fast：非法组合在创建任务时拒绝并说明原因
func TestValidateWorkload(t *testing.T) {
	fixed := func(v int) InputSpec { return InputSpec{Mode: InputFixed, Value: v} }
	ok := []struct {
		name string
		mut  func(*StabilityParams)
	}{
		{"固定输入", func(p *StabilityParams) { p.Workload.Input = fixed(8000) }},
		{"递增输入", func(p *StabilityParams) { p.Workload.Input = InputSpec{Mode: InputRamp, Min: 1000, Max: 4000} }},
		{"命中率 0.5 共享前缀 4000", func(p *StabilityParams) { p.Workload.Input = fixed(8000); p.Workload.CacheHitRate = 0.5 }},
		{"命中率恰好 1024", func(p *StabilityParams) { p.Workload.Input = fixed(2048); p.Workload.CacheHitRate = 0.5 }},
		{"输出目标", func(p *StabilityParams) { p.Workload.Output = 128 }},
		{"大输入配够超时", func(p *StabilityParams) { p.Workload.Input = fixed(150000); p.RequestTimeoutMs = 150000 }},
		{"不塑形时超时仍可低于 60 秒", func(p *StabilityParams) { p.RequestTimeoutMs = 30000 }},
	}
	for _, c := range ok {
		t.Run("ok/"+c.name, func(t *testing.T) {
			p := StabilityParams{Protocol: "openai_chat"}
			p.ApplyDefaults()
			c.mut(&p)
			if err := p.Validate(); err != nil {
				t.Errorf("应合法: %v", err)
			}
		})
	}

	bad := []struct {
		name string
		mut  func(*StabilityParams)
		want string // 错误文案须点明原因
	}{
		{"未知输入模式", func(p *StabilityParams) { p.Workload.Input = InputSpec{Mode: "huge"} }, "输入模式"},
		{"固定值为 0", func(p *StabilityParams) { p.Workload.Input = fixed(0) }, "输入 token"},
		{"区间下限不小于上限", func(p *StabilityParams) { p.Workload.Input = InputSpec{Mode: InputRamp, Min: 4000, Max: 4000} }, "区间"},
		{"命中率越界", func(p *StabilityParams) { p.Workload.Input = fixed(8000); p.Workload.CacheHitRate = 0.99 }, "命中率"},
		{"命中率为负", func(p *StabilityParams) { p.Workload.CacheHitRate = -0.1 }, "命中率"},
		{"命中率无输入塑形", func(p *StabilityParams) { p.Workload.CacheHitRate = 0.5 }, "输入"},
		{"共享前缀不足 1024", func(p *StabilityParams) { p.Workload.Input = fixed(1000); p.Workload.CacheHitRate = 0.5 }, "1024"},
		{"最小请求装不下共享前缀", func(p *StabilityParams) {
			p.Workload.Input = InputSpec{Mode: InputRamp, Min: 1000, Max: 3000}
			p.Workload.CacheHitRate = 0.55
		}, "共享前缀"},
		{"输出越界", func(p *StabilityParams) { p.Workload.Output = 40000 }, "输出"},
		{"输出为负", func(p *StabilityParams) { p.Workload.Output = -1 }, "输出"},
		{"单请求名义 token 超总上限", func(p *StabilityParams) {
			p.Workload.Input = fixed(100000)
			p.RequestTimeoutMs = 100000
			p.MaxTotalTokens = 50000
		}, "总 token"},
		{"大输入超时不够", func(p *StabilityParams) { p.Workload.Input = fixed(150000) }, "150000ms"},
		{"塑形时超时低于 60 秒", func(p *StabilityParams) { p.Workload.Input = fixed(2000); p.RequestTimeoutMs = 30000 }, "60000ms"},
	}
	for _, c := range bad {
		t.Run("bad/"+c.name, func(t *testing.T) {
			p := StabilityParams{Protocol: "openai_chat"}
			p.ApplyDefaults()
			c.mut(&p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("应校验失败")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误 %q 应包含 %q", err, c.want)
			}
		})
	}
}
