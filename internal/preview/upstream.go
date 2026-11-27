package preview

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Upstream is where one container's (or the host's) servers are reached.
type Upstream struct {
	dial  func(ctx context.Context, port int) (net.Conn, error)
	ports func(ctx context.Context) ([]Port, error)
	check func(port int) error // nil: any port may be asked for
	stop  func()
	tr    *http.Transport // HTTP/1.1 to the servers, over dial
}

func newUpstream(dial func(context.Context, int) (net.Conn, error), ports func(context.Context) ([]Port, error), stop func()) *Upstream {
	u := &Upstream{dial: dial, ports: ports, stop: stop}
	var p http.Protocols
	p.SetHTTP1(true)
	u.tr = &http.Transport{
		Protocols: &p,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			_, ps, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			port, err := strconv.Atoi(ps)
			if err != nil || !ValidPort(port) {
				return nil, ErrNotListening
			}
			return dial(ctx, port)
		},
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		// Pass the servers' encodings through untouched.
		DisableCompression: true,
	}
	return u
}

// HostUpstream reaches servers on this machine itself (host-run
// projects): only those the filter lets through, checked again for every
// connection (a server of another session may have taken the port since).
func HostUpstream(f Filter) *Upstream {
	var d net.Dialer
	d.Timeout = 10 * time.Second
	u := newUpstream(func(ctx context.Context, port int) (net.Conn, error) {
		addr, err := DialAddr(port, f)
		if err != nil {
			return nil, err
		}
		return d.DialContext(ctx, "tcp", addr)
	}, func(context.Context) ([]Port, error) { return Listening(f) }, nil)
	u.check = func(port int) error {
		_, err := DialAddr(port, f)
		return err
	}
	return u
}

// BridgeUpstream reaches the servers in a container through its bridge.
func BridgeUpstream(b *Bridge) *Upstream { return newUpstream(b.Dial, b.Ports, b.Close) }

// Ports lists the ports something listens on.
func (u *Upstream) Ports(ctx context.Context) ([]Port, error) { return u.ports(ctx) }

// Check tells whether a port may be opened here (on the host: only a
// server of the session's own folder). Containers allow any.
func (u *Upstream) Check(port int) error {
	if u.check == nil {
		return nil
	}
	return u.check(port)
}

func (u *Upstream) close() {
	u.tr.CloseIdleConnections()
	if u.stop != nil {
		u.stop()
	}
}

// Upstreams keeps one Upstream per container while it is in use.
type Upstreams struct {
	mu sync.Mutex
	m  map[string]*upEntry
}

type upEntry struct {
	u    *Upstream
	used time.Time
}

// An upstream unused for this long is closed (its bridge stops).
const upstreamIdle = 10 * time.Minute

// Get returns the upstream under key, making it with mk if needed.
func (us *Upstreams) Get(key string, mk func() *Upstream) *Upstream {
	us.mu.Lock()
	defer us.mu.Unlock()
	if us.m == nil {
		us.m = map[string]*upEntry{}
	}
	now := time.Now()
	for k, e := range us.m {
		if k != key && now.Sub(e.used) > upstreamIdle {
			go e.u.close()
			delete(us.m, k)
		}
	}
	e := us.m[key]
	if e == nil {
		e = &upEntry{u: mk()}
		us.m[key] = e
	}
	e.used = now
	return e.u
}
