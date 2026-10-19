package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
	"time"

	"deployer/internal/controller"
	"deployer/internal/model"
	"deployer/internal/testpki"
)

func TestControllerAgentLifecycle(t *testing.T) {
	server, err := controller.New(t.TempDir(), false, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	pki := testpki.New(t)
	server.ClientCA = pki.CA
	httpServer := httptest.NewUnstartedServer(server)
	httpServer.TLS = pki.ServerTLS(t)
	httpServer.StartTLS()
	defer httpServer.Close()
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: pki.BrowserTLS()}}
	csrf := ""
	call := func(method, path string, in any, status int, out any) {
		t.Helper()
		var body io.Reader
		if b, ok := in.([]byte); ok {
			body = bytes.NewReader(b)
		} else if in != nil {
			b, _ := json.Marshal(in)
			body = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, httpServer.URL+path, body)
		req.Header.Set("Content-Type", "application/json")
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		res, e := browser.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != status {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, res.StatusCode, status, b)
		}
		if out != nil {
			if e = json.Unmarshal(b, out); e != nil {
				t.Fatal(e)
			}
		}
	}
	var session struct {
		CSRF string `json:"csrf"`
	}
	call("POST", "/api/login", map[string]string{"username": "admin", "password": controller.DefaultAdminPassword}, 200, &session)
	call("POST", "/api/apps", model.App{ID: "sample", Name: "Sample"}, 403, nil)
	csrf = session.CSRF
	call("POST", "/api/apps", model.App{ID: "sample", Name: "Sample", Health: model.Health{Type: "process", TimeoutSeconds: 3, StableSeconds: 1}}, 200, nil)
	h := newHarness(t)
	v := h.version("1.0.0")
	raw, _ := os.ReadFile(h.archives[v.Version])
	upload := fmt.Sprintf("/api/apps/sample/versions?version=1.0.0&arch=%s", runtime.GOARCH)
	call("POST", upload, raw, 201, nil)
	call("POST", upload, raw, 200, nil)
	call("POST", upload, append(raw, []byte("different")...), 409, nil)
	sup := newFake()
	firstCert := pki.Node(t, "node-one")
	client, err := Open(t.TempDir(), httpServer.URL, TLSFiles{CA: firstCert.CA, Cert: firstCert.Cert, Key: firstCert.Key}, sup)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err = client.poll(context.Background()); err != nil {
		t.Fatal("automatic heartbeat registration", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	req := model.OperationRequest{Action: "deploy", Version: "1.0.0", IdempotencyKey: model.ID()}
	path := "/api/nodes/" + client.Identity.NodeID + "/apps/sample/operations"
	var op model.Operation
	call("POST", path, req, 202, &op)
	var duplicate model.Operation
	call("POST", path, req, 202, &duplicate)
	if duplicate.ID != op.ID {
		t.Fatal("request was not idempotent")
	}
	wait := func(id string) model.Operation {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			var o model.Operation
			call("GET", "/api/operations/"+id, nil, 200, &o)
			if o.Terminal() {
				return o
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("operation timed out")
		return model.Operation{}
	}
	if o := wait(op.ID); o.Result != "succeeded" {
		t.Fatal(o)
	}
	call("DELETE", "/api/apps/sample/versions/1.0.0?confirm=true", nil, 409, nil)
	req = model.OperationRequest{Action: "stop", ExpectedRevision: op.Revision, IdempotencyKey: model.ID()}
	call("POST", path, req, 202, &op)
	if o := wait(op.ID); o.Result != "succeeded" {
		t.Fatal(o)
	}
	var snapshot struct {
		Deployments []model.Deployment `json:"deployments"`
	}
	call("GET", "/api/snapshot", nil, 200, &snapshot)
	if len(snapshot.Deployments) != 1 || snapshot.Deployments[0].Current != "1.0.0" || snapshot.Deployments[0].RunState != "stopped" {
		t.Fatal(snapshot)
	}
	secondCert := pki.Node(t, "node-two")
	second, err := Open(t.TempDir(), httpServer.URL, TLSFiles{CA: secondCert.CA, Cert: secondCert.Cert, Key: secondCert.Key}, newFake())
	if err != nil {
		t.Fatal(err)
	}
	if err = second.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- second.Run(ctx2) }()
	defer func() {
		cancel2()
		if err := <-done2; err != nil {
			t.Error(err)
		}
		second.Close()
	}()
	var batch controller.Batch
	call("POST", "/api/batches", map[string]any{"app_id": "sample", "nodes": []string{client.Identity.NodeID, second.Identity.NodeID}, "request": model.OperationRequest{Action: "deploy", Version: "1.0.0"}}, 202, &batch)
	deadline := time.Now().Add(40 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		var state struct {
			Batches     []controller.Batch `json:"batches"`
			Deployments []model.Deployment `json:"deployments"`
		}
		call("GET", "/api/snapshot", nil, 200, &state)
		for _, b := range state.Batches {
			if b.ID == batch.ID && b.State != "running" {
				if b.State != "succeeded" {
					t.Fatal("batch failed", b.Error)
				}
				if len(state.Deployments) != 2 {
					t.Fatal("node deployments conflated")
				}
				for _, d := range state.Deployments {
					if d.Current != "1.0.0" || d.RunState != "running" {
						t.Fatal(d)
					}
				}
				completed = true
			}
		}
		if completed {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !completed {
		t.Fatal("batch timed out")
	}
	call("POST", "/api/users", map[string]any{"id": "reader", "name": "Reader", "role": "viewer", "password": "reader-password-123"}, 200, nil)
	call("POST", "/api/login", map[string]string{"username": "reader", "password": "reader-password-123"}, 200, &session)
	csrf = session.CSRF
	call("POST", "/api/apps", model.App{ID: "forbidden"}, 403, nil)
	call("GET", "/api/snapshot", nil, 200, nil)
}
