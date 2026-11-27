package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tattsun/collie/internal/model"
)

// Custom agent images: an admin can define images of their own, Collie's
// standard image plus what a company needs (packages, its CA, internal
// tools), and each project picks one (model.Project.Image).
//
// A definition holds only what goes on top: Collie builds it FROM the
// standard image, passed as the build arg COLLIE_BASE, so a multi-stage
// build ends with `FROM ${COLLIE_BASE}`. A build's tag derives from the
// standard image's ID and the definition, so a rebuilt standard image (an
// update, newer agents) or an edited definition needs a new build. Until
// that exists, projects keep running the last good build; only a
// definition's very first build is waited for.
//
// Whatever the image says, agents run as the server user (--user), with
// Collie's command (no ENTRYPOINT) and its home, network and limits.

// A project's image choice (besides an image ID).
const (
	ImageDefault  = ""         // follow the server's default
	ImageStandard = "standard" // Collie's own image
)

const (
	customRepo  = "localhost/collie-agent-" // + ID + ":" + hash
	customLabel = "io.collie.custom"        // = definition ID, on every build
	dataLabel   = "io.collie.data"          // = which Collie (data dir) built it
	stdLabel    = "io.collie.std"           // = the standard image definition it was built on
	candidate   = "-new"                    // tag suffix of a build not checked yet
	maxImageDef = 64 << 10
)

// ErrNoImage: no such image definition.
var ErrNoImage = errors.New("no such agent image")

var errImageBuilding = errors.New("the agent container image is being built (first run, a few minutes) — try again shortly")

type ImageDef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Containerfile instructions run on top of Collie's standard image.
	Containerfile string `json:"containerfile"`
	UpdatedAt     int64  `json:"updatedAt,omitempty"`
}

type imageDefs struct {
	Default string     `json:"default,omitempty"` // an image ID; "" = standard
	Images  []ImageDef `json:"images"`
}

// ImageStatus is a custom image's build state.
type ImageStatus struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Ready: projects can run a build of it. Current: that build is of the
	// current definition and standard image (else a newer one is due).
	Ready    bool   `json:"ready"`
	Current  bool   `json:"current"`
	Building bool   `json:"building,omitempty"`
	Error    string `json:"error,omitempty"`
	Log      string `json:"log,omitempty"`
}

type customImage struct {
	tag      string // the build projects run ("" = none yet)
	want     string // the current definition's tag ("" until the standard image exists)
	checked  time.Time
	building bool
	failed   string // want whose build failed; not retried until asked to
	err      string
	log      *tail
	done     chan struct{} // closed when the running build ends
}

func (c *customImage) current() bool { return c.tag != "" && c.tag == c.want }

// stdID identifies this version's standard image definition: builds on
// another one (another Collie version's) may lack what this one needs.
func (m *Manager) stdID() string { return strings.TrimPrefix(m.tag, "localhost/collie-agent:") }

func (m *Manager) imagesPath() string { return filepath.Join(m.cfg.DataDir, "images.json") }

func (m *Manager) loadImageDefs() {
	m.defs = imageDefs{}
	b, err := os.ReadFile(m.imagesPath())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("agent images: %v", err)
		}
		return
	}
	if err := json.Unmarshal(b, &m.defs); err != nil {
		log.Printf("agent images: %s: %v", m.imagesPath(), err)
	}
}

func (m *Manager) defLocked(id string) (ImageDef, bool) {
	for _, d := range m.defs.Images {
		if d.ID == id {
			return d, true
		}
	}
	return ImageDef{}, false
}

func (m *Manager) customLocked(id string) *customImage {
	c := m.custom[id]
	if c == nil {
		c = &customImage{log: &tail{max: 6 << 10}}
		m.custom[id] = c
	}
	return c
}

// Images returns the server's default image ("" = standard) and the
// definitions.
func (m *Manager) Images() (string, []ImageDef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.defs.Default, append([]ImageDef{}, m.defs.Images...)
}

