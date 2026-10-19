package controller

import (
	"deployer/internal/identity"
	"crypto/tls"
	"log"
	"os"
	"sync"
)

// Renewed certificate/key files are picked up by new TLS handshakes. An
// incomplete file replacement never discards the last valid certificate.
func ReloadingTLS(certFile, keyFile string) (*tls.Config, error) {
	current, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	var certInfo, keyInfo os.FileInfo
	certInfo, _ = os.Stat(certFile)
	keyInfo, _ = os.Stat(keyFile)
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		mu.Lock()
		defer mu.Unlock()
		ci, ce := os.Stat(certFile)
		ki, ke := os.Stat(keyFile)
		if ce == nil && ke == nil && (certInfo == nil || keyInfo == nil || !ci.ModTime().Equal(certInfo.ModTime()) || !ki.ModTime().Equal(keyInfo.ModTime()) || ci.Size() != certInfo.Size() || ki.Size() != keyInfo.Size()) {
			next, e := tls.LoadX509KeyPair(certFile, keyFile)
			if e == nil {
				current = next
				certInfo = ci
				keyInfo = ki
			} else {
				log.Printf("TLS 证书更新尚未完成，继续使用已加载的证书: %v", e)
			}
		}
		return &current, nil
	}}, nil
}

// Browser clients may authenticate with their normal session. Agent endpoints
// separately require VerifiedChains, so presenting no certificate never admits
// an Agent. A presented untrusted certificate fails the TLS handshake itself.
func MTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	config, err := ReloadingTLS(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	roots, err := identity.Trust(caFile)
	if err != nil {
		return nil, err
	}
	config.ClientAuth = tls.VerifyClientCertIfGiven
	config.ClientCAs = roots
	var mu sync.Mutex
	config.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		mu.Lock()
		defer mu.Unlock()
		if next, err := identity.Trust(caFile); err == nil {
			roots = next
		}
		fresh := config.Clone()
		fresh.GetConfigForClient = nil
		fresh.ClientCAs = roots
		return fresh, nil
	}
	return config, nil
}
