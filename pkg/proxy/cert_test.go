package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestCertManager_Generate(t *testing.T) {
	cm, err := NewCertManager("")
	if err != nil {
		t.Fatal(err)
	}

	// Verify CA cert is valid
	caPEM := cm.CACertPEM()
	if len(caPEM) == 0 {
		t.Fatal("CA cert PEM is empty")
	}
}

func TestCertManager_GetCertificate(t *testing.T) {
	cm, err := NewCertManager("")
	if err != nil {
		t.Fatal(err)
	}

	cert, err := cm.GetCertificate("registry.example.com")
	if err != nil {
		t.Fatal(err)
	}

	// Parse the leaf cert to verify
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	if leaf.Subject.CommonName != "registry.example.com" {
		t.Fatalf("CN = %q, want %q", leaf.Subject.CommonName, "registry.example.com")
	}

	found := false
	for _, name := range leaf.DNSNames {
		if name == "registry.example.com" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected registry.example.com in SAN")
	}

	// Verify cert is signed by our CA
	roots := x509.NewCertPool()
	roots.AddCert(cm.caCert)
	_, err = leaf.Verify(x509.VerifyOptions{Roots: roots})
	if err != nil {
		t.Fatalf("cert verification failed: %v", err)
	}
}

func TestCertManager_CacheHit(t *testing.T) {
	cm, err := NewCertManager("")
	if err != nil {
		t.Fatal(err)
	}

	cert1, _ := cm.GetCertificate("docker.io")
	cert2, _ := cm.GetCertificate("docker.io")

	// Should be the exact same pointer (cached)
	if cert1 != cert2 {
		t.Fatal("expected cached certificate to be returned")
	}
}

func TestCertManager_DifferentHosts(t *testing.T) {
	cm, err := NewCertManager("")
	if err != nil {
		t.Fatal(err)
	}

	cert1, _ := cm.GetCertificate("docker.io")
	cert2, _ := cm.GetCertificate("ghcr.io")

	if cert1 == cert2 {
		t.Fatal("different hosts should get different certificates")
	}
}

func TestCertManager_Persistence(t *testing.T) {
	dir := t.TempDir()

	// Create and save CA
	cm1, err := NewCertManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	pem1 := cm1.CACertPEM()

	// Load from disk
	cm2, err := NewCertManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	pem2 := cm2.CACertPEM()

	if string(pem1) != string(pem2) {
		t.Fatal("loaded CA should match saved CA")
	}
}

func TestCertManager_TLSConfig(t *testing.T) {
	cm, err := NewCertManager("")
	if err != nil {
		t.Fatal(err)
	}

	cert, err := cm.GetCertificate("example.com")
	if err != nil {
		t.Fatal(err)
	}

	// Verify it works as a tls.Certificate
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{*cert},
	}
	if len(tlsConfig.Certificates) != 1 {
		t.Fatal("expected 1 certificate in TLS config")
	}
}

// TestLoadCA_InvalidCertPEM covers the "block == nil" error path in loadCA when
// the ca.crt file exists but contains no valid PEM block.
func TestLoadCA_InvalidCertPEM(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("not a valid pem"), 0644); err != nil {
		t.Fatal(err)
	}
	// ca.key not needed; error fires on cert PEM decode
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), []byte("not a key either"), 0644); err != nil {
		t.Fatal(err)
	}

	cm := &CertManager{cache: make(map[string]*tls.Certificate)}
	if err := cm.loadCA(dir); err == nil {
		t.Error("expected error for invalid cert PEM block")
	}
}

// TestLoadCA_InvalidKeyPEM covers the "block == nil" error path in loadCA when
// ca.crt is valid PEM but ca.key contains no valid PEM block.
func TestLoadCA_InvalidKeyPEM(t *testing.T) {
	dir := t.TempDir()
	// Generate a real CA so we have a valid ca.crt on disk
	if _, err := NewCertManager(dir); err != nil {
		t.Fatalf("NewCertManager: %v", err)
	}
	// Overwrite ca.key with non-PEM content to trigger the key block == nil path
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), []byte("not a key pem"), 0644); err != nil {
		t.Fatal(err)
	}

	cm := &CertManager{cache: make(map[string]*tls.Certificate)}
	if err := cm.loadCA(dir); err == nil {
		t.Error("expected error for invalid key PEM block")
	}
}

// TestLoadCA_MissingFiles covers the os.ReadFile error path in loadCA when neither
// ca.crt nor ca.key exists in the given directory.
func TestLoadCA_MissingFiles(t *testing.T) {
	dir := t.TempDir() // empty — no ca.crt or ca.key
	cm := &CertManager{cache: make(map[string]*tls.Certificate)}
	if err := cm.loadCA(dir); err == nil {
		t.Error("expected error for missing ca.crt")
	}
}

