package controller

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"deployer/internal/model"
	"deployer/internal/selfupdate"
	"deployer/internal/store"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAgentReleaseAndSequentialRollout(t *testing.T) {
	s, e := New(t.TempDir(), false, http.NotFoundHandler())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	s.UpdatePublicKey = filepath.Join(t.TempDir(), "public")
	os.WriteFile(s.UpdatePublicKey, []byte(base64.StdEncoding.EncodeToString(pub)), 0600)
	binary := filepath.Join(t.TempDir(), "agent")
	os.WriteFile(binary, []byte("test agent"), 0600)
	digest, size, _ := selfupdate.Digest(binary)
	m := selfupdate.Manifest{Version: "1", OS: "linux", Arch: "amd64", Size: size, Digest: digest, Format: selfupdate.Format, Compatibility: selfupdate.Compatibility}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, m.Payload()))
	upload := func(raw []byte) error {
		var b bytes.Buffer
		w := multipart.NewWriter(&b)
		j, _ := json.Marshal(m)
		w.WriteField("manifest", string(j))
		part, _ := w.CreateFormFile("binary", "agent")
		part.Write(raw)
		w.Close()
		req := httptest.NewRequest("POST", "/api/agent-releases", &b)
		req.Header.Set("Content-Type", w.FormDataContentType())
		return s.uploadAgentRelease(httptest.NewRecorder(), req)
	}
	if e = upload([]byte("corrupted!")); e == nil {
		t.Fatal("accepted corrupt upload")
	}
	if e = upload([]byte("test agent")); e != nil {
		t.Fatal(e)
	}
	if e = upload([]byte("test agent")); e != nil {
		t.Fatal("idempotent upload", e)
	}
	listed := httptest.NewRecorder()
	if e = s.agentReleases(listed, httptest.NewRequest("GET", "/api/agent-releases", nil)); e != nil {
		t.Fatal(e)
	}
	var releases []selfupdate.Manifest
	if e = json.Unmarshal(listed.Body.Bytes(), &releases); e != nil || len(releases) != 1 || releases[0].Digest != digest {
		t.Fatal("release list", listed.Body.String(), e)
	}
	roll := AgentRollout{Digest: digest, Nodes: []string{"n1", "n2"}}
	if e = s.DB.Update(func(tx *store.Tx) error { return store.Put(tx, "settings", "agent_rollout", roll) }); e != nil {
		t.Fatal(e)
	}
	poll := func(id string, status selfupdate.Status) model.PollResponse {
		var out model.PollResponse
		if e := s.DB.Update(func(tx *store.Tx) error {
			return s.planAgentUpdate(tx, model.Node{ID: id}, model.Poll{AgentUpdate: status}, &out)
		}); e != nil {
			t.Fatal(e)
		}
		return out
	}
	if poll("n2", selfupdate.Status{Capable: true}).Update != nil {
		t.Fatal("parallel rollout")
	}
	if poll("n1", selfupdate.Status{Capable: true}).Update == nil {
		t.Fatal("missing target")
	}
	status := selfupdate.Status{Capable: true, Digest: digest, Phase: "committed"}
	poll("n1", status)
	s.DB.Update(func(tx *store.Tx) error {
		r, _ := store.Get[AgentRollout](tx, "settings", "agent_rollout")
		r.HealthySince = model.Now() - int64(6*time.Minute/time.Millisecond)
		return store.Put(tx, "settings", "agent_rollout", r)
	})
	poll("n1", status)
	if poll("n2", selfupdate.Status{Capable: true}).Update == nil {
		t.Fatal("next host not released")
	}
	poll("n2", selfupdate.Status{Target: digest, Phase: "failed", Error: "crash"})
	s.DB.View(func(tx *store.Tx) error {
		r, _ := store.Get[AgentRollout](tx, "settings", "agent_rollout")
		if !r.Paused || r.Error != "crash" {
			t.Fatal(r)
		}
		return nil
	})
	if poll("n2", selfupdate.Status{Capable: true}).Update != nil {
		t.Fatal("failed rollout continued")
	}
}
func TestAgentReleaseRequiresAdmin(t *testing.T) {
	s, e := New(t.TempDir(), false, http.NotFoundHandler())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	for _, path := range []string{"/api/agent-releases", "/api/agent-rollout"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
		if w.Code != 401 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/agent/releases/"+string(bytes.Repeat([]byte("a"), 64)), nil))
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}

func TestMissingAgentReleasePausesWithoutBreakingHeartbeat(t *testing.T) {
	s, e := New(t.TempDir(), false, http.NotFoundHandler())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	digest := string(bytes.Repeat([]byte("a"), 64))
	e = s.DB.Update(func(tx *store.Tx) error {
		if e := store.Put(tx, "settings", "agent_rollout", AgentRollout{Digest: digest, Nodes: []string{"n1"}}); e != nil {
			return e
		}
		var out model.PollResponse
		if e := s.planAgentUpdate(tx, model.Node{ID: "n1"}, model.Poll{AgentUpdate: selfupdate.Status{Capable: true}}, &out); e != nil {
			return e
		}
		if out.Update != nil {
			t.Fatal("missing release dispatched")
		}
		roll, e := store.Get[AgentRollout](tx, "settings", "agent_rollout")
		if e == nil && (!roll.Paused || roll.Error == "") {
			t.Fatal(roll)
		}
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
}
