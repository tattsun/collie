package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Proxy is Collie's side: one listener per container (project and user), a
// Unix socket in a directory mounted into that container.
type Proxy struct {
	// Rules returns a project's allowlist; ok is false when the project no
	// longer has one (then nothing gets through).
	Rules func(projectID string) (entries []string, ok bool)

	mu   sync.Mutex
	srvs map[string]*listener // by project/user
	logs map[string]*history  // by project

	dialer    net.Dialer
	transport *http.Transport
	local     localAddrs
	anyAddr   bool // tests: let the loopback etc. through

	// The connections kept open for clients (tunnels, forwarded requests
	// and their WebSockets), checked against the list again as it changes.
	cmu   sync.Mutex
	conns map[*conn]bool
	watch sync.Once
}

type listener struct {
	path string
	hs   *http.Server
}

// conn is a connection the proxy keeps open for a client: a CONNECT
// tunnel, or a forwarded request (a WebSocket's included).
type conn struct {
	key       string // the listener's: project/user
	projectID string
	t         target
	addr      netip.Addr // where it connected, once known
	end       func()
}

// How often open connections are checked against their project's list
// (edits also check them at once: Recheck).
const recheckEvery = 2 * time.Second

func NewProxy() *Proxy {
	p := &Proxy{srvs: map[string]*listener{}, logs: map[string]*history{}}
	p.dialer = net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, ControlContext: p.checkAddr}
	p.transport = &http.Transport{
		Proxy:               nil,
		DialContext:         p.dialer.DialContext,
		MaxIdleConns:        64,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 30 * time.Second,
	}
	return p
}

// Most connections a container can have open through the proxy at once.
const maxConns = 512

