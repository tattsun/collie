package preview

import (
	"encoding/binary"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"
)

// hexAddr writes an address the way /proc/net/tcp does on this machine.
func hexAddr(a string) string {
	b := netip.MustParseAddr(a).AsSlice()
	var sb strings.Builder
	for i := 0; i < len(b); i += 4 {
		w := binary.NativeEndian.Uint32(b[i : i+4])
		sb.WriteString(fmtHex(w))
	}
	return sb.String()
}

func fmtHex(w uint32) string {
	const digits = "0123456789ABCDEF"
	out := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		out[i] = digits[w&0xf]
		w >>= 4
	}
	return string(out)
}

func TestParseProcNet(t *testing.T) {
	head := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	row := func(addr string, port string, st string, uid string, inode string) string {
		return "   0: " + hexAddr(addr) + ":" + port + " " + hexAddr(addr) + ":0000 " + st + " 00000000:00000000 00:00000000 00000000  " + uid + "        0 " + inode + " 1 0000000000000000 100 0 0 10 0\n"
	}
	v4 := head + row("127.0.0.1", "1435", "0A", "1000", "111") + // 5173
		row("0.0.0.0", "0BB8", "0A", "1000", "222") + // 3000
		row("127.0.0.1", "1F90", "01", "1000", "333") // established: not a server
	got := parseProcNet(v4)
	want := []socket{
		{addr: netip.MustParseAddr("127.0.0.1"), port: 5173, uid: 1000, inode: 111},
		{addr: netip.MustParseAddr("0.0.0.0"), port: 3000, uid: 1000, inode: 222},
	}
	if !slices.Equal(got, want) {
		t.Errorf("tcp: %+v\nwant %+v", got, want)
	}
	v6 := head + row("::1", "1435", "0A", "0", "444") + row("::", "1F90", "0A", "1000", "555")
	got = parseProcNet(v6)
	want = []socket{
		{addr: netip.MustParseAddr("::1"), port: 5173, uid: 0, inode: 444},
		{addr: netip.MustParseAddr("::"), port: 8080, uid: 1000, inode: 555},
	}
	if !slices.Equal(got, want) {
		t.Errorf("tcp6: %+v\nwant %+v", got, want)
	}
	if got := parseProcNet("garbage\nmore garbage 0A\n"); len(got) != 0 {
		t.Errorf("garbage: %+v", got)
	}
}

func TestDialAddrStaysLocal(t *testing.T) {
	s := func(a string, port int) socket { return socket{addr: netip.MustParseAddr(a), port: port} }
	socks := []socket{s("::1", 5173), s("0.0.0.0", 3000), s("::", 3000), s("::", 8080), s("10.0.2.100", 9000)}
	cases := map[int]string{
		5173: "[::1]:5173",      // Vite on Node ≥ 17 often listens on ::1 only
		3000: "127.0.0.1:3000",  // every address
		8080: "[::1]:8080",      // every IPv6 (maybe v6 only) address
		9000: "10.0.2.100:9000", // one of the machine's own addresses
	}
	for port, want := range cases {
		if got, err := dialAddr(socks, port); err != nil || got != want {
			t.Errorf("port %d: %q, %v; want %q", port, got, err, want)
		}
	}
	// Nothing else can come out: ports nobody listens on aren't tried
	// anywhere, nor numbers that are no port (5173 + 65536 must not wrap
	// around to 5173).
	for _, port := range []int{22, 5173 + 65536, 0, -1} {
		if got, err := dialAddr(socks, port); err == nil {
			t.Errorf("port %d: dialed %s", port, got)
		}
	}
}

