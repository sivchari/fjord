package pki

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"
)

const (
	rsaKeyBits = 2048

	// caValidity is how long a fjord-generated CA is valid for.
	caValidity = 10 * 365 * 24 * time.Hour

	// clockSkew backdates NotBefore so certificates are already valid on
	// hosts whose clock is slightly behind the one that issued them.
	clockSkew = 5 * time.Minute

	// serialNumberBits bounds the random serial number generated for
	// each certificate.
	serialNumberBits = 128
)

// CA is a self-signed certificate authority used to issue TLS server
// certificates for fjord's admission webhooks.
type CA struct {
	Cert    *x509.Certificate
	Key     *rsa.PrivateKey
	CertPEM []byte
	KeyPEM  []byte
}

// NewCA generates a self-signed RSA certificate authority with the given
// common name. The returned CA is valid for 10 years and can sign server
// certificates via IssueServerCert.
func NewCA(commonName string) (*CA, error) {
	key, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		return nil, fmt.Errorf("generating CA private key: %w", err)
	}

	serialNumber, err := newSerialNumber()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serialNumber,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("creating self-signed CA certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parsing generated CA certificate: %w", err)
	}

	return &CA{
		Cert:    cert,
		Key:     key,
		CertPEM: encodeCertPEM(certDER),
		KeyPEM:  encodeKeyPEM(key),
	}, nil
}

// newSerialNumber generates a random certificate serial number.
func newSerialNumber() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), serialNumberBits)

	serialNumber, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generating certificate serial number: %w", err)
	}

	return serialNumber, nil
}

// encodeCertPEM encodes a DER certificate as PEM.
func encodeCertPEM(certDER []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
}

// encodeKeyPEM encodes an RSA private key as PEM.
func encodeKeyPEM(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
