package evidence

import (
	"bytes"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcapgo"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
)

// archiveSegment files a synthetic segment straight into the archive (the sync
// path is covered by sync tests) and returns its records.
func archiveSegment(t *testing.T, a *Archive, machine, name string, recs []capture.Record) {
	t.Helper()
	tmp := t.TempDir()
	e := writeSegment(t, tmp, machine, name, recs)
	part, err := a.partialPath(machine, e)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(part), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part, mustRead(t, filepath.Join(tmp, name)), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Commit(machine, e, part); err != nil {
		t.Fatal(err)
	}
}

func seq(t *testing.T, session string, t0 time.Time, n, payload int) []capture.Record {
	t.Helper()
	out := make([]capture.Record, n)
	for i := range out {
		out[i] = synthRecord(t, session, t0.Add(time.Duration(i)*time.Second), i, payload)
	}
	return out
}

// exportFixture archives: m1/S1 (5 trimmed records), m1/S2 (3), m2/S3 (4).
func exportFixture(t *testing.T) (*Archive, map[string][]capture.Record) {
	t.Helper()
	a, err := OpenArchive(t.TempDir(), ArchiveOptions{DiskUsage: func(string) (float64, error) { return 1, nil }})
	if err != nil {
		t.Fatal(err)
	}
	recs := map[string][]capture.Record{
		"S1": seq(t, "S1", t0, 5, 300),                     // payload > 64: trimmed
		"S2": seq(t, "S2", t0.Add(time.Minute), 3, 30),     // payload <= 64: full
		"S3": seq(t, "S3", t0.Add(30*time.Second), 4, 100), // other machine, interleaved in time
	}
	archiveSegment(t, a, "m1", "seg-1.ndjson", recs["S1"])
	archiveSegment(t, a, "m1", "seg-2.ndjson", recs["S2"])
	archiveSegment(t, a, "m2", "seg-1.ndjson", recs["S3"])
	return a, recs
}

type pcapPacket struct {
	data []byte
	ci   gopacket.CaptureInfo
}

func readPCAPNG(t *testing.T, b []byte) (pkts []pcapPacket, ifaces int) {
	t.Helper()
	r, err := pcapgo.NewNgReader(bytes.NewReader(b), pcapgo.DefaultNgReaderOptions)
	if err != nil {
		t.Fatalf("not a readable pcapng: %v", err)
	}
	if lt := r.LinkType(); lt != layers.LinkTypeRaw {
		t.Errorf("link type %v, want RAW", lt)
	}
	for {
		data, ci, err := r.ReadPacketData()
		if err != nil {
			break
		}
		pkts = append(pkts, pcapPacket{append([]byte(nil), data...), ci})
	}
	return pkts, r.NInterfaces()
}

