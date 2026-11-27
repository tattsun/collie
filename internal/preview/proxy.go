package preview

import (
	"bufio"
	"context"
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
// Where previews have host names of their own, every container (the
// target) gets its own origin. Otherwise previews open on a few ports of
// Collie's host name (slots), each an origin of its own with a cookie of
// its own: a browser has one preview open per slot, and opening another
// takes a free slot or replaces the one used longest ago. There a link
// only opens in the browser that asked for it (Bind), and nothing a page
// leaves in the browser's cache or storage outlives a switch of its slot
// to another target.
//
// A preview cookie is good for nothing but the one port of the one
// session, and Collie's own sign-in never opens anything here.

// Grant is what a preview link, and then the preview cookie, opens.
type Grant struct {
	UserID string
	// Login is the hash of the Collie sign-in the link was made from (""
	// for the setup token or --no-auth, then Legacy says which).
	Login     string
	Legacy    bool
	SessionID string
	Port      int
	// Target names where the session's servers run: its container, or the
	// host. Previews of different targets never share an origin's state.
	Target string
	// Label is the host label of the target's own origin ("" where
	// previews open on Collie's host name).
	Label string
	// Slot is which of the preview ports it opens on (0 where targets have
	// names of their own).
	Slot int
}

// Target is where a grant leads now.
type Target struct {
	Key      string // Grant.Target
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
	// cookiePrefix starts the preview cookies' names, one per slot (see
	// CookieName). They are never passed on to the servers.
	cookiePrefix = "collie_preview"
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
	maxGrantsPerUser = 32
	maxGrants        = 4096
)

// reserved cookies are Collie's: they never reach a preview's server and
// a server can't set them. (Cookies don't keep to ports, so when previews
// are on another port of Collie's host name, the browser sends Collie's
// sign-in cookie along; it stops here.)
var reserved = map[string]bool{"collie_sess": true, "collie_auth": true}

// signInCookie reports whether a cookie is one of Collie's sign-in
// cookies (under any of the browsers' name prefixes, e.g.
// __Host-collie_sess).
func signInCookie(name string) bool { return reserved[strings.ToLower(unprefixed(name))] }

// unprefixed is a cookie name without __Host- or __Secure- (which
// browsers match in any case).
func unprefixed(name string) string {
	name = strings.TrimSpace(name)
	for _, pre := range []string{"__host-", "__secure-"} {
		if len(name) >= len(pre) && strings.EqualFold(name[:len(pre)], pre) {
			return name[len(pre):]
		}
	}
	return name
}

// CookieName is the preview cookie of a slot. Cookies don't keep to
// ports: each slot's has a name of its own, so opening a preview on one
// port never replaces another port's, and is only ever read there.
func CookieName(slot int) string { return cookiePrefix + "_" + strconv.Itoa(slot) }

// frontDoor are the cookie names (or their beginnings) of sign-in proxies
// that may sit in front of Collie and its previews: oauth2-proxy (split
// into _0, _1 …, and its _csrf), Pomerium, Authelia, Cloudflare Access.
var frontDoor = []string{"_oauth2_proxy", "_pomerium", "authelia_session", "CF_Authorization", "_forward_auth"}

// collieOrDoor reports whether a cookie is Collie's own or a sign-in
// proxy's: never passed to a preview's server, nor set by one.
func collieOrDoor(name string) bool {
	n := strings.ToLower(unprefixed(name))
	if reserved[n] || strings.HasPrefix(n, cookiePrefix) {
		return true
	}
	for _, p := range frontDoor {
		if strings.HasPrefix(n, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// Proxy serves the preview origin.
type Proxy struct {
	// Resolve checks that a grant still holds and says where it leads.
	Resolve func(Grant) (Target, error)
	// Origin is the preview origin a request to a slot was made to, as the
	// browser sees it (scheme://host[:port]), and the target label in its
	// host name where targets have origins of their own. ok is false for a
	// host that is no preview origin.
	Origin func(r *http.Request, slot int) (origin, label string, ok bool)
	// Bind, where previews share one origin, checks that a link is opened
	// in the browser it was made for: the one signed in to Collie with the
	// grant's sign-in. (It only compares; it never signs anything in.)
	Bind func(*http.Request, Grant) bool
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
	dropped                bool
	flights                map[*flight]bool // requests being served, WebSockets included
	// bound sums up the Collie sign-in cookies (signIn) of the browser
	// Bind last passed this grant for, on the shared origin.
	bound string
}

// flight is one request being served for a grant.
type flight struct {
	cancel context.CancelFunc
	conn   net.Conn // a WebSocket's connection
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
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mintLocked(g)
}

// MintSlot is Mint on the slot ChooseSlot picks, in one step: links asked
// for at the same moment don't get the same free slot.
func (p *Proxy) MintSlot(r *http.Request, g Grant, n int) (string, int) {
	p.init()
	p.mu.Lock()
	defer p.mu.Unlock()
	g.Slot = p.chooseSlotLocked(r, g, n)
	return p.mintLocked(g), g.Slot
}

func (p *Proxy) mintLocked(g Grant) string {
	t := secret()
	now := p.clock()
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
	if mine >= maxTokensPerUser {
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

// sweepOnce forgets expired tokens and grants, and ends what is being
// served (WebSockets, streams) for grants that no longer hold (signed out,
// session gone).
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
		case len(g.flights) > 0:
			live = append(live, g)
		}
	}
	p.mu.Unlock()
	for _, g := range live {
		t, err := p.resolve(g.Grant)
		p.mu.Lock()
		if err != nil {
			p.dropGrantLocked(g)
		} else {
			g.target, g.checked = t, p.clock()
		}
		p.mu.Unlock()
	}
}

// resolve is Resolve, also checking that the grant still leads where it
// was made for.
func (p *Proxy) resolve(g Grant) (Target, error) {
	t, err := p.Resolve(g)
	if err == nil && t.Key != g.Target {
		err = ErrGone
	}
	return t, err
}

func (p *Proxy) expired(g *grant, now time.Time) bool {
	return now.Sub(g.used) > grantIdle || now.Sub(g.created) > grantMax
}

// dropLocked forgets a grant and ends everything being served for it.
func (p *Proxy) dropLocked(h string) {
	g := p.grants[h]
	if g == nil {
		return
	}
	delete(p.grants, h)
	g.dropped = true
	for f := range g.flights {
		f.end()
	}
	g.flights = nil
}

func (p *Proxy) dropGrantLocked(g *grant) {
	for h, x := range p.grants {
		if x == g {
			p.dropLocked(h)
		}
	}
	g.dropped = true
}

func (f *flight) end() {
	f.cancel()
	if f.conn != nil {
		_ = f.conn.Close()
	}
}

// browserGrants are the grants behind the request's preview cookies. A
// page can set a cookie of the same name for a narrower path; any genuine
// one counts. p.mu must be held.
func (p *Proxy) browserGrantsLocked(r *http.Request, slot int) map[string]*grant {
	out := map[string]*grant{}
	for _, c := range r.CookiesNamed(CookieName(slot)) {
		h := hashOf(c.Value)
		if g := p.grants[h]; g != nil && g.Slot == slot {
			out[h] = g
		}
	}
	return out
}

// ChooseSlot picks the preview port for a link to g, in the browser that
// asks for it from Collie's own origin (on the same host name, it sends
// the preview cookies along): the slot already showing that server, else
// a free one, else the one used longest ago. Links handed to the same
// sign-in in the last minute count as taken, so opening several at once
// spreads them out.
func (p *Proxy) ChooseSlot(r *http.Request, g Grant, n int) int {
	p.init()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.chooseSlotLocked(r, g, n)
}

func (p *Proxy) chooseSlotLocked(r *http.Request, g Grant, n int) int {
	if n <= 1 {
		return 0
	}
	now := p.clock()
	same := func(x Grant) bool { return x.UserID == g.UserID && x.Target == g.Target && x.Port == g.Port }
	used := make([]time.Time, n) // zero: free
	for i := range n {
		for _, x := range p.browserGrantsLocked(r, i) {
			if p.expired(x, now) || x.Label != "" {
				continue
			}
			if same(x.Grant) {
				return i
			}
			if x.used.After(used[i]) {
				used[i] = x.used
			}
		}
	}
	for _, t := range p.tokens {
		if t.Login != g.Login || t.UserID != g.UserID || !now.Before(t.expires) || t.Slot < 0 || t.Slot >= n {
			continue
		}
		if same(t.Grant) {
			return t.Slot
		}
		used[t.Slot] = now
	}
	best := 0
	for i := range used {
		if used[i].IsZero() {
			return i
		}
		if used[i].Before(used[best]) {
			best = i
		}
	}
	return best
}

// lookup finds the grant behind the request's preview cookie for this
// origin and checks that it still holds.
//
// On the shared origin, a page on any port of Collie's host name can plant
// a preview cookie for a narrower path, even one holding another user's
// grant. So a grant counts there only in the browser signed in to Collie
// with the grant's sign-in (Bind, asked again whenever the sign-in cookies
// change), and a request carrying grants of different users or targets is
// refused rather than served by one of them.
func (p *Proxy) lookup(r *http.Request, label string, slot int) (*grant, error) {
	now := p.clock()
	shared := label == "" && p.Bind != nil
	print := ""
	if shared {
		print = signIn(r)
	}
	var found, unbound, foreign []*grant
	p.mu.Lock()
	for h, x := range p.browserGrantsLocked(r, slot) {
		if p.expired(x, now) {
			p.dropLocked(h)
			continue
		}
		if x.Label != label {
			continue
		}
		if shared && (x.bound == "" || x.bound != print) {
			unbound = append(unbound, x)
			continue
		}
		found = append(found, x)
	}
	p.mu.Unlock()
	for _, x := range unbound {
		// (Outside p.mu: Bind looks up Collie's sign-ins.)
		if p.Bind(r, x.Grant) {
			p.mu.Lock()
			x.bound = print
			p.mu.Unlock()
			found = append(found, x)
		} else {
			foreign = append(foreign, x)
		}
	}
	if len(found) == 0 {
		return nil, p.whyNot(foreign)
	}
	g := found[0]
	for _, x := range found[1:] {
		if x.UserID != g.UserID || x.Target != g.Target {
			return nil, errMixed
		}
		if x.created.After(g.created) {
			g = x
		}
	}
	p.mu.Lock()
	if g.dropped {
		p.mu.Unlock()
		return nil, errNoGrant
	}
	g.used = now
	fresh := now.Sub(g.checked) < recheck
	p.mu.Unlock()
	if fresh {
		return g, nil
	}
	t, err := p.resolve(g.Grant)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.dropGrantLocked(g)
		return nil, err
	}
	g.target, g.checked = t, p.clock()
	return g, nil
}

var (
	errNoGrant = errors.New("no preview open")
	// errMixed: the browser carries previews of different users or
	// targets for this origin, so some were planted.
	errMixed = errors.New("cookies of different previews")
	// errOtherBrowser: the grant is good, but not in a browser with its
	// sign-in.
	errOtherBrowser = errors.New("preview of another browser")
)

// whyNot says why the grants behind a request's cookies, none of which
// Bind passed, open nothing: they ended (signed out, the session gone,
// and then they are dropped), or they are another browser's.
func (p *Proxy) whyNot(foreign []*grant) error {
	if len(foreign) == 0 {
		return errNoGrant
	}
	var ended error
	for _, x := range foreign {
		if _, err := p.resolve(x.Grant); err != nil {
			p.mu.Lock()
			p.dropGrantLocked(x)
			p.mu.Unlock()
			if ended == nil {
				ended = err
			}
			continue
		}
		return errOtherBrowser
	}
	return ended
}

// signIn sums up the request's Collie sign-in cookies: Bind's answer for
// a grant holds as long as they stay the same.
func signIn(r *http.Request) string {
	var b strings.Builder
	for _, c := range r.Cookies() {
		if signInCookie(c.Name) {
			b.WriteString(c.Name)
			b.WriteByte('=')
			b.WriteString(c.Value)
			b.WriteByte(';')
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return hashOf(b.String())
}

// Slot serves one of the preview ports.
func (p *Proxy) Slot(slot int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { p.serve(w, r, slot) })
}

// ServeHTTP serves the first slot (or names of their own).
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) { p.serve(w, r, 0) }

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request, slot int) {
	p.init()
	origin, label, ok := p.Origin(r, slot)
	if !ok {
		p.page(w, r, http.StatusNotFound, msgNotFound, nil)
		return
	}
	if r.URL.Path == OpenPath {
		p.open(w, r, origin, label, slot)
		return
	}
	if strings.HasPrefix(r.URL.Path, ownPrefix) {
		p.page(w, r, http.StatusNotFound, msgNotFound, nil)
		return
	}
	g, err := p.lookup(r, label, slot)
	switch {
	case errors.Is(err, errNoGrant):
		p.page(w, r, http.StatusUnauthorized, msgNotOpen, nil)
		return
	case errors.Is(err, errMixed):
		p.page(w, r, http.StatusForbidden, msgMixed, nil)
		return
	case errors.Is(err, errOtherBrowser):
		p.page(w, r, http.StatusForbidden, msgOtherBrowser, nil)
		return
	case err != nil:
		p.page(w, r, http.StatusForbidden, msgGone, nil)
		return
	}
	if !sameOrigin(r, origin) || !ownRequest(r) {
		http.Error(w, "cross-origin request to a preview refused", http.StatusForbidden)
		return
	}
	if r.Header.Get("Service-Worker") == "script" {
		// A service worker would stay behind and answer for whatever is
		// opened on this origin next.
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
	p.forward(w, r, g, t, port, origin, label == "")
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

// ownRequest refuses what another site's pages make the browser fetch with
// the preview's cookie: other projects' preview names and Collie's other
// ports are the same site, so the cookie (SameSite=Lax) goes along. Only
// navigations, the user's own moves, may come from elsewhere.
func ownRequest(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-site", "cross-site":
		return r.Header.Get("Sec-Fetch-Mode") == "navigate"
	}
	return true
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
func (p *Proxy) open(w http.ResponseWriter, r *http.Request, origin, label string, slot int) {
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
		// preview open here just goes back to it.
		if _, err := p.lookup(r, label, slot); err == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		p.page(w, r, http.StatusForbidden, msgLinkUsed, nil)
		return
	}
	if g.Label != label || g.Slot != slot {
		// A link for another origin (another target's name, another port).
		p.page(w, r, http.StatusForbidden, msgLinkUsed, nil)
		return
	}
	if label == "" && p.Bind != nil && !p.Bind(r, g) {
		// Someone else's link (or one copied to another browser): on the
		// shared origin it would replace this browser's preview with theirs.
		p.page(w, r, http.StatusForbidden, msgOtherBrowser, nil)
		return
	}
	t, err := p.resolve(g)
	if err != nil {
		p.page(w, r, http.StatusForbidden, msgGone, nil)
		return
	}
	s := secret()
	now := p.clock()
	p.mu.Lock()
	// The cookie replaces the one this browser had here: so does the grant.
	same := false
	for k, x := range p.browserGrantsLocked(r, slot) {
		if x.Label == label {
			same = same || x.Target == g.Target
			p.dropLocked(k)
		}
	}
	p.makeRoomLocked(g.UserID)
	ng := &grant{Grant: g, created: now, used: now, checked: now, target: t}
	if label == "" && p.Bind != nil {
		ng.bound = signIn(r) // Bind passed above
	}
	p.grants[hashOf(s)] = ng
	p.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: CookieName(slot), Value: s, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(grantMax.Seconds()), Secure: strings.HasPrefix(origin, "https://"),
	})
	if label != "" || same {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	// This port now leads to another target: clear what the last one's
	// pages left on it in this browser (local storage, IndexedDB, cache)
	// before its page loads. Browsers act on this header for a page more
	// surely than for a redirect.
	h.Set("Clear-Site-Data", `"cache", "storage"`)
	p.page(w, r, http.StatusOK, msgOpening, nil)
}

// makeRoomLocked keeps the grants of one user, and of all users, in
// bounds, dropping the least recently used.
func (p *Proxy) makeRoomLocked(uid string) {
	oldest := func(mine bool) string {
		var o string
		for h, x := range p.grants {
			if (!mine || x.UserID == uid) && (o == "" || x.used.Before(p.grants[o].used)) {
				o = h
			}
		}
		return o
	}
	n := 0
	for _, x := range p.grants {
		if x.UserID == uid {
			n++
		}
	}
	if n >= maxGrantsPerUser {
		p.dropLocked(oldest(true))
	}
	if len(p.grants) >= maxGrants {
		p.dropLocked(oldest(false))
	}
}

// forward passes the request to the port.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, g *grant, t Target, port int, origin string, shared bool) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	f := &flight{cancel: cancel}
	p.mu.Lock()
	if g.dropped {
		p.mu.Unlock()
		p.page(w, r, http.StatusForbidden, msgGone, nil)
		return
	}
	if g.flights == nil {
		g.flights = map[*flight]bool{}
	}
	g.flights[f] = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(g.flights, f)
		p.mu.Unlock()
	}()
	r = r.WithContext(ctx)

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
			if shared {
				// Nothing of one target's may be kept to show for the next.
				resp.Header.Set("Cache-Control", "no-store")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // the browser went away, or the grant ended
			}
			p.page(w, r, http.StatusBadGateway, msgUnreachable, map[string]string{"port": strconv.Itoa(port), "err": err.Error()})
		},
		ErrorLog: log.New(io.Discard, "", 0),
	}
	tw := &trackWriter{ResponseWriter: w}
	tw.hijacked = func(c net.Conn) {
		p.mu.Lock()
		defer p.mu.Unlock()
		f.conn = c
		if g.dropped {
			_ = c.Close()
		}
	}
	rp.ServeHTTP(tw, r)
}

