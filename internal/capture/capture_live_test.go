//go:build linux

package capture

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestLiveSourceCloseUnblocksReader proves, without root, that Close() stops the
// reader and returns promptly even while read() is blocked in Recvfrom. It wires
// a liveSource onto one end of a socketpair (no traffic ever arrives, so the
// reader sits in Recvfrom), then asserts Close returns within a bound and the
// reader goroutine exits — the exact property the live hang violated (T-0017).
func TestLiveSourceCloseUnblocksReader(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	defer unix.Close(fds[1]) // peer end; read end (fds[0]) is closed by Close()

	if err := setRecvTimeout(fds[0], nflogReadTimeout); err != nil {
		unix.Close(fds[0])
		t.Fatalf("setRecvTimeout: %v", err)
	}
	s := &liveSource{
		fd:   fds[0],
		log:  slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
		pkts: make(chan capturedPacket, captureBuffer),
	}
	// installed stays false, so Close does not exec nft: hermetic, no root.
	s.wg.Add(1)
	go s.read() // blocks in Recvfrom (nothing is ever sent on the peer)

	// Give the reader a moment to actually enter Recvfrom.
	time.Sleep(2 * nflogReadTimeout)

	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case cerr := <-closed:
		if cerr != nil {
			t.Errorf("Close returned error: %v", cerr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return: reader was not unblocked (hang regression)")
	}

	// The Packets channel must be closed (reader stopped, no goroutine leak).
	if _, ok := <-s.pkts; ok {
		t.Error("Packets channel should be closed after Close")
	}
	// A second Close must be a harmless no-op.
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
