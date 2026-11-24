package capture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

// serialize builds a wire packet from the given layers.
func serialize(t *testing.T, ls ...gopacket.SerializableLayer) []byte {
	t.Helper()
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}
	if err := gopacket.SerializeLayers(buf, opts, ls...); err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return buf.Bytes()
}

// tcpv4 builds an outbound IPv4/TCP packet with the given payload and SYN/ACK.
func tcpv4(t *testing.T, src, dst string, sport, dport int, syn, ack bool, payload []byte) []byte {
	ip := &layers.IPv4{
		Version: 4, IHL: 5, TTL: 64,
		Protocol: layers.IPProtocolTCP,
		SrcIP:    net.ParseIP(src).To4(), DstIP: net.ParseIP(dst).To4(),
	}
	tcp := &layers.TCP{
		SrcPort: layers.TCPPort(sport), DstPort: layers.TCPPort(dport),
		Seq: 1000, Ack: 2000, Window: 64240, SYN: syn, ACK: ack,
	}
	if err := tcp.SetNetworkLayerForChecksum(ip); err != nil {
		t.Fatalf("checksum layer: %v", err)
	}
	return serialize(t, ip, tcp, gopacket.Payload(payload))
}

func udpv4(t *testing.T, src, dst string, sport, dport int, payload []byte) []byte {
	ip := &layers.IPv4{
		Version: 4, IHL: 5, TTL: 64,
		Protocol: layers.IPProtocolUDP,
		SrcIP:    net.ParseIP(src).To4(), DstIP: net.ParseIP(dst).To4(),
	}
	udp := &layers.UDP{SrcPort: layers.UDPPort(sport), DstPort: layers.UDPPort(dport)}
	if err := udp.SetNetworkLayerForChecksum(ip); err != nil {
		t.Fatalf("checksum layer: %v", err)
	}
	return serialize(t, ip, udp, gopacket.Payload(payload))
}

func icmpv4(t *testing.T, src, dst string, payload []byte) []byte {
	ip := &layers.IPv4{
		Version: 4, IHL: 5, TTL: 64,
		Protocol: layers.IPProtocolICMPv4,
		SrcIP:    net.ParseIP(src).To4(), DstIP: net.ParseIP(dst).To4(),
	}
	icmp := &layers.ICMPv4{TypeCode: layers.CreateICMPv4TypeCode(8, 0)} // echo request
	return serialize(t, ip, icmp, gopacket.Payload(payload))
}

func tcpv6(t *testing.T, src, dst string, sport, dport int, payload []byte) []byte {
	ip := &layers.IPv6{
		Version: 6, HopLimit: 64, NextHeader: layers.IPProtocolTCP,
		SrcIP: net.ParseIP(src), DstIP: net.ParseIP(dst),
	}
	tcp := &layers.TCP{SrcPort: layers.TCPPort(sport), DstPort: layers.TCPPort(dport), PSH: true, ACK: true, Window: 100}
	if err := tcp.SetNetworkLayerForChecksum(ip); err != nil {
		t.Fatalf("checksum layer: %v", err)
	}
	return serialize(t, ip, tcp, gopacket.Payload(payload))
}

func TestBuildRecordFullPayload(t *testing.T) {
	payload := []byte("GET / HTTP/1.1\r\n\r\n")
	pkt := tcpv4(t, "192.168.1.5", "93.184.216.34", 51000, 80, false, true, payload)
	ts := time.Now()

	rec, err := BuildRecord("sess-1", ts, pkt, "203.0.113.9", 0)
	if err != nil {
		t.Fatalf("BuildRecord: %v", err)
	}
	if rec.SessionID != "sess-1" || rec.Protocol != ProtoTCP {
		t.Errorf("got session=%q proto=%q", rec.SessionID, rec.Protocol)
	}
	if rec.Src != "192.168.1.5:51000" || rec.Dst != "93.184.216.34:80" {
		t.Errorf("endpoints: src=%q dst=%q", rec.Src, rec.Dst)
	}
	if rec.Flags != "ACK" {
		t.Errorf("flags = %q, want ACK", rec.Flags)
	}
	if rec.PayloadStatus != PayloadFull || !bytes.Equal(rec.PayloadHead, payload) {
		t.Errorf("status=%q head=%q, want full with the complete payload %q", rec.PayloadStatus, rec.PayloadHead, payload)
	}
	if rec.PayloadBytesFull != len(payload) {
		t.Errorf("bytes_full = %d, want %d", rec.PayloadBytesFull, len(payload))
	}
	want := sha256.Sum256(payload)
	if rec.PayloadSHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("sha256 = %s, want %s", rec.PayloadSHA256, hex.EncodeToString(want[:]))
	}
	if rec.SrcIPUsed != "203.0.113.9" {
		t.Errorf("src_ip_used = %q", rec.SrcIPUsed)
	}
	if rec.Seq != 1000 || rec.Ack != 2000 || rec.Window != 64240 {
		t.Errorf("seq/ack/window = %d/%d/%d", rec.Seq, rec.Ack, rec.Window)
	}
}

