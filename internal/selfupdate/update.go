// Package selfupdate defines the signed, backwards-compatible Agent release format.
package selfupdate

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const MaxBinary = 64 << 20
const Format = 1

// Compatibility is deliberately exact. Bump when rollback cannot read state.
const Compatibility = "protocol3-state3-process1"

var Version = "0.2.4"
var Commit = "unknown"

type Manifest struct {
	Version       string `json:"version"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	Size          int64  `json:"size"`
	Digest        string `json:"digest"`
	Compatibility string `json:"compatibility"`
	Format        int    `json:"format"`
	Signature     string `json:"signature"`
}
type Status struct {
	Version string `json:"version"`
	Digest  string `json:"digest"`
	Target  string `json:"target,omitempty"`
	Phase   string `json:"phase,omitempty"`
	Error   string `json:"error,omitempty"`
	Capable bool   `json:"capable"`
}

func ValidDigest(s string) bool    { return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == "" }
func (m Manifest) Payload() []byte { m.Signature = ""; b, _ := json.Marshal(m); return b }
func (m Manifest) Verify(key []byte) error {
	if m.Format != Format || m.Compatibility != Compatibility || m.OS != "linux" || (m.Arch != "amd64" && m.Arch != "arm64") || !ValidDigest(m.Digest) || m.Size <= 0 || m.Size > MaxBinary || len(m.Version) == 0 || len(m.Version) > 96 || strings.Trim(m.Version, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-") != "" || m.Version == "." || m.Version == ".." {
		return fmt.Errorf("unsupported release metadata")
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, m.Payload(), sig) {
		return fmt.Errorf("invalid release signature")
	}
	return nil
}
func (m Manifest) Platform() error {
	if m.OS != runtime.GOOS || m.Arch != runtime.GOARCH {
		return fmt.Errorf("release platform mismatch")
	}
	return nil
}
func ReadKey(path string) ([]byte, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
}
func Digest(path string) (string, int64, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", 0, e
	}
	defer f.Close()
	h := sha256.New()
	n, e := io.Copy(h, io.LimitReader(f, MaxBinary+1))
	if n > MaxBinary {
		e = fmt.Errorf("binary too large")
	}
	return hex.EncodeToString(h.Sum(nil)), n, e
}
func (m Manifest) CheckFile(path string) error {
	d, n, e := Digest(path)
	if e != nil {
		return e
	}
	if d != m.Digest || n != m.Size {
		return fmt.Errorf("binary size or digest mismatch")
	}
	return nil
}

// Atomic syncs both data and directory before reporting success.
func Atomic(path string, data []byte, mode os.FileMode) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".write-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(mode); e == nil {
		_, e = f.Write(data)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	return SyncDir(filepath.Dir(path))
}
func Save(path string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return Atomic(path, b, 0600)
}
func Load(path string, v any) error {
	b, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func SyncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
