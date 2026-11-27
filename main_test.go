package main

import (
	"slices"
	"testing"
)

func TestPreviewSetup(t *testing.T) {
	cases := []struct {
		name                     string
		addr, domain, pAddr, url string
		noAuth                   bool
		want                     previewConf
	}{
		{name: "trying it out", addr: ":7878", want: previewConf{addr: ":7879"}},
		{name: "behind a proxy", addr: "127.0.0.1:7878", want: previewConf{addr: "127.0.0.1:7879"}},
		{name: "tailscale serve", addr: "127.0.0.1:7878", url: ":8443", want: previewConf{addr: "127.0.0.1:7879", url: ":8443"}},
		{name: "domain", addr: ":443", domain: "collie.example.com",
			want: previewConf{addr: ":8443", url: "https://collie.example.com:8443"}},
		{name: "domain, own name", addr: ":443", domain: "collie.example.com", url: "https://Preview.example.com/",
			want: previewConf{url: "https://preview.example.com", host: "preview.example.com", certHosts: []string{"preview.example.com"}}},
		{name: "domain, own name and port", addr: ":443", domain: "collie.example.com", url: "https://preview.example.com:8443",
			want: previewConf{addr: ":8443", url: "https://preview.example.com:8443", certHosts: []string{"preview.example.com"}}},
		{name: "proxy, own name", addr: "127.0.0.1:7878", url: "https://preview.example.com",
			want: previewConf{addr: "127.0.0.1:7879", url: "https://preview.example.com"}},
		{name: "off", addr: ":7878", pAddr: "off", want: previewConf{url: "off"}},
		{name: "dev", addr: "127.0.0.1:7878", noAuth: true, want: previewConf{addr: "127.0.0.1:7879"}},
	}
	for _, c := range cases {
		got, err := previewSetup(c.addr, c.domain, c.pAddr, c.url, c.noAuth)
		if err != nil || got.addr != c.want.addr || got.url != c.want.url || got.host != c.want.host || !slices.Equal(got.certHosts, c.want.certHosts) {
			t.Errorf("%s: %+v, %v; want %+v", c.name, got, err, c.want)
		}
	}
	for _, bad := range []string{"preview.example.com", "ftp://x", "https://x/path", ":0", ":99999", "https://u@x"} {
		if _, err := previewSetup(":7878", "", "", bad, false); err == nil {
			t.Errorf("--preview-url %q accepted", bad)
		}
	}
	// Without sign-in, previews stay on this machine too.
	if _, err := previewSetup("127.0.0.1:7878", "", ":7879", "", true); err == nil {
		t.Error("--no-auth with previews on every address")
	}
}
