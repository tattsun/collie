package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/preview"
)

// Previews of agents' servers: the pages run on another origin and must
// never get hold of Collie's sign-in, and only a session's owner opens
// its servers.

// Previews on another port of Collie's host name: one origin for all.
const previewOrigin = "https://collie.example.com:8443"

// devServer stands in for an agent's dev server: it records what reached
// it, tries to set Collie's cookies and to reach beyond its origin, and
// echoes WebSocket messages.
type devServer struct {
	port int
	mu   sync.Mutex
	last *http.Request
	stop func()
}

func newDevServer(t *testing.T) *devServer {
	t.Helper()
	d := &devServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			typ, b, err := c.Read(r.Context())
			if err != nil {
				return
			}
			_ = c.Write(r.Context(), typ, append([]byte("echo "), b...))
		}
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, fmt.Sprintf("http://localhost:%d/next?x=1", d.port), http.StatusFound)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.last = r.Clone(context.Background())
		d.mu.Unlock()
		h := w.Header()
		h.Add("Set-Cookie", "collie_sess=planted; Path=/")
		h.Add("Set-Cookie", "collie_preview=planted; Path=/")
		h.Add("Set-Cookie", "app=1; Domain=example.com; Path=/; HttpOnly")
		h.Set("Strict-Transport-Security", "max-age=0")
		h.Set("Cache-Control", "public, max-age=31536000")
		h.Set("Clear-Site-Data", `"cookies"`)
		fmt.Fprintf(w, "hello from %s", r.URL.Path)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	d.port = ln.Addr().(*net.TCPAddr).Port
	d.stop = func() { srv.Close() }
	return d
}

func (d *devServer) lastRequest() *http.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.last
}

type previewFixture struct {
	*testServer
	dev        *devServer
	ownerSess  string // the owner's Collie sign-in cookie
	bobSess    string // another admin's
	carolSess  string // a member of the owner's group
	now        time.Time
	sessionID  string // the owner's host-run session
	sharedID   string // the owner's session shared with the group
	bobSession string // bob's own session
}

func newPreviewFixture(t *testing.T) *previewFixture {
	t.Helper()
	s := newTestServer(t, Options{PreviewURL: previewOrigin, PreviewPublic: []int{8443}})
	// The dev server runs in this test's own process, which host-run
	// previews would otherwise leave out as Collie's, and in no session's
	// folder (see TestHostRunSessionsSeeOnlyTheirOwnServers for that).
	s.hostFilter = preview.Filter{UID: -1, Allow: func([]string) bool { return true }}
	f := &previewFixture{testServer: s, dev: newDevServer(t), now: time.Now()}
	s.pv.Now = func() time.Time { return f.now }
	if err := s.users.AddPasskey(s.owner, webauthn.Credential{ID: []byte("c1")}, ""); err != nil {
		t.Fatal(err)
	}
	bob, err := s.users.CreateUser("bob", "Bob", true)
	if err != nil {
		t.Fatal(err)
	}
	carol, err := s.users.CreateUser("carol", "Carol", false)
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.users.CreateGroup("team", s.owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.users.SetMember(g.ID, carol.ID, "member"); err != nil {
		t.Fatal(err)
	}
	f.ownerSess, _ = s.users.NewSession(s.owner, "test", "")
	f.bobSess, _ = s.users.NewSession(bob.ID, "test", "")
	f.carolSess, _ = s.users.NewSession(carol.ID, "test", "")

	st := s.m.Store()
	st.PutProject(model.Project{ID: "p1", Name: "web", Path: t.TempDir(), OwnerUser: s.owner, Runtime: "host"})
	st.PutProject(model.Project{ID: "p2", Name: "bobs", Path: t.TempDir(), OwnerUser: bob.ID, Runtime: "host"})
	st.PutProject(model.Project{ID: "pt", Name: "team", Path: t.TempDir(), OwnerGroup: g.ID, Runtime: "host"})
	f.sessionID, f.bobSession, f.sharedID = "s1", "s2", "s3"
	st.PutSession(model.Session{ID: "s1", ProjectID: "p1", Owner: s.owner})
	st.PutSession(model.Session{ID: "s2", ProjectID: "p2", Owner: bob.ID})
	st.PutSession(model.Session{ID: "s3", ProjectID: "pt", Owner: s.owner, Shared: true})
	return f
}

// mint asks Collie's origin for a preview link, as the browser of the
// user signed in with sess.
func (f *previewFixture) mint(t *testing.T, sess, sid string, port int) (*httptest.ResponseRecorder, string) {
	t.Helper()
	w := f.do("POST", "/api/sessions/"+sid+"/preview", fmt.Sprintf(`{"port":%d}`, port), cookie(sessionCookie, sess),
		header("Origin", "https://collie.example.com"))
	if w.Code != http.StatusOK {
		return w, ""
	}
	var out struct{ URL string }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	u, err := url.Parse(out.URL)
	if err != nil || u.Scheme+"://"+u.Host != previewOrigin || u.Path != preview.OpenPath {
		t.Fatalf("preview link %q is not on the preview origin", out.URL)
	}
	return w, u.Query().Get("t")
}

// onPreview makes a request to the preview origin.
func (f *previewFixture) onPreview(method, path string, opts ...reqOpt) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = "203.0.113.9:4000"
	r.Host = "collie.example.com:8443"
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	f.PreviewHandler(0).ServeHTTP(w, r)
	return w
}

