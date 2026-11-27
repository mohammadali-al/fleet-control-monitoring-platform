package panel

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
)

func synthRecordWithPayload(t *testing.T, session string, ts time.Time, i int, payload []byte, trimThreshold int) capture.Record {
	t.Helper()
	ip := &layers.IPv4{Version: 4, IHL: 5, TTL: 64, Protocol: layers.IPProtocolTCP,
		SrcIP: net.IPv4(10, 0, 0, 5), DstIP: net.IPv4(192, 0, 2, byte(1+i))}
	tcp := &layers.TCP{SrcPort: layers.TCPPort(40000 + i), DstPort: 443, Seq: uint32(1000 + i), Ack: uint32(i), Window: 65535, PSH: true, ACK: true, DataOffset: 5}
	if err := tcp.SetNetworkLayerForChecksum(ip); err != nil {
		t.Fatal(err)
	}
	buf := gopacket.NewSerializeBuffer()
	if err := gopacket.SerializeLayers(buf, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true},
		ip, tcp, gopacket.Payload(payload)); err != nil {
		t.Fatal(err)
	}
	rec, err := capture.BuildRecord(session, ts, buf.Bytes(), "10.0.0.5", trimThreshold)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func archiveRecords(t *testing.T, e *obsEnv, machine, segFile string, recs []capture.Record) {
	t.Helper()
	var data []byte
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		data = append(append(data, b...), '\n')
	}
	part := filepath.Join(t.TempDir(), segFile+".part")
	if err := os.WriteFile(part, data, 0o640); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	entry := capture.SegmentEntry{
		File: segFile, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(data)),
		Records: len(recs), StartTime: recs[0].Timestamp, EndTime: recs[len(recs)-1].Timestamp,
		SchemaVersion: capture.SchemaVersion, Compression: capture.CompressionNone,
	}
	if _, err := e.obs.Archive.Commit(machine, entry, part); err != nil {
		t.Fatalf("Archive.Commit: %v", err)
	}
}

// GAP-34: evRecords must include payload_head (both full and trimmed payloads),
// and evSessions must count only the records belonging to each session when a
// segment is shared across sessions.
func TestEvidenceRecordsIncludePayloadHead(t *testing.T) {
	e := newObsEnv(t, nil)
	t0 := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)

	fullPayload := []byte("GET /health HTTP/1.1\r\n\r\n")
	longPayload := bytes.Repeat([]byte("A"), 128)
	recs := []capture.Record{
		synthRecordWithPayload(t, "s-head", t0, 0, fullPayload, 64),
		synthRecordWithPayload(t, "s-head", t0.Add(time.Millisecond), 1, longPayload, 64),
	}
	archiveRecords(t, e, "w1", "seg-0001.ndjson", recs)

	code, _, raw := e.raw("/api/evidence/sessions/w1/s-head/records")
	if code != 200 {
		t.Fatalf("records status = %d: %s", code, raw)
	}
	var body struct {
		Records []evRecord `json:"records"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode records: %v (%s)", err, raw)
	}
	if len(body.Records) != 2 {
		t.Fatalf("len(records) = %d, want 2", len(body.Records))
	}
	if !bytes.Equal(body.Records[0].PayloadHead, fullPayload) {
		t.Errorf("full record payload_head = %q, want %q", body.Records[0].PayloadHead, fullPayload)
	}
	if body.Records[1].PayloadStatus != capture.PayloadTrimmed || body.Records[1].PayloadBytes != 128 {
		t.Errorf("trimmed record status=%q bytes=%d", body.Records[1].PayloadStatus, body.Records[1].PayloadBytes)
	}
	if !bytes.Equal(body.Records[1].PayloadHead, longPayload[:64]) {
		t.Errorf("trimmed record payload_head len = %d, want 64 prefix", len(body.Records[1].PayloadHead))
	}

	// Verify the JSON wire key is literally "payload_head" (base64).
	var rawDoc struct {
		Records []map[string]any `json:"records"`
	}
	if err := json.Unmarshal(raw, &rawDoc); err != nil {
		t.Fatal(err)
	}
	gotB64, ok := rawDoc.Records[0]["payload_head"].(string)
	if !ok || gotB64 != base64.StdEncoding.EncodeToString(fullPayload) {
		t.Errorf("raw JSON payload_head = %#v, want base64 %q", rawDoc.Records[0]["payload_head"], base64.StdEncoding.EncodeToString(fullPayload))
	}
}

func TestEvidenceSharedSegmentExactSessionCounts(t *testing.T) {
	e := newObsEnv(t, nil)
	t0 := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)

	// One segment shared between s-alpha (3 records) and s-beta (2 records),
	// plus a dedicated segment for s-alpha (1 record).
	shared := []capture.Record{
		synthRecord(t, "s-alpha", t0, 0),
		synthRecord(t, "s-beta", t0.Add(time.Millisecond), 1),
		synthRecord(t, "s-alpha", t0.Add(2*time.Millisecond), 2),
		synthRecord(t, "s-beta", t0.Add(3*time.Millisecond), 3),
		synthRecord(t, "s-alpha", t0.Add(4*time.Millisecond), 4),
	}
	archiveRecords(t, e, "w1", "seg-shared.ndjson", shared)
	archiveRecords(t, e, "w1", "seg-alpha.ndjson", []capture.Record{
		synthRecord(t, "s-alpha", t0.Add(time.Second), 5),
	})

	var resp struct {
		Sessions []evSession `json:"sessions"`
	}
	if code, _ := e.req("GET", "/api/evidence/sessions", nil, &resp); code != 200 {
		t.Fatalf("sessions status = %d", code)
	}
	byID := map[string]evSession{}
	for _, s := range resp.Sessions {
		byID[s.Session] = s
	}
	alpha, ok := byID["s-alpha"]
	if !ok {
		t.Fatalf("missing s-alpha in %+v", resp.Sessions)
	}
	if !alpha.Shared || alpha.Segments != 2 || alpha.Packets != 4 {
		t.Errorf("s-alpha = %+v, want shared=true segments=2 packets=4", alpha)
	}
	beta, ok := byID["s-beta"]
	if !ok {
		t.Fatalf("missing s-beta in %+v", resp.Sessions)
	}
	if !beta.Shared || beta.Segments != 1 || beta.Packets != 2 {
		t.Errorf("s-beta = %+v, want shared=true segments=1 packets=2", beta)
	}
}
