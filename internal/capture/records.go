// Package capture is the worker-agent evidence-capture subsystem (architecture
// chapter 6, 7.1). It binds to a command session's cgroup v2 directory (created
// by internal/exec, T-0007), captures the outbound traffic that session
// produces, turns each packet into a §6.3 record, and writes those records to
// local segment files with a SHA-256 manifest under DataDir/evidence/<machine>/
// (D-0006).
//
// Scope: agent-local capture + records + local storage only. Sync to the main
// server and export (pcapng/CSV/JSON) are T-0009 and are deliberately absent.
//
// The record/trim/SHA-256/source-IP/segment/manifest logic is pure and fully
// unit-testable without root by feeding synthetic packets to BuildRecord. The
// live kernel path (capture_live.go) needs root and is exercised only there.
package capture

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

// SchemaVersion is the record/segment schema version recorded in the manifest.
// Bump it on any incompatible change to the record or segment layout.
const SchemaVersion = 1

// DefaultTrimThreshold is the per-node payload trim threshold (§6.3): payloads
// longer than this are stored trimmed, with payload_head holding the first
// TrimThreshold bytes (the architecture caps the head at 4 KB).
const DefaultTrimThreshold = 4096

// MaxPayloadHead caps payload_head regardless of the configured threshold
// (§6.3: "First <=4 KB when trimmed").
const MaxPayloadHead = 4096

// Payload status values.
const (
	PayloadFull    = "full"
	PayloadTrimmed = "trimmed"
)

// Protocol values.
const (
	ProtoTCP  = "TCP"
	ProtoUDP  = "UDP"
	ProtoICMP = "ICMP"
)

// ErrNotIP is returned by BuildRecord when the bytes are not an IPv4/IPv6
// packet it can decode (non-IP link traffic is ignored, not recorded).
var ErrNotIP = errors.New("capture: not an IPv4/IPv6 packet")

// ErrUnsupportedProto is returned for IP packets that are not TCP/UDP/ICMP.
var ErrUnsupportedProto = errors.New("capture: unsupported L4 protocol")

// Record is one captured outbound packet (§6.3). It is the unit written to
// segment files as newline-delimited JSON (plaintext, uncompressed).
type Record struct {
	SessionID string    `json:"session_id"`
	Timestamp time.Time `json:"timestamp"` // capture time, sub-ms, NTP-disciplined clock
	Protocol  string    `json:"protocol"`  // TCP | UDP | ICMP
	Src       string    `json:"src"`       // ip:port (ip for ICMP), as seen on the wire
	Dst       string    `json:"dst"`
	Flags     string    `json:"flags"` // TCP flags (e.g. "SYN,ACK") or ICMP "type/code"

	Seq    uint32 `json:"seq,omitempty"`
	Ack    uint32 `json:"ack,omitempty"`
	Window uint16 `json:"window,omitempty"`

	PayloadStatus    string `json:"payload_status"`         // full | trimmed
	PayloadBytesFull int    `json:"payload_bytes_full"`     // exact total payload length, always
	PayloadSHA256    string `json:"payload_sha256"`         // hex SHA-256 of the full payload, always
	PayloadHead      []byte `json:"payload_head,omitempty"` // full payload when full; first <=4 KB when trimmed

	SrcIPUsed string `json:"src_ip_used"` // attribution source address (§6.4)
}

// decoded holds the protocol-specific fields pulled from one packet.
type decoded struct {
	protocol string
	srcIP    net.IP
	dstIP    net.IP
	srcPort  int // -1 when not applicable (ICMP)
	dstPort  int
	flags    string
	seq      uint32
	ack      uint32
	window   uint16
	payload  []byte // L4/application payload (the bytes hashed and trimmed)
}

