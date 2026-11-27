package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/model"
)

// The test binary doubles as a fake podman (COLLIE_FAKE_PODMAN=<state dir>)
// that keeps an image list, so the image logic runs without real builds.
// In the state dir: "gate" makes custom builds wait until it is removed, "fail"
// makes them fail, "missing" holds a command the built image lacks.
func TestMain(m *testing.M) {
	if dir := os.Getenv("COLLIE_FAKE_PODMAN"); dir != "" {
		os.Exit(fakeImagesPodman(dir, os.Args[1:]))
	}
	os.Exit(m.Run())
}

type fakeImage struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Created int64             `json:"Created"`
	Labels  map[string]string `json:"Labels"`
}

type fakeState struct {
	Seq    int64       `json:"seq"`
	Images []fakeImage `json:"images"`
	Builds []string    `json:"builds"` // the -t of every build started
}

func fakeImagesPodman(dir string, args []string) int {
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		args = args[1:] // global flags
	}
	if len(args) == 0 {
		return 0
	}
	if args[0] == "build" {
		var tags []string
		labels := map[string]string{}
		for i := 0; i+1 < len(args); i++ {
			switch args[i] {
			case "-t":
				tags = append(tags, args[i+1])
			case "--label":
				k, v, _ := strings.Cut(args[i+1], "=")
				labels[k] = v
			}
		}
		withState(dir, func(st *fakeState) { st.Builds = append(st.Builds, tags[0]) })
		for labels[customLabel] != "" && exists(filepath.Join(dir, "gate")) {
			time.Sleep(5 * time.Millisecond)
		}
		if exists(filepath.Join(dir, "fail")) {
			fmt.Println("E: Unable to locate package foo")
			return 1
		}
		cf, _ := os.ReadFile("Containerfile")
		os.WriteFile(filepath.Join(dir, "containerfile"), cf, 0o600)
		os.WriteFile(filepath.Join(dir, "buildargs"), []byte(strings.Join(args, "\n")), 0o600)
		withState(dir, func(st *fakeState) {
			for i := range st.Images {
				st.Images[i].Names = slices.DeleteFunc(st.Images[i].Names, func(n string) bool { return slices.Contains(tags, n) })
			}
			// Created: Unix seconds, like podman's, but never the same twice.
			st.Seq = max(time.Now().Unix(), st.Seq+1)
			st.Images = append(st.Images, fakeImage{ID: fmt.Sprintf("%064d", st.Seq), Names: tags, Created: st.Seq, Labels: labels})
		})
		return 0
	}
	code := 0
	withState(dir, func(st *fakeState) {
		find := func(ref string) int {
			return slices.IndexFunc(st.Images, func(im fakeImage) bool { return im.ID == ref || slices.Contains(im.Names, ref) })
		}
		switch {
		case len(args) >= 3 && args[0] == "image" && args[1] == "exists":
			if find(args[2]) < 0 {
				code = 1
			}
		case len(args) >= 2 && args[0] == "image" && args[1] == "inspect":
			if i := find(args[len(args)-1]); i >= 0 {
				fmt.Println(st.Images[i].ID)
			} else {
				code = 125
			}
		case args[0] == "images":
			f := args[len(args)-1]
			out := []fakeImage{}
			for _, im := range st.Images {
				if k, ok := strings.CutPrefix(f, "label="); ok {
					k, v, hasV := strings.Cut(k, "=")
					if lv, ok := im.Labels[k]; !ok || (hasV && lv != v) {
						continue
					}
				}
				out = append(out, im)
			}
			b, _ := json.Marshal(out)
			fmt.Println(string(b))
		case args[0] == "rmi":
			// a name of several only loses that name
			if i := find(args[1]); i < 0 {
				code = 1
			} else if im := &st.Images[i]; im.ID != args[1] && len(im.Names) > 1 {
				im.Names = slices.DeleteFunc(im.Names, func(n string) bool { return n == args[1] })
			} else {
				st.Images = slices.Delete(st.Images, i, i+1)
			}
		case args[0] == "tag":
			if i := find(args[1]); i >= 0 {
				for j := range st.Images {
					st.Images[j].Names = slices.DeleteFunc(st.Images[j].Names, func(n string) bool { return n == args[2] })
				}
				st.Images[i].Names = append(st.Images[i].Names, args[2])
			} else {
				code = 1
			}
		case args[0] == "untag":
			if i := find(args[1]); i >= 0 {
				st.Images[i].Names = slices.DeleteFunc(st.Images[i].Names, func(n string) bool { return n == args[2] })
			}
		case args[0] == "run":
			if b, err := os.ReadFile(filepath.Join(dir, "missing")); err == nil {
				fmt.Println(string(b))
				code = 3
			}
		}
	})
	return code
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func withState(dir string, fn func(*fakeState)) {
	l, _ := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	defer l.Close()
	_ = syscall.Flock(int(l.Fd()), syscall.LOCK_EX)
	var st fakeState
	if b, err := os.ReadFile(filepath.Join(dir, "state.json")); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	fn(&st)
	b, _ := json.Marshal(st)
	_ = os.WriteFile(filepath.Join(dir, "state.json"), b, 0o600)
}

