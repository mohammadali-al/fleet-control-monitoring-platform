package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet/fleettest"
)

type captureControllerAdapter struct {
	c *capture.Controller
}

func (a captureControllerAdapter) Capture(sessionID, cgroupPath string) (agent.SessionCapture, error) {
	return a.c.Capture(sessionID, cgroupPath)
}

func (a captureControllerAdapter) Close() { a.c.Close() }

func (a captureControllerAdapter) PublicIPCache() *capture.PublicIPCache {
	return a.c.PublicIPCache()
}

func TestAgentReportsPublicIPOnConnectAndRefresh(t *testing.T) {
	var mu sync.Mutex
	currentIP := "203.0.113.9"
	mkReflector := func() *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			ip := currentIP
			mu.Unlock()
			_, _ = w.Write([]byte(ip))
		}))
		t.Cleanup(s.Close)
		return s
	}
	r1, r2 := mkReflector(), mkReflector()

	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	startAgent(t, mt, "w1", func(c *agent.Config) {
		c.Capture = config.CaptureConfig{Reflectors: []string{r1.URL, r2.URL}}
		c.CaptureFactory = func(ctx context.Context, cfg capture.Config) (agent.Capturer, error) {
			ctrl, err := capture.NewController(cfg)
			if err != nil {
				return nil, err
			}
			ctrl.Start(ctx)
			return captureControllerAdapter{ctrl}, nil
		}
	})

	rs := dialAgent(t, mt, "w1")
	rs.expect(channel.TypeConfigReq, "")
	rs.send(channel.TypeConfig, "config", channel.ConfigPayload{GateSecret: "sec"})
	var ack channel.ConfigAckPayload
	_ = json.Unmarshal(rs.expect(channel.TypeConfigAck, "").Payload, &ack)
	if !ack.OK {
		t.Fatalf("config ack = %+v", ack)
	}

	// Boot reflector resolution produces TypePublicIP.
	env := rs.expect(channel.TypePublicIP, "")
	var pip channel.PublicIPPayload
	if err := json.Unmarshal(env.Payload, &pip); err != nil {
		t.Fatalf("unmarshal TypePublicIP: %v", err)
	}
	if pip.IP != "203.0.113.9" || !pip.Confirmed || pip.At.IsZero() {
		t.Fatalf("boot public IP = %+v, want 203.0.113.9 confirmed", pip)
	}
	firstAt := pip.At

	// Manual refresh with same IP still emits a fresh TypePublicIP with a newer timestamp.
	time.Sleep(5 * time.Millisecond)
	rs.send(channel.TypePublicIPRefresh, "", nil)
	env = rs.expect(channel.TypePublicIP, "")
	_ = json.Unmarshal(env.Payload, &pip)
	if pip.IP != "203.0.113.9" || !pip.At.After(firstAt) {
		t.Fatalf("same-IP refresh = %+v (firstAt=%v)", pip, firstAt)
	}

	// Change reflector IP and trigger TypePublicIPRefresh -> reports new IP.
	mu.Lock()
	currentIP = "198.51.100.42"
	mu.Unlock()
	rs.send(channel.TypePublicIPRefresh, "", nil)
	env = rs.expect(channel.TypePublicIP, "")
	_ = json.Unmarshal(env.Payload, &pip)
	if pip.IP != "198.51.100.42" || !pip.Confirmed {
		t.Fatalf("changed-IP refresh = %+v, want 198.51.100.42 confirmed", pip)
	}

	// Unknown envelope type is ignored cleanly (backward-tolerant).
	rs.send("future-unknown-type", "", map[string]string{"k": "v"})

	// Reconnect -> agent reports cached IP on connect.
	rs.stop()
	rs2 := dialAgent(t, mt, "w1")
	rs2.expect(channel.TypeConfigReq, "")
	env = rs2.expect(channel.TypePublicIP, "")
	_ = json.Unmarshal(env.Payload, &pip)
	if pip.IP != "198.51.100.42" {
		t.Fatalf("reconnect public IP = %+v, want 198.51.100.42", pip)
	}
}
