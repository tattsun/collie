// Package sandbox runs a project's agents and shells inside a rootless
// Podman container: one long-lived container per project, with only that
// project mounted, resource limits, and a per-project home directory.
package sandbox

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tattsun/collie/internal/egress"
)

//go:embed image
var imageFS embed.FS

// Paths inside the container.
const Home = "/home/agent"

var ErrUnavailable = errors.New("podman is not installed on the server")

type Config struct {
	Podman  string // podman executable
	DataDir string // collie data dir (homes/, shared/, worktrees/)
	Memory  string // default memory limit, e.g. "4g"
	CPUs    string // default CPU limit, e.g. "2"
	Pids    int    // default process limit
}

type Status struct {
	Available bool   `json:"available"`
	Limits    bool   `json:"limits"` // resource limits can be enforced
	Image     string `json:"image"`
	Ready     bool   `json:"ready"`
	Building  bool   `json:"building"`
	Error     string `json:"error,omitempty"`
	Log       string `json:"log,omitempty"`
	Shared    string `json:"shared"`
	// Admin-defined images on top of the standard one, and the one projects
	// run unless they pick another ("" = standard).
	Images       []ImageStatus `json:"images"`
	DefaultImage string        `json:"defaultImage,omitempty"`
	// Server-wide default limits.
	Memory string `json:"memory"`
	CPUs   string `json:"cpus"`
	Pids   int    `json:"pids"`
	// Allowlists: the hosts always allowed and a suggested starting list.
	NetAgents    []string `json:"netAgents"`
	NetSuggested []string `json:"netSuggested"`
}

type Manager struct {
	cfg       Config
	available bool
	tag       string
	global    []string // flags for every podman invocation

	mu       sync.Mutex
	building bool
	buildErr string
	buildLog *tail
	built    chan struct{} // closed when the current build finishes
	ready    bool
	locks    map[string]*sync.Mutex
	selinux  bool                 // SELinux is on: containers run without its labels
	imgIDs   map[string]imageIDAt // IDs of image tags, cached briefly
	stale    map[string]bool      // containers left running with an older configuration
	egress   *egress.Proxy        // for containers on an allowlist (network.go)
	// held, while not nil, keeps Ensure from starting containers until it
	// is closed (HoldStarts, ReleaseStarts).
	held chan struct{}

	// Custom images (images.go).
	defs      imageDefs
	custom    map[string]*customImage
	customSem chan struct{}
	bg        sync.WaitGroup // background builds and prunes
	started   time.Time

	// The plugin directory (see PluginCatalog).
	catMu    sync.Mutex
	catalog  []CatalogPlugin
	catAt    time.Time
	catErr   error // the last refresh failed (at catErrAt)
	catErrAt time.Time

	// OnChange is called when the image status changes.
	OnChange func()
}

func New(cfg Config) *Manager {
	if cfg.Podman == "" {
		cfg.Podman = "podman"
	}
	m := &Manager{cfg: cfg, locks: map[string]*sync.Mutex{}, stale: map[string]bool{}, buildLog: &tail{max: 6 << 10}, egress: egress.NewProxy(),
		imgIDs: map[string]imageIDAt{}, custom: map[string]*customImage{}, customSem: make(chan struct{}, 1), started: time.Now()}
	_, err := exec.LookPath(cfg.Podman)
	m.available = err == nil && Supported()
	m.tag = "localhost/collie-agent:" + imageHash()
	m.global = []string{"--log-level=error"}
	if !userBusAvailable() {
		// No systemd user session (e.g. WSL, or lingering not enabled):
		// the systemd cgroup manager cannot work, so use cgroupfs.
		m.global = append(m.global, "--cgroup-manager=cgroupfs")
	}
	m.loadImageDefs()
	if m.available {
		m.selinux = selinuxEnabled()
		m.ready = m.imageExists()
		m.refreshCustoms()
	}
	return m
}

// Supported reports whether this OS can run isolated projects. Podman on
// macOS runs containers in a VM, which the containers' mounts and tools
// socket don't work through yet.
func Supported() bool { return runtime.GOOS == "linux" }

// selinuxEnabled reports whether SELinux is on (Fedora, RHEL and the
// like), in which case Podman labels containers so that they cannot read
// host files they did not create, such as the project's.
func selinuxEnabled() bool {
	_, err := os.Stat("/sys/fs/selinux/enforce")
	return err == nil
}

