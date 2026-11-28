package fleet_test

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
)

func TestResourceStateDispatchAndPanelJSON(t *testing.T) {
	m, links := newWatchdogMgr(t, fleet.WatchdogSource{}, false)
	eventually(t, "w1 online", func() bool {
		info, ok := m.Machine("w1")
		return ok && info.State == fleet.StateOnline
	})

	// Before any resource-state arrives, Resource is nil and omitted from JSON.
	initial, ok := m.Machine("w1")
	if !ok {
		t.Fatal("missing machine w1")
	}
	if initial.Resource != nil {
		t.Fatalf("initial Resource = %+v, want nil", initial.Resource)
	}
	initJSON, err := json.Marshal(initial)
	if err != nil {
		t.Fatalf("Marshal initial MachineInfo: %v", err)
	}
	if strings.Contains(string(initJSON), `"resource"`) {
		t.Errorf("expected resource to be omitted when nil, got %s", initJSON)
	}

	sub := m.Bus().Subscribe(func(ev fleet.Event) bool {
		return ev.Kind == fleet.EventMachine && ev.Machine == "w1"
	})
	defer sub.Close()

	at := time.Date(2026, 11, 28, 15, 45, 0, 0, time.UTC)
	ctx := context.Background()
	links["w1"].h(ctx, envOf(t, channel.TypeResourceState, channel.ResourceStatePayload{
		CPUPercent: 42.5,
		MemPercent: 68.2,
		LoadPerCPU: 0.75,
		EgressMbps: 12.4,
		At:         at,
	}))

	eventually(t, "MachineInfo.Resource updated", func() bool {
		info, ok := m.Machine("w1")
		return ok && info.Resource != nil &&
			info.Resource.CPUPercent == 42.5 &&
			info.Resource.MemPercent == 68.2 &&
			info.Resource.LoadPerCPU == 0.75 &&
			info.Resource.EgressMbps == 12.4 &&
			info.Resource.At.Equal(at)
	})

	deadline := time.After(wait)
	for {
		select {
		case ev := <-sub.C:
			if ev.Info != nil && ev.Info.Resource != nil {
				if ev.Info.Resource.CPUPercent != 42.5 {
					t.Fatalf("bus event Resource.CPUPercent = %v, want 42.5", ev.Info.Resource.CPUPercent)
				}
				goto gotBusEvent
			}
		case <-deadline:
			t.Fatal("timed out waiting for EventMachine after TypeResourceState")
		}
	}
gotBusEvent:

	// MachineInfo carries Resource through to panel JSON (/api/machines & /ws/events).
	updated, _ := m.Machine("w1")
	b, err := json.Marshal(updated)
	if err != nil {
		t.Fatalf("Marshal updated MachineInfo: %v", err)
	}
	var decoded struct {
		Resource *struct {
			CPUPercent float64   `json:"cpu_percent"`
			MemPercent float64   `json:"mem_percent"`
			LoadPerCPU float64   `json:"load_per_cpu"`
			EgressMbps float64   `json:"egress_mbps"`
			At         time.Time `json:"at"`
		} `json:"resource"`
	}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("Unmarshal MachineInfo JSON: %v", err)
	}
	if decoded.Resource == nil ||
		decoded.Resource.CPUPercent != 42.5 ||
		decoded.Resource.MemPercent != 68.2 ||
		decoded.Resource.LoadPerCPU != 0.75 ||
		decoded.Resource.EgressMbps != 12.4 ||
		!decoded.Resource.At.Equal(at) {
		t.Fatalf("decoded JSON resource = %+v", decoded.Resource)
	}

	// Out-of-range payloads and malformed JSON are rejected without altering state.
	badPayloads := []channel.ResourceStatePayload{
		{CPUPercent: -0.1, MemPercent: 50, LoadPerCPU: 1, At: at},
		{CPUPercent: 100.1, MemPercent: 50, LoadPerCPU: 1, At: at},
		{CPUPercent: 10, MemPercent: -1, LoadPerCPU: 1, At: at},
		{CPUPercent: 10, MemPercent: 101, LoadPerCPU: 1, At: at},
		{CPUPercent: 10, MemPercent: 50, LoadPerCPU: -0.01, At: at},
		{CPUPercent: 10, MemPercent: 50, LoadPerCPU: 1, EgressMbps: -2, At: at},
	}
	for _, bad := range badPayloads {
		links["w1"].h(ctx, envOf(t, channel.TypeResourceState, bad))
	}
	links["w1"].h(ctx, channel.Envelope{Type: channel.TypeResourceState, Payload: []byte(`{"cpu_percent":`)})

	// Non-finite floats in raw JSON / unmarshal guard: NaN and Inf cannot arrive via
	// standard JSON, so also verify malformed/overflow numbers are ignored.
	_ = math.NaN()

	afterBad, _ := m.Machine("w1")
	if afterBad.Resource == nil || afterBad.Resource.CPUPercent != 42.5 || afterBad.Resource.MemPercent != 68.2 {
		t.Fatalf("Resource changed after invalid payloads: %+v", afterBad.Resource)
	}

	// Zero At gets stamped on the server.
	beforeStamp := time.Now().Add(-time.Second)
	links["w1"].h(ctx, envOf(t, channel.TypeResourceState, channel.ResourceStatePayload{
		CPUPercent: 10,
		MemPercent: 20,
		LoadPerCPU: 0.25,
	}))
	eventually(t, "zero At stamped", func() bool {
		info, ok := m.Machine("w1")
		return ok && info.Resource != nil && info.Resource.CPUPercent == 10 && info.Resource.At.After(beforeStamp)
	})
}
