package pki_test

import (
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/sivchari/fjord/internal/pki"
)

func TestNewCA_SelfSignedAndIsCA(t *testing.T) {
	t.Parallel()

	ca, err := pki.NewCA("fjord-ca")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	if !ca.Cert.IsCA {
		t.Error("Cert.IsCA = false, want true")
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)

	if _, err := ca.Cert.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Errorf("self-signed CA failed verification: %v", err)
	}
}

func TestNewCA_PEMRoundTrips(t *testing.T) {
	t.Parallel()

	ca, err := pki.NewCA("fjord-ca")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	certBlock, _ := pem.Decode(ca.CertPEM)
	if certBlock == nil {
		t.Fatal("pem.Decode(CertPEM) returned nil block")
	}

	if _, err := x509.ParseCertificate(certBlock.Bytes); err != nil {
		t.Errorf("x509.ParseCertificate(CertPEM): %v", err)
	}

	keyBlock, _ := pem.Decode(ca.KeyPEM)
	if keyBlock == nil {
		t.Fatal("pem.Decode(KeyPEM) returned nil block")
	}

	if _, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes); err != nil {
		t.Errorf("x509.ParsePKCS1PrivateKey(KeyPEM): %v", err)
	}
}

func TestIssueServerCert_VerifiesForConfiguredDNSNames(t *testing.T) {
	t.Parallel()

	ca, err := pki.NewCA("fjord-ca")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	dnsNames := []string{
		"webhook.fjord-system.svc",
		"webhook.fjord-system.svc.cluster.local",
	}

	serverCert, err := ca.IssueServerCert("webhook.fjord-system.svc", dnsNames)
	if err != nil {
		t.Fatalf("IssueServerCert: %v", err)
	}

	certBlock, _ := pem.Decode(serverCert.CertPEM)
	if certBlock == nil {
		t.Fatal("pem.Decode(CertPEM) returned nil block")
	}

	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("x509.ParseCertificate(CertPEM): %v", err)
	}

	keyBlock, _ := pem.Decode(serverCert.KeyPEM)
	if keyBlock == nil {
		t.Fatal("pem.Decode(KeyPEM) returned nil block")
	}

	if _, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes); err != nil {
		t.Errorf("x509.ParsePKCS1PrivateKey(KeyPEM): %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)

	for _, name := range dnsNames {
		if _, err := cert.Verify(x509.VerifyOptions{DNSName: name, Roots: pool}); err != nil {
			t.Errorf("Verify(DNSName=%s): %v", name, err)
		}
	}

	if _, err := cert.Verify(x509.VerifyOptions{DNSName: "unrelated.example.com", Roots: pool}); err == nil {
		t.Error("Verify(DNSName=unrelated.example.com) succeeded, want error")
	}
}

func TestIssueServerCert_FailsVerificationWithoutCARoot(t *testing.T) {
	t.Parallel()

	ca, err := pki.NewCA("fjord-ca")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	serverCert, err := ca.IssueServerCert("webhook.fjord-system.svc", []string{"webhook.fjord-system.svc"})
	if err != nil {
		t.Fatalf("IssueServerCert: %v", err)
	}

	certBlock, _ := pem.Decode(serverCert.CertPEM)

	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("x509.ParseCertificate(CertPEM): %v", err)
	}

	otherCA, err := pki.NewCA("other-ca")
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(otherCA.Cert)

	if _, err := cert.Verify(x509.VerifyOptions{DNSName: "webhook.fjord-system.svc", Roots: pool}); err == nil {
		t.Error("Verify against unrelated CA succeeded, want error")
	}
}
