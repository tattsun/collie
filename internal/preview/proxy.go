package preview

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// How a preview is opened:
//
//  1. In Collie (its own origin, signed in), the session's owner picks a
//     port. Collie checks they may open it and mints a token: random,
//     single-use, valid for a minute, and good only for that user, sign-in,
//     session and port (Mint).
//  2. The browser goes to <preview origin>/__collie/open?t=<token>. The
//     token is spent and exchanged for a cookie of the preview origin
//     (HttpOnly, so the page can't read it), then the browser is sent to "/".
//  3. Every request with that cookie is checked again (Resolve: the
//     sign-in is still valid, the user still owns the session) and passed
//     to the port, with Collie's cookies and anything identifying the
//     user taken out.
//
// The preview origin never accepts Collie's own sign-in cookie, and a
// preview cookie is good for nothing but the one port of the one session.
// A browser has one preview open at a time: opening another one replaces it.

// Grant is what a preview link, and then the preview cookie, opens.
type Grant struct {
	UserID string
	// Login is the hash of the Collie sign-in the link was made from (""
	// for the setup token or --no-auth, then Legacy says which).
	Login     string
	Legacy    bool
	SessionID string
	Port      int
}

// Target is where a grant leads now.
type Target struct {
	Upstream *Upstream
	// Allow, when not nil, keeps pages from loading or sending anything
	// anywhere but their own origin and these hosts: the project's
	// network allowlist (see allowCSP).
	Allow []string
	// Touch, if set, is called as the preview is used (so the container
	// isn't stopped as idle under it).
	Touch func()
}

// Errors Resolve returns; anything else is shown as is.
var (
	ErrSignedOut = errors.New("signed out of Collie")
	ErrGone      = errors.New("the session is gone or no longer yours")
)

const (
	// CookieName is the preview origin's own cookie. Collie's origin never
	// reads it, and it is never passed on to the servers.
	CookieName = "collie_preview"
	// OpenPath exchanges a token for the cookie. Paths under /__collie/
	// never reach the servers.
	OpenPath   = "/__collie/open"
	ownPrefix  = "/__collie/"
	tokenTTL   = time.Minute
	grantIdle  = 12 * time.Hour
	grantMax   = 7 * 24 * time.Hour
	recheck    = 2 * time.Second
	sweepEvery = 30 * time.Second
	touchEvery = 30 * time.Second

	maxTokensPerUser = 32
	maxTokens        = 4096
	maxGrants        = 4096
)

// reserved cookies are Collie's: they never reach a preview's server and
// a server can't set them. (Cookies don't keep to ports, so when previews
// are on another port of Collie's host name, the browser sends Collie's
// sign-in cookie along; it stops here.)
var reserved = map[string]bool{"collie_sess": true, "collie_auth": true, CookieName: true}

// Proxy serves the preview origin.
type Proxy struct {
	// Resolve checks that a grant still holds and says where it leads.
	Resolve func(Grant) (Target, error)
	// Origin is the preview origin as the browser sees it for a request
	// to it (scheme://host[:port]).
	Origin func(*http.Request) string
	// Back, if set, is Collie's own address for a request to the preview
	// origin (error pages link to it).
	Back func(*http.Request) string

	// Now is the clock (tests).
	Now func() time.Time

	mu     sync.Mutex
	tokens map[string]*token // by hash
	grants map[string]*grant // by hash of the cookie
	sweep  sync.Once
}

type token struct {
	Grant
	expires time.Time
}

type grant struct {
	Grant
	created, used, checked time.Time
	touched                time.Time
	target                 Target
	conns                  map[net.Conn]bool // hijacked (WebSocket) connections
}

func (p *Proxy) clock() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func secret() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func hashOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (p *Proxy) init() {
	p.sweep.Do(func() {
		if p.tokens == nil {
			p.tokens = map[string]*token{}
			p.grants = map[string]*grant{}
		}
		go func() {
			for range time.Tick(sweepEvery) {
				p.sweepOnce()
			}
		}()
	})
}

