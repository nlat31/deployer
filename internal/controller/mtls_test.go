package controller

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"deployer/internal/model"
	"deployer/internal/store"
	"deployer/internal/testpki"
)

func newMTLSTestServer(t *testing.T) (*Server, *testpki.PKI, *httptest.Server) {
	t.Helper()
	pki := testpki.New(t)
	s := newAuthServer(t, t.TempDir())
	s.ClientCA = pki.CA
	cfg, err := MTLSConfig(pki.ServerCert, pki.ServerKey, pki.CA)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(pki.ServerCert, pki.ServerKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Certificates = []tls.Certificate{pair}
	ts := httptest.NewUnstartedServer(s)
	ts.TLS = cfg
	ts.StartTLS()
	t.Cleanup(func() { ts.Close(); s.Close() })
	return s, pki, ts
}
func certClient(t *testing.T, pki *testpki.PKI, files *testpki.Files) *http.Client {
	t.Helper()
	cfg := pki.BrowserTLS()
	if files != nil {
		pair, e := tls.LoadX509KeyPair(files.Cert, files.Key)
		if e != nil {
			t.Fatal(e)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	tr := &http.Transport{TLSClientConfig: cfg}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 5 * time.Second}
}
func pollRequest(t *testing.T, c *http.Client, ts *httptest.Server, p model.Poll, want int) {
	t.Helper()
	p.Protocol = model.Protocol
	if p.Name == "" {
		p.Name = "test host"
	}
	p.OS = "linux"
	p.Arch = "amd64"
	b, _ := json.Marshal(p)
	req, _ := http.NewRequest("POST", ts.URL+"/agent/poll", bytes.NewReader(b))
	req.Header.Set("X-Node-ID", "forged-node")
	req.Header.Set("X-Forwarded-For", "203.0.113.99")
	req.Header.Set("X-Real-IP", "203.0.113.98")
	r, e := c.Do(req)
	if want == 0 {
		if e == nil {
			r.Body.Close()
			t.Fatal("untrusted client completed TLS handshake")
		}
		return
	}
	if e != nil {
		t.Fatal(e)
	}
	defer r.Body.Close()
	raw, _ := io.ReadAll(r.Body)
	if r.StatusCode != want {
		t.Fatalf("poll: %d, want %d: %s", r.StatusCode, want, raw)
	}
}
func TestMTLSHeartbeatIdentityTrustAndRenewal(t *testing.T) {
	s, pki, ts := newMTLSTestServer(t)
	pollRequest(t, certClient(t, pki, nil), ts, model.Poll{}, 401)
	wrong := testpki.New(t).Node(t, "wrong-ca")
	pollRequest(t, certClient(t, pki, &wrong), ts, model.Poll{}, 0)
	expired := pki.Client(t, "spiffe://deployer/nodes/expired", time.Now().Add(-time.Hour))
	pollRequest(t, certClient(t, pki, &expired), ts, model.Poll{}, 0)
	badID := pki.Client(t, "spiffe://other/nodes/fake", time.Now().Add(time.Hour))
	pollRequest(t, certClient(t, pki, &badID), ts, model.Poll{}, 401)
	if err := s.DB.View(func(tx *store.Tx) error {
		ns, e := store.List[model.Node](tx, "nodes")
		if len(ns) != 0 {
			t.Fatal("invalid heartbeat registered host", ns)
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	first := pki.Node(t, "stable-id")
	c := certClient(t, pki, &first)
	pollRequest(t, c, ts, model.Poll{Name: "first name"}, 200)
	renewed := pki.Client(t, "spiffe://deployer/nodes/stable-id", time.Now().Add(2*time.Hour))
	pollRequest(t, certClient(t, pki, &renewed), ts, model.Poll{Name: "renewed host"}, 200)
	if err := s.DB.View(func(tx *store.Tx) error {
		ns, e := store.List[model.Node](tx, "nodes")
		if len(ns) != 1 || ns[0].ID != "stable-id" || ns[0].Name != "renewed host" || ns[0].IP != "127.0.0.1" || ns[0].CertificateExpires < time.Now().Add(90*time.Minute).UnixMilli() {
			t.Fatalf("identity changed on renewal or trusted header: %+v", ns)
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	// Removing a CA must also reject an existing HTTP keep-alive connection.
	other := testpki.New(t)
	b, err := os.ReadFile(other.CA)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.AtomicWrite(pki.CA, b, 0600); err != nil {
		t.Fatal(err)
	}
	pollRequest(t, c, ts, model.Poll{}, 401)
}

func TestOfflineHostManualDeleteAndAuthenticatedReappearance(t *testing.T) {
	s, pki, ts := newMTLSTestServer(t)
	cert := pki.Node(t, "host-a")
	c := certClient(t, pki, &cert)
	pollRequest(t, c, ts, model.Poll{}, 200)
	b := &authBrowser{t: t, s: s}
	b.login(DefaultAdminPassword, 200)
	b.call("DELETE", "/nodes/host-a?confirm=true", nil, 409)
	if err := s.DB.Update(func(tx *store.Tx) error {
		n, e := store.Get[model.Node](tx, "nodes", "host-a")
		if e != nil {
			return e
		}
		n.LastSeen = model.Now() - 31000
		if e = store.Put(tx, "nodes", n.ID, n); e != nil {
			return e
		}
		if e = store.Put(tx, "deployments", model.NodeKey(n.ID, "app"), model.Deployment{NodeID: n.ID, AppID: "app", Revision: 2, Current: "1"}); e != nil {
			return e
		}
		if e = store.Put(tx, "inventories", n.ID, []model.Installation{{AppID: "app", Version: "1"}}); e != nil {
			return e
		}
		if e = store.Put(tx, "operations", "pending", model.Operation{ID: "pending", NodeID: n.ID, AppID: "app", Revision: 2}); e != nil {
			return e
		}
		return store.Put(tx, "batches", "batch", Batch{ID: "batch", Nodes: []string{n.ID}, State: "running", CurrentOperation: "pending"})
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Maintain(); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.View(func(tx *store.Tx) error { _, e := store.Get[model.Node](tx, "nodes", "host-a"); return e }); err != nil {
		t.Fatal("offline host automatically removed", err)
	}
	b.call("DELETE", "/nodes/host-a", nil, 400)
	b.call("DELETE", "/nodes/host-a?confirm=true", nil, 200)
	if err := s.DB.View(func(tx *store.Tx) error {
		for _, kind := range []string{"nodes", "inventories"} {
			if _, e := store.Get[json.RawMessage](tx, kind, "host-a"); e != store.ErrNotFound {
				t.Fatal("host record retained", kind, e)
			}
		}
		if _, e := store.Get[model.Deployment](tx, "deployments", model.NodeKey("host-a", "app")); e != store.ErrNotFound {
			t.Fatal("deployment retained", e)
		}
		o, e := store.Get[model.Operation](tx, "operations", "pending")
		if e != nil {
			return e
		}
		if o.Result != "failed" {
			t.Fatal("deleted host task still dispatchable")
		}
		batch, e := store.Get[Batch](tx, "batches", "batch")
		if e != nil {
			return e
		}
		if batch.State != "running" {
			t.Fatal("deleting an offline node aborted the remaining batch")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pollRequest(t, c, ts, model.Poll{Deployments: []model.Deployment{{AppID: "app", Revision: 2, Current: "1", DesiredState: "running", RunState: "running"}}}, 200)
	if err := s.DB.View(func(tx *store.Tx) error {
		if _, e := store.Get[model.Node](tx, "nodes", "host-a"); e != nil {
			return e
		}
		ops, e := store.Query[model.Operation](tx, "operations", store.Filter{Owner: "host-a", Scope: "app", States: []string{"needs_attention"}})
		if len(ops) == 0 {
			t.Fatal("returning host state not quarantined")
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeatAcceptsRemovedOptionalFieldsDuringRollingUpdate(t *testing.T) {
	s, pki, ts := newMTLSTestServer(t)
	files := pki.Node(t, "older-agent")
	client := certClient(t, pki, &files)
	pollRequest(t, client, ts, model.Poll{}, 200)
	if err := s.DB.Update(func(tx *store.Tx) error {
		return store.Put(tx, "deployments", model.NodeKey("older-agent", "app"), model.Deployment{NodeID: "older-agent", AppID: "app", Revision: 1})
	}); err != nil {
		t.Fatal(err)
	}
	body := `{"protocol":3,"name":"older agent","os":"linux","arch":"amd64","deployments":[{"app_id":"app","revision":1,"config":{"auto_rollback":true},"previous_config":{"auto_rollback":true}}]}`
	response, err := client.Post(ts.URL+"/agent/poll", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		raw, _ := io.ReadAll(response.Body)
		t.Fatal(response.StatusCode, string(raw))
	}
	// Tolerance is limited to heartbeat payloads, not administrator commands.
	request := httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"action":"deploy","auto_rollback":true}`))
	var command model.OperationRequest
	if err := decode(httptest.NewRecorder(), request, &command); err == nil {
		t.Fatal("operator request accepted removed option")
	}
	request = httptest.NewRequest("POST", "/", bytes.NewBufferString(`{"protocol":3} {}`))
	var heartbeat model.Poll
	if err := decodeLimit(httptest.NewRecorder(), request, &heartbeat, 16<<20); err == nil {
		t.Fatal("heartbeat accepted trailing content")
	}
}

func TestHeartbeatPersistsCompleteVersionInventory(t *testing.T) {
	s, pki, ts := newMTLSTestServer(t)
	files := pki.Node(t, "inventory-node")
	c := certClient(t, pki, &files)
	pollRequest(t, c, ts, model.Poll{}, 200)
	deps := []model.Deployment{{AppID: "app-a", Current: "2", RunningVersion: "2", RunState: "running", Revision: 1}, {AppID: "app-b", Current: "1", RunState: "stopped", Revision: 1}}
	if err := s.DB.Update(func(tx *store.Tx) error {
		for _, d := range deps {
			if err := store.Put(tx, "deployments", model.NodeKey("inventory-node", d.AppID), d); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	items := []model.Installation{{AppID: "app-a", Version: "1", InstalledAt: 1000, StartedAt: 2000}, {AppID: "app-a", Version: "2", InstalledAt: 3000, StartedAt: 4000}, {AppID: "app-b", Version: "1", InstalledAt: 5000}}
	for n := 0; n < 2; n++ {
		pollRequest(t, c, ts, model.Poll{Deployments: deps, Installations: items}, 200)
		if err := s.DB.View(func(tx *store.Tx) error {
			got, err := store.Get[[]model.Installation](tx, "inventories", "inventory-node")
			if err != nil {
				return err
			}
			if len(got) != len(items) {
				t.Fatalf("incomplete inventory: %+v", got)
			}
			for i := range got {
				if got[i] != items[i] {
					t.Fatalf("heartbeat changed lifecycle times: %+v", got)
				}
			}
			for _, want := range deps {
				d, err := store.Get[model.Deployment](tx, "deployments", model.NodeKey("inventory-node", want.AppID))
				if err != nil {
					return err
				}
				if d.Current != want.Current || d.RunningVersion != want.RunningVersion || d.RunState != want.RunState {
					t.Fatalf("incorrect runtime state: %+v", d)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