// TestSaveCA_CertWriteError exercises the os.WriteFile(ca.crt) error path by
// pre-creating a directory at that path so the write returns EISDIR.
func TestSaveCA_CertWriteError(t *testing.T) {
	dir := t.TempDir()

	// Pre-create "ca.crt" as a subdirectory so WriteFile on it returns an error.
	if err := os.MkdirAll(filepath.Join(dir, "ca.crt"), 0755); err != nil {
		t.Fatal(err)
	}

	cm := &CertManager{cache: make(map[string]*tls.Certificate)}
	if err := cm.generateCA(); err != nil {
		t.Fatalf("generateCA: %v", err)
	}

	if err := cm.saveCA(dir); err == nil {
		t.Error("expected error when ca.crt path is a directory")
	}
}

// TestSaveCA_KeyWriteError exercises the os.WriteFile(ca.key) error path by
// pre-creating a directory at that path so the write returns EISDIR.
func TestSaveCA_KeyWriteError(t *testing.T) {
	dir := t.TempDir()

	// Pre-create "ca.key" as a subdirectory. ca.crt write succeeds (path is writable);
	// ca.key write fails (path is a directory).
	if err := os.MkdirAll(filepath.Join(dir, "ca.key"), 0755); err != nil {
		t.Fatal(err)
	}

	cm := &CertManager{cache: make(map[string]*tls.Certificate)}
	if err := cm.generateCA(); err != nil {
		t.Fatalf("generateCA: %v", err)
	}

	if err := cm.saveCA(dir); err == nil {
		t.Error("expected error when ca.key path is a directory")
	}
}

// TestLoadCA_MissingKeyFile covers the os.ReadFile(ca.key) error path in loadCA
// when ca.crt exists but ca.key is absent.
func TestLoadCA_MissingKeyFile(t *testing.T) {
	dir := t.TempDir()

	// Write only ca.crt (no ca.key).
	cm := &CertManager{cache: make(map[string]*tls.Certificate)}
	if err := cm.generateCA(); err != nil {
		t.Fatalf("generateCA: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), cm.CACertPEM(), 0644); err != nil {
		t.Fatal(err)
	}

	cm2 := &CertManager{cache: make(map[string]*tls.Certificate)}
	if err := cm2.loadCA(dir); err == nil {
		t.Error("expected error when ca.key is missing")
	}
}

// TestLoadCA_InvalidCertDER covers the x509.ParseCertificate error path in loadCA
// when ca.crt contains a valid PEM block but non-DER certificate bytes.
func TestLoadCA_InvalidCertDER(t *testing.T) {
	dir := t.TempDir()

	// Valid PEM structure, but invalid DER content — ParseCertificate will fail.
	invalidCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not valid der")})
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), invalidCertPEM, 0644); err != nil {
		t.Fatal(err)
	}
	// ca.key just needs to exist so ReadFile succeeds; cert parse fires first.
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), []byte("placeholder"), 0644); err != nil {
		t.Fatal(err)
	}

	cm := &CertManager{cache: make(map[string]*tls.Certificate)}
	if err := cm.loadCA(dir); err == nil {
		t.Error("expected error for invalid cert DER bytes")
	}
}

// TestLoadCA_InvalidKeyDER covers the x509.ParseECPrivateKey error path in loadCA
// when ca.crt is a valid certificate but ca.key PEM contains non-EC-key DER bytes.
func TestLoadCA_InvalidKeyDER(t *testing.T) {
	dir := t.TempDir()

	// Write a valid ca.crt.
	cm := &CertManager{cache: make(map[string]*tls.Certificate)}
	if err := cm.generateCA(); err != nil {
		t.Fatalf("generateCA: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), cm.CACertPEM(), 0644); err != nil {
		t.Fatal(err)
	}
	// Valid PEM block, invalid DER content — ParseECPrivateKey will fail.
	invalidKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("not valid ec key der")})
	if err := os.WriteFile(filepath.Join(dir, "ca.key"), invalidKeyPEM, 0644); err != nil {
		t.Fatal(err)
	}

	cm2 := &CertManager{cache: make(map[string]*tls.Certificate)}
	if err := cm2.loadCA(dir); err == nil {
		t.Error("expected error for invalid EC key DER bytes")
	}
}

// TestNewCertManager_WithDir_Warn exercises the slog.Warn("failed to save CA") path
// in NewCertManager when saveCA returns an error (because ca.crt is a directory).
func TestNewCertManager_WithDir_Warn(t *testing.T) {
	dir := t.TempDir()

	// Make ca.crt a directory so saveCA fails on write, triggering the Warn path.
	if err := os.MkdirAll(filepath.Join(dir, "ca.crt"), 0755); err != nil {
		t.Fatal(err)
	}

	// NewCertManager still succeeds (it just logs a warning on save failure).
	cm, err := NewCertManager(dir)
	if err != nil {
		t.Fatalf("NewCertManager should succeed even when save fails: %v", err)
	}
	if cm == nil {
		t.Fatal("expected non-nil CertManager")
	}
}
