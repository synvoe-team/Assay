package config

import (
	"strings"
	"testing"
)

// setEnv 用 t.Setenv 铺一套最小可用配置，再按用例覆盖
func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ASSAY_DATABASE_URL", "postgres://localhost:5432/assay")
	t.Setenv("ASSAY_FEISHU_APP_ID", "")
	t.Setenv("ASSAY_FEISHU_APP_SECRET", "")
	t.Setenv("ASSAY_FEISHU_REDIRECT_URL", "")
	t.Setenv("ASSAY_FEISHU_EDITION", "")
	t.Setenv("ASSAY_FEISHU_BASE_URL", "")
	t.Setenv("ASSAY_LOCAL_LOGIN", "")
}

func TestDefaultsToPasswordOnlyLogin(t *testing.T) {
	baseEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("最小配置应能启动: %v", err)
	}
	if cfg.FeishuEnabled() {
		t.Error("未配置飞书时不应启用飞书登录")
	}
	if !cfg.LocalLogin {
		t.Error("密码登录默认应开启，否则本地开发无法进站")
	}
	if cfg.FeishuOpenBase != "https://open.feishu.cn" {
		t.Errorf("默认版本应为飞书，实际 %s", cfg.FeishuOpenBase)
	}
}

func TestHalfConfiguredFeishuIsRejected(t *testing.T) {
	baseEnv(t)
	t.Setenv("ASSAY_FEISHU_APP_ID", "cli_x")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "同时配置") {
		t.Fatalf("只填 app_id 必须拒绝启动，实际: %v", err)
	}
}

func TestFeishuWithoutRedirectURLIsRejected(t *testing.T) {
	baseEnv(t)
	t.Setenv("ASSAY_FEISHU_APP_ID", "cli_x")
	t.Setenv("ASSAY_FEISHU_APP_SECRET", "s")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "REDIRECT_URL") {
		t.Fatalf("缺回调地址必须拒绝启动，实际: %v", err)
	}
}

func TestBothLoginChannelsOffIsRejected(t *testing.T) {
	baseEnv(t)
	t.Setenv("ASSAY_LOCAL_LOGIN", "false")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "无人能登录") {
		t.Fatalf("两条登录通道全关必须拒绝启动，实际: %v", err)
	}
}

func TestLarkEdition(t *testing.T) {
	baseEnv(t)
	t.Setenv("ASSAY_FEISHU_EDITION", "lark")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("lark 版本应被接受: %v", err)
	}
	if cfg.FeishuAccountsBase != "https://accounts.larksuite.com" {
		t.Errorf("lark 授权域名错误: %s", cfg.FeishuAccountsBase)
	}
}

func TestUnknownEditionIsRejected(t *testing.T) {
	baseEnv(t)
	t.Setenv("ASSAY_FEISHU_EDITION", "wechat")
	if _, err := Load(); err == nil {
		t.Fatal("未知版本必须拒绝启动")
	}
}
