package controller_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"deployer/internal/controller"
	"deployer/internal/identity"
	"deployer/internal/model"
	"deployer/internal/store"
	"deployer/internal/testpki"
)

func TestUniversalEnrollment(t *testing.T) {
	root := t.TempDir()
	pki := testpki.New(t)
	s, err := controller.New(root, false, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.ConfigureEnrollment(pki.CA); err != nil {
		t.Fatal(err)
	}
	issuerBefore, err := os.ReadFile(filepath.Join(root, "state/agent-issuer/key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	// Production TLS settings, including verification of client certificates.
	ts := httptest.NewUnstartedServer(s)
	ts.TLS, err = controller.MTLSConfig(pki.ServerCert, pki.ServerKey, s.ClientCA)
	if err != nil {
		t.Fatal(err)
	}
	ts.StartTLS()
	defer ts.Close()
	if err = s.ConfigureEnrollment(pki.CA); err != nil {
		t.Fatal(err)
	}
	issuerAfter, _ := os.ReadFile(filepath.Join(root, "state/agent-issuer/key.pem"))
	if !bytes.Equal(issuerBefore, issuerAfter) {
		t.Fatal("issuer identity changed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ids := map[string]bool{}
	for n := 0; n < 2; n++ {
		dir := t.TempDir()
		cert, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
		if err = identity.EnsureIdentity(ctx, ts.URL, pki.CA, cert, key); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			// Force the renewal window while retaining the same public/private key.
			pair, e := tls.LoadX509KeyPair(cert, key)
			if e != nil {
				t.Fatal(e)
			}
			leaf, e := x509.ParseCertificate(pair.Certificate[0])
			if e != nil {
				t.Fatal(e)
			}
			issuer, e := identity.OpenIssuer(filepath.Join(root, "state/agent-issuer"))
			if e != nil {
				t.Fatal(e)
			}
			leaf.NotAfter = time.Now().Add(24 * time.Hour)
			der, e := x509.CreateCertificate(rand.Reader, leaf, issuer.Certificate, leaf.PublicKey, issuer.Key)
			if e != nil {
				t.Fatal(e)
			}
			short := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), issuer.PEM...)
			if e = identity.AtomicFile(cert, short); e != nil {
				t.Fatal(e)
			}
			oldID, _ := identity.NodeID(leaf)
			if e = identity.EnsureIdentity(ctx, ts.URL, pki.CA, cert, key); e != nil {
				t.Fatal(e)
			}
			renewed, e := tls.LoadX509KeyPair(cert, key)
			if e != nil {
				t.Fatal(e)
			}
			leaf, e = x509.ParseCertificate(renewed.Certificate[0])
			if e != nil {
				t.Fatal(e)
			}
			id, _ := identity.NodeID(leaf)
			if id != oldID || time.Until(leaf.NotAfter) < 30*24*time.Hour {
				t.Fatal("renewal changed identity or did not extend lifetime")
			}
		}
		originalKey, _ := os.ReadFile(key)
		originalCert, _ := os.ReadFile(cert)
		if err = identity.EnsureIdentity(ctx, ts.URL, pki.CA, cert, key); err != nil {
			t.Fatal(err)
		}
		repeated, _ := os.ReadFile(cert)
		if !bytes.Equal(originalCert, repeated) {
			t.Fatal("unnecessary enrollment")
		}
		if err = os.Remove(cert); err != nil {
			t.Fatal(err)
		} // Interrupted install reuses persisted key.
		if err = identity.EnsureIdentity(ctx, ts.URL, pki.CA, cert, key); err != nil {
			t.Fatal(err)
		}
		repeated, _ = os.ReadFile(key)
		if !bytes.Equal(originalKey, repeated) {
			t.Fatal("key changed on retry")
		}
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			t.Fatal(err)
		}
		leaf, _ := x509.ParseCertificate(pair.Certificate[0])
		id, err := identity.NodeID(leaf)
		if err != nil {
			t.Fatal(err)
		}
		if ids[id] {
			t.Fatal("two hosts share identity")
		}
		ids[id] = true
		// CSR alone must not create a host; only authenticated heartbeat does.
		err = s.DB.View(func(tx *store.Tx) error {
			nodes, e := store.List[model.Node](tx, "nodes")
			if e == nil && len(nodes) != n {
				t.Fatalf("premature registration: %d", len(nodes))
			}
			return e
		})
		if err != nil {
			t.Fatal(err)
		}
		config := pki.BrowserTLS()
		config.Certificates = []tls.Certificate{pair}
		transport := &http.Transport{TLSClientConfig: config}
		client := &http.Client{Transport: transport}
		defer transport.CloseIdleConnections()
		body, _ := json.Marshal(model.Poll{Protocol: model.Protocol, OS: "linux", Arch: "amd64", Name: "universal-test"})
		response, e := client.Post(ts.URL+"/agent/poll", "application/json", bytes.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		b, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("mTLS heartbeat: %d %s", response.StatusCode, b)
		}
		st, _ := os.Stat(key)
		if st.Mode().Perm() != 0600 {
			t.Fatal("private key permissions")
		}
	}
	browser := &http.Client{Transport: &http.Transport{TLSClientConfig: pki.BrowserTLS()}}
	response, err := browser.Post(ts.URL+"/agent/poll", "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("unauthenticated heartbeat admitted")
	}
	response, err = browser.Post(ts.URL+"/enroll", "application/json", bytes.NewReader([]byte(`{"csr":"invalid"}`)))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal("invalid CSR admitted")
	}
	wrong := testpki.New(t)
	dir := t.TempDir()
	key := filepath.Join(dir, "key.pem")
	cert := filepath.Join(dir, "cert.pem")
	if identity.EnsureIdentity(ctx, ts.URL, wrong.CA, cert, key) == nil {
		t.Fatal("untrusted server admitted")
	}
	if _, err = os.Stat(cert); !os.IsNotExist(err) {
		t.Fatal("certificate saved from untrusted server")
	}
	// A failed first request has already persisted a key; retry keeps it.
	before, _ := os.ReadFile(key)
	if err = identity.EnsureIdentity(ctx, ts.URL, pki.CA, cert, key); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(key)
	if !bytes.Equal(before, after) {
		t.Fatal("failed enrollment reset identity")
	}
}
