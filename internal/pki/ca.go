// Package pki is the platform's internal certificate authority and the TLS
// helpers built on it (architecture 4.4: internal private CA, IP-SAN certs,
// mTLS on every channel).
//
// Design summary:
//   - One CA per deployment. Every machine, the main server included, is a
//     "peer" holding one leaf certificate that carries BOTH serverAuth and
//     clientAuth, so it works whichever side dials (default server_dial makes
//     the agent the TLS server, architecture 4.1).
//   - Peers trust only this CA; system roots are never consulted.
//   - PEM []byte is the interchange type. Where keys live on disk is decided
//     by the callers (T-0003 and later), not here.
//
// Revocation (CRL / OCSP) is deliberately NOT implemented: architecture 14
// mentions revoking a machine, but the channel does not need it to work. It is
// deferred to a later task.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// CACertFile and CAKeyFile are the file names Save and LoadCA use inside
	// the directory they are given.
	CACertFile = "ca.crt"
	CAKeyFile  = "ca.key"

	defaultCAName     = "Fleet Internal CA"
	defaultCAValidity = 10 * 365 * 24 * time.Hour

	// clockSkew backdates NotBefore so a freshly issued cert is accepted by a
	// peer whose clock runs slightly behind.
	clockSkew = 5 * time.Minute
)

// Options configures NewCA. The zero value is valid.
type Options struct {
	// CommonName of the CA certificate. Default "Fleet Internal CA".
	CommonName string
	// Validity of the CA certificate. Default 10 years.
	Validity time.Duration
}

// CA is the internal certificate authority: a CA certificate plus its signing
// key. It is safe for concurrent use (it is immutable after construction).
type CA struct {
	cert    *x509.Certificate
	certPEM []byte
	key     crypto.Signer
	now     func() time.Time
}

// NewCA generates a fresh CA key and a self-signed CA certificate.
//
// Key algorithm: ECDSA P-256. It is universally supported by TLS stacks
// (including FIPS-oriented ones), the signatures and certs are small, and
// Go's crypto/x509 handles it with no special cases. Ed25519 would also work
// but offers nothing we need here.
func NewCA(opts Options) (*CA, error) {
	name := opts.CommonName
	if name == "" {
		name = defaultCAName
	}
	validity := opts.Validity
	if validity == 0 {
		validity = defaultCAValidity
	}
	if validity < 0 {
		return nil, errors.New("pki: CA validity must be positive")
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("pki: generate CA key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(validity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
		MaxPathLenZero:        true, // the CA signs leaves only, no sub-CAs
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, fmt.Errorf("pki: self-sign CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("pki: parse new CA certificate: %w", err)
	}
	return &CA{
		cert:    cert,
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		key:     key,
		now:     time.Now,
	}, nil
}

// CertPEM returns the CA certificate in PEM form (what peers pin as their
// only trust root).
func (c *CA) CertPEM() []byte {
	return append([]byte(nil), c.certPEM...)
}

// Save writes the CA certificate (ca.crt, 0644) and private key (ca.key, 0600)
// as PEM files into dir, creating dir (0700) if needed. The key is PKCS#8.
func (c *CA) Save(dir string) error {
	keyPEM, err := marshalKeyPEM(c.key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("pki: create CA dir: %w", err)
	}
	if err := writeFile(filepath.Join(dir, CAKeyFile), keyPEM, 0o600); err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, CACertFile), c.certPEM, 0o644)
}

// LoadCA reads a CA previously written by Save. The returned CA issues certs
// identical in capability to the one that was saved.
func LoadCA(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, CACertFile))
	if err != nil {
		return nil, fmt.Errorf("pki: read CA cert: %w", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, CAKeyFile))
	if err != nil {
		return nil, fmt.Errorf("pki: read CA key: %w", err)
	}
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("pki: CA cert: %w", err)
	}
	if !cert.IsCA {
		return nil, errors.New("pki: loaded certificate is not a CA")
	}
	key, err := parseKeyPEM(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("pki: CA key: %w", err)
	}
	if !publicKeysEqual(cert.PublicKey, key.Public()) {
		return nil, errors.New("pki: CA key does not match CA certificate")
	}
	return &CA{cert: cert, certPEM: certPEM, key: key, now: time.Now}, nil
}

