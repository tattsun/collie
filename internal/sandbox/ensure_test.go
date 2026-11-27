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
