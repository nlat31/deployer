package controller

import (
	"context"
	"crypto/subtle"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"

	"deployer/internal/identity"
	"deployer/internal/model"
	"deployer/internal/store"
	"golang.org/x/crypto/bcrypt"
)

const DefaultAdminPassword = "xScan!@&"

type contextKey string

func actor(r *http.Request) model.User     { return r.Context().Value(contextKey("user")).(model.User) }
func agentNode(r *http.Request) model.Node { return r.Context().Value(contextKey("node")).(model.Node) }
func roleLevel(role string) int {
	switch role {
	case "admin":
		return 3
	case "operator":
		return 2
	case "viewer":
		return 1
	}
	return 0
}
func originOK(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, e := url.Parse(origin)
	return e == nil && u.Host == r.Host && (u.Scheme == "https" || u.Scheme == "http")
}
func same(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func (s *Server) authorize(role string, fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return s.wrap(func(w http.ResponseWriter, r *http.Request) error {
		c, e := r.Cookie("deployer_session")
		if e != nil || len(c.Value) != 64 {
			return fail(401, "请先登录")
		}
		var session model.Session
		var u model.User
		e = s.DB.View(func(t *store.Tx) error {
			var e error
			session, e = store.Get[model.Session](t, "sessions", hash(c.Value))
			if e != nil {
				return e
			}
			u, e = store.Get[model.User](t, "users", session.UserID)
			return e
		})
		if e != nil || session.ExpiresAt <= model.Now() || u.Disabled || session.AuthVersion != u.AuthVersion {
			return fail(401, "登录已过期，请重新登录")
		}
		if roleLevel(u.Role) < roleLevel(role) {
			return fail(403, "权限不足")
		}
		if r.Method != "GET" && r.Method != "HEAD" && (!same(r.Header.Get("X-CSRF-Token"), session.CSRF) || !originOK(r)) {
			return fail(403, "CSRF 校验失败")
		}
		r = r.WithContext(context.WithValue(context.WithValue(context.WithValue(r.Context(), contextKey("user"), u), contextKey("session"), session), contextKey("session_id"), hash(c.Value)))
		return fn(w, r)
	})
}
func (s *Server) agentAuth(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return s.wrap(func(w http.ResponseWriter, r *http.Request) error {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
			return fail(401, "Agent 必须使用受信任的 mTLS 客户端证书")
		}
		leaf := r.TLS.PeerCertificates[0]
		if now := time.Now(); now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
			return fail(401, "客户端证书已过期或尚未生效")
		}
		if s.ClientCA != "" {
			roots, err := identity.Trust(s.ClientCA)
			if err != nil {
				return fail(503, "客户端 CA 暂不可用")
			}
			intermediates := x509.NewCertPool()
			for _, c := range r.TLS.PeerCertificates[1:] {
				intermediates.AddCert(c)
			}
			if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
				return fail(401, "客户端证书不受当前 CA 信任")
			}
		}
		id, err := identity.NodeID(leaf)
		if err != nil {
			return fail(401, err.Error())
		}
		n := model.Node{ID: id, CertificateExpires: leaf.NotAfter.UnixMilli()}
		if r.URL.Path != "/agent/poll" {
			if err := s.DB.View(func(t *store.Tx) error { _, err := store.Get[model.Node](t, "nodes", id); return err }); err != nil {
				return fail(401, "请先发送有效心跳")
			}
		}
		return fn(w, r.WithContext(context.WithValue(r.Context(), contextKey("node"), n)))
	})
}
func (s *Server) limitAuth(r *http.Request, scope string) error {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	key := host + ":" + scope
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	now := model.Now()
	recent := []int64{}
	for _, t := range s.attempts[key] {
		if now-t < 60000 {
			recent = append(recent, t)
		}
	}
	if len(recent) >= 10 {
		return fail(429, "尝试次数过多，请稍后重试")
	}
	if len(s.attempts) > 10000 {
		s.attempts = map[string][]int64{}
	}
	s.attempts[key] = append(recent, now)
	return nil
}
func issueSession(t *store.Tx, u model.User) (string, model.Session, error) {
	token := model.ID() + model.ID()
	session := model.Session{UserID: u.ID, CSRF: model.ID(), ExpiresAt: time.Now().Add(8 * time.Hour).UnixMilli(), AuthVersion: u.AuthVersion}
	return token, session, store.Put(t, "sessions", hash(token), session)
}
func (s *Server) sessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{Name: "deployer_session", Value: token, Path: "/", HttpOnly: true, Secure: s.cookieSecure, SameSite: http.SameSiteStrictMode, MaxAge: 28800})
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) error {
	if !originOK(r) {
		return fail(403, "来源不匹配")
	}
	if e := s.limitAuth(r, "login"); e != nil {
		return e
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if e := decode(w, r, &in); e != nil {
		return e
	}
	if !model.ValidName(in.Username) || len(in.Password) > 72 {
		return fail(401, "用户名或密码错误")
	}
	var u model.User
	var token string
	var session model.Session
	var challenge string
	e := s.DB.Update(func(t *store.Tx) error {
		var e error
		u, e = store.Get[model.User](t, "users", in.Username)
		if e != nil || u.Disabled || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(in.Password)) != nil {
			return fail(401, "用户名或密码错误")
		}
		if u.TOTPEnabled {
			challenge = model.ID() + model.ID()
			return store.Put(t, "login_challenges", hash(challenge), loginChallenge{UserID: u.ID, AuthVersion: u.AuthVersion, ExpiresAt: time.Now().Add(5 * time.Minute).UnixMilli()})
		}
		token, session, e = issueSession(t, u)
		if e != nil {
			return e
		}
		return audit(t, u.ID, "login", u.ID)
	})
	if e != nil {
		return e
	}
	if challenge != "" {
		jsonOut(w, 202, map[string]any{"mfa_required": true, "challenge": challenge})
		return nil
	}
	s.sessionCookie(w, token)
	jsonOut(w, 200, map[string]any{"user": model.PublicUser(u), "csrf": session.CSRF})
	return nil
}
func (s *Server) session(w http.ResponseWriter, r *http.Request) error {
	jsonOut(w, 200, map[string]any{"user": model.PublicUser(actor(r)), "csrf": r.Context().Value(contextKey("session")).(model.Session).CSRF})
	return nil
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) error {
	id := r.Context().Value(contextKey("session_id")).(string)
	if e := s.DB.Update(func(t *store.Tx) error {
		if e := store.Delete(t, "totp_pending", id); e != nil {
			return e
		}
		return store.Delete(t, "sessions", id)
	}); e != nil {
		return e
	}
	http.SetCookie(w, &http.Cookie{Name: "deployer_session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cookieSecure, SameSite: http.SameSiteStrictMode})
	jsonOut(w, 200, map[string]bool{"ok": true})
	return nil
}
func (s *Server) saveUser(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Role     string `json:"role"`
		Password string `json:"password"`
		Disabled bool   `json:"disabled"`
	}
	if e := decode(w, r, &in); e != nil {
		return e
	}
	if !model.ValidName(in.ID) || roleLevel(in.Role) == 0 || len(in.Name) > 200 || len(in.Password) > 72 {
		return fail(400, "无效用户信息")
	}
	if in.ID == actor(r).ID && (in.Disabled || in.Role != "admin") {
		return fail(400, "不能禁用或降级当前管理员")
	}
	if in.ID == actor(r).ID && in.Password != "" {
		return fail(400, "请在我的帐号修改自己的密码")
	}
	e := s.DB.Update(func(t *store.Tx) error {
		u, e := store.Get[model.User](t, "users", in.ID)
		if e != nil && !errors.Is(e, store.ErrNotFound) {
			return e
		}
		if in.Password != "" || u.ID == "" {
			if len(in.Password) < 8 {
				return fail(400, "密码至少 8 字符")
			}
			b, e := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
			if e != nil {
				return e
			}
			u.PasswordHash = string(b)
		}
		u.ID = in.ID
		u.Name = in.Name
		u.Role = in.Role
		u.Disabled = in.Disabled
		u.AuthVersion++
		if e = store.Put(t, "users", u.ID, u); e != nil {
			return e
		}
		return audit(t, actor(r).ID, "save_user", u.ID)
	})
	if e != nil {
		return e
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
	return nil
}
func (s *Server) users(w http.ResponseWriter, r *http.Request) error {
	var list []model.User
	e := s.DB.View(func(t *store.Tx) error { var e error; list, e = store.List[model.User](t, "users"); return e })
	if e != nil {
		return e
	}
	for i := range list {
		list[i] = model.PublicUser(list[i])
	}
	jsonOut(w, 200, list)
	return nil
}
