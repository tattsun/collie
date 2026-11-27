package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/tattsun/collie/internal/auth"
	"github.com/tattsun/collie/internal/session"
	"github.com/tattsun/collie/internal/store"
	"github.com/tattsun/collie/internal/term"
)

// Tests for running Collie where anyone on the internet can reach it.

type testServer struct {
	*Server
	users *auth.Store
	data  string
	owner string
}

func newTestServer(t *testing.T, opt Options) *testServer {
	t.Helper()
	data := t.TempDir()
	st, err := store.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	users, err := auth.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := users.EnsureBootstrapAdmin("admin")
	if err != nil {
		t.Fatal(err)
	}
	mgr := session.NewManager(session.Config{ClaudeBin: "/nonexistent/claude", CodexBin: "/nonexistent/codex",
		ProjectRoot: t.TempDir(), HostOwner: owner}, st, session.NewHub(), nil)
	mgr.SetAccess(users)
	opt.Owner = owner
	if opt.Token == "" && !opt.NoAuth {
		opt.Token = "the-setup-token"
	}
	return &testServer{Server: New(mgr, term.NewManager(), users, opt), users: users, data: data, owner: owner}
}

type reqOpt func(*http.Request)

func from(addr string) reqOpt { return func(r *http.Request) { r.RemoteAddr = addr } }
func host(h string) reqOpt    { return func(r *http.Request) { r.Host = h } }
func header(k, v string) reqOpt {
	return func(r *http.Request) { r.Header.Set(k, v) }
}
func cookie(name, v string) reqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: name, Value: v}) }
}

func (s *testServer) do(method, path, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	return s.serve(r, opts...)
}

// doBody sends a request with a body of the given content type.
func (s *testServer) doBody(method, path string, body io.Reader, ctype string, opts ...reqOpt) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, body)
	r.Header.Set("Content-Type", ctype)
	return s.serve(r, opts...)
}

