//go:build !linux

package capture

import (
	"fmt"
	"log/slog"
)

// newLiveSource is unavailable off Linux: live capture is cgroup v2 + nftables,
// which is Linux-only. The pure record/segment/manifest/source-IP logic still
// builds and tests everywhere.
func newLiveSource(cgroupPath string, group int, log *slog.Logger) (packetSource, error) {
	return nil, fmt.Errorf("capture: live capture is only supported on Linux")
}