// Listen serves the project's proxy on the Unix socket at path for one of
// its containers (once per process and socket; again if the socket went
// away).
func (p *Proxy) Listen(path, projectID, userID string) error {
	key := projectID + "/" + userID
	p.mu.Lock()
	defer p.mu.Unlock()
	if l := p.srvs[key]; l != nil {
		if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSocket != 0 && l.path == path {
			return nil
		}
		_ = l.hs.Close()
		delete(p.srvs, key)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_ = os.Remove(path) // left over from an earlier run
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	// An old listener closing late must not take the new socket with it.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = os.Chmod(path, 0o600)
	hs := &http.Server{
		Handler:           p.handler(projectID, key),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       5 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	p.srvs[key] = &listener{path: path, hs: hs}
	go func() { _ = hs.Serve(limitListener(ln, maxConns)) }()
	return nil
}

// Close stops the proxy of a project's container (userID "": all of the
// project's, and its log), and ends the connections it has open.
func (p *Proxy) Close(projectID, userID string) {
	mine := func(key string) bool {
		pid, uid, _ := cutLast(key, "/")
		return pid == projectID && (userID == "" || uid == userID)
	}
	p.mu.Lock()
	for key, l := range p.srvs {
		if mine(key) {
			_ = l.hs.Close()
			_ = os.Remove(l.path)
			delete(p.srvs, key)
		}
	}
	if userID == "" {
		delete(p.logs, projectID)
	}
	p.mu.Unlock()
	p.endWhere(func(c *conn) bool { return mine(c.key) })
}

// Recheck ends the project's open connections that its list no longer
// allows (call it when the list changes; it also runs every few seconds).
func (p *Proxy) Recheck(projectID string) {
	p.endWhere(func(c *conn) bool { return c.projectID == projectID && !p.stillAllowed(c) })
}

func (p *Proxy) recheckAll() {
	lists := map[string]*list{} // each project's looked up once
	p.endWhere(func(c *conn) bool { return !p.allowedBy(c, lists) })
}

type list struct {
	entries []string
	has     bool
}

// stillAllowed checks an open connection as a new one would be: its
// destination against the list as it is now, and the address it went to.
func (p *Proxy) stillAllowed(c *conn) bool { return p.allowedBy(c, nil) }

func (p *Proxy) allowedBy(c *conn, lists map[string]*list) bool {
	if p.Rules == nil {
		return false
	}
	l := lists[c.projectID]
	if l == nil {
		l = &list{}
		l.entries, l.has = p.Rules(c.projectID)
		if lists != nil {
			lists[c.projectID] = l
		}
	}
	entries, has := l.entries, l.has
	if !has {
		return false
	}
	rl, ok := matchRule(entries, c.t)
	if !ok {
		return false
	}
	if a := c.addr; a.IsValid() && !p.anyAddr {
		if neverReachable(a) || p.local.has(a) || (privateAddr(a) && !rl.prefix.IsValid()) {
			return false
		}
	}
	return true
}

// track notes a connection kept open for a client; untrack it when done.
func (p *Proxy) track(key, projectID string, t target, end func()) *conn {
	p.watch.Do(func() {
		go func() {
			for range time.Tick(recheckEvery) {
				p.recheckAll()
			}
		}()
	})
	c := &conn{key: key, projectID: projectID, t: t, end: end}
	p.cmu.Lock()
	if p.conns == nil {
		p.conns = map[*conn]bool{}
	}
	p.conns[c] = true
	p.cmu.Unlock()
	return c
}

func (p *Proxy) untrack(c *conn) {
	p.cmu.Lock()
	delete(p.conns, c)
	p.cmu.Unlock()
}

func (p *Proxy) setAddr(c *conn, addr net.Addr) {
	ap, err := netip.ParseAddrPort(addr.String())
	if err != nil {
		return
	}
	p.cmu.Lock()
	c.addr = ap.Addr().Unmap()
	p.cmu.Unlock()
}

// endWhere ends (and forgets) the open connections f picks. f runs
// without the lock held: it may look up the project's list.
func (p *Proxy) endWhere(f func(*conn) bool) {
	p.cmu.Lock()
	all := make([]conn, 0, len(p.conns))
	ptrs := make([]*conn, 0, len(p.conns))
	for c := range p.conns {
		all = append(all, *c)
		ptrs = append(ptrs, c)
	}
	p.cmu.Unlock()
	for i := range all {
		if f(&all[i]) {
			p.untrack(ptrs[i])
			ptrs[i].end()
		}
	}
}

// CloseUser stops the proxies of a user's containers.
func (p *Proxy) CloseUser(userID string) {
	p.mu.Lock()
	var pids []string
	for key := range p.srvs {
		if pid, uid, _ := cutLast(key, "/"); uid == userID {
			pids = append(pids, pid)
		}
	}
	p.mu.Unlock()
	for _, pid := range pids {
		p.Close(pid, userID)
	}
}

func cutLast(s, sep string) (string, string, bool) {
	for i := len(s) - len(sep); i >= 0; i-- {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

func (p *Proxy) handler(projectID, key string) http.Handler {
	fwd := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// The Host header follows the URL, the destination that was
			// checked.
			pr.Out.Host = ""
		},
		Transport: p.transport,
		ModifyResponse: func(res *http.Response) error {
			p.note(projectID, newTarget(res.Request.URL.Hostname(), urlPort(res.Request)), "")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.dialFailed(w, r, projectID, newTarget(r.URL.Hostname(), urlPort(r)), err)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var t target
		if r.Method == http.MethodConnect {
			h, ps, err := net.SplitHostPort(r.Host)
			n, perr := strconv.Atoi(ps)
			if err != nil || perr != nil || n < 1 || n > 65535 {
				http.Error(w, "Collie's proxy: CONNECT needs host:port", http.StatusBadRequest)
				return
			}
			t = newTarget(h, n)
		} else {
			if r.URL.Scheme != "http" || r.URL.Host == "" {
				http.Error(w, "Collie's proxy forwards only http:// URLs and CONNECT", http.StatusBadRequest)
				return
			}
			t = newTarget(r.URL.Hostname(), urlPort(r))
		}
		var rl rule
		ok := false
		if p.Rules != nil {
			if entries, has := p.Rules(projectID); has {
				rl, ok = matchRule(entries, t)
			}
		}
		if !ok {
			p.note(projectID, t, BlockedList)
			deny(w, r, "Blocked by Collie (not on the project's network allowlist)",
				fmt.Sprintf("Collie: %s is not on this project's network allowlist, so this container can't connect to it. "+
					"The project's owner can allow it in Project → Settings → Network.", t.display()))
			return
		}
		// Only an address listed as such may be on a private network.
		ctx := context.WithValue(r.Context(), privateOK{}, rl.prefix.IsValid())
		if r.Method == http.MethodConnect {
			p.tunnel(w, r.WithContext(ctx), projectID, key, t)
			return
		}
		// A forwarded request (a download, a WebSocket) ends when the list
		// stops allowing it: ReverseProxy closes both sides of an upgraded
		// connection when the request's context ends.
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		c := p.track(key, projectID, t, cancel)
		defer p.untrack(c)
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GotConn: func(i httptrace.GotConnInfo) { p.setAddr(c, i.Conn.RemoteAddr()) },
		})
		fwd.ServeHTTP(w, r.WithContext(ctx))
	})
}

