//go:build linux

package watchdog

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

// IFLA_STATS64 is the rtnetlink attribute carrying struct rtnl_link_stats64.
const (
	iflaIfname  = 3
	iflaStats64 = 23
	// tx_bytes is the fourth u64 of rtnl_link_stats64
	// (rx_packets, tx_packets, rx_bytes, tx_bytes, ...).
	stats64TxBytesOffset = 24
	iffLoopback          = 0x8
)

// diskPercent returns percent used of the filesystem at path, as df reports it.
func diskPercent(path string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	used := float64(st.Blocks - st.Bfree)
	avail := float64(st.Bavail)
	if used+avail == 0 {
		return 0, nil
	}
	return clampPercent(100 * used / (used + avail)), nil
}

// linkTxBytes sums transmitted bytes over every non-loopback interface, read
// from rtnetlink (RTM_GETLINK). No root is needed.
func linkTxBytes() (uint64, error) {
	rib, err := syscall.NetlinkRIB(syscall.RTM_GETLINK, syscall.AF_UNSPEC)
	if err != nil {
		return 0, fmt.Errorf("netlink RTM_GETLINK: %w", err)
	}
	return parseLinkTx(rib)
}

// parseLinkTx sums tx_bytes over the non-loopback links in an RTM_GETLINK dump.
func parseLinkTx(rib []byte) (uint64, error) {
	msgs, err := syscall.ParseNetlinkMessage(rib)
	if err != nil {
		return 0, fmt.Errorf("parse netlink: %w", err)
	}
	var total uint64
	var found bool
	for _, m := range msgs {
		if m.Header.Type != syscall.RTM_NEWLINK || len(m.Data) < syscall.SizeofIfInfomsg {
			continue
		}
		ifi := (*syscall.IfInfomsg)(unsafe.Pointer(&m.Data[0]))
		if ifi.Flags&iffLoopback != 0 {
			continue
		}
		attrs, err := syscall.ParseNetlinkRouteAttr(&m)
		if err != nil {
			return 0, fmt.Errorf("parse netlink attrs: %w", err)
		}
		for _, a := range attrs {
			if a.Attr.Type != iflaStats64 || len(a.Value) < stats64TxBytesOffset+8 {
				continue
			}
			total += *(*uint64)(unsafe.Pointer(&a.Value[stats64TxBytesOffset]))
			found = true
		}
	}
	if !found {
		return 0, errors.New("netlink: no link statistics")
	}
	return total, nil
}
