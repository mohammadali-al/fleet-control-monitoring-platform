//go:build linux

package exec

import "syscall"

// attach makes attrs place the child in this cgroup.
func (c *Cgroup) attach(attrs *syscall.SysProcAttr) {
	attrs.UseCgroupFD = true
	attrs.CgroupFD = int(c.dir.Fd())
}
