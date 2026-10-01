package stability

import "testing"

func TestApplyDefaults(t *testing.T) {
	var p StabilityParams
	p.ApplyDefaults()
	if len(p.ConcurrencyLadder) != 5 {
		t.Errorf("默认阶梯 = %v", p.ConcurrencyLadder)
	}
	if p.RequestsPerStage != DefaultRequestsPerStage || p.LadderMaxTokens != DefaultLadderMaxTokens {
		t.Errorf("默认值未落: %+v", p)
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
		{"生成上限越界", func(p *StabilityParams) { p.LadderMaxTokens = 0 }},
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
