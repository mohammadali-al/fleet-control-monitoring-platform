//go:build !linux

package capture

import (
	"context"
	"errors"
)

// watchNetworkChanges is unavailable off Linux; the caller notes and skips it
// while keeping boot, periodic, and manual public-IP refreshes active.
func watchNetworkChanges(context.Context, func()) error {
	return errors.New("network-change trigger is only supported on Linux")
}
