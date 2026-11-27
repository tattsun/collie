// Package update keeps Collie up to date from its signed GitHub releases.
//
// The running server checks for a newer release, downloads and verifies it,
// builds the new version's agent image and then hands over to a separate
// process (a transient systemd unit, see apply.go) that stops Collie,
// swaps the binary, starts it again and rolls back if the new version does
// not come up. By default admins are notified and update with one tap;
// optionally updates install themselves once nothing is running.
package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	assetSums = "SHA256SUMS"
	assetSig  = "SHA256SUMS.sig"
	maxBinary = 256 << 20
)

// AssetName is the release file with the binary for this machine.
func AssetName() string { return "collie-linux-" + runtime.GOARCH }

// Phases of an update in progress.
const (
	PhaseDownloading = "downloading"
	PhasePreparing   = "preparing"  // building the new version's agent image
	PhaseWaiting     = "waiting"    // automatic: waiting until nothing is running
	PhaseRestarting  = "restarting" // handed over to the apply step
)

type Config struct {
	Repo    string // GitHub owner/name the releases come from ("" = no update checks)
	API     string // GitHub API base URL (default https://api.github.com)
	DataDir string
	Current string // the running version
	Keys    []ed25519.PublicKey
	// Podman is the podman executable when isolated projects are possible:
	// the new version's agent image is built before switching to it.
	Podman string
	// Busy counts what a restart would interrupt right now (running agents,
	// terminals in use). Automatic updates wait until it is 0.
	Busy func() int

	// Filled in from the environment by New; tests set them.
	Exe    string                    // the installed binary that gets replaced
	Unit   string                    // the systemd user unit running Collie
	Launch func(args []string) error // starts the apply step outside Collie's unit

	Interval time.Duration // between checks (default 6h)
	Quiet    time.Duration // automatic updates: how long nothing must be busy (default 10m)
	Poll     time.Duration // automatic updates: how often to look (default 30s)
}

// Status is what admins see.
type Status struct {
	Current    string  `json:"current"`
	Enabled    bool    `json:"enabled"` // update checks are on
	Repo       string  `json:"repo,omitempty"`
	Latest     string  `json:"latest,omitempty"`
	Notes      string  `json:"notes,omitempty"`
	URL        string  `json:"url,omitempty"`
	Published  int64   `json:"published,omitempty"`
	Available  bool    `json:"available"`          // Latest is newer than Current
	CanApply   bool    `json:"canApply"`           // this server can update itself
	Why        string  `json:"why,omitempty"`      // …and if not, why
	DevBuild   bool    `json:"devBuild,omitempty"` // not a release: never updated automatically
	Auto       bool    `json:"auto"`
	TokenSet   bool    `json:"tokenSet"`
	CheckedAt  int64   `json:"checkedAt,omitempty"`
	CheckError string  `json:"checkError,omitempty"`
	Phase      string  `json:"phase,omitempty"`
	Error      string  `json:"error,omitempty"`
	Busy       int     `json:"busy"`
	Last       *Result `json:"last,omitempty"` // outcome of the last update
}