// dataID tells this Collie's builds from those of another one using the
// same podman (e.g. a second instance with its own data dir).
func (m *Manager) dataID() string {
	dir, _ := filepath.Abs(m.cfg.DataDir)
	h := sha256.Sum256([]byte(dir))
	return hex.EncodeToString(h[:6])
}

// ValidImage reports whether choice can be a project's image.
func (m *Manager) ValidImage(choice string) bool {
	if choice == ImageDefault || choice == ImageStandard {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.defLocked(choice)
	return ok
}

// resolveImage returns the definition a project's choice runs (false: the
// standard image). A deleted image falls back to the default.
func (m *Manager) resolveImage(choice string) (ImageDef, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if choice == ImageStandard {
		return ImageDef{}, false
	}
	if choice != ImageDefault {
		if d, ok := m.defLocked(choice); ok {
			return d, true
		}
	}
	return m.defLocked(m.defs.Default)
}

// PutImage creates (no ID) or changes an image definition and starts
// building it.
func (m *Manager) PutImage(d ImageDef) (ImageDef, error) {
	d.Name = strings.TrimSpace(d.Name)
	d.Containerfile = strings.TrimRight(strings.ReplaceAll(d.Containerfile, "\r\n", "\n"), "\n ") + "\n"
	switch {
	case d.Name == "" || utf8.RuneCountInString(d.Name) > 60:
		return d, errors.New("give the image a name (up to 60 characters)")
	case len(d.Containerfile) > maxImageDef:
		return d, errors.New("the Containerfile is too long")
	case strings.ContainsRune(d.Containerfile, 0) || !utf8.ValidString(d.Containerfile):
		return d, errors.New("the Containerfile must be text")
	}
	m.mu.Lock()
	err := m.changeDefsLocked(func(defs *imageDefs) error {
		for _, o := range defs.Images {
			if o.ID != d.ID && strings.EqualFold(o.Name, d.Name) {
				return fmt.Errorf("an image named %q exists", o.Name)
			}
		}
		d.UpdatedAt = model.NowMs()
		if d.ID == "" {
			d.ID = model.NewID()
			defs.Images = append(defs.Images, d)
			return nil
		}
		i := slices.IndexFunc(defs.Images, func(o ImageDef) bool { return o.ID == d.ID })
		if i < 0 {
			return ErrNoImage
		}
		defs.Images[i] = d
		return nil
	})
	m.mu.Unlock()
	if err != nil {
		return d, err
	}
	if m.available {
		m.refreshCustom(d.ID)
		_ = m.BuildImage(d.ID, false)
	}
	m.changed()
	return d, nil
}

// DeleteImage removes a definition and its builds (those still used by a
// container go when it is re-created). It cannot be the default.
func (m *Manager) DeleteImage(id string) error {
	m.mu.Lock()
	err := m.changeDefsLocked(func(defs *imageDefs) error {
		i := slices.IndexFunc(defs.Images, func(o ImageDef) bool { return o.ID == id })
		if i < 0 {
			return ErrNoImage
		}
		if defs.Default == id {
			return errors.New("this is the default image: choose another default first")
		}
		defs.Images = slices.Delete(defs.Images, i, i+1)
		return nil
	})
	if err == nil {
		delete(m.custom, id)
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	m.changed()
	m.background(m.PruneImages)
	return nil
}

// SetDefaultImage sets the image of projects that don't pick one (and of
// everyone's Home). It has to have a build: they would all wait for the
// first one, or fail with it.
func (m *Manager) SetDefaultImage(id string) error {
	if id == ImageStandard {
		id = ""
	}
	m.mu.Lock()
	err := m.changeDefsLocked(func(defs *imageDefs) error {
		if id != "" {
			if _, ok := m.defLocked(id); !ok {
				return ErrNoImage
			}
			if m.customLocked(id).tag == "" {
				return errors.New("build the image before making it the default")
			}
		}
		defs.Default = id
		return nil
	})
	m.mu.Unlock()
	if err == nil {
		m.changed()
	}
	return err
}

// changeDefsLocked applies fn to a copy of the definitions and keeps the
// copy once it is saved (m.mu held).
func (m *Manager) changeDefsLocked(fn func(*imageDefs) error) error {
	next := imageDefs{Default: m.defs.Default, Images: slices.Clone(m.defs.Images)}
	if err := fn(&next); err != nil {
		return err
	}
	b, err := json.MarshalIndent(next, "", " ")
	if err != nil {
		return err
	}
	tmp := m.imagesPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.imagesPath()); err != nil {
		return err
	}
	m.defs = next
	return nil
}

// imageStatuses is the custom images part of Status (m.mu held).
func (m *Manager) imageStatusesLocked() []ImageStatus {
	out := []ImageStatus{}
	for _, d := range m.defs.Images {
		c := m.customLocked(d.ID)
		st := ImageStatus{ID: d.ID, Name: d.Name, Ready: c.tag != "", Current: c.current(), Building: c.building, Error: c.err}
		if c.building || c.err != "" {
			st.Log = c.log.String()
		}
		out = append(out, st)
	}
	return out
}

// customTag names the build of d on the standard image baseID.
func customTag(d ImageDef, baseID string) string {
	h := sha256.New()
	h.Write([]byte(baseID))
	h.Write([]byte{0})
	h.Write([]byte(d.Containerfile))
	return customRepo + d.ID + ":" + hex.EncodeToString(h.Sum(nil))[:12]
}

// customContainerfile is what Collie builds for a definition.
func customContainerfile(body string) string {
	return "# Collie's standard agent image plus an admin's additions.\nARG COLLIE_BASE\nFROM ${COLLIE_BASE}\n" + body
}

type imageInfo struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Created int64             `json:"Created"`
	Labels  map[string]string `json:"Labels"`
}

