package evidence

import (
	"bufio"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcapgo"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
)

// Selector chooses which archived records to export (§7.4): per session, per
// machine, per time range, or any combination. A zero field means "no limit".
// The time range is From <= timestamp < To.
type Selector struct {
	Machine string
	Session string
	From    time.Time
	To      time.Time
}

func (s Selector) matchTime(ts time.Time) bool {
	if !s.From.IsZero() && ts.Before(s.From) {
		return false
	}
	if !s.To.IsZero() && !ts.Before(s.To) {
		return false
	}
	return true
}

// overlaps reports whether a segment's time range can contain a match.
func (s Selector) overlaps(seg ArchivedSegment) bool {
	if !s.From.IsZero() && seg.EndTime.Before(s.From) {
		return false
	}
	if !s.To.IsZero() && !seg.StartTime.Before(s.To) {
		return false
	}
	return true
}

func (s Selector) hasSession(seg ArchivedSegment) bool {
	if s.Session == "" {
		return true
	}
	if len(seg.Sessions) == 0 {
		return seg.Session == s.Session
	}
	for _, id := range seg.Sessions {
		if id == s.Session {
			return true
		}
	}
	return false
}

type selected struct {
	machine string
	seg     ArchivedSegment
}

// walk calls fn for every archived record matching sel: segments ordered by
// start time (machine, then file, as tie-breakers), records in file order.
func (a *Archive) walk(sel Selector, fn func(machine string, rec capture.Record) error) error {
	machines := []string{sel.Machine}
	if sel.Machine == "" {
		var err error
		if machines, err = a.Machines(); err != nil {
			return err
		}
	}
	var segs []selected
	for _, m := range machines {
		list, err := a.Segments(m)
		if err != nil {
			return err
		}
		for _, s := range list {
			if sel.overlaps(s) && sel.hasSession(s) {
				segs = append(segs, selected{m, s})
			}
		}
	}
	sort.SliceStable(segs, func(i, j int) bool {
		x, y := segs[i], segs[j]
		if !x.seg.StartTime.Equal(y.seg.StartTime) {
			return x.seg.StartTime.Before(y.seg.StartTime)
		}
		if x.machine != y.machine {
			return x.machine < y.machine
		}
		return x.seg.File < y.seg.File
	})
	for _, s := range segs {
		if c := s.seg.Compression; c != "" && c != capture.CompressionNone {
			return fmt.Errorf("evidence: segment %s/%s uses unsupported compression %q", s.machine, s.seg.File, c)
		}
		f, err := a.OpenSegment(s.machine, s.seg)
		if err != nil {
			return fmt.Errorf("evidence: open %s/%s: %w", s.machine, s.seg.File, err)
		}
		err = readRecords(f, func(r capture.Record) error {
			if sel.Session != "" && r.SessionID != sel.Session {
				return nil
			}
			if !sel.matchTime(r.Timestamp) {
				return nil
			}
			return fn(s.machine, r)
		})
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// ---- CSV / JSON ----

// flatRecord is the JSON export row: the archived record plus its machine.
type flatRecord struct {
	Machine string `json:"machine"`
	capture.Record
}

var csvHeader = []string{
	"machine", "session_id", "timestamp", "protocol", "src", "dst", "flags",
	"seq", "ack", "window", "payload_status", "payload_bytes_full",
	"payload_sha256", "payload_head_b64", "src_ip_used",
}

// ExportCSV writes the selected records as a flat CSV (one row per record) and
// returns the row count. payload_head_b64 is the stored payload prefix.
func ExportCSV(w io.Writer, a *Archive, sel Selector) (int, error) {
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return 0, err
	}
	n := 0
	err := a.walk(sel, func(machine string, r capture.Record) error {
		n++
		return cw.Write([]string{
			machine, r.SessionID, r.Timestamp.UTC().Format(time.RFC3339Nano), r.Protocol, r.Src, r.Dst, r.Flags,
			strconv.FormatUint(uint64(r.Seq), 10), strconv.FormatUint(uint64(r.Ack), 10),
			strconv.FormatUint(uint64(r.Window), 10), r.PayloadStatus, strconv.Itoa(r.PayloadBytesFull),
			r.PayloadSHA256, base64.StdEncoding.EncodeToString(r.PayloadHead), r.SrcIPUsed,
		})
	})
	if err != nil {
		return n, err
	}
	cw.Flush()
	return n, cw.Error()
}

// ExportJSON writes the selected records as one JSON array of flat objects (the
// §6.3 fields plus "machine") and returns the record count. The array is
// streamed; an empty selection yields "[]".
func ExportJSON(w io.Writer, a *Archive, sel Selector) (int, error) {
	bw := bufio.NewWriter(w)
	if _, err := bw.WriteString("["); err != nil {
		return 0, err
	}
	n := 0
	err := a.walk(sel, func(machine string, r capture.Record) error {
		b, err := json.Marshal(flatRecord{Machine: machine, Record: r})
		if err != nil {
			return err
		}
		sep := "\n"
		if n > 0 {
			sep = ",\n"
		}
		n++
		if _, err := bw.WriteString(sep); err != nil {
			return err
		}
		_, err = bw.Write(b)
		return err
	})
	if err != nil {
		return n, err
	}
	if _, err := bw.WriteString("\n]\n"); err != nil {
		return n, err
	}
	return n, bw.Flush()
}

// ---- pcapng ----

// maxRegeneratedPacket bounds a regenerated packet (an IP packet cannot exceed
// 64 KiB); a larger recorded payload length means a corrupt record.
const maxRegeneratedPacket = 65535

// ExportPCAPNG regenerates the selected records as packets in a pcapng file
// (link type RAW: each packet starts at its IP header, as captured) and returns
// the packet count. One pcapng interface per machine.
//
// Payloads are reconstructed as far as stored: bytes past the stored prefix are
// not in the archive, so the packet's captured length is shorter than its
// original length (like a snap-length capture) and its transport checksum is
// left unset unless the whole payload is present. The record's payload SHA-256,
// which testifies to the original payload, is carried by the CSV/JSON export
// (pcapng packets have no per-packet field for it in this writer).
func ExportPCAPNG(w io.Writer, a *Archive, sel Selector) (int, error) {
	var (
		nw     *pcapgo.NgWriter
		ifaces = map[string]int{}
		n      int
	)
	intf := func(machine string) pcapgo.NgInterface {
		return pcapgo.NgInterface{
			Name:                machine,
			Comment:             "regenerated from the evidence archive; payload SHA-256 is in the CSV/JSON export",
			LinkType:            layers.LinkTypeRaw,
			TimestampResolution: 9,
		}
	}
	err := a.walk(sel, func(machine string, r capture.Record) error {
		data, origLen, err := regeneratePacket(r)
		if err != nil {
			return fmt.Errorf("evidence: regenerate packet (%s %s -> %s): %w", r.Protocol, r.Src, r.Dst, err)
		}
		if nw == nil {
			opts := pcapgo.DefaultNgWriterOptions
			opts.SectionInfo.Application = "fleet-control-monitoring-platform"
			if nw, err = pcapgo.NewNgWriterInterface(w, intf(machine), opts); err != nil {
				return err
			}
			ifaces[machine] = 0
		}
		idx, ok := ifaces[machine]
		if !ok {
			if idx, err = nw.AddInterface(intf(machine)); err != nil {
				return err
			}
			ifaces[machine] = idx
		}
		n++
		return nw.WritePacket(gopacket.CaptureInfo{
			Timestamp:      r.Timestamp,
			CaptureLength:  len(data),
			Length:         origLen,
			InterfaceIndex: idx,
		}, data)
	})
	if err != nil {
		return n, err
	}
	if nw == nil { // no records: still emit a valid, empty pcapng
		name := sel.Machine
		if name == "" {
			name = "fleet"
		}
		if nw, err = pcapgo.NewNgWriterInterface(w, intf(name), pcapgo.DefaultNgWriterOptions); err != nil {
			return 0, err
		}
	}
	return n, nw.Flush()
}

// regeneratePacket rebuilds one IP packet from a record. It returns the bytes
// to store (headers plus whatever payload prefix the record kept) and the
// original on-wire length.
func regeneratePacket(r capture.Record) (data []byte, origLen int, err error) {
	full := r.PayloadBytesFull
	if full < 0 || full > maxRegeneratedPacket {
		return nil, 0, fmt.Errorf("implausible payload length %d", full)
	}
	head := r.PayloadHead
	if len(head) > full {
		head = head[:full]
	}
	srcIP, srcPort, err := splitEndpoint(r.Src, r.Protocol)
	if err != nil {
		return nil, 0, err
	}
	dstIP, dstPort, err := splitEndpoint(r.Dst, r.Protocol)
	if err != nil {
		return nil, 0, err
	}
	v6 := srcIP.To4() == nil
	if v6 != (dstIP.To4() == nil) {
		return nil, 0, fmt.Errorf("mixed IP families %s / %s", srcIP, dstIP)
	}

	// The payload is laid out at its full length so every length field is
	// right; only the stored prefix is real, the rest is dropped afterwards.
	payload := make([]byte, full)
	copy(payload, head)
	complete := len(head) == full

	var (
		netL   gopacket.SerializableLayer
		netFor gopacket.NetworkLayer
		l4     []gopacket.SerializableLayer
		csum   func() error
	)
	proto := layers.IPProtocolTCP
	switch r.Protocol {
	case capture.ProtoUDP:
		proto = layers.IPProtocolUDP
	case capture.ProtoICMP:
		proto = layers.IPProtocolICMPv4
		if v6 {
			proto = layers.IPProtocolICMPv6
		}
	case capture.ProtoTCP:
	default:
		return nil, 0, fmt.Errorf("unknown protocol %q", r.Protocol)
	}
	if v6 {
		ip := &layers.IPv6{Version: 6, HopLimit: 64, NextHeader: proto, SrcIP: srcIP, DstIP: dstIP}
		netL, netFor = ip, ip
	} else {
		ip := &layers.IPv4{Version: 4, IHL: 5, TTL: 64, Protocol: proto, SrcIP: srcIP.To4(), DstIP: dstIP.To4()}
		netL, netFor = ip, ip
	}
	switch r.Protocol {
	case capture.ProtoTCP:
		t := &layers.TCP{SrcPort: layers.TCPPort(srcPort), DstPort: layers.TCPPort(dstPort),
			Seq: r.Seq, Ack: r.Ack, Window: r.Window, DataOffset: 5}
		setTCPFlags(t, r.Flags)
		csum = func() error { return t.SetNetworkLayerForChecksum(netFor) }
		l4 = append(l4, t)
	case capture.ProtoUDP:
		u := &layers.UDP{SrcPort: layers.UDPPort(srcPort), DstPort: layers.UDPPort(dstPort)}
		csum = func() error { return u.SetNetworkLayerForChecksum(netFor) }
		l4 = append(l4, u)
	case capture.ProtoICMP:
		typ, code := parseTypeCode(r.Flags)
		if v6 {
			ic := &layers.ICMPv6{TypeCode: layers.CreateICMPv6TypeCode(typ, code)}
			csum = func() error { return ic.SetNetworkLayerForChecksum(netFor) }
			l4 = append(l4, ic)
		} else {
			l4 = append(l4, &layers.ICMPv4{TypeCode: layers.CreateICMPv4TypeCode(typ, code)})
		}
	}
	if csum != nil {
		if err := csum(); err != nil {
			return nil, 0, err
		}
	}

	stack := append([]gopacket.SerializableLayer{netL}, l4...)
	stack = append(stack, gopacket.Payload(payload))
	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: complete}
	if err := gopacket.SerializeLayers(buf, opts, stack...); err != nil {
		return nil, 0, err
	}
	b := buf.Bytes()
	hdr := len(b) - full
	return b[:hdr+len(head)], len(b), nil
}

