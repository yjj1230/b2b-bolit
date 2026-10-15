package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"b2b_pilot_v3/internal/authz"
	"b2b_pilot_v3/internal/wecom"

	"github.com/stretchr/testify/require"
)

// 生产硬化（B2B_AUTH_STRICT=true）：withContext 只认 X-WeCom-Context；
// 无令牌 / 校验失败 / 不在白名单 → 401，不信任前端 X-User-Id。
func TestWithContextAuthStrict(t *testing.T) {
	issuer := wecom.NewContextIssuer("test-secret", time.Hour)
	roleOf := func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" }
	srv := NewServer(nil, nil, nil, roleOf)
	srv.Context = issuer
	srv.RoleResolver = func(userID string) (authz.Role, string, bool) {
		if userID == "u-real:OP" {
			return authz.RoleOP, "chang-an", true
		}
		return "", "", false
	}
	srv.AuthStrict = true

	var got *ctxUser
	h := srv.withContext(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := userOf(r.Context())
		got = &u
		w.WriteHeader(http.StatusOK)
	}))

	tok, err := issuer.Issue("u-real:OP", "chang-an", "")
	require.NoError(t, err)

	// 1) 无令牌 → 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// 2) 只带伪造 X-User-Id → 401（不再信任裸头）
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-User-Id", "u-hongli")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// 3) 无效令牌 → 401
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-WeCom-Context", tok+"x")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// 4) 有效令牌但不在白名单 → 401
	badTok, err := issuer.Issue("not-in-whitelist", "chang-an", "")
	require.NoError(t, err)
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-WeCom-Context", badTok)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// 5) 有效令牌 + 在白名单 → 200，身份=令牌 principal（伪造 X-User-Id 被忽略）
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-WeCom-Context", tok)
	req.Header.Set("X-User-Id", "u-hongli")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, got)
	require.Equal(t, "u-real:OP", got.UserID)
}

// 测试模式（AuthStrict=false，默认）：X-User-Id 仍被信任，业务流转零影响。
func TestWithContextNonStrictPreservesFlow(t *testing.T) {
	srv := NewServer(nil, nil, nil, func(string) (authz.Role, string) { return authz.RoleOP, "chang-an" })
	srv.AuthStrict = false // 默认值

	var got *ctxUser
	h := srv.withContext(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := userOf(r.Context())
		got = &u
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-User-Id", "u-hongli")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, got)
	require.Equal(t, "u-hongli", got.UserID)
}
