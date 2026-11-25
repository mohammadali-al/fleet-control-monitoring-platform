package pki

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"net"
	"strings"
	"testing"
	"time"
)

func requestFor(t *testing.T, key any, tmpl *x509.CertificateRequest) []byte {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func TestSignCSRBindsCertToTheRequestersKey(t *testing.T) {
	ca := mustCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	// The request claims another identity, extra names and a CA flag: none of it is honoured.
	csr := requestFor(t, key, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "somebody-else"}, DNSNames: []string{"evil.example"}, IPAddresses: []net.IP{net.ParseIP("10.9.9.9")},
	})

	certPEM, err := ca.SignCSR(csr, "m1", nil, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		t.Error("the certificate is not for the requester's key")
	}
	if cert.Subject.CommonName != "m1" || len(cert.DNSNames) != 1 || cert.DNSNames[0] != "m1" || len(cert.IPAddresses) != 0 || cert.IsCA {
		t.Errorf("cert CN %q DNS %v IPs %v CA %v: the requested names must be ignored", cert.Subject.CommonName, cert.DNSNames, cert.IPAddresses, cert.IsCA)
	}
	if len(cert.ExtKeyUsage) != 2 {
		t.Errorf("EKUs = %v, want serverAuth and clientAuth like every peer", cert.ExtKeyUsage)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM())
	for _, ku := range []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth} {
		if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{ku}}); err != nil {
			t.Errorf("verify (usage %v): %v", ku, err)
		}
	}
	if cert.NotAfter.After(time.Now().Add(25 * time.Hour)) {
		t.Errorf("NotAfter = %v, want about 24h out", cert.NotAfter)
	}

	// With IPs, they are the IP SANs.
	certPEM, err = ca.SignCSR(csr, "m2", []net.IP{net.ParseIP("10.1.2.3")}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := pem.Decode(certPEM)
	if c2, _ := x509.ParseCertificate(b2.Bytes); len(c2.IPAddresses) != 1 || !c2.IPAddresses[0].Equal(net.ParseIP("10.1.2.3")) {
		t.Errorf("IP SANs = %v", c2.IPAddresses)
	}
}

func TestSignCSRAcceptsOtherStrongKeys(t *testing.T) {
	ca := mustCA(t)
	_, ed, _ := ed25519.GenerateKey(rand.Reader)
	rk, _ := rsa.GenerateKey(rand.Reader, 2048)
	for name, key := range map[string]any{"ed25519": ed, "rsa2048": rk} {
		if _, err := ca.SignCSR(requestFor(t, key, &x509.CertificateRequest{}), "m", nil, time.Hour); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSignCSRRefusals(t *testing.T) {
	ca := mustCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	good := requestFor(t, key, &x509.CertificateRequest{})
	weak, _ := rsa.GenerateKey(rand.Reader, 1024)

	tampered, _ := pem.Decode(good)
	tampered.Bytes[len(tampered.Bytes)-2] ^= 0x55

	tests := []struct {
		name string
		csr  []byte
		id   string
		val  time.Duration
		want string
	}{
		{"not PEM", []byte("hello"), "m", time.Hour, "PEM"},
		{"wrong PEM type", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1}}), "m", time.Hour, "PEM"},
		{"bad signature", pem.EncodeToMemory(tampered), "m", time.Hour, "signature"},
		{"weak RSA", requestFor(t, weak, &x509.CertificateRequest{}), "m", time.Hour, "2048"},
		{"bad id", good, "has space", time.Hour, "id"},
		{"empty id", good, "", time.Hour, "empty"},
		{"zero validity", good, "m", 0, "validity"},
		{"past CA expiry", good, "m", 20 * 365 * 24 * time.Hour, "CA expiry"},
	}
	for _, tc := range tests {
		_, err := ca.SignCSR(tc.csr, tc.id, nil, tc.val)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one mentioning %q", tc.name, err, tc.want)
		}
	}
	if _, err := ca.SignCSR(good, "m", []net.IP{{1, 2}}, time.Hour); err == nil {
		t.Error("an invalid IP was accepted")
	}
}
