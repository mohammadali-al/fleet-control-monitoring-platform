package channel

import (
	"encoding/json"
	"testing"
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
