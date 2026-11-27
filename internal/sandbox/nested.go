package sandbox

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
//   - no no-new-privileges, and CAP_SETUID/CAP_SETGID (which podman also
//     makes ambient for the non-root user): newuidmap/newgidmap give the
//     inner (rootless) Podman a full range of ids, which images and
//     --userns=keep-id need. They are not setuid in the image: as root
//     without CAP_DAC_OVERRIDE they could not open /proc/<pid>/uid_map;
//   - nothing masked or read-only in /proc and /sys (unmask=ALL): an inner
//     container mounts its own /proc, which the kernel refuses while parts
//     of the outer one are covered;
//   - /dev/fuse (fuse-overlayfs) and /dev/net/tun (pasta networking);
//   - /etc/subuid and /etc/subgid listing the ids the container has
//     (everything podman's keep-id maps besides the user's own and 0), and
//     a containers.conf for running without systemd or writable cgroups;
//   - Podman's seccomp profile, plus chroot (unpacking image layers falls
//     back to it, as pivot_root fails on the inherited, locked root mount)
//     and sethostname / setdomainname (an inner container's own UTS
//     namespace). The kernel still requires CAP_SYS_CHROOT / CAP_SYS_ADMIN
//     for them, which the container's own processes don't have, so they
//     only work in namespaces the agent creates;
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

// Podman's own seccomp profile (an admin's override in /etc first).
var podmanSeccompPaths = []string{"/etc/containers/seccomp.json", "/usr/share/containers/seccomp.json"}

// nestedSyscalls are allowed in nested containers (see the top of this file).
var nestedSyscalls = []string{"chroot", "sethostname", "setdomainname"}

// nestedSeccomp is Podman's profile with nestedSyscalls allowed: they are
// taken out of every rule that denies them (the profile denies them unless
// the container has CAP_SYS_CHROOT / CAP_SYS_ADMIN) and allowed outright.
func nestedSeccomp(profile []byte) ([]byte, error) {
	var p map[string]any
	if err := json.Unmarshal(profile, &p); err != nil {
		return nil, fmt.Errorf("seccomp profile: %v", err)
	}
	rules, ok := p["syscalls"].([]any)
	if !ok {
		return nil, errors.New("seccomp profile: no syscalls")
	}
	allow := map[string]bool{}
	for _, n := range nestedSyscalls {
		allow[n] = true
	}
	out := make([]any, 0, len(rules)+1)
	for _, r := range rules {
		rule, ok := r.(map[string]any)
		if action, _ := rule["action"].(string); ok && action != "SCMP_ACT_ALLOW" {
			if names, ok := rule["names"].([]any); ok {
				keep := []any{}
				for _, n := range names {
					if s, _ := n.(string); !allow[s] {
						keep = append(keep, n)
					}
				}
				if len(keep) == 0 {
					continue
				}
				rule["names"] = keep
			}
		}
		out = append(out, r)
	}
	out = append(out, map[string]any{
		"names": nestedSyscalls, "action": "SCMP_ACT_ALLOW", "args": []any{},
		"comment": "Collie: Podman inside the container", "includes": map[string]any{}, "excludes": map[string]any{},
	})
	p["syscalls"] = out
	return json.MarshalIndent(p, "", "  ")
}

// subIDRanges are the ids podman's --userns=keep-id gives the container
// besides the user's own (id): with n subordinate ids on the host, the
// container has 0..n with id mapped to the user and the rest to the
// subordinate ids; listed as "name:start:count" lines for /etc/subuid.
// The container's 0 is left out: mapping it into a new user namespace
// needs CAP_SETFCAP.
func subIDRanges(name string, id, n int) string {
	var b strings.Builder
	if lower := min(id, n) - 1; lower > 0 {
		fmt.Fprintf(&b, "%s:1:%d\n", name, lower)
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
	var seccomp []byte
	for _, path := range podmanSeccompPaths {
		if b, err := os.ReadFile(path); err == nil {
			if seccomp, err = nestedSeccomp(b); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			break
		}
	}
	if seccomp == nil {
		return fmt.Errorf("nested containers need Podman's seccomp profile (%s)", podmanSeccompPaths[len(podmanSeccompPaths)-1])
	}
	// In the container the user is called currentUser() (--passwd-entry).
	name := currentUser()
	files := map[string]string{
		"subuid":          subIDRanges(name, uid, uidN),
		"subgid":          subIDRanges(name, gid, gidN),
		"containers.conf": nestedContainersConf,
		"seccomp.json":    string(seccomp),
	}
	for f, content := range files {
		if err := writeFileIfChanged(filepath.Join(m.nestedDir(), f), content); err != nil {
			return err
		}
	}
	return nil
}

var nestedFiles = []string{"subuid", "subgid", "containers.conf", "seccomp.json"}

func (m *Manager) nestedFilesHash() string {
	h := sha256.New()
	for _, f := range nestedFiles {
		b, _ := os.ReadFile(filepath.Join(m.nestedDir(), f))
		h.Write(b)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
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
		"--security-opt", "seccomp=" + filepath.Join(m.nestedDir(), "seccomp.json"),
		// re-create the container when these files change
		"--label", "collie.nested=" + m.nestedFilesHash(),
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
