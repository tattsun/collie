package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The apply step runs as `collie update apply …` in a transient systemd
// unit (started by the server, see systemdRun), using the binary that is
// installed now — known to work, unlike the new one:
//
//  1. stop Collie's unit
//  2. back up state.json / auth.json, keep the current binary as <exe>.prev
//  3. move the new binary into place and start the unit
//  4. wait for the new version to report in (the "running" marker) and to
//     stay up; otherwise put the old binary and data back and start that
//
// The outcome goes to update/result.json, which the server shows.

type applyOpts struct {
	data, exe, unit, staged, sha, from, to string
}

// service controls Collie's systemd unit (faked in tests).
type service interface {
	Stop(unit string) error
	Start(unit string) error
	// State returns whether the unit is active and how often systemd has
	// restarted it.
	State(unit string) (active bool, restarts int, err error)
}

type timing struct {
	start  time.Duration // for the new version to report in
	stable time.Duration // for it to then keep running
	poll   time.Duration
}

var defaultTiming = timing{start: 90 * time.Second, stable: 10 * time.Second, poll: 500 * time.Millisecond}

// backedUp are the data files restored when rolling back: the new version
// may have rewritten them in a form the old one cannot read.
var backedUp = []string{"state.json", "auth.json"}

func runApply(args []string) int {
	fs := flag.NewFlagSet("update apply", flag.ContinueOnError)
	var o applyOpts
	fs.StringVar(&o.data, "data", "", "data directory")
	fs.StringVar(&o.exe, "exe", "", "installed binary")
	fs.StringVar(&o.unit, "unit", "", "systemd user unit")
	fs.StringVar(&o.staged, "staged", "", "new binary")
	fs.StringVar(&o.sha, "sha256", "", "checksum of the new binary")
	fs.StringVar(&o.from, "from", "", "current version")
	fs.StringVar(&o.to, "to", "", "new version")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if o.data == "" || o.exe == "" || o.unit == "" || o.staged == "" || o.sha == "" || o.to == "" {
		fmt.Fprintln(os.Stderr, "collie update apply: missing arguments (this is run by Collie itself)")
		return 2
	}
	log.SetFlags(0)
	log.Printf("updating collie %s → %s (%s)", o.from, o.to, o.unit)
	err := apply(o, systemctl{}, defaultTiming)
	res := Result{From: o.from, To: o.to, OK: err == nil, At: time.Now().UnixMilli()}
	if err != nil {
		res.Error = err.Error()
		log.Printf("update failed: %v", err)
	} else {
		log.Printf("collie %s is running", o.to)
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	if werr := writeFileAtomic(filepath.Join(o.data, "update", "result.json"), b, 0o600); werr != nil {
		log.Printf("write result: %v", werr)
	}
	if err != nil {
		return 1
	}
	return 0
}

func apply(o applyOpts, svc service, t timing) error {
	dir := filepath.Join(o.data, "update")
	lock, err := os.OpenFile(filepath.Join(dir, "apply.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return errors.New("another update is being applied")
	}

	// Everything that can fail without touching the running server first.
	if sum, err := fileSHA256(o.staged); err != nil || sum != o.sha {
		return errors.New("the downloaded binary changed or is missing; nothing was installed")
	}
	next := o.exe + ".new"
	if err := copyFile(o.staged, next, 0o755); err != nil {
		return fmt.Errorf("copy the new binary next to %s: %w", o.exe, err)
	}
	defer os.Remove(next)

	if err := svc.Stop(o.unit); err != nil {
		_ = svc.Start(o.unit)
		return fmt.Errorf("stop %s: %w", o.unit, err)
	}
	backup := filepath.Join(dir, "backup")
	if err := backupData(o.data, backup); err != nil {
		_ = svc.Start(o.unit)
		return fmt.Errorf("back up the data: %w; nothing was installed", err)
	}
	prev := o.exe + ".prev"
	if err := copyFile(o.exe, prev, 0o755); err != nil {
		_ = svc.Start(o.unit)
		return fmt.Errorf("keep the current binary: %w; nothing was installed", err)
	}
	if err := os.Rename(next, o.exe); err != nil {
		_ = svc.Start(o.unit)
		return fmt.Errorf("install the new binary: %w", err)
	}

	runErr := startAndWait(o, svc, t)
	if runErr == nil {
		_ = os.RemoveAll(filepath.Dir(o.staged))
		return nil
	}

	// Roll back.
	log.Printf("%v — rolling back to %s", runErr, o.from)
	_ = svc.Stop(o.unit)
	err = copyFile(prev, next, 0o755)
	if err == nil {
		err = os.Rename(next, o.exe)
	}
	if err != nil {
		_ = svc.Start(o.unit)
		return fmt.Errorf("%v, and rolling back failed: %v (the previous binary is %s)", runErr, err, prev)
	}
	if err := restoreData(backup, o.data); err != nil {
		log.Printf("restore data: %v", err)
	}
	_ = os.Remove(markerPath(o.data))
	if err := svc.Start(o.unit); err != nil {
		return fmt.Errorf("%v; rolled back, but starting %s failed: %v", runErr, o.from, err)
	}
	return fmt.Errorf("%v; rolled back to %s", runErr, o.from)
}

// startAndWait starts the unit and waits until the new version reports in
// and stays up.
func startAndWait(o applyOpts, svc service, t timing) error {
	_ = os.Remove(markerPath(o.data))
	if err := svc.Start(o.unit); err != nil {
		return fmt.Errorf("start %s: %w", o.unit, err)
	}
	deadline := time.Now().Add(t.start)
	pid := 0
	for pid == 0 {
		if time.Now().After(deadline) {
			return fmt.Errorf("version %s did not start within %s", o.to, t.start)
		}
		time.Sleep(t.poll)
		if v, p, ok := readMarker(o.data); ok && v == o.to && alive(p) {
			pid = p
		}
	}
	_, restarts, err := svc.State(o.unit)
	if err != nil {
		return err
	}
	time.Sleep(t.stable)
	active, restarts2, err := svc.State(o.unit)
	switch {
	case err != nil:
		return err
	case !active || restarts2 != restarts || !alive(pid):
		return fmt.Errorf("version %s stopped right after starting", o.to)
	}
	return nil
}

// ---- the "running" marker ----

func markerPath(dataDir string) string { return filepath.Join(dataDir, "update", "running") }

// MarkRunning records that this process is up and serving (called once the
// server listens); the apply step waits for it.
func MarkRunning(dataDir, version string) error {
	if err := os.MkdirAll(filepath.Join(dataDir, "update"), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(markerPath(dataDir), []byte(fmt.Sprintf("%s\n%d\n", version, os.Getpid())), 0o600)
}

func readMarker(dataDir string) (version string, pid int, ok bool) {
	b, err := os.ReadFile(markerPath(dataDir))
	if err != nil {
		return "", 0, false
	}
	f := strings.Fields(string(b))
	if len(f) != 2 {
		return "", 0, false
	}
	pid, err = strconv.Atoi(f[1])
	return f[0], pid, err == nil
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

// ---- systemctl ----

type systemctl struct{}

func (systemctl) run(args ...string) (string, error) {
	out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (s systemctl) Stop(unit string) error  { _, err := s.run("stop", unit); return err }
func (s systemctl) Start(unit string) error { _, err := s.run("start", unit); return err }

func (s systemctl) State(unit string) (bool, int, error) {
	out, err := s.run("show", "-p", "ActiveState", "-p", "NRestarts", unit)
	if err != nil {
		return false, 0, err
	}
	active, restarts := false, 0
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "ActiveState":
			active = v == "active"
		case "NRestarts":
			restarts, _ = strconv.Atoi(v)
		}
	}
	return active, restarts, nil
}

// ---- files ----

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyFile writes src to dst atomically (via a temporary file next to dst).
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, mode)
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

func backupData(data, backup string) error {
	_ = os.RemoveAll(backup)
	if err := os.MkdirAll(backup, 0o700); err != nil {
		return err
	}
	for _, name := range backedUp {
		src := filepath.Join(data, name)
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := copyFile(src, filepath.Join(backup, name), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func restoreData(backup, data string) error {
	for _, name := range backedUp {
		src := filepath.Join(backup, name)
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := copyFile(src, filepath.Join(data, name), 0o600); err != nil {
			return err
		}
	}
	return nil
}
