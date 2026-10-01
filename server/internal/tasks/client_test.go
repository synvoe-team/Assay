package tasks

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStabilityClientForcesHTTP1 稳定性压测连接池固定 HTTP/1.1：对齐官方 SDK 默认行为与手工基线口径，
// 避免 h2 把所有并发挤进一条连接（大请求体还会被 h2 流控窗口拖慢上传）。
// 对照组用普通连接池，证明测试服务端确实提供 h2。
func TestStabilityClientForcesHTTP1(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	proto := func(c *http.Client) string {
		t.Helper()
		c.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool}
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		resp.Body.Close()
		return resp.Proto
	}

	if got := proto(workerHTTPClient()); got != "HTTP/2.0" {
		t.Fatalf("对照组应协商到 HTTP/2.0，得 %s（测试服务端没开 h2？）", got)
	}
	if got := proto(stabilityHTTPClient()); got != "HTTP/1.1" {
		t.Errorf("稳定性连接池应固定 HTTP/1.1，得 %s", got)
	}
}
