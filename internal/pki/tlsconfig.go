package pki

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
)

// ServerTLSConfig is the config for the side that accepts the connection. It
// presents the peer cert and requires the far side to present a cert that
// chains to our CA (RequireAndVerifyClientCert). The system roots are never
// used; caPEM is the only trust anchor.
func ServerTLSConfig(certPEM, keyPEM, caPEM []byte) (*tls.Config, error) {
	cert, pool, err := loadPeer(certPEM, keyPEM, caPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}, nil
}

// DialTLSConfig is the config for the side that opens the connection. It
// presents the peer cert, verifies the far side against our CA only, and
// requires the far side's cert to cover expectedIP as an IP SAN. ServerName is
// set to the IP's text form, which Go's verifier matches against IP SANs (and
// which suppresses SNI, as it should for an IP).
func DialTLSConfig(certPEM, keyPEM, caPEM []byte, expectedIP net.IP) (*tls.Config, error) {
	if len(expectedIP) != net.IPv4len && len(expectedIP) != net.IPv6len {
		return nil, errors.New("pki: expected IP is not a valid IP address")
	}
	cert, pool, err := loadPeer(certPEM, keyPEM, caPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   expectedIP.String(),
	}, nil
}

// PeerID returns the id (certificate CN) of the far side of a completed
// handshake. It reads only the verified chain, so it fails if the connection
// state carries no verified peer (e.g. verification was skipped, or a server
// without client auth saw no client cert).
func PeerID(cs *tls.ConnectionState) (string, error) {
	if cs == nil {
		return "", errors.New("pki: nil connection state")
	}
	if len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 {
		return "", errors.New("pki: no verified peer certificate")
	}
	id := cs.VerifiedChains[0][0].Subject.CommonName
	if id == "" {
		return "", errors.New("pki: peer certificate has no id")
	}
	return id, nil
}

// loadPeer parses the peer key pair (checking that key and cert match) and
// builds a pool holding only our CA.
func loadPeer(certPEM, keyPEM, caPEM []byte) (tls.Certificate, *x509.CertPool, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("pki: load peer key pair: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return tls.Certificate{}, nil, errors.New("pki: no valid CA certificate in caPEM")
	}
	return cert, pool, nil
}
