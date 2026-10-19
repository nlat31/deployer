package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"deployer/internal/model"
	"github.com/pquerna/otp/totp"
)

type authBrowser struct {
	t      *testing.T
	s      *Server
	cookie *http.Cookie
	csrf   string
}

func (b *authBrowser) call(method, path string, in any, want int) map[string]any {
	b.t.Helper()
	raw, _ := json.Marshal(in)
	req := httptest.NewRequest(method, "http://controller.test/api"+path, bytes.NewReader(raw))
	req.RemoteAddr = "127.0.0.1:4567"
	req.Header.Set("X-CSRF-Token", b.csrf)
	if b.cookie != nil {
		req.AddCookie(b.cookie)
	}
	w := httptest.NewRecorder()
	b.s.ServeHTTP(w, req)
	if w.Code != want {
		b.t.Fatalf("%s %s: want %d, got %d: %s", method, path, want, w.Code, w.Body.String())
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "deployer_session" {
			b.cookie = c
		}
	}
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	if c, ok := out["csrf"].(string); ok {
		b.csrf = c
	}
	return out
}
func (b *authBrowser) login(password string, want int) map[string]any {
	return b.call("POST", "/login", map[string]string{"username": "admin", "password": password}, want)
}
func newAuthServer(t *testing.T, root string) *Server {
	t.Helper()
	s, e := New(root, false, http.NotFoundHandler())
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func TestAccountPasswordPersistenceAndSessionInvalidation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "server")
	s := newAuthServer(t, root)
	b := &authBrowser{t: t, s: s}
	b.login(DefaultAdminPassword, 200)
	other := &authBrowser{t: t, s: s}
	other.login(DefaultAdminPassword, 200)
	b.call("POST", "/account/password", map[string]string{"password": "wrong", "new_password": "new-password-123"}, 400)
	b.call("POST", "/account/password", map[string]string{"password": DefaultAdminPassword, "new_password": "new-password-123"}, 200)
	other.call("GET", "/account", nil, 401)
	b.login(DefaultAdminPassword, 401)
	b.login("new-password-123", 200)
	s.Close()
	s = newAuthServer(t, root)
	defer s.Close()
	b.s = s
	b.login("new-password-123", 200)
	b.login(DefaultAdminPassword, 401)
}
func TestTOTPBindingRecoveryAndReplay(t *testing.T) {
	s := newAuthServer(t, t.TempDir())
	defer s.Close()
	b := &authBrowser{t: t, s: s}
	b.login(DefaultAdminPassword, 200)
	oldCookie := *b.cookie
	oldCSRF := b.csrf
	setup := b.call("POST", "/account/totp/setup", map[string]string{"password": DefaultAdminPassword}, 200)
	secret := setup["secret"].(string)
	if !strings.HasPrefix(setup["qr_code"].(string), "data:image/png;base64,") {
		t.Fatal("QR code was not generated locally")
	}
	info := b.call("GET", "/account", nil, 200)
	if info["totp_enabled"] != false {
		t.Fatal("unconfirmed binding enabled MFA")
	}
	code, e := totp.GenerateCode(secret, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	result := b.call("POST", "/account/totp/confirm", map[string]string{"code": code}, 200)
	codes := result["recovery_codes"].([]any)
	if len(codes) != 10 {
		t.Fatal("recovery codes missing")
	}
	old := &authBrowser{t: t, s: s, cookie: &oldCookie, csrf: oldCSRF}
	old.call("GET", "/account", nil, 401)
	public := b.call("GET", "/session", nil, 200)
	raw, _ := json.Marshal(public)
	if bytes.Contains(raw, []byte(secret)) || bytes.Contains(raw, []byte("recovery_hashes")) {
		t.Fatal("MFA secret exposed")
	}
	login := &authBrowser{t: t, s: s}
	challenge := login.login(DefaultAdminPassword, 202)["challenge"].(string)
	login.call("POST", "/login/mfa", map[string]string{"challenge": challenge, "code": code}, 401)
	login.call("POST", "/login/mfa", map[string]string{"challenge": challenge, "code": codes[0].(string)}, 200)
	replay := login.login(DefaultAdminPassword, 202)["challenge"].(string)
	login.call("POST", "/login/mfa", map[string]string{"challenge": replay, "code": codes[0].(string)}, 401)
	renewed:=login.call("POST", "/account/recovery-codes", map[string]string{"password": DefaultAdminPassword, "code": codes[1].(string)}, 200)
	newLogin := &authBrowser{t: t, s: s}
	c := newLogin.login(DefaultAdminPassword, 202)["challenge"].(string)
	newLogin.call("POST", "/login/mfa", map[string]string{"challenge": c, "code": codes[2].(string)}, 401)
	// Use a fresh dynamic code from the permitted next time step, without sleeps.
	next, _ := totp.GenerateCode(secret, time.Now().Add(30*time.Second))
	newLogin.call("POST", "/login/mfa", map[string]string{"challenge": c, "code": next}, 200)
	newLogin.call("POST","/account/totp/disable",map[string]string{"password":DefaultAdminPassword,"code":renewed["recovery_codes"].([]any)[0].(string)},200)
	newLogin.login(DefaultAdminPassword,200)
}
func TestTOTPRFCVectorAndReplay(t *testing.T) {
	u := model.User{TOTPSecret: "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", TOTPLastCounter: -1}
	now := time.Unix(59, 0)
	if !consumeTOTP(&u, "287082", now) {
		t.Fatal("RFC vector failed")
	}
	if consumeTOTP(&u, "287082", now) {
		t.Fatal("TOTP replay accepted")
	}
}
