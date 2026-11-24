package evidence

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
)

// TestExportPCAPNGFullPacketCarriesRealPayload builds a record the way capture
// does (capture.BuildRecord from a real packet), archives it, and checks the
// exported pcapng packet carries the actual payload bytes, not a zero fill.
func TestExportPCAPNGFullPacketCarriesRealPayload(t *testing.T) {
	payload := []byte("POST /login HTTP/1.1\r\nHost: target.example\r\n\r\nuser=alice")
	ip := &layers.IPv4{Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolTCP,
		SrcIP: []byte{10, 0, 0, 2}, DstIP: []byte{198, 51, 100, 7}}
	tcp := &layers.TCP{SrcPort: 40000, DstPort: 80, PSH: true, ACK: true, Seq: 11, Ack: 22, Window: 500}
	if err := tcp.SetNetworkLayerForChecksum(ip); err != nil {
		t.Fatal(err)
	}
	buf := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buf, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true},
		ip, tcp, gopacket.Payload(payload)); err != nil {
		t.Fatal(err)
	}
	rec, err := capture.BuildRecord("S-full", time.Now(), buf.Bytes(), "203.0.113.9", 0)
	if err != nil {
		t.Fatal(err)
	}
	if rec.PayloadStatus != capture.PayloadFull || !bytes.Equal(rec.PayloadHead, payload) {
		t.Fatalf("record status=%q head=%q, want full with the complete payload", rec.PayloadStatus, rec.PayloadHead)
	}

	a, err := OpenArchive(t.TempDir(), ArchiveOptions{DiskUsage: func(string) (float64, error) { return 1, nil }})
	if err != nil {
		t.Fatal(err)
	}
	archiveSegment(t, a, "m1", "seg-1.ndjson", []capture.Record{rec})

	var out bytes.Buffer
	n, err := ExportPCAPNG(&out, a, Selector{Machine: "m1", Session: "S-full"})
	if err != nil || n != 1 {
		t.Fatalf("export n=%d err=%v", n, err)
	}
	pkts, _ := readPCAPNG(t, out.Bytes())
	if len(pkts) != 1 {
		t.Fatalf("got %d packets, want 1", len(pkts))
	}
	pkt := gopacket.NewPacket(pkts[0].data, layers.LayerTypeIPv4, gopacket.Default)
	got, _ := pkt.Layer(layers.LayerTypeTCP).(*layers.TCP)
	if got == nil {
		t.Fatalf("exported packet does not decode as TCP: %v", pkt)
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Errorf("exported payload = %q, want %q", got.Payload, payload)
	}
	if pkts[0].ci.CaptureLength != pkts[0].ci.Length {
		t.Errorf("full packet captured %d of %d bytes, want all", pkts[0].ci.CaptureLength, pkts[0].ci.Length)
	}
}