// Mint returns a token for a grant that Collie's origin has checked.
func (p *Proxy) Mint(g Grant) string {
	p.init()
	t := secret()
	now := p.clock()
	p.mu.Lock()
	defer p.mu.Unlock()
	mine := 0
	var oldest string
	for h, x := range p.tokens {
		if !now.Before(x.expires) {
			delete(p.tokens, h)
			continue
		}
		if x.UserID == g.UserID {
			mine++
			if oldest == "" || x.expires.Before(p.tokens[oldest].expires) {
				oldest = h
			}
		}
	}
	if mine >= maxTokensPerUser || len(p.tokens) >= maxTokens {
		delete(p.tokens, oldest)
	}
	if len(p.tokens) >= maxTokens {
		return "" // flooded: refuse rather than grow
	}
	p.tokens[hashOf(t)] = &token{Grant: g, expires: now.Add(tokenTTL)}
	return t
}

// redeem spends a token.
func (p *Proxy) redeem(t string) (Grant, bool) {
	if t == "" || len(t) > 128 {
		return Grant{}, false
	}
	h := hashOf(t)
	p.mu.Lock()
	defer p.mu.Unlock()
	x, ok := p.tokens[h]
	delete(p.tokens, h)
	if !ok || !p.clock().Before(x.expires) {
		return Grant{}, false
	}
	return x.Grant, true
}

// SweepNow is sweepOnce (tests).
func (p *Proxy) SweepNow() { p.init(); p.sweepOnce() }

// sweepOnce forgets expired tokens and grants, and closes the WebSockets
// of grants that no longer hold (signed out, session gone).
func (p *Proxy) sweepOnce() {
	now := p.clock()
	p.mu.Lock()
	for h, x := range p.tokens {
		if !now.Before(x.expires) {
			delete(p.tokens, h)
		}
	}
	var live []*grant
	for h, g := range p.grants {
		switch {
		case p.expired(g, now):
			p.dropLocked(h)
		case len(g.conns) > 0:
			live = append(live, g)
		}
	}
	p.mu.Unlock()
	for _, g := range live {
		t, err := p.Resolve(g.Grant)
		p.mu.Lock()
		if err != nil {
			for h, x := range p.grants {
				if x == g {
					p.dropLocked(h)
				}
			}
		} else {
			g.target, g.checked = t, p.clock()
		}
		p.mu.Unlock()
	}
}

func (p *Proxy) expired(g *grant, now time.Time) bool {
	return now.Sub(g.used) > grantIdle || now.Sub(g.created) > grantMax
}

// dropLocked forgets a grant and cuts its WebSockets.
func (p *Proxy) dropLocked(h string) {
	g := p.grants[h]
	if g == nil {
		return
	}
	delete(p.grants, h)
	for c := range g.conns {
		_ = c.Close()
	}
	g.conns = nil
}

// lookup finds the grant behind the request's preview cookie and checks
// that it still holds.
func (p *Proxy) lookup(r *http.Request) (*grant, error) {
	now := p.clock()
	var g *grant
	p.mu.Lock()
	// A page can set a cookie of the same name for a narrower path; any
	// genuine one counts.
	for _, c := range r.CookiesNamed(CookieName) {
		h := hashOf(c.Value)
		if x := p.grants[h]; x != nil {
			if p.expired(x, now) {
				p.dropLocked(h)
				continue
			}
			g = x
			break
		}
	}
	if g == nil {
		p.mu.Unlock()
		return nil, errNoGrant
	}
	g.used = now
	fresh := now.Sub(g.checked) < recheck
	p.mu.Unlock()
	if fresh {
		return g, nil
	}
	t, err := p.Resolve(g.Grant)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		for h, x := range p.grants {
			if x == g {
				p.dropLocked(h)
			}
		}
		return nil, err
	}
	g.target, g.checked = t, p.clock()
	return g, nil
}

var errNoGrant = errors.New("no preview open")

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.init()
	if r.URL.Path == OpenPath {
		p.open(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, ownPrefix) {
		p.page(w, r, http.StatusNotFound, msgNotFound, nil)
		return
	}
	g, err := p.lookup(r)
	switch {
	case errors.Is(err, errNoGrant):
		p.page(w, r, http.StatusUnauthorized, msgNotOpen, nil)
		return
	case err != nil:
		p.page(w, r, http.StatusForbidden, msgGone, nil)
		return
	}
	origin := p.Origin(r)
	if !sameOrigin(r, origin) {
		http.Error(w, "cross-origin request to a preview refused", http.StatusForbidden)
		return
	}
	if r.Header.Get("Service-Worker") == "script" {
		// One origin serves every preview a browser opens; a service worker
		// would stay behind and answer for the next session's server.
		http.Error(w, "service workers are turned off in Collie previews", http.StatusForbidden)
		return
	}
	p.mu.Lock()
	t, port := g.target, g.Port
	touch := t.Touch != nil && p.clock().Sub(g.touched) > touchEvery
	if touch {
		g.touched = p.clock()
	}
	p.mu.Unlock()
	if touch {
		t.Touch()
	}
	p.forward(w, r, g, t, port, origin)
}