func TestExportPCAPNGIsValidAndMatchesRecords(t *testing.T) {
	a, recs := exportFixture(t)
	var buf bytes.Buffer
	n, err := ExportPCAPNG(&buf, a, Selector{Machine: "m1", Session: "S1"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("exported %d packets, want 5", n)
	}
	pkts, _ := readPCAPNG(t, buf.Bytes())
	if len(pkts) != 5 {
		t.Fatalf("reader found %d packets, want 5", len(pkts))
	}
	for i, p := range pkts {
		want := recs["S1"][i]
		pkt := gopacket.NewPacket(p.data, layers.LayerTypeIPv4, gopacket.Default)
		ip, _ := pkt.Layer(layers.LayerTypeIPv4).(*layers.IPv4)
		tcp, _ := pkt.Layer(layers.LayerTypeTCP).(*layers.TCP)
		if ip == nil || tcp == nil {
			t.Fatalf("packet %d does not decode as IPv4/TCP: %v", i, pkt)
		}
		if got := hostPort(ip.SrcIP.String(), int(tcp.SrcPort)); got != want.Src {
			t.Errorf("packet %d src %s, want %s", i, got, want.Src)
		}
		if got := hostPort(ip.DstIP.String(), int(tcp.DstPort)); got != want.Dst {
			t.Errorf("packet %d dst %s, want %s", i, got, want.Dst)
		}
		if tcp.Seq != want.Seq || tcp.Ack != want.Ack || tcp.Window != want.Window || !tcp.PSH || !tcp.ACK || tcp.SYN {
			t.Errorf("packet %d tcp fields wrong: %+v", i, tcp)
		}
		if !p.ci.Timestamp.Equal(want.Timestamp) {
			t.Errorf("packet %d time %v, want %v", i, p.ci.Timestamp, want.Timestamp)
		}
		// Trimmed: the original length is intact, the capture holds headers + head.
		if p.ci.Length != 20+20+want.PayloadBytesFull {
			t.Errorf("packet %d original length %d, want %d", i, p.ci.Length, 20+20+want.PayloadBytesFull)
		}
		if p.ci.CaptureLength != 40+len(want.PayloadHead) || !bytes.Equal(tcp.Payload, want.PayloadHead) {
			t.Errorf("packet %d captured %d bytes / payload %d, want head of %d", i, p.ci.CaptureLength, len(tcp.Payload), len(want.PayloadHead))
		}
		if int(ip.Length) != p.ci.Length {
			t.Errorf("packet %d ip length field %d != original %d", i, ip.Length, p.ci.Length)
		}
	}
}

func hostPort(host string, port int) string {
	if port < 0 {
		return host
	}
	if len(host) > 0 && host[0] != '[' && bytes.IndexByte([]byte(host), ':') >= 0 {
		return "[" + host + "]:" + strconv.Itoa(port)
	}
	return host + ":" + strconv.Itoa(port)
}

func TestExportPCAPNGAllProtocolsAndFullPayload(t *testing.T) {
	a, err := OpenArchive(t.TempDir(), ArchiveOptions{DiskUsage: func(string) (float64, error) { return 1, nil }})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("GET / HTTP/1.1\r\nHost: example.test\r\n\r\n")
	recs := []capture.Record{
		{SessionID: "S", Timestamp: t0, Protocol: "TCP", Src: "10.0.0.5:40000", Dst: "192.0.2.9:80", Flags: "SYN",
			Seq: 7, Window: 1000, PayloadStatus: "full", PayloadSHA256: "x", SrcIPUsed: "10.0.0.5"},
		{SessionID: "S", Timestamp: t0.Add(time.Millisecond), Protocol: "TCP", Src: "10.0.0.5:40000", Dst: "192.0.2.9:80",
			Flags: "PSH,ACK", Seq: 8, Ack: 1, Window: 1000, PayloadStatus: "full", PayloadBytesFull: len(payload),
			PayloadHead: payload, PayloadSHA256: "y", SrcIPUsed: "10.0.0.5"},
		{SessionID: "S", Timestamp: t0.Add(2 * time.Millisecond), Protocol: "UDP", Src: "10.0.0.5:5353", Dst: "192.0.2.53:53",
			PayloadStatus: "full", PayloadBytesFull: 4, PayloadHead: []byte{1, 2, 3, 4}, PayloadSHA256: "z", SrcIPUsed: "10.0.0.5"},
		{SessionID: "S", Timestamp: t0.Add(3 * time.Millisecond), Protocol: "ICMP", Src: "10.0.0.5", Dst: "192.0.2.1",
			Flags: "8/0", PayloadStatus: "full", PayloadSHA256: "w", SrcIPUsed: "10.0.0.5"},
		{SessionID: "S", Timestamp: t0.Add(4 * time.Millisecond), Protocol: "UDP", Src: "[2001:db8::5]:5000", Dst: "[2001:db8::9]:6000",
			PayloadStatus: "full", PayloadBytesFull: 2, PayloadHead: []byte{9, 9}, PayloadSHA256: "v", SrcIPUsed: "2001:db8::5"},
		{SessionID: "S", Timestamp: t0.Add(5 * time.Millisecond), Protocol: "ICMP", Src: "2001:db8::5", Dst: "2001:db8::9",
			Flags: "128/0", PayloadStatus: "full", PayloadSHA256: "u", SrcIPUsed: "2001:db8::5"},
	}
	archiveSegment(t, a, "m1", "seg-1.ndjson", recs)

	var buf bytes.Buffer
	if n, err := ExportPCAPNG(&buf, a, Selector{}); err != nil || n != len(recs) {
		t.Fatalf("export: n=%d err=%v", n, err)
	}
	if out := os.Getenv("FLEET_PCAPNG_OUT"); out != "" { // lets a human open it in tshark/Wireshark
		if err := os.WriteFile(out, buf.Bytes(), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	pkts, _ := readPCAPNG(t, buf.Bytes())
	if len(pkts) != len(recs) {
		t.Fatalf("%d packets, want %d", len(pkts), len(recs))
	}
	first := gopacket.NewPacket(pkts[0].data, layers.LayerTypeIPv4, gopacket.Default)
	if tcp, _ := first.Layer(layers.LayerTypeTCP).(*layers.TCP); tcp == nil || !tcp.SYN || tcp.ACK || tcp.Seq != 7 {
		t.Errorf("SYN packet wrong: %v", first)
	}
	data := gopacket.NewPacket(pkts[1].data, layers.LayerTypeIPv4, gopacket.Default)
	tcp, _ := data.Layer(layers.LayerTypeTCP).(*layers.TCP)
	if tcp == nil || !bytes.Equal(tcp.Payload, payload) || pkts[1].ci.CaptureLength != pkts[1].ci.Length {
		t.Errorf("full-payload packet not reconstructed intact: %v", data)
	}
	if udp, _ := gopacket.NewPacket(pkts[2].data, layers.LayerTypeIPv4, gopacket.Default).Layer(layers.LayerTypeUDP).(*layers.UDP); udp == nil || udp.DstPort != 53 || !bytes.Equal(udp.Payload, []byte{1, 2, 3, 4}) {
		t.Errorf("UDP packet wrong")
	}
	if ic, _ := gopacket.NewPacket(pkts[3].data, layers.LayerTypeIPv4, gopacket.Default).Layer(layers.LayerTypeICMPv4).(*layers.ICMPv4); ic == nil || ic.TypeCode.Type() != 8 {
		t.Errorf("ICMPv4 packet wrong")
	}
	if udp, _ := gopacket.NewPacket(pkts[4].data, layers.LayerTypeIPv6, gopacket.Default).Layer(layers.LayerTypeUDP).(*layers.UDP); udp == nil || udp.SrcPort != 5000 {
		t.Errorf("IPv6 UDP packet wrong")
	}
	if ic, _ := gopacket.NewPacket(pkts[5].data, layers.LayerTypeIPv6, gopacket.Default).Layer(layers.LayerTypeICMPv6).(*layers.ICMPv6); ic == nil || ic.TypeCode.Type() != 128 {
		t.Errorf("ICMPv6 packet wrong")
	}
}

func TestExportEmptySelectionIsStillValid(t *testing.T) {
	a, _ := exportFixture(t)
	sel := Selector{Session: "nope"}
	var pb, cb, jb bytes.Buffer
	if n, err := ExportPCAPNG(&pb, a, sel); err != nil || n != 0 {
		t.Fatalf("pcapng: n=%d err=%v", n, err)
	}
	if pkts, _ := readPCAPNG(t, pb.Bytes()); len(pkts) != 0 {
		t.Errorf("empty pcapng has packets")
	}
	if n, err := ExportCSV(&cb, a, sel); err != nil || n != 0 {
		t.Fatalf("csv: n=%d err=%v", n, err)
	}
	rows, err := csv.NewReader(&cb).ReadAll()
	if err != nil || len(rows) != 1 {
		t.Errorf("empty csv should be header only: %v %v", rows, err)
	}
	if n, err := ExportJSON(&jb, a, sel); err != nil || n != 0 {
		t.Fatalf("json: n=%d err=%v", n, err)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(jb.Bytes(), &arr); err != nil || len(arr) != 0 {
		t.Errorf("empty json should be []: %q %v", jb.String(), err)
	}
}

func TestExportScopeSelectors(t *testing.T) {
	a, _ := exportFixture(t)
	count := func(sel Selector) int {
		t.Helper()
		var b bytes.Buffer
		n, err := ExportCSV(&b, a, sel)
		if err != nil {
			t.Fatal(err)
		}
		var pb bytes.Buffer
		pn, err := ExportPCAPNG(&pb, a, sel)
		if err != nil || pn != n {
			t.Fatalf("pcapng exported %d vs csv %d (err %v)", pn, n, err)
		}
		pkts, _ := readPCAPNG(t, pb.Bytes())
		if len(pkts) != n {
			t.Fatalf("pcapng reader sees %d packets, csv has %d", len(pkts), n)
		}
		return n
	}
	cases := []struct {
		name string
		sel  Selector
		want int
	}{
		{"everything", Selector{}, 12},
		{"machine m1", Selector{Machine: "m1"}, 8},
		{"machine m2", Selector{Machine: "m2"}, 4},
		{"session S2", Selector{Session: "S2"}, 3},
		{"machine+session mismatch", Selector{Machine: "m2", Session: "S1"}, 0},
		// S1 = t0+0..4s, S3 = t0+30..33s, S2 = t0+60..62s
		{"range From", Selector{From: t0.Add(30 * time.Second)}, 7},
		{"range To (exclusive)", Selector{To: t0.Add(2 * time.Second)}, 2},
		{"range inside S1", Selector{From: t0.Add(1 * time.Second), To: t0.Add(4 * time.Second)}, 3},
		{"range across machines", Selector{From: t0.Add(3 * time.Second), To: t0.Add(32 * time.Second)}, 4},
		{"range + machine", Selector{Machine: "m2", From: t0.Add(31 * time.Second), To: t0.Add(33 * time.Second)}, 2},
		{"range outside", Selector{From: t0.Add(time.Hour)}, 0},
	}
	for _, c := range cases {
		if got := count(c.sel); got != c.want {
			t.Errorf("%s: %d records, want %d", c.name, got, c.want)
		}
	}
}

func TestExportCSVAndJSONMatchRecords(t *testing.T) {
	a, recs := exportFixture(t)
	sel := Selector{Machine: "m1", Session: "S1"}
	want := recs["S1"]

	var cb bytes.Buffer
	if n, err := ExportCSV(&cb, a, sel); err != nil || n != len(want) {
		t.Fatalf("csv: n=%d err=%v", n, err)
	}
	rows, err := csv.NewReader(&cb).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(want)+1 {
		t.Fatalf("csv has %d rows, want %d", len(rows), len(want)+1)
	}
	for i, h := range csvHeader {
		if rows[0][i] != h {
			t.Errorf("csv header[%d] = %q, want %q", i, rows[0][i], h)
		}
	}
	for i, r := range want {
		row := rows[i+1]
		ts, _ := time.Parse(time.RFC3339Nano, row[2])
		head, _ := base64.StdEncoding.DecodeString(row[13])
		ok := row[0] == "m1" && row[1] == r.SessionID && ts.Equal(r.Timestamp) && row[3] == r.Protocol &&
			row[4] == r.Src && row[5] == r.Dst && row[6] == r.Flags &&
			row[7] == strconv.FormatUint(uint64(r.Seq), 10) && row[8] == strconv.FormatUint(uint64(r.Ack), 10) &&
			row[9] == strconv.FormatUint(uint64(r.Window), 10) && row[10] == r.PayloadStatus &&
			row[11] == strconv.Itoa(r.PayloadBytesFull) && row[12] == r.PayloadSHA256 &&
			bytes.Equal(head, r.PayloadHead) && row[14] == r.SrcIPUsed
		if !ok {
			t.Errorf("csv row %d does not match record: %v", i, row)
		}
	}

	var jb bytes.Buffer
	if n, err := ExportJSON(&jb, a, sel); err != nil || n != len(want) {
		t.Fatalf("json: n=%d err=%v", n, err)
	}
	var got []struct {
		Machine string `json:"machine"`
		capture.Record
	}
	if err := json.Unmarshal(jb.Bytes(), &got); err != nil {
		t.Fatalf("json does not parse: %v\n%s", err, jb.String())
	}
	if len(got) != len(want) {
		t.Fatalf("json has %d records, want %d", len(got), len(want))
	}
	for i, r := range want {
		g := got[i]
		if g.Machine != "m1" || g.SessionID != r.SessionID || !g.Timestamp.Equal(r.Timestamp) || g.Src != r.Src ||
			g.Dst != r.Dst || g.Seq != r.Seq || g.PayloadSHA256 != r.PayloadSHA256 ||
			g.PayloadBytesFull != r.PayloadBytesFull || !bytes.Equal(g.PayloadHead, r.PayloadHead) {
			t.Errorf("json record %d does not match: %+v", i, g)
		}
	}
}
