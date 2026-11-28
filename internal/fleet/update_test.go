package fleet_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

func TestManagerUpdateDispatchResultAndChunks(t *testing.T) {
	binDir := t.TempDir()
	linuxPayload := []byte("fleet-agent-linux-amd64-v2.0.0-binary-content")
	darwinPayload := []byte("fleet-agent-darwin-arm64-v2.0.0-binary-content")
	if err := os.WriteFile(filepath.Join(binDir, "fleet-agent-linux-amd64"), linuxPayload, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "fleet-agent-darwin-arm64"), darwinPayload, 0o755); err != nil {
		t.Fatal(err)
	}
	linuxHashBytes := sha256.Sum256(linuxPayload)
	linuxHash := hex.EncodeToString(linuxHashBytes[:])

	reg := registry.New()
	for _, mc := range []registry.Machine{
		{ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"10.0.0.1"}, Provisioned: true, AgentVersion: "1.0.0"},
		{ID: "w2", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"10.0.0.2"}, Provisioned: true, AgentVersion: "2.0.0"},
		{ID: "mon1", Role: registry.RoleMonitor, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"10.0.0.3"}, Provisioned: true, AgentVersion: "1.0.0", Specs: &registry.Specs{OS: "darwin", Arch: "arm64"}},
		{ID: "off1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"10.0.0.4"}, Provisioned: true, AgentVersion: "1.0.0"},
	} {
		if err := reg.Add(mc); err != nil {
			t.Fatal(err)
		}
	}

	gate, err := exec.NewGate([]byte("sec"), nil)
	if err != nil {
		t.Fatal(err)
	}

	var links = map[string]*recLink{}
	factory := func(mc registry.Machine, h channel.Handler) (fleet.Link, error) {
		fl := &recLink{h: h}
		links[mc.ID] = fl
		if mc.ID == "off1" {
			return offlineLink{fl}, nil
		}
		return fl, nil
	}

	m, err := fleet.New(fleet.Config{
		Registry:       reg,
		Gate:           gate,
		NewLink:        factory,
		PollInterval:   10 * time.Millisecond,
		AgentBinaryDir: binDir,
		ServerVersion:  "2.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Close() })

	eventually(t, "online agents", func() bool {
		a, _ := m.Machine("w1")
		b, _ := m.Machine("w2")
		c, _ := m.Machine("mon1")
		return a.State == fleet.StateOnline && b.State == fleet.StateOnline && c.State == fleet.StateOnline
	})

	// Report linux/amd64 platform from w1 via TypeAgentVersion.
	links["w1"].h(ctx, envOf(t, channel.TypeAgentVersion, channel.AgentVersionPayload{
		Version: "1.0.0",
		OS:      "linux",
		Arch:    "amd64",
	}))
	eventually(t, "w1 version recorded", func() bool {
		info, _ := m.Machine("w1")
		return info.AgentVersion == "1.0.0"
	})

	// UpdateAgent on already-current w2 is a no-op.
	noopOut, err := m.UpdateAgent(ctx, "w2")
	if err != nil || noopOut.Status != "noop" {
		t.Fatalf("UpdateAgent(w2) = (%+v, %v), want noop", noopOut, err)
	}
	if sent := links["w2"].sentOfType(channel.TypeAgentUpdate); len(sent) != 0 {
		t.Fatalf("w2 received %d TypeAgentUpdate, want 0", len(sent))
	}

	// UpdateAgent on offline / unknown errors.
	if _, err := m.UpdateAgent(ctx, "off1"); !errors.Is(err, fleet.ErrOffline) {
		t.Errorf("UpdateAgent(off1) = %v, want ErrOffline", err)
	}
	if _, err := m.UpdateAgent(ctx, "missing"); !errors.Is(err, fleet.ErrUnknown) {
		t.Errorf("UpdateAgent(missing) = %v, want ErrUnknown", err)
	}

	// UpdateOnlineWorkers updates only outdated online workers (w1, not w2, not mon1, not off1).
	outcomes := m.UpdateOnlineWorkers(ctx)
	if len(outcomes) != 1 || outcomes[0].Machine != "w1" || outcomes[0].Status != "dispatched" {
		t.Fatalf("UpdateOnlineWorkers = %+v, want [w1 dispatched]", outcomes)
	}
	sent := links["w1"].sentOfType(channel.TypeAgentUpdate)
	if len(sent) != 1 {
		t.Fatalf("w1 received %d TypeAgentUpdate, want 1", len(sent))
	}
	var updPayload channel.AgentUpdatePayload
	if err := json.Unmarshal(sent[0].Payload, &updPayload); err != nil {
		t.Fatal(err)
	}
	if updPayload.TargetVersion != "2.0.0" || updPayload.SHA256 != linuxHash || updPayload.OS != "linux" || updPayload.Arch != "amd64" || updPayload.Method != "channel" {
		t.Fatalf("w1 update payload = %+v, want linux/amd64 sha=%s method=channel", updPayload, linuxHash)
	}

	// Chunk request over channel returns the linux/amd64 binary bytes + sha256.
	links["w1"].h(ctx, channel.Envelope{
		Type:    channel.TypeAgentUpdateChunkReq,
		ID:      sent[0].ID,
		Payload: mustJSON(t, channel.AgentUpdateChunkReqPayload{OS: "linux", Arch: "amd64", Offset: 0, Length: 1024}),
	})
	chunks := links["w1"].sentOfType(channel.TypeAgentUpdateChunk)
	if len(chunks) != 1 {
		t.Fatalf("w1 received %d TypeAgentUpdateChunk, want 1", len(chunks))
	}
	var chunk channel.AgentUpdateChunkPayload
	if err := json.Unmarshal(chunks[0].Payload, &chunk); err != nil {
		t.Fatal(err)
	}
	if string(chunk.Data) != string(linuxPayload) || !chunk.EOF || chunk.SHA256 != linuxHash {
		t.Fatalf("chunk = %+v (data=%q), want %q eof=true", chunk, chunk.Data, linuxPayload)
	}

	// AgentUpdateResult progress updates MachineInfo.Update and publishes EventMachine.
	sub := m.Bus().Subscribe(func(ev fleet.Event) bool {
		return ev.Kind == fleet.EventMachine && ev.Machine == "w1"
	})
	defer sub.Close()

	for _, st := range []string{channel.AgentUpdateDownloading, channel.AgentUpdateApplying, channel.AgentUpdateOK} {
		res := channel.AgentUpdateResultPayload{
			TargetVersion:  "2.0.0",
			CurrentVersion: "1.0.0",
			Status:         st,
			At:             time.Now().UTC(),
		}
		if st == channel.AgentUpdateOK {
			res.CurrentVersion = "2.0.0"
		}
		links["w1"].h(ctx, channel.Envelope{
			Type:    channel.TypeAgentUpdateResult,
			ID:      sent[0].ID,
			Payload: mustJSON(t, res),
		})
	}
	eventually(t, "w1 update ok in MachineInfo", func() bool {
		info, _ := m.Machine("w1")
		return info.Update != nil && info.Update.Status == channel.AgentUpdateOK && info.AgentVersion == "2.0.0"
	})

	// UpdateOutdated(ctx, false) picks up mon1 (darwin/arm64) now that w1 and w2 are 2.0.0.
	outAll := m.UpdateOutdated(ctx, false)
	if len(outAll) != 1 || outAll[0].Machine != "mon1" || outAll[0].Status != "dispatched" {
		t.Fatalf("UpdateOutdated = %+v, want [mon1 dispatched]", outAll)
	}
	monSent := links["mon1"].sentOfType(channel.TypeAgentUpdate)
	if len(monSent) != 1 {
		t.Fatalf("mon1 received %d TypeAgentUpdate, want 1", len(monSent))
	}
	var monUpd channel.AgentUpdatePayload
	_ = json.Unmarshal(monSent[0].Payload, &monUpd)
	if monUpd.OS != "darwin" || monUpd.Arch != "arm64" {
		t.Errorf("mon1 update platform = %s/%s, want darwin/arm64", monUpd.OS, monUpd.Arch)
	}

	// Missing platform binary yields ErrAgentBinaryUnavailable.
	if err := reg.Update("mon1", func(mc *registry.Machine) {
		mc.Specs = &registry.Specs{OS: "darwin", Arch: "amd64"}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.UpdateAgent(ctx, "mon1"); !errors.Is(err, config.ErrAgentBinaryUnavailable) {
		t.Errorf("missing platform err = %v, want ErrAgentBinaryUnavailable", err)
	}
	_ = strings.TrimSpace
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type offlineLink struct{ *recLink }

func (offlineLink) Online() bool { return false }