// sameOrigin refuses requests that change something, and WebSockets,
// made from another origin's pages (CSRF). Pages on Collie's other ports
// are same-site, and so would carry the cookie.
func sameOrigin(r *http.Request, origin string) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	if strings.EqualFold(normOrigin(o), normOrigin(origin)) {
		return true
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return !isUpgrade(r)
	}
	return false
}

func isUpgrade(r *http.Request) bool {
	return r.Header.Get("Upgrade") != "" && strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// normOrigin drops default ports.
func normOrigin(o string) string {
	o = strings.ToLower(strings.TrimSuffix(o, "/"))
	if strings.HasPrefix(o, "https://") {
		return strings.TrimSuffix(o, ":443")
	}
	return strings.TrimSuffix(o, ":80")
}

// open exchanges a token for the preview cookie.
func (p *Proxy) open(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		// (Only a GET spends a token: not a HEAD from something that
		// looks at links before they are opened.)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	g, ok := p.redeem(r.URL.Query().Get("t"))
	if !ok {
		// Spent or expired (say, the back button): a browser that has a
		// preview open just goes back to it.
		if _, err := p.lookup(r); err == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		p.page(w, r, http.StatusForbidden, msgLinkUsed, nil)
		return
	}
	t, err := p.Resolve(g)
	if err != nil {
		p.page(w, r, http.StatusForbidden, msgGone, nil)
		return
	}
	s := secret()
	now := p.clock()
	p.mu.Lock()
	// The cookie replaces the one this browser had: so does the grant.
	for _, c := range r.CookiesNamed(CookieName) {
		p.dropLocked(hashOf(c.Value))
	}
	if len(p.grants) >= maxGrants {
		var oldest string
		for h, x := range p.grants {
			if oldest == "" || x.used.Before(p.grants[oldest].used) {
				oldest = h
			}
		}
		p.dropLocked(oldest)
	}
	p.grants[hashOf(s)] = &grant{Grant: g, created: now, used: now, checked: now, target: t}
	p.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: s, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(grantMax.Seconds()), Secure: strings.HasPrefix(p.Origin(r), "https://"),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// forward passes the request to the port.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, g *grant, t Target, port int, origin string) {
	up := "localhost:" + strconv.Itoa(port)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = up
			pr.Out.Host = up
			cleanRequest(pr.Out.Header, origin, up)
		},
		Transport: t.Upstream.tr,
		ModifyResponse: func(resp *http.Response) error {
			cleanResponse(resp.Header, origin, port, t.Allow)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // the browser went away
			}
			p.page(w, r, http.StatusBadGateway, msgUnreachable, map[string]string{"port": strconv.Itoa(port), "err": err.Error()})
		},
		ErrorLog: log.New(io.Discard, "", 0),
	}
	tw := &trackWriter{ResponseWriter: w}
	tw.hijacked = func(c net.Conn) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if g.conns == nil {
			g.conns = map[net.Conn]bool{}
		}
		g.conns[c] = true
	}
	rp.ServeHTTP(tw, r)
	if tw.conn != nil {
		p.mu.Lock()
		delete(g.conns, tw.conn)
		p.mu.Unlock()
	}
}

// trackWriter notes the connection a WebSocket takes over, so it can be
// cut when the grant ends.
type trackWriter struct {
	http.ResponseWriter
	hijacked func(net.Conn)
	conn     net.Conn
}

func (t *trackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, brw, err := http.NewResponseController(t.ResponseWriter).Hijack()
	if err == nil {
		t.conn = c
		t.hijacked(c)
	}
	return c, brw, err
}

func (t *trackWriter) Flush() { _ = http.NewResponseController(t.ResponseWriter).Flush() }

