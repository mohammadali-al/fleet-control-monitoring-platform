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