func (s *testServer) serve(r *http.Request, opts ...reqOpt) *httptest.ResponseRecorder {
	r.RemoteAddr = "127.0.0.1:50000"
	r.Host = "collie.example.com"
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func signedIn(t *testing.T, w *httptest.ResponseRecorder) bool {
	t.Helper()
	var st struct {
		User *userInfo `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("status: %v (%s)", err, w.Body.String())
	}
	return st.User != nil
}

// Before the first passkey, only the setup token signs anyone in: not a
// Tailscale identity header, which any proxy running as root (cloudflared,
// for one) would pass through from the internet.
func TestSetupNeedsTheToken(t *testing.T) {
	s := newTestServer(t, Options{})
	if signedIn(t, s.do("GET", "/api/auth/status", "", header("Tailscale-User-Login", "alice@example.com"))) {
		t.Fatal("a Tailscale identity header signed someone in")
	}
	if signedIn(t, s.do("GET", "/api/auth/status", "")) {
		t.Fatal("signed in without anything")
	}
	if !signedIn(t, s.do("GET", "/api/auth/status", "", header("Authorization", "Bearer the-setup-token"))) {
		t.Fatal("the setup token did not sign in")
	}
	// Once a passkey exists, the token stops working too.
	if err := s.users.AddPasskey(s.owner, webauthn.Credential{ID: []byte("c1")}, ""); err != nil {
		t.Fatal(err)
	}
	if signedIn(t, s.do("GET", "/api/auth/status", "", header("Authorization", "Bearer the-setup-token"))) {
		t.Fatal("the setup token still works after a passkey exists")
	}
}

// --no-auth must only ever answer this machine's own browser.
func TestNoAuthServesOnlyThisMachine(t *testing.T) {
	s := newTestServer(t, Options{NoAuth: true})
	cases := []struct {
		name string
		opts []reqOpt
		ok   bool
	}{
		{"loopback", []reqOpt{host("127.0.0.1:7878")}, true},
		{"localhost (vite)", []reqOpt{host("localhost:5173")}, true},
		{"ipv6 loopback", []reqOpt{from("[::1]:5000"), host("[::1]:7878")}, true},
		{"another machine", []reqOpt{from("192.168.1.20:5000"), host("192.168.1.10:7878")}, false},
		{"DNS rebinding", []reqOpt{host("evil.example:7878")}, false},
		{"through a proxy", []reqOpt{host("127.0.0.1:7878"), header("X-Forwarded-For", "203.0.113.9")}, false},
		{"tailscale funnel", []reqOpt{host("127.0.0.1:7878"), header("Tailscale-Funnel-Request", "?1")}, false},
	}
	for _, c := range cases {
		w := s.do("GET", "/api/auth/status", "", c.opts...)
		if got := w.Code == http.StatusOK; got != c.ok {
			t.Errorf("%s: status %d", c.name, w.Code)
		}
	}
}

func TestSetupTokenGuessingIsThrottled(t *testing.T) {
	s := newTestServer(t, Options{})
	attacker := header("X-Forwarded-For", "203.0.113.7")
	throttled := false
	for i := 0; i < 8; i++ {
		w := s.do("POST", "/api/login", `{"token":"guess"}`, attacker)
		if w.Code == http.StatusTooManyRequests {
			if w.Header().Get("Retry-After") == "" {
				t.Fatal("429 without Retry-After")
			}
			throttled = true
			break
		}
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d: status %d", i, w.Code)
		}
	}
	if !throttled {
		t.Fatal("wrong tokens were never throttled")
	}
	// Even the right token waits now …
	if w := s.do("POST", "/api/login", `{"token":"the-setup-token"}`, attacker); w.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled client got through: %d", w.Code)
	}
	// … but nobody else is affected.
	if w := s.do("POST", "/api/login", `{"token":"the-setup-token"}`, header("X-Forwarded-For", "198.51.100.3")); w.Code != http.StatusOK {
		t.Fatalf("another client was throttled: %d %s", w.Code, w.Body.String())
	}
}

// stalePasskeyCheck makes every login session's passkey check old, the way
// it is some time after signing in.
func (s *testServer) stalePasskeyCheck(t *testing.T) {
	t.Helper()
	s.users.Flush()
	p := filepath.Join(s.data, "auth.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	for _, x := range d["sessions"].([]any) {
		x.(map[string]any)["verifiedAt"] = time.Now().Add(-auth.VerifyTTL - time.Minute).UnixMilli()
	}
	b, _ = json.Marshal(d)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Second)
	_ = os.Chtimes(p, later, later)
}

func TestSensitiveChangesNeedARecentPasskeyCheck(t *testing.T) {
	s := newTestServer(t, Options{})
	if err := s.users.AddPasskey(s.owner, webauthn.Credential{ID: []byte("c1")}, ""); err != nil {
		t.Fatal(err)
	}
	secret, err := s.users.NewSession(s.owner, "test", "203.0.113.1")
	if err != nil {
		t.Fatal(err)
	}
	me := cookie(sessionCookie, secret)
	invite := func() *httptest.ResponseRecorder { return s.do("POST", "/api/admin/invites", `{"admin":true}`, me) }

	// Right after signing in: fine.
	if w := invite(); w.Code != http.StatusOK {
		t.Fatalf("fresh session: %d %s", w.Code, w.Body.String())
	}
	s.stalePasskeyCheck(t)
	w := invite()
	var res struct {
		Reauth bool `json:"reauth"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if w.Code != http.StatusForbidden || !res.Reauth {
		t.Fatalf("stale session: %d %s", w.Code, w.Body.String())
	}
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/me/link", ""},
		{"POST", "/api/me/passkeys/begin", `{}`},
		{"DELETE", "/api/me/passkeys/x", ""},
		{"PATCH", "/api/admin/users/" + s.owner, `{"displayName":"x"}`},
		{"POST", "/api/admin/users/" + s.owner + "/link", ""},
		{"PUT", "/api/shared/env", `{"env":"A=1"}`},
		{"POST", "/api/projects", `{"kind":"existing","path":"/tmp"}`},
	} {
		if w := s.do(c.method, c.path, c.body, me); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"reauth":true`) {
			t.Errorf("%s %s without a recent passkey check: %d %s", c.method, c.path, w.Code, w.Body.String())
		}
	}
	// Everyday things don't ask.
	if w := s.do("GET", "/api/me", "", me); w.Code != http.StatusOK {
		t.Fatalf("GET /api/me: %d", w.Code)
	}
	// A passkey check (what /api/auth/verify/finish records) lets it through.
	_, sess, _ := s.users.SessionUser(secret, "")
	if err := s.users.MarkVerified(sess.Hash); err != nil {
		t.Fatal(err)
	}
	if w := invite(); w.Code != http.StatusOK {
		t.Fatalf("after re-verifying: %d %s", w.Code, w.Body.String())
	}
}

func TestSignOutOneDevice(t *testing.T) {
	s := newTestServer(t, Options{})
	_ = s.users.AddPasskey(s.owner, webauthn.Credential{ID: []byte("c1")}, "")
	mine, _ := s.users.NewSession(s.owner, "Mac", "")
	lost, _ := s.users.NewSession(s.owner, "iPhone", "")
	_, lostSess, _ := s.users.SessionUser(lost, "")
	if w := s.do("DELETE", "/api/me/sessions/"+lostSess.ID(), "", cookie(sessionCookie, mine)); w.Code != http.StatusOK {
		t.Fatalf("sign out: %d %s", w.Code, w.Body.String())
	}
	if signedIn(t, s.do("GET", "/api/auth/status", "", cookie(sessionCookie, lost))) {
		t.Fatal("the signed-out device is still signed in")
	}
	if !signedIn(t, s.do("GET", "/api/auth/status", "", cookie(sessionCookie, mine))) {
		t.Fatal("this device was signed out")
	}
}

func TestHSTSOnlyOverHTTPS(t *testing.T) {
	s := newTestServer(t, Options{})
	if w := s.do("GET", "/api/auth/status", ""); w.Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("HSTS over plain HTTP")
	}
	if w := s.do("GET", "/api/auth/status", "", header("X-Forwarded-Proto", "https")); w.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("no HSTS behind an HTTPS proxy")
	}
	// Only a proxy on this machine is believed.
	if w := s.do("GET", "/api/auth/status", "", from("203.0.113.5:4000"), header("X-Forwarded-Proto", "https")); w.Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("X-Forwarded-Proto believed from a remote client")
	}
}

// A reverse proxy that passes on its own Host (nginx's default) still works
// for the address in --public-url.
func TestPublicURLIsSameOrigin(t *testing.T) {
	s := newTestServer(t, Options{PublicURL: "https://collie.example.com"})
	w := s.do("GET", "/api/auth/status", "", host("127.0.0.1:7878"), header("Origin", "https://collie.example.com"))
	if w.Code != http.StatusOK {
		t.Fatalf("public URL origin rejected: %d", w.Code)
	}
	if w := s.do("GET", "/api/auth/status", "", host("127.0.0.1:7878"), header("Origin", "https://evil.example")); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin accepted: %d", w.Code)
	}
}

func TestFailureLogCannotBeForged(t *testing.T) {
	var buf strings.Builder
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	s := newTestServer(t, Options{})
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "203.0.113.4:1"
	s.authFailed(r, "passkey sign-in", errors.New("bad origin\n2026/01/01 00:00:00 auth: failed passkey sign-in from 198.51.100.1"))
	if n := strings.Count(strings.TrimSpace(buf.String()), "\n"); n != 0 {
		t.Fatalf("one failure became %d lines:\n%s", n+1, buf.String())
	}
	if !strings.Contains(buf.String(), "auth: failed passkey sign-in from 203.0.113.4 ") {
		t.Fatalf("log line: %s", buf.String())
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("X-Forwarded-For", "10.0.0.1, 203.0.113.8")
	if got := clientIP(r); got != "203.0.113.8" {
		t.Fatalf("behind a local proxy: %q", got)
	}
	r.RemoteAddr = "198.51.100.2:1"
	if got := clientIP(r); got != "198.51.100.2" {
		t.Fatalf("a remote client chose its own address: %q", got)
	}
	if limitKey("2001:db8:1:2:3:4:5:6") != limitKey("2001:db8:1:2:ffff::1") {
		t.Fatal("one IPv6 /64 counted as several clients")
	}
}

func TestLimiter(t *testing.T) {
	clock := time.Unix(1000, 0)
	l := newLimiter(60, 3, 2) // 1 per second, burst 3, 2 addresses
	l.now = func() time.Time { return clock }
	for i := 0; i < 3; i++ {
		if ok, _ := l.take("a", 1); !ok {
			t.Fatalf("burst request %d refused", i)
		}
	}
	if ok, wait := l.take("a", 1); ok || wait <= 0 {
		t.Fatal("over the burst")
	}
	clock = clock.Add(10 * time.Second)
	if ok, _ := l.take("a", 1); !ok {
		t.Fatal("did not refill")
	}
	// Spraying addresses can't grow the table: newcomers share a bucket.
	l.take("b", 1)
	for i := 0; i < 5; i++ {
		l.take(string(rune('c'+i)), 1)
	}
	if len(l.m) > 3 {
		t.Fatalf("table grew to %d", len(l.m))
	}
	if ok, _ := l.take("zz", 1); ok {
		t.Fatal("overflow bucket never ran dry")
	}
}