// decode parses an outbound IPv4/IPv6 packet (starting at the IP header) into
// its protocol fields using gopacket. It is pure and root-free.
func decode(data []byte) (decoded, error) {
	if len(data) == 0 {
		return decoded{}, ErrNotIP
	}
	var first gopacket.LayerType
	switch data[0] >> 4 {
	case 4:
		first = layers.LayerTypeIPv4
	case 6:
		first = layers.LayerTypeIPv6
	default:
		return decoded{}, ErrNotIP
	}
	pkt := gopacket.NewPacket(data, first, gopacket.DecodeOptions{Lazy: false, NoCopy: true})
	if pkt.NetworkLayer() == nil {
		if el := pkt.ErrorLayer(); el != nil {
			return decoded{}, fmt.Errorf("capture: decode: %w", el.Error())
		}
		return decoded{}, ErrNotIP
	}
	d := decoded{srcPort: -1, dstPort: -1}
	switch nl := pkt.NetworkLayer().(type) {
	case *layers.IPv4:
		d.srcIP, d.dstIP = nl.SrcIP, nl.DstIP
	case *layers.IPv6:
		d.srcIP, d.dstIP = nl.SrcIP, nl.DstIP
	default:
		return decoded{}, ErrNotIP
	}

	switch {
	case pkt.Layer(layers.LayerTypeTCP) != nil:
		t := pkt.Layer(layers.LayerTypeTCP).(*layers.TCP)
		d.protocol = ProtoTCP
		d.srcPort, d.dstPort = int(t.SrcPort), int(t.DstPort)
		d.flags = tcpFlags(t)
		d.seq, d.ack, d.window = t.Seq, t.Ack, t.Window
		d.payload = t.LayerPayload()
	case pkt.Layer(layers.LayerTypeUDP) != nil:
		u := pkt.Layer(layers.LayerTypeUDP).(*layers.UDP)
		d.protocol = ProtoUDP
		d.srcPort, d.dstPort = int(u.SrcPort), int(u.DstPort)
		d.payload = u.LayerPayload()
	case pkt.Layer(layers.LayerTypeICMPv4) != nil:
		ic := pkt.Layer(layers.LayerTypeICMPv4).(*layers.ICMPv4)
		d.protocol = ProtoICMP
		d.flags = fmt.Sprintf("%d/%d", ic.TypeCode.Type(), ic.TypeCode.Code())
		d.payload = ic.LayerPayload()
	case pkt.Layer(layers.LayerTypeICMPv6) != nil:
		ic := pkt.Layer(layers.LayerTypeICMPv6).(*layers.ICMPv6)
		d.protocol = ProtoICMP
		d.flags = fmt.Sprintf("%d/%d", ic.TypeCode.Type(), ic.TypeCode.Code())
		d.payload = ic.LayerPayload()
	default:
		return decoded{}, ErrUnsupportedProto
	}
	return d, nil
}

// tcpFlags renders the set TCP flags in a stable order.
func tcpFlags(t *layers.TCP) string {
	var f []string
	if t.FIN {
		f = append(f, "FIN")
	}
	if t.SYN {
		f = append(f, "SYN")
	}
	if t.RST {
		f = append(f, "RST")
	}
	if t.PSH {
		f = append(f, "PSH")
	}
	if t.ACK {
		f = append(f, "ACK")
	}
	if t.URG {
		f = append(f, "URG")
	}
	if t.ECE {
		f = append(f, "ECE")
	}
	if t.CWR {
		f = append(f, "CWR")
	}
	if t.NS {
		f = append(f, "NS")
	}
	return strings.Join(f, ",")
}

// endpoint renders ip:port, or just the IP when port < 0 (ICMP).
func endpoint(ip net.IP, port int) string {
	if port < 0 {
		return ip.String()
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(port))
}

// BuildRecord turns one raw outbound IP packet into a §6.3 Record. ts is the
// capture time, srcIPUsed the attribution address (§6.4), and trimThreshold the
// per-node payload threshold (<=0 selects DefaultTrimThreshold). payload_sha256
// and payload_bytes_full always describe the full payload. payload_head holds
// the bytes kept on disk: the complete payload for a full record, or the first
// threshold bytes (<= 4 KB) for a trimmed one.
func BuildRecord(sessionID string, ts time.Time, data []byte, srcIPUsed string, trimThreshold int) (Record, error) {
	d, err := decode(data)
	if err != nil {
		return Record{}, err
	}
	return buildFromDecoded(sessionID, ts, d, srcIPUsed, trimThreshold), nil
}

// buildFromDecoded assembles a Record from already-decoded fields. The live
// capture loop decodes once (to resolve the source IP from the destination)
// and then calls this, avoiding a second parse.
func buildFromDecoded(sessionID string, ts time.Time, d decoded, srcIPUsed string, trimThreshold int) Record {
	if trimThreshold <= 0 {
		trimThreshold = DefaultTrimThreshold
	}
	if trimThreshold > MaxPayloadHead {
		trimThreshold = MaxPayloadHead
	}

	sum := sha256.Sum256(d.payload)
	rec := Record{
		SessionID:        sessionID,
		Timestamp:        ts,
		Protocol:         d.protocol,
		Src:              endpoint(d.srcIP, d.srcPort),
		Dst:              endpoint(d.dstIP, d.dstPort),
		Flags:            d.flags,
		Seq:              d.seq,
		Ack:              d.ack,
		Window:           d.window,
		PayloadBytesFull: len(d.payload),
		PayloadSHA256:    hex.EncodeToString(sum[:]),
		SrcIPUsed:        srcIPUsed,
	}
	if len(d.payload) > trimThreshold {
		rec.PayloadStatus = PayloadTrimmed
		head := make([]byte, trimThreshold)
		copy(head, d.payload[:trimThreshold])
		rec.PayloadHead = head
	} else {
		// A full record retains its complete payload, not only its SHA-256.
		rec.PayloadStatus = PayloadFull
		if len(d.payload) > 0 {
			head := make([]byte, len(d.payload))
			copy(head, d.payload)
			rec.PayloadHead = head
		}
	}
	return rec
}
