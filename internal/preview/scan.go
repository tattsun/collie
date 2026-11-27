package preview

import (
	"os"
	"slices"
)

// Filter says which listening sockets count.
type Filter struct {
	// UID, if not negative, keeps only sockets of that user (on the host:
	// Collie's own user, never other users' services).
	UID int
	// SkipOwn leaves out this process's own sockets (Collie's listeners).
	SkipOwn bool
	// SkipPorts are never listed nor connected to (e.g. the allowlist
	// proxy's bridge in a container, Collie's own ports on the host).
	SkipPorts []int
	// Allow, if set, keeps only servers whose working directories (see
	// ancestry) it accepts: on the host, the ones of one session's folder.
	Allow func(dirs []string) bool
}

func (f Filter) skip(s socket, pr proc) bool {
	if f.UID >= 0 && s.uid != f.UID {
		return true
	}
	if f.SkipOwn && pr.pid == os.Getpid() {
		return true
	}
	if f.Allow != nil && !f.Allow(pr.dirs) {
		return true
	}
	return slices.Contains(f.SkipPorts, s.port)
}

// Listening lists the ports that something listens on in this network
// namespace, leaving out the filter's and Collie's and the agents' own.
func Listening(f Filter) ([]Port, error) {
	socks, procs, err := scan()
	if err != nil {
		return nil, err
	}
	return merge(socks, procs, func(s socket, pr proc) bool { return pr.hide || f.skip(s, pr) }), nil
}

// DialAddr is the address to connect to for a port, which must be
// something listening here (see dialAddr). Ports hidden from the list
// can still be reached by number; the filter's can't.
func DialAddr(port int, f Filter) (string, error) {
	socks, procs, err := scan()
	if err != nil {
		return "", err
	}
	ok := socks[:0]
	for _, s := range socks {
		if !f.skip(s, procs[s.inode]) {
			ok = append(ok, s)
		}
	}
	return dialAddr(ok, port)
}
