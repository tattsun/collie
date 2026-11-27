package egress

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	good := map[string]string{
		"Example.COM":                    "example.com",
		"example.com.":                   "example.com",
		"*.example.com":                  "*.example.com",
		"example.com:443":                "example.com:443",
		"https://github.com/foo/bar.git": "github.com",
		"https://example.com:8443/x":     "example.com:8443",
		"github.com/foo":                 "github.com",
		"git@github.com":                 "github.com",
		"203.0.113.7":                    "203.0.113.7",
		"203.0.113.7:22":                 "203.0.113.7:22",
		"192.168.1.0/24":                 "192.168.1.0/24",
		"192.168.1.9/24":                 "192.168.1.0/24",
		"2001:db8::1":                    "2001:db8::1",
		"[2001:db8::1]:443":              "[2001:db8::1]:443",
		"my_host.internal":               "my_host.internal",
	}
	for in, want := range good {
		if got, err := Normalize(in); err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "*", "*.com", "exa mple.com", "example.com:0", "example.com:99999", "-bad.com", "ex*ample.com", "日本.jp"} {
		if got, err := Normalize(in); err == nil {
			t.Errorf("Normalize(%q) = %q, want an error", in, got)
		}
	}
	list, err := NormalizeList([]string{"github.com", " ", "# comment", "GitHub.com", "pypi.org # packages"})
	if err != nil || !slices.Equal(list, []string{"github.com", "pypi.org"}) {
		t.Errorf("NormalizeList = %q, %v", list, err)
	}
	if _, err := NormalizeList([]string{"ok.com", "not ok"}); err == nil {
		t.Error("a bad entry in a list was accepted")
	}
}

func TestAllowed(t *testing.T) {
	list := []string{"github.com", "*.githubusercontent.com", "example.org:443", "203.0.113.7", "192.168.1.0/24"}
	cases := []struct {
		host string
		port int
		want bool
	}{
		{"github.com", 443, true},
		{"GitHub.com.", 22, true},
		{"api.github.com", 443, false}, // not a wildcard
		{"evilgithub.com", 443, false},
		{"github.com.evil.net", 443, false},
		{"raw.githubusercontent.com", 443, true},
		{"githubusercontent.com", 443, false}, // the wildcard is for subdomains
		{"xgithubusercontent.com", 443, false},
		{"example.org", 443, true},
		{"example.org", 80, false},
		{"203.0.113.7", 8080, true},
		{"[::ffff:203.0.113.7]", 443, true},
		{"203.0.113.8", 443, false},
		{"192.168.1.20", 445, true},
		{"192.168.2.20", 445, false},
		{"api.anthropic.com", 443, true}, // the agents' own services
		{"chatgpt.com", 443, true},
		{"anthropic.com.evil.net", 443, false},
	}
	for _, c := range cases {
		if got := Allowed(list, c.host, c.port); got != c.want {
			t.Errorf("Allowed(%s:%d) = %v, want %v", c.host, c.port, got, c.want)
		}
	}
	if Allowed(nil, "github.com", 443) {
		t.Error("an empty list let github.com through")
	}
}

func TestAddressChecks(t *testing.T) {
	p := NewProxy()
	check := func(addr string, private bool) bool {
		ctx := context.WithValue(context.Background(), privateOK{}, private)
		return p.checkAddr(ctx, "tcp", addr, nil) == nil
	}
	for _, a := range []string{"140.82.112.3:443", "[2606:4700::1111]:443", "[64:ff9b::8c52:7003]:443"} {
		if !check(a, false) {
			t.Errorf("public %s was blocked", a)
		}
	}
	never := []string{"127.0.0.1:7878", "[::1]:7878", "169.254.169.254:80", "[fe80::1]:22", "0.0.0.0:80", "0.1.2.3:80",
		"224.0.0.1:80", "255.255.255.255:80", "[::ffff:127.0.0.1]:7878", "[64:ff9b::7f00:1]:7878", "240.0.0.1:80"}
	for _, a := range never {
		if check(a, true) {
			t.Errorf("%s was reachable, even listed", a)
		}
	}
	private := []string{"10.1.2.3:22", "172.16.0.1:80", "192.168.1.20:445", "100.100.100.100:53", "[fd7a:115c:a1e0::1]:80", "[64:ff9b::c0a8:114]:445"}
	for _, a := range private {
		if check(a, false) {
			t.Errorf("private %s was reachable through a host name", a)
		}
		if !check(a, true) && !strings.HasPrefix(a, "[64:ff9b") {
			t.Errorf("private %s was blocked although listed as an address", a)
		}
	}
	// this server's own addresses
	if addrs, _ := net.InterfaceAddrs(); len(addrs) > 0 {
		for _, ad := range addrs {
			if pn, ok := ad.(*net.IPNet); ok {
				if x, ok := netip.AddrFromSlice(pn.IP); ok && x.Unmap().IsGlobalUnicast() && !privateAddr(x.Unmap()) {
					if check(netip.AddrPortFrom(x.Unmap(), 443).String(), true) {
						t.Errorf("this server's own address %s was reachable", x)
					}
				}
			}
		}
	}
}

// testProxy serves p on a Unix socket and returns an HTTP client using it.
func testProxy(t *testing.T, p *Proxy, project string) (*http.Client, string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "proxy.sock")
	if err := p.Listen(sock, project, "u1"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close(project, "") })
	tr := &http.Transport{
		Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: "proxy.invalid:3128"}),
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}, sock
}

