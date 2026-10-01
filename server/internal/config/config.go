// Package config 集中读取并校验环境变量配置。
// Fail-Fast：缺失或非法的关键配置在启动时立即报错，不允许带病运行。
package config

import (
	"fmt"
	"os"
	"strings"
)

// 飞书与国际版 Lark 的接口域名不同：授权页在 accounts 域，接口在 open 域。
var feishuEditions = map[string][2]string{
	"feishu": {"https://open.feishu.cn", "https://accounts.feishu.cn"},
	"lark":   {"https://open.larksuite.com", "https://accounts.larksuite.com"},
}

type Config struct {
	// Addr HTTP 监听地址，如 ":8080"
	Addr string
	// DatabaseURL PostgreSQL 连接串（必填），如 postgres://localhost:5432/assay
	DatabaseURL string
	// AdminPassword 初始管理员密码（可选）；为空时首次启动随机生成并打印日志
	AdminPassword string
	// GitHubRepo 在线更新所查的 GitHub 仓库（owner/repo）
	GitHubRepo string
	// GitHubToken 访问私有仓库 Release 与触发 Actions 的 token（可选；未配置则在线更新降级为不可用）
	GitHubToken string

	// LocalLogin 是否保留用户名密码登录。飞书不可用时管理员靠它兜底进站
	LocalLogin bool
	// CookieSecure 会话 Cookie 是否带 Secure。生产走 HTTPS 必须为 true；
	// 经 CDN 回源时服务端看到的是明文 HTTP，无法自动判断，只能显式配置
	CookieSecure bool

	// FeishuAppID 飞书自建应用 App ID（cli_ 开头）；为空表示不启用飞书登录
	FeishuAppID string
	// FeishuAppSecret 飞书应用 App Secret
	FeishuAppSecret string
	// FeishuRedirectURL 回调地址，必须与开放平台「安全设置」登记的重定向 URL 完全一致
	FeishuRedirectURL string
	// FeishuOpenBase 飞书接口域名（换 token、取用户信息）
	FeishuOpenBase string
	// FeishuAccountsBase 飞书授权页域名
	FeishuAccountsBase string
}

// Load 从环境变量加载配置并校验。新增配置项必须在此处校验。
func Load() (*Config, error) {
	cfg := &Config{
		Addr:          getenv("ASSAY_ADDR", ":8080"),
		DatabaseURL:   os.Getenv("ASSAY_DATABASE_URL"),
		AdminPassword: os.Getenv("ASSAY_ADMIN_PASSWORD"),
		GitHubRepo:    getenv("ASSAY_GITHUB_REPO", "Yukiho0287/Assay"),
		GitHubToken:   os.Getenv("ASSAY_GITHUB_TOKEN"),

		LocalLogin:   getbool("ASSAY_LOCAL_LOGIN", true),
		CookieSecure: getbool("ASSAY_COOKIE_SECURE", false),

		FeishuAppID:       os.Getenv("ASSAY_FEISHU_APP_ID"),
		FeishuAppSecret:   os.Getenv("ASSAY_FEISHU_APP_SECRET"),
		FeishuRedirectURL: os.Getenv("ASSAY_FEISHU_REDIRECT_URL"),
	}
	if cfg.Addr == "" {
		return nil, fmt.Errorf("ASSAY_ADDR 不能为空")
	}
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("必须设置 ASSAY_DATABASE_URL（如 postgres://localhost:5432/assay）")
	}

	edition := strings.ToLower(getenv("ASSAY_FEISHU_EDITION", "feishu"))
	domains, ok := feishuEditions[edition]
	if !ok {
		return nil, fmt.Errorf("ASSAY_FEISHU_EDITION 只能是 feishu 或 lark，当前为 %q", edition)
	}
	cfg.FeishuOpenBase, cfg.FeishuAccountsBase = domains[0], domains[1]
	// 本地 mock 联调用：一个变量同时改写两个域名，生产环境不要设置
	if base := os.Getenv("ASSAY_FEISHU_BASE_URL"); base != "" {
		cfg.FeishuOpenBase, cfg.FeishuAccountsBase = base, base
	}

	// 半套配置比没配置更危险：只填一半直接拒绝启动
	if (cfg.FeishuAppID == "") != (cfg.FeishuAppSecret == "") {
		return nil, fmt.Errorf("ASSAY_FEISHU_APP_ID 与 ASSAY_FEISHU_APP_SECRET 必须同时配置")
	}
	if cfg.FeishuAppID != "" && cfg.FeishuRedirectURL == "" {
		return nil, fmt.Errorf("启用飞书登录必须设置 ASSAY_FEISHU_REDIRECT_URL（须与开放平台登记的重定向 URL 完全一致）")
	}
	// 两条登录通道全关 = 谁都进不来，这种配置不允许启动
	if !cfg.LocalLogin && cfg.FeishuAppID == "" {
		return nil, fmt.Errorf("ASSAY_LOCAL_LOGIN=false 时必须配置飞书登录，否则无人能登录")
	}
	return cfg, nil
}

// FeishuEnabled 是否启用飞书登录
func (c *Config) FeishuEnabled() bool { return c.FeishuAppID != "" }

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getbool 解析布尔开关，未设置时取默认值；非法值按默认值处理会掩盖配置错误，故直接认作 false
func getbool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}
