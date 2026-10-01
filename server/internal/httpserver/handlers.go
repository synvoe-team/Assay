package httpserver

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/Yukiho0287/assay/server/internal/api"
	"github.com/Yukiho0287/assay/server/internal/auth"
	"github.com/Yukiho0287/assay/server/internal/db"
	"github.com/Yukiho0287/assay/server/internal/feishu"
	"github.com/Yukiho0287/assay/server/internal/tasks"
	"github.com/Yukiho0287/assay/server/internal/update"
	"github.com/Yukiho0287/assay/server/internal/version"
)

const (
	sessionCookie = "assay_session"
	sessionTTL    = 7 * 24 * time.Hour
	// oauthStateCookie 一次性 CSRF state；只在回调路径下发送，5 分钟够走完授权
	oauthStateCookie = "assay_oauth_state"
	oauthStatePath   = "/api/auth/feishu"
	oauthStateTTL    = 5 * time.Minute
)

// dummyHash 用于用户不存在时的空比对，抹平响应时间差异，防止用户名枚举
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("assay-timing-dummy"), bcrypt.DefaultCost)

// handlers 实现 api.ServerInterface；所有业务端点在此实现。
type handlers struct {
	log    *slog.Logger
	q      *db.Queries
	pool   *pgxpool.Pool // 仅用于需要跨语句事务的端点（如创建任务时任务行+入队同事务）
	gh     *update.Client
	tq     *tasks.Client
	broker *taskEventBroker

	fs           *feishu.Client // nil = 未配置飞书登录
	localLogin   bool           // 是否放行用户名密码登录（管理员兜底通道）
	cookieSecure bool           // 强制所有会话 Cookie 带 Secure（不管请求走的什么协议）
}

var _ api.ServerInterface = (*handlers)(nil)

// session 当前请求的会话上下文：用户、角色权限与本会话 token 哈希
type session struct {
	db.GetSessionUserRow
	perms     api.PermissionMap
	tokenHash []byte
}

// requireAuth 解析会话；未登录统一 401。受保护端点的第一行都从这里开始。
func (h *handlers) requireAuth(w http.ResponseWriter, r *http.Request) (session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		writeJSON(w, http.StatusUnauthorized, api.Error{Error: "未登录或会话已过期"})
		return session{}, false
	}
	hash := auth.HashToken(c.Value)
	u, err := h.q.GetSessionUser(r.Context(), hash)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, api.Error{Error: "未登录或会话已过期"})
		return session{}, false
	}
	s := session{GetSessionUserRow: u, tokenHash: hash}
	if err := json.Unmarshal(u.Permissions, &s.perms); err != nil {
		h.log.Error("角色权限数据损坏", "role", u.RoleName, "err", err)
		writeJSON(w, http.StatusInternalServerError, api.Error{Error: "服务内部错误"})
		return session{}, false
	}
	return s, true
}

// requirePerm 模块级门禁：登录 + 对应模块开关开启才放行
func (h *handlers) requirePerm(w http.ResponseWriter, r *http.Request, module string) (session, bool) {
	s, ok := h.requireAuth(w, r)
	if !ok {
		return session{}, false
	}
	if !hasModule(s.perms, module) {
		writeJSON(w, http.StatusForbidden, api.Error{Error: "无权访问该模块"})
		return session{}, false
	}
	return s, true
}

func hasModule(p api.PermissionMap, module string) bool {
	switch module {
	case "channels":
		return p.Channels
	case "quality":
		return p.Quality
	case "stability":
		return p.Stability
	case "users":
		return p.Users
	case "system":
		return p.System
	}
	return false
}

func (h *handlers) GetHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, api.Health{Status: api.Ok})
}

func (h *handlers) GetVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, api.VersionInfo{Version: version.Version})
}

