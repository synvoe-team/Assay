package httpserver

import "testing"

// safeNext 是挡开放重定向的唯一关口：放行站内相对路径，其余一律丢弃回落首页。
func TestSafeNext(t *testing.T) {
	pass := []string{"/", "/quality", "/quality/abc-123", "/settings?tab=users", "/a/b/c#frag"}
	for _, v := range pass {
		if safeNext(v) != v {
			t.Errorf("站内路径 %q 被误杀", v)
		}
	}
	// 这些都会被浏览器解释成跳到外站或非站内目标
	block := []string{
		"//evil.com",          // 协议相对 URL
		"//evil.com/path",     //
		"/\\evil.com",         // 反斜杠变体，部分浏览器等同于 //
		"https://evil.com",    // 绝对 URL
		"http://evil.com",     //
		"javascript:alert(1)", // 伪协议
		"evil.com",            // 无前导斜杠，会被当相对路径拼到当前目录
		"",                    // 空值
		"\\\\evil.com",        // UNC 风格
	}
	for _, v := range block {
		if got := safeNext(v); got != "" {
			t.Errorf("危险目标 %q 应被拒绝，实际返回 %q", v, got)
		}
	}
}
