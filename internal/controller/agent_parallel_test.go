package controller

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"deployer/internal/model"
	"deployer/internal/selfupdate"
	"deployer/internal/store"
)

type parallelRolloutFixture struct {
	s      *Server
	t      *testing.T
	digest string
}

func newParallelRolloutFixture(t *testing.T, count int) *parallelRolloutFixture {
	t.Helper()
	s, e := New(t.TempDir(), false, http.NotFoundHandler())
	if e != nil {
		t.Fatal(e)
	}
	f := &parallelRolloutFixture{s: s, t: t}
	t.Cleanup(func() { f.s.Close() })
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	s.UpdatePublicKey = filepath.Join(s.Root, "public.key")
	if e = os.WriteFile(s.UpdatePublicKey, []byte(base64.StdEncoding.EncodeToString(pub)), 0600); e != nil {
		t.Fatal(e)
	}
	binary := filepath.Join(s.Root, "fixture-agent")
	os.WriteFile(binary, []byte("fixture agent"), 0600)
	digest, size, e := selfupdate.Digest(binary)
	if e != nil {
		t.Fatal(e)
	}
	f.digest = digest
	m := selfupdate.Manifest{Version: "parallel", OS: "linux", Arch: "amd64", Size: size, Digest: digest, Format: selfupdate.Format, Compatibility: selfupdate.Compatibility}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, m.Payload()))
	os.MkdirAll(s.releasePath(digest), 0700)
	os.WriteFile(filepath.Join(s.releasePath(digest), "binary"), []byte("fixture agent"), 0600)
	if e = selfupdate.Save(filepath.Join(s.releasePath(digest), "manifest.json"), m); e != nil {
		t.Fatal(e)
	}
	r := AgentRollout{Digest: digest, Active: map[string]bool{}, Completed: map[string]bool{}}
	for i := 0; i < count; i++ {
		r.Nodes = append(r.Nodes, fmt.Sprintf("n%02d", i))
	}
	f.save(r)
	return f
}
func (f *parallelRolloutFixture) save(r AgentRollout) {
	f.t.Helper()
	if e := f.s.DB.Update(func(tx *store.Tx) error { return store.Put(tx, "settings", "agent_rollout", r) }); e != nil {
		f.t.Fatal(e)
	}
}
func (f *parallelRolloutFixture) get() AgentRollout {
	f.t.Helper()
	var r AgentRollout
	if e := f.s.DB.View(func(tx *store.Tx) error {
		var e error
		r, e = store.Get[AgentRollout](tx, "settings", "agent_rollout")
		return e
	}); e != nil {
		f.t.Fatal(e)
	}
	return r
}
func (f *parallelRolloutFixture) poll(id string, p model.Poll) model.PollResponse {
	f.t.Helper()
	var out model.PollResponse
	if e := f.s.DB.Update(func(tx *store.Tx) error { return f.s.planAgentUpdate(tx, model.Node{ID: id}, p, &out) }); e != nil {
		f.t.Fatal(e)
	}
	return out
}
func (f *parallelRolloutFixture) download(id string) error {
	f.t.Helper()
	req := httptest.NewRequest("GET", "/agent/releases/"+f.digest, nil)
	req.SetPathValue("digest", f.digest)
	req = req.WithContext(context.WithValue(req.Context(), contextKey("node"), model.Node{ID: id}))
	w := httptest.NewRecorder()
	e := f.s.downloadAgentRelease(w, req)
	if e == nil && w.Body.String() != "fixture agent" {
		f.t.Fatal("wrong release body", w.Body.String())
	}
	return e
}
func (f *parallelRolloutFixture) pause(paused bool) {
	f.t.Helper()
	body := fmt.Sprintf(`{"paused":%t}`, paused)
	if e := f.s.setAgentRollout(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/agent-rollout", strings.NewReader(body))); e != nil {
		f.t.Fatal(e)
	}
}

