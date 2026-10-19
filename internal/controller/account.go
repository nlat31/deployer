package controller

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"strings"
	"time"

	"deployer/internal/model"
	"deployer/internal/store"
	"github.com/pquerna/otp/hotp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

type loginChallenge struct {
	UserID      string `json:"user_id"`
	AuthVersion int64  `json:"auth_version"`
	ExpiresAt   int64  `json:"expires_at"`
	Attempts    int    `json:"attempts"`
}
type pendingTOTP struct {
	UserID      string `json:"user_id"`
	AuthVersion int64  `json:"auth_version"`
	Secret      string `json:"secret"`
	ExpiresAt   int64  `json:"expires_at"`
}
type accountProof struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

// Account-wide, persistent throttling also covers distributed attempts where
// an attacker already knows the password and keeps creating new challenges.
func (s *Server) factorLimit(userID string) error {
	return s.DB.Update(func(t *store.Tx) error {
		type throttle struct {
			Started int64 `json:"started"`
			Count   int   `json:"count"`
		}
		v, e := store.Get[throttle](t, "factor_attempts", userID)
		if e != nil && !errors.Is(e, store.ErrNotFound) {
			return e
		}
		now := model.Now()
		if now-v.Started >= 5*60*1000 {
			v = throttle{Started: now}
		}
		if v.Count >= 10 {
			return fail(429, "验证码尝试次数过多，请五分钟后重试")
		}
		v.Count++
		return store.Put(t, "factor_attempts", userID, v)
	})
}

