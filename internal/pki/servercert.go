package pki

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"time"
)

// serverCertValidity is how long a fjord-issued server certificate is
// valid for.
const serverCertValidity = 365 * 24 * time.Hour

// ServerCert is a TLS server certificate signed by a CA, ready to be
// stored in a Secret and used by an admission webhook server.
type ServerCert struct {
	CertPEM []byte
	KeyPEM  []byte
}

// IssueServerCert generates an RSA key pair and issues a TLS server
// certificate signed by ca, valid for the given DNS names for 1 year.
func (ca *CA) IssueServerCert(commonName string, dnsNames []string) (*ServerCert, error) {
	key, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		return nil, fmt.Errorf("generating server private key: %w", err)
	}

	serialNumber, err := newSerialNumber()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject:      pkix.Name{CommonName: commonName},
		DNSNames:     dnsNames,
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     now.Add(serverCertValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return nil, fmt.Errorf("creating server certificate: %w", err)
	}

	return &ServerCert{
		CertPEM: encodeCertPEM(certDER),
		KeyPEM:  encodeKeyPEM(key),
	}, nil
}
