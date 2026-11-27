//go:build linux

package capture

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func buildNLMsg(typ uint16, extra int) []byte {
	total := unix.NLMSG_HDRLEN + extra
	buf := make([]byte, nlmsgAlign(total))
	binary.NativeEndian.PutUint32(buf[0:4], uint32(total))
	binary.NativeEndian.PutUint16(buf[4:6], typ)
	return buf
}

func TestParseNetlinkRouteEvent(t *testing.T) {
	for _, typ := range []uint16{unix.RTM_NEWROUTE, unix.RTM_DELROUTE, unix.RTM_NEWADDR, unix.RTM_DELADDR} {
		if !parseNetlinkRouteEvent(buildNLMsg(typ, 4)) {
			t.Errorf("expected true for netlink message type %d", typ)
		}
	}
	// Multi-message batch where the second message is RTM_NEWADDR.
	batch := append(buildNLMsg(unix.RTM_GETROUTE, 4), buildNLMsg(unix.RTM_NEWADDR, 4)...)
	if !parseNetlinkRouteEvent(batch) {
		t.Error("expected true when RTM_NEWADDR follows RTM_GETROUTE")
	}
	// Non-change message or truncated payload should not trigger.
	if parseNetlinkRouteEvent(buildNLMsg(unix.RTM_GETROUTE, 4)) {
		t.Error("RTM_GETROUTE should not trigger")
	}
	if parseNetlinkRouteEvent([]byte{1, 2, 3}) {
		t.Error("truncated message should not trigger")
	}
}

func TestWatchNetworkChangesExitsOnCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- watchNetworkChanges(ctx, func() {})
	}()
	select {
	case err := <-done:
		// nil on clean cancel, or best-effort error in restricted sandboxes.
		_ = err
	case <-time.After(2 * time.Second):
		t.Fatal("watchNetworkChanges did not exit promptly on context cancel")
	}
}
