// Package registry is the main server's machine registry: the identity of
// every machine (id, role, addresses, connect mode, cert fingerprint, disk
// state) plus its persistence under DataDir/registry/ (see store.go).
//
// Live online state is owned by the channel (internal/channel); the registry
// only holds a derived flag that the channel updates through SetOnline.
package registry

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
)

// Roles and connect modes are shared with the config package.
const (
	RoleWorker  = config.RoleWorker
	RoleMonitor = config.RoleMonitor

	ConnectModeServerDial = config.ConnectModeServerDial
	ConnectModeAgentDial  = config.ConnectModeAgentDial
)

const maxIDLen = 64

// DiskState is the last known disk situation of a machine. The fields are
// stored here; their telemetry source is a later task.
type DiskState struct {
	Status     string     `json:"status,omitempty"` // e.g. ok|warn|critical
	TotalBytes uint64     `json:"total_bytes,omitempty"`
	FreeBytes  uint64     `json:"free_bytes,omitempty"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
}

// Specs is what a machine reported about itself when it enrolled (token
// enrollment, T-0026). All fields are informational and optional.
type Specs struct {
	Hostname  string `json:"hostname,omitempty"`
	OS        string `json:"os,omitempty"`         // linux|darwin
	OSVersion string `json:"os_version,omitempty"` // e.g. "Ubuntu 24.04.1 LTS"
	Kernel    string `json:"kernel,omitempty"`
	Arch      string `json:"arch,omitempty"` // amd64|arm64
	CPUs      int    `json:"cpus,omitempty"`
	MemBytes  uint64 `json:"mem_bytes,omitempty"`
}

// Enrollment channels recorded in Machine.EnrolledVia.
const (
	EnrolledSSH   = "ssh"
	EnrolledToken = "token"
)

// Machine is one registry entry.
type Machine struct {
	ID   string `json:"id"`
	Role string `json:"role"` // worker|monitor

	// PrivateIPs are the LAN addresses the main server dials (server_dial).
	PrivateIPs []string `json:"private_ips"`
	// PublicIP is the cached public address and PublicIPAt when it was seen.
	PublicIP   string     `json:"public_ip,omitempty"`
	PublicIPAt *time.Time `json:"public_ip_at,omitempty"`

	// ConnectMode is server_dial (default) or agent_dial.
	ConnectMode string `json:"connect_mode"`

	// CertFingerprint is the SHA-256 (hex) of the machine's client cert DER.
	CertFingerprint string `json:"cert_fingerprint,omitempty"`
	// SSHHostKey is the machine's SSH host key fingerprint (SHA256:...),
	// pinned on first onboarding and checked on re-provisioning.
	SSHHostKey string `json:"ssh_host_key,omitempty"`

	Disk DiskState `json:"disk"`

	// Specs is what the machine reported at token enrollment (nil for an
	// SSH-onboarded machine). EnrolledVia is "token" or empty/"ssh".
	Specs       *Specs `json:"specs,omitempty"`
	EnrolledVia string `json:"enrolled_via,omitempty"`
	// Stopped is set when the operator stopped the agent from the panel (its
	// service was disabled); a local start of the agent brings it back.
	Stopped bool `json:"stopped,omitempty"`

	// Provisioned is true once onboarding completed on the machine.
	Provisioned bool `json:"provisioned"`
	// Removed marks a decommissioned machine (kept as a record). The
	// fingerprint of its cert at that time is kept in RevokedFingerprint:
	// the intent to revoke it (enforcement belongs with the CA/channel).
	Removed            bool       `json:"removed,omitempty"`
	RemovedAt          *time.Time `json:"removed_at,omitempty"`
	RevokedFingerprint string     `json:"revoked_fingerprint,omitempty"`

	// Online is derived from the channel and never persisted.
	Online bool `json:"-"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Validate checks one machine. It does not check ID uniqueness; see the
// package-level Validate and Registry.Add for that.
func (m Machine) Validate() error {
	if err := validateID(m.ID); err != nil {
		return err
	}
	switch m.Role {
	case RoleWorker, RoleMonitor:
	default:
		return fmt.Errorf("registry: machine %q: invalid role %q (must be %q or %q)", m.ID, m.Role, RoleWorker, RoleMonitor)
	}
	switch m.ConnectMode {
	case ConnectModeServerDial, ConnectModeAgentDial:
	default:
		return fmt.Errorf("registry: machine %q: invalid connect_mode %q (must be %q or %q)", m.ID, m.ConnectMode, ConnectModeServerDial, ConnectModeAgentDial)
	}
	for _, ip := range m.PrivateIPs {
		if net.ParseIP(ip) == nil {
			return fmt.Errorf("registry: machine %q: invalid private IP %q", m.ID, ip)
		}
	}
	if m.PublicIP != "" && net.ParseIP(m.PublicIP) == nil {
		return fmt.Errorf("registry: machine %q: invalid public IP %q", m.ID, m.PublicIP)
	}
	return nil
}

// Validate checks a set of machines: each must be valid and IDs must be
// unique.
func Validate(machines []Machine) error {
	seen := make(map[string]struct{}, len(machines))
	for _, m := range machines {
		if err := m.Validate(); err != nil {
			return err
		}
		if _, dup := seen[m.ID]; dup {
			return fmt.Errorf("registry: duplicate machine id %q", m.ID)
		}
		seen[m.ID] = struct{}{}
	}
	return nil
}

// validateID allows letters, digits, '.', '_' and '-' (safe as a file name,
// a cert CN and a DNS SAN), at most 64 characters.
func validateID(id string) error {
	if id == "" {
		return errors.New("registry: machine id must not be empty")
	}
	if len(id) > maxIDLen {
		return fmt.Errorf("registry: machine id %q too long (max %d)", id, maxIDLen)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("registry: machine id %q has invalid character %q (allowed: letters, digits, . _ -)", id, r)
		}
	}
	return nil
}

