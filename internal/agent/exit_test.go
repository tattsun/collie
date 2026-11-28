package agent

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// waitErr runs a shell command and returns what Wait says about it.
func waitErr(t *testing.T, script string) error {
	t.Helper()
	err := exec.Command("sh", "-c", script).Run()
	if err == nil {
		t.Fatalf("%q exited with 0", script)
	}
	return err
}

// In a container the agent runs under `setsid --wait` (collie-exec), which
// reports a child killed by a signal with that line and exits with the
// signal number (plus 128 with a core dump).
func TestExitErrorReadsSetsid(t *testing.T) {
	for _, c := range []struct {
		code   string
		stderr string
		sig    syscall.Signal
		left   string
		msg    string
	}{
		{"9", "setsid: child 4345 did not exit normally: No such file or directory", syscall.SIGKILL, "",
			"The agent process was killed by signal 9 (SIGKILL)."},
		{"139", "Segfault at 0x0\nsetsid: child 12 did not exit normally: Success\n", syscall.SIGSEGV, "Segfault at 0x0",
			"The agent process was killed by signal 11 (SIGSEGV).\nSegfault at 0x0"},
		{"15", "setsid: child 7 did not exit normally", syscall.SIGTERM, "",
			"The agent process was killed by signal 15 (SIGTERM)."},
		// An agent that exits 9 by itself is not a signal.
		{"9", "fatal: out of tokens", 0, "fatal: out of tokens",
			"agent process exited: exit status 9\nfatal: out of tokens"},
		{"3", "", 0, "", "agent process exited: exit status 3"},
	} {
		e := newExitError(waitErr(t, "exit "+c.code), c.stderr)
		if e.Signal != c.sig || e.Stderr != c.left || e.Error() != c.msg {
			t.Errorf("exit %s with %q: signal %d, stderr %q, message %q; want %d, %q, %q", c.code, c.stderr, e.Signal, e.Stderr, e.Error(), c.sig, c.left, c.msg)
		}
	}
}

// An agent on the host is killed right here.
func TestExitErrorReadsASignal(t *testing.T) {
	e := newExitError(waitErr(t, "kill -KILL $$"), "")
	if e.Signal != syscall.SIGKILL || e.Error() != "The agent process was killed by signal 9 (SIGKILL)." {
		t.Errorf("killed process: %d %q", e.Signal, e.Error())
	}
	var xe *exec.ExitError
	if !errors.As(e, &xe) {
		t.Error("the exec error is not kept")
	}
}

func TestExitErrorFromProc(t *testing.T) {
	p, err := startProc(Config{Cwd: t.TempDir()}, "sh", []string{"-c", "echo boom >&2; kill -TERM $$"}, func([]byte) {})
	if err != nil {
		t.Fatal(err)
	}
	<-p.done
	var e *ExitError
	if err := p.exitError(); !errors.As(err, &e) || e.Signal != syscall.SIGTERM || !strings.HasSuffix(err.Error(), "\nboom") {
		t.Errorf("exit error %v", err)
	}
}
