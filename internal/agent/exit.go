package agent

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
)

// ExitError is how an agent process ended when it ended by itself (not
// because Collie stopped it): the exit status, or the signal that killed
// it, and what it last wrote to stderr.
type ExitError struct {
	Code   int            // exit status; -1 when a signal killed it
	Signal syscall.Signal // the signal that killed it, 0 if it exited
	Stderr string         // its last lines on stderr
	err    error          // from Wait (nil if it exited with 0)
}

func (e *ExitError) Error() string {
	var msg string
	switch {
	case e.Signal != 0:
		msg = fmt.Sprintf("The agent process was killed by signal %d (%s).", int(e.Signal), SignalName(e.Signal))
	case e.err != nil:
		msg = "agent process exited: " + e.err.Error()
	default:
		msg = "agent process exited"
	}
	if e.Stderr != "" {
		msg += "\n" + e.Stderr
	}
	return msg
}

func (e *ExitError) Unwrap() error { return e.err }

// setsidLine is what util-linux's `setsid --wait` (collie-exec, in the
// container) prints when its child was killed by a signal. It then exits
// with the child's raw wait status, so the exit status is the signal
// (plus 128 when it dumped core), and what follows the colon is a stale
// errno that means nothing.
var setsidLine = regexp.MustCompile(`(?m)^setsid: child \d+ did not exit normally(: [^\n]*)?\n?`)

// newExitError reads what Wait returned and the stderr tail.
func newExitError(waitErr error, stderr string) *ExitError {
	e := &ExitError{err: waitErr, Stderr: stderr}
	var xe *exec.ExitError
	if !errors.As(waitErr, &xe) {
		return e
	}
	e.Code = xe.ExitCode()
	if ws, ok := xe.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		// Killed by a signal right here (an agent on the host).
		e.Signal = ws.Signal()
		return e
	}
	if e.Code > 0 && setsidLine.MatchString(stderr) {
		e.Signal = syscall.Signal(e.Code & 0x7f)
		e.Stderr = strings.TrimSpace(setsidLine.ReplaceAllString(stderr, ""))
	}
	return e
}

var signalNames = map[syscall.Signal]string{
	syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT", syscall.SIGQUIT: "SIGQUIT",
	syscall.SIGILL: "SIGILL", syscall.SIGTRAP: "SIGTRAP", syscall.SIGABRT: "SIGABRT",
	syscall.SIGBUS: "SIGBUS", syscall.SIGFPE: "SIGFPE", syscall.SIGKILL: "SIGKILL",
	syscall.SIGUSR1: "SIGUSR1", syscall.SIGSEGV: "SIGSEGV", syscall.SIGUSR2: "SIGUSR2",
	syscall.SIGPIPE: "SIGPIPE", syscall.SIGALRM: "SIGALRM", syscall.SIGTERM: "SIGTERM",
	syscall.SIGXCPU: "SIGXCPU", syscall.SIGXFSZ: "SIGXFSZ", syscall.SIGSYS: "SIGSYS",
}

// SignalName is a signal's usual name (SIGKILL), as on Linux.
func SignalName(s syscall.Signal) string {
	if n, ok := signalNames[s]; ok {
		return n
	}
	return "signal " + fmt.Sprint(int(s))
}
