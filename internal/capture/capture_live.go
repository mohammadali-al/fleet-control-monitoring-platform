//go:build linux

package capture

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Live capture mechanism (chosen per §6.2): cgroup v2 + nftables `socket
// cgroupv2` matcher. For a session we install a tiny inet table whose output
// chain matches the session's cgroup and mirrors every matching outbound packet
// to a per-session NFLOG group with `log group N`. `log` is a non-terminating
// action, so capture never affects the traffic (best-effort, never blocks the
// command, §6.3b). A netlink NETLINK_NETFILTER socket bound to that group
// receives full packet copies, which decode() turns into §6.3 records.
//
// This path needs root (CAP_NET_ADMIN) and the `nft` binary; newLiveSource
// returns an error otherwise so the caller runs the command uncaptured. It is
// exercised only on a real kernel as root (TestLive* are skipped without it).

// nfnetlink_log ABI constants (uapi linux/netfilter/nfnetlink_log.h). x/sys
// exposes the subsystem id but not these, so they are defined here.
const (
	nfulnlMsgPacket = 0
	nfulnlMsgConfig = 1

	nfulaCfgCmd  = 1
	nfulaCfgMode = 2

	nfulnlCfgCmdBind   = 1
	nfulnlCfgCmdPFBind = 3

	nfulnlCopyPacket = 2

	// packet attribute types
	nfulaTimestamp = 3
	nfulaPayload   = 9
)

const cgroupV2Mount = "/sys/fs/cgroup"

// liveSource is a running NFLOG-backed capture for one session cgroup.
type liveSource struct {
	group int
	table string
	fd    int
	log   *slog.Logger

	pkts   chan capturedPacket
	drops  atomic.Uint64
	closed atomic.Bool
	wg     sync.WaitGroup
	once   sync.Once
}

// newLiveSource installs the nftables rule and binds the NFLOG group. It is the
// production sourceFactory wired into Controller.newSource.
func newLiveSource(cgroupPath string, group int, log *slog.Logger) (packetSource, error) {
	if log == nil {
		log = slog.Default()
	}
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("capture: live capture needs root (CAP_NET_ADMIN)")
	}
	if cgroupPath == "" {
		return nil, fmt.Errorf("capture: empty cgroup path")
	}
	rel, level, err := cgroupRelLevel(cgroupPath)
	if err != nil {
		return nil, err
	}

	fd, err := openNFLOG(group)
	if err != nil {
		return nil, err
	}

	s := &liveSource{
		group: group,
		table: fmt.Sprintf("fleet_capture_%d", group),
		fd:    fd,
		log:   log,
		pkts:  make(chan capturedPacket, captureBuffer),
	}
	if err := s.installRule(rel, level); err != nil {
		unix.Close(fd)
		return nil, err
	}
	s.wg.Add(1)
	go s.read()
	return s, nil
}

func (s *liveSource) Packets() <-chan capturedPacket { return s.pkts }
func (s *liveSource) Drops() uint64                  { return s.drops.Load() }

// Close removes the nft rule, stops the reader and closes the channel.
func (s *liveSource) Close() error {
	var err error
	s.once.Do(func() {
		s.closed.Store(true)
		// Closing the fd unblocks the blocked Recvfrom in read().
		err = unix.Close(s.fd)
		s.removeRule()
		s.wg.Wait()
		close(s.pkts)
	})
	return err
}

// cgroupRelLevel converts an absolute cgroup v2 path into the (path, level)
// pair nft's `socket cgroupv2 level N "path"` matcher expects.
func cgroupRelLevel(cgroupPath string) (string, int, error) {
	clean := filepath.Clean(cgroupPath)
	rel := strings.TrimPrefix(clean, cgroupV2Mount+"/")
	if rel == clean || rel == "" {
		return "", 0, fmt.Errorf("capture: cgroup path %q not under %s", cgroupPath, cgroupV2Mount)
	}
	return rel, strings.Count(rel, "/") + 1, nil
}

