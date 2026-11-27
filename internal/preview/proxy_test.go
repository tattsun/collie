package preview

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testProxy is a Proxy on one shared origin in front of servers on this
// machine; gone makes every grant stop holding.
func testProxy(t *testing.T) (*Proxy, *atomic.Bool) {
	t.Helper()
	var gone atomic.Bool
	up := HostUpstream(Filter{UID: -1})
	p := &Proxy{
		Resolve: func(g Grant) (Target, error) {
			if gone.Load() {
				return Target{}, ErrGone
			}
			return Target{Key: g.Target, Upstream: up}, nil
		},
		Origin: func(*http.Request, int) (string, string, bool) { return "https://p.test", "", true },
	}
	return p, &gone
}

func listen(t *testing.T, h http.Handler) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// openIn opens a grant in a browser holding cookie (if any), returning the
// response and the new cookie.
func openIn(t *testing.T, p *Proxy, g Grant, cookie string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	r := httptest.NewRequest("GET", OpenPath+"?t="+p.Mint(g), nil)
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: CookieName(g.Slot), Value: cookie})
	}
	w := httptest.NewRecorder()
	p.Slot(g.Slot).ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName(g.Slot) {
			return w, c.Value
		}
	}
	t.Fatalf("open: %d %s", w.Code, w.Body.String())
	return nil, ""
}

// On the shared origin, switching to another target clears what the last
// one left in the browser; reopening the same one doesn't.
func TestOpenClearsWhatAnotherTargetLeft(t *testing.T) {
	p, _ := testProxy(t)
	a := Grant{UserID: "u", SessionID: "s1", Port: 1, Target: "c:p1/u"}
	w, c := openIn(t, p, a, "")
	if w.Header().Get("Clear-Site-Data") == "" || !strings.Contains(w.Body.String(), `url=/`) {
		t.Errorf("first open: %d %v", w.Code, w.Header())
	}
	a2 := a
	a2.SessionID, a2.Port = "s2", 2 // another session in the same container
	w, c = openIn(t, p, a2, c)
	if w.Code != http.StatusSeeOther || w.Header().Get("Clear-Site-Data") != "" {
		t.Errorf("same container: %d %v", w.Code, w.Header())
	}
	b := Grant{UserID: "u", SessionID: "s3", Port: 1, Target: "c:p2/u"}
	w, _ = openIn(t, p, b, c)
	if w.Header().Get("Clear-Site-Data") != `"cache", "storage"` {
		t.Errorf("another container: %d %v", w.Code, w.Header())
	}
}

// The server's informational responses never reach the browser: they
// would bypass the response filter.
func TestNoInformationalResponses(t *testing.T) {
	p, _ := testProxy(t)
	port := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "collie_sess=planted; Domain=p.test")
		w.Header().Set("Strict-Transport-Security", "max-age=0")
		w.Header().Set("Link", "</x.js>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Set-Cookie")
		w.Header().Del("Strict-Transport-Security")
		w.Header().Del("Link")
		fmt.Fprint(w, "ok")
	}))
	_, c := openIn(t, p, Grant{UserID: "u", Port: port, Target: "host"}, "")
	srv := httptest.NewServer(p)
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: p.test\r\nCookie: %s=%s\r\nConnection: close\r\n\r\n", CookieName(0), c)
	raw, _ := io.ReadAll(conn)
	if strings.Contains(string(raw), "103") || strings.Contains(string(raw), "collie_sess") || strings.Contains(string(raw), "Strict-Transport") {
		t.Errorf("1xx passed on:\n%s", raw)
	}
	if !strings.HasSuffix(string(raw), "ok") {
		t.Errorf("response:\n%s", raw)
	}
}

// Streams (server-sent events, WebSockets) end when the grant does.
func TestStreamsEndWithTheGrant(t *testing.T) {
	p, gone := testProxy(t)
	port := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; ; i++ {
			if _, err := fmt.Fprintf(w, "data: %d\n\n", i); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	_, c := openIn(t, p, Grant{UserID: "u", Port: port, Target: "host"}, "")
	srv := httptest.NewServer(p)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/events", nil)
	req.AddCookie(&http.Cookie{Name: CookieName(0), Value: c})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	if line, err := br.ReadString('\n'); err != nil || !strings.HasPrefix(line, "data:") {
		t.Fatalf("stream: %q %v", line, err)
	}
	gone.Store(true)
	p.SweepNow()
	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, br)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived its grant")
	}
}