// customBuilds lists the builds of custom images (all, or one definition's).
func (m *Manager) customBuilds(ctx context.Context, id string) []imageInfo {
	f := "label=" + customLabel
	if id != "" {
		f += "=" + id
	}
	out, err := m.podman(ctx, "images", "--format", "json", "--filter", f)
	if err != nil {
		return nil
	}
	var imgs []imageInfo
	_ = json.Unmarshal([]byte(out), &imgs)
	return imgs
}

// refreshCustom looks up which build of a definition projects can run:
// the current one if it exists, else the newest, preferring builds on this
// version's standard image definition (another version's only when there
// is none). Lookups of a definition run one at a time, so the last one
// started is what stays.
func (m *Manager) refreshCustom(id string) {
	l := m.lock("image/" + id)
	l.Lock()
	defer l.Unlock()
	m.mu.Lock()
	d, ok := m.defLocked(id)
	m.mu.Unlock()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	want, tag := "", ""
	if base := m.imageID(ctx, m.tag); base != "" {
		want = customTag(d, base)
		if _, err := m.podman(ctx, "image", "exists", want); err == nil {
			tag = want
		}
	}
	if tag == "" {
		var created int64
		same := false
		for _, im := range m.customBuilds(ctx, d.ID) { // the stored id, not the caller's
			i := slices.IndexFunc(im.Names, func(n string) bool { return !strings.HasSuffix(n, candidate) })
			if i < 0 {
				continue
			}
			s := im.Labels[stdLabel] == m.stdID()
			if tag == "" || (s && !same) || (s == same && im.Created >= created) {
				tag, created, same = im.Names[i], im.Created, s
			}
		}
	}
	m.mu.Lock()
	c := m.customLocked(id)
	c.want, c.tag, c.checked = want, tag, time.Now()
	m.mu.Unlock()
}

func (m *Manager) refreshCustoms() {
	_, defs := m.Images()
	for _, d := range defs {
		m.refreshCustom(d.ID)
	}
}

