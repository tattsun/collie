// Package preview lets users open the web servers their agents start (a
// dev server on some port inside the session's container, or on the host
// for host-run projects) in their own browser, through Collie.
//
// Previews are served on their own origin (another port or host name than
// Collie's), so a page an agent wrote never runs where Collie's sign-in
// cookie or API is: it can't read or use them. Only the session's owner
// can open one, with a short-lived, single-use link that Collie's own
// origin hands out (see Proxy).
package preview

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Port is a TCP port something listens on.
type Port struct {
	Port int    `json:"port"`
	Addr string `json:"addr"`              // the address it is bound to ("0.0.0.0", "::1", …)
	Cmd  string `json:"process,omitempty"` // a short form of the command line that listens

	uid   int
	inode uint64
	pid   int
}

// MaxPort bounds port numbers users may ask for.
const MaxPort = 65535

// ValidPort reports whether p can be a TCP port to connect to.
func ValidPort(p int) bool { return p > 0 && p <= MaxPort }

// socket is one row of /proc/net/tcp{,6} (or of lsof).
type socket struct {
	addr  netip.Addr
	port  int
	uid   int
	inode uint64
}

// parseProcNet reads the listening sockets from the contents of
// /proc/net/tcp or /proc/net/tcp6.
func parseProcNet(data string) []socket {
	var out []socket
	for i, line := range strings.Split(data, "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 10 || f[3] != "0A" { // 0A: TCP_LISTEN
			continue
		}
		hexAddr, hexPort, ok := strings.Cut(f[1], ":")
		if !ok {
			continue
		}
		addr, ok := parseHexAddr(hexAddr)
		if !ok {
			continue
		}
		port, err := strconv.ParseUint(hexPort, 16, 16)
		if err != nil || port == 0 {
			continue
		}
		uid, _ := strconv.Atoi(f[7])
		inode, _ := strconv.ParseUint(f[9], 10, 64)
		out = append(out, socket{addr: addr, port: int(port), uid: uid, inode: inode})
	}
	return out
}

// parseHexAddr decodes an address as the kernel prints it: 32-bit words
// in the machine's byte order.
func parseHexAddr(h string) (netip.Addr, bool) {
	if len(h) != 8 && len(h) != 32 {
		return netip.Addr{}, false
	}
	b := make([]byte, 0, len(h)/2)
	for i := 0; i < len(h); i += 8 {
		w, err := strconv.ParseUint(h[i:i+8], 16, 32)
		if err != nil {
			return netip.Addr{}, false
		}
		b = binary.NativeEndian.AppendUint32(b, uint32(w))
	}
	a, ok := netip.AddrFromSlice(b)
	return a.Unmap(), ok
}

// hidden are programs whose ports are Collie's or the agents' own
// machinery (the network bridge, a browser's debugging port), not
// something the user would want to open. They can still be opened by
// number.
var hidden = []string{"collie", "claude", "codex", "chrome", "chromium", "chrome_crashpad", "chrome_crashpad_handler", "headless_shell", "msedge",
	"pasta", "pasta.avx2", "rootlessport", "conmon", "podman"}

// shortCmd turns a command line into a label: the program and its first
// arguments, without directories ("node vite", "python manage.py runserver").
func shortCmd(argv []string) string {
	var parts []string
	n := 0
	for _, a := range argv {
		if a == "" {
			continue
		}
		if n > 0 && strings.HasPrefix(a, "-") && len(parts) > 1 {
			break
		}
		if strings.Contains(a, "/") {
			a = path.Base(a)
		}
		parts = append(parts, a)
		if n++; n == 3 {
			break
		}
	}
	s := strings.Join(parts, " ")
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	return s
}

func isHidden(argv []string, comm string) bool {
	names := []string{comm}
	for i, a := range argv {
		if i == 2 {
			break
		}
		names = append(names, path.Base(a))
	}
	for _, n := range names {
		if slices.Contains(hidden, n) {
			return true
		}
	}
	return false
}