// One user can't push everyone else's previews out.
func TestGrantsPerUser(t *testing.T) {
	p, _ := testProxy(t)
	_, other := openIn(t, p, Grant{UserID: "other", Port: 1, Target: "host"}, "")
	for i := range 3 * maxGrantsPerUser {
		openIn(t, p, Grant{UserID: "flood", Port: i + 1, Target: "host"}, "")
	}
	p.mu.Lock()
	n := 0
	for _, g := range p.grants {
		if g.UserID == "flood" {
			n++
		}
	}
	_, kept := p.grants[hashOf(other)]
	p.mu.Unlock()
	if n > maxGrantsPerUser || !kept {
		t.Errorf("%d grants for one user; the other user's kept: %v", n, kept)
	}
}

// A server that doesn't read what it is sent must not hold up the other
// connections through the same bridge.
func TestBridgeStalledUploadDoesNotBlockOthers(t *testing.T) {
	stall, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stall.Close()
	go func() {
		for {
			c, err := stall.Accept()
			if err != nil {
				return
			}
			defer c.Close() // accepted, never read
		}
	}()
	port := echoServer(t)
	b, _ := pipeBridge(t, Filter{UID: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sc, err := b.Dial(ctx, stall.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	go func() { _, _ = sc.Write(make([]byte, 8<<20)) }()
	time.Sleep(300 * time.Millisecond)
	c, err := b.Dial(ctx, port)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	payload := strings.Repeat("x", 2<<20) + "\n"
	go func() { _, _ = io.WriteString(c, payload) }()
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		got, err := bufio.NewReaderSize(c, 4<<20).ReadString('\n')
		ch <- result{len(got), err}
	}()
	select {
	case r := <-ch:
		if r.err != nil || r.n != len(payload)+len("echo ") {
			t.Errorf("echo through the bridge next to a stalled upload: %d bytes, %v", r.n, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Error("a stalled upload held up another connection through the bridge")
		sc.Close()
		c.Close()
	}
}

// Responses the browser stops reading must not hold up the others either.
func TestBridgeUnreadDownloadsDoNotBlockOthers(t *testing.T) {
	flood, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer flood.Close()
	go func() {
		for {
			c, err := flood.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = c.Write(make([]byte, 16<<20))
			}()
		}
	}()
	port := echoServer(t)
	b, _ := pipeBridge(t, Filter{UID: -1})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range 24 {
		c, err := b.Dial(ctx, flood.Addr().(*net.TCPAddr).Port)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close() // never read
	}
	time.Sleep(500 * time.Millisecond)
	ch := make(chan error, 1)
	go func() {
		c, err := b.Dial(ctx, port)
		if err == nil {
			defer c.Close()
			fmt.Fprintln(c, "hi")
			_, err = bufio.NewReader(c).ReadString('\n')
		}
		ch <- err
	}()
	select {
	case err := <-ch:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(10 * time.Second):
		t.Error("unread downloads held up another connection through the bridge")
	}
}

// choose is the slot Collie's origin picks for a link, for a browser
// holding these preview cookies.
func choose(p *Proxy, g Grant, n int, cookies map[int]string) int {
	r := httptest.NewRequest("POST", "/api/x", nil)
	for slot, v := range cookies {
		r.AddCookie(&http.Cookie{Name: CookieName(slot), Value: v})
	}
	return p.ChooseSlot(r, g, n)
}

func TestChooseSlot(t *testing.T) {
	p, _ := testProxy(t)
	now := time.Now()
	p.Now = func() time.Time { return now }
	g := func(port int) Grant {
		return Grant{UserID: "u", Login: "L", SessionID: "s", Port: port, Target: "c:p/u"}
	}

	// A fresh browser: the first slot.
	if s := choose(p, g(5173), 3, nil); s != 0 {
		t.Errorf("fresh: %d", s)
	}
	// Links handed out in the last minute count as taken: opening two at
	// once spreads them.
	a := g(5173)
	a.Slot = 0
	p.Mint(a)
	if s := choose(p, g(3000), 3, nil); s != 1 {
		t.Errorf("second at once: %d", s)
	}
	// Opened: the cookies tell. The same server goes back to its slot.
	now = now.Add(2 * time.Minute) // links expired
	_, c0 := openIn(t, p, a, "")
	b := g(3000)
	b.Slot = 1
	now = now.Add(time.Second)
	_, c1 := openIn(t, p, b, "")
	cookies := map[int]string{0: c0, 1: c1}
	if s := choose(p, g(5173), 3, cookies); s != 0 {
		t.Errorf("same server: %d", s)
	}
	if s := choose(p, g(8000), 3, cookies); s != 2 {
		t.Errorf("free slot: %d", s)
	}
	// All taken: the one used longest ago (slot 0, unless used since).
	c := g(8000)
	c.Slot = 2
	now = now.Add(time.Second)
	_, c2 := openIn(t, p, c, "")
	cookies[2] = c2
	if s := choose(p, g(9000), 3, cookies); s != 0 {
		t.Errorf("all taken: %d, want the oldest (0)", s)
	}
	now = now.Add(time.Second)
	use(t, p, 0, c0) // slot 0 used again
	if s := choose(p, g(9000), 3, cookies); s != 1 {
		t.Errorf("all taken after using 0: %d, want 1", s)
	}
	// Another user's sign-in's links don't count; one slot: always it.
	if s := choose(p, g(9000), 1, cookies); s != 0 {
		t.Errorf("one slot: %d", s)
	}
}

func use(t *testing.T, p *Proxy, slot int, cookie string) {
	t.Helper()
	r := httptest.NewRequest("GET", "/__collie/nothing", nil)
	r.AddCookie(&http.Cookie{Name: CookieName(slot), Value: cookie})
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, g := range p.browserGrantsLocked(r, slot) {
		g.used = p.clock()
	}
}

// Each slot is a port, an origin and a cookie of its own: nothing opened
// on one works on another.
func TestSlotsKeepApart(t *testing.T) {
	p, _ := testProxy(t)
	port := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	g := Grant{UserID: "u", Port: port, Target: "host", Slot: 1}
	serve := func(slot int, path string, cookies map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Sec-Fetch-Mode", "navigate")
		for k, v := range cookies {
			r.AddCookie(&http.Cookie{Name: k, Value: v})
		}
		w := httptest.NewRecorder()
		p.Slot(slot).ServeHTTP(w, r)
		return w
	}
	// A link for slot 1 doesn't open on slot 0.
	if w := serve(0, OpenPath+"?t="+p.Mint(g), nil); w.Code != http.StatusForbidden || w.Header().Get("Set-Cookie") != "" {
		t.Errorf("slot 1's link on slot 0: %d", w.Code)
	}
	w := serve(1, OpenPath+"?t="+p.Mint(g), nil)
	var c string
	for _, x := range w.Result().Cookies() {
		if x.Name == CookieName(1) {
			c = x.Value
		}
	}
	if c == "" {
		t.Fatalf("open on slot 1: %d", w.Code)
	}
	if w := serve(1, "/", map[string]string{CookieName(1): c}); w.Code != http.StatusOK {
		t.Errorf("slot 1: %d", w.Code)
	}
	// Its cookie, under slot 0's name or its own, opens nothing on slot 0.
	for _, name := range []string{CookieName(0), CookieName(1)} {
		if w := serve(0, "/", map[string]string{name: c}); w.Code != http.StatusUnauthorized {
			t.Errorf("slot 1's cookie as %s on slot 0: %d", name, w.Code)
		}
	}
	// Servers can't set any slot's cookie.
	for _, sc := range []string{CookieName(0) + "=x", CookieName(7) + "=x; Path=/"} {
		if got := cleanSetCookie(sc); got != "" {
			t.Errorf("%q passed", sc)
		}
	}
}
