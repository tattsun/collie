package preview

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// boundProxy is testProxy with Bind: a link opens, and a grant counts on
// the shared origin, only in the browser whose collie_sess is the grant's
// Login.
func boundProxy(t *testing.T) (*Proxy, *int) {
	p, _ := testProxy(t)
	binds := 0
	p.Bind = func(r *http.Request, g Grant) bool {
		binds++
		for _, n := range []string{"collie_sess", "__Host-collie_sess"} {
			for _, c := range r.CookiesNamed(n) {
				if c.Value == g.Login {
					return true
				}
			}
		}
		return false
	}
	return p, &binds
}

// openAs opens a grant in the browser signed in to Collie with sess.
func openAs(t *testing.T, p *Proxy, g Grant, sess string) string {
	t.Helper()
	r := httptest.NewRequest("GET", OpenPath+"?t="+p.Mint(g), nil)
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.AddCookie(&http.Cookie{Name: "collie_sess", Value: sess})
	w := httptest.NewRecorder()
	p.Slot(g.Slot).ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieName(g.Slot) {
			return c.Value
		}
	}
	t.Fatalf("open: %d %s", w.Code, w.Body.String())
	return ""
}

func get(p *Proxy, slot int, path, cookies string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Cookie", cookies)
	w := httptest.NewRecorder()
	p.Slot(slot).ServeHTTP(w, r)
	return w
}

// Another user's preview cookie, planted in the victim's browser for a
// narrower path by a page on Collie's host name, opens nothing there:
// alone it isn't the browser's, and next to the victim's own the request
// is refused rather than served by one of the two.
func TestPlantedGrantOfAnotherUser(t *testing.T) {
	victim := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "VICTIM") }))
	attacker := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ATTACKER") }))
	p, binds := boundProxy(t)
	// The attacker opens their own server in their own browser and reads
	// the cookie's value there.
	att := openAs(t, p, Grant{UserID: "u2", Login: "S2", Port: attacker, Target: "c:pX/u2", Slot: 1}, "S2")
	vic := openAs(t, p, Grant{UserID: "u1", Login: "S1", Port: victim, Target: "c:pY/u1", Slot: 1}, "S1")
	pc := CookieName(1)

	// The victim's own preview works, and Bind isn't asked again for every
	// request.
	n := *binds
	for range 3 {
		if w := get(p, 1, "/", pc+"="+vic+"; collie_sess=S1"); w.Code != http.StatusOK || w.Body.String() != "VICTIM" {
			t.Fatalf("victim's preview: %d %s", w.Code, w.Body.String())
		}
	}
	if *binds != n {
		t.Errorf("Bind asked %d times for the same sign-in", *binds-n)
	}

	// The planted cookie alone, in the victim's browser.
	if w := get(p, 1, "/", pc+"="+att+"; collie_sess=S1"); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "ATTACKER") {
		t.Errorf("planted grant alone: %d %s", w.Code, w.Body.String())
	}
	// Next to the victim's own (sent first, for the longer path).
	for i := range 20 {
		w := get(p, 1, "/src/main.js", pc+"="+att+"; "+pc+"="+vic+"; collie_sess=S1")
		if strings.Contains(w.Body.String(), "ATTACKER") {
			t.Fatalf("request %d reached the attacker's server", i)
		}
		if w.Code != http.StatusOK || w.Body.String() != "VICTIM" {
			t.Fatalf("victim's request with a planted cookie: %d %s", w.Code, w.Body.String())
		}
	}
	// With the attacker's Collie cookie planted too, both grants bind:
	// previews of two users at once are refused.
	w := get(p, 1, "/src/main.js", pc+"="+att+"; "+pc+"="+vic+"; collie_sess=S2; collie_sess=S1")
	if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "ATTACKER") || strings.Contains(w.Body.String(), "VICTIM") {
		t.Errorf("grants of two users at once: %d %s", w.Code, w.Body.String())
	}
	// The attacker's own browser still has its preview.
	if w := get(p, 1, "/", pc+"="+att+"; collie_sess=S2"); w.Code != http.StatusOK || w.Body.String() != "ATTACKER" {
		t.Errorf("attacker's own preview: %d %s", w.Code, w.Body.String())
	}
	// A __Host- sign-in cookie binds as well.
	if w := get(p, 1, "/", pc+"="+vic+"; __Host-collie_sess=S1"); w.Code != http.StatusOK {
		t.Errorf("victim's preview with the __Host- cookie: %d", w.Code)
	}
	// Signed out of Collie in this browser: the grant isn't this browser's.
	if w := get(p, 1, "/", pc+"="+vic); w.Code != http.StatusForbidden {
		t.Errorf("preview without the sign-in cookie: %d", w.Code)
	}
}