func TestParallelAgentRolloutBoundedAndDurable(t *testing.T) {
	f := newParallelRolloutFixture(t, 32)
	capable := model.Poll{AgentUpdate: selfupdate.Status{Capable: true}}
	var wg sync.WaitGroup
	offers := make(chan bool, 31)
	errs := make(chan error, 31)
	// Leave the first host offline. Concurrent heartbeats still fill all slots.
	for i := 1; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var out model.PollResponse
			e := f.s.DB.Update(func(tx *store.Tx) error {
				return f.s.planAgentUpdate(tx, model.Node{ID: fmt.Sprintf("n%02d", i)}, capable, &out)
			})
			if e != nil {
				errs <- e
			}
			offers <- out.Update != nil
		}(i)
	}
	wg.Wait()
	close(offers)
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	count := 0
	for offered := range offers {
		if offered {
			count++
		}
	}
	if count != 20 || len(f.get().Active) != 20 {
		t.Fatal("concurrency cap", count, f.get())
	}
	if f.poll("outsider", capable).Update != nil {
		t.Fatal("unselected host offered release")
	}
	if f.download("outsider") == nil || f.download("n00") == nil {
		t.Fatal("download bypassed reservation")
	}
	r := f.get()
	var active, queued string
	for _, id := range r.Nodes {
		if r.Active[id] {
			active = id
		} else if id != "n00" {
			queued = id
		}
	}
	if f.download(active) != nil {
		t.Fatal("active download denied")
	}
	if f.poll(active, capable).Update == nil || len(f.get().Active) != 20 {
		t.Fatal("retry did not retain slot")
	}
	// Reopen the actual database/server, then try to exceed its persisted cap.
	root, key := f.s.Root, f.s.UpdatePublicKey
	f.s.Close()
	var e error
	f.s, e = New(root, false, http.NotFoundHandler())
	if e != nil {
		t.Fatal(e)
	}
	f.s.UpdatePublicKey = key
	if f.poll(queued, capable).Update != nil {
		t.Fatal("restart lost in-flight reservations")
	}
	committed := model.Poll{AgentUpdate: selfupdate.Status{Capable: true, Digest: f.digest, Phase: "committed"}}
	trial := committed
	trial.Trial = true
	f.poll(active, trial)
	if f.get().Completed[active] {
		t.Fatal("trial counted as committed")
	}
	f.poll(active, committed)
	r = f.get()
	if !r.Completed[active] || r.Active[active] || r.Index != 0 {
		t.Fatal("out-of-order completion", r)
	}
	if f.poll("n00", capable).Update == nil {
		t.Fatal("completion did not immediately release slot")
	}
	if len(f.get().Active) != 20 {
		t.Fatal("invalid active count", f.get())
	}
}

func TestParallelAgentRolloutPauseFailureAndCompletion(t *testing.T) {
	f := newParallelRolloutFixture(t, 4)
	capable := model.Poll{AgentUpdate: selfupdate.Status{Capable: true}}
	for _, id := range []string{"n00", "n01"} {
		if f.poll(id, capable).Update == nil {
			t.Fatal("no offer", id)
		}
	}
	f.pause(true)
	if f.poll("n02", capable).Update != nil || f.download("n00") == nil {
		t.Fatal("pause did not block dispatch/download")
	}
	committed := model.Poll{AgentUpdate: selfupdate.Status{Capable: true, Digest: f.digest, Phase: "committed"}}
	f.poll("n01", committed)
	if !f.get().Completed["n01"] || !f.get().Paused {
		t.Fatal("paused batch lost in-flight completion")
	}
	f.pause(false)
	if f.poll("n02", capable).Update == nil {
		t.Fatal("resume did not dispatch")
	}
	f.poll("n02", model.Poll{AgentUpdate: selfupdate.Status{Target: f.digest, Phase: "failed", Error: "candidate crashed"}})
	r := f.get()
	if !r.Paused || !strings.Contains(r.Error, "n02") || !strings.Contains(r.Error, "candidate crashed") {
		t.Fatal(r)
	}
	if f.poll("n03", capable).Update != nil || f.download("n00") == nil {
		t.Fatal("failure did not stop batch")
	}
	f.poll("n00", committed)
	r = f.get()
	if r.Index != 2 || !r.Paused || len(r.Completed) != 2 {
		t.Fatal("completion after failure", r)
	}
	// No timeout or forced restart of other hosts on failure.
	if !r.Active["n02"] {
		t.Fatal("failed host identity lost")
	}
	req := httptest.NewRequest("POST", "/api/agent-rollout", bytes.NewBufferString(`{"paused":false}`))
	if f.s.setAgentRollout(httptest.NewRecorder(), req) == nil {
		t.Fatal("failed batch resumed without review")
	}
}