func previewCookie(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == preview.CookieName(0) {
			if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || !c.Secure || c.Domain != "" {
				t.Errorf("preview cookie %+v: want HttpOnly, SameSite=Lax, Secure, host-only", c)
			}
			return c.Value
		}
	}
	return ""
}

// open spends a token on the preview origin, in the browser signed in to
// Collie with sess, and returns the cookie.
func (f *previewFixture) open(t *testing.T, tok, sess string) string {
	t.Helper()
	w := f.onPreview("GET", preview.OpenPath+"?t="+tok, cookie(sessionCookie, sess))
	switch {
	case w.Code == http.StatusSeeOther && w.Header().Get("Location") == "/":
	case w.Code == http.StatusOK && w.Header().Get("Clear-Site-Data") == `"cache", "storage"`:
		// (another target than before: what it left is cleared first)
	default:
		t.Fatalf("open: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Error("the token page may leak its URL in Referer")
	}
	c := previewCookie(t, w)
	if c == "" {
		t.Fatal("no preview cookie")
	}
	return c
}

func TestPreviewOnlyForTheSessionsOwner(t *testing.T) {
	f := newPreviewFixture(t)
	port := f.dev.port
	// Bob (an admin, too) can't open the owner's session, nor list its ports.
	if w, _ := f.mint(t, f.bobSess, f.sessionID, port); w.Code != http.StatusNotFound {
		t.Errorf("another user minted a link for someone else's session: %d", w.Code)
	}
	if w := f.do("GET", "/api/sessions/"+f.sessionID+"/ports", "", cookie(sessionCookie, f.bobSess)); w.Code != http.StatusNotFound {
		t.Errorf("another user listed someone else's ports: %d", w.Code)
	}
	// Carol sees the shared group session, but it isn't hers.
	if w, _ := f.mint(t, f.carolSess, f.sharedID, port); w.Code != http.StatusForbidden {
		t.Errorf("a group member opened the owner's shared session: %d %s", w.Code, w.Body.String())
	}
	if w := f.do("GET", "/api/sessions/"+f.sharedID+"/ports", "", cookie(sessionCookie, f.carolSess)); w.Code != http.StatusForbidden {
		t.Errorf("a group member listed the owner's ports: %d", w.Code)
	}
	// Not signed in at all.
	if w := f.do("POST", "/api/sessions/"+f.sessionID+"/preview", `{"port":80}`); w.Code != http.StatusUnauthorized {
		t.Errorf("minted without signing in: %d", w.Code)
	}
	// The owner can.
	w, tok := f.mint(t, f.ownerSess, f.sessionID, port)
	if tok == "" {
		t.Fatalf("owner: %d %s", w.Code, w.Body.String())
	}
	w = f.do("GET", "/api/sessions/"+f.sessionID+"/ports", "", cookie(sessionCookie, f.ownerSess))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), fmt.Sprintf(`"port":%d`, port)) {
		t.Errorf("owner's ports: %d %s", w.Code, w.Body.String())
	}
	// Bob's own token doesn't open the owner's session either: a token is
	// only ever for the session it was minted for.
	_, bobTok := f.mint(t, f.bobSess, f.bobSession, port)
	c := f.open(t, bobTok, f.bobSess)
	f.onPreview("GET", "/", cookie(preview.CookieName(0), c))
	if r := f.dev.lastRequest(); r == nil {
		t.Fatal("bob's own preview didn't reach the server")
	}
}

