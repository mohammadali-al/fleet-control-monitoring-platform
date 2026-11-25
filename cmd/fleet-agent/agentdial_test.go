package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
)

// With connect_mode agent_dial the agent does not listen: it dials the main
// server's agent_dial listener (server_addr) as the machine its certificate
// names, in either role, and stops cleanly on cancel.
func TestAgentDialModeDialsTheServer(t *testing.T) {
	for _, role := range []string{config.RoleWorker, config.RoleMonitor} {
		t.Run(role, func(t *testing.T) {
			dir := t.TempDir()
			ca, err := pki.NewCA(pki.Options{})
			if err != nil {
				t.Fatal(err)
			}
			srvCert, srvKey, err := ca.IssuePeer(config.DefaultServerID, nil, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			srvTLS, err := pki.ServerTLSConfig(srvCert, srvKey, ca.CertPEM())
			if err != nil {
				t.Fatal(err)
			}
			attached := make(chan string, 4)
			acc, err := channel.Accept(channel.AcceptorConfig{
				Addr: "127.0.0.1:0", TLS: srvTLS, ServerID: config.DefaultServerID,
				Attach: func(id string, _ *channel.Conn) (channel.Handler, func(), error) {
					attached <- id
					return nil, nil, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			sctx, scancel := context.WithCancel(context.Background())
			defer scancel()
			go func() { _ = acc.Serve(sctx) }()

			certPEM, keyPEM, err := ca.IssuePeer("w1", nil, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			for name, data := range map[string][]byte{"ca.crt": ca.CertPEM(), "agent.crt": certPEM, "agent.key": keyPEM} {
				if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			doc, err := json.Marshal(map[string]any{"agent": config.AgentConfig{
				Role: role, MachineID: "w1", ConnectMode: config.ConnectModeAgentDial, ServerAddr: acc.Addr().String(),
				CAFile: filepath.Join(dir, "ca.crt"), CertFile: filepath.Join(dir, "agent.crt"), KeyFile: filepath.Join(dir, "agent.key"),
				DataDir: filepath.Join(dir, "data"),
			}})
			if err != nil {
				t.Fatal(err)
			}
			cfgPath := filepath.Join(dir, "config.json")
			if err := os.WriteFile(cfgPath, doc, 0o600); err != nil {
				t.Fatal(err)
			}

			var stdout, stderr lockedBuf
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan int, 1)
			go func() { done <- run(ctx, []string{"--config", cfgPath}, &stdout, &stderr) }()

			select {
			case id := <-attached:
				if id != "w1" {
					t.Errorf("server saw agent %q, want w1", id)
				}
			case code := <-done:
				t.Fatalf("agent exited early with %d: %s", code, stderr.String())
			case <-time.After(10 * time.Second):
				t.Fatalf("agent did not dial the server: %s", stderr.String())
			}
			cancel()
			select {
			case code := <-done:
				if code != 0 || !strings.Contains(stderr.String(), "shut down") {
					t.Errorf("exit %d, log: %s", code, stderr.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("agent did not shut down")
			}
		})
	}
}

func TestAgentDialModeNeedsServerAddr(t *testing.T) {
	cfg := writeAgentConfig(t, config.RoleMonitor)
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"connect_mode":"server_dial"`, `"connect_mode":"agent_dial"`, 1))
	if err := os.WriteFile(cfg, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr lockedBuf
	if code := run(context.Background(), []string{"--config", cfg}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "server_addr") {
		t.Errorf("agent_dial without server_addr: exit %d, stderr %q, want a server_addr error", code, stderr.String())
	}
}
