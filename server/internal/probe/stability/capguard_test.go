package stability

import (
	"testing"
	"time"
)

func TestCapGuardRequests(t *testing.T) {
	c := NewCapGuard(2, 0, 0)
	if !c.Reserve(0) || !c.Reserve(0) {
		t.Fatal("前 2 个应放行")
	}
	if c.Reserve(0) {
		t.Fatal("第 3 个应被请求上限拦下")
	}
	if c.Reason() != CapRequests {
		t.Errorf("Reason=%q，期望 %q", c.Reason(), CapRequests)
	}
}

// Reserve 一并检查 token 上限：调用方只需一道闸；Settle 按实测补差
func TestCapGuardTokens(t *testing.T) {
	c := NewCapGuard(0, 10, 0)
	if !c.Reserve(5) {
		t.Fatal("预扣 5 ≤ 上限 10 应放行")
	}
	c.Settle(5, 11) // 实测 11：补差后累计 11
	if c.Reserve(0) {
		t.Fatal("token 超限后应拒绝")
	}
	if c.Reason() != CapTokens {
		t.Errorf("Reason=%q，期望 %q", c.Reason(), CapTokens)
	}
}

func TestCapGuardDuration(t *testing.T) {
	c := NewCapGuard(0, 0, 20*time.Millisecond)
	if !c.Reserve(0) {
		t.Fatal("截止前应放行")
	}
	time.Sleep(30 * time.Millisecond)
	if c.Reserve(0) {
		t.Fatal("过了截止时刻应拒绝")
	}
	if c.Reason() != CapDuration {
		t.Errorf("Reason=%q，期望 %q", c.Reason(), CapDuration)
	}
}

// 原因只记第一次触发的那个，之后别的闸再触发也不覆盖
func TestCapGuardFirstReasonSticks(t *testing.T) {
	c := NewCapGuard(1, 10, 0)
	c.Reserve(0)
	c.Reserve(0) // requests
	c.Settle(0, 100)
	c.Reserve(0) // tokens 也超了
	if c.Reason() != CapRequests {
		t.Errorf("Reason=%q，期望保持首个 %q", c.Reason(), CapRequests)
	}
}

func TestCapGuardNil(t *testing.T) {
	var c *CapGuard
	c.Settle(1, 2)
	if !c.Reserve(1) || c.Reason() != "" {
		t.Error("nil 硬闸应恒放行、无原因")
	}
}

// 预扣：大输入请求在发出前就按名义 token 占额度，在途一批不会把总 token 硬闸冲穿
func TestCapGuardPreDeduct(t *testing.T) {
	c := NewCapGuard(0, 100, 0)
	if !c.Reserve(60) {
		t.Fatal("首个 60 应放行")
	}
	if c.Reserve(60) {
		t.Fatal("在途已预扣 60，再预扣 60 会超 100，应拦下")
	}
	if c.Reason() != CapTokens {
		t.Errorf("Reason=%q，期望 %q", c.Reason(), CapTokens)
	}
	c.Settle(60, 0) // 请求失败无 usage：退回预扣
	if !c.Reserve(60) {
		t.Fatal("退回预扣后应能再放行")
	}
}
