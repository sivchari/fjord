package authn_test

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/sivchari/fjord/internal/authn"
	"github.com/sivchari/fjord/internal/pki"
)

// nodeAuthnDirWant is the node-side directory Stage's dir mount and
// AuthWebhook both point at.
const nodeAuthnDirWant = "/etc/fjord/authn"

// certPoolFromPEM builds an x509.CertPool trusting the CA certificate
// carried in pemBytes.
func certPoolFromPEM(t *testing.T, pemBytes []byte) *x509.CertPool {
	t.Helper()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatalf("append CA cert to pool")
	}

	return pool
}

// parseCertPEM parses a single PEM-encoded certificate.
func parseCertPEM(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()

	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatalf("decode PEM block")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}

	return cert
}

// x509VerifyOptions returns VerifyOptions trusting pool for DNS name
// "localhost", matching the authenticator's certificate.
func x509VerifyOptions(pool *x509.CertPool) x509.VerifyOptions {
	return x509.VerifyOptions{Roots: pool, DNSName: "localhost"}
}

func TestStage(t *testing.T) {
	t.Parallel()

	ca, err := pki.NewCA("fjord")
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}

	baseDir := t.TempDir()

	staged, err := authn.Stage(baseDir, ca)
	if err != nil {
		t.Fatalf("Stage() error = %v", err)
	}

	assertStagedDir(t, baseDir, staged)
	assertCert(t, staged, ca)
	assertWebhookKubeconfig(t, staged.Dir, ca)
	assertMounts(t, staged)
	assertWebhook(t, staged)
}

func assertStagedDir(t *testing.T, baseDir string, staged *authn.StagedAuthn) {
	t.Helper()

	wantDir := filepath.Join(baseDir, "authn")
	if staged.Dir != wantDir {
		t.Errorf("Dir = %q, want %q", staged.Dir, wantDir)
	}

	info, err := os.Stat(staged.Dir)
	if err != nil {
		t.Fatalf("stat staged dir: %v", err)
	}

	if !info.IsDir() {
		t.Errorf("staged.Dir = %q, want a directory", staged.Dir)
	}
}

// assertCert verifies staged.Cert carries a non-empty certificate and key
// issued by ca for "localhost", and is not staged to disk under staged.Dir
// (cluster.EnsureAuthenticator delivers it via a Secret instead).
func assertCert(t *testing.T, staged *authn.StagedAuthn, ca *pki.CA) {
	t.Helper()

	if staged.Cert == nil {
		t.Fatalf("Cert is nil")
	}

	if len(staged.Cert.CertPEM) == 0 || len(staged.Cert.KeyPEM) == 0 {
		t.Fatalf("Cert.CertPEM/Cert.KeyPEM must not be empty")
	}

	pool := certPoolFromPEM(t, ca.CertPEM)

	cert := parseCertPEM(t, staged.Cert.CertPEM)

	if _, err := cert.Verify(x509VerifyOptions(pool)); err != nil {
		t.Errorf("Cert.CertPEM does not verify against the CA: %v", err)
	}

	if len(cert.DNSNames) == 0 || cert.DNSNames[0] != "localhost" {
		t.Errorf("Cert.CertPEM DNSNames = %v, want [\"localhost\"]", cert.DNSNames)
	}

	if _, err := os.Stat(filepath.Join(staged.Dir, "tls.crt")); !os.IsNotExist(err) {
		t.Errorf("tls.crt exists under staged.Dir, want it delivered only via a Secret")
	}
}

// assertWebhookKubeconfig loads dir/webhook.yaml through clientcmd (the
// same loader kube-apiserver's webhook client uses), verifying it names one
// cluster pointing at the authenticator and trusting ca.
func assertWebhookKubeconfig(t *testing.T, dir string, ca *pki.CA) {
	t.Helper()

	config, err := clientcmd.LoadFromFile(filepath.Join(dir, "webhook.yaml"))
	if err != nil {
		t.Fatalf("load webhook.yaml: %v", err)
	}

	if len(config.Clusters) != 1 {
		t.Fatalf("len(Clusters) = %d, want 1", len(config.Clusters))
	}

	if config.CurrentContext == "" {
		t.Fatalf("CurrentContext is empty")
	}

	webhookCluster, ok := config.Clusters[config.Contexts[config.CurrentContext].Cluster]
	if !ok {
		t.Fatalf("current context %q names no cluster", config.CurrentContext)
	}

	if got, want := webhookCluster.Server, "https://localhost:9443/"; got != want {
		t.Errorf("server = %q, want %q", got, want)
	}

	if !bytes.Equal(webhookCluster.CertificateAuthorityData, ca.CertPEM) {
		t.Errorf("certificate-authority-data does not match the CA's cert PEM")
	}
}

func assertMounts(t *testing.T, staged *authn.StagedAuthn) {
	t.Helper()

	if len(staged.Mounts) != 1 {
		t.Fatalf("len(Mounts) = %d, want 1", len(staged.Mounts))
	}

	dirMount := staged.Mounts[0]
	if dirMount.HostPath != staged.Dir {
		t.Errorf("Mounts[0].HostPath = %q, want %q", dirMount.HostPath, staged.Dir)
	}

	if dirMount.ContainerPath != nodeAuthnDirWant {
		t.Errorf("Mounts[0].ContainerPath = %q, want %q", dirMount.ContainerPath, nodeAuthnDirWant)
	}
}

func assertWebhook(t *testing.T, staged *authn.StagedAuthn) {
	t.Helper()

	if staged.Webhook == nil {
		t.Fatalf("Webhook is nil")
	}

	if got, want := staged.Webhook.ConfigFilePath, nodeAuthnDirWant+"/webhook.yaml"; got != want {
		t.Errorf("Webhook.ConfigFilePath = %q, want %q", got, want)
	}

	if got, want := staged.Webhook.VolumeMountPath, nodeAuthnDirWant; got != want {
		t.Errorf("Webhook.VolumeMountPath = %q, want %q", got, want)
	}
}

func TestStage_Cleanup(t *testing.T) {
	t.Parallel()

	ca, err := pki.NewCA("fjord")
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}

	baseDir := t.TempDir()
	authDir := filepath.Join(baseDir, "cluster")

	staged, err := authn.Stage(authDir, ca)
	if err != nil {
		t.Fatalf("Stage() error = %v", err)
	}

	if err := staged.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}

	if _, err := os.Stat(authDir); !os.IsNotExist(err) {
		t.Errorf("authDir = %q still exists after Cleanup()", authDir)
	}
}
