package httpserver

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Yukiho0287/assay/server/internal/api"
	"github.com/Yukiho0287/assay/server/internal/auth"
	"github.com/Yukiho0287/assay/server/internal/db"
	"github.com/Yukiho0287/assay/server/internal/feishu"
)

// feishuDefaultRole 自动建号时授予的角色：内置 member（无用户管理与系统设置权限）
const feishuDefaultRole = "member"

// oauthNextCookie 记住登录前想去的站内路径，回调后跳回去（深链接不丢）
const oauthNextCookie = "assay_oauth_next"

// safeNext 只放行站内相对路径，挡开放重定向。
// 必须以单个 / 开头：`//evil.com` 和 `/\evil.com` 都会被浏览器当成协议相对 URL 跳到外站。
func safeNext(v string) string {
	if len(v) < 1 || v[0] != '/' {
		return ""
	}
	if len(v) > 1 && (v[1] == '/' || v[1] == '\\') {
		return ""
	}
	return v
}

// GetAuthMethods 登录页据此决定渲染哪些入口，未登录可访问。
func (h *handlers) GetAuthMethods(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, api.AuthMethods{
		Password: h.localLogin,
		Feishu:   h.fs != nil,
	})
}

// FeishuAuthorize 下发一次性 state Cookie 后跳转飞书授权页。
func (h *handlers) FeishuAuthorize(w http.ResponseWriter, r *http.Request, params api.FeishuAuthorizeParams) {
	if h.fs == nil {
		writeJSON(w, http.StatusServiceUnavailable, api.Error{Error: "本站未配置飞书登录"})
		return
	}
	state, err := auth.RandomToken()
	if err != nil {
		h.log.Error("生成 OAuth state 失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, api.Error{Error: "服务内部错误"})
		return
	}
	// state 只存浏览器、不落库：回调时与 query 比对即可证明这次回调源于本站发起
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookie,
		Value:    state,
		Path:     oauthStatePath,
		MaxAge:   int(oauthStateTTL.Seconds()),
		HttpOnly: true,
		Secure:   h.secureCookie(r),
		SameSite: http.SameSiteLaxMode, // 顶层 GET 跳转回来时仍会携带
	})
	// 登录前想去哪，存 Cookie 而不是塞进 state——state 要参与恒定时间比对，不宜承载可变数据
	next := ""
	if params.Next != nil {
		next = safeNext(*params.Next)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oauthNextCookie,
		Value:    next,
		Path:     oauthStatePath,
		MaxAge:   int(oauthStateTTL.Seconds()),
		HttpOnly: true,
		Secure:   h.secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, h.fs.AuthorizeURL(state), http.StatusFound)
}

// FeishuCallback 飞书授权回调：校验 state → 换身份 → 建号/更新 → 签发会话 → 跳首页。
// 全程在后端完成，授权码不经过前端 JS。
func (h *handlers) FeishuCallback(w http.ResponseWriter, r *http.Request, params api.FeishuCallbackParams) {
	h.clearOAuthState(w, r)

	if h.fs == nil {
		h.loginFailed(w, r, "本站未配置飞书登录", nil)
		return
	}
	if params.Error != nil && *params.Error != "" {
		// 用户在授权页点了拒绝，属正常路径，不记 error 级日志
		h.loginFailed(w, r, "已取消飞书授权", nil)
		return
	}
	if params.Code == nil || *params.Code == "" {
		h.loginFailed(w, r, "飞书未返回授权码", nil)
		return
	}
	if !h.stateMatches(r, params.State) {
		h.loginFailed(w, r, "登录状态已失效，请重新发起登录", nil)
		return
	}

	info, err := h.fs.Login(r.Context(), *params.Code)
	if err != nil {
		h.loginFailed(w, r, "飞书身份校验失败", err)
		return
	}

	userID, err := h.resolveFeishuUser(r, info)
	if err != nil {
		h.loginFailed(w, r, "创建账号失败", err)
		return
	}

	if !h.issueSession(w, r, userID) {
		return // issueSession 已写过 500
	}
	h.log.Info("用户登录", "username", info.Name, "method", "feishu", "union_id", info.UnionID)
	// 再校验一次：Cookie 可能被改，绝不拿它的值直接跳转
	dest := "/"
	if c, err := r.Cookie(oauthNextCookie); err == nil {
		if n := safeNext(c.Value); n != "" {
			dest = n
		}
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// stateMatches 恒定时间比对回调 state 与 Cookie 中的一次性随机串
func (h *handlers) stateMatches(r *http.Request, got *string) bool {
	c, err := r.Cookie(oauthStateCookie)
	if err != nil || c.Value == "" || got == nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(*got)) == 1
}

func (h *handlers) clearOAuthState(w http.ResponseWriter, r *http.Request) {
	for _, name := range []string{oauthStateCookie, oauthNextCookie} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     oauthStatePath,
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   h.secureCookie(r),
			SameSite: http.SameSiteLaxMode,
		})
	}
}