// installRule applies the per-session nft table via `nft -f -`.
func (s *liveSource) installRule(rel string, level int) error {
	ruleset := fmt.Sprintf(`table inet %s {
	chain out {
		type filter hook output priority 0; policy accept;
		socket cgroupv2 level %d "%s" log group %d
	}
}
`, s.table, level, rel, s.group)
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(ruleset)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("capture: nft install: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// removeRule deletes the per-session nft table (best-effort).
func (s *liveSource) removeRule() {
	cmd := exec.Command("nft", "delete", "table", "inet", s.table)
	if out, err := cmd.CombinedOutput(); err != nil {
		s.log.Warn("capture: nft cleanup", "table", s.table, "err", err, "out", strings.TrimSpace(string(out)))
	}
}

// openNFLOG opens a netlink socket and binds the NFLOG group with full-packet
// copy mode.
func openNFLOG(group int) (int, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW, unix.NETLINK_NETFILTER)
	if err != nil {
		return -1, fmt.Errorf("capture: netlink socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("capture: netlink bind: %w", err)
	}
	// Keep the socket alive across buffer overruns; overruns are counted as drops.
	_ = unix.SetsockoptInt(fd, unix.SOL_NETLINK, unix.NETLINK_NO_ENOBUFS, 1)

	// PF_BIND for IPv4 and IPv6, then bind the group, then full-packet copy mode.
	if err := nflogConfig(fd, 0, nfulnlCfgCmdPFBind, unix.AF_INET, nil); err != nil {
		unix.Close(fd)
		return -1, err
	}
	_ = nflogConfig(fd, 0, nfulnlCfgCmdPFBind, unix.AF_INET6, nil)
	if err := nflogConfig(fd, uint16(group), nfulnlCfgCmdBind, unix.AF_UNSPEC, nil); err != nil {
		unix.Close(fd)
		return -1, err
	}
	// config_mode: copy_range 0 => whole packet, copy_mode COPY_PACKET.
	mode := make([]byte, 6)
	binary.BigEndian.PutUint32(mode[0:4], 0)
	mode[4] = nfulnlCopyPacket
	if err := nflogConfig(fd, uint16(group), 0, unix.AF_UNSPEC, &cfgMode{mode: mode}); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// cfgMode carries the NFULA_CFG_MODE attribute payload.
type cfgMode struct{ mode []byte }

// nflogConfig sends one NFULNL_MSG_CONFIG message. When cmd != 0 it includes a
// NFULA_CFG_CMD attribute; when mode != nil it includes NFULA_CFG_MODE.
func nflogConfig(fd int, resID uint16, cmd byte, family uint8, mode *cfgMode) error {
	var attrs []byte
	if cmd != 0 {
		attrs = append(attrs, nlAttr(nfulaCfgCmd, []byte{cmd})...)
	}
	if mode != nil {
		attrs = append(attrs, nlAttr(nfulaCfgMode, mode.mode)...)
	}
	msg := nfnlMessage(nfulnlMsgConfig, resID, family, attrs)
	sa := &unix.SockaddrNetlink{Family: unix.AF_NETLINK}
	if err := unix.Sendto(fd, msg, 0, sa); err != nil {
		return fmt.Errorf("capture: nflog config: %w", err)
	}
	return nil
}

// nfnlMessage frames an nfnetlink message: nlmsghdr + nfgenmsg + attrs. The
// message type is (NFNL_SUBSYS_ULOG<<8)|subtype.
func nfnlMessage(subtype uint16, resID uint16, family uint8, attrs []byte) []byte {
	const hdrLen = unix.NLMSG_HDRLEN
	total := hdrLen + 4 + len(attrs) // + nfgenmsg(4)
	b := make([]byte, total)
	binary.LittleEndian.PutUint32(b[0:4], uint32(total))
	binary.LittleEndian.PutUint16(b[4:6], (unix.NFNL_SUBSYS_ULOG<<8)|subtype)
	binary.LittleEndian.PutUint16(b[6:8], unix.NLM_F_REQUEST)
	// seq (8:12) and pid (12:16) left zero
	// nfgenmsg
	b[hdrLen] = family
	b[hdrLen+1] = 0                                         // version
	binary.BigEndian.PutUint16(b[hdrLen+2:hdrLen+4], resID) // res_id, network order
	copy(b[hdrLen+4:], attrs)
	return b
}

// nlAttr encodes one nlattr (TLV), padded to 4 bytes.
func nlAttr(typ uint16, data []byte) []byte {
	l := 4 + len(data)
	b := make([]byte, (l+3)&^3)
	binary.LittleEndian.PutUint16(b[0:2], uint16(l))
	binary.LittleEndian.PutUint16(b[2:4], typ)
	copy(b[4:], data)
	return b
}

// read is the receive loop: it pulls NFLOG packets and forwards their payloads.
func (s *liveSource) read() {
	defer s.wg.Done()
	buf := make([]byte, 128*1024)
	for {
		n, _, err := unix.Recvfrom(s.fd, buf, 0)
		if err != nil {
			if s.closed.Load() {
				return
			}
			if err == unix.ENOBUFS {
				s.drops.Add(1) // kernel dropped messages under burst (§6.3b)
				continue
			}
			if err == unix.EINTR {
				continue
			}
			s.log.Error("capture: nflog recv", "err", err)
			return
		}
		s.dispatch(buf[:n])
	}
}

// dispatch walks the netlink messages in b and forwards packet payloads. Each
// message is nlmsghdr(16) + body, aligned to 4 bytes.
func (s *liveSource) dispatch(b []byte) {
	for len(b) >= unix.NLMSG_HDRLEN {
		mlen := int(binary.LittleEndian.Uint32(b[0:4]))
		mtype := binary.LittleEndian.Uint16(b[4:6])
		if mlen < unix.NLMSG_HDRLEN || mlen > len(b) {
			return
		}
		body := b[unix.NLMSG_HDRLEN:mlen]
		if mtype>>8 == unix.NFNL_SUBSYS_ULOG && mtype&0xff == nfulnlMsgPacket {
			s.forward(body)
		}
		b = b[(mlen+3)&^3:]
	}
}

// forward extracts NFULA_PAYLOAD (and NFULA_TIMESTAMP when present) from one
// packet message's attributes and sends it on, counting a drop if the buffer
// is full (best-effort, never blocks).
func (s *liveSource) forward(data []byte) {
	if len(data) < 4 {
		return
	}
	attrs := data[4:] // skip nfgenmsg
	var payload []byte
	ts := time.Now()
	for len(attrs) >= 4 {
		alen := int(binary.LittleEndian.Uint16(attrs[0:2]))
		atyp := binary.LittleEndian.Uint16(attrs[2:4]) & 0x3fff
		if alen < 4 || alen > len(attrs) {
			break
		}
		val := attrs[4:alen]
		switch atyp {
		case nfulaPayload:
			payload = append([]byte(nil), val...)
		case nfulaTimestamp:
			if len(val) >= 16 {
				sec := binary.BigEndian.Uint64(val[0:8])
				usec := binary.BigEndian.Uint64(val[8:16])
				ts = time.Unix(int64(sec), int64(usec)*1000)
			}
		}
		attrs = attrs[(alen+3)&^3:]
	}
	if payload == nil {
		return
	}
	select {
	case s.pkts <- capturedPacket{data: payload, ts: ts}:
	default:
		s.drops.Add(1) // userspace buffer full: dropped, counted (§6.3b)
	}
}
