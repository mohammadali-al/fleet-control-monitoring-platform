package probe

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// HTTP probes an HTTP(S) URL: status, latency (request start to the end of the
// response body, bounded), and for HTTPS the TLS session details. A result is
// OK when the server answered with a status below 400. Redirects are not
// followed: the monitor reports what the target itself answers.
type HTTP struct {
	// TLSConfig, if set, is used for HTTPS (tests use it to trust a test CA).
	TLSConfig *tls.Config
}

// NewHTTP returns the HTTP(S) prober.
func NewHTTP() *HTTP { return &HTTP{} }

// Type implements Prober.
func (*HTTP) Type() Type { return TypeHTTP }

// maxBodyRead bounds how much of a response body a probe drains.
const maxBodyRead = 64 << 10

// Probe implements Prober.
func (p *HTTP) Probe(ctx context.Context, t Target, timeout time.Duration) Result {
	res := Result{Type: TypeHTTP}
	u, err := url.Parse(t.Address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		res.Error = "invalid http target address " + t.Address
		return res
	}
	tr := &http.Transport{
		TLSClientConfig:   p.TLSConfig.Clone(),
		DisableKeepAlives: true,
		Proxy:             nil,
	}
	if t.IP != "" {
		port := u.Port()
		if port == "" {
			port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
		}
		target := net.JoinHostPort(t.IP, port)
		var d net.Dialer
		tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, target)
		}
	}
	client := &http.Client{
		Transport:     tr,
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer tr.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.Address, nil)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	start := time.Now()
	res.Time = start
	resp, err := client.Do(req)
	if err != nil {
		res.Latency = time.Since(start)
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyRead))
	res.Latency = time.Since(start)
	res.Status = resp.StatusCode
	res.OK = resp.StatusCode < 400
	if resp.TLS != nil {
		res.TLS = tlsInfo(resp.TLS)
	}
	return res
}

func tlsInfo(cs *tls.ConnectionState) *TLSInfo {
	info := &TLSInfo{
		Version: tls.VersionName(cs.Version),
		Cipher:  tls.CipherSuiteName(cs.CipherSuite),
		ALPN:    cs.NegotiatedProtocol,
	}
	if len(cs.PeerCertificates) > 0 {
		leaf := cs.PeerCertificates[0]
		info.Subject = leaf.Subject.String()
		info.Issuer = leaf.Issuer.String()
		info.NotAfter = leaf.NotAfter
		info.DNSNames = leaf.DNSNames
	}
	return info
}
