package identity

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"deployer/internal/model"
)

// A URI SAN is stable across certificate/key renewal. The issuing CA owns the
// identity namespace; neither request headers nor a certificate's serial do.
func NodeID(cert *x509.Certificate) (string, error) {
	if cert == nil {
		return "", fmt.Errorf("缺少客户端证书")
	}
	var id string
	for _, u := range cert.URIs {
		if u.Scheme != "spiffe" || u.Host != "deployer" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || !strings.HasPrefix(u.Path, "/nodes/") {
			continue
		}
		candidate := strings.TrimPrefix(u.Path, "/nodes/")
		if !model.ValidName(candidate) || id != "" {
			return "", fmt.Errorf("客户端证书的主机身份无效或重复")
		}
		id = candidate
	}
	if id == "" {
		return "", fmt.Errorf("客户端证书需要 URI SAN spiffe://deployer/nodes/<主机ID>")
	}
	return id, nil
}

func Trust(path string) (*x509.CertPool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("CA 文件无有效证书")
	}
	return pool, nil
}

// Reload only a complete valid key pair. Enforce identity continuity on Agents.
func ReloadPair(certFile, keyFile, expectedID string) (func() (*tls.Certificate, error), error) {
	load := func() (*tls.Certificate, error) {
		pair, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, err
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return nil, err
		}
		pair.Leaf = leaf
		if expectedID != "" {
			id, err := NodeID(leaf)
			if err != nil {
				return nil, err
			}
			if id != expectedID {
				return nil, fmt.Errorf("证书续期不能改变此数据目录的主机身份")
			}
		}
		if now := time.Now(); now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
			return nil, fmt.Errorf("证书尚未生效或已过期")
		}
		return &pair, nil
	}
	current, err := load()
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	ci, _ := os.Stat(certFile)
	ki, _ := os.Stat(keyFile)
	return func() (*tls.Certificate, error) {
		mu.Lock()
		defer mu.Unlock()
		nc, ec := os.Stat(certFile)
		nk, ek := os.Stat(keyFile)
		if ec == nil && ek == nil && (ci == nil || ki == nil || !os.SameFile(ci, nc) || !os.SameFile(ki, nk) || !ci.ModTime().Equal(nc.ModTime()) || !ki.ModTime().Equal(nk.ModTime()) || ci.Size() != nc.Size() || ki.Size() != nk.Size()) {
			if next, err := load(); err == nil {
				current = next
				ci = nc
				ki = nk
			}
		}
		return current, nil
	}, nil
}
