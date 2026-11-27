package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeService "runs" whatever binary is installed: the new one ("new")
// behaves as newStarts says, the old one ("old") always comes up.
type fakeService struct {
	t         *testing.T
	o         applyOpts
	newStarts func(f *fakeService)
	events    []string
	active    bool
	restarts  int
	flapAfter int // State calls after which a restart is reported (0 = never)
	states    int
}

func (f *fakeService) Stop(string) error {
	f.events = append(f.events, "stop")
	f.active = false
	return nil
}

func (f *fakeService) Start(string) error {
	b, _ := os.ReadFile(f.o.exe)
	f.events = append(f.events, "start "+string(b))
	f.active = true
	if string(b) == "old" {
		_ = MarkRunning(f.o.data, f.o.from)
	} else if f.newStarts != nil {
		f.newStarts(f)
	}
	return nil
}

func (f *fakeService) State(string) (bool, int, error) {
	f.states++
	if f.flapAfter > 0 && f.states > f.flapAfter {
		return f.active, f.restarts + 1, nil
	}
	return f.active, f.restarts, nil
}

var fastTiming = timing{start: time.Second, stable: 20 * time.Millisecond, poll: 5 * time.Millisecond}

func setupApply(t *testing.T) applyOpts {
	dir := t.TempDir()
	o := applyOpts{data: filepath.Join(dir, "data"), exe: filepath.Join(dir, "bin", "collie"), unit: "collie.service",
		staged: filepath.Join(dir, "data", "update", "staged", "collie-0.3.0"), from: "0.2.0", to: "0.3.0"}
	for path, content := range map[string]string{
		o.exe: "old", o.staged: "new",
		filepath.Join(o.data, "state.json"): "old state", filepath.Join(o.data, "auth.json"): "old auth",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	o.sha, _ = fileSHA256(o.staged)
	return o
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func TestApplySwapsAndStarts(t *testing.T) {
	o := setupApply(t)
	svc := &fakeService{t: t, o: o, newStarts: func(f *fakeService) {
		_ = os.WriteFile(filepath.Join(o.data, "state.json"), []byte("new state"), 0o600)
		_ = MarkRunning(o.data, o.to)
	}}
	if err := apply(o, svc, fastTiming); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(svc.events, ", "); got != "stop, start new" {
		t.Errorf("events: %s", got)
	}
	if read(t, o.exe) != "new" || read(t, o.exe+".prev") != "old" {
		t.Errorf("exe %q, prev %q", read(t, o.exe), read(t, o.exe+".prev"))
	}
	if fi, err := os.Stat(o.exe); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("exe mode: %v %v", fi, err)
	}
	if read(t, filepath.Join(o.data, "state.json")) != "new state" {
		t.Error("data was restored after a successful update")
	}
	if read(t, filepath.Join(o.data, "update", "backup", "auth.json")) != "old auth" {
		t.Error("no backup")
	}
	if _, err := os.Stat(o.staged); !os.IsNotExist(err) {
		t.Error("staged binary left behind")
	}
}

func TestApplyRollsBackWhenTheNewVersionDoesNotStart(t *testing.T) {
	o := setupApply(t)
	svc := &fakeService{t: t, o: o, newStarts: func(f *fakeService) {
		// It rewrites the data, then crashes before serving.
		_ = os.WriteFile(filepath.Join(o.data, "state.json"), []byte("new format"), 0o600)
	}}
	err := apply(o, svc, fastTiming)
	if err == nil || !strings.Contains(err.Error(), "did not start") || !strings.Contains(err.Error(), "rolled back to 0.2.0") {
		t.Fatalf("err = %v", err)
	}
	if got := strings.Join(svc.events, ", "); got != "stop, start new, stop, start old" {
		t.Errorf("events: %s", got)
	}
	if read(t, o.exe) != "old" {
		t.Errorf("exe %q after rollback", read(t, o.exe))
	}
	if read(t, filepath.Join(o.data, "state.json")) != "old state" || read(t, filepath.Join(o.data, "auth.json")) != "old auth" {
		t.Error("data not restored")
	}
}

func TestApplyRollsBackWhenTheNewVersionKeepsRestarting(t *testing.T) {
	o := setupApply(t)
	svc := &fakeService{t: t, o: o, flapAfter: 1, newStarts: func(f *fakeService) { _ = MarkRunning(o.data, o.to) }}
	err := apply(o, svc, fastTiming)
	if err == nil || !strings.Contains(err.Error(), "stopped right after starting") {
		t.Fatalf("err = %v", err)
	}
	if read(t, o.exe) != "old" {
		t.Errorf("exe %q after rollback", read(t, o.exe))
	}
}

func TestApplyRefusesAChangedDownload(t *testing.T) {
	o := setupApply(t)
	_ = os.WriteFile(o.staged, []byte("swapped"), 0o755)
	svc := &fakeService{t: t, o: o}
	if err := apply(o, svc, fastTiming); err == nil {
		t.Fatal("installed a binary that does not match its checksum")
	}
	if len(svc.events) != 0 || read(t, o.exe) != "old" {
		t.Errorf("touched the running server: %v, exe %q", svc.events, read(t, o.exe))
	}
}

func TestApplyIgnoresTheOldVersionsMarker(t *testing.T) {
	o := setupApply(t)
	// The old server's marker is still there; the new version never starts.
	_ = MarkRunning(o.data, o.from)
	svc := &fakeService{t: t, o: o}
	if err := apply(o, svc, fastTiming); err == nil {
		t.Fatal("took the old version's marker for the new one")
	}
}