// BuildImage builds a custom image's current definition in the background.
// Without force it does nothing if that exists, is being built or failed
// to; force (an admin's Rebuild) builds it again without the layer cache,
// to pick up newer packages.
func (m *Manager) BuildImage(id string, force bool) error {
	if !m.available {
		return ErrUnavailable
	}
	m.mu.Lock()
	if _, ok := m.defLocked(id); !ok {
		m.mu.Unlock()
		return ErrNoImage
	}
	c := m.customLocked(id)
	if c.building || (!force && c.want != "" && (c.current() || c.failed == c.want)) {
		m.mu.Unlock()
		return nil
	}
	c.building, c.err = true, ""
	c.log = &tail{max: 6 << 10}
	c.done = make(chan struct{})
	logw, done := c.log, c.done
	m.mu.Unlock()
	m.changed()

	m.background(func() {
		var built ImageDef
		var want string
		err := m.waitBase(time.Hour, nil)
		if err == nil {
			m.customSem <- struct{}{} // one at a time
			built, want, err = m.buildCustom(context.Background(), id, force, logw)
			<-m.customSem
		}
		if err != nil {
			log.Printf("agent image %s: %v", id, err)
		}
		m.refreshCustom(id)
		m.mu.Lock()
		c.building = false
		c.failed, c.err = "", ""
		if err != nil {
			c.failed, c.err = want, err.Error()
		}
		// Edited meanwhile, or built on a standard image that has been
		// replaced meanwhile: due again.
		d, ok := m.defLocked(id)
		again := ok && built.ID != "" && (d.Containerfile != built.Containerfile || (c.want != "" && c.want != want))
		close(done)
		m.mu.Unlock()
		m.changed()
		if again {
			_ = m.BuildImage(id, false)
		}
		if err == nil {
			m.PruneImages()
		}
	})
	return nil
}

// buildAllImages brings every custom image up to date, after the standard
// image changed.
func (m *Manager) buildAllImages() {
	m.refreshCustoms()
	_, defs := m.Images()
	for _, d := range defs {
		_ = m.BuildImage(d.ID, false)
	}
}

// BuildStale starts building the custom images that are not up to date
// (e.g. a build failed while an update was being prepared).
func (m *Manager) BuildStale() {
	if !m.available || !m.Status().Ready {
		return
	}
	m.buildAllImages()
}

// buildCustom builds the definition's current image (in the foreground),
// returning what it built.
func (m *Manager) buildCustom(ctx context.Context, id string, noCache bool, w io.Writer) (ImageDef, string, error) {
	m.mu.Lock()
	d, ok := m.defLocked(id)
	m.mu.Unlock()
	if !ok {
		return d, "", ErrNoImage
	}
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	m.forgetImageIDs()
	base := m.imageID(ctx, m.tag)
	if base == "" {
		return d, "", errors.New("Collie's standard agent image is not built yet")
	}
	want := customTag(d, base)
	dir, err := os.MkdirTemp("", "collie-image-") // the build context: just the Containerfile
	if err != nil {
		return d, want, err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "Containerfile"), []byte(customContainerfile(d.Containerfile)), 0o644); err != nil {
		return d, want, err
	}
	// Built under another tag, so that want exists only once it passed the
	// check below.
	cand := want + candidate
	args := []string{"build", "-t", cand, "--label", customLabel + "=" + d.ID, "--label", dataLabel + "=" + m.dataID(),
		"--label", stdLabel + "=" + m.stdID(), "--build-arg", "COLLIE_BASE=" + base, "-f", "Containerfile"}
	if noCache {
		args = append(args, "--no-cache")
	}
	fmt.Fprintf(w, "building agent image %q (%s)\n", d.Name, want)
	if err := m.stream(ctx, dir, w, append(args, ".")...); err != nil {
		return d, want, fmt.Errorf("image build failed: %w", err)
	}
	if err := m.checkImage(ctx, cand); err != nil {
		_, _ = m.podman(ctx, "rmi", cand)
		return d, want, err
	}
	if _, err := m.podman(ctx, "tag", cand, want); err != nil {
		return d, want, err
	}
	_, _ = m.podman(ctx, "untag", cand, cand)
	m.forgetImageIDs()
	return d, want, nil
}

