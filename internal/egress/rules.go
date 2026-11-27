// Package egress limits where a project's containers can connect to. A
// container on an allowlist has no network of its own: its only way out is
// an HTTP proxy in Collie, reached through a Unix socket mounted into it,
// which lets through the hosts on the project's list (and the agents' own
// services) and nothing else.
package egress

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// AgentHosts are always allowed: Claude Code and Codex need them to work
// (their APIs and sign-in).
var AgentHosts = []string{
	"anthropic.com", "*.anthropic.com", "claude.ai", "*.claude.ai", "claude.com", "*.claude.com",
	"openai.com", "*.openai.com", "chatgpt.com", "*.chatgpt.com",
}

// Suggested is a starting list for a new allowlist: git hosting and the
// usual package registries and toolchain downloads.
var Suggested = []string{
	"github.com", "*.github.com", "*.githubusercontent.com",
	"registry.npmjs.org", "pypi.org", "files.pythonhosted.org",
	"proxy.golang.org", "sum.golang.org", "crates.io", "static.crates.io", "index.crates.io",
	"nodejs.org", "mise-versions.jdx.dev",
}

// A rule is one allowlist entry:
//
//	example.com        that host, any port
//	*.example.com      its subdomains (not example.com itself)
//	example.com:443    that host, one port
//	203.0.113.7        an address (also with a port, [v6]:port), or
//	192.168.1.0/24     a range; only addresses listed like this can be
//	                   on a private network
type rule struct {
	host   string       // lower case, without "*." for wildcards
	wild   bool         // subdomains of host
	prefix netip.Prefix // address rules
	port   int          // 0: any
}

// Normalize checks one entry and returns it in canonical form. A pasted
// URL is reduced to its host (and explicit port).
func Normalize(s string) (string, error) {
	r, err := parseRule(s)
	if err != nil {
		return "", err
	}
	return r.String(), nil
}

// NormalizeList normalizes entries, dropping blanks, comments (#) and
// duplicates.
func NormalizeList(in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range in {
		if i := strings.IndexByte(s, '#'); i >= 0 {
			s = s[:i]
		}
		if strings.TrimSpace(s) == "" {
			continue
		}
		n, err := Normalize(s)
		if err != nil {
			return nil, err
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if len(out) > 500 {
		return nil, errors.New("too many allowlist entries (at most 500)")
	}
	return out, nil
}

func (r rule) String() string {
	var h string
	switch {
	case r.prefix.IsValid() && r.prefix.IsSingleIP():
		h = r.prefix.Addr().String()
		if r.port != 0 && r.prefix.Addr().Is6() {
			h = "[" + h + "]"
		}
	case r.prefix.IsValid():
		return r.prefix.String()
	case r.wild:
		h = "*." + r.host
	default:
		h = r.host
	}
	if r.port != 0 {
		h += ":" + strconv.Itoa(r.port)
	}
	return h
}

func parseRule(s string) (rule, error) {
	orig := s
	s = strings.ToLower(strings.TrimSpace(s))
	bad := func(why string) (rule, error) {
		return rule{}, fmt.Errorf("allowlist entry %q: %s", strings.TrimSpace(orig), why)
	}
	if s == "" {
		return bad("empty")
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return bad("not a host name")
		}
		s = u.Host
	} else if i := strings.IndexAny(s, "/?"); i >= 0 {
		// a CIDR range, or a host with a path pasted after it
		if p, err := netip.ParsePrefix(s); err == nil {
			return rule{prefix: p.Masked()}, nil
		}
		s = s[:i]
	}
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:] // user@host
	}
	var r rule
	host := s
	if h, p, err := net.SplitHostPort(s); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return bad("invalid port")
		}
		host, r.port = h, n
	} else if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		host = s[1 : len(s)-1]
	}
	if a, err := netip.ParseAddr(host); err == nil {
		r.prefix = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
		return r, nil
	}
	host = strings.TrimSuffix(host, ".")
	if strings.HasPrefix(host, "*.") {
		r.wild, host = true, host[2:]
	}
	if host == "*" || host == "" {
		return bad("to allow everything, choose Internet instead of an allowlist")
	}
	if !validHost(host) {
		return bad("not a host name")
	}
	if r.wild && !strings.Contains(host, ".") {
		return bad("a wildcard needs a domain with a dot, like *.example.com")
	}
	r.host = host
	return r, nil
}

func validHost(h string) bool {
	if len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// target is where a client asked to connect.
type target struct {
	host string     // lower case, no trailing dot
	addr netip.Addr // set when host is an address
	port int
}

func newTarget(host string, port int) target {
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	t := target{host: host, port: port}
	if a, err := netip.ParseAddr(host); err == nil {
		t.addr = a.Unmap()
		t.host = t.addr.String()
	}
	return t
}

func (r rule) match(t target) bool {
	if r.port != 0 && r.port != t.port {
		return false
	}
	if r.prefix.IsValid() {
		return t.addr.IsValid() && r.prefix.Contains(t.addr)
	}
	if t.addr.IsValid() {
		return false
	}
	if r.wild {
		return strings.HasSuffix(t.host, "."+r.host)
	}
	return t.host == r.host
}

// rules parses entries, skipping any that don't parse (stored lists are
// normalized already).
func rules(entries []string) []rule {
	out := make([]rule, 0, len(entries))
	for _, e := range entries {
		if r, err := parseRule(e); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// Allowed reports whether entries (or AgentHosts) let a client connect to
// host:port.
func Allowed(entries []string, host string, port int) bool {
	_, ok := matchRule(entries, newTarget(host, port))
	return ok
}

// matchRule returns the rule that lets t through.
func matchRule(entries []string, t target) (rule, bool) {
	for _, list := range [][]string{AgentHosts, entries} {
		for _, r := range rules(list) {
			if r.match(t) {
				return r, true
			}
		}
	}
	return rule{}, false
}
