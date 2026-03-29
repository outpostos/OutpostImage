package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CertManager generates and caches TLS certificates for MITM proxy.
type CertManager struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey

	mu    sync.RWMutex
	cache map[string]*tls.Certificate
}

// NewCertManager creates a CertManager. If caDir is non-empty and contains
// ca.crt/ca.key, those are loaded; otherwise a new CA is generated and
// optionally persisted to caDir.
func NewCertManager(caDir string) (*CertManager, error) {
	cm := &CertManager{
		cache: make(map[string]*tls.Certificate),
	}

	if caDir != "" {
		if err := cm.loadCA(caDir); err == nil {
			slog.Info("loaded existing CA", "dir", caDir)
			return cm, nil
		}
	}

	// Generate new CA
	if err := cm.generateCA(); err != nil {
		return nil, fmt.Errorf("generate CA: %w", err)
	}

	if caDir != "" {
		if err := cm.saveCA(caDir); err != nil {
			slog.Warn("failed to save CA", "dir", caDir, "error", err)
		} else {
			slog.Info("saved new CA", "dir", caDir)
		}
	}

	return cm, nil
}

// GetCertificate returns a TLS certificate for the given hostname.
// Certificates are cached for reuse.
func (cm *CertManager) GetCertificate(host string) (*tls.Certificate, error) {
	cm.mu.RLock()
	cert, ok := cm.cache[host]
	cm.mu.RUnlock()
	if ok {
		return cert, nil
	}

	cert, err := cm.generateCert(host)
	if err != nil {
		return nil, err
	}

	cm.mu.Lock()
	cm.cache[host] = cert
	cm.mu.Unlock()

	slog.Debug("generated certificate", "host", host)
	return cert, nil
}

// CACertPEM returns the CA certificate in PEM format.
func (cm *CertManager) CACertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cm.caCert.Raw,
	})
}

func (cm *CertManager) generateCA() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"OutpostImage"},
			CommonName:   "OutpostImage CA",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return err
	}

	cm.caCert = cert
	cm.caKey = key
	return nil
}

func (cm *CertManager) generateCert(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: host,
		},
		DNSNames:  []string{host},
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, cm.caCert, &key.PublicKey, cm.caKey)
	if err != nil {
		return nil, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}

	return &tlsCert, nil
}

func (cm *CertManager) loadCA(dir string) error {
	certPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		return err
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		return fmt.Errorf("invalid CA certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}

	block, _ = pem.Decode(keyPEM)
	if block == nil {
		return fmt.Errorf("invalid CA key PEM")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return err
	}

	cm.caCert = cert
	cm.caKey = key
	return nil
}

// generateSelfSignedCert creates a self-signed TLS certificate for MITM of
// insecure registries. Clients with insecure-registry configured already skip
// TLS verification, so no trusted CA is required.
func generateSelfSignedCert(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &tlsCert, nil
}

func (cm *CertManager) saveCA(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cm.caCert.Raw,
	})
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), certPEM, 0644); err != nil {
		return err
	}

	keyDER, err := x509.MarshalECPrivateKey(cm.caKey)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: keyDER,
	})
	return os.WriteFile(filepath.Join(dir, "ca.key"), keyPEM, 0600)
}