// Result is the outcome of an update, written by the apply step.
type Result struct {
	From  string `json:"from"`
	To    string `json:"to"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	At    int64  `json:"at"`
}

type settings struct {
	Auto  bool   `json:"auto,omitempty"`
	Token string `json:"token,omitempty"` // GitHub token, for a private repository
}

type Updater struct {
	cfg Config
	dir string
	why string // why this server cannot update itself ("" = it can)

	mu       sync.Mutex
	set      settings
	latest   *release
	checked  time.Time
	checkErr string
	phase    string
	err      string
	last     *Result
	cancel   context.CancelFunc
	now      chan struct{} // skip the wait for a quiet moment

	// OnChange is called when the status changes.
	OnChange func()
}

func New(cfg Config) *Updater {
	if cfg.Interval <= 0 {
		cfg.Interval = 6 * time.Hour
	}
	if cfg.Quiet <= 0 {
		cfg.Quiet = 10 * time.Minute
	}
	if cfg.Poll <= 0 {
		cfg.Poll = 30 * time.Second
	}
	if cfg.Exe == "" {
		if exe, err := os.Executable(); err == nil {
			cfg.Exe, _ = filepath.EvalSymlinks(exe)
		}
	}
	if cfg.Unit == "" {
		cfg.Unit = userUnit()
	}
	viaSystemd := cfg.Launch == nil
	if viaSystemd {
		cfg.Launch = systemdRun
	}
	u := &Updater{cfg: cfg, dir: filepath.Join(cfg.DataDir, "update")}
	u.why = u.cannotApply(viaSystemd)
	if b, err := os.ReadFile(filepath.Join(u.dir, "settings.json")); err == nil {
		_ = json.Unmarshal(b, &u.set)
	}
	u.last = readResult(u.dir)
	return u
}

// cannotApply says why this server cannot replace and restart itself.
func (u *Updater) cannotApply(viaSystemd bool) string {
	if len(u.cfg.Keys) == 0 {
		return errNoKeys.Error()
	}
	if u.cfg.Unit == "" {
		return "Collie is not running as a systemd user service (install it with `mise run install`)"
	}
	if _, err := exec.LookPath("systemd-run"); err != nil && viaSystemd {
		return "systemd-run is not available"
	}
	if u.cfg.Exe == "" || !dirWritable(filepath.Dir(u.cfg.Exe)) {
		return fmt.Sprintf("the Collie binary (%s) is not in a directory Collie can write to", u.cfg.Exe)
	}
	return ""
}

func (u *Updater) changed() {
	if u.OnChange != nil {
		go u.OnChange()
	}
}

// Start checks for releases in the background (first after a minute,
// then every Interval) and installs them when automatic updates are on.
func (u *Updater) Start() {
	go u.watchResult(3*time.Minute, 2*time.Second)
	if u.cfg.Repo == "" {
		return
	}
	go func() {
		time.Sleep(time.Minute)
		for {
			u.periodic()
			time.Sleep(u.cfg.Interval)
		}
	}()
}

// watchResult picks up the outcome of the update that started this
// process: the apply step writes it only once this version has proven to
// stay up (or, after a rollback, once the previous version is back).
func (u *Updater) watchResult(window, every time.Duration) {
	for end := time.Now().Add(window); time.Now().Before(end); time.Sleep(every) {
		r := readResult(u.dir)
		u.mu.Lock()
		fresh := r != nil && (u.last == nil || *r != *u.last)
		if fresh {
			u.last = r
		}
		u.mu.Unlock()
		if fresh {
			u.changed()
		}
	}
}

func (u *Updater) periodic() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	err := u.Check(ctx)
	cancel()
	if err == nil {
		u.maybeAuto()
	}
}

// maybeAuto starts an automatic update if one is due.
func (u *Updater) maybeAuto() {
	u.mu.Lock()
	due := u.set.Auto && u.why == "" && u.phase == "" && u.latest != nil &&
		newer(u.latest.Tag, u.cfg.Current) && parseVersion(u.cfg.Current).release() &&
		!u.failedLocked(u.latest.Tag)
	u.mu.Unlock()
	if due {
		if err := u.Apply(true); err != nil {
			log.Printf("update: %v", err)
		}
	}
}

// failedLocked reports whether updating to tag was already tried and
// rolled back (automatic updates do not try it again).
func (u *Updater) failedLocked(tag string) bool {
	return u.last != nil && !u.last.OK && u.last.To == strings.TrimPrefix(tag, "v")
}

func (u *Updater) Status() Status {
	busy := 0
	if u.cfg.Busy != nil {
		busy = u.cfg.Busy()
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	st := Status{
		Current: u.cfg.Current, Enabled: u.cfg.Repo != "", Repo: u.cfg.Repo,
		CanApply: u.why == "", Why: u.why, DevBuild: !parseVersion(u.cfg.Current).release(),
		Auto: u.set.Auto, TokenSet: u.set.Token != "", CheckError: u.checkErr,
		Phase: u.phase, Error: u.err, Busy: busy, Last: u.last,
	}
	if !u.checked.IsZero() {
		st.CheckedAt = u.checked.UnixMilli()
	}
	if r := u.latest; r != nil {
		st.Latest, st.Notes, st.URL = strings.TrimPrefix(r.Tag, "v"), r.Notes, r.URL
		st.Available = newer(r.Tag, u.cfg.Current)
		if !r.Published.IsZero() {
			st.Published = r.Published.UnixMilli()
		}
	}
	return st
}

// Check looks up the latest release.
func (u *Updater) Check(ctx context.Context) error {
	if u.cfg.Repo == "" {
		return errors.New("update checks are turned off")
	}
	u.mu.Lock()
	token := u.set.Token
	u.mu.Unlock()
	rel, err := newGitHub(u.cfg.API, u.cfg.Repo, token).latest(ctx)
	u.mu.Lock()
	u.checked = time.Now()
	if err != nil {
		u.checkErr = err.Error()
	} else {
		u.checkErr, u.latest = "", &rel
	}
	u.mu.Unlock()
	u.changed()
	return err
}

// SetAuto turns automatic updates on or off. Turning them off also calls
// off an automatic update that is waiting for a quiet moment.
func (u *Updater) SetAuto(on bool) error {
	u.mu.Lock()
	u.set.Auto = on
	err := u.saveLocked()
	u.mu.Unlock()
	u.changed()
	if on {
		go u.maybeAuto()
	}
	return err
}

// SetToken stores the GitHub token used to read releases ("" clears it).
func (u *Updater) SetToken(token string) error {
	u.mu.Lock()
	u.set.Token = strings.TrimSpace(token)
	err := u.saveLocked()
	u.mu.Unlock()
	u.changed()
	return err
}

func (u *Updater) saveLocked() error {
	if err := os.MkdirAll(u.dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(u.set, "", "  ")
	return writeFileAtomic(filepath.Join(u.dir, "settings.json"), b, 0o600)
}

// Cancel stops an update that has not restarted Collie yet.
func (u *Updater) Cancel() {
	u.mu.Lock()
	if u.cancel != nil && u.phase != PhaseRestarting {
		u.cancel()
	}
	u.mu.Unlock()
}

// Apply installs the latest release: download and verify it, build its
// agent image, then restart into it. With wait (automatic updates) the
// restart waits until nothing has been busy for a while; otherwise it
// happens as soon as the new version is ready. Calling Apply(false) while
// an automatic update waits restarts right away.
func (u *Updater) Apply(wait bool) error {
	u.mu.Lock()
	switch {
	case u.why != "":
		u.mu.Unlock()
		return errors.New(u.why)
	case u.phase == PhaseWaiting && !wait:
		select {
		case u.now <- struct{}{}:
		default:
		}
		u.mu.Unlock()
		return nil
	case u.phase != "":
		u.mu.Unlock()
		return errors.New("an update is already in progress")
	case u.latest == nil || !newer(u.latest.Tag, u.cfg.Current):
		u.mu.Unlock()
		return errors.New("there is no newer release")
	}
	rel, token := *u.latest, u.set.Token
	ctx, cancel := context.WithCancel(context.Background())
	u.cancel, u.now = cancel, make(chan struct{}, 1)
	u.phase, u.err = PhaseDownloading, ""
	u.mu.Unlock()
	u.changed()

	go func() {
		err := u.run(ctx, rel, token, wait)
		cancel()
		u.mu.Lock()
		u.cancel = nil
		if err != nil {
			u.phase = ""
			if !errors.Is(err, context.Canceled) {
				u.err = err.Error()
				log.Printf("update to %s failed: %v", rel.Tag, err)
			}
		}
		// On success the phase stays "restarting" until this process stops.
		u.mu.Unlock()
		u.changed()
	}()
	return nil
}

func (u *Updater) setPhase(p string) {
	u.mu.Lock()
	u.phase = p
	u.mu.Unlock()
	u.changed()
}

func (u *Updater) run(ctx context.Context, rel release, token string, wait bool) error {
	to := strings.TrimPrefix(rel.Tag, "v")
	staged, sum, err := u.download(ctx, rel, token)
	if err != nil {
		return err
	}
	if err := selfTest(ctx, staged, to); err != nil {
		return err
	}
	if u.cfg.Podman != "" {
		u.setPhase(PhasePreparing)
		if err := u.prepareImage(ctx, staged); err != nil {
			return err
		}
	}
	if wait {
		u.setPhase(PhaseWaiting)
		if err := u.waitQuiet(ctx); err != nil {
			return err
		}
	}
	u.setPhase(PhaseRestarting)
	launched := time.Now()
	log.Printf("updating collie %s → %s", u.cfg.Current, to)
	err = u.cfg.Launch([]string{u.cfg.Exe, "update", "apply",
		"--data", u.cfg.DataDir, "--exe", u.cfg.Exe, "--unit", u.cfg.Unit,
		"--staged", staged, "--sha256", sum, "--from", u.cfg.Current, "--to", to})
	if err != nil {
		return fmt.Errorf("start the update: %w", err)
	}
	// Normally this process is stopped within seconds. If it is still here,
	// the apply step gave up before stopping it.
	deadline := time.After(3 * time.Minute)
	every := min(2*time.Second, u.cfg.Poll)
	for {
		select {
		case <-deadline:
			return errors.New("the update did not go ahead (see `journalctl --user -u 'collie-update-*'`)")
		case <-time.After(every):
		}
		if r := readResult(u.dir); r != nil && r.To == to && r.At >= launched.UnixMilli() && !r.OK {
			u.mu.Lock()
			u.last = r
			u.mu.Unlock()
			return errors.New(r.Error)
		}
	}
}

// download fetches the binary for this machine into update/staged/ and
// checks it against the release's signed checksums.
func (u *Updater) download(ctx context.Context, rel release, token string) (path, sum string, err error) {
	gh := newGitHub(u.cfg.API, u.cfg.Repo, token)
	name := AssetName()
	bin, ok := rel.assets[name]
	if !ok {
		return "", "", fmt.Errorf("release %s has no build for this machine (%s)", rel.Tag, name)
	}
	sumsA, ok1 := rel.assets[assetSums]
	sigA, ok2 := rel.assets[assetSig]
	if !ok1 || !ok2 {
		return "", "", fmt.Errorf("release %s is not signed (%s / %s missing)", rel.Tag, assetSums, assetSig)
	}
	sums, err := gh.get(ctx, sumsA, 1<<20)
	if err != nil {
		return "", "", err
	}
	sig, err := gh.get(ctx, sigA, 4<<10)
	if err != nil {
		return "", "", err
	}
	table, err := verifySums(u.cfg.Keys, rel.Tag, sums, sig)
	if err != nil {
		return "", "", err
	}
	want, ok := table[name]
	if !ok {
		return "", "", fmt.Errorf("%s does not list %s", assetSums, name)
	}

	dir := filepath.Join(u.dir, "staged")
	_ = os.RemoveAll(dir) // an earlier, unfinished update
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	f, err := os.CreateTemp(dir, "collie-*.part")
	if err != nil {
		return "", "", err
	}
	defer os.Remove(f.Name())
	h := sha256.New()
	err = gh.fetch(ctx, bin, maxBinary, io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", "", fmt.Errorf("downloaded %s does not match the signed checksum", name)
	}
	path = filepath.Join(dir, "collie-"+strings.TrimPrefix(rel.Tag, "v"))
	if err := os.Chmod(f.Name(), 0o755); err != nil {
		return "", "", err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return "", "", err
	}
	return path, want, nil
}

// selfTest runs the new binary once: it must start on this machine and be
// the version the release says.
func selfTest(ctx context.Context, bin, want string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return fmt.Errorf("the new version does not run on this server: %w", err)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		return fmt.Errorf("the downloaded binary is version %q, not %q", got, want)
	}
	return nil
}

// prepareImage has the new version build its agent image (if its image
// definition changed), so isolated projects work right after the restart.
func (u *Updater) prepareImage(ctx context.Context, bin string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	var out tail
	cmd := exec.CommandContext(ctx, bin, "--prepare-image", "--data", u.cfg.DataDir, "--podman", u.cfg.Podman)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("building the new version's agent image failed: %v\n%s", err, out.String())
	}
	return nil
}

// waitQuiet returns once nothing has been busy for Config.Quiet, when an
// admin says "update now", or with an error when automatic updates are
// turned off meanwhile.
func (u *Updater) waitQuiet(ctx context.Context) error {
	var quietSince time.Time
	t := time.NewTicker(u.cfg.Poll)
	defer t.Stop()
	for {
		u.mu.Lock()
		auto, now := u.set.Auto, u.now
		u.mu.Unlock()
		if !auto {
			return context.Canceled
		}
		switch {
		case u.cfg.Busy != nil && u.cfg.Busy() > 0:
			quietSince = time.Time{}
		case quietSince.IsZero():
			quietSince = time.Now()
		}
		if !quietSince.IsZero() && time.Since(quietSince) >= u.cfg.Quiet {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-now:
			return nil
		case <-t.C:
		}
	}
}

// ---- helpers ----

// userUnit returns the systemd user service this process runs in, from
// its cgroup (…/user@1000.service/app.slice/collie.service), or "".
func userUnit() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 || parts[0] != "0" {
			continue
		}
		return unitFromCgroup(parts[2])
	}
	return ""
}

func unitFromCgroup(path string) string {
	if !strings.Contains(path, "/user@") {
		return "" // a system service or a login session, not the user manager
	}
	last := filepath.Base(path)
	if !strings.HasSuffix(last, ".service") || strings.HasPrefix(last, "user@") {
		return ""
	}
	return last
}

func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".collie-write-test-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

// systemdRun starts the apply step as a transient user unit: outside
// Collie's own cgroup, so stopping Collie does not stop it.
func systemdRun(args []string) error {
	unit := fmt.Sprintf("collie-update-%d", time.Now().Unix())
	cmd := exec.Command("systemd-run", append([]string{"--user", "--collect", "--quiet",
		"--unit", unit, "--description", "Collie update"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func readResult(dir string) *Result {
	b, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		return nil
	}
	var r Result
	if json.Unmarshal(b, &r) != nil || r.To == "" {
		return nil
	}
	return &r
}

func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// tail keeps the last few KB written to it (command output for errors).
type tail struct {
	mu sync.Mutex
	b  []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - 4<<10; over > 0 {
		t.b = t.b[over:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(bytes.TrimSpace(t.b))
}