func TestPreviewTokensAreSingleUseAndShortLived(t *testing.T) {
	f := newPreviewFixture(t)
	_, tok := f.mint(t, f.ownerSess, f.sessionID, f.dev.port)
	c := f.open(t, tok, f.ownerSess)

	// Spent: it opens nothing again, in this browser or another.
	w := f.onPreview("GET", preview.OpenPath+"?t="+tok)
	if w.Code != http.StatusForbidden || previewCookie(t, w) != "" {
		t.Errorf("a spent token worked again: %d", w.Code)
	}
	// (The back button in the browser that has the preview open just goes back to it.)
	w = f.onPreview("GET", preview.OpenPath+"?t="+tok, cookie(preview.CookieName(0), c))
	if w.Code != http.StatusSeeOther || previewCookie(t, w) != "" {
		t.Errorf("spent token with the preview open: %d", w.Code)
	}

	// Expired after a minute.
	_, tok2 := f.mint(t, f.ownerSess, f.sessionID, f.dev.port)
	f.now = f.now.Add(61 * time.Second)
	if w := f.onPreview("GET", preview.OpenPath+"?t="+tok2); w.Code != http.StatusForbidden {
		t.Errorf("an expired token worked: %d", w.Code)
	}
	// Made up or altered.
	for _, bad := range []string{"", "x", tok + "x", strings.Repeat("a", 43)} {
		if w := f.onPreview("GET", preview.OpenPath+"?t="+url.QueryEscape(bad)); w.Code != http.StatusForbidden {
			t.Errorf("token %q: %d", bad, w.Code)
		}
	}
	// A token is no use on Collie's own origin, and the preview cookie
	// doesn't sign anyone in there.
	_, tok3 := f.mint(t, f.ownerSess, f.sessionID, f.dev.port)
	if w := f.do("GET", "/api/state", "", header("Authorization", "Bearer "+tok3)); w.Code != http.StatusUnauthorized {
		t.Errorf("a preview token signed in to Collie: %d", w.Code)
	}
	if w := f.do("GET", "/api/state", "", cookie(preview.CookieName(0), c)); w.Code != http.StatusUnauthorized {
		t.Errorf("the preview cookie signed in to Collie: %d", w.Code)
	}
}

func TestPreviewIgnoresCollieSignIn(t *testing.T) {
	f := newPreviewFixture(t)
	// Previews on another port of Collie's host get Collie's cookie from
	// the browser too: it must not open anything.
	w := f.onPreview("GET", "/", cookie(sessionCookie, f.ownerSess))
	if w.Code != http.StatusUnauthorized || f.dev.lastRequest() != nil {
		t.Errorf("Collie's sign-in cookie opened a preview: %d", w.Code)
	}
	// Nor does the setup token.
	w = f.onPreview("GET", "/", header("Authorization", "Bearer the-setup-token"), cookie(legacyCookie, "the-setup-token"))
	if w.Code != http.StatusUnauthorized || f.dev.lastRequest() != nil {
		t.Errorf("the setup token opened a preview: %d", w.Code)
	}
}

