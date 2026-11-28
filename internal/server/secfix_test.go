package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/tattsun/collie/internal/model"
	"github.com/tattsun/collie/internal/term"
)

var overHTTPS = header("X-Forwarded-Proto", "https")

// usersWithPasskeys gives the test server two members with passkeys and
// sessions of their own.
func usersWithPasskeys(t *testing.T, s *testServer) (victim, mallory string) {
	t.Helper()
	_ = s.users.AddPasskey(s.owner, webauthn.Credential{ID: []byte("c0")}, "")
	v, _ := s.users.CreateUser("victim", "", false)
	m, _ := s.users.CreateUser("mallory", "", false)
	_ = s.users.AddPasskey(v.ID, webauthn.Credential{ID: []byte("c1")}, "")
	_ = s.users.AddPasskey(m.ID, webauthn.Credential{ID: []byte("c2")}, "")
	victim, _ = s.users.NewSession(v.ID, "iPhone", "")
	mallory, _ = s.users.NewSession(m.ID, "Mac", "")
	return victim, mallory
}

// A page on another port of Collie's name plants someone else's (valid)
// session cookie for a narrower path: the victim's requests must not go to
// that account.
func TestPlantedSessionOfAnotherUserSignsNobodyIn(t *testing.T) {
	s := newTestServer(t, Options{})
	victim, mallory := usersWithPasskeys(t, s)
	app := header("Origin", "https://collie.example.com")
	w := s.do("PUT", "/api/me/env", `{"env":"GH_TOKEN=victims-secret-token"}`,
		cookie(sessionCookie, mallory), cookie(sessionCookie, victim), app, overHTTPS)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("two accounts' cookies: %d %s", w.Code, w.Body.String())
	}
	if w := s.do("GET", "/api/me/env", "", cookie(sessionCookie, mallory)); strings.Contains(w.Body.String(), "victims-secret-token") {
		t.Fatal("the victim's token went to the planted account")
	}
	// Once the browser has the __Host- cookie, plain-named ones don't count.
	w = s.do("GET", "/api/state", "", cookie(sessionCookie, mallory), cookie(hostSessionCookie, victim), overHTTPS)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"victim"`) {
		t.Fatalf("with the __Host- cookie: %d %s", w.Code, w.Body.String())
	}
	// The same session twice (e.g. while the cookie moves) is fine.
	if w := s.do("GET", "/api/state", "", cookie(sessionCookie, victim), cookie(sessionCookie, victim)); w.Code != http.StatusOK {
		t.Fatalf("one session twice: %d", w.Code)
	}
}

func setCookies(w *httptest.ResponseRecorder) map[string]*http.Cookie {
	out := map[string]*http.Cookie{}
	for _, c := range (&http.Response{Header: w.Header()}).Cookies() {
		out[c.Name] = c
	}
	return out
}

// Over HTTPS a sign-in made with the plain cookie moves to the __Host-
// one, for the same session; over plain HTTP it stays.
func TestSessionCookieMovesToHostPrefix(t *testing.T) {
	s := newTestServer(t, Options{})
	victim, _ := usersWithPasskeys(t, s)
	w := s.do("GET", "/api/state", "", cookie(sessionCookie, victim), overHTTPS)
	cs := setCookies(w)
	hc, old := cs[hostSessionCookie], cs[sessionCookie]
	if w.Code != 200 || hc == nil || hc.Value != victim || !hc.Secure || !hc.HttpOnly || hc.Path != "/" || hc.Domain != "" {
		t.Fatalf("no __Host- cookie for the session: %d %+v", w.Code, hc)
	}
	if old == nil || old.MaxAge >= 0 {
		t.Fatalf("plain cookie not expired: %+v", old)
	}
	if w := s.do("GET", "/api/state", "", cookie(hostSessionCookie, victim), overHTTPS); len(setCookies(w)) != 0 {
		t.Fatalf("cookies set again: %v", w.Header()["Set-Cookie"])
	}
	if w := s.do("GET", "/api/state", "", cookie(sessionCookie, victim)); len(setCookies(w)) != 0 {
		t.Fatalf("plain HTTP got: %v", w.Header()["Set-Cookie"])
	}
	// Signing out ends the session and clears both names.
	w = s.do("POST", "/api/auth/logout", "", cookie(hostSessionCookie, victim), overHTTPS)
	cs = setCookies(w)
	if hc := cs[hostSessionCookie]; hc == nil || hc.Value != signedOutValue || hc.MaxAge <= 0 || !hc.Secure || !hc.HttpOnly || cs[sessionCookie] == nil || cs[sessionCookie].MaxAge >= 0 {
		t.Fatalf("logout cookies: %v", w.Header()["Set-Cookie"])
	}
	if w := s.do("GET", "/api/state", "", cookie(hostSessionCookie, victim)); w.Code != http.StatusUnauthorized {
		t.Fatalf("after logout: %d", w.Code)
	}
	// The deleting cookies are Secure over HTTPS, and not over plain HTTP,
	// where browsers would ignore them.
	if !cs[sessionCookie].Secure || cs[legacyCookie] == nil || !cs[legacyCookie].Secure {
		t.Errorf("deleting cookies over HTTPS: %v", w.Header()["Set-Cookie"])
	}
	w = s.do("POST", "/api/auth/logout", "", cookie(legacyCookie, "token"))
	cs = setCookies(w)
	if w.Code != 200 || cs[sessionCookie] == nil || cs[sessionCookie].Secure || cs[legacyCookie] == nil || cs[legacyCookie].Secure || cs[legacyCookie].MaxAge >= 0 {
		t.Errorf("deleting cookies over plain HTTP: %d %v", w.Code, w.Header()["Set-Cookie"])
	}
}

// Plugins run in everyone's (or every member's) containers, and who is in
// a group decides who keeps its files: like the global settings, these
// need a recent passkey check.
func TestPluginsAndMembershipNeedRecentPasskey(t *testing.T) {
	s := newTestServer(t, Options{})
	_ = s.users.AddPasskey(s.owner, webauthn.Credential{ID: []byte("c0")}, "")
	bob, _ := s.users.CreateUser("bob", "", false)
	g, err := s.users.CreateGroup("team", s.owner)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.users.SetMember(g.ID, bob.ID, "member")
	s.m.Store().PutProject(model.Project{ID: "gp", Name: "gp", Path: t.TempDir(), OwnerGroup: g.ID, Runtime: "container"})
	s.m.Store().PutProject(model.Project{ID: "mine", Name: "mine", Path: t.TempDir(), OwnerUser: s.owner, Runtime: "container"})
	secret, _ := s.users.NewSession(s.owner, "x", "")
	s.stalePasskeyCheck(t)
	me := cookie(sessionCookie, secret)
	plugins := `{"plugins":{"enabledPlugins":{"evil@attacker":true}}}`
	// A personal project's plugins reach only its owner's containers.
	if w := s.do("PUT", "/api/projects/mine/plugins", plugins, me); strings.Contains(w.Body.String(), "reauth") {
		t.Errorf("personal project plugins asked for a passkey: %d %s", w.Code, w.Body.String())
	}
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/api/shared/plugins", plugins},
		{"PUT", "/api/groups/" + g.ID + "/plugins", plugins},
		{"POST", "/api/groups/" + g.ID + "/members", `{"user":"bob","role":"owner"}`},
		{"DELETE", "/api/groups/" + g.ID + "/members/" + bob.ID, ""},
		{"PUT", "/api/projects/gp/plugins", plugins},
	} {
		w := s.do(c.method, c.path, c.body, me)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "reauth") {
			t.Errorf("%s %s with a stale check: %d %s", c.method, c.path, w.Code, w.Body.String())
		}
	}
	if s.users.Role(g.ID, bob.ID) != "member" {
		t.Fatal("membership changed")
	}
	// Leaving a group yourself needs nothing.
	bobSecret, _ := s.users.NewSession(bob.ID, "x", "")
	_ = s.users.AddPasskey(bob.ID, webauthn.Credential{ID: []byte("c1")}, "")
	s.stalePasskeyCheck(t)
	if w := s.do("DELETE", "/api/groups/"+g.ID+"/members/"+bob.ID, "", cookie(sessionCookie, bobSecret)); w.Code != 200 {
		t.Fatalf("leaving: %d %s", w.Code, w.Body.String())
	}
}

// On a server without podman a project that names no runtime runs on the
// host, so it needs the same passkey check as asking for the host; and an
// admin's clone of a local path (another user's project) is refused.
func TestProjectsOnTheHostNeedRecentPasskey(t *testing.T) {
	s := newTestServer(t, Options{})
	_ = s.users.AddPasskey(s.owner, webauthn.Credential{ID: []byte("c0")}, "")
	secret, _ := s.users.NewSession(s.owner, "x", "")
	s.stalePasskeyCheck(t)
	me := cookie(sessionCookie, secret)
	w := s.do("POST", "/api/projects", `{"name":"p1","git":false}`, me)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "reauth") {
		t.Fatalf("host project by default: %d %s", w.Code, w.Body.String())
	}
	verified, _ := s.users.NewSession(s.owner, "x", "")
	for _, u := range []string{"p1/../../bob/secret", "bob/secret", "file:///etc", "ext::sh -c id", "-u/x", "C:/x"} {
		body, _ := json.Marshal(map[string]any{"kind": "clone", "url": u, "name": "stolen", "runtime": "host"})
		w := s.do("POST", "/api/projects", string(body), cookie(sessionCookie, verified))
		if w.Code != http.StatusBadRequest {
			t.Errorf("clone %q: %d %s", u, w.Code, w.Body.String())
		}
	}
}

// Cloning a host-run project again runs git on the server, like making
// it: that needs a recent passkey check. An isolated one's doesn't.
func TestCloneAgainOnTheHostNeedsRecentPasskey(t *testing.T) {
	s := newTestServer(t, Options{})
	_ = s.users.AddPasskey(s.owner, webauthn.Credential{ID: []byte("c0")}, "")
	stale, _ := s.users.NewSession(s.owner, "x", "")
	s.stalePasskeyCheck(t)
	verified, _ := s.users.NewSession(s.owner, "x", "")
	for _, p := range []model.Project{
		{ID: "hp", Name: "hp", Path: t.TempDir(), Runtime: "host"},
		{ID: "ip", Name: "ip", Path: t.TempDir(), Runtime: "container"},
	} {
		p.OwnerUser, p.Status, p.Error, p.CloneURL = s.owner, model.ProjectCloneFailed, "git clone: failed", "https://github.com/a/b.git"
		s.m.Store().PutProject(p)
	}
	// file:// is refused once past the check, so nothing is cloned.
	w := s.do("POST", "/api/projects/hp/clone", `{"url":"file:///etc"}`, cookie(sessionCookie, stale))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "reauth") {
		t.Fatalf("host project, stale check: %d %s", w.Code, w.Body.String())
	}
	w = s.do("POST", "/api/projects/hp/clone", `{"url":"file:///etc"}`, cookie(sessionCookie, verified))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "only remote repository URLs") {
		t.Fatalf("host project, fresh check: %d %s", w.Code, w.Body.String())
	}
	w = s.do("POST", "/api/projects/ip/clone", `{}`, cookie(sessionCookie, stale))
	if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "reauth") {
		t.Fatalf("isolated project, stale check: %d %s", w.Code, w.Body.String())
	}
}

// Anyone can start sign-ins; a flood of them, from many addresses, must
// neither lock everyone out nor block signed-in users' passkey checks.
func TestCeremonyFloodBlocksNobody(t *testing.T) {
	s := newTestServer(t, Options{})
	_ = s.users.AddPasskey(s.owner, webauthn.Credential{ID: []byte("c1"), PublicKey: []byte{1}}, "")
	secret, _ := s.users.NewSession(s.owner, "test", "")
	for i := 0; i < 300; i++ {
		xff := header("X-Forwarded-For", fmt.Sprintf("2001:db8:%x::1", i)) // a /64 each
		for j := 0; j < 10; j++ {
			_ = s.do("POST", "/api/auth/login/begin", "", xff, overHTTPS)
		}
	}
	if w := s.do("POST", "/api/auth/login/begin", "", header("X-Forwarded-For", "198.51.100.77"), overHTTPS); w.Code != 200 {
		t.Fatalf("a sign-in after the flood: %d %s", w.Code, w.Body.String())
	}
	if w := s.do("POST", "/api/auth/verify/begin", "", cookie(sessionCookie, secret), header("X-Forwarded-For", "198.51.100.78"), overHTTPS); w.Code != 200 {
		t.Fatalf("a passkey check after the flood: %d %s", w.Code, w.Body.String())
	}
}

// The Origin must match the scheme too: a page served over plain HTTP on
// Collie's name (or with a mixed-up port) can't drive the HTTPS one.
func TestOriginSchemeCounts(t *testing.T) {
	s := newTestServer(t, Options{})
	_ = s.users.AddPasskey(s.owner, webauthn.Credential{ID: []byte("c1")}, "")
	secret, _ := s.users.NewSession(s.owner, "test", "")
	me := cookie(sessionCookie, secret)
	for o, want := range map[string]int{
		"https://collie.example.com":      200,
		"https://collie.example.com:443":  200,
		"http://collie.example.com":       403,
		"http://collie.example.com:443":   403,
		"https://collie.example.com:80":   403,
		"https://collie.example.com:8443": 403,
	} {
		if w := s.do("PATCH", "/api/me", `{"displayName":"x"}`, me, header("Origin", o), overHTTPS); w.Code != want {
			t.Errorf("Origin %s: %d, want %d", o, w.Code, want)
		}
	}
	// Over plain HTTP (a trial on the LAN), the http:// origin is the one.
	if w := s.do("PATCH", "/api/me", `{"displayName":"x"}`, me, header("Origin", "http://collie.example.com")); w.Code != 200 {
		t.Errorf("plain HTTP: %d", w.Code)
	}
	// tailscale serve that doesn't say it's HTTPS: *.ts.net is HTTPS only.
	if w := s.do("PATCH", "/api/me", `{"displayName":"x"}`, me, host("box.tail1.ts.net"), header("Origin", "https://box.tail1.ts.net")); w.Code != 200 {
		t.Errorf("tailscale serve: %d", w.Code)
	}
}

// An agent's HTML page runs scripts only inside Collie's viewer frame;
// opened as a page of its own (a new tab), it never does.
func TestUploadedPagesRunScriptsOnlyInTheFrame(t *testing.T) {
	s := newTestServer(t, Options{})
	secret, _ := s.users.NewSession(s.owner, "x", "")
	name := "0123456789abcdef.html"
	p, _ := s.m.UploadPath(name)
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte("<script>1</script>"), 0o600); err != nil {
		t.Fatal(err)
	}
	get := func(opts ...reqOpt) string {
		w := s.do("GET", "/api/uploads/"+name+"?js=1", "", append(opts, cookie(sessionCookie, secret))...)
		if w.Code != 200 || !strings.Contains(w.Header().Get("Vary"), "Sec-Fetch-Dest") {
			t.Fatalf("%d, Vary %q", w.Code, w.Header().Get("Vary"))
		}
		return w.Header().Get("Content-Security-Policy")
	}
	if csp := get(header("Sec-Fetch-Dest", "iframe")); !strings.HasPrefix(csp, "sandbox allow-scripts;") || !strings.Contains(csp, "webrtc 'block'") {
		t.Errorf("in the frame: %q", csp)
	}
	for _, dest := range []string{"document", ""} {
		if csp := get(header("Sec-Fetch-Dest", dest)); !strings.HasPrefix(csp, "sandbox;") || !strings.Contains(csp, "webrtc 'block'") {
			t.Errorf("as a page (%q): %q", dest, csp)
		}
	}
}

// Without sign-in, a proxy on this machine (vite) may pass on requests only
// from this machine: one listening on the LAN passes on anyone's.
func TestNoAuthThroughALocalProxy(t *testing.T) {
	s := newTestServer(t, Options{NoAuth: true})
	vite := host("localhost:5173")
	for name, c := range map[string]struct {
		opts []reqOpt
		ok   bool
	}{
		"from this machine":   {[]reqOpt{vite, header("X-Forwarded-For", "127.0.0.1"), header("X-Forwarded-Host", "localhost:5173"), header("X-Forwarded-Proto", "http")}, true},
		"from ::1":            {[]reqOpt{vite, header("X-Forwarded-For", "::1")}, true},
		"from the LAN":        {[]reqOpt{vite, header("X-Forwarded-For", "192.168.1.30")}, false},
		"from the LAN, later": {[]reqOpt{vite, header("X-Forwarded-For", "127.0.0.1, 192.168.1.30")}, false},
		"to another name":     {[]reqOpt{vite, header("X-Forwarded-For", "127.0.0.1"), header("X-Forwarded-Host", "evil.example")}, false},
		"garbage":             {[]reqOpt{vite, header("X-Forwarded-For", "nope")}, false},
	} {
		if w := s.do("GET", "/api/auth/status", "", c.opts...); (w.Code == 200) != c.ok {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
}

// The guided Claude sign-in offers only Anthropic's own sign-in links:
// whatever else runs in the Home container could print one first.
func TestSetupTokenLinkMustBeAnthropics(t *testing.T) {
	real := "https://claude.ai/oauth/authorize?code=true&client_id=abc&redirect_uri=https%3A%2F%2Fconsole.anthropic.com%2Foauth%2Fcode%2Fcallback"
	osc := func(u string) string { return "\x1b]8;;" + u + "\x07" + u + "\x1b]8;;\x07\r\n" }
	for _, fake := range []string{
		"https://claude-ai.attacker.example/oauth/authorize",
		"https://claude.ai.attacker.example/oauth",
		"http://claude.ai/oauth/authorize",
		"https://claude.ai/oauth/authorize?redirect_uri=https%3A%2F%2Fevil.example%2Fcb",
		"https://user@claude.ai/oauth",
	} {
		if url, _ := parseSetupToken(osc(fake) + osc(real)); url != real {
			t.Errorf("after %s: %q", fake, url)
		}
		if url, _ := parseSetupToken("Visit " + fake + "\r\n"); url != "" {
			t.Errorf("plain %s accepted", fake)
		}
	}
	l := &claudeLogin{url: real}
	if h := l.snapshot()["host"]; h != "claude.ai" {
		t.Errorf("host %v", h)
	}
}

// Losing admin rights ends the shells on the server itself at once.
func TestDemotedAdminLosesHostShell(t *testing.T) {
	s := newTestServer(t, Options{})
	_ = s.users.AddPasskey(s.owner, webauthn.Credential{ID: []byte("c0")}, "")
	bob, _ := s.users.CreateUser("bob", "", true)
	_ = s.users.AddPasskey(bob.ID, webauthn.Credential{ID: []byte("c1")}, "")
	ownerSecret, _ := s.users.NewSession(s.owner, "x", "")
	bobSecret, _ := s.users.NewSession(bob.ID, "x", "")
	asBob := cookie(sessionCookie, bobSecret)
	w := s.do("POST", "/api/projects", `{"name":"hostp","runtime":"host","git":false}`, asBob)
	if w.Code != 200 {
		t.Fatalf("create host project: %d %s", w.Code, w.Body.String())
	}
	var p struct{ ID string }
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	w = s.do("POST", "/api/terms", fmt.Sprintf(`{"projectId":%q,"cols":80,"rows":24}`, p.ID), asBob)
	if w.Code != 200 {
		t.Fatalf("create term: %d %s", w.Code, w.Body.String())
	}
	var ti struct{ ID string }
	_ = json.Unmarshal(w.Body.Bytes(), &ti)
	tt, _ := s.terms.Get(ti.ID)
	defer tt.Close()
	if w := s.do("PATCH", "/api/admin/users/"+bob.ID, `{"admin":false}`, cookie(sessionCookie, ownerSecret)); w.Code != 200 {
		t.Fatalf("demote: %d %s", w.Code, w.Body.String())
	}
	select {
	case <-tt.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the shell on the server is still running")
	}
	hs := httptest.NewServer(s)
	defer hs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u := strings.Replace(hs.URL, "http://", "ws://", 1) + "/api/terms/" + ti.ID + "/ws"
	if c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{
		"Cookie": {sessionCookie + "=" + bobSecret}, "Origin": {hs.URL}}}); err == nil {
		c.Close(websocket.StatusNormalClosure, "")
		t.Fatal("re-attached after the demotion")
	}
}

// A member removed from a group loses their shells in its projects.
func TestRemovedMemberLosesGroupShell(t *testing.T) {
	s := newTestServer(t, Options{})
	bob, _ := s.users.CreateUser("bob", "", false)
	g, _ := s.users.CreateGroup("team", s.owner)
	_ = s.users.SetMember(g.ID, bob.ID, "member")
	p := model.Project{ID: "gp", Name: "app", Path: t.TempDir(), OwnerGroup: g.ID, Runtime: "container"}
	s.m.Store().PutProject(p)
	// A shell "in the container" (a stand-in process).
	tt, err := s.terms.Create(term.Spec{Cols: 80, Rows: 24, User: bob.ID, Project: p.ID, Owner: p.ID, Cwd: p.Path, Argv: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	defer tt.Close()
	if !s.termAllowed(tt.Info) {
		t.Fatal("a member's shell not allowed")
	}
	if err := s.users.SetMember(g.ID, bob.ID, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tt.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the removed member's shell is still running")
	}
}

// A browser signed out over HTTPS keeps an HttpOnly __Host- placeholder:
// a page on another port then can't create that cookie with someone
// else's sign-in, and a plain-named one it plants doesn't count.
func TestSignedOutBrowserCantBeSignedInAsSomeoneElse(t *testing.T) {
	s := newTestServer(t, Options{Web: fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}})
	victim, mallory := usersWithPasskeys(t, s)
	// A browser opening Collie gets the placeholder, unless it has a
	// sign-in of the plain name (which the API moves instead).
	if hc := setCookies(s.do("GET", "/", "", overHTTPS))[hostSessionCookie]; hc == nil || hc.Value != signedOutValue || !hc.HttpOnly {
		t.Fatalf("no placeholder: %+v", hc)
	}
	if hc := setCookies(s.do("GET", "/", "", cookie(sessionCookie, victim), overHTTPS))[hostSessionCookie]; hc != nil {
		t.Fatalf("placeholder over a plain sign-in: %+v", hc)
	}
	if cs := setCookies(s.do("GET", "/", "")); len(cs) != 0 {
		t.Fatalf("plain HTTP got cookies: %v", cs)
	}
	if hc := setCookies(s.do("GET", "/", "", cookie(hostSessionCookie, signedOutValue), overHTTPS))[hostSessionCookie]; hc != nil {
		t.Fatalf("placeholder set again: %+v", hc)
	}
	w := s.do("GET", "/api/state", "", cookie(hostSessionCookie, signedOutValue), cookie(sessionCookie, mallory), overHTTPS)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("planted plain cookie next to the placeholder: %d", w.Code)
	}
}