// loginFailed 回调失败一律跳回登录页并把原因带在 query 上；
// detail 只进服务端日志，不回显给浏览器（可能含上游报文）
func (h *handlers) loginFailed(w http.ResponseWriter, r *http.Request, reason string, detail error) {
	if detail != nil {
		h.log.Error("飞书登录失败", "reason", reason, "err", detail)
	} else {
		h.log.Warn("飞书登录未完成", "reason", reason)
	}
	http.Redirect(w, r, "/login?error="+url.QueryEscape(reason), http.StatusFound)
}

// resolveFeishuUser 按 union_id 找账号：找到就刷新资料，找不到就自动建号（member 角色）。
func (h *handlers) resolveFeishuUser(r *http.Request, info *feishu.UserInfo) (uuid.UUID, error) {
	ctx := r.Context()
	id, err := h.q.GetUserByFeishuUnionID(ctx, &info.UnionID)
	if err == nil {
		// 姓名、头像可能在飞书侧改过，每次登录同步一次
		if err := h.q.RefreshFeishuUserProfile(ctx, db.RefreshFeishuUserProfileParams{
			ID:           id,
			FeishuOpenID: &info.OpenID,
			DisplayName:  &info.Name,
			AvatarUrl:    &info.AvatarURL,
		}); err != nil {
			h.log.Warn("刷新飞书用户资料失败", "err", err) // 不阻断登录
		}
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("查询飞书用户: %w", err)
	}

	role, err := h.q.GetRoleByName(ctx, feishuDefaultRole)
	if err != nil {
		return uuid.Nil, fmt.Errorf("查询内置 %s 角色: %w", feishuDefaultRole, err)
	}
	username, err := h.uniqueUsername(r, info)
	if err != nil {
		return uuid.Nil, err
	}
	// on conflict (feishu_union_id) 兜住并发首登：同一人只会落一行
	id, err = h.q.UpsertFeishuUser(ctx, db.UpsertFeishuUserParams{
		Username:      username,
		RoleID:        role.ID,
		FeishuUnionID: &info.UnionID,
		FeishuOpenID:  &info.OpenID,
		DisplayName:   &info.Name,
		AvatarUrl:     &info.AvatarURL,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("自动建号: %w", err)
	}
	h.log.Info("飞书首次登录，已自动建号", "username", username, "role", feishuDefaultRole)
	return id, nil
}

// uniqueUsername 用飞书姓名做用户名，撞名则拼 union_id 尾段。
// 用户名只是展示与登录列表用的标识，账号真正的唯一键是 union_id。
func (h *handlers) uniqueUsername(r *http.Request, info *feishu.UserInfo) (string, error) {
	base := strings.TrimSpace(info.Name)
	if base == "" {
		base = "飞书用户"
	}
	for _, candidate := range []string{base, base + "-" + tail(info.UnionID, 6)} {
		taken, err := h.q.UsernameExists(r.Context(), candidate)
		if err != nil {
			return "", fmt.Errorf("检查用户名占用: %w", err)
		}
		if !taken {
			return candidate, nil
		}
	}
	// union_id 全局唯一，走到这里说明同一人已有账号却没被 union_id 查到，属数据异常
	return "", fmt.Errorf("用户名 %q 与其带 union_id 后缀的形式均已被占用", base)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