// buildEnv is the environment for image builds and pulls, with a
// containers.conf written into dir (not a build context): RUN steps get the
// agents' network, without the host's loopback or the alias for its
// address.
func buildEnv(dir string) []string {
	conf := filepath.Join(dir, "containers.conf")
	const net = "[network]\npasta_options = [\"--map-host-loopback\", \"none\", \"--map-guest-addr\", \"none\"]\n"
	if os.WriteFile(conf, []byte(net), 0o644) != nil {
		return nil
	}
	return append(os.Environ(), "CONTAINERS_CONF_OVERRIDE="+conf)
}

// imageNeeds is what a container image must have to run Collie's agents.
var imageNeeds = []string{"bash", "sleep", "claude", "codex", "collie-exec", "collie-kill"}

// checkImage makes sure a custom build can still run Collie's agents.
func (m *Manager) checkImage(ctx context.Context, tag string) error {
	script := `for c in "$@"; do command -v "$c" >/dev/null || { echo "$c"; exit 3; }; done`
	args := []string{"run", "--rm", "--network", "none", "--userns=keep-id", "--user", m.userSpec(), "--entrypoint", "", tag, "/bin/sh", "-c", script, "sh"}
	out, err := m.podman(ctx, append(args, imageNeeds...)...)
	if err == nil {
		return nil
	}
	if missing := strings.TrimSpace(out); missing != "" {
		return fmt.Errorf("the image has no %s: keep Collie's standard image as the base (FROM ${COLLIE_BASE})", missing)
	}
	return fmt.Errorf("the image does not run: %v", err)
}

// userSpec is the uid:gid podman runs containers as: the server user, or
// on a remote Podman the user in its VM.
func (m *Manager) userSpec() string {
	if m.remote && m.remoteUser != "" {
		return m.remoteUser
	}
	return strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
}

// buildImagesSync builds the custom images in the foreground (CLI): all
// of them, or only what is missing. Failures are reported, not fatal:
// projects keep running the last good build. With a budget, what doesn't
// fit in it is left to the server, which builds it when it starts.
func (m *Manager) buildImagesSync(w io.Writer, onlyMissing bool, budget time.Duration) {
	ctx := context.Background()
	if budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, budget)
		defer cancel()
	}
	_, defs := m.Images()
	for _, d := range defs {
		if ctx.Err() != nil {
			fmt.Fprintf(w, "warning: agent image %q: no time left; Collie builds it when it starts\n", d.Name)
			continue
		}
		if onlyMissing {
			m.refreshCustom(d.ID)
			m.mu.Lock()
			cur := m.customLocked(d.ID).current()
			m.mu.Unlock()
			if cur {
				fmt.Fprintf(w, "agent image %q is up to date\n", d.Name)
				continue
			}
		}
		if _, _, err := m.buildCustom(ctx, d.ID, false, w); err != nil {
			fmt.Fprintf(w, "warning: agent image %q: %v (projects keep its last good build)\n", d.Name, err)
		}
	}
}

