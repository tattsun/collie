package sandbox

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tattsun/collie/internal/egress"
	"github.com/tattsun/collie/internal/model"
)

// Runs a real container on an allowlist:
// COLLIE_INTEGRATION=1 go test ./internal/sandbox -run Integration
func TestIntegrationNetworkAllowlist(t *testing.T) {
	if os.Getenv("COLLIE_INTEGRATION") == "" {
		t.Skip("set COLLIE_INTEGRATION=1 (needs podman, the agent image and go)")
	}
	sb := New(Config{Podman: "podman", DataDir: t.TempDir()})
	if !sb.Available() || !sb.Status().Ready {
		t.Skip("podman or the agent image is not available")
	}
	// The container's main process is Collie's own binary (net-bridge).
	bin := filepath.Join(t.TempDir(), "collie")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/tattsun/collie").CombinedOutput(); err != nil {
		t.Fatalf("build collie: %v\n%s", err, out)
	}

	// A service on the server's loopback (like Collie's own API).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var hits atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			c.Close()
		}
	}()

	var mu sync.Mutex
	p := model.Project{ID: "netint" + strconv.Itoa(os.Getpid()), Name: "netint", Path: t.TempDir(), Runtime: "container",
		Network: model.NetAllowlist, NetAllow: []string{"github.com", "127.0.0.1"}}
	sb.SetNetRules(func(id string) (model.Project, bool) {
		mu.Lock()
		defer mu.Unlock()
		return p, id == p.ID
	})
	uid := "netintegration"
	t.Cleanup(func() { sb.Remove(p.ID) })
	if err := sb.Ensure(Spec{Project: p, UserID: uid, CollieBin: bin}, true); err != nil {
		t.Fatal(err)
	}
	n := 0
	run := func(cmd ...string) (string, error) {
		n++
		argv, _ := sb.Exec(p.ID, uid, p.Path, "netint-"+strconv.Itoa(n), false, nil, cmd)
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	status := func(url string, extra ...string) string {
		args := append([]string{"curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "-m", "20", "--retry", "3", "--retry-connrefused"}, extra...)
		out, _ := run(append(args, url)...)
		return out
	}

	ifaces, _ := run("sh", "-c", "tail -n +3 /proc/net/dev | cut -d: -f1 | xargs; echo $HTTPS_PROXY; cat /proc/1/cmdline /proc/2/cmdline | tr '\\0' ' '")
	t.Logf("in the container: %s", ifaces)
	if got := status("https://github.com/"); got != "200" {
		t.Fatalf("listed host: %q", got)
	}
	if got := status("https://example.com/"); !strings.Contains(got, "403") {
		t.Fatalf("unlisted host: %q", got)
	} else {
		t.Logf("unlisted host: %s", got)
	}
	// No way around the proxy: the container has no network of its own.
	if out, err := run("curl", "-sS", "--noproxy", "*", "-m", "5", "https://140.82.112.3/"); err == nil {
		t.Fatalf("direct connection succeeded: %q", out)
	} else {
		t.Logf("direct connection: %s", out)
	}
	// Even listed, the server's loopback stays out of reach.
	if got := status("http://"+ln.Addr().String()+"/", "--noproxy", ""); !strings.Contains(got, "403") || hits.Load() != 0 {
		t.Fatalf("server loopback: %q, %d connection(s)", got, hits.Load())
	}
	// The list applies at once, without restarting anything.
	mu.Lock()
	p.NetAllow = append(p.NetAllow, "example.com")
	mu.Unlock()
	if got := status("https://example.com/"); got != "200" {
		t.Fatalf("newly listed host: %q", got)
	}

	// The container is labelled with its kind of network, so switching
	// re-creates it even while it is in use.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if st := sb.inspect(ctx, Name(p.ID, uid)); st.network != model.NetAllowlist {
		t.Errorf("container's network label: %q", st.network)
	}
	// A project on the internet is as before: direct connections, no proxy.
	open := model.Project{ID: p.ID + "o", Name: "netint-open", Path: t.TempDir(), Runtime: "container"}
	t.Cleanup(func() { sb.Remove(open.ID) })
	if err := sb.Ensure(Spec{Project: open, UserID: uid, CollieBin: bin}, true); err != nil {
		t.Fatal(err)
	}
	if st := sb.inspect(ctx, Name(open.ID, uid)); st.network != "" {
		t.Errorf("open container's network label: %q", st.network)
	}
	argv, _ := sb.Exec(open.ID, uid, open.Path, "netint-open", false, nil,
		[]string{"sh", "-c", "echo \"proxy=$HTTPS_PROXY\"; curl -sS -o /dev/null -w '%{http_code}' -m 20 https://example.com/"})
	if out, _ := exec.Command(argv[0], argv[1:]...).CombinedOutput(); strings.TrimSpace(string(out)) != "proxy=\n200" {
		t.Errorf("open container: %q", out)
	}

	seen := map[string]bool{}
	for _, e := range sb.NetworkLog(p.ID) {
		seen[e.Host+" "+e.Blocked] = true
	}
	for _, want := range []string{"github.com ", "example.com " + egress.BlockedList, "example.com ", "127.0.0.1 " + egress.BlockedAddress} {
		if !seen[want] {
			t.Errorf("log lacks %q: %+v", want, sb.NetworkLog(p.ID))
		}
	}
}
