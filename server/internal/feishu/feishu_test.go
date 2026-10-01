package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// newTestClient 起一个假飞书，返回指向它的客户端
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New("cli_test", "secret_test", "https://assay.example.com/api/auth/feishu/callback", srv.URL, srv.URL)
}

func TestAuthorizeURL(t *testing.T) {
	c := New("cli_test", "s", "https://assay.example.com/api/auth/feishu/callback",
		"https://open.feishu.cn", "https://accounts.feishu.cn")
	raw := c.AuthorizeURL("state-abc")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("授权地址不是合法 URL: %v", err)
	}
	if u.Host != "accounts.feishu.cn" || u.Path != "/open-apis/authen/v1/authorize" {
		t.Errorf("授权页地址错误: %s", raw)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"client_id":     "cli_test",
		"response_type": "code",
		"state":         "state-abc",
		"redirect_uri":  "https://assay.example.com/api/auth/feishu/callback",
		"scope":         scopeUserBase,
	} {
		if got := q.Get(k); got != want {
			t.Errorf("参数 %s = %q, 期望 %q", k, got, want)
		}
	}
}

func TestLoginSuccess(t *testing.T) {
	var gotCode, gotRedirect, gotAuthHeader string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/authen/v2/oauth/token":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotCode, gotRedirect = body["code"], body["redirect_uri"]
			if body["client_id"] != "cli_test" || body["client_secret"] != "secret_test" {
				t.Errorf("凭证未按预期发送: %v", body)
			}
			if body["grant_type"] != "authorization_code" {
				t.Errorf("grant_type = %q", body["grant_type"])
			}
			_, _ = w.Write([]byte(`{"code":0,"access_token":"u-xyz","expires_in":7200,"token_type":"Bearer"}`))
		case "/open-apis/authen/v1/user_info":
			gotAuthHeader = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"code":0,"msg":"success","data":{"name":" 张三 ","avatar_url":"https://a/x.png","open_id":"ou_1","union_id":"on_1"}}`))
		default:
			t.Errorf("请求了预期外的路径 %s", r.URL.Path)
		}
	})

	info, err := c.Login(context.Background(), "code-123")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if gotCode != "code-123" || gotRedirect != c.RedirectURL() {
		t.Errorf("换 token 请求参数错误: code=%q redirect=%q", gotCode, gotRedirect)
	}
	if gotAuthHeader != "Bearer u-xyz" {
		t.Errorf("user_info 未带 user_access_token: %q", gotAuthHeader)
	}
	if info.UnionID != "on_1" || info.OpenID != "ou_1" || info.AvatarURL != "https://a/x.png" {
		t.Errorf("身份字段解析错误: %+v", info)
	}
	if info.Name != "张三" { // 姓名两侧空白必须被裁掉，否则会带进用户名
		t.Errorf("姓名未去空白: %q", info.Name)
	}
}

func TestLoginRejectsBadCode(t *testing.T) {
	// v2 接口失败可能走 OAuth 标准 error 字段，也可能走 code/msg，两种都要认出来
	cases := map[string]string{
		"OAuth 标准错误": `{"error":"invalid_grant","error_description":"code expired"}`,
		"飞书 code/msg": `{"code":20037,"msg":"code has been used"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			})
			if _, err := c.Login(context.Background(), "bad"); err == nil {
				t.Fatal("坏授权码必须报错，静默放行等于任何人都能登录")
			}
		})
	}
}

func TestLoginRejectsMissingUnionID(t *testing.T) {
	// union_id 是账号唯一键，缺了必须失败而不是退化用 open_id 建号
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/oauth/token") {
			_, _ = w.Write([]byte(`{"code":0,"access_token":"u-xyz"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"name":"李四","open_id":"ou_2"}}`))
	})
	_, err := c.Login(context.Background(), "code")
	if err == nil {
		t.Fatal("缺 union_id 必须报错")
	}
	if !strings.Contains(err.Error(), scopeUserBase) {
		t.Errorf("错误提示应指明缺哪个权限，实际: %v", err)
	}
}

func TestLoginSurfacesHTTPError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream down"))
	})
	_, err := c.Login(context.Background(), "code")
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("上游 5xx 必须带状态码报出来，实际: %v", err)
	}
}
