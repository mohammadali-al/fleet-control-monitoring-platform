package fleet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/onboard"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// gateKeyFile holds the dangerous-command confirmation secret under
// <data>/config/. The agents verify tokens with the same secret; the manager
// pushes it to each worker agent over the channel when the agent connects
// (Config.GateSecret).
const gateKeyFile = "gate.key"

// serverCertValidity is the lifetime of the main server's own channel
// certificate, which is re-issued on every start.
const serverCertValidity = 365 * 24 * time.Hour

// Options are what Open needs from the server's configuration.
type Options struct {
	Config  config.ServerConfig
	DataDir string
	Logger  *slog.Logger
}

// Open builds the production Manager: the persisted registry, the internal CA
// (created on first run under <data>/ca), the onboarding provisioner, the
// confirmation gate and mTLS dialers to the agents. Call Start on the result.
func Open(o Options) (*Manager, error) {
	if o.DataDir == "" {
		return nil, errors.New("fleet: no data directory")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	store := registry.NewStore(o.DataDir)
	reg, err := store.Load()
	if err != nil {
		return nil, err
	}
	ca, err := loadOrCreateCA(filepath.Join(o.DataDir, config.CADirName))
	if err != nil {
		return nil, err
	}
	key, err := loadOrCreateGateKey(filepath.Join(o.DataDir, config.ConfigDirName, gateKeyFile))
	if err != nil {
		return nil, err
	}
	gate, err := exec.NewGate(key, nil)
	if err != nil {
		return nil, err
	}
	links, err := newDialerFactory(ca, o.Config.ServerIDOrDefault(), o.Config.ChannelPortOrDefault(), serverCertIPs(o.Config))
	if err != nil {
		return nil, err
	}
	return New(Config{
		Registry: reg,
		Provisioner: &onboard.Provisioner{
			CA: ca, Registry: reg, Store: store,
			AgentBinaryDir:  o.Config.AgentBinaryDir,
			AgentBinaryPath: o.Config.AgentBinaryPath,
			ServerAddr:      agentDialServerAddr(o.Config),
			ServerID:        o.Config.ServerIDOrDefault(),
		},
		Gate:            gate,
		GateSecret:      key,
		Capture:         o.Config.AgentCapture,
		MonitorStore:    o.Config.MonitorStore,
		DataDir:         o.DataDir,
		AgentBinaryDir:  o.Config.AgentBinaryDir,
		AgentBinaryPath: o.Config.AgentBinaryPath,
		UpdateBaseURL:   enrollUpdateBaseURL(o.Config),
		NewLink:         links,
		Logger:          o.Logger,
	})
}

func enrollUpdateBaseURL(cfg config.ServerConfig) string {
	host := strings.TrimSpace(cfg.EnrollHost)
	if cfg.EnrollAddr == "" || host == "" {
		return ""
	}
	_, port, err := net.SplitHostPort(cfg.EnrollAddr)
	if err != nil {
		return ""
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n > 65535 {
		return ""
	}
	return "https://" + net.JoinHostPort(host, strconv.Itoa(n))
}

func loadOrCreateCA(dir string) (*pki.CA, error) {
	ca, err := pki.LoadCA(dir)
	if err == nil {
		return ca, nil
	}
	// Only a directory with neither file gets a new CA; a half-present or
	// unreadable CA is an error, never silently replaced.
	_, certErr := os.Stat(filepath.Join(dir, pki.CACertFile))
	_, keyErr := os.Stat(filepath.Join(dir, pki.CAKeyFile))
	if !errors.Is(certErr, fs.ErrNotExist) || !errors.Is(keyErr, fs.ErrNotExist) {
		return nil, fmt.Errorf("fleet: load CA from %s: %w", dir, err)
	}
	if ca, err = pki.NewCA(pki.Options{}); err != nil {
		return nil, err
	}
	if err := ca.Save(dir); err != nil {
		return nil, err
	}
	return ca, nil
}

func loadOrCreateGateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if key := strings.TrimSpace(string(data)); key != "" {
			return []byte(key), nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("fleet: read %s: %w", path, err)
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, fmt.Errorf("fleet: generate gate key: %w", err)
	}
	key := hex.EncodeToString(b[:])
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("fleet: create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("fleet: write %s: %w", path, err)
	}
	return []byte(key), nil
}

// NewDialerFactory returns the production LinkFactory: an mTLS channel Dialer
// to <machine IP>:port, authenticated with a certificate for serverID issued
// by ca (carrying the host's local IP SANs as well as serverID as CN/DNS SAN),
// and verifying the agent's certificate (IP SAN and CN = machine ID).
func NewDialerFactory(ca *pki.CA, serverID string, port int) (LinkFactory, error) {
	return newDialerFactory(ca, serverID, port, localInterfaceIPs())
}

func newDialerFactory(ca *pki.CA, serverID string, port int, ips []net.IP) (LinkFactory, error) {
	certPEM, keyPEM, err := ca.IssuePeer(serverID, ips, serverCertValidity)
	if err != nil {
		return nil, fmt.Errorf("fleet: issue the server's channel certificate: %w", err)
	}
	caPEM := ca.CertPEM()
	return func(mc registry.Machine, h channel.Handler) (Link, error) {
		if len(mc.PrivateIPs) == 0 {
			return nil, fmt.Errorf("machine %s has no address", mc.ID)
		}
		ip := net.ParseIP(mc.PrivateIPs[0])
		tlsCfg, err := pki.DialTLSConfig(certPEM, keyPEM, caPEM, ip)
		if err != nil {
			return nil, err
		}
		d, err := channel.NewDialer(channel.DialerConfig{
			Addr:      net.JoinHostPort(mc.PrivateIPs[0], fmt.Sprint(port)),
			TLS:       tlsCfg,
			MachineID: serverID,
			Handler:   h,
		})
		if err != nil {
			return nil, err
		}
		return &dialerLink{d: d, id: mc.ID}, nil
	}, nil
}

// serverCertIPs gathers the server's IP SANs from config (EnrollHost,
// ListenAddr, AgentDialAddr if they name a specific IP) and local interfaces,
// mirroring how onboard issues agent certs with IP SANs.
func serverCertIPs(cfg config.ServerConfig) []net.IP {
	var out []net.IP
	seen := map[string]bool{}
	if ip := net.ParseIP(strings.TrimSpace(cfg.EnrollHost)); ip != nil {
		out = appendServerIP(out, seen, ip)
	}
	for _, addr := range []string{cfg.ListenAddr, cfg.AgentDialAddr} {
		if host, _, err := net.SplitHostPort(addr); err == nil {
			if ip := net.ParseIP(strings.TrimSpace(host)); ip != nil {
				out = appendServerIP(out, seen, ip)
			}
		}
	}
	for _, ip := range localInterfaceIPs() {
		out = appendServerIP(out, seen, ip)
	}
	return out
}

func localInterfaceIPs() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []net.IP
	seen := map[string]bool{}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		out = appendServerIP(out, seen, ip)
	}
	return out
}

