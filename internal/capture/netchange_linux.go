//go:build linux

package capture

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

const (
	netlinkRouteTimeout = 150 * time.Millisecond
	netChangeDebounce   = 150 * time.Millisecond
)

// watchNetworkChanges subscribes to Linux NETLINK_ROUTE multicast notifications
// for IPv4/IPv6 route and address changes and invokes onChange (debounced).
// It is best-effort: socket/bind failures return an error for the caller to
// note and skip without affecting boot, periodic, or manual refresh triggers.
func watchNetworkChanges(ctx context.Context, onChange func()) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("netlink route socket: %w", err)
	}
	defer unix.Close(fd)

	if err := setRecvTimeout(fd, netlinkRouteTimeout); err != nil {
		return fmt.Errorf("netlink route timeout: %w", err)
	}
	sa := &unix.SockaddrNetlink{
		Family: unix.AF_NETLINK,
		Groups: unix.RTMGRP_IPV4_ROUTE | unix.RTMGRP_IPV6_ROUTE | unix.RTMGRP_IPV4_IFADDR | unix.RTMGRP_IPV6_IFADDR,
	}
	if err := unix.Bind(fd, sa); err != nil {
		return fmt.Errorf("netlink route bind: %w", err)
	}

	buf := make([]byte, 8192)
	var (
		pending   bool
		lastEvent time.Time
	)
	for {
		if ctx.Err() != nil {
			return nil
		}
		if pending && time.Since(lastEvent) >= netChangeDebounce {
			pending = false
			if onChange != nil {
				onChange()
			}
		}
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EINTR) {
				continue
			}
			if errors.Is(err, unix.ENOBUFS) {
				pending = true
				lastEvent = time.Now()
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("netlink route recv: %w", err)
		}
		if parseNetlinkRouteEvent(buf[:n]) {
			pending = true
			lastEvent = time.Now()
		}
	}
}

func parseNetlinkRouteEvent(b []byte) bool {
	for len(b) >= unix.NLMSG_HDRLEN {
		msgLen := binary.NativeEndian.Uint32(b[0:4])
		msgType := binary.NativeEndian.Uint16(b[4:6])
		if msgLen < unix.NLMSG_HDRLEN || int(msgLen) > len(b) {
			break
		}
		if msgType == unix.NLMSG_DONE {
			break
		}
		switch msgType {
		case unix.RTM_NEWROUTE, unix.RTM_DELROUTE, unix.RTM_NEWADDR, unix.RTM_DELADDR:
			return true
		}
		step := nlmsgAlign(int(msgLen))
		if step <= 0 || step > len(b) {
			break
		}
		b = b[step:]
	}
	return false
}

func nlmsgAlign(n int) int {
	return (n + unix.NLMSG_ALIGNTO - 1) &^ (unix.NLMSG_ALIGNTO - 1)
}
