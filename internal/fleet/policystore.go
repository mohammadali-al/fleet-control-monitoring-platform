package fleet

import (
	"errors"
	"fmt"
	"sync"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
)

// PolicyStore holds the watchdog policy profile on the main server: version-
// stamped YAML under <DataDir>/watchdog/policy.yaml, the same validated file
// format the agents keep as their last-known policy. The manager pushes it to
// every worker agent (WatchdogSource.PolicyFor).
type PolicyStore struct {
	dir string
	mu  sync.Mutex
}

// ErrPolicyVersion means a saved profile does not carry a newer version than
// the stored one. Agents refuse an older profile and an equal one changes
// nothing visible, so every save must bump the version.
var ErrPolicyVersion = errors.New("fleet: the policy version must be higher than the stored one")

// NewPolicyStore returns the store under the base data dir.
func NewPolicyStore(dataDir string) *PolicyStore { return &PolicyStore{dir: dataDir} }

// Current returns the stored profile; nil when none was saved yet.
func (s *PolicyStore) Current() (*watchdog.Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return watchdog.LoadLastKnown(s.dir)
}

// Save validates raw and stores it as the profile. Its version must be higher
// than the stored one (and at least 1). The error is the validation error,
// suitable to show the operator.
func (s *PolicyStore) Save(raw []byte) (*watchdog.Profile, error) {
	pf, err := watchdog.ParseProfile(raw)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := watchdog.LoadLastKnown(s.dir)
	if err != nil {
		return nil, fmt.Errorf("fleet: stored policy: %w", err)
	}
	if cur != nil && pf.Version <= cur.Version {
		return nil, fmt.Errorf("%w (saving %d, stored %d)", ErrPolicyVersion, pf.Version, cur.Version)
	}
	if err := watchdog.SaveLastKnown(s.dir, pf); err != nil {
		return nil, err
	}
	return pf, nil
}

// PolicyFor implements WatchdogSource.PolicyFor: every machine gets the same
// text (the agent resolves host and group overrides itself).
func (s *PolicyStore) PolicyFor(string) (WatchdogPolicy, bool) {
	pf, err := s.Current()
	if err != nil || pf == nil {
		return WatchdogPolicy{}, false
	}
	return WatchdogPolicy{Version: pf.Version, YAML: string(pf.Raw)}, true
}

// SubscribeWatchdog returns a channel of watchdog events as they arrive from
// the agents and a cancel func. A subscriber that cannot keep up misses events.
func (m *Manager) SubscribeWatchdog() (<-chan WatchdogEvent, func()) {
	ch := make(chan WatchdogEvent, 64)
	m.omx.Lock()
	id := m.wdnext
	m.wdnext++
	m.wdsubs[id] = ch
	m.omx.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			m.omx.Lock()
			delete(m.wdsubs, id)
			m.omx.Unlock()
		})
	}
}

func (m *Manager) publishWatchdog(ev WatchdogEvent) {
	m.omx.Lock()
	defer m.omx.Unlock()
	for _, ch := range m.wdsubs {
		select {
		case ch <- ev:
		default:
		}
	}
}