// Names of their own need no Collie cookie (there is none on them): Bind
// is for the shared origin only.
func TestOwnNamesNeedNoBind(t *testing.T) {
	port := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "OK") }))
	p, binds := boundProxy(t)
	p.Origin = func(*http.Request, int) (string, string, bool) { return "https://cabc.preview.test", "cabc", true }
	r := httptest.NewRequest("GET", OpenPath+"?t="+p.Mint(Grant{UserID: "u1", Login: "S1", Port: port, Target: "c:p/u1", Label: "cabc"}), nil)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	var c string
	for _, x := range w.Result().Cookies() {
		c = x.Value
	}
	if w := get(p, 0, "/", CookieName(0)+"="+c); w.Code != http.StatusOK {
		t.Fatalf("preview on its own name: %d %s", w.Code, w.Body.String())
	}
	if *binds != 0 {
		t.Errorf("Bind asked on a name of its own")
	}
}

// What a server sends back may not set Collie's cookies, under any name a
// browser would send as one of them.
func TestSetCookieCantReachCollie(t *testing.T) {
	for _, c := range []string{
		"collie_sess=x; Path=/", "__Host-collie_sess=x; Path=/; Secure", "__secure-collie_sess=x",
		"=collie_sess=x; Path=/api", "collie_sess", " =x", "collie_preview_1=x", "COLLIE_SESS=x",
		"_oauth2_proxy=x", "__Host-_pomerium=x",
	} {
		if got := cleanSetCookie(c); got != "" {
			t.Errorf("Set-Cookie %q passed as %q", c, got)
		}
	}
	if got := cleanSetCookie("app=1; Domain=collie.test; Path=/"); got != "app=1; Path=/" {
		t.Errorf("app cookie: %q", got)
	}
	h := http.Header{}
	h.Set("Cookie", "__Host-collie_sess=s; collie_sess=t; collie_auth=u; app=1; __Host-collie_preview_0=v")
	filterCookies(h)
	if got := h.Get("Cookie"); got != "app=1" {
		t.Errorf("cookies passed to the server: %q", got)
	}
}

// Servers can't have the browser send reports past the preview's origin
// (or, with NEL, about what opens on it later).
func TestReportingHeadersDropped(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Security-Policy-Report-Only", "default-src 'none'; report-uri https://evil.example/r; report-to e")
	h.Add("Content-Security-Policy", "img-src 'none'; report-uri https://evil.example/r2")
	h.Add("Content-Security-Policy", "report-to e")
	h.Add("Content-Security-Policy", "script-src 'self'; REPORT-URI\thttps://evil.example/r4, frame-src 'none'; report-to e")
	h.Set("Reporting-Endpoints", `e="https://evil.example/r3"`)
	h.Set("Report-To", `{"group":"n","max_age":86400,"endpoints":[{"url":"https://evil.example/nel"}]}`)
	h.Set("NEL", `{"report_to":"n","max_age":86400,"success_fraction":1.0}`)
	cleanResponse(h, "https://p.test", 5173, nil)
	for _, k := range []string{"Content-Security-Policy-Report-Only", "Reporting-Endpoints", "Report-To", "NEL"} {
		if v := h.Get(k); v != "" {
			t.Errorf("%s passed: %q", k, v)
		}
	}
	got := strings.Join(h.Values("Content-Security-Policy"), " | ")
	if want := "img-src 'none' | script-src 'self', frame-src 'none'"; got != want {
		t.Errorf("CSP %q, want %q", got, want)
	}
	// On an allowlist, Collie's own policy blocks WebRTC where browsers
	// know how.
	h = http.Header{}
	cleanResponse(h, "https://p.test", 5173, []string{"github.com"})
	if !strings.Contains(h.Get("Content-Security-Policy"), "webrtc 'block'") {
		t.Errorf("allowlist CSP: %q", h.Get("Content-Security-Policy"))
	}
}

// What sign-in proxies in front of Collie say about the user stays with
// Collie.
func TestIdentityHeadersDropped(t *testing.T) {
	h := http.Header{}
	for _, k := range []string{"X-Pomerium-Jwt-Assertion", "X-Pomerium-Claim-Email", "X-Authentik-Jwt", "X-Authentik-Email",
		"Remote-Groups", "Remote-Name", "Remote-User", "X-Forwarded-Email", "Tailscale-User-Login", "Cf-Access-Jwt-Assertion"} {
		h.Set(k, "alice")
	}
	h.Set("Accept", "text/html")
	cleanRequest(h, "https://p.test", "localhost:5173")
	for k := range h {
		switch k {
		case "Accept", "X-Forwarded-Host", "X-Forwarded-Proto":
		default:
			t.Errorf("%s passed", k)
		}
	}
}
