package channel

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEnvelopeJSON(t *testing.T) {
	env, err := NewEnvelope("test.thing", "id-1", map[string]int{"n": 7})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	var back Envelope
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	checkEnvelope(t, back, env)
	if string(back.Payload) != `{"n":7}` {
		t.Errorf("payload = %s", back.Payload)
	}
}

func TestEnvelopeNilPayloadOmitted(t *testing.T) {
	env, err := NewEnvelope(TypePing, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	if string(b) != `{"type":"ping"}` {
		t.Errorf("wire form = %s", b)
	}
}

func TestHelloID(t *testing.T) {
	env, _ := NewEnvelope(TypeHello, "", HelloPayload{MachineID: "m1"})
	if id, err := helloID(env); err != nil || id != "m1" {
		t.Errorf("helloID = %q, %v", id, err)
	}
	if _, err := helloID(Envelope{Type: TypeHello}); err == nil {
		t.Error("helloID accepted an empty payload")
	}
}

func TestPublicIPEnvelopeRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 27, 20, 0, 0, 0, time.UTC)
	want := PublicIPPayload{IP: "203.0.113.9", At: at, Confirmed: true}
	env, err := NewEnvelope(TypePublicIP, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope(TypePublicIP): %v", err)
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Envelope
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal envelope: %v", err)
	}
	checkEnvelope(t, back, env)
	var got PublicIPPayload
	if err := json.Unmarshal(back.Payload, &got); err != nil {
		t.Fatalf("Unmarshal payload: %v", err)
	}
	if got.IP != want.IP || !got.At.Equal(want.At) || got.Confirmed != want.Confirmed {
		t.Errorf("got payload %+v, want %+v", got, want)
	}

	req, err := NewEnvelope(TypePublicIPRefresh, "", nil)
	if err != nil {
		t.Fatalf("NewEnvelope(TypePublicIPRefresh): %v", err)
	}
	rb, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal refresh: %v", err)
	}
	if string(rb) != `{"type":"public-ip-refresh"}` {
		t.Errorf("refresh wire form = %s", rb)
	}
}

func TestManeuverEnvelopeRoundTrip(t *testing.T) {
	t0 := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	prep := ManeuverPreparePayload{
		RunID:        "m-abc",
		ManeuverID:   "web-probe",
		Name:         "Web probe",
		TargetIP:     "198.51.100.10",
		TargetOrigin: "https://shop.example.test",
		StartAt:      t0,
		TotalSec:     120,
		Pattern:      "all",
		Items: []ManeuverPlanItem{
			{ItemID: "item-1", BaselineID: "http-get", Name: "GET", Command: "hey -z 30s https://shop.example.test", ConfirmToken: "secret-tok", StartSec: 0, DurationSec: 30, Replicas: 2},
		},
	}
	if s := prep.Items[0].String(); s == "" || len(s) == 0 || contains(s, "secret-tok") {
		t.Fatalf("String() leaked confirm token or was empty: %q", s)
	}
	env, err := NewEnvelope(TypeManeuverPrepare, prep.RunID, prep)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	b, _ := json.Marshal(env)
	var back Envelope
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	checkEnvelope(t, back, env)
	var got ManeuverPreparePayload
	if err := json.Unmarshal(back.Payload, &got); err != nil {
		t.Fatal(err)
	}
	if got.RunID != prep.RunID || !got.StartAt.Equal(t0) || len(got.Items) != 1 || got.Items[0].Replicas != 2 {
		t.Fatalf("got = %+v", got)
	}
}

func TestAgentUpdateEnvelopesRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 28, 12, 30, 0, 0, time.UTC)

	verEnv, err := NewEnvelope(TypeAgentVersion, "", AgentVersionPayload{
		Version: "1.2.3",
		OS:      "linux",
		Arch:    "amd64",
		At:      at,
	})
	if err != nil {
		t.Fatalf("NewEnvelope(TypeAgentVersion): %v", err)
	}
	b, _ := json.Marshal(verEnv)
	var verBack Envelope
	if err := json.Unmarshal(b, &verBack); err != nil {
		t.Fatal(err)
	}
	checkEnvelope(t, verBack, verEnv)
	var verGot AgentVersionPayload
	if err := json.Unmarshal(verBack.Payload, &verGot); err != nil {
		t.Fatal(err)
	}
	if verGot.Version != "1.2.3" || verGot.OS != "linux" || verGot.Arch != "amd64" || !verGot.At.Equal(at) {
		t.Fatalf("AgentVersionPayload = %+v", verGot)
	}

	upd := AgentUpdatePayload{
		TargetVersion: "2.0.0",
		SHA256:        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Size:          12345,
		OS:            "linux",
		Arch:          "arm64",
		URL:           "https://server.test:7445/enroll/update/agent?os=linux&arch=arm64",
		Method:        "http",
	}
	updEnv, err := NewEnvelope(TypeAgentUpdate, "upd-1", upd)
	if err != nil {
		t.Fatalf("NewEnvelope(TypeAgentUpdate): %v", err)
	}
	ub, _ := json.Marshal(updEnv)
	var updBack Envelope
	if err := json.Unmarshal(ub, &updBack); err != nil {
		t.Fatal(err)
	}
	checkEnvelope(t, updBack, updEnv)
	var updGot AgentUpdatePayload
	if err := json.Unmarshal(updBack.Payload, &updGot); err != nil {
		t.Fatal(err)
	}
	if updGot != upd {
		t.Fatalf("AgentUpdatePayload = %+v, want %+v", updGot, upd)
	}

	for _, status := range []string{AgentUpdateDownloading, AgentUpdateApplying, AgentUpdateOK, AgentUpdateFailed} {
		res := AgentUpdateResultPayload{
			TargetVersion:  "2.0.0",
			CurrentVersion: "1.2.3",
			Status:         status,
			At:             at,
		}
		if status == AgentUpdateFailed {
			res.Error = "checksum mismatch"
		}
		resEnv, err := NewEnvelope(TypeAgentUpdateResult, "upd-1", res)
		if err != nil {
			t.Fatalf("NewEnvelope(TypeAgentUpdateResult %s): %v", status, err)
		}
		rb, _ := json.Marshal(resEnv)
		var resBack Envelope
		if err := json.Unmarshal(rb, &resBack); err != nil {
			t.Fatal(err)
		}
		checkEnvelope(t, resBack, resEnv)
		var resGot AgentUpdateResultPayload
		if err := json.Unmarshal(resBack.Payload, &resGot); err != nil {
			t.Fatal(err)
		}
		if resGot.Status != res.Status || resGot.TargetVersion != res.TargetVersion || resGot.Error != res.Error || !resGot.At.Equal(at) {
			t.Fatalf("AgentUpdateResultPayload = %+v, want %+v", resGot, res)
		}
	}

	reqEnv, err := NewEnvelope(TypeAgentUpdateChunkReq, "upd-1", AgentUpdateChunkReqPayload{OS: "linux", Arch: "amd64", Offset: 1024, Length: 4096})
	if err != nil {
		t.Fatal(err)
	}
	cb, _ := json.Marshal(reqEnv)
	var reqBack Envelope
	if err := json.Unmarshal(cb, &reqBack); err != nil {
		t.Fatal(err)
	}
	checkEnvelope(t, reqBack, reqEnv)

	chunkEnv, err := NewEnvelope(TypeAgentUpdateChunk, "upd-1", AgentUpdateChunkPayload{Offset: 1024, Total: 5120, Data: []byte("abc"), EOF: false})
	if err != nil {
		t.Fatal(err)
	}
	db, _ := json.Marshal(chunkEnv)
	var chunkBack Envelope
	if err := json.Unmarshal(db, &chunkBack); err != nil {
		t.Fatal(err)
	}
	checkEnvelope(t, chunkBack, chunkEnv)
}

func TestResourceStateEnvelopeRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 28, 15, 0, 0, 0, time.UTC)
	want := ResourceStatePayload{
		CPUPercent: 42.5,
		MemPercent: 68.2,
		LoadPerCPU: 0.75,
		EgressMbps: 12.4,
		At:         at,
	}
	env, err := NewEnvelope(TypeResourceState, "", want)
	if err != nil {
		t.Fatalf("NewEnvelope(TypeResourceState): %v", err)
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Envelope
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal envelope: %v", err)
	}
	checkEnvelope(t, back, env)
	var got ResourceStatePayload
	if err := json.Unmarshal(back.Payload, &got); err != nil {
		t.Fatalf("Unmarshal payload: %v", err)
	}
	if got.CPUPercent != want.CPUPercent || got.MemPercent != want.MemPercent ||
		got.LoadPerCPU != want.LoadPerCPU || got.EgressMbps != want.EgressMbps || !got.At.Equal(want.At) {
		t.Errorf("got payload %+v, want %+v", got, want)
	}

	// Zero EgressMbps is omitted on the wire; zero CPU/mem/load are preserved.
	zeroEnv, err := NewEnvelope(TypeResourceState, "", ResourceStatePayload{At: at})
	if err != nil {
		t.Fatalf("NewEnvelope(zero ResourceStatePayload): %v", err)
	}
	zb, err := json.Marshal(zeroEnv)
	if err != nil {
		t.Fatalf("Marshal zero: %v", err)
	}
	if contains(string(zb), "egress_mbps") {
		t.Errorf("expected egress_mbps to be omitted when zero, got %s", zb)
	}
	var zeroBack Envelope
	if err := json.Unmarshal(zb, &zeroBack); err != nil {
		t.Fatalf("Unmarshal zero envelope: %v", err)
	}
	var zeroGot ResourceStatePayload
	if err := json.Unmarshal(zeroBack.Payload, &zeroGot); err != nil {
		t.Fatalf("Unmarshal zero payload: %v", err)
	}
	if zeroGot.CPUPercent != 0 || zeroGot.MemPercent != 0 || zeroGot.LoadPerCPU != 0 || zeroGot.EgressMbps != 0 || !zeroGot.At.Equal(at) {
		t.Errorf("got zero payload %+v, want zeros at %v", zeroGot, at)
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && stringContains(s, sub)
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