// IssuePeer issues one leaf certificate for a machine (the main server is a
// peer too). The cert carries both serverAuth and clientAuth EKUs, id as the
// subject CN and as a DNS SAN, and ips as IP SANs, and is signed by the CA.
// The returned key is a fresh ECDSA P-256 key in PKCS#8 PEM.
//
// validity must be positive and must not run past the CA's own expiry.
func (c *CA) IssuePeer(id string, ips []net.IP, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	if err := validatePeerID(id); err != nil {
		return nil, nil, err
	}
	if validity <= 0 {
		return nil, nil, errors.New("pki: peer validity must be positive")
	}
	now := c.now()
	notAfter := now.Add(validity)
	if notAfter.After(c.cert.NotAfter) {
		return nil, nil, errors.New("pki: peer validity extends past CA expiry")
	}
	ipSANs := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		if ip == nil || len(ip) != net.IPv4len && len(ip) != net.IPv6len {
			return nil, nil, errors.New("pki: invalid IP address for peer SAN")
		}
		ipSANs = append(ipSANs, ip)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: generate peer key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: id},
		DNSNames:              []string{id},
		IPAddresses:           ipSANs,
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              notAfter,
		BasicConstraintsValid: true,
		IsCA:                  false,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, key.Public(), c.key)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: sign peer certificate: %w", err)
	}
	keyPEM, err = marshalKeyPEM(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return certPEM, keyPEM, nil
}

// SignCSR signs a certificate for the public key in a PKCS#10 certificate
// request (PEM), so the requester keeps its private key (token enrollment,
// T-0026). Only the request's public key is used, after its self-signature
// proves possession of the matching private key: the subject, SANs and
// extensions the requester asked for are ignored. The certificate is a peer
// leaf exactly as IssuePeer makes it (CN and DNS SAN = id, serverAuth and
// clientAuth), so the machine's identity is what the server decided, not what
// the machine claimed.
//
// Accepted keys: ECDSA (P-256 or larger), Ed25519, RSA >= 2048 bits.
func (c *CA) SignCSR(csrPEM []byte, id string, ips []net.IP, validity time.Duration) (certPEM []byte, err error) {
	if err := validatePeerID(id); err != nil {
		return nil, err
	}
	if validity <= 0 {
		return nil, errors.New("pki: peer validity must be positive")
	}
	block, _ := pem.Decode(csrPEM)
	if block == nil || (block.Type != "CERTIFICATE REQUEST" && block.Type != "NEW CERTIFICATE REQUEST") {
		return nil, errors.New("pki: no CERTIFICATE REQUEST PEM block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pki: parse certificate request: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("pki: certificate request signature: %w", err)
	}
	if err := checkRequestKey(csr.PublicKey); err != nil {
		return nil, err
	}
	now := c.now()
	notAfter := now.Add(validity)
	if notAfter.After(c.cert.NotAfter) {
		return nil, errors.New("pki: peer validity extends past CA expiry")
	}
	for _, ip := range ips {
		if ip == nil || len(ip) != net.IPv4len && len(ip) != net.IPv6len {
			return nil, errors.New("pki: invalid IP address for peer SAN")
		}
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: id},
		DNSNames:              []string{id},
		IPAddresses:           ips,
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              notAfter,
		BasicConstraintsValid: true,
		IsCA:                  false,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, csr.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("pki: sign certificate request: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

func checkRequestKey(pub crypto.PublicKey) error {
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		if k.Curve.Params().BitSize < 256 {
			return errors.New("pki: request key is a weak elliptic curve")
		}
	case ed25519.PublicKey:
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return errors.New("pki: request RSA key is smaller than 2048 bits")
		}
	default:
		return fmt.Errorf("pki: unsupported request key type %T", pub)
	}
	return nil
}

// validatePeerID requires a non-empty id of printable ASCII without spaces, so
// it is safe both as a CN and as a DNS SAN (IA5String).
func validatePeerID(id string) error {
	if id == "" {
		return errors.New("pki: peer id must not be empty")
	}
	if len(id) > 64 {
		return errors.New("pki: peer id too long (max 64)")
	}
	for _, r := range id {
		if r <= ' ' || r >= 0x7f {
			return fmt.Errorf("pki: peer id %q must be printable ASCII without spaces", id)
		}
	}
	if strings.ContainsAny(id, "*") {
		return errors.New("pki: peer id must not contain a wildcard")
	}
	return nil
}

func newSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 127)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("pki: generate serial: %w", err)
	}
	return n.Add(n, big.NewInt(1)), nil // strictly positive
}

func marshalKeyPEM(key crypto.Signer) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("pki: marshal private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func parseKeyPEM(keyPEM []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("no PKCS#8 PRIVATE KEY PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	s, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("private key is not a signer")
	}
	return s, nil
}

func parseCertPEM(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no CERTIFICATE PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

func publicKeysEqual(a, b crypto.PublicKey) bool {
	type equaler interface{ Equal(crypto.PublicKey) bool }
	if ea, ok := a.(equaler); ok {
		return ea.Equal(b)
	}
	return false
}

// writeFile writes data to path with the given mode, replacing any existing
// file and tightening its mode even if it already existed.
func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.WriteFile(path, data, mode); err != nil {
		return fmt.Errorf("pki: write %s: %w", filepath.Base(path), err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("pki: chmod %s: %w", filepath.Base(path), err)
	}
	return nil
}
