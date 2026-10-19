package controller

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"deployer/internal/identity"
	"deployer/internal/model"
	"deployer/internal/store"
)

func TestBackupSnapshotRetainsKeyAndPassword(t *testing.T) {
	s := newAuthServer(t, t.TempDir())
	defer s.Close()
	b := &authBrowser{t: t, s: s}
	b.login(DefaultAdminPassword, 200)
	if _, err := identity.OpenIssuer(filepath.Join(s.Root, "state", "agent-issuer")); err != nil {
		t.Fatal(err)
	}
	if err := s.maintain(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(s.Root, "backups", time.Now().UTC().Format("2006-01-02"))
	for _, name := range []string{"key.pem", "ca.pem"} {
		source, e := os.ReadFile(filepath.Join(s.Root, "state", "agent-issuer", name))
		if e != nil {
			t.Fatal(e)
		}
		saved, e := os.ReadFile(filepath.Join(backup, "agent-issuer", name))
		if e != nil || !bytes.Equal(source, saved) {
			t.Fatal("issuer not backed up", e)
		}
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err := os.MkdirAll(filepath.Join(restored, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"controller.db", "master.key"} {
		data, err := os.ReadFile(filepath.Join(backup, name))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(restored, "state", name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	r := newAuthServer(t, restored)
	defer r.Close()
	br := &authBrowser{t: t, s: r}
	br.login(DefaultAdminPassword, 200)
}

func TestFactorThrottleSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	s := newAuthServer(t, root)
	for i := 0; i < 10; i++ {
		if err := s.factorLimit("admin"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.factorLimit("admin"); err == nil {
		t.Fatal("unbounded MFA attempts")
	}
	s.Close()
	s = newAuthServer(t, root)
	defer s.Close()
	if err := s.factorLimit("admin"); err == nil {
		t.Fatal("restart bypassed MFA throttle")
	}
	if err := s.factorLimit("different-user"); err != nil {
		t.Fatal("other user throttled", err)
	}
}

func TestTLSReloadKeepsValidCertificateDuringPartialRenewal(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	makePair := func(serial int64) ([]byte, []byte) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}, &x509.Certificate{SerialNumber: big.NewInt(serial)}, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		pk, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: pk})
	}
	write := func(path string, b []byte) {
		if err := store.AtomicWrite(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	c1, k1 := makePair(1)
	write(certPath, c1)
	write(keyPath, k1)
	cfg, err := ReloadingTLS(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := cfg.GetCertificate(nil)
	c2, k2 := makePair(2)
	write(certPath, c2)
	partial, _ := cfg.GetCertificate(nil)
	if !bytes.Equal(original.Certificate[0], partial.Certificate[0]) {
		t.Fatal("lost valid certificate")
	}
	write(keyPath, k2)
	updated, err := cfg.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	x, err := x509.ParseCertificate(updated.Certificate[0])
	if err != nil || x.SerialNumber.Int64() != 2 {
		t.Fatal("certificate not renewed", err)
	}
}

func TestSnapshotAlwaysIncludesBlockedOperations(t *testing.T) {
	s := newAuthServer(t, t.TempDir())
	defer s.Close()
	if err := s.DB.Update(func(tx *store.Tx) error {
		blocked := model.Operation{ID: "blocked", NodeID: "node", AppID: "app", Result: "needs_attention", CreatedAt: 1}
		if err := store.Put(tx, "operations", blocked.ID, blocked); err != nil {
			return err
		}
		for i := 0; i < 210; i++ {
			o := model.Operation{ID: model.ID(), Result: "succeeded", CreatedAt: model.Now()}
			if err := store.Put(tx, "operations", o.ID, o); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	b := &authBrowser{t: t, s: s}
	b.login(DefaultAdminPassword, 200)
	out := b.call("GET", "/snapshot", nil, 200)
	found := false
	for _, raw := range out["operations"].([]any) {
		if raw.(map[string]any)["id"] == "blocked" {
			found = true
		}
	}
	if !found {
		t.Fatal("blocked operation hidden by history")
	}
}
