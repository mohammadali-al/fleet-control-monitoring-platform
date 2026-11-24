package probe

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// ICMP probes a host with echo requests: Latency is the mean RTT of the
// replies, Loss the lost fraction. It needs an ICMP socket: an unprivileged
// "ping" datagram socket where the kernel allows it (Linux
// net.ipv4.ping_group_range), else a raw socket (root / CAP_NET_RAW). If
// neither can be opened the result is a failure naming the cause.
type ICMP struct{}

// NewICMP returns the ICMP prober.
func NewICMP() *ICMP { return &ICMP{} }

// Type implements Prober.
func (*ICMP) Type() Type { return TypeICMP }

// ICMPAvailable reports whether this process can open an ICMP socket for IPv4
// (nil) or why not. Tests use it to skip the live ICMP probe off-root.
func ICMPAvailable() error {
	c, _, err := listenICMP(false)
	if err != nil {
		return err
	}
	return c.Close()
}

// listenICMP opens an unprivileged datagram ICMP socket, falling back to a raw
// one. raw reports which was opened (raw sockets see every ICMP reply, so the
// echo id must be matched; datagram sockets have the id rewritten by the
// kernel).
func listenICMP(v6 bool) (c *icmp.PacketConn, raw bool, err error) {
	dgram, rawNet, bind := "udp4", "ip4:icmp", "0.0.0.0"
	if v6 {
		dgram, rawNet, bind = "udp6", "ip6:ipv6-icmp", "::"
	}
	c, derr := icmp.ListenPacket(dgram, bind)
	if derr == nil {
		return c, false, nil
	}
	c, rerr := icmp.ListenPacket(rawNet, bind)
	if rerr == nil {
		return c, true, nil
	}
	return nil, false, fmt.Errorf("icmp: cannot open a socket (unprivileged: %v; raw: %v)", derr, rerr)
}

// Probe implements Prober.
func (*ICMP) Probe(ctx context.Context, t Target, timeout time.Duration) Result {
	res := Result{Type: TypeICMP}
	host := t.Address
	if t.IP != "" {
		host = t.IP
	}
	dst, err := net.ResolveIPAddr("ip", host)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	v6 := dst.IP.To4() == nil
	count := max(t.Count, 1)
	res.Sent = count

	conn, raw, err := listenICMP(v6)
	if err != nil {
		res.Error = err.Error()
		res.Loss = 1
		return res
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	// Unblock a pending read if ctx is cancelled before the deadline.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	var (
		proto   = 1
		reqType icmp.Type
		repType icmp.Type
		addr    net.Addr = dst
	)
	reqType, repType = ipv4.ICMPTypeEcho, ipv4.ICMPTypeEchoReply
	if v6 {
		proto = 58
		reqType, repType = ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply
	}
	if !raw {
		addr = &net.UDPAddr{IP: dst.IP, Zone: dst.Zone}
	}

	id := int(rand.Uint32() & 0xffff)
	sentAt := make([]time.Time, count)
	res.Time = time.Now()
	for seq := range count {
		msg := icmp.Message{Type: reqType, Code: 0, Body: &icmp.Echo{ID: id, Seq: seq, Data: []byte("fleet-monitor")}}
		b, err := msg.Marshal(nil)
		if err != nil {
			res.Error = err.Error()
			return res
		}
		sentAt[seq] = time.Now()
		if _, err := conn.WriteTo(b, addr); err != nil {
			res.Error = err.Error()
			res.Loss = 1
			return res
		}
	}

	got := make([]bool, count)
	var total time.Duration
	buf := make([]byte, 1500)
	for res.Received < count {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			var ne net.Error
			if !errors.As(err, &ne) || !ne.Timeout() {
				res.Error = err.Error()
			}
			break
		}
		now := time.Now()
		m, err := icmp.ParseMessage(proto, buf[:n])
		if err != nil || m.Type != repType {
			continue
		}
		echo, ok := m.Body.(*icmp.Echo)
		if !ok || echo.Seq < 0 || echo.Seq >= count || got[echo.Seq] {
			continue
		}
		if raw && echo.ID != id {
			continue
		}
		got[echo.Seq] = true
		res.Received++
		total += now.Sub(sentAt[echo.Seq])
	}
	res.Loss = float64(count-res.Received) / float64(count)
	if res.Received > 0 {
		res.Latency = total / time.Duration(res.Received)
		res.OK = true
		res.Error = ""
	} else if res.Error == "" {
		res.Latency = time.Since(res.Time)
		res.Error = "no echo reply (timeout)"
	}
	return res
}