func (m *Manager) Available() bool { return m.available }

// Podman is the podman executable (argv[0] of Exec's command lines).
func (m *Manager) Podman() string { return m.cfg.Podman }

// LimitsSupported reports whether rootless containers can get resource
// limits here: that needs the systemd cgroup manager with a user session
// (on a server: `loginctl enable-linger <user>`).
func (m *Manager) LimitsSupported() bool { return userBusAvailable() }
func (m *Manager) SharedPath() string {
	return filepath.Join(m.cfg.DataDir, "shared")
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{Available: m.available, Limits: m.available && m.LimitsSupported(), Image: m.tag, Ready: m.ready,
		Building: m.building, Error: m.buildErr, Shared: m.SharedPath(), Memory: m.cfg.Memory, CPUs: m.cfg.CPUs, Pids: m.cfg.Pids,
		NetAgents: egress.AgentHosts, NetSuggested: egress.Suggested,
		Images: m.imageStatusesLocked(), DefaultImage: m.defs.Default}
	if m.building || m.buildErr != "" {
		st.Log = m.buildLog.String()
	}
	return st
}

func (m *Manager) changed() {
	if m.OnChange != nil {
		go m.OnChange()
	}
}

// imageHash identifies the image definition and the base it is built on,
// so a changed Containerfile or a new pinned base (i.e. a Collie upgrade)
// results in a new image tag.
func imageHash() string {
	h := sha256.New()
	_ = fs.WalkDir(imageFS, "image", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := imageFS.ReadFile(p)
			h.Write([]byte(p))
			h.Write(b)
		}
		return nil
	})
	h.Write([]byte(BaseImage))
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// The most of podman's answers kept: some come from commands run in a
// container (podman exec), whose binaries an agent may have replaced.
const (
	maxPodmanOutput = 32 << 20
	maxPodmanErrors = 1 << 20
)

func (m *Manager) podman(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.cfg.Podman, append(append([]string(nil), m.global...), args...)...)
	out := &cappedBuffer{max: maxPodmanOutput, full: cancel}
	errb := &cappedBuffer{max: maxPodmanErrors}
	cmd.Stdout = out
	cmd.Stderr = errb
	err := cmd.Run()
	if out.over {
		return "", fmt.Errorf("podman %s: more output than expected", args[0])
	}
	if err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), fmt.Errorf("podman %s: %s", args[0], msg)
	}
	return out.String(), nil
}

func (m *Manager) imageExists() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := m.podman(ctx, "image", "exists", m.tag)
	return err == nil
}

// ---- image build ----

// Build builds the agent image in the background. With refresh, the agent
// layer is rebuilt to pick up the latest claude / codex releases.
func (m *Manager) Build(refresh bool) error {
	if !m.available {
		return ErrUnavailable
	}
	m.mu.Lock()
	if m.building {
		m.mu.Unlock()
		return nil
	}
	m.building, m.buildErr = true, ""
	m.buildLog = &tail{max: 6 << 10}
	m.built = make(chan struct{})
	done := m.built
	logw := m.buildLog
	m.mu.Unlock()
	m.changed()

	m.background(func() {
		err := m.build(refresh, logw)
		if err == nil {
			log.Printf("agent image %s built", m.tag)
			m.mu.Lock()
			m.ready = true
			m.mu.Unlock()
			// The custom images are built on this one: they are due before
			// it is reported done.
			m.buildAllImages()
		}
		m.mu.Lock()
		m.building = false
		if err != nil {
			m.buildErr = err.Error()
			log.Printf("agent image build failed: %v", err)
		}
		close(done)
		m.mu.Unlock()
		m.changed()
		if err == nil {
			m.PruneImages()
		}
	})
	return nil
}

// background runs fn in a goroutine that Wait waits for.
func (m *Manager) background(fn func()) {
	m.bg.Add(1)
	go func() {
		defer m.bg.Done()
		fn()
	}()
}

// Wait waits for the background builds and prunes.
func (m *Manager) Wait() { m.bg.Wait() }