func TestMergeAndLabels(t *testing.T) {
	socks := []socket{
		{addr: netip.MustParseAddr("::"), port: 3000, inode: 1},
		{addr: netip.MustParseAddr("0.0.0.0"), port: 3000, inode: 2},
		{addr: netip.MustParseAddr("127.0.0.1"), port: 9222, inode: 3},
		{addr: netip.MustParseAddr("127.0.0.1"), port: 5173, inode: 4},
	}
	procs := map[uint64]proc{
		1: {pid: 10, cmd: shortCmd([]string{"/usr/bin/node", "/work/node_modules/.bin/next", "dev"})},
		2: {pid: 10, cmd: shortCmd([]string{"/usr/bin/node", "/work/node_modules/.bin/next", "dev"})},
		3: {pid: 11, hide: isHidden([]string{"/opt/ms-playwright/chromium-1/chrome-linux/chrome", "--remote-debugging-port=9222"}, "chrome")},
		4: {pid: 12, cmd: shortCmd([]string{"node", "/work/node_modules/vite/bin/vite.js", "--host", "0.0.0.0"})},
	}
	got := merge(socks, procs, func(_ socket, p proc) bool { return p.hide })
	if len(got) != 2 || got[0].Port != 3000 || got[0].Addr != "0.0.0.0" || got[0].Cmd != "node next dev" ||
		got[1].Port != 5173 || got[1].Cmd != "node vite.js" {
		t.Errorf("merge: %+v", got)
	}
	if !isHidden([]string{"/usr/local/lib/collie/collie", "net-bridge"}, "collie") {
		t.Error("Collie's own bridge is listed")
	}
	if isHidden([]string{"python3", "-m", "http.server"}, "python3") {
		t.Error("a plain server is hidden")
	}
}

func TestParseLsof(t *testing.T) {
	out := "p501\ncnode\nf23\ntIPv4\nn127.0.0.1:5173\nf24\ntIPv6\nn[::1]:5173\np777\ncPython\nf3\ntIPv6\nn*:8000\nf4\ntIPv4\nn*:8001\n"
	socks, procs := parseLsof(out, 501)
	var got []string
	for _, s := range socks {
		got = append(got, s.addr.String()+"/"+procs[s.inode].cmd)
	}
	want := []string{"127.0.0.1/node", "::1/node", "::/Python", "0.0.0.0/Python"}
	if !slices.Equal(got, want) {
		t.Errorf("lsof: %v, want %v", got, want)
	}
}