func urlPort(r *http.Request) int {
	if n, err := strconv.Atoi(r.URL.Port()); err == nil {
		return n
	}
	return 80
}

func (t target) display() string {
	return net.JoinHostPort(t.host, strconv.Itoa(t.port))
}

func (p *Proxy) tunnel(w http.ResponseWriter, r *http.Request, projectID, key string, t target) {
	up, err := p.dialer.DialContext(r.Context(), "tcp", t.display())
	if err != nil {
		p.dialFailed(w, r, projectID, t, err)
		return
	}
	p.note(projectID, t, "")
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "Collie's proxy: can't tunnel this connection", http.StatusInternalServerError)
		return
	}
	c, brw, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	// The tunnel lasts only while the list allows it.
	tc := p.track(key, projectID, t, func() {
		c.Close()
		up.Close()
	})
	p.setAddr(tc, up.RemoteAddr())
	defer p.untrack(tc)
	if !p.stillAllowed(tc) {
		// The list changed while connecting.
		c.Close()
		up.Close()
		return
	}
	_ = c.SetDeadline(time.Time{})
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		c.Close()
		up.Close()
		return
	}
	// What the client sent right after its request (e.g. a TLS hello).
	if n := brw.Reader.Buffered(); n > 0 {
		b, _ := brw.Reader.Peek(n)
		if _, err := up.Write(b); err != nil {
			c.Close()
			up.Close()
			return
		}
	}
	Pipe(c, up)
}

func (p *Proxy) dialFailed(w http.ResponseWriter, r *http.Request, projectID string, t target, err error) {
	var be *blockedAddrError
	if errors.As(err, &be) {
		p.note(projectID, t, BlockedAddress)
		deny(w, r, "Blocked by Collie (private or local address)",
			fmt.Sprintf("Collie: %s is at %s, which this container may not reach (loopback, this server, or a private network). "+
				"Private addresses can be allowed by listing the address itself.", t.display(), be.addr))
		return
	}
	p.note(projectID, t, "")
	http.Error(w, "Collie's proxy: "+err.Error(), http.StatusBadGateway)
}

// deny refuses a request with 403. For CONNECT the reason also goes in the
// status line: many clients show only that when a tunnel fails.
func deny(w http.ResponseWriter, r *http.Request, reason, msg string) {
	if hj, ok := w.(http.Hijacker); ok && r.Method == http.MethodConnect {
		if c, _, err := hj.Hijack(); err == nil {
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			fmt.Fprintf(c, "HTTP/1.1 403 %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s\n",
				reason, len(msg)+1, msg)
			return
		}
	}
	http.Error(w, msg, http.StatusForbidden)
}

// Pipe copies between two connections in both directions until both sides
// are done, then closes them.
func Pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}

// ---- addresses ----

type privateOK struct{}

type blockedAddrError struct{ addr netip.Addr }

func (e *blockedAddrError) Error() string { return "blocked address " + e.addr.String() }

// checkAddr runs for every address the proxy is about to connect to (after
// name resolution, so a name can't be pointed somewhere else later).
func (p *Proxy) checkAddr(ctx context.Context, network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	a := ap.Addr().Unmap()
	if p.anyAddr {
		return nil
	}
	ok, _ := ctx.Value(privateOK{}).(bool)
	if neverReachable(a) || p.local.has(a) || (privateAddr(a) && !ok) {
		return &blockedAddrError{a}
	}
	return nil
}

