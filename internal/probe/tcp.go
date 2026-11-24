package probe

import (
	"context"
	"net"
	"time"
)

// TCP probes a host:port by connecting: Latency is the connect time and OK is
// whether the connection was established. The connection is closed at once.
type TCP struct{}

// NewTCP returns the TCP connect prober.
func NewTCP() *TCP { return &TCP{} }

// Type implements Prober.
func (*TCP) Type() Type { return TypeTCP }

// Probe implements Prober.
func (*TCP) Probe(ctx context.Context, t Target, timeout time.Duration) Result {
	res := Result{Type: TypeTCP}
	addr := t.Address
	if t.IP != "" {
		_, port, err := net.SplitHostPort(t.Address)
		if err != nil {
			res.Error = "invalid tcp target address " + t.Address
			return res
		}
		addr = net.JoinHostPort(t.IP, port)
	}
	d := net.Dialer{Timeout: timeout}
	start := time.Now()
	res.Time = start
	conn, err := d.DialContext(ctx, "tcp", addr)
	res.Latency = time.Since(start)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	_ = conn.Close()
	res.OK = true
	return res
}
