// Package version holds build metadata. The variables are overridden at build
// time via -ldflags "-X .../internal/version.Version=..." (see scripts/build.sh).
package version

import (
	"fmt"
	"runtime"
)

// Set via -ldflags at build time.
var (
	Version   = "0.0.0-dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

// String formats the build metadata on one line.
func String() string {
	return fmt.Sprintf("%s (commit %s, built %s, %s)", Version, Commit, BuildTime, runtime.Version())
}
