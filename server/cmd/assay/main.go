package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/activation"

	"github.com/Yukiho0287/assay/server/internal/auth"
	"github.com/Yukiho0287/assay/server/internal/config"
	"github.com/Yukiho0287/assay/server/internal/connectivity"
	"github.com/Yukiho0287/assay/server/internal/db"
	"github.com/Yukiho0287/assay/server/internal/feishu"
	"github.com/Yukiho0287/assay/server/internal/httpserver"
	"github.com/Yukiho0287/assay/server/internal/tasks"
	"github.com/Yukiho0287/assay/server/internal/update"
	"github.com/Yukiho0287/assay/server/internal/version"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		serve()
	case "version":
		fmt.Println(version.Version)
	case "migrate":
		runMigrate()
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q，可用：serve（默认）| version | migrate pending|up\n", cmd)
		os.Exit(2)
	}
}

// runMigrate 供部署脚本与手工运维使用：
// pending 输出未应用迁移数（0 = 可热重启，>0 = 需备份 + 短停机），up 只跑迁移不起服务。
func runMigrate() {
	sub := ""
	if len(os.Args) > 2 {
		sub = os.Args[2]
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置加载失败:", err)
		os.Exit(1)
	}
	switch sub {
	case "pending":
		n, err := db.PendingMigrations(cfg.DatabaseURL)
		if err != nil {
			fmt.Fprintln(os.Stderr, "检查待应用迁移失败:", err)
			os.Exit(1)
		}
		fmt.Println(n)
	case "up":
		if err := db.Migrate(cfg.DatabaseURL); err != nil {
			fmt.Fprintln(os.Stderr, "执行迁移失败:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "用法: assay migrate pending|up")
		os.Exit(2)
	}
}

func serve() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		log.Error("配置加载失败", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("数据库初始化失败", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := auth.EnsureAdmin(ctx, db.New(pool), log, cfg.AdminPassword); err != nil {
		log.Error("初始管理员引导失败", "err", err)
		os.Exit(1)
	}

	// systemd socket activation：有传入 socket 就复用，重启期间连接由内核排队不被拒绝
	var ln net.Listener
	if listeners, err := activation.Listeners(); err == nil && len(listeners) > 0 {
		ln = listeners[0]
	}

	gh := update.New(cfg.GitHubRepo, cfg.GitHubToken)

	// 飞书登录：未配置则 fs 为 nil，登录页自动只显示密码入口
	ao := httpserver.AuthOptions{LocalLogin: cfg.LocalLogin, CookieSecure: cfg.CookieSecure}
	if cfg.FeishuEnabled() {
		ao.Feishu = feishu.New(cfg.FeishuAppID, cfg.FeishuAppSecret, cfg.FeishuRedirectURL,
			cfg.FeishuOpenBase, cfg.FeishuAccountsBase)
		log.Info("飞书登录已启用", "app_id", cfg.FeishuAppID, "redirect_url", cfg.FeishuRedirectURL)
	}

	// 进程内任务 worker：Start 先清扫孤儿任务再取活；SIGTERM 时 ctx 取消触发软停
	tq, err := tasks.New(pool, log)
	if err != nil {
		log.Error("任务队列初始化失败", "err", err)
		os.Exit(1)
	}
	if err := tq.Start(ctx); err != nil {
		log.Error("任务 worker 启动失败", "err", err)
		os.Exit(1)
	}

	// 定时探活调度器：随 ctx 取消退出，无需显式 Stop
	go connectivity.NewScheduler(pool, log).Run(ctx)

	log.Info("assay 启动", "version", version.Version)
	if err := httpserver.New(cfg.Addr, log, pool, gh, tq, ao).Run(ctx, ln); err != nil {
		log.Error("http server exited", "err", err)
		os.Exit(1)
	}

	// http 已停止接新请求，再等 worker 收尾（SoftStopTimeout 5s 后硬停，重启后自动重跑）
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelStop()
	if err := tq.Stop(stopCtx); err != nil {
		log.Warn("任务 worker 停机未干净退出", "err", err)
	}
	log.Info("shutdown complete")
}