func TestPreviewKeepsCollieCookiesFromTheServer(t *testing.T) {
	f := newPreviewFixture(t)
	_, tok := f.mint(t, f.ownerSess, f.sessionID, f.dev.port)
	c := f.open(t, tok, f.ownerSess)
	w := f.onPreview("GET", "/page?q=1", cookie(preview.CookieName(0), c), cookie(sessionCookie, f.ownerSess),
		cookie(legacyCookie, "the-setup-token"), cookie("app", "mine"),
		header("Origin", previewOrigin), header("Referer", previewOrigin+"/from?x=1"),
		header("Tailscale-User-Login", "alice@example.com"), header("X-Forwarded-For", "198.51.100.1"),
		header("Cf-Connecting-Ip", "198.51.100.1"), header("Forwarded", "for=198.51.100.1"))
	if w.Code != http.StatusOK || w.Body.String() != "hello from /page" {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	r := f.dev.lastRequest()
	if got := r.Header.Get("Cookie"); got != "app=mine" {
		t.Errorf("the server got cookies %q, want only its own", got)
	}
	if strings.Contains(fmt.Sprint(r.Header), f.ownerSess) || strings.Contains(fmt.Sprint(r.Header), c) {
		t.Error("a Collie secret reached the server")
	}
	for _, h := range []string{"Tailscale-User-Login", "X-Forwarded-For", "Cf-Connecting-Ip", "Forwarded"} {
		if r.Header.Get(h) != "" {
			t.Errorf("the server got %s", h)
		}
	}
	local := fmt.Sprintf("localhost:%d", f.dev.port)
	if r.Host != local || r.Header.Get("Origin") != "http://"+local || r.Header.Get("Referer") != "http://"+local+"/from?x=1" {
		t.Errorf("host %q origin %q referer %q: want the server's own", r.Host, r.Header.Get("Origin"), r.Header.Get("Referer"))
	}
	if r.Header.Get("X-Forwarded-Host") != "collie.example.com:8443" || r.Header.Get("X-Forwarded-Proto") != "https" {
		t.Errorf("forwarded host/proto: %v", r.Header)
	}

	// And it can't set Collie's cookies, nor reach past its own origin.
	var names []string
	for _, sc := range w.Header().Values("Set-Cookie") {
		names = append(names, strings.SplitN(sc, "=", 2)[0])
		if strings.Contains(strings.ToLower(sc), "domain=") {
			t.Errorf("cookie for another domain passed: %q", sc)
		}
	}
	if strings.Join(names, ",") != "app" {
		t.Errorf("cookies set through the preview: %v", names)
	}
	if w.Header().Get("Strict-Transport-Security") != "" || w.Header().Get("Clear-Site-Data") != "" {
		t.Error("headers reaching Collie's host name passed")
	}
	// Redirects to the server itself stay on the preview origin.
	w = f.onPreview("GET", "/redirect", cookie(preview.CookieName(0), c))
	if loc := w.Header().Get("Location"); loc != previewOrigin+"/next?x=1" {
		t.Errorf("redirect to %q", loc)
	}
	// Collie's own paths never reach the server.
	if w := f.onPreview("GET", "/__collie/anything", cookie(preview.CookieName(0), c)); w.Code != http.StatusNotFound {
		t.Errorf("/__collie/: %d", w.Code)
	}
}

func TestPreviewEndsWithTheSignIn(t *testing.T) {
	f := newPreviewFixture(t)
	_, tok := f.mint(t, f.ownerSess, f.sessionID, f.dev.port)
	c := f.open(t, tok, f.ownerSess)
	if w := f.onPreview("GET", "/", cookie(preview.CookieName(0), c)); w.Code != http.StatusOK {
		t.Fatalf("preview: %d", w.Code)
	}
	// Signing out of Collie closes the preview (within seconds).
	_, sess, _ := f.users.SessionUser(f.ownerSess, "")
	f.users.DeleteSession(sess.Hash)
	f.now = f.now.Add(3 * time.Second)
	if w := f.onPreview("GET", "/", cookie(preview.CookieName(0), c)); w.Code != http.StatusForbidden {
		t.Errorf("preview after signing out: %d", w.Code)
	}
	// For good.
	if w := f.onPreview("GET", "/", cookie(preview.CookieName(0), c)); w.Code != http.StatusUnauthorized {
		t.Errorf("preview after signing out, again: %d", w.Code)
	}
	// A token minted before the sign-out is no good either.
	sess2, _ := f.users.NewSession(f.owner, "test", "")
	_, tok2 := f.mint(t, sess2, f.sessionID, f.dev.port)
	_, s2, _ := f.users.SessionUser(sess2, "")
	f.users.DeleteSession(s2.Hash)
	if w := f.onPreview("GET", preview.OpenPath+"?t="+tok2, cookie(sessionCookie, sess2)); w.Code != http.StatusForbidden || previewCookie(t, w) != "" {
		t.Errorf("token of a signed-out session: %d", w.Code)
	}
}

func TestPreviewEndsWhenTheSessionIsNoLongerYours(t *testing.T) {
	f := newPreviewFixture(t)
	_, tok := f.mint(t, f.ownerSess, f.sessionID, f.dev.port)
	c := f.open(t, tok, f.ownerSess)
	f.m.Store().DeleteSession(f.sessionID)
	f.now = f.now.Add(3 * time.Second)
	if w := f.onPreview("GET", "/", cookie(preview.CookieName(0), c)); w.Code != http.StatusForbidden {
		t.Errorf("preview of a deleted session: %d", w.Code)
	}
}

func TestPreviewReplacesTheOneOpenBefore(t *testing.T) {
	f := newPreviewFixture(t)
	_, tok := f.mint(t, f.ownerSess, f.sessionID, f.dev.port)
	c1 := f.open(t, tok, f.ownerSess)
	other := newDevServer(t)
	_, tok2 := f.mint(t, f.ownerSess, f.sessionID, other.port)
	w := f.onPreview("GET", preview.OpenPath+"?t="+tok2, cookie(preview.CookieName(0), c1), cookie(sessionCookie, f.ownerSess))
	c2 := previewCookie(t, w)
	if c2 == "" || c2 == c1 {
		t.Fatal("no new cookie")
	}
	if w := f.onPreview("GET", "/", cookie(preview.CookieName(0), c1)); w.Code != http.StatusUnauthorized {
		t.Errorf("the replaced cookie still works: %d", w.Code)
	}
	// The server stops: the page says so and tries again.
	other.stop()
	w = f.onPreview("GET", "/", cookie(preview.CookieName(0), c2))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "http-equiv=\"refresh\"") {
		t.Errorf("nothing listening: %d %s", w.Code, w.Body.String())
	}
}