func (h *handlers) Login(w http.ResponseWriter, r *http.Request) {
	if !h.localLogin {
		writeJSON(w, http.StatusForbidden, api.Error{Error: "本站已关闭密码登录，请使用飞书登录"})
		return
	}
	var req api.LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: "用户名和密码不能为空"})
		return
	}

	u, err := h.q.GetUserByUsername(r.Context(), req.Username)
	// 飞书自动建号的账号 password_hash 为 NULL，同样按「用户名或密码错误」处理，
	// 且照走一次空比对，不让响应时间泄露账号是否存在、是否为飞书账号
	if err != nil || u.PasswordHash == nil {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(req.Password))
		writeJSON(w, http.StatusUnauthorized, api.Error{Error: "用户名或密码错误"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(*u.PasswordHash), []byte(req.Password)) != nil {
		writeJSON(w, http.StatusUnauthorized, api.Error{Error: "用户名或密码错误"})
		return
	}

	if !h.issueSession(w, r, u.ID) {
		return
	}
	h.log.Info("用户登录", "username", u.Username, "method", "password")
	w.WriteHeader(http.StatusNoContent)
}

// secureCookie 判定本次响应的 Cookie 该不该带 Secure。
// 配置项为强制开关；否则看请求实际是不是 HTTPS —— 直连是 r.TLS，
// 经 CDN/反代则看 X-Forwarded-Proto。伪造这个头只会让伪造者自己的 Cookie 发不出去，
// 不构成风险，所以不必校验来源。
func (h *handlers) secureCookie(r *http.Request) bool {
	if h.cookieSecure || r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// issueSession 写会话行并下发 Cookie；失败时已向 w 写过 500，调用方直接返回。
// 密码登录与飞书登录共用同一套会话机制，登录方式不影响会话行为。
func (h *handlers) issueSession(w http.ResponseWriter, r *http.Request, userID uuid.UUID) bool {
	token, hash, err := auth.NewSessionToken()
	if err != nil {
		h.log.Error("生成会话失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, api.Error{Error: "服务内部错误"})
		return false
	}
	if err := h.q.CreateSession(r.Context(), db.CreateSessionParams{
		TokenHash: hash,
		UserID:    userID,
		ExpiresAt: time.Now().Add(sessionTTL),
	}); err != nil {
		h.log.Error("写入会话失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, api.Error{Error: "服务内部错误"})
		return false
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   h.secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
	return true
}

func (h *handlers) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := h.q.DeleteSession(r.Context(), auth.HashToken(c.Value)); err != nil {
			h.log.Error("删除会话失败", "err", err)
		}
	}
	// 无论此前是否持有有效会话，都清 cookie 并返回 204（登出幂等）
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) GetCurrentUser(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireAuth(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, api.CurrentUser{
		Id:          s.ID,
		Username:    s.Username,
		DisplayName: s.DisplayName,
		AvatarUrl:   s.AvatarUrl,
		Role:        s.RoleName,
		Permissions: s.perms,
	})
}

func (h *handlers) ChangeOwnPassword(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireAuth(w, r)
	if !ok {
		return
	}
	var req api.ChangePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.CurrentPassword == "" {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: "请求格式错误"})
		return
	}
	if len(req.NewPassword) < 8 {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: "新密码至少 8 位"})
		return
	}

	hash, err := h.q.GetUserPasswordHash(r.Context(), s.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, api.Error{Error: "服务内部错误"})
		return
	}
	if hash == nil {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: "飞书登录账号没有密码，无需也无法修改"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(*hash), []byte(req.CurrentPassword)) != nil {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: "当前密码错误"})
		return
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		h.log.Error("哈希新密码失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, api.Error{Error: "服务内部错误"})
		return
	}
	newHashStr := string(newHash)
	if _, err := h.q.UpdateUserPassword(r.Context(), db.UpdateUserPasswordParams{
		ID:           s.ID,
		PasswordHash: &newHashStr,
	}); err != nil {
		h.log.Error("更新密码失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, api.Error{Error: "服务内部错误"})
		return
	}
	// 改密后注销本人其他会话，只保留当前这一个
	if err := h.q.DeleteOtherUserSessions(r.Context(), db.DeleteOtherUserSessionsParams{
		UserID:    s.ID,
		TokenHash: s.tokenHash,
	}); err != nil {
		h.log.Error("注销其他会话失败", "err", err)
	}
	h.log.Info("用户修改密码", "username", s.Username)
	w.WriteHeader(http.StatusNoContent)
}