// waitBase makes sure the standard image exists, building it if needed,
// and waits for that (with timeout 0, it fails instead).
func (m *Manager) waitBase(timeout time.Duration, notify func()) error {
	m.waitUp()
	m.mu.Lock()
	ready, building := m.ready, m.building
	m.mu.Unlock()
	if ready {
		return nil
	}
	if !building {
		if err := m.Build(false); err != nil {
			return err
		}
	}
	if timeout == 0 {
		return errImageBuilding
	}
	if notify != nil {
		notify()
	}
	m.mu.Lock()
	done := m.built
	m.mu.Unlock()
	select {
	case <-done:
	case <-time.After(timeout):
		return errors.New("timed out waiting for the agent image build")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ready {
		return fmt.Errorf("agent image build failed: %s", m.buildErr)
	}
	return nil
}

// WaitImage makes sure the image for a project's choice exists and
// returns its tag, building it if needed and waiting for that (up to
// timeout; notify is called once if it has to). With timeout 0 it fails
// instead of waiting. A custom image that has a build already runs that
// while a newer one is built.
func (m *Manager) WaitImage(choice string, timeout time.Duration, notify func()) (string, error) {
	if !m.available {
		return "", ErrUnavailable
	}
	notified := false
	once := func() {
		if notify != nil && !notified {
			notified = true
			notify()
		}
	}
	start := time.Now()
	if err := m.waitBase(timeout, once); err != nil {
		return "", err
	}
	d, ok := m.resolveImage(choice)
	if !ok {
		return m.tag, nil
	}
	m.mu.Lock()
	c := m.customLocked(d.ID)
	fresh := c.current() && time.Since(c.checked) < time.Minute
	tag := c.tag
	m.mu.Unlock()
	if fresh {
		return tag, nil
	}
	m.refreshCustom(d.ID) // a build elsewhere (CLI, an update) or a changed standard image
	if err := m.BuildImage(d.ID, false); err != nil {
		return "", err
	}
	m.mu.Lock()
	tag, building, done, failed := c.tag, c.building, c.done, c.err
	m.mu.Unlock()
	if tag != "" {
		return tag, nil
	}
	if !building {
		if failed == "" {
			return "", fmt.Errorf("agent image %q is not built yet", d.Name)
		}
		return "", fmt.Errorf("agent image %q: %s", d.Name, failed)
	}
	if timeout == 0 {
		return "", errImageBuilding
	}
	once()
	select {
	case <-done:
	case <-time.After(timeout - time.Since(start)):
		return "", errors.New("timed out waiting for the agent image build")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.tag == "" {
		return "", fmt.Errorf("agent image %q: %s", d.Name, c.err)
	}
	return c.tag, nil
}

// pruneCustom removes the builds projects no longer run: a definition's
// builds older than the one in use (podman keeps any a container still
// uses), every build of a deleted one, and builds on another version's
// standard image made before this process started. Builds on another
// version's made since may be an update being prepared, and other Collies'
// builds are left alone.
func (m *Manager) pruneCustom(ctx context.Context) {
	imgs := slices.DeleteFunc(m.customBuilds(ctx, ""), func(im imageInfo) bool { return im.Labels[dataLabel] != m.dataID() })
	m.mu.Lock()
	keep := map[string]string{} // definition → the build projects run
	for _, d := range m.defs.Images {
		keep[d.ID] = m.customLocked(d.ID).tag
	}
	m.mu.Unlock()
	inUse := map[string]int64{}
	for _, im := range imgs {
		id := im.Labels[customLabel]
		if t, ok := keep[id]; ok && t != "" && (im.ID == t || slices.Contains(im.Names, t)) {
			inUse[id] = im.Created
		}
	}
	for _, im := range imgs {
		id := im.Labels[customLabel]
		cur, known := keep[id]
		switch {
		case !known:
		case cur != "" && (im.ID == cur || slices.Contains(im.Names, cur)):
			continue // the build projects run
		case im.Labels[stdLabel] != m.stdID():
			if im.Created >= m.started.Unix() {
				continue
			}
		case cur == "" || inUse[id] == 0 || im.Created >= inUse[id]:
			continue
		}
		if _, err := m.podman(ctx, "rmi", im.ID); err == nil {
			log.Printf("removed old agent image %s", strings.Join(append(im.Names, im.ID[:min(12, len(im.ID))]), " "))
		}
	}
}