var (
	cgnat     = netip.MustParsePrefix("100.64.0.0/10") // incl. tailnets
	thisNet   = netip.MustParsePrefix("0.0.0.0/8")
	ietf      = netip.MustParsePrefix("192.0.0.0/24")
	bench     = netip.MustParsePrefix("198.18.0.0/15")
	reserved4 = netip.MustParsePrefix("240.0.0.0/4")
	siteLocal = netip.MustParsePrefix("fec0::/10")
	nat64     = netip.MustParsePrefix("64:ff9b::/96")
	nat64Loc  = netip.MustParsePrefix("64:ff9b:1::/48")
)

// neverReachable: the loopback, link-local (incl. cloud metadata services)
// and addresses that aren't a single host.
func neverReachable(a netip.Addr) bool {
	if a.Is6() && nat64.Contains(a) {
		b := a.As16()
		v4 := netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
		return neverReachable(v4) || privateAddr(v4)
	}
	return !a.IsGlobalUnicast() || thisNet.Contains(a) || reserved4.Contains(a) || nat64Loc.Contains(a)
}

// privateAddr: private networks (LAN, tailnet, …).
func privateAddr(a netip.Addr) bool {
	return a.IsPrivate() || cgnat.Contains(a) || ietf.Contains(a) || bench.Contains(a) || siteLocal.Contains(a)
}

// localAddrs are this server's own addresses, looked up again every few
// seconds.
type localAddrs struct {
	mu  sync.Mutex
	at  time.Time
	set map[netip.Addr]bool
}

func (l *localAddrs) has(a netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.set == nil || time.Since(l.at) > 10*time.Second {
		l.set = map[netip.Addr]bool{}
		l.at = time.Now()
		addrs, _ := net.InterfaceAddrs()
		for _, ad := range addrs {
			if pn, ok := ad.(*net.IPNet); ok {
				if x, ok := netip.AddrFromSlice(pn.IP); ok {
					l.set[x.Unmap()] = true
				}
			}
		}
	}
	return l.set[a]
}

// ---- recent connections ----

// Why a connection was refused.
const (
	BlockedList    = "list"    // not on the allowlist
	BlockedAddress = "address" // on it, but at a private or local address
)

// Entry is one destination a project's containers asked for recently.
type Entry struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Blocked string `json:"blocked,omitempty"`
	Count   int    `json:"count"`
	Last    int64  `json:"last"` // unix ms
}

const maxEntries = 200

type history struct {
	byKey map[string]*Entry
}

func (p *Proxy) note(projectID string, t target, blocked string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.logs[projectID]
	if h == nil {
		h = &history{byKey: map[string]*Entry{}}
		p.logs[projectID] = h
	}
	key := t.display() + " " + blocked
	e := h.byKey[key]
	if e == nil {
		if len(h.byKey) >= maxEntries {
			var oldest string
			for k, x := range h.byKey {
				if oldest == "" || x.Last < h.byKey[oldest].Last {
					oldest = k
				}
			}
			delete(h.byKey, oldest)
		}
		e = &Entry{Host: t.host, Port: t.port, Blocked: blocked}
		h.byKey[key] = e
	}
	e.Count++
	e.Last = time.Now().UnixMilli()
}

// Recent lists the destinations a project's containers asked for since
// Collie started (the most recent first).
func (p *Proxy) Recent(projectID string) []Entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []Entry{}
	if h := p.logs[projectID]; h != nil {
		for _, e := range h.byKey {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last > out[j].Last })
	return out
}

// ---- limits ----

func limitListener(ln net.Listener, n int) net.Listener {
	return &limited{Listener: ln, sem: make(chan struct{}, n)}
}

type limited struct {
	net.Listener
	sem chan struct{}
}

func (l *limited) Accept() (net.Conn, error) {
	l.sem <- struct{}{}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitedConn{Conn: c, release: func() { <-l.sem }}, nil
}

type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// CloseWrite lets Pipe half-close a hijacked connection.
func (c *limitedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Conn.Close()
}

// ---- inside the container ----

// Bridge runs inside a container on an allowlist (which has no network):
// it listens on addr, the proxy address its programs are given, and
// connects each client to Collie's proxy through the Unix socket.
func Bridge(sock, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return serveBridge(ln, sock)
}

func serveBridge(ln net.Listener, sock string) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) {
				time.Sleep(100 * time.Millisecond)
			}
			continue
		}
		go func() {
			u, err := net.Dial("unix", sock)
			if err != nil {
				c.Close()
				return
			}
			Pipe(c, u)
		}()
	}
}
