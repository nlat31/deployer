// Package testpki creates isolated, current-format certificates for tests only.
package testpki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type Files struct{ CA, Cert, Key string }
type PKI struct {
	CA, ServerCert, ServerKey string
	certificate               *x509.Certificate
	key                       *ecdsa.PrivateKey
}

func serial(t testing.TB) *big.Int {
	t.Helper()
	n, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if e != nil {
		t.Fatal(e)
	}
	return n
}
func private(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	return k
}
func write(t testing.TB, path, kind string, b []byte) {
	t.Helper()
	if e := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: b}), 0600); e != nil {
		t.Fatal(e)
	}
}
func New(t testing.TB) *PKI {
	t.Helper()
	dir := t.TempDir()
	key := private(t)
	ca := &x509.Certificate{SerialNumber: serial(t), Subject: pkix.Name{CommonName: "isolated test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, e := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	parsed, e := x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	p := &PKI{CA: filepath.Join(dir, "ca.pem"), certificate: parsed, key: key}
	write(t, p.CA, "CERTIFICATE", der)
	server := &x509.Certificate{SerialNumber: serial(t), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	f := p.issue(t, server)
	p.ServerCert = f.Cert
	p.ServerKey = f.Key
	return p
}
func (p *PKI) issue(t testing.TB, cert *x509.Certificate) Files {
	t.Helper()
	key := private(t)
	dir := t.TempDir()
	der, e := x509.CreateCertificate(rand.Reader, cert, p.certificate, &key.PublicKey, p.key)
	if e != nil {
		t.Fatal(e)
	}
	b, e := x509.MarshalECPrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	f := Files{CA: p.CA, Cert: filepath.Join(dir, "cert.pem"), Key: filepath.Join(dir, "key.pem")}
	write(t, f.Cert, "CERTIFICATE", der)
	write(t, f.Key, "EC PRIVATE KEY", b)
	return f
}
func (p *PKI) Node(t testing.TB, id string) Files {
	return p.Client(t, "spiffe://deployer/nodes/"+id, time.Now().Add(time.Hour))
}
func (p *PKI) Client(t testing.TB, uri string, expires time.Time) Files {
	t.Helper()
	u, e := url.Parse(uri)
	if e != nil {
		t.Fatal(e)
	}
	return p.issue(t, &x509.Certificate{SerialNumber: serial(t), Subject: pkix.Name{CommonName: "client"}, URIs: []*url.URL{u}, NotBefore: time.Now().Add(-2 * time.Hour), NotAfter: expires, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
}
func (p *PKI) ServerTLS(t testing.TB) *tls.Config {
	t.Helper()
	pair, e := tls.LoadX509KeyPair(p.ServerCert, p.ServerKey)
	if e != nil {
		t.Fatal(e)
	}
	roots := x509.NewCertPool()
	roots.AddCert(p.certificate)
	return &tls.Config{Certificates: []tls.Certificate{pair}, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: roots, MinVersion: tls.VersionTLS12}
}
func (p *PKI) BrowserTLS() *tls.Config {
	roots := x509.NewCertPool()
	roots.AddCert(p.certificate)
	return &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
}