func TestPreviewRefusesOtherOrigins(t *testing.T) {
	f := newPreviewFixture(t)
	_, tok := f.mint(t, f.ownerSess, f.sessionID, f.dev.port)
	c := f.open(t, tok, f.ownerSess)
	ck := cookie(preview.CookieName(0), c)
	// Collie's own origin (same site as a preview on another port) and others.
	for _, o := range []string{"https://collie.example.com", "https://evil.example", "null"} {
		if w := f.onPreview("POST", "/x", ck, header("Origin", o)); w.Code != http.StatusForbidden {
			t.Errorf("POST from %s: %d", o, w.Code)
		}
		if w := f.onPreview("GET", "/ws", ck, header("Origin", o), header("Connection", "Upgrade"), header("Upgrade", "websocket")); w.Code != http.StatusForbidden {
			t.Errorf("WebSocket from %s: %d", o, w.Code)
		}
	}
	if w := f.onPreview("POST", "/x", ck, header("Origin", previewOrigin)); w.Code != http.StatusOK {
		t.Errorf("POST from the preview itself: %d", w.Code)
	}
	// Fetches by another site's pages (Collie's other ports, other
	// projects' preview names are the same site) carry the cookie too.
	for _, site := range []string{"same-site", "cross-site"} {
		if w := f.onPreview("GET", "/x", ck, header("Sec-Fetch-Site", site), header("Sec-Fetch-Mode", "cors")); w.Code != http.StatusForbidden {
			t.Errorf("%s fetch: %d", site, w.Code)
		}
		if w := f.onPreview("GET", "/x", ck, header("Sec-Fetch-Site", site), header("Sec-Fetch-Mode", "navigate")); w.Code != http.StatusOK {
			t.Errorf("%s navigation: %d", site, w.Code)
		}
	}
	// Service workers would outlive the preview and answer for the next one.
	if w := f.onPreview("GET", "/sw.js", ck, header("Service-Worker", "script")); w.Code != http.StatusForbidden {
		t.Errorf("service worker script: %d", w.Code)
	}
}

