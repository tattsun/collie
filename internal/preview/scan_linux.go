package preview

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// scan reads the listening TCP sockets of this network namespace from
// /proc, and which of the processes we may look at own them.
func scan() ([]socket, map[uint64]proc, error) {
	var socks []socket
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			if f == "/proc/net/tcp6" && errors.Is(err, os.ErrNotExist) {
				continue // IPv6 off
			}
			return nil, nil, err
		}
		socks = append(socks, parseProcNet(string(b))...)
	}
	want := make(map[uint64]bool, len(socks))
	for _, s := range socks {
		want[s.inode] = true
	}
	procs := owners(want)
	dirs := map[int][]string{}
	for ino, pr := range procs {
		d, ok := dirs[pr.pid]
		if !ok {
			d = ancestry(pr.pid, os.Getpid(), procParent, procCwd)
			dirs[pr.pid] = d
		}
		pr.dirs = d
		procs[ino] = pr
	}
	return socks, procs, nil
}

func procParent(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	return parseStatPPid(string(b))
}

// procCwd is a process's working directory ("" if not ours to read).
func procCwd(pid int) string {
	d, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
	if err != nil {
		return ""
	}
	return cleanCwd(d)
}

// At most this many processes are looked through for socket owners.
const maxProcs = 8192

// owners maps socket inodes to the processes that hold them (only
// processes of our own user are readable; the rest stay unnamed).
func owners(want map[uint64]bool) map[uint64]proc {
	out := map[uint64]proc{}
	if len(want) == 0 {
		return out
	}
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	n := 0
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if n++; n > maxProcs {
			break
		}
		dir := filepath.Join("/proc", e.Name())
		fds, err := os.ReadDir(filepath.Join(dir, "fd"))
		if err != nil {
			continue
		}
		var found []uint64
		for _, fd := range fds {
			l, err := os.Readlink(filepath.Join(dir, "fd", fd.Name()))
			if err != nil || !strings.HasPrefix(l, "socket:[") {
				continue
			}
			ino, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(l, "socket:["), "]"), 10, 64)
			if err == nil && want[ino] {
				if _, seen := out[ino]; !seen {
					found = append(found, ino)
				}
			}
		}
		if len(found) == 0 {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
		argv := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		comm, _ := os.ReadFile(filepath.Join(dir, "comm"))
		c := strings.TrimSpace(string(comm))
		if len(argv) == 0 || argv[0] == "" {
			argv = []string{c}
		}
		pr := proc{pid: pid, cmd: shortCmd(argv), hide: isHidden(argv, c)}
		for _, ino := range found {
			out[ino] = pr
		}
	}
	return out
}