func TestCleanSetCookie(t *testing.T) {
	cases := map[string]string{
		"app=1; Path=/; HttpOnly":                  "app=1; Path=/; HttpOnly",
		"app=1; Domain=example.com; Path=/":        "app=1; Path=/",
		"app=1; domain=.example.com":               "app=1",
		"collie_sess=x; Path=/":                    "",
		" collie_preview=x":                        "",
		"collie_auth=x; Max-Age=0":                 "",
		"__Host-app=1; Secure; Path=/; SameSite=L": "__Host-app=1; Secure; Path=/; SameSite=L",
	}
	for in, want := range cases {
		if got := cleanSetCookie(in); got != want {
			t.Errorf("cleanSetCookie(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFilterCookies(t *testing.T) {
	h := http.Header{}
	h.Add("Cookie", "a=1; collie_sess=s; b=x=y")
	h.Add("Cookie", "collie_preview=p;collie_auth=t; c=; _oauth2_proxy_0=x; _oauth2_proxy_csrf=y; _pomerium=z; CF_Authorization=w")
	filterCookies(h)
	if got := h.Values("Cookie"); len(got) != 1 || got[0] != "a=1; b=x=y; c=" {
		t.Errorf("cookies: %q", got)
	}
	h = http.Header{"Cookie": {"collie_sess=s"}}
	filterCookies(h)
	if _, ok := h["Cookie"]; ok {
		t.Error("empty Cookie header left")
	}
}

func TestRewriteLocation(t *testing.T) {
	o := "https://preview.example.com"
	cases := map[string]string{
		"http://localhost:5173/a?b=1":  o + "/a?b=1",
		"http://127.0.0.1:5173/":       o + "/",
		"http://[::1]:5173/x":          o + "/x",
		"/relative":                    "/relative",
		"http://localhost:3000/other":  "http://localhost:3000/other", // another port: left as is
		"https://accounts.example/cb":  "https://accounts.example/cb",
		"http://localhost/no-port":     "http://localhost/no-port",
		"javascript://localhost:5173/": "javascript://localhost:5173/",
	}
	for in, want := range cases {
		if got := rewriteLocation(in, o, 5173); got != want {
			t.Errorf("rewriteLocation(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAllowCSP(t *testing.T) {
	csp := allowCSP("https://box.ts.net:8443", []string{"github.com", "*.npmjs.org", "example.com:443", "203.0.113.7", "192.168.1.0/24", "2001:db8::1", "[2001:db8::1]:443"})
	want := "default-src 'self' wss://box.ts.net:8443 github.com *.npmjs.org example.com:443 203.0.113.7 data: blob: 'unsafe-inline' 'unsafe-eval'; " +
		"form-action 'self' github.com *.npmjs.org example.com:443 203.0.113.7; base-uri 'self'"
	if csp != want {
		t.Errorf("csp:\n%s\nwant\n%s", csp, want)
	}
	// An empty allowlist: the page reaches its own origin only.
	if csp := allowCSP("http://10.0.0.5:7879", []string{}); !strings.HasPrefix(csp, "default-src 'self' ws://10.0.0.5:7879 data:") {
		t.Errorf("empty list: %s", csp)
	}
	// Only for projects on an allowlist.
	h := http.Header{}
	cleanResponse(h, "https://p.example", 80, nil)
	if h.Get("Content-Security-Policy") != "" {
		t.Error("CSP added for a project without an allowlist")
	}
	cleanResponse(h, "https://p.example", 80, []string{"github.com"})
	if !strings.Contains(h.Get("Content-Security-Policy"), "github.com") {
		t.Error("no CSP for a project on an allowlist")
	}
}

func TestTokens(t *testing.T) {
	now := time.Now()
	p := &Proxy{Now: func() time.Time { return now }, Resolve: func(Grant) (Target, error) { return Target{}, nil }}
	g := Grant{UserID: "u", Login: "h", SessionID: "s", Port: 3000}
	tok := p.Mint(g)
	if tok == "" || len(tok) < 40 {
		t.Fatalf("token %q", tok)
	}
	if got, ok := p.redeem(tok); !ok || got != g {
		t.Fatalf("redeem: %+v %v", got, ok)
	}
	if _, ok := p.redeem(tok); ok {
		t.Error("a token worked twice")
	}
	tok = p.Mint(g)
	now = now.Add(tokenTTL)
	if _, ok := p.redeem(tok); ok {
		t.Error("an expired token worked")
	}
	// One user can't pile up tokens.
	for range 3 * maxTokensPerUser {
		p.Mint(g)
	}
	p.mu.Lock()
	n := len(p.tokens)
	p.mu.Unlock()
	if n > maxTokensPerUser {
		t.Errorf("%d tokens kept for one user", n)
	}
}

func TestSameOrigin(t *testing.T) {
	o := "https://box.ts.net:8443"
	req := func(method, origin string, upgrade bool) *http.Request {
		r, _ := http.NewRequest(method, "/", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if upgrade {
			r.Header.Set("Connection", "keep-alive, Upgrade")
			r.Header.Set("Upgrade", "websocket")
		}
		return r
	}
	ok := []*http.Request{req("POST", o, false), req("POST", "", false), req("GET", "https://elsewhere", false),
		req("GET", o, true), req("POST", "HTTPS://BOX.TS.NET:8443", false)}
	bad := []*http.Request{req("POST", "https://box.ts.net", false), req("PUT", "null", false),
		req("GET", "https://box.ts.net", true), req("DELETE", "https://evil.example", false)}
	for _, r := range ok {
		if !sameOrigin(r, o) {
			t.Errorf("%s from %q refused", r.Method, r.Header.Get("Origin"))
		}
	}
	for _, r := range bad {
		if sameOrigin(r, o) {
			t.Errorf("%s from %q (upgrade %v) let through", r.Method, r.Header.Get("Origin"), isUpgrade(r))
		}
	}
}
