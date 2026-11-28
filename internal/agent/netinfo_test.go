package agent_test

import (
	"encoding/json"
	"testing"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet/fleettest"
)

func TestAgentReportsPrimaryPrivateIPOnConnect(t *testing.T) {
	restore := agent.SetPrimaryIPFunc(func() (string, error) { return "10.0.0.5", nil })
	defer restore()

	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	startAgent(t, mt, "w1", nil)
	rs := dialAgent(t, mt, "w1")
	rs.expect(channel.TypeConfigReq, "")
	var p channel.NetInfoPayload
	if err := json.Unmarshal(rs.expect(channel.TypeNetInfo, "").Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.PrimaryPrivateIP != "10.0.0.5" || p.At.IsZero() {
		t.Fatalf("net-info = %+v", p)
	}
}