func TestPreviewWebSocketsCloseWithTheSignIn(t *testing.T) {
	f := newPreviewFixture(t)
	_, tok := f.mint(t, f.ownerSess, f.sessionID, f.dev.port)
	c := f.open(t, tok, f.ownerSess)
	srv := httptest.NewServer(f.PreviewHandler(0))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h := http.Header{}
	h.Set("Cookie", preview.CookieName(0)+"="+c+"; "+sessionCookie+"="+f.ownerSess)
	h.Set("Origin", previewOrigin)
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/ws", &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if err := ws.Write(ctx, websocket.MessageText, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	if _, b, err := ws.Read(ctx); err != nil || string(b) != "echo hi" {
		t.Fatalf("echo: %q %v", b, err)
	}
	// Signed out: the open WebSocket is cut too.
	_, sess, _ := f.users.SessionUser(f.ownerSess, "")
	f.users.DeleteSession(sess.Hash)
	f.pv.SweepNow()
	rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
	defer rcancel()
	if _, _, err := ws.Read(rctx); err == nil || rctx.Err() != nil {
		t.Errorf("WebSocket still open after signing out (%v)", err)
	}
}

// Collie's API answers only its own origin: not even the same site, which
// includes previews on Collie's other ports.
func TestAPIRefusesSameSitePages(t *testing.T) {
	f := newPreviewFixture(t)
	for _, site := range []string{"same-site", "cross-site"} {
		w := f.do("GET", "/api/state", "", cookie(sessionCookie, f.ownerSess), header("Sec-Fetch-Site", site))
		if w.Code != http.StatusForbidden {
			t.Errorf("Sec-Fetch-Site %s: %d", site, w.Code)
		}
	}
	for _, site := range []string{"same-origin", "none", ""} {
		w := f.do("GET", "/api/state", "", cookie(sessionCookie, f.ownerSess), header("Sec-Fetch-Site", site))
		if w.Code != http.StatusOK {
			t.Errorf("Sec-Fetch-Site %q: %d", site, w.Code)
		}
	}
	// A page on a preview port posting to Collie.
	w := f.do("POST", "/api/sessions/"+f.sessionID+"/preview", `{"port":80}`, cookie(sessionCookie, f.ownerSess),
		header("Origin", "https://collie.example.com:8443"))
	if w.Code != http.StatusForbidden {
		t.Errorf("mint from the preview port: %d", w.Code)
	}
}

