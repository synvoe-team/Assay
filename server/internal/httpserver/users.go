package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"

	"github.com/Yukiho0287/assay/server/internal/api"
	"github.com/Yukiho0287/assay/server/internal/db"
)

// isUniqueViolation PostgreSQL 唯一约束冲突（23505），用于把重名映射为 409
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// authSource 认证来源由「有没有绑定飞书」推导，不额外存列
func authSource(fromFeishu bool) api.AuthSource {
	if fromFeishu {
		return api.Feishu
	}
	return api.Password
}

func userToAPI(u db.GetUserRow) api.User {
	return api.User{
		Id: u.ID, Username: u.Username, RoleId: u.RoleID, RoleName: u.RoleName,
		AuthSource: authSource(u.FromFeishu), CreatedAt: u.CreatedAt,
	}
}

func (h *handlers) ListUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requirePerm(w, r, "users"); !ok {
		return
	}
	rows, err := h.q.ListUsers(r.Context())
	if err != nil {
		h.log.Error("查询用户列表失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, api.Error{Error: "服务内部错误"})
		return
	}
	out := make([]api.User, 0, len(rows))
	for _, u := range rows {
		out = append(out, api.User{
			Id: u.ID, Username: u.Username, RoleId: u.RoleID, RoleName: u.RoleName,
			AuthSource: authSource(u.FromFeishu), CreatedAt: u.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) CreateUser(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requirePerm(w, r, "users")
	if !ok {
		return
	}
	var req api.UserCreate
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Username == "" {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: "用户名不能为空"})
		return
	}
	if len(req.Username) > 64 {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: "用户名过长（最多 64 字符）"})
		return
	}
	if len(req.Password) < 8 {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: "密码至少 8 位"})
		return
	}
	if _, err := h.q.GetRole(r.Context(), req.RoleId); err != nil {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: "指定的角色不存在"})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		h.log.Error("哈希密码失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, api.Error{Error: "服务内部错误"})
		return
	}
	pwHash := string(hash)
	id, err := h.q.CreateUser(r.Context(), db.CreateUserParams{
		Username:     req.Username,
		PasswordHash: &pwHash,
		RoleID:       req.RoleId,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeJSON(w, http.StatusConflict, api.Error{Error: "用户名已存在"})
			return
		}
		h.log.Error("创建用户失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, api.Error{Error: "服务内部错误"})
		return
	}
	u, err := h.q.GetUser(r.Context(), id)
	if err != nil {
		h.log.Error("回读新建用户失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, api.Error{Error: "服务内部错误"})
		return
	}
	h.log.Info("创建用户", "username", req.Username, "operator", s.Username)
	writeJSON(w, http.StatusCreated, userToAPI(u))
}

func (h *handlers) UpdateUser(w http.ResponseWriter, r *http.Request, id api.IdPath) {
	s, ok := h.requirePerm(w, r, "users")
	if !ok {
		return
	}
	var req api.UserUpdate
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: "请求格式错误"})
		return
	}
	target, err := h.q.GetUser(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, api.Error{Error: "用户不存在"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, api.Error{Error: "服务内部错误"})
		return
	}
	// 给飞书账号设密码等于开了一条绕过飞书的后门，直接拒绝
	if req.Password != nil && target.FromFeishu {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: "飞书登录账号不支持设置密码"})
		return
	}

	if req.RoleId != nil {
		if id == s.ID {
			// 防自锁：不能改自己的角色（把唯一 admin 降级会导致无人能管理系统）
			writeJSON(w, http.StatusBadRequest, api.Error{Error: "不能修改自己的角色"})
			return
		}
		if _, err := h.q.GetRole(r.Context(), *req.RoleId); err != nil {
			writeJSON(w, http.StatusBadRequest, api.Error{Error: "指定的角色不存在"})
			return
		}
		if _, err := h.q.UpdateUserRole(r.Context(), db.UpdateUserRoleParams{ID: id, RoleID: *req.RoleId}); err != nil {
			h.log.Error("更新用户角色失败", "err", err)
			writeJSON(w, http.StatusInternalServerError, api.Error{Error: "服务内部错误"})
			return
		}
	}
	if req.Password != nil {
		if len(*req.Password) < 8 {
			writeJSON(w, http.StatusBadRequest, api.Error{Error: "密码至少 8 位"})
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(*req.Password), bcrypt.DefaultCost)
		if err != nil {
			h.log.Error("哈希密码失败", "err", err)
			writeJSON(w, http.StatusInternalServerError, api.Error{Error: "服务内部错误"})
			return
		}
		hashStr := string(hash)
		if _, err := h.q.UpdateUserPassword(r.Context(), db.UpdateUserPasswordParams{ID: id, PasswordHash: &hashStr}); err != nil {
			h.log.Error("重置用户密码失败", "err", err)
			writeJSON(w, http.StatusInternalServerError, api.Error{Error: "服务内部错误"})
			return
		}
		// 管理员重置密码后强制该用户全部下线重新登录
		if err := h.q.DeleteUserSessions(r.Context(), id); err != nil {
			h.log.Error("注销用户会话失败", "err", err)
		}
	}

	u, err := h.q.GetUser(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, api.Error{Error: "服务内部错误"})
		return
	}
	h.log.Info("更新用户", "target", u.Username, "operator", s.Username)
	writeJSON(w, http.StatusOK, userToAPI(u))
}

func (h *handlers) DeleteUser(w http.ResponseWriter, r *http.Request, id api.IdPath) {
	s, ok := h.requirePerm(w, r, "users")
	if !ok {
		return
	}
	if id == s.ID {
		writeJSON(w, http.StatusBadRequest, api.Error{Error: "不能删除自己"})
		return
	}
	n, err := h.q.DeleteUser(r.Context(), id)
	if err != nil {
		h.log.Error("删除用户失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, api.Error{Error: "服务内部错误"})
		return
	}
	if n == 0 {
		writeJSON(w, http.StatusNotFound, api.Error{Error: "用户不存在"})
		return
	}
	h.log.Info("删除用户", "target", id.String(), "operator", s.Username)
	w.WriteHeader(http.StatusNoContent)
}
