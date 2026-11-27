//go:build !linux

package agent

import "syscall"

// procAttr puts an agent in its own process group (so it can be signalled
// with everything it started). There is no parent-death signal here: an
// agent left behind by a crash exits when its stdin closes.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
