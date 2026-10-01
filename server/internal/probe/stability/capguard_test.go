package stability

import (
	"testing"
	"time"
)

func TestCapGuardRequests(t *testing.T) {
	c := NewCapGuard(2, 0, 0)
	if !c.Reserve() || !c.Reserve() {
		t.Fatal("前 2 个应放行")
	}
	if c.Reserve() {
		t.Fatal("第 3 个应被请求上限拦下")
	}
	if c.Reason() != CapRequests {
		t.Errorf("Reason=%q，期望 %q", c.Reason(), CapRequests)
	}
}

// Reserve 一并检查 token 上限：调用方只需一道闸
func TestCapGuardTokens(t *testing.T) {
	c := NewCapGuard(0, 10, 0)
	c.AddTokens(11)
	if c.Reserve() {
		t.Fatal("token 超限后应拒绝")
	}
	if c.Reason() != CapTokens {
		t.Errorf("Reason=%q，期望 %q", c.Reason(), CapTokens)
	}
}

func TestCapGuardDuration(t *testing.T) {
	c := NewCapGuard(0, 0, 20*time.Millisecond)
	if !c.Reserve() {
		t.Fatal("截止前应放行")
	}
	time.Sleep(30 * time.Millisecond)
	if c.Reserve() {
		t.Fatal("过了截止时刻应拒绝")
	}
	if c.Reason() != CapDuration {
		t.Errorf("Reason=%q，期望 %q", c.Reason(), CapDuration)
	}
}

// 原因只记第一次触发的那个，之后别的闸再触发也不覆盖
func TestCapGuardFirstReasonSticks(t *testing.T) {
	c := NewCapGuard(1, 10, 0)
	c.Reserve()
	c.Reserve() // requests
	c.AddTokens(100)
	c.Reserve() // tokens 也超了
	if c.Reason() != CapRequests {
		t.Errorf("Reason=%q，期望保持首个 %q", c.Reason(), CapRequests)
	}
}

func TestCapGuardNil(t *testing.T) {
	var c *CapGuard
	if !c.Reserve() || c.Reason() != "" {
		t.Error("nil 硬闸应恒放行、无原因")
	}
}
