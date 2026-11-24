package probe

import (
	"context"
	"net"
	"sort"
	"time"
)

// DNS probes a name: Latency is the query time and Answer the resolved
// addresses. With Target.Server set the query goes to that resolver
// (host:port, over UDP), otherwise to the system resolver. A result is OK when
// the name resolved to at least one address.
type DNS struct{}

// NewDNS returns the DNS prober.
func NewDNS() *DNS { return &DNS{} }

// Type implements Prober.
func (*DNS) Type() Type { return TypeDNS }

// Probe implements Prober.
func (*DNS) Probe(ctx context.Context, t Target, timeout time.Duration) Result {
	res := Result{Type: TypeDNS}
	r := net.DefaultResolver
	if t.Server != "" {
		server := t.Server
		r = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: timeout}
				return d.DialContext(ctx, "udp", server)
			},
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	res.Time = start
	addrs, err := r.LookupNetIP(ctx, "ip", t.Address)
	res.Latency = time.Since(start)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	for _, a := range addrs {
		res.Answer = append(res.Answer, a.String())
	}
	sort.Strings(res.Answer)
	res.OK = len(res.Answer) > 0
	if !res.OK {
		res.Error = "no answer"
	}
	return res
}
