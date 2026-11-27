//go:build !linux

package preview

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// scan asks lsof (always there on macOS) for this user's listening TCP
// sockets: other systems have no /proc to read them from.
func scan() ([]socket, map[uint64]proc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	uid := os.Getuid()
	out, err := exec.CommandContext(ctx, "lsof", "-nP", "+c", "0", "-iTCP", "-sTCP:LISTEN", "-a", "-u", strconv.Itoa(uid), "-Fpctn").Output()
	if err != nil && len(out) == 0 {
		// lsof exits 1 when nothing matches.
		if _, ok := err.(*exec.ExitError); ok {
			return nil, map[uint64]proc{}, nil
		}
		return nil, nil, err
	}
	socks, procs := parseLsof(string(out), uid)
	addDirs(ctx, procs)
	return socks, procs, nil
}

// addDirs fills in the working directories of the processes and their
// parents (see ancestry), from ps and lsof.
func addDirs(ctx context.Context, procs map[uint64]proc) {
	if len(procs) == 0 {
		return
	}
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=").Output()
	if err != nil {
		return
	}
	parents := parsePs(string(out))
	stop := os.Getpid()
	chain := map[int]bool{}
	for _, pr := range procs {
		for p, i := pr.pid, 0; i < maxAncestors && p > 1 && p != stop; i++ {
			chain[p] = true
			p = parents[p]
		}
	}
	var list []string
	for p := range chain {
		list = append(list, strconv.Itoa(p))
	}
	cwds := map[int]string{}
	if out, err := exec.CommandContext(ctx, "lsof", "-a", "-d", "cwd", "-Fpn", "-p", strings.Join(list, ",")).Output(); err == nil || len(out) > 0 {
		cwds = parseLsofCwd(string(out))
	}
	for k, pr := range procs {
		pr.dirs = ancestry(pr.pid, stop, func(p int) int { return parents[p] }, func(p int) string { return cwds[p] })
		procs[k] = pr
	}
}
