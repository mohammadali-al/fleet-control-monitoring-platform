//go:build !linux

package exec

import "syscall"

// attach is a no-op off Linux: cgroup v2 attachment (UseCgroupFD) is Linux-only.
func (c *Cgroup) attach(_ *syscall.SysProcAttr) {}