func TestBuildRecordTrim(t *testing.T) {
	payload := make([]byte, 10000)
	for i := range payload {
		payload[i] = byte(i)
	}
	pkt := tcpv4(t, "10.0.0.2", "198.51.100.7", 40000, 443, false, true, payload)

	rec, err := BuildRecord("s", time.Now(), pkt, "", 4096)
	if err != nil {
		t.Fatalf("BuildRecord: %v", err)
	}
	if rec.PayloadStatus != PayloadTrimmed {
		t.Fatalf("status = %q, want trimmed", rec.PayloadStatus)
	}
	if rec.PayloadBytesFull != len(payload) {
		t.Errorf("bytes_full = %d, want %d", rec.PayloadBytesFull, len(payload))
	}
	if len(rec.PayloadHead) != 4096 {
		t.Errorf("head len = %d, want 4096", len(rec.PayloadHead))
	}
	for i := 0; i < 4096; i++ {
		if rec.PayloadHead[i] != payload[i] {
			t.Fatalf("head[%d] mismatch", i)
		}
	}
	// sha256 must be of the FULL payload, not the trimmed head.
	want := sha256.Sum256(payload)
	if rec.PayloadSHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("sha256 is not of the full payload")
	}
}

// TestPayloadHeadAtThresholdBoundary pins the full/trimmed split: a payload of
// exactly the threshold is full (complete payload in payload_head); one byte
// more is trimmed (first threshold bytes only). Full records round-trip through
// JSON with their payload intact.
func TestPayloadHeadAtThresholdBoundary(t *testing.T) {
	const threshold = 100
	mk := func(n int) []byte {
		p := make([]byte, n)
		for i := range p {
			p[i] = byte(i*7 + 1)
		}
		return p
	}
	cases := []struct {
		name     string
		size     int
		status   string
		wantHead int
	}{
		{"empty", 0, PayloadFull, 0},
		{"one byte", 1, PayloadFull, 1},
		{"below threshold", threshold - 1, PayloadFull, threshold - 1},
		{"exactly threshold", threshold, PayloadFull, threshold},
		{"threshold plus one", threshold + 1, PayloadTrimmed, threshold},
		{"far above threshold", 5 * threshold, PayloadTrimmed, threshold},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := mk(tc.size)
			pkt := tcpv4(t, "10.0.0.2", "198.51.100.7", 40000, 443, false, true, payload)
			rec, err := BuildRecord("s", time.Now(), pkt, "", threshold)
			if err != nil {
				t.Fatalf("BuildRecord: %v", err)
			}
			if rec.PayloadStatus != tc.status {
				t.Fatalf("status = %q, want %q", rec.PayloadStatus, tc.status)
			}
			if len(rec.PayloadHead) != tc.wantHead {
				t.Fatalf("payload_head len = %d, want %d", len(rec.PayloadHead), tc.wantHead)
			}
			if !bytes.Equal(rec.PayloadHead, payload[:tc.wantHead]) {
				t.Errorf("payload_head is not the payload's first %d bytes", tc.wantHead)
			}
			if rec.PayloadBytesFull != tc.size {
				t.Errorf("bytes_full = %d, want %d", rec.PayloadBytesFull, tc.size)
			}
			sum := sha256.Sum256(payload)
			if rec.PayloadSHA256 != hex.EncodeToString(sum[:]) {
				t.Errorf("sha256 is not of the full payload")
			}

			// JSON round-trip (the segment line format) keeps payload_head.
			line, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			var back Record
			if err := json.Unmarshal(line, &back); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(back.PayloadHead, rec.PayloadHead) || back.PayloadStatus != rec.PayloadStatus {
				t.Errorf("payload_head/status did not round-trip through JSON")
			}
		})
	}
}

