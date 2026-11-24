package pki

import (
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// All keys in these tests are generated at runtime into t.TempDir(); no real
// key material is ever read or written.

func mustCA(t *testing.T) *CA {
	t.Helper()
	ca, err := NewCA(Options{})
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	return ca
}

func TestNewCAProperties(t *testing.T) {
	ca := mustCA(t)
	c := ca.cert
	if !c.IsCA || !c.BasicConstraintsValid {
		t.Fatal("CA cert must be a CA")
	}
	if c.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Fatal("CA cert must have KeyUsageCertSign")
	}
	if got := c.NotAfter.Sub(c.NotBefore); got < 9*365*24*time.Hour {
		t.Fatalf("CA validity too short: %v", got)
	}
	if string(ca.CertPEM()) != string(ca.certPEM) {
		t.Fatal("CertPEM mismatch")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	ca := mustCA(t)
	dir := filepath.Join(t.TempDir(), "ca")
	if err := ca.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, CAKeyFile))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("CA key mode = %v, want 0600", fi.Mode().Perm())
		}
	}
	loaded, err := LoadCA(dir)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if !loaded.cert.Equal(ca.cert) {
		t.Fatal("loaded CA cert differs")
	}

	// The loaded CA issues a cert that verifies against the ORIGINAL CA, and
	// vice versa: same capability as a fresh one.
	for name, issuer := range map[string]*CA{"loaded": loaded, "fresh": ca} {
		certPEM, _, err := issuer.IssuePeer("peer-1", []net.IP{net.ParseIP("10.0.0.5")}, time.Hour)
		if err != nil {
			t.Fatalf("%s IssuePeer: %v", name, err)
		}
		leaf, err := parseCertPEM(certPEM)
		if err != nil {
			t.Fatal(err)
		}
		pool := x509.NewCertPool()
		pool.AddCert(ca.cert)
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots:     pool,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err != nil {
			t.Fatalf("%s-issued cert does not verify: %v", name, err)
		}
	}
}

func TestLoadCAErrors(t *testing.T) {
	if _, err := LoadCA(t.TempDir()); err == nil {
		t.Fatal("LoadCA of empty dir should fail")
	}
	// Mismatched key and cert.
	a, b := mustCA(t), mustCA(t)
	dir := t.TempDir()
	if err := a.Save(dir); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := b.Save(other); err != nil {
		t.Fatal(err)
	}
	keyB, _ := os.ReadFile(filepath.Join(other, CAKeyFile))
	if err := os.WriteFile(filepath.Join(dir, CAKeyFile), keyB, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCA(dir); err == nil {
		t.Fatal("LoadCA with mismatched key should fail")
	}
}

func TestIssuePeerCert(t *testing.T) {
	ca := mustCA(t)
	ips := []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("2001:db8::1")}
	certPEM, keyPEM, err := ca.IssuePeer("agent-42", ips, 24*time.Hour)
	if err != nil {
		t.Fatalf("IssuePeer: %v", err)
	}
	if len(keyPEM) == 0 {
		t.Fatal("no key returned")
	}
	leaf, err := parseCertPEM(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Subject.CommonName != "agent-42" {
		t.Fatalf("CN = %q", leaf.Subject.CommonName)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "agent-42" {
		t.Fatalf("DNS SANs = %v", leaf.DNSNames)
	}
	if len(leaf.IPAddresses) != 2 || !leaf.IPAddresses[0].Equal(ips[0]) || !leaf.IPAddresses[1].Equal(ips[1]) {
		t.Fatalf("IP SANs = %v", leaf.IPAddresses)
	}
	if leaf.IsCA {
		t.Fatal("peer cert must not be a CA")
	}
	var server, client bool
	for _, eku := range leaf.ExtKeyUsage {
		server = server || eku == x509.ExtKeyUsageServerAuth
		client = client || eku == x509.ExtKeyUsageClientAuth
	}
	if !server || !client {
		t.Fatalf("EKUs = %v, want serverAuth and clientAuth", leaf.ExtKeyUsage)
	}
	if err := leaf.CheckSignatureFrom(ca.cert); err != nil {
		t.Fatalf("not signed by CA: %v", err)
	}
}

func TestIssuePeerValidation(t *testing.T) {
	ca := mustCA(t)
	ip := []net.IP{net.ParseIP("10.0.0.1")}
	cases := []struct {
		name     string
		id       string
		ips      []net.IP
		validity time.Duration
	}{
		{"empty id", "", ip, time.Hour},
		{"space in id", "a b", ip, time.Hour},
		{"wildcard id", "*", ip, time.Hour},
		{"zero validity", "ok", ip, 0},
		{"past CA expiry", "ok", ip, 100 * 365 * 24 * time.Hour},
		{"bad ip", "ok", []net.IP{{1, 2, 3}}, time.Hour},
	}
	for _, tc := range cases {
		if _, _, err := ca.IssuePeer(tc.id, tc.ips, tc.validity); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
}
