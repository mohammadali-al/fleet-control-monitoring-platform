package fleet

import (
	"sync"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
)

// Event kinds published on the bus.
const (
	EventMachine = "machine" // a machine's state or registry entry changed
	EventExec    = "exec"    // a command or shell event from a machine's queue
)

// Event is one thing the panel may want to push to a browser.
type Event struct {
	Kind    string
	Machine string
	// Info is set for EventMachine.
	Info *MachineInfo
	// Exec is set for EventExec (queued, running, refused, chunk, exited).
	Exec *exec.Event
}

// subBuffer is how many events a subscriber may lag behind before it is
// dropped. A dropped subscriber's channel is closed; it must resubscribe and
// refetch its state (the panel's WebSocket handlers close the socket, and the
// page reconnects and reloads).
const subBuffer = 4096

// Bus fans events out to subscribers without ever blocking the publisher
// (publishers run on the channel read loop).
type Bus struct {
	mu   sync.Mutex
	subs map[*Subscription]struct{}
}

// Subscription is one subscriber. Read events from C; C is closed when the
// subscription is closed or dropped for lagging (see Dropped).
type Subscription struct {
	C chan Event

	bus     *Bus
	filter  func(Event) bool
	dropped bool
}

func newBus() *Bus { return &Bus{subs: map[*Subscription]struct{}{}} }

// Subscribe registers a subscriber. filter may be nil (everything).
func (b *Bus) Subscribe(filter func(Event) bool) *Subscription {
	s := &Subscription{C: make(chan Event, subBuffer), bus: b, filter: filter}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

// Close unsubscribes. It is safe to call more than once.
func (s *Subscription) Close() {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	if _, ok := s.bus.subs[s]; ok {
		delete(s.bus.subs, s)
		close(s.C)
	}
}

// Dropped reports whether the subscription was closed because it fell too far
// behind (as opposed to being closed by its owner).
func (s *Subscription) Dropped() bool {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	return s.dropped
}

func (b *Bus) publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.subs {
		if s.filter != nil && !s.filter(ev) {
			continue
		}
		select {
		case s.C <- ev:
		default:
			s.dropped = true
			delete(b.subs, s)
			close(s.C)
		}
	}
}
