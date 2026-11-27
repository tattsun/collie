package agent

import "syscall"

// procAttr puts an agent in its own process group (so it can be signalled
// with everything it started) that the kernel terminates if Collie dies.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}
