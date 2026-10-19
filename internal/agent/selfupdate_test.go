package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"deployer/internal/launcher"
	"deployer/internal/selfupdate"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpdateDownloadVerificationAndFailureSuppression(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DEPLOYER_INSTALL_ROOT", root)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	os.MkdirAll(filepath.Join(root, "config"), 0700)
	os.WriteFile(filepath.Join(root, "config", "update-public.key"), []byte(base64.StdEncoding.EncodeToString(pub)), 0600)
	raw := []byte("candidate binary")
	file := filepath.Join(t.TempDir(), "binary")
	os.WriteFile(file, raw, 0700)
	d, n, _ := selfupdate.Digest(file)
	m := selfupdate.Manifest{Version: "next", OS: "linux", Arch: runtime.GOARCH, Size: n, Digest: d, Format: selfupdate.Format, Compatibility: selfupdate.Compatibility}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, m.Payload()))
	selfupdate.Save(filepath.Join(root, "updater", "agent.json"), launcher.State{Current: "old", Phase: "committed"})
	var corrupt atomic.Bool
	corrupt.Store(true)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if corrupt.Load() {
			w.Write([]byte("wrong"))
		} else {
			w.Write(raw)
		}
	}))
	defer server.Close()
	c := &Client{HTTP: server.Client(), Identity: Identity{Controller: server.URL}}
	if e := c.fetchUpdate(context.Background(), m); e == nil {
		t.Fatal("accepted corrupt download")
	}
	if _, e := os.Stat(filepath.Join(root, "updater", "agent-request.json")); !os.IsNotExist(e) {
		t.Fatal("request saved before validation")
	}
	corrupt.Store(false)
	if e := c.fetchUpdate(context.Background(), m); e != nil {
		t.Fatal(e)
	}
	var request selfupdate.Manifest
	if e := selfupdate.Load(filepath.Join(root, "updater", "agent-request.json"), &request); e != nil || request.Digest != d {
		t.Fatal(e, request)
	}
	before := requests.Load()
	selfupdate.Save(filepath.Join(root, "updater", "agent.json"), launcher.State{Current: "old", Phase: "failed", Failed: []string{d}})
	if e := c.fetchUpdate(context.Background(), m); e != nil {
		t.Fatal(e)
	}
	if requests.Load() != before {
		t.Fatal("retried failed release")
	}
	// A different release is not delayed by the previous release's retry timer.
	selfupdate.Save(filepath.Join(root, "updater", "agent.json"), launcher.State{Current: "old", Phase: "committed"})
	requestPath := filepath.Join(root, "updater", "agent-request.json")
	os.Remove(requestPath)
	c.nextUpdate = time.Now().Add(time.Hour)
	c.lastUpdate = "other"
	c.scheduleUpdate(context.Background(), &m)
	c.workers.Wait()
	if _, e := os.Stat(requestPath); e != nil {
		t.Fatal("new target delayed by old backoff", e)
	}
	os.Remove(requestPath)
	c.scheduleUpdate(context.Background(), &m)
	c.workers.Wait()
	if _, e := os.Stat(requestPath); !os.IsNotExist(e) {
		t.Fatal("same target ignored retry backoff")
	}
	m.Version = "tampered"
	if e := c.fetchUpdate(context.Background(), m); e == nil {
		t.Fatal("accepted forged manifest")
	}
}
