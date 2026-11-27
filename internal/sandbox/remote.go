package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"runtime"
	"strings"
	"time"
)

// Podman on a Mac is a client of Podman machine: the containers run in a
// Linux VM, which sees the Mac's home folder through virtiofs. Collie
// treats any remote Podman service that way (on Linux too, which is how
// it is tested):
//
//   - flags and commands that only work locally (--cgroup-manager,
//     `podman unshare`) are not used;
//   - what the VM can do (resource limits, SELinux) comes from
//     `podman info`;
//   - Unix sockets on the host can't be reached from the VM, so Collie's
//     tools are relayed through `podman exec` to a socket inside each
//     container (tmpfs at CollieRunDir; see session/relay.go), and no
//     host binary is mounted;
//   - containers inside containers are not offered.

// Remote reports whether Podman runs the containers elsewhere (Podman
// machine's VM).
func (m *Manager) Remote() bool { return m.remote }

var errNestedRemote = errors.New("containers inside containers need Podman on Linux; turn off \"Allow containers inside\" for this project")

// serviceIsRemote asks podman whether it is a remote client.
func (m *Manager) serviceIsRemote() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := m.podman(ctx, "info", "--format", "{{.Host.ServiceIsRemote}}")
	return err == nil && strings.TrimSpace(out) == "true"
}

type hostInfo struct {
	cgroupManager, cgroupVersion string
	selinux                      bool
	user                         string // uid:gid podman runs as in the VM
}

func (m *Manager) hostInfo(ctx context.Context) (hostInfo, error) {
	out, err := m.podman(ctx, "info", "--format", "json")
	if err != nil {
		return hostInfo{}, err
	}
	return parseHostInfo([]byte(out))
}

func parseHostInfo(b []byte) (hostInfo, error) {
	var raw struct {
		Host struct {
			CgroupManager string `json:"cgroupManager"`
			CgroupVersion string `json:"cgroupVersion"`
			Security      struct {
				SELinux bool `json:"selinuxEnabled"`
			} `json:"security"`
			IDMappings struct {
				UIDs []idMap `json:"uidmap"`
				GIDs []idMap `json:"gidmap"`
			} `json:"idMappings"`
		} `json:"host"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return hostInfo{}, fmt.Errorf("podman info: %w", err)
	}
	h := raw.Host
	info := hostInfo{cgroupManager: h.CgroupManager, cgroupVersion: h.CgroupVersion, selinux: h.Security.SELinux}
	// Rootless: the user's own ids are what root in its namespace maps to.
	uid, gid := hostID(h.IDMappings.UIDs), hostID(h.IDMappings.GIDs)
	if uid >= 0 && gid >= 0 {
		info.user = fmt.Sprintf("%d:%d", uid, gid)
	}
	return info, nil
}

type idMap struct {
	ContainerID int `json:"container_id"`
	HostID      int `json:"host_id"`
}

func hostID(maps []idMap) int {
	for _, m := range maps {
		if m.ContainerID == 0 {
			return m.HostID
		}
	}
	return -1
}

// connect gets a remote Podman ready (on a Mac, starting Podman machine's
// VM if it isn't running) and reads what it can do. m.up is closed once
// that is done, successful or not.
func (m *Manager) connect() {
	defer close(m.up)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if runtime.GOOS == "darwin" {
		if err := m.startMachine(ctx); err != nil {
			log.Printf("podman machine: %v", err)
		}
		m.checkFirewall(ctx)
	}
	h, err := m.hostInfo(ctx)
	if err != nil {
		log.Printf("isolated projects can't start: %v", err)
		return
	}
	m.applyHostInfo(h)
	ready := m.imageExists()
	m.mu.Lock()
	m.ready = ready
	m.mu.Unlock()
	m.refreshCustoms()
	m.changed()
}

func (m *Manager) applyHostInfo(h hostInfo) {
	m.selinux = h.selinux
	m.remoteUser = h.user
	// Rootless limits need the systemd cgroup manager on cgroup v2, as
	// on a Linux host (Podman machine's VM has both).
	m.limits.Store(h.cgroupManager == "systemd" && h.cgroupVersion == "v2")
}

func (m *Manager) startMachine(ctx context.Context) error {
	out, err := m.podman(ctx, "machine", "inspect", "--format", "{{.State}}")
	if err != nil {
		return fmt.Errorf("%v (create one with `podman machine init`)", err)
	}
	if strings.TrimSpace(out) == "running" {
		return nil
	}
	log.Printf("starting Podman machine…")
	_, err = m.podman(ctx, "machine", "start")
	return err
}

// checkFirewall notes whether Podman machine's VM has the firewall that
// keeps containers off the LAN, the tailnet and the Mac itself
// (deploy/setup-mac.sh).
func (m *Manager) checkFirewall(ctx context.Context) {
	_, err := m.podman(ctx, "machine", "ssh", "sudo nft list table inet collie")
	if err != nil && !m.noFirewall.Load() {
		log.Printf("warning: Podman machine's VM has no firewall from Collie — containers can reach your LAN and this Mac (run deploy/setup-mac.sh)")
	}
	if m.noFirewall.Swap(err != nil) != (err != nil) {
		m.changed()
	}
}

// recheckFirewall looks again, while it was missing, before a container
// starts: setup-mac.sh may have been run since.
func (m *Manager) recheckFirewall() {
	if runtime.GOOS != "darwin" || !m.noFirewall.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	m.checkFirewall(ctx)
}

// waitUp waits until a remote Podman has been connected (see connect).
func (m *Manager) waitUp() {
	if m.up != nil {
		<-m.up
	}
}
