package controller

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"deployer/internal/model"
	"deployer/internal/store"
	"deployer/internal/testpki"
)

func TestRestoredControllerQuarantinesStaleTasksAndCanReconcile(t *testing.T) {
	s := newAuthServer(t, t.TempDir())
	defer s.Close()
	pki := testpki.New(t)
	cert := pki.Node(t, "node")
	s.ClientCA = pki.CA
	pair, _ := tls.LoadX509KeyPair(cert.Cert, cert.Key)
	leaf, _ := x509.ParseCertificate(pair.Certificate[0])
	n := model.Node{ID: "node", OS: "linux", Arch: "amd64"}
	if err := s.DB.Update(func(tx *store.Tx) error {
		if err := store.Put(tx, "nodes", n.ID, n); err != nil {
			return err
		}
		if err := store.Put(tx, "restored_nodes", n.ID, true); err != nil {
			return err
		}
		if err := store.Put(tx, "deployments", model.NodeKey(n.ID, "app"), model.Deployment{NodeID: n.ID, AppID: "app", Revision: 1}); err != nil {
			return err
		}
		o := model.Operation{ID: "old-clean", NodeID: n.ID, AppID: "app", Action: "clean_data", Revision: 1, Stage: "dispatched"}
		return store.Put(tx, "operations", o.ID, o)
	}); err != nil {
		t.Fatal(err)
	}
	poll := func(p model.Poll) model.PollResponse {
		p.Protocol = model.Protocol
		p.Name = "node"
		p.OS = "linux"
		p.Arch = "amd64"
		b, _ := json.Marshal(p)
		r := httptest.NewRequest("POST", "http://test/agent/poll", bytes.NewReader(b))
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("poll: %d %s", w.Code, w.Body.String())
		}
		var out model.PollResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	d := model.Deployment{NodeID: n.ID, AppID: "app", Revision: 4, Current: "2", DesiredState: "running", RunState: "running"}
	late := model.Operation{ID: "lost-result", AppID: "app", Revision: 4, Result: "succeeded"}
	out := poll(model.Poll{Deployments: []model.Deployment{d}, Operations: []model.Operation{late}, Events: []model.Event{{OperationID: "lost-log", Seq: 20}}})
	if len(out.Tasks) != 0 || len(out.Acknowledged) != 1 || out.EventAcks["lost-log"] != 20 {
		t.Fatalf("unsafe recovery response: %+v", out)
	}
	if err := s.DB.View(func(tx *store.Tx) error {
		ops, err := store.Query[model.Operation](tx, "operations", store.Filter{Owner: n.ID, Scope: "app", States: []string{"needs_attention"}})
		if len(ops) != 3 {
			t.Fatalf("missing quarantine: %+v", ops)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A confirmed reconciliation must release controller-only blocks as well.
	if err := s.DB.Update(func(tx *store.Tx) error {
		return store.Put(tx, "operations", "reconcile", model.Operation{ID: "reconcile", NodeID: n.ID, AppID: "app", Action: "reconcile", Revision: 5})
	}); err != nil {
		t.Fatal(err)
	}
	d.Revision = 5
	d.DesiredState = "stopped"
	d.RunState = "stopped"
	if err := s.DB.Update(func(tx *store.Tx) error { return store.Put(tx, "deployments", model.NodeKey(n.ID, "app"), d) }); err != nil {
		t.Fatal(err)
	}
	poll(model.Poll{Deployments: []model.Deployment{d}, Operations: []model.Operation{{ID: "reconcile", Result: "succeeded", Stage: "finished"}}})
	if err := s.DB.View(func(tx *store.Tx) error {
		ops, err := store.Query[model.Operation](tx, "operations", store.Filter{Owner: n.ID, Scope: "app", States: []string{"active", "needs_attention"}})
		if len(ops) != 0 {
			t.Fatalf("reconciliation did not release blocks: %+v", ops)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
