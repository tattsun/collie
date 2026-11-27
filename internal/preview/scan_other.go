//go:build !linux

package preview

import (
	"context"
	"os"
	"os/exec"
	"strconv"
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
	return socks, procs, nil
}