// trackWriter notes the connection a WebSocket takes over, so it can be
// cut when the grant ends, and keeps the server's informational (1xx)
// responses, which ReverseProxy would pass on unfiltered, from the browser.
type trackWriter struct {
	http.ResponseWriter
	hijacked func(net.Conn)
}

func (t *trackWriter) WriteHeader(code int) {
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		return
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *trackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, brw, err := http.NewResponseController(t.ResponseWriter).Hijack()
	if err == nil {
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
		for _, pre := range identityPrefixes {
			if strings.HasPrefix(lk, pre) {
				delete(h, k)
			}
		}
	}
	for _, k := range identityHeaders {
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

// Headers that say who the user is or where they are, set by Tailscale,
// Cloudflare or a sign-in proxy in front of Collie.
var (
	identityPrefixes = []string{"x-forwarded-", "tailscale-", "cf-", "x-auth-request-", "x-goog-authenticated-",
		"x-amzn-oidc-", "x-ms-client-principal", "x-pomerium-", "x-authentik-", "remote-"}
	identityHeaders = []string{"Forwarded", "X-Real-Ip", "True-Client-Ip", "Via", "Remote-User", "Remote-Email",
		"X-Remote-User", "X-Remote-Email", "X-Webauth-User", "X-Webauth-Email"}
)

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

// filterCookies drops Collie's own cookies and sign-in proxies', keeping
// the others as sent.
func filterCookies(h http.Header) {
	var keep []string
	for _, line := range h.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			part = strings.TrimSpace(part)
			name, _, _ := strings.Cut(part, "=")
			if part == "" || collieOrDoor(strings.TrimSpace(name)) {
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
// don't keep to ports), or have the browser send reports to any host,
// past a project's allowlist and, for NEL, about what opens on the origin
// later.
var dropResponse = []string{"Clear-Site-Data", "Strict-Transport-Security", "Alt-Svc", "Public-Key-Pins",
	"Public-Key-Pins-Report-Only", "Service-Worker-Allowed",
	"Report-To", "Reporting-Endpoints", "NEL", "Content-Security-Policy-Report-Only"}

func cleanResponse(h http.Header, origin string, port int, allow []string) {
	for _, k := range dropResponse {
		h.Del(k)
	}
	if csp := h.Values("Content-Security-Policy"); len(csp) > 0 {
		h.Del("Content-Security-Policy")
		for _, v := range csp {
			if v = withoutReports(v); v != "" {
				h.Add("Content-Security-Policy", v)
			}
		}
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

// withoutReports is a CSP header value (one or more policies) without its
// report-uri and report-to directives: violation reports go to any host,
// whatever the policy allows the page itself.
func withoutReports(v string) string {
	var policies []string
	for _, pol := range strings.Split(v, ",") {
		var keep []string
		for _, d := range strings.Split(pol, ";") {
			d = strings.TrimSpace(d)
			name, _, _ := strings.Cut(d, " ")
			name, _, _ = strings.Cut(name, "\t")
			switch strings.ToLower(name) {
			case "", "report-uri", "report-to":
				continue
			}
			keep = append(keep, d)
		}
		if len(keep) > 0 {
			policies = append(policies, strings.Join(keep, "; "))
		}
	}
	return strings.Join(policies, ", ")
}

// cleanSetCookie drops Collie's cookie names and any Domain attribute (a
// cookie for a parent domain would also reach Collie): the server's
// cookies stay on the preview's own host. Cookies without a name are
// dropped too: browsers send `=collie_sess=x` back as `collie_sess=x`.
func cleanSetCookie(c string) string {
	parts := strings.Split(c, ";")
	name, _, hasValue := strings.Cut(parts[0], "=")
	name = strings.TrimSpace(name)
	if !hasValue || name == "" || collieOrDoor(name) {
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
	// (webrtc: STUN and TURN reach any host, whatever connect-src says;
	// browsers that know the directive block it.)
	return "default-src " + all + " data: blob: 'unsafe-inline' 'unsafe-eval'; " +
		"form-action " + strings.Join(append([]string{"'self'"}, hosts...), " ") + "; base-uri 'self'; webrtc 'block'"
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
	msgOtherBrowser = msg{
		"This preview link was made for another browser. Open the preview from Collie in this browser.",
		"このリンクは別のブラウザ用に作られたものです。このブラウザの Collie からプレビューを開いてください。",
	}
	msgMixed = msg{
		"This browser holds preview cookies that Collie didn't give it for this preview. Clear this site's cookies, then open the preview again from Collie.",
		"このブラウザに、Collie がこのプレビュー用に発行していない Cookie が入っています。このサイトの Cookie を削除してから、Collie でプレビューを開き直してください。",
	}
	msgOpening  = msg{"Opening the preview…", "プレビューを開いています…"}
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
	switch m {
	case msgUnreachable:
		refresh = `<meta http-equiv="refresh" content="3">`
	case msgOpening:
		refresh = `<meta http-equiv="refresh" content="0;url=/">`
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