// TestFullPayloadHeadIsACopy: the stored payload must not alias the packet
// buffer (the live loop reuses its read buffer).
func TestFullPayloadHeadIsACopy(t *testing.T) {
	payload := []byte("sensitive-evidence")
	pkt := tcpv4(t, "10.0.0.2", "198.51.100.7", 40000, 443, false, true, payload)
	rec, err := BuildRecord("s", time.Now(), pkt, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range pkt {
		pkt[i] = 0
	}
	if !bytes.Equal(rec.PayloadHead, payload) {
		t.Errorf("payload_head changed when the packet buffer was reused: %q", rec.PayloadHead)
	}
}

func TestBuildRecordThresholdCappedAt4K(t *testing.T) {
	payload := make([]byte, 9000)
	pkt := tcpv4(t, "10.0.0.2", "198.51.100.7", 40000, 443, false, true, payload)
	// Ask for an 8K threshold; it must be capped to the 4K head maximum.
	rec, err := BuildRecord("s", time.Now(), pkt, "", 8192)
	if err != nil {
		t.Fatalf("BuildRecord: %v", err)
	}
	if len(rec.PayloadHead) != MaxPayloadHead {
		t.Errorf("head len = %d, want %d", len(rec.PayloadHead), MaxPayloadHead)
	}
}

func TestBuildRecordUDPandICMP(t *testing.T) {
	u := udpv4(t, "172.16.0.9", "8.8.8.8", 5353, 53, []byte("query"))
	rec, err := BuildRecord("s", time.Now(), u, "", 0)
	if err != nil {
		t.Fatalf("udp: %v", err)
	}
	if rec.Protocol != ProtoUDP || rec.Dst != "8.8.8.8:53" {
		t.Errorf("udp record: proto=%q dst=%q", rec.Protocol, rec.Dst)
	}

	ic := icmpv4(t, "172.16.0.9", "8.8.8.8", []byte("ping-data"))
	rec, err = BuildRecord("s", time.Now(), ic, "", 0)
	if err != nil {
		t.Fatalf("icmp: %v", err)
	}
	if rec.Protocol != ProtoICMP || rec.Src != "172.16.0.9" || rec.Dst != "8.8.8.8" {
		t.Errorf("icmp record: proto=%q src=%q dst=%q", rec.Protocol, rec.Src, rec.Dst)
	}
	if rec.Flags != "8/0" {
		t.Errorf("icmp flags = %q, want 8/0", rec.Flags)
	}
}

func TestBuildRecordIPv6(t *testing.T) {
	pkt := tcpv6(t, "2001:db8::1", "2606:2800:220:1:248:1893:25c8:1946", 40000, 443, []byte("hi"))
	rec, err := BuildRecord("s", time.Now(), pkt, "", 0)
	if err != nil {
		t.Fatalf("ipv6: %v", err)
	}
	if rec.Protocol != ProtoTCP || rec.Flags != "PSH,ACK" {
		t.Errorf("ipv6 record: proto=%q flags=%q", rec.Protocol, rec.Flags)
	}
	if rec.Src != "[2001:db8::1]:40000" {
		t.Errorf("ipv6 src = %q", rec.Src)
	}
}

func TestBuildRecordNonIP(t *testing.T) {
	if _, err := BuildRecord("s", time.Now(), []byte{0x00, 0x01, 0x02}, "", 0); err == nil {
		t.Error("expected error for non-IP bytes")
	}
	if _, err := BuildRecord("s", time.Now(), nil, "", 0); err == nil {
		t.Error("expected error for empty bytes")
	}
}
