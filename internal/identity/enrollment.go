package identity

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func PublicID(key any) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return "node-" + hex.EncodeToString(sum[:]), nil
}
func AtomicFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".identity-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func localKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			return nil, e
		}
		der, e := x509.MarshalPKCS8PrivateKey(key)
		if e != nil {
			return nil, e
		}
		if e = AtomicFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); e != nil {
			return nil, e
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("invalid identity key")
	}
	var parsed any
	if block.Type == "EC PRIVATE KEY" {
		parsed, err = x509.ParseECPrivateKey(block.Bytes)
	} else {
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	}
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("identity requires ECDSA P-256")
	}
	return key, nil
}
func IdentityLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	return func() { syscall.Flock(fd, syscall.LOCK_UN); syscall.Close(fd) }, nil
}
func plainFiles(paths ...string) error {
	for _, p := range paths {
		st, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("identity path must be a regular file: %s", p)
		}
	}
	return nil
}

type Issuer struct {
	Certificate *x509.Certificate
	Key         *ecdsa.PrivateKey
	PEM         []byte
}

// A dedicated online client-only CA is independent of the offline server CA.
func OpenIssuer(directory string) (*Issuer, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	unlock, err := IdentityLock(filepath.Join(directory, "issuer.lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	keyFile, certFile := filepath.Join(directory, "key.pem"), filepath.Join(directory, "ca.pem")
	if err = plainFiles(keyFile, certFile); err != nil {
		return nil, err
	}
	if _, e := os.Stat(certFile); e == nil {
		if _, e = os.Stat(keyFile); e != nil {
			return nil, fmt.Errorf("issuer key missing; refusing identity reset")
		}
	}
	key, err := localKey(keyFile)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(certFile)
	if os.IsNotExist(err) {
		serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if e != nil {
			return nil, e
		}
		c := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Deployer Online Agent CA"}, NotBefore: time.Now().Add(-5 * time.Minute), NotAfter: time.Now().AddDate(20, 0, 0), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
		der, e := x509.CreateCertificate(rand.Reader, c, c, &key.PublicKey, key)
		if e != nil {
			return nil, e
		}
		raw = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		if e = AtomicFile(certFile, raw); e != nil {
			return nil, e
		}
	} else if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("invalid issuer certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	public, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !public.Equal(&key.PublicKey) || !cert.IsCA || time.Now().After(cert.NotAfter) {
		return nil, fmt.Errorf("invalid or expired issuer")
	}
	return &Issuer{cert, key, raw}, nil
}
func (i *Issuer) Sign(csrPEM []byte) ([]byte, error) {
	block, rest := pem.Decode(csrPEM)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("invalid CSR")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err = csr.CheckSignature(); err != nil {
		return nil, err
	}
	public, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || public.Curve != elliptic.P256() {
		return nil, fmt.Errorf("CSR requires ECDSA P-256")
	}
	id, err := PublicID(public)
	if err != nil {
		return nil, err
	}
	uri, _ := url.Parse("spiffe://deployer/nodes/" + id)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	expires := time.Now().AddDate(10, 0, 0)
	if expires.After(i.Certificate.NotAfter) {
		expires = i.Certificate.NotAfter
	}
	if time.Until(expires) < 24*time.Hour {
		return nil, fmt.Errorf("issuer expires soon")
	}
	c := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Deployer Agent"}, URIs: []*url.URL{uri}, NotBefore: time.Now().Add(-5 * time.Minute), NotAfter: expires, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, c, i.Certificate, public, i.Key)
	if err != nil {
		return nil, err
	}
	return append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), i.PEM...), nil
}

// Enrollment proves possession of the key through a signed CSR, verifies the
// server with the packaged CA, and persists the private key BEFORE any request.
func EnsureIdentity(ctx context.Context, server, caFile, certFile, keyFile string) error {
	if certFile == "" || keyFile == "" {
		return fmt.Errorf("client certificate/key paths are required")
	}
	unlock, err := IdentityLock(keyFile + ".lock")
	if err != nil {
		return err
	}
	defer unlock()
	if err = plainFiles(keyFile, certFile); err != nil {
		return err
	}
	if _, e := os.Stat(certFile); e == nil {
		if _, e = os.Stat(keyFile); e != nil {
			return fmt.Errorf("client key missing; refusing identity reset")
		}
	}
	key, err := localKey(keyFile)
	if err != nil {
		return err
	}
	if pair, e := tls.LoadX509KeyPair(certFile, keyFile); e == nil {
		leaf, e := x509.ParseCertificate(pair.Certificate[0])
		if e != nil {
			return e
		}
		if _, e = NodeID(leaf); e != nil {
			return e
		}
		if time.Now().After(leaf.NotBefore) && time.Until(leaf.NotAfter) > 30*24*time.Hour {
			return nil
		}
		id, _ := PublicID(&key.PublicKey)
		current, _ := NodeID(leaf)
		if current != id {
			return fmt.Errorf("external identity requires renewal by its original issuer")
		}
	}
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("enrollment requires an HTTPS root URL")
	}
	roots, err := Trust(caFile)
	if err != nil {
		return err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "Deployer Agent"}}, key)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"csr": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))})
	req, err := http.NewRequestWithContext(ctx, "POST", u.Scheme+"://"+u.Host+"/enroll", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("certificate enrollment HTTP %d", response.StatusCode)
	}
	var result struct {
		Certificate string `json:"certificate"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 32<<10)).Decode(&result); err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	pair, err := tls.X509KeyPair([]byte(result.Certificate), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	id, err := NodeID(leaf)
	if err != nil {
		return err
	}
	expected, _ := PublicID(&key.PublicKey)
	if id != expected {
		return fmt.Errorf("enrollment changed identity")
	}
	if len(pair.Certificate) != 2 {
		return fmt.Errorf("enrollment requires a client issuer chain")
	}
	issuer, err := x509.ParseCertificate(pair.Certificate[1])
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	pool.AddCert(issuer)
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(certFile), 0700); err != nil {
		return err
	}
	return AtomicFile(certFile, []byte(result.Certificate))
}
