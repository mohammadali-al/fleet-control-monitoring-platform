//go:build !linux

package watchdog

import "errors"

var errUnsupported = errors.New("watchdog: host metrics are only supported on Linux")

func diskPercent(string) (float64, error) { return 0, errUnsupported }

func linkTxBytes() (uint64, error) { return 0, errUnsupported }