func TestProxyForwardsOnlyAllowedHosts(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello %s via %s", r.URL.Path, r.Host)
	}))
	defer backend.Close()
	tlsBackend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "secure hello")
	}))
	defer tlsBackend.Close()
	_, port, _ := net.SplitHostPort(backend.Listener.Addr().String())
	_, tlsPort, _ := net.SplitHostPort(tlsBackend.Listener.Addr().String())

	p := NewProxy()
	p.anyAddr = true // the backends are on the loopback
	allow := []string{"localhost"}
	restricted := true
	p.Rules = func(id string) ([]string, bool) { return allow, id == "p1" && restricted }
	c, _ := testProxy(t, p, "p1")

	res, err := c.Get("http://localhost:" + port + "/x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(b) != "hello /x via localhost:"+port {
		t.Fatalf("allowed host: %d %q", res.StatusCode, b)
	}

	// HTTPS goes through a tunnel.
	tc := c.Transport.(*http.Transport)
	tc.TLSClientConfig = &tls.Config{RootCAs: tlsBackend.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs, ServerName: "example.com"}
	res, err = c.Get("https://localhost:" + tlsPort + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if string(b) != "secure hello" {
		t.Fatalf("tunnel: %q", b)
	}

	// Not on the list: refused, with a reason the agent can act on.
	res, err = c.Get("http://127.0.0.1:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 403 || !strings.Contains(string(b), "allowlist") {
		t.Fatalf("unlisted host: %d %q", res.StatusCode, b)
	}
	if _, err := c.Get("https://127.0.0.1:" + tlsPort + "/"); err == nil {
		t.Fatal("tunnel to an unlisted host")
	}

	// The list applies as it is now; a project without one gets nothing.
	allow = nil
	if res, err := c.Get("http://localhost:" + port + "/x"); err != nil || res.StatusCode != 403 {
		t.Fatalf("removed host still allowed: %v %v", res, err)
	}
	allow, restricted = []string{"localhost"}, false
	if res, err := c.Get("http://localhost:" + port + "/x"); err != nil || res.StatusCode != 403 {
		t.Fatalf("a project without an allowlist got through: %v %v", res, err)
	}

	recent := p.Recent("p1")
	got := map[string]int{}
	for _, e := range recent {
		got[fmt.Sprintf("%s:%d %s", e.Host, e.Port, e.Blocked)] = e.Count
	}
	want := map[string]int{
		"localhost:" + port + " ":        1,
		"localhost:" + tlsPort + " ":     1,
		"127.0.0.1:" + port + " list":    1,
		"127.0.0.1:" + tlsPort + " list": 1,
		"localhost:" + port + " list":    2,
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("recent %q = %d, want %d (all: %v)", k, got[k], n, got)
		}
	}
	if len(p.Recent("other")) != 0 {
		t.Error("another project sees this one's connections")
	}
}

func TestProxyRefusesLocalAddresses(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "secret") }))
	defer backend.Close()
	_, port, _ := net.SplitHostPort(backend.Listener.Addr().String())
	p := NewProxy()
	// Even listed, a name that points at the loopback (Collie's own API, a
	// database on the server) stays out of reach.
	p.Rules = func(string) ([]string, bool) { return []string{"localhost", "127.0.0.1"}, true }
	c, _ := testProxy(t, p, "p1")
	for _, u := range []string{"http://localhost:" + port + "/", "http://127.0.0.1:" + port + "/"} {
		res, err := c.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 403 || strings.Contains(string(b), "secret") {
			t.Fatalf("%s: %d %q", u, res.StatusCode, b)
		}
	}
	if _, err := c.Get("https://localhost:" + port + "/"); err == nil {
		t.Fatal("tunnel to the loopback")
	}
	for _, e := range p.Recent("p1") {
		if e.Blocked != BlockedAddress {
			t.Errorf("recorded %+v, want blocked by address", e)
		}
	}
}

func TestProxyRejectsNonProxyRequests(t *testing.T) {
	p := NewProxy()
	p.Rules = func(string) ([]string, bool) { return []string{"example.com"}, true }
	_, sock := testProxy(t, p, "p1")
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "GET /etc/passwd HTTP/1.1\r\nHost: example.com\r\n\r\n")
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 400 {
		t.Fatalf("origin-form request: %d", res.StatusCode)
	}
}

func TestBlockedTunnelSaysWhy(t *testing.T) {
	p := NewProxy()
	p.Rules = func(string) ([]string, bool) { return []string{"github.com"}, true }
	_, sock := testProxy(t, p, "p1")
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 403 || !strings.Contains(res.Status, "allowlist") || !strings.Contains(string(b), "Project → Settings → Network") {
		t.Fatalf("blocked tunnel: %q %q", res.Status, b)
	}
}

func TestBridge(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "through") }))
	defer backend.Close()
	_, port, _ := net.SplitHostPort(backend.Listener.Addr().String())
	p := NewProxy()
	p.anyAddr = true
	p.Rules = func(string) ([]string, bool) { return []string{"localhost"}, true }
	_, sock := testProxy(t, p, "p1")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serveBridge(ln, sock)
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: ln.Addr().String()})}, Timeout: 10 * time.Second}
	res, err := c.Get("http://localhost:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if string(b) != "through" {
		t.Fatalf("through the bridge: %q", b)
	}
}

func TestListenAgainAfterSocketRemoved(t *testing.T) {
	p := NewProxy()
	p.Rules = func(string) ([]string, bool) { return nil, true }
	_, sock := testProxy(t, p, "p1")
	if err := p.Listen(sock, "p1", "u1"); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(sock)
	if err := p.Listen(sock, "p1", "u1"); err != nil {
		t.Fatal(err)
	}
	if c, err := net.Dial("unix", sock); err != nil {
		t.Fatalf("not listening again: %v", err)
	} else {
		c.Close()
	}
}
