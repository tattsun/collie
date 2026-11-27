package sandbox

import (
	"os"
	"path/filepath"

	"github.com/tattsun/collie/internal/egress"
	"github.com/tattsun/collie/internal/model"
)

// Network: a project's containers either reach the internet through pasta
// (with no way to the host; the server's firewall keeps them off the LAN
// and tailnet), or, on an allowlist, have no network at all. Then their only
// way out is Collie's proxy (package egress) on a Unix socket mounted into
// the container, which a small bridge (Collie's own binary, the container's
// main process) offers on the container's loopback; programs find it in the
// usual proxy variables. Programs that ignore those can't connect anywhere.

// Where a container on an allowlist sees its proxy socket, and where its
// programs find the proxy.
const (
	NetRunDir = "/run/collie-net"
	ProxyAddr = "127.0.0.1:3128"
)

const proxySock = "proxy.sock"

// netDir holds the proxy socket of a user's container for a project.
func (m *Manager) netDir(projectID, userID string) string {
	return filepath.Join(m.cfg.DataDir, "netsock", projectID, userID)
}

// Egress is the proxy for containers on an allowlist.
func (m *Manager) Egress() *egress.Proxy { return m.egress }

// networkMode labels a container with the kind of network it has, so a
// change takes effect even while the container is in use.
func networkMode(p model.Project) string {
	if p.Allowlisted() {
		return model.NetAllowlist
	}
	return ""
}

// networkArgs are the run options for the project's network.
func (m *Manager) networkArgs(p model.Project, uid string) []string {
	if !p.Allowlisted() {
		return []string{
			// No shortcuts to the host: neither its loopback nor the
			// host.containers.internal alias is mapped into the container.
			// (The host's other addresses are blocked by
			// deploy/setup-host.sh's firewall.)
			"--network", "pasta:--map-host-loopback,none,--map-guest-addr,none",
		}
	}
	proxy := "http://" + ProxyAddr
	noProxy := "localhost,127.0.0.1,::1"
	return []string{
		"--network", "none",
		"--label", "collie.network=" + model.NetAllowlist,
		"-v", m.netDir(p.ID, uid) + ":" + NetRunDir + ":ro",
		"-e", "HTTP_PROXY=" + proxy, "-e", "HTTPS_PROXY=" + proxy,
		"-e", "http_proxy=" + proxy, "-e", "https_proxy=" + proxy,
		"-e", "NO_PROXY=" + noProxy, "-e", "no_proxy=" + noProxy,
		// Node's own fetch and http honor the variables only when asked.
		"-e", "NODE_USE_ENV_PROXY=1",
		"-e", "COLLIE_NETWORK=" + model.NetAllowlist,
	}
}

// mainCommand is what the container runs: on an allowlist the bridge to
// the proxy, otherwise nothing but a placeholder.
func mainCommand(p model.Project) []string {
	if p.Allowlisted() {
		return []string{CollieBinPath, "net-bridge", NetRunDir + "/" + proxySock, ProxyAddr}
	}
	return []string{"sleep", "infinity"}
}

// prepareNetwork starts the proxy for the user's container of a project on
// an allowlist.
func (m *Manager) prepareNetwork(sp *Spec) error {
	if !sp.Project.Allowlisted() {
		return nil
	}
	if sp.CollieBin == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		sp.CollieBin = exe
	}
	return m.egress.Listen(filepath.Join(m.netDir(sp.Project.ID, sp.UserID), proxySock), sp.Project.ID, sp.UserID)
}

// SetNetRules tells the proxy where to find a project's allowlist (looked
// up for every connection, so changes to the list apply at once).
func (m *Manager) SetNetRules(lookup func(projectID string) (model.Project, bool)) {
	m.egress.Rules = func(id string) ([]string, bool) {
		p, ok := lookup(id)
		if !ok || !p.Allowlisted() {
			return nil, false
		}
		return p.NetAllow, true
	}
}

// NetworkLog lists where a project's containers tried to connect recently.
func (m *Manager) NetworkLog(projectID string) []egress.Entry { return m.egress.Recent(projectID) }