// Match a single not-yet-used time step. Reusing a code is rejected transactionally.
func consumeTOTP(u *model.User, code string, now time.Time) bool {
	if len(code) != 6 {
		return false
	}
	counter := now.Unix() / 30
	for _, offset := range []int64{0, -1, 1} {
		n := counter + offset
		if n < 0 || n <= u.TOTPLastCounter {
			continue
		}
		want, e := hotp.GenerateCode(u.TOTPSecret, uint64(n))
		if e == nil && same(want, code) {
			u.TOTPLastCounter = n
			return true
		}
	}
	return false
}
func consumeFactor(u *model.User, code string) bool {
	code = strings.TrimSpace(code)
	if consumeTOTP(u, code, time.Now()) {
		return true
	}
	digest := hash(u.ID + ":" + strings.ToLower(strings.ReplaceAll(code, "-", "")))
	for i, h := range u.RecoveryHashes {
		if same(h, digest) {
			u.RecoveryHashes = append(u.RecoveryHashes[:i], u.RecoveryHashes[i+1:]...)
			return true
		}
	}
	return false
}
func recoveryCodes(u *model.User) []string {
	out := make([]string, 10)
	u.RecoveryHashes = nil
	for i := range out {
		raw := model.ID()
		out[i] = raw[:8] + "-" + raw[8:16] + "-" + raw[16:24] + "-" + raw[24:]
		u.RecoveryHashes = append(u.RecoveryHashes, hash(u.ID+":"+raw))
	}
	return out
}
func accountInfo(u model.User) map[string]any {
	return map[string]any{"id": u.ID, "name": u.Name, "role": u.Role, "totp_enabled": u.TOTPEnabled, "recovery_codes_remaining": len(u.RecoveryHashes)}
}
func (s *Server) account(w http.ResponseWriter, r *http.Request) error {
	jsonOut(w, 200, accountInfo(actor(r)))
	return nil
}
func currentAccount(t *store.Tx, r *http.Request, proof accountProof, requireFactor bool) (model.User, error) {
	old := actor(r)
	u, e := store.Get[model.User](t, "users", old.ID)
	if e != nil {
		return u, e
	}
	if u.AuthVersion != old.AuthVersion || u.Disabled {
		return u, fail(401, "帐号安全信息已变化，请重新登录")
	}
	if len(proof.Password) > 72 || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(proof.Password)) != nil {
		return u, fail(400, "当前密码错误")
	}
	if requireFactor && u.TOTPEnabled && !consumeFactor(&u, proof.Code) {
		return u, fail(400, "验证码或恢复码错误、已使用或已过期")
	}
	return u, nil
}
func rotateSession(t *store.Tx, r *http.Request, u *model.User) (string, model.Session, error) {
	u.AuthVersion++
	if e := store.Put(t, "users", u.ID, *u); e != nil {
		return "", model.Session{}, e
	}
	if e := store.Delete(t, "totp_pending", r.Context().Value(contextKey("session_id")).(string)); e != nil {
		return "", model.Session{}, e
	}
	return issueSession(t, *u)
}
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) error {
	if e := s.limitAuth(r, "account"); e != nil {
		return e
	}
	if actor(r).TOTPEnabled {
		if e := s.factorLimit(actor(r).ID); e != nil {
			return e
		}
	}
	var in struct {
		accountProof
		NewPassword string `json:"new_password"`
	}
	if e := decode(w, r, &in); e != nil {
		return e
	}
	if len(in.NewPassword) < 8 || len(in.NewPassword) > 72 {
		return fail(400, "新密码需为 8 至 72 字节")
	}
	if same(in.Password, in.NewPassword) {
		return fail(400, "新密码不能与当前密码相同")
	}
	b, e := bcrypt.GenerateFromPassword([]byte(in.NewPassword), bcrypt.DefaultCost)
	if e != nil {
		return e
	}
	e = s.DB.Update(func(t *store.Tx) error {
		u, e := currentAccount(t, r, in.accountProof, true)
		if e != nil {
			return e
		}
		u.PasswordHash = string(b)
		u.AuthVersion++
		if e = store.Put(t, "users", u.ID, u); e != nil {
			return e
		}
		return audit(t, u.ID, "change_password", u.ID)
	})
	if e != nil {
		return e
	}
	http.SetCookie(w, &http.Cookie{Name: "deployer_session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.cookieSecure, SameSite: http.SameSiteStrictMode})
	jsonOut(w, 200, map[string]bool{"ok": true, "login_required": true})
	return nil
}
func (s *Server) setupTOTP(w http.ResponseWriter, r *http.Request) error {
	if e := s.limitAuth(r, "account"); e != nil {
		return e
	}
	var proof accountProof
	if e := decode(w, r, &proof); e != nil {
		return e
	}
	key, e := totp.Generate(totp.GenerateOpts{Issuer: "Deployer", AccountName: actor(r).ID})
	if e != nil {
		return e
	}
	img, e := key.Image(256, 256)
	if e != nil {
		return e
	}
	var data bytes.Buffer
	if e = png.Encode(&data, img); e != nil {
		return e
	}
	pending := pendingTOTP{UserID: actor(r).ID, AuthVersion: actor(r).AuthVersion, Secret: key.Secret(), ExpiresAt: time.Now().Add(10 * time.Minute).UnixMilli()}
	e = s.DB.Update(func(t *store.Tx) error {
		u, e := currentAccount(t, r, proof, false)
		if e != nil {
			return e
		}
		if u.TOTPEnabled {
			return fail(409, "已绑定 Google 验证码")
		}
		return store.Put(t, "totp_pending", r.Context().Value(contextKey("session_id")).(string), pending)
	})
	if e != nil {
		return e
	}
	jsonOut(w, 200, map[string]any{"secret": key.Secret(), "qr_code": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data.Bytes()), "expires_at": pending.ExpiresAt})
	return nil
}
func (s *Server) confirmTOTP(w http.ResponseWriter, r *http.Request) error {
	if e := s.limitAuth(r, "account"); e != nil {
		return e
	}
	var in struct {
		Code string `json:"code"`
	}
	if e := decode(w, r, &in); e != nil {
		return e
	}
	var codes []string
	var token string
	var session model.Session
	var u model.User
	e := s.DB.Update(func(t *store.Tx) error {
		id := r.Context().Value(contextKey("session_id")).(string)
		p, e := store.Get[pendingTOTP](t, "totp_pending", id)
		if e != nil {
			return fail(400, "请重新发起绑定")
		}
		u, e = store.Get[model.User](t, "users", actor(r).ID)
		if e != nil {
			return e
		}
		if p.UserID != u.ID || p.AuthVersion != u.AuthVersion || u.TOTPEnabled || p.ExpiresAt < model.Now() {
			return fail(400, "绑定已过期，请重新发起")
		}
		u.TOTPSecret = p.Secret
		u.TOTPLastCounter = -1
		if !consumeTOTP(&u, in.Code, time.Now()) {
			return fail(400, "验证码错误或已过期")
		}
		u.TOTPEnabled = true
		codes = recoveryCodes(&u)
		token, session, e = rotateSession(t, r, &u)
		if e != nil {
			return e
		}
		return audit(t, u.ID, "enable_totp", u.ID)
	})
	if e != nil {
		return e
	}
	s.sessionCookie(w, token)
	jsonOut(w, 200, map[string]any{"account": accountInfo(u), "csrf": session.CSRF, "recovery_codes": codes})
	return nil
}
func (s *Server) disableTOTP(w http.ResponseWriter, r *http.Request) error {
	return s.modifyTOTP(w, r, false)
}
func (s *Server) regenerateRecovery(w http.ResponseWriter, r *http.Request) error {
	return s.modifyTOTP(w, r, true)
}
func (s *Server) modifyTOTP(w http.ResponseWriter, r *http.Request, regenerate bool) error {
	if e := s.limitAuth(r, "account"); e != nil {
		return e
	}
	if actor(r).TOTPEnabled {
		if e := s.factorLimit(actor(r).ID); e != nil {
			return e
		}
	}
	var proof accountProof
	if e := decode(w, r, &proof); e != nil {
		return e
	}
	var codes []string
	var u model.User
	var token string
	var session model.Session
	e := s.DB.Update(func(t *store.Tx) error {
		var e error
		u, e = currentAccount(t, r, proof, true)
		if e != nil {
			return e
		}
		if !u.TOTPEnabled {
			return fail(409, "尚未绑定 Google 验证码")
		}
		action := "disable_totp"
		if regenerate {
			codes = recoveryCodes(&u)
			action = "regenerate_recovery_codes"
		} else {
			u.TOTPEnabled = false
			u.TOTPSecret = ""
			u.TOTPLastCounter = 0
			u.RecoveryHashes = nil
		}
		token, session, e = rotateSession(t, r, &u)
		if e != nil {
			return e
		}
		return audit(t, u.ID, action, u.ID)
	})
	if e != nil {
		return e
	}
	s.sessionCookie(w, token)
	jsonOut(w, 200, map[string]any{"account": accountInfo(u), "csrf": session.CSRF, "recovery_codes": codes})
	return nil
}
func (s *Server) loginMFA(w http.ResponseWriter, r *http.Request) error {
	if !originOK(r) {
		return fail(403, "来源不匹配")
	}
	if e := s.limitAuth(r, "mfa"); e != nil {
		return e
	}
	var in struct {
		Challenge string `json:"challenge"`
		Code      string `json:"code"`
	}
	if e := decode(w, r, &in); e != nil {
		return e
	}
	if len(in.Challenge) != 64 || len(in.Code) > 100 {
		return fail(401, "登录验证已失效")
	}
	var candidate loginChallenge
	if e := s.DB.View(func(t *store.Tx) error {
		var e error
		candidate, e = store.Get[loginChallenge](t, "login_challenges", hash(in.Challenge))
		return e
	}); e != nil || candidate.ExpiresAt < model.Now() {
		return fail(401, "登录验证已失效")
	}
	if e := s.factorLimit(candidate.UserID); e != nil {
		return e
	}
	var u model.User
	var token string
	var session model.Session
	var denied bool
	e := s.DB.Update(func(t *store.Tx) error {
		id := hash(in.Challenge)
		c, e := store.Get[loginChallenge](t, "login_challenges", id)
		if e != nil {
			return fail(401, "登录验证已失效")
		}
		if c.ExpiresAt < model.Now() || c.Attempts >= 5 {
			return fail(401, "验证已过期或尝试次数过多，请重新登录")
		}
		u, e = store.Get[model.User](t, "users", c.UserID)
		if e != nil {
			return e
		}
		if u.Disabled || !u.TOTPEnabled || u.AuthVersion != c.AuthVersion {
			return fail(401, "帐号状态已变化，请重新登录")
		}
		if !consumeFactor(&u, in.Code) {
			denied = true
			c.Attempts++
			return store.Put(t, "login_challenges", id, c)
		}
		if e = store.Put(t, "users", u.ID, u); e != nil {
			return e
		}
		if e = store.Delete(t, "login_challenges", id); e != nil {
			return e
		}
		token, session, e = issueSession(t, u)
		if e != nil {
			return e
		}
		return audit(t, u.ID, "login_mfa", u.ID)
	})
	if e != nil {
		return e
	}
	if denied {
		return fail(401, "验证码或恢复码错误、已使用或已过期")
	}
	if token == "" {
		return fmt.Errorf("missing verified session")
	}
	s.sessionCookie(w, token)
	jsonOut(w, 200, map[string]any{"user": model.PublicUser(u), "csrf": session.CSRF})
	return nil
}