// merge folds sockets into one Port per port number (a server often
// listens on both 0.0.0.0 and ::), sorted by port.
func merge(socks []socket, names map[uint64]proc, skip func(socket, proc) bool) []Port {
	by := map[int]*Port{}
	for _, s := range socks {
		pr := names[s.inode]
		if skip != nil && skip(s, pr) {
			continue
		}
		p, ok := by[s.port]
		if !ok {
			p = &Port{Port: s.port, Addr: s.addr.String(), uid: s.uid, inode: s.inode, pid: pr.pid}
			by[s.port] = p
		} else if rank(s.addr) < rank(mustAddr(p.Addr)) {
			p.Addr = s.addr.String()
		}
		if p.Cmd == "" {
			p.Cmd = pr.cmd
		}
	}
	out := make([]Port, 0, len(by))
	for _, p := range by {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

func mustAddr(s string) netip.Addr {
	a, _ := netip.ParseAddr(s)
	return a
}

// rank orders bound addresses by how the port is best reached: wildcards
// and loopback first.
func rank(a netip.Addr) int {
	switch {
	case a.IsUnspecified() && a.Is4():
		return 0
	case a.IsLoopback() && a.Is4():
		return 1
	case a.IsUnspecified():
		return 2
	case a.IsLoopback():
		return 3
	}
	return 4
}

// proc is what is known about the process owning a socket.
type proc struct {
	pid  int
	cmd  string
	hide bool
}

// ErrNotListening means nothing listens on the port asked for.
var ErrNotListening = errors.New("nothing is listening on that port")

// dialAddr picks the address to connect to for a port from what listens
// on it: the loopback for servers bound to it or to every address,
// otherwise the address the server is bound to (one of this network
// namespace's own). Nothing else can come out of it, so a preview never
// reaches beyond the machine (or container) the server runs in.
func dialAddr(socks []socket, port int) (string, error) {
	if !ValidPort(port) {
		return "", fmt.Errorf("%w (%d)", ErrNotListening, port)
	}
	best := -1
	var addr netip.Addr
	for _, s := range socks {
		if s.port != port {
			continue
		}
		if r := rank(s.addr); best < 0 || r < best {
			best, addr = r, s.addr
		}
	}
	switch {
	case best < 0:
		return "", fmt.Errorf("%w (%d)", ErrNotListening, port)
	case addr.IsUnspecified() || addr.IsLoopback():
		if addr.Is4() {
			return "127.0.0.1:" + strconv.Itoa(port), nil
		}
		return "[::1]:" + strconv.Itoa(port), nil
	}
	return net.JoinHostPort(addr.String(), strconv.Itoa(port)), nil
}

// parseLsof reads the listening sockets from `lsof -F pctn` output (on
// systems without /proc), all of them the given user's. lsof has no
// socket inodes to give; rows are numbered instead.
func parseLsof(out string, uid int) ([]socket, map[uint64]proc) {
	var socks []socket
	procs := map[uint64]proc{}
	var cur proc
	typ := ""
	var row uint64
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 {
			continue
		}
		v := line[1:]
		switch line[0] {
		case 'p':
			pid, _ := strconv.Atoi(v)
			cur = proc{pid: pid}
		case 'c':
			cur.cmd, cur.hide = shortCmd([]string{v}), isHidden(nil, v)
		case 'f':
			typ = ""
		case 't':
			typ = v
		case 'n':
			i := strings.LastIndexByte(v, ':')
			if i < 0 {
				continue
			}
			host := strings.TrimSuffix(strings.TrimPrefix(v[:i], "["), "]")
			port, err := strconv.Atoi(v[i+1:])
			if err != nil || !ValidPort(port) {
				continue
			}
			if host == "*" {
				host = "0.0.0.0"
				if typ == "IPv6" {
					host = "::"
				}
			}
			a, err := netip.ParseAddr(host)
			if err != nil {
				continue
			}
			row++
			socks = append(socks, socket{addr: a.Unmap(), port: port, uid: uid, inode: row})
			procs[row] = cur
		}
	}
	return socks, procs
}
