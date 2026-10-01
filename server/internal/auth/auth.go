// Package auth 提供会话 token 与初始管理员引导。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"

	"golang.org/x/crypto/bcrypt"

	"github.com/Yukiho0287/assay/server/internal/db"
)

// RandomToken 生成 32 字节密码学随机串（URL 安全），供会话 token 与 OAuth state 共用。
func RandomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成随机 token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// NewSessionToken 生成会话 token：cookie 存随机值，库里只存 SHA-256 哈希。
func NewSessionToken() (token string, hash []byte, err error) {
	token, err = RandomToken()
	if err != nil {
		return "", nil, err
	}
	return token, HashToken(token), nil
}

// HashToken 计算 cookie 中 token 的存库哈希。
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// EnsureAdmin 库里没有任何用户时创建初始 admin（Fail-Fast：失败即阻止启动）。
// 密码优先取 ASSAY_ADMIN_PASSWORD；未设置则随机生成并打印一次日志，绝不硬编码。
func EnsureAdmin(ctx context.Context, q *db.Queries, log *slog.Logger, envPassword string) error {
	n, err := q.CountUsers(ctx)
	if err != nil {
		return fmt.Errorf("统计用户数: %w", err)
	}
	if n > 0 {
		return nil
	}

	password := envPassword
	generated := false
	if password == "" {
		raw := make([]byte, 12)
		if _, err := rand.Read(raw); err != nil {
			return fmt.Errorf("生成初始密码: %w", err)
		}
		password = base64.RawURLEncoding.EncodeToString(raw)
		generated = true
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("哈希初始密码: %w", err)
	}
	// 初始管理员绑定内置 admin 角色（迁移 0002 保证其存在，查不到即 Fail-Fast）
	adminRole, err := q.GetRoleByName(ctx, "admin")
	if err != nil {
		return fmt.Errorf("查询内置 admin 角色: %w", err)
	}
	pwHash := string(hash)
	if _, err := q.CreateUser(ctx, db.CreateUserParams{
		Username:     "admin",
		PasswordHash: &pwHash,
		RoleID:       adminRole.ID,
	}); err != nil {
		return fmt.Errorf("创建初始管理员: %w", err)
	}

	if generated {
		// 初始密码仅此一次打印到日志，请立即保存并尽快修改
		log.Warn("已创建初始管理员，初始密码仅本次打印", "username", "admin", "password", password)
	} else {
		log.Info("已创建初始管理员（密码来自 ASSAY_ADMIN_PASSWORD）", "username", "admin")
	}
	return nil
}