func (m *Manager) build(refresh bool, w io.Writer) error {
	dir, err := os.MkdirTemp("", "collie-image-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := extractImage(dir); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	base, err := m.prepareBase(ctx, dir, w)
	if err != nil {
		return err
	}
	args := []string{"build", "-t", m.tag, "-t", latestTag, "-f", "Containerfile", "--build-arg", "BASE=" + base}
	if refresh {
		args = append(args, "--build-arg", "AGENTS_REV="+strconv.FormatInt(time.Now().Unix(), 10))
	}
	if err := m.stream(ctx, dir, w, append(args, ".")...); err != nil {
		return fmt.Errorf("image build failed: %w", err)
	}
	m.forgetImageIDs()
	return nil
}

// extractImage writes the embedded image definition into dir, keeping its
// layout (Containerfile and scripts at the top, base/ below).
func extractImage(dir string) error {
	return fs.WalkDir(imageFS, "image", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, strings.TrimPrefix(strings.TrimPrefix(p, "image"), "/"))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := imageFS.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o755)
	})
}

// BuildSync builds the image in the foreground, streaming the log to w,
// refreshing the agents layer, and then the custom images on top of it.
func (m *Manager) BuildSync(w io.Writer) error {
	if !m.available {
		return ErrUnavailable
	}
	if err := m.build(true, w); err != nil {
		return err
	}
	m.buildImagesSync(w, false, 0)
	return nil
}

// BuildIfMissing builds the image in the foreground unless it exists, and
// the custom images that are not up to date (an update prepares the new
// version's images this way before switching).
func (m *Manager) BuildIfMissing(w io.Writer) error {
	if !m.available {
		return ErrUnavailable
	}
	if m.imageExists() {
		fmt.Fprintf(w, "agent image %s is up to date\n", m.tag)
	} else if err := m.build(false, w); err != nil {
		return err
	}
	// Within the update's hour: what doesn't fit is built when the new
	// version starts, and projects keep its last build meanwhile.
	m.buildImagesSync(w, true, 30*time.Minute)
	return nil
}

type imageIDAt struct {
	id string
	at time.Time
}

// imageID returns the ID an image tag points to. It is part of each
// container's configuration, so containers move to a rebuilt image (e.g.
// with newer agents) the next time they are (re)started while unused.
func (m *Manager) imageID(ctx context.Context, tag string) string {
	m.mu.Lock()
	c := m.imgIDs[tag]
	m.mu.Unlock()
	if c.id != "" && time.Since(c.at) < time.Minute {
		return c.id
	}
	out, err := m.podman(ctx, "image", "inspect", "--format", "{{.Id}}", tag)
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(out)
	m.mu.Lock()
	m.imgIDs[tag] = imageIDAt{id, time.Now()}
	m.mu.Unlock()
	return id
}

func (m *Manager) forgetImageIDs() {
	m.mu.Lock()
	m.imgIDs = map[string]imageIDAt{}
	m.mu.Unlock()
}

const latestTag = "localhost/collie-agent:latest"

// PruneImages removes agent images older than the current one — earlier
// versions' tags and what rebuilds left behind — to reclaim their
// gigabytes, and the custom images' builds no project runs. Newer ones (an
// update being prepared) stay, and podman keeps anything a container still
// uses; that goes on a later prune.
func (m *Manager) PruneImages() {
	if !m.available {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	m.pruneCustom(ctx)
	out, err := m.podman(ctx, "images", "--format", "json", "--filter", "reference=localhost/collie-agent")
	if err != nil {
		return
	}
	var imgs []struct {
		Names   []string `json:"Names"`
		Created int64    `json:"Created"`
	}
	if json.Unmarshal([]byte(out), &imgs) != nil {
		return
	}
	cur := int64(-1)
	for _, im := range imgs {
		for _, n := range im.Names {
			if n == m.tag {
				cur = im.Created
			}
		}
	}
	if cur < 0 {
		return
	}
	for _, im := range imgs {
		if im.Created >= cur {
			continue
		}
		for _, n := range im.Names {
			// The filter is loose: it also matches custom images' names.
			if strings.HasPrefix(n, "localhost/collie-agent:") && n != m.tag && n != latestTag {
				if _, err := m.podman(ctx, "rmi", n); err == nil {
					log.Printf("removed old agent image %s", n)
				}
			}
		}
	}
	_, _ = m.podman(ctx, "image", "prune", "-f", "--filter", "label=io.collie.agent=1")
	m.pruneBases(ctx)
}

// ---- helpers ----

func userBusAvailable() bool {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	_, err := os.Stat(filepath.Join(dir, "bus"))
	return err == nil
}

type tail struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(b), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
