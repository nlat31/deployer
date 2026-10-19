package controller

import (
	"deployer/internal/identity"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func (s *Server) ConfigureEnrollment(clientCA string) error {
	issuer, err := identity.OpenIssuer(filepath.Join(s.Root, "state", "agent-issuer"))
	if err != nil {
		return err
	}
	previous, err := os.ReadFile(clientCA)
	if err != nil {
		return err
	}
	bundle := append(append(previous, '\n'), issuer.PEM...)
	path := filepath.Join(s.Root, "state", "client-ca-bundle.pem")
	if err = identity.AtomicFile(path, bundle); err != nil {
		return err
	}
	s.ClientCA = path
	s.issuer = issuer
	return nil
}
func (s *Server) enroll(w http.ResponseWriter, r *http.Request) error {
	if r.TLS == nil {
		return fail(401, "HTTPS required")
	}
	if s.issuer == nil {
		return fail(503, "enrollment not enabled")
	}
	if err := s.limitAuth(r, "enrollment"); err != nil {
		return err
	}
	// A global bound complements the per-IP limit and caps CPU without creating
	// persistent host records. Only the first authenticated heartbeat registers.
	s.enrollMu.Lock()

	minute := time.Now().Unix() / 60
	if minute != s.enrollMinute {
		s.enrollMinute = minute
		s.enrollCount = 0
	}
	if s.enrollCount >= 1000 {
		s.enrollMu.Unlock()
		return fail(429, "enrollment busy")
	}
	s.enrollCount++
	s.enrollMu.Unlock()
	var input struct {
		CSR string `json:"csr"`
	}
	if err := decodeLimit(w, r, &input, 16<<10); err != nil {
		return err
	}
	certificate, err := s.issuer.Sign([]byte(input.CSR))
	if err != nil {
		return fail(400, "invalid certificate request")
	}
	jsonOut(w, 200, map[string]string{"certificate": string(certificate)})
	return nil
}