func readState(dir string) fakeState {
	var st fakeState
	withState(dir, func(s *fakeState) { st = *s })
	return st
}

func newFakeManager(t *testing.T) (*Manager, string) {
	t.Helper()
	fake := t.TempDir()
	t.Setenv("COLLIE_FAKE_PODMAN", fake)
	t.Setenv("GORACE", "atexit_sleep_ms=0") // a race-enabled fake would linger 1s per call
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m := New(Config{Podman: exe, DataDir: t.TempDir()})
	t.Cleanup(m.Wait) // before the temp dirs go
	return m, fake
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func imageStatus(m *Manager, id string) ImageStatus {
	for _, st := range m.Status().Images {
		if st.ID == id {
			return st
		}
	}
	return ImageStatus{}
}

func idle(m *Manager, id string) func() bool {
	return func() bool { st := m.Status(); return !st.Building && !imageStatus(m, id).Building }
}

func TestCustomImageLifecycle(t *testing.T) {
	m, fake := newFakeManager(t)
	if tag, err := m.WaitImage(ImageDefault, 10*time.Second, nil); err != nil || tag != m.tag {
		t.Fatalf("standard image: %q, %v", tag, err)
	}
	waitFor(t, "the standard build", idle(m, ""))
	baseID := m.imageID(t.Context(), m.tag)

	// The first build of an image is waited for.
	d, err := m.PutImage(ImageDef{Name: "Android", Containerfile: "RUN apt-get install -y foo"})
	if err != nil {
		t.Fatal(err)
	}
	notified := 0
	v1, err := m.WaitImage(d.ID, 10*time.Second, func() { notified++ })
	if err != nil || !strings.HasPrefix(v1, customRepo+d.ID+":") {
		t.Fatalf("first build: %q, %v", v1, err)
	}
	if notified > 1 {
		t.Errorf("notified %d times", notified)
	}
	cf, _ := os.ReadFile(filepath.Join(fake, "containerfile"))
	if !strings.Contains(string(cf), "ARG COLLIE_BASE\nFROM ${COLLIE_BASE}\nRUN apt-get install -y foo") {
		t.Errorf("built Containerfile:\n%s", cf)
	}
	if args, _ := os.ReadFile(filepath.Join(fake, "buildargs")); !strings.Contains(string(args), "COLLIE_BASE="+baseID) {
		t.Errorf("not built on the standard image %s: %s", baseID, args)
	}
	waitFor(t, "the build to settle", idle(m, d.ID))
	if st := imageStatus(m, d.ID); !st.Ready || !st.Current || st.Error != "" {
		t.Fatalf("status after the build: %+v", st)
	}

	// An edited definition: projects keep running the last good build
	// until the new one exists, then move to it; the old one is pruned.
	os.WriteFile(filepath.Join(fake, "gate"), nil, 0o600)
	d.Containerfile = "RUN apt-get install -y foo bar"
	if _, err := m.PutImage(d); err != nil {
		t.Fatal(err)
	}
	if tag, err := m.WaitImage(d.ID, 10*time.Second, func() { t.Error("waited although a build exists") }); err != nil || tag != v1 {
		t.Fatalf("during the rebuild: %q, %v (want the last good %q)", tag, err, v1)
	}
	if st := imageStatus(m, d.ID); !st.Building || !st.Ready || st.Current {
		t.Errorf("status while rebuilding: %+v", st)
	}
	os.Remove(filepath.Join(fake, "gate"))
	waitFor(t, "the rebuild", idle(m, d.ID))
	v2, err := m.WaitImage(d.ID, 10*time.Second, nil)
	if err != nil || v2 == v1 {
		t.Fatalf("after the rebuild: %q, %v", v2, err)
	}
	waitFor(t, "the old build to be pruned", func() bool {
		return !slices.ContainsFunc(readState(fake).Images, func(im fakeImage) bool { return slices.Contains(im.Names, v1) })
	})

	// A failed build keeps the last good one and is not retried by itself.
	os.WriteFile(filepath.Join(fake, "fail"), nil, 0o600)
	d.Containerfile = "RUN apt-get install -y nonexistent"
	if _, err := m.PutImage(d); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the failing build", idle(m, d.ID))
	if st := imageStatus(m, d.ID); st.Error == "" || !st.Ready || st.Current || !strings.Contains(st.Log, "Unable to locate") {
		t.Fatalf("status after a failed build: %+v", st)
	}
	builds := len(readState(fake).Builds)
	if tag, err := m.WaitImage(d.ID, 10*time.Second, nil); err != nil || tag != v2 {
		t.Fatalf("after a failed build: %q, %v (want the last good %q)", tag, err, v2)
	}
	if n := len(readState(fake).Builds); n != builds {
		t.Errorf("a failed build was retried by itself (%d builds, was %d)", n, builds)
	}
	// Rebuild (an admin's) tries again.
	os.Remove(filepath.Join(fake, "fail"))
	if err := m.BuildImage(d.ID, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the rebuild", idle(m, d.ID))
	if st := imageStatus(m, d.ID); !st.Current || st.Error != "" {
		t.Fatalf("after Rebuild: %+v", st)
	}
	if args, _ := os.ReadFile(filepath.Join(fake, "buildargs")); !strings.Contains(string(args), "--no-cache") {
		t.Error("Rebuild used the layer cache")
	}

	// A new standard image (newer agents, an update) rebuilds the custom ones.
	v3, _ := m.WaitImage(d.ID, 10*time.Second, nil)
	if err := m.Build(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the standard image and the custom one on it", func() bool {
		return idle(m, d.ID)() && imageStatus(m, d.ID).Current
	})
	if v4, _ := m.WaitImage(d.ID, 10*time.Second, nil); v4 == v3 {
		t.Error("the custom image was not rebuilt on the new standard image")
	}
}

func TestCustomImageMustStayCollies(t *testing.T) {
	m, fake := newFakeManager(t)
	if _, err := m.WaitImage(ImageDefault, 10*time.Second, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the standard build", idle(m, ""))
	os.WriteFile(filepath.Join(fake, "missing"), []byte("claude"), 0o600)
	d, err := m.PutImage(ImageDef{Name: "Alpine", Containerfile: "FROM alpine"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.WaitImage(d.ID, 10*time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), "has no claude") {
		t.Fatalf("an image without the agents: %v", err)
	}
	if slices.ContainsFunc(readState(fake).Images, func(im fakeImage) bool { return im.Labels[customLabel] == d.ID }) {
		t.Error("the broken build was kept")
	}
}

func TestDefaultAndDeletedImages(t *testing.T) {
	m, fake := newFakeManager(t)
	a, _ := m.PutImage(ImageDef{Name: "A", Containerfile: "RUN true"})
	b, _ := m.PutImage(ImageDef{Name: "B", Containerfile: "RUN false || true"})
	for _, id := range []string{a.ID, b.ID} {
		if _, err := m.WaitImage(id, 10*time.Second, nil); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the build", idle(m, id))
	}
	if err := m.SetDefaultImage(a.ID); err != nil {
		t.Fatal(err)
	}
	resolve := func(choice string) string {
		d, ok := m.resolveImage(choice)
		if !ok {
			return ImageStandard
		}
		return d.ID
	}
	for choice, want := range map[string]string{ImageDefault: a.ID, ImageStandard: ImageStandard, b.ID: b.ID} {
		if got := resolve(choice); got != want {
			t.Errorf("choice %q runs %q, want %q", choice, got, want)
		}
	}
	if tag, _ := m.WaitImage(ImageStandard, time.Second, nil); tag != m.tag {
		t.Errorf("the standard choice runs %q", tag)
	}
	if err := m.DeleteImage(a.ID); err == nil {
		t.Error("deleted the default image")
	}
	if err := m.DeleteImage(b.ID); err != nil {
		t.Fatal(err)
	}
	if got := resolve(b.ID); got != a.ID {
		t.Errorf("a project on the deleted image runs %q, want the default", got)
	}
	waitFor(t, "the deleted image's builds to go", func() bool {
		return !slices.ContainsFunc(readState(fake).Images, func(im fakeImage) bool { return im.Labels[customLabel] == b.ID })
	})
	if !slices.ContainsFunc(readState(fake).Images, func(im fakeImage) bool { return im.Labels[customLabel] == a.ID }) {
		t.Error("pruned the image still in use")
	}
	// Another Collie's builds (its own data dir) are not ours to prune.
	withState(fake, func(st *fakeState) {
		st.Seq++
		st.Images = append(st.Images, fakeImage{ID: "other", Names: []string{customRepo + "x:1"}, Created: st.Seq,
			Labels: map[string]string{customLabel: "x", dataLabel: "elsewhere"}})
	})
	m.PruneImages()
	if !slices.ContainsFunc(readState(fake).Images, func(im fakeImage) bool { return im.ID == "other" }) {
		t.Error("pruned another Collie's image")
	}
	if m.ValidImage(b.ID) || !m.ValidImage(a.ID) || !m.ValidImage(ImageStandard) || !m.ValidImage(ImageDefault) {
		t.Error("ValidImage")
	}
}

func TestImageDefinitions(t *testing.T) {
	m := newTestManager(t)
	for _, bad := range []ImageDef{
		{Name: " ", Containerfile: "RUN true"},
		{Name: strings.Repeat("x", 61), Containerfile: "RUN true"},
		{Name: "big", Containerfile: strings.Repeat("#", maxImageDef+1)},
		{Name: "bin", Containerfile: "RUN true\x00"},
	} {
		if _, err := m.PutImage(bad); err == nil {
			t.Errorf("accepted %q", bad.Name)
		}
	}
	d, err := m.PutImage(ImageDef{Name: " Data science ", Containerfile: "RUN pip install numpy\r\n\r\n"})
	if err != nil || d.ID == "" || d.Name != "Data science" || d.Containerfile != "RUN pip install numpy\n" {
		t.Fatalf("created %+v, %v", d, err)
	}
	if _, err := m.PutImage(ImageDef{Name: "data SCIENCE", Containerfile: "RUN true"}); err == nil {
		t.Error("two images with the same name")
	}
	if _, err := m.PutImage(ImageDef{ID: "nope", Name: "x"}); err != ErrNoImage {
		t.Errorf("changed a missing image: %v", err)
	}
	if err := m.SetDefaultImage(d.ID); err == nil {
		t.Error("an image that was never built became the default (everyone's Home would wait for it)")
	}
	m.customLocked(d.ID).tag = customRepo + d.ID + ":1" // as if built
	if err := m.SetDefaultImage(d.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.SetDefaultImage("nope"); err == nil {
		t.Error("default set to a missing image")
	}
	// Definitions survive a restart.
	m2 := newTestManager(t)
	m2.cfg.DataDir = m.cfg.DataDir
	m2.loadImageDefs()
	def, imgs := m2.Images()
	if def != d.ID || len(imgs) != 1 || imgs[0] != d {
		t.Fatalf("after reload: default %q, %+v", def, imgs)
	}
	if err := m.SetDefaultImage(ImageStandard); err != nil {
		t.Fatal(err)
	}
	if def, _ := m.Images(); def != "" {
		t.Errorf("standard as default stored as %q", def)
	}
}

func TestCustomImageRunArgs(t *testing.T) {
	m := newTestManager(t)
	m.tag = "localhost/collie-agent:abc"
	p := model.Project{ID: "p1", Path: "/x/p"}
	std := m.runArgs(Spec{Project: p, UserID: "u1"}, nil)
	custom := m.runArgs(Spec{Project: p, UserID: "u1", Image: "localhost/collie-agent-x:1"}, nil)
	tail := func(args []string) []string { return args[len(args)-3:] }
	if !slices.Equal(tail(std), []string{m.tag, "sleep", "infinity"}) || !slices.Equal(tail(custom), []string{"localhost/collie-agent-x:1", "sleep", "infinity"}) {
		t.Fatalf("images run: %q / %q", tail(std), tail(custom))
	}
	if slices.Contains(std, "--user") || slices.Contains(std, "--entrypoint") {
		t.Error("the standard image's containers changed")
	}
	i := slices.Index(custom, "--user")
	if i < 0 || custom[i+1] != userSpec() {
		t.Errorf("a custom image may run as its own USER: %q", custom)
	}
	if i := slices.Index(custom, "--entrypoint"); i < 0 || custom[i+1] != "" {
		t.Errorf("a custom image may run its own ENTRYPOINT: %q", custom)
	}
}

// After an update, a definition may have builds on two versions' standard
// images: each version runs its own, and keeps the other's newer ones (an
// update being prepared).
func TestBuildsOfOtherVersions(t *testing.T) {
	m, fake := newFakeManager(t)
	if _, err := m.WaitImage(ImageDefault, 10*time.Second, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the standard build", idle(m, ""))
	d, _ := m.PutImage(ImageDef{Name: "A", Containerfile: "RUN true"})
	mine, err := m.WaitImage(d.ID, 10*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the build", idle(m, d.ID))
	add := func(name, std string, created int64) {
		withState(fake, func(st *fakeState) {
			st.Images = append(st.Images, fakeImage{ID: name, Names: []string{customRepo + d.ID + ":" + name}, Created: created,
				Labels: map[string]string{customLabel: d.ID, dataLabel: m.dataID(), stdLabel: std}})
		})
	}
	has := func(name string) bool {
		return slices.ContainsFunc(readState(fake).Images, func(im fakeImage) bool { return im.ID == name })
	}
	start := m.started.Unix()
	add("older-version", "0ld", start-100)   // the version this one replaced
	add("newer-version", "n3w", start+10000) // an update being prepared now

	// With the current build gone (say the standard image was rebuilt and
	// the build on it failed), the fallback is this version's build, not
	// the newer one of another version.
	withState(fake, func(st *fakeState) {
		for i := range st.Images {
			if slices.Contains(st.Images[i].Names, mine) {
				st.Images[i].Names = []string{customRepo + d.ID + ":older-same"}
			}
		}
	})
	m.refreshCustom(d.ID)
	if tag := m.Status().Images[0]; !tag.Ready {
		t.Fatal("no fallback")
	}
	m.mu.Lock()
	got := m.customLocked(d.ID).tag
	m.mu.Unlock()
	if got != customRepo+d.ID+":older-same" {
		t.Fatalf("fell back to %q, want this version's build", got)
	}
	m.PruneImages()
	if has("older-version") {
		t.Error("kept the replaced version's build")
	}
	if !has("newer-version") {
		t.Error("pruned the build an update is preparing")
	}
	// Only another version's build left: better than nothing.
	withState(fake, func(st *fakeState) {
		st.Images = slices.DeleteFunc(st.Images, func(im fakeImage) bool { return slices.Contains(im.Names, got) })
	})
	m.refreshCustom(d.ID)
	m.mu.Lock()
	got = m.customLocked(d.ID).tag
	m.mu.Unlock()
	if got != customRepo+d.ID+":newer-version" {
		t.Errorf("fell back to %q, want the only build there is", got)
	}
	m.PruneImages()
	if !has("newer-version") {
		t.Error("pruned the build projects run")
	}
}

// A standard image replaced while a custom one was being built on it: the
// custom one is built again on the new one.
func TestStandardImageChangesDuringABuild(t *testing.T) {
	m, fake := newFakeManager(t)
	if _, err := m.WaitImage(ImageDefault, 10*time.Second, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the standard build", idle(m, ""))
	os.WriteFile(filepath.Join(fake, "gate"), nil, 0o600)
	d, _ := m.PutImage(ImageDef{Name: "A", Containerfile: "RUN true"})
	waitFor(t, "the custom build to start", func() bool { return imageStatus(m, d.ID).Building })
	if err := m.Build(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the standard rebuild", func() bool { return !m.Status().Building })
	os.Remove(filepath.Join(fake, "gate"))
	waitFor(t, "the custom image on the new standard image", func() bool {
		st := imageStatus(m, d.ID)
		return !st.Building && st.Current
	})
	m.forgetImageIDs()
	if want := customTag(d, m.imageID(t.Context(), m.tag)); !slices.ContainsFunc(readState(fake).Images, func(im fakeImage) bool { return slices.Contains(im.Names, want) }) {
		t.Errorf("not built on the new standard image (%s)", want)
	}
}

// Preparing an update builds the custom images within a budget; the rest
// are left to the new version.
func TestPrepareBudget(t *testing.T) {
	m, fake := newFakeManager(t)
	if _, err := m.WaitImage(ImageDefault, 10*time.Second, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the standard build", idle(m, ""))
	m.mu.Lock()
	for _, n := range []string{"A", "B"} {
		_ = m.changeDefsLocked(func(defs *imageDefs) error {
			defs.Images = append(defs.Images, ImageDef{ID: "id" + n, Name: n, Containerfile: "RUN " + n})
			return nil
		})
	}
	m.mu.Unlock()
	var out strings.Builder
	m.buildImagesSync(&out, true, time.Nanosecond)
	if n := len(readState(fake).Builds); n != 1 || !strings.Contains(out.String(), "no time left") {
		t.Fatalf("%d builds with no time left:\n%s", n, out.String())
	}
	out.Reset()
	m.buildImagesSync(&out, true, time.Minute)
	if !strings.Contains(out.String(), `"A"`) || strings.Contains(out.String(), "warning") {
		t.Errorf("with time:\n%s", out.String())
	}
	out.Reset()
	m.buildImagesSync(&out, true, time.Minute)
	if strings.Count(out.String(), "up to date") != 2 {
		t.Errorf("built again although up to date:\n%s", out.String())
	}
}
