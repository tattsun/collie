package sandbox

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/model"
)

// A podman that knows no containers and takes runDelay to create one.
func fakePodman(t *testing.T, runDelay string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "podman")
	script := `#!/bin/sh
for a in "$@"; do
  case "$a" in
    inspect) [ "$prev" = container ] && exit 125 ;;
    run) sleep ` + runDelay + `; exit 0 ;;
  esac
  prev="$a"
done
exit 0
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEnsureReportsASlowStart(t *testing.T) {
	old := slowStartAfter
	slowStartAfter = 200 * time.Millisecond
	defer func() { slowStartAfter = old }()

	for _, c := range []struct {
		delay string
		slow  bool
	}{{"0", false}, {"1", true}} {
		m := newTestManager(t)
		m.cfg.Podman, m.available, m.stale = fakePodman(t, c.delay), true, map[string]bool{}
		var called atomic.Bool
		sp := Spec{Project: model.Project{ID: "p1", Path: t.TempDir()}, UserID: "u1", OnSlowStart: func() { called.Store(true) }}
		if err := m.Ensure(sp, true); err != nil {
			t.Fatalf("run taking %ss: %v", c.delay, err)
		}
		if called.Load() != c.slow {
			t.Errorf("run taking %ss: slow-start notice %v, want %v", c.delay, called.Load(), c.slow)
		}
	}
}

// While start-up moves files that containers mount, nothing starts.
func TestEnsureWaitsForStartUp(t *testing.T) {
	m := newTestManager(t)
	m.cfg.Podman, m.available, m.stale = fakePodman(t, "0"), true, map[string]bool{}
	m.HoldStarts()
	done := make(chan error, 1)
	go func() {
		done <- m.Ensure(Spec{Project: model.Project{ID: "p1", Path: t.TempDir()}, UserID: "u1"}, true)
	}()
	select {
	case err := <-done:
		t.Fatalf("Ensure didn't wait for start-up: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	m.ReleaseStarts()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Ensure still waits after start-up")
	}
	m.ReleaseStarts() // twice is fine
}