func (t *trackWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// cleanRequest makes the request look like one from a browser on the
// server's own machine (dev servers check Host and Origin against
// DNS rebinding), with Collie's cookies and the user's identity taken out.
func cleanRequest(h http.Header, origin, up string) {
	filterCookies(h)
	for k := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-forwarded-") || strings.HasPrefix(lk, "tailscale-") || strings.HasPrefix(lk, "cf-") {
			delete(h, k)
		}
	}
	for _, k := range []string{"Forwarded", "X-Real-Ip", "True-Client-Ip", "Via"} {
		h.Del(k)
	}
	local := "http://" + up
	if o := h.Get("Origin"); o != "" && strings.EqualFold(normOrigin(o), normOrigin(origin)) {
		h.Set("Origin", local)
	}
	if ref := h.Get("Referer"); ref != "" {
		if rest, ok := cutOrigin(ref, origin); ok {
			h.Set("Referer", local+rest)
		}
	}
	if u, err := url.Parse(origin); err == nil {
		h.Set("X-Forwarded-Host", u.Host)
		h.Set("X-Forwarded-Proto", u.Scheme)
	}
}

// cutOrigin returns the part of a URL after the origin, if it is on it.
func cutOrigin(s, origin string) (string, bool) {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", false
	}
	if !strings.EqualFold(normOrigin(u.Scheme+"://"+u.Host), normOrigin(origin)) {
		return "", false
	}
	u.Scheme, u.Host, u.User = "", "", nil
	rest := u.String()
	if rest == "" {
		rest = "/"
	}
	return rest, true
}

// filterCookies drops Collie's own cookies, keeping the others as sent.
func filterCookies(h http.Header) {
	var keep []string
	for _, line := range h.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			part = strings.TrimSpace(part)
			name, _, _ := strings.Cut(part, "=")
			if part == "" || reserved[strings.TrimSpace(name)] {
				continue
			}
			keep = append(keep, part)
		}
	}
	h.Del("Cookie")
	if len(keep) > 0 {
		h.Set("Cookie", strings.Join(keep, "; "))
	}
}

// Response headers a server may not send through a preview: they would
// reach beyond its origin, to Collie's host name (cookies, HSTS, site data
// don't keep to ports).
var dropResponse = []string{"Clear-Site-Data", "Strict-Transport-Security", "Alt-Svc", "Public-Key-Pins",
	"Public-Key-Pins-Report-Only", "Service-Worker-Allowed"}

func cleanResponse(h http.Header, origin string, port int, allow []string) {
	for _, k := range dropResponse {
		h.Del(k)
	}
	if sc := h.Values("Set-Cookie"); len(sc) > 0 {
		h.Del("Set-Cookie")
		for _, c := range sc {
			if c = cleanSetCookie(c); c != "" {
				h.Add("Set-Cookie", c)
			}
		}
	}
	if loc := h.Get("Location"); loc != "" {
		h.Set("Location", rewriteLocation(loc, origin, port))
	}
	if allow != nil {
		h.Add("Content-Security-Policy", allowCSP(origin, allow))
	}
}

// cleanSetCookie drops Collie's cookie names and any Domain attribute (a
// cookie for a parent domain would also reach Collie): the server's
// cookies stay on the preview's own host.
func cleanSetCookie(c string) string {
	parts := strings.Split(c, ";")
	name, _, _ := strings.Cut(parts[0], "=")
	if reserved[strings.TrimSpace(name)] {
		return ""
	}
	out := parts[:1]
	for _, a := range parts[1:] {
		k, _, _ := strings.Cut(a, "=")
		if strings.EqualFold(strings.TrimSpace(k), "domain") {
			continue
		}
		out = append(out, a)
	}
	return strings.Join(out, ";")
}

// rewriteLocation points redirects to the server itself (as it knows
// itself: localhost:port) at the preview origin.
func rewriteLocation(loc, origin string, port int) string {
	u, err := url.Parse(loc)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return loc
	}
	host, ps, err := net.SplitHostPort(u.Host)
	if err != nil || ps != strconv.Itoa(port) {
		return loc
	}
	switch strings.Trim(strings.ToLower(host), "[]") {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0", "::":
	default:
		return loc
	}
	o, err := url.Parse(origin)
	if err != nil {
		return loc
	}
	u.Scheme, u.Host = o.Scheme, o.Host
	return u.String()
}

