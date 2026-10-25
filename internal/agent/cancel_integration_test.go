package agent

import (
	"bytes"
	"context"
	"deployer/internal/controller"
	"deployer/internal/model"
	"deployer/internal/store"
	"deployer/internal/testpki"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
	"time"
)

func TestCancellationTravelsOverRealMTLSHeartbeat(t *testing.T) {
	server, err := controller.New(t.TempDir(), false, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	pki := testpki.New(t)
	server.ClientCA = pki.CA
	ts := httptest.NewUnstartedServer(server)
	ts.TLS = pki.ServerTLS(t)
	ts.StartTLS()
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	transport := &http.Transport{TLSClientConfig: pki.BrowserTLS()}
	defer transport.CloseIdleConnections()
	browser := &http.Client{Jar: jar, Transport: transport, Timeout: 5 * time.Second}
	csrf := ""
	call := func(method, path string, in any, want int, out any) {
		t.Helper()
		var raw []byte
		if data, ok := in.([]byte); ok {
			raw = data
		} else {
			raw, _ = json.Marshal(in)
		}
		req, _ := http.NewRequest(method, ts.URL+path, bytes.NewReader(raw))
		req.Header.Set("X-CSRF-Token", csrf)
		response, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, _ := io.ReadAll(response.Body)
		if response.StatusCode != want {
			t.Fatalf("%s %s: %d %s", method, path, response.StatusCode, data)
		}
		if out != nil {
			if err = json.Unmarshal(data, out); err != nil {
				t.Fatal(err)
			}
		}
	}
	var login struct {
		CSRF string `json:"csrf"`
	}
	call("POST", "/api/login", map[string]string{"username": "admin", "password": controller.DefaultAdminPassword}, 200, &login)
	csrf = login.CSRF
	call("POST", "/api/apps", model.App{ID: "sample", Name: "sample"}, 200, nil)
	h := newHarness(t)
	v := h.version("1")
	archive, err := os.ReadFile(h.archives["1"])
	if err != nil {
		t.Fatal(err)
	}
	call("POST", "/api/apps/sample/versions?version=1&arch="+runtime.GOARCH, archive, 201, nil)
	entered := make(chan struct{})
	sup := cancelSetup{h.sup, entered}
	cert := pki.Node(t, "cancel-node")
	client, err := Open(t.TempDir(), ts.URL, TLSFiles{CA: cert.CA, Cert: cert.Cert, Key: cert.Key}, sup)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err = client.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	var op model.Operation
	call("POST", "/api/nodes/"+client.Identity.NodeID+"/apps/sample/operations", model.OperationRequest{Action: "deploy", Version: v.Version, IdempotencyKey: model.ID()}, 202, &op)
	if err = client.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("setup did not start")
	}
	call("POST", "/api/operations/"+op.ID+"/cancel", nil, 200, nil)
	if err = client.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.workers.Wait()
	if err = client.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	call("GET", "/api/operations/"+op.ID, nil, 200, &op)
	if op.Result != "cancelled" {
		t.Fatal("controller did not receive cancellation", op)
	}
	if h.sup.starts != 0 {
		t.Fatal("cancelled setup started app")
	}
	if err = client.DB.View(func(tx *store.Tx) error {
		local, err := store.Get[model.Operation](tx, "operations", op.ID)
		if err == nil && !local.Acked {
			t.Error("cancellation not acknowledged")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
