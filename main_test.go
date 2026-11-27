package main

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestPreviewSetup(t *testing.T) {
	cases := []struct {
		name                              string
		addr, domain, public, pAddr, pURL string
		n                                 int
		noAuth                            bool
		want                              previewConf
	}{
		{name: "trying it out", addr: ":7878", want: previewConf{addr: ":7879", n: 4}},
		{name: "behind a proxy", addr: "127.0.0.1:7878", public: "https://collie.example.com", want: previewConf{addr: "127.0.0.1:7879", n: 4}},
		{name: "tailscale serve", addr: "127.0.0.1:7878", pURL: ":8443",
			want: previewConf{addr: "127.0.0.1:7879", n: 4, url: ":", public: []int{8443, 8444, 8445, 8446}}},
		{name: "tailscale funnel", addr: "127.0.0.1:7878", pURL: ":8443,10000",
			want: previewConf{addr: "127.0.0.1:7879", n: 2, url: ":", public: []int{8443, 10000}}},
		{name: "a range", addr: "127.0.0.1:7878", pURL: ":9000-9002",
			want: previewConf{addr: "127.0.0.1:7879", n: 3, url: ":", public: []int{9000, 9001, 9002}}},
		{name: "two ports", addr: ":7878", n: 2, want: previewConf{addr: ":7879", n: 2}},
		{name: "domain", addr: ":443", domain: "collie.example.com",
			want: previewConf{addr: ":8443", n: 4, url: "https://collie.example.com:8443"}},
		{name: "domain, names per container", addr: ":443", domain: "collie.example.com", pURL: "https://*.Preview.example.com/",
			want: previewConf{url: "https://*.preview.example.com", wild: ".preview.example.com"}},
		{name: "domain, names per container on a port", addr: ":443", domain: "collie.example.com", pURL: "https://*.preview.example.com:8443",
			want: previewConf{addr: ":8443", n: 1, url: "https://*.preview.example.com:8443"}},
		{name: "proxy, names per container", addr: "127.0.0.1:7878", public: "https://collie.example.com", pURL: "https://*.preview.example.com",
			want: previewConf{addr: "127.0.0.1:7879", n: 1, url: "https://*.preview.example.com"}},
		{name: "proxy, other ports of its name", addr: "127.0.0.1:7878", public: "https://collie.example.com", pURL: "https://collie.example.com:8443",
			want: previewConf{addr: "127.0.0.1:7879", n: 4, url: "https://collie.example.com:8443", public: []int{8443, 8444, 8445, 8446}}},
		{name: "off", addr: ":7878", pAddr: "off", want: previewConf{url: "off"}},
		{name: "dev", addr: "127.0.0.1:7878", noAuth: true, want: previewConf{addr: "127.0.0.1:7879", n: 4}},
	}
	for _, c := range cases {
		if c.n == 0 {
			c.n = 4
		}
		got, err := previewSetup(c.addr, c.domain, c.public, c.pAddr, c.pURL, c.n, c.noAuth)
		if err != nil || got.addr != c.want.addr || got.n != c.want.n || got.url != c.want.url || got.wild != c.want.wild || !slices.Equal(got.public, c.want.public) {
			t.Errorf("%s: %+v, %v; want %+v", c.name, got, err, c.want)
		}
	}
	bad := []string{"preview.example.com", "ftp://x", "https://collie.example.com:8443/path", ":0", ":99999", ":8443-8440", ":a",
		"https://u@collie.example.com:8443",
		// Collie's own origin, or one other name for all previews (the
		// browser wouldn't send Collie's cookie to bind links to it).
		"https://collie.example.com", "https://collie.example.com:443", "https://preview.example.com:8443",
		"https://*.com", "https://a.*.example.com", "https://*x.example.com",
		// Names that include Collie's own, or its parent.
		"https://*.example.com", "https://*.collie.example.com"}
	for _, v := range bad {
		if _, err := previewSetup(":443", "collie.example.com", "", "", v, 4, false); err == nil {
			t.Errorf("--preview-url %q accepted", v)
		}
	}
	for _, n := range []int{0, -1, 17} {
		if _, err := previewSetup(":7878", "", "", "", "", n, false); err == nil {
			t.Errorf("--preview-ports %d accepted", n)
		}
	}
	if _, err := previewSetup(":7878", "", "", ":65534", "", 4, false); err == nil {
		t.Error("ports past 65535 accepted")
	}
	// Without sign-in, previews stay on this machine too.
	if _, err := previewSetup("127.0.0.1:7878", "", "", ":7879", "", 4, true); err == nil {
		t.Error("--no-auth with previews on every address")
	}
}

func TestByHost(t *testing.T) {
	hit := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(name)) })
	}
	h := byHost{suffix: ".preview.example.com", match: hit("preview"), other: hit("collie")}
	for host, want := range map[string]string{
		"collie.example.com": "collie", "c123.preview.example.com": "preview", "C123.Preview.Example.com:443": "preview",
		"preview.example.com": "collie", "evilpreview.example.com": "collie",
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Body.String() != want {
			t.Errorf("%s: %s, want %s", host, w.Body.String(), want)
		}
	}
}
