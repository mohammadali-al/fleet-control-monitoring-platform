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
