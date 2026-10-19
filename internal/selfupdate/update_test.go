package selfupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestSignedReleaseRejectsTampering(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	path := filepath.Join(t.TempDir(), "binary")
	os.WriteFile(path, []byte("binary"), 0700)
	d, n, _ := Digest(path)
	m := Manifest{Version: "1", OS: "linux", Arch: "amd64", Size: n, Digest: d, Compatibility: Compatibility, Format: Format}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, m.Payload()))
	if e := m.Verify(pub); e != nil {
		t.Fatal(e)
	}
	if e := m.CheckFile(path); e != nil {
		t.Fatal(e)
	}
	for _, modify := range []func(*Manifest){func(m *Manifest) { m.Arch = "arm64" }, func(m *Manifest) { m.Version = "2" }, func(m *Manifest) { m.Size++ }, func(m *Manifest) { m.Compatibility = "new" }, func(m *Manifest) { m.Format++ }, func(m *Manifest) { m.Digest = "../binary" }} {
		bad := m
		modify(&bad)
		if bad.Verify(pub) == nil {
			t.Fatal("tampered metadata accepted")
		}
	}
	os.WriteFile(path, []byte("broken"), 0700)
	if m.CheckFile(path) == nil {
		t.Fatal("corrupt binary accepted")
	}
}
func TestAtomicReplace(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state")
	for _, s := range []string{"old", "new"} {
		if e := Atomic(p, []byte(s), 0600); e != nil {
			t.Fatal(e)
		}
		b, e := os.ReadFile(p)
		if e != nil || string(b) != s {
			t.Fatal(e, string(b))
		}
	}
}