// allowCSP keeps a page of a project on a network allowlist to what its
// container may reach: its own origin and the listed hosts. Addresses
// listed as ranges or IPv6 have no CSP form and are left out. A page can
// still navigate away, so this narrows, not seals, where data can go.
func allowCSP(origin string, allow []string) string {
	srcs := []string{"'self'"}
	if u, err := url.Parse(origin); err == nil && u.Host != "" {
		ws := "ws://"
		if u.Scheme == "https" {
			ws = "wss://"
		}
		srcs = append(srcs, ws+u.Host)
	}
	var hosts []string
	for _, a := range allow {
		if s := cspSource(a); s != "" {
			hosts = append(hosts, s)
		}
	}
	all := strings.Join(append(srcs, hosts...), " ")
	return "default-src " + all + " data: blob: 'unsafe-inline' 'unsafe-eval'; " +
		"form-action " + strings.Join(append([]string{"'self'"}, hosts...), " ") + "; base-uri 'self'"
}

func cspSource(entry string) string {
	if strings.ContainsAny(entry, "/[]") || strings.Count(entry, ":") > 1 {
		return "" // a range or an IPv6 address
	}
	for _, r := range entry {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '*' || r == ':' || r == '_') {
			return ""
		}
	}
	return entry
}

// ---- Collie's own pages on the preview origin ----

type msg struct{ en, ja string }

var (
	msgNotOpen = msg{
		"Open this preview from Collie: the session's Ports tab.",
		"Collie のセッション画面の「ポート」タブから開いてください。",
	}
	msgLinkUsed = msg{
		"This preview link has expired or was already used. Open the preview again from Collie.",
		"このリンクは期限切れか使用済みです。Collie からもう一度開いてください。",
	}
	msgGone = msg{
		"This preview is closed: you signed out of Collie, or the session is gone or no longer yours. Open it again from Collie.",
		"このプレビューは閉じられました（Collie からサインアウトしたか、セッションが削除された・利用できなくなった）。Collie からもう一度開いてください。",
	}
	msgUnreachable = msg{
		"Nothing answers on port {port} in the session yet ({err}). This page tries again every few seconds.",
		"セッションのポート {port} からまだ応答がありません（{err}）。数秒ごとに自動で再読み込みします。",
	}
	msgNotFound = msg{"Not found.", "見つかりません。"}
)

// page answers with Collie's own message: a small page for the browser's
// own navigations, plain text for everything else.
func (p *Proxy) page(w http.ResponseWriter, r *http.Request, code int, m msg, vars map[string]string) {
	ja := strings.HasPrefix(strings.ToLower(r.Header.Get("Accept-Language")), "ja")
	text := m.en
	if ja {
		text = m.ja
	}
	for k, v := range vars {
		text = strings.ReplaceAll(text, "{"+k+"}", v)
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	nav := r.Header.Get("Sec-Fetch-Mode") == "navigate" || (r.Header.Get("Sec-Fetch-Mode") == "" && strings.Contains(r.Header.Get("Accept"), "text/html"))
	if !nav || r.Method != http.MethodGet {
		http.Error(w, text, code)
		return
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.WriteHeader(code)
	refresh := ""
	if m == msgUnreachable {
		refresh = `<meta http-equiv="refresh" content="3">`
	}
	back := ""
	if p.Back != nil {
		if u := p.Back(r); u != "" {
			label := "Open Collie"
			if ja {
				label = "Collie を開く"
			}
			back = `<p><a href="` + html.EscapeString(u) + `">` + label + `</a></p>`
		}
	}
	lang := "en"
	if ja {
		lang = "ja"
	}
	_, _ = io.WriteString(w, `<!doctype html><html lang="`+lang+`"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">`+refresh+
		`<title>Collie preview</title><style>body{font:16px/1.5 system-ui,sans-serif;max-width:32em;margin:15vh auto;padding:0 1.2em;color:#333}`+
		`@media(prefers-color-scheme:dark){body{background:#111;color:#ddd}a{color:#8ab4f8}}</style>`+
		`<p>`+html.EscapeString(text)+`</p>`+back+`</html>`)
}