// splitEndpoint parses "ip:port" (TCP/UDP) or a bare IP (ICMP).
func splitEndpoint(s, proto string) (net.IP, int, error) {
	if proto == capture.ProtoICMP {
		ip := net.ParseIP(s)
		if ip == nil {
			return nil, 0, fmt.Errorf("bad address %q", s)
		}
		return ip, 0, nil
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return nil, 0, fmt.Errorf("bad endpoint %q: %w", s, err)
	}
	ip := net.ParseIP(host)
	p, perr := strconv.Atoi(port)
	if ip == nil || perr != nil || p < 0 || p > 65535 {
		return nil, 0, fmt.Errorf("bad endpoint %q", s)
	}
	return ip, p, nil
}

func setTCPFlags(t *layers.TCP, flags string) {
	for _, f := range strings.Split(flags, ",") {
		switch strings.TrimSpace(f) {
		case "FIN":
			t.FIN = true
		case "SYN":
			t.SYN = true
		case "RST":
			t.RST = true
		case "PSH":
			t.PSH = true
		case "ACK":
			t.ACK = true
		case "URG":
			t.URG = true
		case "ECE":
			t.ECE = true
		case "CWR":
			t.CWR = true
		case "NS":
			t.NS = true
		}
	}
}

// parseTypeCode reads the ICMP "type/code" the record stores in flags.
func parseTypeCode(s string) (typ, code uint8) {
	a, b, _ := strings.Cut(s, "/")
	t, _ := strconv.Atoi(a)
	c, _ := strconv.Atoi(b)
	return uint8(t), uint8(c)
}
