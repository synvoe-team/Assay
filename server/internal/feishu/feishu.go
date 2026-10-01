// Package feishu 封装飞书 OAuth 授权码登录：拼授权页地址、用 code 换 user_access_token、取用户身份。
// 只做登录所需的三件事，不做通用 SDK。
package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// scopeUserBase 取姓名与头像所需的最小权限，需在开放平台申请并发版生效
const scopeUserBase = "contact:user.base:readonly"

// Client 飞书登录客户端，实例不可变、并发安全
type Client struct {
	appID        string
	appSecret    string
	redirectURL  string
	openBase     string
	accountsBase string
	http         *http.Client
}

// New 构造客户端。域名由调用方按飞书/Lark 版本传入，便于本地 mock 联调。
func New(appID, appSecret, redirectURL, openBase, accountsBase string) *Client {
	return &Client{
		appID:        appID,
		appSecret:    appSecret,
		redirectURL:  redirectURL,
		openBase:     strings.TrimRight(openBase, "/"),
		accountsBase: strings.TrimRight(accountsBase, "/"),
		http:         &http.Client{Timeout: 15 * time.Second},
	}
}

// RedirectURL 回调地址，供日志与自检展示
func (c *Client) RedirectURL() string { return c.redirectURL }

// AuthorizeURL 拼接授权页地址；state 由调用方生成并与 Cookie 比对防 CSRF。
func (c *Client) AuthorizeURL(state string) string {
	q := url.Values{
		"client_id":     {c.appID},
		"redirect_uri":  {c.redirectURL},
		"response_type": {"code"},
		"scope":         {scopeUserBase},
		"state":         {state},
	}
	return c.accountsBase + "/open-apis/authen/v1/authorize?" + q.Encode()
}

// UserInfo 登录所需的用户身份字段
type UserInfo struct {
	// UnionID 同一开发者下跨应用稳定，作平台账号唯一键
	UnionID string
	// OpenID 仅本应用内唯一
	OpenID string
	// Name 用户姓名
	Name string
	// AvatarURL 头像地址
	AvatarURL string
}

// Login 用授权码完成一次登录：换取 user_access_token 后立即取用户身份。
// token 不外传、不落库——平台自己签发会话，飞书令牌用完即弃。
func (c *Client) Login(ctx context.Context, code string) (*UserInfo, error) {
	token, err := c.exchangeCode(ctx, code)
	if err != nil {
		return nil, err
	}
	return c.userInfo(ctx, token)
}

func (c *Client) exchangeCode(ctx context.Context, code string) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     c.appID,
		"client_secret": c.appSecret,
		"code":          code,
		"redirect_uri":  c.redirectURL,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.openBase+"/open-apis/authen/v2/oauth/token", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("构造换取 token 请求: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	var out struct {
		Code             int    `json:"code"`
		Msg              string `json:"msg"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		AccessToken      string `json:"access_token"`
	}
	if err := c.do(req, &out); err != nil {
		return "", err
	}
	// v2 接口成功返回 code=0；失败可能走 code/msg 也可能走 OAuth 标准的 error 字段，两种都认
	if out.Error != "" {
		return "", fmt.Errorf("飞书拒绝授权码: %s（%s）", out.Error, out.ErrorDescription)
	}
	if out.Code != 0 {
		return "", fmt.Errorf("飞书换取 token 失败: code=%d %s", out.Code, out.Msg)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("飞书返回的 access_token 为空")
	}
	return out.AccessToken, nil
}

func (c *Client) userInfo(ctx context.Context, token string) (*UserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.openBase+"/open-apis/authen/v1/user_info", nil)
	if err != nil {
		return nil, fmt.Errorf("构造获取用户信息请求: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Name      string `json:"name"`
			AvatarURL string `json:"avatar_url"`
			OpenID    string `json:"open_id"`
			UnionID   string `json:"union_id"`
		} `json:"data"`
	}
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("飞书获取用户信息失败: code=%d %s", out.Code, out.Msg)
	}
	// union_id 是账号唯一键，缺了就无法安全建号——宁可失败也不退化用 open_id
	if out.Data.UnionID == "" {
		return nil, fmt.Errorf("飞书未返回 union_id，请确认应用已申请 %s 权限并发版生效", scopeUserBase)
	}
	return &UserInfo{
		UnionID:   out.Data.UnionID,
		OpenID:    out.Data.OpenID,
		Name:      strings.TrimSpace(out.Data.Name),
		AvatarURL: out.Data.AvatarURL,
	}, nil
}

// do 发请求并解析 JSON；非 2xx 时把响应体片段带进错误，便于排查
func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("请求飞书失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("读取飞书响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("飞书返回 HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("解析飞书响应失败: %w（%s）", err, truncate(string(body), 200))
	}
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