func TestParallelAgentRolloutMigratesSerialBatch(t *testing.T) {
	f := newParallelRolloutFixture(t, 25)
	r := f.get()
	r.Index = 2
	r.Active = nil
	r.Completed = nil
	f.save(r)
	if f.download("n02") != nil {
		t.Fatal("migration revoked active serial download")
	}
	if f.poll("n24", model.Poll{AgentUpdate: selfupdate.Status{Capable: true}}).Update == nil {
		t.Fatal("legacy batch did not become parallel")
	}
	r = f.get()
	if !r.Completed["n00"] || !r.Completed["n01"] || !r.Active["n02"] || !r.Active["n24"] || r.Index != 2 {
		t.Fatal(r)
	}
}

func TestAgentReleaseCompressedTransportPreservesSignedBinary(t *testing.T) {
	f := newParallelRolloutFixture(t, 1)
	f.poll("n00", model.Poll{AgentUpdate: selfupdate.Status{Capable: true}})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("digest", f.digest)
		r = r.WithContext(context.WithValue(r.Context(), contextKey("node"), model.Node{ID: "n00"}))
		if err := f.s.downloadAgentRelease(w, r); err != nil {
			http.Error(w, err.Error(), 403)
		}
	})
	ts := httptest.NewTLSServer(handler)
	defer ts.Close()
	// The old Agent's default Go transport transparently decompresses the body.
	response, err := ts.Client().Get(ts.URL + "/agent/releases/" + f.digest)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !response.Uncompressed || string(data) != "fixture agent" {
		t.Fatal("Agent did not receive original representation", response.Uncompressed, string(data))
	}
	m, err := f.s.release(f.digest)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "downloaded")
	os.WriteFile(file, data, 0600)
	if err = m.CheckFile(file); err != nil {
		t.Fatal("signed size/digest verification failed", err)
	}
	cached := filepath.Join(f.s.releasePath(f.digest), "binary.gz")
	before, err := os.Stat(cached)
	if err != nil {
		t.Fatal(err)
	}
	// Explicit gzip clients receive an encoded body; the immutable cache is reused.
	req := httptest.NewRequest("GET", "/agent/releases/"+f.digest, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	reader, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || string(decoded) != "fixture agent" || w.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatal("invalid gzip response", err)
	}
	after, _ := os.Stat(cached)
	if !os.SameFile(before, after) {
		t.Fatal("cache regenerated")
	}
	for _, encoding := range []string{"", "identity", "gzip;q=0", "gzip;q=NaN"} {
		req = httptest.NewRequest("GET", "/agent/releases/"+f.digest, nil)
		req.Header.Set("Accept-Encoding", encoding)
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Header().Get("Content-Encoding") != "" || w.Body.String() != "fixture agent" {
			t.Fatal("unexpected gzip", encoding)
		}
	}
	req = httptest.NewRequest("GET", "/agent/releases/"+f.digest, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Range", "bytes=0-3")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent || w.Body.String() != "fixt" || w.Header().Get("Content-Encoding") != "" {
		t.Fatal("range used wrong representation", w.Code, w.Body.String())
	}
	f.pause(true)
	req = httptest.NewRequest("GET", "/agent/releases/"+f.digest, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatal("compressed cache bypassed pause", w.Code)
	}
}