// ErrNotFound is returned for an unknown machine id.
var ErrNotFound = errors.New("registry: machine not found")

// Registry is the in-memory machine map. It is safe for concurrent use; all
// reads return copies.
type Registry struct {
	mu       sync.RWMutex
	machines map[string]*Machine
	now      func() time.Time
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{machines: make(map[string]*Machine), now: time.Now}
}

func (m Machine) clone() Machine {
	m.PrivateIPs = append([]string(nil), m.PrivateIPs...)
	if m.Specs != nil {
		sp := *m.Specs
		m.Specs = &sp
	}
	return m
}

// Add inserts a new machine. An empty ConnectMode defaults to server_dial.
// It fails on an invalid machine or a duplicate ID. CreatedAt/UpdatedAt are
// set when zero.
func (r *Registry) Add(m Machine) error {
	if m.ConnectMode == "" {
		m.ConnectMode = ConnectModeServerDial
	}
	m = m.clone()
	if err := m.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.machines[m.ID]; dup {
		return fmt.Errorf("registry: duplicate machine id %q", m.ID)
	}
	now := r.now()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = now
	}
	r.machines[m.ID] = &m
	return nil
}

// Get returns a copy of the machine.
func (r *Registry) Get(id string) (Machine, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.machines[id]
	if !ok {
		return Machine{}, false
	}
	return m.clone(), true
}

// List returns copies of all machines (including removed ones), sorted by ID.
func (r *Registry) List() []Machine {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Machine, 0, len(r.machines))
	for _, m := range r.machines {
		out = append(out, m.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Update applies fn to a copy of the machine, validates the result and, if
// valid, stores it and bumps UpdatedAt. The ID may not be changed. On any
// error the stored machine is unchanged.
func (r *Registry) Update(id string, fn func(*Machine)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.machines[id]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	next := cur.clone()
	fn(&next)
	if next.ID != id {
		return fmt.Errorf("registry: machine id may not change (%q -> %q)", id, next.ID)
	}
	if next.ConnectMode == "" {
		next.ConnectMode = ConnectModeServerDial
	}
	if err := next.Validate(); err != nil {
		return err
	}
	next.UpdatedAt = r.now()
	*cur = next
	return nil
}

// Remove deletes the machine record outright. (Decommissioning with a kept
// record is onboard.Provisioner.Remove, which marks the machine removed.)
func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.machines[id]; !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	delete(r.machines, id)
	return nil
}

// SetOnline is the hook the channel layer calls when a machine's connection
// comes up or goes down. The flag is derived state: it is not persisted and
// does not change UpdatedAt.
func (r *Registry) SetOnline(id string, online bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.machines[id]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	m.Online = online
	return nil
}

// Validate checks the whole registry (see the package-level Validate).
func (r *Registry) Validate() error { return Validate(r.List()) }

// replaceAll swaps in a validated machine list (used by Load).
func (r *Registry) replaceAll(machines []Machine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.machines = make(map[string]*Machine, len(machines))
	for _, m := range machines {
		m := m.clone()
		r.machines[m.ID] = &m
	}
}