func TestPreviewBase(t *testing.T) {
	req := func(remote, host string, hdr map[string]string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr, r.Host = remote, host
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r
	}
	cases := []struct {
		name string
		opt  Options
		r    *http.Request
		want string
	}{
		{"own port, on a LAN", Options{PreviewPorts: []int{7879}}, req("192.168.1.5:5000", "192.168.1.10:7878", nil), "http://192.168.1.10:7879"},
		{"own port, on this machine", Options{PreviewPorts: []int{7879}}, req("127.0.0.1:5000", "localhost:7878", nil), "http://localhost:7879"},
		{"own port, behind a local proxy", Options{PreviewPorts: []int{7879}}, req("127.0.0.1:5000", "box.tail1.ts.net", nil), ""},
		{"port of the proxy", Options{PreviewPorts: []int{7879}, PreviewURL: ":", PreviewPublic: []int{8443}}, req("127.0.0.1:5000", "box.tail1.ts.net", nil), "https://box.tail1.ts.net:8443"},
		{"port of the proxy, public URL", Options{PreviewPorts: []int{7879}, PreviewURL: ":", PreviewPublic: []int{8443}, PublicURL: "https://collie.example.com"},
			req("127.0.0.1:5000", "127.0.0.1:7878", map[string]string{"X-Forwarded-Proto": "https"}), "https://collie.example.com:8443"},
		{"URL", Options{PreviewURL: "https://collie.example.com:8443", PreviewPublic: []int{8443}}, req("127.0.0.1:5000", "collie.example.com", nil), "https://collie.example.com:8443"},
		{"own names", Options{PreviewURL: "https://*.preview.example.com"}, req("127.0.0.1:5000", "collie.example.com", nil), "https://" + previewLabel("c:p1/u1") + ".preview.example.com"},
		{"off", Options{PreviewPorts: []int{7879}, PreviewURL: "off"}, req("192.168.1.5:5000", "192.168.1.10:7878", nil), ""},
		{"none", Options{}, req("192.168.1.5:5000", "192.168.1.10:7878", nil), ""},
	}
	for _, c := range cases {
		s := &Server{opt: c.opt}
		if got := s.previewBase(c.r, "c:p1/u1", 0); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// On the shared origin a link opens only in the browser that asked for it:
// otherwise anyone with a Collie account could put their own server's
// page in someone else's browser, on the origin that browser's previews
// use (login CSRF).
func TestPreviewLinkOpensOnlyInItsBrowser(t *testing.T) {
	f := newPreviewFixture(t)
	_, bobTok := f.mint(t, f.bobSess, f.bobSession, f.dev.port)
	for name, opts := range map[string][]reqOpt{
		"the owner's browser": {cookie(sessionCookie, f.ownerSess)},
		"no Collie sign-in":   {},
		"a forged sign-in":    {cookie(sessionCookie, "made-up")},
	} {
		w := f.onPreview("GET", preview.OpenPath+"?t="+bobTok, opts...)
		if w.Code != http.StatusForbidden || previewCookie(t, w) != "" {
			t.Errorf("bob's link in %s: %d", name, w.Code)
		}
		_, bobTok = f.mint(t, f.bobSess, f.bobSession, f.dev.port)
	}
	// In bob's own browser it opens, even with a cookie a page planted for
	// a narrower path in front of the real one.
	w := f.onPreview("GET", preview.OpenPath+"?t="+bobTok, cookie(sessionCookie, "planted"), cookie(sessionCookie, f.bobSess))
	if previewCookie(t, w) == "" {
		t.Errorf("bob's link in bob's browser: %d %s", w.Code, w.Body.String())
	}
}

// Nothing a page leaves in the browser's cache may be shown for the next
// target opened on the shared origin.
func TestPreviewSharedOriginIsNotCached(t *testing.T) {
	f := newPreviewFixture(t)
	_, tok := f.mint(t, f.ownerSess, f.sessionID, f.dev.port)
	c := f.open(t, tok, f.ownerSess)
	w := f.onPreview("GET", "/", cookie(preview.CookieName(0), c))
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q on the shared origin", got)
	}
}

// A page can set a sign-in cookie for a narrower path, which the browser
// sends first: it must not lock the user out of Collie.
func TestSignInSurvivesPlantedCookies(t *testing.T) {
	f := newPreviewFixture(t)
	w := f.do("GET", "/api/state", "", cookie(sessionCookie, "planted"), cookie(sessionCookie, f.ownerSess))
	if w.Code != http.StatusOK {
		t.Errorf("signed out by a planted cookie: %d", w.Code)
	}
}

// With a wildcard, every container gets an origin of its own.
func TestPreviewOriginPerContainer(t *testing.T) {
	f := newPreviewFixture(t)
	f.opt.PreviewURL = "https://*.preview.example.com"
	label := previewLabel("host:" + f.owner) // the owner's host-run sessions' target
	own := label + ".preview.example.com"
	w := f.do("POST", "/api/sessions/"+f.sessionID+"/preview", fmt.Sprintf(`{"port":%d}`, f.dev.port), cookie(sessionCookie, f.ownerSess))
	var out struct{ URL string }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	u, err := url.Parse(out.URL)
	if err != nil || u.Host != own {
		t.Fatalf("link %q, want one on %s", out.URL, own)
	}
	tok := u.Query().Get("t")
	at := func(host string) reqOpt { return func(r *http.Request) { r.Host = host } }

	// Spent on another container's name: refused.
	other := previewLabel("c:p9/u9") + ".preview.example.com"
	if w := f.onPreview("GET", preview.OpenPath+"?t="+tok, at(other)); w.Code != http.StatusForbidden || previewCookie(t, w) != "" {
		t.Errorf("link opened on another container's origin: %d", w.Code)
	}
	w = f.do("POST", "/api/sessions/"+f.sessionID+"/preview", fmt.Sprintf(`{"port":%d}`, f.dev.port), cookie(sessionCookie, f.ownerSess))
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	u, _ = url.Parse(out.URL)
	// Its own origin needs no Collie cookie (there is none on that name),
	// and needs nothing cleared: nothing else ever ran there.
	w = f.onPreview("GET", u.RequestURI(), at(own))
	c := previewCookie(t, w)
	if w.Code != http.StatusSeeOther || c == "" || w.Header().Get("Clear-Site-Data") != "" {
		t.Fatalf("open on its own origin: %d %v", w.Code, w.Header())
	}
	w = f.onPreview("GET", "/", cookie(preview.CookieName(0), c), at(own), header("Origin", "https://"+own))
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "public, max-age=31536000" {
		t.Errorf("preview on its own origin: %d %v", w.Code, w.Header())
	}
	// The cookie is only good on its own origin, and other names are no
	// preview origin at all.
	if w := f.onPreview("GET", "/", cookie(preview.CookieName(0), c), at(other)); w.Code != http.StatusUnauthorized {
		t.Errorf("cookie on another container's origin: %d", w.Code)
	}
	for _, h := range []string{"preview.example.com", "evil.example", "a.b.preview.example.com", "collie.example.com"} {
		if w := f.onPreview("GET", "/", cookie(preview.CookieName(0), c), at(h)); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", h, w.Code)
		}
	}
	// Certificates only for the names of containers in use.
	if !f.PreviewHost(own) || f.PreviewHost(other) || f.PreviewHost("collie.example.com") || f.PreviewHost("x.preview.example.com") {
		t.Error("PreviewHost")
	}
}

// Signing out must end the real sign-in even when a preview page planted a
// cookie of the same name in front of it (and so the previews made from it).
func TestSignOutEndsTheRealSignIn(t *testing.T) {
	f := newPreviewFixture(t)
	w := f.do("POST", "/api/auth/logout", "", cookie(sessionCookie, "planted"), cookie(sessionCookie, f.ownerSess),
		header("Origin", "https://collie.example.com"))
	if w.Code != http.StatusOK {
		t.Fatalf("logout: %d", w.Code)
	}
	if _, _, ok := f.users.SessionUser(f.ownerSess, ""); ok {
		t.Error("the real sign-in survived signing out")
	}
}