func appendServerIP(out []net.IP, seen map[string]bool, ip net.IP) []net.IP {
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalMulticast() || ip.IsLinkLocalUnicast() {
		return out
	}
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	k := ip.String()
	if seen[k] {
		return out
	}
	seen[k] = true
	return append(out, ip)
}

// dialerLink adapts a channel.Dialer to Link. The agent must be who the
// registry says: a peer with a valid certificate for another ID is offline.
type dialerLink struct {
	d  *channel.Dialer
	id string
}

func (l *dialerLink) Send(ctx context.Context, env channel.Envelope) error { return l.d.Send(ctx, env) }
func (l *dialerLink) Run(ctx context.Context) error                        { return l.d.Run(ctx) }
func (l *dialerLink) Online() bool                                         { return l.d.AgentID() == l.id }

// Registry is the machine registry the manager runs on. Token enrollment
// (internal/enroll) adds machines to it and the manager is told to pick the
// change up with Reconcile.
func (m *Manager) Registry() *registry.Registry { return m.cfg.Registry }

// CA is the internal CA the manager's provisioner holds, or nil when
// provisioning is unavailable.
func (m *Manager) CA() *pki.CA {
	if m.cfg.Provisioner == nil {
		return nil
	}
	return m.cfg.Provisioner.CA
}

// Reconcile starts the links of machines that were added to the registry
// outside the manager (token enrollment) and drops the links of machines that
// were removed or detached.
func (m *Manager) Reconcile() { m.reconcile() }

// agentDialServerAddr returns <EnrollHost>:<agent_dial port> when both
// EnrollHost and a valid AgentDialAddr are configured, else "".
func agentDialServerAddr(cfg config.ServerConfig) string {
	host := strings.TrimSpace(cfg.EnrollHost)
	if cfg.AgentDialAddr == "" || host == "" {
		return ""
	}
	_, port, err := net.SplitHostPort(cfg.AgentDialAddr)
	if err != nil {
		return ""
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n > 65535 {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(n))
}
