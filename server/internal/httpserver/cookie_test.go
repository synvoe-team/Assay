package httpserver

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 会话 Cookie 的 Secure 必须跟着请求的实际协议走：
// 经 CDN 的 HTTPS 请求要带 Secure，直连源站明文的管理员兜底通道不能带（带了就登不进去）。
var tlsDummy = tls.ConnectionState{}

func TestSecureCookieFollowsRequestScheme(t *testing.T) {
	cases := []struct {
		name       string
		force      bool
		xfProto    string
		tls        bool
		wantSecure bool
	}{
		{name: "明文直连源站（管理员兜底）", wantSecure: false},
		{name: "经 CDN 的 HTTPS 请求", xfProto: "https", wantSecure: true},
		{name: "X-Forwarded-Proto 大小写不敏感", xfProto: "HTTPS", wantSecure: true},
		{name: "经 CDN 但客户端走的明文", xfProto: "http", wantSecure: false},
		{name: "直连 TLS", tls: true, wantSecure: true},
		{name: "配置强制开启", force: true, wantSecure: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &handlers{cookieSecure: c.force}
			r := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
			if c.xfProto != "" {
				r.Header.Set("X-Forwarded-Proto", c.xfProto)
			}
			if c.tls {
				r.TLS = &tlsDummy
			}
			if got := h.secureCookie(r); got != c.wantSecure {
				t.Errorf("secureCookie = %v, 期望 %v", got, c.wantSecure)
			}
		})
	}
}
