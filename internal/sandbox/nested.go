package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tattsun/collie/internal/model"
)

// Nested containers: an admin can let a project's containers run Podman
// inside (Project.Nested), e.g. to develop and test Collie itself. The
// agent image has Podman; what the project's containers get on top:
//
//   - no no-new-privileges, and CAP_SETUID/CAP_SETGID in the bounding set:
//     the setuid newuidmap/newgidmap give the inner (rootless) Podman a
//     full range of ids, which images and --userns=keep-id need;
//   - nothing masked or read-only in /proc and /sys (unmask=ALL): an inner
//     container mounts its own /proc, which the kernel refuses while parts
//     of the outer one are covered;
//   - /dev/fuse (fuse-overlayfs) and /dev/net/tun (pasta networking);
//   - /etc/subuid and /etc/subgid listing the ids the container has
//     (everything podman's keep-id maps besides the user's own), and a
//     containers.conf for running without systemd or writable cgroups;
//   - the inner image storage in its own host directory, outside the home
//     (whose ownership Collie repairs, which would break the images).
//
// This widens what the agent can reach — setuid binaries work, and more
// of the kernel (FUSE, tun, fresh proc mounts) is exposed — but everything
// still runs in the outer container's user namespace as the server user's
// ids. It is for trusted projects.

func (m *Manager) nestedDir() string { return filepath.Join(m.cfg.DataDir, "nested") }

// NestedStorage holds the inner Podman's images and containers of a user
// in a project (mounted at ~/.local/share/containers).
func (m *Manager) NestedStorage(projectID, userID string) string {
	return filepath.Join(m.nestedDir(), projectID, userID)
}

const nestedStorageMount = Home + "/.local/share/containers"

// Inner Podman runs without systemd and with a read-only cgroup tree (users
// can override this in ~/.config/containers/containers.conf).
const nestedContainersConf = `# Written by Collie for Podman inside an isolated project's container.
# Override in ~/.config/containers/containers.conf.
[containers]
cgroups = "disabled"
log_driver = "k8s-file"

[engine]
cgroup_manager = "cgroupfs"
events_logger = "file"
`

// subIDRanges are the ids podman's --userns=keep-id gives the container
// besides the user's own (id): with n subordinate ids on the host, the
// container has 0..n with id mapped to the user and the rest to the
// subordinate ids; listed as "name:start:count" lines for /etc/subuid.
func subIDRanges(name string, id, n int) string {
	var b strings.Builder
	if lower := min(id, n); lower > 0 {
		fmt.Fprintf(&b, "%s:0:%d\n", name, lower)
	}
	if upper := n - id; upper > 0 {
		fmt.Fprintf(&b, "%s:%d:%d\n", name, id+1, upper)
	}
	return b.String()
}

// hostSubIDs counts the subordinate ids the host gives the user (by name
// or uid) in /etc/subuid or /etc/subgid.
func hostSubIDs(path, name string, id int) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(strings.TrimSpace(sc.Text()), ":")
		if len(parts) != 3 || (parts[0] != name && parts[0] != strconv.Itoa(id)) {
			continue
		}
		if c, err := strconv.Atoi(parts[2]); err == nil && c > 0 {
			n += c
		}
	}
	return n
}

// prepareNested writes the files a nested project's containers mount.
func (m *Manager) prepareNested(p model.Project, userID string) error {
	if err := os.MkdirAll(m.NestedStorage(p.ID, userID), 0o700); err != nil {
		return err
	}
	u, err := user.Current()
	if err != nil {
		return err
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	// Both files list ranges by user (name or uid).
	uidN := hostSubIDs("/etc/subuid", u.Username, uid)
	gidN := hostSubIDs("/etc/subgid", u.Username, uid)
	if uidN == 0 || gidN == 0 {
		return fmt.Errorf("nested containers need subordinate ids for %s in /etc/subuid and /etc/subgid", u.Username)
	}
	// In the container the user is called currentUser() (--passwd-entry).
	name := currentUser()
	files := map[string]string{
		"subuid":          subIDRanges(name, uid, uidN),
		"subgid":          subIDRanges(name, gid, gidN),
		"containers.conf": nestedContainersConf,
	}
	for f, content := range files {
		if err := writeFileIfChanged(filepath.Join(m.nestedDir(), f), content); err != nil {
			return err
		}
	}
	return nil
}

func writeFileIfChanged(path, content string) error {
	if b, err := os.ReadFile(path); err == nil && string(b) == content {
		return nil
	}
	tmp := path + ".collie-tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// nestedArgs are the run options of a nested project's container (see the
// top of this file); the others get no-new-privileges instead.
func (m *Manager) nestedArgs(p model.Project, userID string) []string {
	if !p.Nested {
		return []string{"--security-opt", "no-new-privileges"}
	}
	args := []string{
		"--cap-add", "SETUID,SETGID",
		"--security-opt", "unmask=ALL",
		"-v", filepath.Join(m.nestedDir(), "subuid") + ":/etc/subuid:ro",
		"-v", filepath.Join(m.nestedDir(), "subgid") + ":/etc/subgid:ro",
		"-v", filepath.Join(m.nestedDir(), "containers.conf") + ":/etc/containers/containers.conf:ro",
		"-v", m.NestedStorage(p.ID, userID) + ":" + nestedStorageMount,
	}
	for _, dev := range []string{"/dev/fuse", "/dev/net/tun"} {
		if _, err := os.Stat(dev); err == nil {
			args = append(args, "--device", dev)
		}
	}
	return args
}

// removeNested deletes inner image storage. It holds files of the
// container's other ids, which only `podman unshare` can remove.
func (m *Manager) removeNested(dirs ...string) {
	for _, d := range dirs {
		if _, err := os.Stat(d); err != nil {
			continue
		}
		if m.available {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			_, _ = m.podman(ctx, "unshare", "rm", "-rf", d)
			cancel()
		}
		_ = os.RemoveAll(d)
	}
}
